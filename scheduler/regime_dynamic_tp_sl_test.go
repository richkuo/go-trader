package main

import (
	"sync"
	"testing"
)

func TestStrategyCurrentATRRegime(t *testing.T) {
	sc := StrategyConfig{RegimeATRWindow: "medium"}
	st := &StrategyState{
		RegimeWindows: map[string]string{"medium": "ranging_quiet"},
	}
	if got := strategyCurrentATRRegime(st, sc); got != "ranging_quiet" {
		t.Fatalf("strategyCurrentATRRegime = %q, want ranging_quiet", got)
	}
}

func TestAdvanceDynamicCloseRegime_ConfirmCycles(t *testing.T) {
	sc := StrategyConfig{
		CloseStrategy: &StrategyRef{
			Name: dynamicCloseStrategyName,
			Params: map[string]interface{}{
				"regime_confirm_cycles": 2,
				"trend_regime": map[string]interface{}{
					"trending_up": map[string]interface{}{
						"stop_loss_atr": 1.5,
						"tp_tiers": []interface{}{
							map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
							map[string]interface{}{"atr_multiple": 4.0, "close_fraction": 1.0},
						},
					},
					"ranging": map[string]interface{}{
						"stop_loss_atr": 0.8,
						"tp_tiers": []interface{}{
							map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
							map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
						},
					},
				},
			},
		},
	}
	pos := &Position{RegimeAppliedLabel: "trending_up"}
	st := &StrategyState{
		Regime:        "ranging",
		RegimeWindows: map[string]string{"medium": "ranging"},
	}
	sc.RegimeATRWindow = "medium"

	if changed := advanceDynamicCloseRegime(pos, st, sc); changed {
		t.Fatal("first pending cycle should not apply")
	}
	if pos.RegimeAppliedLabel != "trending_up" {
		t.Fatalf("applied=%q want trending_up", pos.RegimeAppliedLabel)
	}
	if changed := advanceDynamicCloseRegime(pos, st, sc); !changed {
		t.Fatal("second pending cycle should apply ranging")
	}
	if pos.RegimeAppliedLabel != "ranging" {
		t.Fatalf("applied=%q want ranging", pos.RegimeAppliedLabel)
	}
}

func TestAdvancePaperDynamicCloseRegime(t *testing.T) {
	dynamic := &StrategyRef{Name: dynamicCloseStrategyName, Params: unifiedBlock()}
	dynamic.Params["regime_confirm_cycles"] = 2
	ratchet := &StrategyRef{Name: trailingTPRatchetCloseName, Params: map[string]interface{}{}}
	minMove := func(v float64) func(*StrategyConfig, *Position) {
		return func(sc *StrategyConfig, _ *Position) { sc.TrailingStopMinMovePct = &v }
	}
	postTPTrail := func(_ *StrategyConfig, pos *Position) {
		mult := 1.0
		pos.PostTPTrailingATRMult = &mult
	}
	trailOwner := func(sc *StrategyConfig, _ *Position) {
		mult := 1.5
		sc.TrailingStopATRMult = &mult
	}
	mark := 2000.0
	markAt := func(v float64) func(*StrategyConfig, *Position) {
		return func(*StrategyConfig, *Position) { mark = v }
	}
	cases := []struct {
		name        string
		mode        string
		close       *StrategyRef
		mutate      func(*StrategyConfig, *Position)
		cycles      int
		stop        float64
		wantApplied string
		wantRegime  string
		wantStop    float64
		matchesLive bool
	}{
		{"paper position confirms after regime_confirm_cycles", "--mode=paper", dynamic, nil, 2, 0, "ranging", "ranging", 0, false},
		{"paper position holds the old label before confirmation", "--mode=paper", dynamic, nil, 1, 0, "trending_up", "trending_up", 0, false},
		{"live position is left to the protection sync", "--mode=live", dynamic, nil, 2, 0, "trending_up", "trending_up", 0, false},
		{"non-dynamic close is untouched", "--mode=paper", &StrategyRef{Name: "tiered_tp_atr_live_regime", Params: unifiedBlock()}, nil, 2, 0, "trending_up", "trending_down", 0, false},
		{"confirmed flip re-arms the fixed stop at the new label", "--mode=paper", dynamic, nil, 2, 1940, "ranging", "ranging", 1968, true},
		{"unconfirmed flip keeps the fixed stop", "--mode=paper", dynamic, nil, 1, 1940, "trending_up", "trending_up", 1940, false},
		{"flip under the min-move gate keeps the fixed stop", "--mode=paper", dynamic, minMove(5), 2, 1940, "ranging", "ranging", 1940, true},
		{"flip after an sl_after breakeven move re-arms like the live sync", "--mode=paper", dynamic, nil, 2, 2000, "ranging", "ranging", 1968, true},
		{"flip after an sl_after move inside the min-move gate keeps it", "--mode=paper", dynamic, nil, 2, 1970, "ranging", "ranging", 1970, true},
		{"flip keeps a post-TP trailing stop", "--mode=paper", dynamic, postTPTrail, 2, 1990, "ranging", "ranging", 1990, false},
		{"flip keeps a trailing stop owner", "--mode=paper", dynamic, trailOwner, 2, 1990, "ranging", "ranging", 1990, false},
		{"ratchet trail is untouched", "--mode=paper", ratchet, trailOwner, 2, 1990, "trending_up", "trending_down", 1990, false},
		{"flip onto a crossed stop closes at the mark", "--mode=paper", dynamic, markAt(1950), 2, 1940, "ranging", "ranging", 1968, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := StrategyConfig{ID: "hl-dyn", Type: "perps", Platform: "hyperliquid", Args: []string{"sma", "ETH", "1h", tc.mode}, CloseStrategy: tc.close}
			pos := &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 40, Regime: "trending_down", RegimeAppliedLabel: "trending_up", StopLossTriggerPx: tc.stop}
			mark = 2000
			if tc.mutate != nil {
				tc.mutate(&sc, pos)
			}
			st := &StrategyState{Cash: 1000, Regime: "ranging", Positions: map[string]*Position{"ETH": pos}}
			var mu sync.RWMutex
			trades := 0
			for i := 0; i < tc.cycles; i++ {
				n, _ := advancePaperDynamicCloseRegime(sc, st, nil, "ETH", mark, &mu, nil)
				trades += n
			}
			_, open := st.Positions["ETH"]
			wantClosed := trailingStopBreached(pos.Side, mark, tc.wantStop)
			if open == wantClosed || (trades == 1) != wantClosed {
				t.Fatalf("open=%v trades=%d, want closed=%v", open, trades, wantClosed)
			}
			if wantClosed {
				last := st.TradeHistory[len(st.TradeHistory)-1]
				if !last.IsClose || !approxEq(last.Price, mark) {
					t.Fatalf("stop close = %+v, want a close at the mark %.2f", last, mark)
				}
			}
			if pos.RegimeAppliedLabel != tc.wantApplied {
				t.Fatalf("applied label = %q, want %q", pos.RegimeAppliedLabel, tc.wantApplied)
			}
			if got := positionCtxForCheck(sc, pos, nil).Regime; got != tc.wantRegime {
				t.Fatalf("check position regime = %q, want %q", got, tc.wantRegime)
			}
			if !approxEq(pos.StopLossTriggerPx, tc.wantStop) {
				t.Fatalf("paper stop = %.4f, want %.4f", pos.StopLossTriggerPx, tc.wantStop)
			}
			if tc.matchesLive {
				if live := liveStopAfterDynamicFlip(t, sc, "trending_up", "ranging", tc.stop); !approxEq(pos.StopLossTriggerPx, live) {
					t.Fatalf("paper stop = %.4f, live sync places %.4f", pos.StopLossTriggerPx, live)
				}
			}
		})
	}
}

func liveStopAfterDynamicFlip(t *testing.T, sc StrategyConfig, oldLabel, newLabel string, stop float64) float64 {
	t.Helper()
	live := sc
	live.Args = []string{"sma", "ETH", "1h", "--mode=live"}
	pos := &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 40, RegimeAppliedLabel: newLabel, StopLossOID: 7, StopLossTriggerPx: stop}
	plan, ok := buildHyperliquidProtectionPlan(live, pos, 0)
	if !ok {
		t.Fatal("live protection plan did not build")
	}
	forceSL, _ := dynamicProtectionForceReplace(live, pos, plan, oldLabel, true)
	if !forceSL {
		return stop
	}
	return atrStopLossTriggerPx(plan.Side, plan.AvgCost, plan.EntryATR, plan.StopLossATRMult)
}

func TestTriggerPxMoveExceedsMinPct(t *testing.T) {
	if !triggerPxMoveExceedsMinPct(100, 100.6, 0.5) {
		t.Fatal("0.6% move should exceed 0.5% threshold")
	}
	if triggerPxMoveExceedsMinPct(100, 100.4, 0.5) {
		t.Fatal("0.4% move should not exceed 0.5% threshold")
	}
}

func dynamicCloseTestStrategy() StrategyConfig {
	return StrategyConfig{
		CloseStrategy: &StrategyRef{
			Name:   dynamicCloseStrategyName,
			Params: unifiedBlock(),
		},
	}
}

func TestStrategyUsesRegimeTieredTPATRClose_IncludesDynamic(t *testing.T) {
	sc := dynamicCloseTestStrategy()
	if !strategyUsesRegimeTieredTPATRClose(sc) {
		t.Fatal("strategyUsesRegimeTieredTPATRClose must include tiered_tp_atr_live_regime_dynamic")
	}
}

func TestDynamicProtectionSurplusTPOIDs(t *testing.T) {
	got := dynamicProtectionSurplusTPOIDs([]int64{10, 20, 30}, 2)
	if len(got) != 1 || got[0] != 30 {
		t.Fatalf("surplus OIDs = %v, want [30]", got)
	}
	if surplus := dynamicProtectionSurplusTPOIDs([]int64{10, 0, 30}, 2); len(surplus) != 1 || surplus[0] != 30 {
		t.Fatalf("skip zero OID in surplus: %v", surplus)
	}
	if surplus := dynamicProtectionSurplusTPOIDs([]int64{10, 20}, 2); len(surplus) != 0 {
		t.Fatalf("no shrink: got %v", surplus)
	}
}

func TestDynamicProtectionForceReplace(t *testing.T) {
	sc := dynamicCloseTestStrategy()
	plan := hlProtectionPlan{
		Side:            "long",
		AvgCost:         100,
		EntryATR:        10,
		StopLossATRMult: 0.8,
		Tiers: []hlProtectionTier{
			{Multiple: 1.0, Fraction: 0.5},
			{Multiple: 2.0, Fraction: 1.0},
		},
	}

	t.Run("no_change", func(t *testing.T) {
		pos := &Position{TPOIDs: []int64{1, 2}}
		forceSL, forceTP := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", false)
		if forceSL || len(forceTP) > 0 {
			t.Fatalf("regimeChanged=false: forceSL=%v forceTP=%v", forceSL, forceTP)
		}
	})

	t.Run("sl_moves_past_debounce", func(t *testing.T) {
		pos := &Position{StopLossOID: 99, StopLossTriggerPx: 85}
		forceSL, _ := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", true)
		if !forceSL {
			t.Fatal("expected SL force-replace when regime SL mult changes beyond debounce")
		}
	})

	t.Run("filled_tier_skipped", func(t *testing.T) {
		pos := &Position{
			TPOIDs:       []int64{0, 2},
			TPArmedTiers: []bool{true, false},
		}
		_, forceTP := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", true)
		if len(forceTP) != 2 {
			t.Fatalf("forceTP len=%d want 2", len(forceTP))
		}
		if forceTP[0] {
			t.Fatal("filled tier (armed, OID=0) must not force-replace")
		}
	})

	t.Run("never_armed_skipped", func(t *testing.T) {
		pos := &Position{TPOIDs: []int64{0, 0}}
		_, forceTP := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", true)
		if forceTP[0] || forceTP[1] {
			t.Fatalf("never-armed tiers should skip force-replace: %v", forceTP)
		}
	})

	t.Run("tier_count_shrink_surplus", func(t *testing.T) {
		pos := &Position{TPOIDs: []int64{10, 20, 303}}
		surplus := dynamicProtectionSurplusTPOIDs(pos.TPOIDs, 2)
		if len(surplus) != 1 || surplus[0] != 303 {
			t.Fatalf("tier shrink surplus = %v, want [303]", surplus)
		}
	})

	t.Run("resting_tier_replaces_on_move", func(t *testing.T) {
		pos := &Position{
			TPOIDs:       []int64{101, 0},
			TPArmedTiers: []bool{true, false},
		}
		_, forceTP := dynamicProtectionForceReplace(sc, pos, plan, "trending_up", true)
		if !forceTP[0] {
			t.Fatal("resting tier with regime-driven price move should force-replace")
		}
		if forceTP[1] {
			t.Fatal("tier 1 never armed — sync places fresh without cancel")
		}
	})
}

func TestDynamicCloseInSuppressionSet(t *testing.T) {
	sc := StrategyConfig{
		Type:          "perps",
		Platform:      "hyperliquid",
		Args:          []string{"hold", "ETH", "1h", "--mode=live"},
		CloseStrategy: &StrategyRef{Name: dynamicCloseStrategyName},
	}
	if !strategyUsesTieredTPATRClose(sc) {
		t.Fatal("dynamic close must be recognized by strategyUsesTieredTPATRClose")
	}
	if !closeStrategySuppressedByOnChainProtection(sc) {
		t.Fatal("dynamic close must be suppressed on HL live when on-chain TPs active")
	}
}
