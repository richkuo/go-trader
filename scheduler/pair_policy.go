package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const pairPolicyRequestVersion = 2
const pairTargetsVersion = 2
const pairPolicyMaxTimeout = 30 * time.Second

const (
	pairPolicyStatusEnter   = "enter"
	pairPolicyStatusExit    = "exit"
	pairPolicyStatusHold    = "hold"
	pairPolicyStatusInvalid = "invalid"

	pairOrientationLongHLShortOKX = "long_hyperliquid_short_okx"
	pairOrientationLongOKXShortHL = "long_okx_short_hyperliquid"
)

type pairPolicyRunner func(ctx context.Context, stdin []byte, timeout time.Duration) (stdout, stderr []byte, err error)

var pairPolicyRunFn pairPolicyRunner = runPairPolicyProcess

var pairPolicyScript = "shared_scripts/run_pair_policy.py"

var pairPolicyProbeArgv = []string{"--probe-only"}

const (
	pairPolicyMaxStdoutBytes   = 256 << 10
	pairPolicyMaxStderrBytes   = 64 << 10
	pairPolicyMaxDecisionBytes = 64 << 10
	pairPolicyMaxReasonBytes   = 2048
	pairTargetsMaxBytes        = 96 << 10
)

func runPairPolicyProcess(ctx context.Context, stdin []byte, timeout time.Duration) ([]byte, []byte, error) {
	if timeout <= 0 {
		return nil, nil, fmt.Errorf("the policy run has no time left")
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	release, err := acquirePythonSlot(runCtx)
	if err != nil {
		return nil, nil, fmt.Errorf("waiting for a script slot: %w", err)
	}
	defer release()
	env := map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}
	stdout, stderr, runErr := spawnPythonProcessWithEnvLimited(runCtx, pairPolicyScript, nil, stdin, timeout, env, pythonOutputLimits{Stdout: pairPolicyMaxStdoutBytes, Stderr: pairPolicyMaxStderrBytes})
	if runErr == nil && runCtx.Err() != nil {
		runErr = runCtx.Err()
	}
	return stdout, stderr, runErr
}

var pairPolicyResultFields = map[string]bool{"v": true, "pair_id": true, "slot_ms": true, "contract_fingerprint": true, "input_fingerprint": true,
	"status": true, "orientation": true, "targets": true, "hold_reason": true, "error": true, "decision": true}

func pairResultInteger(v interface{}) bool {
	n, ok := v.(pairJSONNumber)
	if !ok || !pairIntegerPattern.MatchString(string(n)) {
		return false
	}
	_, err := strconv.ParseInt(string(n), 10, 64)
	return err == nil
}

func pairResultFinite(v interface{}) bool {
	n, ok := v.(pairJSONNumber)
	if !ok {
		return false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

func pairResultEmptyString(obj map[string]interface{}, key string) bool {
	v, ok := obj[key]
	if !ok {
		return true
	}
	s, isStr := v.(string)
	return isStr && s == ""
}

func pairResultSchemaError(obj map[string]interface{}) error {
	var unknown []string
	for k := range obj {
		if !pairPolicyResultFields[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown result fields %s", strings.Join(unknown, ", "))
	}
	for _, k := range []string{"v", "slot_ms"} {
		if !pairResultInteger(obj[k]) {
			return fmt.Errorf("result field %s must be a JSON integer", k)
		}
	}
	for _, k := range []string{"pair_id", "contract_fingerprint", "input_fingerprint", "status"} {
		if _, ok := obj[k].(string); !ok {
			return fmt.Errorf("result field %s must be a JSON string", k)
		}
	}
	for _, k := range []string{"orientation", "hold_reason", "error"} {
		if v, ok := obj[k]; ok {
			s, isStr := v.(string)
			if !isStr {
				return fmt.Errorf("result field %s must be a JSON string", k)
			}
			if len(s) > pairPolicyMaxReasonBytes {
				return fmt.Errorf("result field %s is %d bytes, above the %d-byte limit", k, len(s), pairPolicyMaxReasonBytes)
			}
		}
	}
	if _, ok := obj["decision"].(map[string]interface{}); !ok {
		return fmt.Errorf("result field decision must be a JSON object")
	}
	status := obj["status"].(string)
	targets, hasTargets := obj["targets"]
	switch status {
	case pairPolicyStatusEnter, pairPolicyStatusExit:
		if status == pairPolicyStatusEnter {
			if s, _ := obj["orientation"].(string); s == "" {
				return fmt.Errorf("an enter result needs an orientation")
			}
		} else if !pairResultEmptyString(obj, "orientation") {
			return fmt.Errorf("an exit result carries no orientation")
		}
		if !pairResultEmptyString(obj, "hold_reason") || !pairResultEmptyString(obj, "error") {
			return fmt.Errorf("a %s result carries no hold_reason or error", status)
		}
		m, ok := targets.(map[string]interface{})
		if !ok {
			return fmt.Errorf("a %s result needs a targets object", status)
		}
		ids := make([]string, 0, len(m))
		for id := range m {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			t, ok := m[id].(map[string]interface{})
			if !ok || len(t) != 2 || !pairResultFinite(t["target_base_qty"]) || !pairResultFinite(t["price_bound"]) {
				return fmt.Errorf("target %s must be exactly {target_base_qty, price_bound} with finite numbers", id)
			}
		}
	case pairPolicyStatusHold, pairPolicyStatusInvalid:
		if hasTargets && targets != nil {
			return fmt.Errorf("a %s result carries no targets", status)
		}
		if !pairResultEmptyString(obj, "orientation") {
			return fmt.Errorf("a %s result carries no orientation", status)
		}
		if status == pairPolicyStatusHold && !pairResultEmptyString(obj, "error") {
			return fmt.Errorf("a hold result carries no error")
		}
		if status == pairPolicyStatusInvalid && !pairResultEmptyString(obj, "hold_reason") {
			return fmt.Errorf("an invalid result carries no hold_reason")
		}
	default:
		return fmt.Errorf("unknown status %q", status)
	}
	return nil
}

func pairDecodePolicyResult(stdout []byte) (*pairPolicyResult, error) {
	tree, err := pairStrictParse(stdout)
	if err != nil {
		return nil, err
	}
	obj, ok := tree.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("the result is not a JSON object")
	}
	if err := pairResultSchemaError(obj); err != nil {
		return nil, err
	}
	var res pairPolicyResult
	dec := json.NewDecoder(bytes.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return nil, err
	}
	if len(res.Decision) > pairPolicyMaxDecisionBytes {
		return nil, fmt.Errorf("the decision is %d bytes, above the %d-byte limit", len(res.Decision), pairPolicyMaxDecisionBytes)
	}
	return &res, nil
}

func pairConversionRates(env pairQuoteEnvelope, memberID string) (map[string]float64, error) {
	em, ok := env.member(memberID)
	if !ok {
		return nil, fmt.Errorf("the verified envelope has no record for member %s", memberID)
	}
	out := map[string]float64{pairUSD: 1}
	for _, c := range em.Conversions {
		r, err := c.RateUSD.rat()
		if err != nil {
			return nil, fmt.Errorf("member %s %s conversion: %w", memberID, c.Currency, err)
		}
		f, _ := r.Float64()
		if !basketFinitePositive(f) {
			return nil, fmt.Errorf("member %s %s conversion %s is not a finite positive rate", memberID, c.Currency, c.RateUSD)
		}
		out[c.Currency] = f
	}
	return out, nil
}

type pairMemberRequest struct {
	PairID     string `json:"pair_id"`
	MemberID   string `json:"member_id"`
	Provider   string `json:"provider"`
	Instrument string `json:"instrument"`
	Mode       string `json:"mode"`
}

func (r pairMemberRequest) JSON() string {
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}

func pairMemberRequestOf(sc StrategyConfig) (pairMemberRequest, bool) {
	if !isPairMember(sc) || sc.pairMember == nil {
		return pairMemberRequest{}, false
	}
	return *sc.pairMember, true
}

func appendPairMemberArg(args []string, sc StrategyConfig) []string {
	if !isPairMember(sc) {
		return args
	}
	req, ok := pairMemberRequestOf(sc)
	if !ok {
		req = pairMemberRequest{PairID: sc.PairID, MemberID: sc.ID, Provider: pairProviderPlatforms[sc.Platform], Mode: pairManagementOnlyMode}
	}
	return append(args, pairMemberFlag+"="+req.JSON())
}

func pairMemberCheckError(sc StrategyConfig, got *pairMemberRequest) string {
	want, ok := pairMemberRequestOf(sc)
	return pairMemberEchoError(sc, want, ok, got)
}

func pairMemberEchoError(sc StrategyConfig, want pairMemberRequest, wantOK bool, got *pairMemberRequest) string {
	if !wantOK {
		return fmt.Sprintf("strategy %s names pair %s, but the admitted pair contract does not list it; no management output is trusted", sc.ID, sc.PairID)
	}
	if got == nil {
		return "the pair member check did not echo the management-only contract; no entry evaluator output is trusted and pair entry holds"
	}
	if *got != want {
		return fmt.Sprintf("the pair member check echoed %s, but Go sent %s; pair entry holds", got.JSON(), want.JSON())
	}
	return ""
}

type pairPolicyMember struct {
	MemberID          string             `json:"member_id"`
	Provider          string             `json:"provider"`
	AccountKey        string             `json:"account_key"`
	Instrument        string             `json:"instrument"`
	Meta              pairInstrumentMeta `json:"meta"`
	BookedBaseQty     float64            `json:"booked_base_qty"`
	BookedAvgPx       float64            `json:"booked_avg_px"`
	CostPolicy        fillCostPolicy     `json:"cost_policy"`
	Source            string             `json:"source"`
	SourceContract    string             `json:"source_contract"`
	ModeledDeliveryMs int64              `json:"modeled_delivery_ms"`
	Leverage          float64            `json:"leverage"`
}

type pairPolicyRequest struct {
	V                   int                    `json:"v"`
	Partition           string                 `json:"partition"`
	PairID              string                 `json:"pair_id"`
	ContractFingerprint string                 `json:"contract_fingerprint"`
	SlotMs              int64                  `json:"slot_ms"`
	CutoffMs            int64                  `json:"cutoff_ms"`
	InputFingerprint    string                 `json:"input_fingerprint"`
	Policy              string                 `json:"policy"`
	Params              map[string]interface{} `json:"params"`
	Limits              pairOperationalLimits  `json:"limits"`
	BookNeed            pairBookNeed           `json:"book_need"`
	Lifecycle           string                 `json:"lifecycle"`
	Members             []pairPolicyMember     `json:"members"`
	Envelope            json.RawMessage        `json:"envelope"`
}

type pairPolicyMemberTarget struct {
	TargetBaseQty float64 `json:"target_base_qty"`
	PriceBound    float64 `json:"price_bound"`
}

type pairPolicyResult struct {
	V                   int                               `json:"v"`
	PairID              string                            `json:"pair_id"`
	SlotMs              int64                             `json:"slot_ms"`
	ContractFingerprint string                            `json:"contract_fingerprint"`
	InputFingerprint    string                            `json:"input_fingerprint"`
	Status              string                            `json:"status"`
	Orientation         string                            `json:"orientation"`
	Targets             map[string]pairPolicyMemberTarget `json:"targets"`
	HoldReason          string                            `json:"hold_reason"`
	Error               string                            `json:"error"`
	Decision            json.RawMessage                   `json:"decision"`
}

type pairTargets struct {
	Version           int                               `json:"version"`
	Kind              string                            `json:"kind"`
	Orientation       string                            `json:"orientation,omitempty"`
	SlotMs            int64                             `json:"slot_ms"`
	InputFingerprint  string                            `json:"input_fingerprint"`
	Members           map[string]pairPolicyMemberTarget `json:"members,omitempty"`
	PolicyReason      string                            `json:"policy_reason,omitempty"`
	CoordinatorReason string                            `json:"coordinator_reason,omitempty"`
	Decision          json.RawMessage                   `json:"decision"`
	FirstLeg          string                            `json:"first_leg,omitempty"`
	BoundEvidence     map[string]pairBoundEvidence      `json:"bound_evidence,omitempty"`
}

func (t pairTargets) JSON() string {
	b, err := json.Marshal(t)
	if err != nil {
		return ""
	}
	return string(b)
}

func parsePairTargets(raw string) (*pairTargets, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var t pairTargets
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, fmt.Errorf("pair decision record is unreadable: %w", err)
	}
	if t.Version != pairTargetsVersion && t.Version != pairTargetsLegacyVersion {
		return nil, fmt.Errorf("pair decision record version %d is not supported", t.Version)
	}
	if t.Version == pairTargetsLegacyVersion && len(t.BoundEvidence) > 0 {
		return nil, fmt.Errorf("a version-1 pair decision record carries bound evidence, which only version 2 defines")
	}
	return &t, nil
}

func pairAgeWithin(referenceMs, receiptMs, limitMs int64) bool {
	if receiptMs > referenceMs || limitMs < 0 {
		return false
	}
	return referenceMs-receiptMs <= limitMs
}

func pairSkewWithin(aMs, bMs, limitMs int64) bool {
	d := aMs - bMs
	if d < 0 {
		d = -d
	}
	return d >= 0 && d <= limitMs
}

func pairBeforeDeadline(nowMs, deadlineMs int64) bool {
	return nowMs < deadlineMs
}

func pairReached(nowMs, startMs, durationMs int64) bool {
	end, ok := pairAddMs(startMs, durationMs)
	if !ok {
		return false
	}
	return nowMs >= end
}

func pairAbsenceReleasable(nowMs, deadlineMs, coverageThroughMs int64) bool {
	limit, ok := pairAddMs(deadlineMs, entryRecoveryGrace.Milliseconds())
	if !ok {
		return false
	}
	return nowMs > limit && coverageThroughMs >= limit
}

func pairLotBase(m pairContractMember) float64 {
	return m.Meta.LotStep * m.Meta.ContractSize
}

func pairFloorLots(qty, lot float64) float64 {
	if !(qty > 0) || !(lot > 0) {
		return 0
	}
	n := math.Floor(qty/lot*(1+1e-12) + 1e-9)
	if n <= 0 {
		return 0
	}
	return math.Round(n*lot*1e12) / 1e12
}

func pairNativeQty(m pairContractMember, baseQty float64) float64 {
	if !(m.Meta.ContractSize > 0) {
		return 0
	}
	if q, err := pairExactOrderQty(m, baseQty); err == nil {
		return q.Native
	}
	n := baseQty / m.Meta.ContractSize
	lots := math.Round(n / m.Meta.LotStep)
	return lots * m.Meta.LotStep
}

func pairLegalLot(m pairContractMember, baseQty, px float64) bool {
	lot := pairLotBase(m)
	if !(baseQty > 0) || !(lot > 0) {
		return false
	}
	units := baseQty / lot
	if math.Abs(units-math.Round(units)) > 1e-6 {
		return false
	}
	native := baseQty / m.Meta.ContractSize
	if native+1e-12 < m.Meta.MinQty {
		return false
	}
	if px > 0 && baseQty*px+1e-9 < m.Meta.MinNotional {
		return false
	}
	return true
}

func validatePairPolicyResult(rt *basketRuntime, req pairPolicyRequest, res *pairPolicyResult) (holdReason string, err error) {
	if res == nil {
		return "", fmt.Errorf("the policy returned no result")
	}
	if res.V != pairPolicyRequestVersion {
		return "", fmt.Errorf("result version %d, want %d", res.V, pairPolicyRequestVersion)
	}
	if res.PairID != req.PairID || res.SlotMs != req.SlotMs || res.ContractFingerprint != req.ContractFingerprint || res.InputFingerprint != req.InputFingerprint {
		return "", fmt.Errorf("result identity (pair %s, slot %d, contract %s, input %s) does not match the request (pair %s, slot %d, contract %s, input %s)",
			res.PairID, res.SlotMs, shortFingerprint(res.ContractFingerprint), shortFingerprint(res.InputFingerprint),
			req.PairID, req.SlotMs, shortFingerprint(req.ContractFingerprint), shortFingerprint(req.InputFingerprint))
	}
	switch res.Status {
	case pairPolicyStatusHold:
		reason := strings.TrimSpace(res.HoldReason)
		if reason == "" {
			reason = "the pair policy held without a reason"
		}
		return reason, nil
	case pairPolicyStatusInvalid:
		why := strings.TrimSpace(res.Error)
		if why == "" {
			why = "the policy reported invalid input"
		}
		return "", fmt.Errorf("%s", why)
	case pairPolicyStatusEnter, pairPolicyStatusExit:
	default:
		return "", fmt.Errorf("unknown status %q", res.Status)
	}
	members := rt.Contract.pairMembersSorted()
	if len(res.Targets) != len(members) {
		return "", fmt.Errorf("the result names %d member targets, want %d", len(res.Targets), len(members))
	}
	for _, m := range members {
		t, ok := res.Targets[m.MemberID]
		if !ok {
			return "", fmt.Errorf("the result has no target for member %s", m.MemberID)
		}
		if !pairFinite(t.TargetBaseQty) || !basketFinitePositive(t.PriceBound) {
			return "", fmt.Errorf("member %s target %v at price bound %v is not finite with a positive bound", m.MemberID, t.TargetBaseQty, t.PriceBound)
		}
	}
	if res.Status == pairPolicyStatusExit {
		if req.Lifecycle != pairLifecycleMatched {
			return "", fmt.Errorf("an exit decision needs a matched pair, the lifecycle is %s", req.Lifecycle)
		}
		for _, m := range members {
			if res.Targets[m.MemberID].TargetBaseQty != 0 {
				return "", fmt.Errorf("an exit decision needs zero targets, member %s has %v", m.MemberID, res.Targets[m.MemberID].TargetBaseQty)
			}
		}
		return "", nil
	}
	if req.Lifecycle != pairLifecycleFlat {
		return "", fmt.Errorf("an entry decision needs a flat pair, the lifecycle is %s", req.Lifecycle)
	}
	var hl, okx float64
	for _, m := range members {
		t := res.Targets[m.MemberID]
		if t.TargetBaseQty == 0 {
			return "", fmt.Errorf("an entry decision needs a nonzero target for member %s", m.MemberID)
		}
		if !pairLegalLot(m, math.Abs(t.TargetBaseQty), 0) {
			return "", fmt.Errorf("member %s target %v is not a legal lot quantity after native conversion (lot %v base, minimum %v native, minimum notional %v)", m.MemberID, t.TargetBaseQty, pairLotBase(m), m.Meta.MinQty, m.Meta.MinNotional)
		}
		switch m.Provider {
		case entryProviderHyperliquid:
			hl = t.TargetBaseQty
		case entryProviderOKX:
			okx = t.TargetBaseQty
		}
	}
	if (hl > 0) == (okx > 0) {
		return "", fmt.Errorf("entry targets must have opposite signs (hyperliquid %v, okx %v)", hl, okx)
	}
	if !basketQtyEqual(math.Abs(hl), math.Abs(okx)) {
		return "", fmt.Errorf("entry targets must have equal absolute base quantity (hyperliquid %v, okx %v)", hl, okx)
	}
	want := pairOrientationLongHLShortOKX
	if okx > 0 {
		want = pairOrientationLongOKXShortHL
	}
	if res.Orientation != want {
		return "", fmt.Errorf("orientation %q disagrees with the target signs (%s)", res.Orientation, want)
	}
	return "", nil
}

type pairPendingFill struct {
	ActionID   string
	MemberID   string
	Signed     float64
	PriceBound float64
	Elective   bool
}

type pairLeg struct {
	Member         pairContractMember
	Booked         float64
	Unapplied      float64
	Val            pairMemberValuation
	ValOK          bool
	ValWhy         string
	MarkUSD        float64
	LowerUSD       float64
	UpperUSD       float64
	PriceRate      float64
	Cash           float64
	AvgCostUSD     float64
	Posted         float64
	BookedCosts    float64
	UnappliedFills []pairPendingFill
	Leverage       float64
	Account        pairAccountUSD
	AccountWhy     string
}

func (l pairLeg) exposure() float64 {
	return l.Booked + l.Unapplied
}

func (l pairLeg) imr() float64 {
	lev := l.Leverage
	if !(lev > 0) {
		lev = 1
	}
	return math.Max(l.Member.Meta.InitialMarginRate, 1/lev)
}

func (l pairLeg) feeRate() float64 {
	return math.Max(math.Max(l.Val.FeeRate, 0), l.Member.CostPolicy.TakerFeePct)
}

func (l pairLeg) adverseUSD(signed float64) float64 {
	if signed > 0 {
		return l.LowerUSD
	}
	return l.UpperUSD
}

type pairPlan struct {
	Limits  pairOperationalLimits
	Legs    map[string]*pairLeg
	Order   []string
	Pending []pairPendingFill
	NowMs   int64
	Views   pairViewSet
}

type pairEndpointReading struct {
	Gross     float64
	Venue     map[string]float64
	Unmatched float64
	Required  map[string]float64
	Available map[string]float64
	Liquidity map[string]float64
	Equity    map[string]float64
	Missing   map[string]string
}

func (p *pairPlan) upper(id string, fills []pairPendingFill) (float64, bool) {
	l := p.Legs[id]
	if l == nil || !l.ValOK || !basketFinitePositive(l.UpperUSD) || !basketFinitePositive(l.PriceRate) {
		return 0, false
	}
	u := l.UpperUSD
	for _, f := range fills {
		if f.MemberID == id && f.Signed > 0 && f.PriceBound*l.PriceRate > u {
			u = f.PriceBound * l.PriceRate
		}
	}
	return u, true
}

type pairResourceTotals struct {
	members     []string
	bookFree    float64
	bookEquity  float64
	venueDeduct float64
	bookedCosts float64
	upnl        float64
	required    float64
	maintenance float64
	feeReserve  float64
	account     pairAccountUSD
	missing     string
}

func (p *pairPlan) reading(fills []pairPendingFill, include []bool) (pairEndpointReading, error) {
	r := pairEndpointReading{Venue: map[string]float64{}, Required: map[string]float64{}, Available: map[string]float64{}, Liquidity: map[string]float64{}, Equity: map[string]float64{}, Missing: map[string]string{}}
	qty := map[string]float64{}
	added := map[string]float64{}
	feeNotional := map[string]float64{}
	chosenByMember := map[string][]pairPendingFill{}
	for _, id := range p.Order {
		qty[id] = p.Legs[id].exposure()
	}
	var chosen []pairPendingFill
	for i, f := range fills {
		if !include[i] {
			continue
		}
		chosen = append(chosen, f)
		chosenByMember[f.MemberID] = append(chosenByMember[f.MemberID], f)
		qty[f.MemberID] += f.Signed
		l := p.Legs[f.MemberID]
		px := 0.0
		if l != nil {
			px = f.PriceBound * l.PriceRate
			if l.ValOK && l.UpperUSD > px {
				px = l.UpperUSD
			}
		}
		feeNotional[f.MemberID] += math.Abs(f.Signed) * px
	}
	maxU := 0.0
	var net float64
	resources := map[string]*pairResourceTotals{}
	var resourceOrder []string
	for _, id := range p.Order {
		l := p.Legs[id]
		u, ok := p.upper(id, chosen)
		if !ok {
			why := l.ValWhy
			if why == "" {
				why = "no checked mark with a price conversion"
			}
			return r, fmt.Errorf("member %s has no own upper valuation at the reader time (%s)", id, why)
		}
		if u > maxU {
			maxU = u
		}
		n := math.Abs(qty[id]) * u
		r.Gross += n
		r.Venue[l.Member.Provider] += n
		net += qty[id]
		start := math.Abs(l.exposure())
		if math.Abs(qty[id]) > start {
			added[id] = math.Abs(qty[id]) - start
		}
		feeReserve := feeNotional[id] * l.feeRate()
		upnl := l.Booked * (l.adverseUSD(l.Booked) - l.AvgCostUSD)
		unappliedFee := 0.0
		unappliedIM := 0.0
		for _, f := range l.UnappliedFills {
			pxUSD := f.PriceBound * l.PriceRate
			upnl += f.Signed * (l.adverseUSD(f.Signed) - pxUSD)
			unappliedFee += math.Abs(f.Signed) * pxUSD * l.feeRate()
			if f.Elective {
				unappliedIM += math.Abs(f.Signed) * pxUSD * l.imr()
			}
		}
		for _, f := range chosenByMember[id] {
			pxUSD := f.PriceBound * l.PriceRate
			upnl += f.Signed * (l.adverseUSD(f.Signed) - pxUSD)
		}
		key := pairAccountResource(l.Val.Key)
		if l.Val.Key.MemberID == "" {
			key = id
		}
		res := resources[key]
		if res == nil {
			res = &pairResourceTotals{account: l.Account}
			resources[key] = res
			resourceOrder = append(resourceOrder, key)
		}
		res.members = append(res.members, id)
		res.required += added[id]*u*l.imr() + feeReserve
		res.maintenance += n * l.Member.Meta.MaintenanceMarginRate
		res.bookFree += l.Cash - l.Posted - unappliedIM - unappliedFee
		res.bookEquity += l.Cash + upnl - feeReserve - unappliedFee
		res.venueDeduct += l.Posted + unappliedIM + unappliedFee + l.BookedCosts
		res.bookedCosts += l.BookedCosts
		res.upnl += upnl
		res.feeReserve += feeReserve + unappliedFee
		switch {
		case l.AccountWhy != "":
			res.missing = pairJoinReasons(res.missing, l.AccountWhy)
		case res.account.EvidenceHash != l.Account.EvidenceHash:
			res.missing = pairJoinReasons(res.missing, fmt.Sprintf("conflicting account records %s and %s for one account resource", shortFingerprint(res.account.EvidenceHash), shortFingerprint(l.Account.EvidenceHash)))
		}
	}
	for _, key := range resourceOrder {
		res := resources[key]
		sort.Strings(res.members)
		label := strings.Join(res.members, "+")
		r.Required[label] = res.required
		r.Liquidity[label] = res.maintenance
		if res.missing != "" {
			r.Missing[label] = res.missing
			r.Available[label] = res.bookFree
			r.Equity[label] = res.bookEquity
			continue
		}
		venueFree := res.account.Free - res.venueDeduct
		venueEquity := res.account.Equity - res.bookedCosts + res.upnl - res.feeReserve
		r.Available[label] = math.Min(res.bookFree, venueFree)
		r.Equity[label] = math.Min(res.bookEquity, venueEquity)
	}
	r.Unmatched = math.Abs(net) * maxU
	return r, nil
}

func (r pairEndpointReading) violations(l pairOperationalLimits) []string {
	var out []string
	if !basketWithin(r.Gross, l.GrossBudgetUSD) {
		out = append(out, fmt.Sprintf("gross $%.2f > budget $%.2f", r.Gross, l.GrossBudgetUSD))
	}
	providers := make([]string, 0, len(r.Venue))
	for p := range r.Venue {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, p := range providers {
		if !basketWithin(r.Venue[p], l.venueCapUSD(p)) {
			out = append(out, fmt.Sprintf("%s $%.2f > venue cap $%.2f", p, r.Venue[p], l.venueCapUSD(p)))
		}
	}
	if !basketWithin(r.Unmatched, l.UnmatchedCapUSD) {
		out = append(out, fmt.Sprintf("unmatched $%.2f > cap $%.2f", r.Unmatched, l.UnmatchedCapUSD))
	}
	ids := make([]string, 0, len(r.Required))
	for id := range r.Required {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if why := r.Missing[id]; why != "" {
			out = append(out, fmt.Sprintf("%s free collateral evidence is unavailable: %s", id, why))
		}
		if r.Available[id] < r.Required[id] {
			out = append(out, fmt.Sprintf("%s free collateral $%.2f is below the $%.2f margin and fee reserve", id, r.Available[id], r.Required[id]))
		}
		if r.Equity[id] < r.Liquidity[id] {
			out = append(out, fmt.Sprintf("%s equity $%.2f is below the $%.2f maintenance margin", id, r.Equity[id], r.Liquidity[id]))
		}
	}
	return out
}

func pairNoWorse(after, before pairEndpointReading, l pairOperationalLimits) []string {
	var bad []string
	if !basketNoWorse(after.Gross, before.Gross, l.GrossBudgetUSD) {
		bad = append(bad, fmt.Sprintf("gross $%.2f", after.Gross))
	}
	for p, v := range after.Venue {
		if !basketNoWorse(v, before.Venue[p], l.venueCapUSD(p)) {
			bad = append(bad, fmt.Sprintf("%s $%.2f", p, v))
		}
	}
	if !basketNoWorse(after.Unmatched, before.Unmatched, l.UnmatchedCapUSD) {
		bad = append(bad, fmt.Sprintf("unmatched $%.2f (from $%.2f, cap $%.2f)", after.Unmatched, before.Unmatched, l.UnmatchedCapUSD))
	}
	sort.Strings(bad)
	return bad
}

type pairCandidate struct {
	MemberID     string
	Member       pairContractMember
	Purpose      string
	Direction    string
	OrderSide    string
	PositionSide string
	FullQty      float64
	PriceBound   float64
	Current      float64
	Target       float64

	ProposedBound float64

	Ref  *pairReductionRef
	Plan *pairPlan
}

func (c pairCandidate) riskBound() float64 {
	return pairMaxBound(c.ProposedBound, c.PriceBound)
}

func (c pairCandidate) minBound() float64 {
	return pairMinBound(c.ProposedBound, c.PriceBound)
}

func (c pairCandidate) signed(qty float64) float64 {
	if c.OrderSide == "buy" {
		return qty
	}
	return -qty
}

type pairEndpointProof struct {
	Legal    bool
	Detail   string
	Worst    pairEndpointReading
	Checked  int
	Breaches []string
}

func (p *pairPlan) proveEndpoints(extra *pairPendingFill, increase bool) pairEndpointProof {
	fills := append([]pairPendingFill(nil), p.Pending...)
	n := len(fills)
	if n > 12 {
		return pairEndpointProof{Detail: fmt.Sprintf("%d unresolved fills exceed the endpoint enumeration limit", n)}
	}
	withExtra := append(append([]pairPendingFill(nil), fills...), pairPendingFill{})
	if extra != nil {
		withExtra[n] = *extra
	}
	proof := pairEndpointProof{Legal: true}
	first := true
	for mask := 0; mask < 1<<n; mask++ {
		inc := make([]bool, n+1)
		for i := 0; i < n; i++ {
			inc[i] = mask&(1<<i) != 0
		}
		r0, err := p.reading(withExtra, inc)
		if err != nil {
			return pairEndpointProof{Detail: err.Error()}
		}
		readings := []pairEndpointReading{r0}
		if extra != nil {
			inc[n] = true
			r1, err := p.reading(withExtra, inc)
			if err != nil {
				return pairEndpointProof{Detail: err.Error()}
			}
			readings = append(readings, r1)
		}
		for _, r := range readings {
			proof.Checked++
			if first || r.Gross > proof.Worst.Gross {
				proof.Worst.Gross = r.Gross
			}
			if first || r.Unmatched > proof.Worst.Unmatched {
				proof.Worst.Unmatched = r.Unmatched
			}
			if first {
				proof.Worst.Venue = map[string]float64{}
			}
			for k, v := range r.Venue {
				if v > proof.Worst.Venue[k] {
					proof.Worst.Venue[k] = v
				}
			}
			first = false
		}
		var bad []string
		if increase {
			for _, r := range readings {
				bad = append(bad, r.violations(p.Limits)...)
			}
		} else if extra != nil {
			bad = pairNoWorse(readings[1], r0, p.Limits)
			for _, w := range readings[1].violations(p.Limits) {
				if !strings.Contains(w, "collateral") && !strings.Contains(w, "maintenance") {
					proof.Breaches = append(proof.Breaches, w)
				}
			}
		}
		if len(bad) > 0 {
			proof.Legal = false
			proof.Detail = fmt.Sprintf("the reachable outcome %0*b leaves %s", n+1, mask|boolBit(extra != nil, n), strings.Join(bad, "; "))
			return proof
		}
	}
	return proof
}

func boolBit(on bool, i int) int {
	if on {
		return 1 << i
	}
	return 0
}

func (p *pairPlan) legalTranche(c pairCandidate) (float64, pairEndpointProof, string) {
	lot := pairLotBase(c.Member)
	full := pairFloorLots(c.FullQty, lot)
	if c.Direction == basketDirectionReduce && basketQtyEqual(c.FullQty, math.Abs(c.Current)) {
		full = c.FullQty
	}
	if !(full > 0) {
		return 0, pairEndpointProof{}, fmt.Sprintf("%s %s %.10g has no legal lot (lot %.10g base)", c.MemberID, c.Direction, c.FullQty, lot)
	}
	increase := c.Direction == basketDirectionIncrease
	try := func(q float64) pairEndpointProof {
		f := pairPendingFill{ActionID: "candidate", MemberID: c.MemberID, Signed: c.signed(q), PriceBound: c.riskBound(), Elective: true}
		return p.proveEndpoints(&f, increase)
	}
	minimum := pairLegalLot
	if !increase {
		minimum = pairReductionMinimum
	}
	if proof := try(full); proof.Legal {
		if !minimum(c.Member, full, c.minBound()) {
			return 0, proof, fmt.Sprintf("%s %s %.10g is below the venue minimum", c.MemberID, c.Direction, full)
		}
		return full, proof, ""
	} else if strings.Contains(proof.Detail, "valuation") || strings.Contains(proof.Detail, "enumeration") {
		return 0, proof, proof.Detail
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 60; i++ {
		mid := (lo + hi) / 2
		if try(full * mid).Legal {
			lo = mid
		} else {
			hi = mid
		}
	}
	qty := pairFloorLots(full*lo, lot)
	if !(qty > 0) {
		proof := try(full)
		return 0, proof, fmt.Sprintf("%s %s: no legal lot-sized tranche (%s)", c.MemberID, c.Direction, proof.Detail)
	}
	proof := try(qty)
	if !proof.Legal {
		return 0, proof, fmt.Sprintf("%s %s: the floored tranche %.10g is not legal (%s)", c.MemberID, c.Direction, qty, proof.Detail)
	}
	if !minimum(c.Member, qty, c.minBound()) {
		return 0, proof, fmt.Sprintf("%s %s %.10g is below the venue minimum", c.MemberID, c.Direction, qty)
	}
	return qty, proof, ""
}

func pairChooseFirstLeg(rt *basketRuntime) string {
	members := rt.Contract.pairMembersSorted()
	if len(members) == 0 {
		return ""
	}
	best := members[0]
	for _, m := range members[1:] {
		if pairLotBase(m) > pairLotBase(best)*(1+1e-12) {
			best = m
		}
	}
	return best.MemberID
}

func suppressPairMemberIncrease(sc StrategyConfig, signal *int, closeFraction, posQty float64, posSide, signalStr string, logger *StrategyLogger) bool {
	if !isPairMember(sc) || signal == nil || *signal == 0 {
		return false
	}
	allowsLong, allowsShort := true, false
	if sc.Type == "perps" {
		allowsLong, allowsShort = PerpsAllowsLong(sc), PerpsAllowsShort(sc)
	}
	if !pausedBlocksSignal(*signal, closeFraction, posQty, posSide, allowsLong, allowsShort) {
		return false
	}
	if logger != nil {
		logger.Error("Pair member check returned a position-increasing %s signal; only the pair coordinator places member increases, so it is dropped (#1862)", signalStr)
	}
	*signal = 0
	return true
}

const pairManagementInputVersion = 1

const pairManagementStdinFlag = "--management-stdin"

type pairManagementInput struct {
	V         int          `json:"v"`
	Candles   [][6]float64 `json:"candles"`
	MarkPrice float64      `json:"mark_price"`
}

func runOKXPairManagementCheck(sc StrategyConfig, posCtx PositionCtx, input pairManagementInput, logger *StrategyLogger) (*OKXResult, string, error) {
	_, result, stderr, err := runOKXPairManagementCheckRaw(sc, posCtx, input, logger)
	if err != nil {
		return nil, stderr, err
	}
	if result.Error != "" {
		return nil, stderr, fmt.Errorf("%s", result.Error)
	}
	if msg := pairMemberCheckError(sc, result.PairMember); msg != "" {
		return nil, stderr, fmt.Errorf("%s", msg)
	}
	return result, stderr, nil
}

func runOKXPairManagementCheckRaw(sc StrategyConfig, posCtx PositionCtx, input pairManagementInput, logger *StrategyLogger) ([]string, *OKXResult, string, error) {
	return runOKXPairManagementCheckRawWith(sc, posCtx, input, logger, runPythonReadOnlyWithStdin)
}

func runOKXPairManagementCheckRawWith(sc StrategyConfig, posCtx PositionCtx, input pairManagementInput, logger *StrategyLogger, run func(script string, args []string, stdin []byte) ([]byte, []byte, error)) ([]string, *OKXResult, string, error) {
	if !isPairMember(sc) || sc.Platform != "okx" {
		return nil, nil, "", fmt.Errorf("strategy %s is not an OKX pair member", sc.ID)
	}
	input.V = pairManagementInputVersion
	stdin, err := json.Marshal(input)
	if err != nil {
		return nil, nil, "", err
	}
	args := append([]string{}, sc.Args...)
	args = appendOpenCloseArgs(args, sc, posCtx)
	if refsArgs, err := buildStrategyRefsArg(sc, "", false); err != nil {
		return args, nil, "", err
	} else if len(refsArgs) > 0 {
		args = append(args, refsArgs...)
	}
	args = appendPairMemberArg(args, sc)
	args = append(args, pairManagementStdinFlag)
	if logger != nil {
		logger.Running(sc.Script, args)
	}
	stdout, stderr, runErr := run(sc.Script, args, stdin)
	var result OKXResult
	if jsonErr := json.Unmarshal(stdout, &result); jsonErr != nil {
		if runErr != nil {
			return args, nil, string(stderr), fmt.Errorf("script error: %w", runErr)
		}
		return args, nil, string(stderr), fmt.Errorf("parse output: %w", jsonErr)
	}
	if result.Error != "" {
		return args, &result, string(stderr), nil
	}
	if runErr != nil {
		return args, nil, string(stderr), fmt.Errorf("script error: %w", runErr)
	}
	return args, &result, string(stderr), nil
}
