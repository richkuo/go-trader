package main

// #841 chunk 2b — the unified per-regime close block.
//
// Operator-facing shape on a tiered_tp_atr_regime / tiered_tp_atr_live_regime
// close ref:
//
//	"params": { "trend_regime": {
//	    "<label>": {
//	        "stop_loss_atr": 1.5,                       // optional, per-regime SL
//	        "tp_tiers": [                               // this regime's ladder
//	            { "atr_multiple": 2.0, "close_fraction": 0.5,
//	              "sl_after": { "kind": "trail_from_here", "tp_atr_fraction": 0.5 } },
//	            { "atr_multiple": 4.0, "close_fraction": 1.0 }
//	        ]
//	    },
//	    ...one entry per regime label, exhaustive...
//	} }
//
// Every value under a label is a plain scalar — the regime is resolved once at
// the top, so sl_after carries no internal trend_regime sub-block. Resolution
// selects the active regime's block (unifiedRegimeScalarParams) and feeds it to
// the existing SCALAR tiered_tp_atr machinery, so each regime resolves
// independently (tier counts may differ between regimes).

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// closeParamsAreUnifiedRegime reports whether a close ref's params use the
// unified per-regime block (top-level "trend_regime") rather than the legacy
// tier-keyed shape (top-level "tp_tiers" with trend_regime inside each tier).
func closeParamsAreUnifiedRegime(params map[string]interface{}) bool {
	if params == nil {
		return false
	}
	_, hasTrend := params[regimeClassifierKey]
	return hasTrend
}

// unifiedRegimeScalarParams selects the scalar tiered-close plan for the given
// regime label out of a unified block. The returned params are a plain scalar
// tiered_tp_atr config ({"tp_tiers": [...]}, plus "atr_source" when the ref is
// a *_live variant) that the existing scalar resolver/evaluator understands.
// stopLossATR is the per-label stop_loss_atr multiple (0 when unset). ok=false
// when the label is absent or malformed — callers validate at load, so a miss
// at runtime means the regime classifier produced an unexpected label and the
// caller should fall back to its scalar sibling.
func unifiedRegimeScalarParams(params map[string]interface{}, regime string) (scalar map[string]interface{}, stopLossATR float64, ok bool) {
	trendRaw, isMap := params[regimeClassifierKey].(map[string]interface{})
	if !isMap {
		return nil, 0, false
	}
	label := strings.TrimSpace(regime)
	labelRaw, isMap := trendRaw[label].(map[string]interface{})
	if !isMap {
		fallback := regimeLookupLabel(label)
		if fallback != label {
			labelRaw, isMap = trendRaw[fallback].(map[string]interface{})
		}
	}
	if !isMap {
		return nil, 0, false
	}
	tiers, hasTiers := labelRaw["tp_tiers"]
	if !hasTiers {
		return nil, 0, false
	}
	scalar = map[string]interface{}{"tp_tiers": tiers}
	// Carry atr_source from the ref's top level so the *_live variant keeps
	// recomputing ATR per tick after the regime block is collapsed to scalar.
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

// strategyUsesUnifiedRegimeClose reports whether the strategy has a regime-aware
// tiered close ref written in the unified per-regime shape. Used to gate SL
// resolution: the unified close owns the (ATR-based) stop loss, armed on the
// cycle after open like stop_loss_atr_regime. #841 2b.
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

// unifiedCloseStopLossATR returns the per-regime stop_loss_atr multiple from a
// strategy's unified per-regime close block for the given regime label. ok=false
// when the strategy isn't unified, the label is absent, or the label set no
// stop_loss_atr (slMult 0 → no fixed SL placed for that regime). #841 2b.
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

// unifiedCloseRefParams returns the params of a strategy's unified per-regime
// close ref, or nil if it doesn't use one.
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

// unifiedCloseParamsEqualForReload reports whether two strategy revisions carry
// an equivalent unified per-regime close block. The block holds the whole exit
// plan (per-regime TP ladder + SL + sl_after) armed at open, so any change —
// including adding/removing the unified close — is unsafe while a position is
// open. #841 2b.
func unifiedCloseParamsEqualForReload(a, b StrategyConfig) bool {
	return reflect.DeepEqual(unifiedCloseRefParams(a), unifiedCloseRefParams(b))
}

// validateUnifiedCloseSoleOwner enforces that a strategy using a unified
// per-regime close block does not also declare strategy-level stop fields. The
// unified close owns the whole exit plan including the per-regime SL (via
// stop_loss_atr), so a strategy-level stop would be an ambiguous second owner.
// #841 2b / #843 sole-owner.
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

// validateUnifiedRegimeClose validates a unified per-regime close block against
// the strategy's regime label vocabulary. Errors are config-load failures so a
// typo can't silently disable the exit plan. The label set must be exhaustive
// (no silent fallback) and contain no unknown labels.
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

	seen := make(map[string]bool, len(trendRaw))
	unknown := make([]string, 0)
	for l := range trendRaw {
		seen[l] = true
		if !valid[l] {
			unknown = append(unknown, l)
		}
	}
	sort.Strings(unknown)
	for _, l := range unknown {
		errs = append(errs, fmt.Sprintf("%s.%s: unknown regime label %q (expected one of: %s)",
			ctxLabel, regimeClassifierKey, l, strings.Join(labels, ", ")))
	}

	for _, l := range labels {
		lr, ok := trendRaw[l]
		if !ok {
			if regimeLabelCoveredBySeen(l, seen) {
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
		// stop_loss_atr is required per label: the unified close owns the SL
		// (EffectiveStopLossPct defers and sole-owner rejects strategy-level
		// stops), so a regime omitting it would run an HL perps position with no
		// stop loss at all. #841 review.
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

// validateUnifiedTierList validates one regime label's scalar tp_tiers ladder:
// non-empty, ascending-capable atr_multiple > 0, close_fraction in (0, 1], and
// a well-formed optional scalar sl_after.
func validateUnifiedTierList(raw interface{}, ctxLabel string) []string {
	items, ok := raw.([]interface{})
	if !ok {
		return []string{fmt.Sprintf("%s.tp_tiers: must be a list, got %T", ctxLabel, raw)}
	}
	// Require >=2 tiers to match the on-chain resolver (strategyTPTiersForRegime
	// returns nil for <2) and the legacy regime validator. On HL-live the
	// software evaluator is suppressed, so a single-tier regime would emit no TP
	// exit at all — only the SL. A single TP-then-trail belongs in #844. #841 review.
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
