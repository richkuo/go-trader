package main

import (
	"fmt"
	"math"
	"sync"
)

const defaultTrailingStopMinMovePct = 0.5

var runHyperliquidUpdateStopLossFunc = RunHyperliquidUpdateStopLoss

// effectiveTrailingStopPct returns the per-position trailing-stop distance as a
// price-% (e.g. 3.0 == 3%). HL perps only.
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
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return 0
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
		pct := *sc.TrailingStopATRMult * pos.EntryATR / pos.AvgCost * 100.0
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
// Returns false when an explicit TrailingStopPct > 0 takes precedence over the
// ATR multiplier — in that case the fixed-pct path arms the trigger and ATR
// is irrelevant.
func atrMultMissingEntryATR(sc StrategyConfig, pos *Position) bool {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return false
	}
	if sc.TrailingStopATRMult == nil || *sc.TrailingStopATRMult <= 0 {
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
	for _, cs := range sc.CloseStrategies {
		if cs == "tiered_tp_atr" {
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

// runHyperliquidTrailingStopUpdate evaluates the per-cycle trailing-stop
// update for an HL perps position. pos is the caller's snapshot of the
// position fields needed for trailing math (AvgCost, EntryATR — held
// outside the state mutex so the subprocess call below can run without
// blocking other strategies). The pointer is taken by value semantics; the
// helper only reads, never writes through it.
func runHyperliquidTrailingStopUpdate(sc StrategyConfig, symbol, side string, qty float64, pos *Position, mark, highWater, currentTrigger float64, currentOID int64, notifier *MultiNotifier, logger *StrategyLogger) (float64, *HyperliquidStopLossUpdateResult, bool) {
	trailingPct := effectiveTrailingStopPct(sc, pos)
	if trailingPct <= 0 || qty <= 0 || mark <= 0 {
		return highWater, nil, true
	}
	avgCost := 0.0
	if pos != nil {
		avgCost = pos.AvgCost
	}
	if highWater <= 0 {
		highWater = avgCost
	}
	newHighWater, newTrigger, replace := computeTrailingStopUpdate(side, mark, highWater, trailingPct, effectiveTrailingStopMinMovePct(sc), currentTrigger)
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
