package main

import (
	"strings"
	"testing"
)

// #1450 — the bankruptcy-bound check must actually reach validateConfig, not
// only the pure helper. A pure-helper-only test would pass while the wiring was
// missing, which is exactly the class of gap this issue is about.
func liqValidationConfig(stopPct, leverage float64, mode, marginMode string) Config {
	sc := StrategyConfig{
		ID:             "hl-eth",
		Type:           "perps",
		Platform:       "hyperliquid",
		Script:         "shared_scripts/check_hyperliquid.py",
		Args:           []string{"sma_crossover", "ETH", "1h", "--mode=" + mode},
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
	// 10% stop at 20x: the position is bankrupt at 5%, so the stop can never fill.
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
		// The acceptance criterion: a valid low-leverage configuration passes.
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
