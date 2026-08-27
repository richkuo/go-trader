package main

import (
	"fmt"
	"sync"
	"time"
)


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

type modelOnlyCloseTracker struct {
	mu   sync.Mutex
	last map[modelOnlyCloseKey]time.Time
}

var modelOnlyCloseThrottle = &modelOnlyCloseTracker{last: make(map[modelOnlyCloseKey]time.Time)}

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

func drainModelOnlyCloseAlerts() []modelOnlyCloseAlert {
	modelOnlyCloseAlertQueue.mu.Lock()
	defer modelOnlyCloseAlertQueue.mu.Unlock()
	out := modelOnlyCloseAlertQueue.pending
	modelOnlyCloseAlertQueue.pending = nil
	return out
}

func formatModelOnlyCloseDM(a modelOnlyCloseAlert) string {
	return fmt.Sprintf("⚠️ **MODEL-ONLY FORCE-CLOSE BOOKED — NO EXCHANGE FILL**\n"+
		"Strategy `%s`: the circuit-breaker/kill-switch sweep closed %s (qty≈%.6f) as an internal reconciliation row with NO exchange order behind it "+
		"(detail: \"model-only reconciliation adjustment; no exchange fill\"). Its realized PnL is a mark-derived estimate and may not match what the venue actually did.\n"+
		"If a real exchange position existed on %s, reconcile it manually. One DM per (strategy, symbol) per alert-throttle window.",
		a.StrategyID, a.Symbol, a.Quantity, a.Symbol)
}
