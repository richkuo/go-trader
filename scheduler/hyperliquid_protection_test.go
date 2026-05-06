package main

import (
	"testing"
)

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

// TestApplyHyperliquidProtectionSyncPreservesExistingOIDs verifies the
// "OID still resting" branch of run_sync_protection: when the result echoes
// the existing OID back (via open_orders verification), pos.TP1OID/TP2OID
// must remain set. A bug where the apply path overwrote with 0 would lose
// the OID and trigger a duplicate-place on the next cycle.
func TestApplyHyperliquidProtectionSyncPreservesExistingOIDs(t *testing.T) {
	pos := &Position{
		Symbol:      "ETH",
		StopLossOID: 100,
		TP1OID:      200,
		TP2OID:      300,
	}
	result := &HyperliquidProtectionSyncResult{
		StopLossOID:       100,
		StopLossTriggerPx: 2900,
		TP1OID:            200,
		TP2OID:            300,
	}
	applyHyperliquidProtectionSync(pos, result)
	if pos.StopLossOID != 100 || pos.TP1OID != 200 || pos.TP2OID != 300 {
		t.Errorf("OIDs mutated: SL=%d TP1=%d TP2=%d, want 100/200/300",
			pos.StopLossOID, pos.TP1OID, pos.TP2OID)
	}
	if pos.StopLossTriggerPx != 2900 {
		t.Errorf("StopLossTriggerPx = %g, want 2900", pos.StopLossTriggerPx)
	}
}

// TestApplyHyperliquidProtectionSyncRetainsOnZeroFields covers the case
// where the Python side couldn't fetch open_orders (so it omits OID fields
// from the result) — pos.TP1OID/TP2OID must NOT be cleared, otherwise the
// next cycle would re-place against an OID that's still resting.
func TestApplyHyperliquidProtectionSyncRetainsOnZeroFields(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 11, TP1OID: 22, TP2OID: 33}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		OpenOrderCheckError: "indexer down",
	})
	if pos.StopLossOID != 11 || pos.TP1OID != 22 || pos.TP2OID != 33 {
		t.Errorf("zero-field result mutated OIDs: SL=%d TP1=%d TP2=%d, want 11/22/33",
			pos.StopLossOID, pos.TP1OID, pos.TP2OID)
	}
}

// TestApplyHyperliquidProtectionSyncClearsFilledExternally is the over-close
// guard: when the Python side detected the OID actually filled on-chain
// (via userFills), the apply path must clear pos.TP1OID so the next cycle
// does not re-place against stale virtual qty (#604 review #1).
func TestApplyHyperliquidProtectionSyncClearsFilledExternally(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 11, TP1OID: 22, TP2OID: 33}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		StopLossFilledExternally: true,
		TP1FilledExternally:      true,
		// TP2 still resting in this scenario.
		TP2OID: 33,
	})
	if pos.StopLossOID != 0 {
		t.Errorf("StopLossOID = %d, want 0 (cleared because filled externally)", pos.StopLossOID)
	}
	if pos.TP1OID != 0 {
		t.Errorf("TP1OID = %d, want 0 (cleared because filled externally)", pos.TP1OID)
	}
	if pos.TP2OID != 33 {
		t.Errorf("TP2OID = %d, want 33 (still resting)", pos.TP2OID)
	}
}

func TestFilterCloseStrategiesForHLOnChainProtection(t *testing.T) {
	mult := 1.0
	cases := []struct {
		name     string
		sc       StrategyConfig
		expected []string
	}{
		{
			name: "tiered_tp_atr_live filtered when TP plan emitted",
			sc: StrategyConfig{
				Type:            "perps",
				Platform:        "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategies: []string{"tiered_tp_atr_live", "tp_at_pct"},
			},
			expected: []string{"tp_at_pct"},
		},
		{
			name: "no filter when no on-chain TPs (no tiered close strategy)",
			sc: StrategyConfig{
				Type:            "perps",
				Platform:        "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategies: []string{"tp_at_pct"},
			},
			expected: []string{"tp_at_pct"},
		},
		{
			name: "non-perps untouched",
			sc: StrategyConfig{
				Type:            "spot",
				Platform:        "hyperliquid",
				CloseStrategies: []string{"tiered_tp_atr_live"},
			},
			expected: []string{"tiered_tp_atr_live"},
		},
		{
			name: "tiered_tp_atr also filtered",
			sc: StrategyConfig{
				Type:            "perps",
				Platform:        "hyperliquid",
				StopLossATRMult: &mult,
				CloseStrategies: []string{"tiered_tp_atr"},
			},
			expected: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterCloseStrategiesForHLOnChainProtection(tc.sc)
			if len(got) != len(tc.expected) {
				t.Fatalf("filtered = %v, want %v", got, tc.expected)
			}
			for i, name := range got {
				if name != tc.expected[i] {
					t.Errorf("filtered[%d] = %q, want %q", i, name, tc.expected[i])
				}
			}
		})
	}
}

// TestCloseStrategiesSuppressedMatchesTieredTPATRClose enforces that
// closeStrategiesSuppressedByOnChainProtection and strategyUsesTieredTPATRClose
// stay in sync. If a new ATR-tiered close evaluator is added to the suppression
// set without updating strategyUsesTieredTPATRClose (or vice versa), this test
// fails immediately.
func TestCloseStrategiesSuppressedMatchesTieredTPATRClose(t *testing.T) {
	// Every name in the suppression set must be recognized by strategyUsesTieredTPATRClose.
	for name := range closeStrategiesSuppressedByOnChainProtection {
		sc := StrategyConfig{CloseStrategies: []string{name}}
		if !strategyUsesTieredTPATRClose(sc) {
			t.Errorf("strategyUsesTieredTPATRClose returned false for %q, which is in closeStrategiesSuppressedByOnChainProtection — add it to strategyUsesTieredTPATRClose", name)
		}
	}

	// A config with only non-suppressed close strategies must return false.
	sc := StrategyConfig{CloseStrategies: []string{"tp_at_pct", "tiered_tp_pct"}}
	if strategyUsesTieredTPATRClose(sc) {
		t.Error("strategyUsesTieredTPATRClose returned true for non-suppressed close strategies")
	}
}

func TestFloatFromAnyCheckedRejectsStrings(t *testing.T) {
	if _, err := floatFromAnyChecked("1.5"); err == nil {
		t.Error("expected error for string input, got nil")
	}
	if _, err := floatFromAnyChecked(nil); err == nil {
		t.Error("expected error for nil input, got nil")
	}
	if _, err := floatFromAnyChecked(true); err == nil {
		t.Error("expected error for bool input, got nil")
	}
	if v, err := floatFromAnyChecked(1.5); err != nil || v != 1.5 {
		t.Errorf("float64 1.5: got (%g, %v), want (1.5, nil)", v, err)
	}
	if v, err := floatFromAnyChecked(2); err != nil || v != 2 {
		t.Errorf("int 2: got (%g, %v), want (2, nil)", v, err)
	}
}

func TestParseHLProtectionTiersSkipsInvalidValues(t *testing.T) {
	raw := []interface{}{
		map[string]interface{}{"atr_multiple": "1.5", "close_fraction": 0.5}, // string rejected
		map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
	}
	tiers := parseHLProtectionTiers(raw)
	if len(tiers) != 1 {
		t.Fatalf("len(tiers) = %d, want 1 (string-typed tier should be skipped)", len(tiers))
	}
	if tiers[0].Multiple != 2 || tiers[0].Fraction != 1 {
		t.Errorf("surviving tier = (%g, %g), want (2, 1)", tiers[0].Multiple, tiers[0].Fraction)
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
