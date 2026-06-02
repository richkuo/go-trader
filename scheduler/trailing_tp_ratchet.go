package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// trailing_tp_ratchet (#844)
//
// A trailing ATR stop where each cleared TP tier (a) tightens the trailing-stop
// ATR multiple and (b) optionally closes `close_fraction` of the position
// (`close_fraction == 0` is a trail-only rung). The "pure trailing" strategy is
// every tier at `close_fraction: 0`.
//
// Ownership split:
//   - The Python close evaluator (shared_strategies/close/trailing_tp_ratchet.py)
//     owns the per-tier partial closes (paper + backtest).
//   - The Go side (this file) owns the on-chain trailing stop: it detects the
//     highest cleared tier from entry-ATR profit distance (frozen pos.Regime +
//     frozen pos.EntryATR) and stamps pos.PostTPTrailingATRMult monotonically.
//     The existing trailing-stop walker (effectiveTrailingStopPct ->
//     runHyperliquidTrailingStopUpdate / runHyperliquidTrailingStopPaper) then
//     cancel+replaces the on-chain SL at the tightened distance.
//
// It places NO on-chain TP orders, so it is deliberately kept OUT of
// isTieredTPATRCloseName / strategyUsesTieredTPATRClose and the on-chain
// protection suppression list. Its regime form keys `tp_tiers` on the position
// regime ({label: [tiers]}) rather than a top-level `trend_regime` block, so it
// is NOT detected as a #841 unified per-regime close (which would reject the
// strategy-level trailing_stop_atr_mult it depends on). Scope: HL perps (v1).

const (
	trailingTPRatchetCloseName       = "trailing_tp_ratchet"
	trailingTPRatchetRegimeCloseName = "trailing_tp_ratchet_regime"
)

// isTrailingTPRatchetCloseName reports whether name is one of the trailing-TP
// ratchet close evaluators.
func isTrailingTPRatchetCloseName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case trailingTPRatchetCloseName, trailingTPRatchetRegimeCloseName:
		return true
	}
	return false
}

// strategyUsesTrailingTPRatchetClose reports whether sc's close ref is a
// trailing-TP ratchet evaluator.
func strategyUsesTrailingTPRatchetClose(sc StrategyConfig) bool {
	for _, ref := range sc.closeRefs() {
		if isTrailingTPRatchetCloseName(ref.Name) {
			return true
		}
	}
	return false
}

// trailingTPRatchetCloseRef returns sc's trailing-TP ratchet close ref (and
// true) if present.
func trailingTPRatchetCloseRef(sc StrategyConfig) (StrategyRef, bool) {
	for _, ref := range sc.closeRefs() {
		if isTrailingTPRatchetCloseName(ref.Name) {
			return ref, true
		}
	}
	return StrategyRef{}, false
}

// trailingTPRatchetTier is one rung of a trailing_tp_ratchet ladder. A cleared
// rung tightens the trailing ATR mult to TrailMultAfter (absolute) or
// TPATRFraction*ATRMultiple (relative — the two are mutually exclusive) and
// optionally closes CloseFraction (cumulative; 0 == trail-only rung).
type trailingTPRatchetTier struct {
	ATRMultiple    float64
	CloseFraction  float64
	TrailMultAfter float64 // absolute ATR mult armed once cleared (0 when TPATRFraction set)
	TPATRFraction  float64 // relative: TPATRFraction*ATRMultiple (0 when TrailMultAfter set)
}

// resolvedTrailMult returns the absolute trailing ATR mult this rung arms once
// cleared.
func (t trailingTPRatchetTier) resolvedTrailMult() float64 {
	if t.TrailMultAfter > 0 {
		return t.TrailMultAfter
	}
	if t.TPATRFraction > 0 && t.ATRMultiple > 0 {
		return t.TPATRFraction * t.ATRMultiple
	}
	return 0
}

// trailingTPRatchetTierListRaw returns the raw tier list for regime (frozen at
// open). `tp_tiers` is a bare list (plain form) or a map of regime label ->
// list (regime form). Returns (nil, false) when no list resolves.
func trailingTPRatchetTierListRaw(params map[string]interface{}, regime string) ([]interface{}, bool) {
	raw, ok := params["tp_tiers"]
	if !ok {
		raw, ok = params["tiers"]
	}
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case []interface{}:
		return v, true
	case map[string]interface{}:
		if regime == "" {
			return nil, false
		}
		lst, ok := v[regime].([]interface{})
		if !ok {
			return nil, false
		}
		return lst, true
	}
	return nil, false
}

// parseTrailingTPRatchetTierList parses a single tier list, returning the sorted
// tiers and any validation errors. Each tier requires a positive atr_multiple
// and exactly one of trailing_mult_after (>0) or tp_atr_fraction (>0);
// close_fraction is optional (default 0, range [0,1]).
func parseTrailingTPRatchetTierList(raw []interface{}, ctxPrefix string) ([]trailingTPRatchetTier, []string) {
	var errs []string
	tiers := make([]trailingTPRatchetTier, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.tiers[%d]: must be an object", ctxPrefix, i))
			continue
		}
		for k := range m {
			switch k {
			case "atr_multiple", "close_fraction", "trailing_mult_after", "tp_atr_fraction":
			default:
				errs = append(errs, fmt.Sprintf("%s.tiers[%d]: unknown key %q (allowed: atr_multiple, close_fraction, trailing_mult_after, tp_atr_fraction)", ctxPrefix, i, k))
			}
		}

		mult, err := floatFromAnyChecked(m["atr_multiple"])
		if err != nil || mult <= 0 {
			errs = append(errs, fmt.Sprintf("%s.tiers[%d]: atr_multiple must be a positive number", ctxPrefix, i))
			continue
		}
		tier := trailingTPRatchetTier{ATRMultiple: mult}
		if v, present := m["close_fraction"]; present {
			frac, err := floatFromAnyChecked(v)
			if err != nil || frac < 0 || frac > 1 {
				errs = append(errs, fmt.Sprintf("%s.tiers[%d]: close_fraction must be a number in [0,1]", ctxPrefix, i))
				continue
			}
			tier.CloseFraction = frac
		}

		_, hasAbs := m["trailing_mult_after"]
		_, hasFrac := m["tp_atr_fraction"]
		if hasAbs == hasFrac { // both set or neither set
			errs = append(errs, fmt.Sprintf("%s.tiers[%d]: set exactly one of trailing_mult_after (absolute ATR mult) or tp_atr_fraction (relative)", ctxPrefix, i))
			continue
		}
		if hasAbs {
			v, err := floatFromAnyChecked(m["trailing_mult_after"])
			if err != nil || v <= 0 {
				errs = append(errs, fmt.Sprintf("%s.tiers[%d]: trailing_mult_after must be a positive number", ctxPrefix, i))
				continue
			}
			tier.TrailMultAfter = v
		} else {
			v, err := floatFromAnyChecked(m["tp_atr_fraction"])
			if err != nil || v <= 0 {
				errs = append(errs, fmt.Sprintf("%s.tiers[%d]: tp_atr_fraction must be a positive number", ctxPrefix, i))
				continue
			}
			tier.TPATRFraction = v
		}
		tiers = append(tiers, tier)
	}
	sort.SliceStable(tiers, func(a, b int) bool { return tiers[a].ATRMultiple < tiers[b].ATRMultiple })
	return tiers, errs
}

// resolveTrailingTPRatchetTiers parses sc's ratchet tier list for the given
// (frozen) regime. Returns (nil, false) when the strategy is not a ratchet
// strategy, no tier list resolves for the regime, or the tiers are malformed.
func resolveTrailingTPRatchetTiers(sc StrategyConfig, regime string) ([]trailingTPRatchetTier, bool) {
	ref, ok := trailingTPRatchetCloseRef(sc)
	if !ok {
		return nil, false
	}
	raw, ok := trailingTPRatchetTierListRaw(ref.Params, regime)
	if !ok {
		return nil, false
	}
	tiers, errs := parseTrailingTPRatchetTierList(raw, trailingTPRatchetCloseName)
	if len(errs) > 0 || len(tiers) == 0 {
		return nil, false
	}
	return tiers, true
}

// highestClearedTrailingTPRatchetMult returns the absolute trailing ATR mult of
// the highest tier cleared by atrProfit (tiers must be sorted ascending by
// ATRMultiple). cleared is false when no tier is cleared.
func highestClearedTrailingTPRatchetMult(tiers []trailingTPRatchetTier, atrProfit float64) (mult float64, cleared bool) {
	for _, t := range tiers {
		if atrProfit+1e-9 >= t.ATRMultiple {
			if m := t.resolvedTrailMult(); m > 0 {
				mult = m
				cleared = true
			}
		}
	}
	return mult, cleared
}

// runTrailingTPRatchetAdjustment ratchets the trailing stop tighter as TP tiers
// clear for a trailing_tp_ratchet strategy. It detects the highest cleared tier
// from entry-ATR profit distance (frozen pos.Regime + frozen pos.EntryATR),
// resolves that rung's tightened trailing ATR mult, and stamps
// pos.PostTPTrailingATRMult monotonically (smaller absolute mult only — never
// loosens). It places/cancels NO order itself; the existing trailing-stop
// walker performs the on-chain cancel+replace once the tighter mult is in
// effect.
//
// Returns the effective PostTPTrailingATRMult after the adjustment (nil when
// none), so the caller can feed it into the trailing-walker position snapshot
// for same-cycle tightening. HL perps only.
func runTrailingTPRatchetAdjustment(sc StrategyConfig, stratState *StrategyState, symbol string, mark float64, mu *sync.RWMutex, logger *StrategyLogger) *float64 {
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		return nil
	}
	if !strategyUsesTrailingTPRatchetClose(sc) || stratState == nil || symbol == "" || mark <= 0 {
		return nil
	}

	mu.RLock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.AvgCost <= 0 || pos.EntryATR <= 0 || pos.Side == "" {
		mu.RUnlock()
		return nil
	}
	side := pos.Side
	avgCost := pos.AvgCost
	entryATR := pos.EntryATR
	regime := pos.Regime
	var currentMult *float64
	if pos.PostTPTrailingATRMult != nil {
		v := *pos.PostTPTrailingATRMult
		currentMult = &v
	}
	mu.RUnlock()

	tiers, ok := resolveTrailingTPRatchetTiers(sc, regime)
	if !ok {
		return currentMult
	}

	atrProfit := (mark - avgCost) / entryATR
	if side == "short" {
		atrProfit = (avgCost - mark) / entryATR
	}
	newMult, cleared := highestClearedTrailingTPRatchetMult(tiers, atrProfit)
	if !cleared || newMult <= 0 {
		return currentMult
	}
	// Monotonic: only tighten (a smaller absolute ATR mult is a tighter trail);
	// never loosen.
	if currentMult != nil && newMult >= *currentMult {
		return currentMult
	}

	mu.Lock()
	p, ok := stratState.Positions[symbol]
	if !ok || p == nil || p.Quantity <= 0 || p.Side != side {
		mu.Unlock()
		return currentMult
	}
	if p.PostTPTrailingATRMult == nil || newMult < *p.PostTPTrailingATRMult {
		m := newMult
		p.PostTPTrailingATRMult = &m
		if logger != nil {
			logger.Info("trailing_tp_ratchet: tier cleared (atr_profit=%.2f) — trailing mult tightened to %.3fx for %s", atrProfit, newMult, symbol)
		}
	}
	eff := *p.PostTPTrailingATRMult
	mu.Unlock()
	return &eff
}

// validateTrailingTPRatchetCloseRef validates a trailing_tp_ratchet[_regime]
// close ref. Returns the errors and whether the ref is regime-keyed (so the
// caller can require regime.enabled=true). atrLabels is the strategy's resolved
// regime vocabulary (composite 7 / ADX 3).
func validateTrailingTPRatchetCloseRef(sc StrategyConfig, ref StrategyRef, atrLabels []string, prefix string) (errs []string, usesRegime bool) {
	name := strings.ToLower(strings.TrimSpace(ref.Name))
	subPrefix := fmt.Sprintf("%s.close_strategy(%s)", prefix, ref.Name)

	// Scope: HL perps only in v1 (the trailing-stop walker is perps-only;
	// manual would need separate walker wiring — mirrors sl_after:trail_from_here).
	if sc.Platform != "hyperliquid" || sc.Type != "perps" {
		errs = append(errs, fmt.Sprintf("%s: %s is HL perps only (v1)", subPrefix, ref.Name))
	}
	// The trailing stop is this strategy's SL owner: require a positive initial
	// trailing_stop_atr_mult (the loose distance in effect before any tier fires).
	if sc.TrailingStopATRMult == nil || *sc.TrailingStopATRMult <= 0 {
		errs = append(errs, fmt.Sprintf("%s: requires a positive strategy-level trailing_stop_atr_mult (the initial trailing distance before any tier clears)", subPrefix))
	}
	for k := range ref.Params {
		switch k {
		case "tp_tiers", "tiers":
		default:
			errs = append(errs, fmt.Sprintf("%s: unknown param %q (allowed: tp_tiers)", subPrefix, k))
		}
	}

	raw, hasTiers := ref.Params["tp_tiers"]
	if !hasTiers {
		raw, hasTiers = ref.Params["tiers"]
	}
	if !hasTiers {
		errs = append(errs, fmt.Sprintf("%s: missing tp_tiers", subPrefix))
		return errs, name == trailingTPRatchetRegimeCloseName
	}

	if name == trailingTPRatchetRegimeCloseName {
		usesRegime = true
		table, ok := raw.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: tp_tiers must be a regime-keyed object {label: [tiers]}", subPrefix))
			return errs, usesRegime
		}
		valid := make(map[string]bool, len(atrLabels))
		for _, l := range atrLabels {
			valid[l] = true
		}
		seen := make(map[string]bool, len(table))
		// Sort keys for deterministic error ordering.
		keys := make([]string, 0, len(table))
		for k := range table {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, label := range keys {
			if !valid[label] {
				errs = append(errs, fmt.Sprintf("%s: regime %q is not in the window vocabulary %v", subPrefix, label, atrLabels))
				continue
			}
			seen[label] = true
			items, ok := table[label].([]interface{})
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: regime %q tiers must be a list", subPrefix, label))
				continue
			}
			if len(items) == 0 {
				errs = append(errs, fmt.Sprintf("%s: regime %q must have at least one tier", subPrefix, label))
			}
			_, subErrs := parseTrailingTPRatchetTierList(items, fmt.Sprintf("%s.%s", subPrefix, label))
			errs = append(errs, subErrs...)
		}
		for _, l := range atrLabels {
			if !seen[l] {
				errs = append(errs, fmt.Sprintf("%s: missing tier table for regime %q (all %v required)", subPrefix, l, atrLabels))
			}
		}
		return errs, usesRegime
	}

	// Plain form: tp_tiers is a list.
	items, ok := raw.([]interface{})
	if !ok {
		errs = append(errs, fmt.Sprintf("%s: tp_tiers must be a list of tiers (use trailing_tp_ratchet_regime for a regime-keyed table)", subPrefix))
		return errs, false
	}
	if len(items) == 0 {
		errs = append(errs, fmt.Sprintf("%s: tp_tiers must have at least one tier", subPrefix))
	}
	_, subErrs := parseTrailingTPRatchetTierList(items, subPrefix)
	errs = append(errs, subErrs...)
	return errs, false
}

// trailingTPRatchetParamsEqualForReload reports whether the trailing_tp_ratchet
// close-ref params are unchanged between two configs. The tier table is armed
// against the position at open (each cleared rung ratchets the trail), so a
// mid-position change re-arms a plan the open didn't respect — callers block it
// while open (consistent with sl_after / unified-block reload gating).
func trailingTPRatchetParamsEqualForReload(a, b StrategyConfig) bool {
	var ap, bp map[string]interface{}
	if ref, ok := trailingTPRatchetCloseRef(a); ok {
		ap = ref.Params
	}
	if ref, ok := trailingTPRatchetCloseRef(b); ok {
		bp = ref.Params
	}
	return reflect.DeepEqual(ap, bp)
}
