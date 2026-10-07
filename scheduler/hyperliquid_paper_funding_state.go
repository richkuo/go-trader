package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const paperFundingStateVersion = 1

var paperFundingClock = func() time.Time { return time.Now().UTC() }

func paperFundingNowMs() int64 {
	return paperFundingClock().UTC().UnixMilli()
}

type paperFundingAnchor struct {
	SignedQty  float64 `json:"signed_qty"`
	PositionID string  `json:"position_id"`
	SinceMs    int64   `json:"since_ms"`
	VerifiedMs int64   `json:"verified_ms"`
}

type paperFundingSegment struct {
	SignedQty  float64 `json:"signed_qty"`
	PositionID string  `json:"position_id"`
	FromMs     int64   `json:"from_ms"`
	ToMs       int64   `json:"to_ms"`
}

type paperFundingUnknown struct {
	FromMs     int64   `json:"from_ms"`
	ToMs       int64   `json:"to_ms"`
	QtyBefore  float64 `json:"qty_before"`
	QtyAfter   float64 `json:"qty_after"`
	PosBefore  string  `json:"pos_before"`
	PosAfter   string  `json:"pos_after"`
	DetectedMs int64   `json:"detected_ms"`
}

type paperFundingHeld struct {
	TimeMs     int64   `json:"time_ms"`
	Rate       float64 `json:"rate"`
	Reason     string  `json:"reason"`
	SignedQty  float64 `json:"signed_qty"`
	PositionID string  `json:"position_id"`
}

type paperFundingGap struct {
	AfterMs    int64 `json:"after_ms"`
	BeforeMs   int64 `json:"before_ms"`
	DetectedMs int64 `json:"detected_ms"`
}

type paperFundingSample struct {
	AtMs int64   `json:"at_ms"`
	Px   float64 `json:"px"`
}

type paperFundingCoin struct {
	Anchor           paperFundingAnchor    `json:"anchor"`
	Segments         []paperFundingSegment `json:"segments,omitempty"`
	Unknown          []paperFundingUnknown `json:"unknown,omitempty"`
	SettledThroughMs int64                 `json:"settled_through_ms"`
	LastRecordMs     int64                 `json:"last_record_ms,omitempty"`
	Held             []paperFundingHeld    `json:"held,omitempty"`
	Gaps             []paperFundingGap     `json:"gaps,omitempty"`
	Samples          []paperFundingSample  `json:"samples,omitempty"`
}

type paperFundingState struct {
	V                 int                          `json:"v"`
	StartedAtMs       int64                        `json:"started_at_ms"`
	ObservedThroughMs int64                        `json:"observed_through_ms"`
	Coins             map[string]*paperFundingCoin `json:"coins"`

	active map[string]bool
	alerts []paperFundingAlert
}

type paperFundingAlert struct {
	Coin     string
	Kind     string
	Critical bool
	Detail   string
}

func newPaperFundingState(nowMs int64) *paperFundingState {
	return &paperFundingState{
		V:                 paperFundingStateVersion,
		StartedAtMs:       nowMs,
		ObservedThroughMs: nowMs,
		Coins:             make(map[string]*paperFundingCoin),
	}
}

func (st *paperFundingState) alert(coin, kind string, critical bool, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	st.alerts = append(st.alerts, paperFundingAlert{Coin: coin, Kind: kind, Critical: critical, Detail: detail})
	level := "WARN"
	if critical {
		level = "CRITICAL"
	}
	fmt.Printf("[%s] [paper-funding] %s %s: %s\n", level, coin, kind, detail)
}

func (st *paperFundingState) drainAlerts() []paperFundingAlert {
	out := st.alerts
	st.alerts = nil
	return out
}

func paperFundingQtyEqual(a, b float64) bool {
	if a == b {
		return true
	}
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return math.Abs(a-b) <= 1e-12*scale
}

func paperFundingBookQty(s *StrategyState, symbol string) (float64, string, string) {
	pos := s.Positions[symbol]
	if pos == nil {
		return 0, "", ""
	}
	mult := pos.Multiplier
	if mult <= 0 {
		mult = 1
	}
	if math.IsNaN(pos.Quantity) || math.IsInf(pos.Quantity, 0) || math.IsNaN(mult) || math.IsInf(mult, 0) {
		return 0, "", fmt.Sprintf("position quantity %v (multiplier %v) is not finite", pos.Quantity, pos.Multiplier)
	}
	if pos.Quantity <= 0 {
		return 0, "", ""
	}
	q := pos.Quantity * mult
	switch pos.Side {
	case "long":
	case "short":
		q = -q
	default:
		return 0, "", fmt.Sprintf("position side %q is neither long nor short", pos.Side)
	}
	return q, ensurePositionTradeID(s.ID, symbol, pos), ""
}

func (st *paperFundingState) coin(symbol string, atMs int64) *paperFundingCoin {
	if c := st.Coins[symbol]; c != nil {
		return c
	}
	verified := st.ObservedThroughMs
	if verified < st.StartedAtMs {
		verified = st.StartedAtMs
	}
	if verified > atMs {
		verified = atMs
	}
	c := &paperFundingCoin{
		Anchor:           paperFundingAnchor{SinceMs: verified, VerifiedMs: verified},
		SettledThroughMs: verified,
	}
	if st.Coins == nil {
		st.Coins = make(map[string]*paperFundingCoin)
	}
	st.Coins[symbol] = c
	return c
}

func (c *paperFundingCoin) clampMs(atMs int64) int64 {
	if atMs < c.Anchor.SinceMs {
		atMs = c.Anchor.SinceMs
	}
	if atMs < c.Anchor.VerifiedMs {
		atMs = c.Anchor.VerifiedMs
	}
	return atMs
}

func (c *paperFundingCoin) appendSegment(qty float64, pid string, fromMs, toMs int64) {
	if toMs <= fromMs || qty == 0 {
		return
	}
	if n := len(c.Segments); n > 0 {
		last := &c.Segments[n-1]
		if last.ToMs == fromMs && last.PositionID == pid && paperFundingQtyEqual(last.SignedQty, qty) {
			last.ToMs = toMs
			return
		}
	}
	c.Segments = append(c.Segments, paperFundingSegment{SignedQty: qty, PositionID: pid, FromMs: fromMs, ToMs: toMs})
}

func (c *paperFundingCoin) anchorMatches(qty float64, pid string) bool {
	return paperFundingQtyEqual(c.Anchor.SignedQty, qty) && (qty == 0 || c.Anchor.PositionID == pid)
}

func (c *paperFundingCoin) verify(qty float64, pid string, atMs int64) bool {
	if c.anchorMatches(qty, pid) {
		if atMs > c.Anchor.VerifiedMs {
			c.Anchor.VerifiedMs = atMs
		}
		return true
	}
	a := c.Anchor
	c.appendSegment(a.SignedQty, a.PositionID, a.SinceMs, a.VerifiedMs)
	if atMs > a.VerifiedMs {
		c.Unknown = append(c.Unknown, paperFundingUnknown{
			FromMs: a.VerifiedMs, ToMs: atMs,
			QtyBefore: a.SignedQty, QtyAfter: qty,
			PosBefore: a.PositionID, PosAfter: pid,
			DetectedMs: atMs,
		})
	}
	c.Anchor = paperFundingAnchor{SignedQty: qty, PositionID: pid, SinceMs: atMs, VerifiedMs: atMs}
	return false
}

func (c *paperFundingCoin) moveTo(qty float64, pid string, atMs int64) {
	if c.anchorMatches(qty, pid) {
		if atMs > c.Anchor.VerifiedMs {
			c.Anchor.VerifiedMs = atMs
		}
		return
	}
	a := c.Anchor
	c.appendSegment(a.SignedQty, a.PositionID, a.SinceMs, atMs)
	c.Anchor = paperFundingAnchor{SignedQty: qty, PositionID: pid, SinceMs: atMs, VerifiedMs: atMs}
}

func mutatePaperPerpsBook(s *StrategyState, symbol string, mutate func()) {
	mutatePaperPerpsBookCoins(s, []string{symbol}, mutate)
}

func mutatePaperPerpsBookCoins(s *StrategyState, symbols []string, mutate func()) {
	if s == nil || s.PaperFunding == nil {
		mutate()
		return
	}
	st := s.PaperFunding
	if st.active == nil {
		st.active = make(map[string]bool)
	}
	var tracked []string
	seen := make(map[string]bool, len(symbols))
	for _, sym := range symbols {
		if sym == "" || st.active[sym] || seen[sym] {
			continue
		}
		seen[sym] = true
		tracked = append(tracked, sym)
	}
	if len(tracked) == 0 {
		mutate()
		return
	}
	nowMs := paperFundingNowMs()
	for _, sym := range tracked {
		c := st.coin(sym, nowMs)
		atMs := c.clampMs(nowMs)
		if atMs != nowMs {
			fmt.Printf("[WARN] [paper-funding] %s %s: clock %d is behind the inventory anchor; booking the change at %d\n", s.ID, sym, nowMs, atMs)
		}
		qty, pid, bad := paperFundingBookQty(s, sym)
		if bad != "" {
			st.alert(sym, "bad_position", true, "%s: %s; counted as flat", s.ID, bad)
		}
		if !c.verify(qty, pid, atMs) {
			st.alert(sym, "helper_bypass", true, "%s: book qty %.10g (position %s) does not match the inventory anchor before a change at %d; the window since the last check is unknown and any funding record inside it is held",
				s.ID, qty, pid, atMs)
		}
		st.active[sym] = true
	}
	defer func() {
		for _, sym := range tracked {
			delete(st.active, sym)
		}
	}()
	mutate()
	for _, sym := range tracked {
		c := st.Coins[sym]
		if c == nil {
			continue
		}
		atMs := c.clampMs(nowMs)
		qty, pid, bad := paperFundingBookQty(s, sym)
		if bad != "" {
			st.alert(sym, "bad_position", true, "%s: %s; counted as flat", s.ID, bad)
		}
		c.moveTo(qty, pid, atMs)
	}
}

func observePaperFunding(s *StrategyState, nowMs int64) {
	if s == nil || s.PaperFunding == nil {
		return
	}
	st := s.PaperFunding
	symbols := make(map[string]bool, len(st.Coins)+len(s.Positions))
	for sym := range st.Coins {
		symbols[sym] = true
	}
	for sym := range s.Positions {
		symbols[sym] = true
	}
	ordered := make([]string, 0, len(symbols))
	for sym := range symbols {
		ordered = append(ordered, sym)
	}
	sort.Strings(ordered)
	for _, sym := range ordered {
		c := st.coin(sym, nowMs)
		atMs := c.clampMs(nowMs)
		qty, pid, bad := paperFundingBookQty(s, sym)
		if bad != "" {
			st.alert(sym, "bad_position", true, "%s: %s; counted as flat", s.ID, bad)
		}
		if !c.verify(qty, pid, atMs) {
			st.alert(sym, "helper_bypass", true, "%s: book qty %.10g (position %s) changed outside the inventory helper; the window since the last check is unknown and any funding record inside it is held",
				s.ID, qty, pid)
		}
	}
	if nowMs > st.ObservedThroughMs {
		st.ObservedThroughMs = nowMs
	}
}

func syncPaperFundingEligibility(state *AppState, cfgs []StrategyConfig, now time.Time) []string {
	if state == nil {
		return nil
	}
	nowMs := now.UTC().UnixMilli()
	var lines []string
	for _, sc := range cfgs {
		s := state.Strategies[sc.ID]
		if s == nil {
			continue
		}
		if !paperFundingEligible(sc) {
			if s.PaperFunding != nil || s.paperFundingCorrupt || s.paperFundingRaw != "" {
				lines = append(lines, fmt.Sprintf("[paper-funding] %s: no longer a paper Hyperliquid perps or manual strategy; dropping its paper funding state and booking nothing", sc.ID))
				s.PaperFunding = nil
				s.paperFundingCorrupt = false
				s.paperFundingRaw = ""
			}
			continue
		}
		if s.paperFundingCorrupt {
			lines = append(lines, fmt.Sprintf("[CRITICAL] [paper-funding] %s: stored paper funding state is unreadable; accounting is held and the stored text is kept unchanged", sc.ID))
			continue
		}
		if s.PaperFunding != nil {
			continue
		}
		st := newPaperFundingState(nowMs)
		syms := make([]string, 0, len(s.Positions))
		for sym := range s.Positions {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for _, sym := range syms {
			qty, pid, bad := paperFundingBookQty(s, sym)
			if bad != "" {
				lines = append(lines, fmt.Sprintf("[CRITICAL] [paper-funding] %s %s: %s; counted as flat", sc.ID, sym, bad))
			}
			c := st.coin(sym, nowMs)
			c.Anchor = paperFundingAnchor{SignedQty: qty, PositionID: pid, SinceMs: nowMs, VerifiedMs: nowMs}
		}
		s.PaperFunding = st
		lines = append(lines, fmt.Sprintf("[paper-funding] %s: paper funding accounting starts at %d (no historical replay; %d open position(s) anchored)", sc.ID, nowMs, len(syms)))
	}
	return lines
}

func marshalPaperFundingState(s *StrategyState) (string, error) {
	if s == nil {
		return "", nil
	}
	if s.paperFundingCorrupt {
		return s.paperFundingRaw, nil
	}
	if s.PaperFunding == nil {
		return "", nil
	}
	blob, err := json.Marshal(s.PaperFunding)
	if err != nil {
		return "", fmt.Errorf("marshal paper funding state for %s: %w", s.ID, err)
	}
	return string(blob), nil
}

func unmarshalPaperFundingState(s *StrategyState, raw string) {
	s.PaperFunding = nil
	s.paperFundingCorrupt = false
	s.paperFundingRaw = ""
	if raw == "" {
		return
	}
	var st paperFundingState
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(&st)
	if err == nil && dec.More() {
		err = fmt.Errorf("trailing data")
	}
	if err == nil {
		err = validatePaperFundingState(&st)
	}
	if err != nil {
		fmt.Printf("[CRITICAL] [paper-funding] %s: stored paper funding state is unreadable (%v); accounting is held and the stored text is kept unchanged\n", s.ID, err)
		s.paperFundingCorrupt = true
		s.paperFundingRaw = raw
		return
	}
	if st.Coins == nil {
		st.Coins = make(map[string]*paperFundingCoin)
	}
	s.PaperFunding = &st
}

func paperFundingFinite(vals ...float64) bool {
	for _, v := range vals {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func validatePaperFundingState(st *paperFundingState) error {
	if st.V != paperFundingStateVersion {
		return fmt.Errorf("version %d, want %d", st.V, paperFundingStateVersion)
	}
	if st.StartedAtMs <= 0 || st.ObservedThroughMs < st.StartedAtMs {
		return fmt.Errorf("started_at_ms %d and observed_through_ms %d are not ordered", st.StartedAtMs, st.ObservedThroughMs)
	}
	for sym, c := range st.Coins {
		if sym == "" || c == nil {
			return fmt.Errorf("coin entry %q is empty", sym)
		}
		a := c.Anchor
		if !paperFundingFinite(a.SignedQty) || a.SinceMs <= 0 || a.VerifiedMs < a.SinceMs || (a.SignedQty != 0 && a.PositionID == "") {
			return fmt.Errorf("coin %s anchor %+v is invalid", sym, a)
		}
		if c.SettledThroughMs <= 0 {
			return fmt.Errorf("coin %s settled_through_ms %d is invalid", sym, c.SettledThroughMs)
		}
		var lastTo int64
		for i, seg := range c.Segments {
			if !paperFundingFinite(seg.SignedQty) || seg.SignedQty == 0 || seg.PositionID == "" || seg.FromMs >= seg.ToMs || (i > 0 && seg.FromMs < lastTo) {
				return fmt.Errorf("coin %s segment %d %+v is invalid or out of order", sym, i, seg)
			}
			lastTo = seg.ToMs
		}
		if len(c.Segments) > 0 && a.SinceMs < lastTo {
			return fmt.Errorf("coin %s anchor starts at %d inside its last segment ending %d", sym, a.SinceMs, lastTo)
		}
		var lastUnknown int64
		for i, u := range c.Unknown {
			if !paperFundingFinite(u.QtyBefore, u.QtyAfter) || u.FromMs >= u.ToMs || u.FromMs < lastUnknown {
				return fmt.Errorf("coin %s unknown window %d %+v is invalid or out of order", sym, i, u)
			}
			lastUnknown = u.ToMs
		}
		for i, h := range c.Held {
			if !paperFundingFinite(h.Rate, h.SignedQty) || h.TimeMs <= 0 || h.Reason == "" {
				return fmt.Errorf("coin %s held item %d is invalid", sym, i)
			}
		}
		for i, g := range c.Gaps {
			if g.AfterMs >= g.BeforeMs {
				return fmt.Errorf("coin %s gap %d is invalid", sym, i)
			}
		}
		var lastSample int64
		for i, smp := range c.Samples {
			if !paperFundingFinite(smp.Px) || smp.Px <= 0 || smp.AtMs <= 0 || smp.AtMs < lastSample {
				return fmt.Errorf("coin %s sample %d is invalid or out of order", sym, i)
			}
			lastSample = smp.AtMs
		}
	}
	return nil
}
