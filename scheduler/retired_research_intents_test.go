package main

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const retiredResearchTag = "archive/basket-pair-research"

func retiredResearchOpenStore(t *testing.T, cfg *Config) *StateStore {
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
	return store
}

func retiredResearchLoad(t *testing.T, cfg *Config, store *StateStore) *AppState {
	t.Helper()
	state, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		t.Fatalf("LoadStateWithStore: %v", err)
	}
	configIDs := map[string]bool{}
	for _, sc := range cfg.Strategies {
		configIDs[sc.ID] = true
	}
	persisted := map[string]bool{}
	for id := range state.Strategies {
		if !configIDs[id] {
			delete(state.Strategies, id)
			continue
		}
		persisted[id] = true
	}
	for _, sc := range cfg.Strategies {
		if state.Strategies[sc.ID] == nil {
			state.Strategies[sc.ID] = NewStrategyState(sc)
		}
	}
	if err := store.LoadEntryGuards(state, cfg.Strategies, persisted); err != nil {
		t.Fatalf("LoadEntryGuards: %v", err)
	}
	return state
}

func retiredResearchIntentRows(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return retiredResearchRows(t, db, `SELECT * FROM entry_order_intents ORDER BY intent_id`)
}

func retiredResearchCaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	func() {
		defer func() { os.Stdout = prev }()
		fn()
	}()
	w.Close()
	return string(<-done)
}

func TestRetiredResearchIntentsFailClosed(t *testing.T) {
	t.Run("a fresh file creates no retired tables and the base entry intent DDL", func(t *testing.T) {
		oldPath := filepath.Join(t.TempDir(), "old.db")
		loadRetiredResearchDump(t, oldPath)
		freshPath := filepath.Join(t.TempDir(), "fresh.db")
		store := retiredResearchOpenStore(t, retiredResearchOrdinaryConfig(freshPath))
		store.Close()
		ddl := func(path string) ([]string, string) {
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			retired := retiredResearchRows(t, db, `SELECT name FROM sqlite_master WHERE tbl_name LIKE 'basket\_%' ESCAPE '\' OR tbl_name LIKE 'pair\_%' ESCAPE '\' ORDER BY name`)
			var text string
			if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'entry_order_intents'`).Scan(&text); err != nil {
				t.Fatal(err)
			}
			return retired, text
		}
		freshRetired, freshDDL := ddl(freshPath)
		oldRetired, oldDDL := ddl(oldPath)
		if len(freshRetired) != 0 {
			t.Fatalf("a fresh file created retired research objects: %v", freshRetired)
		}
		if len(oldRetired) == 0 {
			t.Fatal("the fixture lost its retired research objects")
		}
		if freshDDL != oldDDL {
			t.Fatalf("the entry intent table text changed:\nfresh=%s\nbase=%s", freshDDL, oldDDL)
		}
		db, err := sql.Open("sqlite", freshPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var col int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('portfolio_risk') WHERE name = 'pair_valuation_incomplete_since'`).Scan(&col); err != nil || col != 1 {
			t.Fatalf("a fresh file must keep the portfolio_risk pair valuation column: %d %v", col, err)
		}
	})

	t.Run("a reused former basket member id holds its retired intent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.db")
		loadRetiredResearchDump(t, path)
		before := retiredResearchIntentRows(t, path)
		cfg := &Config{DBFile: path, Strategies: []StrategyConfig{
			{ID: "hl-bsk-btc", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"sma_crossover", "BTC", "1h", "--mode=paper"}, Capital: 1000, MaxDrawdownPct: 20, Leverage: 2},
		}}
		store := retiredResearchOpenStore(t, cfg)
		state := retiredResearchLoad(t, cfg, store)
		s := state.Strategies["hl-bsk-btc"]
		held := s.EntryGuard.unresolvedIntent()
		if held == nil || held.IntentID != "ei-basket-retired-fixture" || !held.researchGroupOwned() {
			store.Close()
			t.Fatalf("the retired basket intent must load as an unresolved research-owned intent: %+v", held)
		}
		lines := strings.Join(entryGuardStartupLines(state), "\n")
		if !strings.Contains(lines, "ei-basket-retired-fixture") || !strings.Contains(lines, retiredResearchTag) {
			store.Close()
			t.Fatalf("the startup line must name the retired intent and the archive tag: %s", lines)
		}
		next := &entryIntent{
			IntentID: "ei-0123456789abcdef01234567", Partition: defaultPaperPartition, StrategyID: s.ID, Symbol: "BTC",
			IntervalMs: 3600000, TargetSide: entrySideLong, BarOpenMs: held.BarOpenMs + 3600000*24, DecisionBoundaryMs: held.DecisionBoundaryMs + 3600000*24,
			CutoffMs: held.CutoffMs + 3600000*24, Provider: "hyperliquid", InstrumentType: "perps", ClientOrderID: "hyperliquid-fresh-attempt",
			Status: entryIntentReserved, CreatedAtMs: 1, UpdatedAtMs: 1,
		}
		if err := store.ReserveEntryIntent(s, next); err == nil {
			store.Close()
			t.Fatal("a new reservation must be blocked while the retired intent is unresolved")
		}
		for part, err := range store.SaveAll(state) {
			if err != nil {
				store.Close()
				t.Fatalf("SaveAll %s: %v", part, err)
			}
		}
		store.Close()
		if after := retiredResearchIntentRows(t, path); !reflect.DeepEqual(before, after) {
			t.Fatalf("the retired intents changed:\nbefore=%q\nafter=%q", before, after)
		}
	})

	t.Run("recovery and resolve-entry refuse a retired live intent", func(t *testing.T) {
		t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0x0000000000000000000000000000000000000001")
		path := filepath.Join(t.TempDir(), "state.db")
		sc := StrategyConfig{ID: "hl-live-reused", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"sma_crossover", "BTC", "1h", "--mode=live"}, Capital: 1000, MaxDrawdownPct: 20, Leverage: 2}
		cfg := &Config{DBFile: path, Strategies: []StrategyConfig{sc}}
		store := retiredResearchOpenStore(t, cfg)
		defer store.Close()
		const intentID = "ei-00112233445566778899aabb"
		if _, err := store.primary().db.Exec(`INSERT INTO entry_order_intents (intent_id, scope, source, strategy_id, symbol, interval_ms, target_side,
			bar_open_ms, decision_boundary_ms, cutoff_ms, provider, account_key, instrument_type, client_order_id, status, request_json, created_at_ms, updated_at_ms)
			VALUES (?, 'live', '', ?, 'BTC', 3600000, 'long', 3600000, 7200000, 7500000, 'hyperliquid', '', 'perps', '0x000000000000000000000000000000aa', 'submitting', ?, 1, 1)`,
			intentID, sc.ID, `{"version":1,"kind":"basket:ba-retired","signal":1,"size":0.01}`); err != nil {
			t.Fatal(err)
		}
		before := retiredResearchIntentRows(t, path)
		state := retiredResearchLoad(t, cfg, store)
		var mu sync.RWMutex
		out := retiredResearchCaptureStdout(t, func() {
			recoverEntryIntents(cfg, state, store, &mu, nil, nil, time.Now())
		})
		if !strings.Contains(out, "recovery refused") || !strings.Contains(out, intentID) || !strings.Contains(out, retiredResearchTag) {
			t.Fatalf("recovery must refuse the retired intent with the archive tag: %q", out)
		}
		deps := entryOperatorDeps{state: state, mu: &mu, store: store, strategy: func(id string) (StrategyConfig, bool) { return sc, id == sc.ID }}
		outcome := resolveEntryAttemptByOperator(deps, "operator", sc.ID, intentID)
		if outcome.Released || !strings.HasPrefix(outcome.Message, "Refused:") || !strings.Contains(outcome.Message, retiredResearchTag) || !strings.Contains(outcome.Message, "fresh id") {
			t.Fatalf("resolve-entry must refuse the retired intent with the archive tag: %+v", outcome)
		}
		if after := retiredResearchIntentRows(t, path); !bytes.Equal([]byte(strings.Join(before, "\n")), []byte(strings.Join(after, "\n"))) {
			t.Fatalf("a refusing route changed the retired intent:\nbefore=%q\nafter=%q", before, after)
		}
	})
}
