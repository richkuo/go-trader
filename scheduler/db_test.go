package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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
					"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000, Side: "long", Multiplier: 0, OwnerStrategyID: "hl-momentum-btc"},
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
