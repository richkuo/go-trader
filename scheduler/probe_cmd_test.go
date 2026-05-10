package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRunProbeMissingConfig: a missing config file fails fast with exit 1
// rather than blowing up in some confusing place — this is the path
// scripts/update.sh hits if config.json is genuinely absent on a fresh box.
func TestRunProbeMissingConfig(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "no-such.json")
	rc := runProbe([]string{"--config", missing})
	if rc != 1 {
		t.Fatalf("missing config should return 1, got %d", rc)
	}
}

// TestRunProbeNoStrategies: an empty strategies list means no scripts to
// probe, so probe trivially succeeds — this is acceptable: a config with no
// configured strategies has no Python contract to validate.
func TestRunProbeNoStrategies(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	body, _ := json.Marshal(map[string]any{
		"interval_seconds": 60,
		"strategies":       []any{},
	})
	if err := os.WriteFile(cfgPath, body, 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	rc := runProbe([]string{"--config", cfgPath})
	if rc != 0 {
		t.Fatalf("empty-strategies probe should return 0, got %d", rc)
	}
}
