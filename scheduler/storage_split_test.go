package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func splitTestConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{
		DBFile:      filepath.Join(dir, "live.db"),
		PaperDBFile: filepath.Join(dir, "paper.db"),
		Strategies: []StrategyConfig{
			{ID: "hl-live", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=live"}},
			{ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=paper"}},
		},
	}
}

func openSplitStore(t *testing.T, cfg *Config) *StateStore {
	t.Helper()
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatalf("resolveStorageLayout: %v", err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatalf("buildStorageIdentityMap: %v", err)
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		t.Fatalf("OpenStateStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func splitTestState(t *testing.T) *AppState {
	t.Helper()
	state := NewAppState()
	state.Strategies["hl-live"] = &StrategyState{
		ID: "hl-live", Type: "perps", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000,
		Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
	}
	state.Strategies["hl-paper"] = &StrategyState{
		ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Cash: 2000, InitialCapital: 2000,
		Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
	}
	return state
}

func TestResolveStorageLayout(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	linkDir := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	livePath := filepath.Join(realDir, "live.db")
	if err := os.WriteFile(livePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write live: %v", err)
	}
	symlinkedFile := filepath.Join(dir, "live-alias.db")
	if err := os.Symlink(livePath, symlinkedFile); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	hardLink := filepath.Join(dir, "live-hard.db")
	if err := os.Link(livePath, hardLink); err != nil {
		t.Fatalf("hard link: %v", err)
	}

	cases := []struct {
		name      string
		db        string
		paper     string
		wantSplit bool
		wantErr   string
	}{
		{name: "single file", db: livePath, wantSplit: false},
		{name: "distinct files", db: livePath, paper: filepath.Join(realDir, "paper.db"), wantSplit: true},
		{name: "paper through a symlinked parent is still distinct", db: livePath, paper: filepath.Join(linkDir, "paper.db"), wantSplit: true},
		{name: "same path twice", db: livePath, paper: livePath, wantErr: "same physical file"},
		{name: "symlinked file alias", db: livePath, paper: symlinkedFile, wantErr: "same physical file"},
		{name: "hard link alias", db: livePath, paper: hardLink, wantErr: "same physical file"},
		{name: "live through a symlinked parent aliases the real path", db: filepath.Join(linkDir, "live.db"), paper: livePath, wantErr: "same physical file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layout, err := resolveStorageLayout(&Config{DBFile: tc.db, PaperDBFile: tc.paper})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveStorageLayout: %v", err)
			}
			if layout.Split != tc.wantSplit {
				t.Fatalf("Split = %v, want %v", layout.Split, tc.wantSplit)
			}
			if !filepath.IsAbs(layout.Files[0].Canonical) {
				t.Errorf("canonical path %q is not absolute", layout.Files[0].Canonical)
			}
		})
	}

	t.Run("relative paths resolve against the working directory", func(t *testing.T) {
		layout, err := resolveStorageLayout(&Config{DBFile: "scheduler/state.db", PaperDBFile: "scheduler/paper.db"})
		if err != nil {
			t.Fatalf("resolveStorageLayout: %v", err)
		}
		if !layout.Split || len(layout.Files) != 2 {
			t.Fatalf("layout = %+v, want a two-file split", layout)
		}
		if layout.Files[0].Canonical == layout.Files[1].Canonical {
			t.Errorf("relative paths collapsed to one canonical path %q", layout.Files[0].Canonical)
		}
	})
}

func TestValidateConfigStorageIdentity(t *testing.T) {
	cases := []struct {
		name       string
		paperFile  string
		strategies []StrategyConfig
		wantErr    string
	}{
		{
			name:      "same storage id in different files is the supported alias",
			paperFile: "paper.db",
			strategies: []StrategyConfig{
				{ID: "hl-live", StorageStrategyID: "hl", Args: []string{"--mode=live"}},
				{ID: "hl-paper", StorageStrategyID: "hl", Args: []string{"--mode=paper"}},
			},
		},
		{
			name:      "duplicate storage id inside one file is a config error",
			paperFile: "paper.db",
			strategies: []StrategyConfig{
				{ID: "hl-live-a", StorageStrategyID: "hl", Args: []string{"--mode=live"}},
				{ID: "hl-live-b", StorageStrategyID: "hl", Args: []string{"--mode=live"}},
			},
			wantErr: "already used by strategy",
		},
		{
			name: "single file collapses both scopes, so an alias collides",
			strategies: []StrategyConfig{
				{ID: "hl-live", StorageStrategyID: "hl", Args: []string{"--mode=live"}},
				{ID: "hl-paper", StorageStrategyID: "hl", Args: []string{"--mode=paper"}},
			},
			wantErr: "already used by strategy",
		},
		{
			name: "a blank storage id is rejected instead of silently defaulting",
			strategies: []StrategyConfig{
				{ID: "hl-live", StorageStrategyID: "   ", Args: []string{"--mode=live"}},
			},
			wantErr: "storage_strategy_id is blank",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateStorageIdentityConfig(&Config{PaperDBFile: tc.paperFile, Strategies: tc.strategies})
			joined := strings.Join(errs, "\n")
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %v, want none", errs)
				}
				return
			}
			if !strings.Contains(joined, tc.wantErr) {
				t.Fatalf("errors = %v, want one containing %q", errs, tc.wantErr)
			}
		})
	}
}

func TestHotReloadStorageIdentity(t *testing.T) {
	base := &Config{
		DBFile:      "live.db",
		PaperDBFile: "paper.db",
		Strategies:  []StrategyConfig{{ID: "hl-live", StorageStrategyID: "hl", Args: []string{"--mode=live"}}},
	}
	t.Run("paper_db_file change is restart-required", func(t *testing.T) {
		next := *base
		next.PaperDBFile = "other.db"
		err := validateHotReloadCompatible(base, &next)
		if err == nil || !strings.Contains(err.Error(), "paper_db_file changed") {
			t.Fatalf("err = %v, want a paper_db_file rejection", err)
		}
	})
	t.Run("storage_strategy_id change is restart-required", func(t *testing.T) {
		next := *base
		next.Strategies = []StrategyConfig{{ID: "hl-live", StorageStrategyID: "hl2", Args: []string{"--mode=live"}}}
		err := validateHotReloadCompatible(base, &next)
		if err == nil || !strings.Contains(err.Error(), "storage_strategy_id changed") {
			t.Fatalf("err = %v, want a storage_strategy_id rejection", err)
		}
	})
	t.Run("an unchanged effective identity reloads", func(t *testing.T) {
		next := *base
		next.Strategies = []StrategyConfig{{ID: "hl-live", StorageStrategyID: " hl ", Args: []string{"--mode=live"}}}
		if errs := storageIdentityReloadErrors(base, &next); len(errs) != 0 {
			t.Fatalf("errors = %v, want none (the trimmed identity is unchanged)", errs)
		}
	})
}

func TestStorageIdentityMap(t *testing.T) {
	cfg := &Config{
		DBFile:      "live.db",
		PaperDBFile: "paper.db",
		Strategies: []StrategyConfig{
			{ID: "hl-live", StorageStrategyID: "hl", Args: []string{"--mode=live"}},
			{ID: "hl-paper", StorageStrategyID: "hl", Args: []string{"--mode=paper"}},
			{ID: "spot-paper", Args: []string{"--mode=paper"}},
		},
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatalf("resolveStorageLayout: %v", err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatalf("buildStorageIdentityMap: %v", err)
	}

	cases := []struct {
		procID    string
		wantRole  storageRole
		wantScope PortfolioScope
		wantStore string
	}{
		{"hl-live", storageRolePrimary, ScopeLive, "hl"},
		{"hl-paper", storageRolePaper, ScopePaper, "hl"},
		{"spot-paper", storageRolePaper, ScopePaper, "spot-paper"},
	}
	for _, tc := range cases {
		got, ok := ident.storageFor(tc.procID)
		if !ok {
			t.Fatalf("storageFor(%q) missing", tc.procID)
		}
		if got.Role != tc.wantRole || got.Scope != tc.wantScope || got.StorageID != tc.wantStore {
			t.Errorf("storageFor(%q) = %+v, want role %s scope %s id %q", tc.procID, got, tc.wantRole, tc.wantScope, tc.wantStore)
		}
		back, ok := ident.processFor(tc.wantRole, tc.wantStore)
		if !ok || back != tc.procID {
			t.Errorf("processFor(%s, %q) = %q/%v, want %q", tc.wantRole, tc.wantStore, back, ok, tc.procID)
		}
	}
	if _, ok := ident.storageFor("not-configured"); ok {
		t.Error("storageFor returned a mapping for an unconfigured strategy")
	}
	if _, ok := ident.processFor(storageRolePrimary, "hl-paper"); ok {
		t.Error("the primary file must not resolve a paper-only stored identifier")
	}

	single := &Config{DBFile: "live.db", Strategies: []StrategyConfig{
		{ID: "hl-live", Args: []string{"--mode=live"}},
		{ID: "hl-paper", Args: []string{"--mode=paper"}},
	}}
	singleLayout, err := resolveStorageLayout(single)
	if err != nil {
		t.Fatalf("resolveStorageLayout single: %v", err)
	}
	singleIdent, err := buildStorageIdentityMap(single, singleLayout)
	if err != nil {
		t.Fatalf("buildStorageIdentityMap single: %v", err)
	}
	for _, id := range []string{"hl-live", "hl-paper"} {
		got, ok := singleIdent.storageFor(id)
		if !ok || got.Role != storageRolePrimary || got.StorageID != id {
			t.Errorf("single-file storageFor(%q) = %+v/%v, want the primary file with an unchanged id", id, got, ok)
		}
	}
}

func TestSchedulerNeedsOwnership(t *testing.T) {
	for _, once := range []bool{false, true} {
		if !schedulerNeedsOwnership(once) {
			t.Errorf("schedulerNeedsOwnership(once=%v) = false, want true (a one-shot cycle writes books too)", once)
		}
	}
}

func TestAcquireStateOwnership(t *testing.T) {
	dir := t.TempDir()
	specs := []storageFileSpec{
		{Role: storageRolePrimary, Path: filepath.Join(dir, "live.db"), Canonical: filepath.Join(dir, "live.db")},
		{Role: storageRolePaper, Path: filepath.Join(dir, "paper.db"), Canonical: filepath.Join(dir, "paper.db")},
	}

	owned, err := acquireStateOwnership(specs)
	if err != nil {
		t.Fatalf("acquireStateOwnership: %v", err)
	}
	if len(owned.locks) != 2 {
		t.Fatalf("held %d lock(s), want 2", len(owned.locks))
	}

	if _, err := acquireStateOwnership(specs); err == nil {
		t.Fatal("a second acquisition succeeded while the first is held")
	}
	owned.Release()

	// A held second file must release the first, so nothing leaks.
	paperOnly, err := acquireStateDBLock(specs[1].Path)
	if err != nil {
		t.Fatalf("acquireStateDBLock paper: %v", err)
	}
	_, err = acquireStateOwnership(specs)
	if err == nil {
		t.Fatal("ownership succeeded while the paper file was locked")
	}
	var locked *stateDBLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("err = %v, want a stateDBLockedError naming the holder", err)
	}
	paperOnly.Release()
	again, err := acquireStateOwnership(specs)
	if err != nil {
		t.Fatalf("ownership after a failed acquisition: %v — the primary lock leaked", err)
	}
	again.Release()

	t.Run("an in-memory path takes no lock", func(t *testing.T) {
		mem, err := acquireStateOwnership([]storageFileSpec{{Role: storageRolePrimary, Path: ":memory:", InMemory: true}})
		if err != nil {
			t.Fatalf("acquireStateOwnership in-memory: %v", err)
		}
		defer mem.Release()
		if len(mem.locks) != 0 {
			t.Errorf("held %d lock(s) for an in-memory path, want 0", len(mem.locks))
		}
	})
}

func TestDeletePendingManualActionsByID(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	for _, id := range []string{"a", "b", "c"} {
		if err := db.InsertPendingManualAction(PendingManualAction{
			StrategyID: id, Action: "close", Symbol: "ETH", Side: "sell",
			Quantity: 1, FillPrice: 100, CreatedAt: now,
		}); err != nil {
			t.Fatalf("InsertPendingManualAction: %v", err)
		}
	}
	rows, err := db.LoadPendingManualActions()
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %d (err=%v), want 3", len(rows), err)
	}

	// Acknowledge only the highest id: the lower, failed rows must survive.
	if err := db.DeletePendingManualActionsByID([]int64{rows[2].ID}); err != nil {
		t.Fatalf("DeletePendingManualActionsByID: %v", err)
	}
	after, err := db.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	if len(after) != 2 || after[0].StrategyID != "a" || after[1].StrategyID != "b" {
		t.Fatalf("surviving rows = %+v, want the two lower ids untouched", after)
	}
}

func TestStateStoreSplitRoundTrip(t *testing.T) {
	cfg := splitTestConfig(t)
	cfg.Strategies[0].StorageStrategyID = "hl"
	cfg.Strategies[1].StorageStrategyID = "hl"
	store := openSplitStore(t, cfg)

	state := splitTestState(t)
	state.Strategies["hl-live"].Positions["ETH"] = &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000}
	state.Strategies["hl-paper"].Positions["ETH"] = &Position{Symbol: "ETH", Side: "short", Quantity: 2, AvgCost: 2100}
	RecordTrade(state.Strategies["hl-live"], Trade{StrategyID: "hl-live", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2000, Timestamp: time.Unix(1, 0).UTC()})
	RecordTrade(state.Strategies["hl-paper"], Trade{StrategyID: "hl-paper", Symbol: "ETH", Side: "sell", Quantity: 2, Price: 2100, Timestamp: time.Unix(1, 0).UTC()})
	state.scopeRisk(ScopeLive).PeakValue = 1000
	state.scopeRisk(ScopePaper).PeakValue = 2000

	for scope, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatalf("SaveAll(%s): %v", scopeLabel(scope), err)
		}
	}

	// Both files store the SAME identifier "hl": each file must hold only its
	// own scope's book.
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
	if reloaded.scopeRisk(ScopeLive).PeakValue != 1000 || reloaded.scopeRisk(ScopePaper).PeakValue != 2000 {
		t.Errorf("risk peaks = live %.0f / paper %.0f, want 1000 / 2000",
			reloaded.scopeRisk(ScopeLive).PeakValue, reloaded.scopeRisk(ScopePaper).PeakValue)
	}
}

func TestStateStoreProcessAliasResumesTheStoredBook(t *testing.T) {
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
	state.scopeRisk(ScopeLive).KillSwitchActive = true
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Rename only the process identifier; the stored identity stays put.
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
	if !reloaded.scopeLatched(ScopeLive) {
		t.Error("kill-switch latch did not survive the rename")
	}

	// The stored identifier is never rewritten.
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
}

func TestStateStoreOrphanBookIsReportedNotAdopted(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	state := splitTestState(t)
	state.Strategies["hl-paper"].Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: 1, AvgCost: 60000}
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}
	store.Close()

	// Drop hl-paper from config: its stored book becomes an orphan.
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
}

func TestStateStoreFaultInjectionKeepsCommittedScopeIntact(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	state := splitTestState(t)
	RecordTrade(state.Strategies["hl-live"], Trade{StrategyID: "hl-live", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2000, Timestamp: time.Unix(3, 0).UTC()})
	RecordTrade(state.Strategies["hl-paper"], Trade{StrategyID: "hl-paper", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2100, Timestamp: time.Unix(3, 0).UTC()})

	// Fail the paper commit after the primary commits.
	origHook := storeCommitHook
	storeCommitHook = func(role storageRole) error {
		if role == storageRolePaper {
			return errors.New("injected paper commit failure")
		}
		return nil
	}
	outcomes := store.SaveAll(state)
	storeCommitHook = origHook

	if outcomes[ScopeLive] != nil {
		t.Fatalf("live save = %v, want success", outcomes[ScopeLive])
	}
	if outcomes[ScopePaper] == nil {
		t.Fatal("paper save = nil, want the injected failure")
	}
	if store.saveFailures(ScopeLive) != 0 || store.saveFailures(ScopePaper) != 1 {
		t.Errorf("failures = live %d / paper %d, want 0 / 1", store.saveFailures(ScopeLive), store.saveFailures(ScopePaper))
	}
	if store.persistenceHoldsScope(ScopeLive) {
		t.Error("the live scope is held although its save committed")
	}
	if !store.persistenceHoldsScope(ScopePaper) {
		t.Error("the paper scope is not held although its save failed")
	}

	// The committed live trade is marked persisted; the failed paper trade is
	// not, so the retry writes it exactly once.
	if !state.Strategies["hl-live"].TradeHistory[0].persisted {
		t.Error("the committed live trade is not marked persisted")
	}
	if state.Strategies["hl-paper"].TradeHistory[0].persisted {
		t.Error("the failed paper trade was marked persisted, so a retry would drop it")
	}

	// Retry: the live file must not double-book its trade.
	for scope, err := range store.SaveAll(state) {
		if err != nil {
			t.Fatalf("retry SaveAll(%s): %v", scopeLabel(scope), err)
		}
	}
	if store.persistenceHoldsScope(ScopePaper) {
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
}

func TestStateStorePaperBooksLoadWithoutPrimaryMetadata(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	state := splitTestState(t)
	if err := store.SaveScope(state, ScopePaper); err != nil {
		t.Fatalf("SaveScope(paper): %v", err)
	}
	// The primary file has no process metadata row at all.
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
	// Each file's row lands in the scope that file owns, and the latch carries
	// across untouched. The live peak is re-based onto the live-only roster by
	// the #1509 rule; the paper peak is kept as recorded.
	if !reloaded.scopeLatched(ScopeLive) {
		t.Error("the primary file's legacy latch did not reach the live scope")
	}
	if !reloaded.scopeLatched(ScopePaper) {
		t.Error("the paper file's legacy latch did not reach the paper scope")
	}
	if reloaded.scopeRisk(ScopePaper).PeakValue != 111 {
		t.Errorf("paper peak = %.0f, want the paper file's legacy row", reloaded.scopeRisk(ScopePaper).PeakValue)
	}
	if _, still := reloaded.PortfolioRisk[scopeUnassigned]; still {
		t.Error("an unscoped row survived placement")
	}

	again, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatalf("second LoadStateWithStore: %v", err)
	}
	if _, still := again.PortfolioRisk[scopeUnassigned]; still {
		t.Error("the placement is not durable; a second boot still finds an unscoped row")
	}
}

func TestStateStoreLegacyUnscopedRowRejectedWithoutALiveScope(t *testing.T) {
	cfg := splitTestConfig(t)
	cfg.Strategies = cfg.Strategies[1:] // paper only
	store := openSplitStore(t, cfg)
	db := store.primary()
	if _, err := db.db.Exec(`INSERT INTO app_state (id, cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries, last_summary_post) VALUES (1, 0, '', '', '', '')`); err != nil {
		t.Fatalf("seed app_state: %v", err)
	}
	if _, err := db.db.Exec(`INSERT INTO portfolio_risk (scope, peak_value, kill_switch_active) VALUES ('', 500, 1)`); err != nil {
		t.Fatalf("seed legacy risk row: %v", err)
	}
	_, _, err := LoadStateWithStore(cfg, store)
	if err == nil || !strings.Contains(err.Error(), "resolve the row by hand") {
		t.Fatalf("err = %v, want a refusal to place an ambiguous legacy row", err)
	}
}

func TestStateStoreCombinedTradeHistoryPagesDeterministically(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	state := splitTestState(t)
	// Identical timestamps in both files, and identical row identifiers.
	ts := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 3; i++ {
		RecordTrade(state.Strategies["hl-live"], Trade{StrategyID: "hl-live", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2000, Timestamp: ts})
		RecordTrade(state.Strategies["hl-paper"], Trade{StrategyID: "hl-paper", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2100, Timestamp: ts})
	}
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}

	all, total, err := store.QueryTradeHistory("", "", time.Time{}, time.Time{}, 100, 0)
	if err != nil {
		t.Fatalf("QueryTradeHistory: %v", err)
	}
	if total != 6 || len(all) != 6 {
		t.Fatalf("combined read = %d rows (total %d), want 6/6", len(all), total)
	}
	var page []Trade
	for offset := 0; offset < 6; offset += 2 {
		got, gotTotal, err := store.QueryTradeHistory("", "", time.Time{}, time.Time{}, 2, offset)
		if err != nil {
			t.Fatalf("QueryTradeHistory(offset=%d): %v", offset, err)
		}
		if gotTotal != 6 {
			t.Errorf("total at offset %d = %d, want 6", offset, gotTotal)
		}
		page = append(page, got...)
	}
	if len(page) != 6 {
		t.Fatalf("paged rows = %d, want 6", len(page))
	}
	for i := range all {
		if page[i].StrategyID != all[i].StrategyID || page[i].SourceScope != all[i].SourceScope || page[i].sourceRowID != all[i].sourceRowID {
			t.Fatalf("page[%d] = %s/%s/%d, want %s/%s/%d — paging is not stable",
				i, page[i].StrategyID, page[i].SourceScope, page[i].sourceRowID,
				all[i].StrategyID, all[i].SourceScope, all[i].sourceRowID)
		}
	}
	liveSeen, paperSeen := 0, 0
	for _, tr := range all {
		switch tr.SourceScope {
		case ScopeLive:
			liveSeen++
		case ScopePaper:
			paperSeen++
		default:
			t.Errorf("row %+v carries no source scope", tr)
		}
	}
	if liveSeen != 3 || paperSeen != 3 {
		t.Errorf("scope split = live %d / paper %d, want 3 / 3", liveSeen, paperSeen)
	}
}

func TestStateStoreCombinedReadFailsWhenAFileIsUnreadable(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	state := splitTestState(t)
	RecordTrade(state.Strategies["hl-live"], Trade{StrategyID: "hl-live", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2000, Timestamp: time.Unix(4, 0).UTC()})
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}
	store.file(storageRolePaper).Close()

	if _, _, err := store.QueryTradeHistory("", "", time.Time{}, time.Time{}, 10, 0); err == nil {
		t.Error("combined trade history succeeded with an unreadable paper file")
	}
	if _, err := store.LifetimeTradeStatsAll(); err == nil {
		t.Error("combined lifetime stats succeeded with an unreadable paper file")
	}
	if _, err := store.RecentTrades(time.Time{}, 10); err == nil {
		t.Error("combined recent trades succeeded with an unreadable paper file")
	}
}

func TestStateStoreRoutesLiveOnlyWritesAndRefusesPaperStrategies(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)

	if _, err := store.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: "hl-paper", Symbol: "ETH", Side: "buy", OrderOID: 1, LimitPrice: 2000, OrderSize: 1, CreatedAt: time.Now().UTC(),
	}); err == nil || !strings.Contains(err.Error(), "live-only") {
		t.Fatalf("err = %v, want a refusal to queue a paper-scope limit order", err)
	}

	if _, err := store.InsertPendingLimitOrder(PendingLimitOrder{
		StrategyID: "hl-live", Symbol: "ETH", Side: "buy", OrderOID: 2, LimitPrice: 2000, OrderSize: 1, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertPendingLimitOrder(live): %v", err)
	}
	var n int
	if err := store.primary().db.QueryRow("SELECT COUNT(*) FROM pending_limit_orders").Scan(&n); err != nil {
		t.Fatalf("count primary limit orders: %v", err)
	}
	if n != 1 {
		t.Errorf("primary limit orders = %d, want 1", n)
	}
	if err := store.file(storageRolePaper).db.QueryRow("SELECT COUNT(*) FROM pending_limit_orders").Scan(&n); err != nil {
		t.Fatalf("count paper limit orders: %v", err)
	}
	if n != 0 {
		t.Errorf("paper limit orders = %d, want 0 (the table is live-only)", n)
	}

	// A read over the live-only table reports "none" for a paper strategy so
	// manual-open/manual-add stay usable; an unmapped id still errors.
	if cnt, err := store.CountPendingLimitOrders("hl-paper", "ETH"); err != nil || cnt != 0 {
		t.Errorf("CountPendingLimitOrders(paper) = %d, %v; want 0, nil", cnt, err)
	}
	if cnt, err := store.CountPendingLimitOrders("hl-live", "ETH"); err != nil || cnt != 1 {
		t.Errorf("CountPendingLimitOrders(live) = %d, %v; want 1, nil", cnt, err)
	}
	if _, err := store.CountPendingLimitOrders("unmapped", "ETH"); err == nil {
		t.Error("CountPendingLimitOrders(unmapped) = nil error, want a storage-owner refusal")
	}
}

func TestDrainPendingManualActionsPerFile(t *testing.T) {
	cfg := splitTestConfig(t)
	cfg.Strategies[0].Type = "manual"
	cfg.Strategies[1].Type = "manual"
	store := openSplitStore(t, cfg)
	state := splitTestState(t)
	state.Strategies["hl-live"].Type = "manual"
	state.Strategies["hl-paper"].Type = "manual"
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}

	origRecorder := tradeRecorder
	tradeRecorder = func(_ string, _ Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()

	now := time.Now().UTC()
	// A failing paper action (no open position) and a succeeding live open.
	if err := store.InsertPendingManualAction(PendingManualAction{
		StrategyID: "hl-paper", Action: "close", Symbol: "ETH", Side: "sell",
		Quantity: 1, FillPrice: 2100, IsFullClose: true, CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert paper close: %v", err)
	}
	if err := store.InsertPendingManualAction(PendingManualAction{
		StrategyID: "hl-live", Action: "open", Symbol: "ETH", Side: "long",
		Quantity: 0.5, FillPrice: 2000, EntryATR: 50, CreatedAt: now,
	}); err != nil {
		t.Fatalf("insert live open: %v", err)
	}

	alerts := drainPendingManualActions(state, cfg, store)
	if len(alerts) != 1 || alerts[0].sc.ID != "hl-live" {
		t.Fatalf("alerts = %+v, want one for hl-live", alerts)
	}

	var liveRows, paperRows int
	if err := store.primary().db.QueryRow("SELECT COUNT(*) FROM pending_manual_actions").Scan(&liveRows); err != nil {
		t.Fatalf("count primary actions: %v", err)
	}
	if err := store.file(storageRolePaper).db.QueryRow("SELECT COUNT(*) FROM pending_manual_actions").Scan(&paperRows); err != nil {
		t.Fatalf("count paper actions: %v", err)
	}
	if liveRows != 0 {
		t.Errorf("primary queue = %d rows, want 0 (the applied action was acknowledged)", liveRows)
	}
	if paperRows != 1 {
		t.Errorf("paper queue = %d rows, want 1 (the failed action survives the other file's acknowledgement)", paperRows)
	}
	if pos := state.Strategies["hl-live"].Positions["ETH"]; pos == nil || pos.Quantity != 0.5 {
		t.Errorf("live position = %+v, want the drained 0.5 ETH open", pos)
	}
}

func TestSaveFailuresPerScope(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	state := splitTestState(t)

	origHook := storeCommitHook
	storeCommitHook = func(role storageRole) error {
		if role == storageRolePaper {
			return errors.New("injected paper commit failure")
		}
		return nil
	}
	for i := 0; i < 3; i++ {
		store.SaveAll(state)
	}
	storeCommitHook = origHook

	if store.saveFailures(ScopeLive) != 0 {
		t.Errorf("live failures = %d, want 0", store.saveFailures(ScopeLive))
	}
	if store.saveFailures(ScopePaper) != 3 {
		t.Errorf("paper failures = %d, want 3", store.saveFailures(ScopePaper))
	}
	if allScopesSaveBlocked(store, cfg) {
		t.Error("allScopesSaveBlocked = true although the live scope saves cleanly")
	}
	due := dueStrategiesPersistable(store, cfg.Strategies)
	if len(due) != 1 || due[0].ID != "hl-live" {
		t.Fatalf("due strategies = %+v, want only hl-live", due)
	}

	// Both scopes blocked: the whole cycle is skipped, as before.
	storeCommitHook = func(role storageRole) error { return errors.New("injected failure") }
	for i := 0; i < 3; i++ {
		store.SaveAll(state)
	}
	storeCommitHook = origHook
	if !allScopesSaveBlocked(store, cfg) {
		t.Error("allScopesSaveBlocked = false although every scope is blocked")
	}
}

func TestPersistenceHoldClearsOnASuccessfulSave(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	state := splitTestState(t)

	origHook := storeCommitHook
	storeCommitHook = func(role storageRole) error {
		if role == storageRoleLive() {
			return errors.New("injected live commit failure")
		}
		return nil
	}
	store.SaveAll(state)
	storeCommitHook = origHook

	if !store.persistenceHoldsScope(ScopeLive) {
		t.Fatal("the live scope is not held after one failed save")
	}
	if store.persistenceHoldsScope(ScopePaper) {
		t.Error("the paper scope is held although its save committed")
	}
	if err := store.SaveScope(state, ScopeLive); err != nil {
		t.Fatalf("SaveScope(live): %v", err)
	}
	if store.persistenceHoldsScope(ScopeLive) {
		t.Error("the hold survived a successful save")
	}
}

// storageRoleLive names the file that owns the live scope in a split layout.
func storageRoleLive() storageRole { return storageRolePrimary }

func TestBackfillPlanBindingRefusesAForeignFile(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	state := splitTestState(t)
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}

	role, storageID, err := bindPlanToStore(store, "hl-paper")
	if err != nil {
		t.Fatalf("bindPlanToStore: %v", err)
	}
	if role != storageRolePaper || storageID != "hl-paper" {
		t.Fatalf("binding = %s/%s, want paper/hl-paper", role, storageID)
	}

	plan := BackfillPlan{StrategyID: "hl-paper", StorageStrategyID: storageID, Role: role, NewCash: 1234}
	if err := store.primary().ApplyBackfillPlan(plan); err == nil || !strings.Contains(err.Error(), "built against the paper state file") {
		t.Fatalf("err = %v, want a refusal to apply a paper plan to the primary file", err)
	}
	if err := store.file(storageRolePaper).ApplyBackfillPlan(plan); err != nil {
		t.Fatalf("ApplyBackfillPlan on its own file: %v", err)
	}
	var cash float64
	if err := store.file(storageRolePaper).db.QueryRow("SELECT cash FROM strategies WHERE id = ?", "hl-paper").Scan(&cash); err != nil {
		t.Fatalf("read paper cash: %v", err)
	}
	if cash != 1234 {
		t.Errorf("paper cash = %.2f, want 1234", cash)
	}
	var liveCash float64
	if err := store.primary().db.QueryRow("SELECT cash FROM strategies WHERE id = ?", "hl-live").Scan(&liveCash); err != nil {
		t.Fatalf("read live cash: %v", err)
	}
	if liveCash != 1000 {
		t.Errorf("live cash = %.2f, want 1000 (the other file is untouched)", liveCash)
	}
}

func TestTradingViewExportCombinesBothFiles(t *testing.T) {
	cfg := splitTestConfig(t)
	store := openSplitStore(t, cfg)
	state := splitTestState(t)
	RecordTrade(state.Strategies["hl-live"], Trade{StrategyID: "hl-live", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2000, Timestamp: time.Unix(10, 0).UTC()})
	RecordTrade(state.Strategies["hl-paper"], Trade{StrategyID: "hl-paper", Symbol: "ETH", Side: "buy", Quantity: 1, Price: 2100, Timestamp: time.Unix(5, 0).UTC()})
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}

	rows, err := store.QueryTradingViewExportTrades([]string{"hl-live", "hl-paper"})
	if err != nil {
		t.Fatalf("QueryTradingViewExportTrades: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one from each file)", len(rows))
	}
	if !rows[0].Timestamp.Before(rows[1].Timestamp) {
		t.Errorf("rows are not in ascending timestamp order: %v then %v", rows[0].Timestamp, rows[1].Timestamp)
	}
	if rows[0].StrategyID != "hl-paper" || rows[1].StrategyID != "hl-live" {
		t.Errorf("order = %s, %s; want hl-paper then hl-live", rows[0].StrategyID, rows[1].StrategyID)
	}
}

func TestManualCommandRoutingAndLocking(t *testing.T) {
	cfg := splitTestConfig(t)
	cfg.Strategies[0].Type = "manual"
	cfg.Strategies[1].Type = "manual"
	store := openSplitStore(t, cfg)
	if err := SaveStateWithStore(splitTestState(t), store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}

	now := time.Now().UTC()
	for _, id := range []string{"hl-live", "hl-paper"} {
		if err := store.InsertPendingManualAction(PendingManualAction{
			StrategyID: id, Action: "close", Symbol: "ETH", Side: "sell",
			Quantity: 1, FillPrice: 2000, CreatedAt: now,
		}); err != nil {
			t.Fatalf("InsertPendingManualAction(%s): %v", id, err)
		}
	}

	// Each queued action lands in the file that owns its strategy.
	for _, tc := range []struct {
		role storageRole
		want string
	}{{storageRolePrimary, "hl-live"}, {storageRolePaper, "hl-paper"}} {
		rows, err := store.file(tc.role).LoadPendingManualActions()
		if err != nil {
			t.Fatalf("LoadPendingManualActions(%s): %v", tc.role, err)
		}
		if len(rows) != 1 || rows[0].StrategyID != tc.want {
			t.Fatalf("%s queue = %+v, want one row for %s", tc.role, rows, tc.want)
		}
		if rows[0].SourceRole != tc.role {
			t.Errorf("row source role = %s, want %s", rows[0].SourceRole, tc.role)
		}
	}

	// The combined read carries both, each tagged with its own file.
	all, err := store.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("combined LoadPendingManualActions: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("combined queue = %d rows, want 2", len(all))
	}

	// A manual command locks only the owning file, so the other stays free.
	unlock, err := store.manualActionLock("hl-paper")
	if err != nil {
		t.Fatalf("manualActionLock(hl-paper): %v", err)
	}
	defer unlock()
	liveUnlock, err := store.manualActionLock("hl-live")
	if err != nil {
		t.Fatalf("manualActionLock(hl-live) blocked by the paper lock: %v", err)
	}
	liveUnlock()

	if _, err := store.manualActionLock("not-configured"); err == nil {
		t.Error("manualActionLock accepted an unconfigured strategy")
	}
}

// A single-book save must acknowledge only that book's actions: another
// strategy's queued row still holds an effect that lives only in memory.
func TestSaveStrategyBookAcknowledgesOnlyItsOwnActions(t *testing.T) {
	cfg := splitTestConfig(t)
	cfg.Strategies = append(cfg.Strategies, StrategyConfig{
		ID: "hl-live-2", Type: "manual", Platform: "hyperliquid", Symbol: "BTC", Args: []string{"--mode=live"},
	})
	cfg.Strategies[0].Type = "manual"
	cfg.Strategies[1].Type = "manual"
	store := openSplitStore(t, cfg)

	state := splitTestState(t)
	state.Strategies["hl-live"].Type = "manual"
	state.Strategies["hl-paper"].Type = "manual"
	state.Strategies["hl-live-2"] = &StrategyState{
		ID: "hl-live-2", Type: "manual", Platform: "hyperliquid", Cash: 3000, InitialCapital: 3000,
		Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{},
	}
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}

	now := time.Now().UTC()
	for _, id := range []string{"hl-live", "hl-live-2"} {
		if err := store.InsertPendingManualAction(PendingManualAction{
			StrategyID: id, Action: "open", Symbol: "ETH", Side: "long",
			Quantity: 1, FillPrice: 2000, CreatedAt: now,
		}); err != nil {
			t.Fatalf("InsertPendingManualAction(%s): %v", id, err)
		}
	}
	rows, err := store.primary().LoadPendingManualActions()
	if err != nil || len(rows) != 2 {
		t.Fatalf("queued rows = %d (err=%v), want 2", len(rows), err)
	}
	for _, r := range rows {
		store.recordAppliedManualAction(r.StrategyID, storageRolePrimary, r.ID)
	}

	// Persist only the first strategy's book.
	if err := store.SaveStrategyBook(state.Strategies["hl-live"]); err != nil {
		t.Fatalf("SaveStrategyBook: %v", err)
	}
	after, err := store.primary().LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	if len(after) != 1 || after[0].StrategyID != "hl-live-2" {
		t.Fatalf("remaining rows = %+v, want only hl-live-2 (its effect is still unsaved)", after)
	}
}

func TestStateStoreEagerDiagnosticsInsertUsesStoredIdentifier(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"), Strategies: []StrategyConfig{
		{ID: "hl-eth-momentum", StorageStrategyID: "hl-perps-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live"}},
	}}
	store := openSplitStore(t, cfg)
	defer store.Close()

	row := &TradeDiagnosticsRow{StrategyID: "hl-eth-momentum", PositionID: "p1", Symbol: "ETH", Side: "long", CloseReason: "signal", EntryPrice: 1800, ExitPrice: 1900, Quantity: 1, RealizedPnL: 100, ClosedAt: time.Unix(10, 0).UTC()}
	if err := store.InsertTradeDiagnostics(row); err != nil {
		t.Fatalf("InsertTradeDiagnostics: %v", err)
	}
	var stored string
	if err := store.primary().db.QueryRow("SELECT strategy_id FROM trade_diagnostics").Scan(&stored); err != nil {
		t.Fatalf("read stored strategy_id: %v", err)
	}
	if stored != "hl-perps-eth" {
		t.Errorf("stored strategy_id = %q, want the storage identifier %q", stored, "hl-perps-eth")
	}
	rows, err := store.TradeDiagnosticsRows("hl-eth-momentum")
	if err != nil {
		t.Fatalf("TradeDiagnosticsRows: %v", err)
	}
	if len(rows) != 1 || rows[0].StrategyID != "hl-eth-momentum" {
		t.Fatalf("rows = %+v, want one row read back under the process identifier", rows)
	}
	// The fill-reconciled correction filters on the stored identifier, so it
	// must hit the eagerly inserted row.
	res, err := store.primary().db.Exec("UPDATE trade_diagnostics SET exit_price = 1950, realized_pnl = 150 WHERE strategy_id = ? AND position_id = ?", "hl-perps-eth", "p1")
	if err != nil {
		t.Fatalf("correction update: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("correction affected %d rows, want 1", n)
	}
	rows, _ = store.TradeDiagnosticsRows("hl-eth-momentum")
	if len(rows) != 1 || rows[0].ExitPrice != 1950 || rows[0].RealizedPnL != 150 {
		t.Errorf("after correction rows = %+v, want exit 1950 / pnl 150 on the eagerly inserted row", rows)
	}
}

func TestLoadStateWithStoreReportsMissingPrimaryFile(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"), Strategies: []StrategyConfig{
		{ID: "hl-paper", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=paper"}},
	}}
	store := openSplitStore(t, cfg)
	if err := SaveStateWithStore(NewAppState(), store); err != nil {
		t.Fatalf("SaveStateWithStore: %v", err)
	}
	store.Close()
	if err := os.Remove(cfg.DBFile); err != nil {
		t.Fatalf("remove primary: %v", err)
	}

	ro, err := openToolStateStoreReadOnly(cfg)
	if err != nil {
		t.Fatalf("openToolStateStoreReadOnly: %v", err)
	}
	defer ro.Close()
	if _, _, err := LoadStateWithStore(cfg, ro); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("LoadStateWithStore err = %v, want a missing primary file error, never a panic", err)
	}
}

func TestStorageRefusalDMNamesEveryRejection(t *testing.T) {
	sendStartupRefusalDM(nil, "Storage layout", "unused")
	body := storageRefusalDMBody(storageInspection{Rejections: []string{"first rejection", "second rejection"}})
	for _, want := range []string{"first rejection", "second rejection", "exit 80"} {
		if !strings.Contains(body, want) {
			t.Errorf("DM body %q lacks %q", body, want)
		}
	}
}
