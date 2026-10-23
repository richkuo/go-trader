package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func pairOldSchemaFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	stmts := []string{entryDecisionSchemaDDL, basketSchemaDDL}
	stmts = append(stmts, basketLifecycleMigrations...)
	for _, ddl := range stmts {
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatalf("old schema: %v", err)
		}
	}
	exec := func(q string, args ...interface{}) {
		if _, err := raw.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO basket_member_actions (action_id, scope, source, basket_id, deadline_ms, strategy_id, symbol, direction, seq, order_side, position_side, status, requested_qty, provider, account_key, client_order_id, created_at_ms, updated_at_ms)
		VALUES ('ba-old-open', 'paper', '', 'xs-old', 100, 'hl-a', 'BTC', 'increase', 1, 'buy', 'long', 'submitting', 0.01, 'paper', 'paper:paper', 'bp-1', 1, 1)`)
	exec(`INSERT INTO basket_member_actions (action_id, scope, source, basket_id, deadline_ms, strategy_id, symbol, direction, seq, order_side, position_side, status, requested_qty, provider, account_key, client_order_id, confirmed_fee, applied_fee, created_at_ms, updated_at_ms, purpose)
		VALUES ('ba-old-done', 'paper', '', 'xs-old', 50, 'hl-a', 'BTC', 'reduce', 1, 'sell', 'long', 'committed', 0.01, 'paper', 'paper:paper', 'bp-0', 0.2, 0.2, 1, 1, 'expiry')`)
	exec(`INSERT INTO entry_order_intents (intent_id, scope, source, strategy_id, symbol, interval_ms, target_side, bar_open_ms, decision_boundary_ms, cutoff_ms, provider, account_key, client_order_id, status, request_json, created_at_ms, updated_at_ms)
		VALUES ('ei-basket', 'live', '', 'hl-a', 'BTC', 3600000, 'long', 1, 2, 3, 'hyperliquid', 'hyperliquid:mainnet:0xabc', 'c-basket', 'submitting', '{"version":1,"kind":"basket:ba-old-open","signal":1}', 1, 1)`)
	exec(`INSERT INTO entry_order_intents (intent_id, scope, source, strategy_id, symbol, interval_ms, target_side, bar_open_ms, decision_boundary_ms, cutoff_ms, provider, account_key, client_order_id, status, request_json, created_at_ms, updated_at_ms)
		VALUES ('ei-plain', 'live', '', 'hl-b', 'ETH', 3600000, 'short', 1, 2, 3, 'hyperliquid', 'hyperliquid:mainnet:0xabc', 'c-plain', 'committed', '{"version":1,"kind":"open","signal":-1}', 1, 1)`)
	return path
}

var pairInsertSeq int

func pairInsertAction(db *sql.DB, id, group, purpose, basket, sid, provider, account, instrument, cloid, status string) error {
	pairInsertSeq++
	request := ""
	if group == pairGroupMode {
		request = `{"v":1}`
	}
	_, err := db.Exec(`INSERT INTO basket_member_actions (action_id, scope, source, basket_id, deadline_ms, strategy_id, symbol, direction, seq, order_side, position_side, status, requested_qty, provider, account_key, client_order_id, created_at_ms, updated_at_ms, purpose, group_mode, instrument, request_json)
		VALUES (?, 'paper', '', ?, 7, ?, 'BTC', 'reduce', ?, 'sell', 'long', ?, 0.01, ?, ?, ?, 1, 1, ?, ?, ?, ?)`,
		id, basket, sid, pairInsertSeq, status, provider, account, cloid, purpose, group, instrument, request)
	return err
}

func TestBasketPairStorageMigratesOldSchemasTwice(t *testing.T) {
	path := pairOldSchemaFile(t)
	before := pairFileDigest(t, path)
	ro, err := openStateDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := ro.loadBasketRecords()
	if err != nil {
		t.Fatalf("read-only basket projection on an unmigrated file: %v", err)
	}
	k := basketKey{Partition: defaultPaperPartition, BasketID: "xs-old"}
	if len(recs.Actions[k]) != 1 || recs.Actions[k][0].GroupMode != basketGroupMode {
		t.Fatalf("an unmigrated file must project the basket group mode: %+v", recs.Actions[k])
	}
	in, _, err := ro.readEntryIntent("ei-basket")
	if err != nil || in.OwnerMode != basketGroupMode || in.DecisionKind != entryDecisionKindCandle {
		t.Fatalf("an unmigrated intent must project its saved owner: %+v %v", in, err)
	}
	ro.Close()
	if after := pairFileDigest(t, path); after != before {
		t.Fatal("read-only inspection changed the unmigrated file")
	}
	for run := 1; run <= 2; run++ {
		db, err := OpenStateDB(path)
		if err != nil {
			t.Fatalf("migration run %d: %v", run, err)
		}
		count := func(q string, args ...interface{}) int {
			var n int
			if err := db.db.QueryRow(q, args...).Scan(&n); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			return n
		}
		if n := count(`SELECT COUNT(*) FROM basket_member_actions WHERE group_mode = 'basket'`); n != run+1 {
			t.Fatalf("run %d: %d basket actions, want %d", run, n, run+1)
		}
		var fee float64
		var purpose string
		if err := db.db.QueryRow(`SELECT applied_fee, purpose FROM basket_member_actions WHERE action_id = 'ba-old-done'`).Scan(&fee, &purpose); err != nil || fee != 0.2 || purpose != basketPurposeExpiry {
			t.Fatalf("run %d: the migrated legacy row lost data: %v %q %v", run, fee, purpose, err)
		}
		if n := count(`SELECT COUNT(*) FROM entry_order_intents`); n != 2 {
			t.Fatalf("run %d: %d intents, want 2", run, n)
		}
		var owner, kind string
		if err := db.db.QueryRow(`SELECT owner_mode, decision_kind FROM entry_order_intents WHERE intent_id = 'ei-basket'`).Scan(&owner, &kind); err != nil || owner != basketGroupMode || kind != entryDecisionKindCandle {
			t.Fatalf("run %d: the basket-linked intent was classified %q/%q (%v)", run, owner, kind, err)
		}
		if err := db.db.QueryRow(`SELECT owner_mode FROM entry_order_intents WHERE intent_id = 'ei-plain'`).Scan(&owner); err != nil || owner != "" {
			t.Fatalf("run %d: the ordinary intent was classified %q (%v)", run, owner, err)
		}
		if err := pairInsertAction(db.db, fmt.Sprintf("ba-second-%d", run), basketGroupMode, basketPurposeRebalance, "xs-old", "hl-a", "paper", "paper:paper", "", fmt.Sprintf("bp-x%d", run), "reserved"); err == nil {
			t.Fatalf("run %d: an unchanged legacy basket must still refuse its second unresolved action", run)
		}
		if err := pairInsertAction(db.db, fmt.Sprintf("ba-done-%d", run), basketGroupMode, basketPurposeRebalance, "xs-old", "hl-a", "paper", "paper:paper", "", fmt.Sprintf("bp-y%d", run), "committed"); err != nil {
			t.Fatalf("run %d: a resolved basket row beside the pending one must still be accepted: %v", run, err)
		}
		if run == 1 {
			if err := pairInsertAction(db.db, "pa-elective", pairGroupMode, pairPurposeEntry, "pr", "m-hl", "hyperliquid", "synthetic:a", "BTC", "p-1", "unresolved"); err != nil {
				t.Fatalf("a pair elective action beside a pending basket action must be allowed: %v", err)
			}
			if err := pairInsertAction(db.db, "pa-prot-hl", pairGroupMode, pairPurposeProtective, "pr", "m-hl", "hyperliquid", "synthetic:a", "BTC", "p-2", "submitting"); err != nil {
				t.Fatalf("the hyperliquid protective reduction must be allowed: %v", err)
			}
			if err := pairInsertAction(db.db, "pa-prot-okx", pairGroupMode, pairPurposeKillSwitch, "pr", "m-okx", "okx", "synthetic:b", "BTC-USDT-SWAP", "p-3", "reserved"); err != nil {
				t.Fatalf("the okx protective reduction must be allowed: %v", err)
			}
		}
		if err := pairInsertAction(db.db, fmt.Sprintf("pa-elective-dup-%d", run), pairGroupMode, pairPurposeExit, "pr", "m-okx", "okx", "synthetic:b", "BTC-USDT-SWAP", fmt.Sprintf("p-d%d", run), "reserved"); err == nil {
			t.Fatalf("run %d: a second unresolved elective pair action must be refused", run)
		}
		if err := pairInsertAction(db.db, fmt.Sprintf("pa-elective-done-%d", run), pairGroupMode, pairPurposeExit, "pr", "m-okx", "okx", "synthetic:b", "BTC-USDT-SWAP", fmt.Sprintf("p-f%d", run), "no_fill"); err != nil {
			t.Fatalf("run %d: a terminal elective pair row must be accepted: %v", run, err)
		}
		if err := pairInsertAction(db.db, fmt.Sprintf("pa-prot-dup-%d", run), pairGroupMode, pairPurposeOperator, "pr", "m-hl", "hyperliquid", "synthetic:a", "BTC", fmt.Sprintf("p-e%d", run), "reserved"); err == nil {
			t.Fatalf("run %d: a second unresolved protective reduction on one account and instrument must be refused", run)
		}
		if n := count(`SELECT COUNT(*) FROM basket_member_actions WHERE group_mode = 'pair' AND status IN ('reserved','submitting','unresolved','settling')`); n != 3 {
			t.Fatalf("run %d: %d unresolved pair rows survived, want 3", run, n)
		}
		if _, err := db.db.Exec(`UPDATE basket_member_actions SET group_mode = 'basket' WHERE action_id = 'pa-elective'`); err == nil {
			t.Fatalf("run %d: the group mode must be immutable", run)
		}
		if _, err := db.db.Exec(`UPDATE entry_order_intents SET owner_mode = '' WHERE intent_id = 'ei-basket'`); err == nil {
			t.Fatalf("run %d: the saved intent owner must be immutable", run)
		}
		if _, err := db.db.Exec(`INSERT INTO entry_order_intents (intent_id, scope, source, strategy_id, symbol, interval_ms, target_side, bar_open_ms, decision_boundary_ms, cutoff_ms, provider, account_key, client_order_id, status, created_at_ms, updated_at_ms, decision_kind)
			VALUES (?, 'paper', '', 'x', 'BTC', 0, 'long', 0, 0, 0, 'okx', 'a', ?, 'reserved', 1, 1, 'candle')`, fmt.Sprintf("ei-bad-%d", run), fmt.Sprintf("c-bad-%d", run)); err == nil {
			t.Fatalf("run %d: a candle intent without an interval must be refused", run)
		}
		db.Close()
	}
}

type pairCommitFault struct {
	n    int
	from int
	to   int
}

func (f *pairCommitFault) install(t *testing.T) {
	prev := storeCommitHook
	storeCommitHook = func(role storageRole) error {
		f.n++
		if f.n >= f.from && (f.to == 0 || f.n <= f.to) {
			return fmt.Errorf("injected commit fault %d", f.n)
		}
		return nil
	}
	t.Cleanup(func() { storeCommitHook = prev })
}

func (e *pairTestEnv) pairRows() (actions, intents, uses int) {
	return e.count(`SELECT COUNT(*) FROM basket_member_actions WHERE group_mode = 'pair'`),
		e.count(`SELECT COUNT(*) FROM entry_order_intents WHERE owner_mode = 'pair'`),
		e.count(`SELECT COUNT(*) FROM pair_quote_event_uses`)
}

func TestBasketPairReservationIsAtomicUnderFaults(t *testing.T) {
	t.Run("reservation commit fails", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		at := pairTestStart()
		e.slot(at, "in-0")
		f := &pairCommitFault{from: 3, to: 3}
		f.install(t)
		e.step(at.Add(time.Second))
		if a, i, u := e.pairRows(); a != 0 || i != 0 || u != 0 {
			t.Fatalf("a failed reservation left %d actions, %d intents and %d quote uses", a, i, u)
		}
		if len(e.venues.okx.submits) != 0 || len(e.rt().Actions) != 0 {
			t.Fatal("a failed reservation must send nothing and keep no action in memory")
		}
	})
	t.Run("crash between reservation and submitting", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		at := pairTestStart()
		e.slot(at, "in-0")
		f := &pairCommitFault{from: 4}
		f.install(t)
		e.step(at.Add(time.Second))
		if len(e.venues.okx.submits) != 0 {
			t.Fatal("a failed submitting transition must send nothing")
		}
		var actionStatus, intentStatus, useStatus string
		if err := e.db().db.QueryRow(`SELECT status FROM basket_member_actions WHERE group_mode = 'pair'`).Scan(&actionStatus); err != nil {
			t.Fatal(err)
		}
		if err := e.db().db.QueryRow(`SELECT status FROM entry_order_intents WHERE owner_mode = 'pair'`).Scan(&intentStatus); err != nil {
			t.Fatal(err)
		}
		if err := e.db().db.QueryRow(`SELECT status FROM pair_quote_event_uses`).Scan(&useStatus); err != nil {
			t.Fatal(err)
		}
		if actionStatus != basketActionReserved || intentStatus != entryIntentReserved || useStatus != pairUseReserved {
			t.Fatalf("the linked reservation must stay all-reserved, got %s/%s/%s", actionStatus, intentStatus, useStatus)
		}
		storeCommitHook = nil
		e.restart()
		e.step(at.Add(2 * time.Second))
		if err := e.db().db.QueryRow(`SELECT status FROM basket_member_actions WHERE group_mode = 'pair' AND action_id = ?`, e.actionsByPurpose(pairPurposeEntry)[0].ActionID).Scan(&actionStatus); err != nil {
			t.Fatal(err)
		}
		if err := e.db().db.QueryRow(`SELECT status FROM entry_order_intents WHERE owner_mode = 'pair'`).Scan(&intentStatus); err != nil {
			t.Fatal(err)
		}
		if err := e.db().db.QueryRow(`SELECT status FROM pair_quote_event_uses`).Scan(&useStatus); err != nil {
			t.Fatal(err)
		}
		if actionStatus != basketActionNoFill || intentStatus != entryIntentNoFill || useStatus != pairUseReleased {
			t.Fatalf("restart must end the never-sent reservation as one unit, got %s/%s/%s", actionStatus, intentStatus, useStatus)
		}
		if len(e.venues.okx.submits) != 0 {
			t.Fatal("a never-sent reservation must never be sent after the restart")
		}
	})
	t.Run("evidence save fails after the fill", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		at := pairTestStart()
		e.slot(at, "in-0")
		f := &pairCommitFault{from: 5}
		f.install(t)
		e.step(at.Add(time.Second))
		var status string
		var applied float64
		if err := e.db().db.QueryRow(`SELECT status, applied_qty FROM basket_member_actions WHERE group_mode = 'pair'`).Scan(&status, &applied); err != nil {
			t.Fatal(err)
		}
		if status != basketActionSubmitting || applied != 0 {
			t.Fatalf("a failed evidence save must leave the submitted action as saved, got %s %v", status, applied)
		}
		storeCommitHook = nil
		e.restart()
		a := e.actionsByPurpose(pairPurposeEntry)[0]
		pairQueryVenue(e, a, basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59800, Terminal: true})
		e.step(at.Add(2 * time.Second))
		e.assertBooks("recovered", 0, -0.06)
		if n := e.bookedTrades(pairTestOKX); n != 1 {
			t.Fatalf("the recovered fill booked %d trades, want 1", n)
		}
	})
	t.Run("book save fails after evidence", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterHoldRunner(e, 0.06)
		at := pairTestStart()
		e.slot(at, "in-0")
		f := &pairCommitFault{from: 6, to: 6}
		f.install(t)
		e.step(at.Add(time.Second))
		var status string
		var applied, expected float64
		if err := e.db().db.QueryRow(`SELECT status, applied_qty FROM basket_member_actions WHERE group_mode = 'pair'`).Scan(&status, &applied); err != nil {
			t.Fatal(err)
		}
		if err := e.db().db.QueryRow(`SELECT COALESCE(SUM(expected_qty), 0) FROM basket_member_state`).Scan(&expected); err != nil {
			t.Fatal(err)
		}
		var use string
		if err := e.db().db.QueryRow(`SELECT status FROM pair_quote_event_uses`).Scan(&use); err != nil {
			t.Fatal(err)
		}
		if status != basketActionSettling || applied != 0 || expected != 0 || use != pairUseReserved {
			t.Fatalf("a progress-only write advanced book-dependent fields: %s applied %v expected %v use %s", status, applied, expected, use)
		}
		e.restart()
		e.step(at.Add(2 * time.Second))
		e.assertBooks("settled once", 0, -0.06)
		if n := e.bookedTrades(pairTestOKX); n != 1 {
			t.Fatalf("the confirmed delta booked %d times, want once", n)
		}
		if err := e.db().db.QueryRow(`SELECT status FROM pair_quote_event_uses`).Scan(&use); err != nil || use != pairUseConsumed {
			t.Fatalf("the quote use must be consumed with the booking, got %s (%v)", use, err)
		}
	})
}

func TestBasketPairSubsetSavesAdvanceOnlyTheirMembers(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	at := pairSlotAt(pairTestStart(), 1)
	e.slot(at, "in-1")
	f := &pairCommitFault{from: 5, to: 5}
	f.install(t)
	e.step(at.Add(time.Second))
	storeCommitHook = nil
	hedge := e.actionsByPurpose(pairPurposeEntry)[1]
	if hedge.StrategyID != pairTestHL || !basketQtyEqual(hedge.AppliedQty, 0.06) {
		t.Fatalf("the hedge must be booked in memory: %+v", hedge)
	}
	readApplied := func(id string) float64 {
		var v float64
		if err := e.db().db.QueryRow(`SELECT applied_qty FROM basket_member_actions WHERE action_id = ?`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if readApplied(hedge.ActionID) != 0 {
		t.Fatal("the failed book save must not have applied the hedge")
	}
	f2 := &pairCommitFault{from: 1}
	f2.install(t)
	e.mu.Lock()
	e.rt().needsDurable = false
	e.bc.executePair(e.rt(), basketCycleInputs{Now: at.Add(2 * time.Second)}, *e.rt().pairLastInputs)
	completed := e.rt().Rebalances[pairTestStart().UnixMilli()].Status == basketRebalanceCompleted
	e.mu.Unlock()
	storeCommitHook = nil
	if !completed {
		t.Fatal("setup: the entry slot must complete in memory")
	}
	e.mu.Lock()
	err := e.store.SaveStrategyBook(e.state.Strategies[pairTestOKX])
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if readApplied(hedge.ActionID) != 0 {
		t.Fatal("a single-book save of the other member advanced the hedge's applied total")
	}
	var slotStatus string
	if err := e.db().db.QueryRow(`SELECT status FROM basket_rebalances WHERE deadline_ms = ?`, pairTestStart().UnixMilli()).Scan(&slotStatus); err != nil {
		t.Fatal(err)
	}
	if slotStatus == basketRebalanceCompleted {
		t.Fatal("a subset save marked whole-pair completion")
	}
	var hlExpected float64
	sid, _ := e.db().toStorageID(pairTestHL)
	_ = e.db().db.QueryRow(`SELECT expected_qty FROM basket_member_state WHERE strategy_id = ?`, sid).Scan(&hlExpected)
	if hlExpected != 0 {
		t.Fatal("a subset save advanced an omitted member's expected quantity")
	}
	e.mu.Lock()
	err = e.store.SaveStrategyBook(e.state.Strategies[pairTestHL])
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !basketQtyEqual(readApplied(hedge.ActionID), 0.06) {
		t.Fatal("the member's own book save must apply its hedge")
	}
}

func TestBasketPairGuardMissFailsTheTransaction(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	if _, err := e.db().db.Exec(`UPDATE basket_member_actions SET status = 'committed' WHERE action_id = ?`, a.ActionID); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := e.db().db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.rt().touchPairProgress()
	err := e.store.PersistBasketProgress(e.rt())
	e.mu.Unlock()
	if !errors.Is(err, errPairGuardMiss) {
		t.Fatalf("a guarded write that misses must fail its transaction, got %v", err)
	}
	var after string
	if err := e.db().db.QueryRow(`SELECT progress_json FROM basket_contracts WHERE basket_id = ?`, pairTestID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("a failed pair transaction committed part of its writes")
	}
}
