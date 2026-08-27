package main

import (
	"errors"
	"math"
	"testing"
	"time"
)


func TestOKXBillSettledDelta(t *testing.T) {
	cases := []struct {
		name      string
		bill      okxBillRecord
		wantDelta float64
		wantKind  string
		wantKnown bool
	}{
		{"USDT trade pnl-fee net", okxBillRecord{Ccy: "USDT", Type: "2", BalChg: 19.7, Pnl: 20, Fee: 0.3}, 19.7, "trade", true},
		{"USDT funding receipt", okxBillRecord{Ccy: "USDT", Type: "8", BalChg: -1.25}, -1.25, "funding_fee", true},
		{"USDT transfer in", okxBillRecord{Ccy: "USDT", Type: "1", BalChg: 100}, 100, "transfer", true},
		{"USDT maker rebate (positive balChg)", okxBillRecord{Ccy: "USDT", Type: "2", BalChg: 0.1, Fee: -0.1}, 0.1, "trade", true},
		{"empty ccy treated as settlement", okxBillRecord{Ccy: "", Type: "8", BalChg: -2}, -2, "funding_fee", true},
		{"unknown type books balChg but not known", okxBillRecord{Ccy: "USDT", Type: "999", BalChg: 5}, 5, "type_999", false},
		{"non-USDT bill contributes 0 and not known", okxBillRecord{Ccy: "BTC", Type: "2", BalChg: 0.001}, 0, "nonsettle_btc", false},
	}
	for _, tc := range cases {
		gotDelta, gotKind, gotKnown := okxBillSettledDelta(tc.bill)
		if math.Abs(gotDelta-tc.wantDelta) > 1e-9 || gotKind != tc.wantKind || gotKnown != tc.wantKnown {
			t.Errorf("%s: okxBillSettledDelta = (%v,%q,%v), want (%v,%q,%v)",
				tc.name, gotDelta, gotKind, gotKnown, tc.wantDelta, tc.wantKind, tc.wantKnown)
		}
	}
}

func TestOKXBillDedupID(t *testing.T) {
	withID := okxBillRecord{BillID: "374241568037822465", Type: "2", TimeMs: 1700000000000, TradeID: "tx1"}
	if got, want := okxBillDedupID(withID), "okxbill:374241568037822465"; got != want {
		t.Errorf("billId present: got %q, want %q", got, want)
	}
	zeroID := okxBillRecord{BillID: "0", Type: "8", TimeMs: 1700000000001, TradeID: "tx2"}
	if got, want := okxBillDedupID(zeroID), "okxbill:8:1700000000001:tx2"; got != want {
		t.Errorf("billId zero: got %q, want %q", got, want)
	}
	emptyID := okxBillRecord{Type: "1", TimeMs: 1700000000002}
	if got, want := okxBillDedupID(emptyID), "okxbill:1:1700000000002:"; got != want {
		t.Errorf("billId empty: got %q, want %q", got, want)
	}
}

func newOKXJournalKey() SharedWalletKey {
	return SharedWalletKey{Platform: "okx", Account: "okx-api-key-123"}
}

func TestOKXCashflowJournalBaselineAnchor(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newOKXJournalKey()
	now := time.UnixMilli(1700000000000).UTC()

	res := fetchOKXCashflowJournalEvents(db, key, 5000.0, 12.5, now)
	if !res.StateFound || res.BillsFetched {
		t.Fatalf("baseline cycle: StateFound=%v BillsFetched=%v, want true/false", res.StateFound, res.BillsFetched)
	}
	st, found, err := db.GetCashflowJournalState(key.Platform, key.Account)
	if err != nil || !found {
		t.Fatalf("state after baseline: found=%v err=%v", found, err)
	}
	if !st.BaselineSet || st.BaselineAccountValue != 5000.0 || st.BaselineUPnL != 12.5 {
		t.Errorf("baseline not anchored: %+v", st)
	}
	if st.FillsSinceMs != now.UnixMilli() {
		t.Errorf("bills cursor not anchored at now: %d, want %d", st.FillsSinceMs, now.UnixMilli())
	}
	if st.FundingSinceMs != 0 || st.TransfersSinceMs != 0 {
		t.Errorf("OKX must leave funding/transfers cursors unused: %+v", st)
	}
}

func TestOKXCashflowJournalIngestAndExpectedEquity(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newOKXJournalKey()
	t0 := time.UnixMilli(1700000000000).UTC()

	if r := fetchOKXCashflowJournalEvents(db, key, 1000.0, 0.0, t0); !r.StateFound {
		t.Fatal("baseline init failed")
	}

	orig := fetchOKXAccountBills
	defer func() { fetchOKXAccountBills = orig }()
	fetchOKXAccountBills = func(sinceMs int64) ([]okxBillRecord, bool, error) {
		return []okxBillRecord{
			{BillID: "b1", TimeMs: t0.UnixMilli() + 10, Ccy: "USDT", Type: "2", BalChg: -0.5, Pnl: 0, Fee: 0.5},
			{BillID: "b2", TimeMs: t0.UnixMilli() + 20, Ccy: "USDT", Type: "2", BalChg: 19.7, Pnl: 20, Fee: 0.3},
			{BillID: "b3", TimeMs: t0.UnixMilli() + 5, Ccy: "USDT", Type: "8", BalChg: -1.0},
			{BillID: "b4", TimeMs: t0.UnixMilli() + 6, Ccy: "USDT", Type: "1", BalChg: 100},
			{BillID: "b5", TimeMs: t0.UnixMilli() + 7, Ccy: "USDT", Type: "1", BalChg: -51},
		}, false, nil
	}

	snap := t0.Add(time.Minute)
	res := fetchOKXCashflowJournalEvents(db, key, 1072.2, 5.0, snap)
	if !res.BillsFetched || res.Capped {
		t.Fatalf("expected bills fetched, not capped: %+v", res)
	}
	st := ingestOKXCashflowJournalEvents(db, res, snap.UnixMilli())
	if st.Incomplete {
		t.Error("all bills classified — journal must not be marked incomplete")
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
		t.Errorf("bills cursor = %d, want %d", st.FillsSinceMs, t0.UnixMilli()+21)
	}
}

func TestOKXCashflowJournalSnapshotBound(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newOKXJournalKey()
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res := okxCashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, BillsFetched: true,
		Bills: []okxBillRecord{
			{BillID: "in", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 10},
			{BillID: "after", TimeMs: 250, Ccy: "USDT", Type: "2", BalChg: 999},
		},
	}
	st := ingestOKXCashflowJournalEvents(db, res, 200)
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-10) > 1e-9 {
		t.Errorf("post-cutoff bill leaked: sum = %v, want 10", sum)
	}
	if st.FillsSinceMs != 151 {
		t.Errorf("cursor must stop before the deferred bill: got %d, want 151", st.FillsSinceMs)
	}
}

func TestOKXCashflowJournalCappedAdvancesOnlyToMaxTime(t *testing.T) {
	mk := func(capped bool) CashflowJournalState {
		db := newCashflowJournalTestDB(t)
		key := newOKXJournalKey()
		base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
		if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
			t.Fatalf("seed: %v", err)
		}
		res := okxCashflowJournalFetchResult{
			Key: key, State: base, StateFound: true, BillsFetched: true, Capped: capped,
			Bills: []okxBillRecord{
				{BillID: "a", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 5},
				{BillID: "b", TimeMs: 200, Ccy: "USDT", Type: "2", BalChg: 5},
			},
		}
		return ingestOKXCashflowJournalEvents(db, res, cashflowCutoffAll)
	}
	if st := mk(false); st.FillsSinceMs != 201 {
		t.Errorf("non-capped: cursor = %d, want 201 (maxTime+1)", st.FillsSinceMs)
	}
	if st := mk(true); st.FillsSinceMs != 200 {
		t.Errorf("capped: cursor = %d, want 200 (maxTime — boundary ms re-read next cycle)", st.FillsSinceMs)
	}
}

func TestOKXCashflowJournalCappedSameMsSiblingNotStranded(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newOKXJournalKey()
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c1 := okxCashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, BillsFetched: true, Capped: true,
		Bills: []okxBillRecord{
			{BillID: "s1", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 1},
			{BillID: "s2", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 1},
			{BillID: "s3", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 1},
		},
	}
	st1 := ingestOKXCashflowJournalEvents(db, c1, cashflowCutoffAll)
	if st1.FillsSinceMs != 150 {
		t.Fatalf("capped cycle: cursor = %d, want 150 (must re-read the boundary ms)", st1.FillsSinceMs)
	}

	c2 := okxCashflowJournalFetchResult{
		Key: key, State: st1, StateFound: true, BillsFetched: true, Capped: false,
		Bills: []okxBillRecord{
			{BillID: "s1", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 1},
			{BillID: "s2", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 1},
			{BillID: "s3", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 1},
			{BillID: "s4", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 1},
			{BillID: "n1", TimeMs: 200, Ccy: "USDT", Type: "2", BalChg: 7},
		},
	}
	st2 := ingestOKXCashflowJournalEvents(db, c2, cashflowCutoffAll)
	if st2.FillsSinceMs != 201 {
		t.Errorf("cleared cycle: cursor = %d, want 201", st2.FillsSinceMs)
	}
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-11) > 1e-9 {
		t.Errorf("same-ms sibling stranded or double-counted: sum = %v, want 11", sum)
	}
}

func TestOKXCashflowJournalCappedNewerTailNotStranded(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newOKXJournalKey()
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c1 := okxCashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, BillsFetched: true, Capped: true,
		Bills: []okxBillRecord{
			{BillID: "a", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 2},
			{BillID: "b", TimeMs: 160, Ccy: "USDT", Type: "2", BalChg: 2},
		},
	}
	st1 := ingestOKXCashflowJournalEvents(db, c1, cashflowCutoffAll)
	if st1.FillsSinceMs != 160 {
		t.Fatalf("capped cycle: cursor = %d, want 160 (must not pass the unfetched newer tail)", st1.FillsSinceMs)
	}

	c2 := okxCashflowJournalFetchResult{
		Key: key, State: st1, StateFound: true, BillsFetched: true, Capped: false,
		Bills: []okxBillRecord{
			{BillID: "b", TimeMs: 160, Ccy: "USDT", Type: "2", BalChg: 2},
			{BillID: "c", TimeMs: 170, Ccy: "USDT", Type: "2", BalChg: 3},
			{BillID: "d", TimeMs: 180, Ccy: "USDT", Type: "2", BalChg: 4},
		},
	}
	st2 := ingestOKXCashflowJournalEvents(db, c2, cashflowCutoffAll)
	if st2.FillsSinceMs != 181 {
		t.Errorf("drained cycle: cursor = %d, want 181", st2.FillsSinceMs)
	}
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-11) > 1e-9 {
		t.Errorf("newer tail stranded or double-counted: sum = %v, want 11", sum)
	}
}

func TestOKXCashflowJournalUnclassifiedLatchesIncomplete(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newOKXJournalKey()
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res := okxCashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, BillsFetched: true,
		Bills: []okxBillRecord{
			{BillID: "u1", TimeMs: 150, Ccy: "USDT", Type: "999", BalChg: 7},
		},
	}
	st := ingestOKXCashflowJournalEvents(db, res, cashflowCutoffAll)
	if !st.Incomplete {
		t.Error("unknown bill type must latch incomplete")
	}
	sum, _ := db.SumCashflowJournal(key.Platform, key.Account)
	if math.Abs(sum-7) > 1e-9 {
		t.Errorf("unknown-type balChg must still be booked (authoritative): sum = %v, want 7", sum)
	}
}

func TestOKXCashflowJournalNonSettlementCcyFailsClosed(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newOKXJournalKey()
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res := okxCashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, BillsFetched: true,
		Bills: []okxBillRecord{
			{BillID: "usdt", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 10},
			{BillID: "btcfee", TimeMs: 151, Ccy: "BTC", Type: "2", BalChg: 0.001},
		},
	}
	st := ingestOKXCashflowJournalEvents(db, res, cashflowCutoffAll)
	if !st.Incomplete {
		t.Error("non-USDT bill must latch incomplete")
	}
	sum, _ := db.SumCashflowJournal(key.Platform, key.Account)
	if math.Abs(sum-10) > 1e-9 {
		t.Errorf("non-USDT balChg must not enter the USDT sum: sum = %v, want 10", sum)
	}
}

func TestOKXCashflowJournalHaltsCursorOnPersistFailure(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newOKXJournalKey()
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close()
	res := okxCashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, BillsFetched: true,
		Bills: []okxBillRecord{{BillID: "b", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 10}},
	}
	st := ingestOKXCashflowJournalEvents(db, res, cashflowCutoffAll)
	if st.FillsSinceMs != 100 {
		t.Errorf("cursor advanced past an un-booked bill: got %d, want 100", st.FillsSinceMs)
	}
}

func TestOKXCashflowJournalReconcileUsability(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newOKXJournalKey()
	t0 := time.UnixMilli(1700000000000).UTC()
	if r := fetchOKXCashflowJournalEvents(db, key, 1000.0, 0.0, t0); !r.StateFound {
		t.Fatal("baseline init failed")
	}
	orig := fetchOKXAccountBills
	defer func() { fetchOKXAccountBills = orig }()

	fetchOKXAccountBills = func(int64) ([]okxBillRecord, bool, error) {
		return []okxBillRecord{{BillID: "b1", TimeMs: t0.UnixMilli() + 10, Ccy: "USDT", Type: "2", BalChg: 5}}, false, nil
	}
	snap := t0.Add(time.Minute)
	rec := reconcileOKXCashflowJournal(db, key, 1005.0, 0.0, snap)
	if rec == nil || !rec.Usable || rec.Incomplete {
		t.Fatalf("clean cycle: rec=%+v, want usable & not incomplete", rec)
	}
	if math.Abs(rec.Drift) > 1e-9 {
		t.Errorf("clean cycle drift = %v, want ~0", rec.Drift)
	}

	fetchOKXAccountBills = func(int64) ([]okxBillRecord, bool, error) {
		return []okxBillRecord{{BillID: "b2", TimeMs: snap.UnixMilli() + 10, Ccy: "USDT", Type: "2", BalChg: 1}}, true, nil
	}
	snap2 := snap.Add(time.Minute)
	rec2 := reconcileOKXCashflowJournal(db, key, 1006.0, 0.0, snap2)
	if rec2 == nil || rec2.Usable {
		t.Fatalf("capped cycle: rec=%+v, want not usable", rec2)
	}

	fetchOKXAccountBills = func(int64) ([]okxBillRecord, bool, error) {
		return nil, false, errors.New("okx bills 500")
	}
	snap3 := snap2.Add(time.Minute)
	rec3 := reconcileOKXCashflowJournal(db, key, 1007.0, 0.0, snap3)
	if rec3 == nil || rec3.Usable {
		t.Fatalf("fetch-error cycle: rec=%+v, want not usable", rec3)
	}
}

func TestOKXCashflowJournalRejectsNonOKXKey(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	hlKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	if rec := reconcileOKXCashflowJournal(db, hlKey, 1000, 0, time.Now()); rec != nil {
		t.Errorf("non-OKX key must return nil, got %+v", rec)
	}
}

func TestOKXCashflowJournalShadowDoesNotMutate(t *testing.T) {
	key := newOKXJournalKey()
	results := []sharedWalletDriftResult{
		{Key: key, Drift: 1.23, Balance: 1000, MemberSum: 998.77},
	}
	rec := &cashflowJournalReconcile{Key: key, AccountValue: 1000, ExpectedEquity: 1002.5, Drift: -2.5, Usable: true}
	logOKXCashflowJournalShadow(results, key, rec)
	if results[0].Drift != 1.23 || results[0].Basis != "" || results[0].ExpectedEquity != 0 {
		t.Errorf("shadow log mutated the drift result: %+v", results[0])
	}
}

func TestParseOKXBillsOutput(t *testing.T) {
	clean := []byte(`{"bills":[{"bill_id":"b1","ts_ms":1700000000000,"ccy":"USDT","type":"2","bal_chg":19.7,"pnl":20,"fee":0.3}],"capped":false,"platform":"okx","timestamp":"t"}`)
	res, _, err := parseOKXBillsOutput(clean, "", nil)
	if err != nil || res == nil || len(res.Bills) != 1 || res.Bills[0].BillID != "b1" || res.Bills[0].BalChg != 19.7 {
		t.Fatalf("clean: res=%+v err=%v", res, err)
	}
	if res.Capped {
		t.Error("clean: capped should be false")
	}

	errEnvelope := []byte(`{"bills":[],"capped":false,"platform":"okx","timestamp":"t","error":"not live"}`)
	if _, _, err := parseOKXBillsOutput(errEnvelope, "", errors.New("exit 1")); err == nil {
		t.Error("error envelope must surface a non-nil error")
	}
	if _, _, err := parseOKXBillsOutput(errEnvelope, "", nil); err == nil {
		t.Error("exit-0-with-error must surface a non-nil error")
	}
	if _, _, err := parseOKXBillsOutput([]byte(`{"bills":[]}`), "", errors.New("exit 1")); err == nil {
		t.Error("exit-nonzero with no error field must be treated as failure")
	}
	if _, _, err := parseOKXBillsOutput([]byte(`not json`), "boom", errors.New("exit 2")); err == nil {
		t.Error("unparseable output must error")
	}
}
