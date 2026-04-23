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
	if r.TotalTrades <= 0 || r.MaxDrawdownPct <= 0 || warnThresholdPct <= 0 {
		return false
	}
	warnDrawdownPct := r.MaxDrawdownPct * warnThresholdPct / 100
	return r.CurrentDrawdownPct > warnDrawdownPct && r.CurrentDrawdownPct <= r.MaxDrawdownPct
}

func effectiveStrategyIntervalSeconds(sc StrategyConfig, s *StrategyState, globalIntervalSeconds int, warnThresholdPct float64) int {
	interval := configuredStrategyIntervalSeconds(sc, globalIntervalSeconds)
	if strategyDrawdownWarningActive(s, warnThresholdPct) && (interval <= 0 || interval > strategyDrawdownFastIntervalSeconds) {
		return strategyDrawdownFastIntervalSeconds
	}
	return interval
}

func nextStrategyCheckDelay(strategies []StrategyConfig, states map[string]*StrategyState, lastRun map[string]time.Time, globalIntervalSeconds int, warnThresholdPct float64, now time.Time) time.Duration {
	var minDelay time.Duration
	hasCandidate := false
	for _, sc := range strategies {
		if shouldSkipZeroCapital(sc) {
			continue
		}
		interval := effectiveStrategyIntervalSeconds(sc, states[sc.ID], globalIntervalSeconds, warnThresholdPct)
		if interval <= 0 {
			continue
		}
		hasCandidate = true
		last, ok := lastRun[sc.ID]
		if !ok {
			return 0
		}
		delay := last.Add(time.Duration(interval) * time.Second).Sub(now)
		if delay <= 0 {
			return 0
		}
		if minDelay == 0 || delay < minDelay {
			minDelay = delay
		}
	}
	if !hasCandidate {
		return -1
	}
	return minDelay
}

func schedulerDelay(strategies []StrategyConfig, states map[string]*StrategyState, lastRun map[string]time.Time, globalIntervalSeconds int, warnThresholdPct float64, now time.Time, fallbackSeconds int) time.Duration {
	delay := nextStrategyCheckDelay(strategies, states, lastRun, globalIntervalSeconds, warnThresholdPct, now)
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
