package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyRegimeDivergence(t *testing.T) {
	cases := []struct {
		name         string
		short        string
		medium       string
		shortEff     float64
		mediumEff    float64
		mode         string
		wantKind     DivergenceKind
		wantDir      string
		wantTrusting string
		wantInactive bool
	}{
		{name: "same clean/choppy trend", short: "trending_up_clean", medium: "trending_up_choppy", mode: onDivergenceTrustShort, wantKind: DivergenceNone},
		{name: "same down trend", short: "trending_down", medium: "trending_down_clean", mode: onDivergenceTrustShort, wantKind: DivergenceNone},
		{name: "both ranging", short: "ranging_volatile", medium: "ranging_quiet", mode: onDivergenceTrustShort, wantKind: DivergenceNone},
		{name: "both ranging_directional", short: "ranging_directional", medium: "ranging_directional", mode: onDivergenceTrustShort, wantKind: DivergenceNone},
		{name: "both empty", short: "", medium: "", mode: onDivergenceTrustShort, wantKind: DivergenceNone},

		{name: "soft short trend vs ranging medium", short: "trending_up_clean", medium: "ranging_volatile", mode: onDivergenceTrustShort, wantKind: DivergenceSoft},
		{name: "soft ranging short vs trend medium", short: "ranging_quiet", medium: "trending_down_choppy", mode: onDivergenceTrustShort, wantKind: DivergenceSoft},

		{name: "hard up_clean vs down trusts short", short: "trending_up_clean", medium: "trending_down", mode: onDivergenceTrustShort, wantKind: DivergenceHard, wantDir: DirectionLong, wantTrusting: "short"},
		{name: "hard up vs down_choppy trusts short", short: "trending_up", medium: "trending_down_choppy", mode: onDivergenceTrustShort, wantKind: DivergenceHard, wantDir: DirectionLong, wantTrusting: "short"},
		{name: "hard down_clean vs up trusts short", short: "trending_down_clean", medium: "trending_up", mode: onDivergenceTrustShort, wantKind: DivergenceHard, wantDir: DirectionShort, wantTrusting: "short"},
		{name: "hard down vs up_clean trusts short", short: "trending_down", medium: "trending_up_clean", mode: onDivergenceTrustShort, wantKind: DivergenceHard, wantDir: DirectionShort, wantTrusting: "short"},

		{name: "trust_medium flips override to medium side", short: "trending_up_clean", medium: "trending_down", mode: onDivergenceTrustMedium, wantKind: DivergenceHard, wantDir: DirectionShort, wantTrusting: "medium"},
		{name: "alert_only never overrides", short: "trending_up_clean", medium: "trending_down", mode: onDivergenceAlertOnly, wantKind: DivergenceHard, wantInactive: true},

		{name: "ranging_directional positive return_eff is hard long", short: "ranging_directional", medium: "trending_down", shortEff: 0.05, mode: onDivergenceTrustShort, wantKind: DivergenceHard, wantDir: DirectionLong, wantTrusting: "short"},
		{name: "ranging_directional negative return_eff agrees", short: "ranging_directional", medium: "trending_down", shortEff: -0.05, mode: onDivergenceTrustShort, wantKind: DivergenceNone},
		{name: "ranging_directional zero return_eff is soft", short: "ranging_directional", medium: "trending_down", mode: onDivergenceTrustShort, wantKind: DivergenceSoft},
		{name: "explicit ranging_directional_up vs down is hard long", short: "ranging_directional_up", medium: "trending_down", mode: onDivergenceTrustShort, wantKind: DivergenceHard, wantDir: DirectionLong, wantTrusting: "short"},
		{name: "explicit ranging_directional_down vs down agrees", short: "ranging_directional_down", medium: "trending_down", shortEff: 0.99, mode: onDivergenceTrustShort, wantKind: DivergenceNone},
		{name: "explicit ranging_directional_down vs up is hard short", short: "ranging_directional_down", medium: "trending_up", mode: onDivergenceTrustShort, wantKind: DivergenceHard, wantDir: DirectionShort, wantTrusting: "short"},
		{name: "trust_medium ranging_directional positive medium eff", short: "trending_down", medium: "ranging_directional", mediumEff: 0.05, mode: onDivergenceTrustMedium, wantKind: DivergenceHard, wantDir: DirectionLong, wantTrusting: "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := classifyRegimeDivergence(tc.short, tc.medium, tc.shortEff, tc.mediumEff, tc.mode)
			if r.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", r.Kind, tc.wantKind)
			}
			if r.OverrideDir != tc.wantDir {
				t.Errorf("OverrideDir = %q, want %q", r.OverrideDir, tc.wantDir)
			}
			if tc.wantTrusting != "" && r.TrustingWindow != tc.wantTrusting {
				t.Errorf("TrustingWindow = %q, want %q", r.TrustingWindow, tc.wantTrusting)
			}
			if tc.wantInactive && r.IsActive() {
				t.Error("IsActive should be false")
			}
		})
	}
}

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
		`{"medium_window":"m","on_divergence":"trust_short"}`,
		`{"short_window":"s","on_divergence":"trust_short"}`,
		`{"short_window":"s","medium_window":"m"}`,
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

func TestApplyRegimeDivergenceOverride_MutatesFlatSC(t *testing.T) {
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
	result := applyRegimeDivergenceOverride(sc, payload, nil, 0)
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
	result := applyRegimeDivergenceOverride(sc, payload, nil, 1.0)
	if result.Kind != DivergenceHard {
		t.Fatalf("expected hard divergence detected, got %q", result.Kind)
	}
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

	updateStrategyDivergenceState(s, r)
	if s.RegimeDivergence.CyclesActive != 2 {
		t.Errorf("second cycle: CyclesActive=%d", s.RegimeDivergence.CyclesActive)
	}

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

func TestUpdateStrategyDivergenceState_ZeroValueClears(t *testing.T) {
	s := &StrategyState{}
	for i := 0; i < 3; i++ {
		updateStrategyDivergenceState(s, DivergenceResult{})
	}
	if s.RegimeDivergence != nil {
		t.Errorf("zero-value result must keep RegimeDivergence nil, got %+v", s.RegimeDivergence)
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

func TestUpdateStrategyDivergenceState_CarriesTrustingWindow(t *testing.T) {
	s := &StrategyState{}
	r := DivergenceResult{Kind: DivergenceHard, ShortLabel: "trending_up_clean", MediumLabel: "trending_down", OverrideDir: DirectionShort, TrustingWindow: "medium"}
	updateStrategyDivergenceState(s, r)
	if s.RegimeDivergence == nil || s.RegimeDivergence.TrustingWindow != "medium" {
		t.Errorf("expected TrustingWindow=medium, got %+v", s.RegimeDivergence)
	}
}
