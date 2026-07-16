package main

import (
	"sync"
	"testing"
	"time"
)

func hedgeTestStrategy(hedge *HedgeConfig) StrategyConfig {
	return StrategyConfig{
		ID:       "hl-eth-hedged",
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "check_hyperliquid.py",
		Args:     []string{"--symbol", "ETH", "live"},
		Hedge:    hedge,
	}
}

// --- accessors ---

func TestHedgeEnabled(t *testing.T) {
	if (StrategyConfig{}).HedgeEnabled() {
		t.Error("nil Hedge should not be enabled")
	}
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: false, Symbol: "BTC"})
	if sc.HedgeEnabled() {
		t.Error("Enabled=false should not be enabled")
	}
	sc = hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC"})
	if !sc.HedgeEnabled() {
		t.Error("Enabled=true should be enabled")
	}
}

func TestHedgeCoinNormalization(t *testing.T) {
	cases := []struct {
		symbol string
		want   string
	}{
		{"BTC", "BTC"},
		{"btc", "BTC"},
		{" btc ", "BTC"},
		{"BTC/USDC:USDC", "BTC"},
		{"", ""},
	}
	for _, c := range cases {
		sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: c.symbol})
		if got := hedgeCoin(sc); got != c.want {
			t.Errorf("hedgeCoin(%q) = %q, want %q", c.symbol, got, c.want)
		}
	}
	if got := hedgeCoin(StrategyConfig{}); got != "" {
		t.Errorf("hedgeCoin with nil Hedge = %q, want empty", got)
	}
}

func TestHedgeRatioDefault(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC"})
	if got := hedgeRatio(sc); got != 1.0 {
		t.Errorf("default hedgeRatio = %v, want 1.0", got)
	}
	sc = hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2.5})
	if got := hedgeRatio(sc); got != 2.5 {
		t.Errorf("hedgeRatio = %v, want 2.5", got)
	}
}

func TestHedgeLeverageAndMarginModeDefaults(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC"})
	if got := hedgeExchangeLeverage(sc); got != 1 {
		t.Errorf("default hedgeExchangeLeverage = %v, want 1", got)
	}
	if got := hedgeMarginMode(sc); got != "isolated" {
		t.Errorf("default hedgeMarginMode = %q, want isolated", got)
	}
	sc = hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Leverage: 3, MarginMode: "cross"})
	if got := hedgeExchangeLeverage(sc); got != 3 {
		t.Errorf("hedgeExchangeLeverage = %v, want 3", got)
	}
	if got := hedgeMarginMode(sc); got != "cross" {
		t.Errorf("hedgeMarginMode = %q, want cross", got)
	}
}

func TestHedgeSideForPrimary(t *testing.T) {
	if got := hedgeSideForPrimary("long"); got != "short" {
		t.Errorf("hedgeSideForPrimary(long) = %q, want short", got)
	}
	if got := hedgeSideForPrimary("short"); got != "long" {
		t.Errorf("hedgeSideForPrimary(short) = %q, want long", got)
	}
}

func TestHedgeConfigEqualAndClone(t *testing.T) {
	if !hedgeConfigEqual(nil, nil) {
		t.Error("nil == nil should be equal")
	}
	a := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}
	if hedgeConfigEqual(nil, a) || hedgeConfigEqual(a, nil) {
		t.Error("nil vs non-nil should not be equal")
	}
	b := &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}
	if !hedgeConfigEqual(a, b) {
		t.Error("identical blocks should be equal")
	}
	c := cloneHedgeConfig(a)
	if c == a {
		t.Error("cloneHedgeConfig must return a distinct pointer")
	}
	if !hedgeConfigEqual(a, c) {
		t.Error("clone should be equal to original")
	}
	if cloneHedgeConfig(nil) != nil {
		t.Error("cloneHedgeConfig(nil) should return nil")
	}
}

// --- pure sizing helpers ---

func TestHedgeOpenNotionalQty(t *testing.T) {
	qty, ok := hedgeOpenNotionalQty(2, 3000, 1.0, 60000)
	if !ok {
		t.Fatal("expected ok=true")
	}
	// notional = 2*3000*1 = 6000; qty = 6000/60000 = 0.1
	if !approxEq(qty, 0.1) {
		t.Errorf("qty = %v, want 0.1", qty)
	}
	if _, ok := hedgeOpenNotionalQty(0, 3000, 1, 60000); ok {
		t.Error("zero primary qty should fail closed")
	}
	if _, ok := hedgeOpenNotionalQty(2, 0, 1, 60000); ok {
		t.Error("zero primary price should fail closed")
	}
	if _, ok := hedgeOpenNotionalQty(2, 3000, 1, 0); ok {
		t.Error("zero hedge price should fail closed")
	}
	if _, ok := hedgeOpenNotionalQty(2, 3000, 0, 60000); ok {
		t.Error("zero ratio should fail closed")
	}
}

func TestHedgeReduceQty(t *testing.T) {
	// Primary halved (basis 2 -> 1): reduce hedge by 50%.
	got := hedgeReduceQty(1.0, 2.0, 1.0)
	if !approxEq(got, 0.5) {
		t.Errorf("hedgeReduceQty = %v, want 0.5", got)
	}
	// Primary flat: full reduce.
	got = hedgeReduceQty(1.0, 2.0, 0)
	if !approxEq(got, 1.0) {
		t.Errorf("hedgeReduceQty full-close = %v, want 1.0", got)
	}
	// No change.
	got = hedgeReduceQty(1.0, 2.0, 2.0)
	if !approxEq(got, 0) {
		t.Errorf("hedgeReduceQty no-op = %v, want 0", got)
	}
	// Zero basis / zero hedge qty guard.
	if got := hedgeReduceQty(0, 2, 1); got != 0 {
		t.Errorf("hedgeReduceQty with zero hedgeQty = %v, want 0", got)
	}
	if got := hedgeReduceQty(1, 0, 1); got != 0 {
		t.Errorf("hedgeReduceQty with zero basis = %v, want 0", got)
	}
}

// --- decision core ---

func TestHedgeTargetDecisionDisabled(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	action := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long"}, 3000, 60000)
	if action.Kind != hedgeActionNone {
		t.Errorf("Kind = %v, want none when hedge disabled", action.Kind)
	}
}

func TestHedgeTargetDecisionOpen(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
	action := hedgeTargetDecision(sc, snap, 3000, 60000)
	if action.Kind != hedgeActionOpen {
		t.Fatalf("Kind = %v, want open", action.Kind)
	}
	if action.Side != "short" {
		t.Errorf("Side = %q, want short (inverse of primary long)", action.Side)
	}
	if !approxEq(action.Qty, 0.1) {
		t.Errorf("Qty = %v, want 0.1", action.Qty)
	}

	// Short primary -> long hedge.
	snap.PrimarySide = "short"
	action = hedgeTargetDecision(sc, snap, 3000, 60000)
	if action.Side != "long" {
		t.Errorf("Side = %q, want long (inverse of primary short)", action.Side)
	}
}

func TestHedgeTargetDecisionOpenFailsClosedOnBadPrice(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC"})
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
	action := hedgeTargetDecision(sc, snap, 3000, 0) // no hedge mark
	if action.Kind != hedgeActionNone {
		t.Errorf("Kind = %v, want none (fail-closed) on unusable hedge price", action.Kind)
	}
	if action.Reason == "" {
		t.Error("expected a Reason explaining the fail-closed no-op")
	}
}

func TestHedgeTargetDecisionCloseOnPrimaryFlat(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC"})
	snap := hedgeSnapshot{HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
	action := hedgeTargetDecision(sc, snap, 3000, 60000)
	if action.Kind != hedgeActionCloseFull {
		t.Fatalf("Kind = %v, want closeFull when primary flat", action.Kind)
	}
	if !approxEq(action.Qty, 0.1) {
		t.Errorf("Qty = %v, want 0.1 (full hedge qty)", action.Qty)
	}
}

func TestHedgeTargetDecisionNoneWhenBothFlat(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC"})
	action := hedgeTargetDecision(sc, hedgeSnapshot{}, 3000, 60000)
	if action.Kind != hedgeActionNone {
		t.Errorf("Kind = %v, want none when both flat", action.Kind)
	}
}

func TestHedgeTargetDecisionAdd(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	// Primary grew from basis 2 to 3; hedge already open at side short.
	snap := hedgeSnapshot{PrimaryQty: 3, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
	action := hedgeTargetDecision(sc, snap, 3000, 60000)
	if action.Kind != hedgeActionAdd {
		t.Fatalf("Kind = %v, want add", action.Kind)
	}
	// delta = 1 primary unit; notional = 1*3000*1 = 3000; qty = 3000/60000 = 0.05
	if !approxEq(action.Qty, 0.05) {
		t.Errorf("Qty = %v, want 0.05", action.Qty)
	}
	if action.Side != "short" {
		t.Errorf("Side = %q, want short", action.Side)
	}
}

func TestHedgeTargetDecisionReduce(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	// Primary shrank from basis 2 to 1 (50%); hedge qty large enough that the
	// resulting reduce clears the min-notional dust floor.
	snap := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 1.0, HedgeSide: "short", HedgeBasis: 2}
	action := hedgeTargetDecision(sc, snap, 3000, 60000)
	if action.Kind != hedgeActionReduce {
		t.Fatalf("Kind = %v, want reduce", action.Kind)
	}
	if !approxEq(action.Qty, 0.5) {
		t.Errorf("Qty = %v, want 0.5 (50%% of hedge qty)", action.Qty)
	}
}

func TestHedgeTargetDecisionReduceDustDeferred(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	// Tiny hedge position; the proportional reduce would be well under the
	// $10 minimum order notional at the given hedge mark.
	snap := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.0001, HedgeSide: "short", HedgeBasis: 2}
	action := hedgeTargetDecision(sc, snap, 3000, 60000)
	if action.Kind != hedgeActionNone {
		t.Errorf("Kind = %v, want none (dust deferred)", action.Kind)
	}
}

func TestHedgeTargetDecisionReduceToFullCloseWhenBasisExhausted(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	// PrimaryQty within hedgeQtyEpsilon of the reduce-to-full threshold ->
	// the computed reduce fraction rounds to 1.0 within tolerance -> closeFull
	// rather than leaving an unfillable dust-sized hedge residual.
	snap := hedgeSnapshot{PrimaryQty: 2e-9, PrimarySide: "long", HedgeQty: 1.0, HedgeSide: "short", HedgeBasis: 2}
	action := hedgeTargetDecision(sc, snap, 3000, 60000)
	if action.Kind != hedgeActionCloseFull {
		t.Errorf("Kind = %v, want closeFull when primary ~ exhausted vs basis", action.Kind)
	}
}

func TestHedgeTargetDecisionWrongSideDefenseInDepth(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC"})
	// Hedge is long but primary is also long (desired hedge side = short) —
	// unreachable via normal flow (direction=both rejected at load) but must
	// never be left as-is.
	snap := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "long", HedgeBasis: 1}
	action := hedgeTargetDecision(sc, snap, 3000, 60000)
	if action.Kind != hedgeActionCloseFull {
		t.Errorf("Kind = %v, want closeFull on side mismatch", action.Kind)
	}
}

func TestHedgeTargetDecisionNoneWhenBasisMatches(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC"})
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
	action := hedgeTargetDecision(sc, snap, 3000, 60000)
	if action.Kind != hedgeActionNone {
		t.Errorf("Kind = %v, want none when primary qty matches basis", action.Kind)
	}
}

// --- runHedgeSync integration: HedgePrimaryQtyBasis must advance on reduce
// (review finding: pre-fix, applyHedgeReduceFill never touched the basis, so
// a stable primary after one partial close kept re-reducing the hedge every
// cycle — geometric decay to dust instead of converging after one reduce) ---

func TestRunHedgeSyncReduceAdvancesBasisThenConverges(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	now := time.Now().UTC()
	s := &StrategyState{
		ID: "hl-eth-hedged", Type: "perps", Platform: "hyperliquid",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", Multiplier: 1, OpenedAt: now},
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1,
				HedgeFor: "ETH", HedgePrimaryQtyBasis: 2, OpenedAt: now},
		},
	}
	prices := map[string]float64{"ETH": 3000, "BTC": 60000}
	var mu sync.RWMutex

	// Cycle 1: primary already sitting at 1 against a basis of 2 (a partial
	// close of the primary happened, e.g. tiered TP) -> one proportional
	// reduce to 0.05, and the basis must advance to the new primary qty (1).
	runHedgeSync(sc, s, prices, &mu, false, nil, nil)
	hpos := s.Positions["BTC"]
	if hpos == nil {
		t.Fatal("hedge position closed unexpectedly on cycle 1")
	}
	if !approxEq(hpos.Quantity, 0.05) {
		t.Errorf("cycle 1: hedge Quantity = %v, want 0.05", hpos.Quantity)
	}
	if !approxEq(hpos.HedgePrimaryQtyBasis, 1) {
		t.Errorf("cycle 1: HedgePrimaryQtyBasis = %v, want 1 (advanced to the new primary qty)", hpos.HedgePrimaryQtyBasis)
	}

	// Must survive (a): primary held steady at 1 for a subsequent cycle -> no
	// further action. Pre-fix, the stale basis of 2 would trigger another
	// reduce here even though the primary hasn't moved since cycle 1.
	runHedgeSync(sc, s, prices, &mu, false, nil, nil)
	hpos = s.Positions["BTC"]
	if hpos == nil {
		t.Fatal("hedge position closed unexpectedly on cycle 2")
	}
	if !approxEq(hpos.Quantity, 0.05) {
		t.Errorf("cycle 2 (primary unchanged): hedge Quantity = %v, want unchanged 0.05 (no compounding reduce)", hpos.Quantity)
	}

	// Must survive (b): a second, independent partial close (1 -> 0.6)
	// reduces once more, proportional to the now-corrected basis of 1 (frac
	// 0.4 -> reduce 0.02), not a compounded fraction of the already-halved
	// hedge.
	s.Positions["ETH"].Quantity = 0.6
	runHedgeSync(sc, s, prices, &mu, false, nil, nil)
	hpos = s.Positions["BTC"]
	if hpos == nil {
		t.Fatal("hedge position closed unexpectedly on cycle 3")
	}
	if !approxEq(hpos.Quantity, 0.03) {
		t.Errorf("cycle 3: hedge Quantity = %v, want 0.03", hpos.Quantity)
	}
	if !approxEq(hpos.HedgePrimaryQtyBasis, 0.6) {
		t.Errorf("cycle 3: HedgePrimaryQtyBasis = %v, want 0.6", hpos.HedgePrimaryQtyBasis)
	}
}

// Must survive (c): a partial close followed by a scale-in add re-sizes the
// add from the corrected (post-reduce) basis, not a stale pre-reduce one.
// Pre-fix this manifests as the wrong action entirely: with the basis stuck
// at 2, a primary increase to 1.5 computes delta = 1.5-2 = -0.5 (a spurious
// REDUCE) instead of the correct +0.5 ADD.
func TestRunHedgeSyncReduceThenAddUsesCorrectedBasis(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	now := time.Now().UTC()
	s := &StrategyState{
		ID: "hl-eth-hedged", Type: "perps", Platform: "hyperliquid",
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", Multiplier: 1, OpenedAt: now},
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1,
				HedgeFor: "ETH", HedgePrimaryQtyBasis: 2, OpenedAt: now},
		},
	}
	prices := map[string]float64{"ETH": 3000, "BTC": 60000}
	var mu sync.RWMutex

	runHedgeSync(sc, s, prices, &mu, false, nil, nil) // cycle 1: reduce to 0.05, basis -> 1

	s.Positions["ETH"].Quantity = 1.5 // scale-in add
	runHedgeSync(sc, s, prices, &mu, false, nil, nil)
	hpos := s.Positions["BTC"]
	if hpos == nil {
		t.Fatal("hedge position closed unexpectedly")
	}
	if !approxEq(hpos.Quantity, 0.075) {
		t.Errorf("after add: hedge Quantity = %v, want 0.075 (0.05 + 0.5*3000/60000)", hpos.Quantity)
	}
	if !approxEq(hpos.HedgePrimaryQtyBasis, 1.5) {
		t.Errorf("after add: HedgePrimaryQtyBasis = %v, want 1.5", hpos.HedgePrimaryQtyBasis)
	}
}

// --- config validation (collision matrix + vocabulary) ---

func TestValidateHedgeConfigsCollisionOwnCoin(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "s1", Type: "perps", Platform: "hyperliquid", Args: []string{"--symbol", "BTC"},
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
	}
	errs := validateHedgeConfigs(strategies)
	if len(errs) == 0 {
		t.Fatal("expected an error for hedge coin == own coin")
	}
}

func TestValidateHedgeConfigsCollisionConfiguredCoin(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "eth-strat", Type: "perps", Platform: "hyperliquid", Args: []string{"--symbol", "ETH"},
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
		{ID: "btc-strat", Type: "perps", Platform: "hyperliquid", Args: []string{"--symbol", "BTC"}},
	}
	errs := validateHedgeConfigs(strategies)
	if len(errs) == 0 {
		t.Fatal("expected an error for hedge coin colliding with another strategy's configured coin")
	}
}

func TestValidateHedgeConfigsCollisionHedgeVsHedge(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "eth-strat", Type: "perps", Platform: "hyperliquid", Args: []string{"--symbol", "ETH"},
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
		{ID: "sol-strat", Type: "perps", Platform: "hyperliquid", Args: []string{"--symbol", "SOL"},
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
	}
	errs := validateHedgeConfigs(strategies)
	if len(errs) == 0 {
		t.Fatal("expected an error for two strategies sharing a hedge coin")
	}
}

func TestValidateHedgeConfigsValidNoCollision(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "eth-strat", Type: "perps", Platform: "hyperliquid", Args: []string{"--symbol", "ETH"},
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}},
	}
	if errs := validateHedgeConfigs(strategies); len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}
}

func TestValidateHedgeConfigsRejectsNonPerpsHL(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "spot-strat", Type: "spot", Platform: "binanceus",
			Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}},
	}
	if errs := validateHedgeConfigs(strategies); len(errs) == 0 {
		t.Fatal("expected an error for hedge on a non-HL-perps strategy")
	}
}

func TestValidateHedgeConfigsRejectsBadVocabulary(t *testing.T) {
	base := StrategyConfig{ID: "s1", Type: "perps", Platform: "hyperliquid", Args: []string{"--symbol", "ETH"}}
	cases := []*HedgeConfig{
		{Enabled: true, Symbol: "BTC", Side: "same"},
		{Enabled: true, Symbol: "BTC", Ratio: 11},
		{Enabled: true, Symbol: "BTC", Ratio: -1},
		{Enabled: true, Symbol: "BTC", MarginMode: "bogus"},
		{Enabled: true, Symbol: "BTC", Leverage: -1},
		{Enabled: true, Symbol: "BTC", Platform: "okx"},
		{Enabled: true, Symbol: "BTC", Type: "spot"},
		{Enabled: true, Symbol: ""},
	}
	for i, h := range cases {
		sc := base
		sc.Hedge = h
		if errs := validateHedgeConfigs([]StrategyConfig{sc}); len(errs) == 0 {
			t.Errorf("case %d: expected a validation error for %+v", i, h)
		}
	}
}

func TestValidateHedgeConfigsRejectsDirectionBoth(t *testing.T) {
	sc := StrategyConfig{ID: "s1", Type: "perps", Platform: "hyperliquid", Args: []string{"--symbol", "ETH"},
		Direction: "both", Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	if errs := validateHedgeConfigs([]StrategyConfig{sc}); len(errs) == 0 {
		t.Fatal("expected an error for hedge + direction=both")
	}
}

func TestValidateHedgeConfigsDisabledBlockStillVocabularyChecked(t *testing.T) {
	sc := StrategyConfig{ID: "s1", Type: "perps", Platform: "hyperliquid", Args: []string{"--symbol", "ETH"},
		Hedge: &HedgeConfig{Enabled: false, Symbol: "BTC", Side: "bogus"}}
	if errs := validateHedgeConfigs([]StrategyConfig{sc}); len(errs) == 0 {
		t.Fatal("expected a vocabulary error even on a disabled hedge block")
	}
}

// Review finding: a disabled hedge block must be exempt from the live-topology
// check (platform=hyperliquid type=perps) the same way it's exempt from the
// collision matrix — it can never place an order, so it must not hard-fail
// config load on an otherwise-valid non-HL/non-perps strategy.
func TestValidateHedgeConfigsDisabledBlockExemptFromTopologyCheck(t *testing.T) {
	sc := StrategyConfig{ID: "spot-strat", Type: "spot", Platform: "binanceus",
		Hedge: &HedgeConfig{Enabled: false, Symbol: "BTC"}}
	if errs := validateHedgeConfigs([]StrategyConfig{sc}); len(errs) != 0 {
		t.Errorf("expected no errors for a disabled hedge block on a non-HL-perps strategy, got %v", errs)
	}
}

func TestValidateHedgeConfigsEnabledBlockOnSpotStillRejected(t *testing.T) {
	sc := StrategyConfig{ID: "spot-strat", Type: "spot", Platform: "binanceus",
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	if errs := validateHedgeConfigs([]StrategyConfig{sc}); len(errs) == 0 {
		t.Fatal("expected an error for an ENABLED hedge block on a non-HL-perps strategy")
	}
}

func TestValidateHedgeConfigsManualTypeRejected(t *testing.T) {
	sc := StrategyConfig{ID: "s1", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	if errs := validateHedgeConfigs([]StrategyConfig{sc}); len(errs) == 0 {
		t.Fatal("expected an error for hedge on type=manual (phase 1 perps-only)")
	}
}

// --- nested unknown-key guard ---

func TestValidateHedgeJSONKeysRejectsTypo(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"s1","hedge":{"enabled":true,"symbol":"BTC","ration":2}}]}`)
	errs := validateHedgeJSONKeys(raw)
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 unknown-field error, got %v", errs)
	}
}

func TestValidateHedgeJSONKeysAcceptsKnownFields(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"s1","hedge":{"enabled":true,"symbol":"BTC","side":"inverse","ratio":1,"platform":"hyperliquid","type":"perps","margin_mode":"cross","leverage":3}}]}`)
	if errs := validateHedgeJSONKeys(raw); len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}
}

// --- Position/Trade routing (TradeType, RiskState, diagnostics) ---

func TestBookPerpsCloseWithFillFeeRoutesHedgeTradeType(t *testing.T) {
	s := &StrategyState{
		ID:       "hl-eth-hedged",
		Type:     "perps",
		Platform: "hyperliquid",
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1,
				HedgeFor: "ETH", HedgePrimaryQtyBasis: 2, OpenedAt: time.Now().UTC()},
		},
	}
	if !bookPerpsCloseWithFillFee(s, "BTC", 59000, 0, false, "", "hedge_close", "Hedge close", "Hedge close", nil) {
		t.Fatal("bookPerpsCloseWithFillFee returned false")
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(s.TradeHistory))
	}
	if s.TradeHistory[0].TradeType != "hedge" {
		t.Errorf("TradeType = %q, want hedge", s.TradeHistory[0].TradeType)
	}
	if _, stillOpen := s.Positions["BTC"]; stillOpen {
		t.Error("position should be closed")
	}
}

// Review finding: the kill-switch/circuit-breaker close path
// (applyHyperliquidCircuitCloseFill, shared by applyHyperliquidKillSwitchCloseFill
// and the CB drain) hardcoded TradeType="perps" and called RecordTradeResult
// unconditionally, bypassing the hedge-aware routing bookPerps*CloseWithFillFee
// already had. A kill-switch/CB close of a sole-owned hedge leg must still tag
// TradeType="hedge" and route PnL via RecordHedgeTradeResult (DailyPnL only,
// never the loss streak) — otherwise a kill-switch flatten during a winning
// primary books a hedge "loss" that can spuriously trip the #1273 loss-streak
// circuit breaker after reset.
func TestApplyHyperliquidCircuitCloseFillRoutesHedgeTradeType(t *testing.T) {
	s := &StrategyState{
		ID: "hl-eth-hedged", Type: "perps", Platform: "hyperliquid",
		RiskState: RiskState{DailyPnLDate: time.Now().UTC().Format("2006-01-02")},
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 70000, Side: "short", Multiplier: 1,
				HedgeFor: "ETH", HedgePrimaryQtyBasis: 2, OpenedAt: time.Now().UTC()},
		},
	}
	// Hedge closes at a loss (short closed at a higher price than entry) —
	// exactly the shape a kill-switch flatten produces while the primary is
	// winning.
	applyHyperliquidCircuitCloseFill(s, "BTC", 0.1, 71000, 5, 0, 999, "kill_switch")

	if len(s.TradeHistory) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(s.TradeHistory))
	}
	if s.TradeHistory[0].TradeType != "hedge" {
		t.Errorf("TradeType = %q, want hedge", s.TradeHistory[0].TradeType)
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("ConsecutiveLosses = %d, want 0 (hedge losses must never feed the streak)", s.RiskState.ConsecutiveLosses)
	}
	if s.RiskState.DailyPnL >= 0 {
		t.Errorf("DailyPnL = %v, want negative (hedge PnL still feeds the daily loss limit)", s.RiskState.DailyPnL)
	}
}

// Sanity counterpart: a non-hedge (primary) coin closed via the same path is
// unaffected — still tags "perps" and still feeds the loss streak.
func TestApplyHyperliquidCircuitCloseFillNonHedgeUnaffected(t *testing.T) {
	s := &StrategyState{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
		RiskState: RiskState{DailyPnLDate: time.Now().UTC().Format("2006-01-02")},
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3100, Side: "long", Multiplier: 1, OpenedAt: time.Now().UTC()},
		},
	}
	applyHyperliquidCircuitCloseFill(s, "ETH", 1, 3000, 5, 0, 1000, "circuit_breaker")

	if s.TradeHistory[0].TradeType != "perps" {
		t.Errorf("TradeType = %q, want perps", s.TradeHistory[0].TradeType)
	}
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("ConsecutiveLosses = %d, want 1 (non-hedge loss must still feed the streak)", s.RiskState.ConsecutiveLosses)
	}
}

func TestRecordHedgeTradeResultNeverIncrementsLossStreak(t *testing.T) {
	r := &RiskState{DailyPnLDate: time.Now().UTC().Format("2006-01-02")}
	RecordHedgeTradeResult(r, -500)
	if r.ConsecutiveLosses != 0 {
		t.Errorf("ConsecutiveLosses = %d, want 0 (hedge losses must never feed the streak)", r.ConsecutiveLosses)
	}
	if !approxEq(r.DailyPnL, -500) {
		t.Errorf("DailyPnL = %v, want -500 (hedge PnL still feeds daily loss limit)", r.DailyPnL)
	}

	// Sanity: the primary-leg counterpart DOES increment the streak, so the
	// distinction is meaningful.
	r2 := &RiskState{DailyPnLDate: time.Now().UTC().Format("2006-01-02")}
	RecordTradeResult(r2, -500)
	if r2.ConsecutiveLosses != 1 {
		t.Errorf("primary RecordTradeResult ConsecutiveLosses = %d, want 1", r2.ConsecutiveLosses)
	}
}

func TestHedgeAwareTradeType(t *testing.T) {
	if got := hedgeAwareTradeType(&Position{}); got != "perps" {
		t.Errorf("hedgeAwareTradeType(no HedgeFor) = %q, want perps", got)
	}
	if got := hedgeAwareTradeType(&Position{HedgeFor: "ETH"}); got != "hedge" {
		t.Errorf("hedgeAwareTradeType(HedgeFor set) = %q, want hedge", got)
	}
	if got := hedgeAwareTradeType(nil); got != "perps" {
		t.Errorf("hedgeAwareTradeType(nil) = %q, want perps", got)
	}
}

// --- DB round-trip ---

func TestHedgePositionPersistsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-eth-hedged": {
				ID:       "hl-eth-hedged",
				Type:     "perps",
				Platform: "hyperliquid",
				Cash:     1000,
				Positions: map[string]*Position{
					"BTC": {
						Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000, Side: "short",
						Multiplier: 1, OwnerStrategyID: "hl-eth-hedged", OpenedAt: now,
						HedgeFor: "ETH", HedgePrimaryQtyBasis: 2,
					},
				},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	pos := loaded.Strategies["hl-eth-hedged"].Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge position missing after round trip")
	}
	if pos.HedgeFor != "ETH" {
		t.Errorf("HedgeFor = %q, want ETH", pos.HedgeFor)
	}
	if !approxEq(pos.HedgePrimaryQtyBasis, 2) {
		t.Errorf("HedgePrimaryQtyBasis = %v, want 2", pos.HedgePrimaryQtyBasis)
	}
}

// --- hot reload ---

func TestValidateHotReloadStateCompatibleBlocksHedgeChangeWhileOpen(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	ns := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2})
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	next := &Config{Strategies: []StrategyConfig{ns}}
	state := &AppState{Strategies: map[string]*StrategyState{
		sc.ID: {ID: sc.ID, Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, Side: "long"},
		}},
	}}
	if err := validateHotReloadStateCompatible(cfg, next, state); err == nil {
		t.Fatal("expected hedge ratio change to be rejected while a position is open")
	}
}

func TestValidateHotReloadStateCompatibleAllowsHedgeChangeWhileFlat(t *testing.T) {
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	ns := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2})
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	next := &Config{Strategies: []StrategyConfig{ns}}
	state := &AppState{Strategies: map[string]*StrategyState{
		sc.ID: {ID: sc.ID, Positions: map[string]*Position{}},
	}}
	if err := validateHotReloadStateCompatible(cfg, next, state); err != nil {
		t.Errorf("expected hedge ratio change to be allowed while flat, got: %v", err)
	}
}

func TestValidateHotReloadStateCompatibleAllowsHedgeChangeWithOnlyHedgeLegResidual(t *testing.T) {
	// Primary flat but a residual hedge leg still open — strategyHasOpenPositions
	// scans the whole Positions map (same map the hedge leg lives in), so this
	// must ALSO be blocked, not just a primary-position check.
	sc := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1})
	ns := hedgeTestStrategy(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 2})
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	next := &Config{Strategies: []StrategyConfig{ns}}
	state := &AppState{Strategies: map[string]*StrategyState{
		sc.ID: {ID: sc.ID, Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", HedgeFor: "ETH"},
		}},
	}}
	if err := validateHotReloadStateCompatible(cfg, next, state); err == nil {
		t.Fatal("expected hedge change to be rejected while a residual hedge leg is open")
	}
}
