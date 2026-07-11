package main

import (
	"testing"
)

func hedgeSC() StrategyConfig {
	return hlPerpsWithHedge("eth-long", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Side: "inverse", Ratio: 1.0, Leverage: 3, MarginMode: "cross"})
}

func TestHedgeDecision_Disabled(t *testing.T) {
	sc := hlPerpsWithHedge("eth", "ETH", nil)
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long"}, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Errorf("disabled hedge should be none, got %s", got.Kind)
	}
}

func TestHedgeDecision_FreshOpenInverse(t *testing.T) {
	sc := hedgeSC()
	// primary long 1 ETH @ 3000, hedge flat, BTC @ 60000, ratio 1 → short 0.05 BTC.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 1, PrimarySide: "long", HedgeSymbol: "BTC"}, 3000, 60000)
	if got.Kind != hedgeActionOpen {
		t.Fatalf("want open, got %s (%s)", got.Kind, got.Reason)
	}
	if got.Side != "short" {
		t.Errorf("hedge side = %q, want short (inverse of long)", got.Side)
	}
	if !approxEq(got.Qty, 0.05) {
		t.Errorf("hedge qty = %g, want 0.05", got.Qty)
	}
	if !approxEq(got.TargetPrimaryBasis, 1) {
		t.Errorf("basis = %g, want 1", got.TargetPrimaryBasis)
	}

	// Short primary → long hedge.
	got = hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "short"}, 3000, 60000)
	if got.Side != "long" {
		t.Errorf("hedge side = %q, want long (inverse of short)", got.Side)
	}
}

func TestHedgeDecision_RatioSizing(t *testing.T) {
	sc := hlPerpsWithHedge("eth", "ETH", &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5})
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}, 3000, 60000)
	// notional = 2*3000*0.5 = 3000; qty = 3000/60000 = 0.05.
	if !approxEq(got.Qty, 0.05) {
		t.Errorf("hedge qty = %g, want 0.05 (ratio 0.5)", got.Qty)
	}
}

func TestHedgeDecision_UnusablePriceFailClosed(t *testing.T) {
	sc := hedgeSC()
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long"}, 0, 60000)
	if got.Kind != hedgeActionNone || got.Reason == "" {
		t.Errorf("unusable primary price should fail closed with reason, got %s %q", got.Kind, got.Reason)
	}
	got = hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long"}, 3000, 0)
	if got.Kind != hedgeActionNone || got.Reason == "" {
		t.Errorf("unusable hedge price should fail closed with reason, got %s %q", got.Kind, got.Reason)
	}
}

func TestHedgeDecision_PrimaryFlatClosesHedge(t *testing.T) {
	sc := hedgeSC()
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 0, HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1}, 3000, 60000)
	if got.Kind != hedgeActionCloseFull {
		t.Fatalf("want close_full, got %s", got.Kind)
	}
	if !approxEq(got.Qty, 0.05) {
		t.Errorf("close qty = %g, want 0.05 (full hedge)", got.Qty)
	}
	// Both flat → none.
	got = hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 0, HedgeQty: 0}, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Errorf("both flat should be none, got %s", got.Kind)
	}
}

func TestHedgeDecision_ScaleInAdd(t *testing.T) {
	sc := hedgeSC()
	// primary grew 1 → 1.5, hedge basis 1, hedge held 0.05 short.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1}, 3000, 60000)
	if got.Kind != hedgeActionAdd {
		t.Fatalf("want add, got %s (%s)", got.Kind, got.Reason)
	}
	// delta 0.5 * 3000 / 60000 = 0.025.
	if !approxEq(got.Qty, 0.025) {
		t.Errorf("add qty = %g, want 0.025", got.Qty)
	}
	if !approxEq(got.TargetPrimaryBasis, 1.5) {
		t.Errorf("new basis = %g, want 1.5", got.TargetPrimaryBasis)
	}
}

func TestHedgeDecision_PartialCloseReduce(t *testing.T) {
	sc := hedgeSC()
	// primary shrank 1 → 0.6, hedge basis 1, hedge held 0.05. fraction 0.4 → reduce 0.02.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 0.6, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1}, 3000, 60000)
	if got.Kind != hedgeActionReduce {
		t.Fatalf("want reduce, got %s (%s)", got.Kind, got.Reason)
	}
	if !approxEq(got.Qty, 0.02) {
		t.Errorf("reduce qty = %g, want 0.02", got.Qty)
	}
}

func TestHedgeDecision_ReduceDustDeferred(t *testing.T) {
	sc := hedgeSC()
	// A tiny reduce whose notional < $10 must defer (none) with basis unchanged.
	// hedge 0.05 basis 1, primary 0.999 → fraction 0.001 → reduce 0.00005 BTC * 60000 = $3 < $10.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 0.999, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1}, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Errorf("dust reduce should defer to none, got %s", got.Kind)
	}
}

func TestHedgeDecision_WrongSideFlattens(t *testing.T) {
	sc := hedgeSC()
	// hedge held LONG but primary is long (should be short) — defense-in-depth flatten.
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "long", HedgeBasis: 1}, 3000, 60000)
	if got.Kind != hedgeActionCloseFull {
		t.Errorf("wrong-side hedge should flatten, got %s", got.Kind)
	}
}

func TestHedgeDecision_InSyncNoop(t *testing.T) {
	sc := hedgeSC()
	got := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1}, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Errorf("in-sync hedge should be none, got %s", got.Kind)
	}
}

func TestHedgeSkipReason(t *testing.T) {
	sc := hedgeSC()
	snap := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeSymbol: "BTC"}
	action := hedgeTargetDecision(sc, snap, 3000, 60000)
	if r := hedgeOrderSkipReason(sc, action, snap, 3000, 60000); r != "" {
		t.Errorf("valid open should not skip, got %q", r)
	}
	// Price goes unusable at spawn → skip.
	if r := hedgeOrderSkipReason(sc, action, snap, 3000, 0); r == "" {
		t.Error("unusable spawn price should skip")
	}
	// Snapshot changed so decision differs (hedge now held) → skip.
	changed := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1}
	if r := hedgeOrderSkipReason(sc, action, changed, 3000, 60000); r == "" {
		t.Error("decision change before spawn should skip")
	}
}

func TestApplyHedgeOpenFill(t *testing.T) {
	sc := hedgeSC()
	s := &StrategyState{ID: "eth-long", Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{}}
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "short", TargetPrimaryBasis: 1}
	// Paper-style booking: modeled fee, useFillFee=false.
	applyHedgeOpenOrAddFill(s, sc, action, "BTC", "ETH", 60000, 0.05, 0, "", false, nil)
	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge position not created")
	}
	if pos.HedgeFor != "ETH" {
		t.Errorf("HedgeFor = %q, want ETH", pos.HedgeFor)
	}
	if pos.Side != "short" {
		t.Errorf("side = %q, want short", pos.Side)
	}
	if !approxEq(pos.Quantity, 0.05) {
		t.Errorf("qty = %g, want 0.05", pos.Quantity)
	}
	if !approxEq(pos.HedgePrimaryQtyBasis, 1) {
		t.Errorf("basis = %g, want 1", pos.HedgePrimaryQtyBasis)
	}
	if pos.OwnerStrategyID != "eth-long" {
		t.Errorf("owner = %q, want eth-long", pos.OwnerStrategyID)
	}
	if pos.Multiplier != 1 {
		t.Errorf("multiplier = %g, want 1", pos.Multiplier)
	}
	// Cash reduced only by the fee (notional stays virtual): a taker fee on
	// $3000 notional is a few dollars, never the full notional.
	if s.Cash >= 1000 || s.Cash < 995 {
		t.Errorf("cash = %g, want ~1000 minus a small taker fee", s.Cash)
	}
	// The open Trade is labeled "hedge".
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].TradeType != "hedge" {
		t.Errorf("expected one hedge trade, got %+v", s.TradeHistory)
	}
	if s.TradeHistory[0].IsClose {
		t.Error("open leg must not be is_close")
	}
}

func TestApplyHedgeOpenPartialFillBasis(t *testing.T) {
	sc := hedgeSC()
	s := &StrategyState{ID: "eth-long", Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{}}
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "short", TargetPrimaryBasis: 1}
	// Only half the requested qty filled → basis is proportionally half.
	applyHedgeOpenOrAddFill(s, sc, action, "BTC", "ETH", 60000, 0.025, 0, "", false, nil)
	pos := s.Positions["BTC"]
	if !approxEq(pos.HedgePrimaryQtyBasis, 0.5) {
		t.Errorf("partial-fill basis = %g, want 0.5", pos.HedgePrimaryQtyBasis)
	}
}

func TestApplyHedgeAddFillBlends(t *testing.T) {
	sc := hedgeSC()
	s := &StrategyState{ID: "eth-long", Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{
		"BTC": {Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 60000, Side: "short", Multiplier: 1, OwnerStrategyID: "eth-long", HedgeFor: "ETH", HedgePrimaryQtyBasis: 1},
	}}
	action := hedgeAction{Kind: hedgeActionAdd, Qty: 0.025, Side: "short", TargetPrimaryBasis: 1.5}
	applyHedgeOpenOrAddFill(s, sc, action, "BTC", "ETH", 66000, 0.025, 0, "", false, nil)
	pos := s.Positions["BTC"]
	// blended qty 0.075; avg = (0.05*60000 + 0.025*66000)/0.075 = 62000.
	if !approxEq(pos.Quantity, 0.075) {
		t.Errorf("blended qty = %g, want 0.075", pos.Quantity)
	}
	if !approxEq(pos.AvgCost, 62000) {
		t.Errorf("blended avg = %g, want 62000", pos.AvgCost)
	}
	if !approxEq(pos.HedgePrimaryQtyBasis, 1.5) {
		t.Errorf("basis = %g, want 1.5", pos.HedgePrimaryQtyBasis)
	}
}

func TestHedgeReduceProportionalQty(t *testing.T) {
	if q := hedgeReduceProportionalQty(0.05, 0.5); !approxEq(q, 0.025) {
		t.Errorf("proportional qty = %g, want 0.025", q)
	}
	if q := hedgeReduceProportionalQty(0.05, 2); !approxEq(q, 0.05) {
		t.Errorf("over-fraction should clamp to full, got %g", q)
	}
	if q := hedgeReduceProportionalQty(0, 0.5); q != 0 {
		t.Errorf("no hedge → 0, got %g", q)
	}
}
