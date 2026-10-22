package main

import (
	"strings"
	"testing"
)

func TestComputeCorrelation_MixedDirections(t *testing.T) {
	strategies := map[string]*StrategyState{
		"long-btc": {
			ID:   "long-btc",
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"short-btc": {
			ID:   "short-btc",
			Type: "perps",
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgStrategies := []StrategyConfig{
		{ID: "long-btc", Type: "spot", Args: []string{"sma", "BTC/USDT"}},
		{ID: "short-btc", Type: "perps", Args: []string{"momentum", "BTC"}},
	}
	prices := map[string]float64{"BTC/USDT": 50000}
	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 60, MaxSameDirectionPct: 75}

	snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)

	ae := snap.Assets["BTC"]
	if ae == nil {
		t.Fatal("expected BTC asset exposure")
	}
	if ae.NetDeltaUSD != 0 {
		t.Errorf("expected NetDeltaUSD=0, got %f", ae.NetDeltaUSD)
	}
	if ae.GrossDeltaUSD != 10000 {
		t.Errorf("expected GrossDeltaUSD=10000, got %f", ae.GrossDeltaUSD)
	}
	if ae.ConcentrationPct != 0 {
		t.Errorf("expected 0%% concentration, got %f", ae.ConcentrationPct)
	}
	hasConcentrationWarning := false
	for _, w := range snap.Warnings {
		if strings.Contains(w, "concentration") {
			hasConcentrationWarning = true
		}
	}
	if hasConcentrationWarning {
		t.Error("did not expect concentration warning with net-zero exposure")
	}
}

func TestComputeCorrelation_OptionsDeltaFromGreeksAndCoarseFallback(t *testing.T) {
	cases := []struct {
		name        string
		id          string
		args        []string
		options     map[string]*OptionPosition
		expectedNet float64
	}{
		{
			name: "greeks",
			id:   "deribit-strat",
			args: []string{"iron_condor", "BTC"},
			options: map[string]*OptionPosition{
				"BTC-CALL-60000": {
					Underlying: "BTC",
					OptionType: "call",
					Action:     "sell",
					Quantity:   1.0,
					Greeks:     OptGreeks{Delta: 0.5},
				},
				"BTC-PUT-40000": {
					Underlying: "BTC",
					OptionType: "put",
					Action:     "buy",
					Quantity:   1.0,
					Greeks:     OptGreeks{Delta: -0.3},
				},
			},
			expectedNet: -40000.0,
		},
		{
			name: "coarse delta",
			id:   "opt-strat",
			args: []string{"straddle", "BTC"},
			options: map[string]*OptionPosition{
				"BTC-CALL-70000": {
					Underlying: "BTC",
					OptionType: "call",
					Action:     "buy",
					Quantity:   2.0,
					Greeks:     OptGreeks{Delta: 0},
				},
				"BTC-PUT-40000": {
					Underlying: "BTC",
					OptionType: "put",
					Action:     "buy",
					Quantity:   1.0,
					Greeks:     OptGreeks{Delta: 0},
				},
			},
			expectedNet: 50000.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			strategies := map[string]*StrategyState{
				tc.id: {
					ID:              tc.id,
					Type:            "options",
					Positions:       make(map[string]*Position),
					OptionPositions: tc.options,
				},
			}
			cfgStrategies := []StrategyConfig{
				{ID: tc.id, Type: "options", Args: tc.args},
			}
			prices := map[string]float64{"BTC/USDT": 50000}
			corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 60, MaxSameDirectionPct: 75}

			snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)

			ae := snap.Assets["BTC"]
			if ae == nil {
				t.Fatalf("%s: expected BTC asset exposure", tc.name)
			}
			if ae.NetDeltaUSD != tc.expectedNet {
				t.Errorf("%s: expected NetDeltaUSD=%f, got %f", tc.name, tc.expectedNet, ae.NetDeltaUSD)
			}
		})
	}
}
