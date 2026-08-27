package main

import (
	"sync"
	"testing"
)

func TestArmTrailingStopAtOpenNow(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	trail := 2.0
	fixed := 1.5
	liveArgs := []string{"x.py", "ETH", "1h", "--mode=live"}
	mkState := func(oid int64, trigger float64) *StrategyState {
		return &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "long", Quantity: 2, InitialQuantity: 2, AvgCost: 2000, EntryATR: 50, RiskAnchorPrice: 2000, StopLossOID: oid, StopLossTriggerPx: trigger},
		}}
	}
	var mu sync.RWMutex

	var called bool
	var gotSize, gotTrigger float64
	var gotCancelOID int64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		gotSize, gotTrigger, gotCancelOID = size, triggerPx, cancelStopLossOID
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx}, "", nil
	}
	sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: liveArgs, TrailingStopATRMult: &trail}
	st := mkState(0, 0)
	if n, _ := armTrailingStopAtOpenNow(sc, st, "ETH", 2000, map[string]float64{"ETH": 0}, 2, &mu, nil, newTestLogger(t)); n != 0 {
		t.Errorf("trades = %d, want 0 (resting placement is not an immediate fill)", n)
	}
	if !called {
		t.Fatalf("expected a subprocess call to place the inline trailing SL")
	}
	if gotCancelOID != 0 {
		t.Errorf("cancel OID = %d, want 0 (fresh open has nothing to cancel)", gotCancelOID)
	}
	if !approxEq(gotSize, 2) {
		t.Errorf("size = %v, want 2 (full filled qty)", gotSize)
	}
	if !approxEq(gotTrigger, 1900) {
		t.Errorf("trigger = %v, want 1900 (AvgCost 2000 less 5%% ATR-trailing)", gotTrigger)
	}
	if got := st.Positions["ETH"].StopLossOID; got != 999 {
		t.Errorf("pos.StopLossOID = %d, want 999 (armed inline)", got)
	}
	if got := st.Positions["ETH"].StopLossTriggerPx; !approxEq(got, 1900) {
		t.Errorf("pos.StopLossTriggerPx = %v, want 1900", got)
	}

	called = false
	st = mkState(123, 1950)
	if n, _ := armTrailingStopAtOpenNow(sc, st, "ETH", 2000, map[string]float64{"ETH": 0}, 2, &mu, nil, newTestLogger(t)); n != 0 || called {
		t.Errorf("existing-SL: expected no-op, got trades=%d called=%v", n, called)
	}

	called = false
	scPaper := sc
	scPaper.Args = []string{"x.py", "ETH", "1h"}
	st = mkState(0, 0)
	armTrailingStopAtOpenNow(scPaper, st, "ETH", 2000, map[string]float64{"ETH": 0}, 2, &mu, nil, newTestLogger(t))
	if called {
		t.Errorf("not-live: expected no subprocess call")
	}

	called = false
	scFixed := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: liveArgs, StopLossATRMult: &fixed}
	st = mkState(0, 0)
	armTrailingStopAtOpenNow(scFixed, st, "ETH", 2000, map[string]float64{"ETH": 0}, 2, &mu, nil, newTestLogger(t))
	if called {
		t.Errorf("fixed-ATR: expected no-op (post-trade sync owns the SL)")
	}

	called = false
	st = mkState(0, 0)
	armTrailingStopAtOpenNow(sc, st, "ETH", 2000, map[string]float64{"ETH": 0}, 0.5, &mu, nil, newTestLogger(t))
	if called {
		t.Errorf("capped: expected deferral, got a subprocess call")
	}
	if st.Positions["ETH"].StopLossOID != 0 {
		t.Errorf("capped: SL OID set (%d) despite deferral", st.Positions["ETH"].StopLossOID)
	}

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: triggerPx}, "", nil
	}
	st = mkState(0, 0)
	n, d := armTrailingStopAtOpenNow(sc, st, "ETH", 2000, map[string]float64{"ETH": 0}, 2, &mu, nil, newTestLogger(t))
	if n != 1 {
		t.Errorf("immediate-fill: trades = %d, want 1 (close booked)", n)
	}
	if d == "" {
		t.Errorf("immediate-fill: expected a non-empty detail string")
	}
	if pos := st.Positions["ETH"]; pos != nil && pos.Quantity > 0 {
		t.Errorf("immediate-fill: position still open (qty=%.4f), want flat", pos.Quantity)
	}
}

func TestArmTrailingStopAtOpenNowRegime(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	var gotSize, gotTrigger float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotSize, gotTrigger = size, triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 777, StopLossTriggerPx: triggerPx}, "", nil
	}
	regimeBlock := &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{
		"trending": {ATR: 2.0},
		"ranging":  {ATR: 1.0},
	}}
	sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: []string{"x.py", "ETH", "1h", "--mode=live"}, TrailingStopATRRegime: regimeBlock}
	st := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Side: "long", Quantity: 2, InitialQuantity: 2, AvgCost: 2000, EntryATR: 50, RiskAnchorPrice: 2000, Regime: "trending"},
	}}
	var mu sync.RWMutex
	armTrailingStopAtOpenNow(sc, st, "ETH", 2000, map[string]float64{"ETH": 0}, 2, &mu, nil, newTestLogger(t))
	if !approxEq(gotSize, 2) {
		t.Errorf("size = %v, want 2", gotSize)
	}
	if !approxEq(gotTrigger, 1900) {
		t.Errorf("trigger = %v, want 1900 (regime trending 2.0x ATR)", gotTrigger)
	}
	if got := st.Positions["ETH"].StopLossOID; got != 777 {
		t.Errorf("pos.StopLossOID = %d, want 777 (regime owner armed inline)", got)
	}
}
