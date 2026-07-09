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
	if tightened, _ := applyTrailingTPRatchetToPosition(sc, pos, "ETH", 110, nil); !tightened {
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

func TestConfigValidationManualRatchetAllowsTrailingATRMult(t *testing.T) {
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
	if err := validateConfig(cfg, false); err != nil {
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
	if len(composite) != 9 {
		t.Fatalf("expected 9 composite labels, got %d", len(composite))
	}

	// #870: the regime variant's opening trail / SL owner is the per-regime
	// trailing_stop_atr_regime block (scalar trailing_stop_atr_mult rejected).
	scOK := StrategyConfig{
		ID: "hl-comp", Type: "perps", Platform: "hyperliquid",
		RegimeATRWindow:       "daily",
		TrailingStopATRRegime: &RegimeATRBlock{raw: composite7StateATR(3.0)},
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
	scBad := StrategyConfig{
		ID: "hl-comp-bad", Type: "perps", Platform: "hyperliquid",
		RegimeATRWindow:       "daily",
		TrailingStopATRRegime: &RegimeATRBlock{raw: composite7StateATR(3.0)},
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
	s := ratchetTestState(&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 5, StopLossOID: 7, RatchetFallbackNormalizePending: true})
	upd := &HyperliquidStopLossUpdateResult{StopLossOID: 42, StopLossTriggerPx: 95}
	fill, px := applyTrailingStopUpdateResult(s, "ETH", "long", 7, 0, false, upd, nil)
	if fill || px != 0 {
		t.Fatalf("resting replacement: fill=%v px=%v want false,0", fill, px)
	}
	p := s.Positions["ETH"]
	if p.StopLossOID != 42 || p.StopLossTriggerPx != 95 {
		t.Fatalf("resting replacement OID/trigger = %d/%v want 42/95", p.StopLossOID, p.StopLossTriggerPx)
	}
	if p.RatchetFallbackNormalizePending {
		t.Fatal("resting replacement must clear ratchet fallback normalize marker")
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
	s := ratchetTestState(&Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 5, StopLossOID: 7, StopLossTriggerPx: 96, RatchetFallbackNormalizePending: true})
	upd := &HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}
	fill, _ := applyTrailingStopUpdateResult(s, "ETH", "long", 7, 0, false, upd, nil)
	if fill {
		t.Fatal("cancel-without-rest: want fill=false")
	}
	p := s.Positions["ETH"]
	if p.StopLossOID != 0 || p.StopLossTriggerPx != 0 {
		t.Fatalf("stale OID/trigger not cleared: %d/%v want 0/0", p.StopLossOID, p.StopLossTriggerPx)
	}
	if !p.RatchetFallbackNormalizePending {
		t.Fatal("cancel-without-rest must leave normalize marker set for retry")
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

// --- #866: system default fallback when tp_tiers is omitted / use_defaults ---

// TestDefaultTrailingRatchetTiers_InternallyValid proves the conservative system
// default ladder satisfies the ratchet invariants (ascending triggers, monotonic
// tighten) so it never fails its own validation when broadcast.
func TestDefaultTrailingRatchetTiers_InternallyValid(t *testing.T) {
	def := defaultTrailingRatchetTiers()
	if len(def) != 3 {
		t.Fatalf("default ladder len=%d want 3", len(def))
	}
	if def[0].TrailingMultAfter != 1.5 {
		t.Fatalf("default first rung trail=%v want 1.5", def[0].TrailingMultAfter)
	}
	if errs := validateTrailingRatchetTierMonotonicity(def, "default"); len(errs) > 0 {
		t.Fatalf("default ladder must be monotonic, got: %v", errs)
	}
	// Initial trail >= first rung (2.0 >= 1.5) must validate clean.
	if errs := validateTrailingRatchetInitialTrail(def, 2.0, "default"); len(errs) > 0 {
		t.Fatalf("default vs initial 2.0 must validate, got: %v", errs)
	}
}

func TestRatchetTierGroupDefaults_InternallyValid(t *testing.T) {
	for group, tiers := range ratchetTierGroupDefaults {
		if errs := validateTrailingRatchetTierMonotonicity(tiers, group); len(errs) > 0 {
			t.Errorf("group %q ladder must be monotonic, got: %v", group, errs)
		}
	}
}

func TestValidateTrailingTPRatchetClose_OmittedTiersUsesDefault(t *testing.T) {
	trail := 2.0
	for _, params := range []map[string]interface{}{
		nil,                    // bare ratchet, no params
		{"use_defaults": true}, // explicit opt-in
	} {
		sc := StrategyConfig{
			ID: "s1", Type: "perps", Platform: "hyperliquid",
			TrailingStopATRMult: &trail,
			CloseStrategy:       &StrategyRef{Name: "trailing_tp_ratchet", Params: params},
		}
		if errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true); len(errs) > 0 {
			t.Fatalf("omitted tp_tiers (params=%v) should validate via default, got: %v", params, errs)
		}
	}
}

func TestValidateTrailingTPRatchetClose_DefaultRespectsInitialTrail(t *testing.T) {
	// Default first rung tightens to 1.5×; an initial trail looser-than... no,
	// TIGHTER than 1.5 (here 1.0) means the default's first rung (1.5) is LOOSER
	// than the initial 1.0 and would silently no-op, so it must be rejected.
	trail := 1.0
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRMult: &trail,
		CloseStrategy:       &StrategyRef{Name: "trailing_tp_ratchet", Params: map[string]interface{}{"use_defaults": true}},
	}
	if errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true); !errListContains(errs, "can only tighten") {
		t.Fatalf("expected initial-trail validation error for default ladder, got: %v", errs)
	}
}

func TestValidateTrailingTPRatchetClose_RegimeOmittedTiersUsesDefault(t *testing.T) {
	// #870: the regime variant's opening trail / SL owner is the per-regime
	// block; each ADX label's open must clear its group's first ratchet rung
	// (trending_* → choppy first rung 1.5; ranging → ranging first rung 1.0).
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
			"trending_up":   {ATR: 2.0},
			"trending_down": {ATR: 2.0},
			"ranging":       {ATR: 1.5},
		}},
		CloseStrategy: &StrategyRef{Name: "trailing_tp_ratchet_regime", Params: map[string]interface{}{"use_defaults": true}},
	}
	// regime.enabled=true: the per-group default satisfies exhaustiveness for all labels.
	if errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true); len(errs) > 0 {
		t.Fatalf("regime ratchet use_defaults should validate via per-group default, got: %v", errs)
	}
	// regime.enabled=false still rejected (independent of tier source).
	if errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, false); !errListContains(errs, "requires top-level regime.enabled=true") {
		t.Fatalf("regime ratchet must still require regime.enabled even with defaults, got: %v", errs)
	}
}

func TestTrailingRatchetTiersForRegime_OmittedReturnsDefault(t *testing.T) {
	trail := 2.0
	cases := []struct {
		name, closeName, regime string
		want                    []trailingRatchetTier
	}{
		{"scalar", "trailing_tp_ratchet", "", defaultTrailingRatchetTiers()},
		// #870: the regime variant resolves the per-quality-group ladder.
		{"regime-clean", "trailing_tp_ratchet_regime", "trending_up_clean", []trailingRatchetTier{
			{ATRMultiple: 3.0, TrailingMultAfter: 1.5}, {ATRMultiple: 4.5, TrailingMultAfter: 1.0}, {ATRMultiple: 6.0, TrailingMultAfter: 0.8},
		}},
		{"regime-choppy", "trailing_tp_ratchet_regime", "trending_up", []trailingRatchetTier{
			{ATRMultiple: 2.0, TrailingMultAfter: 1.5}, {ATRMultiple: 2.5, TrailingMultAfter: 1.0}, {ATRMultiple: 3.0, TrailingMultAfter: 0.8},
		}},
		{"regime-ranging", "trailing_tp_ratchet_regime", "ranging_quiet", []trailingRatchetTier{
			{ATRMultiple: 0.75, CloseFraction: 0.4, TrailingMultAfter: 1.0}, {ATRMultiple: 1.5, CloseFraction: 0.8, TrailingMultAfter: 0.75}, {ATRMultiple: 2.0, CloseFraction: 1.0, TrailingMultAfter: 0.75},
		}},
		// #1059: ranging substates resolve to distinct ladders.
		{"regime-ranging-volatile", "trailing_tp_ratchet_regime", "ranging_volatile", []trailingRatchetTier{
			{ATRMultiple: 1.0, CloseFraction: 0.4, TrailingMultAfter: 1.0}, {ATRMultiple: 2.0, CloseFraction: 0.8, TrailingMultAfter: 0.75}, {ATRMultiple: 3.0, CloseFraction: 1.0, TrailingMultAfter: 0.75},
		}},
		{"regime-ranging-directional", "trailing_tp_ratchet_regime", "ranging_directional", []trailingRatchetTier{
			{ATRMultiple: 1.0, CloseFraction: 0.25, TrailingMultAfter: 1.0}, {ATRMultiple: 2.0, CloseFraction: 0.50, TrailingMultAfter: 1.0}, {ATRMultiple: 3.0, CloseFraction: 0.75, TrailingMultAfter: 0.8}, {ATRMultiple: 4.5, CloseFraction: 0.75, TrailingMultAfter: 0.6},
		}},
		// Bare ADX "ranging" still resolves to the quiet ladder (#1059).
		{"regime-ranging-adx", "trailing_tp_ratchet_regime", "ranging", []trailingRatchetTier{
			{ATRMultiple: 0.75, CloseFraction: 0.4, TrailingMultAfter: 1.0}, {ATRMultiple: 1.5, CloseFraction: 0.8, TrailingMultAfter: 0.75}, {ATRMultiple: 2.0, CloseFraction: 1.0, TrailingMultAfter: 0.75},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{
				ID: "s1", Type: "perps", Platform: "hyperliquid",
				TrailingStopATRMult: &trail,
				CloseStrategy:       &StrategyRef{Name: tc.closeName, Params: map[string]interface{}{"use_defaults": true}},
			}
			got := trailingRatchetTiersForRegime(sc, tc.regime)
			if len(got) != len(tc.want) {
				t.Fatalf("resolved %d tiers want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("tier[%d]=%+v want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestRegimeCloseDefaultGroup(t *testing.T) {
	cases := []struct {
		label, group string
		ok           bool
	}{
		{"trending_up_clean", "clean", true},
		{"trending_down_clean", "clean", true},
		{"trending_up_choppy", "choppy", true},
		{"trending_down_choppy", "choppy", true},
		{"trending_up", "choppy", true},   // ADX trend → choppy (no clean/choppy signal)
		{"trending_down", "choppy", true}, // ADX trend → choppy
		{"ranging", "ranging", true},
		{"ranging_quiet", "ranging", true},
		{"ranging_volatile", "ranging", true},
		{"ranging_directional", "ranging", true},
		{"", "", false},
		{"bogus", "", false},
	}
	for _, tc := range cases {
		g, ok := regimeCloseDefaultGroup(tc.label)
		if ok != tc.ok || g != tc.group {
			t.Errorf("regimeCloseDefaultGroup(%q) = (%q, %v), want (%q, %v)", tc.label, g, ok, tc.group, tc.ok)
		}
	}
}

// TestValidateTrailingTPRatchetClose_RegimeRejectsScalarMult covers #870: the
// regime variant's trail is owned by trailing_stop_atr_regime, so a scalar
// trailing_stop_atr_mult is rejected.
func TestValidateTrailingTPRatchetClose_RegimeRejectsScalarMult(t *testing.T) {
	trail := 2.0
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRMult: &trail,
		TrailingStopATRRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
			"trending_up": {ATR: 2.0}, "trending_down": {ATR: 2.0}, "ranging": {ATR: 1.5},
		}},
		CloseStrategy: &StrategyRef{Name: "trailing_tp_ratchet_regime", Params: map[string]interface{}{"use_defaults": true}},
	}
	errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true)
	if !errListContains(errs, "cannot combine with scalar trailing_stop_atr_mult") {
		t.Fatalf("expected scalar-mult rejection for regime ratchet, got: %v", errs)
	}
}

// TestValidateTrailingTPRatchetClose_RegimePerKeyInitialTrail covers #870: the
// initial-trail coupling is checked against each regime key's own opening trail,
// so a too-loose first rung is rejected scoped to that key only.
func TestValidateTrailingTPRatchetClose_RegimePerKeyInitialTrail(t *testing.T) {
	tier := func(mult, trailAfter float64) map[string]interface{} {
		return map[string]interface{}{"atr_multiple": mult, "close_fraction": 0.0, "trailing_mult_after": trailAfter}
	}
	table := map[string]interface{}{
		"trending_up":   []interface{}{tier(3.0, 1.5), tier(4.0, 1.0)},
		"trending_down": []interface{}{tier(3.0, 1.5), tier(4.0, 1.0)},
		"ranging":       []interface{}{tier(3.0, 2.0), tier(4.0, 1.0)}, // first trail 2.0 > ranging open 1.0
	}
	sc := StrategyConfig{
		ID: "s1", Type: "perps", Platform: "hyperliquid",
		TrailingStopATRRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
			"trending_up": {ATR: 3.0}, "trending_down": {ATR: 3.0}, "ranging": {ATR: 1.0},
		}},
		CloseStrategy: &StrategyRef{Name: "trailing_tp_ratchet_regime", Params: map[string]interface{}{"tp_tiers": table}},
	}
	errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true)
	if !errListContains(errs, "tp_tiers.ranging") || !errListContains(errs, "can only tighten") {
		t.Fatalf("expected per-key initial-trail error scoped to ranging, got: %v", errs)
	}
	if errListContains(errs, "tp_tiers.trending_up[0]") {
		t.Fatalf("trending_up (open 3.0) should pass the initial-trail check, got: %v", errs)
	}
}

// TestValidateTrailingTPRatchetClose_ManualRegimeRatchet covers #870: HL manual
// may own a regime ratchet via trailing_stop_atr_regime (gate widened).
func TestValidateTrailingTPRatchetClose_ManualRegimeRatchet(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-man", Type: "manual", Platform: "hyperliquid",
		TrailingStopATRRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
			"trending_up": {ATR: 2.0}, "trending_down": {ATR: 2.0}, "ranging": {ATR: 1.5},
		}},
		CloseStrategy: &StrategyRef{Name: "trailing_tp_ratchet_regime", Params: map[string]interface{}{"use_defaults": true}},
	}
	if errs := validateTrailingTPRatchetClose(sc, canonicalTrendRegimeLabels, true); len(errs) > 0 {
		t.Fatalf("manual regime ratchet should validate, got: %v", errs)
	}
}

// TestDefaultTrailingRatchetTiersForRegime covers #870 per-group ratchet ladders
// and the #1059 ranging-substate split.
func TestDefaultTrailingRatchetTiersForRegime(t *testing.T) {
	clean := defaultTrailingRatchetTiersForRegime("trending_up_clean")
	if len(clean) != 3 || clean[0].ATRMultiple != 3.0 || clean[0].TrailingMultAfter != 1.5 || clean[2].ATRMultiple != 6.0 {
		t.Fatalf("clean group mismatch: %+v", clean)
	}
	// #1059: ranging_quiet keeps the pre-split geometry; bare ADX "ranging" maps
	// to it too, so ADX behavior is unchanged.
	quiet := defaultTrailingRatchetTiersForRegime("ranging_quiet")
	if len(quiet) != 3 || quiet[0].ATRMultiple != 0.75 || quiet[0].CloseFraction != 0.4 || quiet[2].CloseFraction != 1.0 {
		t.Fatalf("ranging_quiet mismatch: %+v", quiet)
	}
	adx := defaultTrailingRatchetTiersForRegime("ranging")
	if len(adx) != 3 || adx[0].ATRMultiple != 0.75 || adx[2].CloseFraction != 1.0 {
		t.Fatalf("bare ADX ranging must map to the quiet ladder: %+v", adx)
	}
	// #1059: ranging_volatile widens triggers, close fractions unchanged.
	volatile := defaultTrailingRatchetTiersForRegime("ranging_volatile")
	if len(volatile) != 3 ||
		volatile[0].ATRMultiple != 1.0 || volatile[2].ATRMultiple != 3.0 ||
		volatile[0].CloseFraction != 0.4 || volatile[2].CloseFraction != 1.0 {
		t.Fatalf("ranging_volatile mismatch: %+v", volatile)
	}
	// #1059: ranging_directional rides further — 4 tiers, 25/50/75/75 with a
	// let-ride final rung (no extra close, tighter trail).
	dir := defaultTrailingRatchetTiersForRegime("ranging_directional")
	if len(dir) != 4 {
		t.Fatalf("ranging_directional want 4 tiers, got %+v", dir)
	}
	if dir[0].CloseFraction != 0.25 || dir[1].CloseFraction != 0.50 ||
		dir[2].CloseFraction != 0.75 || dir[3].CloseFraction != 0.75 {
		t.Fatalf("ranging_directional close fractions mismatch: %+v", dir)
	}
	if dir[3].ATRMultiple != 4.5 || dir[3].TrailingMultAfter != 0.6 {
		t.Fatalf("ranging_directional let-ride rung mismatch: %+v", dir[3])
	}
	if defaultTrailingRatchetTiersForRegime("") != nil {
		t.Error("empty regime must resolve to nil")
	}
	// #1124: the directional-drift substates must resolve to the SAME 4-tier
	// ranging_directional ladder — never nil. A nil here would mean the ratchet
	// (auto-protective exit) silently never arms for a ranging_directional_up/
	// _down position.
	for _, label := range []string{"ranging_directional_up", "ranging_directional_down"} {
		got := defaultTrailingRatchetTiersForRegime(label)
		if len(got) != len(dir) {
			t.Fatalf("%s want %d tiers (parity with ranging_directional), got %+v", label, len(dir), got)
		}
		for i := range dir {
			if got[i] != dir[i] {
				t.Fatalf("%s tier[%d] = %+v, want %+v (ranging_directional ladder)", label, i, got[i], dir[i])
			}
		}
	}
}

// TestRatchetCloseDefaultGroup covers #1059: the ratchet-only resolver
// differentiates the composite ranging substates, while regimeCloseDefaultGroup
// (the shared B2 ATR-TP fn) keeps collapsing them — verified in
// TestRegimeCloseDefaultGroup.
func TestRatchetCloseDefaultGroup(t *testing.T) {
	cases := []struct {
		label, group string
		ok           bool
	}{
		{"ranging_quiet", "ranging_quiet", true},
		{"ranging_volatile", "ranging_volatile", true},
		{"ranging_directional", "ranging_directional", true},
		// #1124: directional-drift substates share the ranging_directional ladder.
		{"ranging_directional_up", "ranging_directional", true},
		{"ranging_directional_down", "ranging_directional", true},
		{"ranging", "ranging_quiet", true}, // bare ADX → quiet ladder
		{"trending_up_clean", "clean", true},
		{"trending_up_choppy", "choppy", true},
		{"trending_up", "choppy", true},
		{"", "", false},
		{"bogus", "", false},
	}
	for _, tc := range cases {
		g, ok := ratchetCloseDefaultGroup(tc.label)
		if ok != tc.ok || g != tc.group {
			t.Errorf("ratchetCloseDefaultGroup(%q) = (%q, %v), want (%q, %v)", tc.label, g, ok, tc.group, tc.ok)
		}
	}
}

// TestValidateRegimeRatchet_AllDefaultsCompositeValidates is the #870
// integration case: trailing_stop_atr_regime + trailing_tp_ratchet_regime both
// on use_defaults under a composite classifier. The full validateRegimeATRConfig
// path expands the per-group opening trails (#1120: clean 2.5 / choppy 2.25 /
// ranging_quiet 1.0 / ranging_volatile 1.25 / ranging_directional* 1.5) and
// per-group ratchet ladders, and the per-key initial-trail coupling
// must hold for every label (each group's first rung ≤ its open).
func TestValidateRegimeRatchet_AllDefaultsCompositeValidates(t *testing.T) {
	sc := StrategyConfig{
		ID: "hl-allc", Type: "perps", Platform: "hyperliquid",
		RegimeATRWindow:       "daily",
		TrailingStopATRRegime: &RegimeATRBlock{raw: map[string]interface{}{"use_defaults": true}},
		CloseStrategy: &StrategyRef{
			Name:   "trailing_tp_ratchet_regime",
			Params: map[string]interface{}{"use_defaults": true},
		},
	}
	if errs := validateRegimeATRConfig(compositeRegimeCfg(sc)); len(errs) != 0 {
		t.Fatalf("all-defaults composite regime ratchet must validate, got: %v", errs)
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
