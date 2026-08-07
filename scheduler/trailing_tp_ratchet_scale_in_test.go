package main

import (
	"sync"
	"testing"
)

// #1416 (review round 2): a scale-in ADD cycle carries Signal != 0, so the
// manage-path ratchet never runs. If price on that same cycle also clears a
// ratchet tier, the tighter trail must still be stamped and the resting stop
// moved on that cycle — the add branch must not defer it to a later Signal==0
// cycle, and it must do it in ONE cancel+replace sized to the post-add total.

// scaleInRatchetStrategy builds a live HL perps strategy with a two-rung
// ratchet ladder over the issue's real geometry (ATR ~0.56% of price), so a
// single tier step lands under the shipped 0.5% min-move default.
func scaleInRatchetStrategy() StrategyConfig {
	return StrategyConfig{
		ID: "hl-scalein-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py",
		Args:         []string{"x.py", "ETH", "1h", "--mode=live"},
		AllowScaleIn: true,
		CloseStrategy: &StrategyRef{
			Name: trailingTPRatchetCloseName,
			Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
				},
			},
		},
		TrailingStopATRMult: floatPtr(2.5),
		// TrailingStopMinMovePct unset -> defaultTrailingStopMinMovePct (0.5).
	}
}

// scaleInRatchetState builds a post-add position: the add already blended
// AvgCost and set ScaleInResizePending, while RiskAnchorPrice stays frozen at
// the first entry (#873).
func scaleInRatchetState(side string, postAddQty, highWater, trigger, postTPMult float64) *StrategyState {
	pos := &Position{
		Symbol: "ETH", Side: side, Quantity: postAddQty, InitialQuantity: postAddQty,
		AvgCost: ratchetTestAnchor + 3, EntryATR: ratchetTestEntryATR, RiskAnchorPrice: ratchetTestAnchor,
		StopLossOID: 4242, StopLossHighWaterPx: highWater, StopLossTriggerPx: trigger,
		ScaleInResizePending: true, ScaleInCount: 1, SLAdjustedTiersProcessed: 1,
	}
	if postTPMult > 0 {
		m := postTPMult
		pos.PostTPTrailingATRMult = &m
	}
	return &StrategyState{ID: "hl-scalein-eth", Platform: "hyperliquid", Type: "perps",
		Positions: map[string]*Position{"ETH": pos}}
}

// (a) + (c): an add that also clears a tier resizes AND tightens in exactly one
// replace, at the post-add quantity, on both sides.
func TestScaleInResizeTrailingSLNow_AddPlusTightenIsOneReplace(t *testing.T) {
	cases := []struct {
		name, side string
		highWater  float64
	}{
		{"long", "long", 1900.0},
		{"short", "short", 1840.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := runHyperliquidUpdateStopLossFunc
			defer func() { runHyperliquidUpdateStopLossFunc = old }()

			oldTrigger := trailingTriggerFor(c.side, c.highWater, 2.5)
			wantTrigger := trailingTriggerFor(c.side, c.highWater, 2.0)
			sc := scaleInRatchetStrategy()
			// Post-add total 0.3 = pre-add on-chain 0.2 + confirmed add 0.1.
			st := scaleInRatchetState(c.side, 0.3, c.highWater, oldTrigger, 2.0)
			var mu sync.RWMutex

			calls := 0
			var gotSize, gotTrigger float64
			var gotCancelOID int64
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				calls++
				gotSize, gotTrigger, gotCancelOID = size, triggerPx, cancelStopLossOID
				return &HyperliquidStopLossUpdateResult{StopLossOID: 5555, StopLossTriggerPx: triggerPx}, "", nil
			}

			scaleInResizeTrailingSLNow(sc, st, "ETH", c.highWater,
				map[string]float64{"ETH": 0.2}, 0.1, true, &mu, nil, newTestLogger(t))

			if calls != 1 {
				t.Fatalf("cancel+replace calls = %d, want exactly 1 (resize and tighten in one pass)", calls)
			}
			if !approxEq(gotSize, 0.3) {
				t.Errorf("size = %v, want 0.3 (post-add total)", gotSize)
			}
			if gotCancelOID != 4242 {
				t.Errorf("cancel OID = %d, want 4242", gotCancelOID)
			}
			if !approxEq(gotTrigger, wantTrigger) {
				t.Fatalf("trigger = %v, want %v (new 2.0xATR trail, not the old 2.5x %v)", gotTrigger, wantTrigger, oldTrigger)
			}
			if c.side == "long" && gotTrigger <= oldTrigger {
				t.Errorf("long tighten must raise the trigger: got %v, old %v", gotTrigger, oldTrigger)
			}
			if c.side == "short" && gotTrigger >= oldTrigger {
				t.Errorf("short tighten must lower the trigger: got %v, old %v", gotTrigger, oldTrigger)
			}
			if pos := st.Positions["ETH"]; pos.ScaleInResizePending {
				t.Error("ScaleInResizePending must clear after a confirmed resize")
			}
		})
	}
}

// (b): an add with no tier clear keeps the #873 behavior — one forced replace at
// the grown size, no multiplier change and no trigger move.
func TestScaleInResizeTrailingSLNow_AddWithoutTightenIsUnchanged(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	highWater := 1900.0
	// Resting trigger already matches the live trail distance: nothing to tighten.
	trigger := trailingTriggerFor("long", highWater, 2.0)
	sc := scaleInRatchetStrategy()
	st := scaleInRatchetState("long", 0.3, highWater, trigger, 2.0)
	var mu sync.RWMutex

	calls := 0
	var gotSize, gotTrigger float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		calls++
		gotSize, gotTrigger = size, triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 5556, StopLossTriggerPx: triggerPx}, "", nil
	}

	scaleInResizeTrailingSLNow(sc, st, "ETH", highWater,
		map[string]float64{"ETH": 0.2}, 0.1, false, &mu, nil, newTestLogger(t))

	if calls != 1 {
		t.Fatalf("cancel+replace calls = %d, want 1 (#873 forced resize)", calls)
	}
	if !approxEq(gotSize, 0.3) {
		t.Errorf("size = %v, want 0.3 (grown total)", gotSize)
	}
	if !approxEq(gotTrigger, trigger) {
		t.Errorf("trigger = %v, want %v unchanged (no spurious tighten)", gotTrigger, trigger)
	}
}

// A forced resize must never re-arm a trigger LOOSER than the current trail
// implies. Covers the tail of the #621 capped-qty deferral: an earlier cycle
// stamped the tighter multiplier but could not place it, so this pass — which
// is cancelling and re-placing anyway — has to adopt the tighter trigger even
// with no tighten event of its own.
func TestScaleInResizeTrailingSLNow_ForcedResizeAdoptsPendingTighten(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	highWater := 1900.0
	staleTrigger := trailingTriggerFor("long", highWater, 2.5) // placed under the OLD trail
	sc := scaleInRatchetStrategy()
	st := scaleInRatchetState("long", 0.3, highWater, staleTrigger, 2.0) // stamped 2.0x, unplaced
	var mu sync.RWMutex

	var gotTrigger float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotTrigger = triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 5557, StopLossTriggerPx: triggerPx}, "", nil
	}

	// ratchetTightened=false: this cycle saw no tier clear of its own.
	scaleInResizeTrailingSLNow(sc, st, "ETH", highWater,
		map[string]float64{"ETH": 0.2}, 0.1, false, &mu, nil, newTestLogger(t))

	if want := trailingTriggerFor("long", highWater, 2.0); !approxEq(gotTrigger, want) {
		t.Fatalf("trigger = %v, want %v — a forced resize must not re-place the stale wide trigger %v",
			gotTrigger, want, staleTrigger)
	}
}

// The add branch itself must apply the ratchet and hand the alert back, so the
// caller can DM the owner and drive the same-cycle walker.
func TestExecuteHyperliquidScaleInDeferredOpen_AppliesRatchet(t *testing.T) {
	mk := func(mark float64) (StrategyConfig, *StrategyState, *HyperliquidResult) {
		sc := scaleInRatchetStrategy()
		sc.Args = []string{"x.py", "ETH", "1h"} // paper: execResult is nil
		st := &StrategyState{ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 10000,
			Positions: map[string]*Position{
				"ETH": {
					Symbol: "ETH", Side: "long", Quantity: 0.2, InitialQuantity: 0.2,
					AvgCost: ratchetTestAnchor, EntryATR: ratchetTestEntryATR, RiskAnchorPrice: ratchetTestAnchor,
					StopLossHighWaterPx: mark, SLAdjustedTiersProcessed: 1,
				},
			}}
		return sc, st, &HyperliquidResult{Symbol: "ETH", Signal: 1}
	}

	// 3.0 ATR of profit clears the second rung -> trail tightens 2.5x -> 1.0x.
	clearing := ratchetTestAnchor + 3.0*ratchetTestEntryATR
	sc, st, res := mk(clearing)
	trades, _, _, alert := executeHyperliquidScaleInDeferredOpen(sc, st, res, nil, "BUY", clearing, 0.1, newTestLogger(t))
	if trades == 0 {
		t.Fatal("expected the add to book a trade")
	}
	if alert == nil {
		t.Fatal("expected a ratchet tighten alert from an add cycle that clears a tier (#1416)")
	}
	pos := st.Positions["ETH"]
	if pos.PostTPTrailingATRMult == nil || !approxEq(*pos.PostTPTrailingATRMult, 1.0) {
		t.Fatalf("stamped mult = %v, want 1.0", pos.PostTPTrailingATRMult)
	}
	if !approxEq(pos.Quantity, 0.3) {
		t.Errorf("post-add quantity = %v, want 0.3", pos.Quantity)
	}
	if !approxEq(pos.RiskAnchorPrice, ratchetTestAnchor) {
		t.Errorf("RiskAnchorPrice = %v, want %v frozen at the first entry", pos.RiskAnchorPrice, ratchetTestAnchor)
	}

	// An add with no tier newly cleared must not alert and must not stamp.
	belowNextTier := ratchetTestAnchor + 1.5*ratchetTestEntryATR
	sc, st, res = mk(belowNextTier)
	_, _, _, alert = executeHyperliquidScaleInDeferredOpen(sc, st, res, nil, "BUY", belowNextTier, 0.1, newTestLogger(t))
	if alert != nil {
		t.Fatalf("no tier newly cleared: want nil alert, got %+v", alert)
	}
	if st.Positions["ETH"].PostTPTrailingATRMult != nil {
		t.Errorf("no tier cleared: PostTPTrailingATRMult must stay unstamped")
	}
}
