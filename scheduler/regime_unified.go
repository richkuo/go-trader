package main


import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func closeParamsAreUnifiedRegime(params map[string]interface{}) bool {
	if params == nil {
		return false
	}
	_, hasTrend := params[regimeClassifierKey]
	return hasTrend
}

func unifiedRegimeScalarParams(params map[string]interface{}, regime string) (scalar map[string]interface{}, stopLossATR float64, ok bool) {
	trendRaw, isMap := params[regimeClassifierKey].(map[string]interface{})
	if !isMap {
		return nil, 0, false
	}
	r := strings.TrimSpace(regime)
	labelRaw, isMap := trendRaw[r].(map[string]interface{})
	if !isMap {
		if regimeDirectionalSubs[r] {
			labelRaw, isMap = trendRaw[regimeDirectionalBare].(map[string]interface{})
		}
		if !isMap {
			return nil, 0, false
		}
	}
	tiers, hasTiers := labelRaw["tp_tiers"]
	if !hasTiers {
		return nil, 0, false
	}
	scalar = map[string]interface{}{"tp_tiers": tiers}
	if v, ok := params["atr_source"]; ok {
		scalar["atr_source"] = v
	}
	if v, ok := labelRaw["stop_loss_atr"]; ok {
		if f, err := floatFromAnyChecked(v); err == nil {
			stopLossATR = f
		}
	}
	return scalar, stopLossATR, true
}

func strategyUsesUnifiedRegimeClose(sc StrategyConfig) bool {
	for _, ref := range sc.closeRefs() {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if n != "tiered_tp_atr_regime" && n != "tiered_tp_atr_live_regime" && n != dynamicCloseStrategyName {
			continue
		}
		if closeParamsAreUnifiedRegime(ref.Params) {
			return true
		}
	}
	return false
}

func unifiedCloseStopLossATR(sc StrategyConfig, regime string) (float64, bool) {
	for _, ref := range sc.closeRefs() {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if n != "tiered_tp_atr_regime" && n != "tiered_tp_atr_live_regime" && n != dynamicCloseStrategyName {
			continue
		}
		if !closeParamsAreUnifiedRegime(ref.Params) {
			return 0, false
		}
		_, sl, ok := unifiedRegimeScalarParams(ref.Params, regime)
		if !ok || sl <= 0 {
			return 0, false
		}
		return sl, true
	}
	return 0, false
}

func unifiedCloseRefParams(sc StrategyConfig) map[string]interface{} {
	for _, ref := range sc.closeRefs() {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if n != "tiered_tp_atr_regime" && n != "tiered_tp_atr_live_regime" && n != dynamicCloseStrategyName {
			continue
		}
		if closeParamsAreUnifiedRegime(ref.Params) {
			return ref.Params
		}
	}
	return nil
}

func unifiedCloseParamsEqualForReload(a, b StrategyConfig) bool {
	return reflect.DeepEqual(unifiedCloseRefParams(a), unifiedCloseRefParams(b))
}

func validateUnifiedCloseSoleOwner(sc StrategyConfig, ctxLabel string) []string {
	if !strategyUsesUnifiedRegimeClose(sc) {
		return nil
	}
	var errs []string
	conflict := func(set bool, field string) {
		if set {
			errs = append(errs, fmt.Sprintf("%s: %s is not allowed alongside a unified per-regime close — the close owns the SL via per-regime stop_loss_atr", ctxLabel, field))
		}
	}
	conflict(sc.StopLossATRMult != nil, "stop_loss_atr_mult")
	conflict(sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero(), "stop_loss_atr_regime")
	conflict(sc.StopLossPct != nil, "stop_loss_pct")
	conflict(sc.StopLossMarginPct != nil, "stop_loss_margin_pct")
	conflict(sc.TrailingStopATRMult != nil, "trailing_stop_atr_mult")
	conflict(sc.TrailingStopPct != nil, "trailing_stop_pct")
	conflict(sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero(), "trailing_stop_atr_regime")
	return errs
}

func validateUnifiedRegimeClose(params map[string]interface{}, labels []string, ctxLabel string) []string {
	var errs []string
	for k := range params {
		if k != regimeClassifierKey && k != "atr_source" {
			errs = append(errs, fmt.Sprintf("%s: unknown param %q (allowed: trend_regime, atr_source)", ctxLabel, k))
		}
	}
	trendRaw, ok := params[regimeClassifierKey].(map[string]interface{})
	if !ok {
		errs = append(errs, fmt.Sprintf("%s.%s: must be an object", ctxLabel, regimeClassifierKey))
		return errs
	}
	if len(labels) == 0 {
		labels = canonicalTrendRegimeLabels
	}
	valid := make(map[string]bool, len(labels))
	for _, l := range labels {
		valid[l] = true
	}

	unknown := make([]string, 0)
	for l := range trendRaw {
		if !valid[l] {
			unknown = append(unknown, l)
		}
	}
	sort.Strings(unknown)
	for _, l := range unknown {
		errs = append(errs, fmt.Sprintf("%s.%s: unknown regime label %q (expected one of: %s)",
			ctxLabel, regimeClassifierKey, l, strings.Join(labels, ", ")))
	}

	bareDirectional := trendRaw[regimeDirectionalBare] != nil
	for _, l := range labels {
		lr, ok := trendRaw[l]
		if !ok {
			if regimeLabelFamilyCovered(l, bareDirectional) {
				continue
			}
			errs = append(errs, fmt.Sprintf("%s.%s: missing required regime label %q (must be exhaustive — no silent fallback)",
				ctxLabel, regimeClassifierKey, l))
			continue
		}
		lm, ok := lr.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.%s.%s: must be an object, got %T", ctxLabel, regimeClassifierKey, l, lr))
			continue
		}
		for k := range lm {
			if k != "stop_loss_atr" && k != "tp_tiers" {
				errs = append(errs, fmt.Sprintf("%s.%s.%s: unknown key %q (allowed: stop_loss_atr, tp_tiers)",
					ctxLabel, regimeClassifierKey, l, k))
			}
		}
		if v, ok := lm["stop_loss_atr"]; !ok {
			errs = append(errs, fmt.Sprintf("%s.%s.%s: missing required %q (the unified close owns the per-regime SL)", ctxLabel, regimeClassifierKey, l, "stop_loss_atr"))
		} else if f, err := floatFromAnyChecked(v); err != nil || f <= 0 {
			errs = append(errs, fmt.Sprintf("%s.%s.%s.stop_loss_atr: must be > 0", ctxLabel, regimeClassifierKey, l))
		}
		tiersRaw, ok := lm["tp_tiers"]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.%s.%s: missing required %q", ctxLabel, regimeClassifierKey, l, "tp_tiers"))
			continue
		}
		errs = append(errs, validateUnifiedTierList(tiersRaw, fmt.Sprintf("%s.%s.%s", ctxLabel, regimeClassifierKey, l))...)
	}
	return errs
}

func validateUnifiedTierList(raw interface{}, ctxLabel string) []string {
	items, ok := raw.([]interface{})
	if !ok {
		return []string{fmt.Sprintf("%s.tp_tiers: must be a list, got %T", ctxLabel, raw)}
	}
	if len(items) < 2 {
		return []string{fmt.Sprintf("%s.tp_tiers: must have at least 2 tiers, got %d", ctxLabel, len(items))}
	}
	var errs []string
	for i, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.tp_tiers[%d]: must be an object, got %T", ctxLabel, i, item))
			continue
		}
		mult, err := floatFromAnyChecked(firstPresent(m, "atr_multiple"))
		if err != nil || mult <= 0 {
			errs = append(errs, fmt.Sprintf("%s.tp_tiers[%d].atr_multiple: must be > 0", ctxLabel, i))
		}
		frac, err := floatFromAnyChecked(firstPresent(m, "close_fraction"))
		if err != nil || frac <= 0 || frac > 1 {
			errs = append(errs, fmt.Sprintf("%s.tp_tiers[%d].close_fraction: must be in (0, 1]", ctxLabel, i))
		}
		if saRaw, ok := m["sl_after"]; ok {
			rule, perr := parseSLAfterRuleRuntime(saRaw)
			if perr != nil {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers[%d].sl_after: %v", ctxLabel, i, perr))
			} else if verr := validateSLAfterRule(rule); verr != nil {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers[%d].sl_after: %v", ctxLabel, i, verr))
			} else if rule.HasRegime() {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers[%d].sl_after: must be scalar in a unified per-regime block (the regime is resolved at the top level; drop the trend_regime sub-block)", ctxLabel, i))
			}
		}
		for k := range m {
			switch k {
			case "atr_multiple", "close_fraction", "sl_after":
			default:
				errs = append(errs, fmt.Sprintf("%s.tp_tiers[%d]: unknown key %q (allowed: atr_multiple, close_fraction, sl_after)", ctxLabel, i, k))
			}
		}
	}
	return errs
}
