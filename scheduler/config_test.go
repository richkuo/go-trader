package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"strategies": [{
			"id": "test-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000
		}]
	}`
	path := writeTestConfig(t, dir, cfg)

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if loaded.IntervalSeconds != 600 {
		t.Errorf("IntervalSeconds = %d, want 600 (default)", loaded.IntervalSeconds)
	}
	if loaded.LogDir != "logs" {
		t.Errorf("LogDir = %q, want %q", loaded.LogDir, "logs")
	}
	if loaded.StateFile != "scheduler/state.json" {
		t.Errorf("StateFile = %q, want %q", loaded.StateFile, "scheduler/state.json")
	}
	if loaded.AutoUpdate != "off" {
		t.Errorf("AutoUpdate = %q, want %q", loaded.AutoUpdate, "off")
	}
}

func TestLoadConfigPlatformInference(t *testing.T) {
	cases := []struct {
		id       string
		wantPlat string
	}{
		{"hl-btc-sma", "hyperliquid"},
		{"ibkr-btc-vol", "ibkr"},
		{"deribit-btc-cc", "deribit"},
		{"ts-es-sma", "topstep"},
		{"rh-btc-sma", "robinhood"},
		{"okx-btc-sma", "okx"},
		{"luno-btc-sma", "luno"},
		{"spot-btc-sma", "binanceus"},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			dir := t.TempDir()
			cfg := `{
				"strategies": [{
					"id": "` + tc.id + `",
					"type": "spot",
					"script": "shared_scripts/check_strategy.py",
					"args": ["sma_crossover", "BTC/USDT", "1h"],
					"capital": 1000
				}]
			}`
			path := writeTestConfig(t, dir, cfg)

			loaded, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig failed: %v", err)
			}
			if loaded.Strategies[0].Platform != tc.wantPlat {
				t.Errorf("Platform = %q, want %q", loaded.Strategies[0].Platform, tc.wantPlat)
			}
		})
	}
}

func TestLoadConfigMaxDrawdownDefaults(t *testing.T) {
	cases := []struct {
		stratType string
		wantDD    float64
	}{
		{"spot", 60},
		{"options", 40},
		{"perps", 50},
		{"futures", 45},
	}

	for _, tc := range cases {
		t.Run(tc.stratType, func(t *testing.T) {
			dir := t.TempDir()
			cfg := `{
				"strategies": [{
					"id": "test-` + tc.stratType + `",
					"type": "` + tc.stratType + `",
					"script": "shared_scripts/check_strategy.py",
					"args": ["sma_crossover", "BTC/USDT", "1h"],
					"capital": 1000
				}]
			}`
			path := writeTestConfig(t, dir, cfg)

			loaded, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig failed: %v", err)
			}
			if loaded.Strategies[0].MaxDrawdownPct != tc.wantDD {
				t.Errorf("MaxDrawdownPct = %g, want %g", loaded.Strategies[0].MaxDrawdownPct, tc.wantDD)
			}
		})
	}
}

func TestLoadConfigThetaHarvestDefault(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"strategies": [{
			"id": "test-options",
			"type": "options",
			"script": "shared_scripts/check_options.py",
			"args": ["vol_crush", "BTC", "1h"],
			"capital": 5000
		}]
	}`
	path := writeTestConfig(t, dir, cfg)

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	th := loaded.Strategies[0].ThetaHarvest
	if th == nil {
		t.Fatal("ThetaHarvest should be defaulted for options")
	}
	if !th.Enabled {
		t.Error("ThetaHarvest.Enabled should default to true")
	}
	if th.ProfitTargetPct != 60 {
		t.Errorf("ProfitTargetPct = %g, want 60", th.ProfitTargetPct)
	}
	if th.StopLossPct != 200 {
		t.Errorf("StopLossPct = %g, want 200", th.StopLossPct)
	}
	if th.MinDTEClose != 3 {
		t.Errorf("MinDTEClose = %g, want 3", th.MinDTEClose)
	}
}

func TestLoadConfigPortfolioRiskDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"strategies": [{
			"id": "test-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000
		}]
	}`
	path := writeTestConfig(t, dir, cfg)

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if loaded.PortfolioRisk == nil {
		t.Fatal("PortfolioRisk should be defaulted")
	}
	if loaded.PortfolioRisk.MaxDrawdownPct != 25 {
		t.Errorf("MaxDrawdownPct = %g, want 25", loaded.PortfolioRisk.MaxDrawdownPct)
	}
	if loaded.PortfolioRisk.WarnThresholdPct != 80 {
		t.Errorf("WarnThresholdPct = %g, want 80", loaded.PortfolioRisk.WarnThresholdPct)
	}
}

func TestLoadConfigCorrelationDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"strategies": [{
			"id": "test-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000
		}]
	}`
	path := writeTestConfig(t, dir, cfg)

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if loaded.Correlation == nil {
		t.Fatal("Correlation should be defaulted")
	}
	if loaded.Correlation.Enabled {
		t.Error("Correlation.Enabled should default to false")
	}
	if loaded.Correlation.MaxConcentrationPct != 60 {
		t.Errorf("MaxConcentrationPct = %g, want 60", loaded.Correlation.MaxConcentrationPct)
	}
	if loaded.Correlation.MaxSameDirectionPct != 75 {
		t.Errorf("MaxSameDirectionPct = %g, want 75", loaded.Correlation.MaxSameDirectionPct)
	}
}

func TestLoadConfigEnvVarOverrides(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"discord": {"enabled": true, "token": "file-token", "channels": {"spot": "123"}},
		"strategies": [{
			"id": "test-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000
		}]
	}`
	path := writeTestConfig(t, dir, cfg)

	t.Setenv("DISCORD_BOT_TOKEN", "env-token")
	t.Setenv("DISCORD_OWNER_ID", "owner123")
	t.Setenv("STATUS_AUTH_TOKEN", "secret")

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if loaded.Discord.Token != "env-token" {
		t.Errorf("Discord.Token = %q, want %q", loaded.Discord.Token, "env-token")
	}
	if loaded.Discord.OwnerID != "owner123" {
		t.Errorf("Discord.OwnerID = %q, want %q", loaded.Discord.OwnerID, "owner123")
	}
	if loaded.StatusToken != "secret" {
		t.Errorf("StatusToken = %q, want %q", loaded.StatusToken, "secret")
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, "not valid json")
	_, err := LoadConfig(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestValidateConfigErrors(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "empty id",
			cfg: Config{
				Strategies: []StrategyConfig{{
					Type: "spot", Script: "check.py", Capital: 100, MaxDrawdownPct: 10,
				}},
			},
			wantErr: "id is empty",
		},
		{
			name: "duplicate id",
			cfg: Config{
				Strategies: []StrategyConfig{
					{ID: "dup", Type: "spot", Script: "check.py", Capital: 100, MaxDrawdownPct: 10},
					{ID: "dup", Type: "spot", Script: "check.py", Capital: 100, MaxDrawdownPct: 10},
				},
			},
			wantErr: "duplicate id",
		},
		{
			name: "empty script",
			cfg: Config{
				Strategies: []StrategyConfig{{
					ID: "test", Type: "spot", Capital: 100, MaxDrawdownPct: 10,
				}},
			},
			wantErr: "script is empty",
		},
		{
			name: "absolute script path",
			cfg: Config{
				Strategies: []StrategyConfig{{
					ID: "test", Type: "spot", Script: "/abs/path.py", Capital: 100, MaxDrawdownPct: 10,
				}},
			},
			wantErr: "relative path",
		},
		{
			name: "script not .py",
			cfg: Config{
				Strategies: []StrategyConfig{{
					ID: "test", Type: "spot", Script: "check.sh", Capital: 100, MaxDrawdownPct: 10,
				}},
			},
			wantErr: ".py",
		},
		{
			name: "invalid type",
			cfg: Config{
				Strategies: []StrategyConfig{{
					ID: "test", Type: "invalid", Script: "check.py", Capital: 100, MaxDrawdownPct: 10,
				}},
			},
			wantErr: "type must be",
		},
		{
			name: "zero capital no pct",
			cfg: Config{
				Strategies: []StrategyConfig{{
					ID: "test", Type: "spot", Script: "check.py", Capital: 0, MaxDrawdownPct: 10,
				}},
			},
			wantErr: "capital must be > 0",
		},
		{
			name: "invalid drawdown",
			cfg: Config{
				Strategies: []StrategyConfig{{
					ID: "test", Type: "spot", Script: "check.py", Capital: 100, MaxDrawdownPct: 0,
				}},
			},
			wantErr: "max_drawdown_pct",
		},
		{
			name: "capital_pct out of range",
			cfg: Config{
				Strategies: []StrategyConfig{{
					ID: "test", Type: "spot", Script: "check.py", CapitalPct: 1.5, MaxDrawdownPct: 10,
				}},
			},
			wantErr: "capital_pct must be in (0, 1]",
		},
		{
			name: "negative interval",
			cfg: Config{
				Strategies: []StrategyConfig{{
					ID: "test", Type: "spot", Script: "check.py", Capital: 100, MaxDrawdownPct: 10, IntervalSeconds: -1,
				}},
			},
			wantErr: "interval_seconds",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateConfig(&tc.cfg)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestValidateConfigValidConfig(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:             "test-spot",
			Type:           "spot",
			Script:         "shared_scripts/check_strategy.py",
			Capital:        1000,
			MaxDrawdownPct: 60,
		}},
		PortfolioRisk: &PortfolioRiskConfig{
			MaxDrawdownPct:   25,
			WarnThresholdPct: 80,
		},
	}

	if err := ValidateConfig(&cfg); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestValidateConfigPortfolioRisk(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID: "test", Type: "spot", Script: "check.py", Capital: 100, MaxDrawdownPct: 10,
		}},
		PortfolioRisk: &PortfolioRiskConfig{
			MaxDrawdownPct:   0, // invalid
			WarnThresholdPct: 80,
		},
	}

	err := ValidateConfig(&cfg)
	if err == nil {
		t.Fatal("expected error for invalid portfolio risk")
	}
	if !strings.Contains(err.Error(), "portfolio_risk.max_drawdown_pct") {
		t.Errorf("error should mention portfolio_risk.max_drawdown_pct: %v", err)
	}
}
