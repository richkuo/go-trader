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

func TestComputeCorrelation_OptionsGreeks(t *testing.T) {
	strategies := map[string]*StrategyState{
		"deribit-strat": {
			ID:        "deribit-strat",
			Type:      "options",
			Positions: make(map[string]*Position),
			OptionPositions: map[string]*OptionPosition{
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
		},
	}
	cfgStrategies := []StrategyConfig{
		{ID: "deribit-strat", Type: "options", Args: []string{"iron_condor", "BTC"}},
	}
	prices := map[string]float64{"BTC/USDT": 50000}
	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 60, MaxSameDirectionPct: 75}

	snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)

	ae := snap.Assets["BTC"]
	if ae == nil {
		t.Fatal("expected BTC asset exposure")
	}
	expectedNet := -40000.0
	if ae.NetDeltaUSD != expectedNet {
		t.Errorf("expected NetDeltaUSD=%f, got %f", expectedNet, ae.NetDeltaUSD)
	}
}

func TestComputeCorrelation_OptionsCoarseDelta(t *testing.T) {
	strategies := map[string]*StrategyState{
		"opt-strat": {
			ID:        "opt-strat",
			Type:      "options",
			Positions: make(map[string]*Position),
			OptionPositions: map[string]*OptionPosition{
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
		},
	}
	cfgStrategies := []StrategyConfig{
		{ID: "opt-strat", Type: "options", Args: []string{"straddle", "BTC"}},
	}
	prices := map[string]float64{"BTC/USDT": 50000}
	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 60, MaxSameDirectionPct: 75}

	snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)

	ae := snap.Assets["BTC"]
	if ae == nil {
		t.Fatal("expected BTC asset exposure")
	}
	expectedNet := 50000.0
	if ae.NetDeltaUSD != expectedNet {
		t.Errorf("expected NetDeltaUSD=%f, got %f", expectedNet, ae.NetDeltaUSD)
	}
}
