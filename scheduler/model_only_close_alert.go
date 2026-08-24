package main

import (
	"fmt"
	"sync"
	"time"
)

// #1451/#1454 folded alert — throttled owner DM when a model-only close row
// books.
//
// forceCloseAllPositions' generic sweep writes "Circuit breaker close ...
// (model-only reconciliation adjustment; no exchange fill)" rows whenever no
// real exchange fill was available: non-HL venues, a missing-fill fallback, a
// corrupt position, or (before #1454) any manual strategy the kill-switch fill
// chain ignored. The DB row was the ONLY signal — nothing read it. The repo's
// convention for a degraded money-accounting path is a throttled owner DM keyed
// by (strategy_id, symbol) with the alert_throttle_interval floor
// (missing_mark_alerts, circuit_breaker_suppression_alert), so the same shape
// lives here.
//
// CheckRisk and the kill-switch apply block both run under mu, so the alert is
// QUEUED there and drained by the main loop outside mu — the #880 rule that no
// notifier I/O happens under the state lock. Draining is unconditional so an
// unowned deployment cannot grow the queue without bound.

type modelOnlyCloseAlert struct {
	StrategyID string
	Symbol     string
	Quantity   float64
}

var modelOnlyCloseAlertQueue struct {
	mu      sync.Mutex
	pending []modelOnlyCloseAlert
}

type modelOnlyCloseKey struct {
	strategyID string
	symbol     string
}

// modelOnlyCloseThrottle is the process-lifetime once-per-window throttle for
// model-only close DMs. Unlike a clearable live-state tracker this reports a
// booking event, not an ongoing condition, so slots are never retained/cleared
// per cycle — the window alone bounds volume.
type modelOnlyCloseTracker struct {
	mu   sync.Mutex
	last map[modelOnlyCloseKey]time.Time
}

var modelOnlyCloseThrottle = &modelOnlyCloseTracker{last: make(map[modelOnlyCloseKey]time.Time)}

// shouldNotify reports whether a model-only row for (strategyID, symbol) may
// DM now. Fires when the slot is empty or now.Sub(last) >=
// effectiveAlertThrottleInterval() (config alert_throttle_interval, empty →
// 6h). Stamps last under mu when it decides to notify, before the caller sends.
func (t *modelOnlyCloseTracker) shouldNotify(strategyID, symbol string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := modelOnlyCloseKey{strategyID: strategyID, symbol: symbol}
	if last, ok := t.last[k]; ok && now.Sub(last) < effectiveAlertThrottleInterval() {
		return false
	}
	t.last[k] = now
	return true
}

// queueModelOnlyCloseAlert records one model-only close notice for the main
// loop to send, throttled per (strategy, symbol). Safe to call under mu.
func queueModelOnlyCloseAlert(strategyID, symbol string, quantity float64) {
	if strategyID == "" || symbol == "" {
		return
	}
	if !modelOnlyCloseThrottle.shouldNotify(strategyID, symbol, time.Now().UTC()) {
		return
	}
	modelOnlyCloseAlertQueue.mu.Lock()
	defer modelOnlyCloseAlertQueue.mu.Unlock()
	modelOnlyCloseAlertQueue.pending = append(modelOnlyCloseAlertQueue.pending,
		modelOnlyCloseAlert{StrategyID: strategyID, Symbol: symbol, Quantity: quantity})
}

// drainModelOnlyCloseAlerts removes and returns every queued notice. Always
// drain, even with no owner configured, so the queue stays bounded.
func drainModelOnlyCloseAlerts() []modelOnlyCloseAlert {
	modelOnlyCloseAlertQueue.mu.Lock()
	defer modelOnlyCloseAlertQueue.mu.Unlock()
	out := modelOnlyCloseAlertQueue.pending
	modelOnlyCloseAlertQueue.pending = nil
	return out
}

// formatModelOnlyCloseDM renders the owner DM. It states plainly that the row
// is an estimate without an exchange order behind it, so an operator does not
// read it as a confirmed close.
func formatModelOnlyCloseDM(a modelOnlyCloseAlert) string {
	return fmt.Sprintf("⚠️ **MODEL-ONLY FORCE-CLOSE BOOKED — NO EXCHANGE FILL**\n"+
		"Strategy `%s`: the circuit-breaker/kill-switch sweep closed %s (qty≈%.6f) as an internal reconciliation row with NO exchange order behind it "+
		"(detail: \"model-only reconciliation adjustment; no exchange fill\"). Its realized PnL is a mark-derived estimate and may not match what the venue actually did.\n"+
		"If a real exchange position existed on %s, reconcile it manually. One DM per (strategy, symbol) per alert-throttle window.",
		a.StrategyID, a.Symbol, a.Quantity, a.Symbol)
}
