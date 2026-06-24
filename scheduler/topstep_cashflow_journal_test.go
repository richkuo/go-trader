package main

import (
	"errors"
	"math"
	"testing"
	"time"
)

// #1106 TopStep exchange-sourced cash-flow journal (shadow phase): the fill
// settled-cash convention (gross realized PnL − commission, HL-like), kind
// classification + fail-closed on unmapped kinds, dedup, single-stream cursor
// discipline, snapshot bounding, baseline anchoring, and usability gating.

// A fill's settled-cash delta is gross realized PnL minus the commission charged.
// A known kind is classifiable; an unknown kind still books gross−fee but is NOT
// known (latches incomplete). An empty kind is a plain trade fill.
func TestTopStepFillSettledDelta(t *testing.T) {
	cases := []struct {
		name      string
		fill      topstepFillRecord
		wantDelta float64
		wantKind  string
		wantKnown bool
	}{
		{"closing fill pnl-fee net", topstepFillRecord{Kind: "trade", RealizedPnL: 20, Fee: 0.3}, 19.7, "trade", true},
		{"entry fill settles -commission", topstepFillRecord{Kind: "", RealizedPnL: 0, Fee: 0.5}, -0.5, "trade", true},
		{"standalone commission", topstepFillRecord{Kind: "commission", RealizedPnL: 0, Fee: 1.0}, -1.0, "commission", true},
		{"fee synonym maps to commission", topstepFillRecord{Kind: "fee", RealizedPnL: 0, Fee: 0.25}, -0.25, "commission", true},
		{"maker rebate (negative fee → positive delta)", topstepFillRecord{Kind: "trade", RealizedPnL: 0, Fee: -0.1}, 0.1, "trade", true},
		{"unknown kind books gross-fee but not known", topstepFillRecord{Kind: "payout", RealizedPnL: 0, Fee: 0}, 0, "kind_payout", false},
	}
	for _, tc := range cases {
		gotDelta, gotKind, gotKnown := topstepFillSettledDelta(tc.fill)
		if math.Abs(gotDelta-tc.wantDelta) > 1e-9 || gotKind != tc.wantKind || gotKnown != tc.wantKnown {
			t.Errorf("%s: topstepFillSettledDelta = (%v,%q,%v), want (%v,%q,%v)",
				tc.name, gotDelta, gotKind, gotKnown, tc.wantDelta, tc.wantKind, tc.wantKnown)
		}
	}
}

// fillId is the canonical key; the kind:ts:symbol form is the fallback when
// fillId is absent or "0". Namespaced "topstepfill:" so it can never collide with
// the HL fill/funding/transfer or OKX bill namespaces in the shared table.
func TestTopStepFillDedupID(t *testing.T) {
	withID := topstepFillRecord{FillID: "f1", Kind: "trade", TimeMs: 1700000000000, Symbol: "ES"}
	if got, want := topstepFillDedupID(withID), "topstepfill:f1"; got != want {
		t.Errorf("fillId present: got %q, want %q", got, want)
	}
	zeroID := topstepFillRecord{FillID: "0", Kind: "trade", TimeMs: 1700000000001, Symbol: "es"}
	if got, want := topstepFillDedupID(zeroID), "topstepfill:trade:1700000000001:ES"; got != want {
		t.Errorf("fillId zero: got %q, want %q", got, want)
	}
	emptyID := topstepFillRecord{Kind: "commission", TimeMs: 1700000000002, Symbol: "NQ"}
	if got, want := topstepFillDedupID(emptyID), "topstepfill:commission:1700000000002:NQ"; got != want {
		t.Errorf("fillId empty: got %q, want %q", got, want)
	}
}

func newTopStepJournalKey() SharedWalletKey {
	return SharedWalletKey{Platform: "topstep", Account: "ts-acct-123"}
}

// A non-positive (or NaN) equity must be treated as a fetch MISS (error), so the
// shadow journal skips the cycle instead of reconciling against a garbage $0
// equity (which would emit phantom drift and corrupt the persisted baseline). A
// genuinely positive equity flows through unchanged.
func TestValidatedTopStepEquity(t *testing.T) {
	// Good positive equity passes through with uPnL preserved.
	if eq, upnl, err := validatedTopStepEquity(50000.0, 12.5); err != nil || eq != 50000.0 || upnl != 12.5 {
		t.Errorf("positive equity: got (%v,%v,%v), want (50000,12.5,nil)", eq, upnl, err)
	}
	// Missing-equity-coerced-to-0, exact 0, negative, and NaN all become a miss.
	for _, bad := range []float64{0, -1, math.NaN()} {
		if _, _, err := validatedTopStepEquity(bad, 5.0); err == nil {
			t.Errorf("equity %v must be treated as a fetch miss (error), got nil", bad)
		}
	}
}

// First contact anchors the baseline to the supplied equity/uPnL snapshot and the
// fills cursor to now, fetching NO history; TopStep uses only the FillsSinceMs
// cursor (funding/transfers stay 0).
func TestTopStepCashflowJournalBaselineAnchor(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newTopStepJournalKey()
	now := time.UnixMilli(1700000000000).UTC()

	res := fetchTopStepCashflowJournalEvents(db, key, 50000.0, 120.0, now)
	if !res.StateFound || res.FillsFetched {
		t.Fatalf("baseline cycle: StateFound=%v FillsFetched=%v, want true/false", res.StateFound, res.FillsFetched)
	}
	st, found, err := db.GetCashflowJournalState(key.Platform, key.Account)
	if err != nil || !found {
		t.Fatalf("state after baseline: found=%v err=%v", found, err)
	}
	if !st.BaselineSet || st.BaselineAccountValue != 50000.0 || st.BaselineUPnL != 120.0 {
		t.Errorf("baseline not anchored: %+v", st)
	}
	if st.FillsSinceMs != now.UnixMilli() {
		t.Errorf("fills cursor not anchored at now: %d, want %d", st.FillsSinceMs, now.UnixMilli())
	}
	if st.FundingSinceMs != 0 || st.TransfersSinceMs != 0 {
		t.Errorf("TopStep must leave funding/transfers cursors unused: %+v", st)
	}
}

// End-to-end fetch -> ingest -> sum with a stubbed fills feed. The settled sum is
// Σ(gross − fee) over fills; the expected equity closes the loop against a
// hand-computed equity.
func TestTopStepCashflowJournalIngestAndExpectedEquity(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newTopStepJournalKey()
	t0 := time.UnixMilli(1700000000000).UTC()

	// Anchor at equity=1000, uPnL=0, cursor t0.
	if r := fetchTopStepCashflowJournalEvents(db, key, 1000.0, 0.0, t0); !r.StateFound {
		t.Fatal("baseline init failed")
	}

	orig := fetchTopStepAccountFills
	defer func() { fetchTopStepAccountFills = orig }()
	fetchTopStepAccountFills = func(sinceMs int64) ([]topstepFillRecord, bool, error) {
		return []topstepFillRecord{
			{FillID: "f1", TimeMs: t0.UnixMilli() + 10, Symbol: "ES", Kind: "trade", RealizedPnL: 0, Fee: 0.5},   // entry: -0.5
			{FillID: "f2", TimeMs: t0.UnixMilli() + 20, Symbol: "ES", Kind: "trade", RealizedPnL: 20, Fee: 0.3},  // close: 19.7
			{FillID: "f3", TimeMs: t0.UnixMilli() + 5, Symbol: "NQ", Kind: "commission", RealizedPnL: 0, Fee: 1}, // -1.0
		}, false, nil
	}

	snap := t0.Add(time.Minute)
	// equity = 1000 + (−0.5 + 19.7 − 1.0) + (uPnL 5 − 0) = 1023.2
	res := fetchTopStepCashflowJournalEvents(db, key, 1023.2, 5.0, snap)
	if !res.FillsFetched || res.Capped {
		t.Fatalf("expected fills fetched, not capped: %+v", res)
	}
	st := ingestTopStepCashflowJournalEvents(db, res, snap.UnixMilli())
	if st.Incomplete {
		t.Error("all fills classified — journal must not be marked incomplete")
	}

	settled, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	const wantSettled = -0.5 + 19.7 - 1.0 // 18.2
	if math.Abs(settled-wantSettled) > 1e-9 {
		t.Fatalf("settled sum = %v, want %v", settled, wantSettled)
	}

	expected := cashflowJournalExpectedEquity(st.BaselineAccountValue, st.BaselineUPnL, settled, res.CurrentUPnL)
	if math.Abs(expected-1023.2) > 1e-9 {
		t.Errorf("expected equity = %v, want 1023.2", expected)
	}
	if drift := res.AccountValue - expected; math.Abs(drift) > 1e-9 {
		t.Errorf("journal drift = %v, want ~0", drift)
	}
	if st.FillsSinceMs != t0.UnixMilli()+20+1 {
		t.Errorf("fills cursor = %d, want %d", st.FillsSinceMs, t0.UnixMilli()+21)
	}
}

// A fill settled AFTER the equity snapshot is deferred to next cycle: not booked,
// cursor not advanced past it.
func TestTopStepCashflowJournalSnapshotBound(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newTopStepJournalKey()
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res := topstepCashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, FillsFetched: true,
		Fills: []topstepFillRecord{
			{FillID: "in", TimeMs: 150, Symbol: "ES", Kind: "trade", RealizedPnL: 10},
			{FillID: "after", TimeMs: 250, Symbol: "ES", Kind: "trade", RealizedPnL: 999}, // settled after cutoff
		},
	}
	st := ingestTopStepCashflowJournalEvents(db, res, 200) // cutoff 200
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-10) > 1e-9 {
		t.Errorf("post-cutoff fill leaked: sum = %v, want 10", sum)
	}
	if st.FillsSinceMs != 151 {
		t.Errorf("cursor must stop before the deferred fill: got %d, want 151", st.FillsSinceMs)
	}
}

// A NON-capped fetch advances the cursor past the last booked fill (maxTime+1),
// but a CAPPED fetch advances only TO maxTime so the boundary millisecond is
// re-read next cycle (cursor-side complement of the adapter's fail-closed cap).
func TestTopStepCashflowJournalCappedAdvancesOnlyToMaxTime(t *testing.T) {
	mk := func(capped bool) CashflowJournalState {
		db := newCashflowJournalTestDB(t)
		key := newTopStepJournalKey()
		base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
		if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
			t.Fatalf("seed: %v", err)
		}
		res := topstepCashflowJournalFetchResult{
			Key: key, State: base, StateFound: true, FillsFetched: true, Capped: capped,
			Fills: []topstepFillRecord{
				{FillID: "a", TimeMs: 150, Symbol: "ES", Kind: "trade", RealizedPnL: 5},
				{FillID: "b", TimeMs: 200, Symbol: "ES", Kind: "trade", RealizedPnL: 5}, // maxTime = 200
			},
		}
		return ingestTopStepCashflowJournalEvents(db, res, cashflowCutoffAll)
	}
	if st := mk(false); st.FillsSinceMs != 201 {
		t.Errorf("non-capped: cursor = %d, want 201 (maxTime+1)", st.FillsSinceMs)
	}
	if st := mk(true); st.FillsSinceMs != 200 {
		t.Errorf("capped: cursor = %d, want 200 (maxTime — boundary ms re-read next cycle)", st.FillsSinceMs)
	}
}

// An unclassified fill (unknown kind) latches incomplete AND still books its
// authoritative gross−fee delta so the running drift surfaces it.
func TestTopStepCashflowJournalUnclassifiedLatchesIncomplete(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newTopStepJournalKey()
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res := topstepCashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, FillsFetched: true,
		Fills: []topstepFillRecord{
			{FillID: "u1", TimeMs: 150, Symbol: "ES", Kind: "payout", RealizedPnL: 0, Fee: -7}, // unknown kind, balance change +7
		},
	}
	st := ingestTopStepCashflowJournalEvents(db, res, cashflowCutoffAll)
	if !st.Incomplete {
		t.Error("unknown fill kind must latch incomplete")
	}
	sum, _ := db.SumCashflowJournal(key.Platform, key.Account)
	if math.Abs(sum-7) > 1e-9 {
		t.Errorf("unknown-kind gross−fee must still be booked (authoritative): sum = %v, want 7", sum)
	}
}

// A fill insert failure halts the cursor at the failed fill so a crash can never
// strand an un-booked event behind an advanced watermark.
func TestTopStepCashflowJournalHaltsCursorOnPersistFailure(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newTopStepJournalKey()
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	db.Close()
	res := topstepCashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, FillsFetched: true,
		Fills: []topstepFillRecord{{FillID: "b", TimeMs: 150, Symbol: "ES", Kind: "trade", RealizedPnL: 10}},
	}
	st := ingestTopStepCashflowJournalEvents(db, res, cashflowCutoffAll)
	if st.FillsSinceMs != 100 {
		t.Errorf("cursor advanced past an un-booked fill: got %d, want 100", st.FillsSinceMs)
	}
}

// reconcile is usable only when fills fetched, not capped, and not incomplete.
func TestTopStepCashflowJournalReconcileUsability(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := newTopStepJournalKey()
	t0 := time.UnixMilli(1700000000000).UTC()
	if r := fetchTopStepCashflowJournalEvents(db, key, 1000.0, 0.0, t0); !r.StateFound {
		t.Fatal("baseline init failed")
	}
	orig := fetchTopStepAccountFills
	defer func() { fetchTopStepAccountFills = orig }()

	// Clean, classifiable fill → usable.
	fetchTopStepAccountFills = func(int64) ([]topstepFillRecord, bool, error) {
		return []topstepFillRecord{{FillID: "f1", TimeMs: t0.UnixMilli() + 10, Symbol: "ES", Kind: "trade", RealizedPnL: 5}}, false, nil
	}
	snap := t0.Add(time.Minute)
	rec := reconcileTopStepCashflowJournal(db, key, 1005.0, 0.0, snap)
	if rec == nil || !rec.Usable || rec.Incomplete {
		t.Fatalf("clean cycle: rec=%+v, want usable & not incomplete", rec)
	}
	if math.Abs(rec.Drift) > 1e-9 {
		t.Errorf("clean cycle drift = %v, want ~0", rec.Drift)
	}

	// Capped fetch → not usable even though a baseline exists.
	fetchTopStepAccountFills = func(int64) ([]topstepFillRecord, bool, error) {
		return []topstepFillRecord{{FillID: "f2", TimeMs: snap.UnixMilli() + 10, Symbol: "ES", Kind: "trade", RealizedPnL: 1}}, true, nil
	}
	snap2 := snap.Add(time.Minute)
	rec2 := reconcileTopStepCashflowJournal(db, key, 1006.0, 0.0, snap2)
	if rec2 == nil || rec2.Usable {
		t.Fatalf("capped cycle: rec=%+v, want not usable", rec2)
	}

	// Fetch error → StateFound true but fills not fetched, not usable.
	fetchTopStepAccountFills = func(int64) ([]topstepFillRecord, bool, error) {
		return nil, false, errors.New("topstep fills 500")
	}
	snap3 := snap2.Add(time.Minute)
	rec3 := reconcileTopStepCashflowJournal(db, key, 1007.0, 0.0, snap3)
	if rec3 == nil || rec3.Usable {
		t.Fatalf("fetch-error cycle: rec=%+v, want not usable", rec3)
	}
}

// reconcile returns nil for a non-TopStep key (HL/OKX stay on their own paths).
func TestTopStepCashflowJournalRejectsNonTopStepKey(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	okxKey := SharedWalletKey{Platform: "okx", Account: "okx-key"}
	if rec := reconcileTopStepCashflowJournal(db, okxKey, 1000, 0, time.Now()); rec != nil {
		t.Errorf("non-TopStep key must return nil, got %+v", rec)
	}
}

// The shadow logger never mutates driftResults (TopStep has no live alarm).
func TestTopStepCashflowJournalShadowDoesNotMutate(t *testing.T) {
	key := newTopStepJournalKey()
	results := []sharedWalletDriftResult{
		{Key: key, Drift: 1.23, Balance: 50000, MemberSum: 49998.77},
	}
	rec := &cashflowJournalReconcile{Key: key, AccountValue: 50000, ExpectedEquity: 50002.5, Drift: -2.5, Usable: true}
	logTopStepCashflowJournalShadow(results, key, rec)
	if results[0].Drift != 1.23 || results[0].Basis != "" || results[0].ExpectedEquity != 0 {
		t.Errorf("shadow log mutated the drift result: %+v", results[0])
	}
}

// parseTopStepFillsOutput follows the 5-case matrix: clean success,
// exit-0-with-error, exit-nonzero-with-error, exit-nonzero-no-error, unparseable.
func TestParseTopStepFillsOutput(t *testing.T) {
	clean := []byte(`{"fills":[{"fill_id":"f1","ts_ms":1700000000000,"symbol":"ES","kind":"trade","realized_pnl":19.7,"fee":0.3}],"capped":false,"platform":"topstep","timestamp":"t"}`)
	res, _, err := parseTopStepFillsOutput(clean, "", nil)
	if err != nil || res == nil || len(res.Fills) != 1 || res.Fills[0].FillID != "f1" || res.Fills[0].RealizedPnL != 19.7 {
		t.Fatalf("clean: res=%+v err=%v", res, err)
	}
	if res.Capped {
		t.Error("clean: capped should be false")
	}

	errEnvelope := []byte(`{"fills":[],"capped":false,"platform":"topstep","timestamp":"t","error":"not live"}`)
	if _, _, err := parseTopStepFillsOutput(errEnvelope, "", errors.New("exit 1")); err == nil {
		t.Error("error envelope must surface a non-nil error")
	}
	if _, _, err := parseTopStepFillsOutput(errEnvelope, "", nil); err == nil {
		t.Error("exit-0-with-error must surface a non-nil error")
	}
	if _, _, err := parseTopStepFillsOutput([]byte(`{"fills":[]}`), "", errors.New("exit 1")); err == nil {
		t.Error("exit-nonzero with no error field must be treated as failure")
	}
	if _, _, err := parseTopStepFillsOutput([]byte(`not json`), "boom", errors.New("exit 2")); err == nil {
		t.Error("unparseable output must error")
	}
}

// parseTopStepBalanceOutput follows the same 5-case matrix.
func TestParseTopStepBalanceOutput(t *testing.T) {
	clean := []byte(`{"balance":50000.5,"unrealized_pnl":12.5,"platform":"topstep","timestamp":"t"}`)
	res, _, err := parseTopStepBalanceOutput(clean, "", nil)
	if err != nil || res == nil || res.Balance != 50000.5 || res.UnrealizedPnL != 12.5 {
		t.Fatalf("clean: res=%+v err=%v", res, err)
	}
	errEnvelope := []byte(`{"balance":0,"unrealized_pnl":0,"platform":"topstep","timestamp":"t","error":"not live"}`)
	if _, _, err := parseTopStepBalanceOutput(errEnvelope, "", nil); err == nil {
		t.Error("exit-0-with-error must surface a non-nil error")
	}
	if _, _, err := parseTopStepBalanceOutput([]byte(`nope`), "boom", errors.New("exit 2")); err == nil {
		t.Error("unparseable output must error")
	}
}
