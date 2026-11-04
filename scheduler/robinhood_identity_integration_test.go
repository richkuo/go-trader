package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	rhIdentityLive      = "rh-live"
	rhIdentityLiveStore = "rh-live-store"
	rhIdentityPaper     = "rh-paper"
	rhIdentityLab       = "rh-lab"
	rhIdentityAccount   = "acct-key-1"
	rhIdentityHourMs    = int64(3600000)
)

type rhIdentityCoins struct {
	live, paper, lab string
}

type rhIdentityFixture struct {
	t                            *testing.T
	dir                          string
	livePath, paperPath, labPath string
	configPath                   string
	configBytes                  []byte
	coins                        rhIdentityCoins
}

func rhIdentityStrategyConfigs(c rhIdentityCoins) []StrategyConfig {
	return []StrategyConfig{
		{ID: rhIdentityLive, Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
			Args: []string{"momentum_pro", c.live, "1h", "--mode=live"}, Capital: 1000, InitialCapital: 1000, MaxDrawdownPct: 50, StorageStrategyID: rhIdentityLiveStore},
		{ID: rhIdentityPaper, Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
			Args: []string{"momentum_pro", c.paper, "1h", "--mode=paper"}, Capital: 1000, InitialCapital: 1000, MaxDrawdownPct: 50},
		{ID: rhIdentityLab, Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
			Args: []string{"momentum_pro", c.lab, "1h", "--mode=paper"}, Capital: 1000, InitialCapital: 1000, MaxDrawdownPct: 50, PaperSource: "lab"},
	}
}

func (f *rhIdentityFixture) rawConfig(extra ...StrategyConfig) *Config {
	return &Config{DBFile: f.livePath, PaperDBFile: f.paperPath,
		PaperSources: []PaperSourceConfig{{ID: "lab", DBFile: f.labPath}},
		Strategies:   append(rhIdentityStrategyConfigs(f.coins), extra...)}
}

func (f *rhIdentityFixture) writeConfig(initialCapital float64, extra ...map[string]interface{}) {
	f.t.Helper()
	strategies := []interface{}{}
	for _, sc := range rhIdentityStrategyConfigs(f.coins) {
		m := map[string]interface{}{"id": sc.ID, "type": sc.Type, "platform": sc.Platform, "script": sc.Script,
			"args": sc.Args, "capital": sc.Capital, "initial_capital": sc.InitialCapital, "max_drawdown_pct": sc.MaxDrawdownPct}
		if sc.ID == rhIdentityLive {
			m["storage_strategy_id"] = rhIdentityLiveStore
			m["initial_capital"] = initialCapital
		}
		if sc.PaperSource != "" {
			m["paper_source"] = sc.PaperSource
		}
		strategies = append(strategies, m)
	}
	for _, m := range extra {
		strategies = append(strategies, m)
	}
	data, err := json.MarshalIndent(map[string]interface{}{
		"config_version": CurrentConfigVersion, "interval_seconds": 300,
		"db_file": f.livePath, "paper_db_file": f.paperPath,
		"paper_sources": []interface{}{map[string]interface{}{"id": "lab", "db_file": f.labPath}},
		"strategies":    strategies,
	}, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	f.configPath = filepath.Join(f.dir, "config.json")
	if err := os.WriteFile(f.configPath, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.configBytes = data
}

func rhIdentityOpen(t *testing.T, cfg *Config) *StateStore {
	t.Helper()
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	store, err := OpenStateStore(layout, ident)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func rhIdentityBook(id, typ, coin string, qty float64, pid string) *StrategyState {
	s := &StrategyState{ID: id, Type: typ, Platform: "robinhood", Cash: 950, InitialCapital: 1000,
		Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	if coin != "" {
		s.Positions[coin] = &Position{Symbol: coin, Quantity: qty, InitialQuantity: qty, AvgCost: 100, Side: "long",
			OwnerStrategyID: id, TradePositionID: pid, OpenedAt: time.Unix(1700000000, 0).UTC()}
	}
	return s
}

func newRHIdentityFixture(t *testing.T, coins rhIdentityCoins, extraRaw []StrategyConfig, mutate func(state *AppState, store *StateStore)) *rhIdentityFixture {
	t.Helper()
	t.Setenv("ROBINHOOD_USERNAME", "offline-user")
	t.Setenv("ROBINHOOD_PASSWORD", "offline-pass")
	t.Setenv("ROBINHOOD_TOTP_SECRET", "JBSWY3DPEHPK3PXP")
	dir := t.TempDir()
	f := &rhIdentityFixture{t: t, dir: dir, livePath: filepath.Join(dir, "live.db"), paperPath: filepath.Join(dir, "paper.db"),
		labPath: filepath.Join(dir, "lab.db"), coins: coins}
	raw := f.rawConfig(extraRaw...)
	store := rhIdentityOpen(t, raw)

	state := NewAppState()
	live := rhIdentityBook(rhIdentityLive, "spot", coins.live, 0.5, "pid-live")
	live.RiskState.PendingCircuitCloses = map[string]*PendingCircuitClose{
		PlatformPendingCloseRobinhood: {SizeBasis: robinhoodPendingSizeBasisBook, Episode: "ep-1",
			Symbols: []PendingCircuitCloseSymbol{{Symbol: coins.live, Size: 0.5, PositionID: "pid-live"}}},
	}
	live.RiskState.RobinhoodSharedCloseNotice = RobinhoodSharedCloseNotice{Coin: coins.live, CBUntil: time.Unix(1700003600, 0).UTC(), LastNotifiedAt: time.Unix(1700000100, 0).UTC()}
	RecordTrade(live, Trade{StrategyID: rhIdentityLive, Symbol: coins.live, PositionID: "pid-live", Side: "buy", Quantity: 0.5, Price: 100, Value: 50, Timestamp: time.Unix(1700000000, 0).UTC()})
	state.Strategies[rhIdentityLive] = live
	paper := rhIdentityBook(rhIdentityPaper, "spot", coins.paper, 0.25, "pid-paper")
	RecordTrade(paper, Trade{StrategyID: rhIdentityPaper, Symbol: coins.paper, PositionID: "pid-paper", Side: "buy", Quantity: 0.25, Price: 100, Value: 25, Timestamp: time.Unix(1700000000, 0).UTC()})
	state.Strategies[rhIdentityPaper] = paper
	state.Strategies[rhIdentityLab] = rhIdentityBook(rhIdentityLab, "spot", coins.lab, 2, "pid-lab")
	if mutate != nil {
		mutate(state, store)
	}
	if err := SaveStateWithStore(state, store); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	primary := store.primary()
	for _, a := range []*robinhoodOrderAttempt{
		{AttemptID: "rh-held", AccountKey: rhIdentityAccount, Symbol: coins.live, Origin: robinhoodOriginSignal, Side: "buy", RefID: "ref-held",
			Status: robinhoodAttemptUnknown, RequestJSON: `{"symbol":"` + coins.live + `","amount_usd":10}`, EvidenceJSON: `{"note":"held"}`, CreatedAtMs: 2000, UpdatedAtMs: 2000},
		{AttemptID: "rh-old", AccountKey: rhIdentityAccount, Symbol: coins.live, Origin: robinhoodOriginSignal, Side: "buy", RefID: "ref-old", VenueOrderID: "ord-old",
			Status: robinhoodAttemptSettled, RequestJSON: `{"symbol":"` + coins.live + `"}`, EvidenceJSON: `{"note":"settled"}`, ConfirmedQty: 0.5, AppliedQty: 0.5, SettlementSeq: 1, CreatedAtMs: 1000, UpdatedAtMs: 1000},
	} {
		if err := upsertRobinhoodAttemptRow(primary.db, rhIdentityLiveStore, a); err != nil {
			t.Fatalf("seed attempt: %v", err)
		}
	}
	paperDB := store.file(storageRolePaper)
	for _, side := range []string{entrySideLong, entrySideShort} {
		if err := upsertEntryDecisionRow(paperDB.db, defaultPaperPartition, rhIdentityPaper, entryDecisionKey{Symbol: coins.paper, IntervalMs: rhIdentityHourMs, Side: side},
			entryDecisionRecord{LatestObservedBarMs: 7, ConsumedThroughBarMs: 5, UpdatedAtMs: 9}); err != nil {
			t.Fatalf("seed decision: %v", err)
		}
	}
	done := &entryIntent{IntentID: "intent-done", Partition: defaultPaperPartition, Symbol: coins.paper, IntervalMs: rhIdentityHourMs, TargetSide: entrySideLong,
		BarOpenMs: 1, DecisionBoundaryMs: 2, CutoffMs: 3, Provider: "paper", ClientOrderID: "cid-done", Status: entryIntentCommitted,
		RequestJSON: `{"symbol":"` + coins.paper + `"}`, CreatedAtMs: 1, UpdatedAtMs: 1}
	if err := upsertEntryIntentRow(paperDB.db, rhIdentityPaper, done, true); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	labDB := store.file(paperSourceRole("lab"))
	if err := upsertEntryDecisionRow(labDB.db, RiskPartition{Scope: ScopePaper, Source: "lab"}, rhIdentityLab, entryDecisionKey{Symbol: coins.lab, IntervalMs: rhIdentityHourMs, Side: entrySideLong},
		entryDecisionRecord{LatestObservedBarMs: 3, UpdatedAtMs: 4}); err != nil {
		t.Fatalf("seed lab decision: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	f.writeConfig(1000)
	return f
}

func (f *rhIdentityFixture) load() *Config {
	f.t.Helper()
	cfg, err := LoadConfig(f.configPath)
	if err != nil {
		f.t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func (f *rhIdentityFixture) startup(cfg *Config) (*AppState, []string, error) {
	f.t.Helper()
	store := rhIdentityOpen(f.t, cfg)
	defer store.Close()
	state, _, lines, err := prepareStartupState(cfg, store, true)
	return state, lines, err
}

type rhIdentityDump struct {
	Positions   []string
	Strategies  []string
	Attempts    []string
	Decisions   []string
	Intents     []string
	Trades      []string
	InitCapital []string
}

func rhIdentityRows(t *testing.T, path, query string) []string {
	t.Helper()
	raw, err := openStateDBReadOnly(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer raw.Close()
	if ok, err := tableExists(raw, strings.Fields(strings.SplitN(query, " FROM ", 2)[1])[0]); err != nil || !ok {
		return nil
	}
	rows, err := raw.db.Query(query)
	if err != nil {
		t.Fatalf("%s on %s: %v", query, path, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}

func (f *rhIdentityFixture) dump(path string) rhIdentityDump {
	return rhIdentityDump{
		Positions:   rhIdentityRows(f.t, path, `SELECT strategy_id, symbol, position_id, quantity, avg_cost, opened_at FROM positions ORDER BY strategy_id, symbol`),
		Strategies:  rhIdentityRows(f.t, path, `SELECT id, cash, risk_pending_circuit_closes_json, risk_rh_shared_close_notice_json FROM strategies ORDER BY id`),
		Attempts:    rhIdentityRows(f.t, path, `SELECT attempt_id, strategy_id, account_key, symbol, status, ref_id, venue_order_id, request_json, evidence_json, applied_qty, updated_at_ms FROM robinhood_order_attempts ORDER BY attempt_id`),
		Decisions:   rhIdentityRows(f.t, path, `SELECT scope, source, strategy_id, symbol, interval_ms, target_side, latest_observed_bar_ms, consumed_through_bar_ms, updated_at_ms FROM entry_decision_state ORDER BY strategy_id, symbol, target_side`),
		Intents:     rhIdentityRows(f.t, path, `SELECT intent_id, symbol, status, request_json FROM entry_order_intents ORDER BY intent_id`),
		Trades:      rhIdentityRows(f.t, path, `SELECT strategy_id, symbol, position_id, quantity, price FROM trades ORDER BY rowid`),
		InitCapital: rhIdentityRows(f.t, path, `SELECT id, initial_capital FROM strategies ORDER BY id`),
	}
}

func (f *rhIdentityFixture) dumpAll() map[string]rhIdentityDump {
	return map[string]rhIdentityDump{"live": f.dump(f.livePath), "paper": f.dump(f.paperPath), "lab": f.dump(f.labPath)}
}

func rhIdentityJSON(v interface{}) string {
	b, _ := json.MarshalIndent(v, "", " ")
	return string(b)
}

func TestRobinhoodCoinIdentityConfigNormalization(t *testing.T) {
	t.Setenv("ROBINHOOD_USERNAME", "offline-user")
	t.Setenv("ROBINHOOD_PASSWORD", "offline-pass")
	t.Setenv("ROBINHOOD_TOTP_SECRET", "JBSWY3DPEHPK3PXP")
	write := func(t *testing.T, db, liveCoin, paperCoin string) (string, []byte) {
		strategies := []interface{}{
			map[string]interface{}{"id": "rh-live", "type": "spot", "platform": "robinhood", "script": "shared_scripts/check_robinhood.py",
				"args": []string{"momentum_pro", liveCoin, "1h", "--mode=live"}, "capital": 1000.0, "max_drawdown_pct": 50.0},
			map[string]interface{}{"id": "rh-paper", "type": "spot", "platform": "robinhood", "script": "shared_scripts/check_robinhood.py",
				"args": []string{"momentum_pro", paperCoin, "1h", "--mode=paper"}, "capital": 1000.0, "max_drawdown_pct": 50.0},
			map[string]interface{}{"id": "rh-opt", "type": "options", "platform": "robinhood", "script": "shared_scripts/check_options.py",
				"args": []string{"covered_calls", "spy", "--platform=robinhood", "--mode=paper"}, "capital": 1000.0, "max_drawdown_pct": 40.0},
		}
		data, err := json.Marshal(map[string]interface{}{"config_version": CurrentConfigVersion, "interval_seconds": 300,
			"db_file": db, "strategies": strategies})
		if err != nil {
			t.Fatal(err)
		}
		return writeTestConfig(t, t.TempDir(), string(data)), data
	}
	t.Run("every_loader_reads_one_trimmed_uppercase_coin", func(t *testing.T) {
		path, before := write(t, filepath.Join(t.TempDir(), "state.db"), "btc", " Btc ")
		loaders := map[string]func(string) (*Config, error){"live": LoadConfig, "read_only": LoadConfigReadOnly, "probe": LoadConfigForProbe}
		for _, name := range []string{"live", "probe", "read_only"} {
			cfg, err := loaders[name](path)
			if err != nil {
				t.Fatalf("%s loader: %v", name, err)
			}
			got := map[string]string{}
			for _, sc := range cfg.Strategies {
				got[sc.ID] = sc.Args[1]
			}
			if got["rh-live"] != "BTC" || got["rh-paper"] != "BTC" || got["rh-opt"] != "spy" {
				t.Fatalf("%s loader coins = %v, want BTC for both spot strategies and the options underlying unchanged", name, got)
			}
			warns := robinhoodCoinRewriteWarnings(cfg)
			if len(warns) != 2 || !strings.Contains(warns[0], `strategy rh-live robinhood coin "btc" is read as "BTC"`) || !strings.Contains(warns[1], `strategy rh-paper robinhood coin " Btc " is read as "BTC"`) {
				t.Fatalf("%s loader warnings = %q, want one per rewritten strategy", name, warns)
			}
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("normalization must never write the config file")
		}
	})
	t.Run("canonical_config_logs_nothing", func(t *testing.T) {
		path, _ := write(t, filepath.Join(t.TempDir(), "state.db"), "BTC", "ETH")
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if warns := robinhoodCoinRewriteWarnings(cfg); len(warns) != 0 {
			t.Fatalf("warnings = %q, want none", warns)
		}
	})
	t.Run("whitespace_only_coin_refuses", func(t *testing.T) {
		path, _ := write(t, filepath.Join(t.TempDir(), "state.db"), "   ", "BTC")
		_, err := LoadConfig(path)
		if err == nil || !strings.Contains(err.Error(), `strategy[rh-live]: robinhood spot coin args[1] "   " is empty after trimming`) {
			t.Fatalf("err = %v, want the empty coin refusal", err)
		}
	})
	t.Run("case_only_reload_is_hot_and_a_new_coin_needs_restart", func(t *testing.T) {
		db := filepath.Join(t.TempDir(), "state.db")
		oldPath, _ := write(t, db, "btc", "eth")
		cur, err := LoadConfig(oldPath)
		if err != nil {
			t.Fatal(err)
		}
		casePath, _ := write(t, db, "BTC", " Eth")
		next, err := LoadConfig(casePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateHotReloadCompatible(cur, next); err != nil {
			t.Fatalf("a case-only coin edit must reload, got %v", err)
		}
		otherPath, _ := write(t, db, "ETH", "eth")
		other, err := LoadConfig(otherPath)
		if err != nil {
			t.Fatal(err)
		}
		err = validateHotReloadCompatible(cur, other)
		if err == nil || !strings.Contains(err.Error(), "strategy[rh-live] changed non-hot-reloadable fields (restart required)") {
			t.Fatalf("a different coin must stay restart-required, got %v", err)
		}
	})
}

func TestRobinhoodCoinIdentityStartupMigration(t *testing.T) {
	t.Run("converts_every_book_once_and_keeps_history", func(t *testing.T) {
		f := newRHIdentityFixture(t, rhIdentityCoins{live: "btc", paper: "Btc", lab: "eth"}, nil, nil)
		before := f.dumpAll()
		cfg := f.load()

		readOnly := rhIdentityOpen(t, cfg)
		if _, _, lines, err := prepareStartupState(cfg, readOnly, false); err != nil || len(lines) != 0 {
			t.Fatalf("a read-only startup must not migrate, lines=%q err=%v", lines, err)
		}
		readOnly.Close()
		if got := f.dumpAll(); rhIdentityJSON(got) != rhIdentityJSON(before) {
			t.Fatalf("a read-only startup changed the state files")
		}

		state, lines, err := f.startup(cfg)
		if err != nil {
			t.Fatalf("startup: %v", err)
		}
		want := []string{
			"[storage] Robinhood coin identity migrated for rh-live in the primary state file to BTC: positions=1 pending_circuit_closes=1 shared_close_notice=1 unresolved_attempts=1 entry_decisions=0 (#1908)",
			"[storage] Robinhood coin identity migrated for rh-paper in the paper state file to BTC: positions=1 pending_circuit_closes=0 shared_close_notice=0 unresolved_attempts=0 entry_decisions=2 (#1908)",
			"[storage] Robinhood coin identity migrated for rh-lab in the paper:lab state file to ETH: positions=1 pending_circuit_closes=0 shared_close_notice=0 unresolved_attempts=0 entry_decisions=1 (#1908)",
		}
		if strings.Join(lines, "\n") != strings.Join(want, "\n") {
			t.Fatalf("migration lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
		}
		live := state.Strategies[rhIdentityLive]
		if pos := live.Positions["BTC"]; pos == nil || pos.Symbol != "BTC" || pos.Quantity != 0.5 || pos.TradePositionID != "pid-live" || len(live.Positions) != 1 {
			t.Fatalf("live book in memory = %+v, want one BTC position", live.Positions)
		}
		if p := live.RiskState.PendingCircuitCloses[PlatformPendingCloseRobinhood]; p == nil || p.Symbols[0].Symbol != "BTC" || p.Symbols[0].PositionID != "pid-live" || p.Episode != "ep-1" {
			t.Fatalf("pending close in memory = %+v", p)
		}
		if live.RiskState.RobinhoodSharedCloseNotice.Coin != "BTC" {
			t.Fatalf("notice in memory = %+v", live.RiskState.RobinhoodSharedCloseNotice)
		}

		after := f.dumpAll()
		if got := after["live"].Positions; len(got) != 1 || !strings.HasPrefix(got[0], "rh-live-store|BTC|pid-live|0.5|100|") {
			t.Fatalf("live positions = %q", got)
		}
		if got := after["paper"].Positions; len(got) != 1 || !strings.HasPrefix(got[0], "rh-paper|BTC|pid-paper|0.25|100|") {
			t.Fatalf("paper positions = %q", got)
		}
		if got := after["lab"].Positions; len(got) != 1 || !strings.HasPrefix(got[0], "rh-lab|ETH|pid-lab|2|100|") {
			t.Fatalf("lab positions = %q", got)
		}
		if got := after["live"].Strategies[0]; !strings.Contains(got, `"symbol":"BTC","size":0.5,"position_id":"pid-live"`) || !strings.Contains(got, `"coin":"BTC"`) || strings.Contains(got, `btc`) {
			t.Fatalf("live strategy row = %q", got)
		}
		wantAttempts := []string{
			`rh-held|rh-live-store|acct-key-1|BTC|unknown|ref-held||{"symbol":"btc","amount_usd":10}|{"note":"held"}|0|2000`,
			`rh-old|rh-live-store|acct-key-1|btc|settled|ref-old|ord-old|{"symbol":"btc"}|{"note":"settled"}|0.5|1000`,
		}
		if got := after["live"].Attempts; strings.Join(got, "\n") != strings.Join(wantAttempts, "\n") {
			t.Fatalf("attempts =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantAttempts, "\n"))
		}
		wantDecisions := []string{"paper||rh-paper|BTC|3600000|long|7|5|9", "paper||rh-paper|BTC|3600000|short|7|5|9"}
		if got := after["paper"].Decisions; strings.Join(got, "\n") != strings.Join(wantDecisions, "\n") {
			t.Fatalf("paper decisions = %q", got)
		}
		if got := after["lab"].Decisions; len(got) != 1 || got[0] != "paper|lab|rh-lab|ETH|3600000|long|3|0|4" {
			t.Fatalf("lab decisions = %q", got)
		}
		for _, file := range []string{"live", "paper", "lab"} {
			if rhIdentityJSON(after[file].Trades) != rhIdentityJSON(before[file].Trades) || rhIdentityJSON(after[file].Intents) != rhIdentityJSON(before[file].Intents) {
				t.Fatalf("%s history changed: trades %q -> %q, intents %q -> %q", file, before[file].Trades, after[file].Trades, before[file].Intents, after[file].Intents)
			}
		}

		cfg2 := f.load()
		_, lines2, err := f.startup(cfg2)
		if err != nil || len(lines2) != 0 {
			t.Fatalf("a second startup must find nothing to migrate, lines=%q err=%v", lines2, err)
		}
		if got := f.dumpAll(); rhIdentityJSON(got) != rhIdentityJSON(after) {
			t.Fatal("a second startup changed the state files")
		}

		store := rhIdentityOpen(t, cfg2)
		defer store.Close()
		state3, _, _, err := prepareStartupState(cfg2, store, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.LoadRobinhoodOrderGuards(state3, cfg2.Strategies); err != nil {
			t.Fatalf("guard load: %v", err)
		}
		if a := robinhoodUnresolvedForCoinLocked(state3, rhIdentityAccount, "BTC"); a == nil || a.AttemptID != "rh-held" {
			t.Fatalf("the renamed attempt must guard the canonical coin, got %+v", a)
		}
	})

	type refusalCase struct {
		name     string
		coins    rhIdentityCoins
		mutate   func(t *testing.T, state *AppState, store *StateStore)
		extra    []map[string]interface{}
		extraRaw []StrategyConfig
		want     string
	}
	cases := []refusalCase{
		{name: "position_collision_in_a_later_file", coins: rhIdentityCoins{live: "btc", paper: "Btc", lab: "eth"},
			mutate: func(t *testing.T, state *AppState, store *StateStore) {
				state.Strategies[rhIdentityLab].Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 1, AvgCost: 90, Side: "long", OwnerStrategyID: rhIdentityLab, TradePositionID: "pid-lab-2"}
			},
			want: `strategy rh-lab holds positions "eth" and "ETH" that both convert to ETH`},
		{name: "zero_quantity_alias_still_collides", coins: rhIdentityCoins{live: "btc", paper: "Btc", lab: "ETH"},
			mutate: func(t *testing.T, state *AppState, store *StateStore) {
				state.Strategies[rhIdentityLab].Positions["eth"] = &Position{Symbol: "eth", Quantity: 0, AvgCost: 90, Side: "long", OwnerStrategyID: rhIdentityLab, TradePositionID: "pid-lab-0"}
			},
			want: `strategy rh-lab holds positions "eth" and "ETH" that both convert to ETH`},
		{name: "pending_close_collision", coins: rhIdentityCoins{live: "btc", paper: "Btc", lab: "eth"},
			mutate: func(t *testing.T, state *AppState, store *StateStore) {
				p := state.Strategies[rhIdentityLive].RiskState.PendingCircuitCloses[PlatformPendingCloseRobinhood]
				p.Symbols = append(p.Symbols, PendingCircuitCloseSymbol{Symbol: "BTC", Size: 0.1})
			},
			want: "strategy rh-live holds more than one pending Robinhood circuit close that converts to BTC"},
		{name: "entry_decision_collision", coins: rhIdentityCoins{live: "btc", paper: "Btc", lab: "eth"},
			mutate: func(t *testing.T, state *AppState, store *StateStore) {
				if err := upsertEntryDecisionRow(store.file(storageRolePaper).db, defaultPaperPartition, rhIdentityPaper, entryDecisionKey{Symbol: "BTC", IntervalMs: rhIdentityHourMs, Side: entrySideShort}, entryDecisionRecord{UpdatedAtMs: 1}); err != nil {
					t.Fatal(err)
				}
			},
			want: `strategy rh-paper holds entry decisions ["BTC" "Btc"] for interval 3600000ms side short that all convert to BTC`},
		{name: "unsupported_active_entry_intent", coins: rhIdentityCoins{live: "btc", paper: "Btc", lab: "eth"},
			mutate: func(t *testing.T, state *AppState, store *StateStore) {
				in := &entryIntent{IntentID: "intent-live", Partition: defaultPaperPartition, Symbol: "Btc", IntervalMs: rhIdentityHourMs, TargetSide: entrySideShort,
					BarOpenMs: 1, DecisionBoundaryMs: 2, CutoffMs: 3, Provider: "paper", ClientOrderID: "cid-live", Status: entryIntentReserved, CreatedAtMs: 1, UpdatedAtMs: 1}
				if err := upsertEntryIntentRow(store.file(storageRolePaper).db, rhIdentityPaper, in, true); err != nil {
					t.Fatal(err)
				}
			},
			want: "strategy rh-paper holds active entry intent intent-live (Btc, status reserved); no Robinhood entry provider can resolve or rewrite it"},
		{name: "mixed_case_unresolved_attempt_collision", coins: rhIdentityCoins{live: "btc", paper: "Btc", lab: "eth"},
			mutate: func(t *testing.T, state *AppState, store *StateStore) {
				state.Strategies["rh-peer"] = rhIdentityBook("rh-peer", "spot", "", 0, "")
				a := &robinhoodOrderAttempt{AttemptID: "rh-peer-held", AccountKey: rhIdentityAccount, Symbol: "BTC", Origin: robinhoodOriginSignal, Side: "buy", RefID: "ref-peer",
					Status: robinhoodAttemptUnknown, CreatedAtMs: 3000, UpdatedAtMs: 3000}
				if err := upsertRobinhoodAttemptRow(store.primary().db, "rh-peer", a); err != nil {
					t.Fatal(err)
				}
			},
			extraRaw: []StrategyConfig{{ID: "rh-peer", Type: "spot", Platform: "robinhood", Script: "shared_scripts/check_robinhood.py",
				Args: []string{"momentum_pro", "BTC", "1h", "--mode=live"}, Capital: 1000, MaxDrawdownPct: 50}},
			extra: []map[string]interface{}{{"id": "rh-peer", "type": "spot", "platform": "robinhood", "script": "shared_scripts/check_robinhood.py",
				"args": []string{"momentum_pro", "BTC", "1h", "--mode=live"}, "capital": 1000.0, "max_drawdown_pct": 50.0}},
			want: "unresolved Robinhood order attempts rh-held (rh-live, btc), rh-peer-held (rh-peer, BTC) would share one unresolved slot for BTC on one account after conversion"},
	}
	for _, tc := range cases {
		t.Run("refuses_"+tc.name+"_before_any_write", func(t *testing.T) {
			var f *rhIdentityFixture
			f = newRHIdentityFixture(t, tc.coins, tc.extraRaw, func(state *AppState, store *StateStore) { tc.mutate(t, state, store) })
			f.writeConfig(2500, tc.extra...)
			before := f.dumpAll()
			cfg := f.load()
			_, lines, err := f.startup(cfg)
			var startupErr *robinhoodIdentityStartupError
			var refusal *robinhoodIdentityRefusal
			if !errors.As(err, &startupErr) || !errors.As(err, &refusal) || !strings.Contains(err.Error(), tc.want) || len(lines) != 0 {
				t.Fatalf("startup = lines %q err %v, want a preflight refusal naming %q", lines, err, tc.want)
			}
			if !strings.Contains(err.Error(), "no book was changed") || !strings.Contains(err.Error(), "never delete a row") {
				t.Fatalf("refusal must tell the operator what to do, got %v", err)
			}
			if got := f.dumpAll(); rhIdentityJSON(got) != rhIdentityJSON(before) {
				t.Fatalf("a refused preflight changed a state file:\nbefore %s\nafter %s", rhIdentityJSON(before), rhIdentityJSON(got))
			}
			if got := f.dumpAll()["live"].InitCapital; len(got) == 0 || !strings.Contains(strings.Join(got, ","), "rh-live-store|1000") {
				t.Fatalf("the configured initial_capital change must not be written before the refusal, got %q", got)
			}
		})
	}
}

func TestRobinhoodCoinIdentityPerBookTransactions(t *testing.T) {
	failAt := func(role storageRole) func() {
		prev := storeCommitHook
		storeCommitHook = func(r storageRole) error {
			if r == role {
				return fmt.Errorf("injected %s commit failure", role)
			}
			return nil
		}
		return func() { storeCommitHook = prev }
	}
	t.Run("a_failed_book_rolls_back_whole", func(t *testing.T) {
		f := newRHIdentityFixture(t, rhIdentityCoins{live: "btc", paper: "Btc", lab: "eth"}, nil, nil)
		before := f.dumpAll()
		cfg := f.load()
		restore := failAt(storageRolePrimary)
		store := rhIdentityOpen(t, cfg)
		state, _, _, err := loadStateWithStore(cfg, store, true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = migrateRobinhoodCoinIdentity(cfg, store, state)
		restore()
		store.Close()
		var applyErr *robinhoodIdentityApplyError
		if !errors.As(err, &applyErr) || !strings.Contains(err.Error(), "injected primary commit failure") || !strings.Contains(err.Error(), "books already committed: none") {
			t.Fatalf("err = %v, want the primary book failure with nothing committed", err)
		}
		if got := f.dumpAll(); rhIdentityJSON(got) != rhIdentityJSON(before) {
			t.Fatalf("a failed book transaction changed a state file:\nbefore %s\nafter %s", rhIdentityJSON(before), rhIdentityJSON(got))
		}
		live := state.Strategies[rhIdentityLive]
		if live.Positions["btc"] == nil || live.Positions["BTC"] != nil || live.RiskState.PendingCircuitCloses[PlatformPendingCloseRobinhood].Symbols[0].Symbol != "btc" || live.RiskState.RobinhoodSharedCloseNotice.Coin != "btc" {
			t.Fatalf("a failed transaction must not publish the staged book, got %+v", live.Positions)
		}
	})
	t.Run("a_later_book_failure_stops_startup_and_a_restart_finishes", func(t *testing.T) {
		f := newRHIdentityFixture(t, rhIdentityCoins{live: "btc", paper: "Btc", lab: "eth"}, nil, nil)
		before := f.dumpAll()
		cfg := f.load()
		restore := failAt(storageRolePaper)
		_, lines, err := f.startup(cfg)
		restore()
		var startupErr *robinhoodIdentityStartupError
		if !errors.As(err, &startupErr) || !strings.Contains(err.Error(), "failed for rh-paper (paper state file)") ||
			!strings.Contains(err.Error(), "books already committed: rh-live (primary state file)") || !strings.Contains(err.Error(), "never let an older go-trader binary trade these files") {
			t.Fatalf("err = %v, want the paper failure after the committed live book", err)
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "for rh-live in the primary state file") {
			t.Fatalf("lines = %q, want only the committed live book", lines)
		}
		mid := f.dumpAll()
		if got := mid["live"].Positions; len(got) != 1 || !strings.HasPrefix(got[0], "rh-live-store|BTC|pid-live|0.5|") {
			t.Fatalf("live book must stay committed, got %q", got)
		}
		for _, file := range []string{"paper", "lab"} {
			if rhIdentityJSON(mid[file]) != rhIdentityJSON(before[file]) {
				t.Fatalf("%s file changed after the failed paper book:\nbefore %s\nafter %s", file, rhIdentityJSON(before[file]), rhIdentityJSON(mid[file]))
			}
		}
		_, lines, err = f.startup(f.load())
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		if len(lines) != 2 || !strings.Contains(lines[0], "for rh-paper in the paper state file") || !strings.Contains(lines[1], "for rh-lab in the paper:lab state file") {
			t.Fatalf("restart lines = %q, want the paper and lab books only", lines)
		}
		final := f.dumpAll()
		if rhIdentityJSON(final["live"]) != rhIdentityJSON(mid["live"]) {
			t.Fatal("the restart migrated the committed live book again")
		}
		if got := final["paper"].Decisions; len(got) != 2 || !strings.Contains(got[0], "|BTC|") || !strings.Contains(got[1], "|BTC|") {
			t.Fatalf("paper decisions after restart = %q", got)
		}
	})
}
