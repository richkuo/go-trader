package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const robinhoodOrderSchemaDDL = `
CREATE TABLE IF NOT EXISTS robinhood_order_attempts (
    attempt_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope = 'live'),
    source TEXT NOT NULL DEFAULT '' CHECK (source = ''),
    strategy_id TEXT NOT NULL,
    account_key TEXT NOT NULL CHECK (account_key != ''),
    symbol TEXT NOT NULL CHECK (symbol != ''),
    origin TEXT NOT NULL CHECK (origin IN ('signal','circuit_breaker','kill_switch')),
    side TEXT NOT NULL CHECK (side IN ('buy','sell')),
    ref_id TEXT NOT NULL CHECK (ref_id != ''),
    venue_order_id TEXT NOT NULL DEFAULT '',
    venue_account_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('submitting','open','unknown','settling','settled','no_fill')),
    request_json TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '',
    hold_reason TEXT NOT NULL DEFAULT '',
    confirmed_qty REAL NOT NULL DEFAULT 0,
    confirmed_avg_px REAL NOT NULL DEFAULT 0,
    confirmed_fee REAL NOT NULL DEFAULT 0,
    applied_qty REAL NOT NULL DEFAULT 0,
    applied_fee REAL NOT NULL DEFAULT 0,
    settlement_seq INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_rh_attempts_ref
    ON robinhood_order_attempts(account_key, ref_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_rh_attempts_one_unresolved_coin
    ON robinhood_order_attempts(account_key, symbol)
    WHERE status IN ('submitting','open','unknown','settling');
CREATE UNIQUE INDEX IF NOT EXISTS idx_rh_attempts_one_unresolved_strategy
    ON robinhood_order_attempts(scope, source, strategy_id)
    WHERE status IN ('submitting','open','unknown','settling');
`

const robinhoodAttemptColumns = `attempt_id, scope, source, strategy_id, account_key, symbol, origin, side, ref_id,
    venue_order_id, venue_account_id, status, request_json, evidence_json, hold_reason,
    confirmed_qty, confirmed_avg_px, confirmed_fee, applied_qty, applied_fee, settlement_seq,
    created_at_ms, updated_at_ms`

const robinhoodUnresolvedStatusSQL = `('submitting','open','unknown','settling')`

const robinhoodStatusRankSQL = `CASE %s WHEN 'submitting' THEN 0 WHEN 'open' THEN 1 WHEN 'unknown' THEN 1 WHEN 'settling' THEN 2 ELSE 3 END`

func (sdb *StateDB) migrateRobinhoodOrderSchema() error {
	if _, err := sdb.db.Exec(robinhoodOrderSchemaDDL); err != nil {
		return fmt.Errorf("create robinhood order attempt table: %w", err)
	}
	return nil
}

func (sdb *StateDB) robinhoodAttemptOwner(procID string) (string, error) {
	if sdb.ident != nil {
		if owned, ok := sdb.ident.procPartition[procID]; ok && owned != livePartition {
			return "", fmt.Errorf("strategy %q is owned by the %s partition, not live; Robinhood order attempts are live-only", procID, partitionLabel(owned))
		}
	}
	return sdb.toStorageID(procID)
}

func upsertRobinhoodAttemptRow(exec sqlExecer, sid string, a *robinhoodOrderAttempt) error {
	if a == nil {
		return nil
	}
	if robinhoodAttemptStatusRank(a.Status) < 0 {
		return fmt.Errorf("robinhood order attempt %s has unknown status %q", a.AttemptID, a.Status)
	}
	rankNew := fmt.Sprintf(robinhoodStatusRankSQL, "excluded.status")
	rankOld := fmt.Sprintf(robinhoodStatusRankSQL, "status")
	_, err := exec.Exec(`INSERT INTO robinhood_order_attempts (`+robinhoodAttemptColumns+`)
		VALUES (?, 'live', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(attempt_id) DO UPDATE SET
		    venue_order_id = CASE WHEN excluded.venue_order_id != '' THEN excluded.venue_order_id ELSE venue_order_id END,
		    venue_account_id = CASE WHEN excluded.venue_account_id != '' THEN excluded.venue_account_id ELSE venue_account_id END,
		    status = excluded.status,
		    evidence_json = CASE WHEN excluded.evidence_json != '' THEN excluded.evidence_json ELSE evidence_json END,
		    hold_reason = excluded.hold_reason,
		    confirmed_qty = excluded.confirmed_qty,
		    confirmed_avg_px = excluded.confirmed_avg_px,
		    confirmed_fee = excluded.confirmed_fee,
		    applied_qty = excluded.applied_qty,
		    applied_fee = excluded.applied_fee,
		    settlement_seq = MAX(settlement_seq, excluded.settlement_seq),
		    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)
		WHERE `+rankNew+` >= `+rankOld+`
		  AND (status NOT IN ('settled','no_fill') OR status = excluded.status)
		  AND (`+rankNew+` > `+rankOld+` OR excluded.updated_at_ms >= updated_at_ms)
		  AND strategy_id = excluded.strategy_id AND account_key = excluded.account_key
		  AND symbol = excluded.symbol AND ref_id = excluded.ref_id AND side = excluded.side`,
		a.AttemptID, sid, a.AccountKey, a.Symbol, a.Origin, a.Side, a.RefID,
		a.VenueOrderID, a.VenueAccountID, a.Status, a.RequestJSON, a.EvidenceJSON, a.HoldReason,
		a.ConfirmedQty, a.ConfirmedAvgPx, a.ConfirmedFee, a.AppliedQty, a.AppliedFee, a.SettlementSeq,
		a.CreatedAtMs, a.UpdatedAtMs)
	if err != nil {
		return fmt.Errorf("upsert robinhood order attempt %s: %w", a.AttemptID, err)
	}
	return nil
}

func (sdb *StateDB) writeRobinhoodOrderGuardTx(tx *sql.Tx, s *StrategyState, fullPartition bool) error {
	if s == nil || s.RobinhoodOrders == nil || len(s.RobinhoodOrders.Attempts) == 0 {
		return nil
	}
	sid, err := sdb.robinhoodAttemptOwner(s.ID)
	if err != nil {
		return err
	}
	for _, id := range s.RobinhoodOrders.sortedAttemptIDs() {
		a := s.RobinhoodOrders.Attempts[id]
		if a.Status == robinhoodAttemptSettled && a.durableStatus != robinhoodAttemptSettled && a.multiBook && !fullPartition {
			continue
		}
		if err := upsertRobinhoodAttemptRow(tx, sid, a); err != nil {
			return err
		}
	}
	return nil
}

func pruneRobinhoodOrderGuardAfterCommit(s *StrategyState, fullPartition bool) {
	if s == nil || s.RobinhoodOrders == nil {
		return
	}
	for id, a := range s.RobinhoodOrders.Attempts {
		if a.Status == robinhoodAttemptSettled && a.multiBook && !fullPartition && a.durableStatus != robinhoodAttemptSettled {
			continue
		}
		a.durableStatus = a.Status
		if a.unresolved() {
			continue
		}
		delete(s.RobinhoodOrders.Attempts, id)
	}
}

func scanRobinhoodAttempt(scan func(dest ...interface{}) error) (*robinhoodOrderAttempt, string, error) {
	var a robinhoodOrderAttempt
	var scope, source, sid string
	if err := scan(&a.AttemptID, &scope, &source, &sid, &a.AccountKey, &a.Symbol, &a.Origin, &a.Side, &a.RefID,
		&a.VenueOrderID, &a.VenueAccountID, &a.Status, &a.RequestJSON, &a.EvidenceJSON, &a.HoldReason,
		&a.ConfirmedQty, &a.ConfirmedAvgPx, &a.ConfirmedFee, &a.AppliedQty, &a.AppliedFee, &a.SettlementSeq,
		&a.CreatedAtMs, &a.UpdatedAtMs); err != nil {
		return nil, "", err
	}
	if scope != string(ScopeLive) || source != "" {
		return nil, "", fmt.Errorf("robinhood order attempt %s carries partition %s/%s", a.AttemptID, scope, source)
	}
	a.durableStatus = a.Status
	return &a, sid, nil
}

func (sdb *StateDB) loadUnresolvedRobinhoodAttempts() ([]*robinhoodOrderAttempt, []string, error) {
	exists, err := tableExists(sdb, "robinhood_order_attempts")
	if err != nil || !exists {
		return nil, nil, err
	}
	rows, err := sdb.db.Query(`SELECT ` + robinhoodAttemptColumns + ` FROM robinhood_order_attempts
		WHERE status IN ` + robinhoodUnresolvedStatusSQL + ` ORDER BY created_at_ms, attempt_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("read robinhood order attempts in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	defer rows.Close()
	var out []*robinhoodOrderAttempt
	var sids []string
	for rows.Next() {
		a, sid, err := scanRobinhoodAttempt(rows.Scan)
		if err != nil {
			return nil, nil, fmt.Errorf("scan robinhood order attempt in the %s state file: %w", sdb.storageRoleOf(), err)
		}
		out = append(out, a)
		sids = append(sids, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate robinhood order attempts in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	return out, sids, nil
}

func (sdb *StateDB) reserveRobinhoodAttempt(a *robinhoodOrderAttempt) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if a == nil || a.Status != robinhoodAttemptSubmitting {
		return fmt.Errorf("a robinhood order attempt must be reserved as submitting")
	}
	sid, err := sdb.robinhoodAttemptOwner(a.StrategyID)
	if err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin robinhood order reservation: %w", err)
	}
	defer tx.Rollback()
	var held string
	err = tx.QueryRow(`SELECT attempt_id FROM robinhood_order_attempts
		WHERE status IN `+robinhoodUnresolvedStatusSQL+` AND ((account_key = ? AND symbol = ?) OR strategy_id = ?)
		ORDER BY created_at_ms LIMIT 1`, a.AccountKey, a.Symbol, sid).Scan(&held)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read unresolved robinhood order attempts: %w", err)
	}
	if err == nil {
		return fmt.Errorf("robinhood order attempt %s is still unresolved for this coin or strategy", held)
	}
	if err := upsertRobinhoodAttemptRow(tx, sid, a); err != nil {
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
	a.durableStatus = a.Status
	return nil
}

func (sdb *StateDB) transitionRobinhoodAttempt(a *robinhoodOrderAttempt, from []string, to string) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if to == robinhoodAttemptSettled {
		return fmt.Errorf("a robinhood order attempt settles only with its book")
	}
	sid, err := sdb.robinhoodAttemptOwner(a.StrategyID)
	if err != nil {
		return err
	}
	marks := make([]string, len(from))
	args := []interface{}{to, a.EvidenceJSON, a.EvidenceJSON, a.HoldReason, a.VenueOrderID, a.VenueOrderID, a.VenueAccountID, a.VenueAccountID,
		a.ConfirmedQty, a.ConfirmedAvgPx, a.ConfirmedFee, time.Now().UnixMilli(), a.AttemptID, sid, a.RefID}
	for i, f := range from {
		marks[i] = "?"
		args = append(args, f)
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin robinhood order transition: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE robinhood_order_attempts SET status = ?,
		evidence_json = CASE WHEN ? != '' THEN ? ELSE evidence_json END,
		hold_reason = ?,
		venue_order_id = CASE WHEN ? != '' THEN ? ELSE venue_order_id END,
		venue_account_id = CASE WHEN ? != '' THEN ? ELSE venue_account_id END,
		confirmed_qty = ?, confirmed_avg_px = ?, confirmed_fee = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE attempt_id = ? AND strategy_id = ? AND ref_id = ? AND status IN (`+strings.Join(marks, ", ")+`)`, args...)
	if err != nil {
		return fmt.Errorf("transition robinhood order attempt %s to %s: %w", a.AttemptID, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transition robinhood order attempt %s: %w", a.AttemptID, err)
	}
	if n != 1 {
		return fmt.Errorf("robinhood order attempt %s was not in %s; refusing the %s transition", a.AttemptID, strings.Join(from, "/"), to)
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) readRobinhoodAttempt(attemptID string) (*robinhoodOrderAttempt, string, error) {
	exists, err := tableExists(sdb, "robinhood_order_attempts")
	if err != nil {
		return nil, "", err
	}
	if !exists {
		return nil, "", sql.ErrNoRows
	}
	row := sdb.db.QueryRow(`SELECT `+robinhoodAttemptColumns+` FROM robinhood_order_attempts WHERE attempt_id = ?`, attemptID)
	return scanRobinhoodAttempt(row.Scan)
}
