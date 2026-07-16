package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func hedgeTestStrategy(primary, hedge string) StrategyConfig {
	return StrategyConfig{ID: "alpha", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", primary, "paper"}, Hedge: &HedgeConfig{Enabled: true, Symbol: hedge, Side: "inverse", Ratio: 1, MarginMode: "cross", Leverage: 3}}
}

func TestHedgeTargetDecisionInverseNotionalSizing(t *testing.T) {
	sc := hedgeTestStrategy("ETH", "BTC")
	for _, tc := range []struct{ primarySide, wantSide string }{{"long", "short"}, {"short", "long"}} {
		a := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 2, PrimarySide: tc.primarySide}, 2500, 50000)
		if a.Kind != hedgeActionOpen || a.Side != tc.wantSide || mathAbs(a.Qty-0.1) > 1e-12 {
			t.Fatalf("%s: got %+v, want open %s qty=.1", tc.primarySide, a, tc.wantSide)
		}
	}
}

func TestHedgeTargetDecisionQuantityWatermarkPreventsMarkChurn(t *testing.T) {
	sc := hedgeTestStrategy("ETH", "BTC")
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: .1, HedgeSide: "short", HedgeBasis: 2}
	if got := hedgeTargetDecision(sc, snap, 4000, 30000); got.Kind != hedgeActionNone {
		t.Fatalf("marks alone caused hedge trade: %+v", got)
	}
	add := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 3, PrimarySide: "long", HedgeQty: .1, HedgeSide: "short", HedgeBasis: 2}, 2500, 50000)
	if add.Kind != hedgeActionAdd || mathAbs(add.Qty-.05) > 1e-12 {
		t.Fatalf("add decision=%+v", add)
	}
}

func TestHedgeTargetDecisionPartialAndFullClose(t *testing.T) {
	sc := hedgeTestStrategy("ETH", "BTC")
	reduce := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: .2, HedgeSide: "short", HedgeBasis: 2}, 2500, 50000)
	if reduce.Kind != hedgeActionReduce || mathAbs(reduce.Qty-.1) > 1e-12 || reduce.Side != "buy" {
		t.Fatalf("reduce=%+v", reduce)
	}
	close := hedgeTargetDecision(sc, hedgeSnapshot{HedgeQty: .2, HedgeSide: "short"}, 0, 50000)
	if close.Kind != hedgeActionClose || close.Qty != .2 || close.Side != "buy" {
		t.Fatalf("close=%+v", close)
	}
}

func TestHedgeTargetDecisionExternallyMissingLegFailsClosed(t *testing.T) {
	sc := hedgeTestStrategy("ETH", "BTC")
	a := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", PrimaryHedgeSymbol: "BTC"}, 2500, 50000)
	if a.Kind != hedgeActionOpen || a.Qty != 0 || !strings.Contains(a.Reason, "disappeared") {
		t.Fatalf("external hedge loss must route to fail-closed unwind, got %+v", a)
	}
}

func TestValidateHedgeConfigsRejectsCollisionMatrix(t *testing.T) {
	primary := hedgeTestStrategy("ETH", "ETH")
	peer := StrategyConfig{ID: "peer", Type: "manual", Platform: "hyperliquid", Symbol: "BTC"}
	primary.Hedge.Symbol = "BTC/USDC:USDC"
	errs := validateHedgeConfigs([]StrategyConfig{primary, peer})
	if joined := strings.Join(errs, "\n"); !strings.Contains(joined, "configured strategy coin") {
		t.Fatalf("peer collision not rejected: %v", errs)
	}
	a := hedgeTestStrategy("ETH", "BTC")
	b := hedgeTestStrategy("SOL", "BTC")
	b.ID = "beta"
	if joined := strings.Join(validateHedgeConfigs([]StrategyConfig{a, b}), "\n"); !strings.Contains(joined, "shared by strategies alpha, beta") {
		t.Fatalf("hedge-vs-hedge collision not rejected: %s", joined)
	}
}

func TestValidateHedgeConfigsRejectsBidirectionalFlip(t *testing.T) {
	sc := hedgeTestStrategy("ETH", "BTC")
	sc.Direction = DirectionBoth
	if joined := strings.Join(validateHedgeConfigs([]StrategyConfig{sc}), "\n"); !strings.Contains(joined, "direction=both") {
		t.Fatalf("bidirectional hedge not rejected: %s", joined)
	}
}

func TestHedgeNestedUnknownKeyRejected(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"strategies": []any{map[string]any{"id": "alpha", "hedge": map[string]any{"enabled": true, "ration": 1}}}})
	errs := validateStrategyJSONKeys(raw)
	if joined := strings.Join(errs, "\n"); !strings.Contains(joined, `strategy[alpha].hedge: unknown field "ration"`) {
		t.Fatalf("unknown hedge key not rejected: %v", errs)
	}
}

func TestApplyHedgeOpenAndRiskIsolation(t *testing.T) {
	sc := hedgeTestStrategy("ETH", "BTC")
	s := &StrategyState{ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 2500, TradePositionID: "primary-1"}}}
	snap := hedgeSnapshotFor(s, "ETH", "BTC")
	a := hedgeTargetDecision(sc, snap, 2500, 50000)
	if !applyHedgeOpenOrAdd(s, sc, "ETH", snap, a, 50000, .1, 2, "99") {
		t.Fatal("hedge open not applied")
	}
	h := s.Positions["BTC"]
	if h == nil || !h.IsHedge || h.HedgeForSymbol != "ETH" || h.HedgeForPositionID != "primary-1" || h.HedgePrimaryQtyBasis != 2 {
		t.Fatalf("hedge metadata=%+v", h)
	}
	s.RiskState.ConsecutiveLosses = 4
	RecordHedgeTradeResult(&s.RiskState, -25)
	if s.RiskState.ConsecutiveLosses != 4 || s.RiskState.DailyPnL != -25 {
		t.Fatalf("hedge risk routing=%+v", s.RiskState)
	}
}

func TestPartialHedgeFillAdvancesBasisProportionally(t *testing.T) {
	sc := hedgeTestStrategy("ETH", "BTC")
	s := &StrategyState{ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 2500, TradePositionID: "primary-1"}}}
	snap := hedgeSnapshotFor(s, "ETH", "BTC")
	a := hedgeTargetDecision(sc, snap, 2500, 50000) // target .1 BTC
	if !applyHedgeOpenOrAdd(s, sc, "ETH", snap, a, 50000, .04, 0, "") {
		t.Fatal("partial hedge fill not applied")
	}
	if got := s.Positions["BTC"].HedgePrimaryQtyBasis; mathAbs(got-.8) > 1e-12 {
		t.Fatalf("basis=%g, want .8 primary qty covered", got)
	}
}

func TestHedgeHotReloadBlockedOpenAllowedFlat(t *testing.T) {
	old := hedgeTestStrategy("ETH", "BTC")
	next := old
	h := *old.Hedge
	h.Ratio = 2
	next.Hedge = &h
	open := &AppState{Strategies: map[string]*StrategyState{"alpha": {ID: "alpha", Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: .1, IsHedge: true}}}}}
	if err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{old}}, &Config{Strategies: []StrategyConfig{next}}, open); err == nil || !strings.Contains(err.Error(), "hedge changed") {
		t.Fatalf("open hedge reload should fail, got %v", err)
	}
	flat := &AppState{Strategies: map[string]*StrategyState{"alpha": {ID: "alpha", Positions: map[string]*Position{}}}}
	if err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{old}}, &Config{Strategies: []StrategyConfig{next}}, flat); err != nil {
		t.Fatalf("flat hedge reload: %v", err)
	}
}

func TestHedgePositionMetadataPersists(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{Strategies: map[string]*StrategyState{
		"alpha": {ID: "alpha", Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 2, InitialQuantity: 2, AvgCost: 2500, Side: "long", Multiplier: 1, OwnerStrategyID: "alpha", TradePositionID: "primary-1", HedgeSymbol: "BTC"},
			"BTC": {Symbol: "BTC", Quantity: .1, InitialQuantity: .1, AvgCost: 50000, Side: "short", Multiplier: 1, OwnerStrategyID: "alpha", TradePositionID: "hedge-1", IsHedge: true, HedgeForSymbol: "ETH", HedgeForPositionID: "primary-1", HedgePrimaryQtyBasis: 2},
		}, OptionPositions: map[string]*OptionPosition{}},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	h := loaded.Strategies["alpha"].Positions["BTC"]
	if h == nil || !h.IsHedge || h.HedgeForSymbol != "ETH" || h.HedgeForPositionID != "primary-1" || h.HedgePrimaryQtyBasis != 2 {
		t.Fatalf("loaded hedge=%+v", h)
	}
	if got := loaded.Strategies["alpha"].Positions["ETH"].HedgeSymbol; got != "BTC" {
		t.Fatalf("primary hedge_symbol=%q", got)
	}
}

func TestKillSwitchIncludesAndBooksHedgeCoin(t *testing.T) {
	sc := hedgeTestStrategy("ETH", "BTC")
	sc.Args = []string{"hold", "ETH", "1h", "--mode=live"}
	closer, calls := fakeCloser(nil)
	report := forceCloseHyperliquidLive(context.Background(), []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -.1}}, []StrategyConfig{sc}, closer, nil)
	if len(*calls) != 2 || (*calls)[0] != "ETH" || (*calls)[1] != "BTC" {
		t.Fatalf("kill-switch close calls=%v", *calls)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("kill-switch errors=%v", report.Errors)
	}

	s := &StrategyState{ID: sc.ID, Type: "perps", Platform: "hyperliquid", Cash: 1000, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, AvgCost: 2500, Side: "long", Multiplier: 1},
		"BTC": {Symbol: "BTC", Quantity: .1, AvgCost: 50000, Side: "short", Multiplier: 1, IsHedge: true, HedgeForSymbol: "ETH", HedgePrimaryQtyBasis: 2},
	}}
	virtual := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{sc.ID: s}, []StrategyConfig{sc})
	fills := map[string]HyperliquidCloseFill{"ETH": {TotalSz: 2, AvgPx: 2600, Fee: 1, OID: 1}, "BTC": {TotalSz: .1, AvgPx: 49000, Fee: .5, OID: 2}}
	if !applyHyperliquidKillSwitchCloseFill(s, sc, fills, []StrategyConfig{sc}, virtual) {
		t.Fatal("kill-switch fills not applied")
	}
	if len(s.Positions) != 0 {
		t.Fatalf("positions remain after kill switch: %+v", s.Positions)
	}
	if len(s.TradeHistory) != 2 || s.TradeHistory[1].TradeType != "hedge" {
		t.Fatalf("kill-switch trade attribution=%+v", s.TradeHistory)
	}
}

func TestHedgeLedgerIncludedButLifetimeStatsExcluded(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	rows := []Trade{
		{Timestamp: now, PositionID: "primary", Symbol: "ETH", TradeType: "perps", Quantity: 1, Price: 100},
		{Timestamp: now, PositionID: "primary", Symbol: "ETH", TradeType: "perps", Quantity: 1, Price: 110, IsClose: true, RealizedPnL: 10, PnLGross: true},
		{Timestamp: now, PositionID: "hedge", Symbol: "BTC", TradeType: "hedge", Quantity: .1, Price: 100},
		{Timestamp: now, PositionID: "hedge", Symbol: "BTC", TradeType: "hedge", Quantity: .1, Price: 105, IsClose: true, RealizedPnL: -4, ExchangeFee: 1, PnLGross: true},
	}
	for _, row := range rows {
		if err := db.InsertTrade("alpha", row); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	stats, err := db.LifetimeTradeStatsForStrategy("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stats.PositionsOpened != 1 || stats.Wins != 1 || stats.Losses != 0 {
		t.Fatalf("lifetime stats include hedge: %+v", stats)
	}
	ledger, err := db.LedgerNetByStrategy([]string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ledger["alpha"]; mathAbs(got-5) > 1e-12 {
		t.Fatalf("ledger net=%g, want 5 including hedge PnL/fee", got)
	}
}

func mathAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
