package main

import (
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
		if !closeDefaultsTierEvaluator(name) {
			errs = append(errs, fmt.Sprintf("user_close_defaults[%q]: not a tp_tiers close evaluator (allowed: %s)", name, strings.Join(closeDefaultsSupportedNames(), ", ")))
			continue
		}
		for k := range entry {
			if k != "tp_tiers" {
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
	entry, ok := defaults[strings.ToLower(strings.TrimSpace(ref.Name))]
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
