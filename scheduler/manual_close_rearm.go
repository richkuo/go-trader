package main

import (
	"fmt"
	"os"
	"time"
)

type manualCloseProtectionSnapshot struct {
	Symbol          string
	Side            string
	Quantity        float64
	StopLossOID     int64
	TriggerPx       float64
	PositionID      string
	OwnerStrategyID string
	TPOIDs          []int64
	TPArmedTiers    []bool
	FilledQty       float64
	PeerSameQty     float64
	PeerOppQty      float64
	PreSend         hlCloseView
	AvgCost         float64
	EntryATR        float64
}

type manualCloseRearmDecision int

const (
	manualCloseRearmNoCancelRequested manualCloseRearmDecision = iota
	manualCloseRearmCancelNotConfirmed
	manualCloseRearmStopCancelled
	manualCloseRearmOutcomeUnknown
)

func decideManualCloseRearm(execResult *HyperliquidExecuteResult, requestedCancelOIDs []int64, snap manualCloseProtectionSnapshot) manualCloseRearmDecision {
	if snap.StopLossOID <= 0 || !containsInt64(requestedCancelOIDs, snap.StopLossOID) {
		return manualCloseRearmNoCancelRequested
	}
	if execResult != nil && execResult.OrderOutcome == "not_sent" {
		return manualCloseRearmNoCancelRequested
	}
	if !hlExecuteFillOutcome(execResult, nil, snap.Quantity).Known {
		return manualCloseRearmOutcomeUnknown
	}
	if containsInt64(hyperliquidExecuteSucceededCancelOIDs(execResult, requestedCancelOIDs), snap.StopLossOID) {
		return manualCloseRearmStopCancelled
	}
	return manualCloseRearmCancelNotConfirmed
}

func containsInt64(haystack []int64, needle int64) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func manualCloseCancelledOIDsForAlert(execResult *HyperliquidExecuteResult, requestedCancelOIDs []int64) []int64 {
	if confirmed := hyperliquidExecuteSucceededCancelOIDs(execResult, requestedCancelOIDs); len(confirmed) > 0 {
		return confirmed
	}
	var out []int64
	for _, oid := range requestedCancelOIDs {
		if oid > 0 {
			out = append(out, oid)
		}
	}
	return out
}

func (d manualCoreDeps) hyperliquidAccountMaps() (map[string]float64, map[string]float64, map[string]string, error) {
	if d.fetchPositions == nil {
		return nil, nil, nil, fmt.Errorf("no Hyperliquid account reader is configured")
	}
	addr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
	if addr == "" {
		return nil, nil, nil, fmt.Errorf("HYPERLIQUID_ACCOUNT_ADDRESS is not set")
	}
	positions, err := d.fetchPositions(addr)
	if err != nil {
		return nil, nil, nil, err
	}
	onChainAbsQty, liqPxByCoin, netSideByCoin := buildHLLiquidationMaps(positions)
	return onChainAbsQty, liqPxByCoin, netSideByCoin, nil
}

const manualRearmUnverifiedState = "The stop-loss state on the exchange is UNVERIFIED, so the position may be UNPROTECTED."

type manualCloseRearmCause int

const (
	manualCloseRearmAfterRejection manualCloseRearmCause = iota
	manualCloseRearmAfterShortFill
)

func restoreManualStopLossAfterFailedClose(d manualCoreDeps, res *manualCoreResult, sc StrategyConfig, strategyID string, snap manualCloseProtectionSnapshot, execResult *HyperliquidExecuteResult, requestedCancelOIDs []int64) bool {
	positionFlat, _ := restoreManualStopLoss(d, res, sc, strategyID, snap, execResult, requestedCancelOIDs, manualCloseRearmAfterRejection)
	return positionFlat
}

func restoreManualStopLoss(d manualCoreDeps, res *manualCoreResult, sc StrategyConfig, strategyID string, snap manualCloseProtectionSnapshot, execResult *HyperliquidExecuteResult, requestedCancelOIDs []int64, cause manualCloseRearmCause) (bool, bool) {
	if !hyperliquidIsLive(sc.Args) {
		return false, false
	}
	decision := decideManualCloseRearm(execResult, requestedCancelOIDs, snap)
	if decision == manualCloseRearmNoCancelRequested {
		return false, false
	}
	shortFill := cause == manualCloseRearmAfterShortFill
	if decision == manualCloseRearmCancelNotConfirmed {
		if shortFill {
			res.outf("manual-close %s %s: the close filled short of the book and did not confirm the stop-loss cancel, so the previous stop (OID=%d) is checked on-chain before a stop for the %.6f remainder is placed.",
				strategyID, snap.Symbol, snap.StopLossOID, snap.Quantity)
		} else {
			res.outf("manual-close %s %s: the venue rejected the close and did not confirm the stop-loss cancel — a cancel whose reply is lost still removes the trigger, so the previous stop (OID=%d) is verified on-chain and restored rather than assumed live.",
				strategyID, snap.Symbol, snap.StopLossOID)
		}
	}

	fill := hlExecuteFillOutcome(execResult, nil, snap.Quantity)
	if shortFill {
		fill = hlCloseFillOutcome{Filled: snap.FilledQty, Known: true}
	}
	cancelledOIDs := manualCloseCancelledOIDsForAlert(execResult, requestedCancelOIDs)
	closeOutcome := "the venue rejected the manual close"
	switch {
	case shortFill:
		closeOutcome = fmt.Sprintf("the manual close filled short of the book and left %.6f open", snap.Quantity)
	case !fill.Known:
		closeOutcome = "the manual close returned no readable outcome"
	}
	notes := closeRearmOutcomeNote(fill)
	alertf := func(state, reason string) {
		msg := fmt.Sprintf("CRITICAL: [%s] %s: %s after a cancel of the exchange-side protection was requested (order ids %v) and the stop-loss re-arm did not complete: %s. %s%s Verify and re-arm now with `go-trader manual-update-sl %s --trigger %.4f` or close the position.",
			strategyID, snap.Symbol, closeOutcome, cancelledOIDs, reason, state, notes, strategyID, snap.TriggerPx)
		res.outf("%s", msg)
		notifyCloseRearm(d.notifier, msg)
	}
	criticalf := func(reason string) { alertf(manualRearmUnverifiedState, reason) }

	if snap.TriggerPx <= 0 {
		criticalf("the book recorded no stop-loss trigger to restore")
		return false, false
	}
	if d.updateSL == nil {
		criticalf("no stop-loss placement path is configured")
		return false, false
	}
	if d.recordRearmedStopLoss == nil {
		criticalf("no re-arm bookkeeping path is configured")
		return false, false
	}

	unlockSymbol := lockHyperliquidProtectionSync(snap.Symbol)
	defer unlockSymbol()

	onChainAbsQty, liqPxByCoin, netSideByCoin, mapErr := d.hyperliquidAccountMaps()
	stop := resolveHLCloseRemainderStop(snap.Symbol, snap.Side, snap.Quantity+snap.FilledQty, fill, hlCloseBacking{
		PeerSameQty: snap.PeerSameQty,
		PeerOppQty:  snap.PeerOppQty,
		PreSend:     snap.PreSend,
		Refetch: func() (hlOnChainCoinView, error) {
			if mapErr != nil {
				return hlOnChainCoinView{}, mapErr
			}
			return hlOnChainCoinView{Known: true, AbsQty: onChainAbsQty, NetSide: netSideByCoin}, nil
		},
	})
	notes = closeRearmBasisNote(stop) + closeRearmOutcomeNote(fill)
	if mapErr != nil {
		res.errf("warning: could not read the Hyperliquid account before re-arming %s (%v) — re-arming %.6f, sized from %s, with no liquidation clamp", snap.Symbol, mapErr, stop.Qty, closeRemainderBasisText(stop))
	}
	if stop.Unbacked {
		removal := removeManualUnconfirmedOrders(d, res, sc, strategyID, snap, execResult, requestedCancelOIDs, decision)
		msg := fmt.Sprintf("CRITICAL: [%s] %s: %s. No stop-loss was placed: %s shows no on-chain units behind the %.6f remainder (the coin is flat on this side, or every unit left is in the book of a peer strategy).%s Compare the on-chain position with the books before the next close on %s.%s",
			strategyID, snap.Symbol, closeOutcome, closeRemainderBasisText(stop), stop.Remainder, formatCloseRemovalReport(sc, snap.Symbol, removal), snap.Symbol, notes)
		res.outf("%s", msg)
		notifyCloseRearm(d.notifier, msg)
		return true, true
	}
	qty := stop.Qty
	if qty < stop.Remainder-hlSharedCloseQtyTolerance {
		res.errf("warning: re-arm size for %s is %.6f, the on-chain units that back the %.6f remainder per %s", snap.Symbol, qty, stop.Remainder, closeRemainderBasisText(stop))
	}
	triggerPx := snap.TriggerPx
	if clamped, ok := clampStopInsideLiquidation(snap.Side, triggerPx, hlLiquidationPxForSide(liqPxByCoin, netSideByCoin, snap.Symbol, snap.Side)); ok {
		res.errf("warning: the recorded trigger $%.4f for %s sits past the liquidation price — tightening the re-arm to $%.4f", triggerPx, snap.Symbol, clamped)
		triggerPx = clamped
	}

	result, stderr, err := func() (*HyperliquidStopLossUpdateResult, string, error) {
		unlock := lockHyperliquidTrailingUpdate(snap.Symbol)
		defer unlock()
		return d.updateSL(sc.Script, snap.Symbol, snap.Side, qty, triggerPx, snap.StopLossOID)
	}()
	if stderr != "" {
		res.errf("SL re-arm stderr: %s", stderr)
	}
	switch {
	case err != nil:
		criticalf(fmt.Sprintf("%v", err))
		return false, false
	case result == nil:
		criticalf("the stop-loss placement returned no result")
		return false, false
	case result.Error != "":
		criticalf(result.Error)
		return false, false
	}

	if recErr := d.recordRearmedStopLoss(strategyID, snap.Symbol, snap.Side, qty, snap.StopLossOID, result); recErr != nil {
		msg := fmt.Sprintf("CRITICAL: [%s] %s: the stop-loss re-arm outcome could not be recorded in the book (%v) — the book and the exchange may disagree about the stop-loss. Verify on the HL UI and reconcile before the next close.",
			strategyID, snap.Symbol, recErr)
		res.outf("%s", msg)
		notifyCloseRearm(d.notifier, msg)
	}

	switch {
	case result.StopLossFilledImmediately && result.StopLossTriggerPx > 0:
		res.outf("The re-armed stop-loss for %s filled immediately at $%.4f — the position closed on-chain and the reconciler books the close, with its venue fill and fee, at the next scheduler cycle.",
			snap.Symbol, result.StopLossTriggerPx)
		notifyManualRearmBasis(d, res, strategyID, snap, closeOutcome, stop, fill, qty)
		return true, false
	case result.StopLossFilledExternally:
		res.outf("The previous stop-loss for %s (OID=%d) had already filled on-chain, so nothing was re-armed — the reconciler will book the close.",
			snap.Symbol, snap.StopLossOID)
		return true, false
	case result.StopLossOID > 0 && shortFill:
		res.outf("Stop-loss re-armed for the remainder after the short fill: %s %.6f @ $%.4f (OID=%d, superseding the verified OID=%d).",
			snap.Symbol, qty, result.StopLossTriggerPx, result.StopLossOID, snap.StopLossOID)
		notifyManualRearmBasis(d, res, strategyID, snap, closeOutcome, stop, fill, qty)
	case result.StopLossOID > 0:
		res.outf("Stop-loss re-armed after the rejected close: %s %.6f @ $%.4f (OID=%d, superseding the verified OID=%d).",
			snap.Symbol, qty, result.StopLossTriggerPx, result.StopLossOID, snap.StopLossOID)
		notifyManualRearmBasis(d, res, strategyID, snap, closeOutcome, stop, fill, qty)
	case result.StopLossOutcomeUnknown:
		alertf("The previous stop was cancelled and the replacement's outcome could NOT be read, so it may be resting untracked and the position may be UNPROTECTED.",
			"the placement outcome could not be read; the recorded trigger was kept with an unknown order id")
	case result.CancelStopLossError != "":
		alertf(fmt.Sprintf("The venue reported the previous stop (OID=%d) still resting when its open orders were read and no replacement was placed, so the position is most likely still protected by it.", snap.StopLossOID),
			result.CancelStopLossError)
	case result.OpenOrderCheckError != "":
		alertf("The venue's open orders could not be read, so nothing was placed and the stop-loss state is UNVERIFIED.", result.OpenOrderCheckError)
	case result.StopLossError != "":
		criticalf(result.StopLossError)
	default:
		criticalf("the replacement stop-loss did not rest on-chain")
	}
	return false, false
}

func removeManualUnconfirmedOrders(d manualCoreDeps, res *manualCoreResult, sc StrategyConfig, strategyID string, snap manualCloseProtectionSnapshot, execResult *HyperliquidExecuteResult, requestedCancelOIDs []int64, decision manualCloseRearmDecision) hlCloseRemovalReport {
	var removal hlCloseRemovalReport
	stopUnconfirmed := !containsInt64(hyperliquidExecuteSucceededCancelOIDs(execResult, requestedCancelOIDs), snap.StopLossOID)
	if stopUnconfirmed && (decision == manualCloseRearmCancelNotConfirmed || decision == manualCloseRearmOutcomeUnknown) {
		removal.StopOID = snap.StopLossOID
		result, stderr, err := func() (*HyperliquidStopLossUpdateResult, string, error) {
			unlock := lockHyperliquidTrailingUpdate(snap.Symbol)
			defer unlock()
			return d.updateSL(sc.Script, snap.Symbol, snap.Side, 0, 0, snap.StopLossOID)
		}()
		if stderr != "" {
			res.errf("pre-close stop removal stderr: %s", stderr)
		}
		if err != nil {
			res.errf("warning: the pre-close stop removal for %s failed: %v", snap.Symbol, err)
		}
		stopRes := classifyStopRearmRemoval(result)
		switch stopRes.Status {
		case hlStopRearmRemoved:
			removal.Removed = append(removal.Removed, snap.StopLossOID)
			if recErr := d.recordRearmedStopLoss(strategyID, snap.Symbol, snap.Side, 0, snap.StopLossOID, result); recErr != nil {
				removal.addDetail(fmt.Sprintf("the removal of stop OID=%d could not be recorded in the book (%v), so the book may still list it", snap.StopLossOID, recErr))
			}
		case hlStopRearmClosed:
			removal.Filled = append(removal.Filled, snap.StopLossOID)
		case hlStopRearmPreCloseStopResting:
			removal.Resting = append(removal.Resting, snap.StopLossOID)
			removal.addDetail(fmt.Sprintf("stop OID=%d cancel: %s", snap.StopLossOID, stopRes.Detail))
		default:
			removal.Unverified = append(removal.Unverified, snap.StopLossOID)
			removal.addDetail(fmt.Sprintf("stop OID=%d: %s", snap.StopLossOID, stopRes.Detail))
		}
	}
	tp, recErr := removeManualUnconfirmedTakeProfits(d, sc, strategyID, snap, execResult, requestedCancelOIDs)
	removal.Removed = append(removal.Removed, tp.Removed...)
	removal.Filled = append(removal.Filled, tp.Filled...)
	removal.Resting = append(removal.Resting, tp.Resting...)
	removal.Unverified = append(removal.Unverified, tp.Unverified...)
	removal.addDetail(tp.Detail)
	if recErr != nil {
		removal.addDetail(fmt.Sprintf("the take-profit removal could not be recorded in the book (%v), so the book may still list the removed ids", recErr))
	}
	return removal
}

func notifyManualRearmBasis(d manualCoreDeps, res *manualCoreResult, strategyID string, snap manualCloseProtectionSnapshot, closeOutcome string, stop hlCloseRemainderStop, fill hlCloseFillOutcome, qty float64) {
	partly := qty < stop.Remainder-hlSharedCloseQtyTolerance
	if !partly && stop.Basis == hlRemainderBasisFresh {
		return
	}
	backing := ""
	if partly {
		backing = fmt.Sprintf(" The other %.6f units of the %.6f remainder have no on-chain units behind them and get no stop. Compare the on-chain position with the books before the next close on %s.", stop.Remainder-qty, stop.Remainder, snap.Symbol)
	}
	msg := fmt.Sprintf("CRITICAL: [%s] %s: %s. The stop-loss was re-armed for %.6f, sized from %s.%s%s%s",
		strategyID, snap.Symbol, closeOutcome, qty, closeRemainderBasisText(stop), backing, closeRearmBasisNote(stop), closeRearmOutcomeNote(fill))
	res.outf("%s", msg)
	notifyCloseRearm(d.notifier, msg)
}

func notifyCloseRearm(notifier *MultiNotifier, msg string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func hlPositionGoneForSide(onChainAbsQty map[string]float64, netSideByCoin map[string]string, symbol, side string) (bool, string) {
	if qty, ok := onChainAbsQty[symbol]; !ok || qty <= 1e-9 {
		return true, fmt.Sprintf("the venue reports no open %s position", symbol)
	}
	if netSideByCoin[symbol] != side {
		return true, fmt.Sprintf("the venue reports the %s position net %q, not %q", symbol, netSideByCoin[symbol], side)
	}
	return false, ""
}

func rearmedStopLossBookValues(result *HyperliquidStopLossUpdateResult) (int64, float64, string) {
	if result == nil {
		return 0, 0, ""
	}
	switch {
	case result.StopLossFilledImmediately && result.StopLossTriggerPx > 0:
		return 0, 0, "cancel-sl"
	case result.StopLossOID > 0:
		return result.StopLossOID, result.StopLossTriggerPx, "update-sl"
	case result.StopLossOutcomeUnknown:
		return 0, result.StopLossTriggerPx, "update-sl"
	case result.StopLossFilledExternally, result.CancelStopLossSucceeded, result.StopLossNotOpen:
		return 0, 0, "cancel-sl"
	}
	return 0, 0, ""
}

func recordRearmedStopLossInDB(cfg *Config, store *StateStore, strategyID, symbol, side string, qty float64, prevStopOID int64, result *HyperliquidStopLossUpdateResult) error {
	if store == nil {
		return nil
	}
	newOID, newTrigger, action := rearmedStopLossBookValues(result)
	if action == "" {
		return nil
	}
	state, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		return err
	}
	strategy := state.Strategies[strategyID]
	if strategy == nil {
		return nil
	}
	position := strategy.Positions[symbol]
	if position == nil {
		return nil
	}
	if action == "cancel-sl" && position.StopLossOID != 0 && position.StopLossOID != prevStopOID {
		return nil
	}
	position.StopLossOID = newOID
	position.StopLossTriggerPx = newTrigger
	queued := PendingManualAction{
		StrategyID: strategyID,
		Action:     action,
		Symbol:     symbol,
		Side:       side,
		CreatedAt:  time.Now().UTC(),
	}
	if action == "update-sl" {
		queued.Quantity = qty
		queued.StopLossOID = newOID
		queued.StopLossTriggerPx = newTrigger
	}
	return store.SaveStrategyBookQueueingManualAction(strategy, queued)
}
