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

func TestApplyLimitFillProgressGrow(t *testing.T) {
	sc, state := newLimitTestStrategy()
	origRecorder := tradeRecorder
	tradeRecorder = func(string, Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()
	now := time.Now().UTC()

	o := PendingLimitOrder{ID: 1, StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001, LimitPrice: 2000, OrderSize: 1.0, FilledSize: 0}
	if _, err := applyLimitFillProgress(state, sc, o, 0.4, 2000, 0.2, 50, ATRMethodSimple, now); err != nil {
		t.Fatalf("first fill: %v", err)
	}
	o.FilledSize, o.AvgFillPrice, o.FillFee = 0.4, 2000, 0.2

	n, err := applyLimitFillProgress(state, sc, o, 1.0, 2010, 0.5, 50, ATRMethodSimple, now)
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
	if got := state.Strategies[sc.ID].Cash; got != 10000-0.5 {
		t.Errorf("cash = %g, want %g", got, 10000-0.5)
	}

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
	if hist[0].PositionID == "" || hist[0].PositionID != hist[1].PositionID {
		t.Errorf("legs must share position_id: %q vs %q", hist[0].PositionID, hist[1].PositionID)
	}
}

func withStubbedLimitOpen(t *testing.T, open func(script, symbol, side string, size, limitPx float64, tif, marginMode string, leverage float64, snapshot hlExecuteSnapshot) (*HyperliquidLimitOpenResult, string, error)) {
	t.Helper()
	origOpen := runHyperliquidLimitOpenFn
	runHyperliquidLimitOpenFn = open
	t.Cleanup(func() { runHyperliquidLimitOpenFn = origOpen })
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
	go func() { aDone <- runManualLimitOpen(cfg, sc, openTestStore(t, dbA), input) }()

	select {
	case <-enteredSubmit:
	case rc := <-aDone:
		t.Fatalf("first limit-open returned before reaching venue: rc=%d", rc)
	case <-time.After(3 * time.Second):
		t.Fatal("first limit-open did not reach venue")
	}

	bDone := make(chan int, 1)
	go func() { bDone <- runManualLimitOpen(cfg, sc, openTestStore(t, dbB), input) }()

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
	deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
	execCalls := 0
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, hlCloseMode, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
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

func TestManualCloseReconcilesStaleSnapshotAgainstAdoptedLimitFill(t *testing.T) {
	cfg, sc, db := newPartialLimitPositionHarness(t)
	peer := StrategyConfig{
		ID: "hl-manual-eth-peer", Type: "manual", Platform: "hyperliquid",
		Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Leverage: 10,
		Args: []string{"hold", "ETH", "30m", "--mode=live"},
	}
	cfg.Strategies = append(cfg.Strategies, peer)

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
	deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
	var gotCloseQty float64
	var gotFullClose bool
	deps.execute = func(_ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, closeMode hlCloseMode, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
		gotCloseQty = size
		gotFullClose = closeMode == hlCloseModeWhole
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
	if got := actions[0].RealizedPnL; got < 3.09 || got > 3.11 {
		t.Fatalf("queued RealizedPnL = %g, want ~3.10 (0.7*(2010-2005)-0.4)", got)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("pending limit rows = %+v, want deleted after proven cancelled", orders)
	}
}

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

func TestManualClosePartialQtyBetweenStaleAndReconciledSize(t *testing.T) {
	cfg, sc, db := staleReconcileCloseHarness(t)
	deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
	var gotCloseQty float64
	var gotFullClose bool
	deps.execute = func(_ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, closeMode hlCloseMode, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
		gotCloseQty = size
		gotFullClose = closeMode == hlCloseModeWhole
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

func staleReadRowGoneCloseHarness(t *testing.T) (StrategyConfig, manualCoreDeps, *StateDB) {
	t.Helper()
	cfg, sc, db := newPartialLimitPositionHarness(t)
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
	cfg.Strategies = append(cfg.Strategies, StrategyConfig{
		ID: "hl-manual-eth-peer", Type: "manual", Platform: "hyperliquid",
		Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Leverage: 10,
		Args: []string{"hold", "ETH", "30m", "--mode=live"},
	})

	deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
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

func TestManualCloseRereadsFreshPositionWhenRowDeletedBeforeClearResting(t *testing.T) {
	sc, deps, db := staleReadRowGoneCloseHarness(t)
	var gotCloseQty float64
	var gotFullClose bool
	deps.execute = func(_ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, closeMode hlCloseMode, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
		gotCloseQty = size
		gotFullClose = closeMode == hlCloseModeWhole
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
	deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
	deps.fetchMids = func([]string) (map[string]float64, error) {
		return map[string]float64{sc.Symbol: 2000}, nil
	}
	execCalls := 0
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, hlCloseMode, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
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
	deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, hlCloseMode, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
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
	deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
	deps.fetchMids = func([]string) (map[string]float64, error) {
		return map[string]float64{sc.Symbol: 2000}, nil
	}
	deps.execute = func(string, string, string, float64, float64, int64, float64, string, float64, hlCloseMode, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
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

func TestReconcilePendingLimitOrdersFullFillFlushesPositionBeforeRowDelete(t *testing.T) {
	sc, state := newLimitTestStrategy()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.SaveState(state); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}
	var mu sync.RWMutex
	withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.5})

	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: sc.ID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, CreatedAt: time.Now().UTC(),
	})

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

	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)

	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("terminal row not deleted: %+v", orders)
	}
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

	withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 0.4})
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
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
	pos := state.Strategies[sc.ID].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.4 {
		t.Fatalf("after partial: pos=%+v", pos)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 || orders[0].FilledSize != 0.4 {
		t.Fatalf("watermark not persisted: %+v", orders)
	}

	withStubbedHLLiveExposure(t, HLPosition{Coin: "ETH", Size: 1.0})
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
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
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
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
	if cancelCalls != 1 {
		t.Errorf("cancel calls = %d, want 1", cancelCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Errorf("row should be retained pending finalize, got %d", len(orders))
	}
	if pos := state.Strategies[sc.ID].Positions["ETH"]; pos != nil {
		t.Error("no position should exist for an unfilled cancelled order")
	}

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
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Errorf("expected row deleted after cancel finalize, got %d", len(orders))
	}
}

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
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Errorf("row must be retained when book state is unknown, got %d", len(orders))
	}
}
