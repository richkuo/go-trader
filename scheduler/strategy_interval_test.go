package main

import (
	"testing"
	"time"
)

func TestEffectiveStrategyIntervalSeconds_DrawdownWarningUsesFastInterval(t *testing.T) {
	sc := StrategyConfig{ID: "s1", IntervalSeconds: 3600}
	state := &StrategyState{
		RiskState: RiskState{
			MaxDrawdownPct:     10,
			CurrentDrawdownPct: 8.5,
			TotalTrades:        1,
		},
	}

	got := effectiveStrategyIntervalSeconds(sc, state, 600, 80)
	if got != strategyDrawdownFastIntervalSeconds {
		t.Errorf("effective interval = %d, want %d", got, strategyDrawdownFastIntervalSeconds)
	}
}

func TestEffectiveStrategyIntervalSeconds_DrawdownRecoveryReverts(t *testing.T) {
	sc := StrategyConfig{ID: "s1", IntervalSeconds: 3600}
	state := &StrategyState{
		RiskState: RiskState{
			MaxDrawdownPct:     10,
			CurrentDrawdownPct: 7.5,
			TotalTrades:        1,
		},
	}

	got := effectiveStrategyIntervalSeconds(sc, state, 600, 80)
	if got != 3600 {
		t.Errorf("effective interval = %d, want normal interval 3600", got)
	}
}

func TestEffectiveStrategyIntervalSeconds_OnlyWarningStrategySpeedsUp(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "warning", IntervalSeconds: 3600, Capital: 1000},
		{ID: "normal", IntervalSeconds: 3600, Capital: 1000},
	}
	states := map[string]*StrategyState{
		"warning": {
			RiskState: RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 8.1, TotalTrades: 1},
		},
		"normal": {
			RiskState: RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 2.0, TotalTrades: 1},
		},
	}

	if got := effectiveStrategyIntervalSeconds(strategies[0], states["warning"], 600, 80); got != strategyDrawdownFastIntervalSeconds {
		t.Errorf("warning strategy interval = %d, want %d", got, strategyDrawdownFastIntervalSeconds)
	}
	if got := effectiveStrategyIntervalSeconds(strategies[1], states["normal"], 600, 80); got != 3600 {
		t.Errorf("normal strategy interval = %d, want 3600", got)
	}
}

func TestEffectiveStrategyIntervalSeconds_DoesNotSlowAlreadyFastStrategy(t *testing.T) {
	sc := StrategyConfig{ID: "s1", IntervalSeconds: 60}
	state := &StrategyState{
		RiskState: RiskState{
			MaxDrawdownPct:     10,
			CurrentDrawdownPct: 8.5,
			TotalTrades:        1,
		},
	}

	got := effectiveStrategyIntervalSeconds(sc, state, 600, 80)
	if got != 60 {
		t.Errorf("effective interval = %d, want existing faster interval 60", got)
	}
}

func TestEffectiveStrategyIntervalSeconds_CircuitBreakerIsNotWarningMode(t *testing.T) {
	sc := StrategyConfig{ID: "s1", IntervalSeconds: 3600}
	state := &StrategyState{
		RiskState: RiskState{
			MaxDrawdownPct:     10,
			CurrentDrawdownPct: 12,
			TotalTrades:        1,
			CircuitBreaker:     true,
		},
	}

	got := effectiveStrategyIntervalSeconds(sc, state, 600, 80)
	if got != 3600 {
		t.Errorf("effective interval = %d, want normal interval 3600 while circuit breaker is active", got)
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
			RiskState: RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 8.5, TotalTrades: 1},
		},
		"normal": {
			RiskState: RiskState{MaxDrawdownPct: 10, CurrentDrawdownPct: 1.0, TotalTrades: 1},
		},
	}
	lastRun := map[string]time.Time{
		"warning": now.Add(-30 * time.Second),
		"normal":  now.Add(-30 * time.Second),
	}

	got := nextStrategyCheckDelay(strategies, states, lastRun, 600, 80, now)
	if got != time.Minute {
		t.Errorf("next delay = %s, want 1m", got)
	}
}
