package main

import (
	"sync"
	"testing"
)

func TestRunTrailingStopUpdateAfterRatchetTighten_LiveReplacesWiderTrigger(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	postTP := 0.75
	minMove := 0.1
	liveArgs := []string{"x.py", "ETH", "1h", "--mode=live"}
	sc := StrategyConfig{
		ID: "hl-vwap-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: liveArgs,
		CloseStrategy:          &StrategyRef{Name: trailingTPRatchetCloseName},
		TrailingStopATRMult:    floatPtr(2.5),
		TrailingStopMinMovePct: &minMove,
	}
	st := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
		"ETH": {
			Symbol: "ETH", Side: "long", Quantity: 0.2, InitialQuantity: 1.0,
			AvgCost: 100, EntryATR: 5, RiskAnchorPrice: 100,
			StopLossOID: 510751, StopLossTriggerPx: 96.0,
			StopLossHighWaterPx:      102.0,
			PostTPTrailingATRMult:    &postTP,
			SLAdjustedTiersProcessed: 2,
		},
	}}
	var mu sync.RWMutex

	var called bool
	var gotSize, gotTrigger float64
	var gotCancelOID int64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		gotSize, gotTrigger, gotCancelOID = size, triggerPx, cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999001, StopLossTriggerPx: triggerPx}, "", nil
	}

	wantTrigger := 102.0 * (1.0 - 0.75*5.0/100.0)

	n, _ := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 102.0, map[string]float64{"ETH": 0.2}, nil, nil, &mu, nil, newTestLogger(t))
	if n != 0 {
		t.Fatalf("trades = %d, want 0 (resting replacement, not immediate fill)", n)
	}
	if !called {
		t.Fatal("expected cancel+replace of resting SL after ratchet tighten (#1416)")
	}
	if gotCancelOID != 510751 {
		t.Errorf("cancel OID = %d, want 510751 (old resting SL)", gotCancelOID)
	}
	if !approxEq(gotSize, 0.2) {
		t.Errorf("size = %v, want 0.2 (residual after scale-out)", gotSize)
	}
	if !approxEq(gotTrigger, wantTrigger) {
		t.Errorf("trigger = %v, want %v (0.75×ATR from HWM)", gotTrigger, wantTrigger)
	}
	pos := st.Positions["ETH"]
	if pos.StopLossOID != 999001 {
		t.Errorf("StopLossOID = %d, want 999001", pos.StopLossOID)
	}
	if !approxEq(pos.StopLossTriggerPx, wantTrigger) {
		t.Errorf("StopLossTriggerPx = %v, want %v", pos.StopLossTriggerPx, wantTrigger)
	}
}

func TestRunTrailingStopUpdateAfterRatchetTighten_PaperUpdatesVirtualTrigger(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	var called bool
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		return &HyperliquidStopLossUpdateResult{StopLossOID: 1, StopLossTriggerPx: triggerPx}, "", nil
	}

	postTP := 0.75
	minMove := 0.1
	sc := StrategyConfig{
		ID: "hl-paper-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:                   []string{"x.py", "ETH", "1h"},
		CloseStrategy:          &StrategyRef{Name: trailingTPRatchetCloseName},
		TrailingStopATRMult:    floatPtr(2.5),
		TrailingStopMinMovePct: &minMove,
	}
	st := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
		"ETH": {
			Symbol: "ETH", Side: "long", Quantity: 0.2, InitialQuantity: 1.0,
			AvgCost: 100, EntryATR: 5, RiskAnchorPrice: 100,
			StopLossTriggerPx: 96.0, StopLossHighWaterPx: 102.0,
			PostTPTrailingATRMult: &postTP,
		},
	}}
	var mu sync.RWMutex
	wantTrigger := 102.0 * (1.0 - 0.75*5.0/100.0)

	n, _ := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 102.0, nil, nil, nil, &mu, nil, newTestLogger(t))
	if n != 0 {
		t.Fatalf("trades = %d, want 0", n)
	}
	if called {
		t.Fatal("paper mode must not invoke the exchange stop-loss script")
	}
	pos := st.Positions["ETH"]
	if !approxEq(pos.StopLossTriggerPx, wantTrigger) {
		t.Fatalf("paper StopLossTriggerPx = %v, want %v (#1416 paper path)", pos.StopLossTriggerPx, wantTrigger)
	}
}

func TestRunTrailingStopUpdateAfterRatchetTighten_UsesResidualNotPreCloseOnChain(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	postTP := 0.75
	minMove := 0.1
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:                []string{"x.py", "ETH", "1h", "--mode=live"},
		CloseStrategy:       &StrategyRef{Name: trailingTPRatchetCloseName},
		TrailingStopATRMult: floatPtr(2.5), TrailingStopMinMovePct: &minMove,
	}
	st := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
		"ETH": {
			Symbol: "ETH", Side: "long", Quantity: 0.2, InitialQuantity: 1.0,
			AvgCost: 100, EntryATR: 5, RiskAnchorPrice: 100,
			StopLossOID: 7, StopLossTriggerPx: 96, StopLossHighWaterPx: 102,
			PostTPTrailingATRMult: &postTP,
		},
	}}
	var mu sync.RWMutex
	var gotSize float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotSize = size
		return &HyperliquidStopLossUpdateResult{StopLossOID: 8, StopLossTriggerPx: triggerPx}, "", nil
	}
	runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 102, map[string]float64{"ETH": 1.0}, nil, nil, &mu, nil, newTestLogger(t))
	if !approxEq(gotSize, 0.2) {
		t.Fatalf("SL size = %v, want residual 0.2 (not stale on-chain 1.0)", gotSize)
	}
}
