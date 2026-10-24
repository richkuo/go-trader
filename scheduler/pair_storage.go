package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	pairPurposeEntry      = "pair_entry"
	pairPurposeExit       = "pair_exit"
	pairPurposeProtective = "pair_protective"
	pairPurposeKillSwitch = "pair_kill_switch"
	pairPurposeOperator   = "pair_operator"
)

const (
	pairUseReserved = "reserved"
	pairUseConsumed = "consumed"
	pairUseReleased = "released"
)

const pairPendingStatusSQL = `('reserved','submitting','unresolved','settling')`

var errPairGuardMiss = errors.New("a guarded pair write did not apply")

func pairElectivePurpose(p string) bool {
	return p == pairPurposeEntry || p == pairPurposeExit
}

func pairProtectivePurpose(p string) bool {
	return p == pairPurposeProtective || p == pairPurposeKillSwitch || p == pairPurposeOperator
}

var basketActionPairColumns = []basketColumnDefault{
	{"group_mode", "'basket'"},
	{"instrument", "''"},
	{"native_qty", "0"},
	{"contract_size", "0"},
	{"request_json", "''"},
	{"quote_event_id", "''"},
	{"price_bound", "0"},
	{"accept_deadline_ms", "0"},
	{"submitted_at_ms", "0"},
	{"confirmed_notional", "0"},
	{"fee_present", "0"},
	{"fee_currency", "''"},
	{"fee_amount", "0"},
	{"fee_evidence_id", "''"},
	{"fee_evidence_version", "0"},
	{"applied_notional", "0"},
	{"applied_fee_version", "0"},
	{"terminal", "0"},
	{"coverage_through_ms", "0"},
	{"retry_count", "0"},
	{"retry_at_ms", "0"},
	{"recovery_json", "''"},
	{"booked_json", "''"},
	{"accepted_at_ms", "0"},
	{"fill_coverage_json", "''"},
	{"fee_coverage_json", "''"},
	{"fee_final", "0"},
	{"settlement_json", "''"},
}

var basketActionSettlementMigrations = []string{
	"ALTER TABLE basket_member_actions ADD COLUMN accepted_at_ms INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE basket_member_actions ADD COLUMN fill_coverage_json TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE basket_member_actions ADD COLUMN fee_coverage_json TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE basket_member_actions ADD COLUMN fee_final INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE basket_member_actions ADD COLUMN settlement_json TEXT NOT NULL DEFAULT ''",
}

func basketActionProjectedColumns() []basketColumnDefault {
	out := append([]basketColumnDefault(nil), basketActionLifecycleColumns...)
	return append(out, basketActionPairColumns...)
}

const basketActionsV2DDL = `CREATE TABLE basket_member_actions_v2 (
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
    status TEXT NOT NULL CHECK (status IN ('reserved','submitting','unresolved','settling','committed','no_fill')),
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
)`

const basketActionsLegacyCopyColumns = `action_id, scope, source, basket_id, deadline_ms, strategy_id, symbol, direction, seq,
    order_side, position_side, status, requested_qty, pre_qty, pre_notional_usd, mark_px, protection_json,
    provider, account_key, client_order_id, venue_order_id, entry_intent_id, decision_bar_open_ms,
    confirmed_qty, confirmed_avg_px, confirmed_fee, applied_qty, applied_fee, evidence_json, hold_reason,
    created_at_ms, updated_at_ms, purpose, lifecycle_position_id, expiry_ms, lifecycle_attempt`

var basketActionIndexDDL = []string{
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_basket_actions_one_pending
    ON basket_member_actions(scope, source, basket_id)
    WHERE group_mode = 'basket' AND status IN ('reserved','submitting','unresolved','settling')`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_basket_actions_client_order
    ON basket_member_actions(provider, account_key, client_order_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_pair_actions_one_elective
    ON basket_member_actions(scope, source, basket_id)
    WHERE group_mode = 'pair' AND purpose IN ('pair_entry','pair_exit') AND status IN ('reserved','submitting','unresolved','settling')`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_pair_actions_one_protective
    ON basket_member_actions(scope, source, provider, account_key, instrument)
    WHERE group_mode = 'pair' AND purpose IN ('pair_protective','pair_kill_switch','pair_operator') AND status IN ('reserved','submitting','unresolved','settling')`,
	`CREATE TRIGGER IF NOT EXISTS trg_basket_actions_identity_immutable
    BEFORE UPDATE ON basket_member_actions
    WHEN NEW.group_mode != OLD.group_mode OR NEW.purpose != OLD.purpose OR NEW.client_order_id != OLD.client_order_id
      OR NEW.provider != OLD.provider OR NEW.account_key != OLD.account_key OR NEW.instrument != OLD.instrument
      OR NEW.strategy_id != OLD.strategy_id OR NEW.basket_id != OLD.basket_id
      OR (OLD.group_mode = 'pair' AND (NEW.request_json != OLD.request_json OR NEW.quote_event_id != OLD.quote_event_id
          OR NEW.price_bound != OLD.price_bound OR NEW.accept_deadline_ms != OLD.accept_deadline_ms OR NEW.requested_qty != OLD.requested_qty))
    BEGIN SELECT RAISE(ABORT, 'basket action identity, mode and request are immutable'); END`,
}

const entryIntentsV2DDL = `CREATE TABLE entry_order_intents_v2 (
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
)`

var entryIntentIndexDDL = []string{
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_entry_intents_one_unresolved
    ON entry_order_intents(scope, source, strategy_id)
    WHERE status IN ('reserved','submitting','settling')`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_entry_intents_client_order
    ON entry_order_intents(provider, account_key, client_order_id)`,
	`CREATE INDEX IF NOT EXISTS idx_entry_intents_decision
    ON entry_order_intents(scope, source, strategy_id, symbol, interval_ms, target_side, bar_open_ms)`,
	`CREATE TRIGGER IF NOT EXISTS trg_entry_intents_owner_immutable
    BEFORE UPDATE ON entry_order_intents
    WHEN NEW.owner_mode != OLD.owner_mode OR NEW.owner_group_id != OLD.owner_group_id
      OR NEW.decision_kind != OLD.decision_kind OR NEW.quote_event_id != OLD.quote_event_id
      OR NEW.client_order_id != OLD.client_order_id OR NEW.strategy_id != OLD.strategy_id
    BEGIN SELECT RAISE(ABORT, 'entry intent owner and decision identity are immutable'); END`,
}

const pairQuoteUseDDL = `
CREATE TABLE IF NOT EXISTS pair_quote_event_uses (
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
CREATE INDEX IF NOT EXISTS idx_pair_quote_uses_pair ON pair_quote_event_uses(scope, source, pair_id, slot_ms);
`

var basketContractProgressMigrations = []string{
	"ALTER TABLE basket_contracts ADD COLUMN progress_json TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE basket_contracts ADD COLUMN progress_seq INTEGER NOT NULL DEFAULT 0",
}

func (sdb *StateDB) rebuildTableTx(table, ddl, copySQL string, after []string) error {
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
	stmts = append(stmts, after...)
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("rebuild %s: %w", table, err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) migrateEntryIntentOwnerSchema() error {
	has, err := sdb.tableHasColumn("entry_order_intents", "owner_mode")
	if err != nil {
		return fmt.Errorf("introspect entry intent columns: %w", err)
	}
	if !has {
		copySQL := `INSERT INTO entry_order_intents_v2 (` + entryIntentBaseColumns + `, decision_kind, quote_event_id, owner_mode, owner_group_id)
			SELECT ` + entryIntentBaseColumns + `, 'candle', '',
			    CASE WHEN request_json LIKE '%"kind":"basket:%' THEN 'basket' ELSE '' END, ''
			FROM entry_order_intents ORDER BY rowid`
		if err := sdb.rebuildTableTx("entry_order_intents", entryIntentsV2DDL, copySQL, entryIntentIndexDDL); err != nil {
			return err
		}
		return nil
	}
	for _, ddl := range entryIntentIndexDDL {
		if _, err := sdb.db.Exec(ddl); err != nil {
			return fmt.Errorf("migrate entry intent indexes: %w", err)
		}
	}
	return nil
}

func (sdb *StateDB) migratePairSchema() error {
	for _, ddl := range basketContractProgressMigrations {
		if _, err := sdb.db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate basket contract progress: %w", err)
		}
	}
	has, err := sdb.tableHasColumn("basket_member_actions", "group_mode")
	if err != nil {
		return fmt.Errorf("introspect basket action columns: %w", err)
	}
	after := append([]string{`DROP INDEX IF EXISTS idx_basket_actions_one_pending`}, basketActionIndexDDL...)
	if !has {
		copySQL := `INSERT INTO basket_member_actions_v2 (` + basketActionsLegacyCopyColumns + `)
			SELECT ` + basketActionsLegacyCopyColumns + ` FROM basket_member_actions ORDER BY rowid`
		if err := sdb.rebuildTableTx("basket_member_actions", basketActionsV2DDL, copySQL, after); err != nil {
			return err
		}
	} else {
		for _, ddl := range basketActionIndexDDL {
			if _, err := sdb.db.Exec(ddl); err != nil {
				return fmt.Errorf("migrate basket action indexes: %w", err)
			}
		}
	}
	for _, ddl := range basketActionSettlementMigrations {
		if _, err := sdb.db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrate pair settlement columns: %w", err)
		}
	}
	if _, err := sdb.db.Exec(pairQuoteUseDDL); err != nil {
		return fmt.Errorf("create pair quote event uses: %w", err)
	}
	return nil
}

func checkPendingGroupActionTx(tx *sql.Tx, rt *basketRuntime, a *basketAction) error {
	var pending int
	switch {
	case !a.pair():
		if err := tx.QueryRow(`SELECT COUNT(*) FROM basket_member_actions WHERE scope = ? AND source = ? AND basket_id = ?
			AND group_mode = 'basket' AND status IN `+pairPendingStatusSQL, string(a.Partition.Scope), a.Partition.Source, a.BasketID).Scan(&pending); err != nil {
			return fmt.Errorf("read pending basket actions: %w", err)
		}
		if pending > 0 {
			return fmt.Errorf("basket %s already has an unresolved member action", a.BasketID)
		}
	case pairElectivePurpose(a.Purpose):
		if err := tx.QueryRow(`SELECT COUNT(*) FROM basket_member_actions WHERE scope = ? AND source = ? AND basket_id = ?
			AND group_mode = 'pair' AND purpose IN ('pair_entry','pair_exit') AND status IN `+pairPendingStatusSQL,
			string(a.Partition.Scope), a.Partition.Source, a.BasketID).Scan(&pending); err != nil {
			return fmt.Errorf("read pending pair elective actions: %w", err)
		}
		if pending > 0 {
			return fmt.Errorf("pair %s already has an unresolved elective action", a.BasketID)
		}
	case pairProtectivePurpose(a.Purpose):
		if err := tx.QueryRow(`SELECT COUNT(*) FROM basket_member_actions WHERE scope = ? AND source = ? AND provider = ? AND account_key = ? AND instrument = ?
			AND group_mode = 'pair' AND purpose IN ('pair_protective','pair_kill_switch','pair_operator') AND status IN `+pairPendingStatusSQL,
			string(a.Partition.Scope), a.Partition.Source, a.Provider, a.AccountKey, a.Instrument).Scan(&pending); err != nil {
			return fmt.Errorf("read pending pair protective reductions: %w", err)
		}
		if pending > 0 {
			return fmt.Errorf("venue account %s instrument %s already has an unresolved protective reduction", a.AccountKey, a.Instrument)
		}
	default:
		return fmt.Errorf("pair action %s has unknown purpose %q", a.ActionID, a.Purpose)
	}
	return nil
}

type pairFeeFix struct {
	TradeTsMs        int64   `json:"trade_ts_ms"`
	TradeTs          string  `json:"trade_ts,omitempty"`
	Symbol           string  `json:"symbol"`
	OrderID          string  `json:"order_id"`
	DeltaUSD         float64 `json:"delta_usd"`
	Version          int64   `json:"version"`
	ClosedPositionID string  `json:"closed_position_id,omitempty"`
	IsClose          bool    `json:"is_close,omitempty"`
	IntentID         string  `json:"intent_id,omitempty"`
	ConfirmedFee     float64 `json:"confirmed_fee,omitempty"`
	AppliedFeeUSD    float64 `json:"applied_fee_usd,omitempty"`
	TradeRowID       int64   `json:"trade_row_id,omitempty"`
	CostModelVersion int     `json:"cost_model_version,omitempty"`
	CostPolicyID     string  `json:"cost_policy_id,omitempty"`
	FeeSource        string  `json:"fee_source,omitempty"`
}

type pairBookedTrade struct {
	TradeTsMs        int64   `json:"trade_ts_ms"`
	TradeTs          string  `json:"trade_ts,omitempty"`
	Qty              float64 `json:"qty"`
	Px               float64 `json:"px"`
	FeeUSD           float64 `json:"fee_usd"`
	OrderID          string  `json:"order_id"`
	IsClose          bool    `json:"is_close,omitempty"`
	ClosedPositionID string  `json:"closed_position_id,omitempty"`
}

func (a *basketAction) bookedTrades() []pairBookedTrade {
	var out []pairBookedTrade
	if strings.TrimSpace(a.BookedJSON) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(a.BookedJSON), &out)
	return out
}

func pairActionStatusForWrite(a *basketAction, withBook bool) string {
	if withBook || a.Status != basketActionCommitted || a.durableStatus == basketActionCommitted || a.recoveryNote().NativeBooked {
		return a.Status
	}
	if a.durableStatus == "" || basketActionRank(a.durableStatus) < basketActionRank(basketActionSettling) {
		return basketActionSettling
	}
	return a.durableStatus
}

const pairActionTransitionSQL = `(
    status = excluded.status
    OR (status = 'reserved' AND excluded.status IN ('submitting','no_fill'))
    OR (status = 'submitting' AND excluded.status IN ('unresolved','settling','committed','no_fill'))
    OR (status = 'unresolved' AND excluded.status IN ('settling','committed','no_fill'))
    OR (status = 'settling' AND excluded.status IN ('unresolved','committed','no_fill'))
)`

func upsertPairActionRow(exec sqlExecer, sid string, a *basketAction, withBook bool) error {
	if !a.pair() {
		return fmt.Errorf("pair writer received basket action %s", a.ActionID)
	}
	status := pairActionStatusForWrite(a, withBook)
	if basketActionRank(status) < 0 {
		return fmt.Errorf("pair action %s has unknown status %q", a.ActionID, status)
	}
	book := 0
	if withBook {
		book = 1
	}
	feePresent, terminal, feeFinal := 0, 0, 0
	if a.FeePresent {
		feePresent = 1
	}
	if a.Terminal {
		terminal = 1
	}
	if a.FeeFinal {
		feeFinal = 1
	}
	res, err := exec.Exec(`INSERT INTO basket_member_actions (`+basketActionColumns+`, group_mode, instrument, native_qty, contract_size,
		    request_json, quote_event_id, price_bound, accept_deadline_ms, submitted_at_ms, confirmed_notional, fee_present, fee_currency,
		    fee_amount, fee_evidence_id, fee_evidence_version, applied_notional, applied_fee_version, terminal, coverage_through_ms,
		    retry_count, retry_at_ms, recovery_json, booked_json, accepted_at_ms, fill_coverage_json, fee_coverage_json, fee_final, settlement_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		        'pair', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(action_id) DO UPDATE SET
		    status = excluded.status,
		    venue_order_id = CASE WHEN excluded.venue_order_id != '' THEN excluded.venue_order_id ELSE venue_order_id END,
		    entry_intent_id = CASE WHEN entry_intent_id = '' THEN excluded.entry_intent_id ELSE entry_intent_id END,
		    submitted_at_ms = CASE WHEN submitted_at_ms = 0 THEN excluded.submitted_at_ms ELSE submitted_at_ms END,
		    confirmed_qty = excluded.confirmed_qty,
		    confirmed_notional = excluded.confirmed_notional,
		    confirmed_avg_px = excluded.confirmed_avg_px,
		    fee_present = CASE WHEN excluded.fee_evidence_version > fee_evidence_version THEN excluded.fee_present ELSE fee_present END,
		    fee_currency = CASE WHEN excluded.fee_evidence_version > fee_evidence_version THEN excluded.fee_currency ELSE fee_currency END,
		    fee_amount = CASE WHEN excluded.fee_evidence_version > fee_evidence_version THEN excluded.fee_amount ELSE fee_amount END,
		    fee_evidence_id = CASE WHEN excluded.fee_evidence_version > fee_evidence_version THEN excluded.fee_evidence_id ELSE fee_evidence_id END,
		    confirmed_fee = CASE WHEN excluded.fee_evidence_version > fee_evidence_version THEN excluded.confirmed_fee ELSE confirmed_fee END,
		    fee_evidence_version = MAX(fee_evidence_version, excluded.fee_evidence_version),
		    applied_qty = CASE WHEN `+fmt.Sprint(book)+` = 1 THEN excluded.applied_qty ELSE applied_qty END,
		    applied_notional = CASE WHEN `+fmt.Sprint(book)+` = 1 THEN excluded.applied_notional ELSE applied_notional END,
		    applied_fee = CASE WHEN `+fmt.Sprint(book)+` = 1 THEN excluded.applied_fee ELSE applied_fee END,
		    applied_fee_version = CASE WHEN `+fmt.Sprint(book)+` = 1 THEN excluded.applied_fee_version ELSE applied_fee_version END,
		    booked_json = CASE WHEN `+fmt.Sprint(book)+` = 1 THEN excluded.booked_json ELSE booked_json END,
		    settlement_json = CASE WHEN `+fmt.Sprint(book)+` = 1 THEN excluded.settlement_json ELSE settlement_json END,
		    accepted_at_ms = CASE WHEN accepted_at_ms = 0 THEN excluded.accepted_at_ms ELSE accepted_at_ms END,
		    fill_coverage_json = excluded.fill_coverage_json,
		    fee_coverage_json = excluded.fee_coverage_json,
		    fee_final = MAX(fee_final, excluded.fee_final),
		    terminal = MAX(terminal, excluded.terminal),
		    coverage_through_ms = MAX(coverage_through_ms, excluded.coverage_through_ms),
		    retry_count = MAX(retry_count, excluded.retry_count),
		    retry_at_ms = excluded.retry_at_ms,
		    recovery_json = excluded.recovery_json,
		    evidence_json = CASE WHEN excluded.evidence_json != '' THEN excluded.evidence_json ELSE evidence_json END,
		    hold_reason = excluded.hold_reason,
		    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)
		WHERE group_mode = 'pair'
		  AND request_json = excluded.request_json
		  AND `+pairActionTransitionSQL+`
		  AND excluded.confirmed_qty >= confirmed_qty
		  AND excluded.confirmed_notional >= confirmed_notional * (1 - 1e-12) - 1e-12
		  AND (`+fmt.Sprint(book)+` = 0 OR (excluded.applied_qty >= applied_qty AND excluded.applied_qty <= excluded.confirmed_qty * (1 + 1e-9) + 1e-12))
		  AND NOT (excluded.fee_evidence_version = fee_evidence_version AND fee_evidence_version > 0
		           AND (excluded.fee_evidence_id != fee_evidence_id OR excluded.fee_amount != fee_amount OR excluded.fee_currency != fee_currency OR excluded.fee_present != fee_present))
		  AND (fill_coverage_json = excluded.fill_coverage_json OR fill_coverage_json = ?)
		  AND (fee_coverage_json = excluded.fee_coverage_json OR (fee_coverage_json = ? AND excluded.fee_evidence_version > fee_evidence_version))
		  AND excluded.fee_final >= fee_final
		  AND (accepted_at_ms = 0 OR excluded.accepted_at_ms IN (0, accepted_at_ms))
		  AND (`+fmt.Sprint(book)+` = 0 OR settlement_json = excluded.settlement_json OR settlement_json = ?)`,
		a.ActionID, string(a.Partition.Scope), a.Partition.Source, a.BasketID, a.DeadlineMs, sid, a.Symbol, a.Direction, a.Seq,
		a.OrderSide, a.PositionSide, status, a.RequestedQty, a.PreQty, a.PreNotionalUSD, a.MarkPx, a.ProtectionJSON,
		a.Provider, a.AccountKey, a.ClientOrderID, a.VenueOrderID, a.EntryIntentID, a.DecisionBarOpenMs,
		a.ConfirmedQty, a.ConfirmedAvgPx, a.ConfirmedFee, a.AppliedQty, a.AppliedFee, a.EvidenceJSON, a.HoldReason,
		a.CreatedAtMs, a.UpdatedAtMs, a.Purpose, a.LifecyclePositionID, a.ExpiryMs, a.LifecycleAttempt,
		a.Instrument, a.NativeQty, a.ContractSize, a.RequestJSON, a.QuoteEventID, a.PriceBound, a.AcceptDeadlineMs,
		a.SubmittedAtMs, a.ConfirmedNotional, feePresent, a.FeeCurrency, a.FeeAmount, a.FeeEvidenceID, a.FeeEvidenceVersion,
		a.AppliedNotional, a.AppliedFeeVersion, terminal, a.CoverageThroughMs, a.RetryCount, a.RetryAtMs, a.RecoveryJSON, a.BookedJSON,
		a.AcceptedAtMs, a.FillCoverageJSON, a.FeeCoverageJSON, feeFinal, a.SettlementJSON,
		a.durableFills, a.durableFees, a.durableLedger)
	if err != nil {
		return fmt.Errorf("upsert pair action %s: %w", a.ActionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upsert pair action %s: %w", a.ActionID, err)
	}
	if n != 1 {
		return fmt.Errorf("pair action %s (%s): %w; the saved record is newer or the transition is not permitted", a.ActionID, status, errPairGuardMiss)
	}
	return nil
}

type pairQuoteEvent struct {
	PairID              string `json:"pair_id"`
	ContractFingerprint string `json:"contract_fingerprint"`
	SlotMs              int64  `json:"slot_ms"`
	CutoffMs            int64  `json:"cutoff_ms"`
	InputFingerprint    string `json:"input_fingerprint"`
}

func (e pairQuoteEvent) ID() string {
	return "pq-" + sha256Hex([]byte(fmt.Sprintf("%s|%s|%d|%d|%s", e.PairID, e.ContractFingerprint, e.SlotMs, e.CutoffMs, e.InputFingerprint)))[:24]
}

func (e pairQuoteEvent) valid() bool {
	return e.PairID != "" && e.ContractFingerprint != "" && e.SlotMs > 0 && e.CutoffMs > 0 && e.InputFingerprint != ""
}

func pairQuoteEventFromSlot(rb *basketRebalance) (pairQuoteEvent, bool) {
	if rb == nil {
		return pairQuoteEvent{}, false
	}
	e := pairQuoteEvent{PairID: rb.BasketID, ContractFingerprint: rb.ContractFP, SlotMs: rb.DeadlineMs, CutoffMs: rb.CutoffMs, InputFingerprint: rb.InputFP}
	return e, e.valid()
}

type pairQuoteEventUse struct {
	Partition   RiskPartition
	PairID      string
	StrategyID  string
	Event       pairQuoteEvent
	EventID     string
	ActionID    string
	Status      string
	CreatedAtMs int64
	UpdatedAtMs int64

	durableStatus string
}

func (u *pairQuoteEventUse) key() string {
	return u.StrategyID + "|" + u.EventID
}

func pairUseRank(status string) int {
	switch status {
	case pairUseReserved:
		return 0
	case pairUseConsumed, pairUseReleased:
		return 1
	}
	return -1
}

func insertPairQuoteUseTx(tx *sql.Tx, sid string, u *pairQuoteEventUse) error {
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pair_quote_event_uses WHERE scope = ? AND source = ? AND strategy_id = ? AND event_id = ?`,
		string(u.Partition.Scope), u.Partition.Source, sid, u.EventID).Scan(&existing); err != nil {
		return fmt.Errorf("read pair quote event use: %w", err)
	}
	if existing > 0 {
		return fmt.Errorf("member %s already used quote event %s; a duplicate event never resets or enlarges intent", u.StrategyID, u.EventID)
	}
	_, err := tx.Exec(`INSERT INTO pair_quote_event_uses (scope, source, pair_id, strategy_id, event_id, contract_fingerprint, slot_ms, cutoff_ms,
		    input_fingerprint, action_id, status, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(u.Partition.Scope), u.Partition.Source, u.PairID, sid, u.EventID, u.Event.ContractFingerprint, u.Event.SlotMs,
		u.Event.CutoffMs, u.Event.InputFingerprint, u.ActionID, u.Status, u.CreatedAtMs, u.UpdatedAtMs)
	if err != nil {
		return fmt.Errorf("record pair quote event use %s for %s: %w", u.EventID, u.StrategyID, err)
	}
	return nil
}

func updatePairQuoteUseTx(tx *sql.Tx, sid string, u *pairQuoteEventUse) error {
	res, err := tx.Exec(`UPDATE pair_quote_event_uses SET status = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE scope = ? AND source = ? AND strategy_id = ? AND event_id = ? AND action_id = ?
		  AND (status = ? OR status = 'reserved')`,
		u.Status, u.UpdatedAtMs, string(u.Partition.Scope), u.Partition.Source, sid, u.EventID, u.ActionID, u.Status)
	if err != nil {
		return fmt.Errorf("update pair quote event use %s for %s: %w", u.EventID, u.StrategyID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("pair quote event use %s for %s: %w", u.EventID, u.StrategyID, errPairGuardMiss)
	}
	return nil
}

type pairObligation struct {
	Kind     string `json:"kind"`
	MemberID string `json:"member_id,omitempty"`
	ActionID string `json:"action_id,omitempty"`
	Detail   string `json:"detail"`
	AtMs     int64  `json:"at_ms"`
}

type pairRetry struct {
	Count     int   `json:"count"`
	RetryAtMs int64 `json:"retry_at_ms"`
}

type pairBoundRecord struct {
	AtMs        int64              `json:"at_ms"`
	Assumptions []string           `json:"assumptions,omitempty"`
	Readings    map[string]float64 `json:"readings,omitempty"`
	Breaches    []string           `json:"breaches,omitempty"`
}

const (
	pairLifecycleFlat         = "flat"
	pairLifecycleOpening      = "opening"
	pairLifecycleMatched      = "matched"
	pairLifecycleExiting      = "exiting"
	pairLifecycleRecovery     = "recovery"
	pairLifecycleOperatorHeld = "operator_held"
)

const pairProgressVersion = 1

type pairProgress struct {
	V                  int                  `json:"v"`
	Seq                int64                `json:"seq"`
	Lifecycle          string               `json:"lifecycle"`
	OriginSlotMs       int64                `json:"origin_slot_ms,omitempty"`
	ExitReason         string               `json:"exit_reason,omitempty"`
	FirstFillMs        int64                `json:"first_fill_ms,omitempty"`
	RecoveryStartMs    int64                `json:"recovery_start_ms,omitempty"`
	RecoveryReason     string               `json:"recovery_reason,omitempty"`
	IncreasesFrozen    bool                 `json:"increases_frozen,omitempty"`
	FreezeReason       string               `json:"freeze_reason,omitempty"`
	OperatorHeldReason string               `json:"operator_held_reason,omitempty"`
	Exceptions         []basketException    `json:"exceptions,omitempty"`
	Alerts             []string             `json:"alerts,omitempty"`
	Obligations        []pairObligation     `json:"obligations,omitempty"`
	Retries            map[string]pairRetry `json:"retries,omitempty"`
	LastBound          *pairBoundRecord     `json:"last_bound,omitempty"`
	CostEvidence       json.RawMessage      `json:"cost_evidence,omitempty"`
}

func (p *pairProgress) alerted(key string) bool {
	for _, k := range p.Alerts {
		if k == key {
			return true
		}
	}
	return false
}

func (p *pairProgress) markAlerted(key string) {
	if !p.alerted(key) {
		p.Alerts = append(p.Alerts, key)
		sort.Strings(p.Alerts)
	}
}

func (p *pairProgress) obligation(kind, memberID, actionID string) *pairObligation {
	for i := range p.Obligations {
		o := &p.Obligations[i]
		if o.Kind == kind && o.MemberID == memberID && o.ActionID == actionID {
			return o
		}
	}
	return nil
}

func (p *pairProgress) addObligation(o pairObligation) bool {
	if existing := p.obligation(o.Kind, o.MemberID, o.ActionID); existing != nil {
		if existing.Detail == o.Detail {
			return false
		}
		existing.Detail, existing.AtMs = o.Detail, o.AtMs
		return true
	}
	p.Obligations = append(p.Obligations, o)
	return true
}

func (p *pairProgress) clearObligation(kind, memberID, actionID string) bool {
	kept := p.Obligations[:0]
	removed := false
	for _, o := range p.Obligations {
		if o.Kind == kind && o.MemberID == memberID && o.ActionID == actionID {
			removed = true
			continue
		}
		kept = append(kept, o)
	}
	p.Obligations = kept
	return removed
}

func (p pairProgress) JSON() string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func parsePairProgress(raw string) (pairProgress, error) {
	p := pairProgress{V: pairProgressVersion, Lifecycle: pairLifecycleFlat}
	if strings.TrimSpace(raw) == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("saved pair progress is unreadable: %w", err)
	}
	if p.V != pairProgressVersion {
		return p, fmt.Errorf("saved pair progress version %d is not supported", p.V)
	}
	return p, nil
}

type pairRuntimeState struct {
	Config    PairConfig
	Limits    pairOperationalLimits
	Progress  pairProgress
	Uses      map[string]*pairQuoteEventUse
	Intents   map[string]*entryIntent
	admission *pairResearchAdmission

	durableSeq int64
}

func newPairRuntimeState(p PairConfig, limits pairOperationalLimits, adm *pairResearchAdmission) *pairRuntimeState {
	return &pairRuntimeState{
		Config:    p,
		Limits:    limits,
		Progress:  pairProgress{V: pairProgressVersion, Lifecycle: pairLifecycleFlat},
		Uses:      make(map[string]*pairQuoteEventUse),
		Intents:   make(map[string]*entryIntent),
		admission: adm,
	}
}

func (rt *basketRuntime) isPair() bool {
	return rt != nil && rt.Pair != nil && rt.Contract.isPair()
}

func (rt *basketRuntime) touchPairProgress() {
	if rt.Pair != nil {
		rt.Pair.Progress.Seq++
	}
}

func (rt *basketRuntime) sortedUseKeys() []string {
	out := make([]string, 0, len(rt.Pair.Uses))
	for k := range rt.Pair.Uses {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (rt *basketRuntime) pairUnresolvedActions() []*basketAction {
	var out []*basketAction
	for _, id := range rt.sortedActionIDs() {
		if a := rt.Actions[id]; basketActionPending(a.Status) {
			out = append(out, a)
		}
	}
	return out
}

func (rt *basketRuntime) pairPendingElective() *basketAction {
	for _, a := range rt.pairUnresolvedActions() {
		if pairElectivePurpose(a.Purpose) {
			return a
		}
	}
	return nil
}

func (rt *basketRuntime) pairPendingProtective(memberID string) *basketAction {
	for _, a := range rt.pairUnresolvedActions() {
		if pairProtectivePurpose(a.Purpose) && a.StrategyID == memberID {
			return a
		}
	}
	return nil
}

func writePairProgressTx(tx *sql.Tx, rt *basketRuntime) error {
	p := rt.Pair.Progress
	res, err := tx.Exec(`UPDATE basket_contracts SET progress_json = ?, progress_seq = ?
		WHERE scope = ? AND source = ? AND basket_id = ? AND contract_fingerprint = ? AND progress_seq <= ?`,
		p.JSON(), p.Seq, string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID, rt.Contract.Fingerprint(), p.Seq)
	if err != nil {
		return fmt.Errorf("write pair %s progress: %w", rt.Contract.BasketID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("pair %s progress (seq %d): %w; a newer saved progress or a changed contract exists", rt.Contract.BasketID, p.Seq, errPairGuardMiss)
	}
	return nil
}

func upsertPairSlotRow(exec sqlExecer, rb *basketRebalance, status string) error {
	if basketRebalanceRank(status) < 0 {
		return fmt.Errorf("pair %s slot %d has unknown status %q", rb.BasketID, rb.DeadlineMs, status)
	}
	frozen := 0
	if rb.IncreasesFrozen {
		frozen = 1
	}
	rankNew := fmt.Sprintf(basketStatusRankSQL, "excluded.status")
	rankOld := fmt.Sprintf(basketStatusRankSQL, "status")
	res, err := exec.Exec(`INSERT INTO basket_rebalances (`+basketRebalanceColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope, source, basket_id, deadline_ms) DO UPDATE SET
		    status = excluded.status,
		    snapshot_id = CASE WHEN snapshot_id = '' THEN excluded.snapshot_id ELSE snapshot_id END,
		    generation = CASE WHEN generation = 0 THEN excluded.generation ELSE generation END,
		    cutoff_ms = CASE WHEN cutoff_ms = 0 THEN excluded.cutoff_ms ELSE cutoff_ms END,
		    input_fingerprint = CASE WHEN input_fingerprint = '' THEN excluded.input_fingerprint ELSE input_fingerprint END,
		    evidence_json = CASE WHEN evidence_json = '' THEN excluded.evidence_json ELSE evidence_json END,
		    targets_json = CASE WHEN targets_json = '' THEN excluded.targets_json ELSE targets_json END,
		    hold_reason = CASE WHEN excluded.hold_reason != '' THEN excluded.hold_reason ELSE hold_reason END,
		    outcome_json = CASE WHEN excluded.outcome_json != '' THEN excluded.outcome_json ELSE outcome_json END,
		    increases_frozen = MAX(increases_frozen, excluded.increases_frozen),
		    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)
		WHERE contract_fingerprint = excluded.contract_fingerprint
		  AND `+rankNew+` >= `+rankOld+`
		  AND (status NOT IN ('completed','held','superseded') OR status = excluded.status)
		  AND (excluded.targets_json = '' OR targets_json = '' OR targets_json = excluded.targets_json)
		  AND (excluded.input_fingerprint = '' OR input_fingerprint = '' OR input_fingerprint = excluded.input_fingerprint)`,
		string(rb.Partition.Scope), rb.Partition.Source, rb.BasketID, rb.DeadlineMs, status, rb.ContractFP,
		rb.SnapshotID, int64(rb.Generation), rb.CutoffMs, rb.InputFP, rb.EvidenceJSON, rb.TargetsJSON, rb.HoldReason,
		rb.outcomeJSON(), frozen, rb.CreatedAtMs, rb.UpdatedAtMs)
	if err != nil {
		return fmt.Errorf("upsert pair %s slot %d: %w", rb.BasketID, rb.DeadlineMs, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("pair %s slot %d (%s): %w", rb.BasketID, rb.DeadlineMs, status, errPairGuardMiss)
	}
	return nil
}

func (sdb *StateDB) writePairTx(tx *sql.Tx, rt *basketRuntime, books map[string]*StrategyState, nowMs int64) (map[string]basketLifecycle, error) {
	sids, err := sdb.checkBasketOwnership(rt)
	if err != nil {
		return nil, err
	}
	full := rt.savesAllMembers(books)
	if err := checkSavedPairCostEvidenceTx(tx, rt); err != nil {
		return nil, err
	}
	if err := upsertBasketContractRow(tx, rt, nowMs); err != nil {
		return nil, err
	}
	for _, d := range rt.sortedDeadlines() {
		rb := rt.Rebalances[d]
		status := rb.Status
		if !full && status == basketRebalanceCompleted && rb.Outcome.Result != "evidence_only" && rb.durableStatus != basketRebalanceCompleted {
			status = basketRebalanceExecuting
			if rb.durableStatus != "" && basketRebalanceRank(rb.durableStatus) < basketRebalanceRank(basketRebalanceExecuting) {
				status = rb.durableStatus
			}
		}
		if err := upsertPairSlotRow(tx, rb, status); err != nil {
			return nil, err
		}
	}
	for _, id := range rt.sortedActionIDs() {
		a := rt.Actions[id]
		withBook := books[a.StrategyID] != nil
		if err := upsertPairActionRow(tx, sids[a.StrategyID], a, withBook); err != nil {
			if withBook && len(a.pendingFee) > 0 && errors.Is(err, errPairGuardMiss) {
				return nil, &pairFeeFixError{ActionID: a.ActionID, Err: fmt.Errorf("%w: the saved action row refused the staged fee correction: %w", errPairFeeFixProvenance, err)}
			}
			return nil, err
		}
		if withBook {
			for _, fix := range a.pendingFee {
				if err := applyPairFeeFixTx(tx, sids[a.StrategyID], fix); err != nil {
					return nil, &pairFeeFixError{ActionID: a.ActionID, Err: err}
				}
			}
		}
	}
	for _, k := range rt.sortedUseKeys() {
		u := rt.Pair.Uses[k]
		if u.Status == u.durableStatus {
			continue
		}
		if u.Status == pairUseConsumed && books[u.StrategyID] == nil {
			continue
		}
		if err := updatePairQuoteUseTx(tx, sids[u.StrategyID], u); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(rt.Pair.Intents))
	for id := range rt.Pair.Intents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		in := rt.Pair.Intents[id]
		withBook := books[in.StrategyID] != nil
		write := *in
		if !withBook && in.Status == entryIntentCommitted && in.durableStatus != entryIntentCommitted {
			write.Status = entryIntentSettling
		}
		if write.Status == in.durableStatus && !withBook && in.durableStatus != "" && in.Status == in.durableStatus {
			continue
		}
		if err := upsertEntryIntentRow(tx, sids[in.StrategyID], &write, withBook); err != nil {
			return nil, err
		}
	}
	if err := writePairProgressTx(tx, rt); err != nil {
		return nil, err
	}
	k := basketKey{Partition: rt.Partition, BasketID: rt.Contract.BasketID}
	next := make(map[string]basketLifecycle, len(books))
	for _, m := range rt.Contract.Members {
		id := m.StrategyID
		if books[id] == nil {
			continue
		}
		if err := writeBasketMemberBookTx(tx, k, sids[id], rt.Expected[id], basketLifecycle{}, nowMs); err != nil {
			return nil, err
		}
		next[id] = basketLifecycle{}
	}
	return next, nil
}

func (fix *pairFeeFix) tradeTs() string {
	if fix.TradeTs != "" {
		return fix.TradeTs
	}
	return formatTime(time.UnixMilli(fix.TradeTsMs).UTC())
}

func applyPairFeeFixTx(tx *sql.Tx, sid string, fix *pairFeeFix) error {
	ts := fix.tradeTs()
	if fix.TradeRowID <= 0 || fix.CostModelVersion == 0 || fix.CostPolicyID == "" {
		return fmt.Errorf("%w: the correction for %s %s at %s carries no checked target row and member stamp", errPairFeeFixProvenance, fix.Symbol, fix.OrderID, ts)
	}
	if fix.FeeSource != FeeSourceUserFills && fix.FeeSource != FeeSourceUserFillsPending {
		return fmt.Errorf("%w: the correction for %s %s at %s carries fee source %q", errPairFeeFixProvenance, fix.Symbol, fix.OrderID, ts, fix.FeeSource)
	}
	var same int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM trades WHERE strategy_id = ? AND timestamp = ? AND symbol = ? AND exchange_order_id = ?`, sid, ts, fix.Symbol, fix.OrderID).Scan(&same); err != nil {
		return err
	}
	if same != 1 {
		return fmt.Errorf("%w: the booked trade row %s %s at %s matched %d saved rows, want exactly 1", errPairFeeFixProvenance, fix.Symbol, fix.OrderID, ts, same)
	}
	res, err := tx.Exec(`UPDATE trades SET exchange_fee = exchange_fee + ?, fee_source = ?
		WHERE rowid = ? AND strategy_id = ? AND timestamp = ? AND symbol = ? AND exchange_order_id = ? AND cost_model_version = ? AND cost_policy_id = ?`,
		fix.DeltaUSD, fix.FeeSource, fix.TradeRowID, sid, ts, fix.Symbol, fix.OrderID, fix.CostModelVersion, fix.CostPolicyID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: the booked trade row %s %s at %s no longer has row id %d with member stamp %d/%s", errPairFeeFixProvenance, fix.Symbol, fix.OrderID, ts, fix.TradeRowID, fix.CostModelVersion, fix.CostPolicyID)
	}
	if fix.IntentID != "" {
		res, err := tx.Exec(`UPDATE entry_order_intents SET applied_fee = ?, confirmed_fee = ?
			WHERE intent_id = ? AND strategy_id = ? AND owner_mode = 'pair'`, fix.AppliedFeeUSD, fix.ConfirmedFee, fix.IntentID, sid)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("the linked entry intent %s matched %d rows, want exactly 1", fix.IntentID, n)
		}
	}
	if fix.ClosedPositionID == "" {
		return nil
	}
	res, err = tx.Exec(`UPDATE closed_positions SET realized_pnl = realized_pnl - ?
		WHERE strategy_id = ? AND symbol = ? AND closed_at = ?`,
		fix.DeltaUSD, sid, fix.Symbol, ts)
	if err != nil {
		return err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("the closed position %s %s at %s matched %d rows, want exactly 1", fix.ClosedPositionID, fix.Symbol, ts, n)
	}
	return nil
}

func markPairDurable(rt *basketRuntime, books map[string]*StrategyState) {
	if !rt.isPair() {
		return
	}
	for _, a := range rt.Actions {
		withBook := books[a.StrategyID] != nil
		a.durableStatus = pairActionStatusForWrite(a, withBook)
		a.durableFills, a.durableFees = a.FillCoverageJSON, a.FeeCoverageJSON
		if withBook {
			a.durableApplied = a.AppliedQty
			a.durableLedger = a.SettlementJSON
			a.pendingFee = nil
			a.feeFixUndo = nil
		}
	}
	for _, u := range rt.Pair.Uses {
		if u.Status == pairUseConsumed && books[u.StrategyID] == nil {
			continue
		}
		u.durableStatus = u.Status
	}
	for id, in := range rt.Pair.Intents {
		withBook := books[in.StrategyID] != nil
		if !withBook && in.Status == entryIntentCommitted {
			if in.durableStatus != entryIntentCommitted {
				in.durableStatus = entryIntentSettling
			}
			continue
		}
		in.durableStatus = in.Status
		if !in.unresolved() {
			delete(rt.Pair.Intents, id)
		}
	}
	rt.Pair.durableSeq = rt.Pair.Progress.Seq
	rt.Pair.admission.markCostDurable(rt.Pair.Progress.CostEvidence)
}

func (sdb *StateDB) persistPairProgressTx(rt *basketRuntime) error {
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin pair progress: %w", err)
	}
	defer tx.Rollback()
	if _, err := sdb.writePairTx(tx, rt, nil, rt.auditNowMs()); err != nil {
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
	markBasketDurable(rt, nil, nil)
	return nil
}

func (sdb *StateDB) reservePairActionTx(rt *basketRuntime, a *basketAction, intent *entryIntent, use *pairQuoteEventUse) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if !rt.isPair() || !a.pair() {
		return fmt.Errorf("pair reservation needs a pair runtime and a pair action")
	}
	if a.Status != basketActionReserved {
		return fmt.Errorf("pair action %s must be reserved before submission, got %q", a.ActionID, a.Status)
	}
	if strings.TrimSpace(a.RequestJSON) == "" || a.Instrument == "" {
		return fmt.Errorf("pair action %s has no immutable request", a.ActionID)
	}
	if a.Direction == basketDirectionIncrease && (intent == nil || use == nil) {
		return fmt.Errorf("pair increase %s needs its linked entry intent and quote event use", a.ActionID)
	}
	if a.Direction == basketDirectionReduce && intent != nil {
		return fmt.Errorf("pair reduction %s takes no entry intent", a.ActionID)
	}
	req, err := pairValidateActionRequest(rt, a, use, rt.auditNowMs())
	if err != nil {
		return err
	}
	sids, err := sdb.checkBasketOwnership(rt)
	if err != nil {
		return err
	}
	sid, ok := sids[a.StrategyID]
	if !ok {
		return fmt.Errorf("pair action %s names %s, which is not a member of pair %s", a.ActionID, a.StrategyID, rt.Contract.BasketID)
	}
	if _, ok := rt.Contract.pairMember(a.StrategyID); !ok {
		return fmt.Errorf("pair action %s names %s, which the pair contract does not list", a.ActionID, a.StrategyID)
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin pair reservation: %w", err)
	}
	defer tx.Rollback()
	var savedFP string
	if err := tx.QueryRow(`SELECT contract_fingerprint FROM basket_contracts WHERE scope = ? AND source = ? AND basket_id = ?`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID).Scan(&savedFP); err != nil {
		return fmt.Errorf("read pair %s contract before reservation: %w", rt.Contract.BasketID, err)
	}
	if savedFP != rt.Contract.Fingerprint() {
		return fmt.Errorf("pair %s saved contract %s differs from the running contract %s", rt.Contract.BasketID, shortFingerprint(savedFP), shortFingerprint(rt.Contract.Fingerprint()))
	}
	if err := checkPendingGroupActionTx(tx, rt, a); err != nil {
		return err
	}
	if use != nil {
		if err := checkPairIncreaseBindingTx(tx, rt, req, use); err != nil {
			return err
		}
	}
	seq, err := nextBasketActionSeqTx(tx, a, sid)
	if err != nil {
		return err
	}
	a.Seq = seq
	a.ActionID = pairActionID(a.Partition, a.BasketID, a.DeadlineMs, a.StrategyID, a.Purpose, a.Seq)
	var dup int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM basket_member_actions WHERE action_id = ? OR (provider = ? AND account_key = ? AND client_order_id = ?)`,
		a.ActionID, a.Provider, a.AccountKey, a.ClientOrderID).Scan(&dup); err != nil {
		return fmt.Errorf("read pair action identity: %w", err)
	}
	if dup > 0 {
		return fmt.Errorf("pair action %s or client order %s is already reserved", a.ActionID, a.ClientOrderID)
	}
	if err := upsertPairActionRow(tx, sid, a, false); err != nil {
		return err
	}
	if intent != nil {
		if intent.OwnerMode != pairGroupMode || intent.DecisionKind != entryDecisionKindQuote || intent.QuoteEventID != a.QuoteEventID || intent.Status != entryIntentReserved {
			return fmt.Errorf("pair increase %s entry intent is not a reserved pair quote-event intent", a.ActionID)
		}
		var unresolved int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM entry_order_intents WHERE scope = ? AND source = ? AND strategy_id = ?
			AND status IN ('reserved','submitting','settling')`, string(intent.Partition.Scope), intent.Partition.Source, sid).Scan(&unresolved); err != nil {
			return fmt.Errorf("read unresolved entry intents for %s: %w", a.StrategyID, err)
		}
		if unresolved > 0 {
			return fmt.Errorf("member %s already has an unresolved entry", a.StrategyID)
		}
		if err := upsertEntryIntentRow(tx, sid, intent, false); err != nil {
			return err
		}
	}
	if use != nil {
		if use.ActionID != a.ActionID {
			use.ActionID = a.ActionID
		}
		if err := insertPairQuoteUseTx(tx, sid, use); err != nil {
			return err
		}
	}
	slotStatus := ""
	if rb := rt.Rebalances[a.DeadlineMs]; rb != nil && pairElectivePurpose(a.Purpose) {
		slotStatus = rb.Status
		if slotStatus == basketRebalanceFrozen {
			slotStatus = basketRebalanceExecuting
		}
		if err := upsertPairSlotRow(tx, rb, slotStatus); err != nil {
			return err
		}
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.durableStatus = a.Status
	if rb := rt.Rebalances[a.DeadlineMs]; rb != nil && slotStatus != "" {
		rb.Status, rb.durableStatus = slotStatus, slotStatus
	}
	if intent != nil {
		intent.durableStatus = intent.Status
	}
	if use != nil {
		use.durableStatus = use.Status
	}
	return nil
}

func checkPairIncreaseBindingTx(tx *sql.Tx, rt *basketRuntime, req pairActionRequest, use *pairQuoteEventUse) error {
	ev := use.Event
	var status, fp, input, evidence, snapshotID string
	var cutoff, generation int64
	err := tx.QueryRow(`SELECT status, contract_fingerprint, input_fingerprint, evidence_json, cutoff_ms, snapshot_id, generation FROM basket_rebalances
		WHERE scope = ? AND source = ? AND basket_id = ? AND deadline_ms = ?`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID, ev.SlotMs).Scan(&status, &fp, &input, &evidence, &cutoff, &snapshotID, &generation)
	if err != nil {
		return fmt.Errorf("read the selected quote event %s slot before reservation: %w", use.EventID, err)
	}
	if fp != rt.Contract.Fingerprint() || fp != ev.ContractFingerprint || input != ev.InputFingerprint || cutoff != ev.CutoffMs || input != req.EvidenceHash {
		return fmt.Errorf("the saved slot for quote event %s no longer matches the event and the request (contract, cutoff or input fingerprint)", use.EventID)
	}
	if pairSHA256([]byte(evidence)) != input {
		return fmt.Errorf("the saved envelope for quote event %s does not match its input fingerprint", use.EventID)
	}
	env, err := pairCheckEnvelope(pairEnvelopeExpectFor(rt), []byte(evidence))
	if err != nil {
		return fmt.Errorf("the saved envelope for quote event %s fails verification: %w", use.EventID, err)
	}
	if env.SlotMs != ev.SlotMs || env.CutoffMs != ev.CutoffMs || env.SnapshotID != snapshotID || uint64(generation) != env.Generation {
		return fmt.Errorf("the saved envelope for quote event %s disagrees with its slot row", use.EventID)
	}
	em, ok := env.member(use.StrategyID)
	if !ok {
		return fmt.Errorf("the saved envelope for quote event %s has no member %s", use.EventID, use.StrategyID)
	}
	if em.Capability.EvidenceHash != req.CapabilityHash {
		return fmt.Errorf("the request capability hash is not the selected envelope's %s capability", use.StrategyID)
	}
	want, err := pairCanonicalJSON(em.Conversions)
	if err != nil {
		return err
	}
	got, err := pairCanonicalJSON(req.Conversions)
	if err != nil || !bytes.Equal(want, got) {
		return fmt.Errorf("the request conversions are not the selected envelope's %s conversions", use.StrategyID)
	}
	return nil
}

func (sdb *StateDB) markPairActionSubmittingTx(rt *basketRuntime, a *basketAction, intent *entryIntent, submittedAtMs int64) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	sids, err := sdb.checkBasketOwnership(rt)
	if err != nil {
		return err
	}
	sid := sids[a.StrategyID]
	tx, err := sdb.db.Begin()
	if err != nil {
		return fmt.Errorf("begin pair submitting: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE basket_member_actions SET status = 'submitting', submitted_at_ms = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE action_id = ? AND strategy_id = ? AND group_mode = 'pair' AND status = 'reserved' AND request_json = ? AND client_order_id = ?`,
		submittedAtMs, submittedAtMs, a.ActionID, sid, a.RequestJSON, a.ClientOrderID)
	if err != nil {
		return fmt.Errorf("mark pair action %s submitting: %w", a.ActionID, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("mark pair action %s submitting: %w", a.ActionID, errPairGuardMiss)
	}
	if intent != nil {
		res, err := tx.Exec(`UPDATE entry_order_intents SET status = 'submitting', updated_at_ms = MAX(updated_at_ms, ?)
			WHERE intent_id = ? AND strategy_id = ? AND status = 'reserved' AND owner_mode = 'pair' AND quote_event_id = ? AND client_order_id = ?`,
			submittedAtMs, intent.IntentID, sid, a.QuoteEventID, a.ClientOrderID)
		if err != nil {
			return fmt.Errorf("mark pair entry intent %s submitting: %w", intent.IntentID, err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("mark pair entry intent %s submitting: %w", intent.IntentID, errPairGuardMiss)
		}
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.Status, a.durableStatus, a.SubmittedAtMs = basketActionSubmitting, basketActionSubmitting, submittedAtMs
	if intent != nil {
		intent.Status, intent.durableStatus = entryIntentSubmitting, entryIntentSubmitting
	}
	return nil
}

func pairActionID(p RiskPartition, pairID string, slotMs int64, strategyID, purpose string, seq int) string {
	return "pa-" + sha256Hex([]byte(fmt.Sprintf("%s|%s|%d|%s|%s|%d", p.String(), pairID, slotMs, strategyID, purpose, seq)))[:24]
}

func (sdb *StateDB) freezePairEnvelopeTx(rt *basketRuntime, rb *basketRebalance) error {
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	if rb.CutoffMs <= 0 || rb.InputFP == "" || rb.EvidenceJSON == "" {
		return fmt.Errorf("pair %s slot %d envelope needs a cutoff, an input fingerprint and evidence", rb.BasketID, rb.DeadlineMs)
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE basket_rebalances SET status = 'targets_frozen', cutoff_ms = ?, input_fingerprint = ?, evidence_json = ?,
		    snapshot_id = ?, generation = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE scope = ? AND source = ? AND basket_id = ? AND deadline_ms = ? AND status = 'reserved' AND contract_fingerprint = ?
		  AND targets_json = '' AND input_fingerprint = '' AND evidence_json = ''`,
		rb.CutoffMs, rb.InputFP, rb.EvidenceJSON, rb.SnapshotID, int64(rb.Generation), rb.UpdatedAtMs,
		string(rb.Partition.Scope), rb.Partition.Source, rb.BasketID, rb.DeadlineMs, rt.Contract.Fingerprint())
	if err != nil {
		return fmt.Errorf("freeze pair %s slot %d envelope: %w", rb.BasketID, rb.DeadlineMs, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("freeze pair %s slot %d envelope: %w", rb.BasketID, rb.DeadlineMs, errPairGuardMiss)
	}
	var status, input, evidence string
	var cutoff int64
	if err := tx.QueryRow(`SELECT status, cutoff_ms, input_fingerprint, evidence_json FROM basket_rebalances WHERE scope = ? AND source = ? AND basket_id = ? AND deadline_ms = ?`,
		string(rb.Partition.Scope), rb.Partition.Source, rb.BasketID, rb.DeadlineMs).Scan(&status, &cutoff, &input, &evidence); err != nil {
		return fmt.Errorf("read back pair %s slot %d envelope: %w", rb.BasketID, rb.DeadlineMs, err)
	}
	if status != basketRebalanceFrozen || cutoff != rb.CutoffMs || input != rb.InputFP || evidence != rb.EvidenceJSON {
		return fmt.Errorf("pair %s slot %d envelope changed on write", rb.BasketID, rb.DeadlineMs)
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) recheckPairSlot(rt *basketRuntime, slotMs int64, contractFP string) error {
	var status, fp, savedFP string
	err := sdb.db.QueryRow(`SELECT r.status, r.contract_fingerprint, c.contract_fingerprint FROM basket_rebalances r
		JOIN basket_contracts c ON c.scope = r.scope AND c.source = r.source AND c.basket_id = r.basket_id
		WHERE r.scope = ? AND r.source = ? AND r.basket_id = ? AND r.deadline_ms = ?`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID, slotMs).Scan(&status, &fp, &savedFP)
	if err == sql.ErrNoRows {
		return fmt.Errorf("pair %s slot %d is not reserved", rt.Contract.BasketID, slotMs)
	}
	if err != nil {
		return fmt.Errorf("read pair %s slot %d: %w", rt.Contract.BasketID, slotMs, err)
	}
	if status != basketRebalanceReserved {
		return fmt.Errorf("pair %s slot %d is %s, not reserved", rt.Contract.BasketID, slotMs, status)
	}
	if fp != contractFP || savedFP != contractFP || contractFP != rt.Contract.Fingerprint() {
		return fmt.Errorf("pair %s slot %d contract changed after collection started", rt.Contract.BasketID, slotMs)
	}
	return nil
}

func (sdb *StateDB) writePairDecisionTx(rt *basketRuntime, rb *basketRebalance, targetsJSON, status string) error {
	if sdb.readOnly {
		return fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE basket_rebalances SET targets_json = ?, status = ?, hold_reason = ?, outcome_json = ?, updated_at_ms = MAX(updated_at_ms, ?)
		WHERE scope = ? AND source = ? AND basket_id = ? AND deadline_ms = ? AND status = 'targets_frozen' AND targets_json = ''
		  AND input_fingerprint = ? AND contract_fingerprint = ?`,
		targetsJSON, status, rb.HoldReason, rb.outcomeJSON(), rb.UpdatedAtMs,
		string(rb.Partition.Scope), rb.Partition.Source, rb.BasketID, rb.DeadlineMs, rb.InputFP, rt.Contract.Fingerprint())
	if err != nil {
		return fmt.Errorf("freeze pair %s slot %d decision: %w", rb.BasketID, rb.DeadlineMs, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("freeze pair %s slot %d decision: %w", rb.BasketID, rb.DeadlineMs, errPairGuardMiss)
	}
	var stored string
	if err := tx.QueryRow(`SELECT targets_json FROM basket_rebalances WHERE scope = ? AND source = ? AND basket_id = ? AND deadline_ms = ?`,
		string(rb.Partition.Scope), rb.Partition.Source, rb.BasketID, rb.DeadlineMs).Scan(&stored); err != nil {
		return fmt.Errorf("read back pair %s slot %d decision: %w", rb.BasketID, rb.DeadlineMs, err)
	}
	if stored != targetsJSON {
		return fmt.Errorf("pair %s slot %d decision bytes changed on write", rb.BasketID, rb.DeadlineMs)
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	return tx.Commit()
}

func (sdb *StateDB) loadPairRecordsInto(out *basketFileRecords) error {
	exists, err := tableExists(sdb, "pair_quote_event_uses")
	if err != nil {
		return err
	}
	if exists {
		rows, err := sdb.db.Query(`SELECT scope, source, pair_id, strategy_id, event_id, contract_fingerprint, slot_ms, cutoff_ms, input_fingerprint,
			    action_id, status, created_at_ms, updated_at_ms FROM pair_quote_event_uses ORDER BY scope, source, pair_id, slot_ms, strategy_id`)
		if err != nil {
			return fmt.Errorf("read pair quote event uses in the %s state file: %w", sdb.storageRoleOf(), err)
		}
		for rows.Next() {
			var u pairQuoteEventUse
			var scope, source, sid string
			if err := rows.Scan(&scope, &source, &u.PairID, &sid, &u.EventID, &u.Event.ContractFingerprint, &u.Event.SlotMs, &u.Event.CutoffMs,
				&u.Event.InputFingerprint, &u.ActionID, &u.Status, &u.CreatedAtMs, &u.UpdatedAtMs); err != nil {
				rows.Close()
				return err
			}
			u.Partition = RiskPartition{Scope: PortfolioScope(scope), Source: source}
			u.Event.PairID = u.PairID
			u.durableStatus = u.Status
			procID, mapped := sdb.mapStoredStrategyID(sid)
			if !mapped {
				if u.Status == pairUseReserved {
					out.Unmapped = append(out.Unmapped, fmt.Sprintf("pair %s quote event use %s for stored strategy %q", u.PairID, u.EventID, sid))
				}
				continue
			}
			u.StrategyID = procID
			k := basketKey{Partition: u.Partition, BasketID: u.PairID}
			if out.Uses == nil {
				out.Uses = make(map[basketKey][]*pairQuoteEventUse)
			}
			uu := u
			out.Uses[k] = append(out.Uses[k], &uu)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	has, err := sdb.tableHasColumn("basket_contracts", "progress_json")
	if err != nil || !has {
		return err
	}
	rows, err := sdb.db.Query(`SELECT scope, source, basket_id, progress_json FROM basket_contracts WHERE progress_json != '' ORDER BY scope, source, basket_id`)
	if err != nil {
		return fmt.Errorf("read pair progress in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope, source, id, raw string
		if err := rows.Scan(&scope, &source, &id, &raw); err != nil {
			return err
		}
		if out.Progress == nil {
			out.Progress = make(map[basketKey]string)
		}
		out.Progress[basketKey{Partition: RiskPartition{Scope: PortfolioScope(scope), Source: source}, BasketID: id}] = raw
	}
	return rows.Err()
}

func (r *basketFileRecords) pairContract(k basketKey) bool {
	raw, ok := r.Contracts[k]
	if !ok {
		return false
	}
	c, err := parseBasketContract(raw)
	if err != nil {
		return strings.Contains(raw, `"mode":"pair"`)
	}
	return c.isPair()
}

func (r *basketFileRecords) pairPendingFor(k basketKey) []string {
	out := r.pendingFor(k)
	for _, u := range r.Uses[k] {
		if u.Status == pairUseReserved {
			out = append(out, fmt.Sprintf("quote event %s for %s is reserved by action %s", u.EventID, u.StrategyID, u.ActionID))
		}
	}
	if raw, ok := r.Progress[k]; ok {
		if p, err := parsePairProgress(raw); err != nil {
			out = append(out, err.Error())
		} else {
			if p.Lifecycle != pairLifecycleFlat {
				out = append(out, fmt.Sprintf("pair lifecycle is %s", p.Lifecycle))
			}
			for _, o := range p.Obligations {
				out = append(out, fmt.Sprintf("open %s obligation: %s", o.Kind, o.Detail))
			}
		}
	}
	return out
}

func pairFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func (st *StateStore) ReservePairAction(rt *basketRuntime, a *basketAction, intent *entryIntent, use *pairQuoteEventUse) error {
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	return db.reservePairActionTx(rt, a, intent, use)
}

func (st *StateStore) MarkPairActionSubmitting(rt *basketRuntime, a *basketAction, intent *entryIntent) error {
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	return db.markPairActionSubmittingTx(rt, a, intent, rt.auditNowMs())
}

func (st *StateStore) FreezePairEnvelope(rt *basketRuntime, rb *basketRebalance) error {
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	return db.freezePairEnvelopeTx(rt, rb)
}

func (st *StateStore) RecheckPairSlot(rt *basketRuntime, slotMs int64, contractFP string) error {
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	return db.recheckPairSlot(rt, slotMs, contractFP)
}

func (st *StateStore) WritePairDecision(rt *basketRuntime, rb *basketRebalance, targetsJSON, status string) error {
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	return db.writePairDecisionTx(rt, rb, targetsJSON, status)
}

func (rt *basketRuntime) prunePair() {
	latest := rt.latest()
	origin := int64(0)
	if rt.Pair != nil {
		origin = rt.Pair.Progress.OriginSlotMs
	}
	for d, rb := range rt.Rebalances {
		if rb == latest || d == origin || !basketRebalanceTerminal(rb.Status) || rb.durableStatus != rb.Status {
			continue
		}
		keep := false
		for _, a := range rt.Actions {
			if a.DeadlineMs == d && (basketActionPending(a.Status) || a.durableStatus != a.Status || a.pendingFee != nil) {
				keep = true
			}
		}
		if keep {
			continue
		}
		delete(rt.Rebalances, d)
		for id, a := range rt.Actions {
			if a.DeadlineMs == d {
				delete(rt.Actions, id)
			}
		}
	}
	for id, a := range rt.Actions {
		if basketActionPending(a.Status) || a.durableStatus != a.Status || a.pendingFee != nil {
			continue
		}
		if _, ok := rt.Rebalances[a.DeadlineMs]; !ok {
			delete(rt.Actions, id)
		}
	}
	if rt.Pair == nil {
		return
	}
	for k, u := range rt.Pair.Uses {
		if u.Status == pairUseReserved || u.durableStatus != u.Status {
			continue
		}
		if _, ok := rt.Actions[u.ActionID]; ok {
			continue
		}
		if _, ok := rt.Rebalances[u.Event.SlotMs]; ok {
			continue
		}
		delete(rt.Pair.Uses, k)
	}
}

func (st *StateStore) loadPairRuntimes(state *AppState, cfg *Config, records map[storageRole]*basketFileRecords, configured map[basketKey]storageRole, out map[string]*basketRuntime) error {
	adm := cfg.pairAdmission
	p, limits := adm.pair, adm.limits
	contract, err := buildPairContract(cfg, p, limits)
	if err != nil {
		return err
	}
	part, err := parseRiskPartition(contract.Partition)
	if err != nil {
		return err
	}
	rt := newBasketRuntime(BasketConfig{ID: p.ID}, contract, part)
	rt.Pair = newPairRuntimeState(p, limits, adm)
	db, err := st.basketFile(rt)
	if err != nil {
		return err
	}
	role := db.storageRoleOf()
	k := basketKey{Partition: part, BasketID: p.ID}
	configured[k] = role
	progressSaved := false
	for otherRole, r := range records {
		if otherRole == role {
			continue
		}
		if _, ok := r.Contracts[k]; ok {
			if pending := r.pairPendingFor(k); len(pending) > 0 {
				return fmt.Errorf("pair %s recovery evidence sits in the %s state file, but its members are owned by the %s file: %s", p.ID, otherRole, role, strings.Join(pending, "; "))
			}
		}
	}
	if r := records[role]; r != nil {
		if raw, ok := r.Contracts[k]; ok {
			saved, perr := parseBasketContract(raw)
			if perr != nil {
				return fmt.Errorf("pair %s: %w", p.ID, perr)
			}
			if !saved.isPair() {
				return fmt.Errorf("pair %s: the saved group contract is a basket contract; a basket id never becomes a pair", p.ID)
			}
			rt.SavedContract = &saved
			rt.SavedFP = saved.Fingerprint()
		}
		for _, rb := range r.Rebalances[k] {
			rt.Rebalances[rb.DeadlineMs] = rb
		}
		for _, a := range r.Actions[k] {
			if !a.pair() {
				return fmt.Errorf("pair %s holds basket action %s; mixed basket and pair records are refused", p.ID, a.ActionID)
			}
			rt.Actions[a.ActionID] = a
		}
		for id, q := range r.Expected[k] {
			rt.Expected[id] = q
			rt.expectedDurable[id] = q
		}
		for _, u := range r.Uses[k] {
			rt.Pair.Uses[u.key()] = u
		}
		if raw, ok := r.Progress[k]; ok {
			prog, perr := parsePairProgress(raw)
			if perr != nil {
				return fmt.Errorf("pair %s: %w", p.ID, perr)
			}
			rt.Pair.Progress = prog
			rt.Pair.durableSeq = prog.Seq
			progressSaved = true
		}
	}
	if _, err := db.checkBasketOwnership(rt); err != nil {
		return err
	}
	saved := progressSaved || pairRunHasSavedState(state, rt, p.ID)
	if err := bindPairCostEvidence(rt, adm, saved); err != nil {
		return err
	}
	for _, m := range contract.Members {
		if s := state.Strategies[m.StrategyID]; s != nil {
			s.Basket = rt
			s.pairCostPair = p.ID
			if s.EntryGuard != nil {
				for id, in := range s.EntryGuard.Intents {
					if in.OwnerMode == pairGroupMode && in.OwnerGroupID == p.ID {
						rt.Pair.Intents[id] = in
					}
				}
			}
		}
	}
	out[p.ID] = rt
	return nil
}

func pairRunHasSavedState(state *AppState, rt *basketRuntime, pairID string) bool {
	if rt.SavedContract != nil || len(rt.Rebalances) > 0 || len(rt.Actions) > 0 || len(rt.Expected) > 0 || len(rt.Pair.Uses) > 0 {
		return true
	}
	for _, m := range rt.Contract.Members {
		s := state.Strategies[m.StrategyID]
		if s == nil {
			continue
		}
		if len(s.TradeHistory) > 0 || len(s.Positions) > 0 {
			return true
		}
		if s.EntryGuard != nil {
			for _, in := range s.EntryGuard.Intents {
				if in.OwnerMode == pairGroupMode && in.OwnerGroupID == pairID {
					return true
				}
			}
		}
	}
	return false
}

func bindPairCostEvidence(rt *basketRuntime, adm *pairResearchAdmission, saved bool) error {
	id := rt.Contract.BasketID
	want, err := adm.costEvidence()
	if err != nil {
		return fmt.Errorf("pair %s: %w", id, err)
	}
	if ev := rt.Pair.Progress.CostEvidence; len(ev) > 0 {
		if err := adm.verifyCostEvidence(ev); err != nil {
			return fmt.Errorf("pair %s resume refused: %w; the saved state is kept unchanged, and a changed pair identity, cost rate or execution model needs a new research run", id, err)
		}
		adm.markCostDurable(ev)
		return nil
	}
	if saved {
		return fmt.Errorf("pair %s resume refused: the saved run has pair state but no member cost bindings; a pre-binding pair run needs a new research run, and its history is never stamped or converted", id)
	}
	rt.Pair.Progress.CostEvidence = want
	return nil
}

type pairFeeFixError struct {
	ActionID string
	Err      error
}

func (e *pairFeeFixError) Error() string {
	return fmt.Sprintf("pair action %s fee correction: %v", e.ActionID, e.Err)
}

func (e *pairFeeFixError) Unwrap() error { return e.Err }

type pairTradeStamp struct {
	RowID        int64
	ModelVersion int
	PolicyID     string
}

func (st *StateStore) PairTradeStamp(rt *basketRuntime, strategyID, ts, symbol, orderID string) (pairTradeStamp, error) {
	db, err := st.basketFile(rt)
	if err != nil {
		return pairTradeStamp{}, err
	}
	sids, err := db.checkBasketOwnership(rt)
	if err != nil {
		return pairTradeStamp{}, err
	}
	sid, ok := sids[strategyID]
	if !ok {
		return pairTradeStamp{}, fmt.Errorf("%w: pair %s has no member %s", errPairFeeFixProvenance, rt.Contract.BasketID, strategyID)
	}
	rows, err := db.db.Query(`SELECT rowid, cost_model_version, cost_policy_id FROM trades WHERE strategy_id = ? AND timestamp = ? AND symbol = ? AND exchange_order_id = ?`, sid, ts, symbol, orderID)
	if err != nil {
		return pairTradeStamp{}, err
	}
	defer rows.Close()
	var found []pairTradeStamp
	for rows.Next() {
		var t pairTradeStamp
		if err := rows.Scan(&t.RowID, &t.ModelVersion, &t.PolicyID); err != nil {
			return pairTradeStamp{}, err
		}
		found = append(found, t)
	}
	if err := rows.Err(); err != nil {
		return pairTradeStamp{}, err
	}
	if len(found) != 1 {
		return pairTradeStamp{}, fmt.Errorf("%w: the booked trade row %s %s at %s matched %d saved rows, want exactly 1", errPairFeeFixProvenance, symbol, orderID, ts, len(found))
	}
	return found[0], nil
}
