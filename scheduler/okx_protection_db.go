package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const okxProtectionSchemaDDL = `
CREATE TABLE IF NOT EXISTS okx_protection (
    record_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope = 'live'),
    source TEXT NOT NULL DEFAULT '' CHECK (source = ''),
    strategy_id TEXT NOT NULL,
    account_key TEXT NOT NULL CHECK (account_key != ''),
    inst_id TEXT NOT NULL CHECK (inst_id != ''),
    symbol TEXT NOT NULL CHECK (symbol != ''),
    side TEXT NOT NULL DEFAULT '' CHECK (side IN ('','long','short')),
    algo_client_id TEXT NOT NULL CHECK (algo_client_id != ''),
    entry_client_id TEXT NOT NULL DEFAULT '',
    algo_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('reserved','arming','armed','amending','canceling','triggered','reducing','retired')),
    owner TEXT NOT NULL DEFAULT '',
    policy_json TEXT NOT NULL DEFAULT '',
    trigger_px TEXT NOT NULL DEFAULT '',
    size_contracts TEXT NOT NULL DEFAULT '',
    request_json TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '',
    incident_id TEXT NOT NULL DEFAULT '',
    alerted INTEGER NOT NULL DEFAULT 0 CHECK (alerted IN (0, 1)),
    version INTEGER NOT NULL CHECK (version > 0),
    position_id TEXT NOT NULL DEFAULT '',
    place_attempts INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_okx_protection_algo_client
    ON okx_protection(account_key, algo_client_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_okx_protection_one_unresolved
    ON okx_protection(account_key, inst_id)
    WHERE status IN ('reserved','arming','armed','amending','canceling','triggered','reducing');
CREATE UNIQUE INDEX IF NOT EXISTS idx_okx_protection_one_strategy
    ON okx_protection(scope, source, strategy_id)
    WHERE status IN ('reserved','arming','armed','amending','canceling','triggered','reducing');

CREATE TABLE IF NOT EXISTS okx_protection_fills (
    account_key TEXT NOT NULL,
    fill_id TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope = 'live'),
    source TEXT NOT NULL DEFAULT '' CHECK (source = ''),
    strategy_id TEXT NOT NULL,
    record_id TEXT NOT NULL,
    side TEXT NOT NULL,
    qty REAL NOT NULL,
    px REAL NOT NULL,
    fee REAL NOT NULL,
    applied INTEGER NOT NULL DEFAULT 1 CHECK (applied = 1),
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY (account_key, fill_id)
);
CREATE TRIGGER IF NOT EXISTS okx_protection_fills_no_delete
BEFORE DELETE ON okx_protection_fills
BEGIN
    SELECT RAISE(ABORT, 'okx protection fills are append-only');
END;
CREATE TRIGGER IF NOT EXISTS okx_protection_fills_no_update
BEFORE UPDATE ON okx_protection_fills
BEGIN
    SELECT RAISE(ABORT, 'okx protection fills are immutable');
END;
`

const okxProtectionUnresolvedSQL = `('reserved','arming','armed','amending','canceling','triggered','reducing')`

const okxProtectionColumns = `record_id, scope, source, strategy_id, account_key, inst_id, symbol, side,
    algo_client_id, entry_client_id, algo_id, status, owner, policy_json, trigger_px, size_contracts,
    request_json, evidence_json, incident_id, alerted, version, position_id, place_attempts,
    created_at_ms, updated_at_ms`

func (sdb *StateDB) migrateOKXProtectionSchema() error {
	if _, err := sdb.db.Exec(okxProtectionSchemaDDL); err != nil {
		return fmt.Errorf("create okx protection tables: %w", err)
	}
	return nil
}

func (sdb *StateDB) okxProtectionOwner(procID string) (string, error) {
	if sdb.ident != nil {
		if owned, ok := sdb.ident.procPartition[procID]; ok && owned != livePartition {
			return "", fmt.Errorf("strategy %q is owned by the %s partition, not live; OKX protection is live-only", procID, partitionLabel(owned))
		}
	}
	return sdb.toStorageID(procID)
}

func (sdb *StateDB) reserveOKXProtection(s *StrategyState, p *okxProtection) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if p == nil || p.Status != okxProtectReserved || p.Version != 1 {
		return fmt.Errorf("an OKX stop must be reserved at version 1")
	}
	sid, err := sdb.okxProtectionOwner(s.ID)
	if err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin OKX stop reservation: %w", err)
	}
	defer tx.Rollback()
	if err := upsertOKXProtectionRow(tx, sid, p); err != nil {
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
	p.durableVersion = p.Version
	return nil
}

func upsertOKXProtectionRow(exec sqlExecer, sid string, p *okxProtection) error {
	alerted := 0
	if p.Alerted {
		alerted = 1
	}
	res, err := exec.Exec(`INSERT INTO okx_protection (`+okxProtectionColumns+`)
		VALUES (?, 'live', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(record_id) DO UPDATE SET
		    algo_id = CASE WHEN excluded.algo_id != '' THEN excluded.algo_id ELSE algo_id END,
		    side = CASE WHEN excluded.side != '' THEN excluded.side ELSE side END,
		    status = excluded.status,
		    trigger_px = excluded.trigger_px,
		    size_contracts = excluded.size_contracts,
		    request_json = CASE WHEN excluded.request_json != '' THEN excluded.request_json ELSE request_json END,
		    evidence_json = CASE WHEN excluded.evidence_json != '' THEN excluded.evidence_json ELSE evidence_json END,
		    incident_id = CASE WHEN excluded.incident_id != '' THEN excluded.incident_id ELSE incident_id END,
		    alerted = MAX(alerted, excluded.alerted),
		    version = excluded.version,
		    position_id = CASE WHEN excluded.position_id != '' THEN excluded.position_id ELSE position_id END,
		    place_attempts = excluded.place_attempts,
		    updated_at_ms = excluded.updated_at_ms
		WHERE strategy_id = excluded.strategy_id
		  AND account_key = excluded.account_key
		  AND inst_id = excluded.inst_id
		  AND algo_client_id = excluded.algo_client_id
		  AND entry_client_id = excluded.entry_client_id
		  AND owner = excluded.owner
		  AND policy_json = excluded.policy_json
		  AND excluded.version > version`,
		p.RecordID, sid, p.AccountKey, p.InstID, p.Symbol, p.Side,
		p.AlgoClientID, p.EntryClientID, p.AlgoID, p.Status, p.Owner, p.PolicyJSON, p.TriggerPx, p.SizeContracts,
		p.RequestJSON, p.EvidenceJSON, p.IncidentID, alerted, p.Version, p.PositionID, p.PlaceAttempts,
		p.CreatedAtMs, p.UpdatedAtMs)
	if err != nil {
		return fmt.Errorf("upsert OKX protection %s: %w", p.RecordID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("OKX protection %s was not saved; the saved version or identity changed", p.RecordID)
	}
	return nil
}

func (sdb *StateDB) writeOKXProtectionTx(tx *sql.Tx, s *StrategyState) error {
	if s == nil || s.OKXProtection == nil {
		return nil
	}
	p := s.OKXProtection
	sid, err := sdb.okxProtectionOwner(s.ID)
	if err != nil {
		return err
	}
	if p.Version > p.durableVersion {
		if err := upsertOKXProtectionRow(tx, sid, p); err != nil {
			return err
		}
	}
	for _, fill := range p.PendingFills {
		res, err := tx.Exec(`INSERT INTO okx_protection_fills (
			account_key, fill_id, scope, source, strategy_id, record_id, side, qty, px, fee, applied, created_at_ms)
			VALUES (?, ?, 'live', '', ?, ?, ?, ?, ?, ?, 1, ?)
			ON CONFLICT(account_key, fill_id) DO NOTHING`,
			p.AccountKey, fill.FillID, sid, p.RecordID, fill.Side, fill.Qty, fill.Px, fill.Fee, time.Now().UnixMilli())
		if err != nil {
			return fmt.Errorf("insert OKX protection fill %s: %w", fill.FillID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 && (p.AppliedFills == nil || !p.AppliedFills[fill.FillID]) {
			return fmt.Errorf("OKX protection fill %s is already stored and was not applied to this book", fill.FillID)
		}
	}
	return nil
}

func pruneOKXProtectionAfterCommit(s *StrategyState) {
	if s == nil || s.OKXProtection == nil {
		return
	}
	p := s.OKXProtection
	if p.Version > p.durableVersion {
		p.durableVersion = p.Version
	}
	if p.AppliedFills == nil {
		p.AppliedFills = map[string]bool{}
	}
	for _, fill := range p.PendingFills {
		p.AppliedFills[fill.FillID] = true
	}
	p.PendingFills = nil
	if !p.unresolved() {
		s.OKXProtection = nil
	}
}

func scanOKXProtection(scan func(dest ...interface{}) error) (*okxProtection, string, error) {
	var p okxProtection
	var scope, source, sid string
	var alerted int
	if err := scan(&p.RecordID, &scope, &source, &sid, &p.AccountKey, &p.InstID, &p.Symbol, &p.Side,
		&p.AlgoClientID, &p.EntryClientID, &p.AlgoID, &p.Status, &p.Owner, &p.PolicyJSON, &p.TriggerPx, &p.SizeContracts,
		&p.RequestJSON, &p.EvidenceJSON, &p.IncidentID, &alerted, &p.Version, &p.PositionID, &p.PlaceAttempts,
		&p.CreatedAtMs, &p.UpdatedAtMs); err != nil {
		return nil, "", err
	}
	if scope != string(ScopeLive) || source != "" {
		return nil, "", fmt.Errorf("OKX protection %s carries partition %s/%s", p.RecordID, scope, source)
	}
	p.Alerted = alerted == 1
	p.durableVersion = p.Version
	p.AppliedFills = map[string]bool{}
	return &p, sid, nil
}

func (sdb *StateDB) loadUnresolvedOKXProtection() ([]*okxProtection, []string, error) {
	exists, err := tableExists(sdb, "okx_protection")
	if err != nil || !exists {
		return nil, nil, err
	}
	rows, err := sdb.db.Query(`SELECT ` + okxProtectionColumns + ` FROM okx_protection
		WHERE status IN ` + okxProtectionUnresolvedSQL + ` ORDER BY created_at_ms, record_id`)
	if err != nil {
		return nil, nil, fmt.Errorf("read OKX protection in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	defer rows.Close()
	var out []*okxProtection
	var sids []string
	for rows.Next() {
		p, sid, err := scanOKXProtection(rows.Scan)
		if err != nil {
			return nil, nil, err
		}
		fills, err := sdb.loadOKXProtectionFills(p.AccountKey, p.RecordID)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range fills {
			p.AppliedFills[id] = true
		}
		out = append(out, p)
		sids = append(sids, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return out, sids, nil
}

func (sdb *StateDB) loadOKXProtectionFills(accountKey, recordID string) ([]string, error) {
	rows, err := sdb.db.Query(`SELECT fill_id FROM okx_protection_fills WHERE account_key = ? AND record_id = ? ORDER BY fill_id`, accountKey, recordID)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
