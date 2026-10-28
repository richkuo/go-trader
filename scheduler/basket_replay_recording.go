package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	pairRecSchemaVersion   = 1
	pairRecSchemaAsync     = 2
	pairRecMaxPayloadBytes = 16 << 20
	pairRecMaxStdoutBytes  = 4 << 20
	pairRecMaxStderrBytes  = 64 << 10

	pairRecEventBind     = "bind"
	pairRecEventEpoch    = "epoch"
	pairRecEventStart    = "call_start"
	pairRecEventEnd      = "call_end"
	pairRecEventRecovery = "call_recovery"
	pairRecEventWake     = "host_wake"
	pairRecEventBudget   = "budget"
	pairRecEventFailure  = "record_failure"
	pairRecEventClear    = "barrier_clear"
	pairRecEventSeal     = "seal"
	pairRecEventConsume  = "call_consume"

	pairRecPhaseStep      = "step"
	pairRecPhaseBootstrap = "bootstrap"

	pairRecClassResult    = "result"
	pairRecClassInfra     = "infrastructure_failure"
	pairRecClassCancelled = "cancelled"

	pairRecKindCollect       = "collect"
	pairRecKindReference     = "reference"
	pairRecKindCapability    = "capability"
	pairRecKindAccount       = "account"
	pairRecKindConversions   = "conversions"
	pairRecKindPosition      = "position"
	pairRecKindCandles       = "management_candles"
	pairRecKindMarks         = "marks"
	pairRecKindFunding       = "funding"
	pairRecKindFundingOracle = "funding_oracle"
	pairRecKindMatchPoll     = "match_poll"
	pairRecKindPolicy        = "policy"
	pairRecKindNativeCheck   = "native_check"
	pairRecKindMemberRead    = "member_read"
)

var pairRecCallKinds = map[string]bool{
	pairRecKindCollect: true, pairRecKindReference: true, pairRecKindCapability: true, pairRecKindAccount: true, pairRecKindConversions: true,
	pairRecKindPosition: true, pairRecKindCandles: true, pairRecKindMarks: true, pairRecKindFunding: true, pairRecKindFundingOracle: true, pairRecKindMatchPoll: true,
	pairRecKindPolicy: true, pairRecKindNativeCheck: true, pairRecKindMemberRead: true,
}

var pairRecProcessKinds = map[string]bool{pairRecKindPolicy: true, pairRecKindNativeCheck: true}

var pairRecEventKinds = map[string]bool{
	pairRecEventBind: true, pairRecEventEpoch: true, pairRecEventStart: true, pairRecEventEnd: true, pairRecEventRecovery: true,
	pairRecEventWake: true, pairRecEventBudget: true, pairRecEventFailure: true, pairRecEventClear: true, pairRecEventSeal: true,
	pairRecEventConsume: true,
}

var pairRecClasses = map[string]bool{pairRecClassResult: true, pairRecClassInfra: true, pairRecClassCancelled: true}

const pairRecordingSchemaDDL = `
CREATE TABLE IF NOT EXISTS basket_replay_inputs (
    run_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    schema_version INTEGER NOT NULL,
    partition TEXT NOT NULL,
    contract_fingerprint TEXT NOT NULL,
    event_kind TEXT NOT NULL,
    call_id TEXT NOT NULL DEFAULT '',
    prev_prefix_hash TEXT NOT NULL,
    prefix_hash TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    payload BLOB NOT NULL,
    PRIMARY KEY (run_id, seq)
);

CREATE TRIGGER IF NOT EXISTS basket_replay_inputs_immutable BEFORE UPDATE ON basket_replay_inputs
BEGIN SELECT RAISE(ABORT, 'basket_replay_inputs rows are immutable'); END;

CREATE TRIGGER IF NOT EXISTS basket_replay_inputs_append_only BEFORE DELETE ON basket_replay_inputs
BEGIN SELECT RAISE(ABORT, 'basket_replay_inputs rows are append-only'); END;

CREATE TABLE IF NOT EXISTS basket_replay_calls (
    run_id TEXT NOT NULL,
    call_id TEXT NOT NULL,
    parent_id TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    phase TEXT NOT NULL,
    member_id TEXT NOT NULL DEFAULT '',
    slot_ms INTEGER NOT NULL DEFAULT 0,
    action_id TEXT NOT NULL DEFAULT '',
    step_id TEXT NOT NULL DEFAULT '',
    request_hash TEXT NOT NULL,
    start_seq INTEGER NOT NULL,
    end_seq INTEGER NOT NULL DEFAULT 0,
    end_event TEXT NOT NULL DEFAULT '',
    outcome_class TEXT NOT NULL DEFAULT '',
    result_hash TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, call_id)
);

CREATE TRIGGER IF NOT EXISTS basket_replay_calls_terminal_once BEFORE UPDATE ON basket_replay_calls
WHEN OLD.end_seq != 0 OR NEW.end_seq <= OLD.start_seq OR NEW.call_id != OLD.call_id OR NEW.run_id != OLD.run_id OR NEW.parent_id != OLD.parent_id
  OR NEW.kind != OLD.kind OR NEW.phase != OLD.phase OR NEW.member_id != OLD.member_id OR NEW.slot_ms != OLD.slot_ms OR NEW.action_id != OLD.action_id
  OR NEW.step_id != OLD.step_id OR NEW.request_hash != OLD.request_hash OR NEW.start_seq != OLD.start_seq
BEGIN SELECT RAISE(ABORT, 'a basket_replay_calls row takes one terminal transition and is otherwise immutable'); END;

CREATE TRIGGER IF NOT EXISTS basket_replay_calls_append_only BEFORE DELETE ON basket_replay_calls
BEGIN SELECT RAISE(ABORT, 'basket_replay_calls rows are append-only'); END;

CREATE TABLE IF NOT EXISTS basket_replay_slots (
    run_id TEXT NOT NULL,
    pair_id TEXT NOT NULL,
    slot_ms INTEGER NOT NULL,
    collect_call_id TEXT NOT NULL,
    input_fingerprint TEXT NOT NULL,
    envelope_sha256 TEXT NOT NULL,
    start_ms INTEGER NOT NULL,
    cutoff_ms INTEGER NOT NULL,
    deadline_ms INTEGER NOT NULL,
    prefix_seq INTEGER NOT NULL,
    prefix_hash TEXT NOT NULL,
    PRIMARY KEY (run_id, pair_id, slot_ms)
);

CREATE TRIGGER IF NOT EXISTS basket_replay_slots_immutable BEFORE UPDATE ON basket_replay_slots
BEGIN SELECT RAISE(ABORT, 'basket_replay_slots rows are immutable'); END;

CREATE TRIGGER IF NOT EXISTS basket_replay_slots_append_only BEFORE DELETE ON basket_replay_slots
BEGIN SELECT RAISE(ABORT, 'basket_replay_slots rows are append-only'); END;
`

func (sdb *StateDB) ensurePairRecordingSchema() error {
	if _, err := sdb.db.Exec(pairRecordingSchemaDDL); err != nil {
		return fmt.Errorf("create the paper recording tables: %w", err)
	}
	return nil
}

type pairRecEvent struct {
	V        int              `json:"v"`
	Kind     string           `json:"kind"`
	CallID   string           `json:"call_id,omitempty"`
	Bind     *pairRecBind     `json:"bind,omitempty"`
	Epoch    *pairRecEpoch    `json:"epoch,omitempty"`
	Start    *pairRecStart    `json:"start,omitempty"`
	End      *pairRecEnd      `json:"end,omitempty"`
	Recovery *pairRecRecovery `json:"recovery,omitempty"`
	Wake     *pairRecWake     `json:"wake,omitempty"`
	Budget   *pairRecBudget   `json:"budget,omitempty"`
	Failure  *pairRecFailure  `json:"failure,omitempty"`
	Clear    *pairRecClear    `json:"clear,omitempty"`
	Seal     *pairRecSeal     `json:"seal,omitempty"`
	Consume  *pairRecConsume  `json:"consume,omitempty"`
}

type pairRecBind struct {
	RunID            string `json:"run_id"`
	PaperIdentity    string `json:"paper_identity"`
	ContractFP       string `json:"contract_fingerprint"`
	Partition        string `json:"partition"`
	SeedSHA256       string `json:"seed_sha256"`
	EvidenceIdentity string `json:"evidence_identity"`
	FundingBinding   string `json:"funding_binding"`
	AnchorMs         int64  `json:"anchor_ms"`
	Family           int    `json:"family,omitempty"`
}

type pairRecEpoch struct {
	Epoch  int    `json:"epoch"`
	WallMs int64  `json:"wall_ms"`
	Mode   string `json:"mode"`
}

type pairRecStart struct {
	ParentID        string `json:"parent_id,omitempty"`
	Kind            string `json:"kind"`
	Phase           string `json:"phase"`
	MemberID        string `json:"member_id,omitempty"`
	SlotMs          int64  `json:"slot_ms,omitempty"`
	ActionID        string `json:"action_id,omitempty"`
	StepID          string `json:"step_id,omitempty"`
	Ordinal         int    `json:"ordinal"`
	Request         []byte `json:"request"`
	RequestHash     string `json:"request_hash"`
	RawStartMs      int64  `json:"raw_start_ms"`
	LogicalBeforeMs int64  `json:"logical_before_ms"`
	Async           bool   `json:"async,omitempty"`
}

type pairRecConsume struct {
	RequestHash  string `json:"request_hash"`
	TerminalHash string `json:"terminal_hash"`
	TerminalKind string `json:"terminal_kind"`
	WakeSeq      int64  `json:"wake_seq"`
	StepID       string `json:"step_id"`
	LogicalMs    int64  `json:"logical_ms"`
	Disposition  string `json:"disposition"`
	Batch        []byte `json:"batch,omitempty"`
	BatchHash    string `json:"batch_hash,omitempty"`
}

type pairRecEnd struct {
	Class           string `json:"class"`
	Result          []byte `json:"result,omitempty"`
	ResultHash      string `json:"result_hash"`
	RawReceiptMs    int64  `json:"raw_receipt_ms,omitempty"`
	RawCompletionMs int64  `json:"raw_completion_ms"`
	LogicalMs       int64  `json:"logical_ms"`
	Cause           string `json:"cause,omitempty"`
	ExitStatus      int    `json:"exit_status"`
	Stdout          []byte `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	Overflow        bool   `json:"overflow,omitempty"`
	ClockBackward   bool   `json:"clock_backward,omitempty"`
}

type pairRecRecovery struct {
	Reason string `json:"reason"`
	Epoch  int    `json:"epoch"`
}

type pairRecWake struct {
	StepID        string `json:"step_id"`
	Ordinal       int    `json:"ordinal"`
	WallMs        int64  `json:"wall_ms"`
	LogicalMs     int64  `json:"logical_ms"`
	ClockBackward bool   `json:"clock_backward,omitempty"`
	Epoch         int    `json:"epoch"`
}

type pairRecBudget struct {
	Site      string `json:"site"`
	ElapsedNs int64  `json:"elapsed_ns"`
	Cause     string `json:"cause,omitempty"`
	StepID    string `json:"step_id,omitempty"`
}

type pairRecFailure struct {
	CallKind    string `json:"call_kind"`
	MemberID    string `json:"member_id,omitempty"`
	RequestHash string `json:"request_hash"`
	StepID      string `json:"step_id,omitempty"`
	Ordinal     int    `json:"ordinal"`
	Reason      string `json:"reason"`
	FailedSeq   int64  `json:"failed_seq"`
}

type pairRecClear struct {
	VerifiedThroughSeq int64  `json:"verified_through_seq"`
	FailedSeq          int64  `json:"failed_seq"`
	Reason             string `json:"reason"`
	Epoch              int    `json:"epoch"`
}

type pairRecSeal struct {
	FinalSeq         int64  `json:"final_seq"`
	FinalPrefixHash  string `json:"final_prefix_hash"`
	ExportManifest   string `json:"export_manifest_sha256"`
	CallCount        int    `json:"call_count"`
	ProcessFailures  int    `json:"process_failures"`
	InterruptedCalls int    `json:"interrupted_calls"`
}

type pairRecRow struct {
	Seq         int64
	Schema      int
	Partition   string
	ContractFP  string
	Kind        string
	CallID      string
	PrevPrefix  string
	Prefix      string
	PayloadHash string
	Payload     []byte
	Event       pairRecEvent
}

func pairRecPrefixHash(schema int, runID string, seq int64, partition, contractFP, kind, callID, prev, payloadHash string) string {
	if schema == 0 {
		schema = pairRecSchemaVersion
	}
	header := strings.Join([]string{"go-trader.paper-recording/1", runID, strconv.FormatInt(seq, 10), strconv.Itoa(schema), partition, contractFP, kind, callID, prev, payloadHash}, "\x1f")
	return sha256Hex([]byte(header))
}

func pairRecEncode(ev pairRecEvent) ([]byte, error) {
	schema := ev.V
	if schema == 0 {
		schema = pairRecSchemaVersion
	}
	return pairRecEncodeSchema(schema, ev)
}

func pairRecEncodeSchema(schema int, ev pairRecEvent) ([]byte, error) {
	if schema != pairRecSchemaVersion && schema != pairRecSchemaAsync {
		return nil, fmt.Errorf("recording schema %d is not 1 or 2", schema)
	}
	ev.V = schema
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	if len(b) > pairRecMaxPayloadBytes {
		return nil, fmt.Errorf("a %s recording event is %d bytes, above the %d-byte payload bound", ev.Kind, len(b), pairRecMaxPayloadBytes)
	}
	return b, nil
}

func pairRecDecode(b []byte) (pairRecEvent, error) {
	var ev pairRecEvent
	if err := pairReplayStrictDecode(b, &ev); err != nil {
		return ev, err
	}
	again, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	if !bytes.Equal(again, b) {
		return ev, fmt.Errorf("the recording event bytes are not their canonical encoding")
	}
	if (ev.V != pairRecSchemaVersion && ev.V != pairRecSchemaAsync) || !pairRecEventKinds[ev.Kind] {
		return ev, fmt.Errorf("the recording event is version %d kind %q", ev.V, ev.Kind)
	}
	return ev, nil
}

type pairRecHead struct {
	Seq    int64
	Prefix string
}

func pairRecInsertTx(tx *sql.Tx, runID, partition, contractFP string, head pairRecHead, schema int, ev pairRecEvent) (pairRecHead, error) {
	if schema == 0 {
		schema = pairRecSchemaVersion
	}
	payload, err := pairRecEncodeSchema(schema, ev)
	if err != nil {
		return head, err
	}
	seq := head.Seq + 1
	ph := sha256Hex(payload)
	prefix := pairRecPrefixHash(schema, runID, seq, partition, contractFP, ev.Kind, ev.CallID, head.Prefix, ph)
	if _, err := tx.Exec(`INSERT INTO basket_replay_inputs (run_id, seq, schema_version, partition, contract_fingerprint, event_kind, call_id, prev_prefix_hash, prefix_hash, payload_hash, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, runID, seq, schema, partition, contractFP, ev.Kind, ev.CallID, head.Prefix, prefix, ph, payload); err != nil {
		return head, fmt.Errorf("append recording input %d: %w", seq, err)
	}
	if err := pairRecProjectTx(tx, runID, seq, ev); err != nil {
		return head, err
	}
	return pairRecHead{Seq: seq, Prefix: prefix}, nil
}

func pairRecProjectTx(tx *sql.Tx, runID string, seq int64, ev pairRecEvent) error {
	switch ev.Kind {
	case pairRecEventStart:
		s := ev.Start
		if _, err := tx.Exec(`INSERT INTO basket_replay_calls (run_id, call_id, parent_id, kind, phase, member_id, slot_ms, action_id, step_id, request_hash, start_seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, ev.CallID, s.ParentID, s.Kind, s.Phase, s.MemberID, s.SlotMs, s.ActionID, s.StepID, s.RequestHash, seq); err != nil {
			return fmt.Errorf("index call %s: %w", ev.CallID, err)
		}
	case pairRecEventEnd, pairRecEventRecovery:
		class, hash := pairRecClassInfra, ""
		if ev.End != nil {
			class, hash = ev.End.Class, ev.End.ResultHash
		}
		res, err := tx.Exec(`UPDATE basket_replay_calls SET end_seq = ?, end_event = ?, outcome_class = ?, result_hash = ? WHERE run_id = ? AND call_id = ? AND end_seq = 0`, seq, ev.Kind, class, hash, runID, ev.CallID)
		if err != nil {
			return fmt.Errorf("close call %s: %w", ev.CallID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("call %s has no open start to close", ev.CallID)
		}
	}
	return nil
}

func (sdb *StateDB) appendPairRecEvents(runID, partition, contractFP string, head pairRecHead, schema int, events []pairRecEvent, before func(n int) error) (pairRecHead, error) {
	tx, err := sdb.db.Begin()
	if err != nil {
		return head, err
	}
	defer tx.Rollback()
	var lastSeq int64
	var lastPrefix string
	err = tx.QueryRow(`SELECT seq, prefix_hash FROM basket_replay_inputs WHERE run_id = ? ORDER BY seq DESC LIMIT 1`, runID).Scan(&lastSeq, &lastPrefix)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		lastSeq, lastPrefix = 0, ""
	case err != nil:
		return head, err
	}
	if lastSeq != head.Seq || lastPrefix != head.Prefix {
		return head, fmt.Errorf("the committed recording head (seq %d) is not the recorder head (seq %d); the append-only log was changed outside the recorder", lastSeq, head.Seq)
	}
	next := head
	for i, ev := range events {
		if before != nil {
			if err := before(i); err != nil {
				return head, err
			}
		}
		if next, err = pairRecInsertTx(tx, runID, partition, contractFP, next, schema, ev); err != nil {
			return head, err
		}
	}
	if err := tx.Commit(); err != nil {
		return head, err
	}
	return next, nil
}

func readPairRecRows(q interface {
	Query(string, ...interface{}) (*sql.Rows, error)
}, runID string) ([]pairRecRow, error) {
	rows, err := q.Query(`SELECT seq, schema_version, partition, contract_fingerprint, event_kind, call_id, prev_prefix_hash, prefix_hash, payload_hash, payload FROM basket_replay_inputs WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pairRecRow
	for rows.Next() {
		var r pairRecRow
		if err := rows.Scan(&r.Seq, &r.Schema, &r.Partition, &r.ContractFP, &r.Kind, &r.CallID, &r.PrevPrefix, &r.Prefix, &r.PayloadHash, &r.Payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type pairRecCallRow struct {
	CallID      string
	ParentID    string
	Kind        string
	Phase       string
	MemberID    string
	SlotMs      int64
	ActionID    string
	StepID      string
	RequestHash string
	StartSeq    int64
	EndSeq      int64
	EndEvent    string
	Class       string
	ResultHash  string
}

func readPairRecCallRows(q interface {
	Query(string, ...interface{}) (*sql.Rows, error)
}, runID string) ([]pairRecCallRow, error) {
	rows, err := q.Query(`SELECT call_id, parent_id, kind, phase, member_id, slot_ms, action_id, step_id, request_hash, start_seq, end_seq, end_event, outcome_class, result_hash FROM basket_replay_calls WHERE run_id = ? ORDER BY start_seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pairRecCallRow
	for rows.Next() {
		var c pairRecCallRow
		if err := rows.Scan(&c.CallID, &c.ParentID, &c.Kind, &c.Phase, &c.MemberID, &c.SlotMs, &c.ActionID, &c.StepID, &c.RequestHash, &c.StartSeq, &c.EndSeq, &c.EndEvent, &c.Class, &c.ResultHash); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type pairRecSlotRow struct {
	PairID        string
	SlotMs        int64
	CollectCallID string
	InputFP       string
	EnvelopeSHA   string
	StartMs       int64
	CutoffMs      int64
	DeadlineMs    int64
	PrefixSeq     int64
	PrefixHash    string
}

func readPairRecSlotRows(q interface {
	Query(string, ...interface{}) (*sql.Rows, error)
}, runID string) ([]pairRecSlotRow, error) {
	rows, err := q.Query(`SELECT pair_id, slot_ms, collect_call_id, input_fingerprint, envelope_sha256, start_ms, cutoff_ms, deadline_ms, prefix_seq, prefix_hash FROM basket_replay_slots WHERE run_id = ? ORDER BY pair_id, slot_ms`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pairRecSlotRow
	for rows.Next() {
		var s pairRecSlotRow
		if err := rows.Scan(&s.PairID, &s.SlotMs, &s.CollectCallID, &s.InputFP, &s.EnvelopeSHA, &s.StartMs, &s.CutoffMs, &s.DeadlineMs, &s.PrefixSeq, &s.PrefixHash); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type pairRecLog struct {
	runID      string
	partition  string
	contractFP string
	schema     int
	rows       []pairRecRow
	starts     map[string]int
	ends       map[string]int
	consumes   map[string]int
	children   map[string][]string
	stream     []int
	bind       *pairRecBind
	epochs     int
	sealed     *pairRecSeal
}

func (l *pairRecLog) head() pairRecHead {
	if len(l.rows) == 0 {
		return pairRecHead{}
	}
	last := l.rows[len(l.rows)-1]
	return pairRecHead{Seq: last.Seq, Prefix: last.Prefix}
}

func pairRecConsumable(r pairRecRow) bool {
	switch r.Kind {
	case pairRecEventWake, pairRecEventBudget, pairRecEventFailure, pairRecEventClear, pairRecEventConsume:
		return true
	case pairRecEventStart:
		return r.Event.Start != nil && r.Event.Start.ParentID == "" && r.Event.Start.Phase == pairRecPhaseStep
	}
	return false
}

func verifyPairRecLog(rows []pairRecRow, runID, partition, contractFP string) (*pairRecLog, error) {
	l := &pairRecLog{runID: runID, partition: partition, contractFP: contractFP, starts: map[string]int{}, ends: map[string]int{}, consumes: map[string]int{}, children: map[string][]string{}}
	prev := ""
	for i := range rows {
		r := rows[i]
		where := fmt.Sprintf("recording input %d", r.Seq)
		if r.Seq != int64(i+1) {
			return nil, fmt.Errorf("%s is at position %d; the append-only log has a gap or a reordered row", where, i+1)
		}
		if r.Schema != pairRecSchemaVersion && r.Schema != pairRecSchemaAsync {
			return nil, fmt.Errorf("%s schema %d is not 1 or 2", where, r.Schema)
		}
		if l.schema == 0 {
			l.schema = r.Schema
		}
		if r.Schema != l.schema || r.Partition != partition || r.ContractFP != contractFP {
			return nil, fmt.Errorf("%s binds schema %d, partition %s and contract %s, not this run's schema %d", where, r.Schema, r.Partition, shortFingerprint(r.ContractFP), l.schema)
		}
		if r.PrevPrefix != prev {
			return nil, fmt.Errorf("%s does not extend the previous prefix", where)
		}
		if sha256Hex(r.Payload) != r.PayloadHash {
			return nil, fmt.Errorf("%s payload does not match its payload hash", where)
		}
		if want := pairRecPrefixHash(r.Schema, runID, r.Seq, partition, contractFP, r.Kind, r.CallID, r.PrevPrefix, r.PayloadHash); want != r.Prefix {
			return nil, fmt.Errorf("%s prefix hash does not match its row", where)
		}
		ev, err := pairRecDecode(r.Payload)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if ev.Kind != r.Kind || ev.CallID != r.CallID || ev.V != r.Schema {
			return nil, fmt.Errorf("%s row kind %s/%s schema %d differs from its payload %s/%s version %d", where, r.Kind, r.CallID, r.Schema, ev.Kind, ev.CallID, ev.V)
		}
		r.Event = ev
		rows[i] = r
		l.rows = rows
		if err := l.admit(i, r); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		prev = r.Prefix
	}
	l.rows = rows
	if len(rows) > 0 && l.bind == nil {
		return nil, fmt.Errorf("the recording has no binding event")
	}
	for i := range l.rows {
		if pairRecConsumable(l.rows[i]) {
			l.stream = append(l.stream, i)
		}
	}
	return l, nil
}

func (l *pairRecLog) admit(i int, r pairRecRow) error {
	ev := r.Event
	if l.sealed != nil {
		return fmt.Errorf("the recording continues after its seal")
	}
	one := func(set bool) error {
		count := 0
		for _, p := range []bool{ev.Bind != nil, ev.Epoch != nil, ev.Start != nil, ev.End != nil, ev.Recovery != nil, ev.Wake != nil, ev.Budget != nil, ev.Failure != nil, ev.Clear != nil, ev.Seal != nil, ev.Consume != nil} {
			if p {
				count++
			}
		}
		if count != 1 || !set {
			return fmt.Errorf("a %s event must carry exactly its own body", ev.Kind)
		}
		return nil
	}
	switch ev.Kind {
	case pairRecEventBind:
		if err := one(ev.Bind != nil); err != nil {
			return err
		}
		if i != 0 {
			return fmt.Errorf("the binding event must be the first input")
		}
		if ev.Bind.RunID != l.runID || ev.Bind.ContractFP != l.contractFP || ev.Bind.Partition != l.partition {
			return fmt.Errorf("the binding names run %s, contract %s and partition %s", ev.Bind.RunID, shortFingerprint(ev.Bind.ContractFP), ev.Bind.Partition)
		}
		if (l.schema == pairRecSchemaAsync) != (ev.Bind.Family == pairPaperManifestAsyncVer) {
			return fmt.Errorf("schema %d binding names family %d", l.schema, ev.Bind.Family)
		}
		b := *ev.Bind
		l.bind = &b
	case pairRecEventEpoch:
		if err := one(ev.Epoch != nil); err != nil {
			return err
		}
		if ev.Epoch.Epoch != l.epochs+1 {
			return fmt.Errorf("process epoch %d does not follow epoch %d", ev.Epoch.Epoch, l.epochs)
		}
		l.epochs++
	case pairRecEventStart:
		if err := one(ev.Start != nil); err != nil {
			return err
		}
		s := ev.Start
		if ev.CallID == "" || !pairRecCallKinds[s.Kind] || (s.Phase != pairRecPhaseStep && s.Phase != pairRecPhaseBootstrap) {
			return fmt.Errorf("call start %q has kind %q and phase %q", ev.CallID, s.Kind, s.Phase)
		}
		if _, dup := l.starts[ev.CallID]; dup {
			return fmt.Errorf("call %s starts twice", ev.CallID)
		}
		if sha256Hex(s.Request) != s.RequestHash {
			return fmt.Errorf("call %s request does not match its request hash", ev.CallID)
		}
		oracle := s.Kind == "funding_oracle" && s.ParentID == ""
		if s.Async != (l.schema == pairRecSchemaAsync && oracle) {
			return fmt.Errorf("call %s async flag does not match schema %d", ev.CallID, l.schema)
		}
		if s.ParentID != "" {
			pi, ok := l.starts[s.ParentID]
			if !ok {
				return fmt.Errorf("call %s names the unknown parent %s", ev.CallID, s.ParentID)
			}
			if _, closed := l.ends[s.ParentID]; closed {
				return fmt.Errorf("call %s starts after its parent %s ended", ev.CallID, s.ParentID)
			}
			_ = pi
			l.children[s.ParentID] = append(l.children[s.ParentID], ev.CallID)
		}
		l.starts[ev.CallID] = i
	case pairRecEventEnd, pairRecEventRecovery:
		if ev.Kind == pairRecEventEnd {
			if err := one(ev.End != nil); err != nil {
				return err
			}
			if !pairRecClasses[ev.End.Class] {
				return fmt.Errorf("call %s ends with the unknown class %q", ev.CallID, ev.End.Class)
			}
			if pairRecResultHash(ev.End.Result) != ev.End.ResultHash {
				return fmt.Errorf("call %s result does not match its result hash", ev.CallID)
			}
		} else if err := one(ev.Recovery != nil); err != nil {
			return err
		}
		if _, ok := l.starts[ev.CallID]; !ok {
			return fmt.Errorf("call %s ends without a start", ev.CallID)
		}
		if _, dup := l.ends[ev.CallID]; dup {
			return fmt.Errorf("call %s has a second terminal event", ev.CallID)
		}
		l.ends[ev.CallID] = i
	case pairRecEventConsume:
		if err := one(ev.Consume != nil); err != nil {
			return err
		}
		if l.schema != pairRecSchemaAsync {
			return fmt.Errorf("call consumption belongs to schema 2")
		}
		c := ev.Consume
		si, ok := l.starts[ev.CallID]
		if !ok {
			return fmt.Errorf("call %s is consumed without a start", ev.CallID)
		}
		ei, ended := l.ends[ev.CallID]
		if !ended {
			return fmt.Errorf("call %s is consumed without a terminal", ev.CallID)
		}
		if _, dup := l.consumes[ev.CallID]; dup {
			return fmt.Errorf("call %s is consumed twice", ev.CallID)
		}
		start, term := l.rows[si], l.rows[ei]
		if start.Seq >= term.Seq || term.Seq >= r.Seq {
			return fmt.Errorf("call %s consumption is not after its start and terminal", ev.CallID)
		}
		if c.RequestHash != start.Event.Start.RequestHash || c.WakeSeq <= 0 || c.StepID == "" || c.LogicalMs <= 0 {
			return fmt.Errorf("call %s consumption does not bind its request, wake, step and logical time", ev.CallID)
		}
		switch c.TerminalKind {
		case pairRecEventEnd:
			if term.Kind != pairRecEventEnd || c.TerminalHash != term.Event.End.ResultHash {
				return fmt.Errorf("call %s consumption does not bind its terminal hash", ev.CallID)
			}
		case pairRecEventRecovery:
			if term.Kind != pairRecEventRecovery || c.TerminalHash != sha256Hex([]byte(term.Event.Recovery.Reason)) {
				return fmt.Errorf("call %s consumption does not bind its recovery", ev.CallID)
			}
		default:
			return fmt.Errorf("call %s consumption names terminal kind %q", ev.CallID, c.TerminalKind)
		}
		switch c.Disposition {
		case "accepted":
			if len(c.Batch) == 0 || c.BatchHash != sha256Hex(c.Batch) {
				return fmt.Errorf("call %s accepted consumption has no sealed batch", ev.CallID)
			}
		case "refused", "interrupted", "cancelled", "infrastructure_failure":
			if len(c.Batch) != 0 || c.BatchHash != "" {
				return fmt.Errorf("call %s %s consumption carries a batch", ev.CallID, c.Disposition)
			}
		default:
			return fmt.Errorf("call %s consumption disposition %q is unknown", ev.CallID, c.Disposition)
		}
		l.consumes[ev.CallID] = i
	case pairRecEventWake:
		if err := one(ev.Wake != nil); err != nil {
			return err
		}
	case pairRecEventBudget:
		if err := one(ev.Budget != nil); err != nil {
			return err
		}
	case pairRecEventFailure:
		if err := one(ev.Failure != nil); err != nil {
			return err
		}
	case pairRecEventClear:
		if err := one(ev.Clear != nil); err != nil {
			return err
		}
	case pairRecEventSeal:
		if err := one(ev.Seal != nil); err != nil {
			return err
		}
		if ev.Seal.FinalSeq != r.Seq-1 || ev.Seal.FinalPrefixHash != r.PrevPrefix {
			return fmt.Errorf("the seal binds seq %d, not the prefix it follows", ev.Seal.FinalSeq)
		}
		for id, si := range l.starts {
			if _, ok := l.ends[id]; !ok {
				return fmt.Errorf("the seal follows call %s, which has no terminal or recovery event", id)
			}
			if l.rows[si].Event.Start != nil && l.rows[si].Event.Start.Async {
				if _, ok := l.consumes[id]; !ok {
					return fmt.Errorf("the seal follows asynchronous call %s, which has no consumption", id)
				}
			}
		}
		s := *ev.Seal
		l.sealed = &s
	}
	return nil
}

func pairRecResultHash(b []byte) string {
	return sha256Hex(b)
}

func (l *pairRecLog) checkCalls(calls []pairRecCallRow) error {
	want := map[string]pairRecCallRow{}
	for id, si := range l.starts {
		s := l.rows[si]
		c := pairRecCallRow{CallID: id, ParentID: s.Event.Start.ParentID, Kind: s.Event.Start.Kind, Phase: s.Event.Start.Phase, MemberID: s.Event.Start.MemberID,
			SlotMs: s.Event.Start.SlotMs, ActionID: s.Event.Start.ActionID, StepID: s.Event.Start.StepID, RequestHash: s.Event.Start.RequestHash, StartSeq: s.Seq}
		if ei, ok := l.ends[id]; ok {
			e := l.rows[ei]
			c.EndSeq, c.EndEvent = e.Seq, e.Kind
			c.Class = pairRecClassInfra
			if e.Event.End != nil {
				c.Class, c.ResultHash = e.Event.End.Class, e.Event.End.ResultHash
			}
		}
		want[id] = c
	}
	if len(calls) != len(want) {
		return fmt.Errorf("the call index holds %d calls, the committed log %d", len(calls), len(want))
	}
	for _, c := range calls {
		if w, ok := want[c.CallID]; !ok || w != c {
			return fmt.Errorf("the call index row %s differs from its committed log events", c.CallID)
		}
	}
	return nil
}

func (l *pairRecLog) prefixAt(seq int64) (string, bool) {
	if seq < 0 || seq > int64(len(l.rows)) {
		return "", false
	}
	if seq == 0 {
		return "", true
	}
	return l.rows[seq-1].Prefix, true
}

func (l *pairRecLog) checkSlots(slots []pairRecSlotRow) error {
	for _, s := range slots {
		p, ok := l.prefixAt(s.PrefixSeq)
		if !ok || p != s.PrefixHash {
			return fmt.Errorf("slot %d binds recording prefix %d, which is not in the committed log", s.SlotMs, s.PrefixSeq)
		}
		si, ok := l.starts[s.CollectCallID]
		if !ok || l.rows[si].Event.Start.Kind != pairRecKindCollect {
			return fmt.Errorf("slot %d names collection call %s, which the log does not hold", s.SlotMs, s.CollectCallID)
		}
		ei, ok := l.ends[s.CollectCallID]
		if !ok || l.rows[ei].Event.End == nil || l.rows[ei].Event.End.Class != pairRecClassResult || int64(ei+1) > s.PrefixSeq {
			return fmt.Errorf("slot %d collection call %s has no committed result inside its bound prefix", s.SlotMs, s.CollectCallID)
		}
	}
	return nil
}

func (l *pairRecLog) unterminated() []string {
	var out []string
	for id := range l.starts {
		if _, ok := l.ends[id]; !ok {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return l.starts[out[i]] < l.starts[out[j]] })
	return out
}

func (sdb *StateDB) insertPairRecSlotTx(tx *sql.Tx, runID string, s pairRecSlotRow) error {
	_, err := tx.Exec(`INSERT INTO basket_replay_slots (run_id, pair_id, slot_ms, collect_call_id, input_fingerprint, envelope_sha256, start_ms, cutoff_ms, deadline_ms, prefix_seq, prefix_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, s.PairID, s.SlotMs, s.CollectCallID, s.InputFP, s.EnvelopeSHA, s.StartMs, s.CutoffMs, s.DeadlineMs, s.PrefixSeq, s.PrefixHash)
	if err != nil {
		return fmt.Errorf("bind slot %d to its recorded collection: %w", s.SlotMs, err)
	}
	return nil
}
