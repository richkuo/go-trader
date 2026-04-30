package main

import (
	"fmt"
	"math"
)

const defaultTrailingStopMinMovePct = 0.5

var runHyperliquidUpdateStopLossFunc = RunHyperliquidUpdateStopLoss

func effectiveTrailingStopPct(sc StrategyConfig) float64 {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" || sc.TrailingStopPct == nil {
		return 0
	}
	if *sc.TrailingStopPct > 0 {
		return *sc.TrailingStopPct
	}
	return 0
}

func computeTrailingStopUpdate(side string, mark, highWater, trailingPct, minMovePct, currentTrigger float64) (float64, float64, bool) {
	if mark <= 0 || trailingPct <= 0 {
		return highWater, 0, false
	}
	if highWater <= 0 {
		highWater = mark
	}

	candidateHighWater := highWater
	switch side {
	case "long":
		if mark > candidateHighWater {
			candidateHighWater = mark
		}
	case "short":
		if mark < candidateHighWater {
			candidateHighWater = mark
		}
	default:
		return highWater, 0, false
	}
	if candidateHighWater <= 0 {
		return highWater, 0, false
	}

	var candidateTrigger float64
	switch side {
	case "long":
		candidateTrigger = candidateHighWater * (1.0 - trailingPct/100.0)
	case "short":
		candidateTrigger = candidateHighWater * (1.0 + trailingPct/100.0)
	}
	if candidateTrigger <= 0 {
		return candidateHighWater, 0, false
	}
	if currentTrigger <= 0 {
		return candidateHighWater, candidateTrigger, true
	}

	favorable := (side == "long" && candidateTrigger > currentTrigger) ||
		(side == "short" && candidateTrigger < currentTrigger)
	if !favorable {
		return candidateHighWater, 0, false
	}
	movePct := math.Abs(candidateTrigger-currentTrigger) / currentTrigger * 100.0
	if movePct >= minMovePct {
		return candidateHighWater, candidateTrigger, true
	}
	return candidateHighWater, 0, false
}

func runHyperliquidTrailingStopUpdate(sc StrategyConfig, symbol, side string, qty, avgCost, mark, highWater, currentTrigger float64, currentOID int64, notifier *MultiNotifier, logger *StrategyLogger) (float64, *HyperliquidStopLossUpdateResult, bool) {
	trailingPct := effectiveTrailingStopPct(sc)
	if trailingPct <= 0 || qty <= 0 || mark <= 0 {
		return highWater, nil, true
	}
	if highWater <= 0 {
		highWater = avgCost
	}
	newHighWater, newTrigger, replace := computeTrailingStopUpdate(side, mark, highWater, trailingPct, defaultTrailingStopMinMovePct, currentTrigger)
	if !replace {
		return newHighWater, nil, true
	}

	logger.Info("Updating trailing SL for %s: side=%s mark=$%.4f high_water=$%.4f trigger=$%.4f cancel_oid=%d",
		symbol, side, mark, newHighWater, newTrigger, currentOID)
	result, stderr, err := runHyperliquidUpdateStopLossFunc(sc.Script, symbol, side, qty, newTrigger, currentOID)
	if stderr != "" {
		logger.Info("update stop-loss stderr: %s", stderr)
	}
	if err != nil {
		logger.Error("Trailing SL update failed: %v", err)
		return newHighWater, result, false
	}
	if result.Error != "" {
		logger.Error("Trailing SL update returned error: %s", result.Error)
		return newHighWater, result, false
	}
	if result.CancelStopLossError != "" {
		logger.Warn("Trailing SL cancel failed (non-fatal): %s", result.CancelStopLossError)
	}
	if result.StopLossError != "" {
		if isHLOpenOrderCapRejection(result.StopLossError) {
			logger.Error("CRITICAL: HL open-order-cap rejected trailing SL update for %s - position may be under-protected: %s",
				symbol, result.StopLossError)
			if notifier != nil && notifier.HasBackends() {
				msg := fmt.Sprintf("**HL OPEN-ORDER CAP HIT** [%s] %s trailing SL update rejected: %s",
					sc.ID, symbol, result.StopLossError)
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}
		} else {
			logger.Warn("Trailing SL placement failed (non-fatal): %s", result.StopLossError)
		}
	}
	if result.StopLossFilledImmediately {
		logger.Warn("Trailing SL trigger filled at submit for %s — position is flat on-chain", symbol)
	}
	return newHighWater, result, true
}
