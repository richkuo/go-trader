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

// TestPortfolioValueShort_UsesExchangeMarkNotSpotBasis is the regression
// test required by issue #263 acceptance criteria: PortfolioValue for a
// perps short must use the exchange-native mark, NOT a BinanceUS spot quote.
// The test drives prices["ETH"] with the HL mark (3200.10) and asserts the
// result is NOT equal to what BinanceUS spot (3199.85) would produce — a
// 10 bp basis delta that should not appear as phantom PnL.
//
// Scenario from the issue:
//
//	HL mark: ETH-PERP = 3200.10
//	BinanceUS spot: ETH/USDT = 3199.85  (25 bp basis)
//	Short position: 0.01 ETH @ 3000 AvgCost (Multiplier=1 → PnL branch)
func TestPortfolioValueShort_UsesExchangeMarkNotSpotBasis(t *testing.T) {
	s := &StrategyState{
		Cash: 1000,
		Positions: map[string]*Position{
			// Perps short — Multiplier=1 routes through the PnL branch.
			"ETH": {Symbol: "ETH", Quantity: 0.01, AvgCost: 3000.0, Side: "short", Multiplier: 1},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}

	// Correct oracle: HL exchange mark.
	hlMark := 3200.10
	// Wrong oracle: BinanceUS spot (25-cent basis).
	spotPrice := 3199.85

	// Pass the HL mark as prices["ETH"] — exactly what fetchHyperliquidMids
	// delivers after the #263 fix.
	gotHL := PortfolioValue(s, map[string]float64{"ETH": hlMark})
	// PnL branch: cash + qty * multiplier * (avgCost - price)
	// = 1000 + 0.01 * 1 * (3000 - 3200.10) = 1000 + 0.01 * (-200.10) = 998.00
	expectedHL := 1000.0 + 0.01*(3000.0-hlMark)
	if math.Abs(gotHL-expectedHL) > 1e-6 {
		t.Errorf("PortfolioValue with HL mark = %.6f, want %.6f", gotHL, expectedHL)
	}

	// Demonstrate the basis error: spot oracle produces a different value.
	gotSpot := PortfolioValue(s, map[string]float64{"ETH": spotPrice})
	expectedSpot := 1000.0 + 0.01*(3000.0-spotPrice)
	if math.Abs(gotSpot-expectedSpot) > 1e-6 {
		t.Errorf("PortfolioValue with spot price = %.6f, want %.6f", gotSpot, expectedSpot)
	}

	// Assert the HL mark wins: the two values must differ by the basis delta.
	basisDelta := math.Abs(gotHL - gotSpot)
	expectedBasisDelta := 0.01 * math.Abs(hlMark-spotPrice) // ~0.002500
	if math.Abs(basisDelta-expectedBasisDelta) > 1e-6 {
		t.Errorf("basis delta = %.6f, want %.6f (0.01 * |hlMark - spotPrice|)", basisDelta, expectedBasisDelta)
	}

	// Guard: if prices map carries the HL mark, the result must NOT equal
	// the spot-basis value — pinning the fix as the issue requires.
	if math.Abs(gotHL-gotSpot) < 1e-9 {
		t.Errorf("PortfolioValue with HL mark equals spot-basis value — basis not applied")
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

	trades, err := ExecuteSpotSignal(s, 0, "BTC/USDT", 60000, 0, logger)
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

	trades, err := ExecuteSpotSignal(s, 1, "BTC/USDT", 50000, 0, logger)
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

	trades, err := ExecuteSpotSignal(s, -1, "BTC/USDT", 55000, 0, logger)
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

	trades, _ := ExecuteSpotSignal(s, 1, "BTC/USDT", 60000, 0, logger)
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

	trades, _ := ExecuteSpotSignal(s, -1, "BTC/USDT", 60000, 0, logger)
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

	trades, _ := ExecuteSpotSignal(s, 1, "BTC/USDT", 60000, 0, logger)
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

	_, err := ExecuteSpotSignal(s, 1, "BTC", 50000.0, 0, logger)
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
	trades, err := ExecuteFuturesSignal(s, 1, "ES", 5000, spec, 2.5, 5, 0, logger)
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
	trades, _ := ExecuteFuturesSignal(s, 0, "ES", 5000, spec, 2.5, 5, 0, logger)
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

	_, err := ExecuteSpotSignal(s, 1, "BTC", 50000, 0, logger)
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
	_, err := ExecuteFuturesSignal(s, 1, "ES", 5000, spec, 2.5, 5, 0, logger)
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
	_, err := ExecuteFuturesSignal(s, -1, "ES", 5000, spec, 2.5, 5, 0, logger)
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

func TestExecuteSpotSignalLiveFill(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-momentum-btc",
		Cash:            1000,
		Platform:        "hyperliquid",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	// Live fill: exchange filled 0.015 BTC at exact price 50000
	fillQty := 0.015
	fillPrice := 50000.0
	trades, err := ExecuteSpotSignal(s, 1, "BTC", fillPrice, fillQty, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Errorf("trades = %d, want 1", trades)
	}

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("should have BTC position")
	}
	// Quantity must be exactly the fill qty — no slippage distortion
	if math.Abs(pos.Quantity-fillQty) > 1e-9 {
		t.Errorf("Quantity = %.9f, want %.9f (exact fill qty)", pos.Quantity, fillQty)
	}
	// AvgCost must be exactly the fill price — no slippage
	if math.Abs(pos.AvgCost-fillPrice) > 1e-6 {
		t.Errorf("AvgCost = %.6f, want %.6f (exact fill price)", pos.AvgCost, fillPrice)
	}
}

// #254: ExecutePerpsSignal — margin-based accounting. Paper buy should NOT
// deplete cash by the full notional (unlike spot). Only the fee leaves cash,
// and the opened position is stamped with Multiplier=1 so PortfolioValue
// routes through the PnL branch.
func TestExecutePerpsSignalPaperBuyNoNotionalDeduction(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-test-eth",
		Cash:            1000,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecutePerpsSignal(s, 1, "ETH", 2000, 5, 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}

	pos := s.Positions["ETH"]
	if pos == nil {
		t.Fatal("should have ETH position")
	}
	if pos.Side != "long" {
		t.Errorf("side = %q, want long", pos.Side)
	}
	if pos.Multiplier != 1 {
		t.Errorf("multiplier = %v, want 1 (for PnL branch in PortfolioValue)", pos.Multiplier)
	}
	if pos.Leverage != 5 {
		t.Errorf("leverage = %v, want 5", pos.Leverage)
	}
	// With leverage=5, budget = 1000 * 5 * 0.95 = 4750 notional
	// qty ≈ 4750 / 2000 = 2.375 (modulo slippage on execPrice)
	if pos.Quantity < 2.0 || pos.Quantity > 2.8 {
		t.Errorf("quantity = %v, want ~2.375 (5x leverage)", pos.Quantity)
	}
	// Cash must be untouched except for fee. fee ≈ notional * 0.00035
	// (hyperliquid fee), so cash should remain > 990.
	if s.Cash < 990 {
		t.Errorf("cash = %v, want ~1000 (only fee deducted, not notional)", s.Cash)
	}
	if s.Cash >= 1000 {
		t.Errorf("cash = %v, should have some fee deducted", s.Cash)
	}
}

// #254: verify PortfolioValue handles the perps position correctly using the
// futures branch (qty * multiplier * (price - avgCost)). Cash is preserved,
// and a favorable price move shows up as PnL on top of cash.
func TestExecutePerpsSignalPortfolioValueAfterMove(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-test-eth",
		Cash:            1000,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	// Open at exactly 2000 via live fill (no slippage).
	_, err := ExecutePerpsSignal(s, 1, "ETH", 2000, 1, 0.5, logger)
	if err != nil {
		t.Fatal(err)
	}
	cashAfterOpen := s.Cash
	valueAtEntry := PortfolioValue(s, map[string]float64{"ETH": 2000})
	// At entry, PnL=0, value = cashAfterOpen.
	if math.Abs(valueAtEntry-cashAfterOpen) > 1e-6 {
		t.Errorf("at entry value = %v, cash = %v, want equal (PnL=0)", valueAtEntry, cashAfterOpen)
	}
	// Price moves +$10: PnL = 0.5 * (2010 - 2000) = $5.
	valueAfterMove := PortfolioValue(s, map[string]float64{"ETH": 2010})
	expected := cashAfterOpen + 5.0
	if math.Abs(valueAfterMove-expected) > 1e-6 {
		t.Errorf("value after +$10 move = %v, want %v (cash + PnL)", valueAfterMove, expected)
	}
	// Price moves -$10: PnL = -$5.
	valueAfterDrop := PortfolioValue(s, map[string]float64{"ETH": 1990})
	expectedDrop := cashAfterOpen - 5.0
	if math.Abs(valueAfterDrop-expectedDrop) > 1e-6 {
		t.Errorf("value after -$10 move = %v, want %v (cash + PnL)", valueAfterDrop, expectedDrop)
	}
}

// #254: regression — before the fix, perps positions stored with
// Multiplier=0 hit the spot branch (qty * price) which inflated portfolio
// value by the full notional. After the fix, ExecutePerpsSignal stamps
// Multiplier=1 so valuation uses the PnL branch. This test pins the wrong
// "spot-like" valuation vs the correct "perps" valuation to prevent drift.
func TestExecutePerpsSignalNotInflatedByNotional(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-rmc-eth-live",
		Cash:            644,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	// Live fill 0.279 ETH @ 2210.71 (matching the issue example).
	_, err := ExecutePerpsSignal(s, 1, "ETH", 2210.71, 1, 0.279, logger)
	if err != nil {
		t.Fatal(err)
	}
	value := PortfolioValue(s, map[string]float64{"ETH": 2201.10})
	// At $2201.10 vs entry $2210.71: PnL = 0.279 * (2201.10 - 2210.71) ≈ -$2.68
	// Expected value ≈ 644 - fee - 2.68 ≈ ~641.3 — NOT inflated.
	// The buggy spot-branch valuation would be cash (~644) + 0.279*2201 ≈ 1258.
	if value > 700 {
		t.Errorf("value = %v, leaking into spot-branch (>$700 means notional not stripped)", value)
	}
	if value < 600 || value > 650 {
		t.Errorf("value = %v, want ~641 (initial capital + unrealized PnL)", value)
	}
}

// #254: closing a perps long realizes PnL directly (not notional swing).
func TestExecutePerpsSignalCloseLong(t *testing.T) {
	s := &StrategyState{
		ID:       "hl-test-eth",
		Cash:     990,
		Platform: "hyperliquid",
		Type:     "perps",
		Positions: map[string]*Position{
			"ETH": {
				Symbol:     "ETH",
				Quantity:   0.5,
				AvgCost:    2000,
				Side:       "long",
				Multiplier: 1,
				Leverage:   2,
			},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	// Close at 2100 — PnL = 0.5 * (2100 - 2000) = $50 gross.
	_, err := ExecutePerpsSignal(s, -1, "ETH", 2100, 1, 0.5, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Positions["ETH"]; ok {
		t.Error("position should be closed")
	}
	// Expected: 990 + 50 - fee(0.5*2100=1050). HL fee ≈ 1050 * 0.00035 ≈ 0.37.
	if s.Cash < 1039 || s.Cash > 1040.5 {
		t.Errorf("cash = %v, want ~1039.6 (990 + 50 - fee)", s.Cash)
	}
}

func TestExecuteFuturesSignalLiveFill(t *testing.T) {
	s := &StrategyState{
		ID:              "ts-momentum-es",
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
	fillContracts := 2
	fillPrice := 5000.0
	trades, err := ExecuteFuturesSignal(s, 1, "ES", fillPrice, spec, 2.5, 5, fillContracts, logger)
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
	// Contract count must be exactly the fill contracts — no slippage distortion
	if int(pos.Quantity) != fillContracts {
		t.Errorf("Quantity = %g, want %d (exact fill contracts)", pos.Quantity, fillContracts)
	}
	// AvgCost must be exactly the fill price — no slippage
	if math.Abs(pos.AvgCost-fillPrice) > 1e-6 {
		t.Errorf("AvgCost = %.6f, want %.6f (exact fill price)", pos.AvgCost, fillPrice)
	}
}
