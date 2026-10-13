package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	basketPurposeRebalance = "rebalance"
	basketPurposeExpiry    = "expiry"

	basketExpiryDue = "due"
)

var basketLifecycleMigrations = []string{
	"ALTER TABLE basket_member_state ADD COLUMN position_id TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE basket_member_state ADD COLUMN entry_ms INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE basket_member_state ADD COLUMN expiry_ms INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE basket_member_state ADD COLUMN expiry_state TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE basket_member_state ADD COLUMN expiry_attempt INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE basket_member_state ADD COLUMN expiry_failures INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE basket_member_state ADD COLUMN expiry_retry_at_ms INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE basket_member_state ADD COLUMN expiry_alerts_json TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE basket_member_state ADD COLUMN suppress_through_ms INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE basket_member_actions ADD COLUMN purpose TEXT NOT NULL DEFAULT 'rebalance' CHECK (purpose IN ('rebalance','expiry'))",
	"ALTER TABLE basket_member_actions ADD COLUMN lifecycle_position_id TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE basket_member_actions ADD COLUMN expiry_ms INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE basket_member_actions ADD COLUMN lifecycle_attempt INTEGER NOT NULL DEFAULT 0",
}

type basketColumnDefault struct {
	Name    string
	Default string
}

var basketMemberStateLifecycleColumns = []basketColumnDefault{
	{"position_id", "''"},
	{"entry_ms", "0"},
	{"expiry_ms", "0"},
	{"expiry_state", "''"},
	{"expiry_attempt", "0"},
	{"expiry_failures", "0"},
	{"expiry_retry_at_ms", "0"},
	{"expiry_alerts_json", "''"},
	{"suppress_through_ms", "0"},
}

var basketActionLifecycleColumns = []basketColumnDefault{
	{"purpose", "'rebalance'"},
	{"lifecycle_position_id", "''"},
	{"expiry_ms", "0"},
	{"lifecycle_attempt", "0"},
}

type basketLifecycle struct {
	PositionID        string
	EntryMs           int64
	ExpiryMs          int64
	State             string
	Attempt           int
	Failures          int
	RetryAtMs         int64
	Alerts            []string
	SuppressThroughMs int64
}

func (l basketLifecycle) active() bool {
	return l.PositionID != ""
}

func (l basketLifecycle) cleared() basketLifecycle {
	return basketLifecycle{SuppressThroughMs: l.SuppressThroughMs}
}

func (l basketLifecycle) alerted(key string) bool {
	for _, k := range l.Alerts {
		if k == key {
			return true
		}
	}
	return false
}

func (l *basketLifecycle) markAlerted(key string) {
	if l.alerted(key) {
		return
	}
	l.Alerts = append(append([]string(nil), l.Alerts...), key)
	sort.Strings(l.Alerts)
}

func (l basketLifecycle) alertsJSON() string {
	if len(l.Alerts) == 0 {
		return ""
	}
	b, err := json.Marshal(l.Alerts)
	if err != nil {
		return ""
	}
	return string(b)
}

func (l basketLifecycle) sameAs(o basketLifecycle) bool {
	if l.PositionID != o.PositionID || l.EntryMs != o.EntryMs || l.ExpiryMs != o.ExpiryMs || l.State != o.State ||
		l.Attempt != o.Attempt || l.Failures != o.Failures || l.RetryAtMs != o.RetryAtMs || l.SuppressThroughMs != o.SuppressThroughMs {
		return false
	}
	return l.alertsJSON() == o.alertsJSON()
}

func parseBasketLifecycleAlerts(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func mergeBasketLifecycle(stored, mem basketLifecycle) basketLifecycle {
	out := stored
	if mem.active() && mem.PositionID == stored.PositionID {
		out.State, out.Failures, out.RetryAtMs = mem.State, mem.Failures, mem.RetryAtMs
		out.Alerts = append([]string(nil), mem.Alerts...)
		if mem.Attempt > out.Attempt {
			out.Attempt = mem.Attempt
		}
	}
	if mem.SuppressThroughMs > out.SuppressThroughMs {
		out.SuppressThroughMs = mem.SuppressThroughMs
	}
	return out
}

func basketLifecycleFromBook(prev basketLifecycle, s *StrategyState, coin string, horizonMs, fallbackMs int64) basketLifecycle {
	var pos *Position
	if s != nil {
		pos = s.Positions[coin]
	}
	if pos == nil || pos.Quantity <= 0 {
		return prev.cleared()
	}
	pid := ensurePositionTradeID(s.ID, coin, pos)
	if prev.active() && prev.PositionID == pid {
		return prev
	}
	if horizonMs <= 0 {
		return prev.cleared()
	}
	entry := fallbackMs
	if !pos.OpenedAt.IsZero() {
		entry = pos.OpenedAt.UTC().UnixMilli()
	}
	if entry <= 0 {
		return prev.cleared()
	}
	return basketLifecycle{PositionID: pid, EntryMs: entry, ExpiryMs: entry + horizonMs, SuppressThroughMs: prev.SuppressThroughMs}
}

func basketExpiryActionID(p RiskPartition, basketID, strategyID, positionID string, expiryMs int64, attempt int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|expiry|%s|%s|%d|%d", p.String(), basketID, strategyID, positionID, expiryMs, attempt)))
	return "be-" + hex.EncodeToString(sum[:12])
}

func (sdb *StateDB) migrateBasketLifecycleColumns() error {
	for _, ddl := range basketLifecycleMigrations {
		if _, err := sdb.db.Exec(ddl); err != nil {
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("migrate basket lifecycle columns: %w", err)
		}
	}
	return nil
}

func (sdb *StateDB) basketProjection(table string, cols []basketColumnDefault) (string, error) {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		has, err := sdb.tableHasColumn(table, c.Name)
		if err != nil {
			return "", fmt.Errorf("introspect %s columns in the %s state file: %w", table, sdb.storageRoleOf(), err)
		}
		if has {
			parts = append(parts, c.Name)
		} else {
			parts = append(parts, c.Default+" AS "+c.Name)
		}
	}
	return strings.Join(parts, ", "), nil
}

const basketMemberStateBaseColumns = `scope, source, basket_id, strategy_id, expected_qty`

func scanBasketMemberState(scan func(dest ...interface{}) error) (basketKey, string, float64, basketLifecycle, error) {
	var scope, source, id, sid, alerts string
	var q float64
	var l basketLifecycle
	if err := scan(&scope, &source, &id, &sid, &q, &l.PositionID, &l.EntryMs, &l.ExpiryMs, &l.State, &l.Attempt,
		&l.Failures, &l.RetryAtMs, &alerts, &l.SuppressThroughMs); err != nil {
		return basketKey{}, "", 0, l, err
	}
	k := basketKey{Partition: RiskPartition{Scope: PortfolioScope(scope), Source: source}, BasketID: id}
	parsed, err := parseBasketLifecycleAlerts(alerts)
	if err != nil {
		return k, sid, q, l, fmt.Errorf("basket %s member %s expiry alerts are unreadable: %w", id, sid, err)
	}
	l.Alerts = parsed
	return k, sid, q, l, nil
}

const basketMemberStateLifecycleSelect = `position_id, entry_ms, expiry_ms, expiry_state, expiry_attempt, expiry_failures, expiry_retry_at_ms, expiry_alerts_json, suppress_through_ms`

func readBasketMemberStateTx(tx *sql.Tx, k basketKey, sid string) (basketLifecycle, bool, error) {
	row := tx.QueryRow(`SELECT `+basketMemberStateBaseColumns+`, `+basketMemberStateLifecycleSelect+` FROM basket_member_state
		WHERE scope = ? AND source = ? AND basket_id = ? AND strategy_id = ?`,
		string(k.Partition.Scope), k.Partition.Source, k.BasketID, sid)
	_, _, _, l, err := scanBasketMemberState(row.Scan)
	if err == sql.ErrNoRows {
		return basketLifecycle{}, false, nil
	}
	if err != nil {
		return basketLifecycle{}, false, fmt.Errorf("read basket %s member state %s: %w", k.BasketID, sid, err)
	}
	return l, true, nil
}

func writeBasketMemberBookTx(tx *sql.Tx, k basketKey, sid string, expected float64, l basketLifecycle, nowMs int64) error {
	_, err := tx.Exec(`INSERT INTO basket_member_state (scope, source, basket_id, strategy_id, expected_qty, updated_at_ms,
		    position_id, entry_ms, expiry_ms, expiry_state, expiry_attempt, expiry_failures, expiry_retry_at_ms, expiry_alerts_json, suppress_through_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope, source, basket_id, strategy_id) DO UPDATE SET
		    expected_qty = excluded.expected_qty,
		    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms),
		    expiry_attempt = CASE WHEN position_id = excluded.position_id THEN MAX(expiry_attempt, excluded.expiry_attempt) ELSE excluded.expiry_attempt END,
		    position_id = excluded.position_id,
		    entry_ms = excluded.entry_ms,
		    expiry_ms = excluded.expiry_ms,
		    expiry_state = excluded.expiry_state,
		    expiry_failures = excluded.expiry_failures,
		    expiry_retry_at_ms = excluded.expiry_retry_at_ms,
		    expiry_alerts_json = excluded.expiry_alerts_json,
		    suppress_through_ms = MAX(suppress_through_ms, excluded.suppress_through_ms)`,
		string(k.Partition.Scope), k.Partition.Source, k.BasketID, sid, expected, nowMs,
		l.PositionID, l.EntryMs, l.ExpiryMs, l.State, l.Attempt, l.Failures, l.RetryAtMs, l.alertsJSON(), l.SuppressThroughMs)
	if err != nil {
		return fmt.Errorf("upsert basket %s member state %s: %w", k.BasketID, sid, err)
	}
	return nil
}

func writeBasketLifecycleOnlyTx(tx *sql.Tx, k basketKey, sid string, l basketLifecycle, nowMs int64) error {
	_, err := tx.Exec(`UPDATE basket_member_state SET
		    expiry_attempt = CASE WHEN position_id = ? THEN MAX(expiry_attempt, ?) ELSE ? END,
		    position_id = ?, entry_ms = ?, expiry_ms = ?, expiry_state = ?, expiry_failures = ?, expiry_retry_at_ms = ?,
		    expiry_alerts_json = ?, suppress_through_ms = MAX(suppress_through_ms, ?), updated_at_ms = MAX(updated_at_ms, ?)
		WHERE scope = ? AND source = ? AND basket_id = ? AND strategy_id = ?`,
		l.PositionID, l.Attempt, l.Attempt,
		l.PositionID, l.EntryMs, l.ExpiryMs, l.State, l.Failures, l.RetryAtMs, l.alertsJSON(), l.SuppressThroughMs, nowMs,
		string(k.Partition.Scope), k.Partition.Source, k.BasketID, sid)
	if err != nil {
		return fmt.Errorf("update basket %s member lifecycle %s: %w", k.BasketID, sid, err)
	}
	return nil
}

func writeBasketMemberProgressTx(tx *sql.Tx, k basketKey, sid string, l basketLifecycle, nowMs int64) error {
	if l.SuppressThroughMs > 0 {
		if _, err := tx.Exec(`INSERT INTO basket_member_state (scope, source, basket_id, strategy_id, updated_at_ms, suppress_through_ms)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(scope, source, basket_id, strategy_id) DO UPDATE SET
			    suppress_through_ms = MAX(suppress_through_ms, excluded.suppress_through_ms),
			    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)`,
			string(k.Partition.Scope), k.Partition.Source, k.BasketID, sid, nowMs, l.SuppressThroughMs); err != nil {
			return fmt.Errorf("upsert basket %s member suppression %s: %w", k.BasketID, sid, err)
		}
	}
	if !l.active() {
		return nil
	}
	if _, err := tx.Exec(`UPDATE basket_member_state SET
		    expiry_state = ?, expiry_attempt = MAX(expiry_attempt, ?), expiry_failures = ?, expiry_retry_at_ms = ?,
		    expiry_alerts_json = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE scope = ? AND source = ? AND basket_id = ? AND strategy_id = ? AND position_id = ? AND position_id != ''`,
		l.State, l.Attempt, l.Failures, l.RetryAtMs, l.alertsJSON(), nowMs,
		string(k.Partition.Scope), k.Partition.Source, k.BasketID, sid, l.PositionID); err != nil {
		return fmt.Errorf("update basket %s member expiry progress %s: %w", k.BasketID, sid, err)
	}
	return nil
}

func (sdb *StateDB) reconcileStoredBasketLifecycleTx(tx *sql.Tx, sid string, s *StrategyState) error {
	if s == nil {
		return nil
	}
	rows, err := tx.Query(`SELECT `+basketMemberStateBaseColumns+`, `+basketMemberStateLifecycleSelect+` FROM basket_member_state
		WHERE strategy_id = ? ORDER BY scope, source, basket_id`, sid)
	if err != nil {
		return fmt.Errorf("read stored basket lifecycle for %s: %w", s.ID, err)
	}
	type stored struct {
		k basketKey
		l basketLifecycle
	}
	var found []stored
	for rows.Next() {
		k, _, _, l, err := scanBasketMemberState(rows.Scan)
		if err != nil {
			rows.Close()
			return err
		}
		found = append(found, stored{k: k, l: l})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, st := range found {
		owner, err := sdb.basketMemberOwner(s.ID, st.k.Partition)
		if err != nil {
			if st.l.active() {
				return fmt.Errorf("basket %s lifecycle for %s: %w", st.k.BasketID, s.ID, err)
			}
			continue
		}
		if owner != sid {
			return fmt.Errorf("basket %s lifecycle names stored strategy %q, but %s maps to %q", st.k.BasketID, sid, s.ID, owner)
		}
		var raw string
		err = tx.QueryRow(`SELECT contract_json FROM basket_contracts WHERE scope = ? AND source = ? AND basket_id = ?`,
			string(st.k.Partition.Scope), st.k.Partition.Source, st.k.BasketID).Scan(&raw)
		if err == sql.ErrNoRows {
			if st.l.active() {
				return fmt.Errorf("basket %s holds a lifecycle for %s but no saved contract; the book cannot reconcile it", st.k.BasketID, s.ID)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("read basket %s contract: %w", st.k.BasketID, err)
		}
		contract, err := parseBasketContract(raw)
		if err != nil {
			return fmt.Errorf("basket %s: %w", st.k.BasketID, err)
		}
		member, ok := contract.member(s.ID)
		if !ok {
			if st.l.active() {
				return fmt.Errorf("basket %s lifecycle names %s, which its saved contract does not list", st.k.BasketID, s.ID)
			}
			continue
		}
		next := basketLifecycleFromBook(st.l, s, member.Coin, 0, 0)
		if next.sameAs(st.l) {
			continue
		}
		if err := writeBasketLifecycleOnlyTx(tx, st.k, sid, next, next.EntryMs); err != nil {
			return err
		}
	}
	return nil
}

func (sdb *StateDB) reserveBasketExpiryAttemptTx(tx *sql.Tx, a *basketAction, sid string, nowMs int64) error {
	var rowAttempt int
	err := tx.QueryRow(`SELECT expiry_attempt FROM basket_member_state
		WHERE scope = ? AND source = ? AND basket_id = ? AND strategy_id = ? AND position_id = ? AND position_id != '' AND expiry_ms = ?`,
		string(a.Partition.Scope), a.Partition.Source, a.BasketID, sid, a.LifecyclePositionID, a.ExpiryMs).Scan(&rowAttempt)
	if err == sql.ErrNoRows {
		return fmt.Errorf("basket %s has no saved holding-horizon lifecycle for %s position %s; the close waits for it to be durable", a.BasketID, a.StrategyID, a.LifecyclePositionID)
	}
	if err != nil {
		return fmt.Errorf("read basket %s expiry lifecycle for %s: %w", a.BasketID, a.StrategyID, err)
	}
	var maxSaved sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(lifecycle_attempt) FROM basket_member_actions
		WHERE scope = ? AND source = ? AND basket_id = ? AND strategy_id = ? AND purpose = 'expiry' AND lifecycle_position_id = ? AND expiry_ms = ?`,
		string(a.Partition.Scope), a.Partition.Source, a.BasketID, sid, a.LifecyclePositionID, a.ExpiryMs).Scan(&maxSaved); err != nil {
		return fmt.Errorf("read basket %s expiry attempts for %s: %w", a.BasketID, a.StrategyID, err)
	}
	attempt := a.LifecycleAttempt
	if rowAttempt+1 > attempt {
		attempt = rowAttempt + 1
	}
	if maxSaved.Valid && int(maxSaved.Int64)+1 > attempt {
		attempt = int(maxSaved.Int64) + 1
	}
	a.LifecycleAttempt = attempt
	a.ActionID = basketExpiryActionID(a.Partition, a.BasketID, a.StrategyID, a.LifecyclePositionID, a.ExpiryMs, a.LifecycleAttempt)
	if _, err := tx.Exec(`UPDATE basket_member_state SET expiry_attempt = ?, expiry_state = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE scope = ? AND source = ? AND basket_id = ? AND strategy_id = ? AND position_id = ? AND expiry_ms = ?`,
		a.LifecycleAttempt, basketExpiryDue, nowMs,
		string(a.Partition.Scope), a.Partition.Source, a.BasketID, sid, a.LifecyclePositionID, a.ExpiryMs); err != nil {
		return fmt.Errorf("reserve basket %s expiry attempt %d for %s: %w", a.BasketID, a.LifecycleAttempt, a.StrategyID, err)
	}
	return nil
}

func nextBasketActionSeqTx(tx *sql.Tx, a *basketAction, sid string) (int, error) {
	var maxSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM basket_member_actions
		WHERE scope = ? AND source = ? AND basket_id = ? AND deadline_ms = ? AND strategy_id = ? AND direction = ?`,
		string(a.Partition.Scope), a.Partition.Source, a.BasketID, a.DeadlineMs, sid, a.Direction).Scan(&maxSeq); err != nil {
		return 0, fmt.Errorf("read basket action sequence: %w", err)
	}
	next := 1
	if maxSeq.Valid && int(maxSeq.Int64) >= next {
		next = int(maxSeq.Int64) + 1
	}
	if a.Seq > next {
		next = a.Seq
	}
	return next, nil
}

func inspectBasketLifecycleRows(db *StateDB, fi *storageFileInspection, ident storageIdentityMap, ownedScopes map[PortfolioScope]bool, ownedParts map[string]bool) ([]string, error) {
	exists, err := tableExists(db, "basket_member_state")
	if err != nil || !exists {
		return nil, err
	}
	proj, err := db.basketProjection("basket_member_state", basketMemberStateLifecycleColumns)
	if err != nil {
		return nil, err
	}
	rows, err := db.db.Query(`SELECT ` + basketMemberStateBaseColumns + `, ` + proj + ` FROM basket_member_state ORDER BY scope, source, basket_id, strategy_id`)
	if err != nil {
		return nil, fmt.Errorf("read basket member state in the %s state file: %w", fi.Role, err)
	}
	defer rows.Close()
	var rejections []string
	for rows.Next() {
		k, sid, q, l, err := scanBasketMemberState(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan basket member state in the %s state file: %w", fi.Role, err)
		}
		if !l.active() && q == 0 {
			continue
		}
		owner := sid
		if procID, ok := ident.processFor(fi.Role, sid); ok {
			owner = procID
		} else {
			rejections = append(rejections, fmt.Sprintf("%s state file: basket %s member state names stored strategy %q, which this file does not own", fi.Role, k.BasketID, sid))
		}
		if !ownedScopes[k.Partition.Scope] || (len(ownedParts) > 0 && !ownedParts[k.Partition.String()]) {
			rejections = append(rejections, fmt.Sprintf("%s state file: basket %s member state for %s belongs to the %s partition but the file owns %s", fi.Role, k.BasketID, owner, partitionLabel(k.Partition), strings.Join(fi.PartitionsOwned, ", ")))
		}
		if !l.active() {
			continue
		}
		state := "scheduled"
		if l.State == basketExpiryDue {
			state = "due, closing"
		}
		fi.BasketPending = append(fi.BasketPending, fmt.Sprintf("%s %s member %s: unresolved holding-horizon expiry (%s) for position %s, entered %s, expires %s, attempt %d",
			partitionLabel(k.Partition), k.BasketID, owner, state, l.PositionID, formatClosedBarMs(l.EntryMs), formatClosedBarMs(l.ExpiryMs), l.Attempt))
	}
	return rejections, rows.Err()
}
