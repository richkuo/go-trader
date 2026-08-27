package main

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPrimaryPx = 2000.0
	testHedgePx   = 50000.0
)

func hedgeTestState(id string) *StrategyState {
	return &StrategyState{
		ID:        id,
		Type:      "perps",
		Platform:  "hyperliquid",
		Cash:      10000,
		Positions: map[string]*Position{},
	}
}

func hedgeTestConfig() StrategyConfig {
	return withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{
		Enabled: true, Symbol: "BTC", Ratio: 1.0, Leverage: 3, MarginMode: "cross",
	})
}

func primaryPos(qty float64, side string) *Position {
	return &Position{Symbol: "ETH", Quantity: qty, AvgCost: testPrimaryPx, Side: side, Multiplier: 1, OwnerStrategyID: "eth-long"}
}

func hedgePos(qty float64, side string, basis float64) *Position {
	return &Position{
		Symbol: "BTC", Quantity: qty, InitialQuantity: qty, AvgCost: testHedgePx, Side: side,
		Multiplier: 1, OwnerStrategyID: "eth-long", HedgeFor: "ETH", HedgePrimaryQtyBasis: basis,
	}
}

func TestHedgeTargetDecisionOpensInverseWithNotionalSizing(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "long", HedgeSymbol: "BTC"}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)

	if act.Kind != hedgeActionOpen {
		t.Fatalf("kind = %v, want open (%s)", act.Kind, act.Reason)
	}
	if act.HedgeSide != "short" || act.Side != "sell" {
		t.Fatalf("long primary must hedge SHORT via a sell, got side=%q order=%q", act.HedgeSide, act.Side)
	}
	if math.Abs(act.Qty-0.4) > 1e-12 {
		t.Fatalf("qty = %v, want 0.4", act.Qty)
	}
	if act.NewBasis != 10 {
		t.Fatalf("basis = %v, want 10", act.NewBasis)
	}
}

func TestHedgeTargetDecisionShortPrimaryHedgesLong(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 5, PrimarySide: "short", HedgeSymbol: "BTC"}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionOpen || act.HedgeSide != "long" || act.Side != "buy" {
		t.Fatalf("short primary must hedge LONG via a buy, got %v side=%q order=%q", act.Kind, act.HedgeSide, act.Side)
	}
}

func TestHedgeTargetDecisionAppliesRatio(t *testing.T) {
	sc := hedgeTestConfig()
	sc.Hedge.Ratio = 0.5
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "long", HedgeSymbol: "BTC"}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if math.Abs(act.Qty-0.2) > 1e-12 {
		t.Fatalf("qty at ratio 0.5 = %v, want 0.2", act.Qty)
	}
}

func TestHedgeTargetDecisionDefersOpenBelowMinNotional(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 0.0045, PrimarySide: "long", HedgeSymbol: "BTC"}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionNone {
		t.Fatalf("kind = %v (%s), want none — a sub-$%.0f open must be deferred, not submitted", act.Kind, act.Reason, hedgeMinOrderNotionalUSD)
	}
}

func TestHedgeTargetDecisionDeferredOpenFiresAfterGrowth(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 0.02, PrimarySide: "long", HedgeSymbol: "BTC"}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionOpen {
		t.Fatalf("kind = %v (%s), want open — $%.2f notional is above the floor", act.Kind, act.Reason, 0.02*testPrimaryPx)
	}
	if act.NewBasis != 0.02 {
		t.Fatalf("basis = %v, want 0.02", act.NewBasis)
	}
}

func TestHedgeTargetDecisionIgnoresMarkDriftWhenQuantityUnchanged(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "long",
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "short", HedgeBasis: 10,
	}
	for _, marks := range [][2]float64{{2000, 50000}, {2600, 41000}, {1500, 63000}} {
		act := hedgeTargetDecision(sc, snap, marks[0], marks[1])
		if act.Kind != hedgeActionNone {
			t.Fatalf("marks %v produced %v (%s); mark drift must never re-trade the hedge", marks, act.Kind, act.Reason)
		}
	}
}

func TestHedgeTargetDecisionAddsOnPrimaryGrowth(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 15, PrimarySide: "long",
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "short", HedgeBasis: 10,
	}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionAdd {
		t.Fatalf("kind = %v, want add (%s)", act.Kind, act.Reason)
	}
	if math.Abs(act.Qty-0.2) > 1e-12 {
		t.Fatalf("add qty = %v, want 0.2", act.Qty)
	}
	if act.NewBasis != 15 {
		t.Fatalf("basis = %v, want 15", act.NewBasis)
	}
}

func TestHedgeTargetDecisionReducesProportionallyOnPartialClose(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 5, PrimarySide: "long",
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "short", HedgeBasis: 10,
	}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionReduce {
		t.Fatalf("kind = %v, want reduce (%s)", act.Kind, act.Reason)
	}
	if math.Abs(act.Qty-0.2) > 1e-12 {
		t.Fatalf("reduce qty = %v, want 0.2 (half of the held 0.4)", act.Qty)
	}
	if act.NewBasis != 5 {
		t.Fatalf("basis = %v, want 5", act.NewBasis)
	}
}

func TestHedgeTargetDecisionReduceIsImmuneToMarkMovement(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 5, PrimarySide: "long",
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "short", HedgeBasis: 10,
	}
	a := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	b := hedgeTargetDecision(sc, snap, testPrimaryPx*1.4, testHedgePx*0.6)
	if math.Abs(a.Qty-b.Qty) > 1e-12 {
		t.Fatalf("reduce qty moved with marks: %v vs %v", a.Qty, b.Qty)
	}
}

func TestHedgeTargetDecisionClosesWhenPrimaryFlat(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 0,
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "short", HedgeBasis: 10,
	}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionCloseFull || math.Abs(act.Qty-0.4) > 1e-12 || act.NewBasis != 0 {
		t.Fatalf("primary flat must flatten the hedge in full, got %v qty=%v basis=%v (%s)", act.Kind, act.Qty, act.NewBasis, act.Reason)
	}
}

func TestHedgeTargetDecisionClosesOnFlatPrimaryEvenWithoutMarks(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 0,
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "short", HedgeBasis: 10,
	}
	act := hedgeTargetDecision(sc, snap, 0, 0)
	if act.Kind != hedgeActionCloseFull {
		t.Fatalf("kind = %v, want close even with unusable marks (%s)", act.Kind, act.Reason)
	}
}

func TestHedgeTargetDecisionFailsClosedOnUnusableMarks(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "long", HedgeSymbol: "BTC"}
	for _, marks := range [][2]float64{{0, testHedgePx}, {testPrimaryPx, 0}, {-1, -1}} {
		act := hedgeTargetDecision(sc, snap, marks[0], marks[1])
		if act.Kind != hedgeActionNone || !act.Blocked {
			t.Fatalf("marks %v: kind=%v blocked=%v — must fail closed, never size off a stale price", marks, act.Kind, act.Blocked)
		}
	}
}

func TestHedgeTargetDecisionFailsClosedOnUnknownPrimarySide(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "sideways", HedgeSymbol: "BTC"}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if !act.Blocked {
		t.Fatalf("unknown primary side must block, got %v (%s)", act.Kind, act.Reason)
	}
}

func TestHedgeTargetDecisionFlattensWrongSideLeg(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "long",
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "long", HedgeBasis: 10,
	}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionCloseFull {
		t.Fatalf("kind = %v, want close for a wrong-side leg (%s)", act.Kind, act.Reason)
	}
	if !strings.Contains(act.Reason, "doubles exposure") {
		t.Fatalf("reason must explain the danger, got %q", act.Reason)
	}
}

func TestHedgeTargetDecisionClearsCorruptLeg(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "long",
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: -0.4, HedgeSide: "short", HedgeBasis: 10,
	}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionCloseFull || act.Qty != 0.4 {
		t.Fatalf("corrupt leg must be cleared with the absolute qty, got %v qty=%v (%s)", act.Kind, act.Qty, act.Reason)
	}
}

func TestHedgeTargetDecisionDefersDustAddWithoutAdvancingBasis(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 10.001, PrimarySide: "long",
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "short", HedgeBasis: 10,
	}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionNone {
		t.Fatalf("kind = %v, want none for a $2 add (%s)", act.Kind, act.Reason)
	}
	if act.NewBasis != 0 {
		t.Fatalf("a deferred add must NOT advance the basis, got %v", act.NewBasis)
	}
}

func TestHedgeTargetDecisionCollapsesNearTotalReduceIntoFullClose(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 0.001, PrimarySide: "long",
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "short", HedgeBasis: 10,
	}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionCloseFull {
		t.Fatalf("kind = %v, want closeFull when the residual is sub-minimum (%s)", act.Kind, act.Reason)
	}
}

func TestHedgeTargetDecisionReanchorsMissingBasisWithoutTrading(t *testing.T) {
	sc := hedgeTestConfig()
	snap := hedgeSnapshot{
		PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "long",
		HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.4, HedgeSide: "short", HedgeBasis: 0,
	}
	act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionNone {
		t.Fatalf("kind = %v, want none — a missing basis must re-anchor, not trade (%s)", act.Kind, act.Reason)
	}
	if act.NewBasis != 10 {
		t.Fatalf("basis = %v, want 10", act.NewBasis)
	}
}

func TestHedgeTargetDecisionNoopWhenDisabled(t *testing.T) {
	sc := hedgePerpsStrategy("eth-long", "ETH")
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "long"}
	if act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx); act.Kind != hedgeActionNone {
		t.Fatalf("no hedge config must produce no action, got %v", act.Kind)
	}
}

func TestHedgeOrderSkipReasonBlocksStaleDecisions(t *testing.T) {
	sc := hedgeTestConfig()
	open := hedgeAction{Kind: hedgeActionOpen, Qty: 0.4, Side: "sell", HedgeSide: "short", NewBasis: 10}

	cases := []struct {
		name   string
		action hedgeAction
		snap   hedgeSnapshot
		needle string
	}{
		{
			"primary closed between snapshot and spawn",
			open,
			hedgeSnapshot{HedgeSymbol: "BTC", PrimaryQty: 0, PrimarySide: "long"},
			"primary position is flat",
		},
		{
			"hedge already opened by a racing path",
			open,
			hedgeSnapshot{HedgeSymbol: "BTC", PrimaryQty: 10, PrimarySide: "long", HedgeHeld: true, HedgeQty: 0.4},
			"already exists",
		},
		{
			"primary flipped side",
			open,
			hedgeSnapshot{HedgeSymbol: "BTC", PrimaryQty: 10, PrimarySide: "short"},
			"primary side changed",
		},
		{
			"stale add after the primary shrank back",
			hedgeAction{Kind: hedgeActionAdd, Qty: 0.2, Side: "sell", HedgeSide: "short", NewBasis: 15},
			hedgeSnapshot{HedgeSymbol: "BTC", PrimaryQty: 10, PrimarySide: "long", HedgeHeld: true, HedgeQty: 0.4, HedgeBasis: 10},
			"add is stale",
		},
		{
			"add with the hedge leg gone",
			hedgeAction{Kind: hedgeActionAdd, Qty: 0.2, Side: "sell", HedgeSide: "short", NewBasis: 15},
			hedgeSnapshot{HedgeSymbol: "BTC", PrimaryQty: 15, PrimarySide: "long"},
			"hedge leg vanished",
		},
		{
			"reduce with the hedge already flat",
			hedgeAction{Kind: hedgeActionReduce, Qty: 0.2},
			hedgeSnapshot{HedgeSymbol: "BTC", PrimaryQty: 5, PrimarySide: "long"},
			"already flat",
		},
		{
			"non-positive quantity",
			hedgeAction{Kind: hedgeActionOpen, Qty: 0, Side: "sell", HedgeSide: "short"},
			hedgeSnapshot{HedgeSymbol: "BTC", PrimaryQty: 10, PrimarySide: "long"},
			"not positive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hedgeOrderSkipReason(sc, tc.action, tc.snap)
			if !strings.Contains(got, tc.needle) {
				t.Fatalf("skip reason = %q, want it to contain %q", got, tc.needle)
			}
		})
	}
}

func TestHedgeOrderSkipReasonAllowsValidOrder(t *testing.T) {
	sc := hedgeTestConfig()
	act := hedgeAction{Kind: hedgeActionOpen, Qty: 0.4, Side: "sell", HedgeSide: "short", NewBasis: 10}
	snap := hedgeSnapshot{HedgeSymbol: "BTC", PrimaryQty: 10, PrimarySide: "long"}
	if got := hedgeOrderSkipReason(sc, act, snap); got != "" {
		t.Fatalf("valid order was skipped: %q", got)
	}
}

func TestHedgeSnapshotIgnoresUnstampedPositionOnHedgeCoin(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, AvgCost: testHedgePx, Side: "long", Multiplier: 1}

	snap := hedgeSnapshotFromState(sc, s)
	if snap.HedgeHeld {
		t.Fatal("an unstamped position on the hedge coin must not be treated as our hedge leg")
	}
}

func TestHedgeSnapshotIgnoresLegStampedForAnotherPrimary(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	stale := hedgePos(0.4, "short", 10)
	stale.HedgeFor = "SOL"
	s.Positions["BTC"] = stale

	if snap := hedgeSnapshotFromState(sc, s); snap.HedgeHeld {
		t.Fatal("a leg stamped for another primary must not be adopted")
	}
}

func TestHeldHedgeCoinRequiresHeldStampedLeg(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	if got := heldHedgeCoin(sc, s); got != "" {
		t.Fatalf("no leg → %q, want empty", got)
	}
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	if got := heldHedgeCoin(sc, s); got != "BTC" {
		t.Fatalf("held leg → %q, want BTC", got)
	}
	s.Positions["BTC"].Quantity = 0
	if got := heldHedgeCoin(sc, s); got != "" {
		t.Fatalf("flat leg → %q, want empty", got)
	}
}

func TestApplyHedgeFillOpensPositionWithOwnershipMetadata(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	act := hedgeAction{Kind: hedgeActionOpen, Qty: 0.4, Side: "sell", HedgeSide: "short", NewBasis: 10}

	applyHedgeFill(sc, s, "ETH", act, act.Qty, testHedgePx, 12, true, "999", silentStrategyLogger("eth-long"))

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge position was not created")
	}
	if pos.HedgeFor != "ETH" {
		t.Fatalf("HedgeFor = %q, want ETH", pos.HedgeFor)
	}
	if pos.HedgePrimaryQtyBasis != 10 {
		t.Fatalf("basis = %v, want 10", pos.HedgePrimaryQtyBasis)
	}
	if pos.Side != "short" || pos.Quantity != 0.4 {
		t.Fatalf("leg = %s %v, want short 0.4", pos.Side, pos.Quantity)
	}
	if pos.Multiplier != 1 {
		t.Fatalf("Multiplier = %v, want the canonical perps value 1", pos.Multiplier)
	}
	if pos.OwnerStrategyID != "eth-long" {
		t.Fatalf("OwnerStrategyID = %q, want eth-long", pos.OwnerStrategyID)
	}
	if pos.Leverage != 3 {
		t.Fatalf("Leverage = %v, want the hedge block's own 3", pos.Leverage)
	}

	if len(s.TradeHistory) != 1 {
		t.Fatalf("trade rows = %d, want 1", len(s.TradeHistory))
	}
	tr := s.TradeHistory[0]
	if tr.TradeType != hedgeTradeType {
		t.Fatalf("trade_type = %q, want %q — lifetime stats key on it", tr.TradeType, hedgeTradeType)
	}
	if !strings.HasPrefix(tr.Details, "hedge(ETH)") {
		t.Fatalf("details = %q, want a hedge(<primary>) prefix", tr.Details)
	}
	if tr.ExchangeFee != 12 {
		t.Fatalf("ExchangeFee = %v, want the real fill fee 12", tr.ExchangeFee)
	}
	if math.Abs(s.Cash-(10000-12)) > 1e-9 {
		t.Fatalf("cash = %v, want 9988 (open fee deducted)", s.Cash)
	}
}

func TestApplyHedgeFillAdvancesBasisProportionallyOnPartialAdd(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(15, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	act := hedgeAction{Kind: hedgeActionAdd, Qty: 0.2, Side: "sell", HedgeSide: "short", NewBasis: 15}
	act.Qty = 0.2
	applyHedgeFillPartial(t, sc, s, act, 0.1)

	pos := s.Positions["BTC"]
	if math.Abs(pos.HedgePrimaryQtyBasis-12.5) > 1e-9 {
		t.Fatalf("basis = %v, want 12.5 (half of the 10→15 delta)", pos.HedgePrimaryQtyBasis)
	}
	if math.Abs(pos.Quantity-0.5) > 1e-9 {
		t.Fatalf("hedge qty = %v, want 0.5", pos.Quantity)
	}
}

func applyHedgeFillPartial(t *testing.T, sc StrategyConfig, s *StrategyState, act hedgeAction, filled float64) {
	t.Helper()
	requested := act.Qty
	pos := s.Positions[hedgeCoin(sc)]
	prevBasis := pos.HedgePrimaryQtyBasis
	_ = prevBasis
	applyHedgeFill(sc, s, "ETH", act, filled, testHedgePx, 3, true, "1000", silentStrategyLogger(s.ID))
	_ = requested
}

func TestApplyHedgeFillBlendsAddIntoExistingLeg(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(15, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	act := hedgeAction{Kind: hedgeActionAdd, Qty: 0.2, Side: "sell", HedgeSide: "short", NewBasis: 15}
	applyHedgeFill(sc, s, "ETH", act, act.Qty, 60000, 5, true, "1001", silentStrategyLogger("eth-long"))

	pos := s.Positions["BTC"]
	if math.Abs(pos.Quantity-0.6) > 1e-9 {
		t.Fatalf("qty = %v, want 0.6", pos.Quantity)
	}
	wantAvg := (testHedgePx*0.4 + 60000*0.2) / 0.6
	if math.Abs(pos.AvgCost-wantAvg) > 1e-6 {
		t.Fatalf("avg cost = %v, want %v", pos.AvgCost, wantAvg)
	}
	if pos.HedgePrimaryQtyBasis != 15 {
		t.Fatalf("basis = %v, want 15", pos.HedgePrimaryQtyBasis)
	}
}

func TestApplyHedgeFillReduceKeepsLegAndRestampsBasis(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(5, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	act := hedgeAction{Kind: hedgeActionReduce, Qty: 0.2, NewBasis: 5}
	applyHedgeFill(sc, s, "ETH", act, act.Qty, 48000, 4, true, "1002", silentStrategyLogger("eth-long"))

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("a partial reduce must leave the leg open")
	}
	if math.Abs(pos.Quantity-0.2) > 1e-9 {
		t.Fatalf("qty = %v, want 0.2", pos.Quantity)
	}
	if pos.HedgePrimaryQtyBasis != 5 {
		t.Fatalf("basis = %v, want 5 (re-anchored to the reduced primary)", pos.HedgePrimaryQtyBasis)
	}
}

func TestApplyHedgeFillCloseDeletesLeg(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	act := hedgeAction{Kind: hedgeActionCloseFull, Qty: 0.4, NewBasis: 0}
	applyHedgeFill(sc, s, "ETH", act, act.Qty, 48000, 4, true, "1003", silentStrategyLogger("eth-long"))

	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("full close must delete the hedge leg")
	}
}

func TestRecordHedgeTradeResultKeepsDailyPnLButNotTheLossStreak(t *testing.T) {
	r := &RiskState{}
	RecordTradeResult(r, -100)
	RecordTradeResult(r, -100)
	if r.ConsecutiveLosses != 2 {
		t.Fatalf("setup: streak = %d, want 2", r.ConsecutiveLosses)
	}

	RecordHedgeTradeResult(r, 150)
	if r.ConsecutiveLosses != 2 {
		t.Fatalf("streak = %d — a hedge win must not reset a genuine losing streak", r.ConsecutiveLosses)
	}
	RecordHedgeTradeResult(r, -50)
	if r.ConsecutiveLosses != 2 {
		t.Fatalf("streak = %d — a hedge loss must not extend the streak", r.ConsecutiveLosses)
	}
	if math.Abs(r.DailyPnL-(-100-100+150-50)) > 1e-9 {
		t.Fatalf("daily PnL = %v, want -100 (hedge PnL is real cash and must count)", r.DailyPnL)
	}
}

func TestRecordPositionTradeResultRoutesByHedgeStamp(t *testing.T) {
	s := hedgeTestState("eth-long")
	recordPositionTradeResult(s, primaryPos(10, "long"), -5)
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Fatalf("primary loss must extend the streak, got %d", s.RiskState.ConsecutiveLosses)
	}
	recordPositionTradeResult(s, hedgePos(0.4, "short", 10), -5)
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Fatalf("hedge loss must NOT extend the streak, got %d", s.RiskState.ConsecutiveLosses)
	}
}

func TestBookPerpsCloseRoutesHedgeLegAwayFromLossStreak(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	if !bookPerpsCloseWithFillFee(s, "BTC", 55000, 5, true, "77", hedgeCloseCloseReason, "hedge(ETH) close", "hedge", silentStrategyLogger("eth-long")) {
		t.Fatal("close was not booked")
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Fatalf("streak = %d — a hedge close must never reach the loss streak", s.RiskState.ConsecutiveLosses)
	}
	if s.RiskState.DailyPnL >= 0 {
		t.Fatalf("daily PnL = %v, want the hedge loss counted", s.RiskState.DailyPnL)
	}
}

type fakeHedgeExec struct {
	openCalls      []string
	reduceCalls    []string
	unwindCalls    []string
	openErr        error
	openResult     *HyperliquidExecuteResult
	reduceResult   *HyperliquidCloseResult
	unwindResult   *HyperliquidCloseResult
	unwindErr      error
	lastSetMargin  bool
	lastUnwindQty  float64
	lastUnwindOIDs []int64
}

func (f *fakeHedgeExec) executor() hedgeExecutor {
	return hedgeExecutor{
		Open: func(sc StrategyConfig, coin, side string, qty float64, setMargin bool) (*HyperliquidExecuteResult, error) {
			f.openCalls = append(f.openCalls, fmt.Sprintf("%s %s %.8f", side, coin, qty))
			f.lastSetMargin = setMargin
			return f.openResult, f.openErr
		},
		Reduce: func(sc StrategyConfig, coin string, qty *float64) (*HyperliquidCloseResult, error) {
			q := -1.0
			if qty != nil {
				q = *qty
			}
			f.reduceCalls = append(f.reduceCalls, fmt.Sprintf("%s %.8f", coin, q))
			return f.reduceResult, nil
		},
		UnwindPrimary: func(sc StrategyConfig, coin string, qty float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
			f.unwindCalls = append(f.unwindCalls, fmt.Sprintf("%s %.8f", coin, qty))
			f.lastUnwindQty = qty
			f.lastUnwindOIDs = cancelOIDs
			return f.unwindResult, f.unwindErr
		},
	}
}

func execFill(px, sz, fee float64) *HyperliquidExecuteResult {
	return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: px, TotalSz: sz, Fee: fee, OID: 42}}}
}

func closeFill(px, sz, fee float64) *HyperliquidCloseResult {
	return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: px, TotalSz: sz, Fee: fee, OID: 43}}}
}

func TestRunHedgeSyncOpensHedgeOnLivePrimaryOpen(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	var mu sync.RWMutex
	f := &fakeHedgeExec{openResult: execFill(testHedgePx, 0.4, 10)}

	kind := runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, FreshExposureQty: 10, Live: true,
	}, nil, silentStrategyLogger("eth-long"))

	if kind != hedgeActionOpen {
		t.Fatalf("kind = %v, want open", kind)
	}
	if len(f.openCalls) != 1 || f.openCalls[0] != "sell BTC 0.40000000" {
		t.Fatalf("open calls = %v", f.openCalls)
	}
	if !f.lastSetMargin {
		t.Fatal("a FRESH hedge open must assert its own margin_mode/leverage")
	}
	if s.Positions["BTC"] == nil || s.Positions["BTC"].HedgeFor != "ETH" {
		t.Fatal("hedge leg was not booked")
	}
}

func TestRunHedgeSyncAddDoesNotResendMarginSettings(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(15, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	var mu sync.RWMutex
	f := &fakeHedgeExec{openResult: execFill(testHedgePx, 0.2, 5)}

	if kind := runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, Live: true,
	}, nil, silentStrategyLogger("eth-long")); kind != hedgeActionAdd {
		t.Fatalf("kind = %v, want add", kind)
	}
	if f.lastSetMargin {
		t.Fatal("an add must NOT resend margin/leverage — HL rejects it on an open position")
	}
}

func TestRunHedgeSyncUnwindsPrimaryWhenFreshOpenHedgeFails(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	pos := primaryPos(10, "long")
	pos.StopLossOID = 555
	s.Positions["ETH"] = pos
	var mu sync.RWMutex
	f := &fakeHedgeExec{
		openResult:   &HyperliquidExecuteResult{Error: "insufficient margin"},
		unwindResult: closeFill(testPrimaryPx, 10, 8),
	}

	runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, FreshExposureQty: 10,
		PrimaryCancelOIDs: []int64{555}, Live: true,
	}, nil, silentStrategyLogger("eth-long"))

	if len(f.unwindCalls) != 1 {
		t.Fatalf("unwind calls = %v, want exactly one", f.unwindCalls)
	}
	if f.lastUnwindQty != 10 {
		t.Fatalf("unwind qty = %v, want the full 10 (the whole fresh open)", f.lastUnwindQty)
	}
	if len(f.lastUnwindOIDs) != 1 || f.lastUnwindOIDs[0] != 555 {
		t.Fatalf("a FULL unwind must cancel the resting SL, got %v", f.lastUnwindOIDs)
	}
	if _, ok := s.Positions["ETH"]; ok {
		t.Fatal("the primary must not survive a failed fresh-open hedge")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("no hedge leg should exist after a failed open")
	}
}

func TestRunHedgeSyncUnwindsOnlyTheIncrementWhenAddHedgeFails(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	pos := primaryPos(15, "long")
	pos.StopLossOID = 777
	s.Positions["ETH"] = pos
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	var mu sync.RWMutex
	f := &fakeHedgeExec{
		openResult:   &HyperliquidExecuteResult{Error: "order rejected"},
		unwindResult: closeFill(testPrimaryPx, 5, 4),
	}

	runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, FreshExposureQty: 5,
		PrimaryCancelOIDs: []int64{777}, Live: true,
	}, nil, silentStrategyLogger("eth-long"))

	if f.lastUnwindQty != 5 {
		t.Fatalf("unwind qty = %v, want only the 5-unit increment", f.lastUnwindQty)
	}
	if len(f.lastUnwindOIDs) != 0 {
		t.Fatalf("a PARTIAL unwind must NOT cancel protection (the position survives), got %v", f.lastUnwindOIDs)
	}
	remaining := s.Positions["ETH"]
	if remaining == nil || math.Abs(remaining.Quantity-10) > 1e-9 {
		t.Fatalf("primary = %v, want the pre-add 10 to survive", remaining)
	}
	if s.Positions["BTC"] == nil {
		t.Fatal("the pre-add hedge leg must survive")
	}
}

func TestRunHedgeSyncDoesNotUnwindAgedPositionOnHedgeFailure(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	var mu sync.RWMutex
	f := &fakeHedgeExec{openResult: &HyperliquidExecuteResult{Error: "rate limited"}}

	runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, FreshExposureQty: 0, Live: true,
	}, nil, silentStrategyLogger("eth-long"))

	if len(f.unwindCalls) != 0 {
		t.Fatalf("an aged position must not be unwound, got %v", f.unwindCalls)
	}
	if s.Positions["ETH"] == nil {
		t.Fatal("primary must survive")
	}
}

func TestRunHedgeSyncDoesNotMutateStateWhenOpenFails(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	var mu sync.RWMutex
	f := &fakeHedgeExec{openResult: &HyperliquidExecuteResult{Execution: &HyperliquidExecution{}}}

	runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, Live: true,
	}, nil, silentStrategyLogger("eth-long"))

	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("a fill-less execute result must not create a hedge position")
	}
	if len(s.TradeHistory) != 0 {
		t.Fatalf("no trade rows expected, got %d", len(s.TradeHistory))
	}
}

func TestRunHedgeSyncNoOpWhenPrimaryOpenFailed(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	var mu sync.RWMutex
	f := &fakeHedgeExec{}

	kind := runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, Live: true,
	}, nil, silentStrategyLogger("eth-long"))

	if kind != hedgeActionNone {
		t.Fatalf("kind = %v, want none", kind)
	}
	if len(f.openCalls) != 0 || len(f.reduceCalls) != 0 {
		t.Fatalf("no orders expected, got open=%v reduce=%v", f.openCalls, f.reduceCalls)
	}
}

func TestRunHedgeSyncClosesHedgeWhenPrimaryClosed(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	var mu sync.RWMutex
	f := &fakeHedgeExec{reduceResult: closeFill(48000, 0.4, 4)}

	kind := runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: 48000, Live: true,
	}, nil, silentStrategyLogger("eth-long"))

	if kind != hedgeActionCloseFull {
		t.Fatalf("kind = %v, want closeFull", kind)
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("hedge leg must be gone")
	}
}

func TestRunHedgeSyncClearsLegOnAlreadyFlat(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	var mu sync.RWMutex
	f := &fakeHedgeExec{reduceResult: &HyperliquidCloseResult{Close: &HyperliquidClose{AlreadyFlat: true}}}

	runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: 48000, Live: true,
	}, nil, silentStrategyLogger("eth-long"))

	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("an already-flat exchange response must clear the virtual leg")
	}
}

func TestRunHedgeSyncPaperBooksWithoutOrders(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	var mu sync.RWMutex
	f := &fakeHedgeExec{}

	if kind := runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, Live: false,
	}, nil, silentStrategyLogger("eth-long")); kind != hedgeActionOpen {
		t.Fatalf("kind = %v, want open", kind)
	}
	if len(f.openCalls) != 0 {
		t.Fatalf("paper must place no orders, got %v", f.openCalls)
	}
	pos := s.Positions["BTC"]
	if pos == nil || pos.Side != "short" {
		t.Fatal("paper hedge leg was not booked")
	}
	if s.TradeHistory[0].FeeSource != FeeSourceModeled {
		t.Fatalf("paper fee source = %q, want modeled", s.TradeHistory[0].FeeSource)
	}
}

func TestHedgeLifecycleMirrorsPrimaryQuantityEvents(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	var mu sync.RWMutex
	f := &fakeHedgeExec{}
	in := hedgeSyncInputs{PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, Live: false}
	log := silentStrategyLogger("eth-long")

	s.Positions["ETH"] = primaryPos(10, "long")
	runHedgeSync(sc, s, &mu, f.executor(), in, nil, log)
	if got := s.Positions["BTC"].Quantity; math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("after open: hedge = %v, want 0.4", got)
	}

	s.Positions["ETH"].Quantity = 15
	runHedgeSync(sc, s, &mu, f.executor(), in, nil, log)
	if got := s.Positions["BTC"].Quantity; math.Abs(got-0.6) > 1e-9 {
		t.Fatalf("after add: hedge = %v, want 0.6", got)
	}

	s.Positions["ETH"].Quantity = 6
	runHedgeSync(sc, s, &mu, f.executor(), in, nil, log)
	if got := s.Positions["BTC"].Quantity; math.Abs(got-0.24) > 1e-9 {
		t.Fatalf("after partial: hedge = %v, want 0.24", got)
	}

	delete(s.Positions, "ETH")
	runHedgeSync(sc, s, &mu, f.executor(), in, nil, log)
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("after full close: hedge leg must be gone")
	}
}

func TestReconcileHyperliquidHedgeLegBooksExternalCloseAndAlerts(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	var alerts []string
	changed := reconcileHyperliquidHedgeLeg(sc, s, nil, noFillFeeResolver, silentStrategyLogger("eth-long"), nil, &alerts)
	if !changed {
		t.Fatal("an externally closed hedge leg must be reconciled")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("hedge leg must be removed after an external close")
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "closed externally") {
		t.Fatalf("alerts = %v, want an external-close notice", alerts)
	}
}

func TestReconcileHyperliquidHedgeLegDoesNotAdoptForeignPosition(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")

	var alerts []string
	reconcileHyperliquidHedgeLeg(sc, s, []HLPosition{{Coin: "BTC", Size: 2.5, EntryPrice: testHedgePx}},
		noFillFeeResolver, silentStrategyLogger("eth-long"), nil, &alerts)

	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("a foreign on-chain position must never be adopted as our hedge leg")
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "Foreign position on hedge coin") {
		t.Fatalf("alerts = %v, want a foreign-position notice", alerts)
	}
}

func TestReconcileHyperliquidHedgeLegPreservesBasisOnRestart(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	var alerts []string
	reconcileHyperliquidHedgeLeg(sc, s, []HLPosition{{Coin: "BTC", Size: -0.4, EntryPrice: testHedgePx}},
		noFillFeeResolver, silentStrategyLogger("eth-long"), nil, &alerts)

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge leg must survive a matching reconcile")
	}
	if pos.HedgePrimaryQtyBasis != 10 {
		t.Fatalf("basis = %v, want the persisted 10 — ownership must come from state, not inference", pos.HedgePrimaryQtyBasis)
	}
	if len(alerts) != 0 {
		t.Fatalf("a matching reconcile must be silent, got %v", alerts)
	}
}

func TestReconcileHyperliquidHedgeLegRefusesUnstampedPosition(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, AvgCost: testHedgePx, Side: "long", Multiplier: 1}

	var alerts []string
	if reconcileHyperliquidHedgeLeg(sc, s, nil, noFillFeeResolver, silentStrategyLogger("eth-long"), nil, &alerts) {
		t.Fatal("an unstamped position must not be reconciled as a hedge leg")
	}
	if s.Positions["BTC"] == nil {
		t.Fatal("the unstamped position must be left alone, not deleted")
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "Hedge coin conflict") {
		t.Fatalf("alerts = %v, want a conflict notice", alerts)
	}
}

func TestValidateHedgeStateConsistencyFlagsOrphanedAndRepointedLegs(t *testing.T) {
	cases := []struct {
		name   string
		cfg    []StrategyConfig
		needle string
	}{
		{
			"hedge disabled by a config edit + restart",
			[]StrategyConfig{hedgePerpsStrategy("eth-long", "ETH")},
			"hedge block is now absent/disabled",
		},
		{
			"hedge symbol re-pointed",
			[]StrategyConfig{withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: true, Symbol: "SOL"})},
			"config now declares hedge.symbol=SOL",
		},
		{
			"strategy removed entirely",
			nil,
			"no longer in the config",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := hedgeTestState("eth-long")
			s.Positions["BTC"] = hedgePos(0.4, "short", 10)
			state := &AppState{Strategies: map[string]*StrategyState{"eth-long": s}}

			warnings := validateHedgeStateConsistency(state, &Config{Strategies: tc.cfg})
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.needle) {
				t.Fatalf("warnings = %v, want one containing %q", warnings, tc.needle)
			}
			if s.Positions["BTC"] == nil {
				t.Fatal("the startup check must never close a position")
			}
		})
	}
}

func TestValidateHedgeStateConsistencySilentWhenHealthy(t *testing.T) {
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	state := &AppState{Strategies: map[string]*StrategyState{"eth-long": s}}

	if w := validateHedgeStateConsistency(state, &Config{Strategies: []StrategyConfig{hedgeTestConfig()}}); len(w) != 0 {
		t.Fatalf("healthy config must be silent, got %v", w)
	}
}

func TestValidatePerpsDirectionConfigSkipsHedgeLegs(t *testing.T) {
	sc := hedgeTestConfig()
	sc.Direction = DirectionLong
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	state := &AppState{Strategies: map[string]*StrategyState{"eth-long": s}}

	if w := ValidatePerpsDirectionConfig(state, &Config{Strategies: []StrategyConfig{sc}}); len(w) != 0 {
		t.Fatalf("hedge legs must be exempt from the direction gap check, got %v", w)
	}
}

func TestForceCloseHyperliquidLiveIncludesHeldHedgeCoins(t *testing.T) {
	closed := map[string]bool{}
	closer := func(symbol string, partialSz *float64, oids []int64) (*HyperliquidCloseResult, error) {
		closed[symbol] = true
		return closeFill(testHedgePx, 0.4, 3), nil
	}
	positions := []HLPosition{{Coin: "ETH", Size: 10}, {Coin: "BTC", Size: -0.4}}
	report := forceCloseHyperliquidLive(t.Context(), positions,
		[]StrategyConfig{hedgeTestConfig()}, map[string]bool{"BTC": true}, closer, nil)

	if !closed["ETH"] || !closed["BTC"] {
		t.Fatalf("kill switch must flatten both legs, closed = %v", closed)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", report.Errors)
	}
}

func TestForceCloseHyperliquidLiveSkipsUnheldHedgeCoin(t *testing.T) {
	closed := map[string]bool{}
	closer := func(symbol string, partialSz *float64, oids []int64) (*HyperliquidCloseResult, error) {
		closed[symbol] = true
		return closeFill(testHedgePx, 1, 3), nil
	}
	positions := []HLPosition{{Coin: "ETH", Size: 10}, {Coin: "BTC", Size: 2}}
	forceCloseHyperliquidLive(t.Context(), positions,
		[]StrategyConfig{hedgeTestConfig()}, nil, closer, nil)

	if closed["BTC"] {
		t.Fatal("a foreign position on a declared-but-flat hedge coin must NOT be liquidated")
	}
	if !closed["ETH"] {
		t.Fatal("the primary must still be closed")
	}
}

func TestSnapshotHyperliquidVirtualQuantitiesIncludesHedgeLegs(t *testing.T) {
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	snap := snapshotHyperliquidVirtualQuantities(
		map[string]*StrategyState{"eth-long": s}, []StrategyConfig{hedgeTestConfig()})

	if snap["ETH"]["eth-long"] != 10 {
		t.Fatalf("primary qty = %v, want 10", snap["ETH"]["eth-long"])
	}
	if snap["BTC"]["eth-long"] != 0.4 {
		t.Fatalf("hedge qty = %v, want 0.4 — a missing claim leaves the fill nothing to decrement", snap["BTC"]["eth-long"])
	}
}

func TestApplyHyperliquidKillSwitchHedgeFillBooksAgainstOwner(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	fills := map[string]HyperliquidCloseFill{"BTC": {AvgPx: 48000, TotalSz: 0.4, Fee: 5, OID: 91}}
	if !applyHyperliquidKillSwitchHedgeFill(s, sc, fills) {
		t.Fatal("hedge fill was not booked")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("hedge leg must be closed by the kill-switch fill")
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Fatalf("streak = %d — a kill-switch hedge close must not touch it", s.RiskState.ConsecutiveLosses)
	}
}

func TestApplyHyperliquidKillSwitchHedgeFillIsIdempotentOnOID(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	fills := map[string]HyperliquidCloseFill{"BTC": {AvgPx: 48000, TotalSz: 0.4, Fee: 5, OID: 91}}

	applyHyperliquidKillSwitchHedgeFill(s, sc, fills)
	rowsAfterFirst := len(s.TradeHistory)
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	applyHyperliquidKillSwitchHedgeFill(s, sc, fills)

	if len(s.TradeHistory) != rowsAfterFirst {
		t.Fatalf("trade rows = %d, want %d — the same fill OID must book once", len(s.TradeHistory), rowsAfterFirst)
	}
}

func TestApplyHyperliquidKillSwitchHedgeFillIgnoresUnstampedPosition(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 1, AvgCost: testHedgePx, Side: "long", Multiplier: 1}
	fills := map[string]HyperliquidCloseFill{"BTC": {AvgPx: 48000, TotalSz: 1, Fee: 5, OID: 92}}

	if applyHyperliquidKillSwitchHedgeFill(s, sc, fills) {
		t.Fatal("an unstamped position must not be booked as a hedge close")
	}
}

func TestSetHyperliquidCircuitBreakerPendingIncludesHedgeLeg(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 10}, {Coin: "BTC", Size: -0.4}},
		HLLiveAll:   []StrategyConfig{sc},
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)

	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil {
		t.Fatal("no pending circuit close was set")
	}
	if len(p.Symbols) != 2 {
		t.Fatalf("pending symbols = %v, want both the primary and the hedge — flattening only the primary leaves naked INVERSE exposure", p.Symbols)
	}
	got := map[string]float64{}
	for _, sym := range p.Symbols {
		got[sym.Symbol] = sym.Size
	}
	if got["ETH"] != 10 || got["BTC"] != 0.4 {
		t.Fatalf("pending sizes = %v, want ETH 10 / BTC 0.4", got)
	}
}

func TestSetHyperliquidCircuitBreakerPendingSkipsFlatHedge(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")

	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 10}, {Coin: "BTC", Size: -3}},
		HLLiveAll:   []StrategyConfig{sc},
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)

	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil || len(p.Symbols) != 1 || p.Symbols[0].Symbol != "ETH" {
		t.Fatalf("pending = %v, want only the primary when no hedge leg is held", p)
	}
}

func TestCollectPerpsMarkSymbolsIncludesHedgeCoins(t *testing.T) {
	hl, _ := collectPerpsMarkSymbols([]StrategyConfig{hedgeTestConfig()})
	found := map[string]bool{}
	for _, c := range hl {
		found[c] = true
	}
	if !found["ETH"] || !found["BTC"] {
		t.Fatalf("hl mark coins = %v, want both ETH and BTC — without a hedge mark the leg is valued at AvgCost and its loss is invisible", hl)
	}
}

func TestBuildSharedWalletBooksAttributesHedgeLegToOwner(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	state := &AppState{Strategies: map[string]*StrategyState{"eth-long": s}}

	_, virtualQty := buildSharedWalletBooks(
		SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"},
		[]string{"eth-long"},
		map[string]StrategyConfig{"eth-long": sc},
		state,
	)
	if virtualQty["BTC"]["eth-long"] != 0.4 {
		t.Fatalf("hedge claim = %v, want 0.4 — without it the coin reads as an orphan and drift alerts fire every cycle", virtualQty["BTC"])
	}
	if virtualQty["ETH"]["eth-long"] != 10 {
		t.Fatalf("primary claim = %v, want 10", virtualQty["ETH"])
	}
}

func TestHedgeCoinsForStrategiesIsSortedAndDeduped(t *testing.T) {
	got := hedgeCoinsForStrategies([]StrategyConfig{
		withHedge(hedgePerpsStrategy("a", "ETH"), &HedgeConfig{Enabled: true, Symbol: "SOL"}),
		withHedge(hedgePerpsStrategy("b", "AVAX"), &HedgeConfig{Enabled: true, Symbol: "BTC"}),
		hedgePerpsStrategy("c", "LINK"),
		withHedge(hedgePerpsStrategy("d", "OP"), &HedgeConfig{Enabled: false, Symbol: "DOGE"}),
	})
	if len(got) != 2 || got[0] != "BTC" || got[1] != "SOL" {
		t.Fatalf("hedge coins = %v, want sorted [BTC SOL] with the disabled block excluded", got)
	}
}

func TestHedgeStatusLineDescribesConfigAndLeg(t *testing.T) {
	sc := hedgeTestConfig()
	if line := hedgeStatusLine(sc, nil); !strings.Contains(line, "hedge=BTC") || !strings.Contains(line, "cross") {
		t.Fatalf("config-only line = %q", line)
	}
	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	line := hedgeStatusLine(sc, s)
	if !strings.Contains(line, "coupled to ETH") {
		t.Fatalf("held-leg line = %q, want the coupling stated so it is not mistaken for an unmanaged position", line)
	}
	if line := hedgeStatusLine(hedgePerpsStrategy("plain", "ETH"), nil); line != "" {
		t.Fatalf("no hedge → %q, want empty", line)
	}
}

func TestBuildHedgeStatusResolvesDefaultsAndHeldLeg(t *testing.T) {
	sc := withHedge(hedgePerpsStrategy("eth-long", "ETH"), &HedgeConfig{Enabled: true, Symbol: "btc"})
	hs := buildHedgeStatus(sc, nil)
	if hs == nil || hs.Symbol != "BTC" || hs.Ratio != 1 || hs.Leverage != 1 || hs.MarginMode != "isolated" {
		t.Fatalf("resolved status = %+v", hs)
	}
	if hs.Held {
		t.Fatal("no state → Held must be false")
	}
	s := hedgeTestState("eth-long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	hs = buildHedgeStatus(sc, s)
	if !hs.Held || hs.Quantity != 0.4 || hs.CoupledTo != "ETH" || hs.QtyBasis != 10 {
		t.Fatalf("held status = %+v", hs)
	}
	if buildHedgeStatus(hedgePerpsStrategy("plain", "ETH"), nil) != nil {
		t.Fatal("no hedge block → nil status")
	}
}

func TestManualCloseTradeTypeLabelsHedgeLegs(t *testing.T) {
	if got := manualCloseTradeType(primaryPos(10, "long")); got != "perps" {
		t.Fatalf("primary → %q, want perps", got)
	}
	if got := manualCloseTradeType(hedgePos(0.4, "short", 10)); got != hedgeTradeType {
		t.Fatalf("hedge → %q, want %q", got, hedgeTradeType)
	}
}

func TestHedgeFieldsRoundTripThroughSQLite(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {
			ID: "eth-long", Type: "perps", Platform: "hyperliquid", Cash: 5000,
			Positions: map[string]*Position{
				"ETH": primaryPos(10, "long"),
				"BTC": hedgePos(0.4, "short", 10),
			},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got := loaded.Strategies["eth-long"].Positions["BTC"]
	if got == nil {
		t.Fatal("hedge leg did not survive the round trip")
	}
	if got.HedgeFor != "ETH" {
		t.Fatalf("HedgeFor = %q, want ETH", got.HedgeFor)
	}
	if got.HedgePrimaryQtyBasis != 10 {
		t.Fatalf("HedgePrimaryQtyBasis = %v, want 10", got.HedgePrimaryQtyBasis)
	}
	if !got.isHedgeLeg() {
		t.Fatal("restored leg must still identify as a hedge")
	}
	if loaded.Strategies["eth-long"].Positions["ETH"].isHedgeLeg() {
		t.Fatal("primary position must not carry a hedge stamp")
	}
}

func TestHedgeHotReloadBlockedWhileOpenAllowedWhenFlat(t *testing.T) {
	mk := func(h *HedgeConfig) *Config {
		sc := hedgePerpsStrategy("hl-eth", "ETH")
		sc.Capital = 1000
		sc.MaxDrawdownPct = 10
		sc.Leverage = 5
		sc.MarginMode = "isolated"
		sc.Hedge = h
		return minimalReloadConfig([]StrategyConfig{sc})
	}
	withLeg := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{"BTC": hedgePos(0.4, "short", 10)}},
	}}
	flat := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}

	base := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}
	changes := []struct {
		name string
		next *HedgeConfig
	}{
		{"symbol repointed", &HedgeConfig{Enabled: true, Symbol: "SOL", Ratio: 1}},
		{"ratio changed", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5}},
		{"disabled", &HedgeConfig{Enabled: false, Symbol: "BTC", Ratio: 1}},
		{"block removed", nil},
	}
	for _, tc := range changes {
		t.Run(tc.name+" blocked while a leg is open", func(t *testing.T) {
			err := validateHotReloadStateCompatible(mk(base), mk(tc.next), withLeg)
			if err == nil || !strings.Contains(err.Error(), "hedge block changed with open positions") {
				t.Fatalf("err = %v, want the hedge block change to be refused", err)
			}
		})
		t.Run(tc.name+" allowed when flat", func(t *testing.T) {
			if err := validateHotReloadStateCompatible(mk(base), mk(tc.next), flat); err != nil {
				t.Fatalf("flat reload must be allowed, got %v", err)
			}
		})
	}
}

func TestHedgeBlockIsNotRestartRequired(t *testing.T) {
	a := hedgePerpsStrategy("hl-eth", "ETH")
	b := a
	b.Hedge = &HedgeConfig{Enabled: true, Symbol: "BTC"}
	if !reflect.DeepEqual(strategyRestartShape(a), strategyRestartShape(b)) {
		t.Fatal("a hedge-block edit must be hot-reloadable, not restart-required")
	}
}

func TestHotReloadRejectsIntroducedHedgeCollision(t *testing.T) {
	mk := func(h *HedgeConfig) *Config {
		eth := hedgePerpsStrategy("hl-eth", "ETH")
		eth.Capital, eth.MaxDrawdownPct, eth.Leverage, eth.MarginMode = 1000, 10, 5, "isolated"
		eth.Hedge = h
		btc := hedgePerpsStrategy("hl-btc", "BTC")
		btc.Capital, btc.MaxDrawdownPct, btc.Leverage, btc.MarginMode = 1000, 10, 5, "isolated"
		return minimalReloadConfig([]StrategyConfig{eth, btc})
	}
	err := validateHotReloadCompatible(mk(nil), mk(&HedgeConfig{Enabled: true, Symbol: "BTC"}))
	if err == nil || !strings.Contains(err.Error(), "is the primary coin of strategy/strategies hl-btc") {
		t.Fatalf("err = %v, want the introduced collision to be refused", err)
	}
}

func TestLifetimeTradeStatsExcludeHedgeLegs(t *testing.T) {
	sdb := openTestDB(t)
	now := time.Now().UTC()
	trades := []Trade{
		{StrategyID: "eth-long", Timestamp: now, Symbol: "ETH", PositionID: "p1", Side: "buy", Quantity: 10, Price: 2000, Value: 20000, TradeType: "perps", Details: "Open long"},
		{StrategyID: "eth-long", Timestamp: now.Add(time.Second), Symbol: "ETH", PositionID: "p1", Side: "sell", Quantity: 10, Price: 2100, Value: 21000, TradeType: "perps", Details: "Close long", IsClose: true, RealizedPnL: 1000, PnLGross: true},
		{StrategyID: "eth-long", Timestamp: now.Add(2 * time.Second), Symbol: "BTC", PositionID: "h1", Side: "sell", Quantity: 0.4, Price: 50000, Value: 20000, TradeType: "hedge", Details: "hedge(ETH) open"},
		{StrategyID: "eth-long", Timestamp: now.Add(3 * time.Second), Symbol: "BTC", PositionID: "h1", Side: "buy", Quantity: 0.4, Price: 52000, Value: 20800, TradeType: "hedge", Details: "hedge(ETH) close", IsClose: true, RealizedPnL: -800, PnLGross: true},
	}
	for _, tr := range trades {
		if err := sdb.InsertTrade(tr.StrategyID, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	stats, err := sdb.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	got := stats["eth-long"]
	if got.PositionsOpened != 1 {
		t.Fatalf("PositionsOpened = %d, want 1 — the hedge open is not a new round trip", got.PositionsOpened)
	}
	if got.Wins != 1 || got.Losses != 0 {
		t.Fatalf("W/L = %d/%d, want 1/0 — the hedge's mirror-image loss must not be counted", got.Wins, got.Losses)
	}
	single, err := sdb.LifetimeTradeStatsForStrategy("eth-long")
	if err != nil {
		t.Fatalf("LifetimeTradeStatsForStrategy: %v", err)
	}
	if single != got {
		t.Fatalf("per-strategy stats %+v disagree with the all-strategies query %+v", single, got)
	}
}

func TestEveryHedgeLegCarriesTheHedgeTradeType(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	assertAllHedgeRows := func(t *testing.T, s *StrategyState, wantRows int) {
		t.Helper()
		if len(s.TradeHistory) != wantRows {
			t.Fatalf("trade rows = %d, want %d", len(s.TradeHistory), wantRows)
		}
		for i, tr := range s.TradeHistory {
			if tr.TradeType != hedgeTradeType {
				t.Fatalf("row %d (%s %s) trade_type = %q, want %q", i, tr.Side, tr.Symbol, tr.TradeType, hedgeTradeType)
			}
		}
	}

	t.Run("open then reduce then close", func(t *testing.T) {
		sc := hedgeTestConfig()
		s := hedgeTestState("eth-long")
		s.Positions["ETH"] = primaryPos(10, "long")
		log := silentStrategyLogger("eth-long")

		applyHedgeFill(sc, s, "ETH", hedgeAction{Kind: hedgeActionOpen, Qty: 0.4, Side: "sell", HedgeSide: "short", NewBasis: 10}, 0.4, testHedgePx, 10, true, "1", log)
		applyHedgeFill(sc, s, "ETH", hedgeAction{Kind: hedgeActionReduce, Qty: 0.2, NewBasis: 5}, 0.2, 51000, 5, true, "2", log)
		applyHedgeFill(sc, s, "ETH", hedgeAction{Kind: hedgeActionCloseFull, Qty: 0.2, NewBasis: 0}, 0.2, 52000, 5, true, "3", log)

		assertAllHedgeRows(t, s, 3)
	})

	t.Run("corrupt-position clear", func(t *testing.T) {
		s := hedgeTestState("eth-long")
		bad := hedgePos(0, "short", 10)
		bad.AvgCost = 0
		s.Positions["BTC"] = bad
		bookPerpsCloseWithFillFee(s, "BTC", 50000, 0, false, "", hedgeCloseCloseReason, "hedge(ETH) close", "hedge", silentStrategyLogger("eth-long"))
		assertAllHedgeRows(t, s, 1)
	})

	t.Run("circuit-breaker virtual force-close sweep", func(t *testing.T) {
		s := hedgeTestState("eth-long")
		s.Positions["BTC"] = hedgePos(0.4, "short", 10)
		forceCloseAllPositions(s, nil, map[string]float64{"BTC": 51000}, silentStrategyLogger("eth-long"))
		assertAllHedgeRows(t, s, 1)
	})

	t.Run("kill-switch on-chain fill", func(t *testing.T) {
		sc := hedgeTestConfig()
		s := hedgeTestState("eth-long")
		s.Positions["BTC"] = hedgePos(0.4, "short", 10)
		applyHyperliquidKillSwitchHedgeFill(s, sc, map[string]HyperliquidCloseFill{
			"BTC": {AvgPx: 51000, TotalSz: 0.4, Fee: 5, OID: 500},
		})
		assertAllHedgeRows(t, s, 1)
	})

	t.Run("primary legs stay perps", func(t *testing.T) {
		s := hedgeTestState("eth-long")
		s.Positions["ETH"] = primaryPos(10, "long")
		bookPerpsCloseWithFillFee(s, "ETH", 2100, 4, true, "9", "signal", "Close long", "close", silentStrategyLogger("eth-long"))
		if len(s.TradeHistory) != 1 || s.TradeHistory[0].TradeType != "perps" {
			t.Fatalf("primary close must stay trade_type=perps, got %+v", s.TradeHistory)
		}
	})
}

func TestApplyHedgeFillBooksActualFillNotRequestedSize(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	t.Run("partial open anchors the basis to the covered share", func(t *testing.T) {
		sc := hedgeTestConfig()
		s := hedgeTestState("eth-long")
		s.Positions["ETH"] = primaryPos(10, "long")
		act := hedgeAction{Kind: hedgeActionOpen, Qty: 0.4, Side: "sell", HedgeSide: "short", NewBasis: 10}

		applyHedgeFill(sc, s, "ETH", act, 0.1, testHedgePx, 3, true, "1", silentStrategyLogger("eth-long"))

		pos := s.Positions["BTC"]
		if math.Abs(pos.Quantity-0.1) > 1e-9 {
			t.Fatalf("qty = %v, want the filled 0.1, not the requested 0.4", pos.Quantity)
		}
		if math.Abs(pos.HedgePrimaryQtyBasis-2.5) > 1e-9 {
			t.Fatalf("basis = %v, want 2.5 — a quarter fill hedges a quarter of the primary", pos.HedgePrimaryQtyBasis)
		}
		if s.TradeHistory[0].Quantity != 0.1 {
			t.Fatalf("trade qty = %v, want 0.1", s.TradeHistory[0].Quantity)
		}
		snap := hedgeSnapshot{
			PrimarySymbol: "ETH", PrimaryQty: 10, PrimarySide: "long",
			HedgeSymbol: "BTC", HedgeHeld: true, HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2.5,
		}
		if act := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx); act.Kind != hedgeActionAdd {
			t.Fatalf("follow-up = %v, want add to cover the shortfall (%s)", act.Kind, act.Reason)
		}
	})

	t.Run("partial full-close books a partial, not a full close", func(t *testing.T) {
		sc := hedgeTestConfig()
		s := hedgeTestState("eth-long")
		s.Positions["BTC"] = hedgePos(0.4, "short", 10)
		act := hedgeAction{Kind: hedgeActionCloseFull, Qty: 0.4, NewBasis: 0}

		applyHedgeFill(sc, s, "ETH", act, 0.15, 51000, 3, true, "2", silentStrategyLogger("eth-long"))

		pos := s.Positions["BTC"]
		if pos == nil {
			t.Fatal("a short-filled close must NOT mark the leg flat — real exposure remains on-chain")
		}
		if math.Abs(pos.Quantity-0.25) > 1e-9 {
			t.Fatalf("remaining = %v, want 0.25", pos.Quantity)
		}
	})

	t.Run("over-report is clamped to the requested size", func(t *testing.T) {
		sc := hedgeTestConfig()
		s := hedgeTestState("eth-long")
		s.Positions["ETH"] = primaryPos(10, "long")
		act := hedgeAction{Kind: hedgeActionOpen, Qty: 0.4, Side: "sell", HedgeSide: "short", NewBasis: 10}

		applyHedgeFill(sc, s, "ETH", act, 5, testHedgePx, 3, true, "3", silentStrategyLogger("eth-long"))

		if got := s.Positions["BTC"].Quantity; math.Abs(got-0.4) > 1e-9 {
			t.Fatalf("qty = %v, want the requested 0.4 (an over-report must be clamped)", got)
		}
	})

	t.Run("zero fill books nothing", func(t *testing.T) {
		sc := hedgeTestConfig()
		s := hedgeTestState("eth-long")
		s.Positions["ETH"] = primaryPos(10, "long")
		act := hedgeAction{Kind: hedgeActionOpen, Qty: 0.4, Side: "sell", HedgeSide: "short", NewBasis: 10}

		applyHedgeFill(sc, s, "ETH", act, 0, testHedgePx, 3, true, "4", silentStrategyLogger("eth-long"))

		if _, ok := s.Positions["BTC"]; ok {
			t.Fatal("a zero fill must not create a position")
		}
		if len(s.TradeHistory) != 0 {
			t.Fatalf("a zero fill must not record a trade, got %d rows", len(s.TradeHistory))
		}
	})
}

func TestRunHedgeSyncBooksExchangeReportedFillSize(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	var mu sync.RWMutex
	f := &fakeHedgeExec{openResult: execFill(testHedgePx, 0.25, 6)}

	runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
		PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, Live: true,
	}, nil, silentStrategyLogger("eth-long"))

	if got := s.Positions["BTC"].Quantity; math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("booked qty = %v, want the exchange's 0.25", got)
	}
}

func TestPartialHedgeReduceLeavesDeltaForTheNextCycle(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	log := silentStrategyLogger("eth-long")

	newState := func() *StrategyState {
		s := hedgeTestState("eth-long")
		s.Positions["ETH"] = primaryPos(5, "long")
		s.Positions["BTC"] = hedgePos(0.4, "short", 10)
		return s
	}

	t.Run("half-filled reduce still shows a delta next cycle", func(t *testing.T) {
		s := newState()
		act := hedgeAction{Kind: hedgeActionReduce, Qty: 0.2, NewBasis: 5}
		applyHedgeFill(sc, s, "ETH", act, 0.1, 51000, 2, true, "r1", log)

		pos := s.Positions["BTC"]
		if math.Abs(pos.Quantity-0.3) > 1e-9 {
			t.Fatalf("hedge qty = %v, want 0.3 (only half the reduce filled)", pos.Quantity)
		}
		if math.Abs(pos.HedgePrimaryQtyBasis-7.5) > 1e-9 {
			t.Fatalf("basis = %v, want 7.5 — a half-filled reduce must advance the watermark half way", pos.HedgePrimaryQtyBasis)
		}

		snap := hedgeSnapshotFromState(sc, s)
		next := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
		if next.Kind != hedgeActionReduce {
			t.Fatalf("follow-up = %v, want reduce — the surplus must not be abandoned (%s)", next.Kind, next.Reason)
		}
		if math.Abs(next.Qty-0.1) > 1e-9 {
			t.Fatalf("follow-up reduce qty = %v, want 0.1 (0.3 held − 0.2 target)", next.Qty)
		}
	})

	t.Run("two consecutive partials converge without compounding", func(t *testing.T) {
		s := newState()
		applyHedgeFill(sc, s, "ETH", hedgeAction{Kind: hedgeActionReduce, Qty: 0.2, NewBasis: 5}, 0.1, 51000, 2, true, "r1", log)

		snap := hedgeSnapshotFromState(sc, s)
		act2 := hedgeTargetDecision(sc, snap, testPrimaryPx, testHedgePx)
		applyHedgeFill(sc, s, "ETH", act2, act2.Qty/2, 51000, 1, true, "r2", log)

		pos := s.Positions["BTC"]
		if math.Abs(pos.Quantity-0.25) > 1e-9 {
			t.Fatalf("hedge qty = %v, want 0.25", pos.Quantity)
		}
		act3 := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, s), testPrimaryPx, testHedgePx)
		if act3.Kind != hedgeActionReduce {
			t.Fatalf("third-cycle action = %v, want reduce (%s)", act3.Kind, act3.Reason)
		}
		if math.Abs(act3.Qty-0.05) > 1e-9 {
			t.Fatalf("third-cycle reduce qty = %v, want 0.05", act3.Qty)
		}
	})

	t.Run("fully-filled reduce still lands exactly on the target basis", func(t *testing.T) {
		s := newState()
		act := hedgeAction{Kind: hedgeActionReduce, Qty: 0.2, NewBasis: 5}
		applyHedgeFill(sc, s, "ETH", act, 0.2, 51000, 2, true, "r1", log)

		pos := s.Positions["BTC"]
		if pos.HedgePrimaryQtyBasis != 5 {
			t.Fatalf("basis = %v, want exactly 5 on a full fill", pos.HedgePrimaryQtyBasis)
		}
		if next := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, s), testPrimaryPx, testHedgePx); next.Kind != hedgeActionNone {
			t.Fatalf("follow-up = %v, want none — a full fill is in sync (%s)", next.Kind, next.Reason)
		}
	})

	t.Run("short-filled residual full-close keeps a delta", func(t *testing.T) {
		s := hedgeTestState("eth-long")
		s.Positions["ETH"] = primaryPos(0.001, "long")
		s.Positions["BTC"] = hedgePos(0.4, "short", 10)

		act := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, s), testPrimaryPx, testHedgePx)
		if act.Kind != hedgeActionCloseFull {
			t.Fatalf("setup: expected the residual-below-minimum full close, got %v (%s)", act.Kind, act.Reason)
		}
		applyHedgeFill(sc, s, "ETH", act, 0.2, 51000, 2, true, "c1", log)

		pos := s.Positions["BTC"]
		if pos == nil {
			t.Fatal("a short-filled close must leave the remaining leg")
		}
		if math.Abs(pos.Quantity-0.2) > 1e-9 {
			t.Fatalf("hedge qty = %v, want 0.2", pos.Quantity)
		}
		next := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, s), testPrimaryPx, testHedgePx)
		if next.Kind == hedgeActionNone {
			t.Fatalf("follow-up = none — the surplus was abandoned (basis=%v, %s)", pos.HedgePrimaryQtyBasis, next.Reason)
		}
	})

	t.Run("primary-flat close self-heals regardless of fill", func(t *testing.T) {
		s := hedgeTestState("eth-long")
		s.Positions["BTC"] = hedgePos(0.4, "short", 10)
		act := hedgeAction{Kind: hedgeActionCloseFull, Qty: 0.4, NewBasis: 0}
		applyHedgeFill(sc, s, "ETH", act, 0.15, 51000, 2, true, "c2", log)

		next := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, s), testPrimaryPx, testHedgePx)
		if next.Kind != hedgeActionCloseFull {
			t.Fatalf("follow-up = %v, want closeFull — a flat primary must keep flattening (%s)", next.Kind, next.Reason)
		}
	})
}

func TestHedgeReducedBasisInterpolatesByFillRatio(t *testing.T) {
	cases := []struct {
		name                                string
		oldBasis, target, filled, requested float64
		want                                float64
	}{
		{"full fill lands on target", 10, 5, 0.2, 0.2, 5},
		{"over-fill clamps to target", 10, 5, 0.3, 0.2, 5},
		{"half fill interpolates", 10, 5, 0.1, 0.2, 7.5},
		{"quarter fill interpolates", 10, 5, 0.05, 0.2, 8.75},
		{"zero fill holds the old basis", 10, 5, 0, 0.2, 10},
		{"unanchored basis falls through to target", 0, 5, 0.1, 0.2, 5},
		{"zero request falls through to target", 10, 5, 0.1, 0, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hedgeReducedBasis(tc.oldBasis, tc.target, tc.filled, tc.requested)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("hedgeReducedBasis(%v,%v,%v,%v) = %v, want %v", tc.oldBasis, tc.target, tc.filled, tc.requested, got, tc.want)
			}
		})
	}
}

func TestHedgeBasisAfterPartialReduceScalesByHeldQuantity(t *testing.T) {
	cases := []struct {
		name                        string
		oldBasis, preQty, remainQty float64
		want                        float64
	}{
		{"half the leg remains", 10, 0.4, 0.2, 5},
		{"three quarters remain", 10, 0.4, 0.3, 7.5},
		{"nothing filled leaves the basis", 10, 0.4, 0.4, 10},
		{"fully drained", 10, 0.4, 0, 0},
		{"over-report clamps to the old basis", 10, 0.4, 0.5, 10},
		{"unanchored basis untouched", 0, 0.4, 0.2, 0},
		{"zero pre-size untouched", 10, 0, 0.2, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hedgeBasisAfterPartialReduce(tc.oldBasis, tc.preQty, tc.remainQty)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("= %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHeldQuantityAndFillRatioBasisRulesAgree(t *testing.T) {
	for _, filled := range []float64{0.2, 0.15, 0.1, 0.05} {
		byRatio := hedgeReducedBasis(10, 5, filled, 0.2)
		byHeld := hedgeBasisAfterPartialReduce(10, 0.4, 0.4-filled)
		if math.Abs(byRatio-byHeld) > 1e-9 {
			t.Fatalf("filled %v: ratio rule %v vs held rule %v — the two derivations must agree", filled, byRatio, byHeld)
		}
	}
}

func TestHedgeIsInverseOfPrimaryOnChain(t *testing.T) {
	cases := []struct {
		name      string
		positions []HLPosition
		want      bool
	}{
		{"long primary / short hedge", []HLPosition{{Coin: "ETH", Size: 10}, {Coin: "BTC", Size: -0.4}}, true},
		{"short primary / long hedge", []HLPosition{{Coin: "ETH", Size: -10}, {Coin: "BTC", Size: 0.4}}, true},
		{"same side is not a hedge", []HLPosition{{Coin: "ETH", Size: 10}, {Coin: "BTC", Size: 0.4}}, false},
		{"missing hedge position", []HLPosition{{Coin: "ETH", Size: 10}}, false},
		{"missing primary position", []HLPosition{{Coin: "BTC", Size: -0.4}}, false},
		{"flat hedge", []HLPosition{{Coin: "ETH", Size: 10}, {Coin: "BTC", Size: 0}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hedgeIsInverseOfPrimaryOnChain("ETH", "BTC", tc.positions); got != tc.want {
				t.Fatalf("= %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCircuitBreakerFireWithFailedFetchThenRecoveryClosesBothLegs(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	assist := &PlatformRiskAssist{HLPositions: nil, HLLiveAll: []StrategyConfig{sc}}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)
	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Fatal("setup: no pending should be set when the on-chain snapshot is empty")
	}
	if !shouldForceCloseAllPositionsOnCircuitBreaker(&sc, assist) {
		t.Fatal("setup: a sole-owner strategy still force-closes virtually")
	}
	forceCloseAllPositions(s, nil, map[string]float64{"ETH": testPrimaryPx, "BTC": testHedgePx}, silentStrategyLogger("eth-long"))
	if len(s.Positions) != 0 {
		t.Fatalf("setup: the sweep must clear both virtual legs, got %d", len(s.Positions))
	}

	positions := []HLPosition{{Coin: "ETH", Size: 10}, {Coin: "BTC", Size: -0.4}}
	if hCoin := heldHedgeCoin(sc, s); hCoin != "" {
		t.Fatal("precondition: virtual state cannot name the hedge here — that is the whole bug")
	}
	hQty, hok := computeHyperliquidCircuitCloseQty(hedgeCoin(sc), sc.ID, positions, []StrategyConfig{sc})
	if !hok || math.Abs(hQty-0.4) > 1e-9 {
		t.Fatalf("config-derived hedge close qty = %v (ok=%v), want 0.4", hQty, hok)
	}
	if !hedgeIsInverseOfPrimaryOnChain("ETH", hedgeCoin(sc), positions) {
		t.Fatal("the live inverse hedge must pass the discriminator")
	}

	foreign := []HLPosition{{Coin: "ETH", Size: 10}, {Coin: "BTC", Size: 2}}
	if hedgeIsInverseOfPrimaryOnChain("ETH", hedgeCoin(sc), foreign) {
		t.Fatal("a same-side position on the hedge coin must never be closed as a hedge")
	}
}

func TestLossStreakCircuitBreakerWithFailedFetchLeavesNoPending(t *testing.T) {
	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)
	s.RiskState.ConsecutiveLosses = 99

	setHyperliquidCircuitBreakerPending(&sc, s, &PlatformRiskAssist{HLLiveAll: []StrategyConfig{sc}})
	if s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Fatal("an empty on-chain snapshot must not produce a pending close")
	}
	if hedgeCoin(sc) != "BTC" {
		t.Fatalf("hedge coin = %q, want BTC from config", hedgeCoin(sc))
	}
}

func TestExternallyReducedHedgeIsRegrown(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	var alerts []string
	reconcileHyperliquidHedgeLeg(sc, s, []HLPosition{{Coin: "BTC", Size: -0.2, EntryPrice: testHedgePx}},
		noFillFeeResolver, silentStrategyLogger("eth-long"), nil, &alerts)

	pos := s.Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.2) > 1e-9 {
		t.Fatalf("leg = %v, want resynced to 0.2", pos)
	}
	if math.Abs(pos.HedgePrimaryQtyBasis-5) > 1e-9 {
		t.Fatalf("basis = %v, want 5 — it must shrink with the leg so the shortfall is visible", pos.HedgePrimaryQtyBasis)
	}
	act := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, s), testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionAdd {
		t.Fatalf("follow-up = %v, want add — the lost hedge must be rebuilt (%s)", act.Kind, act.Reason)
	}
	if math.Abs(act.Qty-0.2) > 1e-9 {
		t.Fatalf("re-grow qty = %v, want 0.2 (back to the full 0.4)", act.Qty)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "RE-GROW") {
		t.Fatalf("alerts = %v, want the re-grow notice", alerts)
	}
}

func TestExternallyINCREASEDHedgeIsLeftAlone(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	var alerts []string
	reconcileHyperliquidHedgeLeg(sc, s, []HLPosition{{Coin: "BTC", Size: -0.9, EntryPrice: testHedgePx}},
		noFillFeeResolver, silentStrategyLogger("eth-long"), nil, &alerts)

	pos := s.Positions["BTC"]
	if pos.HedgePrimaryQtyBasis != 10 {
		t.Fatalf("basis = %v, want the original 10 — an upward resync must not move it", pos.HedgePrimaryQtyBasis)
	}
	if act := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, s), testPrimaryPx, testHedgePx); act.Kind != hedgeActionNone {
		t.Fatalf("follow-up = %v, want none — surplus we never opened is not ours to trade (%s)", act.Kind, act.Reason)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "EXCEEDS") {
		t.Fatalf("alerts = %v, want the surplus notice", alerts)
	}
}

func TestPrimaryReduceAfterExternalHedgeReductionSizesOffTheShrunkBasis(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	s := hedgeTestState("eth-long")
	s.Positions["ETH"] = primaryPos(10, "long")
	s.Positions["BTC"] = hedgePos(0.4, "short", 10)

	var alerts []string
	reconcileHyperliquidHedgeLeg(sc, s, []HLPosition{{Coin: "BTC", Size: -0.2, EntryPrice: testHedgePx}},
		noFillFeeResolver, silentStrategyLogger("eth-long"), nil, &alerts)

	s.Positions["ETH"].Quantity = 2.5
	act := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, s), testPrimaryPx, testHedgePx)
	if act.Kind != hedgeActionReduce {
		t.Fatalf("kind = %v, want reduce (%s)", act.Kind, act.Reason)
	}
	if math.Abs(act.Qty-0.1) > 1e-9 {
		t.Fatalf("reduce qty = %v, want 0.1", act.Qty)
	}
}

func TestManualDrainReAnchorsHedgeBasisProportionally(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	scByID := map[string]StrategyConfig{"eth-long": sc}

	newState := func() *AppState {
		s := hedgeTestState("eth-long")
		s.Positions["ETH"] = primaryPos(6, "long")
		s.Positions["BTC"] = hedgePos(0.4, "short", 10)
		return &AppState{Strategies: map[string]*StrategyState{"eth-long": s}}
	}
	drainHedgeClose := func(st *AppState, filled float64, full bool) error {
		return applyManualAction(st, nil, scByID, PendingManualAction{
			StrategyID:  "eth-long",
			Action:      "close",
			Symbol:      "BTC",
			Side:        "buy",
			Quantity:    filled,
			FillPrice:   51000,
			FillFee:     1,
			RealizedPnL: -5,
			IsFullClose: full,
			CreatedAt:   time.Now().UTC(),
		})
	}

	t.Run("short fill leaves a delta for the next cycle", func(t *testing.T) {
		st := newState()
		if err := drainHedgeClose(st, 0.08, false); err != nil {
			t.Fatalf("drain: %v", err)
		}
		pos := st.Strategies["eth-long"].Positions["BTC"]
		if math.Abs(pos.Quantity-0.32) > 1e-9 {
			t.Fatalf("hedge qty = %v, want 0.32", pos.Quantity)
		}
		if math.Abs(pos.HedgePrimaryQtyBasis-8) > 1e-9 {
			t.Fatalf("basis = %v, want 8 — a short fill must not claim alignment with the primary's 6", pos.HedgePrimaryQtyBasis)
		}
		act := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, st.Strategies["eth-long"]), testPrimaryPx, testHedgePx)
		if act.Kind != hedgeActionReduce {
			t.Fatalf("follow-up = %v, want reduce — the surplus must be trimmed (%s)", act.Kind, act.Reason)
		}
	})

	t.Run("full fill lands exactly on the primary quantity", func(t *testing.T) {
		st := newState()
		if err := drainHedgeClose(st, 0.16, false); err != nil {
			t.Fatalf("drain: %v", err)
		}
		pos := st.Strategies["eth-long"].Positions["BTC"]
		if math.Abs(pos.HedgePrimaryQtyBasis-6) > 1e-9 {
			t.Fatalf("basis = %v, want 6 (the primary's post-reduce qty)", pos.HedgePrimaryQtyBasis)
		}
		if act := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, st.Strategies["eth-long"]), testPrimaryPx, testHedgePx); act.Kind != hedgeActionNone {
			t.Fatalf("follow-up = %v, want none — a full fill is in sync (%s)", act.Kind, act.Reason)
		}
	})

	t.Run("two consecutive short fills re-base off the true remaining size", func(t *testing.T) {
		st := newState()
		if err := drainHedgeClose(st, 0.08, false); err != nil {
			t.Fatalf("drain 1: %v", err)
		}
		if err := drainHedgeClose(st, 0.08, false); err != nil {
			t.Fatalf("drain 2: %v", err)
		}
		pos := st.Strategies["eth-long"].Positions["BTC"]
		if math.Abs(pos.Quantity-0.24) > 1e-9 {
			t.Fatalf("hedge qty = %v, want 0.24", pos.Quantity)
		}
		if math.Abs(pos.HedgePrimaryQtyBasis-6) > 1e-9 {
			t.Fatalf("basis = %v, want 6 — consecutive partials must converge, not compound", pos.HedgePrimaryQtyBasis)
		}
	})
}

func TestShortFilledCoupledHedgeCloseStaysTracked(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := hedgeTestConfig()
	scByID := map[string]StrategyConfig{"eth-long": sc}

	newState := func() *AppState {
		s := hedgeTestState("eth-long")
		s.Positions["BTC"] = hedgePos(0.4, "short", 10)
		return &AppState{Strategies: map[string]*StrategyState{"eth-long": s}}
	}

	t.Run("60% fill keeps the residue tracked and re-closable", func(t *testing.T) {
		st := newState()
		full := forceCloseCoupledHedgeQueuedFullFlag(t, 0.4, 0.24, true)
		if full {
			t.Fatal("a 60% hedge fill must NOT be queued as a full close, even when the operator fully closed the primary")
		}
		if err := applyManualAction(st, nil, scByID, PendingManualAction{
			StrategyID: "eth-long", Action: "close", Symbol: "BTC", Side: "buy",
			Quantity: 0.24, FillPrice: 51000, FillFee: 1, RealizedPnL: -5,
			IsFullClose: full, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("drain: %v", err)
		}
		pos := st.Strategies["eth-long"].Positions["BTC"]
		if pos == nil {
			t.Fatal("the residue must stay tracked — deleting it strands on-chain exposure")
		}
		if math.Abs(pos.Quantity-0.16) > 1e-9 {
			t.Fatalf("residue = %v, want 0.16", pos.Quantity)
		}
		if pos.HedgeFor != "ETH" {
			t.Fatalf("HedgeFor = %q — the stamp must survive or reconcile refuses to adopt the leg", pos.HedgeFor)
		}
		act := hedgeTargetDecision(sc, hedgeSnapshotFromState(sc, st.Strategies["eth-long"]), testPrimaryPx, testHedgePx)
		if act.Kind != hedgeActionCloseFull {
			t.Fatalf("follow-up = %v, want closeFull (%s)", act.Kind, act.Reason)
		}
	})

	t.Run("100% fill still books as a full close", func(t *testing.T) {
		st := newState()
		full := forceCloseCoupledHedgeQueuedFullFlag(t, 0.4, 0.4, true)
		if !full {
			t.Fatal("a complete hedge fill must be queued as a full close")
		}
		if err := applyManualAction(st, nil, scByID, PendingManualAction{
			StrategyID: "eth-long", Action: "close", Symbol: "BTC", Side: "buy",
			Quantity: 0.4, FillPrice: 51000, FillFee: 1, RealizedPnL: -8,
			IsFullClose: full, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("drain: %v", err)
		}
		if _, ok := st.Strategies["eth-long"].Positions["BTC"]; ok {
			t.Fatal("a complete fill must delete the leg")
		}
	})
}

func forceCloseCoupledHedgeQueuedFullFlag(t *testing.T, heldQty, filled float64, primaryFullClose bool) bool {
	t.Helper()
	db := openTestDB(t)
	sc := hedgeTestConfig()

	deps := manualCoreDeps{
		cfg:     &Config{Strategies: []StrategyConfig{sc}},
		stateDB: db,
		loadState: func(strategyID, symbol string) (manualStateView, error) {
			if symbol != "BTC" {
				return manualStateView{HasStrategy: true}, nil
			}
			return manualStateView{HasStrategy: true, Pos: hedgePos(heldQty, "short", 10)}, nil
		},
		closer: func(symbol string, partialSz *float64, oids []int64) (*HyperliquidCloseResult, error) {
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{AvgPx: 51000, TotalSz: filled, Fee: 1, OID: 7},
			}}, nil
		},
	}
	res := &manualCoreResult{}
	forceCloseCoupledHedgeLeg(deps, sc, res, "eth-long", "ETH", 10, 10, primaryFullClose)

	actions, err := db.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	for _, a := range actions {
		if a.Symbol == "BTC" && a.Action == "close" {
			if math.Abs(a.Quantity-filled) > 1e-9 {
				t.Fatalf("queued hedge close qty = %v, want the actual fill %v", a.Quantity, filled)
			}
			return a.IsFullClose
		}
	}
	t.Fatal("no hedge close action was queued")
	return false
}

func TestStuckCBClosesOrphanedHedgeWhenPrimaryWentFlatDuringTheOutage(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", RiskState: RiskState{
			CircuitBreaker:       true,
			CircuitBreakerUntil:  time.Now().Add(24 * time.Hour),
			PendingCircuitCloses: nil,
		}},
	}}
	cfg := []StrategyConfig{hedgeTestConfig()}
	var mu sync.RWMutex
	var calls []string
	var dms []string
	closer := func(sym string, partialSz *float64, _ []int64) (*HyperliquidCloseResult, error) {
		sz := 0.0
		if partialSz != nil {
			sz = *partialSz
		}
		calls = append(calls, fmt.Sprintf("%s:%g", sym, sz))
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: sz, AvgPx: 1}},
			Platform: "hyperliquid",
		}, nil
	}

	runPendingHyperliquidCircuitCloses(
		context.Background(), state, cfg, "0xabc",
		[]HLPosition{{Coin: "BTC", Size: -0.4, EntryPrice: testHedgePx}},
		true, nil, closer, 30*time.Second, &mu,
		func(msg string) { dms = append(dms, msg) },
	)

	if len(calls) != 1 || calls[0] != "BTC:0.4" {
		t.Fatalf("closer calls = %v, want [BTC:0.4] — the orphaned hedge must still be flattened", calls)
	}
	if len(dms) != 1 || !strings.Contains(dms[0], "orphaned hedge leg closed") {
		t.Fatalf("owner DMs = %v, want the orphaned-hedge CRITICAL alert", dms)
	}
	if !strings.Contains(dms[0], "re-open it and remove") {
		t.Fatalf("the DM must tell the operator what to do if the position was theirs: %q", dms[0])
	}
}

func TestStuckCBClosesBothLegsWhenPrimaryIsStillLive(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", RiskState: RiskState{
			CircuitBreaker: true, CircuitBreakerUntil: time.Now().Add(24 * time.Hour),
		}},
	}}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64, _ []int64) (*HyperliquidCloseResult, error) {
		sz := 0.0
		if partialSz != nil {
			sz = *partialSz
		}
		calls = append(calls, fmt.Sprintf("%s:%g", sym, sz))
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: sz, AvgPx: 1}},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, []StrategyConfig{hedgeTestConfig()}, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 10, EntryPrice: testPrimaryPx}, {Coin: "BTC", Size: -0.4, EntryPrice: testHedgePx}},
		true, nil, closer, 30*time.Second, &mu, nil,
	)
	sort.Strings(calls)
	if len(calls) != 2 || calls[0] != "BTC:0.4" || calls[1] != "ETH:10" {
		t.Fatalf("closer calls = %v, want both legs flattened", calls)
	}
}

func TestStuckCBRefusesSameSideHedgeCoinPositionButStillClosesPrimary(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", RiskState: RiskState{
			CircuitBreaker: true, CircuitBreakerUntil: time.Now().Add(24 * time.Hour),
		}},
	}}
	var mu sync.RWMutex
	var calls []string
	var dms []string
	closer := func(sym string, partialSz *float64, _ []int64) (*HyperliquidCloseResult, error) {
		sz := 0.0
		if partialSz != nil {
			sz = *partialSz
		}
		calls = append(calls, fmt.Sprintf("%s:%g", sym, sz))
		return &HyperliquidCloseResult{
			Close:    &HyperliquidClose{Symbol: sym, Fill: &HyperliquidCloseFill{TotalSz: sz, AvgPx: 1}},
			Platform: "hyperliquid",
		}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, []StrategyConfig{hedgeTestConfig()}, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 10, EntryPrice: testPrimaryPx}, {Coin: "BTC", Size: 0.4, EntryPrice: testHedgePx}},
		true, nil, closer, 30*time.Second, &mu,
		func(msg string) { dms = append(dms, msg) },
	)
	if len(calls) != 1 || calls[0] != "ETH:10" {
		t.Fatalf("closer calls = %v, want only the primary — a same-side position must not be liquidated as a hedge", calls)
	}
	if len(dms) != 1 || !strings.Contains(dms[0], "hedge coin conflict") {
		t.Fatalf("owner DMs = %v, want the conflict alert", dms)
	}
}

func TestStuckCBWithNothingOnChainIsANoOp(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth-long": {ID: "eth-long", RiskState: RiskState{
			CircuitBreaker: true, CircuitBreakerUntil: time.Now().Add(24 * time.Hour),
		}},
	}}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64, _ []int64) (*HyperliquidCloseResult, error) {
		calls = append(calls, sym)
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: sym}}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, []StrategyConfig{hedgeTestConfig()}, "0xabc",
		nil, true, nil, closer, 30*time.Second, &mu, nil,
	)
	if len(calls) != 0 {
		t.Fatalf("closer calls = %v, want none", calls)
	}
	if state.Strategies["eth-long"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid) != nil {
		t.Fatal("no pending should be set when nothing is on-chain")
	}
}
