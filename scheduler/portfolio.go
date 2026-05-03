package main

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Position represents a spot, futures, or perps position.
type Position struct {
	Symbol              string    `json:"symbol"`
	TradePositionID     string    `json:"position_id,omitempty"`
	Quantity            float64   `json:"quantity"`
	InitialQuantity     float64   `json:"initial_quantity,omitempty"` // original open size; partial closes must not rewrite it (#496)
	AvgCost             float64   `json:"avg_cost"`
	EntryATR            float64   `json:"entry_atr,omitempty"`               // ATR value from the entry strategy's open candle when available (#496)
	Side                string    `json:"side"`                              // "long" or "short"
	Multiplier          float64   `json:"multiplier,omitempty"`              // contract multiplier (0 = spot, >0 = futures/perps PnL branch; canonical perps value is 1 — do NOT set to leverage)
	Leverage            float64   `json:"leverage,omitempty"`                // perps exchange leverage (informational; PnL is not scaled by leverage) (#254/#497)
	OwnerStrategyID     string    `json:"owner_strategy_id,omitempty"`       // strategy that opened this position
	OpenedAt            time.Time `json:"opened_at,omitempty"`               // when the position was opened
	StopLossOID         int64     `json:"stop_loss_oid,omitempty"`           // HL perps: resting trigger-order OID for the per-trade stop-loss (0 = none) (#412)
	StopLossTriggerPx   float64   `json:"stop_loss_trigger_px,omitempty"`    // HL perps: trigger price for the resting stop-loss (0 = unknown) (#421)
	StopLossHighWaterPx float64   `json:"stop_loss_high_water_px,omitempty"` // HL perps trailing SL: best mark seen while position open (high for long, low for short) (#501)
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

// recordPerpsStopLossClose books a tracked perps stop-loss fill and removes the
// virtual position. Used both when HL reports an immediate trigger fill at
// submit time and when a previously-resting trigger has fired between cycles.
func recordPerpsStopLossClose(s *StrategyState, symbol string, triggerPx float64, reason string, logger *StrategyLogger) bool {
	if triggerPx <= 0 {
		return false
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil {
		return false
	}

	now := time.Now().UTC()
	qty := pos.Quantity
	avgCost := pos.AvgCost
	side := pos.Side
	var pnl float64
	if side == "long" {
		pnl = qty * (triggerPx - avgCost)
	} else {
		pnl = qty * (avgCost - triggerPx)
	}
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}
	fee := CalculatePlatformSpotFee(feePlatform, qty*triggerPx)
	pnl -= fee
	s.Cash += pnl
	positionID := ensurePositionTradeID(s.ID, symbol, pos)

	trade := Trade{
		Timestamp:   now,
		StrategyID:  s.ID,
		Symbol:      symbol,
		PositionID:  positionID,
		Side:        closeTradeSide(side),
		Quantity:    qty,
		Price:       triggerPx,
		Value:       qty * triggerPx,
		TradeType:   "perps",
		Details:     fmt.Sprintf("Stop loss close, PnL: $%.2f (fee $%.2f)", pnl, fee),
		IsClose:     true,
		RealizedPnL: pnl,
	}
	trade.Regime = s.Regime
	trade.EntryATR = pos.EntryATR
	trade.StopLossTriggerPx = pos.StopLossTriggerPx
	RecordTrade(s, trade)
	RecordTradeResult(&s.RiskState, pnl)
	recordClosedPosition(s, pos, triggerPx, pnl, reason, now)
	delete(s.Positions, symbol)
	clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
	if logger != nil {
		logger.Warn("SL close reconciled @ $%.4f, PnL: $%.2f (fee $%.2f)", triggerPx, pnl, fee)
	}
	return true
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
	PositionID      string    `json:"position_id"`
	ExchangeOrderID string    `json:"exchange_order_id,omitempty"` // exchange-provided order ID (e.g. Hyperliquid oid)
	ExchangeFee     float64   `json:"exchange_fee,omitempty"`      // fee charged by exchange (if available)

	// IsClose marks closing legs of a round-trip (close, stop-loss, circuit-breaker
	// liquidation, theta harvest, wheel call-away). Used by lifetime-stats queries
	// (#455) to count round-trips and W/L without resetting on kill switch /
	// circuit breaker. Opens leave it false. RealizedPnL is the per-trade realized
	// PnL on close legs (0 on opens). Both columns are append-only metadata: once
	// inserted on a close, they identify the round-trip in the trades table.
	IsClose     bool    `json:"is_close,omitempty"`
	RealizedPnL float64 `json:"realized_pnl,omitempty"`
	Regime      string  `json:"regime,omitempty"` // market regime label at time of trade (#482)

	EntryATR          float64 `json:"entry_atr,omitempty"`
	StopLossTriggerPx float64 `json:"stop_loss_trigger_px,omitempty"`

	// persisted tracks whether this Trade has been written to SQLite — set by
	// RecordTrade on successful InsertTrade and by LoadState for DB-loaded
	// rows. SaveState uses this flag instead of a MAX(timestamp) check so an
	// out-of-order RecordTrade failure (T1 fails, T2 succeeds) is picked up
	// on the next flush rather than silently dropped because T1 < latestTS.
	// Not serialized — purely in-memory bookkeeping.
	persisted bool
}

var tradePositionNonce uint64

func newTradePositionID(strategyID, symbol string, openedAt time.Time) string {
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}
	nonce := atomic.AddUint64(&tradePositionNonce, 1)
	return fmt.Sprintf("%s:%s:%d:%d", strategyID, symbol, openedAt.UnixNano(), nonce)
}

func ensurePositionTradeID(strategyID, symbol string, pos *Position) string {
	if pos == nil {
		return ""
	}
	if pos.TradePositionID == "" {
		pos.TradePositionID = newTradePositionID(strategyID, symbol, pos.OpenedAt)
	}
	return pos.TradePositionID
}

func ensureOptionTradeID(strategyID string, pos *OptionPosition) string {
	if pos == nil {
		return ""
	}
	if pos.TradePositionID == "" {
		pos.TradePositionID = newTradePositionID(strategyID, pos.ID, pos.OpenedAt)
	}
	return pos.TradePositionID
}

// Defaulting to "sell" preserves legacy behavior for missing/unknown sides.
func closeTradeSide(positionSide string) string {
	if positionSide == "short" {
		return "buy"
	}
	return "sell"
}

func optionCloseTradeSide(action string) string {
	if action == "sell" {
		return "buy"
	}
	return "sell"
}

func executionFee(modeledFee, fillFee float64, useFillFee bool) float64 {
	if useFillFee && fillFee > 0 {
		return fillFee
	}
	return modeledFee
}

func exchangeFeeForTrade(fillFee float64, useFillFee bool) float64 {
	if useFillFee && fillFee > 0 {
		return fillFee
	}
	return 0
}

func exchangeOrderIDForTrade(fillOID string, useFillMetadata bool) string {
	if useFillMetadata {
		return fillOID
	}
	return ""
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

// perpsLiveOrderSize returns the market-order size to place for a live perps
// execution. PerpsOrderSkipReason must already have passed (no skip). The
// four cases:
//
//   - Fresh open (posQty <= 0):
//   - signal=1           → size = PerpsOpenNotional(...)/price (long from flat)
//   - signal=-1 + AllowShorts → size = PerpsOpenNotional(...)/price (short from flat)
//   - Close-only (legacy, !AllowShorts):
//   - signal=-1 + long   → size = posQty
//   - Flip (AllowShorts + opposite-side position):
//   - signal=1 + short   → size = posQty + PerpsOpenNotional(cash+closePnL,...)/price
//   - signal=-1 + long   → size = posQty + PerpsOpenNotional(cash+closePnL,...)/price
//
// The flip branch is what this helper exists for: without `posQty + newSize`
// a bidirectional scheduler tells ExecutePerpsSignal to virtually close+open
// in one step, but the exchange only closes (size = newSize or size = posQty
// picked either way would desync). A single net-flip order of
// `posQty + newSize` settles to the new side at the intended notional and
// matches the virtual-state transition exactly — see PR #330 review.
//
// avgCost is the entry price of the existing position (0 when flat). For a
// flip, the new-side budget uses `cash + expectedClosePnL` rather than raw
// `cash` so a losing long→short flip at higher leverage doesn't over-size
// past post-close exchange margin. expectedClosePnL can be negative; if it
// zeroes out the post-close budget the flip degrades to close-only sizing
// (reported as insufficient cash rather than silently undersizing).
//
// marginPerTradeUSD opts the sizing into margin-space (#518): when positive,
// notional = min(marginPerTradeUSD, effectiveCash) × exchangeLeverage,
// independent of sizingLeverage. The hardcoded 0.95 safety buffer was
// removed in #518.
//
// Returns (size, ok); when ok is false `reason` is a log-ready string.
//
// closeFraction (#519) scales the close-only return when 0 < frac < 1: a
// partial-close decision from the open/close registry (e.g. tiered_tp_atr
// tier 1) is composed into signal=-1 (long) / signal=+1 (short) by
// shared_tools/strategy_composition.compose_signal — the fraction is the only
// signal that fewer than all of posQty should be reduced. The flip branch is
// unreachable when closeFraction > 0 because compose_signal does not emit a
// flip alongside a close (open_action is dropped while a position is open),
// so closeFraction is intentionally ignored on the open/flip path.
func perpsLiveOrderSize(signal int, price, cash, posQty, avgCost, sizingLeverage, exchangeLeverage, marginPerTradeUSD float64, posSide string, allowShorts bool, closeFraction float64) (size float64, ok bool, reason string) {
	isBuy := signal == 1
	flipping := allowShorts && posQty > 0 && ((isBuy && posSide == "short") || (!isBuy && posSide == "long"))
	// Fresh open: buy always fresh-sizes (legacy buy-vs-migrated-short kept
	// the pre-#330 fresh-open sizing for that edge case), or AllowShorts
	// short-from-flat. Sell + !AllowShorts + no long is unreachable —
	// PerpsOrderSkipReason handled it.
	openingFresh := isBuy || (!isBuy && allowShorts && posQty <= 0)

	if openingFresh || flipping {
		effectiveCash := cash
		if flipping {
			// Close leg realizes PnL before the new side opens on-chain;
			// size the new side against post-close margin so a losing flip
			// at higher leverage doesn't exceed exchange capacity.
			var closePnL float64
			if isBuy { // short → long: profit when price < avgCost
				closePnL = posQty * (avgCost - price)
			} else { // long → short: profit when price > avgCost
				closePnL = posQty * (price - avgCost)
			}
			effectiveCash = cash + closePnL
		}
		budget := PerpsOpenNotional(effectiveCash, sizingLeverage, exchangeLeverage, marginPerTradeUSD)
		if budget < 1 || price <= 0 {
			// Flip + catastrophic drawdown (realized loss wipes out post-close
			// margin): the new side can't be sized, but the close leg still
			// must fire — otherwise a deep-underwater bidirectional strategy
			// would be worse at exiting than a legacy long-only one. Degrade
			// to close-only sizing as the docstring promises.
			if flipping {
				return posQty, true, ""
			}
			label := "buy"
			if !isBuy {
				label = "sell (short-open)"
			}
			return 0, false, fmt.Sprintf("insufficient cash ($%.2f effective) for live %s", effectiveCash, label)
		}
		newSize := budget / price
		if flipping {
			return posQty + newSize, true, ""
		}
		return newSize, true, ""
	}
	// close-only: signal=-1 + long + !allowShorts (or signal=+1 + short
	// composed from a close strategy on a long-only-flipped runtime)
	if posQty <= 0 {
		return 0, false, "no position to close"
	}
	if closeFraction > 0 && closeFraction < 1 {
		// Partial close (#519): tiered_tp_* / fractional close strategies
		// emit close_fraction relative to current_quantity. Size the live
		// order to match so the exchange and virtual state agree on the
		// close leg before Execute*Signal records it.
		return posQty * closeFraction, true, ""
	}
	return posQty, true, ""
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
// sizingLeverage determines notional sizing in paper mode: quantity =
// cash * sizingLeverage / price (the hardcoded 0.95 buffer was removed in
// #518). exchangeLeverage is stored on Position for exchange-margin reporting
// and risk math. The legacy wrapper passes the same value for both so old
// behavior is unchanged (#497). marginPerTradeUSD opts the open into
// margin-space sizing (#518): when positive, paper notional becomes
// min(marginPerTradeUSD, cash) × exchangeLeverage, independent of
// sizingLeverage.
//
// fillQty > 0 means a live fill: use price and fillQty as-is (no slippage,
// no notional recalc). fillQty == 0 means paper mode: compute qty from
// sizing-leverage budget with slippage applied.
//
// fillOID/fillFee carry exchange metadata for live fills (empty/zero for
// paper). One live fill = one exchange fee; if a bidirectional signal flips an
// opposite-side position, ExecutePerpsSignal synthesizes a close+open pair.
// The close leg owns the exchange-reported fill fee; the open leg uses modeled
// fee cash math so the real fee is not counted twice. See #451.
//
// allowShorts toggles bidirectional semantics (#328). When true, signal=-1
// from flat opens a short, and signal=-1 on an existing long flips to a
// short after closing (mirrored to the existing signal=1 + short branch
// which already closes-and-flips). When false (default), signal=-1 only
// closes a long and never opens a short — the legacy long-only behavior
// that strategies like triple_ema and rsi_macd_combo depend on.
func ExecutePerpsSignal(s *StrategyState, signal int, symbol string, price float64, leverage float64, fillQty float64, fillOID string, fillFee float64, allowShorts bool, logger *StrategyLogger) (int, error) {
	return ExecutePerpsSignalWithLeverage(s, signal, symbol, price, leverage, leverage, 0, fillQty, fillOID, fillFee, allowShorts, 0, logger)
}

// ExecutePerpsSignalWithLeverage processes a perps signal.
//
// closeFraction (#519) selects partial-close accounting: when 0 < frac < 1
// AND the signal is a close-action emitted by the open/close registry
// (compose_signal returns -1 on long / +1 on short), the close leg reduces
// pos.Quantity by frac (paper) or fillQty (live) without deleting the
// position, and the bidirectional open-leg path is skipped (compose_signal
// never composes close+open in the same cycle). closeFraction == 0 preserves
// the legacy full-close behavior used by direct strategy signals,
// kill-switch, stop-loss, and forceCloseAllPositions paths.
func ExecutePerpsSignalWithLeverage(s *StrategyState, signal int, symbol string, price float64, sizingLeverage, exchangeLeverage, marginPerTradeUSD float64, fillQty float64, fillOID string, fillFee float64, allowShorts bool, closeFraction float64, logger *StrategyLogger) (int, error) {
	if signal == 0 {
		return 0, nil
	}
	if sizingLeverage <= 0 {
		sizingLeverage = 1
	}
	if exchangeLeverage <= 0 {
		exchangeLeverage = sizingLeverage
	}
	tradesExecuted := 0
	leverageLabel := perpsLeverageLabel(exchangeLeverage, sizingLeverage)
	// #519: partial close suppresses the bidirectional open-leg path —
	// compose_signal never composes a close+open in the same cycle, so any
	// fractional close emitted by the open/close registry is close-only.
	partialClose := closeFraction > 0 && closeFraction < 1
	closeOnlyAction := closeFraction > 0 // any close decision skips open-leg

	// Fee dispatch: for Hyperliquid spot+perps and OKX perps the existing
	// CalculatePlatformSpotFee table already encodes the correct taker fee.
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}

	// flipCloseQty lets the open leg subtract the close-leg qty from a live
	// fill when the exchange executes a single net-flip order of
	// (posQty + newSize). Only set when AllowShorts=true so #451 can charge
	// the real fill fee to the close leg and modeled fee to the open leg on
	// bidirectional flips. Legacy paths (e.g. a migrated short closed by a
	// long-only strategy) keep fillQty as the open-side-only qty, so the open
	// leg carries the single live fill fee.
	var flipCloseQty float64

	if signal == 1 { // Buy — go long (close short first if any)
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			logger.Info("Already long %s (qty=%.6f), skipping buy", symbol, pos.Quantity)
			return 0, nil
		}
		// Close short if exists — realize PnL only (no notional swing).
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" {
			closeQty := pos.Quantity
			if partialClose {
				if fillQty > 0 {
					closeQty = fillQty
				} else {
					closeQty = pos.Quantity * closeFraction
				}
			}
			if allowShorts {
				flipCloseQty = closeQty
			}
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			pnl := closeQty * (pos.AvgCost - execPrice)
			useFillFee := flipCloseQty > 0 || closeOnlyAction
			fee := executionFee(CalculatePlatformSpotFee(feePlatform, closeQty*execPrice), fillFee, useFillFee)
			pnl -= fee
			s.Cash += pnl
			now := time.Now().UTC()
			positionID := ensurePositionTradeID(s.ID, symbol, pos)
			var closeOID string
			if useFillFee {
				closeOID = fillOID
			}
			details := fmt.Sprintf("Close short, PnL: $%.2f (fee $%.2f)", pnl, fee)
			if partialClose {
				details = fmt.Sprintf("Partial-close short %.6f, PnL: $%.2f (fee $%.2f)", closeQty, pnl, fee)
			}
			trade := Trade{
				Timestamp:       now,
				StrategyID:      s.ID,
				Symbol:          symbol,
				PositionID:      positionID,
				Side:            "buy",
				Quantity:        closeQty,
				Price:           execPrice,
				Value:           closeQty * execPrice,
				TradeType:       "perps",
				Details:         details,
				ExchangeOrderID: closeOID,
				ExchangeFee:     exchangeFeeForTrade(fillFee, useFillFee),
				IsClose:         true,
				RealizedPnL:     pnl,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			if partialClose {
				pos.Quantity -= closeQty
				logger.Info("Partial-close short %s: %.6f (remaining %.6f) @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, closeQty, pos.Quantity, execPrice, fee, pnl)
			} else {
				recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
				delete(s.Positions, symbol)
				clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
				logger.Info("Closed short %s @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, execPrice, fee, pnl)
			}
			tradesExecuted++
		}
		// Close-action from the open/close registry (#519): the registry
		// never composes close+open in the same cycle, so any close decision
		// (partial OR full) skips the open-leg path. Legacy direct-signal
		// flips (closeFraction == 0) keep falling through.
		if closeOnlyAction {
			return tradesExecuted, nil
		}
		// Open long
		if s.Cash < 1 {
			logger.Info("Insufficient cash ($%.2f) to open long %s perp", s.Cash, symbol)
			return tradesExecuted, nil
		}
		var execPrice, qty float64
		if fillQty > 0 {
			execPrice = price
			qty = fillQty - flipCloseQty
			if qty <= 0 {
				// Partial-fill on a flip order — the scheduler intended to flip
				// but the exchange only closed. Warn so regressions are visible
				// in the strategy log (matching risk.go's Warn-level signals).
				logger.Warn("Flip fill qty (%.6f) did not cover new long after closing short (%.6f); leaving flat", fillQty, flipCloseQty)
				return tradesExecuted, nil
			}
		} else {
			execPrice = ApplySlippage(price)
			if execPrice <= 0 {
				return tradesExecuted, nil
			}
			// Notional sizing (#518): margin-based when MarginPerTradeUSD set,
			// else legacy cash × sizing_leverage. The 0.95 safety buffer was
			// removed in #518 — operators wanting headroom set a smaller
			// sizing_leverage or margin_per_trade_usd explicitly.
			budget := PerpsOpenNotional(s.Cash, sizingLeverage, exchangeLeverage, marginPerTradeUSD)
			qty = budget / execPrice
		}
		notional := qty * execPrice
		useFillFee := flipCloseQty == 0
		fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), fillFee, useFillFee)
		s.Cash -= fee // margin-based: only fee leaves cash, notional stays virtual
		now := time.Now().UTC()
		positionID := newTradePositionID(s.ID, symbol, now)
		var openOID string
		if useFillFee {
			openOID = fillOID
		}
		s.Positions[symbol] = &Position{
			Symbol:          symbol,
			Quantity:        qty,
			InitialQuantity: qty,
			AvgCost:         execPrice,
			Side:            "long",
			Multiplier:      1, // perps use 1:1 contract size; PnL-branch in PortfolioValue
			Leverage:        exchangeLeverage,
			OwnerStrategyID: s.ID,
			OpenedAt:        now,
			TradePositionID: positionID,
		}
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          symbol,
			PositionID:      positionID,
			Side:            "buy",
			Quantity:        qty,
			Price:           execPrice,
			Value:           notional,
			TradeType:       "perps",
			Details:         fmt.Sprintf("Open long %.6f @ $%.2f (%s, fee $%.2f)", qty, execPrice, leverageLabel, fee),
			ExchangeOrderID: openOID,
			ExchangeFee:     exchangeFeeForTrade(fillFee, useFillFee),
		}
		trade.Regime = s.Regime
		RecordTrade(s, trade)
		logger.Info("BUY %s: %.6f @ $%.2f (%s, notional $%.2f, fee $%.2f)", symbol, qty, execPrice, leverageLabel, notional, fee)
		tradesExecuted++

	} else if signal == -1 { // Sell
		// Dedupe: already short and allowShorts means nothing new to do —
		// symmetric mirror of the "Already long ... skipping buy" branch at
		// portfolio.go:408 in the signal==1 block above.
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" && allowShorts {
			logger.Info("Already short %s (qty=%.6f), skipping sell", symbol, pos.Quantity)
			return 0, nil
		}
		// Close long if exists — realize PnL.
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			closeQty := pos.Quantity
			if partialClose {
				if fillQty > 0 {
					closeQty = fillQty
				} else {
					closeQty = pos.Quantity * closeFraction
				}
			}
			if allowShorts {
				flipCloseQty = closeQty
			}
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			pnl := closeQty * (execPrice - pos.AvgCost)
			useFillFee := flipCloseQty > 0 || !allowShorts || closeOnlyAction
			fee := executionFee(CalculatePlatformSpotFee(feePlatform, closeQty*execPrice), fillFee, useFillFee)
			pnl -= fee
			s.Cash += pnl
			now := time.Now().UTC()
			positionID := ensurePositionTradeID(s.ID, symbol, pos)
			var closeOID string
			if useFillFee {
				closeOID = fillOID
			}
			details := fmt.Sprintf("Close long, PnL: $%.2f (fee $%.2f)", pnl, fee)
			if partialClose {
				details = fmt.Sprintf("Partial-close long %.6f, PnL: $%.2f (fee $%.2f)", closeQty, pnl, fee)
			}
			trade := Trade{
				Timestamp:       now,
				StrategyID:      s.ID,
				Symbol:          symbol,
				PositionID:      positionID,
				Side:            "sell",
				Quantity:        closeQty,
				Price:           execPrice,
				Value:           closeQty * execPrice,
				TradeType:       "perps",
				Details:         details,
				ExchangeOrderID: closeOID,
				ExchangeFee:     exchangeFeeForTrade(fillFee, useFillFee),
				IsClose:         true,
				RealizedPnL:     pnl,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			if partialClose {
				pos.Quantity -= closeQty
				logger.Info("Partial-close long %s: %.6f (remaining %.6f) @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, closeQty, pos.Quantity, execPrice, fee, pnl)
			} else {
				recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
				delete(s.Positions, symbol)
				clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
				logger.Info("SELL %s: %.6f @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, closeQty, execPrice, fee, pnl)
			}
			tradesExecuted++
		}
		// Close-action from the open/close registry (#519): see comment on
		// the symmetric branch in the signal==1 block above.
		if closeOnlyAction {
			return tradesExecuted, nil
		}
		// Legacy long-only path: whether we closed a long or had nothing to
		// close, AllowShorts=false never opens a short. Log only when we did
		// nothing (close-path already logged).
		if !allowShorts {
			if tradesExecuted == 0 {
				logger.Info("No long position in %s to sell, skipping", symbol)
			}
			return tradesExecuted, nil
		}
		// Open short (AllowShorts=true).
		if s.Cash < 1 {
			logger.Info("Insufficient cash ($%.2f) to open short %s perp", s.Cash, symbol)
			return tradesExecuted, nil
		}
		var execPrice, qty float64
		if fillQty > 0 {
			execPrice = price
			qty = fillQty - flipCloseQty
			if qty <= 0 {
				logger.Warn("Flip fill qty (%.6f) did not cover new short after closing long (%.6f); leaving flat", fillQty, flipCloseQty)
				return tradesExecuted, nil
			}
		} else {
			execPrice = ApplySlippage(price)
			if execPrice <= 0 {
				return tradesExecuted, nil
			}
			budget := PerpsOpenNotional(s.Cash, sizingLeverage, exchangeLeverage, marginPerTradeUSD)
			qty = budget / execPrice
		}
		notional := qty * execPrice
		useFillFee := flipCloseQty == 0
		fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), fillFee, useFillFee)
		s.Cash -= fee // margin-based: only fee leaves cash
		now := time.Now().UTC()
		positionID := newTradePositionID(s.ID, symbol, now)
		var openOID string
		if useFillFee {
			openOID = fillOID
		}
		s.Positions[symbol] = &Position{
			Symbol:          symbol,
			Quantity:        qty,
			InitialQuantity: qty,
			AvgCost:         execPrice,
			Side:            "short",
			Multiplier:      1,
			Leverage:        exchangeLeverage,
			OwnerStrategyID: s.ID,
			OpenedAt:        now,
			TradePositionID: positionID,
		}
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          symbol,
			PositionID:      positionID,
			Side:            "sell",
			Quantity:        qty,
			Price:           execPrice,
			Value:           notional,
			TradeType:       "perps",
			Details:         fmt.Sprintf("Open short %.6f @ $%.2f (%s, fee $%.2f)", qty, execPrice, leverageLabel, fee),
			ExchangeOrderID: openOID,
			ExchangeFee:     exchangeFeeForTrade(fillFee, useFillFee),
		}
		trade.Regime = s.Regime
		RecordTrade(s, trade)
		logger.Info("SELL %s: %.6f @ $%.2f (%s, notional $%.2f, fee $%.2f) [open short]", symbol, qty, execPrice, leverageLabel, notional, fee)
		tradesExecuted++
	}
	return tradesExecuted, nil
}

func perpsLeverageLabel(exchangeLeverage, sizingLeverage float64) string {
	if exchangeLeverage == sizingLeverage {
		return fmt.Sprintf("%.1fx", exchangeLeverage)
	}
	return fmt.Sprintf("%.1fx exchange, %.1fx sizing", exchangeLeverage, sizingLeverage)
}

// ExecuteSpotSignal processes a spot signal and executes paper or live trades.
// fillQty > 0 means a live fill: use price as-is (no slippage) and fillQty as position quantity for buys.
// fillQty == 0 means paper mode: apply ApplySlippage and compute qty from state budget.
func ExecuteSpotSignal(s *StrategyState, signal int, symbol string, price float64, fillQty float64, logger *StrategyLogger) (int, error) {
	return ExecuteSpotSignalWithFillFee(s, signal, symbol, price, fillQty, 0, "", 0, logger)
}

// ExecuteSpotSignalWithFillFee processes a spot signal with optional live
// fill metadata. closeFraction (#519) is the partial-close fraction emitted by
// the open/close registry: when 0 < frac < 1 on a close-side signal the close
// leg reduces pos.Quantity (paper) or uses fillQty (live) without deleting
// the position. closeFraction == 0 preserves the legacy full-close semantics.
func ExecuteSpotSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, fillQty float64, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (int, error) {
	if signal == 0 {
		return 0, nil
	}
	tradesExecuted := 0
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}
	fillMetadataUsed := false
	partialClose := closeFraction > 0 && closeFraction < 1

	if signal == 1 { // Buy
		// Check if already long
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			logger.Info("Already long %s (qty=%.6f), skipping buy", symbol, pos.Quantity)
			return 0, nil
		}
		// Close short if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" {
			closeQty := pos.Quantity
			if partialClose {
				if fillQty > 0 {
					closeQty = fillQty
				} else {
					closeQty = pos.Quantity * closeFraction
				}
			}
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			buyCost := closeQty * execPrice
			useFillMetadata := fillQty > 0 && !fillMetadataUsed
			fee := executionFee(CalculatePlatformSpotFee(feePlatform, buyCost), fillFee, useFillMetadata)
			if useFillMetadata {
				fillMetadataUsed = true
			}
			totalCost := buyCost + fee
			pnl := closeQty*pos.AvgCost - totalCost
			s.Cash += closeQty*pos.AvgCost - totalCost
			now := time.Now().UTC()
			positionID := ensurePositionTradeID(s.ID, symbol, pos)
			details := fmt.Sprintf("Close short, PnL: $%.2f (fee $%.2f)", pnl, fee)
			if partialClose {
				details = fmt.Sprintf("Partial-close short %.6f, PnL: $%.2f (fee $%.2f)", closeQty, pnl, fee)
			}
			trade := Trade{
				Timestamp:       now,
				StrategyID:      s.ID,
				Symbol:          symbol,
				PositionID:      positionID,
				Side:            "buy",
				Quantity:        closeQty,
				Price:           execPrice,
				Value:           totalCost,
				TradeType:       "spot",
				Details:         details,
				ExchangeOrderID: exchangeOrderIDForTrade(fillOID, useFillMetadata),
				ExchangeFee:     exchangeFeeForTrade(fillFee, useFillMetadata),
				IsClose:         true,
				RealizedPnL:     pnl,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			if partialClose {
				pos.Quantity -= closeQty
				logger.Info("Partial-close short %s: %.6f (remaining %.6f) @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, closeQty, pos.Quantity, execPrice, fee, pnl)
			} else {
				recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
				delete(s.Positions, symbol)
				logger.Info("Closed short %s @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, execPrice, fee, pnl)
			}
			tradesExecuted++
		}
		// Spot has no flip semantics: a partial close on a short does not
		// open a long in the same cycle. Stop here when this signal is a
		// close-action emitted by the open/close registry (#519).
		if closeFraction > 0 {
			return tradesExecuted, nil
		}
		// Open long — deploy full cash (paper) or exact fill qty (live). The
		// hardcoded 0.95 safety buffer was removed in #518; spot has no
		// margin to leave headroom for, and operators who want a buffer can
		// reserve cash externally.
		budget := s.Cash
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
		useFillMetadata := fillQty > 0 && !fillMetadataUsed
		fee := executionFee(CalculatePlatformSpotFee(feePlatform, tradeCost), fillFee, useFillMetadata)
		if useFillMetadata {
			fillMetadataUsed = true
		}
		s.Cash -= tradeCost + fee
		now := time.Now().UTC()
		positionID := newTradePositionID(s.ID, symbol, now)
		s.Positions[symbol] = &Position{
			Symbol:          symbol,
			TradePositionID: positionID,
			Quantity:        qty,
			InitialQuantity: qty,
			AvgCost:         execPrice,
			Side:            "long",
			OwnerStrategyID: s.ID,
			OpenedAt:        now,
		}
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          symbol,
			PositionID:      positionID,
			Side:            "buy",
			Quantity:        qty,
			Price:           execPrice,
			Value:           tradeCost + fee,
			TradeType:       "spot",
			Details:         fmt.Sprintf("Open long %.6f @ $%.2f (fee $%.2f)", qty, execPrice, fee),
			ExchangeOrderID: exchangeOrderIDForTrade(fillOID, useFillMetadata),
			ExchangeFee:     exchangeFeeForTrade(fillFee, useFillMetadata),
		}
		trade.Regime = s.Regime
		RecordTrade(s, trade)
		logger.Info("BUY %s: %.6f @ $%.2f (fee $%.2f, total $%.2f)", symbol, qty, execPrice, fee, tradeCost+fee)
		tradesExecuted++

	} else if signal == -1 { // Sell
		// Close long if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			closeQty := pos.Quantity
			if partialClose {
				if fillQty > 0 {
					closeQty = fillQty
				} else {
					closeQty = pos.Quantity * closeFraction
				}
			}
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			saleValue := closeQty * execPrice
			useFillMetadata := fillQty > 0 && !fillMetadataUsed
			fee := executionFee(CalculatePlatformSpotFee(feePlatform, saleValue), fillFee, useFillMetadata)
			if useFillMetadata {
				fillMetadataUsed = true
			}
			netProceeds := saleValue - fee
			pnl := netProceeds - (closeQty * pos.AvgCost)
			s.Cash += netProceeds
			now := time.Now().UTC()
			positionID := ensurePositionTradeID(s.ID, symbol, pos)
			details := fmt.Sprintf("Close long, PnL: $%.2f (fee $%.2f)", pnl, fee)
			if partialClose {
				details = fmt.Sprintf("Partial-close long %.6f, PnL: $%.2f (fee $%.2f)", closeQty, pnl, fee)
			}
			trade := Trade{
				Timestamp:       now,
				StrategyID:      s.ID,
				Symbol:          symbol,
				PositionID:      positionID,
				Side:            "sell",
				Quantity:        closeQty,
				Price:           execPrice,
				Value:           netProceeds,
				TradeType:       "spot",
				Details:         details,
				ExchangeOrderID: exchangeOrderIDForTrade(fillOID, useFillMetadata),
				ExchangeFee:     exchangeFeeForTrade(fillFee, useFillMetadata),
				IsClose:         true,
				RealizedPnL:     pnl,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			if partialClose {
				pos.Quantity -= closeQty
				logger.Info("Partial-close long %s: %.6f (remaining %.6f) @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, closeQty, pos.Quantity, execPrice, fee, pnl)
			} else {
				recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
				delete(s.Positions, symbol)
				logger.Info("SELL %s: %.6f @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, closeQty, execPrice, fee, pnl)
			}
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
	return ExecuteFuturesSignalWithFillFee(s, signal, symbol, price, spec, feePerContract, maxContracts, fillContracts, 0, "", 0, logger)
}

// ExecuteFuturesSignalWithFillFee processes a futures signal with optional
// live fill metadata. closeFraction (#519) is the partial-close fraction
// emitted by the open/close registry; whole-contract sizing rounds the close
// leg DOWN to ensure the residual position has at least one contract
// remaining (a tier returning a fraction smaller than 1 contract is a no-op
// rather than a full close).
func ExecuteFuturesSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, spec ContractSpec, feePerContract float64, maxContracts int, fillContracts int, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (int, error) {
	if signal == 0 {
		return 0, nil
	}
	tradesExecuted := 0
	multiplier := spec.Multiplier
	fillMetadataUsed := false
	partialClose := closeFraction > 0 && closeFraction < 1

	if signal == 1 { // Buy
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			logger.Info("Already long %s (%d contracts), skipping buy", symbol, int(pos.Quantity))
			return 0, nil
		}
		// Close short if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" {
			contracts := int(pos.Quantity)
			if partialClose {
				if fillContracts > 0 {
					contracts = fillContracts
				} else {
					contracts = int(float64(int(pos.Quantity)) * closeFraction)
				}
				if contracts < 1 {
					logger.Info("Partial-close fraction %.4f rounds to 0 contracts for %s; skipping", closeFraction, symbol)
					return tradesExecuted, nil
				}
				if contracts >= int(pos.Quantity) {
					// Round-up edge case (e.g. fraction=0.99 of 1 contract):
					// degrade to a full close rather than over-closing.
					partialClose = false
					contracts = int(pos.Quantity)
				}
			}
			var execPrice float64
			if fillContracts > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			pnl := float64(contracts) * multiplier * (pos.AvgCost - execPrice)
			useFillMetadata := fillContracts > 0 && !fillMetadataUsed
			fee := executionFee(CalculateFuturesFee(contracts, feePerContract), fillFee, useFillMetadata)
			if useFillMetadata {
				fillMetadataUsed = true
			}
			pnl -= fee
			s.Cash += pnl
			now := time.Now().UTC()
			positionID := ensurePositionTradeID(s.ID, symbol, pos)
			details := fmt.Sprintf("Close short %d contracts, PnL: $%.2f (fee $%.2f)", contracts, pnl, fee)
			if partialClose {
				details = fmt.Sprintf("Partial-close short %d contracts, PnL: $%.2f (fee $%.2f)", contracts, pnl, fee)
			}
			trade := Trade{
				Timestamp:       now,
				StrategyID:      s.ID,
				Symbol:          symbol,
				PositionID:      positionID,
				Side:            "buy",
				Quantity:        float64(contracts),
				Price:           execPrice,
				Value:           float64(contracts) * multiplier * execPrice,
				TradeType:       "futures",
				Details:         details,
				ExchangeOrderID: exchangeOrderIDForTrade(fillOID, useFillMetadata),
				ExchangeFee:     exchangeFeeForTrade(fillFee, useFillMetadata),
				IsClose:         true,
				RealizedPnL:     pnl,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			if partialClose {
				pos.Quantity -= float64(contracts)
				logger.Info("Partial-close short %s %d contracts (remaining %d) @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, contracts, int(pos.Quantity), execPrice, fee, pnl)
			} else {
				recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
				delete(s.Positions, symbol)
				logger.Info("Closed short %s %d contracts @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, contracts, execPrice, fee, pnl)
			}
			tradesExecuted++
		}
		// Close-action from the open/close registry (#519): a partial-close
		// signal does not flip into a fresh long.
		if closeFraction > 0 {
			return tradesExecuted, nil
		}
		// Open long — whole contracts only. The 0.95 buffer was removed in
		// #518; futures size in whole contracts so the 5% buffer often had no
		// effect anyway, and operators wanting headroom can set max_contracts.
		budget := s.Cash
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
		useFillMetadata := fillContracts > 0 && !fillMetadataUsed
		fee := executionFee(CalculateFuturesFee(contracts, feePerContract), fillFee, useFillMetadata)
		if useFillMetadata {
			fillMetadataUsed = true
		}
		s.Cash -= fee // futures use margin, not full notional; deduct fee only
		now := time.Now().UTC()
		positionID := newTradePositionID(s.ID, symbol, now)
		s.Positions[symbol] = &Position{
			Symbol:          symbol,
			TradePositionID: positionID,
			Quantity:        float64(contracts),
			InitialQuantity: float64(contracts),
			AvgCost:         execPrice,
			Side:            "long",
			Multiplier:      multiplier,
			OwnerStrategyID: s.ID,
			OpenedAt:        now,
		}
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          symbol,
			PositionID:      positionID,
			Side:            "buy",
			Quantity:        float64(contracts),
			Price:           execPrice,
			Value:           float64(contracts) * marginPerContract,
			TradeType:       "futures",
			Details:         fmt.Sprintf("Open long %d contracts @ $%.2f (fee $%.2f)", contracts, execPrice, fee),
			ExchangeOrderID: exchangeOrderIDForTrade(fillOID, useFillMetadata),
			ExchangeFee:     exchangeFeeForTrade(fillFee, useFillMetadata),
		}
		trade.Regime = s.Regime
		RecordTrade(s, trade)
		logger.Info("BUY %s: %d contracts @ $%.2f (fee $%.2f)", symbol, contracts, execPrice, fee)
		tradesExecuted++

	} else if signal == -1 { // Sell
		// Close long if exists
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			contracts := int(pos.Quantity)
			if partialClose {
				if fillContracts > 0 {
					contracts = fillContracts
				} else {
					contracts = int(float64(int(pos.Quantity)) * closeFraction)
				}
				if contracts < 1 {
					logger.Info("Partial-close fraction %.4f rounds to 0 contracts for %s; skipping", closeFraction, symbol)
					return tradesExecuted, nil
				}
				if contracts >= int(pos.Quantity) {
					partialClose = false
					contracts = int(pos.Quantity)
				}
			}
			var execPrice float64
			if fillContracts > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			pnl := float64(contracts) * multiplier * (execPrice - pos.AvgCost)
			useFillMetadata := fillContracts > 0 && !fillMetadataUsed
			fee := executionFee(CalculateFuturesFee(contracts, feePerContract), fillFee, useFillMetadata)
			if useFillMetadata {
				fillMetadataUsed = true
			}
			pnl -= fee
			s.Cash += pnl
			now := time.Now().UTC()
			positionID := ensurePositionTradeID(s.ID, symbol, pos)
			details := fmt.Sprintf("Close long %d contracts, PnL: $%.2f (fee $%.2f)", contracts, pnl, fee)
			if partialClose {
				details = fmt.Sprintf("Partial-close long %d contracts, PnL: $%.2f (fee $%.2f)", contracts, pnl, fee)
			}
			trade := Trade{
				Timestamp:       now,
				StrategyID:      s.ID,
				Symbol:          symbol,
				PositionID:      positionID,
				Side:            "sell",
				Quantity:        float64(contracts),
				Price:           execPrice,
				Value:           float64(contracts) * multiplier * execPrice,
				TradeType:       "futures",
				Details:         details,
				ExchangeOrderID: exchangeOrderIDForTrade(fillOID, useFillMetadata),
				ExchangeFee:     exchangeFeeForTrade(fillFee, useFillMetadata),
				IsClose:         true,
				RealizedPnL:     pnl,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			if partialClose {
				pos.Quantity -= float64(contracts)
				logger.Info("Partial-close long %s %d contracts (remaining %d) @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, contracts, int(pos.Quantity), execPrice, fee, pnl)
			} else {
				recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
				delete(s.Positions, symbol)
				logger.Info("SELL %s: %d contracts @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, contracts, execPrice, fee, pnl)
			}
			tradesExecuted++
		}
		// Close-action from the open/close registry (#519): partial close
		// does not flip into a fresh short.
		if closeFraction > 0 {
			return tradesExecuted, nil
		}
		// Open short if no long was closed or after closing long
		if _, exists := s.Positions[symbol]; !exists {
			budget := s.Cash
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
			useFillMetadata := fillContracts > 0 && !fillMetadataUsed
			fee := executionFee(CalculateFuturesFee(contracts, feePerContract), fillFee, useFillMetadata)
			if useFillMetadata {
				fillMetadataUsed = true
			}
			s.Cash -= fee
			now := time.Now().UTC()
			positionID := newTradePositionID(s.ID, symbol, now)
			s.Positions[symbol] = &Position{
				Symbol:          symbol,
				TradePositionID: positionID,
				Quantity:        float64(contracts),
				InitialQuantity: float64(contracts),
				AvgCost:         execPrice,
				Side:            "short",
				Multiplier:      multiplier,
				OwnerStrategyID: s.ID,
				OpenedAt:        now,
			}
			trade := Trade{
				Timestamp:       now,
				StrategyID:      s.ID,
				Symbol:          symbol,
				PositionID:      positionID,
				Side:            "sell",
				Quantity:        float64(contracts),
				Price:           execPrice,
				Value:           float64(contracts) * marginPerContract,
				TradeType:       "futures",
				Details:         fmt.Sprintf("Open short %d contracts @ $%.2f (fee $%.2f)", contracts, execPrice, fee),
				ExchangeOrderID: exchangeOrderIDForTrade(fillOID, useFillMetadata),
				ExchangeFee:     exchangeFeeForTrade(fillFee, useFillMetadata),
			}
			trade.Regime = s.Regime
			RecordTrade(s, trade)
			logger.Info("SHORT %s: %d contracts @ $%.2f (fee $%.2f)", symbol, contracts, execPrice, fee)
			tradesExecuted++
		}
	}
	return tradesExecuted, nil
}

// stampOpenTradeFromPosition backfills EntryATR and StopLossTriggerPx onto the
// most recent open Trade for symbol after those values are stamped onto the
// Position post-RecordTrade. Only updates fields that are currently zero so
// subsequent calls are idempotent. Updates both the in-memory slice and the
// SQLite row when db is non-nil.
func stampOpenTradeFromPosition(s *StrategyState, db *StateDB, symbol string, pos *Position) {
	if pos == nil {
		return
	}
	for i := len(s.TradeHistory) - 1; i >= 0; i-- {
		t := &s.TradeHistory[i]
		if t.Symbol != symbol {
			continue
		}
		if t.IsClose {
			return // hit a close first — no open to backfill
		}
		changed := false
		if pos.EntryATR > 0 && t.EntryATR == 0 {
			t.EntryATR = pos.EntryATR
			changed = true
		}
		if pos.StopLossTriggerPx > 0 && t.StopLossTriggerPx == 0 {
			t.StopLossTriggerPx = pos.StopLossTriggerPx
			changed = true
		}
		if changed && db != nil {
			_ = db.UpdateTradeStampedFields(s.ID, t.Timestamp, t.EntryATR, t.StopLossTriggerPx)
		}
		return
	}
}
