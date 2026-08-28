package main

import (
	"fmt"
	"sync"
	"time"
)

func scaleInResizeTrailingSLNow(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	preAddOnChainAbsQty map[string]float64,
	hlLiquidationPx map[string]float64,
	hlNetSideByCoin map[string]string,
	filledAddQty float64,
	ratchetTightened bool,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) (int, string) {
	if !hyperliquidIsLive(sc.Args) || stratState == nil || symbol == "" || mark <= 0 {
		return 0, ""
	}
	mu.RLock()
	pos := stratState.Positions[symbol]
	if pos == nil || pos.Quantity <= 0 || !pos.ScaleInResizePending || effectiveTrailingStopPct(sc, pos) <= 0 {
		mu.RUnlock()
		return 0, ""
	}
	side := pos.Side
	highWater := pos.StopLossHighWaterPx
	triggerPx := pos.StopLossTriggerPx
	slOID := pos.StopLossOID
	posSnap := *pos
	mu.RUnlock()

	grownOnChain := map[string]float64{symbol: preAddOnChainAbsQty[symbol] + filledAddQty}
	slEffectiveQty, capped := hlSLEffectiveQty(symbol, posSnap.Quantity, grownOnChain)
	if capped {
		logger.Warn("scale-in eager SL resize: %s still capped (virtual %.6f > on-chain %.6f); deferring to next walker cycle", symbol, posSnap.Quantity, slEffectiveQty)
		return 0, ""
	}
	newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, symbol, side, slEffectiveQty, &posSnap, mark, highWater, triggerPx, slOID, trailingReplacePolicy{forceResize: true, ratchetTightened: ratchetTightened, liquidationPx: hlLiquidationPxForSide(hlLiquidationPx, hlNetSideByCoin, symbol, side)}, notifier, logger)
	mu.Lock()
	defer mu.Unlock()
	trades := 0
	detail := ""
	if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, slOID, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, 0); immediateFill {
		trades = 1
		detail = fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx)
	}
	if updateConfirmed {
		if p, ok := stratState.Positions[symbol]; ok && p != nil {
			p.ScaleInResizePending = false
			logger.Info("Scale-in trailing SL re-sized same-cycle (qty=%.6f)", slEffectiveQty)
		}
	}
	return trades, detail
}

const scaleInTradeType = "scale_in"

func applyScaleIn(pos *Position, addQty, addPrice float64) {
	if pos == nil || addQty <= 0 || addPrice <= 0 {
		return
	}
	if pos.RiskAnchorPrice <= 0 {
		pos.RiskAnchorPrice = pos.AvgCost
	}
	oldQty := pos.Quantity
	newQty := oldQty + addQty
	if newQty > 0 {
		pos.AvgCost = (oldQty*pos.AvgCost + addQty*addPrice) / newQty
	}
	pos.Quantity = newQty
	pos.InitialQuantity += addQty
	pos.ScaleInCount++
	pos.LastAddPrice = addPrice
	pos.AddedNotionalUSD += addQty * addPrice
	pos.ScaleInResizePending = true
}

func scaleInLiveProtectionResizable(sc StrategyConfig) bool {
	trailing := (sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0) ||
		(sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0) ||
		(sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero()) ||
		strategyUsesTrailingTPRatchetClose(sc)
	if trailing {
		return true
	}
	if EffectiveStopLossPct(sc) > 0 {
		return false
	}
	return true
}

type scaleInSnapshot struct {
	Side             string
	Quantity         float64
	AvgCost          float64
	EntryATR         float64
	ScaleInCount     int
	AddedNotionalUSD float64
	LastAddPrice     float64
}

func perpsScaleInDecision(sc StrategyConfig, snap scaleInSnapshot, signal int, price, defaultOpenNotionalUSD float64) (addQty float64, ok bool, reason string) {
	if !sc.AllowScaleIn {
		return 0, false, "scale-in not enabled"
	}
	if price <= 0 {
		return 0, false, "no price for scale-in"
	}
	switch {
	case signal == 1 && snap.Side == "long" && snap.Quantity > 0:
	case signal == -1 && snap.Side == "short" && snap.Quantity > 0:
	default:
		return 0, false, "not a same-direction add"
	}

	var cfg ScaleInConfig
	if sc.ScaleIn != nil {
		cfg = *sc.ScaleIn
	}

	if cfg.MaxAdds > 0 && snap.ScaleInCount >= cfg.MaxAdds {
		return 0, false, "scale-in max_adds reached"
	}

	addNotional := defaultOpenNotionalUSD
	if cfg.AddNotionalUSD > 0 {
		addNotional = cfg.AddNotionalUSD
	}
	if addNotional <= 0 {
		return 0, false, "scale-in add notional resolves to zero"
	}
	if cfg.MaxAddedNotionalUSD > 0 && snap.AddedNotionalUSD+addNotional > cfg.MaxAddedNotionalUSD+1e-9 {
		return 0, false, "scale-in max_added_notional_usd reached"
	}

	if cfg.AddSpacingATR != 0 {
		if snap.EntryATR <= 0 {
			return 0, false, "scale-in spacing requires a positive EntryATR"
		}
		lastAdd := snap.LastAddPrice
		if lastAdd <= 0 {
			lastAdd = snap.AvgCost
		}
		dir := 1.0
		if snap.Side == "short" {
			dir = -1.0
		}
		favorableMove := (price - lastAdd) * dir
		needed := cfg.AddSpacingATR * snap.EntryATR
		if cfg.AddSpacingATR > 0 {
			if favorableMove+1e-9 < needed {
				return 0, false, "scale-in spacing (add-to-winners) not reached"
			}
		} else {
			if -favorableMove+1e-9 < -needed {
				return 0, false, "scale-in spacing (average-down) not reached"
			}
		}
	}

	return addNotional / price, true, ""
}

func applyPerpsScaleIn(s *StrategyState, sc StrategyConfig, symbol string, addPrice, addQty, fillFee float64, fillOID string, useFillFee bool, logger *StrategyLogger) (int, *Trade) {
	if addQty <= 0 || addPrice <= 0 {
		return 0, nil
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil {
		if useFillFee {
			logger.Error("scale-in fill (oid=%s qty=%.6f @ $%.2f) has no position to apply to for %s — fill booked on-chain with NO Trade record", fillOID, addQty, addPrice, symbol)
		}
		return 0, nil
	}
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}
	notional := addQty * addPrice
	fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), fillFee, useFillFee)
	s.Cash -= fee
	side := "buy"
	if pos.Side == "short" {
		side = "sell"
	}
	applyScaleIn(pos, addQty, addPrice)
	now := time.Now().UTC()
	var oid string
	if useFillFee {
		oid = fillOID
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          symbol,
		PositionID:      ensurePositionTradeID(s.ID, symbol, pos),
		Side:            side,
		Quantity:        addQty,
		Price:           addPrice,
		Value:           notional,
		TradeType:       scaleInTradeType,
		Details:         fmt.Sprintf("Scale-in %s %.6f @ $%.2f (add #%d, new qty %.6f, avg $%.2f, fee $%.2f)", pos.Side, addQty, addPrice, pos.ScaleInCount, pos.Quantity, pos.AvgCost, fee),
		ExchangeOrderID: oid,
		ExchangeFee:     fee,
		FeeSource:       executionFeeSource(fillFee, useFillFee),
		PnLGross:        true,
		IsClose:         false,
	}
	trade.Regime = pos.Regime
	trade.EntryATR = pos.EntryATR
	logger.Info("SCALE-IN %s: +%.6f @ $%.2f (new qty %.6f, avg $%.2f, add #%d, fee $%.2f)", symbol, addQty, addPrice, pos.Quantity, pos.AvgCost, pos.ScaleInCount, fee)
	return 1, &trade
}

func scaleInProtectionForceReplace(pos *Position, plan hlProtectionPlan) (forceSL bool, forceTP []bool) {
	forceSL = plan.StopLossATRMult > 0
	if len(plan.Tiers) == 0 {
		return forceSL, nil
	}
	forceTP = make([]bool, len(plan.Tiers))
	for i := range plan.Tiers {
		if i < len(pos.TPOIDs) && pos.TPOIDs[i] > 0 {
			forceTP[i] = true
		}
	}
	return forceSL, forceTP
}

func orForceReplace(a, b []bool) []bool {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	if n == 0 {
		return nil
	}
	out := make([]bool, n)
	for i := 0; i < n; i++ {
		ai := i < len(a) && a[i]
		bi := i < len(b) && b[i]
		out[i] = ai || bi
	}
	return out
}

func runHyperliquidScaleInOrder(sc StrategyConfig, result *HyperliquidResult, addSize float64, walletSnapshot hlExecuteSnapshot, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidExecuteResult, bool) {
	side := "buy"
	if result.Signal == -1 {
		side = "sell"
	}
	logger.Info("Placing live scale-in %s %s size=%.6f", side, result.Symbol, addSize)
	execResult, stderr, err := RunHyperliquidExecute(sc.Script, result.Symbol, side, addSize, 0, 0, 0, "", 0, false, walletSnapshot)
	if stderr != "" {
		logger.Info("execute stderr: %s", stderr)
	}
	if err != nil {
		logger.Error("Live scale-in failed: %v", err)
		notifyLiveExecFailure(notifier, sc, directionOpen, result.Symbol, err.Error())
		return execResult, false
	}
	if execResult.Error != "" {
		logger.Error("Live scale-in returned error: %s", execResult.Error)
		notifyLiveExecFailure(notifier, sc, directionOpen, result.Symbol, execResult.Error)
		return execResult, false
	}
	clearLiveExecThrottle(sc, directionOpen, result.Symbol)
	return execResult, true
}

func executeHyperliquidScaleInDeferredOpen(sc StrategyConfig, s *StrategyState, result *HyperliquidResult, execResult *HyperliquidExecuteResult, signalStr string, price, addQty float64, logger *StrategyLogger) (int, string, *Trade, *RatchetTriggerAlert) {
	fillPrice := price
	fillAddQty := addQty
	var fillOID string
	var fillFee float64
	useFillFee := false
	if execResult != nil && execResult.Execution != nil && execResult.Execution.Fill != nil {
		fill := execResult.Execution.Fill
		if fill.AvgPx > 0 {
			fillPrice = fill.AvgPx
		}
		if fill.TotalSz > 0 {
			fillAddQty = fill.TotalSz
		}
		if fill.OID != 0 {
			fillOID = fmt.Sprintf("%d", fill.OID)
		}
		fillFee = fill.Fee
		useFillFee = true
		logger.Info("Live scale-in fill at $%.2f qty=%.6f (mid was $%.2f)", fillPrice, fillAddQty, price)
	}
	trades, openTrade := applyPerpsScaleIn(s, sc, result.Symbol, fillPrice, fillAddQty, fillFee, fillOID, useFillFee, logger)
	var ratchetAlert *RatchetTriggerAlert
	if trades > 0 {
		if pos, ok := s.Positions[result.Symbol]; ok {
			_, ratchetAlert = applyTrailingTPRatchetToPosition(sc, pos, result.Symbol, price, logger)
		}
	}
	detail := ""
	if trades > 0 {
		prefix := ""
		if execResult != nil {
			prefix = "LIVE "
		}
		detail = fmt.Sprintf("[%s] %sSCALE-IN %s @ $%.2f", sc.ID, prefix, result.Symbol, fillPrice)
	}
	if execResult == nil {
		var pos *Position
		if p, ok := s.Positions[result.Symbol]; ok {
			pos = p
		}
		if recordPositionOpen(s, sc, openTrade, pos) {
			openTrade = nil
		}
	}
	return trades, detail, openTrade, ratchetAlert
}
