package main

import (
	"fmt"
	"os"
	"strings"
)

// runHLSyncProtectionFn is a package var so tests can stub without spawning Python.
var runHLSyncProtectionFn = RunHyperliquidSyncProtection

// formatProtectionSyncWarnings extracts per-field error strings from a
// HyperliquidProtectionSyncResult into a flat slice. Prefers the tiered
// TPErrors slice; falls back to legacy TP1Error/TP2Error scalar fields.
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
	return warns
}

// computeFallbackATR returns a leverage-aware ATR fallback of 0.1*fillPrice/leverage,
// representing a price move that risks 10% of deployed margin at 1× ATR. Returns
// ok=false when leverage or fillPrice are not positive (caller must warn naked).
func computeFallbackATR(fillPrice, leverage float64) (float64, bool) {
	if leverage <= 0 || fillPrice <= 0 {
		return 0, false
	}
	return 0.1 * fillPrice / leverage, true
}

// placeManualProtectionInline calls --sync-protection inline after a manual fill
// and returns the placed TP OIDs. Returns (nil, "", nil) when no tiers are configured
// without spawning Python. A non-empty warnMsg signals a partial failure (position
// remains open; caller should warn but not abort).
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
		effectiveSLATRMult, tiers, stopLossOID, nil, nil,
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

// manualOpenCleanupCloseFn is the close path used by attemptManualOpenCleanup.
// Exposed as a package var so tests can stub without spawning Python (#634).
var manualOpenCleanupCloseFn = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, string, error) {
	return RunHyperliquidClose(hyperliquidLiveCloseScript, symbol, partialSz, cancelOIDs)
}

// attemptManualOpenCleanup tries to flatten a position that was just opened by
// manual-open and cancel its protective triggers, after a fatal error
// (typically pending-action queue insert failure) prevented the scheduler from
// adopting the position. Without this, the next reconcile cycle would see an
// unowned on-chain position with orphaned reduce-only SL/TP orders (#634).
//
// Sized to fillQty (not the full on-chain position) so a peer manual/perps
// position on the same coin is preserved. Returns (cleanedUp, msg) where msg
// is suitable for inclusion in an operator notification.
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

// warnNotifier writes msg to stderr and, when the notifier has backends, also
// broadcasts to all channels and fires an owner DM.
func warnNotifier(notifier *MultiNotifier, msg string) {
	fmt.Fprintln(os.Stderr, "[WARN] "+msg)
	if notifier != nil && notifier.HasBackends() {
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}
