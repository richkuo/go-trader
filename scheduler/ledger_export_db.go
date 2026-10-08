package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const ledgerExportPageSize = 500

var ledgerTradeMandatoryColumns = []string{
	"strategy_id", "timestamp", "symbol", "side", "quantity", "price", "value",
	"exchange_fee", "is_close", "realized_pnl", "pnl_gross",
}

var ledgerTradeOptionalColumns = []string{
	"trade_type", "details", "position_id", "exchange_order_id", "fee_source", "regime", "manual",
	"entry_atr", "stop_loss_atr_mult", "stop_loss_trigger_px", "stop_loss_oid", "tp_oids_json", "tp_tiers_json", "cost_model_version",
}

var ledgerDiagnosticColumns = []string{"strategy_id", "position_id", "symbol", "close_reason", "exit_price", "quantity", "closed_at"}

var ledgerWalletColumns = []string{"platform", "account", "time_ms", "kind", "amount_usd", "dedup_id"}

type ledgerTradeRow struct {
	RowID  int64
	Values map[string]any
}

type ledgerDiagnosticRow struct {
	RowID         int64
	StorageID     string
	PositionID    string
	Symbol        string
	CloseReason   string
	ExitPrice     float64
	Quantity      float64
	ClosedAt      time.Time
	ClosedAtValid bool
}

type ledgerFileRead struct {
	Role               storageRole
	StorageID          string
	Columns            map[string]bool
	Trades             []ledgerTradeRow
	DiagnosticsPresent bool
	Diagnostics        []ledgerDiagnosticRow
}

type ledgerWalletRow struct {
	RowID     int64
	Platform  string
	Account   string
	Kind      string
	DedupID   string
	TimeMS    int64
	AmountUSD float64
}

type ledgerWalletRead struct {
	Role         storageRole
	TablePresent bool
	Rows         []ledgerWalletRow
}

type ledgerTableColumn struct {
	Name string
	Type string
	PK   int
}

type ledgerQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func openStateDBSnapshot(path string) (*StateDB, error) {
	if isInMemoryDBPath(path) {
		return nil, fmt.Errorf("snapshot open needs a real file, got %q", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot %q: %w", path, err)
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&immutable=1&cache=private"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open snapshot %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("snapshot %q: pragma query_only: %w", path, err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		db.Close()
		return nil, fmt.Errorf("snapshot %q: read journal mode: %w", path, err)
	}
	if strings.ToLower(mode) != "delete" {
		db.Close()
		return nil, fmt.Errorf("snapshot %q reports journal mode %q, want delete", path, mode)
	}
	return &StateDB{db: db, path: path, role: storageRolePrimary, readOnly: true}, nil
}

func openLedgerSnapshotStore(layout storageLayout, ident storageIdentityMap) (*StateStore, error) {
	return openStateStoreWith(layout, ident, openStateDBSnapshot)
}

func ledgerTableColumns(q ledgerQueryer, table string) (map[string]ledgerTableColumn, bool, error) {
	var name string
	err := q.QueryRow("SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("probe table %s: %w", table, err)
	}
	rows, err := q.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, false, fmt.Errorf("read %s schema: %w", table, err)
	}
	defer rows.Close()
	out := make(map[string]ledgerTableColumn)
	for rows.Next() {
		var cid, notnull, pk int
		var colName, ctype string
		var dflt any
		if err := rows.Scan(&cid, &colName, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, false, fmt.Errorf("scan %s schema: %w", table, err)
		}
		out[colName] = ledgerTableColumn{Name: colName, Type: strings.ToUpper(strings.TrimSpace(ctype)), PK: pk}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate %s schema: %w", table, err)
	}
	return out, true, nil
}

func ledgerHasRowIDKey(cols map[string]ledgerTableColumn) bool {
	c, ok := cols["rowid"]
	if !ok || c.PK != 1 || c.Type != "INTEGER" {
		return false
	}
	for name, other := range cols {
		if name != "rowid" && other.PK != 0 {
			return false
		}
	}
	return true
}

func (sdb *StateDB) readLedgerSnapshot(procID string, pageSize int) (*ledgerFileRead, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	if pageSize <= 0 {
		return nil, fmt.Errorf("page size must be positive")
	}
	storageID, err := sdb.toStorageID(procID)
	if err != nil {
		return nil, err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin ledger read: %w", err)
	}
	defer tx.Rollback()

	out := &ledgerFileRead{Role: sdb.storageRoleOf(), StorageID: storageID, Columns: map[string]bool{}}

	stratCols, ok, err := ledgerTableColumns(tx, "strategies")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("the %s state file has no strategies table", out.Role)
	}
	if _, ok := stratCols["id"]; !ok {
		return nil, fmt.Errorf("the %s state file strategies table has no id column", out.Role)
	}
	var stored int
	if err := tx.QueryRow("SELECT COUNT(*) FROM strategies WHERE id = ?", storageID).Scan(&stored); err != nil {
		return nil, fmt.Errorf("look up stored strategy %q: %w", storageID, err)
	}
	if stored != 1 {
		return nil, fmt.Errorf("the %s state file stores %d strategy rows for %q (process %q); want exactly 1", out.Role, stored, storageID, procID)
	}

	tradeCols, ok, err := ledgerTableColumns(tx, "trades")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("the %s state file has no trades table", out.Role)
	}
	if !ledgerHasRowIDKey(tradeCols) {
		return nil, fmt.Errorf("the %s state file trades table has no explicit INTEGER PRIMARY KEY rowid column; row identity is not stable", out.Role)
	}
	var missing []string
	for _, col := range ledgerTradeMandatoryColumns {
		if _, ok := tradeCols[col]; !ok {
			missing = append(missing, col)
			continue
		}
		out.Columns[col] = true
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the %s state file trades table lacks mandatory accounting column(s) %s; it needs a schema migration that a read-only export never runs", out.Role, strings.Join(missing, ", "))
	}
	selectCols := append([]string{}, ledgerTradeMandatoryColumns...)
	for _, col := range ledgerTradeOptionalColumns {
		if _, ok := tradeCols[col]; ok {
			out.Columns[col] = true
			selectCols = append(selectCols, col)
		}
	}
	base := "SELECT rowid, " + strings.Join(selectCols, ", ") + " FROM trades WHERE strategy_id = ?"
	var after *int64
	for {
		query, args := ledgerKeysetQuery(base, []any{storageID}, after, pageSize)
		n, lastID, err := scanLedgerTradePage(tx, query, args, selectCols, out)
		if err != nil {
			return nil, err
		}
		if n < pageSize {
			break
		}
		after = &lastID
	}

	diagCols, ok, err := ledgerTableColumns(tx, "trade_diagnostics")
	if err != nil {
		return nil, err
	}
	if ok && ledgerHasRowIDKey(diagCols) {
		complete := true
		for _, col := range ledgerDiagnosticColumns {
			if _, has := diagCols[col]; !has {
				complete = false
			}
		}
		if complete {
			out.DiagnosticsPresent = true
			if err := readLedgerDiagnostics(tx, storageID, pageSize, out); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("end ledger read: %w", err)
	}
	return out, nil
}

func ledgerKeysetQuery(base string, args []any, after *int64, pageSize int) (string, []any) {
	query := base
	out := append([]any{}, args...)
	if after != nil {
		query += " AND rowid > ?"
		out = append(out, *after)
	}
	query += " ORDER BY rowid LIMIT ?"
	out = append(out, pageSize)
	return query, out
}

func scanLedgerTradePage(tx *sql.Tx, query string, args []any, cols []string, out *ledgerFileRead) (int, int64, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return 0, 0, fmt.Errorf("query %s trades: %w", out.Role, err)
	}
	defer rows.Close()
	n := 0
	var lastID int64
	for rows.Next() {
		dest := make([]any, len(cols)+1)
		var rowID any
		dest[0] = &rowID
		vals := make([]any, len(cols))
		for i := range cols {
			dest[i+1] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return 0, 0, fmt.Errorf("scan %s trade: %w", out.Role, err)
		}
		id, ok := rowID.(int64)
		if !ok {
			return 0, 0, fmt.Errorf("%s trade row identifier holds %T", out.Role, rowID)
		}
		row := ledgerTradeRow{RowID: id, Values: make(map[string]any, len(cols))}
		for i, col := range cols {
			row.Values[col] = vals[i]
		}
		out.Trades = append(out.Trades, row)
		lastID = id
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate %s trades: %w", out.Role, err)
	}
	return n, lastID, nil
}

func readLedgerDiagnostics(tx *sql.Tx, storageID string, pageSize int, out *ledgerFileRead) error {
	var after *int64
	for {
		query, args := ledgerKeysetQuery(`SELECT rowid, strategy_id, position_id, symbol, close_reason, exit_price, quantity, closed_at
			FROM trade_diagnostics WHERE strategy_id = ?`, []any{storageID}, after, pageSize)
		rows, err := tx.Query(query, args...)
		if err != nil {
			return fmt.Errorf("query %s trade diagnostics: %w", out.Role, err)
		}
		n := 0
		for rows.Next() {
			var d ledgerDiagnosticRow
			var pos, reason, closedAt sql.NullString
			var exitPx, qty sql.NullFloat64
			if err := rows.Scan(&d.RowID, &d.StorageID, &pos, &d.Symbol, &reason, &exitPx, &qty, &closedAt); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s trade diagnostics: %w", out.Role, err)
			}
			d.PositionID = pos.String
			d.CloseReason = reason.String
			d.ExitPrice = exitPx.Float64
			d.Quantity = qty.Float64
			if closedAt.Valid {
				if t, err := parseLedgerTimestamp(closedAt.String); err == nil {
					d.ClosedAt = t
					d.ClosedAtValid = true
				}
			}
			if !exitPx.Valid || !qty.Valid {
				d.ClosedAtValid = false
			}
			out.Diagnostics = append(out.Diagnostics, d)
			id := d.RowID
			after = &id
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("iterate %s trade diagnostics: %w", out.Role, err)
		}
		if n < pageSize {
			return nil
		}
	}
}

func (sdb *StateDB) readLedgerWalletOrphans(pageSize int) (*ledgerWalletRead, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin wallet read: %w", err)
	}
	defer tx.Rollback()
	out := &ledgerWalletRead{Role: sdb.storageRoleOf()}
	cols, ok, err := ledgerTableColumns(tx, "wallet_transfers")
	if err != nil {
		return nil, err
	}
	if !ok {
		return out, tx.Commit()
	}
	if !ledgerHasRowIDKey(cols) {
		return nil, fmt.Errorf("the %s state file wallet_transfers table has no explicit INTEGER PRIMARY KEY rowid column", out.Role)
	}
	for _, col := range ledgerWalletColumns {
		if _, has := cols[col]; !has {
			return out, tx.Commit()
		}
	}
	out.TablePresent = true
	var after *int64
	for {
		query, args := ledgerKeysetQuery(`SELECT rowid, platform, account, time_ms, kind, amount_usd, dedup_id FROM wallet_transfers
			WHERE platform = 'hyperliquid' AND kind = 'funding_orphan'`, nil, after, pageSize)
		rows, err := tx.Query(query, args...)
		if err != nil {
			return nil, fmt.Errorf("query wallet transfers: %w", err)
		}
		n := 0
		for rows.Next() {
			var r ledgerWalletRow
			if err := rows.Scan(&r.RowID, &r.Platform, &r.Account, &r.TimeMS, &r.Kind, &r.AmountUSD, &r.DedupID); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan wallet transfer: %w", err)
			}
			out.Rows = append(out.Rows, r)
			id := r.RowID
			after = &id
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("iterate wallet transfers: %w", err)
		}
		if n < pageSize {
			break
		}
	}
	return out, tx.Commit()
}
