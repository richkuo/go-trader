package main

import (
	"fmt"
	"time"
)

// hedge.go implements the #1159 phase-1 correlated-hedge engine: pure sizing/
// side helpers, HL order wrappers, and state appliers. The hedge lifecycle is
// mirrored synchronously at the main.go HL perps dispatch site (open/scale-in/
// close/flip) and repaired asynchronously by the coherence sweep
// (hedge_sweep.go) for every path that doesn't run through that dispatch
// (reconcile, kill-switch, circuit breaker, manual force-close, restart).
//
// Collision rejection (hyperliquidHedgeCollisionErrors, config.go) guarantees
// every hedge coin is sole-owned, so every on-chain hedge operation here may
// safely use RunHyperliquidClose's sz=None (full close) form without a
// shared-coin ambiguity check.

// hedgeOpenQty computes the notional-based hedge size for a primary fill:
// hedge notional = primary fill notional * ratio, converted to hedge-coin
// size at hedgeMid. Returns ok=false when the result isn't a usable order
// (non-positive inputs, unpriceable hedge coin, or a qty that rounds to
// non-positive).
func hedgeOpenQty(primaryFillQty, primaryFillPx, ratio, hedgeMid float64) (float64, bool) {
	if primaryFillQty <= 0 || primaryFillPx <= 0 || ratio <= 0 || hedgeMid <= 0 {
		return 0, false
	}
	notional := primaryFillQty * primaryFillPx * ratio
	qty := notional / hedgeMid
	if qty <= 0 {
		return 0, false
	}
	return qty, true
}

// hedgeReduceQty computes the hedge-leg size to reduce by when the primary
// position shrinks by primaryQtyClosed out of a pre-close primaryQtyBefore.
// Reduces the hedge proportionally by quantity share; returns the full hedge
// qty (no dust residue) once the primary fraction closed is within epsilon of
// 1 (i.e. effectively a full close).
func hedgeReduceQty(hedgeQty, primaryQtyBefore, primaryQtyClosed float64) float64 {
	if hedgeQty <= 0 || primaryQtyBefore <= 0 || primaryQtyClosed <= 0 {
		return 0
	}
	frac := primaryQtyClosed / primaryQtyBefore
	if frac >= 1-1e-6 {
		return hedgeQty
	}
	reduce := hedgeQty * frac
	if reduce > hedgeQty {
		reduce = hedgeQty
	}
	return reduce
}

// hedgeOrderSkipReason is the requirement-5 skip-reason mirror for a hedge
// order, evaluated before spawning: catches the qty/price preconditions every
// hedge order needs. Returns "" when the order should proceed. Foreign-
// position and side-mismatch checks (which need the on-chain position list or
// the persisted hedge Position) are done by the caller — the reconcile pass
// and coherence sweep, which already have that context — rather than
// duplicated here.
func hedgeOrderSkipReason(qty, hedgeMid float64) string {
	if hedgeMid <= 0 {
		return "hedge mid price unavailable"
	}
	if qty <= 0 {
		return "hedge order size is non-positive"
	}
	return ""
}

// resolveHedgeMid returns cycleMid when it's already usable (the common
// case — the cycle's mark fetch covered the hedge coin), else attempts a
// single inline re-fetch of just that coin's mid before giving up.
//
// #1337 review (Recommended Optional): every other consumer of a missing HL
// mark degrades gracefully (portfolio valuation falls back to AvgCost, WARN-
// logged) — a hedge-open/add is the only path that turned a transient,
// recoverable one-cycle data gap into an irreversible cost, since
// hedgeMid<=0 fails hedgeOpenQty and triggers the fail-closed unwind of a
// real, just-opened primary position. There is no "next cycle" retry for a
// fresh open — the position that failed to hedge is closed by the unwind,
// so an inline retry within the SAME event is the only chance to recover a
// blip; deferring to the coherence sweep is not an option since the sweep
// never opens a hedge (#1159 phase 1: hedge lifecycle is open/scale-in-only
// at the dispatch mirror). Returns 0 if the coin has no usable mid.
func resolveHedgeMid(sc StrategyConfig, cycleMid float64) float64 {
	if cycleMid > 0 {
		return cycleMid
	}
	coin := hedgeCoin(sc)
	if coin == "" {
		return 0
	}
	mids, err := fetchHyperliquidMids([]string{coin})
	if err != nil {
		return 0
	}
	if mid, ok := mids[coin]; ok && mid > 0 {
		return mid
	}
	return 0
}

// runHyperliquidHedgeOpenOrder places a live HL order on the hedge coin: a
// fresh open when prevPosQty==0 (the sole-owner case guaranteed by collision
// rejection), or a scale-in-shaped add when prevPosQty>0. No stop-loss is
// requested (phase 1: no independent hedge SL/TP) and marginMode/leverage
// come from the hedge block, never the primary's (constraint 3).
func runHyperliquidHedgeOpenOrder(sc StrategyConfig, qty float64, side string, prevPosQty float64) (*HyperliquidExecuteResult, string, error) {
	coin := hedgeCoin(sc)
	return RunHyperliquidExecute(sc.Script, coin, side, qty, 0, 0, prevPosQty, hedgeMarginMode(sc), hedgeExchangeLeverage(sc), false, hlExecuteSnapshot{})
}

// runHyperliquidHedgeCloseOrder submits a reduce-only close on the hedge
// coin. partialSz=nil requests a full close (safe — sz=None — only because
// collision rejection makes the strategy the coin's sole owner); a non-nil
// partialSz requests a sized reduce-only close (e.g. the primary's unwind
// counterpart, or a proportional hedge reduction).
func runHyperliquidHedgeCloseOrder(sc StrategyConfig, partialSz *float64) (*HyperliquidCloseResult, string, error) {
	coin := hedgeCoin(sc)
	return RunHyperliquidClose(sc.Script, coin, partialSz, nil)
}

// applyHedgeOpenFill books a fresh hedge-leg open, or grows an existing one on
// a scale-in add, mirroring the "margin-based: only fee leaves cash" perps-
// open convention (see executePerpsSignalWithLeverage). Must be called under
// the caller's mu.Lock. Stamps hedge pairing metadata on both legs (the hedge
// Position's IsHedge/HedgeForSymbol/HedgeForPositionID and the primary
// Position's HedgeSymbol) and records an open Trade tagged IsHedge. Returns
// the resulting hedge Position, or nil if qty/execPx are non-positive OR
// (#1337 review) an existing hedge Position is on the OPPOSITE side of this
// fill's side — blending would silently sum opposite-direction quantities
// while keeping the stale Side, producing a Position that matches neither
// leg's real exposure. This can only legitimately happen if a caller failed
// to serialize a flip's close-before-reopen (mirrorHedgeAfterPrimaryFill
// already gates that); refusing here is defense-in-depth so a caller bug
// can never silently corrupt the hedge Position — callers MUST treat a nil
// return with a real fill (qty/execPx positive) as a failure requiring the
// same fail-closed handling as an order failure, since real money moved
// on-chain but was refused a home in state.
func applyHedgeOpenFill(s *StrategyState, sc StrategyConfig, primaryCoin, primaryPositionID, side string, qty, execPx, modeledFee, fillFee float64, fillOID string, useFillFee bool) *Position {
	if qty <= 0 || execPx <= 0 {
		return nil
	}
	hedgeSym := hedgeCoin(sc)
	posSide := "long"
	if side == "sell" {
		posSide = "short"
	}
	pos, exists := s.Positions[hedgeSym]
	if exists && pos != nil && pos.Side != posSide && pos.Quantity > 0 {
		return nil
	}
	fee := executionFee(modeledFee, fillFee, useFillFee)
	s.Cash -= fee
	now := time.Now().UTC()
	if !exists || pos == nil {
		positionID := newTradePositionID(s.ID, hedgeSym, now)
		pos = &Position{
			Symbol:             hedgeSym,
			Quantity:           qty,
			InitialQuantity:    qty,
			AvgCost:            execPx,
			Side:               posSide,
			Multiplier:         1,
			Leverage:           hedgeExchangeLeverage(sc),
			OwnerStrategyID:    s.ID,
			OpenedAt:           now,
			TradePositionID:    positionID,
			IsHedge:            true,
			HedgeForSymbol:     primaryCoin,
			HedgeForPositionID: primaryPositionID,
		}
		s.Positions[hedgeSym] = pos
	} else {
		// Scale-in-shaped add: blend price/size (same side, guaranteed by
		// the opposite-side guard above). The hedge leg has no on-chain
		// protection geometry to freeze (phase 1: no hedge SL/TP), so a plain
		// qty/AvgCost blend is sufficient — unlike the primary's #873
		// RiskAnchorPrice freeze.
		totalQty := pos.Quantity + qty
		pos.AvgCost = (pos.AvgCost*pos.Quantity + execPx*qty) / totalQty
		pos.Quantity = totalQty
	}
	if primary, exists := s.Positions[primaryCoin]; exists && primary != nil {
		primary.HedgeSymbol = hedgeSym
	}
	trade := Trade{
		Timestamp:       now,
		StrategyID:      s.ID,
		Symbol:          hedgeSym,
		PositionID:      pos.TradePositionID,
		Side:            side,
		Quantity:        qty,
		Price:           execPx,
		Value:           qty * execPx,
		TradeType:       "perps",
		Details:         fmt.Sprintf("HEDGE open %s %.6f @ $%.2f (fee $%.2f)", posSide, qty, execPx, fee),
		ExchangeOrderID: exchangeOrderIDForTrade(fillOID, useFillFee),
		ExchangeFee:     fee,
		FeeSource:       executionFeeSource(fillFee, useFillFee),
		PnLGross:        true,
		IsHedge:         true,
	}
	trade.Regime = s.Regime
	RecordTrade(s, trade)
	return pos
}

// applyHedgeCloseFill books a full close of the hedge leg, reusing
// bookPerpsCloseWithFillFee (which already tags the Trade IsHedge from
// pos.IsHedge) for correct PnL/corrupt-position/#954-dedup handling, then
// clears the pairing stamp on the primary leg if it's still present. reason
// should carry the "hedge_" prefix (e.g. "hedge_close").
func applyHedgeCloseFill(s *StrategyState, primaryCoin, hedgeSym string, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, logger *StrategyLogger) bool {
	ok := bookPerpsCloseWithFillFee(s, hedgeSym, closePx, fillFee, useFillFee, exchangeOrderID, reason, "HEDGE close", "HEDGE close", logger)
	if ok {
		if primary, exists := s.Positions[primaryCoin]; exists && primary != nil {
			primary.HedgeSymbol = ""
		}
	}
	return ok
}

// applyHedgeReduceFill books a partial (proportional) reduction of the hedge
// leg, reusing bookPerpsPartialCloseWithFillFee for the same reason
// applyHedgeCloseFill reuses the full-close helper.
func applyHedgeReduceFill(s *StrategyState, hedgeSym string, closeQty, closePx, fillFee float64, useFillFee bool, exchangeOrderID, reason string, logger *StrategyLogger) bool {
	return bookPerpsPartialCloseWithFillFee(s, hedgeSym, closeQty, closePx, fillFee, useFillFee, exchangeOrderID, reason, "HEDGE partial close", "HEDGE partial close", logger)
}

// checkHedgeStateDriftAtStartup detects persisted hedge pairing state that
// disagrees with the current config in a way SIGHUP can't have produced —
// only a config edit + process restart can reach these states, since
// validateHotReloadStateCompatible blocks hedge-block edits while a hedge leg
// is open (constraint 7). Returns owner-DM warning lines, mirroring the
// established startup-drift pattern (ValidatePerpsDirectionConfig,
// checkATRMethodDriftAtStartup) — a warning-only surface, not a fail-loud
// process exit; the coherence sweep (hedge_sweep.go) is the actual repair
// path for a resulting P-without-H or H-without-P state, so refusing to boot
// here would only delay that repair while adding an outage risk of its own.
// Three arms:
//  1. hedge enabled but the open primary carries no HedgeSymbol stamp
//     (a hedge was just enabled on a strategy already holding a position);
//  2. hedge disabled/absent but a persisted IsHedge position exists
//     (a hedge was just disabled while a hedge leg is still open on-chain);
//  3. the configured hedge symbol differs from the persisted hedge
//     position's coin (the hedge symbol was edited while a leg was open).
func checkHedgeStateDriftAtStartup(cfg *Config, state *AppState) []string {
	if cfg == nil || state == nil {
		return nil
	}
	var warnings []string
	for _, sc := range cfg.Strategies {
		if sc.Type != "perps" || sc.Platform != "hyperliquid" {
			continue
		}
		ss, ok := state.Strategies[sc.ID]
		if !ok || ss == nil {
			continue
		}
		primaryCoin := hyperliquidConfiguredCoin(sc)
		var primary *Position
		if primaryCoin != "" {
			primary = ss.Positions[primaryCoin]
		}
		var hedgePos *Position
		hedgeSymFromState := ""
		for _, pos := range ss.Positions {
			if pos != nil && pos.IsHedge && pos.HedgeForSymbol == primaryCoin {
				hedgePos = pos
				hedgeSymFromState = pos.Symbol
				break
			}
		}
		if sc.HedgeEnabled() {
			if primary != nil && primary.Quantity > 0 && primary.HedgeSymbol == "" {
				warnings = append(warnings, fmt.Sprintf("hedge (#1159): strategy %s hedge is enabled but the open primary position on %s carries no hedge pairing — a hedge block was likely added to a strategy that already held a position; the coherence sweep will fail-close the unhedged primary", sc.ID, primaryCoin))
			}
			if hedgePos != nil {
				configuredCoin := hedgeCoin(sc)
				if configuredCoin != "" && configuredCoin != hedgeSymFromState {
					warnings = append(warnings, fmt.Sprintf("hedge (#1159): strategy %s hedge.symbol is now %q but an open hedge leg on %q is still persisted — a hedge symbol was edited while a leg was open; flatten manually or restore the prior hedge.symbol", sc.ID, configuredCoin, hedgeSymFromState))
				}
			}
		} else if hedgePos != nil && hedgePos.Quantity > 0 {
			warnings = append(warnings, fmt.Sprintf("hedge (#1159): strategy %s hedge is disabled/absent but an open hedge leg on %s is still persisted — hedge was likely disabled while a leg was open; the coherence sweep will close the orphaned hedge leg", sc.ID, hedgeSymFromState))
		}
	}
	return warnings
}
