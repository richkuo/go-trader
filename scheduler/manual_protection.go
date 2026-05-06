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
	tiers := hyperliquidProtectionTiers(sc)
	if len(tiers) == 0 {
		return nil, "", nil
	}

	result, stderr, err := runHLSyncProtectionFn(
		sc.Script, sc.Symbol, side, fillQty, fillPrice, entryATR,
		effectiveSLATRMult, tiers, stopLossOID, nil,
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

// warnNotifier writes msg to stderr and, when the notifier has backends, also
// broadcasts to all channels and fires an owner DM.
func warnNotifier(notifier *MultiNotifier, msg string) {
	fmt.Fprintln(os.Stderr, "[WARN] "+msg)
	if notifier != nil && notifier.HasBackends() {
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}
