package main

import (
	"encoding/json"
	"fmt"
)

// tradeOpenStopLossATRMult returns the SL ATR multiplier resolved at trade-open
// time, or nil when SL was armed via a non-ATR mechanism (StopLossPct,
// StopLossMarginPct, TrailingStopPct) or no SL at all (#669). Nullness alone
// gates the SL display suffix correctly — a back-computed mult on a pct-armed
// SL position is now distinguishable from a true ATR-armed mult.
func tradeOpenStopLossATRMult(sc StrategyConfig) *float64 {
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		v := *sc.StopLossATRMult
		return &v
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		v := *sc.TrailingStopATRMult
		return &v
	}
	return nil
}

// tradeOpenTPTiersJSON returns a JSON snapshot of the configured ATR-tiered TP
// plan resolved at trade-open time, or "" when the strategy doesn't use
// tiered_tp_atr* (#669). Storing the snapshot at fill time means subsequent
// tier-config edits don't lose the historical record needed for analytics.
func tradeOpenTPTiersJSON(sc StrategyConfig) string {
	tiers := hyperliquidProtectionTiers(sc)
	if len(tiers) == 0 {
		return ""
	}
	type tierJSON struct {
		ATRMultiple   float64 `json:"atr_multiple"`
		CloseFraction float64 `json:"close_fraction"`
	}
	out := make([]tierJSON, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, tierJSON{ATRMultiple: t.Multiple, CloseFraction: t.Fraction})
	}
	b, err := json.Marshal(out)
	if err != nil {
		// Tier values are floats produced by hyperliquidProtectionTiers; this
		// path is unreachable in practice but log so a regression is visible.
		fmt.Printf("[WARN] %s: marshal tp_tiers_json failed: %v\n", sc.ID, err)
		return ""
	}
	return string(b)
}

// stampPositionProtectionSnapshot writes the SL ATR mult + TP tier snapshot
// derived from sc onto the Position so subsequent calls to
// stampOpenTradeFromPosition can backfill the trade row. Idempotent — only
// fills nil/empty fields, leaving any pre-existing fill-time snapshot intact.
func stampPositionProtectionSnapshot(pos *Position, sc StrategyConfig) {
	if pos == nil {
		return
	}
	if pos.StopLossATRMult == nil {
		pos.StopLossATRMult = tradeOpenStopLossATRMult(sc)
	}
	if pos.TPTiersJSON == "" {
		pos.TPTiersJSON = tradeOpenTPTiersJSON(sc)
	}
}

// stampOpenTradeWithProtectionSnapshot is the trade-open helper that combines
// the protection-config snapshot (sc-derived) with the position-derived backfill
// onto the most recent open Trade for symbol. Idempotent on both layers — call
// it after every Execute*Signal trade-open path so analytics rows always carry
// the SL arming method + TP tier snapshot resolved at fill time (#669).
func stampOpenTradeWithProtectionSnapshot(s *StrategyState, db *StateDB, sc StrategyConfig, symbol string, pos *Position) {
	stampPositionProtectionSnapshot(pos, sc)
	stampOpenTradeFromPosition(s, db, symbol, pos)
}
