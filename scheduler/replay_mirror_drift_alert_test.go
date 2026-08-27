package main

import (
	"strings"
	"testing"
	"time"
)

func replayHoldingETH() *Position {
	return &Position{Symbol: "ETH", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 1900, Side: "long", Multiplier: 1}
}

func replayDec(id int64, typ, side string, qty, px float64) ReplayDecision {
	return ReplayDecision{
		DecisionID: id, DecisionType: typ, DecidedAt: time.Now().UTC(),
		Symbol: "ETH", Side: side, Quantity: qty, ReferencePrice: px,
	}
}

func requireOneDriftDM(t *testing.T, dms []string, kind string) {
	t.Helper()
	if len(dms) != 1 {
		t.Fatalf("driftDMs = %v, want 1 containing %q", dms, kind)
	}
	if !strings.Contains(dms[0], kind) {
		t.Fatalf("DM %q missing kind %q", dms[0], kind)
	}
}

func TestReplayDriftTracker_FirstFiresThenThrottles(t *testing.T) {
	tr := &replayDriftTracker{}
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	if !tr.Record("s1", replayDriftKindOpenWhileHolding, now) {
		t.Fatal("first drift of a kind must notify")
	}
	if tr.Record("s1", replayDriftKindOpenWhileHolding, now.Add(time.Minute)) {
		t.Fatal("same (strategy, kind) inside the throttle window must not notify")
	}
	if !tr.Record("s1", replayDriftKindScaleInWhileMismatched, now.Add(time.Minute)) {
		t.Fatal("a different kind on the same strategy must still notify")
	}
	if !tr.Record("s2", replayDriftKindOpenWhileHolding, now.Add(time.Minute)) {
		t.Fatal("the same kind on a different strategy must still notify")
	}
}

func TestReplayDriftTracker_WindowExpiryRefires(t *testing.T) {
	withAlertThrottleInterval(t, time.Hour)
	tr := &replayDriftTracker{}
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	if !tr.Record("s1", replayDriftKindPartialCloseWhileFlat, now) {
		t.Fatal("first must notify")
	}
	if tr.Record("s1", replayDriftKindPartialCloseWhileFlat, now.Add(59*time.Minute)) {
		t.Fatal("59m inside a 1h window must not notify")
	}
	if !tr.Record("s1", replayDriftKindPartialCloseWhileFlat, now.Add(time.Hour)) {
		t.Fatal("at the throttle interval the same slot must re-notify")
	}
}

func TestReplayDriftTracker_TenthEventDoesNotBypassWindow(t *testing.T) {

	tr := &replayDriftTracker{}
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	if !tr.Record("s1", replayDriftKindOpenWhileHolding, now) {
		t.Fatal("first must notify")
	}
	for i := 1; i <= 12; i++ {
		if tr.Record("s1", replayDriftKindOpenWhileHolding, now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("event #%d inside the window notified — drain-failure 10th-event cadence leaked in", i+1)
		}
	}
}

func TestMirrorReplayDriftDM_FirstOfEachKindFires(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = replayHoldingETH()
	applied, trades, _, dms := applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(1, ReplayDecisionOpen, "long", 0.5, 1908),
	}, 1908.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 {
		t.Fatalf("open-while-holding trades=%d applied=%v, want 0/1", trades, applied)
	}
	requireOneDriftDM(t, dms, replayDriftKindOpenWhileHolding)

	sc, s, logger = replayMirrorTestSetup(t, "hl-paper-eth-si")
	applied, trades, _, dms = applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(1, ReplayDecisionScaleIn, "long", 0.25, 1910),
	}, 1910.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 {
		t.Fatalf("scale-in-while-mismatched trades=%d applied=%v, want 0/1", trades, applied)
	}
	requireOneDriftDM(t, dms, replayDriftKindScaleInWhileMismatched)

	sc, s, logger = replayMirrorTestSetup(t, "hl-paper-eth-pc")
	applied, trades, _, dms = applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(1, ReplayDecisionPartialClose, "long", 0.2, 1911),
	}, 1911.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 {
		t.Fatalf("partial-close-while-flat trades=%d applied=%v, want 0/1", trades, applied)
	}
	requireOneDriftDM(t, dms, replayDriftKindPartialCloseWhileFlat)
}

func TestMirrorReplayDriftDM_SameKindInOneBatchThrottled(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = replayHoldingETH()
	pending := []ReplayDecision{
		replayDec(1, ReplayDecisionOpen, "long", 0.5, 1908),
		replayDec(2, ReplayDecisionOpen, "long", 0.4, 1910),
	}
	applied, trades, _, dms := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 2 {
		t.Fatalf("trades=%d applied=%v, want 0 trades and both rows consumed", trades, applied)
	}
	requireOneDriftDM(t, dms, replayDriftKindOpenWhileHolding)
}

func TestMirrorReplayDriftDM_DifferentKindOrStrategyStillFires(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = replayHoldingETH()
	pending := []ReplayDecision{
		replayDec(1, ReplayDecisionOpen, "long", 0.5, 1908),
		replayDec(2, ReplayDecisionScaleIn, "short", 0.25, 1910),
	}
	_, _, _, dms := applyReplayedLiveDecisions(sc, s, pending, 1910.0, replayTestResult(), &Config{}, logger)
	if len(dms) != 2 {
		t.Fatalf("driftDMs = %v, want open-while-holding AND scale-in-while-mismatched", dms)
	}

	sc2 := sc
	sc2.ID = "hl-paper-eth-other"
	s2 := replayTestStrategyState(sc2.ID)
	s2.Positions["ETH"] = replayHoldingETH()
	logger2 := silentStrategyLogger(sc2.ID)
	_, _, _, dms2 := applyReplayedLiveDecisions(sc2, s2, []ReplayDecision{
		replayDec(3, ReplayDecisionOpen, "long", 0.5, 1908),
	}, 1908.0, replayTestResult(), &Config{}, logger2)
	requireOneDriftDM(t, dms2, replayDriftKindOpenWhileHolding)
}

func TestMirrorReplayDriftDM_HighWaterRemarkDoesNotFire(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = replayHoldingETH()
	pending := []ReplayDecision{
		replayDec(1, ReplayDecisionOpen, "long", 0.5, 1908),
	}
	applied, trades, _, dms := applyReplayedLiveDecisions(sc, s, pending, 1908.0, replayTestResult(), &Config{}, logger)
	requireOneDriftDM(t, dms, replayDriftKindOpenWhileHolding)
	if trades != 0 || len(applied) != 1 {
		t.Fatalf("first pass trades=%d applied=%v, want 0/1", trades, applied)
	}

	replayDriftAlerts.reset()
	applied, trades, _, dms = applyReplayedLiveDecisions(sc, s, pending, 1908.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 {
		t.Fatalf("high-water trades=%d applied=%v, want 0/1 (re-mark only)", trades, applied)
	}
	if len(dms) != 0 {
		t.Fatalf("high-water re-mark DMed: %v", dms)
	}
}

func TestMirrorReplayDriftDM_NonBookDriftDoesNotFire(t *testing.T) {
	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	applied, trades, _, dms := applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(1, ReplayDecisionFullClose, "long", 0.5, 1900),
	}, 1900.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 || len(dms) != 0 {
		t.Fatalf("close-while-flat trades=%d applied=%v dms=%v — want consumed, no DM", trades, applied, dms)
	}

	sc, s, logger = replayMirrorTestSetup(t, "hl-paper-eth-unknown")
	applied, trades, _, dms = applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(2, "not_a_type", "long", 0.5, 1900),
	}, 1900.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 || len(dms) != 0 {
		t.Fatalf("unknown-type trades=%d applied=%v dms=%v — want consumed, no DM", trades, applied, dms)
	}

	sc, s, logger = replayMirrorTestSetup(t, "hl-paper-eth-open0")
	s.Cash = 0
	applied, trades, _, dms = applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(3, ReplayDecisionOpen, "long", 0, 1900),
	}, 1900.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 || len(dms) != 0 {
		t.Fatalf("open booked-no-trade trades=%d applied=%v dms=%v — want consumed, no DM", trades, applied, dms)
	}

	sc, s, logger = replayMirrorTestSetup(t, "hl-paper-eth-si0")
	s.Positions["ETH"] = replayHoldingETH()
	applied, trades, _, dms = applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(4, ReplayDecisionScaleIn, "long", 0, 1910),
	}, 1910.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 || len(dms) != 0 {
		t.Fatalf("scale-in booked-no-trade trades=%d applied=%v dms=%v — want consumed, no DM", trades, applied, dms)
	}

	sc, s, logger = replayMirrorTestSetup(t, "hl-paper-eth-pc0")
	s.Positions["ETH"] = replayHoldingETH()
	applied, trades, _, dms = applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(5, ReplayDecisionPartialClose, "long", 0, 1911),
	}, 1911.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 || len(dms) != 0 {
		t.Fatalf("partial-close booked-no-trade trades=%d applied=%v dms=%v — want consumed, no DM", trades, applied, dms)
	}
}

func TestMirrorReplayDriftDM_NilHookStillMarks(t *testing.T) {
	prev := replayDriftWarn
	replayDriftWarn = nil
	t.Cleanup(func() { replayDriftWarn = prev })

	sendReplayDriftWarns(nil)
	sendReplayDriftWarns([]string{"should not panic"})

	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = replayHoldingETH()
	applied, trades, _, dms := applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(1, ReplayDecisionOpen, "long", 0.5, 1908),
	}, 1908.0, replayTestResult(), &Config{}, logger)
	if trades != 0 || len(applied) != 1 || applied[0] != 1 {
		t.Fatalf("nil hook trades=%d applied=%v — row must still be marked", trades, applied)
	}
	requireOneDriftDM(t, dms, replayDriftKindOpenWhileHolding)
	sendReplayDriftWarns(dms)
}

func TestMirrorReplayDriftDM_ApplyDoesNotInvokeHook(t *testing.T) {
	var called []string
	prev := replayDriftWarn
	replayDriftWarn = func(msg string) { called = append(called, msg) }
	t.Cleanup(func() { replayDriftWarn = prev })

	sc, s, logger := replayMirrorTestSetup(t, "hl-paper-eth")
	s.Positions["ETH"] = replayHoldingETH()
	_, _, _, dms := applyReplayedLiveDecisions(sc, s, []ReplayDecision{
		replayDec(1, ReplayDecisionOpen, "long", 0.5, 1908),
	}, 1908.0, replayTestResult(), &Config{}, logger)
	if len(called) != 0 {
		t.Fatalf("apply invoked replayDriftWarn (%v) — DMs must leave via the return value after unlock", called)
	}
	requireOneDriftDM(t, dms, replayDriftKindOpenWhileHolding)
	sendReplayDriftWarns(dms)
	if len(called) != 1 || called[0] != dms[0] {
		t.Fatalf("sendReplayDriftWarns called=%v, want %v", called, dms)
	}
}

func TestMirrorReplayDriftDMAfterUnlock(t *testing.T) {
	src := string(mustReadFile(t, "main.go"))
	applyNeedle := "applyReplayedLiveDecisions(sc, stratState, pending, price, result, cfg, logger)"
	applyIdx := strings.Index(src, applyNeedle)
	if applyIdx < 0 {
		t.Fatal("replay application call not found")
	}
	blockEnd := strings.Index(src[applyIdx:], "if HedgeEnabled(sc)")
	if blockEnd < 0 {
		t.Fatal("could not bound the replay block")
	}
	block := src[applyIdx : applyIdx+blockEnd]
	unlockIdx := strings.Index(block, "mu.Unlock()")
	sendIdx := strings.Index(block, "sendReplayDriftWarns(driftDMs)")
	if sendIdx < 0 {
		t.Fatal("replay block missing sendReplayDriftWarns(driftDMs)")
	}
	if unlockIdx < 0 || sendIdx < unlockIdx {
		t.Fatal("replay drift DMs send before mu.Unlock() — SendOwnerDM would run under mu")
	}
	if !strings.Contains(src, "replayDriftWarn = func(msg string)") {
		t.Fatal("main.go missing replayDriftWarn wiring next to the persist-warn hooks")
	}

	mirrorSrc := string(mustReadFile(t, "replay_mirror.go"))
	if strings.Contains(mirrorSrc, "SendOwnerDM") {
		t.Fatal("replay_mirror.go must not SendOwnerDM; apply returns texts and the caller sends after unlock")
	}
	if strings.Contains(mirrorSrc, "shouldNotifyDrainFailure(") {
		t.Fatal("replay drift throttle must not reuse shouldNotifyDrainFailure (every-10th would violate once-per-window)")
	}
}
