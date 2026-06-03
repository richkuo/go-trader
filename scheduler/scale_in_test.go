package main

import (
	"math"
	"strings"
	"testing"
)

func TestBlendPositionScaleIn(t *testing.T) {
	pos := &Position{
		Quantity:                 1.0,
		InitialQuantity:          1.0,
		AvgCost:                  2000,
		EntryATR:                 50,
		Regime:                   "trending",
		StopLossTriggerPx:        1900,
		SLAdjustedTiersProcessed: 1,
		TPArmedTiers:             []bool{true, false},
	}
	blendPositionScaleIn(pos, 0.5, 2200)
	wantAvg := (1.0*2000 + 0.5*2200) / 1.5
	if math.Abs(pos.AvgCost-wantAvg) > 1e-9 {
		t.Fatalf("AvgCost = %g, want %g", pos.AvgCost, wantAvg)
	}
	if math.Abs(pos.Quantity-1.5) > 1e-9 {
		t.Fatalf("Quantity = %g, want 1.5", pos.Quantity)
	}
	if math.Abs(pos.InitialQuantity-1.5) > 1e-9 {
		t.Fatalf("InitialQuantity = %g, want 1.5 (high-water grows with add)", pos.InitialQuantity)
	}
	if pos.EntryATR != 50 || pos.Regime != "trending" || pos.StopLossTriggerPx != 1900 {
		t.Fatalf("frozen fields moved: EntryATR=%g Regime=%q SL=%g", pos.EntryATR, pos.Regime, pos.StopLossTriggerPx)
	}
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Fatalf("tier watermark changed: %d", pos.SLAdjustedTiersProcessed)
	}
}

func TestPerpsScaleInIntent(t *testing.T) {
	if !PerpsScaleInIntent(1, "long", DirectionLong, true) {
		t.Fatal("expected long scale-in on buy")
	}
	if PerpsScaleInIntent(1, "long", DirectionLong, false) {
		t.Fatal("scale-in disabled should not intent")
	}
	if !PerpsScaleInIntent(-1, "short", DirectionShort, true) {
		t.Fatal("expected short scale-in on sell under direction=short")
	}
	if !PerpsScaleInIntent(-1, "short", DirectionBoth, true) {
		t.Fatal("expected short scale-in on sell under direction=both")
	}
}

func TestPerpsOrderSkipReasonAllowsScaleIn(t *testing.T) {
	if got := PerpsOrderSkipReason(1, "long", DirectionLong, true); got != "" {
		t.Fatalf("scale-in enabled should not skip, got %q", got)
	}
	if got := PerpsOrderSkipReason(1, "long", DirectionLong, false); got == "" {
		t.Fatal("scale-in disabled should skip already-long buy")
	}
}

func TestPerpsLiveOrderSizeScaleInBranch(t *testing.T) {
	size, ok, reason := perpsLiveOrderSize(1, 2000, 1000, 0.4, 2000, 1.0, 1.0, 0, "long", DirectionLong, 0, ScaleInPolicy{Allowed: true})
	if !ok || reason != "" {
		t.Fatalf("scale-in sizing failed: ok=%v reason=%q", ok, reason)
	}
	want := 1000.0 / 2000.0
	if math.Abs(size-want) > 1e-9 {
		t.Fatalf("size = %g, want %g", size, want)
	}
}

func TestPerpsLiveOrderSizeScaleInNotionalClamp(t *testing.T) {
	policy := ScaleInPolicy{Allowed: true, MaxNotionalUSD: 100}
	size, ok, reason := perpsLiveOrderSize(1, 2000, 10000, 0.4, 2000, 1.0, 1.0, 0, "long", DirectionLong, 0, policy)
	if !ok || reason != "" {
		t.Fatalf("clamp sizing failed: ok=%v reason=%q", ok, reason)
	}
	want := 100.0 / 2000.0
	if math.Abs(size-want) > 1e-9 {
		t.Fatalf("size = %g, want clamped %g", size, want)
	}
}

func TestScaleInPreExecBlockedReason(t *testing.T) {
	s := &StrategyState{
		TradeHistory: []Trade{{PositionID: "p1", IsScaleIn: true}},
	}
	pos := &Position{TradePositionID: "p1", Quantity: 1, Side: "long"}
	policy := ScaleInPolicy{Allowed: true, MaxAdds: 1}
	if got := scaleInPreExecBlockedReason(s, pos, policy, 0.1, 2000); got == "" {
		t.Fatal("expected max-adds pre-exec block")
	}
}

func TestScaleInNeedsFrozenTriggerSLRearm(t *testing.T) {
	trailMult := 2.0
	scTrail := StrategyConfig{
		Platform:            "hyperliquid",
		Type:                "perps",
		TrailingStopATRMult: &trailMult,
	}
	pos := &Position{
		Symbol:            "ETH",
		Side:              "long",
		Quantity:          1,
		AvgCost:           2000,
		EntryATR:          50,
		StopLossTriggerPx: 1900,
	}
	if !scaleInNeedsFrozenTriggerSLRearm(scTrail, pos) {
		t.Fatal("trailing owner should need frozen-trigger SL re-arm")
	}
	slMult := 1.5
	scATR := StrategyConfig{
		Platform:        "hyperliquid",
		Type:            "perps",
		StopLossATRMult: &slMult,
	}
	if scaleInNeedsFrozenTriggerSLRearm(scATR, pos) {
		t.Fatal("fixed ATR SL should be covered by protection sync")
	}
	scTrailTP := StrategyConfig{
		Platform:            "hyperliquid",
		Type:                "perps",
		TrailingStopATRMult: &trailMult,
		CloseStrategy: &StrategyRef{
			Name: "tiered_tp_atr_live",
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 1.0},
				},
			},
		},
		Args: []string{"sma_crossover", "ETH", "1h", "--mode=live"},
	}
	if !scaleInNeedsFrozenTriggerSLRearm(scTrailTP, pos) {
		t.Fatal("trailing SL + on-chain tiered TP should still need frozen-trigger SL re-arm")
	}
}

func TestAllowScaleInRejectsScalarStopOnLive(t *testing.T) {
	stopPct := 5.0
	cfg := Config{
		Strategies: []StrategyConfig{{
			ID:             "hl-test-eth",
			Type:           "perps",
			Platform:       "hyperliquid",
			Script:         "shared_scripts/check_hyperliquid.py",
			Args:           []string{"sma_crossover", "ETH", "1h", "--mode=live"},
			Capital:        1000,
			Leverage:       5,
			MaxDrawdownPct: 60,
			AllowScaleIn:   true,
			StopLossPct:    &stopPct,
		}},
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80},
	}
	err := ValidateConfig(&cfg)
	if err == nil || !strings.Contains(err.Error(), "allow_scale_in on HL live requires ATR-based stop loss") {
		t.Fatalf("expected scalar-SL scale-in rejection, got: %v", err)
	}
}

func TestExecutePerpsScaleInBlendsVirtualState(t *testing.T) {
	logger := newTestLogger(t)
	s := &StrategyState{
		ID:       "s1",
		Platform: "hyperliquid",
		Type:     "perps",
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol:                   "ETH",
				TradePositionID:          "s1:ETH:1",
				Quantity:                 0.4,
				InitialQuantity:          0.4,
				AvgCost:                  2000,
				EntryATR:                 40,
				Side:                     "long",
				StopLossTriggerPx:        1900,
				SLAdjustedTiersProcessed: 1,
			},
		},
	}
	policy := ScaleInPolicy{Allowed: true}
	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2100, 1, 1, 0, 0.2, "oid", 0.1, DirectionLong, 0, policy, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	pos := s.Positions["ETH"]
	if math.Abs(pos.Quantity-0.6) > 1e-9 {
		t.Fatalf("Quantity = %g, want 0.6", pos.Quantity)
	}
	wantAvg := (0.4*2000 + 0.2*2100) / 0.6
	if math.Abs(pos.AvgCost-wantAvg) > 1e-9 {
		t.Fatalf("AvgCost = %g, want %g", pos.AvgCost, wantAvg)
	}
	if pos.EntryATR != 40 || pos.StopLossTriggerPx != 1900 {
		t.Fatal("frozen protection geometry changed on scale-in")
	}
	if pos.SLAdjustedTiersProcessed != 1 {
		t.Fatalf("tier watermark reset to %d", pos.SLAdjustedTiersProcessed)
	}
	if len(s.TradeHistory) != 1 || !s.TradeHistory[0].IsScaleIn {
		t.Fatal("expected one IsScaleIn trade row")
	}
}

func TestScaleInMaxAddsCap(t *testing.T) {
	logger := newTestLogger(t)
	s := &StrategyState{
		ID:       "s1",
		Platform: "hyperliquid",
		Type:     "perps",
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol:          "ETH",
				TradePositionID: "s1:ETH:1",
				Quantity:        0.4,
				InitialQuantity: 0.4,
				AvgCost:         2000,
				Side:            "long",
			},
		},
		TradeHistory: []Trade{
			{PositionID: "s1:ETH:1", IsScaleIn: true},
		},
	}
	maxAdds := 1
	policy := ScaleInPolicy{Allowed: true, MaxAdds: maxAdds}
	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2100, 1, 1, 0, 0.1, "", 0, DirectionLong, 0, policy, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if trades != 0 {
		t.Fatalf("expected cap block, trades=%d qty=%g", trades, s.Positions["ETH"].Quantity)
	}
}
