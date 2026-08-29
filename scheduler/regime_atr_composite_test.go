package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func composite7StateATR(atr float64) map[string]interface{} {
	labels := regimeLabelsForClassifier(regimeClassifierComposite)
	tr := make(map[string]interface{}, len(labels))
	for _, l := range labels {
		tr[l] = map[string]interface{}{"atr_multiple": atr}
	}
	return map[string]interface{}{"trend_regime": tr}
}

func composite7StateTier(atr, frac float64) map[string]interface{} {
	labels := regimeLabelsForClassifier(regimeClassifierComposite)
	tr := make(map[string]interface{}, len(labels))
	for _, l := range labels {
		tr[l] = map[string]interface{}{"atr_multiple": atr, "close_fraction": frac}
	}
	return map[string]interface{}{"trend_regime": tr}
}

func compositeRegimeCfg(scs ...StrategyConfig) *Config {
	return &Config{
		Regime: &RegimeConfig{
			Enabled: true,
			Windows: RegimeWindowsMap{
				"daily": {Classifier: regimeClassifierComposite, Period: 24},
			},
		},
		Strategies: scs,
	}
}

func TestValidateRegimeATRConfig_CompositeStopLossExplicit(t *testing.T) {
	sc := StrategyConfig{
		ID:                "hl-test",
		Type:              "perps",
		Platform:          "hyperliquid",
		RegimeATRWindow:   "daily",
		StopLossATRMultRegime: &RegimeATRBlock{raw: composite7StateATR(2.0)},
	}
	cfg := compositeRegimeCfg(sc)
	if errs := validateRegimeATRConfig(cfg); len(errs) != 0 {
		t.Fatalf("composite stop_loss_atr_mult_regime must validate, got: %v", errs)
	}
	block := cfg.Strategies[0].StopLossATRMultRegime
	if got := len(block.TrendRegime); got != 9 {
		t.Fatalf("block must be populated with all 9 composite labels, got %d: %v", got, block.TrendRegime)
	}
	if v, ok := resolveRegimeATR(*block, "trending_up_clean"); !ok || v != 2.0 {
		t.Fatalf("resolveRegimeATR(trending_up_clean) = (%g, %v), want (2.0, true)", v, ok)
	}
}

func TestValidateRegimeATRConfig_CompositeSLAfterTPATRFraction(t *testing.T) {
	tier0 := composite7StateTier(2.0, 0.5)
	tier0["sl_after"] = map[string]interface{}{
		"kind": "trail_from_here",
		"tp_atr_fraction": map[string]interface{}{"trend_regime": map[string]interface{}{
			"trending_up_clean": 0.5, "trending_up_choppy": 0.5,
			"trending_down_clean": 0.5, "trending_down_choppy": 0.5,
			"ranging_directional": 0.5, "ranging_volatile": 0.5, "ranging_quiet": 0.5,
		}},
	}
	tier1 := composite7StateTier(4.0, 1.0)
	slMult := 1.5
	sc := StrategyConfig{
		ID:              "hl-test",
		Type:            "perps",
		Platform:        "hyperliquid",
		RegimeATRWindow: "daily",
		StopLossATRMult: &slMult,
		CloseStrategy: &StrategyRef{
			Name:   "tiered_tp_atr_regime",
			Params: map[string]interface{}{"tp_tiers": []interface{}{tier0, tier1}},
		},
	}
	cfg := compositeRegimeCfg(sc)
	if errs := validateRegimeATRConfig(cfg); len(errs) != 0 {
		t.Fatalf("composite per-tier sl_after tp_atr_fraction must validate, got: %v", errs)
	}
}

func TestValidateRegimeATRConfig_CompositeTrailingExplicit(t *testing.T) {
	sc := StrategyConfig{
		ID:                 "hl-test",
		Type:               "perps",
		Platform:           "hyperliquid",
		RegimeATRWindow:    "daily",
		TrailingStopATRMultRegime: &RegimeATRBlock{raw: composite7StateATR(2.5)},
	}
	cfg := compositeRegimeCfg(sc)
	if errs := validateRegimeATRConfig(cfg); len(errs) != 0 {
		t.Fatalf("composite trailing_stop_atr_mult_regime must validate, got: %v", errs)
	}
	if got := len(cfg.Strategies[0].TrailingStopATRMultRegime.TrendRegime); got != 9 {
		t.Fatalf("trailing block must hold 9 composite labels, got %d", got)
	}
}

func TestParseRegimeATRBlock_TrailingUseDefaultsComposite(t *testing.T) {
	labels := regimeLabelsForClassifier(regimeClassifierComposite)
	raw := map[string]interface{}{"use_defaults": true}
	block, errs := parseRegimeATRBlock(raw, "trailing_stop_atr_mult_regime", regimeSurfaceTrailing, labels)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	for _, label := range []string{"trending_up_clean", "trending_down_clean"} {
		v, ok := resolveRegimeATR(block, label)
		if !ok || v != 2.5 {
			t.Fatalf("resolveRegimeATR(%s) = (%g, %v), want (2.5, true)", label, v, ok)
		}
	}
	for _, label := range []string{"trending_up_choppy", "trending_down_choppy"} {
		v, ok := resolveRegimeATR(block, label)
		if !ok || v != 2.25 {
			t.Fatalf("resolveRegimeATR(%s) = (%g, %v), want (2.25, true)", label, v, ok)
		}
	}
	if v, ok := resolveRegimeATR(block, "ranging_quiet"); !ok || v != 1.0 {
		t.Fatalf("resolveRegimeATR(ranging_quiet) = (%g, %v), want (1.0, true)", v, ok)
	}
	if v, ok := resolveRegimeATR(block, "ranging_volatile"); !ok || v != 1.25 {
		t.Fatalf("resolveRegimeATR(ranging_volatile) = (%g, %v), want (1.25, true)", v, ok)
	}
	for _, label := range []string{"ranging_directional", "ranging_directional_up", "ranging_directional_down"} {
		v, ok := resolveRegimeATR(block, label)
		if !ok || v != 1.5 {
			t.Fatalf("resolveRegimeATR(%s) = (%g, %v), want (1.5, true)", label, v, ok)
		}
	}
}

func TestValidateRegimeATRConfig_CompositeMissingLabelRejected(t *testing.T) {
	raw := composite7StateATR(2.0)
	delete(raw["trend_regime"].(map[string]interface{}), "ranging_volatile")
	sc := StrategyConfig{
		ID:                "hl-test",
		Type:              "perps",
		Platform:          "hyperliquid",
		RegimeATRWindow:   "daily",
		StopLossATRMultRegime: &RegimeATRBlock{raw: raw},
	}
	errs := validateRegimeATRConfig(compositeRegimeCfg(sc))
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "ranging_volatile") {
		t.Fatalf("expected missing-label error naming ranging_volatile, got: %v", errs)
	}
	if strings.Contains(joined, "expected one of: trending_up, trending_down, ranging") {
		t.Fatalf("error must use composite vocabulary, not ADX 3-state: %v", errs)
	}
	if !strings.Contains(joined, "classifier \"composite\"") {
		t.Fatalf("error should carry composite classifier context, got: %v", errs)
	}
}

func TestValidateRegimeATRConfig_CompositeBareDirectionalCoversSubLabels(t *testing.T) {
	raw := composite7StateATR(2.0)
	tr := raw["trend_regime"].(map[string]interface{})
	delete(tr, "ranging_directional_up")
	delete(tr, "ranging_directional_down")
	if _, ok := tr["ranging_directional"]; !ok {
		t.Fatal("test fixture: expected bare ranging_directional key")
	}
	sc := StrategyConfig{
		ID:                "hl-test",
		Type:              "perps",
		Platform:          "hyperliquid",
		RegimeATRWindow:   "daily",
		StopLossATRMultRegime: &RegimeATRBlock{raw: raw},
	}
	if errs := validateRegimeATRConfig(compositeRegimeCfg(sc)); len(errs) != 0 {
		t.Fatalf("legacy bare-only directional block must still validate, got: %v", errs)
	}
}

func TestValidateRegimeATRConfig_CompositeSubLabelsWithoutBareRejected(t *testing.T) {
	raw := composite7StateATR(2.0)
	delete(raw["trend_regime"].(map[string]interface{}), "ranging_directional")
	sc := StrategyConfig{
		ID:                "hl-test",
		Type:              "perps",
		Platform:          "hyperliquid",
		RegimeATRWindow:   "daily",
		StopLossATRMultRegime: &RegimeATRBlock{raw: raw},
	}
	errs := validateRegimeATRConfig(compositeRegimeCfg(sc))
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "ranging_directional") || !strings.Contains(joined, "missing required regime labels") {
		t.Fatalf("expected missing bare ranging_directional error, got: %v", errs)
	}
}

func TestRegimeATRBlock_ResolveSubLabelFallsBackToBareAndExplicitWins(t *testing.T) {
	raw := composite7StateATR(1.5)
	tr := raw["trend_regime"].(map[string]interface{})
	delete(tr, "ranging_directional_up")
	delete(tr, "ranging_directional_down")
	sc := StrategyConfig{
		ID:                "hl-test",
		Type:              "perps",
		Platform:          "hyperliquid",
		RegimeATRWindow:   "daily",
		StopLossATRMultRegime: &RegimeATRBlock{raw: raw},
	}
	if errs := validateRegimeATRConfig(compositeRegimeCfg(sc)); len(errs) != 0 {
		t.Fatalf("fixture must validate, got: %v", errs)
	}
	block := *sc.StopLossATRMultRegime
	if v, ok := block.Resolve("ranging_directional"); !ok || v.ATR != 1.5 {
		t.Fatalf("bare resolve: got (%g, %v), want (1.5, true)", v.ATR, ok)
	}
	for _, sub := range []string{"ranging_directional_up", "ranging_directional_down"} {
		v, ok := block.Resolve(sub)
		if !ok || v.ATR != 1.5 {
			t.Errorf("Resolve(%q): got (%g, %v), want (1.5, true) via bare fallback", sub, v.ATR, ok)
		}
	}

	raw2 := composite7StateATR(1.5)
	raw2["trend_regime"].(map[string]interface{})["ranging_directional_up"] = map[string]interface{}{"atr_multiple": 0.9}
	sc2 := StrategyConfig{
		ID:                "hl-test2",
		Type:              "perps",
		Platform:          "hyperliquid",
		RegimeATRWindow:   "daily",
		StopLossATRMultRegime: &RegimeATRBlock{raw: raw2},
	}
	if errs := validateRegimeATRConfig(compositeRegimeCfg(sc2)); len(errs) != 0 {
		t.Fatalf("explicit-sub fixture must validate, got: %v", errs)
	}
	block2 := *sc2.StopLossATRMultRegime
	if v, ok := block2.Resolve("ranging_directional_up"); !ok || v.ATR != 0.9 {
		t.Fatalf("explicit _up must win: got (%g, %v), want (0.9, true)", v.ATR, ok)
	}
	if v, ok := block2.Resolve("ranging_directional_down"); !ok || v.ATR != 1.5 {
		t.Fatalf("_down must fall back to bare: got (%g, %v), want (1.5, true)", v.ATR, ok)
	}
}

func TestValidateRegimeATRConfig_CompositeTPTiersExplicit(t *testing.T) {
	sc := StrategyConfig{
		ID:              "hl-test",
		Type:            "perps",
		Platform:        "hyperliquid",
		RegimeATRWindow: "daily",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				composite7StateTier(2.0, 0.5),
				composite7StateTier(4.0, 1.0),
			},
		}},
	}
	if errs := validateRegimeATRConfig(compositeRegimeCfg(sc)); len(errs) != 0 {
		t.Fatalf("composite tiered_tp_atr_regime must validate, got: %v", errs)
	}
}

func TestResolveRegimeTPTiers_CompositeRuntime(t *testing.T) {
	raw := []interface{}{
		composite7StateTier(2.0, 0.5),
		composite7StateTier(4.0, 1.0),
	}
	tiers := resolveRegimeTPTiers(raw, "trending_down_choppy")
	if len(tiers) != 2 {
		t.Fatalf("composite runtime resolve must return 2 tiers, got %d: %v", len(tiers), tiers)
	}
	if tiers[0].Multiple != 2.0 || tiers[1].Multiple != 4.0 {
		t.Fatalf("tier multiples mismatch: %v", tiers)
	}
	if got := resolveRegimeTPTiers(raw, "not_a_label"); got != nil {
		t.Fatalf("unknown runtime regime should resolve to nil, got %v", got)
	}
}

func TestStrategyTPTiersForRegime_CompositeUseDefaults(t *testing.T) {
	sc := StrategyConfig{
		Type:          "perps",
		Platform:      "hyperliquid",
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"use_defaults": true}},
	}
	tiers := strategyTPTiersForRegime(sc, "trending_up_clean")
	if len(tiers) != 4 {
		t.Fatalf("use_defaults composite clean must resolve 4 tiers, got %d: %v", len(tiers), tiers)
	}
	if tiers[0].Multiple != 2.5 || tiers[3].Multiple != 7.0 {
		t.Fatalf("composite use_defaults clean baseline mismatch: %v", tiers)
	}
}

func TestMapRegimeToBaselineFamily(t *testing.T) {
	baseline := regimeATRDefaults.StopLoss
	cases := []struct {
		label   string
		wantATR float64
		wantOK  bool
	}{
		{"trending_up", 2.0, true},
		{"ranging", 1.5, true},
		{"trending_up_clean", 2.0, true},
		{"trending_down_choppy", 2.0, true},
		{"ranging_quiet", 1.5, true},
		{"ranging_directional", 1.5, true},
		{"ranging_directional_up", 1.5, true},
		{"ranging_directional_down", 1.5, true},
		{"garbage", 0, false},
	}
	for _, tc := range cases {
		e, ok := mapRegimeToBaselineFamily(baseline, tc.label)
		if ok != tc.wantOK || (ok && e.ATR != tc.wantATR) {
			t.Errorf("mapRegimeToBaselineFamily(%q) = (%g, %v), want (%g, %v)", tc.label, e.ATR, ok, tc.wantATR, tc.wantOK)
		}
	}

	trailing := regimeATRDefaults.Trailing
	for _, label := range []string{"ranging_directional_up", "ranging_directional_down"} {
		e, ok := mapRegimeToBaselineFamily(trailing, label)
		if !ok || e.ATR != 1.5 {
			t.Errorf("Trailing[%q] = (%g, %v), want (1.5, true) — explicit trail, not the 2.0 ranging family", label, e.ATR, ok)
		}
	}
}

func TestRegimeLabelsFromTierRaw(t *testing.T) {
	raw := []interface{}{composite7StateTier(2.0, 0.5)}
	got := regimeLabelsFromTierRaw(raw)
	if len(got) != 9 {
		t.Fatalf("expected 9 inferred labels, got %d: %v", len(got), got)
	}
	if got := regimeLabelsFromTierRaw(nil); len(got) != 3 {
		t.Fatalf("nil raw should fall back to canonical ADX (3), got %v", got)
	}
}

func TestLoadConfig_CompositeStopLossAtrRegime(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	dbPath := filepath.Join(dir, "state.db")
	cfgBody := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"regime": {
			"enabled": true, "period": 14, "adx_threshold": 20,
			"windows": {
				"daily": {"classifier": "composite", "period": 24, "thresholds": {"return_eff": 0.05, "range_eff": 0.03, "adx": 20}}
			}
		},
		"strategies": [{
			"id": "hl-test",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["donchian_breakout", "BTC", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 25,
			"leverage": 1,
			"regime_atr_window": "daily",
			"stop_loss_atr_mult_regime": {
				"trend_regime": {
					"trending_up_clean": {"atr_multiple": 2.0},
					"trending_up_choppy": {"atr_multiple": 1.2},
					"trending_down_clean": {"atr_multiple": 2.0},
					"trending_down_choppy": {"atr_multiple": 1.2},
					"ranging_quiet": {"atr_multiple": 1.5},
					"ranging_volatile": {"atr_multiple": 1.0},
					"ranging_directional": {"atr_multiple": 1.5},
					"ranging_directional_up": {"atr_multiple": 1.5},
					"ranging_directional_down": {"atr_multiple": 1.5}
				}
			}
		}]
	}`
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig must accept composite 9-state stop_loss_atr_mult_regime, got: %v", err)
	}
	block := cfg.Strategies[0].StopLossATRMultRegime
	if block == nil || len(block.TrendRegime) != 9 {
		t.Fatalf("composite SL block must be populated with 9 labels post-load, got %#v", block)
	}
	if v, ok := resolveRegimeATR(*block, "trending_down_choppy"); !ok || v != 1.2 {
		t.Fatalf("runtime resolve trending_down_choppy = (%g, %v), want (1.2, true)", v, ok)
	}
}

func TestLoadConfig_CompositeSLAfterTrailFromHere(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	dbPath := filepath.Join(dir, "state.db")
	cfgBody := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"regime": {
			"enabled": true, "period": 14, "adx_threshold": 20,
			"windows": {
				"daily": {"classifier": "composite", "period": 24, "thresholds": {"return_eff": 0.05, "range_eff": 0.03, "adx": 20}}
			}
		},
		"strategies": [{
			"id": "hl-test",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["donchian_breakout", "BTC", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 25,
			"leverage": 1,
			"regime_atr_window": "daily",
			"stop_loss_atr_mult": 1.0,
			"close_strategies": [{
				"name": "tiered_tp_atr",
				"params": {
					"sl_after": {
						"trail_from_here": {
							"trend_regime": {
								"trending_up_clean": {"atr_multiple": 0.75},
								"trending_up_choppy": {"atr_multiple": 0.5},
								"trending_down_clean": {"atr_multiple": 0.75},
								"trending_down_choppy": {"atr_multiple": 0.5},
								"ranging_directional": {"atr_multiple": 0.4},
								"ranging_volatile": {"atr_multiple": 0.4},
								"ranging_quiet": {"atr_multiple": 0.3}
							}
						}
					},
					"tp_tiers": [
						{"atr_multiple": 2.0, "close_fraction": 0.5},
						{"atr_multiple": 4.0, "close_fraction": 1.0}
					]
				}
			}]
		}]
	}`
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatalf("LoadConfig must accept composite sl_after trail_from_here, got: %v", err)
	}
}
