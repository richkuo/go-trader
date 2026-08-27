package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

var okxFetchBillsScript = "shared_scripts/fetch_okx_bills.py"

func defaultOKXAccountBillsFetcher(sinceMs int64) ([]okxBillRecord, bool, error) {
	result, stderr, err := RunOKXFetchBills(okxFetchBillsScript, sinceMs)
	if stderr != "" {
		fmt.Fprintf(os.Stderr, "[okx-cashflow-journal] fetch_bills stderr: %s\n", stderr)
	}
	if err != nil {
		return nil, false, err
	}
	return result.Bills, result.Capped, nil
}


const okxJournalSettlementCcy = "USDT"

type okxBillRecord struct {
	BillID  string  `json:"bill_id"`
	TimeMs  int64   `json:"ts_ms"`
	Ccy     string  `json:"ccy"`
	Type    string  `json:"type"`
	SubType string  `json:"sub_type"`
	BalChg  float64 `json:"bal_chg"`
	Pnl     float64 `json:"pnl"`
	Fee     float64 `json:"fee"`
	InstID  string  `json:"inst_id"`
	TradeID string  `json:"trade_id"`
}

var okxBillKindByType = map[string]string{
	"1":  "transfer",
	"2":  "trade",
	"3":  "delivery",
	"4":  "auto_margin_buy",
	"5":  "liquidation",
	"6":  "margin_transfer",
	"7":  "interest_deduct",
	"8":  "funding_fee",
	"9":  "adl",
	"10": "clawback",
	"11": "system_token_conv",
	"12": "strategy_transfer",
	"13": "ddh",
	"14": "settlement",
	"22": "repay_forced",
}

func okxBillSettledDelta(b okxBillRecord) (delta float64, kind string, known bool) {
	ccy := strings.ToUpper(strings.TrimSpace(b.Ccy))
	if ccy != "" && ccy != okxJournalSettlementCcy {
		return 0, "nonsettle_" + strings.ToLower(ccy), false
	}
	if mapped, ok := okxBillKindByType[strings.TrimSpace(b.Type)]; ok {
		return b.BalChg, mapped, true
	}
	return b.BalChg, "type_" + strings.TrimSpace(b.Type), false
}

func okxBillDedupID(b okxBillRecord) string {
	if id := strings.TrimSpace(b.BillID); id != "" && id != "0" {
		return "okxbill:" + id
	}
	return fmt.Sprintf("okxbill:%s:%d:%s", strings.TrimSpace(b.Type), b.TimeMs, strings.TrimSpace(b.TradeID))
}

var fetchOKXAccountBills = func(sinceMs int64) (bills []okxBillRecord, capped bool, err error) {
	return defaultOKXAccountBillsFetcher(sinceMs)
}

type okxCashflowJournalFetchResult struct {
	Key          SharedWalletKey
	State        CashflowJournalState
	StateFound   bool
	AccountValue float64
	CurrentUPnL  float64
	Bills        []okxBillRecord
	BillsFetched bool
	Capped       bool
}

func fetchOKXCashflowJournalEvents(sdb *StateDB, key SharedWalletKey, accountValue, currentUPnL float64, now time.Time) okxCashflowJournalFetchResult {
	res := okxCashflowJournalFetchResult{Key: key, AccountValue: accountValue, CurrentUPnL: currentUPnL}
	if sdb == nil || key.Platform != "okx" || key.Account == "" {
		return res
	}
	st, found, err := sdb.GetCashflowJournalState(key.Platform, key.Account)
	if err != nil {
		fmt.Printf("[WARN] okx-cashflow-journal %s: state load failed: %v — skipping ingestion this cycle\n", sharedWalletKeyLabel(key), err)
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
			fmt.Printf("[WARN] okx-cashflow-journal %s: baseline init failed: %v\n", sharedWalletKeyLabel(key), err)
			return res
		}
		fmt.Printf("[okx-cashflow-journal] %s: baseline anchored at eq $%.2f (uPnL $%+.2f) and bills cursor at %s (no historical replay)\n",
			sharedWalletKeyLabel(key), accountValue, currentUPnL, now.UTC().Format(time.RFC3339))
		res.State = st
		res.StateFound = true
		return res
	}
	res.State = st
	res.StateFound = true

	bills, capped, err := fetchOKXAccountBills(st.FillsSinceMs)
	if err != nil {
		fmt.Printf("[WARN] okx-cashflow-journal %s: account-bills fetch failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
		return res
	}
	res.Bills = bills
	res.BillsFetched = true
	res.Capped = capped
	return res
}

func ingestOKXCashflowJournalEvents(sdb *StateDB, res okxCashflowJournalFetchResult, cutoffMs int64) CashflowJournalState {
	st := res.State
	if sdb == nil || !res.StateFound || !res.BillsFetched {
		return st
	}
	key := res.Key

	maxTime := st.FillsSinceMs - 1
	failedAt := int64(-1)
	bills := append([]okxBillRecord(nil), res.Bills...)
	sort.SliceStable(bills, func(i, j int) bool { return bills[i].TimeMs < bills[j].TimeMs })
	for _, b := range bills {
		if b.TimeMs < st.FillsSinceMs {
			continue
		}
		if b.TimeMs > cutoffMs {
			continue
		}
		delta, kind, known := okxBillSettledDelta(b)
		if !known {
			st.Incomplete = true
			fmt.Printf("[WARN] okx-cashflow-journal %s: unclassified bill type=%q subType=%q ccy=%q (billId %s) — recorded kind %q, journal marked incomplete\n",
				sharedWalletKeyLabel(key), b.Type, b.SubType, b.Ccy, b.BillID, kind)
		}
		coin := strings.ToUpper(strings.TrimSpace(b.InstID))
		if err := sdb.InsertCashflowJournalEntry(key.Platform, key.Account, b.TimeMs, kind, delta, coin, b.Pnl, b.Fee, okxBillDedupID(b)); err != nil {
			fmt.Printf("[WARN] okx-cashflow-journal %s: bill insert failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
			failedAt = b.TimeMs
			break
		}
		if b.TimeMs > maxTime {
			maxTime = b.TimeMs
		}
	}
	advanceMax := maxTime
	if res.Capped {
		advanceMax = maxTime - 1
	}
	st.FillsSinceMs = advanceCashflowCursor(st.FillsSinceMs, advanceMax, failedAt)

	if st != res.State {
		if err := sdb.UpsertCashflowJournalState(key.Platform, key.Account, st); err != nil {
			fmt.Printf("[WARN] okx-cashflow-journal %s: cursor advance failed: %v\n", sharedWalletKeyLabel(key), err)
		}
	}
	return st
}

func reconcileOKXCashflowJournal(sdb *StateDB, key SharedWalletKey, accountValue, currentUPnL float64, snapshotAt time.Time) *cashflowJournalReconcile {
	if sdb == nil || key.Platform != "okx" || key.Account == "" {
		return nil
	}
	res := fetchOKXCashflowJournalEvents(sdb, key, accountValue, currentUPnL, snapshotAt)
	if !res.StateFound {
		return nil
	}
	st := ingestOKXCashflowJournalEvents(sdb, res, snapshotAt.UnixMilli())
	rec := &cashflowJournalReconcile{Key: key, AccountValue: res.AccountValue, Incomplete: st.Incomplete}
	if !st.BaselineSet {
		return rec
	}
	settled, err := sdb.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		fmt.Printf("[WARN] okx-cashflow-journal %s: settled-sum read failed: %v\n", sharedWalletKeyLabel(key), err)
		return rec
	}
	rec.SettledSum = settled
	rec.DeltaUPnL = res.CurrentUPnL - st.BaselineUPnL
	rec.ExpectedEquity = cashflowJournalExpectedEquity(st.BaselineAccountValue, st.BaselineUPnL, settled, res.CurrentUPnL)
	rec.Drift = res.AccountValue - rec.ExpectedEquity
	rec.Usable = res.BillsFetched && !res.Capped && !st.Incomplete
	return rec
}

func logOKXCashflowJournalShadow(driftResults []sharedWalletDriftResult, key SharedWalletKey, rec *cashflowJournalReconcile) {
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
		state = "shadow-incomplete (unclassified bill — would fail closed)"
	case !rec.Usable:
		state = "shadow-pending (no usable reading this cycle)"
	}
	fmt.Printf("[okx-cashflow-journal] %s: expected_equity $%.2f vs eq $%.2f → journal_drift $%+.4f (settled Σ $%+.2f, ΔuPnL $%+.2f); capital-weight %s; %s — SHADOW, alarm unchanged\n",
		sharedWalletKeyLabel(key), rec.ExpectedEquity, rec.AccountValue, rec.Drift, rec.SettledSum, rec.DeltaUPnL, splitNote, state)
}
