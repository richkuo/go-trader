package main

import (
	"fmt"
	"os"
	"time"
)

type manualCloseProtectionSnapshot struct {
	Symbol      string
	Side        string
	Quantity    float64
	StopLossOID int64
	TriggerPx   float64
}

type manualCloseRearmDecision int

const (
	manualCloseRearmNoCancelRequested manualCloseRearmDecision = iota
	manualCloseRearmCancelNotConfirmed
	manualCloseRearmStopCancelled
	manualCloseRearmOutcomeUnknown
)

func (d manualCloseRearmDecision) rearms() bool {
	return d == manualCloseRearmStopCancelled || d == manualCloseRearmOutcomeUnknown
}

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

func restoreManualStopLossAfterFailedClose(d manualCoreDeps, res *manualCoreResult, sc StrategyConfig, strategyID string, snap manualCloseProtectionSnapshot, execResult *HyperliquidExecuteResult, requestedCancelOIDs []int64) {
	if !hyperliquidIsLive(sc.Args) {
		return
	}
	decision := decideManualCloseRearm(execResult, requestedCancelOIDs, snap)
	switch decision {
	case manualCloseRearmNoCancelRequested:
		return
	case manualCloseRearmCancelNotConfirmed:
		res.outf("manual-close %s %s: the venue rejected the close and did not confirm the stop-loss cancel — the original trigger $%.4f (OID=%d) is still resting on-chain, so nothing was re-armed.",
			strategyID, snap.Symbol, snap.TriggerPx, snap.StopLossOID)
		return
	}

	cancelledOIDs := manualCloseCancelledOIDsForAlert(execResult, requestedCancelOIDs)
	criticalf := func(reason string) {
		msg := fmt.Sprintf("CRITICAL: [%s] %s: the venue rejected the manual close after the exchange-side protection was cancelled (order ids %v) and the stop-loss re-arm FAILED: %s. The position is UNPROTECTED on-chain. Re-arm now with `go-trader manual-update-sl %s --trigger %.4f` or close the position.",
			strategyID, snap.Symbol, cancelledOIDs, reason, strategyID, snap.TriggerPx)
		res.outf("%s", msg)
		notifyManualCloseRearmFailure(d.notifier, msg)
	}

	if snap.TriggerPx <= 0 {
		criticalf("the book recorded no stop-loss trigger to restore")
		return
	}
	if d.updateSL == nil {
		criticalf("no stop-loss placement path is configured")
		return
	}
	if d.recordRearmedStopLoss == nil {
		criticalf("no re-arm bookkeeping path is configured")
		return
	}

	onChainAbsQty, liqPxByCoin, netSideByCoin, mapErr := d.hyperliquidAccountMaps()
	if mapErr != nil {
		res.errf("warning: could not read the Hyperliquid account before re-arming %s (%v) — re-arming at the recorded size with no liquidation clamp", snap.Symbol, mapErr)
	}
	qty, capped := hlSLEffectiveQty(snap.Symbol, snap.Quantity, onChainAbsQty)
	if capped {
		res.errf("warning: re-arm size for %s capped from the recorded %.6f to the on-chain %.6f", snap.Symbol, snap.Quantity, qty)
	}
	triggerPx := snap.TriggerPx
	if clamped, ok := clampStopInsideLiquidation(snap.Side, triggerPx, hlLiquidationPxForSide(liqPxByCoin, netSideByCoin, snap.Symbol, snap.Side)); ok {
		res.errf("warning: the recorded trigger $%.4f for %s sits past the liquidation price — tightening the re-arm to $%.4f", triggerPx, snap.Symbol, clamped)
		triggerPx = clamped
	}

	result, stderr, err := d.updateSL(sc.Script, snap.Symbol, snap.Side, qty, triggerPx, snap.StopLossOID)
	if stderr != "" {
		res.errf("SL re-arm stderr: %s", stderr)
	}
	switch {
	case err != nil:
		criticalf(fmt.Sprintf("%v", err))
		return
	case result == nil:
		criticalf("the stop-loss placement returned no result")
		return
	case result.Error != "":
		criticalf(result.Error)
		return
	}

	if recErr := d.recordRearmedStopLoss(strategyID, snap.Symbol, snap.Side, qty, snap.StopLossOID, result); recErr != nil {
		msg := fmt.Sprintf("CRITICAL: [%s] %s: the stop-loss re-arm outcome could not be recorded in the book (%v) — the book and the exchange may disagree about the stop-loss. Verify on the HL UI and reconcile before the next close.",
			strategyID, snap.Symbol, recErr)
		res.outf("%s", msg)
		notifyManualCloseRearmFailure(d.notifier, msg)
	}

	switch {
	case result.StopLossFilledImmediately && result.StopLossTriggerPx > 0:
		res.outf("The re-armed stop-loss for %s filled immediately at $%.4f — the position closed on-chain and the close is queued for the next scheduler cycle.",
			snap.Symbol, result.StopLossTriggerPx)
	case result.StopLossFilledExternally:
		res.outf("The previous stop-loss for %s (OID=%d) had already filled on-chain, so nothing was re-armed — the reconciler will book the close.",
			snap.Symbol, snap.StopLossOID)
	case result.StopLossOID > 0:
		res.outf("Stop-loss re-armed after the rejected close: %s %.6f @ $%.4f (OID=%d, replacing the cancelled OID=%d).",
			snap.Symbol, qty, result.StopLossTriggerPx, result.StopLossOID, snap.StopLossOID)
	case result.StopLossOutcomeUnknown:
		criticalf("the placement outcome could not be read; the recorded trigger was kept with an unknown order id")
	case result.CancelStopLossError != "":
		criticalf(result.CancelStopLossError)
	case result.OpenOrderCheckError != "":
		criticalf(result.OpenOrderCheckError)
	case result.StopLossError != "":
		criticalf(result.StopLossError)
	default:
		criticalf("the replacement stop-loss did not rest on-chain")
	}
}

func notifyManualCloseRearmFailure(notifier *MultiNotifier, msg string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}

func rearmedStopLossBookValues(result *HyperliquidStopLossUpdateResult) (int64, float64, bool) {
	if result == nil {
		return 0, 0, false
	}
	switch {
	case result.StopLossOID > 0:
		return result.StopLossOID, result.StopLossTriggerPx, true
	case result.StopLossOutcomeUnknown:
		return 0, result.StopLossTriggerPx, true
	}
	return 0, 0, false
}

func recordRearmedStopLossInDB(cfg *Config, store *StateStore, strategyID, symbol, side string, qty float64, prevStopOID int64, result *HyperliquidStopLossUpdateResult) error {
	if store == nil || result == nil {
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

	if result.StopLossFilledImmediately && result.StopLossTriggerPx > 0 {
		fillQty := qty
		if fillQty > position.Quantity {
			fillQty = position.Quantity
		}
		if fillQty <= 0 {
			return nil
		}
		realizedPnL := fillQty * (result.StopLossTriggerPx - position.AvgCost)
		if position.Side == "short" {
			realizedPnL = fillQty * (position.AvgCost - result.StopLossTriggerPx)
		}
		return store.InsertPendingManualAction(PendingManualAction{
			StrategyID:  strategyID,
			Action:      "close",
			Symbol:      symbol,
			Side:        closeTradeSide(position.Side),
			Quantity:    fillQty,
			FillPrice:   result.StopLossTriggerPx,
			RealizedPnL: realizedPnL,
			IsFullClose: position.Quantity-fillQty <= 0.0001,
			CreatedAt:   time.Now().UTC(),
		})
	}

	newOID, newTrigger, ok := rearmedStopLossBookValues(result)
	if !ok {
		return nil
	}
	position.StopLossOID = newOID
	position.StopLossTriggerPx = newTrigger
	if err := store.SaveStrategyBook(strategy); err != nil {
		return err
	}
	return store.InsertPendingManualAction(PendingManualAction{
		StrategyID:        strategyID,
		Action:            "update-sl",
		Symbol:            symbol,
		Side:              side,
		Quantity:          qty,
		StopLossOID:       newOID,
		StopLossTriggerPx: newTrigger,
		CreatedAt:         time.Now().UTC(),
	})
}
