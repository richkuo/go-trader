package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
)

const pairObligationReductionReference = "reduction_reference_unavailable"

type pairReductionRef struct {
	Ref          pairObservation
	Side         string
	Px           string
	Generation   uint64
	EvidenceHash string
	Exposure     float64
}

func pairReferencePx(ref pairObservation, side string) (string, *big.Rat, error) {
	levels := ref.NativeBids
	what := "best bid"
	switch side {
	case "sell":
	case "buy":
		levels, what = ref.NativeAsks, "best ask"
	default:
		return "", nil, fmt.Errorf("reference side %q is neither buy nor sell", side)
	}
	if len(levels) == 0 {
		return "", nil, fmt.Errorf("the reference has no %s", what)
	}
	text := levels[0].Px
	px, ok := new(big.Rat).SetString(text)
	if !ok || px.Sign() <= 0 || !pairDecimalCanonical(text) {
		return "", nil, fmt.Errorf("the reference %s %q is not a positive canonical decimal", what, text)
	}
	return text, px, nil
}

func pairBoundCrossesPx(side string, bound float64, px *big.Rat) bool {
	if px == nil || math.IsNaN(bound) || math.IsInf(bound, 0) || bound <= 0 {
		return false
	}
	b, err := quoteRatFromFloatDecimal(bound)
	if err != nil {
		return false
	}
	switch side {
	case "buy":
		return b.Cmp(px) >= 0
	case "sell":
		return b.Cmp(px) <= 0
	}
	return false
}

func pairReductionBound(m pairContractMember, side string, ref pairObservation, nowMs int64, lim pairOperationalLimits) (float64, string) {
	w := lim.ProtectiveWidthBps
	if math.IsNaN(w) || math.IsInf(w, 0) || w <= 0 || w >= 10000 {
		return 0, fmt.Sprintf("protective_width_bps %v is not finite, above 0 and below 10000", w)
	}
	if !pairAgeWithin(nowMs, ref.FirstReceiptMs, lim.QuoteAgeMs) {
		return 0, fmt.Sprintf("the reference receipt %d is after %d or older than quote_age_ms %d", ref.FirstReceiptMs, nowMs, lim.QuoteAgeMs)
	}
	_, px, err := pairReferencePx(ref, side)
	if err != nil {
		return 0, err.Error()
	}
	top := quoteRatFloat(px)
	raw := top * (1 - w/10000)
	if side == "buy" {
		raw = top * (1 + w/10000)
	}
	if !basketFinitePositive(raw) {
		return 0, fmt.Sprintf("the %s width bound %v from the reference level %s is not finite and positive", side, raw, px.FloatString(12))
	}
	meta, err := pairExpectedWireMeta(m)
	if err != nil {
		return 0, fmt.Sprintf("member %s contract meta: %v", m.MemberID, err)
	}
	bound, err := pairRoundBoundRule(m.Instrument, meta, side, raw)
	if err != nil {
		return 0, fmt.Sprintf("the %s width bound %v cannot be rounded safe-side to the venue grid: %v", side, raw, err)
	}
	if !pairBoundCrossesPx(side, bound, px) {
		return 0, fmt.Sprintf("the rounded %s width bound %v does not cross the reference level %s", side, bound, px.FloatString(12))
	}
	return bound, ""
}

func pairPolicyExitEvidence(rt *basketRuntime, slotMs int64, memberID string) (pairBoundEvidence, bool, bool) {
	rb := rt.Rebalances[slotMs]
	if rb == nil {
		return pairBoundEvidence{}, false, false
	}
	t, err := parsePairTargets(rb.TargetsJSON)
	if err != nil || t == nil || t.Kind != pairPolicyStatusExit {
		return pairBoundEvidence{}, false, false
	}
	if t.legacy() {
		return pairBoundEvidence{}, false, true
	}
	ev, ok := t.BoundEvidence[memberID]
	return ev, ok, true
}

func (bc *basketCoordinator) pairReductionReference(rt *basketRuntime, m pairContractMember, side string, nowMs int64) (*pairReductionRef, string) {
	v, ok := rt.pairView[m.MemberID]
	if !ok {
		return nil, "the coordinator has published no reference for this member"
	}
	if !v.ReferenceValid || v.EvidenceMs <= 0 {
		why := v.MissingReason
		if why == "" {
			why = "no checked reference"
		}
		return nil, "the published view carries no committed reference: " + why
	}
	if v.ContractFingerprint != rt.Contract.Fingerprint() {
		return nil, "the published reference names another contract"
	}
	ref := pairCloneObservation(v.Observation)
	if err := pairCheckReference(rt, m, &ref, side, nowMs); err != nil {
		return nil, "the published reference is refused: " + err.Error()
	}
	px, _, err := pairReferencePx(ref, side)
	if err != nil {
		return nil, err.Error()
	}
	return &pairReductionRef{Ref: ref, Side: side, Px: px, Generation: v.Generation, EvidenceHash: v.EvidenceHash, Exposure: bc.pairExposure(rt, m.MemberID)}, ""
}

func (bc *basketCoordinator) pairReductionStillValid(rt *basketRuntime, c pairCandidate, nowMs int64) string {
	r := c.Ref
	if r == nil {
		return "the reduction has no saved reference"
	}
	v, ok := rt.pairView[c.MemberID]
	if !ok || v.Generation != r.Generation || v.EvidenceHash != r.EvidenceHash || !v.ReferenceValid {
		return "the reference publication changed after the reduction was planned"
	}
	if r.Side != c.OrderSide {
		return fmt.Sprintf("the reference proves %s depth, the reduction is a %s", r.Side, c.OrderSide)
	}
	if e := bc.pairExposure(rt, c.MemberID); !basketQtyEqual(e, r.Exposure) {
		return fmt.Sprintf("the member exposure moved from %.10g to %.10g after the reduction was planned", r.Exposure, e)
	}
	ref := pairCloneObservation(r.Ref)
	if err := pairCheckReference(rt, c.Member, &ref, c.OrderSide, nowMs); err != nil {
		return "the saved reference is no longer valid: " + err.Error()
	}
	_, px, err := pairReferencePx(ref, c.OrderSide)
	if err != nil {
		return err.Error()
	}
	if !pairBoundCrossesPx(c.OrderSide, c.PriceBound, px) {
		return fmt.Sprintf("the %s bound %v does not cross the reference level %s", c.OrderSide, c.PriceBound, r.Px)
	}
	return ""
}

func pairCheckReductionAuthority(rt *basketRuntime, m pairContractMember, a *basketAction, req pairActionRequest, nowMs int64) error {
	if req.Reference == nil {
		return fmt.Errorf("pair reduction %s has no saved reduction reference; a coordinator reduction is never reserved without one", a.ActionID)
	}
	if err := pairCheckReference(rt, m, req.Reference, req.OrderSide, nowMs); err != nil {
		return fmt.Errorf("pair reduction %s reference: %w", a.ActionID, err)
	}
	if req.ReferenceMaxAgeMs != rt.Pair.Limits.QuoteAgeMs || req.ReferenceReceiptMs != req.Reference.FirstReceiptMs {
		return fmt.Errorf("pair reduction %s reference age policy (receipt %d, max age %d) is not the saved reference receipt %d and quote_age_ms %d",
			a.ActionID, req.ReferenceReceiptMs, req.ReferenceMaxAgeMs, req.Reference.FirstReceiptMs, rt.Pair.Limits.QuoteAgeMs)
	}
	text, px, err := pairReferencePx(*req.Reference, req.OrderSide)
	if err != nil {
		return fmt.Errorf("pair reduction %s reference: %w", a.ActionID, err)
	}
	if req.ReferencePx != text {
		return fmt.Errorf("pair reduction %s reference level %q is not the saved reference level %q", a.ActionID, req.ReferencePx, text)
	}
	v, ok := rt.pairView[a.StrategyID]
	if !ok || !v.ReferenceValid || v.ContractFingerprint != rt.Contract.Fingerprint() || v.Observation.PayloadHash != req.Reference.PayloadHash ||
		v.Observation.FirstReceiptMs != req.Reference.FirstReceiptMs || v.Observation.Provenance != req.Reference.Provenance ||
		v.Observation.SnapshotID != req.Reference.SnapshotID || v.Observation.Generation != req.Reference.Generation {
		return fmt.Errorf("pair reduction %s reference is not the committed published reference of member %s", a.ActionID, a.StrategyID)
	}
	if req.PriceBound != a.PriceBound {
		return fmt.Errorf("pair reduction %s request bound %v is not the action bound %v", a.ActionID, req.PriceBound, a.PriceBound)
	}
	if ev, ok, policy := pairPolicyExitEvidence(rt, req.OriginSlotMs, a.StrategyID); policy && req.Purpose == pairPurposeExit {
		if !ok || ev.RoundedBound != req.PriceBound || ev.Side != req.OrderSide {
			return fmt.Errorf("pair reduction %s bound %v %s is not the saved policy exit bound of slot %d", a.ActionID, req.PriceBound, req.OrderSide, req.OriginSlotMs)
		}
	} else {
		want, why := pairReductionBound(m, req.OrderSide, *req.Reference, nowMs, rt.Pair.Limits)
		if why != "" {
			return fmt.Errorf("pair reduction %s width bound: %s", a.ActionID, why)
		}
		if want != req.PriceBound {
			return fmt.Errorf("pair reduction %s bound %v is not the reference width bound %v", a.ActionID, req.PriceBound, want)
		}
	}
	if !pairBoundCrossesPx(req.OrderSide, req.PriceBound, px) {
		return fmt.Errorf("pair reduction %s %s bound %v does not cross the reference level %s", a.ActionID, req.OrderSide, req.PriceBound, text)
	}
	return nil
}

func pairTransportRequestJSON(raw string) (string, error) {
	var req pairActionRequest
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		return "", fmt.Errorf("the saved pair request is unreadable: %v", err)
	}
	if req.Reference == nil {
		return raw, nil
	}
	req.Reference = nil
	out, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func pairReductionAgeRefusal(raw string, nowMs int64) string {
	var req pairActionRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return "the saved request is unreadable: " + err.Error()
	}
	if !req.ReduceOnly || req.NativeBooked {
		return ""
	}
	if req.ReferenceReceiptMs <= 0 || req.ReferenceMaxAgeMs <= 0 || req.ReferencePx == "" {
		return "the saved reduction request carries no reference authority; an older request stays queryable but never authorizes a send"
	}
	if !pairAgeWithin(nowMs, req.ReferenceReceiptMs, req.ReferenceMaxAgeMs) {
		return fmt.Sprintf("the saved reference received at %d is outside reference_max_age_ms %d of the transport time %d", req.ReferenceReceiptMs, req.ReferenceMaxAgeMs, nowMs)
	}
	px, ok := new(big.Rat).SetString(req.ReferencePx)
	if !ok || px.Sign() <= 0 {
		return fmt.Sprintf("the saved reference level %q is not a positive decimal", req.ReferencePx)
	}
	if !pairBoundCrossesPx(req.OrderSide, req.PriceBound, px) {
		return fmt.Sprintf("the saved %s bound %v does not cross the saved reference level %s", req.OrderSide, req.PriceBound, req.ReferencePx)
	}
	return ""
}

func (bc *basketCoordinator) holdPairReduction(rt *basketRuntime, c pairCandidate, reason string, now time.Time) {
	p := &rt.Pair.Progress
	nowMs := now.UnixMilli()
	retryAt, ok := pairAddMs(nowMs, rt.Pair.Limits.RetryInitialMs)
	if !ok {
		retryAt = math.MaxInt64
	}
	if c.Purpose == pairPurposeProtective {
		r := p.Retries[c.MemberID]
		backoff := rt.Pair.Limits.RetryInitialMs
		for i := 0; i < r.Count && backoff < rt.Pair.Limits.RetryMaxMs; i++ {
			backoff *= 2
		}
		if backoff > rt.Pair.Limits.RetryMaxMs {
			backoff = rt.Pair.Limits.RetryMaxMs
		}
		at, ok := pairAddMs(nowMs, backoff)
		if !ok {
			at = math.MaxInt64
		}
		if p.Retries == nil {
			p.Retries = map[string]pairRetry{}
		}
		p.Retries[c.MemberID] = pairRetry{Count: r.Count + 1, RetryAtMs: at}
		retryAt = at
	}
	retry := "; the check repeats at " + formatClosedBarMs(retryAt)
	detail := fmt.Sprintf("the %s reduction of %.10g base holds: %s; the pair never sends an unbounded or market reduction, native protection stays active and nothing is cancelled%s", c.Purpose, c.FullQty, reason, retry)
	episode := nowMs
	if o := p.obligation(pairObligationReductionReference, c.MemberID, ""); o != nil {
		episode = o.AtMs
		o.Detail, o.RetryAtMs = detail, retryAt
	} else {
		p.Obligations = append(p.Obligations, pairObligation{Kind: pairObligationReductionReference, MemberID: c.MemberID, Detail: detail, AtMs: nowMs, RetryAtMs: retryAt})
	}
	if c.Purpose == pairPurposeProtective && p.RecoveryStartMs > 0 {
		episode = p.RecoveryStartMs
	}
	rt.touchPairProgress()
	bc.persistPair(rt)
	bc.pairLogf(rt, "%s: %s", c.MemberID, detail)
	bc.pairAlert(rt, fmt.Sprintf("reduction_reference|%s|%d", c.MemberID, episode), fmt.Sprintf("%s %s: %s", rt.Contract.BasketID, c.MemberID, detail), true)
}

func (bc *basketCoordinator) clearPairReductionHold(rt *basketRuntime, memberID string) bool {
	p := &rt.Pair.Progress
	a := p.clearObligation(pairObligationReductionReference, memberID, "")
	b := p.clearObligation(pairObligationProtectiveBound, memberID, "")
	if a || b {
		rt.touchPairProgress()
	}
	return a || b
}

func (bc *basketCoordinator) pairHLPeerBooks(rt *basketRuntime, m pairContractMember, posSide string) (float64, float64) {
	var peers []StrategyConfig
	for _, sc := range bc.cfg.Strategies {
		if sc.ID == m.MemberID || sc.Platform != "hyperliquid" {
			continue
		}
		if isPairMember(sc) {
			if prt := bc.byMember[sc.ID]; prt != nil && prt.isPair() {
				if pm, ok := prt.Contract.pairMember(sc.ID); ok && pm.Provider == entryProviderHyperliquid && pm.AccountKey == m.AccountKey {
					peers = append(peers, sc)
				}
			}
			continue
		}
		if isHLLiveReconcilable(sc) && hyperliquidEntryAccountKey() == m.AccountKey {
			peers = append(peers, sc)
		}
	}
	return hlPeerBooksOnCoin(bc.state.Strategies, peers, m.Instrument, m.MemberID, posSide)
}

func (bc *basketCoordinator) pairSizeHLReduction(rt *basketRuntime, c pairCandidate, qty float64, v pairVenue, in *basketCycleInputs) (float64, func(), string) {
	m := c.Member
	var unlock func()
	bc.unlocked(func() { unlock = lockHyperliquidProtectionSync(m.Symbol) })
	fail := func(why string) (float64, func(), string) {
		unlock()
		return 0, nil, why
	}
	*in = bc.refreshPairInputs(rt, *in)
	book := bc.pairExposure(rt, m.MemberID)
	if book == 0 || !basketQtyEqual(book, c.Current) {
		return fail(fmt.Sprintf("the member exposure moved from %.10g to %.10g before the venue position refetch", c.Current, book))
	}
	posSide := entrySideLong
	if book < 0 {
		posSide = entrySideShort
	}
	same, opp := bc.pairHLPeerBooks(rt, m, posSide)
	before := bc.pairSnapshot(rt)
	fp := rt.Contract.Fingerprint()
	gen := rt.pairView[m.MemberID].Generation
	startMs := in.Now.UnixMilli()
	parent := bc.parent
	if parent == nil {
		parent = context.Background()
	}
	limit := time.Duration(rt.Pair.Limits.CollectionTimeoutMs) * time.Millisecond
	var pos pairVenuePosition
	var perr error
	bc.unlocked(func() {
		ctx, cancel := context.WithTimeout(parent, limit)
		defer cancel()
		pos, perr = v.Position(ctx, m, startMs)
	})
	*in = bc.refreshPairInputs(rt, *in)
	if !before.same(bc.pairSnapshot(rt)) || rt.Contract.Fingerprint() != fp || rt.pairView[m.MemberID].Generation != gen {
		return fail("the pair records, contract or reference publication changed while the venue position was refetched")
	}
	fresh := hlOnChainCoinView{}
	if perr == nil {
		if err := pairCheckVenuePosition(rt, m, pos, in.Now.UnixMilli()); err != nil {
			perr = err
		} else if base, err := pairPositionBase(pos); err != nil {
			perr = err
		} else {
			b := pairRatFloat(base)
			fresh = hlOnChainCoinView{Known: true, AbsQty: map[string]float64{m.Instrument: math.Abs(b)}, NetSide: map[string]string{}}
			switch {
			case b > 0:
				fresh.NetSide[m.Instrument] = entrySideLong
			case b < 0:
				fresh.NetSide[m.Instrument] = entrySideShort
			}
		}
	}
	plan := resolveHLCloseOrder(m.Instrument, posSide, math.Abs(book), qty, hlCloseContext{PeerSameQty: same, PeerOppQty: opp,
		Refetch: func() (hlOnChainCoinView, error) { return fresh, perr }})
	switch {
	case plan.Action != hlCloseSend:
		return fail(fmt.Sprintf("the Hyperliquid close size owner answered %s: %s", plan.Action, plan.Reason))
	case plan.Mode != hlCloseModeReduceOnly:
		return fail("the Hyperliquid close size owner would send a netted order that crosses zero (" + plan.Reason + "); a pair coordinator reduction is reduce-only and never crosses")
	}
	size := plan.Size
	if plan.Capped {
		size = pairFloorLots(size, pairLotBase(m))
		if !(size > 0) {
			return fail(fmt.Sprintf("the capped Hyperliquid size %.10g has no legal lot (%s)", plan.Size, plan.Reason))
		}
		if c.Plan != nil {
			capped := c
			capped.FullQty = size
			q, _, why := c.Plan.legalTranche(capped)
			if why != "" {
				return fail("the capped Hyperliquid size has no legal exit tranche: " + why)
			}
			size = q
		}
		if !pairReductionMinimum(m, size, c.PriceBound) {
			return fail(fmt.Sprintf("the capped Hyperliquid size %.10g is below the venue minimum", size))
		}
		bc.pairLogf(rt, "%s %s capped from %.10g to %.10g base by the Hyperliquid close size owner: %s", c.MemberID, c.Purpose, qty, size, plan.Reason)
	}
	return size, unlock, ""
}
