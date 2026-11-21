package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
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

const limitTestLotDecimals = 4

func provenLimitStatus(oid int64, coin, state string, orig, executed, avgPx, fee float64) HyperliquidLimitOrderStatus {
	raw := map[string]string{"live": "open", "filled": "filled", "cancelled": "canceled", "rejected": "rejected"}[state]
	resting := state == "live"
	lots := limitTestLotDecimals
	proven := true
	o, e := orig, executed
	count := 0
	if executed > 0 {
		count = 1
	}
	return HyperliquidLimitOrderStatus{
		OID: oid, Coin: coin, Resting: &resting, OrderStatus: raw, OrderState: state,
		OrigSize: &o, ExecutedSize: &e, SzDecimals: &lots,
		FilledSize: executed, AvgPx: avgPx, Fee: fee, Count: count, FillsProven: &proven,
	}
}

func limitStatusWithUnknownBook(st HyperliquidLimitOrderStatus) HyperliquidLimitOrderStatus {
	st.Resting = nil
	return st
}

func limitStatusWithUnprovenFills(st HyperliquidLimitOrderStatus, reason, kind string) HyperliquidLimitOrderStatus {
	proven := false
	st.FillsProven = &proven
	st.FillsError = reason
	st.FillsReason = kind
	st.FilledSize, st.AvgPx, st.Fee, st.Count = 0, 0, 0, 0
	return st
}

func seedLimitFillLineage(t *testing.T, db *StateDB, rowID int64, strategyID, symbol string, oid int64, qty, px float64, positionID string) {
	t.Helper()
	if rowID > 0 {
		stampLimitRowCommitProof(t, db, rowID)
	}
	if err := db.InsertTrade(strategyID, Trade{
		Timestamp: time.Now().UTC(), StrategyID: strategyID, Symbol: symbol, Side: "buy",
		Quantity: qty, Price: px, Value: qty * px, TradeType: "perps", PositionID: positionID,
		ExchangeOrderID: fmt.Sprintf("%d", oid), Manual: true,
	}); err != nil {
		t.Fatalf("seed limit entry trade: %v", err)
	}
}

func stampLimitRowCommitProof(t *testing.T, db *StateDB, rowID int64) {
	t.Helper()
	rows, err := db.LoadPendingLimitOrders()
	if err != nil {
		t.Fatalf("load row to stamp: %v", err)
	}
	for _, o := range rows {
		if o.ID != rowID {
			continue
		}
		binding := limitFillProofBinding(o.ID, o.OrderOID, o.storedOwner, o.Symbol, o.Side, o.FilledSize, o.AvgFillPrice, o.FillFee)
		if _, err := db.db.Exec("UPDATE pending_limit_orders SET fill_proof_version = ?, fill_proof_binding = ? WHERE id = ?", limitFillProofVersionAtomic, binding, rowID); err != nil {
			t.Fatalf("stamp atomic watermark: %v", err)
		}
		return
	}
	t.Fatalf("row %d not found to stamp", rowID)
}
