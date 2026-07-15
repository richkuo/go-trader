package main

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type hedgeTestExecutor struct {
	openFill   hedgeFill
	openErr    error
	closeFill  hedgeFill
	unwindFill hedgeFill
	openCalls  int
	closeCalls int
	unwinds    int
}

func (e *hedgeTestExecutor) open(StrategyConfig, string, string, float64, bool, []HLPosition) (hedgeFill, error) {
	e.openCalls++
	return e.openFill, e.openErr
}

func (e *hedgeTestExecutor) close(StrategyConfig, string, float64) (hedgeFill, error) {
	e.closeCalls++
	return e.closeFill, nil
}

func (e *hedgeTestExecutor) unwindPrimary(StrategyConfig, string, float64, []int64) (hedgeFill, error) {
	e.unwinds++
	return e.unwindFill, nil
}

func hedgeTestStrategy() StrategyConfig {
	return StrategyConfig{
		ID:       "eth-alpha",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"--strategy", "ETH", "--mode=paper"},
		Hedge: &HedgeConfig{
			Enabled:    true,
			Symbol:     "BTC/USDC:USDC",
			Side:       "inverse",
			Ratio:      0.5,
			Platform:   "hyperliquid",
			Type:       "perps",
			MarginMode: "cross",
			Leverage:   3,
		},
	}
}

func TestHedgeTargetDecisionOpenUsesNotionalRatio(t *testing.T) {
	sc := hedgeTestStrategy()
	action := hedgeTargetDecision(sc, hedgeSnapshot{
		PrimaryQty:        4,
		PrimaryAvgCost:    2_000,
		PrimarySide:       "long",
		PrimaryPositionID: "primary-1",
	}, 40_000)

	if action.Kind != hedgeActionOpen {
		t.Fatalf("kind = %q, want %q (reason=%q)", action.Kind, hedgeActionOpen, action.Reason)
	}
	if action.Side != "short" {
		t.Fatalf("side = %q, want short", action.Side)
	}
	if math.Abs(action.Qty-0.1) > 1e-12 {
		t.Fatalf("qty = %.12f, want 0.1", action.Qty)
	}
}

func TestHedgeCoinPreservesHyperliquidTickerCase(t *testing.T) {
	sc := hedgeTestStrategy()
	h := *sc.Hedge
	h.Symbol = "kPEPE/USDC:USDC"
	sc.Hedge = &h
	if got := hedgeCoin(sc); got != "kPEPE" {
		t.Fatalf("hedge coin = %q, want case-preserved kPEPE", got)
	}
	if got := hedgeCollisionCoin(sc); got != "KPEPE" {
		t.Fatalf("collision coin = %q, want normalized KPEPE", got)
	}
}

func TestHedgeTargetDecisionInverseSideAndQuantityWatermark(t *testing.T) {
	sc := hedgeTestStrategy()

	add := hedgeTargetDecision(sc, hedgeSnapshot{
		PrimaryQty:         6,
		PrimaryAvgCost:     2_000,
		PrimarySide:        "short",
		PrimaryPositionID:  "primary-1",
		HedgeQty:           0.1,
		HedgeSide:          "long",
		HedgeCoveredQty:    4,
		HedgeForPositionID: "primary-1",
	}, 40_000)
	if add.Kind != hedgeActionAdd || add.Side != "long" {
		t.Fatalf("add = %#v, want long add", add)
	}
	if math.Abs(add.Qty-0.05) > 1e-12 {
		t.Fatalf("add qty = %.12f, want 0.05", add.Qty)
	}

	reduce := hedgeTargetDecision(sc, hedgeSnapshot{
		PrimaryQty:         2,
		PrimaryAvgCost:     2_000,
		PrimarySide:        "short",
		PrimaryPositionID:  "primary-1",
		HedgeQty:           0.1,
		HedgeSide:          "long",
		HedgeCoveredQty:    4,
		HedgeForPositionID: "primary-1",
	}, 80_000) // mark drift must not affect proportional reduction
	if reduce.Kind != hedgeActionReduce {
		t.Fatalf("reduce = %#v, want reduce", reduce)
	}
	if math.Abs(reduce.Qty-0.05) > 1e-12 {
		t.Fatalf("reduce qty = %.12f, want 0.05", reduce.Qty)
	}
}

func TestHedgeTargetDecisionClosesOrphanAndRejectsAmbiguousPair(t *testing.T) {
	sc := hedgeTestStrategy()
	orphan := hedgeTargetDecision(sc, hedgeSnapshot{
		HedgeQty:           0.2,
		HedgeSide:          "short",
		HedgeCoveredQty:    4,
		HedgeForPositionID: "primary-1",
	}, 40_000)
	if orphan.Kind != hedgeActionClose || orphan.Qty != 0.2 {
		t.Fatalf("orphan = %#v, want full close", orphan)
	}

	ambiguous := hedgeTargetDecision(sc, hedgeSnapshot{
		PrimaryQty:         4,
		PrimaryAvgCost:     2_000,
		PrimarySide:        "long",
		PrimaryPositionID:  "primary-2",
		HedgeQty:           0.1,
		HedgeSide:          "short",
		HedgeCoveredQty:    4,
		HedgeForPositionID: "primary-1",
	}, 40_000)
	if ambiguous.Kind != hedgeActionBlocked || !strings.Contains(ambiguous.Reason, "ownership") {
		t.Fatalf("ambiguous = %#v, want ownership block", ambiguous)
	}
}

func TestValidateHedgeConfigsRejectsCoinCollisions(t *testing.T) {
	base := hedgeTestStrategy()
	cases := []struct {
		name       string
		strategies []StrategyConfig
		want       string
	}{
		{
			name: "own primary coin",
			strategies: func() []StrategyConfig {
				sc := base
				h := *sc.Hedge
				h.Symbol = "ETH"
				sc.Hedge = &h
				return []StrategyConfig{sc}
			}(),
			want: "own primary coin",
		},
		{
			name: "configured peer coin",
			strategies: []StrategyConfig{
				base,
				{ID: "btc-peer", Type: "manual", Platform: "hyperliquid", Symbol: "btc", Timeframe: "1h", Leverage: 3},
			},
			want: "configured strategy coin",
		},
		{
			name: "shared hedge coin",
			strategies: func() []StrategyConfig {
				other := base
				other.ID = "sol-alpha"
				other.Args = []string{"--strategy", "SOL", "--mode=paper"}
				h := *base.Hedge
				other.Hedge = &h
				return []StrategyConfig{base, other}
			}(),
			want: "shared by hedge strategies",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateHedgeConfigs(tc.strategies)
			if !strings.Contains(strings.Join(errs, "\n"), tc.want) {
				t.Fatalf("errors = %v, want substring %q", errs, tc.want)
			}
		})
	}
}

func TestPaperHedgeLifecycleTracksPrimaryQuantityEvents(t *testing.T) {
	sc := hedgeTestStrategy()
	s := &StrategyState{
		ID: "eth-alpha", Type: "perps", Platform: "hyperliquid", Cash: 10_000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 4, InitialQuantity: 4, AvgCost: 2_000, Side: "long", Multiplier: 1, TradePositionID: "primary-1"},
		},
	}
	prices := map[string]float64{"ETH": 2_000, "BTC": 40_000}
	var mu sync.RWMutex

	trades, _ := runHedgeSyncWithExecutor(sc, s, prices, nil, &mu, nil, nil, true, &hedgeTestExecutor{})
	if trades != 1 {
		t.Fatalf("open trades = %d, want 1", trades)
	}
	h := s.Positions["BTC"]
	if h == nil || !h.IsHedge || h.Side != "short" || math.Abs(h.Quantity-0.1) > 1e-12 || h.HedgeForPositionID != "primary-1" || h.HedgeCoveredPrimaryQty != 4 {
		t.Fatalf("opened hedge = %+v", h)
	}
	if len(s.TradeHistory) != 1 || !s.TradeHistory[0].IsHedge {
		t.Fatalf("hedge open trade not labeled: %+v", s.TradeHistory)
	}

	s.Positions["ETH"].Quantity = 2
	trades, _ = runHedgeSyncWithExecutor(sc, s, prices, nil, &mu, nil, nil, false, &hedgeTestExecutor{})
	if trades != 1 || math.Abs(s.Positions["BTC"].Quantity-0.05) > 1e-12 || s.Positions["BTC"].HedgeCoveredPrimaryQty != 2 {
		t.Fatalf("reduced hedge = %+v, trades=%d", s.Positions["BTC"], trades)
	}

	delete(s.Positions, "ETH")
	trades, _ = runHedgeSyncWithExecutor(sc, s, prices, nil, &mu, nil, nil, false, &hedgeTestExecutor{})
	if trades != 1 || s.Positions["BTC"] != nil {
		t.Fatalf("orphan close left hedge=%+v trades=%d", s.Positions["BTC"], trades)
	}
}

func TestLiveHedgeOpenFailureUnwindsConfirmedPrimary(t *testing.T) {
	sc := hedgeTestStrategy()
	sc.Args = []string{"tema_cross_bd", "ETH", "--mode=live"}
	s := &StrategyState{
		ID: "eth-alpha", Type: "perps", Platform: "hyperliquid", Cash: 10_000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 4, InitialQuantity: 4, AvgCost: 2_000, Side: "long", Multiplier: 1, TradePositionID: "primary-1"},
		},
	}
	exec := &hedgeTestExecutor{
		openErr:    errors.New("hedge rejected"),
		unwindFill: hedgeFill{Price: 2_010, Qty: 4, Fee: 1, OID: "unwind-1"},
	}
	var mu sync.RWMutex
	trades, _ := runHedgeSyncWithExecutor(sc, s, map[string]float64{"ETH": 2_010, "BTC": 40_000}, nil, &mu, nil, nil, true, exec)
	if exec.openCalls != 1 || exec.unwinds != 1 {
		t.Fatalf("calls open=%d unwind=%d", exec.openCalls, exec.unwinds)
	}
	if trades != 1 || s.Positions["ETH"] != nil || s.Positions["BTC"] != nil {
		t.Fatalf("fail-closed state positions=%+v trades=%d", s.Positions, trades)
	}
	if got := s.TradeHistory[len(s.TradeHistory)-1]; !got.IsClose || got.Symbol != "ETH" || got.ExchangeOrderID != "unwind-1" {
		t.Fatalf("unwind trade = %+v", got)
	}
}

func TestHedgeStateAndTradeRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-alpha": {
			ID: "eth-alpha", Type: "perps", Platform: "hyperliquid", Cash: 9_999, InitialCapital: 10_000,
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 4, InitialQuantity: 4, AvgCost: 2_000, Side: "long", Multiplier: 1, TradePositionID: "primary-1", HedgeSymbol: "BTC", OpenedAt: now},
				"BTC": {Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 40_000, Side: "short", Multiplier: 1, TradePositionID: "hedge-1", IsHedge: true, HedgeForSymbol: "ETH", HedgeForPositionID: "primary-1", HedgeCoveredPrimaryQty: 4, OpenedAt: now},
			},
			TradeHistory: []Trade{{Timestamp: now, StrategyID: "eth-alpha", Symbol: "BTC", PositionID: "hedge-1", Side: "sell", Quantity: 0.1, Price: 40_000, Value: 4_000, TradeType: "perps", IsHedge: true}},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	primary := loaded.Strategies["eth-alpha"].Positions["ETH"]
	hedge := loaded.Strategies["eth-alpha"].Positions["BTC"]
	if primary.HedgeSymbol != "BTC" || !hedge.IsHedge || hedge.HedgeForSymbol != "ETH" || hedge.HedgeForPositionID != "primary-1" || hedge.HedgeCoveredPrimaryQty != 4 {
		t.Fatalf("round-trip primary=%+v hedge=%+v", primary, hedge)
	}
	if hist := loaded.Strategies["eth-alpha"].TradeHistory; len(hist) != 1 || !hist[0].IsHedge {
		t.Fatalf("round-trip trade history=%+v", hist)
	}
}

func TestPersistedHedgeDeclarationValidationRejectsDisabledOrMismatchedOwnership(t *testing.T) {
	sc := hedgeTestStrategy()
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-alpha": {ID: "eth-alpha", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, TradePositionID: "p1"},
			"BTC": {Symbol: "BTC", Quantity: 0.1, IsHedge: true, HedgeForSymbol: "ETH", HedgeForPositionID: "p1"},
		}},
	}}
	if err := validatePersistedHedgeDeclarations(state, &Config{Strategies: []StrategyConfig{sc}}); err != nil {
		t.Fatalf("valid pair rejected: %v", err)
	}
	disabled := sc
	disabled.Hedge = nil
	if err := validatePersistedHedgeDeclarations(state, &Config{Strategies: []StrategyConfig{disabled}}); err == nil || !strings.Contains(err.Error(), "no enabled hedge declaration") {
		t.Fatalf("disabled hedge error = %v", err)
	}
	state.Strategies["eth-alpha"].Positions["BTC"].HedgeForPositionID = "wrong"
	if err := validatePersistedHedgeDeclarations(state, &Config{Strategies: []StrategyConfig{sc}}); err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("mismatched ownership error = %v", err)
	}
	state.Strategies["eth-alpha"].Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1}
	if err := validatePersistedHedgeDeclarations(state, &Config{Strategies: []StrategyConfig{sc}}); err == nil || !strings.Contains(err.Error(), "without hedge ownership metadata") {
		t.Fatalf("unowned declared-coin error = %v", err)
	}
}

func TestReconcileAttributesExternalHedgeCloseWithoutTouchingPrimary(t *testing.T) {
	sc := hedgeTestStrategy()
	sc.Args = []string{"tema_cross_bd", "ETH", "--mode=live"}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-alpha": {
			ID: "eth-alpha", Type: "perps", Platform: "hyperliquid", Cash: 10_000,
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, InitialQuantity: 1, AvgCost: 2_000, Side: "long", Multiplier: 1, TradePositionID: "p1"},
				"BTC": {Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 40_000, Side: "short", Multiplier: 1, TradePositionID: "h1", IsHedge: true, HedgeForSymbol: "ETH", HedgeForPositionID: "p1", HedgeCoveredPrimaryQty: 1},
			},
		},
	}}
	lm, err := NewLogManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.RWMutex
	changed, _, _ := reconcileHyperliquidAccountPositions([]StrategyConfig{sc}, []StrategyConfig{sc}, state, &mu, lm, []HLPosition{{Coin: "ETH", Size: 1, EntryPrice: 2_000}}, map[string]float64{"ETH": 2_000, "BTC": 39_000}, "", nil, false)
	if !changed || state.Strategies["eth-alpha"].Positions["BTC"] != nil {
		t.Fatalf("hedge close not reconciled: changed=%v positions=%+v", changed, state.Strategies["eth-alpha"].Positions)
	}
	if state.Strategies["eth-alpha"].Positions["ETH"] == nil {
		t.Fatal("primary was touched by hedge reconciliation")
	}
	hist := state.Strategies["eth-alpha"].TradeHistory
	if len(hist) != 1 || !hist[0].IsHedge || !hist[0].IsClose || hist[0].Symbol != "BTC" {
		t.Fatalf("reconciled trade = %+v", hist)
	}
}

func TestReconcileAttributesExternalPartialHedgeReduction(t *testing.T) {
	sc := hedgeTestStrategy()
	sc.Args = []string{"tema_cross_bd", "ETH", "--mode=live"}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-alpha": {
			ID: "eth-alpha", Type: "perps", Platform: "hyperliquid", Cash: 10_000,
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 2, InitialQuantity: 2, AvgCost: 2_000, Side: "long", Multiplier: 1, TradePositionID: "p1"},
				"BTC": {Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 40_000, Side: "short", Multiplier: 1, TradePositionID: "h1", IsHedge: true, HedgeForSymbol: "ETH", HedgeForPositionID: "p1", HedgeCoveredPrimaryQty: 2},
			},
		},
	}}
	lm, err := NewLogManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.RWMutex
	changed, _, _ := reconcileHyperliquidAccountPositions([]StrategyConfig{sc}, []StrategyConfig{sc}, state, &mu, lm, []HLPosition{{Coin: "ETH", Size: 2, EntryPrice: 2_000}, {Coin: "BTC", Size: -0.05, EntryPrice: 39_000}}, map[string]float64{"ETH": 2_000, "BTC": 39_000}, "", nil, false)
	if !changed {
		t.Fatal("partial external hedge reduction was not reconciled")
	}
	h := state.Strategies["eth-alpha"].Positions["BTC"]
	if h == nil || math.Abs(h.Quantity-0.05) > 1e-12 || math.Abs(h.HedgeCoveredPrimaryQty-1) > 1e-12 {
		t.Fatalf("remaining hedge = %+v, want qty=0.05 covered=1", h)
	}
	if state.Strategies["eth-alpha"].Positions["ETH"].Quantity != 2 {
		t.Fatal("primary was touched by partial hedge reconciliation")
	}
	hist := state.Strategies["eth-alpha"].TradeHistory
	if len(hist) != 1 || !hist[0].IsHedge || !hist[0].IsClose || math.Abs(hist[0].Quantity-0.05) > 1e-12 {
		t.Fatalf("partial hedge trade = %+v", hist)
	}
}

func TestReconcileDoesNotAdoptExternalHedgeGrowth(t *testing.T) {
	sc := hedgeTestStrategy()
	sc.Args = []string{"tema_cross_bd", "ETH", "--mode=live"}
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-alpha": {ID: "eth-alpha", Type: "perps", Platform: "hyperliquid", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 2, AvgCost: 2_000, Side: "long", Multiplier: 1, TradePositionID: "p1"},
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 40_000, Side: "short", Multiplier: 1, TradePositionID: "h1", IsHedge: true, HedgeForSymbol: "ETH", HedgeForPositionID: "p1", HedgeCoveredPrimaryQty: 2},
		}},
	}}
	lm, err := NewLogManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.RWMutex
	changed, _, _ := reconcileHyperliquidAccountPositions([]StrategyConfig{sc}, []StrategyConfig{sc}, state, &mu, lm, []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -0.2}}, nil, "", nil, false)
	if !changed {
		t.Fatal("external hedge growth did not poison coverage")
	}
	h := state.Strategies["eth-alpha"].Positions["BTC"]
	if h.Quantity != 0.1 || h.HedgeCoveredPrimaryQty != 0 {
		t.Fatalf("external growth was adopted or coverage was not poisoned: %+v", h)
	}
}

func TestKillSwitchClosesOnlyPersistedOwnedHedgeCoin(t *testing.T) {
	sc := hedgeTestStrategy()
	sc.Args = []string{"tema_cross_bd", "ETH", "--mode=live"}
	var coins []string
	closer := func(symbol string, _ *float64, _ []int64) (*HyperliquidCloseResult, error) {
		coins = append(coins, symbol)
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: symbol, Fill: &HyperliquidCloseFill{AvgPx: 100, TotalSz: 1}}}, nil
	}
	positions := []HLPosition{{Coin: "ETH", Size: 1}, {Coin: "BTC", Size: -1}, {Coin: "SOL", Size: 1}}
	report := forceCloseHyperliquidLiveOwned(t.Context(), positions, []StrategyConfig{sc}, closer, nil, map[string]bool{"BTC": true})
	if strings.Join(coins, ",") != "ETH,BTC" {
		t.Fatalf("closed coins = %v, want primary+owned hedge only", coins)
	}
	if !report.ConfirmedFlat() || len(report.Fills) != 2 {
		t.Fatalf("report = %+v", report)
	}
}

func TestCircuitBreakerQueuesOwnedHedgeWhenPrimaryCoinIsShared(t *testing.T) {
	sc := hedgeTestStrategy()
	sc.Args = []string{"tema_cross_bd", "ETH", "--mode=live"}
	peer := sc
	peer.ID = "eth-peer"
	peer.Hedge = nil
	s := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", TradePositionID: "p1"},
		"BTC": {Symbol: "BTC", Quantity: 0.05, Side: "short", IsHedge: true, HedgeForSymbol: "ETH", HedgeForPositionID: "p1"},
	}}
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -0.05}},
		HLLiveAll:   []StrategyConfig{sc, peer},
	}
	symbols := hyperliquidCircuitCloseSymbols(&sc, s, assist)
	if len(symbols) != 1 || symbols[0].Symbol != "BTC" || symbols[0].Size != 0.05 {
		t.Fatalf("pending symbols = %+v, want sole-owned BTC hedge only", symbols)
	}
}

func TestCircuitBreakerHedgeFillKeepsAlphaStatsSeparate(t *testing.T) {
	s := &StrategyState{ID: "eth-alpha", Cash: 10_000, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", TradePositionID: "p1", HedgeSymbol: "BTC"},
		"BTC": {Symbol: "BTC", Quantity: 0.05, AvgCost: 40_000, Side: "short", TradePositionID: "h1", IsHedge: true, HedgeForSymbol: "ETH", HedgeForPositionID: "p1"},
	}}
	applyHyperliquidCircuitCloseFill(s, "BTC", 0.05, 41_000, 1, -0.05, 123, "circuit_breaker")
	if s.Positions["BTC"] != nil || s.Positions["ETH"].HedgeSymbol != "" {
		t.Fatalf("pair metadata not cleared after hedge CB close: %+v", s.Positions)
	}
	if len(s.TradeHistory) != 1 || !s.TradeHistory[0].IsHedge || !s.TradeHistory[0].IsClose {
		t.Fatalf("hedge CB trade not labeled: %+v", s.TradeHistory)
	}
	if s.RiskState.ConsecutiveLosses != 0 || s.RiskState.DailyPnL != -51 {
		t.Fatalf("risk accounting = %+v, want daily pnl -51 and unchanged alpha loss streak", s.RiskState)
	}
}

func TestHedgeIncludedInExposureAndMarginModels(t *testing.T) {
	sc := hedgeTestStrategy()
	state := map[string]*StrategyState{
		"eth-alpha": {ID: "eth-alpha", Type: "perps", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 2, AvgCost: 2_000, Side: "long", Multiplier: 1, Leverage: 2},
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 40_000, Side: "short", Multiplier: 1, Leverage: 4, IsHedge: true},
		}},
	}
	assets, skipped := computeAssetDeltas(state, []StrategyConfig{sc}, map[string]float64{"ETH": 2_000, "BTC": 40_000})
	if len(skipped) != 0 || assets["ETH"].NetDeltaUSD != 4_000 || assets["BTC"].NetDeltaUSD != -4_000 {
		t.Fatalf("assets=%+v skipped=%v", assets, skipped)
	}
	_, margin := perpsMarginDrawdownInputs(state["eth-alpha"], 2, map[string]float64{"ETH": 2_000, "BTC": 40_000})
	if margin != 3_000 { // ETH 4000/2 + BTC 4000/4
		t.Fatalf("margin = %.2f, want 3000", margin)
	}
}

func TestHedgeOperatorSurfacesAreExplicit(t *testing.T) {
	sc := hedgeTestStrategy()
	ss := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
		"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 40_000, Side: "short", IsHedge: true, HedgeForSymbol: "ETH", HedgeForPositionID: "p1"},
	}}
	lines := collectPositions(sc, ss, map[string]float64{"BTC": 39_000})
	if len(lines) != 1 || !strings.Contains(lines[0], "HEDGE for ETH") {
		t.Fatalf("position lines = %v", lines)
	}
	text := formatStrategyInspection(sc, map[string]bool{"hedge": true}, &Config{}, nil)
	if !strings.Contains(text, "hedge:") || !strings.Contains(text, "BTC inverse 0.5x") {
		t.Fatalf("inspect text missing hedge:\n%s", text)
	}
	jsonOut := buildStrategyInspectionJSON(sc, map[string]bool{"hedge": true}, &Config{}, nil)
	if _, ok := jsonOut["hedge"]; !ok {
		t.Fatalf("inspect JSON missing hedge: %+v", jsonOut)
	}
}

func TestHedgeHotReloadBlockedWhilePairOpenAndAllowedInRestartShapeWhenFlat(t *testing.T) {
	current := hedgeTestStrategy()
	next := current
	h := *current.Hedge
	h.Ratio = 0.75
	next.Hedge = &h
	state := &AppState{Strategies: map[string]*StrategyState{
		current.ID: {ID: current.ID, Positions: map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 1}}},
	}}
	err := validateHotReloadStateCompatible(&Config{Strategies: []StrategyConfig{current}}, &Config{Strategies: []StrategyConfig{next}}, state)
	if err == nil || !strings.Contains(err.Error(), "hedge changed with open positions") {
		t.Fatalf("open-pair reload error = %v", err)
	}
	if !reflect.DeepEqual(strategyRestartShape(current), strategyRestartShape(next)) {
		t.Fatal("hedge should be masked from restart-only shape; flat hot reload owns it")
	}
}

func TestHedgeUnknownNestedKeyFailsLoudly(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"eth-alpha","hedge":{"enabled":true,"symbol":"BTC","ratlo":0.5}}]}`)
	errs := validateStrategyJSONKeys(raw)
	if len(errs) != 1 || !strings.Contains(errs[0], `strategy[eth-alpha].hedge: unknown field "ratlo"`) {
		t.Fatalf("errors = %v", errs)
	}
}

func TestHedgeTradesDoNotInflateAlphaLifetimeStats(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	rows := []Trade{
		{Timestamp: now, StrategyID: "eth-alpha", Symbol: "ETH", PositionID: "p1", Side: "buy", Quantity: 1, Price: 2_000, TradeType: "perps"},
		{Timestamp: now, StrategyID: "eth-alpha", Symbol: "BTC", PositionID: "h1", Side: "sell", Quantity: 0.05, Price: 40_000, TradeType: "perps", IsHedge: true},
		{Timestamp: now, StrategyID: "eth-alpha", Symbol: "BTC", PositionID: "h1", Side: "buy", Quantity: 0.05, Price: 39_000, TradeType: "perps", IsClose: true, RealizedPnL: 50, PnLGross: true, IsHedge: true},
		{Timestamp: now, StrategyID: "eth-alpha", Symbol: "ETH", PositionID: "p1", Side: "sell", Quantity: 1, Price: 2_100, TradeType: "perps", IsClose: true, RealizedPnL: 100, PnLGross: true},
	}
	for _, row := range rows {
		if err := db.InsertTrade(row.StrategyID, row); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := db.LifetimeTradeStatsForStrategy("eth-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stats.PositionsOpened != 1 || stats.Wins != 1 || stats.Losses != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}
