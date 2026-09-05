package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type feedEvaluationMark struct {
	ID              string
	Deadline        time.Time
	IntervalSeconds int
}

func evaluationDeadline(now time.Time, intervalSeconds int) time.Time {
	if intervalSeconds <= 0 {
		return now.UTC().Truncate(time.Second)
	}
	step := int64(intervalSeconds)
	unix := now.UTC().Unix()
	floored := unix - ((unix%step)+step)%step
	return time.Unix(floored, 0).UTC()
}

func evaluationID(intervalSeconds int, deadline time.Time) string {
	return fmt.Sprintf("%ds/%d", intervalSeconds, deadline.Unix())
}

func feedEvaluationInterval(sc StrategyConfig, intervals map[string]int, globalIntervalSeconds int) int {
	interval := intervals[sc.ID]
	if interval <= 0 {
		interval = configuredStrategyIntervalSeconds(sc, globalIntervalSeconds)
	}
	if interval <= 0 {
		interval = 60
	}
	return interval
}

func feedDeadlineDue(sc StrategyConfig, now time.Time, intervals map[string]int, globalIntervalSeconds int,
	lastEvaluated map[string]feedEvaluationMark) (feedEvaluationMark, bool) {
	interval := feedEvaluationInterval(sc, intervals, globalIntervalSeconds)
	deadline := evaluationDeadline(now, interval)
	mark := feedEvaluationMark{
		ID:              evaluationID(interval, deadline),
		Deadline:        deadline,
		IntervalSeconds: interval,
	}
	prev, seen := lastEvaluated[sc.ID]
	if seen && !deadline.After(prev.Deadline) {
		return mark, false
	}
	return mark, true
}

func computeDueSet(now time.Time, cfg *Config, intervals map[string]int, lastRun map[string]time.Time,
	lastEvaluated map[string]feedEvaluationMark, websocketFeed bool) ([]StrategyConfig, map[string]feedEvaluationMark, []string) {
	due := make([]StrategyConfig, 0, len(cfg.Strategies))
	marks := make(map[string]feedEvaluationMark)
	var zeroCapital []string
	for _, sc := range cfg.Strategies {
		if shouldSkipZeroCapital(sc) {
			zeroCapital = append(zeroCapital, sc.ID)
			continue
		}
		if websocketFeed && feedScopedStrategy(sc) {
			mark, isDue := feedDeadlineDue(sc, now, intervals, cfg.IntervalSeconds, lastEvaluated)
			if !isDue {
				continue
			}
			marks[sc.ID] = mark
			due = append(due, sc)
			continue
		}
		interval := intervals[sc.ID]
		last, exists := lastRun[sc.ID]
		if !exists || now.Sub(last) >= time.Duration(interval)*time.Second {
			due = append(due, sc)
		}
	}
	return due, marks, zeroCapital
}

func markStrategyEvaluated(sc StrategyConfig, websocketFeed bool, marks map[string]feedEvaluationMark,
	lastRun map[string]time.Time, lastEvaluated map[string]feedEvaluationMark, now time.Time) {
	if websocketFeed && feedScopedStrategy(sc) {
		if mark, ok := marks[sc.ID]; ok {
			lastEvaluated[sc.ID] = mark
			return
		}
	}
	lastRun[sc.ID] = now
}

func nextFeedDeadlineDelay(cfg *Config, intervals map[string]int, lastEvaluated map[string]feedEvaluationMark,
	now time.Time) time.Duration {
	var minDelay time.Duration
	has := false
	for _, sc := range cfg.Strategies {
		if !feedScopedStrategy(sc) || shouldSkipZeroCapital(sc) {
			continue
		}
		interval := feedEvaluationInterval(sc, intervals, cfg.IntervalSeconds)
		deadline := evaluationDeadline(now, interval)
		prev, seen := lastEvaluated[sc.ID]
		if !seen || deadline.After(prev.Deadline) {
			return 0
		}
		next := deadline.Add(time.Duration(interval) * time.Second)
		delay := next.Sub(now)
		if delay < 0 {
			delay = 0
		}
		if !has || delay < minDelay {
			minDelay = delay
			has = true
		}
	}
	if !has {
		return -1
	}
	return minDelay
}

func feedCycleEvaluationSummary(marks map[string]feedEvaluationMark) []string {
	byID := make(map[string][]string)
	cadence := make(map[string]int)
	for strategyID, mark := range marks {
		byID[mark.ID] = append(byID[mark.ID], strategyID)
		cadence[mark.ID] = mark.IntervalSeconds
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		members := byID[id]
		sort.Strings(members)
		out = append(out, fmt.Sprintf("[feed] eval=%s cadence=%ds twins=%s", id, cadence[id], strings.Join(members, ", ")))
	}
	if len(ids) > 1 {
		out = append(out, fmt.Sprintf("[feed] %d evaluation identifiers this cycle: cadences differ, so only strategies sharing an identifier evaluated the same snapshot", len(ids)))
	}
	return out
}

func nextStrategyCheckDelayScoped(strategies []StrategyConfig, intervals map[string]int, lastRun map[string]time.Time,
	now time.Time, skip func(StrategyConfig) bool) time.Duration {
	var minDelay time.Duration
	hasCandidate := false
	for _, sc := range strategies {
		if shouldSkipZeroCapital(sc) {
			continue
		}
		if skip != nil && skip(sc) {
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

func schedulerDelayScoped(strategies []StrategyConfig, intervals map[string]int, lastRun map[string]time.Time,
	globalIntervalSeconds int, now time.Time, fallbackSeconds int, skip func(StrategyConfig) bool) time.Duration {
	delay := nextStrategyCheckDelayScoped(strategies, intervals, lastRun, now, skip)
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

func cycleSchedulerDelay(cfg *Config, intervals map[string]int, lastRun map[string]time.Time,
	lastEvaluated map[string]feedEvaluationMark, now time.Time, fallbackSeconds int, websocketFeed bool) time.Duration {
	if !websocketFeed {
		return schedulerDelay(cfg.Strategies, intervals, lastRun, cfg.IntervalSeconds, now, fallbackSeconds)
	}
	skip := func(sc StrategyConfig) bool { return feedScopedStrategy(sc) }
	delay := schedulerDelayScoped(cfg.Strategies, intervals, lastRun, cfg.IntervalSeconds, now, fallbackSeconds, skip)
	feedDelay := nextFeedDeadlineDelay(cfg, intervals, lastEvaluated, now)
	if feedDelay < 0 {
		return delay
	}
	if feedDelay == 0 {
		return time.Second
	}
	if feedDelay < delay {
		return feedDelay
	}
	return delay
}
