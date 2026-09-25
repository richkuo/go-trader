package main

import (
	"math"
	"testing"
)

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
		wantKind  perpsLiveOrderKind
	}{
		{"long_from_flat", 1, 0, 0, "", DirectionLong, 0.5, true, perpsLiveOrderOpen},
		{"short_from_flat_allowed_both", -1, 0, 0, "", DirectionBoth, 0.5, true, perpsLiveOrderOpen},
		{"short_from_flat_short_only", -1, 0, 0, "", DirectionShort, 0.5, true, perpsLiveOrderOpen},
		{"close_long_legacy", -1, 0.3, 2000, "long", DirectionLong, 0.3, true, perpsLiveOrderClose},
		{"close_short_short_only", 1, 0.4, 2000, "short", DirectionShort, 0.4, true, perpsLiveOrderClose},
		{"flip_long_to_short_flat_pnl", -1, 0.5, 2000, "long", DirectionBoth, 1.0, true, perpsLiveOrderFlip},
		{"flip_short_to_long_flat_pnl", 1, 0.5, 2000, "short", DirectionBoth, 1.0, true, perpsLiveOrderFlip},
		{"buy_vs_short_legacy_not_flip", 1, 0.5, 2000, "short", DirectionLong, 0.5, true, perpsLiveOrderOpen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, kind, ok, reason := perpsLiveOrderSizeKind(tc.signal, 2000, 1000, tc.posQty, tc.avgCost, PerpsSizing{SizingLeverage: 1.0, ExchangeLeverage: 1.0}, tc.posSide, tc.direction, 0)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v (reason=%q), want %v", ok, reason, tc.wantOK)
			}
			if ok && size != tc.wantSize {
				t.Errorf("size = %g, want %g", size, tc.wantSize)
			}
			if ok && kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", kind, tc.wantKind)
			}
		})
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
