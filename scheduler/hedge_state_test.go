package main

import (
	"testing"
	"time"
)

// TestHedgePositionRoundTrip verifies the hedge_for / hedge_primary_qty_basis
// columns persist and restore through SaveState/LoadState — startup recovery
// (#1159 acceptance 3) depends on this.
func TestHedgePositionRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"eth-long": {
				ID:       "eth-long",
				Type:     "perps",
				Platform: "hyperliquid",
				Cash:     1000,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 3000, Side: "long", Multiplier: 1, OwnerStrategyID: "eth-long", OpenedAt: now},
					"BTC": {Symbol: "BTC", Quantity: 0.05, AvgCost: 60000, Side: "short", Multiplier: 1, OwnerStrategyID: "eth-long", OpenedAt: now, HedgeFor: "ETH", HedgePrimaryQtyBasis: 1.0},
				},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
				RiskState:       RiskState{PeakValue: 1000},
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
	s := loaded.Strategies["eth-long"]
	if s == nil {
		t.Fatal("strategy missing after load")
	}
	hedge := s.Positions["BTC"]
	if hedge == nil {
		t.Fatal("hedge position missing after load")
	}
	if hedge.HedgeFor != "ETH" {
		t.Errorf("HedgeFor = %q, want ETH", hedge.HedgeFor)
	}
	if hedge.HedgePrimaryQtyBasis != 1.0 {
		t.Errorf("HedgePrimaryQtyBasis = %g, want 1.0", hedge.HedgePrimaryQtyBasis)
	}
	// The primary must NOT carry hedge metadata.
	if p := s.Positions["ETH"]; p == nil || p.HedgeFor != "" {
		t.Errorf("primary should have empty HedgeFor, got %+v", p)
	}
}

// TestRecordHedgeTradeResult verifies a hedge leg's realized PnL feeds daily-PnL
// accounting but never the consecutive-loss streak (#1159).
func TestRecordHedgeTradeResult(t *testing.T) {
	r := &RiskState{ConsecutiveLosses: 2, DailyPnLDate: time.Now().UTC().Format("2006-01-02")}
	RecordHedgeTradeResult(r, -50) // a losing hedge leg
	if r.ConsecutiveLosses != 2 {
		t.Errorf("ConsecutiveLosses = %d, want 2 (hedge loss must not extend the streak)", r.ConsecutiveLosses)
	}
	if r.DailyPnL != -50 {
		t.Errorf("DailyPnL = %g, want -50", r.DailyPnL)
	}
	// A winning hedge leg must not reset the streak either.
	RecordHedgeTradeResult(r, 30)
	if r.ConsecutiveLosses != 2 {
		t.Errorf("ConsecutiveLosses = %d, want 2 (hedge win must not reset the streak)", r.ConsecutiveLosses)
	}
	if r.DailyPnL != -20 {
		t.Errorf("DailyPnL = %g, want -20", r.DailyPnL)
	}
}

// TestHedgeHotReloadBlockedWhileOpen verifies a hedge-block change is blocked
// while a hedge leg is open and allowed while flat (#1159 acceptance 6).
func TestHedgeHotReloadBlockedWhileOpen(t *testing.T) {
	mk := func(ratio float64) *Config {
		sc := StrategyConfig{
			ID: "eth-long", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10,
			Leverage: 5, MarginMode: "isolated",
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: ratio, Leverage: 3, MarginMode: "cross"},
		}
		return minimalReloadConfig([]StrategyConfig{sc})
	}
	openWithHedge := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.05, AvgCost: 60000, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1},
		}},
	}}
	flat := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", Positions: map[string]*Position{}},
	}}

	// Changing ratio while a hedge leg is open must be blocked.
	if err := validateHotReloadStateCompatible(mk(1.0), mk(2.0), openWithHedge); err == nil {
		t.Error("expected hedge-block change to be blocked while a hedge leg is open")
	}
	// Same change while flat must be allowed.
	if err := validateHotReloadStateCompatible(mk(1.0), mk(2.0), flat); err != nil {
		t.Errorf("hedge-block change while flat should be allowed, got %v", err)
	}
}

// TestClassifyHedgePositionTradeType verifies a hedge leg labels as "hedge".
func TestClassifyHedgePositionTradeType(t *testing.T) {
	s := &StrategyState{Platform: "hyperliquid", Type: "perps"}
	hedge := &Position{Symbol: "BTC", Multiplier: 1, HedgeFor: "ETH"}
	if got := classifyPositionTradeType(s, hedge); got != "hedge" {
		t.Errorf("classifyPositionTradeType(hedge) = %q, want hedge", got)
	}
	primary := &Position{Symbol: "ETH", Multiplier: 1}
	if got := classifyPositionTradeType(s, primary); got != "perps" {
		t.Errorf("classifyPositionTradeType(primary) = %q, want perps", got)
	}
}
