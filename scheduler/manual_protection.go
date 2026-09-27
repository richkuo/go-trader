package main

import (
	"fmt"
	"math"
	"os"
	"strings"
)

var runHLSyncProtectionFn = RunHyperliquidSyncProtection

func formatProtectionSyncWarnings(result *HyperliquidProtectionSyncResult) []string {
	var warns []string
	if result.StopLossError != "" {
		warns = append(warns, "SL: "+result.StopLossError)
	}
	for i, e := range result.TPErrors {
		if e != "" {
			warns = append(warns, fmt.Sprintf("TP%d: %s", i+1, e))
		}
	}
	if len(result.TPErrors) == 0 {
		if result.TP1Error != "" {
			warns = append(warns, "TP1: "+result.TP1Error)
		}
		if result.TP2Error != "" {
			warns = append(warns, "TP2: "+result.TP2Error)
		}
	}
	for _, oid := range result.TPCancelFailedOIDs {
		if oid > 0 {
			warns = append(warns, fmt.Sprintf("surplus TP cancel OID=%d failed (will retry)", oid))
		}
	}
	for _, oid := range result.TPCancelFilledOIDs {
		if oid > 0 {
			warns = append(warns, fmt.Sprintf("surplus TP OID=%d filled on-chain (reconciler)", oid))
		}
	}
	return warns
}

func computeFallbackATR(fillPrice, leverage float64) (float64, bool) {
	if leverage <= 0 || fillPrice <= 0 {
		return 0, false
	}
	return 0.1 * fillPrice / leverage, true
}

func placeManualProtectionInline(
	sc StrategyConfig,
	side string,
	fillQty, fillPrice, entryATR, effectiveSLATRMult float64,
	stopLossOID int64,
) ([]int64, string, error) {
	tiers := strategyTPTiers(sc)
	if len(tiers) == 0 {
		return nil, "", nil
	}

	result, stderr, err := runHLSyncProtectionFn(
		sc.Script, sc.Symbol, side, fillQty, fillPrice, entryATR,
		effectiveSLATRMult, tiers, stopLossOID, nil, nil, false, nil, nil, nil,
	)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[manual-open] sync-protection stderr: %s\n", stderr)
	}
	if err != nil {
		return nil, "", err
	}
	if result == nil {
		return nil, "nil result from protection sync", nil
	}
	if result.Error != "" {
		return nil, result.Error, nil
	}

	return result.TPOIDs, strings.Join(formatProtectionSyncWarnings(result), "; "), nil
}

var manualOpenCleanupCloseFn hlSizedCloser = defaultHyperliquidSizedCloser

type manualOpenCleanupInput struct {
	StrategyID  string
	Symbol      string
	Side        string
	FillQty     float64
	StopLossOID int64
	TPOIDs      []int64
	View        manualStateView
	ViewKnown   bool
	Refetch     func() (hlOnChainCoinView, error)
}

func manualOpenCleanupPlanInputs(in manualOpenCleanupInput) (posQty, peerSame, peerOpp float64, reason string) {
	if !in.ViewKnown {
		return 0, 0, 0, "the strategy books could not be read, so the close cannot be sized against the peers on the coin"
	}
	if in.Side != "long" && in.Side != "short" {
		return 0, 0, 0, fmt.Sprintf("the opened side %q is neither long nor short", in.Side)
	}
	posQty = in.FillQty
	if in.Side == "long" {
		peerSame, peerOpp = in.View.PeerLongQty, in.View.PeerShortQty
	} else {
		peerSame, peerOpp = in.View.PeerShortQty, in.View.PeerLongQty
	}
	if own := in.View.Pos; own != nil && own.Quantity > 0 {
		if own.Side == in.Side {
			posQty += own.Quantity
		} else {
			peerOpp += own.Quantity
		}
	}
	return posQty, peerSame, peerOpp, ""
}

func attemptManualOpenCleanup(in manualOpenCleanupInput) (bool, string) {
	cancelOIDs := make([]int64, 0, 1+len(in.TPOIDs))
	if in.StopLossOID > 0 {
		cancelOIDs = append(cancelOIDs, in.StopLossOID)
	}
	for _, oid := range in.TPOIDs {
		if oid > 0 {
			cancelOIDs = append(cancelOIDs, oid)
		}
	}
	unresolved := func(remaining float64, why string) (bool, string) {
		return false, fmt.Sprintf("%s — %.6f of the %.6f %s fill that was never queued is NOT proven closed; its protection %v was not cancelled", why, remaining, in.FillQty, in.Side, cancelOIDs)
	}
	posQty, peerSame, peerOpp, reason := manualOpenCleanupPlanInputs(in)
	if reason != "" {
		return unresolved(in.FillQty, "no close was sent: "+reason)
	}
	plan := resolveHLCloseOrder(in.Symbol, in.Side, posQty, in.FillQty, hlCloseContext{PeerSameQty: peerSame, PeerOppQty: peerOpp, Refetch: in.Refetch})
	if plan.Action != hlCloseSend {
		return unresolved(in.FillQty, "no close was sent: "+plan.Reason)
	}
	req := hlSizedCloseRequest{Symbol: in.Symbol, Side: closeTradeSide(in.Side), Mode: plan.Mode, Size: plan.Size}
	if !plan.Capped && len(cancelOIDs) > 0 {
		req.CancelOIDs = cancelOIDs
		req.CancelMinFill = in.FillQty - 0.0001
	}
	result, err := manualOpenCleanupCloseFn(req)
	outcome := classifyHLSizedClose(result, err)
	switch {
	case outcome.NotSent:
		return unresolved(in.FillQty, "the close was not sent: "+outcome.Detail)
	case !outcome.Known:
		return unresolved(in.FillQty, fmt.Sprintf("the close outcome is UNKNOWN (%s); no second order was sent", outcome.Detail))
	case outcome.Filled <= 0:
		return unresolved(in.FillQty, "the venue rejected the close: "+outcome.Detail)
	}
	filled := math.Min(outcome.Filled, in.FillQty)
	if filled < in.FillQty-0.0001 {
		why := fmt.Sprintf("the %s close filled %.6f of the %.6f fill", operatorSizedCloseLabel(plan.Mode), filled, in.FillQty)
		if plan.Capped {
			why += fmt.Sprintf(" after it was capped to %.6f by the on-chain position (%s)", plan.Size, plan.Reason)
		}
		return unresolved(in.FillQty-filled, why)
	}
	cancelled := hyperliquidSucceededCancelOIDs(result, req.CancelOIDs)
	if len(cancelled) < len(req.CancelOIDs) {
		return false, fmt.Sprintf("position closed (%.6f %s) but the cancel of the orphan triggers was not confirmed (requested %v, confirmed %v: %s) — cancel them on the Hyperliquid UI", filled, operatorSizedCloseLabel(plan.Mode), req.CancelOIDs, cancelled, result.CancelStopLossError)
	}
	return true, fmt.Sprintf("position flattened (%.6f %s) and orphan triggers cancelled", filled, operatorSizedCloseLabel(plan.Mode))
}

func warnNotifier(notifier *MultiNotifier, msg string) {
	fmt.Fprintln(os.Stderr, "[WARN] "+msg)
	if notifier != nil && notifier.HasBackends() {
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}
