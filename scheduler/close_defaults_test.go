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
		// empty tp_tiers is rejected (would inject [] and silently suppress the system default).
		{"empty list", CloseDefaultsMap{"trailing_tp_ratchet": {"tp_tiers": []interface{}{}}}, "must not be empty"},
		{"empty regime map", CloseDefaultsMap{"trailing_tp_ratchet_regime": {"tp_tiers": map[string]interface{}{}}}, "must not be empty"},
		{"wrong type", CloseDefaultsMap{"tiered_tp_atr": {"tp_tiers": 42}}, "must be a tier list or regime-keyed object"},
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
