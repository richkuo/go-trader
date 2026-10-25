package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

const (
	pairVirtualKindBinding  = "binding"
	pairVirtualKindSubmit   = "submit"
	pairVirtualKindRefusal  = "refusal"
	pairVirtualKindDeadline = "deadline"
	pairVirtualKindMatch    = "match"
	pairVirtualKindConsume  = "consume"
	pairVirtualKindVisible  = "visible"
	pairVirtualKindQuery    = "query"
	pairVirtualKindAbsence  = "absence"

	pairVirtualGenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"
)

var (
	errPairVirtualCapacity = errors.New("pair virtual liquidity capacity is exhausted")
	errPairVirtualConflict = errors.New("pair virtual ledger record conflicts with a saved record")
	errPairVirtualBinding  = errors.New("pair virtual venue binding changed")
)

const pairVirtualDDL = `
CREATE TABLE IF NOT EXISTS pair_virtual_events (
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
CREATE INDEX IF NOT EXISTS idx_pair_virtual_events_capacity
    ON pair_virtual_events(run_tag, member_key_json, source_contract, observation_id, book_side, level_index) WHERE kind = 'consume';
CREATE TRIGGER IF NOT EXISTS trg_pair_virtual_events_immutable
    BEFORE UPDATE ON pair_virtual_events
    BEGIN SELECT RAISE(ABORT, 'pair virtual execution history is append-only'); END;
CREATE TRIGGER IF NOT EXISTS trg_pair_virtual_events_kept
    BEFORE DELETE ON pair_virtual_events
    BEGIN SELECT RAISE(ABORT, 'pair virtual execution history is append-only'); END;
CREATE TABLE IF NOT EXISTS pair_virtual_allocations (
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
CREATE TRIGGER IF NOT EXISTS trg_pair_virtual_allocations_immutable
    BEFORE UPDATE ON pair_virtual_allocations
    BEGIN SELECT RAISE(ABORT, 'pair virtual client identifier allocations are append-only'); END;
CREATE TRIGGER IF NOT EXISTS trg_pair_virtual_allocations_kept
    BEFORE DELETE ON pair_virtual_allocations
    BEGIN SELECT RAISE(ABORT, 'pair virtual client identifier allocations are append-only'); END;
`

func (sdb *StateDB) migratePairVirtualSchema() error {
	if _, err := sdb.db.Exec(pairVirtualDDL); err != nil {
		return fmt.Errorf("create pair virtual venue tables: %w", err)
	}
	return nil
}

type pairVirtualIdentity struct {
	RunTag     string
	Partition  RiskPartition
	PairID     string
	MemberID   string
	Provider   string
	AccountKey string
	Instrument string
}

type pairVirtualStoredKey struct {
	Partition  string `json:"partition"`
	Provider   string `json:"provider"`
	AccountKey string `json:"account_key"`
	Instrument string `json:"instrument"`
	StrategyID string `json:"strategy_id"`
}

type pairVirtualEvent struct {
	Seq            int64
	Kind           string
	SemanticKey    string
	ClientOrderID  string
	ObservationID  string
	SourceContract string
	BookSide       string
	LevelIndex     int
	ConsumedNative pairDecimal
	ConsumedBase   pairDecimal
	NativeDelta    pairDecimal
	VenueTimeMs    int64
	ReceiptMs      int64
	VisibleMs      int64
	RequestHash    string
	Body           string
	PrevHash       string
	Hash           string
	CreatedAtMs    int64
}

type pairVirtualAllocation struct {
	Seq           int64
	ClientOrderID string
	CreatedAtMs   int64
	Bound         bool
	Recorded      bool
}

type pairVirtualLedger struct {
	Events      []pairVirtualEvent
	Allocations []pairVirtualAllocation
}

type pairVirtualCapacity struct {
	SourceContract string
	ObservationID  string
	BookSide       string
	LevelIndex     int
	LevelPx        string
	LevelQty       *big.Rat
	Consume        *big.Rat
}

type pairVirtualHashBody struct {
	RunTag         string `json:"run_tag"`
	MemberKey      string `json:"member_key_json"`
	Seq            int64  `json:"seq"`
	Kind           string `json:"kind"`
	SemanticKey    string `json:"semantic_key"`
	ClientOrderID  string `json:"client_order_id"`
	ObservationID  string `json:"observation_id"`
	SourceContract string `json:"source_contract"`
	BookSide       string `json:"book_side"`
	LevelIndex     int    `json:"level_index"`
	ConsumedNative string `json:"consumed_native_qty"`
	ConsumedBase   string `json:"consumed_base_qty"`
	NativeDelta    string `json:"native_qty_delta"`
	VenueTimeMs    int64  `json:"venue_time_ms"`
	ReceiptMs      int64  `json:"receipt_ms"`
	VisibleMs      int64  `json:"visible_ms"`
	RequestHash    string `json:"request_hash"`
	Body           string `json:"body_json"`
	PrevHash       string `json:"previous_event_hash"`
	CreatedAtMs    int64  `json:"created_at_ms"`
}

func pairVirtualEventHash(runTag, memberKey string, e pairVirtualEvent) (string, error) {
	b, err := pairCanonicalJSON(pairVirtualHashBody{RunTag: runTag, MemberKey: memberKey, Seq: e.Seq, Kind: e.Kind, SemanticKey: e.SemanticKey, ClientOrderID: e.ClientOrderID,
		ObservationID: e.ObservationID, SourceContract: e.SourceContract, BookSide: e.BookSide, LevelIndex: e.LevelIndex, ConsumedNative: string(e.ConsumedNative),
		ConsumedBase: string(e.ConsumedBase), NativeDelta: string(e.NativeDelta), VenueTimeMs: e.VenueTimeMs, ReceiptMs: e.ReceiptMs, VisibleMs: e.VisibleMs,
		RequestHash: e.RequestHash, Body: e.Body, PrevHash: e.PrevHash, CreatedAtMs: e.CreatedAtMs})
	if err != nil {
		return "", err
	}
	return pairSHA256(b), nil
}

func (e *pairVirtualEvent) normalize() {
	if e.ConsumedNative == "" {
		e.ConsumedNative = "0"
	}
	if e.ConsumedBase == "" {
		e.ConsumedBase = "0"
	}
	if e.NativeDelta == "" {
		e.NativeDelta = "0"
	}
	if e.LevelIndex == 0 && e.Kind != pairVirtualKindConsume {
		e.LevelIndex = -1
	}
}

func (sdb *StateDB) pairVirtualKey(id pairVirtualIdentity) (string, string, error) {
	if sdb == nil || sdb.db == nil {
		return "", "", fmt.Errorf("state db unavailable")
	}
	if strings.TrimSpace(id.RunTag) == "" || id.PairID == "" || id.MemberID == "" {
		return "", "", fmt.Errorf("pair virtual identity needs a run tag, a pair and a member")
	}
	sid, err := sdb.basketMemberOwner(id.MemberID, id.Partition)
	if err != nil {
		return "", "", fmt.Errorf("pair virtual venue member %s: %w", id.MemberID, err)
	}
	b, err := pairCanonicalJSON(pairVirtualStoredKey{Partition: id.Partition.String(), Provider: id.Provider, AccountKey: id.AccountKey, Instrument: id.Instrument, StrategyID: sid})
	if err != nil {
		return "", "", err
	}
	return sid, string(b), nil
}

const pairVirtualEventColumns = `seq, kind, semantic_key, client_order_id, observation_id, source_contract, book_side, level_index, consumed_native_qty, consumed_base_qty,
    native_qty_delta, venue_time_ms, receipt_ms, visible_ms, request_hash, body_json, previous_event_hash, event_hash, created_at_ms`

type pairVirtualQueryer interface {
	Query(query string, args ...interface{}) (*sql.Rows, error)
}

func pairVirtualReadEvents(q pairVirtualQueryer, runTag, keyJSON string) ([]pairVirtualEvent, error) {
	rows, err := q.Query(`SELECT `+pairVirtualEventColumns+` FROM pair_virtual_events WHERE run_tag = ? AND member_key_json = ? ORDER BY seq`, runTag, keyJSON)
	if err != nil {
		return nil, fmt.Errorf("read pair virtual events: %w", err)
	}
	defer rows.Close()
	var out []pairVirtualEvent
	for rows.Next() {
		var e pairVirtualEvent
		var cn, cb, nd string
		if err := rows.Scan(&e.Seq, &e.Kind, &e.SemanticKey, &e.ClientOrderID, &e.ObservationID, &e.SourceContract, &e.BookSide, &e.LevelIndex, &cn, &cb, &nd,
			&e.VenueTimeMs, &e.ReceiptMs, &e.VisibleMs, &e.RequestHash, &e.Body, &e.PrevHash, &e.Hash, &e.CreatedAtMs); err != nil {
			return nil, err
		}
		e.ConsumedNative, e.ConsumedBase, e.NativeDelta = pairDecimal(cn), pairDecimal(cb), pairDecimal(nd)
		out = append(out, e)
	}
	return out, rows.Err()
}

func pairVirtualCheckChain(runTag, keyJSON string, events []pairVirtualEvent) error {
	prev := pairVirtualGenesisHash
	seen := map[string]bool{}
	for i, e := range events {
		if e.Seq != int64(i)+1 {
			return fmt.Errorf("pair virtual history has seq %d at position %d; the append-only history has a hole", e.Seq, i+1)
		}
		if e.PrevHash != prev {
			return fmt.Errorf("pair virtual event %d names previous hash %s, the chain holds %s", e.Seq, shortFingerprint(e.PrevHash), shortFingerprint(prev))
		}
		h, err := pairVirtualEventHash(runTag, keyJSON, e)
		if err != nil {
			return err
		}
		if h != e.Hash {
			return fmt.Errorf("pair virtual event %d hash %s does not match its content %s", e.Seq, shortFingerprint(e.Hash), shortFingerprint(h))
		}
		if seen[e.SemanticKey] {
			return fmt.Errorf("pair virtual event %d repeats the semantic identity %s", e.Seq, e.SemanticKey)
		}
		seen[e.SemanticKey] = true
		if (i == 0) != (e.Kind == pairVirtualKindBinding) {
			return fmt.Errorf("pair virtual event %d is %s; the binding is exactly the first record", e.Seq, e.Kind)
		}
		for _, d := range []pairDecimal{e.ConsumedNative, e.ConsumedBase, e.NativeDelta} {
			if _, err := d.rat(); err != nil {
				return fmt.Errorf("pair virtual event %d: %v", e.Seq, err)
			}
		}
		prev = e.Hash
	}
	return nil
}

func (sdb *StateDB) loadPairVirtualLedger(id pairVirtualIdentity) (pairVirtualLedger, error) {
	var out pairVirtualLedger
	sid, keyJSON, err := sdb.pairVirtualKey(id)
	if err != nil {
		return out, err
	}
	exists, err := tableExists(sdb, "pair_virtual_events")
	if err != nil || !exists {
		return out, err
	}
	events, err := pairVirtualReadEvents(sdb.db, id.RunTag, keyJSON)
	if err != nil {
		return out, err
	}
	if err := pairVirtualCheckChain(id.RunTag, keyJSON, events); err != nil {
		return out, err
	}
	out.Events = events
	allocs, err := pairVirtualReadAllocations(sdb.db, id, sid, keyJSON)
	if err != nil {
		return out, err
	}
	out.Allocations = allocs
	return out, nil
}

func pairVirtualReadAllocations(q pairVirtualQueryer, id pairVirtualIdentity, sid, keyJSON string) ([]pairVirtualAllocation, error) {
	rows, err := q.Query(`SELECT a.seq, a.client_order_id, a.created_at_ms, b.strategy_id, b.scope, b.source, b.basket_id,
		    EXISTS (SELECT 1 FROM pair_virtual_events e WHERE e.run_tag = a.run_tag AND e.member_key_json = a.member_key_json AND e.client_order_id = a.client_order_id)
		FROM pair_virtual_allocations a
		LEFT JOIN basket_member_actions b ON b.provider = a.provider AND b.account_key = a.account_key AND b.client_order_id = a.client_order_id
		WHERE a.run_tag = ? AND a.member_key_json = ? ORDER BY a.seq`, id.RunTag, keyJSON)
	if err != nil {
		return nil, fmt.Errorf("read pair virtual allocations: %w", err)
	}
	defer rows.Close()
	var out []pairVirtualAllocation
	for rows.Next() {
		var a pairVirtualAllocation
		var bsid, bscope, bsource, bpair sql.NullString
		if err := rows.Scan(&a.Seq, &a.ClientOrderID, &a.CreatedAtMs, &bsid, &bscope, &bsource, &bpair, &a.Recorded); err != nil {
			return nil, err
		}
		if bsid.Valid {
			if bsid.String != sid || bscope.String != string(id.Partition.Scope) || bsource.String != id.Partition.Source || bpair.String != id.PairID {
				return nil, fmt.Errorf("pair virtual client identifier %s is bound to an action of %s in %s/%s pair %s, not member %s in %s pair %s", a.ClientOrderID, bsid.String, bscope.String, bsource.String, bpair.String, id.MemberID, partitionLabel(id.Partition), id.PairID)
			}
			a.Bound = true
		}
		if len(out) > 0 && a.Seq != out[len(out)-1].Seq+1 || len(out) == 0 && a.Seq != 1 {
			return nil, fmt.Errorf("pair virtual allocation history has seq %d after %d; the append-only history has a hole", a.Seq, len(out))
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (sdb *StateDB) pairVirtualEnsureBinding(tx *sql.Tx, id pairVirtualIdentity, sid, keyJSON string, binding string, nowMs int64) (int64, string, error) {
	events, err := pairVirtualReadEvents(tx, id.RunTag, keyJSON)
	if err != nil {
		return 0, "", err
	}
	if err := pairVirtualCheckChain(id.RunTag, keyJSON, events); err != nil {
		return 0, "", err
	}
	if len(events) > 0 {
		if events[0].Body != binding {
			return 0, "", fmt.Errorf("%w: the saved binding for %s run %s differs from the attached venue", errPairVirtualBinding, id.MemberID, id.RunTag)
		}
		last := events[len(events)-1]
		return last.Seq, last.Hash, nil
	}
	e := pairVirtualEvent{Seq: 1, Kind: pairVirtualKindBinding, SemanticKey: pairVirtualKindBinding, Body: binding, PrevHash: pairVirtualGenesisHash, CreatedAtMs: nowMs}
	if err := sdb.pairVirtualInsert(tx, id, sid, keyJSON, &e); err != nil {
		return 0, "", err
	}
	return e.Seq, e.Hash, nil
}

func (sdb *StateDB) pairVirtualInsert(tx *sql.Tx, id pairVirtualIdentity, sid, keyJSON string, e *pairVirtualEvent) error {
	e.normalize()
	h, err := pairVirtualEventHash(id.RunTag, keyJSON, *e)
	if err != nil {
		return err
	}
	e.Hash = h
	_, err = tx.Exec(`INSERT INTO pair_virtual_events (run_tag, member_key_json, seq, scope, source, pair_id, strategy_id, provider, account_key, instrument,
		    kind, semantic_key, client_order_id, observation_id, source_contract, book_side, level_index, consumed_native_qty, consumed_base_qty, native_qty_delta,
		    venue_time_ms, receipt_ms, visible_ms, request_hash, body_json, previous_event_hash, event_hash, created_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.RunTag, keyJSON, e.Seq, string(id.Partition.Scope), id.Partition.Source, id.PairID, sid, id.Provider, id.AccountKey, id.Instrument,
		e.Kind, e.SemanticKey, e.ClientOrderID, e.ObservationID, e.SourceContract, e.BookSide, e.LevelIndex, string(e.ConsumedNative), string(e.ConsumedBase), string(e.NativeDelta),
		e.VenueTimeMs, e.ReceiptMs, e.VisibleMs, e.RequestHash, e.Body, e.PrevHash, e.Hash, e.CreatedAtMs)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("%w: %s (%v)", errPairVirtualConflict, e.SemanticKey, err)
		}
		return fmt.Errorf("append pair virtual event %s: %w", e.SemanticKey, err)
	}
	return nil
}

func (sdb *StateDB) allocatePairVirtualTx(id pairVirtualIdentity, binding string, nowMs int64, mkID func(seq int64) string) (string, error) {
	if sdb.readOnly {
		return "", fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	sid, keyJSON, err := sdb.pairVirtualKey(id)
	if err != nil {
		return "", err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin pair virtual allocation: %w", err)
	}
	defer tx.Rollback()
	if _, _, err := sdb.pairVirtualEnsureBinding(tx, id, sid, keyJSON, binding, nowMs); err != nil {
		return "", err
	}
	allocs, err := pairVirtualReadAllocations(tx, id, sid, keyJSON)
	if err != nil {
		return "", err
	}
	for _, a := range allocs {
		if !a.Bound && !a.Recorded {
			if err := tx.Commit(); err != nil {
				return "", err
			}
			return a.ClientOrderID, nil
		}
	}
	seq := int64(len(allocs)) + 1
	cloid := mkID(seq)
	var clash int
	if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM pair_virtual_allocations WHERE client_order_id = ?) + (SELECT COUNT(*) FROM basket_member_actions WHERE client_order_id = ?)`, cloid, cloid).Scan(&clash); err != nil {
		return "", fmt.Errorf("read pair virtual identifier collisions: %w", err)
	}
	if clash > 0 {
		return "", fmt.Errorf("%w: the client identifier %s already names another allocation or action; a truncated member hash collided", errPairVirtualConflict, cloid)
	}
	if _, err := tx.Exec(`INSERT INTO pair_virtual_allocations (run_tag, member_key_json, seq, scope, source, pair_id, strategy_id, provider, account_key, instrument, client_order_id, created_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id.RunTag, keyJSON, seq, string(id.Partition.Scope), id.Partition.Source, id.PairID, sid, id.Provider, id.AccountKey, id.Instrument, cloid, nowMs); err != nil {
		return "", fmt.Errorf("record pair virtual allocation %s: %w", cloid, err)
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return "", fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return cloid, nil
}

func (sdb *StateDB) appendPairVirtualTx(id pairVirtualIdentity, binding string, events []pairVirtualEvent, caps []pairVirtualCapacity, nowMs int64) ([]pairVirtualEvent, error) {
	if sdb.readOnly {
		return nil, fmt.Errorf("%s state file is open read-only", sdb.storageRoleOf())
	}
	sid, keyJSON, err := sdb.pairVirtualKey(id)
	if err != nil {
		return nil, err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin pair virtual append: %w", err)
	}
	defer tx.Rollback()
	seq, prev, err := sdb.pairVirtualEnsureBinding(tx, id, sid, keyJSON, binding, nowMs)
	if err != nil {
		return nil, err
	}
	used := map[string]*big.Rat{}
	for _, c := range caps {
		k := fmt.Sprintf("%s|%s|%s|%d", c.SourceContract, c.ObservationID, c.BookSide, c.LevelIndex)
		if used[k] == nil {
			total := new(big.Rat)
			rows, err := tx.Query(`SELECT consumed_native_qty, body_json FROM pair_virtual_events WHERE run_tag = ? AND member_key_json = ? AND kind = 'consume'
				AND source_contract = ? AND observation_id = ? AND book_side = ? AND level_index = ?`, id.RunTag, keyJSON, c.SourceContract, c.ObservationID, c.BookSide, c.LevelIndex)
			if err != nil {
				return nil, fmt.Errorf("read pair virtual level capacity: %w", err)
			}
			for rows.Next() {
				var q, body string
				if err := rows.Scan(&q, &body); err != nil {
					rows.Close()
					return nil, err
				}
				r, err := pairDecimal(q).rat()
				if err != nil {
					rows.Close()
					return nil, err
				}
				var saved pairVirtualConsumeBody
				if err := pairVirtualDecodeBody(body, &saved); err != nil {
					rows.Close()
					return nil, err
				}
				lq, _ := new(big.Rat).SetString(saved.LevelQty)
				if saved.LevelPx != c.LevelPx || lq == nil || lq.Cmp(c.LevelQty) != 0 {
					rows.Close()
					return nil, fmt.Errorf("%w: source %s %s level %d was consumed at %s x %s, the record now shows %s x %s", errPairVirtualConflict, c.ObservationID, c.BookSide, c.LevelIndex, saved.LevelPx, saved.LevelQty, c.LevelPx, c.LevelQty.RatString())
				}
				total.Add(total, r)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
			used[k] = total
		}
		used[k].Add(used[k], c.Consume)
		if used[k].Cmp(c.LevelQty) > 0 {
			return nil, fmt.Errorf("%w: source %s %s level %d holds %s native, consumption would reach %s", errPairVirtualCapacity, c.ObservationID, c.BookSide, c.LevelIndex, c.LevelQty.RatString(), used[k].RatString())
		}
	}
	out := make([]pairVirtualEvent, 0, len(events))
	for _, e := range events {
		seq++
		e.Seq, e.PrevHash = seq, prev
		if e.CreatedAtMs == 0 {
			e.CreatedAtMs = nowMs
		}
		if err := sdb.pairVirtualInsert(tx, id, sid, keyJSON, &e); err != nil {
			return nil, err
		}
		prev = e.Hash
		out = append(out, e)
	}
	if storeCommitHook != nil {
		if err := storeCommitHook(sdb.storageRoleOf()); err != nil {
			return nil, fmt.Errorf("commit hook (%s): %w", sdb.storageRoleOf(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

type pairVirtualActionRow struct {
	Found       bool
	Status      string
	RequestJSON string
	StrategyID  string
	BasketID    string
}

func (sdb *StateDB) pairVirtualActionRow(id pairVirtualIdentity, cloid string) (pairVirtualActionRow, error) {
	var out pairVirtualActionRow
	sid, _, err := sdb.pairVirtualKey(id)
	if err != nil {
		return out, err
	}
	var scope, source, rowSID string
	err = sdb.db.QueryRow(`SELECT status, request_json, strategy_id, scope, source, basket_id FROM basket_member_actions WHERE provider = ? AND account_key = ? AND client_order_id = ? AND group_mode = 'pair'`,
		id.Provider, id.AccountKey, cloid).Scan(&out.Status, &out.RequestJSON, &rowSID, &scope, &source, &out.BasketID)
	if err == sql.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("read the saved action for client %s: %w", cloid, err)
	}
	if rowSID != sid || scope != string(id.Partition.Scope) || source != id.Partition.Source || out.BasketID != id.PairID {
		return out, fmt.Errorf("client %s is saved for stored strategy %s in %s/%s pair %s, not member %s of pair %s in %s", cloid, rowSID, scope, source, out.BasketID, id.MemberID, id.PairID, partitionLabel(id.Partition))
	}
	out.Found, out.StrategyID = true, id.MemberID
	return out, nil
}

func (sdb *StateDB) pairVirtualSlotTargets(p RiskPartition, pairID string, slotMs int64) (string, bool, error) {
	var raw string
	err := sdb.db.QueryRow(`SELECT targets_json FROM basket_rebalances WHERE scope = ? AND source = ? AND basket_id = ? AND deadline_ms = ?`,
		string(p.Scope), p.Source, pairID, slotMs).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the saved slot %d decision: %w", slotMs, err)
	}
	return raw, true, nil
}

func (st *StateStore) pairVirtualFile(rt *basketRuntime) (*StateDB, error) {
	if st == nil || rt == nil {
		return nil, fmt.Errorf("pair virtual storage needs a state store and a pair runtime")
	}
	return st.basketFile(rt)
}

func (st *StateStore) LoadPairVirtualLedger(rt *basketRuntime, id pairVirtualIdentity) (pairVirtualLedger, error) {
	db, err := st.pairVirtualFile(rt)
	if err != nil {
		return pairVirtualLedger{}, err
	}
	return db.loadPairVirtualLedger(id)
}

func (st *StateStore) AllocatePairVirtualID(rt *basketRuntime, id pairVirtualIdentity, binding string, nowMs int64, mkID func(seq int64) string) (string, error) {
	db, err := st.pairVirtualFile(rt)
	if err != nil {
		return "", err
	}
	return db.allocatePairVirtualTx(id, binding, nowMs, mkID)
}

func (st *StateStore) AppendPairVirtualEvents(rt *basketRuntime, id pairVirtualIdentity, binding string, events []pairVirtualEvent, caps []pairVirtualCapacity, nowMs int64) ([]pairVirtualEvent, error) {
	db, err := st.pairVirtualFile(rt)
	if err != nil {
		return nil, err
	}
	return db.appendPairVirtualTx(id, binding, events, caps, nowMs)
}

func (st *StateStore) PairVirtualActionRow(rt *basketRuntime, id pairVirtualIdentity, cloid string) (pairVirtualActionRow, error) {
	db, err := st.pairVirtualFile(rt)
	if err != nil {
		return pairVirtualActionRow{}, err
	}
	return db.pairVirtualActionRow(id, cloid)
}

func (st *StateStore) PairVirtualSlotTargets(rt *basketRuntime, slotMs int64) (string, bool, error) {
	db, err := st.pairVirtualFile(rt)
	if err != nil {
		return "", false, err
	}
	return db.pairVirtualSlotTargets(rt.Partition, rt.Contract.BasketID, slotMs)
}

type pairVirtualFileRows struct {
	Events      int
	Allocations int
	RunTags     []string
	Members     []string
}

func (r *pairVirtualFileRows) any() bool {
	return r != nil && (r.Events > 0 || r.Allocations > 0)
}

func (r *pairVirtualFileRows) describe() string {
	return fmt.Sprintf("virtual venue history: %d execution records and %d client identifier allocations for run tags %s", r.Events, r.Allocations, strings.Join(r.RunTags, ", "))
}

func (sdb *StateDB) loadPairVirtualRecordsInto(out *basketFileRecords) error {
	exists, err := tableExists(sdb, "pair_virtual_events")
	if err != nil || !exists {
		return err
	}
	get := func(k basketKey) *pairVirtualFileRows {
		if out.Virtual == nil {
			out.Virtual = map[basketKey]*pairVirtualFileRows{}
		}
		r := out.Virtual[k]
		if r == nil {
			r = &pairVirtualFileRows{}
			out.Virtual[k] = r
		}
		return r
	}
	add := func(list []string, v string) []string {
		for _, x := range list {
			if x == v {
				return list
			}
		}
		list = append(list, v)
		sort.Strings(list)
		return list
	}
	for _, table := range []string{"pair_virtual_events", "pair_virtual_allocations"} {
		rows, err := sdb.db.Query(`SELECT scope, source, pair_id, run_tag, strategy_id, COUNT(*) FROM ` + table + ` GROUP BY scope, source, pair_id, run_tag, strategy_id ORDER BY scope, source, pair_id, run_tag, strategy_id`)
		if err != nil {
			return fmt.Errorf("read %s in the %s state file: %w", table, sdb.storageRoleOf(), err)
		}
		for rows.Next() {
			var scope, source, pairID, runTag, sid string
			var n int
			if err := rows.Scan(&scope, &source, &pairID, &runTag, &sid, &n); err != nil {
				rows.Close()
				return err
			}
			procID, mapped := sdb.mapStoredStrategyID(sid)
			if !mapped {
				out.Unmapped = append(out.Unmapped, fmt.Sprintf("pair %s virtual venue history (%s, run %s) for stored strategy %q", pairID, table, runTag, sid))
				continue
			}
			r := get(basketKey{Partition: RiskPartition{Scope: PortfolioScope(scope), Source: source}, BasketID: pairID})
			if table == "pair_virtual_events" {
				r.Events += n
			} else {
				r.Allocations += n
			}
			r.RunTags = add(r.RunTags, runTag)
			r.Members = add(r.Members, procID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func inspectPairVirtualRows(db *StateDB, fi *storageFileInspection, ident storageIdentityMap, ownedScopes map[PortfolioScope]bool, ownedParts map[string]bool) ([]string, error) {
	exists, err := tableExists(db, "pair_virtual_events")
	if err != nil || !exists {
		return nil, err
	}
	var rejections []string
	owned := func(scope, source string) bool {
		p := RiskPartition{Scope: PortfolioScope(scope), Source: source}
		return ownedScopes[p.Scope] && (len(ownedParts) == 0 || ownedParts[p.String()])
	}
	for _, table := range []string{"pair_virtual_events", "pair_virtual_allocations"} {
		var n int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			return nil, err
		}
		if table == "pair_virtual_events" {
			fi.PairVirtualEvents = n
		} else {
			fi.PairVirtualAllocations = n
		}
		rows, err := db.db.Query(`SELECT DISTINCT scope, source, pair_id, run_tag, strategy_id FROM ` + table + ` ORDER BY scope, source, pair_id, run_tag, strategy_id`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var scope, source, pairID, runTag, sid string
			if err := rows.Scan(&scope, &source, &pairID, &runTag, &sid); err != nil {
				rows.Close()
				return nil, err
			}
			part := RiskPartition{Scope: PortfolioScope(scope), Source: source}
			if _, ok := ident.processFor(fi.Role, sid); !ok {
				rejections = append(rejections, fmt.Sprintf("%s state file: %s for pair %s run %s names stored strategy %q, which this file does not own", fi.Role, table, pairID, runTag, sid))
			}
			if !owned(scope, source) {
				rejections = append(rejections, fmt.Sprintf("%s state file: %s for pair %s run %s belongs to the %s partition but the file owns %s", fi.Role, table, pairID, runTag, partitionLabel(part), strings.Join(fi.PartitionsOwned, ", ")))
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	chains, err := db.db.Query(`SELECT DISTINCT run_tag, member_key_json, pair_id FROM pair_virtual_events ORDER BY run_tag, member_key_json`)
	if err != nil {
		return nil, err
	}
	type chainKey struct{ run, key, pair string }
	var keys []chainKey
	for chains.Next() {
		var k chainKey
		if err := chains.Scan(&k.run, &k.key, &k.pair); err != nil {
			chains.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	chains.Close()
	if err := chains.Err(); err != nil {
		return nil, err
	}
	for _, k := range keys {
		events, err := pairVirtualReadEvents(db.db, k.run, k.key)
		if err != nil {
			return nil, err
		}
		if err := pairVirtualCheckChain(k.run, k.key, events); err != nil {
			rejections = append(rejections, fmt.Sprintf("%s state file: pair %s run %s virtual venue history %s is corrupt: %v", fi.Role, k.pair, k.run, k.key, err))
			continue
		}
		fi.PairVirtualPending = append(fi.PairVirtualPending, pairVirtualUnresolved(k.pair, k.run, events)...)
	}
	return rejections, nil
}

func pairVirtualUnresolved(pairID, runTag string, events []pairVirtualEvent) []string {
	open := map[string]int64{}
	var ids []string
	for _, e := range events {
		switch e.Kind {
		case pairVirtualKindSubmit:
			open[e.ClientOrderID] = e.CreatedAtMs
			ids = append(ids, e.ClientOrderID)
		case pairVirtualKindMatch, pairVirtualKindAbsence:
			delete(open, e.ClientOrderID)
		}
	}
	var out []string
	for _, id := range ids {
		if at, ok := open[id]; ok {
			out = append(out, fmt.Sprintf("pair %s run %s client %s accepted at %s has no saved match or absence proof", pairID, runTag, id, formatClosedBarMs(at)))
		}
	}
	return out
}
