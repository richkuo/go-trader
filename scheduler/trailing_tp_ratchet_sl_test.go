package main

import (
	"sync"
	"testing"
)

// #1416: after a scale-out ratchet stamps a tighter PostTPTrailingATRMult, the
// resting SL must move same-cycle. These tests pin the helper that the perps
// execute path calls when ratchetAlert != nil.
func TestRunTrailingStopUpdateAfterRatchetTighten_LiveReplacesWiderTrigger(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	// Geometry mirrors the issue repro shape at smaller numbers:
	// avg 100, entry ATR 5, residual after 80% scale-out, old trail 2.5×ATR
	// (trigger left at prior HWM), new trail 0.75×ATR already stamped.
	postTP := 0.75
	minMove := 0.1 // below the expected move so min-move cannot mask the bug
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
			StopLossOID: 510751, StopLossTriggerPx: 96.0, // old wider trigger @ prior HWM
			StopLossHighWaterPx:      102.0, // fill/mark HWM after partial close
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

	// Intended trigger: HWM 102 * (1 - 0.75*5/100) = 102 * 0.9625 = 98.175
	wantTrigger := 102.0 * (1.0 - 0.75*5.0/100.0)

	n, _ := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 102.0, map[string]float64{"ETH": 0.2}, &mu, nil, newTestLogger(t))
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
	postTP := 0.75
	minMove := 0.1
	sc := StrategyConfig{
		ID: "hl-paper-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:                   []string{"x.py", "ETH", "1h"}, // paper: no --mode=live
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

	n, _ := runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 102.0, nil, &mu, nil, newTestLogger(t))
	if n != 0 {
		t.Fatalf("trades = %d, want 0", n)
	}
	pos := st.Positions["ETH"]
	if !approxEq(pos.StopLossTriggerPx, wantTrigger) {
		t.Fatalf("paper StopLossTriggerPx = %v, want %v (#1416 paper path)", pos.StopLossTriggerPx, wantTrigger)
	}
}

func TestRunTrailingStopUpdateAfterRatchetTighten_NoOpGuards(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	var called bool
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		return &HyperliquidStopLossUpdateResult{StopLossOID: 1, StopLossTriggerPx: triggerPx}, "", nil
	}

	postTP := 0.75
	liveArgs := []string{"x.py", "ETH", "1h", "--mode=live"}
	mk := func() (*StrategyConfig, *StrategyState) {
		sc := StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: liveArgs,
			CloseStrategy: &StrategyRef{Name: trailingTPRatchetCloseName}, TrailingStopATRMult: floatPtr(2.5),
		}
		st := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Side: "long", Quantity: 0.2, InitialQuantity: 1.0,
				AvgCost: 100, EntryATR: 5, RiskAnchorPrice: 100,
				StopLossOID: 7, StopLossTriggerPx: 96, StopLossHighWaterPx: 102,
				PostTPTrailingATRMult: &postTP,
			},
		}}
		return &sc, st
	}
	var mu sync.RWMutex

	// Flat residual → no-op (full close already booked).
	sc, st := mk()
	st.Positions["ETH"].Quantity = 0
	called = false
	runTrailingStopUpdateAfterRatchetTighten(*sc, st, "ETH", 102, map[string]float64{"ETH": 0}, &mu, nil, newTestLogger(t))
	if called {
		t.Error("flat position: expected no subprocess call")
	}

	// Non-HL platform → no-op.
	sc, st = mk()
	sc.Platform = "okx"
	called = false
	runTrailingStopUpdateAfterRatchetTighten(*sc, st, "ETH", 102, map[string]float64{"ETH": 0.2}, &mu, nil, newTestLogger(t))
	if called {
		t.Error("non-HL: expected no subprocess call")
	}

	// Mark missing → no-op.
	sc, st = mk()
	called = false
	runTrailingStopUpdateAfterRatchetTighten(*sc, st, "ETH", 0, map[string]float64{"ETH": 0.2}, &mu, nil, newTestLogger(t))
	if called {
		t.Error("mark<=0: expected no subprocess call")
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
	// Stale Phase-1 on-chain still shows pre-partial-close size (1.0); residual
	// virtual is 0.2 — hlSLEffectiveQty must pick the residual.
	runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", 102, map[string]float64{"ETH": 1.0}, &mu, nil, newTestLogger(t))
	if !approxEq(gotSize, 0.2) {
		t.Fatalf("SL size = %v, want residual 0.2 (not stale on-chain 1.0)", gotSize)
	}
}

func floatPtr(v float64) *float64 { return &v }
