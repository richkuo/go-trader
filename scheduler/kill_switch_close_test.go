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
	if math.Abs(tA.ExchangeFee-3.0) > 1e-9 || math.Abs(tB.ExchangeFee-1.0) > 1e-9 {
		t.Errorf("peer fees = %.4f / %.4f; want 3.0 / 1.0 (virtual-quantity share of %.1f)", tA.ExchangeFee, tB.ExchangeFee, totalFee)
	}
	if len(stateA.ClosedPositions) != 1 {
		t.Fatalf("expected 1 closed position for A, got %d", len(stateA.ClosedPositions))
	}
	if wantPnL := -153.0; math.Abs(stateA.ClosedPositions[0].RealizedPnL-wantPnL) > 1e-9 {
		t.Errorf("A RealizedPnL = %.4f; want %.4f (1.5 * (3000-3100) - 3.0 fee)", stateA.ClosedPositions[0].RealizedPnL, wantPnL)
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

type killSwitchPlatformHarness struct {
	name         string
	label        string
	coin         string
	extraCoin    string
	build        func(coins []string, closeErrs map[string]error, fetchErr error) (KillSwitchCloseInputs, *[]string, *int)
	errs         func(plan KillSwitchClosePlan) map[string]error
	unconfigured func(plan KillSwitchClosePlan) []string
}

func killSwitchPlatformHarnesses() []killSwitchPlatformHarness {
	return []killSwitchPlatformHarness{
		{
			name: "OKX", label: "OKX", coin: "BTC", extraCoin: "SOL",
			build: func(coins []string, closeErrs map[string]error, fetchErr error) (KillSwitchCloseInputs, *[]string, *int) {
				var positions []OKXPosition
				for _, c := range coins {
					positions = append(positions, OKXPosition{Coin: c, Size: 0.01, EntryPrice: 42000, Side: "long"})
				}
				closer, calls := stubOKXLiveCloser(closeErrs)
				fetcher, fetchCalls := stubOKXPositionsFetcher(positions, fetchErr)
				return KillSwitchCloseInputs{
					OKXLiveAllPerps: []StrategyConfig{{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
						Args: []string{"sma", "BTC", "1h", "--mode=live"}}},
					OKXCloser:       closer,
					OKXFetcher:      fetcher,
					PortfolioReason: "drawdown reason",
					CloseTimeout:    time.Second,
				}, calls, fetchCalls
			},
			errs: func(plan KillSwitchClosePlan) map[string]error { return plan.OKXCloseReport.Errors },
			unconfigured: func(plan KillSwitchClosePlan) []string {
				var out []string
				for _, p := range plan.OKXUnconfigured {
					out = append(out, p.Coin)
				}
				return out
			},
		},
		{
			name: "Robinhood", label: "Robinhood", coin: "BTC", extraCoin: "DOGE",
			build: func(coins []string, closeErrs map[string]error, fetchErr error) (KillSwitchCloseInputs, *[]string, *int) {
				var positions []RobinhoodPosition
				for _, c := range coins {
					positions = append(positions, RobinhoodPosition{Coin: c, Size: 0.01, AvgPrice: 42000})
				}
				closer, calls := stubRHLiveCloser(closeErrs)
				fetcher, fetchCalls := stubRHPositionsFetcher(positions, fetchErr)
				return KillSwitchCloseInputs{
					RHLiveCrypto: []StrategyConfig{{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
						Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}}},
					RHCloser:        closer,
					RHFetcher:       fetcher,
					PortfolioReason: "drawdown reason",
					CloseTimeout:    time.Second,
				}, calls, fetchCalls
			},
			errs: func(plan KillSwitchClosePlan) map[string]error { return plan.RHCloseReport.Errors },
			unconfigured: func(plan KillSwitchClosePlan) []string {
				var out []string
				for _, p := range plan.RHUnconfigured {
					out = append(out, p.Coin)
				}
				return out
			},
		},
		{
			name: "TopStep", label: "TopStep", coin: "ES", extraCoin: "NQ",
			build: func(coins []string, closeErrs map[string]error, fetchErr error) (KillSwitchCloseInputs, *[]string, *int) {
				var positions []TopStepPosition
				for _, c := range coins {
					positions = append(positions, TopStepPosition{Coin: c, Size: 2, AvgPrice: 5000, Side: "long"})
				}
				closer, calls := stubTSLiveCloser(closeErrs)
				fetcher, fetchCalls := stubTSPositionsFetcher(positions, fetchErr)
				return KillSwitchCloseInputs{
					TSLiveAll: []StrategyConfig{{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
						Args: []string{"momentum", "ES", "1h", "--mode=live"}}},
					TSCloser:        closer,
					TSFetcher:       fetcher,
					PortfolioReason: "drawdown reason",
					CloseTimeout:    time.Second,
				}, calls, fetchCalls
			},
			errs: func(plan KillSwitchClosePlan) map[string]error { return plan.TSCloseReport.Errors },
			unconfigured: func(plan KillSwitchClosePlan) []string {
				var out []string
				for _, p := range plan.TSUnconfigured {
					out = append(out, p.Coin)
				}
				return out
			},
		},
	}
}

func TestPlanKillSwitchClose_PlatformLifecycle(t *testing.T) {
	for _, h := range killSwitchPlatformHarnesses() {
		t.Run(h.name+"/HappyPath", func(t *testing.T) {
			in, calls, fetchCalls := h.build([]string{h.coin}, nil, nil)
			plan := planKillSwitchClose(in)
			if !plan.OnChainConfirmedFlat {
				t.Fatalf("expected ConfirmedFlat, got plan=%+v", plan)
			}
			if *fetchCalls != 1 {
				t.Errorf("%s fetcher should be called exactly once, got %d", h.name, *fetchCalls)
			}
			if len(*calls) != 1 || (*calls)[0] != h.coin {
				t.Errorf("closer calls = %v, want [%s]", *calls, h.coin)
			}
			if want := h.label + " closes: [" + h.coin + "]"; !strings.Contains(plan.DiscordMessage, want) {
				t.Errorf("expected %q in message, got: %s", want, plan.DiscordMessage)
			}
		})
		t.Run(h.name+"/CloseError", func(t *testing.T) {
			in, _, _ := h.build([]string{h.coin}, map[string]error{h.coin: fmt.Errorf("%s venue rate limited", h.name)}, nil)
			plan := planKillSwitchClose(in)
			if plan.OnChainConfirmedFlat {
				t.Fatalf("expected NOT ConfirmedFlat on %s close error — would clear virtual state while the venue is still live", h.name)
			}
			if got, ok := h.errs(plan)[h.coin]; !ok || got == nil {
				t.Errorf("expected %s error in %s report, got %v", h.coin, h.name, h.errs(plan))
			}
			if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
				t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
			}
			if !strings.Contains(plan.DiscordMessage, h.name+" venue rate limited") {
				t.Errorf("expected %s error detail in message, got: %s", h.name, plan.DiscordMessage)
			}
		})
		t.Run(h.name+"/FetchFailure", func(t *testing.T) {
			in, calls, _ := h.build(nil, nil, fmt.Errorf("%s auth failed", h.name))
			plan := planKillSwitchClose(in)
			if plan.OnChainConfirmedFlat {
				t.Fatalf("expected NOT ConfirmedFlat on %s fetch failure", h.name)
			}
			if len(*calls) != 0 {
				t.Errorf("closer must not be invoked when fetch failed, got %v", *calls)
			}
			if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
				t.Errorf("expected LATCHED message on fetch failure, got: %s", plan.DiscordMessage)
			}
		})
		t.Run(h.name+"/UnconfiguredBlocksReset", func(t *testing.T) {
			in, calls, _ := h.build([]string{h.coin, h.extraCoin}, nil, nil)
			plan := planKillSwitchClose(in)
			if plan.OnChainConfirmedFlat {
				t.Fatalf("expected NOT ConfirmedFlat — unconfigured %s position is still live", h.extraCoin)
			}
			if got := h.unconfigured(plan); len(got) != 1 || got[0] != h.extraCoin {
				t.Errorf("expected unconfigured=[%s], got %v", h.extraCoin, got)
			}
			if len(*calls) != 1 || (*calls)[0] != h.coin {
				t.Errorf("closer calls = %v, want [%s]", *calls, h.coin)
			}
			if !strings.Contains(plan.DiscordMessage, "manual intervention required") {
				t.Errorf("expected manual intervention note, got: %s", plan.DiscordMessage)
			}
		})
	}
}

func TestPlanKillSwitchClose_PeerPlatformFailureStillLatches(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	hlPos := []HLPosition{{Coin: "ETH", Size: 0.5}}
	cases := []struct {
		name string
		wire func(in *KillSwitchCloseInputs)
		want string
	}{
		{
			name: "OKX", want: "okx err",
			wire: func(in *KillSwitchCloseInputs) {
				in.OKXLiveAllPerps = []StrategyConfig{{ID: "okx-btc", Platform: "okx", Type: "perps",
					Args: []string{"sma", "BTC", "1h", "--mode=live"}}}
				in.OKXCloser, _ = stubOKXLiveCloser(map[string]error{"BTC": fmt.Errorf("okx err")})
				in.OKXFetcher, _ = stubOKXPositionsFetcher([]OKXPosition{{Coin: "BTC", Size: 0.01, Side: "long"}}, nil)
			},
		},
		{
			name: "Robinhood", want: "rh err",
			wire: func(in *KillSwitchCloseInputs) {
				in.RHLiveCrypto = []StrategyConfig{{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
					Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}}}
				in.RHCloser, _ = stubRHLiveCloser(map[string]error{"BTC": fmt.Errorf("rh err")})
				in.RHFetcher, _ = stubRHPositionsFetcher([]RobinhoodPosition{{Coin: "BTC", Size: 0.01}}, nil)
			},
		},
		{
			name: "TopStep", want: "venue down",
			wire: func(in *KillSwitchCloseInputs) {
				in.TSLiveAll = []StrategyConfig{{ID: "ts-momentum-es", Platform: "topstep", Type: "futures",
					Args: []string{"momentum", "ES", "1h", "--mode=live"}}}
				in.TSCloser, _ = stubTSLiveCloser(map[string]error{"ES": fmt.Errorf("venue down")})
				in.TSFetcher, _ = stubTSPositionsFetcher([]TopStepPosition{{Coin: "ES", Size: 2}}, nil)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hlCloser, _ := stubHLLiveCloser(nil)
			hlFetcher, _ := stubHLStateFetcher(nil, nil)
			in := defaultHLInputs("0xaddr", true, hlPos, hlLive, "drawdown reason", time.Second, hlCloser, hlFetcher)
			tc.wire(&in)
			plan := planKillSwitchClose(in)
			if plan.OnChainConfirmedFlat {
				t.Fatalf("%s failure must latch the switch even when HL succeeded", tc.name)
			}
			if len(plan.CloseReport.ClosedCoins) != 1 || plan.CloseReport.ClosedCoins[0] != "ETH" {
				t.Errorf("HL close should still run: got %v", plan.CloseReport.ClosedCoins)
			}
			if !strings.Contains(plan.DiscordMessage, "LATCHED, RETRYING") {
				t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
			}
			if !strings.Contains(plan.DiscordMessage, tc.want) {
				t.Errorf("%s error missing from message, got: %s", tc.name, plan.DiscordMessage)
			}
		})
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
