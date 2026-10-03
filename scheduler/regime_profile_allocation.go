package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

const regimeProfileMinConfirmBars = 12

const regimeProfileExactProfiles = 2

type RegimeProfileAllocation struct {
	Window         string
	Profiles       map[string]string
	ParamSets      map[string]map[string]interface{}
	ConfirmBars    int
	InitialProfile string

	raw map[string]interface{}
}

type RegimeProfileState struct {
	ActiveProfile   string `json:"active_profile,omitempty"`
	PendingProfile  string `json:"pending_profile,omitempty"`
	PendingBarsSeen int    `json:"pending_bars_seen,omitempty"`
	LastBarTime     string `json:"last_bar_time,omitempty"`
}

func (a *RegimeProfileAllocation) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("regime_profile_allocation: %w", err)
	}
	a.raw = raw
	return nil
}

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

func (a *RegimeProfileAllocation) IsConfigured() bool {
	if a == nil {
		return false
	}
	if a.Window != "" || len(a.Profiles) > 0 || len(a.ParamSets) > 0 || a.InitialProfile != "" {
		return true
	}
	return len(a.raw) > 0
}

func (a *RegimeProfileAllocation) IsZero() bool {
	if a == nil {
		return true
	}
	return a.Window == "" && len(a.Profiles) == 0 && len(a.ParamSets) == 0 &&
		a.ConfirmBars == 0 && a.InitialProfile == ""
}

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
	for lbl, prof := range profiles {
		if _, ok := paramSets[prof]; !ok {
			errs = append(errs, fmt.Sprintf("%s.profiles[%q]=%q is not a param_sets profile (valid: %s)", label, lbl, prof, strings.Join(sortedKeys(paramSets), ", ")))
		}
	}
	if _, ok := paramSets[initialProfile]; !ok {
		errs = append(errs, fmt.Sprintf("%s.initial_profile=%q is not a param_sets profile (valid: %s)", label, initialProfile, strings.Join(sortedKeys(paramSets), ", ")))
	}
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
	f, ok := v.(float64)
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
		if desired == "" && regimeDirectionalSubs[label] {
			desired = alloc.Profiles[regimeDirectionalBare]
		}
	}

	barAdvanced := barTime != "" && barTime != next.LastBarTime
	if barAdvanced {
		next.LastBarTime = barTime
		switch {
		case desired == "":
		case desired == next.ActiveProfile:
			next.PendingProfile = ""
			next.PendingBarsSeen = 0
		default:
			if next.PendingProfile == desired {
				next.PendingBarsSeen++
			} else {
				next.PendingProfile = desired
				next.PendingBarsSeen = 1
			}
			if posQty <= 0 && next.PendingBarsSeen >= alloc.ConfirmBars {
				next.ActiveProfile = desired
				next.PendingProfile = ""
				next.PendingBarsSeen = 0
			}
		}
	}

	active := next.ActiveProfile
	if posQty > 0 {
		if p := strings.TrimSpace(posProfile); p != "" {
			active = p
		}
	}
	return active, next
}

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

func updateStrategyProfileState(s *StrategyState, next RegimeProfileState) {
	if s == nil {
		return
	}
	cp := next
	s.RegimeProfile = &cp
}

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

func strategyActiveProfile(s *StrategyState) string {
	if s == nil || s.RegimeProfile == nil {
		return ""
	}
	return s.RegimeProfile.ActiveProfile
}

func formatProfileDMLine(ps *RegimeProfileState) string {
	if ps == nil || strings.TrimSpace(ps.ActiveProfile) == "" {
		return ""
	}
	if ps.PendingProfile != "" && ps.PendingProfile != ps.ActiveProfile {
		return fmt.Sprintf("◆ regime profile: active=%s (pending %s: %d bars)", ps.ActiveProfile, ps.PendingProfile, ps.PendingBarsSeen)
	}
	return fmt.Sprintf("◆ regime profile: active=%s", ps.ActiveProfile)
}
