package main

import (
	"sync"
	"testing"
)

// armTrailingStopAtOpenNow places the initial trailing SL on the SAME cycle as a
// fresh open for an ATR-trailing owner (#885) — those owners get no inline SL at
// the execute call (EffectiveStopLossPct defers to 0) and are not handled by the
// post-trade protection sync (buildHyperliquidProtectionPlan never reads the
// trailing fields), so without this they stay naked until the next Signal==0
// walker cycle.
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

	// Happy path: live ATR-trailing owner with no resting SL → arm inline at the
	// AvgCost-seeded trigger (2000 * (1 - 5%) = 1900), full filled qty, no cancel.
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

	// Guard: a resting SL already exists — scalar trailing_stop_pct places it
	// inline at the execute order, fixed/regime ATR via the post-trade sync — so
	// arming again would double-place. No-op.
	called = false
	st = mkState(123, 1950)
	if n, _ := armTrailingStopAtOpenNow(sc, st, "ETH", 2000, map[string]float64{"ETH": 0}, 2, &mu, nil, newTestLogger(t)); n != 0 || called {
		t.Errorf("existing-SL: expected no-op, got trades=%d called=%v", n, called)
	}

	// Guard: not live (paper) → no subprocess call.
	called = false
	scPaper := sc
	scPaper.Args = []string{"x.py", "ETH", "1h"}
	st = mkState(0, 0)
	armTrailingStopAtOpenNow(scPaper, st, "ETH", 2000, map[string]float64{"ETH": 0}, 2, &mu, nil, newTestLogger(t))
	if called {
		t.Errorf("not-live: expected no subprocess call")
	}

	// Guard: non-trailing owner (fixed ATR) — the post-trade protection sync
	// already armed its SL — so this no-ops on the effectiveTrailingStopPct gate.
	called = false
	scFixed := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: liveArgs, StopLossATRMult: &fixed}
	st = mkState(0, 0)
	armTrailingStopAtOpenNow(scFixed, st, "ETH", 2000, map[string]float64{"ETH": 0}, 2, &mu, nil, newTestLogger(t))
	if called {
		t.Errorf("fixed-ATR: expected no-op (post-trade sync owns the SL)")
	}

	// Guard: still size-capped after correcting on-chain qty (preOpen 0 + fill
	// 0.5 < virtual 2) → defer to the next walker cycle, no placement.
	called = false
	st = mkState(0, 0)
	armTrailingStopAtOpenNow(sc, st, "ETH", 2000, map[string]float64{"ETH": 0}, 0.5, &mu, nil, newTestLogger(t))
	if called {
		t.Errorf("capped: expected deferral, got a subprocess call")
	}
	if st.Positions["ETH"].StopLossOID != 0 {
		t.Errorf("capped: SL OID set (%d) despite deferral", st.Positions["ETH"].StopLossOID)
	}
}
