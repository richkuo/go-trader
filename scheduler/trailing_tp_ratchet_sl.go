package main

import (
	"fmt"
	"sync"
)

var ratchetTightenReplacePolicy = trailingReplacePolicy{ratchetTightened: true}

func runTrailingStopUpdateAfterRatchetTighten(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	hlOnChainAbsQty map[string]float64,

	hlLiquidationPx map[string]float64,
	hlNetSideByCoin map[string]string,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) (int, string) {
	if stratState == nil || symbol == "" || mark <= 0 {
		return 0, ""
	}
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return 0, ""
	}

	mu.RLock()
	pos := stratState.Positions[symbol]
	if pos == nil || pos.Quantity <= 0 || effectiveTrailingStopPct(sc, pos) <= 0 {
		mu.RUnlock()
		return 0, ""
	}
	side := pos.Side
	highWater := pos.StopLossHighWaterPx
	triggerPx := pos.StopLossTriggerPx
	slOID := pos.StopLossOID
	qty := pos.Quantity
	posSnap := *pos
	mu.RUnlock()

	if hyperliquidIsLive(sc.Args) {
		slEffectiveQty, capped := hlSLEffectiveQty(symbol, qty, hlOnChainAbsQty)
		if capped && logger != nil {
			logger.Warn("ratchet same-cycle trailing SL: virtual qty %.6f > on-chain %.6f for %s; capping (#621)", qty, slEffectiveQty, symbol)
		}

		livePolicy := ratchetTightenReplacePolicy
		livePolicy.liquidationPx = hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, symbol, side)
		newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(
			sc, symbol, side, slEffectiveQty, &posSnap, mark, highWater, triggerPx, slOID, livePolicy, notifier, logger)
		mu.Lock()
		defer mu.Unlock()
		if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, slOID, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, 0); immediateFill {
			return 1, fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx)
		}
		return 0, ""
	}

	newHighWater, newTrigger, breach, breachPx := runHyperliquidTrailingStopPaper(sc, side, &posSnap, mark, highWater, triggerPx, ratchetTightenReplacePolicy)
	mu.Lock()
	defer mu.Unlock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.Side != side {
		return 0, ""
	}
	if breach {
		if recordPerpsStopLossClose(stratState, symbol, breachPx, "trailing_stop_loss_paper", logger) {
			return 1, fmt.Sprintf("[%s] PAPER TRAILING SL %s @ $%.2f", sc.ID, symbol, breachPx)
		}
		return 0, ""
	}
	if newHighWater > 0 {
		pos.StopLossHighWaterPx = newHighWater
	}
	if newTrigger > 0 {
		pos.StopLossTriggerPx = newTrigger
		pos.RatchetFallbackNormalizePending = false
		if logger != nil {
			logger.Info("Paper trailing SL trigger updated after ratchet @ $%.4f (high_water=$%.4f)", newTrigger, newHighWater)
		}
	}
	return 0, ""
}
