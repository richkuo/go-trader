package main

import (
	"database/sql"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const driftBasisJournal = "journal"

const journalDriftStreakKeySuffix = ":journal"

type CashflowJournalState struct {
	FillsSinceMs         int64
	FundingSinceMs       int64
	TransfersSinceMs     int64
	BaselineAccountValue float64
	BaselineUPnL         float64
	BaselineSet          bool
	Incomplete           bool
}

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

type CashflowJournalWalletStatus struct {
	Platform             string  `json:"platform"`
	Account              string  `json:"account"`
	BaselineSet          bool    `json:"baseline_set"`
	Incomplete           bool    `json:"incomplete"`
	BaselineAccountValue float64 `json:"baseline_account_value"`
	SettledSum           float64 `json:"settled_sum"`
	EntryCount           int     `json:"entry_count"`
	LastEventMs          int64   `json:"last_event_ms"`
	ShadowOnly           bool    `json:"shadow_only"`
	LiveBasisEligible    bool    `json:"live_basis_eligible"`
	Basis                string  `json:"basis,omitempty"`
}

func (sdb *StateDB) ListCashflowJournalWallets() ([]CashflowJournalWalletStatus, error) {
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	rows, err := sdb.db.Query(
		`SELECT s.platform, s.account, s.baseline_set, s.incomplete, s.baseline_account_value,
		        COALESCE(j.settled_sum, 0), COALESCE(j.entry_count, 0), COALESCE(j.last_event_ms, 0)
		 FROM cashflow_journal_state s
		 LEFT JOIN (SELECT platform, account, SUM(amount_usd) AS settled_sum,
		                   COUNT(*) AS entry_count, MAX(time_ms) AS last_event_ms
		            FROM cashflow_journal GROUP BY platform, account) j
		   ON j.platform = s.platform AND j.account = s.account
		 ORDER BY s.platform, s.account`)
	if err != nil {
		return nil, fmt.Errorf("list cashflow journal wallets: %w", err)
	}
	defer rows.Close()
	var out []CashflowJournalWalletStatus
	for rows.Next() {
		var w CashflowJournalWalletStatus
		var baselineSet, incomplete int
		if err := rows.Scan(&w.Platform, &w.Account, &baselineSet, &incomplete,
			&w.BaselineAccountValue, &w.SettledSum, &w.EntryCount, &w.LastEventMs); err != nil {
			return nil, fmt.Errorf("scan cashflow journal wallet: %w", err)
		}
		w.BaselineSet = baselineSet != 0
		w.Incomplete = incomplete != 0
		w.ShadowOnly = w.Platform != "hyperliquid"
		w.LiveBasisEligible = !w.ShadowOnly && w.BaselineSet && !w.Incomplete
		if !w.ShadowOnly {
			w.Basis = cashflowJournalBases.get(sharedWalletKeyLabel(SharedWalletKey{Platform: w.Platform, Account: w.Account}))
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func cashflowFillSettledDelta(closedPnlGross, fee float64) float64 {
	return closedPnlGross - fee
}

func hlFillIsSpot(coin string) bool {
	c := strings.TrimSpace(coin)
	return strings.HasPrefix(c, "@") || strings.Contains(c, "/")
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
	AccountValue     float64
	CurrentUPnL      float64
	Fills            []hlFillRecord
	Funding          []hlLedgerEvent
	Transfers        []hlLedgerEvent
	FillsFetched     bool
	FundingFetched   bool
	TransfersFetched bool
}

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

func ingestCashflowJournalEvents(sdb *StateDB, res cashflowJournalFetchResult, cutoffMs int64) CashflowJournalState {
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
			if f.Time > cutoffMs {
				continue
			}
			coin := strings.ToUpper(strings.TrimSpace(f.Coin))
			closedPnl := parseHLFloat(f.ClosedPnl)
			fee := parseHLFloat(f.Fee)
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
			if ev.Time > cutoffMs {
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
			if ev.Time > cutoffMs {
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

type cashflowJournalReconcile struct {
	Key            SharedWalletKey
	AccountValue   float64
	ExpectedEquity float64
	Drift          float64
	SettledSum     float64
	DeltaUPnL      float64
	ClosedPnlBasis float64
	Incomplete     bool
	Usable         bool
}

func reconcileCashflowJournal(sdb *StateDB, key SharedWalletKey, accountValue, currentUPnL float64, snapshotAt time.Time) *cashflowJournalReconcile {
	if sdb == nil || key.Platform != "hyperliquid" || key.Account == "" {
		return nil
	}
	res := fetchCashflowJournalEvents(sdb, key, accountValue, currentUPnL, snapshotAt)
	if !res.StateFound {
		return nil
	}
	st := ingestCashflowJournalEvents(sdb, res, snapshotAt.UnixMilli())
	rec := &cashflowJournalReconcile{Key: key, AccountValue: res.AccountValue, Incomplete: st.Incomplete}
	if !st.BaselineSet {
		return rec
	}
	settled, err := sdb.SumCashflowJournal(key.Platform, key.Account)
	if err != nil {
		fmt.Printf("[WARN] cashflow-journal %s: settled-sum read failed: %v\n", sharedWalletKeyLabel(key), err)
		return rec
	}
	rec.SettledSum = settled
	rec.DeltaUPnL = res.CurrentUPnL - st.BaselineUPnL
	rec.ExpectedEquity = cashflowJournalExpectedEquity(st.BaselineAccountValue, st.BaselineUPnL, settled, res.CurrentUPnL)
	rec.Drift = res.AccountValue - rec.ExpectedEquity
	if basis, applied, err := hlClosedPnlBasisForJournal(sdb, key); err != nil {
		fmt.Printf("[WARN] cashflow-journal %s: closedPnl basis unavailable: %v — comparing raw closedPnl this cycle\n", sharedWalletKeyLabel(key), err)
	} else if applied {
		rec.ClosedPnlBasis = basis
		rec.ExpectedEquity -= basis
		rec.Drift = res.AccountValue - rec.ExpectedEquity
	}
	rec.Usable = res.FillsFetched && res.FundingFetched && res.TransfersFetched && !st.Incomplete
	return rec
}

func cashflowJournalAlarmEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GO_TRADER_CASHFLOW_JOURNAL_ALARM"))) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

type cashflowJournalPendingTracker struct {
	mu      sync.Mutex
	streaks map[string]int
}

func (t *cashflowJournalPendingTracker) mark(label string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.streaks == nil {
		t.streaks = make(map[string]int)
	}
	t.streaks[label]++
	return t.streaks[label]
}

func (t *cashflowJournalPendingTracker) reset(label string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.streaks, label)
}

var cashflowJournalPendingStreaks = &cashflowJournalPendingTracker{}

const (
	cashflowBasisJournal     = "journal"
	cashflowBasisPending     = "pending"
	cashflowBasisTradeLedger = "trade_ledger"
	cashflowBasisDisabled    = "disabled"
	cashflowBasisUnknown     = "unknown"
)

type cashflowJournalBasisRegistry struct {
	mu    sync.Mutex
	bases map[string]string
}

func (r *cashflowJournalBasisRegistry) record(label, basis string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bases == nil {
		r.bases = make(map[string]string)
	}
	r.bases[label] = basis
}

func (r *cashflowJournalBasisRegistry) get(label string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.bases[label]; ok {
		return b
	}
	return cashflowBasisUnknown
}

var cashflowJournalBases = &cashflowJournalBasisRegistry{}

func applyCashflowJournalDriftBasis(results []sharedWalletDriftResult, key SharedWalletKey, rec *cashflowJournalReconcile, enabled bool) {
	if key.Platform != "hyperliquid" {
		return
	}
	if rec == nil {
		return
	}
	var ledger *sharedWalletDriftResult
	for i := range results {
		if results[i].Key == key {
			ledger = &results[i]
			break
		}
	}

	label := sharedWalletKeyLabel(key)
	transientPending := enabled && !rec.Incomplete && !rec.Usable
	pendingStreak := 0
	if transientPending {
		pendingStreak = cashflowJournalPendingStreaks.mark(label)
	} else {
		cashflowJournalPendingStreaks.reset(label)
	}
	suppressPending := transientPending && pendingStreak <= sharedWalletDriftAlertThreshold

	ledgerNote := "n/a"
	if ledger != nil {
		ledgerNote = fmt.Sprintf("raw $%+.2f / post-baseline $%+.2f", ledger.Balance-ledger.MemberSum, ledger.Drift)
	}
	var switchNote, basis string
	switch {
	case !enabled:
		switchNote = "OFF (operator-disabled via GO_TRADER_CASHFLOW_JOURNAL_ALARM)"
		basis = cashflowBasisDisabled
	case rec.Incomplete:
		switchNote = "OFF (journal incomplete — failing closed to trade-ledger)"
		basis = cashflowBasisTradeLedger
	case !rec.Usable && suppressPending:
		switchNote = fmt.Sprintf("PENDING (journal not usable — transient miss %d/%d, journal streak preserved)", pendingStreak, sharedWalletDriftAlertThreshold)
		basis = cashflowBasisPending
	case !rec.Usable:
		switchNote = fmt.Sprintf("OFF (journal not usable for %d cycles — failing closed to trade-ledger)", pendingStreak)
		basis = cashflowBasisTradeLedger
	default:
		switchNote = "ON (journal is the drift-alarm basis)"
		basis = cashflowBasisJournal
	}
	cashflowJournalBases.record(label, basis)
	fmt.Printf("[cashflow-journal] %s: expected_equity $%.2f vs accountValue $%.2f → journal_drift $%+.4f (settled Σ $%+.2f, ΔuPnL $%+.2f, closedPnl basis $%+.4f); trade-ledger %s; alarm %s\n",
		sharedWalletKeyLabel(key), rec.ExpectedEquity, rec.AccountValue, rec.Drift, rec.SettledSum, rec.DeltaUPnL, rec.ClosedPnlBasis, ledgerNote, switchNote)

	if ledger == nil {
		return
	}
	if !enabled {
		return
	}
	if !rec.Usable {
		if rec.Incomplete {
			return
		}
		if suppressPending {
			ledger.JournalPending = true
			return
		}
		return
	}
	ledger.Drift = rec.Drift
	ledger.Basis = driftBasisJournal
	ledger.ExpectedEquity = rec.ExpectedEquity
}

func sumHLAccountUPnL(positions []HLPosition) float64 {
	sum := 0.0
	for _, p := range positions {
		sum += p.UnrealizedPnL
	}
	return sum
}

// hyperliquidClosedPnlBasisError is the accumulated gap closedPnl - fill-price
// realized PnL for journaled perp fills. Hyperliquid documents closedPnl as a
// frontend convenience (entry price and pnl); account value follows the margin
// change of the trades. Same-timestamp partials are chained by startPosition
// because the exchange does not return them in position order. Fills absent
// from journaled train entry and do not add to the gap. An unknown entry
// (no chained open) leaves that fill on raw closedPnl.
func hyperliquidClosedPnlBasisError(fills []hlFillRecord, journaled map[string]struct{}) float64 {
	ordered := orderHLFillsByPositionChain(fills)
	type book struct {
		sz, ntl *big.Rat
		known   bool
	}
	books := map[string]*book{}
	sum := new(big.Rat)
	for _, f := range ordered {
		if hlFillIsSpot(f.Coin) {
			continue
		}
		coin := strings.ToUpper(strings.TrimSpace(f.Coin))
		if coin == "" {
			continue
		}
		st := books[coin]
		if st == nil {
			st = &book{sz: new(big.Rat), ntl: new(big.Rat)}
			books[coin] = st
		}
		start := hlRat(f.StartPosition)
		signed := hlFillSignedSize(f)
		if st.sz.Cmp(start) != 0 {
			st.known = false
			st.sz = new(big.Rat).Set(start)
			st.ntl = new(big.Rat)
		}
		if start.Sign() == 0 {
			st.known = true
			st.sz = new(big.Rat)
			st.ntl = new(big.Rat)
		}
		if _, ok := journaled[cashflowFillDedupID(f)]; ok && st.known && start.Sign() != 0 && new(big.Rat).Mul(signed, start).Sign() < 0 {
			closeSz := absRat(signed)
			if startAbs := absRat(start); closeSz.Cmp(startAbs) > 0 {
				closeSz = startAbs
			}
			entry := new(big.Rat).Quo(new(big.Rat).Set(st.ntl), start)
			realized := new(big.Rat).Mul(new(big.Rat).Sub(hlRat(f.Px), entry), closeSz)
			if start.Sign() < 0 {
				realized.Neg(realized)
			}
			sum.Add(sum, new(big.Rat).Sub(hlRat(f.ClosedPnl), realized))
		}
		newSz := new(big.Rat).Add(start, signed)
		if !st.known {
			st.sz = newSz
			continue
		}
		px := hlRat(f.Px)
		if start.Sign() == 0 || new(big.Rat).Mul(signed, start).Sign() > 0 {
			st.ntl = new(big.Rat).Add(st.ntl, new(big.Rat).Mul(px, signed))
			st.sz = newSz
			continue
		}
		if newSz.Sign() == 0 {
			st.sz = new(big.Rat)
			st.ntl = new(big.Rat)
			continue
		}
		if new(big.Rat).Mul(start, newSz).Sign() > 0 {
			entry := new(big.Rat).Quo(new(big.Rat).Set(st.ntl), start)
			st.sz = newSz
			st.ntl = new(big.Rat).Mul(entry, newSz)
			continue
		}
		st.sz = newSz
		st.ntl = new(big.Rat).Mul(px, newSz)
	}
	out, _ := sum.Float64()
	return out
}

func hlRat(s string) *big.Rat {
	r := new(big.Rat)
	if _, ok := r.SetString(strings.TrimSpace(s)); !ok {
		return new(big.Rat)
	}
	return r
}

func absRat(r *big.Rat) *big.Rat {
	if r.Sign() < 0 {
		return new(big.Rat).Neg(r)
	}
	return new(big.Rat).Set(r)
}

func hlFillSignedSize(f hlFillRecord) *big.Rat {
	sz := hlRat(f.Sz)
	switch strings.ToUpper(strings.TrimSpace(f.Side)) {
	case "B":
		return sz
	case "A":
		return new(big.Rat).Neg(sz)
	default:
		return new(big.Rat)
	}
}

func orderHLFillsByPositionChain(fills []hlFillRecord) []hlFillRecord {
	sorted := append([]hlFillRecord(nil), fills...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })
	out := make([]hlFillRecord, 0, len(sorted))
	for i := 0; i < len(sorted); {
		j := i + 1
		for j < len(sorted) && sorted[j].Time == sorted[i].Time {
			j++
		}
		out = append(out, chainHLFillsAtTime(sorted[i:j])...)
		i = j
	}
	return out
}

func chainHLFillsAtTime(fills []hlFillRecord) []hlFillRecord {
	byCoin := map[string][]hlFillRecord{}
	var coins []string
	for _, f := range fills {
		coin := f.Coin
		if _, ok := byCoin[coin]; !ok {
			coins = append(coins, coin)
		}
		byCoin[coin] = append(byCoin[coin], f)
	}
	sort.Strings(coins)
	var out []hlFillRecord
	for _, coin := range coins {
		out = append(out, chainHLCoinFills(byCoin[coin])...)
	}
	return out
}

func chainHLCoinFills(fills []hlFillRecord) []hlFillRecord {
	n := len(fills)
	if n <= 1 {
		return append([]hlFillRecord(nil), fills...)
	}
	used := make([]bool, n)
	starts := make([]*big.Rat, n)
	ends := make([]*big.Rat, n)
	for i, f := range fills {
		starts[i] = hlRat(f.StartPosition)
		ends[i] = new(big.Rat).Add(starts[i], hlFillSignedSize(f))
	}
	var current *big.Rat
	for i := range fills {
		if !hlRatEqualsAny(starts[i], ends) {
			current = starts[i]
			break
		}
	}
	if current == nil {
		current = starts[0]
	}
	seq := make([]hlFillRecord, 0, n)
	for len(seq) < n {
		found := -1
		for i := range fills {
			if used[i] {
				continue
			}
			if starts[i].Cmp(current) == 0 {
				found = i
				break
			}
		}
		if found < 0 {
			for i := range fills {
				if !used[i] {
					seq = append(seq, fills[i])
				}
			}
			break
		}
		used[found] = true
		seq = append(seq, fills[found])
		current = ends[found]
	}
	return seq
}

func hlRatEqualsAny(target *big.Rat, vals []*big.Rat) bool {
	for _, v := range vals {
		if target.Cmp(v) == 0 {
			return true
		}
	}
	return false
}

const hlUserFillsByTimeLimit = 2000

func fetchHyperliquidFillsForBasis(account string) ([]hlFillRecord, error) {
	var all []hlFillRecord
	seen := map[string]struct{}{}
	start := int64(0)
	for page := 0; page < 20; page++ {
		batch, err := fetchHyperliquidUserFillsByTime(account, start)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		var maxT int64
		added := 0
		for _, f := range batch {
			if f.Time > maxT {
				maxT = f.Time
			}
			id := cashflowFillDedupID(f)
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			all = append(all, f)
			added++
		}
		if len(batch) < hlUserFillsByTimeLimit || added == 0 {
			break
		}
		if maxT < start {
			break
		}
		start = maxT
	}
	return all, nil
}

func listCashflowFillDedupIDs(sdb *StateDB, platform, account string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if sdb == nil || sdb.db == nil {
		return nil, fmt.Errorf("state db unavailable")
	}
	rows, err := sdb.db.Query(
		`SELECT dedup_id FROM cashflow_journal WHERE platform = ? AND account = ? AND kind = 'fill'`,
		platform, account)
	if err != nil {
		return nil, fmt.Errorf("list cashflow fill dedup ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan cashflow fill dedup id: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cashflow fill dedup ids: %w", err)
	}
	return out, nil
}

func hlClosedPnlBasisForJournal(sdb *StateDB, key SharedWalletKey) (basis float64, applied bool, err error) {
	ids, err := listCashflowFillDedupIDs(sdb, key.Platform, key.Account)
	if err != nil {
		return 0, false, err
	}
	if len(ids) == 0 {
		return 0, true, nil
	}
	fills, err := fetchHyperliquidFillsForBasis(key.Account)
	if err != nil {
		return 0, false, err
	}
	return hyperliquidClosedPnlBasisError(fills, ids), true, nil
}
