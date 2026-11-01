package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	pairOracleCleanupBound = 5 * time.Second
	pairPaperClockGateEnv  = "GO_TRADER_PAPER_CLOCK_GATE"
	pairPaperGateWatchdog  = 60 * time.Second
)

type pairOraclePidBox struct {
	pid atomic.Int64
}

type pairOraclePidKey struct{}

type pairPaperOracleRequestAsync struct {
	Args          []string        `json:"args"`
	Key           pairEvidenceKey `json:"key"`
	SourceID      string          `json:"source_id"`
	TargetEventMs int64           `json:"target_event_ms"`
	PolicyDigest  string          `json:"policy_digest"`
	AttemptID     string          `json:"attempt_id"`
}

type pairAsyncOracleResult struct {
	res        pairRecResult
	completion int64
	backward   bool
}

type pairAsyncOracle struct {
	cancel     context.CancelFunc
	target     int64
	deadline   int64
	attemptID  string
	callID     string
	start      pairRecStart
	key        pairEvidenceKey
	done       chan struct{}
	mu         sync.Mutex
	ready      atomic.Bool
	res        pairRecResult
	completion int64
	backward   bool
	terminal   *pairRecEnd
	consumed   bool
	recovered  bool
}

func (r *pairReplayRuntime) paperAsync() bool {
	return r != nil && r.paper != nil && (r.paper.manifest.Version == pairPaperManifestAsyncVer || r.paper.manifest.Version == pairPaperManifestBooksVer && r.paper.scenario.FundingPolicy != nil)
}

func (r *pairReplayRuntime) killOracleChild() {
	r.oracleMu.Lock()
	cancel, box := r.oracleCancel, r.oracleBox
	r.oracleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if box == nil {
		return
	}
	if pid := box.pid.Load(); pid > 0 {
		_ = syscall.Kill(-int(pid), syscall.SIGKILL)
	}
}

func (r *pairReplayRuntime) currentOracle() *pairAsyncOracle {
	r.oracleMu.Lock()
	defer r.oracleMu.Unlock()
	return r.oracle
}

func (r *pairReplayRuntime) clearOracle() {
	r.oracleMu.Lock()
	r.oracle = nil
	r.oracleCancel = nil
	r.oracleBox = nil
	r.oracleMu.Unlock()
}

func (w *pairAsyncOracle) snapshot() pairAsyncOracleResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	return pairAsyncOracleResult{res: w.res, completion: w.completion, backward: w.backward}
}

func (w *pairAsyncOracle) take(wait bool) (pairAsyncOracleResult, bool) {
	return w.join(wait, false)
}

func (w *pairAsyncOracle) join(wait, cancelFirst bool) (pairAsyncOracleResult, bool) {
	if w == nil {
		return pairAsyncOracleResult{}, false
	}
	if w.ready.Load() {
		return w.snapshot(), true
	}
	if !wait {
		return pairAsyncOracleResult{}, false
	}
	if cancelFirst && w.cancel != nil {
		w.cancel()
	}
	timer := time.NewTimer(pairOracleCleanupBound)
	defer timer.Stop()
	select {
	case <-w.done:
	case <-timer.C:
		if cancelFirst {
			return pairAsyncOracleResult{}, false
		}
		if w.cancel != nil {
			w.cancel()
		}
		timer.Reset(pairOracleCleanupBound)
		select {
		case <-w.done:
		case <-timer.C:
			return pairAsyncOracleResult{}, false
		}
	}
	if !w.ready.Load() {
		return pairAsyncOracleResult{}, false
	}
	return w.snapshot(), true
}

func (r *pairReplayRuntime) deliverAsyncOracle(wake pairRecWake) []string {
	if r.paper.playback || r.rec.consumingNow() {
		return r.playAsyncOracle(wake)
	}
	var trace []string
	r.finishReadyOracle(wake, &trace)
	policy := r.paper.scenario.FundingPolicy
	if policy == nil {
		return trace
	}
	key, ok := r.oracleMemberKey()
	if !ok {
		return append(trace, "oracle_missing_member")
	}
	digest, err := pairFundingPolicyDigest(*policy)
	if err != nil {
		return append(trace, "oracle_policy_error")
	}
	for r.oracleNextMs > 0 {
		target := r.oracleNextMs
		prepare, err := pairPaperOraclePrepareAt(target, *policy)
		if err != nil || wake.WallMs < prepare {
			break
		}
		deadline, err := pairFundingOracleDeadline(*policy, target)
		if err != nil {
			break
		}
		attemptID, err := pairFundingOracleAttemptID(key, target, digest)
		if err != nil {
			break
		}
		if r.rec.oracleAttemptConsumed(attemptID) {
			r.advanceOracleGrid()
			trace = append(trace, "oracle_consumed")
			continue
		}
		if r.workerFor(target) != nil || r.rec.oracleAttemptRecorded(attemptID) {
			break
		}
		if wake.ClockBackward || wake.WallMs > deadline {
			r.advanceOracleGrid()
			trace = append(trace, "oracle_missed")
			continue
		}
		if err := r.beginOracleAttempt(wake, key, *policy, digest, target, deadline, attemptID); err != nil {
			fmt.Fprintf(os.Stderr, "[paper] funding oracle read for hour %d was not made, so the hour stays for the next wake: %v\n", target, err)
			trace = append(trace, "oracle_not_made")
			break
		}
		if wake.WallMs >= deadline {
			r.finishReadyOracle(wake, &trace)
			continue
		}
		trace = append(trace, "funding_oracle pending")
		break
	}
	return trace
}

func (r *pairReplayRuntime) oracleMemberKey() (pairEvidenceKey, bool) {
	if r.rt == nil {
		return pairEvidenceKey{}, false
	}
	for _, m := range r.rt.Contract.pairMembersSorted() {
		if m.Provider == entryProviderHyperliquid {
			return r.rt.Contract.pairMemberKey(m).evidenceKey(), true
		}
	}
	return pairEvidenceKey{}, false
}

func (r *pairReplayRuntime) workerFor(target int64) *pairAsyncOracle {
	w := r.currentOracle()
	if w != nil && w.target == target && !w.consumed {
		return w
	}
	return nil
}

func (r *pairReplayRuntime) beginOracleAttempt(wake pairRecWake, key pairEvidenceKey, policy pairFundingPolicyV1, digest string, target, deadline int64, attemptID string) error {
	args, err := pairPaperOracleArgs(key, policy, target, deadline, digest)
	if err != nil {
		return err
	}
	req, err := pairCanonicalJSON(pairPaperOracleRequestAsync{Args: args, Key: key, SourceID: r.paper.funding.SourceID(), TargetEventMs: target, PolicyDigest: digest, AttemptID: attemptID})
	if err != nil {
		return err
	}
	callID, start, err := r.rec.beginAsyncCall(pairRecCall{Kind: pairRecKindFundingOracle, MemberID: key.MemberID, SlotMs: target, Request: req})
	if err != nil {
		return err
	}
	w := &pairAsyncOracle{target: target, deadline: deadline, attemptID: attemptID, callID: callID, start: start, key: key, done: make(chan struct{})}
	if wake.WallMs >= deadline {
		w.ready.Store(true)
		w.res = pairRecResult{Class: pairRecClassCancelled, Cause: "the oracle sampling deadline elapsed before the reader launch"}
		w.completion = wake.WallMs
		r.oracleMu.Lock()
		r.oracle = w
		r.oracleMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	box := &pairOraclePidBox{}
	ctx = context.WithValue(ctx, pairOraclePidKey{}, box)
	w.cancel = cancel
	r.oracleMu.Lock()
	r.oracle = w
	r.oracleCancel = cancel
	r.oracleBox = box
	r.oracleMu.Unlock()
	egress := r.paperEgress()
	pairPaperGateMarkLaunched()
	go r.runOracleWorker(ctx, w, egress, args, policy, deadline)
	return nil
}

func (r *pairReplayRuntime) runOracleWorker(ctx context.Context, w *pairAsyncOracle, egress *EgressConfig, args []string, policy pairFundingPolicyV1, deadline int64) {
	defer close(w.done)
	res := pairRunHistoryReader(ctx, egress, args)
	pairPaperGateClear()
	completion := pairPaperWall.Now()
	if clock, ok := pairPaperWall.(interface{ CompletionTick() time.Time }); ok {
		completion = clock.CompletionTick()
	}
	if res.Class == pairRecClassResult {
		obs, err := pairPaperCheckOracle(res.Stdout, w.key, policy, w.target, deadline, res.ReceiptMs)
		if err != nil {
			res.Class, res.Cause = pairRecClassInfra, err.Error()
		} else if raw, err := pairCanonicalJSON(obs); err != nil {
			res.Class, res.Cause = pairRecClassInfra, err.Error()
		} else {
			res.Result = raw
		}
		res.Stdout = nil
	}
	back := completion.UnixMilli() < w.start.RawStartMs || (res.ReceiptMs > 0 && res.ReceiptMs < w.start.RawStartMs) || (res.ReceiptMs > 0 && completion.UnixMilli() < res.ReceiptMs)
	w.mu.Lock()
	w.res = res
	w.completion = completion.UnixMilli()
	w.backward = back
	w.mu.Unlock()
	w.ready.Store(true)
}

func (r *pairReplayRuntime) finishReadyOracle(wake pairRecWake, trace *[]string) {
	w := r.currentOracle()
	if w == nil || w.consumed {
		r.settleOracleFromLog(wake, trace)
		return
	}
	if w.recovered {
		if r.rec.blocked.Load() {
			return
		}
		l, err := readVerifiedPairRecLog(r.rec.file, r.rec.runID, r.rec.partition, r.rec.contractFP)
		if err == nil {
			attempt := pairPaperOracleAttempt{CallID: w.callID, TargetEventMs: w.target, AttemptID: w.attemptID, Key: w.key}
			err = r.consumeLoggedAttemptFrom(l, wake, attempt)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[paper] funding oracle recovery for hour %d could not be consumed: %v\n", w.target, err)
			*trace = append(*trace, "oracle_consume_unrecorded")
			return
		}
		w.consumed = true
		r.clearOracle()
		if w.target == r.oracleNextMs {
			r.advanceOracleGrid()
		}
		*trace = append(*trace, "funding_oracle")
		return
	}
	wait := wake.WallMs >= w.deadline
	res, ok := w.take(wait || w.ready.Load())
	if !ok {
		return
	}
	if w.terminal == nil {
		end, err := r.rec.appendAsyncTerminal(w.callID, w.start, res)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[paper] funding oracle terminal for hour %d could not be recorded: %v\n", w.target, err)
			*trace = append(*trace, "oracle_terminal_unrecorded")
			w.recovered = true
			return
		}
		w.terminal = end
	}
	if err := r.commitAsyncConsumption(wake, w, res); err != nil {
		fmt.Fprintf(os.Stderr, "[paper] funding oracle consumption for hour %d could not be recorded: %v\n", w.target, err)
		*trace = append(*trace, "oracle_consume_unrecorded")
		return
	}
	w.consumed = true
	target := w.target
	r.clearOracle()
	if target == r.oracleNextMs {
		r.advanceOracleGrid()
	}
	*trace = append(*trace, "funding_oracle")
}

func (r *pairReplayRuntime) commitAsyncConsumption(wake pairRecWake, w *pairAsyncOracle, res pairAsyncOracleResult) error {
	end := w.terminal
	rawBoundary := wake.WallMs
	if res.res.ReceiptMs > rawBoundary {
		rawBoundary = res.res.ReceiptMs
	}
	if res.completion > rawBoundary {
		rawBoundary = res.completion
	}
	c := pairRecConsume{RequestHash: w.start.RequestHash, WakeSeq: r.rec.lastWakeSeq(), StepID: r.rec.stepIDNow(), LogicalMs: rawBoundary}
	if end != nil {
		c.TerminalKind = pairRecEventEnd
		c.TerminalHash = end.ResultHash
		eligible, batch, batchHash := r.sealOracleBatch(w, end, res)
		if eligible {
			c.Disposition = "accepted"
			c.Batch, c.BatchHash = batch, batchHash
		} else if end.Class == pairRecClassResult {
			c.Disposition = "refused"
		} else if end.Class == pairRecClassCancelled {
			c.Disposition = "cancelled"
		} else {
			c.Disposition = "infrastructure_failure"
		}
	} else {
		return fmt.Errorf("oracle call %s has no terminal to consume", w.callID)
	}
	if err := r.rec.appendConsume(w.callID, c); err != nil {
		return err
	}
	if c.Disposition == "accepted" {
		if err := r.paper.funding.Append(c.Batch); err != nil {
			return err
		}
	}
	return nil
}

func (r *pairReplayRuntime) sealOracleBatch(w *pairAsyncOracle, end *pairRecEnd, res pairAsyncOracleResult) (bool, []byte, string) {
	if end.Class != pairRecClassResult || end.ClockBackward || res.backward {
		return false, nil, ""
	}
	var obs pairFundingOracleObservation
	if err := pairReplayStrictDecode(end.Result, &obs); err != nil {
		return false, nil, ""
	}
	if r.paper.funding.policy == nil {
		return false, nil, ""
	}
	policy := *r.paper.funding.policy
	deadline, err := pairFundingOracleDeadline(policy, w.target)
	if err != nil || end.RawCompletionMs > deadline || end.RawCompletionMs < obs.ReceiptMs || end.RawReceiptMs != obs.ReceiptMs {
		return false, nil, ""
	}
	if err := pairFundingValidateObservation(obs, policy); err != nil {
		return false, nil, ""
	}
	chain := r.rec.fundingConsumed(r.paper.funding.chain())
	seq, prev := int64(len(chain)+1), ""
	if len(chain) > 0 {
		prev = chain[len(chain)-1].BatchHash
	}
	digest, _ := pairFundingPolicyDigest(policy)
	batch, err := (pairFundingBatchV2{V: pairFundingResearchBatchVersion, SourceID: r.paper.funding.SourceID(), Seq: seq, PrevHash: prev, PolicyDigest: digest,
		Rates: []pairFundingRate{}, Prices: []pairFundingPrice{}, Observations: []pairFundingOracleObservation{obs}, Coverage: []pairFundingCoverageRecord{}}).sealed()
	if err != nil {
		return false, nil, ""
	}
	raw, err := pairFundingBatchValueBytes(pairFundingBatchV2Value(batch))
	if err != nil {
		return false, nil, ""
	}
	if err := r.paper.funding.Check(raw); err != nil {
		return false, nil, ""
	}
	return true, raw, batch.BatchHash
}

func (r *pairReplayRuntime) settleOracleFromLog(wake pairRecWake, trace *[]string) {
	if r.rec == nil || r.rec.log == nil || r.paper == nil || r.paper.funding == nil || r.paper.funding.policy == nil {
		return
	}
	key, ok := r.oracleMemberKey()
	if !ok {
		return
	}
	digest, err := pairFundingPolicyDigest(*r.paper.funding.policy)
	if err != nil {
		return
	}
	target := r.oracleNextMs
	if target <= 0 {
		return
	}
	attemptID, err := pairFundingOracleAttemptID(key, target, digest)
	if err != nil || r.rec.oracleAttemptConsumed(attemptID) || !r.rec.oracleAttemptRecorded(attemptID) {
		return
	}
	attempt := r.rec.oracleAttempt(attemptID)
	if attempt.CallID == "" || !attempt.Async || attempt.StreamPos >= r.rec.cursorNow() {
		return
	}
	if attempt.ConsumeStream >= 0 {
		return
	}
	if attempt.Disposition == "started" {
		return
	}
	if err := r.consumeLoggedAttempt(wake, attempt); err != nil {
		fmt.Fprintf(os.Stderr, "[paper] funding oracle consumption for hour %d could not be recorded: %v\n", target, err)
		*trace = append(*trace, "oracle_consume_unrecorded")
		return
	}
	if target == r.oracleNextMs {
		r.advanceOracleGrid()
	}
	*trace = append(*trace, "funding_oracle")
}

func (r *pairReplayRuntime) consumeLoggedAttempt(wake pairRecWake, attempt pairPaperOracleAttempt) error {
	return r.consumeLoggedAttemptFrom(r.rec.log, wake, attempt)
}

func (r *pairReplayRuntime) consumeLoggedAttemptFrom(l *pairRecLog, wake pairRecWake, attempt pairPaperOracleAttempt) error {
	si, ok := l.starts[attempt.CallID]
	if !ok {
		return fmt.Errorf("oracle call %s is not in the log", attempt.CallID)
	}
	start := l.rows[si].Event.Start
	ei, ended := l.ends[attempt.CallID]
	c := pairRecConsume{RequestHash: start.RequestHash, WakeSeq: r.rec.lastWakeSeq(), StepID: r.rec.stepIDNow(), LogicalMs: wake.WallMs}
	var res pairAsyncOracleResult
	w := &pairAsyncOracle{target: attempt.TargetEventMs, attemptID: attempt.AttemptID, callID: attempt.CallID, start: *start, key: attempt.Key}
	if !ended || ei < 0 || l.rows[ei].Kind == pairRecEventRecovery {
		reason := "the process stopped after this call's start committed and before its outcome committed; the result is unknown and the call is never repeated"
		if ended && ei >= 0 && l.rows[ei].Event.Recovery != nil {
			reason = l.rows[ei].Event.Recovery.Reason
		}
		c.TerminalKind = pairRecEventRecovery
		c.TerminalHash = sha256Hex([]byte(reason))
		c.Disposition = "interrupted"
		return r.rec.appendConsume(attempt.CallID, c)
	}
	end := l.rows[ei].Event.End
	w.terminal = end
	res.res = pairRecResult{Class: end.Class, Result: end.Result, ReceiptMs: end.RawReceiptMs}
	res.completion = end.RawCompletionMs
	res.backward = end.ClockBackward
	if end.RawReceiptMs > c.LogicalMs {
		c.LogicalMs = end.RawReceiptMs
	}
	if end.RawCompletionMs > c.LogicalMs {
		c.LogicalMs = end.RawCompletionMs
	}
	c.TerminalKind = pairRecEventEnd
	c.TerminalHash = end.ResultHash
	eligible, batch, batchHash := r.sealOracleBatch(w, end, res)
	if eligible {
		c.Disposition = "accepted"
		c.Batch, c.BatchHash = batch, batchHash
	} else if end.Class == pairRecClassResult {
		c.Disposition = "refused"
	} else if end.Class == pairRecClassCancelled {
		c.Disposition = "cancelled"
	} else {
		c.Disposition = "infrastructure_failure"
	}
	if err := r.rec.appendConsume(attempt.CallID, c); err != nil {
		return err
	}
	if c.Disposition == "accepted" {
		return r.paper.funding.Append(c.Batch)
	}
	return nil
}

func (r *pairReplayRuntime) playAsyncOracle(wake pairRecWake) []string {
	var trace []string
	for {
		if r.rec.consumeOracleFailure() {
			trace = append(trace, "oracle_not_made")
			continue
		}
		kind, row, ok := r.rec.takeAsyncBoundary()
		if !ok {
			break
		}
		switch kind {
		case pairRecEventStart:
			trace = append(trace, "funding_oracle pending")
		case pairRecEventConsume:
			if row.Event.Consume != nil && row.Event.Consume.Disposition == "accepted" {
				if err := r.paper.funding.Append(row.Event.Consume.Batch); err != nil {
					fmt.Fprintf(os.Stderr, "[paper] the recorded oracle consumption is refused: %v\n", err)
				}
			}
			trace = append(trace, "funding_oracle")
			if r.consumeMatchesHour(row) {
				r.advanceOracleGrid()
			}
		default:
			return trace
		}
	}
	if r.paper.playback {
		trace = append(trace, r.playbackOracleMiss(wake)...)
	}
	return trace
}

func (r *pairReplayRuntime) consumeMatchesHour(row pairRecRow) bool {
	if r.rec == nil || r.rec.log == nil || r.oracleNextMs <= 0 {
		return false
	}
	si, ok := r.rec.log.starts[row.CallID]
	if !ok || r.rec.log.rows[si].Event.Start == nil {
		return false
	}
	return r.rec.log.rows[si].Event.Start.SlotMs == r.oracleNextMs
}

func (r *pairReplayRuntime) playbackOracleMiss(wake pairRecWake) []string {
	policy := r.paper.scenario.FundingPolicy
	if policy == nil {
		return nil
	}
	var trace []string
	key, ok := r.oracleMemberKey()
	if !ok {
		return nil
	}
	digest, err := pairFundingPolicyDigest(*policy)
	if err != nil {
		return nil
	}
	for r.oracleNextMs > 0 {
		target := r.oracleNextMs
		prepare, err := pairPaperOraclePrepareAt(target, *policy)
		if err != nil || wake.WallMs < prepare {
			break
		}
		deadline, err := pairFundingOracleDeadline(*policy, target)
		if err != nil {
			break
		}
		attemptID, err := pairFundingOracleAttemptID(key, target, digest)
		if err != nil {
			break
		}
		if r.rec.oracleAttemptConsumed(attemptID) {
			r.advanceOracleGrid()
			trace = append(trace, "oracle_consumed")
			continue
		}
		if r.rec.oracleAttemptRecorded(attemptID) {
			break
		}
		if wake.ClockBackward || wake.WallMs > deadline {
			r.advanceOracleGrid()
			trace = append(trace, "oracle_missed")
			continue
		}
		break
	}
	return trace
}

func (r *pairReplayRuntime) finalizeOracle() {
	w := r.currentOracle()
	if w == nil || w.consumed {
		r.killOracleChild()
		return
	}
	if w.recovered {
		r.clearOracle()
		r.killOracleChild()
		return
	}
	res, ok := w.join(true, true)
	if !ok {
		if w.terminal == nil {
			_, _ = r.rec.appendAsyncTerminal(w.callID, w.start, pairAsyncOracleResult{res: pairRecResult{Class: pairRecClassCancelled, Cause: "the oracle worker did not finish before the run closed"}, completion: w.deadline})
		}
		r.clearOracle()
		r.killOracleChild()
		return
	}
	if w.terminal == nil {
		end, err := r.rec.appendAsyncTerminal(w.callID, w.start, res)
		if err != nil {
			r.clearOracle()
			r.killOracleChild()
			return
		}
		w.terminal = end
	}
	if r.finished {
		wake := pairRecWake{WallMs: w.deadline}
		if rec := r.rec.lastWakeRecordNow(); rec != nil && rec.WallMs > 0 {
			wake = *rec
		}
		_ = r.commitAsyncConsumption(wake, w, res)
	}
	r.clearOracle()
	r.killOracleChild()
}

func (rec *pairRecorder) beginAsyncCall(c pairRecCall) (string, pairRecStart, error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if c.Phase == "" {
		c.Phase = pairRecPhaseStep
	}
	reqHash := sha256Hex(c.Request)
	ordinal, callID := rec.callOrdinal()
	if rec.blocked.Load() {
		rec.enqueueLocked(pairRecEvent{Kind: pairRecEventFailure, CallID: callID, Failure: &pairRecFailure{CallKind: c.Kind, MemberID: c.MemberID, RequestHash: reqHash,
			StepID: rec.stepID, Ordinal: ordinal, Reason: "the recorder was blocked, so the call was refused before any read: " + rec.blockReason, FailedSeq: rec.failedSeq}})
		return "", pairRecStart{}, pairRecNotMadeError{fmt.Errorf("the %s call was not made: the recording barrier refuses every new outside read until the committed record is verified (%s)", c.Kind, rec.blockReason)}
	}
	start := pairRecStart{Kind: c.Kind, Phase: c.Phase, MemberID: c.MemberID, SlotMs: c.SlotMs, ActionID: c.ActionID, StepID: rec.stepID, Ordinal: ordinal,
		Request: c.Request, RequestHash: reqHash, RawStartMs: rec.now().UnixMilli(), LogicalBeforeMs: rec.logical, Async: true}
	if err := rec.appendLocked(pairRecEvent{Kind: pairRecEventStart, CallID: callID, Start: &start}); err != nil {
		rec.block(err.Error())
		rec.enqueueLocked(pairRecEvent{Kind: pairRecEventFailure, CallID: callID, Failure: &pairRecFailure{CallKind: c.Kind, MemberID: c.MemberID, RequestHash: reqHash,
			StepID: rec.stepID, Ordinal: ordinal, Reason: "the call start did not commit, so the call was never made: " + err.Error(), FailedSeq: rec.failedSeq}})
		return "", pairRecStart{}, pairRecNotMadeError{fmt.Errorf("the %s call start could not be recorded, so no outside read was made: %w", c.Kind, err)}
	}
	rec.fault("exit-after-call-start")
	return callID, start, nil
}

func (rec *pairRecorder) appendAsyncTerminal(callID string, start pairRecStart, res pairAsyncOracleResult) (*pairRecEnd, error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.consuming() {
		return nil, fmt.Errorf("a consumed recording does not append an asynchronous terminal")
	}
	out := res.res
	if out.Class == "" {
		out.Class = pairRecClassResult
	}
	if res.completion > rec.rawHigh {
		rec.rawHigh = res.completion
	}
	end := &pairRecEnd{Class: out.Class, Result: out.Result, ResultHash: pairRecResultHash(out.Result), RawReceiptMs: out.ReceiptMs, RawCompletionMs: res.completion, LogicalMs: rec.logical,
		Cause: out.Cause, ExitStatus: out.ExitStatus, Stdout: pairRecBound(out.Stdout, pairRecMaxStdoutBytes), Stderr: string(pairRecBound([]byte(out.Stderr), pairRecMaxStderrBytes)),
		Overflow: out.Overflow || len(out.Stdout) > pairRecMaxStdoutBytes, ClockBackward: res.backward}
	if err := rec.appendLocked(pairRecEvent{Kind: pairRecEventEnd, CallID: callID, End: end}); err != nil {
		rec.block(err.Error())
		rec.enqueueLocked(pairRecEvent{Kind: pairRecEventRecovery, CallID: callID, Recovery: &pairRecRecovery{Reason: "the outcome was received but its terminal record did not commit, so it was never used: " + err.Error(), Epoch: rec.epoch}})
		return nil, err
	}
	if res.backward {
		rec.backward.Store(true)
	}
	return end, nil
}

func (rec *pairRecorder) appendConsume(callID string, c pairRecConsume) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.mode == pairRecPlayback {
		return fmt.Errorf("a playback never appends to the recording")
	}
	c.LogicalMs = rec.nextLogical(c.LogicalMs)
	if c.StepID == "" {
		c.StepID = rec.stepID
	}
	if c.WakeSeq == 0 {
		c.WakeSeq = rec.lastWake
	}
	if err := rec.appendLocked(pairRecEvent{Kind: pairRecEventConsume, CallID: callID, Consume: &c}); err != nil {
		rec.block(err.Error())
		return err
	}
	rec.setLogical(c.LogicalMs)
	return nil
}

func (rec *pairRecorder) lastWakeSeq() int64 {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.lastWake
}

func (rec *pairRecorder) stepIDNow() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.stepID
}

func (rec *pairRecorder) lastWakeRecordNow() *pairRecWake {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.lastWakeRecord == nil {
		return nil
	}
	w := *rec.lastWakeRecord
	return &w
}

func (rec *pairRecorder) oracleAttempt(attemptID string) pairPaperOracleAttempt {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.rebuildOracleAttemptsLocked()
	if rec.attempts == nil {
		return pairPaperOracleAttempt{}
	}
	return rec.attempts[attemptID]
}

func (rec *pairRecorder) consumeOracleFailure() bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.consuming() {
		return false
	}
	item, ok := rec.streamItem()
	if !ok || item.Kind != pairRecEventFailure || item.Event.Failure == nil || item.Event.Failure.CallKind != pairRecKindFundingOracle {
		return false
	}
	rec.callOrdinal()
	rec.cursor++
	if !rec.blocked.Load() {
		rec.blockReason, rec.failedSeq = item.Event.Failure.Reason, item.Event.Failure.FailedSeq
		rec.blocked.Store(true)
	}
	return true
}

func (rec *pairRecorder) takeAsyncBoundary() (string, pairRecRow, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	item, ok := rec.streamItem()
	if !ok {
		return "", pairRecRow{}, false
	}
	switch item.Kind {
	case pairRecEventConsume:
		rec.cursor++
		if item.Event.Consume != nil {
			rec.setLogical(item.Event.Consume.LogicalMs)
		}
		return item.Kind, *item, true
	case pairRecEventStart:
		if item.Event.Start != nil && item.Event.Start.Async {
			rec.cursor++
			rec.ordinal = item.Event.Start.Ordinal
			return item.Kind, *item, true
		}
	}
	return "", pairRecRow{}, false
}

func (l *pairRecLog) streamIndex(row int) int {
	if l == nil || row < 0 || row >= len(l.streamPositions) {
		return -1
	}
	return l.streamPositions[row] - 1
}

func pairPaperConsumptionFrontier(db *StateDB, runID string) (int, error) {
	cursor, _, err := db.readPairRecCursor(runID)
	if err != nil {
		return 0, err
	}
	frontier := cursor
	steps, err := db.readPairReplaySteps(runID)
	if err != nil {
		return 0, err
	}
	for _, st := range steps {
		if st.Status != pairReplayStatusPending || len(st.Record) == 0 {
			continue
		}
		var rec pairReplayPendingRecord
		if err := json.Unmarshal(st.Record, &rec); err != nil {
			return 0, err
		}
		if rec.Coordinator != nil {
			frontier = rec.Coordinator.RecCursor
		}
	}
	return frontier, nil
}

func (r *pairReplayRuntime) restoreAsyncFunding(l *pairRecLog, frontier int) error {
	if l == nil || r.paper == nil || r.paper.funding == nil {
		return nil
	}
	src := r.paper.funding
	for i, row := range l.rows {
		if row.Kind == pairRecEventConsume && row.Event.Consume != nil && row.Event.Consume.Disposition == "accepted" {
			if l.streamIndex(i) < 0 || l.streamIndex(i) >= frontier {
				continue
			}
			if err := src.Append(row.Event.Consume.Batch); err != nil {
				return refuseReplay("resume_refused", fmt.Sprintf("the recorded oracle consumption of call %s cannot be restored: %v", row.CallID, err))
			}
			continue
		}
		if row.Kind != pairRecEventEnd || row.Event.End == nil || row.Event.End.Class != pairRecClassResult {
			continue
		}
		si, ok := l.starts[row.CallID]
		if !ok || l.rows[si].Event.Start == nil {
			continue
		}
		start := l.rows[si].Event.Start
		if start.Async || start.Kind != pairRecKindFunding {
			continue
		}
		if l.streamIndex(si) < 0 || l.streamIndex(si) >= frontier {
			continue
		}
		if err := pairPaperAdmitFundingResult(src, row.Event.End.Result, *start, row.Event.End); err != nil {
			return refuseReplay("resume_refused", fmt.Sprintf("the recorded funding batch of call %s cannot be restored: %v", row.CallID, err))
		}
	}
	return nil
}

func pairPaperGateDir() string { return os.Getenv(pairPaperClockGateEnv) }

func pairPaperGateMarkLaunched() {
	dir := pairPaperGateDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "launched"), []byte("1\n"), 0o600)
}

func pairPaperGateClear() {
	dir := pairPaperGateDir()
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, "launched"))
	_ = os.Remove(filepath.Join(dir, "active"))
	_ = os.Remove(filepath.Join(dir, "request.json"))
}

func pairPaperGateChildActive() bool {
	dir := pairPaperGateDir()
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "launched")); err == nil {
		return true
	}
	blob, err := os.ReadFile(filepath.Join(dir, "active"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(blob)))
	if err != nil || pid <= 0 {
		return true
	}
	if err := syscall.Kill(pid, 0); err != nil && errors.Is(err, syscall.ESRCH) {
		return false
	}
	return true
}

type pairPaperClockRequest struct {
	TargetMs int64  `json:"target_ms"`
	Kind     string `json:"kind"`
}

func pairPaperGateReadRequest() (pairPaperClockRequest, bool) {
	dir := pairPaperGateDir()
	if dir == "" {
		return pairPaperClockRequest{}, false
	}
	blob, err := os.ReadFile(filepath.Join(dir, "request.json"))
	if err != nil {
		return pairPaperClockRequest{}, false
	}
	var req pairPaperClockRequest
	if err := json.Unmarshal(blob, &req); err != nil || (req.Kind != "until" && req.Kind != "backward") {
		return pairPaperClockRequest{}, false
	}
	return req, true
}

func pairPaperGateWaitAck(deadline time.Time, served pairPaperClockRequest) {
	for {
		req, ok := pairPaperGateReadRequest()
		if !ok || req.TargetMs != served.TargetMs || req.Kind != served.Kind {
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "paper clock gate watchdog expired while waiting for the oracle reader\n")
			os.Exit(basketReplayExitError)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
