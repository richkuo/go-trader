package main

import (
	"testing"
	"time"
)

// TestPositionSLAfterFieldsRoundTrip verifies that the new #708 columns
// (sl_adjusted_tiers_processed, post_tp_trailing_atr_mult) survive a
// save/load cycle.
func TestPositionSLAfterFieldsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	trailMult := 1.25
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"s1": {
				ID: "s1", Type: "perps", Platform: "hyperliquid",
				Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{
					"BTC": {
						Symbol:                   "BTC",
						Quantity:                 0.1,
						AvgCost:                  50000,
						Side:                     "long",
						OpenedAt:                 now,
						SLAdjustedTiersProcessed: 1,
						PostTPTrailingATRMult:    &trailMult,
					},
				},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	pos := loaded.Strategies["s1"].Positions["BTC"]
	if pos == nil {
		t.Fatal("loaded position is nil")
	}
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Errorf("SLAdjustedTiersProcessed = %d, want 1", pos.SLAdjustedTiersProcessed)
	}
	if pos.PostTPTrailingATRMult == nil {
		t.Fatal("PostTPTrailingATRMult is nil after load")
	}
	if *pos.PostTPTrailingATRMult != 1.25 {
		t.Errorf("PostTPTrailingATRMult = %v, want 1.25", *pos.PostTPTrailingATRMult)
	}
}

// TestPositionSLAfterFields_DefaultsForLegacyRow confirms that a row written
// without explicit values lands on the documented defaults (0, nil).
func TestPositionSLAfterFields_DefaultsForLegacyRow(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"s1": {
				ID: "s1", Type: "perps", Platform: "hyperliquid",
				Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long"},
				},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	pos := loaded.Strategies["s1"].Positions["BTC"]
	if pos.SLAdjustedTiersProcessed != 0 {
		t.Errorf("SLAdjustedTiersProcessed default = %d, want 0", pos.SLAdjustedTiersProcessed)
	}
	if pos.PostTPTrailingATRMult != nil {
		t.Errorf("PostTPTrailingATRMult default = %v, want nil", *pos.PostTPTrailingATRMult)
	}
}
