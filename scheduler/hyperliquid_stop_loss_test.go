package main

import (
	"testing"
)

func TestEffectiveStopLossPct(t *testing.T) {
	hlPerps := func(sc StrategyConfig) StrategyConfig {
		sc.Platform = "hyperliquid"
		sc.Type = "perps"
		return sc
	}
	pf := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		sc   StrategyConfig
		want float64
	}{
		{"non-HL returns 0", StrategyConfig{Platform: "okx", Type: "perps", StopLossPct: pf(1.5)}, 0},
		{"non-perps returns 0", StrategyConfig{Platform: "hyperliquid", Type: "spot", StopLossPct: pf(1.5)}, 0},
		{"unset and no drawdown", hlPerps(StrategyConfig{Leverage: 5}), 0},
		{"explicit pct", hlPerps(StrategyConfig{StopLossPct: pf(1.5), Leverage: 5}), 1.5},
		{"trailing pct wins", hlPerps(StrategyConfig{TrailingStopPct: pf(2.5), Leverage: 5}), 2.5},
		{"trailing zero is disabled (no fallback)", hlPerps(StrategyConfig{TrailingStopPct: pf(0), MaxDrawdownPct: 5, Leverage: 5}), 0},
		{"explicit zero is disabled (no fallback)", hlPerps(StrategyConfig{StopLossPct: pf(0), MaxDrawdownPct: 5, Leverage: 5}), 0},
		{"margin pct at 20x", hlPerps(StrategyConfig{StopLossMarginPct: pf(20), Leverage: 20}), 1.0},
		{"margin pct at 10x rescales", hlPerps(StrategyConfig{StopLossMarginPct: pf(20), Leverage: 10}), 2.0},
		{"margin pct without leverage fails safe", hlPerps(StrategyConfig{StopLossMarginPct: pf(20)}), 0},
		{"explicit-zero margin disables (no fallback)", hlPerps(StrategyConfig{StopLossMarginPct: pf(0), MaxDrawdownPct: 7, Leverage: 5}), 0},
		{"explicit wins over margin", hlPerps(StrategyConfig{StopLossPct: pf(3), StopLossMarginPct: pf(20), Leverage: 10}), 3},
		{"trailing wins over explicit before validation", hlPerps(StrategyConfig{TrailingStopPct: pf(4), StopLossPct: pf(3), Leverage: 10}), 4},
		{"drawdown fallback when both nil", hlPerps(StrategyConfig{MaxDrawdownPct: 5, Leverage: 5}), 5},
		{"drawdown fallback capped at 50", hlPerps(StrategyConfig{MaxDrawdownPct: 60, Leverage: 5}), 50},
		{"drawdown fallback at cap boundary", hlPerps(StrategyConfig{MaxDrawdownPct: 50, Leverage: 5}), 50},
		{"drawdown fallback ignored when explicit set", hlPerps(StrategyConfig{StopLossPct: pf(2), MaxDrawdownPct: 10}), 2},
		{"margin fallthrough beats drawdown", hlPerps(StrategyConfig{StopLossMarginPct: pf(20), MaxDrawdownPct: 5, Leverage: 20}), 1.0},
		{"stop_loss_atr_mult_regime defers, no drawdown fallback",
			hlPerps(StrategyConfig{StopLossATRMultRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending": {ATR: 2}}}, MaxDrawdownPct: 5, Leverage: 5}), 0},
		{"trailing_stop_atr_mult_regime defers, no drawdown fallback",
			hlPerps(StrategyConfig{TrailingStopATRMultRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending": {ATR: 2}}}, MaxDrawdownPct: 5, Leverage: 5}), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EffectiveStopLossPct(c.sc)
			if got != c.want {
				t.Errorf("EffectiveStopLossPct(%+v) = %g, want %g", c.sc, got, c.want)
			}
		})
	}
}
