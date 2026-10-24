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
	quoteReplayDBName       = "quote-replay-state.db"
	quoteReplayManifestName = "quote-replay-manifest.json"
	quoteReplayLogDirName   = "logs"
	quoteReplayOwnerMarker  = "go-trader quote-replay runner"

	quoteReplayResultPending     = "pending"
	quoteReplayResultCompleted   = "completed"
	quoteReplayResultInterrupted = "interrupted"
	quoteReplayResultRecovery    = "recovery"
)

const quoteReplaySchemaDDL = `
CREATE TABLE IF NOT EXISTS quote_replay_runs (
    run_id TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    protocol TEXT NOT NULL,
    protocol_fingerprint TEXT NOT NULL,
    source_fingerprint TEXT NOT NULL,
    data_fingerprint TEXT NOT NULL,
    config_fingerprint TEXT NOT NULL,
    policy_fingerprint TEXT NOT NULL,
    model_fingerprint TEXT NOT NULL,
    run_fingerprint TEXT NOT NULL,
    owner_strategy_id TEXT NOT NULL,
    storage_strategy_id TEXT NOT NULL,
    first_deadline_ms INTEGER NOT NULL,
    last_deadline_ms INTEGER NOT NULL,
    cursor_ms INTEGER NOT NULL DEFAULT 0,
    refused TEXT NOT NULL DEFAULT '',
    created_at_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS quote_replay_venue_events (
    seq INTEGER PRIMARY KEY,
    run_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('issue','place','cancel','fill','trigger','venue_cancel','expire')),
    client_order_id TEXT NOT NULL DEFAULT '',
    effect_ms INTEGER NOT NULL,
    event_json TEXT NOT NULL,
    digest TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS quote_replay_decisions (
    run_id TEXT NOT NULL,
    deadline_ms INTEGER NOT NULL,
    record_json TEXT NOT NULL,
    digest TEXT NOT NULL,
    PRIMARY KEY (run_id, deadline_ms)
);

CREATE TABLE IF NOT EXISTS quote_replay_checkpoints (
    run_id TEXT NOT NULL,
    deadline_ms INTEGER NOT NULL,
    venue_seq INTEGER NOT NULL,
    checkpoint_json TEXT NOT NULL,
    digest TEXT NOT NULL,
    PRIMARY KEY (run_id, deadline_ms)
);

CREATE TABLE IF NOT EXISTS quote_replay_deadline_results (
    run_id TEXT NOT NULL,
    deadline_ms INTEGER NOT NULL,
    attempt INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending','completed','interrupted','recovery')),
    summary_json TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, deadline_ms, attempt)
);

CREATE TRIGGER IF NOT EXISTS quote_replay_venue_events_immutable
BEFORE UPDATE ON quote_replay_venue_events
BEGIN SELECT RAISE(ABORT, 'quote replay venue events are immutable'); END;

CREATE TRIGGER IF NOT EXISTS quote_replay_venue_events_retained
BEFORE DELETE ON quote_replay_venue_events
BEGIN SELECT RAISE(ABORT, 'quote replay venue events are retained'); END;

CREATE TRIGGER IF NOT EXISTS quote_replay_decisions_immutable
BEFORE UPDATE ON quote_replay_decisions
BEGIN SELECT RAISE(ABORT, 'quote replay decisions are immutable'); END;

CREATE TRIGGER IF NOT EXISTS quote_replay_decisions_retained
BEFORE DELETE ON quote_replay_decisions
BEGIN SELECT RAISE(ABORT, 'quote replay decisions are retained'); END;
`

type quoteReplayFingerprints struct {
	Protocol string `json:"protocol"`
	Source   string `json:"source"`
	Data     string `json:"data"`
	Config   string `json:"config"`
	Policy   string `json:"policy"`
	Model    string `json:"model"`
	Run      string `json:"run"`
}

type quoteReplayManifest struct {
	Owner           string                  `json:"owner"`
	RunID           string                  `json:"run_id"`
	Fingerprints    quoteReplayFingerprints `json:"fingerprints"`
	OwnerStrategy   string                  `json:"owner_strategy_id"`
	FirstDeadlineMs int64                   `json:"first_deadline_ms"`
	LastDeadlineMs  int64                   `json:"last_deadline_ms"`
	UID             int                     `json:"uid"`
	CreatedAt       string                  `json:"created_at"`
}

var quoteReplayAllowedEntries = map[string]bool{
	quoteReplayDBName:              true,
	quoteReplayDBName + "-wal":     true,
	quoteReplayDBName + "-shm":     true,
	quoteReplayDBName + "-journal": true,
	quoteReplayDBName + ".lock":    true,
	quoteReplayManifestName:        true,
	quoteReplayLogDirName:          true,
}

func quoteReplayOwnedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func quoteReplayHardLinked(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}

func quoteReplayRealPath(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return refuseReplay("state_dir_refused", fmt.Sprintf("run.state_dir %q must be an absolute clean path", dir))
	}
	if err := basketReplayPathAllowed(dir); err != nil {
		return refuseReplay("state_dir_refused", err.Error())
	}
	parent := filepath.Dir(dir)
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return refuseReplay("state_dir_refused", fmt.Sprintf("run.state_dir parent %q is not readable: %v", parent, err))
	}
	if real != parent {
		return refuseReplay("state_dir_refused", fmt.Sprintf("run.state_dir parent %q resolves through a symbolic link to %q; give the real path", parent, real))
	}
	if err := basketReplayPathAllowed(real); err != nil {
		return refuseReplay("state_dir_refused", err.Error())
	}
	return nil
}

func checkQuoteReplayStateDir(mode, dir string) error {
	if err := quoteReplayRealPath(dir); err != nil {
		return err
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
			return refuseReplay("state_missing", fmt.Sprintf("resume mode needs the runner-created state directory %q: %v", dir, err))
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return refuseReplay("state_foreign", fmt.Sprintf("%q is not a plain directory", dir))
		}
		if !quoteReplayOwnedByMe(info) {
			return refuseReplay("state_foreign", fmt.Sprintf("%q is owned by another user", dir))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return refuseReplay("state_foreign", err.Error())
		}
		var extra []string
		for _, e := range entries {
			if !quoteReplayAllowedEntries[e.Name()] {
				extra = append(extra, e.Name())
				continue
			}
			full := filepath.Join(dir, e.Name())
			fi, err := os.Lstat(full)
			if err != nil {
				extra = append(extra, e.Name()+" (unreadable)")
				continue
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				extra = append(extra, e.Name()+" (symbolic link)")
				continue
			}
			if fi.Mode().IsRegular() && quoteReplayHardLinked(fi) {
				extra = append(extra, e.Name()+" (hard link)")
				continue
			}
			if !quoteReplayOwnedByMe(fi) {
				extra = append(extra, e.Name()+" (owned by another user)")
			}
		}
		sort.Strings(extra)
		if len(extra) > 0 {
			return refuseReplay("state_foreign", fmt.Sprintf("%q holds entries the runner never creates: %s", dir, strings.Join(extra, ", ")))
		}
		return nil
	}
	return refuseReplay("spec_invalid", fmt.Sprintf("unknown mode %q", mode))
}

func createQuoteReplayStateDir(dir string, m quoteReplayManifest) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return refuseReplay("state_exists", fmt.Sprintf("could not create the run state directory %q exclusively: %v", dir, err))
	}
	if err := os.Mkdir(filepath.Join(dir, quoteReplayLogDirName), 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, quoteReplayManifestName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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

func readQuoteReplayManifest(dir string) (quoteReplayManifest, error) {
	var m quoteReplayManifest
	path := filepath.Join(dir, quoteReplayManifestName)
	info, err := os.Lstat(path)
	if err != nil {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the run manifest %q is missing: %v", path, err))
	}
	if !info.Mode().IsRegular() || !quoteReplayOwnedByMe(info) || quoteReplayHardLinked(info) {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the run manifest %q is not a regular, singly linked file this user owns", path))
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
	if m.Owner != quoteReplayOwnerMarker {
		return m, refuseReplay("state_foreign", fmt.Sprintf("the run manifest owner %q is not this runner", m.Owner))
	}
	return m, nil
}

func compareQuoteReplayManifest(saved, want quoteReplayManifest) error {
	var diffs []string
	if saved.RunID != want.RunID {
		diffs = append(diffs, fmt.Sprintf("run id %q != %q", saved.RunID, want.RunID))
	}
	pairs := []struct{ name, a, b string }{
		{"protocol", saved.Fingerprints.Protocol, want.Fingerprints.Protocol},
		{"source", saved.Fingerprints.Source, want.Fingerprints.Source},
		{"data", saved.Fingerprints.Data, want.Fingerprints.Data},
		{"config", saved.Fingerprints.Config, want.Fingerprints.Config},
		{"policy", saved.Fingerprints.Policy, want.Fingerprints.Policy},
		{"model", saved.Fingerprints.Model, want.Fingerprints.Model},
		{"run", saved.Fingerprints.Run, want.Fingerprints.Run},
	}
	for _, p := range pairs {
		if p.a != p.b {
			diffs = append(diffs, fmt.Sprintf("%s fingerprint changed", p.name))
		}
	}
	if saved.OwnerStrategy != want.OwnerStrategy || saved.FirstDeadlineMs != want.FirstDeadlineMs || saved.LastDeadlineMs != want.LastDeadlineMs {
		diffs = append(diffs, "the owner or the deadline window changed")
	}
	if saved.UID != want.UID {
		diffs = append(diffs, "the run was created by another user")
	}
	if len(diffs) > 0 {
		return refuseReplay("resume_refused", diffs...)
	}
	return nil
}

type quoteReplayStoredRun struct {
	Manifest quoteReplayManifest
	CursorMs int64
	Refused  string
}

func readQuoteReplayRun(db *StateDB, want quoteReplayManifest) (quoteReplayStoredRun, error) {
	var out quoteReplayStoredRun
	ok, err := tableExists(db, "quote_replay_runs")
	if err != nil {
		return out, refuseReplay("state_foreign", err.Error())
	}
	if !ok {
		return out, refuseReplay("state_foreign", "the state file has no quote replay run record; the runner accepts only state it created and marked")
	}
	rows, err := db.db.Query(`SELECT run_id, owner, protocol_fingerprint, source_fingerprint, data_fingerprint, config_fingerprint, policy_fingerprint,
		model_fingerprint, run_fingerprint, owner_strategy_id, first_deadline_ms, last_deadline_ms, cursor_ms, refused FROM quote_replay_runs`)
	if err != nil {
		return out, refuseReplay("state_foreign", err.Error())
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		var got quoteReplayManifest
		var owner string
		if err := rows.Scan(&got.RunID, &owner, &got.Fingerprints.Protocol, &got.Fingerprints.Source, &got.Fingerprints.Data, &got.Fingerprints.Config,
			&got.Fingerprints.Policy, &got.Fingerprints.Model, &got.Fingerprints.Run, &got.OwnerStrategy, &got.FirstDeadlineMs, &got.LastDeadlineMs, &out.CursorMs, &out.Refused); err != nil {
			return out, refuseReplay("state_foreign", err.Error())
		}
		if owner != quoteReplayOwnerMarker {
			return out, refuseReplay("state_foreign", fmt.Sprintf("the quote replay run record owner %q is not this runner", owner))
		}
		got.UID = want.UID
		if err := compareQuoteReplayManifest(got, want); err != nil {
			return out, err
		}
		out.Manifest = got
	}
	if err := rows.Err(); err != nil {
		return out, refuseReplay("state_foreign", err.Error())
	}
	if n != 1 {
		return out, refuseReplay("state_foreign", fmt.Sprintf("the state file holds %d quote replay run records, not exactly one", n))
	}
	return out, nil
}

func verifyQuoteReplayDBReadOnly(path string, want quoteReplayManifest) (quoteReplayStoredRun, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return quoteReplayStoredRun{}, refuseReplay("state_foreign", fmt.Sprintf("the run state file %q is missing: %v", path, err))
	}
	if !info.Mode().IsRegular() || !quoteReplayOwnedByMe(info) || quoteReplayHardLinked(info) {
		return quoteReplayStoredRun{}, refuseReplay("state_foreign", fmt.Sprintf("the run state file %q is not a regular, singly linked file this user owns", path))
	}
	db, err := openStateDBReadOnly(path)
	if err != nil {
		return quoteReplayStoredRun{}, refuseReplay("state_foreign", fmt.Sprintf("read-only open of %q failed: %v", path, err))
	}
	defer db.Close()
	return readQuoteReplayRun(db, want)
}

func (sdb *StateDB) ensureQuoteReplaySchema(m quoteReplayManifest, storageID string, fresh bool) error {
	tx, err := sdb.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(quoteReplaySchemaDDL); err != nil {
		return fmt.Errorf("create quote replay tables: %w", err)
	}
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM quote_replay_runs`).Scan(&existing); err != nil {
		return err
	}
	if fresh {
		if existing != 0 {
			return refuseReplay("state_exists", "the new run state file already holds a quote replay run record")
		}
		if _, err := tx.Exec(`INSERT INTO quote_replay_runs (run_id, owner, protocol, protocol_fingerprint, source_fingerprint, data_fingerprint, config_fingerprint,
			policy_fingerprint, model_fingerprint, run_fingerprint, owner_strategy_id, storage_strategy_id, first_deadline_ms, last_deadline_ms, cursor_ms, created_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			m.RunID, quoteReplayOwnerMarker, quoteReplayProtocolName, m.Fingerprints.Protocol, m.Fingerprints.Source, m.Fingerprints.Data, m.Fingerprints.Config,
			m.Fingerprints.Policy, m.Fingerprints.Model, m.Fingerprints.Run, m.OwnerStrategy, storageID, m.FirstDeadlineMs, m.LastDeadlineMs, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("insert quote replay run record: %w", err)
		}
	} else if existing != 1 {
		return refuseReplay("state_foreign", "the resumed state file does not hold exactly one quote replay run record")
	}
	return tx.Commit()
}

type quoteReplayVenueEventRow struct {
	Seq      int64           `json:"seq"`
	Kind     string          `json:"kind"`
	ClientID string          `json:"client_order_id,omitempty"`
	EffectMs int64           `json:"effect_ms"`
	Event    json.RawMessage `json:"event"`
}

func (sdb *StateDB) insertQuoteReplayVenueEvent(runID string, seq int64, kind, cid string, effectMs int64, event any) error {
	blob, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = sdb.db.Exec(`INSERT INTO quote_replay_venue_events (seq, run_id, kind, client_order_id, effect_ms, event_json, digest) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		seq, runID, kind, cid, effectMs, string(blob), sha256Hex(blob))
	return err
}

func (sdb *StateDB) quoteReplayVenueEvents(runID string) ([]quoteReplayVenueEventRow, error) {
	rows, err := sdb.db.Query(`SELECT seq, kind, client_order_id, effect_ms, event_json, digest FROM quote_replay_venue_events WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []quoteReplayVenueEventRow
	for rows.Next() {
		var r quoteReplayVenueEventRow
		var blob, digest string
		if err := rows.Scan(&r.Seq, &r.Kind, &r.ClientID, &r.EffectMs, &blob, &digest); err != nil {
			return nil, err
		}
		if sha256Hex([]byte(blob)) != digest {
			return nil, refuseReplay("state_foreign", fmt.Sprintf("venue event %d does not match its digest", r.Seq))
		}
		r.Event = json.RawMessage(blob)
		out = append(out, r)
	}
	return out, rows.Err()
}

type quoteReplayResultRow struct {
	DeadlineMs int64           `json:"deadline_ms"`
	Attempt    int             `json:"attempt"`
	State      string          `json:"state"`
	Summary    json.RawMessage `json:"summary,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
}

func (sdb *StateDB) quoteReplayResults(runID string) ([]quoteReplayResultRow, error) {
	rows, err := sdb.db.Query(`SELECT deadline_ms, attempt, state, summary_json, result_json FROM quote_replay_deadline_results WHERE run_id = ? ORDER BY deadline_ms, attempt`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []quoteReplayResultRow
	for rows.Next() {
		var r quoteReplayResultRow
		var sum, res string
		if err := rows.Scan(&r.DeadlineMs, &r.Attempt, &r.State, &sum, &res); err != nil {
			return nil, err
		}
		if sum != "" {
			r.Summary = json.RawMessage(sum)
		}
		if res != "" {
			r.Result = json.RawMessage(res)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (sdb *StateDB) quoteReplayDecision(runID string, deadlineMs int64) (string, bool, error) {
	var blob string
	err := sdb.db.QueryRow(`SELECT record_json FROM quote_replay_decisions WHERE run_id = ? AND deadline_ms = ?`, runID, deadlineMs).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return blob, err == nil, err
}

func (sdb *StateDB) quoteReplayDecisions(runID string) ([]json.RawMessage, error) {
	rows, err := sdb.db.Query(`SELECT record_json, digest FROM quote_replay_decisions WHERE run_id = ? ORDER BY deadline_ms`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var blob, digest string
		if err := rows.Scan(&blob, &digest); err != nil {
			return nil, err
		}
		if sha256Hex([]byte(blob)) != digest {
			return nil, refuseReplay("state_foreign", "a stored decision record does not match its digest")
		}
		out = append(out, json.RawMessage(blob))
	}
	return out, rows.Err()
}

func (sdb *StateDB) quoteReplayCheckpoint(runID string, deadlineMs int64) (string, int64, bool, error) {
	var blob string
	var seq int64
	err := sdb.db.QueryRow(`SELECT checkpoint_json, venue_seq FROM quote_replay_checkpoints WHERE run_id = ? AND deadline_ms = ?`, runID, deadlineMs).Scan(&blob, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	return blob, seq, err == nil, err
}

func (sdb *StateDB) finishQuoteReplayResult(runID string, deadlineMs int64, attempt int, state string, result any) error {
	blob, err := json.Marshal(result)
	if err != nil {
		return err
	}
	r, err := sdb.db.Exec(`UPDATE quote_replay_deadline_results SET state = ?, result_json = ? WHERE run_id = ? AND deadline_ms = ? AND attempt = ? AND state = 'pending'`,
		state, string(blob), runID, deadlineMs, attempt)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return fmt.Errorf("the deadline %d attempt %d result is not pending", deadlineMs, attempt)
	}
	return nil
}

func (sdb *StateDB) markQuoteReplayRefused(runID, reason string) error {
	_, err := sdb.db.Exec(`UPDATE quote_replay_runs SET refused = ? WHERE run_id = ? AND refused = ''`, reason, runID)
	return err
}

func (sdb *StateDB) markQuoteReplayInterrupted(runID string, deadlineMs int64) (int, error) {
	r, err := sdb.db.Exec(`UPDATE quote_replay_deadline_results SET state = 'interrupted' WHERE run_id = ? AND deadline_ms = ? AND state = 'pending'`, runID, deadlineMs)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

func inspectQuoteReplayRows(db *StateDB, fi *storageFileInspection) ([]string, error) {
	var rejections []string
	ok, err := tableExists(db, "quote_replay_runs")
	if err != nil || !ok {
		return rejections, err
	}
	hasCursor, err := db.tableHasColumn("quote_replay_runs", "cursor_ms")
	if err != nil {
		return nil, err
	}
	query := `SELECT run_id, cursor_ms FROM quote_replay_runs ORDER BY run_id`
	if !hasCursor {
		query = `SELECT run_id, 0 FROM quote_replay_runs ORDER BY run_id`
	}
	rows, err := db.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("read quote replay runs in the %s state file: %w", fi.Role, err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		var id string
		var cursor int64
		if err := rows.Scan(&id, &cursor); err != nil {
			return nil, err
		}
		fi.QuoteReplayRuns = append(fi.QuoteReplayRuns, fmt.Sprintf("%s: cursor %s", id, formatClosedBarMs(cursor)))
		rejections = append(rejections, fmt.Sprintf("%s state file: it holds offline quote replay run %s; the scheduler never runs on replay state", fi.Role, id))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if n == 0 {
		rejections = append(rejections, fmt.Sprintf("%s state file: it holds an empty quote replay run table; the scheduler never runs on replay state", fi.Role))
	}
	return rejections, nil
}
