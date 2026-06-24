package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// #1100: exchange-sourced equity journal for shared-wallet TOTAL reconciliation.
//
// Today the shared-wallet drift alarm reconstructs each account's equity from
// the bot's OWN trade ledger plus exchange unrealized PnL, then compares that
// derived value to the exchange's accountValue:
//
//	member_value_i = initial_capital_i + Σ tradeLedgerDelta_i + owned_uPnL_i
//	raw_drift      = accountValue − Σ member_value_i − Σ wallet_transfers
//
// Every write path, fallback, or model-only cleanup that mis-prices a trade row
// (modeled fee when userFills missed, mark-priced force-close, kill-switch
// residual) shows up as residual drift, because the TOTAL still depends on
// internal rows for realized PnL, fees, and fill prices.
//
// This journal inverts that: it reconstructs the wallet's SETTLED-CASH balance
// from the exchange's own cash-flow events — fills, funding, transfers — pulled
// incrementally per stream with durable cursors and per-event dedup. The
// internal trade rows stay the source of truth for per-strategy ATTRIBUTION
// only; the TOTAL comes from the exchange.
//
// Equity decomposition (HL accountValue = settled cash + unrealized PnL):
//
//	accountValue_t = cash_t + uPnL_t
//	             = baseline_accountValue + (cash_t − cash_0) + (uPnL_t − uPnL_0)
//
// so the journal's reconstruction is:
//
//	expected = baseline_accountValue
//	         + Σ(settled-cash deltas since baseline)   // fills + funding + transfers
//	         + (current_uPnL − baseline_uPnL)
//
// A fill's settled-cash delta is realized PnL (GROSS) minus the fee actually
// charged: opening fills settle −fee (closedPnl=0), closing fills settle
// closedPnl−fee. closedPnl is gross of fees (#698) so the fee is subtracted
// exactly once here; closed_pnl_gross is retained for attribution and is NEVER
// summed into equity on its own.
//
// SCOPE: Hyperliquid shared wallets, SHADOW MODE. The journal is computed
// beside the live trade-ledger drift path and the delta is logged for
// validation. It does NOT yet drive any alarm or member display — that switch,
// plus OKX/TopStep extension and baseline-offset retirement, are later phases
// of #1100. Nothing here mutates StrategyState, so the whole flow runs OUTSIDE
// the state lock (DB writes are serialized by SQLite).

// CashflowJournalState is one wallet's per-stream journal cursors plus the
// adoption baseline. Incomplete latches true once an unmapped event kind is
// seen so a future alarm switch can fail closed to the trade-ledger path.
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
// dedup_id is silently ignored (cursor-overlap re-reads). A non-nil error is a
// genuine persistence failure — the caller halts the cursor at that event so a
// crash can never strand an un-booked event behind an advanced watermark.
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

// cashflowFillSettledDelta is one fill's SIGNED effect on settled cash: realized
// PnL (GROSS) minus the fee actually charged. closedPnl is gross of fees (#698),
// so the fee — which may be a negative maker rebate — is subtracted exactly once
// here. Opening fills (closedPnl=0) settle −fee; closing fills settle
// closedPnl−fee. The gross value is retained separately for attribution and is
// never summed into equity on its own.
func cashflowFillSettledDelta(closedPnlGross, fee float64) float64 {
	return closedPnlGross - fee
}

// hlFillIsSpot reports whether a userFills coin is a SPOT market identifier
// rather than a perps asset. HL spot fills settle against a separate spot USDC
// balance and do NOT move the perps marginSummary.accountValue this journal
// reconciles, so they must contribute $0 to the perps settled-cash sum — the
// same spot exclusion signedPerpFlowUSD already applies on the transfer stream.
// HL spot coins are an index form ("@107") or a named pair ("PURR/USDC"); perps
// assets ("BTC", "kPEPE", "HYPE") never contain "/" or start with "@".
func hlFillIsSpot(coin string) bool {
	c := strings.TrimSpace(coin)
	return strings.HasPrefix(c, "@") || strings.Contains(c, "/")
}

// cashflowJournalExpectedEquity reconstructs the wallet's current accountValue
// from the adoption baseline, the settled-cash deltas booked since, and the
// change in exchange unrealized PnL since baseline. Pure — unit-tested.
func cashflowJournalExpectedEquity(baselineAccountValue, baselineUPnL, settledDeltaSum, currentUPnL float64) float64 {
	return baselineAccountValue + settledDeltaSum + (currentUPnL - baselineUPnL)
}

// advanceCashflowCursor returns the next watermark for one stream: one past the
// highest processed event time, except never AT or AFTER a failed event's
// timestamp (so a same-ms sibling that persisted can't strand the failed event
// behind the cursor). maxProcessed starts at current−1; failedAt is −1 when no
// event failed. Mirrors the wallet_ledger watermark discipline.
func advanceCashflowCursor(current, maxProcessed, failedAt int64) int64 {
	next := maxProcessed + 1
	if failedAt >= 0 && failedAt < next {
		next = failedAt // re-fetch from the failed event; dedup absorbs same-ms re-reads
	}
	if next > current {
		return next
	}
	return current
}

// cashflowFillDedupID is a fill's stable journal identity. A single OID can
// fragment into many partial fills, so the per-fill trade id (tid) is the
// canonical key; the L1 hash + coin + time disambiguate the rare missing-tid
// case so two genuine fills never collide.
func cashflowFillDedupID(f hlFillRecord) string {
	if tid := strings.TrimSpace(f.Tid.String()); tid != "" && tid != "0" {
		return "fill:tid:" + tid
	}
	return fmt.Sprintf("fill:%d:%s:%s", f.Time, f.Hash, strings.ToUpper(strings.TrimSpace(f.Coin)))
}

// cashflowFundingDedupID / cashflowTransferDedupID key the journal's funding and
// non-funding rows. The journal namespaces are independent of the #954
// attribution dedup keys (which live in the trades / wallet_transfers tables).
func cashflowFundingDedupID(ev hlLedgerEvent) string {
	return fmt.Sprintf("funding:%d:%s:%s", ev.Time, ev.Hash, ev.Delta.Coin)
}

func cashflowTransferDedupID(ev hlLedgerEvent) string {
	return fmt.Sprintf("%s:%d:%s", ev.Delta.Type, ev.Time, ev.Hash)
}

// cashflowJournalFetchResult carries one wallet's raw cash-flow events plus the
// coherent accountValue / uPnL snapshot from the no-lock fetch phase to the
// no-lock ingest+compare phase.
type cashflowJournalFetchResult struct {
	Key              SharedWalletKey
	State            CashflowJournalState
	StateFound       bool
	AccountValue     float64 // exchange accountValue (equity incl. uPnL) this cycle
	CurrentUPnL      float64 // Σ exchange unrealized PnL across the account this cycle
	Fills            []hlFillRecord
	Funding          []hlLedgerEvent
	Transfers        []hlLedgerEvent
	FillsFetched     bool
	FundingFetched   bool
	TransfersFetched bool
}

// fetchCashflowJournalEvents reads the wallet's per-stream cursors and pulls
// fills + funding + non-funding ledger events since each (three HTTP POSTs).
// Runs OUTSIDE the state lock. First contact anchors the baseline to the
// supplied accountValue / uPnL snapshot and the cursors to now, fetching no
// history — pre-adoption movement belongs to the baseline, not the journal.
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

// ingestCashflowJournalEvents books the fetched events into cashflow_journal and
// advances each stream's cursor. DB-only (no StrategyState mutation), so it runs
// OUTSIDE the state lock. Returns the post-ingest state (cursors advanced,
// incomplete latched if an unmapped kind was seen). Every insert path halts its
// cursor on persistence failure so a watermark never advances past an un-booked
// event.
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
				continue // cursor-overlap boundary; dedup also covers this
			}
			coin := strings.ToUpper(strings.TrimSpace(f.Coin))
			closedPnl := parseHLFloat(f.ClosedPnl)
			fee := parseHLFloat(f.Fee)
			// A spot fill settles against the separate spot USDC balance, NOT the
			// perps marginSummary.accountValue this journal reconciles, so it
			// contributes $0 to the perps settled-cash sum — mirroring the spot
			// exclusion signedPerpFlowUSD already applies on the transfer stream.
			// The row is still booked (kind "fill_spot", amount 0) so it stays
			// visible + deduped and the cursor still advances past it; closedPnl
			// and fee are retained as attribution metadata only, never summed.
			kind := "fill"
			delta := cashflowFillSettledDelta(closedPnl, fee)
			if hlFillIsSpot(coin) {
				kind = "fill_spot"
				delta = 0
			}
			if err := sdb.InsertCashflowJournalEntry(key.Platform, key.Account, f.Time, kind, delta, coin, closedPnl, fee, cashflowFillDedupID(f)); err != nil {
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
			// Full funding amount moves the balance regardless of which member
			// (if any) owns the coin — the journal is the TOTAL, with no
			// attribution split. Signed: + = balance increased.
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
				// Latch incomplete: an unmapped kind means the journal cannot
				// claim an exact total, so a future alarm switch must fail
				// closed to the trade-ledger path. The $0 row keeps the event
				// visible and the running drift surfaces it.
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

// reconcileCashflowJournalShadow runs the full journal flow for one HL shared
// wallet OUTSIDE the state lock: fetch the cash-flow events, book them, then log
// the journal's reconstructed equity beside the live trade-ledger drift for
// validation. SHADOW MODE — it drives no alarm and mutates no display value.
// ledgerDrift (may be nil) is the matching trade-ledger drift result this cycle,
// logged alongside so an operator can compare the two reconciliation bases.
func reconcileCashflowJournalShadow(sdb *StateDB, key SharedWalletKey, accountValue, currentUPnL float64, ledgerDrift *sharedWalletDriftResult, now time.Time) {
	if sdb == nil || key.Platform != "hyperliquid" || key.Account == "" {
		return
	}
	res := fetchCashflowJournalEvents(sdb, key, accountValue, currentUPnL, now)
	if !res.StateFound {
		return // baseline just anchored (logged there) or fetch/load failed
	}
	st := ingestCashflowJournalEvents(sdb, res)
	if !st.BaselineSet {
		return // defensive: never compute against an un-anchored baseline
	}
	settled, err := sdb.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		fmt.Printf("[WARN] cashflow-journal %s: settled-sum read failed: %v\n", sharedWalletKeyLabel(key), err)
		return
	}
	expected := cashflowJournalExpectedEquity(st.BaselineAccountValue, st.BaselineUPnL, settled, res.CurrentUPnL)
	journalDrift := res.AccountValue - expected

	incompleteNote := ""
	if st.Incomplete {
		incompleteNote = " [INCOMPLETE: unmapped event kind seen — would fail closed once promoted]"
	}
	ledgerNote := "n/a"
	if ledgerDrift != nil {
		// The trade-ledger result carries the post-baseline Drift and the raw
		// reconciliation inputs; raw diff = balance − Σ member values.
		ledgerNote = fmt.Sprintf("raw $%+.2f / post-baseline $%+.2f", ledgerDrift.Balance-ledgerDrift.MemberSum, ledgerDrift.Drift)
	}
	fmt.Printf("[cashflow-journal] %s SHADOW: expected_equity $%.2f vs accountValue $%.2f → journal_drift $%+.4f (settled Σ $%+.2f since baseline $%.2f, ΔuPnL $%+.2f); trade-ledger drift %s%s\n",
		sharedWalletKeyLabel(key), expected, res.AccountValue, journalDrift,
		settled, st.BaselineAccountValue, res.CurrentUPnL-st.BaselineUPnL, ledgerNote, incompleteNote)
}

// sumHLAccountUPnL totals the exchange-reported unrealized PnL across an
// account's open positions — the uPnL component of accountValue the journal
// equity equation needs.
func sumHLAccountUPnL(positions []HLPosition) float64 {
	sum := 0.0
	for _, p := range positions {
		sum += p.UnrealizedPnL
	}
	return sum
}
