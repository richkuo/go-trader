package main

import (
	"fmt"
	"strings"
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
	trades, err := ExecutePerpsSignal(s, 1, "ETH", 2000, 1, 0.5, "12345", 0.42, false, logger)
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

// TestRecordTrade_OutOfOrderFailureRecoveredBySaveState verifies the dedup
// fix for the #289 carry-over: when an earlier-timestamped RecordTrade fails
// eager-insert but a later-timestamped one succeeds, the pre-fix MAX(timestamp)
// dedup in SaveState would skip the earlier row because its ts < latestTS and
// drop it permanently. With the persisted-flag approach, the earlier row is
// still marked persisted=false and SaveState's next flush picks it up.
func TestRecordTrade_OutOfOrderFailureRecoveredBySaveState(t *testing.T) {
	db := openTestDB(t)

	// Inject a recorder that fails the FIRST call, then delegates to the DB.
	// Simulates a transient write hiccup on trade T1 while T2 lands cleanly.
	calls := 0
	prev := tradeRecorder
	tradeRecorder = func(id string, tr Trade) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("simulated transient failure")
		}
		return db.InsertTrade(id, tr)
	}
	t.Cleanup(func() { tradeRecorder = prev })

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"s-oo": {
				ID: "s-oo", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				TradeHistory: []Trade{},
			},
		},
	}

	s := state.Strategies["s-oo"]
	t1 := time.Now().UTC()
	RecordTrade(s, Trade{Timestamp: t1, Symbol: "BTC", Side: "buy", Quantity: 1, Price: 50000, Value: 50000})
	RecordTrade(s, Trade{Timestamp: t1.Add(time.Millisecond), Symbol: "ETH", Side: "buy", Quantity: 5, Price: 2000, Value: 10000})

	// After eager inserts: T1 failed (persisted=false), T2 succeeded (persisted=true).
	if s.TradeHistory[0].persisted {
		t.Fatal("T1 should not be persisted — recorder failed")
	}
	if !s.TradeHistory[1].persisted {
		t.Fatal("T2 should be persisted — recorder succeeded")
	}

	// Cycle-end SaveState must backfill T1, even though its ts < MAX(ts) in DB.
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	ss := loaded.Strategies["s-oo"]
	if ss == nil || len(ss.TradeHistory) != 2 {
		t.Fatalf("loaded trades = %d, want 2 (T1 was dropped by old ts-dedup?)", len(ss.TradeHistory))
	}
	// Symbols must match: BTC then ETH in ts order.
	if ss.TradeHistory[0].Symbol != "BTC" || ss.TradeHistory[1].Symbol != "ETH" {
		t.Errorf("loaded symbols = %q,%q, want BTC,ETH", ss.TradeHistory[0].Symbol, ss.TradeHistory[1].Symbol)
	}
}

// TestRecordTrade_PersistFailureTriggersWarnHook verifies the operator-visible
// notification path (#289 observability follow-up): when InsertTrade fails,
// tradePersistWarn is invoked so the failure surfaces beyond stderr.
func TestRecordTrade_PersistFailureTriggersWarnHook(t *testing.T) {
	prevRec := tradeRecorder
	prevWarn := tradePersistWarn
	tradeRecorder = func(string, Trade) error { return fmt.Errorf("boom") }
	var warnings []string
	tradePersistWarn = func(msg string) { warnings = append(warnings, msg) }
	t.Cleanup(func() {
		tradeRecorder = prevRec
		tradePersistWarn = prevWarn
	})

	s := &StrategyState{ID: "warn-test", TradeHistory: []Trade{}}
	RecordTrade(s, Trade{Timestamp: time.Now().UTC(), Symbol: "BTC", Side: "buy"})

	if len(warnings) != 1 {
		t.Fatalf("warn hook fired %d times, want 1", len(warnings))
	}
	if !strings.Contains(warnings[0], "warn-test") || !strings.Contains(warnings[0], "boom") {
		t.Errorf("warning = %q, want strategy ID + underlying error", warnings[0])
	}
	// In-memory append must still happen despite recorder failure.
	if len(s.TradeHistory) != 1 {
		t.Errorf("TradeHistory len = %d, want 1 (append must survive recorder failure)", len(s.TradeHistory))
	}
	if s.TradeHistory[0].persisted {
		t.Error("trade should not be marked persisted after recorder failure")
	}
}

// TestExecutePerpsSignal_FlipDoesNotDoubleCountFee pins the policy that when
// a buy signal encounters an existing short — producing a close-short +
// open-long pair in memory — only the opening trade carries the exchange
// fee. A single live fill represents one exchange fee; stamping it on both
// synthetic legs would 2× it in analytics. Summed ExchangeFee across the
// two persisted rows must equal the one real fee, and the OID must appear
// on exactly one row (the opener — the trade that reflects the real fill).
func TestExecutePerpsSignal_FlipDoesNotDoubleCountFee(t *testing.T) {
	db := openTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"hl-flip": {
				ID:             "hl-flip",
				Platform:       "hyperliquid",
				Type:           "perps",
				Cash:           1000,
				InitialCapital: 1000,
				Positions: map[string]*Position{
					// Pre-existing short — the only way to trigger the flip
					// branch in current live mode (state migration, paper→live
					// handoff, or a future adapter that opens shorts).
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: "short", Multiplier: 1, Leverage: 1},
				},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}

	logger := newTestLogger(t)
	s := state.Strategies["hl-flip"]

	// Live buy @ $2000 qty=0.3 → closes the full 0.5 short + opens new 0.3
	// long = 2 in-memory trades, 1 real exchange fill worth $0.42.
	trades, err := ExecutePerpsSignal(s, 1, "ETH", 2000, 1, 0.3, "99999", 0.42, false, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignal: %v", err)
	}
	if trades != 2 {
		t.Fatalf("trades = %d, want 2 (close-short + open-long)", trades)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	ss := loaded.Strategies["hl-flip"]
	if ss == nil || len(ss.TradeHistory) != 2 {
		t.Fatalf("loaded trades = %d, want 2", len(ss.TradeHistory))
	}

	var totalFee float64
	oidHits := 0
	var openerFee float64
	for _, tr := range ss.TradeHistory {
		totalFee += tr.ExchangeFee
		if tr.ExchangeOrderID == "99999" {
			oidHits++
			openerFee = tr.ExchangeFee
		}
	}
	if totalFee != 0.42 {
		t.Errorf("sum(ExchangeFee) = %v, want 0.42 (fee double-counted across flip legs)", totalFee)
	}
	if oidHits != 1 {
		t.Errorf("rows with OID=99999 = %d, want 1 (only the opener should carry the real fill's OID)", oidHits)
	}
	if openerFee != 0.42 {
		t.Errorf("opener ExchangeFee = %v, want 0.42", openerFee)
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
