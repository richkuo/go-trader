package main

import (
	"math"
	"testing"
)

func hedgeSC(ratio float64) StrategyConfig {
	return StrategyConfig{
		ID:       "hl-eth",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"check_hyperliquid.py", "ETH", "live"},
		Hedge:    &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: ratio},
	}
}

func TestHedgeInverseSideMapping(t *testing.T) {
	if got := hedgeInverseOrderSide("long"); got != "sell" {
		t.Errorf("primary long → hedge order side: want sell, got %s", got)
	}
	if got := hedgeInverseOrderSide("short"); got != "buy" {
		t.Errorf("primary short → hedge order side: want buy, got %s", got)
	}
	if got := hedgeInversePositionSide("long"); got != "short" {
		t.Errorf("primary long → hedge position side: want short, got %s", got)
	}
	if got := hedgeInversePositionSide("short"); got != "long" {
		t.Errorf("primary short → hedge position side: want long, got %s", got)
	}
	if got := hedgeReduceOrderSide("short"); got != "buy" {
		t.Errorf("reduce short hedge: want buy, got %s", got)
	}
	if got := hedgeReduceOrderSide("long"); got != "sell" {
		t.Errorf("reduce long hedge: want sell, got %s", got)
	}
}

func TestHedgeDecisionDisabled(t *testing.T) {
	sc := hedgeSC(1.0)
	sc.Hedge.Enabled = false
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long"}, 100, 100)
	if got.Kind != hedgeActionNone {
		t.Errorf("disabled hedge should be none, got %v", got.Kind)
	}
}

func TestHedgeDecisionFreshOpenSizingAndSide(t *testing.T) {
	sc := hedgeSC(1.0)
	// Primary: 10 ETH long @ $2000 = $20k notional. Hedge BTC @ $40000, ratio 1.
	// Expected hedge qty = 20000 / 40000 = 0.5 BTC, order side sell (short hedge).
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 10, PrimarySide: "long"}, 2000, 40000)
	if got.Kind != hedgeActionOpen {
		t.Fatalf("want open, got %v", got.Kind)
	}
	if math.Abs(got.Qty-0.5) > 1e-9 {
		t.Errorf("hedge open qty: want 0.5, got %g", got.Qty)
	}
	if got.Side != "sell" {
		t.Errorf("hedge open side: want sell, got %s", got.Side)
	}
	if math.Abs(got.TargetBasis-10) > 1e-9 {
		t.Errorf("target basis: want 10, got %g", got.TargetBasis)
	}
}

func TestHedgeDecisionRatioScalesNotional(t *testing.T) {
	sc := hedgeSC(0.5) // half-notional hedge
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 10, PrimarySide: "long"}, 2000, 40000)
	// 20000 * 0.5 / 40000 = 0.25
	if math.Abs(got.Qty-0.25) > 1e-9 {
		t.Errorf("ratio 0.5 hedge qty: want 0.25, got %g", got.Qty)
	}
}

func TestHedgeDecisionShortPrimaryOpensLongHedge(t *testing.T) {
	sc := hedgeSC(1.0)
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 5, PrimarySide: "short"}, 2000, 40000)
	if got.Kind != hedgeActionOpen || got.Side != "buy" {
		t.Errorf("short primary → long hedge (buy), got kind=%v side=%s", got.Kind, got.Side)
	}
}

func TestHedgeDecisionPrimaryFlatClosesHedge(t *testing.T) {
	sc := hedgeSC(1.0)
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 0, HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}, 2000, 40000)
	if got.Kind != hedgeActionCloseFull {
		t.Fatalf("primary flat should close hedge, got %v", got.Kind)
	}
	if got.Side != "buy" { // close a short hedge → buy
		t.Errorf("close short hedge side: want buy, got %s", got.Side)
	}
	if math.Abs(got.Qty-0.5) > 1e-9 {
		t.Errorf("close qty: want 0.5, got %g", got.Qty)
	}
}

func TestHedgeDecisionBothFlatNone(t *testing.T) {
	sc := hedgeSC(1.0)
	got := hedgeTargetDecision(sc, hedgeSnapshot{}, 2000, 40000)
	if got.Kind != hedgeActionNone {
		t.Errorf("both flat → none, got %v", got.Kind)
	}
}

func TestHedgeDecisionAddOnPrimaryIncrease(t *testing.T) {
	sc := hedgeSC(1.0)
	// Primary grew 10 → 15 (basis 10). delta 5 ETH @ $2000 = $10k / $40000 = 0.25.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 15, PrimarySide: "long", HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}, 2000, 40000)
	if got.Kind != hedgeActionAdd {
		t.Fatalf("want add, got %v", got.Kind)
	}
	if math.Abs(got.Qty-0.25) > 1e-9 {
		t.Errorf("add qty: want 0.25, got %g", got.Qty)
	}
	if got.Side != "sell" {
		t.Errorf("add side (still short hedge): want sell, got %s", got.Side)
	}
	if math.Abs(got.TargetBasis-15) > 1e-9 {
		t.Errorf("target basis: want 15, got %g", got.TargetBasis)
	}
}

func TestHedgeDecisionProportionalReduce(t *testing.T) {
	sc := hedgeSC(1.0)
	// Primary reduced 10 → 6 (basis 10). frac = (10-6)/10 = 0.4. reduce = 0.5*0.4 = 0.2.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 6, PrimarySide: "long", HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}, 2000, 40000)
	if got.Kind != hedgeActionReduce {
		t.Fatalf("want reduce, got %v", got.Kind)
	}
	if math.Abs(got.Qty-0.2) > 1e-9 {
		t.Errorf("reduce qty: want 0.2, got %g", got.Qty)
	}
	if got.Side != "buy" { // reduce short hedge → buy
		t.Errorf("reduce side: want buy, got %s", got.Side)
	}
	if math.Abs(got.TargetBasis-6) > 1e-9 {
		t.Errorf("reduce target basis: want 6, got %g", got.TargetBasis)
	}
}

func TestHedgeDecisionReduceToFlatFullCloses(t *testing.T) {
	sc := hedgeSC(1.0)
	// Primary reduced 10 → 0.0000001 basis 10 → frac ~1 → full close.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1e-12, PrimarySide: "long", HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}, 2000, 40000)
	if got.Kind != hedgeActionCloseFull {
		t.Errorf("reduce-to-flat should full-close, got %v", got.Kind)
	}
}

func TestHedgeDecisionWithinToleranceNone(t *testing.T) {
	sc := hedgeSC(1.0)
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 10, PrimarySide: "long", HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}, 2000, 40000)
	if got.Kind != hedgeActionNone {
		t.Errorf("primary == basis → none, got %v", got.Kind)
	}
}

func TestHedgeDecisionDustAddDeferred(t *testing.T) {
	sc := hedgeSC(1.0)
	// tiny primary increase: 10 → 10.0001, delta 0.0001 ETH @ $2000 = $0.2 → below $10 min.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 10.0001, PrimarySide: "long", HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}, 2000, 40000)
	if got.Kind != hedgeActionNone {
		t.Errorf("dust add should defer to none, got %v (reason=%s)", got.Kind, got.Reason)
	}
}

func TestHedgeDecisionDustReduceDeferred(t *testing.T) {
	sc := hedgeSC(1.0)
	// tiny reduce: 10 → 9.9999, reduce notional ~ $0.2 < $10.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 9.9999, PrimarySide: "long", HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}, 2000, 40000)
	if got.Kind != hedgeActionNone {
		t.Errorf("dust reduce should defer to none, got %v (reason=%s)", got.Kind, got.Reason)
	}
}

func TestHedgeDecisionUnusablePriceFailClosed(t *testing.T) {
	sc := hedgeSC(1.0)
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 10, PrimarySide: "long"}, 2000, 0)
	if got.Kind != hedgeActionNone || !got.FailClosed {
		t.Errorf("zero hedge px → fail-closed none, got kind=%v failClosed=%v", got.Kind, got.FailClosed)
	}
	got = hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 10, PrimarySide: "long"}, 0, 40000)
	if got.Kind != hedgeActionNone || !got.FailClosed {
		t.Errorf("zero primary px → fail-closed none, got kind=%v failClosed=%v", got.Kind, got.FailClosed)
	}
}

func TestHedgeDecisionWrongSideFlattens(t *testing.T) {
	sc := hedgeSC(1.0)
	// Primary long → want short hedge, but a long hedge is held (should never
	// happen with direction=both rejected) → defense-in-depth full close.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 10, PrimarySide: "long", HedgeQty: 0.5, HedgeSide: "long", HedgeBasis: 10}, 2000, 40000)
	if got.Kind != hedgeActionCloseFull {
		t.Errorf("wrong-side hedge should full-close, got %v", got.Kind)
	}
	if got.Side != "sell" { // close a long hedge → sell
		t.Errorf("close long hedge side: want sell, got %s", got.Side)
	}
}

func TestHedgeBasisAfterFill(t *testing.T) {
	// Full fill lands on target.
	if got := hedgeBasisAfterFill(0, 10, 0.5, 0.5); math.Abs(got-10) > 1e-9 {
		t.Errorf("full open fill basis: want 10, got %g", got)
	}
	// Half fill lands halfway.
	if got := hedgeBasisAfterFill(0, 10, 0.5, 0.25); math.Abs(got-5) > 1e-9 {
		t.Errorf("half open fill basis: want 5, got %g", got)
	}
	// Add: old basis 10, target 15, half fill → 12.5.
	if got := hedgeBasisAfterFill(10, 15, 0.25, 0.125); math.Abs(got-12.5) > 1e-9 {
		t.Errorf("half add fill basis: want 12.5, got %g", got)
	}
	// Reduce: old basis 10, target 6, half fill → 8.
	if got := hedgeBasisAfterFill(10, 6, 0.2, 0.1); math.Abs(got-8) > 1e-9 {
		t.Errorf("half reduce fill basis: want 8, got %g", got)
	}
	// Overfill clamps to target.
	if got := hedgeBasisAfterFill(0, 10, 0.5, 0.9); math.Abs(got-10) > 1e-9 {
		t.Errorf("overfill clamps to target 10, got %g", got)
	}
	// requestedQty 0 → target.
	if got := hedgeBasisAfterFill(0, 10, 0, 0); got != 10 {
		t.Errorf("zero requested → target, got %g", got)
	}
}
