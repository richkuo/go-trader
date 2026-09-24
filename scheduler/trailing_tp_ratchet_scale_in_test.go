package main

import (
	"sync"
	"testing"
)

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
	}
}

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
				map[string]float64{"ETH": 0.2}, nil, nil, 0.1, true, &mu, nil, newTestLogger(t))

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

func TestScaleInResizeTrailingSLNow_AddWithoutTightenIsUnchanged(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	highWater := 1900.0
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
		map[string]float64{"ETH": 0.2}, nil, nil, 0.1, false, &mu, nil, newTestLogger(t))

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

func TestScaleInResizeTrailingSLNow_ForcedResizeAdoptsPendingTighten(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	highWater := 1900.0
	staleTrigger := trailingTriggerFor("long", highWater, 2.5)
	sc := scaleInRatchetStrategy()
	st := scaleInRatchetState("long", 0.3, highWater, staleTrigger, 2.0)
	var mu sync.RWMutex

	var gotTrigger float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotTrigger = triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 5557, StopLossTriggerPx: triggerPx}, "", nil
	}

	scaleInResizeTrailingSLNow(sc, st, "ETH", highWater,
		map[string]float64{"ETH": 0.2}, nil, nil, 0.1, false, &mu, nil, newTestLogger(t))

	if want := trailingTriggerFor("long", highWater, 2.0); !approxEq(gotTrigger, want) {
		t.Fatalf("trigger = %v, want %v — a forced resize must not re-place the stale wide trigger %v",
			gotTrigger, want, staleTrigger)
	}
}

func TestExecuteHyperliquidScaleInDeferredOpen_AppliesRatchet(t *testing.T) {
	mk := func(mark float64) (StrategyConfig, *StrategyState, *HyperliquidResult) {
		sc := scaleInRatchetStrategy()
		sc.Args = []string{"x.py", "ETH", "1h"}
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

func TestExecuteHyperliquidScaleInDeferredOpen_LeavesHighWaterUntouched(t *testing.T) {
	sc := scaleInRatchetStrategy()
	sc.Args = []string{"x.py", "ETH", "1h"}
	const establishedHighWater = 1990.0
	st := &StrategyState{ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 10000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Side: "long", Quantity: 0.2, InitialQuantity: 0.2,
				AvgCost: ratchetTestAnchor, EntryATR: ratchetTestEntryATR, RiskAnchorPrice: ratchetTestAnchor,
				StopLossHighWaterPx: establishedHighWater, SLAdjustedTiersProcessed: 1,
			},
		}}
	addPrice := ratchetTestAnchor + 1.5*ratchetTestEntryATR
	if addPrice >= establishedHighWater {
		t.Fatalf("fixture invalid: add price %v must be below the high-water %v", addPrice, establishedHighWater)
	}
	executeHyperliquidScaleInDeferredOpen(sc, st, &HyperliquidResult{Symbol: "ETH", Signal: 1},
		nil, "BUY", addPrice, 0.1, newTestLogger(t))

	if got := st.Positions["ETH"].StopLossHighWaterPx; !approxEq(got, establishedHighWater) {
		t.Fatalf("StopLossHighWaterPx = %v, want %v untouched by the add", got, establishedHighWater)
	}

	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()
	var gotTrigger float64
	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		gotTrigger = triggerPx
		return &HyperliquidStopLossUpdateResult{StopLossOID: 6001, StopLossTriggerPx: triggerPx}, "", nil
	}
	pos := st.Positions["ETH"]
	pos.StopLossHighWaterPx = 0
	live := scaleInRatchetStrategy()
	runHyperliquidTrailingStopUpdate(live, "ETH", "long", pos.Quantity, pos, ratchetTestAnchor-20,
		0, 0, 0, trailingReplacePolicy{}, nil, newTestLogger(t))
	if want := trailingTriggerFor("long", ratchetTestAnchor, 2.5); !approxEq(gotTrigger, want) {
		t.Fatalf("trigger = %v, want %v (seeded from the frozen anchor %v, not the blended AvgCost %v)",
			gotTrigger, want, ratchetTestAnchor, pos.AvgCost)
	}
}
