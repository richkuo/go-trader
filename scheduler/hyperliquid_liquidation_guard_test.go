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

func TestHLClampProtectionSLMultLeavesReachableGeometryAlone(t *testing.T) {
	cases := []struct {
		name                       string
		side                       string
		anchor, atr, slMult, liqPx float64
		wantMult                   float64
		wantClamped                bool
	}{
		{"reachable long", "long", 2400, 30, 1.0, 2340.5, 1.0, false},
		{"unknown liquidation", "long", 2400, 30, 2.5, 0, 2.5, false},
		{"no ATR", "long", 2400, 0, 2.5, 2340.5, 2.5, false},
		{"no multiple", "long", 2400, 30, 0, 2340.5, 0, false},
		{"unknown side", "flat", 2400, 30, 2.5, 2340.5, 2.5, false},
	}
	for _, c := range cases {
		got, clamped := hlClampProtectionSLMult(c.side, c.anchor, c.atr, c.slMult, c.liqPx)
		if clamped != c.wantClamped {
			t.Errorf("%s: clamped = %v, want %v", c.name, clamped, c.wantClamped)
		}
		if !approxEqLiq(got, c.wantMult) {
			t.Errorf("%s: mult = %g, want %g", c.name, got, c.wantMult)
		}
	}

	newMult, ok := hlClampProtectionSLMult("long", 100, 60, 2.0, 90)
	if !ok {
		t.Fatal("a derived trigger at or below zero must be clamped")
	}
	if newMult <= 0 {
		t.Fatalf("clamped multiple = %g, must stay strictly positive", newMult)
	}
	if got := 100 - newMult*60; got <= 0 {
		t.Fatalf("derived trigger = %g, must stay strictly positive", got)
	}
}

func TestHLLiquidationShouldNotifyThrottle(t *testing.T) {
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	send, st := hlLiquidationShouldNotify(hlLiquidationAlertState{}, hlLiquidationActionClamped, base)
	if !send {
		t.Fatal("first observation must notify")
	}
	if !st.Notified {
		t.Fatal("state must record the notification")
	}

	send2, st2 := hlLiquidationShouldNotify(st, hlLiquidationActionClamped, base.Add(time.Minute))
	if send2 {
		t.Error("repeat inside the throttle interval must be suppressed")
	}
	if st2 != st {
		t.Error("a suppressed cycle must carry the previous state forward unchanged")
	}

	send3, _ := hlLiquidationShouldNotify(st, hlLiquidationActionClamped, base.Add(effectiveAlertThrottleInterval()+time.Second))
	if !send3 {
		t.Error("the alert_throttle_interval floor must re-fire the reminder")
	}

	send4, st4 := hlLiquidationShouldNotify(st, hlLiquidationActionReplaceDeferred, base.Add(time.Minute))
	if !send4 {
		t.Error("a newly-deferred replace must notify immediately")
	}
	if st4.LastAction != hlLiquidationActionReplaceDeferred {
		t.Error("state must record the deferred replace")
	}
	send5, _ := hlLiquidationShouldNotify(st4, hlLiquidationActionReplaceDeferred, base.Add(2*time.Minute))
	if send5 {
		t.Error("a sustained deferred replace must not re-fire every cycle")
	}

	send6, st6 := hlLiquidationShouldNotify(st4, hlLiquidationActionProtectionLost, base.Add(3*time.Minute))
	if !send6 {
		t.Fatal("deferred -> protection lost must notify immediately: the position went from protected to naked")
	}
	if st6.LastAction != hlLiquidationActionProtectionLost {
		t.Error("state must record the protection-lost action")
	}
	if send7, _ := hlLiquidationShouldNotify(st6, hlLiquidationActionClamped, base.Add(4*time.Minute)); !send7 {
		t.Error("protection lost -> clamped must notify: the operator was last told the position was naked")
	}
}

func TestHLLiquidationAlertDueAndClear(t *testing.T) {
	hlLiquidationAlerts.Delete(hlLiquidationAlertKey("hl-eth", "ETH"))
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	if !hlLiquidationAlertDue("hl-eth", "ETH", hlLiquidationActionClamped, now) {
		t.Fatal("first observation must be due")
	}
	if hlLiquidationAlertDue("hl-eth", "ETH", hlLiquidationActionClamped, now.Add(time.Minute)) {
		t.Fatal("second observation inside the interval must be suppressed")
	}
	clearHLLiquidationAlert("hl-eth", "ETH")
	if !hlLiquidationAlertDue("hl-eth", "ETH", hlLiquidationActionClamped, now.Add(2*time.Minute)) {
		t.Fatal("a cleared key must notify again on its first observation")
	}
	hlLiquidationAlerts.Delete(hlLiquidationAlertKey("hl-eth", "ETH"))
}

func TestClearHLPerpsPositionAlertThrottlesClearsBoth(t *testing.T) {
	ss := &StrategyState{ID: "hl-eth", Platform: "hyperliquid", Type: "perps"}
	hlLiquidationAlerts.Store(hlLiquidationAlertKey("hl-eth", "ETH"), hlLiquidationAlertState{Notified: true})
	atrMultMissingEntryATRWarned.Store(atrMultMissingEntryATRKey("hl-eth", "ETH"), struct{}{})

	clearHLPerpsPositionAlertThrottles(ss, "ETH")

	if _, ok := hlLiquidationAlerts.Load(hlLiquidationAlertKey("hl-eth", "ETH")); ok {
		t.Error("position close must clear the #1450 liquidation throttle")
	}
	if _, ok := atrMultMissingEntryATRWarned.Load(atrMultMissingEntryATRKey("hl-eth", "ETH")); ok {
		t.Error("position close must still clear the missing-EntryATR throttle")
	}

	hlLiquidationAlerts.Store(hlLiquidationAlertKey("spot-eth", "ETH"), hlLiquidationAlertState{Notified: true})
	clearHLPerpsPositionAlertThrottles(&StrategyState{ID: "spot-eth", Platform: "binanceus", Type: "spot"}, "ETH")
	if _, ok := hlLiquidationAlerts.Load(hlLiquidationAlertKey("spot-eth", "ETH")); !ok {
		t.Error("a non-HL close must not touch the throttle map")
	}
	hlLiquidationAlerts.Delete(hlLiquidationAlertKey("spot-eth", "ETH"))
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

func TestStaticScalarOwnerCannotScaleIn(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:        []string{"x.py", "ETH", "1h", "--mode=live"},
		StopLossPct: floatPtr(3.125),
		Leverage:    3,
	}
	if scaleInLiveProtectionResizable(sc) {
		t.Fatal("a stop_loss_pct owner must classify as a static scalar owner")
	}
	sc.AllowScaleIn = true
	cfg := &Config{ConfigVersion: CurrentConfigVersion, Strategies: []StrategyConfig{sc}}
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "allow_scale_in on live perps requires") {
		t.Fatalf("allow_scale_in on a static scalar owner must be rejected at load, got %v", err)
	}
}

func TestRunHyperliquidLiquidationAuditHealsTrailingOwner(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 0)
	strategies[0].StopLossPct = nil
	strategies[0].TrailingStopATRMult = floatPtr(2.5)
	var mu sync.RWMutex
	var gotTrigger float64
	var gotCancelOID int64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotTrigger, gotCancelOID = triggerPx, cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9100, StopLossTriggerPx: triggerPx}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())

	wantTrigger := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(gotTrigger, wantTrigger) || gotCancelOID != 4242 {
		t.Fatalf("audit must tighten the trailing owner's resting stop, got trigger=%g cancel_oid=%d", gotTrigger, gotCancelOID)
	}
	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 9100 || !approxEqLiq(pos.StopLossTriggerPx, wantTrigger) {
		t.Errorf("position must carry the tightened stop, got oid=%d trigger=%g", pos.StopLossOID, pos.StopLossTriggerPx)
	}
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9101, StopLossTriggerPx: triggerPx}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if calls != 0 {
		t.Errorf("a reachable resting trigger must produce no further order churn, got %d placements", calls)
	}
}

func TestCollectHLLiquidationAuditCandidatesSkipsHedgeLegs(t *testing.T) {
	strategies, state := liqAuditFixture(t, true, 3.125)
	state.Strategies["hl-eth"].Positions["ETH"].HedgeFor = "hl-btc"
	var mu sync.RWMutex
	got := collectHLLiquidationAuditCandidates(strategies, state, map[string]float64{"ETH": 2340.5}, nil, nil, &mu)
	if len(got) != 0 {
		t.Fatalf("candidates = %d, want 0 — a hedge leg carries no SL this strategy owns", len(got))
	}
}

func TestValidateHLStopWithinBankruptcyBound(t *testing.T) {
	live := []string{"x.py", "ETH", "1h", "--mode=live"}
	paper := []string{"x.py", "ETH", "1h", "--mode=paper"}
	base := func() StrategyConfig {
		return StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Args: live, Leverage: 20}
	}

	sc := base()
	sc.StopLossPct = floatPtr(10)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 1 {
		t.Fatalf("stop_loss_pct 10 @ 20x must be rejected, got %v", errs)
	} else if !strings.Contains(errs[0], "stop_loss_pct") {
		t.Errorf("message must name the field, got %q", errs[0])
	}

	sc = base()
	sc.StopLossPct = floatPtr(4.9)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("stop_loss_pct 4.9 @ 20x is valid, got %v", errs)
	}

	sc = base()
	sc.Leverage = 2
	sc.StopLossPct = floatPtr(45)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("stop_loss_pct 45 @ 2x is valid, got %v", errs)
	}

	sc = base()
	sc.Leverage = 1
	sc.StopLossPct = floatPtr(50)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("stop_loss_pct 50 @ 1x is valid, got %v", errs)
	}

	sc = base()
	sc.TrailingStopPct = floatPtr(10)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("trailing_stop_pct 10 @ 20x must NOT be rejected (anchor ratchets), got %v", errs)
	}

	sc = base()
	sc.MaxDrawdownPct = 15
	errs := validateHLStopWithinBankruptcyBound(sc)
	if len(errs) != 1 || !strings.Contains(errs[0], "max_drawdown_pct") {
		t.Errorf("max_drawdown_pct fallback 15 @ 20x must be rejected, got %v", errs)
	}
	sc = base()
	sc.MaxDrawdownPct = 90
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 1 {
		t.Errorf("max_drawdown_pct 90 @ 20x resolves through its 50%% cap to a rejection, got %v", errs)
	}

	sc = base()
	sc.StopLossPct = floatPtr(4.9)
	sc.MaxDrawdownPct = 15
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("explicit stop_loss_pct owns the stop; max_drawdown_pct must not be checked, got %v", errs)
	}

	sc = base()
	sc.StopLossMarginPct = floatPtr(100)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 1 {
		t.Errorf("stop_loss_margin_pct 100 (derived 5%% @ 20x) must be rejected, got %v", errs)
	}
	sc = base()
	sc.StopLossMarginPct = floatPtr(80)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("stop_loss_margin_pct 80 (derived 4%% @ 20x) is valid, got %v", errs)
	}

	sc = base()
	sc.StopLossPct = floatPtr(10)
	sc.Args = paper
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("paper has no liquidation price and must skip, got %v", errs)
	}
	sc = base()
	sc.StopLossPct = floatPtr(10)
	sc.MarginMode = "cross"
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("cross margin liquidation can sit beyond 1/leverage and must skip, got %v", errs)
	}
	sc = base()
	sc.StopLossPct = floatPtr(10)
	sc.Platform = "binanceus"
	sc.Type = "spot"
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("non-HL-perps must skip, got %v", errs)
	}

	sc = base()
	sc.StopLossPct = floatPtr(10)
	sc.MarginMode = ""
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 1 {
		t.Errorf("empty margin_mode must be treated as isolated, got %v", errs)
	}
}

func TestHLLiquidationPxForSideGatesOnNetSide(t *testing.T) {
	liq := map[string]float64{"ETH": 2340.5}
	net := map[string]string{"ETH": "long"}
	if got := hlLiquidationPxForSide(liq, net, "ETH", "long"); got != 2340.5 {
		t.Errorf("matching long = %g, want the net liquidation price", got)
	}
	if got := hlLiquidationPxForSide(liq, net, "ETH", "short"); got != 0 {
		t.Errorf("opposite-side leg = %g, want 0 (unknown)", got)
	}
	if got := hlLiquidationPxForSide(liq, nil, "ETH", "long"); got != 0 {
		t.Errorf("missing net-side map = %g, want 0 (unconfirmable)", got)
	}
	if got := hlLiquidationPxForSide(liq, map[string]string{}, "ETH", "long"); got != 0 {
		t.Errorf("coin absent from the net-side map = %g, want 0", got)
	}
	if got := hlLiquidationPxForSide(map[string]float64{"ETH": 0}, net, "ETH", "long"); got != 0 {
		t.Errorf("non-positive price = %g, want 0", got)
	}
	if got := hlLiquidationPxForSide(liq, net, "DOGE", "long"); got != 0 {
		t.Errorf("coin absent from both maps = %g, want 0", got)
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
			runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, liq, tc.net)
			if gotForce != tc.wantForce {
				t.Errorf("ForceSLReplace = %v, want %v", gotForce, tc.wantForce)
			}
			if gotMult < tc.wantMultLo || gotMult > tc.wantMultHi {
				t.Errorf("plan slMult = %g, want within [%g, %g]", gotMult, tc.wantMultLo, tc.wantMultHi)
			}
		})
	}
}

func TestHLStopBankruptcyBoundSkipsInertPctFieldsUnderUnifiedClose(t *testing.T) {
	base := func(unified bool) StrategyConfig {
		sc := StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script: "x.py", Args: []string{"x.py", "ETH", "1h", "--mode=live"},
			StopLossPct:       floatPtr(10),
			StopLossMarginPct: floatPtr(200),
			Leverage:          20,
			MarginMode:        "isolated",
		}
		if unified {
			sc.CloseStrategy = &StrategyRef{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"trend_regime": map[string]interface{}{
				"trending_up":   map[string]interface{}{"stop_loss_atr": 2.0, "tp_tiers": []interface{}{map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0}}},
				"trending_down": map[string]interface{}{"stop_loss_atr": 2.0, "tp_tiers": []interface{}{map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0}}},
				"ranging":       map[string]interface{}{"stop_loss_atr": 2.0, "tp_tiers": []interface{}{map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0}}},
			}}}
		}
		return sc
	}
	if errs := validateHLStopWithinBankruptcyBound(base(true)); len(errs) != 0 {
		t.Errorf("unified close owns the stop; bound reported %v", errs)
	}
	errs := validateHLStopWithinBankruptcyBound(base(false))
	if len(errs) != 2 {
		t.Errorf("without a unified close both pct fields must be scored, got %v", errs)
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

func TestBuildHLLiquidationMaps(t *testing.T) {
	onChain, liqPx, netSide := buildHLLiquidationMaps([]HLPosition{
		{Coin: "ETH", Size: -2.5, LiquidationPx: 1800.25},
		{Coin: "BTC", Size: 0.4, LiquidationPx: 0},
		{Coin: "DUST", Size: 5e-10, LiquidationPx: 99},
	})
	if onChain["ETH"] != 2.5 || onChain["BTC"] != 0.4 {
		t.Errorf("onChain = %v, want ETH 2.5 / BTC 0.4", onChain)
	}
	if _, ok := onChain["DUST"]; ok {
		t.Error("DUST position must be omitted below the 1e-9 floor")
	}
	if liqPx["ETH"] != 1800.25 {
		t.Errorf("liqPx[ETH] = %v, want 1800.25", liqPx["ETH"])
	}
	if _, ok := liqPx["BTC"]; ok {
		t.Error("BTC reported no liquidation price — must stay unknown")
	}
	if netSide["ETH"] != "short" || netSide["BTC"] != "long" {
		t.Errorf("netSide = %v, want ETH short / BTC long", netSide)
	}
	if _, ok := netSide["DUST"]; ok {
		t.Error("DUST must carry no side stamp")
	}
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
		state := newState()
		dirty, failures := flushOffCycleLiquidationAuditState(state, cfg, db, &mu, 1, false, 0, false)
		if dirty || failures != 0 {
			t.Fatalf("dirty=%v failures=%d, want false/0", dirty, failures)
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
		state := newState()
		dirty, failures := flushOffCycleLiquidationAuditState(state, cfg, db, &mu, 0, false, 2, false)
		if dirty {
			t.Errorf("dirty = true, want false")
		}
		if failures != 2 {
			t.Errorf("failures = %d, want 2 (untouched — no save attempted)", failures)
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
		state := newState()
		db.Close()
		dirty, failures := flushOffCycleLiquidationAuditState(state, cfg, db, &mu, 1, false, 0, false)
		if !dirty {
			t.Errorf("dirty = false after a failed save, want true (close still only in memory)")
		}
		if failures != 1 {
			t.Errorf("failures = %d, want 1 (reported like the end-of-cycle save failure)", failures)
		}
	})

	t.Run("a latched failure retries on a pass that books nothing", func(t *testing.T) {
		db := openTestDB(t)
		state := newState()
		dirty, failures := flushOffCycleLiquidationAuditState(state, cfg, db, &mu, 0, true, 1, false)
		if dirty || failures != 0 {
			t.Fatalf("dirty=%v failures=%d, want false/0 after the retry succeeded", dirty, failures)
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

func TestApplyAuditStopUpdateCountsStateMutations(t *testing.T) {
	newSS := func(oid int64, trigger float64) *StrategyState {
		return &StrategyState{
			ID:        "hl-live",
			Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 25, StopLossOID: oid, StopLossTriggerPx: trigger}},
		}
	}
	logger := newTestLogger(t)

	t.Run("clamp that rests a replacement counts, with no fill", func(t *testing.T) {
		var res hlLiquidationAuditResult
		ss := newSS(111, 1850)
		fill, _ := applyAuditStopUpdate(&res, ss, "ETH", "long", 111, 1.0, &HyperliquidStopLossUpdateResult{StopLossOID: 222, StopLossTriggerPx: 1900}, logger)
		if fill {
			t.Fatalf("immediateFill = true, want false")
		}
		if res.StateMutations != 1 {
			t.Errorf("StateMutations = %d, want 1", res.StateMutations)
		}
		if res.ImmediateFills != 0 || len(res.CloseDetails) != 0 {
			t.Errorf("close accounting touched on a pure replace: %+v", res)
		}
		pos := ss.Positions["ETH"]
		if pos.StopLossOID != 222 || pos.StopLossTriggerPx != 1900 {
			t.Errorf("position = OID %d trigger %.2f, want 222 / 1900", pos.StopLossOID, pos.StopLossTriggerPx)
		}
	})

	t.Run("static-scalar re-arm from no stop counts", func(t *testing.T) {
		var res hlLiquidationAuditResult
		ss := newSS(0, 0)
		if _, _ = applyAuditStopUpdate(&res, ss, "ETH", "long", 0, 1.0, &HyperliquidStopLossUpdateResult{StopLossOID: 777, StopLossTriggerPx: 1880}, logger); res.StateMutations != 1 {
			t.Errorf("StateMutations = %d, want 1 on a re-arm", res.StateMutations)
		}
	})

	t.Run("cancel without a rest zeroes the dead OID and counts", func(t *testing.T) {
		var res hlLiquidationAuditResult
		ss := newSS(111, 1850)
		if _, _ = applyAuditStopUpdate(&res, ss, "ETH", "long", 111, 1.0, &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}, logger); res.StateMutations != 1 {
			t.Errorf("StateMutations = %d, want 1 on cancel-without-rest", res.StateMutations)
		}
		if pos := ss.Positions["ETH"]; pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
			t.Errorf("dead OID not cleared: OID %d trigger %.2f", pos.StopLossOID, pos.StopLossTriggerPx)
		}
	})

	t.Run("a booked close counts", func(t *testing.T) {
		var res hlLiquidationAuditResult
		ss := newSS(111, 1850)
		ss.Cash = 1000
		fill, fillPx := applyAuditStopUpdate(&res, ss, "ETH", "long", 111, 1.0, &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 1850}, logger)
		if !fill || fillPx != 1850 {
			t.Fatalf("fill = %v @ %.2f, want true @ 1850", fill, fillPx)
		}
		if res.StateMutations != 1 {
			t.Errorf("StateMutations = %d, want 1 on a booked close", res.StateMutations)
		}
	})

	t.Run("a call that changes nothing counts nothing", func(t *testing.T) {
		var res hlLiquidationAuditResult
		ss := newSS(111, 1850)

		if _, _ = applyAuditStopUpdate(&res, ss, "ETH", "long", 111, 1.0, nil, logger); res.StateMutations != 0 {
			t.Errorf("StateMutations = %d after a nil update, want 0", res.StateMutations)
		}
		if _, _ = applyAuditStopUpdate(&res, ss, "ETH", "short", 111, 1.0, &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: 1900}, logger); res.StateMutations != 0 {
			t.Errorf("StateMutations = %d after a side mismatch, want 0", res.StateMutations)
		}
		if _, _ = applyAuditStopUpdate(&res, ss, "ETH", "long", 42, 1.0, &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}, logger); res.StateMutations != 0 {
			t.Errorf("StateMutations = %d after a stale-OID cancel, want 0", res.StateMutations)
		}
		if pos := ss.Positions["ETH"]; pos.StopLossOID != 111 || pos.StopLossTriggerPx != 1850 {
			t.Errorf("no-op calls mutated the position: OID %d trigger %.2f", pos.StopLossOID, pos.StopLossTriggerPx)
		}
	})
}

func TestPlaceFreshClassifiesByWhatRests(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	t.Run("book-diff resolved oid outranks the error text", func(t *testing.T) {
		runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			return &HyperliquidStopLossUpdateResult{
				StopLossError:     "place_stop_loss returned no usable status: {...}",
				StopLossOID:       9002,
				StopLossTriggerPx: triggerPx,
			}, "", nil
		}
		result, outcome := hlLiquidationPlaceFresh("x.py", "ETH", "long", 1.0, 2300, nil)
		if outcome != hlReplacePlaced {
			t.Fatalf("outcome = %v, want placed (the diff resolved one resting oid)", outcome)
		}
		if result == nil || result.StopLossOID != 9002 {
			t.Fatalf("result = %+v, want the resolved oid", result)
		}
	})

	t.Run("unreadable retry is outcome-unknown, never protection-lost", func(t *testing.T) {
		runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			return &HyperliquidStopLossUpdateResult{
				StopLossError:          "place_stop_loss failed: boom",
				StopLossOutcomeUnknown: true,
			}, "", nil
		}
		if _, outcome := hlLiquidationPlaceFresh("x.py", "ETH", "long", 1.0, 2300, nil); outcome != hlReplaceOutcomeUnknown {
			t.Fatalf("outcome = %v, want outcome-unknown", outcome)
		}
	})

	t.Run("positively rejected placement still defers", func(t *testing.T) {
		runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			return &HyperliquidStopLossUpdateResult{
				StopLossError: "Order would exceed the open order limit",
			}, "", nil
		}
		if _, outcome := hlLiquidationPlaceFresh("x.py", "ETH", "long", 1.0, 2300, nil); outcome != hlReplaceDeferred {
			t.Fatalf("outcome = %v, want deferred", outcome)
		}
	})
}

func TestFlushOffCycleLiquidationAuditStateForceProbe(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{{ID: "hl-live", Platform: "hyperliquid", Type: "perps"}}}
	newState := func() *AppState {
		return &AppState{Strategies: map[string]*StrategyState{
			"hl-live": {ID: "hl-live", Platform: "hyperliquid", Type: "perps",
				Cash: 1234.5, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		}}
	}
	var mu sync.RWMutex

	t.Run("force saves through a clean latch", func(t *testing.T) {
		db := openTestDB(t)
		state := newState()
		dirty, failures := flushOffCycleLiquidationAuditState(state, cfg, db, &mu, 0, false, 3, true)
		if dirty || failures != 0 {
			t.Fatalf("dirty=%v failures=%d, want false/0 (probe save succeeded, halt cleared)", dirty, failures)
		}
	})

	t.Run("force keeps counting on a failing save", func(t *testing.T) {
		db := openTestDB(t)
		state := newState()
		db.Close()
		dirty, failures := flushOffCycleLiquidationAuditState(state, cfg, db, &mu, 0, false, 3, true)
		if !dirty {
			t.Errorf("dirty = false after a failed probe save, want true")
		}
		if failures != 4 {
			t.Errorf("failures = %d, want 4 (probe failure counts like any save failure)", failures)
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

func TestResidueStateConvergesOnReadableReplace(t *testing.T) {
	ss := &StrategyState{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Side: "long", Quantity: 1.0, AvgCost: 2400,
			StopLossTriggerPx: 2352.2025},
	}}
	residue := ss.Positions["ETH"]
	fill, fillPx := applyTrailingStopUpdateResult(ss, "ETH", "long", 0, 0, true,
		&HyperliquidStopLossUpdateResult{StopLossOID: 7777, StopLossTriggerPx: 2360},
		"trailing_stop_loss_immediate", nil, 0)
	if fill || fillPx != 0 {
		t.Fatalf("resting replacement read as an immediate fill (%v, %.4f)", fill, fillPx)
	}
	if residue.StopLossOID != 7777 || residue.StopLossTriggerPx != 2360 {
		t.Fatalf("state = oid=%d trigger=%.4f, want the single tracked resting stop oid=7777 trigger=2360",
			residue.StopLossOID, residue.StopLossTriggerPx)
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
		{"failed snapshot fetch with no stop armed raises no phantom alert", true, 3.0, zeroStop, map[string]float64{}, nil, map[string]float64{}, false, 0, 0, true},
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

func TestRunHyperliquidLiquidationAuditHealsRegardlessOfDueness(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(sc *StrategyConfig)
	}{
		{"non-due 4h trailing owner", func(sc *StrategyConfig) {
			sc.StopLossPct = nil
			sc.TrailingStopATRMult = floatPtr(2.5)
			sc.Args = []string{"x.py", "ETH", "4h", "--mode=live"}
		}},
		{"non-due 1d static scalar owner", func(sc *StrategyConfig) {
			sc.Args = []string{"x.py", "ETH", "1d", "--mode=live"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearHLLiquidationAlert("hl-eth", "ETH")
			t.Cleanup(func() { clearHLLiquidationAlert("hl-eth", "ETH") })
			strategies, state := liqAuditFixture(t, true, 3.125)
			tc.mutate(&strategies[0])
			placed := false
			stubLiqAuditStopLoss(t, func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				placed = true
				return &HyperliquidStopLossUpdateResult{StopLossOID: 9200, StopLossTriggerPx: triggerPx}, "", nil
			})
			runHyperliquidLiquidationAudit(strategies, state, liqAuditLiqETH(), hlNetSideByCoinAllLong(), liqAuditOnChainOne(), true, &sync.RWMutex{}, nil, time.Now().UTC())
			if !placed {
				t.Fatal("a consistent book must still be healed by the per-cycle audit regardless of due-ness")
			}
		})
	}
}

func TestRunHyperliquidLiquidationAuditTightenOutcomes(t *testing.T) {
	wantTrigger := liqAuditClampedTrigger()
	for _, tc := range []struct {
		name           string
		result         HyperliquidStopLossUpdateResult
		wantOID        int64
		wantPosTrigger float64
		wantAction     hlLiquidationAlertAction
	}{
		{"resting replacement lands", HyperliquidStopLossUpdateResult{StopLossOID: 9001}, 9001, wantTrigger, hlLiquidationActionClamped},
		{"cancel then place is a normal clamp", HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOID: 5150}, 5150, wantTrigger, hlLiquidationActionClamped},
		{"filled externally is the reconciler's close", HyperliquidStopLossUpdateResult{StopLossFilledExternally: true}, 4242, 2325, hlLiquidationActionFilledOnChain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearHLLiquidationAlert("hl-eth", "ETH")
			t.Cleanup(func() { clearHLLiquidationAlert("hl-eth", "ETH") })
			strategies, state := liqAuditFixture(t, true, 3.125)
			var gotTrigger, gotSize float64
			var gotCancelOID int64
			stubLiqAuditStopLoss(t, func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				gotTrigger, gotCancelOID, gotSize = triggerPx, cancelOID, size
				r := tc.result
				if r.StopLossOID > 0 {
					r.StopLossTriggerPx = triggerPx
				}
				return &r, "", nil
			})
			res := runHyperliquidLiquidationAudit(strategies, state, liqAuditLiqETH(), hlNetSideByCoinAllLong(), liqAuditOnChainOne(), true, &sync.RWMutex{}, nil, time.Now().UTC())
			if res.ImmediateFills != 0 || len(res.CloseDetails) != 0 {
				t.Fatalf("result = %+v, want no booked close on a resting or external outcome", res)
			}
			if gotCancelOID != 4242 {
				t.Errorf("cancel OID = %d, want the resting 4242", gotCancelOID)
			}
			if !approxEqLiq(gotSize, 1.0) {
				t.Errorf("replace size = %g, want 1.0", gotSize)
			}
			if !approxEqLiq(gotTrigger, wantTrigger) {
				t.Errorf("replace trigger = %g, want %g (just inside liquidation)", gotTrigger, wantTrigger)
			}
			pos := state.Strategies["hl-eth"].Positions["ETH"]
			if pos.StopLossOID != tc.wantOID || !approxEqLiq(pos.StopLossTriggerPx, tc.wantPosTrigger) {
				t.Errorf("position = oid=%d trigger=%g, want oid=%d trigger=%g", pos.StopLossOID, pos.StopLossTriggerPx, tc.wantOID, tc.wantPosTrigger)
			}
			if last := lastLiqAlertAction("hl-eth", "ETH"); last != tc.wantAction {
				t.Errorf("alert action = %q, want %q", last, tc.wantAction)
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

func TestLiquidationAuditIntervalSeconds(t *testing.T) {
	mk := func(id, platform, typ string, live bool) StrategyConfig {
		args := []string{"--mode", "paper"}
		if live {
			args = []string{"--mode", "live"}
		}
		return StrategyConfig{ID: id, Platform: platform, Type: typ, Args: args}
	}
	fleet := []StrategyConfig{
		mk("hl-live-a", "hyperliquid", "perps", true),
		mk("hl-paper-b", "hyperliquid", "perps", false),
		mk("okx-live-c", "okx", "perps", true),
		mk("hl-live-spot", "hyperliquid", "spot", true),
	}
	fleetIntervals := map[string]int{"hl-live-a": 300, "hl-paper-b": 60, "okx-live-c": 120, "hl-live-spot": 30}
	for _, tc := range []struct {
		name       string
		strategies []StrategyConfig
		intervals  map[string]int
		want       int
	}{
		{"half of the sole live HL perps interval", fleet, fleetIntervals, 150},
		{"no live HL perps or manual", fleet[1:], fleetIntervals, 0},
		{"4h fleet cadence is strictly shorter than the interval it bounds", []StrategyConfig{mk("hl-4h", "hyperliquid", "perps", true)}, map[string]int{"hl-4h": 14400}, 7200},
		{"manual-only fleet is an audit candidate", []StrategyConfig{mk("hl-manual", "hyperliquid", "manual", true)}, map[string]int{"hl-manual": 3600}, 1800},
		{"fast fleet clamps to the floor", []StrategyConfig{mk("hl-4h", "hyperliquid", "perps", true)}, map[string]int{"hl-4h": 30}, liquidationAuditMinIntervalSeconds},
		{"paper manual is not a candidate", []StrategyConfig{mk("hl-pm", "hyperliquid", "manual", false)}, map[string]int{"hl-pm": 3600}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := liquidationAuditIntervalSeconds(tc.strategies, tc.intervals); got != tc.want {
				t.Errorf("interval = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestHLLiquidationAlertMessage(t *testing.T) {
	for _, tc := range []struct {
		name            string
		trigger         float64
		clamped         float64
		liq             float64
		action          hlLiquidationAlertAction
		wantHeadline    string
		wantContains    []string
		wantNotContains []string
		wantUnprotected bool
	}{
		{"refused with no stop omits fabricated geometry and is unprotected", 0, 0, 0, hlLiquidationActionUnreconciled, "**HL POSITION UNPROTECTED**", []string{"NO exchange-side stop"}, []string{"$0.0000"}, true},
		{"refused tighten keeps the old order and names the real prices", 2325, 2352, 2340.5, hlLiquidationActionUnreconciled, "", []string{"$2325.0000", "$2340.5000"}, nil, false},
		{"outcome unknown never claims the position is naked", 1800, 1900, 1750, hlLiquidationActionOutcomeUnknown, "OUTCOME UNKNOWN", []string{"could NOT be read", "may be resting", "KEPT"}, []string{"no exchange-side stop right now"}, false},
		{"outcome unknown after a tighten keeps the cancel claim", 2325, 2352.2025, 2340.5, hlLiquidationActionOutcomeUnknown, "", []string{"CANCELLED"}, nil, false},
		{"placement unknown after a fresh re-arm asserts no cancel", 0, 2352.2025, 2340.5, hlLiquidationActionPlacementUnknown, "", []string{"Nothing was cancelled"}, []string{"old trigger was CANCELLED"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headline, detail, unprotected := hlLiquidationAlertMessage(tc.trigger, tc.clamped, tc.liq, tc.action, "rec")
			if tc.wantHeadline != "" && !strings.Contains(headline, tc.wantHeadline) {
				t.Errorf("headline = %q, want it to contain %q", headline, tc.wantHeadline)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(detail, want) {
					t.Errorf("detail = %q, want it to contain %q", detail, want)
				}
			}
			for _, bad := range tc.wantNotContains {
				if strings.Contains(detail, bad) {
					t.Errorf("detail = %q, must not contain %q", detail, bad)
				}
			}
			if unprotected != tc.wantUnprotected {
				t.Errorf("unprotected = %v, want %v", unprotected, tc.wantUnprotected)
			}
		})
	}
}

func TestOutcomeUnknownIsNotProtectionLost(t *testing.T) {
	if hlLiquidationActionUnprotected(hlLiquidationActionOutcomeUnknown) {
		t.Errorf("outcome-unknown counted as unprotected — the operator is told a fact nobody measured")
	}
	if !hlLiquidationActionUnprotected(hlLiquidationActionProtectionLost) {
		t.Errorf("protection lost must still count as unprotected")
	}
	if hlLiquidationMayRetryReplace(&HyperliquidStopLossUpdateResult{StopLossOutcomeUnknown: true}) {
		t.Errorf("outcome-unknown must not license an in-cycle retry")
	}
	if !hlLiquidationMayRetryReplace(&HyperliquidStopLossUpdateResult{}) {
		t.Errorf("a positively rejected placement must still retry")
	}
	if got := hlLiquidationArmClampAction(&HyperliquidStopLossUpdateResult{StopLossError: "boom", StopLossOutcomeUnknown: true, StopLossTriggerPx: 2300}, true); got != hlLiquidationActionPlacementUnknown {
		t.Errorf("arm-clamp action = %q, want %q", got, hlLiquidationActionPlacementUnknown)
	}
	if got := hlLiquidationArmClampAction(&HyperliquidStopLossUpdateResult{StopLossError: "Order would exceed the open order limit"}, true); got != hlLiquidationActionRearmFailed {
		t.Errorf("positive rejection = %q, want re-arm failed", got)
	}
}
