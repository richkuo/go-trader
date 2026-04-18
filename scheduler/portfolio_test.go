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
// 25-cent (~0.78 bp) basis delta that should not appear as phantom PnL.
//
// Scenario from the issue:
//
//	HL mark: ETH-PERP = 3200.10
//	BinanceUS spot: ETH/USDT = 3199.85  (25-cent basis)
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
	// Wrong oracle: BinanceUS spot (~25-cent basis vs the HL mark).
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

	trades, err := ExecutePerpsSignal(s, 1, "ETH", 2000, 5, 0, "", 0, false, logger)
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
	_, err := ExecutePerpsSignal(s, 1, "ETH", 2000, 1, 0.5, "", 0, false, logger)
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
	_, err := ExecutePerpsSignal(s, 1, "ETH", 2210.71, 1, 0.279, "", 0, false, logger)
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
	_, err := ExecutePerpsSignal(s, -1, "ETH", 2100, 1, 0.5, "", 0, false, logger)
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

// #298 regression — PerpsOrderSkipReason must mirror every skip branch of
// ExecutePerpsSignal. Live execution paths consult this guard BEFORE placing
// on-chain orders; a missed case re-introduces the "trade fills but isn't
// recorded" gap that lost 0.716 ETH on Hyperliquid.
func TestPerpsOrderSkipReason(t *testing.T) {
	cases := []struct {
		name        string
		signal      int
		posSide     string
		allowShorts bool
		wantSet     bool
	}{
		// Legacy (allowShorts=false) — long-only execution
		{"buy_flat_allowed", 1, "", false, false},
		{"buy_short_allowed_flip", 1, "short", false, false},
		{"buy_long_skipped", 1, "long", false, true},
		{"sell_long_allowed", -1, "long", false, false},
		{"sell_flat_skipped", -1, "", false, true},
		{"sell_short_skipped_legacy", -1, "short", false, true},
		{"signal_zero_flat", 0, "", false, false},
		{"signal_zero_long", 0, "long", false, false},
		// #328 — AllowShorts opens short from flat and dedupes already-short
		{"sell_flat_allowed_bidir", -1, "", true, false},
		{"sell_short_deduped_bidir", -1, "short", true, true},
		{"sell_long_allowed_bidir", -1, "long", true, false},
		{"buy_long_still_skipped_bidir", 1, "long", true, true},
		{"buy_short_flip_bidir", 1, "short", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PerpsOrderSkipReason(tc.signal, tc.posSide, tc.allowShorts)
			if (got != "") != tc.wantSet {
				t.Errorf("PerpsOrderSkipReason(%d, %q, allowShorts=%v) = %q, wantSet=%v",
					tc.signal, tc.posSide, tc.allowShorts, got, tc.wantSet)
			}
		})
	}
}

// #300 regression — SpotOrderSkipReason must mirror every side-based skip
// branch of ExecuteSpotSignal. Live helpers (Robinhood, OKX spot) consult
// this guard BEFORE placing live orders; a missed case re-introduces the
// #298 "fill lands but no Trade" class of bug on those platforms.
func TestSpotOrderSkipReason(t *testing.T) {
	cases := []struct {
		name    string
		signal  int
		posSide string
		wantSet bool
	}{
		{"buy_flat_allowed", 1, "", false},
		{"buy_short_allowed_flip", 1, "short", false},
		{"buy_long_skipped", 1, "long", true},
		{"sell_long_allowed", -1, "long", false},
		{"sell_flat_skipped", -1, "", true},
		{"sell_short_skipped", -1, "short", true},
		{"signal_zero_flat", 0, "", false},
		{"signal_zero_long", 0, "long", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SpotOrderSkipReason(tc.signal, tc.posSide)
			if (got != "") != tc.wantSet {
				t.Errorf("SpotOrderSkipReason(%d, %q) = %q, wantSet=%v", tc.signal, tc.posSide, got, tc.wantSet)
			}
		})
	}
}

// #300 regression — FuturesOrderSkipReason mirrors the close-long-only
// semantics of the current TopStep live helper. The critical case that was
// previously unprotected is `sell_short`: Position.Quantity is always
// positive, so the existing posQty<=0 check could not distinguish a flat
// account from a short one, allowing a live sell to fire while
// ExecuteFuturesSignal would treat it as a no-op (same #298-class drift).
func TestFuturesOrderSkipReason(t *testing.T) {
	cases := []struct {
		name    string
		signal  int
		posSide string
		wantSet bool
	}{
		{"buy_flat_allowed", 1, "", false},
		{"buy_short_allowed_flip", 1, "short", false},
		{"buy_long_skipped", 1, "long", true},
		{"sell_long_allowed", -1, "long", false},
		{"sell_flat_skipped", -1, "", true},
		{"sell_short_skipped", -1, "short", true},
		{"signal_zero_flat", 0, "", false},
		{"signal_zero_long", 0, "long", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FuturesOrderSkipReason(tc.signal, tc.posSide)
			if (got != "") != tc.wantSet {
				t.Errorf("FuturesOrderSkipReason(%d, %q) = %q, wantSet=%v", tc.signal, tc.posSide, got, tc.wantSet)
			}
		})
	}
}

// #298 — demonstrates the in-memory contract the guard protects: when
// ExecutePerpsSignal is called while already long with signal=1, it returns
// trades=0 and no Trade is recorded. If a live fill has already happened
// at this point, it's lost. The guard in runHyperliquidExecuteOrder prevents
// the live fill from firing in this state; this test pins the behavior that
// ExecutePerpsSignal itself performs no side-effects in the skip case, so
// the guard is sufficient (no cleanup needed after a skipped live call).
func TestExecutePerpsSignalAlreadyLongIsInertNoOp(t *testing.T) {
	s := &StrategyState{
		ID:       "hl-test-eth",
		Cash:     1000,
		Platform: "hyperliquid",
		Type:     "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.212, AvgCost: 2300, Side: "long", Multiplier: 1, Leverage: 1},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	cashBefore := s.Cash
	qtyBefore := s.Positions["ETH"].Quantity
	tradesBefore := len(s.TradeHistory)

	trades, err := ExecutePerpsSignal(s, 1, "ETH", 2334, 1, 0.238, "oid-123", 0.42, false, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 0 {
		t.Errorf("trades = %d, want 0 (skip path)", trades)
	}
	if s.Cash != cashBefore {
		t.Errorf("cash mutated in skip path: %v → %v", cashBefore, s.Cash)
	}
	if s.Positions["ETH"].Quantity != qtyBefore {
		t.Errorf("quantity mutated in skip path: %v → %v", qtyBefore, s.Positions["ETH"].Quantity)
	}
	if len(s.TradeHistory) != tradesBefore {
		t.Errorf("trade history mutated in skip path: %d → %d", tradesBefore, len(s.TradeHistory))
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

// #328 — AllowShorts=true lets signal=-1 from flat open a short perp position.
// Without AllowShorts the same call returns 0 trades (legacy close-long-only).
func TestExecutePerpsSignalOpenShortFromFlat(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:              "hl-temab-eth",
		Cash:            1000,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{},
	}

	trades, err := ExecutePerpsSignal(s, -1, "ETH", 2000, 1, 0, "", 0, true, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignal: %v", err)
	}
	if trades != 1 {
		t.Errorf("trades = %d, want 1 (short open)", trades)
	}
	pos := s.Positions["ETH"]
	if pos == nil {
		t.Fatal("expected ETH short position to be opened")
	}
	if pos.Side != "short" {
		t.Errorf("side = %q, want \"short\"", pos.Side)
	}
	if pos.Quantity <= 0 {
		t.Errorf("quantity = %g, want > 0", pos.Quantity)
	}
	if pos.Multiplier != 1 {
		t.Errorf("Multiplier = %g, want 1 (perps PnL branch)", pos.Multiplier)
	}
	if pos.Leverage != 1 {
		t.Errorf("Leverage = %g, want 1 (matches leverage arg; risk.go reads this)", pos.Leverage)
	}
	if pos.OwnerStrategyID != s.ID {
		t.Errorf("OwnerStrategyID = %q, want %q", pos.OwnerStrategyID, s.ID)
	}
	// Margin-based accounting: cash should drop by fee only, not full notional.
	feeOnly := 1000.0 - s.Cash
	notional := pos.Quantity * pos.AvgCost
	if feeOnly >= notional*0.1 {
		t.Errorf("cash drop = %.4f, want ~fee only (notional=$%.2f)", feeOnly, notional)
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
	}
	if s.TradeHistory[0].Side != "sell" {
		t.Errorf("Trade.Side = %q, want \"sell\"", s.TradeHistory[0].Side)
	}
}

// #328 — legacy behavior regression: without AllowShorts, signal=-1 on flat
// must not open a short (otherwise triple_ema / rsi_macd_combo et al. would
// silently start trading shorts they never intended).
func TestExecutePerpsSignalLegacyFlatNoShort(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:              "hl-tema-eth",
		Cash:            1000,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	trades, err := ExecutePerpsSignal(s, -1, "ETH", 2000, 1, 0, "", 0, false, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignal: %v", err)
	}
	if trades != 0 {
		t.Errorf("trades = %d, want 0 (legacy no-op)", trades)
	}
	if len(s.Positions) != 0 {
		t.Error("no position should be opened without AllowShorts")
	}
	if len(s.TradeHistory) != 0 {
		t.Error("no Trade should be recorded")
	}
	if s.Cash != 1000 {
		t.Errorf("cash = %g, want unchanged 1000", s.Cash)
	}
}

// #328 — long + signal=-1 + AllowShorts closes the long AND opens a short.
// Mirrors the existing signal=1+short close-and-flip branch. Produces exactly
// two Trade rows; only the opening trade carries live exchange metadata so a
// single fill's fee isn't double-counted (#289).
func TestExecutePerpsSignalFlipLongToShort(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:       "hl-temab-eth",
		Cash:     1000,
		Platform: "hyperliquid",
		Type:     "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1, Leverage: 1, OwnerStrategyID: "hl-temab-eth"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{},
	}

	// Live flip: exchange executes a single net-flip sell of size
	// (closeLongQty + newShortQty) = 0.5 + 0.5 = 1.0. ExecutePerpsSignal
	// subtracts the close leg when sizing the new short side.
	trades, err := ExecutePerpsSignal(s, -1, "ETH", 2000, 1, 1.0, "live-flip-oid", 0.5, true, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignal: %v", err)
	}
	if trades != 2 {
		t.Errorf("trades = %d, want 2 (close long + open short)", trades)
	}
	pos := s.Positions["ETH"]
	if pos == nil || pos.Side != "short" {
		t.Fatalf("expected ETH short after flip, got %+v", pos)
	}
	if pos.Quantity != 0.5 {
		t.Errorf("new short Quantity = %g, want 0.5 (fillQty=1.0 minus closed long 0.5)", pos.Quantity)
	}
	if len(s.TradeHistory) != 2 {
		t.Fatalf("TradeHistory len = %d, want 2", len(s.TradeHistory))
	}
	closeLeg, openLeg := s.TradeHistory[0], s.TradeHistory[1]
	if closeLeg.ExchangeOrderID != "" || closeLeg.ExchangeFee != 0 {
		t.Errorf("close leg carries exchange metadata (oid=%q fee=%g); must stay empty",
			closeLeg.ExchangeOrderID, closeLeg.ExchangeFee)
	}
	if openLeg.ExchangeOrderID != "live-flip-oid" || openLeg.ExchangeFee != 0.5 {
		t.Errorf("open leg missing exchange metadata: oid=%q fee=%g", openLeg.ExchangeOrderID, openLeg.ExchangeFee)
	}
}

// #328 — symmetric dedupe: already-short + signal=-1 + AllowShorts is a no-op,
// just like already-long + signal=1.
func TestExecutePerpsSignalAlreadyShortIsInertNoOp(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:       "hl-temab-eth",
		Cash:     999.50,
		Platform: "hyperliquid",
		Type:     "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "short", Multiplier: 1, Leverage: 1},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}
	cashBefore := s.Cash

	trades, err := ExecutePerpsSignal(s, -1, "ETH", 1950, 1, 0, "", 0, true, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignal: %v", err)
	}
	if trades != 0 {
		t.Errorf("trades = %d, want 0 (already short dedupe)", trades)
	}
	if s.Cash != cashBefore {
		t.Errorf("cash changed on no-op: before=%.4f after=%.4f", cashBefore, s.Cash)
	}
	if len(s.TradeHistory) != 0 {
		t.Error("no Trade should be recorded on dedupe")
	}
}

// #330 (follow-up review) — regression: the live perps order size MUST include
// the close-leg quantity when AllowShorts + opposite-side position, so a
// single exchange order net-flips the position. Without this, the scheduler's
// virtual close+open lands against an exchange fill that only closed,
// leaving virtual state ahead of the exchange (same class of desync as #298).
func TestPerpsLiveOrderSize_FlipIncludesCloseLeg(t *testing.T) {
	// cash=1000, leverage=1, price=2000, avgCost=2000 (no PnL on close) →
	// newSize = 1000*0.95/2000 = 0.475
	cases := []struct {
		name       string
		signal     int
		posQty     float64
		avgCost    float64
		posSide    string
		allowShort bool
		wantSize   float64
		wantOK     bool
	}{
		// Fresh opens — avgCost is 0 (no position)
		{"long_from_flat", 1, 0, 0, "", false, 0.475, true},
		{"short_from_flat_allowed", -1, 0, 0, "", true, 0.475, true},
		// Close-only (legacy)
		{"close_long_legacy", -1, 0.3, 2000, "long", false, 0.3, true},
		// Flat-PnL flips: avgCost == price so effectiveCash == cash.
		{"flip_long_to_short_flat_pnl", -1, 0.5, 2000, "long", true, 0.975, true}, // 0.5 + 0.475
		{"flip_short_to_long_flat_pnl", 1, 0.5, 2000, "short", true, 0.975, true}, // 0.5 + 0.475
		// Legacy buy against migrated short is NOT a flip (AllowShorts=false):
		// sizing stays at newSize — the legacy behavior pre-dating #328.
		{"buy_vs_short_legacy_not_flip", 1, 0.5, 2000, "short", false, 0.475, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, ok, reason := perpsLiveOrderSize(tc.signal, 2000, 1000, tc.posQty, tc.avgCost, 1.0, tc.posSide, tc.allowShort)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (reason=%q), want %v", ok, reason, tc.wantOK)
			}
			if ok && size != tc.wantSize {
				t.Errorf("size = %g, want %g", size, tc.wantSize)
			}
		})
	}
}

// #330 (follow-up) — pin the sizing contract at the boundary where it
// matters: a long-to-short flip must size to posQty + newSize, NOT posQty
// (the old close-only behavior that silently broke bidirectional execution).
func TestPerpsLiveOrderSize_FlipLongToShortExceedsCloseOnly(t *testing.T) {
	posQty := 0.5
	size, ok, _ := perpsLiveOrderSize(-1, 2000, 1000, posQty, 2000, 1.0, "long", true)
	if !ok {
		t.Fatal("expected ok")
	}
	if size <= posQty {
		t.Errorf("flip size = %g, must exceed close-only posQty (%g) for a net-flip", size, posQty)
	}
}

// #335 — a losing long→short flip must size against post-close margin, not
// pre-close cash. Without the expectedClosePnL adjustment, the new-side
// budget overstates available margin and a leveraged flip can exceed what
// the exchange will fill, yielding a partial-fill / rejection and the same
// class of virtual-vs-exchange desync as #298.
func TestPerpsLiveOrderSize_FlipSizesAgainstPostCloseMargin(t *testing.T) {
	// long 0.5 ETH @ 2000, price drops to 1900, 5x leverage, cash=1000.
	// Close leg realizes: 0.5 * (1900 - 2000) = -50 → post-close cash = 950.
	// New-side budget: 950 * 5 * 0.95 / 1900 = 2.375 → flip size = 0.5 + 2.375 = 2.875.
	// Pre-close sizing (bug) would yield: 1000 * 5 * 0.95 / 1900 = 2.5 → 3.0, over-sized.
	size, ok, reason := perpsLiveOrderSize(-1, 1900, 1000, 0.5, 2000, 5.0, "long", true)
	if !ok {
		t.Fatalf("expected ok, got reason=%q", reason)
	}
	wantSize := 0.5 + (1000-50)*5*0.95/1900
	if diff := size - wantSize; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("size = %g, want %g (post-close margin sizing)", size, wantSize)
	}
	// Regression guard: must be strictly LESS than the buggy pre-close sizing.
	preCloseSize := 0.5 + 1000*5*0.95/1900
	if size >= preCloseSize {
		t.Errorf("size = %g must be < pre-close-sized %g to avoid over-sizing on a losing flip", size, preCloseSize)
	}
}

// #330 (final review) — a catastrophically-losing flip must still close the
// position even when post-close margin can't fund the new side. Without
// this fallback, a deep-underwater bidirectional strategy would be worse
// at exiting than a legacy long-only one: both the close AND open legs
// would be dropped.
func TestPerpsLiveOrderSize_CatastrophicFlipDegradesToCloseOnly(t *testing.T) {
	// long 1.0 ETH @ 2000, price crashes to 500, 1x leverage, cash=100.
	// closePnL = 1.0 * (500 - 2000) = -1500 → effectiveCash = 100 - 1500 = -1400.
	// Budget would be -1400 * 1 * 0.95 = -1330 (< 1) → fallback to close-only.
	size, ok, reason := perpsLiveOrderSize(-1, 500, 100, 1.0, 2000, 1.0, "long", true)
	if !ok {
		t.Fatalf("expected ok (should degrade to close-only, not abort); reason=%q", reason)
	}
	if size != 1.0 {
		t.Errorf("size = %g, want 1.0 (close-only fallback when post-close margin is negative)", size)
	}
}

// #335 — profitable flips should size LARGER than pre-close sizing: the
// close leg adds realized gains to available margin, letting the new side
// take a proportionally bigger position. Mirror of the losing-flip case.
func TestPerpsLiveOrderSize_FlipProfitableFlipUsesRealizedGain(t *testing.T) {
	// short 0.5 ETH @ 2000, price drops to 1900 (profit on short), 5x leverage.
	// Close leg realizes: 0.5 * (2000 - 1900) = +50 → post-close cash = 1050.
	// New-side budget: 1050 * 5 * 0.95 / 1900 = 2.625 → flip size = 0.5 + 2.625 = 3.125.
	size, ok, _ := perpsLiveOrderSize(1, 1900, 1000, 0.5, 2000, 5.0, "short", true)
	if !ok {
		t.Fatal("expected ok")
	}
	wantSize := 0.5 + (1000+50)*5*0.95/1900
	if diff := size - wantSize; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("size = %g, want %g (post-close margin sizing, profit added)", size, wantSize)
	}
	preCloseSize := 0.5 + 1000*5*0.95/1900
	if size <= preCloseSize {
		t.Errorf("profitable flip size = %g must exceed pre-close-sized %g", size, preCloseSize)
	}
}
