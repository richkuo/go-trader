package main

import "testing"

func TestHyperliquidTrailingStopUpdate(t *testing.T) {
	t.Run("ApplyResultKeepsStateOnOutcomeUnknown", func(t *testing.T) {
		newSS := func() *StrategyState {
			return &StrategyState{
				ID:        "hl-live",
				Cash:      1000,
				Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 25, StopLossOID: 111, StopLossTriggerPx: 1850}},
			}
		}
		logger := newTestLogger(t)

		ss := newSS()
		fill, _ := applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
			&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true, StopLossTriggerPx: 1860}, "", logger, 0)
		if fill {
			t.Fatalf("immediateFill = true, want false")
		}
		pos := ss.Positions["ETH"]
		if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 1860 {
			t.Fatalf("outcome-unknown left oid=%d trigger=%.2f, want 0 / 1860 (dead OID unrecorded, requested trigger kept)", pos.StopLossOID, pos.StopLossTriggerPx)
		}
		if pos.StopLossOID == 0 && pos.StopLossTriggerPx <= 0 {
			t.Errorf("position reads as Unprotected after an outcome-unknown placement — a re-arm would stack a second untracked stop")
		}
		ss = newSS()
		if _, _ = applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
			&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true}, "", logger, 0); ss.Positions["ETH"].StopLossOID != 0 || ss.Positions["ETH"].StopLossTriggerPx != 1850 {
			t.Errorf("trigger-less payload: oid=%d trigger=%.2f, want 0 / 1850", ss.Positions["ETH"].StopLossOID, ss.Positions["ETH"].StopLossTriggerPx)
		}

		ss = newSS()
		if _, _ = applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
			&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}, "", logger, 0); ss.Positions["ETH"].StopLossOID != 0 || ss.Positions["ETH"].StopLossTriggerPx != 0 {
			t.Errorf("rejected placement left stale state: OID %d trigger %.2f, want 0 / 0", ss.Positions["ETH"].StopLossOID, ss.Positions["ETH"].StopLossTriggerPx)
		}

		ss = newSS()
		if _, _ = applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
			&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true, StopLossOID: 222, StopLossTriggerPx: 1900}, "", logger, 0); ss.Positions["ETH"].StopLossOID != 222 {
			t.Errorf("resolved placement not adopted: OID %d, want 222", ss.Positions["ETH"].StopLossOID)
		}

		ss = newSS()
		if fill, fillPx := applyTrailingStopUpdateResult(ss, "ETH", "long", 111, 0, true,
			&HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true, StopLossFilledImmediately: true, StopLossTriggerPx: 1850}, "", logger, 0); !fill || fillPx != 1850 {
			t.Errorf("fill = %v @ %.2f, want true @ 1850", fill, fillPx)
		}
	})
	t.Run("RunDefersOnOpenOrderCheckFailure", func(t *testing.T) {
		old := runHyperliquidUpdateStopLossFunc
		defer func() { runHyperliquidUpdateStopLossFunc = old }()

		runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			return &HyperliquidStopLossUpdateResult{OpenOrderCheckError: "indexer down"}, "", nil
		}

		trail := 3.0
		sc := StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", TrailingStopPct: &trail}
		logger := silentStrategyLogger("hl-test")
		defer logger.Close()

		newHighWater, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 0.5, &Position{AvgCost: 100}, 110, 100, 97, 111, trailingReplacePolicy{}, nil, logger)
		if ok || result == nil {
			t.Fatalf("runHyperliquidTrailingStopUpdate = (%+v, %v), want deferred result", result, ok)
		}
		if newHighWater != 100 {
			t.Fatalf("newHighWater=%v, want unchanged 100", newHighWater)
		}
	})
}
