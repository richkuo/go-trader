package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schemaDDL = `
CREATE TABLE IF NOT EXISTS app_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    cycle_count INTEGER NOT NULL DEFAULT 0,
    last_cycle TEXT NOT NULL DEFAULT '',
    last_leaderboard_post_date TEXT NOT NULL DEFAULT '',
    last_leaderboard_summaries TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS strategies (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    platform TEXT NOT NULL DEFAULT '',
    cash REAL NOT NULL DEFAULT 0,
    initial_capital REAL NOT NULL DEFAULT 0,
    risk_peak_value REAL NOT NULL DEFAULT 0,
    risk_max_drawdown_pct REAL NOT NULL DEFAULT 0,
    risk_current_drawdown_pct REAL NOT NULL DEFAULT 0,
    risk_daily_pnl REAL NOT NULL DEFAULT 0,
    risk_daily_pnl_date TEXT NOT NULL DEFAULT '',
    risk_consecutive_losses INTEGER NOT NULL DEFAULT 0,
    risk_circuit_breaker INTEGER NOT NULL DEFAULT 0,
    risk_circuit_breaker_until TEXT NOT NULL DEFAULT '',
    risk_total_trades INTEGER NOT NULL DEFAULT 0,
    risk_winning_trades INTEGER NOT NULL DEFAULT 0,
    risk_losing_trades INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS positions (
    strategy_id TEXT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    symbol TEXT NOT NULL,
    quantity REAL NOT NULL,
    avg_cost REAL NOT NULL,
    side TEXT NOT NULL,
    multiplier REAL NOT NULL DEFAULT 0,
    owner_strategy_id TEXT NOT NULL DEFAULT '',
    opened_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (strategy_id, symbol)
);

CREATE TABLE IF NOT EXISTS closed_positions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    strategy_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    quantity REAL NOT NULL,
    avg_cost REAL NOT NULL,
    side TEXT NOT NULL,
    multiplier REAL NOT NULL DEFAULT 0,
    opened_at TEXT NOT NULL DEFAULT '',
    closed_at TEXT NOT NULL DEFAULT '',
    close_price REAL NOT NULL DEFAULT 0,
    realized_pnl REAL NOT NULL DEFAULT 0,
    close_reason TEXT NOT NULL DEFAULT '',
    duration_seconds INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_closed_positions_strategy ON closed_positions(strategy_id);
CREATE INDEX IF NOT EXISTS idx_closed_positions_symbol ON closed_positions(symbol);
CREATE INDEX IF NOT EXISTS idx_closed_positions_closed_at ON closed_positions(closed_at DESC);

CREATE TABLE IF NOT EXISTS closed_option_positions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    strategy_id TEXT NOT NULL,
    position_id TEXT NOT NULL,
    underlying TEXT NOT NULL,
    option_type TEXT NOT NULL,
    strike REAL NOT NULL DEFAULT 0,
    expiry TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    quantity REAL NOT NULL DEFAULT 0,
    entry_premium_usd REAL NOT NULL DEFAULT 0,
    close_price_usd REAL NOT NULL DEFAULT 0,
    realized_pnl REAL NOT NULL DEFAULT 0,
    opened_at TEXT NOT NULL DEFAULT '',
    closed_at TEXT NOT NULL DEFAULT '',
    close_reason TEXT NOT NULL DEFAULT '',
    duration_seconds INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_closed_opt_strategy ON closed_option_positions(strategy_id);
CREATE INDEX IF NOT EXISTS idx_closed_opt_underlying ON closed_option_positions(underlying);
CREATE INDEX IF NOT EXISTS idx_closed_opt_closed_at ON closed_option_positions(closed_at DESC);

CREATE TABLE IF NOT EXISTS option_positions (
    strategy_id TEXT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    id TEXT NOT NULL,
    underlying TEXT NOT NULL,
    option_type TEXT NOT NULL,
    strike REAL NOT NULL,
    expiry TEXT NOT NULL,
    dte REAL NOT NULL DEFAULT 0,
    action TEXT NOT NULL,
    quantity REAL NOT NULL,
    entry_premium REAL NOT NULL DEFAULT 0,
    entry_premium_usd REAL NOT NULL DEFAULT 0,
    current_value_usd REAL NOT NULL DEFAULT 0,
    delta REAL NOT NULL DEFAULT 0,
    gamma REAL NOT NULL DEFAULT 0,
    theta REAL NOT NULL DEFAULT 0,
    vega REAL NOT NULL DEFAULT 0,
    opened_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (strategy_id, id)
);

CREATE TABLE IF NOT EXISTS trades (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    strategy_id TEXT NOT NULL,
    timestamp TEXT NOT NULL,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL,
    quantity REAL NOT NULL,
    price REAL NOT NULL,
    value REAL NOT NULL,
    trade_type TEXT NOT NULL DEFAULT '',
    details TEXT NOT NULL DEFAULT '',
    exchange_order_id TEXT NOT NULL DEFAULT '',
    exchange_fee REAL NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_trades_strategy ON trades(strategy_id);
CREATE INDEX IF NOT EXISTS idx_trades_symbol ON trades(symbol);
CREATE INDEX IF NOT EXISTS idx_trades_timestamp ON trades(timestamp DESC);

CREATE TABLE IF NOT EXISTS portfolio_risk (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    peak_value REAL NOT NULL DEFAULT 0,
    current_drawdown_pct REAL NOT NULL DEFAULT 0,
    current_margin_drawdown_pct REAL NOT NULL DEFAULT 0,
    kill_switch_active INTEGER NOT NULL DEFAULT 0,
    kill_switch_at TEXT NOT NULL DEFAULT '',
    warning_sent INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS kill_switch_events (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp TEXT NOT NULL,
    type TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    drawdown_pct REAL NOT NULL DEFAULT 0,
    portfolio_value REAL NOT NULL DEFAULT 0,
    peak_value REAL NOT NULL DEFAULT 0,
    details TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS correlation_snapshot (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    snapshot_json TEXT NOT NULL DEFAULT '{}'
);
`

// StateDB wraps a SQLite database for persistent state storage.
type StateDB struct {
	db *sql.DB
}

// OpenStateDB opens (or creates) the SQLite database at the given path.
func OpenStateDB(path string) (*StateDB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}

	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	sdb := &StateDB{db: db}
	if err := sdb.migrateSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return sdb, nil
}

// migrateSchema adds columns that may be missing from older databases.
func (sdb *StateDB) migrateSchema() error {
	// Add exchange_order_id and exchange_fee to trades table (added in #219).
	migrations := []string{
		"ALTER TABLE trades ADD COLUMN exchange_order_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE trades ADD COLUMN exchange_fee REAL NOT NULL DEFAULT 0",
		// Position lifecycle tracking (#288).
		"ALTER TABLE positions ADD COLUMN opened_at TEXT NOT NULL DEFAULT ''",
		// Portfolio margin drawdown + kill-switch source tracking (#296 review).
		"ALTER TABLE portfolio_risk ADD COLUMN current_margin_drawdown_pct REAL NOT NULL DEFAULT 0",
		"ALTER TABLE kill_switch_events ADD COLUMN source TEXT NOT NULL DEFAULT ''",
		// Per-leaderboard-summary last-post timestamps stored as JSON (#308).
		"ALTER TABLE app_state ADD COLUMN last_leaderboard_summaries TEXT NOT NULL DEFAULT ''",
	}
	for _, ddl := range migrations {
		if _, err := sdb.db.Exec(ddl); err != nil {
			// "duplicate column name" means the column already exists — skip.
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return err
		}
	}
	return nil
}

// Close closes the database connection.
func (sdb *StateDB) Close() error {
	return sdb.db.Close()
}

// InsertTrade persists a single trade row immediately (#289). This is invoked
// via the tradeRecorder hook the moment a trade is appended to TradeHistory,
// so trades survive mid-cycle crashes even if SaveState never runs.
func (sdb *StateDB) InsertTrade(strategyID string, trade Trade) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	_, err := sdb.db.Exec(`INSERT INTO trades
		(strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strategyID, formatTime(trade.Timestamp), trade.Symbol, trade.Side,
		trade.Quantity, trade.Price, trade.Value, trade.TradeType, trade.Details,
		trade.ExchangeOrderID, trade.ExchangeFee)
	if err != nil {
		return fmt.Errorf("insert trade for %s: %w", strategyID, err)
	}
	return nil
}

// SetInitialCapital is the ONLY sanctioned way to change a strategy's
// initial_capital baseline (#343). All other write paths go through SaveState,
// which preserves the existing baseline. Callers are expected to be an
// explicit user command (CLI flag, admin script, migration), not normal
// runtime state persistence.
func (sdb *StateDB) SetInitialCapital(strategyID string, value float64) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if value <= 0 {
		return fmt.Errorf("initial_capital must be > 0, got %g", value)
	}
	res, err := sdb.db.Exec("UPDATE strategies SET initial_capital = ? WHERE id = ?", value, strategyID)
	if err != nil {
		return fmt.Errorf("update initial_capital for %s: %w", strategyID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no strategy row for id=%q", strategyID)
	}
	fmt.Printf("[state] initial_capital override for %s set to $%.2f (#343)\n", strategyID, value)
	return nil
}

// SaveState writes the full AppState to SQLite within a single transaction.
func (sdb *StateDB) SaveState(state *AppState) error {
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Upsert app_state singleton.
	lbSummariesJSON := ""
	if len(state.LastLeaderboardSummaries) > 0 {
		raw, err := json.Marshal(state.LastLeaderboardSummaries)
		if err != nil {
			return fmt.Errorf("marshal last_leaderboard_summaries: %w", err)
		}
		lbSummariesJSON = string(raw)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO app_state (id, cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries)
		VALUES (1, ?, ?, ?, ?)`,
		state.CycleCount,
		formatTime(state.LastCycle),
		state.LastLeaderboardPostDate,
		lbSummariesJSON,
	); err != nil {
		return fmt.Errorf("upsert app_state: %w", err)
	}

	// 2. Snapshot existing initial_capital per strategy so the save path can
	// never silently rewrite a PnL baseline (#343). Captured before DELETE so
	// the CASCADE doesn't erase the prior values first.
	existingInitCaps := make(map[string]float64)
	existingRows, err := tx.Query("SELECT id, initial_capital FROM strategies")
	if err != nil {
		return fmt.Errorf("read existing initial_capital: %w", err)
	}
	for existingRows.Next() {
		var id string
		var cap float64
		if err := existingRows.Scan(&id, &cap); err != nil {
			existingRows.Close()
			return fmt.Errorf("scan existing initial_capital: %w", err)
		}
		existingInitCaps[id] = cap
	}
	existingRows.Close()

	// 3. Delete all strategies (CASCADE deletes positions + option_positions).
	if _, err := tx.Exec("DELETE FROM strategies"); err != nil {
		return fmt.Errorf("delete strategies: %w", err)
	}

	// 4. Insert strategies with flattened risk state.
	stmtStrat, err := tx.Prepare(`INSERT OR REPLACE INTO strategies (id, type, platform, cash, initial_capital,
		risk_peak_value, risk_max_drawdown_pct, risk_current_drawdown_pct,
		risk_daily_pnl, risk_daily_pnl_date, risk_consecutive_losses,
		risk_circuit_breaker, risk_circuit_breaker_until,
		risk_total_trades, risk_winning_trades, risk_losing_trades)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare strategy insert: %w", err)
	}
	defer stmtStrat.Close()

	stmtPos, err := tx.Prepare(`INSERT INTO positions (strategy_id, symbol, quantity, avg_cost, side, multiplier, owner_strategy_id, opened_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare position insert: %w", err)
	}
	defer stmtPos.Close()

	stmtOpt, err := tx.Prepare(`INSERT INTO option_positions (strategy_id, id, underlying, option_type, strike, expiry, dte,
		action, quantity, entry_premium, entry_premium_usd, current_value_usd,
		delta, gamma, theta, vega, opened_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare option_position insert: %w", err)
	}
	defer stmtOpt.Close()

	for _, s := range state.Strategies {
		// Immutable baseline guard (#343): if a prior initial_capital exists
		// and the incoming value disagrees, keep the prior value so PnL
		// history stays comparable across restarts, state restores, and
		// position closes. A baseline change requires an explicit override
		// via StateDB.SetInitialCapital.
		if prev, ok := existingInitCaps[s.ID]; ok && prev > 0 && s.InitialCapital != prev {
			fmt.Printf("[WARN] state: blocking initial_capital change for %s ($%.2f → $%.2f); baseline preserved (#343)\n",
				s.ID, prev, s.InitialCapital)
			s.InitialCapital = prev
		}

		cbInt := 0
		if s.RiskState.CircuitBreaker {
			cbInt = 1
		}
		if _, err := stmtStrat.Exec(
			s.ID, s.Type, s.Platform, s.Cash, s.InitialCapital,
			s.RiskState.PeakValue, s.RiskState.MaxDrawdownPct, s.RiskState.CurrentDrawdownPct,
			s.RiskState.DailyPnL, s.RiskState.DailyPnLDate, s.RiskState.ConsecutiveLosses,
			cbInt, formatTime(s.RiskState.CircuitBreakerUntil),
			s.RiskState.TotalTrades, s.RiskState.WinningTrades, s.RiskState.LosingTrades,
		); err != nil {
			return fmt.Errorf("insert strategy %s: %w", s.ID, err)
		}

		for _, pos := range s.Positions {
			if _, err := stmtPos.Exec(s.ID, pos.Symbol, pos.Quantity, pos.AvgCost, pos.Side, pos.Multiplier, pos.OwnerStrategyID, formatTime(pos.OpenedAt)); err != nil {
				return fmt.Errorf("insert position %s/%s: %w", s.ID, pos.Symbol, err)
			}
		}

		for key, opt := range s.OptionPositions {
			if _, err := stmtOpt.Exec(
				s.ID, key, opt.Underlying, opt.OptionType, opt.Strike, opt.Expiry, opt.DTE,
				opt.Action, opt.Quantity, opt.EntryPremium, opt.EntryPremiumUSD, opt.CurrentValueUSD,
				opt.Greeks.Delta, opt.Greeks.Gamma, opt.Greeks.Theta, opt.Greeks.Vega,
				formatTime(opt.OpenedAt),
			); err != nil {
				return fmt.Errorf("insert option_position %s/%s: %w", s.ID, key, err)
			}
		}
	}

	// 5. Append-only trades: insert any TradeHistory rows that have not yet been
	//    persisted (t.persisted == false). LoadState and successful RecordTrade
	//    both flip the flag to true, so SaveState only flushes the backlog —
	//    including any rows whose eager InsertTrade earlier in the cycle
	//    failed, even if later-timestamped rows were persisted successfully
	//    (fixes the MAX(timestamp) dedup gap that would silently drop
	//    out-of-order retries).
	stmtTrade, err := tx.Prepare(`INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare trade insert: %w", err)
	}
	defer stmtTrade.Close()

	// Track which rows were flushed in this tx so we can mark them persisted
	// only after Commit succeeds (avoids marking true on a rolled-back tx).
	type trackedFlush struct {
		strat *StrategyState
		index int
	}
	var flushed []trackedFlush

	for _, s := range state.Strategies {
		for i := range s.TradeHistory {
			if s.TradeHistory[i].persisted {
				continue
			}
			t := s.TradeHistory[i]
			if _, err := stmtTrade.Exec(s.ID, formatTime(t.Timestamp), t.Symbol, t.Side, t.Quantity, t.Price, t.Value, t.TradeType, t.Details, t.ExchangeOrderID, t.ExchangeFee); err != nil {
				return fmt.Errorf("insert trade for %s: %w", s.ID, err)
			}
			flushed = append(flushed, trackedFlush{strat: s, index: i})
		}
	}

	// 4b. Append buffered ClosedPosition records (#288). Skip the prepare when
	// no strategy has any buffered closes this cycle — the typical case.
	hasClosed := false
	for _, s := range state.Strategies {
		if len(s.ClosedPositions) > 0 {
			hasClosed = true
			break
		}
	}
	if hasClosed {
		stmtClosed, err := tx.Prepare(`INSERT INTO closed_positions
			(strategy_id, symbol, quantity, avg_cost, side, multiplier,
			 opened_at, closed_at, close_price, realized_pnl, close_reason, duration_seconds)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare closed_position insert: %w", err)
		}
		defer stmtClosed.Close()
		for _, s := range state.Strategies {
			for _, cp := range s.ClosedPositions {
				if _, err := stmtClosed.Exec(
					cp.StrategyID, cp.Symbol, cp.Quantity, cp.AvgCost, cp.Side, cp.Multiplier,
					formatTime(cp.OpenedAt), formatTime(cp.ClosedAt),
					cp.ClosePrice, cp.RealizedPnL, cp.CloseReason, cp.DurationSeconds,
				); err != nil {
					return fmt.Errorf("insert closed_position %s/%s: %w", cp.StrategyID, cp.Symbol, err)
				}
			}
		}
	}

	// 4c. Append buffered ClosedOptionPosition records (#288).
	hasClosedOpt := false
	for _, s := range state.Strategies {
		if len(s.ClosedOptionPositions) > 0 {
			hasClosedOpt = true
			break
		}
	}
	if hasClosedOpt {
		stmtClosedOpt, err := tx.Prepare(`INSERT INTO closed_option_positions
			(strategy_id, position_id, underlying, option_type, strike, expiry,
			 action, quantity, entry_premium_usd, close_price_usd, realized_pnl,
			 opened_at, closed_at, close_reason, duration_seconds)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare closed_option_position insert: %w", err)
		}
		defer stmtClosedOpt.Close()
		for _, s := range state.Strategies {
			for _, cop := range s.ClosedOptionPositions {
				if _, err := stmtClosedOpt.Exec(
					cop.StrategyID, cop.PositionID, cop.Underlying, cop.OptionType,
					cop.Strike, cop.Expiry, cop.Action, cop.Quantity,
					cop.EntryPremiumUSD, cop.ClosePriceUSD, cop.RealizedPnL,
					formatTime(cop.OpenedAt), formatTime(cop.ClosedAt),
					cop.CloseReason, cop.DurationSeconds,
				); err != nil {
					return fmt.Errorf("insert closed_option_position %s/%s: %w", cop.StrategyID, cop.PositionID, err)
				}
			}
		}
	}

	// 6. Upsert portfolio_risk singleton.
	ksActive := 0
	if state.PortfolioRisk.KillSwitchActive {
		ksActive = 1
	}
	warnSent := 0
	if state.PortfolioRisk.WarningSent {
		warnSent = 1
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO portfolio_risk (id, peak_value, current_drawdown_pct, current_margin_drawdown_pct, kill_switch_active, kill_switch_at, warning_sent)
		VALUES (1, ?, ?, ?, ?, ?, ?)`,
		state.PortfolioRisk.PeakValue, state.PortfolioRisk.CurrentDrawdownPct, state.PortfolioRisk.CurrentMarginDrawdownPct,
		ksActive, formatTime(state.PortfolioRisk.KillSwitchAt), warnSent,
	); err != nil {
		return fmt.Errorf("upsert portfolio_risk: %w", err)
	}

	// 7. Kill switch events: replace all (capped at maxKillSwitchEvents).
	if _, err := tx.Exec("DELETE FROM kill_switch_events"); err != nil {
		return fmt.Errorf("delete kill_switch_events: %w", err)
	}
	if len(state.PortfolioRisk.Events) > 0 {
		stmtEvt, err := tx.Prepare(`INSERT INTO kill_switch_events (timestamp, type, source, drawdown_pct, portfolio_value, peak_value, details)
			VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare kill_switch_event insert: %w", err)
		}
		defer stmtEvt.Close()
		for _, evt := range state.PortfolioRisk.Events {
			if _, err := stmtEvt.Exec(formatTime(evt.Timestamp), evt.Type, evt.Source, evt.DrawdownPct, evt.PortfolioValue, evt.PeakValue, evt.Details); err != nil {
				return fmt.Errorf("insert kill_switch_event: %w", err)
			}
		}
	}

	// 8. Upsert correlation_snapshot singleton as JSON.
	snapJSON := "{}"
	if state.CorrelationSnapshot != nil {
		data, err := json.Marshal(state.CorrelationSnapshot)
		if err != nil {
			return fmt.Errorf("marshal correlation_snapshot: %w", err)
		}
		snapJSON = string(data)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO correlation_snapshot (id, snapshot_json) VALUES (1, ?)`, snapJSON); err != nil {
		return fmt.Errorf("upsert correlation_snapshot: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	// Clear buffered ClosedPositions / ClosedOptionPositions only after a
	// successful commit so that a mid-transaction failure does not silently
	// lose history entries. Note: if SaveState fails repeatedly (e.g. disk
	// full) these buffers grow unbounded until a successful commit drains
	// them — acceptable given the cycle cadence, but worth knowing.
	for _, s := range state.Strategies {
		s.ClosedPositions = nil
		s.ClosedOptionPositions = nil
	}
	// Mark flushed trades as persisted only after the tx has committed —
	// otherwise a rollback would leave the flag claiming rows are in DB when
	// they aren't, and the next SaveState would silently skip them.
	for _, f := range flushed {
		f.strat.TradeHistory[f.index].persisted = true
	}
	return nil
}

// QueryClosedPositions returns closed-position history ordered by closed_at desc,
// optionally filtered by strategy/symbol/time bounds. Used by status endpoints
// and leaderboard analytics (#288).
//
// Time filters rely on RFC3339Nano being lexicographically comparable (which it
// is — UTC 4-digit year, zero-padded components, fixed-width nanoseconds), so
// string comparison against formatTime(t) is equivalent to a chronological
// compare. Changing formatTime to a non-lexicographic format would silently
// break the since/until bounds here.
func (sdb *StateDB) QueryClosedPositions(strategyID, symbol string, since, until time.Time, limit, offset int) ([]ClosedPosition, int, error) {
	var where []string
	var args []interface{}
	if strategyID != "" {
		where = append(where, "strategy_id = ?")
		args = append(args, strategyID)
	}
	if symbol != "" {
		where = append(where, "symbol = ?")
		args = append(args, symbol)
	}
	if !since.IsZero() {
		where = append(where, "closed_at >= ?")
		args = append(args, formatTime(since))
	}
	if !until.IsZero() {
		where = append(where, "closed_at <= ?")
		args = append(args, formatTime(until))
	}
	// whereClause is composed from a fixed allowlist of fragments above — no
	// user-controlled SQL is ever concatenated, values flow through ? placeholders.
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := sdb.db.QueryRow("SELECT COUNT(*) FROM closed_positions "+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count closed_positions: %w", err)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	query := fmt.Sprintf(`SELECT strategy_id, symbol, quantity, avg_cost, side, multiplier,
		opened_at, closed_at, close_price, realized_pnl, close_reason, duration_seconds
		FROM closed_positions %s ORDER BY closed_at DESC LIMIT ? OFFSET ?`, whereClause)
	queryArgs := append(args, limit, offset)
	rows, err := sdb.db.Query(query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query closed_positions: %w", err)
	}
	defer rows.Close()

	var out []ClosedPosition
	for rows.Next() {
		var cp ClosedPosition
		var openedStr, closedStr string
		if err := rows.Scan(&cp.StrategyID, &cp.Symbol, &cp.Quantity, &cp.AvgCost, &cp.Side, &cp.Multiplier,
			&openedStr, &closedStr, &cp.ClosePrice, &cp.RealizedPnL, &cp.CloseReason, &cp.DurationSeconds); err != nil {
			return nil, 0, fmt.Errorf("scan closed_position: %w", err)
		}
		cp.OpenedAt = parseTime(openedStr)
		cp.ClosedAt = parseTime(closedStr)
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate closed_positions: %w", err)
	}
	if out == nil {
		out = []ClosedPosition{}
	}
	return out, total, nil
}

// QueryClosedOptionPositions returns closed option-position history ordered by
// closed_at desc, optionally filtered by strategy/underlying/time bounds (#288).
func (sdb *StateDB) QueryClosedOptionPositions(strategyID, underlying string, since, until time.Time, limit, offset int) ([]ClosedOptionPosition, int, error) {
	var where []string
	var args []interface{}
	if strategyID != "" {
		where = append(where, "strategy_id = ?")
		args = append(args, strategyID)
	}
	if underlying != "" {
		where = append(where, "underlying = ?")
		args = append(args, underlying)
	}
	if !since.IsZero() {
		where = append(where, "closed_at >= ?")
		args = append(args, formatTime(since))
	}
	if !until.IsZero() {
		where = append(where, "closed_at <= ?")
		args = append(args, formatTime(until))
	}
	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := sdb.db.QueryRow("SELECT COUNT(*) FROM closed_option_positions "+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count closed_option_positions: %w", err)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	query := fmt.Sprintf(`SELECT strategy_id, position_id, underlying, option_type, strike, expiry,
		action, quantity, entry_premium_usd, close_price_usd, realized_pnl,
		opened_at, closed_at, close_reason, duration_seconds
		FROM closed_option_positions %s ORDER BY closed_at DESC LIMIT ? OFFSET ?`, whereClause)
	queryArgs := append(args, limit, offset)
	rows, err := sdb.db.Query(query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query closed_option_positions: %w", err)
	}
	defer rows.Close()

	var out []ClosedOptionPosition
	for rows.Next() {
		var cop ClosedOptionPosition
		var openedStr, closedStr string
		if err := rows.Scan(&cop.StrategyID, &cop.PositionID, &cop.Underlying, &cop.OptionType,
			&cop.Strike, &cop.Expiry, &cop.Action, &cop.Quantity,
			&cop.EntryPremiumUSD, &cop.ClosePriceUSD, &cop.RealizedPnL,
			&openedStr, &closedStr, &cop.CloseReason, &cop.DurationSeconds); err != nil {
			return nil, 0, fmt.Errorf("scan closed_option_position: %w", err)
		}
		cop.OpenedAt = parseTime(openedStr)
		cop.ClosedAt = parseTime(closedStr)
		out = append(out, cop)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate closed_option_positions: %w", err)
	}
	if out == nil {
		out = []ClosedOptionPosition{}
	}
	return out, total, nil
}

// LoadState reads the full AppState from SQLite.
// Returns (nil, nil) if the database has no data (fresh DB).
func (sdb *StateDB) LoadState() (*AppState, error) {
	// 1. Load app_state singleton.
	var cycleCount int
	var lastCycleStr, lastLeaderboardDate, lastLBSummariesJSON string
	err := sdb.db.QueryRow("SELECT cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries FROM app_state WHERE id = 1").
		Scan(&cycleCount, &lastCycleStr, &lastLeaderboardDate, &lastLBSummariesJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load app_state: %w", err)
	}

	lbSummaries := make(map[string]time.Time)
	if lastLBSummariesJSON != "" {
		if err := json.Unmarshal([]byte(lastLBSummariesJSON), &lbSummaries); err != nil {
			return nil, fmt.Errorf("parse last_leaderboard_summaries: %w", err)
		}
	}

	state := &AppState{
		CycleCount:               cycleCount,
		LastCycle:                parseTime(lastCycleStr),
		LastLeaderboardPostDate:  lastLeaderboardDate,
		LastLeaderboardSummaries: lbSummaries,
		Strategies:               make(map[string]*StrategyState),
	}

	// 2. Load strategies.
	rows, err := sdb.db.Query(`SELECT id, type, platform, cash, initial_capital,
		risk_peak_value, risk_max_drawdown_pct, risk_current_drawdown_pct,
		risk_daily_pnl, risk_daily_pnl_date, risk_consecutive_losses,
		risk_circuit_breaker, risk_circuit_breaker_until,
		risk_total_trades, risk_winning_trades, risk_losing_trades
		FROM strategies`)
	if err != nil {
		return nil, fmt.Errorf("load strategies: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var s StrategyState
		var cbInt int
		var cbUntilStr string
		if err := rows.Scan(
			&s.ID, &s.Type, &s.Platform, &s.Cash, &s.InitialCapital,
			&s.RiskState.PeakValue, &s.RiskState.MaxDrawdownPct, &s.RiskState.CurrentDrawdownPct,
			&s.RiskState.DailyPnL, &s.RiskState.DailyPnLDate, &s.RiskState.ConsecutiveLosses,
			&cbInt, &cbUntilStr,
			&s.RiskState.TotalTrades, &s.RiskState.WinningTrades, &s.RiskState.LosingTrades,
		); err != nil {
			return nil, fmt.Errorf("scan strategy: %w", err)
		}
		s.RiskState.CircuitBreaker = cbInt != 0
		s.RiskState.CircuitBreakerUntil = parseTime(cbUntilStr)
		s.Positions = make(map[string]*Position)
		s.OptionPositions = make(map[string]*OptionPosition)
		s.TradeHistory = []Trade{}
		state.Strategies[s.ID] = &s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate strategies: %w", err)
	}

	// 3. Load positions for each strategy.
	posRows, err := sdb.db.Query("SELECT strategy_id, symbol, quantity, avg_cost, side, multiplier, owner_strategy_id, opened_at FROM positions")
	if err != nil {
		return nil, fmt.Errorf("load positions: %w", err)
	}
	defer posRows.Close()
	for posRows.Next() {
		var stratID string
		var pos Position
		var openedAtStr string
		if err := posRows.Scan(&stratID, &pos.Symbol, &pos.Quantity, &pos.AvgCost, &pos.Side, &pos.Multiplier, &pos.OwnerStrategyID, &openedAtStr); err != nil {
			return nil, fmt.Errorf("scan position: %w", err)
		}
		pos.OpenedAt = parseTime(openedAtStr)
		if s, ok := state.Strategies[stratID]; ok {
			s.Positions[pos.Symbol] = &pos
		}
	}
	if err := posRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate positions: %w", err)
	}

	// 4. Load option positions for each strategy.
	optRows, err := sdb.db.Query(`SELECT strategy_id, id, underlying, option_type, strike, expiry, dte,
		action, quantity, entry_premium, entry_premium_usd, current_value_usd,
		delta, gamma, theta, vega, opened_at FROM option_positions`)
	if err != nil {
		return nil, fmt.Errorf("load option_positions: %w", err)
	}
	defer optRows.Close()
	for optRows.Next() {
		var stratID string
		var opt OptionPosition
		var openedAtStr string
		if err := optRows.Scan(
			&stratID, &opt.ID, &opt.Underlying, &opt.OptionType, &opt.Strike, &opt.Expiry, &opt.DTE,
			&opt.Action, &opt.Quantity, &opt.EntryPremium, &opt.EntryPremiumUSD, &opt.CurrentValueUSD,
			&opt.Greeks.Delta, &opt.Greeks.Gamma, &opt.Greeks.Theta, &opt.Greeks.Vega,
			&openedAtStr,
		); err != nil {
			return nil, fmt.Errorf("scan option_position: %w", err)
		}
		opt.OpenedAt = parseTime(openedAtStr)
		if s, ok := state.Strategies[stratID]; ok {
			s.OptionPositions[opt.ID] = &opt
		}
	}
	if err := optRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate option_positions: %w", err)
	}

	// 5. Load most recent 1000 trades per strategy (full history stays in SQLite).
	for id, s := range state.Strategies {
		tradeRows, err := sdb.db.Query(`SELECT timestamp, strategy_id, symbol, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee
			FROM trades WHERE strategy_id = ? ORDER BY timestamp ASC`, id)
		if err != nil {
			return nil, fmt.Errorf("load trades for %s: %w", id, err)
		}
		var allTrades []Trade
		for tradeRows.Next() {
			var t Trade
			var tsStr string
			if err := tradeRows.Scan(&tsStr, &t.StrategyID, &t.Symbol, &t.Side, &t.Quantity, &t.Price, &t.Value, &t.TradeType, &t.Details, &t.ExchangeOrderID, &t.ExchangeFee); err != nil {
				tradeRows.Close()
				return nil, fmt.Errorf("scan trade: %w", err)
			}
			t.Timestamp = parseTime(tsStr)
			t.persisted = true // loaded from DB → already persisted; SaveState will skip.
			allTrades = append(allTrades, t)
		}
		tradeRows.Close()
		if err := tradeRows.Err(); err != nil {
			return nil, fmt.Errorf("iterate trades for %s: %w", id, err)
		}
		// Keep only the most recent maxTradeHistory in memory.
		if len(allTrades) > maxTradeHistory {
			allTrades = allTrades[len(allTrades)-maxTradeHistory:]
		}
		if allTrades == nil {
			allTrades = []Trade{}
		}
		s.TradeHistory = allTrades
	}

	// 6. Load portfolio_risk.
	var ksActiveInt, warnSentInt int
	var ksAtStr string
	err = sdb.db.QueryRow("SELECT peak_value, current_drawdown_pct, current_margin_drawdown_pct, kill_switch_active, kill_switch_at, warning_sent FROM portfolio_risk WHERE id = 1").
		Scan(&state.PortfolioRisk.PeakValue, &state.PortfolioRisk.CurrentDrawdownPct, &state.PortfolioRisk.CurrentMarginDrawdownPct,
			&ksActiveInt, &ksAtStr, &warnSentInt)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("load portfolio_risk: %w", err)
	}
	state.PortfolioRisk.KillSwitchActive = ksActiveInt != 0
	state.PortfolioRisk.KillSwitchAt = parseTime(ksAtStr)
	state.PortfolioRisk.WarningSent = warnSentInt != 0

	// 7. Load kill switch events.
	evtRows, err := sdb.db.Query("SELECT timestamp, type, source, drawdown_pct, portfolio_value, peak_value, details FROM kill_switch_events ORDER BY rowid ASC")
	if err != nil {
		return nil, fmt.Errorf("load kill_switch_events: %w", err)
	}
	defer evtRows.Close()
	for evtRows.Next() {
		var evt KillSwitchEvent
		var tsStr string
		if err := evtRows.Scan(&tsStr, &evt.Type, &evt.Source, &evt.DrawdownPct, &evt.PortfolioValue, &evt.PeakValue, &evt.Details); err != nil {
			return nil, fmt.Errorf("scan kill_switch_event: %w", err)
		}
		evt.Timestamp = parseTime(tsStr)
		state.PortfolioRisk.Events = append(state.PortfolioRisk.Events, evt)
	}
	if err := evtRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate kill_switch_events: %w", err)
	}

	// 8. Load correlation_snapshot.
	var snapJSON string
	err = sdb.db.QueryRow("SELECT snapshot_json FROM correlation_snapshot WHERE id = 1").Scan(&snapJSON)
	if err == nil && snapJSON != "{}" {
		var snap CorrelationSnapshot
		if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
			return nil, fmt.Errorf("unmarshal correlation_snapshot: %w", err)
		}
		state.CorrelationSnapshot = &snap
	} else if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("load correlation_snapshot: %w", err)
	}

	return state, nil
}

// QueryTradeHistory returns trades filtered by optional strategy/symbol/time bounds,
// ordered by timestamp desc, with limit/offset pagination.
func (sdb *StateDB) QueryTradeHistory(strategyID, symbol string, since, until time.Time, limit, offset int) ([]Trade, int, error) {
	var where []string
	var args []interface{}
	if strategyID != "" {
		where = append(where, "strategy_id = ?")
		args = append(args, strategyID)
	}
	if symbol != "" {
		where = append(where, "symbol = ?")
		args = append(args, symbol)
	}
	if !since.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, formatTime(since))
	}
	if !until.IsZero() {
		where = append(where, "timestamp <= ?")
		args = append(args, formatTime(until))
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	// Count total matching.
	var total int
	if err := sdb.db.QueryRow("SELECT COUNT(*) FROM trades "+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count trades: %w", err)
	}

	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	query := fmt.Sprintf("SELECT timestamp, strategy_id, symbol, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee FROM trades %s ORDER BY timestamp DESC LIMIT ? OFFSET ?", whereClause)
	queryArgs := append(args, limit, offset)
	rows, err := sdb.db.Query(query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query trades: %w", err)
	}
	defer rows.Close()

	var trades []Trade
	for rows.Next() {
		var t Trade
		var tsStr string
		if err := rows.Scan(&tsStr, &t.StrategyID, &t.Symbol, &t.Side, &t.Quantity, &t.Price, &t.Value, &t.TradeType, &t.Details, &t.ExchangeOrderID, &t.ExchangeFee); err != nil {
			return nil, 0, fmt.Errorf("scan trade: %w", err)
		}
		t.Timestamp = parseTime(tsStr)
		trades = append(trades, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate trades: %w", err)
	}
	if trades == nil {
		trades = []Trade{}
	}
	return trades, total, nil
}

// formatTime converts a time.Time to RFC 3339 string, or "" for zero time.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime converts an RFC 3339 string to time.Time, returning zero time for "".
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
