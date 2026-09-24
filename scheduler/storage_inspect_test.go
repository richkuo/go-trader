package main

import (
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return string(sum[:])
}

// rollbackJournalPresent reports a -journal sidecar, which SQLite writes only
// for a write transaction. The -wal / -shm pair is the read path's shared-memory
// index: a read-only connection to a WAL database must be able to create it, and
// cannot delete it on close, so its presence is not a write.
func rollbackJournalPresent(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path + "-journal")
	return err == nil
}

func inspectionFor(t *testing.T, cfg *Config, requireIdle bool) storageInspection {
	t.Helper()
	si, err := inspectStorageLayoutForConfig(cfg, requireIdle)
	if err != nil {
		t.Fatalf("inspectStorageLayoutForConfig: %v", err)
	}
	return si
}

func rejectionsContain(si storageInspection, want string) bool {
	for _, r := range si.Rejections {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

func TestInspectStorageOwnership(t *testing.T) {
	t.Run("a clean split layout is accepted", func(t *testing.T) {
		cfg := splitTestConfig(t)
		store := openSplitStore(t, cfg)
		if err := SaveStateWithStore(splitTestState(t), store); err != nil {
			t.Fatalf("SaveStateWithStore: %v", err)
		}
		store.Close()

		si := inspectionFor(t, cfg, false)
		if !si.OK() {
			t.Fatalf("rejections = %v, want none", si.Rejections)
		}
		if len(si.Files) != 2 {
			t.Fatalf("files = %d, want 2", len(si.Files))
		}
		for _, fi := range si.Files {
			if !fi.Present || len(fi.Strategies) != 1 {
				t.Errorf("%s file = present %v with %d mapped strategy rows, want present with 1", fi.Role, fi.Present, len(fi.Strategies))
			}
		}
	})

	t.Run("a book stored in the wrong file is rejected by name", func(t *testing.T) {
		cfg := splitTestConfig(t)
		store := openSplitStore(t, cfg)
		if err := SaveStateWithStore(splitTestState(t), store); err != nil {
			t.Fatalf("SaveStateWithStore: %v", err)
		}
		// Plant the paper strategy's stored row in the primary file.
		if _, err := store.primary().db.Exec(`INSERT INTO strategies (id, type, platform) VALUES ('hl-paper', 'perps', 'hyperliquid')`); err != nil {
			t.Fatalf("plant cross-scope row: %v", err)
		}
		store.Close()

		si := inspectionFor(t, cfg, false)
		if !rejectionsContain(si, `stored strategy "hl-paper" is mapped to the paper state file`) {
			t.Fatalf("rejections = %v, want one naming hl-paper and the paper file", si.Rejections)
		}
	})

	t.Run("a risk row whose scope the file does not own is rejected", func(t *testing.T) {
		cfg := splitTestConfig(t)
		store := openSplitStore(t, cfg)
		if err := SaveStateWithStore(splitTestState(t), store); err != nil {
			t.Fatalf("SaveStateWithStore: %v", err)
		}
		if _, err := store.primary().db.Exec(`INSERT OR REPLACE INTO portfolio_risk (scope, peak_value) VALUES ('paper', 42)`); err != nil {
			t.Fatalf("plant cross-scope risk row: %v", err)
		}
		store.Close()

		si := inspectionFor(t, cfg, false)
		if !rejectionsContain(si, "portfolio_risk holds a paper-scope row") {
			t.Fatalf("rejections = %v, want one naming the paper-scope row in the primary file", si.Rejections)
		}
	})

	t.Run("an ambiguous legacy row in a split layout with no live strategy is rejected", func(t *testing.T) {
		cfg := splitTestConfig(t)
		cfg.Strategies = cfg.Strategies[1:] // paper only
		store := openSplitStore(t, cfg)
		if _, err := store.primary().db.Exec(`INSERT INTO portfolio_risk (scope, peak_value) VALUES ('', 500)`); err != nil {
			t.Fatalf("plant legacy row: %v", err)
		}
		store.Close()

		si := inspectionFor(t, cfg, false)
		if !rejectionsContain(si, "resolve it by hand") {
			t.Fatalf("rejections = %v, want a refusal to place the ambiguous legacy row", si.Rejections)
		}
	})

	t.Run("an orphan holding positions is reported with its count", func(t *testing.T) {
		cfg := splitTestConfig(t)
		store := openSplitStore(t, cfg)
		state := splitTestState(t)
		state.Strategies["hl-paper"].Positions["BTC"] = &Position{Symbol: "BTC", Side: "long", Quantity: 1, AvgCost: 60000}
		if err := SaveStateWithStore(state, store); err != nil {
			t.Fatalf("SaveStateWithStore: %v", err)
		}
		store.Close()

		trimmed := &Config{DBFile: cfg.DBFile, PaperDBFile: cfg.PaperDBFile, Strategies: cfg.Strategies[:1]}
		si := inspectionFor(t, trimmed, false)
		if !si.OK() {
			t.Fatalf("rejections = %v, want none (an orphan is reported, not rejected)", si.Rejections)
		}
		var found bool
		for _, fi := range si.Files {
			for _, orphan := range fi.Orphans {
				if orphan.StorageID == "hl-paper" {
					found = true
					if orphan.PositionCount != 1 {
						t.Errorf("orphan position count = %d, want 1", orphan.PositionCount)
					}
				}
			}
		}
		if !found {
			t.Fatalf("orphan hl-paper not reported; files = %+v", si.Files)
		}
	})

	t.Run("a pre-scope schema is tolerated and its rows read as legacy", func(t *testing.T) {
		dir := t.TempDir()
		livePath := filepath.Join(dir, "legacy.db")
		raw, err := sql.Open("sqlite", livePath)
		if err != nil {
			t.Fatalf("open raw: %v", err)
		}
		if _, err := raw.Exec(`
			CREATE TABLE strategies (id TEXT PRIMARY KEY, type TEXT NOT NULL DEFAULT '', platform TEXT NOT NULL DEFAULT '');
			CREATE TABLE positions (strategy_id TEXT NOT NULL, symbol TEXT NOT NULL);
			CREATE TABLE portfolio_risk (id INTEGER PRIMARY KEY CHECK (id = 1), peak_value REAL NOT NULL DEFAULT 0, kill_switch_active INTEGER NOT NULL DEFAULT 0);
			INSERT INTO strategies (id) VALUES ('hl-live');
			INSERT INTO portfolio_risk (id, peak_value, kill_switch_active) VALUES (1, 900, 1);
		`); err != nil {
			t.Fatalf("seed legacy schema: %v", err)
		}
		raw.Close()

		cfg := &Config{DBFile: livePath, Strategies: []StrategyConfig{
			{ID: "hl-live", Type: "perps", Platform: "hyperliquid", Args: []string{"--mode=live"}},
		}}
		si := inspectionFor(t, cfg, false)
		if !si.OK() {
			t.Fatalf("rejections = %v, want none on a pre-scope schema", si.Rejections)
		}
		if len(si.Files) != 1 || !si.Files[0].LegacyUnscoped {
			t.Fatalf("files = %+v, want the legacy unscoped row reported", si.Files)
		}
		if len(si.Files[0].Strategies) != 1 || si.Files[0].Strategies[0].ProcessID != "hl-live" {
			t.Errorf("strategies = %+v, want hl-live mapped", si.Files[0].Strategies)
		}
	})

	t.Run("inspection changes no database", func(t *testing.T) {
		cfg := splitTestConfig(t)
		store := openSplitStore(t, cfg)
		if err := SaveStateWithStore(splitTestState(t), store); err != nil {
			t.Fatalf("SaveStateWithStore: %v", err)
		}
		store.Close()

		before := map[string]string{}
		for _, path := range []string{cfg.DBFile, cfg.PaperDBFile} {
			before[path] = fileDigest(t, path)
			if rollbackJournalPresent(t, path) {
				t.Fatalf("%s already carries a rollback journal before the inspection", path)
			}
		}

		if si := inspectionFor(t, cfg, false); !si.OK() {
			t.Fatalf("rejections = %v, want none", si.Rejections)
		}

		for _, path := range []string{cfg.DBFile, cfg.PaperDBFile} {
			if got := fileDigest(t, path); got != before[path] {
				t.Errorf("%s changed during a read-only inspection", path)
			}
			if rollbackJournalPresent(t, path) {
				t.Errorf("%s carries a rollback journal after the inspection — a write transaction ran", path)
			}
		}

		// The content must read back identically too, so no migration ran.
		store2 := openSplitStore(t, cfg)
		reloaded, _, err := LoadStateWithStore(cfg, store2)
		if err != nil {
			t.Fatalf("LoadStateWithStore after inspection: %v", err)
		}
		if len(reloaded.Strategies) != 2 {
			t.Errorf("roster after inspection = %d strategies, want 2", len(reloaded.Strategies))
		}
	})

	t.Run("require-idle turns a held lock into a rejection", func(t *testing.T) {
		cfg := splitTestConfig(t)
		store := openSplitStore(t, cfg)
		if err := SaveStateWithStore(splitTestState(t), store); err != nil {
			t.Fatalf("SaveStateWithStore: %v", err)
		}
		store.Close()

		layout, err := resolveStorageLayout(cfg)
		if err != nil {
			t.Fatalf("resolveStorageLayout: %v", err)
		}
		owned, err := acquireStateOwnership(layout.Files)
		if err != nil {
			t.Fatalf("acquireStateOwnership: %v", err)
		}
		defer owned.Release()

		if si := inspectionFor(t, cfg, false); !si.OK() {
			t.Fatalf("rejections = %v, want none without --require-idle", si.Rejections)
		}
		si := inspectionFor(t, cfg, true)
		if si.OK() {
			t.Fatal("--require-idle accepted a layout whose files are owned by a running process")
		}
		if !rejectionsContain(si, "stop the scheduler before inspecting for idleness") {
			t.Errorf("rejections = %v, want the idle refusal", si.Rejections)
		}
	})

	t.Run("an aliased layout is rejected before any file is opened", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.db")
		si := inspectionFor(t, &Config{DBFile: path, PaperDBFile: path}, false)
		if si.OK() || !rejectionsContain(si, "same physical file") {
			t.Fatalf("rejections = %v, want the alias refusal", si.Rejections)
		}
	})
}
