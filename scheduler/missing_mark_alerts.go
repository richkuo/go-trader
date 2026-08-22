package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// #1444 (PR review): owner-DM escalation for the missing-mark tripwire.
//
// A missing mark on a LIVE position degrades an auto-protective mechanism. On
// Hyperliquid perps and manual it stops two outright — the trailing stop-loss
// walker and the take-profit ratchet both return early behind their own
// `mark > 0` guard. On every venue it also reverts the position to AvgCost
// inside the portfolio kill switch's drawdown input. The repo's convention for
// that class of silent failure is a throttled owner DM (script_failure_alerts,
// the #1431 book-drift path), not a stdout line nobody reads.
//
// #1445 review: which mechanisms the DM may name is venue-dependent — see
// markGatedManagers in risk.go.

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
//
// #1445 review: the mechanism bullets come from miss.DisabledManagers, which
// markGatedManagers derives from the position's type and venue. The text used
// to assert the Hyperliquid walker and ratchet for every live miss, so a
// BinanceUS spot or TopStep futures outage claimed a stop-loss had stopped
// when that venue never ran one. On a venue with no mark-gated manager the DM
// says so and rests on the valuation claim below, which holds everywhere:
// PortfolioValue falls back to pos.AvgCost on a missing key, understating this
// position inside the portfolio kill switch's drawdown input.
func formatMissingMarkDM(miss missingMarkPosition) string {
	venue := miss.Platform
	if venue == "" {
		venue = "unknown platform"
	}
	kind := miss.Type
	if kind == "" {
		kind = "position"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ **No live mark: %s / %s**\nA live %s position on %s is open and no mark resolved this cycle.\n", miss.StrategyID, miss.Symbol, kind, venue)
	for _, mech := range miss.DisabledManagers {
		fmt.Fprintf(&b, "• %s: NOT running\n", mech)
	}
	if len(miss.DisabledManagers) == 0 {
		b.WriteString("• No mark-gated position manager runs on this venue, so no stop-loss or take-profit automation changed state\n")
	}
	b.WriteString("• Portfolio value for this position falls back to entry cost, so the portfolio kill switch reads its drawdown from a stale valuation\n")
	b.WriteString("Manage this position manually until marks resume.")
	return b.String()
}
