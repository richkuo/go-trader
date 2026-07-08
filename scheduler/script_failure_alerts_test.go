package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestScriptFailureTracker_BelowThresholdNoAlert(t *testing.T) {
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	for i := 1; i < scriptFailureAlertThreshold; i++ {
		notify, count := tr.Record("hl-x", "list index out of range", now)
		if notify {
			t.Fatalf("failure %d: expected no alert below threshold", i)
		}
		if count != i {
			t.Fatalf("failure %d: count = %d, want %d", i, count, i)
		}
	}
}

func TestScriptFailureTracker_AlertsAtThreshold(t *testing.T) {
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	var notify bool
	var count int
	for i := 0; i < scriptFailureAlertThreshold; i++ {
		notify, count = tr.Record("hl-x", "boom", now)
	}
	if !notify {
		t.Fatalf("expected alert when streak reaches threshold %d", scriptFailureAlertThreshold)
	}
	if count != scriptFailureAlertThreshold {
		t.Fatalf("count = %d, want %d", count, scriptFailureAlertThreshold)
	}
}

func TestScriptFailureTracker_ThrottlesAfterThreshold(t *testing.T) {
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	// Reach + cross the threshold; the threshold-crossing alert fires once.
	alerts := 0
	for i := 0; i < 9; i++ {
		if notify, _ := tr.Record("hl-x", "boom", now); notify {
			alerts++
		}
	}
	// Failures 1,2 silent; 3 alerts; 4-9 throttled (next cadence hit is #10).
	if alerts != 1 {
		t.Fatalf("alerts in first 9 failures = %d, want 1 (only threshold crossing)", alerts)
	}
	// 10th failure re-alerts on the every-10th cadence.
	if notify, count := tr.Record("hl-x", "boom", now); !notify || count != 10 {
		t.Fatalf("10th failure: notify=%v count=%d, want notify=true count=10", notify, count)
	}
}

func TestScriptFailureTracker_HourlyReAlert(t *testing.T) {
	withAlertThrottleInterval(t, time.Hour)
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	for i := 0; i < scriptFailureAlertThreshold; i++ {
		tr.Record("hl-x", "boom", now) // crosses threshold, alerts at #3
	}
	// A few minutes later, still failing same error: throttled.
	if notify, _ := tr.Record("hl-x", "boom", now.Add(5*time.Minute)); notify {
		t.Fatalf("expected throttle a few minutes after threshold alert")
	}
	// An hour after the last alert: re-alert.
	if notify, _ := tr.Record("hl-x", "boom", now.Add(61*time.Minute)); !notify {
		t.Fatalf("expected hourly re-alert")
	}
}

func TestScriptFailureTracker_ErrorSignatureChangeReAlerts(t *testing.T) {
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	for i := 0; i < scriptFailureAlertThreshold; i++ {
		tr.Record("hl-x", "list index out of range", now)
	}
	// Same streak, brand-new error character: re-alert immediately even though
	// the hourly/every-10th cadence has not been hit.
	if notify, _ := tr.Record("hl-x", "connection refused", now.Add(time.Minute)); !notify {
		t.Fatalf("expected re-alert when error signature changes mid-streak")
	}
}

func TestScriptFailureTracker_ClearReportsRecovery(t *testing.T) {
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	for i := 0; i < scriptFailureAlertThreshold; i++ {
		tr.Record("hl-x", "boom", now)
	}
	recovered, prior := tr.Clear("hl-x")
	if !recovered {
		t.Fatalf("expected recovery=true after an alerted streak")
	}
	if prior != scriptFailureAlertThreshold {
		t.Fatalf("priorCount = %d, want %d", prior, scriptFailureAlertThreshold)
	}
	// After clear the streak restarts from zero.
	if notify, count := tr.Record("hl-x", "boom", now); notify || count != 1 {
		t.Fatalf("post-clear first failure: notify=%v count=%d, want notify=false count=1", notify, count)
	}
}

func TestScriptFailureTracker_ClearBelowThresholdNoRecovery(t *testing.T) {
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	tr.Record("hl-x", "boom", now) // single failure, never alerted
	if recovered, _ := tr.Clear("hl-x"); recovered {
		t.Fatalf("expected no recovery notice when threshold was never crossed")
	}
}

func TestScriptFailureTracker_ClearUnknownStrategy(t *testing.T) {
	tr := &ScriptFailureTracker{}
	if recovered, prior := tr.Clear("never-seen"); recovered || prior != 0 {
		t.Fatalf("Clear on unknown strategy: recovered=%v prior=%d, want false/0", recovered, prior)
	}
}

func TestScriptFailureTracker_IndependentPerStrategy(t *testing.T) {
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	for i := 0; i < scriptFailureAlertThreshold; i++ {
		tr.Record("hl-a", "boom", now)
	}
	// hl-b is on its own counter and stays silent.
	if notify, count := tr.Record("hl-b", "boom", now); notify || count != 1 {
		t.Fatalf("hl-b first failure: notify=%v count=%d, want false/1", notify, count)
	}
}

func TestFormatScriptFailureAlert_NamesModeAndCount(t *testing.T) {
	sc := StrategyConfig{ID: "hl-rmc-eth-live", Platform: "hyperliquid", Script: "check_hyperliquid.py"}
	pidTag := fmt.Sprintf("pid=%d", os.Getpid())
	crash := formatScriptFailureAlert(sc, scriptFailureCrash, "signal: killed", 4)
	if !strings.Contains(crash, "hard crash") || !strings.Contains(crash, "4 consecutive") ||
		!strings.Contains(crash, "hl-rmc-eth-live") || !strings.Contains(crash, "check_hyperliquid.py") ||
		!strings.Contains(crash, pidTag) {
		t.Fatalf("crash alert missing fields: %q", crash)
	}
	soft := formatScriptFailureAlert(sc, scriptFailureError, "list index out of range", 3)
	if !strings.Contains(soft, "script error") || !strings.Contains(soft, "list index out of range") {
		t.Fatalf("soft alert missing fields: %q", soft)
	}
}

func TestFormatScriptRecoveredAlert(t *testing.T) {
	sc := StrategyConfig{ID: "hl-x", Platform: "hyperliquid", Script: "check_hyperliquid.py"}
	pidTag := fmt.Sprintf("pid=%d", os.Getpid())
	msg := formatScriptRecoveredAlert(sc, 7)
	if !strings.Contains(msg, "RECOVERED") || !strings.Contains(msg, "7 consecutive") ||
		!strings.Contains(msg, pidTag) {
		t.Fatalf("recovery alert missing fields: %q", msg)
	}
}

func TestScriptFailureErrorIsTransient(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"Failed to initialize Hyperliquid Exchange client: (429, None, 'null', None)", true},
		{"hl rate limited", true},
		{"X-Cache: Error from cloudfront", true},
		{"HTTP 429 Too Many Requests", true},
		{"status_code=429 from HL", true},
		{"order oid 429 rejected by exchange", false},
		{"price crossed 42900.5", false},
		{"list index out of range", false},
		{"connection refused", false},
	}
	for _, tc := range cases {
		if got := scriptFailureErrorIsTransient(tc.msg); got != tc.want {
			t.Fatalf("transient(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestNotifyScriptFailure_BriefTransientSkipsPrimaryStreak(t *testing.T) {
	id := "transient-brief-1128"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "check_regime.py"}
	err429 := "Failed to initialize Hyperliquid Exchange client: (429, None)"
	for i := 0; i < scriptFailureAlertThreshold+2; i++ {
		notifyScriptFailure(nil, sc, scriptFailureError, err429)
	}
	shouldNotify, count := scriptFailureTracker.Record(id, "connection refused", time.Now().UTC())
	if shouldNotify || count != 1 {
		t.Fatalf("brief transient must not advance primary streak: notify=%v count=%d, want false/1", shouldNotify, count)
	}
}

func TestNotifyScriptFailure_SustainedTransientAlerts(t *testing.T) {
	id := "transient-sustained-1128"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "check_regime.py"}
	err429 := "Failed to initialize Hyperliquid Exchange client: (429, None)"
	for i := 0; i < scriptFailureTransientAlertThreshold; i++ {
		notifyScriptFailure(nil, sc, scriptFailureError, err429)
	}
	recovered, prior := scriptFailureTransientTracker.Clear(id)
	if !recovered || prior != scriptFailureTransientAlertThreshold {
		t.Fatalf("sustained transient: recovered=%v prior=%d, want true/%d", recovered, prior, scriptFailureTransientAlertThreshold)
	}
}

func TestNotifyScriptFailure_AlternatingTransientAndRealStillAlerts(t *testing.T) {
	id := "transient-alternate-1128"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "check_hyperliquid.py"}
	err429 := "Failed to initialize Hyperliquid Exchange client: (429, None)"
	for i := 0; i < scriptFailureAlertThreshold; i++ {
		notifyScriptFailure(nil, sc, scriptFailureError, err429)
		notifyScriptFailure(nil, sc, scriptFailureError, "connection refused")
	}
	recovered, prior := scriptFailureTracker.Clear(id)
	if !recovered || prior != scriptFailureAlertThreshold {
		t.Fatalf("alternating real errors must alert primary tracker: recovered=%v prior=%d", recovered, prior)
	}
}

func TestClearScriptFailure_ClearsTransientTracker(t *testing.T) {
	id := "transient-clear-1128"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	now := time.Now().UTC()
	for i := 0; i < scriptFailureTransientAlertThreshold; i++ {
		recordScriptFailureAtThreshold(scriptFailureTransientTracker, id, "HTTP 429", now, scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration)
	}
	recovered, prior := scriptFailureTransientTracker.Clear(id)
	if !recovered || prior != scriptFailureTransientAlertThreshold {
		t.Fatalf("transient clear: recovered=%v prior=%d", recovered, prior)
	}
}

func TestScriptFailureTransientTracker_WallClockEscalation(t *testing.T) {
	tr := &ScriptFailureTracker{}
	t0 := time.Unix(1700000000, 0).UTC()
	err429 := "HTTP 429"
	id := "slow-interval-wallclock-1128"

	if notify, count := recordScriptFailureAtThreshold(tr, id, err429, t0, scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration); notify || count != 1 {
		t.Fatalf("first failure: notify=%v count=%d, want false/1", notify, count)
	}
	// Two sparse 1h-interval failures: count stays below 15 but wall-clock bound fires.
	tLate := t0.Add(scriptFailureTransientAlertMaxDuration + time.Minute)
	if notify, count := recordScriptFailureAtThreshold(tr, id, err429, tLate, scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration); !notify || count != 2 {
		t.Fatalf("wall-clock escalation: notify=%v count=%d, want true/2", notify, count)
	}
}

func TestScriptFailureTransientTracker_BriefStormStaysSilent(t *testing.T) {
	tr := &ScriptFailureTracker{}
	t0 := time.Unix(1700000000, 0).UTC()
	err429 := "HTTP 429"
	id := "brief-storm-wallclock-1128"

	for i := 1; i < scriptFailureTransientAlertThreshold; i++ {
		if notify, count := recordScriptFailureAtThreshold(tr, id, err429, t0.Add(time.Duration(i)*time.Second), scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration); notify {
			t.Fatalf("failure %d: unexpected alert during brief storm under count and duration bounds", i)
		} else if count != i {
			t.Fatalf("failure %d: count=%d, want %d", i, count, i)
		}
	}
}

func TestScriptFailureTransientTracker_RecoveryResetsWallClock(t *testing.T) {
	tr := &ScriptFailureTracker{}
	t0 := time.Unix(1700000000, 0).UTC()
	err429 := "HTTP 429"
	id := "wallclock-recovery-1128"

	recordScriptFailureAtThreshold(tr, id, err429, t0, scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration)
	tr.Clear(id)
	if notify, count := recordScriptFailureAtThreshold(tr, id, err429, t0.Add(scriptFailureTransientAlertMaxDuration+time.Minute), scriptFailureTransientAlertThreshold, scriptFailureTransientAlertMaxDuration); notify || count != 1 {
		t.Fatalf("post-recovery streak must restart: notify=%v count=%d, want false/1", notify, count)
	}
}
