package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

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

func TestPlanKillSwitchClose_DeclaredButFlatHedgeCoinStaysUnowned(t *testing.T) {
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
	in.HLHedgeCoins = map[string]bool{}
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

func TestApplyKillSwitchSettledLegsWhileLatched_BooksConfirmedHLFill(t *testing.T) {
	cfgs := []StrategyConfig{
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	state := &StrategyState{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1.0, AvgCost: 2000, Side: "long", Multiplier: 1},
		},
	}
	strategies := map[string]*StrategyState{"hl-eth": state}
	plan := KillSwitchClosePlan{OnChainConfirmedFlat: false}
	plan.CloseReport.Fills = map[string]HyperliquidCloseFill{
		"ETH": {TotalSz: 1.0, AvgPx: 2100, Fee: 0.5, OID: 555},
	}
	virtualQty := snapshotHyperliquidVirtualQuantities(strategies, cfgs)

	applyKillSwitchSettledLegsWhileLatched(strategies, cfgs, &plan, cfgs, virtualQty,
		map[string]float64{"ETH": 2100}, nil)

	if len(state.Positions) != 0 {
		t.Fatalf("positions after apply = %+v; want flat", state.Positions)
	}
	var booked *Trade
	for i := range state.TradeHistory {
		tr := &state.TradeHistory[i]
		if tr.ExchangeOrderID == "555" {
			booked = tr
		}
	}
	if booked == nil {
		t.Fatal("confirmed fill must book with its real OID while latched")
	}
	if math.Abs(booked.RealizedPnL-100) > 1e-6 || math.Abs(booked.ExchangeFee-0.5) > 1e-9 {
		t.Errorf("booked fill = PnL %.4f fee %.4f; want 100 / 0.5", booked.RealizedPnL, booked.ExchangeFee)
	}

	before := len(state.TradeHistory)
	applyKillSwitchSettledLegsWhileLatched(strategies, cfgs, &plan, cfgs, virtualQty,
		map[string]float64{"ETH": 2100}, nil)
	if len(state.TradeHistory) != before {
		t.Errorf("same-OID re-application must not double-book: %d -> %d rows", before, len(state.TradeHistory))
	}
}

func TestApplyKillSwitchSettledLegsWhileLatched_UnsettledLegStays(t *testing.T) {
	cfgs := []StrategyConfig{
		{ID: "hl-sol", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "SOL", "1h", "--mode=live"}},
	}
	state := &StrategyState{
		ID: "hl-sol", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"SOL": {Symbol: "SOL", Quantity: 2.0, AvgCost: 100, Side: "long", Multiplier: 1},
		},
	}
	strategies := map[string]*StrategyState{"hl-sol": state}
	plan := KillSwitchClosePlan{OnChainConfirmedFlat: false}

	applyKillSwitchSettledLegsWhileLatched(strategies, cfgs, &plan, cfgs, nil,
		map[string]float64{"SOL": 90}, nil)

	pos := state.Positions["SOL"]
	if pos == nil || math.Abs(pos.Quantity-2.0) > 1e-9 {
		t.Fatalf("unsettled leg must stay untouched for the retry, got %+v", state.Positions)
	}
	if len(state.TradeHistory) != 0 {
		t.Errorf("no trade may book over an unsettled live position, got %+v", state.TradeHistory)
	}
}

func TestForceCloseHyperliquidLive_ManualDivergentArgsStaysInCloseScope(t *testing.T) {
	roster := []StrategyConfig{
		{ID: "hl-manual-eth-live", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
			Args: []string{"hold", "BTC", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 2.0, EntryPrice: 2000}}
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		return &HyperliquidCloseResult{
			Close: &HyperliquidClose{Symbol: symbol,
				Fill: &HyperliquidCloseFill{TotalSz: 2.0, AvgPx: 2100, Fee: 0.5, OID: 777}},
			Platform: "hyperliquid",
		}, nil
	}
	fetcher, _ := stubHLStateFetcher(nil, nil)
	in := defaultHLInputs("0xaddr", true, positions, roster,
		"portfolio drawdown 25.0% exceeds limit 20.0%",
		time.Second, closer, fetcher)
	plan := planKillSwitchClose(in)

	if !plan.OnChainConfirmedFlat {
		t.Fatalf("expected confirmed-flat plan; Unconfigured=%+v errors=%+v", plan.Unconfigured, plan.CloseReport.Errors)
	}
	fill, ok := plan.CloseReport.Fills["ETH"]
	if !ok || fill.TotalSz != 2.0 {
		t.Fatalf("fills = %+v; want ETH booked under the raw configured symbol", plan.CloseReport.Fills)
	}

	state := &StrategyState{
		ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 2.0, AvgCost: 2000, Side: "long", Multiplier: 1},
		},
	}
	virtualQty := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{"hl-manual-eth-live": state}, roster)
	forceCloseKillSwitchPositions(state, roster[0], map[string]float64{"ETH": 2100}, plan.CloseReport.Fills, roster, virtualQty, nil)
	if len(state.Positions) != 0 {
		t.Fatalf("positions after apply = %+v; want flat", state.Positions)
	}
	var booked *Trade
	for i := range state.TradeHistory {
		tr := &state.TradeHistory[i]
		if tr.ExchangeOrderID != "" {
			booked = tr
		}
	}
	if booked == nil {
		t.Fatal("manual leg must book the REAL exchange fill (OID), not fall back to model-only")
	}
}
