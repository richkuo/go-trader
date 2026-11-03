package main

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOptionMarkStorageIntegration(t *testing.T) {
	markAt := time.Date(2026, 3, 2, 15, 4, 5, 123456789, time.UTC)
	cutoff := time.Date(2026, 3, 20, 20, 0, 0, 0, time.UTC)
	seed := func(s *StrategyState) {
		s.Cash = 5000
		s.OptionPositions["leg"] = &OptionPosition{ID: "leg", TradePositionID: "pos-leg", Underlying: "SPY", OptionType: "put", Strike: 470,
			Expiry: "2026-03-20", DTE: 18, Action: "sell", Quantity: 2, ContractMultiplier: 100, EntryPremiumUSD: 300, CurrentValueUSD: -240,
			OpenedAt: markAt.Add(-time.Hour), MarkUpdatedAt: markAt, MarkError: "settlement pending: no daily bar", MarkSource: "robinhood_quote",
			ExpiryCutoffAt: cutoff, SettlementReadyAt: cutoff.Add(time.Hour), markVerifiedAt: markAt}
	}
	check := func(t *testing.T, label string, s *StrategyState) {
		t.Helper()
		leg := s.OptionPositions["leg"]
		if leg == nil {
			t.Fatalf("%s: leg missing", label)
		}
		if !leg.MarkUpdatedAt.Equal(markAt) || leg.MarkError != "settlement pending: no daily bar" || leg.MarkSource != "robinhood_quote" ||
			!leg.ExpiryCutoffAt.Equal(cutoff) || !leg.SettlementReadyAt.Equal(cutoff.Add(time.Hour)) {
			t.Fatalf("%s: leg = %+v", label, leg)
		}
		if !leg.markVerifiedAt.IsZero() {
			t.Fatalf("%s: harvest eligibility must never load from disk", label)
		}
	}

	dir := t.TempDir()
	def := ocSC("rh-store-default", "robinhood", "SPY")
	src := ocSC("rh-store-source", "robinhood", "SPY")
	src.PaperSource = "src"
	src.StorageStrategyID = "rh-store-alias"
	cfg := &Config{DBFile: filepath.Join(dir, "state.db"), IntervalSeconds: 600,
		PaperSources: []PaperSourceConfig{{ID: "src", DBFile: filepath.Join(dir, "src.db")}}, Strategies: []StrategyConfig{def, src}}

	t.Run("both_save_paths_round_trip_in_each_partition", func(t *testing.T) {
		env := openEntryTestEnv(t, cfg)
		for _, sc := range []StrategyConfig{def, src} {
			seed(env.strat(sc.ID))
		}
		env.saveAll()
		env = env.restart()
		for _, sc := range []StrategyConfig{def, src} {
			check(t, "SaveAll "+sc.ID, env.strat(sc.ID))
		}
		for _, sc := range []StrategyConfig{def, src} {
			s := env.strat(sc.ID)
			s.OptionPositions["leg"].MarkSource = "robinhood_black_scholes"
			if err := env.store.SaveStrategyBook(s); err != nil {
				t.Fatalf("book save %s: %v", sc.ID, err)
			}
		}
		env = env.restart()
		for _, sc := range []StrategyConfig{def, src} {
			leg := env.strat(sc.ID).OptionPositions["leg"]
			if leg.MarkSource != "robinhood_black_scholes" || !leg.ExpiryCutoffAt.Equal(cutoff) || !leg.MarkUpdatedAt.Equal(markAt) {
				t.Fatalf("SaveStrategyBook %s: leg = %+v", sc.ID, leg)
			}
		}
		var stored string
		if err := env.fileFor(src.ID).db.QueryRow(`SELECT expiry_cutoff_at FROM option_positions WHERE strategy_id = 'rh-store-alias' AND id = 'leg'`).Scan(&stored); err != nil || stored != formatTime(cutoff) {
			t.Fatalf("aliased row cutoff = %q, %v", stored, err)
		}
	})

	t.Run("old_files_load_read_only_and_migrate_once", func(t *testing.T) {
		oldDir := t.TempDir()
		oldCfg := &Config{DBFile: filepath.Join(oldDir, "state.db"), IntervalSeconds: 600, Strategies: []StrategyConfig{def}}
		env := openEntryTestEnv(t, oldCfg)
		seed(env.strat(def.ID))
		env.saveAll()
		env.store.Close()
		raw, err := OpenStateDB(oldCfg.DBFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, col := range []string{"mark_updated_at", "mark_error", "mark_source", "expiry_cutoff_at", "settlement_ready_at"} {
			if _, err := raw.db.Exec(`ALTER TABLE option_positions DROP COLUMN ` + col); err != nil {
				t.Fatalf("drop %s to build an old file: %v", col, err)
			}
		}
		raw.Close()
		sum := func() [32]byte {
			b, err := os.ReadFile(oldCfg.DBFile)
			if err != nil {
				t.Fatal(err)
			}
			return sha256.Sum256(b)
		}
		before := sum()
		layout, err := resolveStorageLayout(oldCfg)
		if err != nil {
			t.Fatal(err)
		}
		ident, err := buildStorageIdentityMap(oldCfg, layout)
		if err != nil {
			t.Fatal(err)
		}
		ro, err := OpenStateStoreReadOnly(layout, ident)
		if err != nil {
			t.Fatalf("read-only open: %v", err)
		}
		roState, _, err := LoadStateWithStore(oldCfg, ro)
		if err != nil {
			t.Fatalf("read-only load of an old file: %v", err)
		}
		leg := roState.Strategies[def.ID].OptionPositions["leg"]
		if leg == nil || !leg.MarkUpdatedAt.IsZero() || leg.MarkError != "" || !leg.ExpiryCutoffAt.IsZero() || leg.CurrentValueUSD != -240 {
			t.Fatalf("old row must load with empty mark fields: %+v", leg)
		}
		ro.Close()
		if sum() != before {
			t.Fatal("a read-only load changed the old state file")
		}

		env = openEntryTestEnv(t, oldCfg)
		for _, col := range []string{"mark_updated_at", "mark_error", "mark_source", "expiry_cutoff_at", "settlement_ready_at"} {
			if has, err := env.fileFor(def.ID).tableHasColumn("option_positions", col); err != nil || !has {
				t.Fatalf("writable open must add %s: %v %v", col, has, err)
			}
		}
		env.store.Close()
		migrated := sum()
		env = openEntryTestEnv(t, oldCfg)
		env.store.Close()
		if sum() != migrated {
			t.Fatal("a second writable open changed the migrated file")
		}
	})
}
