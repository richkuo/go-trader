package main

import (
	"fmt"
	"time"
)

// ScaleInPolicy gates opt-in strategy scale-in (#873). Zero value disables
// scale-in. Manual-add ignores this struct — operator intent is explicit.
type ScaleInPolicy struct {
	Allowed        bool
	MaxAdds        int     // 0 = unlimited
	MaxNotionalUSD float64 // 0 = unlimited per add
}

// ScaleInPolicyFromConfig derives runtime policy from strategy config.
func ScaleInPolicyFromConfig(sc StrategyConfig) ScaleInPolicy {
	p := ScaleInPolicy{Allowed: sc.AllowScaleIn}
	if sc.MaxScaleInAdds != nil {
		p.MaxAdds = *sc.MaxScaleInAdds
	}
	if sc.MaxScaleInNotionalUSD != nil {
		p.MaxNotionalUSD = *sc.MaxScaleInNotionalUSD
	}
	return p
}

// PerpsScaleInIntent reports whether (signal, posSide, direction) is a
// same-direction re-entry that should scale into the open leg rather than skip.
func PerpsScaleInIntent(signal int, posSide, direction string, allowScaleIn bool) bool {
	if !allowScaleIn || posSide == "" {
		return false
	}
	switch direction {
	case DirectionLong, "":
		return signal == 1 && posSide == "long"
	case DirectionShort:
		return signal == -1 && posSide == "short"
	case DirectionBoth:
		return (signal == 1 && posSide == "long") || (signal == -1 && posSide == "short")
	}
	return false
}

// blendPositionScaleIn updates pos in place for a scale-in add. EntryATR,
// regime label, and TP tier geometry stay frozen — only avg cost and sizes move.
func blendPositionScaleIn(pos *Position, addQty, addPrice float64) {
	if pos == nil || addQty <= 0 || addPrice <= 0 {
		return
	}
	oldQty := pos.Quantity
	if pos.InitialQuantity <= 0 {
		pos.InitialQuantity = oldQty
	}
	pos.AvgCost = (oldQty*pos.AvgCost + addQty*addPrice) / (oldQty + addQty)
	pos.Quantity = oldQty + addQty
	pos.InitialQuantity += addQty
}

func countScaleInAdds(s *StrategyState, positionID string) int {
	if s == nil || positionID == "" {
		return 0
	}
	n := 0
	for _, t := range s.TradeHistory {
		if t.PositionID == positionID && t.IsScaleIn && !t.IsClose {
			n++
		}
	}
	return n
}

func clampScaleInAddQty(addQty, price float64, policy ScaleInPolicy) float64 {
	if addQty <= 0 || price <= 0 || policy.MaxNotionalUSD <= 0 {
		return addQty
	}
	capQty := policy.MaxNotionalUSD / price
	if addQty > capQty {
		return capQty
	}
	return addQty
}

func scaleInBlockedReason(s *StrategyState, pos *Position, policy ScaleInPolicy, addQty, addPrice float64) string {
	if !policy.Allowed {
		return "scale-in not enabled for strategy"
	}
	if pos == nil {
		return "no position to scale into"
	}
	if policy.MaxAdds > 0 && countScaleInAdds(s, pos.TradePositionID) >= policy.MaxAdds {
		return fmt.Sprintf("max scale-in adds (%d) reached", policy.MaxAdds)
	}
	if policy.MaxNotionalUSD > 0 && addQty > 0 && addPrice > 0 {
		if addQty*addPrice > policy.MaxNotionalUSD+1e-9 {
			return fmt.Sprintf("scale-in notional $%.2f exceeds cap $%.2f", addQty*addPrice, policy.MaxNotionalUSD)
		}
	}
	return ""
}

// scaleInPreExecBlockedReason mirrors scaleInBlockedReason for the live order
// spawn guard — must run before RunHyperliquidExecute cancels resting SL/TP (#873).
func scaleInPreExecBlockedReason(s *StrategyState, pos *Position, policy ScaleInPolicy, addQty, addPrice float64) string {
	if !policy.Allowed || pos == nil {
		return ""
	}
	return scaleInBlockedReason(s, pos, policy, addQty, addPrice)
}

// scaleInLiveProtectionRearmSupported reports whether on-chain TP/SL can be
// re-sized after a scale-in add cancels resting orders. Scalar pct/margin SLs
// placed via --stop-loss-pct are not re-armed by buildHyperliquidProtectionPlan.
func scaleInLiveProtectionRearmSupported(sc StrategyConfig) bool {
	if !hyperliquidIsLive(sc.Args) {
		return true
	}
	if strategyUsesTieredTPATRClose(sc) {
		return true
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		return true
	}
	if sc.StopLossATRRegime != nil && sc.StopLossATRRegime.IsConfigured() {
		return true
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		return true
	}
	if sc.TrailingStopATRRegime != nil && sc.TrailingStopATRRegime.IsConfigured() {
		return true
	}
	if strategyUsesUnifiedRegimeClose(sc) {
		return true
	}
	if strategyUsesTrailingTPRatchetClose(sc) {
		return true
	}
	return false
}

func previewBlendedAvgCost(oldQty, oldAvg, addQty, addPrice float64) float64 {
	if oldQty+addQty <= 0 {
		return addPrice
	}
	return (oldQty*oldAvg + addQty*addPrice) / (oldQty + addQty)
}

func executePerpsScaleIn(
	s *StrategyState,
	pos *Position,
	symbol string,
	signal int,
	price float64,
	sizingLeverage, exchangeLeverage, marginPerTradeUSD float64,
	fillQty float64,
	fillOID string,
	fillFee float64,
	policy ScaleInPolicy,
	feePlatform string,
	leverageLabel string,
	logger *StrategyLogger,
	recordOpen func(Trade),
) (int, error) {
	if s.Cash < 1 {
		logger.Info("Insufficient cash ($%.2f) to scale into %s perp", s.Cash, symbol)
		return 0, nil
	}
	var execPrice, addQty float64
	if fillQty > 0 {
		execPrice = price
		addQty = fillQty
	} else {
		execPrice = ApplySlippage(price)
		if execPrice <= 0 {
			return 0, nil
		}
		budget := PerpsOpenNotional(s.Cash, sizingLeverage, exchangeLeverage, marginPerTradeUSD)
		addQty = clampScaleInAddQty(budget/execPrice, execPrice, policy)
	}
	if addQty <= 0 {
		return 0, nil
	}
	if reason := scaleInBlockedReason(s, pos, policy, addQty, execPrice); reason != "" {
		logger.Info("Scale-in blocked for %s: %s", symbol, reason)
		return 0, nil
	}
	notional := addQty * execPrice
	useFillFee := fillQty > 0
	fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), fillFee, useFillFee)
	s.Cash -= fee

	blendPositionScaleIn(pos, addQty, execPrice)

	positionID := ensurePositionTradeID(s.ID, symbol, pos)
	tradeSide := "buy"
	if signal == -1 {
		tradeSide = "sell"
	}
	var openOID string
	if useFillFee {
		openOID = fillOID
	}
	trade := Trade{
		Timestamp:         time.Now().UTC(),
		StrategyID:        s.ID,
		Symbol:            symbol,
		PositionID:        positionID,
		Side:              tradeSide,
		Quantity:          addQty,
		Price:             execPrice,
		Value:             notional,
		TradeType:         "perps",
		Details:           fmt.Sprintf("Scale-in %s +%.6f @ $%.2f (blended avg $%.4f, total %.6f, %s, fee $%.2f)", pos.Side, addQty, execPrice, pos.AvgCost, pos.Quantity, leverageLabel, fee),
		ExchangeOrderID:   openOID,
		ExchangeFee:       exchangeFeeForTrade(fillFee, useFillFee),
		IsScaleIn:         true,
		EntryATR:          pos.EntryATR,
		StopLossTriggerPx: pos.StopLossTriggerPx,
		StopLossATRMult:   pos.StopLossATRMult,
		TPTiersJSON:       pos.TPTiersJSON,
		Regime:            pos.Regime,
	}
	recordOpen(trade)
	logger.Info("Scale-in %s: +%.6f @ $%.2f (blended avg $%.4f, total %.6f, fee $%.2f)", symbol, addQty, execPrice, pos.AvgCost, pos.Quantity, fee)
	return 1, nil
}
