package main

import (
	"testing"
)

func TestStrategyCurrentATRRegime(t *testing.T) {
	sc := StrategyConfig{RegimeATRWindow: "medium"}
	st := &StrategyState{
		RegimeWindows: map[string]string{"medium": "ranging_quiet"},
	}
	if got := strategyCurrentATRRegime(st, sc); got != "ranging_quiet" {
		t.Fatalf("strategyCurrentATRRegime = %q, want ranging_quiet", got)
	}
}

func TestAdvanceDynamicCloseRegime_ConfirmCycles(t *testing.T) {
	sc := StrategyConfig{
		CloseStrategy: &StrategyRef{
			Name: dynamicCloseStrategyName,
			Params: map[string]interface{}{
				"regime_confirm_cycles": 2,
				"trend_regime": map[string]interface{}{
					"trending_up": map[string]interface{}{
						"stop_loss_atr": 1.5,
						"tp_tiers": []interface{}{
							map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
							map[string]interface{}{"atr_multiple": 4.0, "close_fraction": 1.0},
						},
					},
					"ranging": map[string]interface{}{
						"stop_loss_atr": 0.8,
						"tp_tiers": []interface{}{
							map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
							map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
						},
					},
				},
			},
		},
	}
	pos := &Position{RegimeAppliedLabel: "trending_up"}
	st := &StrategyState{
		Regime:        "ranging",
		RegimeWindows: map[string]string{"medium": "ranging"},
	}
	sc.RegimeATRWindow = "medium"

	if changed := advanceDynamicCloseRegime(pos, st, sc); changed {
		t.Fatal("first pending cycle should not apply")
	}
	if pos.RegimeAppliedLabel != "trending_up" {
		t.Fatalf("applied=%q want trending_up", pos.RegimeAppliedLabel)
	}
	if changed := advanceDynamicCloseRegime(pos, st, sc); !changed {
		t.Fatal("second pending cycle should apply ranging")
	}
	if pos.RegimeAppliedLabel != "ranging" {
		t.Fatalf("applied=%q want ranging", pos.RegimeAppliedLabel)
	}
}

func TestTriggerPxMoveExceedsMinPct(t *testing.T) {
	if !triggerPxMoveExceedsMinPct(100, 100.6, 0.5) {
		t.Fatal("0.6% move should exceed 0.5% threshold")
	}
	if triggerPxMoveExceedsMinPct(100, 100.4, 0.5) {
		t.Fatal("0.4% move should not exceed 0.5% threshold")
	}
}

func dynamicCloseTestStrategy() StrategyConfig {
	return StrategyConfig{
		CloseStrategy: &StrategyRef{
			Name:   dynamicCloseStrategyName,
			Params: unifiedBlock(),
		},
	}
}

func TestStrategyUsesRegimeTieredTPATRClose_IncludesDynamic(t *testing.T) {
	sc := dynamicCloseTestStrategy()
	if !strategyUsesRegimeTieredTPATRClose(sc) {
		t.Fatal("strategyUsesRegimeTieredTPATRClose must include tiered_tp_atr_live_regime_dynamic")
	}
}

func TestDynamicProtectionSurplusTPOIDs(t *testing.T) {
	got := dynamicProtectionSurplusTPOIDs([]int64{10, 20, 30}, 2)
	if len(got) != 1 || got[0] != 30 {
		t.Fatalf("surplus OIDs = %v, want [30]", got)
	}
	if surplus := dynamicProtectionSurplusTPOIDs([]int64{10, 0, 30}, 2); len(surplus) != 1 || surplus[0] != 30 {
		t.Fatalf("skip zero OID in surplus: %v", surplus)
	}
	if surplus := dynamicProtectionSurplusTPOIDs([]int64{10, 20}, 2); len(surplus) != 0 {
		t.Fatalf("no shrink: got %v", surplus)
	}
}

func TestDynamicProtectionForceReplace(t *testing.T) {
	sc := dynamicCloseTestStrategy()
	plan := hlProtectionPlan{
		Side:            "long",
		AvgCost:         100,
		EntryATR:        10,
		StopLossATRMult: 0.8,
		Tiers: []hlProtectionTier{
			{Multiple: 1.0, Fraction: 0.5},
			{Multiple: 2.0, Fraction: 1.0},
		},
	}

	t.Run("no_change", func(t *testing.T) {
		pos := &Position{TPOIDs: []int64{1, 2}}
		forceSL, forceTP := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", false)
		if forceSL || len(forceTP) > 0 {
			t.Fatalf("regimeChanged=false: forceSL=%v forceTP=%v", forceSL, forceTP)
		}
	})

	t.Run("sl_moves_past_debounce", func(t *testing.T) {
		pos := &Position{StopLossOID: 99, StopLossTriggerPx: 85}
		forceSL, _ := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", true)
		if !forceSL {
			t.Fatal("expected SL force-replace when regime SL mult changes beyond debounce")
		}
	})

	t.Run("filled_tier_skipped", func(t *testing.T) {
		pos := &Position{
			TPOIDs:       []int64{0, 2},
			TPArmedTiers: []bool{true, false},
		}
		_, forceTP := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", true)
		if len(forceTP) != 2 {
			t.Fatalf("forceTP len=%d want 2", len(forceTP))
		}
		if forceTP[0] {
			t.Fatal("filled tier (armed, OID=0) must not force-replace")
		}
	})

	t.Run("never_armed_skipped", func(t *testing.T) {
		pos := &Position{TPOIDs: []int64{0, 0}}
		_, forceTP := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", true)
		if forceTP[0] || forceTP[1] {
			t.Fatalf("never-armed tiers should skip force-replace: %v", forceTP)
		}
	})

	t.Run("tier_count_shrink_surplus", func(t *testing.T) {
		pos := &Position{TPOIDs: []int64{10, 20, 303}}
		surplus := dynamicProtectionSurplusTPOIDs(pos.TPOIDs, 2)
		if len(surplus) != 1 || surplus[0] != 303 {
			t.Fatalf("tier shrink surplus = %v, want [303]", surplus)
		}
	})

	t.Run("resting_tier_replaces_on_move", func(t *testing.T) {
		pos := &Position{
			TPOIDs:       []int64{101, 0},
			TPArmedTiers: []bool{true, false},
		}
		_, forceTP := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", true)
		if !forceTP[0] {
			t.Fatal("resting tier with regime-driven price move should force-replace")
		}
		if forceTP[1] {
			t.Fatal("tier 1 never armed — sync places fresh without cancel")
		}
	})
}

func TestDynamicCloseInSuppressionSet(t *testing.T) {
	sc := StrategyConfig{
		Type:          "perps",
		Platform:      "hyperliquid",
		Args:          []string{"hold", "ETH", "1h", "--mode=live"},
		CloseStrategy: &StrategyRef{Name: dynamicCloseStrategyName},
	}
	if !strategyUsesTieredTPATRClose(sc) {
		t.Fatal("dynamic close must be recognized by strategyUsesTieredTPATRClose")
	}
	if !closeStrategySuppressedByOnChainProtection(sc) {
		t.Fatal("dynamic close must be suppressed on HL live when on-chain TPs active")
	}
}
