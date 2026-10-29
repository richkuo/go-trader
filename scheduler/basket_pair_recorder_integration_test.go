package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	pairRecTestRun       = "rec-run-1"
	pairRecTestPartition = "paper"
	pairRecTestContract  = "0000000000000000000000000000000000000000000000000000000000000abc"
)

type pairRecTestEnv struct {
	t     *testing.T
	db    *StateDB
	clock *simulatedClock
	rec   *pairRecorder
	wall  int64
}

func newPairRecTestEnv(t *testing.T, faults pairRecFaults) *pairRecTestEnv {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.ensurePairRecordingSchema(); err != nil {
		t.Fatal(err)
	}
	e := &pairRecTestEnv{t: t, db: db, clock: &simulatedClock{}, wall: 1_800_000_000_000}
	e.rec = newPairRecorder(pairRecLive, pairRecTestRun, pairRecTestPartition, pairRecTestContract, db, e.clock, faults)
	e.rec.now = func() time.Time { return time.UnixMilli(atomic.LoadInt64(&e.wall)) }
	e.rec.epoch = 1
	bind := pairRecBind{RunID: pairRecTestRun, PaperIdentity: "id", ContractFP: pairRecTestContract, Partition: pairRecTestPartition, SeedSHA256: "seed", EvidenceIdentity: "ev", AnchorMs: 1}
	if err := e.rec.appendDirect(pairRecEvent{Kind: pairRecEventBind, Bind: &bind}, pairRecEvent{Kind: pairRecEventEpoch, Epoch: &pairRecEpoch{Epoch: 1, WallMs: 1, Mode: "new"}}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *pairRecTestEnv) advance(ms int64) { atomic.AddInt64(&e.wall, ms) }

func (e *pairRecTestEnv) log() *pairRecLog {
	e.t.Helper()
	l, err := readVerifiedPairRecLog(e.db, pairRecTestRun, pairRecTestPartition, pairRecTestContract)
	if err != nil {
		e.t.Fatalf("the committed recording must verify: %v", err)
	}
	return l
}

func pairRecResultOf(s string) func(ctx context.Context, h *pairRecHandle) pairRecResult {
	return func(ctx context.Context, h *pairRecHandle) pairRecResult {
		return pairRecResult{Class: pairRecClassResult, Result: []byte(s)}
	}
}

func TestBasketPairRecorderLogIsAppendOnlyHashChainedAndCheckedOnRead(t *testing.T) {
	e := newPairRecTestEnv(t, pairRecFaults{})
	e.rec.beginStep("s0@1", 0)
	if _, err := e.rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	var live int32
	res, saved, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindCollect, SlotMs: 60000, Request: []byte(`{"slot":60000}`)}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				e.advance(int64(8 - i))
				if _, _, err := h.child(ctx, pairRecCall{Kind: pairRecKindMemberRead, MemberID: fmt.Sprintf("m%d", i), Request: []byte(fmt.Sprintf(`{"m":%d}`, i))}, func(ctx context.Context) pairRecResult {
					atomic.AddInt32(&live, 1)
					return pairRecResult{Class: pairRecClassResult, Result: []byte(fmt.Sprintf(`{"read":%d}`, i))}
				}); err != nil {
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		return pairRecResult{Class: pairRecClassResult, Result: []byte(`{"both":true}`)}
	})
	if err != nil || res.Class != pairRecClassResult || saved == nil || live != 8 {
		t.Fatalf("a parent call with concurrent child reads must commit, got %+v %v (live %d)", res, err, live)
	}
	l := e.log()
	if len(l.children[saved.CallID]) != 8 {
		t.Fatalf("every concurrent child read is bound to its parent, got %v", l.children)
	}
	if got := e.rec.logicalNow(); got != saved.End.LogicalMs || got <= l.rows[2].Event.Wake.LogicalMs {
		t.Fatalf("logical time advances to the parent's committed completion after the wake, got %d (end %d)", got, saved.End.LogicalMs)
	}
	if _, err := e.db.db.Exec(`UPDATE basket_replay_inputs SET payload = x'00' WHERE run_id = ? AND seq = 3`, pairRecTestRun); err == nil {
		t.Fatal("a committed input row must refuse an update")
	}
	if _, err := e.db.db.Exec(`DELETE FROM basket_replay_inputs WHERE run_id = ? AND seq = 3`, pairRecTestRun); err == nil {
		t.Fatal("a committed input row must refuse a delete")
	}
	if _, err := e.db.db.Exec(`UPDATE basket_replay_calls SET result_hash = 'x' WHERE run_id = ? AND call_id = ?`, pairRecTestRun, saved.CallID); err == nil {
		t.Fatal("a terminal call index row must refuse a second transition")
	}
	if _, err := e.db.db.Exec(`DROP TRIGGER basket_replay_inputs_immutable`); err != nil {
		t.Fatal(err)
	}
	res2, err := e.db.db.Exec(`UPDATE basket_replay_inputs SET payload = payload || x'20' WHERE run_id = ? AND call_id = ? AND event_kind = ?`, pairRecTestRun, saved.CallID, pairRecEventEnd)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res2.RowsAffected(); n != 1 {
		t.Fatalf("the tamper must change one committed row, changed %d", n)
	}
	if _, err := readVerifiedPairRecLog(e.db, pairRecTestRun, pairRecTestPartition, pairRecTestContract); err == nil {
		t.Fatal("an altered committed payload must refuse the recording")
	}
}

func TestBasketPairRecorderBackwardWallClockHoldsIncreasesAndKeepsLogicalTimeMonotonic(t *testing.T) {
	e := newPairRecTestEnv(t, pairRecFaults{})
	e.rec.beginStep("s0@1", 0)
	first, err := e.rec.wake("", 0)
	if err != nil || e.rec.increaseHold() != "" {
		t.Fatalf("a forward wake holds nothing: %d %v %q", first, err, e.rec.increaseHold())
	}
	e.advance(-60_000)
	e.rec.beginStep("s1@2", 1)
	second, err := e.rec.wake("", 1)
	if err != nil {
		t.Fatal(err)
	}
	if second <= first || e.clock.Now().UnixMilli() != second {
		t.Fatalf("logical time never moves back: first %d, second %d, clock %d", first, second, e.clock.Now().UnixMilli())
	}
	if e.rec.increaseHold() == "" {
		t.Fatal("a backward wall clock holds new increases for the cycle")
	}
	l := e.log()
	if w := l.rows[len(l.rows)-1].Event.Wake; w == nil || !w.ClockBackward {
		t.Fatalf("the wake records the backward wall clock: %+v", w)
	}
	pb := newPairRecorder(pairRecPlayback, pairRecTestRun, pairRecTestPartition, pairRecTestContract, nil, &simulatedClock{}, pairRecFaults{})
	pb.log = l
	pb.cursor = 1
	pb.beginStep("s1@2", 1)
	if _, err := pb.wake("", 1); err != nil || pb.increaseHold() == "" {
		t.Fatalf("a playback applies the recorded backward-clock hold, got %v %q", err, pb.increaseHold())
	}
}

func TestBasketPairRecorderAppendFailureRefusesReadsUntilAnExplicitVerification(t *testing.T) {
	e := newPairRecTestEnv(t, pairRecFaults{kind: "fail-record-append", n: 4})
	e.rec.beginStep("s0@1", 0)
	if _, err := e.rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	var launched int32
	launch := func(ctx context.Context, h *pairRecHandle) pairRecResult {
		atomic.AddInt32(&launched, 1)
		return pairRecResult{Class: pairRecClassResult, Result: []byte("r")}
	}
	if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindReference, MemberID: "a", Request: []byte("q1")}, launch); err == nil {
		t.Fatal("a call whose start cannot commit must fail")
	}
	if launched != 0 {
		t.Fatal("no outside read runs before its start commits")
	}
	blocked, _, failed := e.rec.barrier()
	if !blocked || failed == 0 || e.rec.increaseHold() == "" {
		t.Fatalf("a failed append blocks increases at once: blocked=%v failed=%d", blocked, failed)
	}
	if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, Request: []byte("q2")}, launch); err == nil || launched != 0 {
		t.Fatalf("every later outside read is refused while blocked, got %v (launched %d)", err, launched)
	}
	elapsed, berr := e.rec.budget("collect", context.Background(), time.Now())
	if berr != nil || elapsed < 0 {
		t.Fatalf("a budget disposition is still measured while blocked: %v", berr)
	}
	if cleared, err := e.rec.clearAtBoundary(func() (int64, error) { return e.log().head().Seq, nil }); cleared || err == nil {
		t.Fatalf("while the store still refuses appends the release cannot commit and the barrier stays, got %v %v", cleared, err)
	}
	if cleared, err := e.rec.clearAtBoundary(func() (int64, error) { return 0, errors.New("still failing") }); cleared || err == nil {
		t.Fatalf("a failed verification keeps the barrier, got %v %v", cleared, err)
	}
	e.rec.faults = pairRecFaults{}
	if cleared, err := e.rec.clearAtBoundary(func() (int64, error) { return e.log().head().Seq, nil }); !cleared || err != nil {
		t.Fatalf("an explicit successful verification clears the barrier, got %v %v", cleared, err)
	}
	if blocked, _, _ := e.rec.barrier(); blocked || e.rec.increaseHold() != "" {
		t.Fatal("the barrier is clear after the recorded verification")
	}
	l := e.log()
	var failures, clears, budgets int
	for _, r := range l.rows {
		switch r.Kind {
		case pairRecEventFailure:
			failures++
		case pairRecEventClear:
			clears++
		case pairRecEventBudget:
			budgets++
		}
	}
	if failures != 2 || clears != 1 || budgets != 1 {
		t.Fatalf("the refused calls, the queued budget and the release are recorded in order, got %d failures, %d budgets, %d clears", failures, budgets, clears)
	}
	pb := newPairRecorder(pairRecPlayback, pairRecTestRun, pairRecTestPartition, pairRecTestContract, nil, &simulatedClock{}, pairRecFaults{})
	pb.log = l
	pb.beginStep("s0@1", 0)
	if _, err := pb.wake("", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pb.call(context.Background(), pairRecCall{Kind: pairRecKindReference, MemberID: "a", Request: []byte("q1")}, launch); err == nil || pb.increaseHold() == "" || launched != 0 {
		t.Fatalf("a playback reproduces the refused call and its barrier without a read, got %v %q", err, pb.increaseHold())
	}
	if _, _, err := pb.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, Request: []byte("q2")}, launch); err == nil || launched != 0 {
		t.Fatalf("a playback refuses the second recorded refusal too: %v", err)
	}
	if _, err := pb.budget("collect", context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if cleared, err := pb.clearAtBoundary(nil); !cleared || err != nil || pb.increaseHold() != "" {
		t.Fatalf("a playback releases the barrier at the recorded verification, got %v %v", cleared, err)
	}
	if left := pb.unconsumed(); len(left) != 0 {
		t.Fatalf("the playback consumed every recorded event, left %v", left)
	}
}

func pairRecCommittedStream(l *pairRecLog) int {
	n := 0
	for _, r := range l.rows {
		if pairRecConsumable(r) {
			n++
		}
	}
	return n
}

func TestBasketPairRecorderBarrierCursorCountsOnlyCommittedRows(t *testing.T) {
	e := newPairRecTestEnv(t, pairRecFaults{kind: "fail-record-append", n: 4})
	e.rec.beginStep("s0@1", 0)
	if _, err := e.rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	launch := pairRecResultOf("r")
	if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindReference, MemberID: "a", Request: []byte("q1")}, launch); err == nil {
		t.Fatal("a call whose start cannot commit must fail")
	}
	if _, err := e.rec.budget("collect", context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	e.rec.beginStep("s1@2", 0)
	if _, err := e.rec.wake("", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, Request: []byte("q2")}, launch); err == nil {
		t.Fatal("every read is refused while blocked")
	}
	if got, want := e.rec.cursorNow(), pairRecCommittedStream(e.log()); got != want {
		t.Fatalf("while the barrier holds the cursor counts committed stream rows only: cursor %d, committed %d", got, want)
	}
	e.rec.faults = pairRecFaults{}
	if cleared, err := e.rec.clearAtBoundary(func() (int64, error) { return e.log().head().Seq, nil }); !cleared || err != nil {
		t.Fatalf("the verified release commits the queue, got %v %v", cleared, err)
	}
	l := e.log()
	if got, want := e.rec.cursorNow(), pairRecCommittedStream(l); got != want || want != len(l.stream) {
		t.Fatalf("after the release the cursor equals the committed stream: cursor %d, committed %d, stream %d", got, want, len(l.stream))
	}
	e.rec.beginStep("s2@3", 0)
	if _, err := e.rec.wake("", 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindReference, MemberID: "a", Request: []byte("q3")}, launch); err != nil {
		t.Fatal(err)
	}
	if got, want := e.rec.cursorNow(), len(e.log().stream); got != want {
		t.Fatalf("later live rows keep the cursor on the committed stream: cursor %d, stream %d", got, want)
	}
}

func TestBasketPairRecorderPlaybackKeepsFailuresWithoutLaunchAndChecksProcessResults(t *testing.T) {
	e := newPairRecTestEnv(t, pairRecFaults{})
	e.rec.beginStep("s0@1", 0)
	if _, err := e.rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	if res, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, SlotMs: 1, Request: []byte("p1")}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		return pairRecResult{Class: pairRecClassCancelled, Cause: context.DeadlineExceeded.Error()}
	}); err != nil || res.Class != pairRecClassCancelled {
		t.Fatalf("a cancelled process outcome is recorded, got %+v %v", res, err)
	}
	if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, SlotMs: 2, Request: []byte("p2")}, pairRecResultOf("enter")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindNativeCheck, MemberID: "m", Request: []byte("c1")}, pairRecResultOf("check")); err != nil {
		t.Fatal(err)
	}
	if d, err := e.rec.budget("policy", context.WithValue(context.Background(), struct{}{}, 1), time.Now().Add(-time.Second)); err != nil || d < time.Second {
		t.Fatalf("a budget disposition records the elapsed wall time, got %s %v", d, err)
	}
	l := e.log()
	pb := newPairRecorder(pairRecPlayback, pairRecTestRun, pairRecTestPartition, pairRecTestContract, nil, &simulatedClock{}, pairRecFaults{})
	pb.log = l
	pb.beginStep("s0@1", 0)
	if _, err := pb.wake("", 0); err != nil {
		t.Fatal(err)
	}
	var launched int32
	res, _, err := pb.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, SlotMs: 1, Request: []byte("p1")}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		atomic.AddInt32(&launched, 1)
		return pairRecResult{Class: pairRecClassResult, Result: []byte("enter")}
	})
	if err != nil || res.Class != pairRecClassCancelled || res.Cause != context.DeadlineExceeded.Error() || launched != 0 {
		t.Fatalf("a recorded cancellation reproduces without a process launch, got %+v %v (launched %d)", res, err, launched)
	}
	if res, _, err := pb.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, SlotMs: 2, Request: []byte("p2")}, pairRecResultOf("enter")); err != nil || string(res.Result) != "enter" {
		t.Fatalf("a recorded result reruns and matches, got %+v %v", res, err)
	}
	if len(pb.equalityReasons()) != 0 {
		t.Fatalf("matching reruns make no refusal: %v", pb.equalityReasons())
	}
	res, _, err = pb.call(context.Background(), pairRecCall{Kind: pairRecKindNativeCheck, MemberID: "m", Request: []byte("c1")}, pairRecResultOf("different"))
	if err != nil || string(res.Result) != "check" || len(pb.equalityReasons()) != 1 {
		t.Fatalf("a rerun that differs keeps the saved result and refuses equality, got %+v %v %v", res, err, pb.equalityReasons())
	}
	d, berr := pb.budget("policy", context.Background(), time.Now())
	if berr != nil || d < time.Second {
		t.Fatalf("a playback returns the recorded budget disposition, never the local replay speed, got %s %v", d, berr)
	}
	if _, _, err := pb.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, SlotMs: 3, Request: []byte("p3")}, pairRecResultOf("x")); err == nil || pb.divergedReason() == "" {
		t.Fatal("an unexpected call beyond the recording diverges")
	}
}

func TestBasketPairRecorderResumeMarksAnInterruptedCallUnknownAndNeverRepeatsIt(t *testing.T) {
	e := newPairRecTestEnv(t, pairRecFaults{})
	e.rec.beginStep("s0@1", 0)
	if _, err := e.rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindMatchPoll, MemberID: "a", Request: []byte("poll")}, pairRecResultOf("book")); err != nil {
		t.Fatal(err)
	}
	start := pairRecStart{Kind: pairRecKindPolicy, Phase: pairRecPhaseStep, SlotMs: 5, StepID: "s0@1", Ordinal: 2, Request: []byte("policy"), RequestHash: sha256Hex([]byte("policy")), RawStartMs: 1}
	if err := e.rec.appendDirect(pairRecEvent{Kind: pairRecEventStart, CallID: "s0@1/2", Start: &start}); err != nil {
		t.Fatal(err)
	}
	l := e.log()
	if ids := l.unterminated(); len(ids) != 1 || ids[0] != "s0@1/2" {
		t.Fatalf("the interrupted start has no terminal event: %v", ids)
	}
	rs := newPairRecorder(pairRecResume, pairRecTestRun, pairRecTestPartition, pairRecTestContract, e.db, &simulatedClock{}, pairRecFaults{})
	rs.log, rs.head, rs.epoch = l, l.head(), 2
	rs.restorePosition(0, 0, 0, "s0@1")
	if _, err := rs.wake("", 0); err != nil {
		t.Fatal(err)
	}
	var launched int32
	launch := func(ctx context.Context, h *pairRecHandle) pairRecResult {
		atomic.AddInt32(&launched, 1)
		return pairRecResult{Class: pairRecClassResult, Result: []byte("fresh")}
	}
	if res, _, err := rs.call(context.Background(), pairRecCall{Kind: pairRecKindMatchPoll, MemberID: "a", Request: []byte("poll")}, launch); err != nil || string(res.Result) != "book" || launched != 0 {
		t.Fatalf("a committed outcome is reused after the restart without a read, got %+v %v", res, err)
	}
	if _, saved, err := rs.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, SlotMs: 5, Request: []byte("policy")}, launch); err == nil || saved == nil || !saved.Recovered || launched != 0 {
		t.Fatalf("an interrupted call returns its unknown outcome and never runs again, got %v %+v (launched %d)", err, saved, launched)
	}
	if res, saved, err := rs.call(context.Background(), pairRecCall{Kind: pairRecKindPolicy, SlotMs: 5, Request: []byte("policy")}, launch); err != nil || launched != 1 || saved.CallID == "s0@1/2" || string(res.Result) != "fresh" {
		t.Fatalf("a later recovery query takes a new identity, got %+v %v", saved, err)
	}
	after := e.log()
	if ids := after.unterminated(); len(ids) != 0 {
		t.Fatalf("every call is terminated or recovered after the resume: %v", ids)
	}
	if after.rows[after.ends["s0@1/2"]].Kind != pairRecEventRecovery {
		t.Fatal("the interrupted call ends with an explicit recovery event")
	}
}

func (e *pairTestEnv) setRecordingBarrier(failedSeq int64) {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	rt := e.rt()
	rt.Pair.Progress.RecordingBlocked = &pairRecBarrier{Reason: "the recording store refused an append", FailedSeq: failedSeq, AtMs: e.now.UnixMilli()}
	rt.touchPairProgress()
	if !e.bc.persistPair(rt) {
		e.t.Fatal("the barrier must save through the pair progress writer")
	}
}

func (e *pairTestEnv) barrier() *pairRecBarrier {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.rt().Pair.Progress.RecordingBlocked
}

func TestBasketPairRecordingBarrierHoldsIncreasesWhileEmergencyClosesContinue(t *testing.T) {
	e := newPairPaperEmergencyEnv(t)
	e.collect = true
	slotsBefore := len(e.rt().Rebalances)
	e.setRecordingBarrier(41)
	e.mu.Lock()
	why := e.bc.pairIncreaseHold(e.rt(), e.inputs(e.now))
	e.mu.Unlock()
	if !strings.Contains(why, "recording barrier") {
		t.Fatalf("the saved barrier holds every increase, got %q", why)
	}
	e.quoteOutage(true)
	if trip := e.tripLossStreak(pairTestOKX); !strings.HasPrefix(trip, RiskReasonConsecutiveLosses) {
		t.Fatalf("the breaker trips, got %q", trip)
	}
	e.step(e.now.Add(time.Minute))
	if e.signed(pairTestOKX) != -0.06 || !e.hasObligation(pairObligationEmergencyClose, pairTestOKX) {
		t.Fatalf("a close whose own mark could not be recorded keeps the book and its obligation, got %v %v", e.signed(pairTestOKX), e.obligationKinds(pairTestOKX))
	}
	if got := len(e.rt().Rebalances); got != slotsBefore {
		t.Fatalf("the barrier collects no new cadence slot, got %d slots after %d", got, slotsBefore)
	}
	e.quoteOutage(false)
	e.step(e.now.Add(time.Second))
	own, valid, _ := e.ownMark(pairTestOKX)
	tr, ok := e.closeTrade(pairTestOKX)
	if e.signed(pairTestOKX) != 0 || !valid || !ok || !pairCostNear(tr.Price, own) {
		t.Fatalf("with committed evidence the staged emergency close books at its own mark %v while the barrier holds increases, got book %v trade %+v", own, e.signed(pairTestOKX), tr)
	}
	if b := e.barrier(); b == nil || b.FailedSeq != 41 {
		t.Fatalf("an emergency close never clears the barrier, got %+v", b)
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if b := e.barrier(); b == nil || b.FailedSeq != 41 {
		t.Fatalf("a restart keeps the saved barrier, got %+v", b)
	}
	e.mu.Lock()
	why = e.bc.pairIncreaseHold(e.rt(), e.inputs(e.now))
	e.mu.Unlock()
	if !strings.Contains(why, "recording barrier") {
		t.Fatalf("after a restart the saved barrier still holds increases without any host hook, got %q", why)
	}
	if trip := e.tripLossStreak(pairTestHL); !strings.HasPrefix(trip, RiskReasonConsecutiveLosses) {
		t.Fatalf("the second breaker trips, got %q", trip)
	}
	for i := 0; i < 3 && e.lifecycle() != pairLifecycleFlat; i++ {
		e.step(e.now.Add(time.Second))
	}
	if e.signed(pairTestHL) != 0 || e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("the second emergency close books and the pair resets to flat, got %v %s", e.signed(pairTestHL), e.lifecycle())
	}
	if b := e.barrier(); b == nil || b.FailedSeq != 41 {
		t.Fatalf("the flat lifecycle reset carries the barrier, got %+v", b)
	}
	file := e.store.file(e.store.layout.roleForPartition(e.rt().Partition))
	var saved string
	if err := file.db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(saved, `"recording_blocked"`) || !strings.Contains(saved, `"v":4`) {
		t.Fatalf("the saved progress carries the barrier as version 4, got %s", saved)
	}
}

type pairRecOracleFixture struct {
	policy   pairFundingPolicyV1
	digest   string
	key      pairEvidenceKey
	target   int64
	deadline int64
	src      *pairRecordedFundingSource
}

func newPairRecOracleFixture(t *testing.T, target int64) *pairRecOracleFixture {
	t.Helper()
	policy := pairFundingPolicyV1{Version: pairFundingPolicyVersion, MemberRules: map[string]string{"hl": pairFundingRuleHLReceiptResearch, "okx": pairFundingRuleOKX},
		WindowMs: 5000, MaxPrintOffsetMs: 60000, PrepareLeadMs: 30000, FromMs: target - 120000, DrainThroughMs: target + 180000}
	digest, err := pairFundingPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	src, err := newPairRecordedFundingSourceWithPolicy("paper:rec-run-1", policy.FromMs, policy)
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := pairFundingOracleDeadline(policy, target)
	if err != nil {
		t.Fatal(err)
	}
	return &pairRecOracleFixture{policy: policy, digest: digest, key: pairEvidenceKey{Partition: "paper", Provider: entryProviderHyperliquid, AccountKey: "synthetic:rec", Instrument: "BTC", MemberID: "hl"},
		target: target, deadline: deadline, src: src}
}

func (f *pairRecOracleFixture) request(t *testing.T) []byte {
	t.Helper()
	args, err := pairPaperOracleArgs(f.key, f.policy, f.target, f.deadline, f.digest)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := pairFundingOracleAttemptID(f.key, f.target, f.digest)
	if err != nil {
		t.Fatal(err)
	}
	chain := f.src.chain()
	prev := ""
	if len(chain) > 0 {
		prev = chain[len(chain)-1].BatchHash
	}
	raw, err := pairCanonicalJSON(pairPaperOracleRequest{Args: args, Key: f.key, SourceID: f.src.SourceID(), Seq: int64(len(chain) + 1), PrevHash: prev, TargetEventMs: f.target, PolicyDigest: f.digest, AttemptID: attempt})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *pairRecOracleFixture) batch(t *testing.T, requestMs, receiptMs int64) []byte {
	t.Helper()
	attempt, _ := pairFundingOracleAttemptID(f.key, f.target, f.digest)
	o, err := pairFundingOracleObservation{V: pairFundingObservationVersion, Key: f.key, EvidenceID: pairFundingOracleEvidenceID(f.key, f.target, receiptMs), AttemptID: attempt,
		PolicyDigest: f.digest, PriceKind: pairFundingPriceOracleReceipt, TargetEventMs: f.target, RequestMs: requestMs, ReceiptMs: receiptMs, AtMs: receiptMs, VisibleMs: receiptMs,
		TimeBasis: pairFundingObservationTimeBasis, Price: "60000.5", Provenance: pairFundingOracleProvenance}.sealed()
	if err != nil {
		t.Fatal(err)
	}
	chain := f.src.chain()
	prev := ""
	if len(chain) > 0 {
		prev = chain[len(chain)-1].BatchHash
	}
	b, err := pairFundingBatchV2{V: pairFundingResearchBatchVersion, SourceID: f.src.SourceID(), Seq: int64(len(chain) + 1), PrevHash: prev, PolicyDigest: f.digest,
		Rates: []pairFundingRate{}, Prices: []pairFundingPrice{}, Observations: []pairFundingOracleObservation{o}, Coverage: []pairFundingCoverageRecord{}}.sealed()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pairFundingBatchValueBytes(pairFundingBatchV2Value(b))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBasketPairRecorderOracleCommitsBeforePublication(t *testing.T) {
	if pairRecProcessKinds[pairRecKindFundingOracle] || !pairRecCallKinds[pairRecKindFundingOracle] {
		t.Fatal("funding_oracle is a recorded call kind that playback never reruns as a process")
	}
	target := int64(1_800_003_600_000)
	type outcome struct {
		requestMs, receiptMs, completionMs int64
		backward                           bool
		admitted                           bool
	}
	cases := map[string]outcome{
		"receipt and completion inside the window": {requestMs: target, receiptMs: target + 4960, completionMs: target + 4980, admitted: true},
		"completion exactly at the deadline":       {requestMs: target, receiptMs: target + 4980, completionMs: target + 5000, admitted: true},
		"receipt at the deadline, completion late": {requestMs: target, receiptMs: target + 5000, completionMs: target + 5020},
		"receipt after the deadline":               {requestMs: target, receiptMs: target + 5001, completionMs: target + 5001},
		"request before the hour":                  {requestMs: target - 1, receiptMs: target + 100, completionMs: target + 120},
		"backward terminal":                        {requestMs: target, receiptMs: target + 100, completionMs: target - 40000, backward: true},
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := cases[name]
		t.Run(name, func(t *testing.T) {
			e := newPairRecTestEnv(t, pairRecFaults{})
			atomic.StoreInt64(&e.wall, target-30000)
			f := newPairRecOracleFixture(t, target)
			e.rec.beginStep("s0@1", 0)
			if _, err := e.rec.wake("", 0); err != nil {
				t.Fatal(err)
			}
			req := f.request(t)
			batch := f.batch(t, c.requestMs, c.receiptMs)
			var sawStart bool
			res, saved, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindFundingOracle, MemberID: f.key.MemberID, SlotMs: target, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
				l := e.log()
				for _, r := range l.rows {
					if r.Kind == pairRecEventStart && r.Event.Start.Kind == pairRecKindFundingOracle {
						_, ended := l.ends[r.CallID]
						sawStart = !ended
					}
				}
				if len(f.src.chain()) != 0 {
					t.Error("nothing is published while the reader runs")
				}
				atomic.StoreInt64(&e.wall, c.completionMs)
				return pairRecResult{Class: pairRecClassResult, Result: batch, ReceiptMs: c.receiptMs}
			})
			if err != nil || res.Class != pairRecClassResult || saved == nil || saved.End == nil {
				t.Fatalf("the oracle call commits its terminal: %+v %v", res, err)
			}
			if !sawStart {
				t.Fatal("the oracle start commits before the reader runs")
			}
			if len(f.src.chain()) != 0 {
				t.Fatal("rec.call publishes nothing; the source changes only through admission")
			}
			l := e.log()
			end := l.rows[l.ends[saved.CallID]].Event.End
			if !bytes.Equal(end.Result, batch) || end.RawReceiptMs != c.receiptMs || end.RawCompletionMs != c.completionMs || end.ClockBackward != c.backward {
				t.Fatalf("the committed terminal holds the exact batch bytes and raw times: %+v", end)
			}
			fromBefore := pairPaperFundingFrom(f.src.chain(), f.key, f.policy.FromMs)
			err = pairPaperAdmitFundingResult(f.src, res.Result, saved.Start, saved.End)
			if c.admitted != (err == nil) {
				t.Fatalf("admission of %s: want admitted=%v, got %v", name, c.admitted, err)
			}
			if !c.admitted {
				if len(f.src.chain()) != 0 {
					t.Fatal("an ineligible terminal stays only in the recording")
				}
				return
			}
			if len(f.src.chain()) != 1 || !bytes.Equal(f.src.chainRaw(1), end.Result) {
				t.Fatal("the admitted batch equals the committed terminal bytes")
			}
			if pairPaperFundingFrom(f.src.chain(), f.key, f.policy.FromMs) != fromBefore {
				t.Fatal("an oracle batch never proves or extends rate coverage")
			}
			if err := pairPaperAdmitFundingResult(f.src, res.Result, saved.Start, saved.End); err != nil || len(f.src.chain()) != 1 {
				t.Fatalf("re-delivery of the accepted bytes is skipped: %v", err)
			}
			attempts, err := pairPaperOracleAttemptIndex(l)
			if err != nil || len(attempts) != 1 {
				t.Fatalf("the committed start is the hour's one attempt: %v %v", attempts, err)
			}
		})
	}
	t.Run("terminal append failure publishes nothing", func(t *testing.T) {
		e := newPairRecTestEnv(t, pairRecFaults{})
		atomic.StoreInt64(&e.wall, target-30000)
		f := newPairRecOracleFixture(t, target)
		e.rec.beginStep("s0@1", 0)
		if _, err := e.rec.wake("", 0); err != nil {
			t.Fatal(err)
		}
		e.rec.faults = pairRecFaults{kind: "fail-record-append", n: 2}
		var ran bool
		_, saved, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindFundingOracle, MemberID: f.key.MemberID, SlotMs: target, Request: f.request(t)}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
			ran = true
			atomic.StoreInt64(&e.wall, target+200)
			return pairRecResult{Class: pairRecClassResult, Result: f.batch(t, target, target+100), ReceiptMs: target + 100}
		})
		if !ran || err == nil || saved != nil {
			t.Fatalf("a terminal that does not commit is never returned for use: ran=%v saved=%+v err=%v", ran, saved, err)
		}
		if len(f.src.chain()) != 0 {
			t.Fatal("no batch is published after a terminal append failure")
		}
		if blocked, _, _ := e.rec.barrier(); !blocked {
			t.Fatal("the failed terminal append sets the recording barrier")
		}
	})
	t.Run("two starts for one hour refuse the log", func(t *testing.T) {
		e := newPairRecTestEnv(t, pairRecFaults{})
		atomic.StoreInt64(&e.wall, target-30000)
		f := newPairRecOracleFixture(t, target)
		e.rec.beginStep("s0@1", 0)
		if _, err := e.rec.wake("", 0); err != nil {
			t.Fatal(err)
		}
		req := f.request(t)
		for i := 0; i < 2; i++ {
			if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindFundingOracle, MemberID: f.key.MemberID, SlotMs: target, Request: req}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
				return pairRecResult{Class: pairRecClassInfra, Cause: "fixture failure"}
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pairPaperOracleAttemptIndex(e.log()); err == nil || !strings.Contains(err.Error(), "one outside attempt") {
			t.Fatalf("a repeated outside attempt for one hour refuses the recording, got %v", err)
		}
	})
}

func TestBasketReplayPairPaperFamily2RefusedSequenceStaysReusable(t *testing.T) {
	target := int64(1_800_003_600_000)
	f := newPairRecOracleFixture(t, target)
	req := f.request(t)
	raw := f.batch(t, target, target+5000)
	start := pairRecStart{Kind: pairRecKindFundingOracle, Request: req}
	end := &pairRecEnd{Class: pairRecClassResult, Result: raw, RawReceiptMs: target + 5000, RawCompletionMs: target + 5020}
	if err := pairPaperAdmitFundingResult(f.src, raw, start, end); err == nil {
		t.Fatal("a family-2 terminal completed after the deadline is refused")
	}
	if len(f.src.chain()) != 0 {
		t.Fatal("the refused terminal allocates no funding sequence")
	}
	next := f.batch(t, target, target+1000)
	if err := f.src.Append(next); err != nil {
		t.Fatalf("the next batch reuses sequence 1: %v", err)
	}
	if got := f.src.chain(); len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("the accepted batch is sequence 1, got %+v", got)
	}
}

func TestBasketReplayPairPaperAsyncConsumptionIsTheOnlyPublication(t *testing.T) {
	db, err := OpenStateDB(filepath.Join(t.TempDir(), basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.ensurePairRecordingSchema(); err != nil {
		t.Fatal(err)
	}
	rec := newPairRecorder(pairRecLive, pairRecTestRun, pairRecTestPartition, pairRecTestContract, db, &simulatedClock{}, pairRecFaults{})
	rec.schema = pairRecSchemaAsync
	rec.now = func() time.Time { return time.UnixMilli(1_800_000_000_000) }
	rec.epoch = 1
	bind := pairRecBind{RunID: pairRecTestRun, PaperIdentity: "id", ContractFP: pairRecTestContract, Partition: pairRecTestPartition, SeedSHA256: "seed", EvidenceIdentity: "ev", AnchorMs: 1, Family: pairPaperManifestAsyncVer}
	if err := rec.appendDirect(pairRecEvent{Kind: pairRecEventBind, Bind: &bind}, pairRecEvent{Kind: pairRecEventEpoch, Epoch: &pairRecEpoch{Epoch: 1, WallMs: 1, Mode: "new"}}); err != nil {
		t.Fatal(err)
	}
	rec.beginStep("s0@1", 0)
	if _, err := rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	req := []byte(`{"attempt_id":"a","key":{"account_key":"synthetic:rec","instrument":"BTC","member_id":"hl","partition":"paper","provider":"hyperliquid"},"policy_digest":"d","source_id":"s","target_event_ms":1}`)
	callID, start, err := rec.beginAsyncCall(pairRecCall{Kind: pairRecKindFundingOracle, MemberID: "hl", SlotMs: 1, Request: req})
	if err != nil {
		t.Fatal(err)
	}
	afterStart := rec.cursorNow()
	_, saved, err := rec.call(context.Background(), pairRecCall{Kind: pairRecKindMatchPoll, MemberID: "hl", Request: []byte("book")}, pairRecResultOf("book"))
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || saved.End == nil || bytes.Contains(saved.End.Result, []byte("obs")) {
		t.Fatal("a later call during the pending oracle does not receive the observation")
	}
	if _, ok := mustPairRecLog(t, db).consumes[callID]; ok {
		t.Fatal("the observation is not published at the start or at a later call")
	}
	afterCall := rec.cursorNow()
	if afterCall <= afterStart {
		t.Fatal("the later call is its own consumable stream item")
	}
	end, err := rec.appendAsyncTerminal(callID, start, pairAsyncOracleResult{res: pairRecResult{Class: pairRecClassResult, Result: []byte(`{"obs":1}`), ReceiptMs: 2}, completion: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rec.cursorNow() != afterCall {
		t.Fatal("an asynchronous terminal is not a consumable stream item")
	}
	l := mustPairRecLog(t, db)
	if _, ok := l.consumes[callID]; ok {
		t.Fatal("the observation is not published at the terminal")
	}
	if err := rec.appendConsume(callID, pairRecConsume{RequestHash: start.RequestHash, TerminalHash: end.ResultHash, TerminalKind: pairRecEventEnd, WakeSeq: 1, StepID: "s0@1", LogicalMs: 4, Disposition: "refused"}); err != nil {
		t.Fatal(err)
	}
	if rec.cursorNow() != afterCall+1 {
		t.Fatalf("call_consume advances the stream cursor once, got %d after %d", rec.cursorNow(), afterCall)
	}
	l = mustPairRecLog(t, db)
	if l.consumes[callID] == 0 && len(l.consumes) != 1 {
		t.Fatal("the consumption event is the publication")
	}
	if err := rec.appendConsume(callID, pairRecConsume{RequestHash: start.RequestHash, TerminalHash: end.ResultHash, TerminalKind: pairRecEventEnd, WakeSeq: 1, StepID: "s0@1", LogicalMs: 5, Disposition: "refused"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readVerifiedPairRecLog(db, pairRecTestRun, pairRecTestPartition, pairRecTestContract); err == nil {
		t.Fatal("a second consumption of the same call is refused")
	}
}

func TestBasketReplayPairPaperAsyncConsumptionRefusesAMismatchedTerminal(t *testing.T) {
	db, err := OpenStateDB(filepath.Join(t.TempDir(), basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.ensurePairRecordingSchema(); err != nil {
		t.Fatal(err)
	}
	rec := newPairRecorder(pairRecLive, pairRecTestRun, pairRecTestPartition, pairRecTestContract, db, &simulatedClock{}, pairRecFaults{})
	rec.schema = pairRecSchemaAsync
	rec.now = func() time.Time { return time.UnixMilli(1_800_000_000_000) }
	rec.epoch = 1
	bind := pairRecBind{RunID: pairRecTestRun, PaperIdentity: "id", ContractFP: pairRecTestContract, Partition: pairRecTestPartition, SeedSHA256: "seed", EvidenceIdentity: "ev", AnchorMs: 1, Family: pairPaperManifestAsyncVer}
	if err := rec.appendDirect(pairRecEvent{Kind: pairRecEventBind, Bind: &bind}, pairRecEvent{Kind: pairRecEventEpoch, Epoch: &pairRecEpoch{Epoch: 1, WallMs: 1, Mode: "new"}}); err != nil {
		t.Fatal(err)
	}
	rec.beginStep("s0@1", 0)
	if _, err := rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	req := []byte(`{"attempt_id":"a","target_event_ms":1}`)
	callID, start, err := rec.beginAsyncCall(pairRecCall{Kind: pairRecKindFundingOracle, MemberID: "hl", SlotMs: 1, Request: req})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.appendConsume(callID, pairRecConsume{RequestHash: start.RequestHash, TerminalHash: start.RequestHash, TerminalKind: pairRecEventEnd, WakeSeq: 1, StepID: "s0@1", LogicalMs: 2, Disposition: "refused"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readVerifiedPairRecLog(db, pairRecTestRun, pairRecTestPartition, pairRecTestContract); err == nil || !strings.Contains(err.Error(), "without a terminal") {
		t.Fatalf("consumption before the terminal is refused, got %v", err)
	}
}

func TestBasketReplayPairPaperSchema1FamilyLogsStayByteForByte(t *testing.T) {
	e := newPairRecTestEnv(t, pairRecFaults{})
	if _, err := readVerifiedPairRecLog(e.db, pairRecTestRun, pairRecTestPartition, pairRecTestContract); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		seq    int64
		kind   string
		prefix string
		raw    string
	}{
		{1, pairRecEventBind, "10752d7e82be9c320fd9b9696777ac5567800e3bc32c0f890a9f62e38486debb", `{"v":1,"kind":"bind","bind":{"run_id":"rec-run-1","paper_identity":"id","contract_fingerprint":"0000000000000000000000000000000000000000000000000000000000000abc","partition":"paper","seed_sha256":"seed","evidence_identity":"ev","funding_binding":"","anchor_ms":1}}`},
		{2, pairRecEventEpoch, "b905dc7e4ab2aea5f563271bc65a42b5609920f892c31db650b067dc1b1aaef9", `{"v":1,"kind":"epoch","epoch":{"epoch":1,"wall_ms":1,"mode":"new"}}`},
	}
	q, err := e.db.db.Query(`SELECT seq, schema_version, event_kind, prefix_hash, payload FROM basket_replay_inputs WHERE run_id = ? ORDER BY seq`, pairRecTestRun)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for _, w := range want {
		if !q.Next() {
			t.Fatalf("schema 1 log is missing row %d", w.seq)
		}
		var seq int64
		var schema int
		var kind, prefix string
		var payload []byte
		if err := q.Scan(&seq, &schema, &kind, &prefix, &payload); err != nil {
			t.Fatal(err)
		}
		if seq != w.seq || schema != pairRecSchemaVersion || kind != w.kind || prefix != w.prefix || string(payload) != w.raw {
			t.Fatalf("family 1 and family 2 logs are schema 1 and must stay these bytes: seq %d schema %d kind %s prefix %s payload %s", seq, schema, kind, prefix, payload)
		}
		if bytes.Contains(payload, []byte(`"family"`)) || bytes.Contains(payload, []byte(`"async"`)) || bytes.Contains(payload, []byte(`"consume"`)) {
			t.Fatalf("schema 1 payload gained a family-3 field: %s", payload)
		}
	}
	if q.Next() {
		t.Fatal("the schema 1 fixture log has more than the bind and epoch rows")
	}
}

func TestBasketReplayPairPaperRateBatchBetweenOracleStartAndConsumptionKeepsItsRequest(t *testing.T) {
	db, err := OpenStateDB(filepath.Join(t.TempDir(), basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.ensurePairRecordingSchema(); err != nil {
		t.Fatal(err)
	}
	rec := newPairRecorder(pairRecLive, pairRecTestRun, pairRecTestPartition, pairRecTestContract, db, &simulatedClock{}, pairRecFaults{})
	rec.schema = pairRecSchemaAsync
	rec.now = func() time.Time { return time.UnixMilli(1_800_000_000_000) }
	rec.epoch = 1
	bind := pairRecBind{RunID: pairRecTestRun, PaperIdentity: "id", ContractFP: pairRecTestContract, Partition: pairRecTestPartition, SeedSHA256: "seed", EvidenceIdentity: "ev", AnchorMs: 1, Family: pairPaperManifestAsyncVer}
	if err := rec.appendDirect(pairRecEvent{Kind: pairRecEventBind, Bind: &bind}, pairRecEvent{Kind: pairRecEventEpoch, Epoch: &pairRecEpoch{Epoch: 1, WallMs: 1, Mode: "new"}}); err != nil {
		t.Fatal(err)
	}
	rec.beginStep("s0@1", 0)
	if _, err := rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	f := newPairRecOracleFixture(t, 1_800_003_600_000)
	oracleReq := []byte(`{"attempt_id":"a","target_event_ms":1}`)
	callID, start, err := rec.beginAsyncCall(pairRecCall{Kind: pairRecKindFundingOracle, MemberID: "hl", SlotMs: 1, Request: oracleReq})
	if err != nil {
		t.Fatal(err)
	}
	rateVal, rateRaw := pairRecTestBatch(t, f, 1, "", nil)
	recorded := []byte(`{"prev_hash":"","seq":1}`)
	if _, _, err := rec.call(context.Background(), pairRecCall{Kind: pairRecKindFunding, MemberID: "hl", Request: recorded}, func(ctx context.Context, h *pairRecHandle) pairRecResult {
		return pairRecResult{Class: pairRecClassResult, Result: rateRaw}
	}); err != nil {
		t.Fatal(err)
	}
	afterRate := rec.cursorNow()
	end, err := rec.appendAsyncTerminal(callID, start, pairAsyncOracleResult{res: pairRecResult{Class: pairRecClassResult, Result: []byte(`{"obs":1}`), ReceiptMs: 2}, completion: 3})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := pairFundingOracleAttemptID(f.key, f.target, f.digest)
	if err != nil {
		t.Fatal(err)
	}
	receipt := f.target + 1000
	obs, err := (pairFundingOracleObservation{V: pairFundingObservationVersion, Key: f.key, EvidenceID: pairFundingOracleEvidenceID(f.key, f.target, receipt), AttemptID: attempt,
		PolicyDigest: f.digest, PriceKind: pairFundingPriceOracleReceipt, TargetEventMs: f.target, RequestMs: f.target, ReceiptMs: receipt, AtMs: receipt, VisibleMs: receipt,
		TimeBasis: pairFundingObservationTimeBasis, Price: "60000.5", Provenance: pairFundingOracleProvenance}).sealed()
	if err != nil {
		t.Fatal(err)
	}
	oracleVal, oracleRaw := pairRecTestBatch(t, f, 2, rateVal.BatchHash, []pairFundingOracleObservation{obs})
	if err := rec.appendConsume(callID, pairRecConsume{RequestHash: start.RequestHash, TerminalHash: end.ResultHash, TerminalKind: pairRecEventEnd, WakeSeq: 1, StepID: "s0@1", LogicalMs: 4, Disposition: "accepted", Batch: oracleRaw, BatchHash: oracleVal.BatchHash}); err != nil {
		t.Fatal(err)
	}
	l := mustPairRecLog(t, db)
	ratePos := -1
	for i, ri := range l.stream {
		row := l.rows[ri]
		if row.Kind == pairRecEventStart && row.Event.Start != nil && row.Event.Start.Kind == pairRecKindFunding && string(row.Event.Start.Request) == string(recorded) {
			ratePos = i
		}
	}
	if ratePos < 0 || afterRate != ratePos+1 {
		t.Fatalf("the rate call is the consumable item before its consumption, pos %d cursor %d", ratePos, afterRate)
	}
	rec.adoptLog(l, afterRate)
	chain := []pairFundingBatchValue{rateVal, oracleVal}
	rec.restorePosition(afterRate, 0, 1, "s0@1")
	visible := rec.fundingConsumed(chain)
	if len(visible) != 1 || visible[0].Seq != 1 || visible[0].PrevHash != "" || visible[0].BatchHash != rateVal.BatchHash {
		t.Fatalf("between the rate commit and the oracle consumption the visible chain is the rate batch alone, got %+v", visible)
	}
	if seq, prev := int64(len(visible)+1), visible[len(visible)-1].BatchHash; seq != 2 || prev != rateVal.BatchHash {
		t.Fatalf("the next rate request is seq 2 after %s, got seq %d prev %s", rateVal.BatchHash, seq, prev)
	}
	rec.restorePosition(ratePos, 0, 1, "s0@1")
	replay := rec.fundingConsumed(chain)
	if len(replay) != 0 {
		t.Fatalf("replaying the recorded rate request sees an empty chain, got %+v", replay)
	}
	if seq, prev := int64(len(replay)+1), ""; seq != 1 || prev != "" {
		t.Fatalf("the resumed rate request is seq 1 with an empty previous hash")
	}
}

func pairRecTestBatch(t *testing.T, f *pairRecOracleFixture, seq int64, prev string, obs []pairFundingOracleObservation) (pairFundingBatchValue, []byte) {
	t.Helper()
	if obs == nil {
		obs = []pairFundingOracleObservation{}
	}
	for i := range obs {
		sealed, err := obs[i].sealed()
		if err != nil {
			t.Fatal(err)
		}
		obs[i] = sealed
	}
	b, err := (pairFundingBatchV2{V: pairFundingResearchBatchVersion, SourceID: f.src.SourceID(), Seq: seq, PrevHash: prev, PolicyDigest: f.digest,
		Rates: []pairFundingRate{}, Prices: []pairFundingPrice{}, Observations: obs, Coverage: []pairFundingCoverageRecord{}}).sealed()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := pairFundingBatchValueBytes(pairFundingBatchV2Value(b))
	if err != nil {
		t.Fatal(err)
	}
	got, err := pairDecodeFundingBatch(raw)
	if err != nil {
		t.Fatal(err)
	}
	return got, raw
}

func mustPairRecLog(t *testing.T, db *StateDB) *pairRecLog {
	t.Helper()
	l, err := readVerifiedPairRecLog(db, pairRecTestRun, pairRecTestPartition, pairRecTestContract)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func newPairAsyncRecordingEnv(t *testing.T) *pairRecTestEnv {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), basketReplayDBName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.ensurePairRecordingSchema(); err != nil {
		t.Fatal(err)
	}
	e := &pairRecTestEnv{t: t, db: db, clock: &simulatedClock{}, wall: 1_800_003_570_000}
	e.rec = newPairRecorder(pairRecLive, pairRecTestRun, pairRecTestPartition, pairRecTestContract, db, e.clock, pairRecFaults{})
	e.rec.schema = pairRecSchemaAsync
	e.rec.now = func() time.Time { return time.UnixMilli(atomic.LoadInt64(&e.wall)) }
	e.rec.epoch = 1
	bind := pairRecBind{RunID: pairRecTestRun, PaperIdentity: "id", ContractFP: pairRecTestContract, Partition: pairRecTestPartition, SeedSHA256: "seed", EvidenceIdentity: "ev", AnchorMs: 1, Family: pairPaperManifestAsyncVer}
	if err := e.rec.appendDirect(pairRecEvent{Kind: pairRecEventBind, Bind: &bind}, pairRecEvent{Kind: pairRecEventEpoch, Epoch: &pairRecEpoch{Epoch: 1, WallMs: 1, Mode: "new"}}); err != nil {
		t.Fatal(err)
	}
	e.rec.beginStep("s0@1", 0)
	if _, err := e.rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestBasketReplayPairPaperAsyncTerminalFailureRecoversOnce(t *testing.T) {
	for _, mode := range []string{"next wake", "failed clear", "restart"} {
		t.Run(mode, func(t *testing.T) {
			e := newPairAsyncRecordingEnv(t)
			f := newPairRecOracleFixture(t, 1_800_003_600_000)
			callID, start, err := e.rec.beginAsyncCall(pairRecCall{Kind: pairRecKindFundingOracle, MemberID: f.key.MemberID, SlotMs: f.target, Request: f.request(t)})
			if err != nil {
				t.Fatal(err)
			}
			w := &pairAsyncOracle{target: f.target, deadline: f.deadline, callID: callID, start: start, key: f.key, res: pairRecResult{Class: pairRecClassInfra, Cause: "fixture outcome"}, completion: f.target + 400}
			w.ready.Store(true)
			r := &pairReplayRuntime{rec: e.rec, oracle: w, oracleNextMs: f.target, paper: &pairPaperRun{manifest: pairPaperManifest{Version: pairPaperManifestAsyncVer}, funding: f.src, scenario: pairPaperScenario{FundingPolicy: &f.policy}}}
			e.rec.faults = pairRecFaults{kind: "fail-record-append", n: 1}
			var trace []string
			r.finishReadyOracle(pairRecWake{WallMs: f.target + 500}, &trace)
			if !w.recovered || w.terminal != nil || len(e.log().ends) != 0 {
				t.Fatal("a failed terminal keeps only the queued recovery")
			}
			if mode == "failed clear" {
				if _, err := e.rec.clearAtBoundary(func() (int64, error) { return 0, errors.New("verification unavailable") }); err == nil {
					t.Fatal("the first clear must fail")
				}
				r.finishReadyOracle(pairRecWake{WallMs: f.target + 600}, &trace)
				r.finalizeOracle()
				if len(e.log().ends) != 0 {
					t.Fatal("a blocked wake and shutdown never retry the terminal")
				}
				r.oracle = w
			}
			e.rec.faults = pairRecFaults{}
			if mode == "restart" {
				path := e.db.path
				if err := e.db.Close(); err != nil {
					t.Fatal(err)
				}
				e.db, err = OpenStateDB(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { e.db.Close() })
				l := e.log()
				rec := newPairRecorder(pairRecResume, pairRecTestRun, pairRecTestPartition, pairRecTestContract, e.db, e.clock, pairRecFaults{})
				rec.schema = pairRecSchemaAsync
				rec.head = l.head()
				rec.epoch = 2
				rec.now = e.rec.now
				if err := rec.appendDirect(pairRecEvent{Kind: pairRecEventRecovery, CallID: callID, Recovery: &pairRecRecovery{Reason: "interrupted by restart", Epoch: 2}}); err != nil {
					t.Fatal(err)
				}
				rec.adoptLog(e.log(), len(l.stream))
				rec.lastWake = e.rec.lastWake
				rec.stepID = e.rec.stepID
				r.rec, r.oracle = rec, nil
				var req pairPaperOracleRequest
				if err := pairReplayStrictDecode(start.Request, &req); err != nil {
					t.Fatal(err)
				}
				if err := r.consumeLoggedAttempt(pairRecWake{WallMs: f.target + 700}, pairPaperOracleAttempt{CallID: callID, TargetEventMs: f.target, AttemptID: req.AttemptID, Key: f.key}); err != nil {
					t.Fatal(err)
				}
				r.advanceOracleGrid()
			} else {
				if cleared, err := e.rec.clearAtBoundary(func() (int64, error) { return e.log().head().Seq, nil }); err != nil || !cleared {
					t.Fatalf("the next valid clear commits the recovery: %v", err)
				}
				r.finishReadyOracle(pairRecWake{WallMs: f.target + 700}, &trace)
			}
			l := e.log()
			if len(l.starts) != 1 || len(l.ends) != 1 || len(l.consumes) != 1 || l.rows[l.ends[callID]].Kind != pairRecEventRecovery || l.rows[l.consumes[callID]].Event.Consume.Disposition != "interrupted" || r.oracleNextMs != 0 {
				t.Fatalf("one start, recovery and interrupted consumption must advance the hour: starts %d ends %d consumes %d next %d", len(l.starts), len(l.ends), len(l.consumes), r.oracleNextMs)
			}
		})
	}
}

func TestBasketReplayPairPaperAsyncTimingUsesProductionDelivery(t *testing.T) {
	for _, mode := range []string{"live", "playback"} {
		for _, backward := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s backward=%v", mode, backward), func(t *testing.T) {
				e := newPairAsyncRecordingEnv(t)
				book := newPairTestEnv(t, nil)
				target := int64(1_800_003_600_000)
				policy := pairFundingPolicyV1{Version: pairFundingPolicyVersion, MemberRules: map[string]string{pairTestHL: pairFundingRuleHLReceiptResearch, pairTestOKX: pairFundingRuleOKX}, WindowMs: 5000, MaxPrintOffsetMs: 60000, PrepareLeadMs: 30000, FromMs: target - 120000, DrainThroughMs: target + 7200000}
				r := &pairReplayRuntime{rt: book.rt(), rec: e.rec, oracleNextMs: target, paper: &pairPaperRun{scenario: pairPaperScenario{FundingPolicy: &policy}}}
				wake := pairRecWake{WallMs: target + 5001}
				if backward {
					wake = pairRecWake{WallMs: target - 29000, ClockBackward: true}
				}
				var trace []string
				if mode == "live" {
					trace = r.deliverAsyncOracle(wake)
				} else {
					trace = r.playbackOracleMiss(wake)
				}
				if strings.Join(trace, ",") != "oracle_missed" || r.oracleNextMs != target+3600000 || len(e.log().starts) != 0 {
					t.Fatalf("a missed wake advances without a call start: trace %v next %d", trace, r.oracleNextMs)
				}
			})
		}
	}
}

func TestBasketReplayPairPaperPlaybackRestoresOnlyDurableFunding(t *testing.T) {
	e := newPairAsyncRecordingEnv(t)
	f := newPairRecOracleFixture(t, 1_800_003_600_000)
	if err := e.db.ensureBasketReplaySchema(); err != nil {
		t.Fatal(err)
	}
	if err := e.db.insertPairReplayRun(pairReplayManifest{RunID: pairRecTestRun}, nil, nil); err != nil {
		t.Fatal(err)
	}
	callID, start, err := e.rec.beginAsyncCall(pairRecCall{Kind: pairRecKindFundingOracle, MemberID: f.key.MemberID, SlotMs: f.target, Request: f.request(t)})
	if err != nil {
		t.Fatal(err)
	}
	batch := f.batch(t, f.target, f.target+100)
	decoded, err := pairDecodeFundingBatch(batch)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := pairCanonicalJSON(decoded.Research.Observations[0])
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&e.wall, f.target+200)
	if _, err := e.rec.wake("", 0); err != nil {
		t.Fatal(err)
	}
	beforeConsume := e.rec.cursorNow()
	end, err := e.rec.appendAsyncTerminal(callID, start, pairAsyncOracleResult{res: pairRecResult{Class: pairRecClassResult, Result: obs, ReceiptMs: f.target + 100}, completion: f.target + 400})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.rec.appendConsume(callID, pairRecConsume{RequestHash: start.RequestHash, TerminalHash: end.ResultHash, TerminalKind: pairRecEventEnd, WakeSeq: e.rec.lastWakeSeq(), StepID: "s0@1", LogicalMs: f.target + 500, Disposition: "accepted", Batch: batch, BatchHash: decoded.BatchHash}); err != nil {
		t.Fatal(err)
	}
	afterConsume := e.rec.cursorNow()
	rate := pfRate(f.key, pairFundingRuleHLReceiptResearch, f.target-60000, "0.0001")
	rate.VisibleMs = f.target - 59000
	rate, err = rate.sealed()
	if err != nil {
		t.Fatal(err)
	}
	rateBatch, err := (pairFundingBatchV2{V: pairFundingResearchBatchVersion, SourceID: f.src.SourceID(), Seq: 2, PrevHash: decoded.BatchHash, PolicyDigest: f.digest, Rates: []pairFundingRate{rate}, Prices: []pairFundingPrice{}, Observations: []pairFundingOracleObservation{}, Coverage: []pairFundingCoverageRecord{}}).sealed()
	if err != nil {
		t.Fatal(err)
	}
	rateRaw, err := pairFundingBatchValueBytes(pairFundingBatchV2Value(rateBatch))
	if err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&e.wall, f.target+1000)
	if _, _, err := e.rec.call(context.Background(), pairRecCall{Kind: pairRecKindFunding, MemberID: f.key.MemberID, Request: []byte(`{"rate":2}`)}, func(context.Context, *pairRecHandle) pairRecResult {
		return pairRecResult{Class: pairRecClassResult, Result: rateRaw}
	}); err != nil {
		t.Fatal(err)
	}
	l := e.log()
	t.Run("last stream item is a consumption", func(t *testing.T) {
		ci := l.consumes[callID]
		tail, err := verifyPairRecLog(append([]pairRecRow(nil), l.rows[:ci+1]...), pairRecTestRun, pairRecTestPartition, pairRecTestContract)
		if err != nil {
			t.Fatal(err)
		}
		attempts, err := pairPaperOracleAttemptIndex(tail)
		if err != nil {
			t.Fatal(err)
		}
		for _, attempt := range attempts {
			if attempt.ConsumeStream != len(tail.stream)-1 {
				t.Fatalf("the final consumption has stream position %d, got %d", len(tail.stream)-1, attempt.ConsumeStream)
			}
		}
		for _, frontier := range []int{len(tail.stream) - 1, len(tail.stream)} {
			src, err := newPairRecordedFundingSourceWithPolicy(f.src.SourceID(), f.policy.FromMs, f.policy)
			if err != nil {
				t.Fatal(err)
			}
			r := &pairReplayRuntime{paper: &pairPaperRun{funding: src}}
			if err := r.restoreAsyncFunding(tail, frontier); err != nil {
				t.Fatal(err)
			}
			if want := frontier - len(tail.stream) + 1; len(src.chain()) != want {
				t.Fatalf("frontier %d restores %d batches, got %d", frontier, want, len(src.chain()))
			}
		}
	})
	for _, c := range []struct {
		name                                   string
		frontier, batches, observations, rates int
	}{
		{"new state", 0, 0, 0, 0},
		{"receipt before exit", beforeConsume, 0, 0, 0},
		{"one consumed sample before rate read", afterConsume, 1, 1, 0},
		{"all consumed", len(l.stream), 2, 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := e.db.db.Exec(`UPDATE basket_replay_runs SET recording_cursor = ? WHERE run_id = ?`, c.frontier, pairRecTestRun); err != nil {
				t.Fatal(err)
			}
			src, err := newPairRecordedFundingSourceWithPolicy(f.src.SourceID(), f.policy.FromMs, f.policy)
			if err != nil {
				t.Fatal(err)
			}
			rec := newPairRecorder(pairRecPlayback, pairRecTestRun, pairRecTestPartition, pairRecTestContract, e.db, &simulatedClock{}, pairRecFaults{})
			rec.adoptLog(l, c.frontier)
			r := &pairReplayRuntime{spec: &pairReplaySpec{}, rec: rec, paper: &pairPaperRun{manifest: pairPaperManifest{Version: pairPaperManifestAsyncVer}, funding: src}}
			r.spec.Run.RunID = pairRecTestRun
			if err := r.playbackFundingBeforeAdmission(filepath.Dir(e.db.path), pairRecordingManifest{Partition: pairRecTestPartition}); err != nil {
				t.Fatal(err)
			}
			if err := r.restoreSourcesFrom(l, -1); err != nil {
				t.Fatal(err)
			}
			got, err := src.Collect(context.Background(), pairFundingRequest{Key: f.key, FromMs: f.policy.FromMs, ThroughMs: f.target + 200, CutoffMs: f.target + 200})
			if err != nil || len(src.chain()) != c.batches || len(got.Observations) != c.observations || len(got.Rates) != c.rates {
				t.Fatalf("frontier %d: batches %d observations %d rates %d: %v", c.frontier, len(src.chain()), len(got.Observations), len(got.Rates), err)
			}
			if c.frontier == beforeConsume {
				if trace := r.playAsyncOracle(pairRecWake{WallMs: f.target + 500}); strings.Join(trace, ",") != "funding_oracle" || len(src.chain()) != 1 {
					t.Fatalf("the remaining sample appears at its consumption: trace %v batches %d", trace, len(src.chain()))
				}
			}
		})
	}
}
