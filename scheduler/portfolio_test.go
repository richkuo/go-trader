package main

import (
	"math"
	"testing"
)

func TestPortfolioValueCashOnly(t *testing.T) {
	s := &StrategyState{
		Cash:            1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	got := PortfolioValue(s, nil)
	if got != 1000 {
		t.Errorf("PortfolioValue = %g, want 1000", got)
	}
}

func TestPortfolioValueWithPositions(t *testing.T) {
	s := &StrategyState{
		Cash: 500,
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long"},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}
	prices := map[string]float64{"BTC/USDT": 60000}

	got := PortfolioValue(s, prices)
	// Cash (500) + position value (0.01 * 60000 = 600) = 1100
	if math.Abs(got-1100) > 0.01 {
		t.Errorf("PortfolioValue = %g, want 1100", got)
	}
}

func TestPortfolioValueFallbackPrice(t *testing.T) {
	s := &StrategyState{
		Cash: 500,
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long"},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}
	// No price provided — falls back to AvgCost
	got := PortfolioValue(s, map[string]float64{})
	if math.Abs(got-1000) > 0.01 { // 500 + 0.01 * 50000 = 1000
		t.Errorf("PortfolioValue with fallback = %g, want 1000", got)
	}
}

func TestPortfolioValueFutures(t *testing.T) {
	s := &StrategyState{
		Cash: 10000,
		Positions: map[string]*Position{
			"ES": {Symbol: "ES", Quantity: 2, AvgCost: 5000, Side: "long", Multiplier: 50},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}
	prices := map[string]float64{"ES": 5100}

	got := PortfolioValue(s, prices)
	// Cash (10000) + PnL (2 * 50 * (5100 - 5000)) = 10000 + 10000 = 20000
	if math.Abs(got-20000) > 0.01 {
		t.Errorf("PortfolioValue futures = %g, want 20000", got)
	}
}

func TestPortfolioValueShort(t *testing.T) {
	s := &StrategyState{
		Cash: 1000,
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 60000, Side: "short"},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}
	prices := map[string]float64{"BTC/USDT": 55000}

	got := PortfolioValue(s, prices)
	// Cash (1000) + short profit: 0.01 * (2*60000 - 55000) = 0.01 * 65000 = 650
	if math.Abs(got-1650) > 0.01 {
		t.Errorf("PortfolioValue short = %g, want 1650", got)
	}
}

func TestPortfolioValueWithOptions(t *testing.T) {
	s := &StrategyState{
		Cash:      1000,
		Positions: make(map[string]*Position),
		OptionPositions: map[string]*OptionPosition{
			"opt1": {CurrentValueUSD: 200},
			"opt2": {CurrentValueUSD: -100}, // sold option liability
		},
	}

	got := PortfolioValue(s, nil)
	// 1000 + 200 + (-100) = 1100
	if math.Abs(got-1100) > 0.01 {
		t.Errorf("PortfolioValue with options = %g, want 1100", got)
	}
}

func TestExecuteSpotSignalHold(t *testing.T) {
	s := &StrategyState{
		Cash:            1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecuteSpotSignal(s, 0, "BTC/USDT", 60000, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 0 {
		t.Errorf("trades = %d, want 0 for hold signal", trades)
	}
	if len(s.Positions) != 0 {
		t.Error("no positions should be opened on hold")
	}
}

func TestExecuteSpotSignalBuy(t *testing.T) {
	s := &StrategyState{
		Cash:            1000,
		Platform:        "binanceus",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecuteSpotSignal(s, 1, "BTC/USDT", 50000, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Errorf("trades = %d, want 1", trades)
	}
	if s.Cash >= 1000 {
		t.Error("cash should decrease after buy")
	}
	pos := s.Positions["BTC/USDT"]
	if pos == nil {
		t.Fatal("should have BTC/USDT position")
	}
	if pos.Side != "long" {
		t.Errorf("side = %q, want %q", pos.Side, "long")
	}
	if pos.Quantity <= 0 {
		t.Error("quantity should be positive")
	}
}

func TestExecuteSpotSignalSell(t *testing.T) {
	s := &StrategyState{
		ID:       "test",
		Cash:     100,
		Platform: "binanceus",
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecuteSpotSignal(s, -1, "BTC/USDT", 55000, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Errorf("trades = %d, want 1", trades)
	}
	if _, ok := s.Positions["BTC/USDT"]; ok {
		t.Error("position should be closed after sell")
	}
	if s.Cash <= 100 {
		t.Error("cash should increase after sell")
	}
}

func TestExecuteSpotSignalBuyAlreadyLong(t *testing.T) {
	s := &StrategyState{
		Cash: 1000,
		Positions: map[string]*Position{
			"BTC/USDT": {Symbol: "BTC/USDT", Quantity: 0.01, AvgCost: 50000, Side: "long"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := ExecuteSpotSignal(s, 1, "BTC/USDT", 60000, logger)
	if trades != 0 {
		t.Error("should not buy when already long")
	}
}

func TestExecuteSpotSignalSellNoPosition(t *testing.T) {
	s := &StrategyState{
		Cash:            1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := ExecuteSpotSignal(s, -1, "BTC/USDT", 60000, logger)
	if trades != 0 {
		t.Error("should not sell when no position")
	}
}

func TestExecuteSpotSignalInsufficientCash(t *testing.T) {
	s := &StrategyState{
		Cash:            0.5, // too little
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := ExecuteSpotSignal(s, 1, "BTC/USDT", 60000, logger)
	if trades != 0 {
		t.Error("should not buy with insufficient cash")
	}
}

func TestExecuteFuturesSignalBuy(t *testing.T) {
	s := &StrategyState{
		ID:              "test",
		Cash:            10000,
		Platform:        "topstep",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	spec := ContractSpec{TickSize: 0.25, TickValue: 12.5, Multiplier: 50, Margin: 500}
	trades, err := ExecuteFuturesSignal(s, 1, "ES", 5000, spec, 2.5, 5, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Errorf("trades = %d, want 1", trades)
	}

	pos := s.Positions["ES"]
	if pos == nil {
		t.Fatal("should have ES position")
	}
	if pos.Side != "long" {
		t.Errorf("side = %q, want %q", pos.Side, "long")
	}
	if pos.Multiplier != 50 {
		t.Errorf("multiplier = %g, want 50", pos.Multiplier)
	}
}

func TestExecuteFuturesSignalHold(t *testing.T) {
	s := &StrategyState{
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	spec := ContractSpec{Multiplier: 50, Margin: 500}
	trades, _ := ExecuteFuturesSignal(s, 0, "ES", 5000, spec, 2.5, 5, logger)
	if trades != 0 {
		t.Error("should not trade on hold signal")
	}
}
