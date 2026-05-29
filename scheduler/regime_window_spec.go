package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	regimeClassifierADX       = "adx"
	regimeClassifierComposite = "composite"
)

// Default composite thresholds (#795); operators may override per window.
var defaultCompositeThresholds = RegimeCompositeThresholds{
	ReturnPct: 0.05,
	RangePct:  0.03,
	ADX:       25.0,
}

// RegimeCompositeThresholds tunes the 3-tuple mapper for classifier=composite.
type RegimeCompositeThresholds struct {
	ReturnPct float64 `json:"return_pct"`
	RangePct  float64 `json:"range_pct"`
	ADX       float64 `json:"adx"`
}

func (t RegimeCompositeThresholds) withDefaults() RegimeCompositeThresholds {
	out := defaultCompositeThresholds
	if t.ReturnPct > 0 {
		out.ReturnPct = t.ReturnPct
	}
	if t.RangePct > 0 {
		out.RangePct = t.RangePct
	}
	if t.ADX > 0 {
		out.ADX = t.ADX
	}
	return out
}

// RegimeWindowSpec describes one named regime window (#792/#795).
// Bare-int JSON in regime.windows parses as {classifier:"adx", period:N}.
type RegimeWindowSpec struct {
	Classifier   string                     `json:"classifier,omitempty"`
	Period       int                        `json:"period"`
	ADXThreshold float64                    `json:"adx_threshold,omitempty"`
	Thresholds   *RegimeCompositeThresholds `json:"thresholds,omitempty"`
}

func (s RegimeWindowSpec) effectiveClassifier() string {
	c := strings.TrimSpace(strings.ToLower(s.Classifier))
	if c == "" {
		return regimeClassifierADX
	}
	return c
}

func (s RegimeWindowSpec) adxThreshold(rc *RegimeConfig) float64 {
	if s.ADXThreshold > 0 {
		return s.ADXThreshold
	}
	if rc != nil && rc.ADXThreshold > 0 {
		return rc.ADXThreshold
	}
	return 20.0
}

func (s RegimeWindowSpec) compositeThresholds() RegimeCompositeThresholds {
	if s.Thresholds == nil {
		return defaultCompositeThresholds
	}
	return s.Thresholds.withDefaults()
}

func (s RegimeWindowSpec) resolvedForEmit(rc *RegimeConfig) RegimeWindowSpec {
	out := s
	out.Classifier = out.effectiveClassifier()
	if out.Period <= 0 && rc != nil && rc.Period > 0 {
		out.Period = rc.Period
	}
	if out.effectiveClassifier() == regimeClassifierADX {
		out.ADXThreshold = out.adxThreshold(rc)
	} else if out.effectiveClassifier() == regimeClassifierComposite {
		th := out.compositeThresholds()
		out.Thresholds = &th
	}
	return out
}

// RegimeWindowsMap is regime.windows: name -> spec. Accepts bare ints for ADX back-compat.
type RegimeWindowsMap map[string]RegimeWindowSpec

func (m *RegimeWindowsMap) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make(RegimeWindowsMap, len(raw))
	for name, blob := range raw {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return fmt.Errorf("regime.windows: window names must be non-empty")
		}
		var asInt int
		if err := json.Unmarshal(blob, &asInt); err == nil {
			out[trimmed] = RegimeWindowSpec{Classifier: regimeClassifierADX, Period: asInt}
			continue
		}
		var spec RegimeWindowSpec
		if err := json.Unmarshal(blob, &spec); err != nil {
			return fmt.Errorf("regime.windows[%q]: %w", trimmed, err)
		}
		out[trimmed] = spec
	}
	*m = out
	return nil
}

func regimeLabelsForClassifier(classifier string) []string {
	switch strings.TrimSpace(strings.ToLower(classifier)) {
	case regimeClassifierComposite:
		return []string{
			"trending_up_clean",
			"trending_up_choppy",
			"trending_down_clean",
			"trending_down_choppy",
			"ranging_quiet",
			"ranging_volatile",
			"ranging_directional",
		}
	default:
		return append([]string(nil), canonicalTrendRegimeLabels...)
	}
}

func regimeClassifierForWindow(rc *RegimeConfig, windowKey string) string {
	if rc == nil || len(rc.Windows) == 0 {
		return regimeClassifierADX
	}
	key := normalizeRegimeWindowKey(windowKey)
	if key == "" || key == regimeWindowDefaultKey {
		key = primaryRegimeWindowKey(rc)
	}
	for name, spec := range rc.Windows {
		if normalizeRegimeWindowKey(name) == key {
			return spec.effectiveClassifier()
		}
	}
	return regimeClassifierADX
}

func regimeLabelsForStrategyWindow(sc StrategyConfig, rc *RegimeConfig, field string) []string {
	key := resolveStrategyRegimeWindow(sc, field, rc)
	return regimeLabelsForClassifier(regimeClassifierForWindow(rc, key))
}

func validateRegimeWindowSpec(name string, spec RegimeWindowSpec, rc *RegimeConfig) []string {
	var errs []string
	prefix := fmt.Sprintf("regime.windows[%q]", name)
	if spec.Period < 2 {
		errs = append(errs, fmt.Sprintf("%s: period must be >= 2, got %d", prefix, spec.Period))
	}
	switch spec.effectiveClassifier() {
	case regimeClassifierADX:
		th := spec.adxThreshold(rc)
		if th <= 0 || th > 100 {
			errs = append(errs, fmt.Sprintf("%s: adx_threshold must be in (0, 100], got %g", prefix, th))
		}
	case regimeClassifierComposite:
		th := spec.compositeThresholds()
		if th.ReturnPct <= 0 {
			errs = append(errs, fmt.Sprintf("%s: thresholds.return_pct must be > 0", prefix))
		}
		if th.RangePct <= 0 {
			errs = append(errs, fmt.Sprintf("%s: thresholds.range_pct must be > 0", prefix))
		}
		if th.ADX <= 0 || th.ADX > 100 {
			errs = append(errs, fmt.Sprintf("%s: thresholds.adx must be in (0, 100], got %g", prefix, th.ADX))
		}
	default:
		errs = append(errs, fmt.Sprintf("%s: classifier must be %q or %q, got %q", prefix, regimeClassifierADX, regimeClassifierComposite, spec.Classifier))
	}
	return errs
}

func regimeWindowsSpecJSON(rc *RegimeConfig) string {
	if rc == nil || !rc.Enabled {
		return ""
	}
	ordered := make(map[string]RegimeWindowSpec)
	if len(rc.Windows) > 0 {
		for name, spec := range rc.Windows {
			ordered[name] = spec.resolvedForEmit(rc)
		}
	} else {
		period := rc.Period
		if period <= 0 {
			period = 14
		}
		ordered[regimeWindowDefaultKey] = RegimeWindowSpec{
			Classifier:   regimeClassifierADX,
			Period:       period,
			ADXThreshold: rc.ADXThreshold,
		}.resolvedForEmit(rc)
	}
	blob, err := json.Marshal(ordered)
	if err != nil {
		return ""
	}
	return string(blob)
}

func regimeWindowClassifierEqual(a, b RegimeWindowSpec) bool {
	return a.effectiveClassifier() == b.effectiveClassifier()
}

func regimeWindowsClassifiersEqual(old, new RegimeWindowsMap) bool {
	if len(old) != len(new) {
		return false
	}
	for name, ospec := range old {
		nspec, ok := new[name]
		if !ok {
			return false
		}
		if !regimeWindowClassifierEqual(ospec, nspec) {
			return false
		}
	}
	return true
}

func openPositionsReferenceRegimeWindow(state *AppState, windowKey string) bool {
	if state == nil || windowKey == "" {
		return false
	}
	key := normalizeRegimeWindowKey(windowKey)
	for _, strat := range state.Strategies {
		for _, pos := range strat.Positions {
			if pos == nil {
				continue
			}
			if len(pos.RegimeWindows) > 0 {
				if _, ok := pos.RegimeWindows[key]; ok {
					return true
				}
				for wname := range pos.RegimeWindows {
					if normalizeRegimeWindowKey(wname) == key {
						return true
					}
				}
			}
			if key == regimeWindowDefaultKey && pos.Regime != "" {
				return true
			}
		}
	}
	return false
}

func validateStrategyRegimeVocabulary(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	rc := cfg.Regime
	var errs []string
	for _, sc := range cfg.Strategies {
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)
		gateLabels := canonicalTrendRegimeLabels
		if rc != nil && rc.Enabled {
			gateLabels = regimeLabelsForStrategyWindow(sc, rc, "gate")
		}
		gateSet := make(map[string]bool, len(gateLabels))
		for _, l := range gateLabels {
			gateSet[l] = true
		}
		for j, label := range sc.AllowedRegimes {
			if !gateSet[label] {
				win := resolveStrategyRegimeWindow(sc, "gate", rc)
				cls := regimeClassifierForWindow(rc, win)
				errs = append(errs, fmt.Sprintf("%s: allowed_regimes[%d] label %q invalid for regime_gate_window %q (classifier %q; valid: %s)",
					prefix, j, label, win, cls, strings.Join(gateLabels, ", ")))
			}
		}
		if sc.RegimeDirectionalPolicy.IsConfigured() {
			dirLabels := canonicalTrendRegimeLabels
			if rc != nil && rc.Enabled {
				dirLabels = regimeLabelsForStrategyWindow(sc, rc, "directional")
			}
			polErrs := sc.RegimeDirectionalPolicy.ResolveRawWithLabels(prefix+".regime_directional_policy", dirLabels)
			errs = append(errs, polErrs...)
		}
		// stop_loss_atr_regime / trailing_stop_atr_regime vocabulary is resolved
		// authoritatively in validateRegimeATRConfig (which also populates the
		// typed runtime fields and runs the mutex checks) using the same
		// window-classifier labels — see #802. Re-resolving here would just
		// double-report the same label errors.
	}
	return errs
}

func formatRegimeWindowSpecInspect(name string, spec RegimeWindowSpec, rc *RegimeConfig) string {
	resolved := spec.resolvedForEmit(rc)
	cls := resolved.effectiveClassifier()
	if cls == regimeClassifierComposite && resolved.Thresholds != nil {
		th := resolved.Thresholds
		return fmt.Sprintf("%s: classifier=%s period=%d thresholds(return_pct=%g range_pct=%g adx=%g)",
			name, cls, resolved.Period, th.ReturnPct, th.RangePct, th.ADX)
	}
	return fmt.Sprintf("%s: classifier=%s period=%d adx_threshold=%g", name, cls, resolved.Period, resolved.ADXThreshold)
}

func formatStrategyRegimeDisplay(ss *StrategyState, rc *RegimeConfig) string {
	if ss == nil {
		return ""
	}
	if regimeMultiWindowEnabled(rc) && len(ss.RegimeWindows) > 0 {
		names := make([]string, 0, len(ss.RegimeWindows))
		for name := range ss.RegimeWindows {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			label := strings.TrimSpace(ss.RegimeWindows[name])
			if label == "" {
				continue
			}
			cls := regimeClassifierForWindow(rc, name)
			parts = append(parts, fmt.Sprintf("%s=%s [%s]", name, label, cls))
		}
		if len(parts) > 0 {
			return strings.Join(parts, "; ")
		}
	}
	return strings.TrimSpace(ss.Regime)
}

func formatRegimeWindowsInspectMap(windows RegimeWindowsMap, rc *RegimeConfig) string {
	if len(windows) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(windows))
	for name := range windows {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, formatRegimeWindowSpecInspect(name, windows[name], rc))
	}
	return strings.Join(parts, "; ")
}
