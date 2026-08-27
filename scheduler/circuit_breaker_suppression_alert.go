package main

import (
	"fmt"
	"strings"
	"sync"
)


type circuitBreakerSuppressionAlert struct {
	StrategyID string
	Reasons    []string
}

var circuitBreakerSuppressionQueue struct {
	mu      sync.Mutex
	pending []circuitBreakerSuppressionAlert
}

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

func drainCircuitBreakerSuppressionAlerts() []circuitBreakerSuppressionAlert {
	circuitBreakerSuppressionQueue.mu.Lock()
	defer circuitBreakerSuppressionQueue.mu.Unlock()
	out := circuitBreakerSuppressionQueue.pending
	circuitBreakerSuppressionQueue.pending = nil
	return out
}

func formatCircuitBreakerSuppressionDM(a circuitBreakerSuppressionAlert) string {
	return fmt.Sprintf("⚠️ **CIRCUIT BREAKER DISABLED — THRESHOLD CROSSED**\n"+
		"Strategy `%s` crossed a halt threshold (%s) with `circuit_breaker: false`, so NO circuit breaker fired.\n"+
		"Nothing was closed and the strategy keeps trading. Since #1448 the portfolio kill switch latches on EQUITY drawdown while it can measure, "+
		"so this per-strategy breaker is what owns margin protection — with it disabled this strategy has no automatic halt at any level.\n"+
		"Set `circuit_breaker: true` (or remove the field) and SIGHUP to restore it. One DM per suppression episode.",
		a.StrategyID, strings.Join(a.Reasons, "; "))
}
