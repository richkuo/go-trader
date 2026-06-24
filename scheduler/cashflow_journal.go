package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// #1100: exchange-sourced equity journal for shared-wallet TOTAL reconciliation
// (Hyperliquid only). Internal trade rows remain the source of truth for
// per-strategy ATTRIBUTION; the wallet-level drift alarm keys off journal equity.
//
// Equity decomposition (HL accountValue = settled cash + unrealized PnL):
//
//	expected = baseline_accountValue
//	         + Σ(settled-cash deltas since baseline)
//	         + (current_uPnL − baseline_uPnL)
//
// A fill's settled-cash delta is closedPnl (GROSS) minus fee (#698). OKX and
// other platforms keep the #918/#954 paths unchanged.

// CashflowJournalState is one wallet's per-stream journal cursors plus the
// adoption baseline. Incomplete latches true once an unmapped event kind is
// seen — alarms fail closed to the trade-ledger path until repaired.
type CashflowJournalState struct {
	FillsSinceMs         int64
	FundingSinceMs       int64
	TransfersSinceMs     int64
	BaselineAccountValue float64
	BaselineUPnL         float64
	BaselineSet          bool
	Incomplete           bool
}

// GetCashflowJournalState loads one wallet's journal state; found=false when the
// wallet has never been ingested.
func (sdb *StateDB) GetCashflowJournalState(platform, account string) (CashflowJournalState, bool, error) {
	var st CashflowJournalState
	if sdb == nil || sdb.db == nil {
		return st, false, fmt.Errorf("state db unavailable")
	}
	var baselineSet, incomplete int
	err := sdb.db.QueryRow(
		`SELECT fills_since_ms, funding_since_ms, transfers_since_ms,
		        baseline_account_value, baseline_upnl, baseline_set, incomplete
		 FROM cashflow_journal_state WHERE platform = ? AND account = ?`,
		platform, account).Scan(&st.FillsSinceMs, &st.FundingSinceMs, &st.TransfersSinceMs,
		&st.BaselineAccountValue, &st.BaselineUPnL, &baselineSet, &incomplete)
	if err == sql.ErrNoRows {
		return st, false, nil
	}
	if err != nil {
		return st, false, fmt.Errorf("load cashflow journal state: %w", err)
	}
	st.BaselineSet = baselineSet != 0
	st.Incomplete = incomplete != 0
	return st, true, nil
}

// UpsertCashflowJournalState writes one wallet's journal state row.
func (sdb *StateDB) UpsertCashflowJournalState(platform, account string, st CashflowJournalState) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	baselineSet := 0
	if st.BaselineSet {
		baselineSet = 1
	}
	incomplete := 0
	if st.Incomplete {
		incomplete = 1
	}
	_, err := sdb.db.Exec(
		`INSERT INTO cashflow_journal_state
		   (platform, account, fills_since_ms, funding_since_ms, transfers_since_ms,
		    baseline_account_value, baseline_upnl, baseline_set, incomplete)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(platform, account) DO UPDATE SET
		   fills_since_ms = excluded.fills_since_ms,
		   funding_since_ms = excluded.funding_since_ms,
		   transfers_since_ms = excluded.transfers_since_ms,
		   baseline_account_value = excluded.baseline_account_value,
		   baseline_upnl = excluded.baseline_upnl,
		   baseline_set = excluded.baseline_set,
		   incomplete = excluded.incomplete`,
		platform, account, st.FillsSinceMs, st.FundingSinceMs, st.TransfersSinceMs,
		st.BaselineAccountValue, st.BaselineUPnL, baselineSet, incomplete)
	if err != nil {
		return fmt.Errorf("upsert cashflow journal state: %w", err)
	}
	return nil
}

// InsertCashflowJournalEntry appends one settled-cash event; a duplicate
// dedup_id is silently ignored (cursor-overlap re-reads).
func (sdb *StateDB) InsertCashflowJournalEntry(platform, account string, timeMs int64, kind string, amountUSD float64, coin string, closedPnlGross, feeUSD float64, dedupID string) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	_, err := sdb.db.Exec(
		`INSERT OR IGNORE INTO cashflow_journal
		   (platform, account, time_ms, kind, amount_usd, coin, closed_pnl_gross, fee_usd, dedup_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		platform, account, timeMs, kind, amountUSD, coin, closedPnlGross, feeUSD, dedupID)
	if err != nil {
		return fmt.Errorf("insert cashflow journal entry: %w", err)
	}
	return nil
}

// SumCashflowJournal returns the signed total of one wallet's settled-cash
// deltas since adoption (Σ amount_usd across every journal row).
func (sdb *StateDB) SumCashflowJournal(platform, account string) (float64, error) {
	if sdb == nil || sdb.db == nil {
		return 0, fmt.Errorf("state db unavailable")
	}
	var sum sql.NullFloat64
	err := sdb.db.QueryRow(
		`SELECT SUM(amount_usd) FROM cashflow_journal WHERE platform = ? AND account = ?`,
		platform, account).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("sum cashflow journal: %w", err)
	}
	return sum.Float64, nil
}

func cashflowFillSettledDelta(closedPnlGross, fee float64) float64 {
	return closedPnlGross - fee
}

func cashflowJournalExpectedEquity(baselineAccountValue, baselineUPnL, settledDeltaSum, currentUPnL float64) float64 {
	return baselineAccountValue + settledDeltaSum + (currentUPnL - baselineUPnL)
}

func advanceCashflowCursor(current, maxProcessed, failedAt int64) int64 {
	next := maxProcessed + 1
	if failedAt >= 0 && failedAt < next {
		next = failedAt
	}
	if next > current {
		return next
	}
	return current
}

func cashflowFillDedupID(f hlFillRecord) string {
	if tid := strings.TrimSpace(f.Tid.String()); tid != "" && tid != "0" {
		return "fill:tid:" + tid
	}
	return fmt.Sprintf("fill:%d:%s:%s", f.Time, f.Hash, strings.ToUpper(strings.TrimSpace(f.Coin)))
}

func cashflowFundingDedupID(ev hlLedgerEvent) string {
	return fmt.Sprintf("funding:%d:%s:%s", ev.Time, ev.Hash, ev.Delta.Coin)
}

func cashflowTransferDedupID(ev hlLedgerEvent) string {
	return fmt.Sprintf("%s:%d:%s", ev.Delta.Type, ev.Time, ev.Hash)
}

type cashflowJournalFetchResult struct {
	Key              SharedWalletKey
	State            CashflowJournalState
	StateFound       bool
	PriorStateExists bool // true when an existing row was loaded (not first anchor)
	AccountValue     float64
	CurrentUPnL      float64
	Fills            []hlFillRecord
	Funding          []hlLedgerEvent
	Transfers        []hlLedgerEvent
	FillsFetched     bool
	FundingFetched   bool
	TransfersFetched bool
}

// fetchCashflowJournalEvents reads per-stream cursors and pulls fills + funding +
// non-funding ledger events since each. Runs OUTSIDE the state lock. First
// contact anchors the baseline to the supplied accountValue / uPnL snapshot and
// the cursors to now — pre-adoption movement belongs to the baseline.
func fetchCashflowJournalEvents(sdb *StateDB, key SharedWalletKey, accountValue, currentUPnL float64, now time.Time) cashflowJournalFetchResult {
	res := cashflowJournalFetchResult{Key: key, AccountValue: accountValue, CurrentUPnL: currentUPnL}
	if sdb == nil || key.Platform != "hyperliquid" || key.Account == "" {
		return res
	}
	st, found, err := sdb.GetCashflowJournalState(key.Platform, key.Account)
	if err != nil {
		fmt.Printf("[WARN] cashflow-journal %s: state load failed: %v — skipping ingestion this cycle\n", sharedWalletKeyLabel(key), err)
		return res
	}
	if !found {
		nowMs := now.UnixMilli()
		st = CashflowJournalState{
			FillsSinceMs:         nowMs,
			FundingSinceMs:       nowMs,
			TransfersSinceMs:     nowMs,
			BaselineAccountValue: accountValue,
			BaselineUPnL:         currentUPnL,
			BaselineSet:          true,
		}
		if err := sdb.UpsertCashflowJournalState(key.Platform, key.Account, st); err != nil {
			fmt.Printf("[WARN] cashflow-journal %s: baseline init failed: %v\n", sharedWalletKeyLabel(key), err)
			return res
		}
		fmt.Printf("[cashflow-journal] %s: baseline anchored at accountValue $%.2f (uPnL $%+.2f) and cursors at %s (no historical replay)\n",
			sharedWalletKeyLabel(key), accountValue, currentUPnL, now.UTC().Format(time.RFC3339))
		res.State = st
		res.StateFound = true
		return res
	}
	res.State = st
	res.StateFound = true
	res.PriorStateExists = true

	if fills, err := fetchHyperliquidUserFillsByTime(key.Account, st.FillsSinceMs); err != nil {
		fmt.Printf("[WARN] cashflow-journal %s: userFills fetch failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
	} else {
		res.Fills = fills
		res.FillsFetched = true
	}
	if funding, err := fetchHyperliquidUserFunding(key.Account, st.FundingSinceMs); err != nil {
		fmt.Printf("[WARN] cashflow-journal %s: userFunding fetch failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
	} else {
		res.Funding = funding
		res.FundingFetched = true
	}
	if transfers, err := fetchHyperliquidLedgerUpdates(key.Account, st.TransfersSinceMs); err != nil {
		fmt.Printf("[WARN] cashflow-journal %s: userNonFundingLedgerUpdates fetch failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
	} else {
		res.Transfers = transfers
		res.TransfersFetched = true
	}
	return res
}

// ingestCashflowJournalEvents books fetched events and advances cursors. DB-only.
func ingestCashflowJournalEvents(sdb *StateDB, res cashflowJournalFetchResult) CashflowJournalState {
	st := res.State
	if sdb == nil || !res.StateFound {
		return st
	}
	key := res.Key

	if res.FillsFetched {
		maxTime := st.FillsSinceMs - 1
		failedAt := int64(-1)
		fills := append([]hlFillRecord(nil), res.Fills...)
		sort.SliceStable(fills, func(i, j int) bool { return fills[i].Time < fills[j].Time })
		for _, f := range fills {
			if f.Time < st.FillsSinceMs {
				continue
			}
			coin := strings.ToUpper(strings.TrimSpace(f.Coin))
			closedPnl := parseHLFloat(f.ClosedPnl)
			fee := parseHLFloat(f.Fee)
			delta := cashflowFillSettledDelta(closedPnl, fee)
			if err := sdb.InsertCashflowJournalEntry(key.Platform, key.Account, f.Time, "fill", delta, coin, closedPnl, fee, cashflowFillDedupID(f)); err != nil {
				fmt.Printf("[WARN] cashflow-journal %s: fill insert failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
				failedAt = f.Time
				break
			}
			if f.Time > maxTime {
				maxTime = f.Time
			}
		}
		st.FillsSinceMs = advanceCashflowCursor(st.FillsSinceMs, maxTime, failedAt)
	}

	if res.FundingFetched {
		maxTime := st.FundingSinceMs - 1
		failedAt := int64(-1)
		events := append([]hlLedgerEvent(nil), res.Funding...)
		sort.SliceStable(events, func(i, j int) bool { return events[i].Time < events[j].Time })
		for _, ev := range events {
			if ev.Time < st.FundingSinceMs {
				continue
			}
			amount := parseHLFloat(ev.Delta.USDC)
			coin := strings.ToUpper(strings.TrimSpace(ev.Delta.Coin))
			if err := sdb.InsertCashflowJournalEntry(key.Platform, key.Account, ev.Time, "funding", amount, coin, 0, 0, cashflowFundingDedupID(ev)); err != nil {
				fmt.Printf("[WARN] cashflow-journal %s: funding insert failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
				failedAt = ev.Time
				break
			}
			if ev.Time > maxTime {
				maxTime = ev.Time
			}
		}
		st.FundingSinceMs = advanceCashflowCursor(st.FundingSinceMs, maxTime, failedAt)
	}

	if res.TransfersFetched {
		maxTime := st.TransfersSinceMs - 1
		failedAt := int64(-1)
		events := append([]hlLedgerEvent(nil), res.Transfers...)
		sort.SliceStable(events, func(i, j int) bool { return events[i].Time < events[j].Time })
		for _, ev := range events {
			if ev.Time < st.TransfersSinceMs {
				continue
			}
			amount, known := signedPerpFlowUSD(ev.Delta, key.Account)
			if !known {
				st.Incomplete = true
				fmt.Printf("[WARN] cashflow-journal %s: unmapped ledger delta type %q (hash %s) — recorded with $0 effect, journal marked incomplete\n",
					sharedWalletKeyLabel(key), ev.Delta.Type, ev.Hash)
			}
			if err := sdb.InsertCashflowJournalEntry(key.Platform, key.Account, ev.Time, ev.Delta.Type, amount, "", 0, 0, cashflowTransferDedupID(ev)); err != nil {
				fmt.Printf("[WARN] cashflow-journal %s: transfer insert failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
				failedAt = ev.Time
				break
			}
			if ev.Time > maxTime {
				maxTime = ev.Time
			}
		}
		st.TransfersSinceMs = advanceCashflowCursor(st.TransfersSinceMs, maxTime, failedAt)
	}

	if st != res.State {
		if err := sdb.UpsertCashflowJournalState(key.Platform, key.Account, st); err != nil {
			fmt.Printf("[WARN] cashflow-journal %s: cursor advance failed: %v\n", sharedWalletKeyLabel(key), err)
		}
	}
	return st
}

// cashflowJournalUsable reports whether journal-based drift alarms are safe this
// cycle. First-anchor cycles (no prior row) are usable with zero post-baseline
// events. Established wallets require all three streams to have fetched cleanly.
func cashflowJournalUsable(fetch cashflowJournalFetchResult, st CashflowJournalState) bool {
	if !fetch.StateFound || !st.BaselineSet || st.Incomplete {
		return false
	}
	if !fetch.PriorStateExists {
		return true
	}
	return fetch.FillsFetched && fetch.FundingFetched && fetch.TransfersFetched
}

// computeCashflowJournalDrift returns the journal expected equity and alarm drift.
func computeCashflowJournalDrift(sdb *StateDB, fetch cashflowJournalFetchResult, st CashflowJournalState) (expected, drift float64, ok bool) {
	if sdb == nil || !cashflowJournalUsable(fetch, st) {
		return 0, 0, false
	}
	settled, err := sdb.SumCashflowJournal(fetch.Key.Platform, fetch.Key.Account)
	if err != nil {
		fmt.Printf("[WARN] cashflow-journal %s: settled-sum read failed: %v\n", sharedWalletKeyLabel(fetch.Key), err)
		return 0, 0, false
	}
	expected = cashflowJournalExpectedEquity(st.BaselineAccountValue, st.BaselineUPnL, settled, fetch.CurrentUPnL)
	drift = fetch.AccountValue - expected
	return expected, drift, true
}

// applyHyperliquidCashflowJournalDrift ingests one HL wallet's journal events and
// patches the matching sharedWalletDriftResult to use journal equity for alarms
// when usable; otherwise leaves the trade-ledger fallback drift in place.
func applyHyperliquidCashflowJournalDrift(results *[]sharedWalletDriftResult, sdb *StateDB, fetch cashflowJournalFetchResult) {
	if sdb == nil || fetch.Key.Platform != "hyperliquid" || !fetch.StateFound {
		return
	}
	st := ingestCashflowJournalEvents(sdb, fetch)
	expected, journalDrift, ok := computeCashflowJournalDrift(sdb, fetch, st)
	for i := range *results {
		if (*results)[i].Key != fetch.Key {
			continue
		}
		r := &(*results)[i]
		if ok {
			r.Drift = journalDrift
			r.ExpectedEquity = expected
			r.DriftBasis = sharedWalletDriftBasisCashflowJournal
			fmt.Printf("[cashflow-journal] %s: expected_equity $%.2f vs accountValue $%.2f → journal_drift $%+.4f (attribution gap $%+.2f, ledger-fallback $%+.2f)\n",
				sharedWalletKeyLabel(fetch.Key), expected, fetch.AccountValue, journalDrift, r.AttributionGap, r.LedgerFallbackDrift)
		} else {
			reason := "journal unusable"
			if st.Incomplete {
				reason = "journal incomplete (unmapped event)"
			} else if fetch.PriorStateExists && (!fetch.FillsFetched || !fetch.FundingFetched || !fetch.TransfersFetched) {
				reason = "fetch miss this cycle"
			}
			fmt.Printf("[WARN] cashflow-journal %s: %s — drift alarm uses trade-ledger fallback $%+.2f\n",
				sharedWalletKeyLabel(fetch.Key), reason, r.LedgerFallbackDrift)
		}
		return
	}
}

// sumHLAccountUPnL totals exchange-reported unrealized PnL across open positions.
func sumHLAccountUPnL(positions []HLPosition) float64 {
	sum := 0.0
	for _, p := range positions {
		sum += p.UnrealizedPnL
	}
	return sum
}
