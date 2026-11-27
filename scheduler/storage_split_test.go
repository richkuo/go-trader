package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStateStoreSplitLayout(t *testing.T) {
	t.Run("SplitRoundTrip", func(t *testing.T) {
		cfg := splitTestConfig(t)
		cfg.Strategies[0].StorageStrategyID = "hl"
		cfg.Strategies[1].StorageStrategyID = "hl"
		store := openSplitStore(t, cfg)

		state := splitTestState(t)
		state.Strategies["hl-live"].Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000}
		state.Strategies["hl-paper"].Positions["ETH"] = &Position{Symbol: "ETH", Side: "short", Quantity: 2, AvgCost: 2100}
		RecordTrade(state.Strategies["hl-live"], Trade{StrategyID: "hl-live", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2000, Timestamp: time.Unix(1, 0).UTC()})
		RecordTrade(state.Strategies["hl-paper"], Trade{StrategyID: "hl-paper", Symbol: "ETH", Side: "sell", Quantity: 2, Price: 2100, Timestamp: time.Unix(1, 0).UTC()})
		state.partitionRisk(livePartition).PeakValue = 1000
		state.partitionRisk(defaultPaperPartition).PeakValue = 2000

		for part, err := range store.SaveAll(state) {
			if err != nil {
				t.Fatalf("SaveAll(%s): %v", partitionLabel(part), err)
			}
		}

		liveRows, err := store.file(storageRolePrimary).loadScopeBooks([]PortfolioScope{ScopeLive})
		if err != nil {
			t.Fatalf("primary loadScopeBooks: %v", err)
		}
		if len(liveRows.Strategies) != 1 || liveRows.Strategies["hl-live"] == nil {
			t.Fatalf("primary strategies = %v, want only hl-live", liveRows.Strategies)
		}
		paperRows, err := store.file(storageRolePaper).loadScopeBooks([]PortfolioScope{ScopePaper})
		if err != nil {
			t.Fatalf("paper loadScopeBooks: %v", err)
		}
		if len(paperRows.Strategies) != 1 || paperRows.Strategies["hl-paper"] == nil {
			t.Fatalf("paper strategies = %v, want only hl-paper", paperRows.Strategies)
		}

		reloaded, orphans, err := LoadStateWithStore(cfg, store)
		if err != nil {
			t.Fatalf("LoadStateWithStore: %v", err)
		}
		if len(orphans) != 0 {
			t.Fatalf("orphans = %+v, want none", orphans)
		}
		live := reloaded.Strategies["hl-live"]
		paper := reloaded.Strategies["hl-paper"]
		if live == nil || paper == nil {
			t.Fatalf("reloaded strategies = %v, want both books", reloaded.Strategies)
		}
		if live.Cash != 1000 || paper.Cash != 2000 {
			t.Errorf("cash = live %.2f / paper %.2f, want 1000 / 2000", live.Cash, paper.Cash)
		}
		if p := live.Positions["ETH"]; p == nil || p.Side != "long" || p.Quantity != 1 {
			t.Errorf("live position = %+v, want the long book", p)
		}
		if p := paper.Positions["ETH"]; p == nil || p.Side != "short" || p.Quantity != 2 {
			t.Errorf("paper position = %+v, want the short book", p)
		}
		if len(live.TradeHistory) != 1 || live.TradeHistory[0].StrategyID != "hl-live" {
			t.Errorf("live trades = %+v, want one row attributed to hl-live", live.TradeHistory)
		}
		if len(paper.TradeHistory) != 1 || paper.TradeHistory[0].StrategyID != "hl-paper" {
			t.Errorf("paper trades = %+v, want one row attributed to hl-paper", paper.TradeHistory)
		}
		if reloaded.partitionRisk(livePartition).PeakValue != 1000 || reloaded.partitionRisk(defaultPaperPartition).PeakValue != 2000 {
			t.Errorf("risk peaks = live %.0f / paper %.0f, want 1000 / 2000",
				reloaded.partitionRisk(livePartition).PeakValue, reloaded.partitionRisk(defaultPaperPartition).PeakValue)
		}
	})
	t.Run("ProcessAliasResumesTheStoredBook", func(t *testing.T) {
		dir := t.TempDir()
		livePath := filepath.Join(dir, "live.db")
		paperPath := filepath.Join(dir, "paper.db")

		before := &Config{DBFile: livePath, PaperDBFile: paperPath, Strategies: []StrategyConfig{
			{ID: "hl-perps-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live"}},
		}}
		store := openSplitStore(t, before)
		state := NewAppState()
		state.Strategies["hl-perps-eth"] = &StrategyState{
			ID: "hl-perps-eth", Type: "perps", Platform: "hyperliquid", Cash: 500, InitialCapital: 500,
			Positions:       map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 3, AvgCost: 1800}},
			OptionPositions: map[string]*OptionPosition{},
		}
		RecordTrade(state.Strategies["hl-perps-eth"], Trade{StrategyID: "hl-perps-eth", Symbol: "ETH", Side: "buy", Quantity: 3, Price: 1800, Timestamp: time.Unix(2, 0).UTC()})
		state.partitionRisk(livePartition).KillSwitchActive = true
		if err := SaveStateWithStore(state, store); err != nil {
			t.Fatalf("SaveStateWithStore: %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		after := &Config{DBFile: livePath, PaperDBFile: paperPath, Strategies: []StrategyConfig{
			{ID: "hl-eth-momentum", StorageStrategyID: "hl-perps-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live"}},
		}}
		store2 := openSplitStore(t, after)
		reloaded, orphans, err := LoadStateWithStore(after, store2)
		if err != nil {
			t.Fatalf("LoadStateWithStore after rename: %v", err)
		}
		if len(orphans) != 0 {
			t.Fatalf("orphans = %+v, want none (the alias adopts the stored book)", orphans)
		}
		ss := reloaded.Strategies["hl-eth-momentum"]
		if ss == nil {
			t.Fatalf("renamed strategy missing; roster = %v", reloaded.Strategies)
		}
		if ss.Cash != 500 {
			t.Errorf("cash = %.2f, want 500 (resumed, not reset)", ss.Cash)
		}
		if p := ss.Positions["ETH"]; p == nil || p.Quantity != 3 {
			t.Errorf("position = %+v, want the stored 3 ETH long", p)
		}
		if len(ss.TradeHistory) != 1 || ss.TradeHistory[0].StrategyID != "hl-eth-momentum" {
			t.Errorf("trades = %+v, want one row read back under the process identifier", ss.TradeHistory)
		}
		if !reloaded.partitionLatched(livePartition) {
			t.Error("kill-switch latch did not survive the rename")
		}

		raw, err := openStateDBReadOnly(livePath)
		if err != nil {
			t.Fatalf("openStateDBReadOnly: %v", err)
		}
		defer raw.Close()
		var storedID string
		if err := raw.db.QueryRow("SELECT id FROM strategies").Scan(&storedID); err != nil {
			t.Fatalf("read stored id: %v", err)
		}
		if storedID != "hl-perps-eth" {
			t.Errorf("stored id = %q, want the original %q (a rename must not rewrite storage)", storedID, "hl-perps-eth")
		}
	})
	t.Run("OrphanBookIsReportedNotAdopted", func(t *testing.T) {
		cfg := splitTestConfig(t)
		store := openSplitStore(t, cfg)
		state := splitTestState(t)
		state.Strategies["hl-paper"].Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: 1, AvgCost: 60000}
		if err := SaveStateWithStore(state, store); err != nil {
			t.Fatalf("SaveStateWithStore: %v", err)
		}
		store.Close()

		trimmed := &Config{DBFile: cfg.DBFile, PaperDBFile: cfg.PaperDBFile, Strategies: cfg.Strategies[:1]}
		store2 := openSplitStore(t, trimmed)
		reloaded, orphans, err := LoadStateWithStore(trimmed, store2)
		if err != nil {
			t.Fatalf("LoadStateWithStore: %v", err)
		}
		if _, adopted := reloaded.Strategies["hl-paper"]; adopted {
			t.Fatal("an orphan book was keyed into the roster")
		}
		if len(orphans) != 1 {
			t.Fatalf("orphans = %+v, want exactly one", orphans)
		}
		if orphans[0].StorageID != "hl-paper" || orphans[0].Role != storageRolePaper || orphans[0].PositionCount != 1 {
			t.Errorf("orphan = %+v, want hl-paper in the paper file holding 1 position", orphans[0])
		}
	})
	t.Run("FaultInjectionKeepsCommittedScopeIntact", func(t *testing.T) {
		cfg := splitTestConfig(t)
		store := openSplitStore(t, cfg)
		state := splitTestState(t)
		RecordTrade(state.Strategies["hl-live"], Trade{StrategyID: "hl-live", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2000, Timestamp: time.Unix(3, 0).UTC()})
		RecordTrade(state.Strategies["hl-paper"], Trade{StrategyID: "hl-paper", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2100, Timestamp: time.Unix(3, 0).UTC()})

		origHook := storeCommitHook
		storeCommitHook = func(role storageRole) error {
			if role == storageRolePaper {
				return errors.New("injected paper commit failure")
			}
			return nil
		}
		outcomes := store.SaveAll(state)
		storeCommitHook = origHook

		if outcomes[livePartition] != nil {
			t.Fatalf("live save = %v, want success", outcomes[livePartition])
		}
		if outcomes[defaultPaperPartition] == nil {
			t.Fatal("paper save = nil, want the injected failure")
		}
		if store.saveFailures(livePartition) != 0 || store.saveFailures(defaultPaperPartition) != 1 {
			t.Errorf("failures = live %d / paper %d, want 0 / 1", store.saveFailures(livePartition), store.saveFailures(defaultPaperPartition))
		}
		if store.persistenceHoldsPartition(livePartition) {
			t.Error("the live scope is held although its save committed")
		}
		if !store.persistenceHoldsPartition(defaultPaperPartition) {
			t.Error("the paper scope is not held although its save failed")
		}

		if !state.Strategies["hl-live"].TradeHistory[0].persisted {
			t.Error("the committed live trade is not marked persisted")
		}
		if state.Strategies["hl-paper"].TradeHistory[0].persisted {
			t.Error("the failed paper trade was marked persisted, so a retry would drop it")
		}

		for part, err := range store.SaveAll(state) {
			if err != nil {
				t.Fatalf("retry SaveAll(%s): %v", partitionLabel(part), err)
			}
		}
		if store.persistenceHoldsPartition(defaultPaperPartition) {
			t.Error("the paper hold survived a successful retry")
		}
		for _, tc := range []struct {
			role storageRole
			id   string
		}{{storageRolePrimary, "hl-live"}, {storageRolePaper, "hl-paper"}} {
			var n int
			if err := store.file(tc.role).db.QueryRow("SELECT COUNT(*) FROM trades WHERE strategy_id = ?", tc.id).Scan(&n); err != nil {
				t.Fatalf("count trades in %s: %v", tc.role, err)
			}
			if n != 1 {
				t.Errorf("%s trades = %d, want exactly 1 (no duplicate booking across the retry)", tc.id, n)
			}
		}
	})
	t.Run("PaperBooksLoadWithoutPrimaryMetadata", func(t *testing.T) {
		cfg := splitTestConfig(t)
		store := openSplitStore(t, cfg)
		state := splitTestState(t)
		if err := store.SavePartition(state, defaultPaperPartition); err != nil {
			t.Fatalf("SaveScope(paper): %v", err)
		}
		if _, err := store.primary().db.Exec("DELETE FROM app_state"); err != nil {
			t.Fatalf("clear app_state: %v", err)
		}

		reloaded, _, err := LoadStateWithStore(cfg, store)
		if err != nil {
			t.Fatalf("LoadStateWithStore: %v", err)
		}
		if reloaded.Strategies["hl-paper"] == nil {
			t.Fatalf("paper book lost when the primary carries no metadata row; roster = %v", reloaded.Strategies)
		}
		if reloaded.Strategies["hl-paper"].Cash != 2000 {
			t.Errorf("paper cash = %.2f, want 2000", reloaded.Strategies["hl-paper"].Cash)
		}
	})
}

func TestStateStoreLegacyUnscopedRowPlacedPerFile(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	for _, role := range []storageRole{storageRolePrimary, storageRolePaper} {
		db := store.file(role)
		if _, err := db.db.Exec(`INSERT INTO app_state (id, cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries, last_summary_post) VALUES (1, 0, '', '', '', '')`); err != nil {
			t.Fatalf("seed app_state (%s): %v", role, err)
		}
		if _, err := db.db.Exec(`INSERT INTO portfolio_risk (scope, peak_value, kill_switch_active) VALUES ('', ?, 1)`, 111.0); err != nil {
			t.Fatalf("seed legacy risk row (%s): %v", role, err)
		}
	}

	reloaded, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatalf("LoadStateWithStore: %v", err)
	}
	if !reloaded.partitionLatched(livePartition) {
		t.Error("the primary file's legacy latch did not reach the live scope")
	}
	if !reloaded.partitionLatched(defaultPaperPartition) {
		t.Error("the paper file's legacy latch did not reach the paper scope")
	}
	if reloaded.partitionRisk(defaultPaperPartition).PeakValue != 111 {
		t.Errorf("paper peak = %.0f, want the paper file's legacy row", reloaded.partitionRisk(defaultPaperPartition).PeakValue)
	}
	if _, still := reloaded.PortfolioRisk[unassignedPartition]; still {
		t.Error("an unscoped row survived placement")
	}

	again, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatalf("second LoadStateWithStore: %v", err)
	}
	if _, still := again.PortfolioRisk[unassignedPartition]; still {
		t.Error("the placement is not durable; a second boot still finds an unscoped row")
	}
}

func TestPortfolioRiskPeakRebaselinePendingMigration(t *testing.T) {
	readPending := func(t *testing.T, sdb *StateDB, scope string) int {
		t.Helper()
		var v int
		if err := sdb.db.QueryRow(`SELECT peak_rebaseline_pending FROM portfolio_risk WHERE scope = ?`, scope).Scan(&v); err != nil {
			t.Fatalf("read peak_rebaseline_pending: %v", err)
		}
		return v
	}

	t.Run("PreUpdateFileReadOnlyThenMigratedAndPreserved", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		sdb, err := OpenStateDB(path)
		if err != nil {
			t.Fatalf("OpenStateDB: %v", err)
		}
		if _, err := sdb.db.Exec(`ALTER TABLE portfolio_risk DROP COLUMN peak_rebaseline_pending`); err != nil {
			t.Fatalf("drop column to model a pre-update file: %v", err)
		}
		if _, err := sdb.db.Exec(`INSERT INTO portfolio_risk (scope, peak_value) VALUES ('live', 100), ('paper', 200)`); err != nil {
			t.Fatalf("seed rows: %v", err)
		}
		sdb.Close()

		ro, err := openStateDBReadOnly(path)
		if err != nil {
			t.Fatalf("openStateDBReadOnly: %v", err)
		}
		books, err := ro.loadScopeBooks([]PortfolioScope{ScopeLive, ScopePaper})
		ro.Close()
		if err != nil {
			t.Fatalf("read-only load of a pre-update file: %v", err)
		}
		if prs := books.PortfolioRisk[ScopeLive]; prs == nil || prs.PeakValue != 100 || prs.PeakRebaselinePending {
			t.Fatalf("read-only load = %+v, want peak 100 and no pending flag", prs)
		}

		sdb, err = OpenStateDB(path)
		if err != nil {
			t.Fatalf("migrating OpenStateDB: %v", err)
		}
		if readPending(t, sdb, "live") != 0 || readPending(t, sdb, "paper") != 0 {
			t.Fatal("migrated rows must default to 0")
		}
		if _, err := sdb.db.Exec(`UPDATE portfolio_risk SET peak_rebaseline_pending = 1 WHERE scope = 'paper'`); err != nil {
			t.Fatalf("set flag: %v", err)
		}
		sdb.Close()
		for i := 0; i < 2; i++ {
			sdb, err = OpenStateDB(path)
			if err != nil {
				t.Fatalf("repeat migration %d: %v", i, err)
			}
			if readPending(t, sdb, "paper") != 1 || readPending(t, sdb, "live") != 0 {
				t.Fatalf("repeat migration %d changed the stored flags", i)
			}
			books, err := sdb.loadScopeBooks([]PortfolioScope{ScopeLive, ScopePaper})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if !books.PortfolioRisk[ScopePaper].PeakRebaselinePending || books.PortfolioRisk[ScopeLive].PeakRebaselinePending {
				t.Fatal("load must map the stored flag to its own scope")
			}
			sdb.Close()
		}
	})

	t.Run("LegacyUnscopedTableRebuildDefaultsToZero", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		sdb, err := OpenStateDB(path)
		if err != nil {
			t.Fatalf("OpenStateDB: %v", err)
		}
		for _, stmt := range []string{
			`DROP TABLE portfolio_risk`,
			`CREATE TABLE portfolio_risk (id INTEGER PRIMARY KEY CHECK (id = 1), peak_value REAL NOT NULL DEFAULT 0, current_drawdown_pct REAL NOT NULL DEFAULT 0, kill_switch_active INTEGER NOT NULL DEFAULT 0, kill_switch_at TEXT NOT NULL DEFAULT '', warning_sent INTEGER NOT NULL DEFAULT 0)`,
			`INSERT INTO portfolio_risk (id, peak_value, kill_switch_active) VALUES (1, 555, 1)`,
		} {
			if _, err := sdb.db.Exec(stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		sdb.Close()
		for i := 0; i < 2; i++ {
			sdb, err = OpenStateDB(path)
			if err != nil {
				t.Fatalf("migrate legacy table (pass %d): %v", i, err)
			}
			if readPending(t, sdb, "") != 0 {
				t.Fatal("the legacy row must copy through the scoped rebuild with the flag at 0")
			}
			books, err := sdb.loadScopeBooks([]PortfolioScope{scopeUnassigned})
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if prs := books.PortfolioRisk[scopeUnassigned]; prs == nil || prs.PeakValue != 555 || !prs.KillSwitchActive || prs.PeakRebaselinePending {
				t.Fatalf("legacy row after rebuild = %+v", prs)
			}
			sdb.Close()
		}
	})

	t.Run("LatchedRowNeverLoadsPending", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		sdb, err := OpenStateDB(path)
		if err != nil {
			t.Fatalf("OpenStateDB: %v", err)
		}
		defer sdb.Close()
		if _, err := sdb.db.Exec(`INSERT INTO portfolio_risk (scope, peak_value, kill_switch_active, peak_rebaseline_pending) VALUES ('live', 100, 1, 1)`); err != nil {
			t.Fatalf("seed: %v", err)
		}
		books, err := sdb.loadScopeBooks([]PortfolioScope{ScopeLive})
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if prs := books.PortfolioRisk[ScopeLive]; prs.PeakRebaselinePending {
			t.Fatal("a latched row must load with the pending change cancelled")
		}
	})
}
