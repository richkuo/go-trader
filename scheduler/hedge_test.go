package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ── fixtures ──────────────────────────────────────────────────────────────

func hedgeTestStrategy(mutate func(sc *StrategyConfig)) StrategyConfig {
	sc := StrategyConfig{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "check_hyperliquid.py",
		Args:      []string{"--mode=live", "ETH", "1h"},
		Capital:   10000,
		Leverage:  3,
		Direction: DirectionLong,
		Hedge: &HedgeConfig{
			Enabled: true, Symbol: "BTC/USDC:USDC", Side: HedgeSideInverse, Ratio: 1.0,
			MarginMode: "cross", Leverage: 3,
		},
	}
	if mutate != nil {
		mutate(&sc)
	}
	return sc
}

func hedgeTestState(positions map[string]*Position) *StrategyState {
	if positions == nil {
		positions = map[string]*Position{}
	}
	return &StrategyState{
		ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
		Cash: 10000, InitialCapital: 10000,
		Positions: positions, OptionPositions: map[string]*OptionPosition{},
	}
}

func primaryPos(qty float64, side string) *Position {
	return &Position{Symbol: "ETH", Quantity: qty, InitialQuantity: qty, AvgCost: 3000, Side: side, Multiplier: 1, OwnerStrategyID: "hl-eth"}
}

func hedgePos(qty float64, side string, basis float64) *Position {
	return &Position{Symbol: "BTC", Quantity: qty, InitialQuantity: qty, AvgCost: 60000, Side: side, Multiplier: 1,
		OwnerStrategyID: "hl-eth", HedgeFor: "ETH", HedgePrimaryQtyBasis: basis}
}

// stubHedgeExecutor records calls and returns scripted outcomes.
type stubHedgeExecutor struct {
	openCalls    []string
	closeCalls   []string
	primaryCalls []string

	openErr    error
	openFill   *HyperliquidFill
	closeErr   error
	closeFill  *HyperliquidCloseFill
	primaryErr error
	// primaryFill nil + primaryErr nil ⇒ the unwind close reports no fill.
	primaryFill *HyperliquidCloseFill
}

func (s *stubHedgeExecutor) OpenHedge(sc StrategyConfig, coin, orderSide string, qty float64, applyMargin bool) (*HyperliquidExecuteResult, error) {
	s.openCalls = append(s.openCalls, fmt.Sprintf("%s/%s/%.6f/margin=%t", coin, orderSide, qty, applyMargin))
	if s.openErr != nil {
		return nil, s.openErr
	}
	f := s.openFill
	if f == nil {
		f = &HyperliquidFill{AvgPx: 60000, TotalSz: qty, OID: 111, Fee: 1.5}
	}
	return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Action: orderSide, Symbol: coin, Size: qty, Fill: f}}, nil
}

func (s *stubHedgeExecutor) CloseHedge(sc StrategyConfig, coin string, qty float64) (*HyperliquidCloseResult, error) {
	s.closeCalls = append(s.closeCalls, fmt.Sprintf("%s/%.6f", coin, qty))
	if s.closeErr != nil {
		return nil, s.closeErr
	}
	f := s.closeFill
	if f == nil {
		f = &HyperliquidCloseFill{AvgPx: 61000, TotalSz: qty, OID: 222, Fee: 1.2}
	}
	return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: coin, Fill: f}}, nil
}

func (s *stubHedgeExecutor) ClosePrimary(sc StrategyConfig, symbol string, qty float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
	s.primaryCalls = append(s.primaryCalls, fmt.Sprintf("%s/%.6f/oids=%v", symbol, qty, cancelOIDs))
	if s.primaryErr != nil {
		return nil, s.primaryErr
	}
	f := s.primaryFill
	if f == nil {
		f = &HyperliquidCloseFill{AvgPx: 3010, TotalSz: qty, OID: 333, Fee: 0.9}
	}
	return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: symbol, Fill: f}}, nil
}

// ── decision core ─────────────────────────────────────────────────────────

// Acceptance 1 + 6: inverse-side mapping and notional sizing.
func TestHedgeTargetDecision_OpenInverseSizing(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	// Long 2 ETH @ $3000 = $6000 notional; ratio 1.0; BTC @ $60000 ⇒ 0.1 BTC short.
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 2, PrimarySide: "long", HedgeCoin: "BTC"}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionOpen {
		t.Fatalf("want open, got %s (%s)", got.Kind, got.Reason)
	}
	if got.Side != "short" {
		t.Fatalf("long primary must map to a SHORT hedge, got %q", got.Side)
	}
	if diff := got.Qty - 0.1; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("want qty 0.1, got %.9f", got.Qty)
	}
	if got.NewBasis != 2 {
		t.Fatalf("want basis 2, got %v", got.NewBasis)
	}

	// Inverse case: a SHORT primary must map to a LONG hedge.
	snap.PrimarySide = "short"
	if got := hedgeTargetDecision(sc, snap, 3000, 60000); got.Side != "long" {
		t.Fatalf("short primary must map to a LONG hedge, got %q", got.Side)
	}
}

func TestHedgeTargetDecision_RatioScalesNotional(t *testing.T) {
	sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Ratio = 0.5 })
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 2, PrimarySide: "long", HedgeCoin: "BTC"}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if diff := got.Qty - 0.05; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("ratio 0.5 must halve the hedge: want 0.05, got %.9f", got.Qty)
	}
}

// Acceptance 2: partial close reduces the hedge proportionally; full close
// closes it; growth adds. Also covers the do-nothing steady state, which is
// what stops mark drift from re-trading the hedge.
func TestHedgeTargetDecision_QuantityEvents(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	base := hedgeSnapshot{PrimarySymbol: "ETH", PrimarySide: "long", HedgeCoin: "BTC", HedgeSide: "short"}

	t.Run("steady state does nothing even when marks moved", func(t *testing.T) {
		snap := base
		snap.PrimaryQty, snap.HedgeQty, snap.HedgeBasis = 2, 0.1, 2
		// Marks far from the open — a price-mirroring design would re-trade here.
		if got := hedgeTargetDecision(sc, snap, 4500, 90000); got.Kind != hedgeActionNone {
			t.Fatalf("want none, got %s qty=%v (%s)", got.Kind, got.Qty, got.Reason)
		}
	})

	t.Run("primary halved reduces the hedge by half", func(t *testing.T) {
		snap := base
		snap.PrimaryQty, snap.HedgeQty, snap.HedgeBasis = 1, 0.1, 2
		got := hedgeTargetDecision(sc, snap, 3000, 60000)
		if got.Kind != hedgeActionReduce {
			t.Fatalf("want reduce, got %s (%s)", got.Kind, got.Reason)
		}
		if diff := got.Qty - 0.05; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("want reduce 0.05, got %.9f", got.Qty)
		}
		if got.NewBasis != 1 {
			t.Fatalf("want new basis 1, got %v", got.NewBasis)
		}
	})

	t.Run("scale-in add grows the hedge by the delta notional", func(t *testing.T) {
		snap := base
		snap.PrimaryQty, snap.HedgeQty, snap.HedgeBasis = 3, 0.1, 2
		got := hedgeTargetDecision(sc, snap, 3000, 60000)
		if got.Kind != hedgeActionAdd {
			t.Fatalf("want add, got %s (%s)", got.Kind, got.Reason)
		}
		// delta 1 ETH × $3000 / $60000 = 0.05 BTC.
		if diff := got.Qty - 0.05; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("want add 0.05, got %.9f", got.Qty)
		}
	})

	t.Run("primary flat closes the hedge in full", func(t *testing.T) {
		snap := base
		snap.PrimaryQty, snap.HedgeQty, snap.HedgeBasis = 0, 0.1, 2
		got := hedgeTargetDecision(sc, snap, 3000, 60000)
		if got.Kind != hedgeActionCloseFull || got.Qty != 0.1 {
			t.Fatalf("want closeFull 0.1, got %s %v (%s)", got.Kind, got.Qty, got.Reason)
		}
	})

	t.Run("primary flat with no marks still closes the hedge", func(t *testing.T) {
		// Closing must never be blocked on a price: the mark is only needed to
		// SIZE an order, and a full close is sized from the position itself.
		snap := base
		snap.PrimaryQty, snap.HedgeQty, snap.HedgeBasis = 0, 0.1, 2
		got := hedgeTargetDecision(sc, snap, 0, 0)
		if got.Kind != hedgeActionCloseFull || got.Blocked {
			t.Fatalf("want unblocked closeFull, got %s blocked=%t (%s)", got.Kind, got.Blocked, got.Reason)
		}
	})

	t.Run("both flat does nothing", func(t *testing.T) {
		if got := hedgeTargetDecision(sc, base, 3000, 60000); got.Kind != hedgeActionNone || got.Blocked {
			t.Fatalf("want unblocked none, got %s blocked=%t", got.Kind, got.Blocked)
		}
	})
}

func TestHedgeTargetDecision_DustReduceDefersWithoutAdvancingBasis(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	// Primary shrank 2 → 1.999: reduce notional ≈ $3, under the $10 minimum.
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 1.999, PrimarySide: "long", HedgeCoin: "BTC",
		HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 2}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionNone {
		t.Fatalf("dust reduce must defer, got %s qty=%v", got.Kind, got.Qty)
	}
	if got.NewBasis != 0 || got.AdoptBasis != 0 {
		t.Fatalf("a deferred reduce must NOT advance the basis (else the shortfall is lost): %+v", got)
	}
	if !strings.Contains(got.Reason, "minimum") {
		t.Fatalf("reason should explain the deferral, got %q", got.Reason)
	}
}

func TestHedgeTargetDecision_FailsClosedOnUnusableInputs(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	open := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 2, PrimarySide: "long", HedgeCoin: "BTC"}

	for _, tc := range []struct {
		name               string
		primaryPx, hedgePx float64
		mutate             func(s *hedgeSnapshot)
	}{
		{"no hedge mark", 3000, 0, nil},
		{"no primary mark", 0, 60000, nil},
		{"unmappable primary side", 3000, 60000, func(s *hedgeSnapshot) { s.PrimarySide = "flat" }},
		{"unresolved hedge coin", 3000, 60000, func(s *hedgeSnapshot) { s.HedgeCoin = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := open
			if tc.mutate != nil {
				tc.mutate(&snap)
			}
			got := hedgeTargetDecision(sc, snap, tc.primaryPx, tc.hedgePx)
			if !got.Blocked || got.Kind != hedgeActionNone {
				t.Fatalf("want blocked none, got %s blocked=%t (%s)", got.Kind, got.Blocked, got.Reason)
			}
		})
	}
}

func TestHedgeTargetDecision_WrongSideHedgeIsUnwound(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	// A LONG hedge behind a LONG primary doubles beta — worse than no hedge.
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 2, PrimarySide: "long", HedgeCoin: "BTC",
		HedgeQty: 0.1, HedgeSide: "long", HedgeBasis: 2}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionCloseFull {
		t.Fatalf("want closeFull, got %s (%s)", got.Kind, got.Reason)
	}
}

func TestHedgeTargetDecision_MissingBasisAdoptsWithoutTrading(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 2, PrimarySide: "long", HedgeCoin: "BTC",
		HedgeQty: 0.1, HedgeSide: "short", HedgeBasis: 0}
	got := hedgeTargetDecision(sc, snap, 3000, 60000)
	if got.Kind != hedgeActionNone || got.AdoptBasis != 2 {
		t.Fatalf("want none with AdoptBasis=2, got %s adopt=%v", got.Kind, got.AdoptBasis)
	}
}

func TestHedgeTargetDecision_DisabledHedgeIsInert(t *testing.T) {
	sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Enabled = false })
	snap := hedgeSnapshot{PrimarySymbol: "ETH", PrimaryQty: 2, PrimarySide: "long"}
	if got := hedgeTargetDecision(sc, snap, 3000, 60000); got.Kind != hedgeActionNone || got.Blocked {
		t.Fatalf("a disabled hedge must be inert, got %s blocked=%t", got.Kind, got.Blocked)
	}
}

// ── skip-reason mirror ────────────────────────────────────────────────────

func TestHedgeOrderSkipReason(t *testing.T) {
	action := hedgeAction{Kind: hedgeActionOpen, Qty: 0.1, Side: "short", NewBasis: 2}
	ok := hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeCoin: "BTC"}
	if why := hedgeOrderSkipReason(action, ok); why != "" {
		t.Fatalf("unchanged state must not skip, got %q", why)
	}
	for _, tc := range []struct {
		name string
		snap hedgeSnapshot
	}{
		{"primary went flat", hedgeSnapshot{PrimaryQty: 0, HedgeCoin: "BTC"}},
		{"primary flipped", hedgeSnapshot{PrimaryQty: 2, PrimarySide: "short", HedgeCoin: "BTC"}},
		{"hedge already exists", hedgeSnapshot{PrimaryQty: 2, PrimarySide: "long", HedgeQty: 0.1, HedgeCoin: "BTC"}},
	} {
		if why := hedgeOrderSkipReason(action, tc.snap); why == "" {
			t.Fatalf("%s: must skip the spawn", tc.name)
		}
	}
	closeAction := hedgeAction{Kind: hedgeActionCloseFull, Qty: 0.1}
	if why := hedgeOrderSkipReason(closeAction, hedgeSnapshot{HedgeQty: 0}); why == "" {
		t.Fatal("closing an already-flat hedge must skip")
	}
}

// ── orchestration ─────────────────────────────────────────────────────────

// Acceptance 1 + 6 (ledger attribution): a fresh primary open produces a hedge
// leg owned by, and booked to, the primary's strategy.
func TestRunHedgeSync_OpensAndAttributesToOwner(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := hedgeTestState(map[string]*Position{"ETH": primaryPos(2, "long")})
	var mu sync.RWMutex
	exec := &stubHedgeExecutor{}

	runHedgeSync(sc, s, &mu, hedgeSyncInputs{PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: 60000, PrimaryOpenedFromFlat: true, Live: true}, exec, nil, nil)

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge leg was not created")
	}
	if !pos.isHedgeLeg() || pos.HedgeFor != "ETH" {
		t.Fatalf("hedge ownership must be stamped on the position: %+v", pos)
	}
	if pos.OwnerStrategyID != "hl-eth" {
		t.Fatalf("hedge must be owned by the primary's strategy, got %q", pos.OwnerStrategyID)
	}
	if pos.Side != "short" || pos.Multiplier != 1 {
		t.Fatalf("want short perps leg, got side=%q multiplier=%v", pos.Side, pos.Multiplier)
	}
	if pos.HedgePrimaryQtyBasis != 2 {
		t.Fatalf("want basis 2, got %v", pos.HedgePrimaryQtyBasis)
	}
	if len(exec.openCalls) != 1 || !strings.Contains(exec.openCalls[0], "BTC/sell/") {
		t.Fatalf("want one BTC sell order, got %v", exec.openCalls)
	}
	if !strings.Contains(exec.openCalls[0], "margin=true") {
		t.Fatalf("a fresh hedge open must carry its own margin/leverage assignment, got %v", exec.openCalls)
	}
	// The hedge's Trade must belong to the owning strategy's ledger.
	if len(s.TradeHistory) != 1 {
		t.Fatalf("want one hedge trade, got %d", len(s.TradeHistory))
	}
	tr := s.TradeHistory[0]
	if tr.StrategyID != "hl-eth" || tr.TradeType != HedgeTradeType || tr.Symbol != "BTC" {
		t.Fatalf("hedge trade must attribute to the owner with the hedge label: %+v", tr)
	}
	if tr.ExchangeFee <= 0 {
		t.Fatal("the hedge's exchange fee must reach the strategy's ledger")
	}
}

// Acceptance 4 (constraint 4): a hedge-open failure on the OPENING cycle
// unwinds the primary reduce-only rather than running unhedged.
func TestRunHedgeSync_FailedHedgeOpenUnwindsPrimary(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	primary := primaryPos(2, "long")
	primary.StopLossOID = 4242
	s := hedgeTestState(map[string]*Position{"ETH": primary})
	var mu sync.RWMutex
	exec := &stubHedgeExecutor{openErr: fmt.Errorf("insufficient margin")}

	runHedgeSync(sc, s, &mu, hedgeSyncInputs{PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: 60000, PrimaryOpenedFromFlat: true, Live: true}, exec, nil, nil)

	if _, still := s.Positions["ETH"]; still {
		t.Fatal("primary must be closed when the hedge cannot be opened (fail-closed)")
	}
	if _, hedged := s.Positions["BTC"]; hedged {
		t.Fatal("no hedge leg may exist after a failed hedge open")
	}
	if len(exec.primaryCalls) != 1 {
		t.Fatalf("want exactly one primary unwind close, got %v", exec.primaryCalls)
	}
	if !strings.Contains(exec.primaryCalls[0], "ETH/2.000000") {
		t.Fatalf("the unwind must be SIZED (shared-coin peers), got %v", exec.primaryCalls)
	}
	if !strings.Contains(exec.primaryCalls[0], "4242") {
		t.Fatalf("the unwind must cancel the just-armed protection OIDs, got %v", exec.primaryCalls)
	}
	var closes int
	for _, tr := range s.TradeHistory {
		if tr.IsClose {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("want exactly one booked close leg, got %d (%+v)", closes, s.TradeHistory)
	}
}

// The inverse of the case above: a hedge failure on a LATER cycle must NOT
// unwind — the primary is already hedged (or converging down), so unwinding
// would liquidate a healthy hedged position on a transient RPC error.
func TestRunHedgeSync_LaterCycleFailureDoesNotUnwind(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := hedgeTestState(map[string]*Position{
		"ETH": primaryPos(3, "long"),
		"BTC": hedgePos(0.1, "short", 2),
	})
	var mu sync.RWMutex
	exec := &stubHedgeExecutor{openErr: fmt.Errorf("rpc timeout")}

	runHedgeSync(sc, s, &mu, hedgeSyncInputs{PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: 60000, PrimaryOpenedFromFlat: false, Live: true}, exec, nil, nil)

	if s.Positions["ETH"] == nil {
		t.Fatal("a later-cycle hedge failure must never unwind the primary")
	}
	if len(exec.primaryCalls) != 0 {
		t.Fatalf("no primary close may be submitted, got %v", exec.primaryCalls)
	}
	if s.Positions["BTC"].Quantity != 0.1 {
		t.Fatal("the existing hedge leg must be left untouched after a failed add")
	}
}

// The scale-in regression (review round 1): a hedge-ADD failure on a cycle that
// also grew the primary must never unwind. The pre-existing size is already
// covered by the held hedge leg, so tearing down the whole position over a
// transient error on the small incremental order both destroys healthy exposure
// and contradicts constraint 4's actual scope. Both the error path and the
// Blocked (unusable marks) path are exercised, and both are asserted even when
// the caller WRONGLY reports the cycle as a flat→open — the hedge-flat clause in
// hedgeFailureMayUnwindPrimary is the defense in depth that must hold.
func TestRunHedgeSync_ScaleInAddFailureNeverUnwindsHedgedPrimary(t *testing.T) {
	sc := hedgeTestStrategy(nil)

	cases := []struct {
		name              string
		hedgePx           float64
		openErr           error
		openedFromFlat    bool
		wantOpenAttempted bool
	}{
		// Production shape after the fix: main.go reports false for a scale-in.
		{"transient add error, correctly flagged as not-from-flat", 60000, fmt.Errorf("rpc timeout"), false, true},
		// Defense in depth: even if the caller mis-reports the cycle, a held
		// hedge leg must make the primary structurally un-unwindable.
		{"transient add error, caller wrongly reports from-flat", 60000, fmt.Errorf("rpc timeout"), true, true},
		// Blocked path: marks momentarily unusable, no order is even attempted.
		{"unusable marks, correctly flagged as not-from-flat", 0, nil, false, false},
		{"unusable marks, caller wrongly reports from-flat", 0, nil, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := hedgeTestState(map[string]*Position{
				"ETH": primaryPos(3, "long"),     // grew 2 → 3 this cycle
				"BTC": hedgePos(0.1, "short", 2), // hedge still sized to the old basis
			})
			var mu sync.RWMutex
			exec := &stubHedgeExecutor{openErr: tc.openErr}

			runHedgeSync(sc, s, &mu, hedgeSyncInputs{
				PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: tc.hedgePx,
				PrimaryOpenedFromFlat: tc.openedFromFlat, Live: true,
			}, exec, nil, nil)

			if s.Positions["ETH"] == nil {
				t.Fatal("an already-hedged primary must NEVER be unwound by a hedge-add failure")
			}
			if s.Positions["ETH"].Quantity != 3 {
				t.Fatalf("the primary must be untouched, got qty %v", s.Positions["ETH"].Quantity)
			}
			if len(exec.primaryCalls) != 0 {
				t.Fatalf("no primary close may be submitted, got %v", exec.primaryCalls)
			}
			if s.Positions["BTC"] == nil || s.Positions["BTC"].Quantity != 0.1 {
				t.Fatalf("the existing hedge leg must be left intact, got %+v", s.Positions["BTC"])
			}
			// The watermark must NOT advance on a failure, so the next cycle
			// retries the same add rather than losing the shortfall.
			if s.Positions["BTC"].HedgePrimaryQtyBasis != 2 {
				t.Fatalf("a failed add must not advance the watermark, got %v", s.Positions["BTC"].HedgePrimaryQtyBasis)
			}
			if got := len(exec.openCalls) > 0; got != tc.wantOpenAttempted {
				t.Fatalf("open attempted = %t, want %t (%v)", got, tc.wantOpenAttempted, exec.openCalls)
			}
		})
	}
}

// The inverse of the case above — the existing fail-closed behavior must NOT
// regress. A genuine flat→open whose hedge cannot be placed still unwinds, on
// both the error path and the Blocked (unusable marks) path.
func TestRunHedgeSync_GenuineFreshOpenStillUnwinds(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	for _, tc := range []struct {
		name    string
		hedgePx float64
		openErr error
	}{
		{"hedge open errored", 60000, fmt.Errorf("insufficient margin")},
		{"marks unusable so the hedge cannot be sized", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := hedgeTestState(map[string]*Position{"ETH": primaryPos(2, "long")})
			var mu sync.RWMutex
			exec := &stubHedgeExecutor{openErr: tc.openErr}

			runHedgeSync(sc, s, &mu, hedgeSyncInputs{
				PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: tc.hedgePx,
				PrimaryOpenedFromFlat: true, Live: true,
			}, exec, nil, nil)

			if _, still := s.Positions["ETH"]; still {
				t.Fatal("a brand-new, still-unhedged primary must be closed when the hedge cannot be placed")
			}
			if len(exec.primaryCalls) != 1 {
				t.Fatalf("want exactly one sized primary unwind, got %v", exec.primaryCalls)
			}
		})
	}
}

// A long-running primary whose hedge was closed EXTERNALLY produces a
// hedgeActionOpen with the hedge flat. That must still not unwind: the
// documented behavior is re-open next cycle + alert, and this cycle never
// opened the primary.
func TestRunHedgeSync_ExternallyClosedHedgeReopenFailureDoesNotUnwind(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := hedgeTestState(map[string]*Position{"ETH": primaryPos(2, "long")})
	var mu sync.RWMutex
	exec := &stubHedgeExecutor{openErr: fmt.Errorf("rpc timeout")}

	runHedgeSync(sc, s, &mu, hedgeSyncInputs{
		PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: 60000,
		PrimaryOpenedFromFlat: false, Live: true,
	}, exec, nil, nil)

	if s.Positions["ETH"] == nil {
		t.Fatal("a long-running primary must not be liquidated over one failed hedge re-open")
	}
	if len(exec.primaryCalls) != 0 {
		t.Fatalf("no primary close may be submitted, got %v", exec.primaryCalls)
	}
}

// The predicate itself, exhaustively: it is the single gate on every path that
// can close primary exposure because of a hedge error.
func TestHedgeFailureMayUnwindPrimary(t *testing.T) {
	cases := []struct {
		name           string
		openedFromFlat bool
		primaryQty     float64
		hedgeQty       float64
		want           bool
	}{
		{"flat->open this cycle, primary held, hedge flat", true, 2, 0, true},
		{"scale-in add: hedge already held", true, 3, 0.1, false},
		{"not opened this cycle, hedge flat", false, 2, 0, false},
		{"not opened this cycle, hedge held", false, 2, 0.1, false},
		{"primary already flat", true, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := hedgeSnapshot{PrimaryQty: tc.primaryQty, HedgeQty: tc.hedgeQty}
			if got := hedgeFailureMayUnwindPrimary(tc.openedFromFlat, snap); got != tc.want {
				t.Fatalf("got %t, want %t", got, tc.want)
			}
		})
	}
}

// Acceptance 6: a failed PRIMARY open places no hedge order at all.
func TestRunHedgeSync_FailedPrimaryOpenPlacesNoHedge(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	// liveExecFailed ⇒ no position was ever created.
	s := hedgeTestState(nil)
	var mu sync.RWMutex
	exec := &stubHedgeExecutor{}

	runHedgeSync(sc, s, &mu, hedgeSyncInputs{PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: 60000, PrimaryOpenedFromFlat: true, Live: true}, exec, nil, nil)

	if len(exec.openCalls) != 0 || len(exec.closeCalls) != 0 || len(exec.primaryCalls) != 0 {
		t.Fatalf("a failed primary open must produce no orders at all: %+v", exec)
	}
	if len(s.Positions) != 0 {
		t.Fatalf("no positions may be created, got %+v", s.Positions)
	}
}

// Acceptance 2: a primary partial close reduces the hedge and never leaves
// untracked residual exposure; the watermark advances so the next cycle is a
// no-op rather than a repeat reduce.
func TestRunHedgeSync_PartialCloseReducesHedge(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := hedgeTestState(map[string]*Position{
		"ETH": primaryPos(1, "long"),
		"BTC": hedgePos(0.1, "short", 2),
	})
	var mu sync.RWMutex
	exec := &stubHedgeExecutor{}
	in := hedgeSyncInputs{PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: 60000, Live: true}

	runHedgeSync(sc, s, &mu, in, exec, nil, nil)

	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge leg must remain open after a partial reduce")
	}
	if diff := pos.Quantity - 0.05; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("want remaining hedge 0.05, got %.9f", pos.Quantity)
	}
	if pos.HedgePrimaryQtyBasis != 1 {
		t.Fatalf("watermark must advance to the new primary qty, got %v", pos.HedgePrimaryQtyBasis)
	}
	// Idempotence: converged state must not trade again.
	before := len(exec.closeCalls)
	runHedgeSync(sc, s, &mu, in, exec, nil, nil)
	if len(exec.closeCalls) != before {
		t.Fatalf("a converged hedge must not re-trade, got %v", exec.closeCalls)
	}
}

func TestRunHedgeSync_PrimaryClosedClosesHedge(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := hedgeTestState(map[string]*Position{"BTC": hedgePos(0.1, "short", 2)})
	var mu sync.RWMutex
	exec := &stubHedgeExecutor{}

	runHedgeSync(sc, s, &mu, hedgeSyncInputs{PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: 60000, Live: true}, exec, nil, nil)

	if _, still := s.Positions["BTC"]; still {
		t.Fatal("hedge must be closed once the primary is flat — no residual exposure")
	}
	if len(exec.closeCalls) != 1 || !strings.Contains(exec.closeCalls[0], "BTC/0.100000") {
		t.Fatalf("want one full-size BTC close, got %v", exec.closeCalls)
	}
}

// Acceptance 6 (ledger attribution): a hedge round-trip's PnL and fees land in
// the owning strategy's cash/ledger, and the hedge's LOSS never advances the
// circuit breaker's loss-streak counter.
func TestRunHedgeSync_HedgeLossDoesNotAdvanceLossStreak(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := hedgeTestState(map[string]*Position{"BTC": hedgePos(0.1, "short", 2)})
	cashBefore := s.Cash
	var mu sync.RWMutex
	// Short hedge closed ABOVE its avg cost ⇒ a loss.
	exec := &stubHedgeExecutor{closeFill: &HyperliquidCloseFill{AvgPx: 61000, TotalSz: 0.1, OID: 9, Fee: 2}}

	runHedgeSync(sc, s, &mu, hedgeSyncInputs{PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: 61000, Live: true}, exec, nil, nil)

	if s.RiskState.ConsecutiveLosses != 0 {
		t.Fatalf("a hedge loss must not advance the loss streak (it loses whenever the primary wins), got %d", s.RiskState.ConsecutiveLosses)
	}
	if s.RiskState.DailyPnL >= 0 {
		t.Fatalf("a hedge loss IS real money and must reach DailyPnL, got %v", s.RiskState.DailyPnL)
	}
	if s.Cash >= cashBefore {
		t.Fatalf("hedge PnL + fee must book to the owning strategy's cash: %v → %v", cashBefore, s.Cash)
	}
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].TradeType != HedgeTradeType || !s.TradeHistory[0].IsClose {
		t.Fatalf("want one hedge close leg on the owner's ledger, got %+v", s.TradeHistory)
	}
	// Contrast: an ordinary perps loss DOES advance the streak.
	s2 := hedgeTestState(map[string]*Position{"ETH": primaryPos(1, "long")})
	bookPerpsCloseWithFillFee(s2, "ETH", 2900, 1, true, "1", "close", "Close", "close", nil)
	if s2.RiskState.ConsecutiveLosses != 1 {
		t.Fatalf("control: an ordinary perps loss must advance the streak, got %d", s2.RiskState.ConsecutiveLosses)
	}
}

func TestRunHedgeSync_PaperModeIsVirtualOnly(t *testing.T) {
	sc := hedgeTestStrategy(func(sc *StrategyConfig) { sc.Args = []string{"--mode=paper", "ETH", "1h"} })
	s := hedgeTestState(map[string]*Position{"ETH": primaryPos(2, "long")})
	var mu sync.RWMutex
	exec := &stubHedgeExecutor{}

	runHedgeSync(sc, s, &mu, hedgeSyncInputs{PrimarySymbol: "ETH", PrimaryPx: 3000, HedgePx: 60000, PrimaryOpenedFromFlat: true, Live: false}, exec, nil, nil)

	if len(exec.openCalls) != 0 {
		t.Fatalf("paper mode must place no on-chain order, got %v", exec.openCalls)
	}
	pos := s.Positions["BTC"]
	if pos == nil || pos.Side != "short" {
		t.Fatalf("paper hedge leg must still be tracked virtually, got %+v", pos)
	}
	if s.TradeHistory[0].FeeSource != FeeSourceModeled {
		t.Fatalf("paper fills must be modeled, got %q", s.TradeHistory[0].FeeSource)
	}
}

// ── config validation (acceptance 5) ──────────────────────────────────────

func hedgeValidationConfig(strategies ...StrategyConfig) *Config {
	return &Config{Strategies: strategies}
}

func TestValidateHedgeConfigs_CollisionMatrix(t *testing.T) {
	eth := hedgeTestStrategy(nil)

	cases := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name: "clean config passes",
			cfg:  hedgeValidationConfig(eth),
		},
		{
			name: "hedge on the strategy's own coin",
			cfg: hedgeValidationConfig(hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.Hedge.Symbol = "ETH"
			})),
			wantErr: "own coin",
		},
		{
			name: "hedge collides with another strategy's configured coin",
			cfg: hedgeValidationConfig(eth, StrategyConfig{
				ID: "hl-btc", Type: "perps", Platform: "hyperliquid", Script: "x.py",
				Args: []string{"--mode=live", "BTC", "1h"},
			}),
			wantErr: "configured coin of strategy",
		},
		{
			name: "hedge collides with a PAPER peer's coin",
			cfg: hedgeValidationConfig(eth, StrategyConfig{
				ID: "hl-btc-paper", Type: "perps", Platform: "hyperliquid", Script: "x.py",
				Args: []string{"--mode=paper", "BTC", "1h"},
			}),
			wantErr: "configured coin of strategy",
		},
		{
			name: "hedge collides with a MANUAL peer's coin",
			cfg: hedgeValidationConfig(eth, StrategyConfig{
				ID: "hl-manual-btc", Type: "manual", Platform: "hyperliquid", Script: "x.py",
				Symbol: "BTC", Args: []string{"--mode=live", "BTC", "1h"},
			}),
			wantErr: "configured coin of strategy",
		},
		{
			name: "two strategies claim the same hedge coin",
			cfg: hedgeValidationConfig(eth, hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.ID = "hl-sol"
				sc.Args = []string{"--mode=live", "SOL", "1h"}
			})),
			wantErr: "claimed by multiple strategies",
		},
		{
			name: "direction both is rejected",
			cfg: hedgeValidationConfig(hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.Direction = DirectionBoth
			})),
			wantErr: "not supported with direction",
		},
		{
			name: "non-perps type is rejected",
			cfg: hedgeValidationConfig(hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.Type = "manual"
				sc.Symbol = "ETH"
			})),
			wantErr: "only supported for perps",
		},
		{
			name: "non-hyperliquid platform is rejected",
			cfg: hedgeValidationConfig(hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.Platform = "okx"
			})),
			wantErr: "only supported on hyperliquid",
		},
		{
			name: "unsupported side is rejected",
			cfg: hedgeValidationConfig(hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.Hedge.Side = "same"
			})),
			wantErr: "hedge.side must be",
		},
		{
			name: "out-of-bounds ratio is rejected",
			cfg: hedgeValidationConfig(hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.Hedge.Ratio = 25
			})),
			wantErr: "hedge.ratio must be",
		},
		{
			name: "bad margin mode is rejected",
			cfg: hedgeValidationConfig(hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.Hedge.MarginMode = "portfolio"
			})),
			wantErr: "hedge.margin_mode must be",
		},
		{
			name: "empty symbol is rejected",
			cfg: hedgeValidationConfig(hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.Hedge.Symbol = ""
			})),
			wantErr: "hedge.symbol is required",
		},
		{
			name: "a DISABLED block is still shape-validated",
			cfg: hedgeValidationConfig(hedgeTestStrategy(func(sc *StrategyConfig) {
				sc.Hedge.Enabled = false
				sc.Hedge.Side = "same"
			})),
			wantErr: "hedge.side must be",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateHedgeConfigs(tc.cfg)
			joined := strings.Join(errs, "\n")
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("want no errors, got:\n%s", joined)
				}
				return
			}
			if !strings.Contains(joined, tc.wantErr) {
				t.Fatalf("want an error containing %q, got:\n%s", tc.wantErr, joined)
			}
		})
	}
}

func TestNormalizeHedgeCoin(t *testing.T) {
	for in, want := range map[string]string{
		"BTC": "BTC", "btc": "BTC", " eth ": "ETH",
		"BTC/USDC:USDC": "BTC", "SOL/USDT": "SOL", "": "",
	} {
		if got := normalizeHedgeCoin(in); got != want {
			t.Fatalf("normalizeHedgeCoin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHedgeAccessorDefaults(t *testing.T) {
	sc := hedgeTestStrategy(func(sc *StrategyConfig) {
		sc.Hedge = &HedgeConfig{Enabled: true, Symbol: "BTC"}
	})
	if HedgeRatio(sc) != DefaultHedgeRatio {
		t.Fatalf("ratio default: got %v", HedgeRatio(sc))
	}
	if hedgeLeverage(sc) != DefaultHedgeLeverage {
		t.Fatalf("leverage default: got %v", hedgeLeverage(sc))
	}
	if hedgeMarginMode(sc) != "isolated" {
		t.Fatalf("margin mode default: got %q", hedgeMarginMode(sc))
	}
	if hedgeSide(sc) != HedgeSideInverse {
		t.Fatalf("side default: got %q", hedgeSide(sc))
	}
	// A nil block must read as fully disabled.
	sc.Hedge = nil
	if HedgeEnabled(sc) || hedgeCoin(sc) != "" {
		t.Fatal("a nil hedge block must be inert")
	}
}

// The nested unknown-key guard: a typo'd ratio must fail loudly rather than
// silently defaulting to 1.0 and doubling the intended hedge.
func TestValidateStrategyJSONKeys_RejectsUnknownHedgeKey(t *testing.T) {
	raw := []byte(`{"strategies":[{"id":"hl-eth","hedge":{"enabled":true,"symbol":"BTC","ration":0.5}}]}`)
	errs := validateStrategyJSONKeys(raw)
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, `hedge: unknown field "ration"`) {
		t.Fatalf("want a nested hedge key error, got:\n%s", joined)
	}
	// Control: the correct spelling must pass.
	ok := []byte(`{"strategies":[{"id":"hl-eth","hedge":{"enabled":true,"symbol":"BTC","ratio":0.5}}]}`)
	if errs := validateStrategyJSONKeys(ok); len(errs) != 0 {
		t.Fatalf("valid hedge keys must pass, got %v", errs)
	}
}

// ── hot reload (constraint 7 / acceptance 6) ──────────────────────────────

func TestValidateHotReloadStateCompatible_HedgeBlockedWhileOpen(t *testing.T) {
	mk := func(mutate func(sc *StrategyConfig)) *Config {
		return minimalReloadConfig([]StrategyConfig{hedgeTestStrategy(mutate)})
	}
	// strategyHasOpenPositions covers a residual HEDGE leg with the primary
	// already flat, because the hedge lives in the same Positions map.
	hedgeOnlyState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{"BTC": hedgePos(0.1, "short", 2)}},
	}}
	primaryOpenState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{"ETH": primaryPos(2, "long")}},
	}}
	flatState := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{}},
	}}

	changes := map[string]func(sc *StrategyConfig){
		"disable":     func(sc *StrategyConfig) { sc.Hedge.Enabled = false },
		"remove":      func(sc *StrategyConfig) { sc.Hedge = nil },
		"symbol":      func(sc *StrategyConfig) { sc.Hedge.Symbol = "SOL" },
		"ratio":       func(sc *StrategyConfig) { sc.Hedge.Ratio = 0.5 },
		"leverage":    func(sc *StrategyConfig) { sc.Hedge.Leverage = 5 },
		"margin_mode": func(sc *StrategyConfig) { sc.Hedge.MarginMode = "isolated" },
	}
	for name, mutate := range changes {
		t.Run(name, func(t *testing.T) {
			for label, state := range map[string]*AppState{"hedge leg open": hedgeOnlyState, "primary open": primaryOpenState} {
				err := validateHotReloadStateCompatible(mk(nil), mk(mutate), state)
				if err == nil || !strings.Contains(err.Error(), "hedge block changed with open positions") {
					t.Fatalf("%s: want a hedge reload block, got: %v", label, err)
				}
			}
			if err := validateHotReloadStateCompatible(mk(nil), mk(mutate), flatState); err != nil {
				t.Fatalf("flat: the same edit must be accepted, got: %v", err)
			}
		})
	}

	// An unchanged hedge block must never be flagged.
	if err := validateHotReloadStateCompatible(mk(nil), mk(nil), hedgeOnlyState); err != nil {
		t.Fatalf("unchanged hedge block must pass, got: %v", err)
	}
	// ...and must not be treated as "restart required" either.
	if err := validateHotReloadCompatible(mk(nil), mk(func(sc *StrategyConfig) { sc.Hedge.Ratio = 0.5 })); err != nil {
		t.Fatalf("a hedge edit must be hot-reloadable (not restart-required), got: %v", err)
	}
}

// ── startup recovery (acceptance 3) ───────────────────────────────────────

func TestValidateHedgeStateConsistency(t *testing.T) {
	withPos := func(pos map[string]*Position) *AppState {
		return &AppState{Strategies: map[string]*StrategyState{"hl-eth": {ID: "hl-eth", Positions: pos}}}
	}

	t.Run("matching config is silent", func(t *testing.T) {
		cfg := &Config{Strategies: []StrategyConfig{hedgeTestStrategy(nil)}}
		if w := validateHedgeStateConsistency(withPos(map[string]*Position{"BTC": hedgePos(0.1, "short", 2)}), cfg); len(w) != 0 {
			t.Fatalf("want no warnings, got %v", w)
		}
	})

	t.Run("hedge disabled by a config edit + restart warns and freezes", func(t *testing.T) {
		cfg := &Config{Strategies: []StrategyConfig{hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge = nil })}}
		state := withPos(map[string]*Position{"BTC": hedgePos(0.1, "short", 2)})
		w := validateHedgeStateConsistency(state, cfg)
		if len(w) != 1 || !strings.Contains(w[0], "FROZEN") {
			t.Fatalf("want one frozen-leg warning, got %v", w)
		}
		if state.Strategies["hl-eth"].Positions["BTC"] == nil {
			t.Fatal("the check must be NON-destructive — a config warning must never close live exposure")
		}
	})

	t.Run("hedge coin changed warns about the stale leg", func(t *testing.T) {
		cfg := &Config{Strategies: []StrategyConfig{hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Symbol = "SOL" })}}
		w := validateHedgeStateConsistency(withPos(map[string]*Position{"BTC": hedgePos(0.1, "short", 2)}), cfg)
		if len(w) != 1 || !strings.Contains(w[0], "SOL") {
			t.Fatalf("want a stale-coin warning naming the new coin, got %v", w)
		}
	})
}

// An inverse hedge leg sits opposite the configured direction by design and
// must not trip the perps direction-vs-config startup validator.
func TestValidatePerpsDirectionConfig_SkipsHedgeLegs(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{hedgeTestStrategy(nil)}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {ID: "hl-eth", Positions: map[string]*Position{
			"BTC": hedgePos(0.1, "short", 2), // short under direction="long"
		}},
	}}
	if w := ValidatePerpsDirectionConfig(state, cfg); len(w) != 0 {
		t.Fatalf("a hedge leg must not warn, got %v", w)
	}
	// Control: a non-hedge short under direction="long" still warns.
	state.Strategies["hl-eth"].Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1}
	if w := ValidatePerpsDirectionConfig(state, cfg); len(w) == 0 {
		t.Fatal("control: an ordinary conflicting short must still warn")
	}
}

// ── kill switch / circuit breaker (acceptance 4) ──────────────────────────

func TestForceCloseHyperliquidLive_ClosesHeldHedgeCoins(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	positions := []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -0.1}, {Coin: "DOGE", Size: 500}}
	var closed []string
	closer := func(coin string, sz *float64, oids []int64) (*HyperliquidCloseResult, error) {
		closed = append(closed, coin)
		return &HyperliquidCloseResult{Close: &HyperliquidClose{Symbol: coin, Fill: &HyperliquidCloseFill{AvgPx: 1, TotalSz: 1}}}, nil
	}

	// Without the held-hedge set, the hedge coin is unowned and skipped.
	rep := forceCloseHyperliquidLive(t.Context(), positions, []StrategyConfig{sc}, closer, nil, nil)
	if len(rep.ClosedCoins) != 1 || rep.ClosedCoins[0] != "ETH" {
		t.Fatalf("baseline: want only ETH closed, got %v", rep.ClosedCoins)
	}

	closed = nil
	rep = forceCloseHyperliquidLive(t.Context(), positions, []StrategyConfig{sc}, closer, nil, map[string]bool{"BTC": true})
	if len(rep.ClosedCoins) != 2 {
		t.Fatalf("want ETH and the held hedge coin closed, got %v", rep.ClosedCoins)
	}
	for _, want := range []string{"ETH", "BTC"} {
		found := false
		for _, c := range rep.ClosedCoins {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("want %s closed, got %v", want, rep.ClosedCoins)
		}
	}
	// A genuinely foreign coin is never touched.
	for _, c := range closed {
		if c == "DOGE" {
			t.Fatal("the kill switch must never liquidate an unowned coin")
		}
	}
}

func TestStrategyHeldHedgeCoin(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	if got := strategyHeldHedgeCoin(sc, hedgeTestState(map[string]*Position{"BTC": hedgePos(0.1, "short", 2)})); got != "BTC" {
		t.Fatalf("want BTC, got %q", got)
	}
	// Declared but flat ⇒ empty, so the kill switch never liquidates a foreign
	// position sitting on a declared-but-unused hedge coin.
	if got := strategyHeldHedgeCoin(sc, hedgeTestState(nil)); got != "" {
		t.Fatalf("a flat hedge coin must not be reported as held, got %q", got)
	}
	// A same-coin position that is NOT stamped as a hedge is not ours.
	notHedge := &Position{Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "short", Multiplier: 1}
	if got := strategyHeldHedgeCoin(sc, hedgeTestState(map[string]*Position{"BTC": notHedge})); got != "" {
		t.Fatalf("ownership comes from the hedge_for stamp, not the coin, got %q", got)
	}
}

func TestSetHyperliquidCircuitBreakerPending_IncludesHedgeLeg(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	s := hedgeTestState(map[string]*Position{
		"ETH": primaryPos(2, "long"),
		"BTC": hedgePos(0.1, "short", 2),
	})
	assist := &PlatformRiskAssist{
		HLPositions: []HLPosition{{Coin: "ETH", Size: 2}, {Coin: "BTC", Size: -0.1}},
		HLLiveAll:   []StrategyConfig{sc},
	}
	setHyperliquidCircuitBreakerPending(&sc, s, assist)

	pending := s.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if pending == nil {
		t.Fatal("no pending circuit close queued")
	}
	if len(pending.Symbols) != 2 {
		t.Fatalf("the hedge leg must be queued alongside the primary (else forceCloseAllPositions clears it virtually while it stays on-chain), got %+v", pending.Symbols)
	}
	byCoin := map[string]float64{}
	for _, c := range pending.Symbols {
		byCoin[c.Symbol] = c.Size
	}
	if byCoin["ETH"] != 2 || byCoin["BTC"] != 0.1 {
		t.Fatalf("wrong close sizes: %+v", byCoin)
	}

	// Inverse: a declared-but-flat hedge queues only the primary.
	s2 := hedgeTestState(map[string]*Position{"ETH": primaryPos(2, "long")})
	setHyperliquidCircuitBreakerPending(&sc, s2, assist)
	p2 := s2.RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p2 == nil || len(p2.Symbols) != 1 || p2.Symbols[0].Symbol != "ETH" {
		t.Fatalf("a flat hedge must not be queued for close, got %+v", p2)
	}
}

// ── marks & shared-wallet attribution (requirement 6) ─────────────────────

func TestCollectPerpsMarkSymbols_IncludesHedgeCoins(t *testing.T) {
	hl, _ := collectPerpsMarkSymbols([]StrategyConfig{hedgeTestStrategy(nil)})
	want := map[string]bool{"ETH": true, "BTC": true}
	if len(hl) != 2 {
		t.Fatalf("want ETH and the hedge coin BTC, got %v", hl)
	}
	for _, c := range hl {
		if !want[c] {
			t.Fatalf("unexpected coin %q in %v", c, hl)
		}
	}
	// A disabled hedge contributes nothing.
	hl, _ = collectPerpsMarkSymbols([]StrategyConfig{hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge.Enabled = false })})
	if len(hl) != 1 || hl[0] != "ETH" {
		t.Fatalf("a disabled hedge must add no mark symbol, got %v", hl)
	}
}

func TestBuildSharedWalletBooks_IncludesHeldHedgeLeg(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": hedgeTestState(map[string]*Position{
			"ETH": primaryPos(2, "long"),
			"BTC": hedgePos(0.1, "short", 2),
		}),
	}}
	_, virtualQty := buildSharedWalletBooks(
		SharedWalletKey{Platform: "hyperliquid"},
		[]string{"hl-eth"},
		map[string]StrategyConfig{"hl-eth": sc},
		state,
	)
	if virtualQty["ETH"]["hl-eth"] != 2 {
		t.Fatalf("primary leg missing from the wallet books: %+v", virtualQty)
	}
	// Without this the hedge coin is classified as an ORPHAN coin, producing
	// phantom drift alerts and losing funding attribution (#1159 req 6).
	if virtualQty["BTC"]["hl-eth"] != 0.1 {
		t.Fatalf("held hedge leg missing from the wallet books: %+v", virtualQty)
	}
}

// ── operator surfaces (requirement 7) ─────────────────────────────────────

func TestBuildHedgeStatus(t *testing.T) {
	sc := hedgeTestStrategy(nil)
	if buildHedgeStatus(hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge = nil }), nil) != nil {
		t.Fatal("no hedge block must render no status")
	}
	st := buildHedgeStatus(sc, hedgeTestState(map[string]*Position{"BTC": hedgePos(0.1, "short", 2)}))
	if st == nil || !st.Held || st.Symbol != "BTC" || st.HeldSide != "short" || st.PrimaryFor != "ETH" {
		t.Fatalf("unexpected hedge status: %+v", st)
	}
	flat := buildHedgeStatus(sc, hedgeTestState(nil))
	if flat == nil || flat.Held {
		t.Fatalf("a configured-but-flat hedge must render as not held: %+v", flat)
	}
}

func TestFormatStrategySummaryLine_ShowsHedge(t *testing.T) {
	line := formatStrategySummaryLine(hedgeTestStrategy(nil), nil, &Config{})
	if !strings.Contains(line, "hedge=BTC") {
		t.Fatalf("startup summary must surface the hedge leg, got %q", line)
	}
	if strings.Contains(formatStrategySummaryLine(hedgeTestStrategy(func(sc *StrategyConfig) { sc.Hedge = nil }), nil, &Config{}), "hedge=") {
		t.Fatal("a strategy with no hedge must not show a hedge tag")
	}
}

func TestPerpsTradeTypeForPosition(t *testing.T) {
	if got := perpsTradeTypeForPosition(hedgePos(0.1, "short", 2)); got != HedgeTradeType {
		t.Fatalf("want %q, got %q", HedgeTradeType, got)
	}
	if got := perpsTradeTypeForPosition(primaryPos(1, "long")); got != "perps" {
		t.Fatalf("want perps, got %q", got)
	}
}
