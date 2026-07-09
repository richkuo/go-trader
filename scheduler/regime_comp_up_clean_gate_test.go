package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ─── #1197: comp_up_clean_p21 live wiring for the breakout futures strategy ──
//
// The #1165 evidence (backtest/candidates/breakout_1165/) validated gating
// breakout futures entries on composite `trending_up_clean` at classifier
// period 21. These tests pin the exact operator config that wiring uses and
// the gate semantics the evidence relied on (entries blocked, closes always
// execute), so the deployed shape can never silently drift from what was
// validated.

func compUpCleanP21FuturesConfig() Config {
	return Config{
		IntervalSeconds: 60,
		Regime: &RegimeConfig{
			Enabled:      true,
			Period:       14, // loadConfig default; the gate reads the p21 window below
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

// The full wiring from the issue — composite medium window at period 21,
// regime_gate_window pointing at it, allowed_regimes carrying the composite
// label — must validate as one piece.
func TestConfigValidation_CompUpCleanP21Wiring_Accepts(t *testing.T) {
	cfg := compUpCleanP21FuturesConfig()
	if err := validateConfig(&cfg, false); err != nil {
		t.Fatalf("comp_up_clean_p21 wiring should validate, got: %v", err)
	}
}

// The realistic deployment shape: a live config that already carries an ADX
// "medium" window for other strategies adds the p21 composite window under its
// own key. With regime_gate_window pointing at it the pairing validates…
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

// …and the touch-set trap: omit regime_gate_window in that same config and the
// gate resolves the PRIMARY window ("medium" when present — here the ADX one),
// whose 3-label vocabulary lacks `trending_up_clean`. Config load must reject
// loudly, never half-apply. (With the composite window itself named "medium"
// and no ADX sibling, primary resolution happens to land on it — the explicit
// regime_gate_window is what makes the wiring deployment-order independent.)
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

// A composite label on an ADX window spec must also reject when the gate
// window is named but carries the wrong classifier — the pairing is
// (label, window classifier), not just window existence.
func TestConfigValidation_CompUpCleanP21Wiring_RejectsADXClassifierWindow(t *testing.T) {
	cfg := compUpCleanP21FuturesConfig()
	cfg.Regime.Windows = RegimeWindowsMap{
		"medium": {Classifier: "adx", Period: 21},
	}
	if err := validateConfig(&cfg, false); err == nil {
		t.Fatal("trending_up_clean against an adx-classifier gate window should fail validation")
	}
}

// Gate semantics the #1165 evidence relied on (README structural note: the
// regime gate blocks ENTRIES only; closes always execute). Flat + label
// outside the allowed set blocks; flat + trending_up_clean passes; an open
// position is never blocked, whatever the label, so close/manage paths run.
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

	// Every non-clean composite label blocks a flat entry.
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

	// The validated label admits the entry.
	if gateLabel, blocked := applyRegimeGate(sc, payloadFor("trending_up_clean"), rc, 0); blocked {
		t.Fatalf("flat entry under trending_up_clean should pass, gateLabel=%q", gateLabel)
	}

	// Closes always execute: with a position open the gate never blocks, even
	// under a label the entry side would reject.
	if _, blocked := applyRegimeGate(sc, payloadFor("trending_down_choppy"), rc, 1.5); blocked {
		t.Fatal("open position must never be gate-blocked (closes always execute)")
	}
}
