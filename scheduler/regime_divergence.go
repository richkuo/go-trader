package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	onDivergenceTrustShort  = "trust_short"
	onDivergenceTrustMedium = "trust_medium"
	onDivergenceAlertOnly   = "alert_only"
)

type divergenceBias int

const (
	biasBullish divergenceBias = 1
	biasNeutral divergenceBias = 0
	biasBearish divergenceBias = -1
)

type DivergenceKind string

const (
	DivergenceNone DivergenceKind = "none"
	DivergenceSoft DivergenceKind = "soft"
	DivergenceHard DivergenceKind = "hard"
)

type DivergenceResult struct {
	Kind           DivergenceKind `json:"kind"`
	ShortLabel     string         `json:"short_label"`
	MediumLabel    string         `json:"medium_label"`
	OverrideDir    string         `json:"override_dir,omitempty"`
	TrustingWindow string         `json:"trusting_window,omitempty"`
}

func (r DivergenceResult) IsActive() bool {
	return r.Kind == DivergenceHard && r.OverrideDir != ""
}

type RegimeDivergenceState struct {
	Short             string `json:"short"`
	Medium            string `json:"medium"`
	Kind              string `json:"kind"`
	ResolvedDirection string `json:"resolved_direction,omitempty"`
	TrustingWindow    string `json:"trusting_window,omitempty"`
	CyclesActive      int    `json:"cycles_active"`
}

type RegimeWindowDivergence struct {
	ShortWindow  string `json:"short_window"`
	MediumWindow string `json:"medium_window"`
	OnDivergence string `json:"on_divergence"`
	raw          map[string]interface{}
}

func (d *RegimeWindowDivergence) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("regime_window_divergence: %w", err)
	}
	d.raw = raw
	return nil
}

func (d RegimeWindowDivergence) MarshalJSON() ([]byte, error) {
	if d.ShortWindow == "" && d.MediumWindow == "" && d.OnDivergence == "" {
		return json.Marshal(d.raw)
	}
	return json.Marshal(map[string]interface{}{
		"short_window":  d.ShortWindow,
		"medium_window": d.MediumWindow,
		"on_divergence": d.OnDivergence,
	})
}

func (d *RegimeWindowDivergence) IsConfigured() bool {
	if d == nil {
		return false
	}
	if d.ShortWindow != "" || d.MediumWindow != "" || d.OnDivergence != "" {
		return true
	}
	return len(d.raw) > 0
}

func (d *RegimeWindowDivergence) IsZero() bool {
	if d == nil {
		return true
	}
	return d.ShortWindow == "" && d.MediumWindow == "" && d.OnDivergence == ""
}

func (d *RegimeWindowDivergence) EqualForReload(other *RegimeWindowDivergence) bool {
	aZero := d == nil || d.IsZero()
	bZero := other == nil || other.IsZero()
	if aZero != bZero {
		return false
	}
	if aZero {
		return true
	}
	return d.ShortWindow == other.ShortWindow &&
		d.MediumWindow == other.MediumWindow &&
		d.OnDivergence == other.OnDivergence
}

func (d *RegimeWindowDivergence) ResolveRaw(label string) []string {
	var errs []string
	if d == nil || len(d.raw) == 0 {
		return errs
	}

	knownKeys := map[string]bool{
		"short_window":  true,
		"medium_window": true,
		"on_divergence": true,
	}
	for k := range d.raw {
		if !knownKeys[k] {
			errs = append(errs, fmt.Sprintf("%s: unknown key %q (valid: short_window, medium_window, on_divergence)", label, k))
		}
	}

	shortRaw, hasShort := d.raw["short_window"]
	if !hasShort {
		errs = append(errs, fmt.Sprintf("%s: missing required key %q", label, "short_window"))
	}
	mediumRaw, hasMedium := d.raw["medium_window"]
	if !hasMedium {
		errs = append(errs, fmt.Sprintf("%s: missing required key %q", label, "medium_window"))
	}
	onDivRaw, hasOnDiv := d.raw["on_divergence"]
	if !hasOnDiv {
		errs = append(errs, fmt.Sprintf("%s: missing required key %q", label, "on_divergence"))
	}

	if len(errs) > 0 {
		return errs
	}

	shortWin, ok := shortRaw.(string)
	if !ok || strings.TrimSpace(shortWin) == "" {
		errs = append(errs, fmt.Sprintf("%s.short_window: must be a non-empty string", label))
	}
	mediumWin, ok := mediumRaw.(string)
	if !ok || strings.TrimSpace(mediumWin) == "" {
		errs = append(errs, fmt.Sprintf("%s.medium_window: must be a non-empty string", label))
	}
	onDiv, ok := onDivRaw.(string)
	if !ok {
		errs = append(errs, fmt.Sprintf("%s.on_divergence: must be a string", label))
	} else {
		switch onDiv {
		case onDivergenceTrustShort, onDivergenceTrustMedium, onDivergenceAlertOnly:

		default:
			errs = append(errs, fmt.Sprintf("%s.on_divergence: must be %q, %q, or %q (got %q)",
				label, onDivergenceTrustShort, onDivergenceTrustMedium, onDivergenceAlertOnly, onDiv))
		}
	}

	if len(errs) > 0 {
		return errs
	}

	shortWin = strings.TrimSpace(shortWin)
	mediumWin = strings.TrimSpace(mediumWin)
	if normalizeRegimeWindowKey(shortWin) == normalizeRegimeWindowKey(mediumWin) {
		errs = append(errs, fmt.Sprintf("%s: short_window and medium_window must be different (got %q and %q)", label, shortWin, mediumWin))
		return errs
	}

	d.ShortWindow = shortWin
	d.MediumWindow = mediumWin
	d.OnDivergence = onDiv
	return errs
}

func regimeLabelBias(label string, snapReturnEff float64) divergenceBias {
	switch strings.TrimSpace(label) {
	case "trending_up", "trending_up_clean", "trending_up_choppy":
		return biasBullish
	case "trending_down", "trending_down_clean", "trending_down_choppy":
		return biasBearish
	case "ranging_directional_up":

		return biasBullish
	case "ranging_directional_down":
		return biasBearish
	case "ranging_directional":

		if snapReturnEff > 0 {
			return biasBullish
		}
		if snapReturnEff < 0 {
			return biasBearish
		}
		return biasNeutral
	default:

		return biasNeutral
	}
}

func biasDirection(b divergenceBias) string {
	switch b {
	case biasBullish:
		return DirectionLong
	case biasBearish:
		return DirectionShort
	default:
		return ""
	}
}

func classifyRegimeDivergence(shortLabel, mediumLabel string, shortReturnEff, mediumReturnEff float64, onDivergence string) DivergenceResult {
	result := DivergenceResult{
		ShortLabel:  shortLabel,
		MediumLabel: mediumLabel,
		Kind:        DivergenceNone,
	}

	shortBias := regimeLabelBias(shortLabel, shortReturnEff)
	mediumBias := regimeLabelBias(mediumLabel, mediumReturnEff)

	if shortBias == mediumBias {
		return result
	}

	if shortBias != biasNeutral && mediumBias != biasNeutral {
		result.Kind = DivergenceHard
	} else {
		result.Kind = DivergenceSoft
	}

	if result.Kind != DivergenceHard {
		return result
	}

	switch onDivergence {
	case onDivergenceTrustShort:
		result.OverrideDir = biasDirection(shortBias)
		result.TrustingWindow = "short"
	case onDivergenceTrustMedium:
		result.OverrideDir = biasDirection(mediumBias)
		result.TrustingWindow = "medium"
	case onDivergenceAlertOnly:

	}
	return result
}

func applyRegimeDivergenceOverride(sc *StrategyConfig, payload RegimePayload, rc *RegimeConfig, posQty float64) DivergenceResult {
	if sc == nil || sc.RegimeWindowDivergence.IsZero() {
		return DivergenceResult{Kind: DivergenceNone}
	}
	d := sc.RegimeWindowDivergence

	shortLabel := payload.Label(d.ShortWindow, rc)
	mediumLabel := payload.Label(d.MediumWindow, rc)

	var shortReturnEff, mediumReturnEff float64
	if snap, ok := payload.Windows[normalizeRegimeWindowKey(d.ShortWindow)]; ok {
		shortReturnEff = snap.Metrics["return_eff"]
	}
	if snap, ok := payload.Windows[normalizeRegimeWindowKey(d.MediumWindow)]; ok {
		mediumReturnEff = snap.Metrics["return_eff"]
	}

	result := classifyRegimeDivergence(shortLabel, mediumLabel, shortReturnEff, mediumReturnEff, d.OnDivergence)

	if result.IsActive() && posQty <= 0 {
		sc.Direction = result.OverrideDir
		sc.InvertSignal = false
	}

	return result
}

func updateStrategyDivergenceState(s *StrategyState, result DivergenceResult) {
	if s == nil {
		return
	}

	if result.Kind != DivergenceSoft && result.Kind != DivergenceHard {
		s.RegimeDivergence = nil
		return
	}
	prev := s.RegimeDivergence
	next := &RegimeDivergenceState{
		Short:             result.ShortLabel,
		Medium:            result.MediumLabel,
		Kind:              string(result.Kind),
		ResolvedDirection: result.OverrideDir,
		TrustingWindow:    result.TrustingWindow,
	}
	if prev != nil &&
		prev.Kind == next.Kind &&
		prev.ResolvedDirection == next.ResolvedDirection {
		next.CyclesActive = prev.CyclesActive + 1
	} else {
		next.CyclesActive = 1
	}
	s.RegimeDivergence = next
}

func formatDivergenceDMLine(ds *RegimeDivergenceState) string {
	if ds == nil || ds.Kind != string(DivergenceHard) || ds.ResolvedDirection == "" {
		return ""
	}
	trusting := ds.TrustingWindow
	if trusting == "" {
		trusting = "short"
	}
	return fmt.Sprintf("⚠ regime divergence: medium=%s short=%s (since %d cycles, trusting %s window → %s)",
		ds.Medium, ds.Short, ds.CyclesActive, trusting, ds.ResolvedDirection)
}
