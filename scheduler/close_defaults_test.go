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

// ─── #1134: user_close_defaults["regime_atr"] standalone stop owners ───────

// regimeATRUserRaw builds an ADX trend_regime map for a user regime_atr
// sub-block (distinct from ratchetRegimeTrailRaw only in intent — reused shape).
func regimeATRUserRaw(up, down, ranging float64) map[string]interface{} {
	return ratchetRegimeTrailRaw(up, down, ranging)
}

// compositeBareDirectionalATR builds a 9-state composite trend_regime map that
// omits ranging_directional_up/_down but keeps the bare ranging_directional
// label — the #1124 bare-covers-subs shape. Used to prove the family rule
// applies to the reserved regime_atr section's per-strategy validation.
func compositeBareDirectionalATR(atr float64) map[string]interface{} {
	labels := regimeLabelsForClassifier(regimeClassifierComposite)
	tr := make(map[string]interface{}, len(labels))
	for _, l := range labels {
		if l == "ranging_directional_up" || l == "ranging_directional_down" {
			continue
		}
		tr[l] = map[string]interface{}{"atr_multiple": atr}
	}
	return map[string]interface{}{"trend_regime": tr}
}

func TestValidateUserCloseDefaultRegimeATR(t *testing.T) {
	validSL := regimeATRUserRaw(2.5, 2.5, 1.75)
	validTrail := regimeATRUserRaw(3.0, 3.0, 2.0)
	cases := []struct {
		name     string
		entry    map[string]interface{}
		want     string
		wantNone bool
	}{
		{"valid both sub-blocks", map[string]interface{}{"stop_loss_atr_regime": validSL, "trailing_stop_atr_regime": validTrail}, "", true},
		{"valid single stop_loss", map[string]interface{}{"stop_loss_atr_regime": validSL}, "", true},
		{"use_defaults no-op accepted", map[string]interface{}{"stop_loss_atr_regime": map[string]interface{}{"use_defaults": true}}, "", true},
		{"composite bare-covers-subs accepted", map[string]interface{}{"stop_loss_atr_regime": compositeBareDirectionalATR(2.0)}, "", true},
		{"stray key", map[string]interface{}{"stop_loss_atr_regime": validSL, "foo": 1}, "unknown key", false},
		{"empty sub-block", map[string]interface{}{"stop_loss_atr_regime": map[string]interface{}{}}, "must not be empty", false},
		{"close_fraction on SL surface", map[string]interface{}{"stop_loss_atr_regime": map[string]interface{}{"trend_regime": map[string]interface{}{"trending_up": map[string]interface{}{"close_fraction": 0.5}}}}, "close_fraction is only allowed", false},
		{"missing trend_regime", map[string]interface{}{"stop_loss_atr_regime": map[string]interface{}{"use_defaults": false}}, "missing", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateUserCloseDefaultRegimeATR("regime_atr", tc.entry)
			if tc.wantNone {
				if len(errs) != 0 {
					t.Fatalf("expected no errors, got: %v", errs)
				}
				return
			}
			if !errListContains(errs, tc.want) {
				t.Fatalf("want %q, got: %v", tc.want, errs)
			}
		})
	}
	// nil entry → must be an object.
	if errs := validateUserCloseDefaultRegimeATR("regime_atr", nil); !errListContains(errs, "must be an object") {
		t.Fatalf("nil entry: want 'must be an object', got: %v", errs)
	}
}

func TestValidateUserCloseDefaultRegimeATR_RoutedThroughValidateUserCloseDefaults(t *testing.T) {
	// The reserved regime_atr key must NOT trip the evaluator-name allowlist:
	// a valid regime_atr section passes the top-level validator, while a
	// malformed one surfaces its dedicated-branch error.
	if errs := validateUserCloseDefaults(CloseDefaultsMap{
		"regime_atr": {"stop_loss_atr_regime": regimeATRUserRaw(2.5, 2.5, 1.75)},
	}); len(errs) != 0 {
		t.Fatalf("valid regime_atr should pass validateUserCloseDefaults, got: %v", errs)
	}
	errs := validateUserCloseDefaults(CloseDefaultsMap{
		"regime_atr": {"stop_loss_atr_regime": map[string]interface{}{"bogus_key": 1}},
	})
	if !errListContains(errs, "unknown key") {
		t.Fatalf("want regime_atr dedicated-branch 'unknown key' error, got: %v", errs)
	}
	// Evaluator-name entries keep their existing three-gate validation.
	if errs := validateUserCloseDefaults(CloseDefaultsMap{"tiered_tp_atr": {}}); !errListContains(errs, "missing tp_tiers") {
		t.Fatalf("evaluator-name tp_tiers-required gate must still fire, got: %v", errs)
	}
}

func TestRegimeATRBlock_IsUseDefaultsOnly(t *testing.T) {
	if (&RegimeATRBlock{}).IsUseDefaultsOnly() {
		t.Fatal("zero block is not use_defaults-only")
	}
	if (&RegimeATRBlock{raw: map[string]interface{}{}}).IsUseDefaultsOnly() {
		t.Fatal("empty-raw block is not use_defaults-only")
	}
	if (&RegimeATRBlock{raw: map[string]interface{}{"use_defaults": false}}).IsUseDefaultsOnly() {
		t.Fatal("use_defaults:false is not use_defaults-only")
	}
	if (&RegimeATRBlock{raw: map[string]interface{}{"trend_regime": map[string]interface{}{"ranging": map[string]interface{}{"atr_multiple": 1.0}}}}).IsUseDefaultsOnly() {
		t.Fatal("explicit trend_regime map is not use_defaults-only (strategy layer wins)")
	}
	if !(&RegimeATRBlock{raw: map[string]interface{}{"use_defaults": true}}).IsUseDefaultsOnly() {
		t.Fatal("{use_defaults:true} must be use_defaults-only")
	}
}

func TestApplyUserCloseDefaultRegimeATR_InjectsStandaloneUseDefaultsOwner(t *testing.T) {
	defaults := CloseDefaultsMap{"regime_atr": {"stop_loss_atr_regime": regimeATRUserRaw(2.5, 2.5, 1.75)}}
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		StopLossATRRegime: &RegimeATRBlock{raw: map[string]interface{}{"use_defaults": true}},
	}
	if !applyUserCloseDefaultRegimeATR(&sc, defaults) {
		t.Fatal("expected injection onto use_defaults-only standalone owner")
	}
	if errs := sc.StopLossATRRegime.ResolveSurface("s1.stop_loss_atr_regime", regimeSurfaceStopLoss); len(errs) != 0 {
		t.Fatalf("injected block must validate: %v", errs)
	}
	if got, ok := resolveRegimeATR(*sc.StopLossATRRegime, "ranging"); !ok || got != 1.75 {
		t.Fatalf("ranging = (%g, %v), want (1.75, true) from user regime_atr", got, ok)
	}
}

func TestApplyUserCloseDefaultRegimeATR_ExplicitStrategyMapWins(t *testing.T) {
	defaults := CloseDefaultsMap{"regime_atr": {"stop_loss_atr_regime": regimeATRUserRaw(9.0, 9.0, 9.0)}}
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		StopLossATRRegime: &RegimeATRBlock{raw: regimeATRUserRaw(2.0, 2.0, 1.5)},
	}
	if applyUserCloseDefaultRegimeATR(&sc, defaults) {
		t.Fatal("explicit per-strategy trend_regime map must NOT be overridden by user regime_atr")
	}
	if errs := sc.StopLossATRRegime.ResolveSurface("s1.stop_loss_atr_regime", regimeSurfaceStopLoss); len(errs) != 0 {
		t.Fatalf("explicit block must validate: %v", errs)
	}
	if got, ok := resolveRegimeATR(*sc.StopLossATRRegime, "ranging"); !ok || got != 1.5 {
		t.Fatalf("ranging = (%g, %v), want (1.5, true) from explicit strategy map", got, ok)
	}
}

func TestApplyUserCloseDefaultRegimeATR_NilOwnerNotInjected(t *testing.T) {
	defaults := CloseDefaultsMap{"regime_atr": {"stop_loss_atr_regime": regimeATRUserRaw(2.5, 2.5, 1.75)}}
	sc := StrategyConfig{ID: "s1", Type: "perps", Platform: "hyperliquid"}
	if applyUserCloseDefaultRegimeATR(&sc, defaults) {
		t.Fatal("a strategy with no stop_loss_atr_regime field must not get one injected")
	}
	if sc.StopLossATRRegime != nil {
		t.Fatalf("StopLossATRRegime = %v, want nil (strategy did not opt into a regime ATR stop)", sc.StopLossATRRegime)
	}
}

func TestApplyUserCloseDefaultRegimeATR_SkipsRatchetRegimeClose(t *testing.T) {
	defaults := CloseDefaultsMap{"regime_atr": {"trailing_stop_atr_regime": regimeATRUserRaw(9.0, 9.0, 9.0)}}
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRRegime: &RegimeATRBlock{raw: map[string]interface{}{"use_defaults": true}},
		CloseStrategy:         &StrategyRef{Name: trailingTPRatchetRegimeCloseName},
	}
	if applyUserCloseDefaultRegimeATR(&sc, defaults) {
		t.Fatal("Phase-2 must skip trailing_tp_ratchet_regime strategies (disjoint from #1133)")
	}
	// The synthesized {use_defaults:true} owner resolves to the system table,
	// NOT the user regime_atr value (9.0).
	if errs := sc.TrailingStopATRRegime.ResolveSurface("s1.trailing_stop_atr_regime", regimeSurfaceTrailing); len(errs) != 0 {
		t.Fatalf("synthesized block must validate: %v", errs)
	}
	if got, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, "ranging"); !ok || got == 9.0 {
		t.Fatalf("ranging = (%g, %v); must not be the user regime_atr value 9.0", got, ok)
	}
}

func TestUserCloseDefaultRegimeATR_LoadConfigEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"regime_atr": {
				"stop_loss_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.5},
						"trending_down": {"atr_multiple": 2.5},
						"ranging": {"atr_multiple": 1.75}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-eth-slregime",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"stop_loss_atr_regime": {"use_defaults": true},
			"close_strategy": {"name": "tiered_tp_atr_live", "params": {"tp_tiers": [{"atr_multiple": 3.0, "close_fraction": 1.0}]}}
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRRegime == nil || !sc.StopLossATRRegime.IsConfigured() {
		t.Fatal("StopLossATRRegime was not injected")
	}
	if got, ok := resolveRegimeATR(*sc.StopLossATRRegime, "ranging"); !ok || got != 1.75 {
		t.Fatalf("ranging SL = (%g, %v), want (1.75, true) from user regime_atr", got, ok)
	}
	if got, ok := resolveRegimeATR(*sc.StopLossATRRegime, "trending_up"); !ok || got != 2.5 {
		t.Fatalf("trending_up SL = (%g, %v), want (2.5, true) from user regime_atr", got, ok)
	}
	// Differs from the system table (ranging=1.5) — proves the user layer took effect.
	if got, _ := resolveRegimeATR(*sc.StopLossATRRegime, "ranging"); got == 1.5 {
		t.Fatal("ranging SL matches system default (1.5) — user regime_atr layer did not apply")
	}
	// No scalar SL default was applied on top (would have tripped the sole-owner mutex).
	if sc.StopLossATRMult != nil {
		t.Fatalf("StopLossATRMult = %v, want nil (regime owner is the sole SL)", *sc.StopLossATRMult)
	}
}

func TestUserCloseDefaultRegimeATR_LoadConfigRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"regime_atr": {"stop_loss_atr_regime": {"trend_regime": {"trending_up": {"close_fraction": 0.5}}}}
		},
		"strategies": [{
			"id": "hl-eth-slregime",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"stop_loss_atr_regime": {"use_defaults": true},
			"close_strategy": {"name": "tiered_tp_atr_live", "params": {"tp_tiers": [{"atr_multiple": 3.0, "close_fraction": 1.0}]}}
		}]
	}`
	_, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err == nil {
		t.Fatal("LoadConfig accepted a malformed regime_atr sub-block (close_fraction on SL surface)")
	}
	if !strings.Contains(err.Error(), "close_fraction is only allowed") {
		t.Fatalf("expected dedicated-branch close_fraction rejection, got: %v", err)
	}
}

func TestUserCloseDefaultRegimeATR_LoadConfigUseDefaultsNoOp(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_close_defaults": {
			"regime_atr": {"stop_loss_atr_regime": {"use_defaults": true}}
		},
		"strategies": [{
			"id": "hl-eth-slregime",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"stop_loss_atr_regime": {"use_defaults": true},
			"close_strategy": {"name": "tiered_tp_atr_live", "params": {"tp_tiers": [{"atr_multiple": 3.0, "close_fraction": 1.0}]}}
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	// A use_defaults user sub-block resolves identically to the system table
	// (documented no-op) — not a distinct middle layer. System ranging SL = 1.5.
	if got, ok := resolveRegimeATR(*cfg.Strategies[0].StopLossATRRegime, "ranging"); !ok || got != 1.5 {
		t.Fatalf("ranging SL = (%g, %v), want (1.5, true) system table (use_defaults no-op)", got, ok)
	}
}

func TestUserCloseDefaultRegimeATR_LoadConfigSkipsManualRatchet(t *testing.T) {
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
		t.Fatalf("CloseStrategy = %v, want synthesized %s", sc.CloseStrategy, trailingTPRatchetRegimeCloseName)
	}
	if sc.TrailingStopATRRegime == nil || !sc.TrailingStopATRRegime.IsConfigured() {
		t.Fatal("manual synthesized TrailingStopATRRegime missing")
	}
	// The manual synthesized trail resolves to the SYSTEM table (ranging=2.0),
	// NOT the user regime_atr value (9.0) — the !strategyUsesTrailingTPRatchetRegimeClose
	// guard prevented Phase-2 from overwriting the live manual SL owner.
	if got, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, "ranging"); !ok || got != 2.0 {
		t.Fatalf("manual ranging trail = (%g, %v), want (2.0, true) system table (Phase-2 must not overwrite)", got, ok)
	}
}

func TestUserCloseDefaultRegimeATR_LoadConfigCompositeBareCoversSubs(t *testing.T) {
	dir := t.TempDir()
	// Composite regime_atr_window; user regime_atr block keyed on bare
	// ranging_directional (omits _up/_down subs) — the #1124 family rule must
	// accept it and runtime resolution of a sub-label must fall back to bare.
	cfgJSON := `{
		"regime": {"enabled": true, "windows": {"daily": {"classifier": "composite", "period": 24}}},
		"user_close_defaults": {
			"regime_atr": {"stop_loss_atr_regime": {
				"trend_regime": {
					"trending_up_clean": {"atr_multiple": 2.5},
					"trending_up_choppy": {"atr_multiple": 2.5},
					"trending_down_clean": {"atr_multiple": 2.5},
					"trending_down_choppy": {"atr_multiple": 2.5},
					"ranging_quiet": {"atr_multiple": 1.5},
					"ranging_volatile": {"atr_multiple": 1.5},
					"ranging_directional": {"atr_multiple": 1.25}
				}
			}}
		},
		"strategies": [{
			"id": "hl-eth-slregime-comp",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"regime_atr_window": "daily",
			"stop_loss_atr_regime": {"use_defaults": true},
			"close_strategy": {"name": "tiered_tp_atr_live", "params": {"tp_tiers": [{"atr_multiple": 3.0, "close_fraction": 1.0}]}}
		}]
	}`
	cfg, err := LoadConfig(writeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if got, ok := resolveRegimeATR(*sc.StopLossATRRegime, "trending_up_clean"); !ok || got != 2.5 {
		t.Fatalf("trending_up_clean SL = (%g, %v), want (2.5, true)", got, ok)
	}
	// Sub-label falls back to the bare ranging_directional entry (1.25).
	if got, ok := resolveRegimeATR(*sc.StopLossATRRegime, "ranging_directional_up"); !ok || got != 1.25 {
		t.Fatalf("ranging_directional_up SL = (%g, %v), want (1.25, true) via bare fallback", got, ok)
	}
}
