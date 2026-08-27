package main

import (
	"strings"
	"testing"
	"time"
)

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

func TestHLReconcileGapTracker_ConfirmationWindow(t *testing.T) {
	tr := &HLReconcileGapTracker{}
	now := time.Now().UTC()
	for i := 1; i < hlReconcileGapAlertThreshold; i++ {
		if notify, _, count := tr.Record("ETH", 1.0, now); notify {
			t.Fatalf("cycle %d: alerted before confirmation window (count=%d)", i, count)
		}
	}
	notify, _, count := tr.Record("ETH", 1.0, now)
	if !notify {
		t.Fatalf("expected alert on threshold crossing (count=%d)", count)
	}
	if count != hlReconcileGapAlertThreshold {
		t.Fatalf("count = %d, want %d", count, hlReconcileGapAlertThreshold)
	}
}

func TestHLReconcileGapTracker_TransientSelfHeals(t *testing.T) {
	tr := &HLReconcileGapTracker{}
	now := time.Now().UTC()
	if notify, _, _ := tr.Record("ETH", 1.0, now); notify {
		t.Fatalf("cycle 1 should not alert")
	}
	if recovered, prior := tr.Clear("ETH"); recovered {
		t.Fatalf("transient clear should not be a recovery (alerted=false), prior=%d", prior)
	}

	if notify, _, count := tr.Record("ETH", 1.0, now); notify || count != 1 {
		t.Fatalf("post-clear Record = (%v, %d), want (false, 1)", notify, count)
	}
}

func TestHLReconcileGapTracker_ThrottleAndRealert(t *testing.T) {
	tr := &HLReconcileGapTracker{}
	now := time.Now().UTC()
	var firstAlertCycle int
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		notify, _, count := tr.Record("ETH", 1.0, now)
		if notify {
			firstAlertCycle = count
		}
	}
	if firstAlertCycle != hlReconcileGapAlertThreshold {
		t.Fatalf("first alert at cycle %d, want %d", firstAlertCycle, hlReconcileGapAlertThreshold)
	}

	if notify, _, _ := tr.Record("ETH", 1.0, now); notify {
		t.Fatalf("steady gap should be throttled immediately after first alert")
	}

	if notify, _, _ := tr.Record("ETH", 1.5, now); !notify {
		t.Fatalf("materially changed residual should re-alert")
	}

	if notify, _, _ := tr.Record("ETH", 1.5, now); notify {
		t.Fatalf("steady residual after re-alert should throttle")
	}
}

func TestHLReconcileGapTracker_StableGapRealertsHourlyNotEveryTenth(t *testing.T) {
	withAlertThrottleInterval(t, time.Hour)
	tr := &HLReconcileGapTracker{}
	now := time.Now().UTC()

	var lastAlertAt time.Time
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		ts := now.Add(time.Duration(i) * 3 * time.Second)
		if notify, _, _ := tr.Record("ETH", 1.0, ts); notify {
			lastAlertAt = ts
		}
	}
	if lastAlertAt.IsZero() {
		t.Fatal("confirmation alert must fire on the threshold crossing")
	}

	base := now.Add(time.Duration(hlReconcileGapAlertThreshold) * 3 * time.Second)
	for i := 1; i <= 40; i++ {
		ts := base.Add(time.Duration(i) * 3 * time.Second)
		if notify, _, count := tr.Record("ETH", 1.0, ts); notify {
			t.Fatalf("stable gap must not re-alert within the hour (cycle count=%d)", count)
		}
	}

	if notify, _, _ := tr.Record("ETH", 1.0, lastAlertAt.Add(time.Hour+time.Second)); !notify {
		t.Fatal("stable gap must re-alert once the hourly back-off elapses")
	}
}

func TestHLReconcileGapTracker_LogThrottled(t *testing.T) {
	tr := &HLReconcileGapTracker{}
	now := time.Now().UTC()

	if notify, log, _ := tr.Record("ETH", 1.0, now); notify || !log {
		t.Fatalf("onset cycle: want notify=false log=true, got notify=%v log=%v", notify, log)
	}

	if notify, log, _ := tr.Record("ETH", 1.0, now.Add(time.Second)); notify || log {
		t.Fatalf("intra-heartbeat window cycle: want notify=false log=false, got notify=%v log=%v", notify, log)
	}

	if notify, log, _ := tr.Record("ETH", 1.0, now.Add(2*time.Second)); !notify || !log {
		t.Fatalf("alert cycle: want notify=true log=true, got notify=%v log=%v", notify, log)
	}

	if notify, log, _ := tr.Record("ETH", 1.0, now.Add(3*time.Second)); notify || log {
		t.Fatalf("stable post-alert cycle: want notify=false log=false, got notify=%v log=%v", notify, log)
	}

	if _, log, _ := tr.Record("ETH", 1.5, now.Add(4*time.Second)); !log {
		t.Fatal("materially-changed residual must log within the heartbeat interval")
	}
}

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

func TestReportHLReconcileGaps_RecoveryWhenGapClears(t *testing.T) {
	hlReconcileGapTracker = &HLReconcileGapTracker{}
	dm := &recordingDMSender{}
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 1.0)})
	}
	if len(dm.msgs) != 1 {
		t.Fatalf("setup: want 1 alert, got %d", len(dm.msgs))
	}

	reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 0.0)})
	if len(dm.msgs) != 2 {
		t.Fatalf("want recovery DM, total = %d", len(dm.msgs))
	}
	if !strings.Contains(dm.msgs[1], "RESOLVED") {
		t.Errorf("second DM should be recovery: %q", dm.msgs[1])
	}

	reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 0.0)})
	if len(dm.msgs) != 2 {
		t.Fatalf("clean cycle should be silent, total = %d", len(dm.msgs))
	}
}

func TestReportHLReconcileGaps_VanishedCoinRecovers(t *testing.T) {
	hlReconcileGapTracker = &HLReconcileGapTracker{}
	dm := &recordingDMSender{}
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		reportHLReconcileGaps(dm, []hlReconcileGapResult{gapResult("ETH", 1.0)})
	}
	if len(dm.msgs) != 1 {
		t.Fatalf("setup: want 1 alert, got %d", len(dm.msgs))
	}

	reportHLReconcileGaps(dm, nil)
	if len(dm.msgs) != 2 || !strings.Contains(dm.msgs[1], "RESOLVED") {
		t.Fatalf("vanished coin should recover, msgs=%v", dm.msgs)
	}
	if len(hlReconcileGapTracker.trackedCoins()) != 0 {
		t.Fatalf("tracker should be empty after sweep, got %v", hlReconcileGapTracker.trackedCoins())
	}
}

func TestReportHLReconcileGaps_NilNotifierStillTracks(t *testing.T) {
	hlReconcileGapTracker = &HLReconcileGapTracker{}
	for i := 0; i < hlReconcileGapAlertThreshold; i++ {
		reportHLReconcileGaps(nil, []hlReconcileGapResult{gapResult("ETH", 1.0)})
	}
	if coins := hlReconcileGapTracker.trackedCoins(); len(coins) != 1 || coins[0] != "ETH" {
		t.Fatalf("nil-notifier path should still track, got %v", coins)
	}

	reportHLReconcileGaps((*MultiNotifier)(nil), []hlReconcileGapResult{gapResult("ETH", 1.0)})
}

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
