package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNewFieldsSince(t *testing.T) {
	cases := []struct {
		version  int
		minCount int // at least this many fields
	}{
		{0, 2}, // v2 owner_id + v3 warn_threshold (v4 dm booleans removed in v7)
		{1, 2}, // v1 baseline → v2+ fields
		{2, 1}, // v3+ only
		{3, 0}, // nothing after v3 in registry
		{4, 0},
		{CurrentConfigVersion, 0}, // no new fields
		{999, 0},                  // future version
	}

	for _, tc := range cases {
		fields := NewFieldsSince(tc.version)
		if len(fields) < tc.minCount {
			t.Errorf("NewFieldsSince(%d) returned %d fields, want >= %d", tc.version, len(fields), tc.minCount)
		}
		// All returned fields should have Version > tc.version
		for _, f := range fields {
			if f.Version <= tc.version {
				t.Errorf("NewFieldsSince(%d) returned field %q with version %d", tc.version, f.JSONPath, f.Version)
			}
		}
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

	original := map[string]interface{}{
		"config_version":   1,
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
	if updated["default_stop_loss_atr_mult"].(float64) != DefaultStopLossATRMult {
		t.Errorf("default_stop_loss_atr_mult = %v, want %g", updated["default_stop_loss_atr_mult"], DefaultStopLossATRMult)
	}
}

func TestMigrateConfigCreatesNestedPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := map[string]interface{}{"config_version": 1}
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

	original := map[string]interface{}{"config_version": 2}
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

	original := map[string]interface{}{"config_version": 1}
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

func TestRemoveNestedField(t *testing.T) {
	obj := map[string]interface{}{
		"top_level": "value1",
		"nested": map[string]interface{}{
			"field": "value2",
			"keep":  "preserved",
		},
	}

	removeNestedField(obj, "top_level")
	if _, ok := obj["top_level"]; ok {
		t.Error("top_level should have been removed")
	}

	removeNestedField(obj, "nested.field")
	nested := obj["nested"].(map[string]interface{})
	if _, ok := nested["field"]; ok {
		t.Error("nested.field should have been removed")
	}
	if nested["keep"] != "preserved" {
		t.Error("nested.keep should be preserved")
	}

	// Removing a non-existent field should be a no-op.
	removeNestedField(obj, "nonexistent.path")
}

func TestMigrateConfigV6SkipsRemovalForCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Config already at v6 with fields that happen to match deprecated names
	// should NOT have them removed (version guard).
	original := map[string]interface{}{
		"config_version": 6,
		"discord": map[string]interface{}{
			"channel_paper_trades": true,
			"channel_live_trades":  true,
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

	// Fields should still be present since config was already at v6.
	discord := updated["discord"].(map[string]interface{})
	if _, ok := discord["channel_paper_trades"]; !ok {
		t.Error("discord.channel_paper_trades should NOT have been removed for v6+ config")
	}
	if _, ok := discord["channel_live_trades"]; !ok {
		t.Error("discord.channel_live_trades should NOT have been removed for v6+ config")
	}
}

func TestMigrateConfigV6RemovesChannelBooleans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := map[string]interface{}{
		"config_version": 5,
		"discord": map[string]interface{}{
			"enabled":              true,
			"channel_paper_trades": true,
			"channel_live_trades":  true,
			"channels": map[string]interface{}{
				"hyperliquid": "ch-123",
			},
		},
		"telegram": map[string]interface{}{
			"channel_paper_trades": false,
			"channel_live_trades":  true,
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

	// Version should be bumped to CurrentConfigVersion.
	version := int(updated["config_version"].(float64))
	if version != CurrentConfigVersion {
		t.Errorf("config_version = %d, want %d", version, CurrentConfigVersion)
	}

	// Channel booleans should be removed from both discord and telegram.
	discord := updated["discord"].(map[string]interface{})
	if _, ok := discord["channel_paper_trades"]; ok {
		t.Error("discord.channel_paper_trades should have been removed")
	}
	if _, ok := discord["channel_live_trades"]; ok {
		t.Error("discord.channel_live_trades should have been removed")
	}

	telegram := updated["telegram"].(map[string]interface{})
	if _, ok := telegram["channel_paper_trades"]; ok {
		t.Error("telegram.channel_paper_trades should have been removed")
	}
	if _, ok := telegram["channel_live_trades"]; ok {
		t.Error("telegram.channel_live_trades should have been removed")
	}

	// Other fields should be preserved.
	if discord["enabled"] != true {
		t.Error("discord.enabled should be preserved")
	}
	channels := discord["channels"].(map[string]interface{})
	if channels["hyperliquid"] != "ch-123" {
		t.Error("discord.channels.hyperliquid should be preserved")
	}
}

func TestMigrateConfigV7TranslatesDMBooleans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := map[string]interface{}{
		"config_version": 6,
		"discord": map[string]interface{}{
			"owner_id":        "disc-owner",
			"dm_paper_trades": true,
			"dm_live_trades":  false,
		},
		"telegram": map[string]interface{}{
			"owner_chat_id":   "tg-owner",
			"dm_paper_trades": false,
			"dm_live_trades":  true,
		},
	}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Strategies: []StrategyConfig{
			{Platform: "hyperliquid"},
			{Platform: "deribit"},
		},
	}
	if err := MigrateConfig(path, nil, cfg); err != nil {
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
	if _, ok := discord["dm_paper_trades"]; ok {
		t.Error("discord.dm_paper_trades should be removed")
	}
	if _, ok := discord["dm_live_trades"]; ok {
		t.Error("discord.dm_live_trades should be removed")
	}
	dmD := discord["dm_channels"].(map[string]interface{})
	if dmD["hyperliquid-paper"] != "disc-owner" || dmD["deribit-paper"] != "disc-owner" {
		t.Errorf("discord dm_channels (paper) = %#v", dmD)
	}
	if _, ok := dmD["hyperliquid"]; ok {
		t.Error("discord live hyperliquid should not be set when dm_live_trades is false")
	}

	tg := updated["telegram"].(map[string]interface{})
	if _, ok := tg["dm_live_trades"]; ok {
		t.Error("telegram.dm_live_trades should be removed")
	}
	dmT := tg["dm_channels"].(map[string]interface{})
	if dmT["hyperliquid"] != "tg-owner" || dmT["deribit"] != "tg-owner" {
		t.Errorf("telegram dm_channels (live) = %#v", dmT)
	}
	if _, ok := dmT["hyperliquid-paper"]; ok {
		t.Error("telegram paper key should not exist when dm_paper_trades is false")
	}
}

func TestMigrateConfigV7RemovesDMBooleansWhenUnset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := map[string]interface{}{
		"config_version": 6,
		"discord": map[string]interface{}{
			"owner_id":        "o1",
			"dm_paper_trades": false,
			"dm_live_trades":  false,
		},
	}
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Strategies: []StrategyConfig{{Platform: "hyperliquid"}}}
	if err := MigrateConfig(path, nil, cfg); err != nil {
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
	if _, ok := discord["dm_paper_trades"]; ok {
		t.Error("dm_paper_trades should be removed")
	}
	if _, ok := discord["dm_channels"]; ok {
		t.Error("dm_channels should not be added when both dm booleans are false")
	}
}

// TestMigrateConfigV8RemovesDeadSummaryFreqFields verifies that the dead
// discord.spot_summary_freq / discord.options_summary_freq fields (replaced by
// the top-level summary_frequency map in #30) are stripped when upgrading
// from v7.
func TestMigrateConfigV8RemovesDeadSummaryFreqFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := map[string]interface{}{
		"config_version": 7,
		"discord": map[string]interface{}{
			"enabled":              true,
			"spot_summary_freq":    "hourly",
			"options_summary_freq": "per_check",
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
	if _, ok := discord["spot_summary_freq"]; ok {
		t.Error("discord.spot_summary_freq should have been removed")
	}
	if _, ok := discord["options_summary_freq"]; ok {
		t.Error("discord.options_summary_freq should have been removed")
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

func TestMigrateConfigV10AddsSizingLeverage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := map[string]interface{}{
		"config_version": 9,
		"strategies": []interface{}{
			map[string]interface{}{
				"id":       "hl-eth",
				"type":     "perps",
				"leverage": float64(2),
			},
			map[string]interface{}{
				"id":              "okx-btc",
				"type":            "perps",
				"leverage":        float64(20),
				"sizing_leverage": float64(3),
			},
			map[string]interface{}{
				"id":       "spot-btc",
				"type":     "spot",
				"leverage": float64(2),
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
	strategies := updated["strategies"].([]interface{})
	hl := strategies[0].(map[string]interface{})
	if got := hl["sizing_leverage"].(float64); got != 2 {
		t.Errorf("hl sizing_leverage = %g, want 2", got)
	}
	okx := strategies[1].(map[string]interface{})
	if got := okx["sizing_leverage"].(float64); got != 3 {
		t.Errorf("okx sizing_leverage = %g, want existing 3", got)
	}
	spot := strategies[2].(map[string]interface{})
	if _, ok := spot["sizing_leverage"]; ok {
		t.Error("spot sizing_leverage should not be added")
	}
	if v := int(updated["config_version"].(float64)); v != CurrentConfigVersion {
		t.Errorf("config_version = %d, want %d", v, CurrentConfigVersion)
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

	// v12 config: tema_cross_bd open + tiered_tp_atr close, with legacy `tiers`
	// in flat params (the bug that motivated #640).
	original := map[string]interface{}{
		"config_version": 12,
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
// strategies in v12 typically have empty `args`, so the migration must not
// leave open_strategy.name = "". Default to "hold" (matches LoadConfig's
// runtime auto-fill for type=manual).
func TestMigrateV13ManualStrategyDefaultsHold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	original := map[string]interface{}{
		"config_version": 12,
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
// stringFromJSON keeps its lenient fmt.Sprint behavior for v6/v7 callers.
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

// TestLoadConfigMigratesV12EndToEnd is the smoke equivalent of running
// `./go-trader --once` against a real v12 config (PR #642 re-review item #5).
// It writes a v12 config containing both a tiered_tp_atr close ref AND extra
// non-tiered keys in flat `params`, then exercises the full pipeline:
//
//	raw v12 JSON → schema migration → file rewritten → LoadConfig parse →
//	defaults + validation → in-memory StrategyConfig with split refs.
//
// Failure modes this catches:
//   - migration silently routes tiers to the open ref (the original #640 bug)
//   - migration loses extra non-tiered open params
//   - on-disk file isn't actually rewritten (LoadConfig would error)
//   - validation rejects the migrated shape
//   - close ref doesn't carry its tiers through to where buildHyperliquidProtectionPlan reads them
func TestLoadConfigMigratesV12EndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	v12 := map[string]interface{}{
		"config_version":             12,
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
	data, err := json.MarshalIndent(v12, "", "  ")
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
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
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

// #656 — full LoadConfig migration path: a v12 config with allow_shorts:true
// must end up with Direction="both" and no AllowShorts in the parsed struct
// (since the JSON key is gone). Mirror of the v13 schema-shape migration test.
func TestLoadConfig_V14_TranslatesAllowShortsToDirection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v12.json")

	v12 := map[string]interface{}{
		"config_version": 12,
		"strategies": []interface{}{
			map[string]interface{}{
				"id":               "hl-temab-eth",
				"type":             "perps",
				"platform":         "hyperliquid",
				"script":           "shared_scripts/check_hyperliquid.py",
				"args":             []interface{}{"triple_ema_bidir", "ETH", "1h", "--mode=paper"},
				"open_strategy":    "triple_ema_bidir",
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
				"open_strategy":    "triple_ema",
				"capital":          1000.0,
				"max_drawdown_pct": 25.0,
				"leverage":         1.0,
				"allow_shorts":     false,
			},
		},
	}
	data, err := json.MarshalIndent(v12, "", "  ")
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
