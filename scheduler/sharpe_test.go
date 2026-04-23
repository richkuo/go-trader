package main

import (
	"math"
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

func TestComputeSharpeRatioInsufficientData(t *testing.T) {
	if got := ComputeSharpeRatio(nil, 1000, 0.02); got != 0 {
		t.Fatalf("empty input should yield 0, got %v", got)
	}
	// Single day = only one sample, variance undefined.
	one := []ClosedPosition{{ClosedAt: day("2026-01-01"), RealizedPnL: 10}}
	if got := ComputeSharpeRatio(one, 1000, 0.02); got != 0 {
		t.Fatalf("single-day input should yield 0, got %v", got)
	}
	// Rows with a zero ClosedAt are skipped.
	skipped := []ClosedPosition{
		{ClosedAt: time.Time{}, RealizedPnL: 10},
		{ClosedAt: time.Time{}, RealizedPnL: -5},
	}
	if got := ComputeSharpeRatio(skipped, 1000, 0.02); got != 0 {
		t.Fatalf("zero-timestamp rows should be skipped, got %v", got)
	}
}

func TestComputeSharpeRatioZeroStdev(t *testing.T) {
	// Two days, identical daily PnL → stdev 0 → Sharpe undefined (0).
	same := []ClosedPosition{
		{ClosedAt: day("2026-01-01"), RealizedPnL: 10},
		{ClosedAt: day("2026-01-02"), RealizedPnL: 10},
	}
	if got := ComputeSharpeRatio(same, 1000, 0); got != 0 {
		t.Fatalf("zero-stdev should yield 0, got %v", got)
	}
}

func TestComputeSharpeRatioInvalidCapital(t *testing.T) {
	closed := []ClosedPosition{
		{ClosedAt: day("2026-01-01"), RealizedPnL: 10},
		{ClosedAt: day("2026-01-02"), RealizedPnL: -5},
	}
	if got := ComputeSharpeRatio(closed, 0, 0.02); got != 0 {
		t.Fatalf("zero capital should yield 0, got %v", got)
	}
	if got := ComputeSharpeRatio(closed, -100, 0.02); got != 0 {
		t.Fatalf("negative capital should yield 0, got %v", got)
	}
}

func TestComputeSharpeRatioKnownValue(t *testing.T) {
	// Three daily returns of 1%, -0.5%, 0.75% on $10k initial capital with a 0%
	// risk-free rate. Returns: 0.01, -0.005, 0.0075 → mean 0.004167,
	// sample stdev 0.00763763. Annualized Sharpe = sqrt(252) * mean / stdev.
	closed := []ClosedPosition{
		{ClosedAt: day("2026-01-01"), RealizedPnL: 100},
		{ClosedAt: day("2026-01-02"), RealizedPnL: -50},
		{ClosedAt: day("2026-01-03"), RealizedPnL: 75},
	}
	got := ComputeSharpeRatio(closed, 10000, 0)
	// Daily returns: 0.01, -0.005, 0.0075
	// mean = 0.0041666667
	// sample variance = ((0.01-m)^2 + (-0.005-m)^2 + (0.0075-m)^2) / 2 = 6.4583e-5
	// stdev = sqrt(6.4583e-5) ≈ 0.008036
	// Sharpe = sqrt(252) * mean / stdev ≈ 8.2305
	want := math.Sqrt(252) * 0.0041666667 / math.Sqrt(6.4583e-5)
	if math.Abs(got-want) > 1e-2 {
		t.Fatalf("Sharpe = %v, want %v", got, want)
	}
	if got < 7.5 || got > 9.0 {
		t.Fatalf("Sharpe = %v outside sanity range", got)
	}
}

func TestComputeSharpeRatioBucketsByDay(t *testing.T) {
	// Two rows on the same UTC day should collapse into one daily return.
	// With only one distinct day of data we should get 0 (insufficient data).
	sameDay := []ClosedPosition{
		{ClosedAt: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC), RealizedPnL: 30},
		{ClosedAt: time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC), RealizedPnL: 20},
	}
	if got := ComputeSharpeRatio(sameDay, 1000, 0); got != 0 {
		t.Fatalf("multiple rows same day should produce one bucket (insufficient), got %v", got)
	}

	// Positive drift with low noise → positive Sharpe.
	var closed []ClosedPosition
	for i := 0; i < 10; i++ {
		closed = append(closed, ClosedPosition{
			ClosedAt:    day("2026-01-01").Add(time.Duration(i) * 24 * time.Hour),
			RealizedPnL: 10 + float64(i%2), // alternating 10, 11
		})
	}
	got := ComputeSharpeRatio(closed, 1000, 0)
	if got <= 0 {
		t.Fatalf("positive drift should yield positive Sharpe, got %v", got)
	}
}

func TestComputeSharpeRatioRiskFreeLowersSharpe(t *testing.T) {
	closed := []ClosedPosition{
		{ClosedAt: day("2026-01-01"), RealizedPnL: 100},
		{ClosedAt: day("2026-01-02"), RealizedPnL: -50},
		{ClosedAt: day("2026-01-03"), RealizedPnL: 75},
	}
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
		t.Fatalf("zero field should yield default, got %v", got)
	}
	if got := RiskFreeRateOrDefault(&Config{RiskFreeRate: 0.05}); got != 0.05 {
		t.Fatalf("custom rate should pass through, got %v", got)
	}
	if got := RiskFreeRateOrDefault(&Config{RiskFreeRate: -0.01}); got != DefaultAnnualRiskFreeRate {
		t.Fatalf("negative rate should fall back to default, got %v", got)
	}
}

func TestFmtSharpe(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "—"},
		{1.23, "+1.23"},
		{-0.75, "-0.75"},
		{2.0, "+2.00"},
	}
	for _, c := range cases {
		if got := fmtSharpe(c.in); got != c.want {
			t.Fatalf("fmtSharpe(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestComputeSharpeByStrategyNilDB(t *testing.T) {
	if got := ComputeSharpeByStrategy(nil, &Config{}, &AppState{}); got != nil {
		t.Fatalf("nil sdb should yield nil map, got %v", got)
	}
}

func TestAggregateSharpeEmpty(t *testing.T) {
	if got := aggregateSharpe(nil, nil, nil, 0.02); got != 0 {
		t.Fatalf("nil inputs should yield 0, got %v", got)
	}
}
