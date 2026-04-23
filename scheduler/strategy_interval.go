package main

import "time"

const (
	strategyDrawdownFastIntervalSeconds = 90
	defaultDrawdownWarnThresholdPct     = 80
)

// configuredDrawdownWarnThresholdPct returns the percent-of-max threshold at
// which a strategy enters per-strategy "drawdown warning" mode (and switches to
// the fast 90s check interval).
//
// Note: this currently reuses cfg.PortfolioRisk.WarnThresholdPct, which was
// originally designed for portfolio-level (PeakValue vs total equity) warnings.
// Per-strategy drawdown warning shares the same knob today; if the two ever
// need to diverge, add an optional StrategyConfig.DrawdownWarnThresholdPct
// override and prefer it over the portfolio-level value here.
func configuredDrawdownWarnThresholdPct(cfg *Config) float64 {
	if cfg != nil && cfg.PortfolioRisk != nil && cfg.PortfolioRisk.WarnThresholdPct > 0 {
		return cfg.PortfolioRisk.WarnThresholdPct
	}
	return defaultDrawdownWarnThresholdPct
}

func configuredStrategyIntervalSeconds(sc StrategyConfig, globalIntervalSeconds int) int {
	if sc.IntervalSeconds > 0 {
		return sc.IntervalSeconds
	}
	return globalIntervalSeconds
}

// strategyDrawdownWarningActive reports whether a strategy should switch to
// the fast check cadence because its current drawdown has crossed the warn
// threshold (warnThresholdPct % of MaxDrawdownPct). The check intentionally
// has no upper bound: if drawdown has exceeded MaxDrawdownPct but the circuit
// breaker hasn't flipped yet (CB is set inside CheckRisk during a real cycle),
// we still want the fast cadence so the next cycle can fire CB ASAP. The
// CircuitBreaker guard above handles the post-CB case.
func strategyDrawdownWarningActive(s *StrategyState, warnThresholdPct float64) bool {
	if s == nil || s.RiskState.CircuitBreaker {
		return false
	}
	r := s.RiskState
	if r.TotalTrades <= 0 || r.MaxDrawdownPct <= 0 || warnThresholdPct <= 0 {
		return false
	}
	warnDrawdownPct := r.MaxDrawdownPct * warnThresholdPct / 100
	return r.CurrentDrawdownPct > warnDrawdownPct
}

func effectiveStrategyIntervalSeconds(sc StrategyConfig, s *StrategyState, globalIntervalSeconds int, warnThresholdPct float64) int {
	interval := configuredStrategyIntervalSeconds(sc, globalIntervalSeconds)
	if strategyDrawdownWarningActive(s, warnThresholdPct) && (interval <= 0 || interval > strategyDrawdownFastIntervalSeconds) {
		return strategyDrawdownFastIntervalSeconds
	}
	return interval
}

// effectiveStrategyIntervals computes the effective per-strategy check
// interval for every strategy in one pass. Callers that need the same
// intervals for both due-detection and the scheduler delay calculation should
// compute this map once per cycle (under mu.RLock) and pass it to both
// strategyIsDue and nextStrategyCheckDelay, instead of recomputing per call.
func effectiveStrategyIntervals(strategies []StrategyConfig, states map[string]*StrategyState, globalIntervalSeconds int, warnThresholdPct float64) map[string]int {
	out := make(map[string]int, len(strategies))
	for _, sc := range strategies {
		out[sc.ID] = effectiveStrategyIntervalSeconds(sc, states[sc.ID], globalIntervalSeconds, warnThresholdPct)
	}
	return out
}

// nextStrategyCheckDelay returns the time until the next strategy is due, or
// 0 if any strategy is due now (including a first-run strategy with no
// lastRun entry), or -1 if no strategy is a delay candidate at all (all
// skipped by zero-capital or non-positive interval). The -1 sentinel lets
// schedulerDelay distinguish "fire ASAP" from "nothing scheduled — fall back."
//
// `intervals` must be the precomputed map from effectiveStrategyIntervals.
func nextStrategyCheckDelay(strategies []StrategyConfig, intervals map[string]int, lastRun map[string]time.Time, now time.Time) time.Duration {
	var minDelay time.Duration
	hasCandidate := false
	for _, sc := range strategies {
		if shouldSkipZeroCapital(sc) {
			continue
		}
		interval := intervals[sc.ID]
		if interval <= 0 {
			continue
		}
		last, ok := lastRun[sc.ID]
		if !ok {
			return 0
		}
		delay := last.Add(time.Duration(interval) * time.Second).Sub(now)
		if delay <= 0 {
			return 0
		}
		if !hasCandidate || delay < minDelay {
			minDelay = delay
			hasCandidate = true
		}
	}
	if !hasCandidate {
		return -1
	}
	return minDelay
}

// schedulerDelay turns the next-due time into a sleep duration: a positive
// next-due wins; "due now" sleeps 1s to yield; "no candidates" falls back to
// fallbackSeconds (then globalIntervalSeconds, then 60s).
func schedulerDelay(strategies []StrategyConfig, intervals map[string]int, lastRun map[string]time.Time, globalIntervalSeconds int, now time.Time, fallbackSeconds int) time.Duration {
	delay := nextStrategyCheckDelay(strategies, intervals, lastRun, now)
	if delay > 0 {
		return delay
	}
	if delay == 0 {
		return time.Second
	}
	if fallbackSeconds <= 0 {
		fallbackSeconds = globalIntervalSeconds
	}
	if fallbackSeconds <= 0 {
		fallbackSeconds = 60
	}
	return time.Duration(fallbackSeconds) * time.Second
}
