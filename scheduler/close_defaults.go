package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// close_defaults.go implements the #866 user_close_defaults override layer — the
// middle of the three-layer close-default resolution:
//
//	system_close_defaults  (Go constant / Python mirror, the built-in fallback)
//	  → user_close_defaults  (this file — a top-level config.json block)
//	    → strategy_close_defaults  (inline tp_tiers on a strategy's close ref)
//
// Resolution is implemented by *injection at load*: for any close ref that omits
// tp_tiers, if user_close_defaults names that evaluator, its tp_tiers is copied
// into the ref's Params before validation/runtime. A ref that already carries an
// explicit tp_tiers (the strategy layer) is left untouched, and a ref with no
// matching user entry falls through to the evaluator's system default unchanged.
// Because injection happens inside loadConfig for both the old and new config on
// SIGHUP, downstream validation, runtime resolution, and hot-reload comparison
// all see the resolved tiers transparently — no separate plumbing required.

// closeDefaultsSupported is the set of close evaluators whose default ladder can
// be overridden via user_close_defaults (#866). Every member resolves its tier
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

const userCloseDefaultTrailingStopATRRegimeKey = "trailing_stop_atr_regime"

// userCloseDefaultRegimeATRKey is the reserved user_close_defaults section that
// holds fleet-wide per-regime ATR defaults for standalone
// stop_loss_atr_regime / trailing_stop_atr_regime owners configured with
// use_defaults:true (Phase 2, #1134). It is NOT a close-evaluator name — it is
// routed through its own validation branch in validateUserCloseDefaults and
// applied by applyUserCloseDefaultRegimeATRs at load, so it never trips the
// evaluator-name allowlist / tp_tiers-required gates. The ratchet-coupled
// trailing_stop_atr_regime owner keeps its own home under
// user_close_defaults["trailing_tp_ratchet_regime"] (#1133); the two are kept
// disjoint by an explicit close-strategy guard on each injection path.
const userCloseDefaultRegimeATRKey = "regime_atr"

// closeDefaultsTierEvaluator reports whether name accepts a user_close_defaults
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

// validateUserCloseDefaults checks the user_close_defaults block shape at load:
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
		// #1134: the reserved `regime_atr` section is NOT a close-evaluator
		// name — it holds fleet-wide per-regime ATR defaults for standalone
		// *_atr_regime use_defaults owners. Route it through a dedicated
		// validation branch BEFORE the evaluator-name allowlist so it never
		// trips the allowlist / stray-key / tp_tiers-required gates that the
		// evaluator-name entries still walk below.
		if normName == userCloseDefaultRegimeATRKey {
			errs = append(errs, validateUserCloseDefaultRegimeATR(name, entry)...)
			continue
		}
		if !closeDefaultsTierEvaluator(name) {
			errs = append(errs, fmt.Sprintf("user_close_defaults[%q]: not a tp_tiers close evaluator (allowed: %s)", name, strings.Join(closeDefaultsSupportedNames(), ", ")))
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
				errs = append(errs, fmt.Sprintf("user_close_defaults[%q]: unknown key %q (only tp_tiers and trailing_stop_atr_regime are allowed)", name, k))
			} else {
				errs = append(errs, fmt.Sprintf("user_close_defaults[%q]: unknown key %q (only tp_tiers is allowed)", name, k))
			}
		}
		tp, ok := entry["tp_tiers"]
		if !ok || tp == nil {
			errs = append(errs, fmt.Sprintf("user_close_defaults[%q]: missing tp_tiers", name))
			continue
		}
		// Deep-validate the ladder here so a malformed user default (empty list,
		// wrong type, non-monotonic ratchet tiers) is attributed to
		// user_close_defaults — not to the strategy it later injects into. An
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

// validateUserCloseDefaultTiers validates a user_close_defaults tp_tiers value
// (scalar list, or regime-keyed map for the *_regime ratchet) with errors
// attributed to the user_close_defaults block. Ratchet ladders also get the
// context-free monotonicity check; the regime-exhaustiveness and initial-trail
// checks stay per-strategy (they need the consuming strategy's classifier and
// trailing_stop_atr_mult).
func validateUserCloseDefaultTiers(name string, tp interface{}) []string {
	ctx := fmt.Sprintf("user_close_defaults[%q].tp_tiers", name)
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

func validateUserCloseDefaultTrailingStopATRRegime(name string, raw interface{}) []string {
	ctx := fmt.Sprintf("user_close_defaults[%q].%s", name, userCloseDefaultTrailingStopATRRegimeKey)
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

// validateUserCloseDefaultRegimeATR validates the reserved
// user_close_defaults["regime_atr"] section (Phase 2, #1134). The section holds
// optional stop_loss_atr_regime / trailing_stop_atr_regime sub-blocks, each a
// trend_regime-shaped map (or a use_defaults:true no-op) that overrides the
// system baseline for standalone *_atr_regime use_defaults consumers.
//
// Sub-blocks are shape-validated here via parseRegimeATRBlock (context-free:
// labels are derived from each block's own trend_regime keys, falling back to
// the canonical ADX vocabulary). Per-strategy classifier-vocabulary validation
// (composite labels, the #1124 bare-covers-subs exhaustiveness rule) happens
// later in validateRegimeATRConfig once a sub-block is injected onto a
// consuming strategy — mirroring the #1133 ratchet-trail layer, which deep-
// validates tiers here and regime-exhaustiveness per-strategy.
//
// A user sub-block set to use_defaults:true is accepted but is a documented
// no-op: parseRegimeATRBlock re-expands the system regimeATRDefaults baseline
// regardless of source (regime_atr.go), so it cannot form a distinct middle
// layer. The injection helper (applyUserCloseDefaultRegimeATR) still applies it
// verbatim, which resolves identically to the system table.
func validateUserCloseDefaultRegimeATR(name string, entry map[string]interface{}) []string {
	ctx := fmt.Sprintf("user_close_defaults[%q]", name)
	if entry == nil {
		return []string{ctx + ": must be an object"}
	}
	var errs []string
	allowedSubKeys := map[string]bool{
		"stop_loss_atr_regime":     true,
		"trailing_stop_atr_regime": true,
	}
	subKeys := make([]string, 0, len(entry))
	for k := range entry {
		subKeys = append(subKeys, k)
	}
	sort.Strings(subKeys)
	for _, k := range subKeys {
		if !allowedSubKeys[k] {
			errs = append(errs, fmt.Sprintf("%s: unknown key %q (only stop_loss_atr_regime and trailing_stop_atr_regime are allowed)", ctx, k))
			continue
		}
		raw := entry[k]
		subCtx := fmt.Sprintf("%s.%s", ctx, k)
		block, ok := raw.(map[string]interface{})
		if !ok || block == nil {
			errs = append(errs, fmt.Sprintf("%s: must be an object", subCtx))
			continue
		}
		if len(block) == 0 {
			errs = append(errs, fmt.Sprintf("%s: must not be empty (omit the sub-block to inherit the system default)", subCtx))
			continue
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
		surface := regimeSurfaceStopLoss
		if k == "trailing_stop_atr_regime" {
			surface = regimeSurfaceTrailing
		}
		_, subErrs := parseRegimeATRBlock(block, subCtx, surface, labels)
		errs = append(errs, subErrs...)
	}
	return errs
}

// applyUserCloseDefaultsToRef injects the user_close_defaults tp_tiers for ref's
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
	if cfg == nil || len(cfg.UserCloseDefaults) == 0 {
		return
	}
	for i := range cfg.Strategies {
		applyUserCloseDefaultRatchetRegimeTrail(&cfg.Strategies[i], cfg.UserCloseDefaults)
	}
}

// userCloseDefaultRegimeATRSubBlocks returns the parsed user-level
// regime_atr sub-blocks (stop_loss_atr_regime / trailing_stop_atr_regime) and
// whether the reserved section is present at all (#1134). The returned maps are
// the raw JSON shapes, deep-cloned by the caller as needed.
func userCloseDefaultRegimeATRSubBlocks(defaults CloseDefaultsMap) (stopLoss, trailing map[string]interface{}, present bool) {
	entry, ok := closeDefaultsEntry(defaults, userCloseDefaultRegimeATRKey)
	if !ok {
		return nil, nil, false
	}
	if raw, has := entry["stop_loss_atr_regime"]; has && raw != nil {
		if m, ok := raw.(map[string]interface{}); ok {
			stopLoss = m
		}
	}
	if raw, has := entry["trailing_stop_atr_regime"]; has && raw != nil {
		if m, ok := raw.(map[string]interface{}); ok {
			trailing = m
		}
	}
	return stopLoss, trailing, true
}

// applyUserCloseDefaultRegimeATR injects the user-level regime_atr default
// onto a standalone *_atr_regime stop owner that is use_defaults-only (Phase 2,
// #1134). Precedence: system regimeATRDefaults < user_close_defaults.regime_atr
// < per-strategy explicit trend_regime map. The per-strategy layer wins because
// IsUseDefaultsOnly is false for an explicit trend_regime block, so injection
// is skipped.
//
// Safety-critical guard: ratchet/manual strategies are excluded via
// !strategyUsesTrailingTPRatchetRegimeClose. The type=manual synthesized trail
// (resolveManualRatchetRegimeTrailBlock, config.go) runs in the per-strategy
// loop BEFORE this hook and assigns a synthesized {use_defaults:true}
// TrailingStopATRRegime with close_strategy=trailing_tp_ratchet_regime; without
// this guard the Phase-2 predicate ("standalone *_atr_regime is use_defaults-
// only") would match that synthesized block and overwrite a live manual SL
// owner, routing it to the wrong default home. The guard makes Phase-2 disjoint
// from the #1133 ratchet-coupled trail, which keeps its own home under
// user_close_defaults["trailing_tp_ratchet_regime"].
//
// Injection replaces the use_defaults expansion in place on the existing field
// (no new field), so the seven mutually-exclusive HL stop owners stay
// mutually exclusive — the sole-owner mutex (regime_atr.go) still trips on any
// accidental second owner, and per-strategy classifier-vocabulary validation
// runs in validateRegimeATRConfig after this hook.
func applyUserCloseDefaultRegimeATR(sc *StrategyConfig, defaults CloseDefaultsMap) bool {
	if sc == nil || strategyUsesTrailingTPRatchetRegimeClose(*sc) {
		return false
	}
	stopLoss, trailing, present := userCloseDefaultRegimeATRSubBlocks(defaults)
	if !present {
		return false
	}
	applied := false
	if stopLoss != nil && sc.StopLossATRRegime.IsUseDefaultsOnly() {
		sc.StopLossATRRegime = &RegimeATRBlock{raw: cloneInterfaceMap(stopLoss)}
		applied = true
	}
	if trailing != nil && sc.TrailingStopATRRegime.IsUseDefaultsOnly() {
		sc.TrailingStopATRRegime = &RegimeATRBlock{raw: cloneInterfaceMap(trailing)}
		applied = true
	}
	return applied
}

// applyUserCloseDefaultRegimeATRs injects user_close_defaults["regime_atr"]
// into every eligible standalone *_atr_regime use_defaults owner. Called once
// per load (and per SIGHUP reload) after the #1133 ratchet-trail injection and
// the per-strategy close-ref normalization/auto-config, before validation — so
// the injected blocks flow through validateRegimeATRConfig's per-strategy
// classifier-vocabulary + sole-owner checks.
func applyUserCloseDefaultRegimeATRs(cfg *Config) {
	if cfg == nil || len(cfg.UserCloseDefaults) == 0 {
		return
	}
	for i := range cfg.Strategies {
		applyUserCloseDefaultRegimeATR(&cfg.Strategies[i], cfg.UserCloseDefaults)
	}
}

// applyUserCloseDefaults injects user_close_defaults into every strategy's close
// ref. Called once per load (and per SIGHUP reload) after close-ref
// normalization, before validation.
func applyUserCloseDefaults(cfg *Config) {
	if cfg == nil || len(cfg.UserCloseDefaults) == 0 {
		return
	}
	for i := range cfg.Strategies {
		applyUserCloseDefaultsToRef(cfg.Strategies[i].CloseStrategy, cfg.UserCloseDefaults)
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
