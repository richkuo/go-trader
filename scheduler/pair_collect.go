package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type pairIncreaseBinding struct {
	Conversions    []pairConversionEvidence
	CapabilityHash string
	EvidenceHash   string
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
	snap := in.Snapshot
	if in.SnapshotFor != nil {
		if s := in.SnapshotFor(rt, start); s != nil {
			snap = s
		}
	}
	snapshotID, generation := "", uint64(0)
	for _, m := range members {
		if m.Source == pairSourceSealedFeed && snap != nil {
			snapshotID, generation = snap.EvaluationID, snap.ConfigGeneration
		}
	}
	req := pairCollectRequest{Partition: rt.Partition, PairID: rt.Contract.BasketID, ContractFingerprint: contractFP, SlotMs: slotMs, StartMs: startMs,
		DeadlineMs: deadlineMs, Members: members, BookNeed: need, SourceContracts: sourceContracts, Snapshot: snap, SnapshotID: snapshotID, Generation: generation}
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
	)
	bc.unlocked(func() {
		wallStart := time.Now()
		ctx, cancel := context.WithTimeout(parent, budget)
		defer cancel()
		obs, collectErr = src.Collect(ctx, req)
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
	if reason == "" && snapshotID != "" {
		cur := in.Snapshot
		if in.SnapshotFor != nil {
			if s := in.SnapshotFor(rt, start); s != nil {
				cur = s
			}
		}
		if cur == nil || cur.EvaluationID != snapshotID || cur.ConfigGeneration != generation {
			reason = "the captured sealed snapshot is no longer the host's snapshot for this read"
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
	if err := bc.freezePairEnvelope(rt, slotMs, env, now); err != nil {
		bc.holdPairSlot(rt, rb, "the envelope could not be frozen in the pair partition file: "+err.Error(), now)
		return bc.refreshPairInputs(rt, in)
	}
	bc.pairLogf(rt, "slot %s: quote envelope frozen (cutoff %d, input %s)", formatClosedBarMs(slotMs), cutoffMs, shortFingerprint(env.InputFP))
	return bc.refreshPairInputs(rt, in)
}

func (bc *basketCoordinator) refreshPairInputs(rt *basketRuntime, in basketCycleInputs) basketCycleInputs {
	in.Now = bc.pairNow(in)
	rt.clock = in.Now
	return in
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
	return &pairIncreaseBinding{Conversions: append([]pairConversionEvidence{}, em.Conversions...), CapabilityHash: em.Capability.EvidenceHash, EvidenceHash: rb.InputFP}, ""
}
