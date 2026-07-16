package main

import "testing"

func TestHedgeHotReloadBlockedWhileOpen(t *testing.T) {
	old := &Config{Strategies: []StrategyConfig{{
		ID: "a", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "ETH", "1h"},
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1},
	}}}
	next := &Config{Strategies: []StrategyConfig{{
		ID: "a", Type: "perps", Platform: "hyperliquid", Args: []string{"m", "ETH", "1h"},
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2},
	}}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"a": {ID: "a", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"},
		}},
	}}
	if err := validateHotReloadStateCompatible(old, next, state); err == nil {
		t.Fatal("expected hedge shape change blocked while open")
	}
	// flat OK
	state.Strategies["a"].Positions = map[string]*Position{}
	if err := validateHotReloadStateCompatible(old, next, state); err != nil {
		t.Fatalf("flat should allow: %v", err)
	}
}
