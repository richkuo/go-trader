package main

import (
	"strings"
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

func TestApplyTrailingTPRatchetToPosition_AfterScaleOut(t *testing.T) {
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
						"atr_multiple": 1.0, "close_fraction": 0.3, "trailing_mult_after": 1.0,
					},
				},
			},
		},
		TrailingStopATRMult: &trailInit,
	}
	pos := &Position{
		Symbol: "ETH", Side: "long", Quantity: 0.7, InitialQuantity: 1,
		AvgCost: 100, EntryATR: 10,
	}
	if !applyTrailingTPRatchetToPosition(sc, pos, "ETH", 110, nil) {
		t.Fatal("expected scale-out tier to tighten residual trail")
	}
	if pos.PostTPTrailingATRMult == nil || *pos.PostTPTrailingATRMult != 1.0 {
		t.Fatalf("PostTPTrailingATRMult=%v want 1.0", pos.PostTPTrailingATRMult)
	}
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Fatalf("watermark=%d want 1", pos.SLAdjustedTiersProcessed)
	}
}

func TestValidateTrailingTPRatchetClose_RejectsNonMonotonicTrail(t *testing.T) {
	trail := 2.0
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRMult: &trail,
		CloseStrategy: &StrategyRef{
			Name: "trailing_tp_ratchet",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{
						"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0,
					},
					map[string]interface{}{
						"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 3.0,
					},
				},
			},
		},
	}
	errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true)
	if len(errs) == 0 {
		t.Fatal("expected monotonic trail validation error")
	}
}

func TestValidateTrailingTPRatchetClose_RejectsDecreasingCloseFraction(t *testing.T) {
	trail := 3.0
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRMult: &trail,
		CloseStrategy: &StrategyRef{
			Name: "trailing_tp_ratchet",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{
						"atr_multiple": 1.0, "close_fraction": 0.4, "trailing_mult_after": 2.0,
					},
					map[string]interface{}{
						"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 1.0,
					},
				},
			},
		},
	}
	errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true)
	if len(errs) == 0 {
		t.Fatal("expected cumulative close_fraction validation error")
	}
}

func TestEffectiveTrailingStopPct_ManualNonRatchetReturnsZero(t *testing.T) {
	trail := 2.0
	sc := StrategyConfig{
		ID: "m1", Type: "manual", Platform: "hyperliquid",
		TrailingStopATRMult: &trail,
		CloseStrategy:       &StrategyRef{Name: "tiered_tp_atr_live"},
	}
	pos := &Position{AvgCost: 100, EntryATR: 5}
	if got := effectiveTrailingStopPct(sc, pos); got != 0 {
		t.Fatalf("manual non-ratchet effectiveTrailingStopPct = %v, want 0", got)
	}
	sc.CloseStrategy = &StrategyRef{Name: "trailing_tp_ratchet"}
	if got := effectiveTrailingStopPct(sc, pos); got <= 0 {
		t.Fatalf("manual ratchet effectiveTrailingStopPct = %v, want > 0", got)
	}
}

func TestValidateConfigManualRatchetAllowsTrailingATRMult(t *testing.T) {
	trail := 3.0
	cfg := &Config{
		Strategies: []StrategyConfig{
			{
				ID: "manual-eth", Type: "manual", Platform: "hyperliquid",
				Symbol: "ETH", Timeframe: "1h", Capital: 1000, Leverage: 10, MaxDrawdownPct: 20,
				TrailingStopATRMult: &trail,
				CloseStrategy: &StrategyRef{
					Name: "trailing_tp_ratchet",
					Params: map[string]interface{}{
						"tp_tiers": []interface{}{
							map[string]interface{}{
								"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.5,
							},
						},
					},
				},
			},
		},
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("manual trailing_tp_ratchet config should validate, got: %v", err)
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
	errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true)
	if len(errs) == 0 {
		t.Fatal("expected error when trailing_stop_atr_mult missing")
	}
}

func TestValidateTrailingTPRatchetClose_RegimeRequiresRegimeEnabled(t *testing.T) {
	trail := 2.0
	tierList := []interface{}{
		map[string]interface{}{
			"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0,
		},
	}
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRMult: &trail,
		CloseStrategy: &StrategyRef{
			Name: "trailing_tp_ratchet_regime",
			Params: map[string]interface{}{
				"tp_tiers": map[string]interface{}{
					"ranging":  tierList,
					"trending": tierList,
					"volatile": tierList,
				},
			},
		},
	}
	errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, false)
	if !errListContains(errs, "requires top-level regime.enabled=true") {
		t.Fatalf("expected regime-enabled validation error, got: %v", errs)
	}
}

func TestValidateTrailingTPRatchetClose_AcceptsRangingObjectFallback(t *testing.T) {
	trail := 2.0
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRMult: &trail,
		CloseStrategy: &StrategyRef{
			Name: "trailing_tp_ratchet",
			Params: map[string]interface{}{
				"tp_tiers": map[string]interface{}{
					"ranging": []interface{}{
						map[string]interface{}{
							"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0,
						},
					},
				},
			},
		},
	}
	if errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true); len(errs) > 0 {
		t.Fatalf("ranging object fallback should validate, got: %v", errs)
	}
	tiers := trailingRatchetTiersForRegime(sc, "")
	if len(tiers) != 1 || tiers[0].ATRMultiple != 1.0 || tiers[0].TrailingMultAfter != 1.0 {
		t.Fatalf("ranging object fallback resolved tiers = %+v, want one 1x -> 1x tier", tiers)
	}
}

func TestValidateTrailingTPRatchetClose_RejectsFixedStopLossOwners(t *testing.T) {
	base := func() StrategyConfig {
		trail := 2.0
		return StrategyConfig{
			ID: "s1", Type: "perps", Platform: "hyperliquid",
			TrailingStopATRMult: &trail,
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
	}
	cases := []struct {
		name string
		edit func(*StrategyConfig)
		want string
	}{
		{
			name: "stop_loss_pct",
			edit: func(sc *StrategyConfig) {
				v := 2.0
				sc.StopLossPct = &v
			},
			want: "cannot combine with stop_loss_pct",
		},
		{
			name: "stop_loss_margin_pct",
			edit: func(sc *StrategyConfig) {
				v := 20.0
				sc.StopLossMarginPct = &v
			},
			want: "cannot combine with stop_loss_margin_pct",
		},
		{
			name: "stop_loss_atr_mult",
			edit: func(sc *StrategyConfig) {
				v := 1.5
				sc.StopLossATRMult = &v
			},
			want: "cannot combine with stop_loss_atr_mult",
		},
		{
			name: "stop_loss_atr_regime",
			edit: func(sc *StrategyConfig) {
				sc.StopLossATRRegime = &RegimeATRBlock{UseDefaults: true}
			},
			want: "cannot combine with stop_loss_atr_regime",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := base()
			tc.edit(&sc)
			errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true)
			if !errListContains(errs, tc.want) {
				t.Fatalf("expected %q validation error, got: %v", tc.want, errs)
			}
		})
	}
}

func TestValidateTrailingTPRatchetClose_RejectsFirstRungLooserThanInitial(t *testing.T) {
	trail := 1.0
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRMult: &trail,
		CloseStrategy: &StrategyRef{
			Name: "trailing_tp_ratchet",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					// First rung trail (2.0×) is LOOSER than the initial 1.0× trail —
					// it would silently no-op at runtime, so reject at load.
					map[string]interface{}{
						"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0,
					},
				},
			},
		},
	}
	errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true)
	if !errListContains(errs, "can only tighten") {
		t.Fatalf("expected first-rung-looser-than-initial validation error, got: %v", errs)
	}
}

// TestValidateTrailingTPRatchetClose_CompositeVocabulary proves the regime form
// validates cleanly against the 7-state composite classifier when the strategy's
// regime_atr_window opts into it (and rejects an ADX-only label under composite).
func TestValidateTrailingTPRatchetClose_CompositeVocabulary(t *testing.T) {
	tierList := []interface{}{
		map[string]interface{}{
			"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0,
		},
	}
	compositeTable := func(labels []string) map[string]interface{} {
		tbl := make(map[string]interface{}, len(labels))
		for _, l := range labels {
			tbl[l] = tierList
		}
		return tbl
	}
	composite := regimeLabelsForClassifier(regimeClassifierComposite)
	if len(composite) != 7 {
		t.Fatalf("expected 7 composite labels, got %d", len(composite))
	}

	trail := 3.0
	scOK := StrategyConfig{
		ID: "hl-comp", Type: "perps", Platform: "hyperliquid",
		RegimeATRWindow:     "daily",
		TrailingStopATRMult: &trail,
		CloseStrategy: &StrategyRef{
			Name:   "trailing_tp_ratchet_regime",
			Params: map[string]interface{}{"tp_tiers": compositeTable(composite)},
		},
	}
	if errs := validateRegimeATRConfig(compositeRegimeCfg(scOK)); len(errs) != 0 {
		t.Fatalf("composite trailing_tp_ratchet_regime must validate, got: %v", errs)
	}

	// A 3-state ADX label under a composite window must be rejected.
	bad := compositeTable(composite)
	bad["ranging"] = tierList // not a composite label
	trail2 := 3.0
	scBad := StrategyConfig{
		ID: "hl-comp-bad", Type: "perps", Platform: "hyperliquid",
		RegimeATRWindow:     "daily",
		TrailingStopATRMult: &trail2,
		CloseStrategy: &StrategyRef{
			Name:   "trailing_tp_ratchet_regime",
			Params: map[string]interface{}{"tp_tiers": bad},
		},
	}
	if errs := validateRegimeATRConfig(compositeRegimeCfg(scBad)); !errListContains(errs, "unknown regime key") {
		t.Fatalf("expected unknown-regime-key error for ADX label under composite window, got: %v", errs)
	}
}

// applyTrailingStopUpdateResult is the shared handler routed through by both the
// perps and manual trailing dispatches. These cover the three slUpdate outcomes
// the manual path previously dropped (immediate fill + cancel-without-rest).
func ratchetTestState(pos *Position) *StrategyState {
	return &StrategyState{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		Positions: map[string]*Position{pos.Symbol: pos},
	}
}

func TestApplyTrailingStopUpdateResult_RestingReplacement(t *testing.T) {
	s := ratchetTestState(&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 5, StopLossOID: 7})
	upd := &HyperliquidStopLossUpdateResult{StopLossOID: 42, StopLossTriggerPx: 95}
	fill, px := applyTrailingStopUpdateResult(s, "ETH", "long", 7, 0, false, upd, nil)
	if fill || px != 0 {
		t.Fatalf("resting replacement: fill=%v px=%v want false,0", fill, px)
	}
	p := s.Positions["ETH"]
	if p.StopLossOID != 42 || p.StopLossTriggerPx != 95 {
		t.Fatalf("resting replacement OID/trigger = %d/%v want 42/95", p.StopLossOID, p.StopLossTriggerPx)
	}
}

func TestApplyTrailingStopUpdateResult_ImmediateFillBooksClose(t *testing.T) {
	s := ratchetTestState(&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 5, StopLossOID: 7})
	upd := &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 95}
	fill, px := applyTrailingStopUpdateResult(s, "ETH", "long", 7, 0, false, upd, nil)
	if !fill || px != 95 {
		t.Fatalf("immediate fill: fill=%v px=%v want true,95", fill, px)
	}
	if p, ok := s.Positions["ETH"]; ok && p != nil && p.Quantity > 0 {
		t.Fatalf("immediate fill should have booked/closed the position, still qty=%v", p.Quantity)
	}
}

func TestApplyTrailingStopUpdateResult_CancelWithoutRestClearsStaleOID(t *testing.T) {
	s := ratchetTestState(&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 5, StopLossOID: 7, StopLossTriggerPx: 96})
	upd := &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}
	fill, _ := applyTrailingStopUpdateResult(s, "ETH", "long", 7, 0, false, upd, nil)
	if fill {
		t.Fatal("cancel-without-rest: want fill=false")
	}
	p := s.Positions["ETH"]
	if p.StopLossOID != 0 || p.StopLossTriggerPx != 0 {
		t.Fatalf("stale OID/trigger not cleared: %d/%v want 0/0", p.StopLossOID, p.StopLossTriggerPx)
	}
}

func TestApplyTrailingStopUpdateResult_SideGuardSkipsMutation(t *testing.T) {
	s := ratchetTestState(&Position{Symbol: "ETH", Side: "short", Quantity: 1, AvgCost: 100, EntryATR: 5, StopLossOID: 7})
	upd := &HyperliquidStopLossUpdateResult{StopLossOID: 42, StopLossTriggerPx: 95}
	fill, _ := applyTrailingStopUpdateResult(s, "ETH", "long", 7, 0, false, upd, nil)
	if fill {
		t.Fatal("side mismatch: want fill=false")
	}
	if p := s.Positions["ETH"]; p.StopLossOID != 7 {
		t.Fatalf("side mismatch must not mutate OID, got %d want 7", p.StopLossOID)
	}
}

func errListContains(errs []string, needle string) bool {
	for _, err := range errs {
		if strings.Contains(err, needle) {
			return true
		}
	}
	return false
}
