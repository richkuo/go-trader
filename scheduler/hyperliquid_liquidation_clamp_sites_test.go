package main

import (
	"strings"
	"sync"
	"testing"
	"time"
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

// #1450 review (2a): a LONG whose liquidation price sits at or above the frozen
// anchor — reachable in cross margin, where account-wide losses push
// liquidationPx up. The clamped price lands on the FAR side of the anchor, so
// no positive multiple can reproduce it. The rewrite must REFUSE rather than
// mirror the distance back across the anchor: a mirrored trigger is itself past
// liquidation, and forcing a replace at it would cancel and re-place the same
// unfillable order every cycle forever.
func TestProtectionPlanRefusesFarSideLongRewrite(t *testing.T) {
	mult := 2.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
		MarginMode:      "cross",
	}
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2325,
	}
	const liqPx = 2400.0 // at the anchor: 2400 * 1.005 = 2412 > anchor

	newMult, clamped := hlClampProtectionSLMult("long", 2400, 30, 2.5, liqPx)
	if clamped {
		t.Fatalf("far-side long clamp must be refused, got mult %g", newMult)
	}
	if !approxEqLiq(newMult, 2.5) {
		t.Errorf("a refused rewrite must return the configured multiple, got %g", newMult)
	}

	plan, ok := buildHyperliquidProtectionPlan(sc, pos, liqPx)
	if !ok {
		t.Fatal("expected a plan")
	}
	if !approxEqLiq(plan.StopLossATRMult, 2.5) {
		t.Errorf("plan mult = %g, want the configured 2.5 — no mirrored rewrite", plan.StopLossATRMult)
	}
	if plan.ForceSLReplace {
		t.Fatal("an unclampable geometry must NOT force a replace — that is the unbounded per-cycle cancel+replace loop")
	}
}

// #1450 review (2b): the mirrored case on the short side.
func TestProtectionPlanRefusesFarSideShortRewrite(t *testing.T) {
	mult := 2.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
		MarginMode:      "cross",
	}
	pos := &Position{
		Symbol: "ETH", Side: "short", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2475,
	}
	const liqPx = 2400.0 // at the anchor: 2400 * 0.995 = 2388 < anchor

	if newMult, clamped := hlClampProtectionSLMult("short", 2400, 30, 2.5, liqPx); clamped {
		t.Fatalf("far-side short clamp must be refused, got mult %g", newMult)
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, liqPx)
	if !ok {
		t.Fatal("expected a plan")
	}
	if plan.ForceSLReplace {
		t.Fatal("an unclampable short geometry must NOT force a replace")
	}
}

// #1450 review (2c): a clamp that DOES reproduce correctly still returns true,
// and once the tightened trigger is resting the next cycle must not force
// another cancel+replace at the same price.
func TestProtectionPlanClampConvergesAfterOneReplace(t *testing.T) {
	mult := 2.5
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossATRMult: &mult,
	}
	const liqPx = 2340.5
	wantTrigger := liqPx * (1 + hlLiquidationStopBufferPct/100.0)

	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2325,
	}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, liqPx)
	if !ok {
		t.Fatal("expected a plan")
	}
	if !plan.ForceSLReplace {
		t.Fatal("cycle 1 must force the replace: the resting trigger is past liquidation")
	}
	got := plan.AvgCost - plan.StopLossATRMult*plan.EntryATR
	if !approxEqLiq(got, wantTrigger) {
		t.Fatalf("cycle 1 derives %g, want %g", got, wantTrigger)
	}

	// Cycle 2: the replacement is resting at the clamped trigger.
	pos.StopLossTriggerPx = wantTrigger
	plan2, ok := buildHyperliquidProtectionPlan(sc, pos, liqPx)
	if !ok {
		t.Fatal("expected a plan")
	}
	if plan2.ForceSLReplace {
		t.Fatal("cycle 2 must NOT force another replace — the resting trigger already equals the clamped one")
	}
	if got2 := plan2.AvgCost - plan2.StopLossATRMult*plan2.EntryATR; !approxEqLiq(got2, wantTrigger) {
		t.Errorf("cycle 2 derives %g, want the same clamped %g (no state drift)", got2, wantTrigger)
	}
}

// --- review round 2: a clamp may only claim success when a stop rests --------

// lastLiqAlertAction reads the action the throttle recorded for the last alert
// on (strategy, symbol). "" means nothing was reported.
func lastLiqAlertAction(strategyID, symbol string) hlLiquidationAlertAction {
	v, ok := hlLiquidationAlerts.Load(hlLiquidationAlertKey(strategyID, symbol))
	if !ok {
		return ""
	}
	st, ok := v.(hlLiquidationAlertState)
	if !ok {
		return ""
	}
	return st.LastAction
}

// (a) The walker cancels the old trigger and the replacement is REJECTED by the
// open-order cap. The position now has no exchange-side stop, so the alert must
// say "protection lost" — reporting a clamp here tells the operator the stop was
// tightened while it was in fact deleted.
func TestTrailingWalkerClampReportsProtectionLostWhenPlacementRejected(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		// The shape check_hyperliquid.py produces when the cancel lands and
		// place_stop_loss is then rejected: no OID, no fill, cancel succeeded.
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded: true,
			StopLossError:           "Order would exceed the open order limit",
		}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	// The STATE update is still confirmed: the OID it points at is gone, and
	// zeroing it is what lets the walker re-arm from nothing next cycle.
	if !ok {
		t.Fatal("the cancelled OID must still be cleared from state")
	}
	if got := lastLiqAlertAction("hl-eth", "ETH"); got != hlLiquidationActionProtectionLost {
		t.Errorf("alert action = %q, want %q", got, hlLiquidationActionProtectionLost)
	}
}

// (b) Both the cancel and the placement land — an ordinary clamp, with no false
// "protection lost".
func TestTrailingWalkerClampReportsClampedWhenReplacementRests(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded: true,
			StopLossOID:             7100,
			StopLossTriggerPx:       triggerPx,
		}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	if _, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t)); !ok {
		t.Fatal("walker must confirm the clamp")
	}
	if got := lastLiqAlertAction("hl-eth", "ETH"); got != hlLiquidationActionClamped {
		t.Errorf("alert action = %q, want %q", got, hlLiquidationActionClamped)
	}
}

// A clamp whose cancel never landed leaves the ORIGINAL stop resting. That is a
// deferral, not a loss — and it must not read as a clamp either.
func TestTrailingWalkerClampReportsDeferredWhenCancelFails(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{CancelStopLossError: "cancel rejected"}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	if _, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t)); ok {
		t.Fatal("a failed cancel must not confirm the update")
	}
	if got := lastLiqAlertAction("hl-eth", "ETH"); got != hlLiquidationActionReplaceDeferred {
		t.Errorf("alert action = %q, want %q", got, hlLiquidationActionReplaceDeferred)
	}
}

// #1456 review (2c): a walker clamp where the OLD stop already filled on-chain
// must NOT report "replace deferred — the original stop is still resting". The
// order just filled; there is nothing left to replace and the reconciler books
// the close.
func TestTrailingWalkerClampReportsFilledOnChainWhenOldStopFilled(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossFilledExternally: true}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	if _, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t)); ok {
		t.Fatal("an externally-filled stop must not confirm the update")
	}
	if got := lastLiqAlertAction("hl-eth", "ETH"); got != hlLiquidationActionFilledOnChain {
		t.Errorf("alert action = %q, want %q — the message must not claim the original stop is still resting", got, hlLiquidationActionFilledOnChain)
	}
}

// #1456 review (2c): the deferred alert text must never assert the ORIGINAL
// stop is still resting for a fill-at-submit shape either.
func TestTrailingWalkerClampFillAtSubmitReportsExitedNotTightened(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded:   true,
			StopLossFilledImmediately: true,
			StopLossTriggerPx:         triggerPx,
		}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	if _, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t)); !ok {
		t.Fatal("the fill at submit confirms the update")
	}
	if got := lastLiqAlertAction("hl-eth", "ETH"); got != hlLiquidationActionExited {
		t.Errorf("alert action = %q, want %q — the position is flat, not tightened", got, hlLiquidationActionExited)
	}
}

// #1456 review (2): the alert TEXT matches the action for every outcome —
// an exited position is not "tightened to $X", and no liquidation lecture is
// appended to a re-arm that had nothing to do with liquidation geometry.
func TestLiquidationAlertMessageMatchesOutcome(t *testing.T) {
	headline, detail, unprotected := hlLiquidationAlertMessage(2330, 2352, 2340.5, hlLiquidationActionExited, hlLiquidationUnprotectedRecovery(liqWalkerStrategy()))
	if headline != "**HL STOP FILLED — POSITION FLAT**" || unprotected {
		t.Errorf("exited: headline=%q unprotected=%v", headline, unprotected)
	}
	for _, banned := range []string{"tightened"} {
		if strings.Contains(detail, banned) {
			t.Errorf("exited detail %q must not claim %q", detail, banned)
		}
	}

	headline, _, _ = hlLiquidationAlertMessage(2330, 2352, 2340.5, hlLiquidationActionFilledOnChain, "")
	if headline != "**HL STOP ALREADY FILLED**" {
		t.Errorf("filled-on-chain headline = %q", headline)
	}

	// Re-arm with NO known liquidation price: no "$0.0000" and no lecture.
	_, armedDetail, _ := hlLiquidationAlertMessage(0, 2352, 0, hlLiquidationActionRearmed, "")
	if strings.Contains(armedDetail, "$0.0000") {
		t.Errorf("re-arm detail with unknown liquidation price prints $0.0000: %q", armedDetail)
	}
}

// #1456 review (2): the past-liquidation lecture is a CAUSE assertion — it may
// ride only on alerts whose triggering condition actually included measured
// past-liquidation geometry and an open position.
func TestLiquidationAlertLectureOnlyOnMeasuredOpenGeometry(t *testing.T) {
	sc := liqWalkerStrategy()
	const lecture = "A stop past liquidation can never fill"

	// (a) audit clamp that fills at submit — flat: no lecture.
	if msg := hlLiquidationAlertFullMessage(sc, "ETH", "long", 2330, 2352, 2340.5, hlLiquidationActionExited); strings.Contains(msg, lecture) {
		t.Errorf("exited alert must not carry the lecture: %q", msg)
	}
	// (b) re-arm with unknown liquidation price — geometry never measured.
	if msg := hlLiquidationAlertFullMessage(sc, "ETH", "long", 0, 2352, 0, hlLiquidationActionRearmed); strings.Contains(msg, lecture) {
		t.Errorf("re-arm without a liquidation price must not carry the lecture: %q", msg)
	}
	// (c) filled-on-chain — nothing left to advise on.
	if msg := hlLiquidationAlertFullMessage(sc, "ETH", "long", 2330, 2352, 2340.5, hlLiquidationActionFilledOnChain); strings.Contains(msg, lecture) {
		t.Errorf("filled-on-chain alert must not carry the lecture: %q", msg)
	}
	// (d) a live clamp with known geometry keeps the advice.
	if msg := hlLiquidationAlertFullMessage(sc, "ETH", "long", 2330, 2352, 2340.5, hlLiquidationActionClamped); !strings.Contains(msg, lecture) {
		t.Errorf("clamped alert with known geometry should keep the lecture: %q", msg)
	}
}

// An escalation from "replace deferred" to "protection lost" must RE-alert on
// the very next observation, inside the throttle interval — both are failures,
// and the second is the one that means the position is naked.
func TestLiquidationAlertReAlertsOnEscalationToProtectionLost(t *testing.T) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	prev := hlLiquidationAlertState{Notified: true, LastNotifiedAt: now, LastAction: hlLiquidationActionReplaceDeferred}
	send, next := hlLiquidationShouldNotify(prev, hlLiquidationActionProtectionLost, now.Add(time.Second))
	if !send {
		t.Fatal("an escalation to protection lost must not be throttled")
	}
	if next.LastAction != hlLiquidationActionProtectionLost {
		t.Errorf("LastAction = %q, want %q", next.LastAction, hlLiquidationActionProtectionLost)
	}
}

// (c) The one-shot fixed-ATR arm: the clamp lands, the PLACEMENT does not. The
// position had no stop and still has none, so nothing may claim a tighten.
func TestFixedATRArmClampActionNeverClaimsATightenWithoutARestingStop(t *testing.T) {
	cases := []struct {
		name   string
		result *HyperliquidStopLossUpdateResult
		armOK  bool
		want   hlLiquidationAlertAction
	}{
		{"open-order cap rejected the placement",
			&HyperliquidStopLossUpdateResult{StopLossError: "Order would exceed the open order limit"}, true,
			hlLiquidationActionRearmFailed},
		{"subprocess failed outright", nil, false, hlLiquidationActionRearmFailed},
		{"result carried a top-level error",
			&HyperliquidStopLossUpdateResult{Error: "boom"}, false, hlLiquidationActionRearmFailed},
		{"placement rested",
			&HyperliquidStopLossUpdateResult{StopLossOID: 8001, StopLossTriggerPx: 2352}, true,
			hlLiquidationActionClamped},
		{"placement filled at submit — position is FLAT, not tightened",
			&HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 2352}, true,
			hlLiquidationActionExited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hlLiquidationArmClampAction(tc.result, tc.armOK); got != tc.want {
				t.Errorf("action = %q, want %q", got, tc.want)
			}
		})
	}
}

// The unprotected report must name the geometry that caused it AND say no stop
// rests — an operator reading only "re-arm did not rest" cannot tell that the
// configured stop was unreachable in the first place.
func TestFixedATRArmFailedMessageNamesBothFacts(t *testing.T) {
	headline, detail, unprotected := hlLiquidationAlertMessage(2325, 2352, 2340.5, hlLiquidationActionRearmFailed, "The scheduler re-arms it on the next cycle")
	if headline != "**HL POSITION UNPROTECTED**" {
		t.Errorf("headline = %q", headline)
	}
	if !unprotected {
		t.Error("a failed arm leaves the position unprotected")
	}
	for _, want := range []string{"$2325.0000", "$2340.5000", "$2352.0000", "no exchange-side stop"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to contain %q", detail, want)
		}
	}
	if strings.Contains(detail, "tightened to") {
		t.Errorf("detail = %q must never claim a tighten", detail)
	}
}

// --- review round 2: the recorded trigger must match the resting order -------

// A protection sync that places nothing must leave the recorded trigger alone.
// The state it would otherwise overwrite is the audit's healed trigger — the
// price an order is actually resting at — and replacing it with the plan's
// derived value records a fiction that the next audit then "heals" by
// cancelling and re-placing a healthy order, once per due cycle forever.
func TestProtectionSyncEchoKeepsTheRestingTrigger(t *testing.T) {
	cases := []struct {
		name string
		side string
		// liqPx sits on the FAR side of the frozen anchor — reachable in cross
		// margin, and the geometry no positive ATR multiple can express.
		liqPx        float64
		healedRestPx float64
	}{
		{"long with liquidation above the anchor", "long", 2500, 2500 * (1 + hlLiquidationStopBufferPct/100.0)},
		{"short with liquidation below the anchor", "short", 2300, 2300 * (1 - hlLiquidationStopBufferPct/100.0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mult := 2.5
			sc := StrategyConfig{
				ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
				Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
				StopLossATRMult: &mult,
			}
			pos := &Position{
				Symbol: "ETH", Side: tc.side, Quantity: 1.0,
				AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
				StopLossOID: 4242, StopLossTriggerPx: tc.healedRestPx,
			}

			plan, ok := buildHyperliquidProtectionPlan(sc, pos, tc.liqPx)
			if !ok {
				t.Fatal("expected a plan")
			}
			// The clamp cannot express this geometry, so the plan must not force
			// a replace — that was the every-cycle cancel+replace loop.
			if plan.ForceSLReplace {
				t.Error("an unclampable far-side geometry must not force a replace")
			}

			// Python echoes the resting OID and, per the #1450 contract, reports
			// NO trigger price because it placed nothing.
			applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{StopLossOID: 4242}, nil)
			if !approxEqLiq(pos.StopLossTriggerPx, tc.healedRestPx) {
				t.Fatalf("recorded trigger = %g, want the resting %g — an echo must not rewrite it",
					pos.StopLossTriggerPx, tc.healedRestPx)
			}

			// And the next audit is a no-op: the recorded trigger is reachable.
			acts := planHyperliquidLiquidationAudit([]hlLiquidationAuditCandidate{{
				StrategyID: "hl-eth", Symbol: "ETH", Side: tc.side, Qty: 1,
				StopLossOID: 4242, StopLossTriggerPx: pos.StopLossTriggerPx,
				LiquidationPx: tc.liqPx, BookConsistent: true,
			}})
			if len(acts) != 0 {
				t.Errorf("audit actions = %+v, want none — the resting stop is already inside liquidation", acts)
			}
		})
	}
}

// (c) A sync that DID place an order still refreshes the recorded trigger, so a
// normally clampable geometry keeps converging in one replace.
func TestProtectionSyncPlacementStillRefreshesTheTrigger(t *testing.T) {
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 1.0,
		AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
		StopLossOID: 4242, StopLossTriggerPx: 2325,
	}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		StopLossOID: 9100, StopLossTriggerPx: 2352,
	}, nil)
	if pos.StopLossOID != 9100 {
		t.Errorf("OID = %d, want the newly placed 9100", pos.StopLossOID)
	}
	if !approxEqLiq(pos.StopLossTriggerPx, 2352) {
		t.Errorf("recorded trigger = %g, want the placed 2352", pos.StopLossTriggerPx)
	}
}

// #1456 review round 5 (optional): an unprotected-position alert must state the
// recovery path the code ACTUALLY performs — the audit's per-cycle re-arm for
// static scalar owners, the owner's own next due manage-only cycle for
// trailing/fixed-ATR owners.
func TestUnprotectedAlertNamesActualRecoveryCadence(t *testing.T) {
	// (a) A trailing owner on a 4h interval: the audit skips it while
	// unprotected, so "every cycle" would be a false promise.
	walker := liqWalkerStrategy()
	walker.IntervalSeconds = 14400
	_, detail, _ := hlLiquidationAlertMessage(2325, 2352, 2340.5, hlLiquidationActionProtectionLost, hlLiquidationUnprotectedRecovery(walker))
	if strings.Contains(detail, "every cycle") {
		t.Errorf("trailing owner detail must not promise per-cycle recovery: %q", detail)
	}
	if !strings.Contains(detail, "next due manage-only cycle") || !strings.Contains(detail, "4h0m0s") {
		t.Errorf("trailing owner detail must name the next due cycle and its interval: %q", detail)
	}

	// Same owner without a per-strategy override: still names the due cycle.
	walker.IntervalSeconds = 0
	_, detail, _ = hlLiquidationAlertMessage(2325, 2352, 2340.5, hlLiquidationActionProtectionLost, hlLiquidationUnprotectedRecovery(walker))
	if strings.Contains(detail, "every cycle") || !strings.Contains(detail, "next due manage-only cycle") {
		t.Errorf("interval-less owner detail = %q", detail)
	}

	// (b) A static scalar owner: the audit genuinely re-arms every cycle.
	scalar := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", StopLossPct: floatPtr(3)}
	if got := hlLiquidationUnprotectedRecovery(scalar); got != "The scheduler re-arms it on the next cycle" {
		t.Errorf("static scalar recovery = %q", got)
	}

	// (c) A successful re-arm carries no stale retry text.
	_, armedDetail, _ := hlLiquidationAlertMessage(0, 2352, 2340.5, hlLiquidationActionRearmed, hlLiquidationUnprotectedRecovery(scalar))
	for _, banned := range []string{"every cycle", "next due"} {
		if strings.Contains(armedDetail, banned) {
			t.Errorf("re-armed detail must not carry retry text %q: %q", banned, armedDetail)
		}
	}
}

// #1456 review round 7: the walker's OWN clamp branch gets the same in-cycle
// retry guarantee the audit enforces — when its cancel lands and the
// replacement does not rest, it places fresh once before returning.
func TestTrailingWalkerClampRetriesPlacementItStripped(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	type call struct {
		cancelOID int64
	}
	var calls []call
	callN := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		callN++
		calls = append(calls, call{cancelOID: cancelStopLossOID})
		if callN == 1 {
			// Cancel lands, the open-order cap rejects the replacement.
			return &HyperliquidStopLossUpdateResult{
				CancelStopLossSucceeded: true,
				StopLossError:           "Order would exceed the open order limit",
			}, "", nil
		}
		// The same-cycle retry RESTS.
		return &HyperliquidStopLossUpdateResult{StopLossOID: 8100, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	newHighWater, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if !ok || result == nil || result.StopLossOID != 8100 {
		t.Fatalf("walker must adopt the RETRY result (oid=%v ok=%v)", result, ok)
	}
	_ = newHighWater
	if len(calls) != 2 || calls[1].cancelOID != 0 {
		t.Errorf("calls = %+v, want exactly one fresh retry with cancelOID 0", calls)
	}
	if got := lastLiqAlertAction("hl-eth", "ETH"); got != hlLiquidationActionClamped {
		t.Errorf("alert action = %q, want %q — the position IS protected again", got, hlLiquidationActionClamped)
	}
}

// An ORDINARY trailing cancel+replace failure (no liquidation clamp involved)
// keeps today's no-retry behavior — the retry belongs to the #1450 clamp path
// only.
func TestTrailingWalkerNonClampReplaceTakesNoRetry(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	sc := liqWalkerStrategy()
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded: true,
			StopLossError:           "Order would exceed the open order limit",
		}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	// No liquidationPx: an ordinary trail move past the min-move debounce
	// (HWM 2500 -> trigger 2425, well below the resting 2330).
	_, _, _ = runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2500, 2330, 4242,
		trailingReplacePolicy{}, nil, newTestLogger(t))
	if calls != 1 {
		t.Errorf("placement calls = %d, want 1 — a non-clamp failure takes no in-cycle retry", calls)
	}
}

// #1456 review round 8 (optional): the manual force-close drain is a
// position-close site like any other — it must clear the per-position
// liquidation-alert throttle so a reopen's first past-liquidation observation
// is never suppressed by a stale key from the prior position.
func TestManualForceCloseClearsLiquidationAlertThrottle(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
		Script: "x.py", Args: []string{"x.py", "ETH", "1h", "--mode=live"}}
	scByID := map[string]StrategyConfig{"hl-eth": sc}
	ss := &StrategyState{ID: "hl-eth", Platform: "hyperliquid", Type: "perps",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "long", Quantity: 1.0, AvgCost: 2400},
		}}
	state := &AppState{Strategies: map[string]*StrategyState{"hl-eth": ss}}

	// Stale throttle left by the PREVIOUS position on this coin.
	hlLiquidationAlerts.Store(hlLiquidationAlertKey("hl-eth", "ETH"),
		hlLiquidationAlertState{Notified: true, LastNotifiedAt: time.Now(), LastAction: hlLiquidationActionClamped})
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	err := applyManualAction(state, nil, scByID, PendingManualAction{
		StrategyID:  "hl-eth",
		Action:      "close",
		Symbol:      "ETH",
		Side:        "sell",
		Quantity:    1.0,
		FillPrice:   2400,
		FillFee:     1,
		RealizedPnL: 10,
		IsFullClose: true,
	})
	if err != nil {
		t.Fatalf("applyManualAction: %v", err)
	}
	if _, stillOpen := ss.Positions["ETH"]; stillOpen {
		t.Fatal("position must be fully closed")
	}
	if _, exists := hlLiquidationAlerts.Load(hlLiquidationAlertKey("hl-eth", "ETH")); exists {
		t.Error("force-close must clear the liquidation-alert throttle — a reopen must re-alert on its first cycle")
	}
}

// #1456 review round 10 (Needs Fixing): a cancel that LANDS followed by a
// subprocess-level error (error payload + CancelStopLossSucceeded) must NOT be
// classified "replace deferred" — the original stop is gone, so the operator
// alert must not claim it still rests and the in-cycle clamp retry must run.
func TestTrailingWalkerErrorPayloadAfterCancelLandedRunsRetry(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		if calls == 1 {
			// cancel landed, then the subprocess raised before placing.
			return &HyperliquidStopLossUpdateResult{Error: "boom after cancel", CancelStopLossSucceeded: true}, "", nil
		}
		// In-cycle fresh retry (cancelOID=0).
		if cancelStopLossOID != 0 {
			t.Errorf("retry cancel OID = %d, want 0 (fresh placement)", cancelStopLossOID)
		}
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7009, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("walker must treat error-after-cancel as cancel-landed and confirm via the retry")
	}
	if calls != 2 {
		t.Fatalf("placement calls = %d, want 2 (failed replace + in-cycle retry)", calls)
	}
}

// Same shape WITHOUT a resting replacement and without a clamp trigger (an
// ordinary trailing move): still never reads as deferred — the outcome is
// protection lost, reported through the confirmed update.
func TestTrailingWalkerErrorPayloadAfterCancelLandedOrdinaryMove(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{Error: "boom after cancel", CancelStopLossSucceeded: true}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2300, 4242,
		trailingReplacePolicy{}, nil, newTestLogger(t))
	if !ok {
		t.Fatal("cancel-landed is a state-confirming shape even without a clamp retry")
	}
	if calls != 1 {
		t.Fatalf("placement calls = %d, want 1 (no clamp, no retry)", calls)
	}
}

// Must-survive (c): a genuine PRE-cancel failure (cancel_stop_loss_error set,
// no cancel_stop_loss_succeeded) stays "replace deferred" — the original stop
// may still rest, so no retry and no confirmation.
func TestTrailingWalkerPreCancelFailureStillDeferred(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	sc := liqWalkerStrategy()
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{CancelStopLossError: "cancel down"}, "", nil
	}
	pos := &Position{AvgCost: 2400, RiskAnchorPrice: 2400}
	_, _, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 1.0, pos, 2400, 2400, 2330, 4242,
		trailingReplacePolicy{liquidationPx: 2340.5}, nil, newTestLogger(t))
	if ok {
		t.Fatal("a failed cancel must stay deferred — the original stop may still rest")
	}
	if calls != 1 {
		t.Fatalf("placement calls = %d, want 1 (no retry on a failed cancel)", calls)
	}
}

// Audit mirror of the Needs Fixing: the same error-after-cancel payload
// reaching hlLiquidationClampReplace classifies hlReplaceProtectionLost (the
// caller then clears the dead OID and retries), while a failed CANCEL stays
// hlReplaceDeferred.
func TestLiquidationClampReplaceClassifiesErrorAfterCancelLanded(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	candidate := hlLiquidationAuditCandidate{
		Script: "x.py", StrategyID: "hl-eth", Symbol: "ETH", Side: "long",
		Qty: 1.0, StopLossOID: 4242, StopLossTriggerPx: 2330,
	}

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{Error: "boom after cancel", CancelStopLossSucceeded: true}, "", nil
	}
	if _, outcome := hlLiquidationClampReplace(candidate, 2335, newTestLogger(t)); outcome != hlReplaceProtectionLost {
		t.Errorf("outcome = %v, want hlReplaceProtectionLost (cancel landed, nothing rests)", outcome)
	}

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{Error: "pre-cancel boom"}, "", nil
	}
	if _, outcome := hlLiquidationClampReplace(candidate, 2335, newTestLogger(t)); outcome != hlReplaceDeferred {
		t.Errorf("outcome = %v, want hlReplaceDeferred (no landed cancel — the old order may rest)", outcome)
	}

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{CancelStopLossError: "cancel down"}, "", nil
	}
	if _, outcome := hlLiquidationClampReplace(candidate, 2335, newTestLogger(t)); outcome != hlReplaceDeferred {
		t.Errorf("outcome = %v, want hlReplaceDeferred for a FAILED cancel", outcome)
	}
}

// #1456 review round 10 (Optional 3): every hedged strategy whose primary the
// audit closed gets ONE reconciler call this cycle; unhedged ones get none.
func TestConvergeHedgesAfterAuditClose(t *testing.T) {
	old := postAuditHedgeSyncFn
	defer func() { postAuditHedgeSyncFn = old }()

	hedged := StrategyConfig{
		ID: "hl-btc", Type: "perps", Platform: "hyperliquid",
		Args:  []string{"x.py", "BTC", "1h"},
		Hedge: &HedgeConfig{Symbol: "ETH", Enabled: true},
	}
	unhedged := liqWalkerStrategy()

	var synced []string
	postAuditHedgeSyncFn = func(sc StrategyConfig, s *StrategyState, mu *sync.RWMutex, exec hedgeExecutor, in hedgeSyncInputs, notifier *MultiNotifier, logger *StrategyLogger) hedgeActionKind {
		synced = append(synced, sc.ID)
		if in.PrimaryPx != 100 || in.HedgePx != 50 {
			t.Errorf("%s: prices PrimaryPx=%g HedgePx=%g, want 100/50", sc.ID, in.PrimaryPx, in.HedgePx)
		}
		if in.FreshExposureQty != 0 {
			t.Errorf("%s: FreshExposureQty=%g, want 0 (the audit only reduces)", sc.ID, in.FreshExposureQty)
		}
		return hedgeActionNone
	}

	details := []hlLiquidationCloseDetail{
		{SC: hedged, Symbol: "BTC", FillPx: 100},
		{SC: unhedged, Symbol: "ETH", FillPx: 2335},
	}
	states := map[string]*StrategyState{"hl-btc": {Positions: map[string]*Position{}}}
	n := convergeHedgesAfterAuditClose(details, states, &sync.RWMutex{},
		map[string]float64{"BTC": 100, "ETH": 50}, nil,
		func(string) (*StrategyLogger, error) { return newTestLogger(t), nil })
	if n != 1 || len(synced) != 1 || synced[0] != "hl-btc" {
		t.Errorf("converged=%d synced=%v, want exactly the hedged strategy hl-btc", n, synced)
	}

	// Missing state: skipped without a reconciler call.
	n = convergeHedgesAfterAuditClose(details[:1], map[string]*StrategyState{}, &sync.RWMutex{},
		map[string]float64{"BTC": 100}, nil,
		func(string) (*StrategyLogger, error) { return newTestLogger(t), nil })
	if n != 0 {
		t.Errorf("converged=%d, want 0 when strategy state is missing", n)
	}
}
