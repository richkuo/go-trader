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

func TestConfigValidationBankruptcyDistanceAcrossStopOwners(t *testing.T) {
	cases := []struct {
		name                string
		stopPct, leverage   float64
		mode, marginMode    string
		mutate              func(*StrategyConfig)
		expectValidationErr bool
	}{
		{"aggressive but valid at 20x", 4.9, 20, "live", "isolated", nil, false},
		{"valid low leverage", 45, 2, "live", "isolated", nil, false},
		{"paper skips entirely", 10, 20, "paper", "isolated", nil, false},
		{"cross margin skips entirely", 10, 20, "live", "cross", nil, false},
		{"impossible at 20x", 10, 20, "live", "isolated", nil, true},
		{"trailing_stop_pct past 100/leverage is not rejected at boot because the anchor ratchets", 0, 20, "live", "isolated", func(sc *StrategyConfig) {
			sc.StopLossPct = nil
			trailing := 10.0
			sc.TrailingStopPct = &trailing
		}, false},
		{"max_drawdown_pct fallback of 15% at 20x is rejected like stop_loss_pct: 15%", 0, 20, "live", "isolated", func(sc *StrategyConfig) {
			sc.StopLossPct = nil
			sc.MaxDrawdownPct = 15
		}, true},
	}
	for _, c := range cases {
		cfg := liqValidationConfig(c.stopPct, c.leverage, c.mode, c.marginMode)
		if c.mutate != nil {
			c.mutate(&cfg.Strategies[0])
		}
		err := validateConfig(&cfg, true)
		gotErr := err != nil && strings.Contains(err.Error(), "bankruptcy")
		if gotErr != c.expectValidationErr {
			t.Errorf("%s: bankruptcy error = %v, want %v (err=%v)", c.name, gotErr, c.expectValidationErr, err)
		}
	}
}
