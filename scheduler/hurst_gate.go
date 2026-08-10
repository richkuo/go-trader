package main

// hurst_gate.go — #1411 per-strategy Hurst entry gate and persistence-scaled
// position sizing. DEFAULT-OFF; per-strategy opt-in only; never auto-enabled.
//
// CALIBRATION STATUS. The live evidence is the #1424 RESOLUTION study
// (backtest/research/hurst_gate_calibration.md — the contract path, now owned by
// hurst_1424_gate_resolution.py; the superseded #1422 and #1410 renders live
// beside it at hurst_1422_gate_power.md and hurst_1410_gate_calibration.md, and
// neither of those scripts may write the contract path any more).
// Recommendation: INCONCLUSIVE, and the study's own VALIDITY GATE FAILED.
//
// #1424 attacked #1422's power shortfall three ways at once: ONE pre-registered
// hypothesis instead of four (so the rank-1 Benjamini-Hochberg bar is alpha, not
// alpha/4), eight added pre-2020 calendar clusters from two venues the binanceus
// cache cannot reach (Bitstamp and Coinbase Exchange, 2013-2020H1), and a
// bounded-variance primary target (signed efficiency over a fixed 96-hour
// horizon). Pool: 860 legs, 26 datasets, 16 windows, ~29.0k primary-cohort rows.
//
// The gate is read ROW-MATCHED and DIRECTIONALLY on the CONFIRMATORY family
// (momentum, 7,992 rows — the family the single pre-registered hypothesis
// belongs to). Those rows resolve 0.013 efficiency units and 0.94 pp of net
// return, and they separate by -0.005 and -0.12 pp. The sign is the finding:
// every null and injection here is one-sided on mean(kept) - mean(suppressed),
// so a NEGATIVE separation means the trades this gate would have SUPPRESSED did
// BETTER, which is the direction the design cannot detect at any magnitude. The
// run therefore carries no bound on the confirmatory family in either
// direction. It is NOT evidence that no edge exists, and it is not the null the
// key risk described; the pre-registered key-risk prediction is UNRESOLVED.
// (The POOLED primary limit is 0.008 over both families' 28,998 rows. It is
// printed in the report but the gate must never read it: a larger pool resolves
// a smaller effect, so pairing it with one family's separation compares two
// samples and biases the gate toward passing.)
//
// The pinned hypothesis (momentum/gate/W512/arm0.52/dis0.48) reached cluster
// p=0.3557 on the primary target and p=0.3387 on net return. #1422's disclosed
// interim look on a SUBSET of these rows read p=0.0485; it did not reproduce on
// the superset.
//
// This mechanism therefore still ships with NO recommended thresholds anywhere
// (config.example.json carries no hurst_gate block) and stays off until an
// operator explicitly opts a strategy in. Nothing here reads a default threshold,
// and nothing enables itself. #1424 re-answers #1412's Stage 0 gate on a larger
// pool: NO JOINT SEPARATION on both families (cluster p=0.9696 momentum, 0.7061
// mean_reversion, against a Bonferroni bar of 0.025), so fusing H into the
// composite classifier is not justified and this standalone gate remains the
// correct amount of Hurst.
//
// LAYERING. This is a STANDALONE gate stacked ON TOP of the allowed_regimes
// label gate. It never touches applyRegimeGate, regimeBlocksOpen,
// resolveRegimeGateOnFailure, map_composite_label, composite thresholds, ATR
// method resolution (#1277), or directional certifications (#1085). With
// hurst_gate absent, every one of those behaves byte-identically.
//
// WHAT IT BLOCKS. Position-INCREASING signals only, classified through
// pausedBlocksSignal exactly as the #1150 pause, #1269 daily-loss and #1344
// notional-cap arms do. Closes, pure-close directional exits, trailing SL, the
// TP ratchet, protection sync, paper SL/TP simulation, hedge sync (#1159) and
// every kill-switch path always pass through.
//
// WHERE H COMES FROM. metrics["hurst"] is emitted ONLY by the composite
// classifier (shared_tools/regime.py latest_regime_composite); the default adx
// path (latest_regime) never emits it. validateHurstGateConfigs therefore
// REJECTS at config load any hurst_gate whose resolved window is not
// composite-classified — otherwise H would be permanently absent and on_failure
// would silently govern every cycle, which under "closed" is a permanent entry
// block indistinguishable from "the feature is off".
//
// #1409 INVARIANT REVOKED. #1409 stamped the metric advisory-only ("never read
// by map_composite_label, gating, or sizing"). This file revokes that for
// gating and sizing; the shared_tools/regime.py comment is updated in the same
// change. map_composite_label still never reads it.

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

// Hurst gate modes.
const (
	// HurstGateModeGate holds position-increasing signals while the hysteresis
	// state machine is disarmed.
	HurstGateModeGate = "gate"
	// HurstGateModeSize scales the computed open size by the persistence
	// distance |H-0.5|. Stateless; never holds on a known H.
	HurstGateModeSize = "size"
)

// Hurst gate failure policy for an absent/NaN H. Mirrors #1278 exactly.
const (
	HurstGateOnFailureOpen   = "open"
	HurstGateOnFailureClosed = "closed"
)

// Hysteresis states persisted per strategy.
const (
	hurstGateStateUnknown  = ""
	hurstGateStateArmed    = "armed"
	hurstGateStateDisarmed = "disarmed"
)

// hurstGateMetricKey is the RegimeSnapshot.Metrics key #1409 writes.
const hurstGateMetricKey = "hurst"

// hurstSizeSpan is the |H-0.5| distance that maps to a full-size (1.0)
// multiplier. 0.15 puts H=0.65 (or H=0.35) at full size and H=0.5 at the floor.
const hurstSizeSpan = 0.15

// hurstDefaultSizeFloor is the multiplier applied when H sits at 0.5 (no
// measurable persistence either way) and size_floor is omitted.
const hurstDefaultSizeFloor = 0.25

// HurstGateConfig is the per-strategy hurst_gate block (#1411). Default-off:
// a nil block, or Enabled=false, leaves every dispatch path byte-identical.
//
// Read ONLY via resolveHurstGateOnFailure / resolveHurstGateWindow /
// evaluateHurstGate — never read the raw fields at a decision site.
type HurstGateConfig struct {
	// Enabled opts the strategy in. false (or an absent block) is a complete
	// no-op: no state is advanced, no size is scaled, no signal is held.
	Enabled bool `json:"enabled"`
	// Mode selects "gate" (hysteresis hold) or "size" (multiplier). Empty
	// defaults to "gate".
	Mode string `json:"mode,omitempty"`
	// Min / Max bound the ARM band for mode=gate: the gate arms while
	// H >= Min (when set) and H <= Max (when set). A momentum-style strategy
	// sets Min; a mean-reversion-style strategy sets Max. Rejected in
	// mode=size, where they carry no meaning.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	// DisarmMin / DisarmMax are the hysteresis exit bounds: once armed, the
	// gate stays armed until H crosses these looser bounds. Omitted collapses
	// hysteresis onto the arm bound. DisarmMin requires Min and must be <= Min;
	// DisarmMax requires Max and must be >= Max.
	DisarmMin *float64 `json:"disarm_min,omitempty"`
	DisarmMax *float64 `json:"disarm_max,omitempty"`
	// WindowKey names the regime window whose metrics["hurst"] the gate reads.
	// Empty falls back to the strategy's gate-window resolution. The resolved
	// window MUST be composite-classified (validation enforces it).
	WindowKey string `json:"window_key,omitempty"`
	// OnFailure is the policy for an absent/NaN H: "open" (default) admits,
	// "closed" holds FRESH OPENS ONLY. Empty inherits regime.hurst_gate_on_failure,
	// then "open". Read via resolveHurstGateOnFailure, never directly.
	OnFailure string `json:"on_failure,omitempty"`
	// SizeFloor is the mode=size lower clamp, in (0, 1]. Omitted defaults to
	// hurstDefaultSizeFloor. Rejected in mode=gate.
	SizeFloor *float64 `json:"size_floor,omitempty"`
}

// HurstGateState is the persisted hysteresis state for one strategy.
//
// Key pins the threshold tuple the state was produced under. A config edit
// that changes any bound produces a different Key, and the stale state is
// DISCARDED rather than reused — a disarmed latch from old thresholds must
// never survive into a new band.
type HurstGateState struct {
	Key      string    `json:"key,omitempty"`
	State    string    `json:"state,omitempty"`
	LastH    float64   `json:"last_h,omitempty"`
	LastHAt  time.Time `json:"last_h_at,omitempty"`
	Observed bool      `json:"observed,omitempty"` // true once a valid H was seen under Key
}

// HurstGateDecision is one cycle's gate outcome. Pure output of
// evaluateHurstGate; the dispatch sites turn Holds into a Signal=0 hold and
// carry SizeMult into the open sizing.
type HurstGateDecision struct {
	Active   bool    // enabled and configuration resolvable this cycle
	Mode     string  // resolved mode
	H        float64 // NaN when absent
	HKnown   bool    // whether H was present and finite
	State    string  // post-transition hysteresis state
	Holds    bool    // gate: hold position-increasing signals this cycle
	SizeMult float64 // size: [floor, 1.0]; always exactly 1.0 otherwise
	Key      string  // threshold key the state is stored under
	Detail   string  // operator log line
}

// OpenSizeMult is the multiplier the executors apply to a COMPUTED open size.
// It is 1.0 unless the gate is active in size mode with a known H, and is
// never greater than 1.0 — the gate can only shrink an open, never grow one.
func (d HurstGateDecision) OpenSizeMult() float64 {
	if !d.Active || d.Mode != HurstGateModeSize {
		return 1.0
	}
	if d.SizeMult <= 0 || d.SizeMult > 1.0 || math.IsNaN(d.SizeMult) {
		return 1.0
	}
	return d.SizeMult
}

// hurstGateConfigured reports whether sc opts into the gate at all.
func hurstGateConfigured(sc StrategyConfig) bool {
	return sc.HurstGate != nil && sc.HurstGate.Enabled
}

func normalizeHurstGateMode(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// resolveHurstGateMode returns the effective mode; empty defaults to "gate".
func resolveHurstGateMode(hg *HurstGateConfig) string {
	if hg == nil {
		return HurstGateModeGate
	}
	if m := normalizeHurstGateMode(hg.Mode); m != "" {
		return m
	}
	return HurstGateModeGate
}

// parseHurstGateMode validates a mode value at config load. Empty is valid.
func parseHurstGateMode(v string) (string, error) {
	switch m := normalizeHurstGateMode(v); m {
	case "", HurstGateModeGate, HurstGateModeSize:
		return m, nil
	}
	return "", fmt.Errorf("hurst_gate.mode must be %q or %q, got %q", HurstGateModeGate, HurstGateModeSize, v)
}

// parseHurstGateOnFailure validates an on_failure value at config load. Empty
// is valid (inherit / default open). Mirrors parseRegimeGateOnFailure (#1278).
func parseHurstGateOnFailure(v string) (string, error) {
	switch n := normalizeRegimeGateOnFailure(v); n {
	case "", HurstGateOnFailureOpen, HurstGateOnFailureClosed:
		return n, nil
	}
	return "", fmt.Errorf("hurst_gate_on_failure must be %q or %q, got %q", HurstGateOnFailureOpen, HurstGateOnFailureClosed, v)
}

// resolveHurstGateOnFailure resolves the effective absent-H policy for a
// strategy: per-strategy hurst_gate.on_failure wins, else the global
// regime.hurst_gate_on_failure, else "open". This is the SINGLE accessor —
// exact mirror of resolveRegimeGateOnFailure (#1278). Never read the raw
// fields at a decision site.
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

// resolveHurstGateWindow resolves which regime window's hurst metric the gate
// reads: an explicit window_key, else the strategy's gate-window resolution
// (which itself falls back to the primary window).
func resolveHurstGateWindow(sc StrategyConfig, rc *RegimeConfig) string {
	if sc.HurstGate != nil {
		if key := normalizeRegimeWindowKey(sc.HurstGate.WindowKey); key != "" && key != regimeWindowDefaultKey {
			return key
		}
	}
	return resolveStrategyRegimeWindow(sc, "gate", rc)
}

// resolveHurstSizeFloor returns the effective mode=size lower clamp.
func resolveHurstSizeFloor(hg *HurstGateConfig) float64 {
	if hg != nil && hg.SizeFloor != nil && *hg.SizeFloor > 0 && *hg.SizeFloor <= 1.0 {
		return *hg.SizeFloor
	}
	return hurstDefaultSizeFloor
}

// hurstGateThresholdKey hashes the tuple that DEFINES the hysteresis state
// series: the window the metric is read from plus the four bounds. Any edit to
// those discards persisted state (a disarmed latch from an old band must never
// carry into a new one). mode / on_failure / size_floor are deliberately
// excluded: they change the DECISION taken from a state, never the state
// series itself.
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

// hurstFromPayload extracts the gate window's hurst metric. Returns
// (value, true) only for a present, finite reading; a missing window, missing
// key, empty payload or non-finite value all return (NaN, false).
//
// regime.py rounds H to 4 decimals and OMITS the key entirely on NaN (bare NaN
// is not valid JSON), so an absent key is the normal "unknown" signal.
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

// RANGE NOTE (verified against the estimator, not assumed). The CONFIG bounds
// are validated to (0, 1) exclusive because that is the only range an operator
// can sensibly gate on. The RUNTIME metric is NOT so bounded: DFA returns H
// well above 1 on a near-deterministic series (a perfectly smooth linear ramp
// measures ~2.0), which is a property of the estimator, not a fault. The two
// comparators below are therefore written to stay correct for any finite H:
// a reading above every configured bound arms a min-only gate and disarms a
// max-only one, and hurstSizeMultiplier clamps at 1.0. Never "simplify" these
// on the assumption that H lies in (0, 1).

// hurstInArmBand reports whether H sits inside the configured ARM band.
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

// hurstCrossedDisarm reports whether H has crossed the (looser) hysteresis
// exit bounds. An omitted disarm bound collapses onto the matching arm bound,
// which makes the state machine degenerate to a plain band test.
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

// advanceHurstState applies one observation to the hysteresis state machine.
// An absent H NEVER transitions state — the #1410 study's NaN policy: NaN is
// unknown, never 0.5, and it neither arms nor disarms.
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
	default: // unknown — first valid reading resolves it outright
		if hurstInArmBand(hg, h) {
			return hurstGateStateArmed
		}
		return hurstGateStateDisarmed
	}
}

// hurstSizeMultiplier maps a known H to the open-size multiplier:
//
//	m = clamp(|H - 0.5| / 0.15, size_floor, 1.0)
//
// The result is always in [floor, 1.0]: the gate can shrink an open, never
// grow one. This is the ISSUE's formula. The #1410 study swept a different
// form (m = clamp(1 + gain*e, 0, 1.5), which can EXCEED 1.0); that study
// shipped no recommendation, so the issue's formula governs here.
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

// evaluateHurstGate is the pure per-cycle decision: no locks, no I/O, no
// mutation. prior is the persisted state; posQty scopes the fail-closed arm.
//
// The fail-closed arm is FLAT-ONLY, the exact #1278 regimeBlocksOpen shape
// (scheduler/regime.go:102-110): an unknown H under on_failure="closed" holds
// FRESH OPENS only and never touches management of an already-open position.
// The known-disarmed arm carries no flat-only restriction because
// pausedBlocksSignal at the dispatch site already scopes it to
// position-increasing actions — adds and flips are held, closes pass.
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

	// A threshold edit invalidates the persisted latch outright.
	priorState := prior.State
	if prior.Key != d.Key {
		priorState = hurstGateStateUnknown
	}
	d.State = advanceHurstState(hg, priorState, h, hKnown)

	failClosed := resolveHurstGateOnFailure(sc, rc) == HurstGateOnFailureClosed

	if d.Mode == HurstGateModeSize {
		// Stateless: a known H always sizes, never holds.
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

	// Gate mode.
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

// hurstGateBandLabel renders the configured arm band for operator log lines.
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

// nextHurstGateState folds a decision back into the persisted state.
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

// IsZero reports whether the latch carries nothing worth persisting.
func (s HurstGateState) IsZero() bool {
	return s.Key == "" && s.State == "" && !s.Observed
}

// marshalHurstGateStateJSON renders the latch for the strategies.hurst_gate_state
// column. An empty latch persists as "" so untouched strategies keep the
// column's default and the row stays byte-identical to pre-#1411 saves.
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

// unmarshalHurstGateStateJSON restores the latch. Blank or malformed JSON
// yields the zero (unknown) latch, which the next valid reading resolves —
// never a fabricated armed/disarmed state.
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

// advanceHurstGate is the dispatch-site entry point: it reads the prior state
// under RLock, evaluates the pure decision, then commits the post-transition
// state under Lock. It runs UNCONDITIONALLY at each dispatch group — signal
// independent — so hysteresis advances on every cycle, including Signal==0
// manage cycles. Only mu is taken; strategiesMu is never involved.
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

// cloneHurstGateConfig deep-copies a hurst_gate block so a SIGHUP adoption
// never aliases the freshly loaded config's pointers into the live one.
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

// formatHurstGateForLog renders a hurst_gate block for the SIGHUP change line.
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

// stampHurstGateAtOpenIfOpened freezes the cycle's H reading and the applied
// size multiplier onto a just-opened position, next to the other open-time
// stamps. Diagnostics-only: nothing reads these back into a gating, sizing,
// close or protection decision. Called under the caller's mu.Lock.
//
// The stamp lands only on a genuine fresh open (opened == true, matching the
// #1085/#1277 stamp discipline), never on an add or a re-affirm, so the
// diagnostics row describes the decision that created the position.
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

// hurstGateStatusMarkerForStrategy renders the Phase 6 status suffix from the
// PERSISTED latch, under RLock. Display-only — it re-reads the same state the
// dispatch sites already advanced this cycle, so it never re-evaluates the
// gate and never mutates anything.
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

// hurstGateStatusMarker renders the display-only status suffix for Phase 6.
// Display-only: the authoritative decision lives at the dispatch sites.
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

// ---------------------------------------------------------------------------
// Config validation
// ---------------------------------------------------------------------------

// validateHurstGateConfigs fails the config load loudly on any unusable
// hurst_gate block. Shape checks run even when enabled=false so a typo'd block
// an operator later flips on fails at EDIT time, not at open time — the
// validateHedgeConfigs discipline (config.go:1727).
//
// The composite-classifier check is load-bearing: metrics["hurst"] exists only
// in composite window payloads, so a gate pointed at an adx window would never
// see H and on_failure would govern every cycle — under "closed" a permanent,
// silent entry block. Reject instead of running permanently-on-failure.
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

		// Type scoping: only the six signal-dispatch arms are wired. Options
		// dispatch through a separate action list and manual has no open
		// signal to suppress, so a block there would silently do nothing.
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

// validateHurstGateBounds enforces range, ordering, and mode scoping on the
// numeric fields. Every rule fires regardless of enabled.
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
		// The bounds define an arm band, which mode=size never consults.
		// Accepting them would silently ignore an operator's stated intent —
		// the #704 misdiagnosis class. Reject instead.
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

// validateHurstGateWindow rejects any configuration where the gate could never
// read a hurst value: regime detection off, multi-window mode off, the window
// missing, or the window classified by anything other than the composite
// classifier (the only producer of metrics["hurst"]).
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
