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

// TestRegimePayload_LabelDefaultWindowNoExplicitWindows is the #797 regression:
// with regime.enabled=true and no explicit regime.windows, the check script
// emits a single-window payload keyed by "default". The default selector
// (empty / "default") must resolve that literal key instead of no-op'ing to an
// empty label, which previously disabled both regime_directional_policy and the
// allowed_regimes gate on live entries.
func TestRegimePayload_LabelDefaultWindowNoExplicitWindows(t *testing.T) {
	var p RegimePayload
	if err := json.Unmarshal([]byte(`{"default":{"regime":"trending_down","score":0.9}}`), &p); err != nil {
		t.Fatal(err)
	}
	if !p.MultiMode {
		t.Fatal("expected multi-window payload for default-keyed result")
	}
	rc := &RegimeConfig{Enabled: true} // no explicit Windows — issue config shape
	for _, key := range []string{"", "default", "DEFAULT"} {
		if got := p.Label(key, rc); got != "trending_down" {
			t.Fatalf("Label(%q) = %q, want trending_down", key, got)
		}
	}
	if got := p.PrimaryLabel(rc); got != "trending_down" {
		t.Fatalf("PrimaryLabel = %q, want trending_down", got)
	}
}

// TestRegimeDirectionalPolicy_DefaultWindowResolves wires the full flat-entry
// resolution path from #797: default-keyed payload + no explicit windows +
// regime_directional_policy must flip a long base config to short+invert.
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

// TestRegimeGate_DefaultWindowBlocks covers the second #797 consumer: the
// allowed_regimes gate shares RegimePayload.Label, so a default-keyed payload
// with no explicit windows must still produce a non-empty label and block an
// entry whose regime is not allowed (previously failed open).
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
	// Allowed regime must pass.
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
			Windows: RegimeWindowsMap{"regime": {Period: 168}},
		},
	}
	errs := validateRegimeWindowsConfig(cfg)
	if len(errs) != 1 {
		t.Fatalf("errs = %v", errs)
	}
}

// #1189: the operator-facing display label must match the strategy's
// configured gate window, not the shared-default window used for
// stratState.Regime.
func TestStrategyDisplayRegimeLabel_UsesGateWindowOverride(t *testing.T) {
	sc := StrategyConfig{RegimeGateWindow: "composite_long"}
	st := &StrategyState{
		Regime: "ranging", // shared-default ("medium") window label
		RegimeWindows: map[string]string{
			"medium":         "ranging",
			"composite_long": "trending_down_choppy",
		},
	}
	if got := strategyDisplayRegimeLabel(st, sc, nil); got != "trending_down_choppy" {
		t.Fatalf("strategyDisplayRegimeLabel = %q, want trending_down_choppy", got)
	}
}

func TestStrategyDisplayRegimeLabel_FallsBackWhenGateWindowUnset(t *testing.T) {
	sc := StrategyConfig{} // no regime_gate_window override
	st := &StrategyState{
		Regime:        "ranging",
		RegimeWindows: map[string]string{"medium": "ranging", "composite_long": "trending_down_choppy"},
	}
	if got := strategyDisplayRegimeLabel(st, sc, nil); got != "ranging" {
		t.Fatalf("strategyDisplayRegimeLabel = %q, want ranging (fallback to shared default)", got)
	}
}

func TestStrategyDisplayRegimeLabel_FallsBackWhenWindowLabelMissing(t *testing.T) {
	sc := StrategyConfig{RegimeGateWindow: "composite_long"}
	st := &StrategyState{
		Regime:        "ranging",
		RegimeWindows: map[string]string{"medium": "ranging"}, // composite_long not populated yet
	}
	if got := strategyDisplayRegimeLabel(st, sc, nil); got != "ranging" {
		t.Fatalf("strategyDisplayRegimeLabel = %q, want ranging (fallback, gate label not yet captured)", got)
	}
}

func TestStrategyDisplayRegimeLabel_NilStratState(t *testing.T) {
	sc := StrategyConfig{RegimeGateWindow: "composite_long"}
	if got := strategyDisplayRegimeLabel(nil, sc, nil); got != "" {
		t.Fatalf("strategyDisplayRegimeLabel = %q, want empty", got)
	}
}

// #1189 regression: a strategy that overrides ONLY regime_gate_window must
// keep resolving its ATR/directional fallbacks (and therefore the #822
// orphan-close and dynamic-regime-close tier resolution) from the shared
// stratState.Regime — strategyDisplayRegimeLabel must never be substituted
// into those call sites.
func TestStrategyDisplayRegimeLabel_DoesNotAffectATRDirectionalFallbacks(t *testing.T) {
	sc := StrategyConfig{RegimeGateWindow: "composite_long"} // ATR/directional windows left unset
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
