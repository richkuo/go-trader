package main

import (
	"testing"
	"time"
)

func TestManualCloseRearmWaitsForThePerSymbolStopLock(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	sc := StrategyConfig{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
		Script: "shared_scripts/check_hyperliquid.py",
		Args:   []string{"hold", "ETH", "1h", "--mode=live"}}
	snap := manualCloseProtectionSnapshot{Symbol: "ETH", Side: "long", Quantity: 0.4, StopLossOID: 5150, TriggerPx: 1900}

	started := make(chan struct{})
	d := manualCoreDeps{
		updateSL: func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			close(started)
			return &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: triggerPx}, "", nil
		},
		recordRearmedStopLoss: func(strategyID, symbol, side string, qty float64, prevStopOID int64, result *HyperliquidStopLossUpdateResult) error {
			return nil
		},
	}

	unlock := lockHyperliquidTrailingUpdate(snap.Symbol)
	done := make(chan struct{})
	go func() {
		defer close(done)
		restoreManualStopLossAfterFailedClose(d, &manualCoreResult{}, sc, sc.ID, snap,
			&HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{snap.StopLossOID}},
			[]int64{snap.StopLossOID})
	}()

	select {
	case <-started:
		unlock()
		t.Fatal("the re-arm placed a stop while the per-symbol stop lock was held by another replacement")
	case <-time.After(200 * time.Millisecond):
	}

	unlock()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-arm never placed a stop after the per-symbol stop lock was released")
	}
	<-done
}
