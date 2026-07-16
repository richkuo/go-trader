package main

import "testing"

func TestHedgeTargetDecision_OpenInverse(t *testing.T) {
	sc := StrategyConfig{
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1.0},
	}
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}
	act := hedgeTargetDecision(sc, snap, 2000, 100000)
	if act.Kind != hedgeActionOpen {
		t.Fatalf("kind=%s want open", act.Kind)
	}
	if act.Side != "sell" {
		t.Fatalf("side=%s want sell", act.Side)
	}
	wantQty := 2 * 2000 * 1.0 / 100000
	if act.Qty < wantQty-1e-12 || act.Qty > wantQty+1e-12 {
		t.Fatalf("qty=%g want %g", act.Qty, wantQty)
	}
}

func TestHedgeTargetDecision_PrimaryFlatClosesHedge(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	snap := hedgeSnapshot{HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 2}
	act := hedgeTargetDecision(sc, snap, 2000, 100000)
	if act.Kind != hedgeActionCloseFull {
		t.Fatalf("kind=%s want close", act.Kind)
	}
	if act.Side != "buy" {
		t.Fatalf("side=%s want buy", act.Side)
	}
}

func TestHedgeTargetDecision_AddAndReduce(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}}
	// add
	snap := hedgeSnapshot{PrimaryQty: 3, PrimarySide: "long", HedgeQty: 0.04, HedgeSide: "short", HedgeBasis: 2}
	act := hedgeTargetDecision(sc, snap, 2000, 100000)
	if act.Kind != hedgeActionAdd {
		t.Fatalf("add kind=%s", act.Kind)
	}
	// reduce
	snap = hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.04, HedgeSide: "short", HedgeBasis: 2}
	act = hedgeTargetDecision(sc, snap, 2000, 100000)
	if act.Kind != hedgeActionReduce && act.Kind != hedgeActionCloseFull {
		t.Fatalf("reduce kind=%s", act.Kind)
	}
}

func TestHedgeTargetDecision_UnusableMarksFailClosed(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	snap := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long"}
	act := hedgeTargetDecision(sc, snap, 0, 100000)
	if !act.FailClosed {
		t.Fatal("expected fail-closed")
	}
}

func TestHedgeOrderSkipReason(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"}}
	snap := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long"}
	act := hedgeAction{Kind: hedgeActionOpen, Qty: 0.01, Side: "sell"}
	if got := hedgeOrderSkipReason(sc, snap, act, 2000, 100000); got != "" {
		t.Fatalf("unexpected skip: %s", got)
	}
	if got := hedgeOrderSkipReason(sc, snap, act, 0, 100000); got == "" {
		t.Fatal("expected skip on missing mark")
	}
}

func TestHedgeInverseMapping(t *testing.T) {
	if hedgeInverseOrderSide("long") != "sell" || hedgeInversePositionSide("long") != "short" {
		t.Fatal("long mapping")
	}
	if hedgeInverseOrderSide("short") != "buy" || hedgeInversePositionSide("short") != "long" {
		t.Fatal("short mapping")
	}
}
