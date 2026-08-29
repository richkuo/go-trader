package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

var closeDefaultsSupported = map[string]struct{}{
	"tiered_tp_pct":              {},
	"tiered_tp_atr":              {},
	"tiered_tp_atr_live":         {},
	"trailing_tp_ratchet":        {},
	"trailing_tp_ratchet_regime": {},
}

const (
	userCloseDefaultTrailingStopATRMultRegimeKey = v19TrailingStopATRMultRegimeKey
	userCloseDefaultStopLossATRMultRegimeKey     = v19StopLossATRMultRegimeKey
	userCloseDefaultRegimeATRKey                 = "regime_atr"
)

func closeDefaultsTierEvaluator(name string) bool {
	_, ok := closeDefaultsSupported[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func closeDefaultsSupportedNames() []string {
	names := make([]string, 0, len(closeDefaultsSupported))
	for name := range closeDefaultsSupported {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func closeDefaultsEntry(defaults CloseDefaultsMap, name string) (map[string]interface{}, bool) {
	if len(defaults) == 0 {
		return nil, false
	}
	want := strings.ToLower(strings.TrimSpace(name))
	if entry, ok := defaults[want]; ok {
		return entry, true
	}
	keys := make([]string, 0, len(defaults))
	for k := range defaults {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.ToLower(strings.TrimSpace(k)) == want {
			return defaults[k], true
		}
	}
	return nil, false
}

func validateUserDefaults(defaults *UserDefaultsConfig) []string {
	if defaults == nil {
		return nil
	}
	var errs []string
	errs = append(errs, validateUserCloseDefaults(defaults.Close)...)
	if defaults.RegimeATR != nil {
		errs = append(errs, validateUserDefaultRegimeATR(defaults.RegimeATR)...)
	}
	return errs
}

func validateUserCloseDefaults(defaults CloseDefaultsMap) []string {
	if len(defaults) == 0 {
		return nil
	}
	var errs []string
	names := make([]string, 0, len(defaults))
	for name := range defaults {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := defaults[name]
		normName := strings.ToLower(strings.TrimSpace(name))
		if !closeDefaultsTierEvaluator(name) {
			if normName == userCloseDefaultRegimeATRKey {
				errs = append(errs, fmt.Sprintf("user_defaults.close[%q]: regime_atr moved to user_defaults.regime_atr", name))
			} else {
				errs = append(errs, fmt.Sprintf("user_defaults.close[%q]: not a tp_tiers close evaluator (allowed: %s)", name, strings.Join(closeDefaultsSupportedNames(), ", ")))
			}
			continue
		}
		for k := range entry {
			if k == "tp_tiers" {
				continue
			}
			if normName == trailingTPRatchetRegimeCloseName && k == userCloseDefaultTrailingStopATRMultRegimeKey {
				continue
			}
			if normName == trailingTPRatchetRegimeCloseName {
				errs = append(errs, fmt.Sprintf("user_defaults.close[%q]: unknown key %q (only tp_tiers and trailing_stop_atr_mult_regime are allowed)", name, k))
			} else {
				errs = append(errs, fmt.Sprintf("user_defaults.close[%q]: unknown key %q (only tp_tiers is allowed)", name, k))
			}
		}
		tp, ok := entry["tp_tiers"]
		if !ok || tp == nil {
			errs = append(errs, fmt.Sprintf("user_defaults.close[%q]: missing tp_tiers", name))
			continue
		}
		errs = append(errs, validateUserCloseDefaultTiers(name, tp)...)
		if normName == trailingTPRatchetRegimeCloseName {
			if raw, ok := entry[userCloseDefaultTrailingStopATRMultRegimeKey]; ok {
				errs = append(errs, validateUserCloseDefaultTrailingStopATRMultRegime(name, raw)...)
			}
		}
	}
	return errs
}

func validateUserCloseDefaultTiers(name string, tp interface{}) []string {
	ctx := fmt.Sprintf("user_defaults.close[%q].tp_tiers", name)
	isRatchet := isTrailingTPRatchetCloseName(name)
	switch v := tp.(type) {
	case []interface{}:
		if len(v) == 0 {
			return []string{ctx + ": must not be empty (omit the entry to use the system default)"}
		}
		if isRatchet {
			tiers, errs := parseTrailingRatchetTierList(v, ctx)
			return append(errs, validateTrailingRatchetTierMonotonicity(tiers, ctx)...)
		}
		return nil
	case map[string]interface{}:
		if len(v) == 0 {
			return []string{ctx + ": regime map must not be empty (omit the entry to use the system default)"}
		}
		var errs []string
		labels := make([]string, 0, len(v))
		for label := range v {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			sub := ctx + "." + label
			list, ok := v[label].([]interface{})
			if !ok {
				errs = append(errs, sub+": must be a tier list")
				continue
			}
			if len(list) == 0 {
				errs = append(errs, sub+": must not be empty")
				continue
			}
			if isRatchet {
				tiers, subErrs := parseTrailingRatchetTierList(list, sub)
				errs = append(errs, subErrs...)
				errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub)...)
			}
		}
		return errs
	default:
		return []string{ctx + ": must be a tier list or regime-keyed object"}
	}
}

func validateUserDefaultRegimeATR(entry map[string]interface{}) []string {
	if entry == nil {
		return []string{"user_defaults.regime_atr: must be an object"}
	}
	if len(entry) == 0 {
		return []string{"user_defaults.regime_atr: must not be empty"}
	}
	var errs []string
	allowed := map[string]bool{
		userCloseDefaultStopLossATRMultRegimeKey:  true,
		userCloseDefaultTrailingStopATRMultRegimeKey: true,
	}
	for k := range entry {
		if !allowed[k] {
			errs = append(errs, fmt.Sprintf("user_defaults.regime_atr: unknown key %q (only stop_loss_atr_mult_regime and trailing_stop_atr_mult_regime are allowed)", k))
		}
	}
	if raw, ok := entry[userCloseDefaultStopLossATRMultRegimeKey]; ok {
		errs = append(errs, validateUserCloseDefaultRegimeATRSubBlock(userCloseDefaultStopLossATRMultRegimeKey, raw, regimeSurfaceStopLoss)...)
	}
	if raw, ok := entry[userCloseDefaultTrailingStopATRMultRegimeKey]; ok {
		errs = append(errs, validateUserCloseDefaultRegimeATRSubBlock(userCloseDefaultTrailingStopATRMultRegimeKey, raw, regimeSurfaceTrailing)...)
	}
	return errs
}

func validateUserCloseDefaultRegimeATRSubBlock(subKey string, raw interface{}, surface regimeATRSurface) []string {
	ctx := fmt.Sprintf("user_defaults.regime_atr.%s", subKey)
	block, ok := raw.(map[string]interface{})
	if !ok || block == nil {
		return []string{ctx + ": must be an object"}
	}
	if len(block) == 0 {
		return []string{ctx + ": must not be empty"}
	}
	labels := canonicalTrendRegimeLabels
	if trendRaw, ok := block[regimeClassifierKey]; ok {
		if trendMap, ok := trendRaw.(map[string]interface{}); ok && len(trendMap) > 0 {
			labels = make([]string, 0, len(trendMap))
			for label := range trendMap {
				labels = append(labels, label)
			}
			sort.Strings(labels)
		}
	}
	_, errs := parseRegimeATRBlock(block, ctx, surface, labels)
	return errs
}

func validateUserCloseDefaultTrailingStopATRMultRegime(name string, raw interface{}) []string {
	ctx := fmt.Sprintf("user_defaults.close[%q].%s", name, userCloseDefaultTrailingStopATRMultRegimeKey)
	block, ok := raw.(map[string]interface{})
	if !ok || block == nil {
		return []string{ctx + ": must be an object"}
	}
	if len(block) == 0 {
		return []string{ctx + ": must not be empty"}
	}
	labels := canonicalTrendRegimeLabels
	if trendRaw, ok := block[regimeClassifierKey]; ok {
		if trendMap, ok := trendRaw.(map[string]interface{}); ok && len(trendMap) > 0 {
			labels = make([]string, 0, len(trendMap))
			for label := range trendMap {
				labels = append(labels, label)
			}
			sort.Strings(labels)
		}
	}
	_, errs := parseRegimeATRBlock(block, ctx, regimeSurfaceTrailing, labels)
	return errs
}

func applyUserCloseDefaultsToRef(ref *StrategyRef, defaults CloseDefaultsMap) bool {
	if ref == nil || len(defaults) == 0 {
		return false
	}
	if _, hasExplicit := closeTierListParam(ref.Params); hasExplicit {
		return false
	}
	entry, ok := closeDefaultsEntry(defaults, ref.Name)
	if !ok {
		return false
	}
	tp, ok := entry["tp_tiers"]
	if !ok || tp == nil {
		return false
	}
	if ref.Params == nil {
		ref.Params = map[string]interface{}{}
	}
	ref.Params["tp_tiers"] = tp
	return true
}

func userCloseDefaultTrailingStopATRMultRegime(defaults CloseDefaultsMap) (*RegimeATRBlock, bool) {
	entry, ok := closeDefaultsEntry(defaults, trailingTPRatchetRegimeCloseName)
	if !ok {
		return nil, false
	}
	raw, ok := entry[userCloseDefaultTrailingStopATRMultRegimeKey]
	if !ok || raw == nil {
		return nil, false
	}
	blockRaw, ok := raw.(map[string]interface{})
	if !ok || blockRaw == nil {
		return nil, false
	}
	return &RegimeATRBlock{raw: cloneInterfaceMap(blockRaw)}, true
}

func cloneInterfaceMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	blob, err := json.Marshal(in)
	if err != nil {
		out := make(map[string]interface{}, len(in))
		for k, v := range in {
			out[k] = v
		}
		return out
	}
	var out map[string]interface{}
	if err := json.Unmarshal(blob, &out); err != nil {
		out = make(map[string]interface{}, len(in))
		for k, v := range in {
			out[k] = v
		}
	}
	return out
}

func regimeATRBlockIsUseDefaultsOnly(b *RegimeATRBlock) bool {
	if b == nil || b.raw == nil {
		return false
	}
	if _, hasTrend := b.raw[regimeClassifierKey]; hasTrend {
		return false
	}
	ud, ok := b.raw["use_defaults"].(bool)
	return ok && ud
}

type userCloseDefaultRegimeATRBlocks struct {
	stopLoss *RegimeATRBlock
	trailing *RegimeATRBlock
}

func parseUserCloseDefaultRegimeATR(entry map[string]interface{}) (userCloseDefaultRegimeATRBlocks, bool) {
	if len(entry) == 0 {
		return userCloseDefaultRegimeATRBlocks{}, false
	}
	var out userCloseDefaultRegimeATRBlocks
	found := false
	if raw, ok := entry[userCloseDefaultStopLossATRMultRegimeKey]; ok && raw != nil {
		if blockRaw, ok := raw.(map[string]interface{}); ok && blockRaw != nil {
			out.stopLoss = &RegimeATRBlock{raw: cloneInterfaceMap(blockRaw)}
			found = true
		}
	}
	if raw, ok := entry[userCloseDefaultTrailingStopATRMultRegimeKey]; ok && raw != nil {
		if blockRaw, ok := raw.(map[string]interface{}); ok && blockRaw != nil {
			out.trailing = &RegimeATRBlock{raw: cloneInterfaceMap(blockRaw)}
			found = true
		}
	}
	return out, found
}

func applyUserCloseDefaultRegimeATR(sc *StrategyConfig, defaults map[string]interface{}) bool {
	if sc == nil || strategyUsesTrailingTPRatchetRegimeClose(*sc) {
		return false
	}
	udef, ok := parseUserCloseDefaultRegimeATR(defaults)
	if !ok {
		return false
	}
	injected := false
	if sc.StopLossATRMultRegime != nil && regimeATRBlockIsUseDefaultsOnly(sc.StopLossATRMultRegime) && udef.stopLoss != nil {
		sc.StopLossATRMultRegime = cloneRegimeATRBlock(udef.stopLoss)
		injected = true
	}
	if sc.TrailingStopATRMultRegime != nil && regimeATRBlockIsUseDefaultsOnly(sc.TrailingStopATRMultRegime) && udef.trailing != nil {
		sc.TrailingStopATRMultRegime = cloneRegimeATRBlock(udef.trailing)
		injected = true
	}
	return injected
}

func applyUserCloseDefaultRegimeATRs(cfg *Config) {
	defaults := cfg.userDefaultsRegimeATR()
	if len(defaults) == 0 {
		return
	}
	for i := range cfg.Strategies {
		applyUserCloseDefaultRegimeATR(&cfg.Strategies[i], defaults)
	}
}

func strategyUsesTrailingTPRatchetRegimeClose(sc StrategyConfig) bool {
	for _, ref := range sc.closeRefs() {
		if strings.ToLower(strings.TrimSpace(ref.Name)) == trailingTPRatchetRegimeCloseName {
			return true
		}
	}
	return false
}

func strategyHasExplicitStopOwner(sc StrategyConfig) bool {
	return sc.StopLossPct != nil ||
		sc.StopLossMarginPct != nil ||
		sc.TrailingStopPct != nil ||
		sc.TrailingStopATRMult != nil ||
		sc.StopLossATRMult != nil ||
		sc.StopLossATRMultRegime.IsConfigured() ||
		sc.TrailingStopATRMultRegime.IsConfigured() ||
		strategyUsesUnifiedRegimeClose(sc)
}

func applyUserCloseDefaultRatchetRegimeTrail(sc *StrategyConfig, defaults CloseDefaultsMap) bool {
	if sc == nil || !strategyUsesTrailingTPRatchetRegimeClose(*sc) || strategyHasExplicitStopOwner(*sc) {
		return false
	}
	block, ok := userCloseDefaultTrailingStopATRMultRegime(defaults)
	if !ok {
		return false
	}
	sc.TrailingStopATRMultRegime = block
	return true
}

func applyUserCloseDefaultRatchetRegimeTrails(cfg *Config) {
	defaults := cfg.userDefaultsClose()
	if len(defaults) == 0 {
		return
	}
	for i := range cfg.Strategies {
		applyUserCloseDefaultRatchetRegimeTrail(&cfg.Strategies[i], defaults)
	}
}

func applyUserCloseDefaults(cfg *Config) {
	defaults := cfg.userDefaultsClose()
	if len(defaults) == 0 {
		return
	}
	for i := range cfg.Strategies {
		applyUserCloseDefaultsToRef(cfg.Strategies[i].CloseStrategy, defaults)
	}
}

func cloneCloseDefaultsMap(defaults CloseDefaultsMap) CloseDefaultsMap {
	if defaults == nil {
		return nil
	}
	out := make(CloseDefaultsMap, len(defaults))
	for name, entry := range defaults {
		if entry == nil {
			out[name] = nil
			continue
		}
		out[name] = cloneInterfaceMap(entry)
	}
	return out
}
