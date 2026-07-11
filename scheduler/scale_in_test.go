package main

import (
	"testing"
	"time"
)

// Scale-in per-position state survives a SaveState/LoadState round-trip (#873).
func TestScaleInStatePersistsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-scalein-eth": {
				ID:       "hl-scalein-eth",
				Type:     "perps",
				Platform: "hyperliquid",
				Cash:     1000,
				Positions: map[string]*Position{
					"ETH": {
						Symbol: "ETH", Quantity: 2, InitialQuantity: 2, AvgCost: 2100, Side: "long",
						Multiplier: 1, OwnerStrategyID: "hl-scalein-eth", OpenedAt: now,
						ScaleInCount: 3, LastAddPrice: 2200, AddedNotionalUSD: 2200, RiskAnchorPrice: 2000,
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
	pos := loaded.Strategies["hl-scalein-eth"].Positions["ETH"]
	if pos.ScaleInCount != 3 {
		t.Errorf("ScaleInCount = %d, want 3", pos.ScaleInCount)
	}
	if !approxEq(pos.LastAddPrice, 2200) {
		t.Errorf("LastAddPrice = %v, want 2200", pos.LastAddPrice)
	}
	if !approxEq(pos.AddedNotionalUSD, 2200) {
		t.Errorf("AddedNotionalUSD = %v, want 2200", pos.AddedNotionalUSD)
	}
	if !approxEq(pos.RiskAnchorPrice, 2000) {
		t.Errorf("RiskAnchorPrice = %v, want 2000", pos.RiskAnchorPrice)
	}
}

// A scale_in leg is excluded from the #T open count but the round-trip is still
// counted and graded (#873).
func TestScaleInLegExcludedFromOpenCount(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	pid := "hl-scalein-eth:ETH:1:1"
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-scalein-eth": {
				ID: "hl-scalein-eth", Type: "perps", Platform: "hyperliquid", Cash: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				TradeHistory: []Trade{
					{Timestamp: now.Add(-3 * time.Hour), StrategyID: "hl-scalein-eth", Symbol: "ETH", PositionID: pid, Side: "buy", Quantity: 1, Price: 2000, Value: 2000, TradeType: "perps"},
					{Timestamp: now.Add(-2 * time.Hour), StrategyID: "hl-scalein-eth", Symbol: "ETH", PositionID: pid, Side: "buy", Quantity: 1, Price: 2100, Value: 2100, TradeType: scaleInTradeType},
					{Timestamp: now.Add(-1 * time.Hour), StrategyID: "hl-scalein-eth", Symbol: "ETH", PositionID: pid, Side: "sell", Quantity: 2, Price: 2300, Value: 4600, TradeType: "perps", IsClose: true, RealizedPnL: 500},
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
	got := stats["hl-scalein-eth"]
	if got.PositionsOpened != 1 {
		t.Errorf("PositionsOpened = %d, want 1 (scale_in leg excluded)", got.PositionsOpened)
	}
	if got.Wins != 1 {
		t.Errorf("Wins = %d, want 1 (round-trip still graded)", got.Wins)
	}
	// per-strategy variant mirrors the exclusion
	one, err := db.LifetimeTradeStatsForStrategy("hl-scalein-eth")
	if err != nil {
		t.Fatalf("LifetimeTradeStatsForStrategy: %v", err)
	}
	if one.PositionsOpened != 1 {
		t.Errorf("per-strategy PositionsOpened = %d, want 1", one.PositionsOpened)
	}
}

// After a scale-in the protection re-size force-replaces the SL and already-
// placed (un-cleared) TP tiers, leaving un-placed tiers for fresh placement and
// never resetting the cleared-tier watermark (#873).
func TestScaleInProtectionForceReplace(t *testing.T) {
	// tier 0 already filled (OID 0, armed), tier 1 still resting (OID > 0).
	pos := &Position{
		TPOIDs:                   []int64{0, 555},
		TPArmedTiers:             []bool{true, true},
		SLAdjustedTiersProcessed: 1,
	}
	plan := hlProtectionPlan{
		StopLossATRMult: 1.5,
		Tiers:           []hlProtectionTier{{Multiple: 1}, {Multiple: 2}},
	}
	forceSL, forceTP := scaleInProtectionForceReplace(pos, plan)
	if !forceSL {
		t.Errorf("forceSL = false, want true (SL must grow to cover the new total)")
	}
	if len(forceTP) != 2 {
		t.Fatalf("forceTP len = %d, want 2", len(forceTP))
	}
	if forceTP[0] {
		t.Errorf("forceTP[0] = true, want false (cleared tier must not be re-placed)")
	}
	if !forceTP[1] {
		t.Errorf("forceTP[1] = false, want true (resting tier must resize to new total)")
	}
	// watermark untouched by the force-replace computation
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Errorf("watermark mutated: %d, want 1", pos.SLAdjustedTiersProcessed)
	}
}

// forceResize makes the trailing-stop walker cancel+replace at the EXISTING
// trigger even when no trailing move occurred, so a scale-in's grown size gets
// covered (#873 review finding 2). Without it, the same inputs are a no-op.
func TestTrailingStopForceResizeReplacesWithoutMove(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	var called bool
	var gotSize, gotTrigger float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		gotSize = size
		gotTrigger = triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 222, StopLossTriggerPx: triggerPx}, "", nil
	}
	trail := 3.0
	minMove := 0.25
	sc := StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", TrailingStopPct: &trail, TrailingStopMinMovePct: &minMove}
	logger := silentStrategyLogger("hl-test")
	defer logger.Close()

	// mark==highWater, currentTrigger already at the trailing level → no move.
	// forceResize=false: no replace.
	called = false
	_, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 2.0, &Position{AvgCost: 100}, 100, 100, 97, 111, false, nil, logger)
	if !ok || result != nil || called {
		t.Fatalf("without force, expected no replace (called=%v result=%+v)", called, result)
	}
	// forceResize=true: replace at the existing trigger (97) with the grown size (2.0).
	called = false
	_, result, ok = runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 2.0, &Position{AvgCost: 100}, 100, 100, 97, 111, true, nil, logger)
	if !ok || result == nil || !called {
		t.Fatalf("with force, expected a replace (called=%v result=%+v ok=%v)", called, result, ok)
	}
	if !approxEq(gotSize, 2.0) {
		t.Errorf("replace size = %v, want 2.0 (grown total)", gotSize)
	}
	if !approxEq(gotTrigger, 97) {
		t.Errorf("replace trigger = %v, want 97 (existing trigger, frozen)", gotTrigger)
	}
}

func TestOrForceReplace(t *testing.T) {
	got := orForceReplace([]bool{true, false}, []bool{false, false, true})
	want := []bool{true, false, true}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if orForceReplace(nil, nil) != nil {
		t.Errorf("orForceReplace(nil,nil) should be nil")
	}
}

// applyManualAction "add" blends a manual scale-in and records a scale_in leg;
// it refuses when no position is open (#873).
func TestApplyManualActionAddBlendsAndRecords(t *testing.T) {
	now := time.Now().UTC()
	ss := &StrategyState{
		ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Cash: 1000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 1, InitialQuantity: 1, AvgCost: 2000, Side: "long",
				EntryATR: 50, Regime: "trending", OwnerStrategyID: "hl-manual-eth", OpenedAt: now},
		},
		OptionPositions: map[string]*OptionPosition{},
	}
	state := &AppState{Strategies: map[string]*StrategyState{"hl-manual-eth": ss}}
	scByID := map[string]StrategyConfig{
		"hl-manual-eth": {ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH"},
	}
	add := PendingManualAction{
		StrategyID: "hl-manual-eth", Action: "add", Symbol: "ETH", Side: "long",
		Quantity: 1, FillPrice: 2200, FillFee: 1.5, CreatedAt: now,
	}
	if err := applyManualAction(state, nil, scByID, add); err != nil {
		t.Fatalf("applyManualAction add: %v", err)
	}
	pos := ss.Positions["ETH"]
	if !approxEq(pos.Quantity, 2) || !approxEq(pos.InitialQuantity, 2) {
		t.Errorf("qty/initial = %v/%v, want 2/2", pos.Quantity, pos.InitialQuantity)
	}
	if !approxEq(pos.AvgCost, 2100) { // (2000+2200)/2
		t.Errorf("AvgCost = %v, want 2100", pos.AvgCost)
	}
	if pos.ScaleInCount != 1 {
		t.Errorf("ScaleInCount = %d, want 1", pos.ScaleInCount)
	}
	if pos.EntryATR != 50 || pos.Regime != "trending" {
		t.Errorf("frozen fields moved: EntryATR=%v Regime=%q", pos.EntryATR, pos.Regime)
	}
	if !approxEq(ss.Cash, 998.5) { // 1000 - fee 1.5
		t.Errorf("Cash = %v, want 998.5", ss.Cash)
	}
	var found bool
	for _, tr := range ss.TradeHistory {
		if tr.TradeType == scaleInTradeType {
			found = true
			if tr.IsClose {
				t.Errorf("scale_in leg marked IsClose")
			}
		}
	}
	if !found {
		t.Errorf("no scale_in trade leg recorded")
	}

	// refuse when flat
	flat := &StrategyState{ID: "hl-manual-eth", Type: "manual", Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	state2 := &AppState{Strategies: map[string]*StrategyState{"hl-manual-eth": flat}}
	if err := applyManualAction(state2, nil, scByID, add); err == nil {
		t.Errorf("expected error adding to a flat strategy")
	}
}

// allow_scale_in is rejected outside HL perps/manual (#873).
func TestConfigValidationRejectsScaleInOffPlatform(t *testing.T) {
	cfg := &Config{
		Strategies: []StrategyConfig{
			{ID: "spot-x", Type: "spot", Platform: "binanceus", Script: "s.py", AllowScaleIn: true},
		},
	}
	err := validateConfig(cfg, true)
	if err == nil {
		t.Fatalf("expected validateConfig to reject allow_scale_in on spot/binanceus")
	}
}

// applyScaleIn blends price+size into an existing position while freezing the
// risk plan (#873).
func TestApplyScaleInBlendsPriceAndSizeFreezesRiskPlan(t *testing.T) {
	mult := 1.5
	pos := &Position{
		Symbol:                   "ETH",
		Side:                     "long",
		Quantity:                 100,
		InitialQuantity:          100,
		AvgCost:                  2000,
		EntryATR:                 50,
		Regime:                   "trending",
		RegimeWindows:            map[string]string{"medium": "trending"},
		SLAdjustedTiersProcessed: 1,
		TPArmedTiers:             []bool{true, false},
		StopLossATRMult:          &mult,
	}
	applyScaleIn(pos, 100, 2200)

	// blended average: (100*2000 + 100*2200)/200 = 2100
	if !approxEq(pos.AvgCost, 2100) {
		t.Fatalf("AvgCost = %v, want 2100", pos.AvgCost)
	}
	if !approxEq(pos.Quantity, 200) {
		t.Fatalf("Quantity = %v, want 200", pos.Quantity)
	}
	// InitialQuantity grows so Quantity < InitialQuantity stays the partial-close test
	if !approxEq(pos.InitialQuantity, 200) {
		t.Fatalf("InitialQuantity = %v, want 200", pos.InitialQuantity)
	}
	if pos.ScaleInCount != 1 {
		t.Fatalf("ScaleInCount = %d, want 1", pos.ScaleInCount)
	}
	if !approxEq(pos.LastAddPrice, 2200) {
		t.Fatalf("LastAddPrice = %v, want 2200", pos.LastAddPrice)
	}
	if !approxEq(pos.AddedNotionalUSD, 100*2200) {
		t.Fatalf("AddedNotionalUSD = %v, want %v", pos.AddedNotionalUSD, 100*2200.0)
	}
	if !pos.ScaleInResizePending {
		t.Fatalf("ScaleInResizePending = false, want true")
	}
	// frozen
	if !approxEq(pos.EntryATR, 50) {
		t.Fatalf("EntryATR moved: %v, want 50 (frozen)", pos.EntryATR)
	}
	if pos.Regime != "trending" {
		t.Fatalf("Regime moved: %q, want trending (frozen)", pos.Regime)
	}
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Fatalf("SLAdjustedTiersProcessed = %d, want 1 (watermark not reset)", pos.SLAdjustedTiersProcessed)
	}
	if len(pos.TPArmedTiers) != 2 || !pos.TPArmedTiers[0] || pos.TPArmedTiers[1] {
		t.Fatalf("TPArmedTiers changed: %v, want [true false] (watermark not reset)", pos.TPArmedTiers)
	}
}

func TestApplyScaleInMultipleAddsAccumulate(t *testing.T) {
	pos := &Position{Side: "short", Quantity: 10, InitialQuantity: 10, AvgCost: 100}
	applyScaleIn(pos, 10, 90)  // added 900
	applyScaleIn(pos, 10, 110) // added 1100
	if pos.ScaleInCount != 2 {
		t.Fatalf("ScaleInCount = %d, want 2", pos.ScaleInCount)
	}
	if !approxEq(pos.Quantity, 30) || !approxEq(pos.InitialQuantity, 30) {
		t.Fatalf("Quantity/InitialQuantity = %v/%v, want 30/30", pos.Quantity, pos.InitialQuantity)
	}
	// (10*100 + 10*90 + 10*110)/30 = 100
	if !approxEq(pos.AvgCost, 100) {
		t.Fatalf("AvgCost = %v, want 100", pos.AvgCost)
	}
	if !approxEq(pos.AddedNotionalUSD, 2000) {
		t.Fatalf("AddedNotionalUSD = %v, want 2000", pos.AddedNotionalUSD)
	}
	if !approxEq(pos.LastAddPrice, 110) {
		t.Fatalf("LastAddPrice = %v, want 110", pos.LastAddPrice)
	}
}

// applyScaleIn stamps a frozen risk anchor (= the AvgCost at first add, which is
// the original entry) so on-chain SL/TP triggers stay pinned to the first entry
// even though the blended AvgCost drives PnL (#873 review finding 1).
func TestApplyScaleInStampsFrozenRiskAnchor(t *testing.T) {
	pos := &Position{Side: "long", Quantity: 100, InitialQuantity: 100, AvgCost: 2000}
	applyScaleIn(pos, 100, 2200) // blend → AvgCost 2100
	if !approxEq(pos.RiskAnchorPrice, 2000) {
		t.Fatalf("RiskAnchorPrice = %v, want 2000 (original entry frozen)", pos.RiskAnchorPrice)
	}
	if !approxEq(pos.AvgCost, 2100) {
		t.Fatalf("AvgCost = %v, want 2100 (blended for PnL)", pos.AvgCost)
	}
	applyScaleIn(pos, 200, 2400) // second add must NOT move the anchor
	if !approxEq(pos.RiskAnchorPrice, 2000) {
		t.Fatalf("RiskAnchorPrice moved on second add: %v, want 2000", pos.RiskAnchorPrice)
	}
	if !approxEq(pos.riskAnchorPrice(), 2000) {
		t.Fatalf("riskAnchorPrice() = %v, want 2000", pos.riskAnchorPrice())
	}
}

// A position that never scaled in falls back to AvgCost for the risk anchor, so
// trigger geometry is unchanged for the common case (#873).
func TestRiskAnchorPriceFallsBackToAvgCost(t *testing.T) {
	pos := &Position{AvgCost: 1500}
	if !approxEq(pos.riskAnchorPrice(), 1500) {
		t.Fatalf("riskAnchorPrice() = %v, want 1500 (fallback to AvgCost)", pos.riskAnchorPrice())
	}
}

// The protection plan computes SL/TP triggers from the frozen risk anchor, not
// the blended AvgCost — so a forced re-size after a scale-in keeps triggers at
// the original entry (#873 review finding 1).
func TestProtectionPlanFreezesTriggersAtRiskAnchor(t *testing.T) {
	mult := 1.5
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 200, InitialQuantity: 200,
		AvgCost: 2100, RiskAnchorPrice: 2000, EntryATR: 50, StopLossATRMult: &mult,
	}
	sc := StrategyConfig{Type: "perps", Platform: "hyperliquid", StopLossATRMult: &mult}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos)
	if !ok {
		t.Fatalf("expected a protection plan")
	}
	if !approxEq(plan.AvgCost, 2000) {
		t.Fatalf("plan.AvgCost = %v, want 2000 (frozen anchor, not blended 2100)", plan.AvgCost)
	}
	if !approxEq(plan.Size, 200) {
		t.Fatalf("plan.Size = %v, want 200 (grown total)", plan.Size)
	}
}

func longSnap() scaleInSnapshot {
	return scaleInSnapshot{Side: "long", Quantity: 100, AvgCost: 2000, EntryATR: 50, LastAddPrice: 2000}
}

func TestPerpsScaleInDecisionRequiresOptIn(t *testing.T) {
	sc := StrategyConfig{AllowScaleIn: false}
	if _, ok, _ := perpsScaleInDecision(sc, longSnap(), 1, 2000, 1000); ok {
		t.Fatalf("scale-in allowed with AllowScaleIn=false")
	}
}

func TestPerpsScaleInDecisionDirectionMatch(t *testing.T) {
	sc := StrategyConfig{AllowScaleIn: true}
	// buy on a long → add
	if _, ok, _ := perpsScaleInDecision(sc, longSnap(), 1, 2000, 1000); !ok {
		t.Fatalf("buy on long should add")
	}
	// sell on a long → not an add (that's a close)
	if _, ok, _ := perpsScaleInDecision(sc, longSnap(), -1, 2000, 1000); ok {
		t.Fatalf("sell on long should NOT add")
	}
	// buy on a short → not an add (that's a cover/flip)
	short := scaleInSnapshot{Side: "short", Quantity: 100, AvgCost: 2000, EntryATR: 50, LastAddPrice: 2000}
	if _, ok, _ := perpsScaleInDecision(sc, short, 1, 2000, 1000); ok {
		t.Fatalf("buy on short should NOT add")
	}
	// sell on a short → add
	if _, ok, _ := perpsScaleInDecision(sc, short, -1, 2000, 1000); !ok {
		t.Fatalf("sell on short should add")
	}
	// flat → not an add
	flat := scaleInSnapshot{Side: "", Quantity: 0}
	if _, ok, _ := perpsScaleInDecision(sc, flat, 1, 2000, 1000); ok {
		t.Fatalf("add from flat should be rejected")
	}
}

func TestPerpsScaleInDecisionMaxAdds(t *testing.T) {
	sc := StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{MaxAdds: 2}}
	snap := longSnap()
	snap.ScaleInCount = 2
	if _, ok, reason := perpsScaleInDecision(sc, snap, 1, 2000, 1000); ok {
		t.Fatalf("add past max_adds allowed (reason=%q)", reason)
	}
	snap.ScaleInCount = 1
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 2000, 1000); !ok {
		t.Fatalf("add under max_adds rejected")
	}
}

func TestPerpsScaleInDecisionMaxAddedNotional(t *testing.T) {
	sc := StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{MaxAddedNotionalUSD: 1500}}
	snap := longSnap()
	snap.AddedNotionalUSD = 1000
	// next add of 1000 would push cumulative to 2000 > 1500
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 2000, 1000); ok {
		t.Fatalf("add past max_added_notional allowed")
	}
	// add of 400 → cumulative 1400 ≤ 1500
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 2000, 400); !ok {
		t.Fatalf("add under max_added_notional rejected")
	}
}

func TestPerpsScaleInDecisionSpacingAddToWinnersLong(t *testing.T) {
	sc := StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}}
	snap := longSnap() // EntryATR 50, LastAddPrice 2000; need +50 in-favor
	// price 2049 → +49 < 50 → blocked
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 2049, 1000); ok {
		t.Fatalf("add allowed before reaching spacing distance")
	}
	// price 2051 → +51 ≥ 50 → allowed
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 2051, 1000); !ok {
		t.Fatalf("add blocked after reaching spacing distance")
	}
	// adverse move blocks add-to-winners
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 1900, 1000); ok {
		t.Fatalf("add-to-winners allowed on adverse move")
	}
}

func TestPerpsScaleInDecisionSpacingAverageDownLong(t *testing.T) {
	sc := StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: -1.0}}
	snap := longSnap() // need -50 adverse (price down 50)
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 1951, 1000); ok {
		t.Fatalf("average-down allowed before reaching adverse distance")
	}
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 1949, 1000); !ok {
		t.Fatalf("average-down blocked after reaching adverse distance")
	}
	// favorable move blocks average-down
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 2100, 1000); ok {
		t.Fatalf("average-down allowed on favorable move")
	}
}

func TestPerpsScaleInDecisionSpacingShort(t *testing.T) {
	// short add-to-winners: price must move DOWN (in-favor for short)
	sc := StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}}
	snap := scaleInSnapshot{Side: "short", Quantity: 100, AvgCost: 2000, EntryATR: 50, LastAddPrice: 2000}
	if _, ok, _ := perpsScaleInDecision(sc, snap, -1, 1949, 1000); !ok {
		t.Fatalf("short add-to-winners blocked on favorable (down) move")
	}
	if _, ok, _ := perpsScaleInDecision(sc, snap, -1, 2100, 1000); ok {
		t.Fatalf("short add-to-winners allowed on adverse (up) move")
	}
}

func TestPerpsScaleInDecisionSpacingZeroNoGate(t *testing.T) {
	sc := StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 0}}
	if _, ok, _ := perpsScaleInDecision(sc, longSnap(), 1, 2000, 1000); !ok {
		t.Fatalf("zero spacing should not gate an add")
	}
}

func TestPerpsScaleInDecisionLastAddPriceFallsBackToAvgCost(t *testing.T) {
	sc := StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}}
	snap := longSnap()
	snap.LastAddPrice = 0 // pre-#873 position; fall back to AvgCost 2000
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 2051, 1000); !ok {
		t.Fatalf("spacing should measure from AvgCost when LastAddPrice unset")
	}
}

func TestPerpsScaleInDecisionAddQtySizing(t *testing.T) {
	// default notional (config AddNotionalUSD unset) → use the passed default
	sc := StrategyConfig{AllowScaleIn: true}
	addQty, ok, _ := perpsScaleInDecision(sc, longSnap(), 1, 2000, 1000)
	if !ok || !approxEq(addQty, 0.5) {
		t.Fatalf("addQty = %v ok=%v, want 0.5 from default notional 1000/2000", addQty, ok)
	}
	// explicit AddNotionalUSD overrides the default
	sc2 := StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddNotionalUSD: 4000}}
	addQty2, ok2, _ := perpsScaleInDecision(sc2, longSnap(), 1, 2000, 1000)
	if !ok2 || !approxEq(addQty2, 2.0) {
		t.Fatalf("addQty = %v ok=%v, want 2.0 from override notional 4000/2000", addQty2, ok2)
	}
}

func TestPerpsScaleInDecisionSpacingNeedsEntryATR(t *testing.T) {
	sc := StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}}
	snap := longSnap()
	snap.EntryATR = 0 // can't evaluate spacing
	if _, ok, _ := perpsScaleInDecision(sc, snap, 1, 5000, 1000); ok {
		t.Fatalf("spacing gate should reject when EntryATR is unavailable")
	}
}
