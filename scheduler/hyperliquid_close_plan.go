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
	Action  hlCloseAction
	Mode    hlCloseMode
	Size    float64
	Capped  bool
	Reason  string
	PreSend hlCloseView
}

type hlCloseView struct {
	Signed float64
	Known  bool
	Source string
}

const (
	hlCloseViewPreSendRefetch = "pre-send refetch"
	hlCloseViewCycleStart     = "cycle-start"
)

func hlCloseViewOf(view hlOnChainCoinView, symbol, source string) hlCloseView {
	if !view.Known {
		return hlCloseView{}
	}
	signed, ok := hlOnChainSignedQty(view, symbol)
	if !ok {
		return hlCloseView{}
	}
	return hlCloseView{Signed: signed, Known: true, Source: source}
}

func (v hlCloseView) from(source string) hlCloseView {
	if v.Known {
		v.Source = source
	}
	return v
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
	same, oppQty := hlPeerBookListOnCoin(strategies, hlLiveAll, coin, selfID, selfSide)
	return hlShareBookSum(same), oppQty
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
	preSend := hlCloseView{Signed: netSigned, Known: true}
	target := s*(posQty-closeQty) + s*peerSameQty - s*peerOppQty
	requested := s * (netSigned - target)
	if requested <= tol {
		return hlCloseOrderPlan{Action: hlCloseSkip, PreSend: preSend, Reason: fmt.Sprintf("the on-chain %s net %.6f is already at or past the %.6f the books state after this close (%s book %.6f, close %.6f, same-side peers %.6f, opposite-side peers %.6f); no close order sent, the reconciler owns the book", symbol, netSigned, target, posSide, posQty, closeQty, peerSameQty, peerOppQty)}
	}
	plan := hlCloseOrderPlan{Action: hlCloseSend, Size: closeQty, PreSend: preSend}
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
	snapshot.PreSend = snapshot.PreSend.from(hlCloseViewCycleStart)
	detail := "no account refetch is available"
	if ctx.Refetch != nil {
		fresh, err := ctx.Refetch()
		if err == nil && fresh.Known {
			plan := planHLCloseOrder(symbol, posSide, posQty, closeQty, ctx.PeerSameQty, ctx.PeerOppQty, fresh)
			plan.PreSend = plan.PreSend.from(hlCloseViewPreSendRefetch)
			return plan
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
	hlRemainderBasisFresh hlCloseRemainderBasis = iota
	hlRemainderBasisDerived
	hlRemainderBasisStale
	hlRemainderBasisNone
)

type hlCloseRemainderStop struct {
	Remainder float64
	Qty       float64
	Basis     hlCloseRemainderBasis
	Unbacked  bool
	AfterFill bool
	Detail    string
}

type hlCloseBacking struct {
	PeerSameQty float64
	PeerOppQty  float64
	PeerSame    []hlShareBook
	PreSend     hlCloseView
	Refetch     func() (hlOnChainCoinView, error)
}

func resolveHLCloseRemainderStop(symbol, side string, bookQty float64, fill hlCloseFillOutcome, b hlCloseBacking) hlCloseRemainderStop {
	tol := hlSharedCloseQtyTolerance
	filled := 0.0
	if fill.Known && fill.Filled > 0 {
		filled = math.Min(fill.Filled, math.Max(bookQty, 0))
	}
	remainder := math.Max(bookQty-filled, 0)
	stop := hlCloseRemainderStop{Remainder: remainder, Qty: remainder, Basis: hlRemainderBasisNone, AfterFill: filled > tol}
	if remainder <= tol {
		stop.Qty = 0
		return stop
	}
	s := hlSideSign(side)
	sized := func(signed float64, basis hlCloseRemainderBasis) hlCloseRemainderStop {
		peers := b.PeerSame
		if len(peers) == 0 && b.PeerSameQty > tol {
			peers = []hlShareBook{{Qty: b.PeerSameQty, Armed: true}}
		}
		res := hlOwnStopShare(hlShareInput{
			Side:   side,
			Self:   hlShareBook{Qty: remainder, Armed: false},
			Same:   peers,
			Opp:    b.PeerOppQty,
			Signed: signed,
			Known:  true,
		})
		stop.Basis = basis
		stop.Qty = res.Qty
		stop.Unbacked = res.Qty <= tol
		return stop
	}
	stop.Detail = "no account reader is available"
	if b.Refetch != nil {
		view, err := b.Refetch()
		switch {
		case err != nil:
			stop.Detail = err.Error()
		case !view.Known:
			stop.Detail = "the account state is not readable"
		default:
			if signed, ok := hlOnChainSignedQty(view, symbol); ok {
				stop.Detail = ""
				return sized(signed, hlRemainderBasisFresh)
			}
			stop.Detail = fmt.Sprintf("the on-chain %s position has no readable side", symbol)
		}
	}
	if b.PreSend.Known && fill.Known {
		return sized(b.PreSend.Signed-s*filled, hlRemainderBasisDerived)
	}
	if b.PreSend.Known {
		return sized(b.PreSend.Signed, hlRemainderBasisStale)
	}
	return stop
}

func hlUnfilledCloseNeedsRearm(cancelRequested bool, fill hlCloseFillOutcome, canceledOIDs []int64) bool {
	return cancelRequested && (!fill.Known || len(canceledOIDs) > 0)
}

func closeRemainderBasisText(stop hlCloseRemainderStop) string {
	switch stop.Basis {
	case hlRemainderBasisFresh:
		return "the post-close account read"
	case hlRemainderBasisDerived:
		return "the pre-send account reading less the confirmed fill"
	case hlRemainderBasisStale:
		return "the pre-send account reading with no fill subtracted, because the close outcome is unknown"
	}
	return "the book remainder, because no account reading is available"
}

func closeRearmBasisNote(stop hlCloseRemainderStop) string {
	if stop.Basis == hlRemainderBasisFresh {
		return ""
	}
	return fmt.Sprintf(" The post-close account read failed (%s), so the stop size comes from %s; verify the on-chain position.", stop.Detail, closeRemainderBasisText(stop))
}

func closeRearmOutcomeNote(fill hlCloseFillOutcome) string {
	if fill.Known {
		return ""
	}
	return " The close outcome is unknown, so the close may have filled after its reply was lost; the reconciler books any fill by order id at the next cycle."
}

func hlManualCancelStopAction(sc StrategyConfig, symbol string) string {
	if sc.Type != "manual" {
		return ""
	}
	return fmt.Sprintf(" (for this `type=manual` strategy, `go-trader manual-cancel-sl %s --symbol %s` cancels the booked stop)", sc.ID, symbol)
}

func closeRearmNextAction(sc StrategyConfig, symbol string, res hlStopRearmResult, prevStopOID int64) string {
	if res.Status == hlStopRearmPreCloseStopResting {
		return fmt.Sprintf("Cancel the pre-close stop (OID=%d) on the Hyperliquid UI%s, then place the remainder stop by hand, or close the position.", prevStopOID, hlManualCancelStopAction(sc, symbol))
	}
	if res.Owner == hlRearmOwnerRecorded {
		trigger := "<price>"
		if res.TriggerPx > 0 {
			trigger = fmt.Sprintf("%.4f", res.TriggerPx)
		}
		return fmt.Sprintf("Check the open orders on Hyperliquid, then re-arm with `go-trader manual-update-sl %s --trigger %s` or close the position.", sc.ID, trigger)
	}
	return "Check the open orders on Hyperliquid, then place the stop by hand or close the position. Do not rely on a later cycle to re-arm it."
}

func closeRearmFailureText(res hlStopRearmResult, prevStopOID int64) (string, string) {
	switch res.Status {
	case hlStopRearmNoTrigger:
		return "no trigger price could be resolved", "The position has NO exchange-side stop."
	case hlStopRearmNoArm:
		return fmt.Sprintf("no re-arm path owns the cancelled stop (OID=%d)", prevStopOID), "The position has NO exchange-side stop."
	case hlStopRearmReadFailed:
		return fmt.Sprintf("the open orders could not be read (%s)", res.Detail), "Nothing was placed, and the stop state is UNVERIFIED."
	case hlStopRearmPreCloseStopResting:
		return fmt.Sprintf("the cancel of the pre-close stop (OID=%d) was rejected (%s)", prevStopOID, res.Detail), "The pre-close stop still rests at its pre-close size, so on a shared coin it can close units of a peer strategy when it fires."
	case hlStopRearmProtectionLost:
		return fmt.Sprintf("the pre-close stop was cancelled and the replacement did not rest (%s)", res.Detail), "The position has NO exchange-side stop."
	case hlStopRearmOutcomeUnknown:
		return fmt.Sprintf("the outcome could not be read (%s)", res.Detail), "The stop state is UNVERIFIED: a stop may rest untracked, or the pre-close stop may still rest. Read the open orders on Hyperliquid before you place anything."
	}
	return fmt.Sprintf("the placement was rejected (%s)", res.Detail), "The position has NO exchange-side stop."
}

func formatCloseRemovalReport(sc StrategyConfig, symbol string, rep hlCloseRemovalReport) string {
	var parts []string
	tpList := func(oids []int64) []int64 {
		var out []int64
		for _, oid := range oids {
			if oid != rep.StopOID {
				out = append(out, oid)
			}
		}
		return out
	}
	if rep.StopOID > 0 {
		switch {
		case containsInt64(rep.Removed, rep.StopOID):
			parts = append(parts, fmt.Sprintf("stop OID %d removed", rep.StopOID))
		case containsInt64(rep.Filled, rep.StopOID):
			parts = append(parts, fmt.Sprintf("stop OID %d already filled (the reconciler books it)", rep.StopOID))
		case containsInt64(rep.Resting, rep.StopOID):
			parts = append(parts, fmt.Sprintf("stop OID %d STILL RESTING at its pre-close size over units that are not this strategy's", rep.StopOID))
		default:
			parts = append(parts, fmt.Sprintf("stop OID %d UNVERIFIED", rep.StopOID))
		}
	}
	for _, leg := range []struct {
		label string
		oids  []int64
	}{
		{"removed", tpList(rep.Removed)},
		{"already filled (the reconciler books them)", tpList(rep.Filled)},
		{"STILL RESTING at their pre-close size", tpList(rep.Resting)},
		{"UNVERIFIED", tpList(rep.Unverified)},
	} {
		if len(leg.oids) > 0 {
			parts = append(parts, fmt.Sprintf("take-profit OIDs %v %s", leg.oids, leg.label))
		}
	}
	if len(parts) == 0 {
		return " No pre-close stop or take-profit order was left unconfirmed by the close."
	}
	text := " Pre-close orders: " + strings.Join(parts, "; ") + "."
	if rep.Detail != "" {
		text += " Detail: " + rep.Detail + "."
	}
	if len(rep.Resting) > 0 || len(rep.Unverified) > 0 {
		stopAction := ""
		if containsInt64(rep.Resting, rep.StopOID) || containsInt64(rep.Unverified, rep.StopOID) {
			stopAction = hlManualCancelStopAction(sc, symbol)
		}
		text += fmt.Sprintf(" Action for a resting or unverified order: read the open orders on Hyperliquid and cancel it on the Hyperliquid UI%s.", stopAction)
	}
	return text
}

func formatCloseTPLegReport(tp hlTPRearmResult) string {
	detail := ""
	if tp.Detail != "" {
		detail = " (" + tp.Detail + ")"
	}
	switch tp.Status {
	case hlTPRearmPlaced:
		return " Take-profit leg: placed at the remainder size."
	case hlTPRearmKept:
		return " Take-profit leg: the resting tiers are kept."
	case hlTPRearmRemoved:
		return fmt.Sprintf(" Take-profit leg: the pre-close tiers were removed%s; the next due protection sync places them at the book size.", detail)
	case hlTPRearmFailed:
		if tp.Unknown != "" {
			return fmt.Sprintf(" Take-profit leg FAILED%s and UNKNOWN (%s). Read the open orders on Hyperliquid before you place anything, then fix the failed take-profit orders by hand.", detail, tp.Unknown)
		}
		return fmt.Sprintf(" Take-profit leg FAILED%s. Read the open orders on Hyperliquid and fix the take-profit orders by hand.", detail)
	case hlTPRearmUnknown:
		return fmt.Sprintf(" Take-profit leg UNKNOWN%s. Read the open orders on Hyperliquid before you place anything.", detail)
	}
	return ""
}

func formatCloseRearmReport(sc StrategyConfig, symbol string, stop hlCloseRemainderStop, fill hlCloseFillOutcome, res hlStopRearmResult, tp hlTPRearmResult, prevStopOID int64) (string, bool) {
	report, critical := formatCloseStopRearmReport(sc, symbol, stop, fill, res, prevStopOID)
	return report + formatCloseTPLegReport(tp), critical || tp.critical()
}

func formatCloseStopRearmReport(sc StrategyConfig, symbol string, stop hlCloseRemainderStop, fill hlCloseFillOutcome, res hlStopRearmResult, prevStopOID int64) (string, bool) {
	notes := closeRearmBasisNote(stop) + closeRearmOutcomeNote(fill)
	fresh := stop.Basis == hlRemainderBasisFresh
	switch res.Status {
	case hlStopRearmUnbacked:
		return fmt.Sprintf("NO STOP PLACED: %s shows no on-chain units behind the %.6f remainder (the coin is flat on this side, or every unit left is in the book of a peer strategy).%s Compare the on-chain position with the books before the next close on %s.%s", closeRemainderBasisText(stop), stop.Remainder, formatCloseRemovalReport(sc, symbol, res.Removal), symbol, notes), true
	case hlStopRearmUnclaimed:
		return fmt.Sprintf("This position has no configured stop owner, so no stop was placed for the %.6f remainder.%s", stop.Remainder, notes), false
	case hlStopRearmClosed:
		return fmt.Sprintf("The %s re-arm found that %s; the reconciler books that close.%s", res.Owner, res.Detail, notes), !fresh
	case hlStopRearmPlaced:
		if res.Qty < stop.Remainder-hlSharedCloseQtyTolerance {
			return fmt.Sprintf("The %s now rests for %.6f at $%.4f (OID=%d), the on-chain units that back the remainder per %s. The other %.6f units of the %.6f remainder have no on-chain units behind them and get no stop. Compare the on-chain position with the books before the next close on %s.%s", res.Owner, res.Qty, res.TriggerPx, res.OID, closeRemainderBasisText(stop), stop.Remainder-res.Qty, stop.Remainder, symbol, notes), true
		}
		if !fresh {
			return fmt.Sprintf("The %s now rests for %.6f at $%.4f (OID=%d).%s", res.Owner, res.Qty, res.TriggerPx, res.OID, notes), true
		}
		return fmt.Sprintf("The %s now rests for %.6f at $%.4f (OID=%d), sized from %s.%s", res.Owner, res.Qty, res.TriggerPx, res.OID, closeRemainderBasisText(stop), notes), false
	}
	reason, state := closeRearmFailureText(res, prevStopOID)
	return fmt.Sprintf("The %s re-arm for %.6f did not complete: %s. %s%s %s", res.Owner, res.Qty, reason, state, notes, closeRearmNextAction(sc, symbol, res, prevStopOID)), true
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

type hlCloseUnconfirmed struct {
	StopOID int64
	TPOIDs  []int64
}

func hlCloseUnconfirmedSet(pos *Position, prevStopOID int64, requestedTPOIDs []int64) hlCloseUnconfirmed {
	var u hlCloseUnconfirmed
	if pos == nil {
		return u
	}
	if prevStopOID > 0 && pos.StopLossOID == prevStopOID {
		u.StopOID = prevStopOID
	}
	for _, oid := range requestedTPOIDs {
		if oid > 0 && containsInt64(pos.TPOIDs, oid) && !containsInt64(u.TPOIDs, oid) {
			u.TPOIDs = append(u.TPOIDs, oid)
		}
	}
	return u
}

type hlCloseRearmContext struct {
	Price         float64
	PrevStopOID   int64
	PrevTPOIDs    []int64
	PrevTriggerPx float64
	PrevHighWater float64
	FillHintsJSON []byte
	LiqPxByCoin   map[string]float64
	NetSideByCoin map[string]string
	Backing       hlCloseBacking
}

func rearmAfterSizedClose(sc StrategyConfig, stratState *StrategyState, stratDB *StateDB, symbol, side string, bookQty float64, fill hlCloseFillOutcome, rearm hlCloseRearmContext, mu *sync.RWMutex, notifier *MultiNotifier, logger *StrategyLogger) (int, string) {
	if !hyperliquidIsLive(sc.Args) {
		return 0, ""
	}
	stop := resolveHLCloseRemainderStop(symbol, side, bookQty, fill, rearm.Backing)
	if stop.Remainder <= hlSharedCloseQtyTolerance {
		return 0, ""
	}
	if stop.Unbacked {
		logger.Error("CRITICAL: close %s left %.6f on the book, but %s shows no on-chain units behind it; no stop is placed", symbol, stop.Remainder, closeRemainderBasisText(stop))
	} else {
		logger.Warn("Close %s left %.6f on the book after cancelling (or possibly cancelling) its protection; re-arming a %.6f stop in this cycle, sized from %s", symbol, stop.Remainder, stop.Qty, closeRemainderBasisText(stop))
	}
	var u hlCloseUnconfirmed
	if stratState != nil {
		mu.RLock()
		u = hlCloseUnconfirmedSet(stratState.Positions[symbol], rearm.PrevStopOID, rearm.PrevTPOIDs)
		mu.RUnlock()
	}
	trades, detail, res, tpRes := rearmProtectionForCloseRemainder(sc, stratState, stratDB, symbol, rearm.Price, rearm.PrevStopOID, rearm.PrevTriggerPx, rearm.PrevHighWater, rearm.FillHintsJSON, rearm.LiqPxByCoin, rearm.NetSideByCoin, u, stop, mu, notifier, logger)
	report, critical := formatCloseRearmReport(sc, symbol, stop, fill, res, tpRes, rearm.PrevStopOID)
	msg := ""
	switch {
	case stop.AfterFill:
		msg = formatSizedCloseShortFillAlert(sc, symbol, side, bookQty-stop.Remainder, bookQty, report, critical)
	case critical:
		msg = formatUnfilledCloseRearmAlert(sc, symbol, side, bookQty, report)
	}
	if msg == "" {
		return trades, detail
	}
	if critical {
		logger.Error("%s", msg)
	} else {
		logger.Warn("%s", msg)
	}
	notifyCloseRearm(notifier, msg)
	return trades, detail
}

func manualCycleCloseDirection(closeSide string) string {
	if closeSide == "sell" {
		return directionClose
	}
	return directionOpen
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
	live := hyperliquidIsLive(sc.Args)
	fill := hlExecuteFillOutcome(execResult, execErr, bookQty)
	execResult, execErr = confirmHyperliquidExecuteFill(execResult, execErr)
	if execErr != nil {
		logger.Error("manual close execute failed: %v", execErr)
		if live {
			notifyLiveExecFailure(notifier, sc, manualCycleCloseDirection(closeSide), sc.Symbol, execErr.Error())
		}
		canceledOIDs := hyperliquidExecuteSucceededCancelOIDs(execResult, requestedCancelOIDs)
		if len(canceledOIDs) > 0 {
			mu.Lock()
			clearHyperliquidProtectionOIDsMatching(stratState.Positions[sc.Symbol], canceledOIDs)
			mu.Unlock()
		}
		if intentFullClose && live && hlUnfilledCloseNeedsRearm(cancelRequested, fill, canceledOIDs) {
			trades, detail := rearmAfterSizedClose(sc, stratState, stratDB, sc.Symbol, side, bookQty, fill, rearm, mu, notifier, logger)
			return trades, detail, 0
		}
		return 0, "", 0
	}
	if live {
		clearLiveExecThrottle(sc, manualCycleCloseDirection(closeSide), sc.Symbol)
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
	if booking.ShortOfIntent && live {
		if extraTrades, slDetail := rearmAfterSizedClose(sc, stratState, stratDB, sc.Symbol, side, bookQty, hlCloseFillOutcome{Filled: action.Quantity, Known: true}, rearm, mu, notifier, logger); extraTrades > 0 {
			trades += extraTrades
			detail = slDetail
		}
	}
	return trades, detail, fillPx
}
