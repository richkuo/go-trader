package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
