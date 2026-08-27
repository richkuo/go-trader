package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	HurstGateModeGate = "gate"

	HurstGateModeSize = "size"
)

const (
	HurstGateOnFailureOpen   = "open"
	HurstGateOnFailureClosed = "closed"
)

const (
	hurstGateStateUnknown  = ""
	hurstGateStateArmed    = "armed"
	hurstGateStateDisarmed = "disarmed"
)

const hurstGateMetricKey = "hurst"

const hurstSizeSpan = 0.15

const hurstDefaultSizeFloor = 0.25

type HurstGateConfig struct {
	Enabled bool `json:"enabled"`

	Mode string `json:"mode,omitempty"`

	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`

	DisarmMin *float64 `json:"disarm_min,omitempty"`
	DisarmMax *float64 `json:"disarm_max,omitempty"`

	WindowKey string `json:"window_key,omitempty"`

	OnFailure string `json:"on_failure,omitempty"`

	SizeFloor *float64 `json:"size_floor,omitempty"`
}

type HurstGateState struct {
	Key      string    `json:"key,omitempty"`
	State    string    `json:"state,omitempty"`
	LastH    float64   `json:"last_h,omitempty"`
	LastHAt  time.Time `json:"last_h_at,omitempty"`
	Observed bool      `json:"observed,omitempty"`
}

type HurstGateDecision struct {
	Active   bool
	Mode     string
	H        float64
	HKnown   bool
	State    string
	Holds    bool
	SizeMult float64
	Key      string
	Detail   string
}

func (d HurstGateDecision) OpenSizeMult() float64 {
	if !d.Active || d.Mode != HurstGateModeSize {
		return 1.0
	}
	if d.SizeMult <= 0 || d.SizeMult > 1.0 || math.IsNaN(d.SizeMult) {
		return 1.0
	}
	return d.SizeMult
}

func hurstGateConfigured(sc StrategyConfig) bool {
	return sc.HurstGate != nil && sc.HurstGate.Enabled
}

func normalizeHurstGateMode(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

func resolveHurstGateMode(hg *HurstGateConfig) string {
	if hg == nil {
		return HurstGateModeGate
	}
	if m := normalizeHurstGateMode(hg.Mode); m != "" {
		return m
	}
	return HurstGateModeGate
}

func parseHurstGateMode(v string) (string, error) {
	switch m := normalizeHurstGateMode(v); m {
	case "", HurstGateModeGate, HurstGateModeSize:
		return m, nil
	}
	return "", fmt.Errorf("hurst_gate.mode must be %q or %q, got %q", HurstGateModeGate, HurstGateModeSize, v)
}

func parseHurstGateOnFailure(v string) (string, error) {
	switch n := normalizeRegimeGateOnFailure(v); n {
	case "", HurstGateOnFailureOpen, HurstGateOnFailureClosed:
		return n, nil
	}
	return "", fmt.Errorf("hurst_gate_on_failure must be %q or %q, got %q", HurstGateOnFailureOpen, HurstGateOnFailureClosed, v)
}

func resolveHurstGateOnFailure(sc StrategyConfig, rc *RegimeConfig) string {
	if sc.HurstGate != nil {
		if v := normalizeRegimeGateOnFailure(sc.HurstGate.OnFailure); v != "" {
			return v
		}
	}
	if rc != nil {
		if v := normalizeRegimeGateOnFailure(rc.HurstGateOnFailure); v != "" {
			return v
		}
	}
	return HurstGateOnFailureOpen
}

func resolveHurstGateWindow(sc StrategyConfig, rc *RegimeConfig) string {
	if sc.HurstGate != nil {
		if key := normalizeRegimeWindowKey(sc.HurstGate.WindowKey); key != "" && key != regimeWindowDefaultKey {
			return key
		}
	}
	return resolveStrategyRegimeWindow(sc, "gate", rc)
}

func resolveHurstSizeFloor(hg *HurstGateConfig) float64 {
	if hg != nil && hg.SizeFloor != nil && *hg.SizeFloor > 0 && *hg.SizeFloor <= 1.0 {
		return *hg.SizeFloor
	}
	return hurstDefaultSizeFloor
}

func hurstGateThresholdKey(hg *HurstGateConfig, windowKey string) string {
	if hg == nil {
		return ""
	}
	parts := []string{
		"w=" + windowKey,
		"min=" + formatOptionalFloatForKey(hg.Min),
		"max=" + formatOptionalFloatForKey(hg.Max),
		"dmin=" + formatOptionalFloatForKey(hg.DisarmMin),
		"dmax=" + formatOptionalFloatForKey(hg.DisarmMax),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:8])
}

func formatOptionalFloatForKey(v *float64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(*v, 'g', 17, 64)
}

func hurstFromPayload(payload RegimePayload, windowKey string) (float64, bool) {
	if payload.IsEmpty() || !payload.MultiMode {
		return math.NaN(), false
	}
	snap, ok := payload.Windows[normalizeRegimeWindowKey(windowKey)]
	if !ok || snap.Metrics == nil {
		return math.NaN(), false
	}
	v, ok := snap.Metrics[hurstGateMetricKey]
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		return math.NaN(), false
	}
	return v, true
}

func hurstInArmBand(hg *HurstGateConfig, h float64) bool {
	if hg == nil {
		return true
	}
	if hg.Min != nil && h < *hg.Min {
		return false
	}
	if hg.Max != nil && h > *hg.Max {
		return false
	}
	return true
}

func hurstCrossedDisarm(hg *HurstGateConfig, h float64) bool {
	if hg == nil {
		return false
	}
	lo := hg.DisarmMin
	if lo == nil {
		lo = hg.Min
	}
	hi := hg.DisarmMax
	if hi == nil {
		hi = hg.Max
	}
	if lo != nil && h < *lo {
		return true
	}
	if hi != nil && h > *hi {
		return true
	}
	return false
}

func advanceHurstState(hg *HurstGateConfig, prior string, h float64, hKnown bool) string {
	if !hKnown {
		return prior
	}
	switch prior {
	case hurstGateStateArmed:
		if hurstCrossedDisarm(hg, h) {
			return hurstGateStateDisarmed
		}
		return hurstGateStateArmed
	case hurstGateStateDisarmed:
		if hurstInArmBand(hg, h) {
			return hurstGateStateArmed
		}
		return hurstGateStateDisarmed
	default:
		if hurstInArmBand(hg, h) {
			return hurstGateStateArmed
		}
		return hurstGateStateDisarmed
	}
}

func hurstSizeMultiplier(h, floor float64) float64 {
	if math.IsNaN(h) || math.IsInf(h, 0) {
		return 1.0
	}
	if floor <= 0 || floor > 1.0 {
		floor = hurstDefaultSizeFloor
	}
	m := math.Abs(h-0.5) / hurstSizeSpan
	if m < floor {
		m = floor
	}
	if m > 1.0 {
		m = 1.0
	}
	return m
}

func evaluateHurstGate(sc StrategyConfig, payload RegimePayload, rc *RegimeConfig, prior HurstGateState, posQty float64) HurstGateDecision {
	d := HurstGateDecision{SizeMult: 1.0, H: math.NaN()}
	if !hurstGateConfigured(sc) {
		return d
	}
	hg := sc.HurstGate
	windowKey := resolveHurstGateWindow(sc, rc)
	d.Active = true
	d.Mode = resolveHurstGateMode(hg)
	d.Key = hurstGateThresholdKey(hg, windowKey)

	h, hKnown := hurstFromPayload(payload, windowKey)
	d.H, d.HKnown = h, hKnown

	priorState := prior.State
	if prior.Key != d.Key {
		priorState = hurstGateStateUnknown
	}
	d.State = advanceHurstState(hg, priorState, h, hKnown)

	failClosed := resolveHurstGateOnFailure(sc, rc) == HurstGateOnFailureClosed

	if d.Mode == HurstGateModeSize {

		if hKnown {
			d.SizeMult = hurstSizeMultiplier(h, resolveHurstSizeFloor(hg))
			d.Detail = fmt.Sprintf("hurst %.4f → size ×%.4f (window %q)", h, d.SizeMult, windowKey)
			return d
		}
		d.SizeMult = 1.0
		if failClosed && posQty <= 0 {
			d.Holds = true
			d.Detail = fmt.Sprintf("hurst unavailable, fail-closed (window %q)", windowKey)
			return d
		}
		d.Detail = fmt.Sprintf("hurst unavailable, fail-open → size ×1.0 (window %q)", windowKey)
		return d
	}

	switch {
	case d.State == hurstGateStateDisarmed:
		d.Holds = true
		d.Detail = fmt.Sprintf("hurst %.4f outside %s (window %q, disarmed)", h, hurstGateBandLabel(hg), windowKey)
	case d.State == hurstGateStateUnknown && failClosed && posQty <= 0:
		d.Holds = true
		d.Detail = fmt.Sprintf("hurst unavailable, fail-closed (window %q)", windowKey)
	case d.State == hurstGateStateUnknown:
		d.Detail = fmt.Sprintf("hurst unavailable, fail-open (window %q)", windowKey)
	default:
		d.Detail = fmt.Sprintf("hurst %.4f within %s (window %q, armed)", h, hurstGateBandLabel(hg), windowKey)
	}
	return d
}

func hurstGateBandLabel(hg *HurstGateConfig) string {
	if hg == nil {
		return "band"
	}
	switch {
	case hg.Min != nil && hg.Max != nil:
		return fmt.Sprintf("band [%g, %g]", *hg.Min, *hg.Max)
	case hg.Min != nil:
		return fmt.Sprintf("min %g", *hg.Min)
	case hg.Max != nil:
		return fmt.Sprintf("max %g", *hg.Max)
	}
	return "band"
}

func nextHurstGateState(prior HurstGateState, d HurstGateDecision, now time.Time) HurstGateState {
	if !d.Active {
		return prior
	}
	next := HurstGateState{Key: d.Key, State: d.State, Observed: prior.Observed && prior.Key == d.Key}
	if d.HKnown {
		next.LastH = d.H
		next.LastHAt = now
		next.Observed = true
	} else if prior.Key == d.Key {
		next.LastH = prior.LastH
		next.LastHAt = prior.LastHAt
	}
	return next
}

func (s HurstGateState) IsZero() bool {
	return s.Key == "" && s.State == "" && !s.Observed
}

func marshalHurstGateStateJSON(st HurstGateState) string {
	if st.IsZero() {
		return ""
	}
	b, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	return string(b)
}

func unmarshalHurstGateStateJSON(raw string) HurstGateState {
	if strings.TrimSpace(raw) == "" {
		return HurstGateState{}
	}
	var st HurstGateState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return HurstGateState{}
	}
	switch st.State {
	case hurstGateStateArmed, hurstGateStateDisarmed:
	default:
		st.State = hurstGateStateUnknown
	}
	return st
}

func advanceHurstGate(sc StrategyConfig, payload RegimePayload, rc *RegimeConfig, stratState *StrategyState, mu *sync.RWMutex, posQty float64) HurstGateDecision {
	if !hurstGateConfigured(sc) {
		return HurstGateDecision{SizeMult: 1.0, H: math.NaN()}
	}
	var prior HurstGateState
	if stratState != nil {
		if mu != nil {
			mu.RLock()
		}
		prior = stratState.HurstGate
		if mu != nil {
			mu.RUnlock()
		}
	}
	d := evaluateHurstGate(sc, payload, rc, prior, posQty)
	if stratState != nil {
		next := nextHurstGateState(prior, d, time.Now().UTC())
		if mu != nil {
			mu.Lock()
		}
		stratState.HurstGate = next
		if mu != nil {
			mu.Unlock()
		}
	}
	return d
}

func cloneHurstGateConfig(hg *HurstGateConfig) *HurstGateConfig {
	if hg == nil {
		return nil
	}
	out := *hg
	out.Min = cloneFloatPtr(hg.Min)
	out.Max = cloneFloatPtr(hg.Max)
	out.DisarmMin = cloneFloatPtr(hg.DisarmMin)
	out.DisarmMax = cloneFloatPtr(hg.DisarmMax)
	out.SizeFloor = cloneFloatPtr(hg.SizeFloor)
	return &out
}

func cloneFloatPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func formatHurstGateForLog(hg *HurstGateConfig) string {
	if hg == nil {
		return "(unset)"
	}
	if !hg.Enabled {
		return "disabled"
	}
	return fmt.Sprintf("enabled mode=%s %s window=%q on_failure=%q",
		resolveHurstGateMode(hg), hurstGateBandLabel(hg), hg.WindowKey, hg.OnFailure)
}

func stampHurstGateAtOpenIfOpened(s *StrategyState, symbol string, opened bool, d HurstGateDecision) {
	if s == nil || !opened || !d.Active {
		return
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil {
		return
	}
	if d.HKnown && !math.IsNaN(d.H) {
		pos.HurstAtOpen = d.H
	}
	if d.Mode == HurstGateModeSize {
		pos.HurstSizeMult = d.OpenSizeMult()
	}
}

func hurstGateStatusMarkerForStrategy(sc StrategyConfig, stratState *StrategyState, rc *RegimeConfig, mu *sync.RWMutex) string {
	if !hurstGateConfigured(sc) || stratState == nil {
		return ""
	}
	if mu != nil {
		mu.RLock()
	}
	st := stratState.HurstGate
	if mu != nil {
		mu.RUnlock()
	}
	mode := resolveHurstGateMode(sc.HurstGate)
	failClosed := resolveHurstGateOnFailure(sc, rc) == HurstGateOnFailureClosed
	d := HurstGateDecision{Active: true, Mode: mode, H: math.NaN(), SizeMult: 1.0, State: st.State}
	if st.Key == hurstGateThresholdKey(sc.HurstGate, resolveHurstGateWindow(sc, rc)) && st.Observed {
		d.H, d.HKnown = st.LastH, true
		if mode == HurstGateModeSize {
			d.SizeMult = hurstSizeMultiplier(st.LastH, resolveHurstSizeFloor(sc.HurstGate))
		}
	}
	if mode == HurstGateModeGate {
		d.Holds = d.State == hurstGateStateDisarmed || (d.State == hurstGateStateUnknown && failClosed)
	} else {
		d.Holds = !d.HKnown && failClosed
	}
	return hurstGateStatusMarker(d)
}

func hurstGateStatusMarker(d HurstGateDecision) string {
	if !d.Active {
		return ""
	}
	if !d.HKnown {
		if d.Holds {
			return "hurst=? (gate closed)"
		}
		return "hurst=?"
	}
	if d.Mode == HurstGateModeSize {
		return fmt.Sprintf("hurst=%.4f (size ×%.2f)", d.H, d.SizeMult)
	}
	if d.Holds {
		return fmt.Sprintf("hurst=%.4f (gate disarmed)", d.H)
	}
	return fmt.Sprintf("hurst=%.4f (gate armed)", d.H)
}

func validateHurstGateConfigs(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var errs []string

	if cfg.Regime != nil {
		if _, err := parseHurstGateOnFailure(cfg.Regime.HurstGateOnFailure); err != nil {
			errs = append(errs, fmt.Sprintf("regime.hurst_gate_on_failure: %v", err))
		}
	}

	for _, sc := range cfg.Strategies {
		hg := sc.HurstGate
		if hg == nil {
			continue
		}
		prefix := fmt.Sprintf("strategy[%s]", sc.ID)

		mode, err := parseHurstGateMode(hg.Mode)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", prefix, err))
			mode = HurstGateModeGate
		}
		if mode == "" {
			mode = HurstGateModeGate
		}
		if _, err := parseHurstGateOnFailure(hg.OnFailure); err != nil {
			errs = append(errs, fmt.Sprintf("%s: hurst_gate.on_failure: %v", prefix, err))
		}

		switch sc.Type {
		case "options", "manual":
			errs = append(errs, fmt.Sprintf("%s: hurst_gate is not supported for type=%q — the gate wires into the spot/perps/futures signal dispatch only (#1411)", prefix, sc.Type))
		}

		errs = append(errs, validateHurstGateBounds(hg, mode, prefix)...)
		errs = append(errs, validateHurstGateWindow(sc, hg, cfg.Regime, prefix)...)
	}
	sort.Strings(errs)
	return errs
}

func validateHurstGateBounds(hg *HurstGateConfig, mode, prefix string) []string {
	var errs []string
	inUnitInterval := func(name string, v *float64) bool {
		if v == nil {
			return true
		}
		if *v <= 0 || *v >= 1 {
			errs = append(errs, fmt.Sprintf("%s: hurst_gate.%s must be in (0, 1) exclusive, got %g", prefix, name, *v))
			return false
		}
		return true
	}
	minOK := inUnitInterval("min", hg.Min)
	maxOK := inUnitInterval("max", hg.Max)
	dMinOK := inUnitInterval("disarm_min", hg.DisarmMin)
	dMaxOK := inUnitInterval("disarm_max", hg.DisarmMax)

	if mode == HurstGateModeSize {

		for name, v := range map[string]*float64{"min": hg.Min, "max": hg.Max, "disarm_min": hg.DisarmMin, "disarm_max": hg.DisarmMax} {
			if v != nil {
				errs = append(errs, fmt.Sprintf("%s: hurst_gate.%s has no meaning with mode=%q (the band gates entries; size scales them) — remove it or switch to mode=%q", prefix, name, HurstGateModeSize, HurstGateModeGate))
			}
		}
	} else {
		if hg.Min == nil && hg.Max == nil {
			errs = append(errs, fmt.Sprintf("%s: hurst_gate with mode=%q requires at least one of min/max — without a band the gate can never disarm", prefix, HurstGateModeGate))
		}
		if minOK && maxOK && hg.Min != nil && hg.Max != nil && *hg.Min >= *hg.Max {
			errs = append(errs, fmt.Sprintf("%s: hurst_gate.min (%g) must be < hurst_gate.max (%g)", prefix, *hg.Min, *hg.Max))
		}
		if hg.DisarmMin != nil {
			if hg.Min == nil {
				errs = append(errs, fmt.Sprintf("%s: hurst_gate.disarm_min requires hurst_gate.min — it is the hysteresis exit for the min bound", prefix))
			} else if dMinOK && minOK && *hg.DisarmMin > *hg.Min {
				errs = append(errs, fmt.Sprintf("%s: hurst_gate.disarm_min (%g) must be <= hurst_gate.min (%g) — a disarm bound tighter than the arm bound inverts hysteresis into a flapping gate", prefix, *hg.DisarmMin, *hg.Min))
			}
		}
		if hg.DisarmMax != nil {
			if hg.Max == nil {
				errs = append(errs, fmt.Sprintf("%s: hurst_gate.disarm_max requires hurst_gate.max — it is the hysteresis exit for the max bound", prefix))
			} else if dMaxOK && maxOK && *hg.DisarmMax < *hg.Max {
				errs = append(errs, fmt.Sprintf("%s: hurst_gate.disarm_max (%g) must be >= hurst_gate.max (%g) — a disarm bound tighter than the arm bound inverts hysteresis into a flapping gate", prefix, *hg.DisarmMax, *hg.Max))
			}
		}
	}

	if hg.SizeFloor != nil {
		if mode != HurstGateModeSize {
			errs = append(errs, fmt.Sprintf("%s: hurst_gate.size_floor only applies with mode=%q, got mode=%q", prefix, HurstGateModeSize, mode))
		}
		if *hg.SizeFloor <= 0 || *hg.SizeFloor > 1 {
			errs = append(errs, fmt.Sprintf("%s: hurst_gate.size_floor must be in (0, 1], got %g", prefix, *hg.SizeFloor))
		}
	}
	return errs
}

func validateHurstGateWindow(sc StrategyConfig, hg *HurstGateConfig, rc *RegimeConfig, prefix string) []string {
	var errs []string
	explicit := normalizeRegimeWindowKey(hg.WindowKey)
	if rc == nil || !rc.Enabled {
		return append(errs, fmt.Sprintf("%s: hurst_gate requires regime.enabled=true — the hurst metric is produced by the regime bundle (#1411)", prefix))
	}
	if !regimeMultiWindowEnabled(rc) {
		return append(errs, fmt.Sprintf("%s: hurst_gate requires regime.windows to be configured with a composite window — the legacy single-lookback regime path never emits a hurst metric (#1411)", prefix))
	}
	key := resolveHurstGateWindow(sc, rc)
	if key == "" || !regimeWindowExists(rc, key) {
		label := key
		if explicit != "" && explicit != regimeWindowDefaultKey {
			label = explicit
		}
		return append(errs, fmt.Sprintf("%s: hurst_gate window %q not found in regime.windows (valid: %s)", prefix, label, strings.Join(sortedRegimeWindowNamesFromConfig(rc.Windows), ", ")))
	}
	spec, ok := regimeWindowSpec(rc, key)
	if !ok {
		return append(errs, fmt.Sprintf("%s: hurst_gate window %q not found in regime.windows (valid: %s)", prefix, key, strings.Join(sortedRegimeWindowNamesFromConfig(rc.Windows), ", ")))
	}
	if spec.effectiveClassifier() != regimeClassifierComposite {
		errs = append(errs, fmt.Sprintf("%s: hurst_gate window %q uses classifier %q, but the hurst metric is emitted ONLY by the %q classifier (shared_tools/regime.py latest_regime_composite) — the gate would never see a value and hurst_gate.on_failure would govern every cycle. Point window_key at a composite window (#1411)", prefix, key, spec.effectiveClassifier(), regimeClassifierComposite))
	}
	return errs
}
