package main

import (
	"testing"
	"time"
)

func hedgeTestStrategy(overrides func(*HedgeConfig)) StrategyConfig {
	hc := &HedgeConfig{Enabled: true, Symbol: "BTC"}
	if overrides != nil {
		overrides(hc)
	}
	return StrategyConfig{
		ID:       "hl-eth-long",
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "shared_scripts/check_hyperliquid.py",
		Args:     []string{"momentum", "ETH", "1h", "--mode=live"},
		Hedge:    hc,
	}
}

// --- Config accessor defaults (#1159) ---

func TestHedgeAccessors_Defaults(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	if !HedgeEnabled(sc) {
		t.Fatalf("HedgeEnabled = false, want true")
	}
	if got := HedgeRatio(sc); got != 1.0 {
		t.Errorf("HedgeRatio default = %v, want 1.0", got)
	}
	if got := hedgeLeverage(sc); got != 1.0 {
		t.Errorf("hedgeLeverage default = %v, want 1.0", got)
	}
	if got := hedgeMarginMode(sc); got != "isolated" {
		t.Errorf("hedgeMarginMode default = %q, want isolated", got)
	}
	if got := hedgeSide(sc); got != HedgeSideInverse {
		t.Errorf("hedgeSide default = %q, want %q", got, HedgeSideInverse)
	}
	if got := hedgeCoin(sc); got != "BTC" {
		t.Errorf("hedgeCoin = %q, want BTC", got)
	}
}

func TestHedgeAccessors_NotEnabled(t *testing.T) {
	sc := hedgeTestStrategy(func(hc *HedgeConfig) { hc.Enabled = false })
	if HedgeEnabled(sc) {
		t.Fatalf("HedgeEnabled = true, want false")
	}
	if got := HedgeRatio(sc); got != 0 {
		t.Errorf("HedgeRatio when disabled = %v, want 0", got)
	}
	if got := hedgeCoin(sc); got != "" {
		t.Errorf("hedgeCoin when disabled = %q, want empty", got)
	}
}

func TestHedgeCoin_CcxtSymbolNormalization(t *testing.T) {
	sc := hedgeTestStrategy(func(hc *HedgeConfig) { hc.Symbol = " btc/usdc:usdc " })
	if got := hedgeCoin(sc); got != "BTC" {
		t.Errorf("hedgeCoin ccxt normalization = %q, want BTC", got)
	}
}

func TestHedgeAccessors_ExplicitOverrides(t *testing.T) {
	sc := hedgeTestStrategy(func(hc *HedgeConfig) {
		hc.Ratio = 2.5
		hc.Leverage = 3
		hc.MarginMode = "cross"
	})
	if got := HedgeRatio(sc); got != 2.5 {
		t.Errorf("HedgeRatio = %v, want 2.5", got)
	}
	if got := hedgeLeverage(sc); got != 3 {
		t.Errorf("hedgeLeverage = %v, want 3", got)
	}
	if got := hedgeMarginMode(sc); got != "cross" {
		t.Errorf("hedgeMarginMode = %q, want cross", got)
	}
}

// --- Config validation (#1159) ---

func TestValidateHedgeConfigs_RejectsBadShape(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*StrategyConfig)
		wantErr bool
	}{
		{"valid", func(sc *StrategyConfig) {}, false},
		{"wrong platform", func(sc *StrategyConfig) { sc.Platform = "okx" }, true},
		{"wrong type", func(sc *StrategyConfig) { sc.Type = "spot" }, true},
		{"bad side", func(sc *StrategyConfig) { sc.Hedge.Side = "same" }, true},
		{"ratio too high", func(sc *StrategyConfig) { sc.Hedge.Ratio = 11 }, true},
		{"ratio negative", func(sc *StrategyConfig) { sc.Hedge.Ratio = -1 }, true},
		{"negative leverage", func(sc *StrategyConfig) { sc.Hedge.Leverage = -1 }, true},
		{"bad margin mode", func(sc *StrategyConfig) { sc.Hedge.MarginMode = "sideways" }, true},
		{"missing symbol", func(sc *StrategyConfig) { sc.Hedge.Symbol = "" }, true},
		{"direction both rejected", func(sc *StrategyConfig) { sc.Direction = DirectionBoth }, true},
		{"paper mode rejected", func(sc *StrategyConfig) { sc.Args = []string{"momentum", "ETH", "1h", "--mode=paper"} }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := hedgeTestStrategy(nil)
			tc.mutate(&sc)
			sc.OpenStrategy = StrategyRef{Name: "momentum"}
			sc.Capital = 1000
			sc.MaxDrawdownPct = 50
			cfg := &Config{ConfigVersion: CurrentConfigVersion, Strategies: []StrategyConfig{sc}}
			err := validateConfig(cfg, true)
			if tc.wantErr && err == nil {
				t.Fatalf("expected validation errors, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no validation errors, got %v", err)
			}
		})
	}
}

func TestHedgeCollisionErrors(t *testing.T) {
	own := hedgeTestStrategy(nil) // hedges BTC, primary coin ETH
	peerOnBTC := StrategyConfig{
		ID: "hl-btc-momentum", Type: "perps", Platform: "hyperliquid",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"momentum", "BTC", "1h", "--mode=live"},
	}
	errs := hedgeCollisionErrors([]StrategyConfig{own, peerOnBTC})
	if len(errs) == 0 {
		t.Fatalf("expected a collision error for hedge coin overlapping a configured strategy coin")
	}
}

func TestHedgeCollisionErrors_SameCoinAsPrimary(t *testing.T) {
	sc := hedgeTestStrategy(func(hc *HedgeConfig) { hc.Symbol = "ETH" }) // primary is also ETH
	errs := hedgeCollisionErrors([]StrategyConfig{sc})
	if len(errs) == 0 {
		t.Fatalf("expected a collision error for hedge coin == own primary coin")
	}
}

func TestHedgeCollisionErrors_HedgeVsHedge(t *testing.T) {
	a := hedgeTestStrategy(nil)
	b := hedgeTestStrategy(func(hc *HedgeConfig) {})
	b.ID = "hl-sol-long"
	b.Args = []string{"momentum", "SOL", "1h", "--mode=live"}
	errs := hedgeCollisionErrors([]StrategyConfig{a, b})
	if len(errs) == 0 {
		t.Fatalf("expected a collision error for two hedges sharing a coin")
	}
}

func TestHedgeCollisionErrors_NoCollision(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	errs := hedgeCollisionErrors([]StrategyConfig{sc})
	if len(errs) != 0 {
		t.Fatalf("expected no collision errors, got %v", errs)
	}
}

// --- Pure decision core (#1159) ---

func TestHedgeTargetDecision_BothFlat(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	got := hedgeTargetDecision(sc, hedgeSnapshot{}, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Fatalf("Kind = %v, want none", got.Kind)
	}
}

func TestHedgeTargetDecision_PrimaryFlatHedgeHeld_ClosesUnconditionally(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	snap := hedgeSnapshot{HedgeQty: 0.01, HedgeSide: "short", HedgeBasis: 2}
	// Deliberately pass unusable prices — closeFull must never fail closed on price.
	got := hedgeTargetDecision(sc, snap, 0, 0)
	if got.Kind != hedgeActionCloseFull || got.Qty != 0.01 || got.Side != "short" {
		t.Fatalf("got %+v, want closeFull qty=0.01 side=short", got)
	}
}

func TestHedgeTargetDecision_Open_InverseMapping(t *testing.T) {
	sc := hedgeTestStrategy(nil) // ratio defaults to 1.0
	longSnap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
	got := hedgeTargetDecision(sc, longSnap, 3000, 60000)
	if got.Kind != hedgeActionOpen || got.Side != "short" {
		t.Fatalf("long primary: got %+v, want open side=short", got)
	}
	wantQty := 2 * 3000 * 1.0 / 60000.0
	if !approxEq(got.Qty, wantQty) {
		t.Errorf("open qty = %v, want %v", got.Qty, wantQty)
	}

	shortSnap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "short"}
	got2 := hedgeTargetDecision(sc, shortSnap, 3000, 60000)
	if got2.Kind != hedgeActionOpen || got2.Side != "long" {
		t.Fatalf("short primary: got %+v, want open side=long", got2)
	}
}

func TestHedgeTargetDecision_Open_RatioSizing(t *testing.T) {
	sc := hedgeTestStrategy(func(hc *HedgeConfig) { hc.Ratio = 0.5 })
	snap := hedgeSnapshot{PrimaryQty: 4, PrimarySide: "long"}
	got := hedgeTargetDecision(sc, snap, 100, 100)
	wantQty := 4 * 100 * 0.5 / 100.0
	if got.Kind != hedgeActionOpen || !approxEq(got.Qty, wantQty) {
		t.Fatalf("got %+v, want open qty=%v", got, wantQty)
	}
}

func TestHedgeTargetDecision_Open_UnusablePriceFailsClosed(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
	if got := hedgeTargetDecision(sc, snap, 0, 60000); got.Kind != hedgeActionNone {
		t.Errorf("primaryPx=0: got %+v, want none", got)
	}
	if got := hedgeTargetDecision(sc, snap, 3000, 0); got.Kind != hedgeActionNone {
		t.Errorf("hedgePx=0: got %+v, want none", got)
	}
}

func TestHedgeTargetDecision_Add_DeltaSizing(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	snap := hedgeSnapshot{
		PrimaryQty: 3, PrimarySide: "long",
		HedgeQty: 1, HedgeSide: "short", HedgeBasis: 2,
	}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	wantQty := (3 - 2) * 3000 * 1.0 / 60000.0
	if got.Kind != hedgeActionAdd || got.Side != "short" || !approxEq(got.Qty, wantQty) {
		t.Fatalf("got %+v, want add qty=%v side=short", got, wantQty)
	}
}

func TestHedgeTargetDecision_Add_UnusablePriceFailsClosed(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	snap := hedgeSnapshot{PrimaryQty: 3, PrimarySide: "long", HedgeQty: 1, HedgeSide: "short", HedgeBasis: 2}
	got := hedgeTargetDecision(sc, snap, 0, 60000)
	if got.Kind != hedgeActionNone {
		t.Fatalf("got %+v, want none (unusable price)", got)
	}
}

func TestHedgeTargetDecision_Reduce_Proportional(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	// Primary shrank from 4 to 1 (75% reduction); hedge held 2 -> expect 1.5 reduced.
	snap := hedgeSnapshot{
		PrimaryQty: 1, PrimarySide: "long",
		HedgeQty: 2, HedgeSide: "short", HedgeBasis: 4,
	}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionReduce {
		t.Fatalf("Kind = %v, want reduce", got.Kind)
	}
	wantQty := 1.5
	if !approxEq(got.Qty, wantQty) {
		t.Errorf("reduce qty = %v, want %v", got.Qty, wantQty)
	}
}

func TestHedgeTargetDecision_Reduce_DustDeferred(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	// Tiny reduce: primary shrank from 100 to 99 (1%); hedge 0.001 BTC * fraction
	// * hedgePx must fall below the $10 floor.
	snap := hedgeSnapshot{
		PrimaryQty: 99, PrimarySide: "long",
		HedgeQty: 0.001, HedgeSide: "short", HedgeBasis: 100,
	}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Fatalf("Kind = %v, want none (dust-deferred)", got.Kind)
	}
}

func TestHedgeTargetDecision_ReduceFullyClampsToHedgeQty(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	// fraction computed > 1 in principle should never happen (PrimaryQty>=0 and
	// basis is a past snapshot >= PrimaryQty here) but clamp defensively.
	snap := hedgeSnapshot{
		PrimaryQty: 0.5, PrimarySide: "long",
		HedgeQty: 1, HedgeSide: "short", HedgeBasis: 1,
	}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionReduce || got.Qty > 1 {
		t.Fatalf("got %+v, want reduce qty <= 1", got)
	}
}

func TestHedgeTargetDecision_NoChange_None(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	snap := hedgeSnapshot{
		PrimaryQty: 2, PrimarySide: "long",
		HedgeQty: 1, HedgeSide: "short", HedgeBasis: 2,
	}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Fatalf("Kind = %v, want none (no qty change)", got.Kind)
	}
}

func TestHedgeTargetDecision_WrongSide_ClosesForReopen(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	// Primary is long (expects hedge short) but hedge is somehow long too.
	snap := hedgeSnapshot{
		PrimaryQty: 2, PrimarySide: "long",
		HedgeQty: 1, HedgeSide: "long", HedgeBasis: 2,
	}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionCloseFull || got.Side != "long" {
		t.Fatalf("got %+v, want closeFull side=long", got)
	}
}

func TestHedgeTargetDecision_NoBasisWatermark_NoAction(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	snap := hedgeSnapshot{
		PrimaryQty: 2, PrimarySide: "long",
		HedgeQty: 1, HedgeSide: "short", HedgeBasis: 0,
	}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Fatalf("Kind = %v, want none (no basis watermark)", got.Kind)
	}
}

func TestHedgeTargetDecision_NotEnabled(t *testing.T) {
	sc := hedgeTestStrategy(func(hc *HedgeConfig) { hc.Enabled = false })
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Fatalf("Kind = %v, want none (hedge not enabled)", got.Kind)
	}
}

// --- Skip-reason mirror (#1159) ---

func TestHedgeOrderSkipReason_MatchesPlanned(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
	planned := hedgeTargetDecision(sc, snap, 3000, 60000)
	if reason := hedgeOrderSkipReason(planned, sc, snap, 3000, 60000); reason != "" {
		t.Fatalf("expected no skip reason, got %q", reason)
	}
}

func TestHedgeOrderSkipReason_PrimaryClosedBetweenDecisionAndSpawn(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	planned := hedgeAction{Kind: hedgeActionOpen, Qty: 0.1, Side: "short"}
	// Fresh snapshot shows the primary went flat since the decision was made.
	freshSnap := hedgeSnapshot{}
	if reason := hedgeOrderSkipReason(planned, sc, freshSnap, 3000, 60000); reason == "" {
		t.Fatalf("expected a skip reason when primary state changed, got none")
	}
}

// --- Position field persistence round-trip (#1159) ---

func TestHedgePositionFieldsPersistRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-eth-long": {
				ID: "hl-eth-long", Type: "perps", Platform: "hyperliquid", Cash: 1000,
				Positions: map[string]*Position{
					"BTC": {
						Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 60000, Side: "short",
						Multiplier: 1, OwnerStrategyID: "hl-eth-long", OpenedAt: now,
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
	pos := loaded.Strategies["hl-eth-long"].Positions["BTC"]
	if pos.HedgeFor != "ETH" {
		t.Errorf("HedgeFor = %q, want ETH", pos.HedgeFor)
	}
	if !approxEq(pos.HedgePrimaryQtyBasis, 2) {
		t.Errorf("HedgePrimaryQtyBasis = %v, want 2", pos.HedgePrimaryQtyBasis)
	}
}

// A hedge leg's open trade is excluded from the #T open count, and its close
// trade is excluded from W/L round-trip grading (#1159) — mirrors scale_in's
// TestScaleInLegExcludedFromOpenCount.
func TestHedgeTradesExcludedFromLifetimeStats(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	primaryPID := "hl-eth-long:ETH:1:1"
	hedgePID := "hl-eth-long:BTC:1:1"
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-eth-long": {
				ID: "hl-eth-long", Type: "perps", Platform: "hyperliquid", Cash: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				TradeHistory: []Trade{
					{Timestamp: now.Add(-3 * time.Hour), StrategyID: "hl-eth-long", Symbol: "ETH", PositionID: primaryPID, Side: "buy", Quantity: 1, Price: 3000, Value: 3000, TradeType: "perps"},
					{Timestamp: now.Add(-3 * time.Hour), StrategyID: "hl-eth-long", Symbol: "BTC", PositionID: hedgePID, Side: "sell", Quantity: 0.05, Price: 60000, Value: 3000, TradeType: hedgeTradeType},
					{Timestamp: now.Add(-1 * time.Hour), StrategyID: "hl-eth-long", Symbol: "ETH", PositionID: primaryPID, Side: "sell", Quantity: 1, Price: 3200, Value: 3200, TradeType: "perps", IsClose: true, RealizedPnL: 200},
					{Timestamp: now.Add(-1 * time.Hour), StrategyID: "hl-eth-long", Symbol: "BTC", PositionID: hedgePID, Side: "buy", Quantity: 0.05, Price: 60500, Value: 3025, TradeType: hedgeTradeType, IsClose: true, RealizedPnL: -25},
				},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	stats, err := db.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	got := stats["hl-eth-long"]
	if got.PositionsOpened != 1 {
		t.Errorf("PositionsOpened = %d, want 1 (hedge open excluded)", got.PositionsOpened)
	}
	if got.Wins != 1 || got.Losses != 0 {
		t.Errorf("Wins=%d Losses=%d, want 1/0 (hedge close excluded from W/L)", got.Wins, got.Losses)
	}
}

// --- CB/kill-switch margin-drawdown hedge exclusion (#1159 review) ---

// A hedge leg's by-construction unrealized loss (the primary is in profit)
// must not enter the margin-drawdown numerator or denominator — otherwise a
// net-flat/net-winning hedged strategy could mis-fire its circuit breaker.
func TestPerpsMarginDrawdownInputs_ExcludesHedgeLeg(t *testing.T) {
	s := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3000, Side: "long", Multiplier: 1},
			"BTC": {Symbol: "BTC", Quantity: 0.05, AvgCost: 60000, Side: "short", Multiplier: 1, HedgeFor: "ETH"},
		},
	}
	prices := map[string]float64{"ETH": 3200, "BTC": 62000}
	loss, margin := perpsMarginDrawdownInputs(s, 10, prices)
	if loss != 0 {
		t.Errorf("unrealizedLoss = %v, want 0 (hedge leg excluded, primary is winning)", loss)
	}
	wantMargin := (1 * 3200.0) / 10
	if !approxEq(margin, wantMargin) {
		t.Errorf("margin = %v, want %v (only primary notional/leverage)", margin, wantMargin)
	}
}

// A primary leg genuinely losing must still count toward drawdown even with
// a hedge open alongside it — the exclusion must not dilute or suppress a
// real drawdown signal.
func TestPerpsMarginDrawdownInputs_PrimaryLossStillCounted(t *testing.T) {
	s := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, AvgCost: 3200, Side: "long", Multiplier: 1},
			"BTC": {Symbol: "BTC", Quantity: 0.05, AvgCost: 60000, Side: "short", Multiplier: 1, HedgeFor: "ETH"},
		},
	}
	prices := map[string]float64{"ETH": 3000, "BTC": 58000}
	loss, margin := perpsMarginDrawdownInputs(s, 10, prices)
	wantLoss := 1 * (3200.0 - 3000.0)
	if !approxEq(loss, wantLoss) {
		t.Errorf("unrealizedLoss = %v, want %v (primary's own loss must still count)", loss, wantLoss)
	}
	wantMargin := (1 * 3000.0) / 10
	if !approxEq(margin, wantMargin) {
		t.Errorf("margin = %v, want %v", margin, wantMargin)
	}
}

// A stray hedge leg with the primary already flat contributes nothing.
func TestPerpsMarginDrawdownInputs_StrayHedgeWithPrimaryFlat(t *testing.T) {
	s := &StrategyState{
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.05, AvgCost: 60000, Side: "short", Multiplier: 1, HedgeFor: "ETH"},
		},
	}
	prices := map[string]float64{"BTC": 65000}
	loss, margin := perpsMarginDrawdownInputs(s, 10, prices)
	if loss != 0 || margin != 0 {
		t.Errorf("loss=%v margin=%v, want 0/0 (stray hedge leg alone must not trip CB)", loss, margin)
	}
}

// --- Partial-fill basis proration (#1159 review) ---

func TestHedgeBasisAfterFill_FullFillConvergesFully(t *testing.T) {
	got := hedgeBasisAfterFill(2, 5, 3, 3)
	if !approxEq(got, 5) {
		t.Errorf("basis = %v, want 5 (full fill converges fully)", got)
	}
}

func TestHedgeBasisAfterFill_OpenPartialFillProratesFromZero(t *testing.T) {
	got := hedgeBasisAfterFill(0, 10, 1, 0.4)
	want := 4.0
	if !approxEq(got, want) {
		t.Errorf("basis = %v, want %v (40%% of the way from 0 to target)", got, want)
	}
}

func TestHedgeBasisAfterFill_AddPartialFillProratesFromOldBasis(t *testing.T) {
	got := hedgeBasisAfterFill(4, 10, 2, 1)
	want := 4 + 0.5*(10.0-4.0)
	if !approxEq(got, want) {
		t.Errorf("basis = %v, want %v", got, want)
	}
}

func TestHedgeBasisAfterFill_ZeroRequestedQtyFallsBackToTarget(t *testing.T) {
	got := hedgeBasisAfterFill(2, 5, 0, 0)
	if !approxEq(got, 5) {
		t.Errorf("basis = %v, want 5 (degenerate requestedQty falls back to target)", got)
	}
}

func TestHedgeBasisAfterFill_OverfillClampsFractionToOne(t *testing.T) {
	got := hedgeBasisAfterFill(0, 10, 1, 1.5)
	if !approxEq(got, 10) {
		t.Errorf("basis = %v, want 10 (fraction clamped to 1)", got)
	}
}

func TestApplyHedgeOpenOrAddFill_PartialFillProratesBasis(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := &StrategyState{ID: sc.ID, Positions: map[string]*Position{}}
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 1.0, Side: "short"}
	// Sizing intended to hedge primaryQtyAtEvent=2 fully via a 1.0-BTC order,
	// but the exchange only filled 0.4 BTC (thin book / slippage cap).
	got := applyHedgeOpenOrAddFill(s, sc, "BTC", action, 0.4, 60000, 5, 111, 2)
	if got != 1 {
		t.Fatalf("applyHedgeOpenOrAddFill returned %d, want 1", got)
	}
	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("expected BTC position to exist")
	}
	wantBasis := 0.4 / 1.0 * 2.0
	if !approxEq(pos.HedgePrimaryQtyBasis, wantBasis) {
		t.Errorf("HedgePrimaryQtyBasis = %v, want %v (prorated by 40%% fill, not stamped as fully converged)", pos.HedgePrimaryQtyBasis, wantBasis)
	}
}

func TestApplyHedgeOpenOrAddFill_FullFillSetsBasisToTarget(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := &StrategyState{ID: sc.ID, Positions: map[string]*Position{}}
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 1.0, Side: "short"}
	applyHedgeOpenOrAddFill(s, sc, "BTC", action, 1.0, 60000, 5, 111, 2)
	pos := s.Positions["BTC"]
	if !approxEq(pos.HedgePrimaryQtyBasis, 2) {
		t.Errorf("HedgePrimaryQtyBasis = %v, want 2 (full fill converges fully)", pos.HedgePrimaryQtyBasis)
	}
}

func TestApplyHedgeReduceOrCloseFill_PartialFillProratesBasis(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := &StrategyState{
		ID: sc.ID,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000, Side: "short",
				Multiplier: 1, OwnerStrategyID: sc.ID, HedgeFor: "ETH", HedgePrimaryQtyBasis: 4},
		},
	}
	// Primary shrank from basis=4 to primaryQtyAtEvent=2; the reduce action
	// requested cutting 0.05 BTC to fully converge, but only 0.02 BTC (40%) filled.
	action := hedgeAction{Kind: hedgeActionReduce, Qty: 0.05, Side: "short"}
	ok := applyHedgeReduceOrCloseFill(s, sc, "BTC", action, 0.02, 61000, 1, 222, 2, nil)
	if !ok {
		t.Fatalf("applyHedgeReduceOrCloseFill returned false, want true")
	}
	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("expected BTC position to still exist (partial reduce)")
	}
	wantBasis := 4 + (0.02/0.05)*(2.0-4.0)
	if !approxEq(pos.HedgePrimaryQtyBasis, wantBasis) {
		t.Errorf("HedgePrimaryQtyBasis = %v, want %v (prorated by 40%% fill, excess hedge left as a live delta)", pos.HedgePrimaryQtyBasis, wantBasis)
	}
}

func TestApplyHedgeReduceOrCloseFill_FullFillSetsBasisToTarget(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := &StrategyState{
		ID: sc.ID,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 60000, Side: "short",
				Multiplier: 1, OwnerStrategyID: sc.ID, HedgeFor: "ETH", HedgePrimaryQtyBasis: 4},
		},
	}
	action := hedgeAction{Kind: hedgeActionReduce, Qty: 0.05, Side: "short"}
	applyHedgeReduceOrCloseFill(s, sc, "BTC", action, 0.05, 61000, 1, 222, 2, nil)
	pos := s.Positions["BTC"]
	if !approxEq(pos.HedgePrimaryQtyBasis, 2) {
		t.Errorf("HedgePrimaryQtyBasis = %v, want 2 (full fill converges fully)", pos.HedgePrimaryQtyBasis)
	}
}
