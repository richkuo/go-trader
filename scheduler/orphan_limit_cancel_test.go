package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func orphanLaneRoster() StrategyConfig {
	return StrategyConfig{
		ID:       "hl-manual-btc-live",
		Type:     "manual",
		Platform: "hyperliquid",
		Symbol:   "BTC",
		Script:   "shared_scripts/check_hyperliquid.py",
		Leverage: 5,
		Args:     []string{"hold", "BTC", "30m", "--mode=live"},
	}
}

func newOrphanLaneState(strategyID string) *AppState {
	return &AppState{
		Strategies: map[string]*StrategyState{
			strategyID: {
				ID:        strategyID,
				Platform:  "hyperliquid",
				Type:      "manual",
				Positions: map[string]*Position{},
				Cash:      10000,
			},
		},
	}
}

func newOrphanLaneNotifier() (*MultiNotifier, *mockNotifier) {
	mock := &mockNotifier{}
	return NewMultiNotifier(notifierBackend{notifier: mock, ownerID: "owner-1"}), mock
}

func resetOrphanLimitCancelAlerts(t *testing.T) {
	t.Helper()
	orphanLimitCancelAlerts.reset()
	t.Cleanup(func() { orphanLimitCancelAlerts.reset() })
}

func seedOrphanLaneRow(t *testing.T, db *StateDB, strategyID string, cancelRequested bool) PendingLimitOrder {
	t.Helper()
	row := PendingLimitOrder{
		StrategyID: strategyID, Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
		CancelRequested: cancelRequested, CreatedAt: time.Now().UTC(),
	}
	id, err := db.InsertPendingLimitOrder(row)
	if err != nil {
		t.Fatalf("seed pending limit order: %v", err)
	}
	row.ID = id
	return row
}

type orphanLaneStubs struct {
	statusCalls int
	cancelCalls int
}

func TestReconcileCancelLaneConvergesRowWhoseStrategyIsAbsent(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

	stubs := &orphanLaneStubs{}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			stubs.statusCalls++
			resting := stubs.cancelCalls == 0
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(resting), FilledSize: 0},
			}}, "", nil
		},
		func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			stubs.cancelCalls++
			return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
		},
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if stubs.cancelCalls != 1 {
		t.Fatalf("cancel calls = %d, want 1 — an orphaned cancel_requested row must be re-issued by the reconciler", stubs.cancelCalls)
	}
	orders, err := db.LoadPendingLimitOrders()
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("row must be cleared once the order is off-book with no unbooked fill, got %+v", orders)
	}
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "cancel-only lane") {
		t.Fatalf("operator must be told the lane cleared the order, dms = %+v", mock.dms)
	}
}

func TestReconcileCancelLaneConvergesPaperAndNonManualRows(t *testing.T) {
	paper := StrategyConfig{
		ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "30m"},
	}
	perps := StrategyConfig{
		ID: "hl-manual-eth-live", Type: "perps", Platform: "hyperliquid", Symbol: "ETH",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"ema_crossover", "ETH", "1h", "--mode=live"},
	}
	for _, sc := range []StrategyConfig{paper, perps} {
		t.Run(sc.Type, func(t *testing.T) {
			resetOrphanLimitCancelAlerts(t)
			cfg := &Config{Strategies: []StrategyConfig{sc}}
			state := newOrphanLaneState(sc.ID)
			db := newLimitTestStateDB(t)
			var mu sync.RWMutex
			seedOrphanLaneRow(t, db, sc.ID, true)

			stubs := &orphanLaneStubs{}
			withStubbedLimitDeps(t,
				func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
					resting := stubs.cancelCalls == 0
					return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
						{OID: 9001, Resting: limitTestBoolPtr(resting), FilledSize: 0},
					}}, "", nil
				},
				func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
					stubs.cancelCalls++
					return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
				},
			)

			reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

			if stubs.cancelCalls != 1 {
				t.Fatalf("cancel calls = %d, want 1 for an adoption-ineligible %s row", stubs.cancelCalls, sc.Type)
			}
			if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
				t.Fatalf("row must be cleared, got %+v", orders)
			}
		})
	}
}

func TestReconcileCancelLaneSkipsCancelWhenOrderIsAlreadyOffBook(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

	stubs := &orphanLaneStubs{}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			stubs.statusCalls++
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			stubs.cancelCalls++
			return nil, "", errors.New("order not found")
		},
	)

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	if stubs.cancelCalls != 0 {
		t.Fatalf("an order already off-book must not be cancelled again, calls = %d", stubs.cancelCalls)
	}
	if stubs.statusCalls != 1 {
		t.Fatalf("status polls = %d, want 1", stubs.statusCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("row must be cleared on a retry that finds the order already gone, got %+v", orders)
	}
}

func TestReconcileCancelLaneKeepsRowAndBooksNothingOnUnadoptedFill(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.5, AvgPx: 2000, Fee: 0.7, Count: 1},
			}}, "", nil
		},
		func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
		},
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("the row must survive as the recovery record for an unbooked fill, got %+v", orders)
	}
	if pos := state.Strategies["hl-manual-eth-live"].Positions["ETH"]; pos != nil {
		t.Fatalf("the lane must book no fill, position = %+v", pos)
	}
	if len(mock.dms) != 1 {
		t.Fatalf("an unresolvable row must raise one owner alert, dms = %+v", mock.dms)
	}
	if !strings.Contains(mock.dms[0].content, "oid=9001") ||
		!strings.Contains(mock.dms[0].content, "hl-manual-eth-live/ETH") {
		t.Fatalf("alert must name the order and its order id, got: %s", mock.dms[0].content)
	}
}

func TestReconcileCancelLaneRetriesAFailedCancelOnLaterTicks(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

	stubs := &orphanLaneStubs{}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 0},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			stubs.cancelCalls++
			return nil, "", errors.New("hyperliquid 503")
		},
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if stubs.cancelCalls != 2 {
		t.Fatalf("a failed cancel must be retried on the next tick, calls = %d", stubs.cancelCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Fatalf("row must be retained while the cancel is unresolved, got %+v", orders)
	}
	if len(mock.dms) != 1 {
		t.Fatalf("the owner alert must be throttled across ticks, dms = %+v", mock.dms)
	}
}

func TestReconcileCancelLaneRefusesWithoutAHyperliquidScript(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	cfg := &Config{Strategies: []StrategyConfig{{
		ID: "okx-ema-btc", Type: "perps", Platform: "okx", Symbol: "BTC",
		Script: "shared_scripts/check_okx.py", Args: []string{"ema_crossover", "BTC", "1h", "--mode=live"},
	}}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

	stubs := &orphanLaneStubs{}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			stubs.statusCalls++
			return &HyperliquidLimitStatusResult{}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			stubs.cancelCalls++
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if stubs.cancelCalls != 0 || stubs.statusCalls != 0 {
		t.Fatalf("the lane must refuse without a Hyperliquid script, status=%d cancel=%d", stubs.statusCalls, stubs.cancelCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Fatalf("the row must survive a refusal, got %+v", orders)
	}
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "no Hyperliquid strategy with a script remains") {
		t.Fatalf("a refusal must be reported to the owner, dms = %+v", mock.dms)
	}
}

func TestReconcileCancelLaneLeavesAnIneligibleRowAloneWithNoCancelQueued(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedOrphanLaneRow(t, db, "hl-manual-eth-live", false)

	stubs := &orphanLaneStubs{}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			stubs.statusCalls++
			return &HyperliquidLimitStatusResult{}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			stubs.cancelCalls++
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if stubs.cancelCalls != 0 || stubs.statusCalls != 0 {
		t.Fatalf("no cancellation is queued, so nothing must be issued: status=%d cancel=%d", stubs.statusCalls, stubs.cancelCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Fatalf("the row must be retained, got %+v", orders)
	}
	if len(mock.dms) != 0 {
		t.Fatalf("a row nobody asked to cancel must not alert, dms = %+v", mock.dms)
	}
}

func TestReconcileCancelLaneCancelsAnExpiredIneligibleRow(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50,
		ExpiresAt: time.Now().UTC().Add(-time.Minute), CreatedAt: time.Now().UTC().Add(-time.Hour),
	})

	stubs := &orphanLaneStubs{}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			resting := stubs.cancelCalls == 0
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(resting), FilledSize: 0},
			}}, "", nil
		},
		func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			stubs.cancelCalls++
			return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
		},
	)

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	if stubs.cancelCalls != 1 {
		t.Fatalf("an expired orphaned row must also converge, calls = %d", stubs.cancelCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("row must be cleared, got %+v", orders)
	}
}

func TestReconcileCancelLaneLeavesEligibleRowsToTheExistingBranch(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	sc, state := newLimitTestStrategy()
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedOrphanLaneRow(t, db, sc.ID, true)

	stubs := &orphanLaneStubs{}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			stubs.statusCalls++
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 0},
			}}, "", nil
		},
		func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			stubs.cancelCalls++
			return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
		},
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if stubs.cancelCalls != 1 {
		t.Fatalf("an eligible row must take the existing branch exactly once, calls = %d", stubs.cancelCalls)
	}
	if stubs.statusCalls != 1 {
		t.Fatalf("an eligible row must not be re-polled by the lane, status polls = %d", stubs.statusCalls)
	}
	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 1 {
		t.Fatalf("the existing branch finalizes next cycle, so the row stays, got %+v", orders)
	}
	if len(mock.dms) != 0 {
		t.Fatalf("an eligible row must raise no orphan alert, dms = %+v", mock.dms)
	}
}

func TestReconcileCancelLaneFlushesAnAdoptedFillBeforeClearingTheRow(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	state := newOrphanLaneState("hl-manual-eth-live")
	ss := state.Strategies["hl-manual-eth-live"]
	ss.Positions["ETH"] = &Position{
		Symbol: "ETH", Quantity: 0.3, InitialQuantity: 0.3, AvgCost: 2000,
		Side: "long", Multiplier: 1, Leverage: 10, OwnerStrategyID: "hl-manual-eth-live",
		OpenedAt: time.Now().UTC(),
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{orphanLaneRoster()}}
	var mu sync.RWMutex

	db.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, FilledSize: 0.3,
		AvgFillPrice: 2000, CancelRequested: true, CreatedAt: time.Now().UTC(),
	})

	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.3, AvgPx: 2000, Fee: 0.4, Count: 1},
			}}, "", nil
		},
		func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
		},
	)

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	if orders, _ := db.LoadPendingLimitOrders(); len(orders) != 0 {
		t.Fatalf("row must be cleared once the adopted fill is flushed, got %+v", orders)
	}
	fresh, err := LoadStateWithDB(cfg, db)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	pos := fresh.Strategies["hl-manual-eth-live"].Positions["ETH"]
	if pos == nil || pos.Quantity != 0.3 {
		t.Fatalf("the adopted fill must reach the state DB before the row is deleted, position = %+v", pos)
	}
}

func TestOrphanLimitCancelTrackerRetainDropsClearedRows(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	now := time.Now().UTC()
	kept := PendingLimitOrder{StrategyID: "a", Symbol: "ETH", OrderOID: 1}
	gone := PendingLimitOrder{StrategyID: "b", Symbol: "BTC", OrderOID: 2}

	if !orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(kept), now) {
		t.Fatal("first alert must fire")
	}
	if !orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(gone), now) {
		t.Fatal("first alert must fire")
	}
	if orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(kept), now) {
		t.Fatal("a repeat inside the throttle window must be suppressed")
	}

	orphanLimitCancelAlerts.Retain([]PendingLimitOrder{kept})

	if orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(kept), now) {
		t.Error("a retained key keeps its throttle window")
	}
	if !orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(gone), now) {
		t.Error("a key dropped by Retain must alert again if the order comes back")
	}
}
