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
	if loaded.DBFile != "scheduler/state.db" {
		t.Errorf("DBFile = %q, want %q", loaded.DBFile, "scheduler/state.db")
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

func TestValidateConfigOpenCloseFields(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:              "test-spot",
			Type:            "spot",
			Platform:        "binanceus",
			Script:          "shared_scripts/check_strategy.py",
			Args:            []string{"sma_crossover", "BTC/USDT", "1h"},
			OpenStrategy:    "momentum",
			CloseStrategies: []string{"rsi", "macd"},
			Capital:         1000,
			MaxDrawdownPct:  60,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("expected valid open/close config, got: %v", err)
	}
}

func TestValidateConfigOpenCloseRejectsOptions(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:              "test-options",
			Type:            "options",
			Platform:        "deribit",
			Script:          "shared_scripts/check_options.py",
			Args:            []string{"vol_mean_reversion", "BTC", "1h"},
			CloseStrategies: []string{"rsi"},
			Capital:         1000,
			MaxDrawdownPct:  40,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := ValidateConfig(&cfg)
	if err == nil {
		t.Fatal("expected options open/close validation error")
	}
	if !strings.Contains(err.Error(), "open_strategy/close_strategies") {
		t.Fatalf("error %q should mention open/close fields", err.Error())
	}
}

func TestValidateConfigCloseStrategyName(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:              "test-spot",
			Type:            "spot",
			Platform:        "binanceus",
			Script:          "shared_scripts/check_strategy.py",
			Args:            []string{"sma_crossover", "BTC/USDT", "1h"},
			CloseStrategies: []string{"bad name"},
			Capital:         1000,
			MaxDrawdownPct:  60,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := ValidateConfig(&cfg)
	if err == nil {
		t.Fatal("expected close strategy name validation error")
	}
	if !strings.Contains(err.Error(), "close_strategies[0]") {
		t.Fatalf("error %q should mention close_strategies[0]", err.Error())
	}
}

func TestValidateConfigOpenCloseDefersRegistryLookupToCheckScript(t *testing.T) {
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:              "test-spot",
			Type:            "spot",
			Platform:        "binanceus",
			Script:          "shared_scripts/check_strategy.py",
			Args:            []string{"sma_crossover", "BTC/USDT", "1h"},
			OpenStrategy:    "not_a_strategy",
			CloseStrategies: []string{"rsi"},
			Capital:         1000,
			MaxDrawdownPct:  60,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("syntactically valid strategy names should be accepted by config validation: %v", err)
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

func TestValidateConfigLeaderboardPostTime(t *testing.T) {
	base := Config{
		Strategies: []StrategyConfig{{
			ID: "test", Type: "spot", Script: "shared_scripts/check_strategy.py",
			Args: []string{"sma_crossover", "BTC/USDT", "1h"}, Capital: 1000, MaxDrawdownPct: 60,
			Platform: "binanceus",
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}

	// Valid time should pass.
	cfg := base
	cfg.LeaderboardPostTime = "11:00"
	if err := ValidateConfig(&cfg); err != nil {
		t.Errorf("expected valid config with leaderboard_post_time=11:00, got: %v", err)
	}

	// Empty (disabled) should pass.
	cfg2 := base
	cfg2.LeaderboardPostTime = ""
	if err := ValidateConfig(&cfg2); err != nil {
		t.Errorf("expected valid config with empty leaderboard_post_time, got: %v", err)
	}

	// Invalid format should fail.
	cfg3 := base
	cfg3.LeaderboardPostTime = "noon"
	err := ValidateConfig(&cfg3)
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

func TestValidateConfigInitialCapitalNegative(t *testing.T) {
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
	err := ValidateConfig(cfg)
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

// #254: perps strategies get default Leverage=1 when unset.
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

// #254: explicit perps Leverage is preserved.
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

// #497: sizing_leverage can differ from exchange leverage.
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

// #254: Leverage must be rejected on non-perps types.
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

// #254: Leverage must be in [1, 100].
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

// #497: fractional sizing_leverage is valid — high exchange leverage with
// conservative position size (e.g. leverage=20, sizing_leverage=0.5) is the
// motivating use case for decoupling.
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

// #486: HL perps strategies default to isolated margin mode.
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

// #494: a single HL perps strategy on a coin still auto-derives its
// exchange-side stop-loss from max_drawdown_pct when stop_loss_* is omitted.
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
	if got := EffectiveStopLossPct(cfg.Strategies[0]); got != 10 {
		t.Errorf("EffectiveStopLossPct = %g, want 10", got)
	}
}

// #486: explicit cross margin mode is preserved.
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

// #486: invalid margin_mode rejected.
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

// #486: margin_mode is HL-perps-only (rejected on spot).
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

// #491: two HL perps strategies on the same coin must agree on margin_mode
// and leverage — HL aggregates positions per coin per account, so peers
// share a single on-chain position. Matching peers load successfully.
func TestLoadConfigHLPerpsPeersOnSameCoinMatching(t *testing.T) {
	dir := t.TempDir()
	// #494: omitted stop_loss_* on same-coin peers is normalized to opt-out so
	// existing multi-strategy configs don't all become stop-loss owners.
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

// #491: peers on the same coin with mismatched margin_mode are rejected.
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

// #491: peers on the same coin with mismatched leverage are rejected.
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

// #491: only one peer may carry stop_loss_pct — reduce-only triggers from
// both peers would race on the shared on-chain position.
func TestLoadConfigHLPerpsPeersConflictingStopLoss(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected validation error for conflicting stop_loss_pct on peers")
	}
	if !strings.Contains(err.Error(), "conflicting stop_loss_pct") {
		t.Errorf("error = %v, want 'conflicting stop_loss_pct'", err)
	}
}

// #491: a single peer with stop_loss_pct is fine; the guard only fires when
// two or more peers configure SLs that would race on the shared position.
func TestLoadConfigHLPerpsPeersSingleStopLossAllowed(t *testing.T) {
	dir := t.TempDir()
	// #494: an omitted same-coin peer is normalized to opt-out, while the
	// explicit positive stop_loss_pct remains the sole trigger owner.
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

// #491: peer-validation only applies within a single coin — strategies on
// different coins don't constrain each other.
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

// #491/#494: peers that disable SL via explicit stop_loss_pct:0 must not trip
// the conflict guard. Explicit zero remains an opt-out even though omitted
// same-coin peers are also normalized to opt-out.
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

// #491: margin_mode defaulting (empty -> "isolated") happens at LoadConfig
// time, so peer comparison must see normalized values. A peer with
// margin_mode:"" should match a peer with margin_mode:"isolated".
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

// #494: two peers that both omit stop_loss_* on the same coin are normalized
// to explicit opt-out so old multi-strategy configs keep loading after v9.
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

func TestValidateConfigDMChannelsInvalidKey(t *testing.T) {
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

func TestValidateConfigDMChannelsEmptyValue(t *testing.T) {
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

func TestValidateConfigDMChannelsValidKeys(t *testing.T) {
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

func TestValidateConfigDMChannelsOrphanSuffix(t *testing.T) {
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

func TestValidateConfigLeaderboardSummariesInvalid(t *testing.T) {
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
			err := ValidateConfig(cfg)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("expected error containing %q, got: %v", tt.want, err)
			}
		})
	}
}

// TestValidateConfigLeaderboardSummariesDuplicateKey covers review item 4 on
// #309: two entries with identical platform/ticker/channel share a single
// LastLeaderboardSummaries timestamp, so whichever posts first silently blocks
// the other. Detect the collision at config load instead.
func TestValidateConfigLeaderboardSummariesDuplicateKey(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{ID: "s1", Type: "spot", Platform: "binanceus", Capital: 100, MaxDrawdownPct: 10, Script: "x.py"},
		},
		LeaderboardSummaries: []LeaderboardSummaryConfig{
			{Platform: "hyperliquid", Ticker: "ETH", Channel: "chan-1", Frequency: "6h"},
			// Case-insensitive collision — Key() normalizes to lowercase.
			{Platform: "Hyperliquid", Ticker: "eth", Channel: "chan-1", Frequency: "12h"},
		},
	}
	err := ValidateConfig(cfg)
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

// TestValidateConfigLeaderboardSummariesDistinctTickersSameChannel confirms we
// don't flag legitimate configurations where the same channel hosts multiple
// leaderboards scoped by distinct tickers.
func TestValidateConfigLeaderboardSummariesDistinctTickersSameChannel(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{ID: "s1", Type: "spot", Platform: "binanceus", Capital: 100, MaxDrawdownPct: 10, Script: "x.py"},
		},
		LeaderboardSummaries: []LeaderboardSummaryConfig{
			{Platform: "hyperliquid", Channel: "hl-ch", Frequency: "6h"},                 // unfiltered
			{Platform: "hyperliquid", Ticker: "ETH", Channel: "hl-ch", Frequency: "12h"}, // ticker-scoped
		},
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Errorf("expected distinct-ticker same-channel config to validate, got: %v", err)
	}
}

func TestValidateConfigLeaderboardSummariesValid(t *testing.T) {
	cfg := &Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{ID: "s1", Type: "spot", Platform: "binanceus", Capital: 100, MaxDrawdownPct: 10, Script: "x.py"},
		},
		LeaderboardSummaries: []LeaderboardSummaryConfig{
			{Platform: "hyperliquid", TopN: 10, Channel: "chan-1", Frequency: "6h"},
			{Platform: "hyperliquid", Ticker: "eth", TopN: 5, Channel: "chan-2", Frequency: "12h"},
			{Platform: "binanceus", TopN: 5, Channel: "chan-3"}, // no freq = on-demand only
		},
	}
	if err := ValidateConfig(cfg); err != nil {
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

// TestStrategyIntervalExceedsGlobalWarning covers #409: per-strategy
// interval_seconds greater than the top-level interval should emit a warning
// describing the "every Nth portfolio cycle" cadence.
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

// TestOrdinal spot-checks the ordinal suffix helper used by the #409 warning.
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
