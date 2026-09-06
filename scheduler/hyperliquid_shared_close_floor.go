package main

import (
	"fmt"
	"math"
	"strings"
)

const (
	hlVenueMinOrderNotionalUSD    = 10.0
	hlVenueMinOrderNotionalMargin = 0.03
	hlSharedCloseStrandedGate     = "shared_coin_stranded_remainder"
	hlSharedCloseDeferredGate     = "shared_coin_on_chain_unknown"
	hlSharedCloseQtyTolerance     = 1e-6
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

func hlVenueCloseGateThresholdUSD() float64 {
	return hlVenueMinOrderNotionalUSD * (1.0 + hlVenueMinOrderNotionalMargin)
}

func hlPeerFlatOnCoin(symbol, posSide string, posQty float64, onChain hlOnChainCoinView) bool {
	coin := strings.ToUpper(strings.TrimSpace(symbol))
	onChainQty := onChain.AbsQty[coin]
	if onChainQty <= 1e-9 {
		return true
	}
	if side, ok := onChain.NetSide[coin]; ok && side != posSide {
		return false
	}
	return onChainQty <= posQty+hlSharedCloseQtyTolerance
}

func evaluateSharedCoinFullCloseFloor(closeFraction float64, symbol string, hlLiveAll []StrategyConfig, posQty float64, posSide string, price float64, onChain hlOnChainCoinView, alreadyHeld bool) (hlSharedCloseFloorOutcome, float64) {
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
	if !onChain.Known {
		return hlSharedCloseFloorDefer, remainderUSD
	}
	if hlPeerFlatOnCoin(symbol, posSide, posQty, onChain) {
		return hlSharedCloseFloorEscalate, remainderUSD
	}
	if alreadyHeld {
		return hlSharedCloseFloorHeld, remainderUSD
	}
	return hlSharedCloseFloorHold, remainderUSD
}

func formatSharedCloseStrandedAlert(strategyID, symbol string, remainderUSD float64, reason string) string {
	return fmt.Sprintf("**CRITICAL — stranded remainder below venue minimum** [%s] %s: the final full close is worth $%.2f, under the $%.2f venue minimum (gate $%.2f). %s The scheduler holds this close and will not resend it until the value rises above the minimum, the peer goes flat, or the operator closes it by hand.",
		strategyID, symbol, remainderUSD, hlVenueMinOrderNotionalUSD, hlVenueCloseGateThresholdUSD(), reason)
}

func notifySharedCloseStranded(notifier *MultiNotifier, sc StrategyConfig, symbol string, remainderUSD float64, reason string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := formatSharedCloseStrandedAlert(sc.ID, symbol, remainderUSD, reason)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func applySharedCoinFullCloseFloor(sc StrategyConfig, result *HyperliquidResult, posQty float64, posSide string, price float64, hlLiveAll []StrategyConfig, onChain hlOnChainCoinView, alreadyHeld bool, notifier *MultiNotifier, logger *StrategyLogger) (hlSharedCloseFloorOutcome, float64) {
	if result == nil {
		return hlSharedCloseFloorNone, 0
	}
	outcome, remainderUSD := evaluateSharedCoinFullCloseFloor(result.CloseFraction, result.Symbol, hlLiveAll, posQty, posSide, price, onChain, alreadyHeld)
	switch outcome {
	case hlSharedCloseFloorEscalate:
		result.ForceFullClose = true
		logger.Info("Final full close %s worth $%.2f is below the venue minimum and every peer is flat on-chain — escalating to market_close(sz=None)", result.Symbol, remainderUSD)
	case hlSharedCloseFloorHold:
		result.Signal = 0
		result.CloseFraction = 0
		result.CloseGate = hlSharedCloseStrandedGate
		logger.Error("Final full close %s worth $%.2f is below the venue minimum while a peer holds on-chain quantity — holding the close and alerting once", result.Symbol, remainderUSD)
		notifySharedCloseStranded(notifier, sc, result.Symbol, remainderUSD, "A peer strategy still holds on-chain quantity on this coin, so a whole-position close would take its exposure and a sized close is rejected by the venue.")
	case hlSharedCloseFloorHeld:
		result.Signal = 0
		result.CloseFraction = 0
		result.CloseGate = hlSharedCloseStrandedGate
		logger.Info("Final full close %s still stranded below the venue minimum ($%.2f) — hold persists, no order sent", result.Symbol, remainderUSD)
	case hlSharedCloseFloorDefer:
		result.Signal = 0
		result.CloseFraction = 0
		result.CloseGate = hlSharedCloseDeferredGate
		logger.Warn("Final full close %s worth $%.2f is below the venue minimum but on-chain positions were not fetched this cycle — deferring to the next cycle", result.Symbol, remainderUSD)
	}
	return outcome, remainderUSD
}

func isHLMinOrderValueRejection(errStr string) bool {
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "minimum value") || strings.Contains(lower, "min value") || strings.Contains(lower, "minimum order value")
}

func stampSharedCloseHold(s *StrategyState, symbol string, remainderUSD float64) {
	if s == nil {
		return
	}
	if pos, ok := s.Positions[symbol]; ok && pos != nil {
		pos.SharedCloseHoldUSD = remainderUSD
	}
}

func clearSharedCloseHold(s *StrategyState, symbol string) bool {
	if s == nil {
		return false
	}
	if pos, ok := s.Positions[symbol]; ok && pos != nil && pos.SharedCloseHoldUSD != 0 {
		pos.SharedCloseHoldUSD = 0
		return true
	}
	return false
}
