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
	// #1456 review round 8: SIGNED comparison. A documented legal long+short
	// book nets to the reported figure; only a same-side excess is drift.
	if !got["MIX"] {
		t.Error("MIX: signed net -0.6 vs on-chain 0.6 across two owners — consistent")
	}
	if got["PHANTOM"] {
		t.Error("PHANTOM: signed net 1.1 exceeds on-chain 0.6 — real drift")
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
	// #1456 review round 13 (optional 3): the cap must be visible so the
	// execution site can log it like every sibling hlSLEffectiveQty caller.
	if !cands[0].QtyCapped || math.Abs(cands[0].VirtualQty-1.0) > 1e-9 {
		t.Errorf("QtyCapped/VirtualQty = %v/%v, want true/1.0", cands[0].QtyCapped, cands[0].VirtualQty)
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

	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{}, nil, map[string]float64{}, false, &sync.RWMutex{}, nil, time.Now().UTC())
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
	headline, detail, unprotected := hlLiquidationAlertMessage(0, 0, 0, hlLiquidationActionUnreconciled, "")
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
	_, armedDetail, armedUnprotected := hlLiquidationAlertMessage(2325, 2352, 2340.5, hlLiquidationActionUnreconciled, "")
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

	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
		runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
		pos := state.Strategies["hl-eth"].Positions["ETH"]
		if pos.StopLossOID != 4242 || !approxEqLiq(pos.StopLossTriggerPx, 2325) {
			t.Fatalf("failure %+v: position must keep its ORIGINAL armed stop, got oid=%d trigger=%g", f, pos.StopLossOID, pos.StopLossTriggerPx)
		}
	}
}

// hlNetSideByCoinAllLong marks every coin's on-chain NET side as "long" — the
// common fixture shape (long ETH positions whose liquidation price is quoted
// below the mark).
func hlNetSideByCoinAllLong() map[string]string {
	return map[string]string{"ETH": "long"}
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
	runHyperliquidLiquidationAudit(strategies, state, liq, hlNetSideByCoinAllLong(), onChain, true, &mu, nil, time.Now().UTC())

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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())

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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{}, true, &mu, nil, time.Now().UTC())
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
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
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
	res := runHyperliquidLiquidationAudit(strategies, state, map[string]float64{"ETH": 2340.5}, hlNetSideByCoinAllLong(), map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if len(res.CloseDetails) != 0 || res.ImmediateFills != 0 {
		t.Fatalf("a resting clamp must produce no close notification, got %+v", res)
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

// --- #1456 review round 5: the liquidation price is a NET per-coin figure ----

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

// Must-survive (a): a shared coin holding a long and a short peer — the net is
// long, so the reported liquidation price describes A's leg only. B's clamp is
// skipped; A's still applies.
func TestCollectHLLiquidationAuditCandidatesSideMismatchSkipsOppositeLeg(t *testing.T) {
	strategies, state := liqAuditFixture(t, true, 3.125) // long 1.0, trigger 2325 (past liq 2340.5)
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
	// On-chain net is long 0.6 (1.6 recorded long legs? no: 1.0 long - 0.4
	// short) — the REALISTIC figure main.go computes from |net signed size|.
	// Round 8: this used to pass the summed 1.4, which let the magnitude-summing
	// book check refuse the coin and mask the whole scenario.
	cands := collectHLLiquidationAuditCandidates(
		append(strategies, shortPeer), state,
		map[string]float64{"ETH": 2340.5},
		map[string]string{"ETH": "long"}, // on-chain NET is long (+1.0 - 0.4)
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

	// Must-survive (b): a genuinely phantom SAME-side third peer (a stale long
	// 0.5 that is actually closed) pushes the signed net past the reported size
	// — the refusal must come back for the whole coin.
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
		map[string]float64{"ETH": 0.6}, // signed net now 1.1 > 0.6: real drift
		&sync.RWMutex{},
	)
	for _, c := range phantom {
		if c.BookConsistent {
			t.Errorf("%s: a phantom same-side peer is real drift and must refuse", c.StrategyID)
		}
	}
}

// Must-survive (b): a sole owner whose recorded side is stale OPPOSITE the
// on-chain net gets no cancel, no placement, no alert — the same treatment as
// an unknown liquidation price.
func TestRunHyperliquidLiquidationAuditStaleSideSoleOwnerDoesNothing(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	var mu sync.RWMutex
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9001, StopLossTriggerPx: triggerPx}, "", nil
	}

	res := runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{"ETH": 2340.5},
		map[string]string{"ETH": "short"}, // stale/opposite net side
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if calls != 0 {
		t.Errorf("placement calls = %d, want 0 — a side mismatch must never touch the resting order", calls)
	}
	if res.ImmediateFills != 0 || len(res.CloseDetails) != 0 {
		t.Errorf("result = %+v, want empty", res)
	}
	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 4242 || pos.StopLossTriggerPx != 2325 {
		t.Errorf("state changed: oid=%d trigger=%g — the healthy stop must be untouched", pos.StopLossOID, pos.StopLossTriggerPx)
	}
	if _, alerted := hlLiquidationAlerts.Load(hlLiquidationAlertKey("hl-eth", "ETH")); alerted {
		t.Error("a side mismatch must not raise a past-liquidation alert")
	}
}

// The protection sync inherits the same gate: a position whose side disagrees
// with the on-chain net keeps pre-#1450 plan behavior (no clamp rewrite, no
// force-replace from past-liquidation geometry); the matching side still heals.
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
	liq := map[string]float64{"ETH": 2800} // derived trigger 2800 sits AT liquidation
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

// --- #1456 review round 6 ----------------------------------------------------

// Optional 1: the boot bound must not score pct fields that resolve as NOTHING
// (EffectiveStopLossPct returns 0 under a unified regime close). Scope
// correction over the finding: validateUnifiedCloseSoleOwner ALREADY rejects
// stop_loss_pct / stop_loss_margin_pct alongside a unified close
// (regime_unified.go), so the combination can never boot — the gate here fixes
// the MISLEADING ERROR LINE, not a boot regression. Must-survive (c): without
// a unified close both fields are still scored.
func TestHLStopBankruptcyBoundSkipsInertPctFieldsUnderUnifiedClose(t *testing.T) {
	base := func(unified bool) StrategyConfig {
		sc := StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Script: "x.py", Args: []string{"x.py", "ETH", "1h", "--mode=live"},
			StopLossPct:       floatPtr(10),
			StopLossMarginPct: floatPtr(200), // 200/20 = 10% >= the 5% bound at 20x
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

// Optional 2 (a): an UNPROTECTED static-scalar owner whose recorded side is
// stale opposite the on-chain net gets no placement and no per-cycle CRITICAL —
// the re-arm needs this cycle's snapshot to confirm the net is on the recorded
// side.
func TestAuditRearmSkipsStaleSideSoleOwner(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	state.Strategies["hl-eth"].Positions["ETH"].StopLossOID = 0
	state.Strategies["hl-eth"].Positions["ETH"].StopLossTriggerPx = 0 // unprotected
	var mu sync.RWMutex
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9001, StopLossTriggerPx: triggerPx}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{"ETH": 2340.5},
		map[string]string{"ETH": "short"}, // net OPPOSITE the recorded long
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if calls != 0 {
		t.Errorf("re-arm placements = %d, want 0 against an unconfirmed side", calls)
	}
	if _, alerted := hlLiquidationAlerts.Load(hlLiquidationAlertKey("hl-eth", "ETH")); alerted {
		t.Error("a skipped stale-side re-arm must not raise the unthrottled CRITICAL")
	}
}

// Optional 2 (b): a MATCHING side with no liquidation price reported must STILL
// re-arm — the re-arm places at the configured distance and does not depend on
// liquidation geometry.
func TestAuditRearmProceedsWithMatchingSideAndUnknownGeometry(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	state.Strategies["hl-eth"].Positions["ETH"].StopLossOID = 0
	state.Strategies["hl-eth"].Positions["ETH"].StopLossTriggerPx = 0
	var mu sync.RWMutex
	var gotTrigger float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotTrigger = triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 9001, StopLossTriggerPx: triggerPx}, "", nil
	}
	// Price map EMPTY (no liquidation price reported); the net side is still
	// known from the snapshot's size sign.
	runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{},
		map[string]string{"ETH": "long"},
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	want := 2400 * (1 - 0.03125) // configured scalar distance off the frozen anchor
	if !approxEqLiq(gotTrigger, want) {
		t.Errorf("re-arm trigger = %g, want the unclamped configured distance %g", gotTrigger, want)
	}
}

// liqTrailingAuditFixture is liqAuditFixture with a TRAILING stop owner instead
// of a static scalar — the owner class whose recovery used to wait for its own
// next due cycle after an audit cancel stripped the stop.
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

// Optional 3: when an audit tighten CANCELS a trailing owner's stop and the
// replacement does not rest, the audit retries the placement once in the SAME
// cycle (fresh, nothing to cancel) — it may not hand a window it created to a
// path running on the strategy's slower cadence. A retry that rests updates
// state and reports a clamp; a retry that also fails reports protection lost
// once; a first-try rest stays a single call.
func TestAuditRetriesPlacementItStrippedSameCycle(t *testing.T) {
	for _, tc := range []struct {
		name          string
		retryOK       bool
		wantCalls     int
		wantSecondOID int64
		wantFinalOID  int64
		wantAction    hlLiquidationAlertAction
	}{
		{"retry rests", true, 2, 0, 9002, hlLiquidationActionClamped},
		{"retry also fails", false, 2, 0, 0, hlLiquidationActionProtectionLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := runHyperliquidUpdateStopLossFunc
			defer func() { runHyperliquidUpdateStopLossFunc = old }()
			clearHLLiquidationAlert("hl-eth", "ETH")

			strategies, state := liqTrailingAuditFixture(t)
			var mu sync.RWMutex
			type call struct {
				cancelOID int64
				trigger   float64
			}
			var calls []call
			callN := 0
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				callN++
				calls = append(calls, call{cancelOID: cancelOID, trigger: triggerPx})
				if callN == 1 {
					// Cancel lands, the open-order cap rejects the replacement.
					return &HyperliquidStopLossUpdateResult{
						CancelStopLossSucceeded: true,
						StopLossError:           "Order would exceed the open order limit",
					}, "", nil
				}
				if tc.retryOK {
					return &HyperliquidStopLossUpdateResult{StopLossOID: 9002, StopLossTriggerPx: triggerPx}, "", nil
				}
				return &HyperliquidStopLossUpdateResult{
					CancelStopLossSucceeded: false,
					StopLossError:           "Order would exceed the open order limit",
				}, "", nil
			}

			runHyperliquidLiquidationAudit(strategies, state,
				map[string]float64{"ETH": 2340.5},
				hlNetSideByCoinAllLong(),
				map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())

			if len(calls) != tc.wantCalls {
				t.Fatalf("placement calls = %d (%+v), want %d", len(calls), calls, tc.wantCalls)
			}
			if tc.wantCalls == 2 && calls[1].cancelOID != tc.wantSecondOID {
				t.Errorf("retry cancelOID = %d, want %d (nothing left to cancel)", calls[1].cancelOID, tc.wantSecondOID)
			}
			pos := state.Strategies["hl-eth"].Positions["ETH"]
			if pos.StopLossOID != tc.wantFinalOID {
				t.Errorf("final oid = %d, want %d", pos.StopLossOID, tc.wantFinalOID)
			}
			if last := lastLiqAlertAction("hl-eth", "ETH"); last != tc.wantAction {
				t.Errorf("alert action = %q, want %q", last, tc.wantAction)
			}
		})
	}

	// (c) A normal clamp that rests FIRST time: no retry, single call.
	t.Run("first-try rest takes no retry", func(t *testing.T) {
		old := runHyperliquidUpdateStopLossFunc
		defer func() { runHyperliquidUpdateStopLossFunc = old }()
		clearHLLiquidationAlert("hl-eth", "ETH")

		strategies, state := liqTrailingAuditFixture(t)
		var mu sync.RWMutex
		calls := 0
		runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			calls++
			return &HyperliquidStopLossUpdateResult{StopLossOID: 9001, StopLossTriggerPx: triggerPx}, "", nil
		}
		runHyperliquidLiquidationAudit(strategies, state,
			map[string]float64{"ETH": 2340.5},
			hlNetSideByCoinAllLong(),
			map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
		if calls != 1 {
			t.Errorf("placement calls = %d, want 1", calls)
		}
	})
}

// #1456 review round 13 (optional 4): the off-cycle audit's cadence comes from
// the fastest LIVE HL perps interval — never from an unrelated platform's or
// paper strategy's slower/faster clock, and 0 with no such strategy.
func TestLiquidationAuditIntervalSeconds(t *testing.T) {
	mk := func(id string, platform, typ string, live bool) StrategyConfig {
		args := []string{"--mode", "paper"}
		if live {
			args = []string{"--mode", "live"}
		}
		return StrategyConfig{ID: id, Platform: platform, Type: typ, Args: args}
	}
	strategies := []StrategyConfig{
		mk("hl-live-a", "hyperliquid", "perps", true),
		mk("hl-paper-b", "hyperliquid", "perps", false),
		mk("okx-live-c", "okx", "perps", true),
		mk("hl-live-spot", "hyperliquid", "spot", true),
	}
	intervals := map[string]int{"hl-live-a": 300, "hl-paper-b": 60, "okx-live-c": 120, "hl-live-spot": 30}
	// #1456 review round 14 (Optional 1): HALF the minimum, not the minimum.
	// Returning 300 made the audit deadline elapse at the same instant
	// hl-live-a became due, so the off-cycle pass could never fire earlier
	// than the dispatch-path audit it exists to pre-empt.
	if got := liquidationAuditIntervalSeconds(strategies, intervals); got != 150 {
		t.Errorf("interval = %d, want 150 (half of hl-live-a's 300)", got)
	}
	if got := liquidationAuditIntervalSeconds(strategies[1:], intervals); got != 0 {
		t.Errorf("interval without any live HL perps or manual = %d, want 0", got)
	}
}

// #1456 review round 14 (Optional 1): the cadence must be strictly shorter
// than the interval bounding the healing window, must never fall below the
// endpoint-protection floor, and must cover live HL `manual` — which
// collectHLLiquidationAuditCandidates audits but the original population
// filter excluded, leaving a manual-only fleet with no off-cycle pass at all.
func TestLiquidationAuditIntervalStrictlyShorterAndCoversManual(t *testing.T) {
	live := func(id, typ string) StrategyConfig {
		return StrategyConfig{ID: id, Platform: "hyperliquid", Type: typ, Args: []string{"--mode", "live"}}
	}

	// (a) one live HL perps strategy at 4h — audited materially sooner than 4h.
	perps4h := []StrategyConfig{live("hl-4h", "perps")}
	iv4h := map[string]int{"hl-4h": 14400}
	got := liquidationAuditIntervalSeconds(perps4h, iv4h)
	if got != 7200 {
		t.Errorf("4h fleet cadence = %d, want 7200", got)
	}
	if got >= 14400 {
		t.Errorf("cadence %d is not strictly shorter than the 14400s interval it bounds", got)
	}

	// (b) live HL manual-only fleet — still gets an off-cycle pass.
	manualOnly := []StrategyConfig{live("hl-manual", "manual")}
	if got := liquidationAuditIntervalSeconds(manualOnly, map[string]int{"hl-manual": 3600}); got != 1800 {
		t.Errorf("manual-only cadence = %d, want 1800 (manual is an audit candidate)", got)
	}

	// (c) a fast fleet floors at liquidationAuditMinIntervalSeconds so halving
	// can never turn the pass into a hot poll against Hyperliquid.
	if got := liquidationAuditIntervalSeconds(perps4h, map[string]int{"hl-4h": 30}); got != liquidationAuditMinIntervalSeconds {
		t.Errorf("fast-fleet cadence = %d, want floor %d", got, liquidationAuditMinIntervalSeconds)
	}

	// (d) a PAPER manual strategy is still excluded — the audit only acts on
	// live positions.
	paperManual := []StrategyConfig{{ID: "hl-pm", Platform: "hyperliquid", Type: "manual", Args: []string{"--mode", "paper"}}}
	if got := liquidationAuditIntervalSeconds(paperManual, map[string]int{"hl-pm": 3600}); got != 0 {
		t.Errorf("paper manual cadence = %d, want 0", got)
	}
}

// #1456 review round 13: the extracted map builder must reproduce the
// dispatch-path semantics — dust omitted entirely (no side stamp, no cap
// entry), positive-only liquidation prices, net side from Size sign.
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

// #1456 review round 14 (Needs Fixing 1): the off-cycle audit branch continues
// past the loop's only end-of-cycle save, so a close it books must be flushed
// inline. Pins the three states the reviewer named: a booked close survives the
// restart, a pass that books nothing writes nothing, and a save failure is
// reported through the same saveFailures counter as the end-of-cycle failure
// (and retried on the next quiet wake rather than being swallowed).
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
		db.Close() // force SaveStateWithDB to fail
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

// #1456 review round 15 (Needs Fixing 1): the audit's NORMAL outcome is a
// resting replacement, not a fill. applyAuditStopUpdate must count every write
// to persisted protection state so the off-cycle flush fires for a clamp or
// re-arm that closed nothing — and must count nothing when the call changed
// nothing, so a quiet pass still writes nothing.
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

		// nil update — the helper returns before touching anything.
		if _, _ = applyAuditStopUpdate(&res, ss, "ETH", "long", 111, 1.0, nil, logger); res.StateMutations != 0 {
			t.Errorf("StateMutations = %d after a nil update, want 0", res.StateMutations)
		}
		// side mismatch — re-validated away inside applyTrailingStopUpdateResult.
		if _, _ = applyAuditStopUpdate(&res, ss, "ETH", "short", 111, 1.0, &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: 1900}, logger); res.StateMutations != 0 {
			t.Errorf("StateMutations = %d after a side mismatch, want 0", res.StateMutations)
		}
		// a cancel whose prevSLOID no longer matches — no branch applies.
		if _, _ = applyAuditStopUpdate(&res, ss, "ETH", "long", 42, 1.0, &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}, logger); res.StateMutations != 0 {
			t.Errorf("StateMutations = %d after a stale-OID cancel, want 0", res.StateMutations)
		}
		if pos := ss.Positions["ETH"]; pos.StopLossOID != 111 || pos.StopLossTriggerPx != 1850 {
			t.Errorf("no-op calls mutated the position: OID %d trigger %.2f", pos.StopLossOID, pos.StopLossTriggerPx)
		}
	})
}

// #1456 review round 16 (Needs Fixing 1): the outcome-unknown shape must never
// be reported as "protection lost". That message asserts the position has NO
// exchange-side stop and its recovery sentence promises a re-place — both
// wrong when the replacement may be resting untracked.
func TestOutcomeUnknownIsNotProtectionLost(t *testing.T) {
	if hlLiquidationActionUnprotected(hlLiquidationActionOutcomeUnknown) {
		t.Errorf("outcome-unknown counted as unprotected — the operator is told a fact nobody measured")
	}
	if !hlLiquidationActionUnprotected(hlLiquidationActionProtectionLost) {
		t.Errorf("protection lost must still count as unprotected")
	}

	headline, detail, unprotected := hlLiquidationAlertMessage(1800, 1900, 1750, hlLiquidationActionOutcomeUnknown, "recovery")
	if unprotected {
		t.Errorf("outcome-unknown message flagged unprotected")
	}
	if !strings.Contains(headline, "OUTCOME UNKNOWN") {
		t.Errorf("headline = %q, want an outcome-unknown headline", headline)
	}
	for _, want := range []string{"could NOT be read", "may be resting", "KEPT"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail = %q, want it to contain %q", detail, want)
		}
	}
	if strings.Contains(detail, "no exchange-side stop right now") {
		t.Errorf("detail asserts the position is unprotected: %q", detail)
	}

	// The retry gate is unchanged: only a positively rejected placement retries.
	if hlLiquidationMayRetryReplace(&HyperliquidStopLossUpdateResult{StopLossOutcomeUnknown: true}) {
		t.Errorf("outcome-unknown must not license an in-cycle retry")
	}
	if !hlLiquidationMayRetryReplace(&HyperliquidStopLossUpdateResult{}) {
		t.Errorf("a positively rejected placement must still retry")
	}
}

// #1456 review round 17 (Needs Fixing 1): a placement must be classified by
// what actually RESTS, never by accompanying error text. Python resolves an
// unreadable submission by open-order book diff and emits stop_loss_error
// TOGETHER with a resolved stop_loss_oid or stop_loss_outcome_unknown.
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

// #1456 review round 17 (Needs Fixing 1) must-survive (a): a clamp retry whose
// response was unreadable but whose open-order diff resolved one fresh oid is
// adopted as a normal clamp — OID recorded, no protection-lost alert, so the
// next cycle cannot re-arm a second full-size reduce-only stop.
func TestAuditRetryAdoptsBookDiffResolvedOID(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqTrailingAuditFixture(t)
	var mu sync.RWMutex
	callN := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		callN++
		if callN == 1 {
			// Cancel lands, the open-order cap rejects the replacement.
			return &HyperliquidStopLossUpdateResult{
				CancelStopLossSucceeded: true,
				StopLossError:           "Order would exceed the open order limit",
			}, "", nil
		}
		// The retry's submission was unreadable but the book diff resolved it:
		// error text AND a resting oid travel together.
		return &HyperliquidStopLossUpdateResult{
			StopLossError:     "place_stop_loss returned no usable status: {...}",
			StopLossOID:       9002,
			StopLossTriggerPx: triggerPx,
		}, "", nil
	}

	res := runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{"ETH": 2340.5},
		hlNetSideByCoinAllLong(),
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())

	if callN != 2 {
		t.Errorf("placement calls = %d, want 2 (clamp + in-cycle retry)", callN)
	}
	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 9002 {
		t.Errorf("final oid = %d, want 9002 (resolved retry adopted)", pos.StopLossOID)
	}
	if last := lastLiqAlertAction("hl-eth", "ETH"); last != hlLiquidationActionClamped {
		t.Errorf("alert action = %q, want %q (no protection-lost report)", last, hlLiquidationActionClamped)
	}
	if res.StateMutations < 1 {
		t.Errorf("state mutations = %d, want >= 1 (oid rewrite must flush)", res.StateMutations)
	}
}

// #1456 review round 17 (Needs Fixing 1) must-survive (b): a retry whose
// outcome is genuinely unresolvable reports outcome-unknown and KEEPS recorded
// stop state — the position must not become an Unprotected re-arm candidate on
// the next cycle while an untracked reduce-only order may be resting.
func TestAuditRetryOutcomeUnknownKeepsStateAndReportsUnknown(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqTrailingAuditFixture(t)
	var mu sync.RWMutex
	callN := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		callN++
		if callN == 1 {
			return &HyperliquidStopLossUpdateResult{
				CancelStopLossSucceeded: true,
				StopLossError:           "Order would exceed the open order limit",
			}, "", nil
		}
		return &HyperliquidStopLossUpdateResult{
			StopLossError:          "place_stop_loss failed: boom",
			StopLossOutcomeUnknown: true,
			StopLossTriggerPx:      triggerPx,
		}, "", nil
	}

	runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{"ETH": 2340.5},
		hlNetSideByCoinAllLong(),
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())

	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 4242 || pos.StopLossTriggerPx != 2325 {
		t.Fatalf("state cleared to oid=%d trigger=%.4f, want kept 4242/2325 (never an Unprotected re-arm candidate)", pos.StopLossOID, pos.StopLossTriggerPx)
	}
	if last := lastLiqAlertAction("hl-eth", "ETH"); last != hlLiquidationActionOutcomeUnknown {
		t.Errorf("alert action = %q, want %q", last, hlLiquidationActionOutcomeUnknown)
	}
}

// #1456 review round 17 (Needs Fixing 1) must-survive (c): a retry positively
// rejected by the open-order cap produces exactly one honest protection-lost
// report and clears the dead OID — unchanged from the pre-round-17 behavior.
func TestAuditRetryCapRejectedStillProtectionLost(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqTrailingAuditFixture(t)
	var mu sync.RWMutex
	callN := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		callN++
		return &HyperliquidStopLossUpdateResult{
			CancelStopLossSucceeded: callN == 1,
			StopLossError:           "Order would exceed the open order limit",
		}, "", nil
	}

	runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{"ETH": 2340.5},
		hlNetSideByCoinAllLong(),
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())

	if callN != 2 {
		t.Errorf("placement calls = %d, want 2", callN)
	}
	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Errorf("dead oid not cleared: oid=%d trigger=%.4f, want 0/0", pos.StopLossOID, pos.StopLossTriggerPx)
	}
	if last := lastLiqAlertAction("hl-eth", "ETH"); last != hlLiquidationActionProtectionLost {
		t.Errorf("alert action = %q, want %q", last, hlLiquidationActionProtectionLost)
	}
}

// #1456 review round 17 (Needs Fixing 2): force=true makes the flush attempt a
// save even with nothing mutated and nothing dirty — the halt path's recovery
// probe, without which a fleet that never comes due could never clear the
// latch after SQLite recovers.
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

// #1456 review round 18 (Needs Fixing 1) must-survive (a): a fresh re-arm
// (cancelOID == 0) whose outcome cannot be read reports outcome-unknown and
// records the REQUESTED trigger — so the following cycle submits no second
// placement and raises no "NO exchange-side stop" CRITICAL.
func TestAuditRearmOutcomeUnknownRecordsTriggerAndStops(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	state.Strategies["hl-eth"].Positions["ETH"].StopLossOID = 0
	state.Strategies["hl-eth"].Positions["ETH"].StopLossTriggerPx = 0
	var mu sync.RWMutex
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		if cancelOID != 0 {
			t.Errorf("re-arm must place fresh, got cancelOID=%d", cancelOID)
		}
		return &HyperliquidStopLossUpdateResult{
			StopLossError:          "place_stop_loss returned no usable status: {...}",
			StopLossOutcomeUnknown: true,
			StopLossTriggerPx:      triggerPx,
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
	wantTrigger := 2340.5 * 1.005 // configured distance clamped just INSIDE liquidation
	if pos.StopLossTriggerPx != wantTrigger || pos.StopLossOID != 0 {
		t.Fatalf("state = oid=%d trigger=%.4f, want oid=0 trigger=%.4f (requested trigger recorded)", pos.StopLossOID, pos.StopLossTriggerPx, wantTrigger)
	}
	if last := lastLiqAlertAction("hl-eth", "ETH"); last != hlLiquidationActionPlacementUnknown {
		t.Fatalf("alert action = %q, want %q (fresh re-arm cancelled nothing)", last, hlLiquidationActionPlacementUnknown)
	}

	// The following cycle: still exactly ONE lifetime placement.
	runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{"ETH": 2340.5},
		hlNetSideByCoinAllLong(),
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	if calls != 1 {
		t.Errorf("second-pass placement calls = %d total, want still 1 (no duplicate)", calls)
	}
	if last := lastLiqAlertAction("hl-eth", "ETH"); last != hlLiquidationActionPlacementUnknown {
		t.Errorf("second-cycle alert action = %q, want %q (unresolved stays surfaced)", last, hlLiquidationActionPlacementUnknown)
	}
}

// #1456 review round 18 (Needs Fixing 1) must-survive (b): a re-arm
// POSITIVELY rejected by the open-order cap is still re-arm failed and still
// retried next cycle — unchanged from before this round.
func TestAuditRearmCapRejectedStillRetriedNextCycle(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	state.Strategies["hl-eth"].Positions["ETH"].StopLossOID = 0
	state.Strategies["hl-eth"].Positions["ETH"].StopLossTriggerPx = 0
	var mu sync.RWMutex
	calls := 0
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		return &HyperliquidStopLossUpdateResult{
			StopLossError: "Order would exceed the open order limit",
		}, "", nil
	}
	for i := 0; i < 2; i++ {
		runHyperliquidLiquidationAudit(strategies, state,
			map[string]float64{"ETH": 2340.5},
			hlNetSideByCoinAllLong(),
			map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	}
	if calls != 2 {
		t.Errorf("placement calls across two passes = %d, want 2 (genuine rejection keeps retrying)", calls)
	}
	if last := lastLiqAlertAction("hl-eth", "ETH"); last != hlLiquidationActionRearmFailed {
		t.Errorf("alert action = %q, want %q", last, hlLiquidationActionRearmFailed)
	}
}

// #1456 review round 18 (Needs Fixing 1) must-survive (c): a re-arm whose
// unreadable response the book diff resolved to one fresh oid is adopted as a
// normal re-arm with that OID recorded.
func TestAuditRearmAdoptsBookDiffResolvedOID(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125)
	state.Strategies["hl-eth"].Positions["ETH"].StopLossOID = 0
	state.Strategies["hl-eth"].Positions["ETH"].StopLossTriggerPx = 0
	var mu sync.RWMutex
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{
			StopLossError:     "place_stop_loss returned no usable status: {...}",
			StopLossOID:       9002,
			StopLossTriggerPx: triggerPx,
		}, "", nil
	}
	runHyperliquidLiquidationAudit(strategies, state,
		map[string]float64{"ETH": 2340.5},
		hlNetSideByCoinAllLong(),
		map[string]float64{"ETH": 1.0}, true, &mu, nil, time.Now().UTC())
	pos := state.Strategies["hl-eth"].Positions["ETH"]
	if pos.StopLossOID != 9002 {
		t.Errorf("oid = %d, want 9002 (resolved re-arm adopted)", pos.StopLossOID)
	}
	if last := lastLiqAlertAction("hl-eth", "ETH"); last != hlLiquidationActionRearmed {
		t.Errorf("alert action = %q, want %q", last, hlLiquidationActionRearmed)
	}
}

// #1456 review round 18 (Needs Fixing 1), round 19 (Optional 3): the one-shot
// fixed-ATR arm reports PLACEMENT unknown instead of re-arm failed for an
// unreadable placement — the arm cancels nothing, so the cancel-claiming
// wording would be false.
func TestHLLiquidationArmClampActionOutcomeUnknown(t *testing.T) {
	if got := hlLiquidationArmClampAction(&HyperliquidStopLossUpdateResult{
		StopLossError:          "boom",
		StopLossOutcomeUnknown: true,
		StopLossTriggerPx:      2300,
	}, true); got != hlLiquidationActionPlacementUnknown {
		t.Errorf("action = %q, want %q", got, hlLiquidationActionPlacementUnknown)
	}
	if got := hlLiquidationArmClampAction(&HyperliquidStopLossUpdateResult{
		StopLossError: "Order would exceed the open order limit",
	}, true); got != hlLiquidationActionRearmFailed {
		t.Errorf("positive rejection = %q, want re-arm failed", got)
	}
}

// #1456 review round 19 (Needs Fixing 1) must-survive (a): a TIGHTEN whose
// cancel lands and whose placement outcome is unreadable must leave state
// pointing at neither the cancelled OID nor the old past-liquidation trigger,
// and the next cycle must submit NO second order — only the alert repeats.
func TestAuditTightenOutcomeUnknownRecordsTriggerAndStops(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	clearHLLiquidationAlert("hl-eth", "ETH")

	strategies, state := liqAuditFixture(t, true, 3.125) // oid=4242 trigger=2325 < liq 2340.5
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

	// Second unreadable cycle: no placement at all — the residue is
	// report-only — and the alert keeps firing under its placement-unknown
	// wording.
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

// #1456 review round 19 (Needs Fixing 1) must-survive (b): from the
// outcome-unknown residue ({oid 0, reachable trigger}), a later READABLE
// placement records exactly one tracked stop.
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

// #1456 review round 19 (Optional 2): a submit fill sized by the #621 on-chain
// cap books the FILLED quantity, leaves the residue in the book with cleared
// protection fields, and never over-books the stale virtual quantity.
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

	// Full-cap fill: unchanged legacy behavior — the whole position closes.
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

// #1456 review round 19 (Optional 3): the two unknown-outcome actions carry
// different cancel claims, and every other action's wording is untouched.
func TestLiquidationAlertPlacementUnknownWording(t *testing.T) {
	_, detailTighten, unprotectedTighten := hlLiquidationAlertMessage(2325, 2352.2025, 2340.5, hlLiquidationActionOutcomeUnknown, "rec")
	if !strings.Contains(detailTighten, "CANCELLED") {
		t.Errorf("tighten detail lost the cancel claim: %q", detailTighten)
	}
	if unprotectedTighten {
		t.Error("outcome unknown must not classify as unprotected")
	}
	_, detailFresh, unprotectedFresh := hlLiquidationAlertMessage(0, 2352.2025, 2340.5, hlLiquidationActionPlacementUnknown, "rec")
	if strings.Contains(detailFresh, "old trigger was CANCELLED") || !strings.Contains(detailFresh, "Nothing was cancelled") {
		t.Errorf("fresh-placement detail asserts a cancel that never happened: %q", detailFresh)
	}
	if unprotectedFresh {
		t.Error("placement unknown must not classify as unprotected")
	}
}
