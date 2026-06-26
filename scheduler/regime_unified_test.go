package main

import (
	"os"
	"path/filepath"
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

func compositeUnifiedBlock(includeSplit bool) map[string]interface{} {
	tiers := []interface{}{
		map[string]interface{}{"atr_multiple": 1.5, "close_fraction": 0.4},
		map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
	}
	trend := map[string]interface{}{
		"trending_up_clean":    map[string]interface{}{"stop_loss_atr": 1.5, "tp_tiers": tiers},
		"trending_up_choppy":   map[string]interface{}{"stop_loss_atr": 1.2, "tp_tiers": tiers},
		"trending_down_clean":  map[string]interface{}{"stop_loss_atr": 1.5, "tp_tiers": tiers},
		"trending_down_choppy": map[string]interface{}{"stop_loss_atr": 1.2, "tp_tiers": tiers},
		"ranging_quiet":        map[string]interface{}{"stop_loss_atr": 0.9, "tp_tiers": tiers},
		"ranging_volatile":     map[string]interface{}{"stop_loss_atr": 0.8, "tp_tiers": tiers},
		"ranging_directional":  map[string]interface{}{"stop_loss_atr": 0.7, "tp_tiers": tiers},
	}
	if includeSplit {
		trend["ranging_directional_up"] = map[string]interface{}{"stop_loss_atr": 0.6, "tp_tiers": tiers}
		trend["ranging_directional_down"] = map[string]interface{}{"stop_loss_atr": 0.5, "tp_tiers": tiers}
	}
	return map[string]interface{}{regimeClassifierKey: trend}
}

func TestValidateUnifiedRegimeClose_CompositeRangingDirectionalFallback(t *testing.T) {
	labels := regimeLabelsForClassifier(regimeClassifierComposite)
	block := compositeUnifiedBlock(false)
	if errs := validateUnifiedRegimeClose(block, labels, "close.params"); len(errs) > 0 {
		t.Fatalf("bare ranging_directional must cover split labels, got: %v", errs)
	}
	trend := block[regimeClassifierKey].(map[string]interface{})
	if len(trend) != 7 {
		t.Fatalf("fallback validation must not expand stored keys, got %d", len(trend))
	}
	for _, label := range []string{"ranging_directional_up", "ranging_directional_down"} {
		scalar, sl, ok := unifiedRegimeScalarParams(block, label)
		if !ok || sl != 0.7 {
			t.Fatalf("resolve %s = (sl=%g ok=%v), want (0.7 true)", label, sl, ok)
		}
		if tiers := scalar["tp_tiers"].([]interface{}); len(tiers) != 2 {
			t.Fatalf("resolve %s tiers len = %d, want 2", label, len(tiers))
		}
	}

	upOnly := compositeUnifiedBlock(false)
	upTrend := upOnly[regimeClassifierKey].(map[string]interface{})
	upTrend["ranging_directional_up"] = upTrend["ranging_directional"]
	delete(upTrend, "ranging_directional")
	errs := validateUnifiedRegimeClose(upOnly, labels, "close.params")
	if joined := strings.Join(errs, " | "); !strings.Contains(joined, "ranging_directional") || !strings.Contains(joined, "ranging_directional_down") {
		t.Fatalf("up-only split config must not cover bare/down labels, got: %v", errs)
	}
}

func TestLoadConfig_CompositeUnifiedCloseBareRangingDirectional(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	dbPath := filepath.Join(dir, "state.db")
	cfgBody := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"regime": {
			"enabled": true, "period": 14, "adx_threshold": 20,
			"windows": {
				"daily": {"classifier": "composite", "period": 24, "thresholds": {"return_pct": 0.05, "range_pct": 0.03, "adx": 20}}
			}
		},
		"strategies": [{
			"id": "hl-unified",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["donchian_breakout", "BTC", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 25,
			"leverage": 1,
			"regime_atr_window": "daily",
			"close_strategy": {
				"name": "tiered_tp_atr_live_regime",
				"params": {
					"trend_regime": {
						"trending_up_clean": {"stop_loss_atr": 1.5, "tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.4}, {"atr_multiple": 3.0, "close_fraction": 1.0}]},
						"trending_up_choppy": {"stop_loss_atr": 1.2, "tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.4}, {"atr_multiple": 3.0, "close_fraction": 1.0}]},
						"trending_down_clean": {"stop_loss_atr": 1.5, "tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.4}, {"atr_multiple": 3.0, "close_fraction": 1.0}]},
						"trending_down_choppy": {"stop_loss_atr": 1.2, "tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.4}, {"atr_multiple": 3.0, "close_fraction": 1.0}]},
						"ranging_quiet": {"stop_loss_atr": 0.9, "tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.4}, {"atr_multiple": 3.0, "close_fraction": 1.0}]},
						"ranging_volatile": {"stop_loss_atr": 0.8, "tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.4}, {"atr_multiple": 3.0, "close_fraction": 1.0}]},
						"ranging_directional": {"stop_loss_atr": 0.7, "tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.4}, {"atr_multiple": 3.0, "close_fraction": 1.0}]}
					}
				}
			}
		}]
	}`
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig must accept base-keyed composite unified close, got: %v", err)
	}
	params := cfg.Strategies[0].CloseStrategy.Params
	if scalar, sl, ok := unifiedRegimeScalarParams(params, "ranging_directional_down"); !ok || sl != 0.7 || len(scalar["tp_tiers"].([]interface{})) != 2 {
		t.Fatalf("runtime resolve ranging_directional_down = (%v, %g, %v), want 2 tiers/0.7/true", scalar, sl, ok)
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
