package main

import (
	"strings"
	"testing"
)

func ratchetFloatPtr(v float64) *float64 { return &v }

func TestStampTrailingTPRatchetMult_MonotonicTighten(t *testing.T) {
	pos := &Position{Quantity: 1.0}

	// First stamp installs the multiple.
	if !stampTrailingTPRatchetMult(pos, ratchetFloatPtr(2.0)) {
		t.Fatalf("expected first stamp to apply")
	}
	if pos.PostTPTrailingATRMult == nil || *pos.PostTPTrailingATRMult != 2.0 {
		t.Fatalf("got %v, want 2.0", pos.PostTPTrailingATRMult)
	}

	// A tighter (smaller) multiple wins.
	if !stampTrailingTPRatchetMult(pos, ratchetFloatPtr(1.0)) {
		t.Fatalf("expected tighter stamp to apply")
	}
	if *pos.PostTPTrailingATRMult != 1.0 {
		t.Fatalf("got %v, want 1.0", *pos.PostTPTrailingATRMult)
	}

	// A looser (larger) multiple is ignored.
	if stampTrailingTPRatchetMult(pos, ratchetFloatPtr(1.5)) {
		t.Fatalf("expected looser stamp to be ignored")
	}
	if *pos.PostTPTrailingATRMult != 1.0 {
		t.Fatalf("got %v, want 1.0 (unchanged)", *pos.PostTPTrailingATRMult)
	}

	// Equal multiple is ignored (no churn).
	if stampTrailingTPRatchetMult(pos, ratchetFloatPtr(1.0)) {
		t.Fatalf("expected equal stamp to be ignored")
	}
}

func TestStampTrailingTPRatchetMult_Guards(t *testing.T) {
	if stampTrailingTPRatchetMult(nil, ratchetFloatPtr(1.0)) {
		t.Fatalf("nil position must no-op")
	}
	if stampTrailingTPRatchetMult(&Position{Quantity: 0}, ratchetFloatPtr(1.0)) {
		t.Fatalf("flat position must no-op")
	}
	if stampTrailingTPRatchetMult(&Position{Quantity: 1}, nil) {
		t.Fatalf("nil mult must no-op")
	}
	if stampTrailingTPRatchetMult(&Position{Quantity: 1}, ratchetFloatPtr(0)) {
		t.Fatalf("non-positive mult must no-op")
	}
}

func TestIsTrailingTPRatchetCloseName(t *testing.T) {
	for _, n := range []string{"trailing_tp_ratchet", "trailing_tp_ratchet_regime", " TRAILING_TP_RATCHET "} {
		if !isTrailingTPRatchetCloseName(n) {
			t.Errorf("expected %q to be a trailing-TP-ratchet name", n)
		}
	}
	for _, n := range []string{"tiered_tp_atr", "tiered_tp_atr_regime", ""} {
		if isTrailingTPRatchetCloseName(n) {
			t.Errorf("expected %q NOT to be a trailing-TP-ratchet name", n)
		}
	}
	if !isTrailingTPRatchetRegimeCloseName("trailing_tp_ratchet_regime") {
		t.Errorf("regime variant not detected")
	}
	if isTrailingTPRatchetRegimeCloseName("trailing_tp_ratchet") {
		t.Errorf("plain variant misclassified as regime")
	}
}

func ratchetSC(name string, params map[string]interface{}, trail *float64) *StrategyConfig {
	return &StrategyConfig{
		ID:                  "s1",
		Platform:            "hyperliquid",
		Type:                "perps",
		TrailingStopATRMult: trail,
		CloseStrategy:       &StrategyRef{Name: name, Params: params},
	}
}

func plainTiers() map[string]interface{} {
	return map[string]interface{}{
		"tp_tiers": []interface{}{
			map[string]interface{}{"atr_multiple": 1.5, "close_fraction": 0.0, "trailing_mult_after": 2.0},
			map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 0.3, "tp_atr_fraction": 0.33},
		},
	}
}

func TestValidateTrailingTPRatchetClose_PlainOK(t *testing.T) {
	sc := ratchetSC("trailing_tp_ratchet", plainTiers(), ratchetFloatPtr(3.0))
	errs, usesRegime := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, "strategy[s1]")
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
	if usesRegime {
		t.Fatalf("plain variant must not set usesRegime")
	}
}

func TestValidateTrailingTPRatchetClose_RegimeOK(t *testing.T) {
	params := map[string]interface{}{
		"tp_tiers": map[string]interface{}{
			"trending_up":   []interface{}{map[string]interface{}{"atr_multiple": 1.5, "close_fraction": 0.0, "trailing_mult_after": 2.0}},
			"ranging":       []interface{}{map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.25, "trailing_mult_after": 1.5}},
			"trending_down": []interface{}{map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0}},
		},
	}
	sc := ratchetSC("trailing_tp_ratchet_regime", params, ratchetFloatPtr(3.0))
	errs, usesRegime := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, "strategy[s1]")
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
	if !usesRegime {
		t.Fatalf("regime variant must set usesRegime")
	}
}

func TestValidateTrailingTPRatchetClose_MissingTrailingStop(t *testing.T) {
	sc := ratchetSC("trailing_tp_ratchet", plainTiers(), nil)
	errs, _ := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, "strategy[s1]")
	if !hasErrContaining(errs, "positive trailing_stop_atr_mult") {
		t.Fatalf("expected trailing_stop_atr_mult error, got %v", errs)
	}
}

func TestValidateTrailingTPRatchetClose_BothTrailForms(t *testing.T) {
	params := map[string]interface{}{
		"tp_tiers": []interface{}{
			map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0, "tp_atr_fraction": 0.5},
		},
	}
	sc := ratchetSC("trailing_tp_ratchet", params, ratchetFloatPtr(3.0))
	errs, _ := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, "strategy[s1]")
	if !hasErrContaining(errs, "mutually exclusive") {
		t.Fatalf("expected mutual-exclusivity error, got %v", errs)
	}
}

func TestValidateTrailingTPRatchetClose_BadRegimeLabel(t *testing.T) {
	params := map[string]interface{}{
		"tp_tiers": map[string]interface{}{
			"trending_up_clean": []interface{}{map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0}},
		},
	}
	sc := ratchetSC("trailing_tp_ratchet_regime", params, ratchetFloatPtr(3.0))
	// adx vocabulary rejects the composite label.
	errs, _ := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, "strategy[s1]")
	if !hasErrContaining(errs, "unknown regime label") {
		t.Fatalf("expected unknown-label error, got %v", errs)
	}
}

func TestValidateTrailingTPRatchetClose_CompositeLabelsOK(t *testing.T) {
	params := map[string]interface{}{
		"tp_tiers": map[string]interface{}{
			"trending_up_clean": []interface{}{map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 2.0}},
			"ranging_volatile":  []interface{}{map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5, "trailing_mult_after": 0.5}},
		},
	}
	sc := ratchetSC("trailing_tp_ratchet_regime", params, ratchetFloatPtr(3.0))
	composite := []string{
		"trending_up_clean", "trending_up_choppy", "trending_down_clean",
		"trending_down_choppy", "ranging_quiet", "ranging_volatile", "ranging_directional",
	}
	errs, _ := validateTrailingTPRatchetClose(sc, composite, "strategy[s1]")
	if len(errs) != 0 {
		t.Fatalf("expected composite labels to validate, got %v", errs)
	}
}

func TestValidateTrailingTPRatchetClose_UnknownParam(t *testing.T) {
	params := plainTiers()
	params["use_defaults"] = true
	sc := ratchetSC("trailing_tp_ratchet", params, ratchetFloatPtr(3.0))
	errs, _ := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, "strategy[s1]")
	if !hasErrContaining(errs, "unknown param") {
		t.Fatalf("expected unknown-param error, got %v", errs)
	}
}

func TestValidateTrailingTPRatchetClose_WrongPlatform(t *testing.T) {
	sc := ratchetSC("trailing_tp_ratchet", plainTiers(), ratchetFloatPtr(3.0))
	sc.Platform = "binanceus"
	sc.Type = "spot"
	errs, _ := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, "strategy[s1]")
	if !hasErrContaining(errs, "HL perps/manual only") {
		t.Fatalf("expected platform error, got %v", errs)
	}
}

func TestTrailingTPRatchetParamsEqualForReload(t *testing.T) {
	a := ratchetSC("trailing_tp_ratchet", plainTiers(), ratchetFloatPtr(3.0))
	b := ratchetSC("trailing_tp_ratchet", plainTiers(), ratchetFloatPtr(3.0))
	if !trailingTPRatchetParamsEqualForReload(*a, *b) {
		t.Fatalf("identical tier tables must compare equal")
	}

	// A tier-table edit is a shape change.
	c := ratchetSC("trailing_tp_ratchet", map[string]interface{}{
		"tp_tiers": []interface{}{map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5, "trailing_mult_after": 1.0}},
	}, ratchetFloatPtr(3.0))
	if trailingTPRatchetParamsEqualForReload(*a, *c) {
		t.Fatalf("changed tier table must compare unequal")
	}

	// Non-ratchet configs compare equal (no gating).
	d := &StrategyConfig{Platform: "hyperliquid", Type: "perps", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"}}
	e := &StrategyConfig{Platform: "hyperliquid", Type: "perps", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"}}
	if !trailingTPRatchetParamsEqualForReload(*d, *e) {
		t.Fatalf("non-ratchet configs must compare equal")
	}
}

func hasErrContaining(errs []string, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}
