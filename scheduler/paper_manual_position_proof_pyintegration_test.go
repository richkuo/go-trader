//go:build pyintegration

package main

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type pmpLayout struct {
	name    string
	file    string
	storage func(id string) string
	mutate  func(doc map[string]any, dir string, strategies []any)
}

func pmpLayouts() []pmpLayout {
	same := func(id string) string { return id }
	return []pmpLayout{
		{name: "single file", file: "state.db", storage: same, mutate: func(doc map[string]any, dir string, strategies []any) {
			doc["strategies"] = strategies
		}},
		{name: "default paper file", file: "paper.db", storage: same, mutate: func(doc map[string]any, dir string, strategies []any) {
			doc["paper_db_file"] = filepath.Join(dir, "paper.db")
			doc["strategies"] = strategies
		}},
		{name: "named paper source", file: "alt.db", storage: same, mutate: func(doc map[string]any, dir string, strategies []any) {
			doc["paper_sources"] = []any{map[string]any{"id": "alt", "db_file": filepath.Join(dir, "alt.db")}}
			for _, raw := range strategies {
				raw.(map[string]any)["paper_source"] = "alt"
			}
			doc["strategies"] = strategies
		}},
		{name: "storage alias", file: "state.db", storage: func(id string) string { return "stored-" + id }, mutate: func(doc map[string]any, dir string, strategies []any) {
			for _, raw := range strategies {
				sc := raw.(map[string]any)
				sc["storage_strategy_id"] = "stored-" + sc["id"].(string)
			}
			doc["strategies"] = strategies
		}},
	}
}

func pmpConfig(e *mrpEnv, name string, layout pmpLayout, ids ...string) (string, string) {
	e.t.Helper()
	dir := e.t.TempDir()
	path := e.writeConfig(name, func(doc map[string]any) {
		doc["db_file"] = filepath.Join(dir, "state.db")
		strategies := make([]any, 0, len(ids))
		for _, id := range ids {
			strategies = append(strategies, mrpPaperManualStrategy(id, "short"))
		}
		layout.mutate(doc, dir, strategies)
	})
	return path, filepath.Join(dir, layout.file)
}

type pmpRow struct {
	found    bool
	qty      float64
	proof    string
	receipts string
	posID    string
}

func pmpPosition(t *testing.T, dbPath, storageID string) pmpRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var r pmpRow
	err = db.QueryRow(`SELECT quantity, paper_sim_proof, paper_warning_receipts_json, position_id FROM positions WHERE strategy_id = ? AND symbol = 'BTC'`, storageID).Scan(&r.qty, &r.proof, &r.receipts, &r.posID)
	if err == sql.ErrNoRows {
		return r
	}
	if err != nil {
		t.Fatalf("read position %s in %s: %v", storageID, dbPath, err)
	}
	r.found = true
	return r
}

func pmpExec(t *testing.T, dbPath, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

func pmpColumns(t *testing.T, dbPath, table string) map[string]bool {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	return out
}

func pmpQueue(t *testing.T, cfgPath string, a PendingManualAction) {
	t.Helper()
	store := openMRPStore(t, cfgPath)
	defer store.Close()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if err := store.InsertPendingManualAction(a); err != nil {
		t.Fatalf("queue %s for %s: %v", a.Action, a.StrategyID, err)
	}
}

func pmpOnce(e *mrpEnv, cfgPath string) mrpRun {
	e.t.Helper()
	r := e.command(cfgPath, nil, "--once", "--status-port", strconv.Itoa(htfITFreePort(e.t)))
	if r.code != 0 {
		e.t.Fatalf("--once failed:\n%s", r)
	}
	return r
}

func pmpWarnings(r mrpRun, id string) int {
	return strings.Count(r.stdout, "[WARN] paper manual position "+id+"/BTC")
}

func TestPaperManualVenueGuardPositionProofLifecycle(t *testing.T) {
	t.Parallel()
	for _, layout := range pmpLayouts() {
		t.Run(layout.name, func(t *testing.T) {
			t.Parallel()
			e := newMRPEnv(t, 40)
			cfg, dbPath := pmpConfig(e, "proof-"+strings.ReplaceAll(layout.name, " ", "-")+".json", layout, "pm-proven", "pm-legacy", "pm-gone")
			if r := e.command(cfg, nil, "manual-open", "pm-proven", "--side", "short", "--size", "0.01", "--fill-price", "100", "--record-only"); r.code != 0 {
				t.Fatalf("record-only open must queue:\n%s", r)
			}
			store := openMRPStore(t, cfg)
			queued, err := store.LoadPendingManualActions()
			store.Close()
			if err != nil || len(queued) != 1 || queued[0].PaperSimProof == "" || queued[0].SourceRole == "" {
				t.Fatalf("the guarded producer must stamp its queued row: %v %+v", err, queued)
			}
			if !strings.Contains(queued[0].PaperSimProof, `"kind":"record_only"`) || !strings.Contains(queued[0].PaperSimProof, `"action":"open"`) {
				t.Fatalf("the stamp must name the producer and the action: %s", queued[0].PaperSimProof)
			}
			pmpQueue(t, cfg, PendingManualAction{StrategyID: "pm-legacy", Action: "open", Symbol: "BTC", Side: "short", Quantity: 0.01, FillPrice: 100})
			pmpQueue(t, cfg, PendingManualAction{StrategyID: "pm-gone", Action: "open", Symbol: "BTC", Side: "short", Quantity: 0.01, FillPrice: 100})
			pmpQueue(t, cfg, PendingManualAction{StrategyID: "pm-gone", Action: "close", Symbol: "BTC", Side: "buy", Quantity: 0.01, FillPrice: 100, IsFullClose: true})

			first := pmpOnce(e, cfg)
			if n := pmpWarnings(first, "pm-legacy"); n != 1 {
				t.Fatalf("a legacy open with no order ids drained in this run must warn once, got %d:\n%s", n, first.stdout)
			}
			if n := pmpWarnings(first, "pm-proven"); n != 0 {
				t.Fatalf("a guarded record-only open queued before the first start must not warn:\n%s", first.stdout)
			}
			if n := pmpWarnings(first, "pm-gone"); n != 0 {
				t.Fatalf("a position opened and closed inside the drain must not warn:\n%s", first.stdout)
			}
			proven := pmpPosition(t, dbPath, layout.storage("pm-proven"))
			legacy := pmpPosition(t, dbPath, layout.storage("pm-legacy"))
			if !proven.found || proven.posID == "" || !strings.Contains(proven.proof, proven.posID) {
				t.Fatalf("the drained record-only open must carry proof bound to its position id in the owning file: %+v", proven)
			}
			if !legacy.found || legacy.proof != "" {
				t.Fatalf("the legacy open must have no proof: %+v", legacy)
			}
			if gone := pmpPosition(t, dbPath, layout.storage("pm-gone")); gone.found {
				t.Fatalf("the opened and closed position must be gone: %+v", gone)
			}

			if r := e.command(cfg, nil, "manual-add", "pm-proven", "--size", "0.01", "--fill-price", "101", "--record-only"); r.code != 0 {
				t.Fatalf("record-only add must queue:\n%s", r)
			}
			second := pmpOnce(e, cfg)
			if pmpWarnings(second, "pm-proven") != 0 || pmpWarnings(second, "pm-legacy") != 1 {
				t.Fatalf("after a simulated add the proven position stays quiet and the legacy one prints once per run:\n%s", second.stdout)
			}
			afterAdd := pmpPosition(t, dbPath, layout.storage("pm-proven"))
			if afterAdd.qty < 0.0199 || afterAdd.proof != proven.proof {
				t.Fatalf("a simulated add must keep the proof: %+v", afterAdd)
			}

			pmpQueue(t, cfg, PendingManualAction{StrategyID: "pm-proven", Action: "add", Symbol: "BTC", Side: "short", Quantity: 0.01, FillPrice: 102, ExchangeOrderID: "5501"})
			third := pmpOnce(e, cfg)
			if n := pmpWarnings(third, "pm-proven"); n != 1 {
				t.Fatalf("an add queued by a release that could send it to the venue must make the position warn in that run, got %d:\n%s", n, third.stdout)
			}
			if after := pmpPosition(t, dbPath, layout.storage("pm-proven")); after.proof != "" {
				t.Fatalf("the uncertain add must clear the proof: %+v", after)
			}
			if kinds := e.venueKinds(); kinds["execute"] != 0 || kinds["limit_open"] != 0 || kinds["cancel"] != 0 {
				t.Fatalf("the paper runs sent venue writes: %v", kinds)
			}
		})
	}
}

func TestPaperManualVenueGuardPositionProofEvidence(t *testing.T) {
	e := newMRPEnv(t, 40)
	layout := pmpLayouts()[0]
	cfg, dbPath := pmpConfig(e, "proof-evidence.json", layout, "pm-mal", "pm-ver", "pm-oid", "pm-sl", "pm-partial", "pm-unknown-add")
	for _, id := range []string{"pm-mal", "pm-ver", "pm-sl", "pm-partial"} {
		if r := e.command(cfg, nil, "manual-open", id, "--side", "short", "--size", "1", "--fill-price", "100", "--record-only"); r.code != 0 {
			t.Fatalf("record-only open %s must queue:\n%s", id, r)
		}
	}
	pmpQueue(t, cfg, PendingManualAction{StrategyID: "pm-oid", Action: "open", Symbol: "BTC", Side: "short", Quantity: 1, FillPrice: 100, ExchangeOrderID: "8801", PaperSimProof: paperSimQueueProofJSON(paperSimKindRecordOnly, "open", "BTC", "")})
	pmpQueue(t, cfg, PendingManualAction{StrategyID: "pm-unknown-add", Action: "open", Symbol: "BTC", Side: "short", Quantity: 1, FillPrice: 100})
	first := pmpOnce(e, cfg)
	if pmpWarnings(first, "pm-oid") != 1 {
		t.Fatalf("a stamped row that carries an exchange order id must not establish proof:\n%s", first.stdout)
	}
	for _, id := range []string{"pm-mal", "pm-ver", "pm-sl", "pm-partial"} {
		if pmpWarnings(first, id) != 0 {
			t.Fatalf("%s is proven and must not warn:\n%s", id, first.stdout)
		}
	}

	if r := e.command(cfg, nil, "manual-add", "pm-unknown-add", "--size", "1", "--fill-price", "101", "--record-only"); r.code != 0 {
		t.Fatalf("record-only add must queue:\n%s", r)
	}
	pmpExec(t, dbPath, `UPDATE positions SET tp_oids_json = 'not-json' WHERE strategy_id = 'pm-mal'`)
	pmpExec(t, dbPath, `UPDATE positions SET paper_sim_proof = replace(paper_sim_proof, '"v":1', '"v":2') WHERE strategy_id = 'pm-ver'`)
	pmpQueue(t, cfg, PendingManualAction{StrategyID: "pm-sl", Action: "update-sl", Symbol: "BTC", Side: "short", StopLossOID: 9101, StopLossTriggerPx: 110})

	paperCfg, err := LoadConfigForProbe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openToolStateStore(paperCfg)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := LoadStateWithStore(paperCfg, store)
	if err != nil {
		t.Fatal(err)
	}
	var sc StrategyConfig
	for _, s := range paperCfg.Strategies {
		if s.ID == "pm-partial" {
			sc = s
		}
	}
	pinned := *state.Strategies["pm-partial"].Positions["BTC"]
	prevLot := paperManualCloseLotLookup
	paperManualCloseLotLookup = func(coin string) hlLotLookup { return hlLotLookup{Known: true, SzDecimals: 4, Coin: coin} }
	closeEval := &HyperliquidResult{Symbol: "BTC", Signal: 1}
	closeEval.CloseFraction = 0.5
	out := queuePaperManualCycleClose(paperManualCycleCloseInput{SC: sc, State: state.Strategies["pm-partial"], Store: store, Pinned: &pinned, Result: closeEval, Price: 100, Mu: &sync.RWMutex{}, Now: time.Now().UTC()})
	paperManualCloseLotLookup = prevLot
	store.Close()
	if !out.Queued || !strings.Contains(out.Action.PaperSimProof, pinned.TradePositionID) || !strings.Contains(out.Action.PaperSimProof, `"kind":"cycle_close"`) {
		t.Fatalf("the paper cycle close producer must queue a stamped row bound to the position: %+v", out)
	}

	second := pmpOnce(e, cfg)
	for id, why := range map[string]string{"pm-mal": "malformed protection data", "pm-ver": "an unknown proof version", "pm-sl": "a protection edit", "pm-unknown-add": "a simulated add to an unknown position"} {
		if n := pmpWarnings(second, id); n != 1 {
			t.Fatalf("%s must warn once after %s, got %d:\n%s", id, why, n, second.stdout)
		}
	}
	if pmpWarnings(second, "pm-partial") != 0 {
		t.Fatalf("a simulated partial cycle close must keep the proof:\n%s", second.stdout)
	}
	partial := pmpPosition(t, dbPath, "pm-partial")
	if !partial.found || partial.qty > 0.51 || partial.qty < 0.49 || !strings.Contains(partial.proof, partial.posID) {
		t.Fatalf("the partial close must reduce the position and keep its proof: %+v", partial)
	}
	if mal := pmpPosition(t, dbPath, "pm-mal"); mal.proof != "" {
		t.Fatalf("malformed protection data must clear the stored proof on the next save: %+v", mal)
	}
	if sl := pmpPosition(t, dbPath, "pm-sl"); sl.proof != "" {
		t.Fatalf("a protection edit must clear the stored proof: %+v", sl)
	}
	if unknown := pmpPosition(t, dbPath, "pm-unknown-add"); unknown.qty < 1.99 || unknown.proof != "" {
		t.Fatalf("a simulated add must leave an unknown position unknown: %+v", unknown)
	}
}

func TestPaperManualVenueGuardPositionProofSchema(t *testing.T) {
	t.Parallel()
	e := newMRPEnv(t, 40)
	layout := pmpLayouts()[0]
	cfg, dbPath := pmpConfig(e, "proof-schema.json", layout, "pm-legacy")
	pmpQueue(t, cfg, PendingManualAction{StrategyID: "pm-legacy", Action: "open", Symbol: "BTC", Side: "short", Quantity: 0.01, FillPrice: 100})
	pmpOnce(e, cfg)
	if cols := pmpColumns(t, dbPath, "positions"); !cols["paper_sim_proof"] || !cols["paper_warning_receipts_json"] {
		t.Fatalf("the migrated file must carry the position columns: %v", cols)
	}
	pmpExec(t, dbPath, `ALTER TABLE positions DROP COLUMN paper_sim_proof`)
	pmpExec(t, dbPath, `ALTER TABLE positions DROP COLUMN paper_warning_receipts_json`)
	pmpExec(t, dbPath, `ALTER TABLE pending_manual_actions DROP COLUMN paper_sim_proof`)
	report := e.command(cfg, nil, "--summary=hyperliquid-paper")
	if pmpWarnings(mrpRun{stdout: report.stdout}, "pm-legacy") != 1 || strings.Contains(report.stderr, "Failed to load state") || strings.Contains(report.stderr, "no such column") {
		t.Fatalf("a read-only report on a pre-upgrade file must load the position and print its warning:\n%s", report)
	}
	if cols := pmpColumns(t, dbPath, "positions"); cols["paper_sim_proof"] || cols["paper_warning_receipts_json"] {
		t.Fatalf("a read-only report must not change the schema: %v", cols)
	}
	migrated := pmpOnce(e, cfg)
	if pmpWarnings(migrated, "pm-legacy") != 1 {
		t.Fatalf("the migrating run must load the old row and warn:\n%s", migrated.stdout)
	}
	if cols := pmpColumns(t, dbPath, "positions"); !cols["paper_sim_proof"] || !cols["paper_warning_receipts_json"] {
		t.Fatalf("the rerun migration must add the columns back: %v", cols)
	}
	if cols := pmpColumns(t, dbPath, "pending_manual_actions"); !cols["paper_sim_proof"] {
		t.Fatalf("the rerun migration must add the queue column back: %v", cols)
	}
	pmpOnce(e, cfg)
}
