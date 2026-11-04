package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type engineHoldLayout struct {
	name     string
	root     func(dir string) map[string]interface{}
	owner    func() map[string]interface{}
	ordinary func() map[string]interface{}
}

func engineHoldLayouts() []engineHoldLayout {
	paperMomentum := func() map[string]interface{} {
		sc := paperResearchMomentum()
		return sc
	}
	liveMomentum := func() map[string]interface{} {
		sc := paperResearchMomentum()
		sc["id"] = "hl-momentum-live"
		sc["args"] = []string{"momentum", "ETH", "1h", "--mode=live"}
		sc["allow_no_edge"] = true
		return sc
	}
	return []engineHoldLayout{
		{
			name:     "single_file",
			root:     func(dir string) map[string]interface{} { return map[string]interface{}{} },
			owner:    func() map[string]interface{} { return researchQuoteOwnerFixture("--mode=paper") },
			ordinary: paperMomentum,
		},
		{
			name: "split_file",
			root: func(dir string) map[string]interface{} {
				return map[string]interface{}{"paper_db_file": filepath.Join(dir, "paper.db")}
			},
			owner:    func() map[string]interface{} { return researchQuoteOwnerFixture("--mode=paper") },
			ordinary: liveMomentum,
		},
		{
			name: "paper_source",
			root: func(dir string) map[string]interface{} {
				return map[string]interface{}{"paper_sources": []map[string]interface{}{{"id": "lab", "label": "Lab", "db_file": filepath.Join(dir, "lab.db")}}}
			},
			owner: func() map[string]interface{} {
				sc := researchQuoteOwnerFixture("--mode=paper")
				sc["paper_source"] = "lab"
				return sc
			},
			ordinary: paperMomentum,
		},
	}
}

type engineHoldFixture struct {
	cfg    *Config
	layout storageLayout
	ident  storageIdentityMap
	owner  storageIdentity
	path   string
	other  string
}

func newEngineHoldFixture(t *testing.T, l engineHoldLayout) engineHoldFixture {
	t.Helper()
	setNoEdgeLiveCredentials(t)
	dir := t.TempDir()
	root := map[string]interface{}{
		"config_version":   CurrentConfigVersion,
		"interval_seconds": 60,
		"log_dir":          filepath.Join(dir, "logs"),
		"db_file":          filepath.Join(dir, "state.db"),
		"market_feed":      "websocket",
		"strategies":       []map[string]interface{}{l.owner(), l.ordinary()},
	}
	for k, v := range l.root(dir) {
		root[k] = v
	}
	blob, _ := json.Marshal(root)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigReadOnly(cfgPath)
	if err != nil {
		t.Fatalf("the held layout must load: %v", err)
	}
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := ident.storageFor("hl-invmm-btc")
	if !ok {
		t.Fatal("the owner has no storage identity")
	}
	f := engineHoldFixture{cfg: cfg, layout: layout, ident: ident, owner: owner}
	for _, spec := range layout.Files {
		if spec.Role == owner.Role {
			f.path = spec.Path
		} else if f.other == "" {
			f.other = spec.Path
		}
	}
	return f
}

func (f engineHoldFixture) seed(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range stmts {
		if _, err := db.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func (f engineHoldFixture) inspect(t *testing.T) storageInspection {
	t.Helper()
	si, err := inspectStorageOwnership(f.layout, f.ident, f.cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	return si
}

func engineHoldFileBytes(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range paths {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if b, err := os.ReadFile(p + suffix); err == nil {
				out[p+suffix] = string(b)
			}
		}
	}
	return out
}

func TestPaperResearchEngineHoldSavedStatePolicy(t *testing.T) {
	type stateCase struct {
		rows     func(sid string) []string
		wrong    bool
		refusal  string
		admitted bool
	}
	strategyRow := func(sid string) string {
		return "INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES ('" + sid + "', 'perps', 'hyperliquid', 1000, 1000)"
	}
	cases := map[string]stateCase{
		"fresh empty": {admitted: true},
		"flat completed history": {rows: func(sid string) []string {
			return []string{strategyRow(sid), "INSERT INTO closed_positions (strategy_id, symbol, quantity, avg_cost, side, closed_at, close_price, realized_pnl) VALUES ('" + sid + "', 'BTC', 0.01, 60000, 'long', '2026-10-01T00:00:00Z', 61000, 10)"}
		}, admitted: true},
		"flat funding history": {rows: func(sid string) []string {
			return []string{strategyRow(sid), "UPDATE strategies SET paper_funding_state = '{\"v\":1,\"coins\":{\"BTC\":{\"anchor\":{\"signed_qty\":0},\"settled_through_ms\":5}}}' WHERE id = '" + sid + "'"}
		}, admitted: true},
		"ordinary exposure": {rows: func(sid string) []string {
			return []string{strategyRow(sid), "INSERT INTO positions (strategy_id, symbol, quantity, avg_cost, side) VALUES ('" + sid + "', 'BTC', 0.01, 60000, 'long')"}
		}, refusal: "open position rows"},
		"option exposure": {rows: func(sid string) []string {
			return []string{strategyRow(sid), "INSERT INTO option_positions (strategy_id, id, underlying, option_type, strike, expiry, action, quantity) VALUES ('" + sid + "', 'o1', 'BTC', 'call', 70000, '2026-12-01', 'buy', 1)"}
		}, refusal: "open option position rows"},
		"protection reference": {rows: func(sid string) []string {
			return []string{strategyRow(sid), "INSERT INTO positions (strategy_id, symbol, quantity, avg_cost, side, stop_loss_oid) VALUES ('" + sid + "', 'BTC', 0.01, 60000, 'long', 42)"}
		}, refusal: "open position rows"},
		"pending manual action": {rows: func(sid string) []string {
			return []string{"INSERT INTO pending_manual_actions (strategy_id, action, symbol, side, quantity, fill_price, created_at) VALUES ('" + sid + "', 'close', 'BTC', 'sell', 0.01, 60000, '2026-10-01T00:00:00Z')"}
		}, refusal: "pending manual action rows"},
		"pending order": {rows: func(sid string) []string {
			return []string{"INSERT INTO pending_limit_orders (strategy_id, symbol, side, order_oid, limit_price, order_size, created_at) VALUES ('" + sid + "', 'BTC', 'buy', 7, 60000, 0.01, '2026-10-01T00:00:00Z')"}
		}, refusal: "pending limit order rows"},
		"unresolved entry intent": {rows: func(sid string) []string {
			return []string{"INSERT INTO entry_order_intents (intent_id, scope, source, strategy_id, symbol, interval_ms, target_side, bar_open_ms, decision_boundary_ms, cutoff_ms, provider, client_order_id, status, created_at_ms, updated_at_ms) VALUES ('i1', 'paper', '" + "SRC" + "', '" + sid + "', 'BTC', 60000, 'long', 1, 2, 3, 'paper', 'c1', 'reserved', 1, 1)"}
		}, refusal: "unresolved entry intents"},
		"unapplied funding evidence": {rows: func(sid string) []string {
			return []string{strategyRow(sid), "UPDATE strategies SET paper_funding_state = '{\"v\":1,\"coins\":{\"BTC\":{\"anchor\":{\"signed_qty\":0.01},\"held\":[{\"time_ms\":5,\"rate\":0.0001,\"reason\":\"x\"}]}}}' WHERE id = '" + sid + "'"}
		}, refusal: "paper funding evidence"},
		"unreadable funding evidence": {rows: func(sid string) []string {
			return []string{strategyRow(sid), "UPDATE strategies SET paper_funding_state = '{not json' WHERE id = '" + sid + "'"}
		}, refusal: "unreadable"},
		"pending circuit-breaker close": {rows: func(sid string) []string {
			return []string{strategyRow(sid), "UPDATE strategies SET risk_pending_circuit_closes_json = '{\"BTC\":1}' WHERE id = '" + sid + "'"}
		}, refusal: "pending circuit-breaker closes"},
		"exposure in the wrong file": {rows: func(sid string) []string {
			return []string{strategyRow(sid), "INSERT INTO positions (strategy_id, symbol, quantity, avg_cost, side) VALUES ('" + sid + "', 'BTC', 0.01, 60000, 'long')"}
		}, wrong: true, refusal: "mapped to the"},
		"flat durable quote contract": {rows: func(sid string) []string {
			return []string{"INSERT INTO quote_contracts (scope, source, strategy_id, coin, provider, account_key, policy_fingerprint, config_fingerprint, start_ms, revision, book_json, updated_at_ms) VALUES ('paper', 'SRC', '" + sid + "', 'BTC', 'paper', 'paper:x', 'p', 'c', 1, 1, '{}', 1)"}
		}, refusal: "durable quote contract and no admitted quote owner"},
	}
	labels := make([]string, 0, len(cases))
	for k := range cases {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	for _, l := range engineHoldLayouts() {
		t.Run(l.name, func(t *testing.T) {
			for _, label := range labels {
				c := cases[label]
				f := newEngineHoldFixture(t, l)
				if c.wrong && f.other == "" {
					continue
				}
				target := f.path
				if c.wrong {
					target = f.other
				}
				if c.rows != nil {
					var stmts []string
					for _, stmt := range c.rows(f.owner.StorageID) {
						stmts = append(stmts, strings.ReplaceAll(stmt, "'SRC'", "'"+f.owner.Partition.Source+"'"))
					}
					f.seed(t, target, stmts...)
				}
				var files []string
				for _, spec := range f.layout.Files {
					files = append(files, spec.Path)
				}
				before := engineHoldFileBytes(t, files...)
				si := f.inspect(t)
				after := engineHoldFileBytes(t, files...)
				for p, b := range before {
					if strings.HasSuffix(p, "-shm") {
						continue
					}
					if a, ok := after[p]; !ok || a != b {
						t.Fatalf("%s: inspection changed or removed %s", label, p)
					}
				}
				for p := range after {
					if _, ok := before[p]; !ok && !strings.HasSuffix(p, "-shm") && !strings.HasSuffix(p, "-wal") {
						t.Fatalf("%s: inspection created %s", label, p)
					}
				}
				joined := strings.Join(si.Rejections, "\n")
				if c.admitted {
					if !si.OK() {
						t.Fatalf("%s: the held owner must start: %s", label, joined)
					}
					continue
				}
				if si.OK() || !strings.Contains(joined, c.refusal) {
					t.Fatalf("%s: startup must refuse with %q, got %q", label, c.refusal, joined)
				}
			}
		})
	}
}

func TestPaperResearchEngineHoldStatusAndReload(t *testing.T) {
	owner := researchQuoteOwnerFixture("--mode=paper")
	_, path := paperResearchServer(t, map[string]interface{}{"market_feed": "websocket"}, paperResearchMomentum(), owner)
	cfg, err := LoadConfigForProbe(path)
	if err != nil {
		t.Fatal(err)
	}
	holds := engineHeldStrategyIDs(cfg.Strategies)
	if len(holds) != 1 || holds["hl-invmm-btc"].Reason != quoteEngineHoldReason {
		t.Fatalf("held set = %+v", holds)
	}
	due := append([]StrategyConfig(nil), cfg.Strategies...)
	marks := map[string]feedEvaluationMark{"hl-invmm-btc": {ID: "x"}, "hl-momentum-eth": {ID: "x"}}
	supported, held := splitEngineHeldDue(due)
	if len(supported) != 1 || supported[0].ID != "hl-momentum-eth" || len(held) != 1 {
		t.Fatalf("split = %d supported, %d held", len(supported), len(held))
	}
	lastRun := map[string]time.Time{}
	lastEval := map[string]feedEvaluationMark{}
	reportEngineHeldDue(held, true, marks, lastRun, lastEval, time.Now())
	if _, stillMarked := marks["hl-invmm-btc"]; stillMarked {
		t.Fatal("a held entry must leave the shared-feed evaluation marks")
	}
	if _, ok := lastEval["hl-invmm-btc"]; !ok {
		t.Fatal("a held entry advances its due bookkeeping")
	}
	view := globalRunEvidence.healthView()["held"].(map[string]map[string]string)
	if view["hl-invmm-btc"]["reason"] != quoteEngineHoldReason || view["hl-invmm-btc"]["tracking_url"] != quoteEngineHoldTrackingURL {
		t.Fatalf("health view = %+v", view["hl-invmm-btc"])
	}
	globalRunEvidence.markHeld("hl-invmm-btc", runEvidenceHeldSaveBlocked, time.Now())
	globalRunEvidence.clearHeld("hl-invmm-btc")
	if h, ok := globalRunEvidence.engineHoldFor("hl-invmm-btc"); !ok || h.reason != quoteEngineHoldReason {
		t.Fatal("a persistence hold must not overwrite or clear the engine hold")
	}
	next := *cfg
	next.Strategies = append([]StrategyConfig(nil), cfg.Strategies...)
	for i := range next.Strategies {
		if next.Strategies[i].ID == "hl-invmm-btc" {
			next.Strategies[i].Quote = nil
			next.Strategies[i].Args = []string{"momentum", "BTC", "1m", "--mode=paper"}
			next.Strategies[i].OpenStrategy = StrategyRef{Name: "momentum"}
			next.Strategies[i].CloseStrategy = nil
		}
	}
	if errs := engineHoldReloadErrors(cfg, &next); len(errs) != 1 || !strings.Contains(errs[0], "restart required") {
		t.Fatalf("a reload that changes the held set must be refused: %v", errs)
	}
	if errs := engineHoldReloadErrors(cfg, cfg); len(errs) != 0 {
		t.Fatalf("an unchanged reload keeps the hold: %v", errs)
	}
	globalRunEvidence.retainEngineHolds(map[string]engineHold{})
	if _, ok := globalRunEvidence.engineHoldFor("hl-invmm-btc"); ok {
		t.Fatal("a removed hold must clear")
	}
}
