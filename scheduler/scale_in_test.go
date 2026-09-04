package main

import (
	"testing"
	"time"
)

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
	one, err := db.LifetimeTradeStatsForStrategy("hl-scalein-eth")
	if err != nil {
		t.Fatalf("LifetimeTradeStatsForStrategy: %v", err)
	}
	if one.PositionsOpened != 1 {
		t.Errorf("per-strategy PositionsOpened = %d, want 1", one.PositionsOpened)
	}
}

func TestScaleInProtectionForceReplace(t *testing.T) {
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
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Errorf("watermark mutated: %d, want 1", pos.SLAdjustedTiersProcessed)
	}
}

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

	called = false
	_, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 2.0, &Position{AvgCost: 100}, 100, 100, 97, 111, trailingReplacePolicy{}, nil, logger)
	if !ok || result != nil || called {
		t.Fatalf("without force, expected no replace (called=%v result=%+v)", called, result)
	}
	called = false
	_, result, ok = runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 2.0, &Position{AvgCost: 100}, 100, 100, 97, 111, trailingReplacePolicy{forceResize: true}, nil, logger)
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
	if !approxEq(pos.AvgCost, 2100) {
		t.Errorf("AvgCost = %v, want 2100", pos.AvgCost)
	}
	if pos.ScaleInCount != 1 {
		t.Errorf("ScaleInCount = %d, want 1", pos.ScaleInCount)
	}
	if pos.EntryATR != 50 || pos.Regime != "trending" {
		t.Errorf("frozen fields moved: EntryATR=%v Regime=%q", pos.EntryATR, pos.Regime)
	}
	if !approxEq(ss.Cash, 998.5) {
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

	flat := &StrategyState{ID: "hl-manual-eth", Type: "manual", Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	state2 := &AppState{Strategies: map[string]*StrategyState{"hl-manual-eth": flat}}
	if err := applyManualAction(state2, nil, scByID, add); err == nil {
		t.Errorf("expected error adding to a flat strategy")
	}
}

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

	if !approxEq(pos.AvgCost, 2100) {
		t.Fatalf("AvgCost = %v, want 2100", pos.AvgCost)
	}
	if !approxEq(pos.Quantity, 200) {
		t.Fatalf("Quantity = %v, want 200", pos.Quantity)
	}
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
	applyScaleIn(pos, 10, 90)
	applyScaleIn(pos, 10, 110)
	if pos.ScaleInCount != 2 {
		t.Fatalf("ScaleInCount = %d, want 2", pos.ScaleInCount)
	}
	if !approxEq(pos.Quantity, 30) || !approxEq(pos.InitialQuantity, 30) {
		t.Fatalf("Quantity/InitialQuantity = %v/%v, want 30/30", pos.Quantity, pos.InitialQuantity)
	}
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

func TestApplyScaleInStampsFrozenRiskAnchor(t *testing.T) {
	pos := &Position{Side: "long", Quantity: 100, InitialQuantity: 100, AvgCost: 2000}
	applyScaleIn(pos, 100, 2200)
	if !approxEq(pos.RiskAnchorPrice, 2000) {
		t.Fatalf("RiskAnchorPrice = %v, want 2000 (original entry frozen)", pos.RiskAnchorPrice)
	}
	if !approxEq(pos.AvgCost, 2100) {
		t.Fatalf("AvgCost = %v, want 2100 (blended for PnL)", pos.AvgCost)
	}
	applyScaleIn(pos, 200, 2400)
	if !approxEq(pos.RiskAnchorPrice, 2000) {
		t.Fatalf("RiskAnchorPrice moved on second add: %v, want 2000", pos.RiskAnchorPrice)
	}
	if !approxEq(pos.riskAnchorPrice(), 2000) {
		t.Fatalf("riskAnchorPrice() = %v, want 2000", pos.riskAnchorPrice())
	}
}

func TestRiskAnchorPriceFallsBackToAvgCost(t *testing.T) {
	pos := &Position{AvgCost: 1500}
	if !approxEq(pos.riskAnchorPrice(), 1500) {
		t.Fatalf("riskAnchorPrice() = %v, want 1500 (fallback to AvgCost)", pos.riskAnchorPrice())
	}
}

func TestProtectionPlanFreezesTriggersAtRiskAnchor(t *testing.T) {
	mult := 1.5
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 200, InitialQuantity: 200,
		AvgCost: 2100, RiskAnchorPrice: 2000, EntryATR: 50, StopLossATRMult: &mult,
	}
	sc := StrategyConfig{Type: "perps", Platform: "hyperliquid", StopLossATRMult: &mult}
	plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
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

func TestPerpsScaleInDecision(t *testing.T) {
	shortSnap := func() scaleInSnapshot {
		return scaleInSnapshot{Side: "short", Quantity: 100, AvgCost: 2000, EntryATR: 50, LastAddPrice: 2000}
	}
	withSnap := func(mut func(*scaleInSnapshot)) scaleInSnapshot {
		s := longSnap()
		mut(&s)
		return s
	}

	cases := []struct {
		name     string
		sc       StrategyConfig
		snap     scaleInSnapshot
		signal   int
		price    float64
		notional float64
		wantOK   bool
		wantQty  *float64
	}{
		{name: "opt-in required", sc: StrategyConfig{AllowScaleIn: false}, snap: longSnap(), signal: 1, price: 2000, notional: 1000},

		{name: "buy on long adds", sc: StrategyConfig{AllowScaleIn: true}, snap: longSnap(), signal: 1, price: 2000, notional: 1000, wantOK: true},
		{name: "sell on long does not add", sc: StrategyConfig{AllowScaleIn: true}, snap: longSnap(), signal: -1, price: 2000, notional: 1000},
		{name: "buy on short does not add", sc: StrategyConfig{AllowScaleIn: true}, snap: shortSnap(), signal: 1, price: 2000, notional: 1000},
		{name: "sell on short adds", sc: StrategyConfig{AllowScaleIn: true}, snap: shortSnap(), signal: -1, price: 2000, notional: 1000, wantOK: true},
		{name: "add from flat is rejected", sc: StrategyConfig{AllowScaleIn: true}, snap: scaleInSnapshot{Side: "", Quantity: 0}, signal: 1, price: 2000, notional: 1000},

		{name: "at max_adds", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{MaxAdds: 2}},
			snap: withSnap(func(s *scaleInSnapshot) { s.ScaleInCount = 2 }), signal: 1, price: 2000, notional: 1000},
		{name: "under max_adds", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{MaxAdds: 2}},
			snap: withSnap(func(s *scaleInSnapshot) { s.ScaleInCount = 1 }), signal: 1, price: 2000, notional: 1000, wantOK: true},

		{name: "past max_added_notional", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{MaxAddedNotionalUSD: 1500}},
			snap: withSnap(func(s *scaleInSnapshot) { s.AddedNotionalUSD = 1000 }), signal: 1, price: 2000, notional: 1000},
		{name: "under max_added_notional", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{MaxAddedNotionalUSD: 1500}},
			snap: withSnap(func(s *scaleInSnapshot) { s.AddedNotionalUSD = 1000 }), signal: 1, price: 2000, notional: 400, wantOK: true},

		{name: "long add-to-winners before the spacing distance", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}},
			snap: longSnap(), signal: 1, price: 2049, notional: 1000},
		{name: "long add-to-winners past the spacing distance", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}},
			snap: longSnap(), signal: 1, price: 2051, notional: 1000, wantOK: true},
		{name: "long add-to-winners on an adverse move", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}},
			snap: longSnap(), signal: 1, price: 1900, notional: 1000},

		{name: "long average-down before the adverse distance", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: -1.0}},
			snap: longSnap(), signal: 1, price: 1951, notional: 1000},
		{name: "long average-down past the adverse distance", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: -1.0}},
			snap: longSnap(), signal: 1, price: 1949, notional: 1000, wantOK: true},
		{name: "long average-down on a favorable move", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: -1.0}},
			snap: longSnap(), signal: 1, price: 2100, notional: 1000},

		{name: "short add-to-winners on a favorable (down) move", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}},
			snap: shortSnap(), signal: -1, price: 1949, notional: 1000, wantOK: true},
		{name: "short add-to-winners on an adverse (up) move", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}},
			snap: shortSnap(), signal: -1, price: 2100, notional: 1000},

		{name: "zero spacing does not gate", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 0}},
			snap: longSnap(), signal: 1, price: 2000, notional: 1000, wantOK: true},
		{name: "spacing measures from AvgCost when LastAddPrice is unset", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}},
			snap: withSnap(func(s *scaleInSnapshot) { s.LastAddPrice = 0 }), signal: 1, price: 2051, notional: 1000, wantOK: true},
		{name: "spacing gate rejects a missing EntryATR", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddSpacingATR: 1.0}},
			snap: withSnap(func(s *scaleInSnapshot) { s.EntryATR = 0 }), signal: 1, price: 5000, notional: 1000},

		{name: "default add notional sizes the leg", sc: StrategyConfig{AllowScaleIn: true},
			snap: longSnap(), signal: 1, price: 2000, notional: 1000, wantOK: true, wantQty: scaleInQtyPtr(0.5)},
		{name: "override add notional sizes the leg", sc: StrategyConfig{AllowScaleIn: true, ScaleIn: &ScaleInConfig{AddNotionalUSD: 4000}},
			snap: longSnap(), signal: 1, price: 2000, notional: 1000, wantOK: true, wantQty: scaleInQtyPtr(2.0)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addQty, ok, reason := perpsScaleInDecision(tc.sc, tc.snap, tc.signal, tc.price, tc.notional)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (reason=%q)", ok, tc.wantOK, reason)
			}
			if tc.wantQty != nil && !approxEq(addQty, *tc.wantQty) {
				t.Fatalf("addQty = %v, want %v", addQty, *tc.wantQty)
			}
		})
	}
}

func scaleInQtyPtr(v float64) *float64 { return &v }
