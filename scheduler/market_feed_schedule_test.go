package main

import (
	"strings"
	"testing"
	"time"
)

func scheduleTestConfig(interval int, strategies ...StrategyConfig) *Config {
	return &Config{IntervalSeconds: interval, MarketFeed: marketFeedWebsocket, Strategies: strategies}
}

func feedPerpsStrategy(id, coin string, intervalSeconds int) StrategyConfig {
	return StrategyConfig{
		ID: id, Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"momentum", coin, "1h", "--mode=paper"}, IntervalSeconds: intervalSeconds,
		Capital: 1000,
	}
}

func ids(strategies []StrategyConfig) []string {
	out := make([]string, 0, len(strategies))
	for _, sc := range strategies {
		out = append(out, sc.ID)
	}
	return out
}

func sameIDs(got []StrategyConfig, want ...string) bool {
	have := ids(got)
	if len(have) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, id := range have {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			return false
		}
	}
	return true
}

func TestDeadlineScheduleContract(t *testing.T) {
	live := feedPerpsStrategy("hl-live", "BTC", 300)
	paper := feedPerpsStrategy("hl-paper", "BTC", 300)
	fast := feedPerpsStrategy("hl-fast", "ETH", 90)
	spot := StrategyConfig{
		ID: "spot-a", Type: "spot", Platform: "binanceus", Script: "shared_scripts/check_strategy.py",
		Args: []string{"sma", "BTC/USDT", "1h"}, IntervalSeconds: 300, Capital: 1000,
	}
	cfg := scheduleTestConfig(300, live, paper, fast, spot)
	intervals := map[string]int{"hl-live": 300, "hl-paper": 300, "hl-fast": 90, "spot-a": 300}

	t.Run("twins with equal cadence share one evaluation identifier", func(t *testing.T) {
		now := time.Unix(1_700_000_123, 0).UTC()
		due, marks, _ := computeDueSet(now, cfg, intervals, map[string]time.Time{}, map[string]feedEvaluationMark{}, true)
		if !sameIDs(due, "hl-live", "hl-paper", "hl-fast", "spot-a") {
			t.Fatalf("first cycle after a restart evaluates everything: %v", ids(due))
		}
		if marks["hl-live"].ID != marks["hl-paper"].ID {
			t.Fatalf("twins must share the identifier: %s vs %s", marks["hl-live"].ID, marks["hl-paper"].ID)
		}
		if marks["hl-live"].ID == marks["hl-fast"].ID {
			t.Fatalf("a different cadence must produce a different identifier")
		}
		if _, ok := marks["spot-a"]; ok {
			t.Fatalf("an out-of-scope strategy must keep last-run semantics, not a deadline mark")
		}
		wantDeadline := time.Unix(1_700_000_100, 0).UTC()
		if !marks["hl-live"].Deadline.Equal(wantDeadline) {
			t.Fatalf("deadline: got %s want %s", marks["hl-live"].Deadline, wantDeadline)
		}
	})

	t.Run("a check that ran long never replays a missed deadline", func(t *testing.T) {
		lastEvaluated := map[string]feedEvaluationMark{}
		start := time.Unix(1_700_000_100, 0).UTC()
		_, marks, _ := computeDueSet(start, cfg, intervals, map[string]time.Time{}, lastEvaluated, true)
		for _, sc := range []StrategyConfig{live, paper, fast} {
			markStrategyEvaluated(sc, true, marks, map[string]time.Time{}, lastEvaluated, start)
		}
		overrun := start.Add(11 * time.Minute)
		due, marks2, _ := computeDueSet(overrun, cfg, intervals, map[string]time.Time{}, lastEvaluated, true)
		if !sameIDs(due, "hl-live", "hl-paper", "hl-fast", "spot-a") {
			t.Fatalf("every in-scope strategy is due again after an overrun: %v", ids(due))
		}
		wantDeadline := evaluationDeadline(overrun, 300)
		if !marks2["hl-live"].Deadline.Equal(wantDeadline) {
			t.Fatalf("the overrun cycle must consume only the newest deadline: got %s want %s",
				marks2["hl-live"].Deadline, wantDeadline)
		}
		for _, sc := range []StrategyConfig{live, paper, fast} {
			markStrategyEvaluated(sc, true, marks2, map[string]time.Time{}, lastEvaluated, overrun)
		}
		stillSame, _, _ := computeDueSet(overrun.Add(time.Second), cfg, intervals, map[string]time.Time{}, lastEvaluated, true)
		if sameIDs(stillSame, "hl-live") || len(ids(stillSame)) > 1 {
			t.Fatalf("the same deadline must not dispatch twice: %v", ids(stillSame))
		}
	})

	t.Run("a drawdown cadence change never dispatches a deadline already consumed", func(t *testing.T) {
		lastEvaluated := map[string]feedEvaluationMark{}
		slowIntervals := map[string]int{"hl-live": 300, "hl-paper": 300, "hl-fast": 90, "spot-a": 300}
		at := time.Unix(1_700_000_100, 0).UTC()
		_, marks, _ := computeDueSet(at, cfg, slowIntervals, map[string]time.Time{}, lastEvaluated, true)
		markStrategyEvaluated(live, true, marks, map[string]time.Time{}, lastEvaluated, at)

		fastIntervals := map[string]int{"hl-live": 90, "hl-paper": 300, "hl-fast": 90, "spot-a": 300}
		within := at.Add(30 * time.Second)
		due, _, _ := computeDueSet(within, cfg, fastIntervals, map[string]time.Time{}, lastEvaluated, true)
		for _, id := range ids(due) {
			if id == "hl-live" {
				t.Fatalf("the 90s grid must not re-dispatch the deadline the 300s grid already consumed")
			}
		}
		later := at.Add(95 * time.Second)
		due2, marks2, _ := computeDueSet(later, cfg, fastIntervals, map[string]time.Time{}, lastEvaluated, true)
		found := false
		for _, id := range ids(due2) {
			if id == "hl-live" {
				found = true
			}
		}
		if !found {
			t.Fatalf("the next 90s deadline must dispatch: %v", ids(due2))
		}
		if marks2["hl-live"].IntervalSeconds != 90 {
			t.Fatalf("the mark must carry the new cadence: %+v", marks2["hl-live"])
		}
	})

	t.Run("rest mode keeps last-run semantics for every strategy", func(t *testing.T) {
		restCfg := scheduleTestConfig(300, live, paper, fast, spot)
		restCfg.MarketFeed = marketFeedREST
		now := time.Unix(1_700_000_123, 0).UTC()
		lastRun := map[string]time.Time{
			"hl-live": now.Add(-10 * time.Second), "hl-paper": now.Add(-10 * time.Second),
			"hl-fast": now.Add(-10 * time.Second), "spot-a": now.Add(-10 * time.Second),
		}
		due, marks, _ := computeDueSet(now, restCfg, intervals, lastRun, map[string]feedEvaluationMark{}, false)
		if len(due) != 0 {
			t.Fatalf("rest mode must respect last-run intervals: %v", ids(due))
		}
		if len(marks) != 0 {
			t.Fatalf("rest mode must produce no deadline marks: %v", marks)
		}
		lastEvaluated := map[string]feedEvaluationMark{}
		markStrategyEvaluated(live, false, marks, lastRun, lastEvaluated, now)
		if !lastRun["hl-live"].Equal(now) {
			t.Fatalf("rest mode must stamp lastRun")
		}
		if len(lastEvaluated) != 0 {
			t.Fatalf("rest mode must never write a deadline mark")
		}
	})

	t.Run("zero-capital strategies are reported and skipped", func(t *testing.T) {
		zero := feedPerpsStrategy("hl-zero", "SOL", 300)
		zero.Capital = 0
		zero.CapitalPct = 10
		zeroCfg := scheduleTestConfig(300, zero)
		due, _, skipped := computeDueSet(time.Unix(1_700_000_123, 0).UTC(), zeroCfg,
			map[string]int{"hl-zero": 300}, map[string]time.Time{}, map[string]feedEvaluationMark{}, true)
		if len(due) != 0 {
			t.Fatalf("a zero-capital strategy must not be due: %v", ids(due))
		}
		if len(skipped) != 1 || skipped[0] != "hl-zero" {
			t.Fatalf("the skip must be reported: %v", skipped)
		}
	})
}

func TestFeedSchedulerDelayNeverBusySpins(t *testing.T) {
	live := feedPerpsStrategy("hl-live", "BTC", 300)
	spot := StrategyConfig{
		ID: "spot-a", Type: "spot", Platform: "binanceus", Script: "shared_scripts/check_strategy.py",
		Args: []string{"sma", "BTC/USDT", "1h"}, IntervalSeconds: 300, Capital: 1000,
	}
	cfg := scheduleTestConfig(300, live, spot)
	intervals := map[string]int{"hl-live": 300, "spot-a": 300}
	now := time.Unix(1_700_000_100, 0).UTC()

	lastEvaluated := map[string]feedEvaluationMark{}
	lastRun := map[string]time.Time{"spot-a": now}
	_, marks, _ := computeDueSet(now, cfg, intervals, lastRun, lastEvaluated, true)
	markStrategyEvaluated(live, true, marks, lastRun, lastEvaluated, now)

	delay := cycleSchedulerDelay(cfg, intervals, lastRun, lastEvaluated, now.Add(time.Second), 60, true)
	if delay < 4*time.Minute {
		t.Fatalf("with both consumers settled the loop must sleep to the next deadline, got %s", delay)
	}
	if got := nextFeedDeadlineDelay(cfg, intervals, lastEvaluated, now.Add(time.Second)); got <= 0 {
		t.Fatalf("the next in-scope deadline must be in the future, got %s", got)
	}
	if got := nextFeedDeadlineDelay(cfg, intervals, map[string]feedEvaluationMark{}, now); got != 0 {
		t.Fatalf("an unconsumed deadline must report zero delay, got %s", got)
	}
	restOnly := scheduleTestConfig(300, spot)
	if got := nextFeedDeadlineDelay(restOnly, intervals, lastEvaluated, now); got != -1 {
		t.Fatalf("with no in-scope strategies the feed must not constrain the delay, got %s", got)
	}
}

func TestEvaluationDeadlineIsEpochAligned(t *testing.T) {
	cases := []struct {
		now      int64
		interval int
		want     int64
	}{
		{now: 1_700_000_123, interval: 300, want: 1_700_000_100},
		{now: 1_700_000_100, interval: 300, want: 1_700_000_100},
		{now: 1_700_000_099, interval: 300, want: 1_699_999_800},
		{now: 1_700_000_123, interval: 90, want: 1_700_000_100},
	}
	for _, tc := range cases {
		got := evaluationDeadline(time.Unix(tc.now, 0).UTC(), tc.interval)
		if got.Unix() != tc.want {
			t.Fatalf("now=%d interval=%d: got %d want %d", tc.now, tc.interval, got.Unix(), tc.want)
		}
		if id := evaluationID(tc.interval, got); id == "" {
			t.Fatalf("evaluation id must not be empty")
		}
	}
}

func TestCycleEvaluationSummaryNamesTwinsAndCadenceSplits(t *testing.T) {
	marks := map[string]feedEvaluationMark{
		"hl-live":  {ID: "300s/1700000100", Deadline: time.Unix(1_700_000_100, 0).UTC(), IntervalSeconds: 300},
		"hl-paper": {ID: "300s/1700000100", Deadline: time.Unix(1_700_000_100, 0).UTC(), IntervalSeconds: 300},
		"hl-fast":  {ID: "90s/1700000100", Deadline: time.Unix(1_700_000_100, 0).UTC(), IntervalSeconds: 90},
	}
	lines := feedCycleEvaluationSummary(marks)
	if len(lines) != 3 {
		t.Fatalf("two cadence groups plus the difference note: %v", lines)
	}
	joined := lines[0] + lines[1] + lines[2]
	for _, want := range []string{"hl-live, hl-paper", "hl-fast", "cadences differ"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary must carry %q: %v", want, lines)
		}
	}
	if id := cycleEvaluationID(marks, 7); id == "" || id == "cycle/7" {
		t.Fatalf("with marks present the snapshot id comes from the evaluation identifiers, got %q", id)
	}
	if id := cycleEvaluationID(map[string]feedEvaluationMark{}, 7); id != "cycle/7" {
		t.Fatalf("with no in-scope strategies the snapshot id falls back to the cycle, got %q", id)
	}
}
