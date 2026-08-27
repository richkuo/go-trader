package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

var regimeDirectionalLegacyWarned sync.Map

type RegimeDirectionalEntry struct {
	Direction    string `json:"direction"`
	InvertSignal bool   `json:"invert_signal"`
}

type RegimeDirectionalPolicy struct {
	TrendRegime map[string]RegimeDirectionalEntry
	raw         map[string]interface{}
}

func (p *RegimeDirectionalPolicy) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("regime_directional_policy: %w", err)
	}
	p.raw = raw
	return nil
}

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

func (p *RegimeDirectionalPolicy) Resolve(regime string) (RegimeDirectionalEntry, bool) {
	if p == nil || len(p.TrendRegime) == 0 {
		return RegimeDirectionalEntry{}, false
	}
	r := strings.TrimSpace(regime)
	if entry, ok := p.TrendRegime[r]; ok {
		return entry, true
	}

	if regimeDirectionalSubs[r] {
		if entry, ok := p.TrendRegime[regimeDirectionalBare]; ok {
			return entry, true
		}
	}
	return RegimeDirectionalEntry{}, false
}

func (p *RegimeDirectionalPolicy) IsConfigured() bool {
	if p == nil {
		return false
	}
	if len(p.TrendRegime) > 0 {
		return true
	}
	return len(p.raw) > 0
}

func (p *RegimeDirectionalPolicy) IsZero() bool {
	if p == nil {
		return true
	}
	return len(p.TrendRegime) == 0
}

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

		default:
			errs = append(errs, fmt.Sprintf("%s.%s.direction: must be %q, %q, or %q (got %q)", label, regimeLabel, DirectionLong, DirectionShort, DirectionBoth, dir))
			continue
		}

		invert := false
		if invRaw, hasInv := entryMap["invert_signal"]; hasInv {
			b, ok := invRaw.(bool)
			if !ok {
				errs = append(errs, fmt.Sprintf("%s.%s.invert_signal: must be a boolean", label, regimeLabel))
				continue
			}
			invert = b
		}

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

	validLabels := map[string]bool{}
	for _, l := range labels {
		validLabels[l] = true
	}
	for _, k := range keys {
		if !validLabels[k] {
			errs = append(errs, fmt.Sprintf("%s: unknown regime label %q (valid: %s)", label, k, strings.Join(labels, ", ")))
		}
	}

	bareDirectional := seen[regimeDirectionalBare]
	missing := []string{}
	for _, l := range labels {
		if seen[l] {
			continue
		}
		if regimeLabelFamilyCovered(l, bareDirectional) {
			continue
		}
		missing = append(missing, l)
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Sprintf("%s: missing required regime labels: %s", label, strings.Join(missing, ", ")))
	}
	if len(errs) == 0 {
		p.TrendRegime = parsed
	}
	return errs
}

func effectiveRegimeForPolicy(currentRegime, posRegime string, posQty float64) string {
	if posQty > 0 && strings.TrimSpace(posRegime) != "" {
		return posRegime
	}
	return currentRegime
}

func applyRegimeDirectionalPolicy(sc *StrategyConfig, currentRegime, posRegime string, posQty float64, certStates map[string]string) (effective RegimeDirectionalEntry, applied bool, legacyFallback bool) {
	if sc == nil || sc.RegimeDirectionalPolicy.IsZero() {
		return RegimeDirectionalEntry{}, false, false
	}
	legacyFallback = posQty > 0 && strings.TrimSpace(posRegime) == ""
	regime := effectiveRegimeForPolicy(currentRegime, posRegime, posQty)
	entry, honored := gatedDirectionalEntry(*sc, regime, certStates)
	if !honored {

		return RegimeDirectionalEntry{}, false, legacyFallback
	}
	sc.Direction = entry.Direction
	sc.InvertSignal = entry.InvertSignal
	return entry, true, legacyFallback
}

func gatedDirectionalEntry(sc StrategyConfig, regime string, certStates map[string]string) (RegimeDirectionalEntry, bool) {
	if sc.RegimeDirectionalPolicy == nil || sc.RegimeDirectionalPolicy.IsZero() {
		return RegimeDirectionalEntry{}, false
	}

	certDir, certOK := certStates[strings.TrimSpace(regime)]
	if !certOK {
		return RegimeDirectionalEntry{}, false
	}
	entry, ok := sc.RegimeDirectionalPolicy.Resolve(regime)
	if !ok {
		return RegimeDirectionalEntry{}, false
	}
	if entry.Direction != DirectionBoth && entry.Direction != certDir {
		return RegimeDirectionalEntry{}, false
	}
	return entry, true
}

func EffectiveDirectionForRegime(sc StrategyConfig, regime string) string {
	if sc.RegimeDirectionalPolicy != nil && !sc.RegimeDirectionalPolicy.IsZero() {
		if entry, ok := sc.RegimeDirectionalPolicy.Resolve(strings.TrimSpace(regime)); ok {
			return entry.Direction
		}
	}
	return EffectiveDirection(sc)
}

func EffectiveDirectionForRegimeGated(sc StrategyConfig, regime string, certStates map[string]string) string {
	if entry, honored := gatedDirectionalEntry(sc, regime, certStates); honored {
		return entry.Direction
	}
	return EffectiveDirection(sc)
}

func EffectiveDirectionForPositionGated(sc StrategyConfig, currentRegime, posRegime string, posQty float64, certStates map[string]string) string {
	regime := effectiveRegimeForPolicy(currentRegime, posRegime, posQty)
	return EffectiveDirectionForRegimeGated(sc, regime, certStates)
}

func EffectiveInvertSignalForPositionGated(sc StrategyConfig, currentRegime, posRegime string, posQty float64, certStates map[string]string) bool {
	regime := effectiveRegimeForPolicy(currentRegime, posRegime, posQty)
	if entry, honored := gatedDirectionalEntry(sc, regime, certStates); honored {
		return entry.InvertSignal
	}
	return sc.InvertSignal
}

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

func policyAllowsPositionSideGated(sc StrategyConfig, posSide string, certStates map[string]string) bool {
	if sc.RegimeDirectionalPolicy == nil || sc.RegimeDirectionalPolicy.IsZero() {
		return false
	}
	labels := make([]string, 0, len(sc.RegimeDirectionalPolicy.TrendRegime))
	for label := range sc.RegimeDirectionalPolicy.TrendRegime {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		entry, honored := gatedDirectionalEntry(sc, label, certStates)
		if !honored {
			continue
		}
		if !perpsPositionConflictsDirection(posSide, entry.Direction) {
			return true
		}
	}
	return false
}

type RegimeDirectionOrphanCloseJob struct {
	StrategyID    string
	Symbol        string
	CloseQty      float64
	CancelOIDs    []int64
	PosSide       string
	CurrentRegime string
	EffectiveDir  string
}

func regimeDirectionOrphanEffectiveDir(stratState *StrategyState, sc StrategyConfig, certStates map[string]string) string {
	current := strategyCurrentDirectionalRegime(stratState, sc)
	return EffectiveDirectionForRegimeGated(sc, current, certStates)
}

func perpsRegimeDirectionOrphanConflict(stratState *StrategyState, sc StrategyConfig, pos *Position) (conflict bool, currentRegime, effectiveDir string) {
	if stratState == nil || pos == nil || pos.Quantity <= 0 {
		return false, "", ""
	}
	if sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
		return false, "", ""
	}
	if pos.OwnerStrategyID != "" && pos.OwnerStrategyID != sc.ID {
		return false, "", ""
	}
	currentRegime = strategyCurrentDirectionalRegime(stratState, sc)
	effectiveDir = regimeDirectionOrphanEffectiveDir(stratState, sc, pos.DirectionCertifiedStatesAtOpen)
	if !perpsPositionConflictsDirection(pos.Side, effectiveDir) {
		return false, currentRegime, effectiveDir
	}
	return true, currentRegime, effectiveDir
}

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
