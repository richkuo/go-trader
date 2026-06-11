package main

import (
	"strings"
	"testing"
	"time"
)

// recordingDMSender captures every owner DM for assertions.
type recordingDMSender struct {
	msgs []string
}

func (r *recordingDMSender) SendOwnerDM(content string) { r.msgs = append(r.msgs, content) }

func gapResult(coin string, delta float64) hlReconcileGapResult {
	return hlReconcileGapResult{
		Coin:       coin,
		DeltaQty:   delta,
		VirtualQty: 1.0,
		OnChainQty: 1.0 - delta,
		Strategies: []string{"hl-owner-" + coin, "hl-peer-" + coin},
	}
}

// TestHLReconcileGapTracker_ConfirmationWindow verifies no alert fires until a
// gap persists hlReconcileGapAlertThreshold consecutive cycles, then exactly
// one alert on the crossing.
func TestHLReconcileGapTracker_ConfirmationWindow(t *testing.T) {
	tr := &HLReconcileGapTracker{}
	now := time.Now().UTC()
	for i := 1; i < hlReconcileGapAlertThreshold; i++ {
		if notify, count := tr.Record("ETH", 1.0, now); notify {
			t.Fatalf("cycle %d: alerted before confirmation window (count=%d)", i, count)
		}
	}
	notify, count := tr.Record("ETH", 1.0, now)
	if !notify {
		t.Fatalf("expected alert on threshold crossing (count=%d)", count)
	}
	if count != hlReconcileGapAlertThreshold {
		t.Fatalf("count = %d, want %d", count, hlReconcileGapAlertThreshold)
	}
}

// TestHLReconcileGapTracker_TransientSelfHeals verifies a gap that clears before
// the confirmation window never alerts (and re-arms its streak afterward).
func TestHLReconcileGapTracker_TransientSelfHeals(t *testing.T) {
	tr := &HLReconcileGapTracker{}
	now := time.Now().UTC()
	if notify, _ := tr.Record("ETH", 1.0, now); notify {
		t.Fatalf("cycle 1 should not alert")
	}
	if recovered, prior := tr.Clear("ETH"); recovered {
		t.Fatalf("transient clear should not be a recovery (alerted=false), prior=%d", prior)
	}
	// Streak fully reset: a fresh gap starts the window over.
	if notify, count := tr.Record("ETH", 1.0, now); notify || count != 1 {
		t.Fatalf("post-clear Record = (%v, %d), want (false, 1)", notify, count)
	}
}

// TestHLReconcileGapTracker_ThrottleAndRealert verifies that after the first
// alert the tracker re-throttles, re-alerts on a materially changed residual,
// and otherwise only on the every-10th-cycle cadence.
func TestHLReconcileGapTracker_ThrottleAndRealert(t *testing.T) {
	tr := &HLReconcileGapTracker{}
	now := time.Now().UTC()
	var firstAlertCycle int
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		notify, count := tr.Record("ETH", 1.0, now)
		if notify {
			firstAlertCycle = count
		}
	}
	if firstAlertCycle != hlReconcileGapAlertThreshold {
		t.Fatalf("first alert at cycle %d, want %d", firstAlertCycle, hlReconcileGapAlertThreshold)
	}
	// Same residual next cycle: throttled (not 10th, not materially changed).
	if notify, _ := tr.Record("ETH", 1.0, now); notify {
		t.Fatalf("steady gap should be throttled immediately after first alert")
	}
	// Materially changed residual (>10% move): re-alert.
	if notify, _ := tr.Record("ETH", 1.5, now); !notify {
		t.Fatalf("materially changed residual should re-alert")
	}
	// Back to throttled for a steady residual.
	if notify, _ := tr.Record("ETH", 1.5, now); notify {
		t.Fatalf("steady residual after re-alert should throttle")
	}
}

// TestHLReconcileGapTracker_Recovery verifies Clear reports recovery only once
// a gap actually alerted.
func TestHLReconcileGapTracker_Recovery(t *testing.T) {
	tr := &HLReconcileGapTracker{}
	now := time.Now().UTC()
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		tr.Record("ETH", 1.0, now)
	}
	recovered, prior := tr.Clear("ETH")
	if !recovered {
		t.Fatalf("alerted gap should report recovery on Clear")
	}
	if prior != hlReconcileGapAlertThreshold {
		t.Fatalf("recovery prior count = %d, want %d", prior, hlReconcileGapAlertThreshold)
	}
}

// TestReportHLReconcileGaps_AlertsAfterWindow drives the report entrypoint over
// several cycles and asserts exactly one alert DM fires on the confirmation
// crossing, none before, and that the message carries the residual + fail-closed
// language.
func TestReportHLReconcileGaps_AlertsAfterWindow(t *testing.T) {
	hlReconcileGapTracker = &HLReconcileGapTracker{}
	dm := &recordingDMSender{}
	for i := 1; i < hlReconcileGapAlertThreshold; i++ {
		reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 1.0)})
		if len(dm.msgs) != 0 {
			t.Fatalf("cycle %d: %d DMs before window, want 0", i, len(dm.msgs))
		}
	}
	reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 1.0)})
	if len(dm.msgs) != 1 {
		t.Fatalf("expected 1 alert DM at window crossing, got %d", len(dm.msgs))
	}
	got := dm.msgs[0]
	if !strings.Contains(got, "HL RECONCILE GAP") || !strings.Contains(got, "ETH") {
		t.Errorf("alert missing header/coin: %q", got)
	}
	if !strings.Contains(got, "fail-closed") {
		t.Errorf("alert should state fail-closed invariant: %q", got)
	}
}

// TestReportHLReconcileGaps_RecoveryWhenGapClears verifies a within-tolerance
// cycle after an alerting gap emits exactly one recovery DM.
func TestReportHLReconcileGaps_RecoveryWhenGapClears(t *testing.T) {
	hlReconcileGapTracker = &HLReconcileGapTracker{}
	dm := &recordingDMSender{}
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 1.0)})
	}
	if len(dm.msgs) != 1 {
		t.Fatalf("setup: want 1 alert, got %d", len(dm.msgs))
	}
	// Gap clears (residual within tolerance) → recovery.
	reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 0.0)})
	if len(dm.msgs) != 2 {
		t.Fatalf("want recovery DM, total = %d", len(dm.msgs))
	}
	if !strings.Contains(dm.msgs[1], "RESOLVED") {
		t.Errorf("second DM should be recovery: %q", dm.msgs[1])
	}
	// A subsequent clean cycle emits nothing further.
	reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 0.0)})
	if len(dm.msgs) != 2 {
		t.Fatalf("clean cycle should be silent, total = %d", len(dm.msgs))
	}
}

// TestReportHLReconcileGaps_VanishedCoinRecovers verifies a coin that drops out
// of the gap map entirely (no longer shared) recovers via the sweep.
func TestReportHLReconcileGaps_VanishedCoinRecovers(t *testing.T) {
	hlReconcileGapTracker = &HLReconcileGapTracker{}
	dm := &recordingDMSender{}
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 1.0)})
	}
	if len(dm.msgs) != 1 {
		t.Fatalf("setup: want 1 alert, got %d", len(dm.msgs))
	}
	// ETH no longer present in the gap map (peer closed → not shared).
	reportHLReconcileGaps(dm, nil)
	if len(dm.msgs) != 2 || !strings.Contains(dm.msgs[1], "RESOLVED") {
		t.Fatalf("vanished coin should recover, msgs=%v", dm.msgs)
	}
	if len(hlReconcileGapTracker.trackedCoins()) != 0 {
		t.Fatalf("tracker should be empty after sweep, got %v", hlReconcileGapTracker.trackedCoins())
	}
}

// TestReportHLReconcileGaps_NilNotifierStillTracks verifies recording proceeds
// without a notifier (so counts/recovery stay correct) and does not panic on a
// nil sender.
func TestReportHLReconcileGaps_NilNotifierStillTracks(t *testing.T) {
	hlReconcileGapTracker = &HLReconcileGapTracker{}
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		reportHLReconcileGaps(nil, []hlReconcileGapResult{gapResult("ETH", 1.0)})
	}
	if coins := hlReconcileGapTracker.trackedCoins(); len(coins) != 1 || coins[0] != "ETH" {
		t.Fatalf("nil-notifier path should still track, got %v", coins)
	}
	// Nil *MultiNotifier (typed nil) must also be safe.
	reportHLReconcileGaps((*MultiNotifier)(nil), []hlReconcileGapResult{gapResult("ETH", 1.0)})
}

// TestReportHLReconcileGaps_BelowToleranceNoAlert verifies sub-epsilon residuals
// are treated as noise (no streak, no alert).
func TestReportHLReconcileGaps_BelowToleranceNoAlert(t *testing.T) {
	hlReconcileGapTracker = &HLReconcileGapTracker{}
	dm := &recordingDMSender{}
	for i := 0; i < hlReconcileGapAlertThreshold+2; i++ {
		reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", hlReconcileGapTolerance/2)})
	}
	if len(dm.msgs) != 0 {
		t.Fatalf("sub-tolerance residual should never alert, got %d DMs", len(dm.msgs))
	}
	if len(hlReconcileGapTracker.trackedCoins()) != 0 {
		t.Fatalf("sub-tolerance residual should not create a streak, got %v", hlReconcileGapTracker.trackedCoins())
	}
}
