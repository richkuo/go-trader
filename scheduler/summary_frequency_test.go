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
	// Trades always post, regardless of cadence setting or cycle position.
	if !ShouldPostSummary("hourly", 5, 300, false, true) {
		t.Error("trades should force a post even mid-window")
	}
	if !ShouldPostSummary("daily", 2, 300, false, true) {
		t.Error("trades should force a post even with daily cadence")
	}
}

func TestShouldPostSummary_LegacyContinuousPostsEveryCycle(t *testing.T) {
	// Empty freq + continuous (options/perps/futures) = every cycle.
	for c := 1; c <= 5; c++ {
		if !ShouldPostSummary("", c, 300, true, false) {
			t.Errorf("continuous legacy default should post every cycle; cycle %d did not", c)
		}
	}
}

func TestShouldPostSummary_LegacySpotPostsHourly(t *testing.T) {
	// Empty freq + non-continuous + interval=300s (5m) → 12 cycles between posts.
	// Cycles 1, 13, 25 post; 2..12 and 14..24 don't.
	cases := []struct {
		cycle int
		want  bool
	}{
		{1, true}, {2, false}, {6, false}, {12, false}, {13, true}, {24, false}, {25, true},
	}
	for _, tc := range cases {
		got := ShouldPostSummary("", tc.cycle, 300, false, false)
		if got != tc.want {
			t.Errorf("spot legacy cycle %d: got %v, want %v", tc.cycle, got, tc.want)
		}
	}
}

func TestShouldPostSummary_EveryAliasOverridesSpotDefault(t *testing.T) {
	// User sets spot to "every" — should post every cycle.
	for c := 1; c <= 10; c++ {
		if !ShouldPostSummary("every", c, 300, false, false) {
			t.Errorf(`freq="every" should post every cycle; cycle %d did not`, c)
		}
	}
}

func TestShouldPostSummary_HourlyAliasThrottlesContinuous(t *testing.T) {
	// User sets options (continuous) to "hourly" — should throttle to 1/hour.
	// interval=300s → 12 cycles between posts.
	if !ShouldPostSummary("hourly", 1, 300, true, false) {
		t.Error("cycle 1 should post")
	}
	if ShouldPostSummary("hourly", 2, 300, true, false) {
		t.Error("cycle 2 should be throttled")
	}
	if !ShouldPostSummary("hourly", 13, 300, true, false) {
		t.Error("cycle 13 should post (12 cycles after cycle 1)")
	}
}

func TestShouldPostSummary_CustomDuration(t *testing.T) {
	// 30m with 5m interval → every 6 cycles.
	want := map[int]bool{1: true, 2: false, 6: false, 7: true, 12: false, 13: true}
	for c, expect := range want {
		got := ShouldPostSummary("30m", c, 300, false, false)
		if got != expect {
			t.Errorf("30m cadence cycle %d: got %v, want %v", c, got, expect)
		}
	}
}

func TestShouldPostSummary_DurationShorterThanInterval(t *testing.T) {
	// 1m cadence with 5m interval — clamp to every cycle.
	for c := 1; c <= 5; c++ {
		if !ShouldPostSummary("1m", c, 300, false, false) {
			t.Errorf("sub-interval cadence should clamp to every cycle; cycle %d did not", c)
		}
	}
}

func TestShouldPostSummary_InvalidValueFallsBackToLegacy(t *testing.T) {
	// Invalid freq should fall back to the legacy default rather than crashing.
	// Continuous legacy = every cycle.
	if !ShouldPostSummary("nonsense", 3, 300, true, false) {
		t.Error("invalid freq + continuous should fall back to legacy every-cycle")
	}
	// Non-continuous legacy = hourly (12 cycles at 300s interval).
	if ShouldPostSummary("nonsense", 2, 300, false, false) {
		t.Error("invalid freq + spot should fall back to legacy hourly (cycle 2 suppressed)")
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
