package main

import "testing"

func TestAnchoredVWAPChannelWiring(t *testing.T) {
	if got := deriveShortName("anchored_vwap_channel"); got != "avwapch" {
		t.Fatalf("deriveShortName(anchored_vwap_channel) = %q, want avwapch", got)
	}
	if !isBidirectionalPerpsStrategy("anchored_vwap_channel") {
		t.Fatal("anchored_vwap_channel must be a bidirectional perps strategy (it shorts the upper line)")
	}
	lists := map[string][]stratDef{
		"spot":    defaultSpotStrategies,
		"perps":   defaultPerpsStrategies,
		"futures": defaultFuturesStrategies,
	}
	for name, list := range lists {
		found := false
		for _, s := range list {
			if s.ID == "anchored_vwap_channel" {
				found = true
				if s.ShortName != "avwapch" {
					t.Fatalf("%s list: anchored_vwap_channel short name = %q, want avwapch", name, s.ShortName)
				}
			}
		}
		if !found {
			t.Fatalf("anchored_vwap_channel missing from default %s list", name)
		}
	}
}

// anchored_vwap_channel fades both edges of an AVWAP channel — range-edge mean
// reversion whose edge depends on ranging conditions, so init pre-gates it to
// the composite quiet/volatile ranging substates exactly like atr_band_revert
// (ranging_directional stays excluded: directional pressure inside a range is
// the breakout precursor, mean reversion's worst case).

func TestGenerateConfig_AnchoredVWAPChannel_DefaultsToCompositeRangingGate(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC"}
	opts.SpotStrategies = []string{"anchored_vwap_channel"}

	cfg := generateConfig(opts)

	sc, ok := findStrategy(cfg, "avwapch-btc")
	if !ok {
		t.Fatalf("expected strategy avwapch-btc, got %v", cfg.Strategies)
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

func TestGenerateConfig_AnchoredVWAPChannel_Perps_DefaultsToCompositeRangingGate(t *testing.T) {
	opts := baseOpts()
	opts.Assets = []string{"BTC"}
	opts.EnableSpot = false
	opts.EnablePerps = true
	opts.PerpsStrategies = []string{"anchored_vwap_channel"}

	cfg := generateConfig(opts)

	sc, ok := findStrategy(cfg, "hl-avwapch-btc")
	if !ok {
		t.Fatalf("expected strategy hl-avwapch-btc, got %v", cfg.Strategies)
	}
	if len(sc.AllowedRegimes) == 0 {
		t.Fatalf("perps anchored_vwap_channel should be regime-gated, got none")
	}
	if cfg.Regime == nil || !cfg.Regime.Enabled {
		t.Fatalf("expected cfg.Regime enabled for perps anchored_vwap_channel")
	}
	if err := validateConfig(cfg, true); err != nil {
		t.Fatalf("generated perps config failed validation: %v", err)
	}
}
