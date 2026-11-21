package main

import (
	"context"
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

const stubHLEndpoint = "https://hl.stub"

func stubHLStateFetcher(positions []HLPosition, err error) (HLStrictPositionReader, *int) {
	var calls int
	reader := func(ctx context.Context, target hlPositionTarget) (hlStrictPositionSnapshot, error) {
		calls++
		if err != nil {
			return hlStrictPositionSnapshot{}, err
		}
		return hlStrictPositionSnapshot{Target: target, Positions: positions}, nil
	}
	return reader, &calls
}

func withHLBaselineRead(baseline []HLPosition, later HLStrictPositionReader) HLStrictPositionReader {
	served := false
	return func(ctx context.Context, target hlPositionTarget) (hlStrictPositionSnapshot, error) {
		if !served {
			served = true
			return hlStrictPositionSnapshot{Target: target, Positions: baseline}, nil
		}
		return later(ctx, target)
	}
}

func defaultHLInputs(hlAddr string, baselineFromPositions bool, positions []HLPosition,
	hlLive []StrategyConfig, reason string, timeout time.Duration,
	closer HyperliquidLiveCloser, reader HLStrictPositionReader) KillSwitchCloseInputs {
	if baselineFromPositions {
		reader = withHLBaselineRead(positions, reader)
	}
	return KillSwitchCloseInputs{
		HLAddr:          hlAddr,
		HLEndpoint:      stubHLEndpoint,
		HLLiveAll:       hlLive,
		HLCloser:        closer,
		HLReader:        reader,
		PortfolioReason: reason,
		CloseTimeout:    timeout,
	}
}

func TestPlanKillSwitchClose(t *testing.T) {
	t.Run("HappyPath", func(t *testing.T) {
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
		if *fetchCalls != 1 {
			t.Errorf("one fresh position read must follow the close, got %d", *fetchCalls)
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
	})
	t.Run("CloseErrorLatchesWithOrWithoutVerification", func(t *testing.T) {
		hlLive := []StrategyConfig{
			{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
				Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
		}
		positions := []HLPosition{{Coin: "ETH", Size: 0.5}}
		run := func(closeErr, fetchErr error, fetchPositions []HLPosition) (KillSwitchClosePlan, int) {
			closer, _ := stubHLLiveCloser(map[string]error{"ETH": closeErr})
			fetcher, fetchCalls := stubHLStateFetcher(fetchPositions, fetchErr)
			plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, hlLive,
				"portfolio drawdown 25.0% exceeds limit 20.0%",
				time.Second, closer, fetcher))
			return plan, *fetchCalls
		}

		t.Run("close error", func(t *testing.T) {
			plan, fetchCalls := run(fmt.Errorf("hl rate limited"), nil, positions)
			if fetchCalls != 1 {
				t.Fatalf("fetcher should be called once to verify the close error, got %d", fetchCalls)
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
		})

		t.Run("verification fetch failure", func(t *testing.T) {
			plan, fetchCalls := run(fmt.Errorf("post-submit disconnect"), fmt.Errorf("hl 503"), nil)
			if fetchCalls != 1 {
				t.Fatalf("fetcher should be called once to verify the close error, got %d", fetchCalls)
			}
			if plan.OnChainConfirmedFlat {
				t.Fatal("expected NOT ConfirmedFlat when the verification fetch also fails")
			}
			if got, ok := plan.CloseReport.Errors["ETH"]; !ok || got == nil {
				t.Errorf("expected ETH error to remain in report, got %v", plan.CloseReport.Errors)
			}
			if !strings.Contains(strings.Join(plan.LogLines, "\n"), "the position read after the last close attempt failed: hl 503") {
				t.Errorf("expected verification fetch error log line, got %v", plan.LogLines)
			}
		})
	})
	t.Run("FetchFailure", func(t *testing.T) {
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
		if !strings.Contains(plan.DiscordMessage, "could not fetch HL positions before the close: hl 503") {
			t.Errorf("expected fetch-failure detail in message, got: %s", plan.DiscordMessage)
		}
	})
	t.Run("UnconfiguredPositionBlocksReset", func(t *testing.T) {
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
	})
	t.Run("PlatformLifecycle", func(t *testing.T) {
		for _, h := range killSwitchPlatformHarnesses() {
			t.Run(h.name+"/HappyPath", func(t *testing.T) {
				in, calls, fetchCalls := h.build([]string{h.coin}, nil, nil)
				plan := planKillSwitchClose(in)
				if !plan.OnChainConfirmedFlat {
					t.Fatalf("expected ConfirmedFlat, got plan=%+v", plan)
				}
				wantFetches := 1
				if h.name == "Robinhood" {
					wantFetches = 2
				}
				if *fetchCalls != wantFetches {
					t.Errorf("%s fetcher should be called %d time(s), got %d", h.name, wantFetches, *fetchCalls)
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
	})
	t.Run("PeerPlatformFailureStillLatches", func(t *testing.T) {
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
				name: "Robinhood", want: "rh err",
				wire: func(in *KillSwitchCloseInputs) {
					in.RHLiveCrypto = []StrategyConfig{{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
						Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}}}
					in.RHCloser, _ = stubRHLiveCloser(map[string]error{"BTC": fmt.Errorf("rh err")})
					in.RHFetcher, _ = stubRHPositionsFetcher([]RobinhoodPosition{{Coin: "BTC", Size: 0.01}}, nil)
					in.RHBookSnapshot = stubRHBookSnapshot(map[string]float64{"BTC": 0.01})
					in.RHSettle = func() robinhoodSettleOutcome { return robinhoodSettleOutcome{} }
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
	})
}

func TestHyperliquidKillSwitchClose(t *testing.T) {
	t.Run("UsesRealFillBeforeMark", func(t *testing.T) {
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
		forceCloseKillSwitchPositions(s, hlLive[0], map[string]float64{"BTC": 48000}, plan.CloseReport.Fills, hlLive, nil, nil, nil)

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
	})
	t.Run("AlreadyFlatRecoversRecentUserFill", func(t *testing.T) {
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
		forceCloseKillSwitchPositions(s, hlLive[0], map[string]float64{"ETH": 1700}, plan.CloseReport.Fills, hlLive, nil, nil, nil)
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
	})
	t.Run("AlreadyFlatRecoveryRefusesUnsafeUserFills", func(t *testing.T) {
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
		plan := func(recover func(since time.Time) (*HLUserFillsResult, error)) KillSwitchClosePlan {
			fetcher, _ := stubHLStateFetcher(nil, nil)
			in := defaultHLInputs("0xaddr", true, positions, hlLive,
				"portfolio drawdown 25.0% exceeds limit 20.0%",
				time.Second, closer, fetcher)
			in.HLNoFillRecoverer = recover
			return planKillSwitchClose(in)
		}

		t.Run("ambiguous userFills falls back", func(t *testing.T) {
			got := plan(func(since time.Time) (*HLUserFillsResult, error) {
				return &HLUserFillsResult{
					ByOID: map[string]HLFillSummary{
						"100": {Coin: "ETH", Fee: 0.4, Qty: 1.0, Px: 1990, ClosedPnLGross: -10, LastTimeMS: time.Now().UnixMilli()},
						"101": {Coin: "ETH", Fee: 0.5, Qty: 1.0, Px: 1989, ClosedPnLGross: -11, LastTimeMS: time.Now().UnixMilli()},
					},
				}, nil
			})
			if !got.OnChainConfirmedFlat {
				t.Fatalf("expected confirmed-flat plan, got %+v", got)
			}
			if _, ok := got.CloseReport.Fills["ETH"]; ok {
				t.Fatalf("ambiguous userFills must not inject a fill: %+v", got.CloseReport.Fills)
			}
			if !strings.Contains(strings.Join(got.LogLines, "\n"), "multiple userFills candidates") {
				t.Fatalf("missing ambiguity warning: %v", got.LogLines)
			}
		})

		t.Run("opening fill is rejected", func(t *testing.T) {
			got := plan(func(since time.Time) (*HLUserFillsResult, error) {
				return &HLUserFillsResult{
					ByOID: map[string]HLFillSummary{
						"300": {Coin: "ETH", Fee: 0.4, Qty: 1.0, Px: 1990, ClosedPnLGross: 0, LastTimeMS: time.Now().UnixMilli()},
					},
				}, nil
			})
			if _, ok := got.CloseReport.Fills["ETH"]; ok {
				t.Fatalf("opening fill must never be adopted as the close: %+v", got.CloseReport.Fills)
			}
			if !strings.Contains(strings.Join(got.LogLines, "\n"), "no userFills match") {
				t.Fatalf("expected fail-closed warning, got: %v", got.LogLines)
			}
		})

		t.Run("fill before since is ignored", func(t *testing.T) {
			var gotSince time.Time
			got := plan(func(since time.Time) (*HLUserFillsResult, error) {
				gotSince = since
				return &HLUserFillsResult{
					ByOID: map[string]HLFillSummary{
						"400": {Coin: "ETH", Fee: 0.4, Qty: 1.0, Px: 1990, ClosedPnLGross: -10,
							LastTimeMS: since.Add(-time.Minute).UnixMilli()},
					},
				}, nil
			})
			if gotSince.IsZero() {
				t.Fatal("recovery since timestamp must be populated")
			}
			if _, ok := got.CloseReport.Fills["ETH"]; ok {
				t.Fatalf("fill before the since bound must not be adopted: %+v", got.CloseReport.Fills)
			}
		})
	})
	t.Run("SharedCoinSplitsFillByVirtualQuantity", func(t *testing.T) {
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

		forceCloseKillSwitchPositions(stateA, hlLive[0], prices, fills, hlLive, hlVirtualQty, nil, nil)
		forceCloseKillSwitchPositions(stateB, hlLive[1], prices, fills, hlLive, hlVirtualQty, nil, nil)

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
	})
}

func stubRHLiveCloser(errs map[string]error) (RobinhoodLiveCloser, *[]string) {
	var calls []string
	closer := func(req RobinhoodCloseRequest) (*RobinhoodCloseResult, error) {
		symbol := req.Symbol
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

const (
	stubRHUsername = "stub-user"
	stubRHVenueID  = "acct-stub"
)

var stubRHAccountKey = robinhoodAccountKeyFor(stubRHUsername)

func useStubRHAccount(t *testing.T) {
	t.Helper()
	prev := robinhoodUsername
	robinhoodUsername = func() string { return stubRHUsername }
	t.Cleanup(func() { robinhoodUsername = prev })
}

func stubRHBookSnapshotOf(targets map[string]float64, heldElsewhere ...string) robinhoodBookSnapshot {
	snap := robinhoodBookSnapshot{CurrentKey: stubRHAccountKey, Books: map[string][]robinhoodBookEntry{}, Held: map[string]bool{}}
	for _, coin := range heldElsewhere {
		snap.Held[coin] = true
	}
	for coin, qty := range targets {
		snap.Books[coin] = append(snap.Books[coin], robinhoodBookEntry{StrategyID: "stub-" + coin, PositionID: "pos-" + coin, Side: "long", Quantity: qty, AccountKey: stubRHAccountKey, VenueAccountID: stubRHVenueID})
		snap.Held[coin] = true
	}
	return snap
}

func stubRHBookSnapshot(targets map[string]float64, heldElsewhere ...string) func() robinhoodBookSnapshot {
	calls := 0
	return func() robinhoodBookSnapshot {
		calls++
		if calls > 1 {
			return stubRHBookSnapshotOf(nil, heldElsewhere...)
		}
		return stubRHBookSnapshotOf(targets, heldElsewhere...)
	}
}

func stubRHPositionsFetcher(positions []RobinhoodPosition, err error) (RobinhoodPositionsFetcher, *int) {
	var calls int
	fetcher := func() (RobinhoodHoldings, error) {
		calls++
		if err != nil {
			return RobinhoodHoldings{}, err
		}
		if calls > 1 {
			return RobinhoodHoldings{AccountID: stubRHVenueID}, nil
		}
		return RobinhoodHoldings{AccountID: stubRHVenueID, Positions: positions}, nil
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
					RHBookSnapshot:  stubRHBookSnapshot(map[string]float64{"BTC": 0.01}, "DOGE"),
					RHSettle:        func() robinhoodSettleOutcome { return robinhoodSettleOutcome{} },
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
	}
}

type hlScriptedRead struct {
	positions []HLPosition
	err       error
}

func hlScriptedReader(seq *[]string, reads ...hlScriptedRead) (HLStrictPositionReader, *int) {
	calls := 0
	return func(ctx context.Context, target hlPositionTarget) (hlStrictPositionSnapshot, error) {
		i := calls
		calls++
		if seq != nil {
			*seq = append(*seq, "read")
		}
		if i >= len(reads) {
			i = len(reads) - 1
		}
		if reads[i].err != nil {
			return hlStrictPositionSnapshot{}, reads[i].err
		}
		return hlStrictPositionSnapshot{Target: target, Positions: reads[i].positions}, nil
	}, &calls
}

func hlProofInputs(roster []StrategyConfig, closer HyperliquidLiveCloser, reader HLStrictPositionReader) KillSwitchCloseInputs {
	return KillSwitchCloseInputs{
		HLAddr:          "0xaddr",
		HLEndpoint:      stubHLEndpoint,
		HLLiveAll:       roster,
		HLCloser:        closer,
		HLReader:        reader,
		PortfolioReason: "portfolio drawdown 25.0% exceeds limit 20.0%",
		CloseTimeout:    time.Second,
	}
}

func hlProofRoster(coins ...string) []StrategyConfig {
	out := make([]StrategyConfig, 0, len(coins))
	for _, c := range coins {
		out = append(out, StrategyConfig{ID: "hl-" + strings.ToLower(c), Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", c, "1h", "--mode=live"}})
	}
	return out
}

func hlReplyCloser(seq *[]string, replies map[string]func() (*HyperliquidCloseResult, error)) HyperliquidLiveCloser {
	return func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		if seq != nil {
			*seq = append(*seq, "close:"+symbol)
		}
		if reply, ok := replies[symbol]; ok {
			return reply()
		}
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: symbol, Fill: &HyperliquidCloseFill{TotalSz: 1, AvgPx: 100}}, Platform: "hyperliquid"}, nil
	}
}

func killSwitchFlatListsHave(plan KillSwitchClosePlan, coin string) bool {
	return containsString(plan.CloseReport.ClosedCoins, coin) || containsString(plan.CloseReport.AlreadyFlat, coin)
}

func TestKillSwitchFlatProof(t *testing.T) {
	t.Run("PartialFillKeepsTheLatchAndBooksTheFillOnce", func(t *testing.T) {
		roster := hlProofRoster("ETH")
		closer := hlReplyCloser(nil, map[string]func() (*HyperliquidCloseResult, error){
			"ETH": func() (*HyperliquidCloseResult, error) {
				return &HyperliquidCloseResult{
					Close:                       &HyperliquidClose{Symbol: "ETH", Fill: &HyperliquidCloseFill{TotalSz: 0.2, AvgPx: 3000, Fee: 0.1, OID: 4242}},
					Platform:                    "hyperliquid",
					CancelStopLossSucceeded:     true,
					CancelStopLossSucceededOIDs: []int64{11, 12},
				}, nil
			},
		})
		reader, reads := hlScriptedReader(nil,
			hlScriptedRead{positions: []HLPosition{{Coin: "ETH", Size: 0.5, EntryPrice: 2900}}},
			hlScriptedRead{positions: []HLPosition{{Coin: "ETH", Size: 0.3, EntryPrice: 2900}}})
		in := hlProofInputs(roster, closer, reader)
		in.HLStopLossOIDs = map[string][]int64{"ETH": {11, 12}}
		plan := planKillSwitchClose(in)

		if *reads != 2 {
			t.Fatalf("reads = %d, want a pre-close read and one read after the close", *reads)
		}
		if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
			t.Fatal("a 0.3 ETH remainder must block flat status and automatic reset")
		}
		if _, ok := plan.CloseReport.Errors["ETH"]; !ok || killSwitchFlatListsHave(plan, "ETH") {
			t.Fatalf("ETH must be an error and absent from both flat lists: errors=%v closed=%v already=%v",
				plan.CloseReport.Errors, plan.CloseReport.ClosedCoins, plan.CloseReport.AlreadyFlat)
		}
		if fill, ok := plan.CloseReport.Fills["ETH"]; !ok || fill.TotalSz != 0.2 {
			t.Fatalf("the reported 0.2 fill must stay available for booking, fills = %+v", plan.CloseReport.Fills)
		}
		for _, want := range []string{"long 0.3 still open", "pre-close long 0.5", "close reported fill 0.2 @ 3000",
			"cancel reported for [11 12] before the close", "the remaining position may be unprotected", "LATCHED, RETRYING"} {
			if !strings.Contains(plan.DiscordMessage, want) {
				t.Errorf("retry message missing %q: %s", want, plan.DiscordMessage)
			}
		}

		state := &StrategyState{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Cash: 1000,
			Positions: map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2900, Side: "long", Multiplier: 1}}}
		strategies := map[string]*StrategyState{"hl-eth": state}
		virtualQty := snapshotHyperliquidVirtualQuantities(strategies, roster)
		for i := 0; i < 2; i++ {
			applyKillSwitchSettledLegsWhileLatched(strategies, roster, &plan, roster, virtualQty, map[string]float64{"ETH": 3000}, nil)
		}
		pos := state.Positions["ETH"]
		if pos == nil || math.Abs(pos.Quantity-0.3) > 1e-9 {
			t.Fatalf("the book must keep the 0.3 remainder while latched, got %+v", state.Positions)
		}
		booked := 0
		for _, tr := range state.TradeHistory {
			if tr.ExchangeOrderID == "4242" {
				booked++
			}
		}
		if booked != 1 {
			t.Fatalf("the 0.2 fill must book exactly once, booked %d times: %+v", booked, state.TradeHistory)
		}
	})

	t.Run("OneReadFollowsTheLastCloserCall", func(t *testing.T) {
		var seq []string
		closer := hlReplyCloser(&seq, map[string]func() (*HyperliquidCloseResult, error){
			"BTC": func() (*HyperliquidCloseResult, error) { return nil, fmt.Errorf("post-submit disconnect") },
			"SOL": func() (*HyperliquidCloseResult, error) {
				return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: "SOL", AlreadyFlat: true}, Platform: "hyperliquid"}, nil
			},
		})
		reader, _ := hlScriptedReader(&seq,
			hlScriptedRead{positions: []HLPosition{{Coin: "BTC", Size: 0.01}, {Coin: "ETH", Size: 1}, {Coin: "SOL", Size: -3}}},
			hlScriptedRead{positions: []HLPosition{}})
		plan := planKillSwitchClose(hlProofInputs(hlProofRoster("BTC", "ETH", "SOL"), closer, reader))

		want := "read,close:BTC,close:ETH,close:SOL,read"
		if got := strings.Join(seq, ","); got != want {
			t.Fatalf("sequence = %s, want %s", got, want)
		}
		if !plan.OnChainConfirmedFlat || !plan.HLProof.Verified {
			t.Fatalf("a valid empty read after the last call must prove venue flat: %+v", plan.CloseReport)
		}
		if err := plan.CloseReport.ResolvedErrors["BTC"]; err == nil || !strings.Contains(err.Error(), "post-submit disconnect") {
			t.Fatalf("the BTC close error must stay available for diagnosis, resolved = %v", plan.CloseReport.ResolvedErrors)
		}
		if !containsString(plan.CloseReport.AlreadyFlat, "BTC") || !containsString(plan.CloseReport.AlreadyFlat, "SOL") {
			t.Fatalf("fresh-read flat coins = %v, want BTC and SOL", plan.CloseReport.AlreadyFlat)
		}

		seq = nil
		noAttempt, reads := hlScriptedReader(&seq, hlScriptedRead{positions: []HLPosition{{Coin: "ETH", Size: 0}}})
		in := hlProofInputs(hlProofRoster("ETH"), closer, noAttempt)
		in.QuoteLane = func(time.Duration) killSwitchQuoteReport {
			seq = append(seq, "quote-lane")
			return killSwitchQuoteReport{Clear: true}
		}
		plan = planKillSwitchClose(in)
		if *reads != 1 || strings.Join(seq, ",") != "quote-lane,read" || !plan.OnChainConfirmedFlat {
			t.Fatalf("a no-attempt cycle uses one strict read taken after the quote lane: reads=%d seq=%v flat=%v", *reads, seq, plan.OnChainConfirmedFlat)
		}
	})

	t.Run("UnreadableOrUnboundAccountsBlockFlatProof", func(t *testing.T) {
		roster := hlProofRoster("ETH")
		open := []HLPosition{{Coin: "ETH", Size: 0.5}}
		cases := []struct {
			name  string
			wire  func(in *KillSwitchCloseInputs)
			want  string
			calls int
		}{
			{name: "missing account", wire: func(in *KillSwitchCloseInputs) { in.HLAddr = "" }, want: "no account address is set"},
			{name: "missing reader", wire: func(in *KillSwitchCloseInputs) { in.HLReader = nil }, want: "reader is unwired"},
			{name: "missing endpoint", wire: func(in *KillSwitchCloseInputs) { in.HLEndpoint = "" }, want: "no Hyperliquid info endpoint"},
			{name: "pre-close read timeout", wire: func(in *KillSwitchCloseInputs) {
				in.HLReader, _ = hlScriptedReader(nil, hlScriptedRead{err: context.DeadlineExceeded})
			}, want: "could not fetch HL positions before the close: context deadline exceeded"},
			{name: "other account answered", wire: func(in *KillSwitchCloseInputs) {
				in.HLReader = func(ctx context.Context, target hlPositionTarget) (hlStrictPositionSnapshot, error) {
					return hlStrictPositionSnapshot{Target: hlPositionTarget{Account: "0xother", Endpoint: target.Endpoint}}, nil
				}
			}, want: "not the close target"},
			{name: "post-close read failure", wire: func(in *KillSwitchCloseInputs) {
				in.HLReader, _ = hlScriptedReader(nil, hlScriptedRead{positions: open}, hlScriptedRead{err: fmt.Errorf("clearinghouseState has no assetPositions list")})
			}, want: "flat not verified", calls: 1},
			{name: "builder namespace", wire: func(in *KillSwitchCloseInputs) {
				in.HLLiveAll = []StrategyConfig{{ID: "hl-xyz", Platform: "hyperliquid", Type: "manual", Symbol: "xyz:TSLA", Args: []string{"hold", "x", "1h", "--mode=live"}}}
			}, want: "perp namespace the position reader does not cover"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var closes []string
				reader, _ := hlScriptedReader(nil, hlScriptedRead{positions: open}, hlScriptedRead{positions: nil})
				in := hlProofInputs(roster, hlReplyCloser(&closes, nil), reader)
				tc.wire(&in)
				plan := planKillSwitchClose(in)
				if plan.OnChainConfirmedFlat || plan.CanAutoResetWithoutOwner() {
					t.Fatalf("%s must keep the latch", tc.name)
				}
				if !strings.Contains(plan.DiscordMessage, tc.want) {
					t.Fatalf("message missing %q: %s", tc.want, plan.DiscordMessage)
				}
				if len(closes) != tc.calls {
					t.Fatalf("closer calls = %v, want %d", closes, tc.calls)
				}
				if tc.calls > 0 && killSwitchFlatListsHave(plan, "ETH") {
					t.Fatalf("an unverified coin must leave both flat lists: %+v", plan.CloseReport)
				}
			})
		}
	})

	t.Run("AcceptedZeroOrUnknownRepliesStillOpenKeepTheLatch", func(t *testing.T) {
		replies := map[string]func() (*HyperliquidCloseResult, error){
			"accepted order with no fill": func() (*HyperliquidCloseResult, error) {
				return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: "ETH", Fill: &HyperliquidCloseFill{}}, Platform: "hyperliquid"}, nil
			},
			"filled reply with zero size": func() (*HyperliquidCloseResult, error) {
				return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: "ETH", Fill: &HyperliquidCloseFill{TotalSz: 0, AvgPx: 3000}}, Platform: "hyperliquid"}, nil
			},
			"already-flat reply": func() (*HyperliquidCloseResult, error) {
				return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: "ETH", AlreadyFlat: true}, Platform: "hyperliquid"}, nil
			},
			"unknown reply": func() (*HyperliquidCloseResult, error) {
				return nil, fmt.Errorf("parse close output: unexpected end of JSON input")
			},
		}
		for name, reply := range replies {
			t.Run(name, func(t *testing.T) {
				open := []HLPosition{{Coin: "ETH", Size: 0.5}}
				reader, _ := hlScriptedReader(nil, hlScriptedRead{positions: open}, hlScriptedRead{positions: open})
				in := hlProofInputs(hlProofRoster("ETH"), hlReplyCloser(nil, map[string]func() (*HyperliquidCloseResult, error){"ETH": reply}), reader)
				in.HLStopLossOIDs = map[string][]int64{"ETH": {77}}
				plan := planKillSwitchClose(in)
				if plan.OnChainConfirmedFlat || killSwitchFlatListsHave(plan, "ETH") {
					t.Fatalf("a still-open ETH after %s must keep the latch: %+v", name, plan.CloseReport)
				}
				if _, ok := plan.CloseReport.Fills["ETH"]; ok {
					t.Fatalf("no usable fill may be offered for booking after %s: %+v", name, plan.CloseReport.Fills)
				}
				if !strings.Contains(plan.DiscordMessage, "protection unverified") {
					t.Fatalf("a requested trigger cancel with no acknowledgement must be called unverified: %s", plan.DiscordMessage)
				}
			})
		}
	})

	t.Run("ALaterCycleWithAFreshFlatReadClearsTheVenueHold", func(t *testing.T) {
		open := []HLPosition{{Coin: "ETH", Size: 0.5}}
		failing, _ := hlScriptedReader(nil, hlScriptedRead{positions: open}, hlScriptedRead{err: fmt.Errorf("hl 503")})
		first := planKillSwitchClose(hlProofInputs(hlProofRoster("ETH"), hlReplyCloser(nil, nil), failing))
		if first.OnChainConfirmedFlat {
			t.Fatal("an unreadable post-close account must keep the latch")
		}
		fresh, reads := hlScriptedReader(nil, hlScriptedRead{positions: nil})
		second := planKillSwitchClose(hlProofInputs(hlProofRoster("ETH"), hlReplyCloser(nil, nil), fresh))
		if *reads != 1 || !second.OnChainConfirmedFlat || !second.HLProof.Verified {
			t.Fatalf("the next cycle (or a restart that keeps the latch) must prove flat from its own read: reads=%d plan=%+v", *reads, second.HLProof)
		}
	})

	t.Run("NewLargerOppositeHedgeAndUnconfiguredRowsBlock", func(t *testing.T) {
		cases := []struct {
			name   string
			roster []StrategyConfig
			hedge  map[string]bool
			post   []HLPosition
			want   string
		}{
			{name: "new", roster: hlProofRoster("ETH", "BTC"), post: []HLPosition{{Coin: "BTC", Size: 0.1}}, want: "BTC long 0.1 appeared"},
			{name: "larger", roster: hlProofRoster("ETH"), post: []HLPosition{{Coin: "ETH", Size: 0.9}}, want: "ETH long 0.9 grew (pre-close long 0.5)"},
			{name: "opposite side", roster: hlProofRoster("ETH"), post: []HLPosition{{Coin: "ETH", Size: -0.2}}, want: "ETH short 0.2 changed side (pre-close long 0.5)"},
			{name: "held hedge leg", roster: hlProofRoster("ETH"), hedge: map[string]bool{"SOL": true}, post: []HLPosition{{Coin: "SOL", Size: -4}}, want: "SOL short 4 appeared"},
			{name: "unconfigured", roster: hlProofRoster("ETH"), post: []HLPosition{{Coin: "DOGE", Size: 100}}, want: "unconfigured coins (manual intervention required) — DOGE szi=100.000000"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				reader, _ := hlScriptedReader(nil, hlScriptedRead{positions: []HLPosition{{Coin: "ETH", Size: 0.5}}}, hlScriptedRead{positions: tc.post})
				in := hlProofInputs(tc.roster, hlReplyCloser(nil, nil), reader)
				in.HLHedgeCoins = tc.hedge
				plan := planKillSwitchClose(in)
				if plan.OnChainConfirmedFlat || plan.HLProof.Verified {
					t.Fatalf("%s row must block flat proof", tc.name)
				}
				if !strings.Contains(plan.DiscordMessage, tc.want) {
					t.Fatalf("message missing %q: %s", tc.want, plan.DiscordMessage)
				}
			})
		}
	})

	t.Run("ResetPromptLabelsTheStatusAsAnObservation", func(t *testing.T) {
		reader, _ := hlScriptedReader(nil, hlScriptedRead{positions: nil})
		plan := planKillSwitchClose(hlProofInputs(hlProofRoster("ETH"), hlReplyCloser(nil, nil), reader))
		latched := []RiskPartition{livePartition}
		single := formatKillSwitchResetPrompt("inst", "0xaddr", plan, livePartition, latched)
		paper := RiskPartition{Scope: ScopePaper}
		multi := formatKillSwitchResetPromptForScopes("inst", "0xaddr",
			map[RiskPartition]KillSwitchClosePlan{livePartition: plan, paper: {OnChainConfirmedFlat: true, DiscordMessage: "paper"}},
			[]RiskPartition{livePartition, paper}, []RiskPartition{livePartition, paper})
		for name, prompt := range map[string]string{"single": single, "multi": multi} {
			for _, want := range []string{"observation from the cycle that prepared this prompt (" + plan.ObservedAt.UTC().Format(time.RFC3339) + ")",
				"Verify current venue positions before you reply"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("%s prompt missing %q: %s", name, want, prompt)
				}
			}
		}
		if strings.Count(multi, "observation from the cycle") != 1 {
			t.Errorf("only the live section carries the venue observation note: %s", multi)
		}
	})
}

func TestKillSwitchResidualContract(t *testing.T) {
	routes := func(r killSwitchExposureRow) bool { return r.Instrument == "BTC-USDT-SWAP" }
	t.Run("OppositeLegsNeverCancel", func(t *testing.T) {
		post := []killSwitchExposureRow{
			{Instrument: "BTC-USDT-SWAP", Leg: "long/cross", Coin: "BTC", Size: 2},
			{Instrument: "BTC-USDT-SWAP", Leg: "short/cross", Coin: "BTC", Size: -2},
		}
		res := evaluateKillSwitchResidual(post, post, routes)
		if res.flat() || len(res.ByCoin["BTC"]) != 2 {
			t.Fatalf("two opposite legs must both stay visible, got %+v", res)
		}
	})
	t.Run("SameBaseCoinOnAnotherInstrumentIsOutsideTheRoute", func(t *testing.T) {
		res := evaluateKillSwitchResidual(nil, []killSwitchExposureRow{{Instrument: "BTC-USD-SWAP", Leg: "net/cross", Coin: "BTC", Size: 5}}, routes)
		if res.flat() || len(res.Outside) != 1 || len(res.ByCoin) != 0 {
			t.Fatalf("a coin-margined BTC swap must be reported outside the close route, got %+v", res)
		}
	})
	t.Run("ZeroRowsAreFlat", func(t *testing.T) {
		res := evaluateKillSwitchResidual(nil, []killSwitchExposureRow{{Instrument: "BTC-USDT-SWAP", Leg: "net/cross", Coin: "BTC", Size: 0}}, routes)
		if !res.flat() {
			t.Fatalf("zero rows must read as flat, got %+v", res)
		}
	})
	t.Run("OKXPlanKeepsUnverifiedCoinsOutOfSettledCleanup", func(t *testing.T) {
		const acct = "okx:swap:live:key-0000000000000000"
		row := func(inst, side string, pos float64) okxStrictPositionRow {
			coin, _, _ := strings.Cut(inst, "-")
			return okxStrictPositionRow{InstID: inst, Coin: coin, PosSide: side, MarginMode: "cross", Contracts: pos}
		}
		reads := []okxStrictPositionSnapshot{
			{Account: acct, Rows: []okxStrictPositionRow{row("BTC-USDT-SWAP", "long", 2), row("BTC-USDT-SWAP", "short", -2), row("ETH-USDT-SWAP", "net", 1)}},
			{Account: acct, Rows: []okxStrictPositionRow{row("BTC-USDT-SWAP", "short", -2)}},
		}
		calls := 0
		var closes []string
		in := KillSwitchCloseInputs{
			OKXLiveAllPerps: []StrategyConfig{
				{ID: "okx-btc", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
				{ID: "okx-eth", Platform: "okx", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
			},
			OKXAccount: acct,
			OKXReader: func(ctx context.Context, target okxPositionTarget) (okxStrictPositionSnapshot, error) {
				snap := reads[min(calls, len(reads)-1)]
				calls++
				return snap, nil
			},
			OKXCloser: func(symbol string, partialSz *float64) (*OKXCloseResult, error) {
				closes = append(closes, symbol)
				return &OKXCloseResult{Close: &OKXClose{Symbol: symbol}, Platform: "okx"}, nil
			},
			CloseTimeout: time.Second,
		}
		plan := planKillSwitchClose(in)
		if strings.Join(closes, ",") != "BTC,ETH" || calls != 2 {
			t.Fatalf("closes = %v reads = %d, want one close per routed coin and one read after the last close", closes, calls)
		}
		if plan.OnChainConfirmedFlat || plan.OKXProof.Verified {
			t.Fatal("a remaining OKX short leg must keep the latch")
		}
		settled := settledKillSwitchSymbols(&plan)["okx"]
		if settled["BTC"] || !settled["ETH"] {
			t.Fatalf("only the verified ETH close may settle books while latched, settled = %v", settled)
		}

		calls = 0
		in.OKXReader = func(ctx context.Context, target okxPositionTarget) (okxStrictPositionSnapshot, error) {
			calls++
			return okxStrictPositionSnapshot{Account: "okx:swap:sandbox:key-0000000000000000"}, nil
		}
		closes = nil
		plan = planKillSwitchClose(in)
		if plan.OnChainConfirmedFlat || len(closes) != 0 || !strings.Contains(plan.DiscordMessage, "not the close target") {
			t.Fatalf("an account or network mismatch must hold before any close: closes=%v msg=%s", closes, plan.DiscordMessage)
		}
	})
}
