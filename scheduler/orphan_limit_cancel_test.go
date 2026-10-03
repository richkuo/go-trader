package main

import (
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
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, notifier, nil)

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
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, notifier, nil)

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

	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)

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

func TestOpenStateDBMigratesOperatorRequiredSinceOntoAnExistingQueue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.db.Exec("ALTER TABLE pending_limit_orders DROP COLUMN operator_required_since"); err != nil {
		t.Fatalf("simulate the pre-migration schema: %v", err)
	}
	if _, err := db.db.Exec(`INSERT INTO pending_limit_orders
		(strategy_id, symbol, side, order_oid, limit_price, order_size, tif, filled_size, avg_fill_price, fill_fee, entry_atr, cancel_requested, expires_at, created_at)
		VALUES ('hl-manual-eth-live','ETH','long',9001,2000,0.5,'Alo',0,0,0,50,1,'', ?)`,
		formatTime(time.Now().UTC())); err != nil {
		t.Fatalf("seed a pre-migration row: %v", err)
	}
	db.Close()

	reopened, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("reopen must migrate the existing queue, got: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	orders, err := reopened.LoadPendingLimitOrders()
	if err != nil {
		t.Fatalf("load after migration: %v", err)
	}
	if len(orders) != 1 || orders[0].OrderOID != 9001 {
		t.Fatalf("the pre-migration row must survive, got %+v", orders)
	}
	if !orders[0].OperatorRequiredSince.IsZero() {
		t.Fatalf("a migrated row must start unmarked, got %+v", orders[0])
	}
	if err := reopened.MarkPendingLimitOrderOperatorRequired(orders[0].ID, time.Now().UTC()); err != nil {
		t.Fatalf("mark on a migrated row: %v", err)
	}
	orders, _ = reopened.LoadPendingLimitOrders()
	if orders[0].OperatorRequiredSince.IsZero() {
		t.Fatalf("the migrated column must persist the marker, got %+v", orders[0])
	}
}

func TestClearOperatorRequiredLimitRowRefusalLadder(t *testing.T) {
	marked := time.Now().UTC()
	row := PendingLimitOrder{
		ID: 1, StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long",
		OrderOID: 9001, OrderSize: 0.5, LimitPrice: 2000,
		OperatorRequiredSince: marked,
	}

	adoptable, _ := newLimitTestStrategy()
	cfgAdoptable := &Config{Strategies: []StrategyConfig{adoptable}}
	unmarkedAdoptable := row
	unmarkedAdoptable.OperatorRequiredSince = time.Time{}
	if got := clearOperatorRequiredLimitRowRefusal(cfgAdoptable, unmarkedAdoptable, true); !strings.Contains(got, "the scheduler adopts this fill itself") {
		t.Errorf("an adoptable row the reconciler has not given up on must never be cleared by hand, got %q", got)
	}
	if got := clearOperatorRequiredLimitRowRefusal(cfgAdoptable, row, true); got != "" {
		t.Errorf("an adoptable row the reconciler marked operator-required must be clearable, or the refusal alert can never be stopped, got %q", got)
	}
	if got := clearOperatorRequiredLimitRowRefusal(cfgAdoptable, row, false); !strings.Contains(got, "--flattened") {
		t.Errorf("clearing a marked adoptable row must still require the operator's explicit assertion, got %q", got)
	}

	cfgOrphan := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}

	unmarked := row
	unmarked.OperatorRequiredSince = time.Time{}
	if got := clearOperatorRequiredLimitRowRefusal(cfgOrphan, unmarked, true); !strings.Contains(got, "still converging on its own") {
		t.Errorf("a row the lane has not given up on must not be cleared, got %q", got)
	}

	if got := clearOperatorRequiredLimitRowRefusal(cfgOrphan, row, false); !strings.Contains(got, "--flattened") {
		t.Errorf("clearing must require the operator's explicit assertion, got %q", got)
	}

	if got := clearOperatorRequiredLimitRowRefusal(cfgOrphan, row, true); got != "" {
		t.Errorf("an operator-required orphaned row with --flattened must clear, got refusal %q", got)
	}
}
