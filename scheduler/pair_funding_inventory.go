package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

const (
	pairInventoryOriginCoordinator = "coordinator"
	pairInventoryOriginNative      = "native"
	pairInventoryOriginUnexplained = "unexplained"
)

type pairInventoryRow struct {
	Seq           int64
	FillID        string
	Origin        string
	ActionID      string
	PositionID    string
	Delta         pairDecimal
	Post          pairDecimal
	ContractSize  pairDecimal
	EventMs       int64
	EventLatestMs int64
	VisibleMs     int64
	RecordedAtMs  int64
}

type pairVenueFill struct {
	FillID    string  `json:"fill_id"`
	BaseQty   float64 `json:"base_qty"`
	EventMs   int64   `json:"event_ms"`
	VisibleMs int64   `json:"visible_ms"`
}

type pairFillPiece struct {
	FillID    string
	Qty       *big.Rat
	EventMs   int64
	VisibleMs int64
}

type pairFillContext struct {
	ActionID  string
	Pieces    []pairFillPiece
	LoMs      int64
	HiMs      int64
	VisibleMs int64
}

type pairFundingMember struct {
	memberID     string
	key          pairEvidenceKey
	member       pairContractMember
	coin         string
	contractSize pairDecimal
	rows         []pairInventoryRow
	durableSeq   int64
	observedMs   int64
	active       bool
	ctx          *pairFillContext
	maxAppliedMs int64
	integrity    []string
}

func pairBookRat(s *StrategyState, coin string) *big.Rat {
	q := basketPositionSigned(s, coin)
	r, err := quoteRatFromFloatDecimal(q)
	if err != nil {
		return new(big.Rat)
	}
	return r
}

func pairRatOf(d pairDecimal) *big.Rat {
	r, err := d.rat()
	if err != nil {
		return new(big.Rat)
	}
	return r
}

func pairBookPositionID(s *StrategyState, coin string) string {
	pos := s.Positions[coin]
	if pos == nil || pos.Quantity == 0 {
		return ""
	}
	return ensurePositionTradeID(s.ID, coin, pos)
}

func pairRowPositionID(post, before *big.Rat, beforeID, afterID string) string {
	if post.Sign() == 0 {
		return ""
	}
	if beforeID != "" && before.Sign() == post.Sign() {
		return beforeID
	}
	return afterID
}

func pairDecimalOfRat(r *big.Rat) pairDecimal {
	s, err := pairRatDecimal(r)
	if err != nil {
		return pairDecimal("0")
	}
	return pairDecimal(s)
}

func (m *pairFundingMember) total() *big.Rat {
	t := new(big.Rat)
	for _, r := range m.rows {
		t.Add(t, pairRatOf(r.Delta))
	}
	return t
}

func (m *pairFundingMember) lastVisibleMs() int64 {
	var v int64
	for _, r := range m.rows {
		if r.VisibleMs > v {
			v = r.VisibleMs
		}
	}
	return v
}

func (m *pairFundingMember) appendRow(r pairInventoryRow, before *big.Rat, beforeID, afterID string) {
	r.Seq = int64(len(m.rows)) + 1
	if r.ContractSize == "" {
		r.ContractSize = m.contractSize
	}
	post := new(big.Rat).Add(m.total(), pairRatOf(r.Delta))
	r.Post = pairDecimalOfRat(post)
	r.PositionID = pairRowPositionID(post, before, beforeID, afterID)
	m.rows = append(m.rows, r)
	if m.maxAppliedMs > 0 && r.EventMs < m.maxAppliedMs {
		m.integrity = append(m.integrity, fmt.Sprintf("inventory change %d (%s, %s base, possible time [%d, %d]) is earlier than the applied funding event at %d whose inventory completeness was already accepted; the applied charge is kept unchanged", r.Seq, r.Origin, r.Delta, r.EventMs, r.EventLatestMs, m.maxAppliedMs))
	}
}

func (m *pairFundingMember) reconcile(book *big.Rat, positionID string, nowMs int64) {
	before := m.total()
	diff := new(big.Rat).Sub(book, before)
	if diff.Sign() == 0 {
		return
	}
	lo := m.observedMs + 1
	if m.observedMs == 0 {
		lo = m.lastVisibleMs() + 1
	}
	if lo > nowMs {
		lo = nowMs
	}
	if lo < 1 {
		lo = 1
	}
	m.appendRow(pairInventoryRow{Origin: pairInventoryOriginUnexplained, Delta: pairDecimalOfRat(diff), EventMs: lo, EventLatestMs: nowMs, VisibleMs: nowMs, RecordedAtMs: nowMs}, before, "", positionID)
}

func (m *pairFundingMember) observe(s *StrategyState, nowMs int64) {
	m.reconcile(pairBookRat(s, m.coin), pairBookPositionID(s, m.coin), nowMs)
	if nowMs > m.observedMs {
		m.observedMs = nowMs
	}
}

func (m *pairFundingMember) recordChange(before, after *big.Rat, beforeID, afterID string, nowMs int64) {
	delta := new(big.Rat).Sub(after, before)
	if delta.Sign() == 0 {
		return
	}
	ctx := m.ctx
	if ctx == nil {
		m.appendRow(pairInventoryRow{Origin: pairInventoryOriginNative, Delta: pairDecimalOfRat(delta), EventMs: nowMs, EventLatestMs: nowMs, VisibleMs: nowMs, RecordedAtMs: nowMs}, before, beforeID, afterID)
		return
	}
	abs := new(big.Rat).Abs(delta)
	sum := new(big.Rat)
	exact := len(ctx.Pieces) > 0
	for _, p := range ctx.Pieces {
		if p.Qty == nil || p.Qty.Sign() <= 0 || p.EventMs <= 0 || p.VisibleMs < p.EventMs {
			exact = false
			break
		}
		sum.Add(sum, p.Qty)
	}
	if exact && sum.Cmp(abs) == 0 {
		pieces := append([]pairFillPiece(nil), ctx.Pieces...)
		sort.SliceStable(pieces, func(i, j int) bool {
			if pieces[i].EventMs != pieces[j].EventMs {
				return pieces[i].EventMs < pieces[j].EventMs
			}
			return pieces[i].FillID < pieces[j].FillID
		})
		for _, p := range pieces {
			d := new(big.Rat).Set(p.Qty)
			if delta.Sign() < 0 {
				d.Neg(d)
			}
			m.appendRow(pairInventoryRow{FillID: p.FillID, Origin: pairInventoryOriginCoordinator, ActionID: ctx.ActionID, Delta: pairDecimalOfRat(d), EventMs: p.EventMs, EventLatestMs: p.EventMs, VisibleMs: p.VisibleMs, RecordedAtMs: nowMs}, before, beforeID, afterID)
		}
		return
	}
	lo, hi, vis := ctx.LoMs, ctx.HiMs, ctx.VisibleMs
	for _, p := range ctx.Pieces {
		if p.EventMs > 0 && (lo == 0 || p.EventMs < lo) {
			lo = p.EventMs
		}
		if p.EventMs > hi {
			hi = p.EventMs
		}
		if p.VisibleMs > vis {
			vis = p.VisibleMs
		}
	}
	if lo <= 0 || hi < lo {
		lo, hi = 1, nowMs
	}
	if vis < hi {
		vis = hi
	}
	m.appendRow(pairInventoryRow{Origin: pairInventoryOriginCoordinator, ActionID: ctx.ActionID, Delta: pairDecimalOfRat(delta), EventMs: lo, EventLatestMs: hi, VisibleMs: vis, RecordedAtMs: nowMs}, before, beforeID, afterID)
}

func pairInventoryTrack(s *StrategyState, symbols []string, run func()) bool {
	if s == nil || s.pairFunding == nil {
		return false
	}
	m := s.pairFunding
	if m.active {
		return false
	}
	touched := false
	for _, sym := range symbols {
		if sym == m.coin {
			touched = true
		}
	}
	if !touched {
		return false
	}
	m.active = true
	defer func() { m.active = false }()
	nowMs := strategyBookNow(s).UnixMilli()
	before := pairBookRat(s, m.coin)
	beforeID := pairBookPositionID(s, m.coin)
	m.reconcile(before, beforeID, nowMs)
	run()
	m.recordChange(before, pairBookRat(s, m.coin), beforeID, pairBookPositionID(s, m.coin), nowMs)
	return true
}

func withPairFillContext(s *StrategyState, ctx *pairFillContext, fn func()) {
	if s == nil || s.pairFunding == nil || ctx == nil {
		fn()
		return
	}
	prev := s.pairFunding.ctx
	s.pairFunding.ctx = ctx
	defer func() { s.pairFunding.ctx = prev }()
	fn()
}

type pairInventoryState int

const (
	pairInventoryExcluded pairInventoryState = iota
	pairInventoryIncluded
	pairInventoryAmbiguous
)

func pairRowStateAt(r pairInventoryRow, t int64) pairInventoryState {
	switch {
	case r.EventLatestMs < t:
		return pairInventoryIncluded
	case r.EventMs >= t:
		return pairInventoryExcluded
	}
	return pairInventoryAmbiguous
}

func (m *pairFundingMember) qtyBefore(t int64) (*big.Rat, bool) {
	q := new(big.Rat)
	amb := false
	for _, r := range m.rows {
		switch pairRowStateAt(r, t) {
		case pairInventoryIncluded:
			q.Add(q, pairRatOf(r.Delta))
		case pairInventoryAmbiguous:
			amb = true
		}
	}
	return q, amb
}

func (m *pairFundingMember) windowTouched(t, windowMs int64) bool {
	if windowMs <= 0 {
		return false
	}
	end := t + windowMs - 1
	for _, r := range m.rows {
		if r.EventMs <= end && r.EventLatestMs >= t {
			return true
		}
	}
	return false
}

func (m *pairFundingMember) exposure(throughMs int64) []pairFundingInterval {
	if len(m.rows) == 0 {
		return nil
	}
	set := map[int64]bool{}
	for _, r := range m.rows {
		set[r.EventMs] = true
		set[r.EventLatestMs] = true
	}
	points := make([]int64, 0, len(set))
	for p := range set {
		points = append(points, p)
	}
	sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })
	var out []pairFundingInterval
	add := func(from, through int64) {
		if through > throughMs {
			through = throughMs
		}
		if from >= through {
			return
		}
		probe := from + 1
		q, amb := m.qtyBefore(probe)
		if amb || q.Sign() != 0 {
			out = append(out, pairFundingInterval{FromMs: from, ThroughMs: through})
		}
	}
	for i := 0; i+1 < len(points); i++ {
		add(points[i], points[i+1])
	}
	add(points[len(points)-1], throughMs)
	return pairFundingNormalize(out)
}

func (m *pairFundingMember) stretchStart(t int64) int64 {
	for _, iv := range m.exposure(t) {
		if iv.FromMs < t && t <= iv.ThroughMs {
			return iv.FromMs
		}
	}
	return t - 1
}

func (m *pairFundingMember) journaledFills(actionID string) map[string]bool {
	out := map[string]bool{}
	for _, r := range m.rows {
		if r.ActionID == actionID && r.FillID != "" {
			out[r.FillID] = true
		}
	}
	return out
}

func pairValidateVenueFills(a *basketAction, out basketVenueOutcome) string {
	if len(out.Fills) == 0 {
		return ""
	}
	seen := map[string]bool{}
	sum := 0.0
	for i, f := range out.Fills {
		if !pairPrintableASCII(f.FillID, pairEvidenceMaxIDLen) {
			return fmt.Sprintf("fill %d has no printable fill id of at most %d characters", i, pairEvidenceMaxIDLen)
		}
		if seen[f.FillID] {
			return fmt.Sprintf("fill id %s appears twice", f.FillID)
		}
		seen[f.FillID] = true
		if !pairFinite(f.BaseQty) || f.BaseQty <= 0 {
			return fmt.Sprintf("fill %s base quantity %v is not finite and positive", f.FillID, f.BaseQty)
		}
		if f.EventMs <= 0 || f.VisibleMs < f.EventMs {
			return fmt.Sprintf("fill %s needs an event time above zero and a visibility at or after it (event %d, visible %d)", f.FillID, f.EventMs, f.VisibleMs)
		}
		if out.AcceptedAtMs > 0 && f.EventMs < out.AcceptedAtMs {
			return fmt.Sprintf("fill %s event time %d is before the venue acceptance at %d", f.FillID, f.EventMs, out.AcceptedAtMs)
		}
		if out.ObservedAtMs > 0 && f.VisibleMs > out.ObservedAtMs {
			return fmt.Sprintf("fill %s is visible at %d, after the observation at %d that reports it", f.FillID, f.VisibleMs, out.ObservedAtMs)
		}
		sum += f.BaseQty
	}
	if !basketQtyEqual(sum, out.CumQty) {
		return fmt.Sprintf("the fills sum to %v, the cumulative quantity is %v; a fill list must be complete", sum, out.CumQty)
	}
	return ""
}

func pairFillsImmutable(prevEvidence string, out basketVenueOutcome) string {
	if strings.TrimSpace(prevEvidence) == "" || len(out.Fills) == 0 {
		return ""
	}
	var prev basketVenueOutcome
	if err := json.Unmarshal([]byte(prevEvidence), &prev); err != nil {
		return ""
	}
	now := map[string]pairVenueFill{}
	for _, f := range out.Fills {
		now[f.FillID] = f
	}
	for _, f := range prev.Fills {
		g, ok := now[f.FillID]
		if !ok {
			return fmt.Sprintf("fill %s reported earlier is missing from the newer evidence", f.FillID)
		}
		if g != f {
			return fmt.Sprintf("fill %s changed between observations (%+v, then %+v)", f.FillID, f, g)
		}
	}
	return ""
}

func (bc *basketCoordinator) pairFillContextFor(rt *basketRuntime, a *basketAction, remaining float64, now int64) *pairFillContext {
	if rt.Pair == nil || rt.Pair.funding == nil {
		return nil
	}
	m := rt.Pair.funding.members[a.StrategyID]
	if m == nil {
		return nil
	}
	var out basketVenueOutcome
	_ = json.Unmarshal([]byte(a.EvidenceJSON), &out)
	ctx := &pairFillContext{ActionID: a.ActionID}
	journaled := m.journaledFills(a.ActionID)
	for _, f := range out.Fills {
		if journaled[f.FillID] {
			continue
		}
		q, err := quoteRatFromFloatDecimal(f.BaseQty)
		if err != nil {
			ctx.Pieces = nil
			break
		}
		ctx.Pieces = append(ctx.Pieces, pairFillPiece{FillID: f.FillID, Qty: q, EventMs: f.EventMs, VisibleMs: f.VisibleMs})
	}
	if len(ctx.Pieces) > 0 {
		sum := 0.0
		for _, f := range out.Fills {
			if !journaled[f.FillID] {
				sum += f.BaseQty
			}
		}
		if !basketQtyEqual(sum, remaining) {
			ctx.Pieces = nil
		}
	}
	lo := out.AcceptedAtMs
	if lo <= 0 {
		lo = a.SubmittedAtMs
	}
	if lo <= 0 {
		lo = a.CreatedAtMs
	}
	hi := out.ObservedAtMs
	if hi <= 0 || hi > now {
		hi = now
	}
	if lo <= 0 || lo > hi {
		lo = hi
	}
	ctx.LoMs, ctx.HiMs, ctx.VisibleMs = lo, hi, hi
	return ctx
}
