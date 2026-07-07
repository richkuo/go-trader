package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func limitTestBoolPtr(b bool) *bool { return &b }

func errInjected() error { return errors.New("injected subprocess error") }

func TestBuildHyperliquidLimitOpenArgs(t *testing.T) {
	// Plain Alo order, no margin enforcement.
	got := buildHyperliquidLimitOpenArgs("BTC", "buy", 0.01, 58000, "Alo", "", 0, hlExecuteSnapshot{})
	joined := strings.Join(got, " ")
	for _, want := range []string{"--limit-open", "--symbol=BTC", "--side=buy", "--size=0.01", "--limit-price=58000", "--tif=Alo", "--mode=live"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", want, got)
		}
	}
	if strings.Contains(joined, "--margin-mode") {
		t.Errorf("argv should omit --margin-mode when empty: %v", got)
	}

	// Margin mode + leverage + account snapshot forwarded.
	got = buildHyperliquidLimitOpenArgs("ETH", "sell", 1.5, 3000, "Gtc", "cross", 5, hlExecuteSnapshot{AccountLeverage: 5, AccountMarginMode: "cross"})
	joined = strings.Join(got, " ")
	for _, want := range []string{"--margin-mode=cross", "--leverage=5", "--account-leverage=5", "--account-margin-mode=cross", "--tif=Gtc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", want, got)
		}
	}

	// Empty tif defaults to Alo.
	got = buildHyperliquidLimitOpenArgs("BTC", "buy", 0.01, 1, "", "", 0, hlExecuteSnapshot{})
	if !strings.Contains(strings.Join(got, " "), "--tif=Alo") {
		t.Errorf("empty tif should default to Alo: %v", got)
	}
}

func TestParseHyperliquidLimitOpenOutput(t *testing.T) {
	resting := []byte(`{"platform":"hyperliquid","timestamp":"t","status":"resting","order_oid":12345,"limit_price":58000,"tif":"Alo"}`)
	res, _, err := parseHyperliquidLimitOpenOutput(resting, "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Status != "resting" || res.OrderOID != 12345 {
		t.Errorf("got status=%q oid=%d", res.Status, res.OrderOID)
	}

	// An Alo rejection arrives as status=error + JSON, alongside a non-nil runErr
	// (exit 1). The JSON is authoritative — parse must surface it, not bury it.
	rejected := []byte(`{"platform":"hyperliquid","timestamp":"t","status":"error","error":"limit order rejected: post only order would have immediately matched"}`)
	res, _, err = parseHyperliquidLimitOpenOutput(rejected, "stderr", errInjected())
	if err != nil {
		t.Fatalf("structured error should parse cleanly, got err: %v", err)
	}
	if res.Status != "error" || !strings.Contains(res.Error, "post only") {
		t.Errorf("got status=%q error=%q", res.Status, res.Error)
	}

	// Garbage stdout + runErr → wrapped error.
	if _, _, err := parseHyperliquidLimitOpenOutput([]byte("not json"), "", errInjected()); err == nil {
		t.Error("expected error for garbage stdout")
	}
}

func TestParseHyperliquidLimitStatusOutput(t *testing.T) {
	// resting=true, partial fill.
	out := []byte(`{"platform":"hyperliquid","timestamp":"t","orders":[{"oid":1,"resting":true,"filled_size":0.4,"avg_px":2000,"fee":0.2,"count":1}]}`)
	res, _, err := parseHyperliquidLimitStatusOutput(out, "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(res.Orders) != 1 {
		t.Fatalf("want 1 order, got %d", len(res.Orders))
	}
	o := res.Orders[0]
	if o.Resting == nil || !*o.Resting {
		t.Errorf("resting should be true ptr, got %v", o.Resting)
	}
	if o.FilledSize != 0.4 || o.AvgPx != 2000 {
		t.Errorf("got filled=%g avg=%g", o.FilledSize, o.AvgPx)
	}

	// resting=null (open-orders fetch failed) must decode to a nil pointer so the
	// scheduler defers the cancelled verdict.
	out = []byte(`{"platform":"hyperliquid","timestamp":"t","open_orders_error":"boom","orders":[{"oid":1,"resting":null,"filled_size":0,"avg_px":0,"fee":0,"count":0}]}`)
	res, _, err = parseHyperliquidLimitStatusOutput(out, "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Orders[0].Resting != nil {
		t.Errorf("resting should be nil ptr on null, got %v", res.Orders[0].Resting)
	}
	if res.OpenOrdersError != "boom" {
		t.Errorf("open_orders_error not surfaced: %q", res.OpenOrdersError)
	}
}

func TestParseHyperliquidCancelOrderOutput(t *testing.T) {
	out := []byte(`{"platform":"hyperliquid","timestamp":"t","oid":7,"cancelled":true}`)
	res, _, err := parseHyperliquidCancelOrderOutput(out, "", nil)
	if err != nil || !res.Cancelled || res.OID != 7 {
		t.Fatalf("got res=%+v err=%v", res, err)
	}
	// Non-fatal "already gone" cancel: cancelled=false + cancel_error, exit 0.
	out = []byte(`{"platform":"hyperliquid","timestamp":"t","oid":7,"cancelled":false,"cancel_error":"order not found"}`)
	res, _, err = parseHyperliquidCancelOrderOutput(out, "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Cancelled || !strings.Contains(res.CancelError, "not found") {
		t.Errorf("got cancelled=%v cancel_error=%q", res.Cancelled, res.CancelError)
	}
}

func TestLimitStatusSinceMs(t *testing.T) {
	// Zero time → 0 (Python falls back to its 7-day window).
	if got := limitStatusSinceMs(time.Time{}); got != 0 {
		t.Errorf("zero time should map to 0, got %d", got)
	}
	// A real placement time → that time minus a 60s skew buffer, in ms.
	created := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	want := created.Add(-60 * time.Second).UnixMilli()
	if got := limitStatusSinceMs(created); got != want {
		t.Errorf("sinceMs = %d, want %d (createdAt - 60s)", got, want)
	}
}

// TestReconcilePendingLimitOrdersAnchorsLookbackToPlacement is the #886 review
// regression: the fill poll must reach back to the order's placement time, not a
// rolling 7-day window, so a fill on an order resting >7 days is never missed.
func TestReconcilePendingLimitOrdersAnchorsLookbackToPlacement(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	// Order placed 30 days ago — far outside the default 7-day window.
	placed := time.Now().UTC().Add(-30 * 24 * time.Hour)
	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: placed,
	})

	var gotSinceMs int64 = -1
	withStubbedLimitDeps(t,
		func(_ string, _ string, _ []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
			gotSinceMs = sinceMs
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 0},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	want := limitStatusSinceMs(placed)
	if gotSinceMs != want {
		t.Errorf("status poll sinceMs = %d, want %d (anchored to 30-day-old placement, not a 7-day window)", gotSinceMs, want)
	}
	// Sanity: the anchor is well before a 7-day-only window would reach.
	sevenDaysAgo := time.Now().UTC().Add(-7 * 24 * time.Hour).UnixMilli()
	if gotSinceMs >= sevenDaysAgo {
		t.Errorf("sinceMs %d should be older than 7 days ago %d", gotSinceMs, sevenDaysAgo)
	}
}

func TestLimitOrderFullyFilled(t *testing.T) {
	if !limitOrderFullyFilled(1.0, 1.0) {
		t.Error("exact match should be full")
	}
	if !limitOrderFullyFilled(0.9999999, 1.0) {
		t.Error("within tolerance should be full")
	}
	if limitOrderFullyFilled(0.4, 1.0) {
		t.Error("partial should not be full")
	}
}

func newLimitTestStrategy() (StrategyConfig, *AppState) {
	sc := StrategyConfig{
		ID:       "hl-manual-eth-live",
		Type:     "manual",
		Platform: "hyperliquid",
		Symbol:   "ETH",
		Script:   "shared_scripts/check_hyperliquid.py",
		Leverage: 10,
		Args:     []string{"hold", "ETH", "30m", "--mode=live"},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			sc.ID: {
				ID:        sc.ID,
				Platform:  "hyperliquid",
				Type:      "manual",
				Positions: map[string]*Position{},
				Cash:      10000,
			},
		},
	}
	return sc, state
}

func TestApplyLimitFillProgressCreate(t *testing.T) {
	sc, state := newLimitTestStrategy()
	origRecorder := tradeRecorder
	tradeRecorder = func(string, Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()

	o := PendingLimitOrder{
		ID: 1, StrategyID: sc.ID, Symbol: "ETH", Side: "long",
		OrderOID: 9001, LimitPrice: 2000, OrderSize: 0.5, FilledSize: 0,
	}
	now := time.Now().UTC()
	n, err := applyLimitFillProgress(state, sc, o, 0.5, 2000, 0.7, 50, now)
	if err != nil {
		t.Fatalf("apply create: %v", err)
	}
	if n != 1 {
		t.Errorf("trades booked = %d, want 1", n)
	}
	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil {
		t.Fatal("position not created")
	}
	if pos.Quantity != 0.5 || pos.AvgCost != 2000 || pos.EntryATR != 50 || pos.Side != "long" {
		t.Errorf("pos = %+v", pos)
	}
	if pos.OwnerStrategyID != sc.ID {
		t.Errorf("owner = %q", pos.OwnerStrategyID)
	}
	if got := state.Strategies[sc.ID].Cash; got != 10000-0.7 {
		t.Errorf("cash = %g, want %g (fee deducted)", got, 10000-0.7)
	}
}

func TestApplyLimitFillProgressGrow(t *testing.T) {
	sc, state := newLimitTestStrategy()
	origRecorder := tradeRecorder
	tradeRecorder = func(string, Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()
	now := time.Now().UTC()

	// First fill 0.4 @ 2000.
	o := PendingLimitOrder{ID: 1, StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001, LimitPrice: 2000, OrderSize: 1.0, FilledSize: 0}
	if _, err := applyLimitFillProgress(state, sc, o, 0.4, 2000, 0.2, 50, now); err != nil {
		t.Fatalf("first fill: %v", err)
	}
	// Watermark advances (simulating the reconcile loop).
	o.FilledSize, o.AvgFillPrice, o.FillFee = 0.4, 2000, 0.2

	// Second fill grows to cumulative 1.0 @ VWAP 2010, cumulative fee 0.5.
	n, err := applyLimitFillProgress(state, sc, o, 1.0, 2010, 0.5, 50, now)
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	if n != 1 {
		t.Errorf("trades booked = %d, want 1", n)
	}
	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos.Quantity != 1.0 {
		t.Errorf("pos.Quantity = %g, want 1.0", pos.Quantity)
	}
	if pos.AvgCost != 2010 {
		t.Errorf("pos.AvgCost = %g, want 2010 (cumulative VWAP)", pos.AvgCost)
	}
	if pos.InitialQuantity != 1.0 {
		t.Errorf("pos.InitialQuantity = %g, want 1.0", pos.InitialQuantity)
	}
	// Cash deducts only the delta fee across both legs: 0.2 + (0.5-0.2) = 0.5.
	if got := state.Strategies[sc.ID].Cash; got != 10000-0.5 {
		t.Errorf("cash = %g, want %g", got, 10000-0.5)
	}

	// #886 review: a multi-partial fill must count as ONE opened position. The
	// first leg is a real open (trade_type=perps); each growth leg is tagged
	// scale_in so LifetimeTradeStats' open-count (is_close=0 AND
	// trade_type<>'scale_in') excludes it — matching a single market open.
	hist := state.Strategies[sc.ID].TradeHistory
	if len(hist) != 2 {
		t.Fatalf("expected 2 trade legs, got %d", len(hist))
	}
	if hist[0].TradeType != "perps" || hist[0].IsClose {
		t.Errorf("first leg should be an open perps trade, got type=%q is_close=%v", hist[0].TradeType, hist[0].IsClose)
	}
	if hist[1].TradeType != scaleInTradeType {
		t.Errorf("growth leg should be tagged %q (excluded from open-count), got %q", scaleInTradeType, hist[1].TradeType)
	}
	// Both legs share the position_id so W/L grouping stays correct.
	if hist[0].PositionID == "" || hist[0].PositionID != hist[1].PositionID {
		t.Errorf("legs must share position_id: %q vs %q", hist[0].PositionID, hist[1].PositionID)
	}
}

func TestApplyLimitFillProgressOwnerGuard(t *testing.T) {
	sc, state := newLimitTestStrategy()
	// Pre-existing foreign position; a first fill must NOT adopt it.
	state.Strategies[sc.ID].Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 3, OwnerStrategyID: "someone-else"}
	o := PendingLimitOrder{ID: 1, StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001, LimitPrice: 2000, OrderSize: 0.5, FilledSize: 0}
	if _, err := applyLimitFillProgress(state, sc, o, 0.5, 2000, 0.7, 50, time.Now().UTC()); err == nil {
		t.Fatal("expected error adopting a pre-existing foreign position")
	}
	if state.Strategies[sc.ID].Positions["ETH"].Quantity != 3 {
		t.Error("foreign position must be untouched")
	}
}

func newLimitTestStateDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newLimitOpenGuardHarness(t *testing.T) (*Config, StrategyConfig, *StateDB) {
	t.Helper()
	sc, state := newLimitTestStrategy()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SaveState(state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}
	return cfg, sc, db
}

func withStubbedLimitOpen(t *testing.T, open func(script, symbol, side string, size, limitPx float64, tif, marginMode string, leverage float64, snapshot hlExecuteSnapshot) (*HyperliquidLimitOpenResult, string, error)) {
	t.Helper()
	origOpen := runHyperliquidLimitOpenFn
	runHyperliquidLimitOpenFn = open
	t.Cleanup(func() { runHyperliquidLimitOpenFn = origOpen })
}

func TestManualLimitOpenRefusesQueuedMarketAction(t *testing.T) {
	cfg, sc, db := newLimitOpenGuardHarness(t)
	if err := db.InsertPendingManualAction(PendingManualAction{
		StrategyID: sc.ID, Action: "open", Symbol: sc.Symbol, Side: "long",
		Quantity: 0.5, FillPrice: 2000, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert pending manual action: %v", err)
	}

	var venueCalls int32
	withStubbedLimitOpen(t, func(string, string, string, float64, float64, string, string, float64, hlExecuteSnapshot) (*HyperliquidLimitOpenResult, string, error) {
		atomic.AddInt32(&venueCalls, 1)
		return &HyperliquidLimitOpenResult{Status: "resting", OrderOID: 9001}, "", nil
	})

	rc := runManualLimitOpen(cfg, sc, db, manualLimitOpenInputs{
		strategyID: sc.ID, side: "long", openSide: "buy", margin: 50, limitPrice: 2000, tif: "Alo",
	})
	if rc == 0 {
		t.Fatal("manual-limit-open with a queued market open returned success")
	}
	if got := atomic.LoadInt32(&venueCalls); got != 0 {
		t.Fatalf("limit venue calls = %d, want 0 when a market open is queued", got)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("pending limit rows = %+v, want none", orders)
	}
}

func TestManualOpenCoreRefusesQueuedLimitOrder(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "test-secret")
	cfg, sc, db := newLimitOpenGuardHarness(t)
	if _, err := db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: sc.Symbol, Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert pending limit: %v", err)
	}

	deps := newCLIManualCoreDeps(cfg, db, nil)
	deps.fetchMids = func([]string) (map[string]float64, error) {
		return map[string]float64{sc.Symbol: 2000}, nil
	}
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
		t.Error("market execute must not be called while a resting limit exists")
		return nil, "", errors.New("execute called")
	}

	_, err := manualOpenCore(deps, sc, manualOpenInputs{StrategyID: sc.ID, Margin: 50})
	if err == nil || !strings.Contains(err.Error(), "resting limit order") {
		t.Fatalf("manual-open err = %v, want resting-limit refusal", err)
	}
}

func TestManualLimitOpenLockPreventsCrossProcessDoubleFire(t *testing.T) {
	sc, state := newLimitTestStrategy()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	dbA, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db A: %v", err)
	}
	defer dbA.Close()
	if err := dbA.SaveState(state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	dbB, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db B: %v", err)
	}
	defer dbB.Close()
	cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}

	var calls int32
	enteredSubmit := make(chan struct{})
	releaseSubmit := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSubmit) }) }
	defer release()

	withStubbedLimitOpen(t, func(string, string, string, float64, float64, string, string, float64, hlExecuteSnapshot) (*HyperliquidLimitOpenResult, string, error) {
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			close(enteredSubmit)
			<-releaseSubmit
		}
		return &HyperliquidLimitOpenResult{Status: "resting", OrderOID: 9000 + int64(call)}, "", nil
	})

	input := manualLimitOpenInputs{
		strategyID: sc.ID, side: "long", openSide: "buy", margin: 50, limitPrice: 2000, tif: "Alo",
	}
	aDone := make(chan int, 1)
	go func() { aDone <- runManualLimitOpen(cfg, sc, dbA, input) }()

	select {
	case <-enteredSubmit:
	case rc := <-aDone:
		t.Fatalf("first limit-open returned before reaching venue: rc=%d", rc)
	case <-time.After(3 * time.Second):
		t.Fatal("first limit-open did not reach venue")
	}

	bDone := make(chan int, 1)
	go func() { bDone <- runManualLimitOpen(cfg, sc, dbB, input) }()

	select {
	case rc := <-bDone:
		t.Fatalf("second limit-open completed while first held the lock (rc=%d, calls=%d)", rc, atomic.LoadInt32(&calls))
	case <-time.After(400 * time.Millisecond):
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("limit venue calls while first submit blocked = %d, want 1", got)
	}

	release()
	if rc := <-aDone; rc != 0 {
		t.Fatalf("first limit-open rc=%d, want 0", rc)
	}
	select {
	case rc := <-bDone:
		if rc == 0 {
			t.Fatalf("second limit-open should be refused after the first row lands")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second limit-open did not return after first released the lock")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("limit venue calls = %d, want exactly one", got)
	}
	if orders, _ := dbA.LoadPendingLimitOrders(); len(orders) != 1 || orders[0].OrderOID != 9001 {
		t.Fatalf("pending limit orders = %+v, want one row for first order", orders)
	}
}

func newPartialLimitPositionHarness(t *testing.T) (*Config, StrategyConfig, *StateDB) {
	t.Helper()
	sc, state := newLimitTestStrategy()
	state.Strategies[sc.ID].Positions[sc.Symbol] = &Position{
		Symbol:          sc.Symbol,
		Quantity:        0.4,
		InitialQuantity: 0.4,
		AvgCost:         2000,
		EntryATR:        50,
		Side:            "long",
		Multiplier:      1,
		Leverage:        sc.Leverage,
		OwnerStrategyID: sc.ID,
		OpenedAt:        time.Now().UTC().Add(-time.Hour),
		TradePositionID: "pos-limit-partial",
	}
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SaveState(state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	if _, err := db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: sc.Symbol, Side: "long", OrderOID: 9001,
		LimitPrice: 1990, OrderSize: 1.0, TIF: "Alo", FilledSize: 0.4,
		AvgFillPrice: 2000, FillFee: 0.2, EntryATR: 50,
		CreatedAt: time.Now().UTC().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("insert pending limit: %v", err)
	}
	cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}
	return cfg, sc, db
}

func TestManualCloseCancelsPartialLimitRemainderBeforeFlatten(t *testing.T) {
	cfg, sc, db := newPartialLimitPositionHarness(t)
	cancelCalls := 0
	statusCalls := 0
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			statusCalls++
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.4, AvgPx: 2000, Fee: 0.2},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			cancelCalls++
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)
	deps := newCLIManualCoreDeps(cfg, db, nil)
	execCalls := 0
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
		execCalls++
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2010, TotalSz: 0.4, OID: 4242, Fee: 0.4}}}, "", nil
	}

	if _, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID}); err != nil {
		t.Fatalf("manual close with partial limit remainder: %v", err)
	}
	if cancelCalls != 1 || statusCalls != 1 || execCalls != 1 {
		t.Fatalf("calls cancel=%d status=%d exec=%d, want 1/1/1", cancelCalls, statusCalls, execCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("pending limit rows = %+v, want deleted after proven cancelled", orders)
	}
	actions, _ := db.LoadPendingManualActions()
	if len(actions) != 1 || actions[0].Action != "close" || actions[0].Quantity != 0.4 {
		t.Fatalf("pending manual actions = %+v, want one close for current position", actions)
	}
}

// TestManualCloseReconcilesStaleSnapshotAgainstAdoptedLimitFill is the review-2
// regression (#1263): when the scheduler adopts a limit fill (persisting the
// watermark) after the CLI's position snapshot but before flushing the grown
// position to the DB, a full close on a SHARED coin must flatten the true,
// larger on-chain size — not the stale snapshot — so it never leaves an
// untracked residual after the daemon books flat on IsFullClose.
func TestManualCloseReconcilesStaleSnapshotAgainstAdoptedLimitFill(t *testing.T) {
	cfg, sc, db := newPartialLimitPositionHarness(t)
	// Share ETH with a live peer so closeFullPosition is false (sized close, not
	// market_close) — the only path where the stale snapshot leaks a residual.
	peer := StrategyConfig{
		ID: "hl-manual-eth-peer", Type: "manual", Platform: "hyperliquid",
		Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Leverage: 10,
		Args: []string{"hold", "ETH", "30m", "--mode=live"},
	}
	cfg.Strategies = append(cfg.Strategies, peer)

	// The watermark on the resting row is already advanced to 0.7 (the daemon
	// adopted the new fill in-memory) while the DB position snapshot still reads
	// the stale 0.4 (SaveState has not flushed the grown position yet).
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("want one resting row, got %d", len(orders))
	}
	if err := db.UpdatePendingLimitOrderFill(orders[0].ID, 0.7, 2005, 0.35); err != nil {
		t.Fatalf("advance watermark: %v", err)
	}

	// Status: off-book, cumulative fill 0.7 @ VWAP 2005 == watermark → fully
	// adopted, no unadopted fill; the cleared fill is the true position size.
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.7, AvgPx: 2005, Fee: 0.35},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)
	deps := newCLIManualCoreDeps(cfg, db, nil)
	var gotCloseQty float64
	var gotFullClose bool
	deps.execute = func(_ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, closeFull bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
		gotCloseQty = size
		gotFullClose = closeFull
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2010, TotalSz: size, OID: 4242, Fee: 0.4}}}, "", nil
	}

	if _, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID}); err != nil {
		t.Fatalf("manual close: %v", err)
	}
	if gotFullClose {
		t.Fatalf("shared coin must take the sized-close path (closeFullPosition=false), got market_close")
	}
	if gotCloseQty != 0.7 {
		t.Fatalf("on-chain close size = %g, want 0.7 (true adopted fill, not stale 0.4 snapshot)", gotCloseQty)
	}
	actions, _ := db.LoadPendingManualActions()
	if len(actions) != 1 || actions[0].Action != "close" || !actions[0].IsFullClose {
		t.Fatalf("pending manual actions = %+v, want one full close", actions)
	}
	if actions[0].Quantity != 0.7 {
		t.Fatalf("queued close quantity = %g, want 0.7 (true size)", actions[0].Quantity)
	}
	// Realized PnL must use the true qty (0.7) and cumulative VWAP (2005), net of
	// the close fee: 0.7*(2010-2005) - 0.4 = 3.1.
	if got := actions[0].RealizedPnL; got < 3.09 || got > 3.11 {
		t.Fatalf("queued RealizedPnL = %g, want ~3.10 (0.7*(2010-2005)-0.4)", got)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("pending limit rows = %+v, want deleted after proven cancelled", orders)
	}
}

// staleReconcileCloseHarness sets up the #1263 stale-snapshot scenario for
// manual-close --qty tests: the DB position reads the stale 0.4 while the
// resting limit row's watermark is advanced to the true adopted 0.7 (the daemon
// adopted the fill but has not flushed the grown position). The status/cancel
// stubs report the order off-book at 0.7, so clearResting reconciles the
// snapshot up to 0.7 before the --qty bounds are evaluated.
func staleReconcileCloseHarness(t *testing.T) (*Config, StrategyConfig, *StateDB) {
	t.Helper()
	cfg, sc, db := newPartialLimitPositionHarness(t)
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("want one resting row, got %d", len(orders))
	}
	if err := db.UpdatePendingLimitOrderFill(orders[0].ID, 0.7, 2005, 0.35); err != nil {
		t.Fatalf("advance watermark: %v", err)
	}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.7, AvgPx: 2005, Fee: 0.35},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)
	return cfg, sc, db
}

// TestManualCloseAcceptsExplicitQtyMatchingReconciledSize is the #1263 review-3
// (finding 2) regression: an explicit --qty equal to the true, already-adopted
// size must be accepted — not refused against the stale, smaller pre-reconcile
// snapshot. Pre-fix the bounds check ran before clearResting, so --qty 0.7 was
// rejected as "exceeds open position 0.4".
func TestManualCloseAcceptsExplicitQtyMatchingReconciledSize(t *testing.T) {
	cfg, sc, db := staleReconcileCloseHarness(t)
	deps := newCLIManualCoreDeps(cfg, db, nil)
	var gotCloseQty float64
	deps.execute = func(_ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
		gotCloseQty = size
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2010, TotalSz: size, OID: 4242, Fee: 0.4}}}, "", nil
	}

	if _, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID, Qty: 0.7}); err != nil {
		t.Fatalf("manual close --qty 0.7 (true adopted size) refused: %v", err)
	}
	if gotCloseQty != 0.7 {
		t.Fatalf("on-chain close size = %g, want 0.7 (reconciled true size)", gotCloseQty)
	}
	actions, _ := db.LoadPendingManualActions()
	if len(actions) != 1 || !actions[0].IsFullClose || actions[0].Quantity != 0.7 {
		t.Fatalf("queued action = %+v, want one full close of 0.7", actions)
	}
}

// TestManualClosePartialQtyBetweenStaleAndReconciledSize covers the finding-2
// must-survive "between" case: --qty 0.5 sits between the stale 0.4 snapshot and
// the true 0.7. Pre-fix it was rejected (0.5 > 0.4). Post-fix it is a valid
// partial close of EXACTLY 0.5 against the reconciled 0.7 — an explicit --qty is
// never scaled up to the reconciled full size, so the close never removes more
// than the operator asked for.
func TestManualClosePartialQtyBetweenStaleAndReconciledSize(t *testing.T) {
	cfg, sc, db := staleReconcileCloseHarness(t)
	deps := newCLIManualCoreDeps(cfg, db, nil)
	var gotCloseQty float64
	var gotFullClose bool
	deps.execute = func(_ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, closeFull bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
		gotCloseQty = size
		gotFullClose = closeFull
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2010, TotalSz: size, OID: 4242, Fee: 0.4}}}, "", nil
	}

	if _, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID, Qty: 0.5}); err != nil {
		t.Fatalf("manual close --qty 0.5 (valid partial vs true 0.7) refused: %v", err)
	}
	if gotCloseQty != 0.5 {
		t.Fatalf("on-chain close size = %g, want 0.5 (explicit partial, not scaled to 0.7)", gotCloseQty)
	}
	if gotFullClose {
		t.Fatalf("explicit partial --qty must take the sized-close path, got closeFullPosition=true")
	}
	actions, _ := db.LoadPendingManualActions()
	if len(actions) != 1 || actions[0].IsFullClose || actions[0].Quantity != 0.5 {
		t.Fatalf("queued action = %+v, want one partial (not full) close of 0.5", actions)
	}
}

// TestManualCloseRejectsQtyExceedingReconciledSize confirms the bounds rejection
// now reports the RECONCILED size (0.7), not the stale snapshot (0.4): --qty 0.9
// exceeds even the true adopted size and is refused with the true figure.
func TestManualCloseRejectsQtyExceedingReconciledSize(t *testing.T) {
	cfg, sc, db := staleReconcileCloseHarness(t)
	deps := newCLIManualCoreDeps(cfg, db, nil)
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
		t.Error("execute must not run when --qty exceeds the reconciled position")
		return nil, "", errors.New("execute called")
	}

	_, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID, Qty: 0.9})
	if err == nil || !strings.Contains(err.Error(), "exceeds open position 0.700000") {
		t.Fatalf("manual close --qty 0.9 err = %v, want rejection citing the reconciled 0.7", err)
	}
}

// staleReadRowGoneCloseHarness reproduces the #1263 review-4 window: the CLI's
// initial loadState captures a stale 0.4 snapshot, then — while the CLI is
// (conceptually) blocked acquiring the global manual-action lock — the daemon
// adopts the terminal limit fill, flushes the grown 0.7 to state.db, and deletes
// the pending_limit_orders row (flush-before-delete). By the time clearResting
// runs it finds NO row (clearedQty==0), so the fix must re-read the fresh 0.7
// from state.db rather than size against the stale 0.4. The coin is shared with
// a live peer so closeFullPosition=false (a sized close, the only path that
// leaks a residual). The injected loadState returns the stale snapshot on its
// first call and delegates to the real DB (0.7) thereafter.
func staleReadRowGoneCloseHarness(t *testing.T) (StrategyConfig, manualCoreDeps, *StateDB) {
	t.Helper()
	cfg, sc, db := newPartialLimitPositionHarness(t)
	// Post-race truth in state.db: row deleted, position grown to 0.7.
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("want one resting row, got %d", len(orders))
	}
	if err := db.DeletePendingLimitOrder(orders[0].ID); err != nil {
		t.Fatalf("delete row: %v", err)
	}
	st, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := st.Strategies[sc.ID].Positions[sc.Symbol]
	p.Quantity, p.InitialQuantity, p.AvgCost = 0.7, 0.7, 2005
	if err := db.SaveState(st); err != nil {
		t.Fatalf("save grown position: %v", err)
	}
	// Share ETH with a live peer so closeFullPosition is false (sized close).
	cfg.Strategies = append(cfg.Strategies, StrategyConfig{
		ID: "hl-manual-eth-peer", Type: "manual", Platform: "hyperliquid",
		Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Leverage: 10,
		Args: []string{"hold", "ETH", "30m", "--mode=live"},
	})

	deps := newCLIManualCoreDeps(cfg, db, nil)
	realLoad := deps.loadState
	callN := 0
	deps.loadState = func(id, sym string) (manualStateView, error) {
		callN++
		if callN == 1 {
			return manualStateView{HasStrategy: true, Pos: &Position{
				Symbol: sc.Symbol, Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000,
				EntryATR: 50, Side: "long", Multiplier: 1, Leverage: sc.Leverage,
				OwnerStrategyID: sc.ID, OpenedAt: time.Now().UTC().Add(-time.Hour),
				TradePositionID: "pos-limit-partial",
			}}, nil
		}
		return realLoad(id, sym)
	}
	return sc, deps, db
}

// TestManualCloseRereadsFreshPositionWhenRowDeletedBeforeClearResting is the
// #1263 review-4 regression: a full close whose pre-lock snapshot is stale (0.4)
// and whose resting row was flushed+deleted before clearResting must flatten the
// true, re-read 0.7 on a shared coin — never the stale snapshot, which would
// leak an untracked residual after the daemon books the IsFullClose row flat.
func TestManualCloseRereadsFreshPositionWhenRowDeletedBeforeClearResting(t *testing.T) {
	sc, deps, db := staleReadRowGoneCloseHarness(t)
	var gotCloseQty float64
	var gotFullClose bool
	deps.execute = func(_ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, closeFull bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
		gotCloseQty = size
		gotFullClose = closeFull
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2010, TotalSz: size, OID: 4242, Fee: 0.4}}}, "", nil
	}

	if _, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID}); err != nil {
		t.Fatalf("manual close: %v", err)
	}
	if gotFullClose {
		t.Fatalf("shared coin must take the sized-close path (closeFullPosition=false), got market_close")
	}
	if gotCloseQty != 0.7 {
		t.Fatalf("on-chain close size = %g, want 0.7 (fresh re-read, not stale 0.4 snapshot)", gotCloseQty)
	}
	actions, _ := db.LoadPendingManualActions()
	if len(actions) != 1 || !actions[0].IsFullClose || actions[0].Quantity != 0.7 {
		t.Fatalf("queued action = %+v, want one full close of 0.7", actions)
	}
}

// TestManualCloseExplicitQtyValidatedAgainstRereadWhenRowGone covers review-4
// must-survive case 3: an explicit --qty equal to the true adopted size (0.7)
// exceeds the stale pre-lock snapshot (0.4) with no resting row left to
// reconcile. Pre-fix the bound at :1020 validated against 0.4 and wrongly
// refused it; the fresh re-read makes 0.7 a valid full close.
func TestManualCloseExplicitQtyValidatedAgainstRereadWhenRowGone(t *testing.T) {
	sc, deps, db := staleReadRowGoneCloseHarness(t)
	var gotCloseQty float64
	deps.execute = func(_ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, _ bool, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
		gotCloseQty = size
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2010, TotalSz: size, OID: 4242, Fee: 0.4}}}, "", nil
	}

	if _, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID, Qty: 0.7}); err != nil {
		t.Fatalf("manual close --qty 0.7 refused against stale snapshot instead of fresh re-read: %v", err)
	}
	if gotCloseQty != 0.7 {
		t.Fatalf("on-chain close size = %g, want 0.7 (validated against re-read)", gotCloseQty)
	}
	actions, _ := db.LoadPendingManualActions()
	if len(actions) != 1 || !actions[0].IsFullClose || actions[0].Quantity != 0.7 {
		t.Fatalf("queued action = %+v, want one full close of 0.7", actions)
	}
}

func TestManualAddCancelsPartialLimitRemainderBeforeAveraging(t *testing.T) {
	cfg, sc, db := newPartialLimitPositionHarness(t)
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.4, AvgPx: 2000, Fee: 0.2},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)
	deps := newCLIManualCoreDeps(cfg, db, nil)
	deps.fetchMids = func([]string) (map[string]float64, error) {
		return map[string]float64{sc.Symbol: 2000}, nil
	}
	execCalls := 0
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
		execCalls++
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 1995, TotalSz: 0.05, OID: 5252, Fee: 0.1}}}, "", nil
	}

	if _, err := manualAddCore(deps, sc, manualAddInputs{StrategyID: sc.ID, Margin: 10}); err != nil {
		t.Fatalf("manual add with partial limit remainder: %v", err)
	}
	if execCalls != 1 {
		t.Fatalf("execute calls = %d, want 1", execCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("pending limit rows = %+v, want deleted after proven cancelled", orders)
	}
	actions, _ := db.LoadPendingManualActions()
	if len(actions) != 1 || actions[0].Action != "add" {
		t.Fatalf("pending manual actions = %+v, want one add", actions)
	}
}

func TestManualCloseDefersWhenLimitCancelHasUnadoptedFill(t *testing.T) {
	cfg, sc, db := newPartialLimitPositionHarness(t)
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.6, AvgPx: 1998, Fee: 0.3},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)
	deps := newCLIManualCoreDeps(cfg, db, nil)
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
		t.Error("execute must not run while a limit fill is unadopted")
		return nil, "", errors.New("execute called")
	}

	_, err := manualCloseCore(deps, sc, manualCloseInputs{StrategyID: sc.ID})
	if err == nil || !strings.Contains(err.Error(), "unadopted fill") {
		t.Fatalf("manual close err = %v, want unadopted-fill refusal", err)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 || !orders[0].CancelRequested {
		t.Fatalf("pending limit rows = %+v, want retained with cancel_requested", orders)
	}
	if actions, _ := db.LoadPendingManualActions(); len(actions) != 0 {
		t.Fatalf("pending manual actions = %+v, want none", actions)
	}
}

func TestManualAddDefersWhenLimitCancelBookStateUnknown(t *testing.T) {
	cfg, sc, db := newPartialLimitPositionHarness(t)
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{OpenOrdersError: "open orders unavailable", Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: nil, FilledSize: 0.4, AvgPx: 2000, Fee: 0.2},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)
	deps := newCLIManualCoreDeps(cfg, db, nil)
	deps.fetchMids = func([]string) (map[string]float64, error) {
		return map[string]float64{sc.Symbol: 2000}, nil
	}
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
		t.Error("execute must not run while limit book state is unknown")
		return nil, "", errors.New("execute called")
	}

	_, err := manualAddCore(deps, sc, manualAddInputs{StrategyID: sc.ID, Margin: 10})
	if err == nil || !strings.Contains(err.Error(), "open-orders state unknown") {
		t.Fatalf("manual add err = %v, want unknown-book refusal", err)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 || !orders[0].CancelRequested {
		t.Fatalf("pending limit rows = %+v, want retained with cancel_requested", orders)
	}
	if actions, _ := db.LoadPendingManualActions(); len(actions) != 0 {
		t.Fatalf("pending manual actions = %+v, want none", actions)
	}
}

func TestPendingLimitOrderCRUD(t *testing.T) {
	db := newLimitTestStateDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	id, err := db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: "s1", Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
		ExpiresAt: now.Add(2 * time.Hour), CreatedAt: now,
	})
	if err != nil || id == 0 {
		t.Fatalf("insert: id=%d err=%v", id, err)
	}

	orders, err := db.LoadPendingLimitOrders()
	if err != nil || len(orders) != 1 {
		t.Fatalf("load: n=%d err=%v", len(orders), err)
	}
	o := orders[0]
	if o.OrderOID != 9001 || o.LimitPrice != 2000 || o.OrderSize != 0.5 || o.TIF != "Alo" || o.EntryATR != 50 {
		t.Errorf("round-trip mismatch: %+v", o)
	}
	if o.ExpiresAt.IsZero() || o.ExpiresAt.Unix() != now.Add(2*time.Hour).Unix() {
		t.Errorf("expires_at mismatch: %v", o.ExpiresAt)
	}

	if cnt, _ := db.CountPendingLimitOrders("s1", "ETH"); cnt != 1 {
		t.Errorf("count = %d, want 1", cnt)
	}

	if err := db.UpdatePendingLimitOrderFill(id, 0.3, 1999, 0.15); err != nil {
		t.Fatalf("update fill: %v", err)
	}
	orders, _ = db.LoadPendingLimitOrders()
	if orders[0].FilledSize != 0.3 || orders[0].AvgFillPrice != 1999 || orders[0].FillFee != 0.15 {
		t.Errorf("watermark not updated: %+v", orders[0])
	}

	n, err := db.MarkPendingLimitOrderCancelRequested("s1", "ETH")
	if err != nil || n != 1 {
		t.Fatalf("mark cancel: n=%d err=%v", n, err)
	}
	orders, _ = db.LoadPendingLimitOrders()
	if !orders[0].CancelRequested {
		t.Error("cancel_requested not set")
	}

	if err := db.DeletePendingLimitOrder(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	orders, _ = db.LoadPendingLimitOrders()
	if len(orders) != 0 {
		t.Errorf("expected empty after delete, got %d", len(orders))
	}
}

// withStubbedLimitDeps swaps the subprocess hooks the reconcile uses and returns
// a restore func. The protection sync is stubbed so no .venv is needed.
func withStubbedLimitDeps(t *testing.T, status func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error), cancel func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error)) {
	t.Helper()
	origStatus := runHyperliquidLimitStatusFn
	origCancel := runHyperliquidCancelOrderFn
	origSync := syncHyperliquidProtection
	origRecorder := tradeRecorder
	runHyperliquidLimitStatusFn = status
	runHyperliquidCancelOrderFn = cancel
	syncHyperliquidProtection = func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
		return &HyperliquidProtectionSyncResult{}, true
	}
	tradeRecorder = func(string, Trade) error { return nil }
	t.Cleanup(func() {
		runHyperliquidLimitStatusFn = origStatus
		runHyperliquidCancelOrderFn = origCancel
		syncHyperliquidProtection = origSync
		tradeRecorder = origRecorder
	})
}

func TestReconcilePendingLimitOrdersFullFill(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex

	id, _ := db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
	})

	// Status: fully filled, no longer resting.
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.5, AvgPx: 2000, Fee: 0.7, Count: 1},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			t.Error("cancel should not be called on a clean fill")
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)

	alerts := reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if len(alerts) != 1 || alerts[0].trades != 1 {
		t.Fatalf("alerts = %+v", alerts)
	}
	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.5 || pos.AvgCost != 2000 {
		t.Fatalf("position = %+v", pos)
	}
	// Terminal: row deleted.
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Errorf("expected row deleted, got %d (id=%d)", len(orders), id)
	}
}

// TestReconcilePendingLimitOrdersFullFillFlushesPositionBeforeRowDelete is the
// #1263 review-3 regression: when a resting limit order fully fills and the
// reconcile deletes its pending_limit_orders row in the SAME cycle, the grown
// position must be durably flushed to state.db BEFORE the row disappears.
// Otherwise a cross-process CLI (manual-close reading state.db) sees the row
// gone while the DB position still understates the fill, and a sized shared-coin
// close leaks an untracked residual. The reconcile grows the position only
// in-memory; the end-of-cycle SaveState is what normally flushes it. This drives
// reconcile in isolation (no end-of-cycle save) and asserts a fresh
// cross-process read already sees the true size — proving the flush now happens
// inside the reconcile, ahead of the delete.
func TestReconcilePendingLimitOrdersFullFillFlushesPositionBeforeRowDelete(t *testing.T) {
	sc, state := newLimitTestStrategy()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Seed the DB with the strategy (no position yet), mirroring a running daemon
	// that persisted the strategy in a prior cycle.
	if err := db.SaveState(state); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}
	var mu sync.RWMutex

	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
	})

	// Fully filled, no longer resting → terminal, row deleted this cycle.
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.5, AvgPx: 2000, Fee: 0.7, Count: 1},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			t.Error("cancel should not be called on a clean fill")
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	// Terminal row deleted.
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("terminal row not deleted: %+v", orders)
	}
	// The grown position is already durable in state.db (flush ran before the
	// delete): a fresh cross-process read sees the true 0.5, not an absent/stale
	// snapshot. Without the flush this reload would carry no ETH position.
	fresh, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	ss := fresh.Strategies[sc.ID]
	if ss == nil || ss.Positions["ETH"] == nil {
		t.Fatalf("position not flushed to state.db before the terminal row was deleted: %+v", fresh.Strategies[sc.ID])
	}
	if got := ss.Positions["ETH"].Quantity; got != 0.5 {
		t.Fatalf("flushed position quantity = %g, want 0.5 (true adopted fill)", got)
	}
}

func TestReconcilePendingLimitOrdersPartialThenComplete(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 1.0, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
	})

	// Cycle 1: partial 0.4, still resting.
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 0.4, AvgPx: 2000, Fee: 0.2, Count: 1},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.4 {
		t.Fatalf("after partial: pos=%+v", pos)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 || orders[0].FilledSize != 0.4 {
		t.Fatalf("watermark not persisted: %+v", orders)
	}

	// Cycle 2: completes to 1.0, no longer resting.
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 1.0, AvgPx: 2005, Fee: 0.5, Count: 2},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	pos = state.Strategies[sc.ID].Positions["ETH"]
	if pos.Quantity != 1.0 || pos.AvgCost != 2005 {
		t.Fatalf("after complete: pos=%+v", pos)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Errorf("expected row deleted after full fill, got %d", len(orders))
	}
}

func TestReconcilePendingLimitOrdersCancelRequested(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
		CancelRequested: true, CreatedAt: time.Now().UTC(),
	})

	// Still resting, no fill. Cancel must be issued; row retained for next cycle.
	cancelCalls := 0
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 0, AvgPx: 0, Fee: 0},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			cancelCalls++
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if cancelCalls != 1 {
		t.Errorf("cancel calls = %d, want 1", cancelCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Errorf("row should be retained pending finalize, got %d", len(orders))
	}
	if pos := state.Strategies[sc.ID].Positions["ETH"]; pos != nil {
		t.Error("no position should exist for an unfilled cancelled order")
	}

	// Next cycle: order gone (resting=false), no fill → finalize + delete.
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Errorf("expected row deleted after cancel finalize, got %d", len(orders))
	}
}

func TestReconcilePendingLimitOrdersExpiry(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	// expires_at in the past → TTL expiry triggers a cancel.
	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
		ExpiresAt: time.Now().UTC().Add(-time.Minute), CreatedAt: time.Now().UTC().Add(-time.Hour),
	})

	cancelCalls := 0
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 0},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			cancelCalls++
			return &HyperliquidCancelOrderResult{OID: 9001, Cancelled: true}, "", nil
		},
	)
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if cancelCalls != 1 {
		t.Errorf("expiry should issue a cancel, calls = %d", cancelCalls)
	}
}

// TestReconcilePendingLimitOrdersDeferOnUnknownBook verifies that when the
// open-orders fetch failed (resting=nil) the reconcile does NOT finalize the
// row as cancelled — it waits for a definitive book state.
func TestReconcilePendingLimitOrdersDeferOnUnknownBook(t *testing.T) {
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
	})
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{OpenOrdersError: "boom", Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: nil, FilledSize: 0},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Errorf("row must be retained when book state is unknown, got %d", len(orders))
	}
}
