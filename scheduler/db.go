package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// initialCapitalGuardWarn is the operator-visible hook for #343 baseline-guard
// warnings. main.go wires it to the owner DM after MultiNotifier is built so
// silent overwrites surface beyond stderr. Nil-safe: SaveState falls back to
// stderr-only when the hook isn't set (early boot, tests).
var initialCapitalGuardWarn func(msg string)

// initialCapitalGuardWarned dedups baseline-guard warnings to one per strategy
// per process lifetime. Without this the per-cycle SaveState would re-emit the
// same warning forever once config drifts from the persisted baseline. Cleared
// on restart so a still-broken caller is re-flagged after redeploy.
var initialCapitalGuardWarned sync.Map // map[string]struct{}, key = strategy ID

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
    active_profile TEXT NOT NULL DEFAULT ''
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
-- idx_trades_close (#455) and idx_trades_strategy_position (#471) are created
-- in migrateSchema, not here, so legacy DBs add columns before indexes
-- reference them.

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
    warning_margin_delta_pct REAL NOT NULL DEFAULT 0
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
		// Portfolio warning diagnostics (#904).
		"ALTER TABLE portfolio_risk ADD COLUMN warn_band_entered_at TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE portfolio_risk ADD COLUMN last_warning_equity_dd_pct REAL NOT NULL DEFAULT 0",
		"ALTER TABLE portfolio_risk ADD COLUMN last_warning_margin_dd_pct REAL NOT NULL DEFAULT 0",
		"ALTER TABLE portfolio_risk ADD COLUMN warning_equity_delta_pct REAL NOT NULL DEFAULT 0",
		"ALTER TABLE portfolio_risk ADD COLUMN warning_margin_delta_pct REAL NOT NULL DEFAULT 0",
		// Per-leaderboard-summary last-post timestamps stored as JSON (#308).
		"ALTER TABLE app_state ADD COLUMN last_leaderboard_summaries TEXT NOT NULL DEFAULT ''",
		// Per-channel regular summary last-post timestamps stored as JSON (#474).
		"ALTER TABLE app_state ADD COLUMN last_summary_post TEXT NOT NULL DEFAULT ''",
		// Per-trade HL stop-loss trigger OID (#412).
		"ALTER TABLE positions ADD COLUMN stop_loss_oid INTEGER NOT NULL DEFAULT 0",
		// Per-trade HL stop-loss trigger price for later-fill reconciliation (#421).
		"ALTER TABLE positions ADD COLUMN stop_loss_trigger_px REAL NOT NULL DEFAULT 0",
		// Trailing SL high/low-water mark while the position is open (#501).
		"ALTER TABLE positions ADD COLUMN stop_loss_high_water_px REAL NOT NULL DEFAULT 0",
		// Lifetime round-trip / win-loss tracking (#455). is_close marks closing
		// legs; realized_pnl carries the per-trade realized PnL.
		"ALTER TABLE trades ADD COLUMN is_close INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN realized_pnl REAL NOT NULL DEFAULT 0",
		"CREATE INDEX IF NOT EXISTS idx_trades_close ON trades(strategy_id, is_close)",
		// Per-position trade grouping (#471).
		"ALTER TABLE trades ADD COLUMN position_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN position_id TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE option_positions ADD COLUMN position_id TEXT NOT NULL DEFAULT ''",
		"CREATE INDEX IF NOT EXISTS idx_trades_strategy_position ON trades(strategy_id, position_id)",
		// Position-aware close evaluator context (#496).
		"ALTER TABLE positions ADD COLUMN initial_quantity REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN entry_atr REAL NOT NULL DEFAULT 0",
		// Market regime label at trade time (#482).
		"ALTER TABLE trades ADD COLUMN regime TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE trades ADD COLUMN entry_atr REAL NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN stop_loss_trigger_px REAL NOT NULL DEFAULT 0",
		// Manual trade flag (#569).
		"ALTER TABLE trades ADD COLUMN manual INTEGER NOT NULL DEFAULT 0",
		// Operator-intent full-close flag for manual close actions (#569 review).
		"ALTER TABLE pending_manual_actions ADD COLUMN is_full_close INTEGER NOT NULL DEFAULT 0",
		// Per-strategy HL reduce-only take-profit OIDs (#601).
		"ALTER TABLE positions ADD COLUMN tp1_oid INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN tp2_oid INTEGER NOT NULL DEFAULT 0",
		// Variable-length per-strategy HL reduce-only take-profit OIDs (#612).
		"ALTER TABLE positions ADD COLUMN tp_oids_json TEXT NOT NULL DEFAULT ''",
		// Inline TP OIDs for manual-open actions so the scheduler drain sets pos.TPOIDs (#632).
		"ALTER TABLE pending_manual_actions ADD COLUMN tp_oids_json TEXT NOT NULL DEFAULT ''",
		// SL arming method + TP tier snapshot at fill time (#669). stop_loss_atr_mult
		// stays nullable: NULL = legacy/unknown or non-ATR arming (pct/margin/trailing-pct);
		// non-NULL = ATR-armed at the recorded multiplier. tp_tiers_json carries the
		// full tier snapshot ([{atr_multiple,close_fraction},...]) so historical
		// tier-config edits don't erase the record. Both columns are additive — no
		// backfill of legacy rows.
		"ALTER TABLE trades ADD COLUMN stop_loss_atr_mult REAL",
		"ALTER TABLE trades ADD COLUMN tp_tiers_json TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN stop_loss_atr_mult REAL",
		"ALTER TABLE positions ADD COLUMN tp_tiers_json TEXT NOT NULL DEFAULT ''",
		// Immutable trade-row snapshot of protection OIDs known at open time (#674).
		"ALTER TABLE trades ADD COLUMN stop_loss_oid INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN tp_oids_json TEXT NOT NULL DEFAULT ''",
		// Post-TP stop-loss adjustment watermark + post-TP trailing distance (#708).
		// sl_adjusted_tiers_processed counts how many leading tiers have been
		// processed; 0 = none, matching the Go zero value so the column never
		// needs a backfill for legacy rows.
		"ALTER TABLE positions ADD COLUMN sl_adjusted_tiers_processed INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN post_tp_trailing_atr_mult REAL",
		// Per-tier "was ever armed" tracking so a tier whose first placement
		// failed (OID=0, never armed) can be distinguished from a tier that
		// filled (OID=0 after Python zeros it). Empty string = legacy row;
		// findHighestClearedTier degrades to legacy behavior for those (#716).
		"ALTER TABLE positions ADD COLUMN tp_armed_tiers_json TEXT NOT NULL DEFAULT ''",
		// Multi-window regime stamps at position open (#792).
		"ALTER TABLE positions ADD COLUMN regime TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN regime_windows_json TEXT NOT NULL DEFAULT ''",
		// #843 dynamic close confirm-cycle state.
		"ALTER TABLE positions ADD COLUMN regime_pending_label TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN regime_pending_count INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN regime_applied_label TEXT NOT NULL DEFAULT ''",
		// #873 scale-in / pyramiding per-position state.
		"ALTER TABLE positions ADD COLUMN scale_in_count INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN last_add_price REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN added_notional_usd REAL NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN risk_anchor_price REAL NOT NULL DEFAULT 0",
		// #873: durable resize-pending flag so a restart between an add and the
		// deferred trailing-SL re-size still grows the on-chain stop next cycle.
		"ALTER TABLE positions ADD COLUMN scale_in_resize_pending INTEGER NOT NULL DEFAULT 0",
		// #1121: durable one-shot marker so a manual-open ratchet fallback SL can
		// widen once to the configured per-regime trail after the daemon stamps regime.
		"ALTER TABLE positions ADD COLUMN ratchet_fallback_normalize_pending INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE pending_manual_actions ADD COLUMN ratchet_fallback_normalize_pending INTEGER NOT NULL DEFAULT 0",
		// #954 gross PnL convention marker + fee provenance. pnl_gross=1 rows
		// store the PRE-FEE realized PnL in realized_pnl with the deducted fee
		// always stamped in exchange_fee; legacy rows (0) store net PnL and
		// stamp exchange_fee only when a real fill fee was captured. All sums
		// must go through tradeNetPnLSQL / tradeLedgerDeltaSQL (trade_pnl.go)
		// so the two conventions never mix. fee_source records provenance:
		// 'userfills' (real exchange fee) vs 'modeled' (taker-rate estimate) —
		// `backfill trade-ledger` targets modeled rows for repair.
		"ALTER TABLE trades ADD COLUMN pnl_gross INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE trades ADD COLUMN fee_source TEXT NOT NULL DEFAULT ''",
		// #998: regime-profile allocation persistence. open_profile freezes the
		// profile for the life of a position; active_profile keeps the flat
		// switch state across restarts.
		"ALTER TABLE positions ADD COLUMN open_profile TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE strategies ADD COLUMN active_profile TEXT NOT NULL DEFAULT ''",
		// #1085: freeze the directional-certification verdict at open so it
		// survives restarts — without this an open CERTIFIED position reloads as
		// uncertified (pos.Regime persists, so the open-time stamp is never
		// re-derived) and gets migrated to base direction (req-2 violation).
		"ALTER TABLE positions ADD COLUMN direction_certified_at_open INTEGER NOT NULL DEFAULT 0",
		// #1085 (per-state sign gate): freeze the certified PER-STATE direction
		// map at open so an open position's sign gating (hold-on-transition AND the
		// #822 orphan check) uses the open-time evidence, immune to a live-artifact
		// change (req 2). Empty string = uncertified at open → base direction.
		"ALTER TABLE positions ADD COLUMN direction_certified_states_json TEXT NOT NULL DEFAULT ''",
		// #1137: LLM entry-analysis idempotency marker + completed verdict.
		// Both persist so a restart never re-dispatches an analysis and a
		// finished verdict survives to the close-time diagnostics row.
		"ALTER TABLE positions ADD COLUMN llm_analysis_requested INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE positions ADD COLUMN llm_verdict TEXT NOT NULL DEFAULT ''",
		// #1277 optional hardening: freeze the resolved atr_method at open so
		// checkATRMethodDriftAtStartup can detect a config edit + restart (not
		// SIGHUP) that changed the effective method while the position stayed
		// open — a gap the SIGHUP hot-reload guard can't see. "" = pre-#1277
		// position, never stamped.
		"ALTER TABLE positions ADD COLUMN atr_method_at_open TEXT NOT NULL DEFAULT ''",
		// #1277 hardening (review round 2): manual opens resolve atr_method at
		// queue time (next to the EntryATR fetch in manualOpenCore) and carry
		// it through the pending queue so the drain stamps the method the ATR
		// was actually computed under — a drain-time re-resolve would mask a
		// config edit + restart landing between queue and drain. "" = row
		// queued pre-upgrade; the drain falls back to drain-time resolution.
		"ALTER TABLE pending_manual_actions ADD COLUMN atr_method TEXT NOT NULL DEFAULT ''",
		// #1159 correlated hedge legs: hedge_for marks a position as the
		// auto-managed hedge leg for the named primary symbol (persisted so
		// startup reconcile recovers ownership without coin→symbol inference);
		// hedge_primary_qty_basis is the primary-qty watermark hedge sync diffs.
		"ALTER TABLE positions ADD COLUMN hedge_for TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE positions ADD COLUMN hedge_primary_qty_basis REAL NOT NULL DEFAULT 0",
	}
	for _, ddl := range migrations {
		if _, err := sdb.db.Exec(ddl); err != nil {
			msg := err.Error()
			// "duplicate column name" means the column already exists — skip
			// (ADD COLUMN idempotency).
			if strings.Contains(msg, "duplicate column") {
				continue
			}
			return err
		}
	}
	if err := sdb.migratePendingCircuitClosesColumn(); err != nil {
		return err
	}
	return sdb.backfillTradeCloseFlags()
}

func firstTwoTPOIDs(oids []int64) (int64, int64) {
	// TODO(#612): drop legacy tp1_oid/tp2_oid writes after one release once
	// all deployed binaries read tp_oids_json.
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

// marshalStringMapJSON / parseStringMapJSON are the generic map[string]string
// JSON codecs for persisted snapshot maps that aren't regime windows (e.g. the
// #1085 frozen certified per-state direction map). Empty/unparseable -> "".
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

// marshalTPArmedTiersJSON / parseTPArmedTiersJSON persist Position.TPArmedTiers
// (#716 item 2). An empty slice marshals to the empty string so legacy DB
// rows (column added with an empty default) round-trip cleanly.
// parseTPArmedTiersJSON returns nil for empty or malformed input; callers
// treat nil as a legacy row and rely on backfillTPArmedTiers below to
// assume any tier with a positive OID was armed.
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

// backfillTPArmedTiers infers the armed state of legacy positions loaded from
// rows persisted before tp_armed_tiers_json existed. A pre-#716 row carries
// pos.TPArmedTiers == nil; we assume any tier with a positive OID at load time
// was successfully armed at some point. Tiers with OID=0 on a legacy row are
// left unarmed — conservative: better to skip a real fill than to fire on a
// never-armed tier.
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

// backfillTradeCloseFlags is a one-time best-effort backfill (#455) for
// pre-existing rows in the trades table that lack is_close/realized_pnl.
// New rows always insert with explicit values, so this only runs against
// rows where is_close=0 AND realized_pnl=0 — rows already populated by a
// fresh insert won't be touched.
//
// The heuristic looks at the Details string (the only structured signal
// available on legacy rows): close legs always include "PnL" (some sites
// use "PnL: $X.XX", others "PnL=$X.XX"), and "expired ITM" identifies
// option assignments / call-aways. We extract the realized PnL via the
// shared regex and flip is_close=1 for matched rows. Best-effort: a row
// whose Details was truncated or never carried a PnL substring stays at
// is_close=0 (and undercounts by the same margin as the legacy in-memory
// counters did pre-#455).
//
// Known asymmetry: the HL on-chain "no virtual position" branch emitted
// "Circuit breaker on-chain close (no virtual position), fill=… fee=$…"
// in its Details — no PnL token. Pre-#455 rows from that branch therefore
// stay is_close=0 here, while post-#455 rows from the same branch land
// is_close=1, realized_pnl=0 (written directly by hyperliquid_balance.go).
func (sdb *StateDB) backfillTradeCloseFlags() error {
	// Only flag rows that haven't been touched. Detect close trades by
	// the "PnL" substring (covers "PnL: $X" and "PnL=$X" forms) — this
	// matches every Details string emitted by a close-leg RecordTrade
	// call site at the time of #455.
	// #954: gross-convention rows are excluded outright. A zero-gross close
	// (no-mark-price AvgCost booking, exact breakeven) legitimately has
	// realized_pnl=0 with a "PnL: $..." Details token carrying the NET value;
	// parsing that into realized_pnl while pnl_gross=1 stays set would make
	// tradeNetPnL subtract the fee twice. This migration is legacy-rows-only
	// by definition.
	_, err := sdb.db.Exec(`UPDATE trades SET is_close = 1
		WHERE is_close = 0 AND realized_pnl = 0 AND COALESCE(pnl_gross, 0) = 0 AND details LIKE '%PnL%'`)
	if err != nil {
		return fmt.Errorf("backfill is_close: %w", err)
	}
	// Parse the realized PnL out of the Details string. SQLite lacks
	// regexp by default, so we walk the rows in Go. Restrict to rows that
	// have both is_close=1 and a "PnL" token: realized_pnl=0 rows without
	// a PnL substring (e.g. the HL-fallback "no virtual position" branch)
	// can never match parseDetailsPnL and would be re-scanned every boot
	// otherwise.
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

// pnlPattern matches the realized-PnL substring emitted by close-leg
// RecordTrade Details strings: "PnL: $-1.23", "PnL=$4.56", "PnL: 7.89".
// Both colon and equals are accepted; the dollar sign and sign are
// optional. Whitespace between "PnL" and the value is tolerated.
var pnlPattern = regexp.MustCompile(`PnL\s*[:=]\s*\$?(-?\d+(?:\.\d+)?)`)

// parseDetailsPnL extracts the realized PnL value from a trade Details
// string. Returns (0, false) when no PnL token is present. Used by the
// #455 backfill to populate realized_pnl on legacy rows.
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

// migratePendingCircuitClosesColumn handles the #356/#359 pending-close column
// across its three possible DB states, gated on a PRAGMA table_info lookup so
// this is a true fixed point under repeated startups:
//
//   - Pre-#356 DB (neither column): ADD COLUMN risk_pending_circuit_closes_json.
//   - Post-#356, pre-#359 DB (legacy column only): RENAME to the new name.
//   - Post-#359 DB (new column only): no-op.
//
// The earlier version unconditionally ran ADD COLUMN + RENAME, which re-added
// a ghost risk_pending_hl_close_json on every post-rename startup (PR #365
// review). CREATE TABLE uses the legacy name so fresh installs land in the
// pre-#359 branch and get renamed; keeping CREATE TABLE untouched avoids a
// schema fork between fresh installs and migrated DBs.
func (sdb *StateDB) migratePendingCircuitClosesColumn() error {
	hasLegacy, hasNew, err := sdb.strategiesColumnPresence()
	if err != nil {
		return fmt.Errorf("introspect strategies columns: %w", err)
	}
	switch {
	case hasNew:
		// Already migrated (or legacy column somehow lingers alongside — the
		// app only reads/writes the new column, so leave as-is rather than
		// risk a destructive DROP COLUMN).
		return nil
	case hasLegacy:
		_, err := sdb.db.Exec("ALTER TABLE strategies RENAME COLUMN risk_pending_hl_close_json TO risk_pending_circuit_closes_json")
		return err
	default:
		_, err := sdb.db.Exec("ALTER TABLE strategies ADD COLUMN risk_pending_circuit_closes_json TEXT NOT NULL DEFAULT ''")
		return err
	}
}

// strategiesColumnPresence reports whether the strategies table currently has
// the legacy (#356) and/or generalized (#359) pending-circuit-close columns.
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
	isClose := 0
	if trade.IsClose {
		isClose = 1
	}
	isManual := 0
	if trade.Manual {
		isManual = 1
	}
	_, err := sdb.db.Exec(`INSERT INTO trades
			(strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, regime, entry_atr, stop_loss_oid, stop_loss_trigger_px, tp_oids_json, manual, stop_loss_atr_mult, tp_tiers_json, pnl_gross, fee_source)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strategyID, formatTime(trade.Timestamp), trade.Symbol, trade.PositionID, trade.Side,
		trade.Quantity, trade.Price, trade.Value, trade.TradeType, trade.Details,
		trade.ExchangeOrderID, trade.ExchangeFee, isClose, trade.RealizedPnL, trade.Regime,
		trade.EntryATR, trade.StopLossOID, trade.StopLossTriggerPx, marshalTPOIDsJSON(trade.TPOIDs), isManual,
		nullableFloat64(trade.StopLossATRMult), trade.TPTiersJSON, boolToInt(trade.PnLGross), trade.FeeSource)
	if err != nil {
		return fmt.Errorf("insert trade for %s: %w", strategyID, err)
	}
	return nil
}

// RecentTrades returns the newest trade rows since a cutoff, newest first.
func (sdb *StateDB) RecentTrades(since time.Time, limit int) ([]Trade, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	if limit <= 0 {
		return nil, nil
	}
	rows, err := sdb.db.Query(`SELECT timestamp, strategy_id, symbol, COALESCE(position_id, '') AS position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, COALESCE(regime, '') AS regime, COALESCE(entry_atr, 0) AS entry_atr, COALESCE(stop_loss_oid, 0) AS stop_loss_oid, COALESCE(stop_loss_trigger_px, 0) AS stop_loss_trigger_px, COALESCE(tp_oids_json, '') AS tp_oids_json, COALESCE(manual, 0) AS manual, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source
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
		if err := rows.Scan(&tsStr, &tr.StrategyID, &tr.Symbol, &tr.PositionID, &tr.Side, &tr.Quantity, &tr.Price, &tr.Value, &tr.TradeType, &tr.Details, &tr.ExchangeOrderID, &tr.ExchangeFee, &isCloseInt, &tr.RealizedPnL, &tr.Regime, &tr.EntryATR, &tr.StopLossOID, &tr.StopLossTriggerPx, &tpOIDsJSON, &isManualInt, &slATRMult, &tr.TPTiersJSON, &pnlGrossInt, &tr.FeeSource); err != nil {
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
		tr.persisted = true
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent trades: %w", err)
	}
	return out, nil
}

// RecentTradesForStrategy returns the newest trade rows for one strategy.
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
	rows, err := sdb.db.Query(`SELECT timestamp, strategy_id, symbol, COALESCE(position_id, '') AS position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, COALESCE(regime, '') AS regime, COALESCE(entry_atr, 0) AS entry_atr, COALESCE(stop_loss_oid, 0) AS stop_loss_oid, COALESCE(stop_loss_trigger_px, 0) AS stop_loss_trigger_px, COALESCE(tp_oids_json, '') AS tp_oids_json, COALESCE(manual, 0) AS manual, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source
		FROM trades WHERE strategy_id = ? ORDER BY timestamp DESC, rowid DESC LIMIT ?`, strategyID, limit)
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
		tr.persisted = true
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent trades for %s: %w", strategyID, err)
	}
	return out, nil
}

// UpdateTradeStampedFields updates open-trade snapshot fields on an existing
// trade row identified by (strategy_id, timestamp). The normal same-cycle open
// path writes these fields in InsertTrade; this remains for fallback arming
// after the open row already exists.
func (sdb *StateDB) UpdateTradeStampedFields(strategyID string, ts time.Time, entryATR float64, stopLossOID int64, stopLossTriggerPx float64, tpOIDs []int64, stopLossATRMult *float64, tpTiersJSON string) error {
	_, err := sdb.db.Exec(
		`UPDATE trades SET entry_atr = ?, stop_loss_oid = ?, stop_loss_trigger_px = ?, tp_oids_json = ?, stop_loss_atr_mult = ?, tp_tiers_json = ? WHERE strategy_id = ? AND timestamp = ?`,
		entryATR, stopLossOID, stopLossTriggerPx, marshalTPOIDsJSON(tpOIDs), nullableFloat64(stopLossATRMult), tpTiersJSON, strategyID, formatTime(ts),
	)
	return err
}

// nullableFloat64 returns a *float64 unchanged for use with database/sql so a
// nil pointer maps to SQL NULL while a non-nil pointer's value is bound. The
// helper exists purely as a callsite-readability anchor — passing the *float64
// directly works the same way under database/sql but obscures intent.
func nullableFloat64(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

// nullableString mirrors nullableFloat64 for *string columns (NULL when nil).
func nullableString(v *string) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

// SetInitialCapital is the ONLY sanctioned way to change a strategy's
// initial_capital baseline (#343). All other write paths go through SaveState,
// which preserves the existing baseline. Callers are expected to be an
// explicit user command (CLI flag, admin script, config-drift reconciler at
// startup), not normal runtime state persistence.
//
// Concurrency: runs inside a transaction so a concurrent SaveState observes
// either the pre- or post-override value, never an interleaved snapshot. With
// SQLite's single-writer model and SetMaxOpenConns(1), a SaveState already in
// progress will serialize behind this update.
//
// In-memory caveat: this only updates the persisted row. Any AppState already
// in memory still holds the stale value until reloaded — risk/PnL calculations
// that fire before the next process restart (or in-place reload of state) will
// continue to use the pre-override baseline. The startup config-drift path in
// main.go handles this by mutating in-memory state alongside the DB write;
// callers invoking this directly mid-run must do the same or accept the gap.
func (sdb *StateDB) SetInitialCapital(strategyID string, value float64) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if value <= 0 {
		return fmt.Errorf("initial_capital must be > 0, got %g", value)
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE strategies SET initial_capital = ? WHERE id = ?", value, strategyID)
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
	// Allow the guard to fire again for this strategy if a future SaveState
	// still tries to revert the new baseline — the override is a fresh state
	// of the world.
	initialCapitalGuardWarned.Delete(strategyID)
	fmt.Fprintf(os.Stderr, "[state] initial_capital override for %s set to $%.2f (#343)\n", strategyID, value)
	return nil
}

// SaveState writes the full AppState to SQLite within a single transaction.
//
// Side effect (#343): when the in-memory StrategyState carries an
// initial_capital that disagrees with the persisted baseline, SaveState
// rewrites the in-memory field to match the persisted value. Callers should
// not rely on the post-save struct holding their original value — the
// persisted baseline is treated as the source of truth. Use
// StateDB.SetInitialCapital to change a baseline.
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
		var existing float64
		if err := existingRows.Scan(&id, &existing); err != nil {
			existingRows.Close()
			return fmt.Errorf("scan existing initial_capital: %w", err)
		}
		existingInitCaps[id] = existing
	}
	// rows.Next() returns false on both exhaustion and mid-iteration error;
	// without this Err() check a transient SQLite failure would yield a
	// silently-incomplete snapshot and leave un-snapshotted strategies
	// unprotected by the baseline guard for this save cycle.
	if err := existingRows.Err(); err != nil {
		existingRows.Close()
		return fmt.Errorf("iterate existing initial_capital: %w", err)
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
		risk_circuit_breaker, risk_circuit_breaker_until, risk_pending_circuit_closes_json, active_profile)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare strategy insert: %w", err)
	}
	defer stmtStrat.Close()

	stmtPos, err := tx.Prepare(`INSERT INTO positions (strategy_id, symbol, position_id, quantity, initial_quantity, avg_cost, entry_atr, side, multiplier, owner_strategy_id, opened_at, stop_loss_oid, stop_loss_trigger_px, stop_loss_high_water_px, tp1_oid, tp2_oid, tp_oids_json, tp_armed_tiers_json, stop_loss_atr_mult, tp_tiers_json, sl_adjusted_tiers_processed, post_tp_trailing_atr_mult, regime, regime_windows_json, regime_pending_label, regime_pending_count, regime_applied_label, scale_in_count, last_add_price, added_notional_usd, risk_anchor_price, scale_in_resize_pending, ratchet_fallback_normalize_pending, open_profile, direction_certified_at_open, direction_certified_states_json, llm_analysis_requested, llm_verdict, atr_method_at_open, hedge_for, hedge_primary_qty_basis)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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

	for _, s := range state.Strategies {
		// Immutable baseline guard (#343): if a prior initial_capital exists
		// and the incoming value disagrees, keep the prior value so PnL
		// history stays comparable across restarts, state restores, and
		// position closes. A baseline change requires an explicit override
		// via StateDB.SetInitialCapital.
		if prev, ok := existingInitCaps[s.ID]; ok && prev > 0 && s.InitialCapital != prev {
			attempted := s.InitialCapital
			s.InitialCapital = prev
			if _, alreadyWarned := initialCapitalGuardWarned.LoadOrStore(s.ID, struct{}{}); !alreadyWarned {
				msg := fmt.Sprintf("blocking initial_capital change for %s ($%.2f → $%.2f); baseline preserved. Use StateDB.SetInitialCapital or set initial_capital in config to change the baseline (#343)",
					s.ID, prev, attempted)
				fmt.Fprintf(os.Stderr, "[state] WARN: %s\n", msg)
				if initialCapitalGuardWarn != nil {
					initialCapitalGuardWarn(msg)
				}
			}
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
			s.RiskState.MarshalPendingCircuitClosesJSON(),
			strategyActiveProfile(s),
		); err != nil {
			return fmt.Errorf("insert strategy %s: %w", s.ID, err)
		}

		for _, pos := range s.Positions {
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
			if _, err := stmtPos.Exec(s.ID, pos.Symbol, positionID, pos.Quantity, pos.InitialQuantity, pos.AvgCost, pos.EntryATR, pos.Side, pos.Multiplier, pos.OwnerStrategyID, formatTime(pos.OpenedAt), pos.StopLossOID, pos.StopLossTriggerPx, pos.StopLossHighWaterPx, tp1OID, tp2OID, marshalTPOIDsJSON(pos.TPOIDs), marshalTPArmedTiersJSON(pos.TPArmedTiers), nullableFloat64(pos.StopLossATRMult), pos.TPTiersJSON, pos.SLAdjustedTiersProcessed, nullableFloat64(pos.PostTPTrailingATRMult), pos.Regime, marshalRegimeWindowsJSON(pos.RegimeWindows), pos.RegimePendingLabel, pos.RegimePendingCount, pos.RegimeAppliedLabel, pos.ScaleInCount, pos.LastAddPrice, pos.AddedNotionalUSD, pos.RiskAnchorPrice, scaleInResizePending, ratchetFallbackNormalizePending, pos.OpenProfile, directionCertifiedAtOpen, marshalStringMapJSON(pos.DirectionCertifiedStatesAtOpen), llmAnalysisRequested, pos.LLMVerdict, pos.ATRMethodAtOpen, pos.HedgeFor, pos.HedgePrimaryQtyBasis); err != nil {
				return fmt.Errorf("insert position %s/%s: %w", s.ID, pos.Symbol, err)
			}
		}

		for key, opt := range s.OptionPositions {
			positionID := ensureOptionTradeID(s.ID, opt)
			if _, err := stmtOpt.Exec(
				s.ID, key, positionID, opt.Underlying, opt.OptionType, opt.Strike, opt.Expiry, opt.DTE,
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
	stmtTrade, err := tx.Prepare(`INSERT INTO trades (strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, regime, entry_atr, stop_loss_oid, stop_loss_trigger_px, tp_oids_json, manual, stop_loss_atr_mult, tp_tiers_json, pnl_gross, fee_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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
			isClose := 0
			if t.IsClose {
				isClose = 1
			}
			isManual := 0
			if t.Manual {
				isManual = 1
			}
			if _, err := stmtTrade.Exec(s.ID, formatTime(t.Timestamp), t.Symbol, t.PositionID, t.Side, t.Quantity, t.Price, t.Value, t.TradeType, t.Details, t.ExchangeOrderID, t.ExchangeFee, isClose, t.RealizedPnL, t.Regime, t.EntryATR, t.StopLossOID, t.StopLossTriggerPx, marshalTPOIDsJSON(t.TPOIDs), isManual, nullableFloat64(t.StopLossATRMult), t.TPTiersJSON, boolToInt(t.PnLGross), t.FeeSource); err != nil {
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
	if _, err := tx.Exec(`INSERT OR REPLACE INTO portfolio_risk (id, peak_value, current_drawdown_pct, current_margin_drawdown_pct, kill_switch_active, kill_switch_at, warning_sent, warn_band_entered_at, last_warning_equity_dd_pct, last_warning_margin_dd_pct, warning_equity_delta_pct, warning_margin_delta_pct)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		state.PortfolioRisk.PeakValue, state.PortfolioRisk.CurrentDrawdownPct, state.PortfolioRisk.CurrentMarginDrawdownPct,
		ksActive, formatTime(state.PortfolioRisk.KillSwitchAt), warnSent, formatTime(state.PortfolioRisk.WarnBandEnteredAt),
		state.PortfolioRisk.LastWarningEquityDDPct, state.PortfolioRisk.LastWarningMarginDDPct,
		state.PortfolioRisk.WarningEquityDeltaPct, state.PortfolioRisk.WarningMarginDeltaPct,
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
	var lastCycleStr, lastLeaderboardDate, lastLBSummariesJSON, lastSummaryPostJSON string
	err := sdb.db.QueryRow("SELECT cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries, last_summary_post FROM app_state WHERE id = 1").
		Scan(&cycleCount, &lastCycleStr, &lastLeaderboardDate, &lastLBSummariesJSON, &lastSummaryPostJSON)
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
	summaryPosts := make(map[string]time.Time)
	if lastSummaryPostJSON != "" {
		if err := json.Unmarshal([]byte(lastSummaryPostJSON), &summaryPosts); err != nil {
			return nil, fmt.Errorf("parse last_summary_post: %w", err)
		}
	}

	state := &AppState{
		CycleCount:               cycleCount,
		LastCycle:                parseTime(lastCycleStr),
		LastLeaderboardPostDate:  lastLeaderboardDate,
		LastLeaderboardSummaries: lbSummaries,
		LastSummaryPost:          summaryPosts,
		Strategies:               make(map[string]*StrategyState),
	}

	// 2. Load strategies.
	rows, err := sdb.db.Query(`SELECT id, type, platform, cash, initial_capital,
		risk_peak_value, risk_max_drawdown_pct, risk_current_drawdown_pct,
		risk_daily_pnl, risk_daily_pnl_date, risk_consecutive_losses,
		risk_circuit_breaker, risk_circuit_breaker_until, risk_pending_circuit_closes_json,
		COALESCE(active_profile, '') AS active_profile
		FROM strategies`)
	if err != nil {
		return nil, fmt.Errorf("load strategies: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var s StrategyState
		var cbInt int
		var cbUntilStr, pendingCircuitClosesJSON, activeProfile string
		if err := rows.Scan(
			&s.ID, &s.Type, &s.Platform, &s.Cash, &s.InitialCapital,
			&s.RiskState.PeakValue, &s.RiskState.MaxDrawdownPct, &s.RiskState.CurrentDrawdownPct,
			&s.RiskState.DailyPnL, &s.RiskState.DailyPnLDate, &s.RiskState.ConsecutiveLosses,
			&cbInt, &cbUntilStr, &pendingCircuitClosesJSON, &activeProfile,
		); err != nil {
			return nil, fmt.Errorf("scan strategy: %w", err)
		}
		s.RiskState.CircuitBreaker = cbInt != 0
		s.RiskState.CircuitBreakerUntil = parseTime(cbUntilStr)
		s.RiskState.UnmarshalPendingCircuitClosesJSON(pendingCircuitClosesJSON)
		// #998: restore the flat-switch active profile; the pending counter
		// re-arms from zero on restart (a restart can only delay a switch).
		if activeProfile != "" {
			s.RegimeProfile = &RegimeProfileState{ActiveProfile: activeProfile}
		}
		s.Positions = make(map[string]*Position)
		s.OptionPositions = make(map[string]*OptionPosition)
		s.TradeHistory = []Trade{}
		state.Strategies[s.ID] = &s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate strategies: %w", err)
	}

	// 3. Load positions for each strategy.
	posRows, err := sdb.db.Query("SELECT strategy_id, symbol, COALESCE(position_id, '') AS position_id, quantity, initial_quantity, avg_cost, entry_atr, side, multiplier, owner_strategy_id, opened_at, stop_loss_oid, stop_loss_trigger_px, stop_loss_high_water_px, COALESCE(tp1_oid, 0) AS tp1_oid, COALESCE(tp2_oid, 0) AS tp2_oid, COALESCE(tp_oids_json, '') AS tp_oids_json, COALESCE(tp_armed_tiers_json, '') AS tp_armed_tiers_json, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(sl_adjusted_tiers_processed, 0) AS sl_adjusted_tiers_processed, post_tp_trailing_atr_mult, COALESCE(regime, '') AS regime, COALESCE(regime_windows_json, '') AS regime_windows_json, COALESCE(regime_pending_label, '') AS regime_pending_label, COALESCE(regime_pending_count, 0) AS regime_pending_count, COALESCE(regime_applied_label, '') AS regime_applied_label, COALESCE(scale_in_count, 0) AS scale_in_count, COALESCE(last_add_price, 0) AS last_add_price, COALESCE(added_notional_usd, 0) AS added_notional_usd, COALESCE(risk_anchor_price, 0) AS risk_anchor_price, COALESCE(scale_in_resize_pending, 0) AS scale_in_resize_pending, COALESCE(ratchet_fallback_normalize_pending, 0) AS ratchet_fallback_normalize_pending, COALESCE(open_profile, '') AS open_profile, COALESCE(direction_certified_at_open, 0) AS direction_certified_at_open, COALESCE(direction_certified_states_json, '') AS direction_certified_states_json, COALESCE(llm_analysis_requested, 0) AS llm_analysis_requested, COALESCE(llm_verdict, '') AS llm_verdict, COALESCE(atr_method_at_open, '') AS atr_method_at_open, COALESCE(hedge_for, '') AS hedge_for, COALESCE(hedge_primary_qty_basis, 0) AS hedge_primary_qty_basis FROM positions")
	if err != nil {
		return nil, fmt.Errorf("load positions: %w", err)
	}
	defer posRows.Close()
	for posRows.Next() {
		var stratID string
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
		if err := posRows.Scan(&stratID, &pos.Symbol, &pos.TradePositionID, &pos.Quantity, &pos.InitialQuantity, &pos.AvgCost, &pos.EntryATR, &pos.Side, &pos.Multiplier, &pos.OwnerStrategyID, &openedAtStr, &pos.StopLossOID, &pos.StopLossTriggerPx, &pos.StopLossHighWaterPx, &tp1OID, &tp2OID, &tpOIDsJSON, &tpArmedTiersJSON, &slATRMult, &pos.TPTiersJSON, &pos.SLAdjustedTiersProcessed, &postTPTrailingMult, &pos.Regime, &regimeWindowsJSON, &pos.RegimePendingLabel, &pos.RegimePendingCount, &pos.RegimeAppliedLabel, &pos.ScaleInCount, &pos.LastAddPrice, &pos.AddedNotionalUSD, &pos.RiskAnchorPrice, &scaleInResizePending, &ratchetFallbackNormalizePending, &pos.OpenProfile, &directionCertifiedAtOpen, &directionCertifiedStatesJSON, &llmAnalysisRequested, &pos.LLMVerdict, &pos.ATRMethodAtOpen, &pos.HedgeFor, &pos.HedgePrimaryQtyBasis); err != nil {
			return nil, fmt.Errorf("scan position: %w", err)
		}
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
		if s, ok := state.Strategies[stratID]; ok {
			s.Positions[pos.Symbol] = &pos
		}
	}
	if err := posRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate positions: %w", err)
	}

	// 4. Load option positions for each strategy.
	optRows, err := sdb.db.Query(`SELECT strategy_id, id, COALESCE(position_id, '') AS position_id, underlying, option_type, strike, expiry, dte,
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
			&stratID, &opt.ID, &opt.TradePositionID, &opt.Underlying, &opt.OptionType, &opt.Strike, &opt.Expiry, &opt.DTE,
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
		tradeRows, err := sdb.db.Query(`SELECT timestamp, strategy_id, symbol, COALESCE(position_id, '') AS position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, COALESCE(regime, '') AS regime, COALESCE(entry_atr, 0) AS entry_atr, COALESCE(stop_loss_oid, 0) AS stop_loss_oid, COALESCE(stop_loss_trigger_px, 0) AS stop_loss_trigger_px, COALESCE(tp_oids_json, '') AS tp_oids_json, COALESCE(manual, 0) AS manual, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source
			FROM trades WHERE strategy_id = ? ORDER BY timestamp ASC`, id)
		if err != nil {
			return nil, fmt.Errorf("load trades for %s: %w", id, err)
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
			t.Timestamp = parseTime(tsStr)
			t.IsClose = isCloseInt != 0
			t.Manual = isManualInt != 0
			t.PnLGross = pnlGrossInt != 0
			t.TPOIDs = parseTPOIDsJSON(tpOIDsJSON, 0, 0)
			if slATRMult.Valid {
				v := slATRMult.Float64
				t.StopLossATRMult = &v
			}
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
	var ksAtStr, warnBandEnteredAtStr string
	err = sdb.db.QueryRow("SELECT peak_value, current_drawdown_pct, current_margin_drawdown_pct, kill_switch_active, kill_switch_at, warning_sent, COALESCE(warn_band_entered_at, '') AS warn_band_entered_at, COALESCE(last_warning_equity_dd_pct, 0) AS last_warning_equity_dd_pct, COALESCE(last_warning_margin_dd_pct, 0) AS last_warning_margin_dd_pct, COALESCE(warning_equity_delta_pct, 0) AS warning_equity_delta_pct, COALESCE(warning_margin_delta_pct, 0) AS warning_margin_delta_pct FROM portfolio_risk WHERE id = 1").
		Scan(&state.PortfolioRisk.PeakValue, &state.PortfolioRisk.CurrentDrawdownPct, &state.PortfolioRisk.CurrentMarginDrawdownPct,
			&ksActiveInt, &ksAtStr, &warnSentInt, &warnBandEnteredAtStr, &state.PortfolioRisk.LastWarningEquityDDPct,
			&state.PortfolioRisk.LastWarningMarginDDPct, &state.PortfolioRisk.WarningEquityDeltaPct, &state.PortfolioRisk.WarningMarginDeltaPct)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("load portfolio_risk: %w", err)
	}
	state.PortfolioRisk.KillSwitchActive = ksActiveInt != 0
	state.PortfolioRisk.KillSwitchAt = parseTime(ksAtStr)
	state.PortfolioRisk.WarningSent = warnSentInt != 0
	state.PortfolioRisk.WarnBandEnteredAt = parseTime(warnBandEnteredAtStr)

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

// LifetimeTradeStats holds the per-strategy lifetime totals derived from
// the trades table (#455/#471/#607). PositionsOpened is the lifetime count
// of open legs (is_close=0 rows) — the #T column in summaries / leaderboard
// shows positions entered, not closed round trips, so partial-close legs
// don't inflate the count and still-open positions are included. Wins and
// Losses are derived from closed round trips grouped by position_id and
// partitioned by strict net realized PnL sign (PnL > 0 → win, PnL < 0 →
// loss); breakeven round trips are excluded from both buckets. Legacy close
// rows without a position_id fall back to one synthetic group per row.
type LifetimeTradeStats struct {
	PositionsOpened int `json:"positions_opened"`
	Wins            int `json:"wins"`
	Losses          int `json:"losses"`
}

// LifetimeTradeStatsAll returns lifetime stats for every strategy that has
// any trade row in the trades table. Strategies with no trades are absent
// from the result; callers should treat a missing key as an all-zero
// struct. PositionsOpened counts is_close=0 rows; Wins/Losses come from
// closed-round-trip aggregation. Used by FormatCategorySummary (#455) and
// the leaderboard (#580) to render lifetime #T / W/L columns that are
// immune to kill-switch / circuit-breaker resets of the in-memory RiskState
// counters.
func (sdb *StateDB) LifetimeTradeStatsAll() (map[string]LifetimeTradeStats, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	out := make(map[string]LifetimeTradeStats)

	// #873: scale-in add legs are open-side (is_close=0) on an EXISTING
	// position id, not new positions — exclude them so #T stays the count of
	// distinct round-trips opened. W/L below is unaffected (it groups close
	// legs, which a scale-in never is).
	openRows, err := sdb.db.Query(`SELECT strategy_id, COUNT(*)
		FROM trades
		WHERE is_close = 0 AND trade_type NOT IN ('scale_in', 'funding', 'hedge')
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
		entry := out[id]
		entry.PositionsOpened = int(opens.Int64)
		out[id] = entry
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
			WHERE is_close = 1 AND trade_type != 'hedge'
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
		entry := out[id]
		entry.Wins = int(wins.Int64)
		entry.Losses = int(losses.Int64)
		out[id] = entry
	}
	if err := closeRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate lifetime trade stats: %w", err)
	}
	return out, nil
}

// LifetimeTradeStatsForStrategy returns lifetime stats for one strategy. This
// keeps dashboard status polling from scanning and grouping the full trades
// table on every request.
func (sdb *StateDB) LifetimeTradeStatsForStrategy(strategyID string) (LifetimeTradeStats, error) {
	if sdb == nil || sdb.db == nil {
		return LifetimeTradeStats{}, fmt.Errorf("state db unavailable")
	}
	if strategyID == "" {
		return LifetimeTradeStats{}, fmt.Errorf("strategy id required")
	}
	var out LifetimeTradeStats
	var opens sql.NullInt64
	// #873: exclude scale-in add legs — they are open-side legs on an existing
	// position, not new round-trips (mirrors LifetimeTradeStatsAll).
	if err := sdb.db.QueryRow(`SELECT COUNT(*)
		FROM trades
		WHERE strategy_id = ? AND is_close = 0 AND trade_type NOT IN ('scale_in', 'funding', 'hedge')`, strategyID).Scan(&opens); err != nil {
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
			WHERE strategy_id = ? AND is_close = 1 AND trade_type != 'hedge'
			GROUP BY pkey
		)`, strategyID).Scan(&wins, &losses); err != nil {
		return LifetimeTradeStats{}, fmt.Errorf("query lifetime trade stats for %s: %w", strategyID, err)
	}
	out.Wins = int(wins.Int64)
	out.Losses = int(losses.Int64)
	return out, nil
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

	query := fmt.Sprintf("SELECT timestamp, strategy_id, symbol, COALESCE(position_id, '') AS position_id, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee, is_close, realized_pnl, COALESCE(regime, '') AS regime, COALESCE(entry_atr, 0) AS entry_atr, COALESCE(stop_loss_oid, 0) AS stop_loss_oid, COALESCE(stop_loss_trigger_px, 0) AS stop_loss_trigger_px, COALESCE(tp_oids_json, '') AS tp_oids_json, stop_loss_atr_mult, COALESCE(tp_tiers_json, '') AS tp_tiers_json, COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source FROM trades %s ORDER BY timestamp DESC LIMIT ? OFFSET ?", whereClause)
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
		if err := rows.Scan(&tsStr, &t.StrategyID, &t.Symbol, &t.PositionID, &t.Side, &t.Quantity, &t.Price, &t.Value, &t.TradeType, &t.Details, &t.ExchangeOrderID, &t.ExchangeFee, &isCloseInt, &t.RealizedPnL, &t.Regime, &t.EntryATR, &t.StopLossOID, &t.StopLossTriggerPx, &tpOIDsJSON, &slATRMult, &t.TPTiersJSON, &pnlGrossInt, &t.FeeSource); err != nil {
			return nil, 0, fmt.Errorf("scan trade: %w", err)
		}
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

// QueryTradingViewExportTrades returns all trades for the given strategies in
// chronological order for deterministic CSV export.
func (sdb *StateDB) QueryTradingViewExportTrades(strategyIDs []string) ([]Trade, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	if len(strategyIDs) == 0 {
		return nil, fmt.Errorf("at least one strategy id is required")
	}
	placeholders := make([]string, len(strategyIDs))
	args := make([]interface{}, 0, len(strategyIDs))
	for i, id := range strategyIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := fmt.Sprintf(`SELECT timestamp, strategy_id, symbol, side, quantity, price, value, trade_type, details, exchange_order_id, exchange_fee
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
		if err := rows.Scan(&tsStr, &t.StrategyID, &t.Symbol, &t.Side, &t.Quantity, &t.Price, &t.Value, &t.TradeType, &t.Details, &t.ExchangeOrderID, &t.ExchangeFee); err != nil {
			return nil, fmt.Errorf("scan TradingView export trade: %w", err)
		}
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

// PendingManualAction is a row from the pending_manual_actions queue table
// written by operator CLIs (manual-open / manual-close / force-close) and
// drained by the scheduler at the top of each cycle (#569/#1140).
type PendingManualAction struct {
	ID                              int64
	StrategyID                      string
	Action                          string // "open" | "close" | "add" | "update-sl" | "cancel-sl"
	Symbol                          string
	Side                            string
	Quantity                        float64
	FillPrice                       float64
	FillFee                         float64
	ExchangeOrderID                 string
	StopLossOID                     int64
	StopLossTriggerPx               float64
	EntryATR                        float64
	ATRMethod                       string // open-only: atr_method resolved at queue time, next to the EntryATR fetch (#1277)
	RealizedPnL                     float64
	IsFullClose                     bool    // close-only: operator/scheduler intent flag (avoids tolerance heuristics on the drain side)
	TPOIDs                          []int64 // open: placed TP OIDs; close: canceled TP OIDs that must be cleared for re-arm
	RatchetFallbackNormalizePending bool    // open-only: one-shot normalize marker for fallback ratchet SL (#1121)
	CreatedAt                       time.Time
}

// InsertPendingManualAction enqueues an operator action for the scheduler to
// drain on its next cycle.
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
	_, err := sdb.db.Exec(`INSERT INTO pending_manual_actions
		(strategy_id, action, symbol, side, quantity, fill_price, fill_fee, exchange_order_id, stop_loss_oid, stop_loss_trigger_px, entry_atr, atr_method, realized_pnl, is_full_close, tp_oids_json, ratchet_fallback_normalize_pending, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.StrategyID, a.Action, a.Symbol, a.Side, a.Quantity, a.FillPrice, a.FillFee,
		a.ExchangeOrderID, a.StopLossOID, a.StopLossTriggerPx, a.EntryATR, a.ATRMethod, a.RealizedPnL,
		isFullClose, marshalTPOIDsJSON(a.TPOIDs), ratchetFallbackNormalizePending, formatTime(a.CreatedAt))
	return err
}

// LoadPendingManualActions returns all queued actions ordered by id (oldest first).
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
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

// DeletePendingManualActionsThrough deletes all rows with id <= maxID.
func (sdb *StateDB) DeletePendingManualActionsThrough(maxID int64) error {
	if sdb == nil || sdb.db == nil {
		return nil
	}
	_, err := sdb.db.Exec("DELETE FROM pending_manual_actions WHERE id <= ?", maxID)
	return err
}

// PendingLimitOrder is a row from the pending_limit_orders table (#883). Unlike
// PendingManualAction (which holds an already-filled action awaiting scheduler
// adoption), a PendingLimitOrder is a *resting* on-chain limit order with no
// fill at placement time. The scheduler polls each row by order_oid every cycle
// and grows the tracked position as fills arrive. filled_size / avg_fill_price /
// fill_fee are watermarks: the cumulative quantity already adopted into the
// position so the next poll only books the incremental delta.
type PendingLimitOrder struct {
	ID              int64
	StrategyID      string
	Symbol          string
	Side            string // "long" | "short"
	OrderOID        int64
	LimitPrice      float64
	OrderSize       float64
	TIF             string
	FilledSize      float64 // cumulative qty already booked into the position
	AvgFillPrice    float64 // size-weighted avg fill price booked so far
	FillFee         float64 // cumulative fee booked so far
	EntryATR        float64 // operator-supplied entry ATR (0 = fetch at fill)
	CancelRequested bool    // operator manual-cancel flips this; scheduler cancels + finalizes
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

// boolToInt maps a bool to the 0/1 integer SQLite stores for boolean columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// InsertPendingLimitOrder records a freshly-placed resting limit order for the
// scheduler to poll. Returns the new row id.
func (sdb *StateDB) InsertPendingLimitOrder(o PendingLimitOrder) (int64, error) {
	if sdb == nil || sdb.db == nil {
		return 0, fmt.Errorf("state db unavailable")
	}
	expiresStr := ""
	if !o.ExpiresAt.IsZero() {
		expiresStr = formatTime(o.ExpiresAt.UTC())
	}
	res, err := sdb.db.Exec(`INSERT INTO pending_limit_orders
		(strategy_id, symbol, side, order_oid, limit_price, order_size, tif, filled_size, avg_fill_price, fill_fee, entry_atr, cancel_requested, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.StrategyID, o.Symbol, o.Side, o.OrderOID, o.LimitPrice, o.OrderSize, o.TIF,
		o.FilledSize, o.AvgFillPrice, o.FillFee, o.EntryATR, boolToInt(o.CancelRequested),
		expiresStr, formatTime(o.CreatedAt.UTC()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LoadPendingLimitOrders returns all resting limit orders ordered by id.
func (sdb *StateDB) LoadPendingLimitOrders() ([]PendingLimitOrder, error) {
	if sdb == nil || sdb.db == nil {
		return nil, nil
	}
	rows, err := sdb.db.Query(`SELECT id, strategy_id, symbol, side, order_oid, limit_price, order_size, tif, filled_size, avg_fill_price, fill_fee, entry_atr, cancel_requested, expires_at, created_at FROM pending_limit_orders ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("load pending limit orders: %w", err)
	}
	defer rows.Close()
	var orders []PendingLimitOrder
	for rows.Next() {
		var o PendingLimitOrder
		var cancelInt int
		var expiresStr, createdStr string
		if err := rows.Scan(&o.ID, &o.StrategyID, &o.Symbol, &o.Side, &o.OrderOID, &o.LimitPrice, &o.OrderSize, &o.TIF, &o.FilledSize, &o.AvgFillPrice, &o.FillFee, &o.EntryATR, &cancelInt, &expiresStr, &createdStr); err != nil {
			return nil, fmt.Errorf("scan pending limit order: %w", err)
		}
		o.CancelRequested = cancelInt != 0
		if expiresStr != "" {
			o.ExpiresAt = parseTime(expiresStr)
		}
		o.CreatedAt = parseTime(createdStr)
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// UpdatePendingLimitOrderFill advances the watermark fields after the scheduler
// books an incremental fill for a resting order.
func (sdb *StateDB) UpdatePendingLimitOrderFill(id int64, filledSize, avgFillPrice, fillFee float64) error {
	if sdb == nil || sdb.db == nil {
		return nil
	}
	_, err := sdb.db.Exec(
		"UPDATE pending_limit_orders SET filled_size = ?, avg_fill_price = ?, fill_fee = ? WHERE id = ?",
		filledSize, avgFillPrice, fillFee, id)
	return err
}

// MarkPendingLimitOrderCancelRequested flags a resting order for cancellation by
// the scheduler. Returns the number of rows affected so the caller can tell the
// operator whether a matching open order existed.
func (sdb *StateDB) MarkPendingLimitOrderCancelRequested(strategyID, symbol string) (int64, error) {
	if sdb == nil || sdb.db == nil {
		return 0, fmt.Errorf("state db unavailable")
	}
	res, err := sdb.db.Exec(
		"UPDATE pending_limit_orders SET cancel_requested = 1 WHERE strategy_id = ? AND symbol = ?",
		strategyID, symbol)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeletePendingLimitOrder removes a single resting-order row by id (terminal:
// fully filled, cancelled, or expired).
func (sdb *StateDB) DeletePendingLimitOrder(id int64) error {
	if sdb == nil || sdb.db == nil {
		return nil
	}
	_, err := sdb.db.Exec("DELETE FROM pending_limit_orders WHERE id = ?", id)
	return err
}

// CountPendingLimitOrders returns the number of resting limit orders for a
// strategy/symbol. Used by manual-open to reject a second resting order (or an
// open position) for the same coin before placing.
func (sdb *StateDB) CountPendingLimitOrders(strategyID, symbol string) (int, error) {
	if sdb == nil || sdb.db == nil {
		return 0, nil
	}
	var n int
	err := sdb.db.QueryRow(
		"SELECT COUNT(*) FROM pending_limit_orders WHERE strategy_id = ? AND symbol = ?",
		strategyID, symbol).Scan(&n)
	return n, err
}

// EarliestTradeTimestamp returns the oldest trade timestamp across the given
// strategy IDs, or zero time when none exist. Used by `go-trader backfill
// hl-fees` to set the lower bound on the userFills query.
func (sdb *StateDB) EarliestTradeTimestamp(strategyIDs []string) (time.Time, error) {
	if sdb == nil || sdb.db == nil {
		return time.Time{}, fmt.Errorf("state db unavailable")
	}
	if len(strategyIDs) == 0 {
		return time.Time{}, nil
	}
	placeholders := make([]string, len(strategyIDs))
	args := make([]interface{}, len(strategyIDs))
	for i, id := range strategyIDs {
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

// ListTradesForBackfill returns the trade rows for one strategy that the
// backfill planner needs (rowid + the columns it reads/rewrites). Ordered by
// timestamp ascending so the cash replay runs in the same order as the live
// fills did.
func (sdb *StateDB) ListTradesForBackfill(strategyID string) ([]TradeBackfillRow, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	rows, err := sdb.db.Query(`
		SELECT rowid, timestamp, symbol, COALESCE(position_id, '') AS position_id,
		       side, quantity, price, value, trade_type, is_close, exchange_order_id, exchange_fee, realized_pnl,
		       COALESCE(pnl_gross, 0) AS pnl_gross, COALESCE(fee_source, '') AS fee_source
		FROM trades
		WHERE strategy_id = ?
		ORDER BY timestamp ASC, rowid ASC`, strategyID)
	if err != nil {
		return nil, fmt.Errorf("list trades for backfill: %w", err)
	}
	defer rows.Close()
	var out []TradeBackfillRow
	for rows.Next() {
		var t TradeBackfillRow
		var tsStr string
		var isCloseInt, pnlGrossInt int
		if err := rows.Scan(&t.RowID, &tsStr, &t.Symbol, &t.PositionID, &t.Side, &t.Quantity, &t.Price, &t.Value, &t.TradeType, &isCloseInt,
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

// ClosedPositionRow captures the closed_positions columns the backfill needs
// to match its rows back to a position_id (which closed_positions itself does
// not store yet — only trades does, since #471).
type ClosedPositionRow struct {
	ID          int64
	Symbol      string
	ClosedAt    time.Time
	RealizedPnL float64
}

// LoadClosedPositionRows returns the closed_positions rows for one strategy
// in close-time order. The backfill matches each row to a close-leg trade row
// (same symbol + matching timestamp) to recover the position_id grouping
// since closed_positions has no position_id column of its own.
func (sdb *StateDB) LoadClosedPositionRows(strategyID string) ([]ClosedPositionRow, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	rows, err := sdb.db.Query(`
		SELECT id, symbol, closed_at, realized_pnl
		FROM closed_positions
		WHERE strategy_id = ?
		ORDER BY closed_at ASC, id ASC`, strategyID)
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

// ApplyBackfillPlan writes the planner's changes to disk inside one
// transaction: trade row updates, closed_positions PnL recompute, and the
// strategies.cash baseline. Caller is responsible for ensuring no scheduler
// is concurrently issuing SaveState — SQLite serializes writers so the
// transaction itself is safe, but a SaveState fired right after a successful
// commit will overwrite the recomputed cash with whatever value its
// in-memory AppState held.
func (sdb *StateDB) ApplyBackfillPlan(plan BackfillPlan) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
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

	// closed_positions are pinned by rowid (planner resolved each one back
	// to a position_id via close-leg trade timestamps).
	cpStmt, err := tx.Prepare(
		"UPDATE closed_positions SET realized_pnl = ? WHERE id = ? AND strategy_id = ?",
	)
	if err != nil {
		return fmt.Errorf("prepare closed_positions update: %w", err)
	}
	defer cpStmt.Close()
	for _, cp := range plan.ClosedPositions {
		if _, err := cpStmt.Exec(cp.NewPnL, cp.RowID, plan.StrategyID); err != nil {
			return fmt.Errorf("update closed_positions id=%d: %w", cp.RowID, err)
		}
	}

	if _, err := tx.Exec("UPDATE strategies SET cash = ? WHERE id = ?", plan.NewCash, plan.StrategyID); err != nil {
		return fmt.Errorf("update strategy cash: %w", err)
	}

	return tx.Commit()
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
