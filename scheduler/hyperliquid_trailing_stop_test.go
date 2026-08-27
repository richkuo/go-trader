package main

import "testing"

func TestApplyTrailingStopUpdateResultKeepsStateOnOutcomeUnknown(t *testing.T) {
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
}
