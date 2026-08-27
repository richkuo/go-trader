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
			"trending_down": map[string]interface{}{
				"stop_loss_atr": 1.0,
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.5, "close_fraction": 0.4},
					map[string]interface{}{"atr_multiple": 2.5, "close_fraction": 0.7},
					map[string]interface{}{"atr_multiple": 3.5, "close_fraction": 1.0},
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

	scalarDown, _, ok := unifiedRegimeScalarParams(unifiedBlock(), "trending_down")
	if !ok {
		t.Fatal("expected ok for trending_down")
	}
	if td := scalarDown["tp_tiers"].([]interface{}); len(td) != 3 {
		t.Fatalf("trending_down tp_tiers len = %d, want 3", len(td))
	}

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
		{"missing stop_loss_atr", func(m map[string]interface{}) {
			delete(m[regimeClassifierKey].(map[string]interface{})["ranging"].(map[string]interface{}), "stop_loss_atr")
		}, "missing required \"stop_loss_atr\""},
		{"single tier rejected", func(m map[string]interface{}) {
			rng := m[regimeClassifierKey].(map[string]interface{})["ranging"].(map[string]interface{})
			rng["tp_tiers"] = []interface{}{map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 1.0}}
		}, "must have at least 2 tiers"},
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
	scalar, _, _ := unifiedRegimeScalarParams(unifiedBlock(), "ranging")
	want := map[string]interface{}{
		"atr_source": "live",
		"tp_tiers": []interface{}{
			map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
			map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
		},
	}
	if !reflect.DeepEqual(scalar, want) {
		t.Fatalf("scalar = %v, want %v", scalar, want)
	}
}

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
		CloseStrategy: &StrategyRef{
			Name: "tiered_tp_atr_live_regime",
			Params: map[string]interface{}{
				regimeClassifierKey: map[string]interface{}{
					"trending_up":   map[string]interface{}{"stop_loss_atr": 1.5, "tp_tiers": tiers(2.0, 4.0)},
					"trending_down": map[string]interface{}{"stop_loss_atr": 1.2, "tp_tiers": tiers(1.8, 3.0)},
					"ranging":       map[string]interface{}{"stop_loss_atr": 0.8, "tp_tiers": tiers(1.0, 2.0)},
				},
			},
		},
	}

	if !strategyUsesUnifiedRegimeClose(sc) {
		t.Fatal("strategyUsesUnifiedRegimeClose = false, want true")
	}
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
		plan, ok := buildHyperliquidProtectionPlan(sc, mkPos(tc.regime), 0)
		if !ok {
			t.Fatalf("%s: protection plan not built", tc.regime)
		}
		if plan.StopLossATRMult != tc.wantSL {
			t.Fatalf("%s: plan.StopLossATRMult = %g, want %g", tc.regime, plan.StopLossATRMult, tc.wantSL)
		}
	}
}

func TestValidateRegimeATRConfig_UnifiedBlockAccepted(t *testing.T) {
	mkCfg := func(params map[string]interface{}) *Config {
		return &Config{
			Regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20},
			Strategies: []StrategyConfig{{
				ID:       "hl-unified",
				Type:     "perps",
				Platform: "hyperliquid",
				CloseStrategy: &StrategyRef{
					Name:   "tiered_tp_atr_live_regime",
					Params: params,
				},
			}},
		}
	}

	valid := mkCfg(unifiedBlock())
	if errs := validateRegimeATRConfig(valid); len(errs) > 0 {
		t.Fatalf("valid unified config rejected: %v", errs)
	}

	bad := unifiedBlock()
	delete(bad[regimeClassifierKey].(map[string]interface{}), "ranging")
	errs := validateRegimeATRConfig(mkCfg(bad))
	joined := strings.Join(errs, " | ")
	if !strings.Contains(joined, "missing required regime label") {
		t.Fatalf("expected unified exhaustiveness error, got: %v", errs)
	}
	if strings.Contains(joined, "missing tiers") {
		t.Fatalf("unified config hit the legacy tier-keyed path: %v", errs)
	}
}

func TestValidateUnifiedCloseSoleOwner(t *testing.T) {
	mk := func() StrategyConfig {
		return StrategyConfig{
			ID: "hl-x", Type: "perps", Platform: "hyperliquid",
			CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live_regime", Params: unifiedBlock()},
		}
	}
	if errs := validateUnifiedCloseSoleOwner(mk(), "s"); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	sc := mk()
	m := 1.5
	sc.StopLossATRMult = &m
	errs := validateUnifiedCloseSoleOwner(sc, "s")
	if len(errs) == 0 || !strings.Contains(errs[0], "stop_loss_atr_mult is not allowed alongside a unified per-regime close") {
		t.Fatalf("expected sole-owner rejection, got: %v", errs)
	}
	plain := StrategyConfig{ID: "p", Type: "perps", Platform: "hyperliquid", StopLossATRMult: &m}
	if errs := validateUnifiedCloseSoleOwner(plain, "p"); len(errs) > 0 {
		t.Fatalf("non-unified strategy should not trip sole-owner: %v", errs)
	}
}

func TestUnifiedCloseParamsEqualForReload(t *testing.T) {
	mk := func(params map[string]interface{}) StrategyConfig {
		return StrategyConfig{CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live_regime", Params: params}}
	}
	a := mk(unifiedBlock())
	if !unifiedCloseParamsEqualForReload(a, mk(unifiedBlock())) {
		t.Fatal("identical unified blocks should compare equal")
	}
	changed := unifiedBlock()
	changed[regimeClassifierKey].(map[string]interface{})["ranging"].(map[string]interface{})["tp_tiers"].([]interface{})[0].(map[string]interface{})["atr_multiple"] = 9.9
	if unifiedCloseParamsEqualForReload(a, mk(changed)) {
		t.Fatal("changed tier multiple should compare unequal")
	}
	if unifiedCloseParamsEqualForReload(a, StrategyConfig{}) {
		t.Fatal("removing the unified close should compare unequal")
	}
	if !unifiedCloseParamsEqualForReload(StrategyConfig{}, StrategyConfig{}) {
		t.Fatal("two non-unified strategies should compare equal")
	}
}

func unifiedCompositeBlock() map[string]interface{} {
	tiers := func(a, b float64) []interface{} {
		return []interface{}{
			map[string]interface{}{"atr_multiple": a, "close_fraction": 0.5},
			map[string]interface{}{"atr_multiple": b, "close_fraction": 1.0},
		}
	}
	return map[string]interface{}{
		regimeClassifierKey: map[string]interface{}{
			"trending_up_clean":    map[string]interface{}{"stop_loss_atr": 1.5, "tp_tiers": tiers(2.0, 4.0)},
			"trending_up_choppy":   map[string]interface{}{"stop_loss_atr": 1.3, "tp_tiers": tiers(1.8, 3.0)},
			"trending_down_clean":  map[string]interface{}{"stop_loss_atr": 1.5, "tp_tiers": tiers(2.0, 4.0)},
			"trending_down_choppy": map[string]interface{}{"stop_loss_atr": 1.3, "tp_tiers": tiers(1.8, 3.0)},
			"ranging_quiet":        map[string]interface{}{"stop_loss_atr": 1.0, "tp_tiers": tiers(1.2, 2.4)},
			"ranging_volatile":     map[string]interface{}{"stop_loss_atr": 1.2, "tp_tiers": tiers(1.5, 3.0)},
			"ranging_directional":  map[string]interface{}{"stop_loss_atr": 1.1, "tp_tiers": tiers(1.3, 2.6)},
		},
	}
}

func TestValidateUnifiedRegimeClose_CompositeBareDirectionalCoversSubLabels(t *testing.T) {
	labels := regimeLabelsForClassifier(regimeClassifierComposite)
	if errs := validateUnifiedRegimeClose(unifiedCompositeBlock(), labels, "close.params"); len(errs) > 0 {
		t.Fatalf("bare-only composite block keyed on ranging_directional rejected: %v", errs)
	}
}

func TestValidateUnifiedRegimeClose_CompositeSubLabelsWithoutBareRejected(t *testing.T) {
	labels := regimeLabelsForClassifier(regimeClassifierComposite)
	m := unifiedCompositeBlock()
	tr := m[regimeClassifierKey].(map[string]interface{})
	delete(tr, "ranging_directional")
	tr["ranging_directional_up"] = map[string]interface{}{"stop_loss_atr": 1.1, "tp_tiers": []interface{}{
		map[string]interface{}{"atr_multiple": 1.3, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 2.6, "close_fraction": 1.0},
	}}
	tr["ranging_directional_down"] = tr["ranging_directional_up"]
	errs := validateUnifiedRegimeClose(m, labels, "close.params")
	joined := strings.Join(errs, " | ")
	if !strings.Contains(joined, `missing required regime label "ranging_directional"`) {
		t.Fatalf("sub-labels-only block must be rejected as missing bare ranging_directional, got: %v", errs)
	}
}

func TestUnifiedRegimeScalarParams_SubLabelFallsBackToBare(t *testing.T) {
	for _, stamp := range []string{"ranging_directional_up", "ranging_directional_down"} {
		scalar, sl, ok := unifiedRegimeScalarParams(unifiedCompositeBlock(), stamp)
		if !ok {
			t.Fatalf("%s: expected ok via bare fallback", stamp)
		}
		if sl != 1.1 {
			t.Fatalf("%s: stop_loss_atr = %g, want 1.1 (bare)", stamp, sl)
		}
		if tiers, ok := scalar["tp_tiers"].([]interface{}); !ok || len(tiers) != 2 {
			t.Fatalf("%s: tp_tiers = %v, want bare 2-tier ladder", stamp, scalar["tp_tiers"])
		}
	}

	m := unifiedCompositeBlock()
	tr := m[regimeClassifierKey].(map[string]interface{})
	tr["ranging_directional_up"] = map[string]interface{}{"stop_loss_atr": 0.9, "tp_tiers": []interface{}{
		map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
		map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
	}}
	if _, sl, ok := unifiedRegimeScalarParams(m, "ranging_directional_up"); !ok || sl != 0.9 {
		t.Fatalf("explicit _up must win: stop_loss_atr = %g, ok = %v, want 0.9/true", sl, ok)
	}
	if _, sl, ok := unifiedRegimeScalarParams(m, "ranging_directional_down"); !ok || sl != 1.1 {
		t.Fatalf("_down must fall back to bare: stop_loss_atr = %g, ok = %v, want 1.1/true", sl, ok)
	}
}

func TestUnifiedRegimeSLFolding_SubLabelStampPlacesSL(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-unified-sub", Platform: "hyperliquid", Type: "perps",
		MaxDrawdownPct: 25,
		CloseStrategy:  &StrategyRef{Name: "tiered_tp_atr_live_regime", Params: unifiedCompositeBlock()},
	}
	for _, stamp := range []string{"ranging_directional_up", "ranging_directional_down"} {
		pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 100, EntryATR: 5, Side: "long", Regime: stamp}
		plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
		if !ok {
			t.Fatalf("%s: protection plan not built (naked position)", stamp)
		}
		if plan.StopLossATRMult != 1.1 {
			t.Fatalf("%s: plan.StopLossATRMult = %g, want 1.1 (bare fallback)", stamp, plan.StopLossATRMult)
		}
	}
}
