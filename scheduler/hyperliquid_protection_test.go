package main

import "testing"

func TestBuildHyperliquidProtectionPlanUsesDefaultTieredATR(t *testing.T) {
	mult := 1.0
	sc := StrategyConfig{
		ID:              "hl-eth",
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &mult,
		CloseStrategies: []string{"tiered_tp_atr_live"},
	}
	pos := &Position{
		Symbol:   "ETH",
		Quantity: 2,
		AvgCost:  3000,
		EntryATR: 50,
		Side:     "long",
		TP1OID:   101,
		TP2OID:   202,
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false")
	}
	if plan.StopLossATRMult != 1 {
		t.Errorf("StopLossATRMult = %g, want 1", plan.StopLossATRMult)
	}
	if plan.TP1Mult != 1 || plan.TP1Fraction != 0.5 || plan.TP2Mult != 2 {
		t.Errorf("tiers = (%g, %g, %g), want (1, 0.5, 2)", plan.TP1Mult, plan.TP1Fraction, plan.TP2Mult)
	}
	if plan.TP1OID != 101 || plan.TP2OID != 202 {
		t.Errorf("TP OIDs = (%d, %d), want (101, 202)", plan.TP1OID, plan.TP2OID)
	}
}

func TestBuildHyperliquidProtectionPlanCustomTiers(t *testing.T) {
	mult := 1.25
	sc := StrategyConfig{
		Type:            "perps",
		Platform:        "hyperliquid",
		StopLossATRMult: &mult,
		CloseStrategies: []string{"tiered_tp_atr_live"},
		Params: map[string]interface{}{
			"tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.4},
			},
		},
	}
	pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2500, EntryATR: 25, Side: "short"}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false")
	}
	if plan.TP1Mult != 2 || plan.TP1Fraction != 0.4 || plan.TP2Mult != 3 {
		t.Errorf("custom tiers = (%g, %g, %g), want (2, 0.4, 3)", plan.TP1Mult, plan.TP1Fraction, plan.TP2Mult)
	}
}
