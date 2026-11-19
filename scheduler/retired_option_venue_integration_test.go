package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func rvOptionStrategy(id, platform, source string) map[string]interface{} {
	sc := map[string]interface{}{"id": id, "type": "options", "script": "shared_scripts/check_options.py",
		"args": []string{"wheel", "BTC", "--platform=" + platform, "--mode=paper"}, "capital": 1000, "max_drawdown_pct": 40}
	if platform != "" {
		sc["platform"] = platform
	}
	if source != "" {
		sc["paper_source"] = source
	}
	return sc
}

func rvWriteConfig(t *testing.T, dir string, root map[string]interface{}) (string, []byte) {
	t.Helper()
	blob, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, blob
}

func rvDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func rvBaseConfig(dir string, version int, strategies ...map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"config_version":   version,
		"interval_seconds": 600,
		"log_dir":          filepath.Join(dir, "logs"),
		"db_file":          filepath.Join(dir, "state.db"),
		"strategies":       strategies,
	}
}

func TestRetiredOptionVenueConfigAdmissionIntegration(t *testing.T) {
	t.Setenv("OKX_API_KEY", "")
	t.Run("supported_options_venues_load", func(t *testing.T) {
		dir := t.TempDir()
		rh := rvOptionStrategy("rh-wheel-spy", "robinhood", "")
		rh["args"] = []string{"wheel", "SPY", "--platform=robinhood", "--mode=paper"}
		path, _ := rvWriteConfig(t, dir, rvBaseConfig(dir, CurrentConfigVersion, rvOptionStrategy("ibkr-wheel-btc", "ibkr", ""), rh))
		cfg, err := LoadConfigReadOnly(path)
		if err != nil {
			t.Fatalf("supported options venues must load: %v", err)
		}
		got := map[string]string{}
		for _, sc := range cfg.Strategies {
			got[sc.ID] = sc.Platform
		}
		if got["ibkr-wheel-btc"] != "ibkr" || got["rh-wheel-spy"] != "robinhood" {
			t.Fatalf("platforms = %v", got)
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(root map[string]interface{})
		want   string
	}{
		{"deribit_platform_field", func(root map[string]interface{}) {
			root["strategies"] = []map[string]interface{}{rvOptionStrategy("opt-vol-btc", "deribit", "")}
		}, `strategy[opt-vol-btc]: platform "deribit" is retired`},
		{"deribit_id_prefix_with_supported_platform", func(root map[string]interface{}) {
			root["strategies"] = []map[string]interface{}{rvOptionStrategy("deribit-vol-btc", "ibkr", "")}
		}, `strategy[deribit-vol-btc]: the strategy id prefix "deribit-" is retired`},
		{"deribit_argument_before_a_supported_argument", func(root map[string]interface{}) {
			sc := rvOptionStrategy("ibkr-vol-btc", "ibkr", "")
			sc["args"] = []string{"wheel", "BTC", "--platform=deribit", "--platform=ibkr"}
			root["strategies"] = []map[string]interface{}{sc}
		}, `strategy[ibkr-vol-btc]: the script argument --platform=deribit is retired`},
		{"deribit_two_token_argument", func(root map[string]interface{}) {
			sc := rvOptionStrategy("ibkr-vol-btc", "ibkr", "")
			sc["args"] = []string{"wheel", "BTC", "--platform", "deribit"}
			root["strategies"] = []map[string]interface{}{sc}
		}, `the script argument --platform=deribit is retired`},
		{"deribit_platform_settings_key", func(root map[string]interface{}) {
			root["platforms"] = map[string]interface{}{"deribit": map[string]interface{}{"risk": map[string]interface{}{"max_drawdown_pct": 30}}}
		}, `config platforms: the settings key "deribit" is retired`},
		{"okx_options_platform_field", func(root map[string]interface{}) {
			root["strategies"] = []map[string]interface{}{rvOptionStrategy("opt-okx-btc", "okx", "")}
		}, `strategy[opt-okx-btc]: options on platform "okx" is retired`},
		{"okx_options_id_prefix_without_platform", func(root map[string]interface{}) {
			sc := rvOptionStrategy("okx-opt-btc", "", "")
			sc["args"] = []string{"wheel", "BTC", "--mode=paper"}
			root["strategies"] = []map[string]interface{}{sc}
		}, `strategy[okx-opt-btc]: an options strategy with the "okx-" id prefix and no platform field is retired`},
		{"okx_options_argument", func(root map[string]interface{}) {
			sc := rvOptionStrategy("ibkr-opt-btc", "ibkr", "")
			sc["args"] = []string{"wheel", "BTC", "--platform=okx"}
			root["strategies"] = []map[string]interface{}{sc}
		}, `strategy[ibkr-opt-btc]: the options script argument --platform=okx is retired`},
		{"options_without_platform_ibkr_id", func(root map[string]interface{}) {
			root["strategies"] = []map[string]interface{}{rvOptionStrategy("ibkr-opt-btc", "", "")}
		}, `strategy[ibkr-opt-btc]: an options strategy needs an explicit platform field`},
		{"options_without_platform_rh_id", func(root map[string]interface{}) {
			sc := rvOptionStrategy("rh-opt-spy", "", "")
			sc["args"] = []string{"wheel", "SPY", "--mode=paper"}
			root["strategies"] = []map[string]interface{}{sc}
		}, `strategy[rh-opt-spy]: an options strategy needs an explicit platform field`},
		{"options_with_blank_platform", func(root map[string]interface{}) {
			sc := rvOptionStrategy("opt-blank", "ibkr", "")
			sc["platform"] = "   "
			root["strategies"] = []map[string]interface{}{sc}
		}, `strategy[opt-blank]: an options strategy needs an explicit platform field`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			root := rvBaseConfig(dir, 19, rvOptionStrategy("ibkr-keep-btc", "ibkr", ""))
			tc.mutate(root)
			path, blob := rvWriteConfig(t, dir, root)
			before := rvDirEntries(t, dir)
			loaders := map[string]func() error{
				"writable":        func() error { _, err := LoadConfig(path); return err },
				"read_only":       func() error { _, err := LoadConfigReadOnly(path); return err },
				"probe":           func() error { _, err := LoadConfigForProbe(path); return err },
				"probe_admission": func() error { _, err := LoadConfigForProbeAdmission(path, true); return err },
				"ledger_export":   func() error { _, err := LoadConfigForLedgerExport(blob); return err },
			}
			names := make([]string, 0, len(loaders))
			for name := range loaders {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				err := loaders[name]()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s load: err = %v, want %q", name, err, tc.want)
				}
				if strings.Contains(tc.want, "is retired") && !strings.Contains(err.Error(), retiredOptionVenueRecovery) {
					t.Fatalf("%s load: the refusal must give the recovery steps: %v", name, err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, blob) {
				t.Fatalf("a refused migration-eligible config was rewritten")
			}
			if got := rvDirEntries(t, dir); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Fatalf("a refused config load created files: before %v after %v", before, got)
			}
		})
	}
}

func TestOptionsCheckDispatchArgsIntegration(t *testing.T) {
	ibkr := StrategyConfig{ID: "ibkr-wheel-btc", Type: "options", Platform: "ibkr", Script: "shared_scripts/check_options.py",
		Args: []string{"wheel", "BTC", "--mode=paper"}}
	rh := StrategyConfig{ID: "rh-wheel-spy", Type: "options", Platform: "robinhood", Script: "shared_scripts/check_options.py",
		Args: []string{"wheel", "SPY", "--mode=paper"}}
	for _, sc := range []StrategyConfig{ibkr, rh} {
		got, err := optionsCheckDispatchArgs(sc)
		want := append(append([]string{}, sc.Args...), "--platform="+sc.Platform)
		if err != nil || strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("%s injected args = %v err=%v, want %v", sc.ID, got, err, want)
		}
		if len(sc.Args) != 3 {
			t.Fatalf("%s: injection changed the configured args %v", sc.ID, sc.Args)
		}
		matching := sc
		matching.Args = append(append([]string{}, sc.Args...), "--platform="+sc.Platform)
		if got, err := optionsCheckDispatchArgs(matching); err != nil || strings.Join(got, " ") != strings.Join(matching.Args, " ") {
			t.Fatalf("%s matching flag args = %v err=%v", sc.ID, got, err)
		}
	}
	for _, tc := range []struct {
		name string
		sc   StrategyConfig
		want string
	}{
		{"conflicting_flag", func() StrategyConfig { s := ibkr; s.Args = []string{"wheel", "BTC", "--platform=robinhood"}; return s }(), "conflicts with the declared platform"},
		{"duplicate_flag", func() StrategyConfig {
			s := ibkr
			s.Args = []string{"wheel", "BTC", "--platform=ibkr", "--platform=ibkr"}
			return s
		}(), "passes 2 --platform flags"},
		{"bare_flag", func() StrategyConfig { s := ibkr; s.Args = []string{"wheel", "BTC", "--platform", "ibkr"}; return s }(), "malformed --platform flag"},
		{"empty_value", func() StrategyConfig { s := ibkr; s.Args = []string{"wheel", "BTC", "--platform="}; return s }(), "malformed --platform flag"},
		{"padded_value", func() StrategyConfig { s := ibkr; s.Args = []string{"wheel", "BTC", "--platform= ibkr"}; return s }(), "malformed --platform flag"},
		{"retired_declared_platform", func() StrategyConfig { s := ibkr; s.Platform = "deribit"; return s }(), "has no option pricer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := optionsCheckDispatchArgs(tc.sc); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("args=%v err=%v, want %q", got, err, tc.want)
			}
			sc := tc.sc
			sc.Script = filepath.Join(t.TempDir(), "absent", "check_options.py")
			logger, buf := ocLogger(sc.ID)
			result, sig, ok := runOptionsCheck(sc, "[]", nil, logger)
			if ok || result != nil || sig != "" || !strings.Contains(buf.String(), "refused before spawn") || strings.Contains(buf.String(), "Script failed") {
				t.Fatalf("a refused flag must stop before the child runs: ok=%v log:\n%s", ok, buf.String())
			}
		})
	}
	t.Run("custom_checker_keeps_its_arguments", func(t *testing.T) {
		sc := ibkr
		sc.Script = "shared_scripts/my_options_checker.py"
		sc.Args = []string{"wheel", "BTC", "--platform", "venue-a", "--platform=venue-b"}
		got, err := optionsCheckDispatchArgs(sc)
		if err != nil || strings.Join(got, " ") != strings.Join(sc.Args, " ") {
			t.Fatalf("custom args = %v err=%v, want unchanged %v", got, err, sc.Args)
		}
	})
}

func TestOptionFeeVenueRefusalIntegration(t *testing.T) {
	for _, tc := range []struct {
		platform string
		want     float64
		ok       bool
	}{
		{"ibkr", 2 * IBKROptionFeeFixed, true},
		{"robinhood", 2 * RobinhoodOptionFeeFixed, true},
		{"deribit", 0, false},
		{"okx", 0, false},
		{"", 0, false},
	} {
		fee, err := CalculateOptionFee(tc.platform, 2)
		if (err == nil) != tc.ok || fee != tc.want {
			t.Fatalf("%q fee = %v err=%v, want %v ok=%v", tc.platform, fee, err, tc.want, tc.ok)
		}
	}

	structure := []OptionsAction{
		{Action: "buy", OptionType: "call", Strike: 57000, Expiry: ocExpiry, PremiumUSD: 100},
		{Action: "sell", OptionType: "call", Strike: 60000, Expiry: ocExpiry, PremiumUSD: 100, Quantity: 2},
		{Action: "buy", OptionType: "call", Strike: 63000, Expiry: ocExpiry, PremiumUSD: 100},
	}
	t.Run("unsupported_venue_without_a_contract_size_books_nothing", func(t *testing.T) {
		for _, platform := range []string{"deribit", "okx"} {
			sc := ocSC("opt-"+platform+"-structure", platform, "BTC")
			env := ocEnv(t, sc)
			s := env.strat(sc.ID)
			logger, _ := ocLogger(sc.ID)
			n, err := ExecuteOptionsSignal(s, ocResult(platform, "BTC", 60000, structure...), logger)
			if err == nil || n != 0 || s.Cash != sc.Capital || len(s.OptionPositions) != 0 || len(s.TradeHistory) != 0 {
				t.Fatalf("%s: trades=%d err=%v cash=%v legs=%d history=%d", platform, n, err, s.Cash, len(s.OptionPositions), len(s.TradeHistory))
			}
		}
	})
	t.Run("venue_with_a_contract_size_but_no_fee_schedule_books_no_leg", func(t *testing.T) {
		optionContractSpecs["kraken"] = optionContractSpec{Platform: "kraken", Multiplier: 1, LegacyMultiplier: 1, Settlement: optionSettlementCash}
		t.Cleanup(func() { delete(optionContractSpecs, "kraken") })
		sc := ocSC("opt-kraken-structure", "kraken", "BTC")
		env := ocEnv(t, sc)
		s := env.strat(sc.ID)
		s.Cash = 100000
		env.saveAll()
		logger, buf := ocLogger(sc.ID)
		n, err := ExecuteOptionsSignal(s, ocResult("kraken", "BTC", 60000, structure...), logger)
		if err != nil || n != 0 || s.Cash != 100000 || len(s.OptionPositions) != 0 || len(s.TradeHistory) != 0 {
			t.Fatalf("trades=%d err=%v cash=%v legs=%d history=%d; log:\n%s", n, err, s.Cash, len(s.OptionPositions), len(s.TradeHistory), buf.String())
		}
		if !strings.Contains(buf.String(), "fee refused") || !strings.Contains(buf.String(), "none of its 3 leg(s) book") {
			t.Fatalf("the refusal must name the fee and the whole structure:\n%s", buf.String())
		}
		env.saveAll()
		if got := len(ocStoredTrades(t, env, sc.ID)); got != 0 {
			t.Fatalf("stored trades = %d, want 0", got)
		}
	})
}

type rvStorageLayout struct {
	name   string
	source string
	root   func(dir string) map[string]interface{}
}

func rvStorageLayouts() []rvStorageLayout {
	return []rvStorageLayout{
		{name: "primary", root: func(string) map[string]interface{} { return nil }},
		{name: "paper", root: func(dir string) map[string]interface{} {
			return map[string]interface{}{"paper_db_file": filepath.Join(dir, "paper.db")}
		}},
		{name: "named_source", source: "lab", root: func(dir string) map[string]interface{} {
			return map[string]interface{}{"paper_sources": []map[string]interface{}{{"id": "lab", "label": "Lab", "db_file": filepath.Join(dir, "lab.db")}}}
		}},
	}
}

type rvStoredBook struct {
	id, typ, platform string
	positions         bool
	options           bool
}

func (b rvStoredBook) stmts() []string {
	out := []string{"INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES ('" + b.id + "', '" + b.typ + "', '" + b.platform + "', 777, 1000)"}
	if b.positions {
		out = append(out, "INSERT INTO positions (strategy_id, symbol, quantity, avg_cost, side) VALUES ('"+b.id+"', 'BTC', 0.5, 60000, 'long')")
	}
	if b.options {
		out = append(out, "INSERT INTO option_positions (strategy_id, id, underlying, option_type, strike, expiry, action, quantity, contract_multiplier) VALUES ('"+b.id+"', 'BTC-put-sell-50000-2099-01-16', 'BTC', 'put', 50000, '2099-01-16', 'sell', 1, 1)")
	}
	return out
}

type rvBookRows struct {
	cash              float64
	positions, legs   int
	strategyRowExists bool
}

func rvReadBook(t *testing.T, path, id string) rvBookRows {
	t.Helper()
	db, err := openStateDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out rvBookRows
	err = db.db.QueryRow("SELECT cash FROM strategies WHERE id = ?", id).Scan(&out.cash)
	out.strategyRowExists = err == nil
	if err := db.db.QueryRow("SELECT COUNT(*) FROM positions WHERE strategy_id = ?", id).Scan(&out.positions); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow("SELECT COUNT(*) FROM option_positions WHERE strategy_id = ?", id).Scan(&out.legs); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRetiredOptionVenueStorageIntegration(t *testing.T) {
	cases := []struct {
		name   string
		book   rvStoredBook
		reuse  bool
		refuse bool
	}{
		{name: "deribit_option_legs_without_a_config_entry", book: rvStoredBook{id: "der-gone", typ: "options", platform: "deribit", options: true}, refuse: true},
		{name: "deribit_regular_leg_only_without_a_config_entry", book: rvStoredBook{id: "der-held-btc", typ: "options", platform: "deribit", positions: true}, refuse: true},
		{name: "okx_options_option_legs", book: rvStoredBook{id: "okx-opt-gone", typ: "options", platform: "okx", options: true}, refuse: true},
		{name: "okx_options_regular_leg_only", book: rvStoredBook{id: "okx-opt-held", typ: "options", platform: "okx", positions: true}, refuse: true},
		{name: "legacy_blank_platform_options_book", book: rvStoredBook{id: "opt-legacy", typ: "options", platform: "", options: true}, refuse: true},
		{name: "reused_id_now_mapped_to_ibkr", book: rvStoredBook{id: "reused-opt", typ: "options", platform: "deribit", options: true, positions: true}, reuse: true, refuse: true},
		{name: "flat_retired_book", book: rvStoredBook{id: "der-flat", typ: "options", platform: "deribit"}},
		{name: "okx_spot_book_is_not_retired", book: rvStoredBook{id: "okx-spot-btc", typ: "spot", platform: "okx", positions: true}},
	}
	for _, l := range rvStorageLayouts() {
		for _, tc := range cases {
			t.Run(l.name+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				strategies := []map[string]interface{}{rvOptionStrategy("ibkr-keep-btc", "ibkr", l.source)}
				if tc.reuse {
					strategies = append(strategies, rvOptionStrategy(tc.book.id, "ibkr", l.source))
				}
				root := rvBaseConfig(dir, CurrentConfigVersion, strategies...)
				for k, v := range l.root(dir) {
					root[k] = v
				}
				path, _ := rvWriteConfig(t, dir, root)
				cfg, err := LoadConfigReadOnly(path)
				if err != nil {
					t.Fatalf("config: %v", err)
				}
				layout, err := resolveStorageLayout(cfg)
				if err != nil {
					t.Fatal(err)
				}
				ident, err := buildStorageIdentityMap(cfg, layout)
				if err != nil {
					t.Fatal(err)
				}
				owner, ok := ident.storageFor("ibkr-keep-btc")
				if !ok {
					t.Fatal("no storage identity for the kept strategy")
				}
				var file string
				for _, spec := range layout.Files {
					if spec.Role == owner.Role {
						file = spec.Path
					}
				}
				seed, err := OpenStateDB(file)
				if err != nil {
					t.Fatal(err)
				}
				for _, stmt := range tc.book.stmts() {
					if _, err := seed.db.Exec(stmt); err != nil {
						t.Fatalf("%s: %v", stmt, err)
					}
				}
				seed.Close()
				before := rvReadBook(t, file, tc.book.id)

				si, err := inspectStorageOwnership(layout, ident, cfg, false)
				if err != nil {
					t.Fatal(err)
				}
				if !tc.refuse {
					if !si.OK() {
						t.Fatalf("a %s must not block startup: %v", tc.name, si.Rejections)
					}
					store, err := openStateStoreWith(layout, ident, openStateDBFromTemplate)
					if err != nil {
						t.Fatal(err)
					}
					defer store.Close()
					state, _, err := LoadStateWithStore(cfg, store)
					if err != nil {
						t.Fatalf("load: %v", err)
					}
					for part, err := range store.SaveAll(state) {
						if err != nil {
							t.Fatalf("first normal save (%s): %v", partitionLabel(part), err)
						}
					}
					return
				}
				joined := strings.Join(si.Rejections, "\n")
				if si.OK() || !strings.Contains(joined, rvQuote(tc.book.id)) || !strings.Contains(joined, "retired venue") || !strings.Contains(joined, retiredOptionVenueRecovery) {
					t.Fatalf("startup inspection must refuse the retired book %s:\n%s", tc.book.id, joined)
				}
				if tc.reuse && !strings.Contains(joined, "cannot take over the book") {
					t.Fatalf("a reused id must be named as a takeover:\n%s", joined)
				}

				store, err := openStateStoreWith(layout, ident, openStateDBFromTemplate)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				state := NewAppState()
				for _, sc := range cfg.Strategies {
					state.Strategies[sc.ID] = NewStrategyState(sc)
				}
				refused := 0
				for _, err := range store.SaveAll(state) {
					if err != nil && strings.Contains(err.Error(), "refusing the save") && strings.Contains(err.Error(), tc.book.id) {
						refused++
					}
				}
				if refused == 0 {
					t.Fatalf("a full save must refuse to drop or replace the retired book %s", tc.book.id)
				}
				if tc.reuse {
					if err := store.SaveStrategyBook(state.Strategies[tc.book.id]); err == nil || !strings.Contains(err.Error(), "refusing the save") {
						t.Fatalf("a single-book save over the retired book: err = %v", err)
					}
				} else if err := store.SaveStrategyBook(state.Strategies["ibkr-keep-btc"]); err != nil {
					t.Fatalf("a single-book save of an unrelated supported book must proceed: %v", err)
				}
				if after := rvReadBook(t, file, tc.book.id); after != before || !after.strategyRowExists || after.cash != 777 {
					t.Fatalf("refused saves changed the retired book: before %+v after %+v", before, after)
				}
			})
		}
	}

	t.Run("in_memory_retired_book_is_refused", func(t *testing.T) {
		dir := t.TempDir()
		path, _ := rvWriteConfig(t, dir, rvBaseConfig(dir, CurrentConfigVersion, rvOptionStrategy("ibkr-keep-btc", "ibkr", "")))
		cfg, err := LoadConfigReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		env := openEntryTestEnv(t, cfg)
		s := env.strat("ibkr-keep-btc")
		s.Platform = "deribit"
		esLeg(s, "BTC", "put", "sell", 50000, 1, 1, 100)
		for _, err := range env.store.SaveAll(env.state) {
			if err == nil || !strings.Contains(err.Error(), `retired venue "deribit"`) {
				t.Fatalf("save err = %v", err)
			}
		}
		if got := rvReadBook(t, cfg.DBFile, "ibkr-keep-btc"); got.legs != 0 {
			t.Fatalf("a refused save wrote %d legs", got.legs)
		}
	})
}

func rvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
