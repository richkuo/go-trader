package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func stubHLLiveCloser(errs map[string]error) (HyperliquidLiveCloser, *[]string) {
	closer, calls, _ := stubHLLiveCloserWithCancel(errs)
	return closer, calls
}

func stubHLLiveCloserWithCancel(errs map[string]error) (HyperliquidLiveCloser, *[]string, *map[string][]int64) {
	var calls []string
	cancels := make(map[string][]int64)
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		calls = append(calls, symbol)
		cancels[symbol] = append([]int64(nil), cancelStopLossOIDs...)
		if err, ok := errs[symbol]; ok && err != nil {
			return nil, err
		}
		return &HyperliquidCloseResult{
			Close:                   &HyperliquidClose{Symbol: symbol, Fill: &HyperliquidCloseFill{TotalSz: 1.0, AvgPx: 100}},
			Platform:                "hyperliquid",
			CancelStopLossSucceeded: firstPositiveStopLossOID(cancelStopLossOIDs) > 0,
		}, nil
	}
	return closer, &calls, &cancels
}

func stubHLStateFetcher(positions []HLPosition, err error) (HLStateFetcher, *int) {
	var calls int
	fetcher := func(addr string) ([]HLPosition, error) {
		calls++
		if err != nil {
			return nil, err
		}
		return positions, nil
	}
	return fetcher, &calls
}

func stubOKXLiveCloser(errs map[string]error) (OKXLiveCloser, *[]string) {
	var calls []string
	closer := func(symbol string, partialSz *float64) (*OKXCloseResult, error) {
		calls = append(calls, symbol)
		if err, ok := errs[symbol]; ok && err != nil {
			return nil, err
		}
		return &OKXCloseResult{
			Close:    &OKXClose{Symbol: symbol, Fill: &OKXCloseFill{TotalSz: 1.0, AvgPx: 100}},
			Platform: "okx",
		}, nil
	}
	return closer, &calls
}

func stubOKXPositionsFetcher(positions []OKXPosition, err error) (OKXPositionsFetcher, *int) {
	var calls int
	fetcher := func() ([]OKXPosition, error) {
		calls++
		if err != nil {
			return nil, err
		}
		return positions, nil
	}
	return fetcher, &calls
}

func defaultHLInputs(hlAddr string, fetched bool, positions []HLPosition,
	hlLive []StrategyConfig, reason string, timeout time.Duration,
	closer HyperliquidLiveCloser, fetcher HLStateFetcher) KillSwitchCloseInputs {
	return KillSwitchCloseInputs{
		HLAddr:          hlAddr,
		HLStateFetched:  fetched,
		HLPositions:     positions,
		HLLiveAll:       hlLive,
		HLCloser:        closer,
		HLFetcher:       fetcher,
		PortfolioReason: reason,
		CloseTimeout:    timeout,
	}
}

func TestPlanKillSwitchClose_HappyPath(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 3000}}
	closer, calls := stubHLLiveCloser(nil)
	fetcher, fetchCalls := stubHLStateFetcher(nil, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher))

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected ConfirmedFlat, got plan=%+v", plan)
	}
	if !plan.CanAutoResetWithoutOwner() {
		t.Fatal("expected happy-path confirmed-flat plan to allow no-owner auto-reset")
	}
	if len(plan.CloseReport.ClosedCoins) != 1 || plan.CloseReport.ClosedCoins[0] != "ETH" {
		t.Errorf("ClosedCoins = %v, want [ETH]", plan.CloseReport.ClosedCoins)
	}
	if *fetchCalls != 0 {
		t.Errorf("fetcher must not be called when state already fetched, got %d", *fetchCalls)
	}
	if len(*calls) != 1 || (*calls)[0] != "ETH" {
		t.Errorf("closer calls = %v, want [ETH]", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "PORTFOLIO KILL SWITCH") ||
		strings.Contains(plan.DiscordMessage, "LATCHED") {
		t.Errorf("expected success-shaped message, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "Virtual state cleared") {
		t.Errorf("expected 'Virtual state cleared' in message, got: %s", plan.DiscordMessage)
	}
	if got := formatKillSwitchAutoResetMessage(plan.DiscordMessage); !strings.Contains(got, "Kill switch auto-reset; trading will resume next cycle") ||
		strings.Contains(got, "Manual reset required") {
		t.Errorf("expected auto-reset message to replace manual-reset instruction, got: %s", got)
	}
}

func TestHyperliquidKillSwitchClose_UsesRealFillBeforeMark(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "BTC", Size: 1.0, EntryPrice: 50000}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close: &HyperliquidClose{
				Symbol: symbol,
				Fill:   &HyperliquidCloseFill{TotalSz: 1.0, AvgPx: 49000, Fee: 2.0},
			},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)
	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher))
	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed-flat plan, got %+v", plan)
	}

	s := &StrategyState{
		ID:       "hl-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Cash:     1000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 1.0, AvgCost: 50000, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	forceCloseKillSwitchPositions(s, hlLive[0], map[string]float64{"BTC": 48000}, plan.CloseReport.Fills, hlLive, nil, nil)

	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(s.TradeHistory))
	}
	trade := s.TradeHistory[0]
	if trade.Price != 49000 {
		t.Fatalf("Trade.Price = %.2f; want closer fill AvgPx 49000, not mark 48000", trade.Price)
	}
	if trade.Quantity != 1.0 {
		t.Errorf("Trade.Quantity = %.6f; want closer fill TotalSz 1.0", trade.Quantity)
	}
	if trade.ExchangeFee != 2.0 {
		t.Errorf("Trade.ExchangeFee = %.4f; want closer fill Fee 2.0", trade.ExchangeFee)
	}
	if len(s.ClosedPositions) != 1 {
		t.Fatalf("expected 1 closed position, got %d", len(s.ClosedPositions))
	}
	closed := s.ClosedPositions[0]
	if closed.ClosePrice != 49000 {
		t.Errorf("ClosedPosition.ClosePrice = %.2f; want fill AvgPx 49000", closed.ClosePrice)
	}
	wantPnL := -1002.0
	if math.Abs(closed.RealizedPnL-wantPnL) > 1e-9 {
		t.Errorf("ClosedPosition.RealizedPnL = %.4f; want %.4f", closed.RealizedPnL, wantPnL)
	}
	if math.Abs(s.Cash-(1000+wantPnL)) > 1e-9 {
		t.Errorf("Cash = %.4f; want %.4f", s.Cash, 1000+wantPnL)
	}
}

func TestHyperliquidKillSwitchClose_AlreadyFlatRecoversRecentUserFill(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.588, EntryPrice: 1754.10}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)
	var recoverCalls int
	in := defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher)
	in.HLNoFillRecoverer = func(since time.Time) (*HLUserFillsResult, error) {
		recoverCalls++
		if since.IsZero() {
			t.Fatal("recovery since timestamp must be populated")
		}
		return &HLUserFillsResult{
			ByOID: map[string]HLFillSummary{
				"474": {
					Coin:           "ETH",
					FirstTimeMS:    time.Now().Add(-time.Second).UnixMilli(),
					LastTimeMS:     time.Now().Add(-time.Second).UnixMilli(),
					Fee:            0.4389,
					ClosedPnLGross: -15.5232,
					Count:          1,
					Qty:            0.588,
					Px:             1727.70,
				},
			},
		}, nil
	}

	plan := planKillSwitchClose(in)
	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed-flat plan, got %+v", plan)
	}
	if recoverCalls != 1 {
		t.Fatalf("recoverCalls = %d, want 1", recoverCalls)
	}
	fill, ok := plan.CloseReport.Fills["ETH"]
	if !ok {
		t.Fatalf("missing recovered fill: %+v", plan.CloseReport.Fills)
	}
	if fill.OID != 474 || math.Abs(fill.TotalSz-0.588) > 1e-9 || math.Abs(fill.AvgPx-1727.70) > 1e-9 || math.Abs(fill.Fee-0.4389) > 1e-9 {
		t.Fatalf("recovered fill = %+v, want oid 474 qty 0.588 px 1727.70 fee 0.4389", fill)
	}
	if !strings.Contains(strings.Join(plan.LogLines, "\n"), "recovered already-flat fill for ETH") {
		t.Fatalf("missing recovery log line: %v", plan.LogLines)
	}

	s := &StrategyState{
		ID:       "hl-eth",
		Type:     "perps",
		Platform: "hyperliquid",
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.588, AvgCost: 1754.10, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	forceCloseKillSwitchPositions(s, hlLive[0], map[string]float64{"ETH": 1700}, plan.CloseReport.Fills, hlLive, nil, nil)
	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(s.TradeHistory))
	}
	trade := s.TradeHistory[0]
	if trade.ExchangeOrderID != "474" || trade.FeeSource != FeeSourceUserFills || !trade.PnLGross {
		t.Fatalf("trade fill metadata = oid %q fee_source %q gross %v, want 474/userfills/true", trade.ExchangeOrderID, trade.FeeSource, trade.PnLGross)
	}
	if math.Abs(trade.Price-1727.70) > 1e-9 || math.Abs(trade.ExchangeFee-0.4389) > 1e-9 {
		t.Fatalf("trade price/fee = %.6f/%.6f, want 1727.70/0.4389", trade.Price, trade.ExchangeFee)
	}
	if len(s.ClosedPositions) != 1 {
		t.Fatalf("expected closed position, got %d", len(s.ClosedPositions))
	}
	wantNetPnL := (1727.70-1754.10)*0.588 - 0.4389
	if math.Abs(s.ClosedPositions[0].RealizedPnL-wantNetPnL) > 1e-9 {
		t.Fatalf("closed pnl = %.6f, want %.6f", s.ClosedPositions[0].RealizedPnL, wantNetPnL)
	}
}

func TestHyperliquidKillSwitchClose_AlreadyFlatRecoversRecentUserFill_LowerKCoin(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-kpepe", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "kPEPE", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "kPEPE", Size: 12345.0, EntryPrice: 0.00012}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)
	var recoverCalls int
	in := defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher)
	in.HLNoFillRecoverer = func(since time.Time) (*HLUserFillsResult, error) {
		recoverCalls++
		if since.IsZero() {
			t.Fatal("recovery since timestamp must be populated")
		}
		return &HLUserFillsResult{
			ByOID: map[string]HLFillSummary{
				"98765": {
					Coin:           "kPEPE",
					FirstTimeMS:    time.Now().Add(-time.Second).UnixMilli(),
					LastTimeMS:     time.Now().Add(-time.Second).UnixMilli(),
					Fee:            0.123,
					ClosedPnLGross: -1.23,
					Count:          1,
					Qty:            12345.0,
					Px:             0.000119,
				},
			},
		}, nil
	}

	plan := planKillSwitchClose(in)
	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed-flat plan, got %+v", plan)
	}
	if recoverCalls != 1 {
		t.Fatalf("recoverCalls = %d, want 1", recoverCalls)
	}

	fill, ok := plan.CloseReport.Fills["kPEPE"]
	if !ok {
		t.Fatalf("missing recovered fill under raw 'kPEPE' key: %+v", plan.CloseReport.Fills)
	}
	if _, ok := plan.CloseReport.Fills["KPEPE"]; ok {
		t.Fatalf("must not also write normalized uppercase key: %+v", plan.CloseReport.Fills)
	}
	if fill.OID != 98765 || math.Abs(fill.TotalSz-12345.0) > 1e-9 || math.Abs(fill.AvgPx-0.000119) > 1e-9 || math.Abs(fill.Fee-0.123) > 1e-9 {
		t.Fatalf("recovered fill = %+v, want oid 98765 qty 12345 px 0.000119 fee 0.123", fill)
	}
	logs := strings.Join(plan.LogLines, "\n")
	if !strings.Contains(logs, "recovered already-flat fill for kPEPE") {
		t.Fatalf("missing recovery log line with raw casing: %v", plan.LogLines)
	}

	s := &StrategyState{
		ID:       "hl-kpepe",
		Type:     "perps",
		Platform: "hyperliquid",
		Cash:     1000,
		Positions: map[string]*Position{
			"kPEPE": {Symbol: "kPEPE", Quantity: 12345.0, AvgCost: 0.00012, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	forceCloseKillSwitchPositions(s, hlLive[0], map[string]float64{"kPEPE": 0.0001}, plan.CloseReport.Fills, hlLive, nil, nil)
	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(s.TradeHistory))
	}
	trade := s.TradeHistory[0]
	if trade.ExchangeOrderID != "98765" || trade.FeeSource != FeeSourceUserFills || !trade.PnLGross {
		t.Fatalf("trade fill metadata = oid %q fee_source %q gross %v, want 98765/userfills/true", trade.ExchangeOrderID, trade.FeeSource, trade.PnLGross)
	}
	if math.Abs(trade.Price-0.000119) > 1e-9 || math.Abs(trade.ExchangeFee-0.123) > 1e-9 {
		t.Fatalf("trade price/fee = %.9f/%.6f, want 0.000119/0.123", trade.Price, trade.ExchangeFee)
	}
}

func TestHyperliquidKillSwitchClose_AlreadyFlatSharedLowKCoinRecoversAndSplits(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5, CapitalPct: 2.0 / 3.0,
			Args: []string{"sma", "kPEPE", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Leverage: 5, CapitalPct: 1.0 / 3.0,
			Args: []string{"ema", "kPEPE", "1h", "--mode=live"}},
		{ID: "hl-zero", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "kPEPE", "1h", "--mode=live"}},
	}

	positions := []HLPosition{{Coin: "kPEPE", Size: 1.5, EntryPrice: 0.00012}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)
	var recoverCalls int
	in := defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher)
	const recOID = "77777"
	const recQty = 1.5
	const recPx = 0.000119
	const recFee = 0.003
	in.HLNoFillRecoverer = func(since time.Time) (*HLUserFillsResult, error) {
		recoverCalls++
		return &HLUserFillsResult{
			ByOID: map[string]HLFillSummary{
				recOID: {
					Coin:           "kPEPE",
					FirstTimeMS:    time.Now().Add(-2 * time.Second).UnixMilli(),
					LastTimeMS:     time.Now().Add(-time.Second).UnixMilli(),
					Fee:            recFee,
					ClosedPnLGross: -0.012,
					Count:          1,
					Qty:            recQty,
					Px:             recPx,
				},
			},
		}, nil
	}

	plan := planKillSwitchClose(in)
	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed-flat plan, got %+v", plan)
	}
	if recoverCalls != 1 {
		t.Fatalf("recoverCalls = %d, want 1", recoverCalls)
	}
	fill, ok := plan.CloseReport.Fills["kPEPE"]
	if !ok {
		t.Fatalf("missing recovered fill under raw 'kPEPE' key: %+v", plan.CloseReport.Fills)
	}
	if _, ok := plan.CloseReport.Fills["KPEPE"]; ok {
		t.Fatalf("must not also write normalized uppercase key on shared low-k: %+v", plan.CloseReport.Fills)
	}
	if fill.OID != 77777 || math.Abs(fill.TotalSz-recQty) > 1e-9 || math.Abs(fill.AvgPx-recPx) > 1e-9 || math.Abs(fill.Fee-recFee) > 1e-9 {
		t.Fatalf("recovered fill = %+v, want oid %s qty %.4f px %.6f fee %.6f", fill, recOID, recQty, recPx, recFee)
	}
	logs := strings.Join(plan.LogLines, "\n")
	if !strings.Contains(logs, "recovered already-flat fill for kPEPE") {
		t.Fatalf("missing recovery log with raw coin key: %v", plan.LogLines)
	}

	stateA := &StrategyState{
		ID: "hl-a", Type: "perps", Platform: "hyperliquid", Cash: 10000,
		Positions: map[string]*Position{
			"kPEPE": {Symbol: "kPEPE", Quantity: 1.0, AvgCost: 0.00012, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	stateB := &StrategyState{
		ID: "hl-b", Type: "perps", Platform: "hyperliquid", Cash: 5000,
		Positions: map[string]*Position{
			"kPEPE": {Symbol: "kPEPE", Quantity: 0.5, AvgCost: 0.00012, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	zeroState := &StrategyState{
		ID:        "hl-zero",
		Type:      "perps",
		Platform:  "hyperliquid",
		Cash:      1000,
		Positions: map[string]*Position{},
	}
	hlVirtualQty := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{
		"hl-a": stateA, "hl-b": stateB, "hl-zero": zeroState,
	}, hlLive)

	prices := map[string]float64{"kPEPE": 0.00005}
	forceCloseKillSwitchPositions(stateA, hlLive[0], prices, plan.CloseReport.Fills, hlLive, hlVirtualQty, nil)
	forceCloseKillSwitchPositions(stateB, hlLive[1], prices, plan.CloseReport.Fills, hlLive, hlVirtualQty, nil)
	forceCloseKillSwitchPositions(zeroState, hlLive[2], prices, plan.CloseReport.Fills, hlLive, hlVirtualQty, nil)

	if len(stateA.TradeHistory) != 1 {
		t.Fatalf("A: expected 1 trade from recovered fill, got %d", len(stateA.TradeHistory))
	}
	ta := stateA.TradeHistory[0]
	if ta.ExchangeOrderID != recOID || ta.FeeSource != FeeSourceUserFills || !ta.PnLGross {
		t.Fatalf("A trade metadata = oid %q src %q gross %v, want %s/UserFills/true", ta.ExchangeOrderID, ta.FeeSource, ta.PnLGross, recOID)
	}
	if math.Abs(ta.Quantity-1.0) > 1e-9 || math.Abs(ta.Price-recPx) > 1e-12 || math.Abs(ta.ExchangeFee-0.002) > 1e-12 {
		t.Fatalf("A qty/px/fee = %.6f/%.6f/%.6f, want 1.0/%.6f/0.002", ta.Quantity, ta.Price, ta.ExchangeFee, recPx)
	}

	if len(stateB.TradeHistory) != 1 {
		t.Fatalf("B: expected 1 trade from recovered fill, got %d", len(stateB.TradeHistory))
	}
	tb := stateB.TradeHistory[0]
	if tb.ExchangeOrderID != recOID || tb.FeeSource != FeeSourceUserFills || !tb.PnLGross {
		t.Fatalf("B trade metadata = oid %q src %q gross %v, want %s/UserFills/true", tb.ExchangeOrderID, tb.FeeSource, tb.PnLGross, recOID)
	}
	if math.Abs(tb.Quantity-0.5) > 1e-9 || math.Abs(tb.Price-recPx) > 1e-12 || math.Abs(tb.ExchangeFee-0.001) > 1e-12 {
		t.Fatalf("B qty/px/fee = %.6f/%.6f/%.6f, want 0.5/%.6f/0.001", tb.Quantity, tb.Price, tb.ExchangeFee, recPx)
	}

	if math.Abs((ta.Quantity+tb.Quantity)-recQty) > 1e-9 || math.Abs((ta.ExchangeFee+tb.ExchangeFee)-recFee) > 1e-12 {
		t.Fatalf("peer shares sum to q=%.6f f=%.6f; want %.6f / %.6f", ta.Quantity+tb.Quantity, ta.ExchangeFee+tb.ExchangeFee, recQty, recFee)
	}

	if len(zeroState.TradeHistory) != 0 {
		t.Fatalf("zero peer must not book a fill share trade, got %d", len(zeroState.TradeHistory))
	}
}

func TestHyperliquidKillSwitchClose_AlreadyFlatSharedLowKCoinAmbiguousFallsBack(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "kPEPE", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"ema", "kPEPE", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "kPEPE", Size: 2.0, EntryPrice: 0.00011}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)
	in := defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher)
	in.HLNoFillRecoverer = func(since time.Time) (*HLUserFillsResult, error) {
		return &HLUserFillsResult{
			ByOID: map[string]HLFillSummary{

				"111": {Coin: "kPEPE", Fee: 0.1, Qty: 2.0, Px: 0.00010, ClosedPnLGross: -0.5, LastTimeMS: time.Now().UnixMilli()},
				"222": {Coin: "kPEPE", Fee: 0.2, Qty: 2.0, Px: 0.00009, ClosedPnLGross: -0.6, LastTimeMS: time.Now().UnixMilli()},
			},
		}, nil
	}

	plan := planKillSwitchClose(in)
	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed-flat plan, got %+v", plan)
	}
	if _, ok := plan.CloseReport.Fills["kPEPE"]; ok {
		t.Fatalf("ambiguous must not inject a fill under raw key: %+v", plan.CloseReport.Fills)
	}
	if _, ok := plan.CloseReport.Fills["KPEPE"]; ok {
		t.Fatalf("ambiguous must not inject under normalized key either")
	}
	if !strings.Contains(strings.Join(plan.LogLines, "\n"), "multiple userFills candidates") {
		t.Fatalf("missing ambiguity warning: %v", plan.LogLines)
	}

	stateA := &StrategyState{
		ID: "hl-a", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{"kPEPE": {Symbol: "kPEPE", Quantity: 1.2, AvgCost: 0.00011, Side: "long", Multiplier: 1, Leverage: 5}},
	}
	stateB := &StrategyState{
		ID: "hl-b", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{"kPEPE": {Symbol: "kPEPE", Quantity: 0.8, AvgCost: 0.00011, Side: "long", Multiplier: 1, Leverage: 5}},
	}
	hlVirtual := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{"hl-a": stateA, "hl-b": stateB}, hlLive)
	mark := 0.00005
	forceCloseKillSwitchPositions(stateA, hlLive[0], map[string]float64{"kPEPE": mark}, plan.CloseReport.Fills, hlLive, hlVirtual, nil)
	forceCloseKillSwitchPositions(stateB, hlLive[1], map[string]float64{"kPEPE": mark}, plan.CloseReport.Fills, hlLive, hlVirtual, nil)

	for _, st := range []*StrategyState{stateA, stateB} {
		for _, tr := range st.TradeHistory {
			if tr.ExchangeOrderID == "111" || tr.ExchangeOrderID == "222" {
				t.Fatalf("peer booked against ambiguous OID %s (should have fallen back)", tr.ExchangeOrderID)
			}

			if tr.Price == 0.00010 || tr.Price == 0.00009 {
				t.Fatalf("peer booked at an ambiguous fill px %.6f instead of mark", tr.Price)
			}
		}
	}
}

func TestHyperliquidKillSwitchClose_AlreadyFlatAmbiguousUserFillFallsBack(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 1.0, EntryPrice: 2000}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)
	in := defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher)
	in.HLNoFillRecoverer = func(since time.Time) (*HLUserFillsResult, error) {
		return &HLUserFillsResult{
			ByOID: map[string]HLFillSummary{
				"100": {Coin: "ETH", Fee: 0.4, Qty: 1.0, Px: 1990, ClosedPnLGross: -10, LastTimeMS: time.Now().UnixMilli()},
				"101": {Coin: "ETH", Fee: 0.5, Qty: 1.0, Px: 1989, ClosedPnLGross: -11, LastTimeMS: time.Now().UnixMilli()},
			},
		}, nil
	}

	plan := planKillSwitchClose(in)
	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed-flat plan, got %+v", plan)
	}
	if _, ok := plan.CloseReport.Fills["ETH"]; ok {
		t.Fatalf("ambiguous userFills must not inject a fill: %+v", plan.CloseReport.Fills)
	}
	if !strings.Contains(strings.Join(plan.LogLines, "\n"), "multiple userFills candidates") {
		t.Fatalf("missing ambiguity warning: %v", plan.LogLines)
	}
}

func TestHyperliquidKillSwitchClose_AlreadyFlatRecoveryRejectsOpeningFill(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 1.0, EntryPrice: 2000}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)
	in := defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher)
	in.HLNoFillRecoverer = func(since time.Time) (*HLUserFillsResult, error) {
		return &HLUserFillsResult{
			ByOID: map[string]HLFillSummary{
				"300": {Coin: "ETH", Fee: 0.4, Qty: 1.0, Px: 1990, ClosedPnLGross: 0, LastTimeMS: time.Now().UnixMilli()},
			},
		}, nil
	}

	plan := planKillSwitchClose(in)
	if _, ok := plan.CloseReport.Fills["ETH"]; ok {
		t.Fatalf("opening fill must never be adopted as the close: %+v", plan.CloseReport.Fills)
	}
	if !strings.Contains(strings.Join(plan.LogLines, "\n"), "no userFills match") {
		t.Fatalf("expected fail-closed warning, got: %v", plan.LogLines)
	}
}

func TestHyperliquidKillSwitchClose_AlreadyFlatRecoveryIgnoresFillBeforeSince(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 1.0, EntryPrice: 2000}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)
	var gotSince time.Time
	in := defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher)
	in.HLNoFillRecoverer = func(since time.Time) (*HLUserFillsResult, error) {
		gotSince = since
		return &HLUserFillsResult{
			ByOID: map[string]HLFillSummary{
				"400": {Coin: "ETH", Fee: 0.4, Qty: 1.0, Px: 1990, ClosedPnLGross: -10,
					LastTimeMS: since.Add(-time.Minute).UnixMilli()},
			},
		}, nil
	}

	plan := planKillSwitchClose(in)
	if gotSince.IsZero() {
		t.Fatal("recovery since timestamp must be populated")
	}
	if _, ok := plan.CloseReport.Fills["ETH"]; ok {
		t.Fatalf("fill before the since bound must not be adopted: %+v", plan.CloseReport.Fills)
	}
}

func TestHyperliquidKillSwitchClose_SharedCoinSplitsFillByVirtualQuantity(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5, CapitalPct: 0.25,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Leverage: 5, CapitalPct: 0.75,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	plan := KillSwitchClosePlan{
		OnChainConfirmedFlat: true,
		CloseReport: HyperliquidLiveCloseReport{
			Fills: map[string]HyperliquidCloseFill{
				"ETH": {TotalSz: 2.0, AvgPx: 3000, Fee: 4.0},
			},
		},
	}
	s := &StrategyState{
		ID:       "hl-a",
		Type:     "perps",
		Platform: "hyperliquid",
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1.5, AvgCost: 3100, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	peerState := &StrategyState{
		ID:       "hl-b",
		Type:     "perps",
		Platform: "hyperliquid",
		Cash:     3000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3100, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	hlVirtualQty := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{
		"hl-a": s,
		"hl-b": peerState,
	}, hlLive)

	forceCloseKillSwitchPositions(s, hlLive[0], map[string]float64{"ETH": 2800}, plan.CloseReport.Fills, hlLive, hlVirtualQty, nil)

	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(s.TradeHistory))
	}
	trade := s.TradeHistory[0]
	if math.Abs(trade.Quantity-1.5) > 1e-9 {
		t.Errorf("Trade.Quantity = %.6f; want 1.5 virtual-quantity share of 2.0 fill", trade.Quantity)
	}
	if trade.Price != 3000 {
		t.Errorf("Trade.Price = %.2f; want real fill AvgPx 3000", trade.Price)
	}
	if trade.ExchangeFee != 3.0 {
		t.Errorf("Trade.ExchangeFee = %.4f; want virtual-quantity fill Fee 3.0", trade.ExchangeFee)
	}
	if len(s.ClosedPositions) != 1 {
		t.Fatalf("expected 1 closed position, got %d", len(s.ClosedPositions))
	}
	wantPnL := -153.0
	if math.Abs(s.ClosedPositions[0].RealizedPnL-wantPnL) > 1e-9 {
		t.Errorf("RealizedPnL = %.4f; want %.4f", s.ClosedPositions[0].RealizedPnL, wantPnL)
	}
}

func TestHyperliquidKillSwitchClose_SharedCoinPeersSumToReport(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5, CapitalPct: 0.25,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Leverage: 5, CapitalPct: 0.75,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	const totalSz, totalFee, avgPx = 2.0, 4.0, 3000.0
	fills := map[string]HyperliquidCloseFill{
		"ETH": {TotalSz: totalSz, AvgPx: avgPx, Fee: totalFee},
	}
	stateA := &StrategyState{
		ID: "hl-a", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1.5, AvgCost: 3100, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	stateB := &StrategyState{
		ID: "hl-b", Type: "perps", Platform: "hyperliquid", Cash: 3000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3100, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	prices := map[string]float64{"ETH": 2800}
	hlVirtualQty := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{
		"hl-a": stateA,
		"hl-b": stateB,
	}, hlLive)

	forceCloseKillSwitchPositions(stateA, hlLive[0], prices, fills, hlLive, hlVirtualQty, nil)
	forceCloseKillSwitchPositions(stateB, hlLive[1], prices, fills, hlLive, hlVirtualQty, nil)

	if len(stateA.TradeHistory) != 1 || len(stateB.TradeHistory) != 1 {
		t.Fatalf("expected 1 trade per peer, got %d / %d",
			len(stateA.TradeHistory), len(stateB.TradeHistory))
	}
	tA, tB := stateA.TradeHistory[0], stateB.TradeHistory[0]
	if math.Abs(tA.Quantity-1.5) > 1e-9 || math.Abs(tB.Quantity-0.5) > 1e-9 {
		t.Errorf("peer fill quantities = %.6f / %.6f; want 1.500000 / 0.500000", tA.Quantity, tB.Quantity)
	}
	if math.Abs((tA.Quantity+tB.Quantity)-totalSz) > 1e-9 {
		t.Errorf("peer fill quantities sum to %.6f; want %.6f", tA.Quantity+tB.Quantity, totalSz)
	}
	if math.Abs((tA.ExchangeFee+tB.ExchangeFee)-totalFee) > 1e-9 {
		t.Errorf("peer fees sum to %.6f; want %.6f", tA.ExchangeFee+tB.ExchangeFee, totalFee)
	}
	if tA.Price != avgPx || tB.Price != avgPx {
		t.Errorf("peer fill prices = %.2f / %.2f; want %.2f for both", tA.Price, tB.Price, avgPx)
	}
}

func TestPlanKillSwitchClose_CloseError(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5}}
	closer, _ := stubHLLiveCloser(map[string]error{"ETH": fmt.Errorf("hl rate limited")})
	fetcher, fetchCalls := stubHLStateFetcher(positions, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher))

	if *fetchCalls != 1 {
		t.Fatalf("fetcher should be called once to verify the close error, got %d", *fetchCalls)
	}
	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat on close error — kill switch would clear virtual state while on-chain is still live")
	}
	if got, ok := plan.CloseReport.Errors["ETH"]; !ok || got == nil {
		t.Errorf("expected ETH error in report, got %v", plan.CloseReport.Errors)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected LATCHED message on close error, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "Virtual state preserved") {
		t.Errorf("expected 'Virtual state preserved' in latched message, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "hl rate limited") {
		t.Errorf("error detail missing from message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_CloseErrorVerifiedFlat(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5}}
	closer, _ := stubHLLiveCloser(map[string]error{"ETH": fmt.Errorf("post-submit disconnect")})
	fetcher, fetchCalls := stubHLStateFetcher(nil, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher))

	if *fetchCalls != 1 {
		t.Fatalf("fetcher should be called once to verify the close error, got %d", *fetchCalls)
	}
	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected ConfirmedFlat after verification fetch proved ETH flat, got plan=%+v", plan)
	}
	if len(plan.CloseReport.Errors) != 0 {
		t.Fatalf("expected verified-flat close error to be cleared, got %v", plan.CloseReport.Errors)
	}
	if len(plan.CloseReport.AlreadyFlat) != 1 || plan.CloseReport.AlreadyFlat[0] != "ETH" {
		t.Errorf("AlreadyFlat = %v, want [ETH]", plan.CloseReport.AlreadyFlat)
	}
	if !strings.Contains(strings.Join(plan.LogLines, "\n"), "verified flat after close error: [ETH]") {
		t.Errorf("expected verification log line, got %v", plan.LogLines)
	}
	if strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected success-shaped message after verified-flat close error, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_CloseErrorVerificationFetchFailure(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5}}
	closer, _ := stubHLLiveCloser(map[string]error{"ETH": fmt.Errorf("post-submit disconnect")})
	fetcher, fetchCalls := stubHLStateFetcher(nil, fmt.Errorf("hl 503"))

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher))

	if *fetchCalls != 1 {
		t.Fatalf("fetcher should be called once to verify the close error, got %d", *fetchCalls)
	}
	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat when the verification fetch also fails")
	}
	if got, ok := plan.CloseReport.Errors["ETH"]; !ok || got == nil {
		t.Errorf("expected ETH error to remain in report, got %v", plan.CloseReport.Errors)
	}
	if !strings.Contains(strings.Join(plan.LogLines, "\n"), "unable to verify HL state after close error: hl 503") {
		t.Errorf("expected verification fetch error log line, got %v", plan.LogLines)
	}
}

func TestPlanKillSwitchClose_OpportunisticFetch(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5}}
	closer, calls := stubHLLiveCloser(nil)
	fetcher, fetchCalls := stubHLStateFetcher(positions, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", false, nil, hlLive,
		"drawdown reason", time.Second, closer, fetcher))

	if *fetchCalls != 1 {
		t.Fatalf("fetcher should be called once, got %d", *fetchCalls)
	}
	if !plan.OnChainConfirmedFlat {
		t.Errorf("expected ConfirmedFlat after successful fetch + close, got plan=%+v", plan)
	}
	if len(*calls) != 1 || (*calls)[0] != "ETH" {
		t.Errorf("closer calls = %v, want [ETH] (fetched positions should feed the closer)", *calls)
	}
}

func TestPlanKillSwitchClose_FetchFailure(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
	}
	closer, calls := stubHLLiveCloser(nil)
	fetcher, fetchCalls := stubHLStateFetcher(nil, fmt.Errorf("hl 503"))

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", false, nil, hlLive,
		"drawdown reason", time.Second, closer, fetcher))

	if *fetchCalls != 1 {
		t.Fatalf("fetcher should be called once on fetch failure, got %d", *fetchCalls)
	}
	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat on fetch failure — cannot verify on-chain state")
	}
	if len(*calls) != 0 {
		t.Errorf("closer must not be invoked when fetch failed, got calls=%v", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected LATCHED message on fetch failure, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "Could not fetch") {
		t.Errorf("expected fetch-failure detail in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_UnconfiguredPositionBlocksReset(t *testing.T) {
	positions := []HLPosition{{Coin: "ETH", Size: 0.517}}
	closer, calls := stubHLLiveCloser(nil)
	fetcher, _ := stubHLStateFetcher(positions, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", false, nil,
		[]StrategyConfig{},
		"drawdown reason", time.Second, closer, fetcher))

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat — on-chain position exists for unconfigured coin")
	}
	if len(plan.Unconfigured) != 1 || plan.Unconfigured[0].Coin != "ETH" {
		t.Errorf("expected Unconfigured=[ETH], got %v", plan.Unconfigured)
	}
	if len(*calls) != 0 {
		t.Errorf("closer must not be invoked for unconfigured coin, got %v", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "manual intervention required") {
		t.Errorf("message must call out manual intervention, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "ETH szi=0.517000") {
		t.Errorf("message must include coin+szi detail, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_NoHLConfigured(t *testing.T) {
	closer, calls := stubHLLiveCloser(nil)
	fetcher, fetchCalls := stubHLStateFetcher(nil, nil)

	plan := planKillSwitchClose(defaultHLInputs("", false, nil, nil,
		"drawdown reason", time.Second, closer, fetcher))

	if !plan.OnChainConfirmedFlat {
		t.Fatal("expected ConfirmedFlat when HL is not configured at all")
	}
	if *fetchCalls != 0 {
		t.Errorf("fetcher must not be called when hlAddr is empty, got %d", *fetchCalls)
	}
	if len(*calls) != 0 {
		t.Errorf("closer must not be called, got %v", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "HL not configured") {
		t.Errorf("expected 'HL not configured' in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_DeterministicErrorOrder(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-sol", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "SOL", "1h", "--mode=live"}},
		{ID: "hl-doge", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "DOGE", "1h", "--mode=live"}},
	}
	positions := []HLPosition{
		{Coin: "BTC", Size: 0.01}, {Coin: "ETH", Size: 0.1},
		{Coin: "SOL", Size: 1.0}, {Coin: "DOGE", Size: 100},
	}
	errs := map[string]error{
		"BTC": fmt.Errorf("err"), "ETH": fmt.Errorf("err"),
		"SOL": fmt.Errorf("err"), "DOGE": fmt.Errorf("err"),
	}
	var prev string
	for i := 0; i < 10; i++ {
		closer, _ := stubHLLiveCloser(errs)
		fetcher, _ := stubHLStateFetcher(positions, nil)
		plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, hlLive,
			"reason", time.Second, closer, fetcher))
		if prev != "" && plan.DiscordMessage != prev {
			t.Fatalf("message should be deterministic across calls\niter %d: %s\nprev: %s", i, plan.DiscordMessage, prev)
		}
		prev = plan.DiscordMessage
	}
	btcPos := strings.Index(prev, "BTC:")
	dogePos := strings.Index(prev, "DOGE:")
	ethPos := strings.Index(prev, "ETH:")
	solPos := strings.Index(prev, "SOL:")
	if !(btcPos < dogePos && dogePos < ethPos && ethPos < solPos) {
		t.Errorf("expected alphabetical ordering BTC < DOGE < ETH < SOL, got positions btc=%d doge=%d eth=%d sol=%d in: %s",
			btcPos, dogePos, ethPos, solPos, prev)
	}
}

func TestPlanKillSwitchClose_ZeroInputsAreSafe(t *testing.T) {
	closer, _ := stubHLLiveCloser(nil)
	fetcher, _ := stubHLStateFetcher(nil, nil)
	plan := planKillSwitchClose(defaultHLInputs("", false, nil, nil, "", time.Second, closer, fetcher))
	if !plan.OnChainConfirmedFlat {
		t.Errorf("zero inputs should yield ConfirmedFlat=true, got %+v", plan)
	}
}

func TestPlanKillSwitchClose_OKXHappyPath(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, EntryPrice: 42000, Side: "long"}}
	closer, calls := stubOKXLiveCloser(nil)
	fetcher, fetchCalls := stubOKXPositionsFetcher(positions, nil)

	in := KillSwitchCloseInputs{
		OKXLiveAllPerps: okxLive,
		OKXCloser:       closer,
		OKXFetcher:      fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	}
	plan := planKillSwitchClose(in)

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected ConfirmedFlat, got plan=%+v", plan)
	}
	if *fetchCalls != 1 {
		t.Errorf("OKX fetcher should be called exactly once, got %d", *fetchCalls)
	}
	if len(*calls) != 1 || (*calls)[0] != "BTC" {
		t.Errorf("closer calls = %v, want [BTC]", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "OKX closes: [BTC]") {
		t.Errorf("expected OKX closes in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_OKXCloseError(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	closer, _ := stubOKXLiveCloser(map[string]error{"BTC": fmt.Errorf("okx rate limited")})
	fetcher, _ := stubOKXPositionsFetcher(positions, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		OKXLiveAllPerps: okxLive,
		OKXCloser:       closer,
		OKXFetcher:      fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat on OKX close error — would clear virtual state while on-chain is live")
	}
	if got, ok := plan.OKXCloseReport.Errors["BTC"]; !ok || got == nil {
		t.Errorf("expected BTC error in OKX report, got %v", plan.OKXCloseReport.Errors)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "okx rate limited") {
		t.Errorf("expected OKX error detail in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_OKXFetchFailure(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	closer, calls := stubOKXLiveCloser(nil)
	fetcher, _ := stubOKXPositionsFetcher(nil, fmt.Errorf("okx auth failed"))

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		OKXLiveAllPerps: okxLive,
		OKXCloser:       closer,
		OKXFetcher:      fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat on OKX fetch failure")
	}
	if len(*calls) != 0 {
		t.Errorf("closer must not be invoked when fetch failed, got %v", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected LATCHED message on fetch failure, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_OKXSpotSurfacesGap(t *testing.T) {
	okxSpot := []StrategyConfig{
		{ID: "okx-sma-btc-spot", Platform: "okx", Type: "spot",
			Args: []string{"sma", "BTC", "1h", "--mode=live", "--inst-type=spot"}},
	}
	closer, _ := stubOKXLiveCloser(nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		OKXLiveAllSpot:  okxSpot,
		OKXCloser:       closer,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if !plan.OnChainConfirmedFlat {
		t.Errorf("spot-only presence must not block ConfirmedFlat, got plan=%+v", plan)
	}
	if !plan.OKXSpotPresent {
		t.Error("expected OKXSpotPresent=true")
	}
	if plan.CanAutoResetWithoutOwner() {
		t.Error("OKX spot operator-required gap must suppress no-owner auto-reset")
	}
	if !strings.Contains(plan.DiscordMessage, "OKX spot") {
		t.Errorf("expected spot gap note in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_HLAndOKXHappyPath(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	hlPos := []HLPosition{{Coin: "ETH", Size: 0.5}}
	okxPos := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	hlCloser, hlCalls := stubHLLiveCloser(nil)
	hlFetcher, _ := stubHLStateFetcher(nil, nil)
	okxCloser, okxCalls := stubOKXLiveCloser(nil)
	okxFetcher, _ := stubOKXPositionsFetcher(okxPos, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		HLAddr:          "0xaddr",
		HLStateFetched:  true,
		HLPositions:     hlPos,
		HLLiveAll:       hlLive,
		HLCloser:        hlCloser,
		HLFetcher:       hlFetcher,
		OKXLiveAllPerps: okxLive,
		OKXCloser:       okxCloser,
		OKXFetcher:      okxFetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected ConfirmedFlat when both platforms succeed, got plan=%+v", plan)
	}
	if len(*hlCalls) != 1 || (*hlCalls)[0] != "ETH" {
		t.Errorf("HL closer calls = %v, want [ETH]", *hlCalls)
	}
	if len(*okxCalls) != 1 || (*okxCalls)[0] != "BTC" {
		t.Errorf("OKX closer calls = %v, want [BTC]", *okxCalls)
	}
	if !strings.Contains(plan.DiscordMessage, "HL closes: [ETH]") {
		t.Errorf("expected HL closes in message, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "OKX closes: [BTC]") {
		t.Errorf("expected OKX closes in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_HLSuccessOKXFailureStillLatches(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	hlPos := []HLPosition{{Coin: "ETH", Size: 0.5}}
	okxPos := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	hlCloser, _ := stubHLLiveCloser(nil)
	hlFetcher, _ := stubHLStateFetcher(nil, nil)
	okxCloser, _ := stubOKXLiveCloser(map[string]error{"BTC": fmt.Errorf("okx err")})
	okxFetcher, _ := stubOKXPositionsFetcher(okxPos, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		HLAddr:          "0xaddr",
		HLStateFetched:  true,
		HLPositions:     hlPos,
		HLLiveAll:       hlLive,
		HLCloser:        hlCloser,
		HLFetcher:       hlFetcher,
		OKXLiveAllPerps: okxLive,
		OKXCloser:       okxCloser,
		OKXFetcher:      okxFetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("OKX failure must latch the switch even when HL succeeded")
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "okx err") {
		t.Errorf("OKX error missing from message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_OKXUnconfiguredBlocksReset(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{
		{Coin: "BTC", Size: 0.01, Side: "long"},
		{Coin: "SOL", Size: 100, Side: "long"},
	}
	closer, calls := stubOKXLiveCloser(nil)
	fetcher, _ := stubOKXPositionsFetcher(positions, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		OKXLiveAllPerps: okxLive,
		OKXCloser:       closer,
		OKXFetcher:      fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat — unconfigured SOL position is still on-chain")
	}
	if len(plan.OKXUnconfigured) != 1 || plan.OKXUnconfigured[0].Coin != "SOL" {
		t.Errorf("expected OKXUnconfigured=[SOL], got %v", plan.OKXUnconfigured)
	}

	if len(*calls) != 1 || (*calls)[0] != "BTC" {
		t.Errorf("closer calls = %v, want [BTC]", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "manual intervention required") {
		t.Errorf("expected manual intervention note, got: %s", plan.DiscordMessage)
	}
}

func stubRHLiveCloser(errs map[string]error) (RobinhoodLiveCloser, *[]string) {
	var calls []string
	closer := func(symbol string) (*RobinhoodCloseResult, error) {
		calls = append(calls, symbol)
		if err, ok := errs[symbol]; ok && err != nil {
			return nil, err
		}
		return &RobinhoodCloseResult{
			Close:    &RobinhoodClose{Symbol: symbol, Fill: &RobinhoodCloseFill{TotalSz: 1.0, AvgPx: 100}},
			Platform: "robinhood",
		}, nil
	}
	return closer, &calls
}

func stubRHPositionsFetcher(positions []RobinhoodPosition, err error) (RobinhoodPositionsFetcher, *int) {
	var calls int
	fetcher := func() ([]RobinhoodPosition, error) {
		calls++
		if err != nil {
			return nil, err
		}
		return positions, nil
	}
	return fetcher, &calls
}

func TestPlanKillSwitchClose_RobinhoodHappyPath(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{{Coin: "BTC", Size: 0.01, AvgPrice: 42000}}
	closer, calls := stubRHLiveCloser(nil)
	fetcher, fetchCalls := stubRHPositionsFetcher(positions, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		RHLiveCrypto:    rhLive,
		RHCloser:        closer,
		RHFetcher:       fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected ConfirmedFlat, got plan=%+v", plan)
	}
	if *fetchCalls != 1 {
		t.Errorf("Robinhood fetcher should be called exactly once, got %d", *fetchCalls)
	}
	if len(*calls) != 1 || (*calls)[0] != "BTC" {
		t.Errorf("closer calls = %v, want [BTC]", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "Robinhood closes: [BTC]") {
		t.Errorf("expected Robinhood closes in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_RobinhoodCloseError(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{{Coin: "BTC", Size: 0.01}}
	closer, _ := stubRHLiveCloser(map[string]error{"BTC": fmt.Errorf("rh rate limited")})
	fetcher, _ := stubRHPositionsFetcher(positions, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		RHLiveCrypto:    rhLive,
		RHCloser:        closer,
		RHFetcher:       fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat on Robinhood close error — would clear virtual state while live is still active")
	}
	if got, ok := plan.RHCloseReport.Errors["BTC"]; !ok || got == nil {
		t.Errorf("expected BTC error in Robinhood report, got %v", plan.RHCloseReport.Errors)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "rh rate limited") {
		t.Errorf("expected Robinhood error detail in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_RobinhoodFetchFailure(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	closer, calls := stubRHLiveCloser(nil)
	fetcher, _ := stubRHPositionsFetcher(nil, fmt.Errorf("rh auth failed"))

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		RHLiveCrypto:    rhLive,
		RHCloser:        closer,
		RHFetcher:       fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat on Robinhood fetch failure")
	}
	if len(*calls) != 0 {
		t.Errorf("closer must not be invoked when fetch failed, got %v", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_RobinhoodOptionsSurfacesGap(t *testing.T) {
	rhOptions := []StrategyConfig{
		{ID: "rh-ccall-spy", Platform: "robinhood", Type: "options",
			Args: []string{"covered_call", "SPY", "1d", "--mode=live"}},
	}
	closer, _ := stubRHLiveCloser(nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		RHLiveOptions:   rhOptions,
		RHCloser:        closer,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if !plan.OnChainConfirmedFlat {
		t.Errorf("options-only presence must not block ConfirmedFlat, got plan=%+v", plan)
	}
	if !plan.RHOptionsPresent {
		t.Error("expected RHOptionsPresent=true")
	}
	if plan.CanAutoResetWithoutOwner() {
		t.Error("Robinhood options operator-required gap must suppress no-owner auto-reset")
	}
	if !strings.Contains(plan.DiscordMessage, "Robinhood options") {
		t.Errorf("expected options gap note in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_RobinhoodUnconfiguredBlocksReset(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{
		{Coin: "BTC", Size: 0.01},
		{Coin: "DOGE", Size: 100},
	}
	closer, calls := stubRHLiveCloser(nil)
	fetcher, _ := stubRHPositionsFetcher(positions, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		RHLiveCrypto:    rhLive,
		RHCloser:        closer,
		RHFetcher:       fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat — unconfigured DOGE balance is still live")
	}
	if len(plan.RHUnconfigured) != 1 || plan.RHUnconfigured[0].Coin != "DOGE" {
		t.Errorf("expected RHUnconfigured=[DOGE], got %v", plan.RHUnconfigured)
	}

	if len(*calls) != 1 || (*calls)[0] != "BTC" {
		t.Errorf("closer calls = %v, want [BTC]", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "manual intervention required") {
		t.Errorf("expected manual intervention note, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_HLAndOKXAndRobinhoodHappyPath(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	rhLive := []StrategyConfig{
		{ID: "rh-sma-sol", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "SOL", "1h", "--mode=live"}},
	}
	hlPos := []HLPosition{{Coin: "ETH", Size: 0.5}}
	okxPos := []OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}
	rhPos := []RobinhoodPosition{{Coin: "SOL", Size: 2.5}}

	hlCloser, _ := stubHLLiveCloser(nil)
	hlFetcher, _ := stubHLStateFetcher(nil, nil)
	okxCloser, _ := stubOKXLiveCloser(nil)
	okxFetcher, _ := stubOKXPositionsFetcher(okxPos, nil)
	rhCloser, rhCalls := stubRHLiveCloser(nil)
	rhFetcher, _ := stubRHPositionsFetcher(rhPos, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		HLAddr:          "0xaddr",
		HLStateFetched:  true,
		HLPositions:     hlPos,
		HLLiveAll:       hlLive,
		HLCloser:        hlCloser,
		HLFetcher:       hlFetcher,
		OKXLiveAllPerps: okxLive,
		OKXCloser:       okxCloser,
		OKXFetcher:      okxFetcher,
		RHLiveCrypto:    rhLive,
		RHCloser:        rhCloser,
		RHFetcher:       rhFetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected ConfirmedFlat, got plan=%+v", plan)
	}
	if len(*rhCalls) != 1 || (*rhCalls)[0] != "SOL" {
		t.Errorf("Robinhood closer calls = %v, want [SOL]", *rhCalls)
	}
	if !strings.Contains(plan.DiscordMessage, "HL closes: [ETH]") {
		t.Errorf("expected HL closes in message, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "OKX closes: [BTC]") {
		t.Errorf("expected OKX closes in message, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "Robinhood closes: [SOL]") {
		t.Errorf("expected Robinhood closes in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_RobinhoodFailureStillLatchesAcrossPlatforms(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	hlPos := []HLPosition{{Coin: "ETH", Size: 0.5}}
	rhPos := []RobinhoodPosition{{Coin: "BTC", Size: 0.01}}

	hlCloser, _ := stubHLLiveCloser(nil)
	hlFetcher, _ := stubHLStateFetcher(nil, nil)
	rhCloser, _ := stubRHLiveCloser(map[string]error{"BTC": fmt.Errorf("rh err")})
	rhFetcher, _ := stubRHPositionsFetcher(rhPos, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		HLAddr:          "0xaddr",
		HLStateFetched:  true,
		HLPositions:     hlPos,
		HLLiveAll:       hlLive,
		HLCloser:        hlCloser,
		HLFetcher:       hlFetcher,
		RHLiveCrypto:    rhLive,
		RHCloser:        rhCloser,
		RHFetcher:       rhFetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("Robinhood failure must latch the switch even when HL succeeded")
	}
	if !strings.Contains(plan.DiscordMessage, "rh err") {
		t.Errorf("Robinhood error missing from message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_RobinhoodDeterministicErrorOrder(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-btc", Platform: "robinhood", Type: "spot", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "rh-eth", Platform: "robinhood", Type: "spot", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "rh-doge", Platform: "robinhood", Type: "spot", Args: []string{"sma", "DOGE", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{
		{Coin: "BTC", Size: 0.01}, {Coin: "ETH", Size: 0.1}, {Coin: "DOGE", Size: 100},
	}
	errs := map[string]error{
		"BTC": fmt.Errorf("err"), "ETH": fmt.Errorf("err"), "DOGE": fmt.Errorf("err"),
	}
	var prev string
	for i := 0; i < 10; i++ {
		closer, _ := stubRHLiveCloser(errs)
		fetcher, _ := stubRHPositionsFetcher(positions, nil)
		plan := planKillSwitchClose(KillSwitchCloseInputs{
			RHLiveCrypto:    rhLive,
			RHCloser:        closer,
			RHFetcher:       fetcher,
			PortfolioReason: "reason",
			CloseTimeout:    time.Second,
		})
		if prev != "" && plan.DiscordMessage != prev {
			t.Fatalf("message should be deterministic, iter %d:\n%s\nprev:\n%s", i, plan.DiscordMessage, prev)
		}
		prev = plan.DiscordMessage
	}
	btcPos := strings.Index(prev, "BTC:")
	dogePos := strings.Index(prev, "DOGE:")
	ethPos := strings.Index(prev, "ETH:")
	if !(btcPos < dogePos && dogePos < ethPos) {
		t.Errorf("expected alphabetical BTC < DOGE < ETH in: %s", prev)
	}
}

func TestPlanKillSwitchClose_OKXDeterministicErrorOrder(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "okx-eth", Platform: "okx", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "okx-sol", Platform: "okx", Type: "perps", Args: []string{"sma", "SOL", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{
		{Coin: "BTC", Size: 0.01, Side: "long"},
		{Coin: "ETH", Size: 0.1, Side: "long"},
		{Coin: "SOL", Size: 1.0, Side: "long"},
	}
	errs := map[string]error{
		"BTC": fmt.Errorf("err"), "ETH": fmt.Errorf("err"), "SOL": fmt.Errorf("err"),
	}
	var prev string
	for i := 0; i < 10; i++ {
		closer, _ := stubOKXLiveCloser(errs)
		fetcher, _ := stubOKXPositionsFetcher(positions, nil)
		plan := planKillSwitchClose(KillSwitchCloseInputs{
			OKXLiveAllPerps: okxLive,
			OKXCloser:       closer,
			OKXFetcher:      fetcher,
			PortfolioReason: "reason",
			CloseTimeout:    time.Second,
		})
		if prev != "" && plan.DiscordMessage != prev {
			t.Fatalf("message should be deterministic, iter %d:\n%s\nprev:\n%s", i, plan.DiscordMessage, prev)
		}
		prev = plan.DiscordMessage
	}
	btcPos := strings.Index(prev, "BTC:")
	ethPos := strings.Index(prev, "ETH:")
	solPos := strings.Index(prev, "SOL:")
	if !(btcPos < ethPos && ethPos < solPos) {
		t.Errorf("expected alphabetical BTC < ETH < SOL in: %s", prev)
	}
}

func stubTSLiveCloser(errs map[string]error) (TopStepLiveCloser, *[]string) {
	var calls []string
	closer := func(symbol string) (*TopStepCloseResult, error) {
		calls = append(calls, symbol)
		if err, ok := errs[symbol]; ok && err != nil {
			return nil, err
		}
		return &TopStepCloseResult{
			Close:    &TopStepClose{Symbol: symbol, Fill: &TopStepCloseFill{TotalContracts: 1, AvgPx: 5000}},
			Platform: "topstep",
		}, nil
	}
	return closer, &calls
}

func stubTSPositionsFetcher(positions []TopStepPosition, err error) (TopStepPositionsFetcher, *int) {
	var calls int
	fetcher := func() ([]TopStepPosition, error) {
		calls++
		if err != nil {
			return nil, err
		}
		return positions, nil
	}
	return fetcher, &calls
}

func TestPlanKillSwitchClose_TopStepHappyPath(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: 2, AvgPrice: 5000, Side: "long"}}
	closer, calls := stubTSLiveCloser(nil)
	fetcher, fetchCalls := stubTSPositionsFetcher(positions, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		TSLiveAll:       tsLive,
		TSCloser:        closer,
		TSFetcher:       fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected ConfirmedFlat, got plan=%+v", plan)
	}
	if *fetchCalls != 1 {
		t.Errorf("TopStep fetcher should be called exactly once, got %d", *fetchCalls)
	}
	if len(*calls) != 1 || (*calls)[0] != "ES" {
		t.Errorf("closer calls = %v, want [ES]", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "TopStep closes: [ES]") {
		t.Errorf("expected TopStep closes in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_TopStepCloseError(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{{Coin: "ES", Size: 2}}
	closer, _ := stubTSLiveCloser(map[string]error{"ES": fmt.Errorf("market closed")})
	fetcher, _ := stubTSPositionsFetcher(positions, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		TSLiveAll:       tsLive,
		TSCloser:        closer,
		TSFetcher:       fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat on TopStep close error — would clear virtual state while CME is still active")
	}
	if got, ok := plan.TSCloseReport.Errors["ES"]; !ok || got == nil {
		t.Errorf("expected ES error in TopStep report, got %v", plan.TSCloseReport.Errors)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
	}
	if !strings.Contains(plan.DiscordMessage, "market closed") {
		t.Errorf("expected TopStep error detail in message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_TopStepFetchFailure(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	closer, calls := stubTSLiveCloser(nil)
	fetcher, _ := stubTSPositionsFetcher(nil, fmt.Errorf("topstepx 401"))

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		TSLiveAll:       tsLive,
		TSCloser:        closer,
		TSFetcher:       fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat on TopStep fetch failure")
	}
	if len(*calls) != 0 {
		t.Errorf("closer must not be invoked when fetch failed, got %v", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
		t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_TopStepUnconfiguredBlocksReset(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	positions := []TopStepPosition{
		{Coin: "ES", Size: 2},
		{Coin: "NQ", Size: 1},
	}
	closer, calls := stubTSLiveCloser(nil)
	fetcher, _ := stubTSPositionsFetcher(positions, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		TSLiveAll:       tsLive,
		TSCloser:        closer,
		TSFetcher:       fetcher,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat — unconfigured NQ position is still live")
	}
	if len(plan.TSUnconfigured) != 1 || plan.TSUnconfigured[0].Coin != "NQ" {
		t.Errorf("expected TSUnconfigured=[NQ], got %v", plan.TSUnconfigured)
	}
	if len(*calls) != 1 || (*calls)[0] != "ES" {
		t.Errorf("closer calls = %v, want [ES]", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "manual intervention required") {
		t.Errorf("expected manual intervention note, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_TopStepFailureStillLatchesAcrossPlatforms(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-mom-btc", Platform: "hyperliquid", Type: "perps",
			Args: []string{"momentum", "BTC", "1h", "--mode=live"}},
	}
	hlPositions := []HLPosition{{Coin: "BTC", Size: 0.5}}
	hlCloser, _ := stubHLLiveCloser(nil)

	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	tsPositions := []TopStepPosition{{Coin: "ES", Size: 2}}
	tsCloser, _ := stubTSLiveCloser(map[string]error{"ES": fmt.Errorf("venue down")})
	tsFetcher, _ := stubTSPositionsFetcher(tsPositions, nil)

	plan := planKillSwitchClose(KillSwitchCloseInputs{
		HLAddr:          "0xabc",
		HLStateFetched:  true,
		HLPositions:     hlPositions,
		HLLiveAll:       hlLive,
		HLCloser:        hlCloser,
		TSLiveAll:       tsLive,
		TSCloser:        tsCloser,
		TSFetcher:       tsFetcher,
		PortfolioReason: "reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat when TopStep fails even though HL succeeded")
	}
	if len(plan.CloseReport.ClosedCoins) != 1 || plan.CloseReport.ClosedCoins[0] != "BTC" {
		t.Errorf("HL close should still run: got %v", plan.CloseReport.ClosedCoins)
	}
}

func TestPlanKillSwitchClose_PlatformBudgetOverrides(t *testing.T) {
	in := KillSwitchCloseInputs{
		CloseTimeout:    90 * time.Second,
		HLCloseTimeout:  10 * time.Second,
		OKXCloseTimeout: 0,
		RHCloseTimeout:  150 * time.Second,
		TSCloseTimeout:  0,
	}
	if got := in.platformCloseBudget(in.HLCloseTimeout); got != 10*time.Second {
		t.Errorf("HL budget = %v, want 10s override", got)
	}
	if got := in.platformCloseBudget(in.OKXCloseTimeout); got != 90*time.Second {
		t.Errorf("OKX budget = %v, want 90s fallback", got)
	}
	if got := in.platformCloseBudget(in.RHCloseTimeout); got != 150*time.Second {
		t.Errorf("RH budget = %v, want 150s override", got)
	}
	if got := in.platformCloseBudget(in.TSCloseTimeout); got != 90*time.Second {
		t.Errorf("TS budget = %v, want 90s fallback", got)
	}
}

func TestPlanKillSwitchClose_HLFetcherUnwiredLatches(t *testing.T) {
	plan := planKillSwitchClose(KillSwitchCloseInputs{
		HLAddr:          "0xabc",
		HLStateFetched:  false,
		HLFetcher:       nil,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat when HLFetcher unwired with HLAddr set")
	}
	found := false
	for _, line := range plan.LogLines {
		if strings.Contains(line, "HLFetcher unwired") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected log line mentioning HLFetcher unwired, got: %v", plan.LogLines)
	}
}

func TestPlanKillSwitchClose_OKXFetcherUnwiredLatches(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	plan := planKillSwitchClose(KillSwitchCloseInputs{
		OKXLiveAllPerps: okxLive,
		OKXFetcher:      nil,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat when OKXFetcher unwired with strategies configured")
	}
	found := false
	for _, line := range plan.LogLines {
		if strings.Contains(line, "OKXFetcher unwired") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected log line mentioning OKXFetcher unwired, got: %v", plan.LogLines)
	}
}

func TestPlanKillSwitchClose_RHFetcherUnwiredLatches(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	plan := planKillSwitchClose(KillSwitchCloseInputs{
		RHLiveCrypto:    rhLive,
		RHFetcher:       nil,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat when RHFetcher unwired with strategies configured")
	}
	found := false
	for _, line := range plan.LogLines {
		if strings.Contains(line, "RHFetcher unwired") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected log line mentioning RHFetcher unwired, got: %v", plan.LogLines)
	}
}

func TestPlanKillSwitchClose_TSFetcherUnwiredLatches(t *testing.T) {
	tsLive := []StrategyConfig{
		{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
			Args: []string{"momentum", "ES", "1h", "--mode=live"}},
	}
	plan := planKillSwitchClose(KillSwitchCloseInputs{
		TSLiveAll:       tsLive,
		TSFetcher:       nil,
		PortfolioReason: "drawdown reason",
		CloseTimeout:    time.Second,
	})

	if plan.OnChainConfirmedFlat {
		t.Fatal("expected NOT ConfirmedFlat when TSFetcher unwired with strategies configured")
	}
	found := false
	for _, line := range plan.LogLines {
		if strings.Contains(line, "TSFetcher unwired") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected log line mentioning TSFetcher unwired, got: %v", plan.LogLines)
	}
}

func TestKillSwitchInstanceLabel_UsesConfigDirBasename(t *testing.T) {
	got := killSwitchInstanceLabel("/var/lib/go-trader/live/config.json")
	if got != "live" {
		t.Errorf("killSwitchInstanceLabel(.../live/config.json) = %q, want %q", got, "live")
	}
	got = killSwitchInstanceLabel("/var/lib/go-trader/paper-hl-eth/config.json")
	if got != "paper-hl-eth" {
		t.Errorf("killSwitchInstanceLabel(.../paper-hl-eth/config.json) = %q, want %q", got, "paper-hl-eth")
	}
}

func TestKillSwitchInstanceLabel_FallsBackWhenPathGivesNothingUseful(t *testing.T) {

	got := killSwitchInstanceLabel("config.json")
	if got == "." {
		t.Errorf("killSwitchInstanceLabel(%q) = %q, want a real fallback, not \".\"", "config.json", got)
	}
	if got == "" {
		t.Error("killSwitchInstanceLabel must never return an empty label")
	}
}

func TestFormatKillSwitchResetPrompt_ConfirmedFlatIncludesContextAndIdentity(t *testing.T) {
	plan := KillSwitchClosePlan{
		OnChainConfirmedFlat: true,
		DiscordMessage:       "**PORTFOLIO KILL SWITCH**\nportfolio drawdown 25.0% exceeds limit 20.0%\nHL closes: [ETH]. Virtual state cleared. Manual reset required.",
	}

	got := formatKillSwitchResetPrompt("live", "0xabc123", plan)

	if !strings.Contains(got, "live") || !strings.Contains(got, "0xabc123") {
		t.Errorf("expected instance label and HL address in prompt, got: %s", got)
	}
	if !strings.Contains(got, "portfolio drawdown 25.0%") {
		t.Errorf("expected reused drawdown reason in prompt, got: %s", got)
	}
	if !strings.Contains(got, "HL closes: [ETH]") {
		t.Errorf("expected reused close report in prompt, got: %s", got)
	}
	if !strings.Contains(got, "does not itself close or protect any position") {
		t.Errorf("expected explicit reset semantics in prompt, got: %s", got)
	}
	if strings.Contains(got, "still retrying") {
		t.Errorf("confirmed-flat prompt must not claim the close is still retrying, got: %s", got)
	}
}

func TestFormatKillSwitchResetPrompt_LatchedRetryingWarnsProtectionMayBeGone(t *testing.T) {
	plan := KillSwitchClosePlan{
		OnChainConfirmedFlat: false,
		DiscordMessage:       "**PORTFOLIO KILL SWITCH (LATCHED, RETRYING)**\nportfolio drawdown 25.0% exceeds limit 20.0%\nHL live close errors — ETH: timeout. Virtual state preserved. Next cycle will retry.",
	}

	got := formatKillSwitchResetPrompt("live", "0xabc123", plan)

	if !strings.Contains(got, "still retrying") {
		t.Errorf("expected a not-yet-confirmed-flat warning in prompt, got: %s", got)
	}
	if !strings.Contains(got, "stop-losses may already be cancelled") {
		t.Errorf("expected the naked-stop-loss risk to be called out while retrying, got: %s", got)
	}
}

func TestFormatKillSwitchResetPrompt_OmitsAddressWhenHLNotConfigured(t *testing.T) {
	plan := KillSwitchClosePlan{
		OnChainConfirmedFlat: true,
		DiscordMessage:       "**PORTFOLIO KILL SWITCH**\nreason\nHL not configured. Virtual state cleared. Manual reset required.",
	}

	got := formatKillSwitchResetPrompt("paper-hl-eth", "", plan)

	if strings.Contains(got, "Hyperliquid ") && strings.Contains(got, "()") {
		t.Errorf("expected no dangling empty-address parenthetical, got: %s", got)
	}
	if !strings.Contains(got, "paper-hl-eth") {
		t.Errorf("expected instance label present regardless of HL address, got: %s", got)
	}
}

func TestHyperliquidKillSwitchFillShare_FailsClosedWhenNotAPeer(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Leverage: 5,
			Args: []string{"ema", "ETH", "1h", "--mode=live"}},
	}
	virtualQty := hlVirtualQuantitySnapshot{
		"ETH": {"hl-a": 1.5, "hl-b": 0.5},
	}

	t.Run("non-peer strategy gets zero share", func(t *testing.T) {
		outsider := StrategyConfig{ID: "hl-c", Platform: "hyperliquid", Type: "perps",
			Args: []string{"rsi", "ETH", "1h", "--mode=live"}}
		sz, fee := hyperliquidKillSwitchFillShare(outsider, "ETH", 2.0, 4.0, hlLive, virtualQty)
		if sz != 0 || fee != 0 {
			t.Errorf("fill share for non-peer = (%v, %v); want (0, 0) fail-closed", sz, fee)
		}
	})

	t.Run("peer with zero virtual quantity gets zero share", func(t *testing.T) {
		zeroQty := hlVirtualQuantitySnapshot{
			"ETH": {"hl-a": 0, "hl-b": 0.5},
		}
		sz, fee := hyperliquidKillSwitchFillShare(hlLive[0], "ETH", 2.0, 4.0, hlLive, zeroQty)
		if sz != 0 || fee != 0 {
			t.Errorf("fill share for zero-qty peer = (%v, %v); want (0, 0) fail-closed", sz, fee)
		}
	})

	t.Run("all peers zero quantity gets zero share", func(t *testing.T) {
		allZero := hlVirtualQuantitySnapshot{
			"ETH": {"hl-a": 0, "hl-b": 0},
		}
		sz, fee := hyperliquidKillSwitchFillShare(hlLive[0], "ETH", 2.0, 4.0, hlLive, allZero)
		if sz != 0 || fee != 0 {
			t.Errorf("fill share with all-zero peers = (%v, %v); want (0, 0) fail-closed", sz, fee)
		}
	})

	t.Run("sole peer receives full fill", func(t *testing.T) {
		solo := hlLive[:1]
		sz, fee := hyperliquidKillSwitchFillShare(hlLive[0], "ETH", 2.0, 4.0, solo, virtualQty)
		if sz != 2.0 || fee != 4.0 {
			t.Errorf("sole-peer fill share = (%v, %v); want full (2.0, 4.0)", sz, fee)
		}
	})

	t.Run("peer shares sum to the reported fill", func(t *testing.T) {
		szA, feeA := hyperliquidKillSwitchFillShare(hlLive[0], "ETH", 2.0, 4.0, hlLive, virtualQty)
		szB, feeB := hyperliquidKillSwitchFillShare(hlLive[1], "ETH", 2.0, 4.0, hlLive, virtualQty)
		if math.Abs(szA+szB-2.0) > 1e-9 || math.Abs(feeA+feeB-4.0) > 1e-9 {
			t.Errorf("peer shares sum to (%v, %v); want (2.0, 4.0)", szA+szB, feeA+feeB)
		}
	})
}
