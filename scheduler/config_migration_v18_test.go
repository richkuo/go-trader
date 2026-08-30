package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	if sc.TrailingStopATRMultRegime == nil || !sc.TrailingStopATRMultRegime.IsConfigured() {
		t.Fatal("TrailingStopATRMultRegime was not populated from the legacy key")
	}
	if got, ok := resolveRegimeATR(*sc.TrailingStopATRMultRegime, "ranging"); !ok || got != 2.0 {
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
	if _, present := rawSC[v19TrailingStopATRMultRegimeKey]; !present {
		t.Errorf("v19 canonical key %q missing on disk after the v18→v19 chain", v19TrailingStopATRMultRegimeKey)
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
	if sc.TrailingStopATRMultRegime == nil || !sc.TrailingStopATRMultRegime.IsConfigured() {
		t.Fatal("close-default trail block was not injected")
	}
	if got, ok := resolveRegimeATR(*sc.TrailingStopATRMultRegime, "ranging"); !ok || got != 1.25 {
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
	if _, present := regimeATR[v19TrailingStopATRMultRegimeKey]; !present {
		t.Errorf("user_defaults.regime_atr missing %q", v19TrailingStopATRMultRegimeKey)
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
	if _, present := ratchet[v19TrailingStopATRMultRegimeKey]; !present {
		t.Errorf("user_defaults.close entry missing %q", v19TrailingStopATRMultRegimeKey)
	}
}

func TestMigrateV18TrailStopATRRegimeKey(t *testing.T) {
	t.Run("conflicting_blocks_rejected_naming_both_keys", func(t *testing.T) {
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
	})
	t.Run("identical_blocks_drop_redundant_legacy_key", func(t *testing.T) {
		raw := map[string]interface{}{
			"strategies": []interface{}{
				map[string]interface{}{
					"id":                        "hl-eth",
					legacyTrailStopATRRegimeKey: map[string]interface{}{"use_defaults": true},
					trailStopATRRegimeKey:       map[string]interface{}{"use_defaults": true},
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
	})
}

func TestNeedsV18TrailStopKeyMigration(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"v18_stamped_still_carrying_legacy_key", `{"config_version": 18, "strategies": [{"id": "a", "trailing_stop_atr_regime": {"use_defaults": true}}]}`, true},
		{"v18_stamped_on_canonical_key", `{"config_version": 18, "strategies": [{"id": "a", "trail_stop_atr_regime": {"use_defaults": true}}]}`, false},
		{"sub_v18_with_no_legacy_key", v18CleanStrategyConfigJSON, false},
		{"sub_v18_carrying_legacy_key", v18LegacyStrategyConfigJSON, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsV18TrailStopKeyMigration([]byte(tc.data)); got != tc.want {
				t.Errorf("needsV18TrailStopKeyMigration = %v, want %v (keyed on the legacy key, never the version)", got, tc.want)
			}
		})
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

func TestMigrationBaseVersionSurvivesLoadTimeMigration(t *testing.T) {
	for _, onDisk := range []int{13, 14, 15, 16, 17} {
		t.Run(fmt.Sprintf("v%d", onDisk), func(t *testing.T) {
			cfgJSON := strings.Replace(v18LegacyStrategyConfigJSON,
				`"config_version": 17,`, fmt.Sprintf(`"config_version": %d,`, onDisk), 1)
			path := writeTestConfig(t, t.TempDir(), cfgJSON)
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.ConfigVersion != CurrentConfigVersion {
				t.Fatalf("ConfigVersion = %d, want %d after the load-time migration", cfg.ConfigVersion, CurrentConfigVersion)
			}
			if got := cfg.MigrationBaseVersion(); got != onDisk {
				t.Fatalf("MigrationBaseVersion() = %d, want %d — main.go gates the operator notices on it, so a load-time rewrite must not erase where the operator came from", got, onDisk)
			}
			if got := configMigrationNotices(cfg.MigrationBaseVersion()); len(got) == 0 {
				t.Fatalf("v%d -> v%d delivered no migration notice", onDisk, CurrentConfigVersion)
			}
		})
	}
}

func TestMigrationBaseVersionCurrentConfigSendsNoNotice(t *testing.T) {
	clean := strings.Replace(
		strings.Replace(v18LegacyStrategyConfigJSON, `"config_version": 17,`, `"config_version": 19,`, 1),
		legacyTrailStopATRRegimeKey, v19TrailingStopATRMultRegimeKey, 1)
	path := writeTestConfig(t, t.TempDir(), clean)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.MigrationBaseVersion(); got != CurrentConfigVersion {
		t.Fatalf("MigrationBaseVersion() = %d, want %d", got, CurrentConfigVersion)
	}
	if got := configMigrationNotices(cfg.MigrationBaseVersion()); len(got) != 0 {
		t.Fatalf("a current-version config produced %d notice(s), want 0", len(got))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("a clean v18 config must not be rewritten on load")
	}
}

func TestMigrationBaseVersionLegacyKeyAtCurrentVersionSendsNoNotice(t *testing.T) {
	stamped := strings.Replace(v18LegacyStrategyConfigJSON, `"config_version": 17,`, `"config_version": 19,`, 1)
	path := writeTestConfig(t, t.TempDir(), stamped)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Strategies[0].TrailingStopATRMultRegime == nil || !cfg.Strategies[0].TrailingStopATRMultRegime.IsConfigured() {
		t.Fatal("legacy key at v19 was not adopted into TrailingStopATRMultRegime")
	}
	raw := readRawConfig(t, path)
	if _, present := rawStrategy(t, raw, 0)[trailStopATRRegimeKey]; present {
		t.Error("v18-canonical key survived the v19 rename on a v19-stamped config")
	}
	if _, present := rawStrategy(t, raw, 0)[v19TrailingStopATRMultRegimeKey]; !present {
		t.Error("v19 canonical key missing after in-place rename")
	}
	if got := configMigrationNotices(cfg.MigrationBaseVersion()); len(got) != 0 {
		t.Fatalf("a v19-stamped config renamed in place produced %d notice(s) — that would repeat every restart", len(got))
	}
}

func TestConfigMigrationNoticesAreOrderedAndVersionScoped(t *testing.T) {
	if got := configMigrationNotices(18); len(got) != 1 || got[0] != v19AtrMultRenameNotice {
		t.Fatalf("v18 base notices = %d entries, want only the v19 notice", len(got))
	}
	got := configMigrationNotices(13)
	want := []string{v14DeprecationNotice, v15DeprecationNotice, v17ATRMethodNotice, v18TrailStopRenameNotice, v19AtrMultRenameNotice}
	if len(got) != len(want) {
		t.Fatalf("v13 base notices = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("notice[%d] out of order", i)
		}
	}
	if n := len(configMigrationNotices(CurrentConfigVersion)); n != 0 {
		t.Fatalf("current-version base produced %d notices, want 0", n)
	}
}

func TestMigrationBaseVersionVersionlessConfigIsTreatedAsCurrentShape(t *testing.T) {
	versionless := strings.Replace(v18LegacyStrategyConfigJSON, "\t\"config_version\": 17,\n", "", 1)
	if strings.Contains(versionless, "config_version") {
		t.Fatal("fixture still stamps a config_version")
	}
	path := writeTestConfig(t, t.TempDir(), versionless)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.MigrationBaseVersion(); got != CurrentConfigVersion {
		t.Fatalf("MigrationBaseVersion() = %d, want %d — #1285 treats a version-less config as current-shape, so it must not be handed the v14/v15 upgrade notices", got, CurrentConfigVersion)
	}
	if n := len(configMigrationNotices(cfg.MigrationBaseVersion())); n != 0 {
		t.Fatalf("a version-less config produced %d notice(s), want 0", n)
	}
}

const v18CleanStrategyConfigJSON = `{
	"config_version": 17,
	"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
	"strategies": [{
		"id": "hl-eth-clean",
		"type": "perps",
		"platform": "hyperliquid",
		"script": "shared_scripts/check_hyperliquid.py",
		"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
		"capital": 1000,
		"leverage": 3,
		"stop_loss_atr_mult": 1.5
	}]
}`

func readOnlyConfigDir(t *testing.T, body string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions are not enforced")
	}
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := os.WriteFile(filepath.Join(dir, "probe.tmp"), []byte("x"), 0o600); err == nil {
		t.Skip("directory permissions are not enforced on this filesystem")
	}
	return path
}

func TestLoadConfigCleanConfigNeedsNoWriteToLoad(t *testing.T) {
	path := readOnlyConfigDir(t, v18CleanStrategyConfigJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a sub-v18 config carrying no legacy key must load without rewriting the file, so a non-writable config path is not a boot blocker: %v", err)
	}
	if got := cfg.MigrationBaseVersion(); got != 17 {
		t.Fatalf("MigrationBaseVersion() = %d, want 17 — the startup notice still has to fire", got)
	}
	if n := len(configMigrationNotices(cfg.MigrationBaseVersion())); n == 0 {
		t.Error("a v17 config still owes the operator the v18 notice")
	}
}

func TestLoadConfigLegacyKeyStillFailsLoudWhenItCannotBeRewritten(t *testing.T) {
	path := readOnlyConfigDir(t, v18LegacyStrategyConfigJSON)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("a config carrying the legacy key must fail the load when the rename cannot be persisted — loading it would leave the on-disk key contradicting the running shape")
	}
}
