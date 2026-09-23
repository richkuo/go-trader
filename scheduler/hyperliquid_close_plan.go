package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

type hlCloseAction int

const (
	hlCloseSend hlCloseAction = iota
	hlCloseSkip
	hlCloseDefer
)

func (a hlCloseAction) String() string {
	switch a {
	case hlCloseSkip:
		return "skip"
	case hlCloseDefer:
		return "defer"
	default:
		return "send"
	}
}

type hlCloseOrderPlan struct {
	Action hlCloseAction
	Mode   hlCloseMode
	Size   float64
	Capped bool
	Reason string
}

type hlCloseContext struct {
	PeerSameQty float64
	PeerOppQty  float64
	OnChain     hlOnChainCoinView
	Refetch     func() (hlOnChainCoinView, error)
}

func hlSideSign(side string) float64 {
	if side == "short" {
		return -1
	}
	return 1
}

func hlPeerBooksOnCoin(strategies map[string]*StrategyState, hlLiveAll []StrategyConfig, coin, selfID, selfSide string) (sameQty, oppQty float64) {
	target := strings.ToUpper(strings.TrimSpace(coin))
	if target == "" {
		return 0, 0
	}
	selfSign := hlSideSign(selfSide)
	add := func(pos *Position) {
		if pos == nil || pos.Quantity <= 0 {
			return
		}
		if hlSideSign(pos.Side) == selfSign {
			sameQty += pos.Quantity
		} else {
			oppQty += pos.Quantity
		}
	}
	for _, sc := range hlLiveAll {
		if sc.ID == selfID {
			continue
		}
		ss := strategies[sc.ID]
		if ss == nil {
			continue
		}
		if raw := hyperliquidRawCoin(sc); raw != "" && strings.ToUpper(strings.TrimSpace(raw)) == target {
			add(hlVirtualPositionFor(ss, sc, raw))
		}
		if hCoin := hedgeCoin(sc); hCoin != "" && strings.ToUpper(strings.TrimSpace(hCoin)) == target {
			if hPos := ss.Positions[hCoin]; hPos.isHedgeLeg() {
				add(hPos)
			}
		}
	}
	return sameQty, oppQty
}

func hlOnChainSignedQty(view hlOnChainCoinView, symbol string) (float64, bool) {
	coin := strings.TrimSpace(symbol)
	key := coin
	abs, found := view.AbsQty[key]
	if !found {
		for k, v := range view.AbsQty {
			if strings.EqualFold(strings.TrimSpace(k), coin) {
				key, abs, found = k, v, true
				break
			}
		}
	}
	if !found || abs <= 1e-9 {
		return 0, true
	}
	if math.IsNaN(abs) || math.IsInf(abs, 0) {
		return 0, false
	}
	switch view.NetSide[key] {
	case "long":
		return abs, true
	case "short":
		return -abs, true
	default:
		return 0, false
	}
}

func finitePositive(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func planHLCloseOrder(symbol, posSide string, posQty, closeQty, peerSameQty, peerOppQty float64, onChain hlOnChainCoinView) hlCloseOrderPlan {
	tol := hlSharedCloseQtyTolerance
	if posSide != "long" && posSide != "short" {
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the %s position side %q is neither long nor short, so the close direction is unknown", symbol, posSide)}
	}
	if !finitePositive(posQty) || !finitePositive(closeQty) || closeQty > posQty+tol {
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the %s close quantity %.6f is not a valid part of the book quantity %.6f", symbol, closeQty, posQty)}
	}
	if closeQty > posQty {
		closeQty = posQty
	}
	if peerSameQty < 0 || peerOppQty < 0 || math.IsNaN(peerSameQty) || math.IsNaN(peerOppQty) || math.IsInf(peerSameQty, 0) || math.IsInf(peerOppQty, 0) {
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the peer book quantities on %s are not usable", symbol)}
	}
	if !onChain.Known {
		if peerOppQty > tol {
			return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the on-chain %s position is unknown and an opposite-side peer holds %.6f in its book, so neither a reduce-only nor a netted close can be sized safely", symbol, peerOppQty)}
		}
		return hlCloseOrderPlan{Action: hlCloseSend, Mode: hlCloseModeReduceOnly, Size: closeQty, Reason: fmt.Sprintf("the on-chain %s position is unknown and no opposite-side peer holds a book, so the close is sent reduce-only at the book size", symbol)}
	}
	netSigned, ok := hlOnChainSignedQty(onChain, symbol)
	if !ok {
		return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the on-chain %s position has no readable side", symbol)}
	}
	s := hlSideSign(posSide)
	target := s*(posQty-closeQty) + s*peerSameQty - s*peerOppQty
	requested := s * (netSigned - target)
	if requested <= tol {
		return hlCloseOrderPlan{Action: hlCloseSkip, Reason: fmt.Sprintf("the on-chain %s net %.6f is already at or past the %.6f the books state after this close (%s book %.6f, close %.6f, same-side peers %.6f, opposite-side peers %.6f); no close order sent, the reconciler owns the book", symbol, netSigned, target, posSide, posQty, closeQty, peerSameQty, peerOppQty)}
	}
	plan := hlCloseOrderPlan{Action: hlCloseSend, Size: closeQty}
	if requested < closeQty-tol {
		plan.Size = requested
		plan.Capped = true
	}
	if s*netSigned > tol && s*target >= -tol {
		plan.Mode = hlCloseModeReduceOnly
		plan.Reason = fmt.Sprintf("the %s close keeps the on-chain net on the %s side (net %.6f, books after close %.6f)", symbol, posSide, netSigned, target)
	} else {
		plan.Mode = hlCloseModeCross
		plan.Reason = fmt.Sprintf("the %s close crosses zero or starts from the other side (net %.6f, books after close %.6f), so it is sent as the netted order", symbol, netSigned, target)
	}
	return plan
}

func resolveHLCloseOrder(symbol, posSide string, posQty, closeQty float64, ctx hlCloseContext) hlCloseOrderPlan {
	snapshot := planHLCloseOrder(symbol, posSide, posQty, closeQty, ctx.PeerSameQty, ctx.PeerOppQty, ctx.OnChain)
	detail := "no account refetch is available"
	if ctx.Refetch != nil {
		fresh, err := ctx.Refetch()
		if err == nil && fresh.Known {
			return planHLCloseOrder(symbol, posSide, posQty, closeQty, ctx.PeerSameQty, ctx.PeerOppQty, fresh)
		}
		detail = "the account state is not readable"
		if err != nil {
			detail = err.Error()
		}
	}
	if snapshot.Action == hlCloseSend && snapshot.Mode == hlCloseModeReduceOnly && !snapshot.Capped {
		snapshot.Reason = fmt.Sprintf("%s; the pre-send account refetch failed (%s), and a reduce-only close at the book size cannot cross zero", snapshot.Reason, detail)
		return snapshot
	}
	return hlCloseOrderPlan{Action: hlCloseDefer, Reason: fmt.Sprintf("the %s close would %s on the cycle-start reading, but the pre-send account refetch failed (%s), so no order is sent and no protection is cancelled", symbol, hlSnapshotPlanVerb(snapshot), detail)}
}

func hlSnapshotPlanVerb(plan hlCloseOrderPlan) string {
	switch {
	case plan.Action == hlCloseSkip:
		return "be skipped"
	case plan.Action == hlCloseDefer:
		return "be deferred"
	case plan.Mode == hlCloseModeCross:
		return "cross zero"
	default:
		return "be capped"
	}
}

func hlOnChainRefetcher(accountAddress string) func() (hlOnChainCoinView, error) {
	return func() (hlOnChainCoinView, error) {
		_, fresh, err := fetchHyperliquidStateFn(accountAddress)
		if err != nil {
			return hlOnChainCoinView{}, err
		}
		return hlOnChainCoinViewFromPositions(fresh), nil
	}
}

func manualCloseFillAttribution(posQty float64, fill *HyperliquidFill) (bookedQty, fee float64, fullClose bool) {
	if fill == nil {
		return 0, 0, false
	}
	bookedQty = fill.TotalSz
	fee = fill.Fee
	if bookedQty > posQty+1e-9 {
		if fill.TotalSz > 0 {
			fee *= posQty / fill.TotalSz
		}
		bookedQty = posQty
	}
	fullClose = posQty-bookedQty <= 0.0001
	return bookedQty, fee, fullClose
}

func notifySizedCloseRemainder(notifier *MultiNotifier, sc StrategyConfig, symbol, side string, filledQty, bookQty float64) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := fmt.Sprintf("**SIZED CLOSE FILLED SHORT** [%s] %s %s close filled %.6f of the %.6f book. The %.6f remainder stays on the book and the protection ids this close cancelled are cleared. In this same cycle the close re-arm places the stop again, sized at the remainder: the protection sync for an ATR stop, the trailing arm for a trail or ratchet stop, and the percentage or recorded-stop arm for any other stop. Compare the on-chain position and open orders with the books.", sc.ID, symbol, side, filledQty, bookQty, bookQty-filledQty)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

type manualCycleCloseBooking struct {
	Action        PendingManualAction
	ClearOIDs     []int64
	ShortOfIntent bool
}

func bookManualCycleClose(sc StrategyConfig, pos *Position, closeSide string, closeQty float64, intentFullClose bool, execResult *HyperliquidExecuteResult, requestedCancelOIDs []int64, now time.Time) manualCycleCloseBooking {
	fill := execResult.Execution.Fill
	bookedQty, bookedFee, bookedFull := manualCloseFillAttribution(pos.Quantity, fill)
	var realizedPnL float64
	if pos.Side == "long" {
		realizedPnL = bookedQty * (fill.AvgPx - pos.AvgCost)
	} else {
		realizedPnL = bookedQty * (pos.AvgCost - fill.AvgPx)
	}
	realizedPnL -= bookedFee
	var oid string
	if fill.OID != 0 {
		oid = fmt.Sprintf("%d", fill.OID)
	}
	fullClose := intentFullClose && bookedFull
	booking := manualCycleCloseBooking{
		Action: PendingManualAction{
			StrategyID:      sc.ID,
			Action:          "close",
			Symbol:          sc.Symbol,
			Side:            closeSide,
			Quantity:        bookedQty,
			FillPrice:       fill.AvgPx,
			FillFee:         bookedFee,
			ExchangeOrderID: oid,
			RealizedPnL:     realizedPnL,
			IsFullClose:     fullClose,
			CreatedAt:       now,
		},
		ShortOfIntent: intentFullClose && !bookedFull,
	}
	if !fullClose {
		booking.ClearOIDs = hyperliquidExecuteSucceededCancelOIDs(execResult, requestedCancelOIDs)
	}
	return booking
}

type hlCloseRearmContext struct {
	Price         float64
	PrevStopOID   int64
	PrevTriggerPx float64
	PrevHighWater float64
	OnChainAbsQty map[string]float64
	FillHintsJSON []byte
	LiqPxByCoin   map[string]float64
	NetSideByCoin map[string]string
}

func settleManualCycleClose(sc StrategyConfig, stratState *StrategyState, stratDB *StateDB, pos *Position, closeSide string, closeQty float64, intentFullClose bool, execResult *HyperliquidExecuteResult, execErr error, requestedCancelOIDs []int64, rearm hlCloseRearmContext, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string, float64) {
	cancelRequested := false
	for _, oid := range requestedCancelOIDs {
		if oid > 0 {
			cancelRequested = true
		}
	}
	execResult, execErr = confirmHyperliquidExecuteFill(execResult, execErr)
	if execErr != nil {
		logger.Error("manual close execute failed: %v", execErr)
		canceledOIDs := hyperliquidExecuteSucceededCancelOIDs(execResult, requestedCancelOIDs)
		if len(canceledOIDs) > 0 {
			mu.Lock()
			clearHyperliquidProtectionOIDsMatching(stratState.Positions[sc.Symbol], canceledOIDs)
			mu.Unlock()
		}
		if intentFullClose && cancelRequested && (execResult == nil || len(canceledOIDs) > 0) {
			return rearmManualCycleCloseStop(sc, stratState, stratDB, 0, rearm, mu, notifier, logger)
		}
		return 0, "", 0
	}
	if execResult.CancelStopLossError != "" {
		logger.Warn("manual close cancel failed (non-fatal) for %s/%s: %s (requested oids=%v) — verify HL on-chain triggers",
			sc.ID, sc.Symbol, execResult.CancelStopLossError, requestedCancelOIDs)
	}
	if execResult.Execution == nil || execResult.Execution.Fill == nil {
		return 0, "", 0
	}
	booking := bookManualCycleClose(sc, pos, closeSide, closeQty, intentFullClose, execResult, requestedCancelOIDs, time.Now().UTC())
	action := booking.Action
	if action.Quantity < closeQty-1e-9 {
		logger.Warn("manual close filled %.6f of the requested %.6f for %s/%s; booking the filled quantity", action.Quantity, closeQty, sc.ID, sc.Symbol)
	}
	if len(booking.ClearOIDs) > 0 {
		mu.Lock()
		clearHyperliquidProtectionOIDsMatching(stratState.Positions[sc.Symbol], booking.ClearOIDs)
		mu.Unlock()
		logger.Info("cleared canceled protection OIDs=%v after the manual close filled short of the full book", booking.ClearOIDs)
	}
	if booking.ShortOfIntent {
		logger.Error("CRITICAL: manual full close %s filled %.6f of the %.6f book; the remainder stays on the book", sc.Symbol, action.Quantity, pos.Quantity)
		notifySizedCloseRemainder(notifier, sc, sc.Symbol, pos.Side, action.Quantity, pos.Quantity)
	}
	trades, detail, fillPx := 0, "", 0.0
	if err := stratDB.InsertPendingManualAction(action); err != nil {
		logger.Error("failed to queue manual close action: %v", err)
	} else {
		trades = 1
		fillPx = action.FillPrice
		detail = fmt.Sprintf("manual close %.4f %s @ $%.2f | PnL=$%.2f", action.Quantity, sc.Symbol, action.FillPrice, action.RealizedPnL)
		logger.Info("Queued manual close: %s", detail)
	}
	if booking.ShortOfIntent && cancelRequested {
		mu.RLock()
		remainder := pos.Quantity - action.Quantity
		mu.RUnlock()
		if extraTrades, slDetail, _ := rearmManualCycleCloseStop(sc, stratState, stratDB, remainder, rearm, mu, notifier, logger); extraTrades > 0 {
			trades += extraTrades
			detail = slDetail
		}
	}
	return trades, detail, fillPx
}

func rearmManualCycleCloseStop(sc StrategyConfig, stratState *StrategyState, stratDB *StateDB, remainderQty float64, rearm hlCloseRearmContext, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string, float64) {
	if !hyperliquidIsLive(sc.Args) {
		return 0, "", 0
	}
	if remainderQty > 0 {
		logger.Warn("Manual close %s left %.6f on the book after cancelling its protection; re-arming the remainder's stop in this cycle", sc.Symbol, remainderQty)
	}
	trades, detail := rearmProtectionForCloseRemainder(sc, stratState, stratDB, sc.Symbol, rearm.Price, rearm.PrevStopOID, rearm.PrevTriggerPx, rearm.PrevHighWater, rearm.OnChainAbsQty, rearm.FillHintsJSON, rearm.LiqPxByCoin, rearm.NetSideByCoin, remainderQty, mu, notifier, logger)
	return trades, detail, 0
}
