package main

import (
	"testing"
)

func TestConfigValidation_AllowedRegimes(t *testing.T) {
	cases := []struct {
		name    string
		cfg     func() Config
		allowed []string
		regime  *RegimeConfig
		wantErr bool
	}{
		{name: "empty accepted", cfg: minimalSpotConfig, allowed: []string{}},
		{name: "nil accepted", cfg: minimalSpotConfig, allowed: nil},
		{name: "valid labels accepted", cfg: minimalSpotConfig, allowed: []string{"trending_up", "trending_down"}},
		{name: "all three labels accepted", cfg: minimalSpotConfig, allowed: []string{"trending_up", "trending_down", "ranging"}},
		{name: "unknown label rejected", cfg: minimalSpotConfig, allowed: []string{"trending_up", "bullish_breakout"}, wantErr: true},
		{
			name: "spot with regime enabled accepted", cfg: minimalSpotConfig,
			allowed: []string{"trending_up"},
			regime:  &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0},
		},
		{name: "options rejected", cfg: minimalOptionsConfig, allowed: []string{"trending_up"}, wantErr: true},
		{name: "options nil accepted", cfg: minimalOptionsConfig, allowed: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg()
			cfg.Strategies[0].AllowedRegimes = tc.allowed
			if tc.regime != nil {
				cfg.Regime = tc.regime
			}
			err := validateConfig(&cfg, false)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation failure")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected validation to pass, got: %v", err)
			}
		})
	}
}

func TestConfigValidation_RegimeConfig(t *testing.T) {
	cases := []struct {
		name    string
		regime  *RegimeConfig
		wantErr bool
	}{
		{name: "nil is valid", regime: nil},
		{name: "valid enabled", regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0}},
		{name: "zero period invalid", regime: &RegimeConfig{Enabled: true, Period: 0, ADXThreshold: 20.0}, wantErr: true},
		{name: "negative period invalid", regime: &RegimeConfig{Enabled: true, Period: -1, ADXThreshold: 20.0}, wantErr: true},
		{name: "zero threshold invalid", regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 0}, wantErr: true},
		{name: "threshold over 100 invalid", regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 101}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalSpotConfig()
			cfg.Regime = tc.regime
			err := validateConfig(&cfg, false)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation failure")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected validation to pass, got: %v", err)
			}
		})
	}
}

func TestRegimeAllowsEntry(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		current string
		want    bool
	}{
		{name: "nil allowed always true", allowed: nil, current: "ranging", want: true},
		{name: "empty allowed always true", allowed: []string{}, current: "trending_up", want: true},
		{name: "matching first label", allowed: []string{"trending_up", "trending_down"}, current: "trending_up", want: true},
		{name: "matching second label", allowed: []string{"trending_up", "trending_down"}, current: "trending_down", want: true},
		{name: "non-matching label", allowed: []string{"trending_up", "trending_down"}, current: "ranging", want: false},
		{name: "bare directional covers _up", allowed: []string{"ranging_directional"}, current: "ranging_directional_up", want: true},
		{name: "bare directional covers _down", allowed: []string{"ranging_directional"}, current: "ranging_directional_down", want: true},
		{name: "bare directional covers itself", allowed: []string{"ranging_directional"}, current: "ranging_directional", want: true},
		{name: "explicit _up does not cover bare", allowed: []string{"ranging_directional_up"}, current: "ranging_directional", want: false},
		{name: "explicit _up does not cover sibling", allowed: []string{"ranging_directional_up"}, current: "ranging_directional_down", want: false},
		{name: "explicit _up matches itself", allowed: []string{"ranging_directional_up"}, current: "ranging_directional_up", want: true},
		{name: "empty current allows when list non-empty", allowed: []string{"trending_up"}, current: "", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := regimeAllowsEntry(tc.allowed, tc.current); got != tc.want {
				t.Errorf("regimeAllowsEntry(%v, %q) = %v, want %v", tc.allowed, tc.current, got, tc.want)
			}
		})
	}
}

func TestRegimeBlocksOpen(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		current string
		posQty  float64
		want    bool
	}{
		{name: "mismatch with no position blocks", allowed: []string{"trending_up"}, current: "ranging", want: true},
		{name: "matching regime does not block", allowed: []string{"trending_up"}, current: "trending_up", want: false},
		{name: "close leg never blocked", allowed: []string{"trending_up"}, current: "ranging", posQty: 1.0, want: false},
		{name: "close leg never blocked on opposite regime", allowed: []string{"trending_up"}, current: "trending_down", posQty: 0.5, want: false},
		{name: "close leg never blocked on empty regime", allowed: []string{"trending_up"}, current: "", posQty: 1.0, want: false},
		{name: "nil allowed never blocks", allowed: nil, current: "ranging", want: false},
		{name: "empty allowed never blocks", allowed: []string{}, current: "ranging", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := regimeBlocksOpen(tc.allowed, tc.current, tc.posQty, false); got != tc.want {
				t.Errorf("regimeBlocksOpen(%v, %q, %g) = %v, want %v", tc.allowed, tc.current, tc.posQty, got, tc.want)
			}
		})
	}
}

func TestHotReload_AllowedRegimesChangeIsAccepted(t *testing.T) {
	cfg := minimalSpotConfig()
	next := minimalSpotConfig()
	next.Strategies[0].AllowedRegimes = []string{"trending_up"}
	if err := validateHotReloadCompatible(&cfg, &next); err != nil {
		t.Fatalf("AllowedRegimes change should be compatible with hot-reload, got: %v", err)
	}
}

func TestHotReload_RegimeConfigChangeRequiresRestart(t *testing.T) {
	cfg := minimalSpotConfig()
	next := minimalSpotConfig()
	next.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0}
	if err := validateHotReloadCompatible(&cfg, &next); err == nil {
		t.Fatal("Regime config change should require restart")
	}
}

func minimalSpotConfig() Config {
	return Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{
				ID:             "test-spot-1",
				Type:           "spot",
				Platform:       "binanceus",
				Script:         "shared_scripts/check_strategy.py",
				Args:           []string{"sma_crossover", "BTC/USDT", "1h"},
				Capital:        1000,
				MaxDrawdownPct: 10,
			},
		},
	}
}

func minimalOptionsConfig() Config {
	return Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{
				ID:             "test-options-1",
				Type:           "options",
				Platform:       "deribit",
				Script:         "shared_scripts/check_options.py",
				Args:           []string{"sell_covered_call", "BTC", "--platform=deribit"},
				Capital:        1000,
				MaxDrawdownPct: 10,
			},
		},
	}
}
