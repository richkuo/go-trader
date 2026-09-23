package main

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func newCashflowJournalTestDB(t *testing.T) *StateDB {
	t.Helper()
	db, err := OpenStateDB(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

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

func TestCashflowJournalIngestAndExpectedEquity(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}

	t0 := time.UnixMilli(1700000000000).UTC()
	if r := fetchCashflowJournalEvents(db, key, 1000.0, 0.0, t0); !r.StateFound {
		t.Fatal("baseline init failed")
	}

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
			{Coin: "BTC", Time: t0.UnixMilli() + 10, Tid: json.Number("1"), ClosedPnl: "0", Fee: "0.5"},
			{Coin: "BTC", Time: t0.UnixMilli() + 20, Tid: json.Number("2"), ClosedPnl: "20", Fee: "0.3"},
		}, nil
	}
	fetchHyperliquidUserFunding = func(addr string, sinceMs int64) ([]hlLedgerEvent, error) {
		return []hlLedgerEvent{
			{Time: t0.UnixMilli() + 5, Hash: "0xf1", Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-1.0"}},
		}, nil
	}
	fetchHyperliquidLedgerUpdates = func(addr string, sinceMs int64) ([]hlLedgerEvent, error) {
		return []hlLedgerEvent{
			{Time: t0.UnixMilli() + 6, Hash: "0xd1", Delta: hlLedgerEventDelta{Type: "deposit", USDC: "100"}},
			{Time: t0.UnixMilli() + 7, Hash: "0xw1", Delta: hlLedgerEventDelta{Type: "withdraw", USDC: "50", Fee: "1"}},
		}, nil
	}

	snap := t0.Add(time.Minute)
	res := fetchCashflowJournalEvents(db, key, 1072.2, 5.0, snap)
	if !res.FillsFetched || !res.FundingFetched || !res.TransfersFetched {
		t.Fatalf("expected all three streams fetched: %+v", res)
	}
	st := ingestCashflowJournalEvents(db, res, snap.UnixMilli())
	if st.Incomplete {
		t.Error("all kinds mapped — journal must not be marked incomplete")
	}

	settled, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	const wantSettled = -0.5 + 19.7 - 1.0 + 100 - 51
	if math.Abs(settled-wantSettled) > 1e-9 {
		t.Fatalf("settled sum = %v, want %v", settled, wantSettled)
	}

	expected := cashflowJournalExpectedEquity(st.BaselineAccountValue, st.BaselineUPnL, settled, res.CurrentUPnL)
	if math.Abs(expected-1072.2) > 1e-9 {
		t.Errorf("expected equity = %v, want 1072.2", expected)
	}
	if drift := res.AccountValue - expected; math.Abs(drift) > 1e-9 {
		t.Errorf("journal drift = %v, want ~0", drift)
	}

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

func TestCashflowJournalDedup(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	if err := db.InsertCashflowJournalEntry(key.Platform, key.Account, 1700000000000, "fill", 19.7, "BTC", 20, 0.3, "fill:tid:42"); err != nil {
		t.Fatalf("insert: %v", err)
	}
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
	st1 := ingestCashflowJournalEvents(db, mkRes(base), cashflowCutoffAll)
	st2 := ingestCashflowJournalEvents(db, mkRes(st1), cashflowCutoffAll)
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
	st := ingestCashflowJournalEvents(db, res, cashflowCutoffAll)
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
	got, _, _ := db.GetCashflowJournalState(key.Platform, key.Account)
	if !got.Incomplete {
		t.Error("Incomplete latch not persisted")
	}
}

func TestCashflowJournalCursorHaltsOnPersistFailure(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	res := cashflowJournalFetchResult{
		Key:        key,
		State:      CashflowJournalState{FillsSinceMs: 100, BaselineSet: true},
		StateFound: true, FillsFetched: true,
		Fills: []hlFillRecord{{Coin: "BTC", Time: 200, Tid: json.Number("1"), ClosedPnl: "10", Fee: "0.2"}},
	}
	db.Close()
	st := ingestCashflowJournalEvents(db, res, cashflowCutoffAll)
	if st.FillsSinceMs != 100 {
		t.Errorf("cursor advanced past a failed insert: %d, want 100 (held)", st.FillsSinceMs)
	}
}

const cashflowCutoffAll = int64(1) << 62

func TestCashflowJournalIngestRespectsCutoff(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	res := cashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, FillsFetched: true,
		Fills: []hlFillRecord{
			{Coin: "BTC", Time: 150, Tid: json.Number("1"), ClosedPnl: "10", Fee: "0.2"},
			{Coin: "BTC", Time: 250, Tid: json.Number("2"), ClosedPnl: "30", Fee: "0.5"},
		},
	}
	st := ingestCashflowJournalEvents(db, res, 200)
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-9.8) > 1e-9 {
		t.Errorf("post-cutoff event booked: sum = %v, want 9.8 (only the <=cutoff fill)", sum)
	}
	if st.FillsSinceMs != 151 {
		t.Errorf("cursor advanced past the cutoff: %d, want 151 (one past the booked fill)", st.FillsSinceMs)
	}

	res2 := res
	res2.State = st
	st2 := ingestCashflowJournalEvents(db, res2, 300)
	sum2, _ := db.SumCashflowJournal(key.Platform, key.Account)
	if math.Abs(sum2-(9.8+29.5)) > 1e-9 {
		t.Errorf("deferred fill not booked next cycle: sum = %v, want 39.3", sum2)
	}
	if st2.FillsSinceMs != 251 {
		t.Errorf("cursor = %d, want 251", st2.FillsSinceMs)
	}
}

func TestReconcileCashflowJournalUsability(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	t0 := time.UnixMilli(1700000000000).UTC()

	rec := reconcileCashflowJournal(db, key, 1000.0, 0.0, t0)
	if rec == nil {
		t.Fatal("first contact should still return a rec (baseline anchored)")
	}
	if rec.Usable {
		t.Error("first contact must not be usable")
	}

	origFills := fetchHyperliquidUserFillsByTime
	origFunding := fetchHyperliquidUserFunding
	origTransfers := fetchHyperliquidLedgerUpdates
	defer func() {
		fetchHyperliquidUserFillsByTime = origFills
		fetchHyperliquidUserFunding = origFunding
		fetchHyperliquidLedgerUpdates = origTransfers
	}()
	fetchHyperliquidUserFillsByTime = func(string, int64) ([]hlFillRecord, error) { return nil, nil }
	fetchHyperliquidUserFunding = func(string, int64) ([]hlLedgerEvent, error) { return nil, nil }
	fetchHyperliquidLedgerUpdates = func(string, int64) ([]hlLedgerEvent, error) { return nil, nil }

	rec2 := reconcileCashflowJournal(db, key, 1000.0, 0.0, t0.Add(time.Minute))
	if rec2 == nil || !rec2.Usable {
		t.Fatalf("steady cycle should be usable: %+v", rec2)
	}
	if math.Abs(rec2.Drift) > 1e-9 {
		t.Errorf("no movement should reconcile to ~0 drift, got %v", rec2.Drift)
	}

	fetchHyperliquidUserFunding = func(string, int64) ([]hlLedgerEvent, error) { return nil, errTestFetch }
	rec3 := reconcileCashflowJournal(db, key, 1000.0, 0.0, t0.Add(2*time.Minute))
	if rec3 == nil || rec3.Usable {
		t.Errorf("a stream fetch failure must make the cycle not usable: %+v", rec3)
	}
}

var errTestFetch = errors.New("simulated fetch failure")

func TestApplyCashflowJournalDriftBasis(t *testing.T) {
	prevPending := cashflowJournalPendingStreaks
	cashflowJournalPendingStreaks = &cashflowJournalPendingTracker{}
	defer func() { cashflowJournalPendingStreaks = prevPending }()

	hlKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	okxKey := SharedWalletKey{Platform: "okx", Account: "k"}
	mk := func() []sharedWalletDriftResult {
		return []sharedWalletDriftResult{
			{Key: hlKey, Drift: 0.40, Balance: 1000, MemberSum: 999.6, OrphanCoins: []string{"BTC"}},
			{Key: okxKey, Drift: 0.05, Balance: 500, MemberSum: 499.95},
		}
	}
	usable := &cashflowJournalReconcile{Key: hlKey, AccountValue: 1000, ExpectedEquity: 1000.0, Drift: 0.0, Usable: true}

	res := mk()
	applyCashflowJournalDriftBasis(res, hlKey, usable, true)
	if res[0].Basis != driftBasisJournal || math.Abs(res[0].Drift) > 1e-9 {
		t.Errorf("HL should switch to journal basis: %+v", res[0])
	}
	if len(res[0].OrphanCoins) != 1 || res[0].OrphanCoins[0] != "BTC" {
		t.Errorf("#1107: OrphanCoins must be preserved under the journal basis: %+v", res[0].OrphanCoins)
	}
	if res[0].JournalPending {
		t.Errorf("usable cycle must not be journal-pending: %+v", res[0])
	}
	if res[1].Basis != "" || res[1].Drift != 0.05 {
		t.Errorf("OKX must be untouched: %+v", res[1])
	}

	res = mk()
	applyCashflowJournalDriftBasis(res, hlKey, usable, false)
	if res[0].Basis != "" || res[0].Drift != 0.40 || res[0].JournalPending {
		t.Errorf("disabled: HL must keep trade-ledger drift, not pending: %+v", res[0])
	}

	cashflowJournalPendingStreaks.reset(sharedWalletKeyLabel(hlKey))
	for cycle := 1; cycle <= sharedWalletDriftAlertThreshold; cycle++ {
		res = mk()
		applyCashflowJournalDriftBasis(res, hlKey, &cashflowJournalReconcile{Key: hlKey, Usable: false}, true)
		if !res[0].JournalPending || res[0].Basis != "" {
			t.Errorf("transient miss cycle %d (within window) must be journal-pending: %+v", cycle, res[0])
		}
	}
	res = mk()
	applyCashflowJournalDriftBasis(res, hlKey, &cashflowJournalReconcile{Key: hlKey, Usable: false}, true)
	if res[0].JournalPending || res[0].Basis != "" || res[0].Drift != 0.40 {
		t.Errorf("persistent miss must fail closed to trade-ledger (drift 0.40, not pending): %+v", res[0])
	}
	res = mk()
	applyCashflowJournalDriftBasis(res, hlKey, usable, true)
	res = mk()
	applyCashflowJournalDriftBasis(res, hlKey, &cashflowJournalReconcile{Key: hlKey, Usable: false}, true)
	if !res[0].JournalPending {
		t.Errorf("a usable cycle must reset the pending streak (next miss suppressed again): %+v", res[0])
	}
	cashflowJournalPendingStreaks.reset(sharedWalletKeyLabel(hlKey))

	res = mk()
	applyCashflowJournalDriftBasis(res, hlKey, &cashflowJournalReconcile{Key: hlKey, Usable: false, Incomplete: true}, true)
	if res[0].Basis != "" || res[0].Drift != 0.40 || res[0].JournalPending {
		t.Errorf("incomplete must fail closed to trade-ledger (not pending): %+v", res[0])
	}

	res = mk()
	applyCashflowJournalDriftBasis(res, hlKey, nil, true)
	if res[0].Basis != "" || res[0].Drift != 0.40 || res[0].JournalPending {
		t.Errorf("nil rec must be a no-op: %+v", res[0])
	}
}

func TestCashflowJournalAlarmEnabled(t *testing.T) {
	cases := map[string]bool{"": true, "1": true, "yes": true, "on": true, "0": false, "off": false, "false": false, "no": false, "  OFF  ": false}
	for v, want := range cases {
		t.Setenv("GO_TRADER_CASHFLOW_JOURNAL_ALARM", v)
		if got := cashflowJournalAlarmEnabled(); got != want {
			t.Errorf("value %q: enabled = %v, want %v", v, got, want)
		}
	}
}

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

func TestCashflowJournalExcludesSpotFills(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	t0 := int64(1700000000000)
	res := cashflowJournalFetchResult{
		Key:        key,
		State:      CashflowJournalState{FillsSinceMs: t0, BaselineSet: true},
		StateFound: true, FillsFetched: true,
		Fills: []hlFillRecord{
			{Coin: "BTC", Time: t0 + 10, Tid: json.Number("1"), ClosedPnl: "0", Fee: "0.5"},
			{Coin: "@107", Time: t0 + 15, Tid: json.Number("2"), ClosedPnl: "0", Fee: "9.8"},
			{Coin: "BTC", Time: t0 + 20, Tid: json.Number("3"), ClosedPnl: "10", Fee: "0.2"},
			{Coin: "PURR/USDC", Time: t0 + 25, Tid: json.Number("4"), ClosedPnl: "0", Fee: "-0.1"},
		},
	}
	st := ingestCashflowJournalEvents(db, res, cashflowCutoffAll)

	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	const wantPerpsOnly = -0.5 + 9.8
	if math.Abs(sum-wantPerpsOnly) > 1e-9 {
		t.Fatalf("spot leaked into perps settled sum: got %v, want %v (perps-only)", sum, wantPerpsOnly)
	}

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

	if st.FillsSinceMs != t0+25+1 {
		t.Errorf("cursor = %d, want %d (advanced past the latest spot fill)", st.FillsSinceMs, t0+26)
	}
}

func TestHyperliquidClosedPnlBasisError(t *testing.T) {
	ids := func(tids ...string) map[string]struct{} {
		out := map[string]struct{}{}
		for _, id := range tids {
			out["fill:tid:"+id] = struct{}{}
		}
		return out
	}
	open := hlFillRecord{Coin: "BTC", Side: "B", Sz: "1", Px: "100", StartPosition: "0", ClosedPnl: "0", Time: 1, Tid: json.Number("1")}
	closeHigh := hlFillRecord{Coin: "BTC", Side: "A", Sz: "1", Px: "110", StartPosition: "1", ClosedPnl: "10.02", Time: 2, Tid: json.Number("2")}
	cases := []struct {
		name  string
		fills []hlFillRecord
		keep  map[string]struct{}
		want  float64
	}{
		{
			name:  "frontend closedPnl two cents above fill price",
			fills: []hlFillRecord{open, closeHigh},
			keep:  ids("1", "2"),
			want:  0.02,
		},
		{
			name: "same-timestamp partials chain by startPosition",
			fills: []hlFillRecord{
				open,
				{Coin: "BTC", Side: "B", Sz: "0.5", Px: "140", StartPosition: "1.5", ClosedPnl: "0", Time: 2, Tid: json.Number("2")},
				{Coin: "BTC", Side: "B", Sz: "0.5", Px: "120", StartPosition: "1", ClosedPnl: "0", Time: 2, Tid: json.Number("3")},
				{Coin: "BTC", Side: "A", Sz: "2", Px: "130", StartPosition: "2", ClosedPnl: "40", Time: 3, Tid: json.Number("4")},
			},
			keep: ids("1", "2", "3", "4"),
			want: 10,
		},
		{
			name:  "pre-journal open still sets entry for a journaled close",
			fills: []hlFillRecord{open, closeHigh},
			keep:  ids("2"),
			want:  0.02,
		},
		{
			name:  "fill outside the journal does not move the basis",
			fills: []hlFillRecord{open, closeHigh},
			keep:  ids("1"),
			want:  0,
		},
		{
			name: "reverse close and reopen uses the held position",
			fills: []hlFillRecord{
				{Coin: "BTC", Side: "B", Sz: "1", Px: "100", StartPosition: "0", ClosedPnl: "0", Time: 1, Tid: json.Number("1")},
				{Coin: "BTC", Side: "B", Sz: "1", Px: "131", StartPosition: "0", ClosedPnl: "0", Time: 2, Tid: json.Number("3")},
				{Coin: "BTC", Side: "A", Sz: "1", Px: "130", StartPosition: "1", ClosedPnl: "30.01", Time: 2, Tid: json.Number("2")},
			},
			keep: ids("1", "2", "3"),
			want: 0.01,
		},
		{
			name: "split flip uses the held position",
			fills: []hlFillRecord{
				{Coin: "BTC", Side: "B", Sz: "1", Px: "100", StartPosition: "0", ClosedPnl: "0", Time: 1, Tid: json.Number("1")},
				{Coin: "BTC", Side: "A", Sz: "1", Px: "109", StartPosition: "0", ClosedPnl: "0", Time: 2, Tid: json.Number("3")},
				{Coin: "BTC", Side: "A", Sz: "1", Px: "110", StartPosition: "1", ClosedPnl: "10.01", Time: 2, Tid: json.Number("2")},
			},
			keep: ids("1", "2", "3"),
			want: 0.01,
		},
		{
			name: "partial close cycle uses the held position",
			fills: []hlFillRecord{
				{Coin: "BTC", Side: "B", Sz: "2", Px: "100", StartPosition: "0", ClosedPnl: "0", Time: 1, Tid: json.Number("1")},
				{Coin: "BTC", Side: "B", Sz: "1", Px: "120", StartPosition: "1", ClosedPnl: "0", Time: 2, Tid: json.Number("3")},
				{Coin: "BTC", Side: "A", Sz: "1", Px: "110", StartPosition: "2", ClosedPnl: "10.01", Time: 2, Tid: json.Number("2")},
				{Coin: "BTC", Side: "A", Sz: "2", Px: "130", StartPosition: "2", ClosedPnl: "40.02", Time: 3, Tid: json.Number("4")},
			},
			keep: ids("1", "2", "3", "4"),
			want: 0.03,
		},
		{
			name: "unresolvable cycle uses raw closedPnl",
			fills: []hlFillRecord{
				{Coin: "BTC", Side: "B", Sz: "1", Px: "131", StartPosition: "0", ClosedPnl: "0", Time: 2, Tid: json.Number("3")},
				{Coin: "BTC", Side: "A", Sz: "1", Px: "130", StartPosition: "1", ClosedPnl: "30.01", Time: 2, Tid: json.Number("2")},
			},
			keep: ids("2", "3"),
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hyperliquidClosedPnlBasisError(tc.fills, tc.keep)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("basis = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconcileCashflowJournalFillPriceBasis(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	t0 := time.UnixMilli(1_700_000_000_000).UTC()

	origFills := fetchHyperliquidUserFillsByTime
	origFunding := fetchHyperliquidUserFunding
	origTransfers := fetchHyperliquidLedgerUpdates
	defer func() {
		fetchHyperliquidUserFillsByTime = origFills
		fetchHyperliquidUserFunding = origFunding
		fetchHyperliquidLedgerUpdates = origTransfers
	}()
	fills := []hlFillRecord{
		{Coin: "BTC", Side: "B", Sz: "1", Px: "100", StartPosition: "0", ClosedPnl: "0", Fee: "0.1", Time: t0.UnixMilli() + 10, Tid: json.Number("1")},
		{Coin: "BTC", Side: "A", Sz: "1", Px: "110", StartPosition: "1", ClosedPnl: "10.02", Fee: "0.1", Time: t0.UnixMilli() + 20, Tid: json.Number("2")},
	}
	fetchHyperliquidUserFillsByTime = func(string, int64) ([]hlFillRecord, error) { return fills, nil }
	fetchHyperliquidUserFunding = func(string, int64) ([]hlLedgerEvent, error) { return nil, nil }
	fetchHyperliquidLedgerUpdates = func(string, int64) ([]hlLedgerEvent, error) { return nil, nil }

	if rec := reconcileCashflowJournal(db, key, 1000, 0, t0); rec == nil || rec.Usable {
		t.Fatalf("baseline cycle: %+v", rec)
	}
	stored, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(stored) > 1e-9 {
		t.Fatalf("baseline must not book fills, settled = %v", stored)
	}

	flat := reconcileCashflowJournal(db, key, 0, 0, t0.Add(time.Minute))
	if flat == nil || !flat.Usable {
		t.Fatalf("steady cycle: %+v", flat)
	}
	stored, err = db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	const rawSettled = -0.1 + (10.02 - 0.1)
	if math.Abs(stored-rawSettled) > 1e-9 {
		t.Fatalf("stored settled = %v, want raw closedPnl sum %v", stored, rawSettled)
	}
	if math.Abs(flat.ClosedPnlBasis-0.02) > 1e-9 {
		t.Fatalf("basis = %v, want 0.02", flat.ClosedPnlBasis)
	}
	corrected := reconcileCashflowJournal(db, key, flat.ExpectedEquity, 0, t0.Add(2*time.Minute))
	if corrected == nil || math.Abs(corrected.Drift) > 1e-6 {
		t.Fatalf("fill-price equity must reconcile, drift = %v", corrected.Drift)
	}
	gap := reconcileCashflowJournal(db, key, corrected.ExpectedEquity-0.02, 0, t0.Add(3*time.Minute))
	if gap == nil || !(math.Abs(gap.Drift) > sharedWalletDriftTolerance) || math.Abs(gap.Drift-(-0.02)) > 1e-6 {
		t.Fatalf("real two-cent cash gap must still alert, drift = %v", gap.Drift)
	}
}

func hlBasisTestFill(coin, side, sz, px, start, closedPnl string, timeMs int64, tid string) hlFillRecord {
	return hlFillRecord{Coin: coin, Side: side, Sz: sz, Px: px, StartPosition: start, ClosedPnl: closedPnl, Time: timeMs, Tid: json.Number(tid)}
}

func TestEnsureHyperliquidCashflowBasis(t *testing.T) {
	type seeded struct {
		timeMs    int64
		closedPnl float64
		tid       string
	}
	fullPage := func(tail ...hlFillRecord) []hlFillRecord {
		out := make([]hlFillRecord, 0, hlUserFillsByTimeLimit)
		for i := 1; i <= hlUserFillsByTimeLimit-len(tail); i++ {
			out = append(out, hlBasisTestFill("SOL", "B", "1", "1", strconv.Itoa(i-1), "0", int64(i), strconv.Itoa(100000+i)))
		}
		return append(out, tail...)
	}
	btcOpen := hlBasisTestFill("BTC", "B", "2", "100", "0", "0", 2500, "9000")
	btcClose := hlBasisTestFill("BTC", "A", "1", "110", "2", "10.01", 3000, "9001")
	cases := []struct {
		name      string
		journaled []seeded
		pages     func(call int, since int64) ([]hlFillRecord, error)
		wantReady []bool
		wantErr   []bool
		after     []hlFillRecord
		wantBasis float64
	}{
		{
			name:      "backfill stops at the journal target",
			journaled: []seeded{{3000, 10.01, "9001"}},
			pages: func(int, int64) ([]hlFillRecord, error) {
				return []hlFillRecord{btcOpen, btcClose, hlBasisTestFill("BTC", "A", "1", "120", "1", "20.02", 5000, "9002")}, nil
			},
			wantReady: []bool{true},
			wantErr:   []bool{false},
			after:     []hlFillRecord{hlBasisTestFill("BTC", "A", "1", "120", "1", "20.02", 5000, "9002")},
			wantBasis: 0.03,
		},
		{
			name:      "full page resumes at its last millisecond without reapplying it",
			journaled: []seeded{{3000, 10.01, "9001"}},
			pages: func(_ int, since int64) ([]hlFillRecord, error) {
				add := hlBasisTestFill("BTC", "B", "1", "120", "1", "0", 2500, "9010")
				if since == 0 {
					return fullPage(hlBasisTestFill("BTC", "B", "1", "100", "0", "0", 2400, "9009"), add), nil
				}
				return []hlFillRecord{add, hlBasisTestFill("BTC", "A", "1", "115", "2", "5.01", 3000, "9001")}, nil
			},
			wantReady: []bool{false, true},
			wantErr:   []bool{false, false},
			wantBasis: 0.01,
		},
		{
			name:      "fetch failure stays pending then resumes",
			journaled: []seeded{{3000, 10.01, "9001"}},
			pages: func(call int, _ int64) ([]hlFillRecord, error) {
				if call == 1 {
					return nil, errors.New("temporary fetch failure")
				}
				return []hlFillRecord{btcOpen, btcClose}, nil
			},
			wantReady: []bool{false, true},
			wantErr:   []bool{true, false},
			wantBasis: 0.01,
		},
		{
			name:      "journaled fills outside exchange history use raw closedPnl",
			journaled: []seeded{{50, 4.02, "7"}, {3000, 10.01, "9001"}},
			pages: func(int, int64) ([]hlFillRecord, error) {
				return []hlFillRecord{btcOpen, btcClose}, nil
			},
			wantReady: []bool{true},
			wantErr:   []bool{false},
			wantBasis: 0.01,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newCashflowJournalTestDB(t)
			key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
			state := CashflowJournalState{FillsSinceMs: 3001, BaselineSet: true}
			if err := db.UpsertCashflowJournalState(key.Platform, key.Account, state); err != nil {
				t.Fatalf("seed state: %v", err)
			}
			for _, j := range tc.journaled {
				if err := db.InsertCashflowJournalEntry(key.Platform, key.Account, j.timeMs, "fill", j.closedPnl, "BTC", j.closedPnl, 0, "fill:tid:"+j.tid); err != nil {
					t.Fatalf("seed fill: %v", err)
				}
			}
			orig := fetchHyperliquidUserFillsByTime
			defer func() { fetchHyperliquidUserFillsByTime = orig }()
			calls := 0
			fetchHyperliquidUserFillsByTime = func(_ string, since int64) ([]hlFillRecord, error) {
				calls++
				return tc.pages(calls, since)
			}
			for i, want := range tc.wantReady {
				ready, err := ensureHyperliquidCashflowBasis(db, key, state)
				if ready != want || (err != nil) != tc.wantErr[i] {
					t.Fatalf("cycle %d: ready=%v err=%v", i+1, ready, err)
				}
			}
			if calls != len(tc.wantReady) {
				t.Fatalf("fetch calls = %d, want %d", calls, len(tc.wantReady))
			}
			if ready, err := ensureHyperliquidCashflowBasis(db, key, state); !ready || err != nil || calls != len(tc.wantReady) {
				t.Fatalf("ready basis must not refetch history: ready=%v err=%v calls=%d", ready, err, calls)
			}
			if len(tc.after) > 0 {
				if _, _, err := db.insertHyperliquidCashflowFills(key, tc.after, state.FillsSinceMs, cashflowCutoffAll); err != nil {
					t.Fatalf("ingest after backfill: %v", err)
				}
			}
			var unresolved int
			if err := db.db.QueryRow(`SELECT COUNT(*) FROM cashflow_journal WHERE kind = 'fill' AND hl_basis_resolved = 0`).Scan(&unresolved); err != nil {
				t.Fatalf("count unresolved: %v", err)
			}
			if unresolved != 0 {
				t.Fatalf("unresolved journaled fills = %d, want 0", unresolved)
			}
			got, err := db.SumHyperliquidCashflowBasis(key)
			if err != nil || math.Abs(got-tc.wantBasis) > 1e-9 {
				t.Fatalf("basis = %v (err %v), want %v", got, err, tc.wantBasis)
			}
		})
	}
}

func TestReconcileCashflowJournalBasisPendingThenBounded(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	t0 := time.UnixMilli(1_700_000_000_000).UTC()
	t0ms := t0.UnixMilli()
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, CashflowJournalState{
		FillsSinceMs: t0ms, FundingSinceMs: t0ms, TransfersSinceMs: t0ms, BaselineAccountValue: 1000, BaselineSet: true,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if err := db.InsertCashflowJournalEntry(key.Platform, key.Account, t0ms-10, "fill", 10.01, "BTC", 10.01, 0, "fill:tid:2"); err != nil {
		t.Fatalf("seed fill: %v", err)
	}
	origFills := fetchHyperliquidUserFillsByTime
	origFunding := fetchHyperliquidUserFunding
	origTransfers := fetchHyperliquidLedgerUpdates
	defer func() {
		fetchHyperliquidUserFillsByTime = origFills
		fetchHyperliquidUserFunding = origFunding
		fetchHyperliquidLedgerUpdates = origTransfers
	}()
	historyCalls := 0
	incrementalCalls := 0
	fetchHyperliquidUserFillsByTime = func(_ string, since int64) ([]hlFillRecord, error) {
		if since != 0 {
			incrementalCalls++
			return nil, nil
		}
		historyCalls++
		if historyCalls == 1 {
			return nil, errors.New("temporary fetch failure")
		}
		return []hlFillRecord{
			hlBasisTestFill("BTC", "B", "1", "100", "0", "0", t0ms-20, "1"),
			hlBasisTestFill("BTC", "A", "1", "110", "1", "10.01", t0ms-10, "2"),
		}, nil
	}
	fetchHyperliquidUserFunding = func(string, int64) ([]hlLedgerEvent, error) { return nil, nil }
	fetchHyperliquidLedgerUpdates = func(string, int64) ([]hlLedgerEvent, error) { return nil, nil }

	pending := reconcileCashflowJournal(db, key, 1010.00, 0, t0.Add(time.Minute))
	if pending == nil || pending.Usable || pending.Incomplete || pending.ClosedPnlBasis != 0 {
		t.Fatalf("backfill failure must hold the alarm pending on the transient path: %+v", pending)
	}
	for i := 2; i <= 4; i++ {
		rec := reconcileCashflowJournal(db, key, 1010.00, 0, t0.Add(time.Duration(i)*time.Minute))
		if rec == nil || !rec.Usable || math.Abs(rec.ClosedPnlBasis-0.01) > 1e-9 || math.Abs(rec.Drift) > 1e-6 {
			t.Fatalf("cycle %d: %+v", i, rec)
		}
	}
	if historyCalls != 2 || incrementalCalls != 4 {
		t.Fatalf("history fetches = %d, incremental fetches = %d; want 2 and 4", historyCalls, incrementalCalls)
	}
}

func TestIngestCashflowJournalFullFillPageDefersLastMillisecond(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, CashflowJournalState{FillsSinceMs: 1, BaselineSet: true}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if ready, err := ensureHyperliquidCashflowBasis(db, key, CashflowJournalState{FillsSinceMs: 1}); !ready || err != nil {
		t.Fatalf("empty journal basis: ready=%v err=%v", ready, err)
	}
	page := make([]hlFillRecord, 0, hlUserFillsByTimeLimit)
	for i := 1; i < hlUserFillsByTimeLimit; i++ {
		page = append(page, hlBasisTestFill("SOL", "B", "1", "1", strconv.Itoa(i-1), "0", int64(i), strconv.Itoa(i)))
	}
	btcOpen := hlBasisTestFill("BTC", "B", "2", "100", "0", "0", 5000, "9000")
	btcClose := hlBasisTestFill("BTC", "A", "1", "110", "2", "10.01", 5000, "9001")
	page = append(page, btcOpen)
	st, _, _ := db.GetCashflowJournalState(key.Platform, key.Account)
	st = ingestCashflowJournalEvents(db, cashflowJournalFetchResult{Key: key, State: st, StateFound: true, Fills: page, FillsFetched: true}, cashflowCutoffAll)
	if st.FillsSinceMs != hlUserFillsByTimeLimit {
		t.Fatalf("cursor = %d, want %d (the truncated last millisecond is refetched)", st.FillsSinceMs, hlUserFillsByTimeLimit)
	}
	st = ingestCashflowJournalEvents(db, cashflowJournalFetchResult{Key: key, State: st, StateFound: true, Fills: []hlFillRecord{btcClose, btcOpen}, FillsFetched: true}, cashflowCutoffAll)
	if st.FillsSinceMs != 5001 {
		t.Fatalf("cursor = %d, want 5001", st.FillsSinceMs)
	}
	basis, err := db.SumHyperliquidCashflowBasis(key)
	if err != nil || math.Abs(basis-0.01) > 1e-9 {
		t.Fatalf("basis = %v (err %v), want 0.01 from the full same-millisecond chain", basis, err)
	}
}
