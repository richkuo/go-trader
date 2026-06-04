package main

import (
	"fmt"
	"sync"
)

// armTrailingStopAtOpenNow places the initial TRAILING stop-loss on the SAME
// cycle as a fresh open (or the open side of a flip), closing the #885 naked
// window. The non-trailing ATR/regime/unified SL owners are already armed this
// cycle by the post-trade protection sync (buildHyperliquidProtectionPlan
// derives their trigger from the just-stamped EntryATR/Regime), and the scalar
// owners (stop_loss_pct, stop_loss_margin_pct, trailing_stop_pct) are placed
// inline at the execute order because EffectiveStopLossPct returns a positive
// pct for them. The ATR-trailing owners (trailing_stop_atr_mult /
// trailing_stop_atr_regime) are the gap: EffectiveStopLossPct defers to 0 (the
// distance needs per-position EntryATR) and buildHyperliquidProtectionPlan never
// reads the trailing fields, so the only path that arms them is the Signal==0
// trailing walker — which first fires the cycle AFTER open. On a long strategy
// interval that leaves the whole position with no exchange-side stop for up to a
// full interval.
//
// Correctness over a single steady-state path: it reuses the exact walker
// primitive (runHyperliquidTrailingStopUpdate) and result handler
// (applyTrailingStopUpdateResult) the next-cycle walker uses, so the inline
// trigger is byte-identical to what the deferred arming would have produced and
// there is only one stop-placement implementation that can't drift. A fresh
// position carries currentTrigger==0/currentOID==0, so the primitive computes
// the initial trigger (AvgCost-seeded high-water) and rests a new SL without
// needing forceResize.
//
// Idempotent and tightly scoped: it no-ops unless the position is a live
// trailing owner with NO resting SL (StopLossOID==0 && StopLossTriggerPx==0).
// That guard is what keeps it from double-placing on scalar trailing_stop_pct
// (inline OID already stamped at the execute order, main.go), on fixed/regime
// ATR (OID stamped by the post-trade sync), and on partial-close legs (the
// reduce-only SL keeps resting). It also means the post-open walker no-ops next
// cycle because the SL already exists.
//
// The #621 size cap uses the on-chain qty corrected with the just-confirmed open
// fill (the per-cycle reconcile snapshot predates this open); if still capped
// (e.g. shared-coin reconcile lag) it defers to the next walker cycle rather
// than rest an undersized stop — never worse than the pre-#885 deferred path.
func armTrailingStopAtOpenNow(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	preOpenOnChainAbsQty map[string]float64,
	filledQty float64,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) (int, string) {
	if !hyperliquidIsLive(sc.Args) || stratState == nil || symbol == "" || mark <= 0 {
		return 0, ""
	}
	mu.RLock()
	pos := stratState.Positions[symbol]
	if pos == nil || pos.Quantity <= 0 || effectiveTrailingStopPct(sc, pos) <= 0 {
		mu.RUnlock()
		return 0, ""
	}
	// Only arm when no SL is resting yet. A fresh open / flip-open leaves
	// StopLossOID==0 and StopLossTriggerPx==0; anything else (scalar SL placed
	// inline, fixed/regime SL placed by the sync, a partial close keeping its
	// reduce-only SL) is already protected and must not be double-placed.
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		mu.RUnlock()
		return 0, ""
	}
	side := pos.Side
	posSnap := *pos
	mu.RUnlock()

	// #621 size cap with the on-chain qty corrected for the just-confirmed open
	// fill (the Phase-1 reconcile snapshot predates this open).
	grownOnChain := map[string]float64{symbol: preOpenOnChainAbsQty[symbol] + filledQty}
	slEffectiveQty, capped := hlSLEffectiveQty(symbol, posSnap.Quantity, grownOnChain)
	if capped {
		logger.Warn("open trailing SL arm: %s still capped (virtual %.6f > on-chain %.6f); deferring initial trailing SL to next walker cycle", symbol, posSnap.Quantity, slEffectiveQty)
		return 0, ""
	}

	// currentTrigger==0 / currentOID==0: the primitive computes the initial
	// trigger and rests a fresh SL (no forceResize needed).
	newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, symbol, side, slEffectiveQty, &posSnap, mark, 0, 0, 0, false, notifier, logger)
	mu.Lock()
	defer mu.Unlock()
	if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, 0, newHighWater, updateConfirmed, slUpdate, logger); immediateFill {
		return 1, fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx)
	}
	if updateConfirmed && slUpdate != nil && slUpdate.StopLossOID > 0 {
		logger.Info("Trailing SL armed inline at open for %s (qty=%.6f)", symbol, slEffectiveQty)
	}
	return 0, ""
}
