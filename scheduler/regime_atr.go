package main

// Regime-aware ATR multiplier resolver (#733).
//
// This file is the single source of truth for parsing the new
// `trend_regime` block that powers `tiered_tp_atr_regime`,
// `tiered_tp_atr_live_regime`, `stop_loss_atr_regime`, and
// `trailing_stop_atr_regime`.
//
// Existing scalar surfaces (`tiered_tp_atr`, `stop_loss_atr_mult`, etc.)
// are untouched — operators opt in by switching to the *_regime sibling.
//
// Shape (use_defaults form):
//
//	{ "use_defaults": true }
//
// Shape (explicit form):
//
//	{ "trend_regime": {
//	    "trending_up":   { "atr_multiple": 2.0, "close_fraction": 0.5 },
//	    "trending_down": { "atr_multiple": 2.0, "close_fraction": 0.5 },
//	    "ranging":       { "atr_multiple": 1.5, "close_fraction": 0.5 }
//	  }
//	}
//
// `close_fraction` per-regime is only honored inside close-evaluator tiers;
// strategy-level SL/trailing fields reject it during validation.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// canonicalTrendRegimeLabels mirrors validRegimeLabels — when not using
// use_defaults, every label here must appear in the trend_regime map.
var canonicalTrendRegimeLabels = []string{"trending_up", "trending_down", "ranging"}

// regimeDirectionalBare is the bare ranging-directional label, whose
// `ranging_directional_up`/`_down` sub-labels the producer (#1124) splits out
// by drift sign. The bare label remains the exact-zero neutral fallback the
// producer emits at return_eff == 0, so it is the parent of the family.
const regimeDirectionalBare = "ranging_directional"

// regimeDirectionalSubs is the set of #1124 directional sub-labels that the
// bare `ranging_directional` label covers for exhaustiveness and runtime
// resolution. Kept as a helper so every consumer applies the family rule
// symmetrically (a missed consumer is a silent never-arm of an auto-protective
// SL/exit — see #1124 review). The rule is one-directional: bare covers its
// subs, never the reverse, so an explicit `_up`/`_down` key always wins on an
// exact match and the bare fallback only fires on a miss.
var regimeDirectionalSubs = map[string]bool{
	"ranging_directional_up":   true,
	"ranging_directional_down": true,
}

// regimeLabelFamilyCovered reports whether an omitted `label` is nevertheless
// satisfied for exhaustiveness because the bare `ranging_directional` parent
// is present (`bareDirectionalPresent`). This is the back-compat rule: a
// 7-label composite block keyed on bare `ranging_directional` still covers
// the `_up`/`_down` sub-labels, so it must not be rejected as non-exhaustive.
// Sub-labels-only (no bare parent) is NOT covered — the producer still emits
// the bare label at return_eff == 0, so omitting it leaves a naked-SL hole.
func regimeLabelFamilyCovered(label string, bareDirectionalPresent bool) bool {
	return bareDirectionalPresent && regimeDirectionalSubs[strings.TrimSpace(label)]
}

// regimeClassifierKey is the wrapper key required around per-label blocks.
// Reserves space for future classifiers (e.g. "vol_regime") to land as
// sibling keys without renaming.
const regimeClassifierKey = "trend_regime"

// regimeATRSurface enumerates the four call sites that consume a
// RegimeATRBlock. Determines which `use_defaults` baseline applies and
// whether `close_fraction` is permitted inside per-regime entries.
type regimeATRSurface int

const (
	regimeSurfaceStopLoss       regimeATRSurface = iota // stop_loss_atr_regime: ATR only, strictly positive
	regimeSurfaceTrailing                               // trailing_stop_atr_regime: ATR only, strictly positive
	regimeSurfaceTPTierATROnly                          // tier with tier-level scalar close_fraction: ATR only, strictly positive
	regimeSurfaceTPTierWithFrac                         // tier with per-regime close_fraction: ATR + close_fraction, strictly positive
	regimeSurfaceSLAfter                                // sl_after.atr (atr_offset variant): ATR only, signed allowed (0 and negatives legal — matches the scalar sl_after atr_offset semantics where signed mults move the SL behind/ahead of entry)
	regimeSurfaceSLAfterTrail                           // sl_after.trail_from_here.atr: ATR only, strictly positive (trail distance is a magnitude)
)

// RegimeATREntry is one resolution slot inside the trend_regime map.
// ATR is required; CloseFraction is only set on tier surfaces.
type RegimeATREntry struct {
	ATR           float64
	CloseFraction float64
	HasCloseFrac  bool
}

// RegimeATRBlock is the parsed shape of one regime-aware multiplier spec.
// Exactly one of UseDefaults / TrendRegime is meaningful — when UseDefaults
// is true the TrendRegime map is still populated (expanded from the per-
// surface baseline) so runtime resolution has a single code path. The flag
// is preserved so `go-trader inspect` can show provenance.
//
// The raw field captures the JSON shape at unmarshal time so LoadConfig can
// run the full surface-aware validation pass with the right strategy ID
// scope; ResolveSurface() must be called before the block is used at
// runtime, otherwise IsZero() always returns true.
type RegimeATRBlock struct {
	UseDefaults bool
	TrendRegime map[string]RegimeATREntry
	raw         map[string]interface{}
}

// UnmarshalJSON captures the raw object shape for later validation.
// LoadConfig is the single caller of ResolveSurface() that converts the
// raw shape into the typed UseDefaults/TrendRegime fields with strategy-
// scoped error messages. Until then, the block is opaque to runtime code.
func (b *RegimeATRBlock) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("regime_atr_block: %w", err)
	}
	b.raw = raw
	return nil
}

// MarshalJSON renders the block back out in its canonical form. Used by
// hot-reload diff logging and `go-trader inspect`.
func (b RegimeATRBlock) MarshalJSON() ([]byte, error) {
	if b.UseDefaults && len(b.TrendRegime) > 0 {
		// Preserve operator intent: render use_defaults instead of the
		// expanded baseline so reload-diffs don't churn on equivalent
		// configs.
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

// ResolveSurface validates and parses the captured raw JSON against the
// given surface (which controls baseline expansion + close_fraction
// allowance). Returns error strings for LoadConfig to surface; on success,
// populates UseDefaults/TrendRegime.
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

// EqualForReload reports whether two blocks have the same reload representation.
// The raw JSON shape is informational only; use_defaults provenance is retained
// so flat/effectively-safe reloads still copy the operator's latest form.
func (b *RegimeATRBlock) EqualForReload(other *RegimeATRBlock) bool {
	if !b.EqualEffectiveForReload(other) {
		return false
	}
	if b == nil || b.IsZero() {
		return true
	}
	return b.UseDefaults == other.UseDefaults
}

// EqualEffectiveForReload reports whether two blocks resolve to the same runtime
// ATR map for hot-reload state-compat checks. Representation-only edits such as
// explicit defaults -> use_defaults do not alter already-armed triggers.
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

// IsZero reports whether the block was omitted from config entirely
// (distinct from an explicit use_defaults expansion). Designed for runtime
// callers that have already gone through ResolveSurface (which populates
// UseDefaults/TrendRegime from raw). Callers that may run BEFORE
// ResolveSurface (e.g. LoadConfig's defaults loop, which runs before
// ValidateConfig) MUST use IsConfigured instead — IsZero returns true on
// a freshly-unmarshaled block that has captured raw JSON but not yet been
// resolved, which would mis-apply scalar auto-defaults on top of an
// operator's explicit regime config.
func (b RegimeATRBlock) IsZero() bool {
	return !b.UseDefaults && len(b.TrendRegime) == 0
}

// IsConfigured reports whether the operator explicitly supplied a value
// for this block in config — either through use_defaults or an explicit
// trend_regime map. Safe to call BEFORE ResolveSurface runs: relies on
// the raw shape captured at unmarshal time. Use this when deciding
// whether scalar auto-defaults should be applied, since `IsZero()`
// returns true on an unresolved-but-raw-populated block (review #735.1).
func (b *RegimeATRBlock) IsConfigured() bool {
	if b == nil {
		return false
	}
	if b.UseDefaults || len(b.TrendRegime) > 0 {
		return true
	}
	return len(b.raw) > 0
}

// Resolve returns the per-label entry for the given regime. The caller is
// responsible for validating the block at config-load time so this can
// assume label presence. Returns (entry, true) on hit, (zero, false) on miss.
//
// #1124: a `ranging_directional_up`/`_down` regime stamp falls back to the bare
// `ranging_directional` entry when the block doesn't carry an explicit sub-label
// key (the back-compat shape — bare label covers the whole directional family).
// Exact match wins first, so an explicit sub key always overrides the bare
// entry. This keeps runtime resolution aligned with the exhaustiveness rule: a
// bare-only block is exhaustive, so it must also resolve at runtime.
func (b RegimeATRBlock) Resolve(regime string) (RegimeATREntry, bool) {
	if b.TrendRegime == nil {
		return RegimeATREntry{}, false
	}
	r := strings.TrimSpace(regime)
	if entry, ok := b.TrendRegime[r]; ok {
		return entry, true
	}
	// #1124: sub-label stamp falls back to the bare ranging_directional entry.
	if regimeDirectionalSubs[r] {
		if entry, ok := b.TrendRegime[regimeDirectionalBare]; ok {
			return entry, true
		}
	}
	return RegimeATREntry{}, false
}

// regimeATRDefaults holds the per-surface baseline expansions for
// `use_defaults: true`. Mirrors the table in issue #733.
var regimeATRDefaults = struct {
	StopLoss map[string]RegimeATREntry
	Trailing map[string]RegimeATREntry
}{
	StopLoss: map[string]RegimeATREntry{
		"trending_up":   {ATR: 2.0},
		"trending_down": {ATR: 2.0},
		"ranging":       {ATR: 1.5},
	},
	// #870: the regime ratchet/trailing opening trail. ADX labels keep their
	// pre-#870 values; composite labels resolve per quality group (clean=2.0,
	// choppy=2.0, ranging=1.0) so a trailing_stop_atr_regime use_defaults block
	// differentiates trend groups from ranges (tight).
	Trailing: map[string]RegimeATREntry{
		"trending_up":          {ATR: 2.5},
		"trending_down":        {ATR: 2.5},
		"ranging":              {ATR: 2.0},
		"trending_up_clean":    {ATR: 2.0},
		"trending_down_clean":  {ATR: 2.0},
		"trending_up_choppy":   {ATR: 2.0},
		"trending_down_choppy": {ATR: 2.0},
		"ranging_quiet":        {ATR: 1.0},
		"ranging_volatile":     {ATR: 1.0},
		"ranging_directional":  {ATR: 1.0},
		// #1124: directional-drift substates inherit the tight ranging_directional
		// opening trail (1.0). Explicit entries keep parity with the bare label —
		// without them mapRegimeToBaselineFamily would fall the _up/_down labels
		// back to the wider "ranging" family (2.0), silently loosening the trail.
		"ranging_directional_up":   {ATR: 1.0},
		"ranging_directional_down": {ATR: 1.0},
	},
}

// regimeCloseDefaultGroup classifies a classifier label into one of three
// default-ladder quality groups (#870). Composite quality suffixes win; bare
// ADX trends fall to choppy (ADX exposes no clean/choppy signal); the
// ranging-family (ADX `ranging` + composite `ranging_*`) maps to ranging.
// Shared by the regime ATR-TP defaults (B2) and the regime ratchet defaults
// (C2) so both differentiate identically.
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

// regimeTPTierGroupDefaults is the per-group default ATR take-profit ladder for
// the regime ATR-TP evaluators (#870 B2). Tier counts are ragged by design:
// clean lets trends run (4 patient rungs), choppy mirrors the scalar default (3
// rungs), ranging scales out fast (2 rungs). Cumulative close fractions; the
// final rung is coerced to 1.0 by finalizeProtectionTiers.
var regimeTPTierGroupDefaults = map[string][]hlProtectionTier{
	"clean":   {{Multiple: 2.5, Fraction: 0.25}, {Multiple: 4.0, Fraction: 0.50}, {Multiple: 5.5, Fraction: 0.75}, {Multiple: 7.0, Fraction: 1.00}},
	"choppy":  {{Multiple: 1.5, Fraction: 0.40}, {Multiple: 3.0, Fraction: 0.80}, {Multiple: 5.0, Fraction: 1.00}},
	"ranging": {{Multiple: 0.5, Fraction: 0.50}, {Multiple: 1.0, Fraction: 1.00}},
}

// regimeTPFleetDefaultLabelsByGroup is the full classifier vocabulary (ADX +
// composite) grouped for inspect-time fleet-default rendering (#870).
var regimeTPFleetDefaultLabelsByGroup = map[string][]string{
	"clean":   {"trending_up_clean", "trending_down_clean"},
	"choppy":  {"trending_up", "trending_down", "trending_up_choppy", "trending_down_choppy"},
	"ranging": {"ranging", "ranging_quiet", "ranging_volatile", "ranging_directional", "ranging_directional_up", "ranging_directional_down"},
}

// defaultRegimeBlockForSurface returns the baseline trend_regime map for a
// non-tier surface. Tier defaults live on regimeATRDefaults.TPTiers and are
// resolved differently because tiers are an ordered list.
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

// parseRegimeATRBlock validates and parses the raw map[string]interface{}
// JSON shape into a RegimeATRBlock. ctxLabel is prefixed onto error messages
// so callers (LoadConfig, tier parser) can scope the failures.
//
// Returns (block, errs). Errors are returned as a slice so the parser can
// report multiple problems at once instead of stopping on the first; callers
// should treat any non-empty slice as a config-load failure.
//
// surface controls which baseline expansion applies for `use_defaults: true`
// (tier surfaces handle their own expansion since tiers are an ordered list,
// so passing a tier surface here returns a zero block — the caller must
// special-case tier-level use_defaults before reaching this function).
func labelsAreCanonicalADX(labels []string) bool {
	if len(labels) != len(canonicalTrendRegimeLabels) {
		return false
	}
	set := make(map[string]bool, len(labels))
	for _, l := range labels {
		set[l] = true
	}
	for _, l := range canonicalTrendRegimeLabels {
		if !set[l] {
			return false
		}
	}
	return true
}

// mapRegimeToBaselineFamily resolves a single regime label to its ADX-family
// baseline entry. Exact ADX matches win first; composite labels fall back to
// their trend family by prefix (trending_up_* → trending_up, trending_down_*
// → trending_down, ranging_* → ranging). Returns (zero, false) for an
// unrecognized label so callers can fail closed. Shared by the use_defaults
// label expansion and the runtime default-tier resolver so composite labels
// resolve to the right baseline at both config-load and runtime.
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
	// Always filter to exactly the requested labels. The baseline may carry
	// extra keys (#870 added composite opening-trail entries to Trailing), so a
	// blanket clone would leak composite keys into an ADX strategy's block.
	// mapRegimeToBaselineFamily exact-matches a present label and otherwise
	// falls back to the ADX family, so ADX vocab still resolves to its 3 entries.
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
	// #1124: the ranging_directional family — bare `ranging_directional` plus
	// its `ranging_directional_up`/`_down` sub-labels — is satisfied for
	// exhaustiveness when the bare label is present (it covers all three at
	// runtime via Resolve's bare fallback, including the return_eff==0 neutral
	// case the producer still emits). Providing only the sub-labels without the
	// bare label is NOT exhaustive (the neutral case would resolve to nil →
	// silent never-arm of an auto-protective exit). So a present bare label
	// covers its sub-labels, and a missing bare label is flagged even when both
	// sub-labels exist.
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
		// sl_after atr_offset accepts signed atr (zero = breakeven, negative
		// = SL behind entry). Every other surface requires a strictly
		// positive magnitude. See #736.
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

// regimeEntryATRRaw reads the canonical "atr_multiple" trigger from a per-regime
// entry. Setting both atr_multiple and the legacy "atr" alias is rejected.
// Returns (raw, present, err). #841.
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

// resolveRegimeATR is the single resolution entry point used by all live and
// backtest code that needs a regime-aware ATR multiplier. Returns (mult, ok).
// ok=false when the block is zero or the regime label is missing — callers
// must already have validated the block at load time, so a missing label
// in practice means the runtime regime classifier produced an unexpected
// value (e.g. detection disabled mid-position); the live caller should
// fall back to its scalar sibling (which validation has already ruled
// out — see regimeFieldConflictsWithScalar).
func resolveRegimeATR(block RegimeATRBlock, regime string) (float64, bool) {
	entry, ok := block.Resolve(regime)
	if !ok || entry.ATR <= 0 {
		return 0, false
	}
	return entry.ATR, true
}

// regimeTierSpec is the parsed form of one tier inside a
// tiered_tp_atr_regime / tiered_tp_atr_live_regime close ref. The block is
// the per-regime ATR/close_fraction map; tierCloseFraction is the tier-level
// scalar shape (mutually exclusive with per-regime close_fraction — the
// parser enforces "pick one shape per tier" at config load).
type regimeTierSpec struct {
	Block                RegimeATRBlock
	TierCloseFraction    float64
	HasTierCloseFraction bool
}

// parseRegimeTPTiers parses the raw tier list from a tiered_tp_atr_regime
// close ref's params["tiers"]. The list shape is identical to the scalar
// tiered_tp_atr tier list except each tier carries a `trend_regime` block
// instead of a flat `atr_multiple`. close_fraction may live per-regime or
// at the tier level (never both within one tier).
//
// errs is non-empty when the operator submitted malformed tier shapes;
// LoadConfig surfaces them as config-validation errors so a typo can't
// silently disable the TP plan.
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
		// Detect per-regime close_fraction shape vs tier-level scalar.
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
		// Pass the tier object minus close_fraction and sl_after so the inner
		// allowlist only sees use_defaults / trend_regime. Both are legitimate
		// sibling keys handled elsewhere — close_fraction by the tier-fraction
		// logic below, sl_after by parseRegimeStrategyTPSLAfterRules — and must
		// not trip the ATR-block unknown-key check. Regression: without stripping
		// sl_after, a per-tier sl_after on a regime close (e.g. tp_atr_fraction)
		// failed config-load AND silently never armed at fire time, since the
		// fire-path tier-multiple resolution re-parses through here.
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

// regimeLabelsFromTierRaw collects the union of trend_regime label keys present
// across a tier list's raw JSON. The runtime tier resolver uses this so a
// re-parse accepts whatever vocabulary the operator configured (composite or
// ADX) without needing the RegimeConfig in scope; config-load validation has
// already enforced exhaustiveness against the window classifier. Falls back to
// the canonical ADX labels when no per-regime keys are present.
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

// resolveRegimeTPTiers takes the raw tier list and a runtime regime label,
// returning the concrete (multiple, fraction) list. Returns nil when the
// regime is unknown or the tier specs fail to resolve — the caller falls
// back to placing only the SL this cycle.
func resolveRegimeTPTiers(raw interface{}, regime string) []hlProtectionTier {
	// Re-parse against the vocabulary actually present in the config (already
	// validated exhaustively against the strategy's regime_atr_window classifier
	// at config load). This keeps the runtime resolver classifier-agnostic — it
	// does not need the RegimeConfig in scope to support composite labels.
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

// validateRegimeATRConfig runs the full surface-aware parsing pass over every
// strategy's regime blocks and accumulates errors with strategy-scoped
// prefixes. Mutex with scalar siblings + regime-enabled requirement are
// also enforced here. Called from ValidateConfig before the runtime-only
// post-validation pass; on success the strategies' RegimeATRBlock fields
// carry typed UseDefaults / TrendRegime values for runtime resolution.
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

		// Resolve the per-regime ATR vocabulary from the strategy's
		// regime_atr_window classifier (composite → 7 labels, ADX → 3). Falls
		// back to the canonical ADX labels when regime detection is off (the
		// regime-enabled requirement below then fails the load anyway). This is
		// the authoritative resolution pass that populates the typed
		// UseDefaults/TrendRegime fields for runtime — see #802.
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

		if sc.StopLossATRRegime != nil {
			sub := sc.StopLossATRRegime.ResolveSurfaceWithLabels(prefix+".stop_loss_atr_regime", regimeSurfaceStopLoss, atrLabels)
			for _, e := range sub {
				errs = append(errs, wrapATR(e))
			}
			if len(sub) == 0 && !sc.StopLossATRRegime.IsZero() {
				usesRegime = true
				// Mutex with scalar siblings.
				if sc.StopLossATRMult != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_regime is mutually exclusive with stop_loss_atr_mult", prefix))
				}
				if sc.StopLossPct != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_regime is mutually exclusive with stop_loss_pct", prefix))
				}
				if sc.StopLossMarginPct != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_regime is mutually exclusive with stop_loss_margin_pct", prefix))
				}
				if sc.TrailingStopPct != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_regime is mutually exclusive with trailing_stop_pct", prefix))
				}
				if sc.TrailingStopATRMult != nil {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_regime is mutually exclusive with trailing_stop_atr_mult", prefix))
				}
				// #1111: IsConfigured (raw-aware), NOT !IsZero() — the trailing block
				// is resolved further below in the `if sc.TrailingStopATRRegime != nil`
				// branch, so here it is still unresolved and IsZero() would report it
				// absent, silently skipping this (sole) mutex check when both regime
				// stops are set.
				if sc.TrailingStopATRRegime.IsConfigured() {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_regime is mutually exclusive with trailing_stop_atr_regime", prefix))
				}
				if sc.Platform != "hyperliquid" || sc.Type != "perps" {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_regime is HL perps only", prefix))
				}
			}
		}
		if sc.TrailingStopATRRegime != nil {
			sub := sc.TrailingStopATRRegime.ResolveSurfaceWithLabels(prefix+".trailing_stop_atr_regime", regimeSurfaceTrailing, atrLabels)
			for _, e := range sub {
				errs = append(errs, wrapATR(e))
			}
			if len(sub) == 0 && !sc.TrailingStopATRRegime.IsZero() {
				usesRegime = true
				if sc.TrailingStopATRMult != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_regime is mutually exclusive with trailing_stop_atr_mult", prefix))
				}
				if sc.TrailingStopPct != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_regime is mutually exclusive with trailing_stop_pct", prefix))
				}
				if sc.StopLossPct != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_regime is mutually exclusive with stop_loss_pct", prefix))
				}
				if sc.StopLossMarginPct != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_regime is mutually exclusive with stop_loss_margin_pct", prefix))
				}
				if sc.StopLossATRMult != nil {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_regime is mutually exclusive with stop_loss_atr_mult", prefix))
				}
				// #870: HL perps, plus HL manual when it owns a regime ratchet
				// (trailing_tp_ratchet_regime's per-regime opening trail / SL).
				manualRatchet := sc.Type == "manual" && strategyUsesTrailingTPRatchetClose(*sc)
				if sc.Platform != "hyperliquid" || (sc.Type != "perps" && !manualRatchet) {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_regime is HL perps only (or HL manual trailing_tp_ratchet_regime)", prefix))
				}
			}
		}

		// Close-ref tier validation: walk each regime-aware tiered_tp_atr_regime /
		// tiered_tp_atr_live_regime close ref and parse its tier list shape.
		// Errors are surfaced here so a typo in tier shapes can't silently
		// disable on-chain TPs.
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
			// #841 2b: unified per-regime block — validate the top-level
			// trend_regime shape and skip the legacy tier-keyed checks.
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
			// Unknown-key check for the close ref params (use_defaults, tiers,
			// atr_source, sl_after).
			for k := range ref.Params {
				switch k {
				case "use_defaults", "tp_tiers", "tiers", "atr_source", "sl_after":
					// known
				default:
					errs = append(errs, fmt.Sprintf("%s: unknown param %q (allowed: use_defaults, tp_tiers, atr_source, sl_after)", subPrefix, k))
				}
			}
			if useDefaults {
				continue // baseline tier list is validated at resolveDefaultRegimeTPTiers call sites
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

// defaultRegimeTPTiersForRegime expands the per-surface default tier list
// for a regime-aware close evaluator with `use_defaults: true`. Returns nil
// when regime is empty so the caller emits only the SL until the position
// regime is stamped.
func defaultRegimeTPTiersForRegime(regime string) []hlProtectionTier {
	if regime == "" {
		return nil
	}
	// #870: resolve the per-quality-group ladder (clean/choppy/ranging) rather
	// than a single positional baseline, so tier counts differ per group.
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

// InspectRegimeTPFleetDefaultBlocks returns deep copies of the fleet baseline
// tier blocks used when a tiered_tp_atr{_live}_regime close ref sets
// use_defaults:true. For go-trader inspect provenance (#738). #870: the
// per-group ladders are ragged, so this renders the positional union across the
// full classifier vocabulary — block[i] only carries the labels whose group
// defines tier i (e.g. only clean labels appear in the 4th block).
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
