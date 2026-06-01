package main

import (
	"reflect"
	"strings"
	"testing"
)

func unifiedBlock() map[string]interface{} {
	return map[string]interface{}{
		"atr_source": "live",
		regimeClassifierKey: map[string]interface{}{
			"trending_up": map[string]interface{}{
				"stop_loss_atr": 1.5,
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5,
						"sl_after": map[string]interface{}{"kind": "trail_from_here", "tp_atr_fraction": 0.5}},
					map[string]interface{}{"atr_multiple": 4.0, "close_fraction": 1.0},
				},
			},
			// Different tier count per regime is allowed under select-then-scalar.
			"trending_down": map[string]interface{}{
				"stop_loss_atr": 1.0,
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.5, "close_fraction": 1.0},
				},
			},
			"ranging": map[string]interface{}{
				"stop_loss_atr": 0.8,
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
				},
			},
		},
	}
}

func TestCloseParamsAreUnifiedRegime(t *testing.T) {
	if !closeParamsAreUnifiedRegime(unifiedBlock()) {
		t.Fatal("unified block not detected")
	}
	legacy := map[string]interface{}{"tp_tiers": []interface{}{}}
	if closeParamsAreUnifiedRegime(legacy) {
		t.Fatal("legacy tier-keyed params misdetected as unified")
	}
	if closeParamsAreUnifiedRegime(nil) {
		t.Fatal("nil params misdetected as unified")
	}
}

func TestUnifiedRegimeScalarParams(t *testing.T) {
	scalar, sl, ok := unifiedRegimeScalarParams(unifiedBlock(), "trending_up")
	if !ok {
		t.Fatal("expected ok for trending_up")
	}
	if sl != 1.5 {
		t.Fatalf("stop_loss_atr = %g, want 1.5", sl)
	}
	if scalar["atr_source"] != "live" {
		t.Fatalf("atr_source not carried: %v", scalar["atr_source"])
	}
	tiers, ok := scalar["tp_tiers"].([]interface{})
	if !ok || len(tiers) != 2 {
		t.Fatalf("tp_tiers = %v, want 2-tier list", scalar["tp_tiers"])
	}

	// Variable tier count: trending_down has a single tier.
	scalarDown, _, ok := unifiedRegimeScalarParams(unifiedBlock(), "trending_down")
	if !ok {
		t.Fatal("expected ok for trending_down")
	}
	if td := scalarDown["tp_tiers"].([]interface{}); len(td) != 1 {
		t.Fatalf("trending_down tp_tiers len = %d, want 1", len(td))
	}

	// Unknown label → miss (caller falls back).
	if _, _, ok := unifiedRegimeScalarParams(unifiedBlock(), "nonsense"); ok {
		t.Fatal("expected miss for unknown regime label")
	}
}

func TestValidateUnifiedRegimeClose_Valid(t *testing.T) {
	labels := []string{"trending_up", "trending_down", "ranging"}
	if errs := validateUnifiedRegimeClose(unifiedBlock(), labels, "close.params"); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestValidateUnifiedRegimeClose_Errors(t *testing.T) {
	labels := []string{"trending_up", "trending_down", "ranging"}

	cases := []struct {
		name    string
		mutate  func(m map[string]interface{})
		wantSub string
	}{
		{"missing label", func(m map[string]interface{}) {
			delete(m[regimeClassifierKey].(map[string]interface{}), "ranging")
		}, "missing required regime label"},
		{"unknown label", func(m map[string]interface{}) {
			m[regimeClassifierKey].(map[string]interface{})["weird"] = map[string]interface{}{
				"tp_tiers": []interface{}{map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 1.0}}}
		}, "unknown regime label"},
		{"bad close_fraction", func(m map[string]interface{}) {
			tier := m[regimeClassifierKey].(map[string]interface{})["ranging"].(map[string]interface{})["tp_tiers"].([]interface{})[0].(map[string]interface{})
			tier["close_fraction"] = 1.5
		}, "close_fraction: must be in (0, 1]"},
		{"bad stop_loss_atr", func(m map[string]interface{}) {
			m[regimeClassifierKey].(map[string]interface{})["ranging"].(map[string]interface{})["stop_loss_atr"] = -1.0
		}, "stop_loss_atr: must be > 0"},
		{"regime-keyed sl_after rejected", func(m map[string]interface{}) {
			tier := m[regimeClassifierKey].(map[string]interface{})["ranging"].(map[string]interface{})["tp_tiers"].([]interface{})[0].(map[string]interface{})
			tier["sl_after"] = map[string]interface{}{"kind": "trail_from_here",
				"tp_atr_fraction": map[string]interface{}{"trend_regime": map[string]interface{}{"ranging": 0.5}}}
		}, "must be scalar in a unified per-regime block"},
		{"unknown tier key", func(m map[string]interface{}) {
			tier := m[regimeClassifierKey].(map[string]interface{})["ranging"].(map[string]interface{})["tp_tiers"].([]interface{})[0].(map[string]interface{})
			tier["bogus"] = 1
		}, "unknown key \"bogus\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := unifiedBlock()
			tc.mutate(m)
			errs := validateUnifiedRegimeClose(m, labels, "close.params")
			joined := strings.Join(errs, " | ")
			if !strings.Contains(joined, tc.wantSub) {
				t.Fatalf("errors %q do not contain %q", joined, tc.wantSub)
			}
		})
	}
}

func TestUnifiedRegimeScalarParams_ShapeMatchesScalarConfig(t *testing.T) {
	// The selected scalar params must be exactly the shape the scalar
	// tiered_tp_atr machinery consumes: {"tp_tiers": [...], "atr_source": ...}.
	scalar, _, _ := unifiedRegimeScalarParams(unifiedBlock(), "ranging")
	want := map[string]interface{}{
		"atr_source": "live",
		"tp_tiers": []interface{}{
			map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
		},
	}
	if !reflect.DeepEqual(scalar, want) {
		t.Fatalf("scalar = %v, want %v", scalar, want)
	}
}

// TestUnifiedRegimeSLFolding verifies #841 2b SL folding: the on-chain
// protection plan resolves the per-regime stop_loss_atr from the unified close
// block, and EffectiveStopLossPct defers (returns 0) instead of using the
// max-drawdown fallback.
func TestUnifiedRegimeSLFolding(t *testing.T) {
	tiers := func(a, b float64) []interface{} {
		return []interface{}{
			map[string]interface{}{"atr_multiple": a, "close_fraction": 0.5},
			map[string]interface{}{"atr_multiple": b, "close_fraction": 1.0},
		}
	}
	sc := StrategyConfig{
		ID: "hl-unified-sl", Platform: "hyperliquid", Type: "perps",
		MaxDrawdownPct: 25,
		CloseStrategies: []StrategyRef{{
			Name: "tiered_tp_atr_live_regime",
			Params: map[string]interface{}{
				regimeClassifierKey: map[string]interface{}{
					"trending_up":   map[string]interface{}{"stop_loss_atr": 1.5, "tp_tiers": tiers(2.0, 4.0)},
					"trending_down": map[string]interface{}{"stop_loss_atr": 1.2, "tp_tiers": tiers(1.8, 3.0)},
					"ranging":       map[string]interface{}{"stop_loss_atr": 0.8, "tp_tiers": tiers(1.0, 2.0)},
				},
			},
		}},
	}

	if !strategyUsesUnifiedRegimeClose(sc) {
		t.Fatal("strategyUsesUnifiedRegimeClose = false, want true")
	}
	// EffectiveStopLossPct must defer (0), not fall through to MaxDrawdownPct.
	if got := EffectiveStopLossPct(sc); got != 0 {
		t.Fatalf("EffectiveStopLossPct = %g, want 0 (deferred, not max-drawdown fallback)", got)
	}

	mkPos := func(regime string) *Position {
		return &Position{Symbol: "ETH", Quantity: 1, AvgCost: 100, EntryATR: 5, Side: "long", Regime: regime}
	}
	for _, tc := range []struct {
		regime string
		wantSL float64
	}{{"trending_up", 1.5}, {"ranging", 0.8}} {
		plan, ok := buildHyperliquidProtectionPlan(sc, mkPos(tc.regime))
		if !ok {
			t.Fatalf("%s: protection plan not built", tc.regime)
		}
		if plan.StopLossATRMult != tc.wantSL {
			t.Fatalf("%s: plan.StopLossATRMult = %g, want %g", tc.regime, plan.StopLossATRMult, tc.wantSL)
		}
	}
}
