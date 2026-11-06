package main

import (
	"database/sql"
	"fmt"
	"time"
)

type positionEvidence struct {
	Trades         []Trade
	Diagnostics    []TradeDiagnosticsRow
	OpenBookRows   []positionBookRow
	Feedback       []tradeFeedbackRecord
	SourceRole     storageRole
	FeedbackStored bool
}

type positionBookRow struct {
	Symbol   string  `json:"symbol"`
	Quantity float64 `json:"quantity"`
	Side     string  `json:"side"`
}

type tradeFeedbackRecord struct {
	RowID               int64     `json:"row_id"`
	StrategyID          string    `json:"strategy_id"`
	PositionID          string    `json:"position_id"`
	Symbol              string    `json:"symbol"`
	Side                string    `json:"side"`
	Platform            string    `json:"platform"`
	InstrumentType      string    `json:"instrument_type"`
	Timeframe           string    `json:"timeframe"`
	PreferredEntryTime  int64     `json:"preferred_entry_time"`
	PreferredEntryPrice float64   `json:"preferred_entry_price"`
	PreferredExitTime   int64     `json:"preferred_exit_time"`
	PreferredExitPrice  float64   `json:"preferred_exit_price"`
	ActualOpenedAt      time.Time `json:"actual_opened_at"`
	ActualClosedAt      time.Time `json:"actual_closed_at"`
	ActualAvgEntryPrice float64   `json:"actual_avg_entry_price"`
	ActualAvgExitPrice  float64   `json:"actual_avg_exit_price"`
	ActualRealizedPnL   float64   `json:"actual_realized_pnl"`
	ActualRealizedBasis string    `json:"actual_realized_pnl_basis"`
	CloseReason         string    `json:"close_reason"`
	EntryATR            float64   `json:"entry_atr"`
	Note                string    `json:"note"`
	EvidenceVersion     int       `json:"evidence_version"`
	EvidenceJSON        string    `json:"-"`
	EvidenceSHA256      string    `json:"evidence_sha256"`
	CreatedAt           time.Time `json:"created_at"`
}

var errTradeFeedbackStale = fmt.Errorf("position evidence changed after validation")

func (sdb *StateDB) tableExistsTx(q interface {
	QueryRow(string, ...interface{}) *sql.Row
}, table string) (bool, error) {
	var n int
	if err := q.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
		return false, fmt.Errorf("check table %s: %w", table, err)
	}
	return n > 0, nil
}

func (sdb *StateDB) ReadPositionEvidence(procID, positionID string) (*positionEvidence, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.toStorageID(procID)
	if err != nil {
		return nil, err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin position evidence read: %w", err)
	}
	defer tx.Rollback()
	ev, err := sdb.readPositionEvidenceTx(tx, sid, positionID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("finish position evidence read: %w", err)
	}
	return ev, nil
}

func (sdb *StateDB) readPositionEvidenceTx(tx *sql.Tx, sid, positionID string) (*positionEvidence, error) {
	if positionID == "" {
		return nil, fmt.Errorf("position id is required")
	}
	ev := &positionEvidence{SourceRole: sdb.storageRoleOf()}

	rows, err := tx.Query("SELECT "+tradeHistorySelectColumns+" FROM trades WHERE strategy_id = ? AND position_id = ? ORDER BY timestamp ASC, rowid ASC", sid, positionID)
	if err != nil {
		return nil, fmt.Errorf("query position trades: %w", err)
	}
	for rows.Next() {
		t, err := sdb.scanTradeHistoryRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		ev.Trades = append(ev.Trades, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate position trades: %w", err)
	}
	rows.Close()

	diagRows, err := tx.Query(`SELECT rowid, strategy_id, position_id, symbol, side, timeframe, regime_at_open, close_reason,
			entry_price, exit_price, quantity, realized_pnl, entry_atr, stop_loss_atr_mult,
			opened_at, closed_at, mfe_price, mae_price, favorable_pct, adverse_pct, capture_ratio,
			metrics_status, llm_verdict
		FROM trade_diagnostics WHERE strategy_id = ? AND position_id = ? ORDER BY rowid ASC`, sid, positionID)
	if err != nil {
		return nil, fmt.Errorf("query position diagnostics: %w", err)
	}
	diags, err := sdb.scanTradeDiagnosticsRows(diagRows)
	diagRows.Close()
	if err != nil {
		return nil, err
	}
	ev.Diagnostics = diags

	bookRows, err := tx.Query(`SELECT symbol, quantity, side FROM positions WHERE strategy_id = ? AND position_id = ? ORDER BY symbol`, sid, positionID)
	if err != nil {
		return nil, fmt.Errorf("query position book rows: %w", err)
	}
	for bookRows.Next() {
		var b positionBookRow
		if err := bookRows.Scan(&b.Symbol, &b.Quantity, &b.Side); err != nil {
			bookRows.Close()
			return nil, fmt.Errorf("scan position book row: %w", err)
		}
		ev.OpenBookRows = append(ev.OpenBookRows, b)
	}
	if err := bookRows.Err(); err != nil {
		bookRows.Close()
		return nil, fmt.Errorf("iterate position book rows: %w", err)
	}
	bookRows.Close()

	stored, err := sdb.tableExistsTx(tx, "trade_feedback")
	if err != nil {
		return nil, err
	}
	ev.FeedbackStored = stored
	if !stored {
		return ev, nil
	}
	fbRows, err := tx.Query(`SELECT rowid, strategy_id, position_id, symbol, side, platform, instrument_type, timeframe,
			preferred_entry_time, preferred_entry_price, preferred_exit_time, preferred_exit_price,
			actual_opened_at, actual_closed_at, actual_avg_entry_price, actual_avg_exit_price,
			actual_realized_pnl, actual_realized_pnl_basis, close_reason, entry_atr, note,
			evidence_version, evidence_json, evidence_sha256, created_at
		FROM trade_feedback WHERE strategy_id = ? AND position_id = ? ORDER BY rowid DESC`, sid, positionID)
	if err != nil {
		return nil, fmt.Errorf("query trade feedback: %w", err)
	}
	for fbRows.Next() {
		var r tradeFeedbackRecord
		var opened, closed, created string
		if err := fbRows.Scan(&r.RowID, &r.StrategyID, &r.PositionID, &r.Symbol, &r.Side, &r.Platform, &r.InstrumentType, &r.Timeframe,
			&r.PreferredEntryTime, &r.PreferredEntryPrice, &r.PreferredExitTime, &r.PreferredExitPrice,
			&opened, &closed, &r.ActualAvgEntryPrice, &r.ActualAvgExitPrice,
			&r.ActualRealizedPnL, &r.ActualRealizedBasis, &r.CloseReason, &r.EntryATR, &r.Note,
			&r.EvidenceVersion, &r.EvidenceJSON, &r.EvidenceSHA256, &created); err != nil {
			fbRows.Close()
			return nil, fmt.Errorf("scan trade feedback: %w", err)
		}
		r.StrategyID = sdb.fromStorageID(r.StrategyID)
		r.ActualOpenedAt = parseTime(opened)
		r.ActualClosedAt = parseTime(closed)
		r.CreatedAt = parseTime(created)
		ev.Feedback = append(ev.Feedback, r)
	}
	if err := fbRows.Err(); err != nil {
		fbRows.Close()
		return nil, fmt.Errorf("iterate trade feedback: %w", err)
	}
	fbRows.Close()
	return ev, nil
}

func (sdb *StateDB) InsertTradeFeedbackChecked(procID string, rec *tradeFeedbackRecord, verify func(*positionEvidence) error) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if rec == nil || verify == nil {
		return fmt.Errorf("trade feedback insert needs a record and a verifier")
	}
	sid, err := sdb.toStorageID(procID)
	if err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin trade feedback insert: %w", err)
	}
	defer tx.Rollback()
	ev, err := sdb.readPositionEvidenceTx(tx, sid, rec.PositionID)
	if err != nil {
		return err
	}
	if !ev.FeedbackStored {
		return fmt.Errorf("the %s state file has no trade_feedback table", sdb.storageRoleOf())
	}
	if err := verify(ev); err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT INTO trade_feedback
			(strategy_id, position_id, symbol, side, platform, instrument_type, timeframe,
			 preferred_entry_time, preferred_entry_price, preferred_exit_time, preferred_exit_price,
			 actual_opened_at, actual_closed_at, actual_avg_entry_price, actual_avg_exit_price,
			 actual_realized_pnl, actual_realized_pnl_basis, close_reason, entry_atr, note,
			 evidence_version, evidence_json, evidence_sha256, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sid, rec.PositionID, rec.Symbol, rec.Side, rec.Platform, rec.InstrumentType, rec.Timeframe,
		rec.PreferredEntryTime, rec.PreferredEntryPrice, rec.PreferredExitTime, rec.PreferredExitPrice,
		formatTime(rec.ActualOpenedAt), formatTime(rec.ActualClosedAt), rec.ActualAvgEntryPrice, rec.ActualAvgExitPrice,
		rec.ActualRealizedPnL, rec.ActualRealizedBasis, rec.CloseReason, rec.EntryATR, rec.Note,
		rec.EvidenceVersion, rec.EvidenceJSON, rec.EvidenceSHA256, formatTime(rec.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert trade feedback for %s: %w", procID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("trade feedback rowid for %s: %w", procID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit trade feedback for %s: %w", procID, err)
	}
	rec.RowID = id
	rec.StrategyID = procID
	return nil
}

func (st *StateStore) ReadPositionEvidence(strategyID, positionID string) (*positionEvidence, error) {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return nil, err
	}
	return db.ReadPositionEvidence(strategyID, positionID)
}

func (st *StateStore) InsertTradeFeedbackChecked(strategyID string, rec *tradeFeedbackRecord, verify func(*positionEvidence) error) error {
	db, err := st.dbForStrategy(strategyID)
	if err != nil {
		return err
	}
	return db.InsertTradeFeedbackChecked(strategyID, rec, verify)
}
