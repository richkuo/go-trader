package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

var deprecatedConfigKeyWarned sync.Map

func warnDeprecatedConfigKey(old, canonical string) {
	if _, loaded := deprecatedConfigKeyWarned.LoadOrStore(old+"->"+canonical, true); loaded {
		return
	}
	fmt.Printf("[DEPRECATED] config key %q is deprecated; use %q\n", old, canonical)
}

func closeTierListParam(params map[string]interface{}) (interface{}, bool) {
	if params == nil {
		return nil, false
	}
	if v, ok := params["tp_tiers"]; ok {
		return v, true
	}
	return nil, false
}

type SLAfterRule struct {
	Kind string

	ATRMult float64

	TrailATRMult float64

	ATRRegime *RegimeATRBlock

	TrailATRRegime *RegimeATRBlock

	TPATRFraction float64

	TPATRFractionRegime *RegimeFloatBlock
}

type RegimeFloatBlock struct {
	TrendRegime map[string]float64
}

func (b *RegimeFloatBlock) Resolve(regime string) (float64, bool) {
	if b == nil || len(b.TrendRegime) == 0 {
		return 0, false
	}
	r := strings.TrimSpace(regime)
	if v, ok := b.TrendRegime[r]; ok {
		return v, true
	}

	if regimeDirectionalSubs[r] {
		if v, ok := b.TrendRegime[regimeDirectionalBare]; ok {
			return v, true
		}
	}
	return 0, false
}

func (b *RegimeFloatBlock) EqualForReload(other *RegimeFloatBlock) bool {
	aZero := b == nil || len(b.TrendRegime) == 0
	bZero := other == nil || len(other.TrendRegime) == 0
	if aZero != bZero {
		return false
	}
	if aZero {
		return true
	}
	if len(b.TrendRegime) != len(other.TrendRegime) {
		return false
	}
	for k, va := range b.TrendRegime {
		if vb, ok := other.TrendRegime[k]; !ok || vb != va {
			return false
		}
	}
	return true
}

func (r SLAfterRule) IsEmpty() bool { return r.Kind == "" }

func (r SLAfterRule) HasRegime() bool {
	return r.ATRRegime != nil || r.TrailATRRegime != nil || r.TPATRFractionRegime != nil
}

func (r SLAfterRule) resolveForRegime(regime string) (SLAfterRule, bool) {
	return r.resolveForRegimeAndTier(regime, 0)
}

func (r SLAfterRule) resolveForRegimeAndTier(regime string, tierMultiple float64) (SLAfterRule, bool) {
	switch r.Kind {
	case "atr_offset":
		if r.ATRRegime == nil {
			return r, true
		}
		entry, ok := r.ATRRegime.Resolve(regime)
		if !ok {
			return SLAfterRule{}, false
		}
		return SLAfterRule{Kind: "atr_offset", ATRMult: entry.ATR}, true
	case "trail_from_here":
		if r.TPATRFractionRegime != nil {
			frac, ok := r.TPATRFractionRegime.Resolve(regime)
			if !ok || frac <= 0 || tierMultiple <= 0 {
				return SLAfterRule{}, false
			}
			return SLAfterRule{Kind: "trail_from_here", TrailATRMult: frac * tierMultiple}, true
		}
		if r.TPATRFraction > 0 {
			if tierMultiple <= 0 {
				return SLAfterRule{}, false
			}
			return SLAfterRule{Kind: "trail_from_here", TrailATRMult: r.TPATRFraction * tierMultiple}, true
		}
		if r.TrailATRRegime == nil {
			return r, true
		}
		entry, ok := r.TrailATRRegime.Resolve(regime)
		if !ok || entry.ATR <= 0 {
			return SLAfterRule{}, false
		}
		return SLAfterRule{Kind: "trail_from_here", TrailATRMult: entry.ATR}, true
	default:
		return r, true
	}
}

func (r SLAfterRule) Equal(other SLAfterRule) bool {
	if r.Kind != other.Kind || r.ATRMult != other.ATRMult ||
		r.TrailATRMult != other.TrailATRMult || r.TPATRFraction != other.TPATRFraction {
		return false
	}
	if !r.ATRRegime.EqualForReload(other.ATRRegime) {
		return false
	}
	if !r.TrailATRRegime.EqualForReload(other.TrailATRRegime) {
		return false
	}
	if !r.TPATRFractionRegime.EqualForReload(other.TPATRFractionRegime) {
		return false
	}
	return true
}

func computePostTPStopLossTrigger(
	rule SLAfterRule, side string, avgCost, entryATR, currentMark float64,
) (triggerPx float64, mode string, ok bool) {
	sideLower := strings.ToLower(strings.TrimSpace(side))
	if sideLower != "long" && sideLower != "short" {
		return 0, "", false
	}
	if avgCost <= 0 {
		return 0, "", false
	}
	switch rule.Kind {
	case "":
		return 0, "", false
	case "breakeven":
		return avgCost, "breakeven", true
	case "atr_offset":
		if entryATR <= 0 {
			return 0, "", false
		}
		var px float64
		if sideLower == "long" {
			px = avgCost + rule.ATRMult*entryATR
		} else {
			px = avgCost - rule.ATRMult*entryATR
		}
		if px <= 0 {
			return 0, "", false
		}
		return px, formatATROffsetMode(rule.ATRMult), true
	case "trail_from_here":
		if entryATR <= 0 || currentMark <= 0 || rule.TrailATRMult <= 0 {
			return 0, "", false
		}
		var px float64
		if sideLower == "long" {
			px = currentMark - rule.TrailATRMult*entryATR
		} else {
			px = currentMark + rule.TrailATRMult*entryATR
		}
		if px <= 0 {
			return 0, "", false
		}
		return px, fmt.Sprintf("trail %g×ATR", rule.TrailATRMult), true
	}
	return 0, "", false
}

func formatATROffsetMode(m float64) string {
	sign := "+"
	if m < 0 {
		sign = "-"
		m = -m
	}
	return fmt.Sprintf("atr%s%g", sign, m)
}

func validateSLAfterRule(rule SLAfterRule) error {
	switch rule.Kind {
	case "":
		return nil
	case "breakeven":
		if rule.ATRRegime != nil || rule.TrailATRRegime != nil || rule.TPATRFractionRegime != nil || rule.TPATRFraction != 0 {
			return errors.New("sl_after breakeven does not accept trend_regime or tp_atr_fraction")
		}
		return nil
	case "atr_offset":
		if rule.TrailATRRegime != nil || rule.TPATRFractionRegime != nil || rule.TPATRFraction != 0 {
			return errors.New("sl_after atr_offset accepts trend_regime under atr, not trail_from_here trail fields")
		}

		return nil
	case "trail_from_here":
		if rule.ATRRegime != nil {
			return errors.New("sl_after trail_from_here accepts trend_regime under trail_from_here.atr, not at the top level")
		}
		forms := 0
		if rule.TrailATRMult > 0 {
			forms++
		}
		if rule.TrailATRRegime != nil {
			forms++
		}
		if rule.TPATRFraction > 0 {
			forms++
		}
		if rule.TPATRFractionRegime != nil {
			forms++
		}
		if forms != 1 {
			return errors.New("sl_after trail_from_here requires exactly one of atr_mult, trend_regime, or tp_atr_fraction")
		}
		return nil
	default:
		return fmt.Errorf("sl_after kind %q is not recognized (expected breakeven|atr_offset|trail_from_here)", rule.Kind)
	}
}

func parseSLAfterRule(raw interface{}) (SLAfterRule, error) {
	return parseSLAfterRuleWithLabels(raw, canonicalTrendRegimeLabels)
}

func parseSLAfterRuleRuntime(raw interface{}) (SLAfterRule, error) {
	return parseSLAfterRuleWithLabels(raw, nil)
}

func parseSLAfterRuleWithLabels(raw interface{}, labels []string) (SLAfterRule, error) {
	if raw == nil {
		return SLAfterRule{}, nil
	}
	switch v := raw.(type) {
	case string:
		kind := strings.ToLower(strings.TrimSpace(v))
		switch kind {
		case "":
			return SLAfterRule{}, nil
		case "breakeven":
			return SLAfterRule{Kind: "breakeven"}, nil
		default:
			return SLAfterRule{}, fmt.Errorf("sl_after string %q is not recognized (expected \"breakeven\")", v)
		}
	case map[string]interface{}:

		if kindRaw, ok := v["kind"]; ok {
			kindStr, isStr := kindRaw.(string)
			if !isStr {
				return SLAfterRule{}, fmt.Errorf("sl_after.kind must be a string, got %T", kindRaw)
			}
			kind := strings.ToLower(strings.TrimSpace(kindStr))
			switch kind {
			case "breakeven":
				return SLAfterRule{Kind: "breakeven"}, nil
			case "atr_offset":
				return parseSLAfterATROffset(v, "sl_after kind=atr_offset", labels)
			case "trail_from_here":
				return parseSLAfterTrailFromHere(v, "sl_after kind=trail_from_here", labels)
			default:
				return SLAfterRule{}, fmt.Errorf("sl_after kind %q is not recognized", kind)
			}
		}

		if trailRaw, ok := v["trail_from_here"]; ok {
			trailMap, isMap := trailRaw.(map[string]interface{})
			if !isMap {
				return SLAfterRule{}, fmt.Errorf("sl_after.trail_from_here must be an object, got %T", trailRaw)
			}
			return parseSLAfterTrailFromHere(trailMap, "sl_after.trail_from_here", labels)
		}

		if _, ok := v[regimeClassifierKey]; ok {
			return parseSLAfterATROffset(v, "sl_after", labels)
		}

		if _, ok := firstNonNil(v, "atr_mult", "atr_offset"); ok {
			return parseSLAfterATROffset(v, "sl_after atr_mult", labels)
		}

		if _, ok := v["use_defaults"]; ok {
			return SLAfterRule{}, fmt.Errorf("sl_after: use_defaults requires a kind — wrap under \"trail_from_here\" or set \"kind\" explicitly (atr_offset/trail_from_here)")
		}
		return SLAfterRule{}, fmt.Errorf("sl_after object must contain \"kind\", \"atr_mult\", \"trail_from_here\", or \"trend_regime\"")
	default:
		return SLAfterRule{}, fmt.Errorf("sl_after must be a string or object, got %T", raw)
	}
}

var scalarMultKeysAtROffset = []string{"atr_mult", "atr_offset", "trail_atr_mult"}

var scalarMultKeysTrailFromHere = []string{"atr_mult", "trail_atr_mult", "atr_offset"}

func parseSLAfterATROffset(m map[string]interface{}, ctxLabel string, labels []string) (SLAfterRule, error) {
	_, hasTrend := m[regimeClassifierKey]
	_, hasUseDefaults := m["use_defaults"]
	if hasTrend || hasUseDefaults {
		if _, ok := firstNonNil(m, scalarMultKeysAtROffset...); ok {
			return SLAfterRule{}, fmt.Errorf("%s: cannot combine scalar atr_mult/atr_offset/trail_atr_mult with trend_regime/use_defaults — pick one shape", ctxLabel)
		}
		regimeRaw := map[string]interface{}{}
		if hasTrend {
			regimeRaw[regimeClassifierKey] = m[regimeClassifierKey]
		}
		if hasUseDefaults {
			regimeRaw["use_defaults"] = m["use_defaults"]
		}
		block, subErrs := parseRegimeATRBlock(regimeRaw, ctxLabel, regimeSurfaceSLAfter, slAfterLabelsForRaw(regimeRaw, labels))
		if len(subErrs) > 0 {
			return SLAfterRule{}, errors.New(strings.Join(subErrs, "; "))
		}
		rule := SLAfterRule{Kind: "atr_offset", ATRRegime: &block}
		return rule, validateSLAfterRule(rule)
	}
	mult, err := floatFromAnyChecked(firstPresent(m, "atr_mult", "atr_offset"))
	if err != nil {
		return SLAfterRule{}, fmt.Errorf("%s: %w", ctxLabel, err)
	}
	rule := SLAfterRule{Kind: "atr_offset", ATRMult: mult}
	return rule, validateSLAfterRule(rule)
}

func parseSLAfterTrailFromHere(m map[string]interface{}, ctxLabel string, labels []string) (SLAfterRule, error) {
	_, hasTrend := m[regimeClassifierKey]
	_, hasUseDefaults := m["use_defaults"]
	if tpRaw, hasTPFraction := m["tp_atr_fraction"]; hasTPFraction {
		if hasTrend || hasUseDefaults {
			return SLAfterRule{}, fmt.Errorf("%s: cannot combine tp_atr_fraction with trend_regime/use_defaults — pick one trail_from_here shape", ctxLabel)
		}
		if _, ok := firstNonNil(m, scalarMultKeysTrailFromHere...); ok {
			return SLAfterRule{}, fmt.Errorf("%s: cannot combine tp_atr_fraction with atr_mult/trail_atr_mult/atr_offset — pick one shape", ctxLabel)
		}
		rule, err := parseSLAfterTPATRFraction(tpRaw, ctxLabel+".tp_atr_fraction", labels)
		if err != nil {
			return SLAfterRule{}, err
		}
		return rule, validateSLAfterRule(rule)
	}
	if hasTrend || hasUseDefaults {
		if _, ok := firstNonNil(m, scalarMultKeysTrailFromHere...); ok {
			return SLAfterRule{}, fmt.Errorf("%s: cannot combine scalar atr_mult/trail_atr_mult/atr_offset with trend_regime/use_defaults — pick one shape", ctxLabel)
		}
		regimeRaw := map[string]interface{}{}
		if hasTrend {
			regimeRaw[regimeClassifierKey] = m[regimeClassifierKey]
		}
		if hasUseDefaults {
			regimeRaw["use_defaults"] = m["use_defaults"]
		}
		block, subErrs := parseRegimeATRBlock(regimeRaw, ctxLabel, regimeSurfaceSLAfterTrail, slAfterLabelsForRaw(regimeRaw, labels))
		if len(subErrs) > 0 {
			return SLAfterRule{}, errors.New(strings.Join(subErrs, "; "))
		}
		rule := SLAfterRule{Kind: "trail_from_here", TrailATRRegime: &block}
		return rule, validateSLAfterRule(rule)
	}
	mult, err := floatFromAnyChecked(firstPresent(m, "atr_mult", "trail_atr_mult"))
	if err != nil {
		return SLAfterRule{}, fmt.Errorf("%s: %w", ctxLabel, err)
	}
	rule := SLAfterRule{Kind: "trail_from_here", TrailATRMult: mult}
	return rule, validateSLAfterRule(rule)
}

func parseSLAfterTPATRFraction(raw interface{}, ctxLabel string, labels []string) (SLAfterRule, error) {
	switch v := raw.(type) {
	case map[string]interface{}:
		block, errs := parseRegimeFloatBlock(v, ctxLabel, slAfterLabelsForRaw(v, labels))
		if len(errs) > 0 {
			return SLAfterRule{}, errors.New(strings.Join(errs, "; "))
		}
		return SLAfterRule{Kind: "trail_from_here", TPATRFractionRegime: &block}, nil
	default:
		frac, err := floatFromAnyChecked(raw)
		if err != nil {
			return SLAfterRule{}, fmt.Errorf("%s: %w", ctxLabel, err)
		}
		if frac <= 0 {
			return SLAfterRule{}, fmt.Errorf("%s: must be > 0, got %g", ctxLabel, frac)
		}
		return SLAfterRule{Kind: "trail_from_here", TPATRFraction: frac}, nil
	}
}

func parseRegimeFloatBlock(raw map[string]interface{}, ctxLabel string, labels []string) (RegimeFloatBlock, []string) {
	var errs []string
	for k := range raw {
		if k != regimeClassifierKey {
			errs = append(errs, fmt.Sprintf("%s: unknown key %q (expected %q)", ctxLabel, k, regimeClassifierKey))
		}
	}
	trendRaw, ok := raw[regimeClassifierKey]
	if !ok {
		errs = append(errs, fmt.Sprintf("%s: missing %q", ctxLabel, regimeClassifierKey))
		return RegimeFloatBlock{}, errs
	}
	trend, ok := trendRaw.(map[string]interface{})
	if !ok {
		errs = append(errs, fmt.Sprintf("%s.%s: must be an object, got %T", ctxLabel, regimeClassifierKey, trendRaw))
		return RegimeFloatBlock{}, errs
	}
	if len(labels) == 0 {
		labels = canonicalTrendRegimeLabels
	}
	valid := map[string]bool{}
	for _, label := range labels {
		valid[label] = true
	}
	unknown := make([]string, 0)
	for label := range trend {
		if !valid[label] {
			unknown = append(unknown, label)
		}
	}
	sort.Strings(unknown)
	for _, label := range unknown {
		errs = append(errs, fmt.Sprintf("%s.%s: unknown regime label %q (expected one of: %s)",
			ctxLabel, regimeClassifierKey, label, strings.Join(labels, ", ")))
	}
	missing := make([]string, 0)

	bareDirectional := trend[regimeDirectionalBare] != nil
	for _, label := range labels {
		if _, ok := trend[label]; ok {
			continue
		}
		if regimeLabelFamilyCovered(label, bareDirectional) {
			continue
		}
		missing = append(missing, label)
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Sprintf("%s.%s: missing required regime labels: %s (must be exhaustive — no silent fallback)",
			ctxLabel, regimeClassifierKey, strings.Join(missing, ", ")))
	}
	out := RegimeFloatBlock{TrendRegime: map[string]float64{}}
	for _, label := range labels {
		rawEntry, ok := trend[label]
		if !ok {
			continue
		}
		frac, err := floatFromAnyChecked(rawEntry)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s.%s.%s: %v", ctxLabel, regimeClassifierKey, label, err))
			continue
		}
		if frac <= 0 {
			errs = append(errs, fmt.Sprintf("%s.%s.%s: must be > 0, got %g", ctxLabel, regimeClassifierKey, label, frac))
			continue
		}
		out.TrendRegime[label] = frac
	}
	if len(errs) > 0 {
		return RegimeFloatBlock{}, errs
	}
	return out, nil
}

func slAfterLabelsForRaw(raw interface{}, labels []string) []string {
	if labels != nil {
		return labels
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return canonicalTrendRegimeLabels
	}
	trend, ok := m[regimeClassifierKey].(map[string]interface{})
	if !ok || len(trend) == 0 {
		return canonicalTrendRegimeLabels
	}
	out := make([]string, 0, len(trend))
	for label := range trend {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func firstNonNil(m map[string]interface{}, keys ...string) (interface{}, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

type tierSLAfterRules struct {
	Default          SLAfterRule
	PerTier          []SLAfterRule
	Multiples        []float64
	TierFingerprints []string
}

func (r tierSLAfterRules) ForTier(idx int) SLAfterRule {
	if idx >= 0 && idx < len(r.PerTier) && !r.PerTier[idx].IsEmpty() {
		return r.PerTier[idx]
	}
	return r.Default
}

func (r tierSLAfterRules) TierMultiple(idx int) float64 {
	if idx >= 0 && idx < len(r.Multiples) {
		return r.Multiples[idx]
	}
	return 0
}

func (r tierSLAfterRules) HasAny() bool {
	if !r.Default.IsEmpty() {
		return true
	}
	for _, t := range r.PerTier {
		if !t.IsEmpty() {
			return true
		}
	}
	return false
}

func (r tierSLAfterRules) UsesTPATRFraction() bool {
	if r.Default.TPATRFraction > 0 || r.Default.TPATRFractionRegime != nil {
		return true
	}
	for _, rule := range r.PerTier {
		if rule.TPATRFraction > 0 || rule.TPATRFractionRegime != nil {
			return true
		}
	}
	return false
}

func (r tierSLAfterRules) EqualForReload(other tierSLAfterRules) bool {
	if !r.HasAny() && !other.HasAny() {
		return true
	}
	if !r.Default.Equal(other.Default) {
		return false
	}
	compareTierMetadata := r.UsesTPATRFraction() || other.UsesTPATRFraction()
	maxLen := len(r.PerTier)
	if len(other.PerTier) > maxLen {
		maxLen = len(other.PerTier)
	}
	for i := 0; i < maxLen; i++ {
		var a, b SLAfterRule
		if i < len(r.PerTier) {
			a = r.PerTier[i]
		}
		if i < len(other.PerTier) {
			b = other.PerTier[i]
		}
		if !a.Equal(b) {
			return false
		}
		if !compareTierMetadata {
			continue
		}
		am, bm := 0.0, 0.0
		if i < len(r.Multiples) {
			am = r.Multiples[i]
		}
		if i < len(other.Multiples) {
			bm = other.Multiples[i]
		}
		if am != bm {
			return false
		}
		af, bf := "", ""
		if i < len(r.TierFingerprints) {
			af = r.TierFingerprints[i]
		}
		if i < len(other.TierFingerprints) {
			bf = other.TierFingerprints[i]
		}
		if af != bf {
			return false
		}
	}
	return true
}

func parseStrategyTPSLAfterRules(sc StrategyConfig) (rules tierSLAfterRules, errs []string) {
	return parseStrategyTPSLAfterRulesForRegime(sc, nil, "")
}

func parseStrategyTPSLAfterRulesWithLabels(sc StrategyConfig, labels []string) (rules tierSLAfterRules, errs []string) {
	return parseStrategyTPSLAfterRulesForRegime(sc, labels, "")
}

func parseStrategyTPSLAfterRulesForRegime(sc StrategyConfig, labels []string, regime string) (rules tierSLAfterRules, errs []string) {
	if !strategyUsesTieredTPATRClose(sc) {
		return rules, nil
	}
	var defaultRaw interface{}
	var tiersRaw interface{}
	var refParams map[string]interface{}
	tieredName := ""
	regimeUseDefaults := false
	for _, ref := range sc.closeRefs() {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if !isTieredTPATRCloseName(n) {
			continue
		}
		tieredName = n
		refParams = ref.Params
		if v, ok := ref.Params["sl_after"]; ok {
			defaultRaw = v
		}
		if v, ok := closeTierListParam(ref.Params); ok {
			tiersRaw = v
		}
		if v, ok := ref.Params["use_defaults"].(bool); ok {
			regimeUseDefaults = v
		}
		break
	}

	unifiedScalar := false
	if closeParamsAreUnifiedRegime(refParams) {
		scalar, _, ok := unifiedRegimeScalarParams(refParams, regime)
		if !ok {
			return rules, errs
		}
		tiersRaw, _ = closeTierListParam(scalar)
		unifiedScalar = true
	}
	if defaultRaw != nil {
		r, err := parseSLAfterRuleWithLabels(defaultRaw, labels)
		if err != nil {
			errs = append(errs, fmt.Sprintf("sl_after (strategy-level): %v", err))
		} else if err := validateSLAfterRule(r); err != nil {
			errs = append(errs, fmt.Sprintf("sl_after (strategy-level): %v", err))
		} else {
			rules.Default = r
		}
	}
	if !unifiedScalar && (tieredName == "tiered_tp_atr_regime" || tieredName == "tiered_tp_atr_live_regime") {
		rules, regimeErrs := parseRegimeStrategyTPSLAfterRules(tieredName, tiersRaw, labels, regime, regimeUseDefaults, rules)
		errs = append(errs, regimeErrs...)
		return rules, errs
	}
	items, ok := tiersRaw.([]interface{})
	if !ok || len(items) == 0 {
		if rules.HasAny() {

			defaults := defaultHLProtectionTiers()
			rules.Multiples = make([]float64, len(defaults))
			rules.TierFingerprints = make([]string, len(defaults))
			for i, t := range defaults {
				rules.Multiples[i] = t.Multiple
				rules.TierFingerprints[i] = fmt.Sprintf("default:%g", t.Multiple)
			}
		}
		return rules, errs
	}
	type pair struct {
		multiple    float64
		rule        SLAfterRule
		fingerprint string
	}
	pairs := make([]pair, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		mult, err := floatFromAnyChecked(firstPresent(m, "atr_multiple", "multiple"))
		if err != nil || mult <= 0 {
			continue
		}
		var r SLAfterRule
		if raw, ok := m["sl_after"]; ok && raw != nil {
			parsed, perr := parseSLAfterRuleWithLabels(raw, labels)
			if perr != nil {
				errs = append(errs, fmt.Sprintf("sl_after (tier[%d]): %v", idx, perr))
			} else if verr := validateSLAfterRule(parsed); verr != nil {
				errs = append(errs, fmt.Sprintf("sl_after (tier[%d]): %v", idx, verr))
			} else {
				r = parsed
			}
		}
		pairs = append(pairs, pair{multiple: mult, rule: r, fingerprint: slAfterTierFingerprint(m)})
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].multiple < pairs[j].multiple })
	rules.PerTier = make([]SLAfterRule, len(pairs))
	rules.Multiples = make([]float64, len(pairs))
	rules.TierFingerprints = make([]string, len(pairs))
	for i, p := range pairs {
		rules.PerTier[i] = p.rule
		rules.Multiples[i] = p.multiple
		rules.TierFingerprints[i] = p.fingerprint
	}
	return rules, errs
}

func parseRegimeStrategyTPSLAfterRules(tieredName string, tiersRaw interface{}, labels []string, regime string, useDefaults bool, rules tierSLAfterRules) (tierSLAfterRules, []string) {
	var errs []string
	items, ok := tiersRaw.([]interface{})
	if !ok {
		if useDefaults && strings.TrimSpace(regime) != "" && rules.HasAny() {
			tiers := defaultRegimeTPTiersForRegime(regime)
			rules.Multiples = make([]float64, len(tiers))
			rules.TierFingerprints = make([]string, len(tiers))
			for i, tier := range tiers {
				rules.Multiples[i] = tier.Multiple
				rules.TierFingerprints[i] = fmt.Sprintf("use_defaults:%g", tier.Multiple)
			}
		}
		return rules, errs
	}
	parseRule := func(idx int, raw interface{}) SLAfterRule {
		if raw == nil {
			return SLAfterRule{}
		}
		parsed, perr := parseSLAfterRuleWithLabels(raw, labels)
		if perr != nil {
			errs = append(errs, fmt.Sprintf("sl_after (tier[%d]): %v", idx, perr))
			return SLAfterRule{}
		}
		if verr := validateSLAfterRule(parsed); verr != nil {
			errs = append(errs, fmt.Sprintf("sl_after (tier[%d]): %v", idx, verr))
			return SLAfterRule{}
		}
		return parsed
	}
	if strings.TrimSpace(regime) == "" {
		rules.PerTier = make([]SLAfterRule, 0, len(items))
		rules.TierFingerprints = make([]string, 0, len(items))
		for idx, item := range items {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			rules.PerTier = append(rules.PerTier, parseRule(idx, m["sl_after"]))
			rules.TierFingerprints = append(rules.TierFingerprints, slAfterTierFingerprint(m))
		}
		return rules, errs
	}
	specs, tierErrs := parseRegimeTPTiers(tiersRaw, tieredName, slAfterLabelsForRegimeTiers(tiersRaw, labels))
	errs = append(errs, tierErrs...)
	if len(tierErrs) > 0 {
		return rules, errs
	}
	type pair struct {
		multiple    float64
		rule        SLAfterRule
		fingerprint string
	}
	pairs := make([]pair, 0, len(specs))
	for idx, spec := range specs {
		entry, ok := spec.Block.Resolve(regime)
		if !ok || entry.ATR <= 0 {
			errs = append(errs, fmt.Sprintf("%s.tiers[%d]: regime %q resolved to no atr for sl_after tier alignment", tieredName, idx, regime))
			continue
		}
		var raw interface{}
		if idx < len(items) {
			if m, ok := items[idx].(map[string]interface{}); ok {
				raw = m["sl_after"]
			}
		}
		fp := ""
		if idx < len(items) {
			if m, ok := items[idx].(map[string]interface{}); ok {
				fp = slAfterTierFingerprint(m)
			}
		}
		pairs = append(pairs, pair{multiple: entry.ATR, rule: parseRule(idx, raw), fingerprint: fp})
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].multiple < pairs[j].multiple })
	rules.PerTier = make([]SLAfterRule, len(pairs))
	rules.Multiples = make([]float64, len(pairs))
	rules.TierFingerprints = make([]string, len(pairs))
	for i, p := range pairs {
		rules.PerTier[i] = p.rule
		rules.Multiples[i] = p.multiple
		rules.TierFingerprints[i] = p.fingerprint
	}
	return rules, errs
}

func slAfterTierFingerprint(m map[string]interface{}) string {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprintf("%v", m)
	}
	return string(b)
}

func slAfterLabelsForRegimeTiers(raw interface{}, labels []string) []string {
	if labels != nil {
		return labels
	}
	return regimeLabelsFromTierRaw(raw)
}

func strategyUsesRegimeTieredTPATRClose(sc StrategyConfig) bool {
	for _, ref := range sc.closeRefs() {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if n == "tiered_tp_atr_regime" || n == "tiered_tp_atr_live_regime" || n == dynamicCloseStrategyName {
			return true
		}
	}
	return false
}

func validatePostTPStopLossRules(sc StrategyConfig) []string {
	return validatePostTPStopLossRulesWithLabels(sc, canonicalTrendRegimeLabels)
}

func validatePostTPStopLossRulesWithLabels(sc StrategyConfig, labels []string) []string {
	rules, errs := parseStrategyTPSLAfterRulesWithLabels(sc, labels)
	out := append([]string(nil), errs...)
	for _, ref := range sc.closeRefs() {
		n := strings.ToLower(strings.TrimSpace(ref.Name))
		if isTieredTPATRCloseName(n) {
			continue
		}
		if isTrailingTPRatchetCloseName(n) {
			if _, ok := ref.Params["sl_after"]; ok {
				out = append(out, fmt.Sprintf("sl_after is not used with %q — use per-tier trailing_mult_after / tp_atr_fraction instead", ref.Name))
			}
			continue
		}
		if _, ok := ref.Params["sl_after"]; ok {
			out = append(out, fmt.Sprintf("sl_after is only honored on tiered_tp_atr / tiered_tp_atr_live close refs; found on %q", ref.Name))
		}
		if tiersRaw, ok := closeTierListParam(ref.Params); ok {
			if items, ok := tiersRaw.([]interface{}); ok {
				for i, item := range items {
					if m, ok := item.(map[string]interface{}); ok {
						if _, ok := m["sl_after"]; ok {
							out = append(out, fmt.Sprintf("sl_after on tier[%d] of %q has no effect; only honored on tiered_tp_atr* close refs", i, ref.Name))
						}
					}
				}
			}
		}
	}
	if !rules.HasAny() {
		return out
	}
	if (sc.TrailingStopATRMult != nil && *sc.TrailingStopATRMult > 0) ||
		(sc.TrailingStopPct != nil && *sc.TrailingStopPct > 0) {
		out = append(out, "sl_after cannot be combined with trailing_stop_atr_mult or trailing_stop_pct — trailing already walks the SL continuously")
	}
	hasFixedSL := (sc.StopLossATRMult != nil && *sc.StopLossATRMult > 0) ||
		(sc.StopLossATRRegime != nil && !sc.StopLossATRRegime.IsZero()) ||
		(sc.StopLossPct != nil && *sc.StopLossPct > 0) ||
		(sc.StopLossMarginPct != nil && *sc.StopLossMarginPct > 0)
	if !hasFixedSL {
		out = append(out, "sl_after requires a fixed stop-loss to adjust (set stop_loss_atr_mult, stop_loss_atr_regime, stop_loss_pct, or stop_loss_margin_pct)")
	}

	if sc.Type == "manual" {
		if rules.Default.Kind == "trail_from_here" {
			out = append(out, "sl_after: trail_from_here is not supported on manual strategies (perps only in v1) — use breakeven or atr_mult instead")
		}
		for i, r := range rules.PerTier {
			if r.Kind == "trail_from_here" {
				out = append(out, fmt.Sprintf("sl_after (tier[%d]): trail_from_here is not supported on manual strategies (perps only in v1) — use breakeven or atr_mult instead", i))
			}
		}
	}
	return out
}

type SLAdjustmentAlert struct {
	StrategyID           string
	Symbol               string
	Side                 string
	TierIdx              int
	OldTriggerPx         float64
	NewTriggerPx         float64
	Mode                 string
	TransitionToTrailing bool
}

func formatSLAdjustmentAlert(a SLAdjustmentAlert) string {
	headline := fmt.Sprintf("SL adjusted post-%s", tpTierLabel(a.TierIdx))
	if a.TransitionToTrailing {
		headline += " → trailing"
	}
	headline += fmt.Sprintf(" — %s", a.StrategyID)
	side := "LONG"
	if a.Side == "short" {
		side = "SHORT"
	}
	priceLine := fmt.Sprintf("%s %s", a.Symbol, side)
	var slLine string
	if a.OldTriggerPx > 0 {
		slLine = fmt.Sprintf("SL: $%.4f → $%.4f (%s)", a.OldTriggerPx, a.NewTriggerPx, a.Mode)
	} else {
		slLine = fmt.Sprintf("SL: $%.4f (%s)", a.NewTriggerPx, a.Mode)
	}
	return fmt.Sprintf("%s\n%s\n%s", headline, priceLine, slLine)
}

func notifySLAdjustment(sender ownerDMSender, enabled bool, alert SLAdjustmentAlert) {
	if !enabled || sender == nil || isNilSender(sender) {
		return
	}
	sender.SendOwnerDM(formatSLAdjustmentAlert(alert))
}

func findHighestClearedTier(tpOIDs []int64, tpArmedTiers []bool, fromIdx int) (int, bool) {
	if fromIdx < 0 {
		fromIdx = 0
	}
	highest := -1
	for i := fromIdx; i < len(tpOIDs); i++ {
		if tpOIDs[i] != 0 {
			continue
		}
		if i >= len(tpArmedTiers) || !tpArmedTiers[i] {
			continue
		}
		highest = i
	}
	if highest >= 0 {
		return highest, true
	}
	return 0, false
}

func runPostTPStopLossAdjustment(
	sc StrategyConfig,
	stratState *StrategyState,
	symbol string,
	mark float64,
	cfg *Config,
	mu *sync.RWMutex,
	notifier *MultiNotifier,
	logger *StrategyLogger,
	hlOnChainAbsQty map[string]float64,
) bool {
	if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
		return false
	}
	if stratState == nil || symbol == "" {
		return false
	}
	rules, _ := parseStrategyTPSLAfterRules(sc)
	if !rules.HasAny() {
		return false
	}

	mu.RLock()
	pos, ok := stratState.Positions[symbol]
	if !ok || pos == nil || pos.Quantity <= 0 || pos.InitialQuantity <= 0 {
		mu.RUnlock()
		return false
	}

	if pos.Quantity >= pos.InitialQuantity-1e-9 {
		mu.RUnlock()
		return false
	}
	clearedIdx, clearedOK := findHighestClearedTier(pos.TPOIDs, pos.TPArmedTiers, pos.SLAdjustedTiersProcessed)
	if !clearedOK {
		mu.RUnlock()
		return false
	}
	side := pos.Side

	avgCost := pos.riskAnchorPrice()
	entryATR := pos.EntryATR
	qty := pos.Quantity
	currentOID := pos.StopLossOID
	posRegime := protectionATRRegimeLabel(pos, sc)
	mu.RUnlock()

	if strategyUsesRegimeTieredTPATRClose(sc) {
		rules, _ = parseStrategyTPSLAfterRulesForRegime(sc, nil, posRegime)
	}
	rawRule := rules.ForTier(clearedIdx)
	tierMultiple := rules.TierMultiple(clearedIdx)

	if rawRule.IsEmpty() {
		mu.Lock()
		if p, ok := stratState.Positions[symbol]; ok && p != nil && p.SLAdjustedTiersProcessed <= clearedIdx {
			p.SLAdjustedTiersProcessed = clearedIdx + 1
		}
		mu.Unlock()
		return false
	}

	if currentOID == 0 {
		return false
	}

	rule, resolved := rawRule.resolveForRegimeAndTier(posRegime, tierMultiple)
	if !resolved {
		if logger != nil {
			logger.Info("post-TP SL adjustment for %s deferred: tier %d rule is regime-aware but pos.Regime=%q yields no entry",
				symbol, clearedIdx, posRegime)
		}
		return false
	}

	triggerPx, mode, computeOK := computePostTPStopLossTrigger(rule, side, avgCost, entryATR, mark)
	if !computeOK {

		return false
	}

	slEffectiveQty, capped := hlSLEffectiveQty(symbol, qty, hlOnChainAbsQty)
	if capped && logger != nil {
		logger.Warn("post-TP SL replace: virtual qty %.6f > on-chain %.6f for %s; capping SL size to on-chain qty (#621)", qty, slEffectiveQty, symbol)
	}

	if logger != nil {
		logger.Info("post-TP SL adjustment for %s: tier %d cleared, mode=%s new_trigger=$%.4f (cancel oid=%d)",
			symbol, clearedIdx, mode, triggerPx, currentOID)
	}
	result, stderr, err := runHyperliquidUpdateStopLossFunc(sc.Script, symbol, side, slEffectiveQty, triggerPx, currentOID)
	if stderr != "" && logger != nil {
		logger.Info("post-TP SL stderr: %s", stderr)
	}
	if err != nil {
		if logger != nil {
			logger.Error("post-TP SL update failed: %v", err)
		}
		return false
	}
	if result == nil || result.Error != "" {
		if logger != nil && result != nil && result.Error != "" {
			logger.Error("post-TP SL update returned error: %s", result.Error)
		}
		return false
	}
	if result.CancelStopLossError != "" && logger != nil {
		logger.Warn("post-TP SL cancel failed (non-fatal): %s", result.CancelStopLossError)
		if result.StopLossOID > 0 && currentOID > 0 && notifier != nil && notifier.HasBackends() {
			msg := fmt.Sprintf("**HL POST-TP SL CANCEL FAILED** [%s] %s old trigger OID %d may still be resting while new trigger OID %d was placed. Check HL open triggers before they accumulate toward the account cap. Error: %s",
				sc.ID, symbol, currentOID, result.StopLossOID, result.CancelStopLossError)
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
		}
	}
	if result.StopLossError != "" {
		if isHLOpenOrderCapRejection(result.StopLossError) {
			if logger != nil {
				logger.Error("CRITICAL: HL open-order-cap rejected post-TP SL update for %s — position may be under-protected: %s",
					symbol, result.StopLossError)
			}
			if notifier != nil && notifier.HasBackends() {
				msg := fmt.Sprintf("**HL OPEN-ORDER CAP HIT** [%s] %s post-TP SL update rejected: %s",
					sc.ID, symbol, result.StopLossError)
				notifier.SendToAllChannels(msg)
				notifier.SendOwnerDM(msg)
			}
		} else if logger != nil {
			logger.Warn("post-TP SL placement failed (non-fatal): %s", result.StopLossError)
		}
	}

	mu.Lock()
	p, ok := stratState.Positions[symbol]
	if !ok || p == nil || p.Quantity <= 0 || p.Side != side {
		mu.Unlock()
		return false
	}
	oldTrigger := p.StopLossTriggerPx
	if result.StopLossOID > 0 {
		p.StopLossOID = result.StopLossOID
	}
	if result.StopLossTriggerPx > 0 {
		p.StopLossTriggerPx = result.StopLossTriggerPx
	} else {
		p.StopLossTriggerPx = triggerPx
	}
	p.SLAdjustedTiersProcessed = clearedIdx + 1
	transitionedToTrailing := false
	if rule.Kind == "trail_from_here" && rule.TrailATRMult > 0 {
		mult := rule.TrailATRMult
		p.PostTPTrailingATRMult = &mult
		if mark > 0 {
			p.StopLossHighWaterPx = mark
		}
		transitionedToTrailing = true
	}
	newTrigger := p.StopLossTriggerPx
	if logger != nil {
		logger.Info("post-TP SL adjusted: oid=%d trigger=$%.4f→$%.4f (mode=%s tier=%d)",
			p.StopLossOID, oldTrigger, newTrigger, mode, clearedIdx)
	}
	mu.Unlock()

	if cfg != nil {
		notifySLAdjustment(notifier, cfg.NotifyTPSLFillsEnabled(), SLAdjustmentAlert{
			StrategyID:           sc.ID,
			Symbol:               symbol,
			Side:                 side,
			TierIdx:              clearedIdx,
			OldTriggerPx:         oldTrigger,
			NewTriggerPx:         newTrigger,
			Mode:                 mode,
			TransitionToTrailing: transitionedToTrailing,
		})
	}
	return true
}
