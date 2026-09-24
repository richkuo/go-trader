package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

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
