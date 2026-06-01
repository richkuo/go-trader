package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// scriptFailureAlertThreshold is the number of consecutive signal-script
// failures for a single strategy before the first operator alert fires.
// Issue #829: a strategy whose check script dies every cycle is "dead in the
// water" but otherwise looks healthy — the scheduler process stays alive and
// the status port keeps serving the last-successful position data. Three
// strikes balances detection latency against noise from a transient indexer
// blip that clears on the next cycle.
const scriptFailureAlertThreshold = 3

// scriptFailureMode distinguishes the two ways a signal-check subprocess can
// fail. Both count toward the same per-strategy consecutive-failure tally —
// #829's concern was that tracking only result.Error would miss hard crashes.
type scriptFailureMode string

const (
	// scriptFailureCrash is a hard crash: non-zero exit with no usable JSON
	// (timeout, OOM, import/init crash, missing script). Surfaced as the
	// run*Check "Script failed: %v" branch.
	scriptFailureCrash scriptFailureMode = "crash"
	// scriptFailureError is a soft error: the script emitted JSON with a
	// non-empty result.Error. Surfaced as the run*Check
	// "Script returned error: %s" branch.
	scriptFailureError scriptFailureMode = "error"
)

// scriptFailureModeLabel renders a scriptFailureMode for operator messages.
func scriptFailureModeLabel(mode scriptFailureMode) string {
	if mode == scriptFailureCrash {
		return "hard crash"
	}
	return "script error"
}

// scriptFailureEntry is one slot in the in-memory per-strategy tracker.
type scriptFailureEntry struct {
	count          int
	lastErrSig     string // first ~120 bytes of the most recent error
	lastNotifiedAt time.Time
	alerted        bool // an alert has fired for the current failure streak
}

// ScriptFailureTracker tracks per-strategy consecutive signal-script failures
// so the main loop can alert operators when a strategy goes dark. All state is
// in-memory; a scheduler restart resets every count, so a restarted strategy
// that still fails re-alerts after the threshold.
type ScriptFailureTracker struct {
	mu      sync.Mutex
	entries map[string]*scriptFailureEntry
}

// Record increments the consecutive-failure count for strategyID and reports
// whether this failure should fire an operator alert, along with the post-
// increment count. Alerts fire when the streak first reaches
// scriptFailureAlertThreshold, then re-throttle (every 10th failure or once an
// hour) while the streak persists. A change in error signature after the
// threshold re-alerts immediately so operators see a shifting failure mode.
func (t *ScriptFailureTracker) Record(strategyID, errSig string, now time.Time) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*scriptFailureEntry)
	}
	e := t.entries[strategyID]
	if e == nil {
		e = &scriptFailureEntry{}
		t.entries[strategyID] = e
	}
	sig := truncErrSig(errSig)
	sigChanged := sig != e.lastErrSig
	e.count++
	e.lastErrSig = sig

	if e.count < scriptFailureAlertThreshold {
		return false, e.count
	}

	shouldNotify := false
	switch {
	case !e.alerted:
		shouldNotify = true // first alert for this streak (threshold crossed)
	case sigChanged:
		shouldNotify = true // failure character changed mid-streak
	case e.count%10 == 0:
		shouldNotify = true
	case !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) >= time.Hour:
		shouldNotify = true
	}
	if shouldNotify {
		e.alerted = true
		e.lastNotifiedAt = now
	}
	return shouldNotify, e.count
}

// Clear resets the failure streak for strategyID after a clean script run and
// reports whether the strategy had been in an alerted state — i.e. a recovery
// notice is warranted — along with the streak length that just ended.
func (t *ScriptFailureTracker) Clear(strategyID string) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		return false, 0
	}
	e := t.entries[strategyID]
	if e == nil {
		return false, 0
	}
	recovered := e.alerted
	priorCount := e.count
	delete(t.entries, strategyID)
	return recovered, priorCount
}

// scriptFailureTracker is the package-level singleton; resets on restart.
var scriptFailureTracker = &ScriptFailureTracker{}

// formatScriptFailureAlert builds the operator message for a failing signal
// script. count is the consecutive-failure count after incrementing. The
// scheduler PID is included so operators can tell which process is producing
// the errors when duplicate go-trader processes are suspected (#845).
func formatScriptFailureAlert(sc StrategyConfig, mode scriptFailureMode, errMsg string, count int) string {
	return fmt.Sprintf("**SIGNAL SCRIPT FAILING** [%s] %s %s (pid=%d, %s, %d consecutive failures): %s",
		sc.ID, sc.Platform, sc.Script, os.Getpid(), scriptFailureModeLabel(mode), count, errMsg)
}

// formatScriptRecoveredAlert builds the operator message for a signal script
// that succeeded after having alerted as dead. priorCount is the streak length
// that just ended. The scheduler PID is included to match the failing alert so
// operators can correlate the recovery with the process that was failing (#845).
func formatScriptRecoveredAlert(sc StrategyConfig, priorCount int) string {
	return fmt.Sprintf("**SIGNAL SCRIPT RECOVERED** [%s] %s %s (pid=%d): succeeded after %d consecutive failures",
		sc.ID, sc.Platform, sc.Script, os.Getpid(), priorCount)
}

// notifyScriptFailure records a signal-script failure for sc and fires a
// throttled operator alert once the consecutive-failure streak crosses
// scriptFailureAlertThreshold. mode distinguishes a hard crash (no JSON) from a
// soft result.Error so the alert names the failure character. The failure is
// always recorded — even with no notifier backends — so the count and recovery
// state stay accurate; nil/empty notifier just suppresses the send.
func notifyScriptFailure(notifier *MultiNotifier, sc StrategyConfig, mode scriptFailureMode, errMsg string) {
	shouldNotify, count := scriptFailureTracker.Record(sc.ID, errMsg, time.Now().UTC())
	if !shouldNotify || notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := formatScriptFailureAlert(sc, mode, errMsg, count)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

// clearScriptFailure resets sc's failure streak after a clean script run and,
// if the strategy had previously alerted as dead, fires a one-shot recovery
// notice. Safe to call every cycle: it no-ops when no streak is active.
func clearScriptFailure(notifier *MultiNotifier, sc StrategyConfig) {
	recovered, priorCount := scriptFailureTracker.Clear(sc.ID)
	if !recovered || notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := formatScriptRecoveredAlert(sc, priorCount)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}
