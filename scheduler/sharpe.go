package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// TradingDaysPerYear is the standard trading-days convention used to annualize
// daily Sharpe ratios. Matches most broker/industry reports (252 sessions/year).
const TradingDaysPerYear = 252

// DefaultAnnualRiskFreeRate is the Sharpe risk-free rate used when the operator
// has not set Config.RiskFreeRate (nil pointer = not set). 2% ≈ short-term
// treasury yield commonly used in retail strategy reports.
const DefaultAnnualRiskFreeRate = 0.02

// minSharpeDays is the minimum number of distinct trading days required to
// report a Sharpe ratio. Set to 20 so the displayed number reflects a
// meaningful sample — with fewer days the sample variance is dominated by
// noise and the annualized figure swings wildly (#397 review). Operators see
// "N/A" instead of a misleading early value.
const minSharpeDays = 20

// sharpeLookbackLimit caps how many closed-position rows are pulled per
// strategy. Sharpe is computed from realized PnL, so a cap here just bounds the
// DB scan — the underlying day-bucketing still reflects the full rolling
// window covered by the returned rows. StateDB.QueryClosedPositions clamps the
// per-call limit to 500 internally, so this is a documented ceiling rather
// than a true maximum.
const sharpeLookbackLimit = 500

// RiskFreeRateOrDefault returns *cfg.RiskFreeRate, falling back to
// DefaultAnnualRiskFreeRate when unset (nil pointer). A genuinely 0% rate is
// expressible as an explicit 0 in the config — the pointer distinguishes
// "missing" from "zero" so operators running backtest comparisons against a
// 0% benchmark are not silently overridden.
func RiskFreeRateOrDefault(cfg *Config) float64 {
	if cfg == nil || cfg.RiskFreeRate == nil {
		return DefaultAnnualRiskFreeRate
	}
	if *cfg.RiskFreeRate < 0 {
		return DefaultAnnualRiskFreeRate
	}
	return *cfg.RiskFreeRate
}

// dailyReturnsContinuous buckets realized PnL by UTC day and fills zeros for
// every day between the earliest and latest close. The Sharpe annualization
// (√252) assumes the sample represents a continuously-running book's daily
// return distribution; skipping flat days would inflate the mean and deflate
// the stdev, overstating the metric. Returns (returns, numDistinctDays) —
// caller uses the day count to gate minSharpeDays.
//
// Day bucketing is UTC. Operators trading NY-session equity futures may expect
// exchange-close buckets, but a consistent zone across platforms is more
// important than per-platform accuracy for a high-level summary metric.
func dailyReturnsContinuous(closed []ClosedPosition, initialCapital float64) ([]float64, int) {
	if initialCapital <= 0 || len(closed) == 0 {
		return nil, 0
	}
	dailyPnL := make(map[string]float64)
	var minDay, maxDay time.Time
	first := true
	for _, cp := range closed {
		if cp.ClosedAt.IsZero() {
			continue
		}
		d := cp.ClosedAt.UTC().Truncate(24 * time.Hour)
		day := d.Format("2006-01-02")
		dailyPnL[day] += cp.RealizedPnL
		if first || d.Before(minDay) {
			minDay = d
		}
		if first || d.After(maxDay) {
			maxDay = d
		}
		first = false
	}
	if first {
		return nil, 0
	}
	distinct := len(dailyPnL)
	var returns []float64
	for d := minDay; !d.After(maxDay); d = d.Add(24 * time.Hour) {
		key := d.Format("2006-01-02")
		returns = append(returns, dailyPnL[key]/initialCapital)
	}
	return returns, distinct
}

// annualizedSharpeFromDaily computes sqrt(252)*(mean-dailyRfr)/stdev for a
// slice of daily return values. Returns 0 when the metric is undefined (too
// few samples or zero stdev). Single source of truth for the Sharpe math;
// shared by per-strategy and aggregate code paths so formula changes stay in
// one place (#397 review item 6).
func annualizedSharpeFromDaily(returns []float64, annualRiskFreeRate float64) float64 {
	if len(returns) < 2 {
		return 0
	}
	var sum float64
	for _, r := range returns {
		sum += r
	}
	mean := sum / float64(len(returns))
	var sqSum float64
	for _, r := range returns {
		d := r - mean
		sqSum += d * d
	}
	// Sample standard deviation (Bessel-corrected). The epsilon floor treats
	// near-zero stdev as "effectively no variance" — without it, summing 20+
	// identical daily returns accumulates tiny IEEE-754 drift (e.g. 0.01 is
	// not exactly representable), which would otherwise divide a near-zero
	// mean-excess by a near-zero stdev and fabricate a Sharpe in the 1e16 range.
	variance := sqSum / float64(len(returns)-1)
	stdev := math.Sqrt(variance)
	if stdev < 1e-12 {
		return 0
	}
	dailyRfr := annualRiskFreeRate / float64(TradingDaysPerYear)
	return math.Sqrt(float64(TradingDaysPerYear)) * (mean - dailyRfr) / stdev
}

// ComputeSharpeRatio returns the annualized Sharpe ratio of a strategy based
// on realized daily returns:
//
//	sharpe = sqrt(252) * (meanDailyReturn - dailyRiskFreeRate) / stdevDailyReturn
//
// Daily return = sum(realized_pnl for positions closed on day D) / initialCapital.
// Days with no closures between the first and last close are filled with zero
// so the sample reflects a continuously-running book.
//
// Returns 0 when the metric is undefined: < minSharpeDays of distinct-close
// data, initialCapital <= 0, or zero standard deviation.
func ComputeSharpeRatio(closed []ClosedPosition, initialCapital, annualRiskFreeRate float64) float64 {
	returns, distinct := dailyReturnsContinuous(closed, initialCapital)
	if distinct < minSharpeDays {
		return 0
	}
	return annualizedSharpeFromDaily(returns, annualRiskFreeRate)
}

// LoadClosedPositionsByStrategy pulls closed-position history for every
// configured strategy once, so per-cycle consumers (per-strategy Sharpe,
// per-channel book Sharpe, per-asset book Sharpe) don't each re-query the DB.
// Returns nil when sdb is nil; strategies with empty history map to an empty
// slice so callers can distinguish "queried, no data" from "not queried".
func LoadClosedPositionsByStrategy(sdb *StateDB, cfg *Config) map[string][]ClosedPosition {
	if sdb == nil || cfg == nil {
		return nil
	}
	out := make(map[string][]ClosedPosition, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		closed, _, err := sdb.QueryClosedPositions(sc.ID, "", time.Time{}, time.Time{}, sharpeLookbackLimit, 0)
		if err != nil {
			continue
		}
		out[sc.ID] = closed
	}
	return out
}

// ComputeSharpeByStrategy builds a map of strategy ID → Sharpe ratio from a
// pre-loaded closed-positions map. Callers that still need the DB-query
// variant can call LoadClosedPositionsByStrategy first. Strategies with no
// realized data or undefined Sharpe are omitted.
func ComputeSharpeByStrategy(closedByStrategy map[string][]ClosedPosition, cfg *Config, state *AppState) map[string]float64 {
	if closedByStrategy == nil || cfg == nil || state == nil {
		return nil
	}
	rfr := RiskFreeRateOrDefault(cfg)
	out := make(map[string]float64)
	for _, sc := range cfg.Strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		initCap := EffectiveInitialCapital(sc, ss)
		if initCap <= 0 {
			continue
		}
		s := ComputeSharpeRatio(closedByStrategy[sc.ID], initCap, rfr)
		if s != 0 {
			out[sc.ID] = s
		}
	}
	return out
}

// aggregateSharpe returns the Sharpe ratio of a portfolio of strategies —
// i.e. "book Sharpe" — computed by summing per-day realized PnL across all
// strategies and dividing by the total initial capital. This is more faithful
// than averaging per-strategy Sharpes, which would weight small strategies
// equally with large ones. Uses the pre-loaded closed-positions map so the
// per-cycle caller queries the DB exactly once per strategy.
func aggregateSharpe(closedByStrategy map[string][]ClosedPosition, strategies []StrategyConfig, state *AppState, annualRiskFreeRate float64) float64 {
	if len(strategies) == 0 || state == nil {
		return 0
	}
	var totalCap float64
	dailyPnL := make(map[string]float64)
	var minDay, maxDay time.Time
	first := true
	for _, sc := range strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		totalCap += EffectiveInitialCapital(sc, ss)
		for _, cp := range closedByStrategy[sc.ID] {
			if cp.ClosedAt.IsZero() {
				continue
			}
			d := cp.ClosedAt.UTC().Truncate(24 * time.Hour)
			day := d.Format("2006-01-02")
			dailyPnL[day] += cp.RealizedPnL
			if first || d.Before(minDay) {
				minDay = d
			}
			if first || d.After(maxDay) {
				maxDay = d
			}
			first = false
		}
	}
	if totalCap <= 0 || len(dailyPnL) < minSharpeDays {
		return 0
	}
	// Fill zero-return days between first and last close so the distribution
	// reflects a continuously-running book (see dailyReturnsContinuous).
	days := make([]string, 0, len(dailyPnL))
	for d := minDay; !d.After(maxDay); d = d.Add(24 * time.Hour) {
		days = append(days, d.Format("2006-01-02"))
	}
	sort.Strings(days)
	returns := make([]float64, len(days))
	for i, d := range days {
		returns[i] = dailyPnL[d] / totalCap
	}
	return annualizedSharpeFromDaily(returns, annualRiskFreeRate)
}

// fmtSharpe renders a Sharpe ratio for summary tables. "N/A" for zero
// (undefined) so operators can distinguish "no data yet" from a genuine 0.00
// Sharpe. Returns plain ASCII to keep fixed-width byte padding (%Ns) aligned
// in Discord monospace code blocks — a UTF-8 em-dash is 3 bytes but 1 rune,
// which misaligns the column by 2 spaces (#397 review item 2).
func fmtSharpe(s float64) string {
	if s == 0 {
		return "N/A"
	}
	if s > 0 {
		return fmt.Sprintf("+%.2f", s)
	}
	return fmt.Sprintf("%.2f", s)
}
