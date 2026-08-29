package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const v18LegacyStrategyConfigJSON = `{
	"config_version": 17,
	"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
	"strategies": [{
		"id": "hl-eth-legacy-trail",
		"type": "perps",
		"platform": "hyperliquid",
		"script": "shared_scripts/check_hyperliquid.py",
		"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
		"capital": 1000,
		"leverage": 3,
		"trailing_stop_atr_regime": {
			"trend_regime": {
				"trending_up": {"atr_multiple": 2.5},
				"trending_down": {"atr_multiple": 2.5},
				"ranging": {"atr_multiple": 2.0}
			}
		}
	}]
}`

func readRawConfig(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return raw
}

func rawStrategy(t *testing.T, raw map[string]interface{}, idx int) map[string]interface{} {
	t.Helper()
	list, ok := raw["strategies"].([]interface{})
	if !ok || len(list) <= idx {
		t.Fatalf("strategies[%d] missing in %v", idx, raw)
	}
	sc, ok := list[idx].(map[string]interface{})
	if !ok {
		t.Fatalf("strategies[%d] is not an object", idx)
	}
	return sc
}

func TestLoadConfigV18MigratesLegacyStrategyTrailKeyBeforeUnknownKeyValidation(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, v18LegacyStrategyConfigJSON)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig with only the legacy key failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.TrailStopATRRegime == nil || !sc.TrailStopATRRegime.IsConfigured() {
		t.Fatal("TrailStopATRRegime was not populated from the legacy key")
	}
	if got, ok := resolveRegimeATR(*sc.TrailStopATRRegime, "ranging"); !ok || got != 2.0 {
		t.Fatalf("ranging trail = (%g, %v), want (2.0, true)", got, ok)
	}

	raw := readRawConfig(t, path)
	if v, _ := raw["config_version"].(float64); int(v) != CurrentConfigVersion {
		t.Fatalf("config_version on disk = %v, want %d", raw["config_version"], CurrentConfigVersion)
	}
	rawSC := rawStrategy(t, raw, 0)
	if _, present := rawSC[legacyTrailStopATRRegimeKey]; present {
		t.Errorf("legacy key %q still on disk after migration", legacyTrailStopATRRegimeKey)
	}
	if _, present := rawSC[trailStopATRRegimeKey]; !present {
		t.Errorf("canonical key %q missing on disk after migration", trailStopATRRegimeKey)
	}
}

func TestLoadConfigV18MigratesLegacyUserDefaultKeys(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"config_version": 17,
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_defaults": {
			"regime_atr": {
				"trailing_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 2.4},
						"trending_down": {"atr_multiple": 2.4},
						"ranging": {"atr_multiple": 1.4}
					}
				}
			},
			"close": {
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
	path := writeTestConfig(t, dir, cfgJSON)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig with legacy user_defaults keys failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.TrailStopATRRegime == nil || !sc.TrailStopATRRegime.IsConfigured() {
		t.Fatal("close-default trail block was not injected")
	}
	if got, ok := resolveRegimeATR(*sc.TrailStopATRRegime, "ranging"); !ok || got != 1.25 {
		t.Fatalf("ranging trail = (%g, %v), want (1.25, true) from user_defaults.close", got, ok)
	}

	raw := readRawConfig(t, path)
	ud, ok := raw["user_defaults"].(map[string]interface{})
	if !ok {
		t.Fatal("user_defaults missing after migration")
	}
	regimeATR, ok := ud["regime_atr"].(map[string]interface{})
	if !ok {
		t.Fatal("user_defaults.regime_atr missing after migration")
	}
	if _, present := regimeATR[legacyTrailStopATRRegimeKey]; present {
		t.Errorf("user_defaults.regime_atr still carries %q", legacyTrailStopATRRegimeKey)
	}
	if _, present := regimeATR[trailStopATRRegimeKey]; !present {
		t.Errorf("user_defaults.regime_atr missing %q", trailStopATRRegimeKey)
	}
	closes, ok := ud["close"].(map[string]interface{})
	if !ok {
		t.Fatal("user_defaults.close missing after migration")
	}
	ratchet, ok := closes["trailing_tp_ratchet_regime"].(map[string]interface{})
	if !ok {
		t.Fatal("user_defaults.close[trailing_tp_ratchet_regime] missing after migration")
	}
	if _, present := ratchet[legacyTrailStopATRRegimeKey]; present {
		t.Errorf("user_defaults.close entry still carries %q", legacyTrailStopATRRegimeKey)
	}
	if _, present := ratchet[trailStopATRRegimeKey]; !present {
		t.Errorf("user_defaults.close entry missing %q", trailStopATRRegimeKey)
	}
}

func TestMigrateV18RejectsConflictingTrailKeys(t *testing.T) {
	raw := map[string]interface{}{
		"strategies": []interface{}{
			map[string]interface{}{
				"id":                        "hl-eth",
				legacyTrailStopATRRegimeKey: map[string]interface{}{"use_defaults": true},
				trailStopATRRegimeKey:       map[string]interface{}{"trend_regime": map[string]interface{}{"ranging": map[string]interface{}{"atr_multiple": 1.0}}},
			},
		},
	}
	err := migrateV18TrailStopATRRegimeKey(raw)
	if err == nil {
		t.Fatal("expected conflict error when both spellings carry different blocks")
	}
	if !strings.Contains(err.Error(), legacyTrailStopATRRegimeKey) || !strings.Contains(err.Error(), trailStopATRRegimeKey) {
		t.Errorf("conflict error should name both keys, got: %v", err)
	}
}

func TestMigrateV18DropsRedundantLegacyTrailKey(t *testing.T) {
	block := map[string]interface{}{"use_defaults": true}
	raw := map[string]interface{}{
		"strategies": []interface{}{
			map[string]interface{}{
				"id":                        "hl-eth",
				legacyTrailStopATRRegimeKey: map[string]interface{}{"use_defaults": true},
				trailStopATRRegimeKey:       block,
			},
		},
	}
	if err := migrateV18TrailStopATRRegimeKey(raw); err != nil {
		t.Fatalf("identical blocks should migrate cleanly, got: %v", err)
	}
	sc := raw["strategies"].([]interface{})[0].(map[string]interface{})
	if _, present := sc[legacyTrailStopATRRegimeKey]; present {
		t.Error("redundant legacy key was not removed")
	}
	if got := sc[trailStopATRRegimeKey]; got == nil {
		t.Error("canonical key was dropped")
	}
}

func TestNeedsV18TrailStopKeyMigrationDetectsLegacyKeyAtCurrentVersion(t *testing.T) {
	current := []byte(`{"config_version": 18, "strategies": [{"id": "a", "trailing_stop_atr_regime": {"use_defaults": true}}]}`)
	if !needsV18TrailStopKeyMigration(current) {
		t.Error("a v18-stamped config still carrying the legacy key must migrate")
	}
	clean := []byte(`{"config_version": 18, "strategies": [{"id": "a", "trail_stop_atr_regime": {"use_defaults": true}}]}`)
	if needsV18TrailStopKeyMigration(clean) {
		t.Error("a v18 config on the canonical key must not migrate again")
	}
}

func TestV18RenameKeepsTrailingStopTriggerPct(t *testing.T) {
	dir := t.TempDir()
	legacyCfg, err := LoadConfig(writeTestConfig(t, dir, v18LegacyStrategyConfigJSON))
	if err != nil {
		t.Fatalf("LoadConfig(legacy key): %v", err)
	}
	renamedCfg, err := LoadConfig(writeTestConfig(t, t.TempDir(),
		strings.Replace(v18LegacyStrategyConfigJSON, legacyTrailStopATRRegimeKey, trailStopATRRegimeKey, 1)))
	if err != nil {
		t.Fatalf("LoadConfig(canonical key): %v", err)
	}

	for _, label := range []string{"trending_up", "trending_down", "ranging"} {
		pos := &Position{AvgCost: 2000, EntryATR: 40, Quantity: 1, Side: "long", Regime: label}
		legacyPct := effectiveTrailingStopPct(legacyCfg.Strategies[0], pos)
		renamedPct := effectiveTrailingStopPct(renamedCfg.Strategies[0], pos)
		if legacyPct <= 0 {
			t.Fatalf("%s: legacy config resolved no trailing stop", label)
		}
		if legacyPct != renamedPct {
			t.Errorf("%s: trailing stop pct legacy=%g renamed=%g — the rename must not move the trigger", label, legacyPct, renamedPct)
		}
	}
}

func TestPromotionBaselineIgnoresTrailStopKeySpelling(t *testing.T) {
	legacy := tuningPromotionBaseline{
		UserDefaultsPresent: true,
		UserDefaults:        json.RawMessage(`{"regime_atr": {"trailing_stop_atr_regime": {"use_defaults": true}}}`),
	}
	renamed := tuningPromotionBaseline{
		UserDefaultsPresent: true,
		UserDefaults:        json.RawMessage(`{"regime_atr": {"trail_stop_atr_regime": {"use_defaults": true}}}`),
	}
	if !promotionBaselinesEqual(legacy, renamed) {
		t.Error("a baseline recorded under the legacy key must not read as drift after the v18 rename")
	}

	different := tuningPromotionBaseline{
		UserDefaultsPresent: true,
		UserDefaults:        json.RawMessage(`{"regime_atr": {"trail_stop_atr_regime": {"trend_regime": {"ranging": {"atr_multiple": 1.0}}}}}`),
	}
	if promotionBaselinesEqual(legacy, different) {
		t.Error("normalizing the key must not mask a genuine baseline change")
	}
}
