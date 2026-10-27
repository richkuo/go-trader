package main

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"sort"
	"time"
)

const (
	pairObligationFundingIncomplete = "funding_incomplete"
	pairObligationFundingGap        = "funding_gap"
	pairObligationFundingIntegrity  = "funding_integrity"

	pairFundingStatusIncomplete   = "incomplete"
	pairFundingStatusApplied      = "applied"
	pairFundingStatusVoid         = "void"
	pairFundingStatusUnresolvable = "unresolvable"

	pairFundingOrderIDPrefix = "pair_funding:"
)

func pairFundingObligation(kind string) bool {
	return kind == pairObligationFundingIncomplete || kind == pairObligationFundingGap || kind == pairObligationFundingIntegrity
}

func pairFundingHoldsAccounting(kind string) bool {
	return kind == pairObligationFundingIncomplete || kind == pairObligationFundingGap
}

type pairFundingHashMemo struct {
	eventMs int64
	rate    pairDecimal
	hash    string
}

type pairFundingAppliedRef struct {
	EventMs                  int64
	SourceHash               string
	EvidenceHash             string
	PolicyDigest             string
	ObservationRecordHash    string
	ObservationTargetEventMs int64
}

type pairFundingIncomplete struct {
	MemberID    string
	EventID     string
	RuleVersion string
	EventMs     int64
	Status      string
	Reason      string
	Detail      string
	SourceHash  string
	CreatedMs   int64
	UpdatedMs   int64
	dirty       bool
}

type pairFundingApplication struct {
	MemberID string
	Event    pairFundingEventValue
	JSON     []byte
	AtMs     int64
}

type pairFundingChainRow struct {
	Seq       int64
	SourceID  string
	BatchHash string
	PrevHash  string
	JSON      []byte
}

type pairFundingVenueEventKey struct {
	Key     pairEvidenceKey
	EventID string
}

type pairFundingRuntime struct {
	source            pairFundingSource
	binding           []byte
	contract          string
	members           map[string]*pairFundingMember
	applied           map[string]pairFundingAppliedRef
	pending           map[string]*pairFundingApplication
	incomplete        map[string]*pairFundingIncomplete
	appliedVenueRules map[pairFundingVenueEventKey]string
	pendingVenueRules map[pairFundingVenueEventKey]string
	chainHead         int64
	chainSaved        []pairFundingChainRow
	hashes            map[string]pairFundingHashMemo
	hashMisses        int64
}

func (f *pairFundingRuntime) sourceHash(r pairFundingRate) (string, error) {
	id := pairFundingIdentity(r.Key, r.EventID, r.RuleVersion)
	if m, ok := f.hashes[id]; ok && m.eventMs == r.EventMs && m.rate == r.Rate {
		return m.hash, nil
	}
	f.hashMisses++
	sh, err := pairFundingSourceHash(r)
	if err != nil {
		return "", err
	}
	f.hashes[id] = pairFundingHashMemo{eventMs: r.EventMs, rate: r.Rate, hash: sh}
	return sh, nil
}

func pairFundingKeyOf(memberID, eventID, rule string) string {
	return memberID + "|" + eventID + "|" + rule
}

func newPairFundingRuntime(adm *pairResearchAdmission, c basketContract) *pairFundingRuntime {
	f := &pairFundingRuntime{source: adm.funding, binding: append([]byte(nil), adm.fundingBinding...), contract: c.Fingerprint(),
		members: map[string]*pairFundingMember{}, applied: map[string]pairFundingAppliedRef{}, pending: map[string]*pairFundingApplication{}, incomplete: map[string]*pairFundingIncomplete{},
		appliedVenueRules: map[pairFundingVenueEventKey]string{}, pendingVenueRules: map[pairFundingVenueEventKey]string{}, hashes: map[string]pairFundingHashMemo{}}
	coins := map[string]string{}
	for _, m := range c.Members {
		coins[m.StrategyID] = m.Coin
	}
	for _, pm := range c.pairMembersSorted() {
		cs, err := pairDecimalFromFloat(pm.Meta.ContractSize)
		if err != nil {
			cs = "0"
		}
		f.members[pm.MemberID] = &pairFundingMember{memberID: pm.MemberID, key: c.pairMemberKey(pm).evidenceKey(), member: pm, coin: coins[pm.MemberID], contractSize: cs}
	}
	return f
}

func (f *pairFundingRuntime) memberIDs() []string {
	out := make([]string, 0, len(f.members))
	for id := range f.members {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (f *pairFundingRuntime) hasPending() bool {
	return f != nil && len(f.pending) > 0
}

func (f *pairFundingRuntime) recordedChainPending() []pairFundingChainRow {
	rs, ok := f.source.(*pairRecordedFundingSource)
	if !ok {
		return nil
	}
	var out []pairFundingChainRow
	for _, b := range rs.chain() {
		if b.Seq <= f.chainHead {
			continue
		}
		out = append(out, pairFundingChainRow{Seq: b.Seq, SourceID: b.SourceID, BatchHash: b.BatchHash, PrevHash: b.PrevHash, JSON: rs.chainRaw(b.Seq)})
	}
	return out
}

func (bc *basketCoordinator) pairInventoryThrough(rt *basketRuntime, m *pairFundingMember) int64 {
	h := m.observedMs
	for _, a := range rt.Actions {
		if a.StrategyID != m.memberID || !pairActionFillPossible(rt, a) {
			continue
		}
		start := a.SubmittedAtMs
		if start <= 0 {
			start = a.CreatedAtMs
		}
		bound := start - 1
		if a.ConfirmedQty-a.AppliedQty <= 1e-12 && a.CoverageThroughMs > bound {
			bound = a.CoverageThroughMs
		}
		if bound < h {
			h = bound
		}
	}
	if from := pairNativeUnknownFromMs(rt, m.memberID); from > 0 && from-1 < h {
		h = from - 1
	}
	return h
}

func (bc *basketCoordinator) pairFillPossibleBefore(rt *basketRuntime, memberID string, t int64) bool {
	for _, a := range rt.Actions {
		if a.StrategyID != memberID || !pairActionFillPossible(rt, a) {
			continue
		}
		start := a.SubmittedAtMs
		if start <= 0 {
			start = a.CreatedAtMs
		}
		if start < t {
			return true
		}
	}
	if from := pairNativeUnknownFromMs(rt, memberID); from > 0 && from < t {
		return true
	}
	return false
}

func (bc *basketCoordinator) observePairInventory(rt *basketRuntime, nowMs int64) {
	f := rt.Pair.funding
	if f == nil {
		return
	}
	for _, id := range f.memberIDs() {
		if s := bc.state.Strategies[id]; s != nil {
			f.members[id].observe(s, nowMs)
		}
	}
}

func (bc *basketCoordinator) drainPairFundingIntegrity(rt *basketRuntime, now time.Time) bool {
	f := rt.Pair.funding
	changed := false
	for _, id := range f.memberIDs() {
		m := f.members[id]
		faults := m.integrity
		m.integrity = nil
		for _, msg := range faults {
			if !rt.Pair.Progress.addObligation(pairObligation{Kind: pairObligationFundingIntegrity, MemberID: id, ActionID: "integrity:" + pairSHA256([]byte(msg))[:16], Detail: msg, AtMs: now.UnixMilli()}) {
				continue
			}
			bc.pairOpenRecovery(rt, fmt.Sprintf("member %s funding integrity hold: %s", id, msg), now)
			bc.pairAlert(rt, "funding-integrity|"+id+"|"+msg, fmt.Sprintf("CRITICAL pair %s member %s funding integrity hold: %s; increases are frozen and an operator must review the venue account", rt.Contract.BasketID, id, msg), true)
			changed = true
		}
	}
	return changed
}

func (bc *basketCoordinator) stepPairFundingDue(in basketCycleInputs) {
	if !bc.active() {
		return
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	for _, id := range bc.sortedPairIDs() {
		rt := bc.runtimes[id]
		if bc.pairLiveRefusal(rt) != "" || !rt.Pair.admission.costDurable() {
			continue
		}
		step := bc.refreshPairInputs(rt, in)
		bc.stepPairFunding(rt, step)
	}
}

func (bc *basketCoordinator) stepPairFunding(rt *basketRuntime, in basketCycleInputs) {
	f := rt.Pair.funding
	if f == nil {
		return
	}
	now := in.Now
	nowMs := now.UnixMilli()
	bc.observePairInventory(rt, nowMs)
	changed := bc.drainPairFundingIntegrity(rt, now)
	if in.SaveBlocked || rt.needsDurable || f.hasPending() {
		if changed {
			rt.touchPairProgress()
			bc.persistPair(rt)
		}
		return
	}
	ids := f.memberIDs()
	reqs := make([]pairFundingRequest, len(ids))
	for i, id := range ids {
		reqs[i] = pairFundingRequest{Key: f.members[id].key, FromMs: 0, ThroughMs: nowMs, CutoffMs: nowMs}
	}
	timeout := time.Duration(rt.Pair.Limits.CollectionTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Second
	}
	src := f.source
	got := make([]pairFundingCollected, len(ids))
	errs := make([]error, len(ids))
	snapBefore := bc.pairSnapshot(rt)
	parent := bc.parent
	if parent == nil {
		parent = context.Background()
	}
	bc.unlocked(func() {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		for i := range ids {
			got[i], errs[i] = src.Collect(ctx, reqs[i])
		}
	})
	if !snapBefore.same(bc.pairSnapshot(rt)) || rt.Pair.funding != f {
		bc.pairLogf(rt, "the pair records changed while funding input was collected; funding settles next cycle")
		if changed {
			rt.touchPairProgress()
			bc.persistPair(rt)
		}
		return
	}
	applied := 0
	for i, id := range ids {
		if errs[i] != nil {
			bc.pairAlert(rt, "funding-collect|"+id+"|"+errs[i].Error(), fmt.Sprintf("pair %s member %s funding input could not be collected: %v; nothing settles and nothing is booked as zero", rt.Contract.BasketID, id, errs[i]), false)
			continue
		}
		n, c := bc.settlePairMemberFunding(rt, f, f.members[id], got[i], now)
		applied += n
		changed = changed || c
	}
	if pairFundingSourcePolicy(f.source) != nil {
		changed = bc.drainPairFundingIntegrity(rt, now) || changed
	}
	if applied > 0 {
		rt.touchPairProgress()
		if err := bc.store.SavePartition(bc.state, rt.Partition); err != nil {
			rt.needsDurable = true
			bc.pairLogf(rt, "%d funding charge(s) are booked in memory, but the %s state file did not save: %v; no elective order is sent until it does, and a restart settles each event once from the saved evidence", applied, partitionLabel(rt.Partition), err)
		}
		return
	}
	if changed {
		rt.touchPairProgress()
		bc.persistPair(rt)
	}
}

func pairFundingDedupeRates(rates []pairFundingRate, hash func(pairFundingRate) (string, error)) ([]pairFundingRate, map[string]string) {
	out := []pairFundingRate{}
	seen := map[string]string{}
	conflicts := map[string]string{}
	for _, r := range rates {
		id := pairFundingIdentity(r.Key, r.EventID, r.RuleVersion)
		sh, err := hash(r)
		if err != nil {
			conflicts[id] = err.Error()
			continue
		}
		if prev, ok := seen[id]; ok {
			if prev != sh {
				conflicts[id] = fmt.Sprintf("source hashes %s and %s", shortFingerprint(prev), shortFingerprint(sh))
			}
			continue
		}
		seen[id] = sh
		out = append(out, r)
	}
	kept := []pairFundingRate{}
	for _, r := range out {
		if _, bad := conflicts[pairFundingIdentity(r.Key, r.EventID, r.RuleVersion)]; !bad {
			kept = append(kept, r)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].EventMs != kept[j].EventMs {
			return kept[i].EventMs < kept[j].EventMs
		}
		return kept[i].RuleVersion < kept[j].RuleVersion
	})
	return kept, conflicts
}

func (bc *basketCoordinator) noteFundingIncomplete(rt *basketRuntime, f *pairFundingRuntime, m *pairFundingMember, r pairFundingRate, sourceHash, reason, detail string, now time.Time) bool {
	key := pairFundingKeyOf(m.memberID, r.EventID, r.RuleVersion)
	nowMs := now.UnixMilli()
	cur := f.incomplete[key]
	changed := false
	if cur == nil {
		cur = &pairFundingIncomplete{MemberID: m.memberID, EventID: r.EventID, RuleVersion: r.RuleVersion, EventMs: r.EventMs, CreatedMs: nowMs}
		f.incomplete[key] = cur
		changed = true
	}
	if cur.SourceHash == "" {
		cur.SourceHash = sourceHash
	}
	if cur.Status != pairFundingStatusIncomplete || cur.Reason != reason || cur.Detail != detail {
		cur.Status, cur.Reason, cur.Detail, cur.UpdatedMs, cur.dirty = pairFundingStatusIncomplete, reason, detail, nowMs, true
		changed = true
	}
	ob := pairObligation{Kind: pairObligationFundingIncomplete, MemberID: m.memberID, ActionID: r.EventID + "|" + r.RuleVersion, Detail: reason + ": " + detail, AtMs: nowMs}
	if rt.Pair.Progress.addObligation(ob) {
		changed = true
		bc.pairAlert(rt, "funding|"+key+"|"+reason, fmt.Sprintf("pair %s member %s funding event %s (%s) is incomplete: %s: %s; it is kept durable, never booked as zero, and settles once when its evidence is complete", rt.Contract.BasketID, m.memberID, r.EventID, r.RuleVersion, reason, detail), false)
	}
	return changed
}

func (bc *basketCoordinator) voidFundingIncomplete(rt *basketRuntime, f *pairFundingRuntime, m *pairFundingMember, r pairFundingRate, detail string, now time.Time) bool {
	key := pairFundingKeyOf(m.memberID, r.EventID, r.RuleVersion)
	cur := f.incomplete[key]
	if cur == nil || cur.Status == pairFundingStatusVoid {
		return false
	}
	cur.Status, cur.Reason, cur.Detail, cur.UpdatedMs, cur.dirty = pairFundingStatusVoid, "zero_inventory", detail, now.UnixMilli(), true
	rt.Pair.Progress.clearObligation(pairObligationFundingIncomplete, m.memberID, r.EventID+"|"+r.RuleVersion)
	return true
}

func (bc *basketCoordinator) noteFundingUnresolvable(rt *basketRuntime, f *pairFundingRuntime, m *pairFundingMember, r pairFundingRate, sourceHash, reason, detail string, now time.Time) bool {
	if !bc.markFundingUnresolvable(rt, f, m, r, sourceHash, reason, detail, now) {
		return false
	}
	key := pairFundingKeyOf(m.memberID, r.EventID, r.RuleVersion)
	bc.pairAlert(rt, "funding-unresolvable|"+key, fmt.Sprintf("CRITICAL pair %s member %s funding event %s (%s) can never settle from the saved inventory history: %s: %s; no charge is booked, the event is saved as unresolvable with this detail, and it does not hold pair completion", rt.Contract.BasketID, m.memberID, r.EventID, r.RuleVersion, reason, detail), true)
	return true
}

func (bc *basketCoordinator) markFundingUnresolvable(rt *basketRuntime, f *pairFundingRuntime, m *pairFundingMember, r pairFundingRate, sourceHash, reason, detail string, now time.Time) bool {
	key := pairFundingKeyOf(m.memberID, r.EventID, r.RuleVersion)
	nowMs := now.UnixMilli()
	cur := f.incomplete[key]
	if cur != nil && cur.Status == pairFundingStatusUnresolvable {
		return false
	}
	if cur == nil {
		cur = &pairFundingIncomplete{MemberID: m.memberID, EventID: r.EventID, RuleVersion: r.RuleVersion, EventMs: r.EventMs, CreatedMs: nowMs}
		f.incomplete[key] = cur
	}
	if cur.SourceHash == "" {
		cur.SourceHash = sourceHash
	}
	cur.Status, cur.Reason, cur.Detail, cur.UpdatedMs, cur.dirty = pairFundingStatusUnresolvable, reason, detail, nowMs, true
	rt.Pair.Progress.clearObligation(pairObligationFundingIncomplete, m.memberID, r.EventID+"|"+r.RuleVersion)
	return true
}

func (m *pairFundingMember) qtyRangeAt(t, windowMs int64) (*big.Rat, *big.Rat) {
	lo, hi := new(big.Rat), new(big.Rat)
	end := t
	if windowMs > 0 {
		end = t + windowMs
	}
	for i, r := range m.rows {
		d := m.rowQty(i)
		switch {
		case r.EventLatestMs < t:
			lo.Add(lo, d)
			hi.Add(hi, d)
		case r.EventMs < end:
			if d.Sign() < 0 {
				lo.Add(lo, d)
			} else {
				hi.Add(hi, d)
			}
		}
	}
	return lo, hi
}

func (m *pairFundingMember) positionBefore(t int64) string {
	var best *pairInventoryRow
	for i := range m.rows {
		r := &m.rows[i]
		if r.EventLatestMs >= t {
			continue
		}
		if best == nil || r.EventLatestMs > best.EventLatestMs || (r.EventLatestMs == best.EventLatestMs && r.Seq > best.Seq) {
			best = r
		}
	}
	if best == nil {
		return ""
	}
	return best.PositionID
}

func pairFundingObservationFor(observations []pairFundingOracleObservation, key pairEvidenceKey, eventMs, cutoffMs int64, policy pairFundingPolicyV1) (pairFundingOracleObservation, string) {
	target, ok := pairFundingResearchTarget(eventMs, policy)
	if !ok {
		return pairFundingOracleObservation{}, ""
	}
	digest, err := pairFundingPolicyDigest(policy)
	if err != nil {
		return pairFundingOracleObservation{}, err.Error()
	}
	byHash := map[string]pairFundingOracleObservation{}
	for _, o := range observations {
		if o.Key != key || o.TargetEventMs != target || o.VisibleMs > cutoffMs || o.PolicyDigest != digest {
			continue
		}
		if err := pairFundingValidateObservation(o, policy); err != nil {
			return pairFundingOracleObservation{}, err.Error()
		}
		byHash[o.RecordHash] = o
	}
	if len(byHash) == 0 {
		return pairFundingOracleObservation{}, ""
	}
	if len(byHash) > 1 {
		hashes := make([]string, 0, len(byHash))
		for hash := range byHash {
			hashes = append(hashes, hash)
		}
		sort.Strings(hashes)
		return pairFundingOracleObservation{}, fmt.Sprintf("conflicting oracle observations for %s at target %d: %s and %s", key, target, shortFingerprint(hashes[0]), shortFingerprint(hashes[1]))
	}
	for _, o := range byHash {
		return o, ""
	}
	return pairFundingOracleObservation{}, ""
}

func pairFundingUnknownAmount(lo, hi *big.Rat, rule pairFundingRule, ruleOK bool, rate pairDecimal, prices []pairFundingPrice, observations []pairFundingOracleObservation, t, cutoffMs int64, policy *pairFundingPolicyV1, key pairEvidenceKey) string {
	out := fmt.Sprintf("the signed base position before %d lies between %s and %s", t, pairDecimalOfRat(lo), pairDecimalOfRat(hi))
	if !ruleOK {
		return out + "; the charge is unknown"
	}
	px := pairDecimal("")
	why := ""
	if rule.RuleVersion == pairFundingRuleHLReceiptResearch && policy != nil {
		observation, conflict := pairFundingObservationFor(observations, key, t, cutoffMs, *policy)
		px, why = observation.Price, conflict
	} else {
		price, conflict := pairFundingPriceFor(prices, rule.PriceKind, t)
		px, why = price.Price, conflict
	}
	rateRat, rerr := rate.rat()
	pxRat, perr := px.rat()
	if why != "" || rerr != nil || perr != nil || pxRat.Sign() <= 0 {
		return out + "; the charge is unknown"
	}
	one := big.NewRat(1, 1)
	a, _, err1 := pairFundingAmounts(lo, pxRat, rateRat, one)
	b, _, err2 := pairFundingAmounts(hi, pxRat, rateRat, one)
	if err1 != nil || err2 != nil {
		return out + "; the charge is unknown"
	}
	return fmt.Sprintf("%s; the unknown native charge lies between %s and %s %s (rate %s, %s price %s)", out, a, b, rule.Currency, rate, rule.PriceKind, px)
}

func pairFundingPriceFor(prices []pairFundingPrice, kind string, atMs int64) (pairFundingPrice, string) {
	var best *pairFundingPrice
	for i := range prices {
		p := &prices[i]
		if p.PriceKind != kind || p.AtMs != atMs {
			continue
		}
		if best != nil && best.Price != p.Price {
			return pairFundingPrice{}, fmt.Sprintf("price records %s and %s value %d differently (%s, %s)", best.EvidenceID, p.EvidenceID, atMs, best.Price, p.Price)
		}
		if best == nil || p.VisibleMs < best.VisibleMs || (p.VisibleMs == best.VisibleMs && p.EvidenceID < best.EvidenceID) {
			best = p
		}
	}
	if best == nil {
		return pairFundingPrice{}, ""
	}
	return *best, ""
}

func pairFundingCoverageGaps(cov []pairFundingCoverageRecord, requested []pairFundingInterval) ([]pairFundingInterval, []pairCoverageGap) {
	var proven []pairFundingInterval
	var declared []pairCoverageGap
	for _, c := range cov {
		proven = append(proven, c.Proven...)
		declared = append(declared, c.Gaps...)
	}
	proven = pairFundingNormalize(proven)
	open := pairFundingSubtract(requested, proven)
	var gaps []pairCoverageGap
	for _, g := range open {
		reason := "unrequested"
		for _, d := range declared {
			if d.FromMs < g.ThroughMs && g.FromMs < d.ThroughMs {
				reason = d.Reason
				break
			}
		}
		gaps = append(gaps, pairCoverageGap{FromMs: g.FromMs, ThroughMs: g.ThroughMs, Reason: reason})
	}
	return pairFundingIntersect(proven, requested), gaps
}

func pairFundingObservationIntegrity(policy *pairFundingPolicyV1, rate pairFundingRate, observations []pairFundingOracleObservation, cutoffMs int64, expectedHash string) string {
	if expectedHash == "" || policy == nil {
		return ""
	}
	if rule, ok := pairFundingSelectedRateRule(policy, rate); !ok || rule.RuleVersion != pairFundingRuleHLReceiptResearch {
		return ""
	}
	observation, conflict := pairFundingObservationFor(observations, rate.Key, rate.EventMs, cutoffMs, *policy)
	if conflict != "" {
		return fmt.Sprintf("funding event %s has %s", rate.EventID, conflict)
	}
	if observation.RecordHash != "" && observation.RecordHash != expectedHash {
		return fmt.Sprintf("funding event %s used observation %s, but admitted evidence now selects %s", rate.EventID, shortFingerprint(expectedHash), shortFingerprint(observation.RecordHash))
	}
	return ""
}

func pairFundingVenueRuleCollision(f *pairFundingRuntime, key pairEvidenceKey, eventID, rule string) (string, bool) {
	venueKey := pairFundingVenueEventKey{Key: key, EventID: eventID}
	if owner, ok := f.appliedVenueRules[venueKey]; ok && owner != rule {
		return owner, true
	}
	if owner, ok := f.pendingVenueRules[venueKey]; ok && owner != rule {
		return owner, true
	}
	return "", false
}

func (bc *basketCoordinator) settlePairMemberFunding(rt *basketRuntime, f *pairFundingRuntime, m *pairFundingMember, got pairFundingCollected, now time.Time) (int, bool) {
	nowMs := now.UnixMilli()
	changed := false
	applied := 0
	s := bc.state.Strategies[m.memberID]
	if s == nil {
		return 0, false
	}
	policy := pairFundingSourcePolicy(f.source)
	records := pairFundingSourceRecordSet(f.source)
	hInv := bc.pairInventoryThrough(rt, m)
	rates, conflicts := pairFundingDedupeRates(got.Rates, f.sourceHash)
	for _, r := range got.Rates {
		id := pairFundingIdentity(r.Key, r.EventID, r.RuleVersion)
		if why, bad := conflicts[id]; bad {
			changed = bc.noteFundingIncomplete(rt, f, m, r, "", pairFundingReasonConflict, "the funding input holds conflicting records for one event identity: "+why, now) || changed
			delete(conflicts, id)
		}
	}
	for idx, r := range rates {
		key := pairFundingKeyOf(m.memberID, r.EventID, r.RuleVersion)
		sh, _ := f.sourceHash(r)
		if ref, ok := f.applied[key]; ok {
			if ref.SourceHash != sh {
				m.integrity = append(m.integrity, fmt.Sprintf("funding event %s was applied from source %s, but the input now carries source %s for the same identity", r.EventID, shortFingerprint(ref.SourceHash), shortFingerprint(sh)))
			}
			if msg := pairFundingObservationIntegrity(policy, r, got.Observations, nowMs, ref.ObservationRecordHash); msg != "" {
				m.integrity = append(m.integrity, msg)
			}
			continue
		}
		if app, ok := f.pending[key]; ok {
			if app.Event.SourceHash != sh {
				m.integrity = append(m.integrity, fmt.Sprintf("funding event %s is staged from source %s, but the input now carries source %s", r.EventID, shortFingerprint(app.Event.SourceHash), shortFingerprint(sh)))
			}
			expectedHash := ""
			if app.Event.Research != nil {
				expectedHash = app.Event.Research.ObservationRecordHash
			}
			if msg := pairFundingObservationIntegrity(policy, r, got.Observations, nowMs, expectedHash); msg != "" {
				m.integrity = append(m.integrity, msg)
			}
			continue
		}
		if cur := f.incomplete[key]; cur != nil && cur.Status == pairFundingStatusUnresolvable {
			if cur.SourceHash != "" && cur.SourceHash != sh {
				m.integrity = append(m.integrity, fmt.Sprintf("funding event %s was saved as unresolvable from source %s, but the input now carries source %s for the same identity", r.EventID, shortFingerprint(cur.SourceHash), shortFingerprint(sh)))
			}
			continue
		}
		if cur := f.incomplete[key]; cur != nil && cur.SourceHash != "" && cur.SourceHash != sh {
			changed = bc.noteFundingIncomplete(rt, f, m, r, "", pairFundingReasonConflict, fmt.Sprintf("the saved incomplete event carries source %s, the input now carries %s", shortFingerprint(cur.SourceHash), shortFingerprint(sh)), now) || changed
			continue
		}
		t := r.EventMs
		rule, ruleOK := pairFundingRuleFor(r.RuleVersion)
		if policy != nil {
			rule, ruleOK = pairFundingSelectedRateRule(policy, r)
		}
		window := int64(0)
		if ruleOK {
			window = rule.AssessmentWindowMs
		}
		need := t
		if t+window-1 > need {
			need = t + window - 1
		}
		if nowMs < need {
			continue
		}
		if hold := pairNativeFundingHoldFromMs(rt, m.memberID); hold > 0 && hold <= need {
			detail := fmt.Sprintf("an operator native decision left the %s inventory unproven from %d and nothing releases that hold, so the inventory the event needs through %d can never be proven", m.memberID, hold, need)
			if bc.markFundingUnresolvable(rt, f, m, r, sh, pairFundingReasonNativeDecision, detail, now) {
				changed = true
				bc.pairAlert(rt, fmt.Sprintf("funding-native-hold|%s|%d", m.memberID, hold), fmt.Sprintf("pair %s member %s: each funding event that needs inventory from %d onward is saved unresolvable (%s) with no charge and no funding obligation, because an operator native decision left that inventory unproven; the result stays incomplete", rt.Contract.BasketID, m.memberID, hold, pairFundingReasonNativeDecision), false)
			}
			continue
		}
		if hInv < need {
			q, amb := m.qtyBefore(t)
			if q.Sign() == 0 && !amb && !bc.pairFillPossibleBefore(rt, m.memberID, need+1) {
				continue
			}
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonInventory, fmt.Sprintf("the inventory stream is complete only through %d, the event needs %d", hInv, need), now) || changed
			continue
		}
		q, amb := m.qtyBefore(t)
		if amb {
			lo, hi := m.qtyRangeAt(t, 0)
			changed = bc.noteFundingUnresolvable(rt, f, m, r, sh, pairFundingReasonFillTime, fmt.Sprintf("a saved inventory change has an unknown time around %d; %s", t, pairFundingUnknownAmount(lo, hi, rule, ruleOK, r.Rate, got.Prices, got.Observations, t, nowMs, policy, m.key)), now) || changed
			continue
		}
		if window > 0 && m.windowTouched(t, window) {
			lo, hi := m.qtyRangeAt(t, window)
			changed = bc.noteFundingUnresolvable(rt, f, m, r, sh, pairFundingReasonAssessment, fmt.Sprintf("an inventory change may lie inside the venue assessment window [%d, %d); %s", t, t+window, pairFundingUnknownAmount(lo, hi, rule, ruleOK, r.Rate, got.Prices, got.Observations, t, nowMs, policy, m.key)), now) || changed
			continue
		}
		if q.Sign() == 0 {
			changed = bc.voidFundingIncomplete(rt, f, m, r, fmt.Sprintf("the inventory before %d is proven zero", t), now) || changed
			continue
		}
		positionID := m.positionBefore(t)
		if positionID == "" {
			changed = bc.noteFundingUnresolvable(rt, f, m, r, sh, pairFundingReasonPositionUnknown, fmt.Sprintf("the saved inventory history holds %s base before %d but names no position identity; %s", pairDecimalOfRat(q), t, pairFundingUnknownAmount(q, q, rule, ruleOK, r.Rate, got.Prices, got.Observations, t, nowMs, policy, m.key)), now) || changed
			continue
		}
		if !ruleOK {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonRuleUnsupported, fmt.Sprintf("rule %q has no checked venue rule in this build", r.RuleVersion), now) || changed
			continue
		}
		if rule.Provider != m.key.Provider {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonRuleProvider, fmt.Sprintf("rule %s serves %s, the member is on %s; another venue's rate never replaces this one", r.RuleVersion, rule.Provider, m.key.Provider), now) || changed
			continue
		}
		if rule.Currency != m.member.Meta.SettlementCurrency {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonCurrency, fmt.Sprintf("rule %s settles in %s, the member in %s", r.RuleVersion, rule.Currency, m.member.Meta.SettlementCurrency), now) || changed
			continue
		}
		prev := m.stretchStart(t)
		for j := idx - 1; j >= 0; j-- {
			if rates[j].EventMs < t {
				if rates[j].EventMs > prev {
					prev = rates[j].EventMs
				}
				break
			}
		}
		requested := []pairFundingInterval{{FromMs: prev, ThroughMs: t}}
		proven, gaps := pairFundingCoverageGaps(got.Coverage, requested)
		if len(gaps) > 0 {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonCoverageGap, fmt.Sprintf("funding coverage does not prove (%d, %d] complete: gap (%d, %d] %s", prev, t, gaps[0].FromMs, gaps[0].ThroughMs, gaps[0].Reason), now) || changed
			continue
		}
		nativeMark, priceHash, priceVisible, why := pairDecimal(""), "", int64(0), ""
		var observation pairFundingOracleObservation
		if rule.RuleVersion == pairFundingRuleHLReceiptResearch {
			observation, why = pairFundingObservationFor(got.Observations, m.key, t, nowMs, *policy)
			nativeMark, priceHash, priceVisible = observation.Price, observation.RecordHash, observation.VisibleMs
		} else {
			price, conflict := pairFundingPriceFor(got.Prices, rule.PriceKind, t)
			nativeMark, priceHash, priceVisible, why = price.Price, price.RecordHash, price.VisibleMs, conflict
		}
		if why != "" {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonConflict, why, now) || changed
			continue
		}
		if priceHash == "" {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonPriceMissing, fmt.Sprintf("no %s price at %d was visible by %d", rule.PriceKind, t, nowMs), now) || changed
			continue
		}
		conv, reason, cerr := rt.Pair.admission.admittedEvidence().FundingConversion(m.member, rule.Currency, t, nowMs)
		if cerr != nil {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, reason, cerr.Error(), now) || changed
			continue
		}
		rateRat, _ := r.Rate.rat()
		pxRat, _ := nativeMark.rat()
		convRat, _ := conv.RateUSD.rat()
		native, usd, aerr := pairFundingAmounts(q, pxRat, rateRat, convRat)
		if aerr != nil {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonArithmetic, aerr.Error(), now) || changed
			continue
		}
		visible := r.VisibleMs
		for _, v := range []int64{priceVisible, conv.FirstReceiptMs, conv.ObservedMs} {
			if v > visible {
				visible = v
			}
		}
		for _, c := range got.Coverage {
			if len(pairFundingIntersect(c.Proven, requested)) > 0 && c.VisibleMs > visible {
				visible = c.VisibleMs
			}
		}
		ev := pairFundingEvent{V: pairFundingEventVersion, Key: m.key, EventID: r.EventID, RuleVersion: r.RuleVersion, EventMs: t, VisibleMs: visible,
			SignedBaseQty: pairDecimalOfRat(q), NativeMark: nativeMark, Rate: r.Rate, Currency: rule.Currency, NativeAmount: pairDecimal(native), USDAmount: pairDecimal(usd),
			Conversion: conv, SourceHash: sh, PriceHash: priceHash,
			Coverage: pairFundingCoverage{V: pairFundingCoverageVersion, Key: m.key, RequestedFromMs: prev, RequestedThroughMs: t, Proven: proven, Gaps: []pairCoverageGap{}, InventoryThroughMs: hInv, SourceKind: got.SourceKind, SourceID: got.SourceID, CutoffMs: nowMs}}
		value := pairFundingEventValue{pairFundingEvent: ev}
		if rule.RuleVersion == pairFundingRuleHLReceiptResearch {
			digest, err := pairFundingPolicyDigest(*policy)
			if err != nil {
				changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonArithmetic, err.Error(), now) || changed
				continue
			}
			ev.V = pairFundingResearchEventVersion
			value = pairFundingEventV3Value(pairFundingEventV3{pairFundingEvent: ev, PolicyDigest: digest, ObservationEvidenceID: observation.EvidenceID, ObservationRecordHash: observation.RecordHash, ObservationTargetEventMs: observation.TargetEventMs, ObservationRequestMs: observation.RequestMs, ObservationReceiptMs: observation.ReceiptMs, ObservationTimeBasis: observation.TimeBasis})
		}
		sealed, err := value.sealed()
		if err == nil {
			err = pairFundingValidateSelectedEvent(sealed, policy, records)
		}
		var raw []byte
		if err == nil {
			raw, err = pairFundingEventValueBytes(sealed)
		}
		if err != nil {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonArithmetic, "the funding event failed its own checks: "+err.Error(), now) || changed
			continue
		}
		usdF, _ := new(big.Rat).SetString(usd)
		amount, _ := usdF.Float64()
		if math.IsNaN(amount) || math.IsInf(amount, 0) {
			changed = bc.noteFundingIncomplete(rt, f, m, r, sh, pairFundingReasonArithmetic, "the USD amount has no finite float projection", now) || changed
			continue
		}
		if owner, collision := pairFundingVenueRuleCollision(f, m.key, r.EventID, r.RuleVersion); collision {
			m.integrity = append(m.integrity, fmt.Sprintf("funding event %s for %s is already owned by rule %s; rule %s cannot apply a second charge", r.EventID, m.key, owner, r.RuleVersion))
			continue
		}
		if bc.stagePairFunding(rt, f, m, s, sealed, raw, amount, positionID, now) {
			applied++
			changed = true
		}
	}
	changed = bc.notePairFundingGaps(rt, m, got, hInv, nowMs) || changed
	return applied, changed
}

func (bc *basketCoordinator) stagePairFunding(rt *basketRuntime, f *pairFundingRuntime, m *pairFundingMember, s *StrategyState, ev pairFundingEventValue, raw []byte, amount float64, positionID string, now time.Time) bool {
	key := pairFundingKeyOf(m.memberID, ev.EventID, ev.RuleVersion)
	venueKey := pairFundingVenueEventKey{Key: ev.Key, EventID: ev.EventID}
	if owner, ok := f.appliedVenueRules[venueKey]; ok {
		if owner != ev.RuleVersion {
			m.integrity = append(m.integrity, fmt.Sprintf("funding event %s for %s is already owned by rule %s; rule %s cannot apply a second charge", ev.EventID, ev.Key, owner, ev.RuleVersion))
		}
		return false
	}
	if owner, ok := f.pendingVenueRules[venueKey]; ok {
		if owner != ev.RuleVersion {
			m.integrity = append(m.integrity, fmt.Sprintf("funding event %s for %s is already staged by rule %s; rule %s cannot apply a second charge", ev.EventID, ev.Key, owner, ev.RuleVersion))
		}
		return false
	}
	delete(f.incomplete, key)
	rt.Pair.Progress.clearObligation(pairObligationFundingIncomplete, m.memberID, ev.EventID+"|"+ev.RuleVersion)
	prevDefer := s.deferTradePersist
	s.deferTradePersist = true
	s.Cash += amount
	withStrategyBookClock(s, time.UnixMilli(ev.EventMs).UTC(), func() {
		RecordTrade(s, Trade{
			Timestamp:       time.UnixMilli(ev.EventMs).UTC(),
			StrategyID:      s.ID,
			Symbol:          m.coin,
			PositionID:      positionID,
			Side:            "funding",
			TradeType:       TradeTypeFunding,
			Details:         fmt.Sprintf("Pair funding %s on %s %s: -(qty %s x %s price %s x rate %s) = %s %s x %s USD/%s = $%s (rule %s)", ev.EventID, m.key.Provider, m.key.Instrument, ev.SignedBaseQty, pairFundingPriceKindOf(pairFundingSourcePolicy(f.source), ev.Key, ev.RuleVersion), ev.NativeMark, ev.Rate, ev.NativeAmount, ev.Currency, ev.Conversion.RateUSD, ev.Currency, ev.USDAmount, ev.RuleVersion),
			ExchangeOrderID: pairFundingOrderIDPrefix + ev.RuleVersion + ":" + ev.EventID,
			RealizedPnL:     amount,
			PnLGross:        true,
		})
	})
	s.deferTradePersist = prevDefer
	f.pending[key] = &pairFundingApplication{MemberID: m.memberID, Event: ev, JSON: raw, AtMs: now.UnixMilli()}
	f.pendingVenueRules[venueKey] = ev.RuleVersion
	if ev.EventMs > m.maxAppliedMs {
		m.maxAppliedMs = ev.EventMs
	}
	bc.pairLogf(rt, "member %s funding %s (%s) applied once: $%s", m.memberID, ev.EventID, ev.RuleVersion, ev.USDAmount)
	return true
}

func (bc *basketCoordinator) notePairFundingGaps(rt *basketRuntime, m *pairFundingMember, got pairFundingCollected, hInv, nowMs int64) bool {
	h := nowMs
	if hInv < h {
		h = hInv
	}
	exposure := m.exposure(h)
	_, gaps := pairFundingCoverageGaps(got.Coverage, exposure)
	p := &rt.Pair.Progress
	want := map[string]string{}
	for _, g := range gaps {
		want[fmt.Sprintf("gap:%d", g.FromMs)] = fmt.Sprintf("held exposure (%d, %d] has no proven funding coverage (%s); a settlement inside it is unknown, never booked as zero, and settles once when coverage arrives", g.FromMs, g.ThroughMs, g.Reason)
	}
	changed := false
	var keep []pairObligation
	for _, o := range p.Obligations {
		if o.Kind == pairObligationFundingGap && o.MemberID == m.memberID {
			if _, ok := want[o.ActionID]; !ok {
				changed = true
				continue
			}
		}
		keep = append(keep, o)
	}
	p.Obligations = keep
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if p.addObligation(pairObligation{Kind: pairObligationFundingGap, MemberID: m.memberID, ActionID: id, Detail: want[id], AtMs: nowMs}) {
			changed = true
			bc.pairAlert(rt, "funding-gap|"+m.memberID+"|"+id, fmt.Sprintf("pair %s member %s: %s", rt.Contract.BasketID, m.memberID, want[id]), false)
		}
	}
	return changed
}

func pairFundingPriceKindOf(policy *pairFundingPolicyV1, key pairEvidenceKey, ruleVersion string) string {
	if rule, ok := pairFundingSelectedRule(policy, key); ok && rule.RuleVersion == ruleVersion {
		return rule.PriceKind
	}
	return "unknown"
}
