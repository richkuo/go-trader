package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *StateDB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	resetInitialCapitalGuardDedup(t)
	return db
}

func openNullablePositionIDDB(t *testing.T) *StateDB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	nullableDDL := strings.ReplaceAll(schemaDDL, "position_id TEXT NOT NULL DEFAULT ''", "position_id TEXT")
	if _, err := raw.Exec(nullableDDL); err != nil {
		raw.Close()
		t.Fatalf("create nullable-position-id schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	resetInitialCapitalGuardDedup(t)
	return db
}

func resetInitialCapitalGuardDedup(t *testing.T) {
	t.Helper()
	initialCapitalGuardWarned = sync.Map{}
	t.Cleanup(func() { initialCapitalGuardWarned = sync.Map{} })
}

func TestOpenStateDB(t *testing.T) {
	db := openTestDB(t)

	var mode string
	if err := db.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}

	tables := []string{"app_state", "strategies", "positions", "option_positions", "trades", "portfolio_risk", "kill_switch_events", "correlation_snapshot"}
	for _, table := range tables {
		var name string
		err := db.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}
}

func TestOpenStateDB_CreatesDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "state.db")
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	db.Close()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("state.db was not created in nested directory")
	}
}

func TestLoadState_EmptyDB(t *testing.T) {
	db := openTestDB(t)
	state, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if state != nil {
		t.Errorf("expected nil state for empty DB, got %+v", state)
	}
}

func makeTestState() *AppState {
	now := time.Now().UTC().Truncate(time.Nanosecond)
	return &AppState{
		CycleCount:              42,
		LastCycle:               now,
		LastLeaderboardPostDate: "2026-04-08",
		Strategies: map[string]*StrategyState{
			"hl-momentum-btc": {
				ID:             "hl-momentum-btc",
				Type:           "perps",
				Platform:       "hyperliquid",
				Cash:           950.50,
				InitialCapital: 1000.0,
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long", Multiplier: 0, OwnerStrategyID: "hl-momentum-btc", OpenedAt: now.Add(-12 * time.Hour)},
				},
				OptionPositions: map[string]*OptionPosition{
					"opt-1": {
						ID: "opt-1", Underlying: "BTC", OptionType: "call", Strike: 55000,
						Expiry: "2026-05-01", DTE: 23, Action: "buy", Quantity: 1,
						EntryPremium: 0.05, EntryPremiumUSD: 2500, CurrentValueUSD: 3000,
						Greeks:   OptGreeks{Delta: 0.6, Gamma: 0.01, Theta: -5, Vega: 100},
						OpenedAt: now.Add(-24 * time.Hour),
					},
				},
				TradeHistory: []Trade{
					{Timestamp: now.Add(-2 * time.Hour), StrategyID: "hl-momentum-btc", Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps", Details: "momentum signal"},
					{Timestamp: now.Add(-1 * time.Hour), StrategyID: "hl-momentum-btc", Symbol: "BTC", Side: "sell", Quantity: 0.05, Price: 51000, Value: 2550, TradeType: "perps", Details: "partial close"},
				},
				RiskState: RiskState{
					PeakValue: 1050, MaxDrawdownPct: 10, CurrentDrawdownPct: 2.5,
					DailyPnL: 50, DailyPnLDate: "2026-04-08",
					ConsecutiveLosses: 0, CircuitBreaker: false,
				},
			},
			"spot-rsi-eth": {
				ID:              "spot-rsi-eth",
				Type:            "spot",
				Platform:        "binanceus",
				Cash:            800,
				InitialCapital:  1000,
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
				TradeHistory:    []Trade{},
				RiskState: RiskState{
					PeakValue: 1000, MaxDrawdownPct: 15,
					CircuitBreaker:      true,
					CircuitBreakerUntil: now.Add(1 * time.Hour),
				},
			},
		},
		PortfolioRisk: PortfolioRiskState{
			PeakValue: 2050, CurrentDrawdownPct: 1.5, CurrentMarginDrawdownPct: 18.7,
			KillSwitchActive:           false,
			WarningSent:                true,
			WarnBandEnteredAt:          now.Add(-20 * time.Minute),
			LastWarningEquityDDPct:     1.5,
			LastWarningMarginDDPct:     18.7,
			WarningEquityDeltaPct:      0.3,
			WarningMarginDeltaPct:      -0.2,
			ManualMarkBasisRebaselined: true,
			DrawdownReadingSubstituted: true,
			UntrustedOverLimitSince:    now.Add(-7 * time.Minute),
			Events: []KillSwitchEvent{
				{Timestamp: now.Add(-3 * time.Hour), Type: "warning", Source: "margin", DrawdownPct: 18.7, PortfolioValue: 1950, PeakValue: 2050, Details: "approaching threshold"},
			},
		},
		CorrelationSnapshot: &CorrelationSnapshot{
			Timestamp:         now,
			PortfolioGrossUSD: 5000,
			Warnings:          []string{"BTC concentration 70%"},
			Assets: map[string]*AssetExposure{
				"BTC": {Asset: "BTC", NetDeltaUSD: 5000, GrossDeltaUSD: 5000, ConcentrationPct: 70,
					Strategies: []StrategyExposure{{StrategyID: "hl-momentum-btc", DeltaUSD: 5000, Type: "perps"}}},
			},
		},
	}
}

func TestSaveAndLoadDBRoundTrip(t *testing.T) {
	db := openTestDB(t)
	original := makeTestState()

	if err := db.SaveState(original); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadState returned nil")
	}

	if loaded.CycleCount != original.CycleCount {
		t.Errorf("CycleCount = %d, want %d", loaded.CycleCount, original.CycleCount)
	}
	if loaded.LastLeaderboardPostDate != original.LastLeaderboardPostDate {
		t.Errorf("LastLeaderboardPostDate = %q, want %q", loaded.LastLeaderboardPostDate, original.LastLeaderboardPostDate)
	}
	if len(loaded.Strategies) != len(original.Strategies) {
		t.Fatalf("strategies count = %d, want %d", len(loaded.Strategies), len(original.Strategies))
	}

	hlStrat := loaded.Strategies["hl-momentum-btc"]
	if hlStrat == nil {
		t.Fatal("missing strategy hl-momentum-btc")
	}
	if hlStrat.Cash != 950.50 {
		t.Errorf("Cash = %f, want 950.50", hlStrat.Cash)
	}
	if hlStrat.Platform != "hyperliquid" {
		t.Errorf("Platform = %q, want %q", hlStrat.Platform, "hyperliquid")
	}

	btcPos := hlStrat.Positions["BTC"]
	if btcPos == nil {
		t.Fatal("missing position BTC")
	}
	if btcPos.Quantity != 0.1 || btcPos.AvgCost != 50000 || btcPos.Side != "long" {
		t.Errorf("position mismatch: %+v", btcPos)
	}
	if btcPos.OwnerStrategyID != "hl-momentum-btc" {
		t.Errorf("OwnerStrategyID = %q, want %q", btcPos.OwnerStrategyID, "hl-momentum-btc")
	}
	if btcPos.OpenedAt.IsZero() {
		t.Error("position OpenedAt should round-trip, got zero")
	}

	opt := hlStrat.OptionPositions["opt-1"]
	if opt == nil {
		t.Fatal("missing option_position opt-1")
	}
	if opt.Strike != 55000 || opt.Action != "buy" || opt.OptionType != "call" {
		t.Errorf("option mismatch: %+v", opt)
	}
	if opt.Greeks.Delta != 0.6 || opt.Greeks.Vega != 100 {
		t.Errorf("greeks mismatch: %+v", opt.Greeks)
	}

	if len(hlStrat.TradeHistory) != 2 {
		t.Fatalf("trade count = %d, want 2", len(hlStrat.TradeHistory))
	}
	if hlStrat.TradeHistory[0].Side != "buy" || hlStrat.TradeHistory[1].Side != "sell" {
		t.Errorf("trade order mismatch")
	}

	if hlStrat.RiskState.DailyPnL != 50 || hlStrat.RiskState.CurrentDrawdownPct != 2.5 {
		t.Errorf("risk state mismatch: %+v", hlStrat.RiskState)
	}

	ethStrat := loaded.Strategies["spot-rsi-eth"]
	if ethStrat == nil {
		t.Fatal("missing strategy spot-rsi-eth")
	}
	if !ethStrat.RiskState.CircuitBreaker {
		t.Error("CircuitBreaker should be true")
	}
	if ethStrat.RiskState.CircuitBreakerUntil.IsZero() {
		t.Error("CircuitBreakerUntil should not be zero")
	}

	if loaded.PortfolioRisk.PeakValue != 2050 {
		t.Errorf("PortfolioRisk.PeakValue = %f, want 2050", loaded.PortfolioRisk.PeakValue)
	}
	if loaded.PortfolioRisk.CurrentDrawdownPct != 1.5 {
		t.Errorf("PortfolioRisk.CurrentDrawdownPct = %f, want 1.5", loaded.PortfolioRisk.CurrentDrawdownPct)
	}
	if loaded.PortfolioRisk.CurrentMarginDrawdownPct != 18.7 {
		t.Errorf("PortfolioRisk.CurrentMarginDrawdownPct = %f, want 18.7", loaded.PortfolioRisk.CurrentMarginDrawdownPct)
	}
	if !loaded.PortfolioRisk.ManualMarkBasisRebaselined {
		t.Error("PortfolioRisk.ManualMarkBasisRebaselined = false, want true (one-shot latch must survive a restart)")
	}
	if !loaded.PortfolioRisk.DrawdownReadingSubstituted {
		t.Error("PortfolioRisk.DrawdownReadingSubstituted = false, want true (a substituted reading must stay labeled across a restart)")
	}
	if loaded.PortfolioRisk.UntrustedOverLimitSince.IsZero() ||
		!loaded.PortfolioRisk.UntrustedOverLimitSince.Equal(original.PortfolioRisk.UntrustedOverLimitSince) {
		t.Errorf("PortfolioRisk.UntrustedOverLimitSince = %v, want %v (a restart must not reopen the deferral window)",
			loaded.PortfolioRisk.UntrustedOverLimitSince, original.PortfolioRisk.UntrustedOverLimitSince)
	}
	if !loaded.PortfolioRisk.WarningSent {
		t.Error("PortfolioRisk.WarningSent should be true")
	}
	if loaded.PortfolioRisk.WarnBandEnteredAt.IsZero() {
		t.Error("PortfolioRisk.WarnBandEnteredAt should round-trip")
	}
	if loaded.PortfolioRisk.LastWarningEquityDDPct != 1.5 {
		t.Errorf("PortfolioRisk.LastWarningEquityDDPct = %f, want 1.5", loaded.PortfolioRisk.LastWarningEquityDDPct)
	}
	if loaded.PortfolioRisk.LastWarningMarginDDPct != 18.7 {
		t.Errorf("PortfolioRisk.LastWarningMarginDDPct = %f, want 18.7", loaded.PortfolioRisk.LastWarningMarginDDPct)
	}
	if loaded.PortfolioRisk.WarningEquityDeltaPct != 0.3 {
		t.Errorf("PortfolioRisk.WarningEquityDeltaPct = %f, want 0.3", loaded.PortfolioRisk.WarningEquityDeltaPct)
	}
	if loaded.PortfolioRisk.WarningMarginDeltaPct != -0.2 {
		t.Errorf("PortfolioRisk.WarningMarginDeltaPct = %f, want -0.2", loaded.PortfolioRisk.WarningMarginDeltaPct)
	}
	if len(loaded.PortfolioRisk.Events) != 1 {
		t.Fatalf("kill switch events = %d, want 1", len(loaded.PortfolioRisk.Events))
	}
	if loaded.PortfolioRisk.Events[0].Type != "warning" {
		t.Errorf("event type = %q, want %q", loaded.PortfolioRisk.Events[0].Type, "warning")
	}
	if loaded.PortfolioRisk.Events[0].Source != "margin" {
		t.Errorf("event source = %q, want %q", loaded.PortfolioRisk.Events[0].Source, "margin")
	}

	if loaded.CorrelationSnapshot == nil {
		t.Fatal("CorrelationSnapshot is nil")
	}
	if loaded.CorrelationSnapshot.PortfolioGrossUSD != 5000 {
		t.Errorf("PortfolioGrossUSD = %f, want 5000", loaded.CorrelationSnapshot.PortfolioGrossUSD)
	}
	if len(loaded.CorrelationSnapshot.Warnings) != 1 {
		t.Fatalf("correlation warnings = %d, want 1", len(loaded.CorrelationSnapshot.Warnings))
	}
	btcExposure := loaded.CorrelationSnapshot.Assets["BTC"]
	if btcExposure == nil {
		t.Fatal("missing BTC exposure in correlation snapshot")
	}
	if btcExposure.ConcentrationPct != 70 {
		t.Errorf("ConcentrationPct = %f, want 70", btcExposure.ConcentrationPct)
	}
}

func TestSaveState_AppendsTradesOnly(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"test": {
				ID: "test", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
				TradeHistory: []Trade{
					{Timestamp: now.Add(-2 * time.Hour), StrategyID: "test", Symbol: "BTC", Side: "buy", Quantity: 1, Price: 100, Value: 100},
					{Timestamp: now.Add(-1 * time.Hour), StrategyID: "test", Symbol: "BTC", Side: "sell", Quantity: 1, Price: 110, Value: 110},
				},
			},
		},
	}

	if err := db.SaveState(state); err != nil {
		t.Fatalf("first SaveState: %v", err)
	}

	state.CycleCount = 2
	state.Strategies["test"].TradeHistory = append(state.Strategies["test"].TradeHistory,
		Trade{Timestamp: now, StrategyID: "test", Symbol: "ETH", Side: "buy", Quantity: 2, Price: 200, Value: 400},
	)

	if err := db.SaveState(state); err != nil {
		t.Fatalf("second SaveState: %v", err)
	}

	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'test'").Scan(&count); err != nil {
		t.Fatalf("count trades: %v", err)
	}
	if count != 3 {
		t.Errorf("trade count = %d, want 3", count)
	}
}

func TestSaveState_KillSwitchEventsStoredAsIs(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()

	state := &AppState{
		CycleCount: 1,
		Strategies: make(map[string]*StrategyState),
	}
	for i := 0; i < 60; i++ {
		state.PortfolioRisk.Events = append(state.PortfolioRisk.Events, KillSwitchEvent{
			Timestamp: now.Add(time.Duration(i) * time.Minute), Type: "warning", DrawdownPct: float64(i),
		})
	}

	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM kill_switch_events").Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 60 {
		t.Errorf("DB event count = %d, want 60 (stored as-is)", count)
	}
}

func TestLoadState_NilMapsInitialized(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"test": {
				ID: "test", Type: "spot", Cash: 100, InitialCapital: 100,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
				TradeHistory: []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	s := loaded.Strategies["test"]
	if s.Positions == nil {
		t.Error("Positions should be initialized, not nil")
	}
	if s.OptionPositions == nil {
		t.Error("OptionPositions should be initialized, not nil")
	}
	if s.TradeHistory == nil {
		t.Error("TradeHistory should be initialized, not nil")
	}
}

func TestQueryTradeHistory_Filters(t *testing.T) {
	cases := []struct {
		name      string
		strategy  string
		symbol    string
		limit     int
		wantTotal int
		wantLen   int
		check     func(t *testing.T, trades []Trade)
	}{
		{
			name: "no filter newest first", limit: 50, wantTotal: 2, wantLen: 2,
			check: func(t *testing.T, trades []Trade) {
				if trades[0].Side != "sell" {
					t.Errorf("first trade should be most recent (sell), got %q", trades[0].Side)
				}
			},
		},
		{
			name: "by strategy", strategy: "hl-momentum-btc", limit: 50, wantTotal: 2, wantLen: 2,
			check: func(t *testing.T, trades []Trade) {
				for _, tr := range trades {
					if tr.StrategyID != "hl-momentum-btc" {
						t.Errorf("trade strategy = %q, want %q", tr.StrategyID, "hl-momentum-btc")
					}
				}
			},
		},
		{name: "by nonexistent strategy", strategy: "nonexistent", limit: 50, wantTotal: 0, wantLen: 0},
		{
			name: "by symbol", symbol: "BTC", limit: 50, wantTotal: 2, wantLen: 2,
			check: func(t *testing.T, trades []Trade) {
				for _, tr := range trades {
					if tr.Symbol != "BTC" {
						t.Errorf("trade symbol = %q, want %q", tr.Symbol, "BTC")
					}
				}
			},
		},
		{name: "limit clamped", limit: 9999, wantTotal: 2, wantLen: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			if err := db.SaveState(makeTestState()); err != nil {
				t.Fatalf("SaveState: %v", err)
			}
			trades, total, err := db.QueryTradeHistory(tc.strategy, tc.symbol, time.Time{}, time.Time{}, tc.limit, 0)
			if err != nil {
				t.Fatalf("QueryTradeHistory: %v", err)
			}
			if total != tc.wantTotal || len(trades) != tc.wantLen {
				t.Fatalf("total=%d len=%d, want %d/%d", total, len(trades), tc.wantTotal, tc.wantLen)
			}
			if tc.check != nil {
				tc.check(t, trades)
			}
		})
	}
}

func TestQueryTradeHistory_Pagination(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"test": {
				ID: "test", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
			},
		},
	}
	for i := 0; i < 10; i++ {
		state.Strategies["test"].TradeHistory = append(state.Strategies["test"].TradeHistory,
			Trade{Timestamp: now.Add(time.Duration(i) * time.Minute), StrategyID: "test", Symbol: "BTC", Side: "buy", Quantity: 1, Price: float64(100 + i), Value: float64(100 + i)},
		)
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	trades, total, err := db.QueryTradeHistory("", "", time.Time{}, time.Time{}, 3, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory page 1: %v", err)
	}
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	if len(trades) != 3 {
		t.Errorf("page 1 len = %d, want 3", len(trades))
	}

	trades2, _, err := db.QueryTradeHistory("", "", time.Time{}, time.Time{}, 3, 3)
	if err != nil {
		t.Fatalf("QueryTradeHistory page 2: %v", err)
	}
	if len(trades2) != 3 {
		t.Errorf("page 2 len = %d, want 3", len(trades2))
	}

	if len(trades) > 0 && len(trades2) > 0 && trades[0].Price == trades2[0].Price {
		t.Error("page 1 and page 2 should have different trades")
	}
}

func TestQueryTradeHistory_TimeBounds(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"test": {
				ID: "test", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
				TradeHistory: []Trade{
					{Timestamp: now.Add(-3 * time.Hour), StrategyID: "test", Symbol: "BTC", Side: "buy", Quantity: 1, Price: 100, Value: 100},
					{Timestamp: now.Add(-2 * time.Hour), StrategyID: "test", Symbol: "BTC", Side: "sell", Quantity: 1, Price: 110, Value: 110},
					{Timestamp: now.Add(-1 * time.Hour), StrategyID: "test", Symbol: "BTC", Side: "buy", Quantity: 1, Price: 105, Value: 105},
				},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	since := now.Add(-150 * time.Minute)
	trades, total, err := db.QueryTradeHistory("", "", since, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory with since: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(trades) != 2 {
		t.Errorf("trades len = %d, want 2", len(trades))
	}
}

func TestCorrelationSnapshotRoundTrip(t *testing.T) {
	db := openTestDB(t)

	state := makeTestState()
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.CorrelationSnapshot == nil {
		t.Fatal("CorrelationSnapshot is nil")
	}
	if loaded.CorrelationSnapshot.PortfolioGrossUSD != 5000 {
		t.Errorf("PortfolioGrossUSD = %f, want 5000", loaded.CorrelationSnapshot.PortfolioGrossUSD)
	}

	state.CorrelationSnapshot = nil
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState nil snapshot: %v", err)
	}
	loaded2, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState nil snapshot: %v", err)
	}
	if loaded2.CorrelationSnapshot != nil {
		t.Errorf("expected nil CorrelationSnapshot, got %+v", loaded2.CorrelationSnapshot)
	}
}

func TestSaveState_DuplicateStrategyIDs(t *testing.T) {
	db := openTestDB(t)

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"hl-sma-btc": {
				ID: "hl-sma-btc", Type: "perps", Platform: "hyperliquid",
				Cash: 500, InitialCapital: 1000,
				Positions:       map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long"}},
				OptionPositions: make(map[string]*OptionPosition),
				TradeHistory:    []Trade{},
			},
			"hl-sma-btc-dup": {
				ID: "hl-sma-btc", Type: "perps", Platform: "hyperliquid",
				Cash: 600, InitialCapital: 1000,
				Positions:       map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.2, AvgCost: 51000, Side: "long"}},
				OptionPositions: make(map[string]*OptionPosition),
				TradeHistory:    []Trade{},
			},
		},
	}

	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState with duplicate IDs should not error: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	if len(loaded.Strategies) != 1 {
		t.Errorf("expected 1 strategy after dedup, got %d", len(loaded.Strategies))
	}
	if _, ok := loaded.Strategies["hl-sma-btc"]; !ok {
		t.Error("expected strategy hl-sma-btc to exist")
	}
}

func TestSaveState_EmptyStrategies(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{
		CycleCount: 5,
		Strategies: make(map[string]*StrategyState),
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded.CycleCount != 5 {
		t.Errorf("CycleCount = %d, want 5", loaded.CycleCount)
	}
	if len(loaded.Strategies) != 0 {
		t.Errorf("strategies count = %d, want 0", len(loaded.Strategies))
	}
}

func TestTradeExchangeFieldsRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Nanosecond)
	type want struct {
		orderID string
		fee     float64
	}
	cases := []struct {
		name     string
		typ      string
		platform string
		trades   []Trade
		want     []want
	}{
		{
			name: "live perps two trades", typ: "perps", platform: "hyperliquid",
			trades: []Trade{
				{Timestamp: now.Add(-1 * time.Hour), Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps", Details: "live buy", ExchangeOrderID: "1234567890", ExchangeFee: 1.75},
				{Timestamp: now, Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps", Details: "live sell", ExchangeOrderID: "1234567891", ExchangeFee: 1.79},
			},
			want: []want{{"1234567890", 1.75}, {"1234567891", 1.79}},
		},
		{
			name: "live perps single trade", typ: "perps", platform: "hyperliquid",
			trades: []Trade{
				{Timestamp: now, Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps", Details: "live", ExchangeOrderID: "9876543210", ExchangeFee: 2.50},
			},
			want: []want{{"9876543210", 2.50}},
		},
		{
			name: "paper spot empty by default", typ: "spot", platform: "binanceus",
			trades: []Trade{
				{Timestamp: now, Symbol: "BTC/USDT", Side: "buy", Quantity: 0.01, Price: 50000, Value: 500, TradeType: "spot", Details: "paper trade"},
			},
			want: []want{{"", 0}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			const id = "trade-exchange-fields"
			trades := make([]Trade, len(tc.trades))
			for i, tr := range tc.trades {
				tr.StrategyID = id
				trades[i] = tr
			}
			state := &AppState{
				CycleCount: 1,
				Strategies: map[string]*StrategyState{
					id: {
						ID: id, Type: tc.typ, Platform: tc.platform,
						Cash: 1000, InitialCapital: 1000,
						Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
						TradeHistory: trades,
					},
				},
			}
			if err := db.SaveState(state); err != nil {
				t.Fatalf("SaveState: %v", err)
			}

			loaded, err := db.LoadState()
			if err != nil {
				t.Fatalf("LoadState: %v", err)
			}
			hist := loaded.Strategies[id].TradeHistory
			if len(hist) != len(tc.want) {
				t.Fatalf("LoadState trade count = %d, want %d", len(hist), len(tc.want))
			}
			for i, w := range tc.want {
				if hist[i].ExchangeOrderID != w.orderID || hist[i].ExchangeFee != w.fee {
					t.Errorf("LoadState trade[%d] = (%q, %g), want (%q, %g)", i, hist[i].ExchangeOrderID, hist[i].ExchangeFee, w.orderID, w.fee)
				}
			}

			queried, total, err := db.QueryTradeHistory(id, "", time.Time{}, time.Time{}, 50, 0)
			if err != nil {
				t.Fatalf("QueryTradeHistory: %v", err)
			}
			if total != len(tc.want) || len(queried) != len(tc.want) {
				t.Fatalf("QueryTradeHistory total=%d len=%d, want %d", total, len(queried), len(tc.want))
			}
			for i, w := range tc.want {
				q := queried[len(queried)-1-i]
				if q.ExchangeOrderID != w.orderID || q.ExchangeFee != w.fee {
					t.Errorf("QueryTradeHistory trade[%d] = (%q, %g), want (%q, %g)", i, q.ExchangeOrderID, q.ExchangeFee, w.orderID, w.fee)
				}
			}
		})
	}
}

func TestSaveLoadState_PositionIDsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"s1": {
				ID: "s1", Type: "options", Platform: "deribit",
				Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", TradePositionID: "spot-position-1", Quantity: 0.1, AvgCost: 50000, Side: "long", OpenedAt: now},
				},
				OptionPositions: map[string]*OptionPosition{
					"BTC-call-buy-65000-2026-12-31": {
						ID: "BTC-call-buy-65000-2026-12-31", TradePositionID: "option-position-1",
						Underlying: "BTC", OptionType: "call", Strike: 65000, Expiry: "2026-12-31",
						DTE: 30, Action: "buy", Quantity: 1, EntryPremiumUSD: 300, CurrentValueUSD: 350,
						OpenedAt: now,
					},
				},
				TradeHistory: []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := loaded.Strategies["s1"].Positions["BTC"].TradePositionID; got != "spot-position-1" {
		t.Errorf("position TradePositionID = %q, want spot-position-1", got)
	}
	if got := loaded.Strategies["s1"].OptionPositions["BTC-call-buy-65000-2026-12-31"].TradePositionID; got != "option-position-1" {
		t.Errorf("option TradePositionID = %q, want option-position-1", got)
	}
}

func TestSaveLoadState_ATRMethodAtOpenRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"hl-eth": {
				ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
				Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{
					"ETH": {Symbol: "ETH", Quantity: 0.5, AvgCost: 3000, Side: "long", OpenedAt: now, ATRMethodAtOpen: ATRMethodWilder},
					"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long", OpenedAt: now},
				},
				TradeHistory: []Trade{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := loaded.Strategies["hl-eth"].Positions["ETH"].ATRMethodAtOpen; got != ATRMethodWilder {
		t.Errorf("ATRMethodAtOpen = %q, want %q", got, ATRMethodWilder)
	}
	if got := loaded.Strategies["hl-eth"].Positions["BTC"].ATRMethodAtOpen; got != "" {
		t.Errorf("pre-#1277 position ATRMethodAtOpen = %q, want empty", got)
	}
}

func TestSaveStateFlushWritesTradePositionID(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"s1": {
				ID: "s1", Type: "spot", Platform: "binanceus",
				Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				TradeHistory: []Trade{{
					Timestamp: now, StrategyID: "s1", Symbol: "BTC", PositionID: "position-save-fallback",
					Side: "sell", Quantity: 1, Price: 110, Value: 110, TradeType: "spot",
					Details: "Close long, PnL: $10", IsClose: true, RealizedPnL: 10,
				}},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	var got string
	if err := db.db.QueryRow("SELECT position_id FROM trades WHERE strategy_id = 's1'").Scan(&got); err != nil {
		t.Fatalf("query position_id: %v", err)
	}
	if got != "position-save-fallback" {
		t.Errorf("position_id = %q, want position-save-fallback", got)
	}
}

func TestLoadState_TradeHistoryBoundedInSQL(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.db.Exec("INSERT INTO app_state (id, cycle_count) VALUES (1, 1)"); err != nil {
		t.Fatalf("seed app_state: %v", err)
	}
	if _, err := db.db.Exec("INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES (?, ?, ?, ?, ?)", "s1", "perps", "hyperliquid", 1000.0, 1000.0); err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = maxTradeHistory + 200
	for i := 0; i < total; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		if _, err := db.db.Exec(`INSERT INTO trades
			(strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details, is_close, realized_pnl)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"s1", formatTime(ts), "BTC", "buy", 0.1, 50000.0, 5000.0, "perps", "open", 0, 0.0,
		); err != nil {
			t.Fatalf("seed trade %d: %v", i, err)
		}
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	trades := loaded.Strategies["s1"].TradeHistory
	if len(trades) != maxTradeHistory {
		t.Fatalf("trade count = %d, want %d", len(trades), maxTradeHistory)
	}
	wantOldest := base.Add(time.Duration(total-maxTradeHistory) * time.Minute)
	wantNewest := base.Add(time.Duration(total-1) * time.Minute)
	if !trades[0].Timestamp.Equal(wantOldest) {
		t.Errorf("trades[0].Timestamp = %v, want %v (oldest surviving trade)", trades[0].Timestamp, wantOldest)
	}
	if !trades[len(trades)-1].Timestamp.Equal(wantNewest) {
		t.Errorf("trades[last].Timestamp = %v, want %v (newest trade)", trades[len(trades)-1].Timestamp, wantNewest)
	}
	for i := 1; i < len(trades); i++ {
		if trades[i].Timestamp.Before(trades[i-1].Timestamp) {
			t.Fatalf("trades not in ascending chronological order at index %d: %v before %v", i, trades[i].Timestamp, trades[i-1].Timestamp)
		}
	}

	rows, err := db.db.Query(`EXPLAIN QUERY PLAN SELECT timestamp FROM trades WHERE strategy_id = ? ORDER BY timestamp DESC, rowid DESC LIMIT ?`, "s1", maxTradeHistory)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns: %v", err)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		for _, v := range vals {
			fmt.Fprintf(&plan, "%v ", v)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate explain rows: %v", err)
	}
	planStr := plan.String()
	if strings.Contains(planStr, "SCAN TABLE trades") {
		t.Errorf("query plan does a full table scan, want an index-satisfied plan: %s", planStr)
	}
	if !strings.Contains(planStr, "idx_trades_strategy_timestamp") {
		t.Errorf("query plan does not use idx_trades_strategy_timestamp: %s", planStr)
	}
}

func TestLoadState_TradePositionIDRoundTripAndLegacyNull(t *testing.T) {
	db := openNullablePositionIDDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	if _, err := db.db.Exec("INSERT INTO app_state (id, cycle_count) VALUES (1, 1)"); err != nil {
		t.Fatalf("seed app_state: %v", err)
	}
	if _, err := db.db.Exec("INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES (?, ?, ?, ?, ?)", "s1", "perps", "hyperliquid", 1000.0, 1000.0); err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	rows := []struct {
		positionID any
		ts         time.Time
	}{
		{"position-load-roundtrip", now},
		{nil, now.Add(time.Second)},
	}
	for i, row := range rows {
		if _, err := db.db.Exec(`INSERT INTO trades
			(strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, details, is_close, realized_pnl)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"s1", formatTime(row.ts), "BTC", row.positionID, "sell", 0.1, 50000.0, 5000.0, "perps", "Close long, PnL: $1", 1, float64(i+1),
		); err != nil {
			t.Fatalf("seed trade %d: %v", i, err)
		}
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	trades := loaded.Strategies["s1"].TradeHistory
	if len(trades) != 2 {
		t.Fatalf("trade count = %d, want 2", len(trades))
	}
	if got := trades[0].PositionID; got != "position-load-roundtrip" {
		t.Errorf("trade[0].PositionID = %q, want position-load-roundtrip", got)
	}
	if got := trades[1].PositionID; got != "" {
		t.Errorf("legacy NULL PositionID = %q, want empty string", got)
	}
}

func TestQueryTradeHistory_PositionIDRoundTripAndLegacyNull(t *testing.T) {
	db := openNullablePositionIDDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)
	rows := []struct {
		positionID any
		ts         time.Time
	}{
		{"position-query-roundtrip", now},
		{nil, now.Add(time.Second)},
	}
	for i, row := range rows {
		if _, err := db.db.Exec(`INSERT INTO trades
			(strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, details, is_close, realized_pnl)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"s1", formatTime(row.ts), "BTC", row.positionID, "sell", 0.1, 50000.0, 5000.0, "perps", "Close long, PnL: $1", 1, float64(i+1),
		); err != nil {
			t.Fatalf("seed trade %d: %v", i, err)
		}
	}
	trades, total, err := db.QueryTradeHistory("s1", "", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	if total != 2 || len(trades) != 2 {
		t.Fatalf("total=%d len=%d, want 2/2", total, len(trades))
	}
	if got := trades[0].PositionID; got != "" {
		t.Errorf("newest legacy NULL PositionID = %q, want empty string", got)
	}
	if got := trades[1].PositionID; got != "position-query-roundtrip" {
		t.Errorf("oldest PositionID = %q, want position-query-roundtrip", got)
	}
}

func TestMigrateSchema_AddsExchangeColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	oldSchema := `
	CREATE TABLE IF NOT EXISTS app_state (
	    id INTEGER PRIMARY KEY CHECK (id = 1),
	    cycle_count INTEGER NOT NULL DEFAULT 0,
	    last_cycle TEXT NOT NULL DEFAULT '',
	    last_top10_summary TEXT NOT NULL DEFAULT '',
	    last_leaderboard_post_date TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS strategies (
	    id TEXT PRIMARY KEY,
	    type TEXT NOT NULL,
	    platform TEXT NOT NULL DEFAULT '',
	    cash REAL NOT NULL DEFAULT 0,
	    initial_capital REAL NOT NULL DEFAULT 0,
	    risk_peak_value REAL NOT NULL DEFAULT 0,
	    risk_max_drawdown_pct REAL NOT NULL DEFAULT 0,
	    risk_current_drawdown_pct REAL NOT NULL DEFAULT 0,
	    risk_daily_pnl REAL NOT NULL DEFAULT 0,
	    risk_daily_pnl_date TEXT NOT NULL DEFAULT '',
	    risk_consecutive_losses INTEGER NOT NULL DEFAULT 0,
	    risk_circuit_breaker INTEGER NOT NULL DEFAULT 0,
	    risk_circuit_breaker_until TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS positions (
	    strategy_id TEXT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
	    symbol TEXT NOT NULL,
	    quantity REAL NOT NULL,
	    avg_cost REAL NOT NULL,
	    side TEXT NOT NULL,
	    multiplier REAL NOT NULL DEFAULT 0,
	    owner_strategy_id TEXT NOT NULL DEFAULT '',
	    PRIMARY KEY (strategy_id, symbol)
	);
	CREATE TABLE IF NOT EXISTS option_positions (
	    strategy_id TEXT NOT NULL, id TEXT NOT NULL, underlying TEXT NOT NULL,
	    option_type TEXT NOT NULL, strike REAL NOT NULL, expiry TEXT NOT NULL,
	    dte REAL NOT NULL DEFAULT 0, action TEXT NOT NULL, quantity REAL NOT NULL,
	    entry_premium REAL NOT NULL DEFAULT 0, entry_premium_usd REAL NOT NULL DEFAULT 0,
	    current_value_usd REAL NOT NULL DEFAULT 0, delta REAL NOT NULL DEFAULT 0,
	    gamma REAL NOT NULL DEFAULT 0, theta REAL NOT NULL DEFAULT 0,
	    vega REAL NOT NULL DEFAULT 0, opened_at TEXT NOT NULL DEFAULT '',
	    PRIMARY KEY (strategy_id, id)
	);
	CREATE TABLE IF NOT EXISTS trades (
	    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
	    strategy_id TEXT NOT NULL,
	    timestamp TEXT NOT NULL,
	    symbol TEXT NOT NULL,
	    side TEXT NOT NULL,
	    quantity REAL NOT NULL,
	    price REAL NOT NULL,
	    value REAL NOT NULL,
	    trade_type TEXT NOT NULL DEFAULT '',
	    details TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS portfolio_risk (
	    id INTEGER PRIMARY KEY CHECK (id = 1),
	    peak_value REAL NOT NULL DEFAULT 0,
	    current_drawdown_pct REAL NOT NULL DEFAULT 0,
	    kill_switch_active INTEGER NOT NULL DEFAULT 0,
	    kill_switch_at TEXT NOT NULL DEFAULT '',
	    warning_sent INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS kill_switch_events (
	    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
	    timestamp TEXT NOT NULL,
	    type TEXT NOT NULL,
	    drawdown_pct REAL NOT NULL DEFAULT 0,
	    portfolio_value REAL NOT NULL DEFAULT 0,
	    peak_value REAL NOT NULL DEFAULT 0,
	    details TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS correlation_snapshot (
	    id INTEGER PRIMARY KEY CHECK (id = 1),
	    snapshot_json TEXT NOT NULL DEFAULT '{}'
	);`

	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO app_state (id, cycle_count) VALUES (1, 1)`); err != nil {
		t.Fatalf("insert app_state: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO strategies (id, type) VALUES ('test', 'perps')`); err != nil {
		t.Fatalf("insert strategy: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO positions (strategy_id, symbol, quantity, avg_cost, side, multiplier, owner_strategy_id)
		VALUES ('test', 'BTC', 0.5, 40000, 'long', 1, 'test')`); err != nil {
		t.Fatalf("insert old position: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details)
		VALUES ('test', '2026-01-01T00:00:00Z', 'BTC', 'buy', 0.1, 50000, 5000, 'perps', 'old trade')`); err != nil {
		t.Fatalf("insert old trade: %v", err)
	}
	db.Close()

	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("OpenStateDB after migration: %v", err)
	}
	defer sdb.Close()

	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("LoadState after migration: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded state is nil")
	}
	if len(loaded.LastSummaryPost) != 0 {
		t.Fatalf("migrated LastSummaryPost = %v, want empty", loaded.LastSummaryPost)
	}
	strat := loaded.Strategies["test"]
	if strat == nil {
		t.Fatal("missing strategy 'test'")
	}
	if len(strat.TradeHistory) != 1 {
		t.Fatalf("trade count = %d, want 1", len(strat.TradeHistory))
	}
	pos := strat.Positions["BTC"]
	if pos == nil {
		t.Fatal("migrated position missing")
	}
	if pos.InitialQuantity != 0 {
		t.Errorf("migrated position InitialQuantity = %g, want 0", pos.InitialQuantity)
	}
	if pos.EntryATR != 0 {
		t.Errorf("migrated position EntryATR = %g, want 0", pos.EntryATR)
	}
	tr := strat.TradeHistory[0]
	if tr.ExchangeOrderID != "" {
		t.Errorf("migrated trade ExchangeOrderID = %q, want empty", tr.ExchangeOrderID)
	}
	if tr.ExchangeFee != 0 {
		t.Errorf("migrated trade ExchangeFee = %g, want 0", tr.ExchangeFee)
	}

	strat.TradeHistory = append(strat.TradeHistory, Trade{
		Timestamp: time.Now().UTC(), StrategyID: "test", Symbol: "BTC", Side: "sell",
		Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps",
		Details: "new live trade", ExchangeOrderID: "999888777", ExchangeFee: 1.50,
	})
	if err := sdb.SaveState(loaded); err != nil {
		t.Fatalf("SaveState with new trade: %v", err)
	}

	loaded2, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("LoadState after new trade: %v", err)
	}
	trades := loaded2.Strategies["test"].TradeHistory
	if len(trades) != 2 {
		t.Fatalf("trade count = %d, want 2", len(trades))
	}
	if trades[1].ExchangeOrderID != "999888777" {
		t.Errorf("new trade ExchangeOrderID = %q, want %q", trades[1].ExchangeOrderID, "999888777")
	}
	if trades[1].ExchangeFee != 1.50 {
		t.Errorf("new trade ExchangeFee = %g, want 1.50", trades[1].ExchangeFee)
	}
}

func TestClosedPositions_Flush(t *testing.T) {
	sdb := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"test": {
				ID: "test", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				TradeHistory: []Trade{},
				ClosedPositions: []ClosedPosition{
					{
						StrategyID: "test", Symbol: "BTC", Quantity: 0.1, AvgCost: 50000,
						Side: "long", Multiplier: 0,
						OpenedAt: now.Add(-24 * time.Hour), ClosedAt: now,
						ClosePrice: 52000, RealizedPnL: 200,
						CloseReason: "signal", DurationSeconds: 86400,
					},
				},
			},
		},
	}

	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if len(state.Strategies["test"].ClosedPositions) != 0 {
		t.Errorf("ClosedPositions buffer not cleared after save, len=%d", len(state.Strategies["test"].ClosedPositions))
	}

	var count int
	if err := sdb.db.QueryRow("SELECT COUNT(*) FROM closed_positions").Scan(&count); err != nil {
		t.Fatalf("count closed_positions: %v", err)
	}
	if count != 1 {
		t.Fatalf("closed_positions rows = %d, want 1", count)
	}

	rows, total, err := sdb.QueryClosedPositions("", "", time.Time{}, time.Time{}, 10, 0)
	if err != nil {
		t.Fatalf("QueryClosedPositions: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("QueryClosedPositions: total=%d len=%d, want 1/1", total, len(rows))
	}
	cp := rows[0]
	if cp.Symbol != "BTC" || cp.Side != "long" || cp.RealizedPnL != 200 || cp.CloseReason != "signal" {
		t.Errorf("closed_position mismatch: %+v", cp)
	}
	if cp.DurationSeconds != 86400 {
		t.Errorf("DurationSeconds = %d, want 86400", cp.DurationSeconds)
	}
	if cp.OpenedAt.IsZero() || cp.ClosedAt.IsZero() {
		t.Errorf("timestamps should round-trip, got opened=%v closed=%v", cp.OpenedAt, cp.ClosedAt)
	}

	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("second SaveState: %v", err)
	}
	if err := sdb.db.QueryRow("SELECT COUNT(*) FROM closed_positions").Scan(&count); err != nil {
		t.Fatalf("count after second save: %v", err)
	}
	if count != 1 {
		t.Errorf("closed_positions rows after re-save = %d, want 1", count)
	}
}

func TestRecordClosedPosition_ExecuteSignal(t *testing.T) {
	openedAt := time.Now().UTC().Add(-2 * time.Hour)
	s := &StrategyState{
		ID: "test", Type: "spot", Platform: "binanceus",
		Cash: 0,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 1.0, AvgCost: 100, Side: "long", OpenedAt: openedAt},
		},
	}
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	if _, err := ExecuteSpotSignalWithFillFee(s, -1, "BTC", 110, 0, 0, "", 0, logger); err != nil {
		t.Fatalf("ExecuteSpotSignalWithFillFee: %v", err)
	}
	if _, exists := s.Positions["BTC"]; exists {
		t.Fatal("position should have been closed")
	}
	if len(s.ClosedPositions) != 1 {
		t.Fatalf("ClosedPositions len = %d, want 1", len(s.ClosedPositions))
	}
	cp := s.ClosedPositions[0]
	if cp.Symbol != "BTC" || cp.Side != "long" {
		t.Errorf("closed position mismatch: %+v", cp)
	}
	if cp.CloseReason != "signal" {
		t.Errorf("CloseReason = %q, want %q", cp.CloseReason, "signal")
	}
	if cp.RealizedPnL <= 0 {
		t.Errorf("RealizedPnL = %g, expected positive (bought @100 sold @~110)", cp.RealizedPnL)
	}
	if cp.DurationSeconds < 7100 || cp.DurationSeconds > 7300 {
		t.Errorf("DurationSeconds = %d, expected ~7200 (2h)", cp.DurationSeconds)
	}
	if !cp.OpenedAt.Equal(openedAt) {
		t.Errorf("OpenedAt mismatch: got %v, want %v", cp.OpenedAt, openedAt)
	}
}

func TestQueryClosedPositions_Filters(t *testing.T) {
	sdb := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"s1": {
				ID: "s1", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				ClosedPositions: []ClosedPosition{
					{StrategyID: "s1", Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "long", OpenedAt: now.Add(-3 * time.Hour), ClosedAt: now.Add(-2 * time.Hour), ClosePrice: 110, RealizedPnL: 10, CloseReason: "signal", DurationSeconds: 3600},
					{StrategyID: "s1", Symbol: "ETH", Quantity: 2, AvgCost: 50, Side: "long", OpenedAt: now.Add(-2 * time.Hour), ClosedAt: now.Add(-1 * time.Hour), ClosePrice: 60, RealizedPnL: 20, CloseReason: "signal", DurationSeconds: 3600},
				},
			},
			"s2": {
				ID: "s2", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				ClosedPositions: []ClosedPosition{
					{StrategyID: "s2", Symbol: "BTC", Quantity: 1, AvgCost: 100, Side: "short", OpenedAt: now.Add(-4 * time.Hour), ClosedAt: now.Add(-30 * time.Minute), ClosePrice: 90, RealizedPnL: 10, CloseReason: "circuit_breaker", DurationSeconds: 12600},
				},
			},
		},
	}
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("first SaveState: %v", err)
	}

	rows, total, err := sdb.QueryClosedPositions("s1", "", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("filter strategy: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("strategy filter: total=%d len=%d, want 2/2", total, len(rows))
	}
	for _, cp := range rows {
		if cp.StrategyID != "s1" {
			t.Errorf("strategy filter leaked %q", cp.StrategyID)
		}
	}

	rows, total, err = sdb.QueryClosedPositions("", "BTC", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("filter symbol: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("symbol filter: total=%d len=%d, want 2/2", total, len(rows))
	}

	since := now.Add(-90 * time.Minute)
	rows, total, err = sdb.QueryClosedPositions("", "", since, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("filter since: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("since filter: total=%d len=%d, want 2/2", total, len(rows))
	}

	until := now.Add(-45 * time.Minute)
	rows, total, err = sdb.QueryClosedPositions("", "", time.Time{}, until, 50, 0)
	if err != nil {
		t.Fatalf("filter until: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("until filter: total=%d len=%d, want 2/2 (s1 BTC + s1 ETH)", total, len(rows))
	}

	rows, total, err = sdb.QueryClosedPositions("s2", "BTC", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("combined filter: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Errorf("combined filter: total=%d len=%d, want 1/1", total, len(rows))
	}
	if len(rows) == 1 && rows[0].CloseReason != "circuit_breaker" {
		t.Errorf("combined filter close_reason=%q, want circuit_breaker", rows[0].CloseReason)
	}

	state.Strategies["s1"].ClosedPositions = []ClosedPosition{
		{StrategyID: "s1", Symbol: "SOL", Quantity: 5, AvgCost: 20, Side: "long", OpenedAt: now.Add(-30 * time.Minute), ClosedAt: now, ClosePrice: 25, RealizedPnL: 25, CloseReason: "signal", DurationSeconds: 1800},
	}
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("second SaveState: %v", err)
	}
	_, total, err = sdb.QueryClosedPositions("", "", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("total after second save: %v", err)
	}
	if total != 4 {
		t.Errorf("total after append = %d, want 4 (3 original + 1 new)", total)
	}
}

func TestClosedOptionPositions_Flush(t *testing.T) {
	sdb := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"test": {
				ID: "test", Type: "options", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				ClosedOptionPositions: []ClosedOptionPosition{
					{
						StrategyID: "test", PositionID: "BTC-call-buy-55000-2026-05-01",
						Underlying: "BTC", OptionType: "call", Strike: 55000,
						Expiry: "2026-05-01", Action: "buy", Quantity: 1,
						EntryPremiumUSD: 2500, ClosePriceUSD: 3000, RealizedPnL: 500,
						OpenedAt: now.Add(-24 * time.Hour), ClosedAt: now,
						CloseReason: "signal", DurationSeconds: 86400,
					},
				},
			},
		},
	}
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if len(state.Strategies["test"].ClosedOptionPositions) != 0 {
		t.Errorf("ClosedOptionPositions buffer not cleared")
	}

	rows, total, err := sdb.QueryClosedOptionPositions("", "", time.Time{}, time.Time{}, 10, 0)
	if err != nil {
		t.Fatalf("QueryClosedOptionPositions: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("total=%d len=%d, want 1/1", total, len(rows))
	}
	cp := rows[0]
	if cp.Underlying != "BTC" || cp.OptionType != "call" || cp.Strike != 55000 {
		t.Errorf("mismatch: %+v", cp)
	}
	if cp.RealizedPnL != 500 || cp.CloseReason != "signal" {
		t.Errorf("pnl/reason mismatch: %+v", cp)
	}
	if cp.DurationSeconds != 86400 {
		t.Errorf("DurationSeconds = %d, want 86400", cp.DurationSeconds)
	}

	rows, total, err = sdb.QueryClosedOptionPositions("", "ETH", time.Time{}, time.Time{}, 10, 0)
	if err != nil {
		t.Fatalf("filter underlying: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Errorf("ETH filter should be empty, got total=%d", total)
	}
}

func TestRecordClosedOptionPosition_ExecuteClose(t *testing.T) {
	openedAt := time.Now().UTC().Add(-3 * time.Hour)
	pos := &OptionPosition{
		ID: "BTC-call-buy-55000-2026-05-01", Underlying: "BTC", OptionType: "call",
		Strike: 55000, Expiry: "2026-05-01", Action: "buy", Quantity: 1,
		EntryPremium: 0.04, EntryPremiumUSD: 2000, CurrentValueUSD: 2500,
		OpenedAt: openedAt,
	}
	s := &StrategyState{
		ID: "test", Type: "options", Platform: "deribit",
		Cash:            0,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{pos.ID: pos},
	}
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	result := &OptionsResult{
		Underlying: "BTC", SpotPrice: 60000, Signal: -1,
		Actions: []OptionsAction{{Action: "close", OptionType: "call", Strike: 55000, PremiumUSD: 2500}},
	}
	if _, err := ExecuteOptionsSignal(s, result, logger); err != nil {
		t.Fatalf("ExecuteOptionsSignal: %v", err)
	}
	if _, exists := s.OptionPositions[pos.ID]; exists {
		t.Fatal("option should have been closed")
	}
	if len(s.ClosedOptionPositions) != 1 {
		t.Fatalf("ClosedOptionPositions len=%d, want 1", len(s.ClosedOptionPositions))
	}
	cp := s.ClosedOptionPositions[0]
	if cp.CloseReason != "signal" {
		t.Errorf("CloseReason=%q, want signal", cp.CloseReason)
	}
	if cp.RealizedPnL <= 0 {
		t.Errorf("RealizedPnL=%g, want positive", cp.RealizedPnL)
	}
	if cp.DurationSeconds < 10700 || cp.DurationSeconds > 11000 {
		t.Errorf("DurationSeconds=%d, want ~10800 (3h)", cp.DurationSeconds)
	}
}

func TestMigrateSchema_AddsOpenedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE positions (
		strategy_id TEXT NOT NULL,
		symbol TEXT NOT NULL,
		quantity REAL NOT NULL,
		avg_cost REAL NOT NULL,
		side TEXT NOT NULL,
		multiplier REAL NOT NULL DEFAULT 0,
		owner_strategy_id TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (strategy_id, symbol)
	)`); err != nil {
		t.Fatalf("create legacy positions: %v", err)
	}
	legacy.Close()

	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	defer db.Close()

	var colCount int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('positions') WHERE name='opened_at'`).Scan(&colCount); err != nil {
		t.Fatalf("pragma query: %v", err)
	}
	if colCount != 1 {
		t.Errorf("opened_at column not added, count=%d", colCount)
	}
}

func TestSaveLoadState_TimestampMaps(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		name  string
		set   func(*AppState, map[string]time.Time)
		get   func(*AppState) map[string]time.Time
		value map[string]time.Time
	}{
		{
			name:  "last leaderboard summaries",
			set:   func(s *AppState, m map[string]time.Time) { s.LastLeaderboardSummaries = m },
			get:   func(s *AppState) map[string]time.Time { return s.LastLeaderboardSummaries },
			value: map[string]time.Time{"hyperliquid:*:123": now.Add(-1 * time.Hour), "hyperliquid:eth:456": now.Add(-2 * time.Hour)},
		},
		{
			name:  "last summary post",
			set:   func(s *AppState, m map[string]time.Time) { s.LastSummaryPost = m },
			get:   func(s *AppState) map[string]time.Time { return s.LastSummaryPost },
			value: map[string]time.Time{"spot": now.Add(-5 * time.Minute), "hyperliquid": now.Add(-30 * time.Minute)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sdb := openTestDB(t)
			state := NewAppState()
			tc.set(state, tc.value)
			if err := sdb.SaveState(state); err != nil {
				t.Fatalf("save: %v", err)
			}
			loaded, err := sdb.LoadState()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			got := tc.get(loaded)
			if len(got) != len(tc.value) {
				t.Fatalf("expected %d entries, got %d", len(tc.value), len(got))
			}
			for k, want := range tc.value {
				g, ok := got[k]
				if !ok {
					t.Errorf("key %q missing after reload", k)
					continue
				}
				if !g.Equal(want) {
					t.Errorf("key %q: got %v, want %v", k, g, want)
				}
			}
		})
	}
}

func TestSaveState_InitialCapitalGuard(t *testing.T) {
	strat := func(id string, cash, initial float64) *StrategyState {
		return &StrategyState{
			ID: id, Type: "spot", Cash: cash, InitialCapital: initial,
			Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
		}
	}
	type book struct{ initial, cash float64 }
	cases := []struct {
		name         string
		seed         map[string]*StrategyState
		save         map[string]*StrategyState
		wantInMemory map[string]book
		wantDB       map[string]book
	}{
		{
			name:         "existing baseline is preserved and restored in memory",
			seed:         map[string]*StrategyState{"hl-tema-eth": strat("hl-tema-eth", 505, 505)},
			save:         map[string]*StrategyState{"hl-tema-eth": strat("hl-tema-eth", 632, 632)},
			wantInMemory: map[string]book{"hl-tema-eth": {505, 632}},
			wantDB:       map[string]book{"hl-tema-eth": {505, 632}},
		},
		{
			name:         "first write lands",
			save:         map[string]*StrategyState{"new-strat": strat("new-strat", 1000, 1000)},
			wantInMemory: map[string]book{"new-strat": {1000, 1000}},
			wantDB:       map[string]book{"new-strat": {1000, 1000}},
		},
		{
			name: "new strategy alongside existing one",
			seed: map[string]*StrategyState{"old": strat("old", 1000, 1000)},
			save: map[string]*StrategyState{
				"old":       strat("old", 1000, 1000),
				"brand-new": strat("brand-new", 2000, 2000),
			},
			wantInMemory: map[string]book{"old": {1000, 1000}, "brand-new": {2000, 2000}},
			wantDB:       map[string]book{"old": {1000, 1000}, "brand-new": {2000, 2000}},
		},
		{
			name:         "prev zero allows baseline establishment",
			seed:         map[string]*StrategyState{"legacy": strat("legacy", 0, 0)},
			save:         map[string]*StrategyState{"legacy": strat("legacy", 1000, 1000)},
			wantInMemory: map[string]book{"legacy": {1000, 1000}},
			wantDB:       map[string]book{"legacy": {1000, 1000}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			if tc.seed != nil {
				if err := db.SaveState(&AppState{Strategies: tc.seed}); err != nil {
					t.Fatalf("seed SaveState: %v", err)
				}
			}
			state := &AppState{Strategies: tc.save}
			if err := db.SaveState(state); err != nil {
				t.Fatalf("SaveState: %v", err)
			}
			for id, w := range tc.wantInMemory {
				s := state.Strategies[id]
				if s.InitialCapital != w.initial || s.Cash != w.cash {
					t.Errorf("in-memory %s = (initial %g, cash %g), want (%g, %g)", id, s.InitialCapital, s.Cash, w.initial, w.cash)
				}
			}
			loaded, err := db.LoadState()
			if err != nil {
				t.Fatalf("LoadState: %v", err)
			}
			for id, w := range tc.wantDB {
				s := loaded.Strategies[id]
				if s == nil {
					t.Fatalf("persisted strategy %s missing", id)
				}
				if s.InitialCapital != w.initial || s.Cash != w.cash {
					t.Errorf("persisted %s = (initial %g, cash %g), want (%g, %g)", id, s.InitialCapital, s.Cash, w.initial, w.cash)
				}
			}
		})
	}
}

func TestSetInitialCapital(t *testing.T) {
	seed := func(t *testing.T, cash float64) *StateDB {
		db := openTestDB(t)
		state := &AppState{
			Strategies: map[string]*StrategyState{
				"s": {
					ID: "s", Type: "spot", Cash: cash, InitialCapital: cash,
					Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				},
			},
		}
		if err := db.SaveState(state); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		return db
	}
	t.Run("explicit override sticks across a stale save", func(t *testing.T) {
		db := seed(t, 505)
		if err := db.SetInitialCapital("s", 750); err != nil {
			t.Fatalf("SetInitialCapital: %v", err)
		}
		state := &AppState{
			Strategies: map[string]*StrategyState{
				"s": {
					ID: "s", Type: "spot", Cash: 505, InitialCapital: 505,
					Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				},
			},
		}
		if err := db.SaveState(state); err != nil {
			t.Fatalf("SaveState after override: %v", err)
		}
		loaded, err := db.LoadState()
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		if got := loaded.Strategies["s"].InitialCapital; got != 750 {
			t.Errorf("initial_capital = %g, want 750 (override must stick)", got)
		}
	})
	t.Run("rejects zero, negative, and unknown strategy", func(t *testing.T) {
		db := seed(t, 1000)
		if err := db.SetInitialCapital("s", 0); err == nil {
			t.Error("expected error for zero initial_capital")
		}
		if err := db.SetInitialCapital("s", -100); err == nil {
			t.Error("expected error for negative initial_capital")
		}
		if err := db.SetInitialCapital("unknown-id", 1000); err == nil {
			t.Error("expected error for unknown strategy id")
		}
	})
}

func TestPersistSharedWalletPoolStateTransitionRoundTrip(t *testing.T) {
	db := openTestDB(t)
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {
			ID: "hl-a", Type: "perps", Platform: "hyperliquid",
			Cash: 1000, InitialCapital: 1000,
			Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			RiskState: RiskState{PeakValue: 1000},
		},
	}}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	s := state.Strategies["hl-a"]
	poolCfg := StrategyConfig{ID: "hl-a", Type: "perps", sharedWalletPoolBudget: true}
	if transition, err := applySharedWalletPoolStateMode(poolCfg, s); err != nil || transition != sharedWalletPoolStateEntered {
		t.Fatalf("enter pool: transition=%q err=%v", transition, err)
	}
	if err := db.PersistSharedWalletPoolStateTransition(s); err != nil {
		t.Fatalf("persist pool entry: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("load pool state: %v", err)
	}
	pooled := loaded.Strategies["hl-a"]
	if !pooled.SharedWalletPoolBudget || !pooled.SharedWalletPerformanceOnly || pooled.Cash != 0 || pooled.InitialCapital != 0 {
		t.Fatalf("pool entry did not round-trip: %+v", pooled)
	}

	pooled.Cash = -100
	if err := db.SaveState(loaded); err != nil {
		t.Fatalf("persist pool performance book: %v", err)
	}
	loaded, err = db.LoadState()
	if err != nil {
		t.Fatalf("reload pool performance book: %v", err)
	}
	ValidateState(loaded, nil)
	pooled = loaded.Strategies["hl-a"]
	if pooled.Cash != -100 {
		t.Fatalf("pool loss must survive reload validation, cash=%v", pooled.Cash)
	}
	allocatedCfg := StrategyConfig{ID: "hl-a", Type: "perps", Capital: 1000}
	if transition, err := applySharedWalletPoolStateMode(allocatedCfg, pooled); err != nil || transition != sharedWalletPoolStateLeft {
		t.Fatalf("leave pool: transition=%q err=%v", transition, err)
	}
	if err := db.PersistSharedWalletPoolStateTransition(pooled); err != nil {
		t.Fatalf("persist pool exit: %v", err)
	}
	loaded, err = db.LoadState()
	if err != nil {
		t.Fatalf("load allocated state: %v", err)
	}
	allocated := loaded.Strategies["hl-a"]
	if allocated.SharedWalletPoolBudget || allocated.SharedWalletPerformanceOnly || allocated.Cash != 900 || allocated.InitialCapital != 1000 {
		t.Fatalf("pool exit did not round-trip exactly once: %+v", allocated)
	}
}

func TestSaveState_GuardWarnIsOneShot(t *testing.T) {
	db := openTestDB(t)

	var warns int
	prev := initialCapitalGuardWarn
	initialCapitalGuardWarn = func(string) { warns++ }
	t.Cleanup(func() { initialCapitalGuardWarn = prev })

	seed := &AppState{
		Strategies: map[string]*StrategyState{
			"s": {
				ID: "s", Type: "spot", Cash: 100, InitialCapital: 100,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(seed); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}

	bad := &AppState{
		Strategies: map[string]*StrategyState{
			"s": {
				ID: "s", Type: "spot", Cash: 100, InitialCapital: 200,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	for i := 0; i < 5; i++ {
		if err := db.SaveState(bad); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if warns != 1 {
		t.Errorf("warn fired %d times, want 1 (one-shot per strategy)", warns)
	}

	if err := db.SetInitialCapital("s", 200); err != nil {
		t.Fatalf("SetInitialCapital: %v", err)
	}
	stillBad := &AppState{
		Strategies: map[string]*StrategyState{
			"s": {
				ID: "s", Type: "spot", Cash: 100, InitialCapital: 50,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(stillBad); err != nil {
		t.Fatalf("stillBad save: %v", err)
	}
	if warns != 2 {
		t.Errorf("warn fired %d times after override, want 2 (dedup must reset)", warns)
	}
}

func TestSaveAndLoadDB_PendingCircuitClose(t *testing.T) {
	seed := func(t *testing.T, pending map[string]*PendingCircuitClose) *StateDB {
		db := openTestDB(t)
		state := &AppState{
			CycleCount: 1,
			LastCycle:  time.Now().UTC().Truncate(time.Second),
			Strategies: map[string]*StrategyState{
				"hl-a": {
					ID: "hl-a", Type: "perps", Platform: "hyperliquid", Cash: 100, InitialCapital: 100,
					Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
					RiskState: RiskState{PeakValue: 100, MaxDrawdownPct: 25, PendingCircuitCloses: pending},
				},
			},
		}
		if err := db.SaveState(state); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		return db
	}
	assertPending := func(t *testing.T, db *StateDB, label string) {
		loaded, err := db.LoadState()
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		p := loaded.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
		if p == nil || len(p.Symbols) != 1 {
			t.Fatalf("%s pending missing: %+v", label, p)
		}
		if p.Symbols[0].Symbol != "ETH" || p.Symbols[0].Size != 0.2585 {
			t.Errorf("%s pending symbol=%q size=%g want ETH 0.2585", label, p.Symbols[0].Symbol, p.Symbols[0].Size)
		}
	}
	t.Run("round trip", func(t *testing.T) {
		db := seed(t, map[string]*PendingCircuitClose{
			PlatformPendingCloseHyperliquid: {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.2585}}},
		})
		assertPending(t, db, "round-trip")
	})
	t.Run("legacy pending hl json migrates on load", func(t *testing.T) {
		db := seed(t, nil)
		if _, err := db.db.Exec(
			"UPDATE strategies SET risk_pending_circuit_closes_json = ? WHERE id = ?",
			`{"coins":[{"coin":"ETH","sz":0.2585}]}`, "hl-a",
		); err != nil {
			t.Fatalf("inject legacy JSON: %v", err)
		}
		assertPending(t, db, "legacy-migrated")
	})
}

func TestMigrateSchema_PendingCircuitClosesColumn_Idempotent(t *testing.T) {
	db := openTestDB(t)
	if err := db.migrateSchema(); err != nil {
		t.Fatalf("second migrateSchema: %v", err)
	}
	if err := db.migrateSchema(); err != nil {
		t.Fatalf("third migrateSchema: %v", err)
	}

	hasLegacy, hasNew, err := db.strategiesColumnPresence()
	if err != nil {
		t.Fatalf("strategiesColumnPresence: %v", err)
	}
	if !hasNew {
		t.Error("expected risk_pending_circuit_closes_json column to exist")
	}
	if hasLegacy {
		t.Error("risk_pending_hl_close_json should not be re-added on subsequent startups")
	}
}

func TestMigrateSchema_PendingCircuitClosesColumn_FromLegacyDB(t *testing.T) {
	db := openTestDB(t)

	_, err := db.db.Exec(`CREATE TABLE strategies_legacy AS SELECT
		id, type, platform, cash, initial_capital,
		risk_peak_value, risk_max_drawdown_pct, risk_current_drawdown_pct,
		risk_daily_pnl, risk_daily_pnl_date, risk_consecutive_losses,
		risk_circuit_breaker, risk_circuit_breaker_until,
		risk_pending_circuit_closes_json AS risk_pending_hl_close_json
		FROM strategies`)
	if err != nil {
		t.Fatalf("build legacy table: %v", err)
	}
	if _, err := db.db.Exec("DROP TABLE strategies"); err != nil {
		t.Fatalf("drop strategies: %v", err)
	}
	if _, err := db.db.Exec("ALTER TABLE strategies_legacy RENAME TO strategies"); err != nil {
		t.Fatalf("rename legacy table: %v", err)
	}

	if _, err := db.db.Exec(
		"INSERT INTO strategies (id, type, platform, cash, initial_capital, risk_pending_hl_close_json) VALUES (?, ?, ?, ?, ?, ?)",
		"hl-rename", "perps", "hyperliquid", 100.0, 100.0,
		`{"coins":[{"coin":"ETH","sz":0.3}]}`,
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	hasLegacy, hasNew, err := db.strategiesColumnPresence()
	if err != nil {
		t.Fatalf("pre-check presence: %v", err)
	}
	if !hasLegacy || hasNew {
		t.Fatalf("expected legacy-only table; hasLegacy=%v hasNew=%v", hasLegacy, hasNew)
	}

	if err := db.migrateSchema(); err != nil {
		t.Fatalf("migrateSchema on legacy DB: %v", err)
	}

	hasLegacy, hasNew, err = db.strategiesColumnPresence()
	if err != nil {
		t.Fatalf("post-check presence: %v", err)
	}
	if hasLegacy {
		t.Error("risk_pending_hl_close_json should be gone after rename")
	}
	if !hasNew {
		t.Error("risk_pending_circuit_closes_json should exist after rename")
	}

	var raw string
	if err := db.db.QueryRow(
		"SELECT risk_pending_circuit_closes_json FROM strategies WHERE id = ?", "hl-rename",
	).Scan(&raw); err != nil {
		t.Fatalf("read renamed column: %v", err)
	}
	if raw != `{"coins":[{"coin":"ETH","sz":0.3}]}` {
		t.Errorf("row data lost in rename; got %q", raw)
	}
}

func TestParseDetailsPnL(t *testing.T) {
	cases := []struct {
		name    string
		details string
		want    float64
		ok      bool
	}{
		{"close_long_perps", "Close long, PnL: $42.50 (fee $0.21)", 42.50, true},
		{"close_short_spot", "Close short, PnL: $-1.23 (fee $0.10)", -1.23, true},
		{"options_close", "Close BTC-call-50000-2026-05-01 PnL=$7.89", 7.89, true},
		{"theta_harvest", "Theta harvest close ETH-put-3000-2026-05-15 PnL=$-4.20", -4.20, true},
		{"circuit_breaker", "Circuit breaker close long, PnL: $0.00", 0.0, true},
		{"wheel_callaway", "Wheel call-away: sold call expired ITM (spot=$50000.00), sold 0.1 BTC @ $51000 PnL=$100.00", 100.0, true},
		{"open_long_no_pnl", "Open long 0.500000 @ $2000.00 (1.0x, fee $0.35)", 0, false},
		{"buy_option_no_pnl", "Buy BTC call strike=50000 exp=2026-05-01 premium=$1.23 fee=$0.05", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseDetailsPnL(tc.details)
			if ok != tc.ok {
				t.Fatalf("ok=%v, want %v (details=%q)", ok, tc.ok, tc.details)
			}
			if ok && got != tc.want {
				t.Errorf("got %v, want %v (details=%q)", got, tc.want, tc.details)
			}
		})
	}
}

func TestBackfillTradeCloseFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	const oldSchema = `
CREATE TABLE app_state (id INTEGER PRIMARY KEY, cycle_count INTEGER NOT NULL DEFAULT 0);
CREATE TABLE strategies (id TEXT PRIMARY KEY, type TEXT NOT NULL DEFAULT '');
CREATE TABLE trades (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    strategy_id TEXT NOT NULL,
    timestamp TEXT NOT NULL,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL,
    quantity REAL NOT NULL,
    price REAL NOT NULL,
    value REAL NOT NULL,
    trade_type TEXT NOT NULL DEFAULT '',
    details TEXT NOT NULL DEFAULT ''
);`
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO strategies (id, type) VALUES ('s1', 'perps')`); err != nil {
		t.Fatalf("seed strategy: %v", err)
	}
	rows := []struct{ side, details string }{
		{"buy", "Open long 0.5 @ $2000.00 (1.0x, fee $0.35)"},
		{"sell", "Close long, PnL: $42.50 (fee $0.21)"},
		{"buy", "Open long 0.4 @ $2010.00 (1.0x, fee $0.30)"},
		{"sell", "Close long, PnL: $-7.10 (fee $0.20)"},
		{"close", "Theta harvest close opt-1 PnL=$3.14"},
	}
	for i, r := range rows {
		ts := time.Now().UTC().Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if _, err := db.Exec(`INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"s1", ts, "BTC", r.side, 0.1, 2000.0, 200.0, "perps", r.details); err != nil {
			t.Fatalf("seed trade %d: %v", i, err)
		}
	}
	db.Close()

	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("OpenStateDB (with migration): %v", err)
	}
	defer sdb.Close()

	stats, err := sdb.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	got := stats["s1"]
	if got.PositionsOpened != 2 {
		t.Errorf("PositionsOpened = %d, want 2 (2 open legs of 5 rows)", got.PositionsOpened)
	}
	if got.Wins != 2 {
		t.Errorf("Wins = %d, want 2 (PnL > 0: $42.50, $3.14)", got.Wins)
	}
	if got.Losses != 1 {
		t.Errorf("Losses = %d, want 1 (PnL < 0: $-7.10)", got.Losses)
	}
}

func TestLifetimeTradeStatsAll(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name       string
		trades     []Trade
		want       map[string]LifetimeTradeStats
		wantAbsent []string
	}{
		{
			name: "fresh insert counts opens wins and losses per strategy",
			trades: []Trade{
				{StrategyID: "s1", Timestamp: now, Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps", Details: "Open long"},
				{StrategyID: "s1", Timestamp: now.Add(time.Second), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps", Details: "Close long, PnL: $100.00", IsClose: true, RealizedPnL: 100},
				{StrategyID: "s1", Timestamp: now.Add(2 * time.Second), Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps", Details: "Open long"},
				{StrategyID: "s1", Timestamp: now.Add(3 * time.Second), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 50500, Value: 5050, TradeType: "perps", Details: "Close long, PnL: $-50.00", IsClose: true, RealizedPnL: -50},
				{StrategyID: "s2", Timestamp: now, Symbol: "ETH", Side: "buy", Quantity: 0.5, Price: 2000, Value: 1000, TradeType: "perps", Details: "Open long"},
				{StrategyID: "s2", Timestamp: now.Add(time.Second), Symbol: "ETH", Side: "sell", Quantity: 0.5, Price: 2100, Value: 1050, TradeType: "perps", Details: "Close long, PnL: $50.00", IsClose: true, RealizedPnL: 50},
			},
			want:       map[string]LifetimeTradeStats{"s1": {PositionsOpened: 2, Wins: 1, Losses: 1}, "s2": {PositionsOpened: 1, Wins: 1, Losses: 0}},
			wantAbsent: []string{"s3"},
		},
		{
			name: "partial closes net by position id",
			trades: []Trade{
				{StrategyID: "s1", Timestamp: now, Symbol: "BTC", PositionID: "s1-BTC-open-1", Side: "buy", Quantity: 0.5, Price: 50000, Value: 25000, TradeType: "perps", Details: "Open long"},
				{StrategyID: "s1", Timestamp: now.Add(time.Second), Symbol: "BTC", PositionID: "s1-BTC-open-1", Side: "sell", Quantity: 0.25, Price: 50100, Value: 12525, TradeType: "perps", Details: "Close long, PnL: $10.00", IsClose: true, RealizedPnL: 10},
				{StrategyID: "s1", Timestamp: now.Add(2 * time.Second), Symbol: "BTC", PositionID: "s1-BTC-open-1", Side: "sell", Quantity: 0.25, Price: 49900, Value: 12475, TradeType: "perps", Details: "Close long, PnL: $-3.00", IsClose: true, RealizedPnL: -3},
			},
			want: map[string]LifetimeTradeStats{"s1": {PositionsOpened: 1, Wins: 1, Losses: 0}},
		},
		{
			name: "position id scoped by strategy",
			trades: []Trade{
				{StrategyID: "s1", Timestamp: now, Symbol: "BTC", PositionID: "shared-position", Side: "sell", Quantity: 1, Price: 101, Value: 101, TradeType: "spot", Details: "Close long, PnL: $1", IsClose: true, RealizedPnL: 1},
				{StrategyID: "s2", Timestamp: now, Symbol: "BTC", PositionID: "shared-position", Side: "sell", Quantity: 1, Price: 99, Value: 99, TradeType: "spot", Details: "Close long, PnL: $-1", IsClose: true, RealizedPnL: -1},
			},
			want: map[string]LifetimeTradeStats{"s1": {PositionsOpened: 0, Wins: 1, Losses: 0}, "s2": {PositionsOpened: 0, Wins: 0, Losses: 1}},
		},
		{
			name: "breakeven position is neither win nor loss",
			trades: []Trade{
				{StrategyID: "s1", Timestamp: now, Symbol: "BTC", PositionID: "p1", Side: "sell", Quantity: 0.25, Price: 50100, Value: 12525, TradeType: "perps", Details: "Close long, PnL: $10", IsClose: true, RealizedPnL: 10},
				{StrategyID: "s1", Timestamp: now.Add(time.Second), Symbol: "BTC", PositionID: "p1", Side: "sell", Quantity: 0.25, Price: 49900, Value: 12475, TradeType: "perps", Details: "Close long, PnL: $-10", IsClose: true, RealizedPnL: -10},
			},
			want: map[string]LifetimeTradeStats{"s1": {PositionsOpened: 0, Wins: 0, Losses: 0}},
		},
		{
			name: "survives risk state reset because stats derive from trades",
			trades: []Trade{
				{StrategyID: "s1", Timestamp: now, Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps", Details: "Close long, PnL: $100", IsClose: true, RealizedPnL: 100},
				{StrategyID: "s1", Timestamp: now.Add(time.Second), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 50500, Value: 5050, TradeType: "perps", Details: "Close long, PnL: $-25", IsClose: true, RealizedPnL: -25},
			},
			want: map[string]LifetimeTradeStats{"s1": {PositionsOpened: 0, Wins: 1, Losses: 1}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sdb := openTestDB(t)
			for _, tr := range tc.trades {
				if err := sdb.InsertTrade(tr.StrategyID, tr); err != nil {
					t.Fatalf("InsertTrade: %v", err)
				}
			}
			stats, err := sdb.LifetimeTradeStatsAll()
			if err != nil {
				t.Fatalf("LifetimeTradeStatsAll: %v", err)
			}
			for id, want := range tc.want {
				if got := stats[id]; got != want {
					t.Errorf("%s stats = %+v, want %+v", id, got, want)
				}
				got, err := sdb.LifetimeTradeStatsForStrategy(id)
				if err != nil {
					t.Fatalf("LifetimeTradeStatsForStrategy(%s): %v", id, err)
				}
				if got != want {
					t.Errorf("%s single-strategy stats = %+v, want %+v", id, got, want)
				}
			}
			for _, id := range tc.wantAbsent {
				if got, ok := stats[id]; ok {
					t.Errorf("unexpected entry for %s with no trades: %+v", id, got)
				}
				got, err := sdb.LifetimeTradeStatsForStrategy(id)
				if err != nil {
					t.Fatalf("LifetimeTradeStatsForStrategy(%s) empty: %v", id, err)
				}
				if got != (LifetimeTradeStats{}) {
					t.Errorf("%s single-strategy empty stats = %+v, want zero", id, got)
				}
			}
		})
	}
}

func TestLifetimeTradeStatsAll_LegacyNullAndEmptyPositionIDStayPerLeg(t *testing.T) {
	sdb := openNullablePositionIDDB(t)
	now := time.Now().UTC()
	rows := []struct {
		positionID any
		pnl        float64
	}{
		{nil, 10},
		{"", -3},
	}
	for i, row := range rows {
		if _, err := sdb.db.Exec(`INSERT INTO trades
			(strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, details, is_close, realized_pnl)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"s1", formatTime(now.Add(time.Duration(i)*time.Second)), "BTC", row.positionID, "sell", 0.25, 50000.0, 12500.0, "perps", "legacy close", 1, row.pnl,
		); err != nil {
			t.Fatalf("seed legacy trade %d: %v", i, err)
		}
	}

	stats, err := sdb.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	if got := stats["s1"]; got.PositionsOpened != 0 || got.Wins != 1 || got.Losses != 1 {
		t.Errorf("legacy stats = %+v, want PositionsOpened=0 Wins=1 Losses=1 (only close legs seeded)", got)
	}
}

func TestLifetimeTradeStatsAll_OptionsSameContractReopenUsesDistinctPositionIDs(t *testing.T) {
	sdb := openTestDB(t)
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("options")
	defer logger.Close()

	s := &StrategyState{
		ID:              "options",
		Type:            "options",
		Platform:        "deribit",
		Cash:            100000,
		InitialCapital:  100000,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
		TradeHistory:    []Trade{},
	}
	openResult := &OptionsResult{
		Signal:     1,
		Underlying: "BTC",
		SpotPrice:  60000,
		Actions: []OptionsAction{{
			Action:     "buy",
			OptionType: "call",
			Strike:     65000,
			Expiry:     "2026-12-31",
			DTE:        30,
			PremiumUSD: 300,
			Quantity:   1,
		}},
	}
	closeResult := &OptionsResult{
		Signal:     -1,
		Underlying: "BTC",
		SpotPrice:  60000,
		Actions: []OptionsAction{{
			Action:     "close",
			OptionType: "call",
			Strike:     65000,
			PremiumUSD: 400,
		}},
	}
	posKey := "BTC-call-buy-65000-2026-12-31"

	if _, err := ExecuteOptionsSignal(s, openResult, logger); err != nil {
		t.Fatalf("first open: %v", err)
	}
	firstID := s.OptionPositions[posKey].TradePositionID
	s.OptionPositions[posKey].CurrentValueUSD = 400
	if _, err := ExecuteOptionsSignal(s, closeResult, logger); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if _, err := ExecuteOptionsSignal(s, openResult, logger); err != nil {
		t.Fatalf("second open: %v", err)
	}
	secondID := s.OptionPositions[posKey].TradePositionID
	if firstID == "" || secondID == "" {
		t.Fatalf("option trade position IDs must be populated: first=%q second=%q", firstID, secondID)
	}
	if firstID == secondID {
		t.Fatalf("reopened same option contract reused position_id %q", firstID)
	}
	s.OptionPositions[posKey].CurrentValueUSD = 350
	if _, err := ExecuteOptionsSignal(s, closeResult, logger); err != nil {
		t.Fatalf("second close: %v", err)
	}

	if err := sdb.SaveState(&AppState{Strategies: map[string]*StrategyState{s.ID: s}}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	stats, err := sdb.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	if got := stats[s.ID]; got.PositionsOpened != 2 || got.Wins != 2 || got.Losses != 0 {
		t.Errorf("option stats = %+v, want PositionsOpened=2 Wins=2 Losses=0", got)
	}
}
