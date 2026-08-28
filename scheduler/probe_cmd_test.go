package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunProbeMissingConfig(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "no-such.json")
	rc := runProbe([]string{"--config", missing})
	if rc != 1 {
		t.Fatalf("missing config should return 1, got %d", rc)
	}
}

func TestRunProbeReturnsExitProbeFailureOnScriptFailure(t *testing.T) {
	orig := probeOneCheckScriptFn
	defer func() { probeOneCheckScriptFn = orig }()
	probeOneCheckScriptFn = func(script string, argv []string) error {
		return formatProbeFailure(script, os.ErrInvalid, "error: unrecognized arguments: --probe-only", "")
	}

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	body, _ := json.Marshal(map[string]any{
		"interval_seconds": 60,
		"strategies": []any{
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
	if rc != ExitProbeFailure {
		t.Fatalf("probe script failure should return %d, got %d", ExitProbeFailure, rc)
	}
}

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

func TestRunProbeHappyPath(t *testing.T) {
	orig := probeOneCheckScriptFn
	defer func() { probeOneCheckScriptFn = orig }()
	type probeCall struct {
		script string
		mode   string
	}
	var probed []probeCall
	probeOneCheckScriptFn = func(script string, argv []string) error {
		mode := "signal"
		for _, a := range argv {
			switch a {
			case "--fetch-atr":
				mode = "fetch-atr"
			case "--execute":
				mode = "execute"
			case "--limit-open":
				mode = "limit-open"
			case "--limit-status":
				mode = "limit-status"
			case "--cancel-order":
				mode = "cancel-order"
			case "--batch-check":
				mode = "batch-check"
			}
			if mode != "signal" {
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
	if len(probed) != 14 {
		t.Fatalf("expected 14 probe invocations, got %d: %v", len(probed), probed)
	}
	var hlSignal, hlFetchATR, hlExecute, hlLimitOpen, hlLimitStatus, hlCancelOrder, hlBatchCheck, spotSignal, candleHelper, schemaHelper, simulateHelper, regimeHelper int
	for _, p := range probed {
		switch {
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "signal":
			hlSignal++
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "fetch-atr":
			hlFetchATR++
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "execute":
			hlExecute++
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "limit-open":
			hlLimitOpen++
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "limit-status":
			hlLimitStatus++
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "cancel-order":
			hlCancelOrder++
		case p.script == "shared_scripts/check_hyperliquid.py" && p.mode == "batch-check":
			hlBatchCheck++
		case p.script == "shared_scripts/check_strategy.py" && p.mode == "signal":
			spotSignal++
		case p.script == "shared_scripts/fetch_candles.py" && p.mode == "signal":
			candleHelper++
		case p.script == "shared_scripts/strategy_tuner_schema.py" && p.mode == "signal":
			schemaHelper++
		case p.script == "shared_scripts/simulate_strategy.py" && p.mode == "signal":
			simulateHelper++
		case p.script == "shared_scripts/check_regime.py" && p.mode == "signal":
			regimeHelper++
		}
	}
	if hlSignal != 2 || hlFetchATR != 1 || hlExecute != 1 || hlLimitOpen != 1 || hlLimitStatus != 1 || hlCancelOrder != 1 || hlBatchCheck != 1 || spotSignal != 2 || candleHelper != 1 || schemaHelper != 1 || simulateHelper != 1 || regimeHelper != 1 {
		t.Fatalf("expected hl-signal=2, hl-fetch-atr=1, hl-execute=1, hl-limit-open=1, hl-limit-status=1, hl-cancel-order=1, hl-batch-check=1, spot-signal=2, candle-helper=1, schema=1, simulate=1, regime=1; got %d/%d/%d/%d/%d/%d/%d/%d/%d/%d/%d/%d (probed=%v)",
			hlSignal, hlFetchATR, hlExecute, hlLimitOpen, hlLimitStatus, hlCancelOrder, hlBatchCheck, spotSignal, candleHelper, schemaHelper, simulateHelper, regimeHelper, probed)
	}
}

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
