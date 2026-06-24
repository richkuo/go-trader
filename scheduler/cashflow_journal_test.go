package main

import (
	"encoding/json"
	"math"
	"testing"
)

func TestCashflowFillSettledDelta(t *testing.T) {
	cases := []struct {
		closed, fee, want float64
	}{
		{0, 1.5, -1.5},   // open: fee only
		{100, 2, 98},     // close gross minus fee
		{50, -0.1, 50.1}, // maker rebate
	}
	for _, tc := range cases {
		if got := cashflowFillSettledDelta(tc.closed, tc.fee); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("closed=%v fee=%v: got %v want %v", tc.closed, tc.fee, got, tc.want)
		}
	}
}

func TestCashflowJournalExpectedEquity(t *testing.T) {
	// baseline accountValue=10000, baseline uPnL=500; settled +200; current uPnL=800 → +300 uPnL delta
	got := cashflowJournalExpectedEquity(10000, 500, 200, 800)
	want := 10000.0 + 200 + (800 - 500)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestAdvanceCashflowCursor(t *testing.T) {
	cases := []struct {
		name                   string
		current, maxProc, fail int64
		want                   int64
	}{
		{"advance past one event", 1000, 1500, -1, 1501},
		{"halt at failed event", 1000, 1500, 1400, 1400},
		{"no advance when nothing processed", 1000, 999, -1, 1000},
	}
	for _, tc := range cases {
		if got := advanceCashflowCursor(tc.current, tc.maxProc, tc.fail); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestCashflowFillDedupID(t *testing.T) {
	withTid := hlFillRecord{Tid: json.Number("42"), Time: 1, Hash: "h", Coin: "btc"}
	if got := cashflowFillDedupID(withTid); got != "fill:tid:42" {
		t.Errorf("tid key = %q", got)
	}
	noTid := hlFillRecord{Time: 99, Hash: "abc", Coin: "eth"}
	if got := cashflowFillDedupID(noTid); got != "fill:99:abc:ETH" {
		t.Errorf("fallback key = %q", got)
	}
}

func TestCashflowJournalIngestAndEquity(t *testing.T) {
	db := newLedgerTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xjournal"}
	nowMs := int64(1_700_000_000_000)
	st := CashflowJournalState{
		FillsSinceMs:         nowMs,
		FundingSinceMs:       nowMs,
		TransfersSinceMs:     nowMs,
		BaselineAccountValue: 10000,
		BaselineUPnL:         100,
		BaselineSet:          true,
	}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, st); err != nil {
		t.Fatalf("upsert state: %v", err)
	}

	eventMs := nowMs + 1000
	res := cashflowJournalFetchResult{
		Key: key, State: st, StateFound: true,
		AccountValue: 10550, CurrentUPnL: 150,
		Fills: []hlFillRecord{{
			Coin: "BTC", Time: eventMs, Tid: json.Number("1"),
			ClosedPnl: "50", Fee: "2",
		}},
		Funding: []hlLedgerEvent{{
			Time: eventMs + 1, Hash: "fh",
			Delta: hlLedgerEventDelta{Type: "funding", Coin: "BTC", USDC: "-1.5"},
		}},
		Transfers: []hlLedgerEvent{{
			Time: eventMs + 2, Hash: "th",
			Delta: hlLedgerEventDelta{Type: "deposit", USDC: "500"},
		}},
		FillsFetched: true, FundingFetched: true, TransfersFetched: true,
	}

	out := ingestCashflowJournalEvents(db, res)
	if out.FillsSinceMs <= nowMs {
		t.Errorf("fills cursor = %d, want > %d", out.FillsSinceMs, nowMs)
	}

	settled, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	wantSettled := 48.0 + (-1.5) + 500.0
	if math.Abs(settled-wantSettled) > 1e-9 {
		t.Errorf("settled = %v want %v", settled, wantSettled)
	}
	expected := cashflowJournalExpectedEquity(10000, 100, settled, 150)
	wantEquity := 10000.0 + wantSettled + 50.0
	if math.Abs(expected-wantEquity) > 1e-9 {
		t.Errorf("expected equity = %v want %v", expected, wantEquity)
	}
}

func TestCashflowJournalDedupIgnoresReplay(t *testing.T) {
	db := newLedgerTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xdedup"}
	nowMs := int64(1_700_000_000_000)
	st := CashflowJournalState{
		FillsSinceMs: nowMs, FundingSinceMs: nowMs, TransfersSinceMs: nowMs,
		BaselineAccountValue: 1, BaselineUPnL: 0, BaselineSet: true,
	}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, st); err != nil {
		t.Fatal(err)
	}
	fill := hlFillRecord{Coin: "ETH", Time: nowMs + 1, Tid: json.Number("99"), ClosedPnl: "10", Fee: "1"}
	res := cashflowJournalFetchResult{
		Key: key, State: st, StateFound: true,
		Fills: []hlFillRecord{fill, fill}, FillsFetched: true,
	}
	ingestCashflowJournalEvents(db, res)
	sum1, _ := db.SumCashflowJournal(key.Platform, key.Account)
	ingestCashflowJournalEvents(db, res)
	sum2, _ := db.SumCashflowJournal(key.Platform, key.Account)
	if math.Abs(sum1-9) > 1e-9 || math.Abs(sum2-sum1) > 1e-9 {
		t.Errorf("dedup failed: sum1=%v sum2=%v", sum1, sum2)
	}
}

func TestApplyHyperliquidCashflowJournalDrift_JournalWins(t *testing.T) {
	db := newLedgerTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xapply"}
	nowMs := int64(1_700_000_000_000)
	seedHLCashflowJournalBaseline(t, db, "0xapply", 1000, 50, nowMs)

	results := []sharedWalletDriftResult{{
		Key: key, Balance: 997, MemberSum: 990, Drift: 0,
		LedgerFallbackDrift: 0, DriftBasis: sharedWalletDriftBasisLedgerFallback,
		AttributionGap: -7,
	}}
	fetch := cashflowJournalFetchResult{
		Key: key, StateFound: true, PriorStateExists: true,
		AccountValue: 997, CurrentUPnL: 50,
		FillsFetched: true, FundingFetched: true, TransfersFetched: true,
	}
	st, _, _ := db.GetCashflowJournalState(key.Platform, key.Account)
	fetch.State = st
	applyHyperliquidCashflowJournalDrift(&results, db, fetch)
	if results[0].DriftBasis != sharedWalletDriftBasisCashflowJournal {
		t.Fatalf("basis = %q", results[0].DriftBasis)
	}
	if math.Abs(results[0].Drift-(-3)) > 1e-9 {
		t.Fatalf("journal drift = %v want -3", results[0].Drift)
	}
}

func TestApplyHyperliquidCashflowJournalDrift_IncompleteFallsBack(t *testing.T) {
	db := newLedgerTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xfallback"}
	nowMs := int64(1_700_000_000_000)
	st := CashflowJournalState{
		FillsSinceMs: nowMs, FundingSinceMs: nowMs, TransfersSinceMs: nowMs,
		BaselineAccountValue: 1000, BaselineUPnL: 0, BaselineSet: true, Incomplete: true,
	}
	if err := db.UpsertCashflowJournalState(key.Platform, key.Account, st); err != nil {
		t.Fatal(err)
	}
	results := []sharedWalletDriftResult{{
		Key: key, Balance: 1000, Drift: -5, LedgerFallbackDrift: -5,
		DriftBasis: sharedWalletDriftBasisLedgerFallback,
	}}
	fetch := cashflowJournalFetchResult{
		Key: key, State: st, StateFound: true, PriorStateExists: true,
		AccountValue: 1000, FillsFetched: true, FundingFetched: true, TransfersFetched: true,
	}
	applyHyperliquidCashflowJournalDrift(&results, db, fetch)
	if results[0].DriftBasis != sharedWalletDriftBasisLedgerFallback || math.Abs(results[0].Drift-(-5)) > 1e-9 {
		t.Fatalf("want ledger fallback preserved, got %+v", results[0])
	}
}

func TestCashflowJournalUsable(t *testing.T) {
	st := CashflowJournalState{BaselineSet: true}
	if !cashflowJournalUsable(cashflowJournalFetchResult{StateFound: true}, st) {
		t.Error("first anchor should be usable")
	}
	if cashflowJournalUsable(cashflowJournalFetchResult{StateFound: true, PriorStateExists: true, FillsFetched: true}, CashflowJournalState{BaselineSet: true, Incomplete: true}) {
		t.Error("incomplete must fail")
	}
	if !cashflowJournalUsable(cashflowJournalFetchResult{
		StateFound: true, PriorStateExists: true,
		FillsFetched: true, FundingFetched: true, TransfersFetched: true,
	}, st) {
		t.Error("all streams ok should be usable")
	}
}

func TestSumHLAccountUPnL(t *testing.T) {
	got := sumHLAccountUPnL([]HLPosition{
		{UnrealizedPnL: 10}, {UnrealizedPnL: -3},
	})
	if math.Abs(got-7) > 1e-9 {
		t.Errorf("got %v want 7", got)
	}
}
