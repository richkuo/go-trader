package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

const (
	quoteOwnerKindManual = "manual"
	quoteOwnerKindQuote  = "quote"

	quoteSubmitReserved   = "reserved"
	quoteSubmitSubmitting = "submitting"
	quoteSubmitResting    = "resting"
	quoteSubmitUnknown    = "unknown"
	quoteSubmitRejected   = "rejected"
	quoteSubmitNeverSent  = "never_sent"
	quoteSubmitFilled     = "filled"
	quoteSubmitCancelled  = "cancelled"
	quoteSubmitTriggered  = "triggered"

	quoteCancelNone      = ""
	quoteCancelRequested = "requested"
	quoteCancelSent      = "sent"
	quoteCancelUnknown   = "unknown"
	quoteCancelConfirmed = "confirmed"

	quoteGenReserved = "reserved"
	quoteGenActive   = "active"
	quoteGenResolved = "resolved"

	quoteSettleFills   = "fills"
	quoteSettleFunding = "funding"
	quoteSettleBoth    = "both"
	quoteSettleNone    = "none"
)

var errQuoteRevisionStale = errors.New("the durable quote book revision changed")

var quotePendingLimitColumnDDL = []string{
	"ALTER TABLE pending_limit_orders ADD COLUMN owner_kind TEXT NOT NULL DEFAULT 'manual'",
	"ALTER TABLE pending_limit_orders ADD COLUMN partition_scope TEXT NOT NULL DEFAULT 'live'",
	"ALTER TABLE pending_limit_orders ADD COLUMN partition_source TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN quote_generation INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE pending_limit_orders ADD COLUMN quote_role TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN provider TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN account_key TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN venue_namespace TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN request_json TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN request_expiry_ms INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE pending_limit_orders ADD COLUMN client_order_id TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN venue_order_id TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN submit_state TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN cancel_state TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN confirmed_qty TEXT NOT NULL DEFAULT '0'",
	"ALTER TABLE pending_limit_orders ADD COLUMN applied_qty TEXT NOT NULL DEFAULT '0'",
	"ALTER TABLE pending_limit_orders ADD COLUMN accepted_at_ms INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE pending_limit_orders ADD COLUMN terminal_at_ms INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE pending_limit_orders ADD COLUMN outcome_json TEXT NOT NULL DEFAULT ''",
	"ALTER TABLE pending_limit_orders ADD COLUMN updated_at_ms INTEGER NOT NULL DEFAULT 0",
}

const quoteSchemaDDL = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_pending_limit_quote_client
    ON pending_limit_orders(provider, account_key, client_order_id) WHERE owner_kind = 'quote';
CREATE UNIQUE INDEX IF NOT EXISTS idx_pending_limit_quote_role
    ON pending_limit_orders(partition_scope, partition_source, strategy_id, quote_role, quote_generation) WHERE owner_kind = 'quote';
CREATE INDEX IF NOT EXISTS idx_pending_limit_owner_kind ON pending_limit_orders(owner_kind);

CREATE TRIGGER IF NOT EXISTS pending_limit_orders_owner_kind_insert
BEFORE INSERT ON pending_limit_orders
WHEN NEW.owner_kind NOT IN ('manual','quote')
  OR (NEW.owner_kind = 'quote' AND (NEW.quote_role NOT IN ('bid','ask','stop','close') OR NEW.client_order_id = '' OR NEW.quote_generation < 1 OR NEW.partition_scope NOT IN ('live','paper')))
BEGIN SELECT RAISE(ABORT, 'pending_limit_orders row has an invalid owner kind or quote identity'); END;

CREATE TRIGGER IF NOT EXISTS pending_limit_orders_owner_kind_update
BEFORE UPDATE OF owner_kind, partition_scope, partition_source, strategy_id, quote_role, quote_generation, client_order_id, request_json, provider, account_key ON pending_limit_orders
WHEN OLD.owner_kind = 'quote' OR NEW.owner_kind != OLD.owner_kind
BEGIN SELECT RAISE(ABORT, 'a quote order identity and frozen request are immutable'); END;

CREATE TRIGGER IF NOT EXISTS pending_limit_orders_quote_retained
BEFORE DELETE ON pending_limit_orders
WHEN OLD.owner_kind = 'quote'
BEGIN SELECT RAISE(ABORT, 'quote order evidence is retained'); END;

CREATE TABLE IF NOT EXISTS quote_contracts (
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
CREATE UNIQUE INDEX IF NOT EXISTS idx_quote_contracts_namespace
    ON quote_contracts(provider, account_key, namespace, coin);

CREATE TABLE IF NOT EXISTS quote_generations (
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

CREATE TABLE IF NOT EXISTS quote_fills (
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
CREATE INDEX IF NOT EXISTS idx_quote_fills_owner ON quote_fills(scope, source, strategy_id, settlement_id);

CREATE TRIGGER IF NOT EXISTS quote_fills_immutable
BEFORE UPDATE ON quote_fills
WHEN NEW.digest != OLD.digest OR NEW.side != OLD.side OR NEW.px != OLD.px OR NEW.size != OLD.size OR NEW.fee != OLD.fee
  OR NEW.fee_asset != OLD.fee_asset OR NEW.raw_json != OLD.raw_json OR NEW.strategy_id != OLD.strategy_id
  OR NEW.scope != OLD.scope OR NEW.source != OLD.source OR NEW.kind != OLD.kind
  OR (OLD.settlement_id != 0 AND NEW.settlement_id != OLD.settlement_id)
BEGIN SELECT RAISE(ABORT, 'quote fill evidence is immutable'); END;

CREATE TRIGGER IF NOT EXISTS quote_fills_retained
BEFORE DELETE ON quote_fills
BEGIN SELECT RAISE(ABORT, 'quote fill evidence is retained'); END;

CREATE TABLE IF NOT EXISTS quote_settlements (
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

CREATE TRIGGER IF NOT EXISTS quote_settlements_retained
BEFORE DELETE ON quote_settlements
BEGIN SELECT RAISE(ABORT, 'quote settlements are retained'); END;

CREATE TRIGGER IF NOT EXISTS quote_settlements_immutable
BEFORE UPDATE ON quote_settlements
BEGIN SELECT RAISE(ABORT, 'quote settlements are immutable'); END;

CREATE TABLE IF NOT EXISTS quote_funding_evidence (
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

CREATE TRIGGER IF NOT EXISTS quote_funding_evidence_immutable
BEFORE UPDATE ON quote_funding_evidence
WHEN NEW.digest != OLD.digest OR NEW.raw_json != OLD.raw_json OR NEW.strategy_id != OLD.strategy_id
  OR (OLD.settlement_id != 0 AND NEW.settlement_id != OLD.settlement_id)
BEGIN SELECT RAISE(ABORT, 'quote funding evidence is immutable'); END;

CREATE TRIGGER IF NOT EXISTS quote_funding_evidence_retained
BEFORE DELETE ON quote_funding_evidence
BEGIN SELECT RAISE(ABORT, 'quote funding evidence is retained'); END;
`

func (sdb *StateDB) migrateQuoteSchema() error {
	for _, ddl := range quotePendingLimitColumnDDL {
		if _, err := sdb.db.Exec(ddl); err != nil {
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("migrate pending_limit_orders for quote ownership: %w", err)
		}
	}
	if _, err := sdb.db.Exec(quoteSchemaDDL); err != nil {
		return fmt.Errorf("create quote tables: %w", err)
	}
	return nil
}

func (sdb *StateDB) manualPendingFilter(prefix string) (string, error) {
	has, err := sdb.tableHasColumn("pending_limit_orders", "owner_kind")
	if err != nil {
		return "", err
	}
	if !has {
		return "", nil
	}
	return " " + prefix + " owner_kind = 'manual'", nil
}

type quoteOwnerKey struct {
	Partition  RiskPartition
	StrategyID string
	sid        string
}

func (k quoteOwnerKey) label() string {
	return fmt.Sprintf("%s/%s", partitionLabel(k.Partition), k.StrategyID)
}

type quoteExactBook struct {
	Qty           string `json:"qty"`
	Cost          string `json:"cost"`
	Cash          string `json:"cash"`
	PositionID    string `json:"position_id,omitempty"`
	FirstOpenMs   int64  `json:"first_open_ms,omitempty"`
	RiskAnchorPx  string `json:"risk_anchor_px,omitempty"`
	InitialQty    string `json:"initial_qty,omitempty"`
	RealizedGross string `json:"realized_gross"`
	Fees          string `json:"fees"`
	Funding       string `json:"funding"`
}

func (b quoteExactBook) qty() *big.Rat    { return mustQuoteRat(b.Qty) }
func (b quoteExactBook) cost() *big.Rat   { return mustQuoteRat(b.Cost) }
func (b quoteExactBook) cash() *big.Rat   { return mustQuoteRat(b.Cash) }
func (b quoteExactBook) anchor() *big.Rat { return mustQuoteRat(b.RiskAnchorPx) }

func (b quoteExactBook) validate() error {
	for name, v := range map[string]string{"qty": b.Qty, "cost": b.Cost, "cash": b.Cash, "realized_gross": b.RealizedGross, "fees": b.Fees, "funding": b.Funding, "risk_anchor_px": b.RiskAnchorPx, "initial_qty": b.InitialQty} {
		if _, err := parseQuoteRat(v); err != nil {
			return fmt.Errorf("quote book %s: %w", name, err)
		}
	}
	q := mustQuoteRat(b.Qty)
	if q.Sign() == 0 && (mustQuoteRat(b.Cost).Sign() != 0 || b.PositionID != "") {
		return fmt.Errorf("a flat quote book carries cost or a position identity")
	}
	if q.Sign() != 0 && (b.PositionID == "" || mustQuoteRat(b.Cost).Sign() <= 0) {
		return fmt.Errorf("an open quote book lacks a position identity or cost basis")
	}
	return nil
}

func (b quoteExactBook) JSON() string {
	blob, _ := json.Marshal(b)
	return string(blob)
}

func parseQuoteExactBook(raw string) (quoteExactBook, error) {
	var b quoteExactBook
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, fmt.Errorf("quote book is unreadable: %w", err)
	}
	return b, b.validate()
}

type quoteContractRow struct {
	Partition          RiskPartition
	StorageID          string
	Coin               string
	Provider           string
	AccountKey         string
	Namespace          string
	PolicyFP           string
	ConfigFP           string
	StartMs            int64
	Revision           int64
	LastDeadlineMs     int64
	AppliedBoundaryMs  int64
	LastGeneration     int64
	LastStopSeq        int64
	LastCloseSeq       int64
	Book               quoteExactBook
	OperatorHold       bool
	OperatorHoldReason string
	UpdatedAtMs        int64
}

const quoteContractColumns = `scope, source, strategy_id, coin, provider, account_key, namespace, policy_fingerprint, config_fingerprint,
    start_ms, revision, last_deadline_ms, applied_boundary_ms, last_generation, last_stop_seq, last_close_seq, book_json,
    operator_hold, operator_hold_reason, updated_at_ms`

type sqlRowScanner interface {
	Scan(dest ...interface{}) error
}

func scanQuoteContract(row sqlRowScanner) (*quoteContractRow, error) {
	var c quoteContractRow
	var scope, source, book string
	var hold int
	if err := row.Scan(&scope, &source, &c.StorageID, &c.Coin, &c.Provider, &c.AccountKey, &c.Namespace, &c.PolicyFP, &c.ConfigFP,
		&c.StartMs, &c.Revision, &c.LastDeadlineMs, &c.AppliedBoundaryMs, &c.LastGeneration, &c.LastStopSeq, &c.LastCloseSeq, &book,
		&hold, &c.OperatorHoldReason, &c.UpdatedAtMs); err != nil {
		return nil, err
	}
	c.Partition = RiskPartition{Scope: PortfolioScope(scope), Source: source}
	b, err := parseQuoteExactBook(book)
	if err != nil {
		return nil, fmt.Errorf("quote contract %s/%s: %w", partitionLabel(c.Partition), c.StorageID, err)
	}
	c.Book = b
	c.OperatorHold = hold != 0
	return &c, nil
}

type sqlQueryRower interface {
	QueryRow(query string, args ...interface{}) *sql.Row
}

func readQuoteContractTx(q sqlQueryRower, k quoteOwnerKey) (*quoteContractRow, error) {
	row := q.QueryRow(`SELECT `+quoteContractColumns+` FROM quote_contracts WHERE scope = ? AND source = ? AND strategy_id = ?`,
		string(k.Partition.Scope), k.Partition.Source, k.sid)
	c, err := scanQuoteContract(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

type quoteOrderRow struct {
	RowID           int64
	SourceRole      storageRole
	Partition       RiskPartition
	StrategyID      string
	storageID       string
	Coin            string
	Side            string
	Role            string
	Generation      int64
	Provider        string
	AccountKey      string
	Namespace       string
	Request         quoteOrderRequest
	RequestJSON     string
	RequestExpiryMs int64
	ClientOrderID   string
	VenueOrderID    string
	SubmitState     string
	CancelState     string
	ConfirmedQty    string
	AppliedQty      string
	AcceptedAtMs    int64
	TerminalAtMs    int64
	OutcomeJSON     string
	CreatedAtMs     int64
	UpdatedAtMs     int64
}

func quoteSubmitTerminal(state string) bool {
	switch state {
	case quoteSubmitRejected, quoteSubmitNeverSent, quoteSubmitFilled, quoteSubmitCancelled:
		return true
	}
	return false
}

func (r *quoteOrderRow) terminal() bool { return quoteSubmitTerminal(r.SubmitState) }

func (r *quoteOrderRow) settled() bool {
	c, err1 := parseQuoteRat(r.ConfirmedQty)
	a, err2 := parseQuoteRat(r.AppliedQty)
	return err1 == nil && err2 == nil && c.Cmp(a) == 0
}

func (r *quoteOrderRow) resolved() bool { return r.terminal() && r.settled() }

func (r *quoteOrderRow) mayRest() bool {
	switch r.SubmitState {
	case quoteSubmitSubmitting, quoteSubmitResting, quoteSubmitUnknown, quoteSubmitTriggered:
		return true
	}
	return false
}

func (r *quoteOrderRow) increasesRisk() bool {
	return (r.Role == quoteRoleBid || r.Role == quoteRoleAsk) && !r.Request.ReduceOnly
}

func (r *quoteOrderRow) remainingQty() *big.Rat {
	sz, err := parseQuoteDecimal(r.Request.Size)
	if err != nil {
		return quoteRatZero()
	}
	c, err := parseQuoteRat(r.ConfirmedQty)
	if err != nil {
		return sz
	}
	rem := quoteRatSub(sz, c)
	if rem.Sign() < 0 {
		return quoteRatZero()
	}
	return rem
}

func (r *quoteOrderRow) ref() quoteRowRef {
	return quoteRowRef{SourceRole: r.SourceRole, Partition: r.Partition, StrategyID: r.StrategyID, RowID: r.RowID}
}

type quoteRowRef struct {
	SourceRole storageRole   `json:"source_role"`
	Partition  RiskPartition `json:"-"`
	StrategyID string        `json:"strategy_id"`
	RowID      int64         `json:"row_id"`
}

const quoteOrderColumns = `id, strategy_id, symbol, side, created_at, partition_scope, partition_source, quote_generation, quote_role,
    provider, account_key, venue_namespace, request_json, request_expiry_ms, client_order_id, venue_order_id, submit_state,
    cancel_state, confirmed_qty, applied_qty, accepted_at_ms, terminal_at_ms, outcome_json, updated_at_ms`

func scanQuoteOrder(row sqlRowScanner) (*quoteOrderRow, error) {
	var o quoteOrderRow
	var scope, source, created string
	if err := row.Scan(&o.RowID, &o.storageID, &o.Coin, &o.Side, &created, &scope, &source, &o.Generation, &o.Role,
		&o.Provider, &o.AccountKey, &o.Namespace, &o.RequestJSON, &o.RequestExpiryMs, &o.ClientOrderID, &o.VenueOrderID, &o.SubmitState,
		&o.CancelState, &o.ConfirmedQty, &o.AppliedQty, &o.AcceptedAtMs, &o.TerminalAtMs, &o.OutcomeJSON, &o.UpdatedAtMs); err != nil {
		return nil, err
	}
	o.Partition = RiskPartition{Scope: PortfolioScope(scope), Source: source}
	o.CreatedAtMs = parseTime(created).UnixMilli()
	req, err := parseQuoteOrderRequest(o.RequestJSON)
	if err != nil {
		return nil, fmt.Errorf("quote order row %d: %w", o.RowID, err)
	}
	o.Request = req
	return &o, nil
}

type quoteGeneration struct {
	Generation      int64  `json:"generation"`
	DeadlineMs      int64  `json:"deadline_ms"`
	Status          string `json:"status"`
	DesiredJSON     string `json:"-"`
	ReservationJSON string `json:"-"`
	TerminalJSON    string `json:"-"`
	HoldReason      string `json:"hold_reason,omitempty"`
	CreatedAtMs     int64  `json:"created_at_ms"`
	UpdatedAtMs     int64  `json:"updated_at_ms"`
}

type quoteFillRow struct {
	ID           int64
	Key          string
	Fill         quoteRawFill
	Kind         string
	CorrectsID   int64
	Role         string
	OrderRowID   int64
	Digest       string
	SettlementID int64
	ObservedAtMs int64
}

type quoteFundingEvidenceRow struct {
	DedupID      string
	Coin         string
	EventMs      int64
	RawJSON      string
	Digest       string
	SettlementID int64
}

type quoteDurableOwner struct {
	Contract    *quoteContractRow
	Orders      []*quoteOrderRow
	Generations []*quoteGeneration
	Unapplied   []*quoteFillRow
	Funding     []*quoteFundingEvidenceRow
	Settlements int
}

func (sdb *StateDB) quoteOwnerKey(procID string, p RiskPartition) (quoteOwnerKey, error) {
	sid, err := sdb.entryOwner(procID, p)
	if err != nil {
		return quoteOwnerKey{}, err
	}
	return quoteOwnerKey{Partition: p, StrategyID: procID, sid: sid}, nil
}

func (sdb *StateDB) loadQuoteDurableOwner(k quoteOwnerKey) (*quoteDurableOwner, error) {
	out := &quoteDurableOwner{}
	exists, err := tableExists(sdb, "quote_contracts")
	if err != nil || !exists {
		return out, err
	}
	c, err := readQuoteContractTx(sdb.db, k)
	if err != nil {
		return nil, err
	}
	out.Contract = c
	rows, err := sdb.db.Query(`SELECT `+quoteOrderColumns+` FROM pending_limit_orders
		WHERE owner_kind = 'quote' AND partition_scope = ? AND partition_source = ? AND strategy_id = ?
		ORDER BY quote_role, quote_generation, id`, string(k.Partition.Scope), k.Partition.Source, k.sid)
	if err != nil {
		return nil, fmt.Errorf("read quote orders for %s: %w", k.label(), err)
	}
	for rows.Next() {
		o, err := scanQuoteOrder(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		o.SourceRole = sdb.storageRoleOf()
		o.StrategyID = k.StrategyID
		out.Orders = append(out.Orders, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	gens, err := sdb.db.Query(`SELECT generation, deadline_ms, status, desired_json, reservation_json, terminal_json, hold_reason, created_at_ms, updated_at_ms
		FROM quote_generations WHERE scope = ? AND source = ? AND strategy_id = ? ORDER BY generation`, string(k.Partition.Scope), k.Partition.Source, k.sid)
	if err != nil {
		return nil, fmt.Errorf("read quote generations for %s: %w", k.label(), err)
	}
	for gens.Next() {
		var g quoteGeneration
		if err := gens.Scan(&g.Generation, &g.DeadlineMs, &g.Status, &g.DesiredJSON, &g.ReservationJSON, &g.TerminalJSON, &g.HoldReason, &g.CreatedAtMs, &g.UpdatedAtMs); err != nil {
			gens.Close()
			return nil, err
		}
		out.Generations = append(out.Generations, &g)
	}
	gens.Close()
	if err := gens.Err(); err != nil {
		return nil, err
	}
	fills, err := sdb.loadQuoteFillRows(sdb.db, k, true)
	if err != nil {
		return nil, err
	}
	out.Unapplied = fills
	fund, err := sdb.db.Query(`SELECT dedup_id, coin, event_ms, raw_json, digest, settlement_id FROM quote_funding_evidence
		WHERE scope = ? AND source = ? AND strategy_id = ? AND settlement_id = 0 ORDER BY event_ms, dedup_id`, string(k.Partition.Scope), k.Partition.Source, k.sid)
	if err != nil {
		return nil, fmt.Errorf("read quote funding evidence for %s: %w", k.label(), err)
	}
	for fund.Next() {
		var f quoteFundingEvidenceRow
		if err := fund.Scan(&f.DedupID, &f.Coin, &f.EventMs, &f.RawJSON, &f.Digest, &f.SettlementID); err != nil {
			fund.Close()
			return nil, err
		}
		out.Funding = append(out.Funding, &f)
	}
	fund.Close()
	if err := fund.Err(); err != nil {
		return nil, err
	}
	if err := sdb.db.QueryRow(`SELECT COUNT(*) FROM quote_settlements WHERE scope = ? AND source = ? AND strategy_id = ?`,
		string(k.Partition.Scope), k.Partition.Source, k.sid).Scan(&out.Settlements); err != nil {
		return nil, err
	}
	return out, nil
}

type sqlQueryer interface {
	Query(query string, args ...interface{}) (*sql.Rows, error)
}

const quoteFillColumns = `id, provider, account_key, namespace, coin, order_id, event_time_ms, trade_id, kind, corrects_fill_id, role,
    client_order_id, order_row_id, side, px, size, fee, fee_asset, start_position, raw_json, digest, settlement_id, observed_at_ms`

func scanQuoteFill(row sqlRowScanner) (*quoteFillRow, error) {
	var f quoteFillRow
	var provider string
	if err := row.Scan(&f.ID, &provider, &f.Fill.Account, &f.Fill.Namespace, &f.Fill.Coin, &f.Fill.OrderID, &f.Fill.TimeMs, &f.Fill.TradeID,
		&f.Kind, &f.CorrectsID, &f.Role, &f.Fill.ClientOrderID, &f.OrderRowID, &f.Fill.Side, &f.Fill.Px, &f.Fill.Size, &f.Fill.Fee,
		&f.Fill.FeeAsset, &f.Fill.StartPosition, &f.Fill.Raw, &f.Digest, &f.SettlementID, &f.ObservedAtMs); err != nil {
		return nil, err
	}
	if f.Kind != "fill" {
		f.Fill.Kind = f.Kind
	}
	f.Key = f.Fill.key()
	return &f, nil
}

func (sdb *StateDB) loadQuoteFillRows(q sqlQueryer, k quoteOwnerKey, unappliedOnly bool) ([]*quoteFillRow, error) {
	where := ""
	if unappliedOnly {
		where = " AND settlement_id = 0"
	}
	rows, err := q.Query(`SELECT `+quoteFillColumns+` FROM quote_fills WHERE scope = ? AND source = ? AND strategy_id = ?`+where+`
		ORDER BY event_time_ms, id`, string(k.Partition.Scope), k.Partition.Source, k.sid)
	if err != nil {
		return nil, fmt.Errorf("read quote fills for %s: %w", k.label(), err)
	}
	defer rows.Close()
	var out []*quoteFillRow
	for rows.Next() {
		f, err := scanQuoteFill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (sdb *StateDB) beginQuoteTx() (*sql.Tx, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	if sdb.readOnly {
		return nil, fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	return sdb.db.Begin()
}

func (sdb *StateDB) commitQuoteTx(tx *sql.Tx) error {
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	return tx.Commit()
}

func checkQuoteRevisionTx(tx *sql.Tx, k quoteOwnerKey, expect int64) (*quoteContractRow, error) {
	c, err := readQuoteContractTx(tx, k)
	if err != nil {
		return nil, err
	}
	if c == nil {
		if expect != 0 {
			return nil, fmt.Errorf("%w: quote owner %s has no durable contract", errQuoteRevisionStale, k.label())
		}
		return nil, nil
	}
	if c.Revision != expect {
		return nil, fmt.Errorf("%w: quote owner %s is at revision %d, memory holds %d", errQuoteRevisionStale, k.label(), c.Revision, expect)
	}
	return c, nil
}

func (sdb *StateDB) createQuoteContract(k quoteOwnerKey, c *quoteContractRow, staged *StrategyState, scope PortfolioScope) ([]int, []TradeDiagnosticsRow, error) {
	tx, err := sdb.beginQuoteTx()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	existing, err := readQuoteContractTx(tx, k)
	if err != nil {
		return nil, nil, err
	}
	if existing != nil {
		return nil, nil, fmt.Errorf("quote owner %s already has a durable contract", k.label())
	}
	var foreign int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pending_limit_orders WHERE owner_kind = 'manual' AND UPPER(symbol) = ?`, strings.ToUpper(c.Coin)).Scan(&foreign); err != nil {
		return nil, nil, err
	}
	if foreign > 0 {
		return nil, nil, fmt.Errorf("the %s state file holds %d unrelated pending %s order(s); quote ownership is exclusive", sdb.storageRoleOf(), foreign, c.Coin)
	}
	if _, err := tx.Exec(`INSERT INTO app_state (id, cycle_count, last_cycle, last_leaderboard_post_date, last_leaderboard_summaries, last_summary_post)
		VALUES (1, 0, '', '', '', '') ON CONFLICT(id) DO NOTHING`); err != nil {
		return nil, nil, fmt.Errorf("ensure app_state: %w", err)
	}
	flushed, diags, err := sdb.writeOneStrategyBookTx(tx, staged, k.sid, scope)
	if err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(`INSERT INTO quote_contracts (`+quoteContractColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(k.Partition.Scope), k.Partition.Source, k.sid, c.Coin, c.Provider, c.AccountKey, c.Namespace, c.PolicyFP, c.ConfigFP,
		c.StartMs, c.Revision, c.LastDeadlineMs, c.AppliedBoundaryMs, c.LastGeneration, c.LastStopSeq, c.LastCloseSeq, c.Book.JSON(),
		boolToInt(c.OperatorHold), c.OperatorHoldReason, c.UpdatedAtMs); err != nil {
		return nil, nil, fmt.Errorf("create quote contract for %s: %w", k.label(), err)
	}
	if err := sdb.commitQuoteTx(tx); err != nil {
		return nil, nil, err
	}
	return flushed, diags, nil
}

func insertQuoteOrderRowTx(tx *sql.Tx, k quoteOwnerKey, o *quoteOrderRow, nowMs int64) (int64, error) {
	if o.RequestJSON == "" {
		o.RequestJSON = o.Request.canonicalJSON()
	}
	res, err := tx.Exec(`INSERT INTO pending_limit_orders
		(strategy_id, symbol, side, order_oid, limit_price, order_size, tif, created_at, owner_kind, partition_scope, partition_source,
		 quote_generation, quote_role, provider, account_key, venue_namespace, request_json, request_expiry_ms, client_order_id,
		 venue_order_id, submit_state, cancel_state, confirmed_qty, applied_qty, updated_at_ms)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, 'quote', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, '', '0', '0', ?)`,
		k.sid, o.Coin, o.Side, quoteRatFloat(mustQuoteRatOrZero(o.Request.Px)), quoteRatFloat(mustQuoteRatOrZero(o.Request.Size)), o.Request.TIF,
		formatTime(timeFromMs(nowMs)), string(k.Partition.Scope), k.Partition.Source, o.Generation, o.Role, o.Provider, o.AccountKey,
		o.Namespace, o.RequestJSON, o.RequestExpiryMs, o.ClientOrderID, o.SubmitState, nowMs)
	if err != nil {
		return 0, fmt.Errorf("reserve quote %s order for %s: %w", o.Role, k.label(), err)
	}
	return res.LastInsertId()
}

func mustQuoteRatOrZero(raw string) *big.Rat {
	r, err := parseQuoteRat(raw)
	if err != nil {
		return quoteRatZero()
	}
	return r
}

type quoteReservation struct {
	ExpectRevision int64
	Generation     *quoteGeneration
	Orders         []*quoteOrderRow
	DeadlineMs     int64
	ContractField  string
	ContractSeq    int64
}

func (sdb *StateDB) reserveQuoteOrders(k quoteOwnerKey, r *quoteReservation, nowMs int64) error {
	tx, err := sdb.beginQuoteTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := checkQuoteRevisionTx(tx, k, r.ExpectRevision); err != nil {
		return err
	}
	if r.Generation != nil {
		g := r.Generation
		if _, err := tx.Exec(`INSERT INTO quote_generations (scope, source, strategy_id, generation, deadline_ms, status, desired_json, reservation_json, terminal_json, hold_reason, created_at_ms, updated_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`, string(k.Partition.Scope), k.Partition.Source, k.sid, g.Generation, g.DeadlineMs, g.Status,
			g.DesiredJSON, g.ReservationJSON, g.HoldReason, nowMs, nowMs); err != nil {
			return fmt.Errorf("reserve quote generation %d for %s: %w", g.Generation, k.label(), err)
		}
		if _, err := tx.Exec(`UPDATE quote_contracts SET last_generation = MAX(last_generation, ?), last_deadline_ms = MAX(last_deadline_ms, ?), updated_at_ms = MAX(updated_at_ms, ?)
			WHERE scope = ? AND source = ? AND strategy_id = ?`, g.Generation, g.DeadlineMs, nowMs, string(k.Partition.Scope), k.Partition.Source, k.sid); err != nil {
			return err
		}
	}
	if r.ContractField != "" {
		col := map[string]string{"stop": "last_stop_seq", "close": "last_close_seq"}[r.ContractField]
		if col == "" {
			return fmt.Errorf("unknown quote sequence %q", r.ContractField)
		}
		if _, err := tx.Exec(`UPDATE quote_contracts SET `+col+` = MAX(`+col+`, ?), updated_at_ms = MAX(updated_at_ms, ?) WHERE scope = ? AND source = ? AND strategy_id = ?`,
			r.ContractSeq, nowMs, string(k.Partition.Scope), k.Partition.Source, k.sid); err != nil {
			return err
		}
	}
	for _, o := range r.Orders {
		id, err := insertQuoteOrderRowTx(tx, k, o, nowMs)
		if err != nil {
			return err
		}
		o.RowID = id
		o.CreatedAtMs = nowMs
		o.UpdatedAtMs = nowMs
	}
	if err := sdb.commitQuoteTx(tx); err != nil {
		for _, o := range r.Orders {
			o.RowID = 0
		}
		return err
	}
	return nil
}

func (sdb *StateDB) updateQuoteOrder(k quoteOwnerKey, o *quoteOrderRow, from []string, nowMs int64) error {
	tx, err := sdb.beginQuoteTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := updateQuoteOrderTx(tx, k, o, from, nowMs); err != nil {
		return err
	}
	return sdb.commitQuoteTx(tx)
}

func updateQuoteOrderTx(tx *sql.Tx, k quoteOwnerKey, o *quoteOrderRow, from []string, nowMs int64) error {
	if o.RowID <= 0 {
		return fmt.Errorf("quote order for %s has no row identity", k.label())
	}
	var storedCancel, storedConfirmed string
	err := tx.QueryRow(`SELECT cancel_state, confirmed_qty FROM pending_limit_orders WHERE id = ? AND owner_kind = 'quote' AND partition_scope = ? AND partition_source = ? AND strategy_id = ?`,
		o.RowID, string(k.Partition.Scope), k.Partition.Source, k.sid).Scan(&storedCancel, &storedConfirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("quote order %d is not owned by %s in the expected state %v", o.RowID, k.label(), from)
	}
	if err != nil {
		return fmt.Errorf("read quote order %d for %s: %w", o.RowID, k.label(), err)
	}
	cancel := o.CancelState
	if cancel == quoteCancelNone && storedCancel != quoteCancelNone {
		cancel = storedCancel
		if storedCancel == quoteCancelRequested && quoteSubmitTerminal(o.SubmitState) {
			cancel = quoteCancelConfirmed
		}
	}
	confirmed, err := quoteLargerQtyText(storedConfirmed, o.ConfirmedQty)
	if err != nil {
		return fmt.Errorf("quote order %d confirmed quantity: %w", o.RowID, err)
	}
	args := []interface{}{o.SubmitState, cancel, o.VenueOrderID, o.VenueOrderID, confirmed, o.AcceptedAtMs, o.TerminalAtMs, o.OutcomeJSON, nowMs,
		o.RowID, string(k.Partition.Scope), k.Partition.Source, k.sid}
	where := ""
	if len(from) > 0 {
		marks := make([]string, 0, len(from))
		for _, f := range from {
			marks = append(marks, "?")
			args = append(args, f)
		}
		where = " AND submit_state IN (" + strings.Join(marks, ", ") + ")"
	}
	res, err := tx.Exec(`UPDATE pending_limit_orders SET submit_state = ?, cancel_state = ?,
		venue_order_id = CASE WHEN ? != '' THEN ? ELSE venue_order_id END,
		confirmed_qty = ?, accepted_at_ms = MAX(accepted_at_ms, ?), terminal_at_ms = MAX(terminal_at_ms, ?), outcome_json = ?, updated_at_ms = ?
		WHERE id = ? AND owner_kind = 'quote' AND partition_scope = ? AND partition_source = ? AND strategy_id = ?`+where, args...)
	if err != nil {
		return fmt.Errorf("update quote order %d for %s: %w", o.RowID, k.label(), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("quote order %d is not owned by %s in the expected state %v", o.RowID, k.label(), from)
	}
	o.CancelState = cancel
	o.ConfirmedQty = confirmed
	o.UpdatedAtMs = nowMs
	return nil
}

func timeFromMs(ms int64) time.Time {
	return time.UnixMilli(ms).UTC()
}

type quoteFillInsert struct {
	Fill       quoteRawFill
	Kind       string
	CorrectsID int64
	Role       string
	OrderRowID int64
}

func (sdb *StateDB) recordQuoteFills(k quoteOwnerKey, provider string, fills []quoteFillInsert, nowMs int64) (int, []string, error) {
	if len(fills) == 0 {
		return 0, nil, nil
	}
	tx, err := sdb.beginQuoteTx()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	inserted := 0
	var conflicts []string
	for _, in := range fills {
		f := in.Fill
		kind := f.kind()
		var existingDigest, existingSID string
		var existingScope, existingSource string
		err := tx.QueryRow(`SELECT digest, strategy_id, scope, source FROM quote_fills WHERE provider = ? AND account_key = ? AND namespace = ? AND coin = ?
			AND order_id = ? AND event_time_ms = ? AND trade_id = ? AND kind = ?`, provider, f.Account, f.Namespace, f.Coin, f.OrderID, f.TimeMs, f.TradeID, kind).
			Scan(&existingDigest, &existingSID, &existingScope, &existingSource)
		switch {
		case err == nil:
			if existingDigest != f.digest() || existingSID != k.sid || existingScope != string(k.Partition.Scope) || existingSource != k.Partition.Source {
				conflicts = append(conflicts, fmt.Sprintf("fill %s has a stored copy that differs from the venue copy", f.key()))
			}
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return 0, nil, err
		}
		if kind == "fee_correction" && in.CorrectsID == 0 {
			err := tx.QueryRow(`SELECT id FROM quote_fills WHERE provider = ? AND account_key = ? AND namespace = ? AND coin = ? AND order_id = ? AND trade_id = ? AND kind = 'fill'
				AND scope = ? AND source = ? AND strategy_id = ?`, provider, f.Account, f.Namespace, f.Coin, f.OrderID, f.CorrectsTrade,
				string(k.Partition.Scope), k.Partition.Source, k.sid).Scan(&in.CorrectsID)
			if errors.Is(err, sql.ErrNoRows) {
				conflicts = append(conflicts, fmt.Sprintf("fee correction %s names trade %s, which is not a saved fill of this owner", f.key(), f.CorrectsTrade))
				continue
			}
			if err != nil {
				return 0, nil, err
			}
		}
		if _, err := tx.Exec(`INSERT INTO quote_fills (scope, source, strategy_id, provider, account_key, namespace, coin, order_id, event_time_ms, trade_id,
			kind, corrects_fill_id, role, client_order_id, order_row_id, side, px, size, fee, fee_asset, start_position, raw_json, digest, settlement_id, observed_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			string(k.Partition.Scope), k.Partition.Source, k.sid, provider, f.Account, f.Namespace, f.Coin, f.OrderID, f.TimeMs, f.TradeID,
			kind, in.CorrectsID, in.Role, f.ClientOrderID, in.OrderRowID, f.Side, f.Px, f.Size, f.Fee, f.FeeAsset, f.StartPosition, f.Raw, f.digest(), nowMs); err != nil {
			return 0, nil, fmt.Errorf("record quote fill %s: %w", f.key(), err)
		}
		inserted++
	}
	if err := sdb.commitQuoteTx(tx); err != nil {
		return 0, nil, err
	}
	sort.Strings(conflicts)
	return inserted, conflicts, nil
}

func (sdb *StateDB) recordQuoteFundingEvidence(k quoteOwnerKey, provider, account, namespace string, ev quoteFundingEvidenceRow, nowMs int64) (bool, error) {
	tx, err := sdb.beginQuoteTx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var digest string
	err = tx.QueryRow(`SELECT digest FROM quote_funding_evidence WHERE provider = ? AND account_key = ? AND namespace = ? AND coin = ? AND dedup_id = ?`,
		provider, account, namespace, ev.Coin, ev.DedupID).Scan(&digest)
	switch {
	case err == nil:
		if digest != ev.Digest {
			return false, fmt.Errorf("quote funding evidence %s has a stored copy that differs from the new copy", ev.DedupID)
		}
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO quote_funding_evidence (provider, account_key, namespace, coin, dedup_id, scope, source, strategy_id, event_ms, raw_json, digest, settlement_id, observed_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`, provider, account, namespace, ev.Coin, ev.DedupID, string(k.Partition.Scope), k.Partition.Source, k.sid,
		ev.EventMs, ev.RawJSON, ev.Digest, nowMs); err != nil {
		return false, err
	}
	if err := sdb.commitQuoteTx(tx); err != nil {
		return false, err
	}
	return true, nil
}

type quoteOrderProgress struct {
	RowID        int64
	AppliedQty   string
	ConfirmedQty string
	SubmitState  string
}

func quoteLargerQtyText(stored, incoming string) (string, error) {
	s, err := parseQuoteRat(stored)
	if err != nil {
		return "", fmt.Errorf("stored quantity: %w", err)
	}
	n, err := parseQuoteRat(incoming)
	if err != nil {
		return "", fmt.Errorf("new quantity: %w", err)
	}
	if n.Cmp(s) > 0 {
		return incoming, nil
	}
	return stored, nil
}

type quoteSettlementCommit struct {
	Key             quoteOwnerKey
	Scope           PortfolioScope
	ExpectRevision  int64
	Kind            string
	DeadlineMs      int64
	BoundaryMs      int64
	Book            quoteExactBook
	Staged          *StrategyState
	FillIDs         []int64
	FundingIDs      []string
	FundingJSON     string
	OrderProgress   []quoteOrderProgress
	Generations     []*quoteGeneration
	Progress        quoteProgressRecord
	CommittedAtMs   int64
	AppliedFillKeys []string
}

type quoteCommitResult struct {
	Revision     int64
	SettlementID int64
	Flushed      []int
	Diags        []TradeDiagnosticsRow
	Summary      quoteCommitSummary
}

func (sdb *StateDB) commitQuoteSettlement(c *quoteSettlementCommit) (*quoteCommitResult, error) {
	tx, err := sdb.beginQuoteTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	k := c.Key
	contract, err := checkQuoteRevisionTx(tx, k, c.ExpectRevision)
	if err != nil {
		return nil, err
	}
	if contract == nil {
		return nil, fmt.Errorf("quote owner %s has no durable contract", k.label())
	}
	res := &quoteCommitResult{}
	summary := quoteCommitSummary{SettlementKind: c.Kind, Revision: contract.Revision, AppliedBoundaryMs: contract.AppliedBoundaryMs, DeadlineMs: c.DeadlineMs}
	if c.Kind != quoteSettleNone {
		if err := c.Book.validate(); err != nil {
			return nil, err
		}
		next := contract.Revision + 1
		flushed, diags, err := sdb.writeOneStrategyBookTx(tx, c.Staged, k.sid, c.Scope)
		if err != nil {
			return nil, err
		}
		res.Flushed, res.Diags = flushed, diags
		fillJSON, _ := json.Marshal(c.FillIDs)
		summary = quoteCommitSummary{SettlementKind: c.Kind, Revision: next, AppliedBoundaryMs: c.BoundaryMs, AppliedFillKeys: c.AppliedFillKeys, DeadlineMs: c.DeadlineMs}
		sumJSON, _ := json.Marshal(summary)
		ins, err := tx.Exec(`INSERT INTO quote_settlements (scope, source, strategy_id, revision, kind, deadline_ms, applied_boundary_ms, fill_ids_json, funding_json, summary_json, committed_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, string(k.Partition.Scope), k.Partition.Source, k.sid, next, c.Kind, c.DeadlineMs, c.BoundaryMs,
			string(fillJSON), c.FundingJSON, string(sumJSON), c.CommittedAtMs)
		if err != nil {
			return nil, fmt.Errorf("record quote settlement for %s: %w", k.label(), err)
		}
		sid, err := ins.LastInsertId()
		if err != nil {
			return nil, err
		}
		res.SettlementID = sid
		for _, id := range c.FillIDs {
			r, err := tx.Exec(`UPDATE quote_fills SET settlement_id = ? WHERE id = ? AND settlement_id = 0 AND scope = ? AND source = ? AND strategy_id = ?`,
				sid, id, string(k.Partition.Scope), k.Partition.Source, k.sid)
			if err != nil {
				return nil, err
			}
			if n, _ := r.RowsAffected(); n != 1 {
				return nil, fmt.Errorf("quote fill %d is already applied or not owned by %s", id, k.label())
			}
		}
		for _, id := range c.FundingIDs {
			r, err := tx.Exec(`UPDATE quote_funding_evidence SET settlement_id = ? WHERE dedup_id = ? AND settlement_id = 0 AND scope = ? AND source = ? AND strategy_id = ?`,
				sid, id, string(k.Partition.Scope), k.Partition.Source, k.sid)
			if err != nil {
				return nil, err
			}
			if n, _ := r.RowsAffected(); n != 1 {
				return nil, fmt.Errorf("quote funding evidence %s is already applied or not owned by %s", id, k.label())
			}
		}
		for _, p := range c.OrderProgress {
			var storedConfirmed string
			err := tx.QueryRow(`SELECT confirmed_qty FROM pending_limit_orders WHERE id = ? AND owner_kind = 'quote' AND partition_scope = ? AND partition_source = ? AND strategy_id = ?`,
				p.RowID, string(k.Partition.Scope), k.Partition.Source, k.sid).Scan(&storedConfirmed)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("quote order %d is not owned by %s", p.RowID, k.label())
			}
			if err != nil {
				return nil, err
			}
			confirmed, err := quoteLargerQtyText(storedConfirmed, p.ConfirmedQty)
			if err == nil {
				confirmed, err = quoteLargerQtyText(confirmed, p.AppliedQty)
			}
			if err != nil {
				return nil, fmt.Errorf("quote order %d confirmed quantity: %w", p.RowID, err)
			}
			r, err := tx.Exec(`UPDATE pending_limit_orders SET applied_qty = ?, confirmed_qty = ?, submit_state = CASE WHEN ? != '' THEN ? ELSE submit_state END, updated_at_ms = ?
				WHERE id = ? AND owner_kind = 'quote' AND partition_scope = ? AND partition_source = ? AND strategy_id = ?`,
				p.AppliedQty, confirmed, p.SubmitState, p.SubmitState, c.CommittedAtMs, p.RowID, string(k.Partition.Scope), k.Partition.Source, k.sid)
			if err != nil {
				return nil, err
			}
			if n, _ := r.RowsAffected(); n != 1 {
				return nil, fmt.Errorf("quote order %d is not owned by %s", p.RowID, k.label())
			}
		}
		for _, g := range c.Generations {
			if _, err := tx.Exec(`UPDATE quote_generations SET status = ?, terminal_json = ?, updated_at_ms = ? WHERE scope = ? AND source = ? AND strategy_id = ? AND generation = ?`,
				g.Status, g.TerminalJSON, c.CommittedAtMs, string(k.Partition.Scope), k.Partition.Source, k.sid, g.Generation); err != nil {
				return nil, err
			}
		}
		r, err := tx.Exec(`UPDATE quote_contracts SET revision = ?, book_json = ?, applied_boundary_ms = MAX(applied_boundary_ms, ?), last_deadline_ms = MAX(last_deadline_ms, ?), updated_at_ms = ?
			WHERE scope = ? AND source = ? AND strategy_id = ? AND revision = ?`, next, c.Book.JSON(), c.BoundaryMs, c.DeadlineMs, c.CommittedAtMs,
			string(k.Partition.Scope), k.Partition.Source, k.sid, contract.Revision)
		if err != nil {
			return nil, err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return nil, fmt.Errorf("%w: quote owner %s moved during the commit", errQuoteRevisionStale, k.label())
		}
		res.Revision = next
	} else {
		res.Revision = contract.Revision
		if _, err := tx.Exec(`UPDATE quote_contracts SET last_deadline_ms = MAX(last_deadline_ms, ?), updated_at_ms = MAX(updated_at_ms, ?)
			WHERE scope = ? AND source = ? AND strategy_id = ? AND revision = ?`, c.DeadlineMs, c.CommittedAtMs,
			string(k.Partition.Scope), k.Partition.Source, k.sid, contract.Revision); err != nil {
			return nil, err
		}
	}
	if c.Progress != nil {
		if err := c.Progress.WriteQuoteProgress(tx, summary); err != nil {
			return nil, fmt.Errorf("quote progress record: %w", err)
		}
	}
	if err := sdb.commitQuoteTx(tx); err != nil {
		return nil, err
	}
	res.Summary = summary
	return res, nil
}

func (sdb *StateDB) setQuoteOperatorHold(k quoteOwnerKey, hold bool, reason string, nowMs int64) error {
	tx, err := sdb.beginQuoteTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`UPDATE quote_contracts SET operator_hold = ?, operator_hold_reason = ?, updated_at_ms = MAX(updated_at_ms, ?) WHERE scope = ? AND source = ? AND strategy_id = ?`,
		boolToInt(hold), reason, nowMs, string(k.Partition.Scope), k.Partition.Source, k.sid)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return fmt.Errorf("quote owner %s has no durable contract in the %s state file", k.label(), sdb.storageRoleOf())
	}
	return sdb.commitQuoteTx(tx)
}

func (sdb *StateDB) requestQuoteGenerationCancel(k quoteOwnerKey, generation int64, nowMs int64) (int, error) {
	tx, err := sdb.beginQuoteTx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	c, err := readQuoteContractTx(tx, k)
	if err != nil {
		return 0, err
	}
	if c == nil {
		return 0, fmt.Errorf("quote owner %s has no durable contract in the %s state file", k.label(), sdb.storageRoleOf())
	}
	if generation != c.LastGeneration {
		return 0, fmt.Errorf("generation %d is not the current quote generation %d of %s", generation, c.LastGeneration, k.label())
	}
	r, err := tx.Exec(`UPDATE pending_limit_orders SET cancel_state = 'requested', updated_at_ms = ?
		WHERE owner_kind = 'quote' AND partition_scope = ? AND partition_source = ? AND strategy_id = ? AND quote_generation = ?
		AND quote_role IN ('bid','ask') AND submit_state IN ('submitting','resting','unknown') AND cancel_state = ''`,
		nowMs, string(k.Partition.Scope), k.Partition.Source, k.sid, generation)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if err := sdb.commitQuoteTx(tx); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (sdb *StateDB) guardQuoteOwnersTx(tx *sql.Tx, strategies []*StrategyState, sids []string, fullFile bool) error {
	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'quote_contracts'`).Scan(&present); err != nil {
		return err
	}
	incoming := make(map[string]*StrategyState, len(strategies))
	for i, s := range strategies {
		incoming[sids[i]] = s
	}
	if present == 0 {
		for i, s := range strategies {
			if s.Quote != nil && s.Quote.Revision > 0 {
				return fmt.Errorf("quote owner %s holds revision %d in memory but the %s state file has no quote contract table", sids[i], s.Quote.Revision, sdb.storageRoleOf())
			}
		}
		return nil
	}
	rows, err := tx.Query(`SELECT scope, source, strategy_id, revision FROM quote_contracts ORDER BY scope, source, strategy_id`)
	if err != nil {
		return fmt.Errorf("read quote contracts before a save: %w", err)
	}
	type owner struct {
		p   RiskPartition
		sid string
		rev int64
	}
	var owners []owner
	durable := map[string]bool{}
	for rows.Next() {
		var o owner
		var scope, source string
		if err := rows.Scan(&scope, &source, &o.sid, &o.rev); err != nil {
			rows.Close()
			return err
		}
		o.p = RiskPartition{Scope: PortfolioScope(scope), Source: source}
		owners = append(owners, o)
		durable[o.sid] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, s := range strategies {
		if s.Quote != nil && s.Quote.Revision > 0 && !durable[sids[i]] {
			return fmt.Errorf("quote owner %s holds revision %d in memory but has no durable contract in the %s state file; refusing the save", s.ID, s.Quote.Revision, sdb.storageRoleOf())
		}
	}
	for _, o := range owners {
		s, ok := incoming[o.sid]
		if !ok {
			if fullFile {
				return fmt.Errorf("a full save of the %s state file omits quote owner %s (%s, revision %d); its book and evidence are kept, so the save is refused", sdb.storageRoleOf(), o.sid, partitionLabel(o.p), o.rev)
			}
			continue
		}
		if s.Quote == nil {
			return fmt.Errorf("quote owner %s has a durable revision %d but no quote state in memory; refusing an ordinary save", s.ID, o.rev)
		}
		if s.Quote.Revision != o.rev {
			return fmt.Errorf("%w: quote owner %s is at durable revision %d, the save carries %d", errQuoteRevisionStale, s.ID, o.rev, s.Quote.Revision)
		}
		if s.Quote.Partition != o.p {
			return fmt.Errorf("quote owner %s is durable in %s, memory names %s", s.ID, partitionLabel(o.p), partitionLabel(s.Quote.Partition))
		}
		if err := quoteSettledFieldsMatchTx(tx, o.sid, s); err != nil {
			return fmt.Errorf("quote owner %s: %w; an ordinary save cannot change settled quote fields", s.ID, err)
		}
	}
	return nil
}

func quoteSettledFieldsMatchTx(tx *sql.Tx, sid string, s *StrategyState) error {
	var cash float64
	var funding string
	err := tx.QueryRow(`SELECT cash, paper_funding_state FROM strategies WHERE id = ?`, sid).Scan(&cash, &funding)
	if err != nil {
		return fmt.Errorf("read the durable quote book: %w", err)
	}
	if cash != s.Cash {
		return fmt.Errorf("cash %.10g differs from the durable %.10g", s.Cash, cash)
	}
	mem, err := marshalPaperFundingState(s)
	if err != nil {
		return err
	}
	if mem != funding {
		return fmt.Errorf("paper funding state differs from the durable revision")
	}
	rows, err := tx.Query(`SELECT symbol, quantity, avg_cost, side, position_id, initial_quantity, risk_anchor_price FROM positions WHERE strategy_id = ? ORDER BY symbol`, sid)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var sym, side, pid string
		var qty, avg, initial, anchor float64
		if err := rows.Scan(&sym, &qty, &avg, &side, &pid, &initial, &anchor); err != nil {
			return err
		}
		seen[sym] = true
		pos := s.Positions[sym]
		if pos == nil {
			return fmt.Errorf("position %s is durable but absent in memory", sym)
		}
		if pos.Quantity != qty || pos.AvgCost != avg || pos.Side != side || pos.TradePositionID != pid || pos.InitialQuantity != initial || pos.RiskAnchorPrice != anchor {
			return fmt.Errorf("position %s differs from the durable revision", sym)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for sym := range s.Positions {
		if !seen[sym] {
			return fmt.Errorf("position %s exists in memory but not in the durable revision", sym)
		}
	}
	return nil
}

type quoteFileSummary struct {
	Owners   []quoteFileOwner
	Unmapped []string
}

type quoteFileOwner struct {
	Partition RiskPartition
	StorageID string
	ProcessID string
	Mapped    bool
	Pending   []string
	Inventory string
	Revision  int64
	Coin      string
}

func (sdb *StateDB) quoteFileSummary(processFor func(string) (string, bool)) (*quoteFileSummary, error) {
	out := &quoteFileSummary{}
	exists, err := tableExists(sdb, "quote_contracts")
	if err != nil || !exists {
		return out, err
	}
	rows, err := sdb.db.Query(`SELECT ` + quoteContractColumns + ` FROM quote_contracts ORDER BY scope, source, strategy_id`)
	if err != nil {
		return nil, fmt.Errorf("read quote contracts in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	var contracts []*quoteContractRow
	for rows.Next() {
		c, err := scanQuoteContract(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		contracts = append(contracts, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, c := range contracts {
		k := quoteOwnerKey{Partition: c.Partition, sid: c.StorageID}
		procID, mapped := processFor(c.StorageID)
		k.StrategyID = procID
		owner := quoteFileOwner{Partition: c.Partition, StorageID: c.StorageID, ProcessID: procID, Mapped: mapped, Inventory: c.Book.Qty, Revision: c.Revision, Coin: c.Coin}
		pending, err := sdb.quoteOwnerPending(k, c)
		if err != nil {
			return nil, err
		}
		owner.Pending = pending
		out.Owners = append(out.Owners, owner)
		known[string(c.Partition.Scope)+"|"+c.Partition.Source+"|"+c.StorageID] = true
	}
	orphan, err := sdb.db.Query(`SELECT DISTINCT partition_scope, partition_source, strategy_id FROM pending_limit_orders WHERE owner_kind = 'quote'
		UNION SELECT DISTINCT scope, source, strategy_id FROM quote_fills WHERE settlement_id = 0
		UNION SELECT DISTINCT scope, source, strategy_id FROM quote_funding_evidence WHERE settlement_id = 0`)
	if err != nil {
		return nil, err
	}
	for orphan.Next() {
		var scope, source, sid string
		if err := orphan.Scan(&scope, &source, &sid); err != nil {
			orphan.Close()
			return nil, err
		}
		if !known[scope+"|"+source+"|"+sid] {
			out.Unmapped = append(out.Unmapped, fmt.Sprintf("quote evidence for stored strategy %q (%s) has no quote contract", sid, partitionLabel(RiskPartition{Scope: PortfolioScope(scope), Source: source})))
		}
	}
	orphan.Close()
	sort.Strings(out.Unmapped)
	return out, orphan.Err()
}

func (sdb *StateDB) quoteOwnerPending(k quoteOwnerKey, c *quoteContractRow) ([]string, error) {
	var out []string
	if q := mustQuoteRat(c.Book.Qty); q.Sign() != 0 {
		out = append(out, fmt.Sprintf("inventory %s %s", c.Book.Qty, c.Coin))
	}
	rows, err := sdb.db.Query(`SELECT `+quoteOrderColumns+` FROM pending_limit_orders WHERE owner_kind = 'quote' AND partition_scope = ? AND partition_source = ? AND strategy_id = ? ORDER BY id`,
		string(k.Partition.Scope), k.Partition.Source, k.sid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		o, err := scanQuoteOrder(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if !o.resolved() {
			out = append(out, fmt.Sprintf("%s order row %d (generation %d, client %s) is %s with applied %s of confirmed %s", o.Role, o.RowID, o.Generation, o.ClientOrderID, o.SubmitState, o.AppliedQty, o.ConfirmedQty))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var unapplied, funding int
	if err := sdb.db.QueryRow(`SELECT COUNT(*) FROM quote_fills WHERE scope = ? AND source = ? AND strategy_id = ? AND settlement_id = 0`,
		string(k.Partition.Scope), k.Partition.Source, k.sid).Scan(&unapplied); err != nil {
		return nil, err
	}
	if unapplied > 0 {
		out = append(out, fmt.Sprintf("%d unapplied fill(s)", unapplied))
	}
	if err := sdb.db.QueryRow(`SELECT COUNT(*) FROM quote_funding_evidence WHERE scope = ? AND source = ? AND strategy_id = ? AND settlement_id = 0`,
		string(k.Partition.Scope), k.Partition.Source, k.sid).Scan(&funding); err != nil {
		return nil, err
	}
	if funding > 0 {
		out = append(out, fmt.Sprintf("%d unapplied funding event(s)", funding))
	}
	return out, nil
}

func inspectQuoteRows(db *StateDB, fi *storageFileInspection, ident storageIdentityMap, admitted map[string]bool) ([]string, error) {
	var rejections []string
	hasCol, err := db.tableHasColumn("pending_limit_orders", "owner_kind")
	if err != nil {
		return nil, err
	}
	exists, err := tableExists(db, "quote_contracts")
	if err != nil {
		return nil, err
	}
	if !exists {
		if hasCol {
			var n int
			if err := db.db.QueryRow(`SELECT COUNT(*) FROM pending_limit_orders WHERE owner_kind = 'quote'`).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				rejections = append(rejections, fmt.Sprintf("%s state file: %d quote order row(s) exist with no quote contract table", fi.Role, n))
			}
		}
		return rejections, nil
	}
	ownedParts := make(map[string]bool, len(fi.PartitionsOwned))
	for _, p := range fi.PartitionsOwned {
		ownedParts[p] = true
	}
	summary, err := db.quoteFileSummary(func(sid string) (string, bool) { return ident.processFor(fi.Role, sid) })
	if err != nil {
		return nil, err
	}
	for _, u := range summary.Unmapped {
		rejections = append(rejections, fmt.Sprintf("%s state file: %s", fi.Role, u))
	}
	for _, o := range summary.Owners {
		fi.QuoteOwners++
		label := fmt.Sprintf("%s %s (stored %s, revision %d, %s)", partitionLabel(o.Partition), o.ProcessID, o.StorageID, o.Revision, o.Coin)
		if len(ownedParts) > 0 && !ownedParts[o.Partition.String()] {
			rejections = append(rejections, fmt.Sprintf("%s state file: quote contract %s belongs to a partition the file does not own (%s)", fi.Role, label, strings.Join(fi.PartitionsOwned, ", ")))
		}
		for _, p := range o.Pending {
			fi.QuotePending = append(fi.QuotePending, label+": "+p)
		}
		if len(o.Pending) == 0 {
			if !o.Mapped || !admitted[o.ProcessID] {
				rejections = append(rejections, fmt.Sprintf("%s state file: quote owner %s has a durable quote contract and no admitted quote owner runs it; every full save of this file refuses an unowned quote contract, so startup refuses before any save", fi.Role, label))
			}
			continue
		}
		if !o.Mapped {
			rejections = append(rejections, fmt.Sprintf("%s state file: quote owner %s is unmapped but still holds recovery evidence: %s; restore its admitted configuration until it is flat and resolved", fi.Role, label, strings.Join(o.Pending, "; ")))
			continue
		}
		if !admitted[o.ProcessID] {
			rejections = append(rejections, fmt.Sprintf("%s state file: quote owner %s holds recovery evidence (%s) and no admitted quote owner runs it; production startup refuses to discard or adopt it", fi.Role, label, strings.Join(o.Pending, "; ")))
		}
	}
	return rejections, nil
}

func (sdb *StateDB) refuseQuoteOwnedStorageID(sid string) error {
	exists, err := tableExists(sdb, "quote_contracts")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var n int
	if err := sdb.db.QueryRow(`SELECT COUNT(*) FROM quote_contracts WHERE strategy_id = ?`, sid).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("stored strategy %q is a quote owner in the %s state file; its trades, book and closed records change only through the quote settlement", sid, sdb.storageRoleOf())
	}
	return nil
}

func quoteFundingOwnerFor(state *AppState, key SharedWalletKey, coin string) *StrategyState {
	if state == nil {
		return nil
	}
	acct := strings.ToLower(strings.TrimSpace(key.Account))
	if acct == "" {
		return nil
	}
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := state.Strategies[id]
		if s == nil || s.Quote == nil || s.Quote.Partition.Scope != ScopeLive {
			continue
		}
		if !strings.EqualFold(s.Quote.Coin, coin) {
			continue
		}
		if strings.HasSuffix(strings.ToLower(s.Quote.AccountKey), acct) {
			return s
		}
	}
	return nil
}

func recordQuoteFundingEvent(sdb *StateDB, owner *StrategyState, ev hlLedgerEvent, coin, dedupID string) bool {
	raw, err := json.Marshal(ev)
	if err != nil {
		fmt.Printf("[WARN] [quote] %s: funding evidence %s could not be encoded: %v; holding the watermark\n", owner.ID, dedupID, err)
		return false
	}
	k, err := sdb.quoteOwnerKey(owner.ID, owner.Quote.Partition)
	if err != nil {
		fmt.Printf("[WARN] [quote] %s: funding evidence %s has no owning file: %v; holding the watermark\n", owner.ID, dedupID, err)
		return false
	}
	row := quoteFundingEvidenceRow{DedupID: dedupID, Coin: coin, EventMs: ev.Time, RawJSON: string(raw), Digest: sha256Hex(raw)}
	if _, err := sdb.recordQuoteFundingEvidence(k, owner.Quote.Provider, owner.Quote.AccountKey, owner.Quote.Namespace, row, quoteNowMs()); err != nil {
		fmt.Printf("[WARN] [quote] %s: funding evidence %s could not be saved: %v; holding the watermark\n", owner.ID, dedupID, err)
		return false
	}
	return true
}

func quoteNowMs() int64 {
	return tradingNow().UTC().UnixMilli()
}

type pendingOrdersCombined struct {
	Manual []PendingLimitOrder
	Quote  []*quoteOrderRow
}

func (sdb *StateDB) loadAllQuoteOrderRows() ([]*quoteOrderRow, error) {
	has, err := sdb.tableHasColumn("pending_limit_orders", "owner_kind")
	if err != nil {
		return nil, err
	}
	if !has {
		if sdb.readOnly {
			return nil, nil
		}
		return nil, fmt.Errorf("the %s state file has no migrated pending_limit_orders table", sdb.storageRoleOf())
	}
	rows, err := sdb.db.Query(`SELECT ` + quoteOrderColumns + ` FROM pending_limit_orders WHERE owner_kind = 'quote' ORDER BY partition_scope, partition_source, strategy_id, id`)
	if err != nil {
		return nil, fmt.Errorf("read quote orders in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	defer rows.Close()
	var out []*quoteOrderRow
	for rows.Next() {
		o, err := scanQuoteOrder(rows)
		if err != nil {
			return nil, err
		}
		o.SourceRole = sdb.storageRoleOf()
		if proc, ok := sdb.mapStoredStrategyID(o.storageID); ok {
			o.StrategyID = proc
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (st *StateStore) LoadPendingOrdersCombined() (pendingOrdersCombined, error) {
	var out pendingOrdersCombined
	if st == nil {
		return out, nil
	}
	manual, err := st.LoadPendingLimitOrders()
	if err != nil {
		return pendingOrdersCombined{}, err
	}
	out.Manual = manual
	for _, role := range st.order {
		db := st.file(role)
		if db == nil {
			continue
		}
		rows, err := db.loadAllQuoteOrderRows()
		if err != nil {
			return pendingOrdersCombined{}, err
		}
		out.Quote = append(out.Quote, rows...)
	}
	return out, nil
}

func dispatchQuotePendingRows(store *StateStore, rows []*quoteOrderRow) []string {
	if len(rows) == 0 {
		return nil
	}
	adm := quoteAdmissionForStore(store)
	type ownerKey struct {
		role storageRole
		part RiskPartition
		id   string
	}
	unresolved := map[ownerKey]int{}
	for _, o := range rows {
		if o.resolved() {
			continue
		}
		unresolved[ownerKey{o.SourceRole, o.Partition, o.StrategyID + "|" + o.storageID}]++
	}
	keys := make([]ownerKey, 0, len(unresolved))
	for k := range unresolved {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].role != keys[j].role {
			return keys[i].role < keys[j].role
		}
		if keys[i].part.String() != keys[j].part.String() {
			return keys[i].part.String() < keys[j].part.String()
		}
		return keys[i].id < keys[j].id
	})
	var lines []string
	handled := false
	for _, k := range keys {
		proc := strings.SplitN(k.id, "|", 2)[0]
		if adm != nil && proc == adm.sc.ID && k.part == adm.partition && k.role == adm.db.storageRoleOf() {
			handled = true
			lines = append(lines, fmt.Sprintf("[limit] %d unresolved quote order row(s) of %s go to the quote settlement owner", unresolved[k], proc))
			continue
		}
		lines = append(lines, fmt.Sprintf("[limit] %d unresolved quote order row(s) for %s (%s, %s state file) are held: no admitted quote coordinator owns them, and the manual gates never touch quote rows", unresolved[k], k.id, partitionLabel(k.part), k.role))
	}
	if handled {
		ctx, cancel := context.WithTimeout(context.Background(), 3*quoteShutdownCleanupBudget)
		adm.resolveUncertain(ctx)
		cancel()
		adm.flushOutbox()
	}
	return lines
}
