package main

import (
	"testing"
	"time"
)

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
