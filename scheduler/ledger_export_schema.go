package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	LedgerExportSchema        = "go-trader.booked-ledger"
	LedgerExportSchemaVersion = 2
)

const (
	ledgerStatusAvailable     = "available"
	ledgerStatusUnavailable   = "unavailable"
	ledgerStatusNotApplicable = "not_applicable"
)

const (
	ledgerReasonColumnAbsent      = "column_absent"
	ledgerReasonStoredNull        = "stored_null"
	ledgerReasonUnstamped         = "unstamped"
	ledgerReasonNotRecorded       = "not_recorded"
	ledgerReasonAmbiguousEvidence = "ambiguous_evidence"
	ledgerReasonNotApplicable     = "not_applicable"
)

const (
	ledgerProvenanceStored  = "stored"
	ledgerProvenanceDerived = "derived"
	ledgerProvenanceMatched = "matched"
)

const (
	ledgerTradeTimestampMeaning  = "stored ledger timestamp; exchange fill time is not established"
	ledgerWalletTimestampMeaning = "stored exchange ledger event time in Unix milliseconds"
	ledgerEventTimestampMeaning  = "ledger_record_time"
	ledgerCaptureConsistency     = "transactional_per_file"
	ledgerConfigBasis            = "current_at_capture"
	ledgerATRMethodRule          = "strategy atr_method, else root atr_method, else simple"
	ledgerWalletOwnership        = "live_wallet"
	ledgerWalletAllocation       = "unallocated"
	ledgerExportPlatform         = "hyperliquid"
)

type ledgerProvenance struct {
	Kind        string `json:"kind"`
	SourceRole  string `json:"source_role"`
	SourceTable string `json:"source_table"`
	SourceRowID string `json:"source_row_id"`
	SourceField string `json:"source_field"`
}

type ledgerEvidence[T any] struct {
	Value      *T                 `json:"value"`
	RawValue   *T                 `json:"raw_value"`
	Status     string             `json:"status"`
	Reason     *string            `json:"reason"`
	Provenance []ledgerProvenance `json:"provenance"`
}

type ledgerEvent struct {
	EventKey          string `json:"event_key"`
	SourceRole        string `json:"source_role"`
	SourceTable       string `json:"source_table"`
	SourceRowID       string `json:"source_row_id"`
	Partition         string `json:"partition"`
	ProcessStrategyID string `json:"process_strategy_id"`
	StorageStrategyID string `json:"storage_strategy_id"`
	TimestampRaw      string `json:"timestamp_raw"`
	Timestamp         string `json:"timestamp"`
	TimestampMeaning  string `json:"timestamp_meaning"`

	Symbol             ledgerEvidence[string]  `json:"symbol"`
	Side               ledgerEvidence[string]  `json:"side"`
	TradeType          ledgerEvidence[string]  `json:"trade_type"`
	Details            ledgerEvidence[string]  `json:"details"`
	PositionID         ledgerEvidence[string]  `json:"position_id"`
	ExchangeOrderID    ledgerEvidence[string]  `json:"exchange_order_id"`
	FeeSource          ledgerEvidence[string]  `json:"fee_source"`
	Regime             ledgerEvidence[string]  `json:"regime"`
	Quantity           ledgerEvidence[float64] `json:"quantity"`
	Price              ledgerEvidence[float64] `json:"price"`
	Value              ledgerEvidence[float64] `json:"value"`
	ExchangeFee        ledgerEvidence[float64] `json:"exchange_fee"`
	RealizedPnL        ledgerEvidence[float64] `json:"realized_pnl"`
	IsClose            ledgerEvidence[bool]    `json:"is_close"`
	PnLGross           ledgerEvidence[bool]    `json:"pnl_gross"`
	Manual             ledgerEvidence[bool]    `json:"manual"`
	RowNetPnL          ledgerEvidence[float64] `json:"row_net_pnl"`
	LedgerDelta        ledgerEvidence[float64] `json:"ledger_delta"`
	EventKind          ledgerEvidence[string]  `json:"event_kind"`
	CloseReason        ledgerEvidence[string]  `json:"close_reason"`
	CloseExtent        ledgerEvidence[string]  `json:"close_extent"`
	PositionAllocation ledgerEvidence[string]  `json:"position_allocation"`
	EntryATR           ledgerEvidence[float64] `json:"entry_atr"`
	StopLossATRMult    ledgerEvidence[float64] `json:"stop_loss_atr_mult"`
	StopLossTriggerPx  ledgerEvidence[float64] `json:"stop_loss_trigger_px"`
	StopLossOID        ledgerEvidence[string]  `json:"stop_loss_oid"`
	TPOIDsJSON         ledgerEvidence[string]  `json:"tp_oids_json"`
	TPTiersJSON        ledgerEvidence[string]  `json:"tp_tiers_json"`
	CostModelVersion   ledgerEvidence[int64]   `json:"cost_model_version"`
}

type ledgerTimestampMeanings struct {
	Trades          string `json:"trades"`
	WalletTransfers string `json:"wallet_transfers"`
}

type ledgerSelection struct {
	Partition         string `json:"partition"`
	ProcessStrategyID string `json:"process_strategy_id"`
	StorageStrategyID string `json:"storage_strategy_id"`
	SourceRole        string `json:"source_role"`
	Platform          string `json:"platform"`
}

type ledgerCaptureInfo struct {
	StartedAt      string  `json:"started_at"`
	CompletedAt    string  `json:"completed_at"`
	SourceRevision *string `json:"source_revision"`
	Consistency    string  `json:"consistency"`
}

type ledgerSnapshotFile struct {
	SourceRole   string `json:"source_role"`
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
}

type ledgerEffectiveConfig struct {
	Basis         string           `json:"basis"`
	ConfigVersion int              `json:"config_version"`
	Strategy      json.RawMessage  `json:"strategy"`
	Regime        json.RawMessage  `json:"regime"`
	PortfolioRisk json.RawMessage  `json:"portfolio_risk"`
	ATRMethod     *ledgerATRMethod `json:"atr_method,omitempty"`
}

type ledgerATRMethod struct {
	Strategy *string `json:"strategy"`
	Root     *string `json:"root"`
	Resolved string  `json:"resolved"`
	Rule     string  `json:"rule"`
}

type ledgerWalletOrphanRecord struct {
	EventKey          string             `json:"event_key"`
	SourceRole        string             `json:"source_role"`
	SourceTable       string             `json:"source_table"`
	SourceRowID       string             `json:"source_row_id"`
	Platform          string             `json:"platform"`
	Account           string             `json:"account"`
	Kind              string             `json:"kind"`
	DedupID           string             `json:"dedup_id"`
	Timestamp         string             `json:"timestamp"`
	TimeMS            string             `json:"time_ms"`
	AmountUSD         float64            `json:"amount_usd"`
	ProcessStrategyID *string            `json:"process_strategy_id"`
	StorageStrategyID *string            `json:"storage_strategy_id"`
	PositionID        *string            `json:"position_id"`
	Provenance        []ledgerProvenance `json:"provenance"`
}

type ledgerWalletOrphanContext struct {
	Status     string                     `json:"status"`
	Reason     *string                    `json:"reason"`
	Ownership  string                     `json:"ownership"`
	Allocation string                     `json:"allocation"`
	Records    []ledgerWalletOrphanRecord `json:"records"`
}

type ledgerExportDocument struct {
	Schema                        string                    `json:"schema"`
	SchemaVersion                 int                       `json:"schema_version"`
	InspectedRevision             *string                   `json:"inspected_revision"`
	CaptureManifestSHA256         string                    `json:"capture_manifest_sha256"`
	TimeBasis                     string                    `json:"time_basis"`
	TimestampMeanings             ledgerTimestampMeanings   `json:"timestamp_meanings"`
	Selection                     ledgerSelection           `json:"selection"`
	Capture                       ledgerCaptureInfo         `json:"capture"`
	SnapshotFiles                 []ledgerSnapshotFile      `json:"snapshot_files"`
	CurrentEffectiveConfiguration ledgerEffectiveConfig     `json:"current_effective_configuration"`
	Events                        []ledgerEvent             `json:"events"`
	WalletOrphanContext           ledgerWalletOrphanContext `json:"wallet_orphan_context"`
}

func ledgerReason(r string) *string {
	return &r
}

func ledgerRowIDText(id int64) string {
	return strconv.FormatInt(id, 10)
}

func ledgerStoredProvenance(role storageRole, table string, rowID int64, field string) []ledgerProvenance {
	return []ledgerProvenance{{
		Kind:        ledgerProvenanceStored,
		SourceRole:  string(role),
		SourceTable: table,
		SourceRowID: ledgerRowIDText(rowID),
		SourceField: field,
	}}
}

func ledgerAvailable[T any](v T, raw T, prov []ledgerProvenance) ledgerEvidence[T] {
	return ledgerEvidence[T]{Value: &v, RawValue: &raw, Status: ledgerStatusAvailable, Provenance: prov}
}

func ledgerDerived[T any](v T, prov []ledgerProvenance) ledgerEvidence[T] {
	return ledgerEvidence[T]{Value: &v, Status: ledgerStatusAvailable, Provenance: prov}
}

func ledgerUnavailable[T any](raw *T, reason string, prov []ledgerProvenance) ledgerEvidence[T] {
	if prov == nil {
		prov = []ledgerProvenance{}
	}
	return ledgerEvidence[T]{RawValue: raw, Status: ledgerStatusUnavailable, Reason: ledgerReason(reason), Provenance: prov}
}

func ledgerNotApplicable[T any]() ledgerEvidence[T] {
	return ledgerEvidence[T]{Status: ledgerStatusNotApplicable, Reason: ledgerReason(ledgerReasonNotApplicable), Provenance: []ledgerProvenance{}}
}

type ledgerFieldContext struct {
	role   storageRole
	rowID  int64
	row    ledgerTradeRow
	fields map[string]bool
}

func (c ledgerFieldContext) present(column string) bool {
	return c.fields[column]
}

func (c ledgerFieldContext) stored(column string) []ledgerProvenance {
	return ledgerStoredProvenance(c.role, "trades", c.rowID, column)
}

func (c ledgerFieldContext) text(column string) (string, bool, error) {
	v, ok := c.row.Values[column]
	if !ok || v == nil {
		return "", false, nil
	}
	switch t := v.(type) {
	case string:
		return t, true, nil
	case []byte:
		return string(t), true, nil
	default:
		return "", false, fmt.Errorf("trades row %d column %s holds %T, want text", c.rowID, column, v)
	}
}

func (c ledgerFieldContext) number(column string) (float64, bool, error) {
	v, ok := c.row.Values[column]
	if !ok || v == nil {
		return 0, false, nil
	}
	var f float64
	switch t := v.(type) {
	case int64:
		f = float64(t)
	case float64:
		f = t
	default:
		return 0, false, fmt.Errorf("trades row %d column %s holds %T, want a number", c.rowID, column, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, fmt.Errorf("trades row %d column %s holds a non-finite number", c.rowID, column)
	}
	return f, true, nil
}

func (c ledgerFieldContext) integer(column string) (int64, bool, error) {
	v, ok := c.row.Values[column]
	if !ok || v == nil {
		return 0, false, nil
	}
	t, isInt := v.(int64)
	if !isInt {
		return 0, false, fmt.Errorf("trades row %d column %s holds %T, want an integer", c.rowID, column, v)
	}
	return t, true, nil
}

func (c ledgerFieldContext) boolean(column string) (bool, bool, error) {
	v, ok := c.row.Values[column]
	if !ok || v == nil {
		return false, false, nil
	}
	t, isInt := v.(int64)
	if !isInt || (t != 0 && t != 1) {
		return false, false, fmt.Errorf("trades row %d column %s holds malformed Boolean storage %v (%T)", c.rowID, column, v, v)
	}
	return t == 1, true, nil
}

func (c ledgerFieldContext) textEvidence(column, emptyReason string) (ledgerEvidence[string], error) {
	if !c.present(column) {
		return ledgerUnavailable[string](nil, ledgerReasonColumnAbsent, nil), nil
	}
	s, ok, err := c.text(column)
	if err != nil {
		return ledgerEvidence[string]{}, err
	}
	if !ok {
		return ledgerUnavailable[string](nil, ledgerReasonStoredNull, c.stored(column)), nil
	}
	if strings.TrimSpace(s) == "" {
		return ledgerUnavailable(&s, emptyReason, c.stored(column)), nil
	}
	return ledgerAvailable(s, s, c.stored(column)), nil
}

func (c ledgerFieldContext) amountEvidence(column string) (ledgerEvidence[float64], error) {
	if !c.present(column) {
		return ledgerUnavailable[float64](nil, ledgerReasonColumnAbsent, nil), nil
	}
	f, ok, err := c.number(column)
	if err != nil {
		return ledgerEvidence[float64]{}, err
	}
	if !ok {
		return ledgerUnavailable[float64](nil, ledgerReasonStoredNull, c.stored(column)), nil
	}
	return ledgerAvailable(f, f, c.stored(column)), nil
}

func (c ledgerFieldContext) stampEvidence(column string) (ledgerEvidence[float64], error) {
	if !c.present(column) {
		return ledgerUnavailable[float64](nil, ledgerReasonColumnAbsent, nil), nil
	}
	f, ok, err := c.number(column)
	if err != nil {
		return ledgerEvidence[float64]{}, err
	}
	if !ok {
		return ledgerUnavailable[float64](nil, ledgerReasonStoredNull, c.stored(column)), nil
	}
	if f == 0 {
		return ledgerUnavailable(&f, ledgerReasonUnstamped, c.stored(column)), nil
	}
	return ledgerAvailable(f, f, c.stored(column)), nil
}

func (c ledgerFieldContext) boolEvidence(column string) (ledgerEvidence[bool], error) {
	if !c.present(column) {
		return ledgerUnavailable[bool](nil, ledgerReasonColumnAbsent, nil), nil
	}
	b, ok, err := c.boolean(column)
	if err != nil {
		return ledgerEvidence[bool]{}, err
	}
	if !ok {
		return ledgerUnavailable[bool](nil, ledgerReasonStoredNull, c.stored(column)), nil
	}
	return ledgerAvailable(b, b, c.stored(column)), nil
}

func (c ledgerFieldContext) costModelEvidence(column string) (ledgerEvidence[int64], error) {
	if !c.present(column) {
		return ledgerUnavailable[int64](nil, ledgerReasonColumnAbsent, nil), nil
	}
	n, ok, err := c.integer(column)
	if err != nil {
		return ledgerEvidence[int64]{}, err
	}
	if !ok {
		return ledgerUnavailable[int64](nil, ledgerReasonStoredNull, c.stored(column)), nil
	}
	if n < 0 {
		return ledgerEvidence[int64]{}, fmt.Errorf("trades row %d column %s holds a negative cost model version %d", c.rowID, column, n)
	}
	if n == 0 {
		return ledgerUnavailable(&n, ledgerReasonUnstamped, c.stored(column)), nil
	}
	return ledgerAvailable(n, n, c.stored(column)), nil
}

func (c ledgerFieldContext) oidEvidence(column string) (ledgerEvidence[string], error) {
	if !c.present(column) {
		return ledgerUnavailable[string](nil, ledgerReasonColumnAbsent, nil), nil
	}
	n, ok, err := c.integer(column)
	if err != nil {
		return ledgerEvidence[string]{}, err
	}
	if !ok {
		return ledgerUnavailable[string](nil, ledgerReasonStoredNull, c.stored(column)), nil
	}
	s := strconv.FormatInt(n, 10)
	if n == 0 {
		return ledgerUnavailable(&s, ledgerReasonUnstamped, c.stored(column)), nil
	}
	return ledgerAvailable(s, s, c.stored(column)), nil
}

func (c ledgerFieldContext) geometryJSONEvidence(column string, integers bool) (ledgerEvidence[string], error) {
	ev, err := c.textEvidence(column, ledgerReasonUnstamped)
	if err != nil || ev.Status != ledgerStatusAvailable {
		return ev, err
	}
	raw := *ev.Value
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return ledgerEvidence[string]{}, fmt.Errorf("trades row %d column %s holds corrupt JSON geometry: %v", c.rowID, column, err)
	}
	for i, elem := range arr {
		if integers {
			var n int64
			if err := json.Unmarshal(elem, &n); err != nil {
				return ledgerEvidence[string]{}, fmt.Errorf("trades row %d column %s element %d is not an integer order id: %v", c.rowID, column, i, err)
			}
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(elem, &obj); err != nil || obj == nil {
			return ledgerEvidence[string]{}, fmt.Errorf("trades row %d column %s element %d is not a tier object", c.rowID, column, i)
		}
	}
	return ev, nil
}

func parseLedgerTimestamp(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func formatLedgerTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

type ledgerCloseMatch struct {
	diag   ledgerDiagnosticRow
	status string
	reason string
}

func buildLedgerEvents(read *ledgerFileRead, partition RiskPartition, procID string) ([]ledgerEvent, error) {
	events := make([]ledgerEvent, 0, len(read.Trades))
	matches, err := matchLedgerCloseEvidence(read)
	if err != nil {
		return nil, err
	}
	for _, row := range read.Trades {
		ev, err := buildLedgerEvent(read, row, partition, procID, matches[row.RowID])
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func buildLedgerEvent(read *ledgerFileRead, row ledgerTradeRow, partition RiskPartition, procID string, match ledgerCloseMatch) (ledgerEvent, error) {
	c := ledgerFieldContext{role: read.Role, rowID: row.RowID, row: row, fields: read.Columns}
	storedID, ok, err := c.text("strategy_id")
	if err != nil {
		return ledgerEvent{}, err
	}
	if !ok || storedID != read.StorageID {
		return ledgerEvent{}, fmt.Errorf("trades row %d is not stored under strategy %q", row.RowID, read.StorageID)
	}
	tsRaw, ok, err := c.text("timestamp")
	if err != nil {
		return ledgerEvent{}, err
	}
	if !ok {
		return ledgerEvent{}, fmt.Errorf("trades row %d has a NULL timestamp", row.RowID)
	}
	ts, err := parseLedgerTimestamp(tsRaw)
	if err != nil {
		return ledgerEvent{}, fmt.Errorf("trades row %d has an invalid timestamp %q: %v", row.RowID, tsRaw, err)
	}
	rowText := ledgerRowIDText(row.RowID)
	ev := ledgerEvent{
		EventKey:          string(read.Role) + "/trades/" + rowText,
		SourceRole:        string(read.Role),
		SourceTable:       "trades",
		SourceRowID:       rowText,
		Partition:         partition.String(),
		ProcessStrategyID: procID,
		StorageStrategyID: storedID,
		TimestampRaw:      tsRaw,
		Timestamp:         formatLedgerTimestamp(ts),
		TimestampMeaning:  ledgerEventTimestampMeaning,
	}
	type textField struct {
		dst    *ledgerEvidence[string]
		column string
		empty  string
	}
	for _, f := range []textField{
		{&ev.Symbol, "symbol", ledgerReasonNotRecorded},
		{&ev.Side, "side", ledgerReasonNotRecorded},
		{&ev.TradeType, "trade_type", ledgerReasonNotRecorded},
		{&ev.Details, "details", ledgerReasonNotRecorded},
		{&ev.PositionID, "position_id", ledgerReasonNotRecorded},
		{&ev.ExchangeOrderID, "exchange_order_id", ledgerReasonNotRecorded},
		{&ev.FeeSource, "fee_source", ledgerReasonUnstamped},
		{&ev.Regime, "regime", ledgerReasonUnstamped},
	} {
		if *f.dst, err = c.textEvidence(f.column, f.empty); err != nil {
			return ledgerEvent{}, err
		}
	}
	type amountField struct {
		dst    *ledgerEvidence[float64]
		column string
	}
	for _, f := range []amountField{
		{&ev.Quantity, "quantity"},
		{&ev.Price, "price"},
		{&ev.Value, "value"},
		{&ev.ExchangeFee, "exchange_fee"},
		{&ev.RealizedPnL, "realized_pnl"},
	} {
		if *f.dst, err = c.amountEvidence(f.column); err != nil {
			return ledgerEvent{}, err
		}
	}
	type boolField struct {
		dst    *ledgerEvidence[bool]
		column string
	}
	for _, f := range []boolField{
		{&ev.IsClose, "is_close"},
		{&ev.PnLGross, "pnl_gross"},
		{&ev.Manual, "manual"},
	} {
		if *f.dst, err = c.boolEvidence(f.column); err != nil {
			return ledgerEvent{}, err
		}
	}
	if ev.ExchangeFee.Status != ledgerStatusAvailable || ev.RealizedPnL.Status != ledgerStatusAvailable ||
		ev.IsClose.Status != ledgerStatusAvailable || ev.PnLGross.Status != ledgerStatusAvailable {
		return ledgerEvent{}, fmt.Errorf("trades row %d lacks a mandatory accounting value (exchange_fee, realized_pnl, is_close, pnl_gross)", row.RowID)
	}
	acct := Trade{
		RealizedPnL: *ev.RealizedPnL.Value,
		ExchangeFee: *ev.ExchangeFee.Value,
		IsClose:     *ev.IsClose.Value,
		PnLGross:    *ev.PnLGross.Value,
	}
	net := tradeNetPnL(acct)
	delta := tradeLedgerDelta(acct)
	if math.IsNaN(net) || math.IsInf(net, 0) || math.IsNaN(delta) || math.IsInf(delta, 0) {
		return ledgerEvent{}, fmt.Errorf("trades row %d accounting is not finite", row.RowID)
	}
	ev.RowNetPnL = ledgerDerived(net, []ledgerProvenance{{
		Kind: ledgerProvenanceDerived, SourceRole: string(read.Role), SourceTable: "trades", SourceRowID: rowText,
		SourceField: "tradeNetPnL(pnl_gross, realized_pnl, exchange_fee)",
	}})
	ev.LedgerDelta = ledgerDerived(delta, []ledgerProvenance{{
		Kind: ledgerProvenanceDerived, SourceRole: string(read.Role), SourceTable: "trades", SourceRowID: rowText,
		SourceField: "tradeLedgerDelta(pnl_gross, is_close, realized_pnl, exchange_fee)",
	}})

	kind := ledgerEventKind(ev)
	ev.EventKind = ledgerDerived(kind, []ledgerProvenance{{
		Kind: ledgerProvenanceDerived, SourceRole: string(read.Role), SourceTable: "trades", SourceRowID: rowText,
		SourceField: "event_kind(trade_type, is_close, side)",
	}})
	alloc := "unallocated"
	if ev.PositionID.Status == ledgerStatusAvailable {
		alloc = "recorded"
	}
	ev.PositionAllocation = ledgerDerived(alloc, []ledgerProvenance{{
		Kind: ledgerProvenanceDerived, SourceRole: string(read.Role), SourceTable: "trades", SourceRowID: rowText,
		SourceField: "position_allocation(position_id)",
	}})

	if kind != "close" {
		ev.CloseReason = ledgerNotApplicable[string]()
		ev.CloseExtent = ledgerNotApplicable[string]()
	} else if match.status == ledgerStatusAvailable {
		prov := []ledgerProvenance{{
			Kind: ledgerProvenanceMatched, SourceRole: string(read.Role), SourceTable: "trade_diagnostics",
			SourceRowID: ledgerRowIDText(match.diag.RowID),
			SourceField: "close_reason; unique match on strategy_id, position_id, symbol, closed_at, quantity, exit_price",
		}}
		reason := match.diag.CloseReason
		ev.CloseReason = ledgerAvailable(reason, reason, prov)
		extentProv := []ledgerProvenance{{
			Kind: ledgerProvenanceMatched, SourceRole: string(read.Role), SourceTable: "trade_diagnostics",
			SourceRowID: ledgerRowIDText(match.diag.RowID),
			SourceField: "full: the position summary's closed quantity equals this close row's quantity",
		}}
		ev.CloseExtent = ledgerDerived("full", extentProv)
	} else {
		r := match.reason
		if r == "" {
			r = ledgerReasonNotRecorded
		}
		ev.CloseReason = ledgerUnavailable[string](nil, r, nil)
		ev.CloseExtent = ledgerUnavailable[string](nil, r, nil)
	}

	if ev.EntryATR, err = c.stampEvidence("entry_atr"); err != nil {
		return ledgerEvent{}, err
	}
	if ev.StopLossTriggerPx, err = c.stampEvidence("stop_loss_trigger_px"); err != nil {
		return ledgerEvent{}, err
	}
	if !c.present("stop_loss_atr_mult") {
		ev.StopLossATRMult = ledgerUnavailable[float64](nil, ledgerReasonColumnAbsent, nil)
	} else if f, ok, err := c.number("stop_loss_atr_mult"); err != nil {
		return ledgerEvent{}, err
	} else if !ok {
		ev.StopLossATRMult = ledgerUnavailable[float64](nil, ledgerReasonUnstamped, c.stored("stop_loss_atr_mult"))
	} else {
		ev.StopLossATRMult = ledgerAvailable(f, f, c.stored("stop_loss_atr_mult"))
	}
	if ev.StopLossOID, err = c.oidEvidence("stop_loss_oid"); err != nil {
		return ledgerEvent{}, err
	}
	if ev.TPOIDsJSON, err = c.geometryJSONEvidence("tp_oids_json", true); err != nil {
		return ledgerEvent{}, err
	}
	if ev.TPTiersJSON, err = c.geometryJSONEvidence("tp_tiers_json", false); err != nil {
		return ledgerEvent{}, err
	}
	if ev.CostModelVersion, err = c.costModelEvidence("cost_model_version"); err != nil {
		return ledgerEvent{}, err
	}
	return ev, nil
}

func ledgerEventKind(ev ledgerEvent) string {
	tradeType := ""
	if ev.TradeType.Status == ledgerStatusAvailable {
		tradeType = strings.ToLower(strings.TrimSpace(*ev.TradeType.Value))
	}
	side := ""
	if ev.Side.Status == ledgerStatusAvailable {
		side = strings.ToLower(strings.TrimSpace(*ev.Side.Value))
	}
	switch {
	case tradeType == TradeTypeFunding || side == "funding":
		return "funding"
	case tradeType == scaleInTradeType:
		return "scale_in"
	case *ev.IsClose.Value:
		return "close"
	case side == "buy" || side == "sell":
		return "non_close"
	default:
		return "other"
	}
}

func matchLedgerCloseEvidence(read *ledgerFileRead) (map[int64]ledgerCloseMatch, error) {
	out := make(map[int64]ledgerCloseMatch, len(read.Trades))
	if !read.DiagnosticsPresent {
		for _, row := range read.Trades {
			out[row.RowID] = ledgerCloseMatch{status: ledgerStatusUnavailable, reason: ledgerReasonNotRecorded}
		}
		return out, nil
	}
	type closeKey struct {
		row  ledgerTradeRow
		pos  string
		sym  string
		ts   time.Time
		qty  float64
		px   float64
		good bool
	}
	keys := make([]closeKey, 0, len(read.Trades))
	for _, row := range read.Trades {
		c := ledgerFieldContext{role: read.Role, rowID: row.RowID, row: row, fields: read.Columns}
		k := closeKey{row: row}
		isClose, ok, err := c.boolean("is_close")
		if err != nil {
			return nil, err
		}
		if !ok || !isClose {
			keys = append(keys, k)
			continue
		}
		pos, posOK, err := c.text("position_id")
		if err != nil {
			return nil, err
		}
		sym, symOK, err := c.text("symbol")
		if err != nil {
			return nil, err
		}
		tsRaw, tsOK, err := c.text("timestamp")
		if err != nil {
			return nil, err
		}
		qty, qtyOK, err := c.number("quantity")
		if err != nil {
			return nil, err
		}
		px, pxOK, err := c.number("price")
		if err != nil {
			return nil, err
		}
		if !posOK || strings.TrimSpace(pos) == "" || !symOK || !tsOK || !qtyOK || !pxOK {
			keys = append(keys, k)
			continue
		}
		ts, err := parseLedgerTimestamp(tsRaw)
		if err != nil {
			keys = append(keys, k)
			continue
		}
		k.pos, k.sym, k.ts, k.qty, k.px, k.good = pos, sym, ts, qty, px, true
		keys = append(keys, k)
	}
	claims := make(map[int64]int)
	candidates := make(map[int64][]ledgerDiagnosticRow)
	ambiguous := make(map[int64]bool)
	for _, k := range keys {
		if !k.good {
			continue
		}
		for _, d := range read.Diagnostics {
			if d.StorageID != read.StorageID || d.PositionID != k.pos || d.Symbol != k.sym {
				continue
			}
			if !d.ClosedAtValid {
				ambiguous[k.row.RowID] = true
				continue
			}
			if !d.ClosedAt.Equal(k.ts) || d.Quantity != k.qty || d.ExitPrice != k.px {
				continue
			}
			candidates[k.row.RowID] = append(candidates[k.row.RowID], d)
		}
		if len(candidates[k.row.RowID]) == 1 {
			claims[candidates[k.row.RowID][0].RowID]++
		}
	}
	for _, k := range keys {
		id := k.row.RowID
		cands := candidates[id]
		switch {
		case !k.good:
			out[id] = ledgerCloseMatch{status: ledgerStatusUnavailable, reason: ledgerReasonNotRecorded}
		case len(cands) > 1 || ambiguous[id]:
			out[id] = ledgerCloseMatch{status: ledgerStatusUnavailable, reason: ledgerReasonAmbiguousEvidence}
		case len(cands) == 0:
			out[id] = ledgerCloseMatch{status: ledgerStatusUnavailable, reason: ledgerReasonNotRecorded}
		case claims[cands[0].RowID] > 1:
			out[id] = ledgerCloseMatch{status: ledgerStatusUnavailable, reason: ledgerReasonAmbiguousEvidence}
		case strings.TrimSpace(cands[0].CloseReason) == "":
			out[id] = ledgerCloseMatch{status: ledgerStatusUnavailable, reason: ledgerReasonNotRecorded}
		default:
			out[id] = ledgerCloseMatch{status: ledgerStatusAvailable, diag: cands[0]}
		}
	}
	return out, nil
}

func buildLedgerWalletContext(read *ledgerWalletRead, partition RiskPartition) (ledgerWalletOrphanContext, error) {
	ctx := ledgerWalletOrphanContext{
		Ownership:  ledgerWalletOwnership,
		Allocation: ledgerWalletAllocation,
		Records:    []ledgerWalletOrphanRecord{},
	}
	if !partition.IsLive() {
		ctx.Status = ledgerStatusNotApplicable
		ctx.Reason = ledgerReason(ledgerReasonNotApplicable)
		return ctx, nil
	}
	if read == nil || !read.TablePresent {
		ctx.Status = ledgerStatusUnavailable
		ctx.Reason = ledgerReason(ledgerReasonColumnAbsent)
		return ctx, nil
	}
	ctx.Status = ledgerStatusAvailable
	for _, r := range read.Rows {
		if math.IsNaN(r.AmountUSD) || math.IsInf(r.AmountUSD, 0) {
			return ledgerWalletOrphanContext{}, fmt.Errorf("wallet_transfers row %d amount_usd is not finite", r.RowID)
		}
		rowText := ledgerRowIDText(r.RowID)
		prov := []ledgerProvenance{}
		for _, field := range []string{"platform", "account", "time_ms", "kind", "amount_usd", "dedup_id"} {
			prov = append(prov, ledgerProvenance{Kind: ledgerProvenanceStored, SourceRole: string(read.Role), SourceTable: "wallet_transfers", SourceRowID: rowText, SourceField: field})
		}
		ctx.Records = append(ctx.Records, ledgerWalletOrphanRecord{
			EventKey:    string(read.Role) + "/wallet_transfers/" + rowText,
			SourceRole:  string(read.Role),
			SourceTable: "wallet_transfers",
			SourceRowID: rowText,
			Platform:    r.Platform,
			Account:     r.Account,
			Kind:        r.Kind,
			DedupID:     r.DedupID,
			Timestamp:   formatLedgerTimestamp(time.UnixMilli(r.TimeMS)),
			TimeMS:      strconv.FormatInt(r.TimeMS, 10),
			AmountUSD:   r.AmountUSD,
			Provenance:  prov,
		})
	}
	return ctx, nil
}
