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

// TestRunProbeHappyPath: a config with two strategies sharing one script and
// one with a distinct script produces exactly two probe invocations (one per
// unique script) and runProbe returns 0. Stubs probeOneCheckScriptFn because
// Go CI doesn't have .venv/bin/python3 (see CLAUDE.md → Testing).
func TestRunProbeHappyPath(t *testing.T) {
	orig := probeOneCheckScriptFn
	defer func() { probeOneCheckScriptFn = orig }()
	var probed []string
	probeOneCheckScriptFn = func(script string) error {
		probed = append(probed, script)
		return nil
	}

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	body, _ := json.Marshal(map[string]any{
		"interval_seconds": 60,
		"strategies": []any{
			map[string]any{
				"id":               "hl-a",
				"type":             "perps",
				"platform":         "hyperliquid",
				"script":           "shared_scripts/check_hyperliquid.py",
				"args":             []string{"momentum", "BTC", "1h"},
				"interval_seconds": 60,
				"capital":          1000.0,
			},
			map[string]any{
				"id":               "hl-b",
				"type":             "perps",
				"platform":         "hyperliquid",
				"script":           "shared_scripts/check_hyperliquid.py",
				"args":             []string{"momentum", "ETH", "1h"},
				"interval_seconds": 60,
				"capital":          1000.0,
			},
			map[string]any{
				"id":               "spot-a",
				"type":             "spot",
				"script":           "shared_scripts/check_strategy.py",
				"args":             []string{"sma", "BTC/USDT", "1h"},
				"interval_seconds": 60,
				"capital":          1000.0,
			},
		},
	})
	if err := os.WriteFile(cfgPath, body, 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	rc := runProbe([]string{"--config", cfgPath})
	if rc != 0 {
		t.Fatalf("happy-path probe should return 0, got %d", rc)
	}
	if len(probed) != 2 {
		t.Fatalf("expected 2 unique scripts probed, got %d: %v", len(probed), probed)
	}
}
