package main

import (
	"testing"
	"time"
)

func TestShouldNotifyDrainFailure(t *testing.T) {
	base := time.Now()
	cases := []struct {
		name           string
		count          int
		lastNotifiedAt time.Time
		now            time.Time
		hourlyThrottle bool
		want           bool
	}{
		{name: "first failure notifies", count: 1, lastNotifiedAt: time.Time{}, now: base, want: true},
		{name: "second suppressed", count: 2, lastNotifiedAt: base, now: base.Add(time.Minute), want: false},
		{name: "fifth suppressed", count: 5, lastNotifiedAt: base, now: base.Add(time.Minute), want: false},
		{name: "ninth suppressed", count: 9, lastNotifiedAt: base, now: base.Add(time.Minute), want: false},
		{name: "tenth notifies", count: 10, lastNotifiedAt: base, now: base.Add(time.Minute), want: true},
		{name: "twentieth notifies", count: 20, lastNotifiedAt: base, now: base.Add(time.Minute), want: true},
		{name: "thirtieth notifies", count: 30, lastNotifiedAt: base, now: base.Add(time.Minute), want: true},
		{name: "hourly notifies off cadence", count: 5, lastNotifiedAt: base, now: base.Add(61 * time.Minute), hourlyThrottle: true, want: true},
		{name: "zero lastNotifiedAt suppresses mid count", count: 5, lastNotifiedAt: time.Time{}, now: base, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hourlyThrottle {
				withAlertThrottleInterval(t, time.Hour)
			}
			if got := shouldNotifyDrainFailure(tc.count, tc.lastNotifiedAt, tc.now); got != tc.want {
				t.Errorf("shouldNotifyDrainFailure(%d) = %v, want %v", tc.count, got, tc.want)
			}
		})
	}
}

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
	notify, _ := th.Record("k1", "err", now)
	if !notify {
		t.Fatal("first call must notify")
	}
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
	th.Record("k1", "error-type-A", now.Add(time.Minute))
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
	withAlertThrottleInterval(t, time.Hour)
	th := &LiveExecFailureThrottle{}
	now := time.Now()
	th.Record("k1", "err", now)
	th.Record("k1", "err", now.Add(30*time.Minute))
	notify, _ := th.Record("k1", "err", now.Add(65*time.Minute))
	if !notify {
		t.Error("expected hourly alert after 65 minutes")
	}
}
