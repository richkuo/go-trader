package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	pairMarkStale       = "stale"
	pairMarkSubstituted = "substituted"
	pairMarkUnavailable = "unavailable"
)

type pairViewSet map[string]pairMemberValuation

type pairLastMark struct {
	MarkUSD  float64
	LowerUSD float64
	UpperUSD float64
	AtMs     int64
}

type pairViewSource interface {
	pairMarksLocked() *pairMarks
	pairEmergencyPendingLocked(part RiskPartition) []string
}

type pairMarks struct {
	nowMs int64
	views pairViewSet
	saved map[string]int64
}

type pairAccountUSD struct {
	Resource     string
	EvidenceHash string
	Equity       float64
	Free         float64
	MarginUsed   float64
}

type pairValueStatus struct {
	Members map[string]string
	Reasons map[string]string
}

func pairParsePx(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil && basketFinitePositive(f)
}

func pairDecimalToFloat(d pairDecimal) (float64, bool) {
	r, err := d.rat()
	if err != nil {
		return 0, false
	}
	f, _ := r.Float64()
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

func (v pairMemberValuation) clone() pairMemberValuation {
	out := v
	out.Conversions = make(map[string]float64, len(v.Conversions))
	for k, c := range v.Conversions {
		out.Conversions[k] = c
	}
	out.ConversionEvidence = append([]pairConversionEvidence{}, v.ConversionEvidence...)
	out.Observation = pairCloneObservation(v.Observation)
	out.Capability.Meta = v.Capability.Meta.clone()
	return out
}

func (vs pairViewSet) clone() pairViewSet {
	out := make(pairViewSet, len(vs))
	for id, v := range vs {
		out[id] = v.clone()
	}
	return out
}

func (v pairMemberValuation) conversion(currency string) (float64, bool) {
	c, ok := v.Conversions[strings.ToUpper(strings.TrimSpace(currency))]
	return c, ok && basketFinitePositive(c)
}

func (v pairMemberValuation) conversionAt(currency string, nowMs int64) (float64, string) {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if cur == pairUSD {
		return 1, ""
	}
	if !v.ConversionValid {
		return 0, fmt.Sprintf("%s has no valid conversion evidence (%s)", v.Key.MemberID, v.MissingReason)
	}
	for _, c := range v.ConversionEvidence {
		if c.Currency != cur {
			continue
		}
		if !pairEligibleAt(c.pairEvidenceStamp, nowMs) {
			return 0, fmt.Sprintf("%s %s conversion %s is not valid at %d (valid %d..%d, received %d)", v.Key.MemberID, cur, c.EvidenceID, nowMs, c.ValidFromMs, c.ValidThroughMs, c.FirstReceiptMs)
		}
		rate, ok := pairDecimalToFloat(c.RateUSD)
		if !ok || !basketFinitePositive(rate) {
			return 0, fmt.Sprintf("%s %s conversion %s has no positive rate", v.Key.MemberID, cur, c.EvidenceID)
		}
		return rate, ""
	}
	return 0, fmt.Sprintf("%s has no %s conversion evidence", v.Key.MemberID, cur)
}

func (v pairMemberValuation) usdMarkAt(nowMs int64) (px float64, valid bool, reason string) {
	if !v.MarkValid {
		why := v.MissingReason
		if why == "" {
			why = "no checked reference"
		}
		return 0, false, fmt.Sprintf("%s has no valid mark: %s", v.Key.MemberID, why)
	}
	if !pairAgeWithin(nowMs, v.EvidenceMs, v.MaxAgeMs) {
		return 0, false, fmt.Sprintf("%s quote evidence at %d is not within %d ms of the reader time %d", v.Key.MemberID, v.EvidenceMs, v.MaxAgeMs, nowMs)
	}
	rate, why := v.conversionAt(v.PriceCurrency, nowMs)
	if why != "" {
		return 0, false, why
	}
	px = v.Mark * rate
	if !basketFinitePositive(px) {
		return 0, false, fmt.Sprintf("%s USD mark %v is not finite and positive", v.Key.MemberID, px)
	}
	return px, true, ""
}

func (v pairMemberValuation) usdBoundsAt(nowMs int64) (lower, upper float64, valid bool, reason string) {
	if _, ok, why := v.usdMarkAt(nowMs); !ok {
		return 0, 0, false, why
	}
	rate, why := v.conversionAt(v.PriceCurrency, nowMs)
	if why != "" {
		return 0, 0, false, why
	}
	lower, upper = v.LowerPx*rate, v.UpperPx*rate
	if !basketFinitePositive(lower) || !basketFinitePositive(upper) || lower > upper {
		return 0, 0, false, fmt.Sprintf("%s USD bounds %v and %v are not ordered finite positive prices", v.Key.MemberID, lower, upper)
	}
	return lower, upper, true, ""
}

func (v pairMemberValuation) capabilityReasonAt(nowMs int64) string {
	if !v.CapabilityValid {
		return fmt.Sprintf("%s has no valid capability evidence (%s)", v.Key.MemberID, v.MissingReason)
	}
	if !pairEligibleAt(v.Capability.pairEvidenceStamp, nowMs) {
		return fmt.Sprintf("%s capability %s is not valid at %d (valid %d..%d)", v.Key.MemberID, v.Capability.EvidenceID, nowMs, v.Capability.ValidFromMs, v.Capability.ValidThroughMs)
	}
	return ""
}

func (v pairMemberValuation) accountAt(nowMs int64) (pairAccountUSD, string) {
	if !v.CollateralValid {
		return pairAccountUSD{}, fmt.Sprintf("%s has no valid account evidence (%s)", v.Key.MemberID, v.MissingReason)
	}
	a := v.Account
	if !pairEligibleAt(a.pairEvidenceStamp, nowMs) {
		return pairAccountUSD{}, fmt.Sprintf("%s account %s is not valid at %d (valid %d..%d)", v.Key.MemberID, a.EvidenceID, nowMs, a.ValidFromMs, a.ValidThroughMs)
	}
	rate, why := v.conversionAt(a.CollateralCurrency, nowMs)
	if why != "" {
		return pairAccountUSD{}, why
	}
	eq, ok1 := pairDecimalToFloat(a.Equity)
	free, ok2 := pairDecimalToFloat(a.FreeCollateral)
	used, ok3 := pairDecimalToFloat(a.MarginUsed)
	if !ok1 || !ok2 || !ok3 {
		return pairAccountUSD{}, fmt.Sprintf("%s account %s has an unreadable amount", v.Key.MemberID, a.EvidenceID)
	}
	return pairAccountUSD{Resource: pairAccountResource(v.Key), EvidenceHash: a.EvidenceHash, Equity: eq * rate, Free: free * rate, MarginUsed: used * rate}, ""
}

func pairAccountResource(k pairMemberKey) string {
	return strings.Join([]string{k.Partition.String(), k.Provider, k.AccountKey}, "|")
}

func (v pairMemberValuation) increaseReasonAt(nowMs int64) string {
	if _, ok, why := v.usdMarkAt(nowMs); !ok {
		return why
	}
	for _, cur := range []string{v.PriceCurrency, v.SettlementCurrency} {
		if _, why := v.conversionAt(cur, nowMs); why != "" {
			return why
		}
	}
	for _, c := range v.ConversionEvidence {
		if _, why := v.conversionAt(c.Currency, nowMs); why != "" {
			return why
		}
	}
	if why := v.capabilityReasonAt(nowMs); why != "" {
		return why
	}
	if _, why := v.accountAt(nowMs); why != "" {
		return why
	}
	return ""
}

func (v pairMemberValuation) evidenceLimitMs() int64 {
	end, ok := pairAddMs(v.EvidenceMs, v.MaxAgeMs)
	if !ok || !v.MarkValid {
		return 0
	}
	for _, c := range v.ConversionEvidence {
		if c.ValidThroughMs < end {
			end = c.ValidThroughMs
		}
	}
	if v.CapabilityValid && v.Capability.ValidThroughMs < end {
		end = v.Capability.ValidThroughMs
	}
	if v.CollateralValid && v.Account.ValidThroughMs < end {
		end = v.Account.ValidThroughMs
	}
	return end
}

func (v pairMemberValuation) depthBound(side string, baseQty float64) (float64, bool) {
	levels := v.Observation.NativeBids
	if side == "buy" {
		levels = v.Observation.NativeAsks
	}
	if !v.MarkValid || len(levels) == 0 || !basketFinitePositive(v.ContractSize) {
		return 0, false
	}
	var cum, px float64
	for _, l := range levels {
		p, ok := pairParsePx(l.Px)
		q, okq := pairParsePx(l.Qty)
		if !ok || !okq {
			return 0, false
		}
		px = p
		cum += q * v.ContractSize
		if cum >= baseQty*(1-1e-12) {
			break
		}
	}
	return px, basketFinitePositive(px)
}

func (v pairMemberValuation) atReader(nowMs int64) pairMemberValuation {
	out := v.clone()
	if v.MarkValid {
		if _, ok, why := v.usdMarkAt(nowMs); !ok {
			out.MarkValid = false
			out.MissingReason = pairJoinReasons(out.MissingReason, pairMarkStale+": "+why)
		}
	}
	if v.CapabilityValid && v.capabilityReasonAt(nowMs) != "" {
		out.CapabilityValid = false
		out.MissingReason = pairJoinReasons(out.MissingReason, v.capabilityReasonAt(nowMs))
	}
	if v.CollateralValid {
		if _, why := v.accountAt(nowMs); why != "" {
			out.CollateralValid = false
			out.MissingReason = pairJoinReasons(out.MissingReason, why)
		}
	}
	return out
}

func pairJoinReasons(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func (bc *basketCoordinator) preparePairRisk(in basketCycleInputs) {
	if !bc.active() {
		return
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	for _, id := range bc.sortedPairIDs() {
		rt := bc.runtimes[id]
		bc.notePairPrepHostInputs(rt, in)
		bc.preparePairRiskFor(rt, in)
	}
}

func (bc *basketCoordinator) notePairPrepHostInputs(rt *basketRuntime, in basketCycleInputs) {
	var missing []string
	if in.PairSnapshotFor == nil {
		missing = append(missing, "PairSnapshotFor")
	}
	if in.PairGeneration == nil {
		missing = append(missing, "PairGeneration")
	}
	var sealed []string
	for _, m := range rt.Contract.pairMembersSorted() {
		if m.Source == pairSourceSealedFeed {
			sealed = append(sealed, m.MemberID)
		}
	}
	note := ""
	if len(missing) > 0 && len(sealed) > 0 {
		note = fmt.Sprintf("the host calls preparePairRisk without %s, so sealed member %s has no checked reference: every valuation of it holds and the incomplete-valuation time limit latches; the host must pass its sealed snapshot callback and feed generation",
			strings.Join(missing, " and "), strings.Join(sealed, ", "))
	}
	if note == rt.pairPrepHostNote {
		return
	}
	rt.pairPrepHostNote = note
	if note != "" {
		bc.emit(rt, note, true)
	}
}

func (bc *basketCoordinator) pairViewBase(rt *basketRuntime, m pairContractMember, fp string) pairMemberValuation {
	return pairMemberValuation{
		Key:                 rt.Contract.pairMemberKey(m),
		Symbol:              m.Symbol,
		ContractFingerprint: fp,
		MaxAgeMs:            rt.Pair.Limits.QuoteAgeMs,
		ContractSize:        m.Meta.ContractSize,
		Conversions:         map[string]float64{pairUSD: 1},
		FeeRate:             m.CostPolicy.TakerFeePct,
		PriceCurrency:       m.Meta.PriceCurrency,
		SettlementCurrency:  m.Meta.SettlementCurrency,
	}
}

type pairPreparedReads struct {
	obs     pairObservation
	refErr  error
	cap     pairCapabilityEvidence
	capErr  error
	acct    pairAccountEvidence
	acctErr error
	convs   []pairConversionEvidence
	convErr error
}

func (bc *basketCoordinator) preparePairRiskFor(rt *basketRuntime, in basketCycleInputs) {
	rt.pairPrepSeq++
	seq := rt.pairPrepSeq
	members := rt.Contract.pairMembersSorted()
	fp := rt.Contract.Fingerprint()
	partition := rt.Partition.String()
	before := bc.pairSnapshot(rt)
	out := make(pairViewSet, len(members))
	refusal := bc.pairLiveRefusal(rt)
	evidence := rt.Pair.admission.evidenceSource()
	budget := time.Duration(rt.Pair.Limits.CollectionTimeoutMs) * time.Millisecond
	if refusal == "" && budget <= 0 {
		refusal = "the pair has no collection timeout to bound its valuation reads"
	}
	if refusal == "" && evidence == nil {
		refusal = "the pair has no admitted evidence source"
	}
	wallStart := time.Now()
	ctx, cancel := context.WithTimeout(bc.parent, budget)
	defer cancel()
	for _, m := range members {
		v := bc.pairViewBase(rt, m, fp)
		if refusal != "" {
			v.MissingReason = refusal
			out[m.MemberID] = v
			continue
		}
		side := "buy"
		if bc.pairExposure(rt, m.MemberID) > 0 {
			side = "sell"
		}
		var r pairPreparedReads
		r.obs, r.refErr = bc.readPairReferenceLocked(ctx, rt, in, m, side)
		refMs := bc.pairNow(in).UnixMilli()
		bc.unlocked(func() {
			r.cap, r.capErr = evidence.Capability(ctx, m, refMs)
			r.acct, r.acctErr = evidence.Account(ctx, m, refMs)
			r.convs, r.convErr = evidence.Conversions(ctx, m, refMs)
		})
		out[m.MemberID] = pairFillView(v, m, partition, r, refMs)
	}
	nowMs := bc.pairNow(in).UnixMilli()
	if rt.pairPrepSeq != seq || bc.runtimes[rt.Contract.BasketID] != rt {
		bc.pairLogf(rt, "a newer valuation preparation replaced this one; its results are discarded")
		return
	}
	var prepErr error
	if refusal == "" {
		_, prepErr = bc.pairWallBudget("valuation", ctx, wallStart)
	}
	changed := ""
	switch {
	case !before.same(bc.pairSnapshot(rt)):
		changed = "the pair records changed while the valuation view was prepared"
	case rt.Contract.Fingerprint() != fp:
		changed = "the pair contract changed while the valuation view was prepared"
	case prepErr != nil:
		changed = fmt.Sprintf("the valuation preparation budget ended (%v)", prepErr)
	}
	for id, v := range out {
		if changed != "" {
			v.MarkValid, v.ReferenceValid, v.ConversionValid, v.CapabilityValid, v.CollateralValid = false, false, false, false, false
			v.MissingReason = pairJoinReasons(changed, v.MissingReason)
		} else {
			v = pairRecheckView(v, nowMs)
		}
		out[id] = v
	}
	bc.publishPairView(rt, out, nowMs)
}

func pairFillView(v pairMemberValuation, m pairContractMember, partition string, r pairPreparedReads, refMs int64) pairMemberValuation {
	var reasons []string
	if r.convErr != nil {
		reasons = append(reasons, "conversion evidence: "+r.convErr.Error())
	} else {
		ok := true
		have := map[string]bool{}
		for _, c := range r.convs {
			if c.Key.Partition != partition {
				reasons, ok = append(reasons, fmt.Sprintf("conversion evidence %s is outside partition %s", c.EvidenceID, partition)), false
				continue
			}
			if err := pairConversionMatches(m, c); err != nil {
				reasons, ok = append(reasons, err.Error()), false
				continue
			}
			rate, rok := pairDecimalToFloat(c.RateUSD)
			if !rok || !basketFinitePositive(rate) {
				reasons, ok = append(reasons, fmt.Sprintf("conversion evidence %s has no positive rate", c.EvidenceID)), false
				continue
			}
			have[c.Currency] = true
			v.Conversions[c.Currency] = rate
			v.ConversionEvidence = append(v.ConversionEvidence, c)
		}
		for _, cur := range pairRequiredConversionCurrencies(m) {
			if !have[cur] {
				reasons, ok = append(reasons, fmt.Sprintf("no %s conversion evidence", cur)), false
			}
		}
		v.ConversionValid = ok
	}
	if r.capErr != nil {
		reasons = append(reasons, "capability evidence: "+r.capErr.Error())
	} else if r.cap.Key.Partition != partition {
		reasons = append(reasons, fmt.Sprintf("capability evidence %s is outside partition %s", r.cap.EvidenceID, partition))
	} else if err := pairCapabilityMatches(m, r.cap); err != nil {
		reasons = append(reasons, err.Error())
	} else {
		v.Capability = r.cap
		v.Capability.Meta = r.cap.Meta.clone()
		v.CapabilityValid = true
	}
	if r.acctErr != nil {
		reasons = append(reasons, "account evidence: "+r.acctErr.Error())
	} else if r.acct.Key.Partition != partition {
		reasons = append(reasons, fmt.Sprintf("account evidence %s is outside partition %s", r.acct.EvidenceID, partition))
	} else if err := pairAccountMatches(m, r.acct); err != nil {
		reasons = append(reasons, err.Error())
	} else {
		v.Account = r.acct
		v.CollateralValid = true
	}
	if r.refErr != nil {
		reasons = append(reasons, "reference: "+r.refErr.Error())
	} else {
		bid, okb := pairTopPx(r.obs.NativeBids)
		ask, oka := pairTopPx(r.obs.NativeAsks)
		switch {
		case !okb || !oka || bid >= ask:
			reasons = append(reasons, "the checked reference has no ordered best bid and ask")
		default:
			v.Observation = pairCloneObservation(r.obs)
			v.Mark, v.LowerPx, v.UpperPx = (bid+ask)/2, bid, ask
			v.EvidenceMs = r.obs.FirstReceiptMs
			v.MarkValid, v.ReferenceValid = true, true
		}
	}
	if v.MarkValid {
		rate, why := v.conversionAt(v.PriceCurrency, refMs)
		if why != "" {
			v.MarkValid = false
			reasons = append(reasons, why)
		} else {
			v.MarkUSD, v.LowerUSD, v.UpperUSD = v.Mark*rate, v.LowerPx*rate, v.UpperPx*rate
		}
	}
	sort.Strings(reasons)
	v.MissingReason = strings.Join(reasons, "; ")
	return v
}

func pairTopPx(levels []pairNativeLevel) (float64, bool) {
	if len(levels) == 0 {
		return 0, false
	}
	return pairParsePx(levels[0].Px)
}

func pairRecheckView(v pairMemberValuation, nowMs int64) pairMemberValuation {
	if v.MarkValid {
		if _, ok, why := v.usdMarkAt(nowMs); !ok {
			v.MarkValid = false
			v.MissingReason = pairJoinReasons(v.MissingReason, why)
		}
	}
	if v.CapabilityValid {
		if why := v.capabilityReasonAt(nowMs); why != "" {
			v.CapabilityValid = false
			v.MissingReason = pairJoinReasons(v.MissingReason, why)
		}
	}
	if v.CollateralValid {
		if _, why := v.accountAt(nowMs); why != "" {
			v.CollateralValid = false
			v.MissingReason = pairJoinReasons(v.MissingReason, why)
		}
	}
	return v
}

type pairViewHashInput struct {
	Key                 pairEvidenceKey  `json:"key"`
	ContractFingerprint string           `json:"contract_fingerprint"`
	Observation         *pairObservation `json:"observation"`
	CapabilityHash      string           `json:"capability_hash"`
	ConversionHashes    []string         `json:"conversion_hashes"`
	AccountHash         string           `json:"account_hash"`
}

func (v pairMemberValuation) computeEvidenceHash() string {
	in := pairViewHashInput{Key: v.Key.evidenceKey(), ContractFingerprint: v.ContractFingerprint, ConversionHashes: []string{}}
	if v.EvidenceMs > 0 {
		obs := pairCloneObservation(v.Observation)
		in.Observation = &obs
	}
	if v.CapabilityValid {
		in.CapabilityHash = v.Capability.EvidenceHash
	}
	if v.CollateralValid {
		in.AccountHash = v.Account.EvidenceHash
	}
	for _, c := range v.ConversionEvidence {
		in.ConversionHashes = append(in.ConversionHashes, c.EvidenceHash)
	}
	sort.Strings(in.ConversionHashes)
	b, err := pairCanonicalJSON(in)
	if err != nil {
		return ""
	}
	return pairSHA256(b)
}

func (bc *basketCoordinator) publishPairView(rt *basketRuntime, out pairViewSet, nowMs int64) {
	rt.pairViewGen++
	if rt.pairLastMark == nil {
		rt.pairLastMark = map[string]pairLastMark{}
	}
	for id, v := range out {
		v.Generation = rt.pairViewGen
		v.EvidenceHash = v.computeEvidenceHash()
		v.MarkLostAtMs, v.MarkLostAtRestart = pairMarkLostAt(v, rt.pairView, id, nowMs)
		switch last, ok := rt.pairLastMark[id]; {
		case basketFinitePositive(v.MarkUSD) && v.EvidenceMs > 0:
			rt.pairLastMark[id] = pairLastMark{MarkUSD: v.MarkUSD, LowerUSD: v.LowerUSD, UpperUSD: v.UpperUSD, AtMs: v.EvidenceMs}
			v.Substituted = !v.MarkValid
		case ok:
			v.MarkUSD, v.LowerUSD, v.UpperUSD = last.MarkUSD, last.LowerUSD, last.UpperUSD
			v.Substituted = true
		}
		out[id] = v
	}
	rt.pairView = out
	var bad []string
	for _, id := range rt.memberIDs() {
		if v, ok := out[id]; ok && v.MissingReason != "" {
			bad = append(bad, id+": "+v.MissingReason)
		}
	}
	note := strings.Join(bad, " | ")
	if note != rt.pairViewNote {
		rt.pairViewNote = note
		if note != "" {
			bc.pairLogf(rt, "valuation view %d holds increases and scoring: %s; queries, settlement, protection and recovery continue", rt.pairViewGen, note)
		} else {
			bc.pairLogf(rt, "valuation view %d is complete for both members", rt.pairViewGen)
		}
	}
}

func (bc *basketCoordinator) pairViews(now time.Time) pairViewSet {
	if !bc.active() {
		return pairViewSet{}
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.pairViewsLocked(now)
}

func (bc *basketCoordinator) pairViewsLocked(now time.Time) pairViewSet {
	out := pairViewSet{}
	if !bc.active() {
		return out
	}
	nowMs := now.UnixMilli()
	for _, id := range bc.sortedPairIDs() {
		for mid, v := range bc.runtimes[id].pairView {
			out[mid] = v.atReader(nowMs)
		}
	}
	return out
}

func (bc *basketCoordinator) pairReaderNow() time.Time {
	clock := bc.pairClock
	if clock == nil {
		clock = tradingNow
	}
	return clock()
}

func (bc *basketCoordinator) pairMarksLocked() *pairMarks {
	if !bc.active() {
		return nil
	}
	now := bc.pairReaderNow()
	views := pairViewSet{}
	saved := map[string]int64{}
	for _, id := range bc.sortedPairIDs() {
		rt := bc.runtimes[id]
		for mid, v := range rt.pairView {
			views[mid] = v.clone()
		}
		if prs := bc.state.partitionRiskIfPresent(rt.Partition); prs != nil && !prs.PairValuationIncompleteSince.IsZero() {
			for _, mid := range rt.memberIDs() {
				saved[mid] = prs.PairValuationIncompleteSince.UnixMilli()
			}
		}
	}
	return &pairMarks{nowMs: now.UnixMilli(), views: views, saved: saved}
}

func newPairMarks(nowMs int64, views pairViewSet) *pairMarks {
	return &pairMarks{nowMs: nowMs, views: views.clone()}
}

func (s *AppState) pairMarksLocked() *pairMarks {
	if s == nil || s.pairViews == nil {
		return nil
	}
	return s.pairViews.pairMarksLocked()
}

func (pm *pairMarks) markFor(s *StrategyState, sym string) (px float64, state, reason string, pair bool) {
	if !strategyPairCostRequired(s) {
		return 0, "", "", false
	}
	if pm == nil {
		return 0, pairMarkUnavailable, "no coordinator valuation view reached this reader", true
	}
	v, ok := pm.views[s.ID]
	if !ok {
		return 0, pairMarkUnavailable, "the coordinator has published no valuation view for this member", true
	}
	if !strings.EqualFold(v.Symbol, sym) {
		return 0, pairMarkUnavailable, fmt.Sprintf("the valuation view names %s, the book holds %s", v.Symbol, sym), true
	}
	if usd, ok, _ := v.usdMarkAt(pm.nowMs); ok {
		return usd, "", "", true
	}
	_, _, why := v.usdMarkAt(pm.nowMs)
	switch {
	case v.MarkValid && basketFinitePositive(v.MarkUSD):
		return v.MarkUSD, pairMarkStale, why, true
	case v.Substituted && basketFinitePositive(v.MarkUSD):
		return v.MarkUSD, pairMarkSubstituted, why, true
	}
	return 0, pairMarkUnavailable, why, true
}

func pairMarkLostAt(v pairMemberValuation, prev pairViewSet, id string, nowMs int64) (int64, bool) {
	if v.MarkValid {
		return 0, false
	}
	p, ok := prev[id]
	switch {
	case !ok:
		return nowMs, true
	case !p.MarkValid && p.MarkLostAtMs > 0:
		return p.MarkLostAtMs, p.MarkLostAtRestart
	case p.MarkValid:
		if end, eok := pairAddMs(p.EvidenceMs, p.MaxAgeMs); eok && end < nowMs {
			return end, false
		}
		return nowMs, false
	}
	return nowMs, true
}

func (pm *pairMarks) incompleteSinceMs(id string) (since int64, known, priorUnknown bool) {
	if pm == nil {
		return 0, false, true
	}
	v, ok := pm.views[id]
	if !ok {
		return 0, false, true
	}
	if !v.MarkValid {
		return v.MarkLostAtMs, v.MarkLostAtMs > 0, v.MarkLostAtRestart || v.MarkLostAtMs <= 0
	}
	if _, valid, _ := v.usdMarkAt(pm.nowMs); valid {
		return 0, false, false
	}
	if end, eok := pairAddMs(v.EvidenceMs, v.MaxAgeMs); eok && end < pm.nowMs {
		return end, true, false
	}
	return pm.nowMs, true, false
}

func (pm *pairMarks) sinceFor(id string) int64 {
	since, known, priorUnknown := pm.incompleteSinceMs(id)
	if saved := pm.saved[id]; saved > 0 && priorUnknown && (!known || saved < since) {
		return saved
	}
	if !known {
		return pm.nowMs
	}
	return since
}

func (pm *pairMarks) partitionOverdue(st pairValueStatus) (int64, map[string]int64) {
	if pm == nil || st.complete() {
		return 0, nil
	}
	members := pm.sinces(st)
	var start int64
	for id, since := range members {
		if start == 0 || since < start {
			start = since
		}
		if saved := pm.saved[id]; saved > 0 && saved < start {
			start = saved
		}
	}
	if start == 0 || pm.nowMs-start < untrustedEquityLatchDeferral.Milliseconds() {
		return 0, nil
	}
	return start, members
}

func (pm *pairMarks) sinces(st pairValueStatus) map[string]int64 {
	if pm == nil || st.complete() {
		return nil
	}
	out := make(map[string]int64, len(st.Members))
	for id := range st.Members {
		out[id] = pm.sinceFor(id)
	}
	return out
}

func (pm *pairMarks) overdue(st pairValueStatus) map[string]int64 {
	if pm == nil || st.complete() {
		return nil
	}
	limit := untrustedEquityLatchDeferral.Milliseconds()
	var out map[string]int64
	for id := range st.Members {
		since := pm.sinceFor(id)
		if pm.nowMs-since < limit {
			continue
		}
		if out == nil {
			out = map[string]int64{}
		}
		out[id] = since
	}
	return out
}

func pairOverdueDetail(overdue map[string]int64, st pairValueStatus) string {
	ids := make([]string, 0, len(overdue))
	for id := range overdue {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s has had no checked mark since %s (%s: %s)", id, time.UnixMilli(overdue[id]).UTC().Format(time.RFC3339), st.Members[id], st.Reasons[id]))
	}
	return strings.Join(parts, "; ")
}

func (st *pairValueStatus) note(id, state, reason string) {
	if state == "" {
		return
	}
	if st.Members == nil {
		st.Members, st.Reasons = map[string]string{}, map[string]string{}
	}
	if prev, ok := st.Members[id]; ok && pairMarkRank(prev) >= pairMarkRank(state) {
		return
	}
	st.Members[id], st.Reasons[id] = state, reason
}

func pairMarkRank(state string) int {
	switch state {
	case pairMarkUnavailable:
		return 3
	case pairMarkSubstituted:
		return 2
	case pairMarkStale:
		return 1
	}
	return 0
}

func (st *pairValueStatus) merge(o pairValueStatus) {
	for id, s := range o.Members {
		st.note(id, s, o.Reasons[id])
	}
}

func (st pairValueStatus) complete() bool {
	return len(st.Members) == 0
}

func (st pairValueStatus) label() string {
	if st.complete() {
		return ""
	}
	ids := make([]string, 0, len(st.Members))
	for id := range st.Members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+" "+st.Members[id])
	}
	return "pair valuation incomplete: " + strings.Join(parts, ", ")
}

func (st pairValueStatus) detail() string {
	if st.complete() {
		return ""
	}
	ids := make([]string, 0, len(st.Members))
	for id := range st.Members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", id, st.Members[id], st.Reasons[id]))
	}
	return strings.Join(parts, "; ")
}

type pairAccountDisplay struct {
	Partition     string   `json:"partition"`
	Provider      string   `json:"provider"`
	AccountKey    string   `json:"account_key"`
	Members       []string `json:"members"`
	EvidenceID    string   `json:"evidence_id,omitempty"`
	EquityUSD     float64  `json:"equity_usd"`
	FreeUSD       float64  `json:"free_collateral_usd"`
	MarginUsedUSD float64  `json:"margin_used_usd"`
	Valid         bool     `json:"valid"`
	Reason        string   `json:"reason,omitempty"`
}

func pairAccountDisplays(pm *pairMarks) []pairAccountDisplay {
	if pm == nil || len(pm.views) == 0 {
		return nil
	}
	byResource := map[string]*pairAccountDisplay{}
	ids := make([]string, 0, len(pm.views))
	for id := range pm.views {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		v := pm.views[id]
		key := pairAccountResource(v.Key)
		d := byResource[key]
		acct, why := v.accountAt(pm.nowMs)
		if d == nil {
			d = &pairAccountDisplay{Partition: v.Key.Partition.String(), Provider: v.Key.Provider, AccountKey: v.Key.AccountKey, EvidenceID: v.Account.EvidenceID,
				EquityUSD: acct.Equity, FreeUSD: acct.Free, MarginUsedUSD: acct.MarginUsed, Valid: why == "", Reason: why}
			byResource[key] = d
		} else if d.Valid && (why != "" || acct.EvidenceHash != v.Account.EvidenceHash || d.EvidenceID != v.Account.EvidenceID) {
			d.Valid = false
			d.Reason = pairJoinReasons(d.Reason, pairJoinReasons(why, "conflicting account records for one account resource"))
		}
		d.Members = append(d.Members, id)
	}
	keys := make([]string, 0, len(byResource))
	for k := range byResource {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]pairAccountDisplay, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byResource[k])
	}
	return out
}

func pairConservativePx(pos *Position, state string, mark float64) float64 {
	if state == "" {
		return mark
	}
	if pos == nil {
		return mark
	}
	if !basketFinitePositive(mark) {
		return pos.AvgCost
	}
	if pos.Side == entrySideShort {
		return math.Max(mark, pos.AvgCost)
	}
	return math.Min(mark, pos.AvgCost)
}
