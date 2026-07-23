package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
)

// fakeHedgeDeps records every order the engine places and returns scripted
// results, so the full lifecycle is testable without subprocesses.
type fakeHedgeDeps struct {
	execCalls  []fakeHedgeExec
	closeCalls []fakeHedgeClose
	execErr    error
	execFill   *HyperliquidFill
	closeErr   map[string]error                 // per-coin close error
	closeFill  map[string]*HyperliquidCloseFill // per-coin close fill
	alerts     []string
}

type fakeHedgeExec struct {
	Coin string
	Side string
	Size float64
}

type fakeHedgeClose struct {
	Coin    string
	Partial *float64
	Cancel  []int64
}

func (f *fakeHedgeDeps) deps() hedgeSyncDeps {
	return hedgeSyncDeps{
		execute: func(sc StrategyConfig, coin, side string, size float64, snapshot hlExecuteSnapshot) (*HyperliquidExecuteResult, error) {
			f.execCalls = append(f.execCalls, fakeHedgeExec{coin, side, size})
			if f.execErr != nil {
				return nil, f.execErr
			}
			fill := f.execFill
			if fill == nil {
				fill = &HyperliquidFill{AvgPx: 50000, TotalSz: size, OID: 1001, Fee: 0.5}
			}
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Action: side, Symbol: coin, Size: size, Fill: fill}}, nil
		},
		closer: func(coin string, partial *float64, cancel []int64) (*HyperliquidCloseResult, error) {
			var pcopy *float64
			if partial != nil {
				v := *partial
				pcopy = &v
			}
			f.closeCalls = append(f.closeCalls, fakeHedgeClose{coin, pcopy, append([]int64(nil), cancel...)})
			if err := f.closeErr[coin]; err != nil {
				return nil, err
			}
			fill := f.closeFill[coin]
			if fill == nil {
				sz := 0.0
				if partial != nil {
					sz = *partial
				}
				fill = &HyperliquidCloseFill{AvgPx: 50000, TotalSz: sz, OID: 2002, Fee: 0.4}
			}
			return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: coin, Fill: fill}, CancelStopLossSucceeded: len(cancel) > 0}, nil
		},
		ownerDM: func(msg string) { f.alerts = append(f.alerts, msg) },
	}
}

func hedgeSyncFixture(primaryQty float64, withHedge bool) (StrategyConfig, *AppState, map[string]float64) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	ss := hedgeTestState("a")
	if primaryQty > 0 {
		ss.Positions["ETH"] = &Position{
			Symbol: "ETH", Quantity: primaryQty, InitialQuantity: primaryQty, AvgCost: 2500,
			Side: "long", Multiplier: 1, Leverage: 3, OwnerStrategyID: "a",
			StopLossOID: 555, TPOIDs: []int64{666},
		}
	}
	if withHedge {
		ss.Positions["BTC"] = &Position{
			Symbol: "BTC", Quantity: 0.1, InitialQuantity: 0.1, AvgCost: 50000,
			Side: "short", Multiplier: 1, Leverage: 2, OwnerStrategyID: "a",
			IsHedge: true, HedgePrimarySymbol: "ETH", HedgeCoveredPrimaryQty: 4,
		}
	}
	state := &AppState{Strategies: map[string]*StrategyState{"a": ss}}
	prices := map[string]float64{"ETH": 2500, "BTC": 50000}
	return sc, state, prices
}

func runHedgeSyncTest(t *testing.T, sc StrategyConfig, state *AppState, prices map[string]float64, hlPositions []HLPosition, hlStateFetched bool, f *fakeHedgeDeps) int {
	t.Helper()
	var mu sync.RWMutex
	return runHedgeLegSync(context.Background(), []StrategyConfig{sc}, state, &mu, prices, hlPositions, hlStateFetched, f.deps(), nil)
}

func TestHedgeSyncOpensHedgeWithPrimary(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, false)
	f := &fakeHedgeDeps{}
	trades := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}}, true, f)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if len(f.execCalls) != 1 || f.execCalls[0].Coin != "BTC" || f.execCalls[0].Side != "sell" || math.Abs(f.execCalls[0].Size-0.1) > 1e-9 {
		t.Fatalf("exec calls = %+v", f.execCalls)
	}
	ss := state.Strategies["a"]
	pos := ss.Positions["BTC"]
	if pos == nil || !pos.IsHedge || pos.Side != "short" || math.Abs(pos.Quantity-0.1) > 1e-9 {
		t.Fatalf("hedge position = %+v", pos)
	}
	if pos.HedgePrimarySymbol != "ETH" || math.Abs(pos.HedgeCoveredPrimaryQty-4) > 1e-9 {
		t.Fatalf("hedge metadata = %+v", pos)
	}
	if pos.Leverage != 2 {
		t.Fatalf("hedge leverage = %g, want the hedge block's 2", pos.Leverage)
	}
	last := ss.TradeHistory[len(ss.TradeHistory)-1]
	if last.IsClose || last.Symbol != "BTC" || last.Side != "sell" || last.ExchangeFee != 0.5 {
		t.Fatalf("open trade = %+v", last)
	}
	// Converged: a second pass must be a no-op (no churn).
	f2 := &fakeHedgeDeps{}
	if n := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: -0.1}}, true, f2); n != 0 {
		t.Fatalf("second pass placed orders: exec=%v close=%v", f2.execCalls, f2.closeCalls)
	}
}

func TestHedgeSyncNoPrimaryNoOrders(t *testing.T) {
	// Failed/absent primary open → no hedge order is ever placed.
	sc, state, prices := hedgeSyncFixture(0, false)
	f := &fakeHedgeDeps{}
	if n := runHedgeSyncTest(t, sc, state, prices, nil, true, f); n != 0 {
		t.Fatalf("trades = %d, want 0", n)
	}
	if len(f.execCalls) != 0 || len(f.closeCalls) != 0 {
		t.Fatalf("orders placed with no primary: exec=%v close=%v", f.execCalls, f.closeCalls)
	}
}

func TestHedgeSyncOpenFailureClosesPrimaryFailClosed(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, false)
	f := &fakeHedgeDeps{execErr: fmt.Errorf("boom")}
	trades := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}}, true, f)
	if trades != 1 {
		t.Fatalf("trades = %d, want the fail-closed primary close", trades)
	}
	// Reduce-only SIZED close of the primary (never partialSz=nil — a shared
	// primary coin's peer exposure must survive), with protection cancelled.
	if len(f.closeCalls) != 1 || f.closeCalls[0].Coin != "ETH" || f.closeCalls[0].Partial == nil || math.Abs(*f.closeCalls[0].Partial-4) > 1e-9 {
		t.Fatalf("close calls = %+v", f.closeCalls)
	}
	if len(f.closeCalls[0].Cancel) != 2 {
		t.Fatalf("expected SL+TP OIDs cancelled, got %v", f.closeCalls[0].Cancel)
	}
	ss := state.Strategies["a"]
	if _, open := ss.Positions["ETH"]; open {
		t.Fatal("primary position not cleared after fail-closed close")
	}
	if len(f.alerts) == 0 || !strings.Contains(f.alerts[len(f.alerts)-1], "fail-closed") {
		t.Fatalf("operator alert missing: %v", f.alerts)
	}
	last := ss.TradeHistory[len(ss.TradeHistory)-1]
	if !last.IsClose || last.Symbol != "ETH" {
		t.Fatalf("fail-closed close trade = %+v", last)
	}
}

func TestHedgeSyncMissingMarkClosesPrimary(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, false)
	delete(prices, "BTC") // hedge mark unavailable → cannot size the open
	f := &fakeHedgeDeps{}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}}, true, f)
	if len(f.execCalls) != 0 {
		t.Fatalf("open placed without a mark: %v", f.execCalls)
	}
	if _, open := state.Strategies["a"].Positions["ETH"]; open {
		t.Fatal("primary not fail-closed when hedge open could not be sized")
	}
}

func TestHedgeSyncForeignOnChainPositionRefusesOpen(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, false)
	f := &fakeHedgeDeps{}
	// Foreign BTC position on-chain, no state hedge → refuse + fail-close.
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: 0.5}}, true, f)
	if len(f.execCalls) != 0 {
		t.Fatalf("opened into a foreign position: %v", f.execCalls)
	}
	if _, open := state.Strategies["a"].Positions["ETH"]; open {
		t.Fatal("primary not fail-closed on foreign hedge-coin position")
	}
	if len(f.alerts) == 0 || !strings.Contains(strings.Join(f.alerts, "\n"), "foreign on-chain position") {
		t.Fatalf("foreign-position alert missing: %v", f.alerts)
	}
}

func TestHedgeSyncUnfetchedChainStateRefusesOpen(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, false)
	f := &fakeHedgeDeps{}
	runHedgeSyncTest(t, sc, state, prices, nil, false, f)
	if len(f.execCalls) != 0 {
		t.Fatalf("opened without verifiable chain state: %v", f.execCalls)
	}
	if _, open := state.Strategies["a"].Positions["ETH"]; open {
		t.Fatal("primary not fail-closed when chain state was unverifiable")
	}
}

func TestHedgeSyncPartialCloseReducesHedge(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	ss.Positions["ETH"].Quantity = 2 // primary partially closed this cycle
	f := &fakeHedgeDeps{}
	trades := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -0.1}}, true, f)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if len(f.closeCalls) != 1 || f.closeCalls[0].Coin != "BTC" || f.closeCalls[0].Partial == nil || math.Abs(*f.closeCalls[0].Partial-0.05) > 1e-9 {
		t.Fatalf("close calls = %+v", f.closeCalls)
	}
	pos := ss.Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.05) > 1e-9 || math.Abs(pos.HedgeCoveredPrimaryQty-2) > 1e-9 {
		t.Fatalf("hedge after reduce = %+v", pos)
	}
}

func TestHedgeSyncFullCloseFollowsPrimary(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(0, true) // primary gone, hedge open
	f := &fakeHedgeDeps{closeFill: map[string]*HyperliquidCloseFill{
		"BTC": {AvgPx: 49000, TotalSz: 0.1, OID: 3003, Fee: 0.4},
	}}
	trades := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "BTC", Size: -0.1}}, true, f)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if len(f.closeCalls) != 1 || f.closeCalls[0].Coin != "BTC" || f.closeCalls[0].Partial != nil {
		t.Fatalf("close calls = %+v (full close must pass nil partial)", f.closeCalls)
	}
	ss := state.Strategies["a"]
	if _, open := ss.Positions["BTC"]; open {
		t.Fatal("hedge not removed after full close")
	}
	last := ss.TradeHistory[len(ss.TradeHistory)-1]
	if !last.IsClose || math.Abs(last.RealizedPnL-(0.1*(50000-49000))) > 1e-6 {
		t.Fatalf("hedge close trade = %+v, want gross PnL 100", last)
	}
}

func TestHedgeSyncFlipClosesThenReopens(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	ss.Positions["ETH"].Side = "short" // primary flipped this cycle
	f := &fakeHedgeDeps{closeFill: map[string]*HyperliquidCloseFill{
		"BTC": {AvgPx: 50000, TotalSz: 0.1, OID: 4004, Fee: 0.4},
	}}
	trades := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: -4}, {Coin: "BTC", Size: -0.1}}, true, f)
	if trades != 2 {
		t.Fatalf("trades = %d, want close + reopen", trades)
	}
	if len(f.closeCalls) != 1 || len(f.execCalls) != 1 || f.execCalls[0].Side != "buy" {
		t.Fatalf("orders: close=%+v exec=%+v", f.closeCalls, f.execCalls)
	}
	pos := ss.Positions["BTC"]
	if pos == nil || pos.Side != "long" || !pos.IsHedge {
		t.Fatalf("flipped hedge = %+v", pos)
	}
}

func TestHedgeSyncAddFailureClosesUncoveredDelta(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	ss.Positions["ETH"].Quantity = 6 // scale-in grew the primary; covered=4
	f := &fakeHedgeDeps{execErr: fmt.Errorf("size rounded to zero")}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 6}, {Coin: "BTC", Size: -0.1}}, true, f)
	// Fail-closed: the UNCOVERED 2 ETH close, not the whole leg.
	if len(f.closeCalls) != 1 || f.closeCalls[0].Coin != "ETH" || f.closeCalls[0].Partial == nil || math.Abs(*f.closeCalls[0].Partial-2) > 1e-9 {
		t.Fatalf("close calls = %+v, want partial 2 ETH", f.closeCalls)
	}
	pos := ss.Positions["ETH"]
	if pos == nil || math.Abs(pos.Quantity-4) > 1e-9 {
		t.Fatalf("primary after delta close = %+v, want qty 4", pos)
	}
	hedge := ss.Positions["BTC"]
	if hedge == nil || math.Abs(hedge.Quantity-0.1) > 1e-9 {
		t.Fatalf("hedge disturbed by add failure: %+v", hedge)
	}
}

func TestHedgeSyncReduceFailureAlertsAndRetries(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	ss.Positions["ETH"].Quantity = 2
	f := &fakeHedgeDeps{closeErr: map[string]error{"BTC": fmt.Errorf("api down")}}
	trades := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -0.1}}, true, f)
	if trades != 0 {
		t.Fatalf("trades = %d, want 0 (close failed)", trades)
	}
	// Over-hedged is the safe direction: no destructive action, loud alert,
	// hedge state untouched so next cycle retries the same reduce.
	pos := ss.Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.1) > 1e-9 || math.Abs(pos.HedgeCoveredPrimaryQty-4) > 1e-9 {
		t.Fatalf("hedge state mutated on failed close: %+v", pos)
	}
	if len(f.alerts) != 1 || !strings.Contains(f.alerts[0], "over-hedged") {
		t.Fatalf("alerts = %v", f.alerts)
	}
}

func TestHedgeSyncScaleInAddsToHedge(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	ss.Positions["ETH"].Quantity = 6
	f := &fakeHedgeDeps{}
	trades := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 6}, {Coin: "BTC", Size: -0.1}}, true, f)
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}
	if len(f.execCalls) != 1 || f.execCalls[0].Side != "sell" || math.Abs(f.execCalls[0].Size-0.05) > 1e-9 {
		t.Fatalf("exec calls = %+v, want sell 0.05 BTC", f.execCalls)
	}
	pos := ss.Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.15) > 1e-9 || math.Abs(pos.HedgeCoveredPrimaryQty-6) > 1e-9 {
		t.Fatalf("hedge after add = %+v", pos)
	}
	// Add leg blends AvgCost and grows InitialQuantity (mirrors #873).
	if math.Abs(pos.InitialQuantity-0.15) > 1e-9 {
		t.Fatalf("InitialQuantity = %g, want 0.15", pos.InitialQuantity)
	}
}

func TestHedgeSyncSkipsNonHedgeAndPaperStrategies(t *testing.T) {
	sc := hedgeTestConfig("a", "ETH", "BTC")
	sc.Args[2] = "--mode=paper" // load-time validation rejects this; engine must also skip it
	_, state, prices := hedgeSyncFixture(4, false)
	f := &fakeHedgeDeps{}
	var mu sync.RWMutex
	n := runHedgeLegSync(context.Background(), []StrategyConfig{sc}, state, &mu, prices, nil, true, f.deps(), nil)
	if n != 0 || len(f.execCalls) != 0 || len(f.closeCalls) != 0 {
		t.Fatalf("paper strategy produced hedge orders: %v %v", f.execCalls, f.closeCalls)
	}
}

// Review finding 1 (#1333): downward on-chain hedge drift (partial ADL /
// liquidation / manual reduction while the primary is unchanged) must
// re-trigger an add that restores coverage — never be adopted as "fully
// covering" and leave the position silently under-hedged.
func TestHedgeSyncDownwardDriftReAddsShortfall(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	// Reconcile observes the hedge shrunk on-chain 0.10 → 0.08.
	resolve := func(coin string, oid int64, qty float64) (HLFillLookup, bool) { return HLFillLookup{}, false }
	if !reconcileHedgeLegForStrategy(sc, ss, []HLPosition{{Coin: "BTC", Size: -0.08, EntryPrice: 50000, Leverage: 2}}, resolve, hedgeSilentLogger("a")) {
		t.Fatal("expected drift resync")
	}
	// Covered must rescale proportionally (4 × 0.08/0.10 = 3.2), so the next
	// sync re-adds the uncovered 0.8 ETH worth: 0.8×2500×0.5÷50000 = 0.02 BTC.
	f := &fakeHedgeDeps{}
	trades := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: -0.08}}, true, f)
	if trades != 1 || len(f.execCalls) != 1 {
		t.Fatalf("trades=%d exec=%+v, want one re-add", trades, f.execCalls)
	}
	if f.execCalls[0].Side != "sell" || math.Abs(f.execCalls[0].Size-0.02) > 1e-9 {
		t.Fatalf("re-add order = %+v, want sell 0.02 BTC", f.execCalls[0])
	}
	pos := ss.Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.10) > 1e-9 || math.Abs(pos.HedgeCoveredPrimaryQty-4) > 1e-9 {
		t.Fatalf("hedge after re-add = %+v, want qty 0.10 covering 4", pos)
	}
}

// Review finding 1 must-survive (b): the upward lost-add drift (crash between
// an add fill and its booking) still must not double-place the add after the
// proportional rescale.
func TestHedgeSyncUpwardDriftNoDoubleAdd(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	// Primary scale-in booked (4 → 6) but the matching hedge add fill was
	// lost: covered is still 4, chain shows the add (0.10 → 0.15).
	ss.Positions["ETH"].Quantity = 6
	resolve := func(coin string, oid int64, qty float64) (HLFillLookup, bool) { return HLFillLookup{}, false }
	if !reconcileHedgeLegForStrategy(sc, ss, []HLPosition{{Coin: "BTC", Size: -0.15, EntryPrice: 50000, Leverage: 2}}, resolve, hedgeSilentLogger("a")) {
		t.Fatal("expected drift resync")
	}
	// covered rescales 4 × 0.15/0.10 = 6 = primary → converged, no orders.
	f := &fakeHedgeDeps{}
	if n := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 6}, {Coin: "BTC", Size: -0.15}}, true, f); n != 0 {
		t.Fatalf("double-placed after lost-add resync: exec=%+v close=%+v", f.execCalls, f.closeCalls)
	}
}

// Review finding 2 (#1333): a flip whose stale-hedge close FAILS leaves the
// hedge on the SAME side as the flipped primary (2× directional exposure, no
// stop) — the engine must de-risk the primary reduce-only, not passively
// retry, and must not mislabel the state as a benign over-hedge.
func TestHedgeSyncFlipCloseFailureDeRisksPrimary(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	ss.Positions["ETH"].Side = "short" // primary flipped; stale hedge is also short
	f := &fakeHedgeDeps{closeErr: map[string]error{"BTC": fmt.Errorf("api down")}}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: -4}, {Coin: "BTC", Size: -0.1}}, true, f)
	// The failed BTC close, then the fail-closed ETH primary close.
	if len(f.closeCalls) != 2 || f.closeCalls[0].Coin != "BTC" || f.closeCalls[1].Coin != "ETH" {
		t.Fatalf("close calls = %+v, want failed BTC close then ETH de-risk", f.closeCalls)
	}
	if f.closeCalls[1].Partial == nil || math.Abs(*f.closeCalls[1].Partial-4) > 1e-9 {
		t.Fatalf("primary de-risk close = %+v, want sized 4 ETH", f.closeCalls[1])
	}
	if _, open := ss.Positions["ETH"]; open {
		t.Fatal("primary not de-risked after flip-close failure")
	}
	joined := strings.Join(f.alerts, "\n")
	if strings.Contains(joined, "over-hedged") {
		t.Fatalf("same-side residual mislabeled as over-hedged: %v", f.alerts)
	}
	if !strings.Contains(joined, "SAME side") {
		t.Fatalf("alert must name the same-side 2x exposure: %v", f.alerts)
	}
	// Exposure must not compound on later cycles: primary is flat, so the
	// next pass just retries the stale-hedge close.
	f2 := &fakeHedgeDeps{closeErr: map[string]error{"BTC": fmt.Errorf("api down")}}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "BTC", Size: -0.1}}, true, f2)
	if len(f2.execCalls) != 0 || len(f2.closeCalls) != 1 || f2.closeCalls[0].Coin != "BTC" {
		t.Fatalf("follow-up cycle = exec %+v close %+v, want only the hedge-close retry", f2.execCalls, f2.closeCalls)
	}
}

// Review finding 2 must-survive (c): flip where the stale-hedge close
// succeeds but the reopen fails → the existing full-primary fail-close path.
func TestHedgeSyncFlipReopenFailureClosesPrimary(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	ss.Positions["ETH"].Side = "short"
	f := &fakeHedgeDeps{
		execErr:   fmt.Errorf("reopen rejected"),
		closeFill: map[string]*HyperliquidCloseFill{"BTC": {AvgPx: 50000, TotalSz: 0.1, OID: 5005, Fee: 0.4}},
	}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: -4}, {Coin: "BTC", Size: -0.1}}, true, f)
	if _, open := ss.Positions["BTC"]; open {
		t.Fatal("stale hedge not closed on flip")
	}
	if _, open := ss.Positions["ETH"]; open {
		t.Fatal("primary not fail-closed when the flip reopen failed")
	}
}

// Review on #1333 (round 2): a PARTIALLY-filled hedge open must scale the
// covered watermark by the actual fill ratio and alert — stamping the full
// primary quantity would record coverage the leg doesn't have, and the next
// cycle would see primary==covered and never re-add the shortfall (a silent,
// permanent under-hedge that reconcile cannot repair).
func TestHedgeSyncPartialOpenFillScalesCoveredAndReAdds(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, false)
	// Requested open is 0.1 BTC (4 ETH x $2500 x ratio 0.5 / $50000); the
	// exchange fills only half.
	f := &fakeHedgeDeps{execFill: &HyperliquidFill{AvgPx: 50000, TotalSz: 0.05, OID: 1001, Fee: 0.5}}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}}, true, f)
	ss := state.Strategies["a"]
	pos := ss.Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.05) > 1e-9 {
		t.Fatalf("hedge leg = %+v", pos)
	}
	if math.Abs(pos.HedgeCoveredPrimaryQty-2) > 1e-9 {
		t.Fatalf("covered = %g, want 2 (4 x 0.05/0.10 fill ratio)", pos.HedgeCoveredPrimaryQty)
	}
	underFillAlerted := false
	for _, a := range f.alerts {
		if strings.Contains(a, "under-filled") {
			underFillAlerted = true
		}
	}
	if !underFillAlerted {
		t.Fatalf("partial open fill must alert; got %v", f.alerts)
	}
	// Next cycle re-adds the uncovered 2 ETH → a 0.05 BTC top-up.
	f2 := &fakeHedgeDeps{}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: -0.05}}, true, f2)
	if len(f2.execCalls) != 1 || f2.execCalls[0].Coin != "BTC" || math.Abs(f2.execCalls[0].Size-0.05) > 1e-9 {
		t.Fatalf("top-up calls = %+v", f2.execCalls)
	}
	if math.Abs(pos.HedgeCoveredPrimaryQty-4) > 1e-9 || math.Abs(pos.Quantity-0.1) > 1e-9 {
		t.Fatalf("after top-up: covered=%g qty=%g, want 4 / 0.1", pos.HedgeCoveredPrimaryQty, pos.Quantity)
	}
	// Fully converged: a third pass is a no-op (no churn regression).
	f3 := &fakeHedgeDeps{}
	if n := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: -0.1}}, true, f3); n != 0 {
		t.Fatalf("third pass placed orders: exec=%v close=%v", f3.execCalls, f3.closeCalls)
	}
}

// Same invariant for a scale-in ADD: covered advances only by the filled
// fraction of the delta, so the next cycle tops up the remainder.
func TestHedgeSyncPartialAddFillScalesCovered(t *testing.T) {
	// Primary grew 4 → 6 ETH with covered=4: the add for the 2-ETH delta
	// requests 0.05 BTC; the exchange fills half of it.
	sc, state, prices := hedgeSyncFixture(6, true)
	f := &fakeHedgeDeps{execFill: &HyperliquidFill{AvgPx: 50000, TotalSz: 0.025, OID: 1002, Fee: 0.3}}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 6}, {Coin: "BTC", Size: -0.1}}, true, f)
	pos := state.Strategies["a"].Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-0.125) > 1e-9 {
		t.Fatalf("hedge leg = %+v", pos)
	}
	if math.Abs(pos.HedgeCoveredPrimaryQty-5) > 1e-9 {
		t.Fatalf("covered = %g, want 5 (4 + half of the 2-ETH delta)", pos.HedgeCoveredPrimaryQty)
	}
	underFillAlerted := false
	for _, a := range f.alerts {
		if strings.Contains(a, "under-filled") {
			underFillAlerted = true
		}
	}
	if !underFillAlerted {
		t.Fatalf("partial add fill must alert; got %v", f.alerts)
	}
}

// Review on #1333 (round 3): the HL adapter rounds order sizes to the asset's
// sz_decimals, so a FULLY-filled hedge order legitimately reports slightly
// less than the planner's unrounded request. That benign lot rounding must
// NOT be misread as a partial fill — else every open scales the watermark,
// fires a spurious under-hedge alert, and re-adds dust next cycle (which can
// round to zero and cascade into hedgeFailClosePrimary).
func TestHedgeSyncLotRoundedFullFillIsNotPartial(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, false)
	// Requested 0.1 BTC; the exchange reports the lot-rounded 0.09999.
	f := &fakeHedgeDeps{execFill: &HyperliquidFill{AvgPx: 50000, TotalSz: 0.09999, OID: 1003, Fee: 0.5}}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}}, true, f)
	pos := state.Strategies["a"].Positions["BTC"]
	if pos == nil || math.Abs(pos.HedgeCoveredPrimaryQty-4) > 1e-12 {
		t.Fatalf("lot-rounded full fill must stamp full coverage; covered = %+v", pos)
	}
	if len(f.alerts) != 0 {
		t.Fatalf("lot-rounded full fill must not alert: %v", f.alerts)
	}
	// Converged: the next cycle must not chase the sub-lot dust.
	f2 := &fakeHedgeDeps{}
	if n := runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: -0.09999}}, true, f2); n != 0 {
		t.Fatalf("dust re-add churn: exec=%v close=%v", f2.execCalls, f2.closeCalls)
	}
	// A fill that rounds UP is equally benign.
	sc2, state2, prices2 := hedgeSyncFixture(4, false)
	f3 := &fakeHedgeDeps{execFill: &HyperliquidFill{AvgPx: 50000, TotalSz: 0.1000001, OID: 1004, Fee: 0.5}}
	runHedgeSyncTest(t, sc2, state2, prices2, []HLPosition{{Coin: "ETH", Size: 4}}, true, f3)
	pos2 := state2.Strategies["a"].Positions["BTC"]
	if pos2 == nil || math.Abs(pos2.HedgeCoveredPrimaryQty-4) > 1e-12 || len(f3.alerts) != 0 {
		t.Fatalf("round-up full fill mishandled: pos=%+v alerts=%v", pos2, f3.alerts)
	}
}

// Review on #1333 (round 3, optional): a flip whose stale-hedge full close
// only PARTIALLY fills must not proceed to the re-open — the re-open would
// overwrite the same-coin residual leg in virtual state, silently discarding
// its quantity. The residual is the SAME side as the flipped primary, so the
// engine de-risks the primary (constraint 4) and leaves the residual for the
// next cycle's planner to close.
func TestHedgeSyncFlipPartialCloseDeRisksPrimaryNotOverwrite(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(4, true)
	ss := state.Strategies["a"]
	ss.Positions["ETH"].Side = "short" // primary flipped; stale short hedge is now same-side
	f := &fakeHedgeDeps{closeFill: map[string]*HyperliquidCloseFill{
		"BTC": {AvgPx: 50000, TotalSz: 0.06, OID: 6006, Fee: 0.3}, // partial: 0.06 of 0.1
		"ETH": {AvgPx: 2500, TotalSz: 4, OID: 6007, Fee: 0.8},
	}}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "ETH", Size: -4}, {Coin: "BTC", Size: -0.1}}, true, f)
	if len(f.execCalls) != 0 {
		t.Fatalf("re-open must not run after a partial flip close: %+v", f.execCalls)
	}
	hp := ss.Positions["BTC"]
	if hp == nil || !hp.IsHedge || hp.Side != "short" || math.Abs(hp.Quantity-0.04) > 1e-9 {
		t.Fatalf("residual hedge leg must survive (0.04 short), got %+v", hp)
	}
	if _, open := ss.Positions["ETH"]; open {
		t.Fatal("primary must be fail-closed when the flip close under-fills")
	}
	joined := strings.Join(f.alerts, "\n")
	if !strings.Contains(joined, "partially closed") {
		t.Fatalf("alert must name the partial flip close: %v", f.alerts)
	}
	// Next cycle: primary flat → the planner just closes the residual.
	f2 := &fakeHedgeDeps{closeFill: map[string]*HyperliquidCloseFill{
		"BTC": {AvgPx: 50000, TotalSz: 0.04, OID: 6008, Fee: 0.2},
	}}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "BTC", Size: -0.04}}, true, f2)
	if _, open := ss.Positions["BTC"]; open {
		t.Fatal("residual hedge not closed on the follow-up cycle")
	}
}

// A partial full-close WITHOUT a flip (primary already flat, or a deep
// reduction) leaves an INVERSE/naked-but-primary-flat residual — the safe
// direction — so it alerts and retries next cycle instead of de-risking.
func TestHedgeSyncPartialFullCloseWithoutFlipRetries(t *testing.T) {
	sc, state, prices := hedgeSyncFixture(0, true) // primary flat, hedge leg 0.1 short
	f := &fakeHedgeDeps{closeFill: map[string]*HyperliquidCloseFill{
		"BTC": {AvgPx: 50000, TotalSz: 0.06, OID: 7007, Fee: 0.3},
	}}
	runHedgeSyncTest(t, sc, state, prices, []HLPosition{{Coin: "BTC", Size: -0.1}}, true, f)
	hp := state.Strategies["a"].Positions["BTC"]
	if hp == nil || math.Abs(hp.Quantity-0.04) > 1e-9 {
		t.Fatalf("residual hedge leg = %+v, want 0.04 remaining", hp)
	}
	joined := strings.Join(f.alerts, "\n")
	if !strings.Contains(joined, "under-filled") {
		t.Fatalf("partial full close must alert: %v", f.alerts)
	}
}
