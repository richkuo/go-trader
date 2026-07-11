package main

import (
	"encoding/json"
	"strings"
)

// needsV15CloseMigration reports whether the on-disk config still carries
// pre-v15 close-strategy keys that MigrateConfig rewrites (#841).
func needsV15CloseMigration(data []byte) bool {
	var meta struct {
		ConfigVersion int `json:"config_version"`
	}
	if err := json.Unmarshal(data, &meta); err == nil && meta.ConfigVersion >= 15 {
		return false
	}
	return true
}

// normalizeDeprecatedCloseRef rewrites in-memory close refs that use deprecated
// evaluator names. Disk migration handles persisted configs; this covers
// hand-edited JSON and hot-reload paths that bypass MigrateConfig.
func normalizeDeprecatedCloseRef(ref *StrategyRef) {
	if ref == nil {
		return
	}
	name := strings.ToLower(strings.TrimSpace(ref.Name))
	if name != "tp_at_pct" {
		return
	}
	warnDeprecatedConfigKey("tp_at_pct", "tiered_tp_pct")
	pct := 0.03
	if ref.Params != nil {
		if v, ok := ref.Params["pct"]; ok {
			if f, err := floatFromAnyChecked(v); err == nil {
				pct = f
			}
		}
	}
	ref.Name = "tiered_tp_pct"
	out := map[string]interface{}{
		"tp_tiers": []interface{}{
			map[string]interface{}{
				"profit_pct":     pct,
				"close_fraction": 1.0,
			},
		},
	}
	if ref.Params != nil {
		if sa, ok := ref.Params["sl_after"]; ok {
			out["sl_after"] = sa
		}
	}
	ref.Params = out
}

// migrateV15CloseKeys rewrites close-strategy params to the canonical #841 shape
// and bumps strategies that still use tp_at_pct to tiered_tp_pct.
func migrateV15CloseKeys(raw map[string]interface{}) {
	defaultSL := DefaultStopLossATRMult
	if v, ok := raw["default_stop_loss_atr_mult"].(float64); ok && v > 0 {
		defaultSL = v
	}
	strategies, ok := raw["strategies"].([]interface{})
	if !ok {
		return
	}
	for _, item := range strategies {
		sc, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		slRegime := cloneOrNewJSONMap(sc["stop_loss_atr_regime"])
		slMult := scalarStopLossFromStrategy(sc, defaultSL)
		folded := migrateV15StrategyCloseRefs(sc, slRegime, slMult)
		if folded {
			migrateV15StripStrategyStopOwners(sc)
		}
		migrateV15StrategyRegimeBlocks(sc)
	}
}

func migrateV15StrategyRegimeBlocks(sc map[string]interface{}) {
	for _, key := range []string{"stop_loss_atr_regime", "trailing_stop_atr_regime"} {
		if raw, ok := sc[key]; ok {
			sc[key] = canonicalizeRegimeBlock(raw)
		}
	}
}

func canonicalizeRegimeBlock(raw interface{}) map[string]interface{} {
	block, ok := raw.(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	out := make(map[string]interface{}, len(block))
	for k, v := range block {
		if k != "trend_regime" {
			out[k] = v
		}
	}
	trRaw, ok := block["trend_regime"].(map[string]interface{})
	if !ok {
		return out
	}
	trOut := make(map[string]interface{}, len(trRaw))
	for label, lr := range trRaw {
		lm, ok := lr.(map[string]interface{})
		if !ok {
			trOut[label] = lr
			continue
		}
		labelOut := make(map[string]interface{}, len(lm))
		for ek, ev := range lm {
			switch ek {
			case "atr", "multiple", "atr_multiple", "fraction":
				// Resolved below with explicit precedence — folding aliases
				// inside a map-range made the survivor depend on Go's
				// randomized iteration order when two legacy aliases were
				// both set.
			default:
				labelOut[ek] = ev
			}
		}
		// Deterministic alias precedence: canonical atr_multiple wins, then
		// legacy "atr", then legacy "multiple".
		for _, key := range []string{"multiple", "atr", "atr_multiple"} {
			if v, ok := lm[key]; ok {
				labelOut["atr_multiple"] = v
			}
		}
		// close_fraction (canonical, copied in the default arm above) wins
		// over the legacy "fraction" alias.
		if v, ok := lm["fraction"]; ok {
			if _, has := labelOut["close_fraction"]; !has {
				labelOut["close_fraction"] = v
			}
		}
		trOut[label] = labelOut
	}
	out["trend_regime"] = trOut
	return out
}

func scalarStopLossFromStrategy(sc map[string]interface{}, fallback float64) float64 {
	if v, ok := sc["stop_loss_atr_mult"]; ok {
		if f, err := floatFromAnyChecked(v); err == nil && f > 0 {
			return f
		}
	}
	return fallback
}

// migrateV15StripStrategyStopOwners removes strategy-level stop fields after a
// legacy regime close folds into a unified block. The unified close owns SL
// via per-regime stop_loss_atr; leaving scalar/regime siblings behind fails
// validateUnifiedCloseSoleOwner on startup (#841).
func migrateV15StripStrategyStopOwners(sc map[string]interface{}) {
	for _, key := range []string{
		"stop_loss_atr_mult",
		"stop_loss_atr_regime",
		"stop_loss_pct",
		"stop_loss_margin_pct",
		"trailing_stop_atr_mult",
		"trailing_stop_pct",
		"trailing_stop_atr_regime",
	} {
		delete(sc, key)
	}
}

func migrateV15StrategyCloseRefs(sc map[string]interface{}, slRegime map[string]interface{}, slMult float64) bool {
	if refRaw, ok := sc["close_strategy"]; ok {
		if ref, ok := refRaw.(map[string]interface{}); ok {
			return migrateV15CloseRef(ref, slRegime, slMult)
		}
	}
	closes, ok := sc["close_strategies"].([]interface{})
	if !ok || len(closes) == 0 {
		return false
	}
	if ref, ok := closes[0].(map[string]interface{}); ok {
		return migrateV15CloseRef(ref, slRegime, slMult)
	}
	return false
}

func migrateV15CloseRef(ref map[string]interface{}, slRegime map[string]interface{}, slMult float64) bool {
	name := strings.ToLower(strings.TrimSpace(stringFromJSON(ref["name"])))
	params, _ := ref["params"].(map[string]interface{})
	if params == nil {
		params = map[string]interface{}{}
	}

	switch name {
	case "tp_at_pct":
		ref["name"] = "tiered_tp_pct"
		ref["params"] = migrateV15TPAtPctParams(params)
		return false
	case "tiered_tp_atr_regime", "tiered_tp_atr_live_regime":
		if !closeParamsAreUnifiedRegime(params) {
			if tierList, ok := v15TierListRaw(params); ok {
				ref["params"] = liftLegacyRegimeCloseToUnified(params, tierList, slRegime, slMult)
				return true
			}
		}
	case trailingTPRatchetRegimeCloseName:
		if len(params) > 0 {
			ref["params"] = canonicalizeTrailingRatchetRegimeParams(params)
		}
		return false
	case trailingTPRatchetCloseName:
		if len(params) > 0 {
			ref["params"] = canonicalizeTrailingRatchetScalarParams(params)
		}
		return false
	}

	if len(params) > 0 {
		ref["params"] = canonicalizeCloseParams(params)
	}
	return false
}

func migrateV15TPAtPctParams(params map[string]interface{}) map[string]interface{} {
	pct := params["pct"]
	if pct == nil {
		pct = 0.03
	}
	return map[string]interface{}{
		"tp_tiers": []interface{}{
			map[string]interface{}{
				"profit_pct":     pct,
				"close_fraction": 1.0,
			},
		},
	}
}

func v15TierListRaw(params map[string]interface{}) (interface{}, bool) {
	if v, ok := params["tp_tiers"]; ok {
		return v, true
	}
	if v, ok := params["tiers"]; ok {
		return v, true
	}
	return nil, false
}

// canonicalizeTrailingRatchetRegimeParams preserves the regime→tier-list map
// shape for trailing_tp_ratchet_regime (#844). The generic canonicalizeTierList
// path only accepts []interface{} and would wipe a keyed table.
func canonicalizeTrailingRatchetRegimeParams(params map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(params))
	for k, v := range params {
		switch k {
		case "tp_tiers":
			table, ok := v.(map[string]interface{})
			if !ok {
				out["tp_tiers"] = v
				continue
			}
			tableOut := make(map[string]interface{}, len(table))
			for label, block := range table {
				tableOut[label] = canonicalizeTierList(block)
			}
			out["tp_tiers"] = tableOut
		default:
			out[k] = v
		}
	}
	return out
}

func canonicalizeTrailingRatchetScalarParams(params map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(params))
	for k, v := range params {
		switch k {
		case "tp_tiers", "tiers":
			out["tp_tiers"] = canonicalizeTierList(v)
		default:
			out[k] = v
		}
	}
	return out
}

func canonicalizeCloseParams(params map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(params))
	for k, v := range params {
		switch k {
		case "tiers", "tp_tiers":
			out["tp_tiers"] = canonicalizeTierList(v)
		case "trend_regime":
			out["trend_regime"] = canonicalizeUnifiedTrendRegime(v)
		default:
			out[k] = v
		}
	}
	return out
}

func canonicalizeUnifiedTrendRegime(raw interface{}) map[string]interface{} {
	tr, ok := raw.(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	out := make(map[string]interface{}, len(tr))
	for label, lr := range tr {
		lm, ok := lr.(map[string]interface{})
		if !ok {
			out[label] = lr
			continue
		}
		labelOut := make(map[string]interface{}, len(lm))
		for k, v := range lm {
			switch k {
			case "tp_tiers", "tiers":
				labelOut["tp_tiers"] = canonicalizeTierList(v)
			default:
				labelOut[k] = v
			}
		}
		out[label] = labelOut
	}
	return out
}

func canonicalizeTierList(raw interface{}) []interface{} {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]interface{}, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			out = append(out, item)
			continue
		}
		out = append(out, canonicalizeTierObject(m))
	}
	return out
}

func canonicalizeTierObject(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch k {
		case "atr", "multiple":
			if _, has := out["atr_multiple"]; !has {
				out["atr_multiple"] = v
			}
		case "atr_multiple":
			out["atr_multiple"] = v
		case "fraction":
			if _, has := out["close_fraction"]; !has {
				out["close_fraction"] = v
			}
		case "close_fraction":
			out["close_fraction"] = v
		case "pct":
			if _, has := out["profit_pct"]; !has {
				out["profit_pct"] = v
			}
		case "profit_pct":
			out["profit_pct"] = v
		case "sl_after":
			out["sl_after"] = canonicalizeSLAfterScalar(v)
		case "trend_regime":
			out["trend_regime"] = v
		default:
			out[k] = v
		}
	}
	return out
}

func canonicalizeSLAfterScalar(raw interface{}) interface{} {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return raw
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch k {
		case "tp_atr_fraction", "atr_mult", "trail_atr_mult":
			if scalar := flattenSingleRegimeScalar(v); scalar != nil {
				out[k] = scalar
			} else {
				out[k] = v
			}
		default:
			out[k] = v
		}
	}
	return out
}

func liftLegacyRegimeCloseToUnified(params map[string]interface{}, tierListRaw interface{}, slRegime map[string]interface{}, slMult float64) map[string]interface{} {
	items, ok := tierListRaw.([]interface{})
	if !ok || len(items) == 0 {
		return canonicalizeCloseParams(params)
	}
	labels := regimeLabelsFromTierRaw(tierListRaw)
	slByLabel := stopLossATRByLabel(slRegime, labels, slMult)

	trendRegime := make(map[string]interface{}, len(labels))
	for _, label := range labels {
		tpTiers := make([]interface{}, 0, len(items))
		for _, item := range items {
			tier, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			tierOut := map[string]interface{}{}

			if tr, ok := tier["trend_regime"].(map[string]interface{}); ok {
				if entry, ok := tr[label].(map[string]interface{}); ok {
					if mult := v15TierTrigger(entry); mult != nil {
						tierOut["atr_multiple"] = mult
					}
					if cf, ok := entry["close_fraction"]; ok {
						tierOut["close_fraction"] = cf
					} else if cf, ok := entry["fraction"]; ok {
						tierOut["close_fraction"] = cf
					}
				}
			}
			if _, hasCF := tierOut["close_fraction"]; !hasCF {
				if cf, ok := tier["close_fraction"]; ok {
					tierOut["close_fraction"] = cf
				} else if cf, ok := tier["fraction"]; ok {
					tierOut["close_fraction"] = cf
				}
			}
			if sa, ok := tier["sl_after"]; ok {
				if scalar := inlineSLAfterForRegime(sa, label); scalar != nil {
					tierOut["sl_after"] = scalar
				}
			}
			if len(tierOut) > 0 {
				tpTiers = append(tpTiers, tierOut)
			}
		}
		labelBlock := map[string]interface{}{
			"stop_loss_atr": slByLabel[label],
			"tp_tiers":      tpTiers,
		}
		trendRegime[label] = labelBlock
	}

	out := map[string]interface{}{"trend_regime": trendRegime}
	if v, ok := params["atr_source"]; ok {
		out["atr_source"] = v
	}
	if v, ok := params["use_defaults"]; ok {
		out["use_defaults"] = v
	}
	return out
}

func stopLossATRByLabel(slRegime map[string]interface{}, labels []string, fallback float64) map[string]float64 {
	out := make(map[string]float64, len(labels))
	tr, _ := slRegime["trend_regime"].(map[string]interface{})
	for _, label := range labels {
		sl := fallback
		if tr != nil {
			if entry, ok := tr[label].(map[string]interface{}); ok {
				if v := v15TierTrigger(entry); v != nil {
					if f, err := floatFromAnyChecked(v); err == nil && f > 0 {
						sl = f
					}
				}
			}
		}
		out[label] = sl
	}
	return out
}

func v15TierTrigger(entry map[string]interface{}) interface{} {
	if v, ok := entry["atr_multiple"]; ok {
		return v
	}
	if v, ok := entry["atr"]; ok {
		return v
	}
	if v, ok := entry["multiple"]; ok {
		return v
	}
	return nil
}

func inlineSLAfterForRegime(raw interface{}, regime string) map[string]interface{} {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch k {
		case "kind":
			out[k] = v
		case "tp_atr_fraction", "atr_mult", "trail_atr_mult":
			if scalar := inlineRegimeScalarField(v, regime); scalar != nil {
				out[k] = scalar
			} else {
				out[k] = v
			}
		default:
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func inlineRegimeScalarField(raw interface{}, regime string) interface{} {
	block, ok := raw.(map[string]interface{})
	if !ok {
		return raw
	}
	tr, ok := block["trend_regime"].(map[string]interface{})
	if !ok {
		return raw
	}
	if v, ok := tr[regime]; ok {
		return v
	}
	return nil
}

func flattenSingleRegimeScalar(raw interface{}) interface{} {
	block, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	tr, ok := block["trend_regime"].(map[string]interface{})
	if !ok || len(tr) != 1 {
		return nil
	}
	for _, v := range tr {
		return v
	}
	return nil
}
