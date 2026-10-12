package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const entryDecisionSchemaDDL = `
CREATE TABLE IF NOT EXISTS entry_decision_state (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    strategy_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    interval_ms INTEGER NOT NULL CHECK (interval_ms > 0),
    target_side TEXT NOT NULL CHECK (target_side IN ('long','short')),
    latest_observed_bar_ms INTEGER NOT NULL DEFAULT 0,
    bootstrap_through_bar_ms INTEGER NOT NULL DEFAULT 0,
    consumed_through_bar_ms INTEGER NOT NULL DEFAULT 0,
    updated_at_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, source, strategy_id, symbol, interval_ms, target_side)
);

CREATE TABLE IF NOT EXISTS entry_order_intents (
    intent_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    strategy_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    interval_ms INTEGER NOT NULL CHECK (interval_ms > 0),
    target_side TEXT NOT NULL CHECK (target_side IN ('long','short')),
    bar_open_ms INTEGER NOT NULL,
    decision_boundary_ms INTEGER NOT NULL,
    cutoff_ms INTEGER NOT NULL,
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL DEFAULT '',
    instrument_type TEXT NOT NULL DEFAULT '',
    client_order_id TEXT NOT NULL,
    initial_stop_client_id TEXT NOT NULL DEFAULT '',
    venue_order_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('reserved','submitting','settling','committed','no_fill')),
    request_json TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '',
    hold_reason TEXT NOT NULL DEFAULT '',
    confirmed_qty REAL NOT NULL DEFAULT 0,
    confirmed_notional REAL NOT NULL DEFAULT 0,
    confirmed_fee REAL NOT NULL DEFAULT 0,
    applied_qty REAL NOT NULL DEFAULT 0,
    applied_notional REAL NOT NULL DEFAULT 0,
    applied_fee REAL NOT NULL DEFAULT 0,
    settlement_seq INTEGER NOT NULL DEFAULT 0,
    replay_payload_json TEXT NOT NULL DEFAULT '',
    replay_delivered_seq INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_entry_intents_one_unresolved
    ON entry_order_intents(scope, source, strategy_id)
    WHERE status IN ('reserved','submitting','settling');
CREATE UNIQUE INDEX IF NOT EXISTS idx_entry_intents_client_order
    ON entry_order_intents(provider, account_key, client_order_id);
CREATE INDEX IF NOT EXISTS idx_entry_intents_decision
    ON entry_order_intents(scope, source, strategy_id, symbol, interval_ms, target_side, bar_open_ms);
`

var errEntryDecisionConsumed = errors.New("the decision candle is already consumed or held for this side")

const entryIntentColumns = `intent_id, scope, source, strategy_id, symbol, interval_ms, target_side,
    bar_open_ms, decision_boundary_ms, cutoff_ms, provider, account_key, instrument_type,
    client_order_id, initial_stop_client_id, venue_order_id, status, request_json, evidence_json,
    hold_reason, confirmed_qty, confirmed_notional, confirmed_fee, applied_qty, applied_notional,
    applied_fee, settlement_seq, replay_payload_json, replay_delivered_seq, created_at_ms, updated_at_ms`

func (sdb *StateDB) migrateEntryDecisionSchema() error {
	if _, err := sdb.db.Exec(entryDecisionSchemaDDL); err != nil {
		return fmt.Errorf("create entry decision tables: %w", err)
	}
	return nil
}

func validEntryPartition(p RiskPartition) error {
	switch p.Scope {
	case ScopeLive:
		if p.Source != "" {
			return fmt.Errorf("live partition carries paper source %q", p.Source)
		}
		return nil
	case ScopePaper:
		return nil
	}
	return fmt.Errorf("entry record has no owning partition")
}

func (sdb *StateDB) entryOwner(procID string, p RiskPartition) (string, error) {
	if err := validEntryPartition(p); err != nil {
		return "", fmt.Errorf("strategy %q: %w", procID, err)
	}
	sid, err := sdb.toStorageID(procID)
	if err != nil {
		return "", err
	}
	if sdb.ident != nil {
		if owned, ok := sdb.ident.procPartition[procID]; ok && owned != p {
			return "", fmt.Errorf("strategy %q is owned by the %s partition in the %s state file, not %s", procID, partitionLabel(owned), sdb.storageRoleOf(), partitionLabel(p))
		}
	}
	return sid, nil
}

func upsertEntryDecisionRow(exec sqlExecer, p RiskPartition, sid string, k entryDecisionKey, rec entryDecisionRecord) error {
	if k.IntervalMs <= 0 || (k.Side != entrySideLong && k.Side != entrySideShort) || strings.TrimSpace(k.Symbol) == "" {
		return fmt.Errorf("malformed entry decision key %+v", k)
	}
	_, err := exec.Exec(`INSERT INTO entry_decision_state
		(scope, source, strategy_id, symbol, interval_ms, target_side, latest_observed_bar_ms, bootstrap_through_bar_ms, consumed_through_bar_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope, source, strategy_id, symbol, interval_ms, target_side) DO UPDATE SET
		    latest_observed_bar_ms = MAX(latest_observed_bar_ms, excluded.latest_observed_bar_ms),
		    bootstrap_through_bar_ms = MAX(bootstrap_through_bar_ms, excluded.bootstrap_through_bar_ms),
		    consumed_through_bar_ms = MAX(consumed_through_bar_ms, excluded.consumed_through_bar_ms),
		    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)`,
		string(p.Scope), p.Source, sid, k.Symbol, k.IntervalMs, k.Side,
		rec.LatestObservedBarMs, rec.BootstrapThroughBarMs, rec.ConsumedThroughBarMs, rec.UpdatedAtMs)
	if err != nil {
		return fmt.Errorf("upsert entry decision %s %s %d: %w", k.Symbol, k.Side, k.IntervalMs, err)
	}
	return nil
}

func upsertEntryIntentRow(exec sqlExecer, sid string, in *entryIntent) error {
	if in == nil {
		return nil
	}
	if entryIntentStatusRank(in.Status) < 0 {
		return fmt.Errorf("entry intent %s has unknown status %q", in.IntentID, in.Status)
	}
	_, err := exec.Exec(`INSERT INTO entry_order_intents (`+entryIntentColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(intent_id) DO UPDATE SET
		    venue_order_id = CASE WHEN excluded.venue_order_id != '' THEN excluded.venue_order_id ELSE venue_order_id END,
		    status = excluded.status,
		    evidence_json = excluded.evidence_json,
		    hold_reason = excluded.hold_reason,
		    confirmed_qty = excluded.confirmed_qty,
		    confirmed_notional = excluded.confirmed_notional,
		    confirmed_fee = excluded.confirmed_fee,
		    applied_qty = excluded.applied_qty,
		    applied_notional = excluded.applied_notional,
		    applied_fee = excluded.applied_fee,
		    settlement_seq = MAX(settlement_seq, excluded.settlement_seq),
		    replay_payload_json = CASE WHEN excluded.replay_payload_json != '' THEN excluded.replay_payload_json ELSE replay_payload_json END,
		    replay_delivered_seq = MAX(replay_delivered_seq, excluded.replay_delivered_seq),
		    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)
		WHERE (CASE excluded.status WHEN 'reserved' THEN 0 WHEN 'submitting' THEN 1 WHEN 'settling' THEN 2 ELSE 3 END)
		    >= (CASE status WHEN 'reserved' THEN 0 WHEN 'submitting' THEN 1 WHEN 'settling' THEN 2 ELSE 3 END)
		  AND (status NOT IN ('committed','no_fill') OR status = excluded.status)`,
		in.IntentID, string(in.Partition.Scope), in.Partition.Source, sid, in.Symbol, in.IntervalMs, in.TargetSide,
		in.BarOpenMs, in.DecisionBoundaryMs, in.CutoffMs, in.Provider, in.AccountKey, in.InstrumentType,
		in.ClientOrderID, in.InitialStopClientID, in.VenueOrderID, in.Status, in.RequestJSON, in.EvidenceJSON,
		in.HoldReason, in.ConfirmedQty, in.ConfirmedNotional, in.ConfirmedFee, in.AppliedQty, in.AppliedNotional,
		in.AppliedFee, in.SettlementSeq, in.ReplayPayloadJSON, in.ReplayDeliveredSeq, in.CreatedAtMs, in.UpdatedAtMs)
	if err != nil {
		return fmt.Errorf("upsert entry intent %s: %w", in.IntentID, err)
	}
	return nil
}

func (sdb *StateDB) writeEntryGuardTx(tx *sql.Tx, s *StrategyState, withBook bool) error {
	if s == nil || s.EntryGuard == nil {
		return nil
	}
	g := s.EntryGuard
	if len(g.Decisions) == 0 && len(g.Intents) == 0 {
		return nil
	}
	sid, err := sdb.entryOwner(s.ID, g.Partition)
	if err != nil {
		return err
	}
	for _, k := range g.sortedDecisionKeys() {
		if err := upsertEntryDecisionRow(tx, g.Partition, sid, k, *g.Decisions[k]); err != nil {
			return err
		}
	}
	for _, id := range g.sortedIntentIDs() {
		in := g.Intents[id]
		if !withBook && in.Status == entryIntentCommitted && !in.commitDurable() {
			continue
		}
		if in.Partition != g.Partition {
			return fmt.Errorf("entry intent %s belongs to the %s partition, the strategy to %s", id, partitionLabel(in.Partition), partitionLabel(g.Partition))
		}
		if err := upsertEntryIntentRow(tx, sid, in); err != nil {
			return err
		}
	}
	return nil
}

func pruneEntryGuardAfterCommit(s *StrategyState) {
	if s == nil || s.EntryGuard == nil {
		return
	}
	for id, in := range s.EntryGuard.Intents {
		in.durableStatus = in.Status
		if in.unresolved() || in.replayPending() {
			continue
		}
		delete(s.EntryGuard.Intents, id)
	}
}

type entryGuardLoadOwner struct {
	Partition   RiskPartition
	PreExisting bool
}

func scanEntryIntent(scan func(dest ...interface{}) error) (*entryIntent, string, error) {
	var in entryIntent
	var scope, source, sid string
	if err := scan(&in.IntentID, &scope, &source, &sid, &in.Symbol, &in.IntervalMs, &in.TargetSide,
		&in.BarOpenMs, &in.DecisionBoundaryMs, &in.CutoffMs, &in.Provider, &in.AccountKey, &in.InstrumentType,
		&in.ClientOrderID, &in.InitialStopClientID, &in.VenueOrderID, &in.Status, &in.RequestJSON, &in.EvidenceJSON,
		&in.HoldReason, &in.ConfirmedQty, &in.ConfirmedNotional, &in.ConfirmedFee, &in.AppliedQty, &in.AppliedNotional,
		&in.AppliedFee, &in.SettlementSeq, &in.ReplayPayloadJSON, &in.ReplayDeliveredSeq, &in.CreatedAtMs, &in.UpdatedAtMs); err != nil {
		return nil, "", err
	}
	in.Partition = RiskPartition{Scope: PortfolioScope(scope), Source: source}
	return &in, sid, nil
}

func (sdb *StateDB) loadEntryGuards(owners map[string]entryGuardLoadOwner) (map[string]*entryGuardState, error) {
	out := make(map[string]*entryGuardState, len(owners))
	for procID, o := range owners {
		if err := validEntryPartition(o.Partition); err != nil {
			return nil, fmt.Errorf("strategy %q: %w", procID, err)
		}
		out[procID] = newEntryGuardState(o.Partition, o.PreExisting)
	}
	exists, err := tableExists(sdb, "entry_decision_state")
	if err != nil {
		return nil, err
	}
	if exists {
		rows, err := sdb.db.Query(`SELECT scope, source, strategy_id, symbol, interval_ms, target_side,
			latest_observed_bar_ms, bootstrap_through_bar_ms, consumed_through_bar_ms, updated_at_ms
			FROM entry_decision_state ORDER BY scope, source, strategy_id, symbol, interval_ms, target_side`)
		if err != nil {
			return nil, fmt.Errorf("read entry decisions in the %s state file: %w", sdb.storageRoleOf(), err)
		}
		for rows.Next() {
			var scope, source, sid string
			var k entryDecisionKey
			var rec entryDecisionRecord
			if err := rows.Scan(&scope, &source, &sid, &k.Symbol, &k.IntervalMs, &k.Side,
				&rec.LatestObservedBarMs, &rec.BootstrapThroughBarMs, &rec.ConsumedThroughBarMs, &rec.UpdatedAtMs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan entry decision in the %s state file: %w", sdb.storageRoleOf(), err)
			}
			procID, mapped := sdb.mapStoredStrategyID(sid)
			if !mapped {
				continue
			}
			g := out[procID]
			if g == nil || g.Partition != (RiskPartition{Scope: PortfolioScope(scope), Source: source}) {
				continue
			}
			r := rec
			g.Decisions[k] = &r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate entry decisions in the %s state file: %w", sdb.storageRoleOf(), err)
		}
	}
	exists, err = tableExists(sdb, "entry_order_intents")
	if err != nil {
		return nil, err
	}
	if exists {
		rows, err := sdb.db.Query(`SELECT ` + entryIntentColumns + ` FROM entry_order_intents
			WHERE status IN ('reserved','submitting','settling')
			   OR (status = 'committed' AND replay_payload_json != '' AND replay_delivered_seq < settlement_seq)
			ORDER BY created_at_ms, intent_id`)
		if err != nil {
			return nil, fmt.Errorf("read entry intents in the %s state file: %w", sdb.storageRoleOf(), err)
		}
		for rows.Next() {
			in, sid, err := scanEntryIntent(rows.Scan)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan entry intent in the %s state file: %w", sdb.storageRoleOf(), err)
			}
			procID, mapped := sdb.mapStoredStrategyID(sid)
			if !mapped {
				continue
			}
			g := out[procID]
			if g == nil || g.Partition != in.Partition {
				continue
			}
			in.StrategyID = procID
			in.durableStatus = in.Status
			g.Intents[in.IntentID] = in
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate entry intents in the %s state file: %w", sdb.storageRoleOf(), err)
		}
	}
	return out, nil
}

func (sdb *StateDB) reserveEntryIntent(s *StrategyState, in *entryIntent) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if s == nil || s.EntryGuard == nil || in == nil {
		return fmt.Errorf("entry guard unavailable")
	}
	if in.Status != entryIntentReserved {
		return fmt.Errorf("entry intent %s must be reserved before submission, got %q", in.IntentID, in.Status)
	}
	g := s.EntryGuard
	sid, err := sdb.entryOwner(s.ID, in.Partition)
	if err != nil {
		return err
	}
	if in.Partition != g.Partition {
		return fmt.Errorf("entry intent partition %s differs from the strategy partition %s", partitionLabel(in.Partition), partitionLabel(g.Partition))
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin entry reservation: %w", err)
	}
	defer tx.Rollback()
	var consumed, bootstrap, observed int64
	err = tx.QueryRow(`SELECT consumed_through_bar_ms, bootstrap_through_bar_ms, latest_observed_bar_ms FROM entry_decision_state
		WHERE scope = ? AND source = ? AND strategy_id = ? AND symbol = ? AND interval_ms = ? AND target_side = ?`,
		string(in.Partition.Scope), in.Partition.Source, sid, in.Symbol, in.IntervalMs, in.TargetSide).Scan(&consumed, &bootstrap, &observed)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read entry decision for %s: %w", in.IntentID, err)
	}
	if err == nil && (in.BarOpenMs <= consumed || in.BarOpenMs <= bootstrap || in.BarOpenMs < observed) {
		return errEntryDecisionConsumed
	}
	var unresolved int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM entry_order_intents WHERE scope = ? AND source = ? AND strategy_id = ?
		AND status IN ('reserved','submitting','settling')`, string(in.Partition.Scope), in.Partition.Source, sid).Scan(&unresolved); err != nil {
		return fmt.Errorf("read unresolved entry intents for %s: %w", s.ID, err)
	}
	if unresolved > 0 {
		return fmt.Errorf("strategy %s already has an unresolved live entry", s.ID)
	}
	for _, k := range g.sortedDecisionKeys() {
		if k.Symbol != in.Symbol || k.IntervalMs != in.IntervalMs {
			continue
		}
		if err := upsertEntryDecisionRow(tx, g.Partition, sid, k, *g.Decisions[k]); err != nil {
			return err
		}
	}
	if err := upsertEntryIntentRow(tx, sid, in); err != nil {
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
	in.durableStatus = in.Status
	return nil
}

func (sdb *StateDB) transitionEntryIntent(procID string, in *entryIntent, from []string, to string) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	sid, err := sdb.entryOwner(procID, in.Partition)
	if err != nil {
		return err
	}
	marks := make([]string, len(from))
	args := []interface{}{to, in.EvidenceJSON, in.HoldReason, in.VenueOrderID, in.VenueOrderID, in.RequestJSON, in.RequestJSON, in.ConfirmedQty, in.ConfirmedNotional, in.ConfirmedFee, time.Now().UnixMilli(), in.IntentID, sid}
	for i, f := range from {
		marks[i] = "?"
		args = append(args, f)
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin entry intent transition: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE entry_order_intents SET status = ?, evidence_json = ?, hold_reason = ?,
		venue_order_id = CASE WHEN ? != '' THEN ? ELSE venue_order_id END,
		request_json = CASE WHEN ? != '' THEN ? ELSE request_json END,
		confirmed_qty = ?, confirmed_notional = ?, confirmed_fee = ?, updated_at_ms = ?
		WHERE intent_id = ? AND strategy_id = ? AND status IN (`+strings.Join(marks, ", ")+`)`, args...)
	if err != nil {
		return fmt.Errorf("transition entry intent %s to %s: %w", in.IntentID, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transition entry intent %s: %w", in.IntentID, err)
	}
	if n != 1 {
		return fmt.Errorf("entry intent %s was not in %s; refusing the %s transition", in.IntentID, strings.Join(from, "/"), to)
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) persistEntryGuardProgress(s *StrategyState) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin entry progress: %w", err)
	}
	defer tx.Rollback()
	if err := sdb.writeEntryGuardTx(tx, s, false); err != nil {
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
	if s.EntryGuard != nil {
		for _, in := range s.EntryGuard.Intents {
			if in.Status != entryIntentCommitted {
				in.durableStatus = in.Status
			}
		}
	}
	return nil
}

func (sdb *StateDB) markEntryIntentReplayDelivered(procID string, in *entryIntent, seq int64) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.entryOwner(procID, in.Partition)
	if err != nil {
		return err
	}
	_, err = sdb.db.Exec(`UPDATE entry_order_intents SET replay_delivered_seq = MAX(replay_delivered_seq, ?), updated_at_ms = ?
		WHERE intent_id = ? AND strategy_id = ? AND status = 'committed'`, seq, time.Now().UnixMilli(), in.IntentID, sid)
	if err != nil {
		return fmt.Errorf("mark entry intent %s replay delivered: %w", in.IntentID, err)
	}
	return nil
}

func (sdb *StateDB) releaseEntryIntentByOperator(procID string, prior *entryIntent, evidenceJSON string, nowMs int64) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if prior == nil || prior.Status != entryIntentSubmitting {
		return fmt.Errorf("only a submitting attempt can be released by the operator")
	}
	sid, err := sdb.entryOwner(procID, prior.Partition)
	if err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin operator release: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE entry_order_intents SET status = 'no_fill', evidence_json = ?, hold_reason = '', updated_at_ms = MAX(updated_at_ms, ?)
		WHERE intent_id = ? AND scope = ? AND source = ? AND strategy_id = ? AND status = 'submitting'
		  AND provider = ? AND account_key = ? AND instrument_type = ? AND symbol = ?
		  AND client_order_id = ? AND initial_stop_client_id = ? AND request_json = ? AND evidence_json = ?
		  AND venue_order_id = '' AND confirmed_qty = 0 AND confirmed_notional = 0 AND confirmed_fee = 0
		  AND applied_qty = 0 AND applied_notional = 0 AND applied_fee = 0 AND settlement_seq = 0`,
		evidenceJSON, nowMs,
		prior.IntentID, string(prior.Partition.Scope), prior.Partition.Source, sid,
		prior.Provider, prior.AccountKey, prior.InstrumentType, prior.Symbol,
		prior.ClientOrderID, prior.InitialStopClientID, prior.RequestJSON, prior.EvidenceJSON)
	if err != nil {
		return fmt.Errorf("operator release of entry intent %s: %w", prior.IntentID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("operator release of entry intent %s: %w", prior.IntentID, err)
	}
	if n != 1 {
		return errEntryOperatorReleaseStale
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) readEntryIntent(intentID string) (*entryIntent, string, error) {
	row := sdb.db.QueryRow(`SELECT `+entryIntentColumns+` FROM entry_order_intents WHERE intent_id = ?`, intentID)
	in, sid, err := scanEntryIntent(row.Scan)
	if err != nil {
		return nil, "", err
	}
	in.StrategyID = sdb.fromStorageID(sid)
	return in, sid, nil
}
