package main

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// #1454 regression tests: live type=manual strategies join the kill-switch
// close scope, fill booking, virtual-quantity split, resting-trigger cancel,
// and unconfigured-coin detection. Before this fix a mixed fleet skipped
// manual-only coins silently and booked model-only rows for them even when a
// real exchange fill existed.

func TestPlanKillSwitchClose_ManualOnlyCoinClosedAndBooked(t *testing.T) {
	roster := []StrategyConfig{
		{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
			Args: []string{"hold", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 2.0, EntryPrice: 3000}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close: &HyperliquidClose{Symbol: symbol,
				Fill: &HyperliquidCloseFill{TotalSz: 2.0, AvgPx: 2900, Fee: 3.0, OID: 499735101008}},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, roster,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher))
	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed-flat plan for a closed manual coin, got %+v", plan)
	}

	s := &StrategyState{
		ID:       "hl-manual-eth-live",
		Type:     "manual",
		Platform: "hyperliquid",
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 2.0, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	forceCloseKillSwitchPositions(s, roster[0], map[string]float64{"ETH": 2800}, plan.CloseReport.Fills, roster, nil, nil)

	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected exactly 1 trade (the real-fill close), got %d", len(s.TradeHistory))
	}
	trade := s.TradeHistory[0]
	if trade.ExchangeOrderID != "499735101008" {
		t.Errorf("Trade.ExchangeOrderID = %q; want real fill OID", trade.ExchangeOrderID)
	}
	if trade.FeeSource != FeeSourceUserFills {
		t.Errorf("FeeSource = %q; want %q", trade.FeeSource, FeeSourceUserFills)
	}
	if math.Abs(trade.Price-2900) > 1e-9 {
		t.Errorf("Trade.Price = %.2f; want fill AvgPx 2900, not mark 2800", trade.Price)
	}
	wantGross := 2.0 * (2900 - 3000)
	if math.Abs(trade.RealizedPnL-wantGross) > 1e-9 {
		t.Errorf("Trade.RealizedPnL = %.4f; want %.4f (fill-derived, gross)", trade.RealizedPnL, wantGross)
	}
	if strings.Contains(trade.Details, "model-only") {
		t.Errorf("trade must not be a model-only row: %s", trade.Details)
	}
	wantNet := wantGross - 3.0
	if len(s.ClosedPositions) != 1 {
		t.Fatalf("expected one closed position, got %+v", s.ClosedPositions)
	}
	if math.Abs(s.ClosedPositions[0].ClosePrice-2900) > 1e-9 || math.Abs(s.ClosedPositions[0].RealizedPnL-wantNet) > 1e-9 {
		t.Errorf("ClosedPosition = price %.2f pnl %.4f; want 2900 / %.4f",
			s.ClosedPositions[0].ClosePrice, s.ClosedPositions[0].RealizedPnL, wantNet)
	}
	if len(s.Positions) != 0 {
		t.Errorf("virtual position must be deleted, still has %v", s.Positions)
	}
}

func TestPlanKillSwitchClose_ManualOnlyCoinCloseFailureLatches(t *testing.T) {
	roster := []StrategyConfig{
		{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
			Args: []string{"hold", "ETH", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 2.0, EntryPrice: 3000}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return nil, fmt.Errorf("simulated HL close failure")
	}
	// Post-close verification fetch must still show the position open — the
	// closer failed, so nothing closed it on-chain.
	fetcher, _ := stubHLStateFetcher(positions, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, roster,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher))

	if plan.OnChainConfirmedFlat {
		t.Fatal("a failed close on a manual-held coin must latch (OnChainConfirmedFlat=false)")
	}
	if _, ok := plan.CloseReport.Errors["ETH"]; !ok {
		t.Fatalf("expected Errors[ETH], got %+v", plan.CloseReport.Errors)
	}
	joined := strings.Join(plan.LogLines, "\n")
	if !strings.Contains(joined, "[CRITICAL] hl-close: ETH failed") {
		t.Errorf("missing CRITICAL failure line, got: %s", joined)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED") {
		t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
	}
}

func TestPlanKillSwitchClose_MixedFleetUnconfiguredCoinBlocksFlat(t *testing.T) {
	roster := []StrategyConfig{
		{ID: "hl-perps-btc-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "BTC", Size: 1.0}, {Coin: "DOGE", Size: 5000}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close: &HyperliquidClose{Symbol: symbol,
				Fill: &HyperliquidCloseFill{TotalSz: 1.0, AvgPx: 50000, Fee: 1.0}},
			Platform: "hyperliquid",
		}, nil
	}
	// Post-close verification fetch must still show the position open — the
	// closer failed, so nothing closed it on-chain.
	fetcher, _ := stubHLStateFetcher(positions, nil)

	plan := planKillSwitchClose(defaultHLInputs("0xaddr", true, positions, roster,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher))

	if plan.OnChainConfirmedFlat {
		t.Fatal("an on-chain position no configured strategy owns must block flat confirmation even on a mixed fleet")
	}
	if len(plan.Unconfigured) != 1 || plan.Unconfigured[0].Coin != "DOGE" {
		t.Errorf("Unconfigured = %+v; want [DOGE]", plan.Unconfigured)
	}
	joined := strings.Join(plan.LogLines, "\n")
	if !strings.Contains(joined, "unconfigured coin DOGE") {
		t.Errorf("missing CRITICAL unconfigured-coin line, got: %s", joined)
	}
	if !strings.Contains(plan.DiscordMessage, "LATCHED") {
		t.Errorf("expected LATCHED message, got: %s", plan.DiscordMessage)
	}
}

func TestHyperliquidKillSwitchShareSplit_PerpsAndManualPeers(t *testing.T) {
	roster := []StrategyConfig{
		{ID: "hl-perps-eth-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
			Args: []string{"hold", "ETH", "1h", "--mode=live"}},
	}
	states := map[string]*StrategyState{
		"hl-perps-eth-live": {ID: "hl-perps-eth-live", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1.5, Side: "long"},
		}},
		"hl-manual-eth-live": {ID: "hl-manual-eth-live", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.5, Side: "long"},
		}},
	}
	snap := snapshotHyperliquidVirtualQuantities(states, roster)
	if snap == nil || snap["ETH"]["hl-perps-eth-live"] != 1.5 || snap["ETH"]["hl-manual-eth-live"] != 0.5 {
		t.Fatalf("virtual snapshot must include BOTH peers' quantities, got %+v", snap)
	}

	szPerps, feePerps := hyperliquidKillSwitchFillShare(roster[0], "ETH", 2.0, 10.0, roster, snap)
	if math.Abs(szPerps-1.5) > 1e-9 || math.Abs(feePerps-7.5) > 1e-9 {
		t.Errorf("perps share = (%.6f, %.6f); want (1.5, 7.5)", szPerps, feePerps)
	}
	szMann, feeMan := hyperliquidKillSwitchFillShare(roster[1], "ETH", 2.0, 10.0, roster, snap)
	if math.Abs(szMann-0.5) > 1e-9 || math.Abs(feeMan-2.5) > 1e-9 {
		t.Errorf("manual share = (%.6f, %.6f); want (0.5, 2.5)", szMann, feeMan)
	}
}

func TestCollectHLKillSwitchStopOIDs_IncludesManualTriggers(t *testing.T) {
	strategies := map[string]*StrategyState{
		"hl-manual-eth-live": {Positions: map[string]*Position{
			"ETH": {StopLossOID: 111, TPOIDs: []int64{222}},
		}},
		"hl-perps-btc-live": {Positions: map[string]*Position{
			"BTC": {StopLossOID: 333},
		}},
	}
	roster := []StrategyConfig{
		{ID: "hl-perps-btc-live", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}},
		{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
			Args: []string{"hold", "ETH", "1h", "--mode=live"}},
	}
	out := collectHLKillSwitchStopOIDs(strategies, roster)
	if !reflect.DeepEqual(out["BTC"], []int64{333}) {
		t.Errorf("BTC OIDs = %v; want [333]", out["BTC"])
	}
	if !reflect.DeepEqual(out["ETH"], []int64{111, 222}) {
		t.Errorf("ETH OIDs = %v; want [111 222] — a manual coin's resting triggers must be cancelled before the flatten", out["ETH"])
	}
}

func TestCollectHLKillSwitchStopOIDs_LowerCaseManualSymbolFallsBackToRawKey(t *testing.T) {
	strategies := map[string]*StrategyState{
		"hl-manual-sol-live": {Positions: map[string]*Position{
			"sol": {StopLossOID: 444}, // operator-entered lower-case symbol
		}},
	}
	roster := []StrategyConfig{
		{ID: "hl-manual-sol-live", Platform: "hyperliquid", Type: "manual", Symbol: "sol",
			Args: []string{"hold", "sol", "1h", "--mode=live"}},
	}
	out := collectHLKillSwitchStopOIDs(strategies, roster)
	// RAW key: the stop-OID map is consumed by forceCloseHyperliquidLive's
	// raw on-chain p.Coin lookup.
	if !reflect.DeepEqual(out["sol"], []int64{444}) {
		t.Errorf("sol OIDs = %v; want [444] under the raw configured symbol", out["sol"])
	}
}

func TestModelOnlyCloseAlert_ThrottledPerStrategySymbol(t *testing.T) {
	tracker := &modelOnlyCloseTracker{last: make(map[modelOnlyCloseKey]time.Time)}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	if !tracker.shouldNotify("s-a", "ETH", now) {
		t.Fatal("first model-only row for a key must notify")
	}
	if tracker.shouldNotify("s-a", "ETH", now.Add(time.Minute)) {
		t.Error("second row inside the window must be throttled")
	}
	if !tracker.shouldNotify("s-a", "BTC", now.Add(time.Minute)) {
		t.Error("an independent (strategy, symbol) key must not be throttled by another key's slot")
	}
	if !tracker.shouldNotify("s-b", "ETH", now.Add(time.Minute)) {
		t.Error("a second strategy's row must not inherit the first strategy's throttle slot")
	}
	if !tracker.shouldNotify("s-a", "ETH", now.Add(effectiveAlertThrottleInterval()+time.Second)) {
		t.Error("a row after the throttle window must notify again")
	}
}

func TestQueueModelOnlyCloseAlert_DrainsAndSkipsEmpty(t *testing.T) {
	drainModelOnlyCloseAlerts() // clear any residue from other tests
	queueModelOnlyCloseAlert("", "ETH", 1.0)
	queueModelOnlyCloseAlert("strat", "", 1.0)
	if got := drainModelOnlyCloseAlerts(); len(got) != 0 {
		t.Fatalf("empty strategy/symbol must not queue, got %+v", got)
	}

	// Direct queue path with fresh keys (the shared global throttle may already
	// hold slots from other tests — use unique IDs).
	id := "queue-drain-test-strat"
	queueModelOnlyCloseAlert(id, "UNIQ", 2.5)
	got := drainModelOnlyCloseAlerts()
	found := false
	for _, a := range got {
		if a.StrategyID == id && a.Symbol == "UNIQ" && math.Abs(a.Quantity-2.5) < 1e-9 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected queued alert for %s/UNIQ, got %+v", id, got)
	}
	if again := drainModelOnlyCloseAlerts(); len(again) != 0 {
		t.Fatalf("drain must empty the queue, got %+v", again)
	}
}

func TestPlanKillSwitchClose_DeclaredButFlatHedgeCoinStaysUnowned(t *testing.T) {
	// #1159 invariant under the #1454 roster: a coin a strategy merely DECLARES
	// as its hedge — with no held leg — may carry a genuinely foreign position,
	// so it must stay outside the close scope even though the roster now spans
	// perps+manual. It must, however, be REPORTED as unowned rather than
	// silently skipped.
	roster := []StrategyConfig{
		{ID: "hl-perps-eth-live", Platform: "hyperliquid", Type: "perps",
			Args:  []string{"sma", "ETH", "1h", "--mode=live"},
			Hedge: &HedgeConfig{Enabled: true, Symbol: "SOL"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 1.0}, {Coin: "SOL", Size: 40}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close: &HyperliquidClose{Symbol: symbol,
				Fill: &HyperliquidCloseFill{TotalSz: 1.0, AvgPx: 3000, Fee: 1.0}},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)

	in := defaultHLInputs("0xaddr", true, positions, roster,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher)
	in.HLHedgeCoins = map[string]bool{} // nothing HELD — SOL is declared-but-flat
	plan := planKillSwitchClose(in)

	if plan.OnChainConfirmedFlat {
		t.Fatal("a foreign position on a declared-but-flat hedge coin must block flat confirmation")
	}
	closed := false
	for _, c := range plan.CloseReport.ClosedCoins {
		if c == "SOL" {
			closed = true
		}
	}
	if closed {
		t.Error("declared-but-flat hedge coin must NOT be adopted into the close scope")
	}
	if len(plan.Unconfigured) != 1 || plan.Unconfigured[0].Coin != "SOL" {
		t.Errorf("Unconfigured = %+v; want [SOL]", plan.Unconfigured)
	}
}
