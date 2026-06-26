package main

// Regime window divergence detection and trust-short override (#907).
//
// The composite (7-state) regime can be evaluated at multiple windows —
// typically a "medium" window (slow, for open/position policy) and a "short"
// window (fast, for tactical/gate logic). They can disagree by design: the
// medium window lags to reduce whipsaw, the short window reacts quickly.
//
// This module adds a derived signal: when the two windows hard-diverge (one
// bullish, the other bearish), surface that disagreement and optionally use
// it to override the effective direction for new entries.
//
// HL perps live only. Unlike regime_directional_policy's #1025 backtest
// resolver, this divergence override still has no bar-level parity path and is
// rejected by backtest/run_backtest.py.
//
// Config shape (per strategy):
//
//	"regime_window_divergence": {
//	  "short_window": "composite_short",
//	  "medium_window": "composite_medium",
//	  "on_divergence": "trust_short"  // or "trust_medium" | "alert_only"
//	}
//
// Override semantics:
//   - "trust_short":  on hard divergence, effective direction = short window bias.
//   - "trust_medium": on hard divergence, effective direction = medium window bias.
//   - "alert_only":   detect and log/surface, but do not mutate sc.Direction.
//
// Open positions: the override governs new-entry direction only. Open positions
// keep hold-on-transition freeze (pos.Regime governs; #779 semantics).
//
// State lifetime: RegimeDivergenceState on StrategyState is in-memory only
// (json:"-") — not persisted to SQLite. Self-heals on restart within 1 cycle.

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

// divergenceBias is the directional bias inferred from a regime label.
type divergenceBias int

const (
	biasBullish divergenceBias = 1
	biasNeutral divergenceBias = 0
	biasBearish divergenceBias = -1
)

// DivergenceKind classifies how two regime labels relate to each other.
type DivergenceKind string

const (
	DivergenceNone DivergenceKind = "none"
	DivergenceSoft DivergenceKind = "soft"
	DivergenceHard DivergenceKind = "hard"
)

// DivergenceResult holds the output of classifyRegimeDivergence.
type DivergenceResult struct {
	Kind           DivergenceKind `json:"kind"`
	ShortLabel     string         `json:"short_label"`
	MediumLabel    string         `json:"medium_label"`
	OverrideDir    string         `json:"override_dir,omitempty"` // "long", "short", or ""
	TrustingWindow string         `json:"trusting_window,omitempty"`
}

// IsActive reports whether the result produced a direction override.
func (r DivergenceResult) IsActive() bool {
	return r.Kind == DivergenceHard && r.OverrideDir != ""
}

// RegimeDivergenceState is per-strategy mutable state tracking the active
// divergence override. In-memory only (json:"-"); self-heals on restart.
type RegimeDivergenceState struct {
	Short             string `json:"short"`
	Medium            string `json:"medium"`
	Kind              string `json:"kind"`
	ResolvedDirection string `json:"resolved_direction,omitempty"`
	TrustingWindow    string `json:"trusting_window,omitempty"` // "short" or "medium" — which window the override follows
	CyclesActive      int    `json:"cycles_active"`
}

// RegimeWindowDivergence is the per-strategy config block for window divergence
// detection. HL perps live only.
type RegimeWindowDivergence struct {
	ShortWindow  string `json:"short_window"`
	MediumWindow string `json:"medium_window"`
	OnDivergence string `json:"on_divergence"`
	raw          map[string]interface{}
}

// UnmarshalJSON captures the raw shape for strategy-scoped validation in
// LoadConfig — mirrors RegimeDirectionalPolicy's deferred-resolve pattern.
func (d *RegimeWindowDivergence) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("regime_window_divergence: %w", err)
	}
	d.raw = raw
	return nil
}

// MarshalJSON renders the canonical form for hot-reload diff logging.
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

// IsConfigured reports whether the operator supplied any value. Safe to call
// before ResolveRaw (relies on captured raw).
func (d *RegimeWindowDivergence) IsConfigured() bool {
	if d == nil {
		return false
	}
	if d.ShortWindow != "" || d.MediumWindow != "" || d.OnDivergence != "" {
		return true
	}
	return len(d.raw) > 0
}

// IsZero reports whether the block is empty after resolution.
func (d *RegimeWindowDivergence) IsZero() bool {
	if d == nil {
		return true
	}
	return d.ShortWindow == "" && d.MediumWindow == "" && d.OnDivergence == ""
}

// EqualForReload reports shape equality for hot-reload state-compat checks.
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

// ResolveRaw parses the captured raw JSON into the typed fields. Called from
// LoadConfig with strategy-scoped errors. Validates:
//   - required keys: short_window, medium_window, on_divergence
//   - on_divergence ∈ {"trust_short", "trust_medium", "alert_only"}
//   - short_window != medium_window
//   - no unknown keys
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
			// valid
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

// regimeLabelBias returns the directional bias of a composite or ADX regime label.
// ranging_directional uses snapReturnEff (from RegimeSnapshot.Metrics["return_eff"])
// to break the bullish/bearish tie when provided; otherwise treated as neutral.
func regimeLabelBias(label string, snapReturnEff float64) divergenceBias {
	switch strings.TrimSpace(label) {
	case "trending_up", "trending_up_clean", "trending_up_choppy":
		return biasBullish
	case "trending_down", "trending_down_clean", "trending_down_choppy":
		return biasBearish
	case "ranging_directional_up":
		// #1124: the label carries the drift direction directly, so the bias is
		// fixed regardless of snapReturnEff.
		return biasBullish
	case "ranging_directional_down":
		return biasBearish
	case "ranging_directional":
		// Bare label: the producer emits it only when return_eff == 0 exactly,
		// but a stale/legacy snapshot may still carry a nonzero return_eff, so
		// keep resolving the sign as the tie-break for back-compat.
		if snapReturnEff > 0 {
			return biasBullish
		}
		if snapReturnEff < 0 {
			return biasBearish
		}
		return biasNeutral
	default:
		// ranging_quiet, ranging_volatile, ranging, empty, unknown
		return biasNeutral
	}
}

// biasDirection converts a divergenceBias to a direction string suitable for
// sc.Direction. Neutral is never used as an override direction.
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

// classifyRegimeDivergence computes the divergence kind and override direction
// for a pair of regime labels. shortReturnEff / mediumReturnEff provide the
// per-window return-efficiency metric for ranging_directional sign resolution.
// Both signs are resolved symmetrically so trust_medium can resolve a direction
// when the medium window is ranging_directional, and hard divergence on a
// ranging_directional side is not undercounted.
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

	// Biases differ. Determine hard vs soft.
	// Hard: biases are strictly opposite (one bullish, one bearish).
	// Soft: one neutral and the other directional. (Same-bias sub-label
	// differences early-return as none above, so they never reach here.)
	if shortBias != biasNeutral && mediumBias != biasNeutral {
		result.Kind = DivergenceHard
	} else {
		result.Kind = DivergenceSoft
	}

	// Only hard divergence generates an override direction.
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
		// surface only; no direction mutation
	}
	return result
}

// applyRegimeDivergenceOverride evaluates the divergence block on sc and
// mutates the local sc.Direction / sc.InvertSignal when a hard divergence
// override applies. Returns the result so the caller can log and carry state.
//
// Called AFTER applyRegimeDirectionalPolicy (divergence wins when it fires,
// overriding the medium-window policy entry).
//
// Caller contract: pass a LOCAL copy of sc (not a pointer into cfg.Strategies).
// Open positions are NOT affected — this function reads posQty to guard against
// changing the effective direction when a position is already open (hold-on-
// transition freeze: the position runs to its natural exit under pos.Regime).
//
// LIMITATION — gate, not signal flip: the override runs AFTER the signal script
// has already produced its raw signal (scForCheck captured the pre-override
// --direction), so it can only filter which side the post-script direction
// machinery (PerpsOrderSkipReason / perpsLiveOrderSize) admits. It does NOT
// re-run the script with a flipped direction. Practical effect:
//   - direction="both" base: the script emits both buy and sell signals, so the
//     override genuinely selects which side opens (full flip behavior — the
//     intended case, matching the #907 ETH example which resolves to "both").
//   - direction="long"/"short" base: the script only ever emits its one side, so
//     a trust_short override resolving to the opposite side acts as a stand-aside
//     gate (it blocks the base side's new entries; it cannot synthesize the
//     opposite entry). Use direction="both" for full divergence-driven flipping.
//
// This intentionally differs from regime_directional_policy, which couples
// direction with invert_signal to synthesize the opposite side.
func applyRegimeDivergenceOverride(sc *StrategyConfig, payload RegimePayload, rc *RegimeConfig, posQty float64) DivergenceResult {
	if sc == nil || sc.RegimeWindowDivergence.IsZero() {
		return DivergenceResult{Kind: DivergenceNone}
	}
	d := sc.RegimeWindowDivergence

	shortLabel := payload.Label(d.ShortWindow, rc)
	mediumLabel := payload.Label(d.MediumWindow, rc)

	// Extract return_eff from each window snapshot for ranging_directional sign.
	var shortReturnEff, mediumReturnEff float64
	if snap, ok := payload.Windows[normalizeRegimeWindowKey(d.ShortWindow)]; ok {
		shortReturnEff = snap.Metrics["return_eff"]
	}
	if snap, ok := payload.Windows[normalizeRegimeWindowKey(d.MediumWindow)]; ok {
		mediumReturnEff = snap.Metrics["return_eff"]
	}

	result := classifyRegimeDivergence(shortLabel, mediumLabel, shortReturnEff, mediumReturnEff, d.OnDivergence)

	// Apply the override only when flat. When a position is open (posQty > 0),
	// hold-on-transition semantics mean the position runs under pos.Regime — the
	// divergence flag is still computed and surfaced, but we do not flip sc.Direction
	// out from under the held position.
	if result.IsActive() && posQty <= 0 {
		sc.Direction = result.OverrideDir
		sc.InvertSignal = false
	}

	return result
}

// updateStrategyDivergenceState updates the in-memory per-strategy divergence
// state after a cycle's divergence result is known. Called from the cycle loop
// after syncStrategyRegimeState.
func updateStrategyDivergenceState(s *StrategyState, result DivergenceResult) {
	if s == nil {
		return
	}
	// Clear on anything that is not an active soft/hard divergence. This covers
	// the zero-value DivergenceResult (Kind == "") that runHyperliquidCheck
	// leaves on result.Divergence when the strategy has no divergence block
	// configured — without this guard an unconfigured HL perps strategy would
	// accrue a non-nil RegimeDivergence{Kind:""} with ever-growing CyclesActive
	// and serialize it into /status. (PR #916 review)
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

// formatDivergenceDMLine formats the trade DM line for active divergence.
// Returns "" when divergence is not active. Names the trusted window from
// TrustingWindow ("short"/"medium") rather than guessing from ResolvedDirection.
func formatDivergenceDMLine(ds *RegimeDivergenceState) string {
	if ds == nil || ds.Kind != string(DivergenceHard) || ds.ResolvedDirection == "" {
		return ""
	}
	trusting := ds.TrustingWindow
	if trusting == "" {
		trusting = "short" // backward-safe default
	}
	return fmt.Sprintf("⚠ regime divergence: medium=%s short=%s (since %d cycles, trusting %s window → %s)",
		ds.Medium, ds.Short, ds.CyclesActive, trusting, ds.ResolvedDirection)
}
