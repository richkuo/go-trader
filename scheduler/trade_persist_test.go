package main

import (
	"testing"
	"time"
)

// TestInsertTrade_WritesRow verifies StateDB.InsertTrade persists a single row
// immediately, independent of SaveState. This is the foundation of #289.
func TestInsertTrade_WritesRow(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	trade := Trade{
		Timestamp: now, StrategyID: "test", Symbol: "BTC", Side: "buy",
		Quantity: 1.5, Price: 50000, Value: 75000, TradeType: "spot",
		Details: "test", ExchangeOrderID: "oid-42", ExchangeFee: 0.75,
	}

	if err := db.InsertTrade("test", trade); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'test'").Scan(&count); err != nil {
		t.Fatalf("count trades: %v", err)
	}
	if count != 1 {
		t.Fatalf("trade count = %d, want 1", count)
	}

	var symbol, oid string
	var fee float64
	if err := db.db.QueryRow(
		"SELECT symbol, exchange_order_id, exchange_fee FROM trades WHERE strategy_id = 'test'",
	).Scan(&symbol, &oid, &fee); err != nil {
		t.Fatalf("read trade: %v", err)
	}
	if symbol != "BTC" || oid != "oid-42" || fee != 0.75 {
		t.Errorf("trade row = (%q, %q, %g), want (BTC, oid-42, 0.75)", symbol, oid, fee)
	}
}

// TestRecordTrade_AppendsAndPersists verifies RecordTrade both appends to
// TradeHistory and invokes the tradeRecorder hook (#289 — crash resilience).
func TestRecordTrade_AppendsAndPersists(t *testing.T) {
	db := openTestDB(t)

	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	s := &StrategyState{ID: "s1", TradeHistory: []Trade{}}
	trade := Trade{
		Timestamp: time.Now().UTC(), Symbol: "ETH", Side: "buy",
		Quantity: 2, Price: 2000, Value: 4000, TradeType: "spot",
	}
	RecordTrade(s, trade)

	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory len = %d, want 1", len(s.TradeHistory))
	}
	if s.TradeHistory[0].StrategyID != "s1" {
		t.Errorf("StrategyID fallback = %q, want s1", s.TradeHistory[0].StrategyID)
	}

	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 's1'").Scan(&count); err != nil {
		t.Fatalf("count trades: %v", err)
	}
	if count != 1 {
		t.Errorf("DB rows = %d, want 1", count)
	}
}

// TestRecordTrade_NoRecorder verifies RecordTrade still appends in-memory when
// tradeRecorder is nil (tests, pre-DB boot, or persistence hook unset).
func TestRecordTrade_NoRecorder(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	s := &StrategyState{ID: "s2", TradeHistory: []Trade{}}
	RecordTrade(s, Trade{Timestamp: time.Now().UTC(), Symbol: "BTC", Side: "buy"})

	if len(s.TradeHistory) != 1 {
		t.Errorf("TradeHistory len = %d, want 1", len(s.TradeHistory))
	}
}

// TestRecordTrade_SaveStateNoDoubleInsert verifies that a trade already written
// via RecordTrade is NOT duplicated when cycle-end SaveState runs. The timestamp
// guard inside SaveState skips any trade whose ts is not strictly greater than
// the max already in DB.
func TestRecordTrade_SaveStateNoDoubleInsert(t *testing.T) {
	db := openTestDB(t)

	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"s3": {
				ID:              "s3",
				Type:            "spot",
				Cash:            1000,
				InitialCapital:  1000,
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
			},
		},
	}

	now := time.Now().UTC()
	RecordTrade(state.Strategies["s3"], Trade{
		Timestamp: now, Symbol: "BTC", Side: "buy", Quantity: 1, Price: 50000, Value: 50000,
	})

	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 's3'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("after RecordTrade + SaveState, trade rows = %d, want 1 (no double-insert)", count)
	}
}

// TestRecordTrade_SurvivesCrashBeforeSave simulates a mid-cycle crash: trades
// written via RecordTrade must still be visible when state is reloaded,
// even though SaveState was never called. This is the core #289 guarantee.
func TestRecordTrade_SurvivesCrashBeforeSave(t *testing.T) {
	db := openTestDB(t)

	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	// Seed a strategy row so LoadState can attach trades to it.
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"s4": {
				ID:              "s4",
				Type:            "spot",
				Cash:            1000,
				InitialCapital:  1000,
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}

	// Execute trades — simulate mid-cycle — then DO NOT call SaveState.
	now := time.Now().UTC()
	RecordTrade(state.Strategies["s4"], Trade{Timestamp: now, Symbol: "BTC", Side: "buy", Quantity: 1, Price: 50000, Value: 50000})
	RecordTrade(state.Strategies["s4"], Trade{Timestamp: now.Add(time.Millisecond), Symbol: "ETH", Side: "buy", Quantity: 5, Price: 2000, Value: 10000})

	// Simulated crash/restart: reload from DB.
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded == nil || loaded.Strategies["s4"] == nil {
		t.Fatal("loaded state missing s4")
	}
	if got := len(loaded.Strategies["s4"].TradeHistory); got != 2 {
		t.Errorf("survived trades = %d, want 2 — mid-cycle crash lost trades", got)
	}
}

// TestExecutePerpsSignal_PersistsExchangeMetadata is the #289 regression guard
// for the fix that threads fillOID/fillFee into ExecutePerpsSignal so every
// Trade is constructed complete before RecordTrade persists it. Prior to the
// fix the OID/fee were stamped onto s.TradeHistory AFTER RecordTrade had
// already written an empty-metadata row; SaveState's timestamp-dedup then
// skipped re-insertion and the DB stayed stale. Reload + assert fills.
func TestExecutePerpsSignal_PersistsExchangeMetadata(t *testing.T) {
	db := openTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	// Seed the strategy row so LoadState has something to hang trades on.
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"hl-live": {
				ID:              "hl-live",
				Platform:        "hyperliquid",
				Type:            "perps",
				Cash:            1000,
				InitialCapital:  1000,
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}

	logger := newTestLogger(t)
	s := state.Strategies["hl-live"]

	// Live open-long @ $2000, qty=0.5, OID=12345, fee=$0.42.
	trades, err := ExecutePerpsSignal(s, 1, "ETH", 2000, 1, 0.5, "12345", 0.42, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignal: %v", err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}

	// Reload from SQLite — simulates mid-cycle crash before SaveState runs.
	// The persisted row must carry the exchange metadata, not the zero values
	// that eager-INSERT-then-stamp would have left behind.
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	ss := loaded.Strategies["hl-live"]
	if ss == nil || len(ss.TradeHistory) != 1 {
		t.Fatalf("loaded trades = %d, want 1", len(ss.TradeHistory))
	}
	got := ss.TradeHistory[0]
	if got.ExchangeOrderID != "12345" {
		t.Errorf("persisted ExchangeOrderID = %q, want %q (stamp never reached DB)", got.ExchangeOrderID, "12345")
	}
	if got.ExchangeFee != 0.42 {
		t.Errorf("persisted ExchangeFee = %v, want 0.42 (stamp never reached DB)", got.ExchangeFee)
	}
}

// TestExecuteSpotSignal_PersistsImmediately verifies that the production
// execution path (ExecuteSpotSignal) writes trades through the tradeRecorder
// hook, not just the end-of-cycle SaveState batch.
func TestExecuteSpotSignal_PersistsImmediately(t *testing.T) {
	db := openTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	s := &StrategyState{
		ID: "spot1", Cash: 10000, InitialCapital: 10000,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	logger := newTestLogger(t)

	if _, err := ExecuteSpotSignal(s, 1, "BTC", 50000, 0, logger); err != nil {
		t.Fatalf("ExecuteSpotSignal: %v", err)
	}

	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'spot1'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("trade rows after ExecuteSpotSignal = %d, want 1 (hook never fired)", count)
	}
}
