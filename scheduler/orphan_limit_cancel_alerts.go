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
	lastNotifiedAt   time.Time
	notifiedSeverity int
}

type orphanLimitCancelTracker struct {
	mu      sync.Mutex
	entries map[orphanLimitCancelKey]*orphanLimitCancelSlot
}

var orphanLimitCancelAlerts = &orphanLimitCancelTracker{}

func orphanLimitCancelKeyFor(o PendingLimitOrder) orphanLimitCancelKey {
	return orphanLimitCancelKey{strategyID: o.StrategyID, symbol: o.Symbol, orderOID: o.OrderOID}
}

func orphanLimitCancelSeverity(state orphanLimitCancelState) int {
	switch state {
	case orphanLimitStateOffBookUnadoptedFill:
		return 3
	case orphanLimitStateResting:
		return 2
	case orphanLimitStateUnknown:
		return 1
	default:
		return 0
	}
}

func (t *orphanLimitCancelTracker) Record(k orphanLimitCancelKey, state orphanLimitCancelState, now time.Time) bool {
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
	severity := orphanLimitCancelSeverity(state)
	windowOpen := !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) < effectiveAlertThrottleInterval()
	if windowOpen && severity <= e.notifiedSeverity {
		return false
	}
	e.lastNotifiedAt = now
	e.notifiedSeverity = severity
	return true
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

func orphanLimitCancelHeadline(state orphanLimitCancelState) string {
	switch state {
	case orphanLimitStateOffBookUnadoptedFill:
		return "limit order filled OFF-BOOK leaving an untracked Hyperliquid position"
	case orphanLimitStateOffBookRowStuck:
		return "limit order is off-book but its queue row could not be cleared"
	case orphanLimitStateResting:
		return "resting limit order NOT cancelled"
	default:
		return "resting limit order not confirmed cancelled"
	}
}

func orphanLimitCancelRetryNote(state orphanLimitCancelState) string {
	if state == orphanLimitStateOffBookUnadoptedFill {
		return "no automatic retry can change this — restore the strategy, or flatten on the exchange and clear the queue row with manual-clear-limit-row"
	}
	return "the reconciler will retry next cycle"
}

func formatOrphanLimitCancelDM(o PendingLimitOrder, block string, outcome orphanLimitCancelOutcome) string {
	var b strings.Builder
	switch outcome.State {
	case orphanLimitStateOffBookUnadoptedFill:
		fmt.Fprintf(&b, "🚨 **UNTRACKED HYPERLIQUID POSITION: %s**\n", killSwitchLimitOrderLabel(o))
		fmt.Fprintf(&b, "The order is OFF-BOOK — nothing is resting on the exchange and there is nothing left to cancel. It filled %.6f %s where the book holds only %.6f: %s.\n",
			outcome.ExchangeFill, o.Symbol, outcome.AdoptedFill, outcome.Reason)
		fmt.Fprintf(&b, "• Its owning strategy %q cannot adopt the fill because %s, so the position is open on Hyperliquid with NO stop-loss, NO take-profit and no owner in the book\n", o.StrategyID, block)
		b.WriteString("• Retrying cannot clear this: no automatic path can book the fill, and the queue row is kept as the recovery record so the fill is never orphaned\n")
		fmt.Fprintf(&b, "• RESTORE %q to this config as a Hyperliquid-live type=manual strategy — the only path the scheduler can finish on its own. It adopts the fill, arms protection, and clears this alert\n", o.StrategyID)
		fmt.Fprintf(&b, "• OR flatten the position yourself on the Hyperliquid UI, then run `go-trader manual-clear-limit-row %d --flattened`. The scheduler cannot see a flatten it does not own, and the order's fill history never changes, so this alert repeats every %s until the queue row is cleared\n",
			o.OrderOID, effectiveAlertThrottleInterval())
		b.WriteString("Do not flatten and then restore without clearing the row: the scheduler would adopt a fill that is no longer on the exchange.")
	case orphanLimitStateOffBookRowStuck:
		fmt.Fprintf(&b, "⚠️ **Limit order queue row not cleared: %s**\n", killSwitchLimitOrderLabel(o))
		fmt.Fprintf(&b, "The order is OFF-BOOK and nothing is resting on Hyperliquid, so no exchange action is needed: %s.\n", outcome.Reason)
		fmt.Fprintf(&b, "• Its owning strategy %q cannot adopt it because %s, so the cancel-only lane owns the row\n", o.StrategyID, block)
		b.WriteString("• This is a scheduler bookkeeping failure, and it is not exposure on the exchange\n")
		b.WriteString("The reconciler retries every cycle and clears the row once the write succeeds.")
	case orphanLimitStateResting:
		fmt.Fprintf(&b, "🚨 **Resting limit order NOT cancelled: %s**\n", killSwitchLimitOrderLabel(o))
		fmt.Fprintf(&b, "%s %.6f %s @ $%.4f (oid=%d) was marked for cancellation and IS STILL RESTING on Hyperliquid: %s.\n",
			o.Side, o.OrderSize, o.Symbol, o.LimitPrice, o.OrderOID, outcome.Reason)
		fmt.Fprintf(&b, "• Its owning strategy %q cannot adopt it because %s, so only the cancel-only lane can clear it\n", o.StrategyID, block)
		b.WriteString("• The order can still fill, putting exposure back on the exchange after an emergency stop\n")
		fmt.Fprintf(&b, "Cancel oid=%d on the Hyperliquid UI, or restore the strategy to this config as a Hyperliquid-live type=manual strategy so the scheduler resolves it.", o.OrderOID)
	default:
		fmt.Fprintf(&b, "🚨 **Resting limit order NOT confirmed cancelled: %s**\n", killSwitchLimitOrderLabel(o))
		fmt.Fprintf(&b, "%s %.6f %s @ $%.4f (oid=%d) was marked for cancellation and the scheduler could not read its on-chain state: %s.\n",
			o.Side, o.OrderSize, o.Symbol, o.LimitPrice, o.OrderOID, outcome.Reason)
		fmt.Fprintf(&b, "• Its owning strategy %q cannot adopt it because %s, so only the cancel-only lane can clear it\n", o.StrategyID, block)
		b.WriteString("• The order MAY still be resting on Hyperliquid and can still fill, putting exposure back on the exchange after an emergency stop\n")
		fmt.Fprintf(&b, "Check oid=%d on the Hyperliquid UI and cancel it if it is still resting, or restore the strategy to this config as a Hyperliquid-live type=manual strategy so the scheduler resolves it.", o.OrderOID)
	}
	return b.String()
}

func reportOrphanLimitCancel(notifier *MultiNotifier, o PendingLimitOrder, block string, outcome orphanLimitCancelOutcome, now time.Time) string {
	fmt.Printf("[CRITICAL] limit: %s %s: %s (its strategy cannot adopt it because %s) — %s\n",
		orphanLimitCancelHeadline(outcome.State), killSwitchLimitOrderLabel(o), outcome.Reason, block,
		orphanLimitCancelRetryNote(outcome.State))
	if !orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(o), outcome.State, now) {
		return ""
	}
	msg := formatOrphanLimitCancelDM(o, block, outcome)
	if notifier != nil && notifier.HasBackends() {
		notifier.SendOwnerDM(msg)
	}
	return msg
}

func orphanLimitCancelResolvedMessage(o PendingLimitOrder, block string, outcome orphanLimitCancelOutcome) string {
	action := fmt.Sprintf("found order oid=%d already off-book and sent no cancel", o.OrderOID)
	if outcome.CancelIssued {
		action = fmt.Sprintf("cancelled the resting order oid=%d", o.OrderOID)
	}
	fill := "with no fill"
	if outcome.AdoptedFill > 0 {
		fill = fmt.Sprintf("with its fill of %.6f already booked and kept in the book", outcome.AdoptedFill)
	}
	return fmt.Sprintf("[limit] %s %s: cancel-only lane %s %s, and cleared its queue row — %s, so no adoption path could clear it",
		o.StrategyID, o.Symbol, action, fill, block)
}
