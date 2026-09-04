package main

import (
	"encoding/json"
	"testing"
)

func TestRegimePayload_UnmarshalLegacyString(t *testing.T) {
	var p RegimePayload
	if err := json.Unmarshal([]byte(`"trending_up"`), &p); err != nil {
		t.Fatal(err)
	}
	if p.MultiMode || p.Legacy != "trending_up" {
		t.Fatalf("got MultiMode=%v Legacy=%q", p.MultiMode, p.Legacy)
	}
	if p.Label("gate", nil) != "trending_up" {
		t.Fatalf("Label() = %q", p.Label("gate", nil))
	}
}

func TestRegimePayload_UnmarshalMultiWindow(t *testing.T) {
	raw := `{"short":{"regime":"ranging","score":0.1,"metrics":{"adx":10}},"long":{"regime":"trending_up","score":0.8,"metrics":{"adx":40}}}`
	var p RegimePayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if !p.MultiMode {
		t.Fatal("expected multi mode")
	}
	rc := &RegimeConfig{
		Enabled: true,
		Windows: RegimeWindowsMap{"short": {Period: 168}, "medium": {Period: 720}, "long": {Period: 2160}},
	}
	if got := p.Label("short", rc); got != "ranging" {
		t.Fatalf("short label = %q", got)
	}
	if got := p.Label("long", rc); got != "trending_up" {
		t.Fatalf("long label = %q", got)
	}
	if got := p.PrimaryLabel(rc); got != "trending_up" {
		t.Fatalf("primary = %q, want trending_up from long when medium absent", got)
	}
}

func TestRegimePayload_LabelDefaultWindowNoExplicitWindows(t *testing.T) {
	var p RegimePayload
	if err := json.Unmarshal([]byte(`{"default":{"regime":"trending_down","score":0.9}}`), &p); err != nil {
		t.Fatal(err)
	}
	if !p.MultiMode {
		t.Fatal("expected multi-window payload for default-keyed result")
	}
	rc := &RegimeConfig{Enabled: true}
	for _, key := range []string{"", "default", "DEFAULT"} {
		if got := p.Label(key, rc); got != "trending_down" {
			t.Fatalf("Label(%q) = %q, want trending_down", key, got)
		}
	}
	if got := p.PrimaryLabel(rc); got != "trending_down" {
		t.Fatalf("PrimaryLabel = %q, want trending_down", got)
	}
}

func TestRegimeDirectionalPolicy_DefaultWindowResolves(t *testing.T) {
	var p RegimePayload
	if err := json.Unmarshal([]byte(`{"default":{"regime":"trending_down"}}`), &p); err != nil {
		t.Fatal(err)
	}
	rc := &RegimeConfig{Enabled: true}
	sc := StrategyConfig{
		Direction:    "long",
		InvertSignal: false,
		RegimeDirectionalPolicy: &RegimeDirectionalPolicy{TrendRegime: map[string]RegimeDirectionalEntry{
			"trending_up":   {Direction: "long", InvertSignal: false},
			"trending_down": {Direction: "short", InvertSignal: true},
			"ranging":       {Direction: "long", InvertSignal: false},
		}},
	}
	label := regimeDirectionalLabel(sc, p, rc)
	if label != "trending_down" {
		t.Fatalf("regimeDirectionalLabel = %q, want trending_down", label)
	}
	entry, applied, _ := applyRegimeDirectionalPolicy(&sc, label, "", 0, map[string]string{"trending_up": "long", "trending_down": "short", "ranging": "long"})
	if !applied {
		t.Fatal("expected policy to apply on flat default-window entry")
	}
	if entry.Direction != "short" || !entry.InvertSignal {
		t.Fatalf("entry = %+v, want short+invert", entry)
	}
	if sc.Direction != "short" || !sc.InvertSignal {
		t.Fatalf("sc not mutated: dir=%q invert=%t", sc.Direction, sc.InvertSignal)
	}
}

func TestRegimeGate_DefaultWindowBlocks(t *testing.T) {
	var p RegimePayload
	if err := json.Unmarshal([]byte(`{"default":{"regime":"trending_down"}}`), &p); err != nil {
		t.Fatal(err)
	}
	rc := &RegimeConfig{Enabled: true}
	sc := StrategyConfig{AllowedRegimes: []string{"trending_up"}}

	if got := regimeGateLabel(sc, p, rc); got != "trending_down" {
		t.Fatalf("regimeGateLabel = %q, want trending_down", got)
	}
	gateLabel, blocked := applyRegimeGate(sc, p, rc, 0)
	if !blocked {
		t.Fatalf("expected gate to block trending_down entry (allowed=trending_up); gateLabel=%q", gateLabel)
	}
	scAllowed := StrategyConfig{AllowedRegimes: []string{"trending_down"}}
	if _, blocked := applyRegimeGate(scAllowed, p, rc, 0); blocked {
		t.Fatal("expected trending_down entry to pass when allowed")
	}
}

func TestRegimeRequiredOhlcvLimit(t *testing.T) {
	rc := &RegimeConfig{
		Enabled: true,
		Period:  14,
		Windows: RegimeWindowsMap{"long": {Period: 2160}},
	}
	got := regimeRequiredOhlcvLimit(rc)
	want := 2*2160 - 1 + regimeOhlcvMargin
	if got != want {
		t.Fatalf("limit = %d, want %d", got, want)
	}
}

func TestValidateRegimeWindowsConfig_Rejections(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
	}{
		{
			name: "strategy gate window without global windows",
			cfg: &Config{
				Regime:     &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20},
				Strategies: []StrategyConfig{{ID: "hl-test", RegimeGateWindow: "long"}},
			},
		},
		{
			name: "reserved window name",
			cfg: &Config{
				Regime: &RegimeConfig{Enabled: true, Windows: RegimeWindowsMap{"regime": {Period: 168}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if errs := validateRegimeWindowsConfig(tc.cfg); len(errs) != 1 {
				t.Fatalf("errs = %v, want exactly 1", errs)
			}
		})
	}
}

func TestRegimePayload_UnmarshalWindowNamedRegime(t *testing.T) {
	raw := `{"regime":{"regime":"ranging","score":0.1,"metrics":{"adx":10}}}`
	var p RegimePayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if !p.MultiMode {
		t.Fatal("expected multi-window payload for window named regime")
	}
	if got := p.Label("regime", nil); got != "ranging" {
		t.Fatalf("label = %q", got)
	}
}

func TestStrategyDisplayRegimeLabel_Fallbacks(t *testing.T) {
	cases := []struct {
		name string
		sc   StrategyConfig
		st   *StrategyState
		want string
	}{
		{
			name: "gate window unset falls back to shared default",
			sc:   StrategyConfig{},
			st: &StrategyState{
				Regime:        "ranging",
				RegimeWindows: map[string]string{"medium": "ranging", "composite_long": "trending_down_choppy"},
			},
			want: "ranging",
		},
		{
			name: "gate window label not captured falls back",
			sc:   StrategyConfig{RegimeGateWindow: "composite_long"},
			st: &StrategyState{
				Regime:        "ranging",
				RegimeWindows: map[string]string{"medium": "ranging"},
			},
			want: "ranging",
		},
		{
			name: "nil strategy state yields empty",
			sc:   StrategyConfig{RegimeGateWindow: "composite_long"},
			st:   nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strategyDisplayRegimeLabel(tc.st, tc.sc, nil); got != tc.want {
				t.Fatalf("strategyDisplayRegimeLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStrategyDisplayRegimeLabel_DoesNotAffectATRDirectionalFallbacks(t *testing.T) {
	sc := StrategyConfig{RegimeGateWindow: "composite_long"}
	st := &StrategyState{
		Regime: "ranging",
		RegimeWindows: map[string]string{
			"medium":         "ranging",
			"composite_long": "trending_down_choppy",
		},
	}
	if got := strategyDisplayRegimeLabel(st, sc, nil); got != "trending_down_choppy" {
		t.Fatalf("display label = %q, want trending_down_choppy", got)
	}
	if got := strategyCurrentATRRegime(st, sc); got != "ranging" {
		t.Fatalf("strategyCurrentATRRegime = %q, want ranging (unaffected shared-default fallback)", got)
	}
	if got := strategyCurrentDirectionalRegime(st, sc); got != "ranging" {
		t.Fatalf("strategyCurrentDirectionalRegime = %q, want ranging (unaffected shared-default fallback)", got)
	}
}
