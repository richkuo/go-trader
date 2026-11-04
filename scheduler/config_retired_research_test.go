package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func dirDigest(t *testing.T, dir string) string {
	t.Helper()
	var parts []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		parts = append(parts, path+"="+missingOrFileDigest(t, path))
		return nil
	})
	sort.Strings(parts)
	return sha256Hex([]byte(strings.Join(parts, "\n")))
}

func missingOrFileDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		t.Fatal(err)
	}
	return sha256Hex(b)
}

func retiredResearchPlainStrategy() map[string]interface{} {
	return map[string]interface{}{
		"id":       "hl-plain-btc",
		"type":     "perps",
		"platform": "hyperliquid",
		"script":   "shared_scripts/check_hyperliquid.py",
		"args":     []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper"},
		"capital":  1000,
	}
}

func TestRetiredResearchConfigRefusedOnEveryRoute(t *testing.T) {
	type variant struct {
		mutate func(root map[string]interface{}, sc map[string]interface{})
		want   string
	}
	variants := map[string]variant{
		"root baskets empty list": {func(r, _ map[string]interface{}) { r["baskets"] = []interface{}{} }, `config root: "baskets" belongs to the basket and pair research feature removed in issue 1955`},
		"root pairs null":         {func(r, _ map[string]interface{}) { r["pairs"] = nil }, `config root: "pairs" belongs to`},
		"strategy basket_id":      {func(_, sc map[string]interface{}) { sc["basket_id"] = "xs-test" }, `strategy[hl-plain-btc]: "basket_id" belongs to`},
		"strategy pair_id empty":  {func(_, sc map[string]interface{}) { sc["pair_id"] = "" }, `strategy[hl-plain-btc]: "pair_id" belongs to`},
		"basket_member token": {func(_, sc map[string]interface{}) {
			sc["args"] = []interface{}{"basket_member", "BTC", "1h", "--mode=paper"}
		}, `strategy[hl-plain-btc]: args[0] "basket_member" belongs to`},
		"pair_member token": {func(_, sc map[string]interface{}) {
			sc["args"] = []interface{}{"pair_member", "BTC", "1h", "--mode=paper"}
		}, `strategy[hl-plain-btc]: args[0] "pair_member" belongs to`},
		"pair_member open strategy object": {func(_, sc map[string]interface{}) {
			sc["open_strategy"] = map[string]interface{}{"name": "pair_member"}
		}, `strategy[hl-plain-btc]: open_strategy "pair_member" belongs to`},
		"basket_member open strategy string": {func(_, sc map[string]interface{}) {
			sc["open_strategy"] = "basket_member"
		}, `strategy[hl-plain-btc]: open_strategy "basket_member" belongs to`},
		"basket member flag": {func(_, sc map[string]interface{}) {
			sc["args"] = []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper", "--basket-member"}
		}, `strategy[hl-plain-btc]: the flag --basket-member belongs to`},
		"pair member flag with value": {func(_, sc map[string]interface{}) {
			sc["args"] = []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper", "--pair-member-json={}"}
		}, `strategy[hl-plain-btc]: the flag --pair-member-json belongs to`},
		"pair member flag bare": {func(_, sc map[string]interface{}) {
			sc["args"] = []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper", "--pair-member-json"}
		}, `strategy[hl-plain-btc]: the flag --pair-member-json belongs to`},
		"management stdin flag": {func(_, sc map[string]interface{}) {
			sc["args"] = []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper", "--management-stdin"}
		}, `strategy[hl-plain-btc]: the flag --management-stdin belongs to`},
	}
	names := make([]string, 0, len(variants))
	for name := range variants {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := variants[name]
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sc := retiredResearchPlainStrategy()
			root := map[string]interface{}{
				"config_version":   12,
				"interval_seconds": 3600,
				"db_file":          filepath.Join(dir, "state.db"),
				"strategies":       []interface{}{sc},
			}
			v.mutate(root, sc)
			doc, err := json.MarshalIndent(root, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, doc, 0o600); err != nil {
				t.Fatal(err)
			}
			before := dirDigest(t, dir)
			loaders := map[string]func() error{
				"LoadConfig":                func() error { _, err := LoadConfig(path); return err },
				"LoadConfigReadOnly":        func() error { _, err := LoadConfigReadOnly(path); return err },
				"LoadConfigForProbe":        func() error { _, err := LoadConfigForProbe(path); return err },
				"LoadConfigForLedgerExport": func() error { _, err := LoadConfigForLedgerExport(doc); return err },
				"feed consumer":             func() error { _, err := loadConsumerConfigForFeed(path); return err },
				"quote-admitted": func() error {
					_, err := loadQuoteAdmittedConfigData(doc, &quoteLoadAdmission{configDigest: quoteConfigDigest(doc)})
					return err
				},
			}
			loaderNames := make([]string, 0, len(loaders))
			for lname := range loaders {
				loaderNames = append(loaderNames, lname)
			}
			sort.Strings(loaderNames)
			for _, lname := range loaderNames {
				err := loaders[lname]()
				if err == nil {
					t.Fatalf("%s accepted a retired research key", lname)
				}
				msg := err.Error()
				if !strings.Contains(msg, v.want) || !strings.Contains(msg, "git tag archive/basket-pair-research") || !strings.HasPrefix(msg, "config validation errors:") {
					t.Fatalf("%s refusal must name the key and the archive tag, got: %s", lname, msg)
				}
			}
			if code := runProbe([]string{"--config", path}); code == 0 {
				t.Fatal("the probe command accepted a retired research key")
			}
			if after := dirDigest(t, dir); after != before {
				t.Fatal("a refusing route changed the config or state files")
			}
		})
	}
	t.Run("order is root keys then strategies in file order", func(t *testing.T) {
		a := retiredResearchPlainStrategy()
		a["id"] = "first"
		a["pair_id"] = "p"
		a["basket_id"] = "b"
		b := retiredResearchPlainStrategy()
		b["id"] = "second"
		b["args"] = []interface{}{"pair_member", "BTC", "1h", "--mode=paper", "--management-stdin"}
		doc, err := json.Marshal(map[string]interface{}{"pairs": []interface{}{}, "baskets": []interface{}{}, "strategies": []interface{}{a, b}})
		if err != nil {
			t.Fatal(err)
		}
		got := retiredResearchConfigRefusals(doc)
		var prefixes []string
		for _, line := range got {
			prefixes = append(prefixes, line[:strings.Index(line, " belongs to")])
		}
		want := []string{`config root: "baskets"`, `config root: "pairs"`, `strategy[first]: "basket_id"`, `strategy[first]: "pair_id"`, `strategy[second]: args[0] "pair_member"`, `strategy[second]: the flag --management-stdin`}
		if strings.Join(prefixes, "\n") != strings.Join(want, "\n") {
			t.Fatalf("refusal order:\n%s\nwant:\n%s", strings.Join(prefixes, "\n"), strings.Join(want, "\n"))
		}
		if retiredResearchConfigRefusals([]byte("{not json")) != nil {
			t.Fatal("malformed JSON is left to the later parse")
		}
	})
	t.Run("operator add-strategy route", func(t *testing.T) {
		for _, token := range []string{"basket_member", "pair_member", "cross_sectional_momentum", "cross_exchange_perp_arbitrage"} {
			if _, _, err := buildAddStrategyEntry(token, "hyperliquid", "BTC"); err == nil {
				t.Fatalf("the operator add-strategy route accepted %q", token)
			}
		}
	})
}
