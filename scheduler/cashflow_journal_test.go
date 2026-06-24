package main

import (
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// #1100 exchange-sourced equity journal: settled-cash convention, the equity
// equation, cursor discipline, dedup, baseline anchoring, and unmapped-kind
// fail-closed latching.

func newCashflowJournalTestDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// A fill's settled-cash delta is realized PnL (GROSS) minus the fee actually
// charged: opens settle -fee, closes settle closedPnl-fee, and a maker rebate
// (negative fee) ADDS to cash. closedPnl is gross of fees (#698) so the fee is
// subtracted exactly once — never twice, never zero times.
func TestCashflowFillSettledDelta(t *testing.T) {
	cases := []struct {
		name      string
		closedPnl float64
		fee       float64
		want      float64
	}{
		{"open: closedPnl 0, fee 0.5 -> -0.5", 0, 0.5, -0.5},
		{"profitable close: 20 gross - 0.3 fee", 20, 0.3, 19.7},
		{"losing close: -15 gross - 0.3 fee", -15, 0.3, -15.3},
		{"maker rebate adds to cash", 20, -0.1, 20.1},
		{"open with maker rebate", 0, -0.2, 0.2},
	}
	for _, tc := range cases {
		if got := cashflowFillSettledDelta(tc.closedPnl, tc.fee); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: cashflowFillSettledDelta(%v,%v) = %v, want %v", tc.name, tc.closedPnl, tc.fee, got, tc.want)
		}
	}
}

// expected = baseline_accountValue + Σ settled deltas + (current_uPnL - baseline_uPnL).
func TestCashflowJournalExpectedEquity(t *testing.T) {
	cases := []struct {
		name                               string
		baseAV, baseUPnL, settled, curUPnL float64
		want                               float64
	}{
		{"flat: no deltas, uPnL unchanged", 1000, 0, 0, 0, 1000},
		{"settled cash moved, uPnL flat", 1000, 0, 67.2, 0, 1067.2},
		{"uPnL rose since baseline", 1000, 0, 0, 25, 1025},
		{"uPnL fell since a positive baseline", 1000, 30, 0, 5, 975},
		{"combined", 1000, 30, 67.2, 5, 1042.2},
	}
	for _, tc := range cases {
		got := cashflowJournalExpectedEquity(tc.baseAV, tc.baseUPnL, tc.settled, tc.curUPnL)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The watermark advances one past the highest processed event, EXCEPT it never
// lands at/after a failed event's timestamp — so a same-ms sibling that
// persisted cannot strand the failed event behind the cursor.
func TestAdvanceCashflowCursor(t *testing.T) {
	cases := []struct {
		name         string
		current      int64
		maxProcessed int64
		failedAt     int64
		want         int64
	}{
		{"no events: maxProcessed=current-1, no fail -> unchanged", 100, 99, -1, 100},
		{"clean advance past last event", 100, 250, -1, 251},
		{"failure on the only event -> cursor unchanged", 100, 99, 200, 100},
		{"failure after some successes -> halt at failed ts", 100, 300, 300, 300},
		{"same-ms sibling succeeded then failed -> never skip failed", 100, 300, 300, 300},
		{"failure strictly after advance still clamps to failed", 100, 150, 200, 151},
	}
	for _, tc := range cases {
		if got := advanceCashflowCursor(tc.current, tc.maxProcessed, tc.failedAt); got != tc.want {
			t.Errorf("%s: advanceCashflowCursor(%d,%d,%d) = %d, want %d", tc.name, tc.current, tc.maxProcessed, tc.failedAt, got, tc.want)
		}
	}
}

// tid is the canonical per-fill key (one OID fragments into many fills); the
// time:hash:coin form is the fallback when tid is absent or zero.
func TestCashflowFillDedupID(t *testing.T) {
	withTid := hlFillRecord{Coin: "BTC", Time: 1700000000000, Hash: "0xabc", Tid: json.Number("987654321")}
	if got, want := cashflowFillDedupID(withTid), "fill:tid:987654321"; got != want {
		t.Errorf("tid present: got %q, want %q", got, want)
	}
	noTid := hlFillRecord{Coin: "eth", Time: 1700000000001, Hash: "0xdef", Tid: json.Number("0")}
	if got, want := cashflowFillDedupID(noTid), "fill:1700000000001:0xdef:ETH"; got != want {
		t.Errorf("tid zero: got %q, want %q", got, want)
	}
	emptyTid := hlFillRecord{Coin: "SOL", Time: 1700000000002, Hash: "0xfff"}
	if got, want := cashflowFillDedupID(emptyTid), "fill:1700000000002:0xfff:SOL"; got != want {
		t.Errorf("tid empty: got %q, want %q", got, want)
	}
}

func TestCashflowJournalStateRoundTrip(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	if _, found, err := db.GetCashflowJournalState("hyperliquid", "0xabc"); err != nil || found {
		t.Fatalf("fresh wallet: found=%v err=%v, want found=false err=nil", found, err)
	}
	st := CashflowJournalState{
		FillsSinceMs: 111, FundingSinceMs: 222, TransfersSinceMs: 333,
		BaselineAccountValue: 1234.5, BaselineUPnL: -6.7, BaselineSet: true, Incomplete: true,
	}
	if err := db.UpsertCashflowJournalState("hyperliquid", "0xabc", st); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := db.GetCashflowJournalState("hyperliquid", "0xabc")
	if err != nil || !found {
		t.Fatalf("reload: found=%v err=%v", found, err)
	}
	if got != st {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, st)
	}
}

// First contact anchors the baseline to the supplied snapshot, sets the cursors
// to now, and fetches NO history — pre-adoption movement belongs to the baseline.
func TestCashflowJournalBaselineAnchorsOnFirstContact(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	now := time.UnixMilli(1700000000000).UTC()

	res := fetchCashflowJournalEvents(db, key, 5000.0, 12.5, now)
	if !res.StateFound {
		t.Fatal("first contact should report StateFound after baseline init")
	}
	if res.FillsFetched || res.FundingFetched || res.TransfersFetched {
		t.Error("first contact must not replay history")
	}
	st, found, err := db.GetCashflowJournalState(key.Platform, key.Account)
	if err != nil || !found {
		t.Fatalf("state after baseline: found=%v err=%v", found, err)
	}
	if !st.BaselineSet || st.BaselineAccountValue != 5000.0 || st.BaselineUPnL != 12.5 {
		t.Errorf("baseline not anchored: %+v", st)
	}
	if st.FillsSinceMs != now.UnixMilli() || st.FundingSinceMs != now.UnixMilli() || st.TransfersSinceMs != now.UnixMilli() {
		t.Errorf("cursors not anchored at now: %+v", st)
	}
}

// End-to-end fetch -> ingest -> sum with stubbed HTTP. Reconstructed settled sum
// must equal Σ(fill settled deltas) + funding + transfers, and the expected
// equity must close the loop against a hand-computed accountValue.
func TestCashflowJournalIngestAndExpectedEquity(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}

	// Anchor baseline at AV=1000, uPnL=0, cursors at t0.
	t0 := time.UnixMilli(1700000000000).UTC()
	if r := fetchCashflowJournalEvents(db, key, 1000.0, 0.0, t0); !r.StateFound {
		t.Fatal("baseline init failed")
	}

	// Stub the three event streams for the next cycle.
	origFills := fetchHyperliquidUserFillsByTime
	origFunding := fetchHyperliquidUserFunding
	origTransfers := fetchHyperliquidLedgerUpdates
	defer func() {
		fetchHyperliquidUserFillsByTime = origFills
		fetchHyperliquidUserFunding = origFunding
		fetchHyperliquidLedgerUpdates = origTransfers
	}()
	fetchHyperliquidUserFillsByTime = func(addr string, sinceMs int64) ([]hlFillRecord, error) {
		return []hlFillRecord{
			{Coin: "BTC", Time: t0.UnixMilli() + 10, Tid: json.Number("1"), ClosedPnl: "0", Fee: "0.5"},  // open: -0.5
			{Coin: "BTC", Time: t0.UnixMilli() + 20, Tid: json.Number("2"), ClosedPnl: "20", Fee: "0.3"}, // close: +19.7
		}, nil
	}
	fetchHyperliquidUserFunding = func(addr string, sinceMs int64) ([]hlLedgerEvent, error) {
		return []hlLedgerEvent{
			{Time: t0.UnixMilli() + 5, Hash: "0xf1", Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-1.0"}},
		}, nil
	}
	fetchHyperliquidLedgerUpdates = func(addr string, sinceMs int64) ([]hlLedgerEvent, error) {
		return []hlLedgerEvent{
			{Time: t0.UnixMilli() + 6, Hash: "0xd1", Delta: hlLedgerEventDelta{Type: "deposit", USDC: "100"}},           // +100
			{Time: t0.UnixMilli() + 7, Hash: "0xw1", Delta: hlLedgerEventDelta{Type: "withdraw", USDC: "50", Fee: "1"}}, // -(50+1)=-51
		}, nil
	}

	res := fetchCashflowJournalEvents(db, key, 1072.2, 5.0, t0.Add(time.Minute))
	if !res.FillsFetched || !res.FundingFetched || !res.TransfersFetched {
		t.Fatalf("expected all three streams fetched: %+v", res)
	}
	st := ingestCashflowJournalEvents(db, res)
	if st.Incomplete {
		t.Error("all kinds mapped — journal must not be marked incomplete")
	}

	settled, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	const wantSettled = -0.5 + 19.7 - 1.0 + 100 - 51 // 67.2
	if math.Abs(settled-wantSettled) > 1e-9 {
		t.Fatalf("settled sum = %v, want %v", settled, wantSettled)
	}

	expected := cashflowJournalExpectedEquity(st.BaselineAccountValue, st.BaselineUPnL, settled, res.CurrentUPnL)
	// baseline 1000 + 67.2 settled + (5 - 0) uPnL = 1072.2; accountValue snapshot
	// was 1072.2 so the journal closes the loop with ~0 drift.
	if math.Abs(expected-1072.2) > 1e-9 {
		t.Errorf("expected equity = %v, want 1072.2", expected)
	}
	if drift := res.AccountValue - expected; math.Abs(drift) > 1e-9 {
		t.Errorf("journal drift = %v, want ~0", drift)
	}

	// Cursors advanced past the latest event of each stream.
	if st.FillsSinceMs != t0.UnixMilli()+20+1 {
		t.Errorf("fills cursor = %d, want %d", st.FillsSinceMs, t0.UnixMilli()+21)
	}
	if st.FundingSinceMs != t0.UnixMilli()+5+1 {
		t.Errorf("funding cursor = %d, want %d", st.FundingSinceMs, t0.UnixMilli()+6)
	}
	if st.TransfersSinceMs != t0.UnixMilli()+7+1 {
		t.Errorf("transfers cursor = %d, want %d", st.TransfersSinceMs, t0.UnixMilli()+8)
	}
}

// A duplicate dedup_id (cursor-overlap re-read) is booked once, so re-ingesting
// the same events must not double-count the settled sum.
func TestCashflowJournalDedup(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	if err := db.InsertCashflowJournalEntry(key.Platform, key.Account, 1700000000000, "fill", 19.7, "BTC", 20, 0.3, "fill:tid:42"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Same dedup_id again (e.g. a re-fetch over the cursor boundary).
	if err := db.InsertCashflowJournalEntry(key.Platform, key.Account, 1700000000000, "fill", 19.7, "BTC", 20, 0.3, "fill:tid:42"); err != nil {
		t.Fatalf("dup insert should be ignored, not error: %v", err)
	}
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-19.7) > 1e-9 {
		t.Errorf("dedup failed: sum = %v, want 19.7 (booked once)", sum)
	}
}

// An ingest pass that re-reads events at/below the watermark (overlap) must not
// re-book them, AND the DB UNIQUE guard backs that up.
func TestCashflowJournalIngestIdempotentOnReplay(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	base := CashflowJournalState{FillsSinceMs: 100, FundingSinceMs: 100, TransfersSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	mkRes := func(st CashflowJournalState) cashflowJournalFetchResult {
		return cashflowJournalFetchResult{
			Key: key, State: st, StateFound: true, FillsFetched: true,
			Fills: []hlFillRecord{{Coin: "BTC", Time: 150, Tid: json.Number("7"), ClosedPnl: "10", Fee: "0.2"}},
		}
	}
	st1 := ingestCashflowJournalEvents(db, mkRes(base))
	// Re-fetch returns the same fill (overlap); cursor already advanced past it.
	st2 := ingestCashflowJournalEvents(db, mkRes(st1))
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-9.8) > 1e-9 {
		t.Errorf("replay double-booked: sum = %v, want 9.8", sum)
	}
	if st2.FillsSinceMs != 151 {
		t.Errorf("cursor regressed on replay: %d, want 151", st2.FillsSinceMs)
	}
}

// An unmapped ledger delta type latches Incomplete (so a future alarm switch
// fails closed) and still records a $0-effect row so the event stays visible.
func TestCashflowJournalUnmappedKindLatchesIncomplete(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	res := cashflowJournalFetchResult{
		Key:        key,
		State:      CashflowJournalState{TransfersSinceMs: 100, BaselineSet: true},
		StateFound: true, TransfersFetched: true,
		Transfers: []hlLedgerEvent{
			{Time: 150, Hash: "0xq", Delta: hlLedgerEventDelta{Type: "someBrandNewKind"}},
		},
	}
	st := ingestCashflowJournalEvents(db, res)
	if !st.Incomplete {
		t.Fatal("unmapped kind must latch Incomplete")
	}
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum) > 1e-9 {
		t.Errorf("unmapped row must record $0 effect, sum = %v", sum)
	}
	// And it persisted the latch.
	got, _, _ := db.GetCashflowJournalState(key.Platform, key.Account)
	if !got.Incomplete {
		t.Error("Incomplete latch not persisted")
	}
}

// A persistence failure must HALT the cursor at the failed event so a crash
// can never strand an un-booked event behind an advanced watermark.
func TestCashflowJournalCursorHaltsOnPersistFailure(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	res := cashflowJournalFetchResult{
		Key:        key,
		State:      CashflowJournalState{FillsSinceMs: 100, BaselineSet: true},
		StateFound: true, FillsFetched: true,
		Fills: []hlFillRecord{{Coin: "BTC", Time: 200, Tid: json.Number("1"), ClosedPnl: "10", Fee: "0.2"}},
	}
	// Force every insert to fail by closing the DB first.
	db.Close()
	st := ingestCashflowJournalEvents(db, res)
	if st.FillsSinceMs != 100 {
		t.Errorf("cursor advanced past a failed insert: %d, want 100 (held)", st.FillsSinceMs)
	}
}

// HL spot coins are an index ("@107") or a named pair ("PURR/USDC"); perps
// assets never contain "/" or start with "@".
func TestHLFillIsSpot(t *testing.T) {
	spot := []string{"@107", "@1", "PURR/USDC", "ETH/USDC", " @5 ", "BTC/USDC"}
	perps := []string{"BTC", "ETH", "kPEPE", "HYPE", "SOL", "", " BTC "}
	for _, c := range spot {
		if !hlFillIsSpot(c) {
			t.Errorf("hlFillIsSpot(%q) = false, want true (spot)", c)
		}
	}
	for _, c := range perps {
		if hlFillIsSpot(c) {
			t.Errorf("hlFillIsSpot(%q) = true, want false (perps)", c)
		}
	}
}

// HL userFillsByTime returns SPOT fills too, but the journal reconciles the
// PERPS marginSummary.accountValue — a spot fill settles against the separate
// spot USDC balance and must contribute $0, exactly as signedPerpFlowUSD
// excludes spot on the transfer stream. Mixing spot into the perps settled sum
// injects spurious drift and can MASK a real perps drift of opposite sign.
func TestCashflowJournalExcludesSpotFills(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	t0 := int64(1700000000000)
	res := cashflowJournalFetchResult{
		Key:        key,
		State:      CashflowJournalState{FillsSinceMs: t0, BaselineSet: true},
		StateFound: true, FillsFetched: true,
		Fills: []hlFillRecord{
			{Coin: "BTC", Time: t0 + 10, Tid: json.Number("1"), ClosedPnl: "0", Fee: "0.5"},        // perps open: -0.5
			{Coin: "@107", Time: t0 + 15, Tid: json.Number("2"), ClosedPnl: "0", Fee: "9.8"},       // SPOT: would mask the perps gain
			{Coin: "BTC", Time: t0 + 20, Tid: json.Number("3"), ClosedPnl: "10", Fee: "0.2"},       // perps close: +9.8
			{Coin: "PURR/USDC", Time: t0 + 25, Tid: json.Number("4"), ClosedPnl: "0", Fee: "-0.1"}, // SPOT maker rebate (negative fee)
		},
	}
	st := ingestCashflowJournalEvents(db, res)

	// (a)+(c): the perps settled sum is byte-identical to the perps-only sum; the
	// spot fee does NOT cancel the real perps gain (no masking), and the spot
	// maker rebate adds nothing.
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	const wantPerpsOnly = -0.5 + 9.8 // 9.3; with the bug it would be 9.3 - 9.8 + 0.1 = -0.4
	if math.Abs(sum-wantPerpsOnly) > 1e-9 {
		t.Fatalf("spot leaked into perps settled sum: got %v, want %v (perps-only)", sum, wantPerpsOnly)
	}

	// (b): spot rows are still booked (visible + deduped) but at $0 amount under a
	// distinct kind, with closedPnl/fee retained as metadata only.
	var spotRows, spotNonZero int
	rows, err := db.db.Query(`SELECT amount_usd FROM cashflow_journal WHERE kind = 'fill_spot' AND platform = ? AND account = ?`, key.Platform, key.Account)
	if err != nil {
		t.Fatalf("query spot rows: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var amt float64
		if err := rows.Scan(&amt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		spotRows++
		if math.Abs(amt) > 1e-9 {
			spotNonZero++
		}
	}
	if spotRows != 2 {
		t.Errorf("expected 2 spot rows booked, got %d", spotRows)
	}
	if spotNonZero != 0 {
		t.Errorf("%d spot rows carried a non-zero perps amount", spotNonZero)
	}

	// Spot fills still advance the cursor (the latest event is a spot fill), so
	// they are booked once and not re-fetched forever.
	if st.FillsSinceMs != t0+25+1 {
		t.Errorf("cursor = %d, want %d (advanced past the latest spot fill)", st.FillsSinceMs, t0+26)
	}
}
