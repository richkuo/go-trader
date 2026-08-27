package main


import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func auditFloatPtr(v float64) *float64 { return &v }

func TestConfigValidationCompoundAllScalarStopOwnersRejected(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script: "check.py", Args: []string{"a", "ETH", "1h"},
			Capital: 1000, MaxDrawdownPct: 10, Leverage: 2,
			StopLossPct:         auditFloatPtr(5),
			StopLossMarginPct:   auditFloatPtr(20),
			TrailingStopPct:     auditFloatPtr(3),
			TrailingStopATRMult: auditFloatPtr(2),
			StopLossATRMult:     auditFloatPtr(1.5),
		}},
	}
	err := validateConfig(&cfg, false)
	if err == nil {
		t.Fatal("expected compound stop-owner config to be rejected")
	}
	msg := err.Error()
	for _, want := range []string{
		"stop_loss_pct and stop_loss_margin_pct are mutually exclusive",
		"trailing_stop_pct is mutually exclusive",
		"trailing_stop_atr_mult is mutually exclusive",
		"stop_loss_atr_mult is mutually exclusive",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("compound rejection missing %q in: %v", want, msg)
		}
	}
}

func TestConfigValidationCompoundScalarPlusRegimeStopOwnersRejected(t *testing.T) {
	raw := []byte(`{
		"id": "hl-eth", "type": "perps", "platform": "hyperliquid",
		"script": "check.py", "args": ["a", "ETH", "1h"],
		"capital": 1000, "max_drawdown_pct": 10, "leverage": 2,
		"stop_loss_atr_mult": 1.5,
		"stop_loss_atr_regime": {"trend_regime": {"trending_up": {"atr_multiple": 2.0}, "trending_down": {"atr_multiple": 2.0}, "ranging": {"atr_multiple": 1.0}}},
		"trailing_stop_atr_regime": {"trend_regime": {"trending_up": {"atr_multiple": 2.0}, "trending_down": {"atr_multiple": 2.0}, "ranging": {"atr_multiple": 1.0}}}
	}`)
	var sc StrategyConfig
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := Config{
		Regime:     &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 25},
		Strategies: []StrategyConfig{sc},
	}
	err := validateConfig(&cfg, false)
	if err == nil {
		t.Fatal("expected scalar+regime compound stop-owner config to be rejected")
	}
	msg := err.Error()
	for _, want := range []string{
		"stop_loss_atr_regime is mutually exclusive with stop_loss_atr_mult",
		"trailing_stop_atr_regime is mutually exclusive with stop_loss_atr_mult",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("compound rejection missing %q in: %v", want, msg)
		}
	}
}

func TestUnmarshalStrategyConfigCanonicalCloseWinsOverLegacyArray(t *testing.T) {
	raw := []byte(`{
		"id": "s", "type": "spot", "script": "check.py", "capital": 100,
		"max_drawdown_pct": 10,
		"close_strategy": {"name": "atr_stop"},
		"close_strategies": [{"name": "tiered_tp_atr"}, {"name": "atr_stop"}]
	}`)
	var sc StrategyConfig
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sc.CloseStrategy == nil || sc.CloseStrategy.Name != "atr_stop" {
		t.Fatalf("canonical close_strategy should win, got %+v", sc.CloseStrategy)
	}
	if len(sc.closeStrategiesLegacy) != 0 {
		t.Fatalf("legacy array should be dropped when canonical key present, got %v", sc.closeStrategiesLegacy)
	}
	cfg := Config{Strategies: []StrategyConfig{sc}}
	if err := validateConfig(&cfg, false); err != nil {
		t.Fatalf("canonical+legacy mix should validate, got: %v", err)
	}
}

func TestUnmarshalStrategyConfigLegacyArrayLenTwoStillRejected(t *testing.T) {
	raw := []byte(`{
		"id": "s", "type": "spot", "script": "check.py", "capital": 100,
		"max_drawdown_pct": 10,
		"close_strategies": [{"name": "tiered_tp_atr"}, {"name": "atr_stop"}]
	}`)
	var sc StrategyConfig
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg := Config{Strategies: []StrategyConfig{sc}}
	err := validateConfig(&cfg, false)
	if err == nil || !strings.Contains(err.Error(), "close_strategies has 2 entries") {
		t.Fatalf("expected len>1 legacy close_strategies rejection, got: %v", err)
	}
}

func TestMigrateConfigNewerVersionPreservesAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	src := map[string]interface{}{
		"config_version": CurrentConfigVersion + 1,
		"discord": map[string]interface{}{
			"spot_summary_freq": "hourly",
		},
		"future_top_level_field": "keep-me",
		"strategies":             []interface{}{},
	}
	data, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateConfig(path, nil, nil); err != nil {
		t.Fatalf("MigrateConfig on newer version: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if v, _ := got["config_version"].(float64); int(v) != CurrentConfigVersion {
		t.Errorf("config_version restamped to %v, want %d", got["config_version"], CurrentConfigVersion)
	}
	if got["future_top_level_field"] != "keep-me" {
		t.Errorf("unknown future field dropped: %v", got["future_top_level_field"])
	}
	discord, _ := got["discord"].(map[string]interface{})
	if discord == nil || discord["spot_summary_freq"] != "hourly" {
		t.Errorf("versioned removal step ran on a newer config: %v", got["discord"])
	}
}

func TestCanonicalizeRegimeBlockAliasPrecedenceDeterministic(t *testing.T) {
	for i := 0; i < 50; i++ {
		block := map[string]interface{}{
			"trend_regime": map[string]interface{}{
				"trending": map[string]interface{}{
					"atr":      2.0,
					"multiple": 3.0,
				},
			},
		}
		out := canonicalizeRegimeBlock(block)
		tr := out["trend_regime"].(map[string]interface{})
		label := tr["trending"].(map[string]interface{})
		if label["atr_multiple"] != 2.0 {
			t.Fatalf("iteration %d: legacy alias fold nondeterministic or wrong precedence: got %v, want 2.0 (atr > multiple)", i, label["atr_multiple"])
		}
	}
	block := map[string]interface{}{
		"trend_regime": map[string]interface{}{
			"trending": map[string]interface{}{
				"atr_multiple": 1.5,
				"atr":          2.0,
				"multiple":     3.0,
			},
		},
	}
	out := canonicalizeRegimeBlock(block)
	label := out["trend_regime"].(map[string]interface{})["trending"].(map[string]interface{})
	if label["atr_multiple"] != 1.5 {
		t.Fatalf("canonical atr_multiple should win over legacy aliases, got %v", label["atr_multiple"])
	}
	block = map[string]interface{}{
		"trend_regime": map[string]interface{}{
			"trending": map[string]interface{}{
				"close_fraction": 0.5,
				"fraction":       0.9,
			},
		},
	}
	out = canonicalizeRegimeBlock(block)
	label = out["trend_regime"].(map[string]interface{})["trending"].(map[string]interface{})
	if label["close_fraction"] != 0.5 {
		t.Fatalf("canonical close_fraction should win over legacy fraction, got %v", label["close_fraction"])
	}
}

func TestApplyHotReloadConfigAllowsLeverageChangeWhenFlat(t *testing.T) {
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 2,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"a", "ETH", "1h"}, Capital: 1000, MaxDrawdownPct: 10, Leverage: 5,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}
	if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
		t.Fatalf("flat leverage change should hot-reload, got: %v", err)
	}
	if cfg.Strategies[0].Leverage != 5 {
		t.Fatalf("leverage not applied when flat: %v", cfg.Strategies[0].Leverage)
	}
}
