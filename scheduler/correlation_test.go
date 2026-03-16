package main

import (
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
	// Net: 0.1*50000 + 0.2*50000 = 15000
	if ae.NetDeltaUSD != 15000 {
		t.Errorf("expected NetDeltaUSD=15000, got %f", ae.NetDeltaUSD)
	}
	if ae.StrategyCount != 2 {
		t.Errorf("expected 2 strategies, got %d", ae.StrategyCount)
	}
	// 100% concentration (only one asset)
	if ae.ConcentrationPct != 100 {
		t.Errorf("expected 100%% concentration, got %f", ae.ConcentrationPct)
	}
	// Should have concentration warning (100% > 60%)
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
	// Net cancels: +5000 - 5000 = 0
	if ae.NetDeltaUSD != 0 {
		t.Errorf("expected NetDeltaUSD=0, got %f", ae.NetDeltaUSD)
	}
	// Gross: 5000 + 5000 = 10000
	if ae.GrossDeltaUSD != 10000 {
		t.Errorf("expected GrossDeltaUSD=10000, got %f", ae.GrossDeltaUSD)
	}
	// Concentration should be 0% (net is zero)
	if ae.ConcentrationPct != 0 {
		t.Errorf("expected 0%% concentration, got %f", ae.ConcentrationPct)
	}
	// No concentration warning
	hasConcentrationWarning := false
	for _, w := range snap.Warnings {
		if contains(w, "concentration") {
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
	// Sold call: sign=-1, delta=0.5, qty=1, spot=50000 → -1 * 0.5 * 1 * 50000 = -25000
	// Bought put: sign=+1, delta=-0.3, qty=1, spot=50000 → +1 * -0.3 * 1 * 50000 = -15000
	// Net: -25000 + -15000 = -40000
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

	// Set threshold above 100% — no warning should fire.
	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 110, MaxSameDirectionPct: 110}
	snap := ComputeCorrelation(strategies, cfgStrategies, prices, corrCfg)
	if len(snap.Warnings) != 0 {
		t.Errorf("expected no warnings with high thresholds, got %v", snap.Warnings)
	}

	// Set threshold below 100% — concentration warning should fire.
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
	// Portfolio gross: 5000 + 3000 = 8000
	if snap.PortfolioGrossUSD != 8000 {
		t.Errorf("expected portfolio gross 8000, got %f", snap.PortfolioGrossUSD)
	}
	// BTC concentration: 5000/8000*100 = 62.5% — should trigger warning at 60%
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

	// 3/4 strategies are long = 75% > 70% threshold
	hasSameDirectionWarning := false
	for _, w := range snap.Warnings {
		if contains(w, "same-direction") {
			hasSameDirectionWarning = true
		}
	}
	if !hasSameDirectionWarning {
		t.Errorf("expected same-direction warning, got warnings: %v", snap.Warnings)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
