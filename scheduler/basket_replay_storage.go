package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	basketReplayDBName       = "replay-state.db"
	basketReplayManifestName = "replay-manifest.json"
	basketReplayLogDirName   = "logs"
	basketReplayOwnerMarker  = "go-trader basket-replay adapter"
)

const basketReplaySchemaDDL = `
CREATE TABLE IF NOT EXISTS basket_replay_runs (
    run_id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    protocol TEXT NOT NULL,
    source_fingerprint TEXT NOT NULL,
    contract_fingerprint TEXT NOT NULL,
    input_fingerprint TEXT NOT NULL,
    config_fingerprint TEXT NOT NULL,
    run_fingerprint TEXT NOT NULL,
    cost_policy_id TEXT NOT NULL DEFAULT '',
    cost_model_version INTEGER NOT NULL DEFAULT 0,
    steps_total INTEGER NOT NULL DEFAULT 0,
    steps_applied INTEGER NOT NULL DEFAULT 0,
    cursor_ms INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS fill_cost_policies (
    policy_id TEXT PRIMARY KEY,
    model_version INTEGER NOT NULL,
    run_fingerprint TEXT NOT NULL,
    policy_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS basket_replay_venue_events (
    event_id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL,
    client_order_id TEXT NOT NULL,
    action_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    script_index INTEGER NOT NULL,
    outcome_json TEXT NOT NULL,
    bounds_json TEXT NOT NULL DEFAULT '',
    at_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS basket_replay_steps (
    run_id TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    record_json TEXT NOT NULL,
    lot_refusals INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, step_index)
);

CREATE INDEX IF NOT EXISTS idx_basket_replay_venue_cloid ON basket_replay_venue_events(run_id, client_order_id, event_id);
CREATE INDEX IF NOT EXISTS idx_basket_replay_venue_step ON basket_replay_venue_events(run_id, step_index, kind);
`

type basketReplayManifest struct {
	Owner        string                   `json:"owner"`
	RunID        string                   `json:"run_id"`
	Fingerprints basketReplayFingerprints `json:"fingerprints"`
	CostPolicyID string                   `json:"cost_policy_id,omitempty"`
	CostVersion  int                      `json:"cost_model_version"`
	StepsTotal   int                      `json:"steps_total"`
	UID          int                      `json:"uid"`
	CreatedAt    string                   `json:"created_at"`
}

var basketReplayDeployedPrefixes = []string{"/var/lib/go-trader", "/opt/go-trader", "/etc/go-trader", "/etc/systemd"}

func basketReplayPathAllowed(dir string) error {
	for _, p := range basketReplayDeployedPrefixes {
		if dir == p || strings.HasPrefix(dir, p+string(os.PathSeparator)) {
			return fmt.Errorf("run.state_dir %q is inside the deployed path %s; a replay never opens deployed state", dir, p)
		}
	}
	return nil
}

func basketReplayRealParent(dir string) (string, error) {
	parent := filepath.Dir(dir)
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("run.state_dir parent %q is not readable: %w", parent, err)
	}
	if real != parent {
		return "", fmt.Errorf("run.state_dir parent %q resolves through a symbolic link to %q; give the real path", parent, real)
	}
	return real, nil
}

func basketReplayOwnedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

var basketReplayAllowedEntries = map[string]bool{
	basketReplayDBName:              true,
	basketReplayDBName + "-wal":     true,
	basketReplayDBName + "-shm":     true,
	basketReplayDBName + "-journal": true,
	basketReplayDBName + ".lock":    true,
	basketReplayManifestName:        true,
	basketReplayLogDirName:          true,
}

func checkBasketReplayStateDir(mode, dir string) error {
	if err := basketReplayPathAllowed(dir); err != nil {
		return refuseReplay("state_dir_refused", err.Error())
	}
	if _, err := basketReplayRealParent(dir); err != nil {
		return refuseReplay("state_dir_refused", err.Error())
	}
	info, err := os.Lstat(dir)
	switch mode {
	case basketReplayModeNew:
		if err == nil {
			return refuseReplay("state_exists", fmt.Sprintf("new-run mode refuses the existing path %q; a new run creates its own empty state directory", dir))
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return refuseReplay("state_dir_refused", err.Error())
		}
		return nil
	case basketReplayModeResume:
		if err != nil {
			return refuseReplay("state_missing", fmt.Sprintf("resume mode needs the adapter-created state directory %q: %v", dir, err))
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return refuseReplay("state_foreign", fmt.Sprintf("%q is not a plain directory", dir))
		}
		if !basketReplayOwnedByMe(info) {
			return refuseReplay("state_foreign", fmt.Sprintf("%q is owned by another user", dir))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return refuseReplay("state_foreign", err.Error())
		}
		var extra []string
		for _, e := range entries {
			if !basketReplayAllowedEntries[e.Name()] {
				extra = append(extra, e.Name())
				continue
			}
			if e.Type()&os.ModeSymlink != 0 {
				extra = append(extra, e.Name()+" (symbolic link)")
			}
		}
		sort.Strings(extra)
		if len(extra) > 0 {
			return refuseReplay("state_foreign", fmt.Sprintf("%q holds entries the adapter never creates: %s", dir, strings.Join(extra, ", ")))
		}
		return nil
	}
	return refuseReplay("spec_invalid", fmt.Sprintf("unknown mode %q", mode))
}

func createBasketReplayStateDir(dir string, m basketReplayManifest) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return refuseReplay("state_exists", fmt.Sprintf("could not create the run state directory %q exclusively: %v", dir, err))
	}
	if err := os.Mkdir(filepath.Join(dir, basketReplayLogDirName), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, basketReplayManifestName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(blob, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readBasketReplayManifest(dir string) (basketReplayManifest, error) {
	var m basketReplayManifest
	path := filepath.Join(dir, basketReplayManifestName)
	info, err := os.Lstat(path)
	if err != nil {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the run manifest %q is missing: %v", path, err))
	}
	if !info.Mode().IsRegular() || !basketReplayOwnedByMe(info) {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the run manifest %q is not a regular file this user owns", path))
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return m, refuseReplay("state_foreign", err.Error())
	}
	dec := json.NewDecoder(strings.NewReader(string(blob)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the run manifest is unreadable: %v", err))
	}
	if m.Owner != basketReplayOwnerMarker {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the run manifest owner %q is not this adapter", m.Owner))
	}
	return m, nil
}

func compareBasketReplayManifest(saved, want basketReplayManifest) error {
	var diffs []string
	if saved.RunID != want.RunID {
		diffs = append(diffs, fmt.Sprintf("run id %q != %q", saved.RunID, want.RunID))
	}
	pairs := []struct{ name, a, b string }{
		{"protocol", saved.Fingerprints.Protocol, want.Fingerprints.Protocol},
		{"source", saved.Fingerprints.Source, want.Fingerprints.Source},
		{"contract", saved.Fingerprints.Contract, want.Fingerprints.Contract},
		{"input", saved.Fingerprints.Input, want.Fingerprints.Input},
		{"config", saved.Fingerprints.Config, want.Fingerprints.Config},
		{"run", saved.Fingerprints.Run, want.Fingerprints.Run},
		{"cost policy", saved.CostPolicyID, want.CostPolicyID},
	}
	for _, p := range pairs {
		if p.a != p.b {
			diffs = append(diffs, fmt.Sprintf("%s fingerprint changed", p.name))
		}
	}
	if saved.CostVersion != want.CostVersion {
		diffs = append(diffs, fmt.Sprintf("cost model version %d != %d", saved.CostVersion, want.CostVersion))
	}
	if saved.UID != want.UID {
		diffs = append(diffs, "the run was created by another user")
	}
	if len(diffs) > 0 {
		return refuseReplay("resume_refused", diffs...)
	}
	return nil
}

func verifyBasketReplayDBReadOnly(path string, want basketReplayManifest) error {
	info, err := os.Lstat(path)
	if err != nil {
		return refuseReplay("state_foreign", fmt.Sprintf("the run state file %q is missing: %v", path, err))
	}
	if !info.Mode().IsRegular() || !basketReplayOwnedByMe(info) {
		return refuseReplay("state_foreign", fmt.Sprintf("the run state file %q is not a regular file this user owns", path))
	}
	db, err := openStateDBReadOnly(path)
	if err != nil {
		return refuseReplay("state_foreign", fmt.Sprintf("read-only open of %q failed: %v", path, err))
	}
	defer db.Close()
	ok, err := tableExists(db, "basket_replay_runs")
	if err != nil {
		return refuseReplay("state_foreign", err.Error())
	}
	if !ok {
		return refuseReplay("state_foreign", fmt.Sprintf("%q has no replay run record; the adapter accepts only state it created", path))
	}
	rows, err := db.db.Query(`SELECT run_id, owner, protocol, source_fingerprint, contract_fingerprint, input_fingerprint, config_fingerprint, run_fingerprint, cost_policy_id, cost_model_version FROM basket_replay_runs`)
	if err != nil {
		return refuseReplay("state_foreign", err.Error())
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		var got basketReplayManifest
		var owner string
		if err := rows.Scan(&got.RunID, &owner, &got.Fingerprints.Protocol, &got.Fingerprints.Source, &got.Fingerprints.Contract, &got.Fingerprints.Input, &got.Fingerprints.Config, &got.Fingerprints.Run, &got.CostPolicyID, &got.CostVersion); err != nil {
			return refuseReplay("state_foreign", err.Error())
		}
		if owner != basketReplayOwnerMarker {
			return refuseReplay("state_foreign", fmt.Sprintf("the replay run record owner %q is not this adapter", owner))
		}
		got.UID = want.UID
		if err := compareBasketReplayManifest(got, want); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return refuseReplay("state_foreign", err.Error())
	}
	if n != 1 {
		return refuseReplay("state_foreign", fmt.Sprintf("%q holds %d replay run records, not exactly one", path, n))
	}
	return nil
}

func (sdb *StateDB) ensureBasketReplaySchema() error {
	if _, err := sdb.db.Exec(basketReplaySchemaDDL); err != nil {
		return fmt.Errorf("create basket replay tables: %w", err)
	}
	return nil
}

func (sdb *StateDB) insertBasketReplayRun(m basketReplayManifest, policy fillCostPolicy) error {
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
	if _, err := tx.Exec(`INSERT INTO basket_replay_runs (run_id, owner, protocol, source_fingerprint, contract_fingerprint, input_fingerprint, config_fingerprint, run_fingerprint, cost_policy_id, cost_model_version, steps_total, created_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.RunID, basketReplayOwnerMarker, m.Fingerprints.Protocol, m.Fingerprints.Source, m.Fingerprints.Contract, m.Fingerprints.Input, m.Fingerprints.Config, m.Fingerprints.Run, m.CostPolicyID, m.CostVersion, m.StepsTotal, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("insert replay run record: %w", err)
	}
	if policy.PolicyID != "" {
		ev := fillCostPolicyEvidence{Version: fillCostPolicyEvidenceVers, Policy: policy, DefaultPolicy: defaultFillCostPolicy(), RunFingerprint: m.Fingerprints.Run}
		blob, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO fill_cost_policies (policy_id, model_version, run_fingerprint, policy_json) VALUES (?, ?, ?, ?)`,
			policy.PolicyID, policy.ModelVersion, m.Fingerprints.Run, string(blob)); err != nil {
			return fmt.Errorf("insert fill cost policy evidence: %w", err)
		}
	}
	return tx.Commit()
}

type basketReplayProgress struct {
	StepsApplied int
	CursorMs     int64
}

func (sdb *StateDB) readBasketReplayProgress(runID string) (basketReplayProgress, error) {
	var p basketReplayProgress
	err := sdb.db.QueryRow(`SELECT steps_applied, cursor_ms FROM basket_replay_runs WHERE run_id = ?`, runID).Scan(&p.StepsApplied, &p.CursorMs)
	return p, err
}

func (sdb *StateDB) verifyBasketReplayPolicy(policy fillCostPolicy, runFP string) error {
	rows, err := sdb.db.Query(`SELECT policy_id, model_version, run_fingerprint, policy_json FROM fill_cost_policies ORDER BY policy_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, fp, blob string
		var ver int
		if err := rows.Scan(&id, &ver, &fp, &blob); err != nil {
			return err
		}
		ids = append(ids, id)
		if id != policy.PolicyID || ver != policy.ModelVersion || fp != runFP {
			return refuseReplay("resume_refused", fmt.Sprintf("stored cost policy %s (version %d) does not match the run's policy %q", id, ver, policy.PolicyID))
		}
		var ev fillCostPolicyEvidence
		if err := json.Unmarshal([]byte(blob), &ev); err != nil || ev.Policy != policy {
			return refuseReplay("resume_refused", fmt.Sprintf("stored cost policy evidence %s is unreadable or differs from the run's policy", id))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if (policy.PolicyID == "") != (len(ids) == 0) {
		return refuseReplay("resume_refused", fmt.Sprintf("the run's cost policy %q and the stored policy evidence %v disagree", policy.PolicyID, ids))
	}
	return nil
}

func writeBasketReplayProgressTx(tx *sql.Tx, runID string, p basketReplayProgress) error {
	res, err := tx.Exec(`UPDATE basket_replay_runs SET steps_applied = ?, cursor_ms = ? WHERE run_id = ? AND steps_applied <= ?`, p.StepsApplied, p.CursorMs, runID, p.StepsApplied)
	if err != nil {
		return fmt.Errorf("record replay progress: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("replay progress for %s would move back to step %d", runID, p.StepsApplied)
	}
	return nil
}

type basketReplayStoredStep struct {
	Record      basketReplayStepRecord
	LotRefusals int
}

func (sdb *StateDB) writeBasketReplaySteps(runID string, steps []basketReplayStoredStep) error {
	tx, err := sdb.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, st := range steps {
		blob, err := json.Marshal(st.Record)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO basket_replay_steps (run_id, step_index, record_json, lot_refusals) VALUES (?, ?, ?, ?)
			ON CONFLICT(run_id, step_index) DO UPDATE SET record_json = excluded.record_json, lot_refusals = excluded.lot_refusals`,
			runID, st.Record.Index, string(blob), st.LotRefusals); err != nil {
			return fmt.Errorf("record replay step %d: %w", st.Record.Index, err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) readBasketReplaySteps(runID string, applied int) ([]basketReplayStoredStep, error) {
	rows, err := sdb.db.Query(`SELECT step_index, record_json, lot_refusals FROM basket_replay_steps WHERE run_id = ? AND step_index < ? ORDER BY step_index`, runID, applied)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []basketReplayStoredStep
	for rows.Next() {
		var idx, lots int
		var blob string
		if err := rows.Scan(&idx, &blob, &lots); err != nil {
			return nil, err
		}
		var st basketReplayStoredStep
		dec := json.NewDecoder(strings.NewReader(blob))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&st.Record); err != nil {
			return nil, refuseReplay("state_foreign", fmt.Sprintf("the stored record of step %d is unreadable: %v", idx, err))
		}
		if st.Record.Index != idx {
			return nil, refuseReplay("state_foreign", fmt.Sprintf("the stored record of step %d names step %d", idx, st.Record.Index))
		}
		st.LotRefusals = lots
		out = append(out, st)
	}
	return out, rows.Err()
}

type basketReplayVenueEvent struct {
	ClientOrderID string
	ActionID      string
	Kind          string
	StepIndex     int
	ScriptIndex   int
	Outcome       basketVenueOutcome
	Bounds        *basketReplayBoundRecord
}

func (sdb *StateDB) insertBasketReplayVenueEvent(runID string, ev basketReplayVenueEvent, atMs int64) error {
	blob, err := json.Marshal(ev.Outcome)
	if err != nil {
		return err
	}
	bounds := ""
	if ev.Bounds != nil {
		b, err := json.Marshal(ev.Bounds)
		if err != nil {
			return err
		}
		bounds = string(b)
	}
	_, err = sdb.db.Exec(`INSERT INTO basket_replay_venue_events (run_id, client_order_id, action_id, kind, step_index, script_index, outcome_json, bounds_json, at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, ev.ClientOrderID, ev.ActionID, ev.Kind, ev.StepIndex, ev.ScriptIndex, string(blob), bounds, atMs)
	return err
}

func (sdb *StateDB) basketReplaySubmitBounds(runID string) (map[string]basketReplayBoundRecord, error) {
	rows, err := sdb.db.Query(`SELECT action_id, bounds_json FROM basket_replay_venue_events WHERE run_id = ? AND kind = 'submit' AND bounds_json != '' ORDER BY event_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]basketReplayBoundRecord{}
	for rows.Next() {
		var id, blob string
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		var b basketReplayBoundRecord
		if err := json.Unmarshal([]byte(blob), &b); err != nil {
			return nil, fmt.Errorf("replay venue bound evidence for %s is unreadable: %w", id, err)
		}
		out[id] = b
	}
	return out, rows.Err()
}

func (sdb *StateDB) countBasketReplayVenueEvents(runID string, step int, kind string) (int, error) {
	var n int
	err := sdb.db.QueryRow(`SELECT COUNT(*) FROM basket_replay_venue_events WHERE run_id = ? AND step_index = ? AND kind = ?`, runID, step, kind).Scan(&n)
	return n, err
}

func (sdb *StateDB) basketReplayVenueEvents(runID, cloid string) ([]basketReplayVenueEvent, error) {
	rows, err := sdb.db.Query(`SELECT client_order_id, action_id, kind, step_index, script_index, outcome_json FROM basket_replay_venue_events WHERE run_id = ? AND client_order_id = ? ORDER BY event_id`, runID, cloid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []basketReplayVenueEvent
	for rows.Next() {
		var ev basketReplayVenueEvent
		var blob string
		if err := rows.Scan(&ev.ClientOrderID, &ev.ActionID, &ev.Kind, &ev.StepIndex, &ev.ScriptIndex, &blob); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(blob), &ev.Outcome); err != nil {
			return nil, fmt.Errorf("replay venue evidence for %s is unreadable: %w", cloid, err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (sdb *StateDB) basketReplayActionCount() (int, error) {
	var n int
	err := sdb.db.QueryRow(`SELECT COUNT(*) FROM basket_member_actions`).Scan(&n)
	return n, err
}

func inspectBasketReplayRows(db *StateDB, fi *storageFileInspection) ([]string, error) {
	var rejections []string
	if ok, err := tableExists(db, "fill_cost_policies"); err != nil {
		return nil, err
	} else if ok {
		rows, err := db.db.Query(`SELECT policy_id, model_version FROM fill_cost_policies ORDER BY policy_id`)
		if err != nil {
			return nil, fmt.Errorf("read fill cost policies in the %s state file: %w", fi.Role, err)
		}
		for rows.Next() {
			var id string
			var ver int
			if err := rows.Scan(&id, &ver); err != nil {
				rows.Close()
				return nil, err
			}
			fi.FillCostPolicies = append(fi.FillCostPolicies, fmt.Sprintf("%s (cost model version %d)", id, ver))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	ok, err := tableExists(db, "basket_replay_runs")
	if err != nil || !ok {
		return rejections, err
	}
	rows, err := db.db.Query(`SELECT run_id, steps_applied, steps_total, cursor_ms, cost_policy_id FROM basket_replay_runs ORDER BY run_id`)
	if err != nil {
		return nil, fmt.Errorf("read basket replay runs in the %s state file: %w", fi.Role, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, policy string
		var applied, total int
		var cursor int64
		if err := rows.Scan(&id, &applied, &total, &cursor, &policy); err != nil {
			return nil, err
		}
		line := fmt.Sprintf("%s: %d of %d steps applied, cursor %s", id, applied, total, formatClosedBarMs(cursor))
		if policy != "" {
			line += ", cost policy " + policy
		}
		fi.ReplayRuns = append(fi.ReplayRuns, line)
		rejections = append(rejections, fmt.Sprintf("%s state file: it holds offline basket replay run %s; the scheduler never runs on replay state", fi.Role, id))
	}
	return rejections, rows.Err()
}
