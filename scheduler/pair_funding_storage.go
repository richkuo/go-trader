package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var errPairFundingBinding = errors.New("pair funding binding is frozen")

const pairFundingDDL = `
CREATE TABLE IF NOT EXISTS pair_funding_events (
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
CREATE UNIQUE INDEX IF NOT EXISTS idx_pair_funding_one_applied
    ON pair_funding_events(scope, source, strategy_id, provider, account_key, instrument, event_ms) WHERE status = 'applied';
CREATE TRIGGER IF NOT EXISTS trg_pair_funding_events_applied_immutable
    BEFORE UPDATE ON pair_funding_events
    WHEN OLD.status IN ('applied','unresolvable')
      OR NEW.pair_id != OLD.pair_id OR NEW.contract_fingerprint != OLD.contract_fingerprint OR NEW.provider != OLD.provider
      OR NEW.account_key != OLD.account_key OR NEW.instrument != OLD.instrument OR NEW.event_ms != OLD.event_ms
      OR (OLD.source_hash != '' AND NEW.source_hash != OLD.source_hash)
    BEGIN SELECT RAISE(ABORT, 'an applied or unresolvable pair funding event and every event identity are immutable'); END;
CREATE TRIGGER IF NOT EXISTS trg_pair_funding_events_applied_kept
    BEFORE DELETE ON pair_funding_events
    WHEN OLD.status IN ('applied','unresolvable')
    BEGIN SELECT RAISE(ABORT, 'an applied or unresolvable pair funding event is never deleted'); END;
CREATE TABLE IF NOT EXISTS pair_funding_inventory (
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
CREATE UNIQUE INDEX IF NOT EXISTS idx_pair_funding_inventory_fill
    ON pair_funding_inventory(scope, source, pair_id, strategy_id, action_id, fill_id) WHERE fill_id != '';
CREATE TRIGGER IF NOT EXISTS trg_pair_funding_inventory_immutable
    BEFORE UPDATE ON pair_funding_inventory
    BEGIN SELECT RAISE(ABORT, 'pair funding inventory history is append-only'); END;
CREATE TRIGGER IF NOT EXISTS trg_pair_funding_inventory_kept
    BEFORE DELETE ON pair_funding_inventory
    BEGIN SELECT RAISE(ABORT, 'pair funding inventory history is append-only'); END;
CREATE TABLE IF NOT EXISTS pair_funding_chain (
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
CREATE TRIGGER IF NOT EXISTS trg_pair_funding_chain_immutable
    BEFORE UPDATE ON pair_funding_chain
    BEGIN SELECT RAISE(ABORT, 'a recorded pair funding batch is immutable'); END;
CREATE TRIGGER IF NOT EXISTS trg_pair_funding_chain_kept
    BEFORE DELETE ON pair_funding_chain
    BEGIN SELECT RAISE(ABORT, 'a recorded pair funding batch is immutable'); END;
`

func (sdb *StateDB) migratePairFundingSchema() error {
	if _, err := sdb.db.Exec(pairFundingDDL); err != nil {
		return fmt.Errorf("create pair funding tables: %w", err)
	}
	return nil
}

type pairFundingEventRow struct {
	PairID       string
	ContractFP   string
	StrategyID   string
	Provider     string
	AccountKey   string
	Instrument   string
	EventID      string
	RuleVersion  string
	EventMs      int64
	Status       string
	Reason       string
	Detail       string
	SourceHash   string
	EvidenceHash string
	USDAmount    string
	EventJSON    string
	CreatedMs    int64
	UpdatedMs    int64
}

type pairFundingFileRows struct {
	Events    []pairFundingEventRow
	Inventory map[string][]pairInventoryRow
	Chain     []pairFundingChainRow
}

func (r *pairFundingFileRows) any() bool {
	if r == nil {
		return false
	}
	if len(r.Events) > 0 || len(r.Chain) > 0 {
		return true
	}
	for _, rows := range r.Inventory {
		if len(rows) > 0 {
			return true
		}
	}
	return false
}

func (r *pairFundingFileRows) open() []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, e := range r.Events {
		if e.Status == pairFundingStatusIncomplete {
			out = append(out, fmt.Sprintf("member %s funding event %s (%s) incomplete: %s", e.StrategyID, e.EventID, e.RuleVersion, e.Reason))
		}
	}
	return out
}

func (sdb *StateDB) loadPairFundingRecordsInto(out *basketFileRecords) error {
	exists, err := tableExists(sdb, "pair_funding_events")
	if err != nil || !exists {
		return err
	}
	get := func(k basketKey) *pairFundingFileRows {
		if out.Funding == nil {
			out.Funding = map[basketKey]*pairFundingFileRows{}
		}
		r := out.Funding[k]
		if r == nil {
			r = &pairFundingFileRows{Inventory: map[string][]pairInventoryRow{}}
			out.Funding[k] = r
		}
		return r
	}
	rows, err := sdb.db.Query(`SELECT scope, source, pair_id, contract_fingerprint, strategy_id, provider, account_key, instrument, event_id, rule_version, event_ms,
		    status, reason, detail, source_hash, evidence_hash, usd_amount, event_json, created_at_ms, updated_at_ms
		FROM pair_funding_events ORDER BY scope, source, pair_id, strategy_id, event_ms, rule_version`)
	if err != nil {
		return fmt.Errorf("read pair funding events in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	for rows.Next() {
		var e pairFundingEventRow
		var scope, source, sid string
		if err := rows.Scan(&scope, &source, &e.PairID, &e.ContractFP, &sid, &e.Provider, &e.AccountKey, &e.Instrument, &e.EventID, &e.RuleVersion, &e.EventMs,
			&e.Status, &e.Reason, &e.Detail, &e.SourceHash, &e.EvidenceHash, &e.USDAmount, &e.EventJSON, &e.CreatedMs, &e.UpdatedMs); err != nil {
			rows.Close()
			return err
		}
		procID, mapped := sdb.mapStoredStrategyID(sid)
		if !mapped {
			out.Unmapped = append(out.Unmapped, fmt.Sprintf("pair %s funding event %s for stored strategy %q", e.PairID, e.EventID, sid))
			continue
		}
		e.StrategyID = procID
		k := basketKey{Partition: RiskPartition{Scope: PortfolioScope(scope), Source: source}, BasketID: e.PairID}
		r := get(k)
		r.Events = append(r.Events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = sdb.db.Query(`SELECT scope, source, pair_id, strategy_id, seq, fill_id, origin, action_id, position_id, delta_base_qty, post_base_qty, contract_size,
		    event_ms, event_latest_ms, visible_ms, recorded_at_ms FROM pair_funding_inventory ORDER BY scope, source, pair_id, strategy_id, seq`)
	if err != nil {
		return fmt.Errorf("read pair funding inventory in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	for rows.Next() {
		var row pairInventoryRow
		var scope, source, pairID, sid, delta, post, cs string
		if err := rows.Scan(&scope, &source, &pairID, &sid, &row.Seq, &row.FillID, &row.Origin, &row.ActionID, &row.PositionID, &delta, &post, &cs,
			&row.EventMs, &row.EventLatestMs, &row.VisibleMs, &row.RecordedAtMs); err != nil {
			rows.Close()
			return err
		}
		row.Delta, row.Post, row.ContractSize = pairDecimal(delta), pairDecimal(post), pairDecimal(cs)
		procID, mapped := sdb.mapStoredStrategyID(sid)
		if !mapped {
			out.Unmapped = append(out.Unmapped, fmt.Sprintf("pair %s funding inventory change %d for stored strategy %q", pairID, row.Seq, sid))
			continue
		}
		k := basketKey{Partition: RiskPartition{Scope: PortfolioScope(scope), Source: source}, BasketID: pairID}
		r := get(k)
		r.Inventory[procID] = append(r.Inventory[procID], row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = sdb.db.Query(`SELECT scope, source, pair_id, seq, source_id, batch_hash, prev_hash, batch_json FROM pair_funding_chain ORDER BY scope, source, pair_id, seq`)
	if err != nil {
		return fmt.Errorf("read pair funding chain in the %s state file: %w", sdb.storageRoleOf(), err)
	}
	defer rows.Close()
	for rows.Next() {
		var c pairFundingChainRow
		var scope, source, pairID, raw string
		if err := rows.Scan(&scope, &source, &pairID, &c.Seq, &c.SourceID, &c.BatchHash, &c.PrevHash, &raw); err != nil {
			return err
		}
		c.JSON = []byte(raw)
		k := basketKey{Partition: RiskPartition{Scope: PortfolioScope(scope), Source: source}, BasketID: pairID}
		r := get(k)
		r.Chain = append(r.Chain, c)
	}
	return rows.Err()
}

func pairFundingCheckJournal(memberID string, rows []pairInventoryRow) error {
	total := pairRatOf("0")
	for i, r := range rows {
		if r.Seq != int64(i)+1 {
			return fmt.Errorf("member %s inventory history has seq %d at position %d; the append-only history has a hole", memberID, r.Seq, i+1)
		}
		for name, d := range map[string]pairDecimal{"delta_base_qty": r.Delta, "post_base_qty": r.Post, "contract_size": r.ContractSize} {
			if _, err := d.rat(); err != nil {
				return fmt.Errorf("member %s inventory change %d %s: %v", memberID, r.Seq, name, err)
			}
		}
		if r.EventMs <= 0 || r.EventMs > r.EventLatestMs || r.EventLatestMs > r.VisibleMs {
			return fmt.Errorf("member %s inventory change %d has an invalid time [%d, %d] visible %d", memberID, r.Seq, r.EventMs, r.EventLatestMs, r.VisibleMs)
		}
		total.Add(total, pairRatOf(r.Delta))
		if total.Cmp(pairRatOf(r.Post)) != 0 {
			return fmt.Errorf("member %s inventory change %d records post quantity %s, the history sums to %s", memberID, r.Seq, r.Post, pairDecimalOfRat(total))
		}
	}
	return nil
}

func (st *StateStore) loadPairFunding(state *AppState, rt *basketRuntime, adm *pairResearchAdmission, rows *pairFundingFileRows) error {
	id := rt.Contract.BasketID
	f := newPairFundingRuntime(adm, rt.Contract)
	if rows == nil {
		rows = &pairFundingFileRows{Inventory: map[string][]pairInventoryRow{}}
	}
	for sid, inv := range rows.Inventory {
		m := f.members[sid]
		if m == nil {
			return fmt.Errorf("pair %s funding inventory names %s, which is not a pair member", id, sid)
		}
		if err := pairFundingCheckJournal(sid, inv); err != nil {
			return fmt.Errorf("pair %s: %w", id, err)
		}
		m.rows = append([]pairInventoryRow(nil), inv...)
		m.durableSeq = int64(len(inv))
	}
	for _, mid := range f.memberIDs() {
		m := f.members[mid]
		s := state.Strategies[mid]
		if s == nil {
			continue
		}
		if book := pairBookRat(s, m.coin); book.Cmp(m.total()) != 0 {
			return fmt.Errorf("pair %s resume refused: member %s saved inventory history sums to %s, but the saved book holds %s; the saved state is kept unchanged", id, mid, pairDecimalOfRat(m.total()), pairDecimalOfRat(book))
		}
	}
	for _, e := range rows.Events {
		m := f.members[e.StrategyID]
		if m == nil {
			return fmt.Errorf("pair %s funding event %s names %s, which is not a pair member", id, e.EventID, e.StrategyID)
		}
		if e.Provider != m.key.Provider || e.AccountKey != m.key.AccountKey || e.Instrument != m.key.Instrument {
			return fmt.Errorf("pair %s funding event %s for %s names %s|%s|%s, the member is %s", id, e.EventID, e.StrategyID, e.Provider, e.AccountKey, e.Instrument, m.key)
		}
		if want := pairFundingEventID(e.Provider, e.Instrument, e.EventMs); e.EventID != want {
			return fmt.Errorf("pair %s funding event %s is not the venue identity %s", id, e.EventID, want)
		}
		key := pairFundingKeyOf(e.StrategyID, e.EventID, e.RuleVersion)
		switch e.Status {
		case pairFundingStatusApplied:
			ev, err := pairDecodeFundingEvent([]byte(e.EventJSON))
			if err != nil {
				return fmt.Errorf("pair %s applied funding event %s is refused: %w", id, e.EventID, err)
			}
			if ev.Key != m.key || ev.EventID != e.EventID || ev.RuleVersion != e.RuleVersion || ev.EvidenceHash != e.EvidenceHash || ev.SourceHash != e.SourceHash || string(ev.USDAmount) != e.USDAmount {
				return fmt.Errorf("pair %s applied funding event %s columns disagree with its saved event bytes", id, e.EventID)
			}
			if e.ContractFP == rt.Contract.Fingerprint() && !adm.admittedEvidence().hasConversion(ev.Conversion) {
				return fmt.Errorf("pair %s applied funding event %s uses conversion %s, which is not a record of the bound evidence document", id, e.EventID, ev.Conversion.EvidenceID)
			}
			rate := pairFundingRate{Key: ev.Key, EventID: ev.EventID, RuleVersion: ev.RuleVersion, EventMs: ev.EventMs, Rate: ev.Rate}
			sh, err := pairFundingSourceHash(rate)
			if err != nil || sh != e.SourceHash {
				return fmt.Errorf("pair %s applied funding event %s source hash %s is not the hash of its saved rate record; the saved state is kept unchanged", id, e.EventID, shortFingerprint(e.SourceHash))
			}
			f.hashes[pairFundingIdentity(ev.Key, ev.EventID, ev.RuleVersion)] = pairFundingHashMemo{eventMs: ev.EventMs, rate: ev.Rate, hash: sh}
			f.applied[key] = pairFundingAppliedRef{EventMs: e.EventMs, SourceHash: e.SourceHash, EvidenceHash: e.EvidenceHash}
			if e.EventMs > m.maxAppliedMs {
				m.maxAppliedMs = e.EventMs
			}
		case pairFundingStatusIncomplete, pairFundingStatusVoid, pairFundingStatusUnresolvable:
			f.incomplete[key] = &pairFundingIncomplete{MemberID: e.StrategyID, EventID: e.EventID, RuleVersion: e.RuleVersion, EventMs: e.EventMs, Status: e.Status, Reason: e.Reason, Detail: e.Detail, SourceHash: e.SourceHash, CreatedMs: e.CreatedMs, UpdatedMs: e.UpdatedMs}
		default:
			return fmt.Errorf("pair %s funding event %s has unknown status %q", id, e.EventID, e.Status)
		}
	}
	if err := pairFundingCheckChain(adm.funding, rows.Chain); err != nil {
		return fmt.Errorf("pair %s resume refused: %w; the saved state is kept unchanged", id, err)
	}
	f.chainHead = int64(len(rows.Chain))
	f.chainSaved = rows.Chain
	rt.Pair.funding = f
	for _, mid := range f.memberIDs() {
		if s := state.Strategies[mid]; s != nil {
			s.pairFunding = f.members[mid]
		}
	}
	return nil
}

func pairFundingCheckChain(src pairFundingSource, saved []pairFundingChainRow) error {
	rs, recorded := src.(*pairRecordedFundingSource)
	if !recorded {
		if len(saved) > 0 {
			return fmt.Errorf("the saved run holds %d recorded funding batches, but this run uses a %s funding source", len(saved), src.Kind())
		}
		return nil
	}
	chain := rs.chain()
	if len(saved) > len(chain) {
		return fmt.Errorf("the saved run holds %d recorded funding batches, the recorded source only %d; saved evidence is never dropped", len(saved), len(chain))
	}
	for i, row := range saved {
		b := chain[i]
		if row.Seq != int64(i)+1 || row.Seq != b.Seq || row.BatchHash != b.BatchHash || row.PrevHash != b.PrevHash || row.SourceID != b.SourceID {
			return fmt.Errorf("saved recorded funding batch %d (%s) is not the source's batch %d (%s); prior evidence is immutable", row.Seq, shortFingerprint(row.BatchHash), b.Seq, shortFingerprint(b.BatchHash))
		}
		if !bytes.Equal(row.JSON, rs.chainRaw(b.Seq)) {
			return fmt.Errorf("saved recorded funding batch %d bytes differ from the source's batch", row.Seq)
		}
	}
	return nil
}

func pairFundingReadSavedChain(path string, part RiskPartition, pairID string) ([]pairFundingChainRow, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	db, err := openPairFundingSnapshot(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	exists, err := tableExists(db, "pair_funding_chain")
	if err != nil || !exists {
		return nil, err
	}
	rows, err := db.db.Query(`SELECT seq, source_id, batch_hash, prev_hash, batch_json FROM pair_funding_chain WHERE scope = ? AND source = ? AND pair_id = ? ORDER BY seq`, string(part.Scope), part.Source, pairID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pairFundingChainRow
	for rows.Next() {
		var c pairFundingChainRow
		var raw string
		if err := rows.Scan(&c.Seq, &c.SourceID, &c.BatchHash, &c.PrevHash, &raw); err != nil {
			return nil, err
		}
		c.JSON = []byte(raw)
		out = append(out, c)
	}
	return out, rows.Err()
}

func openPairFundingSnapshot(path string) (*StateDB, error) {
	if _, err := os.Stat(path + "-wal"); err == nil {
		return openStateDBReadOnly(path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&immutable=1"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db snapshot: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &StateDB{db: db, path: path, role: storageRolePrimary, readOnly: true}, nil
}

func checkSavedPairFundingBindingTx(tx *sql.Tx, rt *basketRuntime) error {
	if rt.Pair.funding == nil {
		return nil
	}
	var raw string
	err := tx.QueryRow(`SELECT progress_json FROM basket_contracts WHERE scope = ? AND source = ? AND basket_id = ?`,
		string(rt.Partition.Scope), rt.Partition.Source, rt.Contract.BasketID).Scan(&raw)
	next := rt.Pair.Progress.FundingBinding
	if errors.Is(err, sql.ErrNoRows) {
		if len(next) == 0 {
			return fmt.Errorf("%w: pair %s has no funding binding to save", errPairFundingBinding, rt.Contract.BasketID)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pair %s saved progress: %w", rt.Contract.BasketID, err)
	}
	saved, err := parsePairProgress(raw)
	if err != nil {
		return fmt.Errorf("%w: pair %s: %v", errPairFundingBinding, rt.Contract.BasketID, err)
	}
	if len(saved.FundingBinding) == 0 {
		return fmt.Errorf("%w: pair %s was saved without a funding binding; a write never adds a binding to a pre-funding run", errPairFundingBinding, rt.Contract.BasketID)
	}
	if len(next) == 0 {
		return fmt.Errorf("%w: pair %s progress would remove its saved funding binding", errPairFundingBinding, rt.Contract.BasketID)
	}
	if err := pairFundingCompareBinding(saved.FundingBinding, next); err != nil {
		return fmt.Errorf("%w: pair %s progress would replace its saved funding binding: %v", errPairFundingBinding, rt.Contract.BasketID, err)
	}
	return nil
}

func (sdb *StateDB) writePairFundingTx(tx *sql.Tx, rt *basketRuntime, sids map[string]string, books map[string]*StrategyState, nowMs int64) error {
	f := rt.Pair.funding
	if f == nil {
		return nil
	}
	scope, source := string(rt.Partition.Scope), rt.Partition.Source
	pairID := rt.Contract.BasketID
	for _, id := range f.memberIDs() {
		s := books[id]
		if s == nil {
			continue
		}
		m := f.members[id]
		m.reconcile(pairBookRat(s, m.coin), pairBookPositionID(s, m.coin), nowMs)
		for _, r := range m.rows {
			if r.Seq <= m.durableSeq {
				continue
			}
			res, err := tx.Exec(`INSERT INTO pair_funding_inventory (scope, source, pair_id, strategy_id, seq, fill_id, origin, action_id, position_id, delta_base_qty, post_base_qty,
				    contract_size, event_ms, event_latest_ms, visible_ms, recorded_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(scope, source, pair_id, strategy_id, seq) DO NOTHING`,
				scope, source, pairID, sids[id], r.Seq, r.FillID, r.Origin, r.ActionID, r.PositionID, string(r.Delta), string(r.Post), string(r.ContractSize),
				r.EventMs, r.EventLatestMs, r.VisibleMs, r.RecordedAtMs)
			if err != nil {
				return fmt.Errorf("record pair %s member %s inventory change %d: %w", pairID, id, r.Seq, err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				var post string
				if err := tx.QueryRow(`SELECT post_base_qty FROM pair_funding_inventory WHERE scope = ? AND source = ? AND pair_id = ? AND strategy_id = ? AND seq = ?`,
					scope, source, pairID, sids[id], r.Seq).Scan(&post); err != nil || post != string(r.Post) {
					return fmt.Errorf("pair %s member %s inventory change %d already exists with different content: %w", pairID, id, r.Seq, errPairGuardMiss)
				}
			}
		}
	}
	keys := make([]string, 0, len(f.incomplete))
	for k := range f.incomplete {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		in := f.incomplete[k]
		if !in.dirty {
			continue
		}
		if _, staged := f.pending[k]; staged {
			continue
		}
		if _, done := f.applied[k]; done {
			continue
		}
		m := f.members[in.MemberID]
		res, err := tx.Exec(`INSERT INTO pair_funding_events (scope, source, pair_id, contract_fingerprint, strategy_id, provider, account_key, instrument, event_id,
			    rule_version, event_ms, status, reason, detail, source_hash, created_at_ms, updated_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(scope, source, strategy_id, event_id, rule_version) DO UPDATE SET
			    status = excluded.status, reason = excluded.reason, detail = excluded.detail,
			    source_hash = CASE WHEN source_hash = '' THEN excluded.source_hash ELSE source_hash END,
			    updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)
			WHERE status NOT IN ('applied','unresolvable')`,
			scope, source, pairID, rt.Contract.Fingerprint(), sids[in.MemberID], m.key.Provider, m.key.AccountKey, m.key.Instrument, in.EventID,
			in.RuleVersion, in.EventMs, in.Status, in.Reason, in.Detail, in.SourceHash, in.CreatedMs, in.UpdatedMs)
		if err != nil {
			return fmt.Errorf("record pair %s funding event %s for %s: %w", pairID, in.EventID, in.MemberID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("pair %s funding event %s for %s is already applied or unresolvable: %w", pairID, in.EventID, in.MemberID, errPairGuardMiss)
		}
	}
	pkeys := make([]string, 0, len(f.pending))
	for k := range f.pending {
		pkeys = append(pkeys, k)
	}
	sort.Strings(pkeys)
	for _, k := range pkeys {
		app := f.pending[k]
		if books[app.MemberID] == nil {
			continue
		}
		m := f.members[app.MemberID]
		ev := app.Event
		res, err := tx.Exec(`INSERT INTO pair_funding_events (scope, source, pair_id, contract_fingerprint, strategy_id, provider, account_key, instrument, event_id,
			    rule_version, event_ms, status, reason, detail, source_hash, evidence_hash, usd_amount, event_json, created_at_ms, updated_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'applied', '', '', ?, ?, ?, ?, ?, ?)
			ON CONFLICT(scope, source, strategy_id, event_id, rule_version) DO UPDATE SET
			    status = 'applied', reason = '', detail = '', source_hash = excluded.source_hash, evidence_hash = excluded.evidence_hash,
			    usd_amount = excluded.usd_amount, event_json = excluded.event_json, updated_at_ms = MAX(updated_at_ms, excluded.updated_at_ms)
			WHERE status NOT IN ('applied','unresolvable') AND (source_hash = '' OR source_hash = excluded.source_hash)`,
			scope, source, pairID, rt.Contract.Fingerprint(), sids[app.MemberID], m.key.Provider, m.key.AccountKey, m.key.Instrument, ev.EventID,
			ev.RuleVersion, ev.EventMs, ev.SourceHash, ev.EvidenceHash, string(ev.USDAmount), string(app.JSON), app.AtMs, app.AtMs)
		if err != nil {
			return fmt.Errorf("apply pair %s funding event %s for %s: %w", pairID, ev.EventID, app.MemberID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("pair %s funding event %s for %s is already applied or carries other source evidence: %w", pairID, ev.EventID, app.MemberID, errPairGuardMiss)
		}
	}
	for _, c := range f.recordedChainPending() {
		if _, err := tx.Exec(`INSERT INTO pair_funding_chain (scope, source, pair_id, seq, source_id, batch_hash, prev_hash, batch_json, created_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, scope, source, pairID, c.Seq, c.SourceID, c.BatchHash, c.PrevHash, string(c.JSON), nowMs); err != nil {
			return fmt.Errorf("record pair %s funding batch %d: %w", pairID, c.Seq, err)
		}
	}
	return nil
}

func markPairFundingDurable(rt *basketRuntime, books map[string]*StrategyState) {
	f := rt.Pair.funding
	if f == nil {
		return
	}
	for id, m := range f.members {
		if books[id] != nil {
			m.durableSeq = int64(len(m.rows))
		}
	}
	for _, in := range f.incomplete {
		in.dirty = false
	}
	for k, app := range f.pending {
		if books[app.MemberID] == nil {
			continue
		}
		f.applied[k] = pairFundingAppliedRef{EventMs: app.Event.EventMs, SourceHash: app.Event.SourceHash, EvidenceHash: app.Event.EvidenceHash}
		delete(f.pending, k)
	}
	if pending := f.recordedChainPending(); len(pending) > 0 {
		f.chainHead = pending[len(pending)-1].Seq
	}
}

func bindPairFundingBinding(rt *basketRuntime, adm *pairResearchAdmission, saved bool) error {
	id := rt.Contract.BasketID
	want := adm.fundingBinding
	if len(want) == 0 {
		return fmt.Errorf("pair %s: the admission has no funding binding", id)
	}
	if got := rt.Pair.Progress.FundingBinding; len(got) > 0 {
		if err := pairFundingCompareBinding(want, got); err != nil {
			return fmt.Errorf("pair %s resume refused: %w; the saved state is kept unchanged, and changed funding input or venue rules need a new research run", id, err)
		}
		return nil
	}
	if saved {
		return fmt.Errorf("pair %s resume refused: the saved run has pair state but no funding binding; a pre-funding pair run needs a new research run, and its history is never charged retroactively", id)
	}
	rt.Pair.Progress.FundingBinding = append([]byte(nil), want...)
	return nil
}

func inspectPairFundingRows(db *StateDB, fi *storageFileInspection, ident storageIdentityMap, ownedScopes map[PortfolioScope]bool, ownedParts map[string]bool) ([]string, error) {
	exists, err := tableExists(db, "pair_funding_events")
	if err != nil || !exists {
		return nil, err
	}
	var rejections []string
	owned := func(scope, source string) bool {
		p := RiskPartition{Scope: PortfolioScope(scope), Source: source}
		return ownedScopes[p.Scope] && (len(ownedParts) == 0 || ownedParts[p.String()])
	}
	rows, err := db.db.Query(`SELECT scope, source, pair_id, strategy_id, event_id, rule_version, status, reason, usd_amount FROM pair_funding_events ORDER BY scope, source, pair_id, event_ms, strategy_id`)
	if err != nil {
		return nil, fmt.Errorf("read pair funding events in the %s state file: %w", fi.Role, err)
	}
	for rows.Next() {
		var scope, source, pairID, sid, eventID, rule, status, reason, usd string
		if err := rows.Scan(&scope, &source, &pairID, &sid, &eventID, &rule, &status, &reason, &usd); err != nil {
			rows.Close()
			return nil, err
		}
		fi.PairFunding++
		owner := sid
		if procID, ok := ident.processFor(fi.Role, sid); ok {
			owner = procID
		} else {
			rejections = append(rejections, fmt.Sprintf("%s state file: pair funding event %s names stored strategy %q, which this file does not own", fi.Role, eventID, sid))
		}
		part := RiskPartition{Scope: PortfolioScope(scope), Source: source}
		if !owned(scope, source) {
			rejections = append(rejections, fmt.Sprintf("%s state file: pair funding event %s belongs to the %s partition but the file owns %s", fi.Role, eventID, partitionLabel(part), strings.Join(fi.PartitionsOwned, ", ")))
		}
		switch status {
		case pairFundingStatusIncomplete:
			fi.PairFundingPending = append(fi.PairFundingPending, fmt.Sprintf("%s %s member %s funding event %s (%s): incomplete, %s", partitionLabel(part), pairID, owner, eventID, rule, reason))
		case pairFundingStatusApplied:
			fi.PairFundingApplied++
		case pairFundingStatusUnresolvable:
			fi.PairFundingUnresolvable = append(fi.PairFundingUnresolvable, fmt.Sprintf("%s %s member %s funding event %s (%s): unresolvable, %s", partitionLabel(part), pairID, owner, eventID, rule, reason))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var inv int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM pair_funding_inventory`).Scan(&inv); err != nil {
		return nil, err
	}
	fi.PairFundingInventory = inv
	invRows, err := db.db.Query(`SELECT DISTINCT scope, source, strategy_id FROM pair_funding_inventory ORDER BY scope, source, strategy_id`)
	if err != nil {
		return nil, err
	}
	for invRows.Next() {
		var scope, source, sid string
		if err := invRows.Scan(&scope, &source, &sid); err != nil {
			invRows.Close()
			return nil, err
		}
		if _, ok := ident.processFor(fi.Role, sid); !ok {
			rejections = append(rejections, fmt.Sprintf("%s state file: pair funding inventory names stored strategy %q, which this file does not own", fi.Role, sid))
		}
		if !owned(scope, source) {
			rejections = append(rejections, fmt.Sprintf("%s state file: pair funding inventory for %q belongs to the %s partition but the file owns %s", fi.Role, sid, partitionLabel(RiskPartition{Scope: PortfolioScope(scope), Source: source}), strings.Join(fi.PartitionsOwned, ", ")))
		}
	}
	invRows.Close()
	return rejections, invRows.Err()
}
