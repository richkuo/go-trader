package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBasketLifecycleCommitsAtEveryBookSaveBoundary(t *testing.T) {
	cfg := basketHorizonConfig(t, 3600, nil)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	env.state.partitionRisk(defaultPaperPartition).KillSwitchActive = true
	env.stepAt(opened.Add(time.Hour+time.Second), basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	env.state.partitionRisk(defaultPaperPartition).KillSwitchActive = false
	want := deadline.UnixMilli()
	for _, m := range basketTestMembers {
		if l, _, _ := env.lifecycleRow(m.ID); !l.active() || l.SuppressThroughMs != want {
			t.Fatalf("%s must hold a due lifecycle with suppression before the native closes: %+v", m.ID, l)
		}
	}
	closeNative := func(s *StrategyState, coin string) {
		t.Helper()
		if !recordPerpsStopLossClose(s, coin, s.Positions[coin].AvgCost, "stop_loss", basketTestLogger(s.ID)) {
			t.Fatalf("%s native stop did not book", s.ID)
		}
	}
	assertCleared := func(id string) {
		t.Helper()
		l, _, _ := env.lifecycleRow(id)
		if l.active() || l.SuppressThroughMs != want {
			t.Fatalf("%s lifecycle %+v, want cleared with the suppression deadline kept", id, l)
		}
	}
	assertActive := func(id string) {
		t.Helper()
		if l, _, _ := env.lifecycleRow(id); !l.active() {
			t.Fatalf("%s lifecycle cleared by a save that did not write its book: %+v", id, l)
		}
	}

	env.mu.Lock()
	btc := env.state.Strategies["hl-bsk-btc"]
	closeNative(btc, "BTC")
	if err := env.store.SavePartition(env.state, defaultPaperPartition); err != nil {
		t.Fatal(err)
	}
	env.mu.Unlock()
	assertCleared("hl-bsk-btc")
	if l := env.rt().Lifecycle["hl-bsk-btc"]; l.active() {
		t.Fatalf("the runtime marker must follow the committed clear: %+v", l)
	}

	env.mu.Lock()
	eth, sol := env.state.Strategies["hl-bsk-eth"], env.state.Strategies["hl-bsk-sol"]
	closeNative(eth, "ETH")
	closeNative(sol, "SOL")
	if err := env.store.SaveStrategyBook(eth); err != nil {
		t.Fatal(err)
	}
	env.mu.Unlock()
	assertCleared("hl-bsk-eth")
	assertActive("hl-bsk-sol")
	assertActive("hl-bsk-avax")
	if l := env.rt().Lifecycle["hl-bsk-sol"]; !l.active() {
		t.Fatal("a member-only save must not mark a peer's lifecycle committed")
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM positions WHERE strategy_id = 'hl-bsk-sol'`); n != 1 {
		t.Fatal("a member-only save must not commit a peer's book")
	}

	env.mu.Lock()
	err := env.store.SaveStrategyBookQueueingManualAction(sol, PendingManualAction{StrategyID: sol.ID, Action: "sl_rearm_test", Symbol: "SOL", CreatedAt: opened})
	env.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	assertCleared("hl-bsk-sol")
	if n := env.dbCount(`SELECT COUNT(*) FROM pending_manual_actions WHERE strategy_id = 'hl-bsk-sol'`); n != 1 {
		t.Fatal("the queued manual action must commit with the lifecycle clear")
	}

	prev := storeCommitHook
	storeCommitHook = func(role storageRole) error { return errors.New("disk full") }
	env.mu.Lock()
	avax := env.state.Strategies["hl-bsk-avax"]
	closeNative(avax, "AVAX")
	err = env.store.SaveStrategyBook(avax)
	env.mu.Unlock()
	storeCommitHook = prev
	if err == nil {
		t.Fatal("the injected commit failure must surface")
	}
	assertActive("hl-bsk-avax")
	if l := env.rt().Lifecycle["hl-bsk-avax"]; !l.active() {
		t.Fatal("a failed save must not clear the runtime lifecycle")
	}

	fresh, _, err := LoadStateWithStore(cfg, env.store)
	if err != nil {
		t.Fatal(err)
	}
	freshAvax := fresh.Strategies["hl-bsk-avax"]
	if freshAvax.Basket != nil || freshAvax.Positions["AVAX"] == nil {
		t.Fatalf("want a pointer-free book that still holds AVAX on disk: %+v", freshAvax)
	}
	closeNative(freshAvax, "AVAX")
	if err := env.store.SaveStrategyBook(freshAvax); err != nil {
		t.Fatal(err)
	}
	assertCleared("hl-bsk-avax")

	env.store.Close()
	env.store = openSourceStore(t, cfg)
	loaded, _, err := LoadStateWithStore(cfg, env.store)
	if err != nil {
		t.Fatal(err)
	}
	runtimes, err := env.store.LoadBaskets(loaded, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range basketTestMembers {
		if l := runtimes["xs-test"].Lifecycle[m.ID]; l.active() || l.SuppressThroughMs != want {
			t.Fatalf("%s reloaded lifecycle %+v, want cleared with suppression", m.ID, l)
		}
	}
}

func TestBasketSubsetSaveCommitsOnlyTheSavedMembersLifecycleAndActions(t *testing.T) {
	cfg := basketHorizonConfig(t, 3600, nil)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	prev := storeCommitHook
	fail := true
	storeCommitHook = func(role storageRole) error {
		if fail && env.rt().pendingAction() == nil && len(env.rt().Actions) > 4 {
			return errors.New("disk full")
		}
		return nil
	}
	t.Cleanup(func() { storeCommitHook = prev })
	env.stepAt(opened.Add(time.Hour+time.Second), basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	fail = false
	rt := env.rt()
	var closed, peer string
	var action *basketAction
	for _, a := range rt.Actions {
		if a.expiry() {
			action = a
			closed = a.StrategyID
		}
	}
	for _, m := range basketTestMembers {
		if m.ID != closed {
			peer = m.ID
			break
		}
	}
	if action == nil || action.Status != basketActionCommitted || !rt.needsDurable {
		t.Fatalf("want one committed expiry that is not durable yet, got %+v (needsDurable %v)", action, rt.needsDurable)
	}
	subset := func(id string) {
		t.Helper()
		env.mu.Lock()
		defer env.mu.Unlock()
		err := env.store.primary().saveStateSubset(env.state, scopeSaveRequest{
			Strategies: []*StrategyState{env.state.Strategies[id]},
			ScopeOf:    map[string]PortfolioScope{id: ScopePaper},
			Scopes:     []PortfolioScope{ScopePaper},
			Partitions: []RiskPartition{defaultPaperPartition},
		})
		if err != nil {
			t.Fatalf("subset save of %s: %v", id, err)
		}
	}
	subset(peer)
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE action_id = ? AND status = 'committed'`, action.ActionID); n != 0 {
		t.Fatal("a subset save of a peer committed another member's book-dependent action")
	}
	if l, _, _ := env.lifecycleRow(closed); !l.active() {
		t.Fatalf("a subset save of a peer cleared %s's lifecycle", closed)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 1`); n != 0 {
		t.Fatal("a subset save of a peer committed another member's close trade")
	}
	if l, _, _ := env.lifecycleRow(peer); !l.active() || l.State != basketExpiryDue {
		t.Fatalf("the saved peer keeps its own lifecycle and progress: %+v", l)
	}
	subset(closed)
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE action_id = ? AND status = 'committed'`, action.ActionID); n != 1 {
		t.Fatal("the member's own subset save must commit its action with its book")
	}
	if l, _, _ := env.lifecycleRow(closed); l.active() {
		t.Fatalf("the member's own subset save must clear its flat lifecycle: %+v", l)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 1`); n != 1 {
		t.Fatalf("persisted closes %d, want the one the saved book carries", n)
	}
	if !rt.needsDurable {
		t.Fatal("a subset save must not declare the whole basket durable")
	}
	env.mu.Lock()
	err := env.store.SavePartition(env.state, defaultPaperPartition)
	env.mu.Unlock()
	if err != nil || rt.needsDurable {
		t.Fatalf("a partition save must finish durability: err %v needsDurable %v", err, rt.needsDurable)
	}
}

func TestBasketExpiryReservationAllocatesPastSavedRowsMissingFromMemory(t *testing.T) {
	cfg := basketHorizonConfig(t, 3600, nil)
	venue := newScriptedBasketVenue(false)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), venue.factory())
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 1)
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenueRejected, Detail: "no liquidity"}}
	at := opened.Add(time.Hour + time.Second)
	env.stepAt(at, snap, true, time.Time{})
	rid := env.expirySubmits(venue)[0].StrategyID
	venue.outcomes = []basketVenueOutcome{{Outcome: basketVenueRejected, Detail: "no liquidity"}}
	env.stepAt(at.Add(61*time.Second), snap, true, time.Time{})
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE strategy_id = ? AND purpose = 'expiry' AND status = 'no_fill'`, rid); n != 2 {
		t.Fatalf("want two saved terminal attempts for %s, got %d", rid, n)
	}
	env.restartFromDisk(basketTestRanker(basketTestWeights, ""), venue.factory())
	rt := env.rt()
	for id, a := range rt.Actions {
		if !basketActionPending(a.Status) {
			delete(rt.Actions, id)
		}
	}
	if rt.nextSeq(deadline.UnixMilli(), rid, basketDirectionReduce) != 1 {
		t.Fatal("the emulated load must hold no terminal rows for the key")
	}
	env.stepAt(at.Add(4*time.Minute), snap, true, time.Time{})
	if q := env.signedQty(rid, basketMemberCoinByID(rid)); q != 0 {
		t.Fatalf("%s did not close on the third attempt: %.10g", rid, q)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE strategy_id = ? AND purpose = 'expiry' AND lifecycle_attempt = 3 AND seq = 3 AND status = 'committed'`, rid); n != 1 {
		t.Fatal("the third attempt must take the next saved sequence for the key")
	}
	if n := env.dbCount(`SELECT COUNT(DISTINCT seq) FROM basket_member_actions WHERE strategy_id = ? AND deadline_ms = ? AND direction = 'reduce'`, rid, deadline.UnixMilli()); n != 3 {
		t.Fatalf("distinct sequences under the unique key %d, want 3", n)
	}
}

func TestBasketLifecycleMigrationAndLegacyFileUpgrade(t *testing.T) {
	cfg := basketHorizonConfig(t, 0, nil)
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	deadline := basketHistoricalDeadline()
	env.openMembers(deadline)
	db := env.store.primary()
	for _, stmt := range []string{
		`ALTER TABLE basket_member_actions RENAME TO bma_v2`,
		`ALTER TABLE basket_member_state RENAME TO bms_v2`,
		`DROP INDEX idx_basket_actions_one_pending`,
		`DROP INDEX idx_basket_actions_client_order`,
		basketSchemaDDL,
		`INSERT INTO basket_member_actions (` + basketActionBaseSelect + `) SELECT ` + basketActionBaseSelect + ` FROM bma_v2`,
		`INSERT INTO basket_member_state (scope, source, basket_id, strategy_id, expected_qty, updated_at_ms) SELECT scope, source, basket_id, strategy_id, expected_qty, updated_at_ms FROM bms_v2`,
		`DROP TABLE bma_v2`,
		`DROP TABLE bms_v2`,
	} {
		if _, err := db.db.Exec(stmt); err != nil {
			t.Fatalf("build the legacy file: %v", err)
		}
	}
	if has, _ := db.tableHasColumn("basket_member_state", "position_id"); has {
		t.Fatal("the fixture must hold the pre-change schema")
	}
	env.store.Close()
	digest := fileDigest(t, cfg.DBFile)
	layout, err := resolveStorageLayout(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := buildStorageIdentityMap(cfg, layout)
	if err != nil {
		t.Fatal(err)
	}
	si, err := inspectStorageOwnership(layout, ident, cfg, false)
	if err != nil || !si.OK() {
		t.Fatalf("read-only inspection of a pre-change file: %v %v", err, si.Rejections)
	}
	if fileDigest(t, cfg.DBFile) != digest {
		t.Fatal("read-only inspection changed the pre-change file")
	}

	env.store = openSourceStore(t, cfg)
	db = env.store.primary()
	if has, _ := db.tableHasColumn("basket_member_state", "suppress_through_ms"); !has {
		t.Fatal("opening the file must add the lifecycle columns")
	}
	before := basketSchemaSnapshot(t, db)
	if err := db.migrateBasketSchema(); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateSchema(); err != nil {
		t.Fatal(err)
	}
	if after := basketSchemaSnapshot(t, db); !reflect.DeepEqual(before, after) {
		t.Fatal("a repeated migration changed the schema")
	}
	loaded, _, err := LoadStateWithStore(cfg, env.store)
	if err != nil {
		t.Fatal(err)
	}
	env.state = loaded
	env.reopen(basketTestRanker(basketTestWeights, ""), nil, true)
	rt := env.rt()
	if rt.SavedFP != rt.Contract.Fingerprint() || strings.Contains(rt.SavedContract.JSON(), "holding_horizon") {
		t.Fatalf("the saved contract fingerprint changed across the upgrade: %s", rt.SavedFP)
	}
	if n := env.dbCount(`SELECT COUNT(*) FROM basket_member_actions WHERE purpose = 'rebalance'`); n != 4 {
		t.Fatalf("legacy action rows must read as rebalance purpose, got %d", n)
	}
	for _, m := range basketTestMembers {
		if l := rt.Lifecycle[m.ID]; l.active() || l.SuppressThroughMs != 0 {
			t.Fatalf("a legacy row must load with no lifecycle: %+v", l)
		}
		if rt.Expected[m.ID] == 0 {
			t.Fatalf("%s expected quantity was lost in the upgrade", m.ID)
		}
	}
	env.cycle(deadline.Add(2*time.Minute), deadline, basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true)
	if n := env.dbCount(`SELECT COUNT(*) FROM trades WHERE is_close = 0`); n != 4 {
		t.Fatalf("the upgraded basket must not rebook: %d opens", n)
	}
}

func TestBasketLifecycleOwnershipInspectionAndRefusals(t *testing.T) {
	dir := t.TempDir()
	path := basketTestConfigJSON(t, dir, func(root map[string]interface{}) {
		root["paper_db_file"] = filepath.Join(dir, "paper.db")
		root["paper_sources"] = []interface{}{
			map[string]interface{}{"id": "alpha", "db_file": filepath.Join(dir, "alpha.db")},
			map[string]interface{}{"id": "beta", "db_file": filepath.Join(dir, "beta.db")},
		}
		for _, m := range basketTestMembers {
			basketTestStrategy(root, m.ID)["paper_source"] = "alpha"
		}
		b := basketTestBasket(root)
		b["holding_horizon_seconds"] = 3600
		b["max_transition_net_usd"] = 1100
		root["strategies"] = append(root["strategies"].([]interface{}), map[string]interface{}{
			"id": "hl-beta-doge", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []interface{}{"rsi", "DOGE", "1h", "--mode=paper"}, "capital": 1000, "paper_source": "beta",
		})
	})
	cfg, err := LoadConfigReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	env := newBasketTestEnv(t, cfg, basketTestRanker(basketTestWeights, ""), nil)
	deadline := basketHistoricalDeadline()
	opened := env.openMembers(deadline)
	alpha := env.store.file(env.store.layout.roleForPartition(paperSourcePartition("alpha")))
	beta := env.store.file(env.store.layout.roleForPartition(paperSourcePartition("beta")))
	count := func(db *StateDB, q string) int {
		var n int
		if err := db.db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(alpha, `SELECT COUNT(*) FROM basket_member_state WHERE position_id != ''`); n != 4 {
		t.Fatalf("alpha holds %d lifecycles, want 4", n)
	}
	for _, other := range []*StateDB{env.store.primary(), beta} {
		if n := count(other, `SELECT COUNT(*) FROM basket_member_state`); n != 0 {
			t.Fatalf("%s file holds %d basket member rows, want 0", other.storageRoleOf(), n)
		}
	}
	inspect := func() storageInspection {
		t.Helper()
		layout, err := resolveStorageLayout(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ident, err := buildStorageIdentityMap(cfg, layout)
		if err != nil {
			t.Fatal(err)
		}
		si, err := inspectStorageOwnership(layout, ident, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		return si
	}
	si := inspect()
	text := formatStorageInspection(si)
	if !si.OK() || !strings.Contains(text, "unresolved holding-horizon expiry (scheduled)") {
		t.Fatalf("inspection must report the scheduled expiry without a rejection:\n%s\n%v", text, si.Rejections)
	}
	env.state.partitionRisk(paperSourcePartition("alpha")).KillSwitchActive = true
	env.stepAt(opened.Add(time.Hour+time.Second), basketTestSnapshot(deadline, 60, basketTestMarks(), 1), true, time.Time{})
	if text := formatStorageInspection(inspect()); !strings.Contains(text, "unresolved holding-horizon expiry (due, closing)") {
		t.Fatalf("inspection must report the due expiry:\n%s", text)
	}

	sid, err := alpha.toStorageID("hl-bsk-btc")
	if err != nil {
		t.Fatal(err)
	}
	var contractJSON string
	if err := alpha.db.QueryRow(`SELECT contract_json FROM basket_contracts WHERE basket_id = 'xs-test'`).Scan(&contractJSON); err != nil {
		t.Fatal(err)
	}
	exec := func(db *StateDB, q string, args ...interface{}) {
		t.Helper()
		if _, err := db.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	reload := func() error {
		t.Helper()
		loaded, _, err := LoadStateWithStore(cfg, env.store)
		if err != nil {
			return err
		}
		_, err = env.store.LoadBaskets(loaded, cfg)
		return err
	}
	if err := reload(); err != nil {
		t.Fatalf("the owning file must load: %v", err)
	}

	exec(beta, `INSERT INTO basket_contracts (scope, source, basket_id, contract_json, contract_fingerprint, updated_at_ms) VALUES ('paper', 'alpha', 'xs-test', ?, 'fp', 1)`, contractJSON)
	exec(beta, `INSERT INTO basket_member_state (scope, source, basket_id, strategy_id, expected_qty, updated_at_ms, position_id, entry_ms, expiry_ms) VALUES ('paper', 'alpha', 'xs-test', 'hl-beta-doge', 0, 1, 'p-stray', 1, 2)`)
	if err := reload(); err == nil || !strings.Contains(err.Error(), "recovery evidence sits in the") || !strings.Contains(err.Error(), "holding-horizon") {
		t.Fatalf("lifecycle evidence in the wrong file must refuse at zero expected quantity, got %v", err)
	}
	if si := inspect(); si.OK() {
		t.Fatal("inspection must reject a lifecycle row in a file that does not own its partition")
	}
	exec(beta, `DELETE FROM basket_member_state`)
	exec(beta, `DELETE FROM basket_contracts`)

	exec(alpha, `INSERT INTO basket_member_state (scope, source, basket_id, strategy_id, expected_qty, updated_at_ms, position_id, entry_ms, expiry_ms) VALUES ('paper', 'alpha', 'xs-test', 'ghost-member', 0, 1, 'p-ghost', 1, 2)`)
	if err := reload(); err == nil || !strings.Contains(err.Error(), "unmapped strategies") || !strings.Contains(err.Error(), "holding-horizon lifecycle") {
		t.Fatalf("an unmapped lifecycle must refuse at zero expected quantity, got %v", err)
	}
	exec(alpha, `DELETE FROM basket_member_state WHERE strategy_id = 'ghost-member'`)

	exec(alpha, `INSERT INTO basket_contracts (scope, source, basket_id, contract_json, contract_fingerprint, updated_at_ms) VALUES ('paper', 'alpha', 'xs-gone', ?, 'fp', 1)`, contractJSON)
	exec(alpha, `INSERT INTO basket_member_state (scope, source, basket_id, strategy_id, expected_qty, updated_at_ms, position_id, entry_ms, expiry_ms) VALUES ('paper', 'alpha', 'xs-gone', ?, 0, 1, 'p-gone', 1, 2)`, sid)
	if err := reload(); err == nil || !strings.Contains(err.Error(), "was removed from the config") || !strings.Contains(err.Error(), "holding-horizon") {
		t.Fatalf("a removed basket with lifecycle evidence must refuse at zero expected quantity, got %v", err)
	}
	exec(alpha, `UPDATE basket_member_state SET position_id = '', suppress_through_ms = 5 WHERE basket_id = 'xs-gone'`)
	if err := reload(); err != nil {
		t.Fatalf("a suppression deadline alone is not recovery evidence: %v", err)
	}
}

func TestBasketLifecycleRuntimeFreeSavesNeverStartALifecycle(t *testing.T) {
	env, expiry := basketExpiredEnv(t)
	cfg := env.cfg
	rows := func(read func(string) (basketLifecycle, float64, bool)) map[string]basketLifecycle {
		t.Helper()
		out := map[string]basketLifecycle{}
		for _, m := range basketTestMembers {
			l, _, ok := read(m.ID)
			if !ok {
				t.Fatalf("%s has no stored lifecycle row", m.ID)
			}
			out[m.ID] = l
		}
		return out
	}
	before := rows(env.lifecycleRow)
	for id, l := range before {
		if !l.active() {
			t.Fatalf("%s must hold an active lifecycle before the runtime-free saves: %+v", id, l)
		}
	}
	fresh, _, err := LoadStateWithStore(cfg, env.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range basketTestMembers {
		s := fresh.Strategies[m.ID]
		if s == nil || s.Basket != nil || s.Positions[m.Coin] == nil {
			t.Fatalf("want a pointer-free book that holds %s: %+v", m.Coin, s)
		}
		if err := env.store.SaveStrategyBook(s); err != nil {
			t.Fatal(err)
		}
	}
	saveAll := func(store *StateStore, state *AppState) {
		t.Helper()
		for p, err := range store.SaveAll(state) {
			if err != nil {
				t.Fatalf("SaveAll %s: %v", partitionLabel(p), err)
			}
		}
	}
	saveAll(env.store, fresh)
	if after := rows(env.lifecycleRow); !reflect.DeepEqual(after, before) {
		t.Fatalf("a runtime-free save of an unchanged open position changed the lifecycle:\nbefore %+v\nafter  %+v", before, after)
	}

	env.bc.stepExpiry(basketCycleInputs{Now: expiry.Add(time.Minute), Snapshot: basketTestSnapshot(expiry.Add(time.Minute), 60, basketTestMarks(), 5)})
	if !env.allFlat() {
		t.Fatal("the expiry pass must flatten every member before the basket is removed")
	}
	settled := rows(env.lifecycleRow)
	env.store.Close()

	removed := loadBasketTestConfig(t, func(root map[string]interface{}) {
		root["db_file"] = cfg.DBFile
		delete(root, "baskets")
		for _, raw := range root["strategies"].([]interface{}) {
			sc := raw.(map[string]interface{})
			delete(sc, "basket_id")
			delete(sc, "closed_bar_decisions")
			args := sc["args"].([]interface{})
			sc["args"] = []interface{}{"rsi", args[1], "1h", "--mode=paper"}
		}
	})
	store := openSourceStore(t, removed)
	reload := func() (*AppState, error) {
		t.Helper()
		loaded, _, err := LoadStateWithStore(removed, store)
		if err != nil {
			return nil, err
		}
		_, err = store.LoadBaskets(loaded, removed)
		return loaded, err
	}
	state, err := reload()
	if err != nil {
		t.Fatalf("a flat, resolved removed basket must load: %v", err)
	}
	btc := state.Strategies["hl-bsk-btc"]
	if btc == nil || btc.Basket != nil || btc.Positions["BTC"] != nil {
		t.Fatalf("want an ordinary flat hl-bsk-btc book with no basket runtime: %+v", btc)
	}
	btc.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 60000, Side: "long", Multiplier: 1, Leverage: 1, OwnerStrategyID: "hl-bsk-btc", OpenedAt: expiry.Add(2 * time.Hour)}
	saveAll(store, state)
	sid, err := store.primary().toStorageID("hl-bsk-btc")
	if err != nil {
		t.Fatal(err)
	}
	row := store.primary().db.QueryRow(`SELECT `+basketMemberStateBaseColumns+`, `+basketMemberStateLifecycleSelect+` FROM basket_member_state WHERE basket_id = 'xs-test' AND strategy_id = ?`, sid)
	_, _, _, l, err := scanBasketMemberState(row.Scan)
	if err != nil {
		t.Fatal(err)
	}
	if l.active() || !reflect.DeepEqual(l, settled["hl-bsk-btc"]) {
		t.Fatalf("a runtime-free save started a lifecycle for the removed basket: %+v, want the settled row %+v", l, settled["hl-bsk-btc"])
	}
	store.Close()
	store = openSourceStore(t, removed)
	if _, err := reload(); err != nil {
		t.Fatalf("the next startup must not refuse the removed basket: %v", err)
	}
}
