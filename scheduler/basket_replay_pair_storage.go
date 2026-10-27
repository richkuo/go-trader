package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const pairReplaySchemaDDL = `
CREATE TABLE IF NOT EXISTS basket_replay_objects (
    run_id TEXT NOT NULL,
    object_hash TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    record_total INTEGER NOT NULL,
    manifest_json TEXT NOT NULL,
    PRIMARY KEY (run_id, object_hash)
);

CREATE TABLE IF NOT EXISTS basket_replay_failures (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    step_id TEXT NOT NULL,
    stage TEXT NOT NULL,
    kind TEXT NOT NULL,
    slot_ms INTEGER NOT NULL DEFAULT 0,
    member_id TEXT NOT NULL DEFAULT '',
    cause TEXT NOT NULL,
    at_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_basket_replay_failures_run ON basket_replay_failures(run_id, seq);
`

var pairReplayRunColumns = []struct{ name, ddl string }{
	{"schema_version", "INTEGER NOT NULL DEFAULT 1"},
	{"lifetime", "TEXT NOT NULL DEFAULT ''"},
	{"member_costs_json", "TEXT NOT NULL DEFAULT ''"},
	{"cursor_step_id", "TEXT NOT NULL DEFAULT ''"},
	{"cursor_phase", "TEXT NOT NULL DEFAULT ''"},
	{"cursor_binding_seq", "INTEGER NOT NULL DEFAULT 0"},
	{"pending_step_id", "TEXT NOT NULL DEFAULT ''"},
	{"pending_step_ms", "INTEGER NOT NULL DEFAULT 0"},
	{"pending_prefix_hash", "TEXT NOT NULL DEFAULT ''"},
	{"recording_cursor", "INTEGER NOT NULL DEFAULT 0"},
	{"recording_logical_ms", "INTEGER NOT NULL DEFAULT 0"},
}

var pairReplayStepColumns = []struct{ name, ddl string }{
	{"step_id", "TEXT NOT NULL DEFAULT ''"},
	{"at_ms", "INTEGER NOT NULL DEFAULT 0"},
	{"phase", "TEXT NOT NULL DEFAULT ''"},
	{"status", "TEXT NOT NULL DEFAULT ''"},
	{"result_hash", "TEXT NOT NULL DEFAULT ''"},
}

const (
	pairReplayPhasePending     = "pending"
	pairReplayPhaseCoordinator = "coordinator"
	pairReplayPhaseComplete    = "complete"
	pairReplayStatusPending    = "pending"
	pairReplayStatusComplete   = "complete"
)

func (sdb *StateDB) migrateBasketReplayColumns() error {
	for _, set := range []struct {
		table string
		cols  []struct{ name, ddl string }
	}{{"basket_replay_runs", pairReplayRunColumns}, {"basket_replay_steps", pairReplayStepColumns}} {
		for _, c := range set.cols {
			ok, err := sdb.tableHasColumn(set.table, c.name)
			if err != nil {
				return fmt.Errorf("introspect %s: %w", set.table, err)
			}
			if ok {
				continue
			}
			if _, err := sdb.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", set.table, c.name, c.ddl)); err != nil {
				return fmt.Errorf("add %s.%s: %w", set.table, c.name, err)
			}
		}
	}
	if _, err := sdb.db.Exec(pairReplaySchemaDDL); err != nil {
		return fmt.Errorf("create pair replay tables: %w", err)
	}
	ok, err := sdb.tableHasColumn("basket_replay_failures", "call_id")
	if err != nil {
		return fmt.Errorf("introspect basket_replay_failures: %w", err)
	}
	if !ok {
		if _, err := sdb.db.Exec("ALTER TABLE basket_replay_failures ADD COLUMN call_id TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("add basket_replay_failures.call_id: %w", err)
		}
	}
	return nil
}

type pairReplayObservation struct {
	Cycle      int                    `json:"cycle"`
	AtMs       int64                  `json:"at_ms"`
	Fields     StrategyDecisionFields `json:"fields"`
	Indicators map[string]interface{} `json:"indicators"`
	Price      float64                `json:"price"`
}

type pairReplayHostState struct {
	Cycle            int                              `json:"cycle"`
	NextManageMs     int64                            `json:"next_manage_ms"`
	LastStepMs       int64                            `json:"last_step_ms"`
	ViewGen          uint64                           `json:"view_generation"`
	PrepSeq          uint64                           `json:"preparation_seq"`
	Observations     map[string]pairReplayObservation `json:"observations"`
	EchoFailures     map[string]string                `json:"echo_failures"`
	PollNextMs       int64                            `json:"poll_next_ms,omitempty"`
	FundingNextMs    int64                            `json:"funding_next_ms,omitempty"`
	CapabilityNextMs int64                            `json:"capability_next_ms,omitempty"`
	OracleNextMs     int64                            `json:"oracle_next_ms,omitempty"`
	PairView         map[string]pairMemberValuation   `json:"pair_view,omitempty"`
	PairLastMarks    map[string]pairLastMark          `json:"pair_last_marks,omitempty"`
}

type pairReplayCollection struct {
	RunID        string   `json:"run_id"`
	PairID       string   `json:"pair_id"`
	ContractFP   string   `json:"contract_fingerprint"`
	SlotMs       int64    `json:"slot_ms"`
	StartMs      int64    `json:"start_ms"`
	DeadlineMs   int64    `json:"deadline_ms"`
	CutoffMs     int64    `json:"cutoff_ms"`
	InputFP      string   `json:"input_fingerprint"`
	EvidenceJSON string   `json:"envelope"`
	SnapshotID   string   `json:"snapshot_id"`
	Generation   uint64   `json:"generation"`
	ObjectHashes []string `json:"object_hashes"`
}

type pairReplayCoordinatorCheckpoint struct {
	HostAfter    pairReplayHostState    `json:"host_after"`
	Managed      bool                   `json:"managed"`
	Risk         []pairReplayRiskRecord `json:"risk"`
	RecCursor    int                    `json:"recording_cursor,omitempty"`
	RecOrdinal   int                    `json:"recording_ordinal,omitempty"`
	RecLogicalMs int64                  `json:"recording_logical_ms,omitempty"`
}

type pairReplayPendingRecord struct {
	V            int                              `json:"v"`
	StepID       string                           `json:"step_id"`
	AtMs         int64                            `json:"at_ms"`
	Ordinal      int                              `json:"ordinal"`
	PrefixHash   string                           `json:"prefix_hash"`
	HostBefore   pairReplayHostState              `json:"host_before"`
	Collection   *pairReplayCollection            `json:"collection,omitempty"`
	Coordinator  *pairReplayCoordinatorCheckpoint `json:"coordinator,omitempty"`
	RecSeq       int64                            `json:"recording_seq,omitempty"`
	RecCursor    int                              `json:"recording_cursor,omitempty"`
	RecLogicalMs int64                            `json:"recording_logical_ms,omitempty"`
}

type pairReplayCursor struct {
	Applied      int
	CursorMs     int64
	CursorStepID string
	CursorPhase  string
	BindingSeq   int64
	PendingID    string
	PendingMs    int64
	PendingHash  string
	MemberCosts  string
	Lifetime     string
	Schema       int
}

type pairReplayStoredStep struct {
	Ordinal    int
	StepID     string
	AtMs       int64
	Phase      string
	Status     string
	ResultHash string
	Record     json.RawMessage
}

type pairReplayFailure struct {
	Seq      int64  `json:"seq"`
	Ordinal  int    `json:"step_index"`
	StepID   string `json:"step_id"`
	Stage    string `json:"stage"`
	Kind     string `json:"kind"`
	SlotMs   int64  `json:"slot_ms,omitempty"`
	MemberID string `json:"member_id,omitempty"`
	Cause    string `json:"cause"`
	AtMs     int64  `json:"at_ms"`
	CallID   string `json:"call_id,omitempty"`
}

func (sdb *StateDB) insertPairReplayRun(m pairReplayManifest, objects []pairReplayObjectRef, totals map[string]int) error {
	tx, err := sdb.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM basket_replay_runs`).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return refuseReplay("state_exists", "the new run state file already holds a replay run record")
	}
	if _, err := tx.Exec(`INSERT INTO basket_replay_runs (run_id, owner, protocol, source_fingerprint, contract_fingerprint, input_fingerprint, config_fingerprint, run_fingerprint, cost_policy_id, cost_model_version, steps_total, created_at_ms, schema_version, lifetime)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', 0, ?, ?, 2, ?)`,
		m.RunID, basketReplayOwnerMarker, m.Fingerprints.Protocol, m.Fingerprints.Source, m.Fingerprints.Contract, m.Fingerprints.Input, m.Fingerprints.Config, m.Fingerprints.Run,
		m.Lifetime.MaxSteps, time.Now().UnixMilli(), m.Lifetime.String()); err != nil {
		return fmt.Errorf("insert pair replay run record: %w", err)
	}
	for _, o := range objects {
		blob, err := json.Marshal(o)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO basket_replay_objects (run_id, object_hash, relative_path, size_bytes, record_total, manifest_json) VALUES (?, ?, ?, ?, ?, ?)`,
			m.RunID, o.SHA256, o.Path, o.SizeBytes, totals[o.Kind+"|"+o.MemberID], string(blob)); err != nil {
			return fmt.Errorf("record input object %s: %w", o.Path, err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) setPairReplayMemberCosts(runID, costs string) error {
	res, err := sdb.db.Exec(`UPDATE basket_replay_runs SET member_costs_json = ? WHERE run_id = ? AND member_costs_json = ''`, costs, runID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("the replay run %s already froze its member costs", runID)
	}
	return nil
}

func readPairReplayCursor(q interface {
	QueryRow(string, ...interface{}) *sql.Row
}, runID string) (pairReplayCursor, error) {
	var c pairReplayCursor
	err := q.QueryRow(`SELECT steps_applied, cursor_ms, cursor_step_id, cursor_phase, cursor_binding_seq, pending_step_id, pending_step_ms, pending_prefix_hash, member_costs_json, lifetime, schema_version FROM basket_replay_runs WHERE run_id = ?`, runID).
		Scan(&c.Applied, &c.CursorMs, &c.CursorStepID, &c.CursorPhase, &c.BindingSeq, &c.PendingID, &c.PendingMs, &c.PendingHash, &c.MemberCosts, &c.Lifetime, &c.Schema)
	return c, err
}

func (sdb *StateDB) readPairReplayCursor(runID string) (pairReplayCursor, error) {
	return readPairReplayCursor(sdb.db, runID)
}

func (sdb *StateDB) beginPairReplayStep(runID string, rec pairReplayPendingRecord) (pairReplayPendingRecord, error) {
	tx, err := sdb.db.Begin()
	if err != nil {
		return rec, err
	}
	defer tx.Rollback()
	var stepID, status, blob string
	err = tx.QueryRow(`SELECT step_id, status, record_json FROM basket_replay_steps WHERE run_id = ? AND step_index = ?`, runID, rec.Ordinal).Scan(&stepID, &status, &blob)
	switch {
	case err == nil:
		if status != pairReplayStatusPending || stepID != rec.StepID {
			return rec, fmt.Errorf("replay step %d already exists as %s %s", rec.Ordinal, status, stepID)
		}
		var saved pairReplayPendingRecord
		if err := pairReplayStrictDecode([]byte(blob), &saved); err != nil {
			return rec, fmt.Errorf("the saved pending record of step %d is unreadable: %w", rec.Ordinal, err)
		}
		if saved.AtMs != rec.AtMs || saved.PrefixHash != rec.PrefixHash {
			return rec, fmt.Errorf("the saved pending record of step %d binds time %d and prefix %s, not %d and %s", rec.Ordinal, saved.AtMs, shortFingerprint(saved.PrefixHash), rec.AtMs, shortFingerprint(rec.PrefixHash))
		}
		return saved, tx.Commit()
	case err != sql.ErrNoRows:
		return rec, err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return rec, err
	}
	if _, err := tx.Exec(`INSERT INTO basket_replay_steps (run_id, step_index, record_json, lot_refusals, step_id, at_ms, phase, status, result_hash) VALUES (?, ?, ?, 0, ?, ?, ?, ?, '')`,
		runID, rec.Ordinal, string(b), rec.StepID, rec.AtMs, pairReplayPhasePending, pairReplayStatusPending); err != nil {
		return rec, fmt.Errorf("record pending replay step %d: %w", rec.Ordinal, err)
	}
	res, err := tx.Exec(`UPDATE basket_replay_runs SET pending_step_id = ?, pending_step_ms = ?, pending_prefix_hash = ? WHERE run_id = ? AND steps_applied = ?`, rec.StepID, rec.AtMs, rec.PrefixHash, runID, rec.Ordinal)
	if err != nil {
		return rec, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return rec, fmt.Errorf("the replay cursor is not at step %d", rec.Ordinal)
	}
	return rec, tx.Commit()
}

func (sdb *StateDB) updatePairReplayPending(runID string, rec pairReplayPendingRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	res, err := sdb.db.Exec(`UPDATE basket_replay_steps SET record_json = ? WHERE run_id = ? AND step_index = ? AND status = ?`, string(b), runID, rec.Ordinal, pairReplayStatusPending)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("replay step %d is not pending", rec.Ordinal)
	}
	return nil
}

func writePairReplayCoordinatorTx(tx *sql.Tx, runID string, rec pairReplayPendingRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE basket_replay_steps SET record_json = ?, phase = ? WHERE run_id = ? AND step_index = ? AND status = ?`, string(b), pairReplayPhaseCoordinator, runID, rec.Ordinal, pairReplayStatusPending)
	if err != nil {
		return fmt.Errorf("record the replay coordinator checkpoint: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("replay step %d is not pending", rec.Ordinal)
	}
	return nil
}

type pairReplayFinalCommit struct {
	Ordinal      int
	StepID       string
	AtMs         int64
	Record       []byte
	ResultHash   string
	BindingSeq   int64
	Recorded     bool
	RecCursor    int
	RecLogicalMs int64
}

func writePairReplayFinalTx(tx *sql.Tx, runID string, c pairReplayFinalCommit) error {
	res, err := tx.Exec(`UPDATE basket_replay_steps SET record_json = ?, phase = ?, status = ?, result_hash = ? WHERE run_id = ? AND step_index = ? AND step_id = ? AND status = ?`,
		string(c.Record), pairReplayPhaseComplete, pairReplayStatusComplete, c.ResultHash, runID, c.Ordinal, c.StepID, pairReplayStatusPending)
	if err != nil {
		return fmt.Errorf("record replay step %d: %w", c.Ordinal, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("replay step %d is not pending", c.Ordinal)
	}
	res, err = tx.Exec(`UPDATE basket_replay_runs SET steps_applied = ?, cursor_ms = ?, cursor_step_id = ?, cursor_phase = ?, cursor_binding_seq = ?, pending_step_id = '', pending_step_ms = 0, pending_prefix_hash = '' WHERE run_id = ? AND steps_applied = ? AND pending_step_id = ?`,
		c.Ordinal+1, c.AtMs, c.StepID, pairReplayPhaseComplete, c.BindingSeq, runID, c.Ordinal, c.StepID)
	if err != nil {
		return fmt.Errorf("advance the replay cursor: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("the replay cursor of %s is not at pending step %d", runID, c.Ordinal)
	}
	if c.Recorded {
		if _, err := tx.Exec(`UPDATE basket_replay_runs SET recording_cursor = ?, recording_logical_ms = ? WHERE run_id = ?`, c.RecCursor, c.RecLogicalMs, runID); err != nil {
			return fmt.Errorf("advance the recording cursor: %w", err)
		}
	}
	return nil
}

func (sdb *StateDB) readPairRecCursor(runID string) (int, int64, error) {
	var c int
	var l int64
	err := sdb.db.QueryRow(`SELECT recording_cursor, recording_logical_ms FROM basket_replay_runs WHERE run_id = ?`, runID).Scan(&c, &l)
	return c, l, err
}

func (sdb *StateDB) readPairReplaySteps(runID string) ([]pairReplayStoredStep, error) {
	rows, err := sdb.db.Query(`SELECT step_index, step_id, at_ms, phase, status, result_hash, record_json FROM basket_replay_steps WHERE run_id = ? ORDER BY step_index`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pairReplayStoredStep
	for rows.Next() {
		var s pairReplayStoredStep
		var blob string
		if err := rows.Scan(&s.Ordinal, &s.StepID, &s.AtMs, &s.Phase, &s.Status, &s.ResultHash, &blob); err != nil {
			return nil, err
		}
		s.Record = json.RawMessage(blob)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (sdb *StateDB) insertPairReplayFailure(runID string, f pairReplayFailure) error {
	_, err := sdb.db.Exec(`INSERT INTO basket_replay_failures (run_id, step_index, step_id, stage, kind, slot_ms, member_id, cause, at_ms, call_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, f.Ordinal, f.StepID, f.Stage, f.Kind, f.SlotMs, f.MemberID, f.Cause, f.AtMs, f.CallID)
	return err
}

func (sdb *StateDB) readPairReplayFailures(runID string) ([]pairReplayFailure, error) {
	rows, err := sdb.db.Query(`SELECT seq, step_index, step_id, stage, kind, slot_ms, member_id, cause, at_ms, call_id FROM basket_replay_failures WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pairReplayFailure
	for rows.Next() {
		var f pairReplayFailure
		if err := rows.Scan(&f.Seq, &f.Ordinal, &f.StepID, &f.Stage, &f.Kind, &f.SlotMs, &f.MemberID, &f.Cause, &f.AtMs, &f.CallID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (sdb *StateDB) readPairReplayObjects(runID string) ([]pairReplayObjectRef, error) {
	rows, err := sdb.db.Query(`SELECT manifest_json FROM basket_replay_objects WHERE run_id = ? ORDER BY object_hash`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pairReplayObjectRef
	for rows.Next() {
		var blob string
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		var o pairReplayObjectRef
		if err := pairReplayStrictDecode([]byte(blob), &o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func pairReplayObjectSet(objs []pairReplayObjectRef) string {
	parts := make([]string, 0, len(objs))
	for _, o := range objs {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%d", o.Kind, o.MemberID, o.Path, o.SHA256, o.SizeBytes))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func verifyPairReplayDBReadOnly(path string, want pairReplayManifest) (pairReplayCursor, error) {
	var c pairReplayCursor
	if err := verifyBasketReplayDBReadOnlyRun(path, want); err != nil {
		return c, err
	}
	db, err := openStateDBReadOnly(path)
	if err != nil {
		return c, refuseReplay("state_foreign", err.Error())
	}
	defer db.Close()
	c, err = db.readPairReplayCursor(want.RunID)
	if err != nil {
		return c, refuseReplay("state_foreign", fmt.Sprintf("the replay run record is not a version-2 record: %v", err))
	}
	if c.Schema != pairReplayProtocolVersion || c.Lifetime != want.Lifetime.String() {
		return c, refuseReplay("resume_refused", fmt.Sprintf("the saved run is schema %d with lifetime %q, this run is schema 2 with lifetime %q", c.Schema, c.Lifetime, want.Lifetime.String()))
	}
	if c.MemberCosts == "" || pairReplayCompactJSON([]byte(c.MemberCosts)) != pairReplayCompactJSON(want.MemberCosts) {
		return c, refuseReplay("resume_refused", "the saved member cost bindings are missing or differ from the replay manifest; initialization never finished or the costs changed")
	}
	objs, err := db.readPairReplayObjects(want.RunID)
	if err != nil {
		return c, refuseReplay("state_foreign", err.Error())
	}
	if pairReplayObjectSet(objs) != pairReplayObjectSet(want.Objects) {
		return c, refuseReplay("resume_refused", "the saved input objects differ from this run's input manifest")
	}
	return c, nil
}

func verifyBasketReplayDBReadOnlyRun(path string, want pairReplayManifest) error {
	return verifyBasketReplayDBReadOnly(path, basketReplayManifest{Owner: basketReplayOwnerMarker, RunID: want.RunID, Fingerprints: want.Fingerprints, UID: want.UID})
}

func pairReplayCompactJSON(b []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return "invalid:" + string(b)
	}
	return buf.String()
}
