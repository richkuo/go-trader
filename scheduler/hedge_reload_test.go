package main

import (
	"strings"
	"testing"
)

// hedgeReloadStrategy builds a minimal HL perps strategy for reload tests.
func hedgeReloadStrategy(hedge *HedgeConfig) StrategyConfig {
	slMult := 2.0
	return StrategyConfig{
		ID:              "hl-eth",
		Type:            "perps",
		Platform:        "hyperliquid",
		Direction:       "long",
		Script:          "check_hyperliquid.py",
		Args:            []string{"check_hyperliquid.py", "ETH", "live"},
		OpenStrategy:    StrategyRef{Name: "tema_cross_bd"},
		Leverage:        5,
		MarginMode:      "isolated",
		StopLossATRMult: &slMult,
		Hedge:           hedge,
	}
}

func TestHedgeHotReloadBlockedWhileHedgeLegOpen(t *testing.T) {
	oldCfg := minimalReloadConfig([]StrategyConfig{hedgeReloadStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0})})
	newCfg := minimalReloadConfig([]StrategyConfig{hedgeReloadStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2.0})})

	// A residual hedge leg (primary already flat) must still block the edit,
	// since strategyHasOpenPositions counts the hedge leg in the same map.
	openState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "short", HedgeFor: "ETH"},
		}},
	}}
	err := validateHotReloadStateCompatible(oldCfg, newCfg, openState)
	if err == nil || !strings.Contains(err.Error(), "hedge shape changed with open positions") {
		t.Fatalf("expected hedge-shape-change block while hedge leg open, got: %v", err)
	}

	// Flat → the same edit is accepted.
	flatState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}
	if err := validateHotReloadStateCompatible(oldCfg, newCfg, flatState); err != nil {
		t.Fatalf("flat: hedge edit must be accepted, got: %v", err)
	}
}

func TestHedgeHotReloadEnableDisableBlockedWhileOpen(t *testing.T) {
	oldCfg := minimalReloadConfig([]StrategyConfig{hedgeReloadStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC"})})
	newCfg := minimalReloadConfig([]StrategyConfig{hedgeReloadStrategy(nil)})
	openState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"},
			"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "short", HedgeFor: "ETH"},
		}},
	}}
	err := validateHotReloadStateCompatible(oldCfg, newCfg, openState)
	if err == nil || !strings.Contains(err.Error(), "hedge shape changed") {
		t.Fatalf("expected disable-while-open block, got: %v", err)
	}
}

func TestHedgeRestartShapeMasksHedge(t *testing.T) {
	// A hedge-only difference must NOT be flagged restart-required (the restart
	// shape masks Hedge, so the DeepEqual on the residual struct ignores it).
	a := strategyRestartShape(hedgeReloadStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0}))
	b := strategyRestartShape(hedgeReloadStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 3.0}))
	if a.Hedge != nil || b.Hedge != nil {
		t.Fatalf("restart shape must nil out Hedge; got a=%v b=%v", a.Hedge, b.Hedge)
	}
}

func TestHedgeConfigEqual(t *testing.T) {
	if !hedgeConfigEqual(nil, nil) {
		t.Error("nil==nil should be equal")
	}
	h := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0}
	if hedgeConfigEqual(h, nil) {
		t.Error("non-nil vs nil should differ")
	}
	if !hedgeConfigEqual(h, &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0}) {
		t.Error("identical blocks should be equal")
	}
	if hedgeConfigEqual(h, &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2.0}) {
		t.Error("differing ratio should not be equal")
	}
}

func TestHedgePositionDBRoundTrip(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID:        "hl-eth",
				Type:      "perps",
				Platform:  "hyperliquid",
				Cash:      10000,
				RiskState: newRiskState(todayUTC(), 0),
				Positions: map[string]*Position{
					"BTC": {
						Symbol: "BTC", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 40000,
						Side: "short", Multiplier: 1, OwnerStrategyID: "hl-eth",
						TradePositionID: "hp-1", HedgeFor: "ETH", HedgePrimaryQtyBasis: 12.5,
					},
				},
				OptionPositions: map[string]*OptionPosition{},
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
	pos := loaded.Strategies["hl-eth"].Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge position not loaded")
	}
	if pos.HedgeFor != "ETH" {
		t.Errorf("HedgeFor round-trip: want ETH, got %q", pos.HedgeFor)
	}
	if pos.HedgePrimaryQtyBasis != 12.5 {
		t.Errorf("HedgePrimaryQtyBasis round-trip: want 12.5, got %g", pos.HedgePrimaryQtyBasis)
	}
}
