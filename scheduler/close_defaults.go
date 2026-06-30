package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// close_defaults.go implements the #866 user_defaults override layer — the
// middle of the three-layer close-default resolution:
//
//	system_close_defaults  (Go constant / Python mirror, the built-in fallback)
//	  → user_defaults        (this file — a top-level config.json block)
//	    → strategy_close_defaults  (inline tp_tiers on a strategy's close ref)
//
// Resolution is implemented by *injection at load*: for any close ref that omits
// tp_tiers, if user_defaults.close names that evaluator, its tp_tiers is copied
// into the ref's Params before validation/runtime. A ref that already carries an
// explicit tp_tiers (the strategy layer) is left untouched, and a ref with no
// matching user entry falls through to the evaluator's system default unchanged.
// Because injection happens inside loadConfig for both the old and new config on
// SIGHUP, downstream validation, runtime resolution, and hot-reload comparison
// all see the resolved tiers transparently — no separate plumbing required.

// closeDefaultsSupported is the set of close evaluators whose default ladder can
// be overridden via user_defaults.close (#866). Every member resolves its tier
// list purely through tp_tiers, so an injected tp_tiers cleanly wins over the
// system default with no precedence ambiguity.
//
// Deliberately EXCLUDED:
//   - tiered_tp_atr_regime / tiered_tp_atr_live_regime: their use_defaults form
//     expands a RegimeATRBlock baseline (regimeATRDefaults), and that interacts
//     with an injected tp_tiers in per-regime ways that belong with the
//     per-regime retune in #870 — not this mechanism issue.
//   - tiered_tp_atr_live_regime_dynamic: trend_regime-shaped, no tp_tiers.
var closeDefaultsSupported = map[string]struct{}{
	"tiered_tp_pct":              {},
	"tiered_tp_atr":              {},
	"tiered_tp_atr_live":         {},
	"trailing_tp_ratchet":        {},
	"trailing_tp_ratchet_regime": {},
}

const (
	userCloseDefaultTrailingStopATRRegimeKey = "trailing_stop_atr_regime"
	userCloseDefaultStopLossATRRegimeKey     = "stop_loss_atr_regime"
	userCloseDefaultRegimeATRKey             = "regime_atr"
)

// closeDefaultsTierEvaluator reports whether name accepts a user_defaults.close
// override (see closeDefaultsSupported).
func closeDefaultsTierEvaluator(name string) bool {
	_, ok := closeDefaultsSupported[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// closeDefaultsSupportedNames returns the sorted supported evaluator names for
// operator-facing error text.
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

// validateUserDefaults checks the user_defaults block shape at load.
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

// validateUserCloseDefaults checks the user_defaults.close block shape at load:
// every key must be a tp_tiers-shaped close evaluator, and every entry must
// carry a non-nil tp_tiers and no other keys. The tier *contents* are validated
// per-evaluator once injected into a consuming strategy (so a regime ladder is
// checked against that strategy's classifier vocabulary, etc.).
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
			if normName == trailingTPRatchetRegimeCloseName && k == userCloseDefaultTrailingStopATRRegimeKey {
				continue
			}
			if normName == trailingTPRatchetRegimeCloseName {
				errs = append(errs, fmt.Sprintf("user_defaults.close[%q]: unknown key %q (only tp_tiers and trailing_stop_atr_regime are allowed)", name, k))
			} else {
				errs = append(errs, fmt.Sprintf("user_defaults.close[%q]: unknown key %q (only tp_tiers is allowed)", name, k))
			}
		}
		tp, ok := entry["tp_tiers"]
		if !ok || tp == nil {
			errs = append(errs, fmt.Sprintf("user_defaults.close[%q]: missing tp_tiers", name))
			continue
		}
		// Deep-validate the ladder here so a malformed user default (empty list,
		// wrong type, non-monotonic ratchet tiers) is attributed to
		// user_defaults.close — not to the strategy it later injects into. An
		// empty tp_tiers is rejected loudly: it would otherwise inject `[]` and
		// silently suppress the system default (runtime resolves to zero tiers).
		errs = append(errs, validateUserCloseDefaultTiers(name, tp)...)
		if normName == trailingTPRatchetRegimeCloseName {
			if raw, ok := entry[userCloseDefaultTrailingStopATRRegimeKey]; ok {
				errs = append(errs, validateUserCloseDefaultTrailingStopATRRegime(name, raw)...)
			}
		}
	}
	return errs
}

// validateUserCloseDefaultTiers validates a user_defaults.close tp_tiers value
// (scalar list, or regime-keyed map for the *_regime ratchet) with errors
// attributed to the user_defaults.close block. Ratchet ladders also get the
// context-free monotonicity check; the regime-exhaustiveness and initial-trail
// checks stay per-strategy (they need the consuming strategy's classifier and
// trailing_stop_atr_mult).
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
		userCloseDefaultStopLossATRRegimeKey:     true,
		userCloseDefaultTrailingStopATRRegimeKey: true,
	}
	for k := range entry {
		if !allowed[k] {
			errs = append(errs, fmt.Sprintf("user_defaults.regime_atr: unknown key %q (only stop_loss_atr_regime and trailing_stop_atr_regime are allowed)", k))
		}
	}
	if raw, ok := entry[userCloseDefaultStopLossATRRegimeKey]; ok {
		errs = append(errs, validateUserCloseDefaultRegimeATRSubBlock(userCloseDefaultStopLossATRRegimeKey, raw, regimeSurfaceStopLoss)...)
	}
	if raw, ok := entry[userCloseDefaultTrailingStopATRRegimeKey]; ok {
		errs = append(errs, validateUserCloseDefaultRegimeATRSubBlock(userCloseDefaultTrailingStopATRRegimeKey, raw, regimeSurfaceTrailing)...)
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

func validateUserCloseDefaultTrailingStopATRRegime(name string, raw interface{}) []string {
	ctx := fmt.Sprintf("user_defaults.close[%q].%s", name, userCloseDefaultTrailingStopATRRegimeKey)
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

// applyUserCloseDefaultsToRef injects the user_defaults.close tp_tiers for ref's
// evaluator when the ref omits its own tp_tiers (the strategy layer wins). A
// no-op when ref is nil, already carries tp_tiers, or has no matching user
// entry. Returns true when an injection occurred (for logging/tests).
func applyUserCloseDefaultsToRef(ref *StrategyRef, defaults CloseDefaultsMap) bool {
	if ref == nil || len(defaults) == 0 {
		return false
	}
	if _, hasExplicit := closeTierListParam(ref.Params); hasExplicit {
		return false // strategy_close_defaults layer wins
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

func userCloseDefaultTrailingStopATRRegime(defaults CloseDefaultsMap) (*RegimeATRBlock, bool) {
	entry, ok := closeDefaultsEntry(defaults, trailingTPRatchetRegimeCloseName)
	if !ok {
		return nil, false
	}
	raw, ok := entry[userCloseDefaultTrailingStopATRRegimeKey]
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

// regimeATRBlockIsUseDefaultsOnly reports whether the operator supplied only
// use_defaults:true (no explicit trend_regime map). Safe before ResolveSurface.
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
	if raw, ok := entry[userCloseDefaultStopLossATRRegimeKey]; ok && raw != nil {
		if blockRaw, ok := raw.(map[string]interface{}); ok && blockRaw != nil {
			out.stopLoss = &RegimeATRBlock{raw: cloneInterfaceMap(blockRaw)}
			found = true
		}
	}
	if raw, ok := entry[userCloseDefaultTrailingStopATRRegimeKey]; ok && raw != nil {
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
	if sc.StopLossATRRegime != nil && regimeATRBlockIsUseDefaultsOnly(sc.StopLossATRRegime) && udef.stopLoss != nil {
		sc.StopLossATRRegime = cloneRegimeATRBlock(udef.stopLoss)
		injected = true
	}
	if sc.TrailingStopATRRegime != nil && regimeATRBlockIsUseDefaultsOnly(sc.TrailingStopATRRegime) && udef.trailing != nil {
		sc.TrailingStopATRRegime = cloneRegimeATRBlock(udef.trailing)
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
		sc.StopLossATRRegime.IsConfigured() ||
		sc.TrailingStopATRRegime.IsConfigured() ||
		strategyUsesUnifiedRegimeClose(sc)
}

func applyUserCloseDefaultRatchetRegimeTrail(sc *StrategyConfig, defaults CloseDefaultsMap) bool {
	if sc == nil || !strategyUsesTrailingTPRatchetRegimeClose(*sc) || strategyHasExplicitStopOwner(*sc) {
		return false
	}
	block, ok := userCloseDefaultTrailingStopATRRegime(defaults)
	if !ok {
		return false
	}
	sc.TrailingStopATRRegime = block
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

// applyUserCloseDefaults injects user_defaults.close into every strategy's close
// ref. Called once per load (and per SIGHUP reload) after close-ref
// normalization, before validation.
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
