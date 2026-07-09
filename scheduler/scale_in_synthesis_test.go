package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// scaleInLiveProtectionResizable classifies the SL owner: ATR/regime fixed and
// trailing SLs can be grown after an add; static scalar SLs cannot (#873/#875).
func TestScaleInLiveProtectionResizable(t *testing.T) {
	atr := 1.5
	trailATR := 2.0
	trailPct := 3.0
	slPct := 4.0
	marginPct := 10.0
	cases := []struct {
		name string
		sc   StrategyConfig
		want bool
	}{
		{"fixed ATR", StrategyConfig{Type: "perps", Platform: "hyperliquid", StopLossATRMult: &atr}, true},
		{"trailing ATR", StrategyConfig{Type: "perps", Platform: "hyperliquid", TrailingStopATRMult: &trailATR}, true},
		{"trailing pct", StrategyConfig{Type: "perps", Platform: "hyperliquid", TrailingStopPct: &trailPct}, true},
		{"scalar stop_loss_pct", StrategyConfig{Type: "perps", Platform: "hyperliquid", StopLossPct: &slPct}, false},
		{"scalar margin_pct", StrategyConfig{Type: "perps", Platform: "hyperliquid", StopLossMarginPct: &marginPct, Leverage: 2}, false},
		{"max_drawdown fallback only", StrategyConfig{Type: "perps", Platform: "hyperliquid", MaxDrawdownPct: 20}, false},
	}
	for _, tc := range cases {
		if got := scaleInLiveProtectionResizable(tc.sc); got != tc.want {
			t.Errorf("%s: scaleInLiveProtectionResizable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A static scalar SL on a LIVE perps strategy with scale-in is rejected at load
// because the resize path can't grow it after an add (#873/#875).
func TestConfigValidationRejectsScalarSLScaleInOnLivePerps(t *testing.T) {
	slPct := 4.0
	cfg := &Config{
		Strategies: []StrategyConfig{{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
			Args:         []string{"x.py", "ETH", "1h", "--mode=live"},
			Capital:      1000,
			AllowScaleIn: true,
			StopLossPct:  &slPct,
		}},
	}
	err := validateConfig(cfg, true)
	if err == nil || !strings.Contains(err.Error(), "requires an ATR/regime or trailing stop-loss") {
		t.Fatalf("expected scalar-SL live scale-in rejection, got: %v", err)
	}
}

// The same scalar SL is fine on PAPER perps (no on-chain orders to under-cover)
// and an ATR SL is fine on live perps — the guard does not fire in either case.
func TestConfigValidationScaleInGuardScopedToLiveScalar(t *testing.T) {
	slPct := 4.0
	atr := 1.5
	guardMsg := "requires an ATR/regime or trailing stop-loss"

	paper := &Config{Strategies: []StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"x.py", "ETH", "1h"}, Capital: 1000, AllowScaleIn: true, StopLossPct: &slPct,
	}}}
	if err := validateConfig(paper, true); err != nil && strings.Contains(err.Error(), guardMsg) {
		t.Errorf("paper scalar-SL scale-in must not trip the live guard: %v", err)
	}

	live := &Config{Strategies: []StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args: []string{"x.py", "ETH", "1h", "--mode=live"}, Capital: 1000, AllowScaleIn: true, StopLossATRMult: &atr,
	}}}
	if err := validateConfig(live, true); err != nil && strings.Contains(err.Error(), guardMsg) {
		t.Errorf("live ATR-SL scale-in must not trip the guard: %v", err)
	}
}

// After a scale-in the fixed ATR stop trigger stays pinned to the frozen entry
// (riskAnchorPrice), not the blended AvgCost (#873 geometry sweep).
func TestScaleInFreezesFixedSLGeometry(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{Type: "perps", Platform: "hyperliquid", StopLossATRMult: &mult}
	// Original entry 2000, then averaged up to a blended 2100 via an add.
	pos := &Position{
		Side: "long", Quantity: 200, InitialQuantity: 200,
		AvgCost: 2100, EntryATR: 50, RiskAnchorPrice: 2000, StopLossATRMult: &mult,
	}
	got := fixedStopLossATRTriggerPx(sc, "long", pos)
	// frozen: anchor - mult*ATR = 2000 - 75 = 1925 (NOT 2100-based 2025).
	if !approxEq(got, 1925) {
		t.Fatalf("fixed SL trigger = %v, want 1925 (frozen at riskAnchorPrice, not blended AvgCost)", got)
	}
}

// scale-in config is hot-reloadable when the strategy is flat (#873).
func TestApplyHotReloadConfigAllowsScaleInChangeWhenFlat(t *testing.T) {
	atr := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"x.py", "ETH", "1h"},
		Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, StopLossATRMult: &atr, AllowScaleIn: false,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"x.py", "ETH", "1h"},
		Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, StopLossATRMult: &atr, AllowScaleIn: true,
		ScaleIn: &ScaleInConfig{MaxAdds: 3},
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Cash: 1000, Positions: map[string]*Position{}},
	}}
	if _, err := applyHotReloadConfig(cfg, next, state, nil, nil); err != nil {
		t.Fatalf("expected scale-in change to succeed when flat, got: %v", err)
	}
	if !cfg.Strategies[0].AllowScaleIn {
		t.Fatalf("AllowScaleIn not applied")
	}
	if cfg.Strategies[0].ScaleIn == nil || cfg.Strategies[0].ScaleIn.MaxAdds != 3 {
		t.Fatalf("ScaleIn block not applied: %+v", cfg.Strategies[0].ScaleIn)
	}
}

// scale-in config changes are blocked while a position is open (#873).
func TestApplyHotReloadConfigRejectsScaleInChangeWithOpenPosition(t *testing.T) {
	atr := 1.5
	cfg := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"x.py", "ETH", "1h"},
		Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, StopLossATRMult: &atr, AllowScaleIn: false,
	}})
	next := minimalReloadConfig([]StrategyConfig{{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"x.py", "ETH", "1h"},
		Capital: 1000, MaxDrawdownPct: 10, Leverage: 2, StopLossATRMult: &atr, AllowScaleIn: true,
	}})
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {
			ID: "hl-eth", Cash: 900,
			Positions: map[string]*Position{"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Leverage: 2}},
		},
	}}
	_, err := applyHotReloadConfig(cfg, next, state, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "allow_scale_in changed with open positions") {
		t.Fatalf("expected open-position scale-in toggle rejection, got: %v", err)
	}
	if cfg.Strategies[0].AllowScaleIn {
		t.Fatalf("current config mutated after rejected reload")
	}
}

// For a trailing-SL strategy that also places on-chain TPs, the protection sync
// re-sizes only the TPs (scaleInProtectionForceReplace returns forceSL=false
// because plan.StopLossATRMult==0) and must DEFER the resize-pending clear to the
// trailing walker — gated by effectiveTrailingStopPct > 0. A non-trailing (fixed
// ATR) owner makes effectiveTrailingStopPct==0, so the sync owns the clear (#882).
func TestScaleInTrailingSLOwnerDefersClearToWalker(t *testing.T) {
	trail := 2.0
	scTrailing := StrategyConfig{Type: "perps", Platform: "hyperliquid", TrailingStopATRMult: &trail}
	pos := &Position{
		Side: "long", Quantity: 200, InitialQuantity: 200, AvgCost: 2100, EntryATR: 50,
		RiskAnchorPrice: 2000, ScaleInResizePending: true, TPOIDs: []int64{111, 222},
	}
	// Trailing walker owns the SL → the sync's plan carries no fixed ATR SL.
	plan := hlProtectionPlan{StopLossATRMult: 0, Tiers: []hlProtectionTier{{Multiple: 1}, {Multiple: 2}}}
	forceSL, forceTP := scaleInProtectionForceReplace(pos, plan)
	if forceSL {
		t.Errorf("forceSL = true, want false (trailing walker owns the SL; sync must not resize it)")
	}
	if len(forceTP) != 2 || !forceTP[0] || !forceTP[1] {
		t.Errorf("forceTP = %v, want [true true] (both resting TP tiers resize on the sync)", forceTP)
	}
	// Sync clear gate: trailing owner → defer to walker.
	if got := effectiveTrailingStopPct(scTrailing, pos); got <= 0 {
		t.Errorf("trailing effectiveTrailingStopPct = %v, want > 0 (sync must defer the clear)", got)
	}
	// Non-trailing (fixed ATR) owner → sync owns the clear.
	fixed := 1.5
	scFixed := StrategyConfig{Type: "perps", Platform: "hyperliquid", StopLossATRMult: &fixed}
	if got := effectiveTrailingStopPct(scFixed, pos); got != 0 {
		t.Errorf("fixed-ATR effectiveTrailingStopPct = %v, want 0 (sync owns the clear)", got)
	}
}

// scaleInResizeTrailingSLNow no-ops (before any subprocess) when it isn't the
// owner of the resize: not live, flag unset, non-trailing SL owner, or still
// size-capped after correcting the on-chain qty for the add fill (#882 hybrid).
func TestScaleInResizeTrailingSLNowGuards(t *testing.T) {
	trail := 2.0
	fixed := 1.5
	liveArgs := []string{"x.py", "ETH", "1h", "--mode=live"}
	mk := func(args []string, slMult *float64, trailMult *float64, pending bool, qty float64) (StrategyConfig, *StrategyState) {
		sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: args, StopLossATRMult: slMult, TrailingStopATRMult: trailMult}
		st := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "long", Quantity: qty, InitialQuantity: qty, AvgCost: 2100, EntryATR: 50, RiskAnchorPrice: 2000, ScaleInResizePending: pending},
		}}
		return sc, st
	}
	var mu sync.RWMutex

	// not live → no-op
	sc, st := mk([]string{"x.py", "ETH", "1h"}, nil, &trail, true, 2)
	if n, d := scaleInResizeTrailingSLNow(sc, st, "ETH", 2050, map[string]float64{"ETH": 2}, 1, &mu, nil, newTestLogger(t)); n != 0 || d != "" {
		t.Errorf("not-live: got (%d,%q), want (0,\"\")", n, d)
	}
	// flag unset → no-op
	sc, st = mk(liveArgs, nil, &trail, false, 2)
	if n, _ := scaleInResizeTrailingSLNow(sc, st, "ETH", 2050, map[string]float64{"ETH": 2}, 1, &mu, nil, newTestLogger(t)); n != 0 {
		t.Errorf("flag-unset: got %d, want 0", n)
	}
	// non-trailing SL owner (fixed ATR) → sync already handled it, no-op
	sc, st = mk(liveArgs, &fixed, nil, true, 2)
	if n, _ := scaleInResizeTrailingSLNow(sc, st, "ETH", 2050, map[string]float64{"ETH": 2}, 1, &mu, nil, newTestLogger(t)); n != 0 {
		t.Errorf("non-trailing: got %d, want 0", n)
	}
	// still capped after correcting on-chain qty (preAdd 0.5 + fill 0.25 < virtual 2) → defer, no-op
	sc, st = mk(liveArgs, nil, &trail, true, 2)
	if n, _ := scaleInResizeTrailingSLNow(sc, st, "ETH", 2050, map[string]float64{"ETH": 0.5}, 0.25, &mu, nil, newTestLogger(t)); n != 0 {
		t.Errorf("capped: got %d, want 0 (deferred)", n)
	}
	// flag must remain set for the deferred walker when capped
	if !st.Positions["ETH"].ScaleInResizePending {
		t.Errorf("capped: flag cleared, want still pending for the next walker cycle")
	}
}

// The durable resize-pending flag survives a SaveState/LoadState round-trip so a
// restart between an add and the deferred trailing-SL re-size still grows the SL
// next cycle (#873 synthesis).
func TestScaleInResizePendingPersistsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Cash: 1000,
			Positions: map[string]*Position{
				"ETH": {
					Symbol: "ETH", Quantity: 2, InitialQuantity: 2, AvgCost: 2100, Side: "long",
					Multiplier: 1, OwnerStrategyID: "hl-eth", OpenedAt: now,
					RiskAnchorPrice: 2000, ScaleInResizePending: true,
				},
			},
			OptionPositions: map[string]*OptionPosition{}, TradeHistory: []Trade{},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !loaded.Strategies["hl-eth"].Positions["ETH"].ScaleInResizePending {
		t.Fatalf("ScaleInResizePending lost across round-trip, want true")
	}
}

func TestRatchetFallbackNormalizePendingPersistsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {
			ID: "hl-eth", Type: "manual", Platform: "hyperliquid", Cash: 1000,
			Positions: map[string]*Position{
				"ETH": {
					Symbol: "ETH", Quantity: 2, InitialQuantity: 2, AvgCost: 2100, Side: "long",
					Multiplier: 1, OwnerStrategyID: "hl-eth", OpenedAt: now,
					RiskAnchorPrice: 2000, RatchetFallbackNormalizePending: true,
				},
			},
			OptionPositions: map[string]*OptionPosition{}, TradeHistory: []Trade{},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !loaded.Strategies["hl-eth"].Positions["ETH"].RatchetFallbackNormalizePending {
		t.Fatalf("RatchetFallbackNormalizePending lost across round-trip, want true")
	}
}
