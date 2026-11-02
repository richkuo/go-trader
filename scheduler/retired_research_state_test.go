package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const retiredResearchStateDump = "testdata/retired_research_state/state.sql"

func loadRetiredResearchDump(t *testing.T, path string) {
	t.Helper()
	dump, err := os.ReadFile(filepath.FromSlash(retiredResearchStateDump))
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(string(dump)); err != nil {
		t.Fatalf("load dump: %v", err)
	}
}

func retiredResearchRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var out []string
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts[i] = fmt.Sprintf("%s=%T:%v", cols[i], v, v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func retiredResearchSnapshot(t *testing.T, path string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	snap := map[string][]string{}
	retired := `(tbl_name LIKE 'basket\_%' ESCAPE '\' OR tbl_name LIKE 'pair\_%' ESCAPE '\')`
	snap["sqlite_master"] = retiredResearchRows(t, db, `SELECT type, name, tbl_name, sql FROM sqlite_master WHERE `+retired+` ORDER BY type, name`)
	for _, row := range retiredResearchRows(t, db, `SELECT name FROM sqlite_master WHERE type = 'table' AND `+retired+` ORDER BY name`) {
		name := strings.TrimPrefix(row, "name=string:")
		snap["table:"+name] = retiredResearchRows(t, db, `SELECT * FROM "`+name+`" ORDER BY rowid`)
	}
	snap["entry_order_intents"] = retiredResearchRows(t, db, `SELECT * FROM entry_order_intents ORDER BY intent_id`)
	snap["portfolio_risk.pair_valuation_incomplete_since"] = retiredResearchRows(t, db, `SELECT scope, pair_valuation_incomplete_since FROM portfolio_risk ORDER BY scope`)
	return snap
}

func retiredResearchOrdinaryConfig(path string) *Config {
	return &Config{
		DBFile: path,
		Strategies: []StrategyConfig{
			{ID: "ordinary-hl-btc", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"sma_crossover", "BTC", "1h", "--mode=paper"}, Capital: 1000, MaxDrawdownPct: 20, Leverage: 2},
			{ID: "ordinary-okx-eth", Platform: "okx", Type: "perps", Script: "shared_scripts/check_okx.py", Args: []string{"sma_crossover", "ETH", "1h", "--mode=paper"}, Capital: 1000, MaxDrawdownPct: 20, Leverage: 2},
		},
	}
}

func openLoadSaveRetiredResearchFile(t *testing.T, cfg *Config) {
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
	state, _, err := LoadStateWithStore(cfg, store)
	if err != nil {
		store.Close()
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
		store.Close()
		t.Fatalf("LoadEntryGuards: %v", err)
	}
	for part, err := range store.SaveAll(state) {
		if err != nil {
			store.Close()
			t.Fatalf("SaveAll %s: %v", part, err)
		}
	}
	store.Close()
}

func TestRetiredResearchStateSurvivesLoadAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	loadRetiredResearchDump(t, path)
	before := retiredResearchSnapshot(t, path)
	for _, key := range []string{"table:basket_contracts", "table:basket_rebalances", "table:basket_member_actions", "table:basket_member_state", "table:pair_quote_event_uses", "table:pair_funding_inventory", "entry_order_intents", "portfolio_risk.pair_valuation_incomplete_since"} {
		if len(before[key]) == 0 {
			t.Fatalf("the fixture has no %s rows; recapture it from the base commit", key)
		}
	}
	if !strings.Contains(strings.Join(before["portfolio_risk.pair_valuation_incomplete_since"], "\n"), "2026-09-30T08:15:00Z") {
		t.Fatalf("the fixture lost its stored pair valuation time: %v", before["portfolio_risk.pair_valuation_incomplete_since"])
	}

	openLoadSaveRetiredResearchFile(t, retiredResearchOrdinaryConfig(path))

	after := retiredResearchSnapshot(t, path)
	if !reflect.DeepEqual(before, after) {
		for key, rows := range before {
			if !reflect.DeepEqual(rows, after[key]) {
				t.Errorf("%s changed across load and save:\nbefore=%q\nafter=%q", key, rows, after[key])
			}
		}
		for key := range after {
			if _, ok := before[key]; !ok {
				t.Errorf("%s appeared across load and save: %q", key, after[key])
			}
		}
		t.FailNow()
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
}
