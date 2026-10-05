package main

import (
	"strings"
	"testing"
)

func liqValidationConfig(stopPct, leverage float64, mode, marginMode string) Config {
	sc := StrategyConfig{
		ID:             "hl-eth",
		Type:           "perps",
		Platform:       "hyperliquid",
		Script:         "shared_scripts/check_hyperliquid.py",
		Args:           []string{"breakout", "ETH", "1h", "--mode=" + mode},
		Capital:        1000,
		MaxDrawdownPct: 40,
		Leverage:       leverage,
		MarginMode:     marginMode,
		StopLossPct:    floatPtr(stopPct),
	}
	return Config{
		Strategies:    []StrategyConfig{sc},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
}

func TestConfigValidationRejectsStopPastBankruptcyDistance(t *testing.T) {
	cfg := liqValidationConfig(10, 20, "live", "isolated")
	err := validateConfig(&cfg, true)
	if err == nil {
		t.Fatal("expected a validation error for a stop past the bankruptcy distance")
	}
	if !strings.Contains(err.Error(), "bankruptcy") {
		t.Fatalf("error %q should explain the bankruptcy distance", err.Error())
	}
}

func TestConfigValidationAcceptsAggressiveButReachableStops(t *testing.T) {
	cases := []struct {
		name                string
		stopPct, leverage   float64
		mode, marginMode    string
		expectValidationErr bool
	}{
		{"aggressive but valid at 20x", 4.9, 20, "live", "isolated", false},
		{"valid low leverage", 45, 2, "live", "isolated", false},
		{"paper skips entirely", 10, 20, "paper", "isolated", false},
		{"cross margin skips entirely", 10, 20, "live", "cross", false},
		{"impossible at 20x", 10, 20, "live", "isolated", true},
	}
	for _, c := range cases {
		cfg := liqValidationConfig(c.stopPct, c.leverage, c.mode, c.marginMode)
		err := validateConfig(&cfg, true)
		gotErr := err != nil && strings.Contains(err.Error(), "bankruptcy")
		if gotErr != c.expectValidationErr {
			t.Errorf("%s: bankruptcy error = %v, want %v (err=%v)", c.name, gotErr, c.expectValidationErr, err)
		}
	}
}

func TestConfigValidationAllowsTrailingPctPastBankruptcyDistance(t *testing.T) {
	cfg := liqValidationConfig(0, 20, "live", "isolated")
	cfg.Strategies[0].StopLossPct = nil
	trailing := 10.0
	cfg.Strategies[0].TrailingStopPct = &trailing
	if err := validateConfig(&cfg, true); err != nil && strings.Contains(err.Error(), "bankruptcy") {
		t.Fatalf("trailing_stop_pct past 100/leverage must not be rejected at boot (anchor ratchets), got %v", err)
	}
}

func TestConfigValidationRejectsMaxDrawdownFallbackPastBankruptcyDistance(t *testing.T) {
	cfg := liqValidationConfig(0, 20, "live", "isolated")
	cfg.Strategies[0].StopLossPct = nil
	cfg.Strategies[0].MaxDrawdownPct = 15
	err := validateConfig(&cfg, true)
	if err == nil || !strings.Contains(err.Error(), "bankruptcy") {
		t.Fatalf("max_drawdown_pct fallback of 15%% at 20x must be rejected like stop_loss_pct: 15%%, got %v", err)
	}
}
