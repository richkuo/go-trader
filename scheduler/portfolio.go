package main

import (
	"fmt"
	"time"
)

// Position represents a spot, futures, or perps position.
type Position struct {
	Symbol          string    `json:"symbol"`
	Quantity        float64   `json:"quantity"`
	AvgCost         float64   `json:"avg_cost"`
	Side            string    `json:"side"`                        // "long" or "short"
	Multiplier      float64   `json:"multiplier,omitempty"`        // contract multiplier (0 = spot, >0 = futures/perps PnL branch; canonical perps value is 1 — do NOT set to leverage)
	Leverage        float64   `json:"leverage,omitempty"`          // perps leverage (informational; PnL is not scaled by leverage) (#254)
	OwnerStrategyID string    `json:"owner_strategy_id,omitempty"` // strategy that opened this position
	OpenedAt        time.Time `json:"opened_at,omitempty"`         // when the position was opened
}

// ClosedPosition is a historical record of a position after it closed (#288).
// Emitted to the closed_positions table so downstream analytics have explicit
// opened_at/closed_at timestamps without deriving them from trade pairs.
type ClosedPosition struct {
	StrategyID      string    `json:"strategy_id"`
	Symbol          string    `json:"symbol"`
	Quantity        float64   `json:"quantity"`
	AvgCost         float64   `json:"avg_cost"`
	Side            string    `json:"side"`
	Multiplier      float64   `json:"multiplier,omitempty"`
	OpenedAt        time.Time `json:"opened_at"`
	ClosedAt        time.Time `json:"closed_at"`
	ClosePrice      float64   `json:"close_price"`
	RealizedPnL     float64   `json:"realized_pnl"`
	CloseReason     string    `json:"close_reason"`
	DurationSeconds int64     `json:"duration_seconds"`
}

// recordClosedPosition appends a ClosedPosition entry to the strategy's buffer.
// The buffer is flushed to SQLite by SaveState and cleared on successful commit.
func recordClosedPosition(s *StrategyState, pos *Position, closePrice, realizedPnL float64, reason string, closedAt time.Time) {
	var duration int64
	if !pos.OpenedAt.IsZero() {
		duration = int64(closedAt.Sub(pos.OpenedAt).Seconds())
	}
	s.ClosedPositions = append(s.ClosedPositions, ClosedPosition{
		StrategyID:      s.ID,
		Symbol:          pos.Symbol,
		Quantity:        pos.Quantity,
		AvgCost:         pos.AvgCost,
		Side:            pos.Side,
		Multiplier:      pos.Multiplier,
		OpenedAt:        pos.OpenedAt,
		ClosedAt:        closedAt,
		ClosePrice:      closePrice,
		RealizedPnL:     realizedPnL,
		CloseReason:     reason,
		DurationSeconds: duration,
	})
}

// Trade represents a completed trade.
type Trade struct {
	Timestamp       time.Time `json:"timestamp"`
	StrategyID      string    `json:"strategy_id"`
	Symbol          string    `json:"symbol"`
	Side            string    `json:"side"` // "buy" or "sell"
	Quantity        float64   `json:"quantity"`
	Price           float64   `json:"price"`
	Value           float64   `json:"value"`
	TradeType       string    `json:"trade_type"` // "spot", "options", or "futures"
	Details         string    `json:"details"`
	ExchangeOrderID string    `json:"exchange_order_id,omitempty"` // exchange-provided order ID (e.g. Hyperliquid oid)
	ExchangeFee     float64   `json:"exchange_fee,omitempty"`      // fee charged by exchange (if available)

	// persisted tracks whether this Trade has been written to SQLite — set by
	// RecordTrade on successful InsertTrade and by LoadState for DB-loaded
	// rows. SaveState uses this flag instead of a MAX(timestamp) check so an
	// out-of-order RecordTrade failure (T1 fails, T2 succeeds) is picked up
	// on the next flush rather than silently dropped because T1 < latestTS.
	// Not serialized — purely in-memory bookkeeping.
	persisted bool
}

// PortfolioValue calculates total value of a strategy's portfolio.
func PortfolioValue(s *StrategyState, prices map[string]float64) float64 {
	total := s.Cash
	for sym, pos := range s.Positions {
		price, ok := prices[sym]
		if !ok {
			price = pos.AvgCost // fallback
		}
		if pos.Multiplier > 0 {
			// Futures: PnL-based valuation (contracts * multiplier * price delta)
			if pos.Side == "long" {
				total += pos.Quantity * pos.Multiplier * (price - pos.AvgCost)
			} else {
				total += pos.Quantity * pos.Multiplier * (pos.AvgCost - price)
			}
		} else if pos.Side == "long" {
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

// ExecutePerpsSignal processes a perps (perpetual futures) signal with
// margin-based accounting (#254). Unlike spot, perps positions do NOT consume
// the full notional from cash — only the fee is deducted, matching the
// futures model. The resulting Position is stamped with Multiplier=1 so
// PortfolioValue takes the PnL branch (cash + qty*(price-entry)).
//
// leverage determines notional sizing in paper mode: quantity =
// cash * leverage * 0.95 / price. With leverage=1 (default) the sizing
// matches 1x spot notional, but without depleting cash.
//
// fillQty > 0 means a live fill: use price and fillQty as-is (no slippage,
// no notional recalc). fillQty == 0 means paper mode: compute qty from
// leveraged budget with slippage applied.
//
// fillOID/fillFee carry exchange metadata for live fills (empty/zero for
// paper). They are stamped ONLY on the trade that represents the new
// position — the opening trade on signal=1, the closing trade on
// signal=-1. The rationale: one live fill = one exchange fee; if a buy
// signal encounters an existing short, ExecutePerpsSignal synthesizes a
// close-short + open-long pair for in-memory accounting, but the real
// exchange action was the single fill that opened the long. Stamping the
// same fee on both legs would double-count it in analytics. The close-leg
// row therefore carries empty exchange metadata — accurate, since no
// distinct exchange order closed it. See #289.
//
// In current live mode the flip branch is unreachable: signal=-1 does not
// open shorts, and runHyperliquidExecuteOrder sizes buys as a fresh open,
// not close+open. The policy above exists so the invariant survives any
// future adapter that does model flips as two fills or adds short-open.
// The same policy is correct if an adapter ever reports a single atomic
// net-flip fill (one OID, one fee, exchange reduces short and opens long
// in one shot) — the single real fee lands on the opener, which is the
// trade that represents the one exchange action.
func ExecutePerpsSignal(s *StrategyState, signal int, symbol string, price float64, leverage float64, fillQty float64, fillOID string, fillFee float64, logger *StrategyLogger) (int, error) {
	if signal == 0 {
		return 0, nil
	}
	if leverage <= 0 {
		leverage = 1
	}
	tradesExecuted := 0

	// Fee dispatch: for Hyperliquid spot+perps and OKX perps the existing
	// CalculatePlatformSpotFee table already encodes the correct taker fee.
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}

	if signal == 1 { // Buy — go long (close short first if any)
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			logger.Info("Already long %s (qty=%.6f), skipping buy", symbol, pos.Quantity)
			return 0, nil
		}
		// Close short if exists — realize PnL only (no notional swing).
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" {
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			pnl := pos.Quantity * (pos.AvgCost - execPrice)
			fee := CalculatePlatformSpotFee(feePlatform, pos.Quantity*execPrice)
			pnl -= fee
			s.Cash += pnl
			now := time.Now().UTC()
			// Synthetic close — no exchange metadata stamped; the real fill
			// (if any) is attributed to the open-long trade below. Prevents
			// fee double-count when a flip produces two in-memory trades
			// from a single exchange fill.
			trade := Trade{
				Timestamp:  now,
				StrategyID: s.ID,
				Symbol:     symbol,
				Side:       "buy",
				Quantity:   pos.Quantity,
				Price:      execPrice,
				Value:      pos.Quantity * execPrice,
				TradeType:  "perps",
				Details:    fmt.Sprintf("Close short, PnL: $%.2f (fee $%.2f)", pnl, fee),
			}
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
			delete(s.Positions, symbol)
			logger.Info("Closed short %s @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, execPrice, fee, pnl)
			tradesExecuted++
		}
		// Open long
		if s.Cash < 1 {
			logger.Info("Insufficient cash ($%.2f) to open long %s perp", s.Cash, symbol)
			return tradesExecuted, nil
		}
		var execPrice, qty float64
		if fillQty > 0 {
			execPrice = price
			qty = fillQty
		} else {
			execPrice = ApplySlippage(price)
			if execPrice <= 0 {
				return tradesExecuted, nil
			}
			// Leveraged notional: cash * leverage * 0.95
			budget := s.Cash * leverage * 0.95
			qty = budget / execPrice
		}
		notional := qty * execPrice
		fee := CalculatePlatformSpotFee(feePlatform, notional)
		s.Cash -= fee // margin-based: only fee leaves cash, notional stays virtual
		now := time.Now().UTC()
		s.Positions[symbol] = &Position{
			Symbol:          symbol,
			Quantity:        qty,
			AvgCost:         execPrice,
			Side:            "long",
			Multiplier:      1, // perps use 1:1 contract size; PnL-branch in PortfolioValue
			Leverage:        leverage,
			OwnerStrategyID: s.ID,
			OpenedAt:        now,
		}
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          symbol,
			Side:            "buy",
			Quantity:        qty,
			Price:           execPrice,
			Value:           notional,
			TradeType:       "perps",
			Details:         fmt.Sprintf("Open long %.6f @ $%.2f (%.1fx, fee $%.2f)", qty, execPrice, leverage, fee),
			ExchangeOrderID: fillOID,
			ExchangeFee:     fillFee,
		}
		RecordTrade(s, trade)
		logger.Info("BUY %s: %.6f @ $%.2f (%.1fx, notional $%.2f, fee $%.2f)", symbol, qty, execPrice, leverage, notional, fee)
		tradesExecuted++

	} else if signal == -1 { // Sell — close long (no auto-open-short; matches current spot wrapper)
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			pnl := pos.Quantity * (execPrice - pos.AvgCost)
			fee := CalculatePlatformSpotFee(feePlatform, pos.Quantity*execPrice)
			pnl -= fee
			s.Cash += pnl
			now := time.Now().UTC()
			trade := Trade{
				Timestamp:       now,
				StrategyID:      s.ID,
				Symbol:          symbol,
				Side:            "sell",
				Quantity:        pos.Quantity,
				Price:           execPrice,
				Value:           pos.Quantity * execPrice,
				TradeType:       "perps",
				Details:         fmt.Sprintf("Close long, PnL: $%.2f (fee $%.2f)", pnl, fee),
				ExchangeOrderID: fillOID,
				ExchangeFee:     fillFee,
			}
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
			delete(s.Positions, symbol)
			logger.Info("SELL %s: %.6f @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, pos.Quantity, execPrice, fee, pnl)
			tradesExecuted++
		} else {
			logger.Info("No long position in %s to sell, skipping", symbol)
		}
	}
	return tradesExecuted, nil
}

// ExecuteSpotSignal processes a spot signal and executes paper or live trades.
// fillQty > 0 means a live fill: use price as-is (no slippage) and fillQty as position quantity for buys.
// fillQty == 0 means paper mode: apply ApplySlippage and compute qty from state budget.
func ExecuteSpotSignal(s *StrategyState, signal int, symbol string, price float64, fillQty float64, logger *StrategyLogger) (int, error) {
	if signal == 0 {
		return 0, nil
	}
	tradesExecuted := 0
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}

	if signal == 1 { // Buy
		// Check if already long
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			logger.Info("Already long %s (qty=%.6f), skipping buy", symbol, pos.Quantity)
			return 0, nil
		}
		// Close short if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" {
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			buyCost := pos.Quantity * execPrice
			fee := CalculatePlatformSpotFee(feePlatform, buyCost)
			totalCost := buyCost + fee
			pnl := pos.Quantity*pos.AvgCost - totalCost
			s.Cash += pos.Quantity*pos.AvgCost - totalCost
			now := time.Now().UTC()
			trade := Trade{
				Timestamp:  now,
				StrategyID: s.ID,
				Symbol:     symbol,
				Side:       "buy",
				Quantity:   pos.Quantity,
				Price:      execPrice,
				Value:      totalCost,
				TradeType:  "spot",
				Details:    fmt.Sprintf("Close short, PnL: $%.2f (fee $%.2f)", pnl, fee),
			}
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
			delete(s.Positions, symbol)
			logger.Info("Closed short %s @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, execPrice, fee, pnl)
			tradesExecuted++
		}
		// Open long — use 95% of cash (paper) or exact fill qty (live)
		budget := s.Cash * 0.95
		if budget < 1 {
			logger.Info("Insufficient cash ($%.2f) to buy %s", s.Cash, symbol)
			return tradesExecuted, nil
		}
		var execPrice, qty float64
		if fillQty > 0 {
			execPrice = price
			qty = fillQty
		} else {
			execPrice = ApplySlippage(price)
			if execPrice <= 0 {
				return tradesExecuted, nil
			}
			qty = budget / execPrice
		}
		tradeCost := qty * execPrice
		fee := CalculatePlatformSpotFee(feePlatform, tradeCost)
		s.Cash -= tradeCost + fee
		now := time.Now().UTC()
		s.Positions[symbol] = &Position{
			Symbol:          symbol,
			Quantity:        qty,
			AvgCost:         execPrice,
			Side:            "long",
			OwnerStrategyID: s.ID,
			OpenedAt:        now,
		}
		trade := Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     symbol,
			Side:       "buy",
			Quantity:   qty,
			Price:      execPrice,
			Value:      tradeCost + fee,
			TradeType:  "spot",
			Details:    fmt.Sprintf("Open long %.6f @ $%.2f (fee $%.2f)", qty, execPrice, fee),
		}
		RecordTrade(s, trade)
		logger.Info("BUY %s: %.6f @ $%.2f (fee $%.2f, total $%.2f)", symbol, qty, execPrice, fee, tradeCost+fee)
		tradesExecuted++

	} else if signal == -1 { // Sell
		// Close long if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			saleValue := pos.Quantity * execPrice
			fee := CalculatePlatformSpotFee(feePlatform, saleValue)
			netProceeds := saleValue - fee
			pnl := netProceeds - (pos.Quantity * pos.AvgCost)
			s.Cash += netProceeds
			now := time.Now().UTC()
			trade := Trade{
				Timestamp:  now,
				StrategyID: s.ID,
				Symbol:     symbol,
				Side:       "sell",
				Quantity:   pos.Quantity,
				Price:      execPrice,
				Value:      netProceeds,
				TradeType:  "spot",
				Details:    fmt.Sprintf("Close long, PnL: $%.2f (fee $%.2f)", pnl, fee),
			}
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
			delete(s.Positions, symbol)
			logger.Info("SELL %s: %.6f @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, pos.Quantity, execPrice, fee, pnl)
			tradesExecuted++
		} else {
			logger.Info("No long position in %s to sell, skipping", symbol)
		}
	}
	return tradesExecuted, nil
}

// ExecuteFuturesSignal processes a futures signal with whole-contract sizing.
// fillContracts > 0 means a live fill: use price as-is (no slippage) and fillContracts as contract count for opens.
// fillContracts == 0 means paper mode: apply ApplySlippage and compute contracts from state budget.
func ExecuteFuturesSignal(s *StrategyState, signal int, symbol string, price float64, spec ContractSpec, feePerContract float64, maxContracts int, fillContracts int, logger *StrategyLogger) (int, error) {
	if signal == 0 {
		return 0, nil
	}
	tradesExecuted := 0
	multiplier := spec.Multiplier

	if signal == 1 { // Buy
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			logger.Info("Already long %s (%d contracts), skipping buy", symbol, int(pos.Quantity))
			return 0, nil
		}
		// Close short if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" {
			var execPrice float64
			if fillContracts > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			contracts := int(pos.Quantity)
			pnl := float64(contracts) * multiplier * (pos.AvgCost - execPrice)
			fee := CalculateFuturesFee(contracts, feePerContract)
			pnl -= fee
			s.Cash += pnl
			now := time.Now().UTC()
			trade := Trade{
				Timestamp:  now,
				StrategyID: s.ID,
				Symbol:     symbol,
				Side:       "buy",
				Quantity:   pos.Quantity,
				Price:      execPrice,
				Value:      float64(contracts) * multiplier * execPrice,
				TradeType:  "futures",
				Details:    fmt.Sprintf("Close short %d contracts, PnL: $%.2f (fee $%.2f)", contracts, pnl, fee),
			}
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
			delete(s.Positions, symbol)
			logger.Info("Closed short %s %d contracts @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, contracts, execPrice, fee, pnl)
			tradesExecuted++
		}
		// Open long — whole contracts only
		budget := s.Cash * 0.95
		if budget < 1 || price <= 0 || multiplier <= 0 {
			logger.Info("Insufficient cash ($%.2f) to buy %s futures", s.Cash, symbol)
			return tradesExecuted, nil
		}
		var execPrice float64
		var contracts int
		marginPerContract := spec.Margin
		if fillContracts > 0 {
			execPrice = price
			contracts = fillContracts
			if marginPerContract <= 0 {
				marginPerContract = price * multiplier
			}
		} else {
			execPrice = ApplySlippage(price)
			if marginPerContract <= 0 {
				marginPerContract = execPrice * multiplier
			}
			contracts = int(budget / marginPerContract)
			if maxContracts > 0 && contracts > maxContracts {
				contracts = maxContracts
			}
		}
		if contracts < 1 {
			logger.Info("Insufficient cash ($%.2f) for even 1 %s contract (margin=$%.2f)", s.Cash, symbol, marginPerContract)
			return tradesExecuted, nil
		}
		fee := CalculateFuturesFee(contracts, feePerContract)
		s.Cash -= fee // futures use margin, not full notional; deduct fee only
		now := time.Now().UTC()
		s.Positions[symbol] = &Position{
			Symbol:          symbol,
			Quantity:        float64(contracts),
			AvgCost:         execPrice,
			Side:            "long",
			Multiplier:      multiplier,
			OwnerStrategyID: s.ID,
			OpenedAt:        now,
		}
		trade := Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     symbol,
			Side:       "buy",
			Quantity:   float64(contracts),
			Price:      execPrice,
			Value:      float64(contracts) * marginPerContract,
			TradeType:  "futures",
			Details:    fmt.Sprintf("Open long %d contracts @ $%.2f (fee $%.2f)", contracts, execPrice, fee),
		}
		RecordTrade(s, trade)
		logger.Info("BUY %s: %d contracts @ $%.2f (fee $%.2f)", symbol, contracts, execPrice, fee)
		tradesExecuted++

	} else if signal == -1 { // Sell
		// Close long if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			var execPrice float64
			if fillContracts > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			contracts := int(pos.Quantity)
			pnl := float64(contracts) * multiplier * (execPrice - pos.AvgCost)
			fee := CalculateFuturesFee(contracts, feePerContract)
			pnl -= fee
			s.Cash += pnl
			now := time.Now().UTC()
			trade := Trade{
				Timestamp:  now,
				StrategyID: s.ID,
				Symbol:     symbol,
				Side:       "sell",
				Quantity:   pos.Quantity,
				Price:      execPrice,
				Value:      float64(contracts) * multiplier * execPrice,
				TradeType:  "futures",
				Details:    fmt.Sprintf("Close long %d contracts, PnL: $%.2f (fee $%.2f)", contracts, pnl, fee),
			}
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
			delete(s.Positions, symbol)
			logger.Info("SELL %s: %d contracts @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, contracts, execPrice, fee, pnl)
			tradesExecuted++
		}
		// Open short if no long was closed or after closing long
		if _, exists := s.Positions[symbol]; !exists {
			budget := s.Cash * 0.95
			if budget < 1 || price <= 0 || multiplier <= 0 {
				logger.Info("Insufficient cash ($%.2f) to short %s futures", s.Cash, symbol)
				return tradesExecuted, nil
			}
			var execPrice float64
			var contracts int
			marginPerContract := spec.Margin
			if fillContracts > 0 {
				execPrice = price
				contracts = fillContracts
				if marginPerContract <= 0 {
					marginPerContract = price * multiplier
				}
			} else {
				execPrice = ApplySlippage(price)
				if marginPerContract <= 0 {
					marginPerContract = execPrice * multiplier
				}
				contracts = int(budget / marginPerContract)
				if maxContracts > 0 && contracts > maxContracts {
					contracts = maxContracts
				}
			}
			if contracts < 1 {
				logger.Info("Insufficient cash ($%.2f) for even 1 %s short contract (margin=$%.2f)", s.Cash, symbol, marginPerContract)
				return tradesExecuted, nil
			}
			fee := CalculateFuturesFee(contracts, feePerContract)
			s.Cash -= fee
			now := time.Now().UTC()
			s.Positions[symbol] = &Position{
				Symbol:          symbol,
				Quantity:        float64(contracts),
				AvgCost:         execPrice,
				Side:            "short",
				Multiplier:      multiplier,
				OwnerStrategyID: s.ID,
				OpenedAt:        now,
			}
			trade := Trade{
				Timestamp:  now,
				StrategyID: s.ID,
				Symbol:     symbol,
				Side:       "sell",
				Quantity:   float64(contracts),
				Price:      execPrice,
				Value:      float64(contracts) * marginPerContract,
				TradeType:  "futures",
				Details:    fmt.Sprintf("Open short %d contracts @ $%.2f (fee $%.2f)", contracts, execPrice, fee),
			}
			RecordTrade(s, trade)
			logger.Info("SHORT %s: %d contracts @ $%.2f (fee $%.2f)", symbol, contracts, execPrice, fee)
			tradesExecuted++
		}
	}
	return tradesExecuted, nil
}
