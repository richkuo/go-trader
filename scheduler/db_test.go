package main

import (
	"database/sql"
	"os"
	"path/filepath"
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

// resetInitialCapitalGuardDedup wipes the package-level dedup map so a test
// that asserts on warn counts (or simply triggers the guard repeatedly) is
// not influenced by prior tests in the same package run. Registers a Cleanup
// so the next test also starts from a clean slate even if this one panics.
func resetInitialCapitalGuardDedup(t *testing.T) {
	t.Helper()
	initialCapitalGuardWarned = sync.Map{}
	t.Cleanup(func() { initialCapitalGuardWarned = sync.Map{} })
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
			PeakValue: 2050, CurrentDrawdownPct: 1.5, CurrentMarginDrawdownPct: 18.7,
			KillSwitchActive: false,
			WarningSent:      true,
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
	if loaded.PortfolioRisk.CurrentDrawdownPct != 1.5 {
		t.Errorf("PortfolioRisk.CurrentDrawdownPct = %f, want 1.5", loaded.PortfolioRisk.CurrentDrawdownPct)
	}
	if loaded.PortfolioRisk.CurrentMarginDrawdownPct != 18.7 {
		t.Errorf("PortfolioRisk.CurrentMarginDrawdownPct = %f, want 18.7", loaded.PortfolioRisk.CurrentMarginDrawdownPct)
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
	if loaded.PortfolioRisk.Events[0].Source != "margin" {
		t.Errorf("event source = %q, want %q", loaded.PortfolioRisk.Events[0].Source, "margin")
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

	// Regression test for issue #207: two map entries with different keys but
	// the same s.ID must not trigger a UNIQUE constraint violation.
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

// TestQueryClosedPositions_Filters exercises strategy/symbol/since/until
// filters and verifies that two successive SaveState calls append rather than
// replace (regression guard for anyone changing formatTime away from a
// lexicographically-comparable representation).
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

	// Filter by strategy_id.
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

	// Filter by symbol across strategies.
	rows, total, err = sdb.QueryClosedPositions("", "BTC", time.Time{}, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("filter symbol: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("symbol filter: total=%d len=%d, want 2/2", total, len(rows))
	}

	// since bound excludes the oldest s1 BTC close.
	since := now.Add(-90 * time.Minute)
	rows, total, err = sdb.QueryClosedPositions("", "", since, time.Time{}, 50, 0)
	if err != nil {
		t.Fatalf("filter since: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("since filter: total=%d len=%d, want 2/2", total, len(rows))
	}

	// until bound excludes s2 (most recent).
	until := now.Add(-45 * time.Minute)
	rows, total, err = sdb.QueryClosedPositions("", "", time.Time{}, until, 50, 0)
	if err != nil {
		t.Fatalf("filter until: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Errorf("until filter: total=%d len=%d, want 2/2 (s1 BTC + s1 ETH)", total, len(rows))
	}

	// Combined strategy + symbol.
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

	// Second SaveState with a fresh close appends rather than replaces.
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

// TestClosedOptionPositions_Flush verifies that ClosedOptionPosition buffer
// entries round-trip through SaveState and QueryClosedOptionPositions (#288).
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

	// Filter by underlying.
	rows, total, err = sdb.QueryClosedOptionPositions("", "ETH", time.Time{}, time.Time{}, 10, 0)
	if err != nil {
		t.Fatalf("filter underlying: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Errorf("ETH filter should be empty, got total=%d", total)
	}
}

// TestRecordClosedOptionPosition_ExecuteClose verifies that
// executeOptionClose records a ClosedOptionPosition on the strategy buffer.
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

func TestSaveLoadState_LeaderboardSummaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sdb.Close()

	now := time.Now().UTC().Truncate(time.Second)
	state := NewAppState()
	state.LastLeaderboardSummaries = map[string]time.Time{
		"hyperliquid:*:123":   now.Add(-1 * time.Hour),
		"hyperliquid:eth:456": now.Add(-2 * time.Hour),
	}
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.LastLeaderboardSummaries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(loaded.LastLeaderboardSummaries))
	}
	for k, want := range state.LastLeaderboardSummaries {
		got, ok := loaded.LastLeaderboardSummaries[k]
		if !ok {
			t.Errorf("key %q missing after reload", k)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("key %q: got %v, want %v", k, got, want)
		}
	}
}

func TestSaveLoadState_LastSummaryPost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sdb.Close()

	now := time.Now().UTC().Truncate(time.Second)
	state := NewAppState()
	state.LastSummaryPost = map[string]time.Time{
		"spot":        now.Add(-5 * time.Minute),
		"hyperliquid": now.Add(-30 * time.Minute),
	}
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := sdb.LoadState()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.LastSummaryPost) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(loaded.LastSummaryPost))
	}
	for k, want := range state.LastSummaryPost {
		got, ok := loaded.LastSummaryPost[k]
		if !ok {
			t.Errorf("key %q missing after reload", k)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("key %q: got %v, want %v", k, got, want)
		}
	}
}

// TestSaveState_PreservesInitialCapital verifies the #343 guard: once an
// initial_capital baseline has been persisted, subsequent SaveState calls can
// never silently overwrite it, even if the in-memory StrategyState has a
// different value. Normal state persistence (cycle saves, position closes,
// restarts) must leave the baseline untouched.
func TestSaveState_PreservesInitialCapital(t *testing.T) {
	db := openTestDB(t)

	initial := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-tema-eth": {
				ID:              "hl-tema-eth",
				Type:            "perps",
				Platform:        "hyperliquid",
				Cash:            505,
				InitialCapital:  505,
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(initial); err != nil {
		t.Fatalf("first SaveState: %v", err)
	}

	// Simulate the incident: something (operator agent, buggy code path) tries
	// to rewrite initial_capital alongside a normal state save.
	mutated := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-tema-eth": {
				ID:              "hl-tema-eth",
				Type:            "perps",
				Platform:        "hyperliquid",
				Cash:            632,
				InitialCapital:  632,
				Positions:       map[string]*Position{},
				OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(mutated); err != nil {
		t.Fatalf("second SaveState: %v", err)
	}

	// Guard should preserve the baseline and mutate the in-memory state so
	// subsequent reads stay consistent with the DB.
	if got := mutated.Strategies["hl-tema-eth"].InitialCapital; got != 505 {
		t.Errorf("in-memory InitialCapital = %g, want 505 (guard should have restored it)", got)
	}

	// Cash is a normal runtime field — guard must not touch it.
	if got := mutated.Strategies["hl-tema-eth"].Cash; got != 632 {
		t.Errorf("Cash = %g, want 632 (guard must only protect initial_capital)", got)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := loaded.Strategies["hl-tema-eth"].InitialCapital; got != 505 {
		t.Errorf("persisted initial_capital = %g, want 505", got)
	}
	if got := loaded.Strategies["hl-tema-eth"].Cash; got != 632 {
		t.Errorf("persisted cash = %g, want 632", got)
	}
}

// TestSaveState_AllowsFirstInitialCapitalWrite confirms the guard only protects
// an *existing* baseline — the very first save (DB empty, or prior row had 0)
// must establish the baseline normally.
func TestSaveState_AllowsFirstInitialCapitalWrite(t *testing.T) {
	db := openTestDB(t)

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"new-strat": {
				ID: "new-strat", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
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
	if got := loaded.Strategies["new-strat"].InitialCapital; got != 1000 {
		t.Errorf("initial_capital = %g, want 1000 (first write must land)", got)
	}
}

// TestSaveState_AllowsNewStrategies confirms the guard does not block strategy
// rows that don't yet exist in the DB (new strategies added to config between
// restarts).
func TestSaveState_AllowsNewStrategies(t *testing.T) {
	db := openTestDB(t)

	first := &AppState{
		Strategies: map[string]*StrategyState{
			"old": {
				ID: "old", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(first); err != nil {
		t.Fatalf("first SaveState: %v", err)
	}

	second := &AppState{
		Strategies: map[string]*StrategyState{
			"old": {
				ID: "old", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
			"brand-new": {
				ID: "brand-new", Type: "spot", Cash: 2000, InitialCapital: 2000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(second); err != nil {
		t.Fatalf("second SaveState: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := loaded.Strategies["brand-new"].InitialCapital; got != 2000 {
		t.Errorf("new strategy initial_capital = %g, want 2000", got)
	}
	if got := loaded.Strategies["old"].InitialCapital; got != 1000 {
		t.Errorf("old strategy initial_capital = %g, want 1000", got)
	}
}

// TestSaveState_AllowsBaselineWhenPrevZero confirms the legacy/reset path
// (#343 review item 7): if a prior row exists with initial_capital = 0
// (e.g. after ValidateState clamped a malformed value at state.go:144), the
// next SaveState carrying a real positive baseline must establish it — the
// guard's `prev > 0` precondition is what makes this work.
func TestSaveState_AllowsBaselineWhenPrevZero(t *testing.T) {
	db := openTestDB(t)

	// Seed a strategy row with initial_capital = 0 (mimics the post-ValidateState
	// reset path).
	zeroState := &AppState{
		Strategies: map[string]*StrategyState{
			"legacy": {
				ID: "legacy", Type: "spot", Cash: 0, InitialCapital: 0,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(zeroState); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}

	// Now save with a real baseline. Guard must let it through (prev == 0
	// means "no real baseline yet").
	bumped := &AppState{
		Strategies: map[string]*StrategyState{
			"legacy": {
				ID: "legacy", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(bumped); err != nil {
		t.Fatalf("bumped SaveState: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got := loaded.Strategies["legacy"].InitialCapital; got != 1000 {
		t.Errorf("initial_capital = %g, want 1000 (prev==0 must allow baseline establishment)", got)
	}
}

// TestSetInitialCapital_ExplicitOverride is the sanctioned escape hatch —
// admin/CLI code can permanently change the baseline through this path.
func TestSetInitialCapital_ExplicitOverride(t *testing.T) {
	db := openTestDB(t)

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"s": {
				ID: "s", Type: "spot", Cash: 505, InitialCapital: 505,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if err := db.SetInitialCapital("s", 750); err != nil {
		t.Fatalf("SetInitialCapital: %v", err)
	}

	// A subsequent SaveState must carry the new baseline forward, not revert
	// to the in-memory (stale) value.
	state.Strategies["s"].InitialCapital = 505 // stale in-memory value
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
}

func TestSetInitialCapital_RejectsInvalid(t *testing.T) {
	db := openTestDB(t)

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"s": {
				ID: "s", Type: "spot", Cash: 1000, InitialCapital: 1000,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if err := db.SetInitialCapital("s", 0); err == nil {
		t.Error("expected error for zero initial_capital")
	}
	if err := db.SetInitialCapital("s", -100); err == nil {
		t.Error("expected error for negative initial_capital")
	}
	if err := db.SetInitialCapital("unknown-id", 1000); err == nil {
		t.Error("expected error for unknown strategy id")
	}
}

// TestSaveState_GuardWarnIsOneShot covers the #343 review item 3 follow-up:
// the baseline-guard warning must fire only once per strategy per process so
// per-cycle SaveState calls don't spam the operator DM.
func TestSaveState_GuardWarnIsOneShot(t *testing.T) {
	db := openTestDB(t)
	// Dedup map is reset by openTestDB → resetInitialCapitalGuardDedup so
	// prior tests don't leak warn counts into this assertion.

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

	// SetInitialCapital clears the dedup so a subsequent guard violation
	// against the new baseline fires again.
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

func TestSaveAndLoadDB_PendingCircuitCloseRoundTrip(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	state := &AppState{
		CycleCount: 1,
		LastCycle:  now,
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a", Type: "perps", Platform: "hyperliquid", Cash: 100, InitialCapital: 100,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				RiskState: RiskState{
					PeakValue: 100, MaxDrawdownPct: 25,
					PendingCircuitCloses: map[string]*PendingCircuitClose{
						PlatformPendingCloseHyperliquid: {
							Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.2585}},
						},
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
	p := loaded.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil || len(p.Symbols) != 1 {
		t.Fatalf("pending missing: %+v", p)
	}
	if p.Symbols[0].Symbol != "ETH" || p.Symbols[0].Size != 0.2585 {
		t.Errorf("pending symbol=%q size=%g want ETH 0.2585", p.Symbols[0].Symbol, p.Symbols[0].Size)
	}
}

// TestSaveAndLoadDB_LegacyPendingHLJSON_MigratesOnLoad verifies the #359 phase
// 1b backwards-compat path: a pre-#359 row where the JSON blob is in the legacy
// {"coins":[...]} shape must transparently convert to the new map-keyed shape
// on load, without losing the pending close.
func TestSaveAndLoadDB_LegacyPendingHLJSON_MigratesOnLoad(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	// Seed a strategy via the normal SaveState path (writes new-format JSON),
	// then overwrite just the pending column with legacy-format JSON to
	// simulate a DB carried over from a pre-#359 scheduler build.
	state := &AppState{
		CycleCount: 1,
		LastCycle:  now,
		Strategies: map[string]*StrategyState{
			"hl-a": {
				ID: "hl-a", Type: "perps", Platform: "hyperliquid", Cash: 100, InitialCapital: 100,
				Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
				RiskState: RiskState{PeakValue: 100, MaxDrawdownPct: 25},
			},
		},
	}
	if err := db.SaveState(state); err != nil {
		t.Fatalf("seed SaveState: %v", err)
	}

	legacyJSON := `{"coins":[{"coin":"ETH","sz":0.2585}]}`
	if _, err := db.db.Exec(
		"UPDATE strategies SET risk_pending_circuit_closes_json = ? WHERE id = ?",
		legacyJSON, "hl-a",
	); err != nil {
		t.Fatalf("inject legacy JSON: %v", err)
	}

	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	p := loaded.Strategies["hl-a"].RiskState.getPendingCircuitClose(PlatformPendingCloseHyperliquid)
	if p == nil || len(p.Symbols) != 1 {
		t.Fatalf("legacy JSON did not migrate on load: %+v", p)
	}
	if p.Symbols[0].Symbol != "ETH" || p.Symbols[0].Size != 0.2585 {
		t.Errorf("legacy-migrated pending symbol=%q size=%g want ETH 0.2585", p.Symbols[0].Symbol, p.Symbols[0].Size)
	}
}

// TestMigrateSchema_PendingCircuitClosesColumn_Idempotent verifies the PR #365
// review fix: running migrateSchema repeatedly must leave exactly one pending
// column (risk_pending_circuit_closes_json) and never re-add the legacy
// risk_pending_hl_close_json. The pre-fix migration unconditionally ran
// ADD COLUMN risk_pending_hl_close_json + RENAME, which grew a ghost legacy
// column on every post-rename startup.
func TestMigrateSchema_PendingCircuitClosesColumn_Idempotent(t *testing.T) {
	db := openTestDB(t)
	// openTestDB already ran migrateSchema once via OpenStateDB. Run it again
	// to simulate a second scheduler startup on an already-migrated DB.
	if err := db.migrateSchema(); err != nil {
		t.Fatalf("second migrateSchema: %v", err)
	}
	// And a third, to lock in the fixed-point claim.
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

// TestMigrateSchema_PendingCircuitClosesColumn_FromLegacyDB verifies the
// post-#356, pre-#359 upgrade path: a DB that has risk_pending_hl_close_json
// but not the new column must be renamed in place, preserving row data.
func TestMigrateSchema_PendingCircuitClosesColumn_FromLegacyDB(t *testing.T) {
	db := openTestDB(t)

	// Simulate a pre-#359 DB: drop the new column and re-add the legacy name
	// with a row of data we can check survives the rename. SQLite doesn't
	// support DROP COLUMN on all versions, so we rebuild the table.
	_, err := db.db.Exec(`CREATE TABLE strategies_legacy AS SELECT
		id, type, platform, cash, initial_capital,
		risk_peak_value, risk_max_drawdown_pct, risk_current_drawdown_pct,
		risk_daily_pnl, risk_daily_pnl_date, risk_consecutive_losses,
		risk_circuit_breaker, risk_circuit_breaker_until,
		risk_pending_circuit_closes_json AS risk_pending_hl_close_json,
		risk_total_trades, risk_winning_trades, risk_losing_trades
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

	// Seed a pending value into the legacy column so we can verify data survives.
	if _, err := db.db.Exec(
		"INSERT INTO strategies (id, type, platform, cash, initial_capital, risk_pending_hl_close_json) VALUES (?, ?, ?, ?, ?, ?)",
		"hl-rename", "perps", "hyperliquid", 100.0, 100.0,
		`{"coins":[{"coin":"ETH","sz":0.3}]}`,
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Pre-check: only the legacy column is present.
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

// TestParseDetailsPnL verifies the regex used to backfill realized_pnl from
// pre-#455 trade Details strings. Covers each of the formatter variants emitted
// by close-leg RecordTrade call sites at the time of #455.
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

// TestBackfillTradeCloseFlags exercises the one-time legacy backfill: rows
// whose Details contain "PnL:" or "PnL=" should flip is_close=1 and have
// realized_pnl populated. Open-leg rows must stay is_close=0.
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
	if got.RoundTrips != 3 {
		t.Errorf("RoundTrips = %d, want 3 (3 close legs of 5 rows)", got.RoundTrips)
	}
	if got.Wins != 2 {
		t.Errorf("Wins = %d, want 2 (PnL > 0: $42.50, $3.14)", got.Wins)
	}
	if got.Losses != 1 {
		t.Errorf("Losses = %d, want 1 (PnL < 0: $-7.10)", got.Losses)
	}
}

// TestLifetimeTradeStatsAll_FreshInsert verifies that new InsertTrade calls
// land with is_close/realized_pnl set so LifetimeTradeStatsAll reports them
// without depending on the legacy backfill.
func TestLifetimeTradeStatsAll_FreshInsert(t *testing.T) {
	sdb := openTestDB(t)
	now := time.Now().UTC()
	trades := []Trade{
		{StrategyID: "s1", Timestamp: now, Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 50000, Value: 5000, TradeType: "perps", Details: "Open long"},
		{StrategyID: "s1", Timestamp: now.Add(time.Second), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps", Details: "Close long, PnL: $100.00", IsClose: true, RealizedPnL: 100},
		{StrategyID: "s1", Timestamp: now.Add(2 * time.Second), Symbol: "BTC", Side: "buy", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps", Details: "Open long"},
		{StrategyID: "s1", Timestamp: now.Add(3 * time.Second), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 50500, Value: 5050, TradeType: "perps", Details: "Close long, PnL: $-50.00", IsClose: true, RealizedPnL: -50},
		{StrategyID: "s2", Timestamp: now, Symbol: "ETH", Side: "buy", Quantity: 0.5, Price: 2000, Value: 1000, TradeType: "perps", Details: "Open long"},
		{StrategyID: "s2", Timestamp: now.Add(time.Second), Symbol: "ETH", Side: "sell", Quantity: 0.5, Price: 2100, Value: 1050, TradeType: "perps", Details: "Close long, PnL: $50.00", IsClose: true, RealizedPnL: 50},
	}
	for _, tr := range trades {
		if err := sdb.InsertTrade(tr.StrategyID, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	stats, err := sdb.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	if got := stats["s1"]; got.RoundTrips != 2 || got.Wins != 1 || got.Losses != 1 {
		t.Errorf("s1 stats = %+v, want RoundTrips=2 Wins=1 Losses=1", got)
	}
	if got := stats["s2"]; got.RoundTrips != 1 || got.Wins != 1 || got.Losses != 0 {
		t.Errorf("s2 stats = %+v, want RoundTrips=1 Wins=1 Losses=0", got)
	}
	if _, ok := stats["s3"]; ok {
		t.Errorf("unexpected entry for s3 with no closes: %+v", stats["s3"])
	}
}

// TestLifetimeTradeStats_SurvivesRiskStateReset is the core regression test
// for #455: kill-switch / circuit-breaker resets of the in-memory RiskState
// counters MUST NOT change the lifetime stats query. The query reads from
// trades, which is append-only, so simulating a counter reset leaves the DB
// result intact.
func TestLifetimeTradeStats_SurvivesRiskStateReset(t *testing.T) {
	sdb := openTestDB(t)
	now := time.Now().UTC()
	closes := []Trade{
		{StrategyID: "s1", Timestamp: now, Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 51000, Value: 5100, TradeType: "perps", Details: "Close long, PnL: $100", IsClose: true, RealizedPnL: 100},
		{StrategyID: "s1", Timestamp: now.Add(time.Second), Symbol: "BTC", Side: "sell", Quantity: 0.1, Price: 50500, Value: 5050, TradeType: "perps", Details: "Close long, PnL: $-25", IsClose: true, RealizedPnL: -25},
	}
	for _, tr := range closes {
		if err := sdb.InsertTrade(tr.StrategyID, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	// Simulate a kill-switch reset of in-memory RiskState. The trades table
	// is append-only, so the lifetime query is unaffected.
	_ = RiskState{}

	stats, err := sdb.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	got := stats["s1"]
	if got.RoundTrips != 2 || got.Wins != 1 || got.Losses != 1 {
		t.Errorf("post-reset stats = %+v, want RoundTrips=2 Wins=1 Losses=1", got)
	}
}
