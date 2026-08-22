package main

import (
	"fmt"
	"sync"
	"time"
)

// #1444 (PR review): owner-DM escalation for the missing-mark tripwire.
//
// A missing mark on a LIVE position disables an auto-protective mechanism —
// the Hyperliquid trailing stop-loss walker and the take-profit ratchet both
// return early behind their own `mark > 0` guard. The repo's convention for
// that class of silent failure is a throttled owner DM (script_failure_alerts,
// the #1431 book-drift path), not a stdout line nobody reads.

type missingMarkKey struct {
	strategyID string
	symbol     string
}

type missingMarkSlot struct {
	lastNotifiedAt time.Time
}

// missingMarkTracker is the process-lifetime, once-per-window throttle for
// live missing-mark DMs, keyed by (strategy_id, symbol) so a multi-cycle mark
// outage across several positions cannot emit one DM per position per cycle.
//
// Unlike replayDriftTracker, this one is CLEARABLE: the condition it reports
// is a live state, not a past event. Retain drops every slot whose position is
// no longer missing a mark, so a recovered mark re-arms the alert with no
// restart — a second outage after a recovery notifies immediately instead of
// hiding behind the first outage's window.
type missingMarkTracker struct {
	mu      sync.Mutex
	entries map[missingMarkKey]*missingMarkSlot
}

var missingMarkAlerts = &missingMarkTracker{}

// Record reports whether this miss should notify. Fires when the slot is empty
// or now.Sub(lastNotifiedAt) >= effectiveAlertThrottleInterval() (config
// alert_throttle_interval, empty -> 6h). Stamps lastNotifiedAt under mu when it
// decides to notify, before the caller sends.
func (t *missingMarkTracker) Record(miss missingMarkPosition, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[missingMarkKey]*missingMarkSlot)
	}
	k := missingMarkKey{strategyID: miss.StrategyID, symbol: miss.Symbol}
	e := t.entries[k]
	if e == nil {
		e = &missingMarkSlot{}
		t.entries[k] = e
	}
	if e.lastNotifiedAt.IsZero() || now.Sub(e.lastNotifiedAt) >= effectiveAlertThrottleInterval() {
		e.lastNotifiedAt = now
		return true
	}
	return false
}

// Retain drops every throttle slot whose (strategy, symbol) is absent from
// this cycle's miss list — the mark came back, the position closed, or the
// strategy was removed. Call it BEFORE the Record loop so a recovered-then-
// re-missing position notifies on the cycle it re-appears.
//
// Record-only misses are kept in the retain set even though they never DM, so
// a position that flips from record-only to live does not inherit a stale
// slot; it simply has none.
func (t *missingMarkTracker) Retain(misses []missingMarkPosition) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.entries) == 0 {
		return
	}
	live := make(map[missingMarkKey]bool, len(misses))
	for _, m := range misses {
		live[missingMarkKey{strategyID: m.StrategyID, symbol: m.Symbol}] = true
	}
	for k := range t.entries {
		if !live[k] {
			delete(t.entries, k)
		}
	}
}

func (t *missingMarkTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[missingMarkKey]*missingMarkSlot)
}

// formatMissingMarkDM is the owner-DM text for a live missing mark. It names
// the disabled protection explicitly — the operator's decision is whether to
// manage the position by hand until the mark returns.
func formatMissingMarkDM(miss missingMarkPosition) string {
	return fmt.Sprintf("⚠️ **No live mark: %s / %s**\nA live position is open and no mark resolved this cycle.\n• Trailing stop-loss walker: NOT running\n• Take-profit ratchet: NOT running\n• Portfolio value for this position falls back to entry cost\nManage this position manually until marks resume.",
		miss.StrategyID, miss.Symbol)
}
