package main

import (
	"strings"
	"testing"
)

const (
	minMoveRequiresErr = "trailing_stop_min_move_pct requires"
	regimeStopMutexErr = "stop_loss_atr_regime is mutually exclusive with trail_stop_atr_regime"
)

func adx3StateATR(atr float64) map[string]interface{} {
	tr := make(map[string]interface{}, len(canonicalTrendRegimeLabels))
	for _, l := range canonicalTrendRegimeLabels {
		tr[l] = map[string]interface{}{"atr_multiple": atr}
	}
	return map[string]interface{}{"trend_regime": tr}
}

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
				TrailStopATRRegime:     &RegimeATRBlock{raw: tc.raw},
				TrailingStopMinMovePct: &minMove,
			}
			err := validateConfig(adxRegimeCfg(sc), false)
			if err != nil && strings.Contains(err.Error(), minMoveRequiresErr) {
				t.Fatalf("trailing_stop_min_move_pct wrongly rejected alongside trail_stop_atr_regime: %v", err)
			}
		})
	}
}

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

func TestValidateRegimeATRConfig_RegimeStopMutexEnforced(t *testing.T) {
	sc := StrategyConfig{
		ID:                 "hl-test",
		Type:               "perps",
		Platform:           "hyperliquid",
		RegimeATRWindow:    "daily",
		StopLossATRRegime:  &RegimeATRBlock{raw: adx3StateATR(2.0)},
		TrailStopATRRegime: &RegimeATRBlock{raw: adx3StateATR(2.0)},
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
		t.Fatalf("config with both stop_loss_atr_regime and trail_stop_atr_regime must be rejected as mutually exclusive, got: %v", errs)
	}
}

func TestValidateRegimeATRConfig_SingleRegimeStopNoMutex(t *testing.T) {
	sc := StrategyConfig{
		ID:                 "hl-test",
		Type:               "perps",
		Platform:           "hyperliquid",
		RegimeATRWindow:    "daily",
		TrailStopATRRegime: &RegimeATRBlock{raw: adx3StateATR(2.0)},
	}
	for _, e := range validateRegimeATRConfig(adxRegimeCfg(sc)) {
		if strings.Contains(e, regimeStopMutexErr) {
			t.Fatalf("single trail_stop_atr_regime must not trip the stop mutex, got: %v", e)
		}
	}
}
