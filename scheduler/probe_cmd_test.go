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
// Go CI should not depend on a real Python runtime for this command-level test.
func TestRunProbeHappyPath(t *testing.T) {
	orig := probeOneCheckScriptFn
	defer func() { probeOneCheckScriptFn = orig }()
	type probeCall struct {
		script string
		mode   string // "signal" or "fetch-atr"
	}
	var probed []probeCall
	probeOneCheckScriptFn = func(script string, argv []string) error {
		mode := "signal"
		for _, a := range argv {
			if a == "--fetch-atr" {
				mode = "fetch-atr"
				break
			}
			if a == "--execute" {
				mode = "execute"
				break
			}
		}
		probed = append(probed, probeCall{script, mode})
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
	// Expect 5 invocations: HL signal-check, HL --fetch-atr (#689), HL
	// --execute (PR #769), spot signal-check, and the dashboard candle helper.
	if len(probed) != 5 {
		t.Fatalf("expected 5 probe invocations, got %d: %v", len(probed), probed)
	}
	var hlSignal, hlFetchATR, hlExecute, spotSignal, candleHelper int
	for _, p := range probed {
		switch {
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "signal":
			hlSignal++
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "fetch-atr":
			hlFetchATR++
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "execute":
			hlExecute++
		case p.script == "shared_scripts/check_strategy.py" && p.mode == "signal":
			spotSignal++
		case p.script == "shared_scripts/fetch_candles.py" && p.mode == "signal":
			candleHelper++
		}
	}
	if hlSignal != 1 || hlFetchATR != 1 || hlExecute != 1 || spotSignal != 1 || candleHelper != 1 {
		t.Fatalf("expected 1 of each (hl-signal, hl-fetch-atr, hl-execute, spot-signal, candle-helper); got %d/%d/%d/%d/%d (probed=%v)",
			hlSignal, hlFetchATR, hlExecute, spotSignal, candleHelper, probed)
	}
}

// #787: update.sh probe must load live HL configs without shell secrets.
func TestRunProbeSkipsLiveCredentialChecks(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "")

	orig := probeOneCheckScriptFn
	defer func() { probeOneCheckScriptFn = orig }()
	probeOneCheckScriptFn = func(script string, argv []string) error { return nil }

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	body, _ := json.Marshal(map[string]any{
		"interval_seconds": 60,
		"strategies": []any{
			map[string]any{
				"id":               "hl-tema-eth-live",
				"type":             "perps",
				"platform":         "hyperliquid",
				"script":           "shared_scripts/check_hyperliquid.py",
				"args":             []string{"triple_ema", "ETH", "1h", "--mode=live"},
				"interval_seconds": 60,
				"capital":          1000.0,
				"max_drawdown_pct": 60.0,
			},
		},
	})
	if err := os.WriteFile(cfgPath, body, 0o644); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	rc := runProbe([]string{"--config", cfgPath})
	if rc != 0 {
		t.Fatalf("probe with live HL config and no shell secrets should return 0, got %d", rc)
	}
}
