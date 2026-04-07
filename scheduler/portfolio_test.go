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

	// Verify exact post-trade cash: budget = 1000 * 0.95 = 950,
	// tradeCost = qty * execPrice = budget = 950 (cancels out),
	// fee = 950 * 0.001 = 0.95, cash = 1000 - 950 - 0.95 = 49.05
	expectedCash := 1000.0 - 1000.0*0.95 - CalculatePlatformSpotFee("binanceus", 1000.0*0.95)
	if math.Abs(s.Cash-expectedCash) > 0.01 {
		t.Errorf("cash = %.4f, want %.4f (initial - budget - fee)", s.Cash, expectedCash)
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

	// Verify exact post-trade cash using recorded execution price.
	// saleValue = qty * execPrice, fee = saleValue * 0.001,
	// netProceeds = saleValue - fee, cash = 100 + netProceeds
	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected 1 trade in history, got %d", len(s.TradeHistory))
	}
	execPrice := s.TradeHistory[0].Price
	saleValue := 0.01 * execPrice
	fee := CalculatePlatformSpotFee("binanceus", saleValue)
	expectedCash := 100.0 + saleValue - fee
	if math.Abs(s.Cash-expectedCash) > 0.01 {
		t.Errorf("cash = %.4f, want %.4f (initial + sale - fee)", s.Cash, expectedCash)
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

func TestExecuteSpotSignalOKXPerpsFee(t *testing.T) {
	s := NewStrategyState(StrategyConfig{
		ID:       "okx-perps-test",
		Type:     "perps",
		Platform: "okx",
		Capital:  1000,
	})

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	_, err := ExecuteSpotSignal(s, 1, "BTC", 50000.0, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify fee used OKX perps rate (0.05%), not spot rate (0.10%)
	// Budget is 95% of 1000 = 950
	// qty = 950 / slipped_price, fee = qty * slipped_price * 0.0005
	// Cash should reflect the perps fee rate
	if len(s.Positions) == 0 {
		t.Fatal("expected a position to be opened")
	}
	pos := s.Positions["BTC"]
	tradeCost := pos.Quantity * pos.AvgCost
	expectedFee := tradeCost * OKXPerpsTakerFeePct
	actualCash := s.Cash
	expectedCash := 1000.0 - tradeCost - expectedFee
	// Allow small floating point tolerance
	diff := actualCash - expectedCash
	if diff < -0.01 || diff > 0.01 {
		t.Errorf("cash mismatch: got %.6f, want %.6f (diff %.6f) -- wrong fee rate may have been used", actualCash, expectedCash, diff)
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

func TestExecuteSpotSignalSetsOwnerStrategyID(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-momentum-btc",
		Cash:            1000,
		Platform:        "hyperliquid",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	_, err := ExecuteSpotSignal(s, 1, "BTC", 50000, logger)
	if err != nil {
		t.Fatal(err)
	}

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("should have BTC position")
	}
	if pos.OwnerStrategyID != "hl-momentum-btc" {
		t.Errorf("OwnerStrategyID = %q, want %q", pos.OwnerStrategyID, "hl-momentum-btc")
	}
}

func TestExecuteFuturesSignalSetsOwnerStrategyID(t *testing.T) {
	s := &StrategyState{
		ID:              "ts-momentum-es",
		Cash:            10000,
		Platform:        "topstep",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	spec := ContractSpec{TickSize: 0.25, TickValue: 12.5, Multiplier: 50, Margin: 500}
	_, err := ExecuteFuturesSignal(s, 1, "ES", 5000, spec, 2.5, 5, logger)
	if err != nil {
		t.Fatal(err)
	}

	pos := s.Positions["ES"]
	if pos == nil {
		t.Fatal("should have ES position")
	}
	if pos.OwnerStrategyID != "ts-momentum-es" {
		t.Errorf("OwnerStrategyID = %q, want %q", pos.OwnerStrategyID, "ts-momentum-es")
	}
}

func TestExecuteFuturesSignalShortSetsOwnerStrategyID(t *testing.T) {
	s := &StrategyState{
		ID:              "ts-trend-es",
		Cash:            10000,
		Platform:        "topstep",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	spec := ContractSpec{TickSize: 0.25, TickValue: 12.5, Multiplier: 50, Margin: 500}
	_, err := ExecuteFuturesSignal(s, -1, "ES", 5000, spec, 2.5, 5, logger)
	if err != nil {
		t.Fatal(err)
	}

	pos := s.Positions["ES"]
	if pos == nil {
		t.Fatal("should have ES short position")
	}
	if pos.Side != "short" {
		t.Errorf("side = %q, want %q", pos.Side, "short")
	}
	if pos.OwnerStrategyID != "ts-trend-es" {
		t.Errorf("OwnerStrategyID = %q, want %q", pos.OwnerStrategyID, "ts-trend-es")
	}
}
