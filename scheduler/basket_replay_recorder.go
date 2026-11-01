package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type pairPaperWallSource interface {
	Now() time.Time
	Sleep(d time.Duration)
	Tick()
}

type pairPaperRealWall struct{}

func (pairPaperRealWall) Now() time.Time        { return time.Now() }
func (pairPaperRealWall) Sleep(d time.Duration) { time.Sleep(d) }
func (pairPaperRealWall) Tick()                 {}

var pairPaperWall pairPaperWallSource = pairPaperRealWall{}

type pairRecMode int

const (
	pairRecLive pairRecMode = iota
	pairRecResume
	pairRecPlayback
)

type pairRecCall struct {
	Kind     string
	Phase    string
	MemberID string
	SlotMs   int64
	ActionID string
	Request  []byte
}

type pairRecResult struct {
	Class      string
	Result     []byte
	ReceiptMs  int64
	Cause      string
	ExitStatus int
	Stdout     []byte
	Stderr     string
	Overflow   bool
}

type pairRecSaved struct {
	CallID    string
	Start     pairRecStart
	End       *pairRecEnd
	Recovered bool
	Pending   bool
	Failure   *pairRecFailure
	Replayed  bool
}

type pairPaperOracleAttempt struct {
	Key           pairEvidenceKey
	TargetEventMs int64
	PolicyDigest  string
	AttemptID     string
	CallID        string
	StartSeq      int64
	EndSeq        int64
	StreamPos     int
	ConsumeStream int
	Async         bool
	Disposition   string
}

type pairRecHandle struct {
	rec      *pairRecorder
	parentID string
	children int
	start    pairRecStart
}

var errPairRecDiverged = errors.New("the recorded call sequence diverged")

var pairRecTestAppendFault func() error

type pairRecNotMadeError struct{ err error }

func (e pairRecNotMadeError) Error() string { return e.err.Error() }

func (e pairRecNotMadeError) Unwrap() error { return e.err }

func pairRecCallNotMade(err error) bool {
	var nm pairRecNotMadeError
	return errors.As(err, &nm)
}

type pairRecFaults struct {
	kind string
	n    int
}

type pairRecorder struct {
	mu         sync.Mutex
	mode       pairRecMode
	runID      string
	partition  string
	contractFP string
	file       *StateDB
	head       pairRecHead
	log        *pairRecLog
	cursor     int
	logical    int64
	rawHigh    int64
	clock      *simulatedClock
	epoch      int
	stepID     string
	ordinal    int
	bootN      int

	blocked     atomic.Bool
	blockReason string
	failedSeq   int64
	queue       []pairRecEvent

	fundingLog     *pairRecLog
	fundingPos     map[string]int
	oracleLog      *pairRecLog
	attempts       map[string]pairPaperOracleAttempt
	lastWakeRecord *pairRecWake
	lastWakePrefix string

	backward     atomic.Bool
	refusals     []string
	diverged     string
	faults       pairRecFaults
	counts       map[string]int
	now          func() time.Time
	tick         func()
	lastWake     int64
	skipped      int
	faultSaid    bool
	schema       int
	beforeExit   func()
	books        *pairPaperBooks
	encodedBytes int64
	limitStopped atomic.Bool
}

func newPairRecorder(mode pairRecMode, runID, partition, contractFP string, file *StateDB, clock *simulatedClock, faults pairRecFaults) *pairRecorder {
	return &pairRecorder{mode: mode, runID: runID, partition: partition, contractFP: contractFP, file: file, clock: clock, faults: faults, counts: map[string]int{},
		now: func() time.Time { return pairPaperWall.Now() }, tick: func() { pairPaperWall.Tick() }}
}

func (rec *pairRecorder) fault(kind string) {
	if rec.faults.kind != kind {
		return
	}
	rec.counts[kind]++
	if rec.counts[kind] == rec.faults.n {
		pairReplayFaultMessage(kind, rec.faults.n)
		if rec.beforeExit != nil {
			rec.beforeExit()
		}
		os.Exit(basketReplayExitFault)
	}
}

func (rec *pairRecorder) failNextAppend() bool {
	if rec.faults.kind != "fail-record-append" {
		return false
	}
	rec.counts["fail-record-append"]++
	return rec.counts["fail-record-append"] >= rec.faults.n
}

func (rec *pairRecorder) refuse(format string, a ...interface{}) {
	rec.refusals = append(rec.refusals, fmt.Sprintf(format, a...))
}

func (rec *pairRecorder) equalityReasons() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := append([]string(nil), rec.refusals...)
	if rec.diverged != "" {
		out = append(out, rec.diverged)
	}
	return out
}

func (rec *pairRecorder) setLogical(ms int64) {
	if ms > rec.logical {
		rec.logical = ms
	}
	if rec.clock != nil && rec.clock.Now().UnixMilli() < rec.logical {
		_ = rec.clock.Set(time.UnixMilli(rec.logical).UTC())
	}
}

func (rec *pairRecorder) nextLogical(raw int64) int64 {
	l := rec.logical + 1
	if raw > l {
		l = raw
	}
	return l
}

func (rec *pairRecorder) noteRaw(raw int64) bool {
	back := raw < rec.rawHigh
	if raw > rec.rawHigh {
		rec.rawHigh = raw
	}
	return back
}

func (rec *pairRecorder) appendLocked(events ...pairRecEvent) error {
	return rec.appendLockedContext(context.Background(), events...)
}

func (rec *pairRecorder) appendLockedContext(ctx context.Context, events ...pairRecEvent) error {
	if rec.mode == pairRecPlayback {
		return fmt.Errorf("a playback never appends to the recording")
	}
	if rec.file == nil {
		return fmt.Errorf("the recorder has no partition file")
	}
	added, err := rec.bookAppendBudget(events)
	if err != nil {
		return err
	}
	fail := false
	next, err := rec.file.appendPairRecEventsContext(ctx, rec.runID, rec.partition, rec.contractFP, rec.head, rec.schemaVersion(), events, func(int) error {
		if rec.failNextAppend() {
			fail = true
			return fmt.Errorf("injected fault fail-record-append:%d", rec.faults.n)
		}
		if pairRecTestAppendFault != nil {
			return pairRecTestAppendFault()
		}
		return nil
	})
	if err != nil {
		if fail && !rec.faultSaid {
			rec.faultSaid = true
			fmt.Fprintf(os.Stderr, "fault fail-record-append:%d\n", rec.faults.n)
		}
		return err
	}
	rec.head = next
	rec.encodedBytes += added
	for _, ev := range events {
		if pairRecConsumableEvent(ev) {
			rec.cursor++
		}
	}
	for range events {
		rec.fault("exit-after-input-commit")
	}
	return nil
}

func pairRecConsumableEvent(ev pairRecEvent) bool {
	switch ev.Kind {
	case pairRecEventWake, pairRecEventBudget, pairRecEventFailure, pairRecEventClear, pairRecEventConsume:
		return true
	case pairRecEventStart:
		return ev.Start != nil && ev.Start.ParentID == "" && ev.Start.Phase == pairRecPhaseStep
	}
	return false
}

func (rec *pairRecorder) schemaVersion() int {
	if rec != nil && pairRecAsyncSchema(rec.schema) {
		return rec.schema
	}
	return pairRecSchemaVersion
}

func (rec *pairRecorder) enqueueLocked(ev pairRecEvent) {
	if rec.schemaVersion() == pairRecSchemaBooks && len(rec.queue) >= 64 {
		rec.limitStopped.Store(true)
		return
	}
	rec.queue = append(rec.queue, ev)
}

func (rec *pairRecorder) block(reason string) {
	if rec.blocked.Load() {
		return
	}
	rec.blockReason = reason
	rec.failedSeq = rec.head.Seq + 1
	rec.blocked.Store(true)
	fmt.Fprintf(os.Stderr, "[paper] CRITICAL: the recording append failed at input %d (%s); new decisions and increases hold until an explicit verification of the committed record clears the barrier; settlement, protection and emergency closes run only on inputs committed before the failure, because no new poll, mark, candle or member check is read\n", rec.failedSeq, reason)
}

func (rec *pairRecorder) barrier() (bool, string, int64) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.blocked.Load(), rec.blockReason, rec.failedSeq
}

func (rec *pairRecorder) beginStep(stepID string, ordinal int) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.stepID, rec.ordinal = stepID, ordinal
	rec.backward.Store(false)
}

func (rec *pairRecorder) callOrdinal() (int, string) {
	rec.ordinal++
	return rec.ordinal, rec.stepID + "/" + strconv.Itoa(rec.ordinal)
}

func (rec *pairRecorder) stepOrdinal() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.ordinal
}

func (rec *pairRecorder) streamItem() (*pairRecRow, bool) {
	if rec.log == nil || rec.cursor >= len(rec.log.stream) {
		return nil, false
	}
	return &rec.log.rows[rec.log.stream[rec.cursor]], true
}

func (rec *pairRecorder) consuming() bool {
	if rec.mode == pairRecPlayback {
		return true
	}
	if rec.mode == pairRecResume {
		if _, ok := rec.streamItem(); ok {
			return true
		}
		rec.mode = pairRecLive
	}
	return false
}

func (rec *pairRecorder) consumingNow() bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.consuming()
}

func (rec *pairRecorder) diverge(format string, a ...interface{}) error {
	msg := fmt.Sprintf(format, a...)
	if rec.diverged == "" {
		rec.diverged = "the recorded call sequence diverged: " + msg
	}
	return fmt.Errorf("%w: %s", errPairRecDiverged, msg)
}

func (rec *pairRecorder) call(ctx context.Context, c pairRecCall, live func(ctx context.Context, h *pairRecHandle) pairRecResult) (pairRecResult, *pairRecSaved, error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if c.Phase == "" {
		c.Phase = pairRecPhaseStep
	}
	reqHash := sha256Hex(c.Request)
	if c.Phase == pairRecPhaseStep && rec.consuming() {
		rec.skipClearsLocked()
		if rec.mode == pairRecResume && !rec.seekLocked(func(row *pairRecRow) bool { return pairRecMatchesCall(row, c, reqHash) }) {
			if rec.consuming() {
				if item, _ := rec.streamItem(); item != nil && item.Kind == pairRecEventWake {
					return pairRecResult{}, nil, rec.diverge("the resumed step %s requests a %s call for %q that its committed tail never holds before the next recorded wake at input %d", rec.stepID, c.Kind, c.MemberID, item.Seq)
				}
			}
		}
		if rec.consuming() {
			return rec.consumeCallLocked(ctx, c, reqHash, live)
		}
	}
	var ordinal int
	var callID string
	if c.Phase == pairRecPhaseBootstrap {
		rec.bootN++
		ordinal, callID = rec.bootN, fmt.Sprintf("boot%d/%d", rec.epoch, rec.bootN)
	} else {
		ordinal, callID = rec.callOrdinal()
	}
	if rec.blocked.Load() {
		rec.enqueueLocked(pairRecEvent{Kind: pairRecEventFailure, CallID: callID, Failure: &pairRecFailure{CallKind: c.Kind, MemberID: c.MemberID, RequestHash: reqHash,
			StepID: rec.stepID, Ordinal: ordinal, Reason: "the recorder was blocked, so the call was refused before any read: " + rec.blockReason, FailedSeq: rec.failedSeq}})
		return pairRecResult{}, nil, pairRecNotMadeError{fmt.Errorf("the %s call was not made: the recording barrier refuses every new outside read until the committed record is verified (%s)", c.Kind, rec.blockReason)}
	}
	rawStart := rec.now().UnixMilli()
	start := pairRecStart{Kind: c.Kind, Phase: c.Phase, MemberID: c.MemberID, SlotMs: c.SlotMs, ActionID: c.ActionID, StepID: rec.stepID, Ordinal: ordinal,
		Request: c.Request, RequestHash: reqHash, RawStartMs: rawStart, LogicalBeforeMs: rec.logical}
	if pairPaperBookCall(c.Kind) {
		head := rec.head
		start.BookPrefix = &head
		if rec.books != nil {
			intake := rec.books.intakeNow()
			start.BookIntake = &intake
		}
	}
	if err := rec.appendLocked(pairRecEvent{Kind: pairRecEventStart, CallID: callID, Start: &start}); err != nil {
		rec.block(err.Error())
		rec.enqueueLocked(pairRecEvent{Kind: pairRecEventFailure, CallID: callID, Failure: &pairRecFailure{CallKind: c.Kind, MemberID: c.MemberID, RequestHash: reqHash,
			StepID: rec.stepID, Ordinal: ordinal, Reason: "the call start did not commit, so the call was never made: " + err.Error(), FailedSeq: rec.failedSeq}})
		return pairRecResult{}, nil, pairRecNotMadeError{fmt.Errorf("the %s call start could not be recorded, so no outside read was made: %w", c.Kind, err)}
	}
	rec.fault("exit-after-call-start")
	h := &pairRecHandle{rec: rec, parentID: callID, start: start}
	rec.mu.Unlock()
	res := live(ctx, h)
	rec.mu.Lock()
	if rec.tick != nil {
		rec.tick()
	}
	return rec.finishLocked(callID, start, res)
}

func (rec *pairRecorder) finishLocked(callID string, start pairRecStart, res pairRecResult) (pairRecResult, *pairRecSaved, error) {
	if res.Class == "" {
		res.Class = pairRecClassResult
	}
	rawEnd := rec.now().UnixMilli()
	back := rec.noteRaw(rawEnd) || rawEnd < start.RawStartMs
	logical := rec.nextLogical(rawEnd)
	if res.ReceiptMs > logical {
		logical = res.ReceiptMs
	}
	end := &pairRecEnd{Class: res.Class, Result: res.Result, ResultHash: pairRecResultHash(res.Result), RawReceiptMs: res.ReceiptMs, RawCompletionMs: rawEnd, LogicalMs: logical,
		Cause: res.Cause, ExitStatus: res.ExitStatus, Stdout: pairRecBound(res.Stdout, pairRecMaxStdoutBytes), Stderr: string(pairRecBound([]byte(res.Stderr), pairRecMaxStderrBytes)),
		Overflow: res.Overflow || len(res.Stdout) > pairRecMaxStdoutBytes, ClockBackward: back}
	if err := rec.appendLocked(pairRecEvent{Kind: pairRecEventEnd, CallID: callID, End: end}); err != nil {
		rec.block(err.Error())
		rec.enqueueLocked(pairRecEvent{Kind: pairRecEventRecovery, CallID: callID, Recovery: &pairRecRecovery{Reason: "the outcome was received but its terminal record did not commit, so it was never used: " + err.Error(), Epoch: rec.epoch}})
		return pairRecResult{}, nil, fmt.Errorf("the %s call outcome could not be recorded, so it is not used: %w", start.Kind, err)
	}
	if start.Phase == pairRecPhaseStep && start.ParentID == "" {
		rec.setLogical(logical)
	}
	if back {
		rec.backward.Store(true)
	}
	return res, &pairRecSaved{CallID: callID, Start: start, End: end}, nil
}

func (h *pairRecHandle) child(ctx context.Context, c pairRecCall, live func(ctx context.Context) pairRecResult) (pairRecResult, string, error) {
	rec := h.rec
	rec.mu.Lock()
	h.children++
	callID := fmt.Sprintf("%s.%d", h.parentID, h.children)
	ordinal := h.children
	if rec.blocked.Load() {
		rec.mu.Unlock()
		return pairRecResult{}, callID, fmt.Errorf("the %s read was not made: the recording barrier holds", c.Kind)
	}
	reqHash := sha256Hex(c.Request)
	start := pairRecStart{ParentID: h.parentID, Kind: c.Kind, Phase: pairRecPhaseStep, MemberID: c.MemberID, SlotMs: c.SlotMs, StepID: rec.stepID, Ordinal: ordinal,
		Request: c.Request, RequestHash: reqHash, RawStartMs: rec.now().UnixMilli(), LogicalBeforeMs: rec.logical}
	if err := rec.appendLocked(pairRecEvent{Kind: pairRecEventStart, CallID: callID, Start: &start}); err != nil {
		rec.block(err.Error())
		rec.mu.Unlock()
		return pairRecResult{}, callID, fmt.Errorf("the %s read start could not be recorded, so it was not made: %w", c.Kind, err)
	}
	rec.fault("exit-after-call-start")
	rec.mu.Unlock()
	res := live(ctx)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out, _, err := rec.finishLocked(callID, start, res)
	return out, callID, err
}

func pairRecBound(b []byte, max int) []byte {
	if len(b) <= max {
		return b
	}
	return append([]byte(nil), b[:max]...)
}

func (rec *pairRecorder) consumeCallLocked(ctx context.Context, c pairRecCall, reqHash string, live func(ctx context.Context, h *pairRecHandle) pairRecResult) (pairRecResult, *pairRecSaved, error) {
	item, ok := rec.streamItem()
	ordinal, _ := rec.callOrdinal()
	switch {
	case !ok:
		return pairRecResult{}, nil, rec.diverge("call %d of step %s (%s for %q) has no recorded call: the recording ends before it", ordinal, rec.stepID, c.Kind, c.MemberID)
	case item.Kind == pairRecEventFailure:
		f := item.Event.Failure
		if f.CallKind != c.Kind || f.MemberID != c.MemberID || f.RequestHash != reqHash {
			return pairRecResult{}, nil, rec.diverge("call %d of step %s is %s for %q with request %s, the record holds a refused %s for %q with request %s", ordinal, rec.stepID, c.Kind, c.MemberID, shortFingerprint(reqHash), f.CallKind, f.MemberID, shortFingerprint(f.RequestHash))
		}
		rec.cursor++
		if !rec.blocked.Load() {
			rec.blockReason, rec.failedSeq = f.Reason, f.FailedSeq
			rec.blocked.Store(true)
		}
		return pairRecResult{}, &pairRecSaved{CallID: item.CallID, Failure: f}, pairRecNotMadeError{fmt.Errorf("the %s call was not made in the recorded run: %s", c.Kind, f.Reason)}
	case item.Kind != pairRecEventStart:
		return pairRecResult{}, nil, rec.diverge("call %d of step %s (%s for %q) meets the recorded %s event at input %d", ordinal, rec.stepID, c.Kind, c.MemberID, item.Kind, item.Seq)
	}
	s := item.Event.Start
	if s.Kind != c.Kind || s.MemberID != c.MemberID || s.RequestHash != reqHash || s.SlotMs != c.SlotMs || s.ActionID != c.ActionID {
		return pairRecResult{}, nil, rec.diverge("call %d of step %s is %s for %q (slot %d, action %q, request %s %s), the record holds %s for %q (slot %d, action %q, request %s %s) at input %d",
			ordinal, rec.stepID, c.Kind, c.MemberID, c.SlotMs, c.ActionID, shortFingerprint(reqHash), pairRecBrief(c.Request), s.Kind, s.MemberID, s.SlotMs, s.ActionID, shortFingerprint(s.RequestHash), pairRecBrief(s.Request), item.Seq)
	}
	if rec.mode == pairRecPlayback && (s.StepID != rec.stepID || s.Ordinal != ordinal) {
		return pairRecResult{}, nil, rec.diverge("call %s is recorded as call %d of step %s, the playback reaches it as call %d of step %s", item.CallID, s.Ordinal, s.StepID, ordinal, rec.stepID)
	}
	rec.cursor++
	rec.ordinal = s.Ordinal
	saved := &pairRecSaved{CallID: item.CallID, Start: *s}
	if s.Async {
		saved.Pending = true
		return pairRecResult{}, saved, nil
	}
	ei, ok := rec.log.ends[item.CallID]
	if !ok {
		if rec.mode == pairRecPlayback {
			return pairRecResult{}, saved, rec.diverge("call %s has no terminal or recovery event in a sealed recording", item.CallID)
		}
		ev := pairRecEvent{Kind: pairRecEventRecovery, CallID: item.CallID, Recovery: &pairRecRecovery{Reason: "the process stopped after the call start committed and before its outcome committed; the result is unknown and the call is never repeated", Epoch: rec.epoch}}
		if err := rec.appendLocked(ev); err != nil {
			rec.block(err.Error())
			return pairRecResult{}, saved, fmt.Errorf("the interrupted %s call %s could not be marked unknown: %w", c.Kind, item.CallID, err)
		}
		rec.log.ends[item.CallID] = -1
		saved.Recovered = true
		return pairRecResult{}, saved, fmt.Errorf("the %s call %s was interrupted by a restart after it started; its result is unknown and it is never repeated", c.Kind, item.CallID)
	}
	if ei < 0 || rec.log.rows[ei].Kind == pairRecEventRecovery {
		saved.Recovered = true
		return pairRecResult{}, saved, fmt.Errorf("the %s call %s has a recorded unknown outcome (interrupted); it is never repeated", c.Kind, item.CallID)
	}
	end := rec.log.rows[ei].Event.End
	saved.End = end
	if end.ClockBackward {
		rec.backward.Store(true)
	}
	res := pairRecResult{Class: end.Class, Result: end.Result, ReceiptMs: end.RawReceiptMs, Cause: end.Cause, ExitStatus: end.ExitStatus, Stdout: end.Stdout, Stderr: end.Stderr, Overflow: end.Overflow}
	if rec.mode == pairRecPlayback && pairRecProcessKinds[c.Kind] && end.Class == pairRecClassResult {
		saved.Replayed = true
		rec.mu.Unlock()
		again := live(ctx, &pairRecHandle{rec: rec, parentID: item.CallID})
		rec.mu.Lock()
		if again.Class != pairRecClassResult {
			rec.refuse("the %s call %s recorded a result, but its offline rerun ended %s (%s); the saved result is kept and no equality is claimed", c.Kind, item.CallID, again.Class, again.Cause)
		} else if got := pairRecResultHash(again.Result); got != end.ResultHash {
			rec.refuse("the %s call %s rerun from its saved request produced result %s, the recording holds %s", c.Kind, item.CallID, shortFingerprint(got), shortFingerprint(end.ResultHash))
		}
	}
	rec.setLogical(end.LogicalMs)
	return res, saved, nil
}

func (rec *pairRecorder) budget(site string, ctx context.Context, wallStart time.Time) (time.Duration, error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.mode == pairRecResume && rec.consuming() {
		rec.seekLocked(func(row *pairRecRow) bool { return row.Kind == pairRecEventBudget && row.Event.Budget.Site == site })
	}
	if rec.consuming() {
		rec.skipClearsLocked()
	}
	if rec.consuming() {
		item, ok := rec.streamItem()
		if !ok || item.Kind != pairRecEventBudget || item.Event.Budget.Site != site {
			if !ok {
				_ = rec.diverge("the %s budget disposition of step %s has no recorded event: the recording ends before it", site, rec.stepID)
				return rec.now().Sub(wallStart), ctx.Err()
			}
			_ = rec.diverge("the %s budget disposition of step %s meets the recorded %s event at input %d", site, rec.stepID, item.Kind, item.Seq)
			return rec.now().Sub(wallStart), ctx.Err()
		}
		rec.cursor++
		b := item.Event.Budget
		return time.Duration(b.ElapsedNs), pairRecCauseError(b.Cause)
	}
	elapsed, err := rec.now().Sub(wallStart), ctx.Err()
	cause := ""
	if err != nil {
		cause = err.Error()
	}
	ev := pairRecEvent{Kind: pairRecEventBudget, Budget: &pairRecBudget{Site: site, ElapsedNs: int64(elapsed), Cause: cause, StepID: rec.stepID}}
	if rec.blocked.Load() {
		rec.enqueueLocked(ev)
		return elapsed, err
	}
	if aerr := rec.appendLocked(ev); aerr != nil {
		rec.block(aerr.Error())
		rec.enqueueLocked(ev)
	}
	return elapsed, err
}

func pairRecCauseError(cause string) error {
	switch cause {
	case "":
		return nil
	case context.DeadlineExceeded.Error():
		return context.DeadlineExceeded
	case context.Canceled.Error():
		return context.Canceled
	}
	return errors.New(cause)
}

func (rec *pairRecorder) wake(stepID string, ordinal int) (int64, error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.consuming() {
		item, ok := rec.streamItem()
		if !ok {
			return 0, rec.diverge("host wake %d has no recorded wake: the recording ends before it", ordinal)
		}
		if item.Kind != pairRecEventWake || item.Event.Wake.Ordinal != ordinal {
			return 0, rec.diverge("host wake %d meets the recorded %s event at input %d", ordinal, item.Kind, item.Seq)
		}
		rec.cursor++
		w := item.Event.Wake
		if w.ClockBackward {
			rec.backward.Store(true)
		}
		rec.lastWake = item.Seq
		rec.lastWakeRecord = w
		rec.lastWakePrefix = item.Prefix
		rec.setLogical(w.LogicalMs)
		return w.LogicalMs, nil
	}
	wall := rec.now().UnixMilli()
	back := rec.noteRaw(wall)
	logical := rec.nextLogical(wall)
	ev := pairRecEvent{Kind: pairRecEventWake, Wake: &pairRecWake{StepID: stepID, Ordinal: ordinal, WallMs: wall, LogicalMs: logical, ClockBackward: back, Epoch: rec.epoch}}
	rec.lastWake = 0
	rec.lastWakeRecord = ev.Wake
	rec.lastWakePrefix = ""
	if rec.blocked.Load() {
		rec.enqueueLocked(ev)
	} else if err := rec.appendLocked(ev); err != nil {
		rec.block(err.Error())
		rec.enqueueLocked(ev)
	} else {
		rec.lastWake = rec.head.Seq
		rec.lastWakePrefix = rec.head.Prefix
	}
	if back {
		rec.backward.Store(true)
		fmt.Fprintf(os.Stderr, "[paper] the wall clock moved back to %d below the recorded high-water %d; logical time stays at %d and new increases hold this cycle\n", wall, rec.rawHigh, logical)
	}
	rec.setLogical(logical)
	return logical, nil
}

func (rec *pairRecorder) wakeAt(seq int64) (pairRecWake, error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if seq > 0 && seq == rec.lastWake && rec.lastWakeRecord != nil {
		return *rec.lastWakeRecord, nil
	}
	if rec.log != nil {
		for _, row := range rec.log.rows {
			if row.Seq == seq && row.Kind == pairRecEventWake && row.Event.Wake != nil {
				return *row.Event.Wake, nil
			}
		}
	}
	return pairRecWake{}, fmt.Errorf("host wake at recording sequence %d is not committed", seq)
}

func (rec *pairRecorder) queuedWake() (pairRecWake, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.mode != pairRecLive || rec.lastWake != 0 || rec.lastWakeRecord == nil || !rec.blocked.Load() {
		return pairRecWake{}, false
	}
	return *rec.lastWakeRecord, true
}

func (rec *pairRecorder) clearAtBoundary(verify func() (int64, error)) (bool, error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.consuming() {
		item, ok := rec.streamItem()
		if !ok || item.Kind != pairRecEventClear {
			return false, nil
		}
		rec.cursor++
		rec.blocked.Store(false)
		rec.blockReason, rec.failedSeq = "", 0
		return true, nil
	}
	if !rec.blocked.Load() {
		return false, nil
	}
	through, err := verify()
	if err != nil {
		return false, fmt.Errorf("the committed record failed its verification, so the barrier stays: %w", err)
	}
	events := append([]pairRecEvent(nil), rec.queue...)
	events = append(events, pairRecEvent{Kind: pairRecEventClear, Clear: &pairRecClear{VerifiedThroughSeq: through, FailedSeq: rec.failedSeq, Reason: rec.blockReason, Epoch: rec.epoch}})
	if err := rec.appendLocked(events...); err != nil {
		return false, fmt.Errorf("the barrier release could not be recorded, so the barrier stays: %w", err)
	}
	rec.queue = nil
	rec.blocked.Store(false)
	rec.blockReason, rec.failedSeq = "", 0
	return true, nil
}

func (rec *pairRecorder) skipClearsLocked() {
	if rec.mode != pairRecPlayback {
		return
	}
	for {
		item, ok := rec.streamItem()
		if !ok || item.Kind != pairRecEventClear {
			return
		}
		rec.cursor++
		rec.blocked.Store(false)
		rec.blockReason, rec.failedSeq = "", 0
	}
}

func (rec *pairRecorder) increaseHold() string {
	if rec.blocked.Load() {
		return "the recording barrier holds every increase: an outside input could not be committed before use"
	}
	if rec.backward.Load() {
		return "the wall clock moved backward during this cycle, so new increases hold while logical time stays monotonic"
	}
	return ""
}

func (rec *pairRecorder) appendDirect(events ...pairRecEvent) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.appendLocked(events...)
}

func (rec *pairRecorder) headNow() pairRecHead {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.head
}

func (rec *pairRecorder) cursorNow() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.cursor
}

func (rec *pairRecorder) logicalNow() int64 {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.logical
}

func (rec *pairRecorder) restorePosition(cursor, ordinal int, logical int64, stepID string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.cursor, rec.ordinal = cursor, ordinal
	if stepID != "" {
		rec.stepID = stepID
	}
	rec.logical = logical
	if rec.clock != nil && rec.clock.Now().UnixMilli() < logical {
		_ = rec.clock.Set(time.UnixMilli(logical).UTC())
	}
	if cursor > 0 && rec.log != nil && cursor <= len(rec.log.stream) {
		for i := cursor - 1; i >= 0; i-- {
			row := rec.log.rows[rec.log.stream[i]]
			if row.Kind == pairRecEventWake {
				rec.lastWake, rec.lastWakeRecord, rec.lastWakePrefix = row.Seq, row.Event.Wake, row.Prefix
				break
			}
		}
	}
	if rec.mode == pairRecResume && (rec.log == nil || rec.cursor >= len(rec.log.stream)) {
		rec.mode = pairRecLive
	}
}

func (rec *pairRecorder) adoptLog(l *pairRecLog, prevCursor int) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if l.schema == pairRecSchemaBooks {
		rec.encodedBytes = pairPaperEncodedLogBytes(l)
	}
	rec.log = l
	if rec.mode != pairRecLive {
		rec.cursor = prevCursor
	}
}

func (rec *pairRecorder) wakeSeq() int64 {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.lastWake
}

func (rec *pairRecorder) prefixAt(seq int64) string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.log != nil {
		if p, ok := rec.log.prefixAt(seq); ok && seq <= int64(len(rec.log.rows)) {
			return p
		}
	}
	if seq > 0 && seq == rec.lastWake {
		return rec.lastWakePrefix
	}
	if seq == rec.head.Seq {
		return rec.head.Prefix
	}
	return ""
}

func (rec *pairRecorder) peekWakeAfterClear() (int64, int, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.log == nil {
		return 0, 0, false
	}
	for i := rec.cursor; i < len(rec.log.stream); i++ {
		row := rec.log.rows[rec.log.stream[i]]
		switch row.Kind {
		case pairRecEventClear:
			continue
		case pairRecEventWake:
			return row.Event.Wake.LogicalMs, row.Event.Wake.Ordinal, true
		}
		return 0, 0, false
	}
	return 0, 0, false
}

func (rec *pairRecorder) unconsumed() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.log == nil {
		return nil
	}
	var out []string
	for i := rec.cursor; i < len(rec.log.stream); i++ {
		row := rec.log.rows[rec.log.stream[i]]
		label := row.Kind
		if row.Event.Start != nil {
			label = row.Event.Start.Kind + " call " + row.CallID
		}
		out = append(out, fmt.Sprintf("input %d (%s)", row.Seq, label))
	}
	return out
}

func (rec *pairRecorder) divergedReason() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.diverged
}

func pairRecBrief(b []byte) string {
	if len(b) > 240 {
		return string(b[:240]) + "..."
	}
	return string(b)
}

func pairRecMatchesCall(row *pairRecRow, c pairRecCall, reqHash string) bool {
	switch row.Kind {
	case pairRecEventStart:
		s := row.Event.Start
		return s.Kind == c.Kind && s.MemberID == c.MemberID && s.SlotMs == c.SlotMs && s.ActionID == c.ActionID && s.RequestHash == reqHash
	case pairRecEventFailure:
		f := row.Event.Failure
		return f.CallKind == c.Kind && f.MemberID == c.MemberID && f.RequestHash == reqHash
	}
	return false
}

func (rec *pairRecorder) seekLocked(match func(row *pairRecRow) bool) bool {
	if rec.log == nil {
		return false
	}
	for i := rec.cursor; i < len(rec.log.stream); i++ {
		row := &rec.log.rows[rec.log.stream[i]]
		if row.Kind == pairRecEventWake {
			rec.skipTo(i)
			return false
		}
		if match(row) {
			rec.skipTo(i)
			return true
		}
	}
	rec.skipTo(len(rec.log.stream))
	rec.mode = pairRecLive
	return false
}

func (rec *pairRecorder) skipTo(i int) {
	for j := rec.cursor; j < i; j++ {
		row := &rec.log.rows[rec.log.stream[j]]
		if row.Kind == pairRecEventConsume && row.Event.Consume != nil {
			rec.setLogical(row.Event.Consume.LogicalMs)
			continue
		}
		if row.Kind != pairRecEventStart {
			continue
		}
		if st := row.Event.Start; st != nil && st.Phase == pairRecPhaseStep && st.StepID == rec.stepID && st.Ordinal > rec.ordinal {
			rec.ordinal = st.Ordinal
		}
		if row.Event.Start != nil && row.Event.Start.Async {
			continue
		}
		if ei, ok := rec.log.ends[row.CallID]; ok && ei >= 0 {
			if end := rec.log.rows[ei].Event.End; end != nil {
				rec.setLogical(end.LogicalMs)
				if end.ClockBackward {
					rec.backward.Store(true)
				}
			}
		}
	}
	rec.skipped += i - rec.cursor
	rec.cursor = i
}

func (rec *pairRecorder) catchUpCollect(slotMs int64) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.mode != pairRecResume || rec.log == nil {
		return false
	}
	for i := rec.cursor; i < len(rec.log.stream); i++ {
		row := &rec.log.rows[rec.log.stream[i]]
		if row.Kind == pairRecEventWake {
			return false
		}
		if st := row.Event.Start; row.Kind == pairRecEventStart && st != nil && st.Kind == pairRecKindCollect && st.SlotMs == slotMs && st.ParentID == "" {
			rec.skipTo(i + 1)
			return true
		}
	}
	return false
}

func pairPaperOracleAttemptIndex(l *pairRecLog) (map[string]pairPaperOracleAttempt, error) {
	attempts := map[string]pairPaperOracleAttempt{}
	if l == nil {
		return attempts, nil
	}
	for i, ri := range l.stream {
		row := l.rows[ri]
		st := row.Event.Start
		if row.Kind != pairRecEventStart || st == nil || st.Kind != pairRecKindFundingOracle || st.ParentID != "" {
			continue
		}
		var req pairPaperOracleRequest
		if err := pairReplayStrictDecode(st.Request, &req); err != nil {
			return nil, fmt.Errorf("oracle call %s has an invalid request: %v", row.CallID, err)
		}
		attemptID, err := pairFundingOracleAttemptID(req.Key, req.TargetEventMs, req.PolicyDigest)
		if err != nil || attemptID != req.AttemptID {
			return nil, fmt.Errorf("oracle call %s has an invalid deterministic attempt identity", row.CallID)
		}
		attempt := pairPaperOracleAttempt{Key: req.Key, TargetEventMs: req.TargetEventMs, PolicyDigest: req.PolicyDigest, AttemptID: req.AttemptID,
			CallID: row.CallID, StartSeq: row.Seq, EndSeq: -1, StreamPos: i, ConsumeStream: -1, Async: st.Async, Disposition: "started"}
		if ci, ok := l.consumes[row.CallID]; ok {
			attempt.ConsumeStream = l.streamIndex(ci)
		}
		if ei, ok := l.ends[row.CallID]; ok {
			if ei < 0 || l.rows[ei].Kind == pairRecEventRecovery {
				attempt.Disposition = "interrupted"
			} else {
				end := l.rows[ei]
				attempt.EndSeq = end.Seq
				attempt.Disposition = end.Event.End.Class
			}
		}
		if prior, exists := attempts[req.AttemptID]; exists {
			return nil, fmt.Errorf("oracle attempt %s has distinct committed starts %s and %s; one hour has one outside attempt", req.AttemptID, prior.CallID, row.CallID)
		}
		attempts[req.AttemptID] = attempt
	}
	return attempts, nil
}

func (rec *pairRecorder) rebuildOracleAttemptsLocked() {
	if rec.oracleLog == rec.log && rec.attempts != nil {
		return
	}
	attempts, err := pairPaperOracleAttemptIndex(rec.log)
	if err != nil {
		rec.refuse("%v", err)
		attempts = map[string]pairPaperOracleAttempt{}
	}
	rec.oracleLog, rec.attempts = rec.log, attempts
}

func (rec *pairRecorder) oracleAttemptConsumed(attemptID string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.rebuildOracleAttemptsLocked()
	attempt, ok := rec.attempts[attemptID]
	if !ok {
		return false
	}
	if attempt.Async {
		return attempt.ConsumeStream >= 0 && attempt.ConsumeStream < rec.cursor
	}
	return attempt.StreamPos < rec.cursor
}

func (rec *pairRecorder) oracleAttemptRecorded(attemptID string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.rebuildOracleAttemptsLocked()
	_, ok := rec.attempts[attemptID]
	return ok
}

func (rec *pairRecorder) fundingConsumed(chain []pairFundingBatchValue) []pairFundingBatchValue {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	l := rec.log
	if l == nil {
		return chain
	}
	if rec.fundingLog != l {
		pos := map[string]int{}
		for i, ri := range l.stream {
			row := l.rows[ri]
			if row.Kind == pairRecEventConsume && row.Event.Consume != nil && row.Event.Consume.Disposition == "accepted" && row.Event.Consume.BatchHash != "" {
				pos[row.Event.Consume.BatchHash] = i
				continue
			}
			st := row.Event.Start
			if row.Kind != pairRecEventStart || st == nil || st.Async || (st.Kind != pairRecKindFunding && st.Kind != pairRecKindFundingOracle) || st.ParentID != "" {
				continue
			}
			ei, ok := l.ends[row.CallID]
			if !ok || ei < 0 || l.rows[ei].Kind != pairRecEventEnd || l.rows[ei].Event.End.Class != pairRecClassResult {
				continue
			}
			b, err := pairDecodeFundingBatch(l.rows[ei].Event.End.Result)
			if err != nil {
				continue
			}
			pos[b.BatchHash] = i
		}
		rec.fundingLog, rec.fundingPos = l, pos
	}
	for i, b := range chain {
		if p, ok := rec.fundingPos[b.BatchHash]; ok && p >= rec.cursor {
			return chain[:i]
		}
	}
	return chain
}

func (rec *pairRecorder) endStep() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.mode == pairRecResume {
		rec.seekLocked(func(*pairRecRow) bool { return false })
	}
}
