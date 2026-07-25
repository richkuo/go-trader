package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func hedgeTestConfig(mode string) StrategyConfig {
	return StrategyConfig{
		ID:       "eth",
		Type:     "perps",
		Platform: "hyperliquid",
		Script:   "shared_scripts/check_hyperliquid.py",
		Args:     []string{"--mode=" + mode, "ETH"},
		Hedge:    &HedgeConfig{Enabled: true, Symbol: "BTC", Ratio: 1},
	}
}

func hedgeTestState(primaryQty float64, side string) *StrategyState {
	s := &StrategyState{
		ID:        "eth",
		Type:      "perps",
		Platform:  "hyperliquid",
		Cash:      10000,
		Positions: map[string]*Position{},
	}
	if primaryQty > 0 {
		s.Positions["ETH"] = &Position{
			Symbol: "ETH", Quantity: primaryQty, InitialQuantity: primaryQty,
			AvgCost: 3000, Side: side, Multiplier: 1, OwnerStrategyID: "eth",
		}
	}
	return s
}

func hedgeTestPrices() map[string]float64 {
	return map[string]float64{"ETH": 3000, "BTC": 60000}
}

// stubHedgeExecutors swaps the injection seams for the duration of a test.
func stubHedgeExecutors(t *testing.T,
	exec func(script, symbol, side string, size, slPct float64, cancelOID int64, prevQty float64, marginMode string, leverage float64, closeFull bool, snap hlExecuteSnapshot, extra ...int64) (*HyperliquidExecuteResult, string, error),
	closeFn func(script, symbol string, sz *float64, oids []int64) (*HyperliquidCloseResult, string, error),
	cancelAfterFn func(script, symbol string, sz *float64, oids []int64) (*HyperliquidCloseResult, string, error),
) {
	t.Helper()
	prevExec, prevClose, prevCancel := hedgeExecuteFn, hedgeCloseFn, hedgeCloseCancelAfterFilFn
	if exec != nil {
		hedgeExecuteFn = exec
	}
	if closeFn != nil {
		hedgeCloseFn = closeFn
	}
	if cancelAfterFn != nil {
		hedgeCloseCancelAfterFilFn = cancelAfterFn
	}
	t.Cleanup(func() {
		hedgeExecuteFn, hedgeCloseFn, hedgeCloseCancelAfterFilFn = prevExec, prevClose, prevCancel
	})
}

func resetHedgeFailures(t *testing.T) {
	t.Helper()
	prev := globalHedgeFailures
	globalHedgeFailures = newHedgeFailureTracker()
	t.Cleanup(func() { globalHedgeFailures = prev })
}

// ---------------------------------------------------------------------------
// Paper end-to-end (AC1, AC2)
// ---------------------------------------------------------------------------

func TestHedgeSyncPaperOpensPairedLegWithOwnershipMetadata(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")

	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	hedge := s.Positions["BTC"]
	if hedge == nil {
		t.Fatal("expected a BTC hedge leg to be opened")
	}
	if hedge.Side != "short" {
		t.Errorf("hedge side = %q, want short (inverse of the long primary)", hedge.Side)
	}
	if math.Abs(hedge.Quantity-0.1) > 1e-9 {
		t.Errorf("hedge qty = %v, want 0.1 (2 ETH × $3000 × 1.0 / $60000)", hedge.Quantity)
	}
	if hedge.HedgeFor != "ETH" {
		t.Errorf("HedgeFor = %q, want ETH — this stamp is the sole ownership record", hedge.HedgeFor)
	}
	if hedge.HedgePrimaryQtyBasis != 2 {
		t.Errorf("basis = %v, want 2", hedge.HedgePrimaryQtyBasis)
	}
	if hedge.OwnerStrategyID != "eth" {
		t.Errorf("OwnerStrategyID = %q, want eth", hedge.OwnerStrategyID)
	}
	if hedge.Multiplier != 1 {
		t.Errorf("hedge Multiplier = %v, want 1 (perps PnL valuation branch)", hedge.Multiplier)
	}
	// The hedge must carry NO protection of its own.
	if hedge.StopLossOID != 0 || hedge.StopLossTriggerPx != 0 || len(hedge.TPOIDs) != 0 {
		t.Errorf("a hedge leg must carry no SL/TP: %+v", hedge)
	}
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].TradeType != TradeTypeHedge {
		t.Fatalf("expected exactly one hedge trade row, got %+v", s.TradeHistory)
	}
	if !strings.HasPrefix(s.TradeHistory[0].Details, "HEDGE(ETH)") {
		t.Errorf("hedge trade details must be self-describing, got %q", s.TradeHistory[0].Details)
	}
	if s.TradeHistory[0].Side != "sell" {
		t.Errorf("a short hedge opens with a sell, got %q", s.TradeHistory[0].Side)
	}
}

func TestHedgeSyncIsIdempotentOnceConverged(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")

	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if got := len(s.TradeHistory); got != 1 {
		t.Fatalf("a converged hedge must not re-trade; got %d trades", got)
	}
	if q := s.Positions["BTC"].Quantity; math.Abs(q-0.1) > 1e-9 {
		t.Errorf("hedge qty drifted to %v across repeated syncs", q)
	}
}

// Mark drift must not re-trade — end-to-end proof of the qty-watermark design.
func TestHedgeSyncDoesNotRetradeOnMarkDrift(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	runHedgeSync(sc, s, &mu, map[string]float64{"ETH": 4500, "BTC": 40000}, nil, nil)
	if got := len(s.TradeHistory); got != 1 {
		t.Fatalf("price movement alone must not trade the hedge; got %d trades", got)
	}
}

func TestHedgeSyncScaleInAddsProportionally(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	// Primary scales in from 2 → 3 ETH.
	s.Positions["ETH"].Quantity = 3
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	hedge := s.Positions["BTC"]
	if math.Abs(hedge.Quantity-0.15) > 1e-9 {
		t.Errorf("hedge qty after add = %v, want 0.15", hedge.Quantity)
	}
	if hedge.HedgePrimaryQtyBasis != 3 {
		t.Errorf("basis after add = %v, want 3", hedge.HedgePrimaryQtyBasis)
	}
	if got := len(s.TradeHistory); got != 2 {
		t.Errorf("expected an open + an add row, got %d", got)
	}
}

func TestHedgeSyncPartialCloseReducesHedgeProportionally(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	// A close evaluator took 50% off the primary.
	s.Positions["ETH"].Quantity = 1
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	hedge := s.Positions["BTC"]
	if hedge == nil {
		t.Fatal("a 50% primary reduction must not close the whole hedge")
	}
	if math.Abs(hedge.Quantity-0.05) > 1e-9 {
		t.Errorf("hedge qty after reduce = %v, want 0.05", hedge.Quantity)
	}
	if hedge.HedgePrimaryQtyBasis != 1 {
		t.Errorf("basis after reduce = %v, want 1", hedge.HedgePrimaryQtyBasis)
	}
}

func TestHedgeSyncClosesHedgeWhenPrimaryGoesFlat(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	// An on-chain SL fill booked by reconcile removed the primary.
	delete(s.Positions, "ETH")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("the hedge must be fully closed once the primary is flat — no untracked residual")
	}
	last := s.TradeHistory[len(s.TradeHistory)-1]
	if !last.IsClose || last.TradeType != TradeTypeHedge {
		t.Errorf("the closing leg must be a hedge close row, got %+v", last)
	}
}

// A config edit + restart bypasses the SIGHUP guard; the reconciler must unwind
// the now-unauthorized leg rather than strand it.
func TestHedgeSyncUnwindsStaleHedgeAfterConfigRemoval(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	sc.Hedge = nil // operator removed the hedge block and restarted
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("a hedge leg the config no longer authorizes must be unwound")
	}
}

func TestHedgeSyncUnwindsStaleHedgeWhenSymbolChanges(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	sc.Hedge.Symbol = "SOL"
	prices := hedgeTestPrices()
	prices["SOL"] = 150
	// A symbol change is a REPLACE: the stale leg is unwound and the newly
	// configured hedge opens in the SAME cycle, so the primary is never left
	// without a hedge for a whole strategy interval.
	runHedgeSync(sc, s, &mu, prices, nil, nil)
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("the stale BTC leg must be unwound")
	}
	sol := s.Positions["SOL"]
	if sol == nil || sol.HedgeFor != "ETH" {
		t.Fatalf("the reconfigured SOL hedge must open in the same cycle, got %+v", sol)
	}
	if sol.Side != "short" {
		t.Errorf("the replacement hedge must be inverse of the long primary, got %q", sol.Side)
	}
}

func TestHedgeSyncNoOpForUnhedgedStrategy(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	sc.Hedge = nil
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	if len(s.Positions) != 1 || len(s.TradeHistory) != 0 {
		t.Fatalf("an unhedged strategy must be untouched: positions=%d trades=%d", len(s.Positions), len(s.TradeHistory))
	}
}

// ---------------------------------------------------------------------------
// Live path + fail-closed unwind (AC6, constraint 4)
// ---------------------------------------------------------------------------

func okExecute(qty, px float64, oid int64) func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
	return func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
			Fill: &HyperliquidFill{TotalSz: qty, AvgPx: px, OID: oid, Fee: 1.25},
		}}, "", nil
	}
}

func failingExecute(msg string) func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
	return func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
		return nil, "", errors.New(msg)
	}
}

func TestHedgeSyncLiveOpenUsesRealFillAndHedgeMarginSettings(t *testing.T) {
	resetHedgeFailures(t)
	var gotSymbol, gotSide, gotMarginMode string
	var gotSize, gotSLPct, gotLeverage float64
	stubHedgeExecutors(t,
		func(_ string, symbol, side string, size, slPct float64, _ int64, _ float64, marginMode string, leverage float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
			gotSymbol, gotSide, gotSize, gotSLPct, gotMarginMode, gotLeverage = symbol, side, size, slPct, marginMode, leverage
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
				Fill: &HyperliquidFill{TotalSz: size, AvgPx: 60100, OID: 77, Fee: 2},
			}}, "", nil
		}, nil, nil)

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	sc.Hedge.MarginMode = "cross"
	sc.Hedge.Leverage = 3
	sc.MarginMode = "isolated"
	sc.Leverage = 10
	s := hedgeTestState(2, "long")

	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if gotSymbol != "BTC" || gotSide != "sell" {
		t.Errorf("order = %s %s, want sell BTC", gotSide, gotSymbol)
	}
	if math.Abs(gotSize-0.1) > 1e-9 {
		t.Errorf("order size = %v, want 0.1", gotSize)
	}
	if gotSLPct != 0 {
		t.Errorf("a hedge leg must be placed with NO stop-loss, got sl_pct=%v", gotSLPct)
	}
	// The hedge carries its OWN margin settings, never the primary's.
	if gotMarginMode != "cross" || gotLeverage != 3 {
		t.Errorf("hedge margin = (%q, %vx), want (cross, 3x) — must not inherit the primary's (isolated, 10x)", gotMarginMode, gotLeverage)
	}
	hedge := s.Positions["BTC"]
	if hedge == nil || math.Abs(hedge.AvgCost-60100) > 1e-9 {
		t.Fatalf("the hedge must be booked at the REAL fill price, got %+v", hedge)
	}
	if s.TradeHistory[0].ExchangeFee != 2 || s.TradeHistory[0].FeeSource != FeeSourceUserFills {
		t.Errorf("the real exchange fee must be booked, got fee=%v source=%q", s.TradeHistory[0].ExchangeFee, s.TradeHistory[0].FeeSource)
	}
}

// An add must NOT resend margin_mode/leverage — HL rejects update_leverage on
// an open position.
func TestHedgeSyncLiveAddDoesNotResendMarginSettings(t *testing.T) {
	resetHedgeFailures(t)
	calls := 0
	var lastMarginMode string
	var lastLeverage float64
	stubHedgeExecutors(t,
		func(_ string, _, _ string, size, _ float64, _ int64, _ float64, marginMode string, leverage float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
			calls++
			lastMarginMode, lastLeverage = marginMode, leverage
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
				Fill: &HyperliquidFill{TotalSz: size, AvgPx: 60000, OID: int64(calls), Fee: 1},
			}}, "", nil
		}, nil, nil)

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	sc.Hedge.MarginMode = "cross"
	sc.Hedge.Leverage = 3
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	s.Positions["ETH"].Quantity = 3
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if calls != 2 {
		t.Fatalf("expected an open and an add, got %d orders", calls)
	}
	if lastMarginMode != "" || lastLeverage != 0 {
		t.Errorf("an add must send no margin settings, got (%q, %v)", lastMarginMode, lastLeverage)
	}
}

// Constraint 4: hedge open fails → the WHOLE primary is closed reduce-only.
func TestHedgeOpenFailureUnwindsEntirePrimary(t *testing.T) {
	resetHedgeFailures(t)
	var closedSymbol string
	var closedSz float64
	var cancelOIDs []int64
	cancelAfterCalls := 0
	stubHedgeExecutors(t, failingExecute("insufficient margin"), nil,
		func(_ string, symbol string, sz *float64, oids []int64) (*HyperliquidCloseResult, string, error) {
			cancelAfterCalls++
			closedSymbol = symbol
			if sz != nil {
				closedSz = *sz
			}
			cancelOIDs = oids
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 2990, OID: 42, Fee: 1.5},
			}}, "", nil
		})

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(2, "long")
	s.Positions["ETH"].StopLossOID = 555
	s.Positions["ETH"].TPOIDs = []int64{556}

	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if cancelAfterCalls != 1 {
		t.Fatalf("the unwind must use the cancel-AFTER-fill close so a failed close never leaves the position naked; calls=%d", cancelAfterCalls)
	}
	if closedSymbol != "ETH" {
		t.Errorf("unwind symbol = %q, want ETH", closedSymbol)
	}
	if math.Abs(closedSz-2) > 1e-9 {
		t.Errorf("unwind must be SIZED to the whole primary (%v), got %v", 2.0, closedSz)
	}
	if len(cancelOIDs) == 0 {
		t.Error("a full unwind must cancel the primary's resting protection OIDs")
	}
	if _, ok := s.Positions["ETH"]; ok {
		t.Fatal("the primary must be booked closed after a successful unwind — never run unhedged")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("a failed hedge order must mutate no hedge state")
	}
	var closeLeg *Trade
	for i := range s.TradeHistory {
		if s.TradeHistory[i].IsClose {
			closeLeg = &s.TradeHistory[i]
		}
	}
	if closeLeg == nil || !strings.Contains(closeLeg.Details, "Unhedged-primary unwind") {
		t.Fatalf("the unwind must book an explanatory close leg, got %+v", s.TradeHistory)
	}
	if closeLeg.Price != 2990 {
		t.Errorf("the unwind must book the REAL fill price, got %v", closeLeg.Price)
	}
}

// A failed ADD unwinds ONLY the unhedged delta — the already-hedged position
// rides on untouched.
func TestHedgeAddFailureUnwindsOnlyTheAddLeg(t *testing.T) {
	resetHedgeFailures(t)
	var closedSz float64
	closeCalls := 0
	execCalls := 0
	stubHedgeExecutors(t,
		func(_ string, _, _ string, size, _ float64, _ int64, _ float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
			execCalls++
			if execCalls == 1 {
				return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
					Fill: &HyperliquidFill{TotalSz: size, AvgPx: 60000, OID: 1, Fee: 1},
				}}, "", nil
			}
			return nil, "", errors.New("add rejected")
		},
		func(_ string, _ string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
			closeCalls++
			closedSz = *sz
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 3010, OID: 9, Fee: 0.5},
			}}, "", nil
		}, nil)

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	// Primary scaled in 2 → 3; the hedge add then fails.
	s.Positions["ETH"].Quantity = 3
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if closeCalls != 1 {
		t.Fatalf("expected exactly one unwind close, got %d", closeCalls)
	}
	if math.Abs(closedSz-1) > 1e-9 {
		t.Fatalf("only the 1 ETH unhedged delta may be unwound, got %v", closedSz)
	}
	primary := s.Positions["ETH"]
	if primary == nil || math.Abs(primary.Quantity-2) > 1e-9 {
		t.Fatalf("the originally-hedged 2 ETH must survive, got %+v", primary)
	}
	hedge := s.Positions["BTC"]
	if hedge == nil || math.Abs(hedge.Quantity-0.1) > 1e-9 {
		t.Fatalf("the existing hedge leg must be untouched, got %+v", hedge)
	}
}

// A failed unwind must leave state untouched and self-heal next cycle.
func TestHedgeOpenFailureWithFailedUnwindLeavesStateIntactAndRetries(t *testing.T) {
	resetHedgeFailures(t)
	unwindAttempts := 0
	stubHedgeExecutors(t, failingExecute("hedge venue down"), nil,
		func(string, string, *float64, []int64) (*HyperliquidCloseResult, string, error) {
			unwindAttempts++
			return nil, "", errors.New("close rejected")
		})

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(2, "long")

	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	if s.Positions["ETH"] == nil {
		t.Fatal("a failed unwind must not mutate virtual state — state would diverge from the chain")
	}
	if len(s.TradeHistory) != 0 {
		t.Fatalf("no trade may be booked when nothing filled, got %+v", s.TradeHistory)
	}
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	if unwindAttempts != 2 {
		t.Fatalf("the next cycle must retry the unwind (state-derived retry), attempts=%d", unwindAttempts)
	}
}

// After N consecutive failures, new entries are held so the open→unwind loop
// stops burning fees.
func TestHedgeRepeatedOpenFailuresEngageEntryHold(t *testing.T) {
	resetHedgeFailures(t)
	stubHedgeExecutors(t, failingExecute("nope"), nil,
		func(_ string, _ string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 3000, OID: 1, Fee: 0.1},
			}}, "", nil
		})

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	for i := 0; i < hedgeOpenFailureHoldThreshold; i++ {
		s := hedgeTestState(2, "long")
		runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	}
	if !hedgeEntryHoldActive(sc) {
		t.Fatalf("entry hold must engage after %d consecutive hedge failures", hedgeOpenFailureHoldThreshold)
	}
}

func TestHedgeSuccessClearsEntryHold(t *testing.T) {
	resetHedgeFailures(t)
	for i := 0; i < hedgeOpenFailureHoldThreshold; i++ {
		globalHedgeFailures.recordFailure("eth")
	}
	if !hedgeEntryHoldActive(hedgeTestConfig("live")) {
		t.Fatal("precondition: the hold should be engaged before the successful open")
	}

	stubHedgeExecutors(t, okExecute(0.1, 60000, 5), nil, nil)
	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if hedgeEntryHoldActive(sc) {
		t.Fatal("a successful hedge open must clear the entry hold")
	}
}

// No mark for the hedge coin → cannot size → treated exactly like a failed
// open (fail closed), not silently skipped.
func TestHedgeUnpriceableHedgeCoinUnwindsPrimary(t *testing.T) {
	resetHedgeFailures(t)
	unwound := false
	stubHedgeExecutors(t, nil, nil,
		func(_ string, _ string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
			unwound = true
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 3000, OID: 3, Fee: 0.2},
			}}, "", nil
		})
	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(2, "long")
	// Only the primary is priced.
	runHedgeSync(sc, s, &mu, map[string]float64{"ETH": 3000}, nil, nil)
	if !unwound {
		t.Fatal("an unsizeable hedge leaves the primary just as unhedged as a rejected order — it must fail closed")
	}
}

// A partial hedge fill must advance the basis only in proportion, so the
// remainder is retried instead of being assumed hedged.
func TestHedgePartialFillAdvancesBasisProportionally(t *testing.T) {
	resetHedgeFailures(t)
	stubHedgeExecutors(t,
		func(_ string, _, _ string, size, _ float64, _ int64, _ float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
				Fill: &HyperliquidFill{TotalSz: size / 2, AvgPx: 60000, OID: 8, Fee: 1},
			}}, "", nil
		}, nil, nil)

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	hedge := s.Positions["BTC"]
	if hedge == nil {
		t.Fatal("the partial fill must still be booked — never drop a real fill")
	}
	if math.Abs(hedge.Quantity-0.05) > 1e-9 {
		t.Errorf("booked qty = %v, want the ACTUAL 0.05 fill", hedge.Quantity)
	}
	if math.Abs(hedge.HedgePrimaryQtyBasis-1) > 1e-9 {
		t.Errorf("basis = %v, want 1 (half of the 2 ETH the order targeted)", hedge.HedgePrimaryQtyBasis)
	}
}

func TestHedgeLiveOrderWithNoConfirmedFillMutatesNothing(t *testing.T) {
	resetHedgeFailures(t)
	stubHedgeExecutors(t,
		func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{}}, "", nil // no Fill
		}, nil,
		func(_ string, _ string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 3000, OID: 1, Fee: 0},
			}}, "", nil
		})
	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("an unconfirmed fill must never create a hedge position")
	}
}

// A failed REDUCE is not escalated — the hedge is over-sized, which is
// risk-reducing — and the next cycle retries it.
func TestHedgeReduceFailureDoesNotUnwindPrimary(t *testing.T) {
	resetHedgeFailures(t)
	execCalls := 0
	closeCalls := 0
	stubHedgeExecutors(t,
		func(_ string, _, _ string, size, _ float64, _ int64, _ float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
			execCalls++
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
				Fill: &HyperliquidFill{TotalSz: size, AvgPx: 60000, OID: 1, Fee: 1},
			}}, "", nil
		},
		func(string, string, *float64, []int64) (*HyperliquidCloseResult, string, error) {
			closeCalls++
			return nil, "", errors.New("reduce rejected")
		}, nil)

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	s.Positions["ETH"].Quantity = 1
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if closeCalls != 1 {
		t.Fatalf("expected one reduce attempt, got %d", closeCalls)
	}
	if s.Positions["ETH"] == nil || math.Abs(s.Positions["ETH"].Quantity-1) > 1e-9 {
		t.Fatal("a failed hedge REDUCE must never unwind the primary — the hedge is over-sized, not missing")
	}
	if hedge := s.Positions["BTC"]; hedge == nil || math.Abs(hedge.Quantity-0.1) > 1e-9 {
		t.Fatalf("a failed reduce must mutate no state so the next cycle retries, got %+v", hedge)
	}
}

// ---------------------------------------------------------------------------
// Booking / accounting (requirement 6)
// ---------------------------------------------------------------------------

func TestHedgeCloseBooksToOwnerLedgerWithHedgeTradeType(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	// Move BTC against the short hedge, then flatten the primary.
	delete(s.Positions, "ETH")
	prices := map[string]float64{"ETH": 3000, "BTC": 66000}
	cashBefore := s.Cash
	runHedgeSync(sc, s, &mu, prices, nil, nil)

	last := s.TradeHistory[len(s.TradeHistory)-1]
	if last.TradeType != TradeTypeHedge {
		t.Errorf("hedge close trade_type = %q, want %q", last.TradeType, TradeTypeHedge)
	}
	if last.StrategyID != "eth" {
		t.Errorf("a hedge leg must book to the OWNING strategy, got %q", last.StrategyID)
	}
	if !last.PnLGross {
		t.Error("hedge closes must use the gross PnL convention like every other perps close")
	}
	// A short BTC hedge closed 10% higher loses money — the cash effect is real
	// and must reach the owner's ledger.
	if s.Cash >= cashBefore {
		t.Errorf("the hedge's realized loss must debit the owner's cash: %.2f → %.2f", cashBefore, s.Cash)
	}
}

// A hedge loss must NOT count toward the consecutive-loss circuit breaker: an
// inverse hedge loses by construction whenever the primary wins.
func TestHedgeCloseDoesNotAdvanceLossStreakButDoesHitDailyPnL(t *testing.T) {
	resetHedgeFailures(t)
	var mu sync.RWMutex
	sc := hedgeTestConfig("paper")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	delete(s.Positions, "ETH")
	s.RiskState.ConsecutiveLosses = 0
	dailyBefore := s.RiskState.DailyPnL
	runHedgeSync(sc, s, &mu, map[string]float64{"ETH": 3000, "BTC": 66000}, nil, nil)

	if s.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("a hedge loss must not advance the loss streak, got %d", s.RiskState.ConsecutiveLosses)
	}
	if s.RiskState.DailyPnL >= dailyBefore {
		t.Errorf("a hedge loss must still reach DailyPnL: %v → %v", dailyBefore, s.RiskState.DailyPnL)
	}
}

func TestRecordHedgeTradeResultLeavesStreakAlone(t *testing.T) {
	r := &RiskState{ConsecutiveLosses: 2}
	RecordHedgeTradeResult(r, -50)
	if r.ConsecutiveLosses != 2 {
		t.Errorf("ConsecutiveLosses = %d, want 2 (untouched)", r.ConsecutiveLosses)
	}
	if r.DailyPnL != -50 {
		t.Errorf("DailyPnL = %v, want -50", r.DailyPnL)
	}
	// A hedge WIN must not reset the streak either — the primary's loss owns it.
	RecordHedgeTradeResult(r, 100)
	if r.ConsecutiveLosses != 2 {
		t.Errorf("a hedge win must not reset the streak, got %d", r.ConsecutiveLosses)
	}
}

func TestRecordTradeResultForPositionRoutesByHedgeStamp(t *testing.T) {
	r := &RiskState{}
	recordTradeResultForPosition(r, &Position{}, -10)
	if r.ConsecutiveLosses != 1 {
		t.Fatalf("an ordinary losing position must advance the streak, got %d", r.ConsecutiveLosses)
	}
	recordTradeResultForPosition(r, &Position{HedgeFor: "ETH"}, -10)
	if r.ConsecutiveLosses != 1 {
		t.Fatalf("a hedge leg must not advance the streak, got %d", r.ConsecutiveLosses)
	}
}

// Hedge round-trips must not pollute per-strategy trade-quality diagnostics.
func TestHedgeCloseSkipsTradeDiagnostics(t *testing.T) {
	var captured []string
	prev := tradeDiagnosticsRecorder
	tradeDiagnosticsRecorder = func(row *TradeDiagnosticsRow) error {
		captured = append(captured, row.Symbol)
		return nil
	}
	t.Cleanup(func() { tradeDiagnosticsRecorder = prev })

	s := &StrategyState{ID: "eth", Positions: map[string]*Position{}}
	hedge := &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short", HedgeFor: "ETH"}
	recordClosedPosition(s, hedge, 60500, -50, "hedge_close", time.Now().UTC())
	if len(s.ClosedPositions) != 1 {
		t.Fatalf("the closed_positions row must still be written, got %d", len(s.ClosedPositions))
	}
	if len(captured) != 0 {
		t.Errorf("a hedge close must not emit a trade-diagnostics row, got %v", captured)
	}
	ordinary := &Position{Symbol: "ETH", Quantity: 2, AvgCost: 3000, Side: "long"}
	recordClosedPosition(s, ordinary, 3100, 200, "signal", time.Now().UTC())
	if len(captured) != 1 || captured[0] != "ETH" {
		t.Errorf("an ordinary close must still emit diagnostics, got %v", captured)
	}
}

func TestClassifyPositionTradeTypeLabelsHedgeLegs(t *testing.T) {
	s := &StrategyState{Platform: "hyperliquid", Type: "perps"}
	if got := classifyPositionTradeType(s, &Position{Multiplier: 1}); got != "perps" {
		t.Errorf("ordinary HL perps = %q, want perps", got)
	}
	if got := classifyPositionTradeType(s, &Position{Multiplier: 1, HedgeFor: "ETH"}); got != TradeTypeHedge {
		t.Errorf("hedge leg = %q, want %q", got, TradeTypeHedge)
	}
}

// ---------------------------------------------------------------------------
// Marks
// ---------------------------------------------------------------------------

func TestCollectPerpsMarkSymbolsIncludesHedgeCoins(t *testing.T) {
	sc := hedgedPerpsStrategy("eth", "ETH", "BTC")
	hl, _ := collectPerpsMarkSymbols([]StrategyConfig{sc})
	found := map[string]bool{}
	for _, c := range hl {
		found[c] = true
	}
	if !found["ETH"] || !found["BTC"] {
		t.Fatalf("hedge coins must be marked every cycle, got %v", hl)
	}
	// A disabled hedge must not add a mark fetch.
	sc.Hedge.Enabled = false
	hl, _ = collectPerpsMarkSymbols([]StrategyConfig{sc})
	for _, c := range hl {
		if c == "BTC" {
			t.Fatal("a disabled hedge must not add a mark symbol")
		}
	}
}

// ---------------------------------------------------------------------------
// Startup consistency + direction validation
// ---------------------------------------------------------------------------

func TestValidatePerpsDirectionConfigSkipsHedgeLegs(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{{
		ID: "eth", Type: "perps", Platform: "hyperliquid",
		Direction: DirectionLong, Args: []string{"--mode=paper", "ETH"},
		Hedge: &HedgeConfig{Enabled: true, Symbol: "BTC"},
	}}}
	state := NewAppState()
	state.Strategies["eth"] = &StrategyState{ID: "eth", Type: "perps", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000},
		// A short hedge under direction="long" would otherwise warn every boot.
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, HedgeFor: "ETH"},
	}}
	if warnings := ValidatePerpsDirectionConfig(state, cfg); len(warnings) != 0 {
		t.Fatalf("an inverse hedge leg must not trip the direction check:\n%s", strings.Join(warnings, "\n"))
	}
}

func TestValidateHedgeStateConsistency(t *testing.T) {
	mkState := func(hedgeCoinSym string, primaryQty float64) *AppState {
		st := NewAppState()
		ss := &StrategyState{ID: "eth", Type: "perps", Positions: map[string]*Position{}}
		if primaryQty > 0 {
			ss.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: primaryQty, Side: "long", AvgCost: 3000}
		}
		if hedgeCoinSym != "" {
			ss.Positions[hedgeCoinSym] = &Position{Symbol: hedgeCoinSym, Quantity: 0.1, Side: "short", AvgCost: 60000, HedgeFor: "ETH"}
		}
		st.Strategies["eth"] = ss
		return st
	}
	hedged := &Config{Strategies: []StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}}

	if w := ValidateHedgeStateConsistency(mkState("BTC", 2), hedged); len(w) != 0 {
		t.Errorf("a matching hedge must produce no warnings:\n%s", strings.Join(w, "\n"))
	}

	noHedge := &Config{Strategies: []StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}}
	noHedge.Strategies[0].Hedge = nil
	w := ValidateHedgeStateConsistency(mkState("BTC", 2), noHedge)
	if len(w) != 1 || !strings.Contains(w[0], "UNWIND") {
		t.Errorf("a held leg with no config must warn about the pending unwind:\n%s", strings.Join(w, "\n"))
	}

	changed := &Config{Strategies: []StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "SOL")}}
	w = ValidateHedgeStateConsistency(mkState("BTC", 2), changed)
	if len(w) != 1 || !strings.Contains(w[0], "hedge.symbol is now SOL") {
		t.Errorf("a symbol change must warn:\n%s", strings.Join(w, "\n"))
	}

	w = ValidateHedgeStateConsistency(mkState("", 2), hedged)
	if len(w) != 1 || !strings.Contains(w[0], "NO hedge leg") {
		t.Errorf("an open primary with no hedge leg must warn:\n%s", strings.Join(w, "\n"))
	}

	// Flat + hedge configured is the ordinary case — silence.
	if w := ValidateHedgeStateConsistency(mkState("", 0), hedged); len(w) != 0 {
		t.Errorf("a flat hedged strategy must be silent:\n%s", strings.Join(w, "\n"))
	}

	// A strategy dropped from the config but still holding a leg.
	orphan := &Config{Strategies: nil}
	w = ValidateHedgeStateConsistency(mkState("BTC", 2), orphan)
	if len(w) != 1 || !strings.Contains(w[0], "no longer in the config") {
		t.Errorf("an orphaned hedge leg must warn:\n%s", strings.Join(w, "\n"))
	}
}

// ---------------------------------------------------------------------------
// Kill switch / circuit breaker
// ---------------------------------------------------------------------------

func TestForceCloseHyperliquidLiveIncludesHeldHedgeCoins(t *testing.T) {
	positions := []HLPosition{
		{Coin: "ETH", Size: 2},
		{Coin: "BTC", Size: -0.1},
		{Coin: "DOGE", Size: 500}, // genuinely foreign
	}
	hlLiveAll := []StrategyConfig{{ID: "eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}}
	var closed []string
	closer := func(coin string, _ *float64, _ []int64) (*HyperliquidCloseResult, error) {
		closed = append(closed, coin)
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{TotalSz: 1, AvgPx: 1}}}, nil
	}
	report := forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, map[string]bool{"BTC": true}, closer, nil)
	if len(report.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", report.Errors)
	}
	got := map[string]bool{}
	for _, c := range closed {
		got[c] = true
	}
	if !got["ETH"] || !got["BTC"] {
		t.Errorf("the kill switch must flatten the primary AND the held hedge, closed=%v", closed)
	}
	if got["DOGE"] {
		t.Error("an unowned coin must never be liquidated")
	}
}

func TestForceCloseHyperliquidLiveIgnoresUnheldHedgeCoin(t *testing.T) {
	positions := []HLPosition{{Coin: "BTC", Size: -0.1}}
	hlLiveAll := []StrategyConfig{{ID: "eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}}
	closed := 0
	closer := func(string, *float64, []int64) (*HyperliquidCloseResult, error) {
		closed++
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{TotalSz: 1, AvgPx: 1}}}, nil
	}
	// heldHedgeCoins is empty: the strategy declares BTC but holds no leg, so
	// the on-chain BTC position is someone else's.
	forceCloseHyperliquidLive(context.Background(), positions, hlLiveAll, nil, closer, nil)
	if closed != 0 {
		t.Fatal("a declared-but-unheld hedge coin must not be liquidated by the kill switch")
	}
}

func TestSnapshotHyperliquidVirtualQuantitiesIncludesHedgeLegs(t *testing.T) {
	states := map[string]*StrategyState{"eth": {ID: "eth", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long"},
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", HedgeFor: "ETH"},
	}}}
	hlLiveAll := []StrategyConfig{{ID: "eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}}
	snap := snapshotHyperliquidVirtualQuantities(states, hlLiveAll)
	if snap["ETH"]["eth"] != 2 {
		t.Errorf("primary qty missing from snapshot: %v", snap)
	}
	if snap["BTC"]["eth"] != 0.1 {
		t.Errorf("hedge qty must be snapshotted under its own coin key, got %v", snap)
	}
}

func TestKillSwitchFillApplicationBooksHedgeLeg(t *testing.T) {
	sc := StrategyConfig{ID: "eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}
	s := &StrategyState{ID: "eth", Type: "perps", Platform: "hyperliquid", Cash: 1000, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000, Multiplier: 1},
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, Multiplier: 1, HedgeFor: "ETH"},
	}}
	fills := map[string]HyperliquidCloseFill{
		"ETH": {TotalSz: 2, AvgPx: 3100, OID: 1, Fee: 1},
		"BTC": {TotalSz: 0.1, AvgPx: 59000, OID: 2, Fee: 0.5},
	}
	hlLiveAll := []StrategyConfig{sc}
	snap := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{"eth": s}, hlLiveAll)

	if !applyHyperliquidKillSwitchCloseFill(s, sc, fills, hlLiveAll, snap) {
		t.Fatal("expected the kill-switch fills to apply")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("the hedge leg must be booked closed, not stranded for the generic mark sweep")
	}
	if _, ok := s.Positions["ETH"]; ok {
		t.Fatal("the primary must be booked closed")
	}
	var hedgeClose *Trade
	for i := range s.TradeHistory {
		if s.TradeHistory[i].Symbol == "BTC" {
			hedgeClose = &s.TradeHistory[i]
		}
	}
	if hedgeClose == nil {
		t.Fatal("the hedge close must produce a Trade row")
	}
	if hedgeClose.TradeType != TradeTypeHedge {
		t.Errorf("hedge kill-switch close trade_type = %q, want %q", hedgeClose.TradeType, TradeTypeHedge)
	}
	if hedgeClose.Price != 59000 {
		t.Errorf("the hedge close must book the REAL fill price, got %v", hedgeClose.Price)
	}
}

// The hedge fill must apply even when the primary produced no fill at all.
func TestKillSwitchBooksHedgeEvenWithNoPrimaryFill(t *testing.T) {
	sc := StrategyConfig{ID: "eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}
	s := &StrategyState{ID: "eth", Type: "perps", Platform: "hyperliquid", Cash: 1000, Positions: map[string]*Position{
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, Multiplier: 1, HedgeFor: "ETH"},
	}}
	fills := map[string]HyperliquidCloseFill{"BTC": {TotalSz: 0.1, AvgPx: 59000, OID: 2, Fee: 0.5}}
	hlLiveAll := []StrategyConfig{sc}
	snap := snapshotHyperliquidVirtualQuantities(map[string]*StrategyState{"eth": s}, hlLiveAll)

	if !applyHyperliquidKillSwitchCloseFill(s, sc, fills, hlLiveAll, snap) {
		t.Fatal("the hedge fill must apply even when the primary had nothing to close")
	}
	if _, ok := s.Positions["BTC"]; ok {
		t.Fatal("the hedge leg must be booked closed")
	}
}

func TestCircuitBreakerPendingIncludesHedgeSymbol(t *testing.T) {
	sc := StrategyConfig{ID: "eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}
	s := &StrategyState{ID: "eth", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000},
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, HedgeFor: "ETH"},
	}}
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -0.1}},
		HLLiveAll:   []StrategyConfig{sc},
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)
	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil {
		t.Fatal("expected a pending circuit close")
	}
	syms := map[string]float64{}
	for _, c := range p.Symbols {
		syms[c.Symbol] = c.Size
	}
	if syms["ETH"] != 2 {
		t.Errorf("primary close missing/mis-sized: %v", syms)
	}
	if syms["BTC"] != 0.1 {
		t.Errorf("the hedge coin must be enqueued for the CB close too, got %v", syms)
	}
}

// When the PRIMARY coin is shared with peers the CB leaves it alone — but the
// sole-owned hedge must still be closed, or forceCloseAllPositions zeroes it
// virtually while real exposure keeps running.
func TestCircuitBreakerSharedPrimaryStillEnqueuesHedge(t *testing.T) {
	sc := StrategyConfig{ID: "eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}
	peer := StrategyConfig{ID: "eth2", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}
	s := &StrategyState{ID: "eth", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000},
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, HedgeFor: "ETH"},
	}}
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 4}, {Coin: "BTC", Size: -0.1}},
		HLLiveAll:   []StrategyConfig{sc, peer},
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)
	p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil {
		t.Fatal("a shared primary must still enqueue the sole-owned hedge close")
	}
	if len(p.Symbols) != 1 || p.Symbols[0].Symbol != "BTC" {
		t.Fatalf("only the hedge coin may be enqueued when the primary is shared, got %+v", p.Symbols)
	}
}

func TestCircuitBreakerNoHedgeNoPendingWhenSharedPrimary(t *testing.T) {
	sc := StrategyConfig{ID: "eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}
	peer := StrategyConfig{ID: "eth2", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live", "ETH"}}
	s := &StrategyState{ID: "eth", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000},
	}}
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 4}},
		HLLiveAll:   []StrategyConfig{sc, peer},
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)
	if p := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid); p != nil {
		t.Fatalf("pre-#1159 behavior must be preserved for unhedged shared-coin strategies, got %+v", p)
	}
}

// ---------------------------------------------------------------------------
// Shared-wallet attribution (requirement 6)
// ---------------------------------------------------------------------------

func TestBuildSharedWalletBooksAttributesHedgeCoin(t *testing.T) {
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	sc := hedgedPerpsStrategy("eth", "ETH", "BTC")
	byID := map[string]StrategyConfig{"eth": sc}
	state := NewAppState()
	state.Strategies["eth"] = &StrategyState{ID: "eth", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000},
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, HedgeFor: "ETH"},
	}}
	_, virtualQty := buildSharedWalletBooks(key, []string{"eth"}, byID, state)
	if virtualQty["ETH"]["eth"] != 2 {
		t.Errorf("primary qty missing: %v", virtualQty)
	}
	if virtualQty["BTC"]["eth"] != 0.1 {
		t.Fatalf("the hedge coin must attribute to its owner (else uPnL and funding book as orphan drift), got %v", virtualQty)
	}
}

// ---------------------------------------------------------------------------
// Manual surfaces
// ---------------------------------------------------------------------------

func TestPendingManualActionRefusesHedgeCoin(t *testing.T) {
	sc := hedgedPerpsStrategy("eth", "ETH", "BTC")
	sc.Args = []string{"--mode=live", "ETH"}
	err := validatePendingManualActionStrategy(sc, PendingManualAction{StrategyID: "eth", Action: "close", Symbol: "BTC"})
	if err == nil || !strings.Contains(err.Error(), "correlated hedge coin") {
		t.Fatalf("a queued action on the hedge coin must be refused, got: %v", err)
	}
	// The primary coin is still allowed.
	if err := validatePendingManualActionStrategy(sc, PendingManualAction{StrategyID: "eth", Action: "close", Symbol: "ETH"}); err != nil {
		t.Fatalf("the primary coin must stay actionable, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Persistence + lifetime stats
// ---------------------------------------------------------------------------

// AC3: hedge ownership must survive a restart from the persisted row alone.
func TestSaveLoadStateHedgeMetadataRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"eth": {
				ID: "eth", Type: "perps", Platform: "hyperliquid",
				Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 2, AvgCost: 3000, Side: "long", Multiplier: 1, OpenedAt: now},
					"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1, OpenedAt: now,
						HedgeFor: "ETH", HedgePrimaryQtyBasis: 2},
				},
				TradeHistory: []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	hedge := loaded.Strategies["eth"].Positions["BTC"]
	if hedge.HedgeFor != "ETH" {
		t.Errorf("HedgeFor = %q, want ETH — restart recovery reads ownership from this column only", hedge.HedgeFor)
	}
	if hedge.HedgePrimaryQtyBasis != 2 {
		t.Errorf("HedgePrimaryQtyBasis = %v, want 2", hedge.HedgePrimaryQtyBasis)
	}
	// An ordinary position must round-trip with an empty stamp.
	if got := loaded.Strategies["eth"].Positions["ETH"].HedgeFor; got != "" {
		t.Errorf("an ordinary position must not carry a hedge stamp, got %q", got)
	}
	// And the reconciler must find the leg by the persisted stamp alone.
	pos, coin := hedgePositionOf(loaded.Strategies["eth"])
	if pos == nil || coin != "BTC" {
		t.Fatalf("hedge ownership must be recoverable from state alone, got (%v, %q)", pos, coin)
	}
}

// Requirement 2 + 6: hedge legs are excluded from #T / W-L, but their cash
// effect stays in the strategy's ledger.
func TestLifetimeStatsExcludeHedgeLegs(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"eth": {
				ID: "eth", Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{},
				TradeHistory: []Trade{
					{Timestamp: now, StrategyID: "eth", Symbol: "ETH", PositionID: "p1", Side: "buy",
						Quantity: 2, Price: 3000, Value: 6000, TradeType: "perps", Details: "open"},
					{Timestamp: now.Add(time.Minute), StrategyID: "eth", Symbol: "ETH", PositionID: "p1", Side: "sell",
						Quantity: 2, Price: 3200, Value: 6400, TradeType: "perps", Details: "close",
						IsClose: true, RealizedPnL: 400, PnLGross: true, ExchangeFee: 5},
					{Timestamp: now, StrategyID: "eth", Symbol: "BTC", PositionID: "h1", Side: "sell",
						Quantity: 0.1, Price: 60000, Value: 6000, TradeType: TradeTypeHedge, Details: "HEDGE(ETH) open",
						ExchangeFee: 3, PnLGross: true},
					{Timestamp: now.Add(time.Minute), StrategyID: "eth", Symbol: "BTC", PositionID: "h1", Side: "buy",
						Quantity: 0.1, Price: 61000, Value: 6100, TradeType: TradeTypeHedge, Details: "HEDGE(ETH) close",
						IsClose: true, RealizedPnL: -100, PnLGross: true, ExchangeFee: 3},
				},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	all, err := db.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	got := all["eth"]
	if got.PositionsOpened != 1 {
		t.Errorf("#T = %d, want 1 — the hedge open is not a separate round-trip", got.PositionsOpened)
	}
	if got.Wins != 1 || got.Losses != 0 {
		t.Errorf("W/L = %d/%d, want 1/0 — the hedge close must not register as a loss", got.Wins, got.Losses)
	}
	one, err := db.LifetimeTradeStatsForStrategy("eth")
	if err != nil {
		t.Fatalf("LifetimeTradeStatsForStrategy: %v", err)
	}
	if one.PositionsOpened != got.PositionsOpened || one.Wins != got.Wins || one.Losses != got.Losses {
		t.Errorf("per-strategy stats must match the bulk query: %+v vs %+v", one, got)
	}
	// The ledger deliberately DOES include hedge rows.
	ledger, err := db.LedgerNetByStrategy([]string{"eth"})
	if err != nil {
		t.Fatalf("LedgerNetByStrategy: %v", err)
	}
	delta := ledger["eth"]
	// primary: +400-5 ; hedge: -3 (open fee) and -100-3 (close)
	want := 400.0 - 5 - 3 - 100 - 3
	if math.Abs(delta-want) > 1e-6 {
		t.Errorf("ledger delta = %v, want %v — hedge PnL and fees must book to the owner", delta, want)
	}
}

// ---------------------------------------------------------------------------
// Reconcile (AC3)
// ---------------------------------------------------------------------------

func TestReconcileHedgeCoinsResyncsQtyFromChain(t *testing.T) {
	lm, _ := NewLogManager("")
	defer lm.Close()
	state := NewAppState()
	state.Strategies["eth"] = &StrategyState{ID: "eth", Type: "perps", Platform: "hyperliquid", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000, Multiplier: 1},
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, Multiplier: 1, HedgeFor: "ETH", HedgePrimaryQtyBasis: 2},
	}}
	// On-chain the hedge is smaller than state thinks (a partial external close).
	positions := []HLPosition{{Coin: "ETH", Size: 2, EntryPrice: 3000}, {Coin: "BTC", Size: -0.06, EntryPrice: 60500}}
	changed := false
	var alerts []ProtectionFillAlert
	reconcileHedgeCoins([]StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}, state, lm, positions, noFillFeeResolver, &alerts, &changed)

	hedge := state.Strategies["eth"].Positions["BTC"]
	if hedge == nil {
		t.Fatal("the hedge leg must survive a qty resync")
	}
	if math.Abs(hedge.Quantity-0.06) > 1e-9 {
		t.Errorf("hedge qty = %v, want the on-chain 0.06", hedge.Quantity)
	}
	if !changed {
		t.Error("a resync must report state as changed")
	}
}

// An externally closed hedge must be booked (not silently dropped), and the
// #822 orphan auto-close must never fire on a hedge leg.
func TestReconcileHedgeCoinsBooksExternalCloseAndSkipsOrphanQueue(t *testing.T) {
	lm, _ := NewLogManager("")
	defer lm.Close()
	state := NewAppState()
	state.Strategies["eth"] = &StrategyState{ID: "eth", Type: "perps", Platform: "hyperliquid", Cash: 1000, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000, Multiplier: 1},
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, Multiplier: 1, HedgeFor: "ETH", HedgePrimaryQtyBasis: 2},
	}}
	positions := []HLPosition{{Coin: "ETH", Size: 2, EntryPrice: 3000}} // BTC gone
	changed := false
	var alerts []ProtectionFillAlert
	reconcileHedgeCoins([]StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}, state, lm, positions, noFillFeeResolver, &alerts, &changed)

	if _, ok := state.Strategies["eth"].Positions["BTC"]; ok {
		t.Fatal("an externally closed hedge must be removed from state")
	}
	trades := state.Strategies["eth"].TradeHistory
	if len(trades) != 1 || !trades[0].IsClose {
		t.Fatalf("the external hedge close must produce a close row, got %+v", trades)
	}
	if trades[0].TradeType != TradeTypeHedge {
		t.Errorf("the external close row must be labelled hedge, got %q", trades[0].TradeType)
	}
}

// A foreign on-chain position on a declared hedge coin must never be adopted.
func TestReconcileHedgeCoinsNeverAdoptsForeignPosition(t *testing.T) {
	lm, _ := NewLogManager("")
	defer lm.Close()
	state := NewAppState()
	state.Strategies["eth"] = &StrategyState{ID: "eth", Type: "perps", Platform: "hyperliquid", Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 2, Side: "long", AvgCost: 3000, Multiplier: 1},
	}}
	positions := []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -0.5, EntryPrice: 60000}}
	changed := false
	var alerts []ProtectionFillAlert
	reconcileHedgeCoins([]StrategyConfig{hedgedPerpsStrategy("eth", "ETH", "BTC")}, state, lm, positions, noFillFeeResolver, &alerts, &changed)

	if _, ok := state.Strategies["eth"].Positions["BTC"]; ok {
		t.Fatal("a foreign BTC position must NOT be adopted — ownership comes from persisted metadata only")
	}
	if len(state.Strategies["eth"].TradeHistory) != 0 {
		t.Fatal("no guessed fill may be booked for a foreign position")
	}
}

// #822 exemption: an inverse hedge must never be queued for the regime/direction
// orphan auto-close, which would flatten it every cycle.
func TestReconcileDoesNotQueueHedgeLegForOrphanClose(t *testing.T) {
	lm, _ := NewLogManager("")
	defer lm.Close()
	sc := StrategyConfig{ID: "eth", Type: "perps", Platform: "hyperliquid",
		Direction: DirectionLong, Args: []string{"--mode=live", "BTC"}}
	ss := &StrategyState{ID: "eth", Type: "perps", Platform: "hyperliquid", Positions: map[string]*Position{
		"BTC": {Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, Multiplier: 1, HedgeFor: "ETH"},
	}}
	logger, _ := lm.GetStrategyLogger("eth")
	var orphans []RegimeDirectionOrphanCloseJob
	var alerts []ProtectionFillAlert
	reconcileHyperliquidPositionsWithResolver(ss, "BTC", []HLPosition{{Coin: "BTC", Size: -0.1, EntryPrice: 60000}},
		noFillFeeResolver, logger, &alerts, &orphans, sc)
	if len(orphans) != 0 {
		t.Fatalf("a hedge leg must never be queued for the #822 orphan auto-close, got %+v", orphans)
	}
}

// ---------------------------------------------------------------------------
// Cycle sweep
// ---------------------------------------------------------------------------

func TestHedgeSweepConvergesStrategiesTheDispatchDidNotReach(t *testing.T) {
	resetHedgeFailures(t)
	lm, _ := NewLogManager("")
	defer lm.Close()
	var mu sync.RWMutex
	state := NewAppState()
	state.Strategies["eth"] = hedgeTestState(2, "long")
	sc := hedgeTestConfig("paper")

	runHedgeSweep([]StrategyConfig{sc}, state, &mu, lm, hedgeTestPrices(), nil, nil)
	if state.Strategies["eth"].Positions["BTC"] == nil {
		t.Fatal("the sweep must hedge a strategy the dispatch never reached")
	}
}

func TestHedgeSweepSkipsStrategiesAlreadySyncedThisCycle(t *testing.T) {
	resetHedgeFailures(t)
	lm, _ := NewLogManager("")
	defer lm.Close()
	var mu sync.RWMutex
	state := NewAppState()
	state.Strategies["eth"] = hedgeTestState(2, "long")
	sc := hedgeTestConfig("paper")

	runHedgeSweep([]StrategyConfig{sc}, state, &mu, lm, hedgeTestPrices(), map[string]bool{"eth": true}, nil)
	if state.Strategies["eth"].Positions["BTC"] != nil {
		t.Fatal("a strategy the dispatch already synced must not be re-attempted in the same cycle")
	}
}

// A strategy whose hedge config was removed but which still HOLDS a leg must
// still be swept, so the stale leg gets unwound.
func TestHedgeSweepStillVisitsStrategiesHoldingAStaleLeg(t *testing.T) {
	resetHedgeFailures(t)
	lm, _ := NewLogManager("")
	defer lm.Close()
	var mu sync.RWMutex
	state := NewAppState()
	ss := hedgeTestState(2, "long")
	ss.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, Side: "short", AvgCost: 60000, Multiplier: 1, HedgeFor: "ETH", HedgePrimaryQtyBasis: 2}
	state.Strategies["eth"] = ss
	sc := hedgeTestConfig("paper")
	sc.Hedge = nil

	runHedgeSweep([]StrategyConfig{sc}, state, &mu, lm, hedgeTestPrices(), nil, nil)
	if _, ok := ss.Positions["BTC"]; ok {
		t.Fatal("a held-but-unconfigured hedge leg must be unwound by the sweep")
	}
}

func TestHedgeSweepIgnoresUnhedgedStrategies(t *testing.T) {
	resetHedgeFailures(t)
	lm, _ := NewLogManager("")
	defer lm.Close()
	var mu sync.RWMutex
	state := NewAppState()
	state.Strategies["eth"] = hedgeTestState(2, "long")
	sc := hedgeTestConfig("paper")
	sc.Hedge = nil

	runHedgeSweep([]StrategyConfig{sc}, state, &mu, lm, hedgeTestPrices(), nil, nil)
	if len(state.Strategies["eth"].TradeHistory) != 0 {
		t.Fatal("an unhedged strategy must be untouched by the sweep")
	}
}

// The drain's shared-primary guard used to clear the WHOLE pending. With a
// hedge leg enqueued alongside a shared primary, that would drop the hedge
// close while forceCloseAllPositions had already zeroed it virtually —
// stranding real on-chain exposure with no virtual record. Strip only the
// shared primary.
func TestCircuitDrainStripsOnlySharedPrimaryAndStillClosesHedge(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {
			ID: "hl-a", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: 1, Side: "long", AvgCost: 3000, Multiplier: 1},
				"BTC": {Symbol: "BTC", Quantity: 0.05, Side: "short", AvgCost: 60000, Multiplier: 1, HedgeFor: "ETH"},
			},
			RiskState: RiskState{CircuitBreaker: true, CircuitBreakerUntil: time.Now().Add(24 * time.Hour)},
		},
	}}
	state.Strategies["hl-a"].RiskState.setPendingCircuitClose(PlatformPendingCloseHyperliquid, &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC", Size: 0.05}, {Symbol: "ETH", Size: 1}},
	})
	cfg := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
		// A peer on the primary coin makes ETH shared.
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h", "--mode=live"}},
	}
	var mu sync.RWMutex
	var calls []string
	closer := func(sym string, partialSz *float64, _ []int64) (*HyperliquidCloseResult, error) {
		calls = append(calls, sym)
		sz := 0.0
		if partialSz != nil {
			sz = *partialSz
		}
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: sym,
			Fill: &HyperliquidCloseFill{TotalSz: sz, AvgPx: 60000}}}, nil
	}
	runPendingHyperliquidCircuitCloses(
		context.Background(), state, cfg, "0xabc",
		[]HLPosition{{Coin: "ETH", Size: 2, EntryPrice: 3000}, {Coin: "BTC", Size: -0.05, EntryPrice: 60000}},
		true, nil, closer, 30*time.Second, &mu, nil,
	)
	if len(calls) != 1 || calls[0] != "BTC" {
		t.Fatalf("only the sole-owned hedge coin may be closed when the primary is shared, calls=%v", calls)
	}
	if _, ok := state.Strategies["hl-a"].Positions["BTC"]; ok {
		t.Error("the hedge leg must be booked closed by the drain")
	}
	if _, ok := state.Strategies["hl-a"].Positions["ETH"]; !ok {
		t.Error("the shared primary must be left untouched on-chain and in state")
	}
}

// ---------------------------------------------------------------------------
// Side-flip safety (review #1406: amplified same-direction exposure)
// ---------------------------------------------------------------------------

// hedgeFlipFixture returns a converged hedged pair, then flips the primary so
// the surviving hedge leg is momentarily SAME-direction as the primary.
func hedgeFlipFixture(t *testing.T, primarySide, flippedSide string, primaryQty float64) (StrategyConfig, *StrategyState) {
	t.Helper()
	sc := hedgeTestConfig("live")
	s := hedgeTestState(primaryQty, primarySide)
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: primaryQty * 3000 / 60000, InitialQuantity: primaryQty * 3000 / 60000,
		AvgCost: 60000, Side: hedgeInverseSide(primarySide), Multiplier: 1,
		OwnerStrategyID: "eth", HedgeFor: "ETH", HedgePrimaryQtyBasis: primaryQty,
	}
	s.Positions["ETH"].Side = flippedSide
	return sc, s
}

// The blocking finding: a failed side-mismatch close leaves a residual leg on
// the SAME side as the flipped primary — roughly double correlated exposure,
// not a benign over-hedge — so it must fail-close the primary.
func TestHedgeFlipCloseFailureUnwindsPrimary(t *testing.T) {
	for _, c := range []struct{ name, from, to string }{
		{"long to short", "long", "short"},
		{"short to long", "short", "long"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetHedgeFailures(t)
			var unwoundSz float64
			unwinds := 0
			stubHedgeExecutors(t, nil,
				func(string, string, *float64, []int64) (*HyperliquidCloseResult, string, error) {
					return nil, "", errors.New("hedge venue rejected the close")
				},
				func(_ string, _ string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
					unwinds++
					unwoundSz = *sz
					return &HyperliquidCloseResult{Close: &HyperliquidClose{
						Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 3000, OID: 11, Fee: 1},
					}}, "", nil
				})

			var mu sync.RWMutex
			sc, s := hedgeFlipFixture(t, c.from, c.to, 2)
			runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

			if unwinds != 1 {
				t.Fatalf("a failed side-mismatch close must fail-close the primary, unwinds=%d", unwinds)
			}
			if math.Abs(unwoundSz-2) > 1e-9 {
				t.Errorf("the WHOLE primary must unwind (the residual leg hedges none of the new side), got %v", unwoundSz)
			}
			if _, ok := s.Positions["ETH"]; ok {
				t.Error("the primary must be booked closed — it was running at amplified exposure")
			}
			if globalHedgeFailures.count("eth") != 1 {
				t.Errorf("the failure must count toward the entry hold, got %d", globalHedgeFailures.count("eth"))
			}
		})
	}
}

// Case (c): a flip right after a scale-in leaves a residual leg LARGER than the
// pre-flip hedge. The stale basis must not be credited as covered exposure.
func TestHedgeFlipAfterScaleInUnwindsWholePrimaryNotJustTheDelta(t *testing.T) {
	resetHedgeFailures(t)
	var unwoundSz float64
	stubHedgeExecutors(t, nil,
		func(string, string, *float64, []int64) (*HyperliquidCloseResult, string, error) {
			return nil, "", errors.New("close rejected")
		},
		func(_ string, _ string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
			unwoundSz = *sz
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 3000, OID: 12, Fee: 1},
			}}, "", nil
		})

	var mu sync.RWMutex
	sc, s := hedgeFlipFixture(t, "long", "short", 3) // scaled in to 3 ETH, basis 3
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if math.Abs(unwoundSz-3) > 1e-9 {
		t.Fatalf("the stale basis must NOT be treated as covered — expected the whole 3 ETH, got %v", unwoundSz)
	}
}

// Case (d), the over-escalation guard: a genuine proportional reduce failure on
// a still-INVERSE hedge stays benign and must not unwind anything.
func TestHedgeInverseReduceFailureStillDoesNotUnwind(t *testing.T) {
	resetHedgeFailures(t)
	unwinds := 0
	stubHedgeExecutors(t, nil,
		func(string, string, *float64, []int64) (*HyperliquidCloseResult, string, error) {
			return nil, "", errors.New("reduce rejected")
		},
		func(string, string, *float64, []int64) (*HyperliquidCloseResult, string, error) {
			unwinds++
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: 1, AvgPx: 3000},
			}}, "", nil
		})

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(1, "long") // primary shrank 2 → 1
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short",
		Multiplier: 1, HedgeFor: "ETH", HedgePrimaryQtyBasis: 2}

	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if unwinds != 0 {
		t.Fatal("an over-sized but still-inverse hedge is risk-reducing — it must never unwind the primary")
	}
	if s.Positions["ETH"] == nil || globalHedgeFailures.count("eth") != 0 {
		t.Error("a benign reduce failure must not count toward the entry hold or touch the primary")
	}
}

// A failed close with the primary already flat has no primary to unwind — the
// reduce-only retry is the only action, and it must not be mis-escalated.
func TestHedgeCloseFailureWithFlatPrimaryDoesNotEscalate(t *testing.T) {
	resetHedgeFailures(t)
	unwinds := 0
	stubHedgeExecutors(t, nil,
		func(string, string, *float64, []int64) (*HyperliquidCloseResult, string, error) {
			return nil, "", errors.New("close rejected")
		},
		func(string, string, *float64, []int64) (*HyperliquidCloseResult, string, error) {
			unwinds++
			return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{TotalSz: 1, AvgPx: 1}}}, "", nil
		})

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(0, "")
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short",
		Multiplier: 1, HedgeFor: "ETH", HedgePrimaryQtyBasis: 2}

	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if unwinds != 0 {
		t.Fatal("there is no primary to unwind when it is already flat")
	}
	if s.Positions["BTC"] == nil {
		t.Error("a failed close must mutate no state so the next cycle retries")
	}
}

// ---------------------------------------------------------------------------
// Same-cycle replace (review #1406 optional 2)
// ---------------------------------------------------------------------------

// Case (a): a clean flip closes the stale leg AND opens the correctly-sided one
// in a single cycle, so the primary is never unhedged for a strategy interval.
func TestHedgeFlipReplacesHedgeInOneCycle(t *testing.T) {
	resetHedgeFailures(t)
	var orders []string
	stubHedgeExecutors(t,
		func(_ string, symbol, side string, size, _ float64, _ int64, _ float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
			orders = append(orders, "open:"+side+":"+symbol)
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
				Fill: &HyperliquidFill{TotalSz: size, AvgPx: 60000, OID: 3, Fee: 1},
			}}, "", nil
		},
		func(_ string, symbol string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
			orders = append(orders, "close:"+symbol)
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 60000, OID: 2, Fee: 1},
			}}, "", nil
		}, nil)

	var mu sync.RWMutex
	sc, s := hedgeFlipFixture(t, "long", "short", 2)
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if len(orders) != 2 || orders[0] != "close:BTC" || orders[1] != "open:buy:BTC" {
		t.Fatalf("expected a same-cycle close-then-reopen, got %v", orders)
	}
	hedge := s.Positions["BTC"]
	if hedge == nil || hedge.Side != "long" {
		t.Fatalf("the replacement hedge must be LONG (inverse of the flipped short primary), got %+v", hedge)
	}
	if hedge.HedgePrimaryQtyBasis != 2 {
		t.Errorf("the replacement hedge must re-base on the current primary qty, got %v", hedge.HedgePrimaryQtyBasis)
	}
	if s.Positions["ETH"] == nil {
		t.Error("a successful replace must leave the primary running")
	}
}

// Case (b): if the reopen half of a replace cannot be sized, the now-unhedged
// primary must still fail-close.
func TestHedgeFlipReopenFailureStillFailsClosed(t *testing.T) {
	resetHedgeFailures(t)
	var unwoundSz float64
	stubHedgeExecutors(t,
		func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
			return nil, "", errors.New("reopen rejected")
		},
		func(_ string, _ string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 60000, OID: 2, Fee: 1},
			}}, "", nil
		},
		func(_ string, _ string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
			unwoundSz = *sz
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 3000, OID: 13, Fee: 1},
			}}, "", nil
		})

	var mu sync.RWMutex
	sc, s := hedgeFlipFixture(t, "long", "short", 2)
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if _, ok := s.Positions["BTC"]; ok {
		t.Error("the stale leg should have been closed successfully")
	}
	if math.Abs(unwoundSz-2) > 1e-9 {
		t.Fatalf("a failed reopen must fail-close the whole now-unhedged primary, unwound %v", unwoundSz)
	}
	if _, ok := s.Positions["ETH"]; ok {
		t.Error("the primary must be booked closed after the reopen failed")
	}
}

// Case (c): a second flip on the following cycle converges just as cleanly.
func TestHedgeRapidDoubleFlipConvergesEachCycle(t *testing.T) {
	resetHedgeFailures(t)
	stubHedgeExecutors(t,
		func(_ string, _, _ string, size, _ float64, _ int64, _ float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
				Fill: &HyperliquidFill{TotalSz: size, AvgPx: 60000, OID: 4, Fee: 1},
			}}, "", nil
		},
		func(_ string, _ string, sz *float64, _ []int64) (*HyperliquidCloseResult, string, error) {
			return &HyperliquidCloseResult{Close: &HyperliquidClose{
				Fill: &HyperliquidCloseFill{TotalSz: *sz, AvgPx: 60000, OID: 5, Fee: 1},
			}}, "", nil
		}, nil)

	var mu sync.RWMutex
	sc, s := hedgeFlipFixture(t, "long", "short", 2)
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	if s.Positions["BTC"].Side != "long" {
		t.Fatalf("first flip: hedge side = %q, want long", s.Positions["BTC"].Side)
	}
	// Flip straight back on the next cycle.
	s.Positions["ETH"].Side = "long"
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	if s.Positions["BTC"].Side != "short" {
		t.Fatalf("second flip: hedge side = %q, want short", s.Positions["BTC"].Side)
	}
	if s.Positions["ETH"] == nil {
		t.Error("neither flip should have unwound the primary — both replaces succeeded")
	}
}

// An ordinary partial fill must NOT re-fire within the cycle: looping there
// would triple order count on a thin book chasing a gap the next cycle closes.
func TestHedgePartialFillDoesNotReorderWithinTheCycle(t *testing.T) {
	resetHedgeFailures(t)
	orders := 0
	stubHedgeExecutors(t,
		func(_ string, _, _ string, size, _ float64, _ int64, _ float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
			orders++
			return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
				Fill: &HyperliquidFill{TotalSz: size / 2, AvgPx: 60000, OID: 6, Fee: 1},
			}}, "", nil
		}, nil, nil)

	var mu sync.RWMutex
	sc := hedgeTestConfig("live")
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)

	if orders != 1 {
		t.Fatalf("a partial fill must be retried NEXT cycle, not re-ordered immediately; orders=%d", orders)
	}
}

// ---------------------------------------------------------------------------
// Entry hold recovery (review #1406 optional 1)
// ---------------------------------------------------------------------------

// The hold must have a reachable clear condition — it blocks the very opens
// that a success-only rule would need, so it expires on a timer.
func TestHedgeEntryHoldExpiresAndReArms(t *testing.T) {
	resetHedgeFailures(t)
	now := time.Now()
	globalHedgeFailures.now = func() time.Time { return now }

	sc := hedgeTestConfig("live")
	var dms int
	for i := 0; i < hedgeOpenFailureHoldThreshold; i++ {
		if _, first := globalHedgeFailures.recordFailure("eth"); first {
			dms++
		}
	}
	if !hedgeEntryHoldActive(sc) {
		t.Fatal("the hold must engage at the threshold")
	}
	if dms != 1 {
		t.Errorf("exactly one DM per episode, got %d", dms)
	}

	// Still held just before the cooldown elapses.
	now = now.Add(hedgeEntryHoldCooldown - time.Minute)
	if !hedgeEntryHoldActive(sc) {
		t.Fatal("the hold must persist for the full cooldown window")
	}

	// Lifts on its own — no restart required.
	now = now.Add(2 * time.Minute)
	if hedgeEntryHoldActive(sc) {
		t.Fatal("the hold must lift automatically once the cooldown elapses")
	}
	if globalHedgeFailures.count("eth") != 0 {
		t.Errorf("expiry must grant a fresh retry budget, got count=%d", globalHedgeFailures.count("eth"))
	}
	// A fresh episode re-alerts rather than staying silent.
	dms = 0
	for i := 0; i < hedgeOpenFailureHoldThreshold; i++ {
		if _, first := globalHedgeFailures.recordFailure("eth"); first {
			dms++
		}
	}
	if dms != 1 || !hedgeEntryHoldActive(sc) {
		t.Errorf("a new episode must re-engage and re-alert (dms=%d, held=%v)", dms, hedgeEntryHoldActive(sc))
	}
}

// Case (c): a transient failure followed by a healthy venue recovers at once,
// without waiting out the cooldown.
func TestHedgeSuccessDuringHoldClearsItImmediately(t *testing.T) {
	resetHedgeFailures(t)
	now := time.Now()
	globalHedgeFailures.now = func() time.Time { return now }
	for i := 0; i < hedgeOpenFailureHoldThreshold; i++ {
		globalHedgeFailures.recordFailure("eth")
	}
	sc := hedgeTestConfig("live")
	if !hedgeEntryHoldActive(sc) {
		t.Fatal("precondition: hold engaged")
	}
	// The hedge sync itself is NOT gated by the entry hold, so an open primary
	// still gets a hedge attempt — and a success clears the hold on the spot.
	stubHedgeExecutors(t, okExecute(0.1, 60000, 7), nil, nil)
	var mu sync.RWMutex
	s := hedgeTestState(2, "long")
	runHedgeSync(sc, s, &mu, hedgeTestPrices(), nil, nil)
	if hedgeEntryHoldActive(sc) {
		t.Fatal("a successful hedge open must clear the hold immediately, not wait out the cooldown")
	}
}

// The operator message must describe the recovery path that actually exists.
func TestHedgeEntryHoldMessageStatesTheRealRecoveryPath(t *testing.T) {
	msg := hedgeEntryHoldMessage("eth", 3, "BTC")
	if !strings.Contains(msg, hedgeEntryHoldCooldown.String()) {
		t.Errorf("the DM must state the hold duration, got %q", msg)
	}
	if !strings.Contains(msg, "lifts automatically") {
		t.Errorf("the DM must state that recovery is automatic, got %q", msg)
	}
	if strings.Contains(msg, "held until a hedge opens successfully") {
		t.Error("the DM must not promise a clear condition the hold itself blocks")
	}
}
