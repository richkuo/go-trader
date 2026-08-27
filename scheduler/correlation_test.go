package main

import (
	"strings"
	"testing"
)

func TestComputeCorrelation_SpotLong(t *testing.T) {
	strategies := map[string]*StrategyState{
		"sma-btc": {
			ID:   "sma-btc",
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"momentum-btc": {
			ID:   "momentum-btc",
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.2, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgStrategies := []StrategyConfig{
		{ID: "sma-btc", Type: "spot", Args: []string{"sma_crossover", "BTC/USDT"}},
		{ID: "momentum-btc", Type: "spot", Args: []string{"momentum", "BTC/USDT"}},
	}
	prices := map[string]float64{"BTC/USDT": 50000}
	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 60, MaxSameDirectionPct: 75}

	snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)

	if snap.Assets["BTC"] == nil {
		t.Fatal("expected BTC asset exposure")
	}
	ae := snap.Assets["BTC"]

	if ae.NetDeltaUSD != 15000 {
		t.Errorf("expected NetDeltaUSD=15000, got %f", ae.NetDeltaUSD)
	}
	if len(ae.Strategies) != 2 {
		t.Errorf("expected 2 strategies, got %d", len(ae.Strategies))
	}

	if ae.ConcentrationPct != 100 {
		t.Errorf("expected 100%% concentration, got %f", ae.ConcentrationPct)
	}

	if len(snap.Warnings) == 0 {
		t.Error("expected concentration warning")
	}
}

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

func TestComputeCorrelation_WarningThresholds(t *testing.T) {
	strategies := map[string]*StrategyState{
		"strat1": {
			ID:   "strat1",
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgStrategies := []StrategyConfig{
		{ID: "strat1", Type: "spot", Args: []string{"sma", "BTC/USDT"}},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 110, MaxSameDirectionPct: 110}
	snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)
	if len(snap.Warnings) != 0 {
		t.Errorf("expected no warnings with high thresholds, got %v", snap.Warnings)
	}

	corrCfg2 := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 50, MaxSameDirectionPct: 110}
	snap2 := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg2)
	if len(snap2.Warnings) == 0 {
		t.Error("expected concentration warning with 50% threshold")
	}
}

func TestComputeCorrelation_NoPositions(t *testing.T) {
	strategies := map[string]*StrategyState{
		"empty": {
			ID:              "empty",
			Type:            "spot",
			Positions:       make(map[string]*Position),
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgStrategies := []StrategyConfig{
		{ID: "empty", Type: "spot", Args: []string{"sma", "BTC/USDT"}},
	}
	prices := map[string]float64{"BTC/USDT": 50000}
	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 60, MaxSameDirectionPct: 75}

	snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)

	if len(snap.Assets) != 0 {
		t.Errorf("expected no assets, got %d", len(snap.Assets))
	}
	if len(snap.Warnings) != 0 {
		t.Errorf("expected no warnings, got %v", snap.Warnings)
	}
	if snap.PortfolioGrossUSD != 0 {
		t.Errorf("expected 0 gross, got %f", snap.PortfolioGrossUSD)
	}
}

func TestComputeCorrelation_MultiAsset(t *testing.T) {
	strategies := map[string]*StrategyState{
		"btc-strat": {
			ID:   "btc-strat",
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"eth-strat": {
			ID:   "eth-strat",
			Type: "spot",
			Positions: map[string]*Position{
				"ETH/USDT": {Symbol: "ETH/USDT", Quantity: 1.0, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgStrategies := []StrategyConfig{
		{ID: "btc-strat", Type: "spot", Args: []string{"sma", "BTC/USDT"}},
		{ID: "eth-strat", Type: "spot", Args: []string{"sma", "ETH/USDT"}},
	}
	prices := map[string]float64{"BTC/USDT": 50000, "ETH/USDT": 3000}
	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 60, MaxSameDirectionPct: 75}

	snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)

	if len(snap.Assets) != 2 {
		t.Fatalf("expected 2 assets, got %d", len(snap.Assets))
	}
	btc := snap.Assets["BTC"]
	eth := snap.Assets["ETH"]
	if btc == nil || eth == nil {
		t.Fatal("expected both BTC and ETH assets")
	}
	if btc.NetDeltaUSD != 5000 {
		t.Errorf("BTC net expected 5000, got %f", btc.NetDeltaUSD)
	}
	if eth.NetDeltaUSD != 3000 {
		t.Errorf("ETH net expected 3000, got %f", eth.NetDeltaUSD)
	}

	if snap.PortfolioGrossUSD != 8000 {
		t.Errorf("expected portfolio gross 8000, got %f", snap.PortfolioGrossUSD)
	}

	if btc.ConcentrationPct < 62 || btc.ConcentrationPct > 63 {
		t.Errorf("expected BTC concentration ~62.5%%, got %f", btc.ConcentrationPct)
	}
}

func TestComputeCorrelation_SameDirectionWarning(t *testing.T) {
	strategies := map[string]*StrategyState{
		"s1": {
			ID:   "s1",
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"s2": {
			ID:   "s2",
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"s3": {
			ID:   "s3",
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"s4": {
			ID:   "s4",
			Type: "perps",
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgStrategies := []StrategyConfig{
		{ID: "s1", Type: "spot", Args: []string{"sma", "BTC/USDT"}},
		{ID: "s2", Type: "spot", Args: []string{"ema", "BTC/USDT"}},
		{ID: "s3", Type: "spot", Args: []string{"rsi", "BTC/USDT"}},
		{ID: "s4", Type: "perps", Args: []string{"momentum", "BTC"}},
	}
	prices := map[string]float64{"BTC/USDT": 50000}
	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 100, MaxSameDirectionPct: 70}

	snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)

	hasSameDirectionWarning := false
	for _, w := range snap.Warnings {
		if strings.Contains(w, "same-direction") {
			hasSameDirectionWarning = true
		}
	}
	if !hasSameDirectionWarning {
		t.Errorf("expected same-direction warning, got warnings: %v", snap.Warnings)
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

func TestFindSpotPrice(t *testing.T) {
	cases := []struct {
		name   string
		asset  string
		prices map[string]float64
		want   float64
	}{
		{"slash-USDT form", "BTC", map[string]float64{"BTC/USDT": 50000}, 50000},
		{"bare key", "BTC", map[string]float64{"BTC": 50000}, 50000},
		{"fallback loop non-USDT", "BTC", map[string]float64{"BTC/USD": 48000}, 48000},
		{"miss returns zero", "ETH", map[string]float64{"BTC/USDT": 50000}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findSpotPrice(tc.asset, tc.prices)
			if got != tc.want {
				t.Errorf("findSpotPrice(%q) = %f, want %f", tc.asset, got, tc.want)
			}
		})
	}
}

func TestComputeCorrelation_NilConfig(t *testing.T) {
	strategies := map[string]*StrategyState{
		"sma-btc": {
			ID:   "sma-btc",
			Type: "spot",
			Positions: map[string]*Position{
				"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.1, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
	}
	cfgStrategies := []StrategyConfig{
		{ID: "sma-btc", Type: "spot", Args: []string{"sma_crossover", "BTC/USDT"}},
	}
	prices := map[string]float64{"BTC/USDT": 50000}

	snap := ComputeCorrelation(strategies, cfgStrategies, prices, nil)

	if snap.Assets["BTC"] == nil {
		t.Fatal("expected BTC asset exposure even with nil config")
	}
	if len(snap.Warnings) != 0 {
		t.Errorf("expected no warnings with nil config, got %v", snap.Warnings)
	}
}
