package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var initialCapitalGuardWarn func(msg string)

var initialCapitalGuardWarned sync.Map

const schemaDDL = `
CREATE TABLE IF NOT EXISTS app_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    cycle_count INTEGER NOT NULL DEFAULT 0,
    last_cycle TEXT NOT NULL DEFAULT '',
    last_leaderboard_post_date TEXT NOT NULL DEFAULT '',
    last_leaderboard_summaries TEXT NOT NULL DEFAULT '',
    last_summary_post TEXT NOT NULL DEFAULT ''
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
    -- #356 legacy name; migratePendingCircuitClosesColumn renames it to
    -- risk_pending_circuit_closes_json. Keeping the legacy name in CREATE
    -- TABLE so fresh installs land on the same rename path as post-#356
    -- DBs — one code path, no schema fork (#359).
    risk_pending_hl_close_json TEXT NOT NULL DEFAULT '',
    -- #998: regime-profile allocation active profile (flat-switch persistence).
    active_profile TEXT NOT NULL DEFAULT '',
    -- #1394: live spot over-budget books still need operator reconciliation.
    cash_reconcile_required INTEGER NOT NULL DEFAULT 0,
    -- #1408: durable pool-mode marker for one-time allocation transitions.
    shared_wallet_pool_budget INTEGER NOT NULL DEFAULT 0,
    -- #1411: Hurst entry-gate hysteresis latch as JSON (threshold key + state).
    hurst_gate_state TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS positions (
    strategy_id TEXT NOT NULL REFERENCES strategies(id) ON DELETE CASCADE,
    symbol TEXT NOT NULL,
    position_id TEXT NOT NULL DEFAULT '',
    quantity REAL NOT NULL,
    initial_quantity REAL NOT NULL DEFAULT 0,
    avg_cost REAL NOT NULL,
    entry_atr REAL NOT NULL DEFAULT 0,
    side TEXT NOT NULL,
    multiplier REAL NOT NULL DEFAULT 0,
    owner_strategy_id TEXT NOT NULL DEFAULT '',
    opened_at TEXT NOT NULL DEFAULT '',
    stop_loss_oid INTEGER NOT NULL DEFAULT 0,
    stop_loss_trigger_px REAL NOT NULL DEFAULT 0,
    stop_loss_high_water_px REAL NOT NULL DEFAULT 0,
    tp1_oid INTEGER NOT NULL DEFAULT 0,
    tp2_oid INTEGER NOT NULL DEFAULT 0,
    tp_oids_json TEXT NOT NULL DEFAULT '',
    tp_armed_tiers_json TEXT NOT NULL DEFAULT '',
    stop_loss_atr_mult REAL,
    tp_tiers_json TEXT NOT NULL DEFAULT '',
    sl_adjusted_tiers_processed INTEGER NOT NULL DEFAULT 0,
    post_tp_trailing_atr_mult REAL,
    regime TEXT NOT NULL DEFAULT '',
    regime_windows_json TEXT NOT NULL DEFAULT '',
    scale_in_count INTEGER NOT NULL DEFAULT 0,
    last_add_price REAL NOT NULL DEFAULT 0,
    added_notional_usd REAL NOT NULL DEFAULT 0,
    risk_anchor_price REAL NOT NULL DEFAULT 0,
    scale_in_resize_pending INTEGER NOT NULL DEFAULT 0,
    ratchet_fallback_normalize_pending INTEGER NOT NULL DEFAULT 0,
    open_profile TEXT NOT NULL DEFAULT '',
    direction_certified_at_open INTEGER NOT NULL DEFAULT 0,
    direction_certified_states_json TEXT NOT NULL DEFAULT '',
    llm_analysis_requested INTEGER NOT NULL DEFAULT 0,
    llm_verdict TEXT NOT NULL DEFAULT '',
    atr_method_at_open TEXT NOT NULL DEFAULT '',
    hedge_for TEXT NOT NULL DEFAULT '',
    hedge_primary_qty_basis REAL NOT NULL DEFAULT 0,
    -- #1411: gate reading + applied size multiplier frozen at open (0 = unstamped).
    hurst_at_open REAL NOT NULL DEFAULT 0,
    hurst_size_mult REAL NOT NULL DEFAULT 0,
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
    position_id TEXT NOT NULL DEFAULT '',
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
    position_id TEXT NOT NULL DEFAULT '',
    side TEXT NOT NULL,
    quantity REAL NOT NULL,
    price REAL NOT NULL,
    value REAL NOT NULL,
    trade_type TEXT NOT NULL DEFAULT '',
    details TEXT NOT NULL DEFAULT '',
    exchange_order_id TEXT NOT NULL DEFAULT '',
    exchange_fee REAL NOT NULL DEFAULT 0,
    is_close INTEGER NOT NULL DEFAULT 0,
    realized_pnl REAL NOT NULL DEFAULT 0,
    stop_loss_atr_mult REAL,
    tp_tiers_json TEXT NOT NULL DEFAULT '',
    stop_loss_oid INTEGER NOT NULL DEFAULT 0,
    tp_oids_json TEXT NOT NULL DEFAULT '',
    pnl_gross INTEGER NOT NULL DEFAULT 0,
    fee_source TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_trades_strategy ON trades(strategy_id);
CREATE INDEX IF NOT EXISTS idx_trades_symbol ON trades(symbol);
CREATE INDEX IF NOT EXISTS idx_trades_timestamp ON trades(timestamp DESC);
-- idx_trades_close (#455), idx_trades_strategy_position (#471), and
-- idx_trades_strategy_timestamp (#1395) are created in migrateSchema, not
-- here, so legacy DBs add columns before indexes reference them.

CREATE TABLE IF NOT EXISTS portfolio_risk (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    peak_value REAL NOT NULL DEFAULT 0,
    current_drawdown_pct REAL NOT NULL DEFAULT 0,
    current_margin_drawdown_pct REAL NOT NULL DEFAULT 0,
    kill_switch_active INTEGER NOT NULL DEFAULT 0,
    kill_switch_at TEXT NOT NULL DEFAULT '',
    warning_sent INTEGER NOT NULL DEFAULT 0,
    warn_band_entered_at TEXT NOT NULL DEFAULT '',
    last_warning_equity_dd_pct REAL NOT NULL DEFAULT 0,
    last_warning_margin_dd_pct REAL NOT NULL DEFAULT 0,
    warning_equity_delta_pct REAL NOT NULL DEFAULT 0,
    warning_margin_delta_pct REAL NOT NULL DEFAULT 0,
    manual_mark_basis_rebaselined INTEGER NOT NULL DEFAULT 0,
    drawdown_reading_substituted INTEGER NOT NULL DEFAULT 0,
    untrusted_over_limit_since TEXT NOT NULL DEFAULT ''
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

CREATE TABLE IF NOT EXISTS pending_manual_actions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    strategy_id TEXT NOT NULL,
    action TEXT NOT NULL,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL,
    quantity REAL NOT NULL,
    fill_price REAL NOT NULL,
    fill_fee REAL NOT NULL DEFAULT 0,
    exchange_order_id TEXT NOT NULL DEFAULT '',
    stop_loss_oid INTEGER NOT NULL DEFAULT 0,
    stop_loss_trigger_px REAL NOT NULL DEFAULT 0,
    entry_atr REAL NOT NULL DEFAULT 0,
    atr_method TEXT NOT NULL DEFAULT '',
    realized_pnl REAL NOT NULL DEFAULT 0,
    is_full_close INTEGER NOT NULL DEFAULT 0,
    tp_oids_json TEXT NOT NULL DEFAULT '',
    ratchet_fallback_normalize_pending INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

-- #954: per-wallet ledger ingestion state for shared on-exchange accounts.
-- Watermarks bound the userFunding / userNonFundingLedgerUpdates fetch windows;
-- baseline_offset_usd zeroes the ledger-vs-balance drift at adoption time so the
-- alarm watches NEW divergence only (history before adoption lives in neither
-- the trades ledger nor wallet_transfers). baseline_set=0 forces a recompute on
-- the next reconciled cycle (also reset by 'backfill trade-ledger --apply').
CREATE TABLE IF NOT EXISTS wallet_ledger_state (
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    funding_since_ms INTEGER NOT NULL DEFAULT 0,
    transfers_since_ms INTEGER NOT NULL DEFAULT 0,
    baseline_offset_usd REAL NOT NULL DEFAULT 0,
    baseline_set INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (platform, account)
);

-- #954: non-trade cash flows that move the wallet balance but belong to no
-- strategy: deposits, withdrawals, class/internal/sub-account transfers, and
-- funding payments on coins no member owns ("funding_orphan"). amount_usd is
-- SIGNED from the perps account's perspective (+ = balance increased). The
-- drift comparison subtracts SUM(amount_usd) so these flows never read as
-- accounting bugs. dedup_id is the exchange event identity (hash + kind).
CREATE TABLE IF NOT EXISTS wallet_transfers (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    time_ms INTEGER NOT NULL,
    kind TEXT NOT NULL,
    amount_usd REAL NOT NULL,
    dedup_id TEXT NOT NULL UNIQUE
);

-- #1100: exchange-sourced equity journal for shared-wallet TOTAL reconciliation.
-- Where wallet_ledger_state / wallet_transfers feed the per-strategy ATTRIBUTION
-- split (#954), this journal reconstructs the wallet's settled-cash balance from
-- the exchange's OWN cash-flow events — fills, funding, transfers — so the total
-- drift alarm no longer depends on internal trade rows being complete and
-- correctly priced. amount_usd is the SIGNED settled-cash effect on accountValue:
--   fill            = closed_pnl_gross - fee_usd  (closed_pnl is GROSS of fees;
--                     the gross value is retained for attribution/display and is
--                     NEVER summed into equity on its own — #698 / #954 invariant)
--   funding         = signed funding usdc
--   <transfer kind> = signedPerpFlowUSD (deposits / withdrawals / transfers / ...)
-- This is the LIVE total-drift-alarm basis for HL wallets (the drift alarm is
-- driven by the exchange-sourced expected-equity); the trade-ledger drift path is
-- retained as the fail-closed fallback and the per-strategy attribution source.
CREATE TABLE IF NOT EXISTS cashflow_journal (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    time_ms INTEGER NOT NULL,
    kind TEXT NOT NULL,
    amount_usd REAL NOT NULL,
    coin TEXT NOT NULL DEFAULT '',
    closed_pnl_gross REAL NOT NULL DEFAULT 0,
    fee_usd REAL NOT NULL DEFAULT 0,
    dedup_id TEXT NOT NULL UNIQUE
);
CREATE INDEX IF NOT EXISTS idx_cashflow_journal_account ON cashflow_journal(platform, account);

-- #1100: per-wallet journal cursors + adoption baseline. fills/funding/transfers
-- watermarks bound the three incremental fetches; baseline_account_value /
-- baseline_upnl anchor the equity equation at adoption so pre-journal history is
-- never replayed. incomplete=1 LATCHES when an unmapped event kind is seen so a
-- future alarm switch can fail closed; baseline_set=0 forces a re-anchor.
CREATE TABLE IF NOT EXISTS cashflow_journal_state (
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    fills_since_ms INTEGER NOT NULL DEFAULT 0,
    funding_since_ms INTEGER NOT NULL DEFAULT 0,
    transfers_since_ms INTEGER NOT NULL DEFAULT 0,
    baseline_account_value REAL NOT NULL DEFAULT 0,
    baseline_upnl REAL NOT NULL DEFAULT 0,
    baseline_set INTEGER NOT NULL DEFAULT 0,
    incomplete INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (platform, account)
);

CREATE TABLE IF NOT EXISTS pending_limit_orders (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    strategy_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL,
    order_oid INTEGER NOT NULL,
    limit_price REAL NOT NULL,
    order_size REAL NOT NULL,
    tif TEXT NOT NULL DEFAULT 'Alo',
    filled_size REAL NOT NULL DEFAULT 0,
    avg_fill_price REAL NOT NULL DEFAULT 0,
    fill_fee REAL NOT NULL DEFAULT 0,
    entry_atr REAL NOT NULL DEFAULT 0,
    cancel_requested INTEGER NOT NULL DEFAULT 0,
    operator_required_since TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

-- #1147 per-trade trade-quality diagnostics: one row per closed position,
-- inserted eagerly at close; nullable quality metrics filled asynchronously.
CREATE TABLE IF NOT EXISTS trade_diagnostics (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    strategy_id TEXT NOT NULL,
    position_id TEXT NOT NULL DEFAULT '',
    symbol TEXT NOT NULL,
    side TEXT NOT NULL DEFAULT '',
    timeframe TEXT NOT NULL DEFAULT '',
    regime_at_open TEXT NOT NULL DEFAULT '',
    close_reason TEXT NOT NULL DEFAULT '',
    entry_price REAL NOT NULL DEFAULT 0,
    exit_price REAL NOT NULL DEFAULT 0,
    quantity REAL NOT NULL DEFAULT 0,
    realized_pnl REAL NOT NULL DEFAULT 0,
    entry_atr REAL NOT NULL DEFAULT 0,
    stop_loss_atr_mult REAL,
    opened_at TEXT NOT NULL DEFAULT '',
    closed_at TEXT NOT NULL DEFAULT '',
    mfe_price REAL,
    mae_price REAL,
    favorable_pct REAL,
    adverse_pct REAL,
    capture_ratio REAL,
    metrics_status TEXT NOT NULL DEFAULT 'pending',
    llm_verdict TEXT
);

CREATE INDEX IF NOT EXISTS idx_trade_diag_strategy ON trade_diagnostics(strategy_id);
CREATE INDEX IF NOT EXISTS idx_trade_diag_position ON trade_diagnostics(strategy_id, position_id);
-- #1231 /api/diagnostics pages newest-first on the dashboard polling path;
-- these keep the ORDER BY closed_at DESC a bounded index walk instead of a
-- full-table temp b-tree sort as lifetime history grows (one row per closed
-- trade, #1147). Composite covers the ?strategy= filtered page.
CREATE INDEX IF NOT EXISTS idx_trade_diag_closed_at ON trade_diagnostics(closed_at DESC, rowid DESC);
CREATE INDEX IF NOT EXISTS idx_trade_diag_strategy_closed_at ON trade_diagnostics(strategy_id, closed_at DESC, rowid DESC);

-- #1224 per-window regime label history: at most one row per closed bar per
-- (bundle key, window) — the processor skips re-recording a bar already stored,
-- so the debounce run counts distinct bars, not raw per-cycle populations.
-- Raw, never debounced; pruned by regime.transitions.retention_days.
CREATE TABLE IF NOT EXISTS regime_window_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    platform TEXT NOT NULL,
    symbol TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    spec_json TEXT NOT NULL,
    window TEXT NOT NULL,
    label TEXT NOT NULL,
    bar_time TEXT NOT NULL DEFAULT '',
    ts TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_regime_hist_key ON regime_window_history(platform, symbol, timeframe, spec_json, window, id);
CREATE INDEX IF NOT EXISTS idx_regime_hist_ts ON regime_window_history(ts);

-- #1224 per-window label transitions (old -> new). alerted_at is the
-- persisted exactly-once marker for the operator DM (restart/SIGHUP safe).
CREATE TABLE IF NOT EXISTS regime_window_transitions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    platform TEXT NOT NULL,
    symbol TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    spec_json TEXT NOT NULL,
    window TEXT NOT NULL,
    old_label TEXT NOT NULL,
    new_label TEXT NOT NULL,
    bar_time TEXT NOT NULL DEFAULT '',
    ts TEXT NOT NULL,
    alerted_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_regime_trans_key ON regime_window_transitions(platform, symbol, timeframe, spec_json, window, id);
CREATE INDEX IF NOT EXISTS idx_regime_trans_ts ON regime_window_transitions(ts);

-- #1224 last-alerted reversal-pattern signature per bundle key (DM dedupe).
CREATE TABLE IF NOT EXISTS regime_reversal_alerts (
    platform TEXT NOT NULL,
    symbol TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    spec_json TEXT NOT NULL,
    signature TEXT NOT NULL,
    alerted_at TEXT NOT NULL,
    PRIMARY KEY (platform, symbol, timeframe, spec_json)
);
`

type StateDB struct {
	db       *sql.DB
	path     string
	role     storageRole
	ident    *fileIdentity
	readOnly bool
}

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

	sdb := &StateDB{db: db, path: path, role: storageRolePrimary}
	if err := sdb.migrateSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return sdb, nil
}

// openStateDBReadOnly opens an existing state file for inspection only: no
// schema DDL, no migrations, and no journal or shared-memory sidecar written by
// this process. A file that does not exist is an error rather than a fresh DB.
func openStateDBReadOnly(path string) (*StateDB, error) {
	if isInMemoryDBPath(path) {
		return nil, fmt.Errorf("read-only open needs a real file, got %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat state db %q: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve state db %q: %w", path, err)
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("pragma busy_timeout: %w", err)
	}
	return &StateDB{db: db, path: path, role: storageRolePrimary, readOnly: true}, nil
}

func (sdb *StateDB) migrateSchema() error {
	migrations := []string{
		"ALTER TABLE trades ADD COLUMN exchange_order_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE trades ADD COLUMN exchange_fee REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN opened_at TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE portfolio_risk ADD COLUMN current_margin_drawdown_pct REAL NOT NULL DEFAULT 0",
		"ALTER TABLE kill_switch_events ADD COLUMN source TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE portfolio_risk ADD COLUMN warn_band_entered_at TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE portfolio_risk ADD COLUMN last_warning_equity_dd_pct REAL NOT NULL DEFAULT 0",
		"ALTER TABLE portfolio_risk ADD COLUMN last_warning_margin_dd_pct REAL NOT NULL DEFAULT 0",
		"ALTER TABLE portfolio_risk ADD COLUMN warning_equity_delta_pct REAL NOT NULL DEFAULT 0",
		"ALTER TABLE portfolio_risk ADD COLUMN warning_margin_delta_pct REAL NOT NULL DEFAULT 0",
		"ALTER TABLE app_state ADD COLUMN last_leaderboard_summaries TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE app_state ADD COLUMN last_summary_post TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN stop_loss_oid INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN stop_loss_trigger_px REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN stop_loss_high_water_px REAL NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN is_close INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN realized_pnl REAL NOT NULL DEFAULT 0",
		"CREATE INDEX IF NOT EXISTS idx_trades_close ON trades(strategy_id, is_close)",
		"ALTER TABLE trades ADD COLUMN position_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN position_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE option_positions ADD COLUMN position_id TEXT NOT NULL DEFAULT ''",
		"CREATE INDEX IF NOT EXISTS idx_trades_strategy_position ON trades(strategy_id, position_id)",
		"ALTER TABLE positions ADD COLUMN initial_quantity REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN entry_atr REAL NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN regime TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE trades ADD COLUMN entry_atr REAL NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN stop_loss_trigger_px REAL NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN manual INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE pending_manual_actions ADD COLUMN is_full_close INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN tp1_oid INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN tp2_oid INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN tp_oids_json TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE pending_manual_actions ADD COLUMN tp_oids_json TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE trades ADD COLUMN stop_loss_atr_mult REAL",
		"ALTER TABLE trades ADD COLUMN tp_tiers_json TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN stop_loss_atr_mult REAL",
		"ALTER TABLE positions ADD COLUMN tp_tiers_json TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE trades ADD COLUMN stop_loss_oid INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN tp_oids_json TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN sl_adjusted_tiers_processed INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN post_tp_trailing_atr_mult REAL",
		"ALTER TABLE positions ADD COLUMN tp_armed_tiers_json TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN regime TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN regime_windows_json TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN regime_pending_label TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN regime_pending_count INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN regime_applied_label TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN scale_in_count INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN last_add_price REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN added_notional_usd REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN risk_anchor_price REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN scale_in_resize_pending INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN ratchet_fallback_normalize_pending INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE pending_manual_actions ADD COLUMN ratchet_fallback_normalize_pending INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN pnl_gross INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN fee_source TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN open_profile TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE strategies ADD COLUMN active_profile TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN direction_certified_at_open INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN direction_certified_states_json TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN llm_analysis_requested INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN llm_verdict TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN atr_method_at_open TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN hedge_for TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN hedge_primary_qty_basis REAL NOT NULL DEFAULT 0",
		"ALTER TABLE pending_manual_actions ADD COLUMN atr_method TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE strategies ADD COLUMN cash_reconcile_required INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE strategies ADD COLUMN shared_wallet_pool_budget INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE strategies ADD COLUMN hurst_gate_state TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE strategies ADD COLUMN replay_mirror_watermark INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE strategies ADD COLUMN replay_mirror_watermark_source TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN hurst_at_open REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN hurst_size_mult REAL NOT NULL DEFAULT 0",
		"ALTER TABLE trade_diagnostics ADD COLUMN hurst_at_open REAL",
		"ALTER TABLE trade_diagnostics ADD COLUMN hurst_size_mult REAL",
		"ALTER TABLE portfolio_risk ADD COLUMN manual_mark_basis_rebaselined INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE portfolio_risk ADD COLUMN drawdown_reading_substituted INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE portfolio_risk ADD COLUMN untrusted_over_limit_since TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE pending_limit_orders ADD COLUMN operator_required_since TEXT NOT NULL DEFAULT ''",
		"CREATE INDEX IF NOT EXISTS idx_trades_strategy_timestamp ON trades(strategy_id, timestamp DESC, rowid DESC)",
		"ALTER TABLE kill_switch_events ADD COLUMN scope TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE portfolio_risk ADD COLUMN kill_switch_close_applied INTEGER NOT NULL DEFAULT 0",
	}
	for _, ddl := range migrations {
		if _, err := sdb.db.Exec(ddl); err != nil {
			msg := err.Error()
			if strings.Contains(msg, "duplicate column") {
				continue
			}
			return err
		}
	}
	if err := sdb.migratePendingCircuitClosesColumn(); err != nil {
		return err
	}
	if err := sdb.migratePortfolioRiskScopeColumns(); err != nil {
		return err
	}
	return sdb.backfillTradeCloseFlags()
}

func firstTwoTPOIDs(oids []int64) (int64, int64) {
	var first, second int64
	if len(oids) > 0 {
		first = oids[0]
	}
	if len(oids) > 1 {
		second = oids[1]
	}
	return first, second
}

func marshalRegimeWindowsJSON(windows map[string]string) string {
	if len(windows) == 0 {
		return ""
	}
	b, err := json.Marshal(windows)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseRegimeWindowsJSON(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func marshalStringMapJSON(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseStringMapJSON(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func marshalTPOIDsJSON(oids []int64) string {
	if len(oids) == 0 {
		return ""
	}
	b, err := json.Marshal(oids)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseTPOIDsJSON(raw string, legacyTP1, legacyTP2 int64) []int64 {
	if strings.TrimSpace(raw) != "" {
		var oids []int64
		if err := json.Unmarshal([]byte(raw), &oids); err == nil {
			return oids
		}
	}
	var oids []int64
	if legacyTP1 > 0 || legacyTP2 > 0 {
		oids = []int64{legacyTP1, legacyTP2}
	}
	return oids
}

func marshalTPArmedTiersJSON(armed []bool) string {
	if len(armed) == 0 {
		return ""
	}
	b, err := json.Marshal(armed)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseTPArmedTiersJSON(raw string) []bool {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var armed []bool
	if err := json.Unmarshal([]byte(raw), &armed); err != nil {
		return nil
	}
	return armed
}

func backfillTPArmedTiers(pos *Position) {
	if pos == nil || pos.TPArmedTiers != nil {
		return
	}
	if len(pos.TPOIDs) == 0 {
		return
	}
	armed := make([]bool, len(pos.TPOIDs))
	for i, oid := range pos.TPOIDs {
		armed[i] = oid > 0
	}
	pos.TPArmedTiers = armed
}

func (sdb *StateDB) backfillTradeCloseFlags() error {
	_, err := sdb.db.Exec(`UPDATE trades SET is_close = 1
		WHERE is_close = 0 AND realized_pnl = 0 AND COALESCE(pnl_gross, 0) = 0 AND details LIKE '%PnL%'`)
	if err != nil {
		return fmt.Errorf("backfill is_close: %w", err)
	}
	rows, err := sdb.db.Query(`SELECT rowid, details FROM trades
		WHERE is_close = 1 AND realized_pnl = 0 AND COALESCE(pnl_gross, 0) = 0 AND details LIKE '%PnL%'`)
	if err != nil {
		return fmt.Errorf("scan backfill candidates: %w", err)
	}
	type pnlRow struct {
		id  int64
		pnl float64
	}
	var updates []pnlRow
	for rows.Next() {
		var id int64
		var details string
		if err := rows.Scan(&id, &details); err != nil {
			rows.Close()
			return fmt.Errorf("scan backfill row: %w", err)
		}
		if pnl, ok := parseDetailsPnL(details); ok {
			updates = append(updates, pnlRow{id: id, pnl: pnl})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate backfill rows: %w", err)
	}
	if len(updates) == 0 {
		return nil
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin backfill tx: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("UPDATE trades SET realized_pnl = ? WHERE rowid = ?")
	if err != nil {
		return fmt.Errorf("prepare backfill update: %w", err)
	}
	defer stmt.Close()
	for _, u := range updates {
		if _, err := stmt.Exec(u.pnl, u.id); err != nil {
			return fmt.Errorf("backfill realized_pnl: %w", err)
		}
	}
	return tx.Commit()
}

var pnlPattern = regexp.MustCompile(`PnL\s*[:=]\s*\$?(-?\d+(?:\.\d+)?)`)

func parseDetailsPnL(details string) (float64, bool) {
	m := pnlPattern.FindStringSubmatch(details)
	if len(m) < 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func (sdb *StateDB) migratePendingCircuitClosesColumn() error {
	hasLegacy, hasNew, err := sdb.strategiesColumnPresence()
	if err != nil {
		return fmt.Errorf("introspect strategies columns: %w", err)
	}
	switch {
	case hasNew:
		return nil
	case hasLegacy:
		_, err := sdb.db.Exec("ALTER TABLE strategies RENAME COLUMN risk_pending_hl_close_json TO risk_pending_circuit_closes_json")
		return err
	default:
		_, err := sdb.db.Exec("ALTER TABLE strategies ADD COLUMN risk_pending_circuit_closes_json TEXT NOT NULL DEFAULT ''")
		return err
	}
}

func (sdb *StateDB) strategiesColumnPresence() (hasLegacy, hasNew bool, err error) {
	rows, err := sdb.db.Query("PRAGMA table_info(strategies)")
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, false, err
		}
		switch name {
		case "risk_pending_hl_close_json":
			hasLegacy = true
		case "risk_pending_circuit_closes_json":
			hasNew = true
		}
	}
	return hasLegacy, hasNew, rows.Err()
}

const portfolioRiskScopedDDL = `CREATE TABLE portfolio_risk_v2 (
    scope TEXT PRIMARY KEY CHECK (scope IN ('live','paper','')),
    peak_value REAL NOT NULL DEFAULT 0,
    current_drawdown_pct REAL NOT NULL DEFAULT 0,
    current_margin_drawdown_pct REAL NOT NULL DEFAULT 0,
    kill_switch_active INTEGER NOT NULL DEFAULT 0,
    kill_switch_at TEXT NOT NULL DEFAULT '',
    warning_sent INTEGER NOT NULL DEFAULT 0,
    warn_band_entered_at TEXT NOT NULL DEFAULT '',
    last_warning_equity_dd_pct REAL NOT NULL DEFAULT 0,
    last_warning_margin_dd_pct REAL NOT NULL DEFAULT 0,
    warning_equity_delta_pct REAL NOT NULL DEFAULT 0,
    warning_margin_delta_pct REAL NOT NULL DEFAULT 0,
    manual_mark_basis_rebaselined INTEGER NOT NULL DEFAULT 0,
    drawdown_reading_substituted INTEGER NOT NULL DEFAULT 0,
    untrusted_over_limit_since TEXT NOT NULL DEFAULT '',
    kill_switch_close_applied INTEGER NOT NULL DEFAULT 0
)`

const portfolioRiskScopedCopySQL = `INSERT INTO portfolio_risk_v2 (scope, peak_value, current_drawdown_pct,
    current_margin_drawdown_pct, kill_switch_active, kill_switch_at, warning_sent, warn_band_entered_at,
    last_warning_equity_dd_pct, last_warning_margin_dd_pct, warning_equity_delta_pct, warning_margin_delta_pct,
    manual_mark_basis_rebaselined, drawdown_reading_substituted, untrusted_over_limit_since, kill_switch_close_applied)
    SELECT '', COALESCE(peak_value, 0), COALESCE(current_drawdown_pct, 0),
    COALESCE(current_margin_drawdown_pct, 0), COALESCE(kill_switch_active, 0), COALESCE(kill_switch_at, ''),
    COALESCE(warning_sent, 0), COALESCE(warn_band_entered_at, ''),
    COALESCE(last_warning_equity_dd_pct, 0), COALESCE(last_warning_margin_dd_pct, 0),
    COALESCE(warning_equity_delta_pct, 0), COALESCE(warning_margin_delta_pct, 0),
    COALESCE(manual_mark_basis_rebaselined, 0), COALESCE(drawdown_reading_substituted, 0),
    COALESCE(untrusted_over_limit_since, ''), COALESCE(kill_switch_close_applied, 0) FROM portfolio_risk`

const correlationSnapshotScopedDDL = `CREATE TABLE correlation_snapshot_v2 (
    scope TEXT PRIMARY KEY CHECK (scope IN ('live','paper','')),
    snapshot_json TEXT NOT NULL DEFAULT '{}'
)`

const correlationSnapshotScopedCopySQL = `INSERT INTO correlation_snapshot_v2 (scope, snapshot_json)
    SELECT '', COALESCE(snapshot_json, '{}') FROM correlation_snapshot`

func (sdb *StateDB) tableHasColumn(table, column string) (bool, error) {
	rows, err := sdb.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			found = true
		}
	}
	return found, rows.Err()
}

func (sdb *StateDB) rebuildTableWithScope(table, ddl, copySQL string) error {
	hasScope, err := sdb.tableHasColumn(table, "scope")
	if err != nil {
		return fmt.Errorf("introspect %s columns: %w", table, err)
	}
	if hasScope {
		return nil
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		fmt.Sprintf("DROP TABLE IF EXISTS %s_v2", table),
		ddl,
		copySQL,
		fmt.Sprintf("DROP TABLE %s", table),
		fmt.Sprintf("ALTER TABLE %s_v2 RENAME TO %s", table, table),
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate %s to scoped key: %w", table, err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) migratePortfolioRiskScopeColumns() error {
	if err := sdb.rebuildTableWithScope("portfolio_risk", portfolioRiskScopedDDL, portfolioRiskScopedCopySQL); err != nil {
		return err
	}
	return sdb.rebuildTableWithScope("correlation_snapshot", correlationSnapshotScopedDDL, correlationSnapshotScopedCopySQL)
}

func (sdb *StateDB) Close() error {
	return sdb.db.Close()
}

func (sdb *StateDB) InsertTrade(strategyID string, trade Trade) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return err
	}
	isClose := 0
	if trade.IsClose {
		isClose = 1
	}
	isManual := 0
	if trade.Manual {
		isManual = 1
	}
	_, err = sdb.db.Exec(`INSERT INTO trades
			(strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, regime, entry_atr, stop_loss_oid, stop_loss_trigger_px, tp_oids_json, manual, stop_loss_atr_mult, tp_tiers_json, pnl_gross, fee_source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sid, formatTime(trade.Timestamp), trade.Symbol, trade.PositionID, trade.Side,
		trade.Quantity, trade.Price, trade.Value, trade.TradeType, trade.Details,
		trade.ExchangeOrderID, trade.ExchangeFee, isClose, trade.RealizedPnL, trade.Regime,
		trade.EntryATR, trade.StopLossOID, trade.StopLossTriggerPx, marshalTPOIDsJSON(trade.TPOIDs), isManual,
		nullableFloat64(trade.StopLossATRMult), trade.TPTiersJSON, boolToInt(trade.PnLGross), trade.FeeSource)
	if err != nil {
		return fmt.Errorf("insert trade for %s: %w", strategyID, err)
	}
	return nil
}

func (sdb *StateDB) RecentTrades(since time.Time, limit int) ([]Trade, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := sdb.db.Query(`SELECT rowid, timestamp, strategy_id, symbol, COALESCE(position_id, '') AS position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, COALESCE(regime, '') AS regime, COALESCE(entry_atr, 0) AS entry_atr, COALESCE(stop_loss_oid, 0) AS stop_loss_oid, COALESCE(stop_loss_trigger_px, 0) AS stop_loss_trigger_px, COALESCE(tp_oids_json, '') AS tp_oids_json, COALESCE(manual, 0) AS manual, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source
		FROM trades WHERE timestamp >= ? ORDER BY timestamp DESC, rowid DESC LIMIT ?`, formatTime(since), limit)
	if err != nil {
		return nil, fmt.Errorf("query recent trades: %w", err)
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		var tr Trade
		var tsStr string
		var isCloseInt, isManualInt, pnlGrossInt int
		var tpOIDsJSON string
		var slATRMult sql.NullFloat64
		if err := rows.Scan(&tr.sourceRowID, &tsStr, &tr.StrategyID, &tr.Symbol, &tr.PositionID, &tr.Side, &tr.Quantity, &tr.Price, &tr.Value, &tr.TradeType, &tr.Details, &tr.ExchangeOrderID, &tr.ExchangeFee, &isCloseInt, &tr.RealizedPnL, &tr.Regime, &tr.EntryATR, &tr.StopLossOID, &tr.StopLossTriggerPx, &tpOIDsJSON, &isManualInt, &slATRMult, &tr.TPTiersJSON, &pnlGrossInt, &tr.FeeSource); err != nil {
			return nil, fmt.Errorf("scan recent trade: %w", err)
		}
		tr.Timestamp = parseTime(tsStr)
		tr.IsClose = isCloseInt != 0
		tr.Manual = isManualInt != 0
		tr.PnLGross = pnlGrossInt != 0
		tr.TPOIDs = parseTPOIDsJSON(tpOIDsJSON, 0, 0)
		if slATRMult.Valid {
			v := slATRMult.Float64
			tr.StopLossATRMult = &v
		}
		tr.StrategyID = sdb.fromStorageID(tr.StrategyID)
		tr.sourceRole = sdb.storageRoleOf()
		tr.persisted = true
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent trades: %w", err)
	}
	return out, nil
}

func (sdb *StateDB) RecentTradesForStrategy(strategyID string, limit int) ([]Trade, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	if strings.TrimSpace(strategyID) == "" {
		return nil, fmt.Errorf("strategy id required")
	}
	if limit <= 0 {
		return nil, nil
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return nil, err
	}
	rows, err := sdb.db.Query(`SELECT timestamp, strategy_id, symbol, COALESCE(position_id, '') AS position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, COALESCE(regime, '') AS regime, COALESCE(entry_atr, 0) AS entry_atr, COALESCE(stop_loss_oid, 0) AS stop_loss_oid, COALESCE(stop_loss_trigger_px, 0) AS stop_loss_trigger_px, COALESCE(tp_oids_json, '') AS tp_oids_json, COALESCE(manual, 0) AS manual, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source
		FROM trades WHERE strategy_id = ? ORDER BY timestamp DESC, rowid DESC LIMIT ?`, sid, limit)
	if err != nil {
		return nil, fmt.Errorf("query recent trades for %s: %w", strategyID, err)
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		var tr Trade
		var tsStr string
		var isCloseInt, isManualInt, pnlGrossInt int
		var tpOIDsJSON string
		var slATRMult sql.NullFloat64
		if err := rows.Scan(&tsStr, &tr.StrategyID, &tr.Symbol, &tr.PositionID, &tr.Side, &tr.Quantity, &tr.Price, &tr.Value, &tr.TradeType, &tr.Details, &tr.ExchangeOrderID, &tr.ExchangeFee, &isCloseInt, &tr.RealizedPnL, &tr.Regime, &tr.EntryATR, &tr.StopLossOID, &tr.StopLossTriggerPx, &tpOIDsJSON, &isManualInt, &slATRMult, &tr.TPTiersJSON, &pnlGrossInt, &tr.FeeSource); err != nil {
			return nil, fmt.Errorf("scan recent trade for %s: %w", strategyID, err)
		}
		tr.Timestamp = parseTime(tsStr)
		tr.IsClose = isCloseInt != 0
		tr.Manual = isManualInt != 0
		tr.PnLGross = pnlGrossInt != 0
		tr.TPOIDs = parseTPOIDsJSON(tpOIDsJSON, 0, 0)
		if slATRMult.Valid {
			v := slATRMult.Float64
			tr.StopLossATRMult = &v
		}
		tr.StrategyID = strategyID
		tr.sourceRole = sdb.storageRoleOf()
		tr.persisted = true
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent trades for %s: %w", strategyID, err)
	}
	return out, nil
}

func (sdb *StateDB) UpdateTradeStampedFields(strategyID string, ts time.Time, entryATR float64, stopLossOID int64, stopLossTriggerPx float64, tpOIDs []int64, stopLossATRMult *float64, tpTiersJSON string) error {
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return err
	}
	_, err = sdb.db.Exec(
		`UPDATE trades SET entry_atr = ?, stop_loss_oid = ?, stop_loss_trigger_px = ?, tp_oids_json = ?, stop_loss_atr_mult = ?, tp_tiers_json = ? WHERE strategy_id = ? AND timestamp = ?`,
		entryATR, stopLossOID, stopLossTriggerPx, marshalTPOIDsJSON(tpOIDs), nullableFloat64(stopLossATRMult), tpTiersJSON, sid, formatTime(ts),
	)
	return err
}

func (sdb *StateDB) ReconcileModelOnlyClose(u modelOnlyCloseCorrection) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.toStorageID(u.StrategyID)
	if err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	ts := formatTime(u.Timestamp)
	var res sql.Result
	if u.Complete {
		res, err = tx.Exec(
			`UPDATE trades SET quantity = ?, price = ?, value = ?, details = ?, exchange_fee = ?, realized_pnl = ?, exchange_order_id = ?, fee_source = ?
			 WHERE strategy_id = ? AND timestamp = ? AND symbol = ? AND is_close = 1 AND exchange_order_id = '' AND fee_source = ?`,
			u.FilledQty, u.RowPrice, u.Value, u.Details, u.CumFee, u.CumGross, u.OID, FeeSourceUserFills,
			sid, ts, u.Symbol, FeeSourceReconcileAdjustment)
	} else {
		res, err = tx.Exec(
			`UPDATE trades SET quantity = ?, price = ?, value = ?, details = ?, exchange_fee = ?, realized_pnl = ?
			 WHERE strategy_id = ? AND timestamp = ? AND symbol = ? AND is_close = 1 AND exchange_order_id = '' AND fee_source = ?`,
			u.FilledQty, u.RowPrice, u.Value, u.Details, u.CumFee, u.CumGross,
			sid, ts, u.Symbol, FeeSourceReconcileAdjustment)
	}
	if err != nil {
		return fmt.Errorf("update trades row: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("model-only trades row not found or already reconciled for %s %s @ %s (affected %d)", u.StrategyID, u.Symbol, ts, n)
	}

	res, err = tx.Exec(
		`UPDATE closed_positions SET close_price = ?, realized_pnl = ?
		 WHERE strategy_id = ? AND symbol = ? AND close_reason = ? AND closed_at = ?`,
		u.VwapPx, u.CumGross-u.CumFee, sid, u.Symbol, u.CloseReason, formatTime(u.ClosedAt))
	if err != nil {
		return fmt.Errorf("update closed_positions row: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("closed_positions basis row not found for %s %s @ %s (affected %d)", u.StrategyID, u.Symbol, formatTime(u.ClosedAt), n)
	}

	if u.PositionID != "" {
		if _, err := tx.Exec(
			`UPDATE trade_diagnostics SET exit_price = ?, realized_pnl = ?
			 WHERE strategy_id = ? AND position_id = ? AND closed_at = ?`,
			u.VwapPx, u.CumGross-u.CumFee, sid, u.PositionID, formatTime(u.ClosedAt)); err != nil {
			return fmt.Errorf("update trade_diagnostics row: %w", err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) LoadModelOnlyCloseBasis(strategyID, symbol, closeReason string, closedAt time.Time) (*modelOnlyClosedBasis, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return nil, err
	}
	row := sdb.db.QueryRow(
		`SELECT quantity, avg_cost, side, multiplier FROM closed_positions
		 WHERE strategy_id = ? AND symbol = ? AND close_reason = ? AND closed_at = ?
		 ORDER BY id DESC LIMIT 1`,
		sid, symbol, closeReason, formatTime(closedAt))
	var b modelOnlyClosedBasis
	if err := row.Scan(&b.Quantity, &b.AvgCost, &b.Side, &b.Multiplier); err != nil {
		return nil, fmt.Errorf("load closed-position basis for %s %s @ %s: %w", strategyID, symbol, formatTime(closedAt), err)
	}
	return &b, nil
}

func (sdb *StateDB) MarkModelOnlyCloseAbandoned(strategyID, symbol string, ts time.Time) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return err
	}
	_, err = sdb.db.Exec(
		`UPDATE trades SET details = details || ? WHERE strategy_id = ? AND timestamp = ? AND symbol = ?
		  AND exchange_order_id = '' AND fee_source = ? AND details LIKE '%fill-reconciled%'
		  AND details NOT LIKE '%[reconcile-abandoned]%'`,
		" [reconcile-abandoned]", sid, formatTime(ts), symbol, FeeSourceReconcileAdjustment)
	return err
}

func nullableFloat64(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func nullableString(v *string) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func (sdb *StateDB) SetInitialCapital(strategyID string, value float64) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if value <= 0 {
		return fmt.Errorf("initial_capital must be > 0, got %g", value)
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE strategies SET initial_capital = ? WHERE id = ?", value, sid)
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	initialCapitalGuardWarned.Delete(strategyID)
	fmt.Fprintf(os.Stderr, "[state] initial_capital override for %s set to $%.2f (#343)\n", strategyID, value)
	return nil
}

func (sdb *StateDB) PersistSharedWalletPoolStateTransition(s *StrategyState) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if s == nil {
		return fmt.Errorf("strategy state is nil")
	}
	sid, err := sdb.toStorageID(s.ID)
	if err != nil {
		return err
	}
	poolInt := 0
	if s.SharedWalletPoolBudget {
		poolInt = 1
	}
	res, err := sdb.db.Exec(
		`UPDATE strategies
		 SET cash = ?, initial_capital = ?, risk_peak_value = ?,
		     risk_current_drawdown_pct = ?, shared_wallet_pool_budget = ?
		 WHERE id = ?`,
		s.Cash, s.InitialCapital, s.RiskState.PeakValue,
		s.RiskState.CurrentDrawdownPct, poolInt, sid,
	)
	if err != nil {
		return fmt.Errorf("persist shared-wallet pool transition for %s: %w", s.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("shared-wallet pool transition rows affected for %s: %w", s.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("no strategy row for id=%q", s.ID)
	}
	initialCapitalGuardWarned.Delete(s.ID)
	return nil
}

// storeCommitHook runs immediately before each per-file commit. It exists so
// tests can inject a persistence failure at a chosen file and prove the other
// file's committed effects are neither lost nor duplicated.
var storeCommitHook func(role storageRole) error

// scopeSaveRequest describes one physical file's share of a save: the books it
// owns, the risk scopes it owns, and the manual actions its transaction
// acknowledges.
type scopeSaveRequest struct {
	Strategies []*StrategyState
	ScopeOf    map[string]PortfolioScope
	Scopes     []PortfolioScope
	// FullFile marks a request that covers every scope the file owns, so the
	// whole roster is replaced exactly as the single-file save always did. A
	// partial request replaces only the rows it names and leaves the other
	// scope's books and any orphan row untouched.
	FullFile bool
	// IncludeUnscoped keeps a legacy unscoped risk row round-tripping in the
	// single-file layout, where one file owns every scope.
	IncludeUnscoped bool
	WriteMeta       bool
	AckActionID     []int64
}

func sortedStrategyStates(m map[string]*StrategyState) []*StrategyState {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*StrategyState, 0, len(ids))
	for _, id := range ids {
		if s := m[id]; s != nil {
			out = append(out, s)
		}
	}
	return out
}

func (sdb *StateDB) SaveState(state *AppState) error {
	return sdb.saveStateSubset(state, scopeSaveRequest{
		Strategies:      sortedStrategyStates(state.Strategies),
		Scopes:          []PortfolioScope{ScopeLive, ScopePaper},
		FullFile:        true,
		IncludeUnscoped: true,
		WriteMeta:       true,
	})
}

func saveProcessMeta(tx *sql.Tx, state *AppState) error {
	lbSummariesJSON := ""
	if len(state.LastLeaderboardSummaries) > 0 {
		raw, err := json.Marshal(state.LastLeaderboardSummaries)
		if err != nil {
			return fmt.Errorf("marshal last_leaderboard_summaries: %w", err)
		}
		lbSummariesJSON = string(raw)
	}
	summaryPostJSON := ""
	if len(state.LastSummaryPost) > 0 {
		raw, err := json.Marshal(state.LastSummaryPost)
		if err != nil {
			return fmt.Errorf("marshal last_summary_post: %w", err)
		}
		summaryPostJSON = string(raw)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO app_state (id, cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries, last_summary_post)
		VALUES (1, ?, ?, ?, ?, ?)`,
		state.CycleCount,
		formatTime(state.LastCycle),
		state.LastLeaderboardPostDate,
		lbSummariesJSON,
		summaryPostJSON,
	); err != nil {
		return fmt.Errorf("upsert app_state: %w", err)
	}
	return nil
}

func scopePlaceholders(scopes []PortfolioScope) (string, []interface{}) {
	args := make([]interface{}, 0, len(scopes)+1)
	marks := make([]string, 0, len(scopes)+1)
	for _, scope := range scopes {
		marks = append(marks, "?")
		args = append(args, string(scope))
	}
	marks = append(marks, "?")
	args = append(args, string(scopeUnassigned))
	return strings.Join(marks, ", "), args
}

func (sdb *StateDB) saveStateSubset(state *AppState, req scopeSaveRequest) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	storageIDs := make([]string, len(req.Strategies))
	for i, s := range req.Strategies {
		sid, err := sdb.toStorageID(s.ID)
		if err != nil {
			return err
		}
		storageIDs[i] = sid
	}

	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO app_state (id, cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries, last_summary_post)
		VALUES (1, 0, '', '', '', '')
		ON CONFLICT(id) DO NOTHING`); err != nil {
		return fmt.Errorf("ensure app_state: %w", err)
	}
	if req.WriteMeta {
		if err := saveProcessMeta(tx, state); err != nil {
			return err
		}
	}

	existingInitCaps := make(map[string]float64)
	existingRows, err := tx.Query("SELECT id, initial_capital FROM strategies")
	if err != nil {
		return fmt.Errorf("read existing initial_capital: %w", err)
	}
	for existingRows.Next() {
		var id string
		var existing float64
		if err := existingRows.Scan(&id, &existing); err != nil {
			existingRows.Close()
			return fmt.Errorf("scan existing initial_capital: %w", err)
		}
		existingInitCaps[id] = existing
	}
	if err := existingRows.Err(); err != nil {
		existingRows.Close()
		return fmt.Errorf("iterate existing initial_capital: %w", err)
	}
	existingRows.Close()

	if req.FullFile {
		if _, err := tx.Exec("DELETE FROM strategies"); err != nil {
			return fmt.Errorf("delete strategies: %w", err)
		}
	} else if len(storageIDs) > 0 {
		marks := make([]string, len(storageIDs))
		args := make([]interface{}, len(storageIDs))
		for i, id := range storageIDs {
			marks[i] = "?"
			args[i] = id
		}
		if _, err := tx.Exec("DELETE FROM strategies WHERE id IN ("+strings.Join(marks, ", ")+")", args...); err != nil {
			return fmt.Errorf("delete strategies: %w", err)
		}
	}

	stmtStrat, err := tx.Prepare(`INSERT OR REPLACE INTO strategies (id, type, platform, cash, initial_capital,
		risk_peak_value, risk_max_drawdown_pct, risk_current_drawdown_pct,
		risk_daily_pnl, risk_daily_pnl_date, risk_consecutive_losses,
		risk_circuit_breaker, risk_circuit_breaker_until, risk_pending_circuit_closes_json, active_profile,
		cash_reconcile_required, shared_wallet_pool_budget, hurst_gate_state, replay_mirror_watermark, replay_mirror_watermark_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare strategy insert: %w", err)
	}
	defer stmtStrat.Close()

	stmtPos, err := tx.Prepare(`INSERT INTO positions (strategy_id, symbol, position_id, quantity, initial_quantity, avg_cost, entry_atr, side, multiplier, owner_strategy_id, opened_at, stop_loss_oid, stop_loss_trigger_px, stop_loss_high_water_px, tp1_oid, tp2_oid, tp_oids_json, tp_armed_tiers_json, stop_loss_atr_mult, tp_tiers_json, sl_adjusted_tiers_processed, post_tp_trailing_atr_mult, regime, regime_windows_json, regime_pending_label, regime_pending_count, regime_applied_label, scale_in_count, last_add_price, added_notional_usd, risk_anchor_price, scale_in_resize_pending, ratchet_fallback_normalize_pending, open_profile, direction_certified_at_open, direction_certified_states_json, llm_analysis_requested, llm_verdict, atr_method_at_open, hedge_for, hedge_primary_qty_basis, hurst_at_open, hurst_size_mult)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare position insert: %w", err)
	}
	defer stmtPos.Close()

	stmtOpt, err := tx.Prepare(`INSERT INTO option_positions (strategy_id, id, position_id, underlying, option_type, strike, expiry, dte,
		action, quantity, entry_premium, entry_premium_usd, current_value_usd,
		delta, gamma, theta, vega, opened_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare option_position insert: %w", err)
	}
	defer stmtOpt.Close()

	for i, s := range req.Strategies {
		sid := storageIDs[i]
		if prev, ok := existingInitCaps[sid]; ok && prev > 0 && s.InitialCapital != prev {
			applyInitialCapitalGuard(s, prev)
		}

		cbInt := 0
		if s.RiskState.CircuitBreaker {
			cbInt = 1
		}
		cashReconcileInt := 0
		if s.CashReconcileRequired {
			cashReconcileInt = 1
		}
		poolBudgetInt := 0
		if s.SharedWalletPoolBudget {
			poolBudgetInt = 1
		}
		if _, err := stmtStrat.Exec(
			sid, s.Type, s.Platform, s.Cash, s.InitialCapital,
			s.RiskState.PeakValue, s.RiskState.MaxDrawdownPct, s.RiskState.CurrentDrawdownPct,
			s.RiskState.DailyPnL, s.RiskState.DailyPnLDate, s.RiskState.ConsecutiveLosses,
			cbInt, formatTime(s.RiskState.CircuitBreakerUntil),
			s.RiskState.MarshalPendingCircuitClosesJSON(),
			strategyActiveProfile(s),
			cashReconcileInt,
			poolBudgetInt,
			marshalHurstGateStateJSON(s.HurstGate),
			s.ReplayMirrorWatermark,
			s.ReplayMirrorWatermarkSource,
		); err != nil {
			return fmt.Errorf("insert strategy %s: %w", s.ID, err)
		}

		for _, sym := range sortedPositionSymbols(s.Positions) {
			pos := s.Positions[sym]
			positionID := ensurePositionTradeID(s.ID, pos.Symbol, pos)
			tp1OID, tp2OID := firstTwoTPOIDs(pos.TPOIDs)
			scaleInResizePending := 0
			if pos.ScaleInResizePending {
				scaleInResizePending = 1
			}
			ratchetFallbackNormalizePending := 0
			if pos.RatchetFallbackNormalizePending {
				ratchetFallbackNormalizePending = 1
			}
			directionCertifiedAtOpen := 0
			if pos.DirectionCertifiedAtOpen {
				directionCertifiedAtOpen = 1
			}
			llmAnalysisRequested := 0
			if pos.LLMAnalysisRequested {
				llmAnalysisRequested = 1
			}
			if _, err := stmtPos.Exec(sid, pos.Symbol, positionID, pos.Quantity, pos.InitialQuantity, pos.AvgCost, pos.EntryATR, pos.Side, pos.Multiplier, sdb.toStorageOwnerID(pos.OwnerStrategyID), formatTime(pos.OpenedAt), pos.StopLossOID, pos.StopLossTriggerPx, pos.StopLossHighWaterPx, tp1OID, tp2OID, marshalTPOIDsJSON(pos.TPOIDs), marshalTPArmedTiersJSON(pos.TPArmedTiers), nullableFloat64(pos.StopLossATRMult), pos.TPTiersJSON, pos.SLAdjustedTiersProcessed, nullableFloat64(pos.PostTPTrailingATRMult), pos.Regime, marshalRegimeWindowsJSON(pos.RegimeWindows), pos.RegimePendingLabel, pos.RegimePendingCount, pos.RegimeAppliedLabel, pos.ScaleInCount, pos.LastAddPrice, pos.AddedNotionalUSD, pos.RiskAnchorPrice, scaleInResizePending, ratchetFallbackNormalizePending, pos.OpenProfile, directionCertifiedAtOpen, marshalStringMapJSON(pos.DirectionCertifiedStatesAtOpen), llmAnalysisRequested, pos.LLMVerdict, pos.ATRMethodAtOpen, pos.HedgeFor, pos.HedgePrimaryQtyBasis, pos.HurstAtOpen, pos.HurstSizeMult); err != nil {
				return fmt.Errorf("insert position %s/%s: %w", s.ID, pos.Symbol, err)
			}
		}

		for _, key := range sortedOptionKeys(s.OptionPositions) {
			opt := s.OptionPositions[key]
			positionID := ensureOptionTradeID(s.ID, opt)
			if _, err := stmtOpt.Exec(
				sid, key, positionID, opt.Underlying, opt.OptionType, opt.Strike, opt.Expiry, opt.DTE,
				opt.Action, opt.Quantity, opt.EntryPremium, opt.EntryPremiumUSD, opt.CurrentValueUSD,
				opt.Greeks.Delta, opt.Greeks.Gamma, opt.Greeks.Theta, opt.Greeks.Vega,
				formatTime(opt.OpenedAt),
			); err != nil {
				return fmt.Errorf("insert option_position %s/%s: %w", s.ID, key, err)
			}
		}
	}

	stmtTrade, err := tx.Prepare(`INSERT INTO trades (strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, regime, entry_atr, stop_loss_oid, stop_loss_trigger_px, tp_oids_json, manual, stop_loss_atr_mult, tp_tiers_json, pnl_gross, fee_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare trade insert: %w", err)
	}
	defer stmtTrade.Close()

	type trackedFlush struct {
		strat *StrategyState
		index int
	}
	var flushed []trackedFlush

	for i, s := range req.Strategies {
		sid := storageIDs[i]
		for j := range s.TradeHistory {
			if s.TradeHistory[j].persisted {
				continue
			}
			t := s.TradeHistory[j]
			isClose := 0
			if t.IsClose {
				isClose = 1
			}
			isManual := 0
			if t.Manual {
				isManual = 1
			}
			if _, err := stmtTrade.Exec(sid, formatTime(t.Timestamp), t.Symbol, t.PositionID, t.Side, t.Quantity, t.Price, t.Value, t.TradeType, t.Details, t.ExchangeOrderID, t.ExchangeFee, isClose, t.RealizedPnL, t.Regime, t.EntryATR, t.StopLossOID, t.StopLossTriggerPx, marshalTPOIDsJSON(t.TPOIDs), isManual, nullableFloat64(t.StopLossATRMult), t.TPTiersJSON, boolToInt(t.PnLGross), t.FeeSource); err != nil {
				return fmt.Errorf("insert trade for %s: %w", s.ID, err)
			}
			flushed = append(flushed, trackedFlush{strat: s, index: j})
		}
	}

	hasClosed := false
	for _, s := range req.Strategies {
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
		for i, s := range req.Strategies {
			sid := storageIDs[i]
			for _, cp := range s.ClosedPositions {
				closedSID := sid
				if cp.StrategyID != s.ID {
					closedSID, err = sdb.toStorageID(cp.StrategyID)
					if err != nil {
						return err
					}
				}
				if _, err := stmtClosed.Exec(
					closedSID, cp.Symbol, cp.Quantity, cp.AvgCost, cp.Side, cp.Multiplier,
					formatTime(cp.OpenedAt), formatTime(cp.ClosedAt),
					cp.ClosePrice, cp.RealizedPnL, cp.CloseReason, cp.DurationSeconds,
				); err != nil {
					return fmt.Errorf("insert closed_position %s/%s: %w", cp.StrategyID, cp.Symbol, err)
				}
			}
		}
	}

	hasClosedOpt := false
	for _, s := range req.Strategies {
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
		for i, s := range req.Strategies {
			sid := storageIDs[i]
			for _, cop := range s.ClosedOptionPositions {
				closedSID := sid
				if cop.StrategyID != s.ID {
					closedSID, err = sdb.toStorageID(cop.StrategyID)
					if err != nil {
						return err
					}
				}
				if _, err := stmtClosedOpt.Exec(
					closedSID, cop.PositionID, cop.Underlying, cop.OptionType,
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

	var flushedDiags []TradeDiagnosticsRow
	for i, s := range req.Strategies {
		rows, err := sdb.flushPendingDiagnosticsFor(tx, s, storageIDs[i], req.ScopeOf[s.ID])
		if err != nil {
			return err
		}
		flushedDiags = append(flushedDiags, rows...)
	}

	scopeFilter, scopeArgs := scopePlaceholders(req.Scopes)
	if req.FullFile {
		if _, err := tx.Exec("DELETE FROM portfolio_risk"); err != nil {
			return fmt.Errorf("delete portfolio_risk: %w", err)
		}
		if _, err := tx.Exec("DELETE FROM kill_switch_events"); err != nil {
			return fmt.Errorf("delete kill_switch_events: %w", err)
		}
	} else {
		if _, err := tx.Exec("DELETE FROM portfolio_risk WHERE scope IN ("+scopeFilter+")", scopeArgs...); err != nil {
			return fmt.Errorf("delete portfolio_risk: %w", err)
		}
		if _, err := tx.Exec("DELETE FROM kill_switch_events WHERE COALESCE(scope, '') IN ("+scopeFilter+")", scopeArgs...); err != nil {
			return fmt.Errorf("delete kill_switch_events: %w", err)
		}
	}
	stmtEvt, err := tx.Prepare(`INSERT INTO kill_switch_events (scope, timestamp, type, source, drawdown_pct, portfolio_value, peak_value, details)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare kill_switch_event insert: %w", err)
	}
	defer stmtEvt.Close()
	owned := make(map[PortfolioScope]bool, len(req.Scopes)+1)
	for _, scope := range req.Scopes {
		owned[scope] = true
	}
	if req.IncludeUnscoped {
		owned[scopeUnassigned] = true
	}
	for _, scope := range sortedPortfolioScopes(state.PortfolioRisk) {
		if !owned[scope] {
			continue
		}
		prs := state.PortfolioRisk[scope]
		if prs == nil {
			continue
		}
		ksActive := boolToInt(prs.KillSwitchActive)
		warnSent := boolToInt(prs.WarningSent)
		basisRebaselined := boolToInt(prs.ManualMarkBasisRebaselined)
		ddSubstituted := boolToInt(prs.DrawdownReadingSubstituted)
		closeApplied := boolToInt(prs.KillSwitchCloseApplied)
		if _, err := tx.Exec(`INSERT OR REPLACE INTO portfolio_risk (scope, peak_value, current_drawdown_pct, current_margin_drawdown_pct, kill_switch_active, kill_switch_at, warning_sent, warn_band_entered_at, last_warning_equity_dd_pct, last_warning_margin_dd_pct, warning_equity_delta_pct, warning_margin_delta_pct, manual_mark_basis_rebaselined, drawdown_reading_substituted, untrusted_over_limit_since, kill_switch_close_applied)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(scope), prs.PeakValue, prs.CurrentDrawdownPct, prs.CurrentMarginDrawdownPct,
			ksActive, formatTime(prs.KillSwitchAt), warnSent, formatTime(prs.WarnBandEnteredAt),
			prs.LastWarningEquityDDPct, prs.LastWarningMarginDDPct,
			prs.WarningEquityDeltaPct, prs.WarningMarginDeltaPct,
			basisRebaselined, ddSubstituted, formatTime(prs.UntrustedOverLimitSince), closeApplied,
		); err != nil {
			return fmt.Errorf("upsert portfolio_risk: %w", err)
		}
		for _, evt := range prs.Events {
			if _, err := stmtEvt.Exec(string(scope), formatTime(evt.Timestamp), evt.Type, evt.Source, evt.DrawdownPct, evt.PortfolioValue, evt.PeakValue, evt.Details); err != nil {
				return fmt.Errorf("insert kill_switch_event: %w", err)
			}
		}
	}

	if req.FullFile {
		if _, err := tx.Exec("DELETE FROM correlation_snapshot"); err != nil {
			return fmt.Errorf("delete correlation_snapshot: %w", err)
		}
	} else if _, err := tx.Exec("DELETE FROM correlation_snapshot WHERE scope IN ("+scopeFilter+")", scopeArgs...); err != nil {
		return fmt.Errorf("delete correlation_snapshot: %w", err)
	}
	for _, scope := range sortedCorrelationScopes(state.CorrelationSnapshot) {
		if !owned[scope] {
			continue
		}
		snap := state.CorrelationSnapshot[scope]
		snapJSON := "{}"
		if snap != nil {
			data, err := json.Marshal(snap)
			if err != nil {
				return fmt.Errorf("marshal correlation_snapshot: %w", err)
			}
			snapJSON = string(data)
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO correlation_snapshot (scope, snapshot_json) VALUES (?, ?)`, string(scope), snapJSON); err != nil {
			return fmt.Errorf("upsert correlation_snapshot: %w", err)
		}
	}

	if err := deletePendingManualActionsByID(tx, req.AckActionID); err != nil {
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
	for _, s := range req.Strategies {
		s.ClosedPositions = nil
		s.ClosedOptionPositions = nil
		s.pendingTradeDiagnostics = nil
	}
	for _, f := range flushed {
		f.strat.TradeHistory[f.index].persisted = true
	}
	enqueueFlushedDiagnostics(flushedDiags)
	return nil
}

func applyInitialCapitalGuard(s *StrategyState, existing float64) {
	if s == nil || existing <= 0 || s.InitialCapital == existing {
		return
	}
	attempted := s.InitialCapital
	s.InitialCapital = existing
	if _, alreadyWarned := initialCapitalGuardWarned.LoadOrStore(s.ID, struct{}{}); !alreadyWarned {
		msg := fmt.Sprintf("blocking initial_capital change for %s ($%.2f → $%.2f); baseline preserved. Use StateDB.SetInitialCapital or set initial_capital in config to change the baseline (#343)",
			s.ID, existing, attempted)
		fmt.Fprintf(os.Stderr, "[state] WARN: %s\n", msg)
		if initialCapitalGuardWarn != nil {
			initialCapitalGuardWarn(msg)
		}
	}
}

func (sdb *StateDB) flushPendingDiagnosticsFor(tx *sql.Tx, s *StrategyState, storageID string, scope PortfolioScope) ([]TradeDiagnosticsRow, error) {
	if tx == nil || s == nil || len(s.pendingTradeDiagnostics) == 0 {
		return nil, nil
	}
	flushed := make([]TradeDiagnosticsRow, 0, len(s.pendingTradeDiagnostics))
	for i := range s.pendingTradeDiagnostics {
		row := &s.pendingTradeDiagnostics[i]
		row.Scope = scope
		row.SourceRole = sdb.storageRoleOf()
		if err := insertTradeDiagnosticsRowAs(tx, row, storageID); err != nil {
			return nil, err
		}
		flushed = append(flushed, *row)
	}
	return flushed, nil
}

func enqueueFlushedDiagnostics(rows []TradeDiagnosticsRow) {
	if tradeDiagnosticsEnqueue == nil {
		return
	}
	for _, row := range rows {
		tradeDiagnosticsEnqueue(row)
	}
}

func (sdb *StateDB) SaveStrategyBook(s *StrategyState) error {
	return sdb.saveStrategyBookWithAcks(s, scopeUnassigned, nil)
}

func (sdb *StateDB) saveStrategyBookWithAcks(s *StrategyState, scope PortfolioScope, ackIDs []int64) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if s == nil {
		return fmt.Errorf("strategy state is nil")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	sid, err := sdb.toStorageID(s.ID)
	if err != nil {
		return err
	}

	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO app_state (id, cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries, last_summary_post)
		VALUES (1, 0, '', '', '', '')
		ON CONFLICT(id) DO NOTHING`); err != nil {
		return fmt.Errorf("ensure app_state: %w", err)
	}

	var existingInitCap float64
	err = tx.QueryRow("SELECT initial_capital FROM strategies WHERE id = ?", sid).Scan(&existingInitCap)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read existing initial_capital for %s: %w", s.ID, err)
	}
	if err == nil {
		applyInitialCapitalGuard(s, existingInitCap)
	}

	cbInt := 0
	if s.RiskState.CircuitBreaker {
		cbInt = 1
	}
	cashReconcileInt := 0
	if s.CashReconcileRequired {
		cashReconcileInt = 1
	}
	poolBudgetInt := 0
	if s.SharedWalletPoolBudget {
		poolBudgetInt = 1
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO strategies (id, type, platform, cash, initial_capital,
		risk_peak_value, risk_max_drawdown_pct, risk_current_drawdown_pct,
		risk_daily_pnl, risk_daily_pnl_date, risk_consecutive_losses,
		risk_circuit_breaker, risk_circuit_breaker_until, risk_pending_circuit_closes_json, active_profile,
		cash_reconcile_required, shared_wallet_pool_budget, hurst_gate_state, replay_mirror_watermark, replay_mirror_watermark_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sid, s.Type, s.Platform, s.Cash, s.InitialCapital,
		s.RiskState.PeakValue, s.RiskState.MaxDrawdownPct, s.RiskState.CurrentDrawdownPct,
		s.RiskState.DailyPnL, s.RiskState.DailyPnLDate, s.RiskState.ConsecutiveLosses,
		cbInt, formatTime(s.RiskState.CircuitBreakerUntil),
		s.RiskState.MarshalPendingCircuitClosesJSON(),
		strategyActiveProfile(s),
		cashReconcileInt,
		poolBudgetInt,
		marshalHurstGateStateJSON(s.HurstGate),
		s.ReplayMirrorWatermark,
		s.ReplayMirrorWatermarkSource,
	); err != nil {
		return fmt.Errorf("insert strategy %s: %w", s.ID, err)
	}

	if _, err := tx.Exec(`DELETE FROM positions WHERE strategy_id = ?`, sid); err != nil {
		return fmt.Errorf("delete positions for %s: %w", s.ID, err)
	}
	if _, err := tx.Exec(`DELETE FROM option_positions WHERE strategy_id = ?`, sid); err != nil {
		return fmt.Errorf("delete option_positions for %s: %w", s.ID, err)
	}

	stmtPos, err := tx.Prepare(`INSERT INTO positions (strategy_id, symbol, position_id, quantity, initial_quantity, avg_cost, entry_atr, side, multiplier, owner_strategy_id, opened_at, stop_loss_oid, stop_loss_trigger_px, stop_loss_high_water_px, tp1_oid, tp2_oid, tp_oids_json, tp_armed_tiers_json, stop_loss_atr_mult, tp_tiers_json, sl_adjusted_tiers_processed, post_tp_trailing_atr_mult, regime, regime_windows_json, regime_pending_label, regime_pending_count, regime_applied_label, scale_in_count, last_add_price, added_notional_usd, risk_anchor_price, scale_in_resize_pending, ratchet_fallback_normalize_pending, open_profile, direction_certified_at_open, direction_certified_states_json, llm_analysis_requested, llm_verdict, atr_method_at_open, hedge_for, hedge_primary_qty_basis, hurst_at_open, hurst_size_mult)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare position insert: %w", err)
	}
	defer stmtPos.Close()
	for _, sym := range sortedPositionSymbols(s.Positions) {
		pos := s.Positions[sym]
		positionID := ensurePositionTradeID(s.ID, pos.Symbol, pos)
		tp1OID, tp2OID := firstTwoTPOIDs(pos.TPOIDs)
		scaleInResizePending := 0
		if pos.ScaleInResizePending {
			scaleInResizePending = 1
		}
		ratchetFallbackNormalizePending := 0
		if pos.RatchetFallbackNormalizePending {
			ratchetFallbackNormalizePending = 1
		}
		directionCertifiedAtOpen := 0
		if pos.DirectionCertifiedAtOpen {
			directionCertifiedAtOpen = 1
		}
		llmAnalysisRequested := 0
		if pos.LLMAnalysisRequested {
			llmAnalysisRequested = 1
		}
		if _, err := stmtPos.Exec(sid, pos.Symbol, positionID, pos.Quantity, pos.InitialQuantity, pos.AvgCost, pos.EntryATR, pos.Side, pos.Multiplier, sdb.toStorageOwnerID(pos.OwnerStrategyID), formatTime(pos.OpenedAt), pos.StopLossOID, pos.StopLossTriggerPx, pos.StopLossHighWaterPx, tp1OID, tp2OID, marshalTPOIDsJSON(pos.TPOIDs), marshalTPArmedTiersJSON(pos.TPArmedTiers), nullableFloat64(pos.StopLossATRMult), pos.TPTiersJSON, pos.SLAdjustedTiersProcessed, nullableFloat64(pos.PostTPTrailingATRMult), pos.Regime, marshalRegimeWindowsJSON(pos.RegimeWindows), pos.RegimePendingLabel, pos.RegimePendingCount, pos.RegimeAppliedLabel, pos.ScaleInCount, pos.LastAddPrice, pos.AddedNotionalUSD, pos.RiskAnchorPrice, scaleInResizePending, ratchetFallbackNormalizePending, pos.OpenProfile, directionCertifiedAtOpen, marshalStringMapJSON(pos.DirectionCertifiedStatesAtOpen), llmAnalysisRequested, pos.LLMVerdict, pos.ATRMethodAtOpen, pos.HedgeFor, pos.HedgePrimaryQtyBasis, pos.HurstAtOpen, pos.HurstSizeMult); err != nil {
			return fmt.Errorf("insert position %s/%s: %w", s.ID, pos.Symbol, err)
		}
	}

	if len(s.OptionPositions) > 0 {
		stmtOpt, err := tx.Prepare(`INSERT INTO option_positions (strategy_id, id, position_id, underlying, option_type, strike, expiry, dte,
			action, quantity, entry_premium, entry_premium_usd, current_value_usd,
			delta, gamma, theta, vega, opened_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare option_position insert: %w", err)
		}
		defer stmtOpt.Close()
		for _, key := range sortedOptionKeys(s.OptionPositions) {
			opt := s.OptionPositions[key]
			positionID := ensureOptionTradeID(s.ID, opt)
			if _, err := stmtOpt.Exec(
				sid, key, positionID, opt.Underlying, opt.OptionType, opt.Strike, opt.Expiry, opt.DTE,
				opt.Action, opt.Quantity, opt.EntryPremium, opt.EntryPremiumUSD, opt.CurrentValueUSD,
				opt.Greeks.Delta, opt.Greeks.Gamma, opt.Greeks.Theta, opt.Greeks.Vega,
				formatTime(opt.OpenedAt),
			); err != nil {
				return fmt.Errorf("insert option_position %s/%s: %w", s.ID, key, err)
			}
		}
	}

	type trackedFlush struct {
		index int
	}
	var flushed []trackedFlush
	stmtTrade, err := tx.Prepare(`INSERT INTO trades (strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, regime, entry_atr, stop_loss_oid, stop_loss_trigger_px, tp_oids_json, manual, stop_loss_atr_mult, tp_tiers_json, pnl_gross, fee_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare trade insert: %w", err)
	}
	defer stmtTrade.Close()
	for i := range s.TradeHistory {
		if s.TradeHistory[i].persisted {
			continue
		}
		t := s.TradeHistory[i]
		isClose := 0
		if t.IsClose {
			isClose = 1
		}
		isManual := 0
		if t.Manual {
			isManual = 1
		}
		if _, err := stmtTrade.Exec(sid, formatTime(t.Timestamp), t.Symbol, t.PositionID, t.Side, t.Quantity, t.Price, t.Value, t.TradeType, t.Details, t.ExchangeOrderID, t.ExchangeFee, isClose, t.RealizedPnL, t.Regime, t.EntryATR, t.StopLossOID, t.StopLossTriggerPx, marshalTPOIDsJSON(t.TPOIDs), isManual, nullableFloat64(t.StopLossATRMult), t.TPTiersJSON, boolToInt(t.PnLGross), t.FeeSource); err != nil {
			return fmt.Errorf("insert trade for %s: %w", s.ID, err)
		}
		flushed = append(flushed, trackedFlush{index: i})
	}

	if len(s.ClosedPositions) > 0 {
		stmtClosed, err := tx.Prepare(`INSERT INTO closed_positions
			(strategy_id, symbol, quantity, avg_cost, side, multiplier,
			 opened_at, closed_at, close_price, realized_pnl, close_reason, duration_seconds)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare closed_position insert: %w", err)
		}
		defer stmtClosed.Close()
		for _, cp := range s.ClosedPositions {
			closedSID, err := sdb.toStorageID(cp.StrategyID)
			if err != nil {
				return err
			}
			if _, err := stmtClosed.Exec(
				closedSID, cp.Symbol, cp.Quantity, cp.AvgCost, cp.Side, cp.Multiplier,
				formatTime(cp.OpenedAt), formatTime(cp.ClosedAt),
				cp.ClosePrice, cp.RealizedPnL, cp.CloseReason, cp.DurationSeconds,
			); err != nil {
				return fmt.Errorf("insert closed_position %s/%s: %w", cp.StrategyID, cp.Symbol, err)
			}
		}
	}

	if len(s.ClosedOptionPositions) > 0 {
		stmtClosedOpt, err := tx.Prepare(`INSERT INTO closed_option_positions
			(strategy_id, position_id, underlying, option_type, strike, expiry,
			 action, quantity, entry_premium_usd, close_price_usd, realized_pnl,
			 opened_at, closed_at, close_reason, duration_seconds)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("prepare closed_option_position insert: %w", err)
		}
		defer stmtClosedOpt.Close()
		for _, cop := range s.ClosedOptionPositions {
			closedSID, err := sdb.toStorageID(cop.StrategyID)
			if err != nil {
				return err
			}
			if _, err := stmtClosedOpt.Exec(
				closedSID, cop.PositionID, cop.Underlying, cop.OptionType,
				cop.Strike, cop.Expiry, cop.Action, cop.Quantity,
				cop.EntryPremiumUSD, cop.ClosePriceUSD, cop.RealizedPnL,
				formatTime(cop.OpenedAt), formatTime(cop.ClosedAt),
				cop.CloseReason, cop.DurationSeconds,
			); err != nil {
				return fmt.Errorf("insert closed_option_position %s/%s: %w", cop.StrategyID, cop.PositionID, err)
			}
		}
	}

	flushedDiags, err := sdb.flushPendingDiagnosticsFor(tx, s, sid, scope)
	if err != nil {
		return err
	}

	if err := deletePendingManualActionsByID(tx, ackIDs); err != nil {
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
	s.ClosedPositions = nil
	s.ClosedOptionPositions = nil
	s.pendingTradeDiagnostics = nil
	for _, f := range flushed {
		s.TradeHistory[f.index].persisted = true
	}
	enqueueFlushedDiagnostics(flushedDiags)
	return nil
}

func (sdb *StateDB) QueryClosedPositions(strategyID, symbol string, since, until time.Time, limit, offset int) ([]ClosedPosition, int, error) {
	var where []string
	var args []interface{}
	if strategyID != "" {
		sid, err := sdb.toStorageID(strategyID)
		if err != nil {
			return nil, 0, err
		}
		where = append(where, "strategy_id = ?")
		args = append(args, sid)
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
		cp.StrategyID = sdb.fromStorageID(cp.StrategyID)
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

func (sdb *StateDB) QueryClosedOptionPositions(strategyID, underlying string, since, until time.Time, limit, offset int) ([]ClosedOptionPosition, int, error) {
	var where []string
	var args []interface{}
	if strategyID != "" {
		sid, err := sdb.toStorageID(strategyID)
		if err != nil {
			return nil, 0, err
		}
		where = append(where, "strategy_id = ?")
		args = append(args, sid)
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
		cop.StrategyID = sdb.fromStorageID(cop.StrategyID)
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

type processMeta struct {
	CycleCount               int
	LastCycle                time.Time
	LastLeaderboardPostDate  string
	LastLeaderboardSummaries map[string]time.Time
	LastSummaryPost          map[string]time.Time
}

// storageOrphan is a stored book whose identifier maps to no configured
// strategy in that file. It is reported rather than keyed into the roster, so
// an alias never silently adopts another book and a stale book stays visible.
type storageOrphan struct {
	Role          storageRole
	StorageID     string
	PositionCount int
}

type scopeLoad struct {
	Role                storageRole
	Strategies          map[string]*StrategyState
	Orphans             []storageOrphan
	PortfolioRisk       map[PortfolioScope]*PortfolioRiskState
	CorrelationSnapshot map[PortfolioScope]*CorrelationSnapshot
}

// mapStoredStrategyID resolves a stored identifier to its process identifier.
// A file with no identity map is the legacy single-file case, where the two
// namespaces are the same and every row maps.
func (sdb *StateDB) mapStoredStrategyID(storageID string) (string, bool) {
	if sdb == nil || sdb.ident == nil {
		return storageID, true
	}
	procID, ok := sdb.ident.storeToProc[storageID]
	return procID, ok
}

func (sdb *StateDB) loadProcessMeta() (processMeta, bool, error) {
	var meta processMeta
	var lastCycleStr, lastLBSummariesJSON, lastSummaryPostJSON string
	err := sdb.db.QueryRow("SELECT cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries, last_summary_post FROM app_state WHERE id = 1").
		Scan(&meta.CycleCount, &lastCycleStr, &meta.LastLeaderboardPostDate, &lastLBSummariesJSON, &lastSummaryPostJSON)
	if err == sql.ErrNoRows {
		return processMeta{}, false, nil
	}
	if err != nil {
		return processMeta{}, false, fmt.Errorf("load app_state: %w", err)
	}
	meta.LastCycle = parseTime(lastCycleStr)
	meta.LastLeaderboardSummaries = make(map[string]time.Time)
	if lastLBSummariesJSON != "" {
		if err := json.Unmarshal([]byte(lastLBSummariesJSON), &meta.LastLeaderboardSummaries); err != nil {
			return processMeta{}, false, fmt.Errorf("parse last_leaderboard_summaries: %w", err)
		}
	}
	meta.LastSummaryPost = make(map[string]time.Time)
	if lastSummaryPostJSON != "" {
		if err := json.Unmarshal([]byte(lastSummaryPostJSON), &meta.LastSummaryPost); err != nil {
			return processMeta{}, false, fmt.Errorf("parse last_summary_post: %w", err)
		}
	}
	return meta, true, nil
}

func (sdb *StateDB) loadScopeBooks(scopes []PortfolioScope) (*scopeLoad, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	out := &scopeLoad{
		Role:                sdb.storageRoleOf(),
		Strategies:          make(map[string]*StrategyState),
		PortfolioRisk:       make(map[PortfolioScope]*PortfolioRiskState),
		CorrelationSnapshot: make(map[PortfolioScope]*CorrelationSnapshot),
	}
	// storageID -> process id for every mapped book in this file.
	loaded := make(map[string]string)
	orphanPositions := make(map[string]int)

	rows, err := sdb.db.Query(`SELECT id, type, platform, cash, initial_capital,
		risk_peak_value, risk_max_drawdown_pct, risk_current_drawdown_pct,
		risk_daily_pnl, risk_daily_pnl_date, risk_consecutive_losses,
		risk_circuit_breaker, risk_circuit_breaker_until, risk_pending_circuit_closes_json,
		COALESCE(active_profile, '') AS active_profile,
		COALESCE(cash_reconcile_required, 0) AS cash_reconcile_required,
		COALESCE(shared_wallet_pool_budget, 0) AS shared_wallet_pool_budget,
		COALESCE(hurst_gate_state, '') AS hurst_gate_state,
		COALESCE(replay_mirror_watermark, 0) AS replay_mirror_watermark,
		COALESCE(replay_mirror_watermark_source, '') AS replay_mirror_watermark_source
		FROM strategies`)
	if err != nil {
		return nil, fmt.Errorf("load strategies: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var s StrategyState
		var storedID string
		var cbInt int
		var cashReconcileInt int
		var poolBudgetInt int
		var cbUntilStr, pendingCircuitClosesJSON, activeProfile string
		var hurstGateJSON string
		if err := rows.Scan(
			&storedID, &s.Type, &s.Platform, &s.Cash, &s.InitialCapital,
			&s.RiskState.PeakValue, &s.RiskState.MaxDrawdownPct, &s.RiskState.CurrentDrawdownPct,
			&s.RiskState.DailyPnL, &s.RiskState.DailyPnLDate, &s.RiskState.ConsecutiveLosses,
			&cbInt, &cbUntilStr, &pendingCircuitClosesJSON, &activeProfile,
			&cashReconcileInt, &poolBudgetInt, &hurstGateJSON,
			&s.ReplayMirrorWatermark,
			&s.ReplayMirrorWatermarkSource,
		); err != nil {
			return nil, fmt.Errorf("scan strategy: %w", err)
		}
		procID, mapped := sdb.mapStoredStrategyID(storedID)
		if !mapped {
			orphanPositions[storedID] = 0
			continue
		}
		s.ID = procID
		s.RiskState.CircuitBreaker = cbInt != 0
		s.RiskState.CircuitBreakerUntil = parseTime(cbUntilStr)
		s.RiskState.UnmarshalPendingCircuitClosesJSON(pendingCircuitClosesJSON)
		s.CashReconcileRequired = cashReconcileInt != 0
		s.SharedWalletPoolBudget = poolBudgetInt != 0
		s.HurstGate = unmarshalHurstGateStateJSON(hurstGateJSON)
		s.SharedWalletPerformanceOnly = s.SharedWalletPoolBudget
		if activeProfile != "" {
			s.RegimeProfile = &RegimeProfileState{ActiveProfile: activeProfile}
		}
		s.Positions = make(map[string]*Position)
		s.OptionPositions = make(map[string]*OptionPosition)
		s.TradeHistory = []Trade{}
		out.Strategies[procID] = &s
		loaded[storedID] = procID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate strategies: %w", err)
	}

	posRows, err := sdb.db.Query("SELECT strategy_id, symbol, COALESCE(position_id, '') AS position_id, quantity, initial_quantity, avg_cost, entry_atr, side, multiplier, owner_strategy_id, opened_at, stop_loss_oid, stop_loss_trigger_px, stop_loss_high_water_px, COALESCE(tp1_oid, 0) AS tp1_oid, COALESCE(tp2_oid, 0) AS tp2_oid, COALESCE(tp_oids_json, '') AS tp_oids_json, COALESCE(tp_armed_tiers_json, '') AS tp_armed_tiers_json, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(sl_adjusted_tiers_processed, 0) AS sl_adjusted_tiers_processed, post_tp_trailing_atr_mult, COALESCE(regime, '') AS regime, COALESCE(regime_windows_json, '') AS regime_windows_json, COALESCE(regime_pending_label, '') AS regime_pending_label, COALESCE(regime_pending_count, 0) AS regime_pending_count, COALESCE(regime_applied_label, '') AS regime_applied_label, COALESCE(scale_in_count, 0) AS scale_in_count, COALESCE(last_add_price, 0) AS last_add_price, COALESCE(added_notional_usd, 0) AS added_notional_usd, COALESCE(risk_anchor_price, 0) AS risk_anchor_price, COALESCE(scale_in_resize_pending, 0) AS scale_in_resize_pending, COALESCE(ratchet_fallback_normalize_pending, 0) AS ratchet_fallback_normalize_pending, COALESCE(open_profile, '') AS open_profile, COALESCE(direction_certified_at_open, 0) AS direction_certified_at_open, COALESCE(direction_certified_states_json, '') AS direction_certified_states_json, COALESCE(llm_analysis_requested, 0) AS llm_analysis_requested, COALESCE(llm_verdict, '') AS llm_verdict, COALESCE(atr_method_at_open, '') AS atr_method_at_open, COALESCE(hedge_for, '') AS hedge_for, COALESCE(hedge_primary_qty_basis, 0) AS hedge_primary_qty_basis, COALESCE(hurst_at_open, 0) AS hurst_at_open, COALESCE(hurst_size_mult, 0) AS hurst_size_mult FROM positions")
	if err != nil {
		return nil, fmt.Errorf("load positions: %w", err)
	}
	defer posRows.Close()
	for posRows.Next() {
		var storedID string
		var pos Position
		var openedAtStr string
		var tp1OID, tp2OID int64
		var tpOIDsJSON string
		var tpArmedTiersJSON string
		var regimeWindowsJSON string
		var slATRMult sql.NullFloat64
		var postTPTrailingMult sql.NullFloat64
		var scaleInResizePending int
		var ratchetFallbackNormalizePending int
		var directionCertifiedAtOpen int
		var directionCertifiedStatesJSON string
		var llmAnalysisRequested int
		if err := posRows.Scan(&storedID, &pos.Symbol, &pos.TradePositionID, &pos.Quantity, &pos.InitialQuantity, &pos.AvgCost, &pos.EntryATR, &pos.Side, &pos.Multiplier, &pos.OwnerStrategyID, &openedAtStr, &pos.StopLossOID, &pos.StopLossTriggerPx, &pos.StopLossHighWaterPx, &tp1OID, &tp2OID, &tpOIDsJSON, &tpArmedTiersJSON, &slATRMult, &pos.TPTiersJSON, &pos.SLAdjustedTiersProcessed, &postTPTrailingMult, &pos.Regime, &regimeWindowsJSON, &pos.RegimePendingLabel, &pos.RegimePendingCount, &pos.RegimeAppliedLabel, &pos.ScaleInCount, &pos.LastAddPrice, &pos.AddedNotionalUSD, &pos.RiskAnchorPrice, &scaleInResizePending, &ratchetFallbackNormalizePending, &pos.OpenProfile, &directionCertifiedAtOpen, &directionCertifiedStatesJSON, &llmAnalysisRequested, &pos.LLMVerdict, &pos.ATRMethodAtOpen, &pos.HedgeFor, &pos.HedgePrimaryQtyBasis, &pos.HurstAtOpen, &pos.HurstSizeMult); err != nil {
			return nil, fmt.Errorf("scan position: %w", err)
		}
		procID, mapped := loaded[storedID]
		if !mapped {
			if _, isOrphan := orphanPositions[storedID]; isOrphan {
				orphanPositions[storedID]++
			}
			continue
		}
		pos.OwnerStrategyID = sdb.fromStorageOwnerID(pos.OwnerStrategyID)
		pos.ScaleInResizePending = scaleInResizePending != 0
		pos.RatchetFallbackNormalizePending = ratchetFallbackNormalizePending != 0
		pos.LLMAnalysisRequested = llmAnalysisRequested != 0
		pos.DirectionCertifiedAtOpen = directionCertifiedAtOpen != 0
		pos.DirectionCertifiedStatesAtOpen = parseStringMapJSON(directionCertifiedStatesJSON)
		pos.OpenedAt = parseTime(openedAtStr)
		pos.TPOIDs = parseTPOIDsJSON(tpOIDsJSON, tp1OID, tp2OID)
		pos.TPArmedTiers = parseTPArmedTiersJSON(tpArmedTiersJSON)
		pos.RegimeWindows = parseRegimeWindowsJSON(regimeWindowsJSON)
		backfillTPArmedTiers(&pos)
		if slATRMult.Valid {
			v := slATRMult.Float64
			pos.StopLossATRMult = &v
		}
		if postTPTrailingMult.Valid {
			v := postTPTrailingMult.Float64
			pos.PostTPTrailingATRMult = &v
		}
		if s, ok := out.Strategies[procID]; ok {
			s.Positions[pos.Symbol] = &pos
		}
	}
	if err := posRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate positions: %w", err)
	}

	optRows, err := sdb.db.Query(`SELECT strategy_id, id, COALESCE(position_id, '') AS position_id, underlying, option_type, strike, expiry, dte,
		action, quantity, entry_premium, entry_premium_usd, current_value_usd,
		delta, gamma, theta, vega, opened_at FROM option_positions`)
	if err != nil {
		return nil, fmt.Errorf("load option_positions: %w", err)
	}
	defer optRows.Close()
	for optRows.Next() {
		var storedID string
		var opt OptionPosition
		var openedAtStr string
		if err := optRows.Scan(
			&storedID, &opt.ID, &opt.TradePositionID, &opt.Underlying, &opt.OptionType, &opt.Strike, &opt.Expiry, &opt.DTE,
			&opt.Action, &opt.Quantity, &opt.EntryPremium, &opt.EntryPremiumUSD, &opt.CurrentValueUSD,
			&opt.Greeks.Delta, &opt.Greeks.Gamma, &opt.Greeks.Theta, &opt.Greeks.Vega,
			&openedAtStr,
		); err != nil {
			return nil, fmt.Errorf("scan option_position: %w", err)
		}
		procID, mapped := loaded[storedID]
		if !mapped {
			continue
		}
		opt.OpenedAt = parseTime(openedAtStr)
		if s, ok := out.Strategies[procID]; ok {
			s.OptionPositions[opt.ID] = &opt
		}
	}
	if err := optRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate option_positions: %w", err)
	}

	for storedID, procID := range loaded {
		s := out.Strategies[procID]
		tradeRows, err := sdb.db.Query(`SELECT timestamp, strategy_id, symbol, COALESCE(position_id, '') AS position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, COALESCE(regime, '') AS regime, COALESCE(entry_atr, 0) AS entry_atr, COALESCE(stop_loss_oid, 0) AS stop_loss_oid, COALESCE(stop_loss_trigger_px, 0) AS stop_loss_trigger_px, COALESCE(tp_oids_json, '') AS tp_oids_json, COALESCE(manual, 0) AS manual, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source
			FROM trades WHERE strategy_id = ? ORDER BY timestamp DESC, rowid DESC LIMIT ?`, storedID, maxTradeHistory)
		if err != nil {
			return nil, fmt.Errorf("load trades for %s: %w", procID, err)
		}
		var allTrades []Trade
		for tradeRows.Next() {
			var t Trade
			var tsStr string
			var isCloseInt, isManualInt, pnlGrossInt int
			var tpOIDsJSON string
			var slATRMult sql.NullFloat64
			if err := tradeRows.Scan(&tsStr, &t.StrategyID, &t.Symbol, &t.PositionID, &t.Side, &t.Quantity, &t.Price, &t.Value, &t.TradeType, &t.Details, &t.ExchangeOrderID, &t.ExchangeFee, &isCloseInt, &t.RealizedPnL, &t.Regime, &t.EntryATR, &t.StopLossOID, &t.StopLossTriggerPx, &tpOIDsJSON, &isManualInt, &slATRMult, &t.TPTiersJSON, &pnlGrossInt, &t.FeeSource); err != nil {
				tradeRows.Close()
				return nil, fmt.Errorf("scan trade: %w", err)
			}
			t.StrategyID = procID
			t.Timestamp = parseTime(tsStr)
			t.IsClose = isCloseInt != 0
			t.Manual = isManualInt != 0
			t.PnLGross = pnlGrossInt != 0
			t.TPOIDs = parseTPOIDsJSON(tpOIDsJSON, 0, 0)
			if slATRMult.Valid {
				v := slATRMult.Float64
				t.StopLossATRMult = &v
			}
			t.persisted = true
			allTrades = append(allTrades, t)
		}
		tradeRows.Close()
		if err := tradeRows.Err(); err != nil {
			return nil, fmt.Errorf("iterate trades for %s: %w", procID, err)
		}
		for i, j := 0, len(allTrades)-1; i < j; i, j = i+1, j-1 {
			allTrades[i], allTrades[j] = allTrades[j], allTrades[i]
		}
		if allTrades == nil {
			allTrades = []Trade{}
		}
		s.TradeHistory = allTrades
	}

	scopeFilter, scopeArgs := scopePlaceholders(scopes)
	prsRows, err := sdb.db.Query("SELECT scope, peak_value, current_drawdown_pct, current_margin_drawdown_pct, kill_switch_active, kill_switch_at, warning_sent, COALESCE(warn_band_entered_at, '') AS warn_band_entered_at, COALESCE(last_warning_equity_dd_pct, 0) AS last_warning_equity_dd_pct, COALESCE(last_warning_margin_dd_pct, 0) AS last_warning_margin_dd_pct, COALESCE(warning_equity_delta_pct, 0) AS warning_equity_delta_pct, COALESCE(warning_margin_delta_pct, 0) AS warning_margin_delta_pct, COALESCE(manual_mark_basis_rebaselined, 0) AS manual_mark_basis_rebaselined, COALESCE(drawdown_reading_substituted, 0) AS drawdown_reading_substituted, COALESCE(untrusted_over_limit_since, '') AS untrusted_over_limit_since, COALESCE(kill_switch_close_applied, 0) AS kill_switch_close_applied FROM portfolio_risk WHERE scope IN ("+scopeFilter+")", scopeArgs...)
	if err != nil {
		return nil, fmt.Errorf("load portfolio_risk: %w", err)
	}
	defer prsRows.Close()
	for prsRows.Next() {
		var scopeStr, ksAtStr, warnBandEnteredAtStr, untrustedOverLimitSinceStr string
		var ksActiveInt, warnSentInt, basisRebaselinedInt, ddSubstitutedInt, closeAppliedInt int
		prs := &PortfolioRiskState{}
		if err := prsRows.Scan(&scopeStr, &prs.PeakValue, &prs.CurrentDrawdownPct, &prs.CurrentMarginDrawdownPct,
			&ksActiveInt, &ksAtStr, &warnSentInt, &warnBandEnteredAtStr, &prs.LastWarningEquityDDPct,
			&prs.LastWarningMarginDDPct, &prs.WarningEquityDeltaPct, &prs.WarningMarginDeltaPct,
			&basisRebaselinedInt, &ddSubstitutedInt, &untrustedOverLimitSinceStr, &closeAppliedInt); err != nil {
			return nil, fmt.Errorf("scan portfolio_risk: %w", err)
		}
		prs.KillSwitchCloseApplied = closeAppliedInt != 0
		prs.ManualMarkBasisRebaselined = basisRebaselinedInt != 0
		prs.DrawdownReadingSubstituted = ddSubstitutedInt != 0
		prs.UntrustedOverLimitSince = parseTime(untrustedOverLimitSinceStr)
		prs.KillSwitchActive = ksActiveInt != 0
		prs.KillSwitchAt = parseTime(ksAtStr)
		prs.WarningSent = warnSentInt != 0
		prs.WarnBandEnteredAt = parseTime(warnBandEnteredAtStr)
		out.PortfolioRisk[PortfolioScope(scopeStr)] = prs
	}
	if err := prsRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate portfolio_risk: %w", err)
	}

	evtRows, err := sdb.db.Query("SELECT COALESCE(scope, '') AS scope, timestamp, type, source, drawdown_pct, portfolio_value, peak_value, details FROM kill_switch_events WHERE COALESCE(scope, '') IN ("+scopeFilter+") ORDER BY rowid ASC", scopeArgs...)
	if err != nil {
		return nil, fmt.Errorf("load kill_switch_events: %w", err)
	}
	defer evtRows.Close()
	for evtRows.Next() {
		var evt KillSwitchEvent
		var tsStr, scopeStr string
		if err := evtRows.Scan(&scopeStr, &tsStr, &evt.Type, &evt.Source, &evt.DrawdownPct, &evt.PortfolioValue, &evt.PeakValue, &evt.Details); err != nil {
			return nil, fmt.Errorf("scan kill_switch_event: %w", err)
		}
		evt.Timestamp = parseTime(tsStr)
		scope := PortfolioScope(scopeStr)
		evt.Scope = scope
		prs, ok := out.PortfolioRisk[scope]
		if !ok || prs == nil {
			prs = &PortfolioRiskState{}
			out.PortfolioRisk[scope] = prs
		}
		prs.Events = append(prs.Events, evt)
	}
	if err := evtRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate kill_switch_events: %w", err)
	}

	snapRows, err := sdb.db.Query("SELECT scope, snapshot_json FROM correlation_snapshot WHERE scope IN ("+scopeFilter+")", scopeArgs...)
	if err != nil {
		return nil, fmt.Errorf("load correlation_snapshot: %w", err)
	}
	defer snapRows.Close()
	for snapRows.Next() {
		var scopeStr, snapJSON string
		if err := snapRows.Scan(&scopeStr, &snapJSON); err != nil {
			return nil, fmt.Errorf("scan correlation_snapshot: %w", err)
		}
		if snapJSON == "" || snapJSON == "{}" {
			continue
		}
		var snap CorrelationSnapshot
		if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
			return nil, fmt.Errorf("unmarshal correlation_snapshot: %w", err)
		}
		out.CorrelationSnapshot[PortfolioScope(scopeStr)] = &snap
	}
	if err := snapRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate correlation_snapshot: %w", err)
	}

	orphanIDs := make([]string, 0, len(orphanPositions))
	for storedID := range orphanPositions {
		orphanIDs = append(orphanIDs, storedID)
	}
	sort.Strings(orphanIDs)
	for _, storedID := range orphanIDs {
		out.Orphans = append(out.Orphans, storageOrphan{
			Role:          out.Role,
			StorageID:     storedID,
			PositionCount: orphanPositions[storedID],
		})
	}

	return out, nil
}

func (sdb *StateDB) LoadState() (*AppState, error) {
	meta, ok, err := sdb.loadProcessMeta()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	books, err := sdb.loadScopeBooks([]PortfolioScope{ScopeLive, ScopePaper})
	if err != nil {
		return nil, err
	}
	state := &AppState{
		CycleCount:               meta.CycleCount,
		LastCycle:                meta.LastCycle,
		LastLeaderboardPostDate:  meta.LastLeaderboardPostDate,
		LastLeaderboardSummaries: meta.LastLeaderboardSummaries,
		LastSummaryPost:          meta.LastSummaryPost,
		Strategies:               books.Strategies,
		PortfolioRisk:            books.PortfolioRisk,
		CorrelationSnapshot:      books.CorrelationSnapshot,
	}
	return state, nil
}

func sortedPositionSymbols(m map[string]*Position) []string {
	out := make([]string, 0, len(m))
	for sym, pos := range m {
		if pos != nil {
			out = append(out, sym)
		}
	}
	sort.Strings(out)
	return out
}

func sortedOptionKeys(m map[string]*OptionPosition) []string {
	out := make([]string, 0, len(m))
	for key, opt := range m {
		if opt != nil {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

func sortedPortfolioScopes(m map[PortfolioScope]*PortfolioRiskState) []PortfolioScope {
	out := make([]PortfolioScope, 0, len(m))
	for scope := range m {
		out = append(out, scope)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedCorrelationScopes(m map[PortfolioScope]*CorrelationSnapshot) []PortfolioScope {
	out := make([]PortfolioScope, 0, len(m))
	for scope := range m {
		out = append(out, scope)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

const tradeStatsExcludedTypesSQL = `('scale_in', 'funding', 'hedge')`

type LifetimeTradeStats struct {
	PositionsOpened int `json:"positions_opened"`
	Wins            int `json:"wins"`
	Losses          int `json:"losses"`
}

func (sdb *StateDB) LifetimeTradeStatsAll() (map[string]LifetimeTradeStats, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	out := make(map[string]LifetimeTradeStats)

	openRows, err := sdb.db.Query(`SELECT strategy_id, COUNT(*)
		FROM trades
		WHERE is_close = 0 AND trade_type NOT IN ` + tradeStatsExcludedTypesSQL + `
		GROUP BY strategy_id`)
	if err != nil {
		return nil, fmt.Errorf("query lifetime open counts: %w", err)
	}
	defer openRows.Close()
	for openRows.Next() {
		var id string
		var opens sql.NullInt64
		if err := openRows.Scan(&id, &opens); err != nil {
			return nil, fmt.Errorf("scan lifetime open counts: %w", err)
		}
		procID := sdb.fromStorageID(id)
		entry := out[procID]
		entry.PositionsOpened = int(opens.Int64)
		out[procID] = entry
	}
	if err := openRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate lifetime open counts: %w", err)
	}

	closeRows, err := sdb.db.Query(`SELECT
			strategy_id,
			SUM(CASE WHEN net_pnl > 0 THEN 1 ELSE 0 END) AS wins,
			SUM(CASE WHEN net_pnl < 0 THEN 1 ELSE 0 END) AS losses
		FROM (
			SELECT
				strategy_id,
				CASE
					WHEN position_id IS NULL OR position_id = ''
					THEN 'legacy:' || rowid
					ELSE position_id
				END AS pkey,
				SUM` + tradeNetPnLSQL + ` AS net_pnl
			FROM trades
			WHERE is_close = 1 AND trade_type NOT IN ` + tradeStatsExcludedTypesSQL + `
			GROUP BY strategy_id, pkey
		)
		GROUP BY strategy_id`)
	if err != nil {
		return nil, fmt.Errorf("query lifetime trade stats: %w", err)
	}
	defer closeRows.Close()
	for closeRows.Next() {
		var id string
		var wins, losses sql.NullInt64
		if err := closeRows.Scan(&id, &wins, &losses); err != nil {
			return nil, fmt.Errorf("scan lifetime trade stats: %w", err)
		}
		procID := sdb.fromStorageID(id)
		entry := out[procID]
		entry.Wins = int(wins.Int64)
		entry.Losses = int(losses.Int64)
		out[procID] = entry
	}
	if err := closeRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate lifetime trade stats: %w", err)
	}
	return out, nil
}

func (sdb *StateDB) LifetimeTradeStatsForStrategy(strategyID string) (LifetimeTradeStats, error) {
	if sdb == nil || sdb.db == nil {
		return LifetimeTradeStats{}, fmt.Errorf("state db unavailable")
	}
	if strategyID == "" {
		return LifetimeTradeStats{}, fmt.Errorf("strategy id required")
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return LifetimeTradeStats{}, err
	}
	var out LifetimeTradeStats
	var opens sql.NullInt64
	if err := sdb.db.QueryRow(`SELECT COUNT(*)
		FROM trades
		WHERE strategy_id = ? AND is_close = 0 AND trade_type NOT IN `+tradeStatsExcludedTypesSQL+``, sid).Scan(&opens); err != nil {
		return LifetimeTradeStats{}, fmt.Errorf("query lifetime open count for %s: %w", strategyID, err)
	}
	out.PositionsOpened = int(opens.Int64)

	var wins, losses sql.NullInt64
	if err := sdb.db.QueryRow(`SELECT
			SUM(CASE WHEN net_pnl > 0 THEN 1 ELSE 0 END) AS wins,
			SUM(CASE WHEN net_pnl < 0 THEN 1 ELSE 0 END) AS losses
		FROM (
			SELECT
				CASE
					WHEN position_id IS NULL OR position_id = ''
					THEN 'legacy:' || rowid
					ELSE position_id
				END AS pkey,
				SUM`+tradeNetPnLSQL+` AS net_pnl
			FROM trades
			WHERE strategy_id = ? AND is_close = 1 AND trade_type NOT IN `+tradeStatsExcludedTypesSQL+`
			GROUP BY pkey
		)`, sid).Scan(&wins, &losses); err != nil {
		return LifetimeTradeStats{}, fmt.Errorf("query lifetime trade stats for %s: %w", strategyID, err)
	}
	out.Wins = int(wins.Int64)
	out.Losses = int(losses.Int64)
	return out, nil
}

func (sdb *StateDB) QueryTradeHistory(strategyID, symbol string, since, until time.Time, limit, offset int) ([]Trade, int, error) {
	var where []string
	var args []interface{}
	if strategyID != "" {
		sid, err := sdb.toStorageID(strategyID)
		if err != nil {
			return nil, 0, err
		}
		where = append(where, "strategy_id = ?")
		args = append(args, sid)
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

	query := fmt.Sprintf("SELECT rowid, timestamp, strategy_id, symbol, COALESCE(position_id, '') AS position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, COALESCE(regime, '') AS regime, COALESCE(entry_atr, 0) AS entry_atr, COALESCE(stop_loss_oid, 0) AS stop_loss_oid, COALESCE(stop_loss_trigger_px, 0) AS stop_loss_trigger_px, COALESCE(tp_oids_json, '') AS tp_oids_json, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source FROM trades %s ORDER BY timestamp DESC, rowid DESC LIMIT ? OFFSET ?", whereClause)
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
		var isCloseInt, pnlGrossInt int
		var tpOIDsJSON string
		var slATRMult sql.NullFloat64
		if err := rows.Scan(&t.sourceRowID, &tsStr, &t.StrategyID, &t.Symbol, &t.PositionID, &t.Side, &t.Quantity, &t.Price, &t.Value, &t.TradeType, &t.Details, &t.ExchangeOrderID, &t.ExchangeFee, &isCloseInt, &t.RealizedPnL, &t.Regime, &t.EntryATR, &t.StopLossOID, &t.StopLossTriggerPx, &tpOIDsJSON, &slATRMult, &t.TPTiersJSON, &pnlGrossInt, &t.FeeSource); err != nil {
			return nil, 0, fmt.Errorf("scan trade: %w", err)
		}
		t.StrategyID = sdb.fromStorageID(t.StrategyID)
		t.sourceRole = sdb.storageRoleOf()
		t.Timestamp = parseTime(tsStr)
		t.IsClose = isCloseInt != 0
		t.PnLGross = pnlGrossInt != 0
		t.TPOIDs = parseTPOIDsJSON(tpOIDsJSON, 0, 0)
		if slATRMult.Valid {
			v := slATRMult.Float64
			t.StopLossATRMult = &v
		}
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

func (sdb *StateDB) QueryTradingViewExportTrades(strategyIDs []string) ([]Trade, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	if len(strategyIDs) == 0 {
		return nil, fmt.Errorf("at least one strategy id is required")
	}
	storageIDs, err := sdb.toStorageIDs(strategyIDs)
	if err != nil {
		return nil, err
	}
	placeholders := make([]string, len(storageIDs))
	args := make([]interface{}, 0, len(storageIDs))
	for i, id := range storageIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := fmt.Sprintf(`SELECT rowid, timestamp, strategy_id, symbol, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee
		FROM trades
		WHERE strategy_id IN (%s)
		ORDER BY timestamp ASC, strategy_id ASC, symbol ASC, rowid ASC`, strings.Join(placeholders, ","))
	rows, err := sdb.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query TradingView export trades: %w", err)
	}
	defer rows.Close()

	var trades []Trade
	for rows.Next() {
		var t Trade
		var tsStr string
		if err := rows.Scan(&t.sourceRowID, &tsStr, &t.StrategyID, &t.Symbol, &t.Side, &t.Quantity, &t.Price, &t.Value, &t.TradeType, &t.Details, &t.ExchangeOrderID, &t.ExchangeFee); err != nil {
			return nil, fmt.Errorf("scan TradingView export trade: %w", err)
		}
		t.StrategyID = sdb.fromStorageID(t.StrategyID)
		t.sourceRole = sdb.storageRoleOf()
		t.Timestamp = parseTime(tsStr)
		trades = append(trades, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate TradingView export trades: %w", err)
	}
	if trades == nil {
		trades = []Trade{}
	}
	return trades, nil
}

type PendingManualAction struct {
	ID                              int64
	StrategyID                      string
	Action                          string
	Symbol                          string
	Side                            string
	Quantity                        float64
	FillPrice                       float64
	FillFee                         float64
	ExchangeOrderID                 string
	StopLossOID                     int64
	StopLossTriggerPx               float64
	EntryATR                        float64
	ATRMethod                       string
	RealizedPnL                     float64
	IsFullClose                     bool
	TPOIDs                          []int64
	RatchetFallbackNormalizePending bool
	CreatedAt                       time.Time

	SourceRole storageRole
}

func (sdb *StateDB) InsertPendingManualAction(a PendingManualAction) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	isFullClose := 0
	if a.IsFullClose {
		isFullClose = 1
	}
	ratchetFallbackNormalizePending := 0
	if a.RatchetFallbackNormalizePending {
		ratchetFallbackNormalizePending = 1
	}
	sid, err := sdb.toStorageID(a.StrategyID)
	if err != nil {
		return err
	}
	_, err = sdb.db.Exec(`INSERT INTO pending_manual_actions
		(strategy_id, action, symbol, side, quantity, fill_price, fill_fee, exchange_order_id, stop_loss_oid, stop_loss_trigger_px, entry_atr, atr_method, realized_pnl, is_full_close, tp_oids_json, ratchet_fallback_normalize_pending, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sid, a.Action, a.Symbol, a.Side, a.Quantity, a.FillPrice, a.FillFee,
		a.ExchangeOrderID, a.StopLossOID, a.StopLossTriggerPx, a.EntryATR, a.ATRMethod, a.RealizedPnL,
		isFullClose, marshalTPOIDsJSON(a.TPOIDs), ratchetFallbackNormalizePending, formatTime(a.CreatedAt))
	return err
}

func (sdb *StateDB) LoadPendingManualActions() ([]PendingManualAction, error) {
	if sdb == nil || sdb.db == nil {
		return nil, nil
	}
	rows, err := sdb.db.Query(`SELECT id, strategy_id, action, symbol, side, quantity, fill_price, fill_fee, exchange_order_id, stop_loss_oid, stop_loss_trigger_px, entry_atr, COALESCE(atr_method, '') AS atr_method, realized_pnl, COALESCE(is_full_close, 0) AS is_full_close, COALESCE(tp_oids_json, '') AS tp_oids_json, COALESCE(ratchet_fallback_normalize_pending, 0) AS ratchet_fallback_normalize_pending, created_at FROM pending_manual_actions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("load pending manual actions: %w", err)
	}
	defer rows.Close()
	var actions []PendingManualAction
	for rows.Next() {
		var a PendingManualAction
		var createdStr string
		var isFullCloseInt int
		var tpOIDsJSON string
		var ratchetFallbackNormalizePending int
		if err := rows.Scan(&a.ID, &a.StrategyID, &a.Action, &a.Symbol, &a.Side, &a.Quantity, &a.FillPrice, &a.FillFee, &a.ExchangeOrderID, &a.StopLossOID, &a.StopLossTriggerPx, &a.EntryATR, &a.ATRMethod, &a.RealizedPnL, &isFullCloseInt, &tpOIDsJSON, &ratchetFallbackNormalizePending, &createdStr); err != nil {
			return nil, fmt.Errorf("scan pending manual action: %w", err)
		}
		a.IsFullClose = isFullCloseInt != 0
		a.TPOIDs = parseTPOIDsJSON(tpOIDsJSON, 0, 0)
		a.RatchetFallbackNormalizePending = ratchetFallbackNormalizePending != 0
		a.CreatedAt = parseTime(createdStr)
		a.StrategyID = sdb.fromStorageID(a.StrategyID)
		a.SourceRole = sdb.storageRoleOf()
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

// deletePendingManualActionsByID acknowledges exactly the actions named, inside
// the transaction that persists their effect. A failed action keeps its row even
// when a later action succeeds, in this file or in the other one.
func deletePendingManualActionsByID(exec sqlExecer, ids []int64) error {
	if exec == nil || len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if _, err := exec.Exec("DELETE FROM pending_manual_actions WHERE id = ?", id); err != nil {
			return fmt.Errorf("acknowledge pending manual action %d: %w", id, err)
		}
	}
	return nil
}

func (sdb *StateDB) DeletePendingManualActionsByID(ids []int64) error {
	if sdb == nil || sdb.db == nil {
		return nil
	}
	return deletePendingManualActionsByID(sdb.db, ids)
}

type PendingLimitOrder struct {
	ID              int64
	StrategyID      string
	Symbol          string
	Side            string
	OrderOID        int64
	LimitPrice      float64
	OrderSize       float64
	TIF             string
	FilledSize      float64
	AvgFillPrice    float64
	FillFee         float64
	EntryATR        float64
	CancelRequested bool

	OperatorRequiredSince time.Time

	ExpiresAt time.Time
	CreatedAt time.Time
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (sdb *StateDB) InsertPendingLimitOrder(o PendingLimitOrder) (int64, error) {
	if sdb == nil || sdb.db == nil {
		return 0, fmt.Errorf("state db unavailable")
	}
	expiresStr := ""
	if !o.ExpiresAt.IsZero() {
		expiresStr = formatTime(o.ExpiresAt.UTC())
	}
	sid, err := sdb.toStorageID(o.StrategyID)
	if err != nil {
		return 0, err
	}
	res, err := sdb.db.Exec(`INSERT INTO pending_limit_orders
		(strategy_id, symbol, side, order_oid, limit_price, order_size, tif, filled_size, avg_fill_price, fill_fee, entry_atr, cancel_requested, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sid, o.Symbol, o.Side, o.OrderOID, o.LimitPrice, o.OrderSize, o.TIF,
		o.FilledSize, o.AvgFillPrice, o.FillFee, o.EntryATR, boolToInt(o.CancelRequested),
		expiresStr, formatTime(o.CreatedAt.UTC()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (sdb *StateDB) LoadPendingLimitOrders() ([]PendingLimitOrder, error) {
	if sdb == nil || sdb.db == nil {
		return nil, nil
	}
	rows, err := sdb.db.Query(`SELECT id, strategy_id, symbol, side, order_oid, limit_price, order_size, tif, filled_size, avg_fill_price, fill_fee, entry_atr, cancel_requested, operator_required_since, expires_at, created_at FROM pending_limit_orders ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("load pending limit orders: %w", err)
	}
	defer rows.Close()
	var orders []PendingLimitOrder
	for rows.Next() {
		var o PendingLimitOrder
		var cancelInt int
		var operatorStr, expiresStr, createdStr string
		if err := rows.Scan(&o.ID, &o.StrategyID, &o.Symbol, &o.Side, &o.OrderOID, &o.LimitPrice, &o.OrderSize, &o.TIF, &o.FilledSize, &o.AvgFillPrice, &o.FillFee, &o.EntryATR, &cancelInt, &operatorStr, &expiresStr, &createdStr); err != nil {
			return nil, fmt.Errorf("scan pending limit order: %w", err)
		}
		o.CancelRequested = cancelInt != 0
		if operatorStr != "" {
			o.OperatorRequiredSince = parseTime(operatorStr)
		}
		if expiresStr != "" {
			o.ExpiresAt = parseTime(expiresStr)
		}
		o.CreatedAt = parseTime(createdStr)
		o.StrategyID = sdb.fromStorageID(o.StrategyID)
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

func (sdb *StateDB) UpdatePendingLimitOrderFill(id int64, filledSize, avgFillPrice, fillFee float64) error {
	if sdb == nil || sdb.db == nil {
		return nil
	}
	_, err := sdb.db.Exec(
		"UPDATE pending_limit_orders SET filled_size = ?, avg_fill_price = ?, fill_fee = ? WHERE id = ?",
		filledSize, avgFillPrice, fillFee, id)
	return err
}

func (sdb *StateDB) MarkPendingLimitOrderCancelRequested(strategyID, symbol string) (int64, error) {
	if sdb == nil || sdb.db == nil {
		return 0, fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return 0, err
	}
	res, err := sdb.db.Exec(
		"UPDATE pending_limit_orders SET cancel_requested = 1 WHERE strategy_id = ? AND symbol = ?",
		sid, symbol)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (sdb *StateDB) MarkPendingLimitOrderOperatorRequired(id int64, at time.Time) error {
	if sdb == nil || sdb.db == nil {
		return nil
	}
	_, err := sdb.db.Exec(
		"UPDATE pending_limit_orders SET operator_required_since = ? WHERE id = ?",
		formatTime(at.UTC()), id)
	return err
}

func (sdb *StateDB) ClearPendingLimitOrderOperatorRequired(id int64) error {
	if sdb == nil || sdb.db == nil {
		return nil
	}
	_, err := sdb.db.Exec(
		"UPDATE pending_limit_orders SET operator_required_since = '' WHERE id = ?", id)
	return err
}

func (sdb *StateDB) DeletePendingLimitOrder(id int64) error {
	if sdb == nil || sdb.db == nil {
		return nil
	}
	_, err := sdb.db.Exec("DELETE FROM pending_limit_orders WHERE id = ?", id)
	return err
}

func (sdb *StateDB) CountPendingLimitOrders(strategyID, symbol string) (int, error) {
	if sdb == nil || sdb.db == nil {
		return 0, nil
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return 0, err
	}
	var n int
	err = sdb.db.QueryRow(
		"SELECT COUNT(*) FROM pending_limit_orders WHERE strategy_id = ? AND symbol = ?",
		sid, symbol).Scan(&n)
	return n, err
}

func (sdb *StateDB) EarliestTradeTimestamp(strategyIDs []string) (time.Time, error) {
	if sdb == nil || sdb.db == nil {
		return time.Time{}, fmt.Errorf("state db unavailable")
	}
	if len(strategyIDs) == 0 {
		return time.Time{}, nil
	}
	storageIDs, err := sdb.toStorageIDs(strategyIDs)
	if err != nil {
		return time.Time{}, err
	}
	placeholders := make([]string, len(storageIDs))
	args := make([]interface{}, len(storageIDs))
	for i, id := range storageIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := fmt.Sprintf(
		"SELECT MIN(timestamp) FROM trades WHERE strategy_id IN (%s) AND timestamp != ''",
		strings.Join(placeholders, ","),
	)
	var ts sql.NullString
	if err := sdb.db.QueryRow(query, args...).Scan(&ts); err != nil {
		return time.Time{}, fmt.Errorf("earliest trade timestamp: %w", err)
	}
	if !ts.Valid || ts.String == "" {
		return time.Time{}, nil
	}
	return parseTime(ts.String), nil
}

func (sdb *StateDB) ListTradesForBackfill(strategyID string) ([]TradeBackfillRow, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return nil, err
	}
	rows, err := sdb.db.Query(`
		SELECT rowid, timestamp, symbol, COALESCE(position_id, '') AS position_id,
		       side, quantity, price, value, trade_type, details, is_close, exchange_order_id, exchange_fee, realized_pnl,
		       COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source
		FROM trades
		WHERE strategy_id = ?
		ORDER BY timestamp ASC, rowid ASC`, sid)
	if err != nil {
		return nil, fmt.Errorf("list trades for backfill: %w", err)
	}
	defer rows.Close()
	var out []TradeBackfillRow
	for rows.Next() {
		var t TradeBackfillRow
		var tsStr string
		var isCloseInt, pnlGrossInt int
		if err := rows.Scan(&t.RowID, &tsStr, &t.Symbol, &t.PositionID, &t.Side, &t.Quantity, &t.Price, &t.Value, &t.TradeType, &t.Details, &isCloseInt,
			&t.ExchangeOrderID, &t.ExchangeFee, &t.RealizedPnL, &pnlGrossInt, &t.FeeSource); err != nil {
			return nil, fmt.Errorf("scan trade: %w", err)
		}
		t.Timestamp = parseTime(tsStr)
		t.IsClose = isCloseInt != 0
		t.PnLGross = pnlGrossInt != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

type ClosedPositionRow struct {
	ID          int64
	Symbol      string
	ClosedAt    time.Time
	RealizedPnL float64
}

func (sdb *StateDB) LoadClosedPositionRows(strategyID string) ([]ClosedPositionRow, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.toStorageID(strategyID)
	if err != nil {
		return nil, err
	}
	rows, err := sdb.db.Query(`
		SELECT id, symbol, closed_at, realized_pnl
		FROM closed_positions
		WHERE strategy_id = ?
		ORDER BY closed_at ASC, id ASC`, sid)
	if err != nil {
		return nil, fmt.Errorf("load closed_positions: %w", err)
	}
	defer rows.Close()
	var out []ClosedPositionRow
	for rows.Next() {
		var r ClosedPositionRow
		var tsStr string
		if err := rows.Scan(&r.ID, &r.Symbol, &tsStr, &r.RealizedPnL); err != nil {
			return nil, fmt.Errorf("scan closed_positions row: %w", err)
		}
		r.ClosedAt = parseTime(tsStr)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (sdb *StateDB) ApplyBackfillPlan(plan BackfillPlan) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	sid, err := sdb.resolvePlanStorageID(plan.Role, plan.StorageStrategyID, plan.StrategyID)
	if err != nil {
		return err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	tradeStmt, err := tx.Prepare(
		"UPDATE trades SET exchange_fee = ?, realized_pnl = ? WHERE rowid = ?",
	)
	if err != nil {
		return fmt.Errorf("prepare trade update: %w", err)
	}
	defer tradeStmt.Close()
	for _, c := range plan.TradeChanges {
		if _, err := tradeStmt.Exec(c.NewFee, c.NewRealizedPnL, c.RowID); err != nil {
			return fmt.Errorf("update trade rowid=%d: %w", c.RowID, err)
		}
	}

	cpStmt, err := tx.Prepare(
		"UPDATE closed_positions SET realized_pnl = ? WHERE id = ? AND strategy_id = ?",
	)
	if err != nil {
		return fmt.Errorf("prepare closed_positions update: %w", err)
	}
	defer cpStmt.Close()
	for _, cp := range plan.ClosedPositions {
		if _, err := cpStmt.Exec(cp.NewPnL, cp.RowID, sid); err != nil {
			return fmt.Errorf("update closed_positions id=%d: %w", cp.RowID, err)
		}
	}

	if _, err := tx.Exec("UPDATE strategies SET cash = ? WHERE id = ?", plan.NewCash, sid); err != nil {
		return fmt.Errorf("update strategy cash: %w", err)
	}

	return tx.Commit()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
