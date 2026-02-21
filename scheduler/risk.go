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

// forceCloseAllPositions liquidates all open positions at current prices.
// Called when any circuit breaker fires.
func forceCloseAllPositions(s *StrategyState, prices map[string]float64, logger *StrategyLogger) {
	now := time.Now().UTC()

	for symbol, pos := range s.Positions {
		price, ok := prices[symbol]
		if !ok {
			price = pos.AvgCost
		}
		var pnl float64
		if pos.Side == "long" {
			proceeds := pos.Quantity * price
			pnl = proceeds - pos.Quantity*pos.AvgCost
			s.Cash += proceeds
		} else {
			pnl = pos.Quantity * (pos.AvgCost - price)
			s.Cash += pos.Quantity*pos.AvgCost - pos.Quantity*price
		}
		if logger != nil {
			logger.Warn("Circuit breaker: force-closing %s %s @ $%.2f (PnL: $%.2f)", pos.Side, symbol, price, pnl)
		}
		trade := Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     symbol,
			Side:       "close",
			Quantity:   pos.Quantity,
			Price:      price,
			Value:      pos.Quantity * price,
			TradeType:  "spot",
			Details:    fmt.Sprintf("Circuit breaker force-close, PnL: $%.2f", pnl),
		}
		s.TradeHistory = append(s.TradeHistory, trade)
		RecordTradeResult(&s.RiskState, pnl)
		delete(s.Positions, symbol)
	}

	for id, pos := range s.OptionPositions {
		var pnl, closePrice float64
		if pos.Action == "buy" {
			pnl = pos.CurrentValueUSD - pos.EntryPremiumUSD
			s.Cash += pos.CurrentValueUSD
			closePrice = pos.CurrentValueUSD
		} else {
			buybackCost := -pos.CurrentValueUSD
			pnl = pos.EntryPremiumUSD - buybackCost
			s.Cash -= buybackCost
			closePrice = buybackCost
		}
		if logger != nil {
			logger.Warn("Circuit breaker: force-closing %s %s @ $%.2f (PnL: $%.2f)", pos.Action, id, closePrice, pnl)
		}
		trade := Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     id,
			Side:       "close",
			Quantity:   pos.Quantity,
			Price:      closePrice,
			Value:      closePrice,
			TradeType:  "options",
			Details:    fmt.Sprintf("Circuit breaker force-close, PnL: $%.2f", pnl),
		}
		s.TradeHistory = append(s.TradeHistory, trade)
		RecordTradeResult(&s.RiskState, pnl)
		delete(s.OptionPositions, id)
	}
}

// CheckRisk evaluates risk state and returns whether trading is allowed.
func CheckRisk(s *StrategyState, portfolioValue float64, prices map[string]float64, logger *StrategyLogger) (bool, string) {
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
			forceCloseAllPositions(s, prices, logger)
			return false, fmt.Sprintf("max drawdown exceeded (%.1f%% > %.1f%%, portfolio=$%.2f peak=$%.2f)",
				r.CurrentDrawdownPct, r.MaxDrawdownPct, portfolioValue, r.PeakValue)
		}
	}

	// Consecutive losses circuit breaker (5 in a row → pause 1h, close positions)
	if r.ConsecutiveLosses >= 5 {
		r.CircuitBreaker = true
		r.CircuitBreakerUntil = now.Add(1 * time.Hour)
		forceCloseAllPositions(s, prices, logger)
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
