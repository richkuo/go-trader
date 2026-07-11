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
