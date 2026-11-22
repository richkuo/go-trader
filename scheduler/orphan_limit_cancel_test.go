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

func TestReconcileCancelLane(t *testing.T) {
	t.Run("ConvergesRowWhoseStrategyIsAbsent", func(t *testing.T) {
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
				state := "cancelled"
				if stubs.cancelCalls == 0 {
					state = "live"
				}
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", state, 0.5, 0, 0, 0),
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
	})
	t.Run("KeepsRowAndBooksNothingOnUnadoptedFill", func(t *testing.T) {
		resetOrphanLimitCancelAlerts(t)
		cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
		state := newOrphanLaneState("hl-manual-eth-live")
		db := newLimitTestStateDB(t)
		var mu sync.RWMutex
		seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

		withStubbedLimitDeps(t,
			func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", "filled", 0.5, 0.5, 2000, 0.7),
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
	})
	t.Run("KeepsAPositiveRowOfAnIneligibleOwnerForReview", func(t *testing.T) {
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

		if _, err := db.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long", OrderOID: 9001,
			LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", EntryATR: 50, FilledSize: 0.3,
			AvgFillPrice: 2000, CancelRequested: true, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed row: %v", err)
		}
		seedLimitFillLineage(t, db, 0, "hl-manual-eth-live", "ETH", 9001, 0.3, 2000, "pos-orphan")

		withStubbedLimitDeps(t,
			func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					provenLimitStatus(9001, "ETH", "cancelled", 0.5, 0.3, 2000, 0.4),
				}}, "", nil
			},
			func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
				return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
			},
		)

		reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)

		orders, _ := db.LoadPendingLimitOrders()
		if len(orders) != 1 || orders[0].FilledSize != 0.3 || orders[0].OperatorRequiredSince.IsZero() {
			t.Fatalf("a positive row whose owner cannot carry its fill must stay and be marked for an operator, got %+v", orders)
		}
		if ev, ok := parseLimitOperatorEvidence(orders[0].OperatorEvidenceJSON); !ok || !ev.Unbookable || !ev.OffBook || !ev.FillsProven {
			t.Fatalf("the marker must carry terminal, off-book evidence, got %+v", ev)
		}
		fresh, err := LoadStateWithDB(cfg, db)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if pos := fresh.Strategies["hl-manual-eth-live"].Positions["ETH"]; pos == nil || pos.Quantity != 0.3 {
			t.Fatalf("keeping the row must leave the book unchanged, position = %+v", pos)
		}
	})
}

func TestOpenStateDBMigratesOperatorRequiredSinceOntoAnExistingQueue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, col := range []string{"operator_required_since", "operator_evidence_json", "fill_proof_version"} {
		if _, err := db.db.Exec("ALTER TABLE pending_limit_orders DROP COLUMN " + col); err != nil {
			t.Fatalf("simulate the pre-migration schema (%s): %v", col, err)
		}
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
	if !orders[0].OperatorRequiredSince.IsZero() || orders[0].OperatorEvidenceJSON != "" || orders[0].FillProofVersion != 0 {
		t.Fatalf("a migrated row must start unmarked with a legacy watermark, got %+v", orders[0])
	}
	evidence := marshalLimitOperatorEvidence(limitRowOperatorEvidence{OrderOID: 9001, Coin: "ETH", OrderState: "filled", OffBook: true, FillsProven: true, ExecutedSize: 0.5})
	if err := reopened.MarkPendingLimitOrderOperatorRequired(orders[0], time.Now().UTC(), evidence); err != nil {
		t.Fatalf("mark on a migrated row: %v", err)
	}
	other := orders[0]
	other.OrderOID++
	if err := reopened.MarkPendingLimitOrderOperatorRequired(other, time.Now().UTC(), evidence); err == nil {
		t.Fatal("a marker bound to another order id must not reach this row")
	}
	orders, _ = reopened.LoadPendingLimitOrders()
	if orders[0].OperatorRequiredSince.IsZero() || orders[0].OperatorEvidenceJSON != evidence {
		t.Fatalf("the migrated columns must persist the marker and its evidence, got %+v", orders[0])
	}
}

func TestClearOperatorRequiredLimitRowRefusalLadder(t *testing.T) {
	marked := time.Now().UTC()
	row := PendingLimitOrder{
		ID: 1, StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long",
		OrderOID: 9001, OrderSize: 0.5, LimitPrice: 2000,
		OperatorRequiredSince: marked,
		OperatorEvidenceJSON: marshalLimitOperatorEvidence(limitRowOperatorEvidence{
			OrderOID: 9001, Coin: "ETH", OrderStatus: "filled", OrderState: "filled", OffBook: true,
			ExecutedSize: 0.5, FillsProven: true, ObservedAt: marked,
		}),
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

	legacy := row
	legacy.OperatorEvidenceJSON = ""
	if got := clearOperatorRequiredLimitRowRefusal(cfgOrphan, legacy, true); !strings.Contains(got, "carries no terminal, off-book proof") {
		t.Errorf("a legacy marker with no terminal proof must be revalidated before a clear, got %q", got)
	}
	onBook := row
	onBook.OperatorEvidenceJSON = marshalLimitOperatorEvidence(limitRowOperatorEvidence{OrderOID: 9001, Coin: "ETH", OrderState: "live", ExecutedSize: 0.5, FillsProven: true})
	if got := clearOperatorRequiredLimitRowRefusal(cfgOrphan, onBook, true); !strings.Contains(got, "has not proven that oid=9001 is off the book") {
		t.Errorf("a marker without off-book proof must refuse, got %q", got)
	}
	settled := row
	settled.OperatorEvidenceJSON = marshalLimitOperatorEvidence(limitRowOperatorEvidence{OrderOID: 9001, Coin: "ETH", OrderState: "cancelled", OffBook: true, ExecutedSize: 0, FillsProven: true})
	if got := clearOperatorRequiredLimitRowRefusal(cfgOrphan, settled, true); !strings.Contains(got, "can still resolve it") {
		t.Errorf("a marker with neither an unbooked fill nor a permanent proof failure must refuse, got %q", got)
	}
	permanent := row
	permanent.OperatorEvidenceJSON = marshalLimitOperatorEvidence(limitRowOperatorEvidence{OrderOID: 9001, Coin: "ETH", OrderState: "cancelled", OffBook: true, ExecutedSize: 0.2, Permanent: true})
	if got := clearOperatorRequiredLimitRowRefusal(cfgOrphan, permanent, true); got != "" {
		t.Errorf("a terminal, off-book row with a permanent fill-proof failure must clear with --flattened, got %q", got)
	}
	if got := clearOperatorRequiredLimitRowRefusal(cfgOrphan, permanent, false); !strings.Contains(got, "--flattened") {
		t.Errorf("a permanent proof failure must still require the flatten assertion, got %q", got)
	}
}
