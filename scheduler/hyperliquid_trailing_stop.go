package main

import (
	"fmt"
	"math"
	"sync"
)

const defaultTrailingStopMinMovePct = 0.5

var runHyperliquidUpdateStopLossFunc = RunHyperliquidUpdateStopLoss

// hlSLEffectiveQty returns the quantity to use for stop-loss placement.
// When the on-chain position is smaller than the virtual position (e.g.
// after a manual TP reduced the position without the bot's knowledge),
// the on-chain qty is used to avoid placing an oversized reduce-only order
// that HL would reject (#621). Returns (virtualQty, false) when no cap applies.
func hlSLEffectiveQty(symbol string, virtualQty float64, onChainQtyMap map[string]float64) (float64, bool) {
	if onChainQty, ok := onChainQtyMap[symbol]; ok && onChainQty > 1e-9 && onChainQty < virtualQty-1e-9 {
		return onChainQty, true
	}
	return virtualQty, false
}

// effectiveTrailingStopPct returns the per-position trailing-stop distance as a
// price-% (e.g. 3.0 == 3%). HL perps only, except manual strategies that
// explicitly use trailing_tp_ratchet*.
//
// Resolution order:
//   - explicit TrailingStopPct (fixed distance) wins; explicit 0 disables.
//   - TrailingStopATRMult derives the distance from the position's EntryATR
//     and AvgCost: pct = mult * entry_atr / avg_cost * 100, capped at
//     MaxAutoStopLossPct so a volatile coin (e.g. mult=3 on a 30%-of-price
//     ATR coin) cannot produce a long-side trigger price <= 0 that HL would
//     silently reject (review of #505). Returns 0 if pos is nil or
//     EntryATR / AvgCost is missing — the trailing loop will simply no-op
//     until stampEntryATRIfOpened populates the position on the cycle after
//     the open fills.
//
// Mutability: EntryATR is stamped once at position open and never re-read,
// so the EntryATR/AvgCost inputs are fixed for the life of the position.
// However, the TrailingStopATRMult value itself IS hot-reloadable — bumping
// the multiplier mid-position via SIGHUP will alter the derived distance on
// the next trailing cycle. Only the nil↔positive *mode* toggle is blocked
// while open (see config_reload.go's state-compat check). Operators who
// expect a strictly fixed distance for the life of a position should not
// edit the multiplier while a position is active.
func effectiveTrailingStopPct(sc StrategyConfig, pos *Position) float64 {
	if sc.Platform != "hyperliquid" {
		return 0
	}
	switch sc.Type {
	case "perps":
	case "manual":
		// Manual strategies only run the on-chain trailing walker when the
		// close evaluator is trailing_tp_ratchet* (#844). Other manual configs
		// (e.g. tiered_tp_atr_live) keep the historical no-trailing behavior.
		if !strategyUsesTrailingTPRatchetClose(sc) {
			return 0
		}
	default:
		return 0
	}
	// #708: Post-TP trailing transition — `sl_after: trail_from_here` stamps
	// pos.PostTPTrailingATRMult when a TP tier fires. From that point the
	// trailing walker takes over with the stamped distance, even though the
	// strategy itself doesn't have sc.TrailingStop* configured (the validator
	// blocks combining sl_after with strategy-level trailing).
	if pos != nil && pos.PostTPTrailingATRMult != nil && *pos.PostTPTrailingATRMult > 0 {
		if pos.EntryATR <= 0 || pos.AvgCost <= 0 {
			return 0
		}
		pct := *pos.PostTPTrailingATRMult * pos.EntryATR / pos.riskAnchorPrice() * 100.0
		if pct > MaxAutoStopLossPct {
			pct = MaxAutoStopLossPct
		}
		return pct
	}
	if sc.TrailingStopPct != nil {
		if *sc.TrailingStopPct > 0 {
			return *sc.TrailingStopPct
		}
		return 0
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		if pos == nil || pos.EntryATR <= 0 || pos.AvgCost <= 0 {
			return 0
		}
		pct := *sc.TrailingStopATRMult * pos.EntryATR / pos.riskAnchorPrice() * 100.0
		if pct > MaxAutoStopLossPct {
			pct = MaxAutoStopLossPct
		}
		return pct
	}
	// #733: regime-aware trailing distance. Resolved once at first cycle
	// after open against pos.Regime, then frozen for the life of the position
	// (callers re-derive each cycle from the same pos.Regime so it stays
	// invariant — the only way it would change is if a hot-reload pointed
	// the regime block at a different shape, which validateHotReloadStateCompatible
	// blocks while open).
	if sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero() {
		if pos == nil || pos.EntryATR <= 0 || pos.AvgCost <= 0 || positionATRRegimeLabel(pos, sc) == "" {
			return 0
		}
		mult, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, positionATRRegimeLabel(pos, sc))
		if !ok {
			return 0
		}
		pct := mult * pos.EntryATR / pos.riskAnchorPrice() * 100.0
		if pct > MaxAutoStopLossPct {
			pct = MaxAutoStopLossPct
		}
		return pct
	}
	return 0
}

// atrMultMissingEntryATR reports whether sc is configured for ATR-derived
// trailing stops but the open position is missing the EntryATR/AvgCost inputs
// needed to derive a trigger distance. The trailing loop uses this to surface
// a one-shot operator alert when stampEntryATRIfOpened never fired (e.g. the
// open strategy did not emit an "atr" indicator), so the position cannot run
// indefinitely without exchange-side protection (#505 review).
//
// Returns false when an explicit TrailingStopPct > 0 is set alongside
// TrailingStopATRMult — in that case the fixed-pct trailing path arms the
// trigger and the ATR mult is ignored (validation enforces exclusivity, so
// this branch is unreachable for StopLossATRMult strategies).
//
// Includes the fixed-distance StopLossATRMult variant (#562): same EntryATR
// dependency, same alerting story.
func atrMultMissingEntryATR(sc StrategyConfig, pos *Position) bool {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return false
	}
	wantsTrailing := sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0
	wantsFixed := sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0
	// #733: regime-aware SL/trailing have the same EntryATR dependency.
	wantsRegimeFixed := sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero()
	wantsRegimeTrailing := sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero()
	if !wantsTrailing && !wantsFixed && !wantsRegimeFixed && !wantsRegimeTrailing {
		return false
	}
	if sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0 {
		return false
	}
	if pos == nil {
		return false
	}
	return pos.EntryATR <= 0 || pos.AvgCost <= 0
}

// effectiveFixedStopLossATRPct returns the per-position fixed (non-trailing)
// stop loss distance as a price-% derived from StopLossATRMult * EntryATR /
// AvgCost. HL perps only.
//
// Returns 0 when sc is non-HL-perps, StopLossATRMult is nil/<=0, or the
// position is missing EntryATR / AvgCost — the arming step will simply
// no-op until stampEntryATRIfOpened populates the position on the cycle
// after the open fills. The derived price-% is capped at MaxAutoStopLossPct
// to mirror trailing_stop_atr_mult so an extreme volatility window can't
// produce a long-side trigger price <= 0.
//
// Once a position is armed (StopLossTriggerPx > 0), callers should not
// re-derive a new trigger from this helper — the trigger is fixed for the
// life of the position. See hyperliquidArmFixedATRStopLossLive /
// runHyperliquidFixedATRStopLossPaper for the one-shot arming gate.
func effectiveFixedStopLossATRPct(sc StrategyConfig, pos *Position) float64 {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return 0
	}
	mult := 0.0
	if v, ok := unifiedCloseStopLossATR(sc, positionATRRegimeLabel(pos, sc)); ok {
		// #841 2b: unified close owns the per-regime SL distance.
		mult = v
	} else if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		mult = *sc.StopLossATRMult
	} else if sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero() {
		// #733: regime-resolved fixed SL distance. pos.Regime is stamped on
		// the first cycle after open; until then this returns 0 and arming
		// is deferred to the next cycle (same deferral semantics as the
		// scalar variant waiting on EntryATR).
		if pos == nil || positionATRRegimeLabel(pos, sc) == "" {
			return 0
		}
		v, ok := resolveRegimeATR(*sc.StopLossATRRegime, positionATRRegimeLabel(pos, sc))
		if !ok {
			return 0
		}
		mult = v
	}
	if mult <= 0 {
		return 0
	}
	if pos == nil || pos.EntryATR <= 0 || pos.AvgCost <= 0 {
		return 0
	}
	pct := mult * pos.EntryATR / pos.riskAnchorPrice() * 100.0
	if pct > MaxAutoStopLossPct {
		pct = MaxAutoStopLossPct
	}
	return pct
}

// fixedStopLossATRTriggerPx returns the fixed trigger price for a position
// using StopLossATRMult. Long: AvgCost - mult*EntryATR; short: AvgCost +
// mult*EntryATR (clamped via the MaxAutoStopLossPct distance cap).
// Returns 0 if not armable.
func fixedStopLossATRTriggerPx(sc StrategyConfig, side string, pos *Position) float64 {
	pct := effectiveFixedStopLossATRPct(sc, pos)
	if pct <= 0 || pos == nil || pos.AvgCost <= 0 {
		return 0
	}
	// #873: the fixed ATR trigger is anchored to the FROZEN entry
	// (riskAnchorPrice), not the blended AvgCost, so a scale-in never shifts
	// the operator's original stop geometry.
	anchor := pos.riskAnchorPrice()
	switch side {
	case "long":
		return anchor * (1.0 - pct/100.0)
	case "short":
		return anchor * (1.0 + pct/100.0)
	}
	return 0
}

// runHyperliquidFixedATRStopLossPaper arms a fixed (non-trailing) ATR-derived
// stop loss for a paper-mode HL perps position. Mirrors the live arming
// semantics (one-shot placement on the cycle after open) but evaluates breach
// in scheduler state instead of resting an order on Hyperliquid.
//
// Returns:
//
//	newTrigger — non-zero when the trigger should be set on the position
//	             (only on the initial arming cycle; subsequent calls return 0).
//	breach     — true when mark has crossed the existing trigger and the
//	             caller should record a synthetic close.
//	breachPx   — trigger price at which the synthetic close should book.
//
// Multi-strategy / partial-close note: each strategy's StrategyState.Positions
// is isolated, so a single strategy's breach closes only that strategy's
// virtual quantity. Peer strategies on the same coin retain their independent
// virtual exposure.
func runHyperliquidFixedATRStopLossPaper(sc StrategyConfig, side string, pos *Position, mark, currentTrigger float64) (newTrigger float64, breach bool, breachPx float64) {
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult <= 0 {
		return 0, false, 0
	}
	if mark <= 0 {
		return 0, false, 0
	}
	if currentTrigger > 0 {
		if trailingStopBreached(side, mark, currentTrigger) {
			return 0, true, currentTrigger
		}
		return 0, false, 0
	}
	tp := fixedStopLossATRTriggerPx(sc, side, pos)
	if tp <= 0 {
		return 0, false, 0
	}
	return tp, false, 0
}

// hyperliquidArmFixedATRStopLossLive places a fixed (non-trailing) reduce-only
// stop-loss trigger on Hyperliquid for the given position. One-shot: callers
// must skip when pos.StopLossOID > 0 (already armed) or the position is
// missing EntryATR. Returns the StopLossUpdateResult and ok=true on success
// (including when the trigger fills immediately at submit). ok=false signals
// the caller should NOT mutate state.
func hyperliquidArmFixedATRStopLossLive(sc StrategyConfig, symbol, side string, qty float64, triggerPx float64, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidStopLossUpdateResult, bool) {
	if triggerPx <= 0 || qty <= 0 {
		return nil, true
	}
	if logger != nil {
		logger.Info("Arming fixed ATR SL for %s: side=%s qty=%.6f trigger=$%.4f", symbol, side, qty, triggerPx)
	}
	result, stderr, err := runHyperliquidUpdateStopLossFunc(sc.Script, symbol, side, qty, triggerPx, 0)
	if stderr != "" && logger != nil {
		logger.Info("arm fixed SL stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("Fixed ATR SL arm failed: %v", err)
		}
		notifyLiveExecFailure(notifier, sc, "fixed-atr-sl-arm", symbol, err.Error())
		return result, false
	}
	if result.Error != "" {
		if logger != nil {
			logger.Error("Fixed ATR SL arm returned error: %s", result.Error)
		}
		notifyLiveExecFailure(notifier, sc, "fixed-atr-sl-arm", symbol, result.Error)
		return result, false
	}
	if result.StopLossError != "" {
		if isHLOpenOrderCapRejection(result.StopLossError) {
			if logger != nil {
				logger.Error("CRITICAL: HL open-order-cap rejected fixed ATR SL arm for %s - position is unprotected: %s",
					symbol, result.StopLossError)
			}
			if notifier != nil && notifier.HasBackends() {
				msg := fmt.Sprintf("**HL OPEN-ORDER CAP HIT** [%s] %s fixed ATR SL arm rejected: %s",
					sc.ID, symbol, result.StopLossError)
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}
		} else if logger != nil {
			logger.Warn("Fixed ATR SL arm placement failed (non-fatal): %s", result.StopLossError)
		}
	}
	if result.StopLossFilledImmediately && logger != nil {
		logger.Warn("Fixed ATR SL trigger filled at submit for %s — position is flat on-chain", symbol)
	}
	return result, true
}

// atrMultMissingEntryATRWarned throttles missing-EntryATR alerts to one per
// (strategy, symbol). Keys are reset by clearATRMultMissingEntryATRWarning
// when a position closes (so a future re-open can re-warn if the bug
// persists) and on hot-reload when the strategy disables ATR-mult.
var atrMultMissingEntryATRWarned sync.Map

func atrMultMissingEntryATRKey(strategyID, symbol string) string {
	return strategyID + ":" + symbol
}

// notifyATRMultMissingEntryATROnce emits a WARN log + notifier alert the
// first time we observe an ATR-mult-configured strategy with a position that
// lacks the EntryATR input. Repeated cycles for the same (strategy, symbol)
// are suppressed so the alert channel is not flooded; downstream operators
// see a single, clear notice that the position is running without
// exchange-side protection.
func notifyATRMultMissingEntryATROnce(sc StrategyConfig, symbol string, notifier *MultiNotifier, logger *StrategyLogger) {
	key := atrMultMissingEntryATRKey(sc.ID, symbol)
	if _, loaded := atrMultMissingEntryATRWarned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if logger != nil {
		logger.Warn("trailing_stop_atr_mult set but Position.EntryATR is 0 for %s — entry strategy must emit an 'atr' indicator on the open candle, so no ATR-derived trigger has been armed for this strategy.", symbol)
	}
	if notifier != nil && notifier.HasBackends() {
		msg := fmt.Sprintf("**HL TRAILING ATR-MULT MISSING ENTRY ATR** [%s] %s — strategy is configured with trailing_stop_atr_mult but the open candle did not produce an ATR indicator, so no ATR-derived trigger has been armed for this strategy. Verify the entry strategy emits `atr`, or switch to a fixed `trailing_stop_pct`. (If a peer strategy on the same coin owns the trigger, this strategy is still covered by the shared exchange-side stop.)",
			sc.ID, symbol)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

// clearATRMultMissingEntryATRWarning drops the throttle key for a
// (strategy, symbol) so the next missing-EntryATR observation re-warns.
// Callers should invoke this on position close so a future re-open that
// hits the same missing-ATR bug is not silently suppressed.
func clearATRMultMissingEntryATRWarning(strategyID, symbol string) {
	atrMultMissingEntryATRWarned.Delete(atrMultMissingEntryATRKey(strategyID, symbol))
}

// clearATRMultMissingEntryATRWarningOnHLPerpsClose is a no-op shortcut for
// non-HL-perps state. Position-close call sites in shared code (e.g.
// ExecutePerpsSignal close-long/short, forceCloseAllPositions) live on a
// path that may run for spot or futures strategies as well; this helper
// avoids spraying platform/type checks at every call site.
func clearATRMultMissingEntryATRWarningOnHLPerpsClose(s *StrategyState, symbol string) {
	if s == nil || s.Platform != "hyperliquid" || s.Type != "perps" {
		return
	}
	clearATRMultMissingEntryATRWarning(s.ID, symbol)
}

// clearATRMultMissingEntryATRWarningsForStrategy drops every throttle key
// belonging to strategyID. Used by hot-reload when the operator disables
// trailing_stop_atr_mult — the throttle should not survive into the next
// configuration regime, since the alert logic no longer applies.
func clearATRMultMissingEntryATRWarningsForStrategy(strategyID string) {
	prefix := strategyID + ":"
	atrMultMissingEntryATRWarned.Range(func(k, _ any) bool {
		key, ok := k.(string)
		if !ok {
			return true
		}
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			atrMultMissingEntryATRWarned.Delete(k)
		}
		return true
	})
}

// tieredTPATRMissingEntryATR reports whether sc is configured with tiered_tp_atr
// as a close strategy but the open position has no EntryATR stamped yet. Unlike
// the ATR-mult trailing check this is platform-agnostic: tiered_tp_atr runs on
// any platform that supports composed close strategies.
func tieredTPATRMissingEntryATR(sc StrategyConfig, pos *Position) bool {
	hasTieredTP := false
	for _, ref := range sc.closeRefs() {
		if isTieredTPATRCloseName(ref.Name) {
			hasTieredTP = true
			break
		}
	}
	if !hasTieredTP {
		return false
	}
	if pos == nil {
		return false
	}
	return pos.EntryATR <= 0 && pos.AvgCost > 0
}

// notifyTieredTPATRMissingEntryATROnce emits a WARN log + notifier alert the
// first time a tiered_tp_atr close strategy is observed on a position that
// has no EntryATR. Uses the same throttle map as the ATR-mult trailing alert
// so a single key per (strategy, symbol) suppresses both variants.
func notifyTieredTPATRMissingEntryATROnce(sc StrategyConfig, symbol string, notifier *MultiNotifier, logger *StrategyLogger) {
	key := atrMultMissingEntryATRKey(sc.ID, symbol)
	if _, loaded := atrMultMissingEntryATRWarned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	if logger != nil {
		logger.Warn("tiered_tp_atr configured but Position.EntryATR is 0 for %s — the open strategy must emit an 'atr' indicator on the open candle; take-profit tiers will noop until EntryATR is stamped.", symbol)
	}
	if notifier != nil && notifier.HasBackends() {
		msg := fmt.Sprintf("**MISSING ENTRY ATR** [%s] %s — close strategy `tiered_tp_atr` is configured but the open candle did not produce an ATR indicator, so take-profit tiers are disabled until EntryATR is stamped. Ensure the entry strategy emits `atr` in its indicator output.",
			sc.ID, symbol)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}

func effectiveTrailingStopMinMovePct(sc StrategyConfig) float64 {
	if sc.TrailingStopMinMovePct != nil && *sc.TrailingStopMinMovePct >= 0 {
		return *sc.TrailingStopMinMovePct
	}
	return defaultTrailingStopMinMovePct
}

func computeTrailingStopUpdate(side string, mark, highWater, trailingPct, minMovePct, currentTrigger float64) (float64, float64, bool) {
	if mark <= 0 || trailingPct <= 0 {
		return highWater, 0, false
	}
	if highWater <= 0 {
		highWater = mark
	}

	candidateHighWater := highWater
	switch side {
	case "long":
		if mark > candidateHighWater {
			candidateHighWater = mark
		}
	case "short":
		if mark < candidateHighWater {
			candidateHighWater = mark
		}
	default:
		return highWater, 0, false
	}
	if candidateHighWater <= 0 {
		return highWater, 0, false
	}

	var candidateTrigger float64
	switch side {
	case "long":
		candidateTrigger = candidateHighWater * (1.0 - trailingPct/100.0)
	case "short":
		candidateTrigger = candidateHighWater * (1.0 + trailingPct/100.0)
	}
	if candidateTrigger <= 0 {
		return candidateHighWater, 0, false
	}
	if currentTrigger <= 0 {
		return candidateHighWater, candidateTrigger, true
	}

	favorable := (side == "long" && candidateTrigger > currentTrigger) ||
		(side == "short" && candidateTrigger < currentTrigger)
	if !favorable {
		return candidateHighWater, 0, false
	}
	movePct := math.Abs(candidateTrigger-currentTrigger) / currentTrigger * 100.0
	if movePct >= minMovePct {
		return candidateHighWater, candidateTrigger, true
	}
	return candidateHighWater, 0, false
}

// trailingStopBreached reports whether mark has crossed the unfavorable side
// of currentTrigger for a position with the given side. Returns false when
// currentTrigger <= 0 (no trigger armed yet — initial cycles set one up
// before any breach can occur) or mark <= 0. Side must be "long" or "short".
//
// Used by the paper-mode trailing-stop loop to evaluate breaches in scheduler
// state (live mode delegates breach evaluation to the exchange trigger order).
func trailingStopBreached(side string, mark, currentTrigger float64) bool {
	if mark <= 0 || currentTrigger <= 0 {
		return false
	}
	switch side {
	case "long":
		return mark <= currentTrigger
	case "short":
		return mark >= currentTrigger
	}
	return false
}

// runHyperliquidTrailingStopPaper computes the per-cycle trailing-stop
// decision for a paper-mode HL perps position. Mirrors the live path's
// semantics (effectiveTrailingStopPct distance, side-aware high-water,
// min-move debounce on trigger replacement) but evaluates breaches in
// scheduler state instead of resting an order on Hyperliquid.
//
// Decision order:
//   - If the existing trigger has been breached by the current mark, signal
//     a synthetic close at the trigger price. The mark/trigger spread within
//     a single cycle is treated as exchange-trigger semantics (fill at the
//     trigger price) so paper PnL matches live behavior on a normal fill.
//   - Otherwise, advance the high-water mark and (when the favorable move
//     clears the min-move debounce) emit a new trigger price for the caller
//     to persist on Position.StopLossTriggerPx.
//
// Returns:
//
//	newHighWater — the (possibly advanced) high-water mark to persist.
//	newTrigger   — non-zero only when the trigger should be replaced.
//	breach       — true when the caller should record a synthetic close.
//	breachPx     — trigger price at which the synthetic close should be booked.
//
// Multi-strategy / partial-close note: each strategy's StrategyState.Positions
// is isolated in scheduler state, so a single strategy's breach closes only
// that strategy's virtual quantity. Peer strategies on the same coin retain
// their independent virtual exposure and run their own trailing loops.
func runHyperliquidTrailingStopPaper(sc StrategyConfig, side string, pos *Position, mark, highWater, currentTrigger float64) (newHighWater, newTrigger float64, breach bool, breachPx float64) {
	trailingPct := effectiveTrailingStopPct(sc, pos)
	if trailingPct <= 0 || mark <= 0 {
		return highWater, 0, false, 0
	}
	if trailingStopBreached(side, mark, currentTrigger) {
		return highWater, 0, true, currentTrigger
	}
	avgCost := 0.0
	if pos != nil {
		// #873: seed the trailing high-water from the FROZEN entry so a
		// scale-in before the first favorable move doesn't reset the trail
		// to the blended average.
		avgCost = pos.riskAnchorPrice()
	}
	if highWater <= 0 {
		highWater = avgCost
	}
	nhw, nt, replace := computeTrailingStopUpdate(side, mark, highWater, trailingPct, effectiveTrailingStopMinMovePct(sc), currentTrigger)
	if replace {
		return nhw, nt, false, 0
	}
	return nhw, 0, false, 0
}

// applyTrailingStopUpdateResult applies a runHyperliquidTrailingStopUpdate
// outcome to the live position. The caller MUST hold the state write lock.
// Both the perps and the manual trailing_tp_ratchet dispatches route through
// this single helper so they can never diverge on the three slUpdate outcomes:
//
//  1. immediate fill — the replacement trigger filled on placement; book a
//     "trailing_stop_loss_immediate" close now (returns immediateFill=true with
//     the fill price) instead of leaving it for a later reconcile to pick up as
//     a delayed, mislabeled hl_sync_external close;
//  2. resting replacement — update the position's OID + trigger;
//  3. cancel-without-rest — the old OID was cancelled but no replacement
//     rested; clear the stale OID/trigger.
//
// expectedSide guards against a side flip between snapshot and lock; prevSLOID
// is the OID captured before the update (used to confirm the cancel applies to
// the OID we expected to replace).
func applyTrailingStopUpdateResult(s *StrategyState, symbol, expectedSide string, prevSLOID int64, newHighWater float64, updateConfirmed bool, slUpdate *HyperliquidStopLossUpdateResult, logger *StrategyLogger) (immediateFill bool, fillPx float64) {
	if s == nil {
		return false, 0
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 {
		return false, 0
	}
	if expectedSide != "" && pos.Side != expectedSide {
		return false, 0
	}
	if newHighWater > 0 && updateConfirmed {
		pos.StopLossHighWaterPx = newHighWater
	}
	if slUpdate == nil {
		return false, 0
	}
	switch {
	case slUpdate.StopLossFilledImmediately && slUpdate.StopLossTriggerPx > 0:
		if recordPerpsStopLossClose(s, symbol, slUpdate.StopLossTriggerPx, "trailing_stop_loss_immediate", logger) {
			return true, slUpdate.StopLossTriggerPx
		}
	case slUpdate.StopLossOID > 0:
		pos.StopLossOID = slUpdate.StopLossOID
		pos.StopLossTriggerPx = slUpdate.StopLossTriggerPx
		if logger != nil {
			logger.Info("Trailing SL trigger updated oid=%d @ $%.4f", slUpdate.StopLossOID, slUpdate.StopLossTriggerPx)
		}
	case slUpdate.CancelStopLossSucceeded && prevSLOID > 0 && pos.StopLossOID == prevSLOID:
		pos.StopLossOID = 0
		pos.StopLossTriggerPx = 0
		if logger != nil {
			logger.Warn("Trailing SL old OID=%d was cancelled but replacement did not rest", prevSLOID)
		}
	}
	return false, 0
}

// runHyperliquidTrailingStopUpdate evaluates the per-cycle trailing-stop
// update for an HL perps position. pos is the caller's snapshot of the
// position fields needed for trailing math (AvgCost, EntryATR — held
// outside the state mutex so the subprocess call below can run without
// blocking other strategies). The pointer is taken by value semantics; the
// helper only reads, never writes through it.
func runHyperliquidTrailingStopUpdate(sc StrategyConfig, symbol, side string, qty float64, pos *Position, mark, highWater, currentTrigger float64, currentOID int64, forceResize bool, notifier *MultiNotifier, logger *StrategyLogger) (float64, *HyperliquidStopLossUpdateResult, bool) {
	trailingPct := effectiveTrailingStopPct(sc, pos)
	if trailingPct <= 0 || qty <= 0 || mark <= 0 {
		return highWater, nil, true
	}
	avgCost := 0.0
	if pos != nil {
		// #873: seed the trailing high-water from the FROZEN entry so a
		// scale-in before the first favorable move doesn't reset the trail
		// to the blended average.
		avgCost = pos.riskAnchorPrice()
	}
	if highWater <= 0 {
		highWater = avgCost
	}
	newHighWater, newTrigger, replace := computeTrailingStopUpdate(side, mark, highWater, trailingPct, effectiveTrailingStopMinMovePct(sc), currentTrigger)
	if forceResize && !replace {
		// #873: a scale-in grew the position; the resting trailing SL still
		// covers only the pre-add size. Force a cancel+replace at the EXISTING
		// trigger so the reduce-only SL covers the new total. Keep the current
		// trigger price (no trailing move yet); fall through to the computed
		// trigger when nothing is resting (currentTrigger==0).
		replace = true
		if currentTrigger > 0 {
			newTrigger = currentTrigger
		}
	}
	if !replace {
		return newHighWater, nil, true
	}

	logger.Info("Updating trailing SL for %s: side=%s mark=$%.4f high_water=$%.4f trigger=$%.4f cancel_oid=%d",
		symbol, side, mark, newHighWater, newTrigger, currentOID)
	result, stderr, err := runHyperliquidUpdateStopLossFunc(sc.Script, symbol, side, qty, newTrigger, currentOID)
	if stderr != "" {
		logger.Info("update stop-loss stderr: %s", stderr)
	}
	if err != nil {
		logger.Error("Trailing SL update failed: %v", err)
		return newHighWater, result, false
	}
	if result.Error != "" {
		logger.Error("Trailing SL update returned error: %s", result.Error)
		return newHighWater, result, false
	}
	if result.CancelStopLossError != "" {
		logger.Warn("Trailing SL cancel failed (non-fatal): %s", result.CancelStopLossError)
		if result.StopLossOID > 0 && currentOID > 0 && notifier != nil && notifier.HasBackends() {
			msg := fmt.Sprintf("**HL TRAILING SL CANCEL FAILED** [%s] %s old trigger OID %d may still be resting while new trigger OID %d was placed. Check Hyperliquid open triggers before they accumulate toward the account cap. Error: %s",
				sc.ID, symbol, currentOID, result.StopLossOID, result.CancelStopLossError)
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
	}
	if result.StopLossError != "" {
		if isHLOpenOrderCapRejection(result.StopLossError) {
			logger.Error("CRITICAL: HL open-order-cap rejected trailing SL update for %s - position may be under-protected: %s",
				symbol, result.StopLossError)
			if notifier != nil && notifier.HasBackends() {
				msg := fmt.Sprintf("**HL OPEN-ORDER CAP HIT** [%s] %s trailing SL update rejected: %s",
					sc.ID, symbol, result.StopLossError)
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}
		} else {
			logger.Warn("Trailing SL placement failed (non-fatal): %s", result.StopLossError)
		}
	}
	if result.StopLossFilledImmediately {
		logger.Warn("Trailing SL trigger filled at submit for %s — position is flat on-chain", symbol)
	}
	return newHighWater, result, true
}
