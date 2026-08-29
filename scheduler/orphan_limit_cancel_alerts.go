package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type orphanLimitCancelKey struct {
	strategyID string
	symbol     string
	orderOID   int64
}

type orphanLimitCancelSlot struct {
	lastNotifiedAt time.Time
}

type orphanLimitCancelTracker struct {
	mu      sync.Mutex
	entries map[orphanLimitCancelKey]*orphanLimitCancelSlot
}

var orphanLimitCancelAlerts = &orphanLimitCancelTracker{}

func orphanLimitCancelKeyFor(o PendingLimitOrder) orphanLimitCancelKey {
	return orphanLimitCancelKey{strategyID: o.StrategyID, symbol: o.Symbol, orderOID: o.OrderOID}
}

func (t *orphanLimitCancelTracker) Record(k orphanLimitCancelKey, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[orphanLimitCancelKey]*orphanLimitCancelSlot)
	}
	e := t.entries[k]
	if e == nil {
		e = &orphanLimitCancelSlot{}
		t.entries[k] = e
	}
	if e.lastNotifiedAt.IsZero() || now.Sub(e.lastNotifiedAt) >= effectiveAlertThrottleInterval() {
		e.lastNotifiedAt = now
		return true
	}
	return false
}

func (t *orphanLimitCancelTracker) Retain(orders []PendingLimitOrder) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.entries) == 0 {
		return
	}
	live := make(map[orphanLimitCancelKey]bool, len(orders))
	for _, o := range orders {
		live[orphanLimitCancelKeyFor(o)] = true
	}
	for k := range t.entries {
		if !live[k] {
			delete(t.entries, k)
		}
	}
}

func (t *orphanLimitCancelTracker) Clear(k orphanLimitCancelKey) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, k)
}

func (t *orphanLimitCancelTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[orphanLimitCancelKey]*orphanLimitCancelSlot)
}

func formatOrphanLimitCancelDM(o PendingLimitOrder, block, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚨 **Resting limit order NOT cancelled: %s**\n", killSwitchLimitOrderLabel(o))
	fmt.Fprintf(&b, "%s %.6f %s @ $%.4f (oid=%d) was marked for cancellation and the scheduler cannot finish the job: %s.\n",
		o.Side, o.OrderSize, o.Symbol, o.LimitPrice, o.OrderOID, reason)
	fmt.Fprintf(&b, "• Its owning strategy %q cannot adopt it because %s, so only the cancel-only lane can clear it\n", o.StrategyID, block)
	b.WriteString("• The order can still be resting on Hyperliquid and can still fill, putting exposure back on the exchange after an emergency stop\n")
	fmt.Fprintf(&b, "Cancel oid=%d on the Hyperliquid UI, or restore the strategy to this config as a Hyperliquid-live type=manual strategy so the scheduler resolves it.", o.OrderOID)
	return b.String()
}

func reportOrphanLimitCancel(notifier *MultiNotifier, o PendingLimitOrder, block, reason string, now time.Time) string {
	fmt.Printf("[CRITICAL] limit: resting limit order %s not confirmed cancelled: %s (its strategy cannot adopt it because %s) — the reconciler will retry next cycle\n",
		killSwitchLimitOrderLabel(o), reason, block)
	if !orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(o), now) {
		return ""
	}
	msg := formatOrphanLimitCancelDM(o, block, reason)
	if notifier != nil && notifier.HasBackends() {
		notifier.SendOwnerDM(msg)
	}
	return msg
}
