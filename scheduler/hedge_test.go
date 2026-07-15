package main

import (
	"testing"
	"time"
)

func TestHedgeTargetDecisionUsesQuantityWatermark(t *testing.T) {
	sc := StrategyConfig{Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC/USDC:USDC", Ratio: 0.5}}

	open := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 2, PrimaryAvgCost: 100, PrimarySide: "long"}, 100, 50)
	if open.Kind != hedgeActionOpen || open.Side != "short" || open.Qty != 2 {
		t.Fatalf("open = %+v, want short 2", open)
	}

	add := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 3, PrimaryAvgCost: 100, PrimarySide: "long", HedgeQty: 2, HedgeBasis: 2, HedgeSide: "short"}, 100, 50)
	if add.Kind != hedgeActionAdd || add.Qty != 1 {
		t.Fatalf("add = %+v, want add 1", add)
	}

	reduce := hedgeTargetDecision(sc, hedgeSnapshot{PrimaryQty: 1, PrimaryAvgCost: 100, PrimarySide: "long", HedgeQty: 2, HedgeBasis: 2, HedgeSide: "short"}, 100, 50)
	if reduce.Kind != hedgeActionReduce || reduce.Qty != 1 {
		t.Fatalf("reduce = %+v, want reduce 1", reduce)
	}

	close := hedgeTargetDecision(sc, hedgeSnapshot{HedgeQty: 2, HedgeBasis: 2, HedgeSide: "short"}, 100, 50)
	if close.Kind != hedgeActionCloseFull || close.Qty != 2 {
		t.Fatalf("close = %+v, want close 2", close)
	}
}

func TestHedgeConfigValidationRejectsUnsafeCollisions(t *testing.T) {
	errs := validateHedgeConfigs([]StrategyConfig{
		{ID: "eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"--mode", "ETH"}, Capital: 100, MaxDrawdownPct: 10, Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1}},
		{ID: "btc", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"--mode", "BTC"}, Capital: 100, MaxDrawdownPct: 10},
	})
	if len(errs) == 0 {
		t.Fatal("validateHedgeConfigs accepted hedge coin configured by another strategy")
	}
}

func TestHedgeStatePersistsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{Strategies: map[string]*StrategyState{
		"eth": {
			ID: "eth", Type: "perps", Platform: "hyperliquid", Cash: 1000,
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.2, InitialQuantity: 0.2, AvgCost: 50000, Side: "short", Multiplier: 1, OwnerStrategyID: "eth", OpenedAt: time.Now(), HedgeFor: "ETH", HedgePrimaryQtyBasis: 2},
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
	pos := loaded.Strategies["eth"].Positions["BTC"]
	if pos.HedgeFor != "ETH" || pos.HedgePrimaryQtyBasis != 2 {
		t.Fatalf("hedge state = %+v, want HedgeFor=ETH basis=2", pos)
	}
}

func TestRecordHedgeTradeResultDoesNotChangeLossStreak(t *testing.T) {
	risk := RiskState{ConsecutiveLosses: 3}
	RecordHedgeTradeResult(&risk, -25)
	if risk.DailyPnL != -25 {
		t.Fatalf("DailyPnL = %v, want -25", risk.DailyPnL)
	}
	if risk.ConsecutiveLosses != 3 {
		t.Fatalf("ConsecutiveLosses = %d, want unchanged 3", risk.ConsecutiveLosses)
	}
}
