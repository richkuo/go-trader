package main

import (
	"math"
	"testing"
)

func hedgedStrategy(coin, hedgeSym string, ratio, leverage float64) StrategyConfig {
	return StrategyConfig{
		ID:       "eth",
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "check_hyperliquid.py",
		Args:     []string{"check_hyperliquid.py", coin}, // paper (no --mode=live)
		Hedge:    &HedgeConfig{Enabled: true, Symbol: hedgeSym, Side: "inverse", Ratio: ratio, Leverage: leverage},
	}
}

func TestHedgeSideForPrimary(t *testing.T) {
	if got := hedgeSideForPrimary("long"); got != "short" {
		t.Fatalf("long → %q, want short", got)
	}
	if got := hedgeSideForPrimary("short"); got != "long" {
		t.Fatalf("short → %q, want long", got)
	}
	if got := hedgeSideForPrimary("weird"); got != "" {
		t.Fatalf("unknown → %q, want empty", got)
	}
}

func TestHedgeExecuteSide(t *testing.T) {
	if hedgeExecuteSide("long") != "buy" || hedgeExecuteSide("short") != "sell" {
		t.Fatalf("execute side mapping wrong")
	}
}

func TestHedgeOpenQty_NotionalSizing(t *testing.T) {
	// primary 2 ETH @ $2000 = $4000 notional; ratio 1.5 → $6000; hedge $60000 → 0.1 BTC.
	qty, ok := hedgeOpenQty(2, 2000, 1.5, 60000)
	if !ok || math.Abs(qty-0.1) > 1e-9 {
		t.Fatalf("qty=%v ok=%v, want 0.1", qty, ok)
	}
	if _, ok := hedgeOpenQty(2, 0, 1, 60000); ok {
		t.Fatalf("expected fail on zero primary price")
	}
	if _, ok := hedgeOpenQty(2, 2000, 1, 0); ok {
		t.Fatalf("expected fail on zero hedge price")
	}
}

func TestHedgeReduceQty_Proportional(t *testing.T) {
	// primary shrank from basis 2 to 0.5 → reduce 75% of hedge qty.
	if got := hedgeReduceQty(0.1, 2, 0.5); math.Abs(got-0.075) > 1e-9 {
		t.Fatalf("reduceQty=%v, want 0.075", got)
	}
	// primary near flat → full hedge qty.
	if got := hedgeReduceQty(0.1, 2, 0); got != 0.1 {
		t.Fatalf("flat reduce=%v, want full 0.1", got)
	}
	// primary grew → no reduce.
	if got := hedgeReduceQty(0.1, 2, 3); got != 0 {
		t.Fatalf("grew reduce=%v, want 0", got)
	}
}

func TestHedgeTargetDecision_Open(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
	act := hedgeTargetDecision(sc, snap, 2000, 60000)
	if act.Kind != hedgeOpen {
		t.Fatalf("kind=%v, want open", act.Kind)
	}
	if act.Side != "short" {
		t.Fatalf("hedge side=%q, want short (inverse of long)", act.Side)
	}
	wantQty := 2 * 2000 * 1.0 / 60000
	if math.Abs(act.RequestedQty-wantQty) > 1e-9 {
		t.Fatalf("qty=%v, want %v", act.RequestedQty, wantQty)
	}
	if act.PrimaryQtyTarget != 2 {
		t.Fatalf("target=%v, want 2", act.PrimaryQtyTarget)
	}
}

func TestHedgeTargetDecision_OpenShortPrimary(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	act := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "short"}, 2000, 60000)
	if act.Kind != hedgeOpen || act.Side != "long" {
		t.Fatalf("short primary should hedge long, got kind=%v side=%q", act.Kind, act.Side)
	}
}

func TestHedgeTargetDecision_PrimaryFlatClosesHedge(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	snap := hedgeSnapshot{PrimaryQty: 0, HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
	act := hedgeTargetDecision(sc, snap, 2000, 60000)
	if act.Kind != hedgeCloseFull || act.RequestedQty != 0.1 {
		t.Fatalf("expected full close of 0.1, got %+v", act)
	}
}

func TestHedgeTargetDecision_Add(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	// primary grew from basis 2 → 3; delta 1 ETH @ $2000 = $2000 / $60000.
	snap := hedgeSnapshot{PrimaryQty: 3, PrimarySide: "long", HedgeQty: 0.0667, HedgeSide: "short", HedgeBasis: 2}
	act := hedgeTargetDecision(sc, snap, 2000, 60000)
	if act.Kind != hedgeAdd {
		t.Fatalf("kind=%v, want add", act.Kind)
	}
	wantQty := 1 * 2000 * 1.0 / 60000
	if math.Abs(act.RequestedQty-wantQty) > 1e-9 {
		t.Fatalf("add qty=%v, want %v", act.RequestedQty, wantQty)
	}
}

func TestHedgeTargetDecision_Reduce(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	// primary shrank from basis 2 → 1 (50%); reduce 50% of 0.1 = 0.05.
	snap := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
	act := hedgeTargetDecision(sc, snap, 2000, 60000)
	if act.Kind != hedgeReduce {
		t.Fatalf("kind=%v, want reduce", act.Kind)
	}
	if math.Abs(act.RequestedQty-0.05) > 1e-9 {
		t.Fatalf("reduce qty=%v, want 0.05", act.RequestedQty)
	}
}

func TestHedgeTargetDecision_WrongSideCloses(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	// primary long wants a short hedge, but the held hedge is long → close it.
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "long", HedgeBasis: 2}
	act := hedgeTargetDecision(sc, snap, 2000, 60000)
	if act.Kind != hedgeCloseFull || act.Reason != "wrong_side" {
		t.Fatalf("expected wrong_side close, got %+v", act)
	}
}

func TestHedgeTargetDecision_DustAddDeferred(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	// tiny growth: delta 0.00001 ETH @ $2000 = $0.02 hedge notional < $10 min.
	snap := hedgeSnapshot{PrimaryQty: 2.00001, PrimarySide: "long", HedgeQty: 0.0667, HedgeSide: "short", HedgeBasis: 2}
	act := hedgeTargetDecision(sc, snap, 2000, 60000)
	if act.Kind != hedgeNone || act.Reason != "add_dust_deferred" {
		t.Fatalf("expected add_dust_deferred, got %+v", act)
	}
}

func TestHedgeTargetDecision_FailClosedUnusablePrice(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	// primary held, hedge flat, but hedge price is 0 → fail closed.
	act := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}, 2000, 0)
	if act.Kind != hedgeNone || !act.FailClosed {
		t.Fatalf("expected fail-closed none, got %+v", act)
	}
}

func TestHedgeTargetDecision_InSync(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: 0.0667, HedgeSide: "short", HedgeBasis: 2}
	if act := hedgeTargetDecision(sc, snap, 2000, 60000); act.Kind != hedgeNone {
		t.Fatalf("expected no action when in sync, got %+v", act)
	}
}

func TestHedgeAppliedBasis_PartialFill(t *testing.T) {
	// requested 0.1 but only 0.05 filled → basis advances halfway from 0 → 2.
	if got := hedgeAppliedBasis(0, 2, 0.1, 0.05); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("partial-fill basis=%v, want 1.0", got)
	}
	// full fill → full target.
	if got := hedgeAppliedBasis(0, 2, 0.1, 0.1); math.Abs(got-2.0) > 1e-9 {
		t.Fatalf("full-fill basis=%v, want 2.0", got)
	}
}

func TestRecordHedgeTradeResult_NoLossStreak(t *testing.T) {
	r := &RiskState{ConsecutiveLosses: 2}
	RecordHedgeTradeResult(r, -50) // a hedge loss must NOT extend the streak
	if r.ConsecutiveLosses != 2 {
		t.Fatalf("ConsecutiveLosses=%d, want 2 (hedge loss must not extend streak)", r.ConsecutiveLosses)
	}
	if math.Abs(r.DailyPnL-(-50)) > 1e-9 {
		t.Fatalf("DailyPnL=%v, want -50", r.DailyPnL)
	}
	// A normal loss DOES extend the streak (contrast).
	RecordTradeResult(r, -10)
	if r.ConsecutiveLosses != 3 {
		t.Fatalf("normal loss ConsecutiveLosses=%d, want 3", r.ConsecutiveLosses)
	}
}

func newHedgeTestState() *StrategyState {
	return &StrategyState{
		ID:        "eth",
		Type:      "perps",
		Platform:  "hyperliquid",
		Cash:      10000,
		Positions: map[string]*Position{},
	}
}

func TestApplyHedgeOpen_CreatesPositionAndTrade(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	s := newHedgeTestState()
	act := hedgeAction{Kind: hedgeOpen, Side: "short", RequestedQty: 0.1, PrimaryQtyTarget: 2}
	applyHedgeFillLocked(sc, s, "ETH", "BTC", act, 60000, 0, 0, true /*paper*/, nil)

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatalf("hedge position not created")
	}
	if pos.HedgeFor != "ETH" {
		t.Fatalf("HedgeFor=%q, want ETH", pos.HedgeFor)
	}
	if pos.Side != "short" || math.Abs(pos.Quantity-0.1) > 1e-9 {
		t.Fatalf("pos side/qty wrong: %+v", pos)
	}
	if math.Abs(pos.HedgePrimaryQtyBasis-2) > 1e-9 {
		t.Fatalf("basis=%v, want 2", pos.HedgePrimaryQtyBasis)
	}
	if pos.Multiplier != 1 {
		t.Fatalf("multiplier=%v, want 1 (perps PnL branch)", pos.Multiplier)
	}
	// Trade recorded as trade_type "hedge".
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].TradeType != "hedge" {
		t.Fatalf("expected one hedge trade, got %+v", s.TradeHistory)
	}
}

func TestApplyHedgeClose_BooksHedgeTradeType(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	s := newHedgeTestState()
	// Seed an open hedge leg.
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1, HedgeFor: "ETH", HedgePrimaryQtyBasis: 2}
	act := hedgeAction{Kind: hedgeCloseFull, RequestedQty: 0.1, Reason: "primary_flat"}
	applyHedgeFillLocked(sc, s, "ETH", "BTC", act, 60000, 0, 0, true, nil)

	if _, ok := s.Positions["BTC"]; ok {
		t.Fatalf("hedge position should be deleted after full close")
	}
	// The close leg must be tagged trade_type="hedge" (excluded from #T/W-L).
	var closeLeg *Trade
	for i := range s.TradeHistory {
		if s.TradeHistory[i].IsClose {
			closeLeg = &s.TradeHistory[i]
		}
	}
	if closeLeg == nil || closeLeg.TradeType != "hedge" {
		t.Fatalf("expected a hedge close leg, got %+v", s.TradeHistory)
	}
}

func TestHedgePositionDBRoundTrip(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"eth": {
				ID:              "eth",
				Type:            "perps",
				Platform:        "hyperliquid",
				Cash:            10000,
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	state.Strategies["eth"].Positions["BTC"] = &Position{
		Symbol:               "BTC",
		Quantity:             0.1,
		InitialQuantity:      0.1,
		AvgCost:              60000,
		Side:                 "short",
		Multiplier:           1,
		OwnerStrategyID:      "eth",
		HedgeFor:             "ETH",
		HedgePrimaryQtyBasis: 2,
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	pos := loaded.Strategies["eth"].Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge position missing after round-trip")
	}
	if pos.HedgeFor != "ETH" {
		t.Errorf("HedgeFor = %q, want ETH", pos.HedgeFor)
	}
	if math.Abs(pos.HedgePrimaryQtyBasis-2) > 1e-9 {
		t.Errorf("HedgePrimaryQtyBasis = %v, want 2", pos.HedgePrimaryQtyBasis)
	}
}

func TestApplyHedgeReduce_AdvancesBasis(t *testing.T) {
	sc := hedgedStrategy("ETH", "BTC", 1.0, 3)
	s := newHedgeTestState()
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1, HedgeFor: "ETH", HedgePrimaryQtyBasis: 2}
	act := hedgeAction{Kind: hedgeReduce, RequestedQty: 0.05, PrimaryQtyTarget: 1}
	applyHedgeFillLocked(sc, s, "ETH", "BTC", act, 60000, 0, 0, true, nil)

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatalf("hedge position should remain after partial reduce")
	}
	if math.Abs(pos.Quantity-0.05) > 1e-9 {
		t.Fatalf("remaining qty=%v, want 0.05", pos.Quantity)
	}
	if math.Abs(pos.HedgePrimaryQtyBasis-1) > 1e-9 {
		t.Fatalf("basis=%v, want 1 (advanced to target)", pos.HedgePrimaryQtyBasis)
	}
}
