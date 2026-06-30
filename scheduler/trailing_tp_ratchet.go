package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const (
	trailingTPRatchetCloseName       = "trailing_tp_ratchet"
	trailingTPRatchetRegimeCloseName = "trailing_tp_ratchet_regime"
)

// trailingRatchetTier is one rung of a trailing_tp_ratchet* close ref.
type trailingRatchetTier struct {
	ATRMultiple       float64
	CloseFraction     float64
	TrailingMultAfter float64
}

func isTrailingTPRatchetCloseName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case trailingTPRatchetCloseName, trailingTPRatchetRegimeCloseName:
		return true
	}
	return false
}

func strategyUsesTrailingTPRatchetClose(sc StrategyConfig) bool {
	for _, ref := range sc.closeRefs() {
		if isTrailingTPRatchetCloseName(ref.Name) {
			return true
		}
	}
	return false
}

// defaultTrailingRatchetTiers is the canonical conservative fallback ladder
// (#866) used when a trailing_tp_ratchet* close ref omits tp_tiers (or sets
// use_defaults:true). Pure let-it-ride: starting from the operator's
// trailing_stop_atr_mult, tighten to 1.5 / 1.0 / 0.5 ×ATR at 2 / 2.5 / 3 ×ATR
// profit, never force-selling (close_fraction 0). It is the single source of
// truth on the Go side; mirrored in
// shared_strategies/close/trailing_tp_ratchet.py as DEFAULT_RATCHET_TIERS — keep
// the two in sync. The regime variant broadcasts this same ladder to every
// classifier label (per-regime group differentiation + per-regime opening trail
// land in #870).
//
// Precondition: the first rung tightens to 1.5×ATR, so a strategy relying on
// this default must set trailing_stop_atr_mult >= 1.5 — otherwise
// validateTrailingRatchetInitialTrail rejects it at load (a looser first rung
// would silently no-op at runtime). The reported bug is fully fixed for trails
// >= 1.5×ATR; a tighter initial trail still needs an explicit tp_tiers.
func defaultTrailingRatchetTiers() []trailingRatchetTier {
	return []trailingRatchetTier{
		{ATRMultiple: 2.0, CloseFraction: 0, TrailingMultAfter: 1.5},
		{ATRMultiple: 2.5, CloseFraction: 0, TrailingMultAfter: 1.0},
		{ATRMultiple: 3.0, CloseFraction: 0, TrailingMultAfter: 0.8},
	}
}

// ratchetTierGroupDefaults is the per-group default ratchet ladder for the
// regime variant (#870 C2). Trend groups (clean/choppy) are pure let-it-ride
// (close_fraction 0); the ranging family scales out as ranges mean-revert.
// #1059 split the single ranging ladder into three composite substate ladders
// keyed by ratchetCloseDefaultGroup (NOT the shared regimeCloseDefaultGroup,
// which still collapses ranging* → "ranging" for the B2 ATR-TP path):
//   - ranging_quiet keeps the pre-#1059 ranging geometry and is also the target
//     for bare ADX "ranging" (no substate signal), so ADX behavior is unchanged.
//   - ranging_volatile widens the triggers (0.75→1.0, 1.5→2.0, 2.0→3.0) to stop
//     scaling out on wide-range noise; close fractions are unchanged.
//   - ranging_directional scales out lighter early (25/50/75 vs 40/80/100) and
//     adds a 4th let-ride rung that only tightens the trail (no extra close) so
//     the runner survives a nascent breakout instead of being fully scaled out.
//
// Each group's first-rung trail couples to that group's opening trail in
// regimeATRDefaults.Trailing (#1120: clean 2.5 / choppy 2.25 / ranging_quiet 1.0
// / ranging_volatile 1.25 / ranging_directional* 1.5), so
// every first rung is <= 1.0 for the ranging substates. The split values are
// starting priors — validate via the #1058 7-state backtester (item 4) before
// relying on the exact geometry. Mirrors DEFAULT_RATCHET_TIERS_BY_GROUP in
// shared_strategies/close/trailing_tp_ratchet.py.
var ratchetTierGroupDefaults = map[string][]trailingRatchetTier{
	"clean": {
		{ATRMultiple: 3.0, CloseFraction: 0, TrailingMultAfter: 1.5},
		{ATRMultiple: 4.5, CloseFraction: 0, TrailingMultAfter: 1.0},
		{ATRMultiple: 6.0, CloseFraction: 0, TrailingMultAfter: 0.8},
	},
	"choppy": {
		{ATRMultiple: 2.0, CloseFraction: 0, TrailingMultAfter: 1.5},
		{ATRMultiple: 2.5, CloseFraction: 0, TrailingMultAfter: 1.0},
		{ATRMultiple: 3.0, CloseFraction: 0, TrailingMultAfter: 0.8},
	},
	"ranging_quiet": {
		{ATRMultiple: 0.75, CloseFraction: 0.4, TrailingMultAfter: 1.0},
		{ATRMultiple: 1.5, CloseFraction: 0.8, TrailingMultAfter: 0.75},
		{ATRMultiple: 2.0, CloseFraction: 1.0, TrailingMultAfter: 0.75},
	},
	"ranging_volatile": {
		{ATRMultiple: 1.0, CloseFraction: 0.4, TrailingMultAfter: 1.0},
		{ATRMultiple: 2.0, CloseFraction: 0.8, TrailingMultAfter: 0.75},
		{ATRMultiple: 3.0, CloseFraction: 1.0, TrailingMultAfter: 0.75},
	},
	"ranging_directional": {
		{ATRMultiple: 1.0, CloseFraction: 0.25, TrailingMultAfter: 1.0},
		{ATRMultiple: 2.0, CloseFraction: 0.50, TrailingMultAfter: 1.0},
		{ATRMultiple: 3.0, CloseFraction: 0.75, TrailingMultAfter: 0.8},
		{ATRMultiple: 4.5, CloseFraction: 0.75, TrailingMultAfter: 0.6},
	},
}

// ratchetCloseDefaultGroup resolves a classifier label to a ratchet default-
// ladder group key (#1059). Unlike the shared regimeCloseDefaultGroup — which
// collapses every ranging* → "ranging" for the B2 ATR-TP path — the ratchet
// ladder differentiates the three composite ranging substates, so each gets its
// own scale-out geometry. Bare ADX "ranging" (no substate signal) maps to the
// quiet ladder, preserving pre-#1059 behavior. clean/choppy and ADX-trend
// labels delegate unchanged to regimeCloseDefaultGroup. Routing the substates
// back through the shared fn would make the B2 regimeTPTierGroupDefaults lookup
// miss and silently emit no TP tiers (never-arm of an auto-protective exit) —
// keep this resolver ratchet-only.
func ratchetCloseDefaultGroup(label string) (string, bool) {
	l := strings.TrimSpace(label)
	switch l {
	case "ranging_quiet", "ranging_volatile", "ranging_directional":
		return l, true
	case "ranging_directional_up", "ranging_directional_down":
		// #1124: the directional-drift substates share the ranging_directional
		// scale-out ladder (the geometry is direction-agnostic — the SL side
		// carries direction, the TP scale-out does not). Map them to that group
		// explicitly; otherwise they fall through to regimeCloseDefaultGroup's
		// "ranging" key, which has NO ratchet ladder in ratchetTierGroupDefaults
		// → defaultTrailingRatchetTiersForRegime returns nil → silent never-arm
		// of the auto-protective ratchet exit (money path).
		return "ranging_directional", true
	case "ranging":
		return "ranging_quiet", true
	}
	return regimeCloseDefaultGroup(l)
}

// defaultTrailingRatchetTiersForRegime resolves the per-group default ratchet
// ladder for a stamped regime label (#870 C2 use_defaults / omitted tp_tiers on
// trailing_tp_ratchet_regime). Returns nil for an empty/unknown label so the
// caller emits only the SL until the position regime is stamped.
func defaultTrailingRatchetTiersForRegime(regime string) []trailingRatchetTier {
	group, ok := ratchetCloseDefaultGroup(regime)
	if !ok {
		return nil
	}
	src := ratchetTierGroupDefaults[group]
	if len(src) == 0 {
		return nil
	}
	out := make([]trailingRatchetTier, len(src))
	copy(out, src)
	return out
}

func resolveTrailingMultAfter(tier map[string]interface{}, firingMultiple float64) (float64, error) {
	_, hasAbs := tier["trailing_mult_after"]
	_, hasFrac := tier["tp_atr_fraction"]
	if hasAbs && hasFrac {
		return 0, fmt.Errorf("cannot combine trailing_mult_after with tp_atr_fraction")
	}
	if hasAbs {
		mult, err := floatFromAnyChecked(tier["trailing_mult_after"])
		if err != nil || mult <= 0 {
			return 0, fmt.Errorf("trailing_mult_after must be > 0")
		}
		return mult, nil
	}
	if hasFrac {
		frac, err := floatFromAnyChecked(tier["tp_atr_fraction"])
		if err != nil || frac <= 0 {
			return 0, fmt.Errorf("tp_atr_fraction must be > 0")
		}
		if firingMultiple <= 0 {
			return 0, fmt.Errorf("firing tier atr_multiple must be > 0 for tp_atr_fraction")
		}
		return frac * firingMultiple, nil
	}
	return 0, fmt.Errorf("requires exactly one of trailing_mult_after or tp_atr_fraction")
}

func parseTrailingRatchetTier(m map[string]interface{}, ctxLabel string, idx int) (trailingRatchetTier, []string) {
	var errs []string
	mult, err := floatFromAnyChecked(firstPresent(m, "atr_multiple", "multiple", "atr"))
	if err != nil || mult <= 0 {
		errs = append(errs, fmt.Sprintf("%s[%d].atr_multiple: must be > 0", ctxLabel, idx))
		return trailingRatchetTier{}, errs
	}
	frac := 0.0
	if raw := firstPresent(m, "close_fraction", "fraction"); raw != nil {
		frac, err = floatFromAnyChecked(raw)
		if err != nil || frac < 0 || frac > 1 {
			errs = append(errs, fmt.Sprintf("%s[%d].close_fraction: must be in [0, 1]", ctxLabel, idx))
			return trailingRatchetTier{}, errs
		}
	}
	trail, terr := resolveTrailingMultAfter(m, mult)
	if terr != nil {
		errs = append(errs, fmt.Sprintf("%s[%d]: %v", ctxLabel, idx, terr))
		return trailingRatchetTier{}, errs
	}
	allowed := map[string]bool{
		"atr_multiple": true, "multiple": true, "atr": true,
		"close_fraction": true, "fraction": true,
		"trailing_mult_after": true, "tp_atr_fraction": true,
	}
	for k := range m {
		if !allowed[k] {
			errs = append(errs, fmt.Sprintf("%s[%d]: unknown key %q", ctxLabel, idx, k))
		}
	}
	return trailingRatchetTier{
		ATRMultiple:       mult,
		CloseFraction:     frac,
		TrailingMultAfter: trail,
	}, errs
}

func parseTrailingRatchetTierList(raw interface{}, ctxLabel string) ([]trailingRatchetTier, []string) {
	items, ok := raw.([]interface{})
	if !ok {
		return nil, []string{fmt.Sprintf("%s: must be a list, got %T", ctxLabel, raw)}
	}
	var errs []string
	out := make([]trailingRatchetTier, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s[%d]: must be an object", ctxLabel, idx))
			continue
		}
		tier, sub := parseTrailingRatchetTier(m, ctxLabel, idx)
		errs = append(errs, sub...)
		if len(sub) == 0 {
			out = append(out, tier)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ATRMultiple < out[j].ATRMultiple })
	if len(out) == 0 && len(errs) == 0 {
		errs = append(errs, fmt.Sprintf("%s: must contain at least one valid tier", ctxLabel))
	}
	return out, errs
}

func trailingRatchetTiersForRegime(sc StrategyConfig, regime string) []trailingRatchetTier {
	if !strategyUsesTrailingTPRatchetClose(sc) {
		return nil
	}
	for _, ref := range sc.closeRefs() {
		name := strings.ToLower(strings.TrimSpace(ref.Name))
		if !isTrailingTPRatchetCloseName(name) {
			continue
		}
		raw, ok := closeTierListParam(ref.Params)
		if !ok {
			// Omitted tp_tiers (or use_defaults:true) resolves to the system
			// default ladder. #870: the regime variant resolves the per-group
			// ladder for the stamped regime; the scalar variant broadcasts the
			// single #866 default.
			if name == trailingTPRatchetRegimeCloseName {
				return defaultTrailingRatchetTiersForRegime(regime)
			}
			return defaultTrailingRatchetTiers()
		}
		if name == trailingTPRatchetRegimeCloseName {
			table, ok := raw.(map[string]interface{})
			if !ok || strings.TrimSpace(regime) == "" {
				return nil
			}
			key := strings.TrimSpace(regime)
			block, ok := table[key]
			if !ok {
				// #1124: sub-label stamp falls back to the bare
				// ranging_directional tier ladder (exact key wins first, so an
				// explicit sub key still overrides bare).
				if regimeDirectionalSubs[key] {
					block, ok = table[regimeDirectionalBare]
				}
				if !ok {
					return nil
				}
			}
			tiers, _ := parseTrailingRatchetTierList(block, ref.Name+".tp_tiers."+regime)
			return tiers
		}
		if table, ok := raw.(map[string]interface{}); ok {
			block := table["default"]
			if block == nil {
				block = table["ranging"]
			}
			tiers, _ := parseTrailingRatchetTierList(block, ref.Name+".tp_tiers")
			return tiers
		}
		tiers, _ := parseTrailingRatchetTierList(raw, ref.Name+".tp_tiers")
		return tiers
	}
	return nil
}

func validateTrailingTPRatchetClose(sc StrategyConfig, labels []string, regimeEnabled bool) []string {
	if !strategyUsesTrailingTPRatchetClose(sc) {
		return nil
	}
	prefix := fmt.Sprintf("strategy[%s]", sc.ID)
	var errs []string
	if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* is HL perps/manual only", prefix))
	}
	// #870: the SL owner + initial trail differs by variant. The scalar
	// trailing_tp_ratchet owns it via trailing_stop_atr_mult; the regime
	// trailing_tp_ratchet_regime owns it via the per-regime trailing_stop_atr_regime
	// block, so per-trade initial risk scales with the stamped regime (tight in
	// ranges, wide in clean trends).
	regimeVariant := false
	for _, ref := range sc.closeRefs() {
		if strings.ToLower(strings.TrimSpace(ref.Name)) == trailingTPRatchetRegimeCloseName {
			regimeVariant = true
			break
		}
	}
	hasRegimeBlock := sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero()
	if regimeVariant {
		if !hasRegimeBlock {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet_regime requires trailing_stop_atr_regime (the per-regime opening trail / SL owner)", prefix))
		}
		if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet_regime cannot combine with scalar trailing_stop_atr_mult (the trailing_stop_atr_regime block owns the trail)", prefix))
		}
	} else {
		if sc.TrailingStopATRMult == nil || *sc.TrailingStopATRMult <= 0 {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet requires trailing_stop_atr_mult > 0 (initial trail distance)", prefix))
		}
		if hasRegimeBlock {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet cannot combine with trailing_stop_atr_regime (use trailing_tp_ratchet_regime)", prefix))
		}
	}
	if sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with trailing_stop_pct", prefix))
	}
	if sc.StopLossPct != nil && *sc.StopLossPct > 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with stop_loss_pct", prefix))
	}
	if sc.StopLossMarginPct != nil && *sc.StopLossMarginPct > 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with stop_loss_margin_pct", prefix))
	}
	if sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0 {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with stop_loss_atr_mult", prefix))
	}
	if sc.StopLossATRRegime.IsConfigured() {
		errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet* cannot combine with stop_loss_atr_regime", prefix))
	}
	// scalarInitialTrail couples the scalar variant's first rung to
	// trailing_stop_atr_mult; the regime variant resolves the open per regime
	// key from the trailing_stop_atr_regime block (populated upstream by
	// ResolveSurfaceWithLabels).
	scalarInitialTrail := 0.0
	if !regimeVariant && sc.TrailingStopATRMult != nil {
		scalarInitialTrail = *sc.TrailingStopATRMult
	}
	regimeKeyOpen := func(key string) float64 {
		if sc.TrailingStopATRRegime == nil {
			return 0
		}
		if v, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, key); ok {
			return v
		}
		return 0
	}
	for _, ref := range sc.closeRefs() {
		if !isTrailingTPRatchetCloseName(ref.Name) {
			continue
		}
		sub := fmt.Sprintf("%s.close_strategy(%s)", prefix, ref.Name)
		name := strings.ToLower(strings.TrimSpace(ref.Name))
		isRegime := name == trailingTPRatchetRegimeCloseName
		// Unknown-key guard runs in every branch, including the
		// omitted-tp_tiers / use_defaults fallback below.
		for k := range ref.Params {
			switch k {
			case "tp_tiers", "use_defaults":
			case "tiers":
				errs = append(errs, fmt.Sprintf("%s: legacy param %q is not supported — use tp_tiers (#841)", sub, k))
			default:
				errs = append(errs, fmt.Sprintf("%s: unknown param %q (allowed: tp_tiers, use_defaults)", sub, k))
			}
		}
		if isRegime && !regimeEnabled {
			errs = append(errs, fmt.Sprintf("%s: trailing_tp_ratchet_regime requires top-level regime.enabled=true", sub))
		}
		raw, hasTiers := closeTierListParam(ref.Params)
		if !hasTiers {
			// Omitted tp_tiers (or use_defaults:true) resolves to the system
			// default ladder. The default is internally valid (monotonic,
			// ascending); the only load-time check that still applies is the
			// initial-trail coupling against the opening trail. #870: the regime
			// variant resolves a per-group ladder per regime key and couples each
			// against that key's opening trail; the scalar variant uses the
			// single #866 default against trailing_stop_atr_mult.
			if isRegime {
				for _, key := range labels {
					def := defaultTrailingRatchetTiersForRegime(key)
					errs = append(errs, validateTrailingRatchetInitialTrail(def, regimeKeyOpen(key), sub+".tp_tiers(default)."+key)...)
				}
			} else {
				def := defaultTrailingRatchetTiers()
				errs = append(errs, validateTrailingRatchetInitialTrail(def, scalarInitialTrail, sub+".tp_tiers(default)")...)
			}
			continue
		}
		if isRegime {
			table, ok := raw.(map[string]interface{})
			if !ok {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers: must be a regime-keyed object", sub))
				continue
			}
			labelSet := make(map[string]bool, len(labels))
			for _, l := range labels {
				labelSet[l] = true
			}
			for key := range table {
				if !labelSet[key] {
					errs = append(errs, fmt.Sprintf("%s.tp_tiers: unknown regime key %q (valid: %s)", sub, key, strings.Join(labels, ", ")))
				}
			}
			bareDirectional := table[regimeDirectionalBare] != nil
			for _, key := range labels {
				block, ok := table[key]
				if !ok {
					// #1124 family rule: bare ranging_directional covers the
					// _up/_down sub-labels for exhaustiveness (the explicit
					// tp_tiers resolver falls back bare→sub at runtime).
					if regimeLabelFamilyCovered(key, bareDirectional) {
						continue
					}
					errs = append(errs, fmt.Sprintf("%s.tp_tiers: missing required regime key %q", sub, key))
					continue
				}
				tiers, subErrs := parseTrailingRatchetTierList(block, sub+".tp_tiers."+key)
				errs = append(errs, subErrs...)
				errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub+".tp_tiers."+key)...)
				errs = append(errs, validateTrailingRatchetInitialTrail(tiers, regimeKeyOpen(key), sub+".tp_tiers."+key)...)
			}
			continue
		}
		if table, ok := raw.(map[string]interface{}); ok {
			block := table["default"]
			if block == nil {
				block = table["ranging"]
			}
			if block == nil {
				errs = append(errs, fmt.Sprintf("%s.tp_tiers: object form requires a \"default\" or \"ranging\" key", sub))
				continue
			}
			tiers, subErrs := parseTrailingRatchetTierList(block, sub+".tp_tiers")
			errs = append(errs, subErrs...)
			errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub+".tp_tiers")...)
			errs = append(errs, validateTrailingRatchetInitialTrail(tiers, scalarInitialTrail, sub+".tp_tiers")...)
			continue
		}
		tiers, subErrs := parseTrailingRatchetTierList(raw, sub+".tp_tiers")
		errs = append(errs, subErrs...)
		errs = append(errs, validateTrailingRatchetTierMonotonicity(tiers, sub+".tp_tiers")...)
		errs = append(errs, validateTrailingRatchetInitialTrail(tiers, scalarInitialTrail, sub+".tp_tiers")...)
	}
	return errs
}

// validateTrailingRatchetInitialTrail rejects a first ratchet rung whose trail
// distance is looser than (greater than) the strategy-level
// trailing_stop_atr_mult. The first rung can only tighten the initial trail —
// a looser first rung would silently no-op at runtime (applyTrailingTPRatchet
// never loosens), so catch the misconfiguration at load. Tiers are sorted
// ascending by atr_multiple, so tiers[0] is the first rung and monotonicity
// guarantees the rest are <= it.
func validateTrailingRatchetInitialTrail(tiers []trailingRatchetTier, initialTrail float64, ctxLabel string) []string {
	if len(tiers) == 0 || initialTrail <= 0 {
		return nil
	}
	if tiers[0].TrailingMultAfter > initialTrail+1e-12 {
		return []string{fmt.Sprintf(
			"%s[0].trailing distance %.4g×ATR must be <= initial trailing_stop_atr_mult (%.4g×ATR) — the first ratchet rung can only tighten",
			ctxLabel, tiers[0].TrailingMultAfter, initialTrail,
		)}
	}
	return nil
}

func validateTrailingRatchetTierMonotonicity(tiers []trailingRatchetTier, ctxLabel string) []string {
	if len(tiers) < 2 {
		return nil
	}
	var errs []string
	prevTrail := tiers[0].TrailingMultAfter
	prevFrac := tiers[0].CloseFraction
	for i := 1; i < len(tiers); i++ {
		curTrail := tiers[i].TrailingMultAfter
		if curTrail > prevTrail+1e-12 {
			errs = append(errs, fmt.Sprintf(
				"%s[%d].trailing distance %.4g×ATR must be <= tier[%d] (%.4g×ATR) — ratchet tiers tighten monotonically",
				ctxLabel, i, curTrail, i-1, prevTrail,
			))
		}
		curFrac := tiers[i].CloseFraction
		if curFrac+1e-12 < prevFrac {
			errs = append(errs, fmt.Sprintf(
				"%s[%d].close_fraction %.4g must be >= tier[%d] close_fraction %.4g — close fractions are cumulative",
				ctxLabel, i, curFrac, i-1, prevFrac,
			))
		}
		prevTrail = curTrail
		prevFrac = curFrac
	}
	return errs
}

func effectiveTrailingRatchetMult(pos *Position, sc StrategyConfig) float64 {
	if pos != nil && pos.PostTPTrailingATRMult != nil && *pos.PostTPTrailingATRMult > 0 {
		return *pos.PostTPTrailingATRMult
	}
	if sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0 {
		return *sc.TrailingStopATRMult
	}
	// #870: the regime ratchet's initial loose trail is the per-regime opening
	// trail from trailing_stop_atr_regime, not a scalar mult — resolve it so the
	// first rung is correctly seen as a tightening (not a no-op against 0).
	if sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero() && pos != nil {
		if v, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, protectionATRRegimeLabel(pos, sc)); ok {
			return v
		}
	}
	return 0
}

func findHighestMarkClearedRatchetTier(tiers []trailingRatchetTier, atrProfit float64, fromIdx int) (int, bool) {
	if fromIdx < 0 {
		fromIdx = 0
	}
	highest := -1
	for i := fromIdx; i < len(tiers); i++ {
		if atrProfit+1e-12 >= tiers[i].ATRMultiple {
			highest = i
		}
	}
	return highest, highest >= 0
}

// applyTrailingTPRatchet stamps a tighter PostTPTrailingATRMult when mark-based
// tier thresholds are newly cleared. Reuses SLAdjustedTiersProcessed as the
// idempotency watermark (ratchet closes do not use on-chain TP OIDs or sl_after).
//
// Caller-visible behavior is intentionally mark-based instead of depending on
// the Python evaluator's close_fraction result: close_fraction=0 tiers still
// need to ratchet, and scale-out tiers should ratchet after the state update
// that preserves the residual position.
func applyTrailingTPRatchet(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	mu *sync.RWMutex,
	logger *StrategyLogger,
) *RatchetTriggerAlert {
	if !strategyUsesTrailingTPRatchetClose(sc) || stratState == nil || symbol == "" || mark <= 0 {
		return nil
	}
	mu.Lock()
	var alert *RatchetTriggerAlert
	pos, ok := stratState.Positions[symbol]
	if ok {
		_, alert = applyTrailingTPRatchetToPosition(sc, pos, symbol, mark, logger)
	}
	mu.Unlock()
	return alert
}

// applyTrailingTPRatchetToPosition applies the same ratchet logic while the
// caller already owns the state lock. Returns (true, *RatchetTriggerAlert) ONLY
// when a tier newly advances the watermark AND tightens the trail — the alert
// snapshot carries the immutable details the owner DM needs, so the caller can
// deliver it (notifyRatchetTrigger) after releasing the lock (#1110). Every
// non-tightening path returns (false, nil), including a watermark-only advance
// (tier cleared but the resulting trail is not tighter), so an already-processed
// or no-tighten tier never alerts.
func applyTrailingTPRatchetToPosition(sc StrategyConfig, pos *Position, symbol string, mark float64, logger *StrategyLogger) (bool, *RatchetTriggerAlert) {
	if !strategyUsesTrailingTPRatchetClose(sc) || pos == nil || symbol == "" || mark <= 0 {
		return false, nil
	}
	if pos.Quantity <= 0 || pos.AvgCost <= 0 || pos.EntryATR <= 0 {
		return false, nil
	}
	side := strings.ToLower(strings.TrimSpace(pos.Side))
	if side != "long" && side != "short" {
		return false, nil
	}
	regime := protectionATRRegimeLabel(pos, sc)
	tiers := trailingRatchetTiersForRegime(sc, regime)
	if len(tiers) == 0 {
		return false, nil
	}
	// #873: ratchet tier-clearing measures ATR profit distance from the FROZEN
	// entry (riskAnchorPrice), not the blended AvgCost, so a scale-in keeps the
	// TP tier offsets pinned to the first entry.
	anchor := pos.riskAnchorPrice()
	profitDistance := mark - anchor
	if side == "short" {
		profitDistance = anchor - mark
	}
	atrProfit := profitDistance / pos.EntryATR
	clearedIdx, clearedOK := findHighestMarkClearedRatchetTier(tiers, atrProfit, pos.SLAdjustedTiersProcessed)
	if !clearedOK {
		return false, nil
	}
	newMult := tiers[clearedIdx].TrailingMultAfter
	current := effectiveTrailingRatchetMult(pos, sc)
	if newMult >= current-1e-12 {
		if pos.SLAdjustedTiersProcessed <= clearedIdx {
			pos.SLAdjustedTiersProcessed = clearedIdx + 1
		}
		return false, nil
	}
	mult := newMult
	pos.PostTPTrailingATRMult = &mult
	pos.SLAdjustedTiersProcessed = clearedIdx + 1
	if logger != nil {
		logger.Info("trailing_tp_ratchet: %s tier %d cleared — trail tightened to %.4g×ATR (from %.4g×ATR)",
			symbol, clearedIdx, newMult, current)
	}
	alert := buildRatchetTriggerAlert(sc, pos, symbol, side, regime, mark, anchor, atrProfit, tiers, clearedIdx, current, newMult)
	return true, alert
}

// buildRatchetTriggerAlert assembles the immutable #1110 alert snapshot at the
// instant a ratchet tier tightens the trail. All inputs are read while the
// caller holds the state lock; the result carries no pointers into pos so it is
// safe to hand to a post-unlock notifier. anchor / atrProfit are passed through
// from the caller so the snapshot matches the exact values the tightening
// decision used.
func buildRatchetTriggerAlert(sc StrategyConfig, pos *Position, symbol, side, regime string, mark, anchor, atrProfit float64, tiers []trailingRatchetTier, clearedIdx int, oldMult, newMult float64) *RatchetTriggerAlert {
	entryATR := pos.EntryATR
	contractMult := 1.0
	if pos.Multiplier > 0 {
		contractMult = pos.Multiplier
	}
	profitDistance := mark - anchor
	if side == "short" {
		profitDistance = anchor - mark
	}
	// Effective HWM for the intended-SL display: the best mark seen while open,
	// floored to the current mark so a stale/unset StopLossHighWaterPx (e.g. the
	// walker hasn't run yet this open) still yields a sensible computed trigger.
	hwm := pos.StopLossHighWaterPx
	if side == "long" {
		if hwm <= 0 || mark > hwm {
			hwm = mark
		}
	} else {
		if hwm <= 0 || mark < hwm {
			hwm = mark
		}
	}
	intendedSL := 0.0
	if entryATR > 0 && hwm > 0 && newMult > 0 {
		if side == "long" {
			intendedSL = hwm - newMult*entryATR
		} else {
			intendedSL = hwm + newMult*entryATR
		}
		if intendedSL <= 0 {
			intendedSL = 0
		}
	}
	a := &RatchetTriggerAlert{
		StrategyID:           sc.ID,
		Symbol:               symbol,
		Side:                 side,
		TierIdx:              clearedIdx,
		TotalTiers:           len(tiers),
		TierATRMultiple:      tiers[clearedIdx].ATRMultiple,
		TierTriggerPx:        atrTierTriggerPx(side, anchor, entryATR, tiers[clearedIdx].ATRMultiple),
		MarkPrice:            mark,
		AnchorPrice:          anchor,
		EntryATR:             entryATR,
		ProfitATR:            atrProfit,
		ProfitUSD:            profitDistance * pos.Quantity * contractMult,
		OldTrailMult:         oldMult,
		NewTrailMult:         newMult,
		HighWaterMark:        hwm,
		IntendedSLTriggerPx:  intendedSL,
		RegimeLabel:          regime,
		PositionRegimeAtOpen: pos.Regime,
	}
	if clearedIdx+1 < len(tiers) {
		nt := tiers[clearedIdx+1]
		a.HasNextTier = true
		a.NextTierATRMultiple = nt.ATRMultiple
		a.NextTierTrailAfter = nt.TrailingMultAfter
		a.NextTierTriggerPx = atrTierTriggerPx(side, anchor, entryATR, nt.ATRMultiple)
	}
	return a
}

// manualCloseEvaluatorDriftWarned dedupes the #1115 close-evaluator drift alert
// to once per (strategy, symbol) per process — keyed "id|symbol". Never reset:
// the operator only needs to see it once after the upgrade+restart that flipped
// the default; pinning close_strategy and restarting re-derives the tiered close
// (no drift, no warning).
var manualCloseEvaluatorDriftWarned sync.Map

// manualCloseEvaluatorDriftedFromTPs reports whether an open manual position was
// opened under a tiered-TP close evaluator (it carries resting on-chain TP OIDs)
// while the strategy's CURRENT close evaluator is the trailing ratchet, which
// places no on-chain TPs (#1115). This is the cross-evaluator drift that occurs
// when the manual close DEFAULT flips from tiered_tp_atr_live to
// trailing_tp_ratchet_regime across a binary upgrade + restart for a position
// opened pre-upgrade: SL ownership moves to the regime trail and the
// previously-placed TP1/TP2 orders are no longer managed by the close evaluator.
// They still rest on-chain (reduce-only) and are cancelled on a full / manual
// close (extraCancelOIDs) — and auto-cancel when the SL flattens the position —
// but can fire mid-life under what is now a let-it-ride config, so the operator
// must be alerted. A ratchet-opened position never carries TP OIDs (the ratchet
// path skips inline TP placement), so this reliably keys off the tiered-open
// fingerprint. Pure so the detection is unit-tested.
func manualCloseEvaluatorDriftedFromTPs(sc StrategyConfig, pos *Position) bool {
	return pos != nil && len(pos.TPOIDs) > 0 && strategyUsesTrailingTPRatchetClose(sc)
}

func trailingRatchetRulesEqualForReload(a, b StrategyConfig) bool {
	return trailingRatchetFingerprint(a) == trailingRatchetFingerprint(b)
}

func trailingRatchetFingerprint(sc StrategyConfig) string {
	for _, ref := range sc.closeRefs() {
		if !isTrailingTPRatchetCloseName(ref.Name) {
			continue
		}
		b, err := json.Marshal(ref.Params)
		if err != nil {
			return fmt.Sprintf("%s:%v", ref.Name, ref.Params)
		}
		return ref.Name + ":" + string(b)
	}
	return ""
}
