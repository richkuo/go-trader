package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

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

func TestRecordTrade_SurvivesCrashBeforeSave(t *testing.T) {
	db := openTestDB(t)

	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

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

	now := time.Now().UTC()
	RecordTrade(state.Strategies["s4"], Trade{Timestamp: now, Symbol: "BTC", Side: "buy", Quantity: 1, Price: 50000, Value: 50000})
	RecordTrade(state.Strategies["s4"], Trade{Timestamp: now.Add(time.Millisecond), Symbol: "ETH", Side: "buy", Quantity: 5, Price: 2000, Value: 10000})

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

func TestExecutePerpsWithLeverage_PersistsExchangeMetadata(t *testing.T) {
	db := openTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

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

	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.5, "12345", 0.42, DirectionLong, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
	}
	if trades != 1 {
		t.Fatalf("trades = %d, want 1", trades)
	}

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

func TestDeferredOpenRecordsProtectionOIDSnapshotOnce(t *testing.T) {
	db := openTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	s := &StrategyState{
		ID:              "hl-live",
		Platform:        "hyperliquid",
		Type:            "perps",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	logger := newTestLogger(t)

	exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.5, "12345", 0.42, DirectionLong, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverageDeferredOpen: %v", err)
	}
	if exec.TradesExecuted != 1 || exec.OpenTrade == nil {
		t.Fatalf("exec = %+v, want one deferred open trade", exec)
	}
	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'hl-live'").Scan(&count); err != nil {
		t.Fatalf("count before record: %v", err)
	}
	if count != 0 {
		t.Fatalf("trade rows before recordPositionOpen = %d, want 0", count)
	}

	pos := s.Positions["ETH"]
	pos.EntryATR = 12.5
	pos.StopLossOID = 111
	pos.StopLossTriggerPx = 1875
	pos.TPOIDs = []int64{222, 333}
	mult := 1.5
	sc := StrategyConfig{ID: "hl-live", Platform: "hyperliquid", Type: "perps", StopLossATRMult: &mult}
	recordPositionOpen(s, sc, exec.OpenTrade, pos)

	var entryATR, triggerPx, slATRMult float64
	var slOID int64
	var tpOIDsJSON string
	if err := db.db.QueryRow(`SELECT entry_atr, stop_loss_oid, stop_loss_trigger_px, tp_oids_json, stop_loss_atr_mult FROM trades WHERE strategy_id = 'hl-live'`).Scan(&entryATR, &slOID, &triggerPx, &tpOIDsJSON, &slATRMult); err != nil {
		t.Fatalf("query trade snapshot: %v", err)
	}
	if entryATR != 12.5 || slOID != 111 || triggerPx != 1875 || tpOIDsJSON != "[222,333]" || slATRMult != 1.5 {
		t.Fatalf("snapshot = atr %.2f slOID %d trigger %.2f tp %q mult %.2f, want atr 12.5 slOID 111 trigger 1875 tp [222,333] mult 1.5", entryATR, slOID, triggerPx, tpOIDsJSON, slATRMult)
	}
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].StopLossOID != 111 || len(s.TradeHistory[0].TPOIDs) != 2 || s.TradeHistory[0].TPOIDs[0] != 222 || s.TradeHistory[0].TPOIDs[1] != 333 {
		t.Fatalf("in-memory trade snapshot = %+v, want SL/TPOID snapshot", s.TradeHistory)
	}
}

func TestDeferredPerpsLiveFillBooksWithZeroVirtualCash(t *testing.T) {
	s := &StrategyState{
		ID:              "hl-pool",
		Platform:        "hyperliquid",
		Type:            "perps",
		Cash:            0,
		InitialCapital:  0,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	logger := newTestLogger(t)

	exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(
		s, 1, "ETH", 2000,
		PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 5, MarginPerTradeUSD: 100},
		0.25, "pool-oid", 0.25, DirectionLong, 0, logger,
	)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverageDeferredOpen: %v", err)
	}
	if exec.TradesExecuted != 1 || exec.OpenTrade == nil {
		t.Fatalf("live fill was not booked: exec=%+v", exec)
	}
	if pos := s.Positions["ETH"]; pos == nil || pos.Quantity != 0.25 {
		t.Fatalf("position=%+v, want booked live qty 0.25", pos)
	}
}

func TestDeferredOpenWrappersDoNotInsertBeforeRecordPositionOpen(t *testing.T) {
	db := openTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })
	logger := newTestLogger(t)

	t.Run("spot", func(t *testing.T) {
		s := &StrategyState{
			ID:              "spot1",
			Platform:        "binanceus",
			Type:            "spot",
			Cash:            1000,
			InitialCapital:  1000,
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
			TradeHistory:    []Trade{},
		}
		exec, err := ExecuteSpotSignalWithFillFeeDeferredOpen(s, 1, "BTC", 50000, 0.01, 0.25, "spot-oid", 0, logger)
		if err != nil {
			t.Fatalf("ExecuteSpotSignalWithFillFeeDeferredOpen: %v", err)
		}
		if exec.TradesExecuted != 1 || exec.OpenTrade == nil {
			t.Fatalf("exec = %+v, want one deferred open trade", exec)
		}
		assertTradeCount(t, db, "spot1", 0)
		recordPositionOpen(s, StrategyConfig{ID: "spot1", Type: "spot", Platform: "binanceus"}, exec.OpenTrade, s.Positions["BTC"])
		assertTradeCount(t, db, "spot1", 1)
	})

	t.Run("futures", func(t *testing.T) {
		s := &StrategyState{
			ID:              "ts-es",
			Platform:        "topstep",
			Type:            "futures",
			Cash:            10000,
			InitialCapital:  10000,
			Positions:       map[string]*Position{},
			OptionPositions: map[string]*OptionPosition{},
			TradeHistory:    []Trade{},
		}
		spec := ContractSpec{Multiplier: 50, Margin: 500}
		exec, err := ExecuteFuturesSignalWithFillFeeDeferredOpen(s, 1, "ES", 5000, spec, 2.5, 5, 1, 1.25, "fut-oid", 0, logger)
		if err != nil {
			t.Fatalf("ExecuteFuturesSignalWithFillFeeDeferredOpen: %v", err)
		}
		if exec.TradesExecuted != 1 || exec.OpenTrade == nil {
			t.Fatalf("exec = %+v, want one deferred open trade", exec)
		}
		assertTradeCount(t, db, "ts-es", 0)
		recordPositionOpen(s, StrategyConfig{ID: "ts-es", Type: "futures", Platform: "topstep"}, exec.OpenTrade, s.Positions["ES"])
		assertTradeCount(t, db, "ts-es", 1)
	})
}

func TestRecordPositionOpenFallsBackWhenPositionMissing(t *testing.T) {
	db := openTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	s := &StrategyState{
		ID:              "hl-live",
		Platform:        "hyperliquid",
		Type:            "perps",
		Cash:            1000,
		InitialCapital:  1000,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	logger := newTestLogger(t)
	exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.5, "12345", 0.42, DirectionLong, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverageDeferredOpen: %v", err)
	}
	delete(s.Positions, "ETH")

	if !recordPositionOpen(s, StrategyConfig{ID: "hl-live", Platform: "hyperliquid", Type: "perps"}, exec.OpenTrade, nil) {
		t.Fatal("recordPositionOpen returned false, want fallback insert")
	}
	assertTradeCount(t, db, "hl-live", 1)
	if len(s.TradeHistory) != 1 || s.TradeHistory[0].ExchangeOrderID != "12345" {
		t.Fatalf("fallback trade = %+v, want bare deferred trade recorded", s.TradeHistory)
	}
}

func assertTradeCount(t *testing.T, db *StateDB, strategyID string, want int) {
	t.Helper()
	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = ?", strategyID).Scan(&count); err != nil {
		t.Fatalf("count trades for %s: %v", strategyID, err)
	}
	if count != want {
		t.Fatalf("trade rows for %s = %d, want %d", strategyID, count, want)
	}
}

func TestRecordTrade_OutOfOrderFailureRecoveredBySaveState(t *testing.T) {
	db := openTestDB(t)

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

	if s.TradeHistory[0].persisted {
		t.Fatal("T1 should not be persisted — recorder failed")
	}
	if !s.TradeHistory[1].persisted {
		t.Fatal("T2 should be persisted — recorder succeeded")
	}

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
	if ss.TradeHistory[0].Symbol != "BTC" || ss.TradeHistory[1].Symbol != "ETH" {
		t.Errorf("loaded symbols = %q,%q, want BTC,ETH", ss.TradeHistory[0].Symbol, ss.TradeHistory[1].Symbol)
	}
}

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
	if len(s.TradeHistory) != 1 {
		t.Errorf("TradeHistory len = %d, want 1 (append must survive recorder failure)", len(s.TradeHistory))
	}
	if s.TradeHistory[0].persisted {
		t.Error("trade should not be marked persisted after recorder failure")
	}
}

func TestExecutePerpsWithLeverage_FlipDoesNotDoubleCountFee(t *testing.T) {
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

	trades, err := ExecutePerpsSignalWithLeverage(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0.8, "99999", 0.42, DirectionBoth, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverage: %v", err)
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
	var closeFee float64
	var openerFee float64
	for _, tr := range ss.TradeHistory {
		totalFee += tr.ExchangeFee
		if tr.ExchangeOrderID == "99999" {
			oidHits++
		}
		if strings.Contains(tr.Details, "Open long") {
			openerFee = tr.ExchangeFee
		} else if tr.IsClose {
			closeFee = tr.ExchangeFee
		}
		if !tr.PnLGross || tr.FeeSource != FeeSourceUserFills {
			t.Errorf("flip leg %q: gross=%v src=%q, want gross userfills row", tr.Details, tr.PnLGross, tr.FeeSource)
		}
	}
	if math.Abs(totalFee-0.42) > 1e-9 {
		t.Errorf("sum(ExchangeFee) = %v, want 0.42 (fee double-counted across flip legs)", totalFee)
	}
	if oidHits != 2 {
		t.Errorf("rows with OID=99999 = %d, want 2 (both flip legs share the order)", oidHits)
	}
	if math.Abs(closeFee-0.2625) > 1e-9 {
		t.Errorf("close ExchangeFee = %v, want 0.2625 (0.5/0.8 share)", closeFee)
	}
	if math.Abs(openerFee-0.1575) > 1e-9 {
		t.Errorf("opener ExchangeFee = %v, want 0.1575 (0.3/0.8 share)", openerFee)
	}
}

func TestExecuteSpotWithFillFee_PersistsImmediately(t *testing.T) {
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

	if _, err := ExecuteSpotSignalWithFillFee(s, 1, "BTC", 50000, 0, 0, "", 0, logger); err != nil {
		t.Fatalf("ExecuteSpotSignalWithFillFee: %v", err)
	}

	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'spot1'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("trade rows after ExecuteSpotSignalWithFillFee = %d, want 1 (hook never fired)", count)
	}
}

func TestOpenTradeCarriesTrailingArmStopAfterEarlyRecord(t *testing.T) {
	trailingMult := 2.0
	sc := StrategyConfig{
		ID:                  "hl-trail",
		Platform:            "hyperliquid",
		Type:                "perps",
		Args:                []string{"--mode", "live"},
		TrailingStopATRMult: &trailingMult,
	}

	pos := &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 25}
	if _, syncOK := buildHyperliquidProtectionPlan(sc, pos, 0); syncOK {
		t.Fatalf("pure trailing owner unexpectedly produced a protection plan — the backfill premise changed")
	}

	db := openTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	s := &StrategyState{
		ID:              "hl-trail",
		Platform:        "hyperliquid",
		Type:            "perps",
		Cash:            10000,
		InitialCapital:  10000,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	logger := newTestLogger(t)

	exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 1, "9001", 0.2, DirectionLong, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverageDeferredOpen: %v", err)
	}
	opened := s.Positions["ETH"]
	opened.EntryATR = 25

	recordPositionOpen(s, sc, exec.OpenTrade, opened)
	if s.TradeHistory[0].StopLossOID != 0 || s.TradeHistory[0].StopLossTriggerPx != 0 {
		t.Fatalf("open row = OID %d trigger %.2f, want the pre-arm blanks this test exists to backfill", s.TradeHistory[0].StopLossOID, s.TradeHistory[0].StopLossTriggerPx)
	}

	opened.StopLossOID = 4242
	opened.StopLossTriggerPx = 1950

	stampOpenTradeWithProtectionSnapshot(s, db, sc, "ETH", opened)
	if s.TradeHistory[0].StopLossOID != 4242 || s.TradeHistory[0].StopLossTriggerPx != 1950 {
		t.Fatalf("in-memory open row = OID %d trigger %.2f, want 4242 / 1950", s.TradeHistory[0].StopLossOID, s.TradeHistory[0].StopLossTriggerPx)
	}
	var slOID int64
	var triggerPx float64
	if err := db.db.QueryRow(`SELECT stop_loss_oid, stop_loss_trigger_px FROM trades WHERE strategy_id = 'hl-trail'`).Scan(&slOID, &triggerPx); err != nil {
		t.Fatalf("query persisted open row: %v", err)
	}
	if slOID != 4242 || triggerPx != 1950 {
		t.Errorf("persisted open row = OID %d trigger %.2f, want 4242 / 1950", slOID, triggerPx)
	}

	opened.StopLossOID = 0
	opened.StopLossTriggerPx = 0
	stampOpenTradeWithProtectionSnapshot(s, db, sc, "ETH", opened)
	if s.TradeHistory[0].StopLossOID != 4242 || s.TradeHistory[0].StopLossTriggerPx != 1950 {
		t.Errorf("re-stamp overwrote the armed snapshot: OID %d trigger %.2f, want 4242 / 1950", s.TradeHistory[0].StopLossOID, s.TradeHistory[0].StopLossTriggerPx)
	}
}

func TestPaperHLPerpsOpenRecordsExactlyOneTradeRow(t *testing.T) {
	db := openTestDB(t)
	prev := tradeRecorder
	tradeRecorder = db.InsertTrade
	t.Cleanup(func() { tradeRecorder = prev })

	sc := StrategyConfig{ID: "hl-paper", Platform: "hyperliquid", Type: "perps", Args: []string{"--mode", "paper"}}
	s := &StrategyState{
		ID:              "hl-paper",
		Platform:        "hyperliquid",
		Type:            "perps",
		Cash:            10000,
		InitialCapital:  10000,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	logger := newTestLogger(t)

	exec, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, 1, "ETH", 2000, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionLong, 0, logger)
	if err != nil {
		t.Fatalf("ExecutePerpsSignalWithLeverageDeferredOpen: %v", err)
	}
	if exec.TradesExecuted != 1 || exec.OpenTrade == nil {
		t.Fatalf("paper exec = %+v, want one deferred open trade", exec)
	}
	recordPositionOpen(s, sc, exec.OpenTrade, s.Positions["ETH"])

	countRows := func(where string) int {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM trades WHERE strategy_id = 'hl-paper' AND ` + where).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", where, err)
		}
		return n
	}
	if got := countRows("is_close = 0"); got != 1 {
		t.Fatalf("paper open rows = %d, want exactly 1", got)
	}
	if got := countRows("is_close = 1"); got != 0 {
		t.Fatalf("close rows after an open = %d, want 0", got)
	}

	if _, err := ExecutePerpsSignalWithLeverageDeferredOpen(s, -1, "ETH", 2100, PerpsSizing{SizingLeverage: 1, ExchangeLeverage: 1}, 0, "", 0, DirectionLong, 0, logger); err != nil {
		t.Fatalf("paper close: %v", err)
	}
	if got := countRows("is_close = 1"); got != 1 {
		t.Errorf("paper close rows = %d, want exactly 1", got)
	}
	if got := countRows("is_close = 0"); got != 1 {
		t.Errorf("open rows after the close = %d, want still exactly 1", got)
	}
}
