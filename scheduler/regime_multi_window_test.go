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
		Windows: map[string]int{"short": 168, "medium": 720, "long": 2160},
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

func TestRegimeRequiredOhlcvLimit(t *testing.T) {
	rc := &RegimeConfig{
		Enabled: true,
		Period:  14,
		Windows: map[string]int{"long": 2160},
	}
	got := regimeRequiredOhlcvLimit(rc)
	want := 2*2160 - 1 + regimeOhlcvMargin
	if got != want {
		t.Fatalf("limit = %d, want %d", got, want)
	}
}

func TestValidateRegimeWindowsConfig_RejectsWindowWithoutGlobalWindows(t *testing.T) {
	cfg := &Config{
		Regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20},
		Strategies: []StrategyConfig{{
			ID:               "hl-test",
			RegimeGateWindow: "long",
		}},
	}
	errs := validateRegimeWindowsConfig(cfg)
	if len(errs) != 1 {
		t.Fatalf("errs = %v", errs)
	}
}

func TestRegimeLabelAtOpen_PrefersStampedWindow(t *testing.T) {
	rc := &RegimeConfig{
		Enabled: true,
		Windows: map[string]int{"short": 168, "medium": 720},
	}
	pos := &Position{
		Regime: "trending_up",
		RegimeWindows: map[string]string{
			"short":  "ranging",
			"medium": "trending_down",
		},
	}
	if got := regimeLabelAtOpen(pos, "medium", rc); got != "trending_down" {
		t.Fatalf("medium = %q", got)
	}
	if got := regimeLabelAtOpen(pos, "", rc); got != "trending_down" {
		t.Fatalf("default primary = %q", got)
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

func TestValidateRegimeWindowsConfig_RejectsReservedWindowName(t *testing.T) {
	cfg := &Config{
		Regime: &RegimeConfig{
			Enabled: true,
			Windows: map[string]int{"regime": 168},
		},
	}
	errs := validateRegimeWindowsConfig(cfg)
	if len(errs) != 1 {
		t.Fatalf("errs = %v", errs)
	}
}
