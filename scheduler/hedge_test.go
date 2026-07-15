package main

import (
	"math"
	"strings"
	"sync"
	"testing"
)

func TestHedgeTargetDecisionUsesNotionalDeltaAndInverseSide(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC", Ratio: 0.5}}

	open := hedgeTargetDecision(sc, hedgeSnapshot{
		PrimaryQty:  2,
		PrimarySide: "long",
	}, 100, 50)
	if open.Kind != hedgeActionOpen || open.Side != "short" || math.Abs(open.Qty-2) > 1e-9 {
		t.Fatalf("open = %#v, want short 2", open)
	}

	add := hedgeTargetDecision(sc, hedgeSnapshot{
		PrimaryQty:  3,
		PrimarySide: "long",
		HedgeQty:    2,
		HedgeSide:   "short",
		HedgeBasis:  2,
	}, 100, 50)
	if add.Kind != hedgeActionAdd || add.Side != "short" || math.Abs(add.Qty-1) > 1e-9 {
		t.Fatalf("add = %#v, want short 1", add)
	}
}

func TestIndependentAlphaTradeCountExcludesHedgeExecutionLegs(t *testing.T) {
	trades := []Trade{
		{TradeType: "perps"},
		{TradeType: "hedge"},
		{TradeType: "hedge"},
		{TradeType: "manual"},
	}
	if got := independentAlphaTradeCount(trades); got != 2 {
		t.Fatalf("independent alpha trade count = %d, want 2", got)
	}
}

func TestHedgeTargetDecisionReducesAndClosesWithoutPrice(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}}

	reduce := hedgeTargetDecision(sc, hedgeSnapshot{
		PrimaryQty:  2,
		PrimarySide: "short",
		HedgeQty:    6,
		HedgeSide:   "long",
		HedgeBasis:  3,
	}, 100, 50)
	if reduce.Kind != hedgeActionReduce || reduce.Side != "long" || math.Abs(reduce.Qty-2) > 1e-9 {
		t.Fatalf("reduce = %#v, want long 2", reduce)
	}

	close := hedgeTargetDecision(sc, hedgeSnapshot{HedgeQty: 6, HedgeSide: "long"}, 0, 0)
	if close.Kind != hedgeActionCloseFull || close.Side != "long" || math.Abs(close.Qty-6) > 1e-9 {
		t.Fatalf("close = %#v, want full long close", close)
	}

	badMark := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long"}, 100, 0)
	if badMark.Kind != hedgeActionNone || badMark.Reason == "" {
		t.Fatalf("bad mark = %#v, want fail-closed reason", badMark)
	}
}

func TestRunHedgeSyncPaperMirrorsPrimaryQuantityLifecycle(t *testing.T) {
	sc := StrategyConfig{
		ID:       "eth-alpha",
		Type:     "perps",
		Platform: "hyperliquid",
		Hedge:    &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC", Ratio: 0.5},
	}
	s := &StrategyState{
		ID:       sc.ID,
		Type:     sc.Type,
		Platform: sc.Platform,
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 2, InitialQuantity: 2, AvgCost: 100, Side: "long", Multiplier: 1, OwnerStrategyID: sc.ID},
		},
	}
	var mu sync.RWMutex

	if got := runHedgeSync(sc, s, "ETH", 100, 50, false, hlExecuteSnapshot{}, &mu, nil, nil, true); got.Trades != 1 {
		t.Fatalf("open sync trades = %d, want 1", got.Trades)
	}
	hedge := s.Positions["BTC"]
	if hedge == nil || hedge.HedgeFor != "ETH" || hedge.Side != "short" || math.Abs(hedge.Quantity-2) > 1e-9 || math.Abs(hedge.HedgePrimaryQtyBasis-2) > 1e-9 {
		t.Fatalf("hedge after open = %#v", hedge)
	}

	s.Positions["ETH"].Quantity = 1
	if got := runHedgeSync(sc, s, "ETH", 0, 50, false, hlExecuteSnapshot{}, &mu, nil, nil, false); got.Trades != 1 {
		t.Fatalf("reduce sync trades = %d, want 1", got.Trades)
	}
	hedge = s.Positions["BTC"]
	if hedge == nil || math.Abs(hedge.Quantity-1) > 1e-9 || math.Abs(hedge.HedgePrimaryQtyBasis-1) > 1e-9 {
		t.Fatalf("hedge after reduce = %#v", hedge)
	}

	delete(s.Positions, "ETH")
	if got := runHedgeSync(sc, s, "ETH", 0, 0, false, hlExecuteSnapshot{}, &mu, nil, nil, false); got.Trades != 1 {
		t.Fatalf("close sync trades = %d, want 1", got.Trades)
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatalf("hedge remained after primary close: %#v", s.Positions["BTC"])
	}
	if len(s.TradeHistory) != 3 {
		t.Fatalf("hedge trades = %d, want 3", len(s.TradeHistory))
	}
	for _, trade := range s.TradeHistory {
		if trade.TradeType != "hedge" {
			t.Fatalf("trade type = %q, want hedge", trade.TradeType)
		}
	}
}

func TestRunHedgeSyncFreezesRestartMismatchedPersistedHedge(t *testing.T) {
	// A config change followed by restart bypasses SIGHUP's flat-only gate. The
	// persisted HedgeFor record must win over the new hedge config so the
	// scheduler neither abandons BTC nor opens a second SOL hedge.
	sc := StrategyConfig{
		ID: "eth-alpha", Type: "perps", Platform: "hyperliquid",
		Hedge: &HedgeConfig{Enabled: true, Symbol: "SOL", Ratio: 1},
	}
	s := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 100, Side: "long"},
		"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 200, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1},
	}}
	var mu sync.RWMutex
	if got := runHedgeSync(sc, s, "ETH", 100, 100, false, hlExecuteSnapshot{}, &mu, nil, nil, false); got.Changed {
		t.Fatalf("mismatched hedge sync changed state: %#v", got)
	}
	if s.Positions["BTC"] == nil || s.Positions["SOL"] != nil {
		t.Fatalf("mismatched hedge state = %#v, want only persisted BTC leg", s.Positions)
	}
}

func TestValidateHedgeConfigsRejectsAmbiguousCoinOwnership(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "eth", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "ETH"}, Hedge: &HedgeConfig{Enabled: true, Symbol: "btc/usdc:usdc"}},
		{ID: "btc", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "BTC"}},
	}
	errs := validateHedgeConfigs(strategies)
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), "collides") {
		t.Fatalf("errors = %v, want primary coin collision", errs)
	}
}

func TestHedgeCircuitBreakerQueuesPrimaryAndPersistedHedge(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
		Args:  []string{"hold", "ETH", "1h", "--mode=live"},
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5},
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 1, Side: "long"},
		"BTC": {Symbol: "BTC", Quantity: 0.02, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1},
	}}
	setHyperliquidCircuitBreakerPending(&sc, state, &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 1}, {Coin: "BTC", Size: -0.02}},
		HLLiveAll:   []StrategyConfig{sc},
	})
	pending := state.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if pending == nil || len(pending.Symbols) != 2 {
		t.Fatalf("pending circuit close = %#v, want primary and hedge", pending)
	}
	if pending.Symbols[0].Symbol != "ETH" || pending.Symbols[1].Symbol != "BTC" {
		t.Fatalf("pending symbols = %#v, want [ETH BTC]", pending.Symbols)
	}
}

func TestHedgeCircuitBreakerDoesNotQueueHedgeWhenPrimaryIsShared(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
		Args:  []string{"hold", "ETH", "1h", "--mode=live"},
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5},
	}
	state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 1, Side: "long"},
		"BTC": {Symbol: "BTC", Quantity: 0.02, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1},
	}}
	peer := StrategyConfig{ID: "hl-eth-peer", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "ETH", "1h", "--mode=live"}}
	setHyperliquidCircuitBreakerPending(&sc, state, &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 1}, {Coin: "BTC", Size: -0.02}},
		HLLiveAll:   []StrategyConfig{sc, peer},
	})
	if pending := state.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid); pending != nil {
		t.Fatalf("shared primary must not queue an independently-closing hedge: %#v", pending)
	}
}

func TestForceCloseAllPositionsClosesPrimaryBeforeHedge(t *testing.T) {
	s := &StrategyState{ID: "eth-alpha", Type: "perps", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 100, Side: "long", Multiplier: 1},
		"BTC": {Symbol: "BTC", Quantity: 2, AvgCost: 50, Side: "short", Multiplier: 1, HedgeFor: "ETH", HedgePrimaryQtyBasis: 1},
	}}
	forceCloseAllPositions(s, map[string]float64{"ETH": 90, "BTC": 60}, nil)
	if len(s.TradeHistory) != 2 {
		t.Fatalf("close trade count = %d, want 2", len(s.TradeHistory))
	}
	if s.TradeHistory[0].Symbol != "ETH" || s.TradeHistory[1].Symbol != "BTC" {
		t.Fatalf("close ordering = %#v, want primary ETH then hedge BTC", s.TradeHistory)
	}
	if s.TradeHistory[1].TradeType != "hedge" {
		t.Fatalf("hedge close trade type = %q, want hedge", s.TradeHistory[1].TradeType)
	}
}

func TestHedgeInspectionSurfacesConfigAndOwnedLeg(t *testing.T) {
	sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.02, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1.25},
		}},
	}}
	text := formatStrategyInspection(sc, nil, nil, state)
	if !strings.Contains(text, "hedge:               inverse BTC ratio=0.5") || !strings.Contains(text, "hedge_for=ETH primary_qty_basis=1.250000") {
		t.Fatalf("inspect output did not surface hedge ownership:\n%s", text)
	}
	out := buildStrategyInspectionJSON(sc, nil, nil, state)
	if _, ok := out["hedge"]; !ok {
		t.Fatalf("inspect JSON did not include hedge config: %#v", out)
	}
}
