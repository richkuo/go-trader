package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)


type missingMarkKey struct {
	strategyID string
	symbol     string
}

type missingMarkSlot struct {
	lastNotifiedAt time.Time
}

type missingMarkTracker struct {
	mu      sync.Mutex
	entries map[missingMarkKey]*missingMarkSlot
}

var missingMarkAlerts = &missingMarkTracker{}

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
