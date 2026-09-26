package main

import (
	"math"
	"strings"
	"sync"
	"testing"
)

func ownSharePair() (StrategyConfig, StrategyConfig, map[string]*StrategyState, *hlCycleShare) {
	trail := 2.0
	atr := 1.5
	live := []string{"x.py", "ETH", "1h", "--mode=live"}
	a := StrategyConfig{ID: "A", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: live, TrailingStopATRMult: &trail, StopLossATRMult: &atr}
	b := StrategyConfig{ID: "B", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: live, TrailingStopATRMult: &trail, StopLossATRMult: &atr}
	states := map[string]*StrategyState{
		"A": {ID: "A", Positions: map[string]*Position{"ETH": {
			Symbol: "ETH", Side: "long", Quantity: 10, InitialQuantity: 10, AvgCost: 100, EntryATR: 5, RiskAnchorPrice: 100,
			StopLossOID: 11, StopLossTriggerPx: 90, StopLossHighWaterPx: 100,
		}}},
		"B": {ID: "B", Positions: map[string]*Position{"ETH": {
			Symbol: "ETH", Side: "long", Quantity: 5, InitialQuantity: 5, AvgCost: 100, EntryATR: 5, RiskAnchorPrice: 100,
			StopLossOID: 22, StopLossTriggerPx: 90,
		}}},
	}
	view := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 12}, NetSide: map[string]string{"ETH": "long"}}
	share := newHLCycleShare(view, hlCoinSubmitSnapshot(), nil, states, []StrategyConfig{a, b}, nil)
	return a, b, states, share
}

func TestSitesPlaceTheOwnShare(t *testing.T) {
	a, b, states, share := ownSharePair()
	var mu sync.RWMutex
	var got float64
	old := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = old })
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, size, _ float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		got = size
		return &HyperliquidStopLossUpdateResult{StopLossOID: 99, StopLossTriggerPx: 95}, "", nil
	}

	minMove := 0.1
	a.TrailingStopMinMovePct = &minMove
	a.CloseStrategy = &StrategyRef{Name: trailingTPRatchetCloseName}
	post := 0.75
	states["A"].Positions["ETH"].PostTPTrailingATRMult = &post
	states["A"].Positions["ETH"].StopLossHighWaterPx = 110
	if _, _ = runTrailingStopUpdateAfterRatchetTighten(a, states["A"], "ETH", 110, share, nil, nil, &mu, nil, newTestLogger(t)); math.Abs(got-8) > 1e-6 {
		t.Fatalf("ratchet size %g, want 8", got)
	}

	got = 0
	states["A"].Positions["ETH"].ScaleInResizePending = true
	if _, _ = scaleInResizeTrailingSLNow(a, states["A"], "ETH", 110, share, nil, nil, false, &mu, nil, newTestLogger(t)); math.Abs(got-8) > 1e-6 {
		t.Fatalf("scale-in size %g, want 8", got)
	}

	setHLActiveCycleShare(share)
	t.Cleanup(func() { setHLActiveCycleShare(nil) })
	cands := collectHLLiquidationAuditCandidates([]StrategyConfig{a, b}, &AppState{Strategies: states}, map[string]float64{"ETH": 80}, map[string]string{"ETH": "long"}, map[string]float64{"ETH": 12}, &mu)
	var aQty float64
	var aCapped bool
	var sawA bool
	for _, c := range cands {
		if c.StrategyID != "A" {
			continue
		}
		sawA = true
		aQty = c.Qty
		aCapped = c.QtyCapped
	}
	if !sawA || math.Abs(aQty-8) > 1e-6 || !aCapped {
		t.Fatalf("audit candidates %+v, want A qty 8 capped", cands)
	}
}

func TestOpenArmPlacesTheUnarmedShare(t *testing.T) {
	a, b, states, _ := ownSharePair()
	for _, id := range []string{"A", "B"} {
		states[id].Positions["ETH"].StopLossOID = 0
		states[id].Positions["ETH"].StopLossTriggerPx = 0
	}
	view := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 12}, NetSide: map[string]string{"ETH": "long"}}
	share := newHLCycleShare(view, hlCoinSubmitSnapshot(), nil, states, []StrategyConfig{a, b}, nil)
	var mu sync.RWMutex
	var got float64
	old := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = old })
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, size, _ float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		got = size
		return &HyperliquidStopLossUpdateResult{StopLossOID: 5, StopLossTriggerPx: 90}, "", nil
	}
	armTrailingStopAtOpenNow(a, states["A"], "ETH", 100, share, &mu, nil, newTestLogger(t))
	if math.Abs(got-8) > 1e-6 {
		t.Fatalf("open arm size %g, want 8", got)
	}
}

func TestProtectionSyncPlacesTheOwnShare(t *testing.T) {
	a, b, states, share := ownSharePair()
	var mu sync.RWMutex
	var got float64
	orig := syncHyperliquidProtection
	syncHyperliquidProtection = func(_ StrategyConfig, plan hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		got = plan.Size
		return &HyperliquidProtectionSyncResult{StopLossOID: 3, StopLossTriggerPx: 90}, true
	}
	t.Cleanup(func() { syncHyperliquidProtection = orig })
	runHyperliquidProtectionSync(a, states["A"], nil, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull, share)
	if math.Abs(got-8) > 1e-6 {
		t.Fatalf("protection sync size %g, want 8", got)
	}
	_ = b
}

func TestParseHLAllOpenOrdersReadsStopSize(t *testing.T) {
	raw := []byte(`{"open_orders":[{"oid":44444,"coin":"ETH","side":"A","sz":0.5,"reduce_only":true,"is_trigger":true,"order_type":"Stop Market","trigger_px":3104.12}],"sz_decimals_by_coin":{"ETH":4}}`)
	got, err := parseHLAllOpenOrders(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orders) != 1 || got.Orders[0].OID != 44444 || math.Abs(got.Orders[0].Sz-0.5) > 1e-9 || got.Decimals["ETH"] != 4 {
		t.Fatalf("parsed %+v", got)
	}
}

func TestImmediateStopFillBooksThePlacedShare(t *testing.T) {
	st := &StrategyState{ID: "A", Cash: 10000, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Side: "long", Quantity: 10, InitialQuantity: 10, AvgCost: 100, RiskAnchorPrice: 100},
	}}
	ok, px := applyTrailingStopUpdateResult(st, "ETH", "long", 0, 0, true, &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 90}, "trailing_stop_loss_immediate", nil, 8)
	if !ok || px != 90 {
		t.Fatalf("fill ok=%t px=%g", ok, px)
	}
	pos := st.Positions["ETH"]
	if pos == nil || math.Abs(pos.Quantity-2) > 1e-9 {
		t.Fatalf("book left %+v, want quantity 2", pos)
	}
}

func TestShareResize(t *testing.T) {
	resetHLShareAlerts()
	t.Cleanup(resetHLShareAlerts)
	a, b, states, share := ownSharePair()
	var mu sync.RWMutex
	state := &AppState{Strategies: states}
	var sizes []float64
	var cancels []int64
	var triggers []float64
	oldUpdate := runHyperliquidUpdateStopLossFunc
	oldCancel := runHyperliquidCancelOrderFn
	t.Cleanup(func() {
		runHyperliquidUpdateStopLossFunc = oldUpdate
		runHyperliquidCancelOrderFn = oldCancel
	})
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, size, trigger float64, cancel int64) (*HyperliquidStopLossUpdateResult, string, error) {
		sizes = append(sizes, size)
		triggers = append(triggers, trigger)
		cancels = append(cancels, cancel)
		return &HyperliquidStopLossUpdateResult{StopLossOID: 77, StopLossTriggerPx: trigger}, "", nil
	}
	runHyperliquidCancelOrderFn = func(_, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
		cancels = append(cancels, oid)
		return &HyperliquidCancelOrderResult{Cancelled: true, OID: oid}, "", nil
	}
	listed := hlAllOpenOrders{Decimals: map[string]int{"ETH": 1}, Orders: []hlListedOpenOrder{{
		OID: 11, Coin: "ETH", Sz: 10, TriggerPx: 90, ReduceOnly: true, IsTrigger: true, OrderType: "Stop Market",
	}}}
	runHyperliquidShareResize([]StrategyConfig{a, b}, state, share, listed, false, nil, &mu, nil)
	if len(sizes) != 1 || math.Abs(sizes[0]-8) > 1e-9 || cancels[0] != 11 || triggers[0] != 90 {
		t.Fatalf("resize sizes=%v cancels=%v triggers=%v, want one update at 8 cancel 11 trigger 90", sizes, cancels, triggers)
	}

	sizes, cancels = nil, nil
	states["A"].Positions["ETH"].StopLossOID = 11
	listed.Orders[0].Sz = 7.9
	listed.Decimals["ETH"] = 1
	// lot 0.1: 7.9 is one lot under 8, so no call. Use decimals 1 → step 0.1, floor(8)=8.
	runHyperliquidShareResize([]StrategyConfig{a, b}, state, share, listed, false, nil, &mu, nil)
	if len(sizes) != 0 {
		t.Fatalf("7.9 vs Q 8 lot 0.1 made %d calls, want 0", len(sizes))
	}

	listed.Orders[0].Sz = 7
	runHyperliquidShareResize([]StrategyConfig{a, b}, state, share, listed, false, nil, &mu, nil)
	if len(sizes) != 1 || math.Abs(sizes[0]-8) > 1e-9 {
		t.Fatalf("grow sizes=%v, want one call at 8", sizes)
	}

	sizes = nil
	states["A"].Positions["ETH"].StopLossOID = 11
	states["A"].Positions["ETH"].TPOIDs = []int64{31, 32}
	listed.Orders = []hlListedOpenOrder{
		{OID: 11, Coin: "ETH", Sz: 8, TriggerPx: 90},
		{OID: 31, Coin: "ETH", Sz: 6},
		{OID: 32, Coin: "ETH", Sz: 4},
	}
	runHyperliquidShareResize([]StrategyConfig{a, b}, state, share, listed, false, nil, &mu, nil)
	if !hlShareTakeForceTP("A", "ETH") {
		t.Fatal("tiers summing to 10 with Q 8 did not force-replace")
	}

	flat := newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: map[string]float64{}, NetSide: map[string]string{}}, hlCoinSubmitSnapshot(), nil, states, []StrategyConfig{a, b}, nil)
	states["A"].Positions["ETH"].StopLossOID = 11
	cancels = nil
	runHyperliquidShareResize([]StrategyConfig{a}, state, flat, hlAllOpenOrders{Orders: []hlListedOpenOrder{{OID: 11, Coin: "ETH", Sz: 10, TriggerPx: 90}}, Decimals: map[string]int{"ETH": 1}}, false, nil, &mu, nil)
	if len(cancels) != 1 || cancels[0] != 11 || states["A"].Positions["ETH"].StopLossOID != 0 {
		t.Fatalf("Q=0 cancel=%v oid=%d", cancels, states["A"].Positions["ETH"].StopLossOID)
	}

	mock := &mockNotifier{}
	mn := NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner"})
	states["A"].Positions["ETH"].StopLossOID = 11
	runHyperliquidCancelOrderFn = func(_, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
		return &HyperliquidCancelOrderResult{Cancelled: false, OID: oid, CancelError: "rejected"}, "", nil
	}
	runHyperliquidShareResize([]StrategyConfig{a}, state, flat, hlAllOpenOrders{Orders: []hlListedOpenOrder{{OID: 11, Coin: "ETH", Sz: 10}}, Decimals: map[string]int{"ETH": 1}}, false, nil, &mu, mn)
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "11") {
		t.Fatalf("refused cancel alerts=%v", mock.dms)
	}

	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, size, _ float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		sizes = append(sizes, size)
		return &HyperliquidStopLossUpdateResult{StopLossOID: 1, StopLossTriggerPx: 90}, "", nil
	}
	sizes = nil
	origPending := pendingManualActionOnSymbolFn
	pendingManualActionOnSymbolFn = func(*StateStore, string) (bool, error) { return true, nil }
	t.Cleanup(func() { pendingManualActionOnSymbolFn = origPending })
	share2 := newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 12}, NetSide: map[string]string{"ETH": "long"}}, hlCoinSubmitSnapshot(), nil, states, []StrategyConfig{a, b}, nil)
	states["A"].Positions["ETH"].StopLossOID = 11
	runHyperliquidShareResize([]StrategyConfig{a, b}, state, share2, hlAllOpenOrders{Orders: []hlListedOpenOrder{{OID: 11, Coin: "ETH", Sz: 10, TriggerPx: 90}}, Decimals: map[string]int{"ETH": 1}}, false, nil, &mu, nil)
	if len(sizes) != 0 {
		t.Fatalf("queued manual row made %d resize calls, want 0", len(sizes))
	}

	resetHLShareAlerts()
	for i := 0; i < 3; i++ {
		runHyperliquidShareResize(nil, state, share, hlAllOpenOrders{}, true, nil, &mu, mn)
	}
	found := false
	for _, dm := range mock.dms {
		if strings.Contains(dm.content, "3 consecutive") {
			found = true
		}
	}
	if !found {
		t.Fatalf("listing failures did not alert once on the third cycle: %v", mock.dms)
	}
}

func TestShareRearmAfterNettingShrinks(t *testing.T) {
	resetHLShareAlerts()
	t.Cleanup(resetHLShareAlerts)
	a, b, states, _ := ownSharePair()
	pa := states["A"].Positions["ETH"]
	pa.StopLossOID, pa.StopLossTriggerPx = 0, 0
	pb := states["B"].Positions["ETH"]
	pb.Side, pb.Quantity, pb.StopLossOID, pb.StopLossTriggerPx = "short", 10, 0, 0
	var mu sync.RWMutex
	state := &AppState{Strategies: states}
	var sizes []float64
	old := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = old })
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, size, trigger float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		sizes = append(sizes, size)
		return &HyperliquidStopLossUpdateResult{StopLossOID: 55, StopLossTriggerPx: trigger}, "", nil
	}
	prices := map[string]float64{"ETH": 100}
	netted := newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: map[string]float64{}, NetSide: map[string]string{}}, hlCoinSubmitSnapshot(), nil, states, []StrategyConfig{a, b}, nil)
	runHyperliquidShareRearm([]StrategyConfig{a, b}, state, netted, prices, nil, nil, nil, nil, &mu, nil)
	if len(sizes) != 0 {
		t.Fatalf("netted-flat coin armed %v, want nothing", sizes)
	}

	delete(states["B"].Positions, "ETH")
	back := newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 10}, NetSide: map[string]string{"ETH": "long"}}, hlCoinSubmitSnapshot(), nil, states, []StrategyConfig{a, b}, nil)
	runHyperliquidShareRearm([]StrategyConfig{a, b}, state, back, prices, nil, nil, nil, nil, &mu, nil)
	if len(sizes) != 1 || math.Abs(sizes[0]-10) > 1e-9 || pa.StopLossOID != 55 {
		t.Fatalf("after the opposite book closed sizes=%v oid=%d, want one arm at 10 and oid 55", sizes, pa.StopLossOID)
	}
	runHyperliquidShareRearm([]StrategyConfig{a, b}, state, back, prices, nil, nil, nil, nil, &mu, nil)
	if len(sizes) != 1 {
		t.Fatalf("armed book re-armed again: %v", sizes)
	}
}

func TestParseHLAllOpenOrdersNormalizesLotKeys(t *testing.T) {
	got, err := parseHLAllOpenOrders([]byte(`{"open_orders":[],"sz_decimals_by_coin":{"kPEPE":0,"BTC":5}}`))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := got.Decimals[hlCoinKey("kPEPE")]; !ok || d != 0 {
		t.Fatalf("kPEPE lot missing under %q: %v", hlCoinKey("kPEPE"), got.Decimals)
	}
	if got.Decimals["BTC"] != 5 {
		t.Fatalf("BTC lot %v", got.Decimals)
	}
}

func TestShareNettingKeepsRestingOrders(t *testing.T) {
	resetHLShareAlerts()
	t.Cleanup(resetHLShareAlerts)
	a, b, states, _ := ownSharePair()
	pb := states["B"].Positions["ETH"]
	pb.Side, pb.Quantity, pb.StopLossOID, pb.StopLossTriggerPx = "short", 10, 0, 0
	delete(states, "B")
	states["B"] = &StrategyState{ID: "B", Positions: map[string]*Position{"ETH": pb}}
	var mu sync.RWMutex
	state := &AppState{Strategies: states}
	calls := 0
	oldUpdate := runHyperliquidUpdateStopLossFunc
	oldCancel := runHyperliquidCancelOrderFn
	t.Cleanup(func() {
		runHyperliquidUpdateStopLossFunc = oldUpdate
		runHyperliquidCancelOrderFn = oldCancel
	})
	runHyperliquidUpdateStopLossFunc = func(_, _, _ string, _, _ float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{}, "", nil
	}
	runHyperliquidCancelOrderFn = func(_, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
		calls++
		return &HyperliquidCancelOrderResult{Cancelled: true, OID: oid}, "", nil
	}
	netted := newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: map[string]float64{}, NetSide: map[string]string{}}, hlCoinSubmitSnapshot(), nil, states, []StrategyConfig{a, b}, nil)
	listed := hlAllOpenOrders{Decimals: map[string]int{"ETH": 1}, Orders: []hlListedOpenOrder{{OID: 11, Coin: "ETH", Sz: 10, TriggerPx: 90}}}
	runHyperliquidShareResize([]StrategyConfig{a}, state, netted, listed, false, nil, &mu, nil)
	pa := states["A"].Positions["ETH"]
	if calls != 0 || pa.StopLossOID != 11 || pa.StopLossTriggerPx != 90 {
		t.Fatalf("netted zero share: calls=%d oid=%d trigger=%g, want the stop kept", calls, pa.StopLossOID, pa.StopLossTriggerPx)
	}
}

func TestShareRearmSyncsAnATROwner(t *testing.T) {
	resetHLShareAlerts()
	t.Cleanup(resetHLShareAlerts)
	a, b, states, _ := ownSharePair()
	a.TrailingStopATRMult = nil
	pa := states["A"].Positions["ETH"]
	pa.StopLossOID, pa.StopLossTriggerPx = 0, 0
	delete(states, "B")
	var mu sync.RWMutex
	state := &AppState{Strategies: states}
	var got []float64
	orig := syncHyperliquidProtection
	syncHyperliquidProtection = func(_ StrategyConfig, plan hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		got = append(got, plan.Size)
		return &HyperliquidProtectionSyncResult{StopLossOID: 3, StopLossTriggerPx: 92.5}, true
	}
	t.Cleanup(func() { syncHyperliquidProtection = orig })
	share := newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 10}, NetSide: map[string]string{"ETH": "long"}}, hlCoinSubmitSnapshot(), nil, states, []StrategyConfig{a, b}, nil)
	runHyperliquidShareRearm([]StrategyConfig{a}, state, share, map[string]float64{"ETH": 100}, nil, nil, nil, nil, &mu, nil)
	if len(got) != 1 || pa.StopLossOID != 3 {
		t.Fatalf("ATR owner re-arm syncs=%v oid=%d, want one sync and oid 3", got, pa.StopLossOID)
	}
	runHyperliquidShareRearm([]StrategyConfig{a}, state, share, map[string]float64{"ETH": 100}, nil, nil, nil, nil, &mu, nil)
	if len(got) != 1 {
		t.Fatalf("armed ATR owner synced again: %v", got)
	}
	stale := newHLCycleShare(hlOnChainCoinView{Known: true, AbsQty: map[string]float64{}, NetSide: map[string]string{}}, hlCoinSubmitSnapshot(), nil, states, []StrategyConfig{a, b}, nil)
	pa.StopLossOID, pa.StopLossTriggerPx = 0, 0
	runHyperliquidShareRearm([]StrategyConfig{a}, state, stale, map[string]float64{"ETH": 100}, nil, nil, nil, nil, &mu, nil)
	if len(got) != 1 {
		t.Fatalf("stale book on a flat chain synced: %v", got)
	}
}

func TestImmediateStopFillBooksTheFlooredSize(t *testing.T) {
	st := &StrategyState{ID: "A", Cash: 10000, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Side: "long", Quantity: 10, InitialQuantity: 10, AvgCost: 100, RiskAnchorPrice: 100},
	}}
	ok, _ := applyTrailingStopUpdateResult(st, "ETH", "long", 0, 0, true, &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 90, StopLossSize: 6.66}, "trailing_stop_loss_immediate", nil, 20.0/3)
	if pos := st.Positions["ETH"]; !ok || pos == nil || math.Abs(pos.Quantity-3.34) > 1e-9 {
		t.Fatalf("fill ok=%t book=%+v, want quantity 3.34", ok, pos)
	}
	if got := hlPlacedStopQty(8, 0); got != 8 {
		t.Fatalf("no reported size booked %g, want 8", got)
	}
}
