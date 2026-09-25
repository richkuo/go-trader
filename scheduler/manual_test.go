package main

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestApplyManualAction99PercentPartialNotCollapsedToFull(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-manual-eth-live": {
				ID:       "hl-manual-eth-live",
				Platform: "hyperliquid",
				Type:     "manual",
				Positions: map[string]*Position{
					"ETH": {
						Symbol:          "ETH",
						Quantity:        0.5,
						InitialQuantity: 0.5,
						AvgCost:         2000,
						Side:            "long",
						Multiplier:      1,
						OwnerStrategyID: "hl-manual-eth-live",
					},
				},
				Cash: 9000,
			},
		},
	}
	scByID := map[string]StrategyConfig{
		"hl-manual-eth-live": {ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 10},
	}

	origRecorder := tradeRecorder
	tradeRecorder = func(_ string, _ Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()

	a := PendingManualAction{
		StrategyID:  "hl-manual-eth-live",
		Action:      "close",
		Symbol:      "ETH",
		Side:        "sell",
		Quantity:    0.495,
		FillPrice:   2100,
		RealizedPnL: 49.0,
		IsFullClose: false,
		CreatedAt:   time.Now().UTC(),
	}
	if err := applyManualAction(state, nil, scByID, a); err != nil {
		t.Fatalf("99%% partial close: %v", err)
	}

	pos := state.Strategies["hl-manual-eth-live"].Positions["ETH"]
	if pos == nil {
		t.Fatal("99%% partial close should leave the position open with dust qty (regression: 0.99 tolerance was collapsing this to full)")
	}
	expectedQty := 0.5 - 0.495
	if abs(pos.Quantity-expectedQty) > 1e-9 {
		t.Errorf("residual qty = %g, want %g", pos.Quantity, expectedQty)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestDrainPendingManualActionsAlerts(t *testing.T) {
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer db.Close()

	openID := "hl-manual-eth-live"
	otherID := "hl-manual-btc-live"
	state := &AppState{
		Strategies: map[string]*StrategyState{
			openID:  {ID: openID, Platform: "hyperliquid", Type: "manual", Positions: map[string]*Position{}, Cash: 10000},
			otherID: {ID: otherID, Platform: "hyperliquid", Type: "manual", Positions: map[string]*Position{}, Cash: 10000},
		},
	}
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: openID, Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 10},
		{ID: otherID, Type: "manual", Platform: "hyperliquid", Symbol: "BTC", Leverage: 10},
	}}

	origRecorder := tradeRecorder
	tradeRecorder = func(_ string, _ Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()

	now := time.Now().UTC()
	_ = db.InsertPendingManualAction(PendingManualAction{StrategyID: otherID, Action: "close", Symbol: "DOGE", Side: "long", Quantity: 1, FillPrice: 0.1, IsFullClose: true, CreatedAt: now})
	_ = db.InsertPendingManualAction(PendingManualAction{StrategyID: openID, Action: "open", Symbol: "ETH", Side: "long", Quantity: 0.5, FillPrice: 2000, FillFee: 0.7, EntryATR: 50, CreatedAt: now})
	_ = db.InsertPendingManualAction(PendingManualAction{StrategyID: openID, Action: "close", Symbol: "ETH", Side: "long", Quantity: 0.5, FillPrice: 2100, FillFee: 0.7, RealizedPnL: 49.3, IsFullClose: true, CreatedAt: now})
	_ = db.InsertPendingManualAction(PendingManualAction{StrategyID: otherID, Action: "open", Symbol: "BTC", Side: "short", Quantity: 0.01, FillPrice: 60000, FillFee: 0.3, EntryATR: 500, CreatedAt: now})

	alerts, _ := drainPendingManualActions(state, cfg, openTestStore(t, db))

	if len(alerts) != 2 {
		t.Fatalf("expected 2 strategy alerts, got %d", len(alerts))
	}
	byID := map[string]manualAlert{}
	for _, a := range alerts {
		byID[a.sc.ID] = a
	}
	if got := byID[openID].trades; got != 2 {
		t.Errorf("%s alert trades = %d, want 2 (open + close)", openID, got)
	}
	if got := byID[otherID].trades; got != 1 {
		t.Errorf("%s alert trades = %d, want 1 (failed DOGE close excluded)", otherID, got)
	}
	for _, a := range alerts {
		if a.trades > len(a.ss.TradeHistory) {
			t.Errorf("%s alert trades=%d exceeds TradeHistory len=%d", a.sc.ID, a.trades, len(a.ss.TradeHistory))
		}
	}

	remaining, _ := db.LoadPendingManualActions()
	if len(remaining) != 1 {
		t.Fatalf("queue after drain = %d rows, want 1 (the failed DOGE close survives acknowledgement of the others)", len(remaining))
	}
	if remaining[0].StrategyID != otherID || remaining[0].Symbol != "DOGE" {
		t.Errorf("surviving row = %s/%s, want the failed %s/DOGE close", remaining[0].StrategyID, remaining[0].Symbol, otherID)
	}
}

func TestApplyManualAction_PerpsForceCloseFull(t *testing.T) {
	stratID := "hl-tcross-eth-live"
	now := time.Now().UTC()
	state := &AppState{
		Strategies: map[string]*StrategyState{
			stratID: {
				ID:       stratID,
				Type:     "perps",
				Platform: "hyperliquid",
				Cash:     1000,
				RiskState: RiskState{
					DailyPnLDate:      todayUTC(),
					DailyPnL:          10,
					ConsecutiveLosses: 3,
				},
				Positions: map[string]*Position{"ETH": {
					Symbol:          "ETH",
					Quantity:        0.4,
					InitialQuantity: 0.4,
					AvgCost:         2000,
					Side:            "long",
					Multiplier:      1,
					Leverage:        2,
					OwnerStrategyID: stratID,
					OpenedAt:        now.Add(-time.Hour),
				}},
			},
		},
	}
	scByID := map[string]StrategyConfig{
		stratID: {
			ID:       stratID,
			Type:     "perps",
			Platform: "hyperliquid",
			Args:     []string{"tcross", "ETH", "1h", "--mode=live"},
		},
	}

	if err := applyManualAction(state, nil, scByID, PendingManualAction{
		StrategyID:      stratID,
		Action:          "close",
		Symbol:          "ETH",
		Side:            "sell",
		Quantity:        0.4,
		FillPrice:       2100,
		FillFee:         1.25,
		ExchangeOrderID: "98765",
		RealizedPnL:     38.75,
		IsFullClose:     true,
		CreatedAt:       now,
	}); err != nil {
		t.Fatalf("applyManualAction perps close: %v", err)
	}

	ss := state.Strategies[stratID]
	if pos := ss.Positions["ETH"]; pos != nil {
		t.Fatalf("position still open after full force-close: %+v", pos)
	}
	if len(ss.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len=%d, want 1", len(ss.TradeHistory))
	}
	trade := ss.TradeHistory[0]
	if trade.Details != "force close ETH @ $2100.0000 | PnL=$38.75" {
		t.Errorf("trade details = %q", trade.Details)
	}
	if trade.Manual {
		t.Error("perps force-close trade Manual=true, want false")
	}
	if trade.ExchangeFee != 1.25 || trade.RealizedPnL != 40 || !trade.PnLGross {
		t.Errorf("trade fee/pnl/gross = %g/%g/%v, want 1.25/40/true", trade.ExchangeFee, trade.RealizedPnL, trade.PnLGross)
	}
	if ss.Cash != 1038.75 {
		t.Errorf("cash = %g, want 1038.75", ss.Cash)
	}
	if ss.RiskState.DailyPnL != 48.75 || ss.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("risk state = daily %.2f losses %d, want daily 48.75 losses 0", ss.RiskState.DailyPnL, ss.RiskState.ConsecutiveLosses)
	}
	if len(ss.ClosedPositions) != 1 || ss.ClosedPositions[0].CloseReason != "force_close" {
		t.Fatalf("closed positions = %+v, want force_close", ss.ClosedPositions)
	}
}

func TestApplyManualAction_PerpsForceCloseLossUpdatesRiskState(t *testing.T) {
	stratID := "hl-tcross-eth-live"
	now := time.Now().UTC()
	state := &AppState{
		Strategies: map[string]*StrategyState{
			stratID: {
				ID:       stratID,
				Type:     "perps",
				Platform: "hyperliquid",
				Cash:     1000,
				RiskState: RiskState{
					DailyPnLDate: todayUTC(),
				},
				Positions: map[string]*Position{"ETH": {
					Symbol:          "ETH",
					Quantity:        0.4,
					InitialQuantity: 0.4,
					AvgCost:         2000,
					Side:            "long",
					Multiplier:      1,
					Leverage:        2,
					OwnerStrategyID: stratID,
					OpenedAt:        now.Add(-time.Hour),
				}},
			},
		},
	}
	scByID := map[string]StrategyConfig{
		stratID: {ID: stratID, Type: "perps", Platform: "hyperliquid", Args: []string{"tcross", "ETH", "1h", "--mode=live"}},
	}

	if err := applyManualAction(state, nil, scByID, PendingManualAction{
		StrategyID:  stratID,
		Action:      "close",
		Symbol:      "ETH",
		Side:        "sell",
		Quantity:    0.4,
		FillPrice:   1900,
		FillFee:     1.25,
		RealizedPnL: -41.25,
		IsFullClose: true,
		CreatedAt:   now,
	}); err != nil {
		t.Fatalf("applyManualAction perps loss close: %v", err)
	}

	ss := state.Strategies[stratID]
	if ss.RiskState.DailyPnL != -41.25 || ss.RiskState.ConsecutiveLosses != 1 {
		t.Fatalf("risk state = daily %.2f losses %d, want daily -41.25 losses 1", ss.RiskState.DailyPnL, ss.RiskState.ConsecutiveLosses)
	}
}

func TestApplyManualAction_PerpsPartialForceCloseClearsCanceledProtection(t *testing.T) {
	stratID := "hl-tcross-eth-live"
	now := time.Now().UTC()
	state := &AppState{
		Strategies: map[string]*StrategyState{
			stratID: {
				ID:       stratID,
				Type:     "perps",
				Platform: "hyperliquid",
				Cash:     1000,
				RiskState: RiskState{
					DailyPnLDate: todayUTC(),
				},
				Positions: map[string]*Position{"ETH": {
					Symbol:            "ETH",
					Quantity:          1.0,
					InitialQuantity:   1.0,
					AvgCost:           2000,
					Side:              "long",
					Multiplier:        1,
					Leverage:          2,
					OwnerStrategyID:   stratID,
					StopLossOID:       111,
					StopLossTriggerPx: 1900,
					TPOIDs:            []int64{222, 333},
					TPArmedTiers:      []bool{true, true},
					OpenedAt:          now.Add(-time.Hour),
				}},
			},
		},
	}
	scByID := map[string]StrategyConfig{
		stratID: {ID: stratID, Type: "perps", Platform: "hyperliquid", Args: []string{"tcross", "ETH", "1h", "--mode=live"}},
	}

	if err := applyManualAction(state, nil, scByID, PendingManualAction{
		StrategyID:  stratID,
		Action:      "close",
		Symbol:      "ETH",
		Side:        "sell",
		Quantity:    0.5,
		FillPrice:   2100,
		FillFee:     1.25,
		RealizedPnL: 48.75,
		IsFullClose: false,
		StopLossOID: 111,
		TPOIDs:      []int64{222, 333},
		CreatedAt:   now,
	}); err != nil {
		t.Fatalf("applyManualAction partial perps close: %v", err)
	}

	pos := state.Strategies[stratID].Positions["ETH"]
	if pos == nil {
		t.Fatal("position deleted, want residual")
	}
	if pos.Quantity != 0.5 {
		t.Errorf("quantity = %g, want 0.5", pos.Quantity)
	}
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Errorf("SL state = oid %d trigger %.2f, want cleared", pos.StopLossOID, pos.StopLossTriggerPx)
	}
	if !reflect.DeepEqual(pos.TPOIDs, []int64{0, 0}) {
		t.Errorf("TPOIDs = %v, want [0 0]", pos.TPOIDs)
	}
	if !reflect.DeepEqual(pos.TPArmedTiers, []bool{false, false}) {
		t.Errorf("TPArmedTiers = %v, want [false false] so protection sync re-arms canceled tiers", pos.TPArmedTiers)
	}
}

func TestApplyManualAction_PerpsForceCloseDuplicateOIDSkipsPartial(t *testing.T) {
	stratID := "hl-tcross-eth-live"
	now := time.Now().UTC()
	state := &AppState{
		Strategies: map[string]*StrategyState{
			stratID: {
				ID:       stratID,
				Type:     "perps",
				Platform: "hyperliquid",
				Cash:     1048.75,
				RiskState: RiskState{
					DailyPnLDate:      todayUTC(),
					DailyPnL:          48.75,
					ConsecutiveLosses: 0,
				},
				Positions: map[string]*Position{"ETH": {
					Symbol:            "ETH",
					Quantity:          0.5,
					InitialQuantity:   1.0,
					AvgCost:           2000,
					Side:              "long",
					Multiplier:        1,
					Leverage:          2,
					OwnerStrategyID:   stratID,
					StopLossOID:       111,
					StopLossTriggerPx: 1900,
					TPOIDs:            []int64{222},
					TPArmedTiers:      []bool{true},
					OpenedAt:          now.Add(-time.Hour),
				}},
				TradeHistory: []Trade{{
					Timestamp:       now,
					StrategyID:      stratID,
					Symbol:          "ETH",
					Side:            "sell",
					Quantity:        0.5,
					Price:           2100,
					TradeType:       "perps",
					ExchangeOrderID: "98765",
					IsClose:         true,
				}},
			},
		},
	}
	scByID := map[string]StrategyConfig{
		stratID: {ID: stratID, Type: "perps", Platform: "hyperliquid", Args: []string{"tcross", "ETH", "1h", "--mode=live"}},
	}

	if err := applyManualAction(state, nil, scByID, PendingManualAction{
		StrategyID:      stratID,
		Action:          "close",
		Symbol:          "ETH",
		Side:            "sell",
		Quantity:        0.5,
		FillPrice:       2100,
		FillFee:         1.25,
		ExchangeOrderID: "98765",
		RealizedPnL:     48.75,
		IsFullClose:     false,
		StopLossOID:     111,
		TPOIDs:          []int64{222},
		CreatedAt:       now,
	}); err != nil {
		t.Fatalf("applyManualAction duplicate partial close: %v", err)
	}

	ss := state.Strategies[stratID]
	if ss.Cash != 1048.75 || ss.RiskState.DailyPnL != 48.75 || len(ss.TradeHistory) != 1 {
		t.Fatalf("duplicate mutated accounting: cash %.2f daily %.2f trades %d", ss.Cash, ss.RiskState.DailyPnL, len(ss.TradeHistory))
	}
	pos := ss.Positions["ETH"]
	if pos == nil || pos.Quantity != 0.5 {
		t.Fatalf("position after duplicate = %+v, want qty 0.5", pos)
	}
	if pos.StopLossOID != 0 || !reflect.DeepEqual(pos.TPOIDs, []int64{0}) || !reflect.DeepEqual(pos.TPArmedTiers, []bool{false}) {
		t.Fatalf("canceled protection not cleared on duplicate: sl=%d tp=%v armed=%v", pos.StopLossOID, pos.TPOIDs, pos.TPArmedTiers)
	}
}

func TestManualActionLockPreventsCrossProcessDoubleFire(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "test-secret")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer db.Close()

	cfg := &Config{
		DBFile: dbPath,
		Strategies: []StrategyConfig{
			{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
				Script: "shared_scripts/check_hyperliquid.py",
				Args:   []string{"hold", "ETH", "1h", "--mode=live"}, Capital: 1000, Leverage: 2},
		},
	}
	pos := &Position{
		Symbol: "ETH", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000,
		EntryATR: 50, Side: "long", Multiplier: 1, Leverage: 2,
		OwnerStrategyID: "hl-manual-eth", StopLossOID: 111, StopLossTriggerPx: 1900,
		OpenedAt: time.Now().UTC().Add(-time.Hour),
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-manual-eth": {ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid",
			Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{"ETH": pos}},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	manualSC, err := lookupManualStrategy(cfg, "hl-manual-eth")
	if err != nil {
		t.Fatalf("lookup manual: %v", err)
	}

	var aFired, bFired int32
	enteredSubmit := make(chan struct{})
	releaseSubmit := make(chan struct{})

	depsA := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
	depsA.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		atomic.AddInt32(&aFired, 1)
		close(enteredSubmit)
		<-releaseSubmit
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: size, OID: 4242, Fee: 1.0}}}, "", nil
	}

	depsB := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
	depsB.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		atomic.AddInt32(&bFired, 1)
		return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: size, OID: 5252, Fee: 1.0}}}, "", nil
	}

	closeIn := manualCloseInputs{StrategyID: "hl-manual-eth", Qty: 0.2}

	aDone := make(chan error, 1)
	go func() {
		_, e := manualCloseCore(depsA, manualSC, closeIn)
		aDone <- e
	}()

	select {
	case <-enteredSubmit:
	case e := <-aDone:
		t.Fatalf("first close returned before reaching the venue submit: %v", e)
	case <-time.After(3 * time.Second):
		t.Fatal("first close did not reach the venue submit")
	}

	bDone := make(chan error, 1)
	go func() {
		_, e := manualCloseCore(depsB, manualSC, closeIn)
		bDone <- e
	}()

	select {
	case e := <-bDone:
		t.Fatalf("second close completed while the first held the manual-action lock (err=%v, bFired=%d) — cross-process double-fire", e, atomic.LoadInt32(&bFired))
	case <-time.After(400 * time.Millisecond):
	}
	if n := atomic.LoadInt32(&bFired); n != 0 {
		t.Fatalf("second close fired on-chain %d time(s) while the first held the lock — double-fire", n)
	}

	close(releaseSubmit)

	if e := <-aDone; e != nil {
		t.Fatalf("first close errored: %v", e)
	}
	select {
	case e := <-bDone:
		if e == nil {
			t.Fatalf("second close should have been refused by the pending-row guard once the lock was released, got nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second close did not return after the lock was released")
	}

	if got := atomic.LoadInt32(&aFired); got != 1 {
		t.Fatalf("first close venue calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&bFired); got != 0 {
		t.Fatalf("second close venue calls = %d, want 0 (guard must refuse it)", got)
	}
}

func TestRunForceCloseQueuesCanceledProtectionOnSoleOwnerUnderfill(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "test-secret")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	stratID := "hl-tcross-eth-live"
	state := &AppState{
		Strategies: map[string]*StrategyState{
			stratID: {
				ID:             stratID,
				Type:           "perps",
				Platform:       "hyperliquid",
				Cash:           1000,
				InitialCapital: 1000,
				Positions: map[string]*Position{"ETH": {
					Symbol:            "ETH",
					Quantity:          1.0,
					InitialQuantity:   1.0,
					AvgCost:           2000,
					Side:              "long",
					Multiplier:        1,
					Leverage:          2,
					OwnerStrategyID:   stratID,
					StopLossOID:       111,
					StopLossTriggerPx: 1900,
					TPOIDs:            []int64{222, 333},
					TPArmedTiers:      []bool{true, true},
					OpenedAt:          time.Now().UTC().Add(-time.Hour),
				}},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		db.Close()
		t.Fatalf("SaveState: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	cfgPath := writeTestConfig(t, dir, fmt.Sprintf(`{
		"db_file": %q,
		"strategies": [{
			"id": %q,
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["tcross", "ETH", "1h", "--mode=live"],
			"capital": 1000,
			"leverage": 2
		}]
	}`, dbPath, stratID))

	var gotPartialNil bool
	var gotCancelOIDs []int64
	closer := func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
		gotPartialNil = partialSz == nil
		gotCancelOIDs = append([]int64(nil), cancelOIDs...)
		return &HyperliquidCloseResult{
			Close: &HyperliquidClose{
				Symbol: symbol,
				Fill:   &HyperliquidCloseFill{AvgPx: 2100, TotalSz: 0.5, OID: 98765, Fee: 1.25},
			},
			Platform:                    "hyperliquid",
			CancelStopLossSucceeded:     true,
			CancelStopLossSucceededOIDs: []int64{111, 222, 333},
		}, nil
	}

	rc := runForceCloseWithCloser([]string{"--config", cfgPath, stratID}, closer)
	if rc != 0 {
		t.Fatalf("runForceCloseWithCloser rc=%d, want 0", rc)
	}
	if !gotPartialNil {
		t.Fatal("partialSz was non-nil for sole-owner full intent")
	}
	if !reflect.DeepEqual(gotCancelOIDs, []int64{111, 222, 333}) {
		t.Fatalf("cancel OIDs = %v, want [111 222 333]", gotCancelOIDs)
	}

	db2, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()
	actions, err := db2.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("queued actions len=%d, want 1", len(actions))
	}
	a := actions[0]
	if a.IsFullClose {
		t.Fatalf("queued full close = true after under-fill, want false")
	}
	if a.StopLossOID != 111 || !reflect.DeepEqual(a.TPOIDs, []int64{222, 333}) {
		t.Fatalf("queued canceled protection = sl %d tp %v, want sl 111 tp [222 333]", a.StopLossOID, a.TPOIDs)
	}
}

func TestRunForceCloseQueuesActualFillQuantity(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "test-secret")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	stratID := "hl-tcross-eth-live"
	state := &AppState{
		Strategies: map[string]*StrategyState{
			stratID: {
				ID:             stratID,
				Type:           "perps",
				Platform:       "hyperliquid",
				Cash:           1000,
				InitialCapital: 1000,
				Positions: map[string]*Position{"ETH": {
					Symbol:          "ETH",
					Quantity:        1.0,
					InitialQuantity: 1.0,
					AvgCost:         2000,
					Side:            "long",
					Multiplier:      1,
					Leverage:        2,
					OwnerStrategyID: stratID,
					OpenedAt:        time.Now().UTC().Add(-time.Hour),
				}},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		db.Close()
		t.Fatalf("SaveState: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	cfgPath := writeTestConfig(t, dir, fmt.Sprintf(`{
		"db_file": %q,
		"strategies": [{
			"id": %q,
			"type": "perps",
			"platform": "hyperliquid",
			"script": "shared_scripts/check_hyperliquid.py",
			"args": ["tcross", "ETH", "1h", "--mode=live"],
			"capital": 1000,
			"leverage": 2
		}]
	}`, dbPath, stratID))

	var gotPartial float64
	closer := func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
		if partialSz == nil {
			t.Fatal("partialSz = nil, want sized partial close")
		}
		gotPartial = *partialSz
		return &HyperliquidCloseResult{
			Close: &HyperliquidClose{
				Symbol: symbol,
				Fill:   &HyperliquidCloseFill{AvgPx: 2100, TotalSz: 0.5, OID: 98765, Fee: 1.25},
			},
			Platform: "hyperliquid",
		}, nil
	}

	rc := runForceCloseWithCloser([]string{"--config", cfgPath, "--qty", "0.8", stratID}, closer)
	if rc != 0 {
		t.Fatalf("runForceCloseWithCloser rc=%d, want 0", rc)
	}
	if gotPartial != 0.8 {
		t.Fatalf("partial close size = %g, want 0.8", gotPartial)
	}

	db2, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()
	actions, err := db2.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("queued actions len=%d, want 1", len(actions))
	}
	a := actions[0]
	if a.Quantity != 0.5 || a.IsFullClose {
		t.Fatalf("queued quantity/full = %g/%v, want 0.5/false", a.Quantity, a.IsFullClose)
	}
	if a.RealizedPnL != 48.75 {
		t.Errorf("queued realized PnL = %g, want 48.75", a.RealizedPnL)
	}
}

func TestResolveManualOpenOrderSize(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-manual-eth-live",
		Platform: "hyperliquid",
		Type:     "manual",
		Symbol:   "ETH",
		Leverage: 10,
	}

	t.Run("--size bypasses fetch", func(t *testing.T) {
		called := false
		fetch := func(coins []string) (map[string]float64, error) {
			called = true
			return nil, errors.New("should not be called")
		}
		qty, mark, err := resolveManualOpenOrderSize(sc, 0.5, 0, 0, fetch)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if qty != 0.5 || mark != 0 || called {
			t.Errorf("got qty=%g mark=%g called=%v; want qty=0.5 mark=0 called=false", qty, mark, called)
		}
	})

	t.Run("--margin resolves with fetched mark", func(t *testing.T) {
		fetch := func(coins []string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		}
		qty, mark, err := resolveManualOpenOrderSize(sc, 0, 0, 50, fetch)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fmt.Sprintf("%.6f", qty) != "0.250000" || mark != 2000 {
			t.Errorf("got qty=%g mark=%g; want qty=0.25 mark=2000", qty, mark)
		}
	})

	t.Run("--notional resolves with fetched mark", func(t *testing.T) {
		fetch := func(coins []string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		}
		qty, mark, err := resolveManualOpenOrderSize(sc, 0, 1000, 0, fetch)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fmt.Sprintf("%.6f", qty) != "0.500000" || mark != 2000 {
			t.Errorf("got qty=%g mark=%g; want qty=0.5 mark=2000", qty, mark)
		}
	})

	t.Run("fetch error surfaces", func(t *testing.T) {
		fetch := func(coins []string) (map[string]float64, error) {
			return nil, errors.New("network down")
		}
		qty, _, err := resolveManualOpenOrderSize(sc, 0, 0, 50, fetch)
		if err == nil || qty != 0 {
			t.Errorf("got qty=%g err=%v; want non-nil err", qty, err)
		}
		if !strings.Contains(err.Error(), "network down") {
			t.Errorf("expected wrapped fetch error, got: %v", err)
		}
	})

	t.Run("missing coin in mark map errors", func(t *testing.T) {
		fetch := func(coins []string) (map[string]float64, error) {
			return map[string]float64{}, nil
		}
		_, _, err := resolveManualOpenOrderSize(sc, 0, 0, 50, fetch)
		if err == nil || !strings.Contains(err.Error(), "missing or non-positive") {
			t.Errorf("expected missing-mark error, got: %v", err)
		}
	})

	t.Run("zero qty errors (e.g. leverage=0 with --margin)", func(t *testing.T) {
		scNoLev := sc
		scNoLev.Leverage = 0
		fetch := func(coins []string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		}
		_, _, err := resolveManualOpenOrderSize(scNoLev, 0, 0, 50, fetch)
		if err == nil || !strings.Contains(err.Error(), "resolved size is zero") {
			t.Errorf("expected zero-size error, got: %v", err)
		}
	})

	t.Run("non-hl strategy errors", func(t *testing.T) {
		scOKX := sc
		scOKX.Platform = "okx"
		fetch := func(coins []string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		}
		_, _, err := resolveManualOpenOrderSize(scOKX, 0, 0, 50, fetch)
		if err == nil || !strings.Contains(err.Error(), "cannot determine HL coin") {
			t.Errorf("expected coin-resolution error, got: %v", err)
		}
	})
}

func TestOperatorSharedCloseFloorGatesBothCores(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "test-secret")
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xoperator")

	flat := []HLPosition{{Coin: "ETH", Size: 0.002}}
	busy := []HLPosition{{Coin: "ETH", Size: 0.5}}

	cases := []struct {
		name          string
		soleOwner     bool
		posQty        float64
		peerBookQty   float64
		positions     []HLPosition
		positionsErr  error
		midsErr       error
		holdReason    string
		wantFired     int
		wantFullClose bool
		wantErrPart   string
	}{
		{name: "flat peers escalate to a whole-position close", posQty: 0.002,
			positions: flat, wantFired: 1, wantFullClose: true},
		{name: "a stored venue-rejected hold does not change the escalation", posQty: 0.002,
			positions: flat, holdReason: hlSharedCloseHoldVenueReject, wantFired: 1, wantFullClose: true},
		{name: "a peer holding on-chain refuses", posQty: 0.002,
			positions: busy, wantErrPart: "no close order sent"},
		{name: "a peer holding in its own book refuses", posQty: 0.002,
			peerBookQty: 0.003, positions: flat, wantErrPart: "0.003000"},
		{name: "an unreadable account refuses", posQty: 0.002,
			positionsErr: fmt.Errorf("clearinghouseState timeout"), wantErrPart: "clearinghouseState timeout"},
		{name: "a single-owner coin sends the order it sends today", soleOwner: true, posQty: 0.002,
			positions: flat, wantFired: 1, wantFullClose: true},
		{name: "a shared coin above the gate sends the sized order", posQty: 0.4,
			positions: flat, wantFired: 1, wantFullClose: false},
		{name: "an unreadable mark sends the sized order it sends today", posQty: 0.002,
			midsErr: fmt.Errorf("allMids timeout"), positions: flat, wantFired: 1, wantFullClose: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, core := range []string{"manual-close", "force-close"} {
				t.Run(core, func(t *testing.T) {
					dir := t.TempDir()
					dbPath := filepath.Join(dir, "state.db")
					db, err := OpenStateDB(dbPath)
					if err != nil {
						t.Fatalf("OpenStateDB: %v", err)
					}
					defer db.Close()

					manualSC := StrategyConfig{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
						Script: "shared_scripts/check_hyperliquid.py",
						Args:   []string{"hold", "ETH", "1h", "--mode=live"}, Capital: 1000, Leverage: 2}
					perpsSC := StrategyConfig{ID: "hl-perps-eth", Type: "perps", Platform: "hyperliquid",
						Script: "shared_scripts/check_hyperliquid.py",
						Args:   []string{"tcross", "ETH", "1h", "--mode=live"}, Capital: 1000, Leverage: 2}
					subject, peer := perpsSC, manualSC
					if core == "manual-close" {
						subject, peer = manualSC, perpsSC
					}
					cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{subject, peer}}
					if tc.soleOwner {
						cfg.Strategies = []StrategyConfig{subject}
					}
					strategyID := subject.ID
					peerID := peer.ID
					mkPos := func(owner string, qty float64) *Position {
						return &Position{
							Symbol: "ETH", Quantity: qty, InitialQuantity: qty, AvgCost: 2000,
							Side: "long", Multiplier: 1, Leverage: 2, OwnerStrategyID: owner,
							SharedCloseHoldReason: tc.holdReason,
							OpenedAt:              time.Now().UTC().Add(-time.Hour),
						}
					}
					state := &AppState{Strategies: map[string]*StrategyState{
						strategyID: {ID: strategyID, Type: subject.Type, Platform: "hyperliquid",
							Cash: 1000, InitialCapital: 1000,
							Positions: map[string]*Position{"ETH": mkPos(strategyID, tc.posQty)}},
					}}
					if tc.peerBookQty > 0 {
						state.Strategies[peerID] = &StrategyState{
							ID: peerID, Type: peer.Type, Platform: "hyperliquid",
							Cash: 1000, InitialCapital: 1000,
							Positions: map[string]*Position{"ETH": mkPos(peerID, tc.peerBookQty)},
						}
					}
					if err := db.SaveState(state); err != nil {
						t.Fatalf("SaveState: %v", err)
					}

					d := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
					d.fetchMids = func(coins []string) (map[string]float64, error) {
						if tc.midsErr != nil {
							return nil, tc.midsErr
						}
						return map[string]float64{"ETH": 2000}, nil
					}
					accountReads := 0
					d.fetchPositions = func(addr string) ([]HLPosition, error) {
						accountReads++
						if tc.positionsErr != nil {
							return nil, tc.positionsErr
						}
						return tc.positions, nil
					}
					fired := 0
					gotFullClose := false
					d.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
						fired++
						gotFullClose = closeMode == hlCloseModeWhole
						return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: size, OID: 7, Fee: 0.01}}}, "", nil
					}
					d.closer = func(symbol string, partialSz *float64, cancelOIDs []int64) (*HyperliquidCloseResult, error) {
						fired++
						gotFullClose = partialSz == nil
						return &HyperliquidCloseResult{Close: &HyperliquidClose{Fill: &HyperliquidCloseFill{AvgPx: 2000, TotalSz: tc.posQty, OID: 7, Fee: 0.01}}}, nil
					}

					var coreErr error
					if core == "manual-close" {
						sc, lookupErr := lookupManualStrategy(cfg, strategyID)
						if lookupErr != nil {
							t.Fatalf("lookup: %v", lookupErr)
						}
						_, coreErr = manualCloseCore(d, sc, manualCloseInputs{StrategyID: strategyID})
					} else {
						sc, sym, lookupErr := lookupForceCloseStrategy(cfg, strategyID)
						if lookupErr != nil {
							t.Fatalf("lookup: %v", lookupErr)
						}
						_, coreErr = forceCloseCore(d, sc, sym, forceCloseInputs{StrategyID: strategyID})
					}

					if tc.wantErrPart != "" {
						if coreErr == nil || !strings.Contains(coreErr.Error(), tc.wantErrPart) {
							t.Fatalf("err = %v, want a refusal containing %q", coreErr, tc.wantErrPart)
						}
						if fired != 0 {
							t.Fatalf("venue calls = %d on a refusal, want 0", fired)
						}
						return
					}
					if coreErr != nil {
						t.Fatalf("unexpected error: %v", coreErr)
					}
					if fired != tc.wantFired {
						t.Fatalf("venue calls = %d, want %d", fired, tc.wantFired)
					}
					if gotFullClose != tc.wantFullClose {
						t.Fatalf("whole-position close = %v, want %v", gotFullClose, tc.wantFullClose)
					}
					wantSizingReads := 0
					if core == "manual-close" {
						wantSizingReads = 1
					}
					if tc.midsErr != nil && accountReads != wantSizingReads {
						t.Fatalf("on-chain account reads = %d on an unreadable mark, want %d (the floor reads none; only the manual-close sizing reads once)", accountReads, wantSizingReads)
					}
				})
			}
		})
	}
}

func TestManualCloseBooksTheVenueFillNotTheBookQuantity(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "test-secret")
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xoperator")

	const bookQty = 0.002

	cases := []struct {
		name        string
		fillSz      float64
		fillErr     bool
		wantQty     float64
		wantFee     float64
		wantFull    bool
		wantErrPart string
	}{
		{name: "a short fill books only what the venue filled", fillSz: 0.0012,
			wantQty: 0.0012, wantFee: 0.01, wantFull: false},
		{name: "a fill equal to the book books the book quantity", fillSz: bookQty,
			wantQty: bookQty, wantFee: 0.01, wantFull: true},
		{name: "a fill above the book attributes only the virtual quantity", fillSz: 0.004,
			wantQty: bookQty, wantFee: 0.005, wantFull: true},
		{name: "a zero fill errors and books nothing", fillErr: true,
			wantErrPart: "no confirmed fill"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatalf("OpenStateDB: %v", err)
			}
			defer db.Close()

			subject := StrategyConfig{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
				Script: "shared_scripts/check_hyperliquid.py",
				Args:   []string{"hold", "ETH", "1h", "--mode=live"}, Capital: 1000, Leverage: 2}
			peer := StrategyConfig{ID: "hl-perps-eth", Type: "perps", Platform: "hyperliquid",
				Script: "shared_scripts/check_hyperliquid.py",
				Args:   []string{"tcross", "ETH", "1h", "--mode=live"}, Capital: 1000, Leverage: 2}
			cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{subject, peer}}

			state := &AppState{Strategies: map[string]*StrategyState{
				subject.ID: {ID: subject.ID, Type: subject.Type, Platform: "hyperliquid",
					Cash: 1000, InitialCapital: 1000,
					Positions: map[string]*Position{"ETH": {
						Symbol: "ETH", Quantity: bookQty, InitialQuantity: bookQty, AvgCost: 2000,
						Side: "long", Multiplier: 1, Leverage: 2, OwnerStrategyID: subject.ID,
						StopLossOID: 5150, OpenedAt: time.Now().UTC().Add(-time.Hour),
					}}},
			}}
			if err := db.SaveState(state); err != nil {
				t.Fatalf("SaveState: %v", err)
			}

			d := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
			d.fetchMids = func(coins []string) (map[string]float64, error) {
				return map[string]float64{"ETH": 2000}, nil
			}
			d.fetchPositions = func(addr string) ([]HLPosition, error) {
				return []HLPosition{{Coin: "ETH", Size: bookQty}}, nil
			}
			gotFullClose := false
			d.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				gotFullClose = closeMode == hlCloseModeWhole
				if tc.fillErr {
					return &HyperliquidExecuteResult{Error: "exchange returned no confirmed fill (sz=0.00000000 px=0.00000000)"}, "", nil
				}
				return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{
					Fill: &HyperliquidFill{AvgPx: 2500, TotalSz: tc.fillSz, OID: 7, Fee: 0.01},
				}}, "", nil
			}

			sc, lookupErr := lookupManualStrategy(cfg, subject.ID)
			if lookupErr != nil {
				t.Fatalf("lookup: %v", lookupErr)
			}
			_, coreErr := manualCloseCore(d, sc, manualCloseInputs{StrategyID: subject.ID})

			queued, loadErr := db.LoadPendingManualActions()
			if loadErr != nil {
				t.Fatalf("LoadPendingManualActions: %v", loadErr)
			}

			if tc.wantErrPart != "" {
				if coreErr == nil || !strings.Contains(coreErr.Error(), tc.wantErrPart) {
					t.Fatalf("err = %v, want one containing %q", coreErr, tc.wantErrPart)
				}
				if len(queued) != 0 {
					t.Fatalf("queued actions = %d on a failed close, want 0", len(queued))
				}
				return
			}
			if coreErr != nil {
				t.Fatalf("unexpected error: %v", coreErr)
			}
			if !gotFullClose {
				t.Fatalf("escalation did not reach the venue as a whole-position close")
			}
			if len(queued) != 1 {
				t.Fatalf("queued actions = %d, want 1", len(queued))
			}
			a := queued[0]
			if math.Abs(a.Quantity-tc.wantQty) > 1e-9 {
				t.Errorf("booked quantity = %v, want %v", a.Quantity, tc.wantQty)
			}
			if math.Abs(a.FillFee-tc.wantFee) > 1e-9 {
				t.Errorf("booked fee = %v, want %v", a.FillFee, tc.wantFee)
			}
			wantPnL := tc.wantQty*(2500-2000) - tc.wantFee
			if math.Abs(a.RealizedPnL-wantPnL) > 1e-9 {
				t.Errorf("booked realized PnL = %v, want %v", a.RealizedPnL, wantPnL)
			}
			if a.IsFullClose != tc.wantFull {
				t.Errorf("IsFullClose = %v, want %v", a.IsFullClose, tc.wantFull)
			}
		})
	}
}
