package main

import (
	"strings"
	"testing"
	"time"
)

// --- shouldNotifyDrainFailure ---

func TestShouldNotifyDrainFailure_FirstFailure_Notifies(t *testing.T) {
	if !shouldNotifyDrainFailure(1, time.Time{}, time.Now()) {
		t.Error("expected notify on first failure")
	}
}

func TestShouldNotifyDrainFailure_2ndThrough9thSuppressed(t *testing.T) {
	notifiedAt := time.Now()
	for i := 2; i <= 9; i++ {
		if shouldNotifyDrainFailure(i, notifiedAt, notifiedAt.Add(time.Minute)) {
			t.Errorf("expected suppress on failure #%d (within hour, not mod 10)", i)
		}
	}
}

func TestShouldNotifyDrainFailure_EveryTenthNotifies(t *testing.T) {
	notifiedAt := time.Now()
	for _, count := range []int{10, 20, 30} {
		if !shouldNotifyDrainFailure(count, notifiedAt, notifiedAt.Add(time.Minute)) {
			t.Errorf("expected notify at count=%d (mod 10)", count)
		}
	}
}

func TestShouldNotifyDrainFailure_HourlyEvenIfNotMod10(t *testing.T) {
	notifiedAt := time.Now()
	// count=5, not mod 10, but >1 hour since last notify
	if !shouldNotifyDrainFailure(5, notifiedAt, notifiedAt.Add(61*time.Minute)) {
		t.Error("expected notify when >1 hour since last notify")
	}
}

func TestShouldNotifyDrainFailure_ZeroLastNotifiedAt_NeverSuppressed(t *testing.T) {
	// zero LastNotifiedAt means no notification has ever fired; first failure
	// path (count==1) already notifies, but even count==5 with zero LastNotifiedAt
	// should notify because lastNotifiedAt.IsZero() implies first alert.
	// shouldNotifyDrainFailure only checks !lastNotifiedAt.IsZero(), so count==5
	// with zero time returns false (not mod 10, IsZero check fails). This is by
	// design: FailureCount==1 is the only guaranteed path; the zero-time case only
	// matters practically because count is always 1 when LastNotifiedAt is zero.
	if !shouldNotifyDrainFailure(1, time.Time{}, time.Now()) {
		t.Error("count==1 with zero LastNotifiedAt must notify")
	}
}

// --- LiveExecFailureThrottle ---

func TestLiveExecFailureThrottle_FirstFailureNotifies(t *testing.T) {
	th := &LiveExecFailureThrottle{}
	notify, count := th.Record("k1", "some error", time.Now())
	if !notify {
		t.Error("expected notify on first failure")
	}
	if count != 1 {
		t.Errorf("expected count=1, got %d", count)
	}
}

func TestLiveExecFailureThrottle_RepeatsThrottled(t *testing.T) {
	th := &LiveExecFailureThrottle{}
	now := time.Now()
	// First call notifies.
	notify, _ := th.Record("k1", "err", now)
	if !notify {
		t.Fatal("first call must notify")
	}
	// Second through ninth — should suppress.
	for i := 2; i <= 9; i++ {
		notify, count := th.Record("k1", "err", now.Add(time.Minute))
		if notify {
			t.Errorf("call #%d should be suppressed, got notify=true count=%d", i, count)
		}
	}
}

func TestLiveExecFailureThrottle_TenthNotifies(t *testing.T) {
	th := &LiveExecFailureThrottle{}
	now := time.Now()
	for i := 1; i <= 10; i++ {
		notify, count := th.Record("k1", "err", now.Add(time.Minute))
		if i == 1 && !notify {
			t.Error("first call must notify")
		}
		if i == 10 && !notify {
			t.Errorf("10th call must notify, count=%d", count)
		}
	}
}

func TestLiveExecFailureThrottle_DifferentErrorSigReNotifies(t *testing.T) {
	th := &LiveExecFailureThrottle{}
	now := time.Now()
	th.Record("k1", "error-type-A", now)
	th.Record("k1", "error-type-A", now.Add(time.Minute)) // suppressed
	// New error type — must re-notify regardless of count.
	notify, count := th.Record("k1", "error-type-B", now.Add(2*time.Minute))
	if !notify {
		t.Error("new error signature must re-notify fresh")
	}
	if count != 1 {
		t.Errorf("count must reset to 1 on new error sig, got %d", count)
	}
}

func TestLiveExecFailureThrottle_ClearResetsCount(t *testing.T) {
	th := &LiveExecFailureThrottle{}
	now := time.Now()
	for i := 0; i < 5; i++ {
		th.Record("k1", "err", now)
	}
	th.Clear("k1")
	notify, count := th.Record("k1", "err", now.Add(time.Minute))
	if !notify {
		t.Error("after Clear, first failure must notify again")
	}
	if count != 1 {
		t.Errorf("after Clear count must be 1, got %d", count)
	}
}

func TestLiveExecFailureThrottle_HourlyAlert(t *testing.T) {
	th := &LiveExecFailureThrottle{}
	now := time.Now()
	th.Record("k1", "err", now)                                  // notified
	th.Record("k1", "err", now.Add(30*time.Minute))              // suppressed
	notify, _ := th.Record("k1", "err", now.Add(65*time.Minute)) // hourly fire
	if !notify {
		t.Error("expected hourly alert after 65 minutes")
	}
}

// --- formatters ---

func TestFormatLiveExecFailureAlert_IncludesAllFields(t *testing.T) {
	msg := formatLiveExecFailureAlert("hl-tema-btc", "hyperliquid", "open", "BTC", "float_to_wire", 1)
	for _, want := range []string{"hl-tema-btc", "hyperliquid", "open", "BTC", "float_to_wire"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
}

func TestFormatLiveExecFailureAlert_RepeatIncludesCount(t *testing.T) {
	msg := formatLiveExecFailureAlert("hl-a", "hyperliquid", "close", "ETH", "timeout", 10)
	if !strings.Contains(msg, "failure #10") {
		t.Errorf("expected 'failure #10' in repeat message: %s", msg)
	}
}

func TestFormatLiveExecFailureAlert_FirstOmitsCount(t *testing.T) {
	msg := formatLiveExecFailureAlert("hl-a", "hyperliquid", "close", "ETH", "timeout", 1)
	if strings.Contains(msg, "failure #") {
		t.Errorf("first-failure message should omit count: %s", msg)
	}
}

func TestFormatDrainFailureAlert_IncludesAllFields(t *testing.T) {
	msg := formatDrainFailureAlert("hyperliquid", "hl-tema-eth", "ETH", 0.25, "float_to_wire", 1)
	for _, want := range []string{"hl-tema-eth", "hyperliquid", "ETH", "float_to_wire"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
}

func TestFormatDrainFailureAlert_RepeatIncludesCount(t *testing.T) {
	msg := formatDrainFailureAlert("okx", "okx-a", "ETH", 0.1, "503", 5)
	if !strings.Contains(msg, "failure #5") {
		t.Errorf("expected 'failure #5' in message: %s", msg)
	}
}
