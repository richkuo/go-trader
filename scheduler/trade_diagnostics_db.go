package main

import (
	"database/sql"
	"fmt"
)

// StateDB persistence for #1147 trade diagnostics. Insert runs eagerly on the
// close path (same cost class as InsertTrade); the metrics UPDATE runs from
// the async worker, keyed by rowid.

// InsertTradeDiagnostics persists the identity/outcome part of a diagnostics
// row and stamps row.RowID for the follow-up metrics update.
func (sdb *StateDB) InsertTradeDiagnostics(row *TradeDiagnosticsRow) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if row == nil {
		return fmt.Errorf("nil diagnostics row")
	}
	res, err := sdb.db.Exec(`INSERT INTO trade_diagnostics
			(strategy_id, position_id, symbol, side, timeframe, regime_at_open, close_reason,
			 entry_price, exit_price, quantity, realized_pnl, entry_atr, stop_loss_atr_mult,
			 opened_at, closed_at, metrics_status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.StrategyID, row.PositionID, row.Symbol, row.Side, row.Timeframe, row.RegimeAtOpen, row.CloseReason,
		row.EntryPrice, row.ExitPrice, row.Quantity, row.RealizedPnL, row.EntryATR, nullableFloat64(row.StopLossATRMult),
		formatTime(row.OpenedAt), formatTime(row.ClosedAt), row.MetricsStatus)
	if err != nil {
		return fmt.Errorf("insert trade diagnostics for %s: %w", row.StrategyID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("trade diagnostics rowid for %s: %w", row.StrategyID, err)
	}
	row.RowID = id
	return nil
}

// UpdateTradeDiagnosticsMetrics fills in the derived quality metrics (or just
// the terminal metrics_status when metrics is nil — failure paths leave the
// quality columns NULL).
func (sdb *StateDB) UpdateTradeDiagnosticsMetrics(rowID int64, timeframe string, m *tradeQualityMetrics, status string) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if m == nil {
		_, err := sdb.db.Exec(
			`UPDATE trade_diagnostics SET timeframe = ?, metrics_status = ? WHERE rowid = ?`,
			timeframe, status, rowID)
		if err != nil {
			return fmt.Errorf("update trade diagnostics status row %d: %w", rowID, err)
		}
		return nil
	}
	_, err := sdb.db.Exec(
		`UPDATE trade_diagnostics
			SET timeframe = ?, mfe_price = ?, mae_price = ?, favorable_pct = ?, adverse_pct = ?,
			    capture_ratio = ?, metrics_status = ?
			WHERE rowid = ?`,
		timeframe, m.MFEPrice, m.MAEPrice, m.FavorablePct, m.AdversePct,
		nullableFloat64(m.CaptureRatio), status, rowID)
	if err != nil {
		return fmt.Errorf("update trade diagnostics metrics row %d: %w", rowID, err)
	}
	return nil
}

// TradeDiagnosticsRows loads diagnostics rows (all strategies when strategyID
// is empty), oldest first.
func (sdb *StateDB) TradeDiagnosticsRows(strategyID string) ([]TradeDiagnosticsRow, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	query := `SELECT rowid, strategy_id, position_id, symbol, side, timeframe, regime_at_open, close_reason,
			entry_price, exit_price, quantity, realized_pnl, entry_atr, stop_loss_atr_mult,
			opened_at, closed_at, mfe_price, mae_price, favorable_pct, adverse_pct, capture_ratio,
			metrics_status, llm_verdict
		FROM trade_diagnostics`
	var args []interface{}
	if strategyID != "" {
		query += ` WHERE strategy_id = ?`
		args = append(args, strategyID)
	}
	query += ` ORDER BY closed_at ASC, rowid ASC`
	rows, err := sdb.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query trade diagnostics: %w", err)
	}
	defer rows.Close()
	var out []TradeDiagnosticsRow
	for rows.Next() {
		var r TradeDiagnosticsRow
		var slMult, mfe, mae, fav, adv, capture sql.NullFloat64
		var openedAt, closedAt string
		var verdict sql.NullString
		if err := rows.Scan(&r.RowID, &r.StrategyID, &r.PositionID, &r.Symbol, &r.Side, &r.Timeframe,
			&r.RegimeAtOpen, &r.CloseReason, &r.EntryPrice, &r.ExitPrice, &r.Quantity, &r.RealizedPnL,
			&r.EntryATR, &slMult, &openedAt, &closedAt, &mfe, &mae, &fav, &adv, &capture,
			&r.MetricsStatus, &verdict); err != nil {
			return nil, fmt.Errorf("scan trade diagnostics: %w", err)
		}
		r.OpenedAt = parseTime(openedAt)
		r.ClosedAt = parseTime(closedAt)
		r.StopLossATRMult = nullFloatPtr(slMult)
		r.MFEPrice = nullFloatPtr(mfe)
		r.MAEPrice = nullFloatPtr(mae)
		r.FavorablePct = nullFloatPtr(fav)
		r.AdversePct = nullFloatPtr(adv)
		r.CaptureRatio = nullFloatPtr(capture)
		if verdict.Valid {
			v := verdict.String
			r.LLMVerdict = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NetPnLByPosition sums the convention-aware NET realized PnL of close legs
// per (strategy_id, position_id), so multi-leg exits (tiered-TP scale-outs,
// partial closes) aggregate correctly in the report. Rows with an empty
// position_id can't be attributed and are excluded — the report falls back to
// the diagnostics row's own final-leg PnL for those.
func (sdb *StateDB) NetPnLByPosition(strategyID string) (map[string]map[string]float64, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	query := `SELECT strategy_id, position_id, SUM(` + tradeNetPnLSQL + `)
		FROM trades WHERE is_close = 1 AND position_id != ''`
	var args []interface{}
	if strategyID != "" {
		query += ` AND strategy_id = ?`
		args = append(args, strategyID)
	}
	query += ` GROUP BY strategy_id, position_id`
	rows, err := sdb.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query net pnl by position: %w", err)
	}
	defer rows.Close()
	out := make(map[string]map[string]float64)
	for rows.Next() {
		var sid, pid string
		var net float64
		if err := rows.Scan(&sid, &pid, &net); err != nil {
			return nil, fmt.Errorf("scan net pnl by position: %w", err)
		}
		if out[sid] == nil {
			out[sid] = make(map[string]float64)
		}
		out[sid][pid] = net
	}
	return out, rows.Err()
}

func nullFloatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}
