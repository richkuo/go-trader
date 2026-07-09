package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #1115: type=manual HL strategies default their close evaluator to
// trailing_tp_ratchet_regime when regime detection is enabled and a default
// per-regime trail block resolves for the active classifier vocabulary, falling
// back to tiered_tp_atr_live otherwise. The synthesized trailing_stop_atr_regime
// block becomes the SL owner (no scalar stop_loss_atr_mult conflict), and the
// position opens with an armed initial SL (no naked window). These tests cover
// both modes, both classifier vocabularies, the override-wins cases, and the
// no-naked-SL invariant (a resolvable opening trail for every active label).

func writeRatchetRegimeTestConfig(t *testing.T, dir, body string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(body), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

func manualStrategyByID(cfg *Config, id string) (StrategyConfig, bool) {
	for _, sc := range cfg.Strategies {
		if sc.ID == id {
			return sc, true
		}
	}
	return StrategyConfig{}, false
}

// Regime DISABLED: unchanged behavior — tiered_tp_atr_live + scalar manual SL,
// and no synthesized regime trail block.
func TestManualDefault_RegimeDisabled_KeepsTieredTPATRLive(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	cfg, err := LoadConfig(writeRatchetRegimeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	sc, ok := manualStrategyByID(cfg, "hl-manual-eth-live")
	if !ok {
		t.Fatal("strategy not found")
	}
	if sc.CloseStrategy == nil || sc.CloseStrategy.Name != "tiered_tp_atr_live" {
		t.Fatalf("CloseStrategy = %v, want tiered_tp_atr_live (regime off)", sc.CloseStrategy)
	}
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != defaultManualStopLossATRMult {
		t.Fatalf("StopLossATRMult = %v, want %g scalar default", sc.StopLossATRMult, defaultManualStopLossATRMult)
	}
	if sc.TrailingStopATRRegime.IsConfigured() {
		t.Fatal("TrailingStopATRRegime must not be synthesized when regime is off")
	}
}

// Regime ENABLED (ADX): defaults to trailing_tp_ratchet_regime with a synthesized
// 3-label trail block, no scalar SL, and passes the full validation pipeline. The
// no-naked invariant: every active label resolves a positive opening trail.
func TestManualDefault_RegimeADX_SelectsRatchetRegime(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	cfgJSON := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	cfg, err := LoadConfig(writeRatchetRegimeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig (regime ADX): %v", err)
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	sc, _ := manualStrategyByID(cfg, "hl-manual-eth-live")
	if sc.CloseStrategy == nil || sc.CloseStrategy.Name != trailingTPRatchetRegimeCloseName {
		t.Fatalf("CloseStrategy = %v, want %s", sc.CloseStrategy, trailingTPRatchetRegimeCloseName)
	}
	if sc.StopLossATRMult != nil {
		t.Fatalf("StopLossATRMult = %v, want nil (the regime block owns the SL)", *sc.StopLossATRMult)
	}
	block := sc.TrailingStopATRRegime
	if block == nil || len(block.TrendRegime) != 3 {
		t.Fatalf("trailing_stop_atr_regime must resolve to 3 ADX labels, got %#v", block)
	}
	// No-naked invariant: an opening trail resolves for every active ADX label.
	for _, label := range []string{"trending_up", "trending_down", "ranging"} {
		if v, ok := resolveRegimeATR(*block, label); !ok || v <= 0 {
			t.Fatalf("opening trail for %q = (%g, %v), want positive", label, v, ok)
		}
	}
}

// Regime ENABLED (composite): defaults to trailing_tp_ratchet_regime with a
// synthesized 7-label trail block; every composite label resolves a positive
// opening trail and the config validates.
func TestManualDefault_RegimeComposite_SelectsRatchetRegime(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	cfgJSON := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"regime": {
			"enabled": true, "period": 14, "adx_threshold": 20,
			"windows": {
				"daily": {"classifier": "composite", "period": 24, "thresholds": {"return_eff": 0.05, "range_eff": 0.03, "adx": 20}}
			}
		},
		"strategies": [{
			"id": "hl-manual-btc-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "BTC",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20,
			"regime_atr_window": "daily"
		}]
	}`
	cfg, err := LoadConfig(writeRatchetRegimeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig (regime composite): %v", err)
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	sc, _ := manualStrategyByID(cfg, "hl-manual-btc-live")
	if sc.CloseStrategy == nil || sc.CloseStrategy.Name != trailingTPRatchetRegimeCloseName {
		t.Fatalf("CloseStrategy = %v, want %s", sc.CloseStrategy, trailingTPRatchetRegimeCloseName)
	}
	if sc.StopLossATRMult != nil {
		t.Fatalf("StopLossATRMult = %v, want nil", *sc.StopLossATRMult)
	}
	block := sc.TrailingStopATRRegime
	if block == nil || len(block.TrendRegime) != 9 {
		t.Fatalf("trailing_stop_atr_regime must resolve to 9 composite labels, got %#v", block)
	}
	for _, label := range []string{
		"trending_up_clean", "trending_up_choppy", "trending_down_clean",
		"trending_down_choppy", "ranging_quiet", "ranging_volatile", "ranging_directional",
		"ranging_directional_up", "ranging_directional_down",
	} {
		if v, ok := resolveRegimeATR(*block, label); !ok || v <= 0 {
			t.Fatalf("opening trail for composite label %q = (%g, %v), want positive", label, v, ok)
		}
	}
}

// Override-wins: an explicit close_strategy on a regime-enabled config is
// preserved (the ratchet default never overrides operator intent).
func TestManualDefault_ExplicitCloseStrategyWins(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	cfgJSON := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20,
			"close_strategy": {"name": "tiered_tp_atr_live"}
		}]
	}`
	cfg, err := LoadConfig(writeRatchetRegimeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	sc, _ := manualStrategyByID(cfg, "hl-manual-eth-live")
	if sc.CloseStrategy == nil || sc.CloseStrategy.Name != "tiered_tp_atr_live" {
		t.Fatalf("CloseStrategy = %v, want explicit tiered_tp_atr_live preserved", sc.CloseStrategy)
	}
	if sc.TrailingStopATRRegime.IsConfigured() {
		t.Fatal("explicit close_strategy must not get a synthesized regime trail block")
	}
}

// Override-wins (stop field): an explicit scalar stop on a regime-enabled config
// (with no close_strategy) keeps tiered_tp_atr_live so the operator's stop is not
// invalidated by the ratchet's no-scalar-stop rule.
func TestManualDefault_ExplicitStopFieldKeepsTiered(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	cfgJSON := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20,
			"stop_loss_atr_mult": 2.5
		}]
	}`
	cfg, err := LoadConfig(writeRatchetRegimeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	sc, _ := manualStrategyByID(cfg, "hl-manual-eth-live")
	if sc.CloseStrategy == nil || sc.CloseStrategy.Name != "tiered_tp_atr_live" {
		t.Fatalf("CloseStrategy = %v, want tiered_tp_atr_live (explicit stop field set)", sc.CloseStrategy)
	}
	if sc.StopLossATRMult == nil || *sc.StopLossATRMult != 2.5 {
		t.Fatalf("StopLossATRMult = %v, want explicit 2.5 preserved", sc.StopLossATRMult)
	}
	if sc.TrailingStopATRRegime.IsConfigured() {
		t.Fatal("explicit stop field must not get a synthesized regime trail block")
	}
}

// Operator-tunable: user_defaults.manual.trailing_stop_atr_regime supplies the
// per-regime opening trail in place of the use_defaults baseline.
func TestManualDefault_ManualDefaultsTrailBlockOverride(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	cfgJSON := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_defaults": {
			"manual": {
				"trailing_stop_atr_regime": {
					"trend_regime": {
						"trending_up": {"atr_multiple": 3.0},
						"trending_down": {"atr_multiple": 3.0},
						"ranging": {"atr_multiple": 1.5}
					}
				}
			}
		},
		"strategies": [{
			"id": "hl-manual-eth-live",
			"type": "manual",
			"platform": "hyperliquid",
			"symbol": "ETH",
			"timeframe": "1h",
			"capital": 1000,
			"leverage": 20,
			"max_drawdown_pct": 20
		}]
	}`
	cfg, err := LoadConfig(writeRatchetRegimeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	sc, _ := manualStrategyByID(cfg, "hl-manual-eth-live")
	if sc.CloseStrategy == nil || sc.CloseStrategy.Name != trailingTPRatchetRegimeCloseName {
		t.Fatalf("CloseStrategy = %v, want %s", sc.CloseStrategy, trailingTPRatchetRegimeCloseName)
	}
	block := sc.TrailingStopATRRegime
	if block == nil {
		t.Fatal("trailing_stop_atr_regime must be synthesized from user_defaults.manual override")
	}
	if v, ok := resolveRegimeATR(*block, "trending_up"); !ok || v != 3.0 {
		t.Fatalf("operator-tuned trending_up trail = (%g, %v), want (3.0, true)", v, ok)
	}
	if v, ok := resolveRegimeATR(*block, "ranging"); !ok || v != 1.5 {
		t.Fatalf("operator-tuned ranging trail = (%g, %v), want (1.5, true)", v, ok)
	}
}

// The user_defaults.manual override block must not alias across strategies: each
// adopting strategy resolves an independent copy (cloneRegimeATRBlock).
func TestManualDefault_ManualDefaultsTrailBlockNotAliased(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	cfgJSON := `{
		"db_file": "` + strings.ReplaceAll(dbPath, "\\", "\\\\") + `",
		"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
		"user_defaults": {
			"manual": {
				"trailing_stop_atr_regime": {"use_defaults": true}
			}
		},
		"strategies": [
			{"id": "hl-manual-eth", "type": "manual", "platform": "hyperliquid", "symbol": "ETH", "timeframe": "1h", "capital": 1000, "leverage": 20, "max_drawdown_pct": 20},
			{"id": "hl-manual-btc", "type": "manual", "platform": "hyperliquid", "symbol": "BTC", "timeframe": "1h", "capital": 1000, "leverage": 20, "max_drawdown_pct": 20}
		]
	}`
	cfg, err := LoadConfig(writeRatchetRegimeTestConfig(t, dir, cfgJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := validateConfig(cfg, false); err != nil {
		t.Fatalf("validateConfig: %v", err)
	}
	eth, _ := manualStrategyByID(cfg, "hl-manual-eth")
	btc, _ := manualStrategyByID(cfg, "hl-manual-btc")
	if eth.TrailingStopATRRegime == btc.TrailingStopATRRegime {
		t.Fatal("the two strategies must not share the same *RegimeATRBlock pointer")
	}
	for _, sc := range []StrategyConfig{eth, btc} {
		if sc.CloseStrategy == nil || sc.CloseStrategy.Name != trailingTPRatchetRegimeCloseName {
			t.Fatalf("%s CloseStrategy = %v, want %s", sc.ID, sc.CloseStrategy, trailingTPRatchetRegimeCloseName)
		}
		if v, ok := resolveRegimeATR(*sc.TrailingStopATRRegime, "ranging"); !ok || v <= 0 {
			t.Fatalf("%s ranging trail = (%g, %v), want positive", sc.ID, v, ok)
		}
	}
}

// The no-naked invariant lives in the pure resolve-or-fallback decision: a
// manual-open under trailing_tp_ratchet_regime must ALWAYS arm a strictly-positive
// SL distance — the per-regime trail when the label resolves one, else the
// protective configured fallback. (The subprocess regime read is split out into the
// impure resolveManualRatchetRegimeLabel; this tests the safety-critical branch.)
func TestManualRatchetOpeningTrailOrFallback(t *testing.T) {
	block := &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
		"trending_up": {ATR: 2.5},
		"ranging":     {ATR: 1.0},
	}}
	cases := []struct {
		name       string
		block      *RegimeATRBlock
		label      string
		fallback   float64
		wantMult   float64
		wantFellBk bool
	}{
		{"resolvable label → per-regime trail", block, "trending_up", 2.25, 2.5, false},
		{"resolvable ranging label → per-regime trail", block, "ranging", 2.25, 1.0, false},
		{"empty label (regime read failed) → configured fallback", block, "", 2.25, 2.25, true},
		{"label with no configured trail → configured fallback", block, "trending_down", 2.25, 2.25, true},
		{"nil block → configured fallback", nil, "trending_up", 2.25, 2.25, true},
		{"non-positive configured fallback → hardcoded fallback", block, "", 0, defaultManualStopLossATRMult, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mult, fellBack := manualRatchetOpeningTrailOrFallback(tc.block, tc.label, tc.fallback)
			if mult != tc.wantMult || fellBack != tc.wantFellBk {
				t.Fatalf("manualRatchetOpeningTrailOrFallback(%v, %q, %g) = (%g, %v), want (%g, %v)",
					tc.block, tc.label, tc.fallback, mult, fellBack, tc.wantMult, tc.wantFellBk)
			}
			// Invariant: the armed distance is NEVER <= 0 (never naked).
			if mult <= 0 {
				t.Fatalf("armed mult = %g, must be strictly positive (no-naked invariant)", mult)
			}
		})
	}
}

// A position opened under a tiered-TP close (resting TP OIDs) whose strategy now
// resolves to the trailing ratchet is the #1115 close-evaluator drift that the
// daemon must alert on. A ratchet-opened position (no TP OIDs) and a still-tiered
// strategy must NOT trip it.
func TestManualCloseEvaluatorDriftedFromTPs(t *testing.T) {
	ratchet := StrategyConfig{CloseStrategy: &StrategyRef{Name: trailingTPRatchetRegimeCloseName}}
	tiered := StrategyConfig{CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"}}
	withTPs := &Position{TPOIDs: []int64{111, 222}}
	noTPs := &Position{}

	if !manualCloseEvaluatorDriftedFromTPs(ratchet, withTPs) {
		t.Error("ratchet strategy + resting TP OIDs must report drift")
	}
	if manualCloseEvaluatorDriftedFromTPs(ratchet, noTPs) {
		t.Error("ratchet strategy + no TP OIDs (ratchet-opened) must NOT report drift")
	}
	if manualCloseEvaluatorDriftedFromTPs(tiered, withTPs) {
		t.Error("still-tiered strategy must NOT report drift (no evaluator change)")
	}
	if manualCloseEvaluatorDriftedFromTPs(ratchet, nil) {
		t.Error("nil position must NOT report drift")
	}
}
