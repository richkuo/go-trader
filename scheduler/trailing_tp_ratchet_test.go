package main

import (
	"testing"
)

func ratchetTestState(pos *Position) *StrategyState {
	return &StrategyState{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		Positions: map[string]*Position{pos.Symbol: pos},
	}
}

func TestApplyTrailingStopUpdateResult_ImmediateFillBooksClose(t *testing.T) {
	s := ratchetTestState(&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 5, StopLossOID: 7})
	upd := &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 95}
	fill, px := applyTrailingStopUpdateResult(s, "ETH", "long", 7, 0, false, upd, "trailing_stop_loss_immediate", nil, 0)
	if !fill || px != 95 {
		t.Fatalf("immediate fill: fill=%v px=%v want true,95", fill, px)
	}
	if p, ok := s.Positions["ETH"]; ok && p != nil && p.Quantity > 0 {
		t.Fatalf("immediate fill should have booked/closed the position, still qty=%v", p.Quantity)
	}
}

func TestApplyTrailingStopUpdateResult_CancelWithoutRestClearsStaleOID(t *testing.T) {
	s := ratchetTestState(&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 5, StopLossOID: 7, StopLossTriggerPx: 96, RatchetFallbackNormalizePending: true})
	upd := &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}
	fill, _ := applyTrailingStopUpdateResult(s, "ETH", "long", 7, 0, false, upd, "trailing_stop_loss_immediate", nil, 0)
	if fill {
		t.Fatal("cancel-without-rest: want fill=false")
	}
	p := s.Positions["ETH"]
	if p.StopLossOID != 0 || p.StopLossTriggerPx != 0 {
		t.Fatalf("stale OID/trigger not cleared: %d/%v want 0/0", p.StopLossOID, p.StopLossTriggerPx)
	}
	if !p.RatchetFallbackNormalizePending {
		t.Fatal("cancel-without-rest must leave normalize marker set for retry")
	}
}

func TestApplyTrailingStopUpdateResult_SideGuardSkipsMutation(t *testing.T) {
	s := ratchetTestState(&Position{Symbol: "ETH", Side: "short", Quantity: 1, AvgCost: 100, EntryATR: 5, StopLossOID: 7})
	upd := &HyperliquidStopLossUpdateResult{StopLossOID: 42, StopLossTriggerPx: 95}
	fill, _ := applyTrailingStopUpdateResult(s, "ETH", "long", 7, 0, false, upd, "trailing_stop_loss_immediate", nil, 0)
	if fill {
		t.Fatal("side mismatch: want fill=false")
	}
	if p := s.Positions["ETH"]; p.StopLossOID != 7 {
		t.Fatalf("side mismatch must not mutate OID, got %d want 7", p.StopLossOID)
	}
}
