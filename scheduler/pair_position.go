package main

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
)

const pairVenuePositionVersion = 2

const (
	pairPositionUnchecked = "unchecked"
	pairPositionMatched   = "matched"
	pairPositionMismatch  = "mismatch"
	pairPositionFailed    = "failed"
)

type pairVenuePosition struct {
	V               int             `json:"v"`
	Key             pairEvidenceKey `json:"key"`
	SignedNativeQty string          `json:"signed_native_qty"`
	ContractSize    string          `json:"contract_size"`
	PositionMode    string          `json:"position_mode"`
	MarginMode      string          `json:"margin_mode"`
	ObservedMs      int64           `json:"observed_ms"`
	FirstReceiptMs  int64           `json:"first_receipt_ms"`
	EvidenceID      string          `json:"evidence_id"`
	EvidenceHash    string          `json:"evidence_hash"`
}

func (p pairVenuePosition) computeHash() (string, error) {
	c := p
	c.EvidenceHash = ""
	return pairRecordHashOf(c)
}

func (p pairVenuePosition) sealed() (pairVenuePosition, error) {
	h, err := p.computeHash()
	if err != nil {
		return p, err
	}
	p.EvidenceHash = h
	return p, nil
}

type pairPositionMatch struct {
	Generation  string `json:"generation"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	CheckedAtMs int64  `json:"checked_at_ms"`
	EpisodeMs   int64  `json:"episode_ms,omitempty"`
	HeldSinceMs int64  `json:"held_since_ms,omitempty"`
}

type pairPositionAuth struct {
	Generation string
	LimitMs    int64
	ReadAtMs   int64
}

type pairPositionState struct {
	auth          *pairPositionAuth
	attemptMs     int64
	attemptGen    string
	readsInFlight bool
}

func pairPositionBase(p pairVenuePosition) (*big.Rat, error) {
	q, err := pairCoverageRat("signed native quantity", p.SignedNativeQty, false)
	if err != nil {
		return nil, err
	}
	cs, err := pairCoverageRat("contract size", p.ContractSize, true)
	if err != nil {
		return nil, err
	}
	return new(big.Rat).Mul(q, cs), nil
}

func pairCheckVenuePosition(rt *basketRuntime, m pairContractMember, p pairVenuePosition, nowMs int64) error {
	if p.V != pairVenuePositionVersion {
		return fmt.Errorf("position record version %d, want %d", p.V, pairVenuePositionVersion)
	}
	want := pairEvidenceKey{Partition: rt.Partition.String(), Provider: m.Provider, AccountKey: m.AccountKey, Instrument: m.Instrument, MemberID: m.MemberID}
	if p.Key != want {
		return fmt.Errorf("position record key %s does not name member %s", p.Key, want)
	}
	h, err := p.computeHash()
	if err != nil {
		return err
	}
	if h != p.EvidenceHash {
		return fmt.Errorf("position record %s hash does not match its content", p.EvidenceID)
	}
	if !pairPrintableASCII(p.EvidenceID, pairEvidenceMaxIDLen) {
		return fmt.Errorf("position record has no printable evidence id")
	}
	if !pairDecimalCanonical(p.SignedNativeQty) {
		return fmt.Errorf("position quantity %q is not a canonical decimal", p.SignedNativeQty)
	}
	wantCS, err := pairDecimalFromFloat(m.Meta.ContractSize)
	if err != nil {
		return err
	}
	if p.ContractSize != string(wantCS) {
		return fmt.Errorf("position contract size %s differs from the contract size %s", p.ContractSize, wantCS)
	}
	if p.PositionMode != m.Meta.PositionMode {
		return fmt.Errorf("position mode %q differs from the contract position mode %q", p.PositionMode, m.Meta.PositionMode)
	}
	zero := p.SignedNativeQty == "0"
	if !(zero && p.MarginMode == "") && p.MarginMode != m.Meta.MarginMode {
		return fmt.Errorf("position margin mode %q differs from the contract margin mode %q", p.MarginMode, m.Meta.MarginMode)
	}
	if p.ObservedMs <= 0 || p.ObservedMs > p.FirstReceiptMs || p.FirstReceiptMs > nowMs {
		return fmt.Errorf("position times observed %d, received %d and coordinator %d are not ordered", p.ObservedMs, p.FirstReceiptMs, nowMs)
	}
	if !pairAgeWithin(nowMs, p.ObservedMs, rt.Pair.Limits.QuoteAgeMs) || !pairAgeWithin(nowMs, p.FirstReceiptMs, rt.Pair.Limits.QuoteAgeMs) {
		return fmt.Errorf("position observed %d and received %d are not within quote_age_ms %d of %d", p.ObservedMs, p.FirstReceiptMs, rt.Pair.Limits.QuoteAgeMs, nowMs)
	}
	return nil
}

func (bc *basketCoordinator) pairLiveMembers(rt *basketRuntime) ([]pairContractMember, []pairVenue, error) {
	var ms []pairContractMember
	var vs []pairVenue
	for _, m := range rt.Contract.pairMembersSorted() {
		sc, ok := bc.members[m.MemberID]
		if !ok {
			return nil, nil, fmt.Errorf("member %s has no configuration", m.MemberID)
		}
		v, err := bc.pairVenueFor(sc)
		if err != nil {
			return nil, nil, err
		}
		if !v.LiveShaped() {
			continue
		}
		ms, vs = append(ms, m), append(vs, v)
	}
	return ms, vs, nil
}

func (bc *basketCoordinator) pairPositionGeneration(rt *basketRuntime, exclude string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "p|%s|%s|%s\n", rt.Partition.String(), rt.Contract.BasketID, rt.Contract.Fingerprint())
	for _, m := range rt.Contract.pairMembersSorted() {
		fmt.Fprintf(&b, "m|%s|%s|%s|%s|%s|%.17g\n", m.MemberID, m.Provider, m.AccountKey, m.Instrument, m.Symbol, basketPositionSigned(bc.state.Strategies[m.MemberID], m.Symbol))
	}
	for _, id := range rt.sortedActionIDs() {
		if id == exclude {
			continue
		}
		a := rt.Actions[id]
		fmt.Fprintf(&b, "a|%s|%s|%s|%s|%s|%.17g|%.17g|%.17g|%t|%t\n", id, a.StrategyID, a.Status, a.Direction, a.OrderSide, a.RequestedQty, a.ConfirmedQty, a.AppliedQty, a.Terminal, a.recoveryNote().NativeBooked)
	}
	return pairSHA256([]byte(b.String()))
}

func (bc *basketCoordinator) pairReachable(rt *basketRuntime, m pairContractMember, exclude string) (lo, hi float64) {
	book := basketPositionSigned(bc.state.Strategies[m.MemberID], m.Symbol)
	lo, hi = book, book
	for _, id := range rt.sortedActionIDs() {
		a := rt.Actions[id]
		if a.StrategyID != m.MemberID || id == exclude {
			continue
		}
		if d := a.ConfirmedQty - a.AppliedQty; d > 1e-12 {
			lo += a.signedDelta() * d
			hi += a.signedDelta() * d
		}
		if !basketActionPending(a.Status) || a.Status == basketActionReserved || a.Terminal {
			continue
		}
		if rem := a.RequestedQty - a.ConfirmedQty; rem > 1e-12 {
			if d := a.signedDelta() * rem; d > 0 {
				hi += d
			} else {
				lo += d
			}
		}
	}
	return lo, hi
}

func pairWithinReach(v, lo, hi float64) bool {
	return (v >= lo || basketQtyEqual(v, lo)) && (v <= hi || basketQtyEqual(v, hi))
}

func (bc *basketCoordinator) pairPositionAuthorized(rt *basketRuntime, exclude string, nowMs int64) string {
	st := rt.pairPositions
	if st == nil || st.auth == nil {
		return "no fresh venue position read matches the pair records since the last change"
	}
	if st.auth.Generation != bc.pairPositionGeneration(rt, exclude) {
		return "the pair records changed after the last matching venue position read"
	}
	if nowMs > st.auth.LimitMs {
		return fmt.Sprintf("the last matching venue position read is older than quote_age_ms (valid through %d, now %d)", st.auth.LimitMs, nowMs)
	}
	return ""
}

func (bc *basketCoordinator) notePairPositionStatus(rt *basketRuntime, status, reason, gen string, nowMs int64) bool {
	p := &rt.Pair.Progress
	prev := p.PositionMatch
	next := pairPositionMatch{Generation: gen, Status: status, Reason: reason, CheckedAtMs: nowMs}
	if prev != nil {
		next.EpisodeMs, next.HeldSinceMs = prev.EpisodeMs, prev.HeldSinceMs
	}
	switch status {
	case pairPositionMatched:
		next.EpisodeMs, next.HeldSinceMs = 0, 0
	case pairPositionMismatch:
		if next.EpisodeMs == 0 {
			next.EpisodeMs = nowMs
		}
		if next.HeldSinceMs == 0 {
			next.HeldSinceMs = nowMs
		}
	default:
		if next.HeldSinceMs == 0 {
			next.HeldSinceMs = nowMs
		}
	}
	if prev != nil && prev.Status == next.Status && prev.Reason == next.Reason && prev.Generation == next.Generation && prev.EpisodeMs == next.EpisodeMs && prev.HeldSinceMs == next.HeldSinceMs {
		return false
	}
	p.PositionMatch = &next
	rt.touchPairProgress()
	return true
}

func (bc *basketCoordinator) savePairPositionMatch(rt *basketRuntime) {
	if !bc.persistPair(rt) {
		rt.needsDurable = true
	}
}

func (bc *basketCoordinator) pairReadPositions(rt *basketRuntime, in basketCycleInputs, exclude string) (basketCycleInputs, string) {
	ms, vs, err := bc.pairLiveMembers(rt)
	if err != nil {
		return in, err.Error()
	}
	if len(ms) == 0 {
		return in, ""
	}
	if rt.pairPositions == nil {
		rt.pairPositions = &pairPositionState{}
	}
	st := rt.pairPositions
	if st.readsInFlight {
		return in, "a venue position read is already in progress"
	}
	gen := bc.pairPositionGeneration(rt, exclude)
	fp := rt.Contract.Fingerprint()
	startMs := bc.pairNow(in).UnixMilli()
	st.attemptMs, st.attemptGen = startMs, gen
	limit := time.Duration(rt.Pair.Limits.CollectionTimeoutMs) * time.Millisecond
	parent := bc.parent
	if parent == nil {
		parent = context.Background()
	}
	got := make([]pairVenuePosition, len(ms))
	errs := make([]error, len(ms))
	st.readsInFlight = true
	var ctxErr error
	bc.unlocked(func() {
		ctx, cancel := context.WithTimeout(parent, limit)
		defer cancel()
		type answer struct {
			i   int
			p   pairVenuePosition
			err error
		}
		answers := make(chan answer, len(ms))
		for i := range ms {
			go func(i int, v pairVenue, m pairContractMember) {
				p, err := v.Position(ctx, m, startMs)
				answers <- answer{i: i, p: p, err: err}
			}(i, vs[i], ms[i])
		}
		for n := 0; n < len(ms); n++ {
			select {
			case a := <-answers:
				got[a.i], errs[a.i] = a.p, a.err
			case <-ctx.Done():
				ctxErr = ctx.Err()
				return
			}
		}
		ctxErr = ctx.Err()
	})
	st.readsInFlight = false
	in = bc.refreshPairInputs(rt, in)
	nowMs := in.Now.UnixMilli()
	if rt.Contract.Fingerprint() != fp || bc.pairPositionGeneration(rt, exclude) != gen {
		st.auth = nil
		return in, "the pair records changed while the venue positions were read; the answer is discarded"
	}
	fail := func(reason string) (basketCycleInputs, string) {
		st.auth = nil
		if bc.notePairPositionStatus(rt, pairPositionFailed, reason, gen, nowMs) {
			bc.savePairPositionMatch(rt)
			bc.pairLogf(rt, "increases hold: %s; queries, settlement, closes and protection continue", reason)
		}
		return in, reason
	}
	if ctxErr != nil {
		return fail(fmt.Sprintf("the venue position reads ended with %v within collection_timeout_ms %d", ctxErr, rt.Pair.Limits.CollectionTimeoutMs))
	}
	limitMs := int64(math.MaxInt64)
	var mismatches []string
	for i, m := range ms {
		if errs[i] != nil {
			return fail(fmt.Sprintf("the %s position read failed: %v", m.MemberID, errs[i]))
		}
		if err := pairCheckVenuePosition(rt, m, got[i], nowMs); err != nil {
			return fail(fmt.Sprintf("the %s position read is refused: %v", m.MemberID, err))
		}
		base, err := pairPositionBase(got[i])
		if err != nil {
			return fail(fmt.Sprintf("the %s position read is refused: %v", m.MemberID, err))
		}
		v := pairRatFloat(base)
		lo, hi := bc.pairReachable(rt, m, exclude)
		if !pairWithinReach(v, lo, hi) {
			mismatches = append(mismatches, fmt.Sprintf("%s venue holds %.10g base, outside the reachable range [%.10g, %.10g] from the book, unapplied fills and possible unresolved fills", m.MemberID, v, lo, hi))
		}
		if end, ok := pairAddMs(got[i].ObservedMs, rt.Pair.Limits.QuoteAgeMs); ok && end < limitMs {
			limitMs = end
		}
	}
	if len(mismatches) > 0 {
		sort.Strings(mismatches)
		reason := strings.Join(mismatches, "; ")
		st.auth = nil
		changed := bc.notePairPositionStatus(rt, pairPositionMismatch, reason, gen, nowMs)
		episode := rt.Pair.Progress.PositionMatch.EpisodeMs
		bc.pairOpenRecovery(rt, "venue position mismatch: "+reason, in.Now)
		bc.pairAlert(rt, fmt.Sprintf("position_match|%d", episode), fmt.Sprintf("**PAIR POSITION MISMATCH** %s: %s; increases are frozen, recovery is open and no unmatched residual is adopted into the book", rt.Contract.BasketID, reason), true)
		_ = changed
		bc.savePairPositionMatch(rt)
		return in, reason
	}
	st.auth = &pairPositionAuth{Generation: gen, LimitMs: limitMs, ReadAtMs: nowMs}
	if bc.notePairPositionStatus(rt, pairPositionMatched, "", gen, nowMs) {
		bc.savePairPositionMatch(rt)
	}
	return in, ""
}

func (bc *basketCoordinator) stepPairPositions(rt *basketRuntime, in basketCycleInputs) basketCycleInputs {
	if bc.state.partitionLatched(rt.Partition) {
		return in
	}
	ms, _, err := bc.pairLiveMembers(rt)
	if err != nil || len(ms) == 0 {
		return in
	}
	if rt.pairPositions == nil {
		rt.pairPositions = &pairPositionState{}
	}
	st := rt.pairPositions
	nowMs := in.Now.UnixMilli()
	gen := bc.pairPositionGeneration(rt, "")
	due := st.attemptMs == 0 || st.attemptGen != gen || nowMs-st.attemptMs >= rt.Pair.Limits.DecisionCadenceMs
	if !due {
		return in
	}
	in, why := bc.pairReadPositions(rt, in, "")
	if why != "" {
		bc.pairPositionOpeningDeadline(rt, in.Now)
	}
	return in
}

func (bc *basketCoordinator) pairPositionOpeningDeadline(rt *basketRuntime, now time.Time) {
	p := &rt.Pair.Progress
	pm := p.PositionMatch
	if p.Lifecycle != pairLifecycleOpening || pm == nil || pm.HeldSinceMs == 0 || pm.Status == pairPositionMatched {
		return
	}
	var net float64
	for _, id := range rt.memberIDs() {
		net += bc.pairExposure(rt, id)
	}
	if math.Abs(net) <= 1e-12 {
		return
	}
	horizon, ok := pairAddMs(rt.Pair.Limits.AcceptanceDeadlineMs, entryRecoveryGrace.Milliseconds())
	if !ok || !pairReached(now.UnixMilli(), pm.HeldSinceMs, horizon) {
		return
	}
	bc.pairOpenRecovery(rt, fmt.Sprintf("the venue position barrier has held the hedge of a one-sided opening since %s (%s), past the acceptance deadline plus the absence grace", formatClosedBarMs(pm.HeldSinceMs), pm.Reason), now)
	bc.savePairPositionMatch(rt)
}
