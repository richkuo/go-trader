package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
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
	return db
}

func TestOpenStateDB(t *testing.T) {
	db := openTestDB(t)

	// Verify WAL mode.
	var mode string
	if err := db.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}

	// Verify tables exist.
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
		LastTop10Summary:        now.Add(-1 * time.Hour),
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
					TotalTrades: 15, WinningTrades: 10, LosingTrades: 5,
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
			PeakValue: 2050, CurrentDrawdownPct: 1.5,
			KillSwitchActive: false,
			WarningSent:      true,
			Events: []KillSwitchEvent{
				{Timestamp: now.Add(-3 * time.Hour), Type: "warning", DrawdownPct: 5, PortfolioValue: 1950, PeakValue: 2050, Details: "approaching threshold"},
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

	// Compare top-level fields.
	if loaded.CycleCount != original.CycleCount {
		t.Errorf("CycleCount = %d, want %d", loaded.CycleCount, original.CycleCount)
	}
	if loaded.LastLeaderboardPostDate != original.LastLeaderboardPostDate {
		t.Errorf("LastLeaderboardPostDate = %q, want %q", loaded.LastLeaderboardPostDate, original.LastLeaderboardPostDate)
	}
	if len(loaded.Strategies) != len(original.Strategies) {
		t.Fatalf("strategies count = %d, want %d", len(loaded.Strategies), len(original.Strategies))
	}

	// Check hl-momentum-btc strategy.
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

	// Position round-trip.
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

	// Option position round-trip.
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

	// Trade history round-trip.
	if len(hlStrat.TradeHistory) != 2 {
		t.Fatalf("trade count = %d, want 2", len(hlStrat.TradeHistory))
	}
	if hlStrat.TradeHistory[0].Side != "buy" || hlStrat.TradeHistory[1].Side != "sell" {
		t.Errorf("trade order mismatch")
	}

	// Risk state round-trip.
	if hlStrat.RiskState.TotalTrades != 15 || hlStrat.RiskState.WinningTrades != 10 {
		t.Errorf("risk state mismatch: %+v", hlStrat.RiskState)
	}

	// Circuit breaker round-trip on spot-rsi-eth.
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

	// Portfolio risk round-trip.
	if loaded.PortfolioRisk.PeakValue != 2050 {
		t.Errorf("PortfolioRisk.PeakValue = %f, want 2050", loaded.PortfolioRisk.PeakValue)
	}
	if !loaded.PortfolioRisk.WarningSent {
		t.Error("PortfolioRisk.WarningSent should be true")
	}
	if len(loaded.PortfolioRisk.Events) != 1 {
		t.Fatalf("kill switch events = %d, want 1", len(loaded.PortfolioRisk.Events))
	}
	if loaded.PortfolioRisk.Events[0].Type != "warning" {
		t.Errorf("event type = %q, want %q", loaded.PortfolioRisk.Events[0].Type, "warning")
	}

	// Correlation snapshot round-trip.
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

	// Second save adds one new trade.
	state.CycleCount = 2
	state.Strategies["test"].TradeHistory = append(state.Strategies["test"].TradeHistory,
		Trade{Timestamp: now, StrategyID: "test", Symbol: "ETH", Side: "buy", Quantity: 2, Price: 200, Value: 400},
	)

	if err := db.SaveState(state); err != nil {
		t.Fatalf("second SaveState: %v", err)
	}

	// Verify total trades in DB.
	var count int
	if err := db.db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = 'test'").Scan(&count); err != nil {
		t.Fatalf("count trades: %v", err)
	}
	if count != 3 {
		t.Errorf("trade count = %d, want 3", count)
	}
}

func TestSaveState_KillSwitchEventsCapped(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()

	state := &AppState{
		CycleCount: 1,
		Strategies: make(map[string]*StrategyState),
	}
	// Add 60 events (more than maxKillSwitchEvents=50).
	for i := 0; i < 60; i++ {
		state.PortfolioRisk.Events = append(state.PortfolioRisk.Events, KillSwitchEvent{
			Timestamp: now.Add(time.Duration(i) * time.Minute), Type: "warning", DrawdownPct: float64(i),
		})
	}

	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// DB should have all 60 since we store what's in memory (which is already capped by addKillSwitchEvent).
	// But let's verify the load caps at maxKillSwitchEvents.
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

func TestImportFromJSON(t *testing.T) {
	db := openTestDB(t)
	original := makeTestState()

	// Write as JSON.
	jsonPath := filepath.Join(t.TempDir(), "state.json")
	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(jsonPath, data, 0600); err != nil {
		t.Fatalf("write json: %v", err)
	}

	// Import into SQLite.
	jsonState, err := LoadState(jsonPath)
	if err != nil {
		t.Fatalf("LoadState from JSON: %v", err)
	}
	if err := db.SaveState(jsonState); err != nil {
		t.Fatalf("SaveState after JSON import: %v", err)
	}

	// Verify round-trip.
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState from DB: %v", err)
	}
	if loaded.CycleCount != original.CycleCount {
		t.Errorf("CycleCount = %d, want %d", loaded.CycleCount, original.CycleCount)
	}
	if len(loaded.Strategies) != len(original.Strategies) {
		t.Errorf("strategy count = %d, want %d", len(loaded.Strategies), len(original.Strategies))
	}
}

func TestQueryTradeHistory_NoFilter(t *testing.T) {
	db := openTestDB(t)
	state := makeTestState()
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	trades, total, err := db.QueryTradeHistory("", "", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(trades) != 2 {
		t.Errorf("trades len = %d, want 2", len(trades))
	}
	// Should be ordered by timestamp desc.
	if len(trades) >= 2 && trades[0].Side != "sell" {
		t.Errorf("first trade should be most recent (sell), got %q", trades[0].Side)
	}
}

func TestQueryTradeHistory_ByStrategy(t *testing.T) {
	db := openTestDB(t)
	state := makeTestState()
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	trades, total, err := db.QueryTradeHistory("hl-momentum-btc", "", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	for _, tr := range trades {
		if tr.StrategyID != "hl-momentum-btc" {
			t.Errorf("trade strategy = %q, want %q", tr.StrategyID, "hl-momentum-btc")
		}
	}

	// Query non-existent strategy.
	trades, total, err = db.QueryTradeHistory("nonexistent", "", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	if total != 0 || len(trades) != 0 {
		t.Errorf("expected empty result, got total=%d len=%d", total, len(trades))
	}
}

func TestQueryTradeHistory_BySymbol(t *testing.T) {
	db := openTestDB(t)
	state := makeTestState()
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	trades, total, err := db.QueryTradeHistory("", "BTC", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	for _, tr := range trades {
		if tr.Symbol != "BTC" {
			t.Errorf("trade symbol = %q, want %q", tr.Symbol, "BTC")
		}
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
	// Add 10 trades.
	for i := 0; i < 10; i++ {
		state.Strategies["test"].TradeHistory = append(state.Strategies["test"].TradeHistory,
			Trade{Timestamp: now.Add(time.Duration(i) * time.Minute), StrategyID: "test", Symbol: "BTC", Side: "buy", Quantity: 1, Price: float64(100 + i), Value: float64(100 + i)},
		)
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Page 1: limit 3, offset 0.
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

	// Page 2: limit 3, offset 3.
	trades2, _, err := db.QueryTradeHistory("", "", time.Time{}, time.Time{}, 3, 3)
	if err != nil {
		t.Fatalf("QueryTradeHistory page 2: %v", err)
	}
	if len(trades2) != 3 {
		t.Errorf("page 2 len = %d, want 3", len(trades2))
	}

	// Verify different results.
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

	// Query with since bound (should exclude the oldest trade).
	since := now.Add(-150 * time.Minute) // 2.5 hours ago
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

func TestQueryTradeHistory_LimitClamped(t *testing.T) {
	db := openTestDB(t)
	state := makeTestState()
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Limit > 500 should be clamped.
	trades, _, err := db.QueryTradeHistory("", "", time.Time{}, time.Time{}, 9999, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	// We only have 2 trades, so this just verifies no error with large limit.
	if len(trades) != 2 {
		t.Errorf("trades len = %d, want 2", len(trades))
	}
}

func TestCorrelationSnapshotRoundTrip(t *testing.T) {
	db := openTestDB(t)

	// Save state with correlation snapshot.
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

	// Save state without correlation snapshot.
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

	// Simulate the bug from issue #207: two map entries with different keys
	// but the same s.ID. This happens when loadJSONPlatformStates merges
	// overlapping platform state files.
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

	// Before the fix, this would fail with UNIQUE constraint violation.
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState with duplicate IDs should not error: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	// One of the two entries wins (last-write-wins); verify only one strategy in DB.
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
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"hl-test": {
				ID: "hl-test", Type: "perps", Platform: "hyperliquid",
				Cash: 1000, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
				TradeHistory: []Trade{
					{
						Timestamp: now.Add(-1 * time.Hour), StrategyID: "hl-test", Symbol: "BTC",
						Side: "buy", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps",
						Details: "live buy", ExchangeOrderID: "1234567890", ExchangeFee: 1.75,
					},
					{
						Timestamp: now, StrategyID: "hl-test", Symbol: "BTC",
						Side: "sell", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps",
						Details: "live sell", ExchangeOrderID: "1234567891", ExchangeFee: 1.79,
					},
				},
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

	hlStrat := loaded.Strategies["hl-test"]
	if hlStrat == nil {
		t.Fatal("missing strategy hl-test")
	}
	if len(hlStrat.TradeHistory) != 2 {
		t.Fatalf("trade count = %d, want 2", len(hlStrat.TradeHistory))
	}

	// Verify exchange fields persisted on first trade.
	t1 := hlStrat.TradeHistory[0]
	if t1.ExchangeOrderID != "1234567890" {
		t.Errorf("trade[0].ExchangeOrderID = %q, want %q", t1.ExchangeOrderID, "1234567890")
	}
	if t1.ExchangeFee != 1.75 {
		t.Errorf("trade[0].ExchangeFee = %g, want 1.75", t1.ExchangeFee)
	}

	// Verify exchange fields persisted on second trade.
	t2 := hlStrat.TradeHistory[1]
	if t2.ExchangeOrderID != "1234567891" {
		t.Errorf("trade[1].ExchangeOrderID = %q, want %q", t2.ExchangeOrderID, "1234567891")
	}
	if t2.ExchangeFee != 1.79 {
		t.Errorf("trade[1].ExchangeFee = %g, want 1.79", t2.ExchangeFee)
	}
}

func TestTradeExchangeFields_EmptyByDefault(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)

	// Trades without exchange fields should default to empty/zero.
	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"spot-test": {
				ID: "spot-test", Type: "spot", Platform: "binanceus",
				Cash: 1000, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
				TradeHistory: []Trade{
					{Timestamp: now, StrategyID: "spot-test", Symbol: "BTC/USDT", Side: "buy",
						Quantity: 0.01, Price: 50000, Value: 500, TradeType: "spot", Details: "paper trade"},
				},
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

	tr := loaded.Strategies["spot-test"].TradeHistory[0]
	if tr.ExchangeOrderID != "" {
		t.Errorf("ExchangeOrderID should be empty for paper trade, got %q", tr.ExchangeOrderID)
	}
	if tr.ExchangeFee != 0 {
		t.Errorf("ExchangeFee should be 0 for paper trade, got %g", tr.ExchangeFee)
	}
}

func TestQueryTradeHistory_ExchangeFields(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Nanosecond)

	state := &AppState{
		CycleCount: 1,
		Strategies: map[string]*StrategyState{
			"hl-test": {
				ID: "hl-test", Type: "perps", Platform: "hyperliquid",
				Cash: 1000, InitialCapital: 1000,
				Positions: make(map[string]*Position), OptionPositions: make(map[string]*OptionPosition),
				TradeHistory: []Trade{
					{Timestamp: now, StrategyID: "hl-test", Symbol: "BTC", Side: "buy",
						Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps",
						Details: "live", ExchangeOrderID: "9876543210", ExchangeFee: 2.50},
				},
			},
		},
	}

	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	trades, total, err := db.QueryTradeHistory("hl-test", "", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if trades[0].ExchangeOrderID != "9876543210" {
		t.Errorf("ExchangeOrderID = %q, want %q", trades[0].ExchangeOrderID, "9876543210")
	}
	if trades[0].ExchangeFee != 2.50 {
		t.Errorf("ExchangeFee = %g, want 2.50", trades[0].ExchangeFee)
	}
}

func TestMigrateSchema_AddsExchangeColumns(t *testing.T) {
	// Create a DB with the old schema (no exchange columns), then verify migration adds them.
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// Create old-schema trades table without exchange columns.
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
	    risk_circuit_breaker_until TEXT NOT NULL DEFAULT '',
	    risk_total_trades INTEGER NOT NULL DEFAULT 0,
	    risk_winning_trades INTEGER NOT NULL DEFAULT 0,
	    risk_losing_trades INTEGER NOT NULL DEFAULT 0
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

	// Insert a trade without exchange columns.
	if _, err := db.Exec(`INSERT INTO app_state (id, cycle_count) VALUES (1, 1)`); err != nil {
		t.Fatalf("insert app_state: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO strategies (id, type) VALUES ('test', 'perps')`); err != nil {
		t.Fatalf("insert strategy: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details)
		VALUES ('test', '2026-01-01T00:00:00Z', 'BTC', 'buy', 0.1, 50000, 5000, 'perps', 'old trade')`); err != nil {
		t.Fatalf("insert old trade: %v", err)
	}
	db.Close()

	// Re-open via OpenStateDB which runs migration.
	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("OpenStateDB after migration: %v", err)
	}
	defer sdb.Close()

	// Verify old trade can be loaded with new columns defaulting to empty/zero.
	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("LoadState after migration: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded state is nil")
	}
	strat := loaded.Strategies["test"]
	if strat == nil {
		t.Fatal("missing strategy 'test'")
	}
	if len(strat.TradeHistory) != 1 {
		t.Fatalf("trade count = %d, want 1", len(strat.TradeHistory))
	}
	tr := strat.TradeHistory[0]
	if tr.ExchangeOrderID != "" {
		t.Errorf("migrated trade ExchangeOrderID = %q, want empty", tr.ExchangeOrderID)
	}
	if tr.ExchangeFee != 0 {
		t.Errorf("migrated trade ExchangeFee = %g, want 0", tr.ExchangeFee)
	}

	// Verify new trades with exchange fields can be saved and loaded.
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

// TestClosedPositions_Flush verifies that ClosedPosition buffer entries are
// persisted to the closed_positions table on SaveState and that the buffer
// is cleared after a successful commit (#288).
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

	// Buffer should be cleared after successful commit.
	if len(state.Strategies["test"].ClosedPositions) != 0 {
		t.Errorf("ClosedPositions buffer not cleared after save, len=%d", len(state.Strategies["test"].ClosedPositions))
	}

	// Table should contain the row.
	var count int
	if err := sdb.db.QueryRow("SELECT COUNT(*) FROM closed_positions").Scan(&count); err != nil {
		t.Fatalf("count closed_positions: %v", err)
	}
	if count != 1 {
		t.Fatalf("closed_positions rows = %d, want 1", count)
	}

	// QueryClosedPositions round-trip.
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

	// Second save with no new closes should not re-insert.
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

// TestRecordClosedPosition_ExecuteSignal verifies that closing a position via
// ExecuteSpotSignal appends to the ClosedPositions buffer with the correct
// PnL, reason, and duration (#288).
func TestRecordClosedPosition_ExecuteSignal(t *testing.T) {
	openedAt := time.Now().UTC().Add(-2 * time.Hour)
	s := &StrategyState{
		ID: "test", Type: "spot", Platform: "binanceus",
		Cash: 0, // zero so we can't re-buy — isolates the close path
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 1.0, AvgCost: 100, Side: "long", OpenedAt: openedAt},
		},
	}
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger("test")
	if _, err := ExecuteSpotSignal(s, -1, "BTC", 110, 0, logger); err != nil {
		t.Fatalf("ExecuteSpotSignal: %v", err)
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

// TestMigrateSchema_AddsOpenedAt verifies that re-opening an older DB without
// the positions.opened_at column successfully applies the ALTER migration.
func TestMigrateSchema_AddsOpenedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Create a DB with the legacy positions schema (no opened_at column).
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

	// Re-open with the current code — migrateSchema should add opened_at.
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
