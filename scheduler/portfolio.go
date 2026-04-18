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
//
// DurationSeconds == 0 means "unknown" (position migrated from before #288
// without an OpenedAt timestamp) — not "instant close." Analytics that bucket
// by duration should treat zero as a sentinel, not a real value.
//
// ClosePrice note: for the synthetic "hl_sync_external" reason — positions
// that disappeared from the exchange between reconcile cycles — both
// ClosePrice and RealizedPnL are 0 (the real fill price is unknown). Downstream
// analytics that compute avg close price or slippage should filter
// `close_reason != 'hl_sync_external'`.
//
// The JSON tags on this struct are for ad-hoc marshalling by callers (status
// endpoint responses, leaderboard summaries); StrategyState.ClosedPositions
// itself is `json:"-"` because history lives only in SQLite.
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

// ClosedOptionPosition is a historical record of an option position after it
// closed (#288). Same lifecycle notes as ClosedPosition: DurationSeconds == 0
// means unknown opened_at; expiry-based closes (expired_worthless,
// expired_itm) record ClosePriceUSD as the intrinsic value at expiry.
type ClosedOptionPosition struct {
	StrategyID      string    `json:"strategy_id"`
	PositionID      string    `json:"position_id"`
	Underlying      string    `json:"underlying"`
	OptionType      string    `json:"option_type"` // "call" or "put"
	Strike          float64   `json:"strike"`
	Expiry          string    `json:"expiry"`
	Action          string    `json:"action"` // original direction: "buy" or "sell"
	Quantity        float64   `json:"quantity"`
	EntryPremiumUSD float64   `json:"entry_premium_usd"`
	ClosePriceUSD   float64   `json:"close_price_usd"`
	RealizedPnL     float64   `json:"realized_pnl"`
	OpenedAt        time.Time `json:"opened_at"`
	ClosedAt        time.Time `json:"closed_at"`
	CloseReason     string    `json:"close_reason"`
	DurationSeconds int64     `json:"duration_seconds"`
}

// recordClosedPosition appends a ClosedPosition entry to the strategy's buffer.
// The buffer is flushed to SQLite by SaveState and cleared on successful commit.
//
// Durability boundary: this records to an in-memory buffer only. Call sites
// invoke it immediately before delete(s.Positions, symbol), so a crash between
// the close and the next SaveState loses the closed_position row (the Trade
// row is still persisted eagerly via the tradeRecorder hook). Downstream
// analytics that must survive every crash should be derived from the trades
// table, not closed_positions.
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

// recordClosedOptionPosition appends a ClosedOptionPosition entry to the
// strategy's buffer. Same durability boundary as recordClosedPosition —
// in-memory until SaveState commits.
func recordClosedOptionPosition(s *StrategyState, pos *OptionPosition, closePriceUSD, realizedPnL float64, reason string, closedAt time.Time) {
	var duration int64
	if !pos.OpenedAt.IsZero() {
		duration = int64(closedAt.Sub(pos.OpenedAt).Seconds())
	}
	s.ClosedOptionPositions = append(s.ClosedOptionPositions, ClosedOptionPosition{
		StrategyID:      s.ID,
		PositionID:      pos.ID,
		Underlying:      pos.Underlying,
		OptionType:      pos.OptionType,
		Strike:          pos.Strike,
		Expiry:          pos.Expiry,
		Action:          pos.Action,
		Quantity:        pos.Quantity,
		EntryPremiumUSD: pos.EntryPremiumUSD,
		ClosePriceUSD:   closePriceUSD,
		RealizedPnL:     realizedPnL,
		OpenedAt:        pos.OpenedAt,
		ClosedAt:        closedAt,
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

// PerpsOrderSkipReason returns a non-empty reason when ExecutePerpsSignal
// would treat (signal, current position side) as a no-op. Callers that place
// live orders BEFORE invoking ExecutePerpsSignal (e.g. runHyperliquidExecuteOrder)
// must consult this guard first — otherwise the on-chain fill happens but the
// in-memory execution path returns 0 and no Trade is recorded, leaving
// virtual state permanently behind actual exchange positions (#298).
//
// posSide is "" when no position exists; "long" or "short" otherwise.
// allowShorts toggles the branches that ExecutePerpsSignal exposes when
// bidirectional execution is enabled (#328):
//   - allowShorts=false (legacy): signal=-1 with no long is a skip (close-long-only).
//   - allowShorts=true: signal=-1 with no position opens a short; signal=-1
//     while already short is a skip (mirrors "already long, skipping buy").
//
// The `s.Cash < 1` branch inside the open paths is NOT mirrored here because
// cash after a flip-close leg cannot be derived from (signal, posSide) alone —
// live callers guard cash upstream before placing the order (see
// runHyperliquidExecuteOrder). If a new side-based no-op branch is added to
// ExecutePerpsSignal, add it here too.
func PerpsOrderSkipReason(signal int, posSide string, allowShorts bool) string {
	switch signal {
	case 1:
		if posSide == "long" {
			return "already long, skipping buy"
		}
	case -1:
		if allowShorts {
			if posSide == "short" {
				return "already short, skipping sell"
			}
		} else if posSide != "long" {
			return "no long position to sell, skipping"
		}
	}
	return ""
}

// SpotOrderSkipReason mirrors PerpsOrderSkipReason for spot. ExecuteSpotSignal's
// side-based skip branches ("already long, skipping buy" at signal=1,
// "No long position to sell, skipping" at signal=-1) must be consulted BEFORE
// the live helper spawns a Python order placer — otherwise a live fill lands
// on the exchange but ExecuteSpotSignal returns 0 and no Trade is recorded,
// leaving virtual state behind real holdings. See #298 / #300.
//
// Matching conditions to ExecuteSpotSignal:
//   - signal == 1 && pos.Side == "long"  → "Already long, skipping buy"
//   - signal == -1 && no long position    → "No long position to sell, skipping"
//
// Cash-insufficient skips inside the open-long path are not mirrored here —
// live helpers guard cash upstream before placing the order.
func SpotOrderSkipReason(signal int, posSide string) string {
	switch signal {
	case 1:
		if posSide == "long" {
			return "already long, skipping buy"
		}
	case -1:
		if posSide != "long" {
			return "no long position to sell, skipping"
		}
	}
	return ""
}

// FuturesOrderSkipReason is the futures peer of PerpsOrderSkipReason. It
// reflects the CLOSE-LONG-ONLY semantics of the current TopStep live helper
// (runTopStepExecuteOrder treats signal=-1 as close-long and never opens a
// live short, even though paper-mode ExecuteFuturesSignal can). With those
// semantics, the guard matches spot/perps:
//   - signal == 1 && pos.Side == "long" → "Already long, skipping buy"
//   - signal == -1 && no long position   → "No long position to sell, skipping"
//
// Without this guard, a live sell fires with posSide=="short" (Quantity is
// always positive so the posQty<=0 check does not catch it) but
// ExecuteFuturesSignal is a side-based no-op when already short, producing a
// silent state drift identical in shape to #298. If the live helper is ever
// extended to open shorts, this guard must be revisited.
func FuturesOrderSkipReason(signal int, posSide string) string {
	switch signal {
	case 1:
		if posSide == "long" {
			return "already long, skipping buy"
		}
	case -1:
		if posSide != "long" {
			return "no long position to sell, skipping"
		}
	}
	return ""
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
// allowShorts toggles bidirectional semantics (#328). When true, signal=-1
// from flat opens a short, and signal=-1 on an existing long flips to a
// short after closing (mirrored to the existing signal=1 + short branch
// which already closes-and-flips). When false (default), signal=-1 only
// closes a long and never opens a short — the legacy long-only behavior
// that strategies like triple_ema and rsi_macd_combo depend on.
//
// Fill metadata rationale: one live fill = one exchange fee; if a signal
// encounters an opposite-side position, ExecutePerpsSignal synthesizes a
// close+open pair for in-memory accounting, but the real exchange action
// was the single fill that opened the new side. Stamping the same fee on
// both legs would double-count it in analytics. The close leg therefore
// carries empty exchange metadata — accurate, since no distinct exchange
// order closed it. See #289.
func ExecutePerpsSignal(s *StrategyState, signal int, symbol string, price float64, leverage float64, fillQty float64, fillOID string, fillFee float64, allowShorts bool, logger *StrategyLogger) (int, error) {
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

	} else if signal == -1 { // Sell
		// Dedupe: already short and allowShorts means nothing new to do (mirrors
		// the "already long, skipping buy" branch above).
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" && allowShorts {
			logger.Info("Already short %s (qty=%.6f), skipping sell", symbol, pos.Quantity)
			return 0, nil
		}
		// Close long if exists — realize PnL.
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
			// When flipping to short, the close-long leg is the synthetic half of
			// a single real exchange fill — same rationale as the close-short leg
			// in the signal=1 branch. Stamp exchange metadata only on the new
			// opener so the fee is not double-counted.
			var closeOID string
			var closeFee float64
			if !allowShorts {
				closeOID = fillOID
				closeFee = fillFee
			}
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
				ExchangeOrderID: closeOID,
				ExchangeFee:     closeFee,
			}
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
			delete(s.Positions, symbol)
			logger.Info("SELL %s: %.6f @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, pos.Quantity, execPrice, fee, pnl)
			tradesExecuted++
		} else if !allowShorts {
			logger.Info("No long position in %s to sell, skipping", symbol)
			return tradesExecuted, nil
		}
		// Open short when bidirectional execution is enabled.
		if !allowShorts {
			return tradesExecuted, nil
		}
		if s.Cash < 1 {
			logger.Info("Insufficient cash ($%.2f) to open short %s perp", s.Cash, symbol)
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
			budget := s.Cash * leverage * 0.95
			qty = budget / execPrice
		}
		notional := qty * execPrice
		fee := CalculatePlatformSpotFee(feePlatform, notional)
		s.Cash -= fee // margin-based: only fee leaves cash
		now := time.Now().UTC()
		s.Positions[symbol] = &Position{
			Symbol:          symbol,
			Quantity:        qty,
			AvgCost:         execPrice,
			Side:            "short",
			Multiplier:      1,
			Leverage:        leverage,
			OwnerStrategyID: s.ID,
			OpenedAt:        now,
		}
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          symbol,
			Side:            "sell",
			Quantity:        qty,
			Price:           execPrice,
			Value:           notional,
			TradeType:       "perps",
			Details:         fmt.Sprintf("Open short %.6f @ $%.2f (%.1fx, fee $%.2f)", qty, execPrice, leverage, fee),
			ExchangeOrderID: fillOID,
			ExchangeFee:     fillFee,
		}
		RecordTrade(s, trade)
		logger.Info("SELL %s: %.6f @ $%.2f (%.1fx, notional $%.2f, fee $%.2f) [open short]", symbol, qty, execPrice, leverage, notional, fee)
		tradesExecuted++
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
