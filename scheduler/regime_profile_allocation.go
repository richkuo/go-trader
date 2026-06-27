package main

// Regime-profile allocation (#998) — slow regime switch between two validated
// profiles of one strategy.
//
// Instead of shipping one compromise default, an operator runs TWO validated
// param profiles of the same open strategy and lets a slow regime classifier
// decide which profile is active. Reference case (#976): regime_adaptive_htf —
// a fade-only profile wins grind/ranging windows, a breakout+drift-confirm
// profile dominates trending years. The two regimes are disjoint, so a slow
// switch between the profiles can beat either single default.
//
// Mechanism (HL perps only, live + paper):
//   - The classifier is the global per-cycle regime store (#879) read at a slow
//     long-window spec named by `window`. Its label is mapped through `profiles`
//     to one of the `param_sets`.
//   - Switching is HYSTERETIC and FLAT-ONLY: a switch commits only after the
//     desired profile has persisted for `confirm_bars` closed bars AND the
//     strategy is flat. While a position is open the active profile is FROZEN
//     to the profile stamped at open (pos.OpenProfile) — hold-on-transition,
//     same contract as regime_directional_policy / pos.Regime. The hysteresis
//     counter keeps accruing while open, so a regime that flipped during a long
//     hold switches on the first flat bar.
//   - The active profile's params merge OVER the base open_strategy.params on
//     the loop-local sc before the check subprocess runs, so the profile shapes
//     the signal itself (not just a post-script direction gate).
//
// Hysteresis counts CLOSED BARS, not scheduler cycles: the counter advances
// only when the regime bundle's BarTime moves, so live cadence matches the
// per-bar backtester exactly (parity, not a cycle-vs-bar approximation).
//
// Backtest parity: the switch rule is a pure function of closed-bar OHLCV, so
// backtest/backtester.py replays it (NOT rejected at --config load like
// regime_window_divergence). Validation requires each profile to clear the M1
// bar on its own regime's windows and the switched composite to beat the better
// single profile on the full multi-window table (#998 point 4).

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// regimeProfileMinConfirmBars is the foot-gun floor for confirm_bars: switching
// faster than the profiles' own signals curve-fits the regime boundary (#998 /
// M1 step-8 warning). Below this we WARN (not reject) so research can probe it.
const regimeProfileMinConfirmBars = 12

// regimeProfileExactProfiles is the M4 definition: exactly two validated
// profiles. param_sets with any other count is rejected at load.
const regimeProfileExactProfiles = 2

// RegimeProfileAllocation is the per-strategy config block. HL perps only.
type RegimeProfileAllocation struct {
	Window         string                            // long-window key in regime.windows that drives the switch
	Profiles       map[string]string                 // regime label -> profile name (must cover the window classifier's full vocabulary)
	ParamSets      map[string]map[string]interface{} // profile name -> open_strategy param overrides (exactly two)
	ConfirmBars    int                               // closed bars the desired profile must persist before a flat switch commits
	InitialProfile string                            // profile active before any switch / on cold start with no persisted active

	raw map[string]interface{}
}

// RegimeProfileState is the per-strategy mutable switch state. ActiveProfile is
// persisted (strategies.active_profile) so a flat restart keeps the last active
// profile; the pending counter is in-memory and re-arms from zero on restart
// (conservative — a restart can only DELAY a switch, never fast-track one).
type RegimeProfileState struct {
	ActiveProfile   string `json:"active_profile,omitempty"`
	PendingProfile  string `json:"pending_profile,omitempty"`
	PendingBarsSeen int    `json:"pending_bars_seen,omitempty"`
	LastBarTime     string `json:"last_bar_time,omitempty"`
}

// UnmarshalJSON captures the raw shape for strategy-scoped validation in
// LoadConfig (deferred-resolve pattern, mirrors RegimeWindowDivergence).
func (a *RegimeProfileAllocation) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("regime_profile_allocation: %w", err)
	}
	a.raw = raw
	return nil
}

// MarshalJSON renders the canonical form for hot-reload diff logging.
func (a RegimeProfileAllocation) MarshalJSON() ([]byte, error) {
	if a.IsZero() {
		return json.Marshal(a.raw)
	}
	return json.Marshal(map[string]interface{}{
		"window":          a.Window,
		"profiles":        a.Profiles,
		"param_sets":      a.ParamSets,
		"confirm_bars":    a.ConfirmBars,
		"initial_profile": a.InitialProfile,
	})
}

// IsConfigured reports whether the operator supplied any value. Safe before
// ResolveRaw (reads captured raw).
func (a *RegimeProfileAllocation) IsConfigured() bool {
	if a == nil {
		return false
	}
	if a.Window != "" || len(a.Profiles) > 0 || len(a.ParamSets) > 0 || a.InitialProfile != "" {
		return true
	}
	return len(a.raw) > 0
}

// IsZero reports whether the block is empty after resolution.
func (a *RegimeProfileAllocation) IsZero() bool {
	if a == nil {
		return true
	}
	return a.Window == "" && len(a.Profiles) == 0 && len(a.ParamSets) == 0 &&
		a.ConfirmBars == 0 && a.InitialProfile == ""
}

// EqualForReload reports shape equality for hot-reload state-compat checks. Any
// difference in window, profiles, param_sets, confirm_bars, or initial_profile
// is a reshape (blocked while open, applied when flat).
func (a *RegimeProfileAllocation) EqualForReload(other *RegimeProfileAllocation) bool {
	aZero := a == nil || a.IsZero()
	bZero := other == nil || other.IsZero()
	if aZero != bZero {
		return false
	}
	if aZero {
		return true
	}
	if a.Window != other.Window || a.ConfirmBars != other.ConfirmBars || a.InitialProfile != other.InitialProfile {
		return false
	}
	if !stringMapEqual(a.Profiles, other.Profiles) {
		return false
	}
	return paramSetsEqual(a.ParamSets, other.ParamSets)
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// paramSetsEqual compares two profile->params maps structurally via canonical
// JSON so a numeric/string param tweak counts as a reshape.
func paramSetsEqual(a, b map[string]map[string]interface{}) bool {
	if len(a) != len(b) {
		return false
	}
	for name, pa := range a {
		pb, ok := b[name]
		if !ok {
			return false
		}
		ja, errA := canonicalJSON(pa)
		jb, errB := canonicalJSON(pb)
		if errA != nil || errB != nil || ja != jb {
			return false
		}
	}
	return true
}

func canonicalJSON(m map[string]interface{}) (string, error) {
	blob, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(blob), nil
}

// regimeProfileAllocationWindow returns the configured switch window key from
// the raw or resolved block (the classifier vocabulary is keyed on it, and the
// labels are needed BEFORE ResolveRaw populates a.Window). Returns "" when
// absent or unconfigured.
func regimeProfileAllocationWindow(sc StrategyConfig) string {
	a := sc.RegimeProfileAllocation
	if a == nil {
		return ""
	}
	if a.Window != "" {
		return a.Window
	}
	if w, ok := a.raw["window"].(string); ok {
		return strings.TrimSpace(w)
	}
	return ""
}

// ResolveRaw parses the captured raw JSON into typed fields with strategy-scoped
// errors. `labels` is the full vocabulary of the configured window's classifier
// — every label must map to a profile so no regime is left unhandled. Validates:
//   - required keys: window, profiles, param_sets, confirm_bars, initial_profile
//   - exactly two param_sets entries (the M4 two-profile definition)
//   - every label in `labels` present in profiles; every profiles value and
//     initial_profile names a param_sets key
//   - confirm_bars >= 1 (WARN when < regimeProfileMinConfirmBars)
//   - no unknown keys
//
// Pass labels=nil to skip the label-coverage check (used when the classifier
// vocabulary isn't resolvable yet — window-existence validation handles that).
func (a *RegimeProfileAllocation) ResolveRaw(label string, labels []string) []string {
	var errs []string
	if a == nil || len(a.raw) == 0 {
		return errs
	}

	knownKeys := map[string]bool{
		"window":          true,
		"profiles":        true,
		"param_sets":      true,
		"confirm_bars":    true,
		"initial_profile": true,
	}
	for k := range a.raw {
		if !knownKeys[k] {
			errs = append(errs, fmt.Sprintf("%s: unknown key %q (valid: window, profiles, param_sets, confirm_bars, initial_profile)", label, k))
		}
	}

	window, werrs := rawString(a.raw, "window", label)
	errs = append(errs, werrs...)
	initialProfile, ierrs := rawString(a.raw, "initial_profile", label)
	errs = append(errs, ierrs...)

	profiles, perrs := rawStringMap(a.raw, "profiles", label)
	errs = append(errs, perrs...)
	paramSets, pserrs := rawParamSets(a.raw, "param_sets", label)
	errs = append(errs, pserrs...)

	confirmBars, cerrs := rawInt(a.raw, "confirm_bars", label)
	errs = append(errs, cerrs...)

	if len(errs) > 0 {
		return errs
	}

	if confirmBars < 1 {
		errs = append(errs, fmt.Sprintf("%s.confirm_bars: must be >= 1, got %d", label, confirmBars))
	}
	if len(paramSets) != regimeProfileExactProfiles {
		errs = append(errs, fmt.Sprintf("%s.param_sets: must define exactly %d profiles (the M4 two-profile model), got %d", label, regimeProfileExactProfiles, len(paramSets)))
	}
	// Every profiles value must name a param_sets key.
	for lbl, prof := range profiles {
		if _, ok := paramSets[prof]; !ok {
			errs = append(errs, fmt.Sprintf("%s.profiles[%q]=%q is not a param_sets profile (valid: %s)", label, lbl, prof, strings.Join(sortedKeys(paramSets), ", ")))
		}
	}
	if _, ok := paramSets[initialProfile]; !ok {
		errs = append(errs, fmt.Sprintf("%s.initial_profile=%q is not a param_sets profile (valid: %s)", label, initialProfile, strings.Join(sortedKeys(paramSets), ", ")))
	}
	// Every classifier label must be covered by profiles.
	// #1124: a present bare `ranging_directional` mapping covers its _up/_down
	// sub-labels (back-compat — the bare profile resolves the whole family at
	// runtime via resolveRegimeProfile's bare fallback, including the
	// return_eff==0 neutral case the producer emits).
	_, bareDirectional := profiles[regimeDirectionalBare]
	for _, l := range labels {
		if _, ok := profiles[l]; ok {
			continue
		}
		if regimeLabelFamilyCovered(l, bareDirectional) {
			continue
		}
		errs = append(errs, fmt.Sprintf("%s.profiles: missing mapping for regime label %q (every label of the window classifier must map to a profile)", label, l))
	}

	if len(errs) > 0 {
		return errs
	}

	a.Window = window
	a.Profiles = profiles
	a.ParamSets = paramSets
	a.ConfirmBars = confirmBars
	a.InitialProfile = initialProfile

	if confirmBars < regimeProfileMinConfirmBars {
		fmt.Printf("[WARN] %s.confirm_bars=%d is below %d — switching faster than the profiles' own signals curve-fits the regime boundary (#998). Prefer a slow long-window switch.\n",
			label, confirmBars, regimeProfileMinConfirmBars)
	}
	return errs
}

func rawString(raw map[string]interface{}, key, label string) (string, []string) {
	v, ok := raw[key]
	if !ok {
		return "", []string{fmt.Sprintf("%s: missing required key %q", label, key)}
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", []string{fmt.Sprintf("%s.%s: must be a non-empty string", label, key)}
	}
	return strings.TrimSpace(s), nil
}

func rawInt(raw map[string]interface{}, key, label string) (int, []string) {
	v, ok := raw[key]
	if !ok {
		return 0, []string{fmt.Sprintf("%s: missing required key %q", label, key)}
	}
	f, ok := v.(float64) // encoding/json decodes numbers as float64
	if !ok || f != math.Trunc(f) {
		return 0, []string{fmt.Sprintf("%s.%s: must be an integer", label, key)}
	}
	return int(f), nil
}

func rawStringMap(raw map[string]interface{}, key, label string) (map[string]string, []string) {
	v, ok := raw[key]
	if !ok {
		return nil, []string{fmt.Sprintf("%s: missing required key %q", label, key)}
	}
	m, ok := v.(map[string]interface{})
	if !ok || len(m) == 0 {
		return nil, []string{fmt.Sprintf("%s.%s: must be a non-empty object", label, key)}
	}
	out := make(map[string]string, len(m))
	var errs []string
	for k, val := range m {
		s, ok := val.(string)
		if !ok || strings.TrimSpace(s) == "" {
			errs = append(errs, fmt.Sprintf("%s.%s[%q]: must be a non-empty string", label, key, k))
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(s)
	}
	return out, errs
}

func rawParamSets(raw map[string]interface{}, key, label string) (map[string]map[string]interface{}, []string) {
	v, ok := raw[key]
	if !ok {
		return nil, []string{fmt.Sprintf("%s: missing required key %q", label, key)}
	}
	m, ok := v.(map[string]interface{})
	if !ok || len(m) == 0 {
		return nil, []string{fmt.Sprintf("%s.%s: must be a non-empty object", label, key)}
	}
	out := make(map[string]map[string]interface{}, len(m))
	var errs []string
	for k, val := range m {
		params, ok := val.(map[string]interface{})
		if !ok {
			errs = append(errs, fmt.Sprintf("%s.%s[%q]: must be a params object", label, key, k))
			continue
		}
		out[strings.TrimSpace(k)] = params
	}
	return out, errs
}

func sortedKeys(m map[string]map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolveRegimeProfile is the PURE switch state machine (unit-testable without
// Python). It returns the profile that governs THIS cycle's params plus the
// next persisted state.
//
//   - label:      the current long-window regime label (empty = bundle failure
//     / fail-open → freeze the counter, keep ActiveProfile).
//   - barTime:    the regime bundle's closed-bar timestamp; the hysteresis
//     counter advances only when this moves (closed-bar cadence).
//   - prev:       prior persisted+in-memory state (nil = cold start).
//   - posQty:     open position size; >0 freezes the active profile to posProfile
//     (hold-on-transition) and BLOCKS a switch commit, but the
//     counter still accrues so a flat bar commits immediately.
//   - posProfile: the profile stamped on the open position (pos.OpenProfile);
//     "" for legacy positions → fall back to ActiveProfile.
func resolveRegimeProfile(alloc *RegimeProfileAllocation, label, barTime string, prev *RegimeProfileState, posQty float64, posProfile string) (string, RegimeProfileState) {
	next := RegimeProfileState{}
	if prev != nil {
		next = *prev
	}
	if next.ActiveProfile == "" {
		next.ActiveProfile = alloc.InitialProfile
	}

	label = strings.TrimSpace(label)
	desired := ""
	if label != "" {
		desired = alloc.Profiles[label]
		// #1124: sub-label stamp falls back to the bare ranging_directional
		// mapping when no explicit sub-label profile is configured.
		if desired == "" && regimeDirectionalSubs[label] {
			desired = alloc.Profiles[regimeDirectionalBare]
		}
	}

	barAdvanced := barTime != "" && barTime != next.LastBarTime
	if barAdvanced {
		next.LastBarTime = barTime
		switch {
		case desired == "":
			// Fail-open / unknown label: freeze the counter, hold ActiveProfile.
		case desired == next.ActiveProfile:
			// Regime agrees with the active profile: clear any pending switch.
			next.PendingProfile = ""
			next.PendingBarsSeen = 0
		default:
			// Desired differs from active: accrue hysteresis.
			if next.PendingProfile == desired {
				next.PendingBarsSeen++
			} else {
				next.PendingProfile = desired
				next.PendingBarsSeen = 1
			}
			// Commit only when flat AND the desired profile has persisted for
			// confirm_bars. While a position is open the counter keeps growing
			// but the switch is deferred to the first flat bar.
			if posQty <= 0 && next.PendingBarsSeen >= alloc.ConfirmBars {
				next.ActiveProfile = desired
				next.PendingProfile = ""
				next.PendingBarsSeen = 0
			}
		}
	}

	active := next.ActiveProfile
	if posQty > 0 {
		// Position open: the profile is frozen to whatever opened it.
		if p := strings.TrimSpace(posProfile); p != "" {
			active = p
		}
	}
	return active, next
}

// applyRegimeProfileParams merges the active profile's param overrides OVER a
// COPY of the loop-local sc.OpenStrategy.Params. It allocates a fresh map so the
// shared cfg.Strategies params map is never mutated (the loop-local sc is a
// value copy, but its Params map field still aliases cfg). No-op when the
// profile has no param_sets entry.
func applyRegimeProfileParams(sc *StrategyConfig, alloc *RegimeProfileAllocation, profile string) {
	if sc == nil || alloc == nil {
		return
	}
	overrides, ok := alloc.ParamSets[profile]
	if !ok {
		return
	}
	merged := make(map[string]interface{}, len(sc.OpenStrategy.Params)+len(overrides))
	for k, v := range sc.OpenStrategy.Params {
		merged[k] = v
	}
	for k, v := range overrides {
		merged[k] = v
	}
	sc.OpenStrategy.Params = merged
}

// updateStrategyProfileState commits the resolved next state onto the strategy
// state after a cycle. Clears the state when the strategy has no allocation
// block configured (mirrors updateStrategyDivergenceState's unconfigured guard).
func updateStrategyProfileState(s *StrategyState, next RegimeProfileState) {
	if s == nil {
		return
	}
	cp := next
	s.RegimeProfile = &cp
}

// stampPositionProfileIfOpened stamps the active profile on the position the
// first time we observe one without a stamp, freezing the profile for the life
// of the position (hold-on-transition). No-op when the position is absent,
// already stamped, or the profile is empty.
func stampPositionProfileIfOpened(s *StrategyState, symbol, profile string) {
	if s == nil || strings.TrimSpace(profile) == "" {
		return
	}
	pos, ok := s.Positions[symbol]
	if !ok || pos == nil || strings.TrimSpace(pos.OpenProfile) != "" {
		return
	}
	pos.OpenProfile = profile
}

// strategyActiveProfile returns the persisted active-profile name for the
// strategies table, or "" when no allocation state exists.
func strategyActiveProfile(s *StrategyState) string {
	if s == nil || s.RegimeProfile == nil {
		return ""
	}
	return s.RegimeProfile.ActiveProfile
}

// formatProfileDMLine formats the trade DM line for the active profile. Returns
// "" when no allocation state is present.
func formatProfileDMLine(ps *RegimeProfileState) string {
	if ps == nil || strings.TrimSpace(ps.ActiveProfile) == "" {
		return ""
	}
	if ps.PendingProfile != "" && ps.PendingProfile != ps.ActiveProfile {
		return fmt.Sprintf("◆ regime profile: active=%s (pending %s: %d bars)", ps.ActiveProfile, ps.PendingProfile, ps.PendingBarsSeen)
	}
	return fmt.Sprintf("◆ regime profile: active=%s", ps.ActiveProfile)
}
