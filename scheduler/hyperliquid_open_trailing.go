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
	share *hlCycleShare,
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
	book := pos.Quantity
	armed := hlBookArmed(pos)
	var peers []hlShareBook
	var opp float64
	if share != nil {
		peers, opp = share.peers(symbol, sc.ID, side)
	}
	posSnap := *pos
	mu.RUnlock()

	q := hlStopQty{Qty: book, Fresh: true}
	if share != nil {
		q = share.StopQty(sc, symbol, side, book, armed, peers, opp)
	}
	slEffectiveQty, deferArm, place := hlFreshArmQty(q, book)
	if deferArm {
		if logger != nil {
			logger.Warn("open trailing SL arm: %s account read failed after the fill; deferring initial trailing SL to next walker cycle", symbol)
		}
		return 0, ""
	}
	if !place {
		return 0, ""
	}

	newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, symbol, side, slEffectiveQty, &posSnap, mark, 0, 0, 0, trailingReplacePolicy{}, notifier, logger)
	mu.Lock()
	defer mu.Unlock()
	if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, 0, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, slEffectiveQty); immediateFill {
		return 1, fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx)
	}
	if updateConfirmed && slUpdate != nil && slUpdate.StopLossOID > 0 {
		logger.Info("Trailing SL armed inline at open for %s (qty=%.6f)", symbol, slEffectiveQty)
	}
	return 0, ""
}
