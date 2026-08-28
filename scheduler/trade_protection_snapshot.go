package main

import (
	"encoding/json"
	"fmt"
)

func tradeOpenStopLossATRMult(sc StrategyConfig) *float64 {
	return tradeOpenStopLossATRMultForRegime(sc, "")
}

func tradeOpenStopLossATRMultForRegime(sc StrategyConfig, regime string) *float64 {
	if v, ok := unifiedCloseStopLossATR(sc, regime); ok {
		return &v
	}
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

func tradeOpenTPTiersJSON(sc StrategyConfig) string {
	return tradeOpenTPTiersJSONForRegime(sc, "")
}

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
		fmt.Printf("[WARN] %s: marshal tp_tiers_json failed: %v\n", sc.ID, err)
		return ""
	}
	return string(b)
}

func stampPositionProtectionSnapshot(pos *Position, sc StrategyConfig) {
	if pos == nil {
		return
	}
	if pos.StopLossATRMult == nil {
		pos.StopLossATRMult = tradeOpenStopLossATRMultForRegime(sc, positionATRRegimeLabel(pos, sc))
	}
	if pos.TPTiersJSON == "" {
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
	if pos != nil && !pos.isHedgeLeg() {
		decisionType := ReplayDecisionOpen
		if pos.Quantity > trade.Quantity+1e-9 {
			decisionType = ReplayDecisionScaleIn
		}
		recordReplayDecision(s, decisionType, trade.Symbol, pos.Side, trade.Quantity, trade.Price, "", trade.Timestamp, pos.EntryATR, pos.Regime)
	}
	return true
}

func stampOpenTradeWithProtectionSnapshot(s *StrategyState, db *StateDB, sc StrategyConfig, symbol string, pos *Position) {
	stampPositionProtectionSnapshot(pos, sc)
	stampOpenTradeFromPosition(s, db, symbol, pos)
}
