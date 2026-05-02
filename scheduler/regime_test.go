package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// ─── ValidateConfig — AllowedRegimes ─────────────────────────────────────────

func TestValidateConfig_AllowedRegimes_AcceptsEmpty(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Strategies[0].AllowedRegimes = []string{}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("empty AllowedRegimes should be valid, got: %v", err)
	}
}

func TestValidateConfig_AllowedRegimes_AcceptsNil(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Strategies[0].AllowedRegimes = nil
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("nil AllowedRegimes should be valid, got: %v", err)
	}
}

func TestValidateConfig_AllowedRegimes_AcceptsValidLabels(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Strategies[0].AllowedRegimes = []string{"trending_up", "trending_down"}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("valid labels should pass, got: %v", err)
	}
}

func TestValidateConfig_AllowedRegimes_RejectsUnknownLabel(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Strategies[0].AllowedRegimes = []string{"trending_up", "bullish_breakout"}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatal("unknown regime label should fail validation")
	}
}

func TestValidateConfig_AllowedRegimes_AcceptsAllThreeLabels(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Strategies[0].AllowedRegimes = []string{"trending_up", "trending_down", "ranging"}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("all three valid labels should pass, got: %v", err)
	}
}

// ─── RegimeConfig validation ──────────────────────────────────────────────────

func TestValidateConfig_RegimeConfig_NilIsValid(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Regime = nil
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("nil Regime should be valid, got: %v", err)
	}
}

func TestValidateConfig_RegimeConfig_ValidEnabled(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("valid RegimeConfig should pass, got: %v", err)
	}
}

func TestValidateConfig_RegimeConfig_ZeroPeriodInvalid(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Regime = &RegimeConfig{Enabled: true, Period: 0, ADXThreshold: 20.0}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatal("Period=0 should fail validation")
	}
}

func TestValidateConfig_RegimeConfig_NegativePeriodInvalid(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Regime = &RegimeConfig{Enabled: true, Period: -1, ADXThreshold: 20.0}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatal("negative Period should fail validation")
	}
}

func TestValidateConfig_RegimeConfig_ZeroThresholdInvalid(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 0}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatal("ADXThreshold=0 should fail validation")
	}
}

func TestValidateConfig_RegimeConfig_ThresholdOver100Invalid(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 101}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatal("ADXThreshold>100 should fail validation")
	}
}

// ─── regimeAllowsEntry ────────────────────────────────────────────────────────

func TestRegimeAllowsEntry_EmptyAllowedAlwaysTrue(t *testing.T) {
	if !regimeAllowsEntry(nil, "ranging") {
		t.Error("nil AllowedRegimes should always allow entry")
	}
	if !regimeAllowsEntry([]string{}, "trending_up") {
		t.Error("empty AllowedRegimes should always allow entry")
	}
}

func TestRegimeAllowsEntry_MatchingLabel(t *testing.T) {
	allowed := []string{"trending_up", "trending_down"}
	if !regimeAllowsEntry(allowed, "trending_up") {
		t.Error("trending_up should be allowed")
	}
	if !regimeAllowsEntry(allowed, "trending_down") {
		t.Error("trending_down should be allowed")
	}
}

func TestRegimeAllowsEntry_NonMatchingLabel(t *testing.T) {
	allowed := []string{"trending_up", "trending_down"}
	if regimeAllowsEntry(allowed, "ranging") {
		t.Error("ranging should be blocked")
	}
}

func TestRegimeAllowsEntry_EmptyCurrentAllowsWhenListNonEmpty(t *testing.T) {
	// When regime field is empty (script disabled / not available), allow entry
	// so existing strategies without regime are unaffected.
	allowed := []string{"trending_up"}
	if !regimeAllowsEntry(allowed, "") {
		t.Error("empty regime string (script did not compute regime) should not block entry")
	}
}

// ─── regimeBlocksOpen — close legs always pass through ──────────────────────

func TestRegimeBlocksOpen_BlocksOpenWhenNoPosition(t *testing.T) {
	allowed := []string{"trending_up"}
	if !regimeBlocksOpen(allowed, "ranging", 0) {
		t.Error("regime mismatch with posQty=0 should block the open")
	}
}

func TestRegimeBlocksOpen_AllowsOpenWhenRegimeMatches(t *testing.T) {
	allowed := []string{"trending_up"}
	if regimeBlocksOpen(allowed, "trending_up", 0) {
		t.Error("matching regime should not block")
	}
}

func TestRegimeBlocksOpen_NeverBlocksWhenPositionExists(t *testing.T) {
	// Regression for review point 1 (#546): close legs must pass through the
	// regime gate even when the current regime is not in the allowed list.
	// Otherwise a long-then-ranging scenario would silently skip the close
	// signal, contradicting "existing positions are always managed by close
	// paths regardless".
	allowed := []string{"trending_up"}
	if regimeBlocksOpen(allowed, "ranging", 1.0) {
		t.Error("close leg (posQty>0) must never be blocked by regime gate")
	}
	if regimeBlocksOpen(allowed, "trending_down", 0.5) {
		t.Error("close leg (posQty>0) must never be blocked even on opposite regime")
	}
	if regimeBlocksOpen(allowed, "", 1.0) {
		// Empty current regime is also "allow"; combined with posQty>0 this is
		// doubly safe but we still assert it.
		t.Error("close leg (posQty>0) must never be blocked when regime is empty")
	}
}

func TestRegimeBlocksOpen_EmptyAllowedNeverBlocks(t *testing.T) {
	if regimeBlocksOpen(nil, "ranging", 0) {
		t.Error("nil allowed list (no gate configured) must never block")
	}
	if regimeBlocksOpen([]string{}, "ranging", 0) {
		t.Error("empty allowed list (no gate configured) must never block")
	}
}

// ─── StrategyDecisionFields includes Regime ───────────────────────────────────

func TestStrategyDecisionFields_RegimeRoundTrip(t *testing.T) {
	sdf := StrategyDecisionFields{Regime: "trending_up"}
	b, err := json.Marshal(sdf)
	if err != nil {
		t.Fatal(err)
	}
	var out StrategyDecisionFields
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Regime != "trending_up" {
		t.Errorf("expected trending_up, got %q", out.Regime)
	}
}

func TestStrategyDecisionFields_RegimeOmitEmpty(t *testing.T) {
	sdf := StrategyDecisionFields{}
	b, err := json.Marshal(sdf)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["regime"]; ok {
		t.Error("empty Regime should be omitted from JSON")
	}
}

// ─── Config version bump ──────────────────────────────────────────────────────

func TestCurrentConfigVersion_IsEleven(t *testing.T) {
	if CurrentConfigVersion != 11 {
		t.Errorf("expected CurrentConfigVersion=11, got %d", CurrentConfigVersion)
	}
}

// ─── ValidateConfig — AllowedRegimes on options strategies ───────────────────

func TestValidateConfig_AllowedRegimes_RejectsOnOptions(t *testing.T) {
	cfg := minimalOptionsConfig()
	cfg.Strategies[0].AllowedRegimes = []string{"trending_up"}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatal("allowed_regimes on options strategy should fail validation (gate not wired)")
	}
}

func TestValidateConfig_AllowedRegimes_AcceptsEmptyOnOptions(t *testing.T) {
	cfg := minimalOptionsConfig()
	cfg.Strategies[0].AllowedRegimes = nil
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("nil allowed_regimes on options strategy should be valid, got: %v", err)
	}
}

func TestValidateConfig_AllowedRegimes_AcceptsOnSpotWithRegimeEnabled(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0}
	cfg.Strategies[0].AllowedRegimes = []string{"trending_up"}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("allowed_regimes on spot with regime enabled should pass, got: %v", err)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns what was printed.
func captureStdout(fn func()) string {
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestValidateConfig_AllowedRegimes_WarnsWhenRegimeDisabled(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Regime = nil // disabled
	cfg.Strategies[0].AllowedRegimes = []string{"trending_up"}
	var out string
	out = captureStdout(func() {
		_ = ValidateConfig(&cfg)
	})
	if !strings.Contains(out, "[WARN]") || !strings.Contains(out, "regime.enabled=false") {
		t.Fatalf("expected [WARN] about regime.enabled=false, got: %q", out)
	}
}

func TestValidateConfig_AllowedRegimes_NoWarnWhenRegimeEnabled(t *testing.T) {
	cfg := minimalSpotConfig()
	cfg.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0}
	cfg.Strategies[0].AllowedRegimes = []string{"trending_up"}
	out := captureStdout(func() {
		_ = ValidateConfig(&cfg)
	})
	if strings.Contains(out, "regime.enabled=false") {
		t.Fatalf("unexpected [WARN] about regime.enabled=false when regime is enabled, got: %q", out)
	}
}

// ─── hot-reload: AllowedRegimes is soft, Regime is restart-required ───────────

func TestHotReload_AllowedRegimesChangeIsAccepted(t *testing.T) {
	cfg := minimalSpotConfig()
	next := minimalSpotConfig()
	next.Strategies[0].AllowedRegimes = []string{"trending_up"}
	// validateHotReloadCompatible only checks shape, not per-strategy soft fields
	if err := validateHotReloadCompatible(&cfg, &next); err != nil {
		t.Fatalf("AllowedRegimes change should be compatible with hot-reload, got: %v", err)
	}
}

func TestHotReload_RegimeConfigChangeRequiresRestart(t *testing.T) {
	cfg := minimalSpotConfig()
	next := minimalSpotConfig()
	next.Regime = &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20.0}
	if err := validateHotReloadCompatible(&cfg, &next); err == nil {
		t.Fatal("Regime config change should require restart")
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func minimalSpotConfig() Config {
	return Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{
				ID:             "test-spot-1",
				Type:           "spot",
				Platform:       "binanceus",
				Script:         "shared_scripts/check_strategy.py",
				Args:           []string{"sma_crossover", "BTC/USDT", "1h"},
				Capital:        1000,
				MaxDrawdownPct: 10,
			},
		},
	}
}

func minimalOptionsConfig() Config {
	return Config{
		IntervalSeconds: 60,
		Strategies: []StrategyConfig{
			{
				ID:             "test-options-1",
				Type:           "options",
				Platform:       "deribit",
				Script:         "shared_scripts/check_options.py",
				Args:           []string{"sell_covered_call", "BTC", "--platform=deribit"},
				Capital:        1000,
				MaxDrawdownPct: 10,
			},
		},
	}
}
