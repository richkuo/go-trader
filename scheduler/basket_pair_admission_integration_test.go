package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type legacyBasketContractV1 struct {
	Version                  int                        `json:"v"`
	BasketID                 string                     `json:"basket_id"`
	Partition                string                     `json:"partition"`
	Ranker                   string                     `json:"ranker"`
	Params                   map[string]interface{}     `json:"params,omitempty"`
	RebalanceIntervalSeconds int                        `json:"rebalance_interval_seconds"`
	EvaluationWindowSeconds  int                        `json:"evaluation_window_seconds"`
	RankerTimeoutSeconds     int                        `json:"ranker_timeout_seconds"`
	LookbackBars             int                        `json:"lookback_bars"`
	GrossBudgetUSD           float64                    `json:"gross_budget_usd"`
	MaxTransitionNetUSD      float64                    `json:"max_transition_net_usd"`
	MaxCoinNotionalUSD       float64                    `json:"max_coin_notional_usd"`
	MinEligibleMembers       int                        `json:"min_eligible_members"`
	HoldingHorizonSeconds    int64                      `json:"holding_horizon_seconds,omitempty"`
	EligibilitySource        string                     `json:"eligibility_source"`
	EligibilityVersion       string                     `json:"eligibility_version"`
	MinHistoryBars           int                        `json:"min_history_bars"`
	Listings                 map[string]basketListingMs `json:"listings"`
	Members                  []basketContractMember     `json:"members"`
}

func TestBasketPairLeavesVersionOneBasketContractsUnchanged(t *testing.T) {
	cfg := loadBasketTestConfig(t, nil)
	c, err := buildBasketContract(cfg, cfg.Baskets[0])
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacyBasketContractV1{Version: c.Version, BasketID: c.BasketID, Partition: c.Partition, Ranker: c.Ranker, Params: c.Params,
		RebalanceIntervalSeconds: c.RebalanceIntervalSeconds, EvaluationWindowSeconds: c.EvaluationWindowSeconds, RankerTimeoutSeconds: c.RankerTimeoutSeconds,
		LookbackBars: c.LookbackBars, GrossBudgetUSD: c.GrossBudgetUSD, MaxTransitionNetUSD: c.MaxTransitionNetUSD, MaxCoinNotionalUSD: c.MaxCoinNotionalUSD,
		MinEligibleMembers: c.MinEligibleMembers, HoldingHorizonSeconds: c.HoldingHorizonSeconds, EligibilitySource: c.EligibilitySource,
		EligibilityVersion: c.EligibilityVersion, MinHistoryBars: c.MinHistoryBars, Listings: c.Listings, Members: c.Members}
	want, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if c.JSON() != string(want) || c.Fingerprint() != sha256Hex(want) {
		t.Fatalf("a version-1 basket contract changed bytes:\n got %s\nwant %s", c.JSON(), want)
	}
	parsed, err := parseBasketContract(string(want))
	if err != nil || parsed.isPair() || parsed.Fingerprint() != c.Fingerprint() {
		t.Fatalf("a saved version-1 basket contract must parse unchanged: %v", err)
	}
	var forged map[string]interface{}
	_ = json.Unmarshal(want, &forged)
	forged["pair_v"] = 1
	blob, _ := json.Marshal(forged)
	if _, err := parseBasketContract(string(blob)); err == nil {
		t.Fatal("a basket contract carrying pair fields without the pair mode must be refused")
	}
}

func TestBasketPairAdmitsIsolatedPaperContracts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(root map[string]interface{})
		part   RiskPartition
	}{
		{"default paper", nil, defaultPaperPartition},
		{"split store", func(root map[string]interface{}) {
			dir := filepath.Dir(root["db_file"].(string))
			root["paper_db_file"] = filepath.Join(dir, "paper.db")
		}, defaultPaperPartition},
		{"paper source", func(root map[string]interface{}) {
			dir := filepath.Dir(root["db_file"].(string))
			root["paper_sources"] = []interface{}{map[string]interface{}{"id": "xv", "db_file": filepath.Join(dir, "xv.db")}}
			for _, id := range []string{pairTestHL, pairTestOKX} {
				pairTestStrategy(root, id)["paper_source"] = "xv"
			}
		}, paperSourcePartition("xv")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPairTestEnv(t, tc.mutate)
			rt := e.rt()
			if !rt.isPair() || rt.Partition != tc.part || len(rt.Contract.pairMembersSorted()) != 2 {
				t.Fatalf("the admitted pair runtime is %+v", rt.Contract)
			}
			keys := map[string]bool{}
			for _, m := range rt.Contract.pairMembersSorted() {
				keys[rt.Contract.pairMemberKey(m).String()] = true
			}
			if !keys[tc.part.String()+"|okx|synthetic:okx-research|BTC-USDT-SWAP|"+pairTestOKX] || !keys[tc.part.String()+"|hyperliquid|synthetic:hl-research|BTC|"+pairTestHL] {
				t.Fatalf("the member keys must bind partition, provider, account, native instrument and member: %v", keys)
			}
			pairEnterHoldRunner(e, 0.06)
			e.slotStep(0)
			e.slotStep(1)
			e.assertBooks("admitted pair runs", 0.06, -0.06)
			blob, err := os.ReadFile(filepath.Join(e.dir, pairResearchManifestName))
			if err != nil {
				t.Fatal(err)
			}
			var m pairResearchManifest
			if err := json.Unmarshal(blob, &m); err != nil || m.ContractFP != rt.Contract.Fingerprint() || m.RunID != "run-1" {
				t.Fatalf("the research manifest must bind the run and contract: %s", blob)
			}
			e.store.Close()
			adm, _, err := newPairResearchAdmission(e.doc, "run-1", e.dir, true, pairResearchDeps{VenueFor: e.venues.factory()})
			if err != nil {
				t.Fatalf("a resume of the same run and contract must be admitted: %v", err)
			}
			adm.release()
			if _, _, err := newPairResearchAdmission(e.doc, "run-2", e.dir, true, pairResearchDeps{VenueFor: e.venues.factory()}); err == nil {
				t.Fatal("a resume under another run id must be refused")
			}
			if _, _, err := newPairResearchAdmission(e.doc, "run-1", e.dir, false, pairResearchDeps{VenueFor: e.venues.factory()}); err == nil {
				t.Fatal("a new run in a used state directory must be refused")
			}
		})
	}
}

func pairAdmitRefused(t *testing.T, mutate func(root map[string]interface{}), want string) {
	t.Helper()
	registerPairTestPolicy(t)
	dir := filepath.Join(t.TempDir(), "run")
	doc := pairTestDoc(t, dir, mutate)
	venues := newPairTestVenues(false)
	_, _, err := newPairResearchAdmission(doc, "run-1", dir, false, pairResearchDeps{VenueFor: venues.factory()})
	if err == nil {
		t.Fatalf("the research admission accepted a contract that must be refused (%s)", want)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("a refused admission wrote state into %s (%v)", dir, statErr)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("the refusal must name %q, got %v", want, err)
	}
}

func TestBasketPairAdmissionRefusesBadContracts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(root map[string]interface{})
		want   string
	}{
		{"missing member", func(r map[string]interface{}) {
			p := pairTestPair(r)
			p["members"] = p["members"].([]interface{})[:1]
		}, "exactly two native members"},
		{"duplicate member", func(r map[string]interface{}) {
			p := pairTestPair(r)
			ms := p["members"].([]interface{})
			p["members"] = []interface{}{ms[0], ms[0]}
		}, "listed twice"},
		{"two members on one venue", func(r map[string]interface{}) {
			sc := pairTestStrategy(r, pairTestOKX)
			sc["platform"], sc["script"] = "hyperliquid", "shared_scripts/check_hyperliquid.py"
		}, "two members use the hyperliquid venue"},
		{"mixed partitions", func(r map[string]interface{}) {
			dir := filepath.Dir(r["db_file"].(string))
			r["paper_sources"] = []interface{}{map[string]interface{}{"id": "xv", "db_file": filepath.Join(dir, "xv.db")}}
			pairTestStrategy(r, pairTestOKX)["paper_source"] = "xv"
		}, "members span risk partitions"},
		{"conflicting standalone owner", func(r map[string]interface{}) {
			r["strategies"] = append(r["strategies"].([]interface{}), map[string]interface{}{
				"id": "hl-other-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
				"args": []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper"}, "capital": 1000,
			})
		}, "owns exclusively"},
		{"shared venue account and instrument", func(r map[string]interface{}) {
			pairTestMemberEntry(r, 1)["account_key"] = "synthetic:hl-research"
			pairTestMemberEntry(r, 1)["instrument"] = "BTC"
		}, "instrument"},
		{"unsupported okx mapping", func(r map[string]interface{}) {
			pairTestMemberEntry(r, 1)["instrument"] = "BTC-USD-SWAP"
		}, "must be the OKX linear swap"},
		{"okx long-short mode", func(r map[string]interface{}) {
			pairTestMemberEntry(r, 1)["meta"].(map[string]interface{})["position_mode"] = "long_short"
		}, "net position mode"},
		{"inactive instrument", func(r map[string]interface{}) {
			pairTestMemberEntry(r, 0)["meta"].(map[string]interface{})["trading_status"] = "suspended"
		}, "trading_status"},
		{"non-synthetic account", func(r map[string]interface{}) {
			pairTestMemberEntry(r, 0)["account_key"] = "hyperliquid:mainnet:0xabc"
		}, "synthetic research account"},
		{"different underlying", func(r map[string]interface{}) {
			pairTestStrategy(r, pairTestOKX)["args"] = []interface{}{"pair_member", "ETH", "1h", "--mode=paper", "--inst-type=swap"}
		}, "must be the pair underlying"},
		{"open evaluator", func(r map[string]interface{}) {
			pairTestStrategy(r, pairTestHL)["open_strategy"] = map[string]interface{}{"name": "sma_crossover"}
		}, "must not set open_strategy"},
		{"positional evaluator", func(r map[string]interface{}) {
			pairTestStrategy(r, pairTestHL)["args"] = []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper"}
		}, "pair_member"},
		{"open-strategy close fallback", func(r map[string]interface{}) {
			pairTestStrategy(r, pairTestHL)["close_strategy"] = map[string]interface{}{"name": "sma_crossover"}
		}, "open-strategy close fallback"},
		{"scale-in", func(r map[string]interface{}) { pairTestStrategy(r, pairTestHL)["allow_scale_in"] = true }, "allow_scale_in"},
		{"inversion", func(r map[string]interface{}) { pairTestStrategy(r, pairTestOKX)["invert_signal"] = true }, "invert_signal"},
		{"hedge", func(r map[string]interface{}) {
			pairTestStrategy(r, pairTestHL)["hedge"] = map[string]interface{}{"enabled": true, "symbol": "ETH"}
		}, "hedge"},
		{"replay mirror", func(r map[string]interface{}) { pairTestStrategy(r, pairTestHL)["replay_sharing"] = "mirror" }, "replay_sharing"},
		{"entry gate", func(r map[string]interface{}) { pairTestStrategy(r, pairTestHL)["htf_filter"] = true }, "htf_filter"},
		{"independent sizing", func(r map[string]interface{}) { pairTestStrategy(r, pairTestOKX)["risk_per_trade_pct"] = 1.0 }, "risk_per_trade_pct"},
		{"closed-bar decisions", func(r map[string]interface{}) { pairTestStrategy(r, pairTestHL)["closed_bar_decisions"] = true }, "closed_bar_decisions"},
		{"mixed basket membership", func(r map[string]interface{}) { pairTestStrategy(r, pairTestHL)["basket_id"] = "xs" }, "basket"},
		{"okx without a stop owner", func(r map[string]interface{}) {
			pairTestStrategy(r, pairTestOKX)["close_strategy"] = map[string]interface{}{"name": "tiered_tp_pct", "params": map[string]interface{}{"tp_tiers": []interface{}{map[string]interface{}{"profit_pct": 0.5, "close_fraction": 1.0}}}}
		}, "resolvable stop owner"},
		{"unknown policy", func(r map[string]interface{}) { pairTestPair(r)["policy"] = "spread_magic" }, "not a registered pair policy"},
		{"unknown policy parameter", func(r map[string]interface{}) {
			pairTestPair(r)["params"] = map[string]interface{}{"entry_bps": 5, "boost": 2}
		}, `unknown field "boost" in params`},
		{"unknown pair key", func(r map[string]interface{}) { pairTestPair(r)["mode"] = "live" }, `unknown field "mode"`},
		{"unknown meta key", func(r map[string]interface{}) {
			pairTestMemberEntry(r, 0)["meta"].(map[string]interface{})["fees"] = 1
		}, `unknown field "fees"`},
		{"baskets beside the pair", func(r map[string]interface{}) {
			r["baskets"] = []interface{}{map[string]interface{}{"id": pairTestID}}
		}, "basket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { pairAdmitRefused(t, tc.mutate, tc.want) })
	}
}

func TestBasketPairConfigLimitRulesAtTheirBoundaries(t *testing.T) {
	grace := entryRecoveryGrace.Milliseconds()
	set := func(field string, v interface{}) func(r map[string]interface{}) {
		return func(r map[string]interface{}) { pairTestPair(r)["limits"].(map[string]interface{})[field] = v }
	}
	setRaw := func(field, raw string) func(r map[string]interface{}) {
		return func(r map[string]interface{}) {
			pairTestPair(r)["limits"].(map[string]interface{})[field] = json.RawMessage(raw)
		}
	}
	both := func(fs ...func(r map[string]interface{})) func(r map[string]interface{}) {
		return func(r map[string]interface{}) {
			for _, f := range fs {
				f(r)
			}
		}
	}
	accepted := []struct {
		name   string
		mutate func(r map[string]interface{})
	}{
		{"venue cap equal to the gross budget", set("okx_notional_cap_usd", 12000)},
		{"unmatched cap equal to the gross budget", set("unmatched_cap_usd", 12000)},
		{"timing at 1", both(set("decision_cadence_ms", 1), set("retry_initial_ms", 1))},
		{"timing at the maximum", both(set("max_hold_age_ms", 2147483647), set("recovery_age_ms", 2147483647))},
		{"retry initial equal to retry max", set("retry_initial_ms", 40000)},
		{"recovery age equal to the deadline plus grace", set("recovery_age_ms", 30000+grace)},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			registerPairTestPolicy(t)
			dir := filepath.Join(t.TempDir(), "run")
			adm, _, err := newPairResearchAdmission(pairTestDoc(t, dir, tc.mutate), "run-1", dir, false, pairResearchDeps{VenueFor: newPairTestVenues(false).factory()})
			if err != nil {
				t.Fatalf("the boundary value must pass: %v", err)
			}
			adm.release()
		})
	}
	refused := []struct {
		name   string
		mutate func(r map[string]interface{})
		want   string
	}{
		{"venue cap just above the gross budget", set("okx_notional_cap_usd", math.Nextafter(12000, math.Inf(1))), "exceeds gross_budget_usd"},
		{"zero dollar field", set("gross_budget_usd", 0), "gross_budget_usd must be a finite number above zero"},
		{"timing zero", set("decision_cadence_ms", 0), "decision_cadence_ms must be in [1, 2147483647]"},
		{"timing above the maximum", set("max_hold_age_ms", int64(2147483648)), "max_hold_age_ms must be in [1, 2147483647]"},
		{"retry initial above retry max", set("retry_initial_ms", 40001), "retry_initial_ms 40001 exceeds retry_max_ms"},
		{"recovery age one millisecond short", set("recovery_age_ms", 30000+grace-1), "recovery_age_ms"},
		{"null", setRaw("gross_budget_usd", "null"), "gross_budget_usd must be a JSON number, got null"},
		{"boolean", setRaw("retry_max_ms", "true"), "retry_max_ms must be an integer number of milliseconds, got boolean"},
		{"string", setRaw("unmatched_cap_usd", `"100"`), "unmatched_cap_usd must be a JSON number, got string"},
		{"fraction", setRaw("acceptance_deadline_ms", "1.5"), "acceptance_deadline_ms must be an integer number of milliseconds"},
		{"exponent", setRaw("decision_cadence_ms", "1e3"), "decision_cadence_ms must be an integer number of milliseconds"},
		{"unknown key", set("quote_age_ms", 100), `unknown field "quote_age_ms"`},
		{"missing field", func(r map[string]interface{}) {
			delete(pairTestPair(r)["limits"].(map[string]interface{}), "retry_max_ms")
		}, "retry_max_ms is missing"},
		{"zero contract size", func(r map[string]interface{}) {
			pairTestMemberEntry(r, 1)["meta"].(map[string]interface{})["contract_size"] = 0
		}, "meta.contract_size must be a finite number above zero"},
		{"zero tick", func(r map[string]interface{}) {
			pairTestMemberEntry(r, 0)["meta"].(map[string]interface{})["tick_step"] = 0
		}, "meta.tick_step"},
		{"zero lot", func(r map[string]interface{}) {
			pairTestMemberEntry(r, 1)["meta"].(map[string]interface{})["lot_step"] = 0
		}, "meta.lot_step"},
		{"zero capital", func(r map[string]interface{}) { pairTestStrategy(r, pairTestHL)["capital"] = 0 }, "finite capital above zero"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) { pairAdmitRefused(t, tc.mutate, tc.want) })
	}
}

func TestBasketPairOrdinaryRoutesRefuseActivation(t *testing.T) {
	registerPairTestPolicy(t)
	variants := map[string]func(root map[string]interface{}){
		"root pairs": nil,
		"member pair_id only": func(r map[string]interface{}) {
			delete(r, "pairs")
		},
		"pair_member token only": func(r map[string]interface{}) {
			delete(r, "pairs")
			for _, id := range []string{pairTestHL, pairTestOKX} {
				delete(pairTestStrategy(r, id), "pair_id")
			}
		},
		"pair flag only": func(r map[string]interface{}) {
			delete(r, "pairs")
			for _, id := range []string{pairTestHL, pairTestOKX} {
				sc := pairTestStrategy(r, id)
				delete(sc, "pair_id")
				sc["args"] = []interface{}{"sma_crossover", "BTC", "1h", "--mode=paper", pairMemberFlag + "={}"}
			}
		},
		"paper with acknowledgement and an old config version": func(r map[string]interface{}) {
			r["config_version"] = 12
			for _, id := range []string{pairTestHL, pairTestOKX} {
				pairTestStrategy(r, id)["allow_no_edge"] = true
			}
		},
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			doc := pairTestDoc(t, dir, mutate)
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
				"forged admission": func() error {
					_, err := loadConfigDataWith("", doc, true, true, &pairResearchAdmission{nonce: "forged"})
					return err
				},
			}
			for lname, load := range loaders {
				if err := load(); err == nil {
					t.Fatalf("%s activated a pair reference", lname)
				}
			}
			if code := runProbe([]string{"--config", path}); code == 0 {
				t.Fatal("the probe command admitted a pair reference")
			}
			if after := dirDigest(t, dir); after != before {
				t.Fatal("an ordinary route changed the config or state files while refusing a pair")
			}
		})
	}
	if _, _, err := buildAddStrategyEntry(pairMemberStrategyToken, "hyperliquid", "BTC"); err == nil {
		t.Fatal("the operator add-strategy route accepted the pair member token")
	}
}

func TestBasketPairReleasedAdmissionAndReloadAreRefused(t *testing.T) {
	e := newPairTestEnv(t, nil)
	e.adm.release()
	if _, err := loadPairResearchConfig(e.adm, e.doc); err == nil {
		t.Fatal("a released research admission must not activate a pair again")
	}
	next := *e.cfg
	next.Pairs = append([]PairConfig(nil), e.cfg.Pairs...)
	next.Pairs[0].Params = map[string]interface{}{"entry_bps": 9}
	if err := validateHotReloadCompatible(e.cfg, &next); err == nil {
		t.Fatal("a reload that changes a pair contract must be refused")
	}
	stripped := *e.cfg
	stripped.Strategies = append([]StrategyConfig(nil), e.cfg.Strategies...)
	stripped.Strategies[0].PairID = ""
	if err := validateHotReloadCompatible(e.cfg, &stripped); err == nil {
		t.Fatal("a reload that removes a member's pair_id must be refused")
	}
}

func TestBasketPairOrdinaryLoadRefusesResearchRecords(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	e.store.Close()
	dbPath := e.cfg.DBFile
	before := pairFileDigest(t, dbPath)
	ordinary := loadBasketTestConfig(t, func(root map[string]interface{}) {
		root["db_file"] = dbPath
		delete(root, "baskets")
		var kept []interface{}
		for _, raw := range root["strategies"].([]interface{}) {
			sc := raw.(map[string]interface{})
			delete(sc, "basket_id")
			sc["args"] = []interface{}{"sma_crossover", sc["args"].([]interface{})[1], "1h", "--mode=paper"}
			delete(sc, "closed_bar_decisions")
			kept = append(kept, sc)
		}
		root["strategies"] = kept
	})
	store := openSourceStore(t, ordinary)
	state := NewAppState()
	for _, sc := range ordinary.Strategies {
		state.Strategies[sc.ID] = NewStrategyState(sc)
	}
	if _, err := store.LoadBaskets(state, ordinary); err == nil {
		t.Fatal("an ordinary load adopted research pair records")
	}
	var n int
	if err := store.primary().db.QueryRow(`SELECT COUNT(*) FROM basket_member_actions WHERE group_mode = 'pair'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the refused load changed the pair records: %d (%v)", n, err)
	}
	store.Close()
	_ = before
}
