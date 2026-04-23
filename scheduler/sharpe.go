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
// has not set Config.RiskFreeRate (0 is treated as "not set"). 2% ≈ short-term
// treasury yield commonly used in retail strategy reports.
const DefaultAnnualRiskFreeRate = 0.02

// minSharpeDays is the minimum number of distinct trading days required to
// report a Sharpe ratio. A single day gives zero variance; two is the smallest
// sample where stdev is defined.
const minSharpeDays = 2

// sharpeLookbackLimit caps how many closed-position rows are pulled per
// strategy. Sharpe is computed from realized PnL, so a cap here just bounds the
// DB scan — the underlying day-bucketing still reflects the full rolling
// window covered by the returned rows. 2000 covers a year of heavy activity.
const sharpeLookbackLimit = 2000

// RiskFreeRateOrDefault returns cfg.RiskFreeRate, falling back to
// DefaultAnnualRiskFreeRate when unset. Treats zero and negative values as
// unset to keep the config forgiving — operators who truly want a 0% rate can
// set a trivially small positive value if needed.
func RiskFreeRateOrDefault(cfg *Config) float64 {
	if cfg == nil || cfg.RiskFreeRate <= 0 {
		return DefaultAnnualRiskFreeRate
	}
	return cfg.RiskFreeRate
}

// ComputeSharpeRatio returns the annualized Sharpe ratio of a strategy based
// on realized daily returns, computed as:
//
//	sharpe = sqrt(252) * (meanDailyReturn - dailyRiskFreeRate) / stdevDailyReturn
//
// Daily return = sum(realized_pnl for positions closed on day D) / initialCapital.
// The annual risk-free rate is converted to a daily simple rate by dividing by
// TradingDaysPerYear.
//
// Returns 0 when the metric is undefined: < minSharpeDays of realized data,
// initialCapital <= 0, or zero standard deviation (all daily returns equal).
func ComputeSharpeRatio(closed []ClosedPosition, initialCapital, annualRiskFreeRate float64) float64 {
	if initialCapital <= 0 || len(closed) == 0 {
		return 0
	}
	dailyPnL := make(map[string]float64)
	for _, cp := range closed {
		if cp.ClosedAt.IsZero() {
			continue
		}
		day := cp.ClosedAt.UTC().Format("2006-01-02")
		dailyPnL[day] += cp.RealizedPnL
	}
	if len(dailyPnL) < minSharpeDays {
		return 0
	}
	returns := make([]float64, 0, len(dailyPnL))
	for _, pnl := range dailyPnL {
		returns = append(returns, pnl/initialCapital)
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
	// Sample standard deviation (Bessel-corrected).
	variance := sqSum / float64(len(returns)-1)
	stdev := math.Sqrt(variance)
	if stdev == 0 {
		return 0
	}
	dailyRfr := annualRiskFreeRate / float64(TradingDaysPerYear)
	return math.Sqrt(float64(TradingDaysPerYear)) * (mean - dailyRfr) / stdev
}

// ComputeSharpeByStrategy builds a map of strategy ID → Sharpe ratio by
// pulling closed-position history from sdb for every configured strategy.
// Returns nil if sdb is nil (callers treat nil as "Sharpe unavailable").
// Strategies with no realized data are omitted from the map.
func ComputeSharpeByStrategy(sdb *StateDB, cfg *Config, state *AppState) map[string]float64 {
	if sdb == nil || cfg == nil || state == nil {
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
		closed, _, err := sdb.QueryClosedPositions(sc.ID, "", time.Time{}, time.Time{}, sharpeLookbackLimit, 0)
		if err != nil {
			continue
		}
		s := ComputeSharpeRatio(closed, initCap, rfr)
		if s != 0 {
			out[sc.ID] = s
		}
	}
	return out
}

// aggregateSharpe returns the Sharpe ratio of a portfolio of strategies,
// computed by summing per-day realized PnL across all strategies and dividing
// by the total initial capital — i.e. treating the group as one book. This is
// more faithful than averaging per-strategy Sharpes, which would weight small
// strategies equally with large ones.
func aggregateSharpe(sdb *StateDB, strategies []StrategyConfig, state *AppState, annualRiskFreeRate float64) float64 {
	if sdb == nil || len(strategies) == 0 {
		return 0
	}
	var totalCap float64
	dailyPnL := make(map[string]float64)
	for _, sc := range strategies {
		ss := state.Strategies[sc.ID]
		if ss == nil {
			continue
		}
		totalCap += EffectiveInitialCapital(sc, ss)
		closed, _, err := sdb.QueryClosedPositions(sc.ID, "", time.Time{}, time.Time{}, sharpeLookbackLimit, 0)
		if err != nil {
			continue
		}
		for _, cp := range closed {
			if cp.ClosedAt.IsZero() {
				continue
			}
			day := cp.ClosedAt.UTC().Format("2006-01-02")
			dailyPnL[day] += cp.RealizedPnL
		}
	}
	if totalCap <= 0 || len(dailyPnL) < minSharpeDays {
		return 0
	}
	days := make([]string, 0, len(dailyPnL))
	for d := range dailyPnL {
		days = append(days, d)
	}
	sort.Strings(days)
	returns := make([]float64, len(days))
	for i, d := range days {
		returns[i] = dailyPnL[d] / totalCap
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
	variance := sqSum / float64(len(returns)-1)
	stdev := math.Sqrt(variance)
	if stdev == 0 {
		return 0
	}
	dailyRfr := annualRiskFreeRate / float64(TradingDaysPerYear)
	return math.Sqrt(float64(TradingDaysPerYear)) * (mean - dailyRfr) / stdev
}

// fmtSharpe renders a Sharpe ratio for summary tables. "—" for zero (undefined)
// so operators can distinguish "no data yet" from a genuine 0.00 Sharpe.
func fmtSharpe(s float64) string {
	if s == 0 {
		return "—"
	}
	if s > 0 {
		return fmt.Sprintf("+%.2f", s)
	}
	return fmt.Sprintf("%.2f", s)
}
