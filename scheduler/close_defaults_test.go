package main

import (
	"strings"
	"testing"
)

func ratchetUserTiers() []interface{} {
	return []interface{}{
		map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 2.0, "close_fraction": 0.0},
		map[string]interface{}{"atr_multiple": 2.0, "trailing_mult_after": 1.0, "close_fraction": 0.0},
	}
}

func ratchetRegimeUserTiers() map[string]interface{} {
	tierList := func() []interface{} {
		return []interface{}{
			map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0},
			map[string]interface{}{"atr_multiple": 2.0, "trailing_mult_after": 0.75, "close_fraction": 0.0},
		}
	}
	return map[string]interface{}{
		"trending_up":   tierList(),
		"trending_down": tierList(),
		"ranging":       tierList(),
	}
}

func ratchetRegimeTrailRaw(up, down, ranging float64) map[string]interface{} {
	return map[string]interface{}{
		"trend_regime": map[string]interface{}{
			"trending_up":   map[string]interface{}{"atr_multiple": up},
			"trending_down": map[string]interface{}{"atr_multiple": down},
			"ranging":       map[string]interface{}{"atr_multiple": ranging},
		},
	}
}

func TestApplyUserCloseDefaultsToRef_InjectsWhenAbsent(t *testing.T) {
	defaults := CloseDefaultsMap{"trailing_tp_ratchet": {"tp_tiers": ratchetUserTiers()}}
	ref := &StrategyRef{Name: "trailing_tp_ratchet", Params: map[string]interface{}{"use_defaults": true}}
	if !applyUserCloseDefaultsToRef(ref, defaults) {
		t.Fatal("expected injection when tp_tiers absent")
	}
	if _, ok := closeTierListParam(ref.Params); !ok {
		t.Fatal("tp_tiers not injected")
	}
}

func TestApplyUserCloseDefaultsToRef_StrategyTiersWin(t *testing.T) {
	defaults := CloseDefaultsMap{"trailing_tp_ratchet": {"tp_tiers": ratchetUserTiers()}}
	explicit := []interface{}{map[string]interface{}{"atr_multiple": 9.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}}
	ref := &StrategyRef{Name: "trailing_tp_ratchet", Params: map[string]interface{}{"tp_tiers": explicit}}
	if applyUserCloseDefaultsToRef(ref, defaults) {
		t.Fatal("explicit per-strategy tp_tiers must win (no injection)")
	}
	got, _ := closeTierListParam(ref.Params)
	list, _ := got.([]interface{})
	if len(list) != 1 {
		t.Fatalf("explicit tiers mutated: %+v", got)
	}
}

func TestApplyUserCloseDefaultsToRef_NoMatchFallsThrough(t *testing.T) {
	defaults := CloseDefaultsMap{"tiered_tp_atr": {"tp_tiers": ratchetUserTiers()}}
	ref := &StrategyRef{Name: "trailing_tp_ratchet"} // no matching entry
	if applyUserCloseDefaultsToRef(ref, defaults) {
		t.Fatal("no matching entry should not inject")
	}
	if _, ok := closeTierListParam(ref.Params); ok {
		t.Fatal("tp_tiers should remain absent so the system default applies")
	}
}

func TestValidateUserCloseDefaults(t *testing.T) {
	validTiered := []interface{}{map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5}}
	if errs := validateUserCloseDefaults(CloseDefaultsMap{"tiered_tp_atr": {"tp_tiers": validTiered}}); len(errs) != 0 {
		t.Fatalf("a non-empty entry should pass, got: %v", errs)
	}
	if errs := validateUserCloseDefaults(CloseDefaultsMap{"trailing_tp_ratchet_regime": {
		"tp_tiers":                 ratchetRegimeUserTiers(),
		"trailing_stop_atr_regime": ratchetRegimeTrailRaw(2.25, 2.25, 1.25),
	}}); len(errs) != 0 {
		t.Fatalf("trailing_tp_ratchet_regime trail default should pass, got: %v", errs)
	}
	// Non-monotonic ratchet ladder: trail loosens 1.0 -> 2.0 across rungs.
	nonMonotonicRatchet := []interface{}{
		map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0},
		map[string]interface{}{"atr_multiple": 2.0, "trailing_mult_after": 2.0, "close_fraction": 0.0},
	}
	cases := []struct {
		name     string
		defaults CloseDefaultsMap
		want     string
	}{
		{"unknown evaluator", CloseDefaultsMap{"bogus_close": {"tp_tiers": []interface{}{}}}, "not a tp_tiers close evaluator"},
		{"missing tp_tiers", CloseDefaultsMap{"tiered_tp_atr": {}}, "missing tp_tiers"},
		{"stray key", CloseDefaultsMap{"tiered_tp_atr": {"tp_tiers": validTiered, "foo": 1}}, "unknown key"},
		{"trail key on other evaluator", CloseDefaultsMap{"trailing_tp_ratchet": {"tp_tiers": ratchetUserTiers(), "trailing_stop_atr_regime": ratchetRegimeTrailRaw(2.0, 2.0, 1.0)}}, "unknown key"},
		// empty tp_tiers is rejected (would inject [] and silently suppress the system default).
		{"empty list", CloseDefaultsMap{"trailing_tp_ratchet": {"tp_tiers": []interface{}{}}}, "must not be empty"},
		{"empty regime map", CloseDefaultsMap{"trailing_tp_ratchet_regime": {"tp_tiers": map[string]interface{}{}}}, "must not be empty"},
		{"wrong type", CloseDefaultsMap{"tiered_tp_atr": {"tp_tiers": 42}}, "must be a tier list or regime-keyed object"},
		{"bad trail shape", CloseDefaultsMap{"trailing_tp_ratchet_regime": {"tp_tiers": ratchetRegimeUserTiers(), "trailing_stop_atr_regime": map[string]interface{}{"trend_regime": map[string]interface{}{"trending_up": map[string]interface{}{"close_fraction": 0.5}}}}}, "close_fraction is only allowed inside close-evaluator tiers"},
		// non-monotonic ratchet ladder attributed to user_close_defaults, not the strategy.
		{"non-monotonic ratchet attributed", CloseDefaultsMap{"trailing_tp_ratchet": {"tp_tiers": nonMonotonicRatchet}}, "user_close_defaults[\"trailing_tp_ratchet\"].tp_tiers"},
		// the dynamic unified-regime evaluator is trend_regime-shaped (no tp_tiers) and excluded.
		{"dynamic excluded", CloseDefaultsMap{"tiered_tp_atr_live_regime_dynamic": {"tp_tiers": []interface{}{}}}, "not a tp_tiers close evaluator"},
		// regime tiered-ATR override is deferred to #870 (use_defaults baseline interaction).
		{"tiered regime excluded", CloseDefaultsMap{"tiered_tp_atr_regime": {"tp_tiers": []interface{}{}}}, "not a tp_tiers close evaluator"},
		{"regime_atr stray key", CloseDefaultsMap{"regime_atr": {"stop_loss_atr_regime": ratchetRegimeTrailRaw(2.0, 2.0, 1.5), "foo": 1}}, "unknown key"},
		{"regime_atr empty", CloseDefaultsMap{"regime_atr": {}}, "must not be empty"},
		{"regime_atr bad stop shape", CloseDefaultsMap{"regime_atr": {"stop_loss_atr_regime": map[string]interface{}{"trend_regime": map[string]interface{}{"trending_up": map[string]interface{}{"close_fraction": 0.5}}}}}, "close_fraction is only allowed inside close-evaluator tiers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if errs := validateUserCloseDefaults(tc.defaults); !errListContains(errs, tc.want) {
				t.Fatalf("want %q, got: %v", tc.want, errs)
			}
		})
	}
}

// TestUserCloseDefaults_EndToEndRatchet proves the middle layer: a ratchet
// strategy with use_defaults (no tp_tiers) resolves to the operator's
// user_close_defaults ladder — not the system default — and still validates.
func TestUserCloseDefaults_EndToEndRatchet(t *testing.T) {
	trail := 3.0
	cfg := &Config{
		UserCloseDefaults: CloseDefaultsMap{"trailing_tp_ratchet": {"tp_tiers": ratchetUserTiers()}},
		Strategies: []StrategyConfig{{
			ID: "s1", Type: "perps", Platform: "hyperliquid",
			TrailingStopATRMult: &trail,
			CloseStrategy:       &StrategyRef{Name: "trailing_tp_ratchet", Params: map[string]interface{}{"use_defaults": true}},
		}},
	}
	applyUserCloseDefaults(cfg)
	sc := cfg.Strategies[0]
	tiers := trailingRatchetTiersForRegime(sc, "")
	if len(tiers) != 2 || tiers[0].ATRMultiple != 1.0 || tiers[1].TrailingMultAfter != 1.0 {
		t.Fatalf("expected user-default tiers, got %+v", tiers)
	}
	// Differs from the 3-tier system default (proves the user layer took effect).
	if len(tiers) == len(defaultTrailingRatchetTiers()) {
		t.Fatal("resolved tiers match system default — user layer did not apply")
	}
	if errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true); len(errs) != 0 {
		t.Fatalf("user-default ratchet should validate, got: %v", errs)
	}
}

// TestUserCloseDefaults_LoadConfigInjects exercises the full load path
// (migrate → inject → validate) through LoadConfig with a temp config file.
func TestUserCloseDefaults_LoadConfigInjects(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"user_close_defaults": {
			"trailing_tp_ratchet": { "tp_tiers": [
				{"atr_multiple": 1.0, "trailing_mult_after": 2.0, "close_fraction": 0.0},
				{"atr_multiple": 2.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}
			]}
		},
		"strategies": [{
			"id": "hl-eth-ratchet",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"trailing_stop_atr_mult": 3.0,
			"close_strategy": {"name": "trailing_tp_ratchet", "params": {"use_defaults": true}}
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	tiers := trailingRatchetTiersForRegime(sc, "")
	if len(tiers) != 2 {
		t.Fatalf("expected 2 injected user-default tiers, got %d (%+v)", len(tiers), tiers)
	}
	if tiers[0].TrailingMultAfter != 2.0 || tiers[1].TrailingMultAfter != 1.0 {
		t.Fatalf("injected tiers = %+v, want trails 2.0 then 1.0", tiers)
	}
}

// TestUserCloseDefaults_LoadConfigRejectsUnknownEvaluator proves the block
// validation fires through LoadConfig.
func TestUserCloseDefaults_LoadConfigRejectsUnknownEvaluator(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"user_close_defaults": { "not_a_close_evaluator": { "tp_tiers": [] } },
		"strategies": [{
			"id": "test-spot", "type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"], "capital": 1000
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "not a tp_tiers close evaluator") {
		t.Fatalf("expected unknown-evaluator rejection, got: %v", err)
	}
}

func TestUserCloseDefaults_LoadConfigInjectsRatchetRegimeTrailBeforeScalarDefault(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"trailing_tp_ratchet_regime": {
				"tp_tiers": {
					"trending_up": [
						{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0},
						{"atr_multiple": 2.0, "trailing_mult_after": 0.75, "close_fraction": 0.0}
					],
					"trending_down": [
						{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0},
						{"atr_multiple": 2.0, "trailing_mult_after": 0.75, "close_fraction": 0.0}
					],
					"ranging": [
						{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0},
						{"atr_multiple": 2.0, "trailing_mult_after": 0.75, "close_fraction": 0.0}
					]
				},
				"trailing_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.25},
						"trending_down": {"atr_multiple": 2.25},
						"ranging": {"atr_multiple": 1.25}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-eth-ratchet-regime",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"close_strategy": {"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": true}}
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult != nil {
		t.Fatalf("StopLossATRMult = %v, want nil because user regime trail owns the SL", *sc.StopLossATRMult)
	}
	if sc.TrailingStopATRRegime == nil || !sc.TrailingStopATRRegime.IsConfigured() {
		t.Fatal("TrailingStopATRRegime was not injected")
	}
	if got, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, "ranging"); !ok || got != 1.25 {
		t.Fatalf("ranging trail = (%g, %v), want (1.25, true)", got, ok)
	}
	tiers := trailingRatchetTiersForRegime(sc, "trending_up")
	if len(tiers) != 2 || tiers[0].TrailingMultAfter != 1.0 || tiers[1].TrailingMultAfter != 0.75 {
		t.Fatalf("injected regime tiers = %+v, want user defaults", tiers)
	}
}

func TestUserCloseDefaults_RatchetRegimeTrailDoesNotOverrideExplicitStopOwner(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"trailing_tp_ratchet_regime": {
				"tp_tiers": {
					"trending_up": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
					"trending_down": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
					"ranging": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}]
				},
				"trailing_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.0},
						"trending_down": {"atr_multiple": 2.0},
						"ranging": {"atr_multiple": 1.0}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-eth-ratchet-regime",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"stop_loss_atr_mult": 1.0,
			"close_strategy": {"name": "trailing_tp_ratchet_regime", "params": {"use_defaults": true}}
		}]
	}`
	_, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err == nil {
		t.Fatal("LoadConfig accepted an explicit scalar stop owner with trailing_tp_ratchet_regime")
	}
	if !strings.Contains(err.Error(), "requires trailing_stop_atr_regime") || !strings.Contains(err.Error(), "cannot combine with stop_loss_atr_mult") {
		t.Fatalf("expected missing-regime-owner plus scalar-conflict errors, got: %v", err)
	}
}

func TestUserCloseDefaults_ManualSynthesizedRatchetUsesUserTrail(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"trailing_tp_ratchet_regime": {
				"tp_tiers": {
					"trending_up": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
					"trending_down": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
					"ranging": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}]
				},
				"trailing_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.75},
						"trending_down": {"atr_multiple": 2.75},
						"ranging": {"atr_multiple": 1.5}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-manual-eth",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.CloseStrategy == nil || sc.CloseStrategy.Name != trailingTPRatchetRegimeCloseName {
		t.Fatalf("CloseStrategy = %v, want %s", sc.CloseStrategy, trailingTPRatchetRegimeCloseName)
	}
	if got, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, "trending_up"); !ok || got != 2.75 {
		t.Fatalf("trending_up trail = (%g, %v), want (2.75, true)", got, ok)
	}
	if sc.StopLossATRMult != nil {
		t.Fatalf("StopLossATRMult = %v, want nil under regime ratchet trail owner", *sc.StopLossATRMult)
	}
}

func TestUserCloseDefaults_ManualDefaultsTrailWinsOverUserTrail(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"manual_defaults": {
			"trailing_stop_atr_regime": {
				"trend_regime": {
					"trending_up": {"atr_multiple": 3.5},
					"trending_down": {"atr_multiple": 3.5},
					"ranging": {"atr_multiple": 2.0}
				}
			}
		},
		"user_close_defaults": {
			"trailing_tp_ratchet_regime": {
				"tp_tiers": {
					"trending_up": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
					"trending_down": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
					"ranging": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}]
				},
				"trailing_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.0},
						"trending_down": {"atr_multiple": 2.0},
						"ranging": {"atr_multiple": 1.0}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-manual-eth",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if got, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, "trending_up"); !ok || got != 3.5 {
		t.Fatalf("trending_up trail = (%g, %v), want manual default (3.5, true)", got, ok)
	}
}

func TestUserCloseDefaults_RegimeATRInjectsStandaloneStopLoss(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"regime_atr": {
				"stop_loss_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.25},
						"trending_down": {"atr_multiple": 2.25},
						"ranging": {"atr_multiple": 1.25}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-eth-sl-regime",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"stop_loss_atr_regime": {"use_defaults": true}
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if got, ok := resolveRegimeATR(*sc.StopLossATRRegime, "ranging"); !ok || got != 1.25 {
		t.Fatalf("ranging SL = (%g, %v), want user default (1.25, true)", got, ok)
	}
}

func TestUserCloseDefaults_RegimeATRStrategyExplicitWins(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"regime_atr": {
				"stop_loss_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 9.0},
						"trending_down": {"atr_multiple": 9.0},
						"ranging": {"atr_multiple": 9.0}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-eth-sl-regime",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"stop_loss_atr_regime": {
				"trend_regime": {
					"trending_up": {"atr_multiple": 2.0},
					"trending_down": {"atr_multiple": 2.0},
					"ranging": {"atr_multiple": 1.5}
				}
			}
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if got, ok := resolveRegimeATR(*sc.StopLossATRRegime, "ranging"); !ok || got != 1.5 {
		t.Fatalf("ranging SL = (%g, %v), want per-strategy explicit (1.5, true)", got, ok)
	}
}

func TestUserCloseDefaults_RegimeATRSkipsManualRatchet(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"regime_atr": {
				"trailing_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 9.0},
						"trending_down": {"atr_multiple": 9.0},
						"ranging": {"atr_multiple": 9.0}
					}
				}
			},
			"trailing_tp_ratchet_regime": {
				"tp_tiers": {
					"trending_up": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
					"trending_down": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}],
					"ranging": [{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}]
				},
				"trailing_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.75},
						"trending_down": {"atr_multiple": 2.75},
						"ranging": {"atr_multiple": 1.5}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-manual-eth",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if got, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, "trending_up"); !ok || got != 2.75 {
		t.Fatalf("trending_up trail = (%g, %v), want #1133 ratchet-coupled default (2.75, true), not regime_atr", got, ok)
	}
}

func TestRegimeATRBlockIsUseDefaultsOnly(t *testing.T) {
	if regimeATRBlockIsUseDefaultsOnly(nil) {
		t.Fatal("nil block is not use_defaults-only")
	}
	if regimeATRBlockIsUseDefaultsOnly(&RegimeATRBlock{raw: map[string]interface{}{"use_defaults": true}}) != true {
		t.Fatal("expected use_defaults-only")
	}
	if regimeATRBlockIsUseDefaultsOnly(&RegimeATRBlock{raw: map[string]interface{}{
		"use_defaults": true,
		"trend_regime": map[string]interface{}{"ranging": map[string]interface{}{"atr_multiple": 1.0}},
	}}) {
		t.Fatal("explicit trend_regime must not count as use_defaults-only")
	}
}

// A flat strategy whose standalone stop_loss_atr_regime is use_defaults-only
// must pick up a changed user_close_defaults.regime_atr default across SIGHUP:
// LoadConfig re-runs injection for the next config and the flat regime-block
// copy applies it. Proves the hot-reload claim rides existing infra (#1134).
func TestUserCloseDefaults_RegimeATRHotReloadAppliesWhenFlat(t *testing.T) {
	const tmpl = `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"regime_atr": {
				"stop_loss_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.0},
						"trending_down": {"atr_multiple": 2.0},
						"ranging": {"atr_multiple": RANGING}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-eth-sl-regime",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"stop_loss_atr_regime": {"use_defaults": true}
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, t.TempDir(), strings.Replace(tmpl, "RANGING", "1.25", 1)))
	if err != nil {
		t.Fatalf("LoadConfig (old) failed: %v", err)
	}
	next, err := LoadConfig(writeTestConfig(t, t.TempDir(), strings.Replace(tmpl, "RANGING", "1.75", 1)))
	if err != nil {
		t.Fatalf("LoadConfig (next) failed: %v", err)
	}
	// Sanity: injection produced the old default before reload.
	if got, ok := resolveRegimeATR(*cfg.Strategies[0].StopLossATRRegime, "ranging"); !ok || got != 1.25 {
		t.Fatalf("pre-reload ranging SL = (%g, %v), want injected old default (1.25, true)", got, ok)
	}
	if _, err := applyHotReloadConfig(cfg, next, NewAppState(), nil, nil); err != nil {
		t.Fatalf("applyHotReloadConfig failed: %v", err)
	}
	if got, ok := resolveRegimeATR(*cfg.Strategies[0].StopLossATRRegime, "ranging"); !ok || got != 1.75 {
		t.Fatalf("post-reload ranging SL = (%g, %v), want new default (1.75, true)", got, ok)
	}
}
