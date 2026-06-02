package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// trailing_tp_ratchet (#844): a close strategy built on the strategy-level
// trailing ATR stop, where each cleared TP tier (a) tightens the trailing ATR
// multiple and (b) optionally scales out a fraction at that tier's price. The
// Python evaluator detects tier-fills by profit-distance (it places NO on-chain
// TPs) and returns post_tp_trailing_atr_mult; the runtime stamps it onto
// Position.PostTPTrailingATRMult (tighten-only) and the existing trailing-stop
// walker (effectiveTrailingStopPct / runHyperliquidTrailingStopUpdate) takes
// over at the new distance — no new live path.

const (
	trailingTPRatchetName       = "trailing_tp_ratchet"
	trailingTPRatchetRegimeName = "trailing_tp_ratchet_regime"
)

// isTrailingTPRatchetCloseName reports whether name is one of the two registered
// trailing-TP-ratchet close evaluators.
func isTrailingTPRatchetCloseName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case trailingTPRatchetName, trailingTPRatchetRegimeName:
		return true
	}
	return false
}

// isTrailingTPRatchetRegimeCloseName reports whether name is the regime-keyed
// variant (tp_tiers is a {label: [tiers]} map).
func isTrailingTPRatchetRegimeCloseName(name string) bool {
	return strings.ToLower(strings.TrimSpace(name)) == trailingTPRatchetRegimeName
}

// strategyUsesTrailingTPRatchet reports whether the strategy's close ref is a
// trailing-TP-ratchet evaluator.
func strategyUsesTrailingTPRatchet(sc StrategyConfig) bool {
	for _, ref := range sc.closeRefs() {
		if isTrailingTPRatchetCloseName(ref.Name) {
			return true
		}
	}
	return false
}

// stampTrailingTPRatchetMult applies a monotonic (tighten-only) update of
// pos.PostTPTrailingATRMult. A tighter trail is a strictly SMALLER multiple, so
// a looser (>=) candidate — or a stale repeat — is ignored. Returns true when
// the stamp changed. Caller must hold the state lock.
func stampTrailingTPRatchetMult(pos *Position, newMult *float64) bool {
	if pos == nil || pos.Quantity <= 0 || newMult == nil || *newMult <= 0 {
		return false
	}
	if pos.PostTPTrailingATRMult != nil && *pos.PostTPTrailingATRMult <= *newMult {
		return false
	}
	m := *newMult
	pos.PostTPTrailingATRMult = &m
	return true
}

// applyTrailingTPRatchet stamps the trail multiple carried on a close result
// onto the live position. No-ops unless the evaluator returned one (only the
// trailing_tp_ratchet family does), so it is safe to call after every perps /
// manual execute. Caller must hold the state lock.
func applyTrailingTPRatchet(s *StrategyState, symbol string, mult *float64) bool {
	if s == nil || mult == nil {
		return false
	}
	pos, ok := s.Positions[symbol]
	if !ok {
		return false
	}
	return stampTrailingTPRatchetMult(pos, mult)
}

// trailingTPRatchetReloadKey returns a deterministic fingerprint of the
// trailing-TP-ratchet close ref (name + tp_tiers shape) and whether one is
// configured. json.Marshal sorts map keys, so the key is stable across reloads.
func trailingTPRatchetReloadKey(sc StrategyConfig) (string, bool) {
	for _, ref := range sc.closeRefs() {
		if !isTrailingTPRatchetCloseName(ref.Name) {
			continue
		}
		b, err := json.Marshal(ref.Params["tp_tiers"])
		if err != nil {
			b = []byte(fmt.Sprintf("%v", ref.Params["tp_tiers"]))
		}
		return strings.ToLower(strings.TrimSpace(ref.Name)) + "|" + string(b), true
	}
	return "", false
}

// trailingTPRatchetParamsEqualForReload reports whether the trailing-TP-ratchet
// tier table is unchanged between two configs. A tier-table (or name) edit is a
// shape change that must be blocked while a position is open (#844) — the trail
// ratchet schedule was frozen at open.
func trailingTPRatchetParamsEqualForReload(a, b StrategyConfig) bool {
	ka, ua := trailingTPRatchetReloadKey(a)
	kb, ub := trailingTPRatchetReloadKey(b)
	if ua != ub {
		return false
	}
	if !ua {
		return true
	}
	return ka == kb
}

// validateTrailingTPRatchetClose validates a strategy's trailing-TP-ratchet
// close ref at config load. atrLabels is the regime vocabulary resolved from the
// strategy's regime_atr_window classifier (3-state adx or 7-state composite).
// Returns accumulated errors and whether the regime-keyed variant is in use (so
// the caller can enforce regime.enabled).
func validateTrailingTPRatchetClose(sc *StrategyConfig, atrLabels []string, prefix string) (errs []string, usesRegime bool) {
	if sc == nil {
		return nil, false
	}
	if len(atrLabels) == 0 {
		atrLabels = canonicalTrendRegimeLabels
	}
	for _, ref := range sc.closeRefs() {
		name := strings.ToLower(strings.TrimSpace(ref.Name))
		if !isTrailingTPRatchetCloseName(name) {
			continue
		}
		sub := fmt.Sprintf("%s.close_strategy(%s)", prefix, ref.Name)
		regimeVariant := isTrailingTPRatchetRegimeCloseName(name)
		if regimeVariant {
			usesRegime = true
		}

		// HL perps / manual only — the trailing-stop walker is HL-only.
		if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
			errs = append(errs, fmt.Sprintf("%s: %s is HL perps/manual only (got platform=%q type=%q)", sub, ref.Name, sc.Platform, sc.Type))
		}

		// The strategy-level trailing_stop_atr_mult is the initial (loose) trail
		// in effect before any tier fires.
		if sc.TrailingStopATRMult == nil || *sc.TrailingStopATRMult <= 0 {
			errs = append(errs, fmt.Sprintf("%s: requires a positive trailing_stop_atr_mult (the initial trail distance before any tier fires)", sub))
		}

		// Unknown-key guard: tp_tiers is the only owned param.
		for k := range ref.Params {
			if k != "tp_tiers" {
				errs = append(errs, fmt.Sprintf("%s: unknown param %q (allowed: tp_tiers)", sub, k))
			}
		}

		raw, ok := ref.Params["tp_tiers"]
		if !ok || raw == nil {
			errs = append(errs, fmt.Sprintf("%s: missing tp_tiers", sub))
			continue
		}

		if regimeVariant {
			tableRaw, ok := raw.(map[string]interface{})
			if !ok {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers: must be a regime-keyed object {label: [tiers]}, got %T", sub, raw))
				continue
			}
			if len(tableRaw) == 0 {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers: must define at least one regime", sub))
				continue
			}
			allowed := make(map[string]bool, len(atrLabels))
			for _, l := range atrLabels {
				allowed[l] = true
			}
			labels := make([]string, 0, len(tableRaw))
			for k := range tableRaw {
				labels = append(labels, k)
			}
			sort.Strings(labels)
			for _, label := range labels {
				if !allowed[label] {
					errs = append(errs, fmt.Sprintf("%s.tp_tiers: unknown regime label %q (expected one of: %s)", sub, label, strings.Join(atrLabels, ", ")))
					continue
				}
				errs = append(errs, validateRatchetTierList(tableRaw[label], fmt.Sprintf("%s.tp_tiers.%s", sub, label))...)
			}
			continue
		}

		// Plain variant: a flat list (or a {"default": [...]} map).
		if m, ok := raw.(map[string]interface{}); ok {
			def, hasDefault := m["default"]
			if !hasDefault {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers: object form requires a \"default\" key for %s (use %s for regime-keyed tables)", sub, trailingTPRatchetName, trailingTPRatchetRegimeName))
				continue
			}
			errs = append(errs, validateRatchetTierList(def, fmt.Sprintf("%s.tp_tiers.default", sub))...)
			continue
		}
		errs = append(errs, validateRatchetTierList(raw, fmt.Sprintf("%s.tp_tiers", sub))...)
	}
	return errs, usesRegime
}

// validateRatchetTierList validates one tier list (the per-regime list for the
// regime variant, or the flat list for the plain variant).
func validateRatchetTierList(raw interface{}, ctx string) []string {
	var errs []string
	items, ok := raw.([]interface{})
	if !ok {
		return []string{fmt.Sprintf("%s: must be a list of tiers, got %T", ctx, raw)}
	}
	if len(items) == 0 {
		return []string{fmt.Sprintf("%s: must have at least one tier", ctx)}
	}
	for idx, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s[%d]: must be an object, got %T", ctx, idx, item))
			continue
		}
		tctx := fmt.Sprintf("%s[%d]", ctx, idx)
		for k := range m {
			switch k {
			case "atr_multiple", "atr", "multiple", "close_fraction", "fraction",
				"trailing_mult_after", "tp_atr_fraction":
				// known (legacy atr/multiple/fraction aliases accepted at runtime)
			default:
				errs = append(errs, fmt.Sprintf("%s: unknown key %q (allowed: atr_multiple, close_fraction, trailing_mult_after, tp_atr_fraction)", tctx, k))
			}
		}

		trigRaw, hasTrig := firstNonNil(m, "atr_multiple", "atr", "multiple")
		if !hasTrig {
			errs = append(errs, fmt.Sprintf("%s: missing required 'atr_multiple'", tctx))
		} else if trig, err := floatFromAnyChecked(trigRaw); err != nil {
			errs = append(errs, fmt.Sprintf("%s.atr_multiple: %v", tctx, err))
		} else if trig <= 0 {
			errs = append(errs, fmt.Sprintf("%s.atr_multiple: must be > 0, got %g", tctx, trig))
		}

		if fracRaw, has := firstNonNil(m, "close_fraction", "fraction"); has {
			if frac, err := floatFromAnyChecked(fracRaw); err != nil {
				errs = append(errs, fmt.Sprintf("%s.close_fraction: %v", tctx, err))
			} else if frac < 0 || frac > 1 {
				errs = append(errs, fmt.Sprintf("%s.close_fraction: must be in [0, 1], got %g", tctx, frac))
			}
		}

		_, hasAbs := m["trailing_mult_after"]
		_, hasFrac := m["tp_atr_fraction"]
		if hasAbs && hasFrac {
			errs = append(errs, fmt.Sprintf("%s: trailing_mult_after and tp_atr_fraction are mutually exclusive (pick one trail-spec form)", tctx))
		}
		if hasAbs {
			if v, err := floatFromAnyChecked(m["trailing_mult_after"]); err != nil {
				errs = append(errs, fmt.Sprintf("%s.trailing_mult_after: %v", tctx, err))
			} else if v <= 0 {
				errs = append(errs, fmt.Sprintf("%s.trailing_mult_after: must be > 0, got %g", tctx, v))
			}
		}
		if hasFrac {
			if v, err := floatFromAnyChecked(m["tp_atr_fraction"]); err != nil {
				errs = append(errs, fmt.Sprintf("%s.tp_atr_fraction: %v", tctx, err))
			} else if v <= 0 {
				errs = append(errs, fmt.Sprintf("%s.tp_atr_fraction: must be > 0, got %g", tctx, v))
			}
		}
	}
	return errs
}
