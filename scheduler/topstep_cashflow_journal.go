package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type topstepFillRecord struct {
	FillID      string  `json:"fill_id"`
	TimeMs      int64   `json:"ts_ms"`
	Symbol      string  `json:"symbol"`
	Kind        string  `json:"kind"`
	RealizedPnL float64 `json:"realized_pnl"`
	Fee         float64 `json:"fee"`
}

var topstepFillKindByType = map[string]string{
	"":           "trade",
	"trade":      "trade",
	"fill":       "trade",
	"commission": "commission",
	"fee":        "commission",
}

func topstepFillSettledDelta(f topstepFillRecord) (delta float64, kind string, known bool) {
	delta = cashflowFillSettledDelta(f.RealizedPnL, f.Fee)
	k := strings.ToLower(strings.TrimSpace(f.Kind))
	if mapped, ok := topstepFillKindByType[k]; ok {
		return delta, mapped, true
	}
	return delta, "kind_" + k, false
}

func topstepFillDedupID(f topstepFillRecord) string {
	if id := strings.TrimSpace(f.FillID); id != "" && id != "0" {
		return "topstepfill:" + id
	}
	return fmt.Sprintf("topstepfill:%s:%d:%s", strings.TrimSpace(f.Kind), f.TimeMs, strings.ToUpper(strings.TrimSpace(f.Symbol)))
}

var topstepFillsScript = "shared_scripts/fetch_topstep_fills.py"

var topstepBalanceScript = "shared_scripts/fetch_topstep_balance.py"

func defaultTopStepFillsFetcher(sinceMs int64) ([]topstepFillRecord, bool, error) {
	result, stderr, err := RunTopStepFetchFills(topstepFillsScript, sinceMs)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[topstep-cashflow-journal] fetch_fills stderr: %s\n", stderr)
	}
	if err != nil {
		return nil, false, err
	}
	return result.Fills, result.Capped, nil
}

var fetchTopStepAccountFills = func(sinceMs int64) (fills []topstepFillRecord, capped bool, err error) {
	return defaultTopStepFillsFetcher(sinceMs)
}

func defaultTopStepEquitySnapshot() (equity, upnl float64, err error) {
	if os.Getenv("TOPSTEP_ACCOUNT_ID") == "" {
		return 0, 0, fmt.Errorf("TOPSTEP_ACCOUNT_ID not set")
	}
	result, stderr, err := RunTopStepFetchBalance(topstepBalanceScript)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[topstep-balance] stderr: %s\n", stderr)
	}
	if err != nil {
		return 0, 0, err
	}
	return validatedTopStepEquity(result.Balance, result.UnrealizedPnL)
}

func validatedTopStepEquity(balance, upnl float64) (equity, upnlOut float64, err error) {
	if !(balance > 0) {
		return 0, 0, fmt.Errorf("non-positive TopStep equity $%.2f — treating as a fetch miss (malformed /v1/account/balance response)", balance)
	}
	return balance, upnl, nil
}

type topstepCashflowJournalFetchResult struct {
	Key          SharedWalletKey
	State        CashflowJournalState
	StateFound   bool
	AccountValue float64
	CurrentUPnL  float64
	Fills        []topstepFillRecord
	FillsFetched bool
	Capped       bool
}

func fetchTopStepCashflowJournalEvents(sdb *StateDB, key SharedWalletKey, accountValue, currentUPnL float64, now time.Time) topstepCashflowJournalFetchResult {
	res := topstepCashflowJournalFetchResult{Key: key, AccountValue: accountValue, CurrentUPnL: currentUPnL}
	if sdb == nil || key.Platform != "topstep" || key.Account == "" {
		return res
	}
	st, found, err := sdb.GetCashflowJournalState(key.Platform, key.Account)
	if err != nil {
		fmt.Printf("[WARN] topstep-cashflow-journal %s: state load failed: %v — skipping ingestion this cycle\n", sharedWalletKeyLabel(key), err)
		return res
	}
	if !found {
		nowMs := now.UnixMilli()
		st = CashflowJournalState{
			FillsSinceMs:         nowMs,
			BaselineAccountValue: accountValue,
			BaselineUPnL:         currentUPnL,
			BaselineSet:          true,
		}
		if err := sdb.UpsertCashflowJournalState(key.Platform, key.Account, st); err != nil {
			fmt.Printf("[WARN] topstep-cashflow-journal %s: baseline init failed: %v\n", sharedWalletKeyLabel(key), err)
			return res
		}
		fmt.Printf("[topstep-cashflow-journal] %s: baseline anchored at equity $%.2f (uPnL $%+.2f) and fills cursor at %s (no historical replay)\n",
			sharedWalletKeyLabel(key), accountValue, currentUPnL, now.UTC().Format(time.RFC3339))
		res.State = st
		res.StateFound = true
		return res
	}
	res.State = st
	res.StateFound = true

	fills, capped, err := fetchTopStepAccountFills(st.FillsSinceMs)
	if err != nil {
		fmt.Printf("[WARN] topstep-cashflow-journal %s: fills fetch failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
		return res
	}
	res.Fills = fills
	res.FillsFetched = true
	res.Capped = capped
	return res
}

func ingestTopStepCashflowJournalEvents(sdb *StateDB, res topstepCashflowJournalFetchResult, cutoffMs int64) CashflowJournalState {
	st := res.State
	if sdb == nil || !res.StateFound || !res.FillsFetched {
		return st
	}
	key := res.Key

	maxTime := st.FillsSinceMs - 1
	failedAt := int64(-1)
	fills := append([]topstepFillRecord(nil), res.Fills...)
	sort.SliceStable(fills, func(i, j int) bool { return fills[i].TimeMs < fills[j].TimeMs })
	for _, f := range fills {
		if f.TimeMs < st.FillsSinceMs {
			continue
		}
		if f.TimeMs > cutoffMs {
			continue
		}
		delta, kind, known := topstepFillSettledDelta(f)
		if !known {
			st.Incomplete = true
			fmt.Printf("[WARN] topstep-cashflow-journal %s: unclassified fill kind=%q (fillId %s) — recorded kind %q, journal marked incomplete\n",
				sharedWalletKeyLabel(key), f.Kind, f.FillID, kind)
		}
		coin := strings.ToUpper(strings.TrimSpace(f.Symbol))
		if err := sdb.InsertCashflowJournalEntry(key.Platform, key.Account, f.TimeMs, kind, delta, coin, f.RealizedPnL, f.Fee, topstepFillDedupID(f)); err != nil {
			fmt.Printf("[WARN] topstep-cashflow-journal %s: fill insert failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
			failedAt = f.TimeMs
			break
		}
		if f.TimeMs > maxTime {
			maxTime = f.TimeMs
		}
	}
	advanceMax := maxTime
	if res.Capped {
		advanceMax = maxTime - 1
	}
	st.FillsSinceMs = advanceCashflowCursor(st.FillsSinceMs, advanceMax, failedAt)

	if st != res.State {
		if err := sdb.UpsertCashflowJournalState(key.Platform, key.Account, st); err != nil {
			fmt.Printf("[WARN] topstep-cashflow-journal %s: cursor advance failed: %v\n", sharedWalletKeyLabel(key), err)
		}
	}
	return st
}

func reconcileTopStepCashflowJournal(sdb *StateDB, key SharedWalletKey, accountValue, currentUPnL float64, snapshotAt time.Time) *cashflowJournalReconcile {
	if sdb == nil || key.Platform != "topstep" || key.Account == "" {
		return nil
	}
	res := fetchTopStepCashflowJournalEvents(sdb, key, accountValue, currentUPnL, snapshotAt)
	if !res.StateFound {
		return nil
	}
	st := ingestTopStepCashflowJournalEvents(sdb, res, snapshotAt.UnixMilli())
	rec := &cashflowJournalReconcile{Key: key, AccountValue: res.AccountValue, Incomplete: st.Incomplete}
	if !st.BaselineSet {
		return rec
	}
	settled, err := sdb.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		fmt.Printf("[WARN] topstep-cashflow-journal %s: settled-sum read failed: %v\n", sharedWalletKeyLabel(key), err)
		return rec
	}
	rec.SettledSum = settled
	rec.DeltaUPnL = res.CurrentUPnL - st.BaselineUPnL
	rec.ExpectedEquity = cashflowJournalExpectedEquity(st.BaselineAccountValue, st.BaselineUPnL, settled, res.CurrentUPnL)
	rec.Drift = res.AccountValue - rec.ExpectedEquity
	rec.Usable = res.FillsFetched && !res.Capped && !st.Incomplete
	return rec
}

func logTopStepCashflowJournalShadow(driftResults []sharedWalletDriftResult, key SharedWalletKey, rec *cashflowJournalReconcile) {
	if rec == nil {
		return
	}
	splitNote := "n/a"
	for i := range driftResults {
		if driftResults[i].Key == key {
			d := driftResults[i]
			splitNote = fmt.Sprintf("raw $%+.2f / drift $%+.2f", d.Balance-d.MemberSum, d.Drift)
			break
		}
	}
	state := "shadow-usable"
	switch {
	case rec.Incomplete:
		state = "shadow-incomplete (unclassified fill — would fail closed)"
	case !rec.Usable:
		state = "shadow-pending (no usable reading this cycle)"
	}
	fmt.Printf("[topstep-cashflow-journal] %s: expected_equity $%.2f vs equity $%.2f → journal_drift $%+.4f (settled Σ $%+.2f, ΔuPnL $%+.2f); display %s; %s — SHADOW, no alarm\n",
		sharedWalletKeyLabel(key), rec.ExpectedEquity, rec.AccountValue, rec.Drift, rec.SettledSum, rec.DeltaUPnL, splitNote, state)
}
