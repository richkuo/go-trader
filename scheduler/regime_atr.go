package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

var canonicalTrendRegimeLabels = []string{"trending_up", "trending_down", "ranging"}

const regimeDirectionalBare = "ranging_directional"

var regimeDirectionalSubs = map[string]bool{
	"ranging_directional_up":   true,
	"ranging_directional_down": true,
}

func regimeLabelFamilyCovered(label string, bareDirectionalPresent bool) bool {
	return bareDirectionalPresent && regimeDirectionalSubs[strings.TrimSpace(label)]
}

const regimeClassifierKey = "trend_regime"

type regimeATRSurface int

const (
	regimeSurfaceStopLoss regimeATRSurface = iota
	regimeSurfaceTrailing
	regimeSurfaceTPTierATROnly
	regimeSurfaceTPTierWithFrac
	regimeSurfaceSLAfter
	regimeSurfaceSLAfterTrail
)

type RegimeATREntry struct {
	ATR           float64
	CloseFraction float64
	HasCloseFrac  bool
}

type RegimeATRBlock struct {
	UseDefaults bool
	TrendRegime map[string]RegimeATREntry
	raw         map[string]interface{}
}

func (b *RegimeATRBlock) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("regime_atr_block: %w", err)
	}
	b.raw = raw
	return nil
}

func (b RegimeATRBlock) MarshalJSON() ([]byte, error) {
	if b.UseDefaults && len(b.TrendRegime) > 0 {
		return json.Marshal(map[string]bool{"use_defaults": true})
	}
	if len(b.TrendRegime) == 0 {
		return json.Marshal(b.raw)
	}
	out := map[string]map[string]map[string]interface{}{regimeClassifierKey: {}}
	for label, entry := range b.TrendRegime {
		e := map[string]interface{}{"atr_multiple": entry.ATR}
		if entry.HasCloseFrac {
			e["close_fraction"] = entry.CloseFraction
		}
		out[regimeClassifierKey][label] = e
	}
	return json.Marshal(out)
}

func (b *RegimeATRBlock) ResolveSurface(ctxLabel string, surface regimeATRSurface) []string {
	return b.ResolveSurfaceWithLabels(ctxLabel, surface, canonicalTrendRegimeLabels)
}

func (b *RegimeATRBlock) ResolveSurfaceWithLabels(ctxLabel string, surface regimeATRSurface, labels []string) []string {
	if b == nil {
		return nil
	}
	if len(labels) == 0 {
		labels = canonicalTrendRegimeLabels
	}
	parsed, errs := parseRegimeATRBlock(b.raw, ctxLabel, surface, labels)
	if len(errs) > 0 {
		return errs
	}
	b.UseDefaults = parsed.UseDefaults
	b.TrendRegime = parsed.TrendRegime
	return nil
}

func (b *RegimeATRBlock) EqualForReload(other *RegimeATRBlock) bool {
	if !b.EqualEffectiveForReload(other) {
		return false
	}
	if b == nil || b.IsZero() {
		return true
	}
	return b.UseDefaults == other.UseDefaults
}

func (b *RegimeATRBlock) EqualEffectiveForReload(other *RegimeATRBlock) bool {
	aZero := b == nil || b.IsZero()
	bZero := other == nil || other.IsZero()
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
		vb, ok := other.TrendRegime[k]
		if !ok {
			return false
		}
		if va.ATR != vb.ATR || va.CloseFraction != vb.CloseFraction || va.HasCloseFrac != vb.HasCloseFrac {
			return false
		}
	}
	return true
}

func (b RegimeATRBlock) IsZero() bool {
	return !b.UseDefaults && len(b.TrendRegime) == 0
}

func (b *RegimeATRBlock) IsConfigured() bool {
	if b == nil {
		return false
	}
	if b.UseDefaults || len(b.TrendRegime) > 0 {
		return true
	}
	return len(b.raw) > 0
}

func (b RegimeATRBlock) Resolve(regime string) (RegimeATREntry, bool) {
	if b.TrendRegime == nil {
		return RegimeATREntry{}, false
	}
	r := strings.TrimSpace(regime)
	if entry, ok := b.TrendRegime[r]; ok {
		return entry, true
	}
	if regimeDirectionalSubs[r] {
		if entry, ok := b.TrendRegime[regimeDirectionalBare]; ok {
			return entry, true
		}
	}
	return RegimeATREntry{}, false
}

var regimeATRDefaults = struct {
	StopLoss map[string]RegimeATREntry
	Trailing map[string]RegimeATREntry
}{
	StopLoss: map[string]RegimeATREntry{
		"trending_up":   {ATR: 2.0},
		"trending_down": {ATR: 2.0},
		"ranging":       {ATR: 1.5},
	},
	Trailing: map[string]RegimeATREntry{
		"trending_up":              {ATR: 2.5},
		"trending_down":            {ATR: 2.5},
		"ranging":                  {ATR: 2.0},
		"trending_up_clean":        {ATR: 2.5},
		"trending_down_clean":      {ATR: 2.5},
		"trending_up_choppy":       {ATR: 2.25},
		"trending_down_choppy":     {ATR: 2.25},
		"ranging_quiet":            {ATR: 1.0},
		"ranging_volatile":         {ATR: 1.25},
		"ranging_directional":      {ATR: 1.5},
		"ranging_directional_up":   {ATR: 1.5},
		"ranging_directional_down": {ATR: 1.5},
	},
}

func regimeCloseDefaultGroup(label string) (string, bool) {
	l := strings.TrimSpace(label)
	switch {
	case l == "":
		return "", false
	case strings.HasSuffix(l, "_clean"):
		return "clean", true
	case strings.HasSuffix(l, "_choppy"):
		return "choppy", true
	case strings.HasPrefix(l, "ranging"):
		return "ranging", true
	case strings.HasPrefix(l, "trending_up"), strings.HasPrefix(l, "trending_down"):
		return "choppy", true
	}
	return "", false
}

var regimeTPTierGroupDefaults = map[string][]hlProtectionTier{
	"clean":   {{Multiple: 2.5, Fraction: 0.25}, {Multiple: 4.0, Fraction: 0.50}, {Multiple: 5.5, Fraction: 0.75}, {Multiple: 7.0, Fraction: 1.00}},
	"choppy":  {{Multiple: 1.5, Fraction: 0.40}, {Multiple: 3.0, Fraction: 0.80}, {Multiple: 5.0, Fraction: 1.00}},
	"ranging": {{Multiple: 0.5, Fraction: 0.50}, {Multiple: 1.0, Fraction: 1.00}},
}

var regimeTPFleetDefaultLabelsByGroup = map[string][]string{
	"clean":   {"trending_up_clean", "trending_down_clean"},
	"choppy":  {"trending_up", "trending_down", "trending_up_choppy", "trending_down_choppy"},
	"ranging": {"ranging", "ranging_quiet", "ranging_volatile", "ranging_directional", "ranging_directional_up", "ranging_directional_down"},
}

func defaultRegimeBlockForSurface(surface regimeATRSurface) (map[string]RegimeATREntry, bool) {
	switch surface {
	case regimeSurfaceStopLoss:
		return cloneRegimeMap(regimeATRDefaults.StopLoss), true
	case regimeSurfaceTrailing:
		return cloneRegimeMap(regimeATRDefaults.Trailing), true
	default:
		return nil, false
	}
}

func cloneRegimeMap(in map[string]RegimeATREntry) map[string]RegimeATREntry {
	if in == nil {
		return nil
	}
	out := make(map[string]RegimeATREntry, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mapRegimeToBaselineFamily(baseline map[string]RegimeATREntry, label string) (RegimeATREntry, bool) {
	if e, ok := baseline[label]; ok {
		return e, true
	}
	switch {
	case strings.HasPrefix(label, "trending_up"):
		e, ok := baseline["trending_up"]
		return e, ok
	case strings.HasPrefix(label, "trending_down"):
		e, ok := baseline["trending_down"]
		return e, ok
	case strings.HasPrefix(label, "ranging"):
		e, ok := baseline["ranging"]
		return e, ok
	}
	return RegimeATREntry{}, false
}

func expandRegimeATRDefaultsForLabels(baseline map[string]RegimeATREntry, labels []string) map[string]RegimeATREntry {
	out := make(map[string]RegimeATREntry, len(labels))
	for _, label := range labels {
		if e, ok := mapRegimeToBaselineFamily(baseline, label); ok {
			out[label] = e
		}
	}
	return out
}

func parseRegimeATRBlock(raw map[string]interface{}, ctxLabel string, surface regimeATRSurface, labels []string) (RegimeATRBlock, []string) {
	var errs []string
	if raw == nil {
		return RegimeATRBlock{}, nil
	}

	allowedTopKeys := map[string]bool{
		"use_defaults":      true,
		regimeClassifierKey: true,
	}
	for k := range raw {
		if !allowedTopKeys[k] {
			errs = append(errs, fmt.Sprintf("%s: unknown key %q (expected %q or %q)", ctxLabel, k, "use_defaults", regimeClassifierKey))
		}
	}

	useDefaultsRaw, hasUseDefaults := raw["use_defaults"]
	trendRaw, hasTrend := raw[regimeClassifierKey]

	useDefaults := false
	if hasUseDefaults {
		b, ok := useDefaultsRaw.(bool)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: use_defaults must be a boolean, got %T", ctxLabel, useDefaultsRaw))
		} else {
			useDefaults = b
		}
	}

	if useDefaults && hasTrend {
		errs = append(errs, fmt.Sprintf("%s: cannot combine use_defaults:true with explicit %s (use_defaults is all-or-nothing)", ctxLabel, regimeClassifierKey))
	}

	if useDefaults {
		baseline, ok := defaultRegimeBlockForSurface(surface)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: use_defaults not supported on this surface (tier-level use_defaults is handled by the close evaluator parser)", ctxLabel))
			return RegimeATRBlock{}, errs
		}
		if len(labels) == 0 {
			labels = canonicalTrendRegimeLabels
		}
		return RegimeATRBlock{UseDefaults: true, TrendRegime: expandRegimeATRDefaultsForLabels(baseline, labels)}, errs
	}

	if !hasTrend {
		errs = append(errs, fmt.Sprintf("%s: missing %q (either set use_defaults:true or supply a trend_regime block)", ctxLabel, regimeClassifierKey))
		return RegimeATRBlock{}, errs
	}

	trendMap, ok := trendRaw.(map[string]interface{})
	if !ok {
		errs = append(errs, fmt.Sprintf("%s: %s must be an object, got %T", ctxLabel, regimeClassifierKey, trendRaw))
		return RegimeATRBlock{}, errs
	}

	if len(labels) == 0 {
		labels = canonicalTrendRegimeLabels
	}
	validLabels := map[string]bool{}
	for _, l := range labels {
		validLabels[l] = true
	}

	unknownLabels := []string{}
	for k := range trendMap {
		if !validLabels[k] {
			unknownLabels = append(unknownLabels, k)
		}
	}
	sort.Strings(unknownLabels)
	for _, k := range unknownLabels {
		errs = append(errs, fmt.Sprintf("%s.%s: unknown regime label %q (expected one of: %s)", ctxLabel, regimeClassifierKey, k, strings.Join(labels, ", ")))
	}

	missingLabels := []string{}
	bareDirectional := trendMap[regimeDirectionalBare] != nil
	for _, l := range labels {
		if _, ok := trendMap[l]; ok {
			continue
		}
		if regimeLabelFamilyCovered(l, bareDirectional) {
			continue
		}
		missingLabels = append(missingLabels, l)
	}
	if len(missingLabels) > 0 {
		errs = append(errs, fmt.Sprintf("%s.%s: missing required regime labels: %s (must be exhaustive — no silent fallback)", ctxLabel, regimeClassifierKey, strings.Join(missingLabels, ", ")))
	}

	result := make(map[string]RegimeATREntry, len(labels))
	for _, label := range labels {
		labelRaw, ok := trendMap[label]
		if !ok {
			continue
		}
		entryMap, ok := labelRaw.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.%s.%s: must be an object, got %T", ctxLabel, regimeClassifierKey, label, labelRaw))
			continue
		}

		allowFrac := surface == regimeSurfaceTPTierWithFrac
		allowedEntryKeys := map[string]bool{"atr_multiple": true}
		if allowFrac {
			allowedEntryKeys["close_fraction"] = true
		}
		entryUnknown := []string{}
		for k := range entryMap {
			if !allowedEntryKeys[k] {
				entryUnknown = append(entryUnknown, k)
			}
		}
		sort.Strings(entryUnknown)
		for _, k := range entryUnknown {
			hint := ""
			if k == "close_fraction" {
				hint = " — close_fraction is only allowed inside close-evaluator tiers; for SL/trailing/sl_after surfaces, only atr_multiple is accepted"
			}
			errs = append(errs, fmt.Sprintf("%s.%s.%s: unknown key %q%s", ctxLabel, regimeClassifierKey, label, k, hint))
		}

		atrRaw, hasATR, atrErr := regimeEntryATRRaw(entryMap)
		if atrErr != nil {
			errs = append(errs, fmt.Sprintf("%s.%s.%s: %v", ctxLabel, regimeClassifierKey, label, atrErr))
			continue
		}
		if !hasATR {
			errs = append(errs, fmt.Sprintf("%s.%s.%s: missing required %q", ctxLabel, regimeClassifierKey, label, "atr_multiple"))
			continue
		}
		atr, err := floatFromAnyChecked(atrRaw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s.%s.%s.atr_multiple: %v", ctxLabel, regimeClassifierKey, label, err))
			continue
		}
		if surface != regimeSurfaceSLAfter && atr <= 0 {
			errs = append(errs, fmt.Sprintf("%s.%s.%s.atr_multiple: must be > 0, got %g", ctxLabel, regimeClassifierKey, label, atr))
			continue
		}
		entry := RegimeATREntry{ATR: atr}
		if allowFrac {
			if fracRaw, ok := entryMap["close_fraction"]; ok {
				f, err := floatFromAnyChecked(fracRaw)
				if err != nil {
					errs = append(errs, fmt.Sprintf("%s.%s.%s.close_fraction: %v", ctxLabel, regimeClassifierKey, label, err))
					continue
				}
				if f <= 0 || f > 1 {
					errs = append(errs, fmt.Sprintf("%s.%s.%s.close_fraction: must be in (0, 1], got %g", ctxLabel, regimeClassifierKey, label, f))
					continue
				}
				entry.CloseFraction = f
				entry.HasCloseFrac = true
			}
		}
		result[label] = entry
	}

	return RegimeATRBlock{TrendRegime: result}, errs
}

func regimeEntryATRRaw(entryMap map[string]interface{}) (interface{}, bool, error) {
	canon, hasCanon := entryMap["atr_multiple"]
	_, hasLegacy := entryMap["atr"]
	switch {
	case hasCanon && hasLegacy:
		return nil, false, fmt.Errorf("set only one of %q or %q (%q is the deprecated alias)", "atr_multiple", "atr", "atr")
	case hasCanon:
		return canon, true, nil
	default:
		return nil, false, nil
	}
}

func resolveRegimeATR(block RegimeATRBlock, regime string) (float64, bool) {
	entry, ok := block.Resolve(regime)
	if !ok || entry.ATR <= 0 {
		return 0, false
	}
	return entry.ATR, true
}

type regimeTierSpec struct {
	Block                RegimeATRBlock
	TierCloseFraction    float64
	HasTierCloseFraction bool
}

func parseRegimeTPTiers(raw interface{}, ctxLabel string, labels []string) ([]regimeTierSpec, []string) {
	var errs []string
	if raw == nil {
		return nil, errs
	}
	if len(labels) == 0 {
		labels = canonicalTrendRegimeLabels
	}
	items, ok := raw.([]interface{})
	if !ok {
		errs = append(errs, fmt.Sprintf("%s.tiers: must be a list, got %T", ctxLabel, raw))
		return nil, errs
	}
	out := make([]regimeTierSpec, 0, len(items))
	for idx, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.tiers[%d]: must be an object, got %T", ctxLabel, idx, item))
			continue
		}
		perRegimeHasFrac := false
		if trendRaw, ok := m[regimeClassifierKey].(map[string]interface{}); ok {
			for _, v := range trendRaw {
				if entryMap, ok := v.(map[string]interface{}); ok {
					if _, ok := entryMap["close_fraction"]; ok {
						perRegimeHasFrac = true
						break
					}
				}
			}
		}
		tierLevelFrac, hasTierLevelFrac := m["close_fraction"]
		if perRegimeHasFrac && hasTierLevelFrac {
			errs = append(errs, fmt.Sprintf("%s.tiers[%d]: cannot combine per-regime close_fraction with tier-level scalar close_fraction (pick one shape per tier)", ctxLabel, idx))
			continue
		}
		if !perRegimeHasFrac && !hasTierLevelFrac {
			errs = append(errs, fmt.Sprintf("%s.tiers[%d]: missing close_fraction (either at tier level or inside every per-regime entry)", ctxLabel, idx))
			continue
		}
		surface := regimeSurfaceTPTierATROnly
		if perRegimeHasFrac {
			surface = regimeSurfaceTPTierWithFrac
		}
		subset := make(map[string]interface{}, len(m))
		for k, v := range m {
			if k == "close_fraction" || k == "sl_after" {
				continue
			}
			subset[k] = v
		}
		block, subErrs := parseRegimeATRBlock(subset, fmt.Sprintf("%s.tiers[%d]", ctxLabel, idx), surface, labels)
		errs = append(errs, subErrs...)

		spec := regimeTierSpec{Block: block}
		if hasTierLevelFrac {
			frac, err := floatFromAnyChecked(tierLevelFrac)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s.tiers[%d].close_fraction: %v", ctxLabel, idx, err))
				continue
			}
			if frac <= 0 || frac > 1 {
				errs = append(errs, fmt.Sprintf("%s.tiers[%d].close_fraction: must be in (0, 1], got %g", ctxLabel, idx, frac))
				continue
			}
			spec.TierCloseFraction = frac
			spec.HasTierCloseFraction = true
		}
		out = append(out, spec)
	}
	return out, errs
}

func regimeLabelsFromTierRaw(raw interface{}) []string {
	items, ok := raw.([]interface{})
	if !ok {
		return canonicalTrendRegimeLabels
	}
	set := map[string]bool{}
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		tr, ok := m[regimeClassifierKey].(map[string]interface{})
		if !ok {
			continue
		}
		for k := range tr {
			set[k] = true
		}
	}
	if len(set) == 0 {
		return canonicalTrendRegimeLabels
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func resolveRegimeTPTiers(raw interface{}, regime string) []hlProtectionTier {
	specs, errs := parseRegimeTPTiers(raw, "tiered_tp_atr_regime", regimeLabelsFromTierRaw(raw))
	if len(errs) > 0 || len(specs) == 0 || regime == "" {
		return nil
	}
	out := make([]hlProtectionTier, 0, len(specs))
	for _, spec := range specs {
		entry, ok := spec.Block.Resolve(regime)
		if !ok || entry.ATR <= 0 {
			return nil
		}
		frac := 0.0
		if spec.HasTierCloseFraction {
			frac = spec.TierCloseFraction
		} else if entry.HasCloseFrac && entry.CloseFraction > 0 {
			frac = entry.CloseFraction
		}
		if frac <= 0 {
			return nil
		}
		out = append(out, hlProtectionTier{Multiple: entry.ATR, Fraction: frac})
	}
	return out
}

func validateRegimeATRConfig(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	regimeEnabled := cfg.Regime != nil && cfg.Regime.Enabled
	var errs []string
	for i := range cfg.Strategies {
		sc := &cfg.Strategies[i]
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)
		if sc.ID == "" {
			prefix = fmt.Sprintf("strategy[%d]", i)
		}

		usesRegime := false

		atrLabels := canonicalTrendRegimeLabels
		atrWindow := ""
		atrClassifier := ""
		if regimeEnabled {
			atrLabels = regimeLabelsForStrategyWindow(*sc, cfg.Regime, "atr")
			atrWindow = resolveStrategyRegimeWindow(*sc, "atr", cfg.Regime)
			atrClassifier = regimeClassifierForWindow(cfg.Regime, atrWindow)
		}
		wrapATR := func(e string) string {
			if regimeEnabled {
				return fmt.Sprintf("%s (regime_atr_window %q, classifier %q): %s", prefix, atrWindow, atrClassifier, e)
			}
			return e
		}

		if sc.StopLossATRMultRegime != nil {
			sub := sc.StopLossATRMultRegime.ResolveSurfaceWithLabels(prefix+".stop_loss_atr_mult_regime", regimeSurfaceStopLoss, atrLabels)
			for _, e := range sub {
				errs = append(errs, wrapATR(e))
			}
			if len(sub) == 0 && !sc.StopLossATRMultRegime.IsZero() {
				usesRegime = true
				if sc.StopLossATRMult != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult_regime is mutually exclusive with stop_loss_atr_mult", prefix))
				}
				if sc.StopLossPct != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult_regime is mutually exclusive with stop_loss_pct", prefix))
				}
				if sc.StopLossMarginPct != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult_regime is mutually exclusive with stop_loss_margin_pct", prefix))
				}
				if sc.TrailingStopPct != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult_regime is mutually exclusive with trailing_stop_pct", prefix))
				}
				if sc.TrailingStopATRMult != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult_regime is mutually exclusive with trailing_stop_atr_mult", prefix))
				}
				if sc.TrailingStopATRMultRegime.IsConfigured() {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult_regime is mutually exclusive with trailing_stop_atr_mult_regime", prefix))
				}
				if sc.Platform != "hyperliquid" || sc.Type != "perps" {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_mult_regime is HL perps only", prefix))
				}
			}
		}
		if sc.TrailingStopATRMultRegime != nil {
			sub := sc.TrailingStopATRMultRegime.ResolveSurfaceWithLabels(prefix+".trailing_stop_atr_mult_regime", regimeSurfaceTrailing, atrLabels)
			for _, e := range sub {
				errs = append(errs, wrapATR(e))
			}
			if len(sub) == 0 && !sc.TrailingStopATRMultRegime.IsZero() {
				usesRegime = true
				if sc.TrailingStopATRMult != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult_regime is mutually exclusive with trailing_stop_atr_mult", prefix))
				}
				if sc.TrailingStopPct != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult_regime is mutually exclusive with trailing_stop_pct", prefix))
				}
				if sc.StopLossPct != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult_regime is mutually exclusive with stop_loss_pct", prefix))
				}
				if sc.StopLossMarginPct != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult_regime is mutually exclusive with stop_loss_margin_pct", prefix))
				}
				if sc.StopLossATRMult != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult_regime is mutually exclusive with stop_loss_atr_mult", prefix))
				}
				manualRatchet := sc.Type == "manual" && strategyUsesTrailingTPRatchetClose(*sc)
				if sc.Platform != "hyperliquid" || (sc.Type != "perps" && !manualRatchet) {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_mult_regime is HL perps only (or HL manual trailing_tp_ratchet_regime)", prefix))
				}
			}
		}

		for _, ref := range sc.closeRefs() {
			name := strings.ToLower(strings.TrimSpace(ref.Name))
			if name == dynamicCloseStrategyName {
				usesRegime = true
				subPrefix := fmt.Sprintf("%s.close_strategy(%s)", prefix, ref.Name)
				if sc.Platform != "hyperliquid" || (sc.Type != "perps" && sc.Type != "manual") {
					errs = append(errs, fmt.Sprintf("%s: %s is HL perps/manual only", subPrefix, ref.Name))
				}
				if !closeParamsAreUnifiedRegime(ref.Params) {
					errs = append(errs, fmt.Sprintf("%s: requires unified per-regime trend_regime block", subPrefix))
				} else {
					errs = append(errs, validateDynamicRegimeClose(ref.Params, atrLabels, subPrefix)...)
				}
				continue
			}
			if name != "tiered_tp_atr_regime" && name != "tiered_tp_atr_live_regime" {
				continue
			}
			usesRegime = true
			subPrefix := fmt.Sprintf("%s.close_strategy(%s)", prefix, ref.Name)
			if closeParamsAreUnifiedRegime(ref.Params) {
				errs = append(errs, validateUnifiedRegimeClose(ref.Params, atrLabels, subPrefix)...)
				continue
			}
			useDefaults := false
			if v, ok := ref.Params["use_defaults"].(bool); ok {
				useDefaults = v
			}
			tiersRaw, hasTiers := closeTierListParam(ref.Params)
			if useDefaults && hasTiers {
				errs = append(errs, fmt.Sprintf("%s: cannot combine use_defaults:true with explicit tiers (use_defaults is all-or-nothing)", subPrefix))
				continue
			}
			if !useDefaults && !hasTiers {
				errs = append(errs, fmt.Sprintf("%s: missing tiers (either set use_defaults:true or supply a tiers list)", subPrefix))
				continue
			}
			for k := range ref.Params {
				switch k {
				case "use_defaults", "tp_tiers", "tiers", "atr_source", "sl_after":
				default:
					errs = append(errs, fmt.Sprintf("%s: unknown param %q (allowed: use_defaults, tp_tiers, atr_source, sl_after)", subPrefix, k))
				}
			}
			if useDefaults {
				continue
			}
			if specs, subErrs := parseRegimeTPTiers(tiersRaw, subPrefix, atrLabels); len(subErrs) > 0 {
				errs = append(errs, subErrs...)
			} else if len(specs) < 2 {
				errs = append(errs, fmt.Sprintf("%s: must have at least 2 tiers, got %d", subPrefix, len(specs)))
			}
		}

		if usesRegime && !regimeEnabled {
			errs = append(errs, fmt.Sprintf("%s: regime-aware stop/TP fields require top-level regime.enabled=true", prefix))
		}
		errs = append(errs, validateUnifiedCloseSoleOwner(*sc, prefix)...)
		errs = append(errs, validateTrailingTPRatchetClose(*sc, atrLabels, regimeEnabled)...)
	}
	return errs
}

func defaultRegimeTPTiersForRegime(regime string) []hlProtectionTier {
	if regime == "" {
		return nil
	}
	group, ok := regimeCloseDefaultGroup(regime)
	if !ok {
		return nil
	}
	ladder := regimeTPTierGroupDefaults[group]
	if len(ladder) < 2 {
		return nil
	}
	out := make([]hlProtectionTier, len(ladder))
	copy(out, ladder)
	return finalizeProtectionTiers(out)
}

func InspectRegimeTPFleetDefaultBlocks() []RegimeATRBlock {
	maxTiers := 0
	for _, ladder := range regimeTPTierGroupDefaults {
		if len(ladder) > maxTiers {
			maxTiers = len(ladder)
		}
	}
	out := make([]RegimeATRBlock, maxTiers)
	for i := 0; i < maxTiers; i++ {
		tr := map[string]RegimeATREntry{}
		for group, ladder := range regimeTPTierGroupDefaults {
			if i >= len(ladder) {
				continue
			}
			for _, label := range regimeTPFleetDefaultLabelsByGroup[group] {
				tr[label] = RegimeATREntry{ATR: ladder[i].Multiple, CloseFraction: ladder[i].Fraction, HasCloseFrac: true}
			}
		}
		out[i] = RegimeATRBlock{UseDefaults: true, TrendRegime: tr}
	}
	return out
}
