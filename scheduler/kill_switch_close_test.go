package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Tests for planKillSwitchClose — the orchestration seam for #341 / #345.
// Covers the "latch until flat" wiring that is the actual fix. Without
// them, the load-bearing `killSwitchFired && plan.OnChainConfirmedFlat`
// guard around forceCloseAllPositions could regress silently — exactly
// the shape of the original #341 bug (virtual state mutated without
// confirming on-chain closure).

// stubHLLiveCloser returns a HyperliquidLiveCloser that records every invocation
// and maps coin → canned error. Missing keys yield a synthetic success.
func stubHLLiveCloser(errs map[string]error) (HyperliquidLiveCloser, *[]string) {
	closer, calls, _ := stubHLLiveCloserWithCancel(errs)
	return closer, calls
}

// stubHLLiveCloserWithCancel mirrors stubHLLiveCloser but also surfaces the
// per-call cancelStopLossOIDs so #421 tests can assert the kill-switch /
// CB-drain plumbing actually threads the OID through. cancels[symbol]
// holds the OIDs seen for that coin.
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

// stubHLStateFetcher returns an HLStateFetcher that replays a fixed response list
// and records invocation count. errOnce > 0 means the Nth call errors.
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

// stubOKXLiveCloser mirrors stubHLLiveCloser for OKX. Used in tests that
// want to ensure the OKX path isn't invoked — the default (empty errs) is
// a synthetic success that should never be triggered in HL-only tests.
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

// stubOKXPositionsFetcher returns an OKXPositionsFetcher that replays a
// fixed response and records invocation count.
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

// defaultHLInputs builds a KillSwitchCloseInputs for an HL-only test. Any
// OKX fields are zeroed — the OKX plan branch only fires when
// OKXLiveAllPerps is non-empty, so these tests stay HL-exclusive.
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

// Happy path: HL configured, on-chain state already fetched, one live strategy
// with an open position. Plan reports ConfirmedFlat, closer called once,
// Discord message is the success shape. This is the test that locks in the
// main.go gate: if plan.OnChainConfirmedFlat regresses to false here, the
// kill switch stops clearing virtual state even when the exchange confirmed
// the close.
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

// Close failure: closer errors for one coin. Plan must NOT be ConfirmedFlat
// — caller must keep virtual state intact and retry next cycle.
func TestPlanKillSwitchClose_CloseError(t *testing.T) {
	hlLive := []StrategyConfig{
		{ID: "hl-ema-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.5}}
	closer, _ := stubHLLiveCloser(map[string]error{"ETH": fmt.Errorf("hl rate limited")})
	fetcher, _ := stubHLStateFetcher(nil, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, hlLive,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher))

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

// Opportunistic fetch: HL configured but main.go didn't fetch state this
// cycle. planKillSwitchClose must re-fetch — otherwise the kill switch
// reports "no live HL exposure" without checking.
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

// Opportunistic fetch failure: HL configured, fetch errors. Plan must NOT be
// ConfirmedFlat — we cannot verify on-chain state, so caller must not clear
// virtual state.
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

// False-reassurance case: HL configured but no live HL strategies are
// configured, yet the wallet still has on-chain positions. planKillSwitchClose
// must fetch state, detect the positions, block virtual state mutation, and
// surface them in the Discord message.
func TestPlanKillSwitchClose_UnconfiguredPositionBlocksReset(t *testing.T) {
	positions := []HLPosition{{Coin: "ETH", Size: 0.517}}
	closer, calls := stubHLLiveCloser(nil)
	fetcher, _ := stubHLStateFetcher(positions, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", false, nil,
		[]StrategyConfig{}, // NO live HL strategies configured
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

// No HL at all: hlAddr="" and no live HL strategies. Kill switch should
// proceed normally (ConfirmedFlat=true) so spot/options/futures-only users
// don't regress.
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

// Stable error ordering (bot review #3): when multiple coins fail with the
// same errors, the Discord message must be byte-identical across calls.
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
		fetcher, _ := stubHLStateFetcher(nil, nil)
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

// Not-killSwitchFired guard: the caller only invokes planKillSwitchClose
// when killSwitchFired==true, but we still verify that a pure-data call
// with all-zero inputs returns a sensible default — specifically
// OnChainConfirmedFlat=true so a mistaken invocation from a future
// refactor wouldn't spuriously latch.
func TestPlanKillSwitchClose_ZeroInputsAreSafe(t *testing.T) {
	closer, _ := stubHLLiveCloser(nil)
	fetcher, _ := stubHLStateFetcher(nil, nil)
	plan := planKillSwitchClose(defaultHLInputs("", false, nil, nil, "", time.Second, closer, fetcher))
	if !plan.OnChainConfirmedFlat {
		t.Errorf("zero inputs should yield ConfirmedFlat=true, got %+v", plan)
	}
}

// ── OKX tests (#345) ───────────────────────────────────────────────────

// OKX happy path: one live OKX perps strategy with an open position. Plan
// reports ConfirmedFlat, closer called once, Discord message mentions OKX
// closes. This is the #345 analog of TestPlanKillSwitchClose_HappyPath.
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

// OKX close failure: closer errors, plan must latch. Mirrors the HL close-
// error test — this is the load-bearing #345 correctness case. Without
// this, a silent OKX close failure would clear virtual state while on-chain
// exposure remained (the exact #341/#345 bug class).
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

// OKX fetch failure: fetcher errors → kill switch must latch, closer
// must NOT be invoked (we don't know which coins to close). Same guard
// semantic as TestPlanKillSwitchClose_FetchFailure.
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

// OKX spot strategy present: surface as unhandled gap. Does NOT block
// ConfirmedFlat (we have no reliable way to check spot balances — a hard
// latch would freeze the scheduler forever for any OKX spot user).
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

	// Spot gap alone does NOT latch — the Discord message surfaces it.
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

// HL + OKX combined: both platforms have successful closes. Plan is
// ConfirmedFlat, message lists both. Verifies the two platforms compose
// rather than clobber each other's report.
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

// Either platform failing latches the switch: HL succeeds, OKX fails.
// Critical correctness test — without it, OKX-side failures would be
// hidden behind an HL-side success (exactly the #345 bug class).
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

// OKX unconfigured: fetcher reports a position for a coin no live perps
// strategy trades. Kill switch refuses to liquidate and latches.
func TestPlanKillSwitchClose_OKXUnconfiguredBlocksReset(t *testing.T) {
	okxLive := []StrategyConfig{
		{ID: "okx-btc", Platform: "okx", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []OKXPosition{
		{Coin: "BTC", Size: 0.01, Side: "long"},
		{Coin: "SOL", Size: 100, Side: "long"}, // not configured
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
	// BTC should still be closed (it's configured).
	if len(*calls) != 1 || (*calls)[0] != "BTC" {
		t.Errorf("closer calls = %v, want [BTC]", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "manual intervention required") {
		t.Errorf("expected manual intervention note, got: %s", plan.DiscordMessage)
	}
}

// ── Robinhood tests (#346) ─────────────────────────────────────────────

// stubRHLiveCloser mirrors stubHLLiveCloser / stubOKXLiveCloser for
// Robinhood. Missing-coin entries yield a synthetic success.
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

// stubRHPositionsFetcher returns a RobinhoodPositionsFetcher that replays a
// fixed response and records invocation count.
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

// Robinhood happy path: one live Robinhood crypto strategy with an open
// position. Plan reports ConfirmedFlat, closer called once, Discord
// message mentions Robinhood closes. The #346 analog of HL/OKX happy-path.
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

// Robinhood close failure: closer errors, plan must latch. Mirrors the
// HL/OKX close-error tests — this is the load-bearing #346 correctness
// case. Without it, a silent Robinhood close failure would clear virtual
// state while on-account exposure remained.
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

// Robinhood fetch failure: fetcher errors → kill switch must latch,
// closer must NOT be invoked (we don't know which coins to close). Same
// guard semantic as HL/OKX fetch failure.
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

// Robinhood options strategy present: surface as unhandled gap. Does NOT
// block ConfirmedFlat (hard-latch would freeze the scheduler forever for
// any Robinhood options user). Mirrors OKX spot semantic.
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

// Robinhood unconfigured: fetcher reports a balance for a coin no live
// crypto strategy trades. Kill switch refuses to liquidate and latches.
func TestPlanKillSwitchClose_RobinhoodUnconfiguredBlocksReset(t *testing.T) {
	rhLive := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
			Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}},
	}
	positions := []RobinhoodPosition{
		{Coin: "BTC", Size: 0.01},
		{Coin: "DOGE", Size: 100}, // unconfigured
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
	// BTC should still be closed (it's configured).
	if len(*calls) != 1 || (*calls)[0] != "BTC" {
		t.Errorf("closer calls = %v, want [BTC]", *calls)
	}
	if !strings.Contains(plan.DiscordMessage, "manual intervention required") {
		t.Errorf("expected manual intervention note, got: %s", plan.DiscordMessage)
	}
}

// Combined: HL + OKX + Robinhood all succeeding. Plan ConfirmedFlat,
// message lists all three.
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

// Any platform failing latches the switch: HL + OKX succeed, Robinhood
// fails. Without this test a silent Robinhood failure could be hidden
// behind the other platforms' successes (the #346 bug class).
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

// Robinhood deterministic error ordering: multiple failing coins produce
// a stable message. Mirrors HL/OKX determinism tests.
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

// OKX deterministic error ordering: multiple failing coins produce a stable
// message. Mirrors TestPlanKillSwitchClose_DeterministicErrorOrder for the
// OKX path — Go map iteration randomization would otherwise produce
// flaky messages for identical failures.
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

// ── TopStep tests (#347) ───────────────────────────────────────────────

// stubTSLiveCloser mirrors stubHLLiveCloser / stubOKXLiveCloser / stubRHLiveCloser
// for TopStep. Missing-symbol entries yield a synthetic success.
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

// stubTSPositionsFetcher returns a TopStepPositionsFetcher that replays a
// fixed response and records invocation count.
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

// TopStep happy path: one live futures strategy with an open ES position.
// Plan reports ConfirmedFlat, closer called once, Discord message mentions
// TopStep closes. The #347 analog of HL/OKX/Robinhood happy-path.
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

// TopStep close failure: the load-bearing #347 correctness case. Without
// latching on a failed close, virtual state would clear while CME exposure
// remained — exactly the #341 bug shape.
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

// TopStep fetch failure: fetcher errors → kill switch must latch, closer
// must NOT be invoked. Same guard as HL/OKX/Robinhood fetch failure. This
// branch also covers the CME-closed-hours case when the fetch itself can't
// reach TopStepX (credentials, auth token expired, etc).
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

// TopStep unconfigured: fetcher reports a position for a symbol no live
// futures strategy trades. Kill switch refuses to liquidate and latches.
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

// Cross-platform latch: HL happy but TopStep fails — plan must still
// latch across the board. Mirrors the existing
// RobinhoodFailureStillLatchesAcrossPlatforms test. Proves either
// platform flipping ConfirmedFlat=false cascades correctly.
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

// Per-platform CloseTimeout overrides take precedence over the
// CloseTimeout fallback. Without this, RH (TOTP-slow) and HL (fast) would
// share a single budget that could not be tuned independently. (#350)
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

// HL fetcher unwired while HLAddr configured: must latch with a CRITICAL
// log line. Defense-in-depth against a future main.go regression that
// drops HLFetcher; without this, the kill switch would silently bypass
// HL exposure. (#350)
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

// OKX fetcher unwired while strategies configured: must latch. Without
// the else branch, len(strategies)>0 && fetcher==nil silently skipped
// OKX and cleared OnChainConfirmedFlat=true. (#350)
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

// RH fetcher unwired while strategies configured: must latch. (#350)
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

// TS fetcher unwired while strategies configured: must latch. (#350)
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
