package main

import (
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// Direction labels for live-exec failure throttle keys and alert messages.
// Use these constants at every call site to avoid typos that would silently
// fragment the per-(strategy, platform, symbol, direction) throttle key.
const (
	directionOpen  = "open"
	directionClose = "close"
)

// shouldNotifyDrainFailure decides whether a circuit-breaker close drain
// should fire a Discord/DM alert for the current failure. Notifies on the
// first failure, every 10th failure, or when at least one hour has elapsed
// since the last notification — whichever fires first. This keeps operator
// alerts actionable without spamming every retry cycle (#427).
//
// failureCount is the count AFTER incrementing (i.e. 1 = first failure).
// lastNotifiedAt is the zero Time if no notification has been sent yet.
func shouldNotifyDrainFailure(failureCount int, lastNotifiedAt, now time.Time) bool {
	if failureCount <= 1 {
		return true
	}
	if failureCount%10 == 0 {
		return true
	}
	if !lastNotifiedAt.IsZero() && now.Sub(lastNotifiedAt) >= time.Hour {
		return true
	}
	return false
}

// formatDrainFailureAlert builds the operator message for a failed CB close
// attempt on the given platform drain. count is the failure count after
// incrementing (1 = first failure).
func formatDrainFailureAlert(platform, strategyID, symbol string, size float64, errMsg string, count int) string {
	countNote := ""
	if count > 1 {
		countNote = fmt.Sprintf(" (failure #%d, still retrying)", count)
	}
	return fmt.Sprintf("**CIRCUIT CLOSE FAILED** [%s] %s %s sz=%.6f: %s%s",
		strategyID, platform, symbol, size, errMsg, countNote)
}

// liveExecFailureEntry is one slot in the in-memory live-exec throttle.
type liveExecFailureEntry struct {
	count          int
	lastNotifiedAt time.Time
	lastErrSig     string // first 120 chars of the error message
}

// LiveExecFailureThrottle tracks per-(strategyID, platform, symbol, direction)
// consecutive live-order failures so the main loop can throttle operator alerts.
// All state is in-memory; restarting the scheduler naturally resets the counts
// so the first failure after a restart always notifies.
type LiveExecFailureThrottle struct {
	mu      sync.Mutex
	entries map[string]*liveExecFailureEntry
}

// liveExecKey builds the map key for a live execution failure entry.
func liveExecKey(strategyID, platform, symbol, direction string) string {
	return strategyID + "|" + platform + "|" + symbol + "|" + direction
}

// truncErrSig returns the first ~120 bytes of an error message to use as an
// error signature for detecting "same error vs. new error". Walks back to a
// rune boundary so the signature never splits a multi-byte UTF-8 codepoint.
func truncErrSig(errMsg string) string {
	const limit = 120
	if len(errMsg) <= limit {
		return errMsg
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(errMsg[cut]) {
		cut--
	}
	return errMsg[:cut]
}

// Record increments the failure counter for the given key. Returns
// (shouldNotify bool, failureCount int). Same-signature errors apply the
// standard throttle cadence (1st, 10th, hourly). A change in error signature
// resets the count and notifies immediately — the operator needs to know when
// the error character changes.
func (t *LiveExecFailureThrottle) Record(key, errSig string, now time.Time) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*liveExecFailureEntry)
	}
	e := t.entries[key]
	if e == nil {
		e = &liveExecFailureEntry{}
		t.entries[key] = e
	}
	sig := truncErrSig(errSig)
	if sig != e.lastErrSig {
		// New error type — treat as a fresh first failure.
		e.count = 1
		e.lastErrSig = sig
		e.lastNotifiedAt = now
		return true, 1
	}
	e.count++
	if shouldNotifyDrainFailure(e.count, e.lastNotifiedAt, now) {
		e.lastNotifiedAt = now
		return true, e.count
	}
	return false, e.count
}

// Clear removes the throttle entry for the given key. Called when a live
// order succeeds so that the next failure re-notifies as a fresh first hit.
func (t *LiveExecFailureThrottle) Clear(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries != nil {
		delete(t.entries, key)
	}
}

// liveExecThrottle is the package-level singleton; resets on process restart.
var liveExecThrottle = &LiveExecFailureThrottle{}

// formatLiveExecFailureAlert builds the operator message for a failed live
// order attempt (open or close). count is the failure count after incrementing.
func formatLiveExecFailureAlert(strategyID, platform, direction, symbol, errMsg string, count int) string {
	countNote := ""
	if count > 1 {
		countNote = fmt.Sprintf(" (failure #%d)", count)
	}
	return fmt.Sprintf("**LIVE ORDER FAILED** [%s] %s %s %s: %s%s",
		strategyID, platform, direction, symbol, errMsg, countNote)
}

// notifyLiveExecFailure fires a throttled Discord+DM alert when a live order
// wrapper returns ok=false. direction is a short action label used as the
// throttle-key fragment and operator-facing message prefix (e.g. directionOpen,
// directionClose, "fixed-atr-sl-arm"). The directionOpen/directionClose
// constants cover the common open/close paths; callers may pass any short
// label for non-standard actions. Uses the package-level liveExecThrottle;
// nil-safe (returns immediately if notifier has no backends).
func notifyLiveExecFailure(notifier *MultiNotifier, sc StrategyConfig, direction, symbol, errMsg string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	key := liveExecKey(sc.ID, sc.Platform, symbol, direction)
	shouldNotify, count := liveExecThrottle.Record(key, errMsg, time.Now().UTC())
	if !shouldNotify {
		return
	}
	msg := formatLiveExecFailureAlert(sc.ID, sc.Platform, direction, symbol, errMsg, count)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

// clearLiveExecThrottle removes the throttle entry for a successful live order.
// Calling this on success means the next failure for the same key notifies fresh.
func clearLiveExecThrottle(sc StrategyConfig, direction, symbol string) {
	key := liveExecKey(sc.ID, sc.Platform, symbol, direction)
	liveExecThrottle.Clear(key)
}
