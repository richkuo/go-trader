package main

import (
	"testing"
	"time"
)

func TestEffectiveStrategyIntervalSeconds(t *testing.T) {
	fast := strategyDrawdownFastIntervalSeconds
	cases := []struct {
		name     string
		interval int
		risk     RiskState
		want     int
	}{
		{
			name:     "drawdown warning uses fast interval",
			interval: 3600,
			risk:     RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 8.5},
			want:     fast,
		},
		{
			name:     "drawdown recovery reverts to normal interval",
			interval: 3600,
			risk:     RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 7.5},
			want:     3600,
		},
		{
			name:     "warning strategy speeds up",
			interval: 3600,
			risk:     RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 8.1},
			want:     fast,
		},
		{
			name:     "non-warning peer keeps its own interval",
			interval: 3600,
			risk:     RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 2.0},
			want:     3600,
		},
		{
			name:     "already fast strategy is not slowed",
			interval: 60,
			risk:     RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 8.5},
			want:     60,
		},
		{
			name:     "circuit breaker is not warning mode",
			interval: 3600,
			risk:     RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 12, CircuitBreaker: true},
			want:     3600,
		},
		{
			name:     "over max drawdown before circuit breaker still fast",
			interval: 3600,
			risk:     RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 12},
			want:     fast,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{ID: "s1", IntervalSeconds: tc.interval, Capital: 1000}
			state := &StrategyState{RiskState: tc.risk}
			if got := effectiveStrategyIntervalSeconds(sc, state, 600, 80); got != tc.want {
				t.Errorf("effective interval = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNextStrategyCheckDelay_UsesWarningInterval(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	strategies := []StrategyConfig{
		{ID: "warning", IntervalSeconds: 3600, Capital: 1000},
		{ID: "normal", IntervalSeconds: 3600, Capital: 1000},
	}
	states := map[string]*StrategyState{
		"warning": {
			RiskState: RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 8.5},
		},
		"normal": {
			RiskState: RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 1.0},
		},
	}
	lastRun := map[string]time.Time{
		"warning": now.Add(-30 * time.Second),
		"normal":  now.Add(-30 * time.Second),
	}

	intervals := effectiveStrategyIntervals(strategies, states, 600, 80)
	got := nextStrategyCheckDelay(strategies, intervals, lastRun, now)
	if got != time.Minute {
		t.Errorf("next delay = %s, want 1m", got)
	}
}

func TestNextStrategyCheckDelay_FirstRunReturnsZero(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	strategies := []StrategyConfig{
		{ID: "fresh", IntervalSeconds: 3600, Capital: 1000},
	}
	intervals := effectiveStrategyIntervals(strategies, nil, 600, 80)

	got := nextStrategyCheckDelay(strategies, intervals, map[string]time.Time{}, now)
	if got != 0 {
		t.Errorf("first-run delay = %s, want 0", got)
	}

	sd := schedulerDelay(strategies, intervals, map[string]time.Time{}, 600, now, 60)
	if sd != time.Second {
		t.Errorf("schedulerDelay first-run = %s, want 1s", sd)
	}
}

func TestNextStrategyCheckDelay_NoCandidatesReturnsNegative(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	strategies := []StrategyConfig{
		{ID: "skipped-zero-cap", IntervalSeconds: 3600, CapitalPct: 0.5, Capital: 0},
	}
	intervals := effectiveStrategyIntervals(strategies, nil, 600, 80)

	got := nextStrategyCheckDelay(strategies, intervals, map[string]time.Time{}, now)
	if got != -1 {
		t.Errorf("no-candidates delay = %s, want -1", got)
	}

	sd := schedulerDelay(strategies, intervals, map[string]time.Time{}, 600, now, 120)
	if sd != 120*time.Second {
		t.Errorf("schedulerDelay no-candidates = %s, want 120s fallback", sd)
	}

	sd = schedulerDelay(strategies, intervals, map[string]time.Time{}, 600, now, 0)
	if sd != 600*time.Second {
		t.Errorf("schedulerDelay fallback->global = %s, want 600s", sd)
	}

	sd = schedulerDelay(strategies, intervals, map[string]time.Time{}, 0, now, 0)
	if sd != 60*time.Second {
		t.Errorf("schedulerDelay ultimate fallback = %s, want 60s", sd)
	}
}
