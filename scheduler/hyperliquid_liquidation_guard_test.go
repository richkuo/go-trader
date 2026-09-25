package main

import (
	"sort"

	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

var errLiqAuditStub = errors.New("simulated stop-loss subprocess failure")

func approxEqLiq(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestStopPastLiquidationDirections(t *testing.T) {
	cases := []struct {
		name      string
		side      string
		triggerPx float64
		liqPx     float64
		want      bool
	}{
		{"long past", "long", 2325, 2340.5, true},
		{"long exactly at liquidation", "long", 2340.5, 2340.5, true},
		{"long safely inside", "long", 2360, 2340.5, false},
		{"short past", "short", 2460, 2440, true},
		{"short exactly at liquidation", "short", 2440, 2440, true},
		{"short safely inside", "short", 2420, 2440, false},
		{"unknown liquidation", "long", 2325, 0, false},
		{"no stop armed", "long", 0, 2340.5, false},
		{"negative liquidation", "long", 2325, -1, false},
		{"unknown side", "flat", 2325, 2340.5, false},
	}
	for _, c := range cases {
		if got := stopPastLiquidation(c.side, c.triggerPx, c.liqPx); got != c.want {
			t.Errorf("%s: stopPastLiquidation(%q, %g, %g) = %v, want %v", c.name, c.side, c.triggerPx, c.liqPx, got, c.want)
		}
	}
}

func TestClampStopInsideLiquidationTightensOnly(t *testing.T) {
	buf := hlLiquidationStopBufferPct / 100.0

	got, ok := clampStopInsideLiquidation("long", 2325, 2340.5)
	if !ok {
		t.Fatal("long past-liquidation trigger must clamp")
	}
	if want := 2340.5 * (1 + buf); !approxEqLiq(got, want) {
		t.Errorf("long clamp = %g, want %g", got, want)
	}
	if got <= 2340.5 {
		t.Errorf("long clamp %g must sit strictly INSIDE liquidation 2340.5", got)
	}
	if got <= 2325 {
		t.Errorf("long clamp %g must be TIGHTER than the original 2325 (one-way tighten)", got)
	}

	got, ok = clampStopInsideLiquidation("short", 2460, 2440)
	if !ok {
		t.Fatal("short past-liquidation trigger must clamp")
	}
	if want := 2440 * (1 - buf); !approxEqLiq(got, want) {
		t.Errorf("short clamp = %g, want %g", got, want)
	}
	if got >= 2440 {
		t.Errorf("short clamp %g must sit strictly INSIDE liquidation 2440", got)
	}
	if got >= 2460 {
		t.Errorf("short clamp %g must be TIGHTER than the original 2460 (one-way tighten)", got)
	}
}

func TestClampStopInsideLiquidationPassthroughAndNeverZero(t *testing.T) {
	cases := []struct {
		name      string
		side      string
		triggerPx float64
		liqPx     float64
	}{
		{"already reachable long", "long", 2360, 2340.5},
		{"already reachable short", "short", 2420, 2440},
		{"unknown liquidation", "long", 2325, 0},
		{"nothing armed", "long", 0, 2340.5},
		{"unknown side", "flat", 2325, 2340.5},
	}
	for _, c := range cases {
		got, ok := clampStopInsideLiquidation(c.side, c.triggerPx, c.liqPx)
		if ok {
			t.Errorf("%s: expected no clamp", c.name)
		}
		if got != c.triggerPx {
			t.Errorf("%s: passthrough = %g, want the input %g unchanged", c.name, got, c.triggerPx)
		}
	}

	for _, side := range []string{"long", "short"} {
		for _, liq := range []float64{1e-6, 0.35, 42000, 1e9} {
			for _, trig := range []float64{1e-6, 0.30, 41000, 1e9} {
				got, _ := clampStopInsideLiquidation(side, trig, liq)
				if got <= 0 {
					t.Fatalf("clampStopInsideLiquidation(%q, %g, %g) = %g — a clamp must never return a non-positive trigger", side, trig, liq, got)
				}
			}
		}
	}
}

func TestHLClampProtectionSLMultRewritesMultiple(t *testing.T) {
	anchor, atr, liq := 2400.0, 30.0, 2340.5
	newMult, ok := hlClampProtectionSLMult("long", anchor, atr, 2.5, liq)
	if !ok {
		t.Fatal("expected the past-liquidation multiple to be clamped")
	}
	wantTrigger := liq * (1 + hlLiquidationStopBufferPct/100.0)
	if gotTrigger := anchor - newMult*atr; !approxEqLiq(gotTrigger, wantTrigger) {
		t.Errorf("derived trigger = %g, want %g", gotTrigger, wantTrigger)
	}
	if newMult >= 2.5 {
		t.Errorf("clamped multiple %g must be SMALLER than 2.5 (tighter stop)", newMult)
	}

	liq = 2460.0
	newMult, ok = hlClampProtectionSLMult("short", anchor, atr, 2.5, liq)
	if !ok {
		t.Fatal("expected the short past-liquidation multiple to be clamped")
	}
	wantTrigger = liq * (1 - hlLiquidationStopBufferPct/100.0)
	if gotTrigger := anchor + newMult*atr; !approxEqLiq(gotTrigger, wantTrigger) {
		t.Errorf("short derived trigger = %g, want %g", gotTrigger, wantTrigger)
	}
}

func TestHLLiquidationCoinBookConsistent(t *testing.T) {
	got := hlLiquidationCoinBookConsistent(
		map[string]float64{"ETH": 2.0, "BTC": 1.0, "SOL": 0.5, "AVAX": 3.0, "DOGE": 5.0, "MIX": -0.6, "PHANTOM": 1.1},
		map[string]int{"ETH": 2, "BTC": 2, "SOL": 1, "AVAX": 1, "DOGE": 1, "MIX": 2, "PHANTOM": 3},
		map[string]float64{"ETH": 2.0, "BTC": 0.4, "AVAX": 2.0, "MIX": 0.6, "PHANTOM": 0.6},
	)
	if !got["ETH"] {
		t.Error("ETH: recorded size equals on-chain size — consistent")
	}
	if got["BTC"] {
		t.Error("BTC: recorded 1.0 across TWO owners exceeds on-chain 0.4 — a phantom position")
	}
	if got["SOL"] {
		t.Error("SOL: absent from the snapshot — nothing on-chain backs the recorded size")
	}
	if !got["MIX"] {
		t.Error("MIX: signed net -0.6 vs on-chain 0.6 across two owners — consistent")
	}
	if got["PHANTOM"] {
		t.Error("PHANTOM: signed net 1.1 exceeds on-chain 0.6 — real drift")
	}
	if !got["AVAX"] {
		t.Error("AVAX: sole owner with virtual > on-chain — must stay actionable, sized to on-chain")
	}
	if got["DOGE"] {
		t.Error("DOGE: sole owner but absent from the snapshot — must still refuse")
	}
}

func TestCollectHLLiquidationAuditCandidatesHealsSoleOwnerDrift(t *testing.T) {
	strategies, state := liqAuditFixture(t, true, 3.0)
	cands := collectHLLiquidationAuditCandidates(
		strategies, state,
		map[string]float64{"ETH": 2340.5},
		hlNetSideByCoinAllLong(),
		map[string]float64{"ETH": 0.6},
		&sync.RWMutex{},
	)
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	if !cands[0].BookConsistent {
		t.Fatal("sole owner with virtual > on-chain must stay actionable")
	}
	if math.Abs(cands[0].Qty-0.6) > 1e-9 {
		t.Errorf("Qty = %v, want the on-chain cap 0.6 (#621)", cands[0].Qty)
	}
	if !cands[0].QtyCapped || math.Abs(cands[0].VirtualQty-1.0) > 1e-9 {
		t.Errorf("QtyCapped/VirtualQty = %v/%v, want true/1.0", cands[0].QtyCapped, cands[0].VirtualQty)
	}
	acts := planHyperliquidLiquidationAudit(cands)
	if len(acts) != 1 || acts[0].Kind != hlAuditTighten {
		t.Fatalf("actions = %+v, want one tighten", acts)
	}

	peer := strategies[0]
	peer.ID = "hl-eth-peer"
	state.Strategies["hl-eth-peer"] = &StrategyState{
		ID: "hl-eth-peer", Platform: "hyperliquid", Type: "perps",
		Positions: map[string]*Position{"ETH": {
			Symbol: "ETH", Side: "long", Quantity: 1.0,
			AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
			StopLossOID: 99, StopLossTriggerPx: 2325,
		}},
	}
	shared := collectHLLiquidationAuditCandidates(
		append(strategies, peer), state,
		map[string]float64{"ETH": 2340.5},
		hlNetSideByCoinAllLong(),
		map[string]float64{"ETH": 0.6},
		&sync.RWMutex{},
	)
	for _, c := range shared {
		if c.BookConsistent {
			t.Errorf("%s: a shared coin with a phantom must refuse", c.StrategyID)
		}
	}
}

func liqAuditFixture(t *testing.T, live bool, stopPct float64) ([]StrategyConfig, *AppState) {
	t.Helper()
	mode := "--mode=live"
	if !live {
		mode = "--mode=paper"
	}
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:        []string{"x.py", "ETH", "1h", mode},
		StopLossPct: floatPtr(stopPct),
		Leverage:    3,
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Side: "long", Quantity: 1.0,
				AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
				StopLossOID: 4242, StopLossTriggerPx: 2325,
			},
		}},
	}}
	return []StrategyConfig{sc}, state
}

func TestRunHyperliquidLiquidationAuditKeepsOriginalStopOnFailure(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex

	for _, failure := range []*HyperliquidStopLossUpdateResult{
		nil,
		{Error: "boom"},
		{CancelStopLossError: "cancel rejected"},
		{OpenOrderCheckError: "open order lookup failed"},
		{StopLossFilledExternally: true},
		{StopLossError: "open order cap"},
	} {
		f := failure
		state.Strategies["hl-eth"].Positions["ETH"].StopLossOID = 4242
		state.Strategies["hl-eth"].Positions["ETH"].StopLossTriggerPx = 2325
		runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			if f == nil {
				return nil, "", errLiqAuditStub
			}
			return f, "", nil
		}
		runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
		pos := state.Strategies["hl-eth"].Positions["ETH"]
		if pos.StopLossOID != 4242 || !approxEqLiq(pos.StopLossTriggerPx, 2325) {
			t.Fatalf("failure %+v: position must keep its ORIGINAL armed stop, got oid=%d trigger=%g", f, pos.StopLossOID, pos.StopLossTriggerPx)
		}
	}
}

func hlNetSideByCoinAllLong() map[string]string {
	return map[string]string{"ETH": "long"}
}

func TestRunHyperliquidLiquidationAuditCancelledThenRejectedRearms(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	liq := map[string]float64{"ETH": 2340.5}
	onChain := map[string]float64{"ETH": 1.0}

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded: true,
			StopLossError:           "Order would exceed the open order limit",
			StopLossTriggerPx:       triggerPx,
		}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, liq, hlNetSideByCoinAllLong(), onChain, true, &mu, nil, time.Now().UTC())

	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Fatalf("state must record that the stop is GONE, got oid=%d trigger=%g", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	var gotTrigger float64
	var gotCancelOID int64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotTrigger, gotCancelOID = triggerPx, cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7777, StopLossTriggerPx: triggerPx}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, liq, hlNetSideByCoinAllLong(), onChain, true, &mu, nil, time.Now().UTC())

	if gotCancelOID != 0 {
		t.Errorf("a re-arm has nothing to cancel, got cancel OID %d", gotCancelOID)
	}
	wantTrigger := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(gotTrigger, wantTrigger) {
		t.Errorf("re-arm trigger = %g, want %g (the scalar distance, clamped inside liquidation)", gotTrigger, wantTrigger)
	}
	if pos.StopLossOID != 7777 {
		t.Errorf("position must end the cycle armed, got oid=%d", pos.StopLossOID)
	}
}

func TestCollectHLLiquidationAuditCandidatesSideMismatchSkipsOppositeLeg(t *testing.T) {
	strategies, state := liqAuditFixture(t, true, 3.125)
	shortPeer := strategies[0]
	shortPeer.ID = "hl-eth-short"
	state.Strategies["hl-eth-short"] = &StrategyState{
		ID: "hl-eth-short", Platform: "hyperliquid", Type: "perps",
		Positions: map[string]*Position{"ETH": {
			Symbol: "ETH", Side: "short", Quantity: 0.4,
			AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
			StopLossOID: 88, StopLossTriggerPx: 2460,
		}},
	}
	cands := collectHLLiquidationAuditCandidates(
		append(strategies, shortPeer), state,
		map[string]float64{"ETH": 2340.5},
		map[string]string{"ETH": "long"},
		map[string]float64{"ETH": 0.6},
		&sync.RWMutex{},
	)
	var actedFor []string
	for _, c := range cands {
		if c.StrategyID == "hl-eth-short" && c.LiquidationPx != 0 {
			t.Errorf("short peer read a liquidation price %g that describes the opposite net leg", c.LiquidationPx)
		}
		if c.StrategyID == "hl-eth" && !c.BookConsistent {
			t.Errorf("a legal long+short book nets to the reported 0.6 — must NOT read as a phantom (#1456 review round 8)")
		}
	}
	for _, a := range planHyperliquidLiquidationAudit(cands) {
		actedFor = append(actedFor, a.Candidate.StrategyID)
		if a.Candidate.StrategyID == "hl-eth-short" {
			t.Errorf("the short peer's healthy stop must not be clamped against the net's liquidation price")
		}
		if a.Kind == hlAuditRefuse {
			t.Errorf("%s: a healthy bidirectional book must be tightened, not refused", a.Candidate.StrategyID)
		}
	}
	sort.Strings(actedFor)
	if len(actedFor) != 1 || actedFor[0] != "hl-eth" {
		t.Errorf("actions ran for %v, want only the net-matching long owner [hl-eth]", actedFor)
	}

	stalePeer := strategies[0]
	stalePeer.ID = "hl-eth-stale"
	state.Strategies["hl-eth-stale"] = &StrategyState{
		ID: "hl-eth-stale", Platform: "hyperliquid", Type: "perps",
		Positions: map[string]*Position{"ETH": {
			Symbol: "ETH", Side: "long", Quantity: 0.5,
			AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
			StopLossOID: 77, StopLossTriggerPx: 2325,
		}},
	}
	phantom := collectHLLiquidationAuditCandidates(
		append(append(strategies, shortPeer), stalePeer), state,
		map[string]float64{"ETH": 2340.5},
		map[string]string{"ETH": "long"},
		map[string]float64{"ETH": 0.6},
		&sync.RWMutex{},
	)
	for _, c := range phantom {
		if c.BookConsistent {
			t.Errorf("%s: a phantom same-side peer is real drift and must refuse", c.StrategyID)
		}
	}
}

func TestProtectionSyncSideMismatchNeverForcesPastLiquidationReplace(t *testing.T) {
	mult := 2.0
	sc := StrategyConfig{
		ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid",
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
		StopLossATRMult: &mult,
	}
	newState := func() *StrategyState {
		return &StrategyState{
			ID: "hl-manual-eth", Platform: "hyperliquid", Type: "manual",
			Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, RiskAnchorPrice: 3000, EntryATR: 100,
					Side: "long", StopLossOID: 55, StopLossTriggerPx: 2790},
			},
		}
	}
	liq := map[string]float64{"ETH": 2800}
	var mu sync.RWMutex

	for _, tc := range []struct {
		name       string
		net        map[string]string
		wantForce  bool
		wantMultLo float64
		wantMultHi float64
	}{
		{"side matches the net — the heal applies", map[string]string{"ETH": "long"}, true, 0, 2.0},
		{"side disagrees with the net — pre-#1450 behavior", map[string]string{"ETH": "short"}, false, 2.0, 2.0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := newState()
			var gotForce bool
			var gotMult float64
			withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, plan hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
				gotForce = plan.ForceSLReplace
				gotMult = plan.StopLossATRMult
				return &HyperliquidProtectionSyncResult{StopLossOID: 55}, true
			})
			runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, liq, tc.net, hlProtectionGuardFull)
			if gotForce != tc.wantForce {
				t.Errorf("ForceSLReplace = %v, want %v", gotForce, tc.wantForce)
			}
			if gotMult < tc.wantMultLo || gotMult > tc.wantMultHi {
				t.Errorf("plan slMult = %g, want within [%g, %g]", gotMult, tc.wantMultLo, tc.wantMultHi)
			}
		})
	}
}

func liqTrailingAuditFixture(t *testing.T) ([]StrategyConfig, *AppState) {
	t.Helper()
	trail := 3.0
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:            []string{"x.py", "ETH", "1h", "--mode=live"},
		TrailingStopPct: &trail, Leverage: 3,
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Side: "long", Quantity: 1.0,
				AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30,
				StopLossOID: 4242, StopLossTriggerPx: 2325,
			},
		}},
	}}
	return []StrategyConfig{sc}, state
}

func TestFlushOffCycleLiquidationAuditState(t *testing.T) {
	newState := func() *AppState {
		return &AppState{
			Strategies: map[string]*StrategyState{
				"hl-live": {
					ID:              "hl-live",
					Platform:        "hyperliquid",
					Type:            "perps",
					Cash:            1234.5,
					InitialCapital:  1000,
					Positions:       map[string]*Position{},
					OptionPositions: map[string]*OptionPosition{},
					TradeHistory: []Trade{{
						Symbol:      "ETH",
						Timestamp:   time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC),
						Side:        "sell",
						Quantity:    1,
						Price:       1900,
						IsClose:     true,
						RealizedPnL: -100,
						TradeType:   "stop_loss",
						PositionID:  "pos-1",
						ExchangeFee: 0.1,
						StrategyID:  "hl-live",
					}},
				},
			},
		}
	}
	cfg := &Config{Strategies: []StrategyConfig{{ID: "hl-live", Platform: "hyperliquid", Type: "perps"}}}
	var mu sync.RWMutex

	t.Run("booked close is persisted before the branch sleeps", func(t *testing.T) {
		db := openTestDB(t)
		store := openTestStore(t, db)
		state := newState()
		dirty := flushOffCycleLiquidationAuditState(state, cfg, store, &mu, 1, false, false)
		if dirty || store.saveFailures(livePartition) != 0 {
			t.Fatalf("dirty=%v failures=%d, want false/0", dirty, store.saveFailures(livePartition))
		}
		loaded, err := LoadStateWithDB(cfg, db)
		if err != nil {
			t.Fatalf("LoadStateWithDB: %v", err)
		}
		ss := loaded.Strategies["hl-live"]
		if ss == nil || len(ss.TradeHistory) != 1 || !ss.TradeHistory[0].IsClose {
			t.Fatalf("reloaded trades = %+v, want the booked close", ss)
		}
		if ss.TradeHistory[0].Price != 1900 || ss.TradeHistory[0].TradeType != "stop_loss" {
			t.Errorf("reloaded close = price %.2f type %q, want 1900 / stop_loss", ss.TradeHistory[0].Price, ss.TradeHistory[0].TradeType)
		}
	})

	t.Run("nothing changed and nothing pending writes nothing", func(t *testing.T) {
		db := openTestDB(t)
		store := openTestStore(t, db)
		store.recordSaveOutcome(livePartition, errors.New("earlier failure"))
		store.recordSaveOutcome(livePartition, errors.New("earlier failure"))
		state := newState()
		dirty := flushOffCycleLiquidationAuditState(state, cfg, store, &mu, 0, false, false)
		if dirty {
			t.Errorf("dirty = true, want false")
		}
		if got := store.saveFailures(livePartition); got != 2 {
			t.Errorf("failures = %d, want 2 (untouched — no save attempted)", got)
		}
		loaded, err := LoadStateWithDB(cfg, db)
		if err != nil {
			t.Fatalf("LoadStateWithDB: %v", err)
		}
		if ss := loaded.Strategies["hl-live"]; ss != nil && len(ss.TradeHistory) != 0 {
			t.Errorf("wrote %d trade(s) on a no-op pass, want 0", len(ss.TradeHistory))
		}
	})

	t.Run("save failure counts and latches for retry", func(t *testing.T) {
		db := openTestDB(t)
		store := openTestStore(t, db)
		state := newState()
		db.Close()
		dirty := flushOffCycleLiquidationAuditState(state, cfg, store, &mu, 1, false, false)
		if !dirty {
			t.Errorf("dirty = false after a failed save, want true (close still only in memory)")
		}
		if got := store.saveFailures(livePartition); got != 1 {
			t.Errorf("live failures = %d, want 1 (reported like the end-of-cycle save failure)", got)
		}
		if !store.persistenceHoldsPartition(livePartition) {
			t.Errorf("persistence hold = false after one failed save, want true")
		}
	})

	t.Run("a latched failure retries on a pass that books nothing", func(t *testing.T) {
		db := openTestDB(t)
		store := openTestStore(t, db)
		store.recordSaveOutcome(livePartition, errors.New("earlier failure"))
		state := newState()
		dirty := flushOffCycleLiquidationAuditState(state, cfg, store, &mu, 0, true, false)
		if dirty || store.saveFailures(livePartition) != 0 {
			t.Fatalf("dirty=%v failures=%d, want false/0 after the retry succeeded", dirty, store.saveFailures(livePartition))
		}
		if store.persistenceHoldsPartition(livePartition) {
			t.Errorf("persistence hold still set after a successful retry, want cleared")
		}
		loaded, err := LoadStateWithDB(cfg, db)
		if err != nil {
			t.Fatalf("LoadStateWithDB: %v", err)
		}
		if ss := loaded.Strategies["hl-live"]; ss == nil || len(ss.TradeHistory) != 1 {
			t.Fatalf("retry did not persist the carried-over close: %+v", ss)
		}
	})
}

func TestAuditTightenOutcomeUnknownRecordsTriggerAndStops(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		if cancelOID != 4242 {
			t.Errorf("tighten must cancel the recorded stop, got cancelOID=%d", cancelOID)
		}
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded: true,
			StopLossOutcomeUnknown:  true,
			StopLossTriggerPx:       triggerPx,
		}, "", nil
	}

	runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{"ETH": 2340.5},
		hlNetSideByCoinAllLong(),
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if calls != 1 {
		t.Fatalf("first-pass placement calls = %d, want 1", calls)
	}
	pos := state.Strategies["hl-eth"].Positions["ETH"]
	wantTrigger := 2340.5 * 1.005
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != wantTrigger {
		t.Fatalf("state = oid=%d trigger=%.4f, want oid=0 trigger=%.4f (requested trigger, dead OID unrecorded)", pos.StopLossOID, pos.StopLossTriggerPx, wantTrigger)
	}

	runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{"ETH": 2340.5},
		hlNetSideByCoinAllLong(),
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if calls != 1 {
		t.Errorf("second-pass placement calls = %d total, want still 1 (no stacking)", calls)
	}
	if last := lastLiqAlertAction("hl-eth", "ETH"); last != hlLiquidationActionPlacementUnknown {
		t.Errorf("second-cycle alert action = %q, want %q", last, hlLiquidationActionPlacementUnknown)
	}
}

func TestApplyAuditStopUpdateBooksPartialFillQty(t *testing.T) {
	ss := &StrategyState{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Side: "long", Quantity: 1.0, AvgCost: 2000},
	}}
	pos := ss.Positions["ETH"]
	pos.StopLossOID = 555
	pos.StopLossTriggerPx = 1900

	immediate, fillPx := applyAuditStopUpdate(&hlLiquidationAuditResult{}, ss, "ETH", "long", 555, 0.6,
		&HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 1900}, nil)
	if !immediate || fillPx != 1900 {
		t.Fatalf("immediate=%v fillPx=%.4f, want true/1900", immediate, fillPx)
	}
	if pos.Quantity != 0.4 {
		t.Fatalf("residual quantity = %.6f, want 0.4 (only the filled portion booked)", pos.Quantity)
	}
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Errorf("residue protection = oid=%d trigger=%.4f, want cleared (the fired order protects nothing)", pos.StopLossOID, pos.StopLossTriggerPx)
	}
	if len(ss.TradeHistory) != 1 || ss.TradeHistory[0].Quantity != 0.6 {
		t.Errorf("booked trades = %+v, want one close of 0.6", ss.TradeHistory)
	}

	ss2 := &StrategyState{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Side: "long", Quantity: 1.0, AvgCost: 2000},
	}}
	immediate2, _ := applyAuditStopUpdate(&hlLiquidationAuditResult{}, ss2, "ETH", "long", 0, 1.0,
		&HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 1900}, nil)
	if !immediate2 {
		t.Fatal("full-quantity fill not booked")
	}
	if _, ok := ss2.Positions["ETH"]; ok {
		t.Error("position should be deleted on a full-quantity close")
	}
}

func liqAuditOnChainOne() map[string]float64 { return map[string]float64{"ETH": 1.0} }

func liqAuditLiqETH() map[string]float64 { return map[string]float64{"ETH": 2340.5} }

func liqAuditClampedTrigger() float64 { return 2340.5 * (1 + hlLiquidationStopBufferPct/100.0) }

func stubLiqAuditStopLoss(t *testing.T, fn func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error)) {
	t.Helper()
	old := runHyperliquidUpdateStopLossFunc
	runHyperliquidUpdateStopLossFunc = fn
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = old })
}

func TestPlanHyperliquidLiquidationAudit(t *testing.T) {
	t.Run("classification sorts deterministically and tightens every past-liquidation owner", func(t *testing.T) {
		cands := []hlLiquidationAuditCandidate{
			{StrategyID: "b-scalar", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 11, StopLossTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: true, BookConsistent: true},
			{StrategyID: "a-trailing", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 12, StopLossTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: false, BookConsistent: true},
			{StrategyID: "c-ok", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 13, StopLossTriggerPx: 2360, LiquidationPx: 2340.5, StaticScalarOwner: true, BookConsistent: true},
			{StrategyID: "d-unknown", Symbol: "BTC", Side: "long", Qty: 1, StopLossOID: 14, StopLossTriggerPx: 100, LiquidationPx: 0, StaticScalarOwner: true, BookConsistent: true},
			{StrategyID: "e-nostop", Symbol: "BTC", Side: "long", Qty: 1, Unprotected: true, RearmTriggerPx: 39000, LiquidationPx: 40000, StaticScalarOwner: false, BookConsistent: true},
			{StrategyID: "f-noqty", Symbol: "BTC", Side: "long", Qty: 0, StopLossOID: 15, StopLossTriggerPx: 39000, LiquidationPx: 40000, StaticScalarOwner: true, BookConsistent: true},
		}
		actions := planHyperliquidLiquidationAudit(cands)
		if len(actions) != 2 {
			t.Fatalf("actions = %d, want 2 (only the two past-liquidation candidates)", len(actions))
		}
		if actions[0].Candidate.StrategyID != "a-trailing" || actions[1].Candidate.StrategyID != "b-scalar" {
			t.Fatalf("actions not sorted deterministically: %s, %s", actions[0].Candidate.StrategyID, actions[1].Candidate.StrategyID)
		}
		for _, a := range actions {
			if a.Kind != hlAuditTighten {
				t.Errorf("%s: kind = %v, want a tighten job — the audit heals every owner", a.Candidate.StrategyID, a.Kind)
			}
		}
		if !approxEqLiq(actions[1].ClampedTriggerPx, liqAuditClampedTrigger()) {
			t.Errorf("clamped trigger = %g, want %g", actions[1].ClampedTriggerPx, liqAuditClampedTrigger())
		}
	})

	t.Run("unprotected static-scalar owner is re-armed at the configured distance", func(t *testing.T) {
		cands := []hlLiquidationAuditCandidate{
			{StrategyID: "hl-eth", Symbol: "ETH", Side: "long", Qty: 1, Unprotected: true, RearmTriggerPx: 2325, LiquidationPx: 2300, StaticScalarOwner: true, BookConsistent: true},
		}
		actions := planHyperliquidLiquidationAudit(cands)
		if len(actions) != 1 || actions[0].Kind != hlAuditRearm {
			t.Fatalf("actions = %+v, want one re-arm job", actions)
		}
		if !approxEqLiq(actions[0].ClampedTriggerPx, 2325) {
			t.Errorf("re-arm trigger = %g, want 2325", actions[0].ClampedTriggerPx)
		}
	})

	t.Run("unreconciled coin refuses both tighten and re-arm", func(t *testing.T) {
		cands := []hlLiquidationAuditCandidate{
			{StrategyID: "hl-eth", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 11, StopLossTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: true, BookConsistent: false},
			{StrategyID: "hl-eth2", Symbol: "ETH", Side: "long", Qty: 1, Unprotected: true, RearmTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: true, BookConsistent: false},
		}
		for _, a := range planHyperliquidLiquidationAudit(cands) {
			if a.Kind != hlAuditRefuse {
				t.Errorf("%s: kind = %v, want a refusal on an unreconciled coin", a.Candidate.StrategyID, a.Kind)
			}
		}
	})
}

func TestRunHyperliquidLiquidationAuditNoOpCases(t *testing.T) {
	zeroStop := func(strategies []StrategyConfig, state *AppState) []StrategyConfig {
		state.Strategies["hl-eth"].Positions["ETH"].StopLossOID = 0
		state.Strategies["hl-eth"].Positions["ETH"].StopLossTriggerPx = 0
		return strategies
	}
	for _, tc := range []struct {
		name        string
		live        bool
		stopPct     float64
		mutate      func([]StrategyConfig, *AppState) []StrategyConfig
		liq         map[string]float64
		net         map[string]string
		onChain     map[string]float64
		snapshot    bool
		wantOID     int64
		wantTrigger float64
		wantNoAlert bool
	}{
		{"paper strategy is never clamped against a live liquidation price", false, 3.125, nil, liqAuditLiqETH(), hlNetSideByCoinAllLong(), liqAuditOnChainOne(), true, 4242, 2325, true},
		{"shared coin whose book exceeds the on-chain snapshot refuses", true, 3.125, func(strategies []StrategyConfig, state *AppState) []StrategyConfig {
			scB := strategies[0]
			scB.ID = "hl-eth-b"
			state.Strategies["hl-eth-b"] = &StrategyState{ID: "hl-eth-b", Platform: "hyperliquid", Type: "perps", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: 1.0, AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30, StopLossOID: 4343, StopLossTriggerPx: 2325},
			}}
			return append(strategies, scB)
		}, liqAuditLiqETH(), hlNetSideByCoinAllLong(), liqAuditOnChainOne(), true, 4242, 2325, false},
		{"no on-chain position for the coin", true, 3.125, nil, liqAuditLiqETH(), hlNetSideByCoinAllLong(), map[string]float64{}, true, 4242, 2325, false},
		{"failed snapshot fetch with no stop armed raises no phantom alert", true, 3.0, zeroStop, liqAuditLiqETH(), hlNetSideByCoinAllLong(), liqAuditOnChainOne(), false, 0, 0, true},
		{"stale net side leaves a healthy resting stop untouched", true, 3.125, nil, liqAuditLiqETH(), map[string]string{"ETH": "short"}, liqAuditOnChainOne(), true, 4242, 2325, true},
		{"stale net side skips the sole-owner re-arm", true, 3.125, zeroStop, liqAuditLiqETH(), map[string]string{"ETH": "short"}, liqAuditOnChainOne(), true, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearHLLiquidationAlert("hl-eth", "ETH")
			clearHLLiquidationAlert("hl-eth-b", "ETH")
			t.Cleanup(func() {
				clearHLLiquidationAlert("hl-eth", "ETH")
				clearHLLiquidationAlert("hl-eth-b", "ETH")
			})
			strategies, state := liqAuditFixture(t, tc.live, tc.stopPct)
			if tc.mutate != nil {
				strategies = tc.mutate(strategies, state)
			}
			calls := 0
			stubLiqAuditStopLoss(t, func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				calls++
				return &HyperliquidStopLossUpdateResult{StopLossOID: 9001, StopLossTriggerPx: triggerPx}, "", nil
			})
			res := runHyperliquidLiquidationAudit(strategies, state, tc.liq, tc.net, tc.onChain, tc.snapshot, &sync.RWMutex{}, nil, time.Now().UTC())
			if calls != 0 {
				t.Errorf("placement calls = %d, want 0 — the audit must not touch the resting order", calls)
			}
			if res.ImmediateFills != 0 || len(res.CloseDetails) != 0 {
				t.Errorf("result = %+v, want empty", res)
			}
			pos := state.Strategies["hl-eth"].Positions["ETH"]
			if pos.StopLossOID != tc.wantOID || !approxEqLiq(pos.StopLossTriggerPx, tc.wantTrigger) {
				t.Errorf("state = oid=%d trigger=%g, want untouched oid=%d trigger=%g", pos.StopLossOID, pos.StopLossTriggerPx, tc.wantOID, tc.wantTrigger)
			}
			if _, alerted := hlLiquidationAlerts.Load(hlLiquidationAlertKey("hl-eth", "ETH")); tc.wantNoAlert && alerted {
				t.Error("a no-op pass must not raise a liquidation alert")
			}
		})
	}
}

func TestRunHyperliquidLiquidationAuditImmediateFillBooksClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result HyperliquidStopLossUpdateResult
	}{
		{"cancel then immediate fill at submit", HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossFilledImmediately: true}},
		{"immediate fill without a cancel flag", HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearHLLiquidationAlert("hl-eth", "ETH")
			t.Cleanup(func() { clearHLLiquidationAlert("hl-eth", "ETH") })
			strategies, state := liqAuditFixture(t, true, 3.125)
			stubLiqAuditStopLoss(t, func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				r := tc.result
				r.StopLossTriggerPx = triggerPx
				return &r, "", nil
			})
			res := runHyperliquidLiquidationAudit(strategies, state, liqAuditLiqETH(), hlNetSideByCoinAllLong(), liqAuditOnChainOne(), true, &sync.RWMutex{}, nil, time.Now().UTC())
			if res.ImmediateFills != 1 || len(res.CloseDetails) != 1 {
				t.Fatalf("immediate fills = %d, close details = %d, want 1/1 — a booked close must reach the trade notifier", res.ImmediateFills, len(res.CloseDetails))
			}
			cd := res.CloseDetails[0]
			if cd.SC.ID != "hl-eth" || cd.Symbol != "ETH" || cd.Detail == "" {
				t.Errorf("close detail = %+v, want a populated per-strategy line", cd)
			}
			ss := state.Strategies["hl-eth"]
			if _, still := ss.Positions["ETH"]; still {
				t.Error("an immediate fill must book the close and drop the position")
			}
			if len(ss.ClosedPositions) == 0 {
				t.Fatal("no closed position recorded for the immediate close")
			}
			if cp := ss.ClosedPositions[len(ss.ClosedPositions)-1]; cp.CloseReason != "liquidation_clamp_sl_immediate" {
				t.Errorf("persisted CloseReason = %q, want liquidation_clamp_sl_immediate (not the trailing walker)", cp.CloseReason)
			}
			if len(ss.TradeHistory) == 0 {
				t.Fatal("no trade recorded for the immediate close")
			}
			if tr := ss.TradeHistory[len(ss.TradeHistory)-1]; !strings.Contains(tr.Details, "Liquidation-clamp SL") {
				t.Errorf("trade details %q must match the LIQUIDATION-CLAMP operator wording", tr.Details)
			}
			if last := lastLiqAlertAction("hl-eth", "ETH"); last != hlLiquidationActionExited {
				t.Errorf("alert action = %q, want %q — the position is FLAT", last, hlLiquidationActionExited)
			}
		})
	}
}

func TestAuditRetriesPlacementItStrippedSameCycle(t *testing.T) {
	capRejected := "Order would exceed the open order limit"
	for _, tc := range []struct {
		name             string
		second           HyperliquidStopLossUpdateResult
		wantFinalOID     int64
		wantFinalTrigger float64
		wantAction       hlLiquidationAlertAction
		wantMinMutations int
	}{
		{"retry rests", HyperliquidStopLossUpdateResult{StopLossOID: 9002}, 9002, -1, hlLiquidationActionClamped, 0},
		{"retry resolves a resting oid by book diff despite the error text", HyperliquidStopLossUpdateResult{StopLossError: "place_stop_loss returned no usable status: {...}", StopLossOID: 9002}, 9002, -1, hlLiquidationActionClamped, 1},
		{"retry outcome unknown keeps the recorded state and reports unknown", HyperliquidStopLossUpdateResult{StopLossError: "place_stop_loss failed: boom", StopLossOutcomeUnknown: true}, 4242, 2325, hlLiquidationActionOutcomeUnknown, 0},
		{"retry also cap-rejected is protection lost", HyperliquidStopLossUpdateResult{StopLossError: capRejected}, 0, 0, hlLiquidationActionProtectionLost, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearHLLiquidationAlert("hl-eth", "ETH")
			t.Cleanup(func() { clearHLLiquidationAlert("hl-eth", "ETH") })
			strategies, state := liqTrailingAuditFixture(t)
			type call struct {
				cancelOID int64
				trigger   float64
			}
			var calls []call
			stubLiqAuditStopLoss(t, func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				calls = append(calls, call{cancelOID: cancelOID, trigger: triggerPx})
				if len(calls) == 1 {
					return &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossError: capRejected}, "", nil
				}
				r := tc.second
				if r.StopLossOID > 0 || r.StopLossOutcomeUnknown {
					r.StopLossTriggerPx = triggerPx
				}
				return &r, "", nil
			})
			res := runHyperliquidLiquidationAudit(strategies, state, liqAuditLiqETH(), hlNetSideByCoinAllLong(), liqAuditOnChainOne(), true, &sync.RWMutex{}, nil, time.Now().UTC())
			if len(calls) != 2 {
				t.Fatalf("placement calls = %d (%+v), want 2 (clamp + in-cycle retry)", len(calls), calls)
			}
			if calls[1].cancelOID != 0 {
				t.Errorf("retry cancelOID = %d, want 0 (nothing left to cancel)", calls[1].cancelOID)
			}
			pos := state.Strategies["hl-eth"].Positions["ETH"]
			if pos.StopLossOID != tc.wantFinalOID {
				t.Errorf("final oid = %d, want %d", pos.StopLossOID, tc.wantFinalOID)
			}
			if tc.wantFinalTrigger >= 0 && !approxEqLiq(pos.StopLossTriggerPx, tc.wantFinalTrigger) {
				t.Errorf("final trigger = %.4f, want %.4f", pos.StopLossTriggerPx, tc.wantFinalTrigger)
			}
			if last := lastLiqAlertAction("hl-eth", "ETH"); last != tc.wantAction {
				t.Errorf("alert action = %q, want %q", last, tc.wantAction)
			}
			if res.StateMutations < tc.wantMinMutations {
				t.Errorf("state mutations = %d, want >= %d (oid rewrite must flush)", res.StateMutations, tc.wantMinMutations)
			}
		})
	}

	t.Run("first-try rest takes no retry", func(t *testing.T) {
		clearHLLiquidationAlert("hl-eth", "ETH")
		t.Cleanup(func() { clearHLLiquidationAlert("hl-eth", "ETH") })
		strategies, state := liqTrailingAuditFixture(t)
		calls := 0
		stubLiqAuditStopLoss(t, func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			calls++
			return &HyperliquidStopLossUpdateResult{StopLossOID: 9001, StopLossTriggerPx: triggerPx}, "", nil
		})
		runHyperliquidLiquidationAudit(strategies, state, liqAuditLiqETH(), hlNetSideByCoinAllLong(), liqAuditOnChainOne(), true, &sync.RWMutex{}, nil, time.Now().UTC())
		if calls != 1 {
			t.Errorf("placement calls = %d, want 1", calls)
		}
	})
}

func TestAuditRearmOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		liq         map[string]float64
		result      HyperliquidStopLossUpdateResult
		passes      int
		wantCalls   int
		wantOID     int64
		wantTrigger float64
		wantAction  hlLiquidationAlertAction
	}{
		{"outcome unknown records the requested trigger and stops re-placing", liqAuditLiqETH(), HyperliquidStopLossUpdateResult{StopLossError: "place_stop_loss returned no usable status: {...}", StopLossOutcomeUnknown: true}, 2, 1, 0, 2340.5 * 1.005, hlLiquidationActionPlacementUnknown},
		{"genuine cap rejection keeps retrying next cycle", liqAuditLiqETH(), HyperliquidStopLossUpdateResult{StopLossError: "Order would exceed the open order limit"}, 2, 2, 0, -1, hlLiquidationActionRearmFailed},
		{"book-diff resolved oid is adopted despite the error text", liqAuditLiqETH(), HyperliquidStopLossUpdateResult{StopLossError: "place_stop_loss returned no usable status: {...}", StopLossOID: 9002}, 1, 1, 9002, -1, hlLiquidationActionRearmed},
		{"matching side with unknown geometry places the unclamped configured distance", map[string]float64{}, HyperliquidStopLossUpdateResult{StopLossOID: 9001}, 1, 1, 9001, 2400 * (1 - 0.03125), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearHLLiquidationAlert("hl-eth", "ETH")
			t.Cleanup(func() { clearHLLiquidationAlert("hl-eth", "ETH") })
			strategies, state := liqAuditFixture(t, true, 3.125)
			state.Strategies["hl-eth"].Positions["ETH"].StopLossOID = 0
			state.Strategies["hl-eth"].Positions["ETH"].StopLossTriggerPx = 0
			calls := 0
			var gotTrigger float64
			stubLiqAuditStopLoss(t, func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				calls++
				gotTrigger = triggerPx
				if cancelOID != 0 {
					t.Errorf("re-arm must place fresh, got cancelOID=%d", cancelOID)
				}
				r := tc.result
				if r.StopLossOID > 0 || r.StopLossOutcomeUnknown {
					r.StopLossTriggerPx = triggerPx
				}
				return &r, "", nil
			})
			for i := 0; i < tc.passes; i++ {
				runHyperliquidLiquidationAudit(strategies, state, tc.liq, hlNetSideByCoinAllLong(), liqAuditOnChainOne(), true, &sync.RWMutex{}, nil, time.Now().UTC())
				if tc.wantAction != "" {
					if last := lastLiqAlertAction("hl-eth", "ETH"); last != tc.wantAction {
						t.Errorf("pass %d alert action = %q, want %q", i+1, last, tc.wantAction)
					}
				}
			}
			if calls != tc.wantCalls {
				t.Errorf("placement calls across %d pass(es) = %d, want %d", tc.passes, calls, tc.wantCalls)
			}
			pos := state.Strategies["hl-eth"].Positions["ETH"]
			if pos.StopLossOID != tc.wantOID {
				t.Errorf("oid = %d, want %d", pos.StopLossOID, tc.wantOID)
			}
			if tc.wantTrigger >= 0 {
				if !approxEqLiq(gotTrigger, tc.wantTrigger) {
					t.Errorf("re-arm trigger = %.4f, want %.4f", gotTrigger, tc.wantTrigger)
				}
				if !approxEqLiq(pos.StopLossTriggerPx, tc.wantTrigger) {
					t.Errorf("recorded trigger = %.4f, want %.4f", pos.StopLossTriggerPx, tc.wantTrigger)
				}
			}
		})
	}
}
