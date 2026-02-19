package main

import (
	"fmt"
	"time"
)

// Position represents a spot position.
type Position struct {
	Symbol   string  `json:"symbol"`
	Quantity float64 `json:"quantity"`
	AvgCost  float64 `json:"avg_cost"`
	Side     string  `json:"side"` // "long" or "short"
}

// Trade represents a completed trade.
type Trade struct {
	Timestamp  time.Time `json:"timestamp"`
	StrategyID string    `json:"strategy_id"`
	Symbol     string    `json:"symbol"`
	Side       string    `json:"side"` // "buy" or "sell"
	Quantity   float64   `json:"quantity"`
	Price      float64   `json:"price"`
	Value      float64   `json:"value"`
	TradeType  string    `json:"trade_type"` // "spot" or "options"
	Details    string    `json:"details"`
}

// PortfolioValue calculates total value of a strategy's portfolio.
func PortfolioValue(s *StrategyState, prices map[string]float64) float64 {
	total := s.Cash
	for sym, pos := range s.Positions {
		price, ok := prices[sym]
		if !ok {
			price = pos.AvgCost // fallback
		}
		if pos.Side == "long" {
			total += pos.Quantity * price
		} else {
			// Short: profit = (avg_cost - current_price) * qty
			total += pos.Quantity * (2*pos.AvgCost - price)
		}
	}
	// Add option positions estimated value
	for _, opt := range s.OptionPositions {
		total += opt.CurrentValueUSD
	}
	return total
}

// ExecuteSpotSignal processes a spot signal and executes paper trades.
func ExecuteSpotSignal(s *StrategyState, signal int, symbol string, price float64, logger *StrategyLogger) (int, error) {
	if signal == 0 {
		return 0, nil
	}
	tradesExecuted := 0

	if signal == 1 { // Buy
		// Check if already long
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			logger.Info("Already long %s (qty=%.6f), skipping buy", symbol, pos.Quantity)
			return 0, nil
		}
		// Close short if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" {
			execPrice := ApplySlippage(price)
			buyCost := pos.Quantity * execPrice
			fee := CalculateSpotFee(buyCost)
			totalCost := buyCost + fee
			pnl := pos.Quantity*pos.AvgCost - totalCost
			s.Cash += pos.Quantity*pos.AvgCost - totalCost
			trade := Trade{
				Timestamp:  time.Now().UTC(),
				StrategyID: s.ID,
				Symbol:     symbol,
				Side:       "buy",
				Quantity:   pos.Quantity,
				Price:      execPrice,
				Value:      totalCost,
				TradeType:  "spot",
				Details:    fmt.Sprintf("Close short, PnL: $%.2f (fee $%.2f)", pnl, fee),
			}
			s.TradeHistory = append(s.TradeHistory, trade)
			RecordTradeResult(&s.RiskState, pnl)
			delete(s.Positions, symbol)
			logger.Info("Closed short %s @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, execPrice, fee, pnl)
			tradesExecuted++
		}
		// Open long — use 95% of cash
		budget := s.Cash * 0.95
		if budget < 1 {
			logger.Info("Insufficient cash ($%.2f) to buy %s", s.Cash, symbol)
			return tradesExecuted, nil
		}
		// Apply slippage
		execPrice := ApplySlippage(price)
		if execPrice <= 0 {
			return tradesExecuted, nil
		}
		qty := budget / execPrice
		tradeCost := qty * execPrice
		fee := CalculateSpotFee(tradeCost)
		s.Cash -= tradeCost + fee
		s.Positions[symbol] = &Position{
			Symbol:   symbol,
			Quantity: qty,
			AvgCost:  execPrice,
			Side:     "long",
		}
		trade := Trade{
			Timestamp:  time.Now().UTC(),
			StrategyID: s.ID,
			Symbol:     symbol,
			Side:       "buy",
			Quantity:   qty,
			Price:      execPrice,
			Value:      tradeCost + fee,
			TradeType:  "spot",
			Details:    fmt.Sprintf("Open long %.6f @ $%.2f (fee $%.2f)", qty, execPrice, fee),
		}
		s.TradeHistory = append(s.TradeHistory, trade)
		logger.Info("BUY %s: %.6f @ $%.2f (fee $%.2f, total $%.2f)", symbol, qty, execPrice, fee, tradeCost+fee)
		tradesExecuted++

	} else if signal == -1 { // Sell
		// Close long if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			execPrice := ApplySlippage(price)
			saleValue := pos.Quantity * execPrice
			fee := CalculateSpotFee(saleValue)
			netProceeds := saleValue - fee
			pnl := netProceeds - (pos.Quantity * pos.AvgCost)
			s.Cash += netProceeds
			trade := Trade{
				Timestamp:  time.Now().UTC(),
				StrategyID: s.ID,
				Symbol:     symbol,
				Side:       "sell",
				Quantity:   pos.Quantity,
				Price:      execPrice,
				Value:      netProceeds,
				TradeType:  "spot",
				Details:    fmt.Sprintf("Close long, PnL: $%.2f (fee $%.2f)", pnl, fee),
			}
			s.TradeHistory = append(s.TradeHistory, trade)
			RecordTradeResult(&s.RiskState, pnl)
			delete(s.Positions, symbol)
			logger.Info("SELL %s: %.6f @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, pos.Quantity, execPrice, fee, pnl)
			tradesExecuted++
		} else {
			logger.Info("No long position in %s to sell, skipping", symbol)
		}
	}
	return tradesExecuted, nil
}
