package main

import "time"

const (
	strategyDrawdownFastIntervalSeconds = 90
	defaultDrawdownWarnThresholdPct     = 80
)

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

func strategyDrawdownWarningActive(s *StrategyState, warnThresholdPct float64) bool {
	if s == nil || s.RiskState.CircuitBreaker {
		return false
	}
	r := s.RiskState
	if r.MaxDrawdownPct <= 0 || warnThresholdPct <= 0 {
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

func effectiveStrategyIntervals(strategies []StrategyConfig, states map[string]*StrategyState, globalIntervalSeconds int, warnThresholdPct float64) map[string]int {
	out := make(map[string]int, len(strategies))
	for _, sc := range strategies {
		out[sc.ID] = effectiveStrategyIntervalSeconds(sc, states[sc.ID], globalIntervalSeconds, warnThresholdPct)
	}
	return out
}

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
