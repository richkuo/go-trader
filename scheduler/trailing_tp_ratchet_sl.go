package main

import (
	"fmt"
	"sync"
)

// ratchetTightenReplacePolicy is the walker policy every ratchet→walker
// dispatch uses on a cycle where applyTrailingTPRatchet* returned a non-nil
// alert (i.e. a tier STRICTLY tightened the trail). It drops the min-move
// debounce so the newly stamped, tighter trigger reaches the exchange the same
// cycle instead of being discarded whenever Δmult × EntryATR / anchor lands
// under trailing_stop_min_move_pct (#1416).
//
// Bounded by construction: a tier alerts at most once (SLAdjustedTiersProcessed
// advances past it), so this can never turn into per-cycle cancel+replace churn.
var ratchetTightenReplacePolicy = trailingReplacePolicy{ratchetTightened: true}

// runTrailingStopUpdateAfterRatchetTighten re-runs the trailing walker on the
// SAME cycle a ratchet tier tightened during execute (#1416).
//
// Why this exists: the perps manage path (ratchet → re-snapshot → walker) is
// gated on result.Signal == 0. A scale-out ratchet tier emits close_fraction > 0,
// so that path is skipped; execute books the partial close and
// applyTrailingTPRatchetToPosition stamps the tighter PostTPTrailingATRMult
// afterward — but nothing replaces the resting SL until a later Signal==0
// cycle. That leaves the residual under-protected at the OLD wider trigger for
// up to a full strategy interval (and unbounded if every later cycle also
// emits a close signal).
//
// Mirrors the manual manage sequence (ratchet then walker) and the #885/#882
// same-cycle walker pattern: live cancel+replace runs OUTSIDE mu via
// runHyperliquidTrailingStopUpdate; paper advances the virtual trigger via
// runHyperliquidTrailingStopPaper. Caller gates on a non-nil tighten alert so
// trail-only / no-op ratchet cycles don't pay an extra walker pass.
//
// Partial-close on-chain qty: the Phase-1 reconcile snapshot is pre-close
// (larger than residual). hlSLEffectiveQty takes min(virtual, on-chain), so the
// residual virtual qty wins — never oversized relative to what's left.
func runTrailingStopUpdateAfterRatchetTighten(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	hlOnChainAbsQty map[string]float64,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
) (int, string) {
	if stratState == nil || symbol == "" || mark <= 0 {
		return 0, ""
	}
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return 0, ""
	}

	mu.RLock()
	pos := stratState.Positions[symbol]
	if pos == nil || pos.Quantity <= 0 || effectiveTrailingStopPct(sc, pos) <= 0 {
		mu.RUnlock()
		return 0, ""
	}
	side := pos.Side
	highWater := pos.StopLossHighWaterPx
	triggerPx := pos.StopLossTriggerPx
	slOID := pos.StopLossOID
	qty := pos.Quantity
	posSnap := *pos
	mu.RUnlock()

	if hyperliquidIsLive(sc.Args) {
		slEffectiveQty, capped := hlSLEffectiveQty(symbol, qty, hlOnChainAbsQty)
		if capped && logger != nil {
			logger.Warn("ratchet same-cycle trailing SL: virtual qty %.6f > on-chain %.6f for %s; capping (#621)", qty, slEffectiveQty, symbol)
		}
		newHighWater, slUpdate, updateConfirmed := runHyperliquidTrailingStopUpdate(
			sc, symbol, side, slEffectiveQty, &posSnap, mark, highWater, triggerPx, slOID, ratchetTightenReplacePolicy, notifier, logger)
		mu.Lock()
		defer mu.Unlock()
		if immediateFill, fillPx := applyTrailingStopUpdateResult(stratState, symbol, side, slOID, newHighWater, updateConfirmed, slUpdate, logger); immediateFill {
			return 1, fmt.Sprintf("[%s] LIVE TRAILING SL %s @ $%.2f", sc.ID, symbol, fillPx)
		}
		return 0, ""
	}

	// Paper (#532): no exchange trigger; advance the virtual HWM/trigger with the
	// tightened PostTPTrailingATRMult already stamped by the ratchet.
	newHighWater, newTrigger, breach, breachPx := runHyperliquidTrailingStopPaper(sc, side, &posSnap, mark, highWater, triggerPx, ratchetTightenReplacePolicy)
	mu.Lock()
	defer mu.Unlock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.Side != side {
		return 0, ""
	}
	if breach {
		if recordPerpsStopLossClose(stratState, symbol, breachPx, "trailing_stop_loss_paper", logger) {
			return 1, fmt.Sprintf("[%s] PAPER TRAILING SL %s @ $%.2f", sc.ID, symbol, breachPx)
		}
		return 0, ""
	}
	if newHighWater > 0 {
		pos.StopLossHighWaterPx = newHighWater
	}
	if newTrigger > 0 {
		pos.StopLossTriggerPx = newTrigger
		pos.RatchetFallbackNormalizePending = false
		if logger != nil {
			logger.Info("Paper trailing SL trigger updated after ratchet @ $%.4f (high_water=$%.4f)", newTrigger, newHighWater)
		}
	}
	return 0, ""
}
