package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// classifyRegimeDivergence
// ---------------------------------------------------------------------------

func TestClassifyRegimeDivergence_NoneWhenSame(t *testing.T) {
	cases := []struct {
		short, medium string
	}{
		{"trending_up_clean", "trending_up_choppy"},    // both bullish
		{"trending_down", "trending_down_clean"},       // both bearish
		{"ranging_volatile", "ranging_quiet"},          // both neutral
		{"ranging_directional", "ranging_directional"}, // same
		{"", ""}, // both empty
	}
	for _, tc := range cases {
		r := classifyRegimeDivergence(tc.short, tc.medium, 0, 0, onDivergenceTrustShort)
		if r.Kind != DivergenceNone {
			t.Errorf("short=%q medium=%q: expected none, got %q", tc.short, tc.medium, r.Kind)
		}
		if r.OverrideDir != "" {
			t.Errorf("short=%q medium=%q: expected no override, got %q", tc.short, tc.medium, r.OverrideDir)
		}
	}
}

func TestClassifyRegimeDivergence_SoftWhenOneNeutral(t *testing.T) {
	cases := []struct {
		short, medium string
	}{
		{"trending_up_clean", "ranging_volatile"}, // bullish vs neutral
		{"ranging_quiet", "trending_down_choppy"}, // neutral vs bearish
	}
	for _, tc := range cases {
		r := classifyRegimeDivergence(tc.short, tc.medium, 0, 0, onDivergenceTrustShort)
		if r.Kind != DivergenceSoft {
			t.Errorf("short=%q medium=%q: expected soft, got %q", tc.short, tc.medium, r.Kind)
		}
		if r.OverrideDir != "" {
			t.Errorf("short=%q medium=%q: soft divergence should not override, got %q", tc.short, tc.medium, r.OverrideDir)
		}
	}
}

func TestClassifyRegimeDivergence_HardOppositeDirections(t *testing.T) {
	cases := []struct {
		short, medium, wantDir string
	}{
		{"trending_up_clean", "trending_down", DirectionLong},
		{"trending_up", "trending_down_choppy", DirectionLong},
		{"trending_down_clean", "trending_up", DirectionShort},
		{"trending_down", "trending_up_clean", DirectionShort},
	}
	for _, tc := range cases {
		r := classifyRegimeDivergence(tc.short, tc.medium, 0, 0, onDivergenceTrustShort)
		if r.Kind != DivergenceHard {
			t.Errorf("short=%q medium=%q: expected hard, got %q", tc.short, tc.medium, r.Kind)
		}
		if r.OverrideDir != tc.wantDir {
			t.Errorf("short=%q medium=%q trust_short: expected dir=%q, got %q", tc.short, tc.medium, tc.wantDir, r.OverrideDir)
		}
		if r.TrustingWindow != "short" {
			t.Errorf("short=%q medium=%q trust_short: expected trusting=short, got %q", tc.short, tc.medium, r.TrustingWindow)
		}
	}
}

func TestClassifyRegimeDivergence_TrustMedium(t *testing.T) {
	// short=up, medium=down, trust_medium → follow medium (short)
	r := classifyRegimeDivergence("trending_up_clean", "trending_down", 0, 0, onDivergenceTrustMedium)
	if r.Kind != DivergenceHard {
		t.Fatalf("expected hard, got %q", r.Kind)
	}
	if r.OverrideDir != DirectionShort {
		t.Errorf("trust_medium: expected short, got %q", r.OverrideDir)
	}
	if r.TrustingWindow != "medium" {
		t.Errorf("trust_medium: expected trusting=medium, got %q", r.TrustingWindow)
	}
}

func TestClassifyRegimeDivergence_AlertOnly(t *testing.T) {
	r := classifyRegimeDivergence("trending_up_clean", "trending_down", 0, 0, onDivergenceAlertOnly)
	if r.Kind != DivergenceHard {
		t.Fatalf("expected hard, got %q", r.Kind)
	}
	if r.OverrideDir != "" {
		t.Errorf("alert_only: expected no override dir, got %q", r.OverrideDir)
	}
	if r.IsActive() {
		t.Error("alert_only: IsActive should be false")
	}
}

func TestClassifyRegimeDivergence_RangingDirectionalSign(t *testing.T) {
	// ranging_directional with positive return_eff → bullish → should diverge hard against trending_down
	r := classifyRegimeDivergence("ranging_directional", "trending_down", 0.05, 0, onDivergenceTrustShort)
	if r.Kind != DivergenceHard {
		t.Fatalf("positive return_eff: expected hard, got %q", r.Kind)
	}
	if r.OverrideDir != DirectionLong {
		t.Errorf("positive return_eff: expected long, got %q", r.OverrideDir)
	}

	// negative return_eff → bearish → same direction as trending_down → none
	r2 := classifyRegimeDivergence("ranging_directional", "trending_down", -0.05, 0, onDivergenceTrustShort)
	if r2.Kind != DivergenceNone {
		t.Errorf("negative return_eff: expected none, got %q", r2.Kind)
	}

	// zero return_eff → neutral → soft against trending_down
	r3 := classifyRegimeDivergence("ranging_directional", "trending_down", 0, 0, onDivergenceTrustShort)
	if r3.Kind != DivergenceSoft {
		t.Errorf("zero return_eff: expected soft, got %q", r3.Kind)
	}

	r4 := classifyRegimeDivergence("ranging_directional_up", "trending_down", 0, 0, onDivergenceTrustShort)
	if r4.Kind != DivergenceHard || r4.OverrideDir != DirectionLong {
		t.Errorf("ranging_directional_up: got kind=%q dir=%q, want hard/long", r4.Kind, r4.OverrideDir)
	}
	r5 := classifyRegimeDivergence("ranging_directional_down", "trending_up", 0, 0, onDivergenceTrustShort)
	if r5.Kind != DivergenceHard || r5.OverrideDir != DirectionShort {
		t.Errorf("ranging_directional_down: got kind=%q dir=%q, want hard/short", r5.Kind, r5.OverrideDir)
	}
}

// ---------------------------------------------------------------------------
// RegimeWindowDivergence config + ResolveRaw
// ---------------------------------------------------------------------------

func TestRegimeWindowDivergence_ResolveRaw_Valid(t *testing.T) {
	raw := `{"short_window":"composite_short","medium_window":"composite_medium","on_divergence":"trust_short"}`
	var d RegimeWindowDivergence
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errs := d.ResolveRaw("strategy[test].regime_window_divergence")
	if len(errs) > 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
	if d.ShortWindow != "composite_short" {
		t.Errorf("ShortWindow: got %q", d.ShortWindow)
	}
	if d.MediumWindow != "composite_medium" {
		t.Errorf("MediumWindow: got %q", d.MediumWindow)
	}
	if d.OnDivergence != onDivergenceTrustShort {
		t.Errorf("OnDivergence: got %q", d.OnDivergence)
	}
}

func TestRegimeWindowDivergence_ResolveRaw_AllModes(t *testing.T) {
	for _, mode := range []string{onDivergenceTrustShort, onDivergenceTrustMedium, onDivergenceAlertOnly} {
		raw := `{"short_window":"s","medium_window":"m","on_divergence":"` + mode + `"}`
		var d RegimeWindowDivergence
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("unmarshal %q: %v", mode, err)
		}
		errs := d.ResolveRaw("test")
		if len(errs) > 0 {
			t.Errorf("mode %q: unexpected errors: %v", mode, errs)
		}
	}
}

func TestRegimeWindowDivergence_ResolveRaw_UnknownMode(t *testing.T) {
	raw := `{"short_window":"s","medium_window":"m","on_divergence":"do_nothing"}`
	var d RegimeWindowDivergence
	json.Unmarshal([]byte(raw), &d)
	errs := d.ResolveRaw("test")
	if len(errs) == 0 {
		t.Error("expected error for unknown on_divergence")
	}
}

func TestRegimeWindowDivergence_ResolveRaw_MissingKeys(t *testing.T) {
	cases := []string{
		`{"medium_window":"m","on_divergence":"trust_short"}`, // missing short_window
		`{"short_window":"s","on_divergence":"trust_short"}`,  // missing medium_window
		`{"short_window":"s","medium_window":"m"}`,            // missing on_divergence
	}
	for _, raw := range cases {
		var d RegimeWindowDivergence
		json.Unmarshal([]byte(raw), &d)
		errs := d.ResolveRaw("test")
		if len(errs) == 0 {
			t.Errorf("expected error for %s", raw)
		}
	}
}

func TestRegimeWindowDivergence_ResolveRaw_SameWindow(t *testing.T) {
	raw := `{"short_window":"composite","medium_window":"composite","on_divergence":"trust_short"}`
	var d RegimeWindowDivergence
	json.Unmarshal([]byte(raw), &d)
	errs := d.ResolveRaw("test")
	if len(errs) == 0 {
		t.Error("expected error when short_window == medium_window")
	}
}

func TestRegimeWindowDivergence_ResolveRaw_UnknownKey(t *testing.T) {
	raw := `{"short_window":"s","medium_window":"m","on_divergence":"trust_short","extra_key":"bad"}`
	var d RegimeWindowDivergence
	json.Unmarshal([]byte(raw), &d)
	errs := d.ResolveRaw("test")
	if len(errs) == 0 {
		t.Error("expected error for unknown key")
	}
}

func TestRegimeWindowDivergence_IsConfigured_IsZero(t *testing.T) {
	var nilPtr *RegimeWindowDivergence
	if nilPtr.IsConfigured() {
		t.Error("nil IsConfigured should be false")
	}
	if !nilPtr.IsZero() {
		t.Error("nil IsZero should be true")
	}

	raw := `{"short_window":"s","medium_window":"m","on_divergence":"trust_short"}`
	var d RegimeWindowDivergence
	json.Unmarshal([]byte(raw), &d)
	if !d.IsConfigured() {
		t.Error("configured block: IsConfigured should be true")
	}
	// IsZero is true before ResolveRaw (fields not set yet)
	if !d.IsZero() {
		t.Error("before ResolveRaw: IsZero should be true (raw only)")
	}
	d.ResolveRaw("test")
	if d.IsZero() {
		t.Error("after ResolveRaw: IsZero should be false")
	}
}

func TestRegimeWindowDivergence_EqualForReload(t *testing.T) {
	parse := func(raw string) *RegimeWindowDivergence {
		var d RegimeWindowDivergence
		json.Unmarshal([]byte(raw), &d)
		d.ResolveRaw("test")
		return &d
	}
	a := parse(`{"short_window":"s","medium_window":"m","on_divergence":"trust_short"}`)
	b := parse(`{"short_window":"s","medium_window":"m","on_divergence":"trust_short"}`)
	if !a.EqualForReload(b) {
		t.Error("identical blocks: EqualForReload should be true")
	}

	c := parse(`{"short_window":"s","medium_window":"m","on_divergence":"trust_medium"}`)
	if a.EqualForReload(c) {
		t.Error("different on_divergence: EqualForReload should be false")
	}

	var nilPtr *RegimeWindowDivergence
	if !nilPtr.EqualForReload(nilPtr) {
		t.Error("nil == nil should be true")
	}
	if nilPtr.EqualForReload(a) {
		t.Error("nil != configured")
	}
}

// ---------------------------------------------------------------------------
// applyRegimeDivergenceOverride
// ---------------------------------------------------------------------------

func TestApplyRegimeDivergenceOverride_MutatesFlatSC(t *testing.T) {
	// hard divergence when flat → sc.Direction mutated
	d := &RegimeWindowDivergence{ShortWindow: "short", MediumWindow: "medium", OnDivergence: onDivergenceTrustShort}
	sc := &StrategyConfig{Direction: DirectionBoth, InvertSignal: true}
	sc.RegimeWindowDivergence = d

	payload := RegimePayload{
		MultiMode: true,
		Windows: map[string]RegimeSnapshot{
			"short":  {Regime: "trending_up_clean"},
			"medium": {Regime: "trending_down"},
		},
	}
	result := applyRegimeDivergenceOverride(sc, payload, nil, 0) // posQty=0 (flat)
	if result.Kind != DivergenceHard {
		t.Fatalf("expected hard, got %q", result.Kind)
	}
	if sc.Direction != DirectionLong {
		t.Errorf("expected sc.Direction=long after override, got %q", sc.Direction)
	}
	if sc.InvertSignal {
		t.Error("expected sc.InvertSignal=false after override")
	}
}

func TestApplyRegimeDivergenceOverride_DoesNotMutateWhenOpen(t *testing.T) {
	// hard divergence but position is open → no mutation (hold-on-transition)
	d := &RegimeWindowDivergence{ShortWindow: "short", MediumWindow: "medium", OnDivergence: onDivergenceTrustShort}
	sc := &StrategyConfig{Direction: DirectionBoth}
	sc.RegimeWindowDivergence = d

	payload := RegimePayload{
		MultiMode: true,
		Windows: map[string]RegimeSnapshot{
			"short":  {Regime: "trending_up_clean"},
			"medium": {Regime: "trending_down"},
		},
	}
	result := applyRegimeDivergenceOverride(sc, payload, nil, 1.0) // posQty > 0
	if result.Kind != DivergenceHard {
		t.Fatalf("expected hard divergence detected, got %q", result.Kind)
	}
	// Direction must NOT have been mutated
	if sc.Direction != DirectionBoth {
		t.Errorf("open position: sc.Direction should be unchanged, got %q", sc.Direction)
	}
}

func TestApplyRegimeDivergenceOverride_NoOpWhenNotConfigured(t *testing.T) {
	sc := &StrategyConfig{Direction: DirectionBoth}
	payload := RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{
		"short":  {Regime: "trending_up_clean"},
		"medium": {Regime: "trending_down"},
	}}
	result := applyRegimeDivergenceOverride(sc, payload, nil, 0)
	if result.Kind != DivergenceNone {
		t.Errorf("unconfigured: expected none, got %q", result.Kind)
	}
	if sc.Direction != DirectionBoth {
		t.Error("unconfigured: sc.Direction should be unchanged")
	}
}

func TestApplyRegimeDivergenceOverride_AlertOnly_NoMutation(t *testing.T) {
	d := &RegimeWindowDivergence{ShortWindow: "short", MediumWindow: "medium", OnDivergence: onDivergenceAlertOnly}
	sc := &StrategyConfig{Direction: DirectionBoth}
	sc.RegimeWindowDivergence = d

	payload := RegimePayload{MultiMode: true, Windows: map[string]RegimeSnapshot{
		"short":  {Regime: "trending_up_clean"},
		"medium": {Regime: "trending_down"},
	}}
	applyRegimeDivergenceOverride(sc, payload, nil, 0)
	if sc.Direction != DirectionBoth {
		t.Errorf("alert_only: sc.Direction should be unchanged, got %q", sc.Direction)
	}
}

// ---------------------------------------------------------------------------
// updateStrategyDivergenceState
// ---------------------------------------------------------------------------

func TestUpdateStrategyDivergenceState_CounterIncrement(t *testing.T) {
	s := &StrategyState{}
	r := DivergenceResult{Kind: DivergenceHard, ShortLabel: "trending_up_clean", MediumLabel: "trending_down", OverrideDir: DirectionLong}

	updateStrategyDivergenceState(s, r)
	if s.RegimeDivergence == nil {
		t.Fatal("expected non-nil divergence state")
	}
	if s.RegimeDivergence.CyclesActive != 1 {
		t.Errorf("first cycle: CyclesActive=%d", s.RegimeDivergence.CyclesActive)
	}

	// Second cycle with same result → increment
	updateStrategyDivergenceState(s, r)
	if s.RegimeDivergence.CyclesActive != 2 {
		t.Errorf("second cycle: CyclesActive=%d", s.RegimeDivergence.CyclesActive)
	}

	// Direction change → reset
	r2 := DivergenceResult{Kind: DivergenceHard, ShortLabel: "trending_down", MediumLabel: "trending_up", OverrideDir: DirectionShort}
	updateStrategyDivergenceState(s, r2)
	if s.RegimeDivergence.CyclesActive != 1 {
		t.Errorf("direction change: CyclesActive should reset to 1, got %d", s.RegimeDivergence.CyclesActive)
	}
}

func TestUpdateStrategyDivergenceState_ClearsOnNone(t *testing.T) {
	s := &StrategyState{RegimeDivergence: &RegimeDivergenceState{CyclesActive: 5}}
	updateStrategyDivergenceState(s, DivergenceResult{Kind: DivergenceNone})
	if s.RegimeDivergence != nil {
		t.Error("expected RegimeDivergence cleared on none")
	}
}

// PR #916 fix 1: the zero-value DivergenceResult (Kind=="") left on
// result.Divergence for unconfigured strategies must clear state, not accrue
// a non-nil RegimeDivergence with growing CyclesActive.
func TestUpdateStrategyDivergenceState_ZeroValueClears(t *testing.T) {
	s := &StrategyState{}
	for i := 0; i < 3; i++ {
		updateStrategyDivergenceState(s, DivergenceResult{}) // Kind == ""
	}
	if s.RegimeDivergence != nil {
		t.Errorf("zero-value result must keep RegimeDivergence nil, got %+v", s.RegimeDivergence)
	}
}

// PR #916 fix 4: the DM line must name the trusted window from TrustingWindow,
// not infer "short" from ResolvedDirection.
func TestFormatDivergenceDMLine_TrustsCorrectWindow(t *testing.T) {
	ds := &RegimeDivergenceState{
		Short:             "trending_up_clean",
		Medium:            "trending_down",
		Kind:              string(DivergenceHard),
		ResolvedDirection: DirectionShort,
		TrustingWindow:    "medium",
		CyclesActive:      3,
	}
	line := formatDivergenceDMLine(ds)
	if line == "" {
		t.Fatal("expected non-empty DM line")
	}
	if !strings.Contains(line, "trusting medium window") {
		t.Errorf("expected 'trusting medium window' in DM line, got: %q", line)
	}
	if !strings.Contains(line, "→ short") {
		t.Errorf("expected resolved direction in DM line, got: %q", line)
	}
}

func TestFormatDivergenceDMLine_EmptyWhenInactive(t *testing.T) {
	if formatDivergenceDMLine(nil) != "" {
		t.Error("nil state should produce empty line")
	}
	soft := &RegimeDivergenceState{Kind: string(DivergenceSoft)}
	if formatDivergenceDMLine(soft) != "" {
		t.Error("soft divergence should produce empty line")
	}
	noDir := &RegimeDivergenceState{Kind: string(DivergenceHard)}
	if formatDivergenceDMLine(noDir) != "" {
		t.Error("hard divergence without override dir should produce empty line")
	}
}

// PR #916 fix 5: medium return_eff is resolved symmetrically, so trust_medium
// can resolve a direction when the medium window is ranging_directional.
func TestClassifyRegimeDivergence_TrustMediumRangingDirectional(t *testing.T) {
	r := classifyRegimeDivergence("trending_down", "ranging_directional", 0, 0.05, onDivergenceTrustMedium)
	if r.Kind != DivergenceHard {
		t.Fatalf("expected hard divergence, got %q", r.Kind)
	}
	if r.OverrideDir != DirectionLong {
		t.Errorf("trust_medium ranging_directional+: expected long, got %q", r.OverrideDir)
	}
}

// PR #916 fix 2: a typo'd window name must be rejected at validation time,
// not silently no-op at runtime. The existence check runs in
// validateStrategyRegimeVocabulary (after ResolveRaw), so this exercises the
// real ordering bug — validateRegimeWindowsConfig ran first with empty fields.
func TestValidateStrategyRegimeVocabulary_RejectsBadDivergenceWindow(t *testing.T) {
	mk := func(short, medium string) *RegimeWindowDivergence {
		var d RegimeWindowDivergence
		json.Unmarshal([]byte(`{"short_window":"`+short+`","medium_window":"`+medium+`","on_divergence":"trust_short"}`), &d)
		return &d
	}
	cfg := &Config{
		Regime: &RegimeConfig{
			Enabled: true,
			Windows: RegimeWindowsMap{
				"composite_short":  {Classifier: regimeClassifierComposite, Period: 50},
				"composite_medium": {Classifier: regimeClassifierComposite, Period: 200},
			},
		},
		Strategies: []StrategyConfig{{
			ID:                     "hl-test",
			RegimeWindowDivergence: mk("does_not_exist", "composite_medium"),
		}},
	}
	errs := validateStrategyRegimeVocabulary(cfg)
	if len(errs) == 0 {
		t.Fatal("expected error for non-existent short_window")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e, "does_not_exist") && strings.Contains(e, "not found in regime.windows") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected window-not-found error, got: %v", errs)
	}
}

func TestValidateStrategyRegimeVocabulary_AcceptsGoodDivergenceWindows(t *testing.T) {
	var d RegimeWindowDivergence
	json.Unmarshal([]byte(`{"short_window":"composite_short","medium_window":"composite_medium","on_divergence":"trust_short"}`), &d)
	cfg := &Config{
		Regime: &RegimeConfig{
			Enabled: true,
			Windows: RegimeWindowsMap{
				"composite_short":  {Classifier: regimeClassifierComposite, Period: 50},
				"composite_medium": {Classifier: regimeClassifierComposite, Period: 200},
			},
		},
		Strategies: []StrategyConfig{{
			ID:                     "hl-test",
			RegimeWindowDivergence: &d,
		}},
	}
	errs := validateStrategyRegimeVocabulary(cfg)
	for _, e := range errs {
		if strings.Contains(e, "regime_window_divergence") {
			t.Errorf("unexpected divergence validation error: %s", e)
		}
	}
}

// PR #916 fix 4: TrustingWindow is threaded into RegimeDivergenceState.
func TestUpdateStrategyDivergenceState_CarriesTrustingWindow(t *testing.T) {
	s := &StrategyState{}
	r := DivergenceResult{Kind: DivergenceHard, ShortLabel: "trending_up_clean", MediumLabel: "trending_down", OverrideDir: DirectionShort, TrustingWindow: "medium"}
	updateStrategyDivergenceState(s, r)
	if s.RegimeDivergence == nil || s.RegimeDivergence.TrustingWindow != "medium" {
		t.Errorf("expected TrustingWindow=medium, got %+v", s.RegimeDivergence)
	}
}
