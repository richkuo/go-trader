package main

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const pairTargetsLegacyVersion = 1

const pairObligationReductionMinimum = "reduction_below_minimum"

const pairObligationProtectiveBound = "protective_bound_unavailable"

type pairBoundEvidence struct {
	ProposedBound  float64 `json:"proposed_bound"`
	RoundedBound   float64 `json:"rounded_bound"`
	Side           string  `json:"side"`
	CapabilityHash string  `json:"capability_hash"`
}

func (t *pairTargets) legacy() bool {
	return t != nil && t.Version == pairTargetsLegacyVersion
}

func (t *pairTargets) actionable() bool {
	return t != nil && (t.Kind == pairPolicyStatusEnter || t.Kind == pairPolicyStatusExit)
}

func pairSafeSide(side string, rounded, proposed float64) bool {
	switch side {
	case "buy":
		return rounded <= proposed
	case "sell":
		return rounded >= proposed
	}
	return false
}

func pairMinBound(proposed, rounded float64) float64 {
	if proposed > 0 && proposed < rounded {
		return proposed
	}
	return rounded
}

func pairMaxBound(proposed, rounded float64) float64 {
	if proposed > rounded {
		return proposed
	}
	return rounded
}

func pairRoundBoundRule(instrument string, meta pairWireMeta, side string, bound float64) (float64, error) {
	if side != "buy" && side != "sell" {
		return 0, fmt.Errorf("bound side %q is neither buy nor sell", side)
	}
	if math.IsNaN(bound) || math.IsInf(bound, 0) || bound <= 0 {
		return 0, fmt.Errorf("bound %v must be finite and above zero", bound)
	}
	x, err := quoteRatFromFloatDecimal(bound)
	if err != nil {
		return 0, err
	}
	round := func(px *big.Rat) (*big.Rat, error) {
		switch meta.PriceRule {
		case pairPriceRuleHyperliquid:
			rule := newQuotePriceRule(hlLotLookup{Known: true, SzDecimals: meta.SzDecimals, Coin: instrument})
			return rule.roundQuotePx(side, px)
		case pairPriceRuleFixedTick:
			tick, err := pairParseDecimalInput(meta.TickStepText)
			if err != nil || tick.Sign() <= 0 {
				return nil, fmt.Errorf("tick_step_text %q is not a positive decimal", meta.TickStepText)
			}
			q := new(big.Rat).Quo(px, tick)
			n := new(big.Int).Quo(q.Num(), q.Denom())
			if side == "sell" && new(big.Rat).SetInt(n).Cmp(q) != 0 {
				n.Add(n, big.NewInt(1))
			}
			return new(big.Rat).Mul(new(big.Rat).SetInt(n), tick), nil
		}
		return nil, fmt.Errorf("price rule %q is not supported", meta.PriceRule)
	}
	r, err := round(x)
	if err != nil {
		return 0, err
	}
	if r.Sign() <= 0 {
		return 0, fmt.Errorf("bound %v rounds to zero", bound)
	}
	f := quoteRatFloat(r)
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0, fmt.Errorf("rounded bound %s has no finite positive float", r.RatString())
	}
	back, err := quoteRatFromFloatDecimal(f)
	if err != nil || back.Cmp(r) != 0 {
		return 0, fmt.Errorf("rounded bound %s does not survive float projection exactly", r.RatString())
	}
	again, err := round(back)
	if err != nil || again.Cmp(back) != 0 {
		return 0, fmt.Errorf("projected bound %v is not a legal venue price", f)
	}
	if (side == "buy" && back.Cmp(x) > 0) || (side == "sell" && back.Cmp(x) < 0) {
		return 0, fmt.Errorf("projected bound %v would widen the %s bound %v", f, side, bound)
	}
	return f, nil
}

func pairBoundOnGrid(m pairContractMember, side string, bound float64) error {
	meta, err := pairExpectedWireMeta(m)
	if err != nil {
		return fmt.Errorf("member %s contract meta: %w", m.MemberID, err)
	}
	got, err := pairRoundBoundRule(m.Instrument, meta, side, bound)
	if err != nil {
		return err
	}
	if got != bound {
		return fmt.Errorf("the %s bound %v is not a legal %s price (it rounds to %v)", side, bound, m.Instrument, got)
	}
	return nil
}

func pairExactDecimalFloat(r *big.Rat, what string) (float64, string, error) {
	text, err := pairRatDecimal(r)
	if err != nil {
		return 0, "", fmt.Errorf("%s %s is not a finite decimal: %w", what, r.RatString(), err)
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, "", fmt.Errorf("%s %s has no finite float", what, text)
	}
	back, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'f', -1, 64))
	if !ok || back.Cmp(r) != 0 {
		return 0, "", fmt.Errorf("%s %s does not survive its float projection exactly", what, text)
	}
	return f, text, nil
}

type pairOrderQty struct {
	Native     float64
	Base       float64
	NativeText string
	BaseText   string
	Lots       int64
}

func pairExactOrderQty(m pairContractMember, baseQty float64) (pairOrderQty, error) {
	meta, err := pairExpectedWireMeta(m)
	if err != nil {
		return pairOrderQty{}, fmt.Errorf("member %s contract meta: %w", m.MemberID, err)
	}
	lot, err := meta.LotStep.rat()
	if err != nil || lot.Sign() <= 0 {
		return pairOrderQty{}, fmt.Errorf("member %s lot step %s is not positive", m.MemberID, meta.LotStep)
	}
	if meta.LotStepText != "" {
		if lot, err = pairParseDecimalInput(meta.LotStepText); err != nil || lot.Sign() <= 0 {
			return pairOrderQty{}, fmt.Errorf("member %s lot_step_text %q is not positive", m.MemberID, meta.LotStepText)
		}
	}
	cs, err := meta.ContractSize.rat()
	if err != nil || cs.Sign() <= 0 {
		return pairOrderQty{}, fmt.Errorf("member %s contract size %s is not positive", m.MemberID, meta.ContractSize)
	}
	if !(baseQty > 0) || math.IsInf(baseQty, 0) {
		return pairOrderQty{}, fmt.Errorf("order quantity %v is not finite and positive", baseQty)
	}
	lotBase := quoteRatFloat(new(big.Rat).Mul(lot, cs))
	n := math.Round(baseQty / lotBase)
	if n < 1 || n > 1e15 || math.Abs(n*lotBase-baseQty) > 1e-9*math.Max(1, baseQty) {
		return pairOrderQty{}, fmt.Errorf("order quantity %v is not a whole number of %s lots of %v base", baseQty, m.Instrument, lotBase)
	}
	native := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(n)), lot)
	base := new(big.Rat).Mul(native, cs)
	nf, nt, err := pairExactDecimalFloat(native, "native quantity")
	if err != nil {
		return pairOrderQty{}, err
	}
	bf, bt, err := pairExactDecimalFloat(base, "base quantity")
	if err != nil {
		return pairOrderQty{}, err
	}
	return pairOrderQty{Native: nf, Base: bf, NativeText: nt, BaseText: bt, Lots: int64(n)}, nil
}

func pairCheckExactRequestQty(m pairContractMember, req pairActionRequest) error {
	q, err := pairExactOrderQty(m, req.BaseQty)
	if err != nil {
		return err
	}
	if req.NativeQty != q.Native || req.BaseQty != q.Base {
		return fmt.Errorf("the request quantities (native %v, base %v) are not the exact %s grid values (native %s, base %s)", req.NativeQty, req.BaseQty, m.Instrument, q.NativeText, q.BaseText)
	}
	if req.ContractSize != m.Meta.ContractSize {
		return fmt.Errorf("the request contract size %v is not the contract's %v", req.ContractSize, m.Meta.ContractSize)
	}
	return nil
}

func pairCheckSavedTargets(rt *basketRuntime, t *pairTargets) error {
	if t == nil || !t.actionable() {
		return nil
	}
	if t.legacy() {
		return fmt.Errorf("the version-1 decision record carries no saved bound evidence, so it cannot authorize an order")
	}
	members := rt.Contract.pairMembersSorted()
	if len(t.BoundEvidence) != len(members) || len(t.Members) != len(members) {
		return fmt.Errorf("the decision record has %d bound evidence records and %d targets, want %d each", len(t.BoundEvidence), len(t.Members), len(members))
	}
	for _, m := range members {
		ev, ok := t.BoundEvidence[m.MemberID]
		if !ok {
			return fmt.Errorf("the decision record has no bound evidence for member %s", m.MemberID)
		}
		tg, ok := t.Members[m.MemberID]
		if !ok {
			return fmt.Errorf("the decision record has no target for member %s", m.MemberID)
		}
		if ev.Side != "buy" && ev.Side != "sell" {
			return fmt.Errorf("member %s bound evidence side %q is neither buy nor sell", m.MemberID, ev.Side)
		}
		if !pairHexDigestPattern.MatchString(ev.CapabilityHash) {
			return fmt.Errorf("member %s bound evidence names no capability hash", m.MemberID)
		}
		if !basketFinitePositive(ev.ProposedBound) || !basketFinitePositive(ev.RoundedBound) {
			return fmt.Errorf("member %s bound evidence (%v, %v) is not finite and positive", m.MemberID, ev.ProposedBound, ev.RoundedBound)
		}
		if !pairSafeSide(ev.Side, ev.RoundedBound, ev.ProposedBound) {
			return fmt.Errorf("member %s rounded %s bound %v is beyond the proposed bound %v", m.MemberID, ev.Side, ev.RoundedBound, ev.ProposedBound)
		}
		if tg.PriceBound != ev.RoundedBound {
			return fmt.Errorf("member %s saved bound %v is not its rounded bound %v", m.MemberID, tg.PriceBound, ev.RoundedBound)
		}
		if t.Kind == pairPolicyStatusEnter && (tg.TargetBaseQty > 0) != (ev.Side == "buy") {
			return fmt.Errorf("member %s target %v disagrees with its %s bound evidence", m.MemberID, tg.TargetBaseQty, ev.Side)
		}
		if err := pairBoundOnGrid(m, ev.Side, ev.RoundedBound); err != nil {
			return fmt.Errorf("member %s: %w", m.MemberID, err)
		}
	}
	return nil
}

func (bc *basketCoordinator) pairDecisionSides(rt *basketRuntime, res *pairPolicyResult) (map[string]string, error) {
	out := map[string]string{}
	for _, m := range rt.Contract.pairMembersSorted() {
		t := res.Targets[m.MemberID]
		cur := bc.pairExposure(rt, m.MemberID)
		change := t.TargetBaseQty - cur
		switch {
		case res.Status == pairPolicyStatusExit && cur == 0:
			return nil, fmt.Errorf("member %s has no exposure for the exit to close", m.MemberID)
		case change > 0:
			out[m.MemberID] = "buy"
		case change < 0:
			out[m.MemberID] = "sell"
		default:
			return nil, fmt.Errorf("member %s target %v needs no order from its exposure %v", m.MemberID, t.TargetBaseQty, cur)
		}
	}
	return out, nil
}

func (bc *basketCoordinator) pairRoundTargets(rt *basketRuntime, env pairQuoteEnvelope, res *pairPolicyResult) (map[string]pairPolicyMemberTarget, map[string]pairBoundEvidence, error) {
	sides, err := bc.pairDecisionSides(rt, res)
	if err != nil {
		return nil, nil, err
	}
	targets := map[string]pairPolicyMemberTarget{}
	evidence := map[string]pairBoundEvidence{}
	for _, m := range rt.Contract.pairMembersSorted() {
		t := res.Targets[m.MemberID]
		em, ok := env.member(m.MemberID)
		if !ok {
			return nil, nil, fmt.Errorf("the verified envelope has no capability for member %s", m.MemberID)
		}
		side := sides[m.MemberID]
		rounded, err := pairRoundBound(m, em.Capability, side, t.PriceBound)
		if err != nil {
			return nil, nil, fmt.Errorf("member %s %s bound %v cannot be rounded to the venue grid: %w", m.MemberID, side, t.PriceBound, err)
		}
		if !pairSafeSide(side, rounded, t.PriceBound) {
			return nil, nil, fmt.Errorf("member %s rounded %s bound %v widens the proposed bound %v", m.MemberID, side, rounded, t.PriceBound)
		}
		targets[m.MemberID] = pairPolicyMemberTarget{TargetBaseQty: t.TargetBaseQty, PriceBound: rounded}
		evidence[m.MemberID] = pairBoundEvidence{ProposedBound: t.PriceBound, RoundedBound: rounded, Side: side, CapabilityHash: em.Capability.EvidenceHash}
	}
	return targets, evidence, nil
}

func (bc *basketCoordinator) pairRoundedEconomics(rt *basketRuntime, env pairQuoteEnvelope, kind string, targets map[string]pairPolicyMemberTarget, evidence map[string]pairBoundEvidence) string {
	var bad []string
	conservative := map[string]pairPolicyMemberTarget{}
	for _, m := range rt.Contract.pairMembersSorted() {
		t, ev := targets[m.MemberID], evidence[m.MemberID]
		minPx := pairMinBound(ev.ProposedBound, ev.RoundedBound)
		qty := math.Abs(t.TargetBaseQty)
		if kind == pairPolicyStatusExit {
			qty = math.Abs(bc.pairExposure(rt, m.MemberID))
		}
		if !pairLegalLot(m, qty, minPx) {
			what := "target"
			if kind == pairPolicyStatusExit {
				what = "close tranche"
			}
			bad = append(bad, fmt.Sprintf("member %s %s %v is not a legal lot quantity after rounding (lot %v base, minimum %v native, minimum notional %v at %v)", m.MemberID, what, qty, pairLotBase(m), m.Meta.MinQty, m.Meta.MinNotional, minPx))
		}
		conservative[m.MemberID] = pairPolicyMemberTarget{TargetBaseQty: t.TargetBaseQty, PriceBound: pairMaxBound(ev.ProposedBound, ev.RoundedBound)}
	}
	if kind == pairPolicyStatusEnter && len(bad) == 0 {
		if why := bc.pairFullFillCapCheck(rt, env, conservative); why != "" {
			bad = append(bad, "the full fill of the frozen targets at max(proposed, rounded) bounds would break a cap: "+why)
		}
	}
	sort.Strings(bad)
	return strings.Join(bad, "; ")
}

func pairTargetEvidence(rt *basketRuntime, slotMs int64, memberID string) (pairBoundEvidence, bool) {
	rb := rt.Rebalances[slotMs]
	if rb == nil {
		return pairBoundEvidence{}, false
	}
	t, err := parsePairTargets(rb.TargetsJSON)
	if err != nil || t == nil || t.legacy() {
		return pairBoundEvidence{}, false
	}
	ev, ok := t.BoundEvidence[memberID]
	return ev, ok
}

func (bc *basketCoordinator) pairRiskBound(rt *basketRuntime, a *basketAction) float64 {
	b := a.PriceBound
	if ev, ok := pairTargetEvidence(rt, a.DeadlineMs, a.StrategyID); ok && ev.Side == a.OrderSide && ev.RoundedBound == a.PriceBound {
		b = pairMaxBound(ev.ProposedBound, b)
	}
	return b
}

func pairDepthWithin(env pairQuoteEnvelope, memberID, side string, bound float64) (*big.Rat, error) {
	em, ok := env.member(memberID)
	if !ok {
		return nil, fmt.Errorf("the envelope has no member %s", memberID)
	}
	b, err := quoteRatFromFloatDecimal(bound)
	if err != nil {
		return nil, err
	}
	levels := em.Observation.Asks
	if side == "sell" {
		levels = em.Observation.Bids
	} else if side != "buy" {
		return nil, fmt.Errorf("depth side %q is neither buy nor sell", side)
	}
	total := new(big.Rat)
	for _, l := range levels {
		px, ok := new(big.Rat).SetString(l.Px)
		if !ok {
			return nil, fmt.Errorf("member %s saved level price %q is not a decimal", memberID, l.Px)
		}
		qty, ok := new(big.Rat).SetString(l.BaseQty)
		if !ok || qty.Sign() <= 0 {
			return nil, fmt.Errorf("member %s saved level quantity %q is not a positive decimal", memberID, l.BaseQty)
		}
		if (side == "buy" && px.Cmp(b) > 0) || (side == "sell" && px.Cmp(b) < 0) {
			break
		}
		total.Add(total, qty)
	}
	return total, nil
}

func (bc *basketCoordinator) pairDispatchBookCheck(rt *basketRuntime, m pairContractMember, ev pairQuoteEvent, side string, bound, baseQty float64) string {
	rb := rt.Rebalances[ev.SlotMs]
	if rb == nil {
		return fmt.Sprintf("the selected quote event %s has no saved slot", ev.ID())
	}
	env, why := verifyPairEnvelope(rt, rb)
	if why != "" {
		return "the selected quote event's envelope fails verification: " + why
	}
	em, ok := env.member(m.MemberID)
	if !ok {
		return fmt.Sprintf("the selected envelope has no member %s", m.MemberID)
	}
	again, err := pairRoundBound(m, em.Capability, side, bound)
	if err != nil {
		return fmt.Sprintf("the saved %s bound %v is not checkable under the selected event's capability: %v", side, bound, err)
	}
	if again != bound {
		return fmt.Sprintf("the saved %s bound %v is not a legal price under the selected event's capability (it rounds to %v)", side, bound, again)
	}
	if !pairBoundCrosses(env, m.MemberID, side, bound) {
		return fmt.Sprintf("the saved %s bound %v no longer crosses the selected best level", side, bound)
	}
	depth, err := pairDepthWithin(env, m.MemberID, side, bound)
	if err != nil {
		return err.Error()
	}
	want, err := quoteRatFromFloatDecimal(baseQty)
	if err != nil {
		return err.Error()
	}
	if depth.Cmp(want) < 0 {
		return fmt.Sprintf("the selected saved depth at or inside the %s bound %v holds %s base, below the %v base tranche", side, bound, depth.FloatString(12), baseQty)
	}
	return ""
}

func pairLiveShapedRuntime(rt *basketRuntime) bool {
	return rt != nil && rt.Pair != nil && rt.Pair.admission != nil && rt.Pair.admission.kind == pairAdmissionLiveShapedFixture
}

func pairCheckLiveRequest(rt *basketRuntime, m pairContractMember, a *basketAction, req pairActionRequest) error {
	if !pairLiveShapedRuntime(rt) {
		return nil
	}
	if !basketFinitePositive(req.PriceBound) || req.PriceBound != a.PriceBound {
		return fmt.Errorf("pair action %s live-shaped request needs a finite positive price bound equal to the action's, got %v", a.ActionID, req.PriceBound)
	}
	if err := pairBoundOnGrid(m, req.OrderSide, req.PriceBound); err != nil {
		return fmt.Errorf("pair action %s live-shaped request bound: %w", a.ActionID, err)
	}
	if err := pairCheckExactRequestQty(m, req); err != nil {
		return fmt.Errorf("pair action %s live-shaped request quantity: %w", a.ActionID, err)
	}
	if req.BaseQty != a.RequestedQty || req.NativeQty != a.NativeQty {
		return fmt.Errorf("pair action %s request quantities differ from the reserved action", a.ActionID)
	}
	ev, ok := pairTargetEvidence(rt, req.OriginSlotMs, a.StrategyID)
	needEvidence := req.Purpose == pairPurposeEntry
	if req.Purpose == pairPurposeExit {
		if rb := rt.Rebalances[req.OriginSlotMs]; rb != nil {
			if t, err := parsePairTargets(rb.TargetsJSON); err == nil && t != nil && t.Kind == pairPolicyStatusExit {
				needEvidence = true
			}
		}
	}
	if needEvidence && (!ok || ev.RoundedBound != req.PriceBound || ev.Side != req.OrderSide) {
		return fmt.Errorf("pair action %s request bound %v %s is not the saved rounded bound evidence of slot %d", a.ActionID, req.PriceBound, req.OrderSide, req.OriginSlotMs)
	}
	return nil
}

var (
	pairHLAccountKeyPattern  = regexp.MustCompile(`^hyperliquid:(mainnet|testnet):0x[0-9a-f]{40}$`)
	pairOKXAccountKeyPattern = regexp.MustCompile(`^okx:swap:(live|sandbox):key-[0-9a-f]{16}$`)
)

func pairAccountKeyError(provider, key string, liveShapedFixture bool) string {
	if strings.HasPrefix(key, pairSyntheticAccountPrefix) && len(key) > len(pairSyntheticAccountPrefix) {
		return ""
	}
	if liveShapedFixture {
		switch {
		case provider == entryProviderHyperliquid && pairHLAccountKeyPattern.MatchString(key):
			return ""
		case provider == entryProviderOKX && pairOKXAccountKeyPattern.MatchString(key):
			return ""
		}
		return fmt.Sprintf("account_key %q must name a synthetic research account (%s<name>) or, in the controlled live-shaped fixture only, the %s account format", key, pairSyntheticAccountPrefix, pairRealAccountFormat(provider))
	}
	return fmt.Sprintf("account_key %q must name an explicit synthetic research account (%s<name>)", key, pairSyntheticAccountPrefix)
}

func pairRealAccountFormat(provider string) string {
	if provider == entryProviderOKX {
		return "okx:swap:<network>:key-<16 hex>"
	}
	return "hyperliquid:<network>:<address>"
}
