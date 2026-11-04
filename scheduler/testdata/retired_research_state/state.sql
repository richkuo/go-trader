PRAGMA foreign_keys=OFF;
BEGIN TRANSACTION;
CREATE TABLE app_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    cycle_count INTEGER NOT NULL DEFAULT 0,
    last_cycle TEXT NOT NULL DEFAULT '',
    last_leaderboard_post_date TEXT NOT NULL DEFAULT '',
    last_leaderboard_summaries TEXT NOT NULL DEFAULT '',
    last_summary_post TEXT NOT NULL DEFAULT ''
);
INSERT INTO app_state VALUES(1,0,'','','','');
CREATE TABLE strategies (
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
    risk_pending_circuit_closes_json TEXT NOT NULL DEFAULT '',
    -- #998: regime-profile allocation active profile (flat-switch persistence).
    active_profile TEXT NOT NULL DEFAULT '',
    -- #1394: live spot over-budget books still need operator reconciliation.
    cash_reconcile_required INTEGER NOT NULL DEFAULT 0,
    -- #1408: durable pool-mode marker for one-time allocation transitions.
    shared_wallet_pool_budget INTEGER NOT NULL DEFAULT 0,
    -- #1411: Hurst entry-gate hysteresis latch as JSON (threshold key + state).
    hurst_gate_state TEXT NOT NULL DEFAULT ''
, replay_mirror_watermark INTEGER NOT NULL DEFAULT 0, replay_mirror_watermark_source TEXT NOT NULL DEFAULT '', paper_funding_state TEXT NOT NULL DEFAULT '', quote_precision_rebaselined INTEGER NOT NULL DEFAULT 0, risk_rh_shared_close_notice_json TEXT NOT NULL DEFAULT '');
INSERT INTO strategies VALUES('hl-bsk-avax','perps','hyperliquid',4999.551349437499994,5000.0,5000.0,90.0,0.0,0.0,'',0,0,'','','',0,0,'',0,'','',0,'');
INSERT INTO strategies VALUES('hl-bsk-btc','perps','hyperliquid',4999.551305764999597,5000.0,5000.0,90.0,0.0,0.0,'',0,0,'','','',0,0,'',0,'','',0,'');
INSERT INTO strategies VALUES('hl-bsk-eth','perps','hyperliquid',4999.551170697500311,5000.0,5000.0,90.0,0.0,0.0,'',0,0,'','','',0,0,'',0,'','',0,'');
INSERT INTO strategies VALUES('hl-bsk-sol','perps','hyperliquid',4999.552024100000381,5000.0,5000.0,90.0,0.0,0.0,'',0,0,'','','',0,0,'',0,'','',0,'');
INSERT INTO strategies VALUES('pair-hl-btc','perps','hyperliquid',5998.369200000000091,6000.0,6000.0,50.0,0.0,0.0,'',0,0,'','','',0,0,'',0,'','',0,'');
INSERT INTO strategies VALUES('pair-okx-btc','perps','okx',5998.388100000000122,6000.0,6000.0,50.0,0.0,0.0,'',0,0,'','','',0,0,'',0,'','',0,'');
CREATE TABLE positions (
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
    shared_close_hold_usd REAL NOT NULL DEFAULT 0,
    shared_close_hold_reason TEXT NOT NULL DEFAULT '',
    -- #1411: gate reading + applied size multiplier frozen at open (0 = unstamped).
    hurst_at_open REAL NOT NULL DEFAULT 0,
    hurst_size_mult REAL NOT NULL DEFAULT 0,
    sl_after_moved INTEGER NOT NULL DEFAULT 0,
    tp_consumptions_json TEXT NOT NULL DEFAULT '',
    sl_after_trigger_px REAL NOT NULL DEFAULT 0,
    resting_tp_scanned_open_ms INTEGER NOT NULL DEFAULT 0,
    resting_tp_reach_px REAL NOT NULL DEFAULT 0,
    resting_tp_stop_trigger_px REAL NOT NULL DEFAULT 0, regime_snapshots_json TEXT NOT NULL DEFAULT '', regime_pending_label TEXT NOT NULL DEFAULT '', regime_pending_count INTEGER NOT NULL DEFAULT 0, regime_applied_label TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (strategy_id, symbol)
);
INSERT INTO positions VALUES('hl-bsk-avax','AVAX','hl-bsk-avax:AVAX:1793577660000000000:1',33.25,33.25,29.98500000000000299,0.2999999999999999889,'short',1.0,'hl-bsk-avax','2026-11-02T00:01:00Z',0,30.28500000000000014,0.0,0,0,'','',1.0,'',0,NULL,'','',0,0.0,0.0,0.0,0,0,'',0,'',0,'','simple','',0.0,0.0,'',0.0,0.0,0,'',0.0,0,0.0,0.0,'{}','',0,'');
INSERT INTO positions VALUES('hl-bsk-btc','BTC','hl-bsk-btc:BTC:1793577660000000000:3',0.01660999999999999977,0.01660999999999999977,60030.0,600.0,'long',1.0,'hl-bsk-btc','2026-11-02T00:01:00Z',0,59430.0,0.0,0,0,'','',1.0,'',0,NULL,'','',0,0.0,0.0,0.0,0,0,'',0,'',0,'','simple','',0.0,0.0,'',0.0,0.0,0,'',0.0,0,0.0,0.0,'{}','',0,'');
INSERT INTO positions VALUES('hl-bsk-eth','ETH','hl-bsk-eth:ETH:1793577660000000000:2',0.3322999999999999843,0.3322999999999999843,3001.5,30.0,'long',1.0,'hl-bsk-eth','2026-11-02T00:01:00Z',0,2971.5,0.0,0,0,'','',1.0,'',0,NULL,'','',0,0.0,0.0,0.0,0,0,'',0,'',0,'','simple','',0.0,0.0,'',0.0,0.0,0,'',0.0,0,0.0,0.0,'{}','',0,'');
INSERT INTO positions VALUES('hl-bsk-sol','SOL','hl-bsk-sol:SOL:1793577660000000000:4',6.639999999999999681,6.639999999999999681,149.9250000000000113,1.5,'short',1.0,'hl-bsk-sol','2026-11-02T00:01:00Z',0,151.4250000000000113,0.0,0,0,'','',1.0,'',0,NULL,'','',0,0.0,0.0,0.0,0,0,'',0,'',0,'','simple','',0.0,0.0,'',0.0,0.0,0,'',0.0,0,0.0,0.0,'{}','',0,'');
INSERT INTO positions VALUES('pair-hl-btc','BTC','pair-hl-btc:BTC:1790856061000000000:6',0.05999999999999999778,0.05999999999999999778,60400.0,0.0,'long',1.0,'pair-hl-btc','2026-10-01T12:01:01Z',0,0.0,0.0,0,0,'','',1.0,'',0,NULL,'','',0,0.0,0.0,0.0,0,0,'',0,'',0,'','simple','',0.0,0.0,'',0.0,0.0,0,'',0.0,0,0.0,0.0,'{}','',0,'');
INSERT INTO positions VALUES('pair-okx-btc','BTC','pair-okx-btc:BTC:1790856001000000000:5',0.05999999999999999778,0.05999999999999999778,59700.0,0.0,'short',1.0,'pair-okx-btc','2026-10-01T12:00:01Z',0,0.0,0.0,0,0,'','',NULL,'',0,NULL,'','',0,0.0,0.0,0.0,0,0,'',0,'',0,'','','',0.0,0.0,'',0.0,0.0,0,'',0.0,0,0.0,0.0,'{}','',0,'');
CREATE TABLE closed_positions (
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
CREATE TABLE closed_option_positions (
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
CREATE TABLE option_positions (
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
CREATE TABLE trades (
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
    fee_source TEXT NOT NULL DEFAULT '',
    cost_model_version INTEGER NOT NULL DEFAULT 0,
    cost_policy_id TEXT NOT NULL DEFAULT ''
, regime TEXT NOT NULL DEFAULT '', entry_atr REAL NOT NULL DEFAULT 0, stop_loss_trigger_px REAL NOT NULL DEFAULT 0, manual INTEGER NOT NULL DEFAULT 0);
INSERT INTO trades VALUES(1,'hl-bsk-avax','2026-11-02T00:01:00Z','AVAX','hl-bsk-avax:AVAX:1793577660000000000:1','sell',33.25,29.98500000000000299,997.00125000000014,'perps','Open short 33.250000 @ $29.99 (1.0x, fee $0.45)','',0.4486505625000000718,0,0.0,1.0,'',0,'',1,'modeled',1,'','',0.2999999999999999889,30.28500000000000014,0);
INSERT INTO trades VALUES(2,'hl-bsk-eth','2026-11-02T00:01:00Z','ETH','hl-bsk-eth:ETH:1793577660000000000:2','buy',0.3322999999999999843,3001.5,997.398449999999911,'perps','Open long 0.332300 @ $3001.50 (1.0x, fee $0.45)','',0.4488293024999999714,0,0.0,1.0,'',0,'',1,'modeled',1,'','',30.0,2971.5,0);
INSERT INTO trades VALUES(3,'hl-bsk-btc','2026-11-02T00:01:00Z','BTC','hl-bsk-btc:BTC:1793577660000000000:3','buy',0.01660999999999999977,60030.0,997.098299999999995,'perps','Open long 0.016610 @ $60030.00 (1.0x, fee $0.45)','',0.4486942349999999969,0,0.0,1.0,'',0,'',1,'modeled',1,'','',600.0,59430.0,0);
INSERT INTO trades VALUES(4,'hl-bsk-sol','2026-11-02T00:01:00Z','SOL','hl-bsk-sol:SOL:1793577660000000000:4','sell',6.639999999999999681,149.9250000000000113,995.502000000000067,'perps','Open short 6.640000 @ $149.93 (1.0x, fee $0.45)','',0.447975900000000038,0,0.0,1.0,'',0,'',1,'modeled',1,'','',1.5,151.4250000000000113,0);
INSERT INTO trades VALUES(5,'pair-okx-btc','2026-10-01T12:00:01Z','BTC','pair-okx-btc:BTC:1790856001000000000:5','sell',0.05999999999999999778,59700.0,3582.0,'perps','Open short 0.060000 @ $59700.00 (2.0x, fee $1.61)','okx-000001',1.611899999999999889,0,0.0,NULL,'',0,'',1,'modeled',1001,'pair-cost-1bf37071793e7392f966f9a85d970a3a6718a30312a8634333e91f8933a374aa','',0.0,0.0,0);
INSERT INTO trades VALUES(6,'pair-hl-btc','2026-10-01T12:01:01Z','BTC','pair-hl-btc:BTC:1790856061000000000:6','buy',0.05999999999999999778,60400.0,3624.0,'perps','Open long 0.060000 @ $60400.00 (2.0x, fee $1.63)','hyperliquid-000002',1.630800000000000027,0,0.0,1.0,'',0,'',1,'modeled',1001,'pair-cost-dc425e59866a8c9a77715582604e59a859a2ed240efeba31ecd115292a7a97c6','',0.0,0.0,0);
CREATE TABLE kill_switch_events (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp TEXT NOT NULL,
    type TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    drawdown_pct REAL NOT NULL DEFAULT 0,
    portfolio_value REAL NOT NULL DEFAULT 0,
    peak_value REAL NOT NULL DEFAULT 0,
    details TEXT NOT NULL DEFAULT ''
, scope TEXT NOT NULL DEFAULT '');
CREATE TABLE pending_manual_actions (
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
    prev_tp_oids_json TEXT NOT NULL DEFAULT '',
    tp_armed_tiers_json TEXT NOT NULL DEFAULT '',
    position_id TEXT NOT NULL DEFAULT '',
    ratchet_fallback_normalize_pending INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
, regime_snapshots_json TEXT NOT NULL DEFAULT '');
CREATE TABLE wallet_ledger_state (
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    funding_since_ms INTEGER NOT NULL DEFAULT 0,
    transfers_since_ms INTEGER NOT NULL DEFAULT 0,
    baseline_offset_usd REAL NOT NULL DEFAULT 0,
    baseline_set INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (platform, account)
);
CREATE TABLE wallet_transfers (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    time_ms INTEGER NOT NULL,
    kind TEXT NOT NULL,
    amount_usd REAL NOT NULL,
    dedup_id TEXT NOT NULL UNIQUE
);
CREATE TABLE cashflow_journal (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    time_ms INTEGER NOT NULL,
    kind TEXT NOT NULL,
    amount_usd REAL NOT NULL,
    coin TEXT NOT NULL DEFAULT '',
    closed_pnl_gross REAL NOT NULL DEFAULT 0,
    fee_usd REAL NOT NULL DEFAULT 0,
    hl_basis_error REAL NOT NULL DEFAULT 0,
    hl_basis_resolved INTEGER NOT NULL DEFAULT 0,
    dedup_id TEXT NOT NULL UNIQUE
);
CREATE TABLE cashflow_hl_basis_state (
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    scan_since_ms INTEGER NOT NULL DEFAULT 0,
    target_ms INTEGER NOT NULL DEFAULT 0,
    ready INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (platform, account)
);
CREATE TABLE cashflow_hl_basis_book (
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    coin TEXT NOT NULL,
    position_size TEXT NOT NULL DEFAULT '0',
    entry_notional TEXT NOT NULL DEFAULT '0',
    has_position INTEGER NOT NULL DEFAULT 0,
    entry_known INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (platform, account, coin)
);
CREATE TABLE cashflow_hl_basis_seen (
    platform TEXT NOT NULL,
    account TEXT NOT NULL,
    dedup_id TEXT NOT NULL,
    PRIMARY KEY (platform, account, dedup_id)
);
CREATE TABLE cashflow_journal_state (
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
CREATE TABLE pending_limit_orders (
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
, regime_snapshots_json TEXT NOT NULL DEFAULT '', owner_kind TEXT NOT NULL DEFAULT 'manual', partition_scope TEXT NOT NULL DEFAULT 'live', partition_source TEXT NOT NULL DEFAULT '', quote_generation INTEGER NOT NULL DEFAULT 0, quote_role TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', account_key TEXT NOT NULL DEFAULT '', venue_namespace TEXT NOT NULL DEFAULT '', request_json TEXT NOT NULL DEFAULT '', request_expiry_ms INTEGER NOT NULL DEFAULT 0, client_order_id TEXT NOT NULL DEFAULT '', venue_order_id TEXT NOT NULL DEFAULT '', submit_state TEXT NOT NULL DEFAULT '', cancel_state TEXT NOT NULL DEFAULT '', confirmed_qty TEXT NOT NULL DEFAULT '0', applied_qty TEXT NOT NULL DEFAULT '0', accepted_at_ms INTEGER NOT NULL DEFAULT 0, terminal_at_ms INTEGER NOT NULL DEFAULT 0, outcome_json TEXT NOT NULL DEFAULT '', updated_at_ms INTEGER NOT NULL DEFAULT 0);
CREATE TABLE trade_diagnostics (
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
, hurst_at_open REAL, hurst_size_mult REAL);
CREATE TABLE regime_window_history (
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
CREATE TABLE regime_window_transitions (
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
CREATE TABLE regime_reversal_alerts (
    platform TEXT NOT NULL,
    symbol TEXT NOT NULL,
    timeframe TEXT NOT NULL,
    spec_json TEXT NOT NULL,
    signature TEXT NOT NULL,
    alerted_at TEXT NOT NULL,
    PRIMARY KEY (platform, symbol, timeframe, spec_json)
);
CREATE TABLE IF NOT EXISTS "portfolio_risk" (
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
    kill_switch_close_applied INTEGER NOT NULL DEFAULT 0,
    quote_precision_rebaselined INTEGER NOT NULL DEFAULT 0,
    pair_valuation_incomplete_since TEXT NOT NULL DEFAULT ''
);
INSERT INTO portfolio_risk VALUES('paper',4100.0,0.0,0.0,0,'',0,'',0.0,0.0,0.0,0.0,0,0,'',0,0,'2026-09-30T08:15:00Z');
CREATE TABLE IF NOT EXISTS "correlation_snapshot" (
    scope TEXT PRIMARY KEY CHECK (scope IN ('live','paper','')),
    snapshot_json TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE entry_decision_state (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    strategy_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    interval_ms INTEGER NOT NULL CHECK (interval_ms > 0),
    target_side TEXT NOT NULL CHECK (target_side IN ('long','short')),
    latest_observed_bar_ms INTEGER NOT NULL DEFAULT 0,
    bootstrap_through_bar_ms INTEGER NOT NULL DEFAULT 0,
    consumed_through_bar_ms INTEGER NOT NULL DEFAULT 0,
    updated_at_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, source, strategy_id, symbol, interval_ms, target_side)
);
INSERT INTO entry_decision_state VALUES('paper','','hl-bsk-avax','AVAX',3600000,'long',1793574000000,0,0,1793577660000);
INSERT INTO entry_decision_state VALUES('paper','','hl-bsk-avax','AVAX',3600000,'short',1793574000000,0,1793574000000,1793577660000);
INSERT INTO entry_decision_state VALUES('paper','','hl-bsk-btc','BTC',3600000,'long',1793574000000,0,1793574000000,1793577660000);
INSERT INTO entry_decision_state VALUES('paper','','hl-bsk-btc','BTC',3600000,'short',1793574000000,0,0,1793577660000);
INSERT INTO entry_decision_state VALUES('paper','','hl-bsk-eth','ETH',3600000,'long',1793574000000,0,1793574000000,1793577660000);
INSERT INTO entry_decision_state VALUES('paper','','hl-bsk-eth','ETH',3600000,'short',1793574000000,0,0,1793577660000);
INSERT INTO entry_decision_state VALUES('paper','','hl-bsk-sol','SOL',3600000,'long',1793574000000,0,0,1793577660000);
INSERT INTO entry_decision_state VALUES('paper','','hl-bsk-sol','SOL',3600000,'short',1793574000000,0,1793574000000,1793577660000);
CREATE TABLE IF NOT EXISTS "entry_order_intents" (
    intent_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    strategy_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    interval_ms INTEGER NOT NULL,
    target_side TEXT NOT NULL CHECK (target_side IN ('long','short')),
    bar_open_ms INTEGER NOT NULL,
    decision_boundary_ms INTEGER NOT NULL,
    cutoff_ms INTEGER NOT NULL,
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL DEFAULT '',
    instrument_type TEXT NOT NULL DEFAULT '',
    client_order_id TEXT NOT NULL,
    initial_stop_client_id TEXT NOT NULL DEFAULT '',
    venue_order_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('reserved','submitting','settling','committed','no_fill')),
    request_json TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '',
    hold_reason TEXT NOT NULL DEFAULT '',
    confirmed_qty REAL NOT NULL DEFAULT 0,
    confirmed_notional REAL NOT NULL DEFAULT 0,
    confirmed_fee REAL NOT NULL DEFAULT 0,
    applied_qty REAL NOT NULL DEFAULT 0,
    applied_notional REAL NOT NULL DEFAULT 0,
    applied_fee REAL NOT NULL DEFAULT 0,
    settlement_seq INTEGER NOT NULL DEFAULT 0,
    replay_payload_json TEXT NOT NULL DEFAULT '',
    replay_delivered_seq INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    decision_kind TEXT NOT NULL DEFAULT 'candle' CHECK (decision_kind IN ('candle','quote')),
    quote_event_id TEXT NOT NULL DEFAULT '',
    owner_mode TEXT NOT NULL DEFAULT '' CHECK (owner_mode IN ('','basket','pair')),
    owner_group_id TEXT NOT NULL DEFAULT '',
    CHECK ((decision_kind = 'candle' AND interval_ms > 0 AND quote_event_id = '')
        OR (decision_kind = 'quote' AND interval_ms = 0 AND quote_event_id != '' AND owner_mode = 'pair'))
);
INSERT INTO entry_order_intents VALUES('ei-basket-retired-fixture','paper','','hl-bsk-btc','BTC',3600000,'long',1793581200000,1793584800000,1793585100000,'hyperliquid','','perps','hyperliquid-retired-fixture','','','reserved','{"version":1,"kind":"basket:ba-retired-fixture","signal":1,"size":0.01,"prev_pos_qty":0,"price":60000,"script":"shared_scripts/run_basket_ranker.py"}','','',0.0,0.0,0.0,0.0,0.0,0.0,0,'',0,1793577720000,1793577720000,'candle','','','');
INSERT INTO entry_order_intents VALUES('ei-6d8924f3b9318eb778232231','paper','','pair-okx-btc','BTC',0,'short',0,1790856000000,1790856000500,'okx','synthetic:okx-research','perps','okx-000001','','','committed','{"version":1,"kind":"pair:btc-xvenue:pq-4687ae86f0d4184bfe86f295","signal":-1,"size":0.06,"prev_pos_qty":0,"price":59700,"script":"shared_scripts/check_okx.py"}','{"outcome":"filled","filled_qty":0,"observed_at_ms":1790856001000,"client_order_id":"okx-000001","account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","side":"sell","cum_qty":0.06,"cum_notional":3582,"accepted_at_ms":1790856001000,"terminal":true}','',0.05999999999999999778,3582.0,0.0,0.05999999999999999778,3582.0,1.611899999999999889,1,'',0,1790856001000,1790856001000,'quote','pq-4687ae86f0d4184bfe86f295','pair','btc-xvenue');
INSERT INTO entry_order_intents VALUES('ei-ddc9ba4a6614ee797ff472ef','paper','','pair-hl-btc','BTC',0,'long',0,1790856060000,1790856060500,'hyperliquid','synthetic:hl-research','perps','hyperliquid-000002','','','committed','{"version":1,"kind":"pair:btc-xvenue:pq-d406cbd50865f42d08703003","signal":1,"size":0.06,"prev_pos_qty":0,"price":60400,"script":"shared_scripts/check_hyperliquid.py"}','{"outcome":"filled","filled_qty":0,"observed_at_ms":1790856061000,"client_order_id":"hyperliquid-000002","account_key":"synthetic:hl-research","instrument":"BTC","side":"buy","cum_qty":0.06,"cum_notional":3624,"accepted_at_ms":1790856061000,"terminal":true}','',0.05999999999999999778,3624.0,0.0,0.05999999999999999778,3624.0,1.630800000000000027,1,'',0,1790856061000,1790856061000,'quote','pq-d406cbd50865f42d08703003','pair','btc-xvenue');
CREATE TABLE robinhood_order_attempts (
    attempt_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope = 'live'),
    source TEXT NOT NULL DEFAULT '' CHECK (source = ''),
    strategy_id TEXT NOT NULL,
    account_key TEXT NOT NULL CHECK (account_key != ''),
    symbol TEXT NOT NULL CHECK (symbol != ''),
    origin TEXT NOT NULL CHECK (origin IN ('signal','circuit_breaker','kill_switch')),
    side TEXT NOT NULL CHECK (side IN ('buy','sell')),
    ref_id TEXT NOT NULL CHECK (ref_id != ''),
    venue_order_id TEXT NOT NULL DEFAULT '',
    venue_account_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('submitting','open','unknown','settling','settled','no_fill')),
    request_json TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '',
    hold_reason TEXT NOT NULL DEFAULT '',
    confirmed_qty REAL NOT NULL DEFAULT 0,
    confirmed_avg_px REAL NOT NULL DEFAULT 0,
    confirmed_fee REAL NOT NULL DEFAULT 0,
    applied_qty REAL NOT NULL DEFAULT 0,
    applied_fee REAL NOT NULL DEFAULT 0,
    settlement_seq INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);
CREATE TABLE basket_contracts (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    basket_id TEXT NOT NULL,
    contract_json TEXT NOT NULL,
    contract_fingerprint TEXT NOT NULL,
    updated_at_ms INTEGER NOT NULL, progress_json TEXT NOT NULL DEFAULT '', progress_seq INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, source, basket_id)
);
INSERT INTO basket_contracts VALUES('paper','','xs-test','{"v":1,"basket_id":"xs-test","partition":"paper","ranker":"basket_scripted_fixture","params":{"weights":{"AVAX":-0.25,"BTC":0.25,"ETH":0.25,"SOL":-0.25}},"rebalance_interval_seconds":86400,"evaluation_window_seconds":1800,"ranker_timeout_seconds":60,"lookback_bars":48,"gross_budget_usd":4000,"max_transition_net_usd":1100,"max_coin_notional_usd":1500,"min_eligible_members":3,"eligibility_source":"operator listing ledger","eligibility_version":"2026-10-01","min_history_bars":24,"listings":{"AVAX":{"listed_at_ms":1577836800000,"known_at_ms":1577836800000},"BTC":{"listed_at_ms":1577836800000,"known_at_ms":1577836800000},"ETH":{"listed_at_ms":1577836800000,"known_at_ms":1577836800000},"SOL":{"listed_at_ms":1577836800000,"known_at_ms":1577836800000}},"members":[{"strategy_id":"hl-bsk-avax","coin":"AVAX","timeframe":"1h","interval_ms":3600000},{"strategy_id":"hl-bsk-btc","coin":"BTC","timeframe":"1h","interval_ms":3600000},{"strategy_id":"hl-bsk-eth","coin":"ETH","timeframe":"1h","interval_ms":3600000},{"strategy_id":"hl-bsk-sol","coin":"SOL","timeframe":"1h","interval_ms":3600000}]}','8a053f7f4fabe6606bb8b35b78cc959bdf586438b7b895e8a1ccfc13ab962018',1793577660000,'',0);
INSERT INTO basket_contracts VALUES('paper','','btc-xvenue','{"v":1,"basket_id":"btc-xvenue","partition":"paper","ranker":"","rebalance_interval_seconds":0,"evaluation_window_seconds":0,"ranker_timeout_seconds":0,"lookback_bars":0,"gross_budget_usd":0,"max_transition_net_usd":0,"max_coin_notional_usd":0,"min_eligible_members":0,"eligibility_source":"","eligibility_version":"","min_history_bars":0,"listings":{},"members":[{"strategy_id":"pair-hl-btc","coin":"BTC","timeframe":"1h","interval_ms":3600000},{"strategy_id":"pair-okx-btc","coin":"BTC","timeframe":"1h","interval_ms":3600000}],"mode":"pair","pair_v":2,"pair":{"underlying":"BTC","policy":"pair_scripted_fixture","params":{"entry_bps":5},"limits":{"gross_budget_usd":12000,"hyperliquid_notional_cap_usd":7000,"okx_notional_cap_usd":7000,"unmatched_cap_usd":4000,"decision_cadence_ms":60000,"acceptance_deadline_ms":30000,"recovery_age_ms":600000,"max_hold_age_ms":3600000,"retry_initial_ms":5000,"retry_max_ms":40000,"quote_age_ms":5000,"collection_timeout_ms":2000,"receive_skew_ms":250,"envelope_max_bytes":1048576,"protective_width_bps":50},"book_need":{"depth":5,"window_ms":60000,"capacity":256},"members":[{"member_id":"pair-hl-btc","platform":"hyperliquid","type":"perps","provider":"hyperliquid","account_key":"synthetic:hl-research","instrument":"BTC","symbol":"BTC","timeframe":"1h","close_strategy":"tiered_tp_pct","meta":{"native_unit":"base","base_asset":"BTC","contract_size":1,"quote_currency":"USDC","settlement_currency":"USDC","tick_step":0.001,"lot_step":0.001,"min_qty":0.001,"min_notional":10,"trading_status":"live","linear":true,"position_mode":"one_way","margin_mode":"cross","max_leverage":20,"initial_margin_rate":0.05,"maintenance_margin_rate":0.02,"price_currency":"USDC","price_rule":"hyperliquid_perps_v1","sz_decimals":3,"tick_step_text":"","lot_step_text":"","fee_currencies":["USDC"]},"source":"sealed_feed","modeled_delivery_ms":0,"cost_policy":{"model_version":0,"taker_fee_pct":0.00045,"slippage_pct":0.0005,"fill_role":"taker_all","spread":"inside_slippage"},"leverage":2},{"member_id":"pair-okx-btc","platform":"okx","type":"perps","provider":"okx","account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","symbol":"BTC","timeframe":"1h","close_strategy":"atr_stop","meta":{"native_unit":"contract","base_asset":"BTC","contract_size":0.01,"quote_currency":"USDT","settlement_currency":"USDT","tick_step":0.1,"lot_step":1,"min_qty":1,"min_notional":0,"trading_status":"live","linear":true,"position_mode":"net","margin_mode":"cross","max_leverage":20,"initial_margin_rate":0.05,"maintenance_margin_rate":0.02,"price_currency":"USDT","price_rule":"fixed_tick_v1","sz_decimals":0,"tick_step_text":"0.1","lot_step_text":"1","fee_currencies":["USDT"]},"source":"public_snapshot","modeled_delivery_ms":0,"cost_policy":{"model_version":0,"taker_fee_pct":0.00045,"slippage_pct":0.0005,"fill_role":"taker_all","spread":"inside_slippage"},"leverage":2}]}}','d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90',1790856061000,'{"v":2,"seq":10,"lifecycle":"matched","origin_slot_ms":1790856000000,"first_fill_ms":1790856001000,"last_bound":{"at_ms":1790856061000,"assumptions":["independent no-fill and full-fill endpoints for every unresolved request","each member''s own best ask from its checked reference, converted with its price currency","saved request price bounds widen the upper valuation","free collateral is the smaller of book and account evidence, posted margin counted once"],"readings":{"gross_usd":7230.005999999999,"hyperliquid_usd":3624,"okx_usd":3606.006,"unmatched_usd":3606.006}},"cost_evidence":{"contract_fingerprint":"d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90","members":[{"binding":{"contract_fingerprint":"d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90","cost_policy_rates":{"fill_role":"taker_all","slippage_pct":"0.0005","spread":"inside_slippage","taker_fee_pct":"0.00045"},"execution_model":"virtual_ioc_adverse_max_v1","member_key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"v":2},"member_id":"pair-hl-btc","model_version":1001,"policy_id":"pair-cost-dc425e59866a8c9a77715582604e59a859a2ed240efeba31ecd115292a7a97c6"},{"binding":{"contract_fingerprint":"d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90","cost_policy_rates":{"fill_role":"taker_all","slippage_pct":"0.0005","spread":"inside_slippage","taker_fee_pct":"0.00045"},"execution_model":"virtual_ioc_adverse_max_v1","member_key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"v":2},"member_id":"pair-okx-btc","model_version":1001,"policy_id":"pair-cost-1bf37071793e7392f966f9a85d970a3a6718a30312a8634333e91f8933a374aa"}],"v":1},"funding_binding":{"anchor_ms":0,"contract_fingerprint":"d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90","digest":"c0eb04834827a31a644c2f750f2f93fb8cd80eb51eb25fb30b4fca1c015ca43a","members":[{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"}],"rules":["hyperliquid_hourly_oracle_v1","okx_swap_realized_mark_v1"],"rules_digest":"757ef01a927a5d72003d498f9a124e0e84f8c23c091b4f93b7008541c23b5beb","source_id":"fixture-funding","source_kind":"frozen","v":1}}',10);
CREATE TABLE basket_rebalances (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    basket_id TEXT NOT NULL,
    deadline_ms INTEGER NOT NULL CHECK (deadline_ms > 0),
    status TEXT NOT NULL CHECK (status IN ('reserved','ranking','targets_frozen','executing','completed','held','superseded')),
    contract_fingerprint TEXT NOT NULL,
    snapshot_id TEXT NOT NULL DEFAULT '',
    generation INTEGER NOT NULL DEFAULT 0,
    cutoff_ms INTEGER NOT NULL DEFAULT 0,
    input_fingerprint TEXT NOT NULL DEFAULT '',
    evidence_json TEXT NOT NULL DEFAULT '',
    targets_json TEXT NOT NULL DEFAULT '',
    hold_reason TEXT NOT NULL DEFAULT '',
    outcome_json TEXT NOT NULL DEFAULT '',
    increases_frozen INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (scope, source, basket_id, deadline_ms)
);
INSERT INTO basket_rebalances VALUES('paper','','xs-test',1793577600000,'completed','8a053f7f4fabe6606bb8b35b78cc959bdf586438b7b895e8a1ccfc13ab962018','test/1793577600',7,1793577600000,'fp-test','{"eligibility":{"AVAX":{"symbol":"AVAX","status":"eligible","reason":"","closed_bars":48,"boundary_ms":0,"bar_open_ms":0,"input_sha256":"","mark":30.07379405048801,"listed_at_ms":0,"known_at_ms":0},"BTC":{"symbol":"BTC","status":"eligible","reason":"","closed_bars":48,"boundary_ms":0,"bar_open_ms":0,"input_sha256":"","mark":60169.662983517395,"listed_at_ms":0,"known_at_ms":0},"ETH":{"symbol":"ETH","status":"eligible","reason":"","closed_bars":48,"boundary_ms":0,"bar_open_ms":0,"input_sha256":"","mark":3008.9832747540354,"listed_at_ms":0,"known_at_ms":0},"SOL":{"symbol":"SOL","status":"eligible","reason":"","closed_bars":48,"boundary_ms":0,"bar_open_ms":0,"input_sha256":"","mark":150.44123624526,"listed_at_ms":0,"known_at_ms":0}},"scores":{"AVAX":-0.25,"BTC":0.25,"ETH":0.25,"SOL":-0.25}}','{"v":1,"gross_budget_usd":4000,"members":{"hl-bsk-avax":{"strategy_id":"hl-bsk-avax","coin":"AVAX","weight":-0.25,"score":-0.25,"target_notional_usd":-1000,"mark_px":30.07379405048801,"target_qty":-33.25,"residual_usd":-0.046347821273618806},"hl-bsk-btc":{"strategy_id":"hl-bsk-btc","coin":"BTC","weight":0.25,"score":0.25,"target_notional_usd":1000,"mark_px":60169.662983517395,"target_qty":0.01661,"residual_usd":0.5818978437760777},"hl-bsk-eth":{"strategy_id":"hl-bsk-eth","coin":"ETH","weight":0.25,"score":0.25,"target_notional_usd":1000,"mark_px":3008.9832747540354,"target_qty":0.3323,"residual_usd":0.11485779923408651},"hl-bsk-sol":{"strategy_id":"hl-bsk-sol","coin":"SOL","weight":-0.25,"score":-0.25,"target_notional_usd":-1000,"mark_px":150.44123624526,"target_qty":-6.64,"residual_usd":-1.0701913314736433}}}','','{"deviations":[{"strategy_id":"hl-bsk-avax","kind":"lot_residual","detail":"target $-1000.00 floors to -33.25 lots (residual $-0.05)","at_ms":1793577660000},{"strategy_id":"hl-bsk-btc","kind":"lot_residual","detail":"target $1000.00 floors to 0.01661 lots (residual $0.58)","at_ms":1793577660000},{"strategy_id":"hl-bsk-eth","kind":"lot_residual","detail":"target $1000.00 floors to 0.3323 lots (residual $0.11)","at_ms":1793577660000},{"strategy_id":"hl-bsk-sol","kind":"lot_residual","detail":"target $-1000.00 floors to -6.64 lots (residual $-1.07)","at_ms":1793577660000}],"result":"completed_with_deviations","completed_at_ms":1793577660000}',0,1793577660000,1793577660000);
INSERT INTO basket_rebalances VALUES('paper','','btc-xvenue',1790856000000,'completed','d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90','snap-in-0',1,1790856000500,'3076d430ecbaed2d538c5ce870bb7b4d96448ab4ac9de41dd372512ce4303aaf','{"contract_fingerprint":"d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90","cutoff_ms":1790856000500,"deadline_ms":1790856002000,"generation":1,"members":[{"account":{"collateral_currency":"USDC","equity":"6000","evidence_hash":"2cdb5781c3d703dc75ecba6a2d44816fc294487e7cc039b82cd36259210a905b","evidence_id":"acct-pair-hl-btc","first_receipt_ms":1700000000500,"free_collateral":"6000","key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"leverage":"2","margin_mode":"cross","margin_used":"0","observed_ms":1700000000000,"position_mode":"one_way","provenance":"research-schedule:fixture","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000},"capability":{"evidence_hash":"f7695767c1b3b544dc92f182fa4ad8a1ae5749e1072f70e4d1894008a4f64f20","evidence_id":"cap-pair-hl-btc","first_receipt_ms":1700000000500,"key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"meta":{"base_asset":"BTC","contract_size":"1","fee_currencies":["USDC"],"linear":true,"lot_step":"0.001","lot_step_text":"","max_leverage":"20","min_notional":"10","min_qty":"0.001","native_unit":"base","price_currency":"USDC","price_rule":"hyperliquid_perps_v1","quote_currency":"USDC","settlement_currency":"USDC","sz_decimals":3,"tick_step":"0.001","tick_step_text":"","trading_status":"live"},"metadata_generation":1,"observed_ms":1700000000000,"provenance":"hyperliquid_info_meta_v1","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000},"conversions":[{"currency":"USDC","evidence_hash":"efc195f8358092c49c638543375ed283518a0ad576925ea604af9fb6af959b26","evidence_id":"conv-pair-hl-btc-USDC","first_receipt_ms":1700000000500,"key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"observed_ms":1700000000000,"provenance":"research-schedule:fixture","rate_usd":"1","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000}],"key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"observation":{"asks":[{"base_qty":"0.5","px":"60001"},{"base_qty":"0.6","px":"60002"},{"base_qty":"0.7","px":"60003"},{"base_qty":"0.8","px":"60004"},{"base_qty":"0.9","px":"60005"}],"bids":[{"base_qty":"0.5","px":"59999"},{"base_qty":"0.6","px":"59998"},{"base_qty":"0.7","px":"59997"},{"base_qty":"0.8","px":"59996"},{"base_qty":"0.9","px":"59995"}],"contract_size":"1","coverage":{"from_ms":1790855879950,"gaps":[],"identity_semantics":"sha256-lp(host,namespace,coin,event_ms,levels)/1635-v1","prefix_complete":true,"required_capacity":256,"required_depth":5,"required_window_ms":60000,"source_contract":"hyperliquid_ws_l2Book","status":"complete","through_ms":1790855999950,"time_semantics":"source_event_ms+first_receipt_ms/1635-v1"},"first_receipt_ms":1790855999850,"generation":1,"key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"modeled_delivery_ms":0,"native_asks":[{"px":"60001","qty":"0.5"},{"px":"60002","qty":"0.6"},{"px":"60003","qty":"0.7"},{"px":"60004","qty":"0.8"},{"px":"60005","qty":"0.9"}],"native_bids":[{"px":"59999","qty":"0.5"},{"px":"59998","qty":"0.6"},{"px":"59997","qty":"0.7"},{"px":"59996","qty":"0.8"},{"px":"59995","qty":"0.9"}],"native_unit":"base","normalization_id":"base*1@cap-pair-hl-btc","payload":"eyJhdmFpbGFibGUiOnRydWUsImNhcGFjaXR5IjoyNTYsImNvaW4iOiJCVEMiLCJjb250cmFjdCI6Imh5cGVybGlxdWlkLWwyYm9vay1uYXRpdmUtc2xvdy8xNjM1LXYxIiwiY3V0b2ZmX21zIjoxNzkwODU1OTk5OTUwLCJkZXB0aCI6NSwiaWRlbnRpdHlfc2VtYW50aWNzIjoic2hhMjU2LWxwKGhvc3QsbmFtZXNwYWNlLGNvaW4sZXZlbnRfbXMsbGV2ZWxzKS8xNjM1LXYxIiwia2luZCI6ImwyX2Jvb2siLCJtYXJrZXJzIjpbXSwibWF4X2Z1dHVyZV9za2V3X21zIjoyNTAsIm1heF9nYXBfbXMiOjEwMDAwLCJtYXhfbGFnX21zIjo1MDAwLCJvYnNlcnZhdGlvbnMiOlt7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0MDgwMCwiaWRlbnRpdHkiOiI3YTI0ZDkyYWZlMjczZGJmYjRkNGRhMTcxMmNlNDQzMzA5NTg3Mjc5NjA3N2VhMTAzMWIzMTk3Y2Y3ZGZjMTNhIiwicmVjdl9tcyI6MTc5MDg1NTk0MDg1MCwic2VxIjoxLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0MTgwMCwiaWRlbnRpdHkiOiI2YmVjZGE2NzUxMTZiZGNjYjM4NDI3NzM5MjU1Yjg5YTBmN2RiMmRhYThiYTU5M2ZiOGQ4NDFmOTg4OWUyNWY2IiwicmVjdl9tcyI6MTc5MDg1NTk0MTg1MCwic2VxIjoyLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0MjgwMCwiaWRlbnRpdHkiOiJkODU5MGFlMDYwNDFjNmY0ZGQ0NGJlMzcyZDI1NTU4NGVlYThlMmI2MTIyYjY4OWY0N2VkOWJlNTVlYmU3ZjliIiwicmVjdl9tcyI6MTc5MDg1NTk0Mjg1MCwic2VxIjozLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0MzgwMCwiaWRlbnRpdHkiOiJlM2I0NWY1ZTEyMzhmOWRmZjU0MTI5YzFhNTRkNTNkODU1NzQyOGU2NTJkZmMwMjgwMmVjNTQ1Njc2Yjg0NzkwIiwicmVjdl9tcyI6MTc5MDg1NTk0Mzg1MCwic2VxIjo0LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0NDgwMCwiaWRlbnRpdHkiOiI0MDBjMTQzMjI0ZGFjOGYyNjZiMzkzZTkwMDQ1NzIzMzU5YWQxNTQ2ZjllZjFjYzgyM2VmOGNiZmNjMzgyYTkxIiwicmVjdl9tcyI6MTc5MDg1NTk0NDg1MCwic2VxIjo1LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0NTgwMCwiaWRlbnRpdHkiOiJiNmMxMmQ2M2NiYWYzZDhlYWRiZmY5MTI5ZjBkNjhhOTI4Y2M2N2QyZTIzOTFhZWM5YjYyM2ZlZjgwODFlYjUxIiwicmVjdl9tcyI6MTc5MDg1NTk0NTg1MCwic2VxIjo2LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0NjgwMCwiaWRlbnRpdHkiOiIzMmJmNTdhYjFjZDY0ZjZlMzZhMzVlMWU5ODQ4NTljNThhZGUzNDg5NmM5ZjdlZjcwYWE5MTNmMTQ2MmUyNGE1IiwicmVjdl9tcyI6MTc5MDg1NTk0Njg1MCwic2VxIjo3LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0NzgwMCwiaWRlbnRpdHkiOiIwODcwNDlmMmFiMzVhZTUxNDBiYTczY2VkMzJiMjU4YzU0ODY0YjBlZTMzNTA3NDFjYTkzMTBlODdiOWZhYjdjIiwicmVjdl9tcyI6MTc5MDg1NTk0Nzg1MCwic2VxIjo4LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0ODgwMCwiaWRlbnRpdHkiOiIzODdhYjAxMTA1NDE3MDNhZDM5OTdhNTBkYTQzNTU3YWUwOTE0NTg0YTI4Y2Q4Mjc4YzE5NjFhMzc5NmUxNGExIiwicmVjdl9tcyI6MTc5MDg1NTk0ODg1MCwic2VxIjo5LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk0OTgwMCwiaWRlbnRpdHkiOiJiMGE0MmEyNjE3ZWFjMzExMWZiYTY3OTZjMTllODRjNTg0OTZjM2UyNzhjMmE2MjRhZWE5NzVlMDVmOWY3MjRmIiwicmVjdl9tcyI6MTc5MDg1NTk0OTg1MCwic2VxIjoxMCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5NTA4MDAsImlkZW50aXR5IjoiNDdmYWVhYzQ5OGJjYmI0ZjgzNTA0Mzk5MzU0NWU5MmU5YjIzNGI2MGQyMjdmMjY1YjdhNzMyNTVlODNhNWNjNyIsInJlY3ZfbXMiOjE3OTA4NTU5NTA4NTAsInNlcSI6MTEsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTUxODAwLCJpZGVudGl0eSI6IjhkZTJjOTUwNGEyN2QyZmQ1NWVjZTg5MzhjNjVjMGM5MTc5MmY3ZTA2NTAzNGJjODRhYjMzNTU4MWMxMTlkNTkiLCJyZWN2X21zIjoxNzkwODU1OTUxODUwLCJzZXEiOjEyLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk1MjgwMCwiaWRlbnRpdHkiOiJkYWQ4YmU4NzNkODY1NzM2ZTI0Yjc1NWY0ZDc4MDUwNWQxZGNhM2YxN2UxZjFkMmU2MzYzYzMwOTQ3MzkyNjI3IiwicmVjdl9tcyI6MTc5MDg1NTk1Mjg1MCwic2VxIjoxMywic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5NTM4MDAsImlkZW50aXR5IjoiNWM2ZjllMTZjODE3MjVkOTgxMWU4NDczOWU2ZDU3OTQ5ZThlM2NmZjYyNDQ1M2MzMTkwNjQxOTIzZWU5OTEwZiIsInJlY3ZfbXMiOjE3OTA4NTU5NTM4NTAsInNlcSI6MTQsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTU0ODAwLCJpZGVudGl0eSI6Ijk5NzdjODliMWI5NWI0Y2U2MThmNzU4MGJkNGZkMzBiOTJhYmI3MDM0MGM0YmIzZmJkODQ1ZDdlYWEwZTk2NjAiLCJyZWN2X21zIjoxNzkwODU1OTU0ODUwLCJzZXEiOjE1LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk1NTgwMCwiaWRlbnRpdHkiOiJiZmIxZjhiZTUzNGZjOWIyNjk0ZGI0NWEwN2I5MjcyNDk4NWRjMDY2NjgzMzI0ZTMyZGRhZWY5YWFmODg4ZWM3IiwicmVjdl9tcyI6MTc5MDg1NTk1NTg1MCwic2VxIjoxNiwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5NTY4MDAsImlkZW50aXR5IjoiNWY0MDQ3YTkzMjJmNTdlNDlmYTE5NzE3ZDAxMmQ3Zjc0NzRmYjUzODc3ZDdiMzkxMGY5YjEyMDY3YTZhMmQ2MSIsInJlY3ZfbXMiOjE3OTA4NTU5NTY4NTAsInNlcSI6MTcsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTU3ODAwLCJpZGVudGl0eSI6ImU4YzhlMDZjNjhiNDg1ZmY3YTM1MmI0NmQ4N2M3MGYzNWFhNGU4YWY0NDc2N2RmOGUyNGRjOTQxMDhlNjkyOGQiLCJyZWN2X21zIjoxNzkwODU1OTU3ODUwLCJzZXEiOjE4LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk1ODgwMCwiaWRlbnRpdHkiOiJmNjY0Mjc4NjQ3YzI4ZmFlYjJmNzU1ZGIwNjVmYjRhOTZkZmNmMDZiODhlZDVjOGJjYzYxYmY5ZWFkYjUzYmMzIiwicmVjdl9tcyI6MTc5MDg1NTk1ODg1MCwic2VxIjoxOSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5NTk4MDAsImlkZW50aXR5IjoiNDBiYmNiNzc3NTU2ODI2YWU5YjE2MzBiZWU0MDA5M2NiODhiNDJhZGQyYmE1OTA1YTI4NmMzYzRjNDhhODk0MSIsInJlY3ZfbXMiOjE3OTA4NTU5NTk4NTAsInNlcSI6MjAsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTYwODAwLCJpZGVudGl0eSI6IjQ3YjE2YTM0NjEwMDk2ZGZiZGZmMWQxMjIxMjM4YjU2MTNkZGE1ZTUyNWYzYjBlYmM5NGMzODVjNzRkNGQ1MTAiLCJyZWN2X21zIjoxNzkwODU1OTYwODUwLCJzZXEiOjIxLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk2MTgwMCwiaWRlbnRpdHkiOiJjYmQ2M2Y3MjcxYTMyZDEzMWQyM2JkZTJjMmIzMjBlZmNlODQyMzJhOGJhYTg4YTYzOTdiNmNkNmMwNzM4Y2Y2IiwicmVjdl9tcyI6MTc5MDg1NTk2MTg1MCwic2VxIjoyMiwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5NjI4MDAsImlkZW50aXR5IjoiZDA0YjViZjVmNjdiZTdkNTBjN2ViNzk3NjJjZGIzZDg1YTEzYTRhNzhmZjkzODE5NWNjNzUyY2NlYjk4YTlmMyIsInJlY3ZfbXMiOjE3OTA4NTU5NjI4NTAsInNlcSI6MjMsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTYzODAwLCJpZGVudGl0eSI6Ijc3NTMzMzQ2YjIxOWE1NjNmNjY5MDk0ZmY0ZmM0OWViOTU2MDI3Yzk0ZmU3MjI5MzYxZjg2Y2JkNGMzYTE1NDQiLCJyZWN2X21zIjoxNzkwODU1OTYzODUwLCJzZXEiOjI0LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk2NDgwMCwiaWRlbnRpdHkiOiI5Nzg5ZWEzYjIxZmZkZDNmODFhMDhjOGE2NTY2ZDdhOGNmMTJkYzQ3OWI0ZjFlMWY1OGIzMmUxMmNlOWJhYzg4IiwicmVjdl9tcyI6MTc5MDg1NTk2NDg1MCwic2VxIjoyNSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5NjU4MDAsImlkZW50aXR5IjoiZTAyYjcxMjM4OTA4YWM0ZjhlMDdiYTNkOGUzMzdlN2E3N2RlYTMyNTBiNzJkZDFiMjE3ODBiNjAxZDM5ZmMwNCIsInJlY3ZfbXMiOjE3OTA4NTU5NjU4NTAsInNlcSI6MjYsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTY2ODAwLCJpZGVudGl0eSI6IjRhZmZkMGIxMGIxMDZkNTA1NDc4YjU1NjkyODRmMGJlMTNiNjU4N2M0OWIwNjEzM2ZlMGY3YTkwOTU4MmUyOGUiLCJyZWN2X21zIjoxNzkwODU1OTY2ODUwLCJzZXEiOjI3LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk2NzgwMCwiaWRlbnRpdHkiOiJiZWEyMmYyODFhNGY4YjRmNjFkMzlmMjFiZDVjYWEyYTNlNTU1MjUxNWJlM2Y2Mzg1MjBjYzc2MDJmMWNhNTVlIiwicmVjdl9tcyI6MTc5MDg1NTk2Nzg1MCwic2VxIjoyOCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5Njg4MDAsImlkZW50aXR5IjoiMjAxNzA4MDFlODYzYjY5YzdhMzQwOTQwMGQ0ODkzNTI3MWY3ODJiNmYyYjYzYjA2NmVmZTQ0Yjg0NzgwMzg3NyIsInJlY3ZfbXMiOjE3OTA4NTU5Njg4NTAsInNlcSI6MjksInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTY5ODAwLCJpZGVudGl0eSI6IjBmNmY3MDNlZjM0YTU1OGM2YjNkY2E5YTU4Y2FiNjhlZjYyZjdiMTUzZGViMWM5YjYwM2E2YzI4MDFkNjY1MDIiLCJyZWN2X21zIjoxNzkwODU1OTY5ODUwLCJzZXEiOjMwLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk3MDgwMCwiaWRlbnRpdHkiOiJhNzEwNjIwMzY0MzQ0YzQ0NThlNjI3NWI4ZDZmOWUyODc4MmJhMGYyNjZmMGYwZmI0NjM2YzZmOWMzMWJmNDZhIiwicmVjdl9tcyI6MTc5MDg1NTk3MDg1MCwic2VxIjozMSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5NzE4MDAsImlkZW50aXR5IjoiZTQ0NzIyYjkyNTliM2JmNWU5YzkwN2JiYjhmNjNhMGI1OTFlZDQyM2EyZjM5ZTQwZmU2ODQ3OTE2ZDY4NTAyZSIsInJlY3ZfbXMiOjE3OTA4NTU5NzE4NTAsInNlcSI6MzIsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTcyODAwLCJpZGVudGl0eSI6IjEzMDllZDkzN2UyYzc4Y2E1ZDEyNDYzOWI3YWZiNTJlZTg5Nzk2YzM2NWMxMjQ2MDM3MjhhMWUyZDE2OTFjMjYiLCJyZWN2X21zIjoxNzkwODU1OTcyODUwLCJzZXEiOjMzLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk3MzgwMCwiaWRlbnRpdHkiOiJlMTlmN2ExMWM3MmZkNWY5YTFjNTA3ZmIwNzZlNzlkN2FlMTNiZWRhN2U1NGRiY2JiY2IyOGYxZDYzN2Q1YmI2IiwicmVjdl9tcyI6MTc5MDg1NTk3Mzg1MCwic2VxIjozNCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5NzQ4MDAsImlkZW50aXR5IjoiMjAxZTg5NTQ0MDg5ZTgxMDE3MDdmYmNkMTM4YjBjNTYxZGM4MDRjZjVhMjQyZjFjODEwMWJjODUxZTBkYjhlYyIsInJlY3ZfbXMiOjE3OTA4NTU5NzQ4NTAsInNlcSI6MzUsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTc1ODAwLCJpZGVudGl0eSI6IjE1MzAxZjM0ZjhhYmFkMThjNjExNmJjYmZkYjgyNWQ4MzU0OTNiMGYzZjk3Y2IyYWE3MzYyYWViMjgwNWY5MjkiLCJyZWN2X21zIjoxNzkwODU1OTc1ODUwLCJzZXEiOjM2LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk3NjgwMCwiaWRlbnRpdHkiOiI1NmUwNTMwMzkwZTczMGZlNTU4MGM1M2ZhNzZkMzUzODhiMmRkNmUzZTE0OTM0MmJjY2Y3ZTFlNTFmODRjMWQ1IiwicmVjdl9tcyI6MTc5MDg1NTk3Njg1MCwic2VxIjozNywic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5Nzc4MDAsImlkZW50aXR5IjoiZGI3MWI4MjU5OTg3YTdhMjY5YmIxZDg2MGY0N2NiZGJiYmNjODMzZTkxNzIwN2YzMzcxY2QzZmQ2MTczNTcwYyIsInJlY3ZfbXMiOjE3OTA4NTU5Nzc4NTAsInNlcSI6MzgsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTc4ODAwLCJpZGVudGl0eSI6IjU0OWI2N2YyYWUyNGI5MDA1NmM3NzVhNWFlZDRmYjRlOWI4NDJjM2VjMzQwMTU5OTY5ZWQxYWQ3YjdmNDAyMDYiLCJyZWN2X21zIjoxNzkwODU1OTc4ODUwLCJzZXEiOjM5LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk3OTgwMCwiaWRlbnRpdHkiOiJiZjRlOGE4YWY4NDk0N2VlNGZiYzI3MTFlNDBkNzc5ODQ4N2NmMDg3NjdmZDRlYTRhOTVlMGQ4NjUzN2NmMWM2IiwicmVjdl9tcyI6MTc5MDg1NTk3OTg1MCwic2VxIjo0MCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5ODA4MDAsImlkZW50aXR5IjoiMGIxM2QxYWExOTQ1MzBmYWU3ZjI0YjY5ZmJlMzE2YTM2OTIyN2IxYzRmOTBmMTM2MzY3NWVlZGJiODZlZmZlOSIsInJlY3ZfbXMiOjE3OTA4NTU5ODA4NTAsInNlcSI6NDEsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTgxODAwLCJpZGVudGl0eSI6ImZiMjg1YjU4MDM0MGIwZGEzNzA2ZGYzODY1MzM5M2RhNjNkZmQyODcwZDM0MzA3YTFmNjEyMmM3ZWYzZmZiNGIiLCJyZWN2X21zIjoxNzkwODU1OTgxODUwLCJzZXEiOjQyLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk4MjgwMCwiaWRlbnRpdHkiOiJmOGMxNjNkMzU4MTY2MjVjYmJmMWRkMDc3NDNmMmU5NzZjZDIyNGEzOTRjMWNjOTNjMDM0MzMwYjdkN2IxZWUwIiwicmVjdl9tcyI6MTc5MDg1NTk4Mjg1MCwic2VxIjo0Mywic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5ODM4MDAsImlkZW50aXR5IjoiMTM5MDYwZTg5ZTE4MGE3YjJmNzg3YjA0NDJjNzEzNGExYzViNDU1YmRmOTEwNjAwMzE0MzMxNTRkMGFkMjQyNCIsInJlY3ZfbXMiOjE3OTA4NTU5ODM4NTAsInNlcSI6NDQsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTg0ODAwLCJpZGVudGl0eSI6ImMzYWNjNmU2ZTM3OGFkNjc1NzkwYTA5YzVkMjBiNDEyYjM2NWZhOWRmYmNiZDVkNWNjMjhiZjIxM2NmYWI5NjEiLCJyZWN2X21zIjoxNzkwODU1OTg0ODUwLCJzZXEiOjQ1LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk4NTgwMCwiaWRlbnRpdHkiOiI3NGVmNjZiZTNhNTc0MjkwMDhhYTFiNWFkMzI5YWFiNDg3ODNhZGFlYTJmYWU0NGRmZGU5OTYyNmY4NjcyYmVjIiwicmVjdl9tcyI6MTc5MDg1NTk4NTg1MCwic2VxIjo0Niwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5ODY4MDAsImlkZW50aXR5IjoiYzI3NjQyYTQ4Zjg2YjY1YjYwMmQ2ZTBlNWJkMDdjOTliNzVhMWY4MzI1YzRhYzkyNGFiOTcwOWU4MzIwMmRiYiIsInJlY3ZfbXMiOjE3OTA4NTU5ODY4NTAsInNlcSI6NDcsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTg3ODAwLCJpZGVudGl0eSI6ImJmNTgzNDI5ZmYwYzAxMDVhMDMxZDdhNGNkMmI1MDUwOWJmOGMyZGMwNjlkNDY2MjMzZTBkZjk0MDUxODI0MzUiLCJyZWN2X21zIjoxNzkwODU1OTg3ODUwLCJzZXEiOjQ4LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk4ODgwMCwiaWRlbnRpdHkiOiJjMDBlMzI1NGYxYmY1ZTUyOGM4MjU3OWJjOTQ1ZTk3YmQ3OGQxYTYyOGE2YWI0NTE3NDYwMjQ0OTQ5ODMxZmFmIiwicmVjdl9tcyI6MTc5MDg1NTk4ODg1MCwic2VxIjo0OSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5ODk4MDAsImlkZW50aXR5IjoiYzk2NDA1YWQyNTZlZDJkODQyMDUwZTE2NWRkZjA4N2EyYzVjOTM5ZTU5NTZjMzdjZWRhZDFlM2JiNDc2OTk5MyIsInJlY3ZfbXMiOjE3OTA4NTU5ODk4NTAsInNlcSI6NTAsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTkwODAwLCJpZGVudGl0eSI6IjYwYjk3ZWI5MTg0MWUzZGJhMmRjODBiNGY5MGE5MTA0YmIyOWQ0ZTYyM2RiMWFiNDYxMDE0NmQyN2ExNzQzNWEiLCJyZWN2X21zIjoxNzkwODU1OTkwODUwLCJzZXEiOjUxLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk5MTgwMCwiaWRlbnRpdHkiOiI1ZTg5NDBmNDk5MDlmZjc5YzgzMTk1Y2FhNDNkMDUwMzc2MTg2NjcxNGU1ODhiNTllYjZhNzNhOGI2ZTdhN2M2IiwicmVjdl9tcyI6MTc5MDg1NTk5MTg1MCwic2VxIjo1Miwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5OTI4MDAsImlkZW50aXR5IjoiMTI3MTZhZTIwOGE0YzJmY2QyZmQ4YTU3YzNiOTZhNDg2M2E3NTk5OGFhMzBmNzg0ZWIxZmE4NzgzMjk5YzkwNyIsInJlY3ZfbXMiOjE3OTA4NTU5OTI4NTAsInNlcSI6NTMsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTkzODAwLCJpZGVudGl0eSI6IjkyMWVjOTU4NWNjMWIwOTZiZmQxN2Y4Zjc0ZjMzNmM1MjYyNTkyYWViN2E3ODg2MmQ4Y2M0NDNiNjcxNmZmNDciLCJyZWN2X21zIjoxNzkwODU1OTkzODUwLCJzZXEiOjU0LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk5NDgwMCwiaWRlbnRpdHkiOiIzOTU3MzQwOWVjZGYwZmUzMzdkZjg1ZGE5MzgwYTMzZDJhMjZmNmEzZWVjN2VjYTlkMjhmZmYxMGIxMTVlZTY4IiwicmVjdl9tcyI6MTc5MDg1NTk5NDg1MCwic2VxIjo1NSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5OTU4MDAsImlkZW50aXR5IjoiYmVlZWY1MjE3NjI2NjY1NTk4OWVjNDJhMzFlZjc1YWRkYzIyMmE0YWFjYzdhNzUxNGNhM2FlNzg2MzhkODEzZiIsInJlY3ZfbXMiOjE3OTA4NTU5OTU4NTAsInNlcSI6NTYsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTk2ODAwLCJpZGVudGl0eSI6ImZhMWJiMTg4Yjc3YjgxM2ZlNmUwMmFmMmYwYjIzMWRiMjlhMTI5ZjAyMjY3YzI4YWRmNTRhMDZhYWRmYjM1YjIiLCJyZWN2X21zIjoxNzkwODU1OTk2ODUwLCJzZXEiOjU3LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NTk5NzgwMCwiaWRlbnRpdHkiOiJkNjUxODQ2MzE1ODBkYjQ0MTU4YWRjZTU5Mzc4NGMwZGZkOTQ1OWMyMjc4Y2NiMTEzYmRhYmY0YzIzODYyMTZlIiwicmVjdl9tcyI6MTc5MDg1NTk5Nzg1MCwic2VxIjo1OCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTU5OTg4MDAsImlkZW50aXR5IjoiZjUzOTc4OTUyM2M5NmU2ODkzMTNkZjNlMDk3OTUyODFmNWY4M2E0ZWU2MDAzNTM1OThjNjg0NjJhNzJkNTBmNSIsInJlY3ZfbXMiOjE3OTA4NTU5OTg4NTAsInNlcSI6NTksInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU1OTk5ODAwLCJpZGVudGl0eSI6IjMxYzQxOWYzNjgyOTM1YjA0ZjJhNjVjOGMzMzFlNWE4Yzk3NzZiN2RmZjViOWViOWExNTcyNTZkYzE5YWM2ZmMiLCJyZWN2X21zIjoxNzkwODU1OTk5ODUwLCJzZXEiOjYwLCJzZXNzaW9uIjoxfV0sIm93bmVyX2luc3RhbmNlIjoiZml4dHVyZS1mZWVkIiwicHJlZml4X2NvbXBsZXRlIjp0cnVlLCJyZXRhaW5lZF9mcm9tX21zIjoxNzkwODU1ODc5OTUwLCJzbmFwc2hvdF9pZCI6InNuYXAtaW4tMCIsInNvdXJjZSI6Imh5cGVybGlxdWlkX3dzX2wyQm9vayIsInRpbWVfc2VtYW50aWNzIjoic291cmNlX2V2ZW50X21zK2ZpcnN0X3JlY2VpcHRfbXMvMTYzNS12MSIsIndpbmRvd19tcyI6NjAwMDB9","payload_hash":"71cf409b0841716c6b7136c1fcb0c8509adc0f9907983da01d5c6d8a201365cd","price_currency":"USDC","provenance":"hyperliquid_ws_l2Book@fixture-feed","snapshot_id":"snap-in-0","source":"sealed_feed","source_cutoff_ms":1790855999950,"v":2,"venue_time_ms":1790855999800}},{"account":{"collateral_currency":"USDT","equity":"6000","evidence_hash":"588cbd52217be98c0bbafc559761f7e0bdea6a8adb06d633488faf400bc9c2d5","evidence_id":"acct-pair-okx-btc","first_receipt_ms":1700000000500,"free_collateral":"6000","key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"leverage":"2","margin_mode":"cross","margin_used":"0","observed_ms":1700000000000,"position_mode":"net","provenance":"research-schedule:fixture","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000},"capability":{"evidence_hash":"47c32c38c23f0db4e2c4f37958ada179ace9977f725761d31258873f74dc2fe2","evidence_id":"cap-pair-okx-btc","first_receipt_ms":1700000000500,"key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"meta":{"base_asset":"BTC","contract_size":"0.01","fee_currencies":["USDT"],"linear":true,"lot_step":"1","lot_step_text":"1","max_leverage":"20","min_notional":"0","min_qty":"1","native_unit":"contract","price_currency":"USDT","price_rule":"fixed_tick_v1","quote_currency":"USDT","settlement_currency":"USDT","sz_decimals":0,"tick_step":"0.1","tick_step_text":"0.1","trading_status":"live"},"metadata_generation":1,"observed_ms":1700000000000,"provenance":"okx_public_instrument_v1","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000},"conversions":[{"currency":"USDT","evidence_hash":"3a34e57783979f039535e4d494bd21dcc0b8e065482ae7168bafeb933e4b1f5a","evidence_id":"conv-pair-okx-btc-USDT","first_receipt_ms":1700000000500,"key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"observed_ms":1700000000000,"provenance":"research-schedule:fixture","rate_usd":"1","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000}],"key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"observation":{"asks":[{"base_qty":"0.03","px":"60100.1"},{"base_qty":"0.04","px":"60100.2"},{"base_qty":"0.05","px":"60100.3"},{"base_qty":"0.06","px":"60100.4"},{"base_qty":"0.07","px":"60100.5"}],"bids":[{"base_qty":"0.03","px":"60099.9"},{"base_qty":"0.04","px":"60099.8"},{"base_qty":"0.05","px":"60099.7"},{"base_qty":"0.06","px":"60099.6"},{"base_qty":"0.07","px":"60099.5"}],"contract_size":"0.01","coverage":{"from_ms":1790855999800,"gaps":[],"identity_semantics":"sha256(canonical_payload)/1871-v1","prefix_complete":true,"required_capacity":1,"required_depth":5,"required_window_ms":0,"source_contract":"okx_public_books_snapshot_v1","status":"complete","through_ms":1790855999800,"time_semantics":"okx_books_ts_ms+reader_receipt_ms/1871-v1"},"first_receipt_ms":1790855999800,"generation":0,"key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"modeled_delivery_ms":0,"native_asks":[{"px":"60100.1","qty":"3"},{"px":"60100.2","qty":"4"},{"px":"60100.3","qty":"5"},{"px":"60100.4","qty":"6"},{"px":"60100.5","qty":"7"}],"native_bids":[{"px":"60099.9","qty":"3"},{"px":"60099.8","qty":"4"},{"px":"60099.7","qty":"5"},{"px":"60099.6","qty":"6"},{"px":"60099.5","qty":"7"}],"native_unit":"contract","normalization_id":"contract*0.01@cap-pair-okx-btc","payload":"eyJyZXF1ZXN0Ijp7Imluc3RJZCI6IkJUQy1VU0RULVNXQVAiLCJzeiI6IjUifSwicmVzcG9uc2UiOnsiY29kZSI6IjAiLCJkYXRhIjpbeyJhc2tzIjpbWyI2MDEwMC4xIiwiMyIsIjAiLCIyIl0sWyI2MDEwMC4yIiwiNCIsIjAiLCIyIl0sWyI2MDEwMC4zIiwiNSIsIjAiLCIyIl0sWyI2MDEwMC40IiwiNiIsIjAiLCIyIl0sWyI2MDEwMC41IiwiNyIsIjAiLCIyIl1dLCJiaWRzIjpbWyI2MDA5OS45IiwiMyIsIjAiLCIyIl0sWyI2MDA5OS44IiwiNCIsIjAiLCIyIl0sWyI2MDA5OS43IiwiNSIsIjAiLCIyIl0sWyI2MDA5OS42IiwiNiIsIjAiLCIyIl0sWyI2MDA5OS41IiwiNyIsIjAiLCIyIl1dLCJ0cyI6IjE3OTA4NTU5OTk3ODAifV0sIm1zZyI6IiJ9fQ==","payload_hash":"18305d0b1c7cf80c4a44da99b03f0276badd747ec1729c5e971ccb527f2d5afd","price_currency":"USDT","provenance":"okx_public_books_snapshot_v1@https://www.okx.com","snapshot_id":"","source":"public_snapshot","source_cutoff_ms":1790855999800,"v":2,"venue_time_ms":1790855999780}}],"pair_id":"btc-xvenue","partition":"paper","slot_ms":1790856000000,"snapshot_id":"snap-in-0","start_ms":1790856000000,"v":2}','{"version":2,"kind":"enter","orientation":"long_hyperliquid_short_okx","slot_ms":1790856000000,"input_fingerprint":"3076d430ecbaed2d538c5ce870bb7b4d96448ab4ac9de41dd372512ce4303aaf","members":{"pair-hl-btc":{"target_base_qty":0.06,"price_bound":60400},"pair-okx-btc":{"target_base_qty":-0.06,"price_bound":59700}},"decision":{"reason":"fixture spread"},"first_leg":"pair-okx-btc","bound_evidence":{"pair-hl-btc":{"proposed_bound":60400,"rounded_bound":60400,"side":"buy","capability_hash":"f7695767c1b3b544dc92f182fa4ad8a1ae5749e1072f70e4d1894008a4f64f20"},"pair-okx-btc":{"proposed_bound":59700,"rounded_bound":59700,"side":"sell","capability_hash":"47c32c38c23f0db4e2c4f37958ada179ace9977f725761d31258873f74dc2fe2"}}}','','{"result":"completed","completed_at_ms":1790856061000}',0,1790856000000,1790856061000);
INSERT INTO basket_rebalances VALUES('paper','','btc-xvenue',1790856060000,'completed','d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90','snap-in-1',1,1790856060500,'82d254e6b4505d81e4fc06e8e57fe63137b925069a77d8b4acc2a2a1fd90ad5b','{"contract_fingerprint":"d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90","cutoff_ms":1790856060500,"deadline_ms":1790856062000,"generation":1,"members":[{"account":{"collateral_currency":"USDC","equity":"6000","evidence_hash":"2cdb5781c3d703dc75ecba6a2d44816fc294487e7cc039b82cd36259210a905b","evidence_id":"acct-pair-hl-btc","first_receipt_ms":1700000000500,"free_collateral":"6000","key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"leverage":"2","margin_mode":"cross","margin_used":"0","observed_ms":1700000000000,"position_mode":"one_way","provenance":"research-schedule:fixture","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000},"capability":{"evidence_hash":"f7695767c1b3b544dc92f182fa4ad8a1ae5749e1072f70e4d1894008a4f64f20","evidence_id":"cap-pair-hl-btc","first_receipt_ms":1700000000500,"key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"meta":{"base_asset":"BTC","contract_size":"1","fee_currencies":["USDC"],"linear":true,"lot_step":"0.001","lot_step_text":"","max_leverage":"20","min_notional":"10","min_qty":"0.001","native_unit":"base","price_currency":"USDC","price_rule":"hyperliquid_perps_v1","quote_currency":"USDC","settlement_currency":"USDC","sz_decimals":3,"tick_step":"0.001","tick_step_text":"","trading_status":"live"},"metadata_generation":1,"observed_ms":1700000000000,"provenance":"hyperliquid_info_meta_v1","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000},"conversions":[{"currency":"USDC","evidence_hash":"efc195f8358092c49c638543375ed283518a0ad576925ea604af9fb6af959b26","evidence_id":"conv-pair-hl-btc-USDC","first_receipt_ms":1700000000500,"key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"observed_ms":1700000000000,"provenance":"research-schedule:fixture","rate_usd":"1","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000}],"key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"observation":{"asks":[{"base_qty":"0.5","px":"60001"},{"base_qty":"0.6","px":"60002"},{"base_qty":"0.7","px":"60003"},{"base_qty":"0.8","px":"60004"},{"base_qty":"0.9","px":"60005"}],"bids":[{"base_qty":"0.5","px":"59999"},{"base_qty":"0.6","px":"59998"},{"base_qty":"0.7","px":"59997"},{"base_qty":"0.8","px":"59996"},{"base_qty":"0.9","px":"59995"}],"contract_size":"1","coverage":{"from_ms":1790855939950,"gaps":[],"identity_semantics":"sha256-lp(host,namespace,coin,event_ms,levels)/1635-v1","prefix_complete":true,"required_capacity":256,"required_depth":5,"required_window_ms":60000,"source_contract":"hyperliquid_ws_l2Book","status":"complete","through_ms":1790856059950,"time_semantics":"source_event_ms+first_receipt_ms/1635-v1"},"first_receipt_ms":1790856059850,"generation":1,"key":{"account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc","partition":"paper","provider":"hyperliquid"},"modeled_delivery_ms":0,"native_asks":[{"px":"60001","qty":"0.5"},{"px":"60002","qty":"0.6"},{"px":"60003","qty":"0.7"},{"px":"60004","qty":"0.8"},{"px":"60005","qty":"0.9"}],"native_bids":[{"px":"59999","qty":"0.5"},{"px":"59998","qty":"0.6"},{"px":"59997","qty":"0.7"},{"px":"59996","qty":"0.8"},{"px":"59995","qty":"0.9"}],"native_unit":"base","normalization_id":"base*1@cap-pair-hl-btc","payload":"eyJhdmFpbGFibGUiOnRydWUsImNhcGFjaXR5IjoyNTYsImNvaW4iOiJCVEMiLCJjb250cmFjdCI6Imh5cGVybGlxdWlkLWwyYm9vay1uYXRpdmUtc2xvdy8xNjM1LXYxIiwiY3V0b2ZmX21zIjoxNzkwODU2MDU5OTUwLCJkZXB0aCI6NSwiaWRlbnRpdHlfc2VtYW50aWNzIjoic2hhMjU2LWxwKGhvc3QsbmFtZXNwYWNlLGNvaW4sZXZlbnRfbXMsbGV2ZWxzKS8xNjM1LXYxIiwia2luZCI6ImwyX2Jvb2siLCJtYXJrZXJzIjpbXSwibWF4X2Z1dHVyZV9za2V3X21zIjoyNTAsIm1heF9nYXBfbXMiOjEwMDAwLCJtYXhfbGFnX21zIjo1MDAwLCJvYnNlcnZhdGlvbnMiOlt7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwMDgwMCwiaWRlbnRpdHkiOiI0M2YxYzVlMzVhNDJjNGYxY2EyNGRjZmUxOTI3NDFmZmFkMTQ0MDU5NGM5MTRiNzUzNjYzOWQyZWY5MTA4MGYzIiwicmVjdl9tcyI6MTc5MDg1NjAwMDg1MCwic2VxIjoxLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwMTgwMCwiaWRlbnRpdHkiOiI3NWI5ZjMwZmJmZWUxYzVlODQ3MTEzNGNlOGRmNzlkYWI5YmQ0MzNjNDI1N2VjNDc0ZDFlOTE1M2Q1Y2NjZjhkIiwicmVjdl9tcyI6MTc5MDg1NjAwMTg1MCwic2VxIjoyLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwMjgwMCwiaWRlbnRpdHkiOiJhNjRmMjA0NDc3ZDk5ZjhhYmQ4NzM4ZmQ5ZTlkMGYwNTIyY2JkZjQ0NDI2NmNjYzNiZTJiY2E0YjFmMzVkMWU4IiwicmVjdl9tcyI6MTc5MDg1NjAwMjg1MCwic2VxIjozLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwMzgwMCwiaWRlbnRpdHkiOiIwMTM3ZDNmYWNkYTBiOGE1MWMwYjkwMjhhNmFiODU3YjlkMDJlMGZkNjgwNDgzOTRjYmQ5YzIwNTVhOWM5Y2IwIiwicmVjdl9tcyI6MTc5MDg1NjAwMzg1MCwic2VxIjo0LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwNDgwMCwiaWRlbnRpdHkiOiIzMWMzY2I5ZjliODEwMzU4NzI4MTgwOGQ2ZjNlYmIxMzBjYmY1NzQyMDc2Y2YxYTM2MGY1MjA4ODVkMWQ0YjVlIiwicmVjdl9tcyI6MTc5MDg1NjAwNDg1MCwic2VxIjo1LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwNTgwMCwiaWRlbnRpdHkiOiI0NmVhMTdmMzc1ZDIyZDU3Njc1OTg5NDY4YzBjZDk2NDgyNThjOTM4Yjc4Y2MwN2Q5ZWFhMDcyMTVkZmMxMTJhIiwicmVjdl9tcyI6MTc5MDg1NjAwNTg1MCwic2VxIjo2LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwNjgwMCwiaWRlbnRpdHkiOiI4NWFiMmUxYzNmYzRhYmJkOTEzYjgzMWQ5OTMyZmUwNmJhNzRiMzE5M2IzNTY0M2YyNzIzYzYwYzQzZDZjNjgwIiwicmVjdl9tcyI6MTc5MDg1NjAwNjg1MCwic2VxIjo3LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwNzgwMCwiaWRlbnRpdHkiOiJiNzFkMjdmN2E1OTNiNDZlZWRjZjEwMzJjMzdiZGEwMmQ0NTA2NGE3YWEzOThjNWIxNTQzODc0ZWY0NWNhNjhiIiwicmVjdl9tcyI6MTc5MDg1NjAwNzg1MCwic2VxIjo4LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwODgwMCwiaWRlbnRpdHkiOiIxYzY0MWE5YzQ4ZjJiYWMyMWE5ZDNmZmNmMGUwMjA1OGY1ZjgyYmI3Nzc5MmZhNGU4OGEyNTFlNTczZDUyNTk4IiwicmVjdl9tcyI6MTc5MDg1NjAwODg1MCwic2VxIjo5LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAwOTgwMCwiaWRlbnRpdHkiOiI4MDBiMTk4MzAxYzgyM2QxNjA0M2QyMjIzNTkwN2Q5N2FmY2FmZWQ4OGZlOGZiYTE1Njk3MzMwMGMzODcwOGVjIiwicmVjdl9tcyI6MTc5MDg1NjAwOTg1MCwic2VxIjoxMCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMTA4MDAsImlkZW50aXR5IjoiZjRmMDYwN2FhZmYzYmZmZDZhZTMyM2IyMzBhOGRlNjM2NTA4MjI4ZDMzYmFjODU0NzVjZjUzM2ViZGI4ODUyMSIsInJlY3ZfbXMiOjE3OTA4NTYwMTA4NTAsInNlcSI6MTEsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDExODAwLCJpZGVudGl0eSI6ImVkYmRlYjIxYTYwNzEwNTI2NDZkNmEwZWU4MmQyYzFhOTg2Zjg1NzQ1Mzk4YTU0NDQwYjUxZGMyM2JkYTRlNGIiLCJyZWN2X21zIjoxNzkwODU2MDExODUwLCJzZXEiOjEyLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAxMjgwMCwiaWRlbnRpdHkiOiIwOWZmNDE0NjAxYmE4MmJhZjUwZTJlNWM5YWE2MTMwMWJmZDRhOGU0MDEyYjk5NzcxNTIwZTE4OWRmMTViZTQ4IiwicmVjdl9tcyI6MTc5MDg1NjAxMjg1MCwic2VxIjoxMywic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMTM4MDAsImlkZW50aXR5IjoiYThlODkxZTFiZmMxNzA5MDM1YWYyMTk4OThjYWY4MTMwOTI0NDIxYWNhYTJiNTI0OWJkYzhmZTQ2ZGUxNzVhYiIsInJlY3ZfbXMiOjE3OTA4NTYwMTM4NTAsInNlcSI6MTQsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDE0ODAwLCJpZGVudGl0eSI6IjhiZWE3NzJmY2Y0OWEwZjcxNzI1MzVkZjgyMGVlMWYwZWRkN2UyMjdlN2U3MThlM2NiOGQ3YTAxZmIzZmI2MGMiLCJyZWN2X21zIjoxNzkwODU2MDE0ODUwLCJzZXEiOjE1LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAxNTgwMCwiaWRlbnRpdHkiOiI3NjM1N2NjZmYzMWFiYjRmNTM4Y2Y3MWE0YzY2YjkxNzAwNTZlZTAwMDNjYWZmMDBhNzI3YjUwNDAyZjI1MWMyIiwicmVjdl9tcyI6MTc5MDg1NjAxNTg1MCwic2VxIjoxNiwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMTY4MDAsImlkZW50aXR5IjoiYTI1MzgyYmUwYWRmMjk0NWZlYjU3NThjM2FmM2M1N2ZhZDdhNzBjYTA0OGEyYmU2Y2Q5MDBhYjQwNmI0MWMxOSIsInJlY3ZfbXMiOjE3OTA4NTYwMTY4NTAsInNlcSI6MTcsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDE3ODAwLCJpZGVudGl0eSI6IjcxNjFhOGM0NTZmOWVkZWJjMzA3ZmU3OWJiYWFmNzY5ZTM1YjM0NTJkYjViNzUxZDNiYjAwNDgzODhmOGU3ZmYiLCJyZWN2X21zIjoxNzkwODU2MDE3ODUwLCJzZXEiOjE4LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAxODgwMCwiaWRlbnRpdHkiOiJlNzJmZjc4ZTEyYWQyNzFkMjhjMGU4MzEzMjMzOTAyN2UyYjM4NzQ5YjQ0ZWJiNWVmNmI0NTM5NmRmNWZiNDU4IiwicmVjdl9tcyI6MTc5MDg1NjAxODg1MCwic2VxIjoxOSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMTk4MDAsImlkZW50aXR5IjoiYWM1NDc5M2RhNTE3Yzg0ZDA0NDNjMjU0ZWFlYWZiZWIxZGIyYmE2NTVhNjliMTYwZGI1ZTUxZDBmMTRlNDZiOCIsInJlY3ZfbXMiOjE3OTA4NTYwMTk4NTAsInNlcSI6MjAsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDIwODAwLCJpZGVudGl0eSI6IjA3OGMwYjJhYmJlM2YzYTEwNzlhOWVjMzkxNWFkNDM3ZTZlNTMzYmMyMTdjNjliMTRhYjgwZTRjN2Q3ZWFmZDciLCJyZWN2X21zIjoxNzkwODU2MDIwODUwLCJzZXEiOjIxLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAyMTgwMCwiaWRlbnRpdHkiOiJjZDVhMjg2OTY5YzVmZDY5YmYwMThmYTllNDdhZjczYzMyMzc4YTQ2MjhjNzQzNGU1ZmY5MWViOTE3ZDAyNmZlIiwicmVjdl9tcyI6MTc5MDg1NjAyMTg1MCwic2VxIjoyMiwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMjI4MDAsImlkZW50aXR5IjoiOGNhMzA5YjkwODc5Zjk3YTc0NmY2NWJmY2QwMWJlNWEwNDAzNjIyODllZTZhNGY1YzAzYTM4YWI3Zjk5OTg5MCIsInJlY3ZfbXMiOjE3OTA4NTYwMjI4NTAsInNlcSI6MjMsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDIzODAwLCJpZGVudGl0eSI6IjE5M2Y1M2U3MWNhZWJiMjM1MjBmOTk1YWRmNzQzMjliMWUzYWQ4Yzk2NWU3NTQyM2Q3NWYxMWEyY2M0YzFhYzgiLCJyZWN2X21zIjoxNzkwODU2MDIzODUwLCJzZXEiOjI0LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAyNDgwMCwiaWRlbnRpdHkiOiI0OWRiOGZlNTY3MDc2NmJhZTc0NWY0NzgwZjYzZDE0NGEyMGVkNzVkY2ZmNDk5Mzk0ZGQ0MWM3NGViZTM1ZjcyIiwicmVjdl9tcyI6MTc5MDg1NjAyNDg1MCwic2VxIjoyNSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMjU4MDAsImlkZW50aXR5IjoiYTcyMmM0MTdlNmNmN2IxMjY4Yzk0Mjk2OTZmNTk0Nzc4N2RkNzUyNmY5MzgzNGQ5NDgxOGIyYjk5NjdkM2YzNSIsInJlY3ZfbXMiOjE3OTA4NTYwMjU4NTAsInNlcSI6MjYsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDI2ODAwLCJpZGVudGl0eSI6IjRjNDhlM2Y2YWE5MzRkZTBlMGRlMWRjNjFjY2E1NzllY2ZjYzQwMTBiMmY0N2I0MGI3NTYzYzMzNTg5MDY5MzAiLCJyZWN2X21zIjoxNzkwODU2MDI2ODUwLCJzZXEiOjI3LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAyNzgwMCwiaWRlbnRpdHkiOiI4MjcwZDNjOGI1YWI5ZDhjODZlNzU0M2RlMjQ1MmEwZDljZGE1MDNkNjlmZTQzYjZjNGQxNjM3M2Y1MjBlZTkxIiwicmVjdl9tcyI6MTc5MDg1NjAyNzg1MCwic2VxIjoyOCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMjg4MDAsImlkZW50aXR5IjoiMmFmMDRmYjQ0OWQ5NTRjYTY1ZmE3MDBkYjIwMjAwMWQzN2E0MTFhOGU2OTI0ZWEwYzU1NTJmMzhiOThkNDNlMyIsInJlY3ZfbXMiOjE3OTA4NTYwMjg4NTAsInNlcSI6MjksInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDI5ODAwLCJpZGVudGl0eSI6ImRjZjZkODJmY2UyYWVjYWYyMjYzMmIyNWIxOGIwYTVjYzljYmI5Y2E3MmZmNmM2M2MwNWU4ZWUxMjUzZTljYWQiLCJyZWN2X21zIjoxNzkwODU2MDI5ODUwLCJzZXEiOjMwLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAzMDgwMCwiaWRlbnRpdHkiOiJlNjdjZTYxZjljOTZjNDA1M2I0YWMzNTkyNjI0MTIzNTdkYjZkZDE2YzgwNTljZWMyNWFhZTJiNDM2NDhhNWRlIiwicmVjdl9tcyI6MTc5MDg1NjAzMDg1MCwic2VxIjozMSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMzE4MDAsImlkZW50aXR5IjoiNzVmMzAwMjRjNjdiYjI5M2UwZjU3NjZmZjgzN2E1MGQ5ODNmNGQxNzI0MjMxNzMwMWFmYTE3M2RiYmI2MmViZSIsInJlY3ZfbXMiOjE3OTA4NTYwMzE4NTAsInNlcSI6MzIsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDMyODAwLCJpZGVudGl0eSI6ImY3N2VhY2ExZDM5OTY1ZmNiMWQxNzc3MzQ5ZWJlMDZkYjQ1MGQxYjVmOWRhMThiNjgzNmNkMmNmN2E1M2I5ZTciLCJyZWN2X21zIjoxNzkwODU2MDMyODUwLCJzZXEiOjMzLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAzMzgwMCwiaWRlbnRpdHkiOiIwMWE0NzU2YjIxMGNhNGYwYjEwMmViYzlkZDAzNWFmZDkyNDljOGFiYmE4OWZhN2NkY2NiMzMwZDljZGVmMTk5IiwicmVjdl9tcyI6MTc5MDg1NjAzMzg1MCwic2VxIjozNCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMzQ4MDAsImlkZW50aXR5IjoiZDZmODRmYzczMTMzZDUyZTg0MTcwM2ZkMjk3ODE3YTFkNGM1NzI4NmRiZmMzZTNlMGU4YmExNTBiOTQzMDYxMiIsInJlY3ZfbXMiOjE3OTA4NTYwMzQ4NTAsInNlcSI6MzUsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDM1ODAwLCJpZGVudGl0eSI6IjhkOWM2YmVjZWY2ZWEyMzIxOWM0NTUwY2ZjZmQ3MGE4ZTc3N2ZlMmRkMGE2ZWM4Y2ZlYzJjOTljNjJmY2MwMzMiLCJyZWN2X21zIjoxNzkwODU2MDM1ODUwLCJzZXEiOjM2LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAzNjgwMCwiaWRlbnRpdHkiOiI4MjU0NjMwZTRiODJkMGQ1NjM4YTUwNzNmYmE1YzZiZDE5MzY4MGRhOWUxYjA3MDU0Y2RjZDc5OWY0ZDJmNDMzIiwicmVjdl9tcyI6MTc5MDg1NjAzNjg1MCwic2VxIjozNywic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwMzc4MDAsImlkZW50aXR5IjoiZDFhZmUxOTY5YzI0NGVjYTM2ZTAwM2YxZThhNDgxOGM1MDc3Njg4MzY5YTUyM2UyNTA2ZTAxZjYxOWYzYmU1ZiIsInJlY3ZfbXMiOjE3OTA4NTYwMzc4NTAsInNlcSI6MzgsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDM4ODAwLCJpZGVudGl0eSI6ImQwZGM0ZTA5MTk4YjIzNzVlYmU5YWM2ZmQ2MmExMTI1ZjYwOTA3MmRiZmNkMDRiMzg0MjBhMDQ5N2QzNjU0Y2QiLCJyZWN2X21zIjoxNzkwODU2MDM4ODUwLCJzZXEiOjM5LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjAzOTgwMCwiaWRlbnRpdHkiOiJlZmRkYmVhYWViYmE2YTIyMmJlMDIxODMxMDFjMWIwZjhhZGQ4MDYxZmU4MzIxYjY2YTA5ZjA1YzYxY2I4NzRiIiwicmVjdl9tcyI6MTc5MDg1NjAzOTg1MCwic2VxIjo0MCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwNDA4MDAsImlkZW50aXR5IjoiZDVjNGExYzFiNDUyODIxMmEzNWIyNDMwMmE3NTA3ODU0ODk2YzgwMWQ1MTVkMWUwNzc2NWVjNjA2OThhNTFhYyIsInJlY3ZfbXMiOjE3OTA4NTYwNDA4NTAsInNlcSI6NDEsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDQxODAwLCJpZGVudGl0eSI6ImY2Y2U5NDI5NTk0NjhkMWRkYjAxODk5YTFjY2YzZTFhZWViMmMxNWI2Y2Y3NzdjNTJhM2IyMmY4MzU1ZDg3ZDciLCJyZWN2X21zIjoxNzkwODU2MDQxODUwLCJzZXEiOjQyLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjA0MjgwMCwiaWRlbnRpdHkiOiI5YzI5ZDUwZDE1Y2ViZjUxNzQ1YWEzNDEzNjJmZGQyYTI5NjI2MGI0YmY3ZTY0M2FhMTliZTU1YWMyMmNjM2I4IiwicmVjdl9tcyI6MTc5MDg1NjA0Mjg1MCwic2VxIjo0Mywic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwNDM4MDAsImlkZW50aXR5IjoiOWQxYzU4ZWFlYTA5NDcxNmRiYzJmNDc0NDc1MGVkMmMxMGEzMjg3YmY3N2Q4ZWM5OTE3MGE5MDEyNmE3NDYwYSIsInJlY3ZfbXMiOjE3OTA4NTYwNDM4NTAsInNlcSI6NDQsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDQ0ODAwLCJpZGVudGl0eSI6IjAzZGE5YWQ2ZDMyNmJmZjc0MTFkNThiNmQzOGFhZDJmODU3NGVkMjI4N2E2NzY1YTViYWU4ZWMzZTVjOGQyOTkiLCJyZWN2X21zIjoxNzkwODU2MDQ0ODUwLCJzZXEiOjQ1LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjA0NTgwMCwiaWRlbnRpdHkiOiIwYzA3YzhlMjQ5MjZkODY4ZWI4MzYyNDljZGU5YjFmODE0NDQ4NmE5MTg5NDlhYzkyMjAzZTM5Mzc3MGJiNDZhIiwicmVjdl9tcyI6MTc5MDg1NjA0NTg1MCwic2VxIjo0Niwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwNDY4MDAsImlkZW50aXR5IjoiZTY3YjVhZDc1YjcwODJhYTc2ZmRkNDc2MDYyZjM3YzEyNTcxYjYzMDE1MGViZDUwY2IxZjBlYmU5NTYyYzg5NCIsInJlY3ZfbXMiOjE3OTA4NTYwNDY4NTAsInNlcSI6NDcsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDQ3ODAwLCJpZGVudGl0eSI6IjE3ZDY4MzY4MDdlNzc2NDY0ZmU2MmZjMWE5ZGU3MjZjM2ExYTFiMDFiNjdiMjU3ZTg0NTkyMmM0NzEwNDExNzUiLCJyZWN2X21zIjoxNzkwODU2MDQ3ODUwLCJzZXEiOjQ4LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjA0ODgwMCwiaWRlbnRpdHkiOiI4NjY5MzM5MjFmYmIyOGFkOGEzNzM3NDQ0MTFiODZiNWU3MWRhODg2YTZjODIxYWRiNTU0MzllMGZhZGY5Mjk2IiwicmVjdl9tcyI6MTc5MDg1NjA0ODg1MCwic2VxIjo0OSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwNDk4MDAsImlkZW50aXR5IjoiYTRhZjc2YjE4YmYxY2RkOGIzMjQ5NmIyYjE5NmVjZTdiNWY3MmMwOTNiZDEyNzRhNjc2ZGFjZGIzZjU5YmM1ZCIsInJlY3ZfbXMiOjE3OTA4NTYwNDk4NTAsInNlcSI6NTAsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDUwODAwLCJpZGVudGl0eSI6IjAwZDE4MmNjMzRjNzExYjYzNWRjYjhhZWViNzQ5OGI0MmVlM2QzYTEzMWNjYzEyN2JhOTcwMjQ0ZWJlNDk2ZWQiLCJyZWN2X21zIjoxNzkwODU2MDUwODUwLCJzZXEiOjUxLCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjA1MTgwMCwiaWRlbnRpdHkiOiJhNjc2MzRmNDRmZTU4OTdkM2UxNjBlYmE5ZDFkNTU0YWUxNzRiMjQyMmZkZTAxNWM5NzMyY2UyNTkyOWVkZTYyIiwicmVjdl9tcyI6MTc5MDg1NjA1MTg1MCwic2VxIjo1Miwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwNTI4MDAsImlkZW50aXR5IjoiNGVkMGExM2NlNWJmOGE2MGIyMjZmOTkxYzk2N2U0NDZiNTA2MzVkNTM4YWQzNTBkMGM3Y2MyYjdmZGFjYWU4NSIsInJlY3ZfbXMiOjE3OTA4NTYwNTI4NTAsInNlcSI6NTMsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDUzODAwLCJpZGVudGl0eSI6IjFlZGYwYmY2YmViYWU0YjVjMDU4MmQzN2NmMmQ2MjE5MzllZGRlMjNkOWU5ZDYzNzI2MmU0YzEzMzVhYWZlMjQiLCJyZWN2X21zIjoxNzkwODU2MDUzODUwLCJzZXEiOjU0LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjA1NDgwMCwiaWRlbnRpdHkiOiI1NzFiOWUzYTBhODYwZWQxYjViMDcxZjNhMGVkYTc5MjdiZmFjMjgyYzg3YTI5MzMwNjNhZDllYzFlYzhlMzU5IiwicmVjdl9tcyI6MTc5MDg1NjA1NDg1MCwic2VxIjo1NSwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwNTU4MDAsImlkZW50aXR5IjoiMzQyYzNhMjdkYTJmN2IxMzEzOTVhNDliYjQ0ZWY5YWZhMDQ0NmZhMTE0MDRmOWZlYjY0Njk3NjgxNDU1ZTdlOSIsInJlY3ZfbXMiOjE3OTA4NTYwNTU4NTAsInNlcSI6NTYsInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDU2ODAwLCJpZGVudGl0eSI6IjhkYTMyMTFjMTNhZWExMTQ1ZGQ4NTExNTU5NWJjNWExYTU4YWVkY2Q3MDc3ZjZiN2JmOWIxNWMzOWExNWVjMGQiLCJyZWN2X21zIjoxNzkwODU2MDU2ODUwLCJzZXEiOjU3LCJzZXNzaW9uIjoxfSx7ImFza19kZXB0aCI6NSwiYXNrcyI6W1siNjAwMDEiLCIwLjUiLDFdLFsiNjAwMDIiLCIwLjYiLDFdLFsiNjAwMDMiLCIwLjciLDFdLFsiNjAwMDQiLCIwLjgiLDFdLFsiNjAwMDUiLCIwLjkiLDFdXSwiYmlkX2RlcHRoIjo1LCJiaWRzIjpbWyI1OTk5OSIsIjAuNSIsMV0sWyI1OTk5OCIsIjAuNiIsMV0sWyI1OTk5NyIsIjAuNyIsMV0sWyI1OTk5NiIsIjAuOCIsMV0sWyI1OTk5NSIsIjAuOSIsMV1dLCJldmVudF9tcyI6MTc5MDg1NjA1NzgwMCwiaWRlbnRpdHkiOiIzMmQ1ZmU5MzFkZGRkMmJkNmYzMmViMmE5Njc3YWZjZmI2NzY3MWEyZTNiOTkyNDUwMTllZDZmMTdkNDZiMTRhIiwicmVjdl9tcyI6MTc5MDg1NjA1Nzg1MCwic2VxIjo1OCwic2Vzc2lvbiI6MX0seyJhc2tfZGVwdGgiOjUsImFza3MiOltbIjYwMDAxIiwiMC41IiwxXSxbIjYwMDAyIiwiMC42IiwxXSxbIjYwMDAzIiwiMC43IiwxXSxbIjYwMDA0IiwiMC44IiwxXSxbIjYwMDA1IiwiMC45IiwxXV0sImJpZF9kZXB0aCI6NSwiYmlkcyI6W1siNTk5OTkiLCIwLjUiLDFdLFsiNTk5OTgiLCIwLjYiLDFdLFsiNTk5OTciLCIwLjciLDFdLFsiNTk5OTYiLCIwLjgiLDFdLFsiNTk5OTUiLCIwLjkiLDFdXSwiZXZlbnRfbXMiOjE3OTA4NTYwNTg4MDAsImlkZW50aXR5IjoiMDk3Zjc4MDM4ZmU5Y2MyNmJhMGY0MDg2ZGFmNmRmYjNlZGY2ZGY4NTJhZmI0NDRiYTg0NjgwNjA1NDcxMGQwMSIsInJlY3ZfbXMiOjE3OTA4NTYwNTg4NTAsInNlcSI6NTksInNlc3Npb24iOjF9LHsiYXNrX2RlcHRoIjo1LCJhc2tzIjpbWyI2MDAwMSIsIjAuNSIsMV0sWyI2MDAwMiIsIjAuNiIsMV0sWyI2MDAwMyIsIjAuNyIsMV0sWyI2MDAwNCIsIjAuOCIsMV0sWyI2MDAwNSIsIjAuOSIsMV1dLCJiaWRfZGVwdGgiOjUsImJpZHMiOltbIjU5OTk5IiwiMC41IiwxXSxbIjU5OTk4IiwiMC42IiwxXSxbIjU5OTk3IiwiMC43IiwxXSxbIjU5OTk2IiwiMC44IiwxXSxbIjU5OTk1IiwiMC45IiwxXV0sImV2ZW50X21zIjoxNzkwODU2MDU5ODAwLCJpZGVudGl0eSI6IjY0YmZiY2E2YmVlYTViOTYwZjY1YjI3ZTRlOWQ3MDEyOTQxOGMxYjE1ZmZjZTI5ODcwZTgwMTc4YmU1NGQzODgiLCJyZWN2X21zIjoxNzkwODU2MDU5ODUwLCJzZXEiOjYwLCJzZXNzaW9uIjoxfV0sIm93bmVyX2luc3RhbmNlIjoiZml4dHVyZS1mZWVkIiwicHJlZml4X2NvbXBsZXRlIjp0cnVlLCJyZXRhaW5lZF9mcm9tX21zIjoxNzkwODU1OTM5OTUwLCJzbmFwc2hvdF9pZCI6InNuYXAtaW4tMSIsInNvdXJjZSI6Imh5cGVybGlxdWlkX3dzX2wyQm9vayIsInRpbWVfc2VtYW50aWNzIjoic291cmNlX2V2ZW50X21zK2ZpcnN0X3JlY2VpcHRfbXMvMTYzNS12MSIsIndpbmRvd19tcyI6NjAwMDB9","payload_hash":"71b233541cad52ace572e0a8efb54f87a340ad9085691ca644a4532c4dd2965e","price_currency":"USDC","provenance":"hyperliquid_ws_l2Book@fixture-feed","snapshot_id":"snap-in-1","source":"sealed_feed","source_cutoff_ms":1790856059950,"v":2,"venue_time_ms":1790856059800}},{"account":{"collateral_currency":"USDT","equity":"6000","evidence_hash":"588cbd52217be98c0bbafc559761f7e0bdea6a8adb06d633488faf400bc9c2d5","evidence_id":"acct-pair-okx-btc","first_receipt_ms":1700000000500,"free_collateral":"6000","key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"leverage":"2","margin_mode":"cross","margin_used":"0","observed_ms":1700000000000,"position_mode":"net","provenance":"research-schedule:fixture","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000},"capability":{"evidence_hash":"47c32c38c23f0db4e2c4f37958ada179ace9977f725761d31258873f74dc2fe2","evidence_id":"cap-pair-okx-btc","first_receipt_ms":1700000000500,"key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"meta":{"base_asset":"BTC","contract_size":"0.01","fee_currencies":["USDT"],"linear":true,"lot_step":"1","lot_step_text":"1","max_leverage":"20","min_notional":"0","min_qty":"1","native_unit":"contract","price_currency":"USDT","price_rule":"fixed_tick_v1","quote_currency":"USDT","settlement_currency":"USDT","sz_decimals":0,"tick_step":"0.1","tick_step_text":"0.1","trading_status":"live"},"metadata_generation":1,"observed_ms":1700000000000,"provenance":"okx_public_instrument_v1","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000},"conversions":[{"currency":"USDT","evidence_hash":"3a34e57783979f039535e4d494bd21dcc0b8e065482ae7168bafeb933e4b1f5a","evidence_id":"conv-pair-okx-btc-USDT","first_receipt_ms":1700000000500,"key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"observed_ms":1700000000000,"provenance":"research-schedule:fixture","rate_usd":"1","v":2,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000}],"key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"observation":{"asks":[{"base_qty":"0.03","px":"60100.1"},{"base_qty":"0.04","px":"60100.2"},{"base_qty":"0.05","px":"60100.3"},{"base_qty":"0.06","px":"60100.4"},{"base_qty":"0.07","px":"60100.5"}],"bids":[{"base_qty":"0.03","px":"60099.9"},{"base_qty":"0.04","px":"60099.8"},{"base_qty":"0.05","px":"60099.7"},{"base_qty":"0.06","px":"60099.6"},{"base_qty":"0.07","px":"60099.5"}],"contract_size":"0.01","coverage":{"from_ms":1790856059800,"gaps":[],"identity_semantics":"sha256(canonical_payload)/1871-v1","prefix_complete":true,"required_capacity":1,"required_depth":5,"required_window_ms":0,"source_contract":"okx_public_books_snapshot_v1","status":"complete","through_ms":1790856059800,"time_semantics":"okx_books_ts_ms+reader_receipt_ms/1871-v1"},"first_receipt_ms":1790856059800,"generation":0,"key":{"account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc","partition":"paper","provider":"okx"},"modeled_delivery_ms":0,"native_asks":[{"px":"60100.1","qty":"3"},{"px":"60100.2","qty":"4"},{"px":"60100.3","qty":"5"},{"px":"60100.4","qty":"6"},{"px":"60100.5","qty":"7"}],"native_bids":[{"px":"60099.9","qty":"3"},{"px":"60099.8","qty":"4"},{"px":"60099.7","qty":"5"},{"px":"60099.6","qty":"6"},{"px":"60099.5","qty":"7"}],"native_unit":"contract","normalization_id":"contract*0.01@cap-pair-okx-btc","payload":"eyJyZXF1ZXN0Ijp7Imluc3RJZCI6IkJUQy1VU0RULVNXQVAiLCJzeiI6IjUifSwicmVzcG9uc2UiOnsiY29kZSI6IjAiLCJkYXRhIjpbeyJhc2tzIjpbWyI2MDEwMC4xIiwiMyIsIjAiLCIyIl0sWyI2MDEwMC4yIiwiNCIsIjAiLCIyIl0sWyI2MDEwMC4zIiwiNSIsIjAiLCIyIl0sWyI2MDEwMC40IiwiNiIsIjAiLCIyIl0sWyI2MDEwMC41IiwiNyIsIjAiLCIyIl1dLCJiaWRzIjpbWyI2MDA5OS45IiwiMyIsIjAiLCIyIl0sWyI2MDA5OS44IiwiNCIsIjAiLCIyIl0sWyI2MDA5OS43IiwiNSIsIjAiLCIyIl0sWyI2MDA5OS42IiwiNiIsIjAiLCIyIl0sWyI2MDA5OS41IiwiNyIsIjAiLCIyIl1dLCJ0cyI6IjE3OTA4NTYwNTk3ODAifV0sIm1zZyI6IiJ9fQ==","payload_hash":"aa44c498e3cd35c9da2becb07152dff5e2efd6d87f29d6fb044e53f7d4ca4da8","price_currency":"USDT","provenance":"okx_public_books_snapshot_v1@https://www.okx.com","snapshot_id":"","source":"public_snapshot","source_cutoff_ms":1790856059800,"v":2,"venue_time_ms":1790856059780}}],"pair_id":"btc-xvenue","partition":"paper","slot_ms":1790856060000,"snapshot_id":"snap-in-1","start_ms":1790856060000,"v":2}','','','{"result":"evidence_only","completed_at_ms":1790856061000}',0,1790856060000,1790856061000);
CREATE TABLE basket_member_state (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    basket_id TEXT NOT NULL,
    strategy_id TEXT NOT NULL,
    expected_qty REAL NOT NULL DEFAULT 0,
    updated_at_ms INTEGER NOT NULL DEFAULT 0, position_id TEXT NOT NULL DEFAULT '', entry_ms INTEGER NOT NULL DEFAULT 0, expiry_ms INTEGER NOT NULL DEFAULT 0, expiry_state TEXT NOT NULL DEFAULT '', expiry_attempt INTEGER NOT NULL DEFAULT 0, expiry_failures INTEGER NOT NULL DEFAULT 0, expiry_retry_at_ms INTEGER NOT NULL DEFAULT 0, expiry_alerts_json TEXT NOT NULL DEFAULT '', suppress_through_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, source, basket_id, strategy_id)
);
INSERT INTO basket_member_state VALUES('paper','','xs-test','hl-bsk-avax',-33.25,1793577660000,'',0,0,'',0,0,0,'',0);
INSERT INTO basket_member_state VALUES('paper','','xs-test','hl-bsk-btc',0.01660999999999999977,1793577660000,'',0,0,'',0,0,0,'',0);
INSERT INTO basket_member_state VALUES('paper','','xs-test','hl-bsk-eth',0.3322999999999999843,1793577660000,'',0,0,'',0,0,0,'',0);
INSERT INTO basket_member_state VALUES('paper','','xs-test','hl-bsk-sol',-6.639999999999999681,1793577660000,'',0,0,'',0,0,0,'',0);
INSERT INTO basket_member_state VALUES('paper','','btc-xvenue','pair-hl-btc',0.05999999999999999778,1790856061000,'',0,0,'',0,0,0,'',0);
INSERT INTO basket_member_state VALUES('paper','','btc-xvenue','pair-okx-btc',-0.05999999999999999778,1790856061000,'',0,0,'',0,0,0,'',0);
CREATE TABLE IF NOT EXISTS "basket_member_actions" (
    action_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    basket_id TEXT NOT NULL,
    deadline_ms INTEGER NOT NULL,
    strategy_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('reduce','increase')),
    seq INTEGER NOT NULL CHECK (seq >= 1),
    order_side TEXT NOT NULL CHECK (order_side IN ('buy','sell')),
    position_side TEXT NOT NULL CHECK (position_side IN ('long','short')),
    status TEXT NOT NULL CHECK (status IN ('reserved','submitting','unresolved','settling','committed','no_fill','held')),
    requested_qty REAL NOT NULL CHECK (requested_qty > 0),
    pre_qty REAL NOT NULL DEFAULT 0,
    pre_notional_usd REAL NOT NULL DEFAULT 0,
    mark_px REAL NOT NULL DEFAULT 0,
    protection_json TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL DEFAULT '',
    client_order_id TEXT NOT NULL,
    venue_order_id TEXT NOT NULL DEFAULT '',
    entry_intent_id TEXT NOT NULL DEFAULT '',
    decision_bar_open_ms INTEGER NOT NULL DEFAULT 0,
    confirmed_qty REAL NOT NULL DEFAULT 0,
    confirmed_avg_px REAL NOT NULL DEFAULT 0,
    confirmed_fee REAL NOT NULL DEFAULT 0,
    applied_qty REAL NOT NULL DEFAULT 0,
    applied_fee REAL NOT NULL DEFAULT 0,
    evidence_json TEXT NOT NULL DEFAULT '',
    hold_reason TEXT NOT NULL DEFAULT '',
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    purpose TEXT NOT NULL DEFAULT 'rebalance' CHECK (purpose IN ('rebalance','expiry','pair_entry','pair_exit','pair_protective','pair_kill_switch','pair_operator')),
    lifecycle_position_id TEXT NOT NULL DEFAULT '',
    expiry_ms INTEGER NOT NULL DEFAULT 0,
    lifecycle_attempt INTEGER NOT NULL DEFAULT 0,
    group_mode TEXT NOT NULL DEFAULT 'basket' CHECK (group_mode IN ('basket','pair')),
    instrument TEXT NOT NULL DEFAULT '',
    native_qty REAL NOT NULL DEFAULT 0,
    contract_size REAL NOT NULL DEFAULT 0,
    request_json TEXT NOT NULL DEFAULT '',
    quote_event_id TEXT NOT NULL DEFAULT '',
    price_bound REAL NOT NULL DEFAULT 0,
    accept_deadline_ms INTEGER NOT NULL DEFAULT 0,
    submitted_at_ms INTEGER NOT NULL DEFAULT 0,
    confirmed_notional REAL NOT NULL DEFAULT 0,
    fee_present INTEGER NOT NULL DEFAULT 0,
    fee_currency TEXT NOT NULL DEFAULT '',
    fee_amount REAL NOT NULL DEFAULT 0,
    fee_evidence_id TEXT NOT NULL DEFAULT '',
    fee_evidence_version INTEGER NOT NULL DEFAULT 0,
    applied_notional REAL NOT NULL DEFAULT 0,
    applied_fee_version INTEGER NOT NULL DEFAULT 0,
    terminal INTEGER NOT NULL DEFAULT 0,
    coverage_through_ms INTEGER NOT NULL DEFAULT 0,
    retry_count INTEGER NOT NULL DEFAULT 0,
    retry_at_ms INTEGER NOT NULL DEFAULT 0,
    recovery_json TEXT NOT NULL DEFAULT '',
    booked_json TEXT NOT NULL DEFAULT '',
    accepted_at_ms INTEGER NOT NULL DEFAULT 0,
    fill_coverage_json TEXT NOT NULL DEFAULT '',
    fee_coverage_json TEXT NOT NULL DEFAULT '',
    fee_final INTEGER NOT NULL DEFAULT 0,
    settlement_json TEXT NOT NULL DEFAULT '',
    CHECK (group_mode = 'pair' OR purpose IN ('rebalance','expiry')),
    CHECK (group_mode = 'basket' OR purpose IN ('pair_entry','pair_exit','pair_protective','pair_kill_switch','pair_operator')),
    CHECK (group_mode = 'basket' OR (request_json != '' AND instrument != '')),
    UNIQUE (scope, source, basket_id, deadline_ms, strategy_id, direction, seq)
);
INSERT INTO basket_member_actions VALUES('ba-0866ba57d106226bd8ae2736','paper','','xs-test',1793577600000,'hl-bsk-avax','AVAX','increase',1,'sell','short','committed',33.25,0.0,0.0,30.0,'{"regime":"","decision_atr":0.3,"decision_interval_ms":3600000,"decision_boundary_ms":1793577600000,"decision_cutoff_ms":1793577630000}','paper','paper:paper','bp-ff5efa2b52a38c744933b7c8','','',1793574000000,33.25,29.98500000000000299,0.0,33.25,0.4486505625000000718,'{"outcome":"filled","filled_qty":33.25,"observed_at_ms":1793577660000}','',1793577660000,1793577660000,'rebalance','',0,0,'basket','',0.0,0.0,'','',0.0,0,0,0.0,0,'',0.0,'',0,0.0,0,0,0,0,0,'','',0,'','',0,'');
INSERT INTO basket_member_actions VALUES('ba-f5fd9c40f802a737dddd95fa','paper','','xs-test',1793577600000,'hl-bsk-eth','ETH','increase',1,'buy','long','committed',0.3322999999999999843,0.0,0.0,3000.0,'{"regime":"","decision_atr":30,"decision_interval_ms":3600000,"decision_boundary_ms":1793577600000,"decision_cutoff_ms":1793577630000}','paper','paper:paper','bp-9401ec947a69241c2d1ec571','','',1793574000000,0.3322999999999999843,3001.5,0.0,0.3322999999999999843,0.4488293024999999714,'{"outcome":"filled","filled_qty":0.3323,"observed_at_ms":1793577660000}','',1793577660000,1793577660000,'rebalance','',0,0,'basket','',0.0,0.0,'','',0.0,0,0,0.0,0,'',0.0,'',0,0.0,0,0,0,0,0,'','',0,'','',0,'');
INSERT INTO basket_member_actions VALUES('ba-24ec472fdcc6527a9176f87e','paper','','xs-test',1793577600000,'hl-bsk-btc','BTC','increase',1,'buy','long','committed',0.01660999999999999977,0.0,0.0,60000.0,'{"regime":"","decision_atr":600,"decision_interval_ms":3600000,"decision_boundary_ms":1793577600000,"decision_cutoff_ms":1793577630000}','paper','paper:paper','bp-288d8329eb6fda3323d9468a','','',1793574000000,0.01660999999999999977,60030.0,0.0,0.01660999999999999977,0.4486942349999999969,'{"outcome":"filled","filled_qty":0.01661,"observed_at_ms":1793577660000}','',1793577660000,1793577660000,'rebalance','',0,0,'basket','',0.0,0.0,'','',0.0,0,0,0.0,0,'',0.0,'',0,0.0,0,0,0,0,0,'','',0,'','',0,'');
INSERT INTO basket_member_actions VALUES('ba-2358f88319786e4331307c0b','paper','','xs-test',1793577600000,'hl-bsk-sol','SOL','increase',1,'sell','short','committed',6.639999999999999681,0.0,0.0,150.0,'{"regime":"","decision_atr":1.5,"decision_interval_ms":3600000,"decision_boundary_ms":1793577600000,"decision_cutoff_ms":1793577630000}','paper','paper:paper','bp-2ddbcae9618bf749ce211244','','',1793574000000,6.639999999999999681,149.9250000000000113,0.0,6.639999999999999681,0.447975900000000038,'{"outcome":"filled","filled_qty":6.64,"observed_at_ms":1793577660000}','',1793577660000,1793577660000,'rebalance','',0,0,'basket','',0.0,0.0,'','',0.0,0,0,0.0,0,'',0.0,'',0,0.0,0,0,0,0,0,'','',0,'','',0,'');
INSERT INTO basket_member_actions VALUES('pa-d962191dcf2c103600b71db7','paper','','btc-xvenue',1790856000000,'pair-okx-btc','BTC','increase',1,'sell','short','committed',0.05999999999999999778,0.0,0.0,0.0,'','okx','synthetic:okx-research','okx-000001','','ei-6d8924f3b9318eb778232231',0,0.05999999999999999778,59700.0,0.0,0.05999999999999999778,1.611899999999999889,'{"outcome":"filled","filled_qty":0,"observed_at_ms":1790856001000,"client_order_id":"okx-000001","account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","side":"sell","cum_qty":0.06,"cum_notional":3582,"accepted_at_ms":1790856001000,"terminal":true}','',1790856001000,1790856001000,'pair_entry','',0,0,'pair','BTC-USDT-SWAP',6.0,0.0100000000000000002,'{"v":2,"pair_id":"btc-xvenue","partition":"paper","member_id":"pair-okx-btc","provider":"okx","account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","client_order_id":"okx-000001","purpose":"pair_entry","order_side":"sell","position_side":"short","base_qty":0.06,"native_qty":6,"contract_size":0.01,"price_bound":59700,"accept_deadline_ms":1790856031000,"quote_event_id":"pq-4687ae86f0d4184bfe86f295","origin_slot_ms":1790856000000,"reduce_only":false,"contract_fingerprint":"d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90","conversions":[{"v":2,"key":{"partition":"paper","provider":"okx","account_key":"synthetic:okx-research","instrument":"BTC-USDT-SWAP","member_id":"pair-okx-btc"},"evidence_id":"conv-pair-okx-btc-USDT","evidence_hash":"3a34e57783979f039535e4d494bd21dcc0b8e065482ae7168bafeb933e4b1f5a","observed_ms":1700000000000,"first_receipt_ms":1700000000500,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000,"provenance":"research-schedule:fixture","currency":"USDT","rate_usd":"1"}],"capability_hash":"47c32c38c23f0db4e2c4f37958ada179ace9977f725761d31258873f74dc2fe2","evidence_hash":"3076d430ecbaed2d538c5ce870bb7b4d96448ab4ac9de41dd372512ce4303aaf"}','pq-4687ae86f0d4184bfe86f295',59700.0,1790856031000,1790856001000,3582.0,0,'',0.0,'',0,3582.0,0,1,0,0,0,'','[{"trade_ts_ms":1790856001000,"trade_ts":"2026-10-01T12:00:01Z","qty":0.06,"px":59700,"fee_usd":1.6118999999999999,"order_id":"okx-000001"}]',0,'','',0,'');
INSERT INTO basket_member_actions VALUES('pa-f0e7d23e6d7b77a1bd0bb352','paper','','btc-xvenue',1790856000000,'pair-hl-btc','BTC','increase',1,'buy','long','committed',0.05999999999999999778,0.0,0.0,0.0,'','hyperliquid','synthetic:hl-research','hyperliquid-000002','','ei-ddc9ba4a6614ee797ff472ef',0,0.05999999999999999778,60400.0,0.0,0.05999999999999999778,1.630800000000000027,'{"outcome":"filled","filled_qty":0,"observed_at_ms":1790856061000,"client_order_id":"hyperliquid-000002","account_key":"synthetic:hl-research","instrument":"BTC","side":"buy","cum_qty":0.06,"cum_notional":3624,"accepted_at_ms":1790856061000,"terminal":true}','',1790856061000,1790856061000,'pair_entry','',0,0,'pair','BTC',0.05999999999999999778,1.0,'{"v":2,"pair_id":"btc-xvenue","partition":"paper","member_id":"pair-hl-btc","provider":"hyperliquid","account_key":"synthetic:hl-research","instrument":"BTC","client_order_id":"hyperliquid-000002","purpose":"pair_entry","order_side":"buy","position_side":"long","base_qty":0.06,"native_qty":0.06,"contract_size":1,"price_bound":60400,"accept_deadline_ms":1790856091000,"quote_event_id":"pq-d406cbd50865f42d08703003","origin_slot_ms":1790856000000,"reduce_only":false,"contract_fingerprint":"d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90","conversions":[{"v":2,"key":{"partition":"paper","provider":"hyperliquid","account_key":"synthetic:hl-research","instrument":"BTC","member_id":"pair-hl-btc"},"evidence_id":"conv-pair-hl-btc-USDC","evidence_hash":"efc195f8358092c49c638543375ed283518a0ad576925ea604af9fb6af959b26","observed_ms":1700000000000,"first_receipt_ms":1700000000500,"valid_from_ms":1700000000500,"valid_through_ms":4102444800000,"provenance":"research-schedule:fixture","currency":"USDC","rate_usd":"1"}],"capability_hash":"f7695767c1b3b544dc92f182fa4ad8a1ae5749e1072f70e4d1894008a4f64f20","evidence_hash":"82d254e6b4505d81e4fc06e8e57fe63137b925069a77d8b4acc2a2a1fd90ad5b"}','pq-d406cbd50865f42d08703003',60400.0,1790856091000,1790856061000,3624.0,0,'',0.0,'',0,3624.0,0,1,0,0,0,'','[{"trade_ts_ms":1790856061000,"trade_ts":"2026-10-01T12:01:01Z","qty":0.06,"px":60400,"fee_usd":1.6308,"order_id":"hyperliquid-000002"}]',0,'','',0,'');
CREATE TABLE pair_quote_event_uses (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    pair_id TEXT NOT NULL,
    strategy_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    contract_fingerprint TEXT NOT NULL,
    slot_ms INTEGER NOT NULL CHECK (slot_ms > 0),
    cutoff_ms INTEGER NOT NULL CHECK (cutoff_ms > 0),
    input_fingerprint TEXT NOT NULL,
    action_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('reserved','consumed','released')),
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (scope, source, strategy_id, event_id)
);
INSERT INTO pair_quote_event_uses VALUES('paper','','btc-xvenue','pair-okx-btc','pq-4687ae86f0d4184bfe86f295','d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90',1790856000000,1790856000500,'3076d430ecbaed2d538c5ce870bb7b4d96448ab4ac9de41dd372512ce4303aaf','pa-d962191dcf2c103600b71db7','consumed',1790856001000,1790856001000);
INSERT INTO pair_quote_event_uses VALUES('paper','','btc-xvenue','pair-hl-btc','pq-d406cbd50865f42d08703003','d7cce18b811b31a0446c5698d2d083cd9fbbb2a54d2f27b74fdfb526b7183c90',1790856060000,1790856060500,'82d254e6b4505d81e4fc06e8e57fe63137b925069a77d8b4acc2a2a1fd90ad5b','pa-f0e7d23e6d7b77a1bd0bb352','consumed',1790856061000,1790856061000);
CREATE TABLE pair_funding_events (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    pair_id TEXT NOT NULL,
    contract_fingerprint TEXT NOT NULL,
    strategy_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL,
    instrument TEXT NOT NULL,
    event_id TEXT NOT NULL,
    rule_version TEXT NOT NULL,
    event_ms INTEGER NOT NULL CHECK (event_ms > 0),
    status TEXT NOT NULL CHECK (status IN ('incomplete','applied','void','unresolvable')),
    reason TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    source_hash TEXT NOT NULL DEFAULT '',
    evidence_hash TEXT NOT NULL DEFAULT '',
    usd_amount TEXT NOT NULL DEFAULT '',
    event_json TEXT NOT NULL DEFAULT '',
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (scope, source, strategy_id, event_id, rule_version),
    UNIQUE (scope, source, strategy_id, provider, account_key, instrument, rule_version, event_ms),
    CHECK (status != 'applied' OR (source_hash != '' AND evidence_hash != '' AND event_json != '' AND usd_amount != ''))
);
CREATE TABLE pair_funding_inventory (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    pair_id TEXT NOT NULL,
    strategy_id TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq >= 1),
    fill_id TEXT NOT NULL DEFAULT '',
    origin TEXT NOT NULL CHECK (origin IN ('coordinator','native','unexplained')),
    action_id TEXT NOT NULL DEFAULT '',
    position_id TEXT NOT NULL DEFAULT '',
    delta_base_qty TEXT NOT NULL,
    post_base_qty TEXT NOT NULL,
    contract_size TEXT NOT NULL,
    event_ms INTEGER NOT NULL,
    event_latest_ms INTEGER NOT NULL,
    visible_ms INTEGER NOT NULL,
    recorded_at_ms INTEGER NOT NULL,
    PRIMARY KEY (scope, source, pair_id, strategy_id, seq),
    CHECK (event_ms > 0 AND event_ms <= event_latest_ms AND event_latest_ms <= visible_ms)
);
INSERT INTO pair_funding_inventory VALUES('paper','','btc-xvenue','pair-okx-btc',1,'','coordinator','pa-d962191dcf2c103600b71db7','pair-okx-btc:BTC:1790856001000000000:5','-0.06','-0.06','0.01',1790856001000,1790856001000,1790856001000,1790856001000);
INSERT INTO pair_funding_inventory VALUES('paper','','btc-xvenue','pair-hl-btc',1,'','coordinator','pa-f0e7d23e6d7b77a1bd0bb352','pair-hl-btc:BTC:1790856061000000000:6','0.06','0.06','1',1790856061000,1790856061000,1790856061000,1790856061000);
CREATE TABLE pair_funding_chain (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    pair_id TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq >= 1),
    source_id TEXT NOT NULL,
    batch_hash TEXT NOT NULL,
    prev_hash TEXT NOT NULL,
    batch_json TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY (scope, source, pair_id, seq)
);
CREATE TABLE pair_native_resolutions (
    resolution_id  TEXT PRIMARY KEY,
    scope          TEXT NOT NULL,
    source         TEXT NOT NULL DEFAULT '',
    pair_id        TEXT NOT NULL,
    strategy_id    TEXT NOT NULL,
    kind           TEXT NOT NULL,
    obligation_id  TEXT NOT NULL,
    attempt_id     TEXT NOT NULL DEFAULT '',
    mode           TEXT NOT NULL,
    decided_by     TEXT NOT NULL,
    operator       TEXT NOT NULL DEFAULT '',
    reason         TEXT NOT NULL DEFAULT '',
    revision       TEXT NOT NULL DEFAULT '',
    decided_at_ms  INTEGER NOT NULL,
    committed_at_ms INTEGER NOT NULL,
    record_json    TEXT NOT NULL,
    alert_status   TEXT NOT NULL DEFAULT '',
    UNIQUE(scope, source, pair_id, kind, strategy_id, obligation_id)
);
CREATE TABLE pair_virtual_events (
    run_tag TEXT NOT NULL,
    member_key_json TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq >= 1),
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    pair_id TEXT NOT NULL,
    strategy_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL,
    instrument TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('binding','submit','refusal','deadline','match','consume','visible','query','absence')),
    semantic_key TEXT NOT NULL,
    client_order_id TEXT NOT NULL DEFAULT '',
    observation_id TEXT NOT NULL DEFAULT '',
    source_contract TEXT NOT NULL DEFAULT '',
    book_side TEXT NOT NULL DEFAULT '',
    level_index INTEGER NOT NULL DEFAULT -1,
    consumed_native_qty TEXT NOT NULL DEFAULT '0',
    consumed_base_qty TEXT NOT NULL DEFAULT '0',
    native_qty_delta TEXT NOT NULL DEFAULT '0',
    venue_time_ms INTEGER NOT NULL DEFAULT 0,
    receipt_ms INTEGER NOT NULL DEFAULT 0,
    visible_ms INTEGER NOT NULL DEFAULT 0,
    request_hash TEXT NOT NULL DEFAULT '',
    body_json TEXT NOT NULL,
    previous_event_hash TEXT NOT NULL,
    event_hash TEXT NOT NULL,
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY (run_tag, member_key_json, seq),
    UNIQUE (run_tag, member_key_json, semantic_key)
);
CREATE TABLE pair_virtual_allocations (
    run_tag TEXT NOT NULL,
    member_key_json TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq >= 1),
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    pair_id TEXT NOT NULL,
    strategy_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL,
    instrument TEXT NOT NULL,
    client_order_id TEXT NOT NULL UNIQUE,
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY (run_tag, member_key_json, seq)
);
CREATE TABLE quote_contracts (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    strategy_id TEXT NOT NULL,
    coin TEXT NOT NULL,
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL,
    namespace TEXT NOT NULL DEFAULT '',
    policy_fingerprint TEXT NOT NULL,
    config_fingerprint TEXT NOT NULL,
    start_ms INTEGER NOT NULL CHECK (start_ms > 0),
    revision INTEGER NOT NULL CHECK (revision >= 1),
    last_deadline_ms INTEGER NOT NULL DEFAULT 0,
    applied_boundary_ms INTEGER NOT NULL DEFAULT 0,
    last_generation INTEGER NOT NULL DEFAULT 0,
    last_stop_seq INTEGER NOT NULL DEFAULT 0,
    last_close_seq INTEGER NOT NULL DEFAULT 0,
    book_json TEXT NOT NULL,
    operator_hold INTEGER NOT NULL DEFAULT 0,
    operator_hold_reason TEXT NOT NULL DEFAULT '',
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (scope, source, strategy_id)
);
CREATE TABLE quote_generations (
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    strategy_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation >= 1),
    deadline_ms INTEGER NOT NULL CHECK (deadline_ms > 0),
    status TEXT NOT NULL CHECK (status IN ('reserved','active','resolved')),
    desired_json TEXT NOT NULL DEFAULT '',
    reservation_json TEXT NOT NULL DEFAULT '',
    terminal_json TEXT NOT NULL DEFAULT '',
    hold_reason TEXT NOT NULL DEFAULT '',
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (scope, source, strategy_id, generation),
    UNIQUE (scope, source, strategy_id, deadline_ms)
);
CREATE TABLE quote_fills (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    strategy_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL,
    namespace TEXT NOT NULL DEFAULT '',
    coin TEXT NOT NULL,
    order_id TEXT NOT NULL,
    event_time_ms INTEGER NOT NULL CHECK (event_time_ms > 0),
    trade_id TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT 'fill' CHECK (kind IN ('fill','fee_correction')),
    corrects_fill_id INTEGER NOT NULL DEFAULT 0,
    role TEXT NOT NULL DEFAULT '',
    client_order_id TEXT NOT NULL DEFAULT '',
    order_row_id INTEGER NOT NULL DEFAULT 0,
    side TEXT NOT NULL CHECK (side IN ('buy','sell')),
    px TEXT NOT NULL,
    size TEXT NOT NULL,
    fee TEXT NOT NULL,
    fee_asset TEXT NOT NULL,
    start_position TEXT NOT NULL DEFAULT '',
    raw_json TEXT NOT NULL,
    digest TEXT NOT NULL,
    settlement_id INTEGER NOT NULL DEFAULT 0,
    observed_at_ms INTEGER NOT NULL,
    UNIQUE (provider, account_key, namespace, coin, order_id, event_time_ms, trade_id, kind)
);
CREATE TABLE quote_settlements (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    strategy_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('fills','funding','both')),
    deadline_ms INTEGER NOT NULL,
    applied_boundary_ms INTEGER NOT NULL,
    fill_ids_json TEXT NOT NULL DEFAULT '[]',
    funding_json TEXT NOT NULL DEFAULT '',
    summary_json TEXT NOT NULL DEFAULT '',
    committed_at_ms INTEGER NOT NULL,
    UNIQUE (scope, source, strategy_id, revision)
);
CREATE TABLE quote_funding_evidence (
    provider TEXT NOT NULL,
    account_key TEXT NOT NULL,
    namespace TEXT NOT NULL DEFAULT '',
    coin TEXT NOT NULL,
    dedup_id TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('live','paper')),
    source TEXT NOT NULL DEFAULT '',
    strategy_id TEXT NOT NULL,
    event_ms INTEGER NOT NULL,
    raw_json TEXT NOT NULL,
    digest TEXT NOT NULL,
    settlement_id INTEGER NOT NULL DEFAULT 0,
    observed_at_ms INTEGER NOT NULL,
    PRIMARY KEY (provider, account_key, namespace, coin, dedup_id)
);
INSERT INTO sqlite_sequence VALUES('trades',6);
CREATE TRIGGER trg_entry_intents_owner_immutable
    BEFORE UPDATE ON entry_order_intents
    WHEN NEW.owner_mode != OLD.owner_mode OR NEW.owner_group_id != OLD.owner_group_id
      OR NEW.decision_kind != OLD.decision_kind OR NEW.quote_event_id != OLD.quote_event_id
      OR NEW.client_order_id != OLD.client_order_id OR NEW.strategy_id != OLD.strategy_id
    BEGIN SELECT RAISE(ABORT, 'entry intent owner and decision identity are immutable'); END;
CREATE TRIGGER trg_basket_actions_identity_immutable
    BEFORE UPDATE ON basket_member_actions
    WHEN NEW.group_mode != OLD.group_mode OR NEW.purpose != OLD.purpose OR NEW.client_order_id != OLD.client_order_id
      OR NEW.provider != OLD.provider OR NEW.account_key != OLD.account_key OR NEW.instrument != OLD.instrument
      OR NEW.strategy_id != OLD.strategy_id OR NEW.basket_id != OLD.basket_id
      OR (OLD.group_mode = 'pair' AND (NEW.request_json != OLD.request_json OR NEW.quote_event_id != OLD.quote_event_id
          OR NEW.price_bound != OLD.price_bound OR NEW.accept_deadline_ms != OLD.accept_deadline_ms OR NEW.requested_qty != OLD.requested_qty))
    BEGIN SELECT RAISE(ABORT, 'basket action identity, mode and request are immutable'); END;
CREATE TRIGGER trg_pair_funding_events_applied_immutable
    BEFORE UPDATE ON pair_funding_events
    WHEN OLD.status IN ('applied','unresolvable')
      OR NEW.pair_id != OLD.pair_id OR NEW.contract_fingerprint != OLD.contract_fingerprint OR NEW.provider != OLD.provider
      OR NEW.account_key != OLD.account_key OR NEW.instrument != OLD.instrument OR NEW.event_ms != OLD.event_ms
      OR (OLD.source_hash != '' AND NEW.source_hash != OLD.source_hash)
    BEGIN SELECT RAISE(ABORT, 'an applied or unresolvable pair funding event and every event identity are immutable'); END;
CREATE TRIGGER trg_pair_funding_events_applied_kept
    BEFORE DELETE ON pair_funding_events
    WHEN OLD.status IN ('applied','unresolvable')
    BEGIN SELECT RAISE(ABORT, 'an applied or unresolvable pair funding event is never deleted'); END;
CREATE TRIGGER trg_pair_funding_inventory_immutable
    BEFORE UPDATE ON pair_funding_inventory
    BEGIN SELECT RAISE(ABORT, 'pair funding inventory history is append-only'); END;
CREATE TRIGGER trg_pair_funding_inventory_kept
    BEFORE DELETE ON pair_funding_inventory
    BEGIN SELECT RAISE(ABORT, 'pair funding inventory history is append-only'); END;
CREATE TRIGGER trg_pair_funding_chain_immutable
    BEFORE UPDATE ON pair_funding_chain
    BEGIN SELECT RAISE(ABORT, 'a recorded pair funding batch is immutable'); END;
CREATE TRIGGER trg_pair_funding_chain_kept
    BEFORE DELETE ON pair_funding_chain
    BEGIN SELECT RAISE(ABORT, 'a recorded pair funding batch is immutable'); END;
CREATE TRIGGER trg_pair_virtual_events_immutable
    BEFORE UPDATE ON pair_virtual_events
    BEGIN SELECT RAISE(ABORT, 'pair virtual execution history is append-only'); END;
CREATE TRIGGER trg_pair_virtual_events_kept
    BEFORE DELETE ON pair_virtual_events
    BEGIN SELECT RAISE(ABORT, 'pair virtual execution history is append-only'); END;
CREATE TRIGGER trg_pair_virtual_allocations_immutable
    BEFORE UPDATE ON pair_virtual_allocations
    BEGIN SELECT RAISE(ABORT, 'pair virtual client identifier allocations are append-only'); END;
CREATE TRIGGER trg_pair_virtual_allocations_kept
    BEFORE DELETE ON pair_virtual_allocations
    BEGIN SELECT RAISE(ABORT, 'pair virtual client identifier allocations are append-only'); END;
CREATE TRIGGER pending_limit_orders_owner_kind_insert
BEFORE INSERT ON pending_limit_orders
WHEN NEW.owner_kind NOT IN ('manual','quote')
  OR (NEW.owner_kind = 'quote' AND (NEW.quote_role NOT IN ('bid','ask','stop','close') OR NEW.client_order_id = '' OR NEW.quote_generation < 1 OR NEW.partition_scope NOT IN ('live','paper')))
BEGIN SELECT RAISE(ABORT, 'pending_limit_orders row has an invalid owner kind or quote identity'); END;
CREATE TRIGGER pending_limit_orders_owner_kind_update
BEFORE UPDATE OF owner_kind, partition_scope, partition_source, strategy_id, quote_role, quote_generation, client_order_id, request_json, provider, account_key ON pending_limit_orders
WHEN OLD.owner_kind = 'quote' OR NEW.owner_kind != OLD.owner_kind
BEGIN SELECT RAISE(ABORT, 'a quote order identity and frozen request are immutable'); END;
CREATE TRIGGER pending_limit_orders_quote_retained
BEFORE DELETE ON pending_limit_orders
WHEN OLD.owner_kind = 'quote'
BEGIN SELECT RAISE(ABORT, 'quote order evidence is retained'); END;
CREATE TRIGGER quote_fills_immutable
BEFORE UPDATE ON quote_fills
WHEN NEW.digest != OLD.digest OR NEW.side != OLD.side OR NEW.px != OLD.px OR NEW.size != OLD.size OR NEW.fee != OLD.fee
  OR NEW.fee_asset != OLD.fee_asset OR NEW.raw_json != OLD.raw_json OR NEW.strategy_id != OLD.strategy_id
  OR NEW.scope != OLD.scope OR NEW.source != OLD.source OR NEW.kind != OLD.kind
  OR (OLD.settlement_id != 0 AND NEW.settlement_id != OLD.settlement_id)
BEGIN SELECT RAISE(ABORT, 'quote fill evidence is immutable'); END;
CREATE TRIGGER quote_fills_retained
BEFORE DELETE ON quote_fills
BEGIN SELECT RAISE(ABORT, 'quote fill evidence is retained'); END;
CREATE TRIGGER quote_settlements_retained
BEFORE DELETE ON quote_settlements
BEGIN SELECT RAISE(ABORT, 'quote settlements are retained'); END;
CREATE TRIGGER quote_settlements_immutable
BEFORE UPDATE ON quote_settlements
BEGIN SELECT RAISE(ABORT, 'quote settlements are immutable'); END;
CREATE TRIGGER quote_funding_evidence_immutable
BEFORE UPDATE ON quote_funding_evidence
WHEN NEW.digest != OLD.digest OR NEW.raw_json != OLD.raw_json OR NEW.strategy_id != OLD.strategy_id
  OR (OLD.settlement_id != 0 AND NEW.settlement_id != OLD.settlement_id)
BEGIN SELECT RAISE(ABORT, 'quote funding evidence is immutable'); END;
CREATE TRIGGER quote_funding_evidence_retained
BEFORE DELETE ON quote_funding_evidence
BEGIN SELECT RAISE(ABORT, 'quote funding evidence is retained'); END;
CREATE INDEX idx_closed_positions_strategy ON closed_positions(strategy_id);
CREATE INDEX idx_closed_positions_symbol ON closed_positions(symbol);
CREATE INDEX idx_closed_positions_closed_at ON closed_positions(closed_at DESC);
CREATE INDEX idx_closed_opt_strategy ON closed_option_positions(strategy_id);
CREATE INDEX idx_closed_opt_underlying ON closed_option_positions(underlying);
CREATE INDEX idx_closed_opt_closed_at ON closed_option_positions(closed_at DESC);
CREATE INDEX idx_trades_strategy ON trades(strategy_id);
CREATE INDEX idx_trades_symbol ON trades(symbol);
CREATE INDEX idx_trades_timestamp ON trades(timestamp DESC);
CREATE INDEX idx_cashflow_journal_account ON cashflow_journal(platform, account);
CREATE INDEX idx_trade_diag_strategy ON trade_diagnostics(strategy_id);
CREATE INDEX idx_trade_diag_position ON trade_diagnostics(strategy_id, position_id);
CREATE INDEX idx_trade_diag_closed_at ON trade_diagnostics(closed_at DESC, rowid DESC);
CREATE INDEX idx_trade_diag_strategy_closed_at ON trade_diagnostics(strategy_id, closed_at DESC, rowid DESC);
CREATE INDEX idx_regime_hist_key ON regime_window_history(platform, symbol, timeframe, spec_json, window, id);
CREATE INDEX idx_regime_hist_ts ON regime_window_history(ts);
CREATE INDEX idx_regime_trans_key ON regime_window_transitions(platform, symbol, timeframe, spec_json, window, id);
CREATE INDEX idx_regime_trans_ts ON regime_window_transitions(ts);
CREATE INDEX idx_trades_close ON trades(strategy_id, is_close);
CREATE INDEX idx_trades_strategy_position ON trades(strategy_id, position_id);
CREATE INDEX idx_trades_strategy_timestamp ON trades(strategy_id, timestamp DESC, rowid DESC);
CREATE UNIQUE INDEX idx_entry_intents_one_unresolved
    ON entry_order_intents(scope, source, strategy_id)
    WHERE status IN ('reserved','submitting','settling');
CREATE UNIQUE INDEX idx_entry_intents_client_order
    ON entry_order_intents(provider, account_key, client_order_id);
CREATE INDEX idx_entry_intents_decision
    ON entry_order_intents(scope, source, strategy_id, symbol, interval_ms, target_side, bar_open_ms);
CREATE UNIQUE INDEX idx_rh_attempts_ref
    ON robinhood_order_attempts(account_key, ref_id);
CREATE UNIQUE INDEX idx_rh_attempts_one_unresolved_coin
    ON robinhood_order_attempts(account_key, symbol)
    WHERE status IN ('submitting','open','unknown','settling');
CREATE UNIQUE INDEX idx_rh_attempts_one_unresolved_strategy
    ON robinhood_order_attempts(scope, source, strategy_id)
    WHERE status IN ('submitting','open','unknown','settling');
CREATE UNIQUE INDEX idx_basket_actions_one_pending
    ON basket_member_actions(scope, source, basket_id)
    WHERE group_mode = 'basket' AND status IN ('reserved','submitting','unresolved','settling');
CREATE UNIQUE INDEX idx_basket_actions_client_order
    ON basket_member_actions(provider, account_key, client_order_id);
CREATE UNIQUE INDEX idx_pair_actions_one_elective
    ON basket_member_actions(scope, source, basket_id)
    WHERE group_mode = 'pair' AND purpose IN ('pair_entry','pair_exit') AND status IN ('reserved','submitting','unresolved','settling');
CREATE UNIQUE INDEX idx_pair_actions_one_protective
    ON basket_member_actions(scope, source, provider, account_key, instrument)
    WHERE group_mode = 'pair' AND ((purpose = 'pair_protective' AND status IN ('reserved','submitting','unresolved','settling'))
      OR (purpose IN ('pair_kill_switch','pair_operator') AND status IN ('reserved','submitting','unresolved')));
CREATE INDEX idx_pair_quote_uses_pair ON pair_quote_event_uses(scope, source, pair_id, slot_ms);
CREATE UNIQUE INDEX idx_pair_funding_one_applied
    ON pair_funding_events(scope, source, strategy_id, provider, account_key, instrument, event_ms) WHERE status = 'applied';
CREATE UNIQUE INDEX idx_pair_funding_inventory_fill
    ON pair_funding_inventory(scope, source, pair_id, strategy_id, action_id, fill_id) WHERE fill_id != '';
CREATE INDEX idx_pair_virtual_events_capacity
    ON pair_virtual_events(run_tag, member_key_json, source_contract, observation_id, book_side, level_index) WHERE kind = 'consume';
CREATE INDEX idx_pair_virtual_events_client
    ON pair_virtual_events(run_tag, member_key_json, client_order_id);
CREATE INDEX idx_basket_member_actions_client
    ON basket_member_actions(client_order_id);
CREATE UNIQUE INDEX idx_pending_limit_quote_client
    ON pending_limit_orders(provider, account_key, client_order_id) WHERE owner_kind = 'quote';
CREATE UNIQUE INDEX idx_pending_limit_quote_role
    ON pending_limit_orders(partition_scope, partition_source, strategy_id, quote_role, quote_generation) WHERE owner_kind = 'quote';
CREATE INDEX idx_pending_limit_owner_kind ON pending_limit_orders(owner_kind);
CREATE UNIQUE INDEX idx_quote_contracts_namespace
    ON quote_contracts(provider, account_key, namespace, coin);
CREATE INDEX idx_quote_fills_owner ON quote_fills(scope, source, strategy_id, settlement_id);
COMMIT;
