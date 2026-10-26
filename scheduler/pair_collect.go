package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

const pairObligationPolicyTimeout = "policy_infrastructure_timeout"

type pairIncreaseBinding struct {
	Conversions    []pairConversionEvidence
	CapabilityHash string
	EvidenceHash   string
	ReceiptMs      int64
}

func (bc *basketCoordinator) pairNow(in basketCycleInputs) time.Time {
	now := in.Now
	if bc.pairClock != nil {
		if fresh := bc.pairClock(); fresh.After(now) {
			now = fresh
		}
	}
	return now
}

func (bc *basketCoordinator) notePairQuoteHold(rt *basketRuntime, reason string) {
	if rt.pairQuoteNote == reason {
		return
	}
	rt.pairQuoteNote = reason
	bc.pairLogf(rt, "quote collection holds: %s; no cadence slot is collected, and queries, settlement and protection continue", reason)
}

func (bc *basketCoordinator) holdPairSlot(rt *basketRuntime, rb *basketRebalance, reason string, now time.Time) {
	if rb == nil || rt.Rebalances[rb.DeadlineMs] != rb || basketRebalanceTerminal(rb.Status) {
		return
	}
	rb.Status = basketRebalanceHeld
	rb.HoldReason = reason
	rb.Outcome.Result = "held"
	rb.UpdatedAtMs = now.UnixMilli()
	rt.touchPairProgress()
	bc.pairLogf(rt, "slot %s held: %s", formatClosedBarMs(rb.DeadlineMs), reason)
	if !bc.persistPair(rt) {
		rt.needsDurable = true
	}
}

func pairCollectLifecycle(lifecycle string) bool {
	return lifecycle == "" || lifecycle == pairLifecycleFlat || lifecycle == pairLifecycleOpening || lifecycle == pairLifecycleMatched
}

func pairNeedsSeal(members []pairContractMember) bool {
	for _, m := range members {
		if m.Source == pairSourceSealedFeed {
			return true
		}
	}
	return false
}

func pairSealTimeMs(s *marketSnapshot) int64 {
	if !s.Deadline.IsZero() {
		return s.Deadline.UnixMilli()
	}
	if !s.SealedAt.IsZero() {
		return s.SealedAt.UnixMilli()
	}
	return 0
}

func pairReadSeal(rt *basketRuntime, atMs int64, at time.Time, snapFor func(rt *basketRuntime, at time.Time) *marketSnapshot, genFor func() uint64) (*marketSnapshot, string) {
	if snapFor == nil {
		return nil, "no sealed market snapshot: the host supplies no PairSnapshotFor callback, so the sealed read holds and nothing replaces it"
	}
	if genFor == nil {
		return nil, "no sealed market snapshot: the host supplies no PairGeneration callback, so the seal generation cannot be checked and the sealed read holds"
	}
	snap := snapFor(rt, at)
	if snap == nil {
		return nil, fmt.Sprintf("no sealed market snapshot was returned for the read time %d; a sealed member holds and no public read or cycle snapshot replaces it", atMs)
	}
	if snap.EvaluationID == "" || !pairPrintableASCII(snap.EvaluationID, pairSnapshotIDMaxLen) {
		return nil, fmt.Sprintf("the sealed snapshot for the read time %d has no printable identity of at most %d bytes", atMs, pairSnapshotIDMaxLen)
	}
	if snap.ConfigGeneration == 0 {
		return nil, fmt.Sprintf("the sealed snapshot %q has no configuration generation", snap.EvaluationID)
	}
	sealMs := pairSealTimeMs(snap)
	if sealMs <= 0 {
		return nil, fmt.Sprintf("the sealed snapshot %q has no seal time", snap.EvaluationID)
	}
	if sealMs > atMs {
		return nil, fmt.Sprintf("the sealed snapshot %q was sealed at %d, after the read time %d", snap.EvaluationID, sealMs, atMs)
	}
	if cur := genFor(); cur != snap.ConfigGeneration {
		return nil, fmt.Sprintf("the sealed snapshot %q generation %d is not the host's current feed generation %d", snap.EvaluationID, snap.ConfigGeneration, cur)
	}
	return snap, ""
}

func (bc *basketCoordinator) notePairStep(rt *basketRuntime, in basketCycleInputs) {
	if ms := in.Now.UnixMilli(); ms > rt.pairLastStepMs {
		rt.pairLastStepMs = ms
	}
}

func (bc *basketCoordinator) collectPairSlot(rt *basketRuntime, in basketCycleInputs) basketCycleInputs {
	if in.SaveBlocked || rt.needsDurable || !pairCollectLifecycle(rt.Pair.Progress.Lifecycle) {
		return in
	}
	start := bc.pairNow(in)
	startMs := start.UnixMilli()
	limits := rt.Pair.Limits
	if limits.DecisionCadenceMs <= 0 || startMs <= 0 {
		return in
	}
	slotMs := startMs - startMs%limits.DecisionCadenceMs
	if slotMs <= 0 {
		return in
	}
	if _, taken := rt.Rebalances[slotMs]; taken {
		return in
	}
	adm := rt.Pair.admission
	src := adm.quoteSource()
	if err := pairQuoteSourceErrors(rt.Contract, src); err != nil {
		bc.notePairQuoteHold(rt, err.Error())
		return in
	}
	evidence := adm.evidenceSource()
	if evidence == nil {
		bc.notePairQuoteHold(rt, "the pair has no admitted evidence source")
		return in
	}
	deadlineMs, ok := pairAddMs(startMs, limits.CollectionTimeoutMs)
	if !ok {
		bc.notePairQuoteHold(rt, "the collection deadline overflows")
		return in
	}
	rt.pairQuoteNote = ""
	rb, err := bc.reservePairSlot(rt, slotMs, start)
	if errors.Is(err, errBasketDeadlineTaken) {
		return in
	}
	if err != nil {
		bc.pairLogf(rt, "slot %s could not be reserved: %v; nothing is collected this step", formatClosedBarMs(slotMs), err)
		return in
	}
	members := rt.Contract.pairMembersSorted()
	contractFP := rt.Contract.Fingerprint()
	need := rt.Contract.Pair.BookNeed
	sourceContracts := pairSourceContractMap(members)
	needSeal := pairNeedsSeal(members)
	snapFor, genFor := in.PairSnapshotFor, in.PairGeneration
	req := pairCollectRequest{Partition: rt.Partition, PairID: rt.Contract.BasketID, ContractFingerprint: contractFP, SlotMs: slotMs, StartMs: startMs,
		DeadlineMs: deadlineMs, Members: members, BookNeed: need, SourceContracts: sourceContracts}
	before := bc.pairSnapshot(rt)
	parent, clock := bc.parent, bc.pairClock
	if clock == nil {
		clock = tradingNow
	}
	budget := time.Duration(deadlineMs-startMs) * time.Millisecond
	var (
		obs        []pairObservation
		caps       []pairCapabilityEvidence
		accts      []pairAccountEvidence
		convs      []pairConversionEvidence
		collectErr error
		cutoffMs   int64
		ctxErr     error
		elapsed    time.Duration
		snapshotID string
		generation uint64
	)
	bc.unlocked(func() {
		wallStart := time.Now()
		ctx, cancel := context.WithTimeout(parent, budget)
		defer cancel()
		if needSeal {
			snap, why := pairReadSeal(rt, startMs, start, snapFor, genFor)
			if why != "" {
				collectErr = errors.New(why)
			} else {
				snapshotID, generation = snap.EvaluationID, snap.ConfigGeneration
				req.Snapshot, req.SnapshotID, req.Generation = snap, snapshotID, generation
			}
		}
		if collectErr == nil {
			obs, collectErr = src.Collect(ctx, req)
		}
		if collectErr == nil {
			cutoffMs = clock().UnixMilli()
			if cutoffMs < startMs {
				cutoffMs = startMs
			}
			for _, m := range members {
				c, err := evidence.Capability(ctx, m, cutoffMs)
				if err != nil {
					collectErr = fmt.Errorf("member %s capability evidence: %w", m.MemberID, err)
					break
				}
				a, err := evidence.Account(ctx, m, cutoffMs)
				if err != nil {
					collectErr = fmt.Errorf("member %s account evidence: %w", m.MemberID, err)
					break
				}
				cs, err := evidence.Conversions(ctx, m, cutoffMs)
				if err != nil {
					collectErr = fmt.Errorf("member %s conversion evidence: %w", m.MemberID, err)
					break
				}
				caps, accts, convs = append(caps, c), append(accts, a), append(convs, cs...)
			}
			if final := clock().UnixMilli(); collectErr == nil && final > cutoffMs {
				cutoffMs = final
			}
		}
		if collectErr == nil && needSeal {
			if cur := genFor(); cur != generation {
				collectErr = fmt.Errorf("the host feed generation moved from %d to %d during the read; the captured seal no longer belongs to the current feed configuration", generation, cur)
			}
		}
		ctxErr = ctx.Err()
		elapsed = time.Since(wallStart)
	})
	now := bc.pairNow(in)
	if !before.same(bc.pairSnapshot(rt)) || rt.Rebalances[slotMs] != rb || rb.Status != basketRebalanceReserved {
		bc.holdPairSlot(rt, rb, "the pair records changed while the quotes were collected; a cadence slot is never collected twice", now)
		return bc.refreshPairInputs(rt, in)
	}
	reason := ""
	switch {
	case collectErr != nil:
		reason = "quote or evidence collection failed: " + collectErr.Error()
	case ctxErr != nil:
		reason = fmt.Sprintf("the collection budget ended before the reads finished (%v)", ctxErr)
	case elapsed >= budget:
		reason = fmt.Sprintf("the collection took %s, at or past its %s budget", elapsed.Round(time.Millisecond), budget)
	case !pairBeforeDeadline(cutoffMs, deadlineMs):
		reason = fmt.Sprintf("the collection cutoff %d is at or after its logical deadline %d", cutoffMs, deadlineMs)
	case rt.Contract.Fingerprint() != contractFP:
		reason = "the pair contract changed while the quotes were collected"
	}
	if reason == "" {
		if err := bc.recheckPairSlot(rt, slotMs, contractFP); err != nil {
			reason = "the reserved slot failed its recheck: " + err.Error()
		}
	}
	if reason != "" {
		bc.holdPairSlot(rt, rb, reason, now)
		return bc.refreshPairInputs(rt, in)
	}
	env, why := buildPairEnvelope(pairEnvelopeInputs{Partition: rt.Partition, PairID: rt.Contract.BasketID, ContractFingerprint: contractFP, SlotMs: slotMs,
		StartMs: startMs, DeadlineMs: deadlineMs, CutoffMs: cutoffMs, Members: members, BookNeed: need, SourceContracts: sourceContracts,
		Observations: obs, Capabilities: caps, Conversions: convs, Accounts: accts, SnapshotID: snapshotID, Generation: generation, Limits: limits})
	if why != "" {
		bc.holdPairSlot(rt, rb, "the collected quotes do not form a valid envelope: "+why, now)
		return bc.refreshPairInputs(rt, in)
	}
	if h := bc.pairReplay; h != nil && h.collected != nil {
		if err := h.collected(rt, pairCollectedSlot{SlotMs: slotMs, StartMs: startMs, DeadlineMs: deadlineMs, Envelope: env}); err != nil {
			bc.holdPairSlot(rt, rb, "the replay host could not record the checked collection before the freeze: "+err.Error(), now)
			return bc.refreshPairInputs(rt, in)
		}
	}
	if err := bc.freezePairEnvelope(rt, slotMs, env, now); err != nil {
		bc.holdPairSlot(rt, rb, "the envelope could not be frozen in the pair partition file: "+err.Error(), now)
		return bc.refreshPairInputs(rt, in)
	}
	if h := bc.pairReplay; h != nil && h.frozen != nil {
		h.frozen(rt, slotMs)
	}
	bc.pairLogf(rt, "slot %s: quote envelope frozen (cutoff %d, input %s)", formatClosedBarMs(slotMs), cutoffMs, shortFingerprint(env.InputFP))
	return bc.refreshPairInputs(rt, in)
}

func (bc *basketCoordinator) refreshPairInputs(rt *basketRuntime, in basketCycleInputs) basketCycleInputs {
	in.Now = bc.pairNow(in)
	rt.clock = in.Now
	return in
}

func (bc *basketCoordinator) readPairReference(ctx context.Context, rt *basketRuntime, in basketCycleInputs, member pairContractMember, side string) (pairObservation, error) {
	if !bc.active() {
		return pairObservation{}, fmt.Errorf("a pair reference needs an active coordinator")
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.readPairReferenceLocked(ctx, rt, in, member, side)
}

func (bc *basketCoordinator) readPairReferenceLocked(ctx context.Context, rt *basketRuntime, in basketCycleInputs, member pairContractMember, side string) (pairObservation, error) {
	if ctx == nil {
		return pairObservation{}, fmt.Errorf("a pair reference needs a caller context")
	}
	if !rt.isPair() || bc.runtimes[rt.Contract.BasketID] != rt {
		return pairObservation{}, fmt.Errorf("a pair reference needs a pair runtime admitted by this coordinator")
	}
	m, ok := rt.Contract.pairMember(member.MemberID)
	if !ok || m.Provider != member.Provider || m.AccountKey != member.AccountKey || m.Instrument != member.Instrument || m.Source != member.Source {
		return pairObservation{}, fmt.Errorf("pair %s has no admitted member %s with provider %s, account %s, instrument %s and source %s", rt.Contract.BasketID, member.MemberID, member.Provider, member.AccountKey, member.Instrument, member.Source)
	}
	if side != "buy" && side != "sell" {
		return pairObservation{}, fmt.Errorf("a pair reference needs the order side buy or sell, got %q", side)
	}
	src := rt.Pair.admission.quoteSource()
	if err := pairQuoteSourceErrors(rt.Contract, src); err != nil {
		return pairObservation{}, err
	}
	sourceContract := pairSourceContractFor(m.Provider, m.Source)
	if sourceContract == "" {
		return pairObservation{}, fmt.Errorf("member %s source %s has no source contract", m.MemberID, m.Source)
	}
	limits := rt.Pair.Limits
	start := bc.pairNow(in)
	startMs := start.UnixMilli()
	deadlineMs, ok := pairAddMs(startMs, limits.CollectionTimeoutMs)
	if !ok || startMs <= 0 || limits.CollectionTimeoutMs <= 0 {
		return pairObservation{}, fmt.Errorf("the reference deadline for %s overflows or has no collection timeout", m.MemberID)
	}
	budget := time.Duration(limits.CollectionTimeoutMs) * time.Millisecond
	contractFP := rt.Contract.Fingerprint()
	before := bc.pairSnapshot(rt)
	needSeal := m.Source == pairSourceSealedFeed
	snapFor, genFor := in.PairSnapshotFor, in.PairGeneration
	req := pairReferenceRequest{Partition: rt.Partition, PairID: rt.Contract.BasketID, ContractFingerprint: contractFP, Member: m, Side: side,
		BookNeed: rt.Contract.Pair.BookNeed, SourceContract: sourceContract, StartMs: startMs, DeadlineMs: deadlineMs}
	var (
		obs        pairObservation
		readErr    error
		ctxErr     error
		elapsed    time.Duration
		snapshotID string
		generation uint64
	)
	bc.unlocked(func() {
		wallStart := time.Now()
		rctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		if needSeal {
			snap, why := pairReadSeal(rt, startMs, start, snapFor, genFor)
			if why != "" {
				readErr = errors.New(why)
			} else {
				snapshotID, generation = snap.EvaluationID, snap.ConfigGeneration
				req.Snapshot, req.SnapshotID, req.Generation = snap, snapshotID, generation
			}
		}
		if readErr == nil {
			obs, readErr = src.Reference(rctx, req)
		}
		if readErr == nil && needSeal {
			if cur := genFor(); cur != generation {
				readErr = fmt.Errorf("the host feed generation moved from %d to %d during the reference read", generation, cur)
			}
		}
		ctxErr = rctx.Err()
		elapsed = time.Since(wallStart)
	})
	now := bc.pairNow(in)
	nowMs := now.UnixMilli()
	switch {
	case !before.same(bc.pairSnapshot(rt)):
		return pairObservation{}, fmt.Errorf("the pair records changed while the %s reference for %s was read; no substitute observation is used", side, m.MemberID)
	case rt.Contract.Fingerprint() != contractFP:
		return pairObservation{}, fmt.Errorf("the pair contract changed while the %s reference for %s was read", side, m.MemberID)
	case readErr != nil:
		return pairObservation{}, fmt.Errorf("the %s reference for %s failed: %w", side, m.MemberID, readErr)
	case ctxErr != nil:
		return pairObservation{}, fmt.Errorf("the %s reference for %s ended with its context (%w)", side, m.MemberID, ctxErr)
	case elapsed >= budget:
		return pairObservation{}, fmt.Errorf("the %s reference for %s took %s, at or past its %s budget", side, m.MemberID, elapsed.Round(time.Millisecond), budget)
	case !pairBeforeDeadline(nowMs, deadlineMs):
		return pairObservation{}, fmt.Errorf("the %s reference for %s completed at %d, at or after its logical deadline %d", side, m.MemberID, nowMs, deadlineMs)
	}
	if needSeal && (obs.SnapshotID != snapshotID || obs.Generation != generation) {
		return pairObservation{}, fmt.Errorf("the %s reference for %s carries snapshot %q generation %d, not the captured snapshot %q generation %d", side, m.MemberID, obs.SnapshotID, obs.Generation, snapshotID, generation)
	}
	if !needSeal && (obs.SnapshotID != "" || obs.Generation != 0) {
		return pairObservation{}, fmt.Errorf("the %s reference for %s carries a snapshot identity its %s source cannot have", side, m.MemberID, m.Source)
	}
	if err := pairCheckReference(rt, m, &obs, side, nowMs); err != nil {
		return pairObservation{}, fmt.Errorf("the %s reference for %s fails its check at %d: %w", side, m.MemberID, nowMs, err)
	}
	return pairCloneObservation(obs), nil
}

type pairPolicyWindow struct {
	nextMs     int64
	cutoffEnd  int64
	receiptMs  int64
	receiptEnd int64
}

func pairPolicyWindowFor(rt *basketRuntime, rb *basketRebalance, env pairQuoteEnvelope) (pairPolicyWindow, string) {
	l := rt.Pair.Limits
	var w pairPolicyWindow
	var ok bool
	if w.nextMs, ok = pairAddMs(rb.DeadlineMs, l.DecisionCadenceMs); !ok || l.DecisionCadenceMs <= 0 {
		return w, "the next cadence slot time overflows"
	}
	if w.cutoffEnd, ok = pairAddMs(env.CutoffMs, l.QuoteAgeMs); !ok || env.CutoffMs <= 0 || l.QuoteAgeMs < 0 {
		return w, "the envelope cutoff plus quote_age_ms overflows"
	}
	w.receiptMs = math.MaxInt64
	for _, m := range env.Members {
		if r := m.Observation.FirstReceiptMs; r < w.receiptMs {
			w.receiptMs = r
		}
	}
	if len(env.Members) == 0 || w.receiptMs <= 0 || w.receiptMs > env.CutoffMs {
		return w, "the envelope has no verified member receipt at or before its cutoff"
	}
	if w.receiptEnd, ok = pairAddMs(w.receiptMs, l.QuoteAgeMs); !ok {
		return w, "the earliest member receipt plus quote_age_ms overflows"
	}
	return w, ""
}

func (w pairPolicyWindow) endMs() int64 {
	end := w.nextMs
	if w.cutoffEnd < end {
		end = w.cutoffEnd
	}
	if w.receiptEnd < end {
		end = w.receiptEnd
	}
	return end
}

func (w pairPolicyWindow) eligible(nowMs int64, quoteAgeMs int64) string {
	switch {
	case !pairBeforeDeadline(nowMs, w.nextMs):
		return fmt.Sprintf("the time %d is at or after the next cadence slot %d", nowMs, w.nextMs)
	case !pairAgeWithin(nowMs, w.receiptMs, quoteAgeMs):
		return fmt.Sprintf("the earliest member receipt %d is outside quote_age_ms %d of the time %d", w.receiptMs, quoteAgeMs, nowMs)
	case nowMs > w.cutoffEnd:
		return fmt.Sprintf("the envelope cutoff %d is older than quote_age_ms %d at the time %d", w.cutoffEnd-quoteAgeMs, quoteAgeMs, nowMs)
	}
	return ""
}

func pairMsUntil(ms int64, now time.Time) time.Duration {
	return time.UnixMilli(ms).Sub(now)
}

func (bc *basketCoordinator) pairPolicyBudget(rt *basketRuntime, rb *basketRebalance, now time.Time) (time.Duration, bool) {
	env, why := verifyPairEnvelope(rt, rb)
	if why != "" {
		return 0, false
	}
	d, why := bc.pairPolicyBudgetFor(rt, rb, env, now)
	return d, why == ""
}

func (bc *basketCoordinator) pairPolicyBudgetFor(rt *basketRuntime, rb *basketRebalance, env pairQuoteEnvelope, now time.Time) (time.Duration, string) {
	w, why := pairPolicyWindowFor(rt, rb, env)
	if why != "" {
		return 0, why
	}
	if tradingClockBound() {
		if why := w.eligible(now.UnixMilli(), rt.Pair.Limits.QuoteAgeMs); why != "" {
			return 0, "the simulated time is outside the useful decision window: " + why
		}
		return pairPolicyMaxTimeout, ""
	}
	budget := pairPolicyMaxTimeout
	for _, b := range []struct {
		ms   int64
		what string
	}{{w.nextMs, "the next cadence slot"}, {w.cutoffEnd, "the envelope cutoff plus quote_age_ms"}, {w.receiptEnd, "the earliest member receipt plus quote_age_ms"}} {
		d := pairMsUntil(b.ms, now)
		if d <= 0 {
			return 0, fmt.Sprintf("no policy time is left before %s (%d) at %d", b.what, b.ms, now.UnixMilli())
		}
		if d < budget {
			budget = d
		}
	}
	return budget, ""
}

func (p *pairProgress) addObligationOnce(o pairObligation) bool {
	if p.obligation(o.Kind, o.MemberID, o.ActionID) != nil {
		return false
	}
	p.Obligations = append(p.Obligations, o)
	return true
}

func pairRecoveryObligation(o pairObligation) bool {
	return o.Kind != pairObligationPolicyTimeout
}

func pairIncompleteEvidence(rt *basketRuntime) (pairObligation, bool) {
	if !rt.isPair() {
		return pairObligation{}, false
	}
	p := &rt.Pair.Progress
	if o := p.obligation(pairObligationPolicyTimeout, "", ""); o != nil {
		return *o, true
	}
	var found []pairObligation
	for _, o := range p.Obligations {
		if pairNativeObligationKind(o.Kind) {
			found = append(found, o)
			continue
		}
		if a := rt.Actions[o.ActionID]; a != nil && a.pairNative() && (pairSettlementObligationKind(o.Kind) || o.Kind == pairObligationFeeIncomplete) {
			found = append(found, o)
		}
	}
	for _, r := range p.NativeReceipts {
		found = append(found, pairObligation{Kind: pairObligationNativeReceipt, MemberID: r.MemberID, ActionID: r.ID, Detail: "a confirmed native fill receipt is not attached to an accounting action", AtMs: r.CreatedAtMs})
	}
	for _, c := range p.PaperCloses {
		if c.Status == pairPaperCloseStaged {
			found = append(found, pairObligation{Kind: pairObligationEmergencyClose, MemberID: c.MemberID, Detail: "a staged emergency close is not committed", AtMs: c.CloseAtMs})
		}
	}
	for _, h := range p.NativeFundingHolds {
		found = append(found, pairObligation{Kind: pairObligationNativeFundingUncertain, MemberID: h.MemberID, ActionID: h.ResolutionID, Detail: h.Detail, AtMs: h.AtMs, FromMs: h.FromMs})
	}
	if len(found) == 0 {
		return pairObligation{}, false
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Kind != found[j].Kind {
			return found[i].Kind < found[j].Kind
		}
		if found[i].MemberID != found[j].MemberID {
			return found[i].MemberID < found[j].MemberID
		}
		return found[i].ActionID < found[j].ActionID
	})
	return found[0], true
}

func (bc *basketCoordinator) notePairPolicyTimeout(rt *basketRuntime, rb *basketRebalance, ctxErr error, budget time.Duration, now time.Time) {
	cause := "deadline"
	if errors.Is(ctxErr, context.Canceled) {
		cause = "cancelled"
	}
	detail := fmt.Sprintf("slot %s: the policy run ended by its context (%s, %v) within a %s budget; the research run result is incomplete", formatClosedBarMs(rb.DeadlineMs), cause, ctxErr, budget)
	if rt.Pair.Progress.addObligationOnce(pairObligation{Kind: pairObligationPolicyTimeout, Detail: detail, AtMs: now.UnixMilli()}) {
		bc.pairAlert(rt, pairObligationPolicyTimeout, fmt.Sprintf("pair %s %s; the run is marked incomplete and keeps the first timeout", rt.Contract.BasketID, detail), false)
	}
	rt.touchPairProgress()
}

func (bc *basketCoordinator) holdPairPolicyTimeout(rt *basketRuntime, rb *basketRebalance, ctxErr error, budget time.Duration, now time.Time) bool {
	bc.notePairPolicyTimeout(rt, rb, ctxErr, budget, now)
	reason := fmt.Sprintf("the policy run ended by its context (%v) within its %s budget; an infrastructure timeout holds the slot and marks the research run incomplete", ctxErr, budget)
	t := pairTargets{Version: pairTargetsVersion, Kind: pairPolicyStatusHold, SlotMs: rb.DeadlineMs, InputFingerprint: rb.InputFP, CoordinatorReason: reason}
	raw := t.JSON()
	if raw == "" {
		bc.pairLogf(rt, "slot %s timeout hold could not be encoded; a later step decides it again", formatClosedBarMs(rb.DeadlineMs))
		if !bc.persistPair(rt) {
			rt.needsDurable = true
		}
		return false
	}
	rb.TargetsJSON, rb.Status, rb.HoldReason = raw, basketRebalanceHeld, reason
	rb.Outcome.Result = "held"
	rb.UpdatedAtMs = now.UnixMilli()
	rt.touchPairProgress()
	bc.pairLogf(rt, "slot %s held: %s", formatClosedBarMs(rb.DeadlineMs), reason)
	if !bc.persistPair(rt) {
		rt.needsDurable = true
		bc.pairLogf(rt, "slot %s: the timeout hold and its incomplete marker are kept in memory and saved together before any elective work", formatClosedBarMs(rb.DeadlineMs))
	}
	return true
}

func pairManagementMaxAge(cfg *Config) time.Duration {
	if cfg == nil || cfg.IntervalSeconds <= 0 {
		return 0
	}
	return 2 * time.Duration(cfg.IntervalSeconds) * time.Second
}

func (bc *basketCoordinator) pairWakeDelay(now time.Time) time.Duration {
	if !bc.active() {
		return -1
	}
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.pairWakeDelayLocked(now)
}

func (bc *basketCoordinator) pairWakeDelayLocked(now time.Time) time.Duration {
	best := time.Duration(-1)
	nowMs := now.UnixMilli()
	for _, id := range bc.sortedPairIDs() {
		rt := bc.runtimes[id]
		for _, b := range bc.pairWakeBoundaries(rt, nowMs) {
			var d time.Duration
			switch {
			case b > nowMs:
				d = pairMsUntil(b, now)
				if d < 0 {
					d = 0
				}
			case b > rt.pairLastStepMs:
				d = 0
			default:
				continue
			}
			if best < 0 || d < best {
				best = d
			}
		}
	}
	return best
}

func (bc *basketCoordinator) pairWakeBoundaries(rt *basketRuntime, nowMs int64) []int64 {
	var out []int64
	add := func(ms int64, ok bool) {
		if ok && ms > 0 && ms < math.MaxInt64 {
			out = append(out, ms)
		}
	}
	if !rt.isPair() {
		return out
	}
	l, p := rt.Pair.Limits, rt.Pair.Progress
	if c := l.DecisionCadenceMs; c > 0 && nowMs > 0 {
		cur := nowMs - nowMs%c
		add(cur, true)
		add(pairAddMs(cur, c))
	}
	for _, d := range rt.sortedDeadlines() {
		rb := rt.Rebalances[d]
		if rb.Status != basketRebalanceFrozen || rb.TargetsJSON != "" || rb.EvidenceJSON == "" {
			continue
		}
		env, why := verifyPairEnvelope(rt, rb)
		if why != "" {
			continue
		}
		if w, why := pairPolicyWindowFor(rt, rb, env); why == "" {
			add(w.endMs(), true)
		}
	}
	for _, at := range p.NativeAttempts {
		if at.RetryAtMs > 0 && at.OrdersComplete && at.Ambiguous == "" {
			add(at.RetryAtMs, true)
		}
	}
	for _, r := range p.NativeRequests {
		if !rt.Pair.nativeSeen[r.ID] {
			add(nowMs, true)
		}
	}
	grace := entryRecoveryGrace.Milliseconds()
	for _, a := range rt.pairUnresolvedActions() {
		if a.AcceptDeadlineMs <= 0 {
			continue
		}
		end, ok := pairAddMs(a.AcceptDeadlineMs, grace)
		if a.recoveryNote().NativeBooked {
			add(end, ok)
			continue
		}
		add(a.AcceptDeadlineMs, true)
		if ok {
			add(pairAddMs(end, 1))
		}
	}
	if p.Lifecycle == pairLifecycleRecovery || p.Lifecycle == pairLifecycleOperatorHeld {
		for _, id := range rt.memberIDs() {
			r, ok := p.Retries[id]
			if !ok || bc.pairExposure(rt, id) == 0 || rt.pairPendingProtective(id) != nil {
				continue
			}
			add(r.RetryAtMs, true)
		}
	}
	for _, o := range p.Obligations {
		if o.Kind != pairObligationReductionReference || o.MemberID == "" || o.RetryAtMs <= 0 || bc.pairExposure(rt, o.MemberID) == 0 {
			continue
		}
		at := o.RetryAtMs
		if next, ok := pairAddMs(rt.pairLastStepMs, l.RetryInitialMs); ok && next > at {
			at = next
		}
		add(at, true)
	}
	if (p.Lifecycle == pairLifecycleOpening || p.Lifecycle == pairLifecycleMatched) && p.FirstFillMs > 0 {
		add(pairAddMs(p.FirstFillMs, l.MaxHoldAgeMs))
	}
	for _, id := range rt.memberIDs() {
		if vv := bc.pairVirtualVenueFor(id); vv != nil {
			add(vv.NextVisibleMs())
		}
	}
	if p.Lifecycle == pairLifecycleRecovery && p.RecoveryStartMs > 0 {
		add(pairAddMs(p.RecoveryStartMs, l.RecoveryAgeMs))
	}
	return out
}

func (bc *basketCoordinator) pairBindIncrease(rt *basketRuntime, memberID string, ev pairQuoteEvent, now time.Time) (*pairIncreaseBinding, string) {
	rb := rt.Rebalances[ev.SlotMs]
	if rb == nil {
		return nil, fmt.Sprintf("the selected quote event %s has no saved slot", ev.ID())
	}
	got, ok := pairQuoteEventFromSlot(rb)
	if !ok || got.ID() != ev.ID() {
		return nil, fmt.Sprintf("the selected quote event %s does not match its saved slot", ev.ID())
	}
	env, why := verifyPairEnvelope(rt, rb)
	if why != "" {
		return nil, "the selected quote event's envelope fails verification: " + why
	}
	em, ok := env.member(memberID)
	if !ok {
		return nil, fmt.Sprintf("the selected envelope has no member %s", memberID)
	}
	if !pairAgeWithin(now.UnixMilli(), em.Observation.FirstReceiptMs, rt.Pair.Limits.QuoteAgeMs) {
		return nil, fmt.Sprintf("the selected quote for %s was received at %d, outside quote_age_ms %d of the dispatch time %d", memberID, em.Observation.FirstReceiptMs, rt.Pair.Limits.QuoteAgeMs, now.UnixMilli())
	}
	return &pairIncreaseBinding{Conversions: append([]pairConversionEvidence{}, em.Conversions...), CapabilityHash: em.Capability.EvidenceHash, EvidenceHash: rb.InputFP, ReceiptMs: em.Observation.FirstReceiptMs}, ""
}
