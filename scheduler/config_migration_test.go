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
		{0, 6},                    // all fields; v5 channel booleans removed in v6
		{1, 6},                    // v1 baseline, should get all v2+ fields
		{2, 4},                    // should get v3+ fields
		{3, 4},                    // should get v4+ fields
		{4, 0},                    // v5 channel booleans removed — no new fields since v4
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
	if err := MigrateConfig(path, values); err != nil {
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
		"discord.dm_live_trades": "true",
	}
	if err := MigrateConfig(path, values); err != nil {
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
	if discord["dm_live_trades"] != "true" {
		t.Errorf("discord.dm_live_trades = %v, want %q", discord["dm_live_trades"], "true")
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
	if err := MigrateConfig(path, nil); err != nil {
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

	if err := MigrateConfig(path, nil); err != nil {
		t.Fatal(err)
	}

	// tmp file should not remain
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should not exist after migration")
	}
}

func TestMigrateConfigMissingFile(t *testing.T) {
	err := MigrateConfig("/nonexistent/config.json", nil)
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

	err := MigrateConfig(path, nil)
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

	if err := MigrateConfig(path, nil); err != nil {
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

	if err := MigrateConfig(path, nil); err != nil {
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

	// Version should be bumped to 6.
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
