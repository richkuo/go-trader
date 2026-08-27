package main

import "testing"

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
	if sc.RegimeGateOnFailure != RegimeGateOnFailureClosed {
		t.Fatalf("regime_gate_on_failure = %q, want %q", sc.RegimeGateOnFailure, RegimeGateOnFailureClosed)
	}

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

	if vErrs := validateStrategyRegimeVocabulary(cfg); len(vErrs) != 0 {
		t.Fatalf("regime vocabulary errors: %v", vErrs)
	}
	if err := validateConfig(cfg, true); err != nil {
		t.Fatalf("generated config failed validation: %v", err)
	}
}

func TestGenerateConfig_WithoutATRBandRevert_NoForcedRegime(t *testing.T) {
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
