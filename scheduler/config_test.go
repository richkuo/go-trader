package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if loaded.DBFile != "scheduler/state.db" {
		t.Errorf("DBFile = %q, want %q", loaded.DBFile, "scheduler/state.db")
	}
	if loaded.AutoUpdate != "off" {
		t.Errorf("AutoUpdate = %q, want %q", loaded.AutoUpdate, "off")
	}
	if loaded.DefaultStopLossATRMult == nil || *loaded.DefaultStopLossATRMult != DefaultStopLossATRMult {
		t.Errorf("DefaultStopLossATRMult = %v, want %g", loaded.DefaultStopLossATRMult, DefaultStopLossATRMult)
	}
}

func TestLoadConfigRegimeTimeframe(t *testing.T) {
	t.Run("normalizes accepted timeframe", func(t *testing.T) {
		dir := t.TempDir()
		cfg := `{
			"regime": {"enabled": true, "timeframe": " 1D "},
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
		if loaded.Regime == nil || loaded.Regime.Timeframe != "1d" {
			t.Fatalf("Regime.Timeframe = %v, want 1d", loaded.Regime)
		}
	})

	t.Run("rejects unknown timeframe", func(t *testing.T) {
		dir := t.TempDir()
		cfg := `{
			"regime": {"enabled": true, "timeframe": "7h"},
			"strategies": [{
				"id": "test-spot",
				"type": "spot",
				"script": "shared_scripts/check_strategy.py",
				"args": ["sma_crossover", "BTC/USDT", "1h"],
				"capital": 1000
			}]
		}`
		path := writeTestConfig(t, dir, cfg)

		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("expected invalid regime.timeframe to be rejected")
		}
		if !strings.Contains(err.Error(), "regime.timeframe") {
			t.Fatalf("error = %v, want regime.timeframe", err)
		}
	})
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
	if loaded.PortfolioRisk.WarnThresholdPct != 60 {
		t.Errorf("WarnThresholdPct = %g, want 60", loaded.PortfolioRisk.WarnThresholdPct)
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

func TestConfigValidationErrors(t *testing.T) {
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
			name: "capital_pct hyperliquid missing account address",
			cfg: Config{
				Strategies: []StrategyConfig{{
					ID: "hl-test", Type: "perps", Script: "check.py", Platform: "hyperliquid", CapitalPct: 0.5, MaxDrawdownPct: 10,
				}},
			},
			wantErr: "capital_pct requires HYPERLIQUID_ACCOUNT_ADDRESS env var",
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
			err := validateConfig(&tc.cfg, false)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestConfigValidationValidConfig(t *testing.T) {
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

	if err := validateConfig(&cfg, false); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestConfigValidationOpenCloseFields(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:             "test-spot",
			Type:           "spot",
			Platform:       "binanceus",
			Script:         "shared_scripts/check_strategy.py",
			Args:           []string{"sma_crossover", "BTC/USDT", "1h"},
			OpenStrategy:   StrategyRef{Name: "momentum"},
			CloseStrategy:  &StrategyRef{Name: "rsi"},
			Capital:        1000,
			MaxDrawdownPct: 60,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := validateConfig(&cfg, false); err != nil {
		t.Fatalf("expected valid open/close config, got: %v", err)
	}
}

func TestLoadConfigRejectsMultipleCloseStrategies(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"config_version": 14,
		"strategies": [{
			"id": "multi-close",
			"type": "spot",
			"platform": "binanceus",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"open_strategy": {"name": "momentum"},
			"close_strategies": [{"name": "rsi"}, {"name": "macd"}],
			"capital": 1000,
			"max_drawdown_pct": 60
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected rejection of multi-entry close_strategies array")
	}
	if !strings.Contains(err.Error(), "single close_strategy") {
		t.Fatalf("error %q should explain the #842 single-close collapse", err.Error())
	}
}

func TestConfigValidationOpenCloseRejectsOptions(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:             "test-options",
			Type:           "options",
			Platform:       "deribit",
			Script:         "shared_scripts/check_options.py",
			Args:           []string{"vol_mean_reversion", "BTC", "1h"},
			CloseStrategy:  &StrategyRef{Name: "rsi"},
			Capital:        1000,
			MaxDrawdownPct: 40,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := validateConfig(&cfg, false)
	if err == nil {
		t.Fatal("expected options open/close validation error")
	}
	if !strings.Contains(err.Error(), "close_strategy") {
		t.Fatalf("error %q should mention close_strategy field", err.Error())
	}
}

func TestConfigValidationCloseStrategyName(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:             "test-spot",
			Type:           "spot",
			Platform:       "binanceus",
			Script:         "shared_scripts/check_strategy.py",
			Args:           []string{"sma_crossover", "BTC/USDT", "1h"},
			CloseStrategy:  &StrategyRef{Name: "bad name"},
			Capital:        1000,
			MaxDrawdownPct: 60,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := validateConfig(&cfg, false)
	if err == nil {
		t.Fatal("expected close strategy name validation error")
	}
	if !strings.Contains(err.Error(), "close_strategy") {
		t.Fatalf("error %q should mention close_strategy", err.Error())
	}
}

func TestConfigValidationOpenCloseDefersRegistryLookupToCheckScript(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:             "test-spot",
			Type:           "spot",
			Platform:       "binanceus",
			Script:         "shared_scripts/check_strategy.py",
			Args:           []string{"sma_crossover", "BTC/USDT", "1h"},
			OpenStrategy:   StrategyRef{Name: "not_a_strategy"},
			CloseStrategy:  &StrategyRef{Name: "rsi"},
			Capital:        1000,
			MaxDrawdownPct: 60,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := validateConfig(&cfg, false); err != nil {
		t.Fatalf("syntactically valid strategy names should be accepted by config validation: %v", err)
	}
}

func TestConfigValidationPortfolioRisk(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID: "test", Type: "spot", Script: "check.py", Capital: 100, MaxDrawdownPct: 10,
		}},
		PortfolioRisk: &PortfolioRiskConfig{
			MaxDrawdownPct:   0,
			WarnThresholdPct: 80,
		},
	}

	err := validateConfig(&cfg, false)
	if err == nil {
		t.Fatal("expected error for invalid portfolio risk")
	}
	if !strings.Contains(err.Error(), "portfolio_risk.max_drawdown_pct") {
		t.Errorf("error should mention portfolio_risk.max_drawdown_pct: %v", err)
	}
}

func TestParseLeaderboardPostTime(t *testing.T) {
	tests := []struct {
		input  string
		wantH  int
		wantM  int
		wantOK bool
	}{
		{"11:00", 11, 0, true},
		{"09:30", 9, 30, true},
		{"23:59", 23, 59, true},
		{"00:00", 0, 0, true},
		{"", 0, 0, false},
		{"25:00", 0, 0, false},
		{"12:61", 0, 0, false},
		{"noon", 0, 0, false},
		{"12", 0, 0, false},
		{"-1:00", 0, 0, false},
		{"12:-5", 0, 0, false},
		{"1a:00", 0, 0, false},
		{" 5:00", 0, 0, false},
		{"12:3x", 0, 0, false},
	}
	for _, tt := range tests {
		h, m, ok := ParseLeaderboardPostTime(tt.input)
		if ok != tt.wantOK {
			t.Errorf("ParseLeaderboardPostTime(%q): ok=%v, want %v", tt.input, ok, tt.wantOK)
			continue
		}
		if ok && (h != tt.wantH || m != tt.wantM) {
			t.Errorf("ParseLeaderboardPostTime(%q) = (%d, %d), want (%d, %d)", tt.input, h, m, tt.wantH, tt.wantM)
		}
	}
}

func TestConfigValidationLeaderboardPostTime(t *testing.T) {
	base := Config{
		Strategies: []StrategyConfig{{
			ID: "test", Type: "spot", Script: "shared_scripts/check_strategy.py",
			Args: []string{"sma_crossover", "BTC/USDT", "1h"}, Capital: 1000, MaxDrawdownPct: 60,
			Platform: "binanceus",
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}

	cfg := base
	cfg.LeaderboardPostTime = "11:00"
	if err := validateConfig(&cfg, false); err != nil {
		t.Errorf("expected valid config with leaderboard_post_time=11:00, got: %v", err)
	}

	cfg2 := base
	cfg2.LeaderboardPostTime = ""
	if err := validateConfig(&cfg2, false); err != nil {
		t.Errorf("expected valid config with empty leaderboard_post_time, got: %v", err)
	}

	cfg3 := base
	cfg3.LeaderboardPostTime = "noon"
	err := validateConfig(&cfg3, false)
	if err == nil {
		t.Fatal("expected error for invalid leaderboard_post_time")
	}
	if !strings.Contains(err.Error(), "leaderboard_post_time") {
		t.Errorf("error should mention leaderboard_post_time: %v", err)
	}
}

func TestLoadConfigLeaderboardPostTime(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"leaderboard_post_time": "09:30",
		"strategies": [{
			"id": "test-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.LeaderboardPostTime != "09:30" {
		t.Errorf("LeaderboardPostTime = %q, want %q", cfg.LeaderboardPostTime, "09:30")
	}
}

func TestEffectiveInitialCapital(t *testing.T) {
	tests := []struct {
		name string
		sc   StrategyConfig
		ss   *StrategyState
		want float64
	}{
		{
			name: "config initial_capital takes priority",
			sc:   StrategyConfig{Capital: 600, InitialCapital: 500},
			ss:   &StrategyState{InitialCapital: 550},
			want: 500,
		},
		{
			name: "state initial_capital when config not set",
			sc:   StrategyConfig{Capital: 600},
			ss:   &StrategyState{InitialCapital: 550},
			want: 550,
		},
		{
			name: "falls back to config capital",
			sc:   StrategyConfig{Capital: 600},
			ss:   &StrategyState{InitialCapital: 0},
			want: 600,
		},
		{
			name: "nil state falls back to config capital",
			sc:   StrategyConfig{Capital: 600},
			ss:   nil,
			want: 600,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveInitialCapital(tt.sc, tt.ss)
			if got != tt.want {
				t.Errorf("EffectiveInitialCapital() = %g, want %g", got, tt.want)
			}
		})
	}
}

func TestEffectiveInitialCapital_SharedWalletPoolHasNoAllocationBaseline(t *testing.T) {
	marginCap := 100.0
	sc := StrategyConfig{
		Platform: "hyperliquid", Type: "perps",
		Args:                   []string{"sma", "BTC", "1h", "--mode=live"},
		MarginPerTradeUSD:      &marginCap,
		sharedWalletPoolBudget: true,
	}
	ss := &StrategyState{InitialCapital: 500}
	if got := EffectiveInitialCapital(sc, ss); got != 0 {
		t.Fatalf("pooled initial capital=%v, want 0 even with a legacy persisted baseline", got)
	}
}

func TestConfigValidationInitialCapitalNegative(t *testing.T) {
	cfg := &Config{
		Strategies: []StrategyConfig{{
			ID:             "test",
			Type:           "spot",
			Script:         "test.py",
			Capital:        1000,
			InitialCapital: -100,
			MaxDrawdownPct: 10,
		}},
	}
	err := validateConfig(cfg, false)
	if err == nil {
		t.Fatal("expected error for negative initial_capital")
	}
	if !strings.Contains(err.Error(), "initial_capital") {
		t.Errorf("error should mention initial_capital: %v", err)
	}
}

func TestLoadConfigInitialCapital(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "sma-btc",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma", "BTC/USDT"],
			"capital": 600,
			"initial_capital": 505,
			"max_drawdown_pct": 10
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.InitialCapital != 505 {
		t.Errorf("InitialCapital = %g, want 505", sc.InitialCapital)
	}
	if sc.Capital != 600 {
		t.Errorf("Capital = %g, want 600 (should not be overwritten)", sc.Capital)
	}
}

func TestLoadConfigPerpsLeverageDefault(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.Leverage != 1 {
		t.Errorf("Leverage = %g, want 1 (default)", sc.Leverage)
	}
}

func TestLoadConfigPerpsLeverageExplicit(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 10
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Strategies[0].Leverage != 10 {
		t.Errorf("Leverage = %g, want 10", cfg.Strategies[0].Leverage)
	}
	if cfg.Strategies[0].SizingLeverage != 10 {
		t.Errorf("SizingLeverage = %g, want 10 (defaults to leverage)", cfg.Strategies[0].SizingLeverage)
	}
}

func TestLoadConfigPerpsSizingLeverageExplicit(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 20,
			"sizing_leverage": 2
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if got := EffectiveExchangeLeverage(sc); got != 20 {
		t.Errorf("EffectiveExchangeLeverage = %g, want 20", got)
	}
	if got := EffectiveSizingLeverage(sc); got != 2 {
		t.Errorf("EffectiveSizingLeverage = %g, want 2", got)
	}
}

func TestLoadConfigLeverageRejectsSpot(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "test-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000,
			"leverage": 5
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for leverage on spot strategy")
	}
	if !strings.Contains(err.Error(), "leverage is only supported for perps") {
		t.Errorf("error = %v, want 'leverage is only supported for perps'", err)
	}
}

func TestLoadConfigLeverageRejectsOutOfRange(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 150
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for leverage=150")
	}
	if !strings.Contains(err.Error(), "leverage must be in") {
		t.Errorf("error = %v, want 'leverage must be in'", err)
	}
}

func TestLoadConfigSizingLeverageRejectsSpot(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "test-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000,
			"sizing_leverage": 2
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for sizing_leverage on spot strategy")
	}
	if !strings.Contains(err.Error(), "sizing_leverage is only supported for perps") {
		t.Errorf("error = %v, want 'sizing_leverage is only supported for perps'", err)
	}
}

func TestLoadConfigSizingLeverageAcceptsFractional(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 20,
			"sizing_leverage": 0.5
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig with sizing_leverage=0.5 failed: %v", err)
	}
	if got := cfg.Strategies[0].SizingLeverage; got != 0.5 {
		t.Errorf("SizingLeverage = %g, want 0.5", got)
	}
}

func TestLoadConfigSizingLeverageRejectsOutOfRange(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 20,
			"sizing_leverage": 200
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for sizing_leverage=200")
	}
	if !strings.Contains(err.Error(), "sizing_leverage must be in") {
		t.Errorf("error = %v, want 'sizing_leverage must be in'", err)
	}
}

func TestLoadConfigMarginPerTradeUSDAccepted(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 20,
			"margin_per_trade_usd": 56
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig with margin_per_trade_usd=56 failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.MarginPerTradeUSD == nil || *sc.MarginPerTradeUSD != 56 {
		t.Errorf("MarginPerTradeUSD = %v, want pointer to 56", sc.MarginPerTradeUSD)
	}
	if got := EffectiveMarginPerTradeUSD(sc); got != 56 {
		t.Errorf("EffectiveMarginPerTradeUSD = %g, want 56", got)
	}
	if got := ComputePerpsOpenNotional(sc, 1000); got != 1120 {
		t.Errorf("ComputePerpsOpenNotional(cash=1000) = %g, want 1120 (56 × 20)", got)
	}
}

func TestLoadConfigMarginPerTradeUSDRejectsSpot(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "test-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000,
			"margin_per_trade_usd": 100
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for margin_per_trade_usd on spot strategy")
	}
	if !strings.Contains(err.Error(), "margin_per_trade_usd is only supported for perps") {
		t.Errorf("error = %v, want 'margin_per_trade_usd is only supported for perps'", err)
	}
}

func TestLoadConfigMarginPerTradeUSDRejectsNonPositive(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"leverage": 20,
			"margin_per_trade_usd": 0
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for margin_per_trade_usd=0")
	}
	if !strings.Contains(err.Error(), "margin_per_trade_usd must be positive") {
		t.Errorf("error = %v, want 'margin_per_trade_usd must be positive'", err)
	}
}

func TestEffectiveMarginPerTradeUSDOmittedReturnsZero(t *testing.T) {
	sc := StrategyConfig{Type: "perps", Leverage: 20, SizingLeverage: 1}
	if got := EffectiveMarginPerTradeUSD(sc); got != 0 {
		t.Errorf("EffectiveMarginPerTradeUSD(omitted) = %g, want 0", got)
	}
	if got := ComputePerpsOpenNotional(sc, 1000); got != 1000 {
		t.Errorf("ComputePerpsOpenNotional with omitted margin_per_trade_usd = %g, want 1000 (cash × sizing_leverage)", got)
	}
}

func TestLoadConfigHLPerpsDefaultsToIsolatedMargin(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Strategies[0].MarginMode != "isolated" {
		t.Errorf("MarginMode = %q, want %q (default)", cfg.Strategies[0].MarginMode, "isolated")
	}
}

func TestLoadConfigHLPerpsSingleStrategyAutoDerivesStopLoss(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 10,
			"leverage": 5
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult == nil {
		t.Fatal("StopLossATRMult = nil, want default 1.0 applied")
	}
	if got := *sc.StopLossATRMult; got != DefaultStopLossATRMult {
		t.Errorf("StopLossATRMult = %g, want %g (default)", got, DefaultStopLossATRMult)
	}
	if got := EffectiveStopLossPct(sc); got != 0 {
		t.Errorf("EffectiveStopLossPct = %g, want 0 (deferred until EntryATR is stamped)", got)
	}
}

func TestLoadConfigManualDefaultsStopLossATRMultTo2Point0(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult == nil {
		t.Fatal("StopLossATRMult = nil, want 2.0 default applied")
	}
	if got := *sc.StopLossATRMult; got != defaultManualStopLossATRMult {
		t.Errorf("StopLossATRMult = %g, want %g", got, defaultManualStopLossATRMult)
	}
}

func TestLoadConfigManualOptsOutWhenGlobalDefaultIsZero(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"default_stop_loss_atr_mult": 0,
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult != nil {
		t.Errorf("StopLossATRMult = %v, want nil (default_stop_loss_atr_mult=0 is the global opt-out)", *sc.StopLossATRMult)
	}
}

func TestLoadConfigManualExplicitATRMultPreserved(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20,
			"stop_loss_atr_mult": 2.5
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult == nil {
		t.Fatal("StopLossATRMult = nil, want explicit 2.5 preserved")
	}
	if got := *sc.StopLossATRMult; got != 2.5 {
		t.Errorf("StopLossATRMult = %g, want 2.5 (explicit value)", got)
	}
}

func TestLoadConfigManualDefaultsStopLossATRMultOverride(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"user_defaults": {"manual": {"stop_loss_atr_mult": 2.25}},
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult == nil {
		t.Fatal("StopLossATRMult = nil, want 2.25 from user_defaults.manual")
	}
	if got := *sc.StopLossATRMult; got != 2.25 {
		t.Errorf("StopLossATRMult = %g, want 2.25 (user_defaults.manual override)", got)
	}
}

func TestLoadConfigManualDefaultsStopLossATRMultZeroOptsOut(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"user_defaults": {"manual": {"stop_loss_atr_mult": 0}},
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult != nil {
		t.Errorf("StopLossATRMult = %v, want nil (user_defaults.manual.stop_loss_atr_mult=0 opts manual strategies out)", *sc.StopLossATRMult)
	}
}

func TestLoadConfigManualDefaultsTPTiersOverride(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"user_defaults": {
			"manual": {
				"tp_tiers": [
					{"atr_multiple": 1.5, "close_fraction": 0.4},
					{"atr_multiple": 2.5, "close_fraction": 1.0}
				]
			}
		},
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.CloseStrategy == nil || sc.CloseStrategy.Name != "tiered_tp_atr_live" {
		t.Fatalf("CloseStrategy = %+v, want single tiered_tp_atr_live entry", sc.CloseStrategy)
	}
	tiersAny, ok := sc.CloseStrategy.Params["tp_tiers"]
	if !ok {
		t.Fatal("close strategy params missing tp_tiers")
	}
	tiers, ok := tiersAny.([]interface{})
	if !ok {
		t.Fatalf("tiers = %T, want []interface{}", tiersAny)
	}
	if len(tiers) != 2 {
		t.Fatalf("tiers length = %d, want 2", len(tiers))
	}
	want := []struct{ atrMult, frac float64 }{{1.5, 0.4}, {2.5, 1.0}}
	for i, t0 := range tiers {
		m, ok := t0.(map[string]interface{})
		if !ok {
			t.Fatalf("tier[%d] = %T, want map[string]interface{}", i, t0)
		}
		if got := m["atr_multiple"].(float64); got != want[i].atrMult {
			t.Errorf("tier[%d].atr_multiple = %g, want %g", i, got, want[i].atrMult)
		}
		if got := m["close_fraction"].(float64); got != want[i].frac {
			t.Errorf("tier[%d].close_fraction = %g, want %g", i, got, want[i].frac)
		}
	}
}

func TestLoadConfigManualDefaultsTPTiersDoesNotOverrideExplicit(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"config_version": 14,
		"user_defaults": {
			"manual": {
				"tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 1.0}]
			}
		},
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20,
			"close_strategies": [{
				"name": "tiered_tp_atr_live",
				"params": {"tp_tiers": [{"atr_multiple": 5.0, "close_fraction": 1.0}]}
			}]
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	tiers := cfg.Strategies[0].CloseStrategy.Params["tp_tiers"].([]interface{})
	if len(tiers) != 1 {
		t.Fatalf("tiers length = %d, want 1 (explicit override)", len(tiers))
	}
	got := tiers[0].(map[string]interface{})["atr_multiple"].(float64)
	if got != 5.0 {
		t.Errorf("tier[0].atr_multiple = %g, want 5.0 (explicit, not user_defaults.manual)", got)
	}
}

func TestLoadConfigManualDefaultsAbsentPreservesHardcodedDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	sc := cfg.Strategies[0]
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != defaultManualStopLossATRMult {
		t.Errorf("StopLossATRMult = %v, want %g (hardcoded fallback)", sc.StopLossATRMult, defaultManualStopLossATRMult)
	}
	tiers := sc.CloseStrategy.Params["tp_tiers"].([]interface{})
	if len(tiers) != 2 {
		t.Fatalf("tiers length = %d, want 2 (hardcoded fallback)", len(tiers))
	}
	t0 := tiers[0].(map[string]interface{})
	t1 := tiers[1].(map[string]interface{})
	if t0["atr_multiple"].(float64) != 2.0 || t0["close_fraction"].(float64) != 0.5 {
		t.Errorf("tier[0] = %+v, want {2.0, 0.5}", t0)
	}
	if t1["atr_multiple"].(float64) != 3.0 || t1["close_fraction"].(float64) != 1.0 {
		t.Errorf("tier[1] = %+v, want {3.0, 1.0}", t1)
	}
}

func TestLoadConfigManualDefaultsValidation(t *testing.T) {
	cases := []struct {
		name      string
		block     string
		wantError string
	}{
		{"margin negative", `"margin_usd": -1`, "user_defaults.manual.margin_usd"},
		{"margin zero", `"margin_usd": 0`, "user_defaults.manual.margin_usd"},
		{"slmult negative", `"stop_loss_atr_mult": -0.5`, "user_defaults.manual.stop_loss_atr_mult"},
		{"side invalid", `"side": "neutral"`, "user_defaults.manual.side"},
		{"tier atr zero", `"tp_tiers": [{"atr_multiple": 0, "close_fraction": 1.0}]`, "atr_multiple"},
		{"tier frac zero", `"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0}]`, "close_fraction"},
		{"tier frac over 1", `"tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 1.5}]`, "close_fraction"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgJSON := `{
				"user_defaults": {"manual": {` + tc.block + `}},
				"strategies": [{
					"id": "hl-manual-eth-live",
					"type": "manual",
					"platform": "hyperliquid",
					"symbol": "ETH",
					"timeframe": "1h",
					"capital": 1000,
					"leverage": 20,
					"max_drawdown_pct": 20
				}]
			}`
			path := writeTestConfig(t, dir, cfgJSON)
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatalf("LoadConfig accepted invalid user_defaults.manual block: %s", tc.block)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("error %q does not mention %q", err, tc.wantError)
			}
		})
	}
}

func TestConfigResolveManualHelpersFallback(t *testing.T) {
	var cfg Config
	if got := cfg.resolveManualMarginUSD(); got != defaultManualMarginUSD {
		t.Errorf("resolveManualMarginUSD = %g, want %g", got, defaultManualMarginUSD)
	}
	if got := cfg.resolveManualSide(); got != "long" {
		t.Errorf("resolveManualSide = %q, want %q", got, "long")
	}
	if got := cfg.resolveManualStopLossATRMult(); got != defaultManualStopLossATRMult {
		t.Errorf("resolveManualStopLossATRMult = %g, want %g", got, defaultManualStopLossATRMult)
	}
	if got := cfg.resolveManualRatchetFallbackATRMult(); got != defaultManualStopLossATRMult {
		t.Errorf("resolveManualRatchetFallbackATRMult = %g, want %g", got, defaultManualStopLossATRMult)
	}
	tiers := cfg.resolveManualTPTiers()
	if len(tiers) != 2 {
		t.Fatalf("resolveManualTPTiers length = %d, want 2", len(tiers))
	}
}

func TestConfigResolveManualHelpersFromConfig(t *testing.T) {
	margin := 125.0
	slMult := 2.0
	cfg := Config{
		UserDefaults: &UserDefaultsConfig{
			Manual: &ManualDefaultsConfig{
				MarginUSD:       &margin,
				StopLossATRMult: &slMult,
				Side:            "short",
				TPTiers: []ManualTPTier{
					{ATRMultiple: 1.0, CloseFraction: 0.3},
					{ATRMultiple: 2.0, CloseFraction: 0.7},
					{ATRMultiple: 4.0, CloseFraction: 1.0},
				},
			},
		},
	}
	if got := cfg.resolveManualMarginUSD(); got != 125.0 {
		t.Errorf("resolveManualMarginUSD = %g, want 125.0", got)
	}
	if got := cfg.resolveManualSide(); got != "short" {
		t.Errorf("resolveManualSide = %q, want %q", got, "short")
	}
	if got := cfg.resolveManualStopLossATRMult(); got != 2.0 {
		t.Errorf("resolveManualStopLossATRMult = %g, want 2.0", got)
	}
	if got := cfg.resolveManualRatchetFallbackATRMult(); got != 2.0 {
		t.Errorf("resolveManualRatchetFallbackATRMult = %g, want 2.0", got)
	}
	tiers := cfg.resolveManualTPTiers()
	if len(tiers) != 3 {
		t.Fatalf("resolveManualTPTiers length = %d, want 3", len(tiers))
	}
	mid := tiers[1].(map[string]interface{})
	if mid["atr_multiple"].(float64) != 2.0 || mid["close_fraction"].(float64) != 0.7 {
		t.Errorf("tier[1] = %+v, want {2.0, 0.7}", mid)
	}
}

func TestConfigResolveManualRatchetFallbackIgnoresZeroScalarOptOut(t *testing.T) {
	slMult := 0.0
	cfg := Config{UserDefaults: &UserDefaultsConfig{Manual: &ManualDefaultsConfig{StopLossATRMult: &slMult}}}
	if got := cfg.resolveManualStopLossATRMult(); got != 0 {
		t.Errorf("resolveManualStopLossATRMult = %g, want 0 scalar opt-out", got)
	}
	if got := cfg.resolveManualRatchetFallbackATRMult(); got != defaultManualStopLossATRMult {
		t.Errorf("resolveManualRatchetFallbackATRMult = %g, want %g no-naked fallback", got, defaultManualStopLossATRMult)
	}
}

func TestLoadConfigHLPerpsExplicitCross(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"margin_mode": "cross"
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Strategies[0].MarginMode != "cross" {
		t.Errorf("MarginMode = %q, want %q", cfg.Strategies[0].MarginMode, "cross")
	}
}

func TestLoadConfigMarginModeRejectsInvalidValue(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test-eth",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
			"capital": 1000,
			"margin_mode": "portfolio"
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for margin_mode=portfolio")
	}
	if !strings.Contains(err.Error(), "margin_mode must be") {
		t.Errorf("error = %v, want 'margin_mode must be'", err)
	}
}

func TestLoadConfigMarginModeRejectsSpot(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "test-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000,
			"margin_mode": "isolated"
		}]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for margin_mode on spot")
	}
	if !strings.Contains(err.Error(), "margin_mode is only supported for HL perps") {
		t.Errorf("error = %v, want 'margin_mode is only supported for HL perps'", err)
	}
}

func TestLoadConfigHLPerpsPeersOnSameCoinMatching(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 5,
				"margin_mode": "isolated"
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["donchian_breakout", "ETH", "4h", "--mode=paper"],
				"capital": 500,
				"leverage": 5,
				"margin_mode": "isolated"
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if len(cfg.Strategies) != 2 {
		t.Fatalf("expected 2 strategies, got %d", len(cfg.Strategies))
	}
	for _, sc := range cfg.Strategies {
		if got := EffectiveStopLossPct(sc); got != 0 {
			t.Errorf("%s EffectiveStopLossPct = %g, want 0 for omitted same-coin peer", sc.ID, got)
		}
	}
}

func TestLoadConfigHLPerpsPeersMismatchedMarginMode(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 5,
				"margin_mode": "isolated"
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["donchian_breakout", "ETH", "4h", "--mode=paper"],
				"capital": 500,
				"leverage": 5,
				"margin_mode": "cross"
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for mismatched margin_mode on peers")
	}
	if !strings.Contains(err.Error(), "disagree on margin_mode") {
		t.Errorf("error = %v, want 'disagree on margin_mode'", err)
	}
	if !strings.Contains(err.Error(), "ETH") {
		t.Errorf("error = %v, want mention of coin ETH", err)
	}
}

func TestLoadConfigHLPerpsPeersMismatchedLeverage(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 5,
				"margin_mode": "isolated"
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["donchian_breakout", "ETH", "4h", "--mode=paper"],
				"capital": 500,
				"leverage": 10,
				"margin_mode": "isolated"
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for mismatched leverage on peers")
	}
	if !strings.Contains(err.Error(), "disagree on leverage") {
		t.Errorf("error = %v, want 'disagree on leverage'", err)
	}
}

func TestLoadConfigHLPerpsPeersMultipleStopLossAllowed(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 5,
				"margin_mode": "isolated",
				"stop_loss_pct": 3.0
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["donchian_breakout", "ETH", "4h", "--mode=paper"],
				"capital": 500,
				"leverage": 5,
				"margin_mode": "isolated",
				"stop_loss_pct": 5.0
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
}

func TestLoadConfigHLPerpsPeersSingleStopLossAllowed(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 5,
				"margin_mode": "isolated",
				"stop_loss_pct": 3.0
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["donchian_breakout", "ETH", "4h", "--mode=paper"],
				"capital": 500,
				"leverage": 5,
				"margin_mode": "isolated"
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	got := map[string]float64{}
	for _, sc := range cfg.Strategies {
		got[sc.ID] = EffectiveStopLossPct(sc)
	}
	if got["hl-eth-trend"] != 3 {
		t.Errorf("explicit owner EffectiveStopLossPct = %g, want 3", got["hl-eth-trend"])
	}
	if got["hl-eth-breakout"] != 0 {
		t.Errorf("omitted peer EffectiveStopLossPct = %g, want 0", got["hl-eth-breakout"])
	}
}

func TestLoadConfigHLPerpsPeersDifferentCoinsIndependent(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 5,
				"margin_mode": "isolated"
			},
			{
				"id": "hl-btc-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "BTC", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 10,
				"margin_mode": "cross"
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
}

func TestLoadConfigHLPerpsPeersNoStopLossAllowed(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 5,
				"margin_mode": "isolated",
				"stop_loss_pct": 0
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["donchian_breakout", "ETH", "4h", "--mode=paper"],
				"capital": 500,
				"leverage": 5,
				"margin_mode": "isolated",
				"stop_loss_pct": 0
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig failed for two peers with stop_loss_pct:0: %v", err)
	}
}

func TestLoadConfigHLPerpsPeersDefaultedMarginModeMatches(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 5
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["donchian_breakout", "ETH", "4h", "--mode=paper"],
				"capital": 500,
				"leverage": 5,
				"margin_mode": "isolated"
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed for defaulted vs explicit margin_mode peers: %v", err)
	}
	for _, sc := range cfg.Strategies {
		if sc.MarginMode != "isolated" {
			t.Errorf("strategy %s margin_mode = %q, want %q", sc.ID, sc.MarginMode, "isolated")
		}
	}
}

func TestLoadConfigHLPerpsPeersOmittedStopLossDoesNotConflict(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [
			{
				"id": "hl-eth-trend",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
				"capital": 1000,
				"leverage": 5,
				"margin_mode": "isolated"
			},
			{
				"id": "hl-eth-breakout",
				"type": "perps",
				"platform": "hyperliquid",
				"script": "shared_scripts/check_hyperliquid.py",
				"args": ["donchian_breakout", "ETH", "4h", "--mode=paper"],
				"capital": 500,
				"leverage": 5,
				"margin_mode": "isolated"
			}
		]
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed for omitted same-coin stop_loss_* peers: %v", err)
	}
	for _, sc := range cfg.Strategies {
		if got := EffectiveStopLossPct(sc); got != 0 {
			t.Errorf("%s EffectiveStopLossPct = %g, want 0", sc.ID, got)
		}
	}
}

func TestConfigValidationDMChannelsInvalidKey(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "t-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000,
			"max_drawdown_pct": 60
		}],
		"discord": {
			"enabled": false,
			"channels": {},
			"dm_channels": { "hyperliquid-paper-extra": "123456789" }
		}
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for invalid dm_channels key")
	}
	if !strings.Contains(err.Error(), "dm_channels key") {
		t.Errorf("error = %v, want mention of dm_channels key", err)
	}
}

func TestConfigValidationDMChannelsEmptyValue(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "t-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000,
			"max_drawdown_pct": 60
		}],
		"discord": {
			"enabled": false,
			"channels": {},
			"dm_channels": { "hyperliquid-paper": "" }
		}
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for empty dm_channels value")
	}
	if !strings.Contains(err.Error(), "dm_channels[\"hyperliquid-paper\"]") {
		t.Errorf("error = %v", err)
	}
}

func TestConfigValidationDMChannelsValidKeys(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-test",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "BTC", "1h", "--mode=paper"],
			"capital": 1000,
			"max_drawdown_pct": 50
		}],
		"discord": {
			"enabled": false,
			"channels": {},
			"dm_channels": {
				"hyperliquid": "111",
				"hyperliquid-paper": "222",
				"deribit": "333"
			}
		},
		"telegram": {
			"enabled": false,
			"channels": {},
			"dm_channels": { "okx-paper": "444" }
		}
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Discord.DMChannels["hyperliquid"] != "111" || loaded.Discord.DMChannels["hyperliquid-paper"] != "222" {
		t.Errorf("discord dm_channels mismatch: %#v", loaded.Discord.DMChannels)
	}
	if loaded.Telegram.DMChannels["okx-paper"] != "444" {
		t.Errorf("telegram dm_channels mismatch: %#v", loaded.Telegram.DMChannels)
	}
}

func TestConfigValidationDMChannelsOrphanSuffix(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "t-spot",
			"type": "spot",
			"script": "shared_scripts/check_strategy.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000,
			"max_drawdown_pct": 60
		}],
		"discord": {
			"enabled": false,
			"channels": {},
			"dm_channels": { "-paper": "123" }
		}
	}`
	path := writeTestConfig(t, dir, cfgJSON)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected validation error for orphan -paper key")
	}
	if !strings.Contains(err.Error(), "platform prefix is empty") {
		t.Errorf("error = %v, want mention of empty platform prefix", err)
	}
}

func TestConfigValidationLeaderboardSummariesInvalid(t *testing.T) {
	tests := []struct {
		name string
		lc   LeaderboardSummaryConfig
		want string
	}{
		{"missing platform", LeaderboardSummaryConfig{Channel: "c1"}, "platform is required"},
		{"missing channel", LeaderboardSummaryConfig{Platform: "hyperliquid"}, "channel is required"},
		{"negative top_n", LeaderboardSummaryConfig{Platform: "hl", Channel: "c1", TopN: -1}, "top_n must be >= 0"},
		{"invalid freq", LeaderboardSummaryConfig{Platform: "hl", Channel: "c1", Frequency: "abc"}, "frequency invalid"},
		{"freq too short", LeaderboardSummaryConfig{Platform: "hl", Channel: "c1", Frequency: "30s"}, "frequency must be >= 1m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				IntervalSeconds:      60,
				Strategies:           []StrategyConfig{{ID: "s1", Type: "spot", Platform: "binanceus", Capital: 100, MaxDrawdownPct: 10, Script: "x.py"}},
				LeaderboardSummaries: []LeaderboardSummaryConfig{tt.lc},
			}
			err := validateConfig(cfg, false)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("expected error containing %q, got: %v", tt.want, err)
			}
		})
	}
}

func TestConfigValidationLeaderboardSummariesDuplicateKey(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{ID: "s1", Type: "spot", Platform: "binanceus", Capital: 100, MaxDrawdownPct: 10, Script: "x.py"},
		},
		LeaderboardSummaries: []LeaderboardSummaryConfig{
			{Platform: "hyperliquid", Ticker: "ETH", Channel: "chan-1", Frequency: "6h"},
			{Platform: "Hyperliquid", Ticker: "eth", Channel: "chan-1", Frequency: "12h"},
		},
	}
	err := validateConfig(cfg, false)
	if err == nil {
		t.Fatal("expected duplicate-key validation error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate entry") {
		t.Errorf("expected error to mention 'duplicate entry', got: %v", err)
	}
	if !strings.Contains(err.Error(), "leaderboard_summaries[0]") {
		t.Errorf("expected error to reference first-occurrence index [0], got: %v", err)
	}
}

func TestConfigValidationLeaderboardSummariesDistinctTickersSameChannel(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{ID: "s1", Type: "spot", Platform: "binanceus", Capital: 100, MaxDrawdownPct: 10, Script: "x.py"},
		},
		LeaderboardSummaries: []LeaderboardSummaryConfig{
			{Platform: "hyperliquid", Channel: "hl-ch", Frequency: "6h"},
			{Platform: "hyperliquid", Ticker: "ETH", Channel: "hl-ch", Frequency: "12h"},
		},
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Errorf("expected distinct-ticker same-channel config to validate, got: %v", err)
	}
}

func TestConfigValidationLeaderboardSummariesValid(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{ID: "s1", Type: "spot", Platform: "binanceus", Capital: 100, MaxDrawdownPct: 10, Script: "x.py"},
		},
		LeaderboardSummaries: []LeaderboardSummaryConfig{
			{Platform: "hyperliquid", TopN: 10, Channel: "chan-1", Frequency: "6h"},
			{Platform: "hyperliquid", Ticker: "eth", TopN: 5, Channel: "chan-2", Frequency: "12h"},
			{Platform: "binanceus", TopN: 5, Channel: "chan-3"},
		},
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Errorf("expected valid config, got: %v", err)
	}
}

func TestLoadConfigLeaderboardSummaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{
		"interval_seconds": 60,
		"log_dir": "logs",
		"discord": {"enabled": false, "token": "", "channels": {}},
		"strategies": [
			{"id": "hl-sma-btc", "type": "perps", "platform": "hyperliquid", "script": "x.py", "capital": 1000, "max_drawdown_pct": 10}
		],
		"leaderboard_summaries": [
			{"platform": "hyperliquid", "ticker": null, "top_n": 10, "channel": "11111111111111111", "frequency": "6h"},
			{"platform": "hyperliquid", "ticker": "eth", "top_n": 5, "channel": "22222222222222222", "frequency": "12h"}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.LeaderboardSummaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(loaded.LeaderboardSummaries))
	}
	if loaded.LeaderboardSummaries[0].TopN != 10 || loaded.LeaderboardSummaries[0].Frequency != "6h" {
		t.Errorf("first summary wrong: %+v", loaded.LeaderboardSummaries[0])
	}
	if loaded.LeaderboardSummaries[1].Ticker != "eth" {
		t.Errorf("second summary ticker: got %q, want 'eth'", loaded.LeaderboardSummaries[1].Ticker)
	}
}

func TestStrategyIntervalExceedsGlobalWarning(t *testing.T) {
	cases := []struct {
		name           string
		strategyID     string
		strategyInt    int
		globalInt      int
		wantWarn       bool
		wantSubstrings []string
	}{
		{
			name:        "strategy interval matches global",
			strategyID:  "hl-tema-eth",
			strategyInt: 300,
			globalInt:   300,
			wantWarn:    false,
		},
		{
			name:        "strategy interval below global",
			strategyID:  "hl-tema-eth",
			strategyInt: 120,
			globalInt:   300,
			wantWarn:    false,
		},
		{
			name:        "strategy interval zero uses global",
			strategyID:  "hl-tema-eth",
			strategyInt: 0,
			globalInt:   300,
			wantWarn:    false,
		},
		{
			name:           "exact triple — every 3rd cycle",
			strategyID:     "hl-tema-eth-live",
			strategyInt:    900,
			globalInt:      300,
			wantWarn:       true,
			wantSubstrings: []string{`"hl-tema-eth-live"`, "interval_seconds=900", "interval_seconds=300", "every 3rd portfolio cycle"},
		},
		{
			name:           "non-multiple rounds up — every 2nd cycle",
			strategyID:     "s1",
			strategyInt:    400,
			globalInt:      300,
			wantWarn:       true,
			wantSubstrings: []string{"every 2nd portfolio cycle"},
		},
		{
			name:           "11x uses 'th' suffix",
			strategyID:     "s1",
			strategyInt:    3300,
			globalInt:      300,
			wantWarn:       true,
			wantSubstrings: []string{"every 11th portfolio cycle"},
		},
		{
			name:           "21x uses 'st' suffix",
			strategyID:     "s1",
			strategyInt:    6300,
			globalInt:      300,
			wantWarn:       true,
			wantSubstrings: []string{"every 21st portfolio cycle"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{ID: tc.strategyID, IntervalSeconds: tc.strategyInt}
			got := strategyIntervalExceedsGlobalWarning(sc, tc.globalInt)
			if tc.wantWarn {
				if got == "" {
					t.Fatalf("expected warning, got empty string")
				}
				if !strings.HasPrefix(got, "[WARN] strategy ") {
					t.Errorf("warning should start with '[WARN] strategy ', got %q", got)
				}
				for _, sub := range tc.wantSubstrings {
					if !strings.Contains(got, sub) {
						t.Errorf("warning %q missing substring %q", got, sub)
					}
				}
			} else if got != "" {
				t.Errorf("expected no warning, got %q", got)
			}
		})
	}
}

func TestOrdinal(t *testing.T) {
	cases := map[int]string{
		1:   "1st",
		2:   "2nd",
		3:   "3rd",
		4:   "4th",
		11:  "11th",
		12:  "12th",
		13:  "13th",
		21:  "21st",
		22:  "22nd",
		23:  "23rd",
		101: "101st",
		111: "111th",
		112: "112th",
		113: "113th",
	}
	for n, want := range cases {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestConfigValidationManualSymbolSharingAllowed(t *testing.T) {
	cases := []struct {
		name  string
		other StrategyConfig
	}{
		{
			name: "manual shares with manual on same coin",
			other: StrategyConfig{
				ID:             "hl-manual-eth-2",
				Type:           "manual",
				Platform:       "hyperliquid",
				Symbol:         "ETH",
				Timeframe:      "1h",
				Leverage:       10,
				Capital:        1000,
				MaxDrawdownPct: 60,
			},
		},
		{
			name: "manual shares with perps on same coin",
			other: StrategyConfig{
				ID:             "hl-perps-eth-live",
				Type:           "perps",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_hyperliquid.py",
				Args:           []string{"sma_crossover", "ETH", "1h", "--mode=paper"},
				Capital:        1000,
				Leverage:       10,
				MaxDrawdownPct: 60,
			},
		},
		{
			name: "manual on different coin is allowed",
			other: StrategyConfig{
				ID:             "hl-manual-btc",
				Type:           "manual",
				Platform:       "hyperliquid",
				Symbol:         "BTC",
				Timeframe:      "1h",
				Leverage:       5,
				Capital:        1000,
				MaxDrawdownPct: 60,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Strategies: []StrategyConfig{
					{
						ID:             "hl-manual-eth",
						Type:           "manual",
						Platform:       "hyperliquid",
						Symbol:         "ETH",
						Timeframe:      "1h",
						Leverage:       10,
						Capital:        1000,
						MaxDrawdownPct: 60,
					},
					tc.other,
				},
				PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
			}
			err := validateConfig(&cfg, false)
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

func TestConfigValidationManualPerpsPeerLeverageMismatchRejected(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{
			{
				ID:             "hl-manual-eth",
				Type:           "manual",
				Platform:       "hyperliquid",
				Symbol:         "ETH",
				Timeframe:      "1h",
				Leverage:       10,
				Capital:        1000,
				MaxDrawdownPct: 60,
			},
			{
				ID:             "hl-perps-eth-live",
				Type:           "perps",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_hyperliquid.py",
				Args:           []string{"sma_crossover", "ETH", "1h", "--mode=paper"},
				Capital:        1000,
				Leverage:       5,
				MaxDrawdownPct: 60,
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := validateConfig(&cfg, false)
	if err == nil || !strings.Contains(err.Error(), "disagree on leverage") {
		t.Fatalf("expected leverage peer error, got: %v", err)
	}
}

func TestConfigValidationManualPerpsPeerMarginModeMismatchRejected(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{
			{
				ID:             "hl-manual-eth",
				Type:           "manual",
				Platform:       "hyperliquid",
				Symbol:         "ETH",
				Timeframe:      "1h",
				Leverage:       5,
				MarginMode:     "isolated",
				Capital:        1000,
				MaxDrawdownPct: 60,
			},
			{
				ID:             "hl-perps-eth-live",
				Type:           "perps",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_hyperliquid.py",
				Args:           []string{"sma_crossover", "ETH", "1h", "--mode=paper"},
				Capital:        1000,
				Leverage:       5,
				MarginMode:     "cross",
				MaxDrawdownPct: 60,
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := validateConfig(&cfg, false)
	if err == nil || !strings.Contains(err.Error(), "disagree on margin_mode") {
		t.Fatalf("expected margin_mode peer error, got: %v", err)
	}
}

func TestConfigValidationManualPerpsMultipleTrailingStopOwnersAllowed(t *testing.T) {
	manualTrailing := 1.5
	perpsTrailingPct := 0.02
	cfg := Config{
		Strategies: []StrategyConfig{
			{
				ID:                  "hl-manual-eth",
				Type:                "manual",
				Platform:            "hyperliquid",
				Symbol:              "ETH",
				Timeframe:           "1h",
				Leverage:            5,
				Capital:             1000,
				MaxDrawdownPct:      60,
				TrailingStopATRMult: &manualTrailing,
				CloseStrategy:       &StrategyRef{Name: "trailing_tp_ratchet"},
			},
			{
				ID:              "hl-perps-eth-live",
				Type:            "perps",
				Platform:        "hyperliquid",
				Script:          "shared_scripts/check_hyperliquid.py",
				Args:            []string{"sma_crossover", "ETH", "1h", "--mode=paper"},
				Capital:         1000,
				Leverage:        5,
				MaxDrawdownPct:  60,
				TrailingStopPct: &perpsTrailingPct,
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := validateConfig(&cfg, false)
	if err != nil {
		t.Fatalf("expected manual+perps trailing peers to validate, got: %v", err)
	}
}

func TestConfigValidationMultipleTrailingRatchetRegimeOwnersAllowed(t *testing.T) {
	regimeTrail := func() *RegimeATRBlock {
		return &RegimeATRBlock{raw: map[string]interface{}{
			"trend_regime": map[string]interface{}{
				"trending_up":   map[string]interface{}{"atr_multiple": 3.0},
				"trending_down": map[string]interface{}{"atr_multiple": 3.0},
				"ranging":       map[string]interface{}{"atr_multiple": 3.0},
			},
		}}
	}
	cfg := Config{
		Regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20},
		Strategies: []StrategyConfig{
			{
				ID:                 "hl-ratchet-a",
				Type:               "perps",
				Platform:           "hyperliquid",
				Script:             "shared_scripts/check_hyperliquid.py",
				Args:               []string{"tema", "ETH", "1h", "--mode=paper"},
				Capital:            1000,
				Leverage:           5,
				MaxDrawdownPct:     60,
				TrailingStopATRMultRegime: regimeTrail(),
				CloseStrategy:      &StrategyRef{Name: "trailing_tp_ratchet_regime", Params: map[string]interface{}{"use_defaults": true}},
			},
			{
				ID:                 "hl-ratchet-b",
				Type:               "perps",
				Platform:           "hyperliquid",
				Script:             "shared_scripts/check_hyperliquid.py",
				Args:               []string{"rmc", "ETH", "1h", "--mode=paper"},
				Capital:            1000,
				Leverage:           5,
				MaxDrawdownPct:     60,
				TrailingStopATRMultRegime: regimeTrail(),
				CloseStrategy:      &StrategyRef{Name: "trailing_tp_ratchet_regime", Params: map[string]interface{}{"use_defaults": true}},
			},
		},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := validateConfig(&cfg, false); err != nil {
		t.Fatalf("expected multiple trailing_tp_ratchet_regime owners to validate, got: %v", err)
	}
}

func TestConfigValidationInvertSignal(t *testing.T) {
	hlPerps := func(direction string, invert bool) StrategyConfig {
		return StrategyConfig{
			ID:             "hl-test-eth",
			Type:           "perps",
			Platform:       "hyperliquid",
			Script:         "shared_scripts/check_hyperliquid.py",
			Args:           []string{"sma_crossover", "ETH", "1h", "--mode=paper"},
			Capital:        1000,
			Leverage:       5,
			MaxDrawdownPct: 60,
			Direction:      direction,
			InvertSignal:   invert,
		}
	}
	hlManual := func(direction string, invert bool) StrategyConfig {
		return StrategyConfig{
			ID:             "hl-manual-eth",
			Type:           "manual",
			Platform:       "hyperliquid",
			Symbol:         "ETH",
			Timeframe:      "1h",
			Leverage:       5,
			Capital:        1000,
			MaxDrawdownPct: 60,
			Direction:      direction,
			InvertSignal:   invert,
		}
	}

	t.Run("accepts HL perps + invert + direction=short", func(t *testing.T) {
		cfg := Config{
			Strategies:    []StrategyConfig{hlPerps(DirectionShort, true)},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		if err := validateConfig(&cfg, false); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("accepts HL perps + invert + direction=long", func(t *testing.T) {
		cfg := Config{
			Strategies:    []StrategyConfig{hlPerps(DirectionLong, true)},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		if err := validateConfig(&cfg, false); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("accepts HL perps + invert + direction=both", func(t *testing.T) {
		cfg := Config{
			Strategies:    []StrategyConfig{hlPerps(DirectionBoth, true)},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		if err := validateConfig(&cfg, false); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("accepts HL manual + invert + direction=short", func(t *testing.T) {
		cfg := Config{
			Strategies:    []StrategyConfig{hlManual(DirectionShort, true)},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		if err := validateConfig(&cfg, false); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("rejects invert on HL spot", func(t *testing.T) {
		cfg := Config{
			Strategies: []StrategyConfig{{
				ID:             "hl-spot-btc",
				Type:           "spot",
				Platform:       "hyperliquid",
				Script:         "shared_scripts/check_strategy.py",
				Capital:        1000,
				MaxDrawdownPct: 60,
				InvertSignal:   true,
			}},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		err := validateConfig(&cfg, false)
		if err == nil || !strings.Contains(err.Error(), "invert_signal is only supported for HL perps/manual") {
			t.Fatalf("expected HL-perps/manual-only error, got: %v", err)
		}
	})

	t.Run("rejects invert on non-HL perps", func(t *testing.T) {
		cfg := Config{
			Strategies: []StrategyConfig{{
				ID:             "okx-perps-btc",
				Type:           "perps",
				Platform:       "okx",
				Script:         "shared_scripts/check_okx.py",
				Args:           []string{"sma_crossover", "BTC-USDT-SWAP", "1h"},
				Capital:        1000,
				Leverage:       5,
				MaxDrawdownPct: 60,
				InvertSignal:   true,
			}},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
		err := validateConfig(&cfg, false)
		if err == nil || !strings.Contains(err.Error(), "invert_signal is only supported for HL perps/manual") {
			t.Fatalf("expected HL-perps/manual-only error, got: %v", err)
		}
	})
}

func TestConfigValidationHLLiveRequiresSecretKey(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "")
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:             "hl-tema-eth-live",
			Type:           "perps",
			Platform:       "hyperliquid",
			Script:         "shared_scripts/check_hyperliquid.py",
			Args:           []string{"triple_ema", "ETH", "1h", "--mode=live"},
			Capital:        1000,
			MaxDrawdownPct: 60,
		}},
	}
	err := validateConfig(&cfg, false)
	if err == nil || !strings.Contains(err.Error(), "HYPERLIQUID_SECRET_KEY") {
		t.Fatalf("expected live secret error, got: %v", err)
	}
}

func TestLoadConfigForProbeSkipsLiveCredentialChecks(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "")
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")
	body, err := json.Marshal(map[string]any{
		"interval_seconds": 600,
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
			map[string]any{
				"id":               "hl-cap-pct",
				"type":             "perps",
				"platform":         "hyperliquid",
				"script":           "shared_scripts/check_hyperliquid.py",
				"args":             []string{"triple_ema", "BTC", "1h", "--mode=live"},
				"interval_seconds": 60,
				"capital_pct":      0.25,
				"max_drawdown_pct": 60.0,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("LoadConfig should still require live credentials")
	}
	if _, err := LoadConfigForProbe(path); err != nil {
		t.Fatalf("LoadConfigForProbe should skip live credential checks: %v", err)
	}
}

func TestCircuitBreakerEnabled_DefaultsToTrue(t *testing.T) {
	var nilSC *StrategyConfig
	if !nilSC.CircuitBreakerEnabled() {
		t.Fatal("nil receiver should report enabled")
	}
	sc := &StrategyConfig{}
	if !sc.CircuitBreakerEnabled() {
		t.Fatal("nil CircuitBreaker field should report enabled")
	}
	f := false
	sc.CircuitBreaker = &f
	if sc.CircuitBreakerEnabled() {
		t.Fatal("explicit false should report disabled")
	}
	tr := true
	sc.CircuitBreaker = &tr
	if !sc.CircuitBreakerEnabled() {
		t.Fatal("explicit true should report enabled")
	}
}

func TestCircuitBreakerOverrideAccessors(t *testing.T) {
	for name, sc := range map[string]*StrategyConfig{"nil receiver": nil, "nil fields": {}} {
		if got := sc.CircuitBreakerDrawdownCooldown(); got != 24*time.Hour {
			t.Errorf("%s: drawdown cooldown = %v, want 24h", name, got)
		}
		if got := sc.CircuitBreakerLossStreakThreshold(); got != 5 {
			t.Errorf("%s: loss-streak threshold = %d, want 5", name, got)
		}
		if got := sc.CircuitBreakerLossStreakCooldown(); got != time.Hour {
			t.Errorf("%s: loss-streak cooldown = %v, want 1h", name, got)
		}
	}
	dd, th, lc := 720, 3, 30
	sc := &StrategyConfig{CBDrawdownCooldownMinutes: &dd, CBLossStreakThreshold: &th, CBLossStreakCooldownMinutes: &lc}
	if got := sc.CircuitBreakerDrawdownCooldown(); got != 12*time.Hour {
		t.Errorf("override drawdown cooldown = %v, want 12h", got)
	}
	if got := sc.CircuitBreakerLossStreakThreshold(); got != 3 {
		t.Errorf("override loss-streak threshold = %d, want 3", got)
	}
	if got := sc.CircuitBreakerLossStreakCooldown(); got != 30*time.Minute {
		t.Errorf("override loss-streak cooldown = %v, want 30m", got)
	}
}

func TestConfigValidationCBOverrides(t *testing.T) {
	intp := func(v int) *int { return &v }
	mk := func(mut func(*StrategyConfig)) Config {
		sc := StrategyConfig{
			ID: "test-spot", Type: "spot", Platform: "binanceus",
			Script:  "shared_scripts/check_strategy.py",
			Args:    []string{"sma_crossover", "BTC/USDT", "1h"},
			Capital: 1000, MaxDrawdownPct: 60,
		}
		mut(&sc)
		return Config{
			Strategies:    []StrategyConfig{sc},
			PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
		}
	}

	valid := mk(func(sc *StrategyConfig) {
		sc.CBDrawdownCooldownMinutes = intp(720)
		sc.CBLossStreakThreshold = intp(3)
		sc.CBLossStreakCooldownMinutes = intp(30)
	})
	if err := validateConfig(&valid, false); err != nil {
		t.Fatalf("in-bounds cb_* overrides should validate: %v", err)
	}
	boundary := mk(func(sc *StrategyConfig) {
		sc.CBDrawdownCooldownMinutes = intp(30 * 24 * 60)
		sc.CBLossStreakThreshold = intp(100)
		sc.CBLossStreakCooldownMinutes = intp(1)
	})
	if err := validateConfig(&boundary, false); err != nil {
		t.Fatalf("boundary cb_* overrides should validate: %v", err)
	}

	cases := []struct {
		name    string
		mut     func(*StrategyConfig)
		wantErr string
	}{
		{"zero drawdown cooldown", func(sc *StrategyConfig) { sc.CBDrawdownCooldownMinutes = intp(0) }, "cb_drawdown_cooldown_minutes must be positive"},
		{"negative loss threshold", func(sc *StrategyConfig) { sc.CBLossStreakThreshold = intp(-1) }, "cb_loss_streak_threshold must be positive"},
		{"zero loss cooldown", func(sc *StrategyConfig) { sc.CBLossStreakCooldownMinutes = intp(0) }, "cb_loss_streak_cooldown_minutes must be positive"},
		{"loss threshold above cap", func(sc *StrategyConfig) { sc.CBLossStreakThreshold = intp(101) }, "cb_loss_streak_threshold must be <= 100"},
		{"drawdown cooldown above 30 days", func(sc *StrategyConfig) { sc.CBDrawdownCooldownMinutes = intp(30*24*60 + 1) }, "cb_drawdown_cooldown_minutes must be <= 43200"},
		{"loss cooldown above 30 days", func(sc *StrategyConfig) { sc.CBLossStreakCooldownMinutes = intp(30*24*60 + 1) }, "cb_loss_streak_cooldown_minutes must be <= 43200"},
		{"manual rejects drawdown cooldown", func(sc *StrategyConfig) {
			sc.Type = "manual"
			sc.Platform = "hyperliquid"
			sc.Symbol = "ETH"
			sc.Timeframe = "1h"
			sc.Leverage = 2
			sc.CBDrawdownCooldownMinutes = intp(720)
		}, "cb_drawdown_cooldown_minutes is not supported for manual strategies"},
		{"manual rejects loss threshold", func(sc *StrategyConfig) {
			sc.Type = "manual"
			sc.Platform = "hyperliquid"
			sc.Symbol = "ETH"
			sc.Timeframe = "1h"
			sc.Leverage = 2
			sc.CBLossStreakThreshold = intp(3)
		}, "cb_loss_streak_threshold is not supported for manual strategies"},
		{"manual rejects loss cooldown", func(sc *StrategyConfig) {
			sc.Type = "manual"
			sc.Platform = "hyperliquid"
			sc.Symbol = "ETH"
			sc.Timeframe = "1h"
			sc.Leverage = 2
			sc.CBLossStreakCooldownMinutes = intp(30)
		}, "cb_loss_streak_cooldown_minutes is not supported for manual strategies"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mk(tc.mut)
			err := validateConfig(&cfg, false)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestStrategyNotifyRatchetTriggersEnabled_TwoLayerResolve(t *testing.T) {
	tr := true
	f := false
	globalFalse := &Config{NotifyRatchetTriggers: &f}
	globalTrue := &Config{NotifyRatchetTriggers: &tr}
	globalDefault := &Config{}

	cases := []struct {
		name     string
		sc       *StrategyConfig
		cfg      *Config
		expected bool
	}{
		{"nil strategy field inherits global default (true)", &StrategyConfig{}, globalDefault, true},
		{"nil strategy field inherits global false", &StrategyConfig{}, globalFalse, false},
		{"nil strategy field inherits global true", &StrategyConfig{}, globalTrue, true},
		{"strategy false overrides global true", &StrategyConfig{NotifyRatchetTriggers: &f}, globalTrue, false},
		{"strategy true overrides global false", &StrategyConfig{NotifyRatchetTriggers: &tr}, globalFalse, true},
		{"nil receiver inherits global false", nil, globalFalse, false},
		{"nil receiver inherits global default (true)", nil, globalDefault, true},
	}
	for _, c := range cases {
		if got := c.sc.NotifyRatchetTriggersEnabled(c.cfg); got != c.expected {
			t.Errorf("%s: got %v, want %v", c.name, got, c.expected)
		}
	}
}
