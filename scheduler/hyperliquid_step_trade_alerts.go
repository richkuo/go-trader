package main

import (
	"fmt"
	"strings"
	"sync"
)

type hlStepTradeAlerts struct {
	sc       StrategyConfig
	ss       *StrategyState
	baseline int
	labels   map[int]string
}

func beginHyperliquidStepTradeAlerts(sc StrategyConfig, ss *StrategyState, mu *sync.RWMutex) *hlStepTradeAlerts {
	mu.RLock()
	baseline := len(ss.TradeHistory)
	mu.RUnlock()
	return &hlStepTradeAlerts{sc: sc, ss: ss, baseline: baseline, labels: map[int]string{}}
}

func (a *hlStepTradeAlerts) historyLen(mu *sync.RWMutex) int {
	mu.RLock()
	defer mu.RUnlock()
	return len(a.ss.TradeHistory)
}

func (a *hlStepTradeAlerts) historyLenLocked() int {
	return len(a.ss.TradeHistory)
}

func (a *hlStepTradeAlerts) bindWindowLocked(before int, labels ...string) {
	if before < a.baseline {
		return
	}
	n := len(a.ss.TradeHistory)
	if n-before <= 0 || n-before != len(labels) {
		return
	}
	for i, label := range labels {
		if label != "" {
			a.labels[before+i] = label
		}
	}
}

func (a *hlStepTradeAlerts) bindWindow(mu *sync.RWMutex, before int, labels ...string) {
	mu.RLock()
	defer mu.RUnlock()
	a.bindWindowLocked(before, labels...)
}

func (a *hlStepTradeAlerts) bindExecuteLocked(before int, execDetail string) {
	if execDetail == "" || before < a.baseline {
		return
	}
	n := len(a.ss.TradeHistory)
	if n <= before {
		return
	}
	target := before
	for i := before; i < n; i++ {
		if !a.ss.TradeHistory[i].IsClose {
			target = i
			break
		}
	}
	a.labels[target] = execDetail
}

func (a *hlStepTradeAlerts) finish(mu *sync.RWMutex, notifier tradeAlertRouter, rc *RegimeConfig, logger *StrategyLogger) (int, string) {
	n, lines := a.finishLines(mu, notifier, rc, logger)
	return n, strings.Join(lines, "; ")
}

func (a *hlStepTradeAlerts) finishLines(mu *sync.RWMutex, notifier tradeAlertRouter, rc *RegimeConfig, logger *StrategyLogger) (int, []string) {
	mu.RLock()
	n := len(a.ss.TradeHistory)
	if a.baseline > n {
		mu.RUnlock()
		if logger != nil {
			logger.Error("trade-alert interval invariant failure for %s: baseline %d exceeds history length %d; no rows selected", a.sc.ID, a.baseline, n)
		}
		return 0, nil
	}
	rows := make([]Trade, 0, n-a.baseline)
	lines := make([]string, 0, n-a.baseline)
	for i := a.baseline; i < n; i++ {
		t := a.ss.TradeHistory[i]
		if !hyperliquidPublicTradeAlertRow(t) {
			continue
		}
		rows = append(rows, t)
		line := a.labels[i]
		if line == "" {
			line = hlStepTradeLine(a.sc, t)
		}
		lines = append(lines, line)
	}
	mu.RUnlock()
	if len(rows) == 0 {
		return 0, nil
	}
	sendTradeAlertRows(a.sc, rows, notifier, rc)
	return len(rows), lines
}

func hlStepTradeLine(sc StrategyConfig, t Trade) string {
	prefix := ""
	if hyperliquidIsLive(sc.Args) {
		prefix = "LIVE "
	}
	line := fmt.Sprintf("[%s] %s%s %s %.6f @ $%.2f", sc.ID, prefix, strings.ToUpper(t.Side), t.Symbol, t.Quantity, t.Price)
	if !t.IsClose {
		return line
	}
	if source := tradeAlertCloseSource(t.Details); source != "" {
		return line + " | " + source
	}
	if t.Details != "" {
		return line + " | " + t.Details
	}
	return line
}
