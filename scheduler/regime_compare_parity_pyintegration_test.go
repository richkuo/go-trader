//go:build pyintegration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// regimeCompareParityDriver checks the comparison resolvers against the live
// Go resolvers. The comparison must not grow a second definition of window
// selection, lookback, or certification identity.
const regimeCompareParityDriver = `import json
import sys

root = sys.argv[1]
sys.path.insert(0, root + "/backtest")
sys.path.insert(0, root + "/shared_tools")

from directional_certification import directional_cert_identity
from regime import required_ohlcv_limit, resolve_strategy_regime_window

req = json.load(sys.stdin)
out = []
for case in req["cases"]:
    regime = case["regime"]
    out.append({
        "window": resolve_strategy_regime_window(case["strategy"], case["field"], regime),
        "limit": required_ohlcv_limit(int(regime.get("period") or 14), regime.get("windows") or None),
        "identity": directional_cert_identity(case["strategy"], regime),
    })
json.dump(out, sys.stdout)
`

type regimeParityCase struct {
	Field    string
	Strategy StrategyConfig
	Regime   *RegimeConfig
	Python   map[string]any
}

func TestRegimeCompareResolversMatchLive(t *testing.T) {
	windows := RegimeWindowsMap{
		"short":  {Classifier: "adx", Period: 8, ADXThreshold: 20},
		"medium": {Classifier: "composite", Period: 28},
	}
	multi := &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20, Windows: windows}
	off := &RegimeConfig{Enabled: false, Period: 14}
	longPeriod := &RegimeConfig{Enabled: true, Period: 120}
	override := &RegimeConfig{Enabled: true, Period: 14, Timeframe: "4H", Windows: windows}
	cases := []regimeParityCase{
		{Field: "gate", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}}, Regime: multi},
		{Field: "gate", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}, RegimeGateWindow: "Short"}, Regime: multi},
		{Field: "directional", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}, RegimeDirectionalWindow: "short"}, Regime: off},
		{Field: "atr", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}}, Regime: off},
		{Field: "directional", Strategy: StrategyConfig{Args: []string{"sma", "BTC/USDT", "1h"}}, Regime: multi},
		{Field: "directional", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}, RegimeDirectionalWindow: "short"}, Regime: override},
		{Field: "gate", Strategy: StrategyConfig{Args: []string{"sma", "BTC", "1h"}}, Regime: longPeriod},
	}
	payload := map[string]any{"cases": []map[string]any{}}
	for _, c := range cases {
		body, err := json.Marshal(c.Regime)
		if err != nil {
			t.Fatal(err)
		}
		var regime map[string]any
		if err := json.Unmarshal(body, &regime); err != nil {
			t.Fatal(err)
		}
		strategy := map[string]any{
			"args":                      c.Strategy.Args,
			"regime_gate_window":        c.Strategy.RegimeGateWindow,
			"regime_atr_window":         c.Strategy.RegimeATRWindow,
			"regime_directional_window": c.Strategy.RegimeDirectionalWindow,
		}
		payload["cases"] = append(payload["cases"].([]map[string]any), map[string]any{
			"field": c.Field, "strategy": strategy, "regime": regime,
		})
	}
	got := runRegimeCompareParityDriver(t, payload)
	if len(got) != len(cases) {
		t.Fatalf("driver returned %d cases, want %d", len(got), len(cases))
	}
	for i, c := range cases {
		window := resolveStrategyRegimeWindow(c.Strategy, c.Field, c.Regime)
		if got[i].Window != window {
			t.Fatalf("case %d window: python %q go %q", i, got[i].Window, window)
		}
		limit := regimeRequiredOhlcvLimit(c.Regime)
		if got[i].Limit != limit {
			t.Fatalf("case %d lookback: python %d go %d", i, got[i].Limit, limit)
		}
		asset, tf, classifier, ok := directionalCertIdentity(c.Strategy, c.Regime)
		if !ok {
			t.Fatalf("case %d: Go identity did not resolve", i)
		}
		if got[i].Identity.Asset != asset || got[i].Identity.Timeframe != tf || got[i].Identity.Classifier != classifier {
			t.Fatalf("case %d identity: python %+v go %s %s %s", i, got[i].Identity, asset, tf, classifier)
		}
	}
}

type regimeParityIdentity struct {
	Asset      string `json:"asset"`
	Timeframe  string `json:"timeframe"`
	Classifier string `json:"classifier"`
}

type regimeParityResult struct {
	Window   string               `json:"window"`
	Limit    int                  `json:"limit"`
	Identity regimeParityIdentity `json:"identity"`
}

func runRegimeCompareParityDriver(t *testing.T, req any) []regimeParityResult {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	python := filepath.Join(root, ".venv", "bin", "python3")
	if _, err := os.Stat(python); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	dir := t.TempDir()
	driver := filepath.Join(dir, "regime_compare_parity_driver.py")
	if err := os.WriteFile(driver, []byte(regimeCompareParityDriver), 0o600); err != nil {
		t.Fatalf("write driver: %v", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	cmd := exec.Command(python, "-I", driver, root)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("parity driver: %v\n%s", err, stderr.String())
	}
	var out []regimeParityResult
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("parse driver output: %v\n%s", err, stdout.String())
	}
	return out
}
