package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNewFieldsSince(t *testing.T) {
	// #1285: the v2/v3 registry entries were removed with the migration floor —
	// every supported config (v13+) already carries those fields. The registry
	// stays empty until a future version introduces an operator-set field.
	cases := []int{0, 1, MinSupportedConfigVersion, CurrentConfigVersion, 999}
	for _, version := range cases {
		fields := NewFieldsSince(version)
		if len(fields) != 0 {
			t.Errorf("NewFieldsSince(%d) returned %d fields, want 0 (registry emptied by #1285 floor)", version, len(fields))
		}
		// Any future registry entry must have Version > the queried version.
		for _, f := range fields {
			if f.Version <= version {
				t.Errorf("NewFieldsSince(%d) returned field %q with version %d", version, f.JSONPath, f.Version)
			}
		}
	}
}

// TestMinSupportedConfigVersionFloor pins the floor and its relation to the
// current version — a floor above CurrentConfigVersion would reject every
// config, and a floor below 13 would reference deleted handlers.
func TestMinSupportedConfigVersionFloor(t *testing.T) {
	if MinSupportedConfigVersion != 13 {
		t.Errorf("MinSupportedConfigVersion = %d, want 13 — raising the floor requires fresh fleet verification (#1285)", MinSupportedConfigVersion)
	}
	if MinSupportedConfigVersion > CurrentConfigVersion {
		t.Errorf("MinSupportedConfigVersion (%d) > CurrentConfigVersion (%d)", MinSupportedConfigVersion, CurrentConfigVersion)
	}
}

func TestNewFieldsSinceFieldProperties(t *testing.T) {
	fields := NewFieldsSince(0)
	for _, f := range fields {
		if f.JSONPath == "" {
			t.Error("field has empty JSONPath")
		}
		if f.Description == "" {
			t.Errorf("field %q has empty Description", f.JSONPath)
		}
		if f.FieldType == "" {
			t.Errorf("field %q has empty FieldType", f.JSONPath)
		}
		if f.Version <= 0 {
			t.Errorf("field %q has invalid version %d", f.JSONPath, f.Version)
		}
	}
}

func TestMigrateConfigBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Version-less config (no config_version stamp): treated as a hand-authored
	// current-shape file — migrates and stamps CurrentConfigVersion (#1285).
	original := map[string]interface{}{
		"interval_seconds": 300,
		"strategies":       []interface{}{},
	}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	values := map[string]string{
		"discord.owner_id": "12345",
	}
	if err := MigrateConfig(path, values, nil); err != nil {
		t.Fatalf("MigrateConfig failed: %v", err)
	}

	// Read back
	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(result, &updated); err != nil {
		t.Fatal(err)
	}

	// Version should be bumped
	version := int(updated["config_version"].(float64))
	if version != CurrentConfigVersion {
		t.Errorf("config_version = %d, want %d", version, CurrentConfigVersion)
	}

	// Check nested field was set
	discord, ok := updated["discord"].(map[string]interface{})
	if !ok {
		t.Fatal("discord section missing")
	}
	if discord["owner_id"] != "12345" {
		t.Errorf("discord.owner_id = %v, want %q", discord["owner_id"], "12345")
	}

	// Original fields should be preserved
	if updated["interval_seconds"].(float64) != 300 {
		t.Error("interval_seconds should be preserved")
	}
	// The v12 default_stop_loss_atr_mult on-disk backfill was removed with the
	// #1285 floor; LoadConfig's nil→DefaultStopLossATRMult runtime default
	// keeps behavior identical without the disk write.
	if _, ok := updated["default_stop_loss_atr_mult"]; ok {
		t.Error("default_stop_loss_atr_mult should no longer be backfilled on disk (#1285)")
	}
}

func TestMigrateConfigCreatesNestedPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := map[string]interface{}{"interval_seconds": 300}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	values := map[string]string{
		"discord.dm_channels.hyperliquid": "999888777",
	}
	if err := MigrateConfig(path, values, nil); err != nil {
		t.Fatal(err)
	}

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(result, &updated); err != nil {
		t.Fatal(err)
	}

	discord := updated["discord"].(map[string]interface{})
	dmCh := discord["dm_channels"].(map[string]interface{})
	if dmCh["hyperliquid"] != "999888777" {
		t.Errorf("discord.dm_channels.hyperliquid = %v, want %q", dmCh["hyperliquid"], "999888777")
	}
}

func TestMigrateConfigNilValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := map[string]interface{}{"interval_seconds": 300}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	// nil values — just bump version
	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatal(err)
	}

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(result, &updated); err != nil {
		t.Fatal(err)
	}

	version := int(updated["config_version"].(float64))
	if version != CurrentConfigVersion {
		t.Errorf("config_version = %d, want %d", version, CurrentConfigVersion)
	}
}

func TestMigrateConfigAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := map[string]interface{}{"interval_seconds": 300}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatal(err)
	}

	// tmp file should not remain
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should not exist after migration")
	}
}

func TestMigrateConfigMissingFile(t *testing.T) {
	err := MigrateConfig("/nonexistent/config.json", nil, nil)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestMigrateConfigInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}

	err := MigrateConfig(path, nil, nil)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestSetNestedField(t *testing.T) {
	obj := make(map[string]interface{})

	setNestedField(obj, "top_level", "value1")
	if obj["top_level"] != "value1" {
		t.Errorf("top_level = %v, want %q", obj["top_level"], "value1")
	}

	setNestedField(obj, "nested.field", "value2")
	nested := obj["nested"].(map[string]interface{})
	if nested["field"] != "value2" {
		t.Errorf("nested.field = %v, want %q", nested["field"], "value2")
	}

	setNestedField(obj, "deep.nested.field", "value3")
	deep := obj["deep"].(map[string]interface{})
	deepNested := deep["nested"].(map[string]interface{})
	if deepNested["field"] != "value3" {
		t.Errorf("deep.nested.field = %v, want %q", deepNested["field"], "value3")
	}
}

// writeRawConfig marshals obj to path and returns the on-disk bytes, for the
// #1285 rejection tests that assert the file is left byte-identical.
func writeRawConfig(t *testing.T, path string, obj map[string]interface{}) []byte {
	t.Helper()
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

// assertUnsupportedVersionError asserts the #1285 fail-loud rejection: the
// error names the stamped version, the floor, and the migration path, and the
// config file on disk is byte-identical (no partial migration).
func assertUnsupportedVersionError(t *testing.T, err error, version int, path string, original []byte) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected unsupported-config_version error for version %d, got nil", version)
	}
	msg := err.Error()
	for _, want := range []string{
		fmt.Sprintf("config_version %d is no longer supported", version),
		fmt.Sprintf("minimum %d", MinSupportedConfigVersion),
		"go-trader.prev",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, original) {
		t.Errorf("config file was modified despite rejection — partial migration:\n%s", after)
	}
}

// TestMigrateConfigRejectsPreFloorVersions covers the #1285 floor: every
// stamped version below MinSupportedConfigVersion fails loudly with an
// actionable message and leaves the file untouched. Fixtures carry the legacy
// fields the deleted v6–v12 handlers used to rewrite, proving nothing is
// silently dropped or translated on the rejection path.
func TestMigrateConfigRejectsPreFloorVersions(t *testing.T) {
	cases := []struct {
		name    string
		version int
		extra   map[string]interface{}
	}{
		{
			// Pre-v6 channel booleans (deleted v6 handler).
			name:    "v5_channel_booleans",
			version: 5,
			extra: map[string]interface{}{
				"discord": map[string]interface{}{
					"enabled":              true,
					"channel_paper_trades": true,
					"channel_live_trades":  true,
				},
			},
		},
		{
			// Pre-v7 dm booleans (deleted v7 dm_channels translation).
			name:    "v6_dm_booleans",
			version: 6,
			extra: map[string]interface{}{
				"discord": map[string]interface{}{
					"owner_id":        "disc-owner",
					"dm_paper_trades": true,
					"dm_live_trades":  false,
				},
			},
		},
		{
			// Pre-v8 dead summary-freq fields (deleted v8 cleanup).
			name:    "v7_summary_freq",
			version: 7,
			extra: map[string]interface{}{
				"discord": map[string]interface{}{
					"spot_summary_freq":    "hourly",
					"options_summary_freq": "per_check",
				},
			},
		},
		{
			// Pre-v10 perps leverage without sizing_leverage (deleted v10
			// backfill). Runtime stays identical: EffectiveSizingLeverage
			// falls back to Leverage when sizing_leverage is unset.
			name:    "v9_sizing_leverage",
			version: 9,
			extra: map[string]interface{}{
				"strategies": []interface{}{
					map[string]interface{}{"id": "hl-eth", "type": "perps", "leverage": float64(2)},
				},
			},
		},
		{
			// Last version below the floor.
			name:    "v12_boundary",
			version: 12,
			extra:   map[string]interface{}{},
		},
		{
			name:    "v1_ancient",
			version: 1,
			extra:   map[string]interface{}{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			obj := map[string]interface{}{"config_version": tc.version}
			for k, v := range tc.extra {
				obj[k] = v
			}
			original := writeRawConfig(t, path, obj)
			err := MigrateConfig(path, nil, nil)
			assertUnsupportedVersionError(t, err, tc.version, path, original)
		})
	}
}

// TestMigrateConfigAtFloorNeverStripsLegacyNames — supported configs (v13+)
// keep fields that happen to match the pre-floor deprecated names; the
// deleted v6/v8 removal handlers must not resurface at any supported version.
func TestMigrateConfigAtFloorNeverStripsLegacyNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeRawConfig(t, path, map[string]interface{}{
		"config_version": MinSupportedConfigVersion,
		"discord": map[string]interface{}{
			"channel_paper_trades": true,
			"channel_live_trades":  true,
			"spot_summary_freq":    "hourly",
		},
	})
	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatalf("MigrateConfig failed: %v", err)
	}
	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(result, &updated); err != nil {
		t.Fatal(err)
	}
	if v := int(updated["config_version"].(float64)); v != CurrentConfigVersion {
		t.Errorf("config_version = %d, want %d", v, CurrentConfigVersion)
	}
	discord := updated["discord"].(map[string]interface{})
	for _, key := range []string{"channel_paper_trades", "channel_live_trades", "spot_summary_freq"} {
		if _, ok := discord[key]; !ok {
			t.Errorf("discord.%s should NOT be removed for a supported config", key)
		}
	}
}

// assertVersionlessDMKeyError asserts the #1285-review version-less rejection:
// the error names the offending legacy key, explains the missing config_version
// stamp, points at dm_channels + the go-trader.prev recovery path, and (when a
// path/original is supplied) the on-disk file is byte-identical — never
// partially migrated.
func assertVersionlessDMKeyError(t *testing.T, err error, key, path string, original []byte) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected version-less removed-translation-key rejection for %q, got nil", key)
	}
	msg := err.Error()
	for _, want := range []string{key, "no config_version stamp", "dm_channels", "go-trader.prev"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if path == "" {
		return
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, original) {
		t.Errorf("config file was modified despite rejection — partial migration:\n%s", after)
	}
}

// TestVersionlessConfigRejectsRemovedDMTranslationKeys covers the #1285-review
// gap: a version-less config (no config_version stamp) is treated as
// current-shape and no longer runs the deleted v7 dm_paper_trades/dm_live_trades
// → dm_channels translation. That translation has no runtime substitute, so a
// version-less file still carrying either key must be rejected loudly at the raw
// gate, at MigrateConfig, and at LoadConfig — never silently loaded with DM
// routing dropped, never partially migrated. Covers both the discord and
// telegram sections and both booleans, each with a populated owner target (the
// realistic silent-loss scenario from the review).
func TestVersionlessConfigRejectsRemovedDMTranslationKeys(t *testing.T) {
	cases := []struct {
		name    string
		section string
		key     string
	}{
		{"discord_dm_paper_trades", "discord", "dm_paper_trades"},
		{"discord_dm_live_trades", "discord", "dm_live_trades"},
		{"telegram_dm_paper_trades", "telegram", "dm_paper_trades"},
		{"telegram_dm_live_trades", "telegram", "dm_live_trades"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			ownerKey := "owner_id"
			if tc.section == "telegram" {
				ownerKey = "owner_chat_id"
			}
			obj := map[string]interface{}{
				// No config_version key — version-less.
				"interval_seconds": 3600,
				tc.section: map[string]interface{}{
					ownerKey: "owner-target",
					tc.key:   true,
				},
			}
			original := writeRawConfig(t, path, obj)
			fullKey := tc.section + "." + tc.key

			// Raw pre-migration gate rejects (no file to check).
			assertVersionlessDMKeyError(t, checkRawConfigVersionSupported(original), fullKey, "", nil)
			// MigrateConfig rejects before any rewrite — file byte-identical.
			assertVersionlessDMKeyError(t, MigrateConfig(path, nil, nil), fullKey, path, original)
			// LoadConfig rejects — the daemon never starts on it, file untouched.
			_, loadErr := LoadConfig(path)
			assertVersionlessDMKeyError(t, loadErr, fullKey, path, original)
		})
	}
}

// TestVersionlessConfigRemovedTranslationKeyAcceptance pins the negative side of
// the #1285-review guard: it must fire ONLY on the removed v7 dm_* booleans and
// never on (a) the inert v6 channel_* / v8 summary-freq keys [review must-survive
// b], (b) the CURRENT dm_channels map — whose key merely shares the "dm_" prefix,
// (c) a clean version-less config with no legacy keys [review must-survive c], or
// (d) a stamped (v13+) config carrying a stray dm_* key, which the version-floor
// path owns, not this guard.
func TestVersionlessConfigRemovedTranslationKeyAcceptance(t *testing.T) {
	cases := []struct {
		name string
		obj  map[string]interface{}
	}{
		{
			name: "versionless_inert_channel_and_summary_keys",
			obj: map[string]interface{}{
				"discord": map[string]interface{}{
					"channel_paper_trades": true,
					"channel_live_trades":  true,
					"spot_summary_freq":    "hourly",
				},
			},
		},
		{
			name: "versionless_current_dm_channels_map",
			obj: map[string]interface{}{
				"discord": map[string]interface{}{
					"dm_channels": map[string]interface{}{"hyperliquid-paper": "disc-owner"},
				},
			},
		},
		{
			name: "versionless_clean_no_discord",
			obj:  map[string]interface{}{"interval_seconds": 3600},
		},
		{
			name: "stamped_config_with_stray_dm_key",
			obj: map[string]interface{}{
				"config_version": CurrentConfigVersion,
				"discord":        map[string]interface{}{"dm_paper_trades": true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.MarshalIndent(tc.obj, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if key, found := versionlessConfigRemovedTranslationKey(data); found {
				t.Errorf("versionlessConfigRemovedTranslationKey flagged %q — %s should be accepted", key, tc.name)
			}
			if err := checkRawConfigVersionSupported(data); err != nil {
				t.Errorf("checkRawConfigVersionSupported rejected %s: %v", tc.name, err)
			}
		})
	}
}

// TestVersionlessConfigWithInertPreFloorKeysMigrates is the migration-path proof
// of review must-survive (b): a version-less config carrying the inert v6
// channel_* / v8 summary-freq keys migrates cleanly to CurrentConfigVersion and
// RETAINS those keys on disk (the runtime ignores them) — the #1285-review guard
// never trips on them, and nothing is stripped or translated.
func TestVersionlessConfigWithInertPreFloorKeysMigrates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeRawConfig(t, path, map[string]interface{}{
		// No config_version key — version-less.
		"interval_seconds": 3600,
		"strategies":       []interface{}{},
		"discord": map[string]interface{}{
			"owner_id":             "disc-owner",
			"channel_paper_trades": true,
			"channel_live_trades":  true,
			"spot_summary_freq":    "hourly",
		},
	})
	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatalf("MigrateConfig rejected a version-less config with inert pre-floor keys: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(after, &updated); err != nil {
		t.Fatal(err)
	}
	if v := int(updated["config_version"].(float64)); v != CurrentConfigVersion {
		t.Errorf("config_version = %d, want %d", v, CurrentConfigVersion)
	}
	discord := updated["discord"].(map[string]interface{})
	for _, key := range []string{"channel_paper_trades", "channel_live_trades", "spot_summary_freq"} {
		if _, ok := discord[key]; !ok {
			t.Errorf("discord.%s should be retained (inert) for a version-less config", key)
		}
	}
}

// TestMigrateConfigV8PreservesFieldsAtCurrentVersion verifies the version
// guard — a config already at the current version must not have the v8
// deprecated fields stripped if a user intentionally reintroduced them.
func TestMigrateConfigV8PreservesFieldsAtCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := map[string]interface{}{
		"config_version": CurrentConfigVersion,
		"discord": map[string]interface{}{
			"spot_summary_freq": "hourly",
		},
	}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatal(err)
	}
	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]interface{}
	if err := json.Unmarshal(result, &updated); err != nil {
		t.Fatal(err)
	}
	discord := updated["discord"].(map[string]interface{})
	if _, ok := discord["spot_summary_freq"]; !ok {
		t.Error("discord.spot_summary_freq should NOT be removed when already at CurrentConfigVersion")
	}
}

func TestJSONBoolish(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want bool
	}{
		{"bool true", true, true},
		{"bool false", false, false},
		{"string true", "true", true},
		{"string True", "True", true},
		{"string TRUE", "TRUE", true},
		{"string 1", "1", true},
		{"string false", "false", false},
		{"string 0", "0", false},
		{"string empty", "", false},
		{"string whitespace true", "  true  ", true},
		{"float64 nonzero", float64(1.5), true},
		{"float64 negative", float64(-1), true},
		{"float64 zero", float64(0), false},
		{"nil", nil, false},
		{"int", int(1), false},
		{"slice", []interface{}{}, false},
		{"map", map[string]interface{}{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jsonBoolish(tc.in)
			if got != tc.want {
				t.Errorf("jsonBoolish(%#v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestStringFromJSON(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"nil", nil, ""},
		{"string trimmed", "  hello  ", "hello"},
		{"string empty", "", ""},
		{"int", int(123), "123"},
		{"float64", float64(1.5), "1.5"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stringFromJSON(tc.in)
			if got != tc.want {
				t.Errorf("stringFromJSON(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCloneOrNewJSONMap(t *testing.T) {
	t.Run("nil returns empty non-nil map", func(t *testing.T) {
		got := cloneOrNewJSONMap(nil)
		if got == nil {
			t.Error("expected non-nil map, got nil")
		}
		if len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})
	t.Run("non-map string returns empty non-nil map", func(t *testing.T) {
		got := cloneOrNewJSONMap("hello")
		if got == nil || len(got) != 0 {
			t.Errorf("expected empty non-nil map, got %v", got)
		}
	})
	t.Run("non-map int returns empty non-nil map", func(t *testing.T) {
		got := cloneOrNewJSONMap(42)
		if got == nil || len(got) != 0 {
			t.Errorf("expected empty non-nil map, got %v", got)
		}
	})
	t.Run("populated map is shallow copy", func(t *testing.T) {
		orig := map[string]interface{}{"a": "alpha", "b": float64(2)}
		got := cloneOrNewJSONMap(orig)
		if got["a"] != "alpha" || got["b"] != float64(2) {
			t.Errorf("clone missing expected values: %v", got)
		}
		// Mutating clone must not affect original.
		got["a"] = "changed"
		if orig["a"] != "alpha" {
			t.Error("mutating clone affected original")
		}
		// Adding a new key to clone must not affect original.
		got["c"] = "new"
		if _, ok := orig["c"]; ok {
			t.Error("new key in clone leaked to original")
		}
	})
}

// TestMigrateV13StrategyShape covers the v12→v13 schema migration: legacy flat
// open_strategy/close_strategies/params get rewritten to co-located refs, with
// close-owned legacy keys (registered in closeStrategyOwnedKeys) routed to the
// matching close ref. This is #640's structural migration.
func TestMigrateV13StrategyShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Version-less config (post-#1285 the only consumer of the v13 shape
	// synthesis — stamped pre-v13 configs are rejected at the floor):
	// tema_cross_bd open + tiered_tp_atr close, with legacy `tiers` in flat
	// params (the bug that motivated #640).
	original := map[string]interface{}{
		"strategies": []interface{}{
			map[string]interface{}{
				"id":               "hl-temacb-btc",
				"type":             "perps",
				"platform":         "hyperliquid",
				"args":             []interface{}{"tema_cross_bd", "BTC", "1h", "--mode=paper"},
				"open_strategy":    "tema_cross_bd",
				"close_strategies": []interface{}{"tiered_tp_atr"},
				"params": map[string]interface{}{
					"tp_tiers": []interface{}{
						map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
						map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
					},
					"short_period": 5.0,
				},
			},
			map[string]interface{}{
				// Legacy: open_strategy empty → migration falls back to args[0].
				"id":       "hl-rsi-eth",
				"type":     "perps",
				"platform": "hyperliquid",
				"args":     []interface{}{"rsi", "ETH", "1h", "--mode=paper"},
			},
		},
	}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatalf("MigrateConfig: %v", err)
	}

	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(migrated, &got); err != nil {
		t.Fatal(err)
	}

	if int(got["config_version"].(float64)) != CurrentConfigVersion {
		t.Errorf("config_version = %v, want %d", got["config_version"], CurrentConfigVersion)
	}

	strategies := got["strategies"].([]interface{})
	if len(strategies) != 2 {
		t.Fatalf("strategies len = %d, want 2", len(strategies))
	}

	// Strategy 0: tiered_tp_atr — `tiers` should have moved to the close ref.
	s0 := strategies[0].(map[string]interface{})
	if _, hasParams := s0["params"]; hasParams {
		t.Error("strategy[0] still has flat `params` after migration")
	}
	open0, ok := s0["open_strategy"].(map[string]interface{})
	if !ok {
		t.Fatalf("strategy[0].open_strategy = %v, want object", s0["open_strategy"])
	}
	if open0["name"] != "tema_cross_bd" {
		t.Errorf("strategy[0].open_strategy.name = %v, want tema_cross_bd", open0["name"])
	}
	openParams, _ := open0["params"].(map[string]interface{})
	if openParams == nil || openParams["short_period"] != 5.0 {
		t.Errorf("strategy[0].open_strategy.params = %v, want {short_period:5}", openParams)
	}
	if _, leakedTiers := openParams["tiers"]; leakedTiers {
		t.Error("strategy[0].open_strategy.params still contains tiers (should have moved to close ref)")
	}

	closes0 := s0["close_strategies"].([]interface{})
	if len(closes0) != 1 {
		t.Fatalf("strategy[0].close_strategies len = %d, want 1", len(closes0))
	}
	close0 := closes0[0].(map[string]interface{})
	if close0["name"] != "tiered_tp_atr" {
		t.Errorf("strategy[0].close_strategies[0].name = %v, want tiered_tp_atr", close0["name"])
	}
	closeParams, ok := close0["params"].(map[string]interface{})
	if !ok {
		t.Fatalf("strategy[0].close_strategies[0].params missing — tiers should have moved here")
	}
	tiers, ok := closeParams["tp_tiers"].([]interface{})
	if !ok || len(tiers) != 2 {
		t.Errorf("close ref params.tp_tiers = %v, want 2-element slice", closeParams["tp_tiers"])
	}

	// Strategy 1: empty open_strategy → migration falls back to args[0]=rsi.
	s1 := strategies[1].(map[string]interface{})
	open1 := s1["open_strategy"].(map[string]interface{})
	if open1["name"] != "rsi" {
		t.Errorf("strategy[1].open_strategy.name = %v, want rsi (from args[0])", open1["name"])
	}
	if _, hasParams := open1["params"]; hasParams {
		t.Error("strategy[1] should not have open params (legacy had no params)")
	}
}

// TestCloseStrategyOwnedKeysMirrorsPythonRegistry asserts every Python close
// strategy's default_params keys are listed in closeStrategyOwnedKeys, so
// adding a new close evaluator in shared_strategies/close/registry.py without
// also updating the Go-side migration map cannot silently route legacy params
// to the open ref. The test shells out to a small Python script that prints
// the registry's default_params per strategy as JSON. It prefers the locked
// uv environment, with plain python3 as a fallback for local test runs.
func TestCloseStrategyOwnedKeysMirrorsPythonRegistry(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	script := `
import sys, json, os
sys.path.insert(0, os.path.join("shared_tools"))
sys.path.insert(0, os.path.join("shared_strategies", "close"))
import importlib.util
spec = importlib.util.spec_from_file_location("close_registry", os.path.join("shared_strategies", "close", "registry.py"))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)
out = {name: list(entry["default_params"].keys()) for name, entry in mod.STRATEGIES.items()}
print(json.dumps(out))
`
	run := func(name string, args ...string) ([]byte, error) {
		cmd := exec.Command(name, args...)
		cmd.Dir = repoRoot
		return cmd.CombinedOutput()
	}

	var output []byte
	var uvOutput []byte
	var uvErr error
	ran := false
	if uvPath, err := exec.LookPath("uv"); err == nil {
		output, uvErr = run(uvPath, "run", "--no-sync", "python", "-c", script)
		if uvErr == nil {
			ran = true
		} else {
			uvOutput = output
		}
	}
	if !ran {
		if pyPath, err := exec.LookPath("python3"); err == nil {
			output, err = run(pyPath, "-c", script)
			if err != nil {
				if uvErr != nil {
					t.Fatalf("uv close registry script failed (%v):\n%s\npython3 fallback failed (%v):\n%s", uvErr, uvOutput, err, output)
				}
				t.Fatalf("python close registry script failed (%v):\n%s", err, output)
			}
		} else if uvErr != nil {
			t.Fatalf("uv close registry script failed (%v):\n%s\nno python3 fallback available", uvErr, uvOutput)
		} else {
			t.Skip("no python3 available; skipping registry sync check")
		}
	}

	// CombinedOutput merges stdout+stderr. The script writes only the JSON
	// blob to stdout, but a Python warning to stderr would prepend non-JSON.
	// Locate the JSON object by trimming to the first '{'.
	if idx := bytes.IndexByte(output, '{'); idx > 0 {
		output = output[idx:]
	}
	var registry map[string][]string
	if err := json.Unmarshal(output, &registry); err != nil {
		t.Fatalf("parse python registry output: %v\n%s", err, output)
	}
	for name, keys := range registry {
		owned, ok := closeStrategyOwnedKeys[name]
		if !ok {
			t.Errorf("close strategy %q has default_params %v in Python registry but is missing from closeStrategyOwnedKeys — legacy migrations would route those params to the open ref", name, keys)
			continue
		}
		for _, k := range keys {
			if _, present := owned[k]; !present {
				t.Errorf("close strategy %q has default_param %q in Python registry but it's missing from closeStrategyOwnedKeys[%q]", name, k, name)
			}
		}
	}
}

// TestMigrateV13ManualStrategyDefaultsHold covers #640 review #2: type=manual
// strategies without a stamp typically have empty `args`, so the migration
// must not leave open_strategy.name = "". Default to "hold" (matches
// LoadConfig's runtime auto-fill for type=manual). Version-less fixture —
// stamped pre-v13 configs are rejected at the #1285 floor.
func TestMigrateV13ManualStrategyDefaultsHold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := map[string]interface{}{
		"strategies": []interface{}{
			map[string]interface{}{
				"id":        "hl-manual-eth",
				"type":      "manual",
				"platform":  "hyperliquid",
				"symbol":    "ETH",
				"timeframe": "1h",
			},
		},
	}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatalf("MigrateConfig: %v", err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(migrated, &got); err != nil {
		t.Fatal(err)
	}
	strategies := got["strategies"].([]interface{})
	open0 := strategies[0].(map[string]interface{})["open_strategy"].(map[string]interface{})
	if open0["name"] != "hold" {
		t.Errorf("manual strategy open_strategy.name = %v, want hold", open0["name"])
	}
}

// TestStrictStringFromJSON is the fail-safe path for a hand-edited config
// where someone wrote an object/array shape without bumping config_version.
// Returning "" lets the v13 migration's args[0] fallback take over instead
// of writing a corrupted "map[name:foo]" name (#640 review #3).
// stringFromJSON keeps its lenient fmt.Sprint behavior for the v14 type check.
func TestStrictStringFromJSON(t *testing.T) {
	if got := strictStringFromJSON(map[string]interface{}{"name": "foo"}); got != "" {
		t.Errorf("strictStringFromJSON(map) = %q, want empty (fail-safe)", got)
	}
	if got := strictStringFromJSON([]interface{}{"a", "b"}); got != "" {
		t.Errorf("strictStringFromJSON([]) = %q, want empty", got)
	}
	if got := strictStringFromJSON(42.0); got != "" {
		t.Errorf("strictStringFromJSON(float) = %q, want empty", got)
	}
	if got := strictStringFromJSON("  real  "); got != "real" {
		t.Errorf("strictStringFromJSON(string) = %q, want real (trimmed)", got)
	}
}

// TestLoadConfigMigratesVersionlessEndToEnd is the smoke equivalent of running
// `./go-trader --once` against a version-less legacy-shaped config (originally
// PR #642 re-review item #5 at v12; converted to version-less by the #1285
// floor — stamped pre-v13 configs are now rejected, see
// TestLoadConfigRejectsPreFloorVersion). It writes a config containing both a
// tiered_tp_atr close ref AND extra non-tiered keys in flat `params`, then
// exercises the full pipeline:
//
//	raw legacy JSON → schema migration → file rewritten → LoadConfig parse →
//	defaults + validation → in-memory StrategyConfig with split refs.
//
// Failure modes this catches:
//   - migration silently routes tiers to the open ref (the original #640 bug)
//   - migration loses extra non-tiered open params
//   - on-disk file isn't actually rewritten (LoadConfig would error)
//   - validation rejects the migrated shape
//   - close ref doesn't carry its tiers through to where buildHyperliquidProtectionPlan reads them
func TestLoadConfigMigratesVersionlessEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	legacy := map[string]interface{}{
		"interval_seconds":           3600,
		"log_dir":                    "logs",
		"db_file":                    filepath.Join(dir, "state.db"),
		"default_stop_loss_atr_mult": 1.0,
		"portfolio_risk":             map[string]interface{}{"max_drawdown_pct": 25, "warn_threshold_pct": 60},
		"strategies": []interface{}{
			map[string]interface{}{
				"id":               "hl-temacb-btc",
				"type":             "perps",
				"platform":         "hyperliquid",
				"script":           "shared_scripts/check_hyperliquid.py",
				"args":             []interface{}{"tema_cross_bd", "BTC", "1h", "--mode=paper"},
				"open_strategy":    "tema_cross_bd",
				"close_strategies": []interface{}{"tiered_tp_atr"},
				"capital":          1000.0,
				"max_drawdown_pct": 25.0,
				"leverage":         1.0,
				"allow_shorts":     true,
				"params": map[string]interface{}{
					// Owned by tiered_tp_atr → must move to the close ref.
					"tp_tiers": []interface{}{
						map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
						map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
					},
					// Open-strategy-only → must stay on open ref.
					"short_period": 5.0,
					"mid_period":   13.0,
				},
			},
		},
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}

	if got := cfg.ConfigVersion; got != CurrentConfigVersion {
		t.Errorf("ConfigVersion = %d, want %d", got, CurrentConfigVersion)
	}
	if len(cfg.Strategies) != 1 {
		t.Fatalf("len(strategies) = %d, want 1", len(cfg.Strategies))
	}
	sc := cfg.Strategies[0]

	if sc.OpenStrategy.Name != "tema_cross_bd" {
		t.Errorf("OpenStrategy.Name = %q, want tema_cross_bd", sc.OpenStrategy.Name)
	}
	// Open params: short_period + mid_period only; tiers must NOT be here.
	if got := sc.OpenStrategy.Params["short_period"]; got != 5.0 {
		t.Errorf("OpenStrategy.Params[short_period] = %v, want 5", got)
	}
	if got := sc.OpenStrategy.Params["mid_period"]; got != 13.0 {
		t.Errorf("OpenStrategy.Params[mid_period] = %v, want 13", got)
	}
	if _, leaked := sc.OpenStrategy.Params["tiers"]; leaked {
		t.Error("OpenStrategy.Params still contains tiers — the original #640 bug")
	}
	// Close ref: tiers landed here. The v13 migration writes a single-element
	// close_strategies array, which UnmarshalJSON lifts to the single
	// close_strategy ref (#842).
	if sc.CloseStrategy == nil {
		t.Fatalf("CloseStrategy = nil, want a single tiered_tp_atr ref")
	}
	close0 := *sc.CloseStrategy
	if close0.Name != "tiered_tp_atr" {
		t.Errorf("CloseStrategy.Name = %q, want tiered_tp_atr", close0.Name)
	}
	tiers, ok := close0.Params["tp_tiers"].([]interface{})
	if !ok || len(tiers) != 2 {
		t.Fatalf("CloseStrategy.Params[tp_tiers] = %v, want 2-element slice", close0.Params["tp_tiers"])
	}

	// End-to-end check: tiers reach buildHyperliquidProtectionPlan via the
	// close ref's params, exactly the path that was broken pre-#640.
	pos := &Position{Symbol: "BTC", Quantity: 1, AvgCost: 50000, EntryATR: 500, Side: "long"}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatal("buildHyperliquidProtectionPlan returned ok=false")
	}
	wantTiers := []hlProtectionTier{{Multiple: 2, Fraction: 0.5}, {Multiple: 3, Fraction: 1}}
	if !reflect.DeepEqual(plan.Tiers, wantTiers) {
		t.Errorf("plan.Tiers = %+v, want %+v (custom tiers from migrated config)", plan.Tiers, wantTiers)
	}

	// Re-loading the now-v13 file must not retrigger migration.
	cfg2, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig (re-read): %v", err)
	}
	if !reflect.DeepEqual(cfg.Strategies[0].OpenStrategy, cfg2.Strategies[0].OpenStrategy) {
		t.Error("re-load produced different OpenStrategy — migration is not idempotent")
	}
}

// TestLoadConfigRejectsPreFloorVersion is the LoadConfig-level #1285 floor
// check: a config stamped below MinSupportedConfigVersion must fail the load
// with the actionable rejection BEFORE any migration pass touches the file —
// the daemon must never start on (or partially rewrite) an ancient config.
func TestLoadConfigRejectsPreFloorVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := writeRawConfig(t, path, map[string]interface{}{
		"config_version":   12,
		"interval_seconds": 3600,
		"strategies": []interface{}{
			map[string]interface{}{
				"id":               "hl-temacb-btc",
				"type":             "perps",
				"platform":         "hyperliquid",
				"script":           "shared_scripts/check_hyperliquid.py",
				"args":             []interface{}{"tema_cross_bd", "BTC", "1h", "--mode=paper"},
				"open_strategy":    "tema_cross_bd",
				"close_strategies": []interface{}{"tiered_tp_atr"},
				"capital":          1000.0,
				"max_drawdown_pct": 25.0,
			},
		},
	})
	_, err := LoadConfig(path)
	assertUnsupportedVersionError(t, err, 12, path, original)
}

// #656 — v14 migration converts allow_shorts:bool → direction:string. Both
// boolean values must map correctly, and the legacy key must be removed.
func TestMigrateV14Direction(t *testing.T) {
	cases := []struct {
		name           string
		strategy       map[string]interface{}
		wantDir        string
		wantLegacyKept bool // legacy key preserved (e.g. non-perps shouldn't get a direction)
	}{
		{
			name: "perps_allow_shorts_true_to_both",
			strategy: map[string]interface{}{
				"id": "hl-temab-eth", "type": "perps", "platform": "hyperliquid",
				"allow_shorts": true,
			},
			wantDir: "both",
		},
		{
			name: "perps_allow_shorts_false_to_long",
			strategy: map[string]interface{}{
				"id": "hl-tema-eth", "type": "perps", "platform": "hyperliquid",
				"allow_shorts": false,
			},
			wantDir: "long",
		},
		{
			name: "perps_already_has_direction_preserves",
			strategy: map[string]interface{}{
				"id": "hl-bear-eth", "type": "perps", "platform": "hyperliquid",
				"allow_shorts": false, "direction": "short",
			},
			wantDir: "short", // direction wins, allow_shorts dropped
		},
		{
			name: "non_perps_drops_legacy_key_no_direction",
			strategy: map[string]interface{}{
				"id": "bn-sma-btc", "type": "spot", "platform": "binanceus",
				"allow_shorts": true,
			},
			wantDir: "",
		},
		// #656 review: type=manual trades HL perps and previously gated
		// manual-open --side via allow_shorts. The migration must translate
		// manual the same as perps, otherwise existing manual configs with
		// allow_shorts:true silently regress to long-only post-v14.
		{
			name: "manual_allow_shorts_true_to_both",
			strategy: map[string]interface{}{
				"id": "hl-manual-eth", "type": "manual", "platform": "hyperliquid",
				"allow_shorts": true,
			},
			wantDir: "both",
		},
		{
			name: "manual_allow_shorts_false_to_long",
			strategy: map[string]interface{}{
				"id": "hl-manual-btc", "type": "manual", "platform": "hyperliquid",
				"allow_shorts": false,
			},
			wantDir: "long",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := map[string]interface{}{
				"strategies": []interface{}{tc.strategy},
			}
			migrateV14Direction(raw)

			strategies := raw["strategies"].([]interface{})
			sc := strategies[0].(map[string]interface{})
			if _, leaked := sc["allow_shorts"]; leaked {
				t.Errorf("allow_shorts must be deleted post-migration, got %+v", sc)
			}
			gotDir, _ := sc["direction"].(string)
			if gotDir != tc.wantDir {
				t.Errorf("direction = %q, want %q (full strategy: %+v)", gotDir, tc.wantDir, sc)
			}
		})
	}
}

// #656 — full LoadConfig migration path: a pre-v14 config with
// allow_shorts:true must end up with Direction="both" and no AllowShorts in
// the parsed struct (since the JSON key is gone). Fixture stamped at the
// #1285 floor (v13) — the oldest version that still migrates.
func TestLoadConfig_V14_TranslatesAllowShortsToDirection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v13.json")

	v13 := map[string]interface{}{
		"config_version": MinSupportedConfigVersion,
		"strategies": []interface{}{
			map[string]interface{}{
				"id":               "hl-temab-eth",
				"type":             "perps",
				"platform":         "hyperliquid",
				"script":           "shared_scripts/check_hyperliquid.py",
				"args":             []interface{}{"triple_ema_bidir", "ETH", "1h", "--mode=paper"},
				"open_strategy":    map[string]interface{}{"name": "triple_ema_bidir"},
				"capital":          1000.0,
				"max_drawdown_pct": 25.0,
				"leverage":         1.0,
				"allow_shorts":     true,
			},
			map[string]interface{}{
				"id":               "hl-tema-btc",
				"type":             "perps",
				"platform":         "hyperliquid",
				"script":           "shared_scripts/check_hyperliquid.py",
				"args":             []interface{}{"triple_ema", "BTC", "1h", "--mode=paper"},
				"open_strategy":    map[string]interface{}{"name": "triple_ema"},
				"capital":          1000.0,
				"max_drawdown_pct": 25.0,
				"leverage":         1.0,
				"allow_shorts":     false,
			},
		},
	}
	data, err := json.MarshalIndent(v13, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ConfigVersion != CurrentConfigVersion {
		t.Errorf("ConfigVersion = %d, want %d", cfg.ConfigVersion, CurrentConfigVersion)
	}
	byID := map[string]StrategyConfig{}
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}
	if got := byID["hl-temab-eth"].Direction; got != "both" {
		t.Errorf("hl-temab-eth Direction = %q, want %q (allow_shorts=true)", got, "both")
	}
	if got := byID["hl-tema-btc"].Direction; got != "long" {
		t.Errorf("hl-tema-btc Direction = %q, want %q (allow_shorts=false)", got, "long")
	}
	// Legacy field must be gone (zero value).
	if byID["hl-temab-eth"].AllowShorts {
		t.Error("hl-temab-eth AllowShorts should be false post-migration (key removed from JSON)")
	}
	if byID["hl-tema-btc"].AllowShorts {
		t.Error("hl-tema-btc AllowShorts should be false post-migration")
	}
	// EffectiveDirection should agree.
	if got := EffectiveDirection(byID["hl-temab-eth"]); got != DirectionBoth {
		t.Errorf("EffectiveDirection(temab) = %q, want %q", got, DirectionBoth)
	}
	if got := EffectiveDirection(byID["hl-tema-btc"]); got != DirectionLong {
		t.Errorf("EffectiveDirection(tema) = %q, want %q", got, DirectionLong)
	}
}

func TestMigrateConfigV16UserDefaultsAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := map[string]interface{}{
		"config_version": 15,
		"user_close_defaults": map[string]interface{}{
			"trailing_tp_ratchet": map[string]interface{}{
				"tp_tiers": []interface{}{map[string]interface{}{"atr_multiple": 1.0, "trailing_mult_after": 1.0, "close_fraction": 0.0}},
			},
			"regime_atr": map[string]interface{}{
				"stop_loss_atr_regime": map[string]interface{}{"use_defaults": true},
			},
		},
		"manual_defaults": map[string]interface{}{
			"margin_usd":         125.0,
			"stop_loss_atr_mult": 2.25,
			"side":               "short",
		},
	}
	writeMigrationJSON(t, path, raw)

	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatalf("MigrateConfig: %v", err)
	}
	updated := readMigrationJSON(t, path)
	if int(updated["config_version"].(float64)) != CurrentConfigVersion {
		t.Fatalf("config_version = %v, want %d", updated["config_version"], CurrentConfigVersion)
	}
	if _, ok := updated["user_close_defaults"]; ok {
		t.Fatal("legacy user_close_defaults key was not removed")
	}
	if _, ok := updated["manual_defaults"]; ok {
		t.Fatal("legacy manual_defaults key was not removed")
	}
	userDefaults := updated["user_defaults"].(map[string]interface{})
	closeDefaults := userDefaults["close"].(map[string]interface{})
	if _, ok := closeDefaults["regime_atr"]; ok {
		t.Fatal("regime_atr stayed inside user_defaults.close")
	}
	if _, ok := closeDefaults["trailing_tp_ratchet"]; !ok {
		t.Fatalf("trailing_tp_ratchet missing from user_defaults.close: %+v", closeDefaults)
	}
	regimeATR := userDefaults["regime_atr"].(map[string]interface{})
	if _, ok := regimeATR["stop_loss_atr_regime"]; !ok {
		t.Fatalf("stop_loss_atr_regime missing from user_defaults.regime_atr: %+v", regimeATR)
	}
	manual := userDefaults["manual"].(map[string]interface{})
	if manual["margin_usd"].(float64) != 125.0 || manual["side"].(string) != "short" {
		t.Fatalf("manual defaults not migrated: %+v", manual)
	}
}

func TestMigrateConfigV16UserDefaultsEquivalentAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	closeDefaults := map[string]interface{}{
		"tiered_tp_atr": map[string]interface{}{
			"tp_tiers": []interface{}{map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0}},
		},
	}
	raw := map[string]interface{}{
		"config_version":             16,
		"user_close_defaults":        closeDefaults,
		"user_defaults":              map[string]interface{}{"close": closeDefaults},
		"interval_seconds":           600,
		"default_stop_loss_atr_mult": 1.0,
	}
	writeMigrationJSON(t, path, raw)

	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatalf("MigrateConfig accepted equivalent legacy/canonical aliases: %v", err)
	}
	updated := readMigrationJSON(t, path)
	if _, ok := updated["user_close_defaults"]; ok {
		t.Fatal("equivalent legacy user_close_defaults key was not removed")
	}
	userDefaults := updated["user_defaults"].(map[string]interface{})
	if !reflect.DeepEqual(userDefaults["close"], closeDefaults) {
		t.Fatalf("canonical close defaults changed: %+v", userDefaults["close"])
	}
}

func TestMigrateConfigV16UserDefaultsConflictRejects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := map[string]interface{}{
		"config_version": 16,
		"user_defaults": map[string]interface{}{
			"manual": map[string]interface{}{"side": "long"},
		},
		"manual_defaults": map[string]interface{}{"side": "short"},
	}
	writeMigrationJSON(t, path, raw)

	err := MigrateConfig(path, nil, nil)
	if err == nil {
		t.Fatal("MigrateConfig accepted conflicting user_defaults.manual/manual_defaults")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("conflicts")) {
		t.Fatalf("error %q does not mention conflict", err)
	}
}

func writeMigrationJSON(t *testing.T, path string, raw map[string]interface{}) {
	t.Helper()
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readMigrationJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
