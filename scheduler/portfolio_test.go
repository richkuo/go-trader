package main

import (
	"encoding/json"
	"math"
	"testing"
)

func TestPositionJSONUsesPositionID(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{
			name:  "position",
			value: Position{Symbol: "BTC", TradePositionID: "spot-position-1"},
		},
		{
			name:  "option_position",
			value: OptionPosition{ID: "opt-1", TradePositionID: "option-position-1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if _, ok := payload["position_id"]; !ok {
				t.Fatalf("position_id missing from %s JSON: %s", tc.name, raw)
			}
			if _, ok := payload["trade_position_id"]; ok {
				t.Fatalf("trade_position_id should not be emitted in %s JSON: %s", tc.name, raw)
			}
		})
	}
}

func TestExecuteSpotResultSetsInitialQuantityAndEntryATR(t *testing.T) {
	prevRecorder := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prevRecorder })

	state := &StrategyState{
		ID:        "spot-test",
		Type:      "spot",
		Platform:  "binanceus",
		Cash:      1000,
		Positions: map[string]*Position{},
	}
	result := &SpotResult{
		Symbol:     "BTC/USDT",
		Signal:     1,
		Indicators: map[string]interface{}{"atr": 3.5},
	}
	trades, _ := executeSpotResult(
		StrategyConfig{ID: "spot-test"},
		state,
		nil,
		result,
		"BUY",
		100,
		nil,
		nil,
		HurstGateDecision{},
		silentStrategyLogger("spot-test"),
	)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	pos := state.Positions["BTC/USDT"]
	if pos == nil {
		t.Fatal("position was not opened")
	}
	if pos.InitialQuantity != pos.Quantity {
		t.Fatalf("InitialQuantity = %g, want current open qty %g", pos.InitialQuantity, pos.Quantity)
	}
	if pos.EntryATR != 3.5 {
		t.Fatalf("EntryATR = %g, want 3.5", pos.EntryATR)
	}
}

func TestPartialClosePreservesInitialQuantityAndEntryATR(t *testing.T) {
	prevRecorder := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prevRecorder })

	state := &StrategyState{
		ID:       "hl-test",
		Type:     "perps",
		Platform: "hyperliquid",
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol:          "ETH",
				Quantity:        1,
				InitialQuantity: 1,
				AvgCost:         3000,
				EntryATR:        150,
				Side:            "long",
				Multiplier:      1,
			},
		},
	}

	applyHyperliquidCircuitCloseFill(state, "ETH", 0.4, 3100, 1, 1, 0, "")
	pos := state.Positions["ETH"]
	if pos == nil {
		t.Fatal("position should remain after partial close")
	}
	if math.Abs(pos.Quantity-0.6) > 1e-9 {
		t.Fatalf("Quantity = %g, want 0.6", pos.Quantity)
	}
	if pos.InitialQuantity != 1 {
		t.Fatalf("InitialQuantity = %g, want 1", pos.InitialQuantity)
	}
	if pos.EntryATR != 150 {
		t.Fatalf("EntryATR = %g, want 150", pos.EntryATR)
	}
}

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
	got := PortfolioValue(s, map[string]float64{})
	if math.Abs(got-1000) > 0.01 {
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
	if math.Abs(got-1650) > 0.01 {
		t.Errorf("PortfolioValue short = %g, want 1650", got)
	}
}

func TestPortfolioValueShort_UsesExchangeMarkNotSpotBasis(t *testing.T) {
	s := &StrategyState{
		Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.01, AvgCost: 3000.0, Side: "short", Multiplier: 1},
		},
		OptionPositions: make(map[string]*OptionPosition),
	}

	hlMark := 3200.10
	spotPrice := 3199.85

	gotHL := PortfolioValue(s, map[string]float64{"ETH": hlMark})
	expectedHL := 1000.0 + 0.01*(3000.0-hlMark)
	if math.Abs(gotHL-expectedHL) > 1e-6 {
		t.Errorf("PortfolioValue with HL mark = %.6f, want %.6f", gotHL, expectedHL)
	}

	gotSpot := PortfolioValue(s, map[string]float64{"ETH": spotPrice})
	expectedSpot := 1000.0 + 0.01*(3000.0-spotPrice)
	if math.Abs(gotSpot-expectedSpot) > 1e-6 {
		t.Errorf("PortfolioValue with spot price = %.6f, want %.6f", gotSpot, expectedSpot)
	}

	basisDelta := math.Abs(gotHL - gotSpot)
	expectedBasisDelta := 0.01 * math.Abs(hlMark-spotPrice)
	if math.Abs(basisDelta-expectedBasisDelta) > 1e-6 {
		t.Errorf("basis delta = %.6f, want %.6f (0.01 * |hlMark - spotPrice|)", basisDelta, expectedBasisDelta)
	}

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
			"opt2": {CurrentValueUSD: -100},
		},
	}

	got := PortfolioValue(s, nil)
	if math.Abs(got-1100) > 0.01 {
		t.Errorf("PortfolioValue with options = %g, want 1100", got)
	}
}

func TestExecuteSpotWithFillFeeHold(t *testing.T) {
	s := &StrategyState{
		Cash:            1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecuteSpotSignalWithFillFee(s, 0, "BTC/USDT", 60000, 0, 0, "", 0, logger)
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

func TestExecuteSpotWithFillFeeBuy(t *testing.T) {
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

	trades, err := ExecuteSpotSignalWithFillFee(s, 1, "BTC/USDT", 50000, 0, 0, "", 0, logger)
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

	expectedCash := 1000.0 - 1000.0 - CalculatePlatformSpotFee("binanceus", 1000.0)
	if math.Abs(s.Cash-expectedCash) > 0.01 {
		t.Errorf("cash = %.4f, want %.4f (initial - budget - fee)", s.Cash, expectedCash)
	}
}

func TestExecuteSpotWithFillFeeSell(t *testing.T) {
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

	trades, err := ExecuteSpotSignalWithFillFee(s, -1, "BTC/USDT", 55000, 0, 0, "", 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Errorf("trades = %d, want 1", trades)
	}
	if _, ok := s.Positions["BTC/USDT"]; ok {
		t.Error("position should be closed after sell")
	}

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

func TestExecuteSpotWithFillFeeBuyAlreadyLong(t *testing.T) {
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

	trades, _ := ExecuteSpotSignalWithFillFee(s, 1, "BTC/USDT", 60000, 0, 0, "", 0, logger)
	if trades != 0 {
		t.Error("should not buy when already long")
	}
}

func TestExecuteSpotWithFillFeeSellNoPosition(t *testing.T) {
	s := &StrategyState{
		Cash:            1000,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := ExecuteSpotSignalWithFillFee(s, -1, "BTC/USDT", 60000, 0, 0, "", 0, logger)
	if trades != 0 {
		t.Error("should not sell when no position")
	}
}

func TestExecuteSpotWithFillFeeInsufficientCash(t *testing.T) {
	s := &StrategyState{
		Cash:            0.5,
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, _ := ExecuteSpotSignalWithFillFee(s, 1, "BTC/USDT", 60000, 0, 0, "", 0, logger)
	if trades != 0 {
		t.Error("should not buy with insufficient cash")
	}
}

func TestExecuteSpotWithFillFeeOKXPerpsFee(t *testing.T) {
	s := NewStrategyState(StrategyConfig{
		ID:       "okx-perps-test",
		Type:     "perps",
		Platform: "okx",
		Capital:  1000,
	})

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	_, err := ExecuteSpotSignalWithFillFee(s, 1, "BTC", 50000.0, 0, 0, "", 0, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.Positions) == 0 {
		t.Fatal("expected a position to be opened")
	}
	pos := s.Positions["BTC"]
	tradeCost := pos.Quantity * pos.AvgCost
	expectedFee := tradeCost * OKXPerpsTakerFeePct
	actualCash := s.Cash
	expectedCash := 1000.0 - tradeCost - expectedFee
	diff := actualCash - expectedCash
	if diff < -0.01 || diff > 0.01 {
		t.Errorf("cash mismatch: got %.6f, want %.6f (diff %.6f) -- wrong fee rate may have been used", actualCash, expectedCash, diff)
	}
}

func TestExecuteFuturesWithFillFeeBuy(t *testing.T) {
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
	trades, err := ExecuteFuturesSignalWithFillFee(s, 1, "ES", 5000, spec, 2.5, 5, 0, 0, "", 0, logger)
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

func TestExecuteFuturesWithFillFeeHold(t *testing.T) {
	s := &StrategyState{
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	spec := ContractSpec{Multiplier: 50, Margin: 500}
	trades, _ := ExecuteFuturesSignalWithFillFee(s, 0, "ES", 5000, spec, 2.5, 5, 0, 0, "", 0, logger)
	if trades != 0 {
		t.Error("should not trade on hold signal")
	}
}

func TestExecuteSpotWithFillFeeSetsOwnerStrategyID(t *testing.T) {
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

	_, err := ExecuteSpotSignalWithFillFee(s, 1, "BTC", 50000, 0, 0, "", 0, logger)
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

func TestExecuteFuturesWithFillFeeSetsOwnerStrategyID(t *testing.T) {
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
	_, err := ExecuteFuturesSignalWithFillFee(s, 1, "ES", 5000, spec, 2.5, 5, 0, 0, "", 0, logger)
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

func TestExecuteFuturesWithFillFeeShortSetsOwnerStrategyID(t *testing.T) {
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
	_, err := ExecuteFuturesSignalWithFillFee(s, -1, "ES", 5000, spec, 2.5, 5, 0, 0, "", 0, logger)
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

func TestExecuteSpotWithFillFeeLiveFill(t *testing.T) {
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

	fillQty := 0.015
	fillPrice := 50000.0
	trades, err := ExecuteSpotSignalWithFillFee(s, 1, "BTC", fillPrice, fillQty, 0, "", 0, logger)
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
	if math.Abs(pos.Quantity-fillQty) > 1e-9 {
		t.Errorf("Quantity = %.9f, want %.9f (exact fill qty)", pos.Quantity, fillQty)
	}
	if math.Abs(pos.AvgCost-fillPrice) > 1e-6 {
		t.Errorf("AvgCost = %.6f, want %.6f (exact fill price)", pos.AvgCost, fillPrice)
	}
}

func TestExecutionFeeSelection(t *testing.T) {
	cases := []struct {
		name       string
		modeledFee float64
		fillFee    float64
		useFillFee bool
		want       float64
	}{
		{"zero_fill_fee_falls_back", 0.35, 0, true, 0.35},
		{"non_zero_fill_fee_uses_real", 0.35, 0.12, true, 0.12},
		{"flip_open_leg_uses_modeled", 0.35, 0.12, false, 0.35},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := executionFee(tc.modeledFee, tc.fillFee, tc.useFillFee)
			if got != tc.want {
				t.Errorf("executionFee(%g, %g, %v) = %g, want %g",
					tc.modeledFee, tc.fillFee, tc.useFillFee, got, tc.want)
			}
		})
	}
}

func TestExecuteSpotWithFillFeeLiveFillUsesExchangeFee(t *testing.T) {
	s := &StrategyState{
		ID:              "rh-momentum-btc",
		Cash:            1000,
		Platform:        "robinhood",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	fillQty := 0.015
	fillPrice := 50000.0
	fillFee := 0.17
	trades, err := ExecuteSpotSignalWithFillFee(s, 1, "BTC", fillPrice, fillQty, fillFee, "", 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Errorf("trades = %d, want 1", trades)
	}
	wantCash := 1000.0 - fillQty*fillPrice - fillFee
	if math.Abs(s.Cash-wantCash) > 1e-9 {
		t.Errorf("cash = %.9f, want %.9f (live fill fee)", s.Cash, wantCash)
	}
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].ExchangeFee != fillFee {
		t.Fatalf("ExchangeFee = %v, want %v", s.TradeHistory, fillFee)
	}
}

func TestExecuteSpotWithFillFeeLiveFillUsesExchangeOrderID(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	t.Run("open", func(t *testing.T) {
		s := &StrategyState{
			ID:              "rh-momentum-btc",
			Cash:            1000,
			Platform:        "robinhood",
			Positions:       make(map[string]*Position),
			OptionPositions: make(map[string]*OptionPosition),
			TradeHistory:    []Trade{},
			RiskState:       RiskState{},
		}

		trades, err := ExecuteSpotSignalWithFillFee(s, 1, "BTC", 50000, 0.015, 0.17, "rh-open-oid", 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if trades != 1 {
			t.Fatalf("trades = %d, want 1", trades)
		}
		if got := s.TradeHistory[0].ExchangeOrderID; got != "rh-open-oid" {
			t.Errorf("ExchangeOrderID = %q, want rh-open-oid", got)
		}
	})

	t.Run("close", func(t *testing.T) {
		s := &StrategyState{
			ID:              "rh-momentum-btc",
			Cash:            250,
			Platform:        "robinhood",
			Positions:       map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.015, AvgCost: 49000, Side: "long"}},
			OptionPositions: make(map[string]*OptionPosition),
			TradeHistory:    []Trade{},
			RiskState:       RiskState{},
		}

		trades, err := ExecuteSpotSignalWithFillFee(s, -1, "BTC", 50000, 0.015, 0.17, "rh-close-oid", 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if trades != 1 {
			t.Fatalf("trades = %d, want 1", trades)
		}
		if got := s.TradeHistory[0].ExchangeOrderID; got != "rh-close-oid" {
			t.Errorf("ExchangeOrderID = %q, want rh-close-oid", got)
		}
	})
}

func TestExecutePerpsWithLeveragePaperBuyNoNotionalDeduction(t *testing.T) {
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

	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 5, ExchangeLeverage: 5}, 0, "", 0, DirectionLong, 0, logger)
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
	if pos.Quantity < 2.2 || pos.Quantity > 2.8 {
		t.Errorf("quantity = %v, want ~2.5 (5x leverage)", pos.Quantity)
	}
	if s.Cash < 990 {
		t.Errorf("cash = %v, want ~1000 (only fee deducted, not notional)", s.Cash)
	}
	if s.Cash >= 1000 {
		t.Errorf("cash = %v, should have some fee deducted", s.Cash)
	}
}

func TestExecutePerpsWithLeverageDecouplesSizingAndExchangeLeverage(t *testing.T) {
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

	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 2, ExchangeLeverage: 20}, 0, "", 0, DirectionLong, 0, logger)
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
	if pos.Leverage != 20 {
		t.Errorf("position leverage = %g, want exchange leverage 20", pos.Leverage)
	}
	if pos.Quantity < 0.85 || pos.Quantity > 1.15 {
		t.Errorf("quantity = %g, want ~1.0 from sizing_leverage=2", pos.Quantity)
	}
	if pos.Quantity > 5 {
		t.Errorf("quantity = %g, appears to have used exchange leverage for sizing", pos.Quantity)
	}
}

func TestExecutePerpsWithLeverageLiveOpenUsesExchangeFee(t *testing.T) {
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

	fillFee := 0.42
	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.5, "oid-1", fillFee, DirectionLong, 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	wantCash := 1000.0 - fillFee
	if math.Abs(s.Cash-wantCash) > 1e-9 {
		t.Errorf("cash = %.9f, want %.9f (real fill fee)", s.Cash, wantCash)
	}
	if s.TradeHistory[0].ExchangeFee != fillFee {
		t.Errorf("ExchangeFee = %g, want %g", s.TradeHistory[0].ExchangeFee, fillFee)
	}
}

func TestExecutePerpsWithLeveragePortfolioValueAfterMove(t *testing.T) {
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

	_, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.5, "", 0, DirectionLong, 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	cashAfterOpen := s.Cash
	valueAtEntry := PortfolioValue(s, map[string]float64{"ETH": 2000})
	if math.Abs(valueAtEntry-cashAfterOpen) > 1e-6 {
		t.Errorf("at entry value = %v, cash = %v, want equal (PnL=0)", valueAtEntry, cashAfterOpen)
	}
	valueAfterMove := PortfolioValue(s, map[string]float64{"ETH": 2010})
	expected := cashAfterOpen + 5.0
	if math.Abs(valueAfterMove-expected) > 1e-6 {
		t.Errorf("value after +$10 move = %v, want %v (cash + PnL)", valueAfterMove, expected)
	}
	valueAfterDrop := PortfolioValue(s, map[string]float64{"ETH": 1990})
	expectedDrop := cashAfterOpen - 5.0
	if math.Abs(valueAfterDrop-expectedDrop) > 1e-6 {
		t.Errorf("value after -$10 move = %v, want %v (cash + PnL)", valueAfterDrop, expectedDrop)
	}
}

func TestExecutePerpsWithLeverageNotInflatedByNotional(t *testing.T) {
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

	_, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2210.71, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.279, "", 0, DirectionLong, 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	value := PortfolioValue(s, map[string]float64{"ETH": 2201.10})
	if value > 700 {
		t.Errorf("value = %v, leaking into spot-branch (>$700 means notional not stripped)", value)
	}
	if value < 600 || value > 650 {
		t.Errorf("value = %v, want ~641 (initial capital + unrealized PnL)", value)
	}
}

func TestExecutePerpsWithLeverageCloseLong(t *testing.T) {
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

	_, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2100, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.5, "", 0, DirectionLong, 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Positions["ETH"]; ok {
		t.Error("position should be closed")
	}
	if s.Cash < 1039 || s.Cash > 1040.5 {
		t.Errorf("cash = %v, want ~1039.6 (990 + 50 - fee)", s.Cash)
	}
}

func TestPerpsOrderSkipReason(t *testing.T) {
	cases := []struct {
		name      string
		signal    int
		posSide   string
		direction string
		wantSet   bool
	}{
		{"buy_flat_allowed_long", 1, "", DirectionLong, false},
		{"buy_short_allowed_flip_long", 1, "short", DirectionLong, false},
		{"buy_long_skipped_long", 1, "long", DirectionLong, true},
		{"sell_long_allowed_long", -1, "long", DirectionLong, false},
		{"sell_flat_skipped_long", -1, "", DirectionLong, true},
		{"sell_short_skipped_long", -1, "short", DirectionLong, true},
		{"signal_zero_flat", 0, "", DirectionLong, false},
		{"signal_zero_long", 0, "long", DirectionLong, false},
		{"empty_dir_buy_long_skipped", 1, "long", "", true},
		{"empty_dir_sell_flat_skipped", -1, "", "", true},
		{"sell_flat_allowed_both", -1, "", DirectionBoth, false},
		{"sell_short_deduped_both", -1, "short", DirectionBoth, true},
		{"sell_long_allowed_both", -1, "long", DirectionBoth, false},
		{"buy_long_skipped_both", 1, "long", DirectionBoth, true},
		{"buy_short_flip_both", 1, "short", DirectionBoth, false},
		{"sell_flat_allowed_short", -1, "", DirectionShort, false},
		{"sell_short_skipped_short", -1, "short", DirectionShort, true},
		{"sell_long_skipped_short", -1, "long", DirectionShort, true},
		{"buy_short_allowed_short", 1, "short", DirectionShort, false},
		{"buy_flat_skipped_short", 1, "", DirectionShort, true},
		{"buy_long_skipped_short", 1, "long", DirectionShort, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PerpsOrderSkipReason(tc.signal, tc.posSide, tc.direction)
			if (got != "") != tc.wantSet {
				t.Errorf("PerpsOrderSkipReason(%d, %q, direction=%q) = %q, wantSet=%v",
					tc.signal, tc.posSide, tc.direction, got, tc.wantSet)
			}
		})
	}
}

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

func TestExecutePerpsWithLeverageAlreadyLongIsInertNoOp(t *testing.T) {
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

	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2334, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.238, "oid-123", 0.42, DirectionLong, 0, logger)
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

func TestExecuteFuturesWithFillFeeLiveFill(t *testing.T) {
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
	trades, err := ExecuteFuturesSignalWithFillFee(s, 1, "ES", fillPrice, spec, 2.5, 5, fillContracts, 0, "", 0, logger)
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
	if int(pos.Quantity) != fillContracts {
		t.Errorf("Quantity = %g, want %d (exact fill contracts)", pos.Quantity, fillContracts)
	}
	if math.Abs(pos.AvgCost-fillPrice) > 1e-6 {
		t.Errorf("AvgCost = %.6f, want %.6f (exact fill price)", pos.AvgCost, fillPrice)
	}
}

func TestExecuteFuturesWithFillFeeLiveFillUsesExchangeFee(t *testing.T) {
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
	fillFee := 4.12
	trades, err := ExecuteFuturesSignalWithFillFee(s, 1, "ES", 5000, spec, 2.5, 5, 2, fillFee, "", 0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	wantCash := 10000.0 - fillFee
	if math.Abs(s.Cash-wantCash) > 1e-9 {
		t.Errorf("cash = %.9f, want %.9f (real fill fee)", s.Cash, wantCash)
	}
	if s.TradeHistory[0].ExchangeFee != fillFee {
		t.Errorf("ExchangeFee = %g, want %g", s.TradeHistory[0].ExchangeFee, fillFee)
	}
}

func TestExecuteFuturesWithFillFeeLiveFillUsesExchangeOrderID(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	spec := ContractSpec{TickSize: 0.25, TickValue: 12.5, Multiplier: 50, Margin: 500}

	t.Run("open", func(t *testing.T) {
		s := &StrategyState{
			ID:              "ts-momentum-es",
			Cash:            10000,
			Platform:        "topstep",
			Positions:       make(map[string]*Position),
			OptionPositions: make(map[string]*OptionPosition),
			TradeHistory:    []Trade{},
			RiskState:       RiskState{},
		}

		trades, err := ExecuteFuturesSignalWithFillFee(s, 1, "ES", 5000, spec, 2.5, 5, 2, 4.12, "ts-open-oid", 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if trades != 1 {
			t.Fatalf("trades = %d, want 1", trades)
		}
		if got := s.TradeHistory[0].ExchangeOrderID; got != "ts-open-oid" {
			t.Errorf("ExchangeOrderID = %q, want ts-open-oid", got)
		}
	})

	t.Run("close", func(t *testing.T) {
		s := &StrategyState{
			ID:              "ts-momentum-es",
			Cash:            10000,
			Platform:        "topstep",
			Positions:       map[string]*Position{"ES": {Symbol: "ES", Quantity: 2, AvgCost: 4990, Side: "long", Multiplier: 50}},
			OptionPositions: make(map[string]*OptionPosition),
			TradeHistory:    []Trade{},
			RiskState:       RiskState{},
		}

		trades, err := ExecuteFuturesSignalWithFillFee(s, -1, "ES", 5000, spec, 2.5, 5, 2, 4.12, "ts-close-oid", 0, logger)
		if err != nil {
			t.Fatal(err)
		}
		if trades != 2 {
			t.Fatalf("trades = %d, want 2", trades)
		}
		if got := s.TradeHistory[0].ExchangeOrderID; got != "ts-close-oid" {
			t.Errorf("ExchangeOrderID = %q, want ts-close-oid", got)
		}
		if got := s.TradeHistory[1].ExchangeOrderID; got != "" {
			t.Errorf("open leg ExchangeOrderID = %q, want empty modeled metadata leg", got)
		}
	})
}

func TestExecutePerpsWithLeverageOpenShortFromFlat(t *testing.T) {
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

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionBoth, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
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

func TestExecutePerpsWithLeverageLegacyFlatNoShort(t *testing.T) {
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

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionLong, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
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

func TestExecutePerpsWithLeverage_DirectionShort_OpenShortFromFlat(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:              "hl-bear-eth",
		Cash:            1000,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionShort, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1 (open short under direction=short)", trades)
	}
	pos := s.Positions["ETH"]
	if pos == nil || pos.Side != "short" {
		t.Fatalf("expected short position, got %+v", pos)
	}
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].Side != "sell" {
		t.Errorf("expected one sell trade, got %+v", s.TradeHistory)
	}
}

func TestExecutePerpsWithLeverage_DirectionShort_BuyFromFlatSkipped(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:              "hl-bear-eth",
		Cash:            1000,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionShort, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
	}
	if trades != 0 {
		t.Errorf("trades = %d, want 0 (signal=1 from flat is a no-op under direction=short)", trades)
	}
	if len(s.Positions) != 0 {
		t.Error("no long should be opened under direction=short")
	}
	if s.Cash != 1000 {
		t.Errorf("cash = %g, want unchanged 1000", s.Cash)
	}
}

func TestExecutePerpsWithLeverage_DirectionShort_BuyClosesShort(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:       "hl-bear-eth",
		Cash:     1000,
		Platform: "hyperliquid",
		Type:     "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2100, Side: "short", Multiplier: 1, Leverage: 1, OwnerStrategyID: "hl-bear-eth"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionShort, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1 (close short, no flip)", trades)
	}
	if _, exists := s.Positions["ETH"]; exists {
		t.Error("position should be closed (no flip into long under direction=short)")
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len = %d, want 1 (close leg only)", len(s.TradeHistory))
	}
	if !s.TradeHistory[0].IsClose {
		t.Error("trade should be marked IsClose=true")
	}
}

func TestExecutePerpsWithLeverage_DirectionShort_OrphanLongNotAutoClosed(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:       "hl-bear-eth",
		Cash:     1000,
		Platform: "hyperliquid",
		Type:     "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 1, OwnerStrategyID: "hl-bear-eth"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}
	cashBefore := s.Cash

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionShort, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
	}
	if trades != 0 {
		t.Errorf("trades = %d, want 0 (orphan long must not be auto-closed)", trades)
	}
	if pos := s.Positions["ETH"]; pos == nil || pos.Side != "long" {
		t.Error("orphan long should remain in place")
	}
	if s.Cash != cashBefore {
		t.Errorf("cash should be unchanged on orphan-skip, got %g", s.Cash)
	}
}

func TestExecutePerpsWithLeverage_DirectionShort_AlreadyShortDedupes(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:       "hl-bear-eth",
		Cash:     1000,
		Platform: "hyperliquid",
		Type:     "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "short", Multiplier: 1, Leverage: 1, OwnerStrategyID: "hl-bear-eth"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}
	cashBefore := s.Cash

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionShort, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
	}
	if trades != 0 {
		t.Errorf("trades = %d, want 0 (already short dedupe)", trades)
	}
	if s.Cash != cashBefore {
		t.Errorf("cash should be unchanged on dedupe, got %g", s.Cash)
	}
}

func TestEffectiveDirection_PrecedenceAndFallback(t *testing.T) {
	cases := []struct {
		name      string
		sc        StrategyConfig
		wantDir   string
		wantLong  bool
		wantShort bool
	}{
		{"perps_explicit_long", StrategyConfig{Type: "perps", Direction: DirectionLong}, DirectionLong, true, false},
		{"perps_explicit_short", StrategyConfig{Type: "perps", Direction: DirectionShort}, DirectionShort, false, true},
		{"perps_explicit_both", StrategyConfig{Type: "perps", Direction: DirectionBoth}, DirectionBoth, true, true},
		{"perps_legacy_allowshorts_true", StrategyConfig{Type: "perps", AllowShorts: true}, DirectionBoth, true, true},
		{"perps_legacy_allowshorts_false", StrategyConfig{Type: "perps", AllowShorts: false}, DirectionLong, true, false},
		{"perps_unknown_falls_back", StrategyConfig{Type: "perps", Direction: "weird", AllowShorts: true}, DirectionBoth, true, true},
		{"non_perps_always_long", StrategyConfig{Type: "spot", Direction: DirectionShort}, DirectionLong, true, false},
		{"manual_explicit_short", StrategyConfig{Type: "manual", Direction: DirectionShort}, DirectionShort, false, true},
		{"manual_explicit_both", StrategyConfig{Type: "manual", Direction: DirectionBoth}, DirectionBoth, true, true},
		{"manual_legacy_allowshorts_true", StrategyConfig{Type: "manual", AllowShorts: true}, DirectionBoth, true, true},
		{"manual_legacy_allowshorts_false", StrategyConfig{Type: "manual"}, DirectionLong, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveDirection(tc.sc); got != tc.wantDir {
				t.Errorf("EffectiveDirection = %q, want %q", got, tc.wantDir)
			}
			if got := PerpsAllowsLong(tc.sc); got != tc.wantLong {
				t.Errorf("PerpsAllowsLong = %v, want %v", got, tc.wantLong)
			}
			if got := PerpsAllowsShort(tc.sc); got != tc.wantShort {
				t.Errorf("PerpsAllowsShort = %v, want %v", got, tc.wantShort)
			}
		})
	}
}

func TestExecutePerpsWithLeverageLegacyCloseShortThenOpenLongUsesOpenFillFee(t *testing.T) {
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	s := &StrategyState{
		ID:       "hl-legacy-eth",
		Cash:     1000,
		Platform: "hyperliquid",
		Type:     "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2100, Side: "short", Multiplier: 1, Leverage: 1, OwnerStrategyID: "hl-legacy-eth"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{},
	}

	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.3, "legacy-open-oid", 0.42, DirectionLong, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
	}
	if trades != 2 {
		t.Fatalf("trades = %d, want 2 (legacy close short + open long)", trades)
	}
	if len(s.TradeHistory) != 2 {
		t.Fatalf("TradeHistory len = %d, want 2", len(s.TradeHistory))
	}

	closeLeg, openLeg := s.TradeHistory[0], s.TradeHistory[1]
	modeledCloseFee := CalculatePlatformSpotFee("hyperliquid", 0.5*2000)
	if closeLeg.ExchangeOrderID != "" || math.Abs(closeLeg.ExchangeFee-modeledCloseFee) > 1e-9 || closeLeg.FeeSource != FeeSourceModeled {
		t.Errorf("legacy close leg = oid %q fee %g src %q, want no-OID modeled fee %g",
			closeLeg.ExchangeOrderID, closeLeg.ExchangeFee, closeLeg.FeeSource, modeledCloseFee)
	}
	if openLeg.ExchangeOrderID != "legacy-open-oid" || openLeg.ExchangeFee != 0.42 || openLeg.FeeSource != FeeSourceUserFills {
		t.Errorf("legacy open leg exchange metadata = oid %q fee %g, want oid legacy-open-oid fee 0.42",
			openLeg.ExchangeOrderID, openLeg.ExchangeFee)
	}
	pos := s.Positions["ETH"]
	if pos == nil || pos.Side != "long" || pos.Quantity != 0.3 {
		t.Fatalf("position after legacy close/open = %+v, want long qty 0.3", pos)
	}

	wantCash := 1000.0 + (0.5*(2100-2000) - modeledCloseFee) - 0.42
	if math.Abs(s.Cash-wantCash) > 1e-9 {
		t.Errorf("cash = %.9f, want %.9f (close modeled fee + open real fill fee)", s.Cash, wantCash)
	}
}

func TestExecutePerpsWithLeverageFlipLongToShort(t *testing.T) {
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

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 1.0, "live-flip-oid", 0.5, DirectionBoth, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
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
	if closeLeg.ExchangeOrderID != "live-flip-oid" || math.Abs(closeLeg.ExchangeFee-0.25) > 1e-9 {
		t.Errorf("close leg exchange metadata = oid %q fee %g, want oid live-flip-oid fee 0.25",
			closeLeg.ExchangeOrderID, closeLeg.ExchangeFee)
	}
	if openLeg.ExchangeOrderID != "live-flip-oid" || math.Abs(openLeg.ExchangeFee-0.25) > 1e-9 || openLeg.FeeSource != FeeSourceUserFills {
		t.Errorf("open leg exchange metadata = oid %q fee %g, want shared OID with fee 0.25",
			openLeg.ExchangeOrderID, openLeg.ExchangeFee)
	}
	wantCash := 1000.0 + 50 - 0.5
	if math.Abs(s.Cash-wantCash) > 1e-9 {
		t.Errorf("cash = %.9f, want %.9f (single real fee apportioned across flip legs)", s.Cash, wantCash)
	}
}

func TestExecutePerpsWithLeverageAlreadyShortIsInertNoOp(t *testing.T) {
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

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 1950, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionBoth, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
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

func TestPerpsLiveOrderSize_FlipIncludesCloseLeg(t *testing.T) {
	cases := []struct {
		name      string
		signal    int
		posQty    float64
		avgCost   float64
		posSide   string
		direction string
		wantSize  float64
		wantOK    bool
	}{
		{"long_from_flat", 1, 0, 0, "", DirectionLong, 0.5, true},
		{"short_from_flat_allowed_both", -1, 0, 0, "", DirectionBoth, 0.5, true},
		{"short_from_flat_short_only", -1, 0, 0, "", DirectionShort, 0.5, true},
		{"close_long_legacy", -1, 0.3, 2000, "long", DirectionLong, 0.3, true},
		{"close_short_short_only", 1, 0.4, 2000, "short", DirectionShort, 0.4, true},
		{"flip_long_to_short_flat_pnl", -1, 0.5, 2000, "long", DirectionBoth, 1.0, true},
		{"flip_short_to_long_flat_pnl", 1, 0.5, 2000, "short", DirectionBoth, 1.0, true},
		{"buy_vs_short_legacy_not_flip", 1, 0.5, 2000, "short", DirectionLong, 0.5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, ok, reason := perpsLiveOrderSize(tc.signal, 2000, 1000, tc.posQty, tc.avgCost, PerpsSizing{SizingLeverage: 1.0, ExchangeLeverage: 1.0}, tc.posSide, tc.direction, 0)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (reason=%q), want %v", ok, reason, tc.wantOK)
			}
			if ok && size != tc.wantSize {
				t.Errorf("size = %g, want %g", size, tc.wantSize)
			}
		})
	}
}

func TestPerpsLiveOrderSize_FlipLongToShortExceedsCloseOnly(t *testing.T) {
	posQty := 0.5
	size, ok, _ := perpsLiveOrderSize(-1, 2000, 1000, posQty, 2000, PerpsSizing{SizingLeverage: 1.0, ExchangeLeverage: 1.0}, "long", DirectionBoth, 0)
	if !ok {
		t.Fatal("expected ok")
	}
	if size <= posQty {
		t.Errorf("flip size = %g, must exceed close-only posQty (%g) for a net-flip", size, posQty)
	}
}

func TestPerpsLiveOrderSize_FlipSizesAgainstPostCloseMargin(t *testing.T) {
	size, ok, reason := perpsLiveOrderSize(-1, 1900, 1000, 0.5, 2000, PerpsSizing{SizingLeverage: 5.0, ExchangeLeverage: 5.0}, "long", DirectionBoth, 0)
	if !ok {
		t.Fatalf("expected ok, got reason=%q", reason)
	}
	wantSize := 0.5 + float64(1000-50)*5/1900
	if diff := size - wantSize; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("size = %g, want %g (post-close margin sizing)", size, wantSize)
	}
	preCloseSize := 0.5 + float64(1000)*5/1900
	if size >= preCloseSize {
		t.Errorf("size = %g must be < pre-close-sized %g to avoid over-sizing on a losing flip", size, preCloseSize)
	}
}

func TestPerpsLiveOrderSize_SharedWalletPoolUsesReleasedMargin(t *testing.T) {
	sizing := PerpsSizing{
		ExchangeLeverage:    5,
		MarginPerTradeUSD:   100,
		SharedWalletPool:    true,
		ReleasableMarginUSD: 200,
	}
	size, ok, reason := perpsLiveOrderSize(
		-1, 2000, 0, 0.5, 2200, sizing, "long", DirectionBoth, 0,
	)
	if !ok || reason != "" {
		t.Fatalf("pooled flip rejected: ok=%v reason=%q", ok, reason)
	}
	if math.Abs(size-0.75) > 1e-9 {
		t.Fatalf("pooled flip size=%v, want 0.75", size)
	}
}

func TestPerpsLiveOrderSize_SharedWalletPoolUnknownBalanceFlipClosesOnly(t *testing.T) {
	sizing := PerpsSizing{
		ExchangeLeverage:  5,
		MarginPerTradeUSD: 100,
		SharedWalletPool:  true,
	}
	size, ok, reason := perpsLiveOrderSize(
		-1, 2000, 0, 0.5, 2200, sizing, "long", DirectionBoth, 0,
	)
	if !ok || reason != "" {
		t.Fatalf("unknown-balance pooled flip rejected instead of closing: ok=%v reason=%q", ok, reason)
	}
	if size != 0.5 {
		t.Fatalf("unknown-balance pooled flip size=%v, want close-only 0.5", size)
	}
}

func TestWithSharedWalletPoolSizingReleasesMarginOnlyWithKnownBalance(t *testing.T) {
	sc := StrategyConfig{sharedWalletPoolBudget: true}
	base := PerpsSizing{ExchangeLeverage: 5}
	unknown := withSharedWalletPoolSizing(sc, base, 0.5, 2000, 2200, 4, false)
	if !unknown.SharedWalletPool || unknown.ReleasableMarginUSD != 0 {
		t.Fatalf("unknown balance must not expose released margin: %+v", unknown)
	}
	known := withSharedWalletPoolSizing(sc, base, 0.5, 2000, 2200, 4, true)
	if !known.SharedWalletPool || known.ReleasableMarginUSD != 275 {
		t.Fatalf("known balance released margin=%v, want 275", known.ReleasableMarginUSD)
	}
	winner := withSharedWalletPoolSizing(sc, base, 0.5, 2400, 2200, 4, true)
	if winner.ReleasableMarginUSD != 300 {
		t.Fatalf("winning position released margin=%v, want 300", winner.ReleasableMarginUSD)
	}
	legacy := withSharedWalletPoolSizing(sc, base, 0.5, 2000, 2200, 0, true)
	if legacy.ReleasableMarginUSD != 220 {
		t.Fatalf("legacy unstamped leverage must fall back to config: got %v, want 220", legacy.ReleasableMarginUSD)
	}
}

func TestPerpsLiveOrderSize_CatastrophicFlipDegradesToCloseOnly(t *testing.T) {
	size, ok, reason := perpsLiveOrderSize(-1, 500, 100, 1.0, 2000, PerpsSizing{SizingLeverage: 1.0, ExchangeLeverage: 1.0}, "long", DirectionBoth, 0)
	if !ok {
		t.Fatalf("expected ok (should degrade to close-only, not abort); reason=%q", reason)
	}
	if size != 1.0 {
		t.Errorf("size = %g, want 1.0 (close-only fallback when post-close margin is negative)", size)
	}
}

func TestPerpsLiveOrderSize_FlipProfitableFlipUsesRealizedGain(t *testing.T) {
	size, ok, _ := perpsLiveOrderSize(1, 1900, 1000, 0.5, 2000, PerpsSizing{SizingLeverage: 5.0, ExchangeLeverage: 5.0}, "short", DirectionBoth, 0)
	if !ok {
		t.Fatal("expected ok")
	}
	wantSize := 0.5 + float64(1000+50)*5/1900
	if diff := size - wantSize; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("size = %g, want %g (post-close margin sizing, profit added)", size, wantSize)
	}
	preCloseSize := 0.5 + float64(1000)*5/1900
	if size <= preCloseSize {
		t.Errorf("profitable flip size = %g must exceed pre-close-sized %g", size, preCloseSize)
	}
}

func TestPerpsOpenNotional(t *testing.T) {
	cases := []struct {
		name              string
		cash              float64
		sizingLeverage    float64
		exchangeLev       float64
		marginPerTradeUSD float64
		want              float64
	}{
		{"legacy_1x", 1000, 1, 1, 0, 1000},
		{"legacy_5x", 1000, 5, 5, 0, 5000},
		{"issue_518_legacy_pain", 560, 0.1, 20, 0, 56},
		{"issue_518_fixed", 560, 0.1, 20, 56, 1120},
		{"margin_clamps_to_cash", 100, 1, 5, 200, 500},
		{"margin_overrides_sizing_leverage", 1000, 0.5, 10, 100, 1000},
		{"negative_cash_returns_zero", -100, 1, 1, 0, 0},
		{"zero_cash_returns_zero", 0, 5, 5, 50, 0},
		{"margin_zero_exchange_leverage_fallback", 1000, 1, 0, 100, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PerpsOpenNotional(tc.cash, tc.sizingLeverage, tc.exchangeLev, tc.marginPerTradeUSD)
			if got != tc.want {
				t.Errorf("PerpsOpenNotional(cash=%g, sl=%g, el=%g, m=%g) = %g, want %g",
					tc.cash, tc.sizingLeverage, tc.exchangeLev, tc.marginPerTradeUSD, got, tc.want)
			}
		})
	}
}

func TestExecutePerpsWithLeverageMarginPerTradeUSDOverridesSizingLeverage(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-test-eth",
		Cash:            560,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2257, PerpsSizing{SizingLeverage: 0.1, ExchangeLeverage: 20, MarginPerTradeUSD: 56}, 0, "", 0, DirectionLong, 0, logger)
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
	if pos.Quantity < 0.45 || pos.Quantity > 0.55 {
		t.Errorf("quantity = %g, want ~0.50 (margin_per_trade_usd=$56 × 20x leverage at $2257)", pos.Quantity)
	}
}

func TestPerpsLiveOrderSize_PartialCloseScalesPosQty(t *testing.T) {
	size, ok, reason := perpsLiveOrderSize(-1, 2100, 1000, 0.4, 2000, PerpsSizing{SizingLeverage: 1.0, ExchangeLeverage: 1.0}, "long", DirectionLong, 0.5)
	if !ok {
		t.Fatalf("expected ok, got reason=%q", reason)
	}
	if math.Abs(size-0.2) > 1e-9 {
		t.Errorf("size = %g, want 0.2 (0.4 * 0.5)", size)
	}
}

func TestPerpsLiveOrderSize_FullCloseFractionIsFullPosQty(t *testing.T) {
	for _, frac := range []float64{0, 1.0} {
		size, ok, _ := perpsLiveOrderSize(-1, 2100, 1000, 0.4, 2000, PerpsSizing{SizingLeverage: 1.0, ExchangeLeverage: 1.0}, "long", DirectionLong, frac)
		if !ok || math.Abs(size-0.4) > 1e-9 {
			t.Errorf("frac=%g: size = %g (ok=%v), want 0.4", frac, size, ok)
		}
	}
}

func TestExecutePerpsWithLeverage_PartialCloseLongPaperPreservesRemainder(t *testing.T) {
	pos := &Position{
		Symbol:          "ETH",
		TradePositionID: "etrip-1",
		Quantity:        0.4,
		InitialQuantity: 0.4,
		AvgCost:         2000,
		Side:            "long",
		Multiplier:      1,
		Leverage:        1,
	}
	s := &StrategyState{
		ID:              "hl-test-eth",
		Cash:            990,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       map[string]*Position{"ETH": pos},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2100, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionLong, 0.5, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	got, ok := s.Positions["ETH"]
	if !ok {
		t.Fatal("position should remain after partial close")
	}
	if math.Abs(got.Quantity-0.2) > 1e-9 {
		t.Errorf("Quantity = %g, want 0.2 (0.4 - 0.4*0.5)", got.Quantity)
	}
	if math.Abs(got.InitialQuantity-0.4) > 1e-9 {
		t.Errorf("InitialQuantity = %g, want 0.4 (must not rewrite)", got.InitialQuantity)
	}
	if got.AvgCost != 2000 {
		t.Errorf("AvgCost = %g, want 2000 (unchanged on partial close)", got.AvgCost)
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("trade history = %d, want 1", len(s.TradeHistory))
	}
	tr := s.TradeHistory[0]
	if math.Abs(tr.Quantity-0.2) > 1e-9 {
		t.Errorf("trade.Quantity = %g, want 0.2", tr.Quantity)
	}
	if !tr.IsClose {
		t.Error("trade.IsClose = false, want true")
	}
	if tr.PositionID != "etrip-1" {
		t.Errorf("trade.PositionID = %q, want %q (round-trip grouping)", tr.PositionID, "etrip-1")
	}
	wantGross := 0.2 * (tr.Price - 2000)
	wantFee := CalculatePlatformSpotFee("hyperliquid", 0.2*tr.Price)
	if !tr.PnLGross || math.Abs(tr.RealizedPnL-wantGross) > 1e-6 {
		t.Errorf("RealizedPnL = %g (gross=%v), want gross %g (partial slice only)", tr.RealizedPnL, tr.PnLGross, wantGross)
	}
	if math.Abs(tradeNetPnL(tr)-(wantGross-wantFee)) > 1e-6 {
		t.Errorf("tradeNetPnL = %g, want %g (slice PnL net of modeled fee)", tradeNetPnL(tr), wantGross-wantFee)
	}
}

func TestExecutePerpsWithLeverage_PartialCloseDoesNotFlipShortWithAllowShorts(t *testing.T) {
	pos := &Position{
		Symbol:          "ETH",
		TradePositionID: "etrip-1",
		Quantity:        0.4,
		InitialQuantity: 0.4,
		AvgCost:         2000,
		Side:            "long",
		Multiplier:      1,
		Leverage:        1,
	}
	s := &StrategyState{
		ID:              "hl-bidir",
		Cash:            990,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       map[string]*Position{"ETH": pos},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2100, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionBoth, 0.5, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1 (only the close leg, no flip-open)", trades)
	}
	got, ok := s.Positions["ETH"]
	if !ok {
		t.Fatal("position should remain after partial close (residual long)")
	}
	if got.Side != "long" {
		t.Errorf("Side = %q, want long (no flip)", got.Side)
	}
	if math.Abs(got.Quantity-0.2) > 1e-9 {
		t.Errorf("Quantity = %g, want 0.2", got.Quantity)
	}
}

func TestExecutePerpsWithLeverage_FullCloseFromRegistryDoesNotFlip(t *testing.T) {
	pos := &Position{
		Symbol:          "ETH",
		TradePositionID: "etrip-1",
		Quantity:        0.4,
		InitialQuantity: 0.4,
		AvgCost:         2000,
		Side:            "long",
		Multiplier:      1,
		Leverage:        1,
	}
	s := &StrategyState{
		ID:              "hl-bidir-full",
		Cash:            990,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       map[string]*Position{"ETH": pos},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2100, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionBoth, 1.0, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1 (close only, no fresh short)", trades)
	}
	if _, ok := s.Positions["ETH"]; ok {
		t.Error("position should be deleted after full close")
	}
}

func TestExecutePerpsWithLeverage_PartialCloseLongLiveUsesFillQty(t *testing.T) {
	pos := &Position{
		Symbol:          "ETH",
		TradePositionID: "etrip-live",
		Quantity:        0.4,
		InitialQuantity: 0.4,
		AvgCost:         2000,
		Side:            "long",
		Multiplier:      1,
		Leverage:        1,
	}
	s := &StrategyState{
		ID:              "hl-test-eth",
		Cash:            990,
		Platform:        "hyperliquid",
		Type:            "perps",
		Positions:       map[string]*Position{"ETH": pos},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	_, err := ExecutePerpsSignalWithLeverage(s, -1, "ETH", 2100, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.18, "live-oid", 0.05, DirectionLong, 0.5, logger)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Positions["ETH"]
	if got == nil {
		t.Fatal("position should remain")
	}
	if math.Abs(got.Quantity-(0.4-0.18)) > 1e-9 {
		t.Errorf("Quantity = %g, want 0.22 (0.4 - fillQty 0.18)", got.Quantity)
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("history = %d, want 1", len(s.TradeHistory))
	}
	if math.Abs(s.TradeHistory[0].Quantity-0.18) > 1e-9 {
		t.Errorf("trade.Quantity = %g, want 0.18 (live fillQty)", s.TradeHistory[0].Quantity)
	}
}

func TestPerpsLiveOrderSize_CloseActionUnderBothDoesNotFlipSize(t *testing.T) {
	cases := []struct {
		name     string
		signal   int
		posSide  string
		frac     float64
		wantSize float64
	}{
		{"partial close long", -1, "long", 0.5, 0.2},
		{"partial close short", 1, "short", 0.5, 0.2},
		{"full close long", -1, "long", 1.0, 0.4},
		{"full close short", 1, "short", 1.0, 0.4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, ok, reason := perpsLiveOrderSize(tc.signal, 2000, 1000, 0.4, 2000, PerpsSizing{SizingLeverage: 1.0, ExchangeLeverage: 1.0}, tc.posSide, DirectionBoth, tc.frac)
			if !ok {
				t.Fatalf("expected ok, got reason=%q", reason)
			}
			if math.Abs(size-tc.wantSize) > 1e-9 {
				t.Errorf("size = %g, want %g (close-only, must not flip-size posQty+newSize)", size, tc.wantSize)
			}
		})
	}
}

func TestPerpsLiveOrderSize_GenuineFlipStillFlipSizes(t *testing.T) {
	size, ok, _ := perpsLiveOrderSize(-1, 2000, 1000, 0.4, 2000, PerpsSizing{SizingLeverage: 1.0, ExchangeLeverage: 1.0}, "long", DirectionBoth, 0)
	if !ok {
		t.Fatal("expected ok")
	}
	if size <= 0.4 {
		t.Errorf("size = %g, want > 0.4 (flip = closeQty + newSize)", size)
	}
}

func TestExecutePerpsWithLeverage_PartialCloseCapsCloseQtyAtPosQuantity(t *testing.T) {
	cases := []struct {
		name    string
		signal  int
		posSide string
	}{
		{"close long oversized fill", -1, "long"},
		{"close short oversized fill", 1, "short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := &Position{
				Symbol:          "ETH",
				TradePositionID: "etrip-cap",
				Quantity:        0.597,
				InitialQuantity: 0.597,
				AvgCost:         2000,
				Side:            tc.posSide,
				Multiplier:      1,
				Leverage:        1,
			}
			s := &StrategyState{
				ID:              "hl-cap",
				Cash:            990,
				Platform:        "hyperliquid",
				Type:            "perps",
				Positions:       map[string]*Position{"ETH": pos},
				OptionPositions: make(map[string]*OptionPosition),
				TradeHistory:    []Trade{},
				RiskState:       RiskState{PeakValue: 1000},
			}
			lm, _ := NewLogManager("")
			logger, _ := lm.GetStrategyLogger("test")
			defer logger.Close()

			_, err := ExecutePerpsSignalWithLeverage(s, tc.signal, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 1.192, "oid", 0, DirectionBoth, 0.5, logger)
			if err != nil {
				t.Fatal(err)
			}
			got := s.Positions["ETH"]
			if got == nil {
				t.Fatal("position should remain (zero or residual), not be dropped")
			}
			if got.Quantity < 0 {
				t.Errorf("Quantity = %g, must never be negative (cap closeQty at pos.Quantity)", got.Quantity)
			}
			if got.Quantity > 1e-9 {
				t.Errorf("Quantity = %g, want ~0 (closeQty capped at 0.597 fully closes)", got.Quantity)
			}
			if len(s.TradeHistory) != 1 {
				t.Fatalf("history = %d, want 1", len(s.TradeHistory))
			}
			if math.Abs(s.TradeHistory[0].Quantity-0.597) > 1e-9 {
				t.Errorf("trade.Quantity = %g, want 0.597 (capped close leg, not 1.192)", s.TradeHistory[0].Quantity)
			}
		})
	}
}

func TestPerpsCloseActionSuppressesNewSL(t *testing.T) {
	cases := []struct {
		name                    string
		signal                  int
		posSide                 string
		allowsLong, allowsShort bool
		frac                    float64
		want                    bool
	}{
		{"full close long under both", -1, "long", true, true, 1.0, true},
		{"full close short under both", 1, "short", true, true, 1.0, true},
		{"flip long-to-short under both", -1, "long", true, true, 0, false},
		{"flip short-to-long under both", 1, "short", true, true, 0, false},
		{"partial close under both not pureClose", -1, "long", true, true, 0.5, false},
		{"long-only sell on long", -1, "long", true, false, 0, true},
		{"short-only buy on short", 1, "short", false, true, 0, true},
		{"both-direction long open arms SL", 1, "", true, true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := perpsCloseActionSuppressesNewSL(tc.signal, tc.posSide, tc.allowsLong, tc.allowsShort, tc.frac)
			if got != tc.want {
				t.Errorf("perpsCloseActionSuppressesNewSL = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExecuteSpotWithFillFee_PartialCloseLongPaperPreservesRemainder(t *testing.T) {
	pos := &Position{
		Symbol:          "BTC/USDT",
		TradePositionID: "spot-trip",
		Quantity:        0.02,
		InitialQuantity: 0.02,
		AvgCost:         50000,
		Side:            "long",
	}
	s := &StrategyState{
		ID:              "test",
		Cash:            100,
		Platform:        "binanceus",
		Positions:       map[string]*Position{"BTC/USDT": pos},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 1000},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	trades, err := ExecuteSpotSignalWithFillFee(s, -1, "BTC/USDT", 55000, 0, 0, "", 0.5, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	got, ok := s.Positions["BTC/USDT"]
	if !ok {
		t.Fatal("position should remain after partial close")
	}
	if math.Abs(got.Quantity-0.01) > 1e-12 {
		t.Errorf("Quantity = %g, want 0.01 (0.02 * 0.5)", got.Quantity)
	}
	if math.Abs(got.InitialQuantity-0.02) > 1e-12 {
		t.Errorf("InitialQuantity = %g, want 0.02 (must not rewrite)", got.InitialQuantity)
	}
	if !s.TradeHistory[0].IsClose {
		t.Error("trade.IsClose = false, want true")
	}
	if s.TradeHistory[0].PositionID != "spot-trip" {
		t.Errorf("trade.PositionID = %q, want spot-trip (round-trip grouping)", s.TradeHistory[0].PositionID)
	}
}

func TestExecuteFuturesWithFillFee_PartialCloseRoundsDownContracts(t *testing.T) {
	pos := &Position{
		Symbol:          "ES",
		TradePositionID: "futures-trip",
		Quantity:        4,
		InitialQuantity: 4,
		AvgCost:         5000,
		Side:            "long",
		Multiplier:      50,
	}
	s := &StrategyState{
		ID:              "ts-es",
		Cash:            10000,
		Platform:        "topstep",
		Positions:       map[string]*Position{"ES": pos},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 10000},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	spec := ContractSpec{Multiplier: 50, Margin: 1000}
	trades, err := ExecuteFuturesSignalWithFillFee(s, -1, "ES", 5050, spec, 2.5, 5, 0, 0, "", 0.5, logger)
	if err != nil {
		t.Fatal(err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	got, ok := s.Positions["ES"]
	if !ok {
		t.Fatal("position should remain")
	}
	if int(got.Quantity) != 2 {
		t.Errorf("Quantity = %g, want 2", got.Quantity)
	}
	if int(s.TradeHistory[0].Quantity) != 2 {
		t.Errorf("trade.Quantity = %g, want 2", s.TradeHistory[0].Quantity)
	}
}

func TestExecuteFuturesWithFillFee_PartialCloseFractionTooSmallNoOps(t *testing.T) {
	pos := &Position{
		Symbol:          "ES",
		TradePositionID: "futures-trip-2",
		Quantity:        2,
		InitialQuantity: 2,
		AvgCost:         5000,
		Side:            "long",
		Multiplier:      50,
	}
	s := &StrategyState{
		ID:              "ts-es",
		Cash:            10000,
		Platform:        "topstep",
		Positions:       map[string]*Position{"ES": pos},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
		RiskState:       RiskState{PeakValue: 10000},
	}

	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	defer logger.Close()

	spec := ContractSpec{Multiplier: 50, Margin: 1000}
	trades, _ := ExecuteFuturesSignalWithFillFee(s, -1, "ES", 5050, spec, 2.5, 5, 0, 0, "", 0.25, logger)
	if trades != 0 {
		t.Errorf("trades = %d, want 0 (sub-contract fraction must no-op)", trades)
	}
	got := s.Positions["ES"]
	if got == nil || int(got.Quantity) != 2 {
		t.Errorf("position must remain at 2 contracts, got %v", got)
	}
}

func TestFormatStatusLine(t *testing.T) {
	cases := []struct {
		name   string
		regime string
		want   string
	}{
		{
			name:   "with regime",
			regime: "trending_up",
			want:   "Status: cash=$100.50 | positions=2 | value=$250.00 | trades=3 | regime=trending_up",
		},
		{
			name:   "empty regime renders dash",
			regime: "",
			want:   "Status: cash=$100.50 | positions=2 | value=$250.00 | trades=3 | regime=-",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatStatusLine(100.50, 2, 250.0, 3, tc.regime)
			if got != tc.want {
				t.Errorf("formatStatusLine = %q, want %q", got, tc.want)
			}
		})
	}
}
