package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const TradingDaysPerYear = 252

const DefaultAnnualRiskFreeRate = 0.02

const minSharpeDays = 20

const sharpeLookbackLimit = 500

func RiskFreeRateOrDefault(cfg *Config) float64 {
	if cfg == nil || cfg.RiskFreeRate == nil {
		return DefaultAnnualRiskFreeRate
	}
	if *cfg.RiskFreeRate < 0 {
		return DefaultAnnualRiskFreeRate
	}
	return *cfg.RiskFreeRate
}

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

	variance := sqSum / float64(len(returns)-1)
	stdev := math.Sqrt(variance)
	if stdev < 1e-12 {
		return 0
	}
	dailyRfr := annualRiskFreeRate / float64(TradingDaysPerYear)
	return math.Sqrt(float64(TradingDaysPerYear)) * (mean - dailyRfr) / stdev
}

func ComputeSharpeRatio(closed []ClosedPosition, initialCapital, annualRiskFreeRate float64) float64 {
	returns, distinct := dailyReturnsContinuous(closed, initialCapital)
	if distinct < minSharpeDays {
		return 0
	}
	return annualizedSharpeFromDaily(returns, annualRiskFreeRate)
}

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

func fmtSharpe(s float64) string {
	if s == 0 {
		return "N/A"
	}
	if s > 0 {
		return fmt.Sprintf("+%.2f", s)
	}
	return fmt.Sprintf("%.2f", s)
}
