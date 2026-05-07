package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
					"tiers": []interface{}{
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
	tiers, ok := closeParams["tiers"].([]interface{})
	if !ok || len(tiers) != 2 {
		t.Errorf("close ref params.tiers = %v, want 2-element slice", closeParams["tiers"])
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
