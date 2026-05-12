package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRegimeATRBlock_UseDefaultsExpandsToBaseline(t *testing.T) {
	raw := map[string]interface{}{"use_defaults": true}
	got, errs := parseRegimeATRBlock(raw, "stop_loss_atr_regime", regimeSurfaceStopLoss)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !got.UseDefaults {
		t.Fatalf("UseDefaults should be true after expansion")
	}
	for _, label := range canonicalTrendRegimeLabels {
		entry, ok := got.TrendRegime[label]
		if !ok {
			t.Fatalf("default expansion missing label %q", label)
		}
		if entry.ATR <= 0 {
			t.Fatalf("default %s.atr must be > 0, got %g", label, entry.ATR)
		}
	}
	// ranging should differ from trending_up per baseline table.
	if got.TrendRegime["ranging"].ATR == got.TrendRegime["trending_up"].ATR {
		t.Fatalf("ranging should differ from trending_up in stop_loss defaults")
	}
}

func TestParseRegimeATRBlock_RejectsBareLabelKeys(t *testing.T) {
	// Bare labels without the trend_regime wrapper must be rejected.
	raw := map[string]interface{}{
		"trending_up":   map[string]interface{}{"atr": 2.0},
		"trending_down": map[string]interface{}{"atr": 2.0},
		"ranging":       map[string]interface{}{"atr": 1.5},
	}
	_, errs := parseRegimeATRBlock(raw, "stop_loss_atr_regime", regimeSurfaceStopLoss)
	if len(errs) == 0 {
		t.Fatalf("expected errors for bare label keys")
	}
	// At least one error should mention the classifier wrapper.
	found := false
	for _, e := range errs {
		if strings.Contains(e, regimeClassifierKey) || strings.Contains(e, "unknown key") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected error mentioning classifier wrapper, got: %v", errs)
	}
}

func TestParseRegimeATRBlock_RequiresExhaustiveLabels(t *testing.T) {
	raw := map[string]interface{}{
		regimeClassifierKey: map[string]interface{}{
			"trending_up": map[string]interface{}{"atr": 2.0},
			"ranging":     map[string]interface{}{"atr": 1.5},
		},
	}
	_, errs := parseRegimeATRBlock(raw, "stop_loss_atr_regime", regimeSurfaceStopLoss)
	if len(errs) == 0 {
		t.Fatalf("expected missing-label error")
	}
	missing := false
	for _, e := range errs {
		if strings.Contains(e, "missing required regime labels") && strings.Contains(e, "trending_down") {
			missing = true
		}
	}
	if !missing {
		t.Fatalf("expected error mentioning trending_down missing, got: %v", errs)
	}
}

func TestParseRegimeATRBlock_RejectsUseDefaultsAndExplicit(t *testing.T) {
	raw := map[string]interface{}{
		"use_defaults": true,
		regimeClassifierKey: map[string]interface{}{
			"trending_up":   map[string]interface{}{"atr": 2.0},
			"trending_down": map[string]interface{}{"atr": 2.0},
			"ranging":       map[string]interface{}{"atr": 1.5},
		},
	}
	_, errs := parseRegimeATRBlock(raw, "stop_loss_atr_regime", regimeSurfaceStopLoss)
	if len(errs) == 0 {
		t.Fatalf("expected mutex error")
	}
}

func TestParseRegimeATRBlock_RejectsCloseFractionOnStopLossSurface(t *testing.T) {
	raw := map[string]interface{}{
		regimeClassifierKey: map[string]interface{}{
			"trending_up":   map[string]interface{}{"atr": 2.0, "close_fraction": 0.5},
			"trending_down": map[string]interface{}{"atr": 2.0},
			"ranging":       map[string]interface{}{"atr": 1.5},
		},
	}
	_, errs := parseRegimeATRBlock(raw, "stop_loss_atr_regime", regimeSurfaceStopLoss)
	if len(errs) == 0 {
		t.Fatalf("expected error for close_fraction on stop_loss surface")
	}
}

func TestParseRegimeTPTiers_RejectsMixedShape(t *testing.T) {
	raw := []interface{}{
		map[string]interface{}{
			regimeClassifierKey: map[string]interface{}{
				"trending_up":   map[string]interface{}{"atr": 3.0, "close_fraction": 0.4},
				"trending_down": map[string]interface{}{"atr": 3.0, "close_fraction": 0.4},
				"ranging":       map[string]interface{}{"atr": 1.5, "close_fraction": 0.6},
			},
			"close_fraction": 0.5,
		},
	}
	_, errs := parseRegimeTPTiers(raw, "tiered_tp_atr_regime")
	if len(errs) == 0 {
		t.Fatalf("expected mixed-shape error")
	}
}

func TestResolveRegimeATR_ReturnsLabeledMultiplier(t *testing.T) {
	block := RegimeATRBlock{
		TrendRegime: map[string]RegimeATREntry{
			"trending_up":   {ATR: 2.0},
			"trending_down": {ATR: 2.0},
			"ranging":       {ATR: 1.5},
		},
	}
	got, ok := resolveRegimeATR(block, "ranging")
	if !ok || got != 1.5 {
		t.Fatalf("resolveRegimeATR(ranging) = (%g, %v), want (1.5, true)", got, ok)
	}
	if _, ok := resolveRegimeATR(block, "nonsense"); ok {
		t.Fatalf("unknown regime label should resolve to ok=false")
	}
}

func TestDefaultRegimeTPTiersForRegime(t *testing.T) {
	tiers := defaultRegimeTPTiersForRegime("ranging")
	if len(tiers) != 2 {
		t.Fatalf("expected 2 default tiers, got %d", len(tiers))
	}
	if tiers[0].Multiple != 1.5 || tiers[1].Multiple != 2.5 {
		t.Fatalf("baseline mult mismatch: got %v", tiers)
	}
	if tiers[len(tiers)-1].Fraction != 1.0 {
		t.Fatalf("final tier fraction must be coerced to 1.0, got %g", tiers[len(tiers)-1].Fraction)
	}
}

func TestStrategyTPTiersForRegime_LegacyScalarUntouched(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{
			{Name: "tiered_tp_atr", Params: map[string]interface{}{
				"tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
				},
			}},
		},
	}
	tiers := strategyTPTiersForRegime(sc, "")
	if len(tiers) != 2 {
		t.Fatalf("legacy scalar should resolve regardless of regime: got %d tiers", len(tiers))
	}
}

func TestStrategyTPTiersForRegime_RegimeAwareNeedsRegime(t *testing.T) {
	sc := StrategyConfig{
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategies: []StrategyRef{
			{Name: "tiered_tp_atr_regime", Params: map[string]interface{}{"use_defaults": true}},
		},
	}
	// Empty regime → nil so the protection loop defers TP placement.
	if tiers := strategyTPTiersForRegime(sc, ""); len(tiers) != 0 {
		t.Fatalf("regime-aware without pos.Regime must return nil, got %v", tiers)
	}
	if tiers := strategyTPTiersForRegime(sc, "ranging"); len(tiers) != 2 {
		t.Fatalf("regime-aware with ranging should resolve 2 tiers, got %v", tiers)
	}
}

func TestRegimeATRBlock_UnmarshalThenResolveSurface(t *testing.T) {
	raw := []byte(`{"trend_regime": {
		"trending_up": {"atr": 2.0},
		"trending_down": {"atr": 2.0},
		"ranging": {"atr": 1.5}
	}}`)
	var b RegimeATRBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !b.IsZero() {
		t.Fatalf("block must look zero until ResolveSurface is called")
	}
	errs := b.ResolveSurface("test.stop_loss_atr_regime", regimeSurfaceStopLoss)
	if len(errs) > 0 {
		t.Fatalf("ResolveSurface errors: %v", errs)
	}
	if b.IsZero() {
		t.Fatalf("block must be populated after ResolveSurface")
	}
	if got, ok := resolveRegimeATR(b, "ranging"); !ok || got != 1.5 {
		t.Fatalf("ranging resolution failed: got=%g ok=%v", got, ok)
	}
}

func TestRegimeATRBlock_EqualForReload(t *testing.T) {
	mk := func(atr float64) *RegimeATRBlock {
		return &RegimeATRBlock{
			TrendRegime: map[string]RegimeATREntry{
				"trending_up":   {ATR: atr},
				"trending_down": {ATR: atr},
				"ranging":       {ATR: 1.5},
			},
		}
	}
	a := mk(2.0)
	b := mk(2.0)
	if !a.EqualForReload(b) {
		t.Fatalf("identical blocks should be equal")
	}
	c := mk(2.5)
	if a.EqualForReload(c) {
		t.Fatalf("differing trending_up should not be equal")
	}
	var zeroA *RegimeATRBlock
	var zeroB *RegimeATRBlock
	if !zeroA.EqualForReload(zeroB) {
		t.Fatalf("nil/nil should compare equal")
	}
	if a.EqualForReload(nil) {
		t.Fatalf("non-nil vs nil should differ")
	}
}

func TestValidateRegimeATRConfig_RequiresRegimeEnabled(t *testing.T) {
	cfg := &Config{
		Regime: &RegimeConfig{Enabled: false},
		Strategies: []StrategyConfig{
			{
				ID:       "test",
				Type:     "perps",
				Platform: "hyperliquid",
				StopLossATRRegime: &RegimeATRBlock{
					raw: map[string]interface{}{"use_defaults": true},
				},
			},
		},
	}
	errs := validateRegimeATRConfig(cfg)
	enabledErr := false
	for _, e := range errs {
		if strings.Contains(e, "regime.enabled=true") {
			enabledErr = true
		}
	}
	if !enabledErr {
		t.Fatalf("expected regime.enabled requirement error, got: %v", errs)
	}
}

func TestValidateRegimeATRConfig_RejectsScalarRegimeMutex(t *testing.T) {
	mult := 2.0
	cfg := &Config{
		Regime: &RegimeConfig{Enabled: true, Period: 14, ADXThreshold: 20},
		Strategies: []StrategyConfig{
			{
				ID:              "test",
				Type:            "perps",
				Platform:        "hyperliquid",
				StopLossATRMult: &mult,
				StopLossATRRegime: &RegimeATRBlock{
					raw: map[string]interface{}{"use_defaults": true},
				},
			},
		},
	}
	errs := validateRegimeATRConfig(cfg)
	found := false
	for _, e := range errs {
		if strings.Contains(e, "mutually exclusive with stop_loss_atr_mult") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected mutex error, got: %v", errs)
	}
}
