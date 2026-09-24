package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

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

func TestLoadConfigDynamicCloseConfirmCycles(t *testing.T) {
	cases := []struct {
		name       string
		close      string
		extra      string
		wantCycles int
		wantErr    string
	}{
		{"three cycles load and drive both readers", dynamicCloseStrategyName, `"regime_confirm_cycles": 3,`, 3, ""},
		{"one cycle confirms at once", dynamicCloseStrategyName, `"regime_confirm_cycles": 1,`, 1, ""},
		{"absent key keeps the default", dynamicCloseStrategyName, ``, 2, ""},
		{"zero rejected", dynamicCloseStrategyName, `"regime_confirm_cycles": 0,`, 0, "regime_confirm_cycles: must be a whole number >= 1"},
		{"negative rejected", dynamicCloseStrategyName, `"regime_confirm_cycles": -1,`, 0, "regime_confirm_cycles: must be a whole number >= 1"},
		{"fraction rejected", dynamicCloseStrategyName, `"regime_confirm_cycles": 2.5,`, 0, "regime_confirm_cycles: must be a whole number >= 1"},
		{"quoted number rejected", dynamicCloseStrategyName, `"regime_confirm_cycles": "2",`, 0, "regime_confirm_cycles: must be a whole number >= 1"},
		{"boolean rejected", dynamicCloseStrategyName, `"regime_confirm_cycles": true,`, 0, "regime_confirm_cycles: must be a whole number >= 1"},
		{"null rejected", dynamicCloseStrategyName, `"regime_confirm_cycles": null,`, 0, "regime_confirm_cycles: must be a whole number >= 1"},
		{"value past the int range rejected", dynamicCloseStrategyName, `"regime_confirm_cycles": 1e19,`, 0, "regime_confirm_cycles: must be a whole number >= 1"},
		{"unknown key reported once", dynamicCloseStrategyName, `"foo": 1,`, 0, `unknown param "foo" (allowed: trend_regime, atr_source, regime_confirm_cycles)`},
		{"key stays unknown on the unified close", "tiered_tp_atr_live_regime", `"regime_confirm_cycles": 2,`, 0, `unknown param "regime_confirm_cycles" (allowed: trend_regime, atr_source)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgJSON := fmt.Sprintf(`{
				"regime": {"enabled": true, "period": 14, "adx_threshold": 20},
				"strategies": [{
					"id": "hl-eth-dyn",
					"type": "perps",
					"platform": "hyperliquid",
					"script": "shared_scripts/check_hyperliquid.py",
					"args": ["sma_crossover", "ETH", "1h", "--mode=paper"],
					"capital": 1000,
					"close_strategy": {"name": %q, "params": {%s
						"trend_regime": {
							"trending_up": {"stop_loss_atr": 1.5, "tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 0.5}, {"atr_multiple": 4.0, "close_fraction": 1.0}]},
							"trending_down": {"stop_loss_atr": 1.0, "tp_tiers": [{"atr_multiple": 1.5, "close_fraction": 0.5}, {"atr_multiple": 3.0, "close_fraction": 1.0}]},
							"ranging": {"stop_loss_atr": 0.8, "tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.5}, {"atr_multiple": 2.0, "close_fraction": 1.0}]}
						}
					}}
				}]
			}`, tc.close, tc.extra)
			cfg, err := LoadConfig(writeTestConfig(t, t.TempDir(), cfgJSON))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("LoadConfig succeeded, want a validation error")
				}
				msg := err.Error()
				if strings.Count(msg, "\n  ") != 1 || strings.Count(msg, tc.wantErr) != 1 || strings.Count(msg, "regime_confirm_cycles") != 1 {
					t.Fatalf("LoadConfig error = %q, want exactly one error %q and one regime_confirm_cycles mention", msg, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig failed: %v", err)
			}
			sc := cfg.Strategies[0]
			if got := dynamicCloseConfirmCycles(sc); got != tc.wantCycles {
				t.Fatalf("dynamicCloseConfirmCycles = %d, want %d", got, tc.wantCycles)
			}
			livePos := &Position{RegimeAppliedLabel: "trending_up"}
			liveSt := &StrategyState{Regime: "ranging"}
			paperPos := &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 40, RegimeAppliedLabel: "trending_up"}
			paperSt := &StrategyState{Cash: 1000, Regime: "ranging", Positions: map[string]*Position{"ETH": paperPos}}
			var mu sync.RWMutex
			for cycle := 1; cycle <= tc.wantCycles; cycle++ {
				advanceDynamicCloseRegime(livePos, liveSt, sc)
				advancePaperDynamicCloseRegime(sc, paperSt, nil, "ETH", 2000, &mu, nil)
				want := "trending_up"
				if cycle == tc.wantCycles {
					want = "ranging"
				}
				if livePos.RegimeAppliedLabel != want || paperPos.RegimeAppliedLabel != want {
					t.Fatalf("cycle %d: live applied = %q, paper applied = %q, want %q", cycle, livePos.RegimeAppliedLabel, paperPos.RegimeAppliedLabel, want)
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

func dynamicCloseTestStrategy() StrategyConfig {
	return StrategyConfig{
		CloseStrategy: &StrategyRef{
			Name:   dynamicCloseStrategyName,
			Params: unifiedBlock(),
		},
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
