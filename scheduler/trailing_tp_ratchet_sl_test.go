package main

import (
	"math"
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
	// Paper must never reach the exchange script. Stub the live hook and assert
	// it stays untouched, so a regression that mis-detects paper as live fails
	// here instead of spawning Python from Go CI.
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
	if called {
		t.Fatal("paper mode must not invoke the exchange stop-loss script")
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

// --- #1416 min-move debounce interaction (default config, both sides) ---
//
// Geometry below mirrors the issue's live ETH position: anchor 1868.80 with
// EntryATR 10.4989, so one ATR is ~0.56% of price. A single tier step of
// Δmult = 0.5 therefore shifts the trigger only ~0.28% — under the SHIPPED
// default trailing_stop_min_move_pct of 0.5. These cases pin that a ratchet
// tighten still lands, and that nothing else loses the debounce.
const (
	ratchetTestAnchor   = 1868.80
	ratchetTestEntryATR = 10.4989
)

func ratchetMinMoveStrategy(live bool) StrategyConfig {
	args := []string{"x.py", "ETH", "1h"}
	if live {
		args = append(args, "--mode=live")
	}
	return StrategyConfig{
		ID: "hl-vwap-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: args,
		CloseStrategy:       &StrategyRef{Name: trailingTPRatchetCloseName},
		TrailingStopATRMult: floatPtr(2.5),
		// TrailingStopMinMovePct deliberately unset -> defaultTrailingStopMinMovePct (0.5).
	}
}

func ratchetMinMovePosition(side string, mult, highWater float64) *StrategyState {
	m := mult
	return &StrategyState{ID: "hl-vwap-eth", Positions: map[string]*Position{
		"ETH": {
			Symbol: "ETH", Side: side, Quantity: 0.2, InitialQuantity: 1.0,
			AvgCost: ratchetTestAnchor, EntryATR: ratchetTestEntryATR, RiskAnchorPrice: ratchetTestAnchor,
			StopLossOID: 510751, StopLossHighWaterPx: highWater,
			PostTPTrailingATRMult: &m, SLAdjustedTiersProcessed: 1,
		},
	}}
}

// trailingTriggerFor returns the trigger the walker should compute for the
// given side, high-water and ATR multiple under this fixture's geometry.
func trailingTriggerFor(side string, highWater, mult float64) float64 {
	pct := mult * ratchetTestEntryATR / ratchetTestAnchor
	if side == "short" {
		return highWater * (1.0 + pct)
	}
	return highWater * (1.0 - pct)
}

func TestRunTrailingStopUpdateAfterRatchetTighten_LandsUnderDefaultMinMove(t *testing.T) {
	cases := []struct {
		name      string
		side      string
		highWater float64
	}{
		{"long", "long", 1900.0},
		{"short", "short", 1840.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := runHyperliquidUpdateStopLossFunc
			defer func() { runHyperliquidUpdateStopLossFunc = old }()

			// Trail tightened 2.5x -> 2.0x ATR; the resting trigger still sits at
			// the 2.5x distance from the same high-water.
			oldTrigger := trailingTriggerFor(c.side, c.highWater, 2.5)
			wantTrigger := trailingTriggerFor(c.side, c.highWater, 2.0)

			// Sanity: this move MUST be under the shipped default, else the case
			// is not exercising the debounce at all.
			movePct := math.Abs(wantTrigger-oldTrigger) / oldTrigger * 100.0
			if movePct >= defaultTrailingStopMinMovePct {
				t.Fatalf("fixture invalid: move %.4f%% >= default min-move %.2f%%", movePct, defaultTrailingStopMinMovePct)
			}

			sc := ratchetMinMoveStrategy(true)
			st := ratchetMinMovePosition(c.side, 2.0, c.highWater)
			st.Positions["ETH"].StopLossTriggerPx = oldTrigger
			var mu sync.RWMutex

			calls := 0
			var gotTrigger float64
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				calls++
				gotTrigger = triggerPx
				return &HyperliquidStopLossUpdateResult{StopLossOID: 999001, StopLossTriggerPx: triggerPx}, "", nil
			}

			runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", c.highWater, map[string]float64{"ETH": 0.2}, &mu, nil, newTestLogger(t))

			if calls != 1 {
				t.Fatalf("cancel+replace calls = %d, want 1 (#1416: tighten must not be debounced)", calls)
			}
			if !approxEq(gotTrigger, wantTrigger) {
				t.Fatalf("trigger = %v, want %v (2.0xATR from high-water %v)", gotTrigger, wantTrigger, c.highWater)
			}
			// A tighten must move the stop toward the mark, never away from it.
			if c.side == "long" && gotTrigger <= oldTrigger {
				t.Errorf("long tighten must raise the trigger: got %v, old %v", gotTrigger, oldTrigger)
			}
			if c.side == "short" && gotTrigger >= oldTrigger {
				t.Errorf("short tighten must lower the trigger: got %v, old %v", gotTrigger, oldTrigger)
			}
			if got := st.Positions["ETH"].StopLossTriggerPx; !approxEq(got, wantTrigger) {
				t.Errorf("persisted StopLossTriggerPx = %v, want %v", got, wantTrigger)
			}
		})
	}
}

// A watermark-only cycle (no tier tightened -> ratchetAlert == nil -> the manage
// walker runs with a zero policy) must stay fully debounced: no forced replace,
// no subprocess.
func TestTrailingWalker_NoRatchetTighten_StaysDebounced(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	called := false
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		called = true
		return &HyperliquidStopLossUpdateResult{StopLossOID: 1, StopLossTriggerPx: triggerPx}, "", nil
	}

	sc := ratchetMinMoveStrategy(true)
	highWater := 1900.0
	pos := ratchetMinMovePosition("long", 2.0, highWater).Positions["ETH"]
	// Trigger already at the 2.0x distance; a sub-threshold high-water drift is
	// the only pending move.
	pos.StopLossTriggerPx = trailingTriggerFor("long", highWater, 2.0)
	drifted := highWater * 1.001 // +0.1% -> trigger shifts 0.1%, under the 0.5% default

	_, result, _ := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 0.2, pos, drifted, highWater,
		pos.StopLossTriggerPx, pos.StopLossOID, trailingReplacePolicy{}, nil, newTestLogger(t))
	if called || result != nil {
		t.Fatal("sub-threshold drift with no ratchet tighten must stay debounced")
	}
}

// The bypass drops the debounce, never the direction gate: a candidate that
// would WIDEN the stop must not replace even with ratchetTightened set.
func TestTrailingWalker_RatchetBypassNeverWidens(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		t.Run(side, func(t *testing.T) {
			highWater := 1900.0
			if side == "short" {
				highWater = 1840.0
			}
			// Resting trigger is already TIGHTER (1.0xATR) than what the current
			// 2.0x trail would compute, e.g. after a manual stop edit.
			currentTrigger := trailingTriggerFor(side, highWater, 1.0)
			trailingPct := 2.0 * ratchetTestEntryATR / ratchetTestAnchor * 100.0

			_, trigger, replace := computeTrailingStopUpdateInternal(
				side, highWater, highWater, trailingPct, defaultTrailingStopMinMovePct, currentTrigger, false, true)
			if replace || trigger != 0 {
				t.Fatalf("bypass must not widen: replace=%v trigger=%v (current %v)", replace, trigger, currentTrigger)
			}
		})
	}
}

// Two tiers clearing in one cycle: the ratchet stamps only the tightest
// multiplier, so the walker must issue exactly ONE cancel+replace at that
// multiplier — not one per tier.
func TestRunTrailingStopUpdateAfterRatchetTighten_TwoTiersOneCycleReplacesOnce(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	sc := ratchetMinMoveStrategy(true)
	sc.CloseStrategy = &StrategyRef{
		Name: trailingTPRatchetCloseName,
		Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
			},
		},
	}
	// Mark 2.5 ATR above the anchor clears BOTH tiers on this cycle.
	mark := ratchetTestAnchor + 2.5*ratchetTestEntryATR
	st := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
		"ETH": {
			Symbol: "ETH", Side: "long", Quantity: 0.2, InitialQuantity: 1.0,
			AvgCost: ratchetTestAnchor, EntryATR: ratchetTestEntryATR, RiskAnchorPrice: ratchetTestAnchor,
			StopLossOID: 510751, StopLossHighWaterPx: mark,
			StopLossTriggerPx: trailingTriggerFor("long", mark, 2.5),
		},
	}}
	var mu sync.RWMutex

	tightened, alert := applyTrailingTPRatchetToPosition(sc, st.Positions["ETH"], "ETH", mark, newTestLogger(t))
	if !tightened || alert == nil {
		t.Fatalf("expected a tighten alert from a two-tier cycle (got tightened=%v alert=%v)", tightened, alert)
	}
	if got := *st.Positions["ETH"].PostTPTrailingATRMult; !approxEq(got, 1.0) {
		t.Fatalf("stamped mult = %v, want 1.0 (tightest cleared tier)", got)
	}

	calls := 0
	var gotTrigger float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		gotTrigger = triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 999002, StopLossTriggerPx: triggerPx}, "", nil
	}
	runTrailingStopUpdateAfterRatchetTighten(sc, st, "ETH", mark, map[string]float64{"ETH": 0.2}, &mu, nil, newTestLogger(t))

	if calls != 1 {
		t.Fatalf("cancel+replace calls = %d, want exactly 1 for a two-tier cycle", calls)
	}
	if want := trailingTriggerFor("long", mark, 1.0); !approxEq(gotTrigger, want) {
		t.Fatalf("trigger = %v, want %v (tightest multiplier only)", gotTrigger, want)
	}
}
