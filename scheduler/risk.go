package main

import (
	"fmt"
	"time"
)

// RiskState tracks risk metrics for a strategy.
type RiskState struct {
	PeakValue         float64   `json:"peak_value"`
	MaxDrawdownPct    float64   `json:"max_drawdown_pct"`
	CurrentDrawdownPct float64  `json:"current_drawdown_pct"`
	DailyPnL          float64   `json:"daily_pnl"`
	DailyPnLDate       string   `json:"daily_pnl_date"`
	ConsecutiveLosses  int      `json:"consecutive_losses"`
	CircuitBreaker     bool     `json:"circuit_breaker"`
	CircuitBreakerUntil time.Time `json:"circuit_breaker_until"`
	TotalTrades        int      `json:"total_trades"`
	WinningTrades      int      `json:"winning_trades"`
	LosingTrades       int      `json:"losing_trades"`
}

// rolloverDailyPnL resets DailyPnL to zero whenever the UTC date has advanced
// past DailyPnLDate. Calling this at both risk-check time and trade-record time
// ensures the reset is applied regardless of which code path runs first after
// midnight — fixing issue #27 where a skipped or late risk check could cause
// trades to be counted against the wrong day.
func rolloverDailyPnL(r *RiskState) {
	today := time.Now().UTC().Format("2006-01-02")
	if r.DailyPnLDate != today {
		r.DailyPnL = 0
		r.DailyPnLDate = today
	}
}

// CheckRisk evaluates risk state and returns whether trading is allowed.
func CheckRisk(s *StrategyState, portfolioValue float64) (bool, string) {
	r := &s.RiskState
	now := time.Now().UTC()

	rolloverDailyPnL(r)

	// Check circuit breaker
	if r.CircuitBreaker {
		if now.Before(r.CircuitBreakerUntil) {
			return false, "circuit breaker active"
		}
		r.CircuitBreaker = false
		r.ConsecutiveLosses = 0
	}

	// Update peak
	if portfolioValue > r.PeakValue {
		r.PeakValue = portfolioValue
	}

	// Check drawdown
	if r.PeakValue > 0 {
		r.CurrentDrawdownPct = ((r.PeakValue - portfolioValue) / r.PeakValue) * 100
		if r.TotalTrades > 0 && r.CurrentDrawdownPct > r.MaxDrawdownPct {
			r.CircuitBreaker = true
			r.CircuitBreakerUntil = now.Add(24 * time.Hour)
			return false, fmt.Sprintf("max drawdown exceeded (%.1f%% > %.1f%%, portfolio=$%.2f peak=$%.2f)",
				r.CurrentDrawdownPct, r.MaxDrawdownPct, portfolioValue, r.PeakValue)
		}
	}

	// Consecutive losses circuit breaker (5 in a row → pause 1h)
	if r.ConsecutiveLosses >= 5 {
		r.CircuitBreaker = true
		r.CircuitBreakerUntil = now.Add(1 * time.Hour)
		return false, "5 consecutive losses"
	}

	return true, ""
}

// RecordTradeResult updates risk state with trade outcome.
func RecordTradeResult(r *RiskState, pnl float64) {
	rolloverDailyPnL(r)
	r.TotalTrades++
	r.DailyPnL += pnl
	if pnl >= 0 {
		r.WinningTrades++
		r.ConsecutiveLosses = 0
	} else {
		r.LosingTrades++
		r.ConsecutiveLosses++
	}
}
