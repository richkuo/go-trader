package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type hlProtectionPlan struct {
	Symbol          string
	Side            string
	Size            float64
	AvgCost         float64
	EntryATR        float64
	StopLossATRMult float64
	StopLossOID     int64
	Tiers           []hlProtectionTier
	TPOIDs          []int64
	TPArmedTiers    []bool
	ForceSLReplace  bool
	ForceTPReplace  []bool
	CancelTPOIDs    []int64
}

func buildHyperliquidProtectionPlan(sc StrategyConfig, pos *Position, liquidationPx float64) (hlProtectionPlan, bool) {
	if (sc.Type != "perps" && sc.Type != "manual") || sc.Platform != "hyperliquid" || pos == nil {
		return hlProtectionPlan{}, false
	}
	if pos.Symbol == "" || pos.Quantity <= 0 || pos.AvgCost <= 0 || pos.EntryATR <= 0 {
		return hlProtectionPlan{}, false
	}
	if pos.Side != "long" && pos.Side != "short" {
		return hlProtectionPlan{}, false
	}
	atrRegime := protectionATRRegimeLabel(pos, sc)
	slMult := 0.0
	if v, ok := unifiedCloseStopLossATR(sc, atrRegime); ok {
		slMult = v
	} else if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		slMult = *sc.StopLossATRMult
	} else if sc.StopLossATRMultRegime != nil && !sc.StopLossATRMultRegime.IsZero() {
		if v, ok := resolveRegimeATR(*sc.StopLossATRMultRegime, positionATRRegimeLabel(pos, sc)); ok {
			slMult = v
		}
	}
	tiers := strategyTPTiersForRegime(sc, atrRegime)
	if slMult <= 0 && len(tiers) == 0 {
		return hlProtectionPlan{}, false
	}
	forceSLPastLiquidation := false
	if liquidationPx > 0 {
		if clampedMult, clamped := hlClampProtectionSLMult(pos.Side, pos.riskAnchorPrice(), pos.EntryATR, slMult, liquidationPx); clamped {
			slMult = clampedMult
			if clampedPx := hlProtectionSLTriggerPx(pos.Side, pos.riskAnchorPrice(), pos.EntryATR, slMult); hlTriggerStrictlyTighter(pos.Side, clampedPx, pos.StopLossTriggerPx) {
				forceSLPastLiquidation = true
			}
		}
		if stopPastLiquidation(pos.Side, pos.StopLossTriggerPx, liquidationPx) &&
			hlProtectionSLTriggerReachable(pos.Side, pos.riskAnchorPrice(), pos.EntryATR, slMult, liquidationPx) {
			forceSLPastLiquidation = true
		}
	}
	if pos.StopLossOID == 0 && pos.StopLossTriggerPx > 0 {
		slMult = 0
		forceSLPastLiquidation = false
	}
	tierCount := len(tiers)
	return hlProtectionPlan{
		ForceSLReplace:  forceSLPastLiquidation,
		Symbol:          pos.Symbol,
		Side:            pos.Side,
		Size:            pos.Quantity,
		AvgCost:         pos.riskAnchorPrice(),
		EntryATR:        pos.EntryATR,
		StopLossATRMult: slMult,
		StopLossOID:     pos.StopLossOID,
		Tiers:           tiers,
		TPOIDs:          tpOIDsForTierCount(pos.TPOIDs, tierCount),
		TPArmedTiers:    tpArmedTiersForTierCount(pos.TPArmedTiers, tierCount),
	}, true
}

func strategyTPTiers(sc StrategyConfig) []hlProtectionTier {
	return strategyTPTiersForRegime(sc, "")
}

func strategyTPTiersForRegime(sc StrategyConfig, regime string) []hlProtectionTier {
	if !strategyUsesTieredTPATRClose(sc) {
		return nil
	}
	var raw interface{}
	regimeAware := false
	for _, ref := range sc.closeRefs() {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if !isTieredTPATRCloseName(n) {
			continue
		}
		if n == "tiered_tp_atr_regime" || n == "tiered_tp_atr_live_regime" || n == dynamicCloseStrategyName {
			regimeAware = true
		}
		if regimeAware && closeParamsAreUnifiedRegime(ref.Params) {
			scalar, _, ok := unifiedRegimeScalarParams(ref.Params, regime)
			if !ok {
				return nil
			}
			sel, _ := closeTierListParam(scalar)
			tiers := parseHLProtectionTiers(sel)
			if len(tiers) < 2 {
				return nil
			}
			return finalizeProtectionTiers(tiers)
		}
		if v, ok := closeTierListParam(ref.Params); ok {
			raw = v
			break
		}
		if regimeAware {
			if useDefaults, ok := ref.Params["use_defaults"].(bool); ok && useDefaults {
				return defaultRegimeTPTiersForRegime(regime)
			}
			break
		}
	}
	if regimeAware {
		tiers := resolveRegimeTPTiers(raw, regime)
		if len(tiers) < 2 {
			return nil
		}
		return finalizeProtectionTiers(tiers)
	}
	tiers := parseHLProtectionTiers(raw)
	if len(tiers) == 0 {
		tiers = defaultHLProtectionTiers()
	}
	if len(tiers) < 2 {
		return nil
	}
	return finalizeProtectionTiers(tiers)
}

func defaultHLProtectionTiers() []hlProtectionTier {
	return []hlProtectionTier{
		{Multiple: 1.5, Fraction: 0.40},
		{Multiple: 3.0, Fraction: 0.80},
		{Multiple: 5.0, Fraction: 1.00},
	}
}

func finalizeProtectionTiers(tiers []hlProtectionTier) []hlProtectionTier {
	prevFraction := 0.0
	for _, tier := range tiers {
		if tier.Multiple <= 0 || tier.Fraction <= prevFraction {
			return nil
		}
		prevFraction = tier.Fraction
	}
	tiers[len(tiers)-1].Fraction = 1
	return tiers
}

type hlProtectionTier struct {
	Multiple float64
	Fraction float64
}

func tieredTPATRPrices(sc StrategyConfig, side string, entryPrice, entryATR float64) []float64 {
	return tieredTPATRPricesFromTiers(strategyTPTiers(sc), side, entryPrice, entryATR)
}

func tieredTPATRPricesForRegime(sc StrategyConfig, side string, entryPrice, entryATR float64, regime string) []float64 {
	return tieredTPATRPricesFromTiers(strategyTPTiersForRegime(sc, regime), side, entryPrice, entryATR)
}

func tieredTPATRPricesFromTiers(tiers []hlProtectionTier, side string, entryPrice, entryATR float64) []float64 {
	if len(tiers) == 0 || entryATR <= 0 || entryPrice <= 0 {
		return nil
	}
	sideLower := strings.ToLower(strings.TrimSpace(side))
	prices := make([]float64, len(tiers))
	for i, t := range tiers {
		offset := t.Multiple * entryATR
		switch sideLower {
		case "short":
			prices[i] = entryPrice - offset
		case "long":
			prices[i] = entryPrice + offset
		default:
			return nil
		}
	}
	return prices
}

func parseHLProtectionTiers(raw interface{}) []hlProtectionTier {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	tiers := make([]hlProtectionTier, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			fmt.Printf("[WARN] hl-protection: tier[%d] is not an object, skipping (got %T)\n", idx, item)
			continue
		}
		multiple, mErr := floatFromAnyChecked(m["atr_multiple"])
		if mErr != nil {
			fmt.Printf("[WARN] hl-protection: tier[%d] atr_multiple invalid: %v — tier skipped\n", idx, mErr)
			continue
		}
		fraction, fErr := floatFromAnyChecked(m["close_fraction"])
		if fErr != nil {
			fmt.Printf("[WARN] hl-protection: tier[%d] close_fraction/fraction invalid: %v — tier skipped\n", idx, fErr)
			continue
		}
		if multiple <= 0 || fraction <= 0 {
			fmt.Printf("[WARN] hl-protection: tier[%d] non-positive multiple=%g fraction=%g — tier skipped\n", idx, multiple, fraction)
			continue
		}
		if fraction > 1 {
			fraction = 1
		}
		tiers = append(tiers, hlProtectionTier{Multiple: multiple, Fraction: fraction})
	}
	sort.SliceStable(tiers, func(i, j int) bool { return tiers[i].Multiple < tiers[j].Multiple })
	return tiers
}

func tpOIDsForTierCount(oids []int64, tierCount int) []int64 {
	if tierCount <= 0 {
		return nil
	}
	out := make([]int64, tierCount)
	copy(out, oids)
	return out
}

func tpArmedTiersForTierCount(armed []bool, tierCount int) []bool {
	if tierCount <= 0 {
		return nil
	}
	out := make([]bool, tierCount)
	copy(out, armed)
	return out
}

func cloneInt64s(vals []int64) []int64 {
	if len(vals) == 0 {
		return nil
	}
	out := make([]int64, len(vals))
	copy(out, vals)
	return out
}

func firstPresent(m map[string]interface{}, keys ...string) interface{} {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return nil
}

func floatFromAnyChecked(v interface{}) (float64, error) {
	switch x := v.(type) {
	case nil:
		return 0, fmt.Errorf("missing value")
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case jsonNumber:
		f, err := x.Float64()
		if err != nil {
			return 0, fmt.Errorf("jsonNumber: %w", err)
		}
		return f, nil
	case string:
		return 0, fmt.Errorf("string %q is not a number; quote-strip the value in config.json", x)
	default:
		return 0, fmt.Errorf("unsupported type %T", v)
	}
}

type jsonNumber interface {
	Float64() (float64, error)
}

var syncHyperliquidProtection = func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger, reconcileFillHintsJSON []byte) (*HyperliquidProtectionSyncResult, bool) {
	result, stderr, err := RunHyperliquidSyncProtection(
		sc.Script, plan.Symbol, plan.Side, plan.Size, plan.AvgCost, plan.EntryATR,
		plan.StopLossATRMult, plan.Tiers, plan.StopLossOID, plan.TPOIDs, plan.TPArmedTiers,
		plan.ForceSLReplace, plan.ForceTPReplace, plan.CancelTPOIDs,
		reconcileFillHintsJSON,
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
	warnings := formatProtectionSyncWarnings(result)
	if len(warnings) > 0 {
		msg := fmt.Sprintf("%s %s protection partially failed: %v", sc.ID, plan.Symbol, warnings)
		if logger != nil {
			logger.Warn("%s", msg)
		}
		notifyHLProtectionFailure(notifier, sc, plan.Symbol, msg)
	}
	if hlProtectionLostExchangeStop(result) {
		msg := fmt.Sprintf("**HL PROTECTION CRITICAL** [%s] %s force-replace cancelled the resting stop-loss but the replacement did NOT rest — the position has NO exchange-side stop; recorded state cleared, next sync re-places", sc.ID, plan.Symbol)
		if logger != nil {
			logger.Error("%s", msg)
		}
		if notifier != nil && notifier.HasBackends() {
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
	}
	if hlProtectionStopOutcomeUnknown(result) {
		msg := fmt.Sprintf("**HL PROTECTION CRITICAL** [%s] %s force-replace cancelled the resting stop-loss and the replacement's outcome could NOT be read (open-order diff inconclusive) — a reduce-only stop may be resting untracked; recorded state kept, verify the order book on Hyperliquid", sc.ID, plan.Symbol)
		if logger != nil {
			logger.Error("%s", msg)
		}
		if notifier != nil && notifier.HasBackends() {
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
	}
	return result, true
}

func hlProtectionLostExchangeStop(result *HyperliquidProtectionSyncResult) bool {
	return result != nil &&
		result.CancelStopLossSucceeded &&
		result.StopLossOID <= 0 &&
		!result.StopLossFilledImmediately &&
		!result.StopLossOutcomeUnknown
}

func hlProtectionStopOutcomeUnknown(result *HyperliquidProtectionSyncResult) bool {
	return result != nil &&
		result.CancelStopLossSucceeded &&
		result.StopLossOID <= 0 &&
		!result.StopLossFilledImmediately &&
		result.StopLossOutcomeUnknown
}

func applyHyperliquidProtectionSync(pos *Position, result *HyperliquidProtectionSyncResult, cancelTPOIDs []int64) {
	if pos == nil || result == nil {
		return
	}
	if result.StopLossFilledExternally {
		pos.StopLossOID = 0
	}
	if result.StopLossOID > 0 {
		pos.StopLossOID = result.StopLossOID
	} else if result.CancelStopLossSucceeded && !result.StopLossOutcomeUnknown {
		pos.StopLossOID = 0
		pos.StopLossTriggerPx = 0
	}
	if result.StopLossTriggerPx > 0 {
		pos.StopLossTriggerPx = result.StopLossTriggerPx
	}
	if result.TPOIDs != nil {
		pos.TPOIDs = cloneInt64s(result.TPOIDs)
	} else if result.TP1OID > 0 || result.TP2OID > 0 {
		pos.TPOIDs = []int64{result.TP1OID, result.TP2OID}
	}
	if len(pos.TPOIDs) > 0 {
		if len(pos.TPArmedTiers) < len(pos.TPOIDs) {
			extended := make([]bool, len(pos.TPOIDs))
			copy(extended, pos.TPArmedTiers)
			pos.TPArmedTiers = extended
		}
		for i, oid := range pos.TPOIDs {
			if oid > 0 {
				pos.TPArmedTiers[i] = true
			}
		}
	}
	if len(result.TPFilledExternally) > 0 {
		if len(pos.TPOIDs) < len(result.TPFilledExternally) {
			pos.TPOIDs = tpOIDsForTierCount(pos.TPOIDs, len(result.TPFilledExternally))
		}
		if len(pos.TPArmedTiers) < len(result.TPFilledExternally) {
			extended := make([]bool, len(result.TPFilledExternally))
			copy(extended, pos.TPArmedTiers)
			pos.TPArmedTiers = extended
		}
		for idx, filled := range result.TPFilledExternally {
			if filled {
				pos.TPOIDs[idx] = 0
				pos.TPArmedTiers[idx] = true
			}
		}
	} else if result.TP1FilledExternally || result.TP2FilledExternally {
		if len(pos.TPOIDs) < 2 {
			pos.TPOIDs = tpOIDsForTierCount(pos.TPOIDs, 2)
		}
		if len(pos.TPArmedTiers) < 2 {
			extended := make([]bool, 2)
			copy(extended, pos.TPArmedTiers)
			pos.TPArmedTiers = extended
		}
		if result.TP1FilledExternally {
			pos.TPOIDs[0] = 0
			pos.TPArmedTiers[0] = true
		}
		if result.TP2FilledExternally {
			pos.TPOIDs[1] = 0
			pos.TPArmedTiers[1] = true
		}
	}
	applySurplusTPCancelOutcome(pos, result, cancelTPOIDs)
}

func applySurplusTPCancelOutcome(pos *Position, result *HyperliquidProtectionSyncResult, cancelTPOIDs []int64) {
	if pos == nil || result == nil {
		return
	}
	for _, oid := range result.TPCancelFailedOIDs {
		if oid <= 0 {
			continue
		}
		found := false
		for _, existing := range pos.TPOIDs {
			if existing == oid {
				found = true
				break
			}
		}
		if found {
			continue
		}
		pos.TPOIDs = append(pos.TPOIDs, oid)
		if len(pos.TPArmedTiers) < len(pos.TPOIDs) {
			extended := make([]bool, len(pos.TPOIDs))
			copy(extended, pos.TPArmedTiers)
			pos.TPArmedTiers = extended
		}
		pos.TPArmedTiers[len(pos.TPOIDs)-1] = true
	}
	if len(cancelTPOIDs) == 0 {
		return
	}
	failed := make(map[int64]struct{}, len(result.TPCancelFailedOIDs))
	for _, oid := range result.TPCancelFailedOIDs {
		if oid > 0 {
			failed[oid] = struct{}{}
		}
	}
	clear := make(map[int64]struct{})
	for _, oid := range result.TPCancelFilledOIDs {
		if oid > 0 {
			clear[oid] = struct{}{}
		}
	}
	for _, oid := range cancelTPOIDs {
		if oid <= 0 {
			continue
		}
		if _, isFailed := failed[oid]; isFailed {
			continue
		}
		clear[oid] = struct{}{}
	}
	if len(clear) == 0 || len(pos.TPOIDs) == 0 {
		return
	}
	if len(pos.TPArmedTiers) < len(pos.TPOIDs) {
		extended := make([]bool, len(pos.TPOIDs))
		copy(extended, pos.TPArmedTiers)
		pos.TPArmedTiers = extended
	}
	for i, oid := range pos.TPOIDs {
		if _, ok := clear[oid]; ok {
			pos.TPOIDs[i] = 0
			pos.TPArmedTiers[i] = true
		}
	}
}

func runHyperliquidProtectionSync(
	sc StrategyConfig,
	stratState *StrategyState,
	db *StateDB,
	symbol string,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
	logTag string,
	reconcileFillHintsJSON []byte,
	liqPxByCoin map[string]float64,
	netSideByCoin map[string]string,
) (bool, float64) {
	if stratState == nil || symbol == "" {
		return false, 0
	}
	var plan hlProtectionPlan
	var syncOK bool
	if strategyUsesDynamicRegimeClose(sc) {
		mu.Lock()
		if pos, ok := stratState.Positions[symbol]; ok {
			oldAppliedRegime := pos.RegimeAppliedLabel
			regimeChanged := advanceDynamicCloseRegime(pos, stratState, sc)
			plan, syncOK = buildHyperliquidProtectionPlan(sc, pos, hlLiquidationPxForSide(liqPxByCoin, netSideByCoin, symbol, pos.Side))
			if syncOK {
				plan.CancelTPOIDs = dynamicProtectionSurplusTPOIDs(pos.TPOIDs, len(plan.Tiers))
				if regimeChanged {
					forceSL, forceTP := dynamicProtectionForceReplace(sc, pos, plan, oldAppliedRegime, true)
					plan.ForceSLReplace = plan.ForceSLReplace || forceSL
					plan.ForceTPReplace = orForceReplace(plan.ForceTPReplace, forceTP)
				}
				if pos.ScaleInResizePending {
					fSL, fTP := scaleInProtectionForceReplace(pos, plan)
					plan.ForceSLReplace = plan.ForceSLReplace || fSL
					plan.ForceTPReplace = orForceReplace(plan.ForceTPReplace, fTP)
				}
			}
		}
		mu.Unlock()
	} else {
		mu.RLock()
		if pos, ok := stratState.Positions[symbol]; ok {
			plan, syncOK = buildHyperliquidProtectionPlan(sc, pos, hlLiquidationPxForSide(liqPxByCoin, netSideByCoin, symbol, pos.Side))
			if syncOK && pos.ScaleInResizePending {
				fSL, fTP := scaleInProtectionForceReplace(pos, plan)
				plan.ForceSLReplace = plan.ForceSLReplace || fSL
				plan.ForceTPReplace = orForceReplace(plan.ForceTPReplace, fTP)
			}
		}
		mu.RUnlock()
	}
	if !syncOK {
		return false, 0
	}
	protection, ok := syncHyperliquidProtection(sc, plan, notifier, logger, reconcileFillHintsJSON)
	if !ok || protection == nil {
		return false, 0
	}
	mu.Lock()
	defer mu.Unlock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.Side != plan.Side {
		return false, 0
	}
	if protection.StopLossFilledImmediately && protection.StopLossTriggerPx > 0 {
		if recordPerpsStopLossClose(stratState, symbol, protection.StopLossTriggerPx, "protection_sync_sl_immediate", logger) {
			return true, protection.StopLossTriggerPx
		}
	}
	applyHyperliquidProtectionSync(pos, protection, plan.CancelTPOIDs)
	if effectiveTrailingStopPct(sc, pos) <= 0 {
		pos.ScaleInResizePending = false
	}
	if logger != nil && len(protection.TPCancelFilledOIDs) > 0 {
		logger.Info("surplus TP OIDs filled on-chain (reconciler will book): %v", protection.TPCancelFilledOIDs)
	}
	stampOpenTradeWithProtectionSnapshot(stratState, db, sc, symbol, pos)
	if logger != nil {
		logger.Info("%s (sl_oid=%d tp_oids=%v)", logTag, pos.StopLossOID, pos.TPOIDs)
	}
	return true, 0
}

func hyperliquidPlacesOnChainTPs(sc StrategyConfig) bool {
	if (sc.Type != "perps" && sc.Type != "manual") || sc.Platform != "hyperliquid" {
		return false
	}
	if !hyperliquidIsLive(sc.Args) {
		return false
	}
	return strategyUsesTieredTPATRClose(sc)
}

var closeStrategiesSuppressedByOnChainProtection = map[string]struct{}{
	"tiered_tp_atr":                     {},
	"tiered_tp_atr_live":                {},
	"tiered_tp_atr_regime":              {},
	"tiered_tp_atr_live_regime":         {},
	"tiered_tp_atr_live_regime_dynamic": {},
}

func isTieredTPATRCloseName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tiered_tp_atr", "tiered_tp_atr_live",
		"tiered_tp_atr_regime", "tiered_tp_atr_live_regime",
		dynamicCloseStrategyName:
		return true
	}
	return false
}

func closeStrategySuppressedByOnChainProtection(sc StrategyConfig) bool {
	if !hyperliquidPlacesOnChainTPs(sc) || sc.CloseStrategy == nil {
		return false
	}
	_, suppress := closeStrategiesSuppressedByOnChainProtection[strings.TrimSpace(sc.CloseStrategy.Name)]
	return suppress
}

func strategyConfigWithOnChainProtectionFilter(sc StrategyConfig) StrategyConfig {
	if !closeStrategySuppressedByOnChainProtection(sc) {
		return sc
	}
	clone := sc
	clone.CloseStrategy = nil
	return clone
}

func notifyHLProtectionFailure(notifier *MultiNotifier, sc StrategyConfig, symbol, reason string) {
	if notifier == nil || !notifier.HasBackends() {
		return
	}
	msg := fmt.Sprintf("**HL PROTECTION WARNING** [%s] %s reduce-only SL/TP sync failed: %s", sc.ID, symbol, reason)
	notifier.SendToAllChannels(msg)
	notifier.SendOwnerDM(msg)
}
