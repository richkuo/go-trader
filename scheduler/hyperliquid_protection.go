package main

import (
	"fmt"
	"sort"
)

type hlProtectionPlan struct {
	Symbol          string
	Side            string
	Size            float64
	AvgCost         float64
	EntryATR        float64
	StopLossATRMult float64
	TP1Mult         float64
	TP1Fraction     float64
	TP2Mult         float64
	StopLossOID     int64
	TP1OID          int64
	TP2OID          int64
}

func buildHyperliquidProtectionPlan(sc StrategyConfig, pos *Position) (hlProtectionPlan, bool) {
	if sc.Type != "perps" || sc.Platform != "hyperliquid" || pos == nil {
		return hlProtectionPlan{}, false
	}
	if pos.Symbol == "" || pos.Quantity <= 0 || pos.AvgCost <= 0 || pos.EntryATR <= 0 {
		return hlProtectionPlan{}, false
	}
	if pos.Side != "long" && pos.Side != "short" {
		return hlProtectionPlan{}, false
	}
	slMult := 0.0
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		slMult = *sc.StopLossATRMult
	}
	tp1Mult, tp1Fraction, tp2Mult := hyperliquidProtectionTiers(sc)
	if slMult <= 0 && (tp1Mult <= 0 || tp1Fraction <= 0 || tp2Mult <= 0) {
		return hlProtectionPlan{}, false
	}
	return hlProtectionPlan{
		Symbol:          pos.Symbol,
		Side:            pos.Side,
		Size:            pos.Quantity,
		AvgCost:         pos.AvgCost,
		EntryATR:        pos.EntryATR,
		StopLossATRMult: slMult,
		TP1Mult:         tp1Mult,
		TP1Fraction:     tp1Fraction,
		TP2Mult:         tp2Mult,
		StopLossOID:     pos.StopLossOID,
		TP1OID:          pos.TP1OID,
		TP2OID:          pos.TP2OID,
	}, true
}

func hyperliquidProtectionTiers(sc StrategyConfig) (float64, float64, float64) {
	if !strategyUsesTieredTPATRClose(sc) {
		return 0, 0, 0
	}
	tiers := parseHLProtectionTiers(sc.Params["tiers"])
	if len(tiers) == 0 {
		tiers = []hlProtectionTier{
			{Multiple: 1, Fraction: 0.5},
			{Multiple: 2, Fraction: 1},
		}
	}
	if len(tiers) < 2 {
		return 0, 0, 0
	}
	tp1 := tiers[0]
	tp2 := tiers[1]
	if tp1.Multiple <= 0 || tp1.Fraction <= 0 || tp2.Multiple <= 0 || tp2.Fraction <= tp1.Fraction {
		return 0, 0, 0
	}
	return tp1.Multiple, tp1.Fraction, tp2.Multiple
}

type hlProtectionTier struct {
	Multiple float64
	Fraction float64
}

func parseHLProtectionTiers(raw interface{}) []hlProtectionTier {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	tiers := make([]hlProtectionTier, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		multiple := floatFromAny(firstPresent(m, "atr_multiple", "multiple"))
		fraction := floatFromAny(firstPresent(m, "close_fraction", "fraction"))
		if multiple <= 0 || fraction <= 0 {
			continue
		}
		if fraction > 1 {
			fraction = 1
		}
		tiers = append(tiers, hlProtectionTier{Multiple: multiple, Fraction: fraction})
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Multiple < tiers[j].Multiple })
	return tiers
}

func firstPresent(m map[string]interface{}, keys ...string) interface{} {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return nil
}

func floatFromAny(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case jsonNumber:
		f, _ := x.Float64()
		return f
	default:
		return 0
	}
}

type jsonNumber interface {
	Float64() (float64, error)
}

func syncHyperliquidProtection(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger) (*HyperliquidProtectionSyncResult, bool) {
	result, stderr, err := RunHyperliquidSyncProtection(
		sc.Script, plan.Symbol, plan.Side, plan.Size, plan.AvgCost, plan.EntryATR,
		plan.StopLossATRMult, plan.TP1Mult, plan.TP1Fraction, plan.TP2Mult,
		plan.StopLossOID, plan.TP1OID, plan.TP2OID,
	)
	if stderr != "" && logger != nil {
		logger.Info("protection sync stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("HL protection sync failed: %v", err)
		}
		notifyHLProtectionFailure(notifier, sc, plan.Symbol, err.Error())
		return result, false
	}
	if result == nil {
		return nil, false
	}
	if result.Error != "" {
		if logger != nil {
			logger.Error("HL protection sync returned error: %s", result.Error)
		}
		notifyHLProtectionFailure(notifier, sc, plan.Symbol, result.Error)
		return result, false
	}
	var warnings []string
	if result.StopLossError != "" {
		warnings = append(warnings, "SL: "+result.StopLossError)
	}
	if result.TP1Error != "" {
		warnings = append(warnings, "TP1: "+result.TP1Error)
	}
	if result.TP2Error != "" {
		warnings = append(warnings, "TP2: "+result.TP2Error)
	}
	if len(warnings) > 0 {
		msg := fmt.Sprintf("%s %s protection partially failed: %v", sc.ID, plan.Symbol, warnings)
		if logger != nil {
			logger.Warn("%s", msg)
		}
		notifyHLProtectionFailure(notifier, sc, plan.Symbol, msg)
	}
	return result, true
}

func applyHyperliquidProtectionSync(pos *Position, result *HyperliquidProtectionSyncResult) {
	if pos == nil || result == nil {
		return
	}
	if result.StopLossOID > 0 {
		pos.StopLossOID = result.StopLossOID
	}
	if result.StopLossTriggerPx > 0 {
		pos.StopLossTriggerPx = result.StopLossTriggerPx
	}
	if result.TP1OID > 0 {
		pos.TP1OID = result.TP1OID
	}
	if result.TP2OID > 0 {
		pos.TP2OID = result.TP2OID
	}
}

func notifyHLProtectionFailure(notifier *MultiNotifier, sc StrategyConfig, symbol, reason string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := fmt.Sprintf("**HL PROTECTION WARNING** [%s] %s reduce-only SL/TP sync failed: %s", sc.ID, symbol, reason)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}
