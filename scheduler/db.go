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
    last_top10_summary TEXT NOT NULL DEFAULT '',
    last_leaderboard_post_date TEXT NOT NULL DEFAULT ''
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
    PRIMARY KEY (strategy_id, symbol)
);

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
    kill_switch_active INTEGER NOT NULL DEFAULT 0,
    kill_switch_at TEXT NOT NULL DEFAULT '',
    warning_sent INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS kill_switch_events (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp TEXT NOT NULL,
    type TEXT NOT NULL,
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

// SaveState writes the full AppState to SQLite within a single transaction.
func (sdb *StateDB) SaveState(state *AppState) error {
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// 1. Upsert app_state singleton.
	if _, err := tx.Exec(`INSERT OR REPLACE INTO app_state (id, cycle_count, last_cycle, last_top10_summary, last_leaderboard_post_date)
		VALUES (1, ?, ?, ?, ?)`,
		state.CycleCount,
		formatTime(state.LastCycle),
		formatTime(state.LastTop10Summary),
		state.LastLeaderboardPostDate,
	); err != nil {
		return fmt.Errorf("upsert app_state: %w", err)
	}

	// 2. Delete all strategies (CASCADE deletes positions + option_positions).
	if _, err := tx.Exec("DELETE FROM strategies"); err != nil {
		return fmt.Errorf("delete strategies: %w", err)
	}

	// 3. Insert strategies with flattened risk state.
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

	stmtPos, err := tx.Prepare(`INSERT INTO positions (strategy_id, symbol, quantity, avg_cost, side, multiplier, owner_strategy_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
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
			if _, err := stmtPos.Exec(s.ID, pos.Symbol, pos.Quantity, pos.AvgCost, pos.Side, pos.Multiplier, pos.OwnerStrategyID); err != nil {
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

	// 4. Append-only trades: find the latest timestamp per strategy already in DB,
	//    insert only newer trades.
	stmtTrade, err := tx.Prepare(`INSERT INTO trades (strategy_id, timestamp, symbol, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare trade insert: %w", err)
	}
	defer stmtTrade.Close()

	for _, s := range state.Strategies {
		if len(s.TradeHistory) == 0 {
			continue
		}
		var latestTS string
		err := tx.QueryRow("SELECT COALESCE(MAX(timestamp), '') FROM trades WHERE strategy_id = ?", s.ID).Scan(&latestTS)
		if err != nil {
			return fmt.Errorf("query latest trade for %s: %w", s.ID, err)
		}
		for _, t := range s.TradeHistory {
			ts := formatTime(t.Timestamp)
			if ts > latestTS {
				if _, err := stmtTrade.Exec(s.ID, ts, t.Symbol, t.Side, t.Quantity, t.Price, t.Value, t.TradeType, t.Details, t.ExchangeOrderID, t.ExchangeFee); err != nil {
					return fmt.Errorf("insert trade for %s: %w", s.ID, err)
				}
			}
		}
	}

	// 5. Upsert portfolio_risk singleton.
	ksActive := 0
	if state.PortfolioRisk.KillSwitchActive {
		ksActive = 1
	}
	warnSent := 0
	if state.PortfolioRisk.WarningSent {
		warnSent = 1
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO portfolio_risk (id, peak_value, current_drawdown_pct, kill_switch_active, kill_switch_at, warning_sent)
		VALUES (1, ?, ?, ?, ?, ?)`,
		state.PortfolioRisk.PeakValue, state.PortfolioRisk.CurrentDrawdownPct,
		ksActive, formatTime(state.PortfolioRisk.KillSwitchAt), warnSent,
	); err != nil {
		return fmt.Errorf("upsert portfolio_risk: %w", err)
	}

	// 6. Kill switch events: replace all (capped at maxKillSwitchEvents).
	if _, err := tx.Exec("DELETE FROM kill_switch_events"); err != nil {
		return fmt.Errorf("delete kill_switch_events: %w", err)
	}
	if len(state.PortfolioRisk.Events) > 0 {
		stmtEvt, err := tx.Prepare(`INSERT INTO kill_switch_events (timestamp, type, drawdown_pct, portfolio_value, peak_value, details)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare kill_switch_event insert: %w", err)
		}
		defer stmtEvt.Close()
		for _, evt := range state.PortfolioRisk.Events {
			if _, err := stmtEvt.Exec(formatTime(evt.Timestamp), evt.Type, evt.DrawdownPct, evt.PortfolioValue, evt.PeakValue, evt.Details); err != nil {
				return fmt.Errorf("insert kill_switch_event: %w", err)
			}
		}
	}

	// 7. Upsert correlation_snapshot singleton as JSON.
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

	return tx.Commit()
}

// LoadState reads the full AppState from SQLite.
// Returns (nil, nil) if the database has no data (fresh DB).
func (sdb *StateDB) LoadState() (*AppState, error) {
	// 1. Load app_state singleton.
	var cycleCount int
	var lastCycleStr, lastTop10Str, lastLeaderboardDate string
	err := sdb.db.QueryRow("SELECT cycle_count, last_cycle, last_top10_summary, last_leaderboard_post_date FROM app_state WHERE id = 1").
		Scan(&cycleCount, &lastCycleStr, &lastTop10Str, &lastLeaderboardDate)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load app_state: %w", err)
	}

	state := &AppState{
		CycleCount:              cycleCount,
		LastCycle:               parseTime(lastCycleStr),
		LastTop10Summary:        parseTime(lastTop10Str),
		LastLeaderboardPostDate: lastLeaderboardDate,
		Strategies:              make(map[string]*StrategyState),
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
	posRows, err := sdb.db.Query("SELECT strategy_id, symbol, quantity, avg_cost, side, multiplier, owner_strategy_id FROM positions")
	if err != nil {
		return nil, fmt.Errorf("load positions: %w", err)
	}
	defer posRows.Close()
	for posRows.Next() {
		var stratID string
		var pos Position
		if err := posRows.Scan(&stratID, &pos.Symbol, &pos.Quantity, &pos.AvgCost, &pos.Side, &pos.Multiplier, &pos.OwnerStrategyID); err != nil {
			return nil, fmt.Errorf("scan position: %w", err)
		}
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
	err = sdb.db.QueryRow("SELECT peak_value, current_drawdown_pct, kill_switch_active, kill_switch_at, warning_sent FROM portfolio_risk WHERE id = 1").
		Scan(&state.PortfolioRisk.PeakValue, &state.PortfolioRisk.CurrentDrawdownPct,
			&ksActiveInt, &ksAtStr, &warnSentInt)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("load portfolio_risk: %w", err)
	}
	state.PortfolioRisk.KillSwitchActive = ksActiveInt != 0
	state.PortfolioRisk.KillSwitchAt = parseTime(ksAtStr)
	state.PortfolioRisk.WarningSent = warnSentInt != 0

	// 7. Load kill switch events.
	evtRows, err := sdb.db.Query("SELECT timestamp, type, drawdown_pct, portfolio_value, peak_value, details FROM kill_switch_events ORDER BY rowid ASC")
	if err != nil {
		return nil, fmt.Errorf("load kill_switch_events: %w", err)
	}
	defer evtRows.Close()
	for evtRows.Next() {
		var evt KillSwitchEvent
		var tsStr string
		if err := evtRows.Scan(&tsStr, &evt.Type, &evt.DrawdownPct, &evt.PortfolioValue, &evt.PeakValue, &evt.Details); err != nil {
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
