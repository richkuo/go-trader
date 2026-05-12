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
//	    "trending_up":   { "atr": 2.0, "close_fraction": 0.5 },
//	    "trending_down": { "atr": 2.0, "close_fraction": 0.5 },
//	    "ranging":       { "atr": 1.5, "close_fraction": 0.5 }
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

// regimeClassifierKey is the wrapper key required around per-label blocks.
// Reserves space for future classifiers (e.g. "vol_regime") to land as
// sibling keys without renaming.
const regimeClassifierKey = "trend_regime"

// regimeATRSurface enumerates the four call sites that consume a
// RegimeATRBlock. Determines which `use_defaults` baseline applies and
// whether `close_fraction` is permitted inside per-regime entries.
type regimeATRSurface int

const (
	regimeSurfaceStopLoss       regimeATRSurface = iota // stop_loss_atr_regime: ATR only
	regimeSurfaceTrailing                               // trailing_stop_atr_regime: ATR only
	regimeSurfaceTPTierATROnly                          // tier with tier-level scalar close_fraction: ATR only inside per-regime
	regimeSurfaceTPTierWithFrac                         // tier with per-regime close_fraction: ATR + close_fraction
	regimeSurfaceSLAfter                                // sl_after.{atr,trail_from_here.atr}: ATR only
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
		e := map[string]interface{}{"atr": entry.ATR}
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
	if b == nil {
		return nil
	}
	parsed, errs := parseRegimeATRBlock(b.raw, ctxLabel, surface)
	if len(errs) > 0 {
		return errs
	}
	b.UseDefaults = parsed.UseDefaults
	b.TrendRegime = parsed.TrendRegime
	return nil
}

// EqualForReload reports whether two blocks have the same shape for
// hot-reload state-compat purposes. Compares the resolved fields; the raw
// shape is informational only.
func (b *RegimeATRBlock) EqualForReload(other *RegimeATRBlock) bool {
	aZero := b == nil || b.IsZero()
	bZero := other == nil || other.IsZero()
	if aZero != bZero {
		return false
	}
	if aZero {
		return true
	}
	if b.UseDefaults != other.UseDefaults {
		return false
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
func (b RegimeATRBlock) Resolve(regime string) (RegimeATREntry, bool) {
	if b.TrendRegime == nil {
		return RegimeATREntry{}, false
	}
	entry, ok := b.TrendRegime[strings.TrimSpace(regime)]
	return entry, ok
}

// regimeATRDefaults holds the per-surface baseline expansions for
// `use_defaults: true`. Mirrors the table in issue #733.
var regimeATRDefaults = struct {
	StopLoss map[string]RegimeATREntry
	Trailing map[string]RegimeATREntry
	// TPTiers is a tier list — each entry is one tier's regime map. Tier
	// indices are positional and the final close_fraction is coerced to 1.0
	// by the consumer to match the live `strategyTPTiers` contract.
	TPTiers []RegimeATRBlock
}{
	StopLoss: map[string]RegimeATREntry{
		"trending_up":   {ATR: 2.0},
		"trending_down": {ATR: 2.0},
		"ranging":       {ATR: 1.5},
	},
	Trailing: map[string]RegimeATREntry{
		"trending_up":   {ATR: 2.5},
		"trending_down": {ATR: 2.5},
		"ranging":       {ATR: 2.0},
	},
	TPTiers: []RegimeATRBlock{
		{TrendRegime: map[string]RegimeATREntry{
			"trending_up":   {ATR: 2.0, CloseFraction: 0.5, HasCloseFrac: true},
			"trending_down": {ATR: 2.0, CloseFraction: 0.5, HasCloseFrac: true},
			"ranging":       {ATR: 1.5, CloseFraction: 0.5, HasCloseFrac: true},
		}},
		{TrendRegime: map[string]RegimeATREntry{
			"trending_up":   {ATR: 4.0, CloseFraction: 1.0, HasCloseFrac: true},
			"trending_down": {ATR: 4.0, CloseFraction: 1.0, HasCloseFrac: true},
			"ranging":       {ATR: 2.5, CloseFraction: 1.0, HasCloseFrac: true},
		}},
	},
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
func parseRegimeATRBlock(raw map[string]interface{}, ctxLabel string, surface regimeATRSurface) (RegimeATRBlock, []string) {
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
		return RegimeATRBlock{UseDefaults: true, TrendRegime: baseline}, errs
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

	validLabels := map[string]bool{}
	for _, l := range canonicalTrendRegimeLabels {
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
		errs = append(errs, fmt.Sprintf("%s.%s: unknown regime label %q (expected one of: %s)", ctxLabel, regimeClassifierKey, k, strings.Join(canonicalTrendRegimeLabels, ", ")))
	}

	missingLabels := []string{}
	for _, l := range canonicalTrendRegimeLabels {
		if _, ok := trendMap[l]; !ok {
			missingLabels = append(missingLabels, l)
		}
	}
	if len(missingLabels) > 0 {
		errs = append(errs, fmt.Sprintf("%s.%s: missing required regime labels: %s (must be exhaustive — no silent fallback)", ctxLabel, regimeClassifierKey, strings.Join(missingLabels, ", ")))
	}

	result := make(map[string]RegimeATREntry, len(canonicalTrendRegimeLabels))
	for _, label := range canonicalTrendRegimeLabels {
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
		allowedEntryKeys := map[string]bool{"atr": true}
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
				hint = " — close_fraction is only allowed inside close-evaluator tiers; for SL/trailing/sl_after surfaces, only atr is accepted"
			}
			errs = append(errs, fmt.Sprintf("%s.%s.%s: unknown key %q%s", ctxLabel, regimeClassifierKey, label, k, hint))
		}

		atrRaw, hasATR := entryMap["atr"]
		if !hasATR {
			errs = append(errs, fmt.Sprintf("%s.%s.%s: missing required %q", ctxLabel, regimeClassifierKey, label, "atr"))
			continue
		}
		atr, err := floatFromAnyChecked(atrRaw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s.%s.%s.atr: %v", ctxLabel, regimeClassifierKey, label, err))
			continue
		}
		if atr <= 0 {
			errs = append(errs, fmt.Sprintf("%s.%s.%s.atr: must be > 0, got %g", ctxLabel, regimeClassifierKey, label, atr))
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
func parseRegimeTPTiers(raw interface{}, ctxLabel string) ([]regimeTierSpec, []string) {
	var errs []string
	if raw == nil {
		return nil, errs
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
		// Pass the tier object minus close_fraction so the inner allowlist
		// only sees use_defaults / trend_regime.
		subset := make(map[string]interface{}, len(m))
		for k, v := range m {
			if k == "close_fraction" {
				continue
			}
			subset[k] = v
		}
		block, subErrs := parseRegimeATRBlock(subset, fmt.Sprintf("%s.tiers[%d]", ctxLabel, idx), surface)
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

// resolveRegimeTPTiers takes the raw tier list and a runtime regime label,
// returning the concrete (multiple, fraction) list. Returns nil when the
// regime is unknown or the tier specs fail to resolve — the caller falls
// back to placing only the SL this cycle.
func resolveRegimeTPTiers(raw interface{}, regime string) []hlProtectionTier {
	specs, errs := parseRegimeTPTiers(raw, "tiered_tp_atr_regime")
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

		if sc.StopLossATRRegime != nil {
			sub := sc.StopLossATRRegime.ResolveSurface(prefix+".stop_loss_atr_regime", regimeSurfaceStopLoss)
			errs = append(errs, sub...)
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
				if sc.TrailingStopATRRegime != nil && !sc.TrailingStopATRRegime.IsZero() {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_regime is mutually exclusive with trailing_stop_atr_regime", prefix))
				}
				if sc.Platform != "hyperliquid" || sc.Type != "perps" {
					errs = append(errs, fmt.Sprintf("%s: stop_loss_atr_regime is HL perps only", prefix))
				}
			}
		}
		if sc.TrailingStopATRRegime != nil {
			sub := sc.TrailingStopATRRegime.ResolveSurface(prefix+".trailing_stop_atr_regime", regimeSurfaceTrailing)
			errs = append(errs, sub...)
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
				if sc.Platform != "hyperliquid" || sc.Type != "perps" {
					errs = append(errs, fmt.Sprintf("%s: trailing_stop_atr_regime is HL perps only", prefix))
				}
			}
		}

		// Close-ref tier validation: walk each regime-aware tiered_tp_atr_regime /
		// tiered_tp_atr_live_regime close ref and parse its tier list shape.
		// Errors are surfaced here so a typo in tier shapes can't silently
		// disable on-chain TPs.
		for j, ref := range sc.CloseStrategies {
			name := strings.ToLower(strings.TrimSpace(ref.Name))
			if name != "tiered_tp_atr_regime" && name != "tiered_tp_atr_live_regime" {
				continue
			}
			usesRegime = true
			subPrefix := fmt.Sprintf("%s.close_strategies[%d](%s)", prefix, j, ref.Name)
			useDefaults := false
			if v, ok := ref.Params["use_defaults"].(bool); ok {
				useDefaults = v
			}
			tiersRaw, hasTiers := ref.Params["tiers"]
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
				case "use_defaults", "tiers", "atr_source", "sl_after":
					// known
				default:
					errs = append(errs, fmt.Sprintf("%s: unknown param %q (allowed: use_defaults, tiers, atr_source, sl_after)", subPrefix, k))
				}
			}
			if useDefaults {
				continue // baseline tier list is validated at resolveDefaultRegimeTPTiers call sites
			}
			if specs, subErrs := parseRegimeTPTiers(tiersRaw, subPrefix); len(subErrs) > 0 {
				errs = append(errs, subErrs...)
			} else if len(specs) < 2 {
				errs = append(errs, fmt.Sprintf("%s: must have at least 2 tiers, got %d", subPrefix, len(specs)))
			}
		}

		if usesRegime && !regimeEnabled {
			errs = append(errs, fmt.Sprintf("%s: regime-aware stop/TP fields require top-level regime.enabled=true", prefix))
		}
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
	out := make([]hlProtectionTier, 0, len(regimeATRDefaults.TPTiers))
	for _, block := range regimeATRDefaults.TPTiers {
		entry, ok := block.Resolve(regime)
		if !ok || entry.ATR <= 0 {
			return nil
		}
		frac := entry.CloseFraction
		if !entry.HasCloseFrac || frac <= 0 {
			return nil
		}
		out = append(out, hlProtectionTier{Multiple: entry.ATR, Fraction: frac})
	}
	return finalizeProtectionTiers(out)
}
