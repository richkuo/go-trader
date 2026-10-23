package main

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func quoteSettledEnv(t *testing.T, o quoteTestOptions) *quoteTestEnv {
	t.Helper()
	e := newQuoteTestEnv(t, o)
	e.mustRun(1, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
	bid := e.sides(1)[quoteRoleBid]
	e.venue.fill(t, bid.ClientOrderID, "0.1", "99", "0.01", e.deadlineMs(1)+1000)
	e.mustRun(2, 100, e.pair("99", "101", "0.2"), quoteExitRequest{})
	ratEq(t, "settled inventory", e.contract().Book.Qty, "0.1")
	return e
}

func TestQuoteRevisionGuardsEverySaveFormAndRetainsEvidence(t *testing.T) {
	e := quoteSettledEnv(t, quoteTestOptions{})
	st := e.adm.state
	mu := e.adm.mu
	stale := cloneQuoteStrategy(st.Strategies[quoteTestOwner])
	stale.Quote.Revision--

	mu.Lock()
	if err := e.store.SaveStrategyBook(st.Strategies[quoteTestOwner]); err != nil {
		mu.Unlock()
		t.Fatalf("a save that carries the durable revision and settled fields must pass: %v", err)
	}
	if errs := e.store.SaveAll(st); len(errs) != 0 {
		for _, err := range errs {
			if err != nil {
				mu.Unlock()
				t.Fatalf("a fresh full save must pass: %v", err)
			}
		}
	}
	mu.Unlock()

	staleState := &AppState{Strategies: map[string]*StrategyState{quoteTestOwner: stale}, PortfolioRisk: st.PortfolioRisk}
	if err := e.store.SaveStrategyBook(stale); !errors.Is(err, errQuoteRevisionStale) {
		t.Fatalf("a single-strategy save with a stale revision must be refused: %v", err)
	}
	if err := e.store.SavePartition(staleState, e.part); !errors.Is(err, errQuoteRevisionStale) {
		t.Fatalf("a partition save with a stale revision must be refused: %v", err)
	}
	if err := SaveStateWithStore(staleState, e.store); err == nil {
		t.Fatal("a full save with a stale revision must be refused")
	}
	omitted := &AppState{Strategies: map[string]*StrategyState{}, PortfolioRisk: st.PortfolioRisk}
	if err := SaveStateWithStore(omitted, e.store); err == nil {
		t.Fatal("a full save that omits a durable quote owner must be refused")
	}
	tampered := cloneQuoteStrategy(st.Strategies[quoteTestOwner])
	tampered.Cash += 5
	if err := e.store.SaveStrategyBook(tampered); err == nil {
		t.Fatal("an ordinary save that changes a quote owner's cash must be refused")
	}
	fundingTamper := cloneQuoteStrategy(st.Strategies[quoteTestOwner])
	fundingTamper.PaperFunding.ObservedThroughMs++
	if err := e.store.SaveStrategyBook(fundingTamper); err == nil {
		t.Fatal("an ordinary save that changes a quote owner's funding state must be refused")
	}
	posTamper := cloneQuoteStrategy(st.Strategies[quoteTestOwner])
	posTamper.Positions["BTC"].Quantity = 0.2
	if err := e.store.SaveStrategyBook(posTamper); err == nil {
		t.Fatal("an ordinary save that changes quote inventory must be refused")
	}
	if got := e.contract().Book.Qty; got != "1/10" && got != "0.1" {
		ratEq(t, "durable inventory after refused saves", got, "0.1")
	}
	if _, err := e.db().db.Exec(`DELETE FROM quote_fills`); err == nil {
		t.Fatal("quote fill evidence must not be deletable")
	}
	if _, err := e.db().db.Exec(`UPDATE quote_fills SET fee = '0'`); err == nil {
		t.Fatal("quote fill evidence must be immutable")
	}
	if _, err := e.db().db.Exec(`DELETE FROM pending_limit_orders WHERE owner_kind = 'quote'`); err == nil {
		t.Fatal("quote order evidence must be retained")
	}
	if _, err := e.db().db.Exec(`DELETE FROM strategies WHERE id = ?`, quoteTestOwner); err != nil {
		t.Fatalf("ordinary strategy row replacement: %v", err)
	}
	if n := e.count(`SELECT COUNT(*) FROM quote_fills`); n != 1 {
		t.Fatalf("retained fill evidence must survive strategy and position deletion: %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM pending_limit_orders WHERE owner_kind = 'quote'`); n < 3 {
		t.Fatalf("retained order evidence must survive strategy deletion: %d", n)
	}
}

func TestQuoteRowsAreInvisibleToEveryLegacyPendingLimitPath(t *testing.T) {
	e := quoteSettledEnv(t, quoteTestOptions{})
	var quoteID int64
	if err := e.db().db.QueryRow(`SELECT id FROM pending_limit_orders WHERE owner_kind = 'quote' ORDER BY id LIMIT 1`).Scan(&quoteID); err != nil {
		t.Fatal(err)
	}
	manualID, err := e.store.InsertPendingLimitOrder(PendingLimitOrder{StrategyID: quoteTestOwner, Symbol: "ETH", Side: "buy", OrderOID: 7, LimitPrice: 1, OrderSize: 1, TIF: "Alo", CreatedAt: e.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := e.store.LoadPendingLimitOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 1 || legacy[0].ID != manualID {
		t.Fatalf("legacy reads must see only the manual row: %+v", legacy)
	}
	before := e.quoteRowSignature(t)
	_ = e.store.UpdatePendingLimitOrderFill(quoteID, 9, 9, 9)
	_ = e.store.MarkPendingLimitOrderOperatorRequired(quoteID, e.clock.Now())
	_ = e.store.ClearPendingLimitOrderOperatorRequired(quoteID)
	_ = e.store.DeletePendingLimitOrder(quoteID)
	if _, err := e.store.MarkPendingLimitOrderCancelRequested(quoteTestOwner, "BTC"); err != nil {
		t.Fatal(err)
	}
	if n, err := e.store.CountPendingLimitOrders(quoteTestOwner, "BTC"); err != nil || n != 0 {
		t.Fatalf("legacy tallies must not count quote rows: %d %v", n, err)
	}
	if after := e.quoteRowSignature(t); after != before {
		t.Fatal("a legacy row-id mutation changed a quote row")
	}
	combined, err := e.store.LoadPendingOrdersCombined()
	if err != nil {
		t.Fatal(err)
	}
	if len(combined.Manual) != 1 || len(combined.Quote) < 3 {
		t.Fatalf("combined read %d manual %d quote", len(combined.Manual), len(combined.Quote))
	}
	for _, q := range combined.Quote {
		if q.StrategyID != quoteTestOwner || q.Partition != e.part || q.SourceRole == "" {
			t.Fatalf("a combined quote row must carry its owner: %+v", q)
		}
	}
}

func (e *quoteTestEnv) quoteRowSignature(t *testing.T) [32]byte {
	t.Helper()
	rows, err := e.db().db.Query(`SELECT id, submit_state, cancel_state, confirmed_qty, applied_qty, filled_size, operator_required_since, cancel_requested FROM pending_limit_orders WHERE owner_kind = 'quote' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var id int64
		var a, b, c, d, f string
		var fs float64
		var cr int
		if err := rows.Scan(&id, &a, &b, &c, &d, &fs, &f, &cr); err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(a + b + c + d + f))
		h.Write([]byte{byte(id), byte(cr), byte(int64(fs * 1000))})
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func TestQuoteStorageMatrixIsolatesFilesPartitionsAndRowIDs(t *testing.T) {
	e := quoteSettledEnv(t, quoteTestOptions{paperSrc: true})
	if e.part.Source != "lab" || e.db().storageRoleOf() != paperSourceRole("lab") {
		t.Fatalf("the owner must live in the named paper file: %s %s", partitionLabel(e.part), e.db().storageRoleOf())
	}
	primary := e.store.primary()
	if _, err := primary.db.Exec(`INSERT INTO pending_limit_orders (strategy_id, symbol, side, order_oid, limit_price, order_size, created_at) VALUES ('manual-x', 'BTC', 'buy', 1, 1, 1, '2026-10-20T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var quoteID int64
	if err := e.db().db.QueryRow(`SELECT MIN(id) FROM pending_limit_orders WHERE owner_kind = 'quote'`).Scan(&quoteID); err != nil {
		t.Fatal(err)
	}
	var manualID int64
	if err := primary.db.QueryRow(`SELECT MIN(id) FROM pending_limit_orders`).Scan(&manualID); err != nil {
		t.Fatal(err)
	}
	if quoteID != manualID {
		t.Fatalf("the matrix needs equal numeric row ids in two files: quote %d manual %d", quoteID, manualID)
	}
	wrong := quoteOwnerKey{Partition: defaultPaperPartition, StrategyID: quoteTestOwner, sid: quoteTestOwner}
	row := &quoteOrderRow{RowID: quoteID, SubmitState: quoteSubmitCancelled, ConfirmedQty: "0"}
	if err := e.db().updateQuoteOrder(wrong, row, nil, e.clock.Now().UnixMilli()); err == nil {
		t.Fatal("a row id under the wrong partition must change nothing")
	}
	if err := primary.updateQuoteOrder(quoteOwnerKey{Partition: e.part, StrategyID: quoteTestOwner, sid: quoteTestOwner}, row, nil, e.clock.Now().UnixMilli()); err == nil {
		t.Fatal("a row id in the wrong file must change nothing")
	}
	if n := e.count(`SELECT COUNT(*) FROM pending_limit_orders WHERE id = ? AND submit_state = 'cancelled'`, quoteID); n != 0 {
		t.Fatal("the wrong-file mutation changed the quote row")
	}
	combined, err := e.store.LoadPendingOrdersCombined()
	if err != nil {
		t.Fatal(err)
	}
	if len(combined.Manual) != 1 || combined.Manual[0].ID != manualID {
		t.Fatalf("manual rows %+v", combined.Manual)
	}
	for _, q := range combined.Quote {
		if q.SourceRole != paperSourceRole("lab") {
			t.Fatalf("quote row source %s", q.SourceRole)
		}
	}
	if _, err := e.db().db.Exec(`ALTER TABLE pending_limit_orders RENAME TO pending_limit_orders_gone`); err != nil {
		t.Fatal(err)
	}
	if got, err := e.store.LoadPendingOrdersCombined(); err == nil {
		t.Fatalf("a failed file query must fail the whole combined read, got %d manual rows", len(got.Manual))
	}
	if _, err := e.db().db.Exec(`ALTER TABLE pending_limit_orders_gone RENAME TO pending_limit_orders`); err != nil {
		t.Fatal(err)
	}
}

func TestQuoteInspectionReadsOldAndNewSchemasWithoutWriting(t *testing.T) {
	e := quoteSettledEnv(t, quoteTestOptions{})
	layout, err := resolveStorageLayout(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident, _ := buildStorageIdentityMap(e.cfg, layout)
	path := layout.Files[0].Path
	e.close()
	sum := func() [32]byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(b)
	}
	before := sum()
	admitted, err := inspectStorageOwnership(layout, ident, e.cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if !admitted.OK() || admitted.Files[0].QuoteOwners != 1 || len(admitted.Files[0].QuotePending) == 0 {
		t.Fatalf("an admitted owner with pending evidence must inspect cleanly: %+v %v", admitted.Files, admitted.Rejections)
	}
	var production Config = *e.cfg
	production.quoteLoad = nil
	refused, err := inspectStorageOwnership(layout, ident, &production, false)
	if err != nil {
		t.Fatal(err)
	}
	if refused.OK() {
		t.Fatal("production inspection must refuse unresolved quote evidence with no admitted owner")
	}
	if after := sum(); after != before {
		t.Fatal("inspection wrote to the state file")
	}

	old := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE pending_limit_orders (id INTEGER PRIMARY KEY AUTOINCREMENT, strategy_id TEXT NOT NULL, symbol TEXT NOT NULL, side TEXT NOT NULL,
		order_oid INTEGER NOT NULL, limit_price REAL NOT NULL, order_size REAL NOT NULL, tif TEXT NOT NULL DEFAULT 'Alo', filled_size REAL NOT NULL DEFAULT 0,
		avg_fill_price REAL NOT NULL DEFAULT 0, fill_fee REAL NOT NULL DEFAULT 0, entry_atr REAL NOT NULL DEFAULT 0, cancel_requested INTEGER NOT NULL DEFAULT 0,
		operator_required_since TEXT NOT NULL DEFAULT '', expires_at TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
		INSERT INTO pending_limit_orders (strategy_id, symbol, side, order_oid, limit_price, order_size, created_at) VALUES ('m', 'BTC', 'buy', 5, 1, 1, '2026-01-01T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	oldSum, _ := os.ReadFile(old)
	ro, err := openStateDBReadOnly(old)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ro.LoadPendingLimitOrders()
	if err != nil || len(rows) != 1 {
		t.Fatalf("a read-only old schema must project legacy rows as manual: %v %d", err, len(rows))
	}
	fi := &storageFileInspection{Role: storageRolePrimary, PartitionsOwned: []string{"live"}}
	if rej, err := inspectQuoteRows(ro, fi, storageIdentityMap{}, nil); err != nil || len(rej) != 0 {
		t.Fatalf("an unmigrated file inspects with no quote evidence: %v %v", rej, err)
	}
	ro.Close()
	if now, _ := os.ReadFile(old); string(now) != string(oldSum) {
		t.Fatal("read-only inspection changed the old file")
	}
	migrated, err := OpenStateDB(old)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var kind, scope string
	if err := migrated.db.QueryRow(`SELECT owner_kind, partition_scope FROM pending_limit_orders`).Scan(&kind, &scope); err != nil || kind != "manual" || scope != "live" {
		t.Fatalf("migration must keep existing rows manual and live: %q %q %v", kind, scope, err)
	}
	if err := migrated.migrateQuoteSchema(); err != nil {
		t.Fatalf("a second migration must be idempotent: %v", err)
	}
}

func TestQuoteRowsGoToTheQuoteOwnerBeforeTheManualGates(t *testing.T) {
	e := quoteSettledEnv(t, quoteTestOptions{})
	before := e.quoteRowSignature(t)
	e.adm.Close()
	if lines := dispatchQuotePendingRows(e.store, mustCombinedQuoteRows(t, e)); len(lines) == 0 {
		t.Fatal("unowned quote rows must be reported as held")
	}
	alerts := reconcilePendingLimitOrders(e.adm.state, e.cfg, e.store, e.adm.mu, nil, nil)
	if len(alerts) != 0 {
		t.Fatalf("the manual path must not act on quote rows: %+v", alerts)
	}
	if after := e.quoteRowSignature(t); after != before {
		t.Fatal("with no admitted owner the orchestrator must leave quote rows untouched")
	}
	quoteBoundAdmissions.Store(e.store, e.adm)
	e.venue.cancelHook = nil
	g := e.sides(1)
	if err := e.adm.saveOutcomeForTest(g[quoteRoleAsk], quoteCancelRequested); err != nil {
		t.Fatal(err)
	}
	reconcilePendingLimitOrders(e.adm.state, e.cfg, e.store, e.adm.mu, nil, nil)
	if got := e.sides(1)[quoteRoleAsk]; got.SubmitState != quoteSubmitCancelled {
		t.Fatalf("the orchestrator must hand owned quote rows to the quote owner, which resolves the requested cancel: %+v", got)
	}
}

func mustCombinedQuoteRows(t *testing.T, e *quoteTestEnv) []*quoteOrderRow {
	t.Helper()
	c, err := e.store.LoadPendingOrdersCombined()
	if err != nil {
		t.Fatal(err)
	}
	return c.Quote
}

func (a *quoteAdmission) saveOutcomeForTest(o *quoteOrderRow, cancel string) error {
	from := o.SubmitState
	o.CancelState = cancel
	return a.saveOutcome(o, []string{from})
}

func TestQuoteStoredIdentityTranslationStaysInsideTheStateDB(t *testing.T) {
	e := quoteSettledEnv(t, quoteTestOptions{paperDB: true, strategy: map[string]any{"storage_strategy_id": "stored-quote-btc"}})
	if n := e.count(`SELECT COUNT(*) FROM quote_contracts WHERE strategy_id = 'stored-quote-btc'`); n != 1 {
		t.Fatalf("the contract must be keyed by the stored identifier: %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM pending_limit_orders WHERE owner_kind = 'quote' AND strategy_id != 'stored-quote-btc'`); n != 0 {
		t.Fatalf("%d quote rows leaked the process identifier into storage", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM strategies WHERE id = 'stored-quote-btc'`); n != 1 {
		t.Fatal("the settled book must be written under the stored identifier")
	}
	combined, err := e.store.LoadPendingOrdersCombined()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range combined.Quote {
		if q.StrategyID != quoteTestOwner || q.SourceRole != storageRolePaper {
			t.Fatalf("a combined read must translate back to the process id in the paper file: %+v", q)
		}
	}
	e.reopen()
	if s := e.owner(); s.Quote == nil || s.Quote.Revision != e.contract().Revision || s.Positions["BTC"] == nil {
		t.Fatalf("the translated owner must reload: %+v", s.Quote)
	}
}
