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
	share *hlCycleShare,
	hlLiquidationPx map[string]float64,
	hlNetSideByCoin map[string]string,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) (int, string, ratchetStopEvidence) {
	if stratState == nil || symbol == "" || mark <= 0 {
		return 0, "", ratchetStopEvidence{}
	}
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return 0, "", ratchetStopEvidence{}
	}

	mu.RLock()
	pos := stratState.Positions[symbol]
	if pos == nil || pos.Quantity <= 0 || effectiveTrailingStopPct(sc, pos) <= 0 {
		mu.RUnlock()
		return 0, "", ratchetStopEvidence{}
	}
	side := pos.Side
	highWater := pos.StopLossHighWaterPx
	triggerPx := pos.StopLossTriggerPx
	slOID := pos.StopLossOID
	qty := pos.Quantity
	armed := hlBookArmed(pos)
	var peers []hlShareBook
	var opp float64
	if share != nil {
		peers, opp = share.peers(symbol, sc.ID, side)
	}
	posSnap := *pos
	posSnap.TPConsumptions = cloneTPConsumptions(pos.TPConsumptions)
	mu.RUnlock()

	if hyperliquidIsLive(sc.Args) {
		q := hlStopQty{Qty: qty, Fresh: true}
		if share != nil {
			q = share.StopQty(sc, symbol, side, qty, armed, peers, opp)
		}
		slEffectiveQty, capped, place := hlReplaceQty(q, qty)
		if !place {
			return 0, "", ratchetStopEvidence{}
		}
		if capped && logger != nil {
			logger.Warn("ratchet same-cycle trailing SL: virtual qty %.6f > chain share %.6f for %s; capping", qty, slEffectiveQty, symbol)
		}
		livePolicy := ratchetTightenReplacePolicy
		livePolicy.liquidationPx = hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, symbol, side)
		newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(
			sc, symbol, side, slEffectiveQty, &posSnap, mark, highWater, triggerPx, slOID, livePolicy, notifier, logger)
		ev := ratchetStopEvidence{Ran: true, Live: true, Confirmed: updateConfirmed, Result: slUpdate}
		mu.Lock()
		defer mu.Unlock()
		if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, slOID, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, slEffectiveQty); immediateFill {
			return 1, fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx), ev
		}
		return 0, "", ev
	}

	newHighWater, newTrigger, breach, breachPx := runHyperliquidTrailingStopPaper(sc, side, &posSnap, mark, highWater, triggerPx, ratchetTightenReplacePolicy)
	ev := ratchetStopEvidence{Ran: true, Live: false}
	mu.Lock()
	defer mu.Unlock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.Side != side {
		return 0, "", ev
	}
	if breach {
		if newTrigger > 0 {
			pos.StopLossTriggerPx = newTrigger
		}
		breachPx = paperStopBookPx(side, breachPx)
		if recordPerpsStopLossClose(stratState, symbol, breachPx, paperStopReasonTrailing, logger) {
			return 1, fmt.Sprintf("[%s] PAPER TRAILING SL %s @ $%.2f", sc.ID, symbol, breachPx), ev
		}
		return 0, "", ev
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
	return 0, "", ev
}
