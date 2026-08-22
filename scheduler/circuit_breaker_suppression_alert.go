package main

import (
	"fmt"
	"strings"
	"sync"
)

// #1449 review — owner-DM escalation for a SUPPRESSED circuit breaker.
//
// #1448 moved the portfolio kill-switch latch to the equity signal whenever
// that signal can measure, which leaves the #292 per-strategy circuit breaker
// as the owner of margin protection. That breaker is opt-outable: an explicit
// circuit_breaker:false suppresses both of its firing arms (#1048). A perps
// strategy with the breaker off therefore has NO automatic protection at any
// level, and before this the only trace was a once-per-episode logger.Warn.
//
// The repo's convention for a degraded auto-protective mechanism is a
// throttled owner DM (missing_mark_alerts, script_failure_alerts, the #1431
// book-drift path), so the same crossing now raises one. Throttling is
// inherited rather than duplicated: recordCircuitBreakerSuppression only
// queues while claiming the circuitBreakerSuppressedWarned key, so the DM
// fires exactly once per suppression episode and re-arms with the log line
// when the breaker is re-enabled or every threshold clears.
//
// Scope is EVERY strategy type, not perps only. Futures carry leverage on the
// same breaker, and a spot strategy with the breaker off is equally unhalted;
// narrowing to perps would hide the identical gap elsewhere, and the
// once-per-episode throttle already bounds the volume.
//
// CheckRisk runs under the state lock, so the DM is queued there and drained
// by the main loop after mu.Unlock() — the #880 convention that no notifier
// I/O happens under mu.

type circuitBreakerSuppressionAlert struct {
	StrategyID string
	Reasons    []string
}

// circuitBreakerSuppressionQueue is the hand-off between CheckRisk (under mu)
// and the main loop (outside mu). Guarded by its own mutex because CheckRisk
// holds mu, not this one.
var circuitBreakerSuppressionQueue struct {
	mu      sync.Mutex
	pending []circuitBreakerSuppressionAlert
}

// queueCircuitBreakerSuppressionAlert records one suppression notice for the
// main loop to send. Safe to call under mu.
func queueCircuitBreakerSuppressionAlert(strategyID string, reasons []string) {
	if strategyID == "" || len(reasons) == 0 {
		return
	}
	circuitBreakerSuppressionQueue.mu.Lock()
	defer circuitBreakerSuppressionQueue.mu.Unlock()
	circuitBreakerSuppressionQueue.pending = append(circuitBreakerSuppressionQueue.pending, circuitBreakerSuppressionAlert{
		StrategyID: strategyID,
		Reasons:    append([]string(nil), reasons...),
	})
}

// drainCircuitBreakerSuppressionAlerts removes and returns every queued
// notice. Always drain, even with no owner configured, so an unowned
// deployment cannot grow the queue without bound.
func drainCircuitBreakerSuppressionAlerts() []circuitBreakerSuppressionAlert {
	circuitBreakerSuppressionQueue.mu.Lock()
	defer circuitBreakerSuppressionQueue.mu.Unlock()
	out := circuitBreakerSuppressionQueue.pending
	circuitBreakerSuppressionQueue.pending = nil
	return out
}

// formatCircuitBreakerSuppressionDM renders the owner DM. It states plainly
// that nothing was closed, so an operator does not read it as a fire, and it
// names the one action that restores protection.
func formatCircuitBreakerSuppressionDM(a circuitBreakerSuppressionAlert) string {
	return fmt.Sprintf("⚠️ **CIRCUIT BREAKER DISABLED — THRESHOLD CROSSED**\n"+
		"Strategy `%s` crossed a halt threshold (%s) with `circuit_breaker: false`, so NO circuit breaker fired.\n"+
		"Nothing was closed and the strategy keeps trading. Since #1448 the portfolio kill switch latches on EQUITY drawdown while it can measure, "+
		"so this per-strategy breaker is what owns margin protection — with it disabled this strategy has no automatic halt at any level.\n"+
		"Set `circuit_breaker: true` (or remove the field) and SIGHUP to restore it. One DM per suppression episode.",
		a.StrategyID, strings.Join(a.Reasons, "; "))
}
