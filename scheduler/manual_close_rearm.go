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
	PlanShare       float64
	PlanShareKnown  bool
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
	if execResult == nil {
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
	return restoreManualStopLoss(d, res, sc, strategyID, snap, execResult, requestedCancelOIDs, manualCloseRearmAfterRejection)
}

func restoreManualStopLoss(d manualCoreDeps, res *manualCoreResult, sc StrategyConfig, strategyID string, snap manualCloseProtectionSnapshot, execResult *HyperliquidExecuteResult, requestedCancelOIDs []int64, cause manualCloseRearmCause) bool {
	if !hyperliquidIsLive(sc.Args) {
		return false
	}
	decision := decideManualCloseRearm(execResult, requestedCancelOIDs, snap)
	if decision == manualCloseRearmNoCancelRequested {
		return false
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

	cancelledOIDs := manualCloseCancelledOIDsForAlert(execResult, requestedCancelOIDs)
	closeOutcome := "the venue rejected the manual close"
	if shortFill {
		closeOutcome = fmt.Sprintf("the manual close filled short of the book and left %.6f open", snap.Quantity)
	}
	alertf := func(state, reason string) {
		msg := fmt.Sprintf("CRITICAL: [%s] %s: %s after a cancel of the exchange-side protection was requested (order ids %v) and the stop-loss re-arm did not complete: %s. %s Verify and re-arm now with `go-trader manual-update-sl %s --trigger %.4f` or close the position.",
			strategyID, snap.Symbol, closeOutcome, cancelledOIDs, reason, state, strategyID, snap.TriggerPx)
		res.outf("%s", msg)
		notifyManualCloseRearmFailure(d.notifier, msg)
	}
	criticalf := func(reason string) { alertf(manualRearmUnverifiedState, reason) }

	if snap.TriggerPx <= 0 {
		criticalf("the book recorded no stop-loss trigger to restore")
		return false
	}
	if d.updateSL == nil {
		criticalf("no stop-loss placement path is configured")
		return false
	}
	if d.recordRearmedStopLoss == nil {
		criticalf("no re-arm bookkeeping path is configured")
		return false
	}

	unlockSymbol := lockHyperliquidProtectionSync(snap.Symbol)
	defer unlockSymbol()

	onChainAbsQty, liqPxByCoin, netSideByCoin, mapErr := d.hyperliquidAccountMaps()
	stop := resolveHLCloseRemainderStop(snap.Symbol, snap.Side, snap.Quantity+snap.FilledQty, snap.FilledQty, execResult != nil, hlCloseBacking{
		PlanShare:      snap.PlanShare,
		PlanShareKnown: snap.PlanShareKnown,
		PeerSameQty:    snap.PeerSameQty,
		PeerOppQty:     snap.PeerOppQty,
		Refetch: func() (hlOnChainCoinView, error) {
			if mapErr != nil {
				return hlOnChainCoinView{}, mapErr
			}
			return hlOnChainCoinView{Known: true, AbsQty: onChainAbsQty, NetSide: netSideByCoin}, nil
		},
	})
	unbackedf := func(basis string) {
		msg := fmt.Sprintf("CRITICAL: [%s] %s: %s. No stop-loss was placed: %s shows no on-chain units behind the %.6f remainder (the coin is flat on this side, or every unit left is in the book of a peer strategy). Compare the on-chain position with the books before the next close on %s.",
			strategyID, snap.Symbol, closeOutcome, basis, snap.Quantity, snap.Symbol)
		res.outf("%s", msg)
		notifyManualCloseRearmFailure(d.notifier, msg)
	}
	if mapErr != nil && stop.measured() {
		res.errf("warning: could not read the Hyperliquid account before re-arming %s (%v) — re-arming %.6f, sized from %s, with no liquidation clamp", snap.Symbol, mapErr, stop.Qty, closeRemainderBasisText(stop))
	} else if mapErr != nil {
		res.errf("warning: could not read the Hyperliquid account before re-arming %s (%v) — re-arming at the recorded %.6f with no liquidation clamp and no check of which on-chain units back it", snap.Symbol, mapErr, stop.Qty)
	} else if gone, detail := hlPositionGoneForSide(onChainAbsQty, netSideByCoin, snap.Symbol, snap.Side); gone {
		if shortFill {
			unbackedf(fmt.Sprintf("a post-close account reading (%s)", detail))
		} else {
			res.outf("manual-close %s %s: %s, so the close order most likely filled after the command lost its reply — no stop-loss was placed; the reconciler books the close at the next scheduler cycle.",
				strategyID, snap.Symbol, detail)
		}
		return true
	}
	if stop.unbacked() {
		unbackedf(closeRemainderBasisText(stop))
		return true
	}
	qty, capped := stop.Qty, false
	if !stop.measured() {
		qty, capped = hlSLEffectiveQty(snap.Symbol, stop.Qty, onChainAbsQty)
	}
	if capped {
		res.errf("warning: re-arm size for %s capped from the recorded %.6f to the on-chain %.6f", snap.Symbol, snap.Quantity, qty)
	}
	if stop.measured() && qty < snap.Quantity-hlSharedCloseQtyTolerance {
		res.errf("warning: re-arm size for %s is %.6f, the on-chain units that back the %.6f remainder per %s", snap.Symbol, qty, snap.Quantity, closeRemainderBasisText(stop))
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
		return false
	case result == nil:
		criticalf("the stop-loss placement returned no result")
		return false
	case result.Error != "":
		criticalf(result.Error)
		return false
	}

	if recErr := d.recordRearmedStopLoss(strategyID, snap.Symbol, snap.Side, qty, snap.StopLossOID, result); recErr != nil {
		msg := fmt.Sprintf("CRITICAL: [%s] %s: the stop-loss re-arm outcome could not be recorded in the book (%v) — the book and the exchange may disagree about the stop-loss. Verify on the HL UI and reconcile before the next close.",
			strategyID, snap.Symbol, recErr)
		res.outf("%s", msg)
		notifyManualCloseRearmFailure(d.notifier, msg)
	}

	switch {
	case result.StopLossFilledImmediately && result.StopLossTriggerPx > 0:
		res.outf("The re-armed stop-loss for %s filled immediately at $%.4f — the position closed on-chain and the reconciler books the close, with its venue fill and fee, at the next scheduler cycle.",
			snap.Symbol, result.StopLossTriggerPx)
		return true
	case result.StopLossFilledExternally:
		res.outf("The previous stop-loss for %s (OID=%d) had already filled on-chain, so nothing was re-armed — the reconciler will book the close.",
			snap.Symbol, snap.StopLossOID)
		return true
	case result.StopLossOID > 0 && shortFill:
		res.outf("Stop-loss re-armed for the remainder after the short fill: %s %.6f @ $%.4f (OID=%d, superseding the verified OID=%d).",
			snap.Symbol, qty, result.StopLossTriggerPx, result.StopLossOID, snap.StopLossOID)
		notifyManualPartlyBackedRearm(d, res, strategyID, snap, closeOutcome, stop, qty)
	case result.StopLossOID > 0:
		res.outf("Stop-loss re-armed after the rejected close: %s %.6f @ $%.4f (OID=%d, superseding the verified OID=%d).",
			snap.Symbol, qty, result.StopLossTriggerPx, result.StopLossOID, snap.StopLossOID)
		notifyManualPartlyBackedRearm(d, res, strategyID, snap, closeOutcome, stop, qty)
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
	return false
}

func notifyManualPartlyBackedRearm(d manualCoreDeps, res *manualCoreResult, strategyID string, snap manualCloseProtectionSnapshot, closeOutcome string, stop hlCloseRemainderStop, qty float64) {
	if !stop.measured() || qty >= snap.Quantity-hlSharedCloseQtyTolerance {
		return
	}
	msg := fmt.Sprintf("CRITICAL: [%s] %s: %s. The stop-loss was re-armed for %.6f, the on-chain units that back the remainder per %s. The other %.6f units of the %.6f remainder have no on-chain units behind them and get no stop. Compare the on-chain position with the books before the next close on %s.",
		strategyID, snap.Symbol, closeOutcome, qty, closeRemainderBasisText(stop), snap.Quantity-qty, snap.Quantity, snap.Symbol)
	res.outf("%s", msg)
	notifyManualCloseRearmFailure(d.notifier, msg)
}

func notifyManualCloseRearmFailure(notifier *MultiNotifier, msg string) {
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
	case result.StopLossFilledExternally, result.CancelStopLossSucceeded:
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
