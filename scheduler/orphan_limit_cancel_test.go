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

	state := orphanLimitStateOffBookUnadoptedFill
	if !orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(kept), state, now) {
		t.Fatal("first alert must fire")
	}
	if !orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(gone), state, now) {
		t.Fatal("first alert must fire")
	}
	if orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(kept), state, now) {
		t.Fatal("a repeat inside the throttle window must be suppressed")
	}

	orphanLimitCancelAlerts.Retain([]PendingLimitOrder{kept})

	if orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(kept), state, now) {
		t.Error("a retained key keeps its throttle window")
	}
	if !orphanLimitCancelAlerts.Record(orphanLimitCancelKeyFor(gone), state, now) {
		t.Error("a key dropped by Retain must alert again if the order comes back")
	}
}

func offBookUnadoptedFillOutcome() orphanLimitCancelOutcome {
	return orphanLimitCancelOutcome{
		State:        orphanLimitStateOffBookUnadoptedFill,
		Reason:       "the exchange reports a filled size of 0.500000 where the book holds 0.000000",
		CancelIssued: true,
		AdoptedFill:  0,
		ExchangeFill: 0.5,
	}
}

func TestOrphanLimitCancelDMDescribesAnOffBookFillAsAnUntrackedPosition(t *testing.T) {
	o := PendingLimitOrder{
		StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5,
	}
	msg := formatOrphanLimitCancelDM(o, "the strategy is absent from this config", offBookUnadoptedFillOutcome())

	for _, banned := range []string{
		"can still be resting",
		"MAY still be resting",
		"IS STILL RESTING",
		"Cancel oid=9001 on the Hyperliquid UI",
		"NOT cancelled",
		"NOT confirmed cancelled",
	} {
		if strings.Contains(msg, banned) {
			t.Errorf("an off-book filled order must not be described as resting or cancellable, found %q in:\n%s", banned, msg)
		}
	}
	for _, want := range []string{
		"UNTRACKED HYPERLIQUID POSITION",
		"OFF-BOOK",
		"nothing left to cancel",
		"NO stop-loss",
		"Retrying cannot clear this",
		"flatten the position yourself",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("alert must state the real on-chain state, missing %q in:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "hl-manual-eth-live/ETH oid=9001") {
		t.Errorf("alert must name the order and its order id, got:\n%s", msg)
	}
}

func TestOrphanLimitCancelRetryNoteDeniesARetryOnlyForTheUnadoptableFill(t *testing.T) {
	if note := orphanLimitCancelRetryNote(orphanLimitStateOffBookUnadoptedFill); strings.Contains(note, "will retry next cycle") {
		t.Errorf("an unadoptable fill must not promise an automatic retry, got %q", note)
	}
	for _, state := range []orphanLimitCancelState{
		orphanLimitStateUnknown, orphanLimitStateResting, orphanLimitStateOffBookRowStuck,
	} {
		if note := orphanLimitCancelRetryNote(state); !strings.Contains(note, "will retry next cycle") {
			t.Errorf("state %d genuinely converges, so it must keep the retry promise, got %q", state, note)
		}
	}
}

func TestOrphanLimitCancelDMKeepsRestingWordingForAStillRestingOrder(t *testing.T) {
	o := PendingLimitOrder{
		StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5,
	}
	resting := formatOrphanLimitCancelDM(o, "the strategy is absent from this config", orphanLimitCancelOutcome{
		State: orphanLimitStateResting, Reason: "cancel failed: hyperliquid 503",
	})
	if !strings.Contains(resting, "IS STILL RESTING") || !strings.Contains(resting, "Cancel oid=9001 on the Hyperliquid UI") {
		t.Errorf("a still-resting order must keep the resting wording and the cancel instruction, got:\n%s", resting)
	}

	unknown := formatOrphanLimitCancelDM(o, "the strategy is absent from this config", orphanLimitCancelOutcome{
		State: orphanLimitStateUnknown, Reason: "order state unreadable: boom",
	})
	if !strings.Contains(unknown, "MAY still be resting") || !strings.Contains(unknown, "Check oid=9001 on the Hyperliquid UI") {
		t.Errorf("an unreadable state must say the order may still be resting, got:\n%s", unknown)
	}

	stuck := formatOrphanLimitCancelDM(o, "the strategy is absent from this config", orphanLimitCancelOutcome{
		State: orphanLimitStateOffBookRowStuck, Reason: "the queue row could not be cleared (disk full)",
	})
	if strings.Contains(stuck, "still resting") || strings.Contains(stuck, "Cancel oid=") {
		t.Errorf("an off-book row-write failure needs no exchange action, got:\n%s", stuck)
	}
	if !strings.Contains(stuck, "not exposure on the exchange") {
		t.Errorf("a bookkeeping failure must say it is not exposure, got:\n%s", stuck)
	}
}

func TestOrphanLimitCancelResolvedMessageDistinguishesTheThreeOutcomes(t *testing.T) {
	o := PendingLimitOrder{StrategyID: "hl-manual-eth-live", Symbol: "ETH", OrderOID: 9001}
	block := "the strategy is absent from this config"

	cancelled := orphanLimitCancelResolvedMessage(o, block, orphanLimitCancelOutcome{
		Resolved: true, CancelIssued: true,
	})
	if !strings.Contains(cancelled, "cancelled the resting order oid=9001") || !strings.Contains(cancelled, "with no fill") {
		t.Errorf("a cancel the lane issued must be reported as a cancel, got: %s", cancelled)
	}

	alreadyGone := orphanLimitCancelResolvedMessage(o, block, orphanLimitCancelOutcome{Resolved: true})
	if !strings.Contains(alreadyGone, "already off-book and sent no cancel") {
		t.Errorf("a finalize without a cancel must say no cancel was sent, got: %s", alreadyGone)
	}
	if strings.Contains(alreadyGone, "cancelled the resting order") {
		t.Errorf("a finalize without a cancel must not claim an on-chain cancel, got: %s", alreadyGone)
	}

	filled := orphanLimitCancelResolvedMessage(o, block, orphanLimitCancelOutcome{Resolved: true, AdoptedFill: 0.5})
	if !strings.Contains(filled, "fill of 0.500000 already booked") {
		t.Errorf("an adopted fill must be reported as booked exposure, got: %s", filled)
	}
	if strings.Contains(filled, "with no fill") {
		t.Errorf("an adopted fill must not be reported as no fill, got: %s", filled)
	}
}

func TestReconcileCancelLaneReportsAFinalizeWithoutACancelHonestly(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			t.Error("no cancel should be issued for an order already off-book")
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if len(mock.dms) != 1 {
		t.Fatalf("dms = %+v", mock.dms)
	}
	if !strings.Contains(mock.dms[0].content, "already off-book and sent no cancel") {
		t.Fatalf("the lane must not claim a cancel it never sent, got: %s", mock.dms[0].content)
	}
}

func TestReconcileCancelLaneBacksOffTheExchangePollForAnUnresolvableRow(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	applyAlertThrottleInterval(time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	row := seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

	stubs := &orphanLaneStubs{}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			stubs.statusCalls++
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.5, AvgPx: 2000, Fee: 0.7, Count: 1},
			}}, "", nil
		},
		func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			stubs.cancelCalls++
			return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
		},
	)

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if stubs.statusCalls != 1 {
		t.Fatalf("first pass must poll once, calls = %d", stubs.statusCalls)
	}
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if stubs.statusCalls != 1 {
		t.Fatalf("a row the lane cannot resolve must not re-poll every cycle, calls = %d", stubs.statusCalls)
	}

	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("row must survive, got %+v", orders)
	}
	if orders[0].OperatorRequiredSince.IsZero() {
		t.Fatalf("the backoff must be persisted on the row so a restart re-derives it, got %+v", orders[0])
	}

	if err := db.MarkPendingLimitOrderOperatorRequired(row.ID, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("age the marker: %v", err)
	}
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	if stubs.statusCalls != 2 {
		t.Fatalf("the lane must poll again once the backoff window elapses, calls = %d", stubs.statusCalls)
	}
}

func TestOrphanLimitPollDeferredIsReDerivedFromTheRowAcrossARestart(t *testing.T) {
	applyAlertThrottleInterval(time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })
	now := time.Now().UTC()

	if orphanLimitPollDeferred(PendingLimitOrder{}, now) {
		t.Error("an unmarked row must never be deferred")
	}
	if !orphanLimitPollDeferred(PendingLimitOrder{OperatorRequiredSince: now.Add(-time.Minute)}, now) {
		t.Error("a freshly marked row must be deferred")
	}
	if orphanLimitPollDeferred(PendingLimitOrder{OperatorRequiredSince: now.Add(-2 * time.Hour)}, now) {
		t.Error("a marker older than the throttle window must poll again")
	}
}

func TestReconcileCancelLaneBacksOffEachUnresolvableRowIndependently(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	applyAlertThrottleInterval(time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	for _, oid := range []int64{9001, 9002} {
		if _, err := db.InsertPendingLimitOrder(PendingLimitOrder{
			StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long", OrderOID: oid,
			LimitPrice: 2000, OrderSize: 0.5, TIF: "Alo", CancelRequested: true,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	polled := map[int64]int{}
	withStubbedLimitDeps(t,
		func(_ string, _ string, oids []int64, _ int64) (*HyperliquidLimitStatusResult, string, error) {
			polled[oids[0]]++
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: oids[0], Resting: limitTestBoolPtr(false), FilledSize: 0.5, AvgPx: 2000, Count: 1},
			}}, "", nil
		},
		func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
		},
	)

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	if polled[9001] != 1 || polled[9002] != 1 {
		t.Fatalf("each row must be polled once and then deferred on its own marker, polls = %+v", polled)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 2 {
		t.Fatalf("both rows must survive, got %+v", orders)
	}
	for _, o := range orders {
		if o.OperatorRequiredSince.IsZero() {
			t.Fatalf("row oid=%d carries no persisted marker: %+v", o.OrderOID, o)
		}
	}
}

func TestReconcileCancelLaneClearsTheMarkerWhenTheStrategyBecomesAdoptable(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	applyAlertThrottleInterval(time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	sc, state := newLimitTestStrategy()
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	row := seedOrphanLaneRow(t, db, sc.ID, true)
	if err := db.MarkPendingLimitOrderOperatorRequired(row.ID, time.Now().UTC()); err != nil {
		t.Fatalf("mark: %v", err)
	}

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

	cfg := &Config{Strategies: []StrategyConfig{sc}}
	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	if stubs.statusCalls != 1 || stubs.cancelCalls != 1 {
		t.Fatalf("the backoff must never delay the eligible branch, status=%d cancel=%d", stubs.statusCalls, stubs.cancelCalls)
	}
	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("row = %+v", orders)
	}
	if !orders[0].OperatorRequiredSince.IsZero() {
		t.Fatalf("an adoptable row must not keep a stale operator-required marker, got %+v", orders[0])
	}
}

func TestReconcileCancelLaneClearsTheMarkerWhenTheStateStopsBeingTerminal(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	applyAlertThrottleInterval(time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	row := seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)
	if err := db.MarkPendingLimitOrderOperatorRequired(row.ID, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("mark: %v", err)
	}

	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			return nil, "", errors.New("hyperliquid 503")
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{}, "", nil
		},
	)

	reconcilePendingLimitOrders(state, cfg, db, &mu, nil, nil)

	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 {
		t.Fatalf("row = %+v", orders)
	}
	if !orders[0].OperatorRequiredSince.IsZero() {
		t.Fatalf("a transient failure must not inherit the terminal backoff, got %+v", orders[0])
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

func TestOrphanLimitCancelDMOffersOnlyPathsThatStopTheAlert(t *testing.T) {
	applyAlertThrottleInterval(6 * time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	o := PendingLimitOrder{
		StrategyID: "hl-manual-eth-live", Symbol: "ETH", Side: "long", OrderOID: 9001,
		LimitPrice: 2000, OrderSize: 0.5,
	}
	msg := formatOrphanLimitCancelDM(o, "the strategy is absent from this config", offBookUnadoptedFillOutcome())

	for _, want := range []string{
		"go-trader manual-clear-limit-row 9001 --flattened",
		"cannot see a flatten it does not own",
		"this alert repeats every 6h0m0s until the queue row is cleared",
		"Do not flatten and then restore without clearing the row",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("every remediation path named must actually stop the alert, missing %q in:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "or flatten the position yourself on the Hyperliquid UI.") {
		t.Errorf("a bare flatten does not clear the row, so it must not be offered as a terminal resolution:\n%s", msg)
	}
	if note := orphanLimitCancelRetryNote(orphanLimitStateOffBookUnadoptedFill); !strings.Contains(note, "manual-clear-limit-row") {
		t.Errorf("the log line must name the same clearing path as the DM, got %q", note)
	}
}

func TestFindPendingLimitOrderByOID(t *testing.T) {
	orders := []PendingLimitOrder{
		{ID: 1, OrderOID: 9001, Symbol: "ETH"},
		{ID: 2, OrderOID: 9002, Symbol: "BTC"},
	}
	got, ok := findPendingLimitOrderByOID(orders, 9002)
	if !ok || got.ID != 2 || got.Symbol != "BTC" {
		t.Fatalf("lookup by oid = %+v ok=%v", got, ok)
	}
	if _, ok := findPendingLimitOrderByOID(orders, 9003); ok {
		t.Error("an unknown oid must not match a row")
	}
	if _, ok := findPendingLimitOrderByOID(nil, 9001); ok {
		t.Error("an empty queue must not match a row")
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
	if got := clearOperatorRequiredLimitRowRefusal(cfgAdoptable, row, true); !strings.Contains(got, "the scheduler adopts this fill itself") {
		t.Errorf("an adoptable row must never be cleared by hand, got %q", got)
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

func TestReconcileStopsAlertingOnceTheOperatorClearsTheRow(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	applyAlertThrottleInterval(time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	row := seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

	stubs := &orphanLaneStubs{}
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			stubs.statusCalls++
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.5, AvgPx: 2000, Count: 1},
			}}, "", nil
		},
		func(_ string, _ string, oid int64) (*HyperliquidCancelOrderResult, string, error) {
			return &HyperliquidCancelOrderResult{OID: oid, Cancelled: true}, "", nil
		},
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)
	if len(mock.dms) != 1 {
		t.Fatalf("the first pass must alert, dms = %+v", mock.dms)
	}

	orders, _ := db.LoadPendingLimitOrders()
	if len(orders) != 1 || orders[0].OperatorRequiredSince.IsZero() {
		t.Fatalf("the row must be marked operator-required before it can be cleared, got %+v", orders)
	}
	if refusal := clearOperatorRequiredLimitRowRefusal(cfg, orders[0], true); refusal != "" {
		t.Fatalf("the command must accept the row the alert names, got %q", refusal)
	}
	if err := db.DeletePendingLimitOrder(row.ID); err != nil {
		t.Fatalf("clear row: %v", err)
	}

	before := stubs.statusCalls
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)

	if stubs.statusCalls != before {
		t.Errorf("a cleared row must drive no further exchange poll, calls = %d want %d", stubs.statusCalls, before)
	}
	if len(mock.dms) != 1 {
		t.Errorf("a cleared row must stop the CRITICAL alert, dms = %+v", mock.dms)
	}
}

func TestOrphanLimitCancelSeverityRanksExposureAboveBookkeeping(t *testing.T) {
	got := map[orphanLimitCancelState]int{}
	for _, st := range []orphanLimitCancelState{
		orphanLimitStateOffBookRowStuck, orphanLimitStateUnknown,
		orphanLimitStateResting, orphanLimitStateOffBookUnadoptedFill,
	} {
		got[st] = orphanLimitCancelSeverity(st)
	}
	if !(got[orphanLimitStateOffBookRowStuck] < got[orphanLimitStateUnknown] &&
		got[orphanLimitStateUnknown] < got[orphanLimitStateResting] &&
		got[orphanLimitStateResting] < got[orphanLimitStateOffBookUnadoptedFill]) {
		t.Fatalf("an untracked position must outrank a possible resting order, which must outrank a bookkeeping failure: %+v", got)
	}
}

func TestOrphanLimitCancelThrottleLetsAnEscalationThrough(t *testing.T) {
	applyAlertThrottleInterval(6 * time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	cases := []struct {
		name  string
		first orphanLimitCancelState
	}{
		{"after a resting alert", orphanLimitStateResting},
		{"after an unreadable-state alert", orphanLimitStateUnknown},
		{"after a row-stuck alert", orphanLimitStateOffBookRowStuck},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetOrphanLimitCancelAlerts(t)
			k := orphanLimitCancelKeyFor(PendingLimitOrder{StrategyID: "a", Symbol: "ETH", OrderOID: 9001})
			now := time.Now().UTC()

			if !orphanLimitCancelAlerts.Record(k, tc.first, now) {
				t.Fatal("the first alert must fire")
			}
			if !orphanLimitCancelAlerts.Record(k, orphanLimitStateOffBookUnadoptedFill, now.Add(time.Minute)) {
				t.Fatal("an untracked position must reach the owner even inside the earlier alert's throttle window")
			}
			if orphanLimitCancelAlerts.Record(k, orphanLimitStateOffBookUnadoptedFill, now.Add(2*time.Minute)) {
				t.Error("an unchanged severity must still be throttled after the escalation")
			}
			if orphanLimitCancelAlerts.Record(k, tc.first, now.Add(3*time.Minute)) {
				t.Error("a de-escalation must not re-open the throttle window")
			}
		})
	}
}

func TestOrphanLimitCancelThrottleSuppressesAnUnchangedSeverity(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	applyAlertThrottleInterval(6 * time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	k := orphanLimitCancelKeyFor(PendingLimitOrder{StrategyID: "a", Symbol: "ETH", OrderOID: 9001})
	now := time.Now().UTC()

	if !orphanLimitCancelAlerts.Record(k, orphanLimitStateOffBookUnadoptedFill, now) {
		t.Fatal("the first alert must fire")
	}
	if orphanLimitCancelAlerts.Record(k, orphanLimitStateOffBookUnadoptedFill, now.Add(5*time.Hour)) {
		t.Error("two consecutive untracked-position outcomes must not double-alert inside one window")
	}
	if !orphanLimitCancelAlerts.Record(k, orphanLimitStateOffBookUnadoptedFill, now.Add(7*time.Hour)) {
		t.Error("the repeat must fire again once the window elapses")
	}
}

func TestOrphanLimitCancelThrottleDoesNotLatchSeverityAcrossWindows(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	applyAlertThrottleInterval(time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	k := orphanLimitCancelKeyFor(PendingLimitOrder{StrategyID: "a", Symbol: "ETH", OrderOID: 9001})
	now := time.Now().UTC()

	if !orphanLimitCancelAlerts.Record(k, orphanLimitStateOffBookUnadoptedFill, now) {
		t.Fatal("the first alert must fire")
	}
	if !orphanLimitCancelAlerts.Record(k, orphanLimitStateResting, now.Add(2*time.Hour)) {
		t.Error("a fresh window must alert on its own severity, not stay gated by the previous window's peak")
	}
	if !orphanLimitCancelAlerts.Record(k, orphanLimitStateOffBookUnadoptedFill, now.Add(2*time.Hour+time.Minute)) {
		t.Error("the escalation must still fire inside the new window")
	}
}

func TestReconcileDeliversAnEscalationDMAfterARestingAlert(t *testing.T) {
	resetOrphanLimitCancelAlerts(t)
	applyAlertThrottleInterval(6 * time.Hour)
	t.Cleanup(func() { applyAlertThrottleInterval(DefaultAlertThrottleInterval) })

	cfg := &Config{Strategies: []StrategyConfig{orphanLaneRoster()}}
	state := newOrphanLaneState("hl-manual-eth-live")
	db := newLimitTestStateDB(t)
	var mu sync.RWMutex
	seedOrphanLaneRow(t, db, "hl-manual-eth-live", true)

	resting := true
	withStubbedLimitDeps(t,
		func(string, string, []int64, int64) (*HyperliquidLimitStatusResult, string, error) {
			if resting {
				return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
					{OID: 9001, Resting: limitTestBoolPtr(true), FilledSize: 0},
				}}, "", nil
			}
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: 9001, Resting: limitTestBoolPtr(false), FilledSize: 0.5, AvgPx: 2000, Count: 1},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			return nil, "", errors.New("hyperliquid 503")
		},
	)

	notifier, mock := newOrphanLaneNotifier()
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)
	if len(mock.dms) != 1 || !strings.Contains(mock.dms[0].content, "IS STILL RESTING") {
		t.Fatalf("the cancel failure must alert as a resting order, dms = %+v", mock.dms)
	}

	resting = false
	reconcilePendingLimitOrders(state, cfg, db, &mu, notifier, nil)
	if len(mock.dms) != 2 {
		t.Fatalf("the untracked position must reach the owner in the same throttle window, dms = %d", len(mock.dms))
	}
	if !strings.Contains(mock.dms[1].content, "UNTRACKED HYPERLIQUID POSITION") {
		t.Fatalf("the escalation DM must describe the untracked position, got: %s", mock.dms[1].content)
	}
}
