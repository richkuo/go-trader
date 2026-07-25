package main

import (
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Accessors
// ---------------------------------------------------------------------------

func TestHedgeCoinNormalization(t *testing.T) {
	cases := map[string]string{
		"BTC":            "BTC",
		"btc":            "BTC",
		"  eth  ":        "ETH",
		"BTC/USDC:USDC":  "BTC",
		"btc/usdc:usdc":  "BTC",
		"SOL/USDC":       "SOL",
		"kPEPE":          "KPEPE",
		"":               "",
		"   ":            "",
		"/USDC:USDC":     "",
		"HYPE:something": "HYPE",
	}
	for in, want := range cases {
		if got := normalizeHedgeCoin(in); got != want {
			t.Errorf("normalizeHedgeCoin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHedgeAccessorDefaults(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	if !HedgeEnabled(sc) {
		t.Fatal("HedgeEnabled should be true for an enabled block with a symbol")
	}
	if got := hedgeRatio(sc); got != HedgeDefaultRatio {
		t.Errorf("hedgeRatio default = %v, want %v", got, HedgeDefaultRatio)
	}
	if got := hedgeExchangeLeverage(sc); got != 1 {
		t.Errorf("hedgeExchangeLeverage default = %v, want 1", got)
	}
	if got := hedgeMarginMode(sc); got != "isolated" {
		t.Errorf("hedgeMarginMode default = %q, want isolated", got)
	}
	// An enabled block with an unparseable symbol must not read as enabled —
	// otherwise the sync would try to trade coin "".
	bad := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "  "}}
	if HedgeEnabled(bad) {
		t.Error("HedgeEnabled must be false when the symbol does not normalize to a coin")
	}
	if HedgeEnabled(StrategyConfig{}) {
		t.Error("HedgeEnabled must be false with no hedge block")
	}
}

func TestHedgeInverseSideMapping(t *testing.T) {
	if got := hedgeInverseSide("long"); got != "short" {
		t.Errorf("long primary should hedge short, got %q", got)
	}
	if got := hedgeInverseSide("short"); got != "long" {
		t.Errorf("short primary should hedge long, got %q", got)
	}
	if got := hedgeInverseSide("weird"); got != "" {
		t.Errorf("unknown side must map to empty (fail closed), got %q", got)
	}
	if got := hedgeOrderSideForPositionSide("short"); got != "sell" {
		t.Errorf("short hedge position opens with a sell, got %q", got)
	}
	if got := hedgeOrderSideForPositionSide("long"); got != "buy" {
		t.Errorf("long hedge position opens with a buy, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Sizing
// ---------------------------------------------------------------------------

func TestHedgeQtyForNotional(t *testing.T) {
	// 2 ETH @ $3000 = $6000 notional; ratio 1.0; BTC @ $60000 → 0.1 BTC.
	qty, ok := hedgeQtyForNotional(2, 3000, 1.0, 60000)
	if !ok || math.Abs(qty-0.1) > 1e-12 {
		t.Fatalf("hedgeQtyForNotional = (%v, %v), want (0.1, true)", qty, ok)
	}
	// ratio 0.5 halves it.
	qty, ok = hedgeQtyForNotional(2, 3000, 0.5, 60000)
	if !ok || math.Abs(qty-0.05) > 1e-12 {
		t.Fatalf("ratio 0.5 = (%v, %v), want (0.05, true)", qty, ok)
	}
	// Fail closed on every unusable input — never guess a size.
	for _, c := range []struct {
		name                             string
		delta, primaryPx, ratio, hedgePx float64
	}{
		{"zero delta", 0, 3000, 1, 60000},
		{"negative delta", -1, 3000, 1, 60000},
		{"no primary mark", 2, 0, 1, 60000},
		{"no hedge mark", 2, 3000, 1, 0},
		{"negative hedge mark", 2, 3000, 1, -5},
		{"zero ratio", 2, 3000, 0, 60000},
	} {
		if qty, ok := hedgeQtyForNotional(c.delta, c.primaryPx, c.ratio, c.hedgePx); ok {
			t.Errorf("%s: expected fail-closed, got qty=%v", c.name, qty)
		}
	}
}

// ---------------------------------------------------------------------------
// Decision core
// ---------------------------------------------------------------------------

func baseSnap() hedgeSnapshot {
	return hedgeSnapshot{
		PrimarySymbol: "ETH",
		PrimaryQty:    2,
		PrimarySide:   "long",
		PrimaryPx:     3000,
		HedgeSymbol:   "BTC",
		HedgePx:       60000,
	}
}

func TestHedgeDecisionOpensInverseHedgeSizedByNotional(t *testing.T) {
	act := hedgeTargetDecision(true, 1.0, baseSnap())
	if act.Kind != hedgeActionOpen {
		t.Fatalf("kind = %v, want open (%s)", act.Kind, act.Reason)
	}
	if act.PositionSide != "short" {
		t.Errorf("a long primary must hedge SHORT, got %q", act.PositionSide)
	}
	if math.Abs(act.Qty-0.1) > 1e-12 {
		t.Errorf("qty = %v, want 0.1", act.Qty)
	}
	if act.NewBasis != 2 {
		t.Errorf("NewBasis = %v, want the full primary qty 2", act.NewBasis)
	}
}

func TestHedgeDecisionShortPrimaryOpensLongHedge(t *testing.T) {
	snap := baseSnap()
	snap.PrimarySide = "short"
	act := hedgeTargetDecision(true, 1.0, snap)
	if act.Kind != hedgeActionOpen || act.PositionSide != "long" {
		t.Fatalf("short primary must open a LONG hedge, got kind=%v side=%q", act.Kind, act.PositionSide)
	}
}

func TestHedgeDecisionDisabledDoesNothing(t *testing.T) {
	if act := hedgeTargetDecision(false, 1.0, baseSnap()); act.Kind != hedgeActionNone {
		t.Fatalf("disabled hedge must not act, got %v", act.Kind)
	}
}

func TestHedgeDecisionConvergedIsNoOp(t *testing.T) {
	snap := baseSnap()
	snap.HedgeQty = 0.1
	snap.HedgeSide = "short"
	snap.HedgeBasis = 2
	if act := hedgeTargetDecision(true, 1.0, snap); act.Kind != hedgeActionNone {
		t.Fatalf("converged hedge must be a no-op, got %v (%s)", act.Kind, act.Reason)
	}
}

// Mark drift must NEVER re-trade the hedge — the whole point of keying the
// target on a quantity watermark rather than on notional parity.
func TestHedgeDecisionIgnoresMarkDrift(t *testing.T) {
	snap := baseSnap()
	snap.HedgeQty = 0.1
	snap.HedgeSide = "short"
	snap.HedgeBasis = 2
	snap.PrimaryPx = 4500 // ETH +50%
	snap.HedgePx = 42000  // BTC -30%
	if act := hedgeTargetDecision(true, 1.0, snap); act.Kind != hedgeActionNone {
		t.Fatalf("price drift alone must not move the hedge, got %v qty=%v (%s)", act.Kind, act.Qty, act.Reason)
	}
}

func TestHedgeDecisionAddsOnPrimaryGrowth(t *testing.T) {
	snap := baseSnap()
	snap.PrimaryQty = 3 // scaled in by 1 ETH
	snap.HedgeQty = 0.1
	snap.HedgeSide = "short"
	snap.HedgeBasis = 2
	act := hedgeTargetDecision(true, 1.0, snap)
	if act.Kind != hedgeActionAdd {
		t.Fatalf("kind = %v, want add (%s)", act.Kind, act.Reason)
	}
	// Sized on the DELTA notional (1 ETH × $3000 / $60000), not the total.
	if math.Abs(act.Qty-0.05) > 1e-12 {
		t.Errorf("add qty = %v, want 0.05 (delta-notional sizing)", act.Qty)
	}
	if act.NewBasis != 3 {
		t.Errorf("NewBasis = %v, want 3", act.NewBasis)
	}
	if act.PositionSide != "short" {
		t.Errorf("add must keep the hedge side, got %q", act.PositionSide)
	}
}

func TestHedgeDecisionReducesProportionallyOnPartialClose(t *testing.T) {
	snap := baseSnap()
	snap.PrimaryQty = 1.5 // 25% of the basis closed
	snap.HedgeQty = 0.1
	snap.HedgeSide = "short"
	snap.HedgeBasis = 2
	act := hedgeTargetDecision(true, 1.0, snap)
	if act.Kind != hedgeActionReduce {
		t.Fatalf("kind = %v, want reduce (%s)", act.Kind, act.Reason)
	}
	if math.Abs(act.Qty-0.025) > 1e-12 {
		t.Errorf("reduce qty = %v, want 0.025 (25%% of 0.1)", act.Qty)
	}
	if act.NewBasis != 1.5 {
		t.Errorf("NewBasis = %v, want 1.5", act.NewBasis)
	}
}

func TestHedgeDecisionClosesWhenPrimaryFlat(t *testing.T) {
	snap := baseSnap()
	snap.PrimaryQty = 0
	snap.PrimarySide = ""
	snap.HedgeQty = 0.1
	snap.HedgeSide = "short"
	snap.HedgeBasis = 2
	act := hedgeTargetDecision(true, 1.0, snap)
	if act.Kind != hedgeActionCloseFull {
		t.Fatalf("kind = %v, want closeFull (%s)", act.Kind, act.Reason)
	}
	if act.Qty != 0.1 {
		t.Errorf("close qty = %v, want the whole hedge leg 0.1", act.Qty)
	}
}

func TestHedgeDecisionBothFlatIsNoOp(t *testing.T) {
	snap := baseSnap()
	snap.PrimaryQty = 0
	snap.PrimarySide = ""
	if act := hedgeTargetDecision(true, 1.0, snap); act.Kind != hedgeActionNone {
		t.Fatalf("both flat must be a no-op, got %v", act.Kind)
	}
}

func TestHedgeDecisionSideMismatchUnwindsFirst(t *testing.T) {
	snap := baseSnap()
	snap.HedgeQty = 0.1
	snap.HedgeSide = "long" // same side as the long primary — wrong
	snap.HedgeBasis = 2
	act := hedgeTargetDecision(true, 1.0, snap)
	if act.Kind != hedgeActionCloseFull {
		t.Fatalf("kind = %v, want closeFull for a side mismatch (%s)", act.Kind, act.Reason)
	}
	if !strings.Contains(act.Reason, "no longer inverse") {
		t.Errorf("reason should explain the side mismatch, got %q", act.Reason)
	}
}

func TestHedgeDecisionStaleConfigUnwinds(t *testing.T) {
	snap := baseSnap()
	snap.HedgeQty = 0.1
	snap.HedgeSide = "short"
	snap.HedgeBasis = 2
	snap.HedgeStaleReason = "hedge block removed"
	// Even with enabled=false (config gone), the held leg must be unwound
	// rather than stranded.
	act := hedgeTargetDecision(false, 1.0, snap)
	if act.Kind != hedgeActionCloseFull {
		t.Fatalf("kind = %v, want closeFull for a stale hedge (%s)", act.Kind, act.Reason)
	}
	if act.Reason != "hedge block removed" {
		t.Errorf("stale reason should be propagated, got %q", act.Reason)
	}
}

func TestHedgeDecisionStaleConfigWithNoLegIsNoOp(t *testing.T) {
	snap := baseSnap()
	snap.HedgeStaleReason = "hedge block removed"
	if act := hedgeTargetDecision(false, 1.0, snap); act.Kind != hedgeActionNone {
		t.Fatalf("nothing held → nothing to unwind, got %v", act.Kind)
	}
}

// Fail closed: an unpriceable hedge must NOT open a guessed size.
func TestHedgeDecisionUnpricedOpenIsBlockedNotGuessed(t *testing.T) {
	snap := baseSnap()
	snap.HedgePx = 0
	act := hedgeTargetDecision(true, 1.0, snap)
	if act.Kind != hedgeActionNone || !act.Blocked {
		t.Fatalf("expected a BLOCKED no-op, got kind=%v blocked=%v qty=%v", act.Kind, act.Blocked, act.Qty)
	}
	snap = baseSnap()
	snap.PrimaryPx = 0
	act = hedgeTargetDecision(true, 1.0, snap)
	if act.Kind != hedgeActionNone || !act.Blocked {
		t.Fatalf("missing primary mark must block too, got kind=%v blocked=%v", act.Kind, act.Blocked)
	}
}

func TestHedgeDecisionCorruptPrimarySideIsBlocked(t *testing.T) {
	snap := baseSnap()
	snap.PrimarySide = "sideways"
	act := hedgeTargetDecision(true, 1.0, snap)
	if act.Kind != hedgeActionNone || !act.Blocked {
		t.Fatalf("unrecognized primary side must fail closed, got kind=%v blocked=%v", act.Kind, act.Blocked)
	}
}

// A sub-minimum reduce defers WITHOUT advancing the basis, so the shortfall
// accumulates into a fillable order instead of spamming rejects forever.
func TestHedgeDecisionDustReduceDefersAndKeepsBasis(t *testing.T) {
	snap := baseSnap()
	snap.PrimaryQty = 1.999 // 0.05% closed
	snap.HedgeQty = 0.1
	snap.HedgeSide = "short"
	snap.HedgeBasis = 2
	act := hedgeTargetDecision(true, 1.0, snap)
	if act.Kind != hedgeActionNone || !act.Blocked {
		t.Fatalf("a $%.2f reduce should defer, got kind=%v qty=%v", hedgeMinOrderNotionalUSD, act.Kind, act.Qty)
	}
	if act.NewBasis != 0 {
		t.Errorf("a deferred reduce must not carry a new basis, got %v", act.NewBasis)
	}
	if !strings.Contains(act.Reason, "basis not advanced") {
		t.Errorf("reason should state the basis is preserved, got %q", act.Reason)
	}
}

// A full close is never deferred for dust — the residual must always clear.
func TestHedgeDecisionFullCloseIsNeverDustDeferred(t *testing.T) {
	snap := baseSnap()
	snap.PrimaryQty = 0
	snap.PrimarySide = ""
	snap.HedgeQty = 0.00001 // ~$0.60 of BTC
	snap.HedgeSide = "short"
	snap.HedgeBasis = 2
	if act := hedgeTargetDecision(true, 1.0, snap); act.Kind != hedgeActionCloseFull {
		t.Fatalf("a dust-sized full close must still execute, got %v (%s)", act.Kind, act.Reason)
	}
}

// A legacy/repaired hedge row with no watermark must adopt the current primary
// qty rather than treating the whole position as an unhedged delta (which would
// double the hedge).
func TestHedgeDecisionMissingBasisAdoptsPrimaryQty(t *testing.T) {
	snap := baseSnap()
	snap.HedgeQty = 0.1
	snap.HedgeSide = "short"
	snap.HedgeBasis = 0
	if act := hedgeTargetDecision(true, 1.0, snap); act.Kind != hedgeActionNone {
		t.Fatalf("a missing basis must adopt the primary qty, not re-hedge; got %v qty=%v", act.Kind, act.Qty)
	}
}

func TestHedgeDecisionRatioScalesTheOpen(t *testing.T) {
	act := hedgeTargetDecision(true, 0.5, baseSnap())
	if math.Abs(act.Qty-0.05) > 1e-12 {
		t.Errorf("ratio 0.5 qty = %v, want 0.05", act.Qty)
	}
	// A non-positive ratio falls back to the documented default rather than
	// silently refusing to hedge.
	act = hedgeTargetDecision(true, 0, baseSnap())
	if math.Abs(act.Qty-0.1) > 1e-12 {
		t.Errorf("ratio 0 should default to %v, got qty %v", HedgeDefaultRatio, act.Qty)
	}
}

func TestHedgeConverged(t *testing.T) {
	if !hedgeConverged(2, 2) {
		t.Error("identical quantities must be converged")
	}
	if !hedgeConverged(2+1e-12, 2) {
		t.Error("float noise must be inside the tolerance band")
	}
	if hedgeConverged(1.5, 2) {
		t.Error("a 25% reduction must NOT be swallowed by the tolerance band")
	}
	if !hedgeConverged(0, 0) {
		t.Error("flat vs flat is converged")
	}
}

// ---------------------------------------------------------------------------
// Skip-reason mirror
// ---------------------------------------------------------------------------

func TestHedgeOrderSkipReasonCatchesStateMovingUnderTheDecision(t *testing.T) {
	open := hedgeAction{Kind: hedgeActionOpen, Qty: 0.1, PositionSide: "short"}

	fresh := baseSnap()
	if reason := hedgeOrderSkipReason(open, fresh); reason != "" {
		t.Errorf("unchanged state should not skip, got %q", reason)
	}

	// A hedge leg appeared between the decision and the spawn.
	fresh = baseSnap()
	fresh.HedgeQty = 0.1
	fresh.HedgeSide = "short"
	if reason := hedgeOrderSkipReason(open, fresh); reason == "" {
		t.Error("an existing hedge leg must skip the open (double-hedge guard)")
	}

	// The primary closed between the decision and the spawn.
	fresh = baseSnap()
	fresh.PrimaryQty = 0
	if reason := hedgeOrderSkipReason(open, fresh); reason == "" {
		t.Error("a flat primary must skip the open")
	}

	// The primary flipped side.
	fresh = baseSnap()
	fresh.PrimarySide = "short"
	if reason := hedgeOrderSkipReason(open, fresh); reason == "" {
		t.Error("a flipped primary must skip a stale-side open")
	}

	// Add against a vanished leg.
	add := hedgeAction{Kind: hedgeActionAdd, Qty: 0.05, PositionSide: "short"}
	fresh = baseSnap()
	if reason := hedgeOrderSkipReason(add, fresh); reason == "" {
		t.Error("an add must skip when the hedge leg is gone")
	}

	// Reduce/close against a flat leg.
	red := hedgeAction{Kind: hedgeActionReduce, Qty: 0.02}
	fresh = baseSnap()
	if reason := hedgeOrderSkipReason(red, fresh); reason == "" {
		t.Error("a reduce must skip when the hedge leg is already flat")
	}
}

// ---------------------------------------------------------------------------
// Failure tracker / entry hold
// ---------------------------------------------------------------------------

func TestHedgeEntryHoldEngagesAfterConsecutiveFailuresAndClears(t *testing.T) {
	tracker := &hedgeFailureTracker{counts: map[string]int{}, warned: map[string]bool{}}
	prev := globalHedgeFailures
	globalHedgeFailures = tracker
	defer func() { globalHedgeFailures = prev }()

	sc := StrategyConfig{ID: "s1", Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	if hedgeEntryHoldActive(sc) {
		t.Fatal("hold must be off with no failures")
	}
	holds := 0
	for i := 1; i <= hedgeOpenFailureHoldThreshold; i++ {
		if _, first := tracker.recordFailure("s1"); first {
			holds++
		}
	}
	if holds != 1 {
		t.Errorf("the hold DM must fire exactly once per episode, fired %d times", holds)
	}
	if !hedgeEntryHoldActive(sc) {
		t.Fatalf("hold must engage at %d consecutive failures", hedgeOpenFailureHoldThreshold)
	}
	// Further failures must not re-fire the one-shot DM.
	if _, first := tracker.recordFailure("s1"); first {
		t.Error("the hold DM must not re-fire while still held")
	}
	tracker.clear("s1")
	if hedgeEntryHoldActive(sc) {
		t.Error("a successful hedge open must clear the hold")
	}
	// A strategy without a hedge is never held.
	if hedgeEntryHoldActive(StrategyConfig{ID: "s2"}) {
		t.Error("an unhedged strategy must never be entry-held")
	}
}

// ---------------------------------------------------------------------------
// Operator surfaces
// ---------------------------------------------------------------------------

func TestHedgeSummaryTag(t *testing.T) {
	if got := hedgeSummaryTag(StrategyConfig{}); got != "" {
		t.Errorf("no hedge block should render nothing, got %q", got)
	}
	enabled := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC", Ratio: 1.5, MarginMode: "cross", Leverage: 3}}
	if got := hedgeSummaryTag(enabled); got != "hedge=BTC×1.50(inverse,cross,3x)" {
		t.Errorf("summary tag = %q", got)
	}
	// A parked block must NOT look identical to no block at all.
	parked := StrategyConfig{Hedge: &HedgeConfig{Symbol: "BTC"}}
	if got := hedgeSummaryTag(parked); got != "hedge=BTC(disabled)" {
		t.Errorf("disabled tag = %q", got)
	}
}

func TestHedgeStatusNoteLabelsTheLegAsCoupled(t *testing.T) {
	sc := StrategyConfig{
		ID: "s1", Platform: "hyperliquid", Type: "perps",
		Args:  []string{"--mode=paper", "ETH"},
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"},
	}
	s := &StrategyState{ID: "s1", Positions: map[string]*Position{}}
	if note := hedgeStatusNote(sc, s); !strings.Contains(note, "flat") {
		t.Errorf("a flat hedge should still be reported, got %q", note)
	}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, HedgeFor: "ETH", HedgePrimaryQtyBasis: 2}
	note := hedgeStatusNote(sc, s)
	if !strings.Contains(note, "auto-managed") || !strings.Contains(note, "coupled to ETH") {
		t.Errorf("a held hedge must be labelled auto-managed and coupled, got %q", note)
	}
	// Disabling the config while a leg is held must warn about the pending unwind.
	sc.Hedge.Enabled = false
	if note := hedgeStatusNote(sc, s); !strings.Contains(note, "will be unwound") {
		t.Errorf("a stale hedge must announce the unwind, got %q", note)
	}
}

func TestHedgeStatusForSerialization(t *testing.T) {
	sc := StrategyConfig{
		ID: "s1", Platform: "hyperliquid", Type: "perps",
		Args:  []string{"--mode=paper", "ETH"},
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2},
	}
	s := &StrategyState{ID: "s1", Positions: map[string]*Position{
		"BTC": {Symbol: "BTC", Quantity: 0.2, Side: "short", AvgCost: 60000, HedgeFor: "ETH", HedgePrimaryQtyBasis: 2},
	}}
	st := hedgeStatusFor(sc, s)
	if st == nil {
		t.Fatal("hedgeStatusFor returned nil for a hedged strategy")
	}
	if st.Symbol != "BTC" || st.PrimarySymbol != "ETH" || st.Ratio != 2 || st.HeldQty != 0.2 || st.HeldSide != "short" {
		t.Errorf("unexpected status: %+v", st)
	}
	if st.StaleConfig {
		t.Error("a matching config must not be flagged stale")
	}
	if hedgeStatusFor(StrategyConfig{ID: "x"}, &StrategyState{ID: "x", Positions: map[string]*Position{}}) != nil {
		t.Error("an unhedged strategy must serialize to nil")
	}
}

func TestHedgePositionOfIgnoresOrdinaryPositions(t *testing.T) {
	s := &StrategyState{ID: "s1", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long"},
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", HedgeFor: "ETH"},
	}}
	pos, coin := hedgePositionOf(s)
	if pos == nil || coin != "BTC" {
		t.Fatalf("hedgePositionOf = (%v, %q), want the BTC leg", pos, coin)
	}
	delete(s.Positions, "BTC")
	if pos, _ := hedgePositionOf(s); pos != nil {
		t.Error("an ordinary position must never be identified as a hedge leg")
	}
}
