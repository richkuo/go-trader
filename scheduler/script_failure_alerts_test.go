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

	alerts := 0
	for i := 0; i < 9; i++ {
		if notify, _ := tr.Record("hl-x", "boom", now); notify {
			alerts++
		}
	}

	if alerts != 1 {
		t.Fatalf("alerts in first 9 failures = %d, want 1 (only threshold crossing)", alerts)
	}

	if notify, count := tr.Record("hl-x", "boom", now); !notify || count != 10 {
		t.Fatalf("10th failure: notify=%v count=%d, want notify=true count=10", notify, count)
	}
}

func TestScriptFailureTracker_HourlyReAlert(t *testing.T) {
	withAlertThrottleInterval(t, time.Hour)
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	for i := 0; i < scriptFailureAlertThreshold; i++ {
		tr.Record("hl-x", "boom", now)
	}

	if notify, _ := tr.Record("hl-x", "boom", now.Add(5*time.Minute)); notify {
		t.Fatalf("expected throttle a few minutes after threshold alert")
	}

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

	if notify, count := tr.Record("hl-x", "boom", now); notify || count != 1 {
		t.Fatalf("post-clear first failure: notify=%v count=%d, want notify=false count=1", notify, count)
	}
}

func TestScriptFailureTracker_ClearBelowThresholdNoRecovery(t *testing.T) {
	tr := &ScriptFailureTracker{}
	now := time.Unix(1700000000, 0).UTC()
	tr.Record("hl-x", "boom", now)
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
		{"(502, '<html>...')", true},
		{"regime bundle hyperliquid/BNB/30m: (502, '<html>...502 Bad Gateway...</html>')", true},
		{"(502, None)", true},
		{"HTTP 502 Bad Gateway", true},
		{"status_code=503 from upstream", true},
		{"HTTP 500 Internal Server Error", true},
		{"order oid 429 rejected by exchange", false},
		{"price crossed 42900.5", false},
		{"list index out of range", false},
		{"connection refused", false},
		{"status: 504", false},
		{"500 Internal Server Error", false},
		{"<html> parser expected closing tag", false},
		{"price 5500.50", false},
		{"HTTP 404 Not Found", false},
		{"(400, 'Bad Request')", false},
		{"NameError: foo", false},
		{"script timed out after 30s", true},
		{"regime bundle hyperliquid/HYPE/30m: script timed out after 30s", true},
		{"script error: script timed out after 30s (stderr: )", true},
		{"fatal error: all goroutines are asleep - deadlock!", false},
		{"NameError: name 'foo' is not defined", false},
		{"ImportError: No module named 'foo'", false},
		{"cancelled at phase-budget seal (45s); signature unavailable this cycle", false},
		{`Post "https://api.hyperliquid.xyz/info": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`, false},
	}
	for _, tc := range cases {
		if got := scriptFailureErrorIsTransient(tc.msg); got != tc.want {
			t.Fatalf("transient(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestFormatScriptFailureTransientAlert_NamesUpstreamError(t *testing.T) {
	sc := StrategyConfig{ID: "regime[hyperliquid/BNB/30m]", Platform: "hyperliquid", Script: "check_regime.py"}
	msg := formatScriptFailureTransientAlert(sc, scriptFailureError, "regime bundle hyperliquid/BNB/30m: (502, None)", 15)
	if !strings.Contains(msg, "upstream error") {
		t.Fatalf("transient alert must name upstream error: %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "throttle") {
		t.Fatalf("transient alert must not call 5xx a throttle: %q", msg)
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

const wrapped502BundleErr = "regime bundle hyperliquid/BNB/30m: (502, '<html>\\r\\n<head><title>502 Bad Gateway</title></head>\\r\\n<body>\\r\\n<center><h1>502 Bad Gateway</h1></center>\\r\\n<hr><center>nginx/1.22.1</center>\\r\\n</body>\\r\\n</html>\\r\\n')"

func TestNotifyScriptFailure_Wrapped502SkipsPrimaryStreak(t *testing.T) {
	id := "regime[hyperliquid/BNB/30m]"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "shared_scripts/check_regime.py"}
	for i := 0; i < scriptFailureAlertThreshold; i++ {
		notifyScriptFailure(nil, sc, scriptFailureError, wrapped502BundleErr)
	}
	recovered, prior := scriptFailureTracker.Clear(id)
	if recovered || prior != 0 {
		t.Fatalf("three wrapped 502s must not touch primary tracker: recovered=%v prior=%d, want false/0", recovered, prior)
	}
	transientRecovered, transientPrior := scriptFailureTransientTracker.Clear(id)
	if transientRecovered || transientPrior != scriptFailureAlertThreshold {
		t.Fatalf("wrapped 502 must increment transient tracker only: recovered=%v prior=%d, want false/%d", transientRecovered, transientPrior, scriptFailureAlertThreshold)
	}
}

func TestNotifyScriptFailure_Sustained502AlertsTransient(t *testing.T) {
	id := "regime[hyperliquid/BNB/30m]-sustained"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "shared_scripts/check_regime.py"}
	for i := 0; i < scriptFailureTransientAlertThreshold; i++ {
		notifyScriptFailure(nil, sc, scriptFailureError, wrapped502BundleErr)
	}
	recovered, prior := scriptFailureTransientTracker.Clear(id)
	if !recovered || prior != scriptFailureTransientAlertThreshold {
		t.Fatalf("sustained 502: recovered=%v prior=%d, want true/%d", recovered, prior, scriptFailureTransientAlertThreshold)
	}
	primaryRecovered, primaryPrior := scriptFailureTracker.Clear(id)
	if primaryRecovered || primaryPrior != 0 {
		t.Fatalf("sustained 502 must leave primary at 0: recovered=%v prior=%d", primaryRecovered, primaryPrior)
	}
}

func TestNotifyScriptFailure_502ThenRealStartsPrimary(t *testing.T) {
	id := "regime[hyperliquid/BNB/30m]-handoff"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "shared_scripts/check_regime.py"}
	notifyScriptFailure(nil, sc, scriptFailureError, wrapped502BundleErr)
	notifyScriptFailure(nil, sc, scriptFailureError, "NameError: foo")
	recovered, prior := scriptFailureTracker.Clear(id)
	if recovered || prior != 1 {
		t.Fatalf("real error after 502 must start primary at 1: recovered=%v prior=%d, want false/1", recovered, prior)
	}
}

func TestClearScriptFailure_Clears502TransientTracker(t *testing.T) {
	id := "regime[hyperliquid/BNB/30m]-clear"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "shared_scripts/check_regime.py"}
	notifyScriptFailure(nil, sc, scriptFailureError, wrapped502BundleErr)
	clearScriptFailure(nil, sc)
	_, transientPrior := scriptFailureTransientTracker.Clear(id)
	if transientPrior != 0 {
		t.Fatalf("clearScriptFailure must reset 502 transient streak: prior=%d, want 0", transientPrior)
	}
}

const wrappedTimeoutErr = "script error: script timed out after 30s (stderr: )"
const regimeTimeoutErr = "regime bundle hyperliquid/HYPE/30m: script timed out after 30s"

func TestNotifyScriptFailure_WrappedTimeoutSkipsPrimaryStreak(t *testing.T) {
	id := "regime[hyperliquid/HYPE/30m]"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "shared_scripts/check_regime.py"}
	for i := 0; i < scriptFailureAlertThreshold; i++ {
		notifyScriptFailure(nil, sc, scriptFailureError, wrappedTimeoutErr)
	}
	recovered, prior := scriptFailureTracker.Clear(id)
	if recovered || prior != 0 {
		t.Fatalf("three wrapped timeouts must not fire primary: recovered=%v prior=%d, want false/0", recovered, prior)
	}
	transientRecovered, transientPrior := scriptFailureTransientTracker.Clear(id)
	if transientRecovered || transientPrior != scriptFailureAlertThreshold {
		t.Fatalf("wrapped timeout must increment transient tracker only: recovered=%v prior=%d, want false/%d", transientRecovered, transientPrior, scriptFailureAlertThreshold)
	}
}

func TestNotifyScriptFailure_SustainedTimeoutAlertsTransient(t *testing.T) {
	id := "regime[hyperliquid/HYPE/30m]-sustained"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "shared_scripts/check_regime.py"}
	for i := 0; i < scriptFailureTransientAlertThreshold; i++ {
		notifyScriptFailure(nil, sc, scriptFailureError, wrappedTimeoutErr)
	}
	recovered, prior := scriptFailureTransientTracker.Clear(id)
	if !recovered || prior != scriptFailureTransientAlertThreshold {
		t.Fatalf("sustained timeout: recovered=%v prior=%d, want true/%d", recovered, prior, scriptFailureTransientAlertThreshold)
	}
	primaryRecovered, primaryPrior := scriptFailureTracker.Clear(id)
	if primaryRecovered || primaryPrior != 0 {
		t.Fatalf("sustained timeout must leave primary at 0: recovered=%v prior=%d", primaryRecovered, primaryPrior)
	}
}

func TestNotifyScriptFailure_502ThenTimeoutStaysTransient(t *testing.T) {
	id := "regime[hyperliquid/HYPE/30m]-mixed"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "shared_scripts/check_regime.py"}
	notifyScriptFailure(nil, sc, scriptFailureError, wrapped502BundleErr)
	notifyScriptFailure(nil, sc, scriptFailureError, wrappedTimeoutErr)
	recovered, prior := scriptFailureTracker.Clear(id)
	if recovered || prior != 0 {
		t.Fatalf("502 then timeout must leave primary at 0: recovered=%v prior=%d", recovered, prior)
	}
	_, transientPrior := scriptFailureTransientTracker.Clear(id)
	if transientPrior != 2 {
		t.Fatalf("502 then timeout must increment transient only: prior=%d, want 2", transientPrior)
	}
}

func TestNotifyScriptFailure_TimeoutThenRealStartsPrimary(t *testing.T) {
	id := "regime[hyperliquid/HYPE/30m]-handoff"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "shared_scripts/check_regime.py"}
	notifyScriptFailure(nil, sc, scriptFailureError, wrappedTimeoutErr)
	notifyScriptFailure(nil, sc, scriptFailureError, "NameError: foo")
	recovered, prior := scriptFailureTracker.Clear(id)
	if recovered || prior != 1 {
		t.Fatalf("real error after timeout must start primary at 1: recovered=%v prior=%d, want false/1", recovered, prior)
	}
}

func TestClearScriptFailure_ClearsTimeoutTransientTracker(t *testing.T) {
	id := "regime[hyperliquid/HYPE/30m]-clear"
	defer func() {
		scriptFailureTracker.Clear(id)
		scriptFailureTransientTracker.Clear(id)
	}()
	sc := StrategyConfig{ID: id, Platform: "hyperliquid", Script: "shared_scripts/check_regime.py"}
	notifyScriptFailure(nil, sc, scriptFailureError, regimeTimeoutErr)
	clearScriptFailure(nil, sc)
	_, transientPrior := scriptFailureTransientTracker.Clear(id)
	if transientPrior != 0 {
		t.Fatalf("clearScriptFailure must reset timeout transient streak: prior=%d, want 0", transientPrior)
	}
	_, primaryPrior := scriptFailureTracker.Clear(id)
	if primaryPrior != 0 {
		t.Fatalf("clearScriptFailure must reset primary streak: prior=%d, want 0", primaryPrior)
	}
}
