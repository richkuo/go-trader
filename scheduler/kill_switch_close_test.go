package main

import (
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
