package main

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
)

// hedgeTestLogger returns a StrategyLogger writing to buf (same-package
// test seam, mirrors hyperliquid_stop_loss_test.go).
func hedgeTestLogger(buf *bytes.Buffer) *StrategyLogger {
	return &StrategyLogger{stratID: "test", writer: buf}
}

// ---------------------------------------------------------------------------
// Phase C — decision core
// ---------------------------------------------------------------------------

func TestHedgeTargetDecision(t *testing.T) {
	longETH := func(h *HedgeConfig) StrategyConfig {
		sc := hlHedgeTestStrategy("hl-eth", h)
		return sc
	}
	enabled := &HedgeConfig{Enabled: true, Symbol: "BTC"}

	t.Run("not enabled", func(t *testing.T) {
		sc := longETH(&HedgeConfig{Enabled: false, Symbol: "BTC"})
		snap := hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long"}
		a := hedgeTargetDecision(sc, snap, 3000, 90000)
		if a.Kind != hedgeActionNone {
			t.Errorf("kind = %s, want none", a.Kind)
		}
	})

	t.Run("both flat", func(t *testing.T) {
		a := hedgeTargetDecision(longETH(enabled), hedgeSnapshot{}, 3000, 90000)
		if a.Kind != hedgeActionNone {
			t.Errorf("kind = %s, want none", a.Kind)
		}
	})

	t.Run("primary flat + hedge held → closeFull", func(t *testing.T) {
		snap := hedgeSnapshot{HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionCloseFull {
			t.Errorf("kind = %s, want closeFull", a.Kind)
		}
	})

	t.Run("primary held + hedge flat → open inverse, notional sizing", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 1.5, PrimaryAvgCost: 3000, PrimarySide: "long"}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionOpen || a.Side != "sell" {
			t.Fatalf("kind/side = %s/%s, want open/sell", a.Kind, a.Side)
		}
		// 1.5 ETH × $3000 × 1.0 / $90000 = 0.05 BTC
		if want := 1.5 * 3000.0 / 90000.0; a.Qty != want {
			t.Errorf("qty = %g, want %g", a.Qty, want)
		}
	})

	t.Run("ratio scales the open size", func(t *testing.T) {
		sc := longETH(&HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 0.5})
		snap := hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long"}
		a := hedgeTargetDecision(sc, snap, 3000, 90000)
		if want := 1.5 * 3000.0 * 0.5 / 90000.0; a.Qty != want {
			t.Errorf("qty = %g, want %g (ratio 0.5)", a.Qty, want)
		}
	})

	t.Run("short primary → buy hedge", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "short"}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionOpen || a.Side != "buy" {
			t.Errorf("kind/side = %s/%s, want open/buy", a.Kind, a.Side)
		}
	})

	t.Run("hedge on wrong side → closeFull alert", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "long", HedgeBasis: 1.5}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionCloseFull || !strings.Contains(a.Reason, "not the inverse") {
			t.Errorf("kind/reason = %s/%q, want closeFull + inverse alert", a.Kind, a.Reason)
		}
	})

	t.Run("unusable primary side → fail-closed none", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "flat-ish"}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionNone || a.Reason == "" {
			t.Errorf("kind/reason = %s/%q, want none + reason", a.Kind, a.Reason)
		}
	})

	t.Run("unusable prices → fail-closed none", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long"}
		for _, px := range [][2]float64{{0, 90000}, {-1, 90000}, {3000, 0}, {3000, -5}} {
			a := hedgeTargetDecision(longETH(enabled), snap, px[0], px[1])
			if a.Kind != hedgeActionNone || !strings.Contains(a.Reason, "unusable price") {
				t.Errorf("px=%v: kind/reason = %s/%q, want fail-closed none", px, a.Kind, a.Reason)
			}
		}
		// Same guard with a hedge leg held (manage path).
		snap.HedgeQty, snap.HedgeSide, snap.HedgeBasis = 0.05, "short", 1.5
		a := hedgeTargetDecision(longETH(enabled), snap, 0, 90000)
		if a.Kind != hedgeActionNone {
			t.Errorf("manage path: kind = %s, want none", a.Kind)
		}
	})

	t.Run("primary grew → add delta-notional", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 2.0, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionAdd || a.Side != "sell" {
			t.Fatalf("kind/side = %s/%s, want add/sell", a.Kind, a.Side)
		}
		// delta 0.5 ETH × $3000 × 1.0 / $90000 = 0.01666… BTC
		if want := 0.5 * 3000.0 / 90000.0; a.Qty != want {
			t.Errorf("qty = %g, want %g", a.Qty, want)
		}
	})

	t.Run("add below min notional defers", func(t *testing.T) {
		// delta 0.0001 ETH → $0.30 notional.
		snap := hedgeSnapshot{PrimaryQty: 1.5001, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionNone || !strings.Contains(a.Reason, "below min") {
			t.Errorf("kind/reason = %s/%q, want defer", a.Kind, a.Reason)
		}
	})

	t.Run("primary shrank → proportional reduce", func(t *testing.T) {
		// basis 1.5 → primary 0.75: reduce half the hedge leg.
		snap := hedgeSnapshot{PrimaryQty: 0.75, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionReduce {
			t.Fatalf("kind = %s, want reduce (%s)", a.Kind, a.Reason)
		}
		if want := 0.025; a.Qty != want {
			t.Errorf("qty = %g, want %g", a.Qty, want)
		}
	})

	t.Run("reduce covering the whole leg → closeFull (no dust residue)", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 0.0001, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionCloseFull {
			t.Errorf("kind = %s, want closeFull (%s)", a.Kind, a.Reason)
		}
	})

	t.Run("reduce below min notional defers, basis not advanced", func(t *testing.T) {
		// basis 1.5 → primary 1.4: frac = 0.1/1.5 → 0.00333 BTC ≈ $300... use
		// a smaller delta: basis 1.5 → primary 1.49999 → 0.0000333 BTC ≈ $3.
		snap := hedgeSnapshot{PrimaryQty: 1.49999, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionNone || !strings.Contains(a.Reason, "below min") {
			t.Errorf("kind/reason = %s/%q, want defer", a.Kind, a.Reason)
		}
	})

	t.Run("within tolerance → none", func(t *testing.T) {
		snap := hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}
		a := hedgeTargetDecision(longETH(enabled), snap, 3000, 90000)
		if a.Kind != hedgeActionNone {
			t.Errorf("kind = %s, want none", a.Kind)
		}
	})

	t.Run("mark drift alone never re-trades", func(t *testing.T) {
		// Same qtys, wildly different marks → still none (qty-event mirroring).
		snap := hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}
		a := hedgeTargetDecision(longETH(enabled), snap, 3600, 60000)
		if a.Kind != hedgeActionNone {
			t.Errorf("kind = %s, want none — mark drift must not re-trade", a.Kind)
		}
	})
}

func TestHedgeOrderSkipReason(t *testing.T) {
	enabled := &HedgeConfig{Enabled: true, Symbol: "BTC"}
	sc := hlHedgeTestStrategy("hl-eth", enabled)
	openAction := hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "sell"}

	cases := []struct {
		name    string
		sc      StrategyConfig
		action  hedgeAction
		snap    hedgeSnapshot
		hedgePx float64
		want    string // "" = proceed; else substring
	}{
		{"none action", sc, hedgeAction{Kind: hedgeActionNone}, hedgeSnapshot{}, 90000, "no hedge action"},
		{"not enabled", hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: false, Symbol: "BTC"}), openAction, hedgeSnapshot{}, 90000, "not enabled"},
		{"open ok", sc, openAction, hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long"}, 90000, ""},
		{"open while hedge held", sc, openAction, hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short"}, 90000, "already open"},
		{"open zero qty", sc, hedgeAction{Kind: hedgeActionOpen, Qty: 0, Side: "sell"}, hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long"}, 90000, "zero/negative"},
		{"open no mark", sc, openAction, hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long"}, 0, "no hedge mark"},
		{"open bad side", sc, hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "hold"}, hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long"}, 90000, "bad hedge order side"},
		{"add ok", sc, hedgeAction{Kind: hedgeActionAdd, Qty: 0.01, Side: "sell"}, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}, 90000, ""},
		{"add while hedge flat", sc, hedgeAction{Kind: hedgeActionAdd, Qty: 0.01, Side: "sell"}, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long"}, 90000, "wanted open"},
		{"add with side drift", sc, hedgeAction{Kind: hedgeActionAdd, Qty: 0.01, Side: "sell"}, hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "long", HedgeBasis: 1.5}, 90000, "drifted"},
		{"reduce ok", sc, hedgeAction{Kind: hedgeActionReduce, Qty: 0.025}, hedgeSnapshot{PrimaryQty: 0.75, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}, 90000, ""},
		{"reduce while flat", sc, hedgeAction{Kind: hedgeActionReduce, Qty: 0.025}, hedgeSnapshot{PrimaryQty: 0.75, PrimarySide: "long"}, 90000, "no hedge leg"},
		{"reduce zero qty", sc, hedgeAction{Kind: hedgeActionReduce, Qty: 0}, hedgeSnapshot{PrimaryQty: 0.75, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}, 90000, "zero/negative"},
		{"closeFull ok", sc, hedgeAction{Kind: hedgeActionCloseFull}, hedgeSnapshot{HedgeQty: 0.05, HedgeSide: "short"}, 90000, ""},
		{"closeFull while flat", sc, hedgeAction{Kind: hedgeActionCloseFull}, hedgeSnapshot{}, 90000, "no hedge leg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hedgeOrderSkipReason(tc.sc, tc.action, tc.snap, tc.hedgePx)
			if tc.want == "" && got != "" {
				t.Errorf("skip reason = %q, want proceed", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("skip reason = %q, want substring %q", got, tc.want)
			}
		})
	}
}

func TestCaptureHedgeSnapshot(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	snap := captureHedgeSnapshot(s, sc)
	if snap.PrimaryQty != 1.5 || snap.PrimarySide != "long" || snap.PrimaryAvgCost != 3000 {
		t.Errorf("primary snapshot = %+v", snap)
	}
	if snap.HedgeQty != 0.05 || snap.HedgeSide != "short" || snap.HedgeBasis != 1.5 || snap.HedgeAvgCost != 90000 {
		t.Errorf("hedge snapshot = %+v", snap)
	}
	if got := captureHedgeSnapshot(nil, sc); got != (hedgeSnapshot{}) {
		t.Errorf("nil state should give zero snapshot, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Phase D — execution (stubbed seams) and booking
// ---------------------------------------------------------------------------

func TestRunHedgeOrder_OpenFlatPassesOwnMarginLeverage(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC", MarginMode: "cross", Leverage: 3})
	var gotMargin string
	var gotLeverage, gotSize float64
	var gotSide, gotSymbol string
	var gotSL float64
	prev := runHyperliquidHedgeExecuteFn
	defer func() { runHyperliquidHedgeExecuteFn = prev }()
	runHyperliquidHedgeExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		gotSymbol, gotSide, gotSize, gotSL, gotMargin, gotLeverage = symbol, side, size, stopLossPct, marginMode, leverage
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
			Fill: &HyperliquidFill{AvgPx: 90050, TotalSz: 0.05, OID: 4242, Fee: 2.25},
		}}, "", nil
	}

	snap := hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long"} // hedge flat
	res := runHedgeOrder(sc, hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "sell"}, snap, nil, nil)
	if !res.OK || res.FillPx != 90050 || res.FillQty != 0.05 || res.FillOID != 4242 || res.FillFee != 2.25 {
		t.Errorf("result = %+v", res)
	}
	if gotSymbol != "BTC" || gotSide != "sell" || gotSize != 0.05 {
		t.Errorf("order = %s %s %g", gotSymbol, gotSide, gotSize)
	}
	if gotSL != 0 {
		t.Errorf("hedge open must carry no SL, got stopLossPct=%g", gotSL)
	}
	if gotMargin != "cross" || gotLeverage != 3 {
		t.Errorf("flat hedge open must pass its own margin/leverage, got %q/%g", gotMargin, gotLeverage)
	}
}

func TestRunHedgeOrder_AddOmitsMarginLeverage(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC", MarginMode: "cross", Leverage: 3})
	var gotMargin string
	var gotLeverage float64
	prev := runHyperliquidHedgeExecuteFn
	defer func() { runHyperliquidHedgeExecuteFn = prev }()
	runHyperliquidHedgeExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		gotMargin, gotLeverage = marginMode, leverage
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
			Fill: &HyperliquidFill{AvgPx: 90000, TotalSz: size},
		}}, "", nil
	}

	// Hedge leg already held → HL rejects update_leverage on an open
	// position, so margin/leverage must NOT be passed.
	snap := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}
	res := runHedgeOrder(sc, hedgeAction{Kind: hedgeActionAdd, Qty: 0.016, Side: "sell"}, snap, nil, nil)
	if !res.OK {
		t.Fatalf("result = %+v", res)
	}
	if gotMargin != "" || gotLeverage != 0 {
		t.Errorf("add on an open hedge leg must omit margin/leverage, got %q/%g", gotMargin, gotLeverage)
	}
}

func TestRunHedgeOrder_Failures(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	snap := hedgeSnapshot{PrimaryQty: 1.5, PrimarySide: "long"}

	prev := runHyperliquidHedgeExecuteFn
	defer func() { runHyperliquidHedgeExecuteFn = prev }()

	// Subprocess/venue error.
	runHyperliquidHedgeExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return nil, "stderr", fmt.Errorf("insufficient margin")
	}
	if res := runHedgeOrder(sc, hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "sell"}, snap, nil, nil); res.OK || res.ErrMsg == "" {
		t.Errorf("venue error: result = %+v, want failure", res)
	}

	// Exit 0 but no fill.
	runHyperliquidHedgeExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{}}, "", nil
	}
	if res := runHedgeOrder(sc, hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "sell"}, snap, nil, nil); res.OK || res.ErrMsg == "" {
		t.Errorf("no fill: result = %+v, want failure", res)
	}
}

func TestRunHedgeOrder_CloseSizing(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	var gotSz *float64
	var gotOIDs []int64
	prev := runHyperliquidHedgeCloseFn
	defer func() { runHyperliquidHedgeCloseFn = prev }()
	runHyperliquidHedgeCloseFn = func(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
		gotSz, gotOIDs = partialSz, cancelStopLossOIDs
		sz := 0.0
		if partialSz != nil {
			sz = *partialSz
		}
		return &HyperliquidCloseResult{Close: &HyperliquidClose{
			Fill: &HyperliquidCloseFill{AvgPx: 91000, TotalSz: sz, OID: 77, Fee: 1.5},
		}}, "", nil
	}

	snap := hedgeSnapshot{PrimaryQty: 1, PrimarySide: "long", HedgeQty: 0.05, HedgeSide: "short", HedgeBasis: 1.5}

	// closeFull sizes to the HELD qty (not action.Qty).
	res := runHedgeOrder(sc, hedgeAction{Kind: hedgeActionCloseFull}, snap, nil, nil)
	if !res.OK || gotSz == nil || *gotSz != 0.05 {
		t.Errorf("closeFull: result=%+v sz=%v", res, gotSz)
	}
	if len(gotOIDs) != 0 {
		t.Errorf("hedge close must carry no cancel OIDs, got %v", gotOIDs)
	}
	if res.FillPx != 91000 || res.FillOID != 77 || res.FillFee != 1.5 {
		t.Errorf("close fill = %+v", res)
	}

	// reduce larger than held clamps to held.
	res = runHedgeOrder(sc, hedgeAction{Kind: hedgeActionReduce, Qty: 0.5}, snap, nil, nil)
	if !res.OK || gotSz == nil || *gotSz != 0.05 {
		t.Errorf("oversized reduce must clamp to held: result=%+v sz=%v", res, gotSz)
	}

	// reduce within held stays as requested.
	res = runHedgeOrder(sc, hedgeAction{Kind: hedgeActionReduce, Qty: 0.02}, snap, nil, nil)
	if !res.OK || gotSz == nil || *gotSz != 0.02 {
		t.Errorf("reduce: result=%+v sz=%v", res, gotSz)
	}
}

func TestRunHedgeOrder_CloseAlreadyFlatAndNoFill(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	snap := hedgeSnapshot{HedgeQty: 0.05, HedgeSide: "short"}
	prev := runHyperliquidHedgeCloseFn
	defer func() { runHyperliquidHedgeCloseFn = prev }()

	runHyperliquidHedgeCloseFn = func(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
		return &HyperliquidCloseResult{Close: &HyperliquidClose{AlreadyFlat: true}}, "", nil
	}
	if res := runHedgeOrder(sc, hedgeAction{Kind: hedgeActionCloseFull}, snap, nil, nil); !res.OK || !res.AlreadyFlat {
		t.Errorf("already-flat: result = %+v", res)
	}

	runHyperliquidHedgeCloseFn = func(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
		return &HyperliquidCloseResult{Close: &HyperliquidClose{}}, "", nil
	}
	if res := runHedgeOrder(sc, hedgeAction{Kind: hedgeActionCloseFull}, snap, nil, nil); res.OK || res.ErrMsg == "" {
		t.Errorf("no-fill close: result = %+v, want failure", res)
	}
}

func TestPaperHedgeOrderResult(t *testing.T) {
	snap := hedgeSnapshot{HedgeQty: 0.05, HedgeSide: "short"}

	res := paperHedgeOrderResult(hedgeAction{Kind: hedgeActionOpen, Qty: 0.05}, hedgeSnapshot{}, 90000)
	if !res.OK || res.FillPx != 90000 || res.FillQty != 0.05 || res.FillOID != 0 {
		t.Errorf("open: %+v", res)
	}
	res = paperHedgeOrderResult(hedgeAction{Kind: hedgeActionCloseFull}, snap, 91000)
	if !res.OK || res.FillQty != 0.05 {
		t.Errorf("closeFull: %+v", res)
	}
	res = paperHedgeOrderResult(hedgeAction{Kind: hedgeActionReduce, Qty: 0.5}, snap, 91000)
	if !res.OK || res.FillQty != 0.05 {
		t.Errorf("reduce must clamp to held: %+v", res)
	}
	if res := paperHedgeOrderResult(hedgeAction{Kind: hedgeActionNone}, snap, 91000); res.OK {
		t.Errorf("none action: %+v", res)
	}
	if res := paperHedgeOrderResult(hedgeAction{Kind: hedgeActionOpen, Qty: 0.05}, hedgeSnapshot{}, 0); res.OK {
		t.Errorf("zero mark must fail: %+v", res)
	}
}

func TestApplyHedgeFill_OpenCreatesLeg(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC", Leverage: 2})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC") // hedge not open yet
	snap := captureHedgeSnapshot(s, sc)

	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "sell"}
	res := hedgeOrderResult{OK: true, FillPx: 90050, FillQty: 0.05, FillFee: 2.25, FillOID: 4242}
	cashBefore := s.Cash
	applyHedgeFill(s, sc, action, snap, res, false, nil)

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge leg not created")
	}
	if pos.Quantity != 0.05 || pos.InitialQuantity != 0.05 || pos.AvgCost != 90050 {
		t.Errorf("position = %+v", pos)
	}
	if pos.Side != "short" || pos.Multiplier != 1 || pos.Leverage != 2 {
		t.Errorf("side/multiplier/leverage = %s/%g/%g", pos.Side, pos.Multiplier, pos.Leverage)
	}
	if pos.HedgeFor != "ETH" || pos.HedgePrimaryQtyBasis != 1.5 {
		t.Errorf("hedge stamps = %q basis %g, want ETH/1.5", pos.HedgeFor, pos.HedgePrimaryQtyBasis)
	}
	if pos.OwnerStrategyID != "hl-eth" {
		t.Errorf("owner = %q", pos.OwnerStrategyID)
	}
	if len(s.TradeHistory) != 1 {
		t.Fatalf("want 1 trade, got %d", len(s.TradeHistory))
	}
	tr := s.TradeHistory[0]
	if tr.TradeType != hedgeTradeType || tr.Side != "sell" || tr.IsClose {
		t.Errorf("trade = %+v", tr)
	}
	if !strings.HasPrefix(tr.Details, "hedge(ETH) open") {
		t.Errorf("details = %q", tr.Details)
	}
	if tr.ExchangeOrderID != "4242" || tr.ExchangeFee != 2.25 || tr.FeeSource != FeeSourceUserFills {
		t.Errorf("exchange fields = oid %q fee %g src %q", tr.ExchangeOrderID, tr.ExchangeFee, tr.FeeSource)
	}
	if s.Cash != cashBefore-2.25 {
		t.Errorf("cash = %g, want fee-only debit %g", s.Cash, cashBefore-2.25)
	}
}

func TestApplyHedgeFill_OpenPartialFillAdvancesBasisProportionally(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	snap := captureHedgeSnapshot(s, sc)

	// Requested 0.05 BTC (full primary), only half filled → basis covers half
	// the primary; next cycle's decision diffs the remainder.
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "sell"}
	res := hedgeOrderResult{OK: true, FillPx: 90000, FillQty: 0.025, FillFee: 1, FillOID: 1}
	applyHedgeFill(s, sc, action, snap, res, false, nil)
	pos := s.Positions["BTC"]
	if pos == nil || pos.Quantity != 0.025 {
		t.Fatalf("position = %+v", pos)
	}
	if want := 0.75; pos.HedgePrimaryQtyBasis != want {
		t.Errorf("basis = %g, want %g (1.5 × 0.025/0.05)", pos.HedgePrimaryQtyBasis, want)
	}
}

func TestApplyHedgeFill_AddBlendsAndAdvancesBasis(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	// Primary grew 1.5 → 2.0 (scale-in); hedge still sized for 1.5.
	s.Positions["ETH"].Quantity = 2.0
	snap := captureHedgeSnapshot(s, sc)

	action := hedgeAction{Kind: hedgeActionAdd, Qty: 0.016, Side: "sell"}
	res := hedgeOrderResult{OK: true, FillPx: 95000, FillQty: 0.016, FillFee: 0.5, FillOID: 9}
	applyHedgeFill(s, sc, action, snap, res, false, nil)

	pos := s.Positions["BTC"]
	if pos.Quantity != 0.066 {
		t.Errorf("qty = %g, want 0.066", pos.Quantity)
	}
	wantAvg := (0.05*90000 + 0.016*95000) / 0.066
	if math.Abs(pos.AvgCost-wantAvg) > 1e-9 {
		t.Errorf("avg cost = %g, want %g", pos.AvgCost, wantAvg)
	}
	if math.Abs(pos.InitialQuantity-0.066) > 1e-9 {
		t.Errorf("initial qty = %g, want 0.066 (grows with the add)", pos.InitialQuantity)
	}
	// Full fill → basis advances the whole delta: 1.5 + (2.0−1.5)×1 = 2.0.
	if pos.HedgePrimaryQtyBasis != 2.0 {
		t.Errorf("basis = %g, want 2.0", pos.HedgePrimaryQtyBasis)
	}
	tr := s.TradeHistory[len(s.TradeHistory)-1]
	if tr.TradeType != hedgeTradeType || !strings.HasPrefix(tr.Details, "hedge(ETH) add") {
		t.Errorf("trade = %+v", tr)
	}
}

func TestApplyHedgeFill_AddPartialFillAdvancesBasisByFillFraction(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	s.Positions["ETH"].Quantity = 2.0
	snap := captureHedgeSnapshot(s, sc)

	// Requested add 0.016, half filled → basis += 0.5 × 0.5 = 1.75.
	action := hedgeAction{Kind: hedgeActionAdd, Qty: 0.016, Side: "sell"}
	res := hedgeOrderResult{OK: true, FillPx: 95000, FillQty: 0.008, FillFee: 0.25, FillOID: 9}
	applyHedgeFill(s, sc, action, snap, res, false, nil)
	if want := 1.75; s.Positions["BTC"].HedgePrimaryQtyBasis != want {
		t.Errorf("basis = %g, want %g", s.Positions["BTC"].HedgePrimaryQtyBasis, want)
	}
}

func TestApplyHedgeFill_ReduceBooksPartialAndScalesBasis(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	s.Positions["ETH"].Quantity = 0.75 // primary halved
	snap := captureHedgeSnapshot(s, sc)

	action := hedgeAction{Kind: hedgeActionReduce, Qty: 0.025}
	res := hedgeOrderResult{OK: true, FillPx: 92000, FillQty: 0.025, FillFee: 1.15, FillOID: 55}
	cashBefore := s.Cash
	applyHedgeFill(s, sc, action, snap, res, false, nil)

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge leg should survive a partial reduce")
	}
	if pos.Quantity != 0.025 {
		t.Errorf("qty = %g, want 0.025", pos.Quantity)
	}
	if want := 0.75; pos.HedgePrimaryQtyBasis != want {
		t.Errorf("basis = %g, want %g (1.5 × (1 − 0.025/0.05))", pos.HedgePrimaryQtyBasis, want)
	}
	tr := s.TradeHistory[len(s.TradeHistory)-1]
	if tr.TradeType != hedgeTradeType || !tr.IsClose || !strings.HasPrefix(tr.Details, "hedge(ETH) reduce") {
		t.Errorf("trade = %+v", tr)
	}
	// Short 0.025 @ 90000 → 92000: gross −50, fee 1.15 → −51.15 into DailyPnL
	// (cash), never the loss streak.
	if math.Abs(s.RiskState.DailyPnL-(s.Cash-cashBefore)) > 1e-9 {
		t.Errorf("DailyPnL = %g, want booked %g", s.RiskState.DailyPnL, s.Cash-cashBefore)
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("hedge reduce loss must not feed the streak, got %d", s.RiskState.ConsecutiveLosses)
	}
}

func TestApplyHedgeFill_CloseFullDeletesLeg(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	snap := captureHedgeSnapshot(s, sc)

	action := hedgeAction{Kind: hedgeActionCloseFull}
	res := hedgeOrderResult{OK: true, FillPx: 92000, FillQty: 0.05, FillFee: 2.3, FillOID: 56}
	applyHedgeFill(s, sc, action, snap, res, false, nil)

	if _, ok := s.Positions["BTC"]; ok {
		t.Error("hedge leg should be deleted after closeFull")
	}
	tr := s.TradeHistory[len(s.TradeHistory)-1]
	if tr.TradeType != hedgeTradeType || !tr.IsClose || !strings.HasPrefix(tr.Details, "hedge(ETH) close") {
		t.Errorf("trade = %+v", tr)
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("hedge close loss must not feed the streak, got %d", s.RiskState.ConsecutiveLosses)
	}
}

func TestApplyHedgeFill_PaperBooksAtMarkWithModeledFee(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	snap := captureHedgeSnapshot(s, sc)

	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "sell"}
	res := paperHedgeOrderResult(action, snap, 90000)
	cashBefore := s.Cash
	applyHedgeFill(s, sc, action, snap, res, true, nil)

	pos := s.Positions["BTC"]
	if pos == nil || pos.AvgCost != 90000 {
		t.Fatalf("paper position = %+v", pos)
	}
	tr := s.TradeHistory[0]
	wantFee := CalculatePlatformSpotFee("hyperliquid", 0.05*90000)
	if tr.FeeSource != FeeSourceModeled || tr.ExchangeFee != wantFee || tr.ExchangeOrderID != "" {
		t.Errorf("paper trade fee = %g src %q oid %q, want %g modeled", tr.ExchangeFee, tr.FeeSource, tr.ExchangeOrderID, wantFee)
	}
	if s.Cash != cashBefore-wantFee {
		t.Errorf("cash = %g, want %g", s.Cash, cashBefore-wantFee)
	}
}

func TestApplyHedgeFill_RecheckMismatchStillBooks(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	snap := captureHedgeSnapshot(s, sc)

	// State moves after the snapshot (an earlier same-cycle booking shrank
	// the primary). The confirmed fill must still be booked, with a WARN.
	s.Positions["ETH"].Quantity = 0.5
	var buf bytes.Buffer
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "sell"}
	res := hedgeOrderResult{OK: true, FillPx: 90000, FillQty: 0.05, FillFee: 1, FillOID: 7}
	applyHedgeFill(s, sc, action, snap, res, false, hedgeTestLogger(&buf))

	if s.Positions["BTC"] == nil || s.Positions["BTC"].Quantity != 0.05 {
		t.Fatal("confirmed fill must be booked even on snapshot mismatch")
	}
	if !strings.Contains(buf.String(), "moved between snapshot") {
		t.Errorf("want mismatch WARN, got: %s", buf.String())
	}
}

func TestApplyHedgeFill_MatchingSnapshotDoesNotWarn(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")
	snap := captureHedgeSnapshot(s, sc)
	var buf bytes.Buffer
	applyHedgeFill(s, sc, hedgeAction{Kind: hedgeActionOpen, Qty: 0.05, Side: "sell"}, snap,
		hedgeOrderResult{OK: true, FillPx: 90000, FillQty: 0.05, FillFee: 1, FillOID: 7}, false, hedgeTestLogger(&buf))
	if strings.Contains(buf.String(), "moved between snapshot") {
		t.Errorf("matching snapshot must not warn, got: %s", buf.String())
	}
}

func TestApplyHedgeFill_AlreadyFlatCloseLeavesLegUntouched(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	snap := captureHedgeSnapshot(s, sc)
	tradesBefore := len(s.TradeHistory)

	applyHedgeFill(s, sc, hedgeAction{Kind: hedgeActionCloseFull}, snap,
		hedgeOrderResult{OK: true, AlreadyFlat: true}, false, nil)

	pos := s.Positions["BTC"]
	if pos == nil || pos.Quantity != 0.05 {
		t.Error("already-flat close must leave the virtual leg for reconcile")
	}
	if len(s.TradeHistory) != tradesBefore {
		t.Error("already-flat close must not book a trade")
	}
}

func TestApplyHedgeFill_AdoptsForeignLegWithWarn(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	s.Positions["BTC"].HedgeFor = "" // lost stamp / legacy row
	s.Positions["BTC"].HedgePrimaryQtyBasis = 0
	snap := captureHedgeSnapshot(s, sc)

	var buf bytes.Buffer
	action := hedgeAction{Kind: hedgeActionAdd, Qty: 0.01, Side: "sell"}
	res := hedgeOrderResult{OK: true, FillPx: 90000, FillQty: 0.01, FillFee: 0.5, FillOID: 3}
	applyHedgeFill(s, sc, action, snap, res, false, hedgeTestLogger(&buf))

	pos := s.Positions["BTC"]
	if pos.HedgeFor != "ETH" {
		t.Errorf("foreign leg should be adopted (HedgeFor stamped), got %q", pos.HedgeFor)
	}
	if math.Abs(pos.Quantity-0.06) > 1e-9 {
		t.Errorf("qty = %g, want 0.06 blended", pos.Quantity)
	}
	if !strings.Contains(buf.String(), "WITHOUT hedge metadata") {
		t.Errorf("want adoption WARN, got: %s", buf.String())
	}
}

// TestBookPerpsCloseTradeType_HedgeStamped pins the choke-point invariant:
// ANY hedge close leg booked through the standard helpers carries
// trade_type "hedge" (the phase-B stats exclusion key) regardless of caller.
func TestBookPerpsCloseTradeType_HedgeStamped(t *testing.T) {
	s := hedgeTestStrategyState()
	bookPerpsCloseWithFillFee(s, "BTC", 91000, 0, false, "", "hedge_close", "hedge(ETH) close", "Hedge close", nil)
	if tr := s.TradeHistory[len(s.TradeHistory)-1]; tr.TradeType != hedgeTradeType {
		t.Errorf("full close trade_type = %q, want hedge", tr.TradeType)
	}

	s2 := hedgeTestStrategyState()
	bookPerpsPartialCloseWithFillFee(s2, "BTC", 0.01, 91000, 0, false, "", "hedge_reduce", "hedge(ETH) reduce", "Hedge reduce", nil)
	if tr := s2.TradeHistory[len(s2.TradeHistory)-1]; tr.TradeType != hedgeTradeType {
		t.Errorf("partial close trade_type = %q, want hedge", tr.TradeType)
	}

	// Corrupt hedge position → zero-PnL leg, still hedge-typed.
	s3 := hedgeTestStrategyState()
	s3.Positions["BTC"].AvgCost = 0
	bookPerpsCloseWithFillFee(s3, "BTC", 91000, 0, false, "", "hedge_close", "hedge(ETH) close", "Hedge close", nil)
	if tr := s3.TradeHistory[len(s3.TradeHistory)-1]; tr.TradeType != hedgeTradeType {
		t.Errorf("corrupt close trade_type = %q, want hedge", tr.TradeType)
	}

	// Control: the primary leg stays "perps".
	s4 := hedgeTestStrategyState()
	bookPerpsCloseWithFillFee(s4, "ETH", 3100, 0, false, "", "close", "Close", "Close", nil)
	if tr := s4.TradeHistory[len(s4.TradeHistory)-1]; tr.TradeType != "perps" {
		t.Errorf("primary close trade_type = %q, want perps", tr.TradeType)
	}
}

// ---------------------------------------------------------------------------
// Fail-closed primary unwind
// ---------------------------------------------------------------------------

func TestRunPrimaryUnwindOrder(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	var gotSz *float64
	var gotOIDs []int64
	prev := runHyperliquidUnwindCloseFn
	defer func() { runHyperliquidUnwindCloseFn = prev }()
	runHyperliquidUnwindCloseFn = func(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
		gotSz, gotOIDs = partialSz, cancelStopLossOIDs
		return &HyperliquidCloseResult{Close: &HyperliquidClose{
			Fill: &HyperliquidCloseFill{AvgPx: 2990, TotalSz: *partialSz, OID: 88, Fee: 1.5},
		}}, "", nil
	}

	res := runPrimaryUnwindOrder(sc, "ETH", 1.5, []int64{111, 222})
	if !res.OK || res.FillPx != 2990 || res.FillQty != 1.5 || res.FillOID != 88 {
		t.Errorf("result = %+v", res)
	}
	if gotSz == nil || *gotSz != 1.5 {
		t.Errorf("unwind must be a SIZED close (shared-coin peers), got %v", gotSz)
	}
	if len(gotOIDs) != 2 || gotOIDs[0] != 111 || gotOIDs[1] != 222 {
		t.Errorf("cancel OIDs = %v, want [111 222]", gotOIDs)
	}

	// Zero qty refuses without spawning.
	if res := runPrimaryUnwindOrder(sc, "ETH", 0, nil); res.OK || res.ErrMsg == "" {
		t.Errorf("zero unwind: %+v", res)
	}
}

func TestApplyPrimaryUnwindFill(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})

	t.Run("full unwind books hedge_open_failed_unwind", func(t *testing.T) {
		s := hedgeTestStrategyState()
		delete(s.Positions, "BTC")
		res := hedgeOrderResult{OK: true, FillPx: 2990, FillQty: 1.5, FillFee: 2, FillOID: 88}
		if !applyPrimaryUnwindFill(s, "ETH", res, nil) {
			t.Fatal("unwind apply failed")
		}
		if _, ok := s.Positions["ETH"]; ok {
			t.Error("primary should be flat after full unwind")
		}
		tr := s.TradeHistory[len(s.TradeHistory)-1]
		if tr.TradeType != "perps" || !tr.IsClose || !strings.Contains(tr.Details, "primary unwind") {
			t.Errorf("unwind trade = %+v (a PRIMARY leg: perps-typed, streak-fed)", tr)
		}
		// Primary unwind is a real primary loss → feeds the streak (unlike
		// hedge legs).
		if s.RiskState.ConsecutiveLosses != 1 {
			t.Errorf("streak = %d, want 1", s.RiskState.ConsecutiveLosses)
		}
	})

	t.Run("partial unwind reduces the primary", func(t *testing.T) {
		s := hedgeTestStrategyState()
		delete(s.Positions, "BTC")
		res := hedgeOrderResult{OK: true, FillPx: 2990, FillQty: 0.5, FillFee: 1, FillOID: 89}
		if !applyPrimaryUnwindFill(s, "ETH", res, nil) {
			t.Fatal("partial unwind apply failed")
		}
		if s.Positions["ETH"].Quantity != 1.0 {
			t.Errorf("qty = %g, want 1.0", s.Positions["ETH"].Quantity)
		}
	})

	t.Run("already-flat clears the virtual leg at zero PnL", func(t *testing.T) {
		s := hedgeTestStrategyState()
		delete(s.Positions, "BTC")
		res := hedgeOrderResult{OK: true, AlreadyFlat: true}
		if !applyPrimaryUnwindFill(s, "ETH", res, nil) {
			t.Fatal("already-flat unwind apply failed")
		}
		if _, ok := s.Positions["ETH"]; ok {
			t.Error("primary should be cleared")
		}
		tr := s.TradeHistory[len(s.TradeHistory)-1]
		if tr.RealizedPnL != 0 {
			t.Errorf("already-flat unwind must book zero PnL, got %g", tr.RealizedPnL)
		}
	})

	t.Run("no position → false, no panic", func(t *testing.T) {
		s := hedgeTestStrategyState()
		delete(s.Positions, "ETH")
		if applyPrimaryUnwindFill(s, "ETH", hedgeOrderResult{OK: true, FillPx: 1, FillQty: 1}, nil) {
			t.Error("want false with no primary position")
		}
	})

	_ = sc
}

func TestHedgeUnwindAlertFormats(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	if msg := formatHedgeOpenFailureCritical(sc, "insufficient margin"); !strings.Contains(msg, "CRITICAL") || !strings.Contains(msg, "ETH") || !strings.Contains(msg, "BTC") || !strings.Contains(msg, "insufficient margin") {
		t.Errorf("open-failure alert = %q", msg)
	}
	if msg := formatHedgeUnwindSuccessCritical(sc, 1.5); !strings.Contains(msg, "CRITICAL") || !strings.Contains(msg, "hedge_open_failed_unwind") {
		t.Errorf("unwind-success alert = %q", msg)
	}
	if msg := formatHedgeUnwindFailureCritical(sc, 1.5, "venue down"); !strings.Contains(msg, "CRITICAL") || !strings.Contains(msg, "OPEN AND UNHEDGED") || !strings.Contains(msg, "venue down") {
		t.Errorf("unwind-failure alert = %q", msg)
	}
}

func TestHedgeStateMatchesSnapshot(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	snap := captureHedgeSnapshot(s, sc)
	if !hedgeStateMatchesSnapshot(s, sc, snap) {
		t.Error("fresh snapshot should match")
	}
	snap.PrimaryQty += 0.5
	if hedgeStateMatchesSnapshot(s, sc, snap) {
		t.Error("primary drift should mismatch")
	}
	snap = captureHedgeSnapshot(s, sc)
	snap.HedgeBasis += 0.1
	if hedgeStateMatchesSnapshot(s, sc, snap) {
		t.Error("basis drift should mismatch")
	}
	if hedgeStateMatchesSnapshot(nil, sc, snap) {
		t.Error("nil state should mismatch")
	}
}

// TestHedgeOpenRoundTrip pins the full pipeline Phase E will drive:
// decision → skip-reason → stubbed live order → apply under lock.
func TestHedgeOpenRoundTrip(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	delete(s.Positions, "BTC")

	snap := captureHedgeSnapshot(s, sc)
	action := hedgeTargetDecision(sc, snap, 3000, 90000)
	if action.Kind != hedgeActionOpen {
		t.Fatalf("decision = %s, want open", action.Kind)
	}
	if reason := hedgeOrderSkipReason(sc, action, snap, 90000); reason != "" {
		t.Fatalf("skip reason = %q, want proceed", reason)
	}

	prev := runHyperliquidHedgeExecuteFn
	defer func() { runHyperliquidHedgeExecuteFn = prev }()
	runHyperliquidHedgeExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelStopLossOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
			Fill: &HyperliquidFill{AvgPx: 90010, TotalSz: size, OID: 1, Fee: 1},
		}}, "", nil
	}
	res := runHedgeOrder(sc, action, snap, nil, nil)
	if !res.OK {
		t.Fatalf("order failed: %+v", res)
	}
	applyHedgeFill(s, sc, action, snap, res, false, nil)

	// Converged: next decision against the new state is none.
	snap2 := captureHedgeSnapshot(s, sc)
	if a := hedgeTargetDecision(sc, snap2, 3000, 90000); a.Kind != hedgeActionNone {
		t.Errorf("post-open decision = %s (%s), want none — hedge not converged", a.Kind, a.Reason)
	}
}

// TestHedgeReduceRoundTrip pins the shrink path: primary halves, hedge
// reduces proportionally, and the next decision is none.
func TestHedgeReduceRoundTrip(t *testing.T) {
	sc := hlHedgeTestStrategy("hl-eth", &HedgeConfig{Enabled: true, Symbol: "BTC"})
	s := hedgeTestStrategyState()
	s.Positions["ETH"].Quantity = 0.75

	snap := captureHedgeSnapshot(s, sc)
	action := hedgeTargetDecision(sc, snap, 3000, 90000)
	if action.Kind != hedgeActionReduce {
		t.Fatalf("decision = %s, want reduce", action.Kind)
	}
	if reason := hedgeOrderSkipReason(sc, action, snap, 90000); reason != "" {
		t.Fatalf("skip reason = %q", reason)
	}

	prev := runHyperliquidHedgeCloseFn
	defer func() { runHyperliquidHedgeCloseFn = prev }()
	runHyperliquidHedgeCloseFn = func(script, symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, string, error) {
		return &HyperliquidCloseResult{Close: &HyperliquidClose{
			Fill: &HyperliquidCloseFill{AvgPx: 90000, TotalSz: *partialSz, OID: 2, Fee: 0.5},
		}}, "", nil
	}
	res := runHedgeOrder(sc, action, snap, nil, nil)
	if !res.OK {
		t.Fatalf("order failed: %+v", res)
	}
	applyHedgeFill(s, sc, action, snap, res, false, nil)

	snap2 := captureHedgeSnapshot(s, sc)
	if a := hedgeTargetDecision(sc, snap2, 3000, 90000); a.Kind != hedgeActionNone {
		t.Errorf("post-reduce decision = %s (%s), want none — hedge not converged", a.Kind, a.Reason)
	}
}
