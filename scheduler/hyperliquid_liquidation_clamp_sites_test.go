package main

import (
	"testing"
)

// #1450 — the clamp at the two self-healing owner sites: the trailing walker
// and the protection plan.

// --- walker clamp ----------------------------------------------------------

func liqWalkerStrategy() StrategyConfig {
	trail := 3.0
	minMove := 0.5
	return StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:                   []string{"x.py", "ETH", "1h", "--mode=live"},
		TrailingStopPct:        &trail,
		TrailingStopMinMovePct: &minMove,
	}
}

// A trailing candidate that would rest past liquidation is placed just inside
// it instead — and the replace happens even though the shift is below the
// min-move debounce, because the debounce exists to avoid churn, not to keep an
// unreachable stop resting.
func TestTrailingWalkerClampsCandidateInsideLiquidation(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	// mark 2400, trail 3% -> candidate 2328, which sits below liquidation 2340.5.
	var gotTrigger float64
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		gotTrigger = triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7001, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2320, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("walker must confirm the clamped replacement")
	}
	if calls != 1 {
		t.Fatalf("placement calls = %d, want 1", calls)
	}
	want := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(gotTrigger, want) {
		t.Errorf("placed trigger = %g, want %g (just inside liquidation)", gotTrigger, want)
	}
}

// The heal: the walker sees no reason to move, but the RESTING trigger is past
// liquidation (armed on the open cycle, before any liquidation price existed).
func TestTrailingWalkerHealsRestingStopPastLiquidation(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	var gotTrigger float64
	var gotCancelOID int64
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		gotTrigger, gotCancelOID = triggerPx, cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7002, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	// High-water 2400 -> candidate 2328; the resting trigger 2330 is already
	// MORE favorable, so the walker alone would not replace. But 2330 is past
	// liquidation 2340.5 and must be healed.
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("walker must confirm the heal")
	}
	if calls != 1 {
		t.Fatalf("placement calls = %d, want 1 (heal an unreachable resting stop)", calls)
	}
	if gotCancelOID != 4242 {
		t.Errorf("cancel OID = %d, want 4242", gotCancelOID)
	}
	want := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(gotTrigger, want) {
		t.Errorf("healed trigger = %g, want %g", gotTrigger, want)
	}
}

// liquidationPx == 0 (unknown / paper) must be byte-identical to the pre-#1450
// walker: no extra placement, no changed trigger.
func TestTrailingWalkerUnknownLiquidationIsUnchanged(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	sc := liqWalkerStrategy()
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7003, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	// Same inputs as the heal test, liquidationPx unknown: no replacement.
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("walker must report ok when it declines to replace")
	}
	if calls != 0 {
		t.Fatalf("placement calls = %d, want 0 — an unknown liquidation price must change nothing", calls)
	}
}

// The clamp is one-way. A liquidation price far below the resting stop must
// never widen it back out.
func TestTrailingWalkerClampNeverWidens(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	sc := liqWalkerStrategy()
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7004, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	// Resting 2330 is comfortably ABOVE liquidation 1500 — reachable, nothing to do.
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 1500}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("walker must report ok")
	}
	if calls != 0 {
		t.Fatalf("placement calls = %d, want 0 — a reachable stop must never be re-placed (and never widened)", calls)
	}
}

// --- protection plan clamp -------------------------------------------------

func TestProtectionPlanClampsSLMultPastLiquidation(t *testing.T) {
	mult := 2.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
	}
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2325,
	}

	// Baseline: liquidationPx == 0 leaves the plan byte-identical.
	basePlan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
	if !ok {
		t.Fatal("expected a plan")
	}
	if !approxEqLiq(basePlan.StopLossATRMult, 2.5) {
		t.Fatalf("baseline mult = %g, want the configured 2.5", basePlan.StopLossATRMult)
	}
	if basePlan.ForceSLReplace {
		t.Error("an unknown liquidation price must not force a replace")
	}

	// Clamped: the derived trigger 2400 - 2.5*30 = 2325 sits past liquidation.
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 2340.5)
	if !ok {
		t.Fatal("expected a plan")
	}
	if plan.StopLossATRMult >= 2.5 {
		t.Errorf("clamped mult = %g, want strictly below the configured 2.5", plan.StopLossATRMult)
	}
	wantTrigger := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if got := plan.AvgCost - plan.StopLossATRMult*plan.EntryATR; !approxEqLiq(got, wantTrigger) {
		t.Errorf("plan derives trigger %g, want %g", got, wantTrigger)
	}
	if !plan.ForceSLReplace {
		t.Error("a clamped SL must force the cancel+replace, or the unreachable order keeps resting")
	}
}

// The open-cycle heal: the resolved multiple is fine, but the RESTING trigger
// (armed inline at open, before any liquidation price existed) is past
// liquidation. ForceSLReplace is what re-places it.
func TestProtectionPlanForcesReplaceForRestingStopPastLiquidation(t *testing.T) {
	mult := 0.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
	}
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2325, // resting, past liquidation
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 2340.5)
	if !ok {
		t.Fatal("expected a plan")
	}
	// 2400 - 0.5*30 = 2385, comfortably inside liquidation: nothing to clamp.
	if !approxEqLiq(plan.StopLossATRMult, 0.5) {
		t.Errorf("mult = %g, want the configured 0.5 (already reachable)", plan.StopLossATRMult)
	}
	if !plan.ForceSLReplace {
		t.Fatal("a resting trigger past liquidation must force a replace even when the resolved multiple is fine")
	}
}

func TestProtectionPlanShortSideClamp(t *testing.T) {
	mult := 2.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
	}
	pos := &Position{
		Symbol: "ETH", Side: "short", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2475,
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 2460)
	if !ok {
		t.Fatal("expected a plan")
	}
	wantTrigger := 2460 * (1 - hlLiquidationStopBufferPct/100.0)
	if got := plan.AvgCost + plan.StopLossATRMult*plan.EntryATR; !approxEqLiq(got, wantTrigger) {
		t.Errorf("short plan derives trigger %g, want %g", got, wantTrigger)
	}
	if !plan.ForceSLReplace {
		t.Error("a clamped short SL must force the cancel+replace")
	}
}
