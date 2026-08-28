package main

import (
	"fmt"
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

var manualOpenCleanupCloseFn = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, string, error) {
	return RunHyperliquidClose(hyperliquidLiveCloseScript, symbol, partialSz, cancelOIDs)
}

func attemptManualOpenCleanup(symbol string, fillQty float64, stopLossOID int64, tpOIDs []int64) (bool, string) {
	cancelOIDs := make([]int64, 0, 1+len(tpOIDs))
	if stopLossOID > 0 {
		cancelOIDs = append(cancelOIDs, stopLossOID)
	}
	for _, oid := range tpOIDs {
		if oid > 0 {
			cancelOIDs = append(cancelOIDs, oid)
		}
	}

	sz := fillQty
	result, stderr, err := manualOpenCleanupCloseFn(symbol, &sz, cancelOIDs)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[manual-open cleanup] close stderr: %s\n", stderr)
	}
	if err != nil {
		return false, fmt.Sprintf("close failed: %v", err)
	}
	if result == nil {
		return false, "cleanup close returned nil result"
	}
	if result.CancelStopLossError != "" {
		return true, fmt.Sprintf("position closed but trigger cancel reported: %s", result.CancelStopLossError)
	}
	return true, "position flattened and orphan triggers cancelled"
}

func warnNotifier(notifier *MultiNotifier, msg string) {
	fmt.Fprintln(os.Stderr, "[WARN] "+msg)
	if notifier != nil && notifier.HasBackends() {
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}
