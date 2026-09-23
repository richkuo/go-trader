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
	Action        hlCloseAction
	Mode          hlCloseMode
	Size          float64
	Capped        bool
	Reason        string
	OwnShare      float64
	OwnShareKnown bool
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
	ownShare := s*netSigned - peerSameQty + peerOppQty
	target := s*(posQty-closeQty) + s*peerSameQty - s*peerOppQty
	requested := s * (netSigned - target)
	if requested <= tol {
		return hlCloseOrderPlan{Action: hlCloseSkip, OwnShare: ownShare, OwnShareKnown: true, Reason: fmt.Sprintf("the on-chain %s net %.6f is already at or past the %.6f the books state after this close (%s book %.6f, close %.6f, same-side peers %.6f, opposite-side peers %.6f); no close order sent, the reconciler owns the book", symbol, netSigned, target, posSide, posQty, closeQty, peerSameQty, peerOppQty)}
	}
	plan := hlCloseOrderPlan{Action: hlCloseSend, Size: closeQty, OwnShare: ownShare, OwnShareKnown: true}
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
		snapshot.OwnShare, snapshot.OwnShareKnown = 0, false
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

type hlCloseRemainderBasis int

const (
	hlRemainderBasisBook hlCloseRemainderBasis = iota
	hlRemainderBasisPlan
	hlRemainderBasisRead
	hlRemainderBasisUnverified
)

type hlCloseRemainderStop struct {
	Remainder float64
	Qty       float64
	Basis     hlCloseRemainderBasis
	AfterFill bool
	Detail    string
}

func (s hlCloseRemainderStop) measured() bool {
	return s.Basis == hlRemainderBasisPlan || s.Basis == hlRemainderBasisRead
}

func (s hlCloseRemainderStop) unbacked() bool {
	return s.measured() && s.Qty <= 0
}

type hlCloseBacking struct {
	PlanShare      float64
	PlanShareKnown bool
	PeerSameQty    float64
	PeerOppQty     float64
	Refetch        func() (hlOnChainCoinView, error)
}

func resolveHLCloseRemainderStop(symbol, side string, bookQty, filledQty float64, outcomeKnown bool, b hlCloseBacking) hlCloseRemainderStop {
	tol := hlSharedCloseQtyTolerance
	remainder := math.Max(bookQty-filledQty, 0)
	stop := hlCloseRemainderStop{Remainder: remainder, Qty: remainder, AfterFill: filledQty > tol}
	measured := func(backed float64, basis hlCloseRemainderBasis) hlCloseRemainderStop {
		stop.Basis = basis
		switch {
		case backed >= remainder-tol:
			stop.Qty = remainder
		case backed <= tol:
			stop.Qty = 0
		default:
			stop.Qty = backed
		}
		return stop
	}
	if outcomeKnown && b.PlanShareKnown {
		return measured(b.PlanShare-filledQty, hlRemainderBasisPlan)
	}
	if outcomeKnown && !stop.AfterFill {
		stop.Basis = hlRemainderBasisBook
		return stop
	}
	stop.Basis = hlRemainderBasisUnverified
	if b.Refetch == nil {
		stop.Detail = "no account reader is available"
		return stop
	}
	view, err := b.Refetch()
	if err != nil {
		stop.Detail = err.Error()
		return stop
	}
	if !view.Known {
		stop.Detail = "the account state is not readable"
		return stop
	}
	signed, ok := hlOnChainSignedQty(view, symbol)
	if !ok {
		stop.Detail = fmt.Sprintf("the on-chain %s position has no readable side", symbol)
		return stop
	}
	return measured(hlSideSign(side)*signed-b.PeerSameQty+b.PeerOppQty, hlRemainderBasisRead)
}

func closeRemainderBasisText(stop hlCloseRemainderStop) string {
	switch stop.Basis {
	case hlRemainderBasisPlan:
		return "the pre-send account reading less the fill"
	case hlRemainderBasisRead:
		return "a post-close account reading"
	}
	return ""
}

func closeRearmNextAction(sc StrategyConfig, res hlStopRearmResult) string {
	if res.Owner == hlRearmOwnerRecorded {
		trigger := "<price>"
		if res.TriggerPx > 0 {
			trigger = fmt.Sprintf("%.4f", res.TriggerPx)
		}
		return fmt.Sprintf("Check the open orders on Hyperliquid, then re-arm with `go-trader manual-update-sl %s --trigger %s` or close the position.", sc.ID, trigger)
	}
	return "Check the open orders on Hyperliquid, then place the stop by hand or close the position. Do not rely on a later cycle to re-arm it."
}

func closeRearmFailureText(res hlStopRearmResult, stop hlCloseRemainderStop, prevStopOID int64) (string, string) {
	switch res.Status {
	case hlStopRearmNoTrigger:
		return "no trigger price could be resolved", "The position has NO exchange-side stop."
	case hlStopRearmNoArm:
		return fmt.Sprintf("no re-arm path owns the cancelled stop (OID=%d)", prevStopOID), "The position has NO exchange-side stop."
	case hlStopRearmReadFailed:
		return fmt.Sprintf("the open orders could not be read (%s)", res.Detail), "Nothing was placed, and the stop state is UNVERIFIED."
	case hlStopRearmPreCloseStopResting:
		reason := fmt.Sprintf("the cancel of the pre-close stop (OID=%d) was rejected (%s)", prevStopOID, res.Detail)
		if stop.AfterFill || stop.Qty < stop.Remainder-hlSharedCloseQtyTolerance {
			return reason, fmt.Sprintf("That stop still rests at its pre-close size, which is larger than the %.6f this position now needs, so on a shared coin it can close units of a peer strategy when it fires.", res.Qty)
		}
		return reason, "That stop most likely still rests and protects the position, but its state is not verified."
	case hlStopRearmProtectionLost:
		return fmt.Sprintf("the pre-close stop was cancelled and the replacement did not rest (%s)", res.Detail), "The position has NO exchange-side stop."
	case hlStopRearmOutcomeUnknown:
		return fmt.Sprintf("the placement outcome could not be read (%s)", res.Detail), "A stop may rest untracked, so the position may be UNPROTECTED."
	}
	return fmt.Sprintf("the placement was rejected (%s)", res.Detail), "The position may have NO exchange-side stop."
}

func formatCloseRearmReport(sc StrategyConfig, symbol string, stop hlCloseRemainderStop, res hlStopRearmResult, prevStopOID int64) (string, bool) {
	basis := closeRemainderBasisText(stop)
	unverified := ""
	if stop.Basis == hlRemainderBasisUnverified {
		unverified = fmt.Sprintf(" The account read after the close failed (%s), so the stop is sized at the %.6f book remainder with no check of which on-chain units back it.", stop.Detail, stop.Remainder)
	}
	switch res.Status {
	case hlStopRearmUnbacked:
		return fmt.Sprintf("No stop was placed: %s shows no on-chain units behind the %.6f remainder (the coin is flat on this side, or every unit left is in the book of a peer strategy). Compare the on-chain position with the books before the next close on %s.", basis, stop.Remainder, symbol), true
	case hlStopRearmUnclaimed:
		return fmt.Sprintf("This position has no configured stop owner, so no stop was placed for the %.6f remainder.%s", stop.Remainder, unverified), false
	case hlStopRearmClosed:
		return fmt.Sprintf("The %s re-arm found that %s; the reconciler books that close.", res.Owner, res.Detail), false
	case hlStopRearmPlaced:
		if stop.measured() && res.Qty < stop.Remainder-hlSharedCloseQtyTolerance {
			return fmt.Sprintf("The %s now rests for %.6f at $%.4f (OID=%d), the on-chain units that back the remainder per %s. The other %.6f units of the %.6f remainder have no on-chain units behind them and get no stop. Compare the on-chain position with the books before the next close on %s.", res.Owner, res.Qty, res.TriggerPx, res.OID, basis, stop.Remainder-res.Qty, stop.Remainder, symbol), true
		}
		sized := ""
		if stop.measured() {
			sized = ", sized from " + basis
		}
		return fmt.Sprintf("The %s now rests for %.6f at $%.4f (OID=%d)%s.%s", res.Owner, res.Qty, res.TriggerPx, res.OID, sized, unverified), false
	}
	reason, state := closeRearmFailureText(res, stop, prevStopOID)
	return fmt.Sprintf("The %s re-arm for %.6f did not complete: %s. %s%s %s", res.Owner, res.Qty, reason, state, unverified, closeRearmNextAction(sc, res)), true
}

func formatSizedCloseShortFillAlert(sc StrategyConfig, symbol, side string, filledQty, bookQty float64, report string, critical bool) string {
	prefix := ""
	if critical {
		prefix = "CRITICAL: "
	}
	return fmt.Sprintf("%s**SIZED CLOSE FILLED SHORT** [%s] %s %s close filled %.6f of the %.6f book. The %.6f remainder stays on the book, and the protection ids this close cancelled are cleared. Stop re-arm: %s", prefix, sc.ID, symbol, side, filledQty, bookQty, bookQty-filledQty, report)
}

func formatUnfilledCloseRearmAlert(sc StrategyConfig, symbol, side string, bookQty float64, report string) string {
	return fmt.Sprintf("CRITICAL: [%s] %s %s close of the %.6f book did not fill after it cancelled, or may have cancelled, the exchange-side protection. Stop re-arm: %s", sc.ID, symbol, side, bookQty, report)
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
	Backing       hlCloseBacking
}

func rearmCloseRemainderStop(sc StrategyConfig, stratState *StrategyState, stratDB *StateDB, symbol string, stop hlCloseRemainderStop, rearm hlCloseRearmContext, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string, hlStopRearmResult) {
	if !hyperliquidIsLive(sc.Args) {
		return 0, "", hlStopRearmResult{}
	}
	if stop.unbacked() {
		logger.Error("CRITICAL: close %s left %.6f on the book, but %s shows no on-chain units behind it; no stop is placed", symbol, stop.Remainder, closeRemainderBasisText(stop))
	} else {
		logger.Warn("Close %s left %.6f on the book after cancelling (or possibly cancelling) its protection; re-arming a %.6f stop in this cycle", symbol, stop.Remainder, stop.Qty)
	}
	return rearmProtectionForCloseRemainder(sc, stratState, stratDB, symbol, rearm.Price, rearm.PrevStopOID, rearm.PrevTriggerPx, rearm.PrevHighWater, rearm.OnChainAbsQty, rearm.FillHintsJSON, rearm.LiqPxByCoin, rearm.NetSideByCoin, stop, mu, notifier, logger)
}

func rearmShortFilledClose(sc StrategyConfig, stratState *StrategyState, stratDB *StateDB, symbol, side string, bookQty, filledQty float64, rearm hlCloseRearmContext, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	stop := resolveHLCloseRemainderStop(symbol, side, bookQty, filledQty, true, rearm.Backing)
	trades, detail, res := rearmCloseRemainderStop(sc, stratState, stratDB, symbol, stop, rearm, mu, notifier, logger)
	report, critical := formatCloseRearmReport(sc, symbol, stop, res, rearm.PrevStopOID)
	msg := formatSizedCloseShortFillAlert(sc, symbol, side, filledQty, bookQty, report, critical)
	if critical {
		logger.Error("%s", msg)
	} else {
		logger.Warn("%s", msg)
	}
	notifyManualCloseRearmFailure(notifier, msg)
	return trades, detail
}

func rearmUnfilledClose(sc StrategyConfig, stratState *StrategyState, stratDB *StateDB, symbol, side string, bookQty float64, outcomeKnown bool, rearm hlCloseRearmContext, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	stop := resolveHLCloseRemainderStop(symbol, side, bookQty, 0, outcomeKnown, rearm.Backing)
	trades, detail, res := rearmCloseRemainderStop(sc, stratState, stratDB, symbol, stop, rearm, mu, notifier, logger)
	if report, critical := formatCloseRearmReport(sc, symbol, stop, res, rearm.PrevStopOID); critical {
		msg := formatUnfilledCloseRearmAlert(sc, symbol, side, bookQty, report)
		logger.Error("%s", msg)
		notifyManualCloseRearmFailure(notifier, msg)
	}
	return trades, detail
}

func rearmExecuteLaneSizedCloseShortFill(sc StrategyConfig, stratState *StrategyState, stratDB *StateDB, result *HyperliquidResult, posQty float64, posSide string, rearm hlCloseRearmContext, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	rearm.Backing.PlanShare, rearm.Backing.PlanShareKnown = result.SizedClosePlanShare, result.SizedClosePlanShareKnown
	return rearmShortFilledClose(sc, stratState, stratDB, result.Symbol, posSide, posQty, result.SizedCloseBookedQty, rearm, mu, notifier, logger)
}

func rearmExecuteLaneUnfilledClose(sc StrategyConfig, stratState *StrategyState, stratDB *StateDB, result *HyperliquidResult, execResult *HyperliquidExecuteResult, posQty float64, posSide string, rearm hlCloseRearmContext, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	rearm.Backing.PlanShare, rearm.Backing.PlanShareKnown = result.SizedClosePlanShare, result.SizedClosePlanShareKnown
	return rearmUnfilledClose(sc, stratState, stratDB, result.Symbol, posSide, posQty, execResult != nil, rearm, mu, notifier, logger)
}

func settleManualCycleClose(sc StrategyConfig, stratState *StrategyState, stratDB *StateDB, pos *Position, closeSide string, closeQty float64, intentFullClose bool, execResult *HyperliquidExecuteResult, execErr error, requestedCancelOIDs []int64, rearm hlCloseRearmContext, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string, float64) {
	cancelRequested := false
	for _, oid := range requestedCancelOIDs {
		if oid > 0 {
			cancelRequested = true
		}
	}
	mu.RLock()
	bookQty := pos.Quantity
	side := pos.Side
	mu.RUnlock()
	execResult, execErr = confirmHyperliquidExecuteFill(execResult, execErr)
	if execErr != nil {
		logger.Error("manual close execute failed: %v", execErr)
		canceledOIDs := hyperliquidExecuteSucceededCancelOIDs(execResult, requestedCancelOIDs)
		if len(canceledOIDs) > 0 {
			mu.Lock()
			clearHyperliquidProtectionOIDsMatching(stratState.Positions[sc.Symbol], canceledOIDs)
			mu.Unlock()
		}
		if intentFullClose && cancelRequested && (execResult == nil || len(canceledOIDs) > 0) && hyperliquidIsLive(sc.Args) {
			trades, detail := rearmUnfilledClose(sc, stratState, stratDB, sc.Symbol, side, bookQty, execResult != nil, rearm, mu, notifier, logger)
			return trades, detail, 0
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
	trades, detail, fillPx := 0, "", 0.0
	if err := stratDB.InsertPendingManualAction(action); err != nil {
		logger.Error("failed to queue manual close action: %v", err)
	} else {
		trades = 1
		fillPx = action.FillPrice
		detail = fmt.Sprintf("manual close %.4f %s @ $%.2f | PnL=$%.2f", action.Quantity, sc.Symbol, action.FillPrice, action.RealizedPnL)
		logger.Info("Queued manual close: %s", detail)
	}
	if booking.ShortOfIntent && hyperliquidIsLive(sc.Args) {
		if extraTrades, slDetail := rearmShortFilledClose(sc, stratState, stratDB, sc.Symbol, side, bookQty, action.Quantity, rearm, mu, notifier, logger); extraTrades > 0 {
			trades += extraTrades
			detail = slDetail
		}
	}
	return trades, detail, fillPx
}
