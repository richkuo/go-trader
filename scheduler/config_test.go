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

func TestLoadConfigHyperliquidTop10Freq(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"hyperliquid_top10_freq": "6h",
		"strategies": [{
			"id": "hl-sma-btc",
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["sma_crossover", "BTC/USDT", "1h"],
			"capital": 1000
		}]
	}`
	path := writeTestConfig(t, dir, cfg)

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if loaded.HyperliquidTop10Freq != "6h" {
		t.Errorf("HyperliquidTop10Freq = %q, want %q", loaded.HyperliquidTop10Freq, "6h")
	}
}

func TestValidateConfigHyperliquidTop10FreqInvalid(t *testing.T) {
	cfg := Config{
		HyperliquidTop10Freq: "not-a-duration",
		Strategies: []StrategyConfig{{
			ID: "test", Type: "spot", Script: "check.py", Capital: 100, MaxDrawdownPct: 10,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := ValidateConfig(&cfg)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
	if !strings.Contains(err.Error(), "hyperliquid_top10_freq") {
		t.Errorf("error should mention hyperliquid_top10_freq: %v", err)
	}
}

func TestValidateConfigHyperliquidTop10FreqTooShort(t *testing.T) {
	cfg := Config{
		HyperliquidTop10Freq: "30s",
		Strategies: []StrategyConfig{{
			ID: "test", Type: "spot", Script: "check.py", Capital: 100, MaxDrawdownPct: 10,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := ValidateConfig(&cfg)
	if err == nil {
		t.Fatal("expected error for too-short duration")
	}
	if !strings.Contains(err.Error(), ">= 1m") {
		t.Errorf("error should mention >= 1m: %v", err)
	}
}

func TestValidateConfigHyperliquidTop10FreqValid(t *testing.T) {
	cfg := Config{
		HyperliquidTop10Freq: "6h",
		Strategies: []StrategyConfig{{
			ID: "test", Type: "spot", Script: "shared_scripts/check_strategy.py",
			Args: []string{"sma_crossover", "BTC/USDT", "1h"}, Capital: 100, MaxDrawdownPct: 10,
			Platform: "binanceus",
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Errorf("expected valid config with HyperliquidTop10Freq=6h, got: %v", err)
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
