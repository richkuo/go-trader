package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseSummaryFrequency(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"empty means legacy default", "", -1, false},
		{"whitespace means legacy default", "   ", -1, false},
		{"every alias", "every", 0, false},
		{"per_check alias", "per_check", 0, false},
		{"always alias", "always", 0, false},
		{"case-insensitive alias", "Every", 0, false},
		{"hourly alias", "hourly", time.Hour, false},
		{"daily alias", "daily", 24 * time.Hour, false},
		{"go duration minutes", "30m", 30 * time.Minute, false},
		{"go duration hours", "2h", 2 * time.Hour, false},
		{"go duration combined", "1h30m", 90 * time.Minute, false},
		{"invalid string", "sometimes", 0, true},
		{"negative duration rejected", "-5m", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSummaryFrequency(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil (value=%v)", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseSummaryFrequency(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestShouldPostSummary_TradesForcePost(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	lastPost := now.Add(-10 * time.Second)

	// Trades always post, regardless of cadence setting or elapsed time.
	if !ShouldPostSummary("hourly", false, true, lastPost, now) {
		t.Error("trades should force a post even mid-window")
	}
	if !ShouldPostSummary("daily", false, true, lastPost, now) {
		t.Error("trades should force a post even with daily cadence")
	}
}

func TestShouldPostSummary_TradeForcedPostResetsCadenceWindow(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	lastByChannel := map[string]time.Time{}
	shouldPost := func(chKey string, at time.Time, hasTrades bool) bool {
		if !ShouldPostSummary("5m", false, hasTrades, lastByChannel[chKey], at) {
			return false
		}
		lastByChannel[chKey] = at
		return true
	}

	if !shouldPost("spot", now, false) {
		t.Fatal("first cadence check should post")
	}
	if shouldPost("spot", now.Add(10*time.Second), false) {
		t.Fatal("non-trade post should be suppressed before the 5m cadence elapses")
	}
	if !shouldPost("spot", now.Add(10*time.Second), true) {
		t.Fatal("trade should force a post before the cadence elapses")
	}
	if shouldPost("spot", now.Add(5*time.Minute), false) {
		t.Fatal("trade-forced post should reset the cadence window")
	}
	if !shouldPost("spot", now.Add(5*time.Minute+10*time.Second), false) {
		t.Fatal("cadence should elapse relative to the trade-forced post time")
	}
}

func TestShouldPostSummary_LegacyContinuousPostsEveryRun(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	lastPost := now

	// Empty freq + continuous (options/perps/futures) = every channel run.
	for i := 0; i < 5; i++ {
		if !ShouldPostSummary("", true, false, lastPost, now.Add(time.Duration(i)*time.Second)) {
			t.Errorf("continuous legacy default should post every run; iteration %d did not", i)
		}
	}
}

func TestShouldPostSummary_LegacySpotPostsHourly(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		lastPost time.Time
		now      time.Time
		want     bool
	}{
		{"first post", time.Time{}, now, true},
		{"before one hour", now, now.Add(time.Hour - time.Second), false},
		{"at one hour", now, now.Add(time.Hour), true},
		{"after one hour", now, now.Add(time.Hour + time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldPostSummary("", false, false, tc.lastPost, tc.now)
			if got != tc.want {
				t.Errorf("legacy spot cadence: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldPostSummary_EveryAliasOverridesSpotDefault(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	lastPost := now

	// User sets spot to "every" — should post every channel run.
	for i := 0; i < 10; i++ {
		if !ShouldPostSummary("every", false, false, lastPost, now.Add(time.Duration(i)*time.Second)) {
			t.Errorf(`freq="every" should post every run; iteration %d did not`, i)
		}
	}
}

func TestShouldPostSummary_HourlyAliasThrottlesContinuousByWallClock(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	if !ShouldPostSummary("hourly", true, false, time.Time{}, now) {
		t.Error("first run should post")
	}
	if ShouldPostSummary("hourly", true, false, now, now.Add(time.Hour-time.Second)) {
		t.Error("continuous channel should be throttled before one hour elapses")
	}
	if !ShouldPostSummary("hourly", true, false, now, now.Add(time.Hour)) {
		t.Error("continuous channel should post once one hour elapses")
	}
}

func TestShouldPostSummary_CustomDurationUsesWallClock(t *testing.T) {
	start := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	lastPost := time.Time{}
	check := func(at time.Time) bool {
		if !ShouldPostSummary("30m", false, false, lastPost, at) {
			return false
		}
		lastPost = at
		return true
	}

	if !check(start) {
		t.Fatal("first 30m cadence check should post")
	}
	if check(start.Add(30*time.Minute - time.Second)) {
		t.Fatal("30m cadence should suppress before duration elapses")
	}
	if !check(start.Add(30 * time.Minute)) {
		t.Fatal("30m cadence should post when duration elapses")
	}
	if check(start.Add(30 * time.Minute)) {
		t.Fatal("30m cadence should suppress immediately after updating last post")
	}
	if !check(start.Add(60 * time.Minute)) {
		t.Fatal("30m cadence should post again at 2x duration")
	}
}

func TestShouldPostSummary_InvalidValueFallsBackToLegacy(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	// Invalid freq should fall back to the legacy default rather than crashing.
	// Continuous legacy = every channel run.
	if !ShouldPostSummary("nonsense", true, false, now, now.Add(time.Second)) {
		t.Error("invalid freq + continuous should fall back to legacy every-run")
	}
	// Non-continuous legacy = hourly wall-clock cadence.
	if ShouldPostSummary("nonsense", false, false, now, now.Add(time.Hour-time.Second)) {
		t.Error("invalid freq + spot should fall back to legacy hourly cadence")
	}
	if !ShouldPostSummary("nonsense", false, false, now, now.Add(time.Hour)) {
		t.Error("invalid freq + spot should post once the legacy hourly cadence elapses")
	}
}

func TestValidateConfig_SummaryFrequency(t *testing.T) {
	base := &Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{ID: "s1", Type: "spot", Platform: "binanceus", Capital: 100, MaxDrawdownPct: 10, Script: "x.py"},
		},
	}

	t.Run("valid values pass", func(t *testing.T) {
		cfg := *base
		cfg.SummaryFrequency = map[string]string{
			"spot":        "hourly",
			"options":     "every",
			"hyperliquid": "30m",
		}
		if err := ValidateConfig(&cfg); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("invalid value rejected", func(t *testing.T) {
		cfg := *base
		cfg.SummaryFrequency = map[string]string{"spot": "sometimes"}
		err := ValidateConfig(&cfg)
		if err == nil {
			t.Fatal("expected validation error for invalid value")
		}
		if !strings.Contains(err.Error(), "summary_frequency") {
			t.Errorf("expected error to mention summary_frequency, got: %v", err)
		}
	})

	t.Run("empty key rejected", func(t *testing.T) {
		cfg := *base
		cfg.SummaryFrequency = map[string]string{"": "hourly"}
		err := ValidateConfig(&cfg)
		if err == nil {
			t.Fatal("expected validation error for empty key")
		}
	})
}
