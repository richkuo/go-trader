package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const rhBindVenue = "acct-1"

type rhBindRow struct {
	Key, Venue, Source string
}

type rhBindFixture struct {
	t   *testing.T
	dir string
	cfg *Config
	key string
}

func rhBindLive(id, coin, storage string) StrategyConfig {
	return StrategyConfig{ID: id, Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
		Args: []string{"momentum_pro", coin, "1h", "--mode=live"}, Capital: 1000, InitialCapital: 1000, MaxDrawdownPct: 50, StorageStrategyID: storage}
}

func newRHBindFixture(t *testing.T, strategies ...StrategyConfig) *rhBindFixture {
	t.Helper()
	t.Setenv("ROBINHOOD_USERNAME", "offline-user")
	dir := t.TempDir()
	cfg := &Config{DBFile: filepath.Join(dir, "live.db"), PaperDBFile: filepath.Join(dir, "paper.db"), Strategies: strategies}
	return &rhBindFixture{t: t, dir: dir, cfg: cfg, key: robinhoodAccountKey()}
}

func (f *rhBindFixture) open() *StateStore {
	return rhIdentityOpen(f.t, f.cfg)
}

func (f *rhBindFixture) seed(mutate func(state *AppState, store *StateStore)) {
	f.t.Helper()
	store := f.open()
	defer store.Close()
	state := NewAppState()
	for _, sc := range f.cfg.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	mutate(state, store)
	if err := SaveStateWithStore(state, store); err != nil {
		f.t.Fatalf("seed save: %v", err)
	}
}

func rhBindOpenBook(s *StrategyState, coin, pid, oid, tradeSymbol string) {
	s.Positions[coin] = &Position{Symbol: coin, Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 100, Side: "long", OwnerStrategyID: s.ID, TradePositionID: pid, OpenedAt: time.Unix(1700000000, 0).UTC()}
	RecordTrade(s, Trade{StrategyID: s.ID, Symbol: tradeSymbol, PositionID: pid, Side: "buy", Quantity: 0.5, Price: 100, Value: 50, ExchangeOrderID: oid, Timestamp: time.Unix(1700000000, 0).UTC()})
}

func (f *rhBindFixture) attempt(store *StateStore, procID, id, symbol, oid, key, venue string) {
	f.t.Helper()
	db, err := store.dbForStrategy(procID)
	if err != nil {
		f.t.Fatal(err)
	}
	sid, err := db.robinhoodAttemptOwner(procID)
	if err != nil {
		f.t.Fatal(err)
	}
	a := &robinhoodOrderAttempt{AttemptID: id, AccountKey: key, Symbol: symbol, Origin: robinhoodOriginSignal, Side: "buy", RefID: "ref-" + id, VenueOrderID: oid, VenueAccountID: venue,
		Status: robinhoodAttemptSettled, RequestJSON: `{}`, ConfirmedQty: 0.5, AppliedQty: 0.5, SettlementSeq: 1, CreatedAtMs: 1000, UpdatedAtMs: 1000}
	if err := upsertRobinhoodAttemptRow(db.db, sid, a); err != nil {
		f.t.Fatalf("seed attempt: %v", err)
	}
}

func (f *rhBindFixture) rows(path string) map[string]rhBindRow {
	f.t.Helper()
	db, err := openStateDBReadOnly(path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.db.Query(`SELECT strategy_id, symbol, account_key, venue_account_id, account_binding_source FROM positions ORDER BY strategy_id, symbol`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]rhBindRow{}
	for rows.Next() {
		var sid, sym string
		var r rhBindRow
		if err := rows.Scan(&sid, &sym, &r.Key, &r.Venue, &r.Source); err != nil {
			f.t.Fatal(err)
		}
		out[sid+"/"+sym] = r
	}
	return out
}

func (f *rhBindFixture) startup() (*AppState, []string, error) {
	f.t.Helper()
	store := f.open()
	defer store.Close()
	state, _, _, lines, err := prepareStartupStateReport(f.cfg, store, true)
	return state, lines, err
}

func TestRobinhoodAccountBindingPersistence(t *testing.T) {
	live := rhBindLive("rh-live", "BTC", "rh-live-store")
	paper := StrategyConfig{ID: "rh-paper", Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
		Args: []string{"momentum_pro", "BTC", "1h", "--mode=paper"}, Capital: 1000, InitialCapital: 1000, MaxDrawdownPct: 50}
	f := newRHBindFixture(t, live, paper)
	f.seed(func(state *AppState, _ *StateStore) {
		rhBindOpenBook(state.Strategies["rh-live"], "BTC", "pid-live", "", "BTC")
		rhBindOpenBook(state.Strategies["rh-paper"], "BTC", "pid-paper", "", "BTC")
	})

	db, err := openStateDB(f.cfg.DBFile, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"account_key", "venue_account_id", "account_binding_source"} {
		if _, err := db.db.Exec("ALTER TABLE positions DROP COLUMN " + col); err != nil {
			t.Fatalf("drop %s to build the old schema: %v", col, err)
		}
	}
	db.Close()

	for pass := 1; pass <= 2; pass++ {
		store := f.open()
		state, _, err := LoadStateWithStore(f.cfg, store)
		if err != nil {
			t.Fatalf("migration pass %d: %v", pass, err)
		}
		if pos := state.Strategies["rh-live"].Positions["BTC"]; pos == nil || pos.AccountKey != "" || pos.VenueAccountID != "" {
			t.Fatalf("pass %d: migrated book = %+v, want empty binding defaults", pass, pos)
		}
		store.Close()
	}

	store := f.open()
	state, _, err := LoadStateWithStore(f.cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	s := state.Strategies["rh-live"]
	s.Positions["BTC"].AccountKey, s.Positions["BTC"].VenueAccountID, s.Positions["BTC"].AccountBindingSource = f.key, rhBindVenue, robinhoodBindingSourceSettlement
	if err := store.SaveStrategyBook(s); err != nil {
		t.Fatalf("per-book save: %v", err)
	}
	store.Close()
	if got := f.rows(f.cfg.DBFile)["rh-live-store/BTC"]; got != (rhBindRow{f.key, rhBindVenue, robinhoodBindingSourceSettlement}) {
		t.Fatalf("per-book save row under the storage id = %+v", got)
	}

	store = f.open()
	state, _, err = LoadStateWithStore(f.cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	pos := state.Strategies["rh-live"].Positions["BTC"]
	if pos.AccountKey != f.key || pos.VenueAccountID != rhBindVenue || pos.AccountBindingSource != robinhoodBindingSourceSettlement {
		t.Fatalf("reopened book = %+v, want the binding back", pos)
	}
	pos.AccountBindingSource = robinhoodBindingSourceOperator + "owner-1"
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("full save: %v", err)
	}
	store.Close()
	if got := f.rows(f.cfg.DBFile)["rh-live-store/BTC"]; got != (rhBindRow{f.key, rhBindVenue, "operator:owner-1"}) {
		t.Fatalf("full save row = %+v", got)
	}
	if got := f.rows(f.cfg.PaperDBFile)["rh-paper/BTC"]; got != (rhBindRow{}) {
		t.Fatalf("paper row = %+v, want empty binding fields", got)
	}
}

func TestRobinhoodAccountBindingBackfill(t *testing.T) {
	cfgs := []StrategyConfig{
		rhBindLive("rh-uniq", "BTC", "rh-uniq-store"),
		rhBindLive("rh-none", "ETH", ""),
		rhBindLive("rh-two", "SOL", ""),
		rhBindLive("rh-noorder", "DOGE", ""),
		rhBindLive("rh-partial", "ADA", ""),
		rhBindLive("rh-kept", "LTC", ""),
	}
	build := func(t *testing.T) *rhBindFixture {
		f := newRHBindFixture(t, cfgs...)
		f.seed(func(state *AppState, store *StateStore) {
			rhBindOpenBook(state.Strategies["rh-uniq"], "BTC", "pid-u", "ord-u", "btc")
			rhBindOpenBook(state.Strategies["rh-none"], "ETH", "pid-n", "ord-n", "ETH")
			rhBindOpenBook(state.Strategies["rh-two"], "SOL", "pid-t", "ord-t", "SOL")
			rhBindOpenBook(state.Strategies["rh-noorder"], "DOGE", "pid-d", "", "DOGE")
			rhBindOpenBook(state.Strategies["rh-partial"], "ADA", "pid-a", "ord-a", "ADA")
			rhBindOpenBook(state.Strategies["rh-kept"], "LTC", "pid-k", "ord-k", "LTC")
			kept := state.Strategies["rh-kept"].Positions["LTC"]
			kept.AccountKey, kept.VenueAccountID, kept.AccountBindingSource = "robinhood:user-older", "acct-old", robinhoodBindingSourceSettlement
		})
		store := f.open()
		f.attempt(store, "rh-uniq", "rh-u1", "btc", "ord-u", f.key, rhBindVenue)
		f.attempt(store, "rh-two", "rh-t1", "SOL", "ord-t", f.key, rhBindVenue)
		f.attempt(store, "rh-two", "rh-t2", "SOL", "ord-t", f.key, rhBindVenue)
		f.attempt(store, "rh-partial", "rh-a1", "ADA", "ord-a", f.key, "")
		f.attempt(store, "rh-kept", "rh-k1", "LTC", "ord-k", f.key, rhBindVenue)
		store.Close()
		return f
	}

	t.Run("binds_only_unique_complete_evidence_and_is_idempotent", func(t *testing.T) {
		f := build(t)
		state, lines, err := f.startup()
		if err != nil {
			t.Fatalf("startup: %v", err)
		}
		joined := strings.Join(lines, "\n")
		for _, want := range []string{
			"rh-uniq BTC bound by backfill",
			"rh-partial ADA bound by backfill",
			"rh-none ETH stays unbound: no settled live signal buy matches the opening order",
			"rh-two SOL stays unbound: 2 settled live signal buys match the opening order",
			"rh-noorder DOGE stays unbound: the opening trade has no venue order id",
			"[robinhood-account] rh-uniq BTC qty=0.5 position=pid-u state=current",
			"[robinhood-account] rh-partial ADA qty=0.5 position=pid-a state=unbound",
			"[robinhood-account] rh-kept LTC qty=0.5 position=pid-k state=foreign",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("startup lines miss %q:\n%s", want, joined)
			}
		}
		if strings.Contains(joined, "offline-user") {
			t.Fatalf("startup lines print the login name:\n%s", joined)
		}
		rows := f.rows(f.cfg.DBFile)
		want := map[string]rhBindRow{
			"rh-uniq-store/BTC": {f.key, rhBindVenue, robinhoodBindingSourceBackfill},
			"rh-partial/ADA":    {f.key, "", robinhoodBindingSourceBackfill},
			"rh-none/ETH":       {},
			"rh-two/SOL":        {},
			"rh-noorder/DOGE":   {},
			"rh-kept/LTC":       {"robinhood:user-older", "acct-old", robinhoodBindingSourceSettlement},
		}
		for k, w := range want {
			if rows[k] != w {
				t.Errorf("row %s = %+v, want %+v", k, rows[k], w)
			}
		}
		if pos := state.Strategies["rh-uniq"].Positions["BTC"]; pos.AccountKey != f.key || pos.VenueAccountID != rhBindVenue || robinhoodBookAccountState(pos, f.key, "") != robinhoodAccountCurrent {
			t.Fatalf("memory book = %+v, want bound and current", pos)
		}
		if st := robinhoodBookAccountState(state.Strategies["rh-partial"].Positions["ADA"], f.key, ""); st != robinhoodAccountUnbound {
			t.Fatalf("a key-only binding is %s, want held as unbound", st)
		}
		_, again, err := f.startup()
		if err != nil {
			t.Fatalf("second startup: %v", err)
		}
		if strings.Contains(strings.Join(again, "\n"), "bound by backfill") {
			t.Fatalf("second start bound again:\n%s", strings.Join(again, "\n"))
		}
		if after := f.rows(f.cfg.DBFile); fmt.Sprint(sortedRowKeys(after)) != fmt.Sprint(sortedRowKeys(rows)) || after["rh-uniq-store/BTC"] != rows["rh-uniq-store/BTC"] || after["rh-partial/ADA"] != rows["rh-partial/ADA"] {
			t.Fatalf("second start changed rows: %+v", after)
		}
	})

	t.Run("preflight_read_failure_writes_nothing", func(t *testing.T) {
		f := build(t)
		robinhoodBindingReadHook = func(procID string) error {
			if procID == "rh-uniq" {
				return fmt.Errorf("injected binding evidence read failure")
			}
			return nil
		}
		_, _, err := f.startup()
		robinhoodBindingReadHook = nil
		var bindErr *robinhoodBindingStartupError
		if !errors.As(err, &bindErr) || !strings.Contains(err.Error(), "no book was bound") {
			t.Fatalf("startup err = %v, want a binding refusal before any write", err)
		}
		for k, r := range f.rows(f.cfg.DBFile) {
			if k != "rh-kept/LTC" && r != (rhBindRow{}) {
				t.Fatalf("row %s = %+v after a failed preflight, want unchanged", k, r)
			}
		}
		if _, _, err := f.startup(); err != nil {
			t.Fatalf("restart after the read recovers: %v", err)
		}
		if got := f.rows(f.cfg.DBFile)["rh-uniq-store/BTC"]; got.Key != f.key {
			t.Fatalf("restart did not bind: %+v", got)
		}
	})

	t.Run("later_commit_failure_names_earlier_commits_and_restart_finishes", func(t *testing.T) {
		f := build(t)
		calls := 0
		storeCommitHook = func(storageRole) error {
			calls++
			if calls == 2 {
				return fmt.Errorf("injected binding commit failure")
			}
			return nil
		}
		_, _, err := f.startup()
		storeCommitHook = nil
		var bindErr *robinhoodBindingStartupError
		if !errors.As(err, &bindErr) || !strings.Contains(err.Error(), "failed for rh-uniq (primary state file)") || !strings.Contains(err.Error(), "books already bound: rh-partial (primary state file)") {
			t.Fatalf("startup err = %v, want the failed book and the earlier commit named", err)
		}
		rows := f.rows(f.cfg.DBFile)
		if rows["rh-partial/ADA"].Source != robinhoodBindingSourceBackfill || rows["rh-uniq-store/BTC"] != (rhBindRow{}) {
			t.Fatalf("rows = %+v, want rh-partial committed and rh-uniq unchanged", rows)
		}
		state, lines, err := f.startup()
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		if !strings.Contains(strings.Join(lines, "\n"), "rh-uniq BTC bound by backfill") || strings.Contains(strings.Join(lines, "\n"), "rh-partial ADA bound by backfill") {
			t.Fatalf("restart lines:\n%s", strings.Join(lines, "\n"))
		}
		if pos := state.Strategies["rh-uniq"].Positions["BTC"]; pos.AccountKey != f.key {
			t.Fatalf("restart memory = %+v", pos)
		}
	})

	t.Run("read_only_startup_never_backfills", func(t *testing.T) {
		f := build(t)
		store := f.open()
		_, _, _, lines, err := prepareStartupStateReport(f.cfg, store, false)
		store.Close()
		if err != nil || len(lines) != 0 {
			t.Fatalf("read-only startup lines=%v err=%v, want none", lines, err)
		}
		if got := f.rows(f.cfg.DBFile)["rh-uniq-store/BTC"]; got != (rhBindRow{}) {
			t.Fatalf("read-only startup bound %+v", got)
		}
	})
}

func sortedRowKeys(m map[string]rhBindRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func rhBindFileBytes(t *testing.T, paths ...string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, p := range paths {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			b, err := os.ReadFile(p + suffix)
			if err == nil {
				out[p+suffix] = b
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}
	return out
}

func TestRobinhoodAccountBindingStorageInspectIsReadOnly(t *testing.T) {
	for _, schema := range []string{"old", "current"} {
		t.Run(schema, func(t *testing.T) {
			f := newRHBindFixture(t, rhBindLive("rh-live", "BTC", "rh-live-store"))
			f.seed(func(state *AppState, _ *StateStore) {
				rhBindOpenBook(state.Strategies["rh-live"], "BTC", "pid-live", "ord-1", "BTC")
				if schema == "current" {
					p := state.Strategies["rh-live"].Positions["BTC"]
					p.AccountKey, p.VenueAccountID, p.AccountBindingSource = f.key, rhBindVenue, robinhoodBindingSourceBackfill
				}
			})
			if schema == "old" {
				db, err := openStateDB(f.cfg.DBFile, false)
				if err != nil {
					t.Fatal(err)
				}
				for _, col := range []string{"account_key", "venue_account_id", "account_binding_source"} {
					if _, err := db.db.Exec("ALTER TABLE positions DROP COLUMN " + col); err != nil {
						t.Fatal(err)
					}
				}
				db.Close()
			}
			before := rhBindFileBytes(t, f.cfg.DBFile, f.cfg.PaperDBFile)
			si, err := inspectStorageLayoutForConfig(f.cfg, false)
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			after := rhBindFileBytes(t, f.cfg.DBFile, f.cfg.PaperDBFile)
			for p, b := range before {
				if !bytes.Equal(b, after[p]) {
					t.Fatalf("inspect changed %s", p)
				}
			}
			for p, b := range after {
				if _, existed := before[p]; !existed && strings.HasSuffix(p, "-wal") && len(b) != 0 {
					t.Fatalf("inspect wrote %d bytes of write-ahead log to %s", len(b), p)
				}
			}
			var books []string
			for _, fi := range si.Files {
				books = append(books, fi.RobinhoodBooks...)
			}
			want := "local_state=current"
			if schema == "old" {
				want = "local_state=missing evidence"
			}
			if len(books) != 1 || !strings.Contains(books[0], "rh-live BTC long qty=0.5 position=pid-live") || !strings.Contains(books[0], want) {
				t.Fatalf("inspect books = %v, want one rh-live line with %q", books, want)
			}
		})
	}
}
