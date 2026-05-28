package main

// Regime-aware directional policy (#779).
//
// Lets a single HL perps strategy switch between long/short/inverse modes
// automatically as the market regime changes, without operator hot-edits.
// Pairs with #775's `invert_signal` so an "inverse short in trending_down,
// plain long otherwise" config is encoded once and applied deterministically.
//
// Shape (all three canonical labels required):
//
//	"regime_directional_policy": {
//	  "trend_regime": {
//	    "trending_down": { "direction": "short", "invert_signal": true },
//	    "trending_up":   { "direction": "long",  "invert_signal": false },
//	    "ranging":       { "direction": "long",  "invert_signal": false }
//	  }
//	}
//
// Resolver semantics (applied inside runHyperliquidCheck after the script
// returns and result.Regime is known):
//
//   - When flat (posQty == 0): resolve from result.Regime (current cycle).
//   - When a position is open (posQty > 0): resolve from pos.Regime —
//     positions inherit the policy they were opened under and run to their
//     natural exit (SL/TP/close evaluator). New entries opposite to the
//     held side never fire because PerpsOrderSkipReason / perpsLiveOrderSize
//     both gate on the resolved Direction; close evaluators always run.
//
// HL perps only (mirrors invert_signal's surface, validated in config.go).
// Static StrategyConfig.Direction / InvertSignal remain the BASE config —
// used when this block is absent. When present, the policy overrides both
// fields per-regime.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// regimeDirectionalLegacyWarned tracks per-strategy one-shot warnings for the
// "open position with empty pos.Regime" fallback path (positions opened before
// regime stamping landed in #741). Keyed by strategy ID; cleared on process
// restart. Warning is informational — the policy still resolves under the
// current cycle's regime — but hold-on-transition isn't guaranteed for that
// position. Self-heals once the position closes and the next entry stamps
// regime at open.
var regimeDirectionalLegacyWarned sync.Map

// RegimeDirectionalEntry is the per-regime override pair.
type RegimeDirectionalEntry struct {
	Direction    string `json:"direction"`
	InvertSignal bool   `json:"invert_signal"`
}

// RegimeDirectionalPolicy wraps the trend_regime map. The wrapper key
// matches RegimeATRBlock (regimeClassifierKey) so future classifiers
// (e.g. vol_regime) can compose alongside the trend_regime block.
type RegimeDirectionalPolicy struct {
	TrendRegime map[string]RegimeDirectionalEntry
	raw         map[string]interface{}
}

// UnmarshalJSON captures the raw shape for later strategy-scoped validation
// in LoadConfig — mirrors RegimeATRBlock's deferred-resolve pattern so
// error messages can name the offending strategy ID.
func (p *RegimeDirectionalPolicy) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("regime_directional_policy: %w", err)
	}
	p.raw = raw
	return nil
}

// MarshalJSON renders the canonical form for hot-reload diff logging and
// `go-trader inspect`. Defined on the value receiver so encoding/json picks
// it up whether the field is a pointer or value.
func (p RegimeDirectionalPolicy) MarshalJSON() ([]byte, error) {
	if len(p.TrendRegime) == 0 {
		return json.Marshal(p.raw)
	}
	inner := map[string]RegimeDirectionalEntry{}
	for k, v := range p.TrendRegime {
		inner[k] = v
	}
	return json.Marshal(map[string]map[string]RegimeDirectionalEntry{regimeClassifierKey: inner})
}

// Resolve runs after ResolveRaw has populated TrendRegime. Validation
// guarantees the lookup hits when regime is one of the canonical labels;
// fallback returns (zero, false) which callers translate to "use static
// base Direction/InvertSignal".
func (p *RegimeDirectionalPolicy) Resolve(regime string) (RegimeDirectionalEntry, bool) {
	if p == nil || len(p.TrendRegime) == 0 {
		return RegimeDirectionalEntry{}, false
	}
	entry, ok := p.TrendRegime[strings.TrimSpace(regime)]
	return entry, ok
}

// IsConfigured reports whether the operator supplied any value. Safe to
// call before ResolveRaw (relies on captured raw, parallels
// RegimeATRBlock.IsConfigured).
func (p *RegimeDirectionalPolicy) IsConfigured() bool {
	if p == nil {
		return false
	}
	if len(p.TrendRegime) > 0 {
		return true
	}
	return len(p.raw) > 0
}

// IsZero reports whether the block is empty after resolution. Pointer
// receiver so calls on a nil *RegimeDirectionalPolicy don't panic.
func (p *RegimeDirectionalPolicy) IsZero() bool {
	if p == nil {
		return true
	}
	return len(p.TrendRegime) == 0
}

// EqualForReload reports shape equality for hot-reload state-compat
// (parallels RegimeATRBlock.EqualForReload).
func (p *RegimeDirectionalPolicy) EqualForReload(other *RegimeDirectionalPolicy) bool {
	aZero := p == nil || p.IsZero()
	bZero := other == nil || other.IsZero()
	if aZero != bZero {
		return false
	}
	if aZero {
		return true
	}
	if len(p.TrendRegime) != len(other.TrendRegime) {
		return false
	}
	for k, va := range p.TrendRegime {
		vb, ok := other.TrendRegime[k]
		if !ok {
			return false
		}
		if va.Direction != vb.Direction || va.InvertSignal != vb.InvertSignal {
			return false
		}
	}
	return true
}

// ResolveRaw parses the captured raw JSON into the typed TrendRegime map.
// Called from LoadConfig with strategy-scoped errors. The validation
// enforces:
//   - top-level shape: { "trend_regime": { <label>: { direction, invert_signal } } }
//   - every canonical label (trending_up, trending_down, ranging) present
//   - direction is one of "long" / "short" / "both"
//   - invert_signal is bool (json default false when omitted)
//
// Once resolved, TrendRegime is the runtime source of truth and raw is
// retained only for MarshalJSON re-rendering.
func (p *RegimeDirectionalPolicy) ResolveRaw(label string) []string {
	return p.ResolveRawWithLabels(label, canonicalTrendRegimeLabels)
}

func (p *RegimeDirectionalPolicy) ResolveRawWithLabels(label string, labels []string) []string {
	var errs []string
	if p == nil {
		return errs
	}
	if len(p.raw) == 0 {
		return errs
	}
	classifierRaw, ok := p.raw[regimeClassifierKey]
	if !ok {
		errs = append(errs, fmt.Sprintf("%s: missing required %q wrapper key", label, regimeClassifierKey))
		return errs
	}
	classifier, ok := classifierRaw.(map[string]interface{})
	if !ok {
		errs = append(errs, fmt.Sprintf("%s: %q must be an object", label, regimeClassifierKey))
		return errs
	}
	parsed := make(map[string]RegimeDirectionalEntry, len(classifier))
	// `seen` records every regime label the operator actually wrote in the
	// config (even if its body failed validation). The canonical-label
	// "missing required" check below consults `seen`, not `parsed`, so a
	// single typo (e.g. direction="sideways") fires one error against the
	// offending key instead of also being double-reported as "missing".
	seen := make(map[string]bool, len(classifier))
	keys := make([]string, 0, len(classifier))
	for k := range classifier {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, regimeLabel := range keys {
		seen[regimeLabel] = true
		entryRaw := classifier[regimeLabel]
		entryMap, ok := entryRaw.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.%s: must be an object with {direction, invert_signal}", label, regimeLabel))
			continue
		}
		// Direction is required.
		dirRaw, hasDir := entryMap["direction"]
		if !hasDir {
			errs = append(errs, fmt.Sprintf("%s.%s: missing required key %q", label, regimeLabel, "direction"))
			continue
		}
		dir, ok := dirRaw.(string)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.%s.direction: must be a string", label, regimeLabel))
			continue
		}
		switch dir {
		case DirectionLong, DirectionShort, DirectionBoth:
			// valid
		default:
			errs = append(errs, fmt.Sprintf("%s.%s.direction: must be %q, %q, or %q (got %q)", label, regimeLabel, DirectionLong, DirectionShort, DirectionBoth, dir))
			continue
		}
		// invert_signal is optional (defaults to false).
		invert := false
		if invRaw, hasInv := entryMap["invert_signal"]; hasInv {
			b, ok := invRaw.(bool)
			if !ok {
				errs = append(errs, fmt.Sprintf("%s.%s.invert_signal: must be a boolean", label, regimeLabel))
				continue
			}
			invert = b
		}
		// Reject unknown keys to catch typos early.
		for k := range entryMap {
			if k != "direction" && k != "invert_signal" {
				errs = append(errs, fmt.Sprintf("%s.%s: unknown key %q (valid: direction, invert_signal)", label, regimeLabel, k))
			}
		}
		parsed[regimeLabel] = RegimeDirectionalEntry{Direction: dir, InvertSignal: invert}
	}
	if len(labels) == 0 {
		labels = canonicalTrendRegimeLabels
	}
	// Reject unknown regime labels.
	validLabels := map[string]bool{}
	for _, l := range labels {
		validLabels[l] = true
	}
	for _, k := range keys {
		if !validLabels[k] {
			errs = append(errs, fmt.Sprintf("%s: unknown regime label %q (valid: %s)", label, k, strings.Join(labels, ", ")))
		}
	}
	// Require all canonical labels present so there's never an undefined
	// fallback at runtime. Operators who want the static config to apply
	// for one regime can spell it out explicitly. Uses `seen` (not `parsed`)
	// so a label that's present-but-invalid isn't also reported as missing.
	missing := []string{}
	for _, l := range labels {
		if !seen[l] {
			missing = append(missing, l)
		}
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Sprintf("%s: missing required regime labels: %s", label, strings.Join(missing, ", ")))
	}
	if len(errs) == 0 {
		p.TrendRegime = parsed
	}
	return errs
}

// effectiveRegimeForPolicy chooses which regime label to feed the resolver.
//   - When flat (posQty <= 0), use the current cycle's regime (the fresh
//     entry decision should reflect the new regime immediately).
//   - When a position is open (posQty > 0), use the regime stamped on the
//     position at open time. This implements the "hold until natural exit"
//     semantics: the policy that opened the position governs its lifecycle,
//     and the new regime's policy only takes effect once flat.
//
// When posRegime is empty (legacy position pre-dating regime stamping),
// fall back to the current cycle's regime so the policy still resolves.
func effectiveRegimeForPolicy(currentRegime, posRegime string, posQty float64) string {
	if posQty > 0 && strings.TrimSpace(posRegime) != "" {
		return posRegime
	}
	return currentRegime
}

// applyRegimeDirectionalPolicy resolves the per-regime override and mutates
// the local sc copy in place. Returns the effective entry actually used so
// /status and inspect can show it. When the strategy has no policy block
// (or no regime is available), sc is left untouched and changed=false.
//
// `legacyFallback` is true iff a position is open but pos.Regime is empty
// (pre-#741 position predating regime stamping). The resolver still resolves
// against the current regime so the policy doesn't no-op, but the
// hold-on-transition guarantee can't be honored for that position — the
// caller should emit a one-shot operator warning.
//
// Caller contract: pass a LOCAL copy of sc (the loop iteration variable),
// not a pointer into cfg.Strategies. The mutation propagates to all
// downstream uses in the same cycle (applySignalInversion, EffectiveDirection
// calls in execute paths, etc.) without persisting into shared config state.
func applyRegimeDirectionalPolicy(sc *StrategyConfig, currentRegime, posRegime string, posQty float64) (effective RegimeDirectionalEntry, applied bool, legacyFallback bool) {
	if sc == nil || sc.RegimeDirectionalPolicy.IsZero() {
		return RegimeDirectionalEntry{}, false, false
	}
	legacyFallback = posQty > 0 && strings.TrimSpace(posRegime) == ""
	regime := effectiveRegimeForPolicy(currentRegime, posRegime, posQty)
	entry, ok := sc.RegimeDirectionalPolicy.Resolve(regime)
	if !ok {
		// Regime unknown / not in policy map. Leave sc untouched so the
		// static base config applies. Validation guarantees all canonical
		// labels are present, so this only happens for empty/unknown
		// regimes (e.g. regime detection disabled).
		return RegimeDirectionalEntry{}, false, false
	}
	sc.Direction = entry.Direction
	sc.InvertSignal = entry.InvertSignal
	return entry, true, legacyFallback
}

// EffectiveDirectionForRegime returns the direction that applies for a single
// regime label, honoring regime_directional_policy when configured (#783).
func EffectiveDirectionForRegime(sc StrategyConfig, regime string) string {
	if sc.RegimeDirectionalPolicy != nil && !sc.RegimeDirectionalPolicy.IsZero() {
		if entry, ok := sc.RegimeDirectionalPolicy.Resolve(strings.TrimSpace(regime)); ok {
			return entry.Direction
		}
	}
	return EffectiveDirection(sc)
}

// EffectiveDirectionForPosition resolves direction for an open position using
// the same hold-on-transition semantics as applyRegimeDirectionalPolicy: when
// posQty > 0 and pos.Regime is stamped, that regime governs; otherwise the
// current cycle regime is used (empty at startup validation → base direction).
func EffectiveDirectionForPosition(sc StrategyConfig, currentRegime, posRegime string, posQty float64) string {
	regime := effectiveRegimeForPolicy(currentRegime, posRegime, posQty)
	return EffectiveDirectionForRegime(sc, regime)
}

// policyAllowsPositionSide reports whether posSide is permitted under at least
// one entry in regime_directional_policy. Used when pos.Regime is empty
// (legacy / pre-#741) so validation does not false-positive a side that some
// regime intentionally allows (#783). Iterates resolved TrendRegime entries
// only — no fallback to base direction for missing labels.
func policyAllowsPositionSide(sc StrategyConfig, posSide string) bool {
	if sc.RegimeDirectionalPolicy == nil || sc.RegimeDirectionalPolicy.IsZero() {
		return false
	}
	labels := make([]string, 0, len(sc.RegimeDirectionalPolicy.TrendRegime))
	for label := range sc.RegimeDirectionalPolicy.TrendRegime {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		entry := sc.RegimeDirectionalPolicy.TrendRegime[label]
		if !perpsPositionConflictsDirection(posSide, entry.Direction) {
			return true
		}
	}
	return false
}

// perpsPositionConflictsDirection reports whether an open position's side
// conflicts with a resolved effective direction ("both" never conflicts).
func perpsPositionConflictsDirection(posSide, effectiveDir string) bool {
	switch effectiveDir {
	case DirectionBoth:
		return false
	case DirectionLong:
		return posSide == "short"
	case DirectionShort:
		return posSide == "long"
	default:
		return false
	}
}
