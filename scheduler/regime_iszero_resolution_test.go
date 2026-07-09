package main

import (
	"strings"
	"testing"
)

// #1111 regression suite. Several config-load-time validators read a regime ATR
// block (trailing_stop_atr_regime / stop_loss_atr_regime) via IsZero() BEFORE
// the block has been resolved. RegimeATRBlock.UnmarshalJSON captures only raw
// JSON; the typed TrendRegime/UseDefaults fields are populated later, by
// validateRegimeATRConfig. IsZero() returns true on an unresolved-but-configured
// block, so a pre-resolution !IsZero() read treats a configured block as absent.
// The blocks below are constructed raw-only (no resolved TrendRegime) to
// reproduce that exact production state — a struct with TrendRegime already set
// would not exercise the bug, which is why it shipped.

const (
	minMoveRequiresErr = "trailing_stop_min_move_pct requires"
	regimeStopMutexErr = "stop_loss_atr_regime is mutually exclusive with trailing_stop_atr_regime"
)

// adx3StateATR builds a trend_regime raw map covering the 3 canonical ADX labels
// with the given ATR multiplier — the shape RegimeATRBlock.UnmarshalJSON leaves
// in b.raw before ResolveSurface runs.
func adx3StateATR(atr float64) map[string]interface{} {
	tr := make(map[string]interface{}, len(canonicalTrendRegimeLabels))
	for _, l := range canonicalTrendRegimeLabels {
		tr[l] = map[string]interface{}{"atr_multiple": atr}
	}
	return map[string]interface{}{"trend_regime": tr}
}

// adxRegimeCfg wraps strategies in a config with ADX regime detection enabled so
// regime-aware stop blocks resolve against the canonical 3-state vocabulary.
func adxRegimeCfg(scs ...StrategyConfig) *Config {
	return &Config{
		IntervalSeconds: 60,
		Regime: &RegimeConfig{
			Enabled:      true,
			Period:       14,
			ADXThreshold: 20,
			Windows: RegimeWindowsMap{
				"daily": {Classifier: regimeClassifierADX, Period: 14},
			},
		},
		Strategies:    scs,
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
	}
}

// Instance 1 — false reject. trailing_stop_min_move_pct must be accepted when
// the strategy supplies trailing_stop_atr_regime, even though at the min-move
// check the block is still raw. Covers both the explicit trend_regime shape and
// the use_defaults shape (both leave a non-empty raw, empty TrendRegime).
func TestConfigValidation_MinMoveAcceptsUnresolvedRegimeTrail(t *testing.T) {
	minMove := 0.1
	for _, tc := range []struct {
		name string
		raw  map[string]interface{}
	}{
		{"explicit trend_regime", adx3StateATR(2.0)},
		{"use_defaults", map[string]interface{}{"use_defaults": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{
				ID:                     "hl-rmc-eth-live",
				Type:                   "perps",
				Platform:               "hyperliquid",
				Script:                 "shared_scripts/check_hyperliquid.py",
				Capital:                1000,
				MaxDrawdownPct:         10,
				Leverage:               10,
				MarginMode:             "isolated",
				RegimeATRWindow:        "daily",
				TrailingStopATRRegime:  &RegimeATRBlock{raw: tc.raw},
				TrailingStopMinMovePct: &minMove,
			}
			err := validateConfig(adxRegimeCfg(sc), false)
			if err != nil && strings.Contains(err.Error(), minMoveRequiresErr) {
				t.Fatalf("trailing_stop_min_move_pct wrongly rejected alongside trailing_stop_atr_regime: %v", err)
			}
		})
	}
}

// Instance 1 inverse — the fix must not turn the validator into a no-op. A
// strategy with trailing_stop_min_move_pct but no trailing mode at all must
// still be rejected.
func TestConfigValidation_MinMoveStillRequiresATrailingMode(t *testing.T) {
	minMove := 0.1
	sc := StrategyConfig{
		ID:                     "hl-rmc-eth-live",
		Type:                   "perps",
		Platform:               "hyperliquid",
		Script:                 "shared_scripts/check_hyperliquid.py",
		Capital:                1000,
		MaxDrawdownPct:         10,
		Leverage:               10,
		MarginMode:             "isolated",
		TrailingStopMinMovePct: &minMove,
	}
	err := validateConfig(adxRegimeCfg(sc), false)
	if err == nil || !strings.Contains(err.Error(), minMoveRequiresErr) {
		t.Fatalf("trailing_stop_min_move_pct without any trailing mode must be rejected, got: %v", err)
	}
}

// Instance 2 — false accept. stop_loss_atr_regime and trailing_stop_atr_regime
// are mutually exclusive, but the sole enforcement read the trailing block via
// IsZero() before it was resolved, so the mutex never fired when both were set.
func TestValidateRegimeATRConfig_RegimeStopMutexEnforced(t *testing.T) {
	sc := StrategyConfig{
		ID:                    "hl-test",
		Type:                  "perps",
		Platform:              "hyperliquid",
		RegimeATRWindow:       "daily",
		StopLossATRRegime:     &RegimeATRBlock{raw: adx3StateATR(2.0)},
		TrailingStopATRRegime: &RegimeATRBlock{raw: adx3StateATR(2.0)},
	}
	errs := validateRegimeATRConfig(adxRegimeCfg(sc))
	found := false
	for _, e := range errs {
		if strings.Contains(e, regimeStopMutexErr) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("config with both stop_loss_atr_regime and trailing_stop_atr_regime must be rejected as mutually exclusive, got: %v", errs)
	}
}

// Instance 2 control — a single regime stop (only trailing) must NOT trip the
// mutex; guards against the fix over-firing.
func TestValidateRegimeATRConfig_SingleRegimeStopNoMutex(t *testing.T) {
	sc := StrategyConfig{
		ID:                    "hl-test",
		Type:                  "perps",
		Platform:              "hyperliquid",
		RegimeATRWindow:       "daily",
		TrailingStopATRRegime: &RegimeATRBlock{raw: adx3StateATR(2.0)},
	}
	for _, e := range validateRegimeATRConfig(adxRegimeCfg(sc)) {
		if strings.Contains(e, regimeStopMutexErr) {
			t.Fatalf("single trailing_stop_atr_regime must not trip the stop mutex, got: %v", e)
		}
	}
}
