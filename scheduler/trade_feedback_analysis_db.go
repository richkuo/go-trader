package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	tradeFeedbackAnalysisAccepted = "accepted"
	tradeFeedbackAnalysisOK       = "ok"
	tradeFeedbackAnalysisInvalid  = "invalid"
	tradeFeedbackAnalysisFailed   = "failed"
)

var errTradeFeedbackAnalysisUnknownRequest = errors.New("no accepted analysis request with this id in the owning state file")

type tradeFeedbackAnalysisEvent struct {
	RowID             int64     `json:"row_id"`
	RequestID         string    `json:"request_id"`
	EventStatus       string    `json:"event_status"`
	StrategyID        string    `json:"strategy_id"`
	Partition         string    `json:"partition"`
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	ModelVerification string    `json:"model_verification"`
	Effort            string    `json:"effort"`
	TimeoutS          int       `json:"timeout_s"`
	BundleVersion     int       `json:"bundle_version"`
	BundleSHA256      string    `json:"bundle_sha256"`
	BundleJSON        string    `json:"-"`
	FeedbackRowIDs    []int64   `json:"feedback_rowids"`
	ResultJSON        string    `json:"-"`
	ValidationJSON    string    `json:"-"`
	Error             string    `json:"error"`
	UsageJSON         string    `json:"-"`
	CreatedAt         time.Time `json:"created_at"`
}

type tradeFeedbackAnalysisTerminal struct {
	Status            string
	Model             string
	ModelVerification string
	ResultJSON        string
	ValidationJSON    string
	Error             string
	UsageJSON         string
	CreatedAt         time.Time
}

type tradeFeedbackAnalysisRequest struct {
	Accepted tradeFeedbackAnalysisEvent  `json:"accepted"`
	Terminal *tradeFeedbackAnalysisEvent `json:"terminal,omitempty"`
}

const tradeFeedbackAnalysisColumns = `rowid, request_id, event_status, strategy_id, partition, provider, model, model_verification,
	effort, timeout_s, bundle_version, bundle_sha256, bundle_json, feedback_rowids, result_json, validation_json,
	error, usage_json, created_at`

func (sdb *StateDB) scanTradeFeedbackAnalysisEvent(row interface{ Scan(...interface{}) error }) (tradeFeedbackAnalysisEvent, string, error) {
	var ev tradeFeedbackAnalysisEvent
	var rowIDs, created string
	if err := row.Scan(&ev.RowID, &ev.RequestID, &ev.EventStatus, &ev.StrategyID, &ev.Partition, &ev.Provider, &ev.Model,
		&ev.ModelVerification, &ev.Effort, &ev.TimeoutS, &ev.BundleVersion, &ev.BundleSHA256, &ev.BundleJSON, &rowIDs,
		&ev.ResultJSON, &ev.ValidationJSON, &ev.Error, &ev.UsageJSON, &created); err != nil {
		return ev, "", err
	}
	if err := json.Unmarshal([]byte(rowIDs), &ev.FeedbackRowIDs); err != nil {
		return ev, "", fmt.Errorf("decode feedback_rowids of analysis %s: %w", ev.RequestID, err)
	}
	stored := ev.StrategyID
	ev.StrategyID = sdb.fromStorageID(stored)
	ev.CreatedAt = parseTime(created)
	return ev, stored, nil
}

func (sdb *StateDB) requireAnalysisTable(q interface {
	QueryRow(string, ...interface{}) *sql.Row
}) error {
	ok, err := sdb.tableExistsTx(q, "trade_feedback_analyses")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the %s state file has no trade_feedback_analyses table", sdb.storageRoleOf())
	}
	return nil
}

// ReadLatestTradeFeedback returns, for one strategy, the highest feedback row
// of each position, newest first, limited to limit candidates. The limit is
// applied before any integrity check so an invalid newest mark never pulls an
// older mark or another position into the window.
func (sdb *StateDB) ReadLatestTradeFeedback(procID string, limit int) ([]tradeFeedbackRecord, []string, error) {
	if sdb == nil || sdb.db == nil {
		return nil, nil, fmt.Errorf("state db unavailable")
	}
	if limit <= 0 {
		return nil, nil, fmt.Errorf("candidate limit must be positive")
	}
	sid, err := sdb.toStorageID(procID)
	if err != nil {
		return nil, nil, err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return nil, nil, fmt.Errorf("begin trade feedback read: %w", err)
	}
	defer tx.Rollback()
	ok, err := sdb.tableExistsTx(tx, "trade_feedback")
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("the %s state file has no trade_feedback table", sdb.storageRoleOf())
	}
	rows, err := tx.Query(`SELECT f.rowid, f.strategy_id, f.position_id, f.symbol, f.side, f.platform, f.instrument_type, f.timeframe,
			f.preferred_entry_time, f.preferred_entry_price, f.preferred_exit_time, f.preferred_exit_price,
			f.actual_opened_at, f.actual_closed_at, f.actual_avg_entry_price, f.actual_avg_exit_price,
			f.actual_realized_pnl, f.actual_realized_pnl_basis, f.close_reason, f.entry_atr, f.note,
			f.evidence_version, f.evidence_json, f.evidence_sha256, f.created_at
		FROM trade_feedback f
		WHERE f.strategy_id = ?
		  AND f.rowid = (SELECT MAX(g.rowid) FROM trade_feedback g WHERE g.strategy_id = f.strategy_id AND g.position_id = f.position_id)
		ORDER BY f.rowid DESC
		LIMIT ?`, sid, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("query latest trade feedback: %w", err)
	}
	var out []tradeFeedbackRecord
	var stored []string
	for rows.Next() {
		var r tradeFeedbackRecord
		var opened, closed, created string
		if err := rows.Scan(&r.RowID, &r.StrategyID, &r.PositionID, &r.Symbol, &r.Side, &r.Platform, &r.InstrumentType, &r.Timeframe,
			&r.PreferredEntryTime, &r.PreferredEntryPrice, &r.PreferredExitTime, &r.PreferredExitPrice,
			&opened, &closed, &r.ActualAvgEntryPrice, &r.ActualAvgExitPrice,
			&r.ActualRealizedPnL, &r.ActualRealizedBasis, &r.CloseReason, &r.EntryATR, &r.Note,
			&r.EvidenceVersion, &r.EvidenceJSON, &r.EvidenceSHA256, &created); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan latest trade feedback: %w", err)
		}
		stored = append(stored, r.StrategyID)
		r.StrategyID = sdb.fromStorageID(r.StrategyID)
		r.ActualOpenedAt = parseTime(opened)
		r.ActualClosedAt = parseTime(closed)
		r.CreatedAt = parseTime(created)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("iterate latest trade feedback: %w", err)
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("finish trade feedback read: %w", err)
	}
	return out, stored, nil
}

func (sdb *StateDB) InsertTradeFeedbackAnalysisAccepted(procID string, ev *tradeFeedbackAnalysisEvent) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if ev == nil || ev.RequestID == "" || ev.BundleJSON == "" || ev.BundleSHA256 == "" {
		return fmt.Errorf("an accepted analysis needs a request id and a frozen bundle")
	}
	sid, err := sdb.toStorageID(procID)
	if err != nil {
		return err
	}
	rowIDs, err := json.Marshal(nonNilInt64s(ev.FeedbackRowIDs))
	if err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin analysis admission: %w", err)
	}
	defer tx.Rollback()
	if err := sdb.requireAnalysisTable(tx); err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT INTO trade_feedback_analyses
			(request_id, event_status, strategy_id, partition, provider, model, model_verification, effort, timeout_s,
			 bundle_version, bundle_sha256, bundle_json, feedback_rowids, result_json, validation_json, error, usage_json, created_at)
			VALUES (?, 'accepted', ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, '', '', '', '', ?)`,
		ev.RequestID, sid, ev.Partition, ev.Provider, ev.Model, ev.Effort, ev.TimeoutS,
		ev.BundleVersion, ev.BundleSHA256, ev.BundleJSON, string(rowIDs), formatTime(ev.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert accepted analysis %s: %w", ev.RequestID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit accepted analysis %s: %w", ev.RequestID, err)
	}
	ev.RowID = id
	ev.EventStatus = tradeFeedbackAnalysisAccepted
	ev.StrategyID = procID
	return nil
}

func validTradeFeedbackTerminalStatus(s string) bool {
	return s == tradeFeedbackAnalysisOK || s == tradeFeedbackAnalysisInvalid || s == tradeFeedbackAnalysisFailed
}

// AppendTradeFeedbackAnalysisTerminal writes the one terminal event of an
// accepted request in this file. It copies the owner, partition, provider and
// input identity from the accepted row, so a terminal event can never move a
// request to another strategy or file. A repeat with identical content is a
// no-op; a conflicting repeat is an error.
func (sdb *StateDB) AppendTradeFeedbackAnalysisTerminal(requestID string, term tradeFeedbackAnalysisTerminal) (bool, error) {
	if sdb == nil || sdb.db == nil {
		return false, fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return false, fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if !validTradeFeedbackTerminalStatus(term.Status) {
		return false, fmt.Errorf("terminal status %q is not ok, invalid or failed", term.Status)
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin analysis terminal write: %w", err)
	}
	defer tx.Rollback()
	if err := sdb.requireAnalysisTable(tx); err != nil {
		return false, err
	}
	var acceptedRow int64
	err = tx.QueryRow(`SELECT rowid FROM trade_feedback_analyses WHERE request_id = ? AND event_status = 'accepted'`, requestID).Scan(&acceptedRow)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: %s", errTradeFeedbackAnalysisUnknownRequest, requestID)
	}
	if err != nil {
		return false, fmt.Errorf("read accepted analysis %s: %w", requestID, err)
	}
	var prev tradeFeedbackAnalysisTerminal
	err = tx.QueryRow(`SELECT event_status, model, model_verification, result_json, validation_json, error, usage_json
		FROM trade_feedback_analyses WHERE request_id = ? AND event_status <> 'accepted'`, requestID).
		Scan(&prev.Status, &prev.Model, &prev.ModelVerification, &prev.ResultJSON, &prev.ValidationJSON, &prev.Error, &prev.UsageJSON)
	switch {
	case err == nil:
		term.CreatedAt = time.Time{}
		if prev == term {
			return false, nil
		}
		return false, fmt.Errorf("analysis %s already has a different %s outcome", requestID, prev.Status)
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("read analysis outcome %s: %w", requestID, err)
	}
	created := term.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	if _, err := tx.Exec(`INSERT INTO trade_feedback_analyses
			(request_id, event_status, strategy_id, partition, provider, model, model_verification, effort, timeout_s,
			 bundle_version, bundle_sha256, bundle_json, feedback_rowids, result_json, validation_json, error, usage_json, created_at)
		SELECT request_id, ?, strategy_id, partition, provider, ?, ?, effort, timeout_s,
			 bundle_version, bundle_sha256, '', feedback_rowids, ?, ?, ?, ?, ?
		FROM trade_feedback_analyses WHERE rowid = ?`,
		term.Status, term.Model, term.ModelVerification, term.ResultJSON, term.ValidationJSON, term.Error, term.UsageJSON,
		formatTime(created), acceptedRow); err != nil {
		return false, fmt.Errorf("insert analysis outcome %s: %w", requestID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit analysis outcome %s: %w", requestID, err)
	}
	return true, nil
}

func (sdb *StateDB) readAnalysisRequestsTx(tx *sql.Tx, where string, args ...interface{}) ([]tradeFeedbackAnalysisRequest, []string, error) {
	rows, err := tx.Query(`SELECT `+tradeFeedbackAnalysisColumns+` FROM trade_feedback_analyses WHERE event_status = 'accepted' AND `+where, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("query analysis requests: %w", err)
	}
	var out []tradeFeedbackAnalysisRequest
	var stored []string
	for rows.Next() {
		ev, sid, err := sdb.scanTradeFeedbackAnalysisEvent(rows)
		if err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan analysis request: %w", err)
		}
		out = append(out, tradeFeedbackAnalysisRequest{Accepted: ev})
		stored = append(stored, sid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("iterate analysis requests: %w", err)
	}
	rows.Close()
	for i := range out {
		row := tx.QueryRow(`SELECT `+tradeFeedbackAnalysisColumns+` FROM trade_feedback_analyses WHERE request_id = ? AND event_status <> 'accepted'`, out[i].Accepted.RequestID)
		ev, _, err := sdb.scanTradeFeedbackAnalysisEvent(row)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read analysis outcome %s: %w", out[i].Accepted.RequestID, err)
		}
		out[i].Terminal = &ev
	}
	return out, stored, nil
}

func (sdb *StateDB) readAnalysisRequests(where string, args ...interface{}) ([]tradeFeedbackAnalysisRequest, []string, error) {
	if sdb == nil || sdb.db == nil {
		return nil, nil, fmt.Errorf("state db unavailable")
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return nil, nil, fmt.Errorf("begin analysis read: %w", err)
	}
	defer tx.Rollback()
	if err := sdb.requireAnalysisTable(tx); err != nil {
		return nil, nil, err
	}
	out, stored, err := sdb.readAnalysisRequestsTx(tx, where, args...)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("finish analysis read: %w", err)
	}
	return out, stored, nil
}

func (sdb *StateDB) ReadTradeFeedbackAnalyses(procID string, limit int) ([]tradeFeedbackAnalysisRequest, error) {
	sid, err := sdb.toStorageID(procID)
	if err != nil {
		return nil, err
	}
	out, _, err := sdb.readAnalysisRequests(`strategy_id = ? ORDER BY rowid DESC LIMIT ?`, sid, limit)
	return out, err
}

func (sdb *StateDB) ReadTradeFeedbackAnalysis(procID, requestID string) (*tradeFeedbackAnalysisRequest, error) {
	sid, err := sdb.toStorageID(procID)
	if err != nil {
		return nil, err
	}
	out, _, err := sdb.readAnalysisRequests(`strategy_id = ? AND request_id = ?`, sid, requestID)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

func (sdb *StateDB) unresolvedTradeFeedbackAnalyses() ([]tradeFeedbackAnalysisRequest, error) {
	out, _, err := sdb.readAnalysisRequests(`NOT EXISTS (SELECT 1 FROM trade_feedback_analyses t
		WHERE t.request_id = trade_feedback_analyses.request_id AND t.event_status <> 'accepted') ORDER BY rowid ASC`)
	return out, err
}

func nonNilInt64s(in []int64) []int64 {
	if in == nil {
		return []int64{}
	}
	return in
}

func (st *StateStore) ReadLatestTradeFeedback(strategyID string, limit int) (*StateDB, []tradeFeedbackRecord, []string, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, nil, nil, err
	}
	rows, stored, err := db.ReadLatestTradeFeedback(strategyID, limit)
	return db, rows, stored, err
}

func (st *StateStore) InsertTradeFeedbackAnalysisAccepted(strategyID string, ev *tradeFeedbackAnalysisEvent) (*StateDB, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	if err := db.InsertTradeFeedbackAnalysisAccepted(strategyID, ev); err != nil {
		return nil, err
	}
	return db, nil
}

func (st *StateStore) ReadTradeFeedbackAnalyses(strategyID string, limit int) ([]tradeFeedbackAnalysisRequest, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	return db.ReadTradeFeedbackAnalyses(strategyID, limit)
}

func (st *StateStore) ReadTradeFeedbackAnalysis(strategyID, requestID string) (*tradeFeedbackAnalysisRequest, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	return db.ReadTradeFeedbackAnalysis(strategyID, requestID)
}

// RecoverTradeFeedbackAnalyses closes every accepted request that has no
// outcome, file by file in store order, with one failed event. It never calls
// a provider. A request whose stored partition this file does not own is left
// untouched and reported, so recovery cannot reassign an owner.
func (st *StateStore) RecoverTradeFeedbackAnalyses(reason string, now time.Time) (int, error) {
	if st == nil {
		return 0, fmt.Errorf("state store unavailable")
	}
	recovered := 0
	var errs []string
	for _, role := range st.order {
		db := st.files[role]
		if db == nil {
			continue
		}
		pending, err := db.unresolvedTradeFeedbackAnalyses()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", role, err))
			continue
		}
		owned := st.layout.partitionsForRole(role)
		for _, req := range pending {
			p, perr := parseRiskPartition(req.Accepted.Partition)
			if perr != nil || p == unassignedPartition || !partitionInList(p, owned) {
				errs = append(errs, fmt.Sprintf("%s: analysis %s names partition %q that this file does not own; left unresolved", role, req.Accepted.RequestID, req.Accepted.Partition))
				continue
			}
			if _, err := db.AppendTradeFeedbackAnalysisTerminal(req.Accepted.RequestID, tradeFeedbackAnalysisTerminal{
				Status: tradeFeedbackAnalysisFailed, Error: reason, CreatedAt: now,
			}); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", role, err))
				continue
			}
			recovered++
		}
	}
	if len(errs) > 0 {
		return recovered, errors.New(strings.Join(errs, "; "))
	}
	return recovered, nil
}
