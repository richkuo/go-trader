package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func manyDays(start time.Time, n int, pnl func(i int) float64) []ClosedPosition {
	out := make([]ClosedPosition, n)
	for i := 0; i < n; i++ {
		out[i] = ClosedPosition{
			ClosedAt:    start.Add(time.Duration(i) * 24 * time.Hour),
			RealizedPnL: pnl(i),
		}
	}
	return out
}

func TestComputeSharpeRatioZeroCases(t *testing.T) {
	alt := func(i int) float64 {
		if i%2 == 0 {
			return 10
		}
		return -5
	}
	cases := []struct {
		name    string
		closed  []ClosedPosition
		capital float64
		rfr     float64
	}{
		{"empty input", nil, 1000, 0.02},
		{"single day", []ClosedPosition{{ClosedAt: day("2026-01-01"), RealizedPnL: 10}}, 1000, 0.02},
		{"zero-timestamp rows skipped", []ClosedPosition{
			{ClosedAt: time.Time{}, RealizedPnL: 10},
			{ClosedAt: time.Time{}, RealizedPnL: -5},
		}, 1000, 0.02},
		{"fewer than minSharpeDays distinct days", manyDays(day("2026-01-01"), minSharpeDays-1, alt), 1000, 0},
		{"zero stdev", manyDays(day("2026-01-01"), minSharpeDays, func(int) float64 { return 10 }), 1000, 0},
		{"zero capital", manyDays(day("2026-01-01"), minSharpeDays, alt), 0, 0.02},
		{"negative capital", manyDays(day("2026-01-01"), minSharpeDays, alt), -100, 0.02},
		{"sparse series gated by minSharpeDays", []ClosedPosition{
			{ClosedAt: day("2026-01-01"), RealizedPnL: 100},
			{ClosedAt: day("2026-01-31"), RealizedPnL: -50},
		}, 10000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeSharpeRatio(tc.closed, tc.capital, tc.rfr); got != 0 {
				t.Fatalf("ComputeSharpeRatio = %v, want 0", got)
			}
		})
	}
}

func TestComputeSharpeRatioKnownValue(t *testing.T) {
	pattern := []float64{100, -50, 75}
	closed := manyDays(day("2026-01-01"), minSharpeDays, func(i int) float64 {
		return pattern[i%len(pattern)]
	})
	got := ComputeSharpeRatio(closed, 10000, 0)

	returns := make([]float64, minSharpeDays)
	for i := 0; i < minSharpeDays; i++ {
		returns[i] = pattern[i%len(pattern)] / 10000
	}
	var sum float64
	for _, r := range returns {
		sum += r
	}
	mean := sum / float64(minSharpeDays)
	var sqSum float64
	for _, r := range returns {
		d := r - mean
		sqSum += d * d
	}
	variance := sqSum / float64(minSharpeDays-1)
	want := math.Sqrt(252) * mean / math.Sqrt(variance)
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("Sharpe = %v, want %v", got, want)
	}
	if got <= 0 {
		t.Fatalf("positive-drift series should yield positive Sharpe, got %v", got)
	}
}

func TestComputeSharpeRatioBucketsByDay(t *testing.T) {
	sameDay := []ClosedPosition{
		{ClosedAt: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC), RealizedPnL: 30},
		{ClosedAt: time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC), RealizedPnL: 20},
	}
	if got := ComputeSharpeRatio(sameDay, 1000, 0); got != 0 {
		t.Fatalf("multiple rows same day should produce one bucket (insufficient), got %v", got)
	}

	closed := manyDays(day("2026-01-01"), minSharpeDays, func(i int) float64 {
		return 10 + float64(i%2)
	})
	got := ComputeSharpeRatio(closed, 1000, 0)
	if got <= 0 {
		t.Fatalf("positive drift should yield positive Sharpe, got %v", got)
	}
}

func TestComputeSharpeRatioRiskFreeLowersSharpe(t *testing.T) {
	closed := manyDays(day("2026-01-01"), minSharpeDays, func(i int) float64 {
		pattern := []float64{100, -50, 75}
		return pattern[i%3]
	})
	withZero := ComputeSharpeRatio(closed, 10000, 0)
	withRfr := ComputeSharpeRatio(closed, 10000, 0.02)
	if !(withZero > withRfr) {
		t.Fatalf("risk-free rate should lower Sharpe: withZero=%v withRfr=%v", withZero, withRfr)
	}
}

func TestRiskFreeRateOrDefault(t *testing.T) {
	if got := RiskFreeRateOrDefault(nil); got != DefaultAnnualRiskFreeRate {
		t.Fatalf("nil cfg should yield default, got %v", got)
	}
	if got := RiskFreeRateOrDefault(&Config{}); got != DefaultAnnualRiskFreeRate {
		t.Fatalf("nil field should yield default, got %v", got)
	}
	zero := 0.0
	if got := RiskFreeRateOrDefault(&Config{RiskFreeRate: &zero}); got != 0 {
		t.Fatalf("explicit zero should be respected, got %v", got)
	}
	custom := 0.05
	if got := RiskFreeRateOrDefault(&Config{RiskFreeRate: &custom}); got != 0.05 {
		t.Fatalf("custom rate should pass through, got %v", got)
	}
	negative := -0.01
	if got := RiskFreeRateOrDefault(&Config{RiskFreeRate: &negative}); got != DefaultAnnualRiskFreeRate {
		t.Fatalf("negative rate should fall back to default, got %v", got)
	}
}

func TestComputeSharpeByStrategyNilMap(t *testing.T) {
	if got := ComputeSharpeByStrategy(nil, &Config{}, &AppState{}); got != nil {
		t.Fatalf("nil map should yield nil, got %v", got)
	}
}

func TestAggregateSharpeEmpty(t *testing.T) {
	if got := aggregateSharpe(nil, nil, nil, 0.02); got != 0 {
		t.Fatalf("nil inputs should yield 0, got %v", got)
	}
	if got := aggregateSharpe(nil, []StrategyConfig{}, &AppState{}, 0.02); got != 0 {
		t.Fatalf("empty strategies should yield 0, got %v", got)
	}
}

func TestAggregateSharpePositivePath(t *testing.T) {
	base := day("2026-01-01")
	closedByStrategy := map[string][]ClosedPosition{
		"a": manyDays(base, minSharpeDays, func(i int) float64 {
			pattern := []float64{100, -50, 75, 25}
			return pattern[i%len(pattern)]
		}),
		"b": manyDays(base, minSharpeDays, func(i int) float64 {
			pattern := []float64{20, 30, -10, 40}
			return pattern[i%len(pattern)]
		}),
	}
	strategies := []StrategyConfig{
		{ID: "a", Capital: 10000},
		{ID: "b", Capital: 5000},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"a": {InitialCapital: 10000},
		"b": {InitialCapital: 5000},
	}}

	pooled := aggregateSharpe(closedByStrategy, strategies, state, 0)
	if pooled == 0 {
		t.Fatalf("pooled Sharpe should be non-zero for profitable mixed strategies")
	}

	sharpeA := ComputeSharpeRatio(closedByStrategy["a"], 10000, 0)
	sharpeB := ComputeSharpeRatio(closedByStrategy["b"], 5000, 0)
	mean := (sharpeA + sharpeB) / 2
	if math.Abs(pooled-mean) < 1e-9 {
		t.Fatalf("pooled Sharpe (%v) should differ from mean of per-strategy Sharpes (%v)", pooled, mean)
	}
}

func TestAggregateSharpeFillsFlatDays(t *testing.T) {
	closedByStrategy := map[string][]ClosedPosition{
		"a": {
			{ClosedAt: day("2026-01-01"), RealizedPnL: 100},
			{ClosedAt: day("2026-01-31"), RealizedPnL: -50},
		},
	}
	strategies := []StrategyConfig{{ID: "a", Capital: 10000}}
	state := &AppState{Strategies: map[string]*StrategyState{"a": {InitialCapital: 10000}}}
	if got := aggregateSharpe(closedByStrategy, strategies, state, 0); got != 0 {
		t.Fatalf("sparse series (2 distinct closes, 30 calendar days) should yield 0, got %v", got)
	}
}

func TestComputeSharpeByStrategyFromMap(t *testing.T) {
	base := day("2026-01-01")
	closedByStrategy := map[string][]ClosedPosition{
		"a": manyDays(base, minSharpeDays, func(i int) float64 {
			if i%2 == 0 {
				return 50
			}
			return -20
		}),
		"b": nil,
	}
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "a", Capital: 10000},
		{ID: "b", Capital: 5000},
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"a": {InitialCapital: 10000},
		"b": {InitialCapital: 5000},
	}}
	got := ComputeSharpeByStrategy(closedByStrategy, cfg, state)
	if _, ok := got["a"]; !ok {
		t.Fatalf("expected Sharpe entry for 'a', got %v", got)
	}
	if _, ok := got["b"]; ok {
		t.Fatalf("no-history strategy should be omitted, got %v", got)
	}
}

func TestLoadClosedPositionsByStrategyNilDB(t *testing.T) {
	if got := LoadClosedPositionsByStrategy(nil, &Config{}); got != nil {
		t.Fatalf("nil sdb should yield nil, got %v", got)
	}
}

func TestLoadClosedPositionsByStrategy(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer sdb.Close()

	sc := StrategyConfig{ID: "a", Capital: 10000, Type: "spot"}
	cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}
	state := NewAppState()
	ss := NewStrategyState(sc)
	ss.ClosedPositions = []ClosedPosition{
		{StrategyID: "a", Symbol: "BTC/USDT", Quantity: 1, AvgCost: 50000, Side: "long",
			Multiplier: 1, OpenedAt: day("2026-01-01"), ClosedAt: day("2026-01-02"),
			ClosePrice: 51000, RealizedPnL: 100, CloseReason: "test", DurationSeconds: 86400},
	}
	state.Strategies["a"] = ss
	if err := SaveStateWithDB(state, cfg, sdb); err != nil {
		t.Fatalf("SaveStateWithDB: %v", err)
	}

	got := LoadClosedPositionsByStrategy(sdb, cfg)
	if got == nil {
		t.Fatal("LoadClosedPositionsByStrategy returned nil")
	}
	if len(got["a"]) != 1 {
		t.Fatalf("expected 1 closed position for 'a', got %d", len(got["a"]))
	}
	if got["a"][0].RealizedPnL != 100 {
		t.Fatalf("expected realized pnl 100, got %v", got["a"][0].RealizedPnL)
	}
}
