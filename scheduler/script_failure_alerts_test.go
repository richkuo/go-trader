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
		msg   string
		want  bool
	}{
		{"Failed to initialize Hyperliquid Exchange client: (429, None, 'null', None)", true},
		{"hl rate limited", true},
		{"X-Cache: Error from cloudfront", true},
		{"list index out of range", false},
		{"connection refused", false},
	}
	for _, tc := range cases {
		if got := scriptFailureErrorIsTransient(tc.msg); got != tc.want {
			t.Fatalf("transient(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestNotifyScriptFailure_Transient429SkipsStreak(t *testing.T) {
	id := "transient-skip-1128"
	defer scriptFailureTracker.Clear(id)
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "check_regime.py"}
	err429 := "Failed to initialize Hyperliquid Exchange client: (429, None)"
	for i := 0; i < scriptFailureAlertThreshold+2; i++ {
		notifyScriptFailure(nil, sc, scriptFailureError, err429)
	}
	shouldNotify, count := scriptFailureTracker.Record(id, "connection refused", time.Now().UTC())
	if shouldNotify || count != 1 {
		t.Fatalf("transient-only notify must not advance streak: notify=%v count=%d, want false/1", shouldNotify, count)
	}
}
