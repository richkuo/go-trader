package main

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func hfp(v float64) *float64 { return &v }

func hurstTestRegimeConfig(classifier string) *RegimeConfig {
	return &RegimeConfig{
		Enabled: true,
		Windows: RegimeWindowsMap{
			"medium": RegimeWindowSpec{Classifier: classifier, Period: 20},
			"long":   RegimeWindowSpec{Classifier: classifier, Period: 50},
		},
	}
}

func hurstPayload(windowKey string, h float64, present bool) RegimePayload {
	snap := RegimeSnapshot{Regime: "trending_up", Metrics: map[string]float64{"adx": 30}}
	if present {
		snap.Metrics["hurst"] = h
	}
	return RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{windowKey: snap}}
}

func hurstStrategy(hg *HurstGateConfig) StrategyConfig {
	return StrategyConfig{ID: "s1", Type: "perps", Platform: "hyperliquid", HurstGate: hg}
}

func TestResolveHurstGateOnFailurePrecedence(t *testing.T) {
	cases := []struct {
		name       string
		perStrat   string
		global     string
		wantPolicy string
	}{
		{"both unset defaults open", "", "", HurstGateOnFailureOpen},
		{"global closed inherited", "", "closed", HurstGateOnFailureClosed},
		{"per-strategy wins over global", "open", "closed", HurstGateOnFailureOpen},
		{"per-strategy closed over open global", "closed", "open", HurstGateOnFailureClosed},
		{"case and space insensitive", " CLOSED ", "", HurstGateOnFailureClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := hurstStrategy(&HurstGateConfig{Enabled: true, OnFailure: tc.perStrat})
			rc := &RegimeConfig{Enabled: true, HurstGateOnFailure: tc.global}
			if got := resolveHurstGateOnFailure(sc, rc); got != tc.wantPolicy {
				t.Fatalf("got %q want %q", got, tc.wantPolicy)
			}
		})
	}
	if got := resolveHurstGateOnFailure(StrategyConfig{ID: "x"}, nil); got != HurstGateOnFailureOpen {
		t.Fatalf("nil config: got %q want open", got)
	}
}

func TestResolveHurstGateWindowFallsBackToGateWindow(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	sc := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55)})
	if got := resolveHurstGateWindow(sc, rc); got != "medium" {
		t.Fatalf("empty window_key should resolve to the primary window, got %q", got)
	}
	sc.RegimeGateWindow = "long"
	if got := resolveHurstGateWindow(sc, rc); got != "long" {
		t.Fatalf("should follow regime_gate_window, got %q", got)
	}
	sc.HurstGate.WindowKey = "medium"
	if got := resolveHurstGateWindow(sc, rc); got != "medium" {
		t.Fatalf("explicit window_key must win, got %q", got)
	}
}

func TestHurstStateMachineHysteresisMinOnly(t *testing.T) {
	hg := &HurstGateConfig{Enabled: true, Min: hfp(0.55), DisarmMin: hfp(0.50)}
	st := advanceHurstState(hg, hurstGateStateUnknown, 0.40, true)
	if st != hurstGateStateDisarmed {
		t.Fatalf("first reading below min should disarm, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.52, true)
	if st != hurstGateStateDisarmed {
		t.Fatalf("0.52 is inside the gap and must not re-arm, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.55, true)
	if st != hurstGateStateArmed {
		t.Fatalf("H at the arm bound should arm, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.51, true)
	if st != hurstGateStateArmed {
		t.Fatalf("armed must survive a dip above disarm_min, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.4999, true)
	if st != hurstGateStateDisarmed {
		t.Fatalf("below disarm_min should disarm, got %q", st)
	}
}

func TestHurstStateMachineHysteresisMaxOnly(t *testing.T) {
	hg := &HurstGateConfig{Enabled: true, Max: hfp(0.45), DisarmMax: hfp(0.50)}
	st := advanceHurstState(hg, hurstGateStateUnknown, 0.40, true)
	if st != hurstGateStateArmed {
		t.Fatalf("mean-reversion gate arms below max, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.48, true)
	if st != hurstGateStateArmed {
		t.Fatalf("inside the gap must stay armed, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.51, true)
	if st != hurstGateStateDisarmed {
		t.Fatalf("above disarm_max should disarm, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.47, true)
	if st != hurstGateStateDisarmed {
		t.Fatalf("gap must not re-arm from disarmed, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.45, true)
	if st != hurstGateStateArmed {
		t.Fatalf("back inside the arm band should arm, got %q", st)
	}
}

func TestHurstStateMachineUnsetDisarmCollapsesToArmBound(t *testing.T) {
	hg := &HurstGateConfig{Enabled: true, Min: hfp(0.55)}
	st := advanceHurstState(hg, hurstGateStateArmed, 0.5499, true)
	if st != hurstGateStateDisarmed {
		t.Fatalf("without disarm_min the arm bound is the disarm bound, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.55, true)
	if st != hurstGateStateArmed {
		t.Fatalf("at the bound should arm, got %q", st)
	}
}

func TestHurstStateMachineBandConfig(t *testing.T) {
	hg := &HurstGateConfig{Enabled: true, Min: hfp(0.45), Max: hfp(0.55), DisarmMin: hfp(0.40), DisarmMax: hfp(0.60)}
	st := advanceHurstState(hg, hurstGateStateUnknown, 0.50, true)
	if st != hurstGateStateArmed {
		t.Fatalf("inside band arms, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.58, true)
	if st != hurstGateStateArmed {
		t.Fatalf("inside the upper gap must stay armed, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.61, true)
	if st != hurstGateStateDisarmed {
		t.Fatalf("above disarm_max disarms, got %q", st)
	}
	st = advanceHurstState(hg, st, 0.56, true)
	if st != hurstGateStateDisarmed {
		t.Fatalf("upper gap must not re-arm, got %q", st)
	}
}

func TestHurstStateMachineAbsentHNeverTransitions(t *testing.T) {
	hg := &HurstGateConfig{Enabled: true, Min: hfp(0.55)}
	for _, prior := range []string{hurstGateStateUnknown, hurstGateStateArmed, hurstGateStateDisarmed} {
		if got := advanceHurstState(hg, prior, math.NaN(), false); got != prior {
			t.Fatalf("absent H changed %q -> %q; NaN must neither arm nor disarm", prior, got)
		}
	}
}

func TestHurstStateMachineHandlesReadingsAboveOne(t *testing.T) {
	minOnly := &HurstGateConfig{Enabled: true, Min: hfp(0.55)}
	if got := advanceHurstState(minOnly, hurstGateStateUnknown, 2.0033, true); got != hurstGateStateArmed {
		t.Fatalf("H=2.0033 is above min and must arm a momentum gate, got %q", got)
	}
	maxOnly := &HurstGateConfig{Enabled: true, Max: hfp(0.45)}
	if got := advanceHurstState(maxOnly, hurstGateStateArmed, 2.0033, true); got != hurstGateStateDisarmed {
		t.Fatalf("H=2.0033 is above max and must disarm a mean-reversion gate, got %q", got)
	}
}

func TestHurstThresholdKeyChangesWithEveryBoundAndWindow(t *testing.T) {
	base := &HurstGateConfig{Enabled: true, Min: hfp(0.55), Max: hfp(0.80), DisarmMin: hfp(0.50), DisarmMax: hfp(0.85)}
	baseKey := hurstGateThresholdKey(base, "medium")
	if baseKey == "" {
		t.Fatal("expected a non-empty threshold key")
	}
	if hurstGateThresholdKey(base, "medium") != baseKey {
		t.Fatal("threshold key must be deterministic")
	}
	mutations := map[string]*HurstGateConfig{
		"min":        {Min: hfp(0.56), Max: hfp(0.80), DisarmMin: hfp(0.50), DisarmMax: hfp(0.85)},
		"max":        {Min: hfp(0.55), Max: hfp(0.81), DisarmMin: hfp(0.50), DisarmMax: hfp(0.85)},
		"disarm_min": {Min: hfp(0.55), Max: hfp(0.80), DisarmMin: hfp(0.49), DisarmMax: hfp(0.85)},
		"disarm_max": {Min: hfp(0.55), Max: hfp(0.80), DisarmMin: hfp(0.50), DisarmMax: hfp(0.86)},
		"min unset":  {Max: hfp(0.80), DisarmMax: hfp(0.85)},
	}
	for name, hg := range mutations {
		if hurstGateThresholdKey(hg, "medium") == baseKey {
			t.Fatalf("changing %s must change the threshold key", name)
		}
	}
	if hurstGateThresholdKey(base, "long") == baseKey {
		t.Fatal("changing the window must change the threshold key")
	}
	withMode := *base
	withMode.Mode = HurstGateModeGate
	withMode.OnFailure = "closed"
	withMode.SizeFloor = hfp(0.4)
	if hurstGateThresholdKey(&withMode, "medium") != baseKey {
		t.Fatal("mode/on_failure/size_floor must NOT be part of the state key")
	}
}

func TestHurstThresholdChangeDiscardsPersistedLatch(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	sc := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55)})
	prior := HurstGateState{Key: "stale-key", State: hurstGateStateDisarmed, Observed: true}
	d := evaluateHurstGate(sc, hurstPayload("medium", 0.60, true), rc, prior, 0)
	if d.State != hurstGateStateArmed {
		t.Fatalf("stale latch must be discarded and re-derived, got state %q", d.State)
	}
	if d.Holds {
		t.Fatal("an in-band reading under fresh thresholds must not hold")
	}
	prior.Key = d.Key
	prior.State = hurstGateStateDisarmed
	d2 := evaluateHurstGate(sc, hurstPayload("medium", 0.52, true), rc, prior, 0)
	if d2.State != hurstGateStateDisarmed || !d2.Holds {
		t.Fatalf("matching key must honor the disarmed latch, got state=%q holds=%v", d2.State, d2.Holds)
	}
}

func TestEvaluateHurstGateDisabledIsInert(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	for _, sc := range []StrategyConfig{
		hurstStrategy(nil),
		hurstStrategy(&HurstGateConfig{Enabled: false, Min: hfp(0.9)}),
	} {
		d := evaluateHurstGate(sc, hurstPayload("medium", 0.10, true), rc, HurstGateState{}, 0)
		if d.Active || d.Holds {
			t.Fatalf("default-off gate must be completely inert, got %+v", d)
		}
		if d.OpenSizeMult() != 1.0 {
			t.Fatalf("inactive gate must never scale size, got %v", d.OpenSizeMult())
		}
	}
}

func TestEvaluateHurstGateFailClosedIsFlatOnly(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	sc := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), OnFailure: "closed"})
	absent := hurstPayload("medium", 0, false)

	flat := evaluateHurstGate(sc, absent, rc, HurstGateState{}, 0)
	if !flat.Holds {
		t.Fatal("fail-closed with unknown H must hold a FRESH open")
	}
	open := evaluateHurstGate(sc, absent, rc, HurstGateState{}, 1.5)
	if open.Holds {
		t.Fatal("fail-closed must never hold while a position is open (posQty>0 management)")
	}
	scOpen := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), OnFailure: "open"})
	for _, qty := range []float64{0, 1.5} {
		if evaluateHurstGate(scOpen, absent, rc, HurstGateState{}, qty).Holds {
			t.Fatalf("fail-open must admit at posQty=%v", qty)
		}
	}
}

func TestEvaluateHurstGateKnownDisarmedHoldsRegardlessOfPosition(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	sc := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55)})
	d := evaluateHurstGate(sc, hurstPayload("medium", 0.20, true), rc, HurstGateState{}, 3.0)
	if !d.Holds {
		t.Fatal("a known out-of-band reading must hold position-increasing signals even while open")
	}
	if !strings.Contains(d.Detail, "disarmed") {
		t.Fatalf("detail should name the state, got %q", d.Detail)
	}
}

func TestEvaluateHurstGateWrongWindowYieldsUnknownH(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	sc := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), WindowKey: "long", OnFailure: "closed"})
	d := evaluateHurstGate(sc, hurstPayload("medium", 0.90, true), rc, HurstGateState{}, 0)
	if d.HKnown {
		t.Fatal("reading the wrong window must not pick up another window's hurst")
	}
	if !d.Holds {
		t.Fatal("unknown H under fail-closed must hold a fresh open")
	}
}

func TestHurstFromPayloadRejectsNonFinite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p := RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{
			"medium": {Metrics: map[string]float64{"hurst": v}},
		}}
		if _, ok := hurstFromPayload(p, "medium"); ok {
			t.Fatalf("non-finite %v must read as unknown", v)
		}
	}
	if _, ok := hurstFromPayload(RegimePayload{Legacy: "trending_up"}, "medium"); ok {
		t.Fatal("legacy single-label payload must read as unknown")
	}
}

func TestHurstSizeMultiplierFormulaAndClamps(t *testing.T) {
	cases := []struct {
		h, floor, want float64
	}{
		{0.500, 0.25, 0.25},
		{0.5375, 0.25, 0.25},
		{0.575, 0.25, 0.5},
		{0.65, 0.25, 1.0},
		{0.80, 0.25, 1.0},
		{0.35, 0.25, 1.0},
		{2.0033, 0.25, 1.0},
		{0.50, 0.9, 0.9},
		{0.625, hurstDefaultSizeFloor, 0.8333333333333334},
	}
	for _, tc := range cases {
		got := hurstSizeMultiplier(tc.h, tc.floor)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("h=%v floor=%v: got %v want %v", tc.h, tc.floor, got, tc.want)
		}
		if got > 1.0 {
			t.Fatalf("h=%v: multiplier %v exceeds 1.0 — the gate must never grow an open", tc.h, got)
		}
	}
	if got := hurstSizeMultiplier(math.NaN(), 0.25); got != 1.0 {
		t.Fatalf("NaN H must resolve to a neutral 1.0 multiplier, got %v", got)
	}
}

func TestEvaluateHurstGateSizeModeNeverHoldsOnKnownH(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	sc := hurstStrategy(&HurstGateConfig{Enabled: true, Mode: HurstGateModeSize, SizeFloor: hfp(0.3), OnFailure: "closed"})
	d := evaluateHurstGate(sc, hurstPayload("medium", 0.50, true), rc, HurstGateState{}, 0)
	if d.Holds {
		t.Fatal("size mode must never hold on a known reading — it scales instead")
	}
	if math.Abs(d.OpenSizeMult()-0.3) > 1e-9 {
		t.Fatalf("H=0.5 should size at the floor, got %v", d.OpenSizeMult())
	}
	absent := evaluateHurstGate(sc, hurstPayload("medium", 0, false), rc, HurstGateState{}, 0)
	if !absent.Holds {
		t.Fatal("size mode with unknown H under fail-closed must hold a fresh open")
	}
	if absent.OpenSizeMult() != 1.0 {
		t.Fatalf("held cycle must not also carry a scaled multiplier, got %v", absent.OpenSizeMult())
	}
	openPos := evaluateHurstGate(sc, hurstPayload("medium", 0, false), rc, HurstGateState{}, 2.0)
	if openPos.Holds {
		t.Fatal("size-mode fail-closed must be flat-only too")
	}
	scOpen := hurstStrategy(&HurstGateConfig{Enabled: true, Mode: HurstGateModeSize})
	neutral := evaluateHurstGate(scOpen, hurstPayload("medium", 0, false), rc, HurstGateState{}, 0)
	if neutral.Holds || neutral.OpenSizeMult() != 1.0 {
		t.Fatalf("unknown H fail-open must admit at full size, got %+v", neutral)
	}
}

func TestGateModeNeverScalesSize(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	sc := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55)})
	d := evaluateHurstGate(sc, hurstPayload("medium", 0.90, true), rc, HurstGateState{}, 0)
	if d.OpenSizeMult() != 1.0 {
		t.Fatalf("gate mode must always size at 1.0, got %v", d.OpenSizeMult())
	}
}

func TestOpenSizeMultRejectsOutOfRangeValues(t *testing.T) {
	for _, v := range []float64{-1, 0, 1.5, math.NaN(), math.Inf(1)} {
		d := HurstGateDecision{Active: true, Mode: HurstGateModeSize, SizeMult: v}
		if got := d.OpenSizeMult(); got != 1.0 {
			t.Fatalf("out-of-range SizeMult %v must resolve to 1.0, got %v", v, got)
		}
	}
}

func TestHurstHoldOnlyBlocksPositionIncreasingSignals(t *testing.T) {
	blocked := func(signal int, closeFraction, posQty float64, posSide string, allowsLong, allowsShort bool) bool {
		return pausedBlocksSignal(signal, closeFraction, posQty, posSide, allowsLong, allowsShort)
	}
	cases := []struct {
		name                              string
		signal                            int
		closeFraction, posQty             float64
		posSide                           string
		allowsLong, allowsShort, wantHold bool
	}{
		{"fresh open from flat", 1, 0, 0, "", true, false, true},
		{"scale-in add on long", 1, 0, 2, "long", true, false, true},
		{"bidirectional flip", 1, 0, 2, "short", true, true, true},
		{"signal 0 manage cycle", 0, 0, 2, "long", true, false, false},
		{"registry close action", -1, 0.5, 2, "long", true, false, false},
		{"full registry close", -1, 1, 2, "long", true, false, false},
		{"pure-close long-only exit", -1, 0, 2, "long", true, false, false},
		{"pure-close short-only cover", 1, 0, 2, "short", false, true, false},
		{"futures opposite side is a flip", -1, 0, 2, "long", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blocked(tc.signal, tc.closeFraction, tc.posQty, tc.posSide, tc.allowsLong, tc.allowsShort); got != tc.wantHold {
				t.Fatalf("got hold=%v want %v", got, tc.wantHold)
			}
		})
	}
}

func TestAdvanceHurstGateCommitsStateEveryCycleIncludingManageCycles(t *testing.T) {
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	sc := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55), DisarmMin: hfp(0.50)})
	st := &StrategyState{ID: "s1", Positions: map[string]*Position{}}
	var mu sync.RWMutex

	d := advanceHurstGate(sc, hurstPayload("medium", 0.60, true), rc, st, &mu, 0)
	if d.State != hurstGateStateArmed || st.HurstGate.State != hurstGateStateArmed {
		t.Fatalf("cycle 1: decision=%q persisted=%q", d.State, st.HurstGate.State)
	}
	if !st.HurstGate.Observed || st.HurstGate.LastH != 0.60 {
		t.Fatalf("expected the reading to be recorded, got %+v", st.HurstGate)
	}
	d = advanceHurstGate(sc, hurstPayload("medium", 0.30, true), rc, st, &mu, 0)
	if d.State != hurstGateStateDisarmed || st.HurstGate.State != hurstGateStateDisarmed {
		t.Fatalf("cycle 2: decision=%q persisted=%q", d.State, st.HurstGate.State)
	}
	d = advanceHurstGate(sc, hurstPayload("medium", 0, false), rc, st, &mu, 0)
	if d.State != hurstGateStateDisarmed || st.HurstGate.LastH != 0.30 {
		t.Fatalf("absent H must hold state and last reading, got %+v", st.HurstGate)
	}
	scOff := hurstStrategy(nil)
	before := st.HurstGate
	advanceHurstGate(scOff, hurstPayload("medium", 0.9, true), rc, st, &mu, 0)
	if st.HurstGate != before {
		t.Fatal("a strategy without a hurst_gate must never touch the latch")
	}
}

func TestHurstGateStateJSONRoundTrip(t *testing.T) {
	st := HurstGateState{Key: "abc123", State: hurstGateStateDisarmed, LastH: 0.4321, LastHAt: time.Now().UTC().Truncate(time.Second), Observed: true}
	raw := marshalHurstGateStateJSON(st)
	if raw == "" {
		t.Fatal("expected non-empty JSON for a populated latch")
	}
	back := unmarshalHurstGateStateJSON(raw)
	if back.Key != st.Key || back.State != st.State || back.LastH != st.LastH || !back.Observed {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if marshalHurstGateStateJSON(HurstGateState{}) != "" {
		t.Fatal("an empty latch must persist as an empty column value")
	}
	for _, raw := range []string{"", "   ", "not json", `{"state":"bogus","key":"k"}`} {
		got := unmarshalHurstGateStateJSON(raw)
		if got.State != hurstGateStateUnknown {
			t.Fatalf("%q should load as unknown, got %q", raw, got.State)
		}
	}
}

func TestPerpsOpenNotionalSizedAppliesEntryMultInNotionalMode(t *testing.T) {
	sizing := PerpsSizing{SizingLeverage: 2, ExchangeLeverage: 5}
	full := PerpsOpenNotionalSized(1000, 100, sizing)
	half := PerpsOpenNotionalSized(1000, 100, withEntrySizeMult(sizing, 0.5))
	if math.Abs(half-full*0.5) > 1e-9 {
		t.Fatalf("multiplier should halve notional: full=%v scaled=%v", full, half)
	}
	if PerpsOpenNotionalSized(1000, 100, sizing) != PerpsOpenNotional(1000, 2, 5, 0) {
		t.Fatal("zero-value EntrySizeMult must be a no-op")
	}
}

func TestPerpsOpenNotionalSizedComposesWithRiskPerTradePct(t *testing.T) {
	sizing := PerpsSizing{RiskPerTradePct: 1, RiskStopDistance: 5, ExchangeLeverage: 10}
	full := PerpsOpenNotionalSized(10000, 100, sizing)
	scaled := PerpsOpenNotionalSized(10000, 100, withEntrySizeMult(sizing, 0.4))
	if math.Abs(scaled-full*0.4) > 1e-9 {
		t.Fatalf("risk-mode notional should scale linearly: full=%v scaled=%v", full, scaled)
	}
	tight := PerpsSizing{RiskPerTradePct: 5, RiskStopDistance: 0.01, ExchangeLeverage: 3}
	capped := PerpsOpenNotionalSized(1000, 100, tight)
	if math.Abs(capped-1000*3) > 1e-9 {
		t.Fatalf("expected the cash x leverage cap to bind, got %v", capped)
	}
	cappedScaled := PerpsOpenNotionalSized(1000, 100, withEntrySizeMult(tight, 0.25))
	if cappedScaled > capped {
		t.Fatalf("scaled notional %v must never exceed the capped notional %v", cappedScaled, capped)
	}
	if math.Abs(cappedScaled-capped*0.25) > 1e-9 {
		t.Fatalf("scaling applies after the cap: got %v want %v", cappedScaled, capped*0.25)
	}
}

func TestEntrySizeMultOutOfRangeIsNeutral(t *testing.T) {
	for _, v := range []float64{-0.5, 0, 1.0001, 42} {
		s := PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1, EntrySizeMult: v}
		if s.entrySizeMult() != 1.0 {
			t.Fatalf("EntrySizeMult=%v must resolve neutral, got %v", v, s.entrySizeMult())
		}
	}
}

func TestNormalizeOpenSizeMultIsFailSafeSmall(t *testing.T) {
	for _, v := range []float64{-1, 0, 1.2, math.NaN()} {
		if normalizeOpenSizeMult(v) != 1.0 {
			t.Fatalf("%v must normalize to 1.0", v)
		}
	}
	if normalizeOpenSizeMult(0.4) != 0.4 {
		t.Fatal("in-range multipliers pass through unchanged")
	}
}

func TestSpotPaperOpenScalesWithMultiplierAndClosesDoNot(t *testing.T) {
	logger := silentStrategyLogger("spot-hurst")
	newState := func() *StrategyState {
		return &StrategyState{ID: "spot-hurst", Type: "spot", Cash: 1000, Positions: map[string]*Position{}, TradeHistory: []Trade{}}
	}
	full := newState()
	if _, err := ExecuteSpotSignalWithFillFeeSizedDeferredOpen(full, 1, "BTC/USD", 100, 0, 0, "", 0, 1.0, logger); err != nil {
		t.Fatal(err)
	}
	scaled := newState()
	if _, err := ExecuteSpotSignalWithFillFeeSizedDeferredOpen(scaled, 1, "BTC/USD", 100, 0, 0, "", 0, 0.5, logger); err != nil {
		t.Fatal(err)
	}
	fullPos, scaledPos := full.Positions["BTC/USD"], scaled.Positions["BTC/USD"]
	fn := fullPos.Quantity * fullPos.AvgCost
	sn := scaledPos.Quantity * scaledPos.AvgCost
	if math.Abs(fn-1000) > 1e-6 {
		t.Fatalf("unscaled open should commit the full $1000 budget, got %v", fn)
	}
	if math.Abs(sn-500) > 1e-6 {
		t.Fatalf("half multiplier should commit half the budget: got %v want 500", sn)
	}
	if _, err := ExecuteSpotSignalWithFillFeeSizedDeferredOpen(scaled, -1, "BTC/USD", 110, 0, 0, "", 0, 0.5, logger); err != nil {
		t.Fatal(err)
	}
	if _, still := scaled.Positions["BTC/USD"]; still {
		t.Fatal("a close must fully exit regardless of the size multiplier")
	}
}

func TestFuturesPaperOpenFloorsToZeroAndRefuses(t *testing.T) {
	logger := silentStrategyLogger("fut-hurst")
	spec := ContractSpec{Margin: 400, Multiplier: 1}
	s := &StrategyState{ID: "fut-hurst", Type: "futures", Cash: 1000, Positions: map[string]*Position{}, TradeHistory: []Trade{}}
	res, err := ExecuteFuturesSignalWithFillFeeSizedDeferredOpen(s, 1, "ES", 100, spec, 1, 0, 0, 0, "", 0, 1.0, logger)
	if err != nil || res.TradesExecuted != 1 || s.Positions["ES"].Quantity != 2 {
		t.Fatalf("expected 2 contracts, got %+v err=%v", s.Positions["ES"], err)
	}
	s2 := &StrategyState{ID: "fut-hurst", Type: "futures", Cash: 1000, Positions: map[string]*Position{}, TradeHistory: []Trade{}}
	res2, err := ExecuteFuturesSignalWithFillFeeSizedDeferredOpen(s2, 1, "ES", 100, spec, 1, 0, 0, 0, "", 0, 0.3, logger)
	if err != nil {
		t.Fatal(err)
	}
	if res2.TradesExecuted != 0 || len(s2.Positions) != 0 {
		t.Fatalf("a floor-to-zero scaled size must refuse the open, got %d trades / %d positions", res2.TradesExecuted, len(s2.Positions))
	}
	if s2.Cash != 1000 {
		t.Fatalf("a refused open must not move cash, got %v", s2.Cash)
	}
}

func TestStampHurstGateAtOpenIfOpened(t *testing.T) {
	newState := func() *StrategyState {
		return &StrategyState{ID: "s1", Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 1}}}
	}
	sized := HurstGateDecision{Active: true, Mode: HurstGateModeSize, H: 0.62, HKnown: true, SizeMult: 0.8}
	s := newState()
	stampHurstGateAtOpenIfOpened(s, "BTC", true, sized)
	if s.Positions["BTC"].HurstAtOpen != 0.62 || s.Positions["BTC"].HurstSizeMult != 0.8 {
		t.Fatalf("size mode should stamp both, got %+v", s.Positions["BTC"])
	}
	gated := HurstGateDecision{Active: true, Mode: HurstGateModeGate, H: 0.62, HKnown: true, SizeMult: 1.0}
	s = newState()
	stampHurstGateAtOpenIfOpened(s, "BTC", true, gated)
	if s.Positions["BTC"].HurstAtOpen != 0.62 || s.Positions["BTC"].HurstSizeMult != 0 {
		t.Fatalf("gate mode should stamp H only, got %+v", s.Positions["BTC"])
	}
	s = newState()
	stampHurstGateAtOpenIfOpened(s, "BTC", false, sized)
	stampHurstGateAtOpenIfOpened(s, "BTC", true, HurstGateDecision{})
	stampHurstGateAtOpenIfOpened(s, "BTC", true, HurstGateDecision{Active: true, Mode: HurstGateModeGate, H: math.NaN()})
	if s.Positions["BTC"].HurstAtOpen != 0 || s.Positions["BTC"].HurstSizeMult != 0 {
		t.Fatalf("expected no stamp, got %+v", s.Positions["BTC"])
	}
}

func TestCaptureTradeDiagnosticsCarriesHurstAndNeverWritesLLMVerdict(t *testing.T) {
	var captured *TradeDiagnosticsRow
	prevRecorder, prevEnqueue := tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue
	tradeDiagnosticsRecorder = func(row *TradeDiagnosticsRow) error { captured = row; return nil }
	tradeDiagnosticsEnqueue = nil
	defer func() { tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue = prevRecorder, prevEnqueue }()

	s := &StrategyState{ID: "s1"}
	pos := &Position{Symbol: "BTC", Side: "long", AvgCost: 100, Quantity: 1, HurstAtOpen: 0.61, HurstSizeMult: 0.75}
	captureTradeDiagnostics(s, pos, 110, 10, "signal_close", time.Now().UTC())
	if captured == nil {
		t.Fatal("expected a diagnostics row")
	}
	if captured.HurstAtOpen == nil || *captured.HurstAtOpen != 0.61 {
		t.Fatalf("hurst_at_open not carried: %+v", captured.HurstAtOpen)
	}
	if captured.HurstSizeMult == nil || *captured.HurstSizeMult != 0.75 {
		t.Fatalf("hurst_size_mult not carried: %+v", captured.HurstSizeMult)
	}
	if captured.LLMVerdict != nil {
		t.Fatalf("the hurst stamp path must never write llm_verdict, got %v", *captured.LLMVerdict)
	}
	captured = nil
	captureTradeDiagnostics(s, &Position{Symbol: "ETH", Side: "long", AvgCost: 10, Quantity: 1}, 11, 1, "signal_close", time.Now().UTC())
	if captured.HurstAtOpen != nil || captured.HurstSizeMult != nil {
		t.Fatal("an unstamped position must leave both diagnostics columns NULL")
	}
}

func TestHurstStampsAcceptReadingsAboveOne(t *testing.T) {
	const aboveOne = 2.0033

	s := &StrategyState{ID: "s1", Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 1}}}
	stampHurstGateAtOpenIfOpened(s, "BTC", true, HurstGateDecision{
		Active: true, Mode: HurstGateModeGate, H: aboveOne, HKnown: true, SizeMult: 1.0,
	})
	if got := s.Positions["BTC"].HurstAtOpen; got != aboveOne {
		t.Fatalf("a >1 reading must stamp verbatim, got %v", got)
	}

	var captured *TradeDiagnosticsRow
	prevRecorder, prevEnqueue := tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue
	tradeDiagnosticsRecorder = func(row *TradeDiagnosticsRow) error { captured = row; return nil }
	tradeDiagnosticsEnqueue = nil
	defer func() { tradeDiagnosticsRecorder, tradeDiagnosticsEnqueue = prevRecorder, prevEnqueue }()
	captureTradeDiagnostics(s, s.Positions["BTC"], 110, 10, "signal_close", time.Now().UTC())
	if captured == nil || captured.HurstAtOpen == nil || *captured.HurstAtOpen != aboveOne {
		t.Fatalf("a >1 hurst_at_open must reach trade_diagnostics unchanged, got %+v", captured)
	}

	db := openTestDB(t)
	state := &AppState{Strategies: map[string]*StrategyState{
		"s1": {
			ID: "s1", Type: "perps", Cash: 1000,
			OptionPositions: map[string]*OptionPosition{},
			Positions: map[string]*Position{"BTC": {
				Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "long", Multiplier: 1,
				OwnerStrategyID: "s1", HurstAtOpen: aboveOne, HurstSizeMult: 1.0,
			}},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := loaded.Strategies["s1"].Positions["BTC"].HurstAtOpen; got != aboveOne {
		t.Fatalf("a >1 hurst_at_open must survive a restart, got %v", got)
	}

	captured = nil
	captureTradeDiagnostics(s, &Position{Symbol: "ETH", Side: "long", AvgCost: 10, Quantity: 1}, 11, 1, "signal_close", time.Now().UTC())
	if captured.HurstAtOpen != nil || captured.HurstSizeMult != nil {
		t.Fatal("0 must still read as unstamped")
	}
}

func TestHurstGateStatusMarker(t *testing.T) {
	cases := []struct {
		name string
		d    HurstGateDecision
		want string
	}{
		{"inactive renders nothing", HurstGateDecision{}, ""},
		{"armed", HurstGateDecision{Active: true, Mode: HurstGateModeGate, H: 0.6123, HKnown: true}, "hurst=0.6123 (gate armed)"},
		{"disarmed", HurstGateDecision{Active: true, Mode: HurstGateModeGate, H: 0.48, HKnown: true, Holds: true}, "hurst=0.4800 (gate disarmed)"},
		{"unknown fail-closed", HurstGateDecision{Active: true, Mode: HurstGateModeGate, Holds: true}, "hurst=? (gate closed)"},
		{"unknown fail-open", HurstGateDecision{Active: true, Mode: HurstGateModeGate}, "hurst=?"},
		{"size", HurstGateDecision{Active: true, Mode: HurstGateModeSize, H: 0.575, HKnown: true, SizeMult: 0.5}, "hurst=0.5750 (size ×0.50)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hurstGateStatusMarker(tc.d); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestHurstGateLatchSurvivesRestart(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{Strategies: map[string]*StrategyState{
		"s1": {
			ID: "s1", Type: "perps", Platform: "hyperliquid", Cash: 1000,
			Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			HurstGate: HurstGateState{Key: "k1", State: hurstGateStateDisarmed, LastH: 0.4321, Observed: true},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got := loaded.Strategies["s1"].HurstGate
	if got.Key != "k1" || got.State != hurstGateStateDisarmed || got.LastH != 0.4321 || !got.Observed {
		t.Fatalf("a disarmed latch must survive a restart, got %+v", got)
	}
}

func TestHurstGateLatchDiscardedWhenThresholdsChange(t *testing.T) {
	db := openTestDB(t)
	rc := hurstTestRegimeConfig(regimeClassifierComposite)
	sc := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.55)})
	origKey := hurstGateThresholdKey(sc.HurstGate, resolveHurstGateWindow(sc, rc))
	state := &AppState{Strategies: map[string]*StrategyState{
		"s1": {
			ID: "s1", Type: "perps", Cash: 1000,
			Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			HurstGate: HurstGateState{Key: origKey, State: hurstGateStateDisarmed, LastH: 0.20, Observed: true},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	ss := loaded.Strategies["s1"]

	same := advanceHurstGate(sc, hurstPayload("medium", 0.50, true), rc, ss, new(sync.RWMutex), 0)
	if same.State != hurstGateStateDisarmed || !same.Holds {
		t.Fatalf("unchanged thresholds must honor the persisted latch, got %+v", same)
	}

	ss.HurstGate = HurstGateState{Key: origKey, State: hurstGateStateDisarmed, LastH: 0.20, Observed: true}
	edited := hurstStrategy(&HurstGateConfig{Enabled: true, Min: hfp(0.45)})
	after := advanceHurstGate(edited, hurstPayload("medium", 0.50, true), rc, ss, new(sync.RWMutex), 0)
	if after.State != hurstGateStateArmed || after.Holds {
		t.Fatalf("a threshold change must discard the stale latch and re-derive, got %+v", after)
	}
	if ss.HurstGate.Key == origKey {
		t.Fatal("the persisted key must be rewritten to the new threshold tuple")
	}
}

func TestPositionHurstStampsPersist(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{Strategies: map[string]*StrategyState{
		"s1": {
			ID: "s1", Type: "perps", Cash: 1000,
			OptionPositions: map[string]*OptionPosition{},
			Positions: map[string]*Position{"BTC": {
				Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "long", Multiplier: 1,
				OwnerStrategyID: "s1", HurstAtOpen: 0.6123, HurstSizeMult: 0.75,
			}},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	pos := loaded.Strategies["s1"].Positions["BTC"]
	if pos.HurstAtOpen != 0.6123 || pos.HurstSizeMult != 0.75 {
		t.Fatalf("open-time hurst stamps must survive a restart, got %+v", pos)
	}
}

func TestHurstMigrationsAreIdempotent(t *testing.T) {
	path := t.TempDir() + "/state.db"
	for i := 0; i < 3; i++ {
		db, err := OpenStateDB(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	for _, q := range []string{
		"SELECT hurst_gate_state FROM strategies LIMIT 1",
		"SELECT hurst_at_open, hurst_size_mult FROM positions LIMIT 1",
		"SELECT hurst_at_open, hurst_size_mult FROM trade_diagnostics LIMIT 1",
	} {
		if _, err := db.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
