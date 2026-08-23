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
	send, st := hlLiquidationShouldNotify(hlLiquidationAlertState{}, hlLiquidationActionClamped, base)
	if !send {
		t.Fatal("first observation must notify")
	}
	if !st.Notified {
		t.Fatal("state must record the notification")
	}

	// A repeat inside the interval is suppressed and leaves state untouched.
	send2, st2 := hlLiquidationShouldNotify(st, hlLiquidationActionClamped, base.Add(time.Minute))
	if send2 {
		t.Error("repeat inside the throttle interval must be suppressed")
	}
	if st2 != st {
		t.Error("a suppressed cycle must carry the previous state forward unchanged")
	}

	// Past the interval floor it re-fires.
	send3, _ := hlLiquidationShouldNotify(st, hlLiquidationActionClamped, base.Add(effectiveAlertThrottleInterval()+time.Second))
	if !send3 {
		t.Error("the alert_throttle_interval floor must re-fire the reminder")
	}

	// Escape hatch: the clamp stopped landing — escalate immediately, even
	// inside the interval.
	send4, st4 := hlLiquidationShouldNotify(st, hlLiquidationActionReplaceDeferred, base.Add(time.Minute))
	if !send4 {
		t.Error("a newly-deferred replace must notify immediately")
	}
	if st4.LastAction != hlLiquidationActionReplaceDeferred {
		t.Error("state must record the deferred replace")
	}
	// ...but a SUSTAINED failure does not re-fire every cycle.
	send5, _ := hlLiquidationShouldNotify(st4, hlLiquidationActionReplaceDeferred, base.Add(2*time.Minute))
	if send5 {
		t.Error("a sustained deferred replace must not re-fire every cycle")
	}

	// Deferred -> protection lost is an ESCALATION between two failure states.
	// A boolean "did it fail" could not see it; the action change must.
	send6, st6 := hlLiquidationShouldNotify(st4, hlLiquidationActionProtectionLost, base.Add(3*time.Minute))
	if !send6 {
		t.Fatal("deferred -> protection lost must notify immediately: the position went from protected to naked")
	}
	if st6.LastAction != hlLiquidationActionProtectionLost {
		t.Error("state must record the protection-lost action")
	}
	// And the recovery back to a landed clamp is reported too.
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
	// Clearing (condition healed, or the position closed) re-arms the first-fire.
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
		// scalar owner past liquidation -> tighten job
		{StrategyID: "b-scalar", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 11, StopLossTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: true, BookConsistent: true},
		// trailing owner past liquidation -> ALSO a tighten job. The walker only
		// runs for a due strategy whose signal check returned Signal == 0, so
		// deferring to it can leave the stop unreachable for a whole interval.
		{StrategyID: "a-trailing", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 12, StopLossTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: false, BookConsistent: true},
		// reachable geometry -> no action
		{StrategyID: "c-ok", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 13, StopLossTriggerPx: 2360, LiquidationPx: 2340.5, StaticScalarOwner: true, BookConsistent: true},
		// liquidation unknown -> no action
		{StrategyID: "d-unknown", Symbol: "BTC", Side: "long", Qty: 1, StopLossOID: 14, StopLossTriggerPx: 100, LiquidationPx: 0, StaticScalarOwner: true, BookConsistent: true},
		// nothing armed, non-scalar owner -> no action (its own path re-arms)
		{StrategyID: "e-nostop", Symbol: "BTC", Side: "long", Qty: 1, Unprotected: true, RearmTriggerPx: 39000, LiquidationPx: 40000, StaticScalarOwner: false, BookConsistent: true},
		// zero size -> no action
		{StrategyID: "f-noqty", Symbol: "BTC", Side: "long", Qty: 0, StopLossOID: 15, StopLossTriggerPx: 39000, LiquidationPx: 40000, StaticScalarOwner: true, BookConsistent: true},
	}
	actions := planHyperliquidLiquidationAudit(cands)
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want 2 (only the two past-liquidation candidates)", len(actions))
	}
	// Deterministic order by strategy id.
	if actions[0].Candidate.StrategyID != "a-trailing" || actions[1].Candidate.StrategyID != "b-scalar" {
		t.Fatalf("actions not sorted deterministically: %s, %s", actions[0].Candidate.StrategyID, actions[1].Candidate.StrategyID)
	}
	for _, a := range actions {
		if a.Kind != hlAuditTighten {
			t.Errorf("%s: kind = %v, want a tighten job — the audit heals every owner", a.Candidate.StrategyID, a.Kind)
		}
	}
	want := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(actions[1].ClampedTriggerPx, want) {
		t.Errorf("clamped trigger = %g, want %g", actions[1].ClampedTriggerPx, want)
	}
}

// #1450 review (3): a static scalar owner with NO resting stop is re-armed by
// the audit. Nothing else re-arms it — the walker needs a trailing distance,
// the fixed-ATR arm needs stop_loss_atr_mult, and the protection plan resolves
// to nothing for a scalar owner — so without this the position stays naked
// until it closes.
func TestPlanHyperliquidLiquidationAuditRearmsUnprotectedScalarOwner(t *testing.T) {
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
}

// #1450 review (optional 1): a coin whose recorded size across live strategies
// exceeds the on-chain snapshot carries a phantom position. Moving a
// reduce-only trigger there could close a PEER strategy's real size, so the
// audit refuses and reports instead.
func TestPlanHyperliquidLiquidationAuditRefusesUnreconciledCoin(t *testing.T) {
	cands := []hlLiquidationAuditCandidate{
		{StrategyID: "hl-eth", Symbol: "ETH", Side: "long", Qty: 1, StopLossOID: 11, StopLossTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: true, BookConsistent: false},
		{StrategyID: "hl-eth2", Symbol: "ETH", Side: "long", Qty: 1, Unprotected: true, RearmTriggerPx: 2325, LiquidationPx: 2340.5, StaticScalarOwner: true, BookConsistent: false},
	}
	for _, a := range planHyperliquidLiquidationAudit(cands) {
		if a.Kind != hlAuditRefuse {
			t.Errorf("%s: kind = %v, want a refusal on an unreconciled coin", a.Candidate.StrategyID, a.Kind)
		}
	}
}

func TestHLLiquidationCoinBookConsistent(t *testing.T) {
	got := hlLiquidationCoinBookConsistent(
		map[string]float64{"ETH": 2.0, "BTC": 1.0, "SOL": 0.5, "AVAX": 3.0, "DOGE": 5.0},
		map[string]int{"ETH": 2, "BTC": 2, "SOL": 1, "AVAX": 1, "DOGE": 1},
		map[string]float64{"ETH": 2.0, "BTC": 0.4, "AVAX": 2.0},
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
	// #1450 review round 2 (optional 1): a SOLE owner whose recorded size drifted
	// above the on-chain size (e.g. a manual partial TP) has no peer to harm.
	// hlSLEffectiveQty caps the placement to the confirmed on-chain quantity, so
	// the audit must still heal it rather than alert forever.
	if !got["AVAX"] {
		t.Error("AVAX: sole owner with virtual > on-chain — must stay actionable, sized to on-chain")
	}
	// A sole owner is not a licence to act on a coin with no on-chain backing:
	// there is no confirmed quantity to size a replacement from.
	if got["DOGE"] {
		t.Error("DOGE: sole owner but absent from the snapshot — must still refuse")
	}
}

// #1450 review round 2 (optional 1): the sole-owner route must reach the audit
// decision end-to-end, and a shared coin with a phantom must still refuse.
func TestCollectHLLiquidationAuditCandidatesHealsSoleOwnerDrift(t *testing.T) {
	strategies, state := liqAuditFixture(t, true, 3.0)
	// Manual partial TP: the book still records 1.0, the exchange shows 0.6.
	cands := collectHLLiquidationAuditCandidates(
		strategies, state,
		map[string]float64{"ETH": 2340.5},
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
	acts := planHyperliquidLiquidationAudit(cands)
	if len(acts) != 1 || acts[0].Kind != hlAuditTighten {
		t.Fatalf("actions = %+v, want one tighten", acts)
	}

	// Add a SECOND live owner on the same coin: now the drift could move a
	// reduce-only trigger against a peer, so the refusal comes back.
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
		map[string]float64{"ETH": 0.6},
		&sync.RWMutex{},
	)
	for _, c := range shared {
		if c.BookConsistent {
			t.Errorf("%s: a shared coin with a phantom must refuse", c.StrategyID)
		}
	}
}

// #1450 review round 2 (optional 2): a failed clearinghouseState fetch hands the
// audit empty maps. That is "no snapshot this cycle", never "every position is a
// phantom" — it must produce no action and no alert.
func TestRunHyperliquidLiquidationAuditSkipsWithoutSnapshot(t *testing.T) {
	strategies, state := liqAuditFixture(t, true, 3.0)
	// An unprotected static-scalar position: the shape that used to emit a
	// CRITICAL phantom alert with $0.0000 prices.
	state.Strategies["hl-eth"].Positions["ETH"].StopLossOID = 0
	state.Strategies["hl-eth"].Positions["ETH"].StopLossTriggerPx = 0
	calls := 0
	orig := runHyperliquidUpdateStopLossFunc
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, qty, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7, StopLossTriggerPx: triggerPx}, "", nil
	}
	defer func() { runHyperliquidUpdateStopLossFunc = orig }()
	hlLiquidationAlerts.Delete(hlLiquidationAlertKey("hl-eth", "ETH"))

	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{}, map[string]float64{}, false, &sync.RWMutex{}, nil, time.Now().UTC())
	if res.ImmediateFills != 0 || len(res.CloseDetails) != 0 {
		t.Errorf("result = %+v, want an empty result with no snapshot", res)
	}
	if calls != 0 {
		t.Errorf("placement calls = %d, want 0 with no snapshot", calls)
	}
	if _, alerted := hlLiquidationAlerts.Load(hlLiquidationAlertKey("hl-eth", "ETH")); alerted {
		t.Error("a failed fetch must not raise a phantom-position alert")
	}
}

// #1450 review round 2 (optional 2): an audit refusal on a position that carries
// NO stop must say so, never quote a $0.0000 trigger past a $0.0000 liquidation
// price.
func TestHLLiquidationAlertMessageOmitsUnknownGeometry(t *testing.T) {
	headline, detail, unprotected := hlLiquidationAlertMessage(0, 0, 0, hlLiquidationActionUnreconciled)
	if strings.Contains(detail, "$0.0000") {
		t.Errorf("detail = %q, want no fabricated $0.0000 prices", detail)
	}
	if !strings.Contains(detail, "NO exchange-side stop") {
		t.Errorf("detail = %q, want it to say the position has no stop", detail)
	}
	if headline != "**HL POSITION UNPROTECTED**" {
		t.Errorf("headline = %q, want the unprotected headline", headline)
	}
	if !unprotected {
		t.Error("a refused candidate with no stop is unprotected — must log CRITICAL")
	}
	// A real geometry still prints both prices.
	_, armedDetail, armedUnprotected := hlLiquidationAlertMessage(2325, 2352, 2340.5, hlLiquidationActionUnreconciled)
	if !strings.Contains(armedDetail, "$2325.0000") || !strings.Contains(armedDetail, "$2340.5000") {
		t.Errorf("detail = %q, want the real trigger and liquidation prices", armedDetail)
	}
	if armedUnprotected {
		t.Error("a refused TIGHTEN still has the old order resting — not unprotected")
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

	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if res.ImmediateFills != 0 {
		t.Fatalf("immediate fills = %d, want 0 (a resting replacement)", res.ImmediateFills)
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

// (b) THE acceptance test: when the replace fails BEFORE the cancel lands, the
// position must still end the cycle with its ORIGINAL armed exchange-side stop.
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
		// The cancel never ran (no cancel_stop_loss_succeeded), so the old
		// trigger is untouched even though the placement was rejected.
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
		runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
		pos := state.Strategies["hl-eth"].Positions["ETH"]
		if pos.StopLossOID != 4242 || !approxEqLiq(pos.StopLossTriggerPx, 2325) {
			t.Fatalf("failure %+v: position must keep its ORIGINAL armed stop, got oid=%d trigger=%g", f, pos.StopLossOID, pos.StopLossTriggerPx)
		}
	}
}

// #1450 review (1a): a cancel that LANDS followed by a placement that does NOT
// rest leaves the position naked. It must never read as a successful clamp, the
// state must stop pointing at the dead OID, and the very next audit must
// re-place a stop rather than skipping the position forever.
func TestRunHyperliquidLiquidationAuditCancelledThenRejectedRearms(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	liq := map[string]float64{"ETH": 2340.5}
	onChain := map[string]float64{"ETH": 1.0}

	// Cycle 1: cancel lands, the open-order cap rejects the replacement.
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded: true,
			StopLossError:           "Order would exceed the open order limit",
			StopLossTriggerPx:       triggerPx,
		}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, liq, onChain, true, &mu, nil, time.Now().UTC())

	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Fatalf("state must record that the stop is GONE, got oid=%d trigger=%g", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	// Cycle 2: the position is now an unprotected static scalar owner, so the
	// audit re-arms it at the configured distance, clamped inside liquidation.
	var gotTrigger float64
	var gotCancelOID int64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotTrigger, gotCancelOID = triggerPx, cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 7777, StopLossTriggerPx: triggerPx}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, liq, onChain, true, &mu, nil, time.Now().UTC())

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

// #1450 review (1b): cancel + successful placement is a normal clamp and must
// NOT report protection lost.
func TestRunHyperliquidLiquidationAuditCancelThenPlaceIsANormalClamp(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true, StopLossOID: 5150, StopLossTriggerPx: triggerPx}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if got := state.Strategies["hl-eth"].Positions["ETH"].StopLossOID; got != 5150 {
		t.Fatalf("position SL oid = %d, want the replacement 5150", got)
	}
	st, _ := hlLiquidationAlerts.Load(hlLiquidationAlertKey("hl-eth", "ETH"))
	if s, ok := st.(hlLiquidationAlertState); !ok || s.LastAction != hlLiquidationActionClamped {
		t.Errorf("last action = %+v, want a landed clamp — no false protection-lost alert", st)
	}
	clearHLLiquidationAlert("hl-eth", "ETH")
}

// #1456 review (2a): an audit clamp that FILLS at submit reports the position
// as EXITED — never "tightened to $X" for a position that ended the cycle flat.
func TestRunHyperliquidLiquidationAuditFillAtSubmitReportsExited(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded:   true,
			StopLossFilledImmediately: true,
			StopLossTriggerPx:         triggerPx,
		}, "", nil
	}
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if res.ImmediateFills != 1 || len(res.CloseDetails) != 1 {
		t.Fatalf("immediate fills = %d, close details = %d, want 1/1", res.ImmediateFills, len(res.CloseDetails))
	}
	st, _ := hlLiquidationAlerts.Load(hlLiquidationAlertKey("hl-eth", "ETH"))
	if s2, ok := st.(hlLiquidationAlertState); !ok || s2.LastAction != hlLiquidationActionExited {
		t.Errorf("last action = %+v, want %q — the position is FLAT", st, hlLiquidationActionExited)
	}
}

// #1456 review (4): the close the audit books names the AUDIT mechanism, so the
// persisted CloseReason matches the LIQUIDATION-CLAMP operator DM.
func TestRunHyperliquidLiquidationAuditImmediateCloseReasonsTheAudit(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded:   true,
			StopLossFilledImmediately: true,
			StopLossTriggerPx:         triggerPx,
		}, "", nil
	}
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if res.ImmediateFills != 1 || len(res.CloseDetails) != 1 {
		t.Fatalf("immediate fills = %d, close details = %d, want 1/1", res.ImmediateFills, len(res.CloseDetails))
	}
	ss := state.Strategies["hl-eth"]
	if len(ss.ClosedPositions) == 0 {
		t.Fatal("no closed position recorded for the immediate close")
	}
	cp := ss.ClosedPositions[len(ss.ClosedPositions)-1]
	if cp.CloseReason != "liquidation_clamp_sl_immediate" {
		t.Errorf("persisted CloseReason = %q, want liquidation_clamp_sl_immediate (not the trailing walker)", cp.CloseReason)
	}
	if len(ss.TradeHistory) == 0 {
		t.Fatal("no trade recorded for the immediate close")
	}
	tr := ss.TradeHistory[len(ss.TradeHistory)-1]
	if !strings.Contains(tr.Details, "Liquidation-clamp SL") {
		t.Errorf("trade details %q must match the LIQUIDATION-CLAMP operator wording", tr.Details)
	}
}

// #1456 review (2c): an audit replace whose OLD stop already fired on-chain is
// reported as SL filled — never as "replace deferred; original still resting".
func TestRunHyperliquidLiquidationAuditFilledExternallyReportsFilled(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	defer clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossFilledExternally: true}, "", nil
	}
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if res.ImmediateFills != 0 {
		t.Fatalf("the reconciler owns this close, audit booked %d", res.ImmediateFills)
	}
	st, _ := hlLiquidationAlerts.Load(hlLiquidationAlertKey("hl-eth", "ETH"))
	if s2, ok := st.(hlLiquidationAlertState); !ok || s2.LastAction != hlLiquidationActionFilledOnChain {
		t.Errorf("last action = %+v, want %q", st, hlLiquidationActionFilledOnChain)
	}
}

// #1450 review (1c): a live perps static scalar owner cannot scale in at all
// (config.go rejects allow_scale_in for a non-resizable SL owner), so the
// re-arm can never race an add and double-arm.
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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if called {
		t.Fatal("a paper strategy must never be clamped against a live liquidation price")
	}
	if state.Strategies["hl-eth"].Positions["ETH"].StopLossOID != 4242 {
		t.Error("paper position state must be untouched")
	}
}

// #1450 review (3): the trailing walker runs only inside the dispatch over DUE
// strategies and only when that cycle's signal check returned Signal == 0, so
// the audit must heal a trailing owner itself instead of announcing a repair
// that will not happen for up to a full strategy interval.
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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())

	wantTrigger := 2340.5 * (1 + hlLiquidationStopBufferPct/100.0)
	if !approxEqLiq(gotTrigger, wantTrigger) || gotCancelOID != 4242 {
		t.Fatalf("audit must tighten the trailing owner's resting stop, got trigger=%g cancel_oid=%d", gotTrigger, gotCancelOID)
	}
	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 9100 || !approxEqLiq(pos.StopLossTriggerPx, wantTrigger) {
		t.Errorf("position must carry the tightened stop, got oid=%d trigger=%g", pos.StopLossOID, pos.StopLossTriggerPx)
	}
	// Idempotent: the tightened trigger is no longer past liquidation, so a
	// second audit in the same conditions places nothing.
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9101, StopLossTriggerPx: triggerPx}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if calls != 0 {
		t.Errorf("a reachable resting trigger must produce no further order churn, got %d placements", calls)
	}
}

// #1450 review (3b/3c): the audit never depends on a signal result, so a
// strategy whose signal check errored or that executed no trade is healed on
// exactly the same cycle. The audit runs pre-dispatch over cfg.Strategies, so
// this is structural — the test pins the trailing owner being healed with no
// dispatch involvement at all.
func TestRunHyperliquidLiquidationAuditIsIndependentOfSignalResults(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 0)
	strategies[0].StopLossPct = nil
	strategies[0].TrailingStopATRMult = floatPtr(2.5)
	// A 4h strategy that is not due this cycle still reaches the audit.
	strategies[0].Args = []string{"x.py", "ETH", "4h", "--mode=live"}
	var mu sync.RWMutex
	placed := false
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		placed = true
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9200, StopLossTriggerPx: triggerPx}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if !placed {
		t.Fatal("a non-due 4h trailing owner must still be healed by the per-cycle audit")
	}
}

// #1450 review (optional 1a): a phantom position on a shared coin must never
// drive a cancel+replace — the reduce-only trigger it rests could close the
// PEER strategy's real size.
func TestRunHyperliquidLiquidationAuditRefusesPhantomOnSharedCoin(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")
	clearHLLiquidationAlert("hl-eth-b", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	// A peer on the same coin, so the recorded total is 2.0 against 1.0
	// on-chain: one of the two positions no longer exists.
	scB := strategies[0]
	scB.ID = "hl-eth-b"
	strategies = append(strategies, scB)
	state.Strategies["hl-eth-b"] = &StrategyState{ID: "hl-eth-b", Platform: "hyperliquid", Type: "perps", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Side: "long", Quantity: 1.0, AvgCost: 2400, RiskAnchorPrice: 2400, EntryATR: 30, StopLossOID: 4343, StopLossTriggerPx: 2325},
	}}
	var mu sync.RWMutex
	called := false
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		return &HyperliquidStopLossUpdateResult{StopLossOID: 1}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if called {
		t.Fatal("the audit must not move a reduce-only trigger on a coin whose book exceeds the on-chain snapshot")
	}
	if state.Strategies["hl-eth"].Positions["ETH"].StopLossOID != 4242 {
		t.Error("a refused candidate's state must be untouched")
	}
}

// #1450 review (optional 1b): a non-due strategy with a genuinely open,
// consistent position is still healed — the refusal is about book drift, not
// about due-ness.
func TestRunHyperliquidLiquidationAuditHealsConsistentNonDuePosition(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	strategies[0].Args = []string{"x.py", "ETH", "1d", "--mode=live"}
	var mu sync.RWMutex
	called := false
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9300, StopLossTriggerPx: triggerPx}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if !called {
		t.Fatal("a consistent book must still be healed regardless of due-ness")
	}
}

// #1450 review (optional 1c): a coin with no on-chain position at all is a
// no-op — nothing backs the recorded size.
func TestRunHyperliquidLiquidationAuditNoOpsWithoutOnChainPosition(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	called := false
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		return &HyperliquidStopLossUpdateResult{StopLossOID: 1}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{}, true, &mu, nil, time.Now().UTC())
	if called {
		t.Fatal("no on-chain position for the coin must be a no-op")
	}
}

// #1450 review (optional 2): a position that exits on an audit-placed clamped
// stop yields an operator-facing close line, so the main loop can raise the
// same trade notification every other close path produces.
func TestRunHyperliquidLiquidationAuditReportsBookedCloses(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: triggerPx}, "", nil
	}
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if res.ImmediateFills != 1 {
		t.Fatalf("immediate fills = %d, want 1", res.ImmediateFills)
	}
	if len(res.CloseDetails) != 1 {
		t.Fatalf("close details = %d, want 1 — a booked close must reach the trade notifier", len(res.CloseDetails))
	}
	cd := res.CloseDetails[0]
	if cd.SC.ID != "hl-eth" || cd.Symbol != "ETH" || cd.Detail == "" {
		t.Errorf("close detail = %+v, want a populated per-strategy line", cd)
	}
	if _, still := state.Strategies["hl-eth"].Positions["ETH"]; still {
		t.Error("an immediate fill must book the close and drop the position")
	}
}

// A clamp that rests normally must NOT emit a close notification.
func TestRunHyperliquidLiquidationAuditRestingClampEmitsNoClose(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9400, StopLossTriggerPx: triggerPx}, "", nil
	}
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if len(res.CloseDetails) != 0 || res.ImmediateFills != 0 {
		t.Fatalf("a resting clamp must produce no close notification, got %+v", res)
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

	// #1456 review: trailing_stop_pct is EXCLUDED — its anchor ratchets with
	// the mark, so the entry-anchored bound is not exact for it and the
	// runtime clamp handles the pre-move window. It must load.
	sc = base()
	sc.TrailingStopPct = floatPtr(10)
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("trailing_stop_pct 10 @ 20x must NOT be rejected (anchor ratchets), got %v", errs)
	}

	// #1456 review: the MaxDrawdownPct fallback is entry-anchored and IS
	// covered when it owns the stop.
	sc = base()
	sc.MaxDrawdownPct = 15
	errs := validateHLStopWithinBankruptcyBound(sc)
	if len(errs) != 1 || !strings.Contains(errs[0], "max_drawdown_pct") {
		t.Errorf("max_drawdown_pct fallback 15 @ 20x must be rejected, got %v", errs)
	}
	// EffectiveStopLossPct caps the fallback at MaxAutoStopLossPct (50); the
	// check compares the RESOLVED value, so 90 @ 20x rejects via its cap.
	sc = base()
	sc.MaxDrawdownPct = 90
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 1 {
		t.Errorf("max_drawdown_pct 90 @ 20x resolves through its 50%% cap to a rejection, got %v", errs)
	}

	// The fallback check does not fire when an explicit owner wins instead.
	sc = base()
	sc.StopLossPct = floatPtr(4.9)
	sc.MaxDrawdownPct = 15
	if errs := validateHLStopWithinBankruptcyBound(sc); len(errs) != 0 {
		t.Errorf("explicit stop_loss_pct owns the stop; max_drawdown_pct must not be checked, got %v", errs)
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
