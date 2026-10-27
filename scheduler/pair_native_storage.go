package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const pairNativeResolutionDDL = `CREATE TABLE IF NOT EXISTS pair_native_resolutions (
    resolution_id  TEXT PRIMARY KEY,
    scope          TEXT NOT NULL,
    source         TEXT NOT NULL DEFAULT '',
    pair_id        TEXT NOT NULL,
    strategy_id    TEXT NOT NULL,
    kind           TEXT NOT NULL,
    obligation_id  TEXT NOT NULL,
    attempt_id     TEXT NOT NULL DEFAULT '',
    mode           TEXT NOT NULL,
    decided_by     TEXT NOT NULL,
    operator       TEXT NOT NULL DEFAULT '',
    reason         TEXT NOT NULL DEFAULT '',
    revision       TEXT NOT NULL DEFAULT '',
    decided_at_ms  INTEGER NOT NULL,
    committed_at_ms INTEGER NOT NULL,
    record_json    TEXT NOT NULL,
    alert_status   TEXT NOT NULL DEFAULT '',
    UNIQUE(scope, source, pair_id, kind, strategy_id, obligation_id)
)`

const (
	pairNativeAlertQueued     = "queued"
	pairNativeAlertDispatched = "dispatched"
)

var errPairNativeResolved = errors.New("the native obligation already has a committed resolution")

func (sdb *StateDB) migratePairNativeSchema() error {
	if _, err := sdb.db.Exec(pairNativeResolutionDDL); err != nil {
		return fmt.Errorf("create pair native resolution table: %w", err)
	}
	return nil
}

func (sdb *StateDB) loadPairNativeRecordsInto(out *basketFileRecords) error {
	exists, err := tableExists(sdb, "pair_native_resolutions")
	if err != nil || !exists {
		return err
	}
	rows, err := sdb.db.Query(`SELECT scope, source, pair_id, strategy_id, record_json, alert_status FROM pair_native_resolutions
		ORDER BY scope, source, pair_id, committed_at_ms, resolution_id`)
	if err != nil {
		return fmt.Errorf("read pair native resolutions in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope, source, pairID, sid, raw, alert string
		if err := rows.Scan(&scope, &source, &pairID, &sid, &raw, &alert); err != nil {
			return err
		}
		var r pairNativeResolution
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return fmt.Errorf("pair %s native resolution record is unreadable: %w", pairID, err)
		}
		procID, mapped := sdb.mapStoredStrategyID(sid)
		if !mapped {
			out.Unmapped = append(out.Unmapped, fmt.Sprintf("pair %s native resolution %s for stored strategy %q", pairID, r.ID, sid))
			continue
		}
		r.MemberID = procID
		r.AlertStatus = alert
		k := basketKey{Partition: RiskPartition{Scope: PortfolioScope(scope), Source: source}, BasketID: pairID}
		if out.NativeResolutions == nil {
			out.NativeResolutions = make(map[basketKey][]pairNativeResolution)
		}
		out.NativeResolutions[k] = append(out.NativeResolutions[k], r)
	}
	return rows.Err()
}

func insertPairNativeResolutionTx(tx *sql.Tx, rt *basketRuntime, sids map[string]string, r pairNativeResolution) error {
	sid, ok := sids[r.MemberID]
	if !ok {
		return fmt.Errorf("native resolution %s names %s, which is not a member of pair %s", r.ID, r.MemberID, rt.Contract.BasketID)
	}
	alert := r.AlertStatus
	r.AlertStatus = ""
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var taken int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pair_native_resolutions WHERE resolution_id = ? OR (scope = ? AND source = ? AND pair_id = ? AND kind = ? AND strategy_id = ? AND obligation_id = ?)`,
		r.ID, string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID, r.Kind, sid, r.ObligationID).Scan(&taken); err != nil {
		return fmt.Errorf("read native resolution identity: %w", err)
	}
	if taken > 0 {
		return fmt.Errorf("native resolution %s for %s %s %s: %w", r.ID, r.Kind, r.MemberID, r.ObligationID, errPairNativeResolved)
	}
	_, err = tx.Exec(`INSERT INTO pair_native_resolutions (resolution_id, scope, source, pair_id, strategy_id, kind, obligation_id, attempt_id, mode, decided_by,
		    operator, reason, revision, decided_at_ms, committed_at_ms, record_json, alert_status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID, sid, r.Kind, r.ObligationID, r.AttemptID, r.Mode, r.DecidedBy,
		r.Operator, r.Reason, r.Revision, r.DecidedAtMs, r.CommittedAtMs, string(body), alert)
	if err != nil {
		return fmt.Errorf("insert native resolution %s: %w", r.ID, err)
	}
	return nil
}

func writePairProgressValueTx(tx *sql.Tx, rt *basketRuntime, p pairProgress) error {
	if err := checkSavedPairNativeBoundsTx(tx, rt, p); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE basket_contracts SET progress_json = ?, progress_seq = ?
		WHERE scope = ? AND source = ? AND basket_id = ? AND contract_fingerprint = ? AND progress_seq <= ?`,
		p.JSON(), p.Seq, string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID, rt.Contract.Fingerprint(), p.Seq)
	if err != nil {
		return fmt.Errorf("write pair %s progress: %w", rt.Contract.BasketID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("pair %s progress (seq %d): %w; a newer saved progress or a changed contract exists", rt.Contract.BasketID, p.Seq, errPairGuardMiss)
	}
	return nil
}

func writePairProgressFinalizingTx(tx *sql.Tx, rt *basketRuntime, sids map[string]string, books map[string]*StrategyState) error {
	rt.Pair.finalizing = nil
	fin := pairNativeFinalizable(rt, books)
	if len(fin) == 0 {
		return writePairProgressTx(tx, rt)
	}
	for _, r := range fin {
		if err := insertPairNativeResolutionTx(tx, rt, sids, r); err != nil {
			return err
		}
	}
	if err := writePairProgressValueTx(tx, rt, pairNativeProgressAfter(rt.Pair.Progress, fin)); err != nil {
		return err
	}
	rt.Pair.finalizing = fin
	return nil
}

type pairNativeRowUpdate struct {
	action   *basketAction
	from     []string
	to       string
	zero     bool
	fills    []pairConfirmedFill
	savedRaw string
}

type pairNativeFillGuard struct {
	memberID string
	fills    []pairConfirmedFill
}

func (st *StateStore) CommitPairNative(rt *basketRuntime, rows []pairNativeResolution, updates []pairNativeRowUpdate, guards []pairNativeFillGuard) error {
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	return db.commitPairNativeTx(rt, rows, updates, guards)
}

func (sdb *StateDB) commitPairNativeTx(rt *basketRuntime, rows []pairNativeResolution, updates []pairNativeRowUpdate, guards []pairNativeFillGuard) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if !rt.isPair() {
		return fmt.Errorf("a native resolution needs a pair runtime")
	}
	sids, err := sdb.checkBasketOwnership(rt)
	if err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin native resolution: %w", err)
	}
	defer tx.Rollback()
	var savedFP string
	if err := tx.QueryRow(`SELECT contract_fingerprint FROM basket_contracts WHERE scope = ? AND source = ? AND basket_id = ?`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID).Scan(&savedFP); err != nil {
		return fmt.Errorf("read pair %s contract before a native resolution: %w", rt.Contract.BasketID, err)
	}
	if savedFP != rt.Contract.Fingerprint() {
		return fmt.Errorf("pair %s saved contract differs from the running contract", rt.Contract.BasketID)
	}
	for _, g := range guards {
		sid, ok := sids[g.memberID]
		if !ok {
			return fmt.Errorf("native fill guard names %s, which is not a member of pair %s", g.memberID, rt.Contract.BasketID)
		}
		if err := pairFillsUnownedTx(tx, rt, sid, "", g.fills); err != nil {
			return err
		}
	}
	for _, u := range updates {
		if err := applyPairNativeRowUpdateTx(tx, rt, sids, u); err != nil {
			return err
		}
	}
	for _, r := range rows {
		if err := insertPairNativeResolutionTx(tx, rt, sids, r); err != nil {
			return err
		}
	}
	if _, err := sdb.writePairTx(tx, rt, nil, rt.auditNowMs()); err != nil {
		return err
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	markBasketDurable(rt, nil, nil)
	return nil
}

func pairFillsUnownedTx(tx *sql.Tx, rt *basketRuntime, sid, excludeActionID string, fills []pairConfirmedFill) error {
	if len(fills) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, f := range fills {
		want[f.FillID] = true
	}
	rows, err := tx.Query(`SELECT action_id, purpose, fill_coverage_json FROM basket_member_actions WHERE scope = ? AND source = ? AND basket_id = ? AND strategy_id = ?
		AND group_mode = 'pair' AND fill_coverage_json != '' AND action_id != ?`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID, sid, excludeActionID)
	if err != nil {
		return fmt.Errorf("read saved pair fills: %w", err)
	}
	defer rows.Close()
	conflict := ""
	for rows.Next() {
		var id, purpose, raw string
		if err := rows.Scan(&id, &purpose, &raw); err != nil {
			return err
		}
		saved, err := pairParseFills(raw)
		if err != nil {
			return fmt.Errorf("saved pair action %s fill records are unreadable: %w", id, err)
		}
		for _, f := range saved {
			if want[f.FillID] && conflict == "" {
				conflict = fmt.Sprintf("native fill %s is already saved on pair action %s (%s); a fill is accounted once", f.FillID, id, purpose)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if conflict != "" {
		return errors.New(conflict)
	}
	return nil
}

func applyPairNativeRowUpdateTx(tx *sql.Tx, rt *basketRuntime, sids map[string]string, u pairNativeRowUpdate) error {
	a := u.action
	if a == nil || !a.pairNative() || len(u.from) == 0 {
		return fmt.Errorf("a native recovery transition needs a native pair action and its permitted prior statuses")
	}
	sid, ok := sids[a.StrategyID]
	if !ok {
		return fmt.Errorf("native action %s names %s, which is not a member of pair %s", a.ActionID, a.StrategyID, rt.Contract.BasketID)
	}
	if err := pairFillsUnownedTx(tx, rt, sid, a.ActionID, u.fills); err != nil {
		return err
	}
	marks := make([]string, len(u.from))
	args := []interface{}{u.to, a.UpdatedAtMs, a.ActionID, sid, a.RequestJSON, a.ClientOrderID, u.savedRaw}
	for i, s := range u.from {
		marks[i] = "?"
		args = append(args, s)
	}
	q := `UPDATE basket_member_actions SET status = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE action_id = ? AND strategy_id = ? AND group_mode = 'pair' AND purpose IN ('pair_kill_switch','pair_operator')
		  AND request_json = ? AND client_order_id = ? AND fill_coverage_json = ? AND status IN (` + strings.Join(marks, ",") + `)`
	if u.zero {
		q += ` AND confirmed_qty = 0 AND applied_qty = 0`
	}
	res, err := tx.Exec(q, args...)
	if err != nil {
		return fmt.Errorf("native recovery transition for %s: %w", a.ActionID, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("native recovery transition for %s (%v to %s): %w; the saved row is not in a permitted state", a.ActionID, u.from, u.to, errPairGuardMiss)
	}
	return nil
}

func (st *StateStore) MarkPairNativeAlerts(rt *basketRuntime, ids []string, status string) error {
	if len(ids) == 0 {
		return nil
	}
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	if db.readOnly {
		return fmt.Errorf("%s state file is open read-only", db.storageRoleOf())
	}
	sort.Strings(ids)
	marks := make([]string, len(ids))
	args := []interface{}{status, string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID}
	for i, id := range ids {
		marks[i] = "?"
		args = append(args, id)
	}
	_, err = db.db.Exec(`UPDATE pair_native_resolutions SET alert_status = ? WHERE scope = ? AND source = ? AND pair_id = ? AND alert_status = 'queued'
		AND resolution_id IN (`+strings.Join(marks, ",")+`)`, args...)
	return err
}
