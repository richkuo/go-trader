package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
)

const (
	hlVenueMinOrderNotionalUSD    = 10.0
	hlVenueMinOrderNotionalMargin = 0.03
	hlSharedCloseStrandedGate     = "shared_coin_stranded_remainder"
	hlSharedCloseDeferredGate     = "shared_coin_on_chain_unknown"
	hlSharedCloseQtyTolerance     = 1e-6
	hlSharedCloseHoldPeerBusy     = "peer_busy"
	hlSharedCloseHoldVenueReject  = "venue_rejected"
)

type hlSharedCloseFloorOutcome int

const (
	hlSharedCloseFloorNone hlSharedCloseFloorOutcome = iota
	hlSharedCloseFloorEscalate
	hlSharedCloseFloorHold
	hlSharedCloseFloorHeld
	hlSharedCloseFloorDefer
)

func (o hlSharedCloseFloorOutcome) String() string {
	switch o {
	case hlSharedCloseFloorEscalate:
		return "escalate"
	case hlSharedCloseFloorHold:
		return "hold"
	case hlSharedCloseFloorHeld:
		return "held"
	case hlSharedCloseFloorDefer:
		return "defer"
	default:
		return "none"
	}
}

type hlOnChainCoinView struct {
	Known   bool
	AbsQty  map[string]float64
	NetSide map[string]string
}

func hlPeerVirtualQtyOnCoin(snapshot hlVirtualQuantitySnapshot, coin, selfID string) float64 {
	target := strings.ToUpper(strings.TrimSpace(coin))
	total := 0.0
	for rawCoin, byID := range snapshot {
		if strings.ToUpper(strings.TrimSpace(rawCoin)) != target {
			continue
		}
		for id, qty := range byID {
			if id != selfID && qty > 0 {
				total += qty
			}
		}
	}
	return total
}

func hlVenueCloseGateThresholdUSD() float64 {
	return hlVenueMinOrderNotionalUSD * (1.0 + hlVenueMinOrderNotionalMargin)
}

func hlOnChainCoinViewFromPositions(positions []HLPosition) hlOnChainCoinView {
	absQty, _, netSide := buildHLLiquidationMaps(positions)
	return hlOnChainCoinView{Known: true, AbsQty: absQty, NetSide: netSide}
}

func hlPeerFlatOnCoin(symbol, posSide string, posQty float64, onChain hlOnChainCoinView) bool {
	coin := strings.TrimSpace(symbol)
	onChainQty := onChain.AbsQty[coin]
	if onChainQty <= 1e-9 {
		return true
	}
	if side, ok := onChain.NetSide[coin]; ok && side != posSide {
		return false
	}
	return onChainQty <= posQty+hlSharedCloseQtyTolerance
}

func evaluateSharedCoinFullCloseFloor(closeFraction float64, symbol string, hlLiveAll []StrategyConfig, posQty float64, posSide string, price float64, onChain hlOnChainCoinView, peerVirtualQty float64, heldReason string) (hlSharedCloseFloorOutcome, float64) {
	if closeFraction != 1.0 || posQty <= 0 || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return hlSharedCloseFloorNone, 0
	}
	if len(hlLiveStrategiesForCoin(symbol, hlLiveAll)) <= 1 {
		return hlSharedCloseFloorNone, 0
	}
	remainderUSD := posQty * price
	if remainderUSD >= hlVenueCloseGateThresholdUSD() {
		return hlSharedCloseFloorNone, remainderUSD
	}
	if heldReason == hlSharedCloseHoldVenueReject {
		return hlSharedCloseFloorHeld, remainderUSD
	}
	if !onChain.Known {
		return hlSharedCloseFloorDefer, remainderUSD
	}
	if peerVirtualQty <= 1e-9 && hlPeerFlatOnCoin(symbol, posSide, posQty, onChain) {
		return hlSharedCloseFloorEscalate, remainderUSD
	}
	if heldReason != "" {
		return hlSharedCloseFloorHeld, remainderUSD
	}
	return hlSharedCloseFloorHold, remainderUSD
}

func formatSharedCloseStrandedAlert(strategyID, symbol string, remainderUSD float64, reason, holdReason string) string {
	recovery := "The scheduler holds this close and will not resend it until the value rises above the gate, every peer is flat on-chain and in its own book, or the operator closes it by hand."
	if holdReason == hlSharedCloseHoldVenueReject {
		recovery = "The scheduler holds this close and will not resend it until the value rises above the gate; a peer going flat does not resend it, and the bot's force-close sends the same sized order the venue rejects (issue 1534), so close the remainder directly on the venue or add to it above the gate."
	}
	return fmt.Sprintf("**CRITICAL — stranded remainder below venue minimum gate** [%s] %s: the final full close is worth $%.2f, under the $%.2f gate (the $%.2f venue minimum plus the %.0f%% safety margin). %s %s",
		strategyID, symbol, remainderUSD, hlVenueCloseGateThresholdUSD(), hlVenueMinOrderNotionalUSD, hlVenueMinOrderNotionalMargin*100, reason, recovery)
}

func notifySharedCloseStranded(notifier *MultiNotifier, sc StrategyConfig, symbol string, remainderUSD float64, reason, holdReason string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := formatSharedCloseStrandedAlert(sc.ID, symbol, remainderUSD, reason, holdReason)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func applySharedCoinFullCloseFloor(sc StrategyConfig, result *HyperliquidResult, posQty float64, posSide string, price float64, hlLiveAll []StrategyConfig, onChain hlOnChainCoinView, peerVirtualQty float64, heldReason string, refetch func() (hlOnChainCoinView, error), notifier *MultiNotifier, logger *StrategyLogger) (hlSharedCloseFloorOutcome, float64) {
	if result == nil || result.Signal == 0 {
		return hlSharedCloseFloorNone, 0
	}
	outcome, remainderUSD := evaluateSharedCoinFullCloseFloor(result.CloseFraction, result.Symbol, hlLiveAll, posQty, posSide, price, onChain, peerVirtualQty, heldReason)
	if outcome == hlSharedCloseFloorEscalate {
		if refetch == nil {
			outcome = hlSharedCloseFloorDefer
		} else if fresh, err := refetch(); err != nil || !fresh.Known {
			logger.Warn("Final full close %s: pre-escalation account refetch failed (%v) — deferring to the next cycle", result.Symbol, err)
			outcome = hlSharedCloseFloorDefer
		} else {
			outcome, remainderUSD = evaluateSharedCoinFullCloseFloor(result.CloseFraction, result.Symbol, hlLiveAll, posQty, posSide, price, fresh, peerVirtualQty, heldReason)
			if outcome != hlSharedCloseFloorEscalate {
				logger.Warn("Final full close %s: the refetched account state no longer shows every peer flat — outcome %s", result.Symbol, outcome)
			}
		}
	}
	switch outcome {
	case hlSharedCloseFloorEscalate:
		result.ForceFullClose = true
		logger.Info("Final full close %s worth $%.2f is below the venue minimum gate and every peer is flat on the refetched account state and in its own book — escalating to market_close(sz=None)", result.Symbol, remainderUSD)
	case hlSharedCloseFloorHold:
		result.Signal = 0
		result.CloseFraction = 0
		result.CloseGate = hlSharedCloseStrandedGate
		logger.Error("Final full close %s worth $%.2f is below the venue minimum gate while a peer holds quantity (peer virtual %.6f) — holding the close and alerting once; stop-loss management continues", result.Symbol, remainderUSD, peerVirtualQty)
		notifySharedCloseStranded(notifier, sc, result.Symbol, remainderUSD, "A peer strategy still holds quantity on this coin (on-chain or in its own book), so a whole-position close would take its exposure and a sized close is rejected by the venue.", hlSharedCloseHoldPeerBusy)
	case hlSharedCloseFloorHeld:
		result.Signal = 0
		result.CloseFraction = 0
		result.CloseGate = hlSharedCloseStrandedGate
		logger.Info("Final full close %s still stranded below the venue minimum ($%.2f, hold reason %s) — hold persists, no order sent", result.Symbol, remainderUSD, heldReason)
	case hlSharedCloseFloorDefer:
		result.Signal = 0
		result.CloseFraction = 0
		result.CloseGate = hlSharedCloseDeferredGate
		logger.Warn("Final full close %s worth $%.2f is below the venue minimum but on-chain positions are not known for this cycle — deferring to the next cycle", result.Symbol, remainderUSD)
	}
	return outcome, remainderUSD
}

func isHLMinOrderValueRejection(errStr string) bool {
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "minimum value") || strings.Contains(lower, "min value") || strings.Contains(lower, "minimum order value")
}

func stampSharedCloseHold(s *StrategyState, symbol string, remainderUSD float64, reason string) {
	if s == nil {
		return
	}
	if pos, ok := s.Positions[symbol]; ok && pos != nil {
		pos.SharedCloseHoldUSD = remainderUSD
		pos.SharedCloseHoldReason = reason
	}
}

func clearSharedCloseHold(s *StrategyState, symbol string) bool {
	if s == nil {
		return false
	}
	if pos, ok := s.Positions[symbol]; ok && pos != nil && (pos.SharedCloseHoldUSD != 0 || pos.SharedCloseHoldReason != "") {
		pos.SharedCloseHoldUSD = 0
		pos.SharedCloseHoldReason = ""
		return true
	}
	return false
}

func rearmProtectionAfterFailedClose(sc StrategyConfig, stratState *StrategyState, db *StateDB, symbol string, price float64, prevStopOID int64, prevTriggerPx, prevHighWater float64, onChainAbsQty map[string]float64, reconcileFillHintsJSON []byte, liqPxByCoin map[string]float64, netSideByCoin map[string]string, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	if stratState == nil || symbol == "" {
		return 0, ""
	}
	trades := 0
	detail := ""
	if _, fillPx := runHyperliquidProtectionSync(sc, stratState, db, symbol, mu, notifier, logger, "HL protection re-armed after failed close", reconcileFillHintsJSON, liqPxByCoin, netSideByCoin); fillPx > 0 {
		trades++
		detail = fmt.Sprintf("[%s] LIVE PROTECTION SYNC SL %s @ $%.2f", sc.ID, symbol, fillPx)
	}
	if extraTrades, slDetail := rearmTrailingStopAfterFailedClose(sc, stratState, symbol, price, prevStopOID, prevTriggerPx, prevHighWater, onChainAbsQty, liqPxByCoin, netSideByCoin, mu, notifier, logger); extraTrades > 0 {
		trades += extraTrades
		detail = slDetail
	}
	return trades, detail
}

func rearmTrailingStopAfterFailedClose(sc StrategyConfig, stratState *StrategyState, symbol string, mark float64, prevStopOID int64, prevTriggerPx, prevHighWater float64, onChainAbsQty map[string]float64, liqPxByCoin map[string]float64, netSideByCoin map[string]string, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	if !hyperliquidIsLive(sc.Args) || stratState == nil || symbol == "" || mark <= 0 {
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
	if highWater <= 0 {
		highWater = prevHighWater
	}
	triggerPx := pos.StopLossTriggerPx
	if triggerPx <= 0 {
		triggerPx = prevTriggerPx
	}
	cancelOID := pos.StopLossOID
	if cancelOID <= 0 {
		cancelOID = prevStopOID
	}
	posSnap := *pos
	mu.RUnlock()

	slEffectiveQty, capped := hlSLEffectiveQty(symbol, posSnap.Quantity, onChainAbsQty)
	if capped {
		logger.Warn("failed-close trailing SL re-arm: virtual qty %.6f > on-chain %.6f for %s; capping SL size to on-chain qty (#621)", posSnap.Quantity, slEffectiveQty, symbol)
	}
	logger.Warn("Failed close %s cancelled its on-chain stop (oid=%d); re-arming the trailing SL from high-water $%.4f with the old oid verified on-chain before any cancel", symbol, cancelOID, highWater)
	policy := trailingReplacePolicy{forceResize: true, liquidationPx: hlLiquidationPxForSide(liqPxByCoin, netSideByCoin, symbol, side)}
	newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(sc, symbol, side, slEffectiveQty, &posSnap, mark, highWater, triggerPx, cancelOID, policy, notifier, logger)
	mu.Lock()
	defer mu.Unlock()
	if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, cancelOID, newHighWater, updateConfirmed, slUpdate, "trailing_stop_loss_immediate", logger, 0); immediateFill {
		return 1, fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx)
	}
	if updateConfirmed && slUpdate != nil && slUpdate.StopLossOID > 0 {
		logger.Info("Trailing SL re-armed after failed close for %s (qty=%.6f high_water=$%.4f trigger=$%.4f)", symbol, slEffectiveQty, newHighWater, slUpdate.StopLossTriggerPx)
	}
	return 0, ""
}
