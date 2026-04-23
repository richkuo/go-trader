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

// Drawdown that has already exceeded MaxDrawdownPct (but circuit breaker
// hasn't flipped yet — CB only sets inside CheckRisk during a real cycle)
// must still use the fast cadence so the next cycle can trip CB ASAP.
// Regression test for #417 review: the previous implementation had an upper
// bound `<= MaxDrawdownPct` that incorrectly fell back to the slow interval
// in this most-urgent case.
func TestEffectiveStrategyIntervalSeconds_OverMaxDrawdownStillFast(t *testing.T) {
	sc := StrategyConfig{ID: "s1", IntervalSeconds: 3600}
	state := &StrategyState{
		RiskState: RiskState{
			MaxDrawdownPct:     10,
			CurrentDrawdownPct: 12, // already over max; CB not yet set
			TotalTrades:        1,
		},
	}

	got := effectiveStrategyIntervalSeconds(sc, state, 600, 80)
	if got != strategyDrawdownFastIntervalSeconds {
		t.Errorf("effective interval = %d, want fast %d when drawdown exceeds max but CB not yet set", got, strategyDrawdownFastIntervalSeconds)
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

	intervals := effectiveStrategyIntervals(strategies, states, 600, 80)
	got := nextStrategyCheckDelay(strategies, intervals, lastRun, now)
	if got != time.Minute {
		t.Errorf("next delay = %s, want 1m", got)
	}
}

// First-run path: a strategy with no lastRun entry must short-circuit to 0
// so schedulerDelay yields immediately (1s) and the dueStrategies loop picks
// it up next cycle.
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

	// schedulerDelay should turn that into a 1s yield.
	sd := schedulerDelay(strategies, intervals, map[string]time.Time{}, 600, now, 60)
	if sd != time.Second {
		t.Errorf("schedulerDelay first-run = %s, want 1s", sd)
	}
}

// No-candidate path: every strategy is skipped (zero capital with capital_pct
// set, or non-positive interval). nextStrategyCheckDelay must return -1 so
// schedulerDelay falls back to the configured fallback interval rather than
// busy-looping.
func TestNextStrategyCheckDelay_NoCandidatesReturnsNegative(t *testing.T) {
	now := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	strategies := []StrategyConfig{
		// shouldSkipZeroCapital: capital_pct set (0-1) + capital == 0
		{ID: "skipped-zero-cap", IntervalSeconds: 3600, CapitalPct: 0.5, Capital: 0},
	}
	intervals := effectiveStrategyIntervals(strategies, nil, 600, 80)

	got := nextStrategyCheckDelay(strategies, intervals, map[string]time.Time{}, now)
	if got != -1 {
		t.Errorf("no-candidates delay = %s, want -1", got)
	}

	// schedulerDelay should fall back to the configured fallbackSeconds.
	sd := schedulerDelay(strategies, intervals, map[string]time.Time{}, 600, now, 120)
	if sd != 120*time.Second {
		t.Errorf("schedulerDelay no-candidates = %s, want 120s fallback", sd)
	}

	// fallbackSeconds <= 0 → use globalIntervalSeconds.
	sd = schedulerDelay(strategies, intervals, map[string]time.Time{}, 600, now, 0)
	if sd != 600*time.Second {
		t.Errorf("schedulerDelay fallback->global = %s, want 600s", sd)
	}

	// Both <= 0 → 60s ultimate fallback.
	sd = schedulerDelay(strategies, intervals, map[string]time.Time{}, 0, now, 0)
	if sd != 60*time.Second {
		t.Errorf("schedulerDelay ultimate fallback = %s, want 60s", sd)
	}
}
