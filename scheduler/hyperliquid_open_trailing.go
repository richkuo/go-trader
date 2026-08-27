package main

import (
	"fmt"
	"sync"
)

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
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		mu.RUnlock()
		return 0, ""
	}
	side := pos.Side
	posSnap := *pos
	mu.RUnlock()

	grownOnChain := map[string]float64{symbol: preOpenOnChainAbsQty[symbol] + filledQty}
	slEffectiveQty, capped := hlSLEffectiveQty(symbol, posSnap.Quantity, grownOnChain)
	if capped {
		logger.Warn("open trailing SL arm: %s still capped (virtual %.6f > on-chain %.6f); deferring initial trailing SL to next walker cycle", symbol, posSnap.Quantity, slEffectiveQty)
		return 0, ""
	}

	newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, symbol, side, slEffectiveQty, &posSnap, mark, 0, 0, 0, trailingReplacePolicy{}, notifier, logger)
	mu.Lock()
	defer mu.Unlock()
	if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, 0, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, 0); immediateFill {
		return 1, fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx)
	}
	if updateConfirmed && slUpdate != nil && slUpdate.StopLossOID > 0 {
		logger.Info("Trailing SL armed inline at open for %s (qty=%.6f)", symbol, slEffectiveQty)
	}
	return 0, ""
}
