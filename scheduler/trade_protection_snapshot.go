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
//
// Regime-aware variants (#733) return a regime-resolved multiplier when a
// position regime is supplied; without one, they return nil because the
// final multiplier depends on the position's stamped regime label.
func tradeOpenStopLossATRMult(sc StrategyConfig) *float64 {
	return tradeOpenStopLossATRMultForRegime(sc, "")
}

func tradeOpenStopLossATRMultForRegime(sc StrategyConfig, regime string) *float64 {
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		v := *sc.StopLossATRMult
		return &v
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		v := *sc.TrailingStopATRMult
		return &v
	}
	if sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero() && regime != "" {
		if v, ok := resolveRegimeATR(*sc.StopLossATRRegime, regime); ok {
			return &v
		}
	}
	if sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero() && regime != "" {
		if v, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, regime); ok {
			return &v
		}
	}
	return nil
}

// tradeOpenTPTiersJSON returns a JSON snapshot of the configured ATR-tiered TP
// plan resolved at trade-open time, or "" when the strategy doesn't use
// tiered_tp_atr* (#669). Storing the snapshot at fill time means subsequent
// tier-config edits don't lose the historical record needed for analytics.
func tradeOpenTPTiersJSON(sc StrategyConfig) string {
	return tradeOpenTPTiersJSONForRegime(sc, "")
}

// tradeOpenTPTiersJSONForRegime is the regime-aware variant — resolves
// regime-tier multipliers via the position's stamped regime label.
// Empty regime → scalar tiered_tp_atr* still snapshots fine; regime-aware
// variants return "" until pos.Regime is populated and we re-stamp on the
// next protection-sync cycle (#733).
func tradeOpenTPTiersJSONForRegime(sc StrategyConfig, regime string) string {
	tiers := strategyTPTiersForRegime(sc, regime)
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
		// Tier values are floats produced by strategyTPTiers; this
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
		pos.StopLossATRMult = tradeOpenStopLossATRMultForRegime(sc, positionATRRegimeLabel(pos, sc))
	}
	if pos.TPTiersJSON == "" {
		// strategyTPTiersForRegime resolves regime-aware tier multipliers from
		// the position's stamped regime. Empty regime → falls back to the
		// raw scalar tiers when present (legacy tiered_tp_atr); regime-aware
		// strategies without a stamped regime yet write an empty snapshot
		// here and will be re-stamped on the next protection-sync cycle.
		pos.TPTiersJSON = tradeOpenTPTiersJSONForRegime(sc, positionATRRegimeLabel(pos, sc))
	}
}

func copyPositionOpenSnapshotToTrade(trade *Trade, pos *Position) {
	if trade == nil || pos == nil {
		return
	}
	trade.EntryATR = pos.EntryATR
	trade.StopLossOID = pos.StopLossOID
	trade.StopLossTriggerPx = pos.StopLossTriggerPx
	trade.TPOIDs = cloneInt64s(pos.TPOIDs)
	if pos.StopLossATRMult != nil {
		v := *pos.StopLossATRMult
		trade.StopLossATRMult = &v
	} else {
		trade.StopLossATRMult = nil
	}
	trade.TPTiersJSON = pos.TPTiersJSON
}

func recordPositionOpen(s *StrategyState, sc StrategyConfig, trade *Trade, pos *Position) bool {
	if s == nil || trade == nil {
		return false
	}
	stampPositionProtectionSnapshot(pos, sc)
	copyPositionOpenSnapshotToTrade(trade, pos)
	RecordTrade(s, *trade)
	return true
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
