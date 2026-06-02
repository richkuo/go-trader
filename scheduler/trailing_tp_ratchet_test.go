package main

import (
	"sync"
	"testing"
)

func TestResolveTrailingMultAfter_AbsoluteAndFraction(t *testing.T) {
	tier := map[string]interface{}{
		"atr_multiple":        3.0,
		"close_fraction":      0.0,
		"trailing_mult_after": 1.5,
	}
	mult, err := resolveTrailingMultAfter(tier, 3.0)
	if err != nil || mult != 1.5 {
		t.Fatalf("absolute: mult=%v err=%v want 1.5", mult, err)
	}
	tier2 := map[string]interface{}{
		"atr_multiple":    2.0,
		"close_fraction":  0.0,
		"tp_atr_fraction": 0.5,
	}
	mult2, err := resolveTrailingMultAfter(tier2, 2.0)
	if err != nil || mult2 != 1.0 {
		t.Fatalf("fraction: mult=%v err=%v want 1.0", mult2, err)
	}
}

func TestFindHighestMarkClearedRatchetTier(t *testing.T) {
	tiers := []trailingRatchetTier{
		{ATRMultiple: 1.0, TrailingMultAfter: 2.0},
		{ATRMultiple: 2.0, TrailingMultAfter: 1.0},
	}
	idx, ok := findHighestMarkClearedRatchetTier(tiers, 1.5, 0)
	if !ok || idx != 0 {
		t.Fatalf("idx=%d ok=%v want 0,true", idx, ok)
	}
	idx, ok = findHighestMarkClearedRatchetTier(tiers, 2.5, 0)
	if !ok || idx != 1 {
		t.Fatalf("idx=%d ok=%v want 1,true", idx, ok)
	}
	idx, ok = findHighestMarkClearedRatchetTier(tiers, 2.5, 2)
	if ok {
		t.Fatalf("from watermark 2: idx=%d ok=%v want false", idx, ok)
	}
}

func TestApplyTrailingTPRatchet_MonotonicTighten(t *testing.T) {
	trailInit := 3.0
	sc := StrategyConfig{
		ID:       "s1",
		Type:     "perps",
		Platform: "hyperliquid",
		CloseStrategy: &StrategyRef{
			Name: "trailing_tp_ratchet",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{
						"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0,
					},
					map[string]interface{}{
						"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 1.0,
					},
				},
			},
		},
		TrailingStopATRMult: &trailInit,
	}
	state := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Side: "long", Quantity: 1, InitialQuantity: 1,
				AvgCost: 100, EntryATR: 10, Regime: "ranging",
			},
		},
	}
	var mu sync.RWMutex
	applyTrailingTPRatchet(sc, state, "ETH", 110, &mu, nil)
	pos := state.Positions["ETH"]
	if pos.PostTPTrailingATRMult == nil || *pos.PostTPTrailingATRMult != 2.0 {
		t.Fatalf("after tier0: PostTPTrailingATRMult=%v want 2.0", pos.PostTPTrailingATRMult)
	}
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Fatalf("watermark=%d want 1", pos.SLAdjustedTiersProcessed)
	}
	applyTrailingTPRatchet(sc, state, "ETH", 120, &mu, nil)
	if pos.PostTPTrailingATRMult == nil || *pos.PostTPTrailingATRMult != 1.0 {
		t.Fatalf("after tier1: PostTPTrailingATRMult=%v want 1.0", pos.PostTPTrailingATRMult)
	}
}

func TestValidateTrailingTPRatchetClose_RequiresTrailingMult(t *testing.T) {
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		CloseStrategy: &StrategyRef{
			Name: "trailing_tp_ratchet",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{
						"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0,
					},
				},
			},
		},
	}
	errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels)
	if len(errs) == 0 {
		t.Fatal("expected error when trailing_stop_atr_mult missing")
	}
}
