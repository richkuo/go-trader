package main

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Position represents a spot, futures, or perps position.
type Position struct {
	Symbol                 string    `json:"symbol"`
	TradePositionID        string    `json:"position_id,omitempty"`
	Quantity               float64   `json:"quantity"`
	InitialQuantity        float64   `json:"initial_quantity,omitempty"` // original open size; partial closes must not rewrite it (#496)
	AvgCost                float64   `json:"avg_cost"`
	EntryATR               float64   `json:"entry_atr,omitempty"`                 // ATR value from the entry strategy's open candle when available (#496)
	Side                   string    `json:"side"`                                // "long" or "short"
	Multiplier             float64   `json:"multiplier,omitempty"`                // contract multiplier (0 = spot, >0 = futures/perps PnL branch; canonical perps value is 1 — do NOT set to leverage)
	Leverage               float64   `json:"leverage,omitempty"`                  // perps exchange leverage (informational; PnL is not scaled by leverage) (#254/#497)
	OwnerStrategyID        string    `json:"owner_strategy_id,omitempty"`         // strategy that opened this position
	IsHedge                bool      `json:"is_hedge,omitempty"`                  // #1159: strategy-owned correlated hedge leg
	HedgePrimarySymbol     string    `json:"hedge_primary_symbol,omitempty"`      // #1159: primary symbol whose lifecycle owns this hedge
	HedgePrimaryPositionID string    `json:"hedge_primary_position_id,omitempty"` // #1159: owning primary round-trip identity
	OpenedAt               time.Time `json:"opened_at,omitempty"`                 // when the position was opened
	StopLossOID            int64     `json:"stop_loss_oid,omitempty"`             // HL perps: resting trigger-order OID for the per-trade stop-loss (0 = none) (#412)
	StopLossTriggerPx      float64   `json:"stop_loss_trigger_px,omitempty"`      // HL perps: trigger price for the resting stop-loss (0 = unknown) (#421)
	StopLossHighWaterPx    float64   `json:"stop_loss_high_water_px,omitempty"`   // HL perps trailing SL: best mark seen while position open (high for long, low for short) (#501)
	TPOIDs                 []int64   `json:"tp_oids,omitempty"`                   // HL perps: resting reduce-only TP limit OIDs, one per configured tier (#601/#612)
	// TPArmedTiers[i] = true once tier i has been observed with a positive OID
	// (i.e. successfully placed by runHyperliquidProtectionSync at least once).
	// findHighestClearedTier requires this so a tier whose first placement
	// failed transiently — leaving OID=0 with the tier never armed — is NOT
	// mistaken for a fired TP when a non-TP partial close (close-evaluator)
	// shrinks the position. See #716 item 2.
	TPArmedTiers    []bool            `json:"tp_armed_tiers,omitempty"`
	StopLossATRMult *float64          `json:"stop_loss_atr_mult,omitempty"` // HL perps: ATR multiplier resolved at fill time when SL was ATR-armed; nil = armed via pct/margin/trailing/none (#669)
	TPTiersJSON     string            `json:"tp_tiers_json,omitempty"`      // HL perps: JSON snapshot of [{atr_multiple,close_fraction},...] resolved at fill time; "" = strategy doesn't use tiered_tp_atr* (#669)
	Regime          string            `json:"regime,omitempty"`             // regime label stamped at position open via stampPositionRegimeIfOpened. Drives regime-aware tier/SL multipliers for the life of the position (#733). Distinct from StrategyState.Regime which tracks the most recent classifier output.
	RegimeWindows   map[string]string `json:"regime_windows,omitempty"`     // per-window regime labels stamped at open (#792)
	OpenProfile     string            `json:"open_profile,omitempty"`       // regime-profile allocation: profile active when this position opened, frozen for its life (hold-on-transition). Read by resolveRegimeProfile when a position is open. (#998)
	// DirectionCertifiedAtOpen records whether regime_directional_policy was
	// CERTIFIED (#1085) for this strategy's (asset,timeframe,classifier) at the
	// moment the position opened. The live entry gate keys on the CURRENT
	// certification verdict only when flat; once open, the position rides under
	// this stamp so a later certification expiry/refresh can never silently flip
	// its effective direction or trip the #822 orphan-close. Legacy positions
	// (pre-#1085) default false → resolve to base direction (the intended
	// from-flat migration; #822 auto-closes sole-owner conflicts, shared-coin
	// conflicts are surfaced to the operator).
	DirectionCertifiedAtOpen bool `json:"direction_certified_at_open,omitempty"`
	// DirectionCertifiedStatesAtOpen freezes the certified PER-STATE direction
	// map (#1085) at the moment the position opened. Per-state SIGN gating of an
	// OPEN position (hold-on-transition AND the #822 orphan check) consults this
	// open-time evidence — never the live artifact, which a SIGHUP/expiry could
	// change mid-position (req 2) — so a certified cell never bets opposite the
	// certified sign for a state, and an open position is never re-gated by a later
	// artifact change. nil = cell uncertified at open / no policy → every state
	// resolves to base direction. Persisted as a JSON map column (mirrors
	// regime_windows_json).
	DirectionCertifiedStatesAtOpen map[string]string `json:"direction_certified_states_at_open,omitempty"`
	// #843 dynamic close: confirm-cycle state for live ATR-regime re-resolution.
	RegimePendingLabel string `json:"regime_pending_label,omitempty"`
	RegimePendingCount int    `json:"regime_pending_count,omitempty"`
	RegimeAppliedLabel string `json:"regime_applied_label,omitempty"`
	// SLAdjustedTiersProcessed counts how many leading tiers have already had
	// their sl_after rule applied: 0 = none, N = tiers [0..N-1] processed.
	// Idempotency watermark so restarts don't re-fire the same bump (#708).
	SLAdjustedTiersProcessed int `json:"sl_adjusted_tiers_processed,omitempty"`
	// PostTPTrailingATRMult is set when an sl_after `trail_from_here` rule
	// fires; it tells the trailing-stop walker to take over for the remainder
	// of the position at this ATR distance. nil = no post-TP trailing (#708).
	PostTPTrailingATRMult *float64 `json:"post_tp_trailing_atr_mult,omitempty"`
	// Scale-in / pyramiding state (#873). A scale-in blends price+size into the
	// existing position and FREEZES EntryATR, Regime, and the TP tier geometry —
	// only protection sizing is re-based. ScaleInCount is the number of add legs
	// applied (gated by scale_in.max_adds). LastAddPrice is the fill price of the
	// most recent entry leg (open or add), the watermark for add_spacing_atr; 0 =
	// never stamped (pre-#873 positions fall back to AvgCost). AddedNotionalUSD is
	// the cumulative USD notional added across all add legs (gated by
	// scale_in.max_added_notional_usd). ScaleInResizePending is set by applyScaleIn
	// so the next HL-live protection sync force-replaces SL + un-cleared TP tiers
	// at the grown size; cleared after the sync. It is PERSISTED (positions-table
	// scale_in_resize_pending) so a restart between an add and the deferred
	// trailing-SL re-size still grows the on-chain stop next cycle (#873; the wire
	// JSON omits it). The cleared-tier watermark (SLAdjustedTiersProcessed/
	// TPArmedTiers) is never reset by an add.
	ScaleInCount     int     `json:"scale_in_count,omitempty"`
	LastAddPrice     float64 `json:"last_add_price,omitempty"` // fill price of the most recent ADD leg (stamped only by applyScaleIn; the add_spacing_atr watermark, falls back to AvgCost when 0)
	AddedNotionalUSD float64 `json:"added_notional_usd,omitempty"`
	// RiskAnchorPrice is the AvgCost captured at the FIRST scale-in (= the
	// original entry price, before any blend). On-chain SL/TP triggers are
	// computed from this frozen anchor — not the blended AvgCost — so an add
	// re-sizes protection to the new total at the UNCHANGED trigger geometry
	// (#873 design: freeze the entry; the blended AvgCost drives PnL only).
	// 0 = never scaled in → callers fall back to AvgCost.
	RiskAnchorPrice      float64 `json:"risk_anchor_price,omitempty"`
	ScaleInResizePending bool    `json:"-"`
	// RatchetFallbackNormalizePending is set when manual-open had to arm a
	// trailing_tp_ratchet_regime position with the protective fallback distance
	// because the live regime label was unavailable. The next trailing walker may
	// widen exactly once to the configured regime trail, then clears this flag.
	RatchetFallbackNormalizePending bool `json:"-"`
	// #1137 LLM entry analysis (advisory-only). LLMAnalysisRequested is the
	// idempotency marker set at dispatch — at most one analysis per opened
	// position, surviving restarts. LLMVerdict is the pipeline's read on the
	// entry (bullish/bearish/mixed), stamped when the async job completes and
	// copied into trade_diagnostics.llm_verdict at close; "" = analysis
	// disabled, failed, or not finished before the close.
	LLMAnalysisRequested bool   `json:"llm_analysis_requested,omitempty"`
	LLMVerdict           string `json:"llm_verdict,omitempty"`
	// ATRMethodAtOpen freezes the resolved atr_method (#1277) the moment this
	// position opened (stampATRMethodAtOpenIfOpened; never re-stamped on a
	// scale-in add — mirrors RiskAnchorPrice/EntryATR freeze-at-entry
	// semantics). EntryATR and on-chain reduce-only protection are sized once
	// from the frozen EntryATR and therefore immune to a later atr_method
	// change, but a live-recomputed close evaluator (tiered_tp_atr_live,
	// atr_stop/avwap_stop with atr_source=live) reads market_ctx["atr"] fresh
	// every cycle under whatever method the CURRENT config resolves — the
	// SIGHUP hot-reload guard (config_reload.go) blocks an effective-method
	// flip while open, but a config edit + process restart bypasses it (no
	// "old" resolved value to diff against). checkATRMethodDriftAtStartup
	// compares this stamp to the live resolution once per boot to catch that
	// gap. "" = pre-#1277 position, never stamped (drift check skips it).
	ATRMethodAtOpen string `json:"atr_method_at_open,omitempty"`
}

// riskAnchorPrice returns the price geometry that on-chain SL/TP triggers are
// anchored to: the frozen original entry (RiskAnchorPrice) once a position has
// scaled in, else the current AvgCost (which equals the entry for a position
// that never scaled in). Keeps trigger prices pinned to the first entry across
// adds while the blended AvgCost drives PnL (#873).
func (p *Position) riskAnchorPrice() float64 {
	if p.RiskAnchorPrice > 0 {
		return p.RiskAnchorPrice
	}
	return p.AvgCost
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
// that disappeared from the exchange between reconcile cycles — ClosePrice and
// RealizedPnL are 0 when no mark price was available at reconcile time, and
// approximate (mark-based, not the actual fill price) otherwise. Downstream
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
	// #1147: every full-close path funnels through here, so this is the
	// diagnostics capture choke point. Eager identity insert only — the
	// hold-window OHLCV fetch happens in the async worker, outside mu.
	captureTradeDiagnostics(s, pos, closePrice, realizedPnL, reason, closedAt)
}

// closePositionIsCorrupt reports whether a position's structural fields make a
// qty*(price-avgCost) realized-PnL meaningless (#1009). A non-positive quantity
// (e.g. the negative residual a mis-sized direction reversal used to leave) or a
// non-positive average cost (a zeroed/garbage entry that books the full notional
// as PnL — the ~4884x overstatement folded in from PR #1008) must never feed a
// PnL booking. A force/flatten close on such a position has to clear it with a
// zero-PnL leg rather than inject a phantom realized_pnl that diverges from its
// closed_positions row and drives a persistent shared-wallet drift alert.
func closePositionIsCorrupt(pos *Position) bool {
	return pos == nil || pos.Quantity <= 0 || pos.AvgCost <= 0
}

// absQty returns the magnitude of a (possibly corrupt, possibly negative)
// position quantity for display on a reconciliation/close leg without pulling in
// the math package at these hot call sites.
func absQty(q float64) float64 {
	if q < 0 {
		return -q
	}
	return q
}

// bookPerpsClose is the shared close-booking path for perps: computes PnL at
// closePx, deducts the platform taker fee, credits s.Cash, records a close
// Trade + ClosedPosition, and removes the virtual position. detailsPrefix is
// embedded in Trade.Details ("<prefix>, PnL: $X (fee $Y)") and logPrefix in
// the strategy-logger Warn line ("<prefix> @ $px, PnL: $X (fee $Y)").
// Returns false (no mutation) when closePx <= 0 or the position is missing,
// so callers can choose a fallback path.
func bookPerpsClose(s *StrategyState, symbol string, closePx float64, reason, detailsPrefix, logPrefix string, logger *StrategyLogger) bool {
	return bookPerpsCloseWithFillFee(s, symbol, closePx, 0, false, "", reason, detailsPrefix, logPrefix, logger)
}

// bookPerpsCloseWithFillFee extends bookPerpsClose with on-chain fill metadata.
// When useFillFee is true the supplied fillFee is treated as authoritative —
// it replaces the modeled fee in PnL math AND populates Trade.ExchangeFee
// regardless of sign (zero / negative maker rebate fills are kept verbatim
// rather than silently falling back to the modeled taker fee). useFillFee
// must only be set when an actual userFills row was matched, so this is the
// "lookup performed and produced data" sentinel — see #588 review point 2.
// exchangeOrderID is stamped on Trade.ExchangeOrderID when non-empty
// (typically the OID that triggered the on-chain close, e.g.
// Position.StopLossOID).
func bookPerpsCloseWithFillFee(s *StrategyState, symbol string, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason, detailsPrefix, logPrefix string, logger *StrategyLogger) bool {
	if closePx <= 0 {
		return false
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil {
		return false
	}
	// #1009: an executable flatten/drain/kill-switch close must never compute
	// PnL off a structurally-corrupt position. Clear it with a zero-PnL leg so
	// the booked realized_pnl reconciles with the closed_positions row instead
	// of inventing a number that drives shared-wallet drift alerts.
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
			TradeType:       "perps",
			Details:         fmt.Sprintf("%s (corrupt position qty=%.6f avg_cost=%.4f) — zero PnL booked", detailsPrefix, pos.Quantity, pos.AvgCost),
			IsClose:         true,
			RealizedPnL:     0,
			PnLGross:        true,
			ExchangeOrderID: exchangeOrderIDForTrade(exchangeOrderID, useFillFee),
			FeeSource:       FeeSourceModeled,
		}
		trade.Regime = s.Regime
		RecordTrade(s, trade)
		RecordTradeResult(&s.RiskState, 0)
		recordClosedPosition(s, pos, closePx, 0, reason+"_corrupt", now)
		delete(s.Positions, symbol)
		clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
		return true
	}
	// #954 one-fill-one-row: a FULL close whose OID already produced a close
	// row for this strategy (a circuit-breaker force-close and the
	// reconciler's external-close detection racing over the same on-chain
	// fill) must not double-book cash or insert a second Trade. Clear the
	// virtual position and report success. Partial closes are exempt —
	// multiple legs of one OID across cycles are legitimate
	// (bookPerpsPartialCloseWithFillFee).
	if useFillFee && exchangeOrderID != "" && strategyHasCloseTradeForOID(s, exchangeOrderID) {
		if logger != nil {
			logger.Warn("%s: close for OID %s already booked — clearing virtual position without a duplicate Trade (#954)", logPrefix, exchangeOrderID)
		}
		recordClosedPosition(s, pos, closePx, 0, reason+"_dup_oid", time.Now().UTC())
		delete(s.Positions, symbol)
		clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
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
	// Reconciler-side fee selection: trust the userFills lookup result when
	// useFillFee is true, even if the fee is zero or negative (maker rebate).
	// Only fall back to the modeled fee when no row matched. Diverges from
	// executionFee() which gates on `> 0` for spot/futures paper paths where
	// useFillFee=true with fillFee=0 simply means "paper close, no exchange
	// fee" and the modeled fee is the right model.
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
	// Note (#604 review #3/#612): pos.TPOIDs are dropped on the floor here
	// when the reconciler detects an external close. HL auto-cancels
	// reduce-only orders once the underlying position is flat (a TP fill
	// would otherwise create a new opposite-side position, violating the
	// reduce-only flag), so the orphan OIDs are cleared exchange-side. The
	// executable close paths (full-close via runHyperliquidExecuteOrder,
	// kill-switch flatten, per-strategy circuit drain) DO explicitly cancel
	// TP OIDs by piggy-backing on the existing --cancel-stop-loss-oid argv
	// so we don't depend on auto-cancel timing for paths under our control.

	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          symbol,
		PositionID:      positionID,
		Side:            closeTradeSide(side),
		Quantity:        qty,
		Price:           closePx,
		Value:           qty * closePx,
		TradeType:       "perps",
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
	RecordTradeResult(&s.RiskState, pnl)
	recordClosedPosition(s, pos, closePx, pnl, reason, now)
	delete(s.Positions, symbol)
	clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
	if logger != nil {
		logger.Warn("%s @ $%.4f, PnL: $%.2f (fee $%.2f)", logPrefix, closePx, pnl, fee)
	}
	return true
}

// bookPerpsPartialCloseWithFillFee records a reconciler-observed perps partial
// close, credits realized PnL on the closed slice, and leaves the remaining
// virtual position open with its original AvgCost/InitialQuantity.
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
	// TP fills are typically maker-priced (HyperliquidMakerFeePct exists for
	// that), but this fallback fires only when userFills misses and the true
	// fill type is unknown — deliberately keep the taker rate (#1315 decision)
	// so virtual cash is never overstated by an optimistic maker assumption.
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
		TradeType:       "perps",
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
	RecordTradeResult(&s.RiskState, pnl)

	remaining := pos.Quantity - qty
	if remaining <= 1e-9 {
		recordClosedPosition(s, pos, closePx, pnl, reason, now)
		delete(s.Positions, symbol)
		clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
	} else {
		pos.Quantity = remaining
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

// stopLossCloseDetailsPrefix picks the Trade.Details prefix based on the
// internal SL-close reason so the trade-alert classifier can tell paper
// stops, trailing stops, and exchange-fired stops apart (#716 item 4).
// Reasons not listed default to "Stop loss close" — the canonical exchange-
// SL prefix used by the reconciler and the immediate-fill paths.
func stopLossCloseDetailsPrefix(reason string) string {
	switch reason {
	case "trailing_stop_loss_paper":
		return "Paper trailing SL close"
	case "trailing_stop_loss_immediate":
		return "Trailing SL close"
	case "stop_loss_atr_paper":
		return "Paper SL close"
	}
	return "Stop loss close"
}

// recordPerpsStopLossClose books a tracked perps stop-loss fill and removes the
// virtual position. Used both when HL reports an immediate trigger fill at
// submit time and when a previously-resting trigger has fired between cycles.
func recordPerpsStopLossClose(s *StrategyState, symbol string, triggerPx float64, reason string, logger *StrategyLogger) bool {
	return bookPerpsClose(s, symbol, triggerPx, reason, stopLossCloseDetailsPrefix(reason), "SL close reconciled", logger)
}

// recordPerpsStopLossCloseWithFillFee is the reconciler entry point — same
// behavior as recordPerpsStopLossClose but threads the userFills-resolved
// exchange fee + OID into the close Trade so virtual cash matches the
// on-chain accountValue (#588). When useFillFee=false (or fillFee<=0) the
// modeled fee is used; failed indexer lookups fall back to the legacy path.
func recordPerpsStopLossCloseWithFillFee(s *StrategyState, symbol string, triggerPx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, logger *StrategyLogger) bool {
	return bookPerpsCloseWithFillFee(s, symbol, triggerPx, fillFee, useFillFee, exchangeOrderID, reason, stopLossCloseDetailsPrefix(reason), "SL close reconciled", logger)
}

// recordPerpsExternalCloseWithFillFee is the reconciler entry point for an
// external perps close. It threads the userFills-resolved fee and (optional)
// exchange OID. The OID is rarely available for external closes since the
// close happened off-scheduler, but the coin+size match path can still
// recover the fee.
func recordPerpsExternalCloseWithFillFee(s *StrategyState, symbol string, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, logger *StrategyLogger) bool {
	return bookPerpsCloseWithFillFee(s, symbol, closePx, fillFee, useFillFee, exchangeOrderID, reason, "External close @ mark", "External close reconciled", logger)
}

func recordPerpsExternalPartialCloseWithFillFee(s *StrategyState, symbol string, closeQty, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, logger *StrategyLogger) bool {
	return bookPerpsPartialCloseWithFillFee(s, symbol, closeQty, closePx, fillFee, useFillFee, exchangeOrderID, reason, "External partial close @ mark", "External partial close reconciled", logger)
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

	// PnLGross marks rows written under the #954 gross convention: RealizedPnL
	// stores the PRE-FEE realized PnL on close legs (still 0 on opens; the
	// funding amount on trade_type="funding" rows) and ExchangeFee always
	// carries the fee that was deducted from cash — real userFills fee or the
	// modeled taker estimate, FeeSource says which. Legacy rows (false) store
	// net RealizedPnL and stamp ExchangeFee only when a real fill fee was
	// captured. Never sum RealizedPnL directly across rows — use tradeNetPnL /
	// tradeNetPnLSQL / tradeLedgerDeltaSQL (trade_pnl.go) so the two
	// conventions cannot mix (#698's gross-vs-net guard, generalized).
	PnLGross bool `json:"pnl_gross,omitempty"`
	// FeeSource records ExchangeFee provenance: "userfills" (real exchange
	// fee), "modeled" (taker-rate estimate; `backfill trade-ledger` repairs
	// these from userFills), "reconcile_adjustment" (model-only virtual-state
	// cleanup with no exchange fill to true up), or "" (legacy row / no fee
	// context).
	FeeSource string `json:"fee_source,omitempty"`

	Regime string `json:"regime,omitempty"` // market regime label at time of trade (#482)
	// RegimeDivergenceNote carries a pre-formatted divergence line for trade DMs
	// when a regime_window_divergence override was active at entry (#907). Not
	// persisted to SQLite — set transiently when a trade is recorded.
	RegimeDivergenceNote string `json:"-"`
	// RegimeProfileNote carries a pre-formatted active-profile line for trade
	// DMs when a regime_profile_allocation block is active (#998). Not persisted
	// to SQLite — set transiently when a trade is recorded.
	RegimeProfileNote string `json:"-"`

	EntryATR          float64 `json:"entry_atr,omitempty"`
	StopLossOID       int64   `json:"stop_loss_oid,omitempty"`
	StopLossTriggerPx float64 `json:"stop_loss_trigger_px,omitempty"`
	TPOIDs            []int64 `json:"tp_oids,omitempty"`
	Manual            bool    `json:"manual,omitempty"` // set when position was opened via manual-open CLI (#569)

	// SL arming method + TP tier snapshot at fill time (#669). StopLossATRMult
	// is non-nil iff SL was ATR-armed (sc.StopLossATRMult>0 OR
	// sc.TrailingStopATRMult>0); the value is the configured multiplier
	// resolved at open. nil = armed via pct/margin/trailing-pct or no SL.
	// Nullness alone gates the SL display suffix correctly. TPTiersJSON is the
	// full tier snapshot ([{atr_multiple,close_fraction},...]) resolved at
	// open so historical tier-config changes don't erase the record. Empty =
	// strategy doesn't use tiered_tp_atr*.
	StopLossATRMult *float64 `json:"stop_loss_atr_mult,omitempty"`
	TPTiersJSON     string   `json:"tp_tiers_json,omitempty"`

	// persisted tracks whether this Trade has been written to SQLite — set by
	// RecordTrade on successful InsertTrade and by LoadState for DB-loaded
	// rows. SaveState uses this flag instead of a MAX(timestamp) check so an
	// out-of-order RecordTrade failure (T1 fails, T2 succeeds) is picked up
	// on the next flush rather than silently dropped because T1 < latestTS.
	// Not serialized — purely in-memory bookkeeping.
	persisted bool
}

type SignalExecutionResult struct {
	TradesExecuted int
	OpenTrade      *Trade
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

// executionFeeSource reports the Trade.FeeSource matching what executionFee
// returned: the real fill fee only when useFillFee gated a positive value.
func executionFeeSource(fillFee float64, useFillFee bool) string {
	if useFillFee && fillFee > 0 {
		return FeeSourceUserFills
	}
	return FeeSourceModeled
}

// flipFeeShare apportions a live flip order's SINGLE real exchange fee to one
// leg by quantity share (#954). A bidirectional flip executes one net order of
// (closeQty + openQty); HL charges one fee on the whole fill. Pre-#954 the
// close leg absorbed the full real fee AND the open leg deducted a modeled fee
// on its own notional, overcharging the virtual cash book by the modeled open
// fee each flip — invisible while open-leg fees were not stamped, but a
// permanent ledger-vs-balance drift under the trade-ledger display path.
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

// strategyHasCloseTradeForOID reports whether the strategy's in-memory trade
// history already holds a CLOSE leg for this exchange order id (#954
// one-fill-one-row). In-memory is sufficient: the racing bookers run
// sequentially under the same write lock within a cycle or two, well inside
// the maxTradeHistory window, and TradeHistory is rehydrated from SQLite at
// startup.
func strategyHasCloseTradeForOID(s *StrategyState, exchangeOrderID string) bool {
	if s == nil || exchangeOrderID == "" {
		return false
	}
	for i := len(s.TradeHistory) - 1; i >= 0; i-- {
		t := s.TradeHistory[i]
		if t.IsClose && t.ExchangeOrderID == exchangeOrderID {
			return true
		}
	}
	return false
}

// formatStatusLine renders the per-strategy Phase 6 status log line. regime is
// the strategy's most recent primary regime label; an empty label (spot/options
// or strategies without regime detection) is shown as "-".
func formatStatusLine(cash float64, posCount int, value float64, trades int, regime string) string {
	if regime == "" {
		regime = "-"
	}
	return fmt.Sprintf("Status: cash=$%.2f | positions=%d | value=$%.2f | trades=%d | regime=%s",
		cash, posCount, value, trades, regime)
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

// PerpsOrderSkipReason returns a non-empty reason when ExecutePerpsSignalWithLeverage
// would treat (signal, current position side) as a no-op. Callers that place
// live orders BEFORE invoking ExecutePerpsSignalWithLeverage (e.g. runHyperliquidExecuteOrder)
// must consult this guard first — otherwise the on-chain fill happens but the
// in-memory execution path returns 0 and no Trade is recorded, leaving
// virtual state permanently behind actual exchange positions (#298).
//
// posSide is "" when no position exists; "long" or "short" otherwise.
// direction is the StrategyConfig.Direction enum (#656):
//   - "long" (legacy): signal=-1 with no long is a skip (close-long-only); signal=1 only opens longs.
//   - "short": signal=1 with no short is a skip (close-short-only); signal=-1 only opens shorts.
//   - "both": bidirectional — signal=-1 with no position opens a short; signal=-1
//     while already short is a skip (mirrors "already long, skipping buy"); signal=1
//     opens long from flat or flips a short.
//
// Empty/unknown direction is treated as "long" for safety (matches the legacy
// allow_shorts=false default).
//
// The `s.Cash < 1` branch inside the open paths is NOT mirrored here because
// cash after a flip-close leg cannot be derived from (signal, posSide) alone —
// live callers guard cash upstream before placing the order (see
// runHyperliquidExecuteOrder). If a new side-based no-op branch is added to
// ExecutePerpsSignalWithLeverage, add it here too.
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
			// #656: orphan long under direction="short" must NOT place an
			// open-short order. ExecutePerpsSignalWithLeverage would skip-and-warn, so
			// skipping the live order here mirrors that and avoids the #298
			// "live fill lands but no Trade recorded" gap.
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

// perpsLiveOrderSize returns the market-order size to place for a live perps
// execution. PerpsOrderSkipReason must already have passed (no skip).
//
// direction is the StrategyConfig.Direction enum (#656). The four cases per
// direction value:
//
//   - direction="long" (legacy long-only):
//   - signal=1 + flat   → size = PerpsOpenNotional(...)/price (open long)
//   - signal=1 + short  → size = posQty + new (legacy migrated short flip)
//   - signal=-1 + long  → size = posQty (close-only)
//   - direction="short" (#656 short-only):
//   - signal=-1 + flat  → size = PerpsOpenNotional(...)/price (open short)
//   - signal=1 + short  → size = posQty (close-only)
//   - direction="both" (bidirectional):
//   - signal=1 + flat   → fresh long
//   - signal=-1 + flat  → fresh short
//   - signal=1 + short  → flip = posQty + new
//   - signal=-1 + long  → flip = posQty + new
//
// The flip branch is what this helper exists for: without `posQty + newSize`
// a bidirectional scheduler tells ExecutePerpsSignalWithLeverage to virtually close+open
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
// sizing bundles the notional/risk sizing inputs (#497/#518/#1268). With
// sizing.MarginPerTradeUSD positive the open budget is margin-space:
// notional = min(marginPerTradeUSD, effectiveCash) × exchangeLeverage,
// independent of sizingLeverage (the hardcoded 0.95 safety buffer was removed
// in #518). With sizing.RiskPerTradePct positive the budget is risk-based
// (PerpsRiskBasedNotional); an unresolvable stop distance FAILS CLOSED on a
// fresh open (ok=false with the resolver's reason — never a silent notional
// fallback) and degrades a flip to close-only, same as insufficient cash.
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
func perpsLiveOrderSize(signal int, price, cash, posQty, avgCost float64, sizing PerpsSizing, posSide, direction string, closeFraction float64) (size float64, ok bool, reason string) {
	isBuy := signal == 1
	allowsLong := direction == DirectionLong || direction == DirectionBoth || direction == ""
	allowsShort := direction == DirectionShort || direction == DirectionBoth
	// Flip only happens under bidirectional ("both"); a directional gate
	// ("long"/"short") never flips because the opposite-direction signal is
	// either a close-only (long-only sell on long, short-only buy on short)
	// or has already been rejected by PerpsOrderSkipReason.
	//
	// #1009: a flip must also require closeFraction == 0. Any closeFraction > 0
	// is a close action from the open/close registry — closeOnlyAction in the
	// executor (ExecutePerpsSignalWithLeverage) skips the open leg entirely, so
	// the sizer must NOT size a (posQty + newSize) reversal here. Without this
	// guard a fractional/full close under "both" placed a flip-sized order whose
	// fill (> posQty) drove the executor's close leg negative.
	flipping := direction == DirectionBoth && posQty > 0 && closeFraction == 0 && ((isBuy && posSide == "short") || (!isBuy && posSide == "long"))
	// Fresh open: a buy from flat under any direction that allows longs, or
	// a sell from flat under any direction that allows shorts. Buy + short
	// position under "long"-direction is the legacy-migrated-short edge case
	// (pre-#330) that fresh-sizes without offset; #656 keeps that intact.
	openingFresh := false
	if isBuy && allowsLong && (posQty <= 0 || (posSide == "short" && direction == DirectionLong)) {
		openingFresh = true
	}
	if !isBuy && allowsShort && posQty <= 0 {
		openingFresh = true
	}

	if openingFresh || flipping {
		// #1268 fail-closed: risk-per-trade sizing with an unresolvable stop
		// distance refuses a FRESH open outright (the generic insufficient-cash
		// message below would misattribute the cause). A flip falls through to
		// the budget<1 close-only degrade — the close leg must still fire.
		if openingFresh && sizing.RiskPerTradePct > 0 && sizing.RiskStopDistance <= 0 {
			return 0, false, fmt.Sprintf("risk_per_trade_pct sizing: %s — refusing open (fail-closed)", sizing.riskUnresolvedLabel())
		}
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
		budget := PerpsOpenNotionalSized(effectiveCash, price, sizing)
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

// perpsCloseActionSuppressesNewSL reports whether a perps order is a close-only
// action that opens no new position, so the HL execute path must NOT arm a new
// reduce-only stop-loss (it may still cancel the existing one — that is gated
// separately on !partialClose). It is the `pureClose` decision in
// runHyperliquidExecuteOrder, extracted as a pure helper for testability.
//
// True when:
//   - a directional gate closes only: signal=-1 on a long with shorts disallowed,
//     or signal=1 on a short with longs disallowed (legacy long/short-only exits); or
//   - a FULL close-action from the open/close registry (closeFraction == 1.0) —
//     the executor's closeOnlyAction returns before any open leg, so no new side
//     exists to protect. Before #1009 this case was masked by `flipping == true`
//     (which set prev_pos_qty = posQty → net_new_sz = 0 → SL skipped); once the
//     flip predicate correctly required closeFraction == 0, the suppression had
//     to move here or a fresh SL leaked onto a just-closed position.
//
// False for a genuine reversal flip (direction="both", opposite side,
// closeFraction == 0): that opens a new side and MUST arm its stop-loss. A
// partial close (0 < frac < 1) is handled by the caller's separate partialClose
// guard (which suppresses BOTH cancel and new-SL), so it is intentionally not
// folded in here.
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

// SpotOrderSkipReason mirrors PerpsOrderSkipReason for spot. ExecuteSpotSignalWithFillFee's
// side-based skip branches ("already long, skipping buy" at signal=1,
// "No long position to sell, skipping" at signal=-1) must be consulted BEFORE
// the live helper spawns a Python order placer — otherwise a live fill lands
// on the exchange but ExecuteSpotSignalWithFillFee returns 0 and no Trade is recorded,
// leaving virtual state behind real holdings. See #298 / #300.
//
// Matching conditions to ExecuteSpotSignalWithFillFee:
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
// live short, even though paper-mode ExecuteFuturesSignalWithFillFee can). With those
// semantics, the guard matches spot/perps:
//   - signal == 1 && pos.Side == "long" → "Already long, skipping buy"
//   - signal == -1 && no long position   → "No long position to sell, skipping"
//
// Without this guard, a live sell fires with posSide=="short" (Quantity is
// always positive so the posQty<=0 check does not catch it) but
// ExecuteFuturesSignalWithFillFee is a side-based no-op when already short, producing a
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
//
// direction (#656) gates the open-side branches:
//   - "long": signal=1 opens long; signal=-1 closes long; never opens short.
//   - "short": signal=-1 opens short; signal=1 closes short; never opens long.
//   - "both": fully bidirectional (legacy AllowShorts=true).
//
// Empty direction is treated as "long" for safety.
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
	bidirectional := direction == DirectionBoth // flip semantics retained only for "both"
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
			// #656 review: surface the state-config gap on signal=1 with an
			// orphan long under direction="short" — symmetric with the
			// orphan-long warning in the signal=-1 branch below.
			if !allowsLong {
				logger.Warn("Orphan long %s under direction=%q (qty=%.6f); skipping buy — close manually if intentional", symbol, direction, pos.Quantity)
			} else {
				logger.Info("Already long %s (qty=%.6f), skipping buy", symbol, pos.Quantity)
			}
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
				// #1009 backstop: a flip-sized fill must never close more than
				// the position holds — else pos.Quantity -= closeQty goes
				// negative and realized_pnl is booked against the over-large
				// fill qty. perpsLiveOrderSize no longer flip-sizes a close, but
				// cap here defensively across every caller (paper, OKX, manual).
				if closeQty > pos.Quantity {
					closeQty = pos.Quantity
				}
			}
			// Flip semantics only under direction="both"; "short"-direction
			// closes the short terminally (no open-long follows), and the
			// legacy "long"-direction migrated-short close also has no flip
			// counterpart that needs offsetting (open-long fresh-sizes from cash).
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
			// Terminal close: no open-long leg follows when this is a registry
			// close-only action (#519) or when direction forbids long opens (#656).
			// Either way the close leg owns the single live fill fee.
			terminalClose := closeOnlyAction || !allowsLong
			useFillFee := flipCloseQty > 0 || terminalClose
			legFillFee := fillFee
			if flipCloseQty > 0 && !terminalClose && fillQty > 0 {
				// Live flip: the exchange charged ONE fee for the whole
				// (close + open) order — this leg takes its qty share; the
				// open leg below takes the rest (#954).
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
		// #656: direction="short" closes shorts on signal=1 but never opens
		// a long. PerpsOrderSkipReason already drops signal=1 from flat under
		// "short", so this path is reached only after a close-short above.
		if !allowsLong {
			if tradesExecuted == 0 {
				logger.Info("No short position in %s to buy-cover, skipping (direction=%q)", symbol, direction)
			}
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
			// sizing_leverage or margin_per_trade_usd explicitly. Risk mode
			// (#1268) sizes qty from stop distance and FAILS CLOSED when the
			// distance is unresolvable — never a notional fallback. On a paper
			// flip the close-short leg above already realized its PnL into
			// s.Cash, so the risk base is post-close cash, matching live.
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
			// Live flip: this open leg carries its qty share of the single
			// fill fee (the close leg took the rest) and stamps the shared
			// OID — `backfill trade-ledger` apportions fee across all legs
			// of one OID, closedPnl across close legs only (#954).
			useFillFee = true
			legFillFee = flipFeeShare(fillFee, qty, fillQty)
		}
		fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), legFillFee, useFillFee)
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

	} else if signal == -1 { // Sell
		// Dedupe: already short under any direction that allows shorts —
		// symmetric mirror of the "Already long ... skipping buy" branch
		// in the signal==1 block above. Covers direction="short" (#656)
		// and direction="both" (#328); under direction="long" (legacy),
		// allowsShort=false so the migrated-short edge case falls through
		// and is handled by the close-long branch (no match) + the
		// "no long to sell" return at the bottom.
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "short" && allowsShort {
			logger.Info("Already short %s (qty=%.6f), skipping sell", symbol, pos.Quantity)
			return 0, nil
		}
		// Close long if exists — realize PnL.
		if pos, exists := s.Positions[symbol]; exists && pos.Side == "long" {
			// #656: direction="short" with an orphan long is a state-config
			// gap (ValidatePerpsDirectionConfig surfaces it at startup). Don't
			// auto-close here — silent flatten of an operator-seeded position
			// is worse than leaving it visible. The orphan stays put; signal=1
			// would also skip via PerpsOrderSkipReason.
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
				// #1009 backstop: see the symmetric close-short branch above —
				// cap a flip-sized fill at the held quantity so the residual
				// never goes negative and PnL is not overstated.
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
			// Terminal close: no open-short leg follows when this is a registry
			// close-only action (#519) or when direction forbids short opens (#656).
			terminalClose := closeOnlyAction || !allowsShort
			useFillFee := flipCloseQty > 0 || terminalClose
			legFillFee := fillFee
			if flipCloseQty > 0 && !terminalClose && fillQty > 0 {
				// Live flip: qty share of the single fill fee (see the
				// symmetric signal==1 branch).
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
		// Long-only path: whether we closed a long or had nothing to close,
		// direction="long" never opens a short. Log only when we did nothing
		// (close-path already logged). #656: also gates direction="" defaulting.
		if !allowsShort {
			if tradesExecuted == 0 {
				logger.Info("No long position in %s to sell, skipping", symbol)
			}
			return tradesExecuted, nil
		}
		// Open short (direction="short" or "both").
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
			// #1268: same risk-mode dispatch + fail-closed guard as the
			// open-long branch above.
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
			// Live flip: this open leg carries its qty share of the single
			// fill fee (the close leg took the rest) and stamps the shared
			// OID — `backfill trade-ledger` apportions fee across all legs
			// of one OID, closedPnl across close legs only (#954).
			useFillFee = true
			legFillFee = flipFeeShare(fillFee, qty, fillQty)
		}
		fee := executionFee(CalculatePlatformSpotFee(feePlatform, notional), legFillFee, useFillFee)
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

// perpsSizingLabel renders the sizing mode for trade Details/log lines: the
// legacy leverage label, or the risk-per-trade form when the mode is on (#1268).
func perpsSizingLabel(sizing PerpsSizing) string {
	if sizing.RiskPerTradePct > 0 {
		return fmt.Sprintf("%.1fx exchange, risk %g%%/trade", sizing.ExchangeLeverage, sizing.RiskPerTradePct)
	}
	return perpsLeverageLabel(sizing.ExchangeLeverage, sizing.SizingLeverage)
}

// ExecuteSpotSignalWithFillFee processes a spot signal with optional live
// fill metadata. closeFraction (#519) is the partial-close fraction emitted by
// the open/close registry: when 0 < frac < 1 on a close-side signal the close
// leg reduces pos.Quantity (paper) or uses fillQty (live) without deleting
// the position. closeFraction == 0 preserves the legacy full-close semantics.
func ExecuteSpotSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, fillQty float64, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (int, error) {
	return executeSpotSignalWithFillFee(s, signal, symbol, price, fillQty, fillFee, fillOID, closeFraction, logger, func(trade Trade) {
		RecordTrade(s, trade)
	})
}

func ExecuteSpotSignalWithFillFeeDeferredOpen(s *StrategyState, signal int, symbol string, price float64, fillQty float64, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (SignalExecutionResult, error) {
	var result SignalExecutionResult
	trades, err := executeSpotSignalWithFillFee(s, signal, symbol, price, fillQty, fillFee, fillOID, closeFraction, logger, func(trade Trade) {
		t := trade
		result.OpenTrade = &t
	})
	result.TradesExecuted = trades
	return result, err
}

func executeSpotSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, fillQty float64, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger, recordOpen func(Trade)) (int, error) {
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
			grossPnL := pnl + fee
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
			ExchangeFee:     fee,
			FeeSource:       executionFeeSource(fillFee, useFillMetadata),
			PnLGross:        true,
		}
		trade.Regime = s.Regime
		recordOpen(trade)
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
			grossPnL := pnl + fee
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
	return tradesExecuted, nil
}

// ExecuteFuturesSignalWithFillFee processes a futures signal with optional
// live fill metadata. closeFraction (#519) is the partial-close fraction
// emitted by the open/close registry; whole-contract sizing rounds the close
// leg DOWN to ensure the residual position has at least one contract
// remaining (a tier returning a fraction smaller than 1 contract is a no-op
// rather than a full close).
func ExecuteFuturesSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, spec ContractSpec, feePerContract float64, maxContracts int, fillContracts int, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (int, error) {
	return executeFuturesSignalWithFillFee(s, signal, symbol, price, spec, feePerContract, maxContracts, fillContracts, fillFee, fillOID, closeFraction, logger, func(trade Trade) {
		RecordTrade(s, trade)
	})
}

func ExecuteFuturesSignalWithFillFeeDeferredOpen(s *StrategyState, signal int, symbol string, price float64, spec ContractSpec, feePerContract float64, maxContracts int, fillContracts int, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger) (SignalExecutionResult, error) {
	var result SignalExecutionResult
	trades, err := executeFuturesSignalWithFillFee(s, signal, symbol, price, spec, feePerContract, maxContracts, fillContracts, fillFee, fillOID, closeFraction, logger, func(trade Trade) {
		t := trade
		result.OpenTrade = &t
	})
	result.TradesExecuted = trades
	return result, err
}

func executeFuturesSignalWithFillFee(s *StrategyState, signal int, symbol string, price float64, spec ContractSpec, feePerContract float64, maxContracts int, fillContracts int, fillFee float64, fillOID string, closeFraction float64, logger *StrategyLogger, recordOpen func(Trade)) (int, error) {
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
			ExchangeFee:     fee,
			FeeSource:       executionFeeSource(fillFee, useFillMetadata),
			PnLGross:        true,
		}
		trade.Regime = s.Regime
		recordOpen(trade)
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

// stampOpenTradeFromPosition backfills open-trade protection snapshot fields
// onto the most recent open Trade for symbol after those values are stamped
// onto the Position post-RecordTrade. The normal same-cycle open path records
// these fields in one INSERT via recordPositionOpen; this helper remains for
// fallback protection arming after the open row already exists.
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
