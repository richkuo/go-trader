package main

import "testing"

// atr_band_revert is a ranging mean-reversion strategy whose edge depends on the
// regime filter, so init wires it to the composite (7-state) ranging gate by
// default: a composite "medium" regime window plus allowed_regimes restricted to
// the quiet/volatile ranging substates (ranging_directional is excluded — that
// substate is the range most likely about to break into a trend).

func findStrategy(cfg *Config, id string) (StrategyConfig, bool) {
	for _, s := range cfg.Strategies {
		if s.ID == id {
			return s, true
		}
	}
	return StrategyConfig{}, false
}

func TestGenerateConfig_ATRBandRevert_DefaultsToCompositeRangingGate(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC"}
	opts.SpotStrategies = []string{"atr_band_revert"}

	cfg := generateConfig(opts)

	// 1. The strategy is gated to the composite ranging family.
	sc, ok := findStrategy(cfg, "abr-btc")
	if !ok {
		t.Fatalf("expected strategy abr-btc, got %v", cfg.Strategies)
	}
	want := []string{"ranging_quiet", "ranging_volatile"}
	if len(sc.AllowedRegimes) != len(want) {
		t.Fatalf("allowed_regimes = %v, want %v", sc.AllowedRegimes, want)
	}
	for i, l := range want {
		if sc.AllowedRegimes[i] != l {
			t.Fatalf("allowed_regimes[%d] = %q, want %q", i, sc.AllowedRegimes[i], l)
		}
	}
	// #1278: newly generated gated configs get the conservative fail-closed
	// entry-gate failure policy (existing configs keep the fail-open default).
	if sc.RegimeGateOnFailure != RegimeGateOnFailureClosed {
		t.Fatalf("regime_gate_on_failure = %q, want %q", sc.RegimeGateOnFailure, RegimeGateOnFailureClosed)
	}

	// 2. A composite "medium" regime window is enabled globally.
	if cfg.Regime == nil || !cfg.Regime.Enabled {
		t.Fatalf("expected cfg.Regime enabled, got %+v", cfg.Regime)
	}
	win, ok := cfg.Regime.Windows["medium"]
	if !ok {
		t.Fatalf("expected a composite 'medium' window, got windows %+v", cfg.Regime.Windows)
	}
	if win.effectiveClassifier() != regimeClassifierComposite {
		t.Fatalf("medium window classifier = %q, want composite", win.effectiveClassifier())
	}

	// 3. The generated config passes validation, incl. regime vocabulary
	//    (composite labels resolve against the composite primary window).
	if vErrs := validateStrategyRegimeVocabulary(cfg); len(vErrs) != 0 {
		t.Fatalf("regime vocabulary errors: %v", vErrs)
	}
	if err := validateConfig(cfg, true); err != nil {
		t.Fatalf("generated config failed validation: %v", err)
	}
}

func TestGenerateConfig_WithoutATRBandRevert_NoForcedRegime(t *testing.T) {
	// A config that doesn't select atr_band_revert must NOT silently enable
	// global regime detection.
	opts := baseOpts()
	opts.Assets = []string{"BTC"}
	opts.SpotStrategies = []string{"momentum"}

	cfg := generateConfig(opts)

	if cfg.Regime != nil && cfg.Regime.Enabled {
		t.Fatalf("regime should stay disabled when atr_band_revert is not selected, got %+v", cfg.Regime)
	}
	sc, ok := findStrategy(cfg, "momentum-btc")
	if !ok {
		t.Fatalf("expected momentum-btc")
	}
	if len(sc.AllowedRegimes) != 0 {
		t.Fatalf("momentum should not be regime-gated, got %v", sc.AllowedRegimes)
	}
}

func TestGenerateConfig_ATRBandRevert_Perps_DefaultsToCompositeRangingGate(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC"}
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.PerpsStrategies = []string{"atr_band_revert"}

	cfg := generateConfig(opts)

	sc, ok := findStrategy(cfg, "hl-abr-btc")
	if !ok {
		t.Fatalf("expected strategy hl-abr-btc, got %v", cfg.Strategies)
	}
	if len(sc.AllowedRegimes) == 0 {
		t.Fatalf("perps atr_band_revert should be regime-gated, got none")
	}
	if cfg.Regime == nil || !cfg.Regime.Enabled {
		t.Fatalf("expected cfg.Regime enabled for perps atr_band_revert")
	}
	if err := validateConfig(cfg, true); err != nil {
		t.Fatalf("generated perps config failed validation: %v", err)
	}
}
