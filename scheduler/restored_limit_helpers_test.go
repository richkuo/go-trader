package main

import (
	"path/filepath"
	"sync"
	"testing"
)

func limitTestBoolPtr(b bool) *bool { return &b }

func newLimitTestStateDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
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

func withStubbedHLLiveExposure(t *testing.T, positions ...HLPosition) {
	t.Helper()
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xlimittest")
	orig := fetchHyperliquidStateFn
	snapshot := append([]HLPosition(nil), positions...)
	fetchHyperliquidStateFn = func(string) (float64, []HLPosition, error) {
		return 0, snapshot, nil
	}
	limitFillExposureAlerts.reset()
	t.Cleanup(func() { fetchHyperliquidStateFn = orig })
}

func withStubbedLimitDeps(t *testing.T, status func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error), cancel func(script, symbol string, oid int64) (*HyperliquidCancelOrderResult, string, error)) {
	t.Helper()
	origStatus := runHyperliquidLimitStatusFn
	origCancel := runHyperliquidCancelOrderFn
	origSync := syncHyperliquidProtection
	origRecorder := tradeRecorder
	runHyperliquidLimitStatusFn = limitTestEvidenceStatus(status)
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

func (sdb *StateDB) DeletePendingLimitOrder(id int64) error {
	_, err := sdb.db.Exec("DELETE FROM pending_limit_orders WHERE id = ? AND owner_kind = 'manual'", id)
	return err
}

func (sdb *StateDB) UpdatePendingLimitOrderFill(id int64, filledSize, avgFillPrice, fillFee float64) error {
	_, err := sdb.db.Exec(
		"UPDATE pending_limit_orders SET filled_size = ?, avg_fill_price = ?, fill_fee = ? WHERE id = ? AND owner_kind = 'manual'",
		filledSize, avgFillPrice, fillFee, id)
	return err
}

func (st *StateStore) DeletePendingLimitOrder(id int64) error {
	db, err := st.liveFile()
	if err != nil {
		return err
	}
	return db.DeletePendingLimitOrder(id)
}

func (st *StateStore) UpdatePendingLimitOrderFill(id int64, filledSize, avgFillPrice, fillFee float64) error {
	db, err := st.liveFile()
	if err != nil {
		return err
	}
	return db.UpdatePendingLimitOrderFill(id, filledSize, avgFillPrice, fillFee)
}

// limitTestEvidenceStatus completes a stubbed --limit-status answer with the
// order-state evidence the script reports for a consistent venue: the coin,
// a live or terminal state that matches the resting flag, the executed size,
// and a fill history that accounts for it. An entry that names its own
// OrderStatus is passed through unchanged.
func limitTestEvidenceStatus(status func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error)) func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
	if status == nil {
		return nil
	}
	return func(script, symbol string, oids []int64, sinceMs int64) (*HyperliquidLimitStatusResult, string, error) {
		res, stderr, err := status(script, symbol, oids, sinceMs)
		if res == nil {
			return res, stderr, err
		}
		out := *res
		out.Orders = append([]HyperliquidLimitOrderStatus(nil), res.Orders...)
		for i := range out.Orders {
			out.Orders[i] = limitTestEvidence(symbol, out.Orders[i])
		}
		return &out, stderr, err
	}
}

func limitTestEvidence(symbol string, st HyperliquidLimitOrderStatus) HyperliquidLimitOrderStatus {
	if st.OrderStatus != "" {
		return st
	}
	if st.Coin == "" {
		st.Coin = symbol
	}
	if st.Resting != nil {
		if *st.Resting {
			st.OrderStatus = "open"
		} else {
			st.OrderStatus = "canceled"
			st.Terminal = true
		}
	}
	if st.ExecutedSize == 0 {
		st.ExecutedSize = st.FilledSize
	}
	st.FillsComplete = st.FillsError == "" && st.StatusError == ""
	return st
}

// sealLimitRowThroughReconcile books a resting partial fill on the single
// queue row through the real settlement path, so the row carries a sealed
// watermark and the book holds the matching position.
func sealLimitRowThroughReconcile(t *testing.T, db *StateDB, sc StrategyConfig, state *AppState, filled, px, fee float64) {
	t.Helper()
	withStubbedHLLiveExposure(t, HLPosition{Coin: sc.Symbol, Size: hlSignedQty("long", filled)})
	withStubbedLimitDeps(t,
		func(_, _ string, oids []int64, _ int64) (*HyperliquidLimitStatusResult, string, error) {
			return &HyperliquidLimitStatusResult{Orders: []HyperliquidLimitOrderStatus{
				{OID: oids[0], Resting: limitTestBoolPtr(true), FilledSize: filled, AvgPx: px, Fee: fee, Count: 1},
			}}, "", nil
		},
		func(string, string, int64) (*HyperliquidCancelOrderResult, string, error) {
			t.Fatal("sealing a fill must not cancel")
			return nil, "", nil
		},
	)
	var mu sync.RWMutex
	cfg := &Config{Strategies: []StrategyConfig{sc}}
	reconcilePendingLimitOrders(state, cfg, openTestStore(t, db), &mu, nil, nil)
	orders, err := db.LoadPendingLimitOrders()
	if err != nil || len(orders) != 1 || orders[0].FilledSize != filled || !orders[0].fillSealed() {
		t.Fatalf("seal through settlement: %+v %v", orders, err)
	}
}

// sealLimitRowFixture seals one queue row at the given watermark through the
// real settlement transaction, saving the supplied books with it. The books
// must already hold the position the fill produced.
func sealLimitRowFixture(t *testing.T, db *StateDB, state *AppState, rowID int64, filled, px, fee float64) {
	t.Helper()
	orders, err := db.LoadPendingLimitOrders()
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	for _, o := range orders {
		if o.ID != rowID {
			continue
		}
		if err := singleFileStore(db).SettleManualLimitFills(state, []string{o.StrategyID}, []limitFillSeal{{Prev: o, FilledSize: filled, AvgFillPrice: px, FillFee: fee}}); err != nil {
			t.Fatalf("seal row %d: %v", rowID, err)
		}
		return
	}
	t.Fatalf("row %d not found", rowID)
}
