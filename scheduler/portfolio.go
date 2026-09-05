package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Position struct {
	Symbol                          string            `json:"symbol"`
	TradePositionID                 string            `json:"position_id,omitempty"`
	Quantity                        float64           `json:"quantity"`
	InitialQuantity                 float64           `json:"initial_quantity,omitempty"`
	AvgCost                         float64           `json:"avg_cost"`
	EntryATR                        float64           `json:"entry_atr,omitempty"`
	Side                            string            `json:"side"`
	Multiplier                      float64           `json:"multiplier,omitempty"`
	Leverage                        float64           `json:"leverage,omitempty"`
	OwnerStrategyID                 string            `json:"owner_strategy_id,omitempty"`
	OpenedAt                        time.Time         `json:"opened_at,omitempty"`
	StopLossOID                     int64             `json:"stop_loss_oid,omitempty"`
	StopLossTriggerPx               float64           `json:"stop_loss_trigger_px,omitempty"`
	StopLossHighWaterPx             float64           `json:"stop_loss_high_water_px,omitempty"`
	TPOIDs                          []int64           `json:"tp_oids,omitempty"`
	TPArmedTiers                    []bool            `json:"tp_armed_tiers,omitempty"`
	StopLossATRMult                 *float64          `json:"stop_loss_atr_mult,omitempty"`
	TPTiersJSON                     string            `json:"tp_tiers_json,omitempty"`
	Regime                          string            `json:"regime,omitempty"`
	RegimeWindows                   map[string]string `json:"regime_windows,omitempty"`
	OpenProfile                     string            `json:"open_profile,omitempty"`
	DirectionCertifiedAtOpen        bool              `json:"direction_certified_at_open,omitempty"`
	DirectionCertifiedStatesAtOpen  map[string]string `json:"direction_certified_states_at_open,omitempty"`
	RegimePendingLabel              string            `json:"regime_pending_label,omitempty"`
	RegimePendingCount              int               `json:"regime_pending_count,omitempty"`
	RegimeAppliedLabel              string            `json:"regime_applied_label,omitempty"`
	SLAdjustedTiersProcessed        int               `json:"sl_adjusted_tiers_processed,omitempty"`
	PostTPTrailingATRMult           *float64          `json:"post_tp_trailing_atr_mult,omitempty"`
	ScaleInCount                    int               `json:"scale_in_count,omitempty"`
	LastAddPrice                    float64           `json:"last_add_price,omitempty"`
	AddedNotionalUSD                float64           `json:"added_notional_usd,omitempty"`
	RiskAnchorPrice                 float64           `json:"risk_anchor_price,omitempty"`
	ScaleInResizePending            bool              `json:"-"`
	RatchetFallbackNormalizePending bool              `json:"-"`
	LLMAnalysisRequested            bool              `json:"llm_analysis_requested,omitempty"`
	LLMVerdict                      string            `json:"llm_verdict,omitempty"`
	ATRMethodAtOpen                 string            `json:"atr_method_at_open,omitempty"`
	HurstAtOpen                     float64           `json:"hurst_at_open,omitempty"`
	HurstSizeMult                   float64           `json:"hurst_size_mult,omitempty"`
	HedgeFor                        string            `json:"hedge_for,omitempty"`
	HedgePrimaryQtyBasis            float64           `json:"hedge_primary_qty_basis,omitempty"`
}

func (p *Position) isHedgeLeg() bool {
	return p != nil && p.HedgeFor != ""
}

func (p *Position) riskAnchorPrice() float64 {
	if p.RiskAnchorPrice > 0 {
		return p.RiskAnchorPrice
	}
	return p.AvgCost
}

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

type ClosedOptionPosition struct {
	StrategyID      string    `json:"strategy_id"`
	PositionID      string    `json:"position_id"`
	Underlying      string    `json:"underlying"`
	OptionType      string    `json:"option_type"`
	Strike          float64   `json:"strike"`
	Expiry          string    `json:"expiry"`
	Action          string    `json:"action"`
	Quantity        float64   `json:"quantity"`
	EntryPremiumUSD float64   `json:"entry_premium_usd"`
	ClosePriceUSD   float64   `json:"close_price_usd"`
	RealizedPnL     float64   `json:"realized_pnl"`
	OpenedAt        time.Time `json:"opened_at"`
	ClosedAt        time.Time `json:"closed_at"`
	CloseReason     string    `json:"close_reason"`
	DurationSeconds int64     `json:"duration_seconds"`
}

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
	captureTradeDiagnostics(s, pos, closePrice, realizedPnL, reason, closedAt)
	if pos.Quantity > 0 && !pos.isHedgeLeg() {
		recordReplayDecision(s, ReplayDecisionFullClose, pos.Symbol, pos.Side, pos.Quantity, closePrice, reason, closedAt, 0, "")
	}
}

func closePositionIsCorrupt(pos *Position) bool {
	return pos == nil || pos.Quantity <= 0 || pos.AvgCost <= 0
}

func absQty(q float64) float64 {
	if q < 0 {
		return -q
	}
	return q
}

func bookPerpsClose(s *StrategyState, symbol string, closePx float64, reason, detailsPrefix, logPrefix string, logger *StrategyLogger) bool {
	return bookPerpsCloseWithFillFee(s, symbol, closePx, 0, false, "", reason, detailsPrefix, logPrefix, logger)
}

func bookPerpsCloseWithFillFee(s *StrategyState, symbol string, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason, detailsPrefix, logPrefix string, logger *StrategyLogger) bool {
	if closePx <= 0 {
		return false
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil {
		return false
	}
	if closePositionIsCorrupt(pos) {
		now := time.Now().UTC()
		if logger != nil {
			logger.Warn("%s: refusing to book PnL for corrupt position %s (qty=%.6f avg_cost=%.4f); clearing with zero realized PnL (#1009)", logPrefix, symbol, pos.Quantity, pos.AvgCost)
		}
		positionID := ensurePositionTradeID(s.ID, symbol, pos)
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          symbol,
			PositionID:      positionID,
			Side:            closeTradeSide(pos.Side),
			Quantity:        absQty(pos.Quantity),
			Price:           closePx,
			Value:           0,
			TradeType:       perpsPositionTradeType(pos),
			Details:         fmt.Sprintf("%s (corrupt position qty=%.6f avg_cost=%.4f) — zero PnL booked", detailsPrefix, pos.Quantity, pos.AvgCost),
			IsClose:         true,
			RealizedPnL:     0,
			PnLGross:        true,
			ExchangeOrderID: exchangeOrderIDForTrade(exchangeOrderID, useFillFee),
			FeeSource:       FeeSourceModeled,
		}
		trade.Regime = s.Regime
		RecordTrade(s, trade)
		recordPositionTradeResult(s, pos, 0)
		recordClosedPosition(s, pos, closePx, 0, reason+"_corrupt", now)
		delete(s.Positions, symbol)
		clearHLPerpsPositionAlertThrottles(s, symbol)
		return true
	}
	if useFillFee && exchangeOrderID != "" && strategyHasCloseTradeForOID(s, exchangeOrderID) {
		if logger != nil {
			logger.Warn("%s: close for OID %s already booked — clearing virtual position without a duplicate Trade (#954)", logPrefix, exchangeOrderID)
		}
		recordClosedPosition(s, pos, closePx, 0, reason+"_dup_oid", time.Now().UTC())
		delete(s.Positions, symbol)
		clearHLPerpsPositionAlertThrottles(s, symbol)
		return true
	}

	now := time.Now().UTC()
	qty := pos.Quantity
	avgCost := pos.AvgCost
	side := pos.Side
	var pnl float64
	if side == "long" {
		pnl = qty * (closePx - avgCost)
	} else {
		pnl = qty * (avgCost - closePx)
	}
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}
	fee := CalculatePlatformSpotFee(feePlatform, qty*closePx)
	feeSource := FeeSourceModeled
	if useFillFee {
		fee = fillFee
		feeSource = FeeSourceUserFills
	}
	grossPnL := pnl
	pnl -= fee
	s.Cash += pnl
	positionID := ensurePositionTradeID(s.ID, symbol, pos)

	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          symbol,
		PositionID:      positionID,
		Side:            closeTradeSide(side),
		Quantity:        qty,
		Price:           closePx,
		Value:           qty * closePx,
		TradeType:       perpsPositionTradeType(pos),
		Details:         fmt.Sprintf("%s, PnL: $%.2f (fee $%.2f)", detailsPrefix, pnl, fee),
		IsClose:         true,
		RealizedPnL:     grossPnL,
		PnLGross:        true,
		ExchangeOrderID: exchangeOrderIDForTrade(exchangeOrderID, useFillFee),
		ExchangeFee:     fee,
		FeeSource:       feeSource,
	}
	trade.Regime = s.Regime
	trade.EntryATR = pos.EntryATR
	trade.StopLossTriggerPx = pos.StopLossTriggerPx
	trade.StopLossATRMult = pos.StopLossATRMult
	trade.TPTiersJSON = pos.TPTiersJSON
	RecordTrade(s, trade)
	recordPositionTradeResult(s, pos, pnl)
	recordClosedPosition(s, pos, closePx, pnl, reason, now)
	delete(s.Positions, symbol)
	clearHLPerpsPositionAlertThrottles(s, symbol)
	if logger != nil {
		logger.Warn("%s @ $%.4f, PnL: $%.2f (fee $%.2f)", logPrefix, closePx, pnl, fee)
	}
	return true
}

func bookPerpsPartialCloseWithFillFee(s *StrategyState, symbol string, closeQty, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason, detailsPrefix, logPrefix string, logger *StrategyLogger) bool {
	if closeQty <= 0 || closePx <= 0 {
		return false
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 {
		return false
	}

	now := time.Now().UTC()
	qty := closeQty
	if qty > pos.Quantity {
		if logger != nil {
			logger.Warn("Partial close qty %.6f exceeds virtual position qty %.6f for %s; clamping to position qty", qty, pos.Quantity, symbol)
		} else {
			fmt.Printf("[WARN] partial close qty %.6f exceeds virtual position qty %.6f for %s; clamping to position qty\n", qty, pos.Quantity, symbol)
		}
		qty = pos.Quantity
	}
	avgCost := pos.AvgCost
	side := pos.Side
	var pnl float64
	if side == "long" {
		pnl = qty * (closePx - avgCost)
	} else {
		pnl = qty * (avgCost - closePx)
	}
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}
	fee := CalculatePlatformSpotFee(feePlatform, qty*closePx)
	feeSource := FeeSourceModeled
	if useFillFee {
		fee = fillFee
		feeSource = FeeSourceUserFills
	}
	grossPnL := pnl
	pnl -= fee
	s.Cash += pnl
	positionID := ensurePositionTradeID(s.ID, symbol, pos)

	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          symbol,
		PositionID:      positionID,
		Side:            closeTradeSide(side),
		Quantity:        qty,
		Price:           closePx,
		Value:           qty * closePx,
		TradeType:       perpsPositionTradeType(pos),
		Details:         fmt.Sprintf("%s %.6f, PnL: $%.2f (fee $%.2f)", detailsPrefix, qty, pnl, fee),
		IsClose:         true,
		RealizedPnL:     grossPnL,
		PnLGross:        true,
		ExchangeOrderID: exchangeOrderIDForTrade(exchangeOrderID, useFillFee),
		ExchangeFee:     fee,
		FeeSource:       feeSource,
	}
	trade.Regime = s.Regime
	trade.EntryATR = pos.EntryATR
	trade.StopLossTriggerPx = pos.StopLossTriggerPx
	trade.StopLossATRMult = pos.StopLossATRMult
	trade.TPTiersJSON = pos.TPTiersJSON
	RecordTrade(s, trade)
	recordPositionTradeResult(s, pos, pnl)

	remaining := pos.Quantity - qty
	if remaining <= 1e-9 {
		recordClosedPosition(s, pos, closePx, pnl, reason, now)
		delete(s.Positions, symbol)
		clearHLPerpsPositionAlertThrottles(s, symbol)
	} else {
		pos.Quantity = remaining
		if !pos.isHedgeLeg() {
			recordReplayDecision(s, ReplayDecisionPartialClose, symbol, side, qty, closePx, reason, now, 0, "")
		}
	}
	if logger != nil {
		remainingForLog := remaining
		if remainingForLog < 0 {
			remainingForLog = 0
		}
		logger.Warn("%s %.6f @ $%.4f, remaining %.6f, PnL: $%.2f (fee $%.2f)", logPrefix, qty, closePx, remainingForLog, pnl, fee)
	}
	return true
}

func stopLossCloseDetailsPrefix(reason string) string {
	switch reason {
	case "trailing_stop_loss_paper":
		return "Paper trailing SL close"
	case "trailing_stop_loss_immediate":
		return "Trailing SL close"
	case "liquidation_clamp_sl_immediate":
		return "Liquidation-clamp SL close"
	case "stop_loss_atr_paper":
		return "Paper SL close"
	case "replay_live_mirror":
		return "Live mirror replay close"
	}
	return "Stop loss close"
}

func recordPerpsStopLossClose(s *StrategyState, symbol string, triggerPx float64, reason string, logger *StrategyLogger) bool {
	return bookPerpsClose(s, symbol, triggerPx, reason, stopLossCloseDetailsPrefix(reason), "SL close reconciled", logger)
}

func recordPerpsStopLossCloseQty(s *StrategyState, symbol string, fillQty, triggerPx float64, reason string, logger *StrategyLogger) bool {
	if pos, ok := s.Positions[symbol]; fillQty > 0 && ok && pos != nil && pos.Quantity > 0 && fillQty < pos.Quantity-1e-9 {
		return bookPerpsPartialCloseWithFillFee(s, symbol, fillQty, triggerPx, 0, false, "", reason, stopLossCloseDetailsPrefix(reason), "SL close reconciled", logger)
	}
	return recordPerpsStopLossClose(s, symbol, triggerPx, reason, logger)
}

func recordPerpsStopLossCloseWithFillFee(s *StrategyState, symbol string, triggerPx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, logger *StrategyLogger) bool {
	return bookPerpsCloseWithFillFee(s, symbol, triggerPx, fillFee, useFillFee, exchangeOrderID, reason, stopLossCloseDetailsPrefix(reason), "SL close reconciled", logger)
}

func recordPerpsExternalCloseWithFillFee(s *StrategyState, symbol string, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, logger *StrategyLogger) bool {
	return bookPerpsCloseWithFillFee(s, symbol, closePx, fillFee, useFillFee, exchangeOrderID, reason, "External close @ mark", "External close reconciled", logger)
}

func recordPerpsExternalPartialCloseWithFillFee(s *StrategyState, symbol string, closeQty, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, logger *StrategyLogger) bool {
	return bookPerpsPartialCloseWithFillFee(s, symbol, closeQty, closePx, fillFee, useFillFee, exchangeOrderID, reason, "External partial close @ mark", "External partial close reconciled", logger)
}

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

type Trade struct {
	Timestamp       time.Time `json:"timestamp"`
	StrategyID      string    `json:"strategy_id"`
	Symbol          string    `json:"symbol"`
	Side            string    `json:"side"`
	Quantity        float64   `json:"quantity"`
	Price           float64   `json:"price"`
	Value           float64   `json:"value"`
	TradeType       string    `json:"trade_type"`
	Details         string    `json:"details"`
	PositionID      string    `json:"position_id"`
	ExchangeOrderID string    `json:"exchange_order_id,omitempty"`
	ExchangeFee     float64   `json:"exchange_fee,omitempty"`

	IsClose     bool    `json:"is_close,omitempty"`
	RealizedPnL float64 `json:"realized_pnl,omitempty"`

	PnLGross  bool   `json:"pnl_gross,omitempty"`
	FeeSource string `json:"fee_source,omitempty"`

	Regime               string `json:"regime,omitempty"`
	RegimeDivergenceNote string `json:"-"`
	RegimeProfileNote    string `json:"-"`

	EntryATR          float64 `json:"entry_atr,omitempty"`
	StopLossOID       int64   `json:"stop_loss_oid,omitempty"`
	StopLossTriggerPx float64 `json:"stop_loss_trigger_px,omitempty"`
	TPOIDs            []int64 `json:"tp_oids,omitempty"`
	Manual            bool    `json:"manual,omitempty"`

	StopLossATRMult *float64 `json:"stop_loss_atr_mult,omitempty"`
	TPTiersJSON     string   `json:"tp_tiers_json,omitempty"`

	// SourceScope and sourceRowID carry the provenance a combined read needs to
	// order rows from two files deterministically. sourceRowID is the row
	// identifier inside its own file, so it is only a tie-break within a scope.
	SourceScope PortfolioScope `json:"source_scope,omitempty"`
	sourceRole  storageRole
	sourceRowID int64

	persisted bool
}

type SignalExecutionResult struct {
	TradesExecuted        int
	OpenTrade             *Trade
	CashReconcileRequired bool
	CashOverBudgetAlert   string
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

func executionFeeSource(fillFee float64, useFillFee bool) string {
	if useFillFee && fillFee > 0 {
		return FeeSourceUserFills
	}
	return FeeSourceModeled
}

func flipFeeShare(fillFee, legQty, fillQty float64) float64 {
	if fillQty <= 0 || legQty <= 0 {
		return fillFee
	}
	share := legQty / fillQty
	if share > 1 {
		share = 1
	}
	return fillFee * share
}

func exchangeOrderIDForTrade(fillOID string, useFillMetadata bool) string {
	if useFillMetadata {
		return fillOID
	}
	return ""
}

func strategyHasCloseTradeForOID(s *StrategyState, exchangeOrderID string) bool {
	if s == nil || exchangeOrderID == "" {
		return false
	}
	for i := len(s.TradeHistory) - 1; i >= 0; i-- {
		t := &s.TradeHistory[i]
		if !t.IsClose {
			continue
		}
		if t.ExchangeOrderID == exchangeOrderID || tradeHasModelOnlySliceOID(t, exchangeOrderID) {
			return true
		}
	}
	return false
}

func formatStatusLine(cash float64, posCount int, value float64, trades int, regime string) string {
	if regime == "" {
		regime = "-"
	}
	return fmt.Sprintf("Status: cash=$%.2f | positions=%d | value=$%.2f | trades=%d | regime=%s",
		cash, posCount, value, trades, regime)
}

func PortfolioValue(s *StrategyState, prices map[string]float64) float64 {
	total := s.Cash
	for sym, pos := range s.Positions {
		price, ok := prices[sym]
		if !ok {
			price = pos.AvgCost
		}
		if pos.Multiplier > 0 {
			if pos.Side == "long" {
				total += pos.Quantity * pos.Multiplier * (price - pos.AvgCost)
			} else {
				total += pos.Quantity * pos.Multiplier * (pos.AvgCost - price)
			}
		} else if pos.Side == "long" {
			total += pos.Quantity * price
		} else {
			total += pos.Quantity * (2*pos.AvgCost - price)
		}
	}
	for _, opt := range s.OptionPositions {
		total += opt.CurrentValueUSD
	}
	return total
}

func PerpsOrderSkipReason(signal int, posSide, direction string) string {
	switch direction {
	case DirectionLong, "":
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
	case DirectionShort:
		switch signal {
		case 1:
			if posSide != "short" {
				return "no short position to buy-cover, skipping"
			}
		case -1:
			if posSide == "short" {
				return "already short, skipping sell"
			}
			if posSide == "long" {
				return "orphan long under direction=\"short\", skipping (state-config gap)"
			}
		}
	case DirectionBoth:
		switch signal {
		case 1:
			if posSide == "long" {
				return "already long, skipping buy"
			}
		case -1:
			if posSide == "short" {
				return "already short, skipping sell"
			}
		}
	}
	return ""
}

func perpsLiveOrderSize(signal int, price, cash, posQty, avgCost float64, sizing PerpsSizing, posSide, direction string, closeFraction float64) (size float64, ok bool, reason string) {
	isBuy := signal == 1
	allowsLong := direction == DirectionLong || direction == DirectionBoth || direction == ""
	allowsShort := direction == DirectionShort || direction == DirectionBoth
	flipping := direction == DirectionBoth && posQty > 0 && closeFraction == 0 && ((isBuy && posSide == "short") || (!isBuy && posSide == "long"))
	openingFresh := false
	if isBuy && allowsLong && (posQty <= 0 || (posSide == "short" && direction == DirectionLong)) {
		openingFresh = true
	}
	if !isBuy && allowsShort && posQty <= 0 {
		openingFresh = true
	}

	if openingFresh || flipping {
		if openingFresh && sizing.RiskPerTradePct > 0 && sizing.RiskStopDistance <= 0 {
			return 0, false, fmt.Sprintf("risk_per_trade_pct sizing: %s — refusing open (fail-closed)", sizing.riskUnresolvedLabel())
		}
		effectiveCash := cash
		if flipping {
			if sizing.SharedWalletPool {
				effectiveCash += sizing.ReleasableMarginUSD
			} else {
				var closePnL float64
				if isBuy {
					closePnL = posQty * (avgCost - price)
				} else {
					closePnL = posQty * (price - avgCost)
				}
				effectiveCash += closePnL
			}
		}
		budget := PerpsOpenNotionalSized(effectiveCash, price, sizing)
		if budget < 1 || price <= 0 {
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
	if posQty <= 0 {
		return 0, false, "no position to close"
	}
	if closeFraction > 0 && closeFraction < 1 {
		return posQty * closeFraction, true, ""
	}
	return posQty, true, ""
}

func perpsCloseActionSuppressesNewSL(signal int, posSide string, allowsLong, allowsShort bool, closeFraction float64) bool {
	if signal == -1 && posSide == "long" && !allowsShort {
		return true
	}
	if signal == 1 && posSide == "short" && !allowsLong {
		return true
	}
	if closeFraction == 1.0 {
		return true
	}
	return false
}

const spotLiveCashBudgetTolerance = 0.01

func spotLiveBuyExceedsCash(cash, totalDebit float64) bool {
	return totalDebit > cash+spotLiveCashBudgetTolerance
}

func formatSpotLiveCashOverBudgetAlert(strategyID, symbol string, cashBefore, totalDebit, cashAfter, fillQty, fillPrice, fee float64) string {
	return fmt.Sprintf(
		"**CRITICAL: LIVE SPOT CASH OVER BUDGET** [%s] %s fill %.6f @ $%.4f (fee $%.4f, debit $%.4f) exceeded virtual cash $%.4f → cash now $%.4f. Fill was booked (venue already filled); reconcile virtual cash against the exchange — dropping the fill would leave state behind real holdings (#1394 / #298).",
		strategyID, symbol, fillQty, fillPrice, fee, totalDebit, cashBefore, cashAfter,
	)
}

func notifySpotLiveCashOverBudget(sender ownerDMSender, msg string) {
	if msg == "" || sender == nil || isNilSender(sender) {
		return
	}
	sender.SendOwnerDM(msg)
	if mn, ok := sender.(*MultiNotifier); ok && mn != nil && mn.HasBackends() {
		mn.SendToAllChannels(msg)
	}
}

func maybeClearCashReconcileRequired(s *StrategyState) {
	_ = s
}

func clearCashReconcileRequired(s *StrategyState) bool {
	if s == nil || !s.CashReconcileRequired {
		return false
	}
	s.CashReconcileRequired = false
	return true
}

func clearCashReconcileRequiredForStrategy(state *AppState, id string) (cash float64, cleared bool, err error) {
	if state == nil || state.Strategies == nil {
		return 0, false, fmt.Errorf("unknown strategy ID: %s", id)
	}
	s, ok := state.Strategies[id]
	if !ok || s == nil {
		return 0, false, fmt.Errorf("unknown strategy ID: %s", id)
	}
	cash = s.Cash
	cleared = clearCashReconcileRequired(s)
	return cash, cleared, nil
}

func cashReconcileBlocksLiveBuy(cashReconcileRequired bool, isBuy bool) bool {
	return cashReconcileRequired && isBuy
}

func formatSpotLiveCashReconcileReminder(ids []string, cashByID map[string]float64) string {
	if len(ids) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("**CRITICAL: CASH RECONCILE STILL REQUIRED** — live spot books remain over-budget until virtual cash is restored:\n")
	for _, id := range ids {
		cash := cashByID[id]
		b.WriteString(fmt.Sprintf("- %s cash=$%.4f\n", id, cash))
	}
	b.WriteString("Further live spot buys are held; closes still run. Clear with /go-trader-clear-cash-reconcile after confirming books match the venue — cash ≥ $0.01 alone does not clear.")
	return b.String()
}

type spotCashReconcileReminderTracker struct {
	mu             sync.Mutex
	lastNotifiedAt time.Time
	lastSig        string
}

var globalSpotCashReconcileReminder = &spotCashReconcileReminderTracker{}

func (t *spotCashReconcileReminderTracker) ShouldNotify(sig string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if sig == "" {
		t.lastSig = ""
		return false
	}
	switch {
	case t.lastNotifiedAt.IsZero():
		t.lastNotifiedAt = now
		t.lastSig = sig
		return true
	case sig != t.lastSig:
		if cashReconcileSigHasNewID(sig, t.lastSig) {
			t.lastNotifiedAt = now
			t.lastSig = sig
			return true
		}
		t.lastSig = sig
		return false
	case now.Sub(t.lastNotifiedAt) >= effectiveAlertThrottleInterval():
		t.lastNotifiedAt = now
		return true
	}
	return false
}

func cashReconcileSigHasNewID(sig, lastSig string) bool {
	last := make(map[string]struct{})
	for _, id := range strings.Split(lastSig, ",") {
		if id == "" {
			continue
		}
		last[id] = struct{}{}
	}
	for _, id := range strings.Split(sig, ",") {
		if id == "" {
			continue
		}
		if _, ok := last[id]; !ok {
			return true
		}
	}
	return false
}

func (t *spotCashReconcileReminderTracker) MarkNotified(sig string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if sig == "" {
		return
	}
	t.lastNotifiedAt = now
	t.lastSig = sig
}

func collectCashReconcileRequiredSnapshots(state *AppState) (ids []string, cashByID map[string]float64) {
	cashByID = make(map[string]float64)
	if state == nil {
		return nil, cashByID
	}
	for id, s := range state.Strategies {
		if s == nil || !s.CashReconcileRequired {
			continue
		}
		ids = append(ids, id)
		cashByID[id] = s.Cash
	}
	sort.Strings(ids)
	return ids, cashByID
}

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

func ExecutePerpsSignalWithLeverage(s *StrategyState, signal int, symbol string, price float64, sizing PerpsSizing, fillQty float64, fillOID string, fillFee float64, direction string, closeFraction float64, logger *StrategyLogger) (int, error) {
	return executePerpsSignalWithLeverage(s, signal, symbol, price, sizing, fillQty, fillOID, fillFee, direction, closeFraction, logger, func(trade Trade) {
		RecordTrade(s, trade)
	})
}

func ExecutePerpsSignalWithLeverageDeferredOpen(s *StrategyState, signal int, symbol string, price float64, sizing PerpsSizing, fillQty float64, fillOID string, fillFee float64, direction string, closeFraction float64, logger *StrategyLogger) (SignalExecutionResult, error) {
	var result SignalExecutionResult
	trades, err := executePerpsSignalWithLeverage(s, signal, symbol, price, sizing, fillQty, fillOID, fillFee, direction, closeFraction, logger, func(trade Trade) {
		t := trade
		result.OpenTrade = &t
	})
	result.TradesExecuted = trades
	return result, err
}

func executePerpsSignalWithLeverage(s *StrategyState, signal int, symbol string, price float64, sizing PerpsSizing, fillQty float64, fillOID string, fillFee float64, direction string, closeFraction float64, logger *StrategyLogger, recordOpen func(Trade)) (int, error) {
	if direction == "" {
		direction = DirectionLong
	}
	allowsLong := direction == DirectionLong || direction == DirectionBoth
	allowsShort := direction == DirectionShort || direction == DirectionBoth
	bidirectional := direction == DirectionBoth
	if signal == 0 {
		return 0, nil
	}
	if sizing.SizingLeverage <= 0 {
		sizing.SizingLeverage = 1
	}
	if sizing.ExchangeLeverage <= 0 {
		sizing.ExchangeLeverage = sizing.SizingLeverage
	}
	exchangeLeverage := sizing.ExchangeLeverage
	tradesExecuted := 0
	leverageLabel := perpsSizingLabel(sizing)
	partialClose := closeFraction > 0 && closeFraction < 1
	closeOnlyAction := closeFraction > 0

	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}

	var flipCloseQty float64

	if signal == 1 {
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			if !allowsLong {
				logger.Warn("Orphan long %s under direction=%q (qty=%.6f); skipping buy — close manually if intentional", symbol, direction, pos.Quantity)
			} else {
				logger.Info("Already long %s (qty=%.6f), skipping buy", symbol, pos.Quantity)
			}
			return 0, nil
		}
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" {
			closeQty := pos.Quantity
			if partialClose {
				if fillQty > 0 {
					closeQty = fillQty
				} else {
					closeQty = pos.Quantity * closeFraction
				}
				if closeQty > pos.Quantity {
					closeQty = pos.Quantity
				}
			}
			if bidirectional {
				flipCloseQty = closeQty
			}
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			pnl := closeQty * (pos.AvgCost - execPrice)
			terminalClose := closeOnlyAction || !allowsLong
			useFillFee := flipCloseQty > 0 || terminalClose
			legFillFee := fillFee
			if flipCloseQty > 0 && !terminalClose && fillQty > 0 {
				legFillFee = flipFeeShare(fillFee, closeQty, fillQty)
			}
			fee := executionFee(CalculatePlatformSpotFee(feePlatform, closeQty*execPrice), legFillFee, useFillFee)
			grossPnL := pnl
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
				ExchangeFee:     fee,
				FeeSource:       executionFeeSource(legFillFee, useFillFee),
				IsClose:         true,
				RealizedPnL:     grossPnL,
				PnLGross:        true,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			trade.StopLossATRMult = pos.StopLossATRMult
			trade.TPTiersJSON = pos.TPTiersJSON
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			if partialClose {
				pos.Quantity -= closeQty
				if !pos.isHedgeLeg() {
					recordReplayDecision(s, ReplayDecisionPartialClose, symbol, pos.Side, closeQty, execPrice, "", now, 0, "")
				}
				logger.Info("Partial-close short %s: %.6f (remaining %.6f) @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, closeQty, pos.Quantity, execPrice, fee, pnl)
			} else {
				recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
				delete(s.Positions, symbol)
				clearHLPerpsPositionAlertThrottles(s, symbol)
				logger.Info("Closed short %s @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, execPrice, fee, pnl)
			}
			tradesExecuted++
		}
		if closeOnlyAction {
			return tradesExecuted, nil
		}
		if !allowsLong {
			if tradesExecuted == 0 {
				logger.Info("No short position in %s to buy-cover, skipping (direction=%q)", symbol, direction)
			}
			return tradesExecuted, nil
		}
		if s.Cash < 1 && fillQty <= 0 {
			logger.Info("Insufficient cash ($%.2f) to open long %s perp", s.Cash, symbol)
			return tradesExecuted, nil
		}
		var execPrice, qty float64
		if fillQty > 0 {
			execPrice = price
			qty = fillQty - flipCloseQty
			if qty <= 0 {
				logger.Warn("Flip fill qty (%.6f) did not cover new long after closing short (%.6f); leaving flat", fillQty, flipCloseQty)
				return tradesExecuted, nil
			}
		} else {
			execPrice = ApplySlippage(price)
			if execPrice <= 0 {
				return tradesExecuted, nil
			}
			if sizing.RiskPerTradePct > 0 && sizing.RiskStopDistance <= 0 {
				logger.Info("Risk-per-trade sizing: %s — refusing open long %s (fail-closed)", sizing.riskUnresolvedLabel(), symbol)
				return tradesExecuted, nil
			}
			budget := PerpsOpenNotionalSized(s.Cash, execPrice, sizing)
			qty = budget / execPrice
		}
		notional := qty * execPrice
		useFillFee := flipCloseQty == 0
		legFillFee := fillFee
		if flipCloseQty > 0 && fillQty > 0 && fillFee > 0 {
			useFillFee = true
			legFillFee = flipFeeShare(fillFee, qty, fillQty)
		}
		fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), legFillFee, useFillFee)
		s.Cash -= fee
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
			Side:            "buy",
			Quantity:        qty,
			Price:           execPrice,
			Value:           notional,
			TradeType:       "perps",
			Details:         fmt.Sprintf("Open long %.6f @ $%.2f (%s, fee $%.2f)", qty, execPrice, leverageLabel, fee),
			ExchangeOrderID: openOID,
			ExchangeFee:     fee,
			FeeSource:       executionFeeSource(legFillFee, useFillFee),
			PnLGross:        true,
		}
		trade.Regime = s.Regime
		trade.RegimeDivergenceNote = formatDivergenceDMLine(s.RegimeDivergence)
		trade.RegimeProfileNote = formatProfileDMLine(s.RegimeProfile)
		recordOpen(trade)
		logger.Info("BUY %s: %.6f @ $%.2f (%s, notional $%.2f, fee $%.2f)", symbol, qty, execPrice, leverageLabel, notional, fee)
		tradesExecuted++

	} else if signal == -1 {
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" && allowsShort {
			logger.Info("Already short %s (qty=%.6f), skipping sell", symbol, pos.Quantity)
			return 0, nil
		}
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			if !allowsLong {
				logger.Warn("Orphan long %s under direction=%q (qty=%.6f); leaving in place — close manually if intentional", symbol, direction, pos.Quantity)
				return tradesExecuted, nil
			}
			closeQty := pos.Quantity
			if partialClose {
				if fillQty > 0 {
					closeQty = fillQty
				} else {
					closeQty = pos.Quantity * closeFraction
				}
				if closeQty > pos.Quantity {
					closeQty = pos.Quantity
				}
			}
			if bidirectional {
				flipCloseQty = closeQty
			}
			var execPrice float64
			if fillQty > 0 {
				execPrice = price
			} else {
				execPrice = ApplySlippage(price)
			}
			pnl := closeQty * (execPrice - pos.AvgCost)
			terminalClose := closeOnlyAction || !allowsShort
			useFillFee := flipCloseQty > 0 || terminalClose
			legFillFee := fillFee
			if flipCloseQty > 0 && !terminalClose && fillQty > 0 {
				legFillFee = flipFeeShare(fillFee, closeQty, fillQty)
			}
			fee := executionFee(CalculatePlatformSpotFee(feePlatform, closeQty*execPrice), legFillFee, useFillFee)
			grossPnL := pnl
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
				ExchangeFee:     fee,
				FeeSource:       executionFeeSource(legFillFee, useFillFee),
				IsClose:         true,
				RealizedPnL:     grossPnL,
				PnLGross:        true,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			trade.StopLossATRMult = pos.StopLossATRMult
			trade.TPTiersJSON = pos.TPTiersJSON
			RecordTrade(s, trade)
			RecordTradeResult(&s.RiskState, pnl)
			if partialClose {
				pos.Quantity -= closeQty
				if !pos.isHedgeLeg() {
					recordReplayDecision(s, ReplayDecisionPartialClose, symbol, pos.Side, closeQty, execPrice, "", now, 0, "")
				}
				logger.Info("Partial-close long %s: %.6f (remaining %.6f) @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, closeQty, pos.Quantity, execPrice, fee, pnl)
			} else {
				recordClosedPosition(s, pos, execPrice, pnl, "signal", now)
				delete(s.Positions, symbol)
				clearHLPerpsPositionAlertThrottles(s, symbol)
				logger.Info("SELL %s: %.6f @ $%.2f (fee $%.2f) | PnL: $%.2f", symbol, closeQty, execPrice, fee, pnl)
			}
			tradesExecuted++
		}
		if closeOnlyAction {
			return tradesExecuted, nil
		}
		if !allowsShort {
			if tradesExecuted == 0 {
				logger.Info("No long position in %s to sell, skipping", symbol)
			}
			return tradesExecuted, nil
		}
		if s.Cash < 1 && fillQty <= 0 {
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
			if sizing.RiskPerTradePct > 0 && sizing.RiskStopDistance <= 0 {
				logger.Info("Risk-per-trade sizing: %s — refusing open short %s (fail-closed)", sizing.riskUnresolvedLabel(), symbol)
				return tradesExecuted, nil
			}
			budget := PerpsOpenNotionalSized(s.Cash, execPrice, sizing)
			qty = budget / execPrice
		}
		notional := qty * execPrice
		useFillFee := flipCloseQty == 0
		legFillFee := fillFee
		if flipCloseQty > 0 && fillQty > 0 && fillFee > 0 {
			useFillFee = true
			legFillFee = flipFeeShare(fillFee, qty, fillQty)
		}
		fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), legFillFee, useFillFee)
		s.Cash -= fee
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
			ExchangeFee:     fee,
			FeeSource:       executionFeeSource(legFillFee, useFillFee),
			PnLGross:        true,
		}
		trade.Regime = s.Regime
		trade.RegimeDivergenceNote = formatDivergenceDMLine(s.RegimeDivergence)
		trade.RegimeProfileNote = formatProfileDMLine(s.RegimeProfile)
		recordOpen(trade)
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

func perpsSizingLabel(sizing PerpsSizing) string {
	if sizing.RiskPerTradePct > 0 {
		return fmt.Sprintf("%.1fx exchange, risk %g%%/trade", sizing.ExchangeLeverage, sizing.RiskPerTradePct)
	}
	return perpsLeverageLabel(sizing.ExchangeLeverage, sizing.SizingLeverage)
}

func ExecuteSpotSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, fillQty float64, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (int, error) {
	out, err := executeSpotSignalWithFillFee(s, signal, symbol, price, fillQty, fillFee, fillOID, closeFraction, 1.0, logger, func(trade Trade) {
		RecordTrade(s, trade)
	})
	return out.TradesExecuted, err
}

func ExecuteSpotSignalWithFillFeeDeferredOpen(s *StrategyState, signal int, symbol string, price float64, fillQty float64, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (SignalExecutionResult, error) {
	return ExecuteSpotSignalWithFillFeeSizedDeferredOpen(s, signal, symbol, price, fillQty, fillFee, fillOID, closeFraction, 1.0, logger)
}

func ExecuteSpotSignalWithFillFeeSizedDeferredOpen(s *StrategyState, signal int, symbol string, price float64, fillQty float64, fillFee float64, fillOID string, closeFraction float64, openSizeMult float64, logger *StrategyLogger) (SignalExecutionResult, error) {
	var result SignalExecutionResult
	out, err := executeSpotSignalWithFillFee(s, signal, symbol, price, fillQty, fillFee, fillOID, closeFraction, openSizeMult, logger, func(trade Trade) {
		t := trade
		result.OpenTrade = &t
	})
	result.TradesExecuted = out.TradesExecuted
	result.CashReconcileRequired = out.CashReconcileRequired
	result.CashOverBudgetAlert = out.CashOverBudgetAlert
	return result, err
}

func normalizeOpenSizeMult(m float64) float64 {
	if m <= 0 || m > 1.0 || math.IsNaN(m) {
		return 1.0
	}
	return m
}

type spotSignalExecOutcome struct {
	TradesExecuted        int
	CashReconcileRequired bool
	CashOverBudgetAlert   string
}

func executeSpotSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, fillQty float64, fillFee float64, fillOID string, closeFraction float64, openSizeMult float64, logger *StrategyLogger, recordOpen func(Trade)) (spotSignalExecOutcome, error) {
	if signal == 0 {
		return spotSignalExecOutcome{}, nil
	}
	out := spotSignalExecOutcome{}
	tradesExecuted := 0
	feePlatform := s.Platform
	if s.Platform == "okx" && s.Type == "perps" {
		feePlatform = "okx-perps"
	}
	fillMetadataUsed := false
	partialClose := closeFraction > 0 && closeFraction < 1

	if signal == 1 {
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			logger.Info("Already long %s (qty=%.6f), skipping buy", symbol, pos.Quantity)
			return spotSignalExecOutcome{}, nil
		}
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
			grossPnL := pnl + fee
			s.Cash += closeQty*pos.AvgCost - totalCost
			maybeClearCashReconcileRequired(s)
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
				ExchangeFee:     fee,
				FeeSource:       executionFeeSource(fillFee, useFillMetadata),
				IsClose:         true,
				RealizedPnL:     grossPnL,
				PnLGross:        true,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			trade.StopLossATRMult = pos.StopLossATRMult
			trade.TPTiersJSON = pos.TPTiersJSON
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
		if closeFraction > 0 {
			out.TradesExecuted = tradesExecuted
			return out, nil
		}
		budget := s.Cash
		liveBuy := fillQty > 0
		if !liveBuy {
			budget *= normalizeOpenSizeMult(openSizeMult)
		}
		if !liveBuy && budget < 1 {
			logger.Info("Insufficient cash ($%.2f) to buy %s", s.Cash, symbol)
			out.TradesExecuted = tradesExecuted
			return out, nil
		}
		var execPrice, qty float64
		if liveBuy {
			execPrice = price
			qty = fillQty
		} else {
			execPrice = ApplySlippage(price)
			if execPrice <= 0 {
				out.TradesExecuted = tradesExecuted
				return out, nil
			}
			qty = budget / execPrice
		}
		tradeCost := qty * execPrice
		useFillMetadata := fillQty > 0 && !fillMetadataUsed
		fee := executionFee(CalculatePlatformSpotFee(feePlatform, tradeCost), fillFee, useFillMetadata)
		if useFillMetadata {
			fillMetadataUsed = true
		}
		totalDebit := tradeCost + fee
		cashBefore := s.Cash
		overBudget := liveBuy && spotLiveBuyExceedsCash(cashBefore, totalDebit)
		s.Cash -= totalDebit
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
		details := fmt.Sprintf("Open long %.6f @ $%.2f (fee $%.2f)", qty, execPrice, fee)
		if overBudget {
			s.CashReconcileRequired = true
			out.CashReconcileRequired = true
			out.CashOverBudgetAlert = formatSpotLiveCashOverBudgetAlert(
				s.ID, symbol, cashBefore, totalDebit, s.Cash, qty, execPrice, fee)
			details += " [CASH OVER BUDGET — reconcile required]"
			logger.Error("CRITICAL: live spot buy over virtual cash for %s: debit $%.4f > cash $%.4f (tol $%.2f) → cash now $%.4f — fill booked, reconcile required (#1394)",
				symbol, totalDebit, cashBefore, spotLiveCashBudgetTolerance, s.Cash)
		}
		trade := Trade{
			Timestamp:       now,
			StrategyID:      s.ID,
			Symbol:          symbol,
			PositionID:      positionID,
			Side:            "buy",
			Quantity:        qty,
			Price:           execPrice,
			Value:           totalDebit,
			TradeType:       "spot",
			Details:         details,
			ExchangeOrderID: exchangeOrderIDForTrade(fillOID, useFillMetadata),
			ExchangeFee:     fee,
			FeeSource:       executionFeeSource(fillFee, useFillMetadata),
			PnLGross:        true,
		}
		trade.Regime = s.Regime
		recordOpen(trade)
		logger.Info("BUY %s: %.6f @ $%.2f (fee $%.2f, total $%.2f)", symbol, qty, execPrice, fee, totalDebit)
		tradesExecuted++

	} else if signal == -1 {
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
			grossPnL := pnl + fee
			s.Cash += netProceeds
			maybeClearCashReconcileRequired(s)
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
				ExchangeFee:     fee,
				FeeSource:       executionFeeSource(fillFee, useFillMetadata),
				IsClose:         true,
				RealizedPnL:     grossPnL,
				PnLGross:        true,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			trade.StopLossATRMult = pos.StopLossATRMult
			trade.TPTiersJSON = pos.TPTiersJSON
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
	out.TradesExecuted = tradesExecuted
	return out, nil
}

func ExecuteFuturesSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, spec ContractSpec, feePerContract float64, maxContracts int, fillContracts int, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (int, error) {
	return executeFuturesSignalWithFillFee(s, signal, symbol, price, spec, feePerContract, maxContracts, fillContracts, fillFee, fillOID, closeFraction, 1.0, logger, func(trade Trade) {
		RecordTrade(s, trade)
	})
}

func ExecuteFuturesSignalWithFillFeeDeferredOpen(s *StrategyState, signal int, symbol string, price float64, spec ContractSpec, feePerContract float64, maxContracts int, fillContracts int, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (SignalExecutionResult, error) {
	return ExecuteFuturesSignalWithFillFeeSizedDeferredOpen(s, signal, symbol, price, spec, feePerContract, maxContracts, fillContracts, fillFee, fillOID, closeFraction, 1.0, logger)
}

func ExecuteFuturesSignalWithFillFeeSizedDeferredOpen(s *StrategyState, signal int, symbol string, price float64, spec ContractSpec, feePerContract float64, maxContracts int, fillContracts int, fillFee float64, fillOID string, closeFraction float64, openSizeMult float64, logger *StrategyLogger) (SignalExecutionResult, error) {
	var result SignalExecutionResult
	trades, err := executeFuturesSignalWithFillFee(s, signal, symbol, price, spec, feePerContract, maxContracts, fillContracts, fillFee, fillOID, closeFraction, openSizeMult, logger, func(trade Trade) {
		t := trade
		result.OpenTrade = &t
	})
	result.TradesExecuted = trades
	return result, err
}

func executeFuturesSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, spec ContractSpec, feePerContract float64, maxContracts int, fillContracts int, fillFee float64, fillOID string, closeFraction float64, openSizeMult float64, logger *StrategyLogger, recordOpen func(Trade)) (int, error) {
	if signal == 0 {
		return 0, nil
	}
	tradesExecuted := 0
	multiplier := spec.Multiplier
	fillMetadataUsed := false
	partialClose := closeFraction > 0 && closeFraction < 1

	if signal == 1 {
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			logger.Info("Already long %s (%d contracts), skipping buy", symbol, int(pos.Quantity))
			return 0, nil
		}
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
			grossPnL := pnl
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
				ExchangeFee:     fee,
				FeeSource:       executionFeeSource(fillFee, useFillMetadata),
				IsClose:         true,
				RealizedPnL:     grossPnL,
				PnLGross:        true,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			trade.StopLossATRMult = pos.StopLossATRMult
			trade.TPTiersJSON = pos.TPTiersJSON
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
		if closeFraction > 0 {
			return tradesExecuted, nil
		}
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
			contracts = int(budget * normalizeOpenSizeMult(openSizeMult) / marginPerContract)
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
		s.Cash -= fee
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
			ExchangeFee:     fee,
			FeeSource:       executionFeeSource(fillFee, useFillMetadata),
			PnLGross:        true,
		}
		trade.Regime = s.Regime
		recordOpen(trade)
		logger.Info("BUY %s: %d contracts @ $%.2f (fee $%.2f)", symbol, contracts, execPrice, fee)
		tradesExecuted++

	} else if signal == -1 {
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
			grossPnL := pnl
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
				ExchangeFee:     fee,
				FeeSource:       executionFeeSource(fillFee, useFillMetadata),
				IsClose:         true,
				RealizedPnL:     grossPnL,
				PnLGross:        true,
			}
			trade.Regime = s.Regime
			trade.EntryATR = pos.EntryATR
			trade.StopLossTriggerPx = pos.StopLossTriggerPx
			trade.StopLossATRMult = pos.StopLossATRMult
			trade.TPTiersJSON = pos.TPTiersJSON
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
		if closeFraction > 0 {
			return tradesExecuted, nil
		}
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
				ExchangeFee:     fee,
				FeeSource:       executionFeeSource(fillFee, useFillMetadata),
				PnLGross:        true,
			}
			trade.Regime = s.Regime
			recordOpen(trade)
			logger.Info("SHORT %s: %d contracts @ $%.2f (fee $%.2f)", symbol, contracts, execPrice, fee)
			tradesExecuted++
		}
	}
	return tradesExecuted, nil
}

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
			return
		}
		changed := false
		if pos.EntryATR > 0 && t.EntryATR == 0 {
			t.EntryATR = pos.EntryATR
			changed = true
		}
		if pos.StopLossOID > 0 && t.StopLossOID == 0 {
			t.StopLossOID = pos.StopLossOID
			changed = true
		}
		if pos.StopLossTriggerPx > 0 && t.StopLossTriggerPx == 0 {
			t.StopLossTriggerPx = pos.StopLossTriggerPx
			changed = true
		}
		if len(pos.TPOIDs) > 0 && len(t.TPOIDs) == 0 {
			t.TPOIDs = cloneInt64s(pos.TPOIDs)
			changed = true
		}
		if pos.StopLossATRMult != nil && t.StopLossATRMult == nil {
			v := *pos.StopLossATRMult
			t.StopLossATRMult = &v
			changed = true
		}
		if pos.TPTiersJSON != "" && t.TPTiersJSON == "" {
			t.TPTiersJSON = pos.TPTiersJSON
			changed = true
		}
		if changed && db != nil {
			_ = db.UpdateTradeStampedFields(s.ID, t.Timestamp, t.EntryATR, t.StopLossOID, t.StopLossTriggerPx, t.TPOIDs, t.StopLossATRMult, t.TPTiersJSON)
		}
		return
	}
}
