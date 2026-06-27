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
			// Different tier count per regime is allowed under select-then-scalar
			// (>=2 required to match the on-chain resolver): 3 tiers here vs 2.
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

	// Variable tier count: trending_down has 3 tiers vs trending_up's 2.
	scalarDown, _, ok := unifiedRegimeScalarParams(unifiedBlock(), "trending_down")
	if !ok {
		t.Fatal("expected ok for trending_down")
	}
	if td := scalarDown["tp_tiers"].([]interface{}); len(td) != 3 {
		t.Fatalf("trending_down tp_tiers len = %d, want 3", len(td))
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
	// The selected scalar params must be exactly the shape the scalar
	// tiered_tp_atr machinery consumes: {"tp_tiers": [...], "atr_source": ...}.
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

// TestValidateRegimeATRConfig_UnifiedBlockAccepted verifies the #841 2b gate:
// a unified per-regime close config validates (no longer rejected as "missing
// tiers"), and a malformed one surfaces the unified validation error.
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

	// Drop a required label → unified validator should fire, not "missing tiers".
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

// TestValidateUnifiedCloseSoleOwner verifies #841 2b sole-owner enforcement: a
// unified per-regime close may not coexist with a strategy-level stop field.
func TestValidateUnifiedCloseSoleOwner(t *testing.T) {
	mk := func() StrategyConfig {
		return StrategyConfig{
			ID: "hl-x", Type: "perps", Platform: "hyperliquid",
			CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live_regime", Params: unifiedBlock()},
		}
	}
	// Clean: no strategy-level stop → no errors.
	if errs := validateUnifiedCloseSoleOwner(mk(), "s"); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	// Conflict: strategy-level stop_loss_atr_mult set → rejected.
	sc := mk()
	m := 1.5
	sc.StopLossATRMult = &m
	errs := validateUnifiedCloseSoleOwner(sc, "s")
	if len(errs) == 0 || !strings.Contains(errs[0], "stop_loss_atr_mult is not allowed alongside a unified per-regime close") {
		t.Fatalf("expected sole-owner rejection, got: %v", errs)
	}
	// Non-unified strategy → helper is a no-op.
	plain := StrategyConfig{ID: "p", Type: "perps", Platform: "hyperliquid", StopLossATRMult: &m}
	if errs := validateUnifiedCloseSoleOwner(plain, "p"); len(errs) > 0 {
		t.Fatalf("non-unified strategy should not trip sole-owner: %v", errs)
	}
}

// TestUnifiedCloseParamsEqualForReload verifies #841 2b hot-reload gating: a
// changed (or added/removed) unified per-regime close block is detected so the
// reload validator can reject it while a position is open.
func TestUnifiedCloseParamsEqualForReload(t *testing.T) {
	mk := func(params map[string]interface{}) StrategyConfig {
		return StrategyConfig{CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live_regime", Params: params}}
	}
	a := mk(unifiedBlock())
	if !unifiedCloseParamsEqualForReload(a, mk(unifiedBlock())) {
		t.Fatal("identical unified blocks should compare equal")
	}
	// Change a tier multiple → not equal.
	changed := unifiedBlock()
	changed[regimeClassifierKey].(map[string]interface{})["ranging"].(map[string]interface{})["tp_tiers"].([]interface{})[0].(map[string]interface{})["atr_multiple"] = 9.9
	if unifiedCloseParamsEqualForReload(a, mk(changed)) {
		t.Fatal("changed tier multiple should compare unequal")
	}
	// Remove the unified close entirely → not equal.
	if unifiedCloseParamsEqualForReload(a, StrategyConfig{}) {
		t.Fatal("removing the unified close should compare unequal")
	}
	// Two non-unified strategies → both nil → equal (no false positive).
	if !unifiedCloseParamsEqualForReload(StrategyConfig{}, StrategyConfig{}) {
		t.Fatal("two non-unified strategies should compare equal")
	}
}

// unifiedCompositeBlock is a 9-label composite unified block keyed on bare
// ranging_directional (no _up/_down keys) — the pre-#1124 shape that the
// #1124 family rule must keep valid and keep resolving for _up/_down stamps.
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

// TestValidateUnifiedRegimeClose_CompositeBareDirectionalCoversSubLabels:
// the #1124 family rule — a bare ranging_directional covers its _up/_down
// sub-labels for exhaustiveness, so the pre-#1124 bare-only shape still loads
// under the 9-label composite vocabulary.
func TestValidateUnifiedRegimeClose_CompositeBareDirectionalCoversSubLabels(t *testing.T) {
	labels := regimeLabelsForClassifier(regimeClassifierComposite)
	if errs := validateUnifiedRegimeClose(unifiedCompositeBlock(), labels, "close.params"); len(errs) > 0 {
		t.Fatalf("bare-only composite block keyed on ranging_directional rejected: %v", errs)
	}
}

// TestValidateUnifiedRegimeClose_CompositeSubLabelsWithoutBareRejected:
// sub-labels-only (no bare parent) is still non-exhaustive because the
// producer emits bare ranging_directional at exactly return_eff == 0. The
// rule is one-directional: subs never satisfy the bare requirement.
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

// TestUnifiedRegimeScalarParams_SubLabelFallsBackToBare: a _up/_down stamp
// resolves to the bare ranging_directional block at runtime, returning its
// SL and TP ladder (the sole-owner safety guarantee — no naked position). An
// explicit sub-label key wins over bare on exact match, while the sibling in
// the same block still falls back to bare (one-directional rule).
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

	// Explicit _up key wins over bare on exact match; _down still falls back.
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

// TestUnifiedRegimeSLFolding_SubLabelStampPlacesSL: end-to-end #1124 safety —
// a composite unified close keyed on bare ranging_directional (the sole SL
// owner) places an SL via the on-chain protection plan when the producer
// stamps _up/_down. Without the bare fallback this would be a naked position.
func TestUnifiedRegimeSLFolding_SubLabelStampPlacesSL(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-unified-sub", Platform: "hyperliquid", Type: "perps",
		MaxDrawdownPct: 25,
		CloseStrategy:  &StrategyRef{Name: "tiered_tp_atr_live_regime", Params: unifiedCompositeBlock()},
	}
	for _, stamp := range []string{"ranging_directional_up", "ranging_directional_down"} {
		pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 100, EntryATR: 5, Side: "long", Regime: stamp}
		plan, ok := buildHyperliquidProtectionPlan(sc, pos)
		if !ok {
			t.Fatalf("%s: protection plan not built (naked position)", stamp)
		}
		if plan.StopLossATRMult != 1.1 {
			t.Fatalf("%s: plan.StopLossATRMult = %g, want 1.1 (bare fallback)", stamp, plan.StopLossATRMult)
		}
	}
}
