package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func compUpCleanP21FuturesConfig() Config {
	return Config{
		IntervalSeconds: 60,
		Regime: &RegimeConfig{
			Enabled:      true,
			Period:       14,
			ADXThreshold: 20,
			Windows: RegimeWindowsMap{
				"medium": {Classifier: "composite", Period: 21},
			},
		},
		Strategies: []StrategyConfig{
			{
				ID:               "ts-breakout-btc",
				Type:             "futures",
				Platform:         "topstep",
				Script:           "shared_scripts/check_topstep.py",
				Args:             []string{"breakout", "BTC", "1h"},
				Capital:          5000,
				MaxDrawdownPct:   10,
				RegimeGateWindow: "medium",
				AllowedRegimes:   []string{"trending_up_clean"},
			},
		},
	}
}

func TestConfigValidation_CompUpCleanP21Wiring_Accepts(t *testing.T) {
	cfg := compUpCleanP21FuturesConfig()
	if err := validateConfig(&cfg, false); err != nil {
		t.Fatalf("comp_up_clean_p21 wiring should validate, got: %v", err)
	}
}

func TestConfigValidation_CompUpCleanP21Wiring_AcceptsAlongsideExistingADXWindow(t *testing.T) {
	cfg := compUpCleanP21FuturesConfig()
	cfg.Regime.Windows = RegimeWindowsMap{
		"medium":   {Classifier: "adx", Period: 14, ADXThreshold: 20},
		"comp_p21": {Classifier: "composite", Period: 21},
	}
	cfg.Strategies[0].RegimeGateWindow = "comp_p21"
	if err := validateConfig(&cfg, false); err != nil {
		t.Fatalf("comp_p21 gate window alongside an ADX medium window should validate, got: %v", err)
	}
}

func TestConfigValidation_CompUpCleanP21Wiring_RejectsWithoutGateWindow(t *testing.T) {
	cfg := compUpCleanP21FuturesConfig()
	cfg.Regime.Windows = RegimeWindowsMap{
		"medium":   {Classifier: "adx", Period: 14, ADXThreshold: 20},
		"comp_p21": {Classifier: "composite", Period: 21},
	}
	cfg.Strategies[0].RegimeGateWindow = ""
	err := validateConfig(&cfg, false)
	if err == nil {
		t.Fatal("trending_up_clean against the primary ADX window should fail validation")
	}
	if !strings.Contains(err.Error(), "trending_up_clean") {
		t.Fatalf("error should name the invalid label, got: %v", err)
	}
}

func TestConfigValidation_CompUpCleanP21Wiring_RejectsADXClassifierWindow(t *testing.T) {
	cfg := compUpCleanP21FuturesConfig()
	cfg.Regime.Windows = RegimeWindowsMap{
		"medium": {Classifier: "adx", Period: 21},
	}
	if err := validateConfig(&cfg, false); err == nil {
		t.Fatal("trending_up_clean against an adx-classifier gate window should fail validation")
	}
}

func TestApplyRegimeGate_CompUpCleanP21_EntriesBlockedClosesPass(t *testing.T) {
	cfg := compUpCleanP21FuturesConfig()
	sc := cfg.Strategies[0]
	rc := cfg.Regime

	payloadFor := func(label string) RegimePayload {
		var p RegimePayload
		raw := `{"medium":{"regime":"` + label + `"}}`
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("payload unmarshal: %v", err)
		}
		return p
	}

	for _, label := range []string{
		"trending_up_choppy", "trending_down_clean", "trending_down_choppy",
		"ranging_quiet", "ranging_volatile", "ranging_directional",
		"ranging_directional_up", "ranging_directional_down",
	} {
		gateLabel, blocked := applyRegimeGate(sc, payloadFor(label), rc, 0)
		if !blocked {
			t.Errorf("flat entry under %q should be blocked, gateLabel=%q", label, gateLabel)
		}
	}

	if gateLabel, blocked := applyRegimeGate(sc, payloadFor("trending_up_clean"), rc, 0); blocked {
		t.Fatalf("flat entry under trending_up_clean should pass, gateLabel=%q", gateLabel)
	}

	if _, blocked := applyRegimeGate(sc, payloadFor("trending_down_choppy"), rc, 1.5); blocked {
		t.Fatal("open position must never be gate-blocked (closes always execute)")
	}
}
