package main

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

var errLiqAuditStub = errors.New("simulated stop-loss subprocess failure")

func approxEqLiq(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// --- pure geometry ---------------------------------------------------------

func TestStopPastLiquidationDirections(t *testing.T) {
	cases := []struct {
		name      string
		side      string
		triggerPx float64
		liqPx     float64
		want      bool
	}{
		// The motivating example: ETH long, entry 2400, liquidation 2340,
		// stop_loss_atr_mult 2.5 x ATR 30 -> trigger 2325, $15 past liquidation.
		{"long past", "long", 2325, 2340.5, true},
		{"long exactly at liquidation", "long", 2340.5, 2340.5, true},
		{"long safely inside", "long", 2360, 2340.5, false},
		{"short past", "short", 2460, 2440, true},
		{"short exactly at liquidation", "short", 2440, 2440, true},
		{"short safely inside", "short", 2420, 2440, false},
		// 0 means "unknown" on BOTH inputs and must never drive a clamp.
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

	// Long: the clamp moves the stop UP, i.e. tighter, and strictly inside.
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

	// Short: the clamp moves the stop DOWN, i.e. tighter.
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

	// The load-bearing safety property: a positive input trigger can never come
	// back as 0. Placing 0 would remove exchange-side protection outright.
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
	// Long: anchor 2400, ATR 30, mult 2.5 -> trigger 2325, past liquidation
	// 2340.5. The rewritten multiple must reproduce the clamped trigger through
	// the unchanged Python derivation anchor - mult*ATR.
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

	// Short: anchor 2400, ATR 30, mult 2.5 -> trigger 2475, past liquidation 2460.
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

	// A derived long trigger at or below zero is past liquidation by
	// construction. It must be rewritten, and never to a non-positive multiple
	// (0 would tell Python to place no stop at all).
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

// --- throttle --------------------------------------------------------------

func TestHLLiquidationShouldNotifyThrottle(t *testing.T) {
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// First observation always fires.
	send, st := hlLiquidationShouldNotify(hlLiquidationAlertState{}, false, base)
	if !send {
		t.Fatal("first observation must notify")
	}
	if !st.Notified {
		t.Fatal("state must record the notification")
	}

	// A repeat inside the interval is suppressed and leaves state untouched.
	send2, st2 := hlLiquidationShouldNotify(st, false, base.Add(time.Minute))
	if send2 {
		t.Error("repeat inside the throttle interval must be suppressed")
	}
	if st2 != st {
		t.Error("a suppressed cycle must carry the previous state forward unchanged")
	}

	// Past the interval floor it re-fires.
	send3, _ := hlLiquidationShouldNotify(st, false, base.Add(effectiveAlertThrottleInterval()+time.Second))
	if !send3 {
		t.Error("the alert_throttle_interval floor must re-fire the reminder")
	}

	// Escape hatch: the clamp stopped landing — escalate immediately, even
	// inside the interval.
	send4, st4 := hlLiquidationShouldNotify(st, true, base.Add(time.Minute))
	if !send4 {
		t.Error("a newly-deferred replace must notify immediately")
	}
	if !st4.ReplaceFailed {
		t.Error("state must record the deferred replace")
	}
	// ...but a SUSTAINED failure does not re-fire every cycle.
	send5, _ := hlLiquidationShouldNotify(st4, true, base.Add(2*time.Minute))
	if send5 {
		t.Error("a sustained deferred replace must not re-fire every cycle")
	}
}

func TestHLLiquidationAlertDueAndClear(t *testing.T) {
	hlLiquidationAlerts.Delete(hlLiquidationAlertKey("hl-eth", "ETH"))
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	if !hlLiquidationAlertDue("hl-eth", "ETH", false, now) {
		t.Fatal("first observation must be due")
	}
	if hlLiquidationAlertDue("hl-eth", "ETH", false, now.Add(time.Minute)) {
		t.Fatal("second observation inside the interval must be suppressed")
	}
	// Clearing (condition healed, or the position closed) re-arms the first-fire.
	clearHLLiquidationAlert("hl-eth", "ETH")
	if !hlLiquidationAlertDue("hl-eth", "ETH", false, now.Add(2*time.Minute)) {
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

	// Non-HL state is a no-op, so shared spot/futures close paths stay clean.
	hlLiquidationAlerts.Store(hlLiquidationAlertKey("spot-eth", "ETH"), hlLiquidationAlertState{Notified: true})
	clearHLPerpsPositionAlertThrottles(&StrategyState{ID: "spot-eth", Platform: "binanceus", Type: "spot"}, "ETH")
	if _, ok := hlLiquidationAlerts.Load(hlLiquidationAlertKey("spot-eth", "ETH")); !ok {
		t.Error("a non-HL close must not touch the throttle map")
	}
	hlLiquidationAlerts.Delete(hlLiquidationAlertKey("spot-eth", "ETH"))
}

// --- audit decision --------------------------------------------------------

func TestPlanHyperliquidLiquidationAuditClassification(t *testing.T) {
	cands := []hlLiquidationAuditCandidate{
		// scalar owner past liquidation -> replace job
		{StrategyID: "b-scalar", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 11, StopLossTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: true},
		// trailing owner past liquidation -> alert only
		{StrategyID: "a-trailing", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 12, StopLossTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: false},
		// reachable geometry -> no action
		{StrategyID: "c-ok", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 13, StopLossTriggerPx: 2360, LiquidationPx: 2340.5, StaticScalarOwner: true},
		// liquidation unknown -> no action
		{StrategyID: "d-unknown", Symbol: "BTC", Side: "long", Qty: 1, StopLossOID: 14, StopLossTriggerPx: 100, LiquidationPx: 0, StaticScalarOwner: true},
		// nothing armed -> no action
		{StrategyID: "e-nostop", Symbol: "BTC", Side: "long", Qty: 1, StopLossOID: 0, StopLossTriggerPx: 0, LiquidationPx: 40000, StaticScalarOwner: true},
		// zero size -> no action
		{StrategyID: "f-noqty", Symbol: "BTC", Side: "long", Qty: 0, StopLossOID: 15, StopLossTriggerPx: 39000, LiquidationPx: 40000, StaticScalarOwner: true},
	}
	actions := planHyperliquidLiquidationAudit(cands)
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want 2 (only the two past-liquidation candidates)", len(actions))
	}
	// Deterministic order by strategy id.
	if actions[0].Candidate.StrategyID != "a-trailing" || actions[1].Candidate.StrategyID != "b-scalar" {
		t.Fatalf("actions not sorted deterministically: %s, %s", actions[0].Candidate.StrategyID, actions[1].Candidate.StrategyID)
	}
	if actions[0].Replace {
		t.Error("a trailing owner must be alert-only — the walker owns its OID this cycle")
	}
	if !actions[1].Replace {
		t.Error("a static scalar owner has no re-place path, so the audit must replace it")
	}
	want := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(actions[1].ClampedTriggerPx, want) {
		t.Errorf("clamped trigger = %g, want %g", actions[1].ClampedTriggerPx, want)
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

// (a) A scalar stop past liquidation is cancelled and re-placed at the clamped
// trigger, and the position ends the cycle with the NEW oid.
func TestRunHyperliquidLiquidationAuditReplacesScalarStop(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125) // 2400 * (1-0.03125) = 2325
	var mu sync.RWMutex

	var gotTrigger float64
	var gotCancelOID int64
	var gotSize float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotTrigger, gotCancelOID, gotSize = triggerPx, cancelStopLossOID, size
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9001, StopLossTriggerPx: triggerPx}, "", nil
	}

	fills := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, &mu, nil, time.Now().UTC())
	if fills != 0 {
		t.Fatalf("immediate fills = %d, want 0 (a resting replacement)", fills)
	}
	if gotCancelOID != 4242 {
		t.Errorf("cancel OID = %d, want the resting 4242", gotCancelOID)
	}
	if !approxEqLiq(gotSize, 1.0) {
		t.Errorf("replace size = %g, want 1.0", gotSize)
	}
	wantTrigger := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(gotTrigger, wantTrigger) {
		t.Errorf("replace trigger = %g, want %g (just inside liquidation)", gotTrigger, wantTrigger)
	}
	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 9001 {
		t.Errorf("position SL oid = %d, want the replacement 9001", pos.StopLossOID)
	}
	if !approxEqLiq(pos.StopLossTriggerPx, wantTrigger) {
		t.Errorf("position trigger = %g, want %g", pos.StopLossTriggerPx, wantTrigger)
	}
}

// (b) THE acceptance test: when the replace fails, the position must still end
// the cycle with its ORIGINAL armed exchange-side stop. Protection is never
// removed by this mechanism.
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
		runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, &mu, nil, time.Now().UTC())
		pos := state.Strategies["hl-eth"].Positions["ETH"]
		if pos.StopLossOID != 4242 || !approxEqLiq(pos.StopLossTriggerPx, 2325) {
			t.Fatalf("failure %+v: position must keep its ORIGINAL armed stop, got oid=%d trigger=%g", f, pos.StopLossOID, pos.StopLossTriggerPx)
		}
	}
}

// (c) A paper strategy sharing the coin must never match a live liquidation
// price — paper has no account and no liquidation.
func TestRunHyperliquidLiquidationAuditSkipsPaper(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, false, 3.125)
	var mu sync.RWMutex
	called := false
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		return &HyperliquidStopLossUpdateResult{StopLossOID: 1}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, &mu, nil, time.Now().UTC())
	if called {
		t.Fatal("a paper strategy must never be clamped against a live liquidation price")
	}
	if state.Strategies["hl-eth"].Positions["ETH"].StopLossOID != 4242 {
		t.Error("paper position state must be untouched")
	}
}

// A trailing owner produces an alert but no replace job — the walker owns its
// OID this cycle and a second cancel+replace would race it.
func TestRunHyperliquidLiquidationAuditLeavesTrailingOwnerToTheWalker(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 0)
	strategies[0].StopLossPct = nil
	strategies[0].TrailingStopATRMult = floatPtr(2.5)
	var mu sync.RWMutex
	called := false
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		return &HyperliquidStopLossUpdateResult{StopLossOID: 1}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, &mu, nil, time.Now().UTC())
	if called {
		t.Fatal("the audit must not cancel+replace a trailing owner's OID")
	}
	if state.Strategies["hl-eth"].Positions["ETH"].StopLossOID != 4242 {
		t.Error("trailing owner position state must be untouched by the audit")
	}
}

func TestCollectHLLiquidationAuditCandidatesSkipsHedgeLegs(t *testing.T) {
	strategies, state := liqAuditFixture(t, true, 3.125)
	state.Strategies["hl-eth"].Positions["ETH"].HedgeFor = "hl-btc"
	var mu sync.RWMutex
	got := collectHLLiquidationAuditCandidates(strategies, state, map[string]float64{"ETH": 2340.5}, nil, &mu)
	if len(got) != 0 {
		t.Fatalf("candidates = %d, want 0 — a hedge leg carries no SL this strategy owns", len(got))
	}
}

// --- boot-time validation --------------------------------------------------

func TestValidateHLStopWithinBankruptcyBound(t *testing.T) {
	live := []string{"x.py", "ETH", "1h", "--mode=live"}
	paper := []string{"x.py", "ETH", "1h", "--mode=paper"}
	base := func() StrategyConfig {
		return StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Args: live, Leverage: 20}
	}

	// Impossible: 10% stop at 20x — bankruptcy sits at 5%.
	sc := base()
	sc.StopLossPct = floatPtr(10)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 1 {
		t.Fatalf("stop_loss_pct 10 @ 20x must be rejected, got %v", errs)
	} else if !strings.Contains(errs[0], "stop_loss_pct") {
		t.Errorf("message must name the field, got %q", errs[0])
	}

	// Aggressive but valid: 4.9% at 20x.
	sc = base()
	sc.StopLossPct = floatPtr(4.9)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("stop_loss_pct 4.9 @ 20x is valid, got %v", errs)
	}

	// Acceptance criterion: a valid low-leverage configuration must pass.
	sc = base()
	sc.Leverage = 2
	sc.StopLossPct = floatPtr(45)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("stop_loss_pct 45 @ 2x is valid, got %v", errs)
	}

	// Leverage 1 puts the bound at 100%, above the existing 50% caps.
	sc = base()
	sc.Leverage = 1
	sc.StopLossPct = floatPtr(50)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("stop_loss_pct 50 @ 1x is valid, got %v", errs)
	}

	// trailing_stop_pct is covered too.
	sc = base()
	sc.TrailingStopPct = floatPtr(10)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 1 {
		t.Errorf("trailing_stop_pct 10 @ 20x must be rejected, got %v", errs)
	}

	// stop_loss_margin_pct is read through its leverage derivation.
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

	// Out of scope: paper, cross margin, non-HL, non-perps.
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

	// Empty margin_mode reads as isolated, matching the runtime default.
	sc = base()
	sc.StopLossPct = floatPtr(10)
	sc.MarginMode = ""
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 1 {
		t.Errorf("empty margin_mode must be treated as isolated, got %v", errs)
	}
}
