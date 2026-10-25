package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"trading-scheduler/internal/egresspolicy"
)

func TestBasketPairAdmissionExclusiveClaimHasOneWinner(t *testing.T) {
	registerPairTestPolicy(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "run")
	doc := pairTestDoc(t, dir, nil)
	var wg sync.WaitGroup
	results := make([]error, 2)
	admissions := make([]*pairResearchAdmission, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			admissions[i], _, results[i] = newPairResearchAdmission(doc, "run-1", dir, false, pairTestDeps(t, doc, newPairTestVenues(false)))
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, err := range results {
		if err == nil {
			winners++
			t.Cleanup(admissions[i].release)
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one concurrent claim must win, got %d (%v)", winners, results)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != pairResearchManifestName {
		t.Fatalf("the winner's directory must hold only its own manifest, got %v %v", entries, err)
	}
	before := dirDigest(t, dir)
	if _, _, err := newPairResearchAdmission(doc, "run-2", dir, false, pairTestDeps(t, doc, newPairTestVenues(false))); err == nil {
		t.Fatal("a later claim of the winner's directory must be refused")
	}
	if after := dirDigest(t, dir); after != before {
		t.Fatal("the losing claim changed the winner's directory")
	}
	empty := filepath.Join(parent, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(empty, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{empty, link} {
		d := pairTestDoc(t, path, nil)
		if _, _, err := newPairResearchAdmission(d, "run-3", path, false, pairTestDeps(t, d, newPairTestVenues(false))); err == nil {
			t.Fatalf("a new run must refuse the existing path %s", path)
		}
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) != 0 {
		t.Fatalf("a refused claim must leave the existing directory unchanged, got %v %v", entries, err)
	}
	invalid := filepath.Join(parent, "invalid")
	bad := pairTestDoc(t, invalid, pairSetLimit("decision_cadence_ms", 0))
	if _, _, err := newPairResearchAdmission(bad, "run-4", invalid, false, pairTestDeps(t, bad, newPairTestVenues(false))); err == nil {
		t.Fatal("an invalid configuration must be refused")
	}
	if _, err := os.Lstat(invalid); !os.IsNotExist(err) {
		t.Fatalf("pure checks run before the claim, so an invalid configuration creates no directory: %v", err)
	}
}

func TestBasketReplayConfiglessEgressBranchReadsNoDeployedConfig(t *testing.T) {
	prev := processEgress
	processEgress = &egressRuntime{}
	t.Cleanup(func() { processEgress = prev })
	if !egressLocalSubcommands["basket-replay"] {
		t.Fatal("basket-replay keeps its local classification")
	}
	t.Chdir(t.TempDir())
	t.Setenv(egresspolicy.EnvProxy, "http://127.0.0.1:9")
	code, stop := gateSubcommandEgress("basket-replay", []string{"--spec", "/nonexistent/spec.json"})
	if stop || code != 0 || !processEgress.isActive() {
		t.Fatalf("the configless branch must record proxy mode without reading a deployed configuration, got (%d, %v, active %v)", code, stop, processEgress.isActive())
	}
	processEgress = &egressRuntime{}
	t.Setenv(egresspolicy.EnvProxy, "")
	if code, stop := gateSubcommandEgress("basket-replay", nil); stop || code != 0 || processEgress.isActive() {
		t.Fatalf("no proxy variable needs no setup (%d, %v)", code, stop)
	}
}

func TestBasketReplayPairStorageMigrationKeepsVersionOneRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), basketReplayDBName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `CREATE TABLE basket_replay_runs (run_id TEXT PRIMARY KEY, owner TEXT NOT NULL, protocol TEXT NOT NULL, source_fingerprint TEXT NOT NULL,
		contract_fingerprint TEXT NOT NULL, input_fingerprint TEXT NOT NULL, config_fingerprint TEXT NOT NULL, run_fingerprint TEXT NOT NULL,
		cost_policy_id TEXT NOT NULL DEFAULT '', cost_model_version INTEGER NOT NULL DEFAULT 0, steps_total INTEGER NOT NULL DEFAULT 0,
		steps_applied INTEGER NOT NULL DEFAULT 0, cursor_ms INTEGER NOT NULL DEFAULT 0, created_at_ms INTEGER NOT NULL DEFAULT 0);
	CREATE TABLE basket_replay_steps (run_id TEXT NOT NULL, step_index INTEGER NOT NULL, record_json TEXT NOT NULL, lot_refusals INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (run_id, step_index));
	INSERT INTO basket_replay_runs (run_id, owner, protocol, source_fingerprint, contract_fingerprint, input_fingerprint, config_fingerprint, run_fingerprint, steps_total, steps_applied, cursor_ms)
		VALUES ('v1-run', 'go-trader basket-replay adapter', 'go-trader.basket-replay/1', 's', 'c', 'i', 'g', 'r', 4, 2, 1700000000000);
	INSERT INTO basket_replay_steps (run_id, step_index, record_json) VALUES ('v1-run', 0, '{"index":0,"at":"x","funding_rows_booked":0,"members":[],"partition":{"partition":"paper","kill_switch_active":false,"kill_switch_fired":false,"peak_value":0,"drawdown_pct":0,"daily_loss_entries_held":false,"daily_loss_usd":0,"notional_blocked":false,"kill_switch_events":0},"exposure":{"gross_usd":0,"net_usd":0,"per_coin_usd":null,"basket_equity_usd":0}}');`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	db.Close()
	sdb, err := OpenStateDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	for i := 0; i < 2; i++ {
		if err := sdb.ensureBasketReplaySchema(); err != nil {
			t.Fatalf("migration pass %d: %v", i+1, err)
		}
	}
	p, err := sdb.readBasketReplayProgress("v1-run")
	if err != nil || p.StepsApplied != 2 || p.CursorMs != 1700000000000 {
		t.Fatalf("the version-1 run row must keep its progress, got %+v %v", p, err)
	}
	steps, err := sdb.readBasketReplaySteps("v1-run", 2)
	if err != nil || len(steps) != 1 {
		t.Fatalf("the version-1 step record must stay readable, got %d %v", len(steps), err)
	}
	c, err := sdb.readPairReplayCursor("v1-run")
	if err != nil || c.Schema != 1 || c.Applied != 2 || c.MemberCosts != "" || c.PendingID != "" {
		t.Fatalf("migrated version-1 rows must project the version-2 column defaults, got %+v %v", c, err)
	}
	for _, table := range []string{"basket_replay_objects", "basket_replay_failures"} {
		if ok, err := tableExists(sdb, table); err != nil || !ok {
			t.Fatalf("the migration must create %s: %v", table, err)
		}
	}
	fi := &storageFileInspection{Role: storageRolePrimary}
	rej, err := inspectBasketReplayRows(sdb, fi)
	if err != nil || len(rej) != 1 || len(fi.ReplayRuns) != 1 {
		t.Fatalf("storage inspection must still list and refuse the replay run, got %v %v %v", rej, fi.ReplayRuns, err)
	}
}
