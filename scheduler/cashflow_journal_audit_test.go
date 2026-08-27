package main


import (
	"encoding/json"
	"math"
	"testing"
)

func TestApplyCashflowJournalDriftBasis_UsableNonZeroJournalDriftOverridesLedger(t *testing.T) {
	prevPending := cashflowJournalPendingStreaks
	cashflowJournalPendingStreaks = &cashflowJournalPendingTracker{}
	defer func() { cashflowJournalPendingStreaks = prevPending }()

	hlKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xaudit"}
	label := sharedWalletKeyLabel(hlKey)

	cashflowJournalPendingStreaks.mark(label)
	cashflowJournalPendingStreaks.mark(label)

	res := []sharedWalletDriftResult{{Key: hlKey, Drift: 0.02, Balance: 1000, MemberSum: 999.98}}
	rec := &cashflowJournalReconcile{Key: hlKey, Usable: true, Drift: 50.0, ExpectedEquity: 950, AccountValue: 1000}
	applyCashflowJournalDriftBasis(res, hlKey, rec, true)

	if res[0].Basis != driftBasisJournal {
		t.Errorf("Basis = %q, want %q", res[0].Basis, driftBasisJournal)
	}
	if math.Abs(res[0].Drift-50.0) > 1e-9 {
		t.Errorf("journal drift must override ledger drift: got %v, want 50.0", res[0].Drift)
	}
	if math.Abs(res[0].ExpectedEquity-950) > 1e-9 {
		t.Errorf("ExpectedEquity = %v, want 950", res[0].ExpectedEquity)
	}
	if res[0].JournalPending {
		t.Errorf("usable cycle must not be journal-pending: %+v", res[0])
	}
	if got := cashflowJournalPendingStreaks.mark(label); got != 1 {
		t.Errorf("pending streak not reset by usable cycle: next mark = %d, want 1", got)
	}
	cashflowJournalPendingStreaks.reset(label)

	res = []sharedWalletDriftResult{{Key: hlKey, Drift: 0.40, Balance: 1000, MemberSum: 999.6}}
	rec = &cashflowJournalReconcile{Key: hlKey, Usable: true, Drift: 0.0, ExpectedEquity: 1000, AccountValue: 1000}
	applyCashflowJournalDriftBasis(res, hlKey, rec, true)
	if res[0].Basis != driftBasisJournal || math.Abs(res[0].Drift) > 1e-9 {
		t.Errorf("zero journal drift must override ledger 0.40: %+v", res[0])
	}
	if math.Abs(res[0].ExpectedEquity-1000) > 1e-9 {
		t.Errorf("ExpectedEquity = %v, want 1000", res[0].ExpectedEquity)
	}
}

func TestApplyCashflowJournalDriftBasis_RefusesNonHLKey(t *testing.T) {
	prevPending := cashflowJournalPendingStreaks
	cashflowJournalPendingStreaks = &cashflowJournalPendingTracker{}
	defer func() { cashflowJournalPendingStreaks = prevPending }()

	okxKey := SharedWalletKey{Platform: "okx", Account: "shadow"}
	res := []sharedWalletDriftResult{{Key: okxKey, Drift: 1.23, Balance: 500, MemberSum: 498.77}}
	rec := &cashflowJournalReconcile{Key: okxKey, Usable: true, Drift: -2.5, ExpectedEquity: 497.5, AccountValue: 500}
	applyCashflowJournalDriftBasis(res, okxKey, rec, true)

	if res[0].Basis != "" || res[0].Drift != 1.23 || res[0].JournalPending {
		t.Errorf("non-HL key must be refused (entry unchanged): %+v", res[0])
	}
	if got := cashflowJournalPendingStreaks.mark(sharedWalletKeyLabel(okxKey)); got != 1 {
		t.Errorf("guard must not mutate the pending tracker: next mark = %d, want 1", got)
	}
}

func TestCashflowJournalIngestBooksEventAtWatermark(t *testing.T) {
	t.Run("hyperliquid", func(t *testing.T) {
		db := newCashflowJournalTestDB(t)
		key := SharedWalletKey{Platform: "hyperliquid", Account: "0xwm"}
		base := CashflowJournalState{FillsSinceMs: 150, BaselineSet: true}
		res := cashflowJournalFetchResult{
			Key: key, State: base, StateFound: true, FillsFetched: true,
			Fills: []hlFillRecord{
				{Coin: "BTC", Time: 150, Tid: json.Number("1"), ClosedPnl: "10", Fee: "0.2"},
			},
		}
		st := ingestCashflowJournalEvents(db, res, cashflowCutoffAll)
		sum, err := db.SumCashflowJournal(key.Platform, key.Account)
		if err != nil {
			t.Fatalf("sum: %v", err)
		}
		if math.Abs(sum-9.8) > 1e-9 {
			t.Errorf("fill at exact watermark not booked: sum = %v, want 9.8", sum)
		}
		if st.FillsSinceMs != 151 {
			t.Errorf("cursor = %d, want 151", st.FillsSinceMs)
		}
	})

	t.Run("okx", func(t *testing.T) {
		db := newCashflowJournalTestDB(t)
		key := newOKXJournalKey()
		base := CashflowJournalState{FillsSinceMs: 150, BaselineSet: true}
		res := okxCashflowJournalFetchResult{
			Key: key, State: base, StateFound: true, BillsFetched: true,
			Bills: []okxBillRecord{
				{BillID: "wm", TimeMs: 150, Ccy: "USDT", Type: "2", BalChg: 9.8},
			},
		}
		st := ingestOKXCashflowJournalEvents(db, res, cashflowCutoffAll)
		sum, err := db.SumCashflowJournal(key.Platform, key.Account)
		if err != nil {
			t.Fatalf("sum: %v", err)
		}
		if math.Abs(sum-9.8) > 1e-9 {
			t.Errorf("bill at exact watermark not booked: sum = %v, want 9.8", sum)
		}
		if st.FillsSinceMs != 151 {
			t.Errorf("cursor = %d, want 151", st.FillsSinceMs)
		}
	})

	t.Run("topstep", func(t *testing.T) {
		db := newCashflowJournalTestDB(t)
		key := newTopStepJournalKey()
		base := CashflowJournalState{FillsSinceMs: 150, BaselineSet: true}
		res := topstepCashflowJournalFetchResult{
			Key: key, State: base, StateFound: true, FillsFetched: true,
			Fills: []topstepFillRecord{
				{FillID: "wm", TimeMs: 150, Symbol: "ES", Kind: "trade", RealizedPnL: 10, Fee: 0.2},
			},
		}
		st := ingestTopStepCashflowJournalEvents(db, res, cashflowCutoffAll)
		sum, err := db.SumCashflowJournal(key.Platform, key.Account)
		if err != nil {
			t.Fatalf("sum: %v", err)
		}
		if math.Abs(sum-9.8) > 1e-9 {
			t.Errorf("fill at exact watermark not booked: sum = %v, want 9.8", sum)
		}
		if st.FillsSinceMs != 151 {
			t.Errorf("cursor = %d, want 151", st.FillsSinceMs)
		}
	})
}

func TestCashflowJournalIngestBooksEventAtCutoff(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xcut"}
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	res := cashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, FillsFetched: true,
		Fills: []hlFillRecord{
			{Coin: "BTC", Time: 200, Tid: json.Number("1"), ClosedPnl: "10", Fee: "0.2"},
			{Coin: "BTC", Time: 201, Tid: json.Number("2"), ClosedPnl: "30", Fee: "0.5"},
		},
	}
	st := ingestCashflowJournalEvents(db, res, 200)
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-9.8) > 1e-9 {
		t.Errorf("fill at exact cutoff must book: sum = %v, want 9.8", sum)
	}
	if st.FillsSinceMs != 201 {
		t.Errorf("cursor = %d, want 201 (one past the booked fill, not past the deferred one)", st.FillsSinceMs)
	}
}

func TestSumCashflowJournalIgnoresClosedPnlGross(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xsum"}

	if err := db.InsertCashflowJournalEntry(key.Platform, key.Account, 100, "fill", 19.7, "BTC", 20, 0.3, "fill:1"); err != nil {
		t.Fatalf("insert fill: %v", err)
	}
	if err := db.InsertCashflowJournalEntry(key.Platform, key.Account, 110, "deposit", 100, "", 0, 0, "xfer:1"); err != nil {
		t.Fatalf("insert deposit: %v", err)
	}
	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-119.7) > 1e-9 {
		t.Errorf("sum = %v, want 119.7 (19.7 net + 100 deposit; closed_pnl_gross excluded)", sum)
	}
}

func TestCashflowJournalBooksZeroAmountEvents(t *testing.T) {
	db := newCashflowJournalTestDB(t)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xzero"}
	base := CashflowJournalState{FillsSinceMs: 100, BaselineSet: true}
	res := cashflowJournalFetchResult{
		Key: key, State: base, StateFound: true, FillsFetched: true,
		Fills: []hlFillRecord{
			{Coin: "BTC", Time: 150, Tid: json.Number("1"), ClosedPnl: "0", Fee: "0"},
			{Coin: "BTC", Time: 160, Tid: json.Number("2"), ClosedPnl: "10", Fee: "0.2"},
		},
	}
	st := ingestCashflowJournalEvents(db, res, cashflowCutoffAll)

	sum, err := db.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if math.Abs(sum-9.8) > 1e-9 {
		t.Errorf("sum = %v, want 9.8 (zero-delta fill contributes 0)", sum)
	}
	var rows int
	if err := db.db.QueryRow(
		`SELECT COUNT(*) FROM cashflow_journal WHERE platform = ? AND account = ?`,
		key.Platform, key.Account).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 2 {
		t.Errorf("zero-amount row must persist: rows = %d, want 2", rows)
	}
	if st.FillsSinceMs != 161 {
		t.Errorf("cursor must advance past the zero-amount fill: %d, want 161", st.FillsSinceMs)
	}
	if st.Incomplete {
		t.Error("a mapped zero-amount fill must NOT latch Incomplete")
	}
}
