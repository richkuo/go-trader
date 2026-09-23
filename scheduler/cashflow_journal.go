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
	st, _ := ingestCashflowJournalEventsChecked(sdb, res, cutoffMs)
	return st
}

func ingestCashflowJournalEventsChecked(sdb *StateDB, res cashflowJournalFetchResult, cutoffMs int64) (CashflowJournalState, bool) {
	st := res.State
	if sdb == nil || !res.StateFound {
		return st, false
	}
	key := res.Key
	insertsOK := true

	if res.FillsFetched {
		maxTime, failedAt, unpriced, err := sdb.insertHyperliquidCashflowFills(key, res.Fills, st.FillsSinceMs, cutoffMs)
		logHLUnpricedCloses(key, unpriced)
		if err != nil {
			insertsOK = false
			fmt.Printf("[WARN] cashflow-journal %s: fill insert failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
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
				insertsOK = false
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
				insertsOK = false
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
	return st, insertsOK
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
	basisReady, basisErr := ensureHyperliquidCashflowBasis(sdb, key, res.State)
	if basisErr != nil {
		fmt.Printf("[WARN] cashflow-journal %s: closedPnl basis backfill failed: %v — alarm pending\n", sharedWalletKeyLabel(key), basisErr)
	}
	if !basisReady {
		res.FillsFetched = false
	}
	st, insertsOK := ingestCashflowJournalEventsChecked(sdb, res, snapshotAt.UnixMilli())
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
	if basisReady {
		if basis, err := sdb.SumHyperliquidCashflowBasis(key); err != nil {
			fmt.Printf("[WARN] cashflow-journal %s: closedPnl basis read failed: %v — alarm pending\n", sharedWalletKeyLabel(key), err)
			basisReady = false
		} else {
			rec.ClosedPnlBasis = basis
			rec.ExpectedEquity -= basis
			rec.Drift = res.AccountValue - rec.ExpectedEquity
		}
	}
	rec.Usable = basisReady && insertsOK && res.FillsFetched && res.FundingFetched && res.TransfersFetched && !st.Incomplete
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

type hlBasisBook struct {
	sz          *big.Rat
	ntl         *big.Rat
	hasPosition bool
	entryKnown  bool
}

type hlBasisState struct {
	scanSinceMs int64
	targetMs    int64
	ready       bool
}

const hlUserFillsByTimeLimit = 2000

func parseHLRat(s string) (*big.Rat, bool) {
	r := new(big.Rat)
	if _, ok := r.SetString(strings.TrimSpace(s)); !ok {
		return new(big.Rat), false
	}
	return r, true
}

func absRat(r *big.Rat) *big.Rat {
	if r.Sign() < 0 {
		return new(big.Rat).Neg(r)
	}
	return new(big.Rat).Set(r)
}

func hlFillSignedSize(f hlFillRecord) (*big.Rat, bool) {
	sz, ok := parseHLRat(f.Sz)
	if !ok || sz.Sign() < 0 {
		return new(big.Rat), false
	}
	switch strings.ToUpper(strings.TrimSpace(f.Side)) {
	case "B":
		return sz, true
	case "A":
		return new(big.Rat).Neg(sz), true
	default:
		return new(big.Rat), false
	}
}

func chainHLCoinFills(fills []hlFillRecord, expected *big.Rat, hasExpected bool) ([]hlFillRecord, bool) {
	if len(fills) <= 1 {
		return append([]hlFillRecord(nil), fills...), true
	}
	starts := make([]*big.Rat, len(fills))
	ends := make([]*big.Rat, len(fills))
	for i, f := range fills {
		start, startOK := parseHLRat(f.StartPosition)
		signed, sizeOK := hlFillSignedSize(f)
		if !startOK || !sizeOK {
			return append([]hlFillRecord(nil), fills...), false
		}
		starts[i] = start
		ends[i] = new(big.Rat).Add(start, signed)
	}
	current := new(big.Rat)
	if hasExpected {
		current.Set(expected)
	} else {
		head := -1
		for i := range fills {
			matchesEnd := false
			for j := range fills {
				if i != j && starts[i].Cmp(ends[j]) == 0 {
					matchesEnd = true
					break
				}
			}
			if !matchesEnd {
				if head >= 0 {
					return append([]hlFillRecord(nil), fills...), false
				}
				head = i
			}
		}
		if head < 0 {
			return append([]hlFillRecord(nil), fills...), false
		}
		current.Set(starts[head])
	}
	used := make([]bool, len(fills))
	ordered := make([]hlFillRecord, 0, len(fills))
	for len(ordered) < len(fills) {
		found := -1
		for i := range fills {
			if !used[i] && starts[i].Cmp(current) == 0 {
				if found >= 0 {
					return append([]hlFillRecord(nil), fills...), false
				}
				found = i
			}
		}
		if found < 0 {
			return append([]hlFillRecord(nil), fills...), false
		}
		used[found] = true
		ordered = append(ordered, fills[found])
		current.Set(ends[found])
	}
	return ordered, true
}

func applyHLBasisFill(book *hlBasisBook, f hlFillRecord, journaled bool) (*big.Rat, bool, bool) {
	start, startOK := parseHLRat(f.StartPosition)
	signed, sizeOK := hlFillSignedSize(f)
	px, pxOK := parseHLRat(f.Px)
	closedPnl, pnlOK := parseHLRat(f.ClosedPnl)
	if !startOK || !sizeOK || !pxOK || !pnlOK {
		book.hasPosition = false
		book.entryKnown = false
		book.sz = new(big.Rat)
		book.ntl = new(big.Rat)
		return new(big.Rat), false, journaled
	}
	if !book.hasPosition || book.sz.Cmp(start) != 0 {
		book.hasPosition = true
		book.entryKnown = false
		book.sz = new(big.Rat).Set(start)
		book.ntl = new(big.Rat)
	}
	if start.Sign() == 0 {
		book.entryKnown = true
		book.ntl = new(big.Rat)
	}
	basis := new(big.Rat)
	closing := start.Sign() != 0 && new(big.Rat).Mul(signed, start).Sign() < 0
	unpriced := journaled && closing && !book.entryKnown
	if journaled && closing && book.entryKnown {
		closeSz := absRat(signed)
		if startAbs := absRat(start); closeSz.Cmp(startAbs) > 0 {
			closeSz = startAbs
		}
		entry := new(big.Rat).Quo(new(big.Rat).Set(book.ntl), start)
		realized := new(big.Rat).Mul(new(big.Rat).Sub(px, entry), closeSz)
		if start.Sign() < 0 {
			realized.Neg(realized)
		}
		basis.Sub(closedPnl, realized)
	}
	newSz := new(big.Rat).Add(start, signed)
	if !book.entryKnown {
		book.sz = newSz
		return basis, true, unpriced
	}
	if start.Sign() == 0 || new(big.Rat).Mul(signed, start).Sign() > 0 {
		book.ntl.Add(book.ntl, new(big.Rat).Mul(px, signed))
		book.sz = newSz
		return basis, true, unpriced
	}
	if newSz.Sign() == 0 {
		book.sz = new(big.Rat)
		book.ntl = new(big.Rat)
		return basis, true, unpriced
	}
	if new(big.Rat).Mul(start, newSz).Sign() > 0 {
		entry := new(big.Rat).Quo(new(big.Rat).Set(book.ntl), start)
		book.sz = newSz
		book.ntl = new(big.Rat).Mul(entry, newSz)
		return basis, true, unpriced
	}
	book.sz = newSz
	book.ntl = new(big.Rat).Mul(px, newSz)
	return basis, true, unpriced
}

func processHLBasisFills(books map[string]*hlBasisBook, fills []hlFillRecord, journaled map[string]struct{}) (map[string]float64, float64, int) {
	sorted := append([]hlFillRecord(nil), fills...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })
	contributions := make(map[string]float64)
	total := new(big.Rat)
	unpriced := 0
	for i := 0; i < len(sorted); {
		j := i + 1
		for j < len(sorted) && sorted[j].Time == sorted[i].Time {
			j++
		}
		byCoin := make(map[string][]hlFillRecord)
		var coins []string
		for _, f := range sorted[i:j] {
			coin := strings.ToUpper(strings.TrimSpace(f.Coin))
			if hlFillIsSpot(coin) || coin == "" {
				continue
			}
			if _, ok := byCoin[coin]; !ok {
				coins = append(coins, coin)
			}
			byCoin[coin] = append(byCoin[coin], f)
		}
		sort.Strings(coins)
		for _, coin := range coins {
			book := books[coin]
			if book == nil {
				book = &hlBasisBook{sz: new(big.Rat), ntl: new(big.Rat)}
				books[coin] = book
			}
			ordered, resolved := chainHLCoinFills(byCoin[coin], book.sz, book.hasPosition)
			if !resolved {
				if book.hasPosition {
					final := new(big.Rat).Set(book.sz)
					valid := true
					for _, f := range byCoin[coin] {
						signed, ok := hlFillSignedSize(f)
						if !ok {
							valid = false
							break
						}
						final.Add(final, signed)
					}
					book.hasPosition = valid
					book.sz = final
				} else {
					book.hasPosition = false
					book.sz = new(big.Rat)
				}
				book.entryKnown = false
				book.ntl = new(big.Rat)
				for _, f := range byCoin[coin] {
					if _, ok := journaled[cashflowFillDedupID(f)]; ok {
						contributions[cashflowFillDedupID(f)] = 0
						if pnl, ok := parseHLRat(f.ClosedPnl); !ok || pnl.Sign() != 0 {
							unpriced++
						}
					}
				}
				continue
			}
			for _, f := range ordered {
				id := cashflowFillDedupID(f)
				_, keep := journaled[id]
				basis, valid, missed := applyHLBasisFill(book, f, keep)
				if missed {
					unpriced++
				}
				if keep {
					value := 0.0
					if valid {
						value, _ = basis.Float64()
						total.Add(total, basis)
					}
					contributions[id] = value
				}
			}
		}
		i = j
	}
	out, _ := total.Float64()
	return contributions, out, unpriced
}

func hyperliquidClosedPnlBasisError(fills []hlFillRecord, journaled map[string]struct{}) float64 {
	_, total, _ := processHLBasisFills(make(map[string]*hlBasisBook), fills, journaled)
	return total
}

func loadHLBasisBooks(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, key SharedWalletKey) (map[string]*hlBasisBook, error) {
	rows, err := q.Query(`SELECT coin, position_size, entry_notional, has_position, entry_known
		FROM cashflow_hl_basis_book WHERE platform = ? AND account = ?`, key.Platform, key.Account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	books := make(map[string]*hlBasisBook)
	for rows.Next() {
		var coin, sizeText, notionalText string
		var hasPosition, entryKnown int
		if err := rows.Scan(&coin, &sizeText, &notionalText, &hasPosition, &entryKnown); err != nil {
			return nil, err
		}
		sz, szOK := parseHLRat(sizeText)
		ntl, ntlOK := parseHLRat(notionalText)
		if !szOK || !ntlOK {
			return nil, fmt.Errorf("invalid basis book for %s", coin)
		}
		books[coin] = &hlBasisBook{sz: sz, ntl: ntl, hasPosition: hasPosition != 0, entryKnown: entryKnown != 0}
	}
	return books, rows.Err()
}

func saveHLBasisBooks(tx *sql.Tx, key SharedWalletKey, books map[string]*hlBasisBook) error {
	coins := make([]string, 0, len(books))
	for coin := range books {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	for _, coin := range coins {
		book := books[coin]
		if _, err := tx.Exec(`INSERT INTO cashflow_hl_basis_book
			(platform, account, coin, position_size, entry_notional, has_position, entry_known)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(platform, account, coin) DO UPDATE SET
			position_size = excluded.position_size, entry_notional = excluded.entry_notional,
			has_position = excluded.has_position, entry_known = excluded.entry_known`,
			key.Platform, key.Account, coin, book.sz.RatString(), book.ntl.RatString(), boolInt(book.hasPosition), boolInt(book.entryKnown)); err != nil {
			return err
		}
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (sdb *StateDB) loadHLBasisState(key SharedWalletKey) (hlBasisState, bool, error) {
	var st hlBasisState
	var ready int
	err := sdb.db.QueryRow(`SELECT scan_since_ms, target_ms, ready
		FROM cashflow_hl_basis_state WHERE platform = ? AND account = ?`, key.Platform, key.Account).
		Scan(&st.scanSinceMs, &st.targetMs, &ready)
	if err == sql.ErrNoRows {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	st.ready = ready != 0
	return st, true, nil
}

func ensureHyperliquidCashflowBasis(sdb *StateDB, key SharedWalletKey, journalState CashflowJournalState) (bool, error) {
	if sdb == nil || sdb.db == nil {
		return false, fmt.Errorf("state db unavailable")
	}
	st, found, err := sdb.loadHLBasisState(key)
	if err != nil {
		return false, err
	}
	if !found {
		st = hlBasisState{targetMs: journalState.FillsSinceMs - 1}
		if _, err := sdb.db.Exec(`INSERT INTO cashflow_hl_basis_state
			(platform, account, scan_since_ms, target_ms, ready)
			VALUES (?, ?, 0, ?, 0)`, key.Platform, key.Account, st.targetMs); err != nil {
			return false, err
		}
	}
	if st.ready {
		return true, nil
	}
	batch, err := fetchHyperliquidUserFillsByTime(key.Account, st.scanSinceMs)
	if err != nil {
		return false, err
	}
	tx, err := sdb.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	books, err := loadHLBasisBooks(tx, key)
	if err != nil {
		return false, err
	}
	journaled := make(map[string]struct{})
	rows, err := tx.Query(`SELECT dedup_id FROM cashflow_journal
		WHERE platform = ? AND account = ? AND kind = 'fill' AND hl_basis_resolved = 0`, key.Platform, key.Account)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		journaled[id] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	fullPage := len(batch) >= hlUserFillsByTimeLimit
	maxTime := st.scanSinceMs
	unique := make([]hlFillRecord, 0, len(batch))
	for _, f := range batch {
		if f.Time > maxTime {
			maxTime = f.Time
		}
		if f.Time < st.scanSinceMs || f.Time > st.targetMs {
			continue
		}
		result, err := tx.Exec(`INSERT OR IGNORE INTO cashflow_hl_basis_seen (platform, account, dedup_id) VALUES (?, ?, ?)`,
			key.Platform, key.Account, cashflowFillDedupID(f))
		if err != nil {
			return false, err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if added > 0 {
			unique = append(unique, f)
		}
	}
	contributions, _, unpriced := processHLBasisFills(books, unique, journaled)
	for id, basis := range contributions {
		if _, err := tx.Exec(`UPDATE cashflow_journal SET hl_basis_error = ?, hl_basis_resolved = 1
			WHERE platform = ? AND account = ? AND dedup_id = ?`, basis, key.Platform, key.Account, id); err != nil {
			return false, err
		}
	}
	stuck := fullPage && maxTime <= st.targetMs && len(unique) == 0
	done := !fullPage || maxTime > st.targetMs || stuck
	if stuck {
		books = map[string]*hlBasisBook{}
		if _, err := tx.Exec(`DELETE FROM cashflow_hl_basis_book WHERE platform = ? AND account = ?`, key.Platform, key.Account); err != nil {
			return false, err
		}
	}
	if err := saveHLBasisBooks(tx, key, books); err != nil {
		return false, err
	}
	var uncovered int64
	if done {
		result, err := tx.Exec(`UPDATE cashflow_journal SET hl_basis_error = 0, hl_basis_resolved = 1
			WHERE platform = ? AND account = ? AND kind = 'fill' AND hl_basis_resolved = 0`, key.Platform, key.Account)
		if err != nil {
			return false, err
		}
		if uncovered, err = result.RowsAffected(); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`DELETE FROM cashflow_hl_basis_seen WHERE platform = ? AND account = ?`, key.Platform, key.Account); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(`UPDATE cashflow_hl_basis_state SET scan_since_ms = ?, ready = ?
		WHERE platform = ? AND account = ?`, maxTime, boolInt(done), key.Platform, key.Account); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if stuck {
		fmt.Printf("[WARN] cashflow-journal %s: closedPnl basis backfill stalled on a full page at %d ms — later closes stay on raw closedPnl until each position is flat\n", sharedWalletKeyLabel(key), maxTime)
	}
	if uncovered > 0 {
		fmt.Printf("[WARN] cashflow-journal %s: %d journaled fills are outside the exchange fill history — they stay on raw closedPnl\n", sharedWalletKeyLabel(key), uncovered)
	}
	logHLUnpricedCloses(key, unpriced)
	return done, nil
}

func (sdb *StateDB) SumHyperliquidCashflowBasis(key SharedWalletKey) (float64, error) {
	if sdb == nil || sdb.db == nil {
		return 0, fmt.Errorf("state db unavailable")
	}
	var sum sql.NullFloat64
	if err := sdb.db.QueryRow(`SELECT SUM(hl_basis_error) FROM cashflow_journal
		WHERE platform = ? AND account = ? AND kind = 'fill'`, key.Platform, key.Account).Scan(&sum); err != nil {
		return 0, fmt.Errorf("sum closedPnl basis: %w", err)
	}
	return sum.Float64, nil
}

func logHLUnpricedCloses(key SharedWalletKey, n int) {
	if n > 0 {
		fmt.Printf("[WARN] cashflow-journal %s: %d journaled closes have no known entry price in exchange fill history — they stay on raw closedPnl\n", sharedWalletKeyLabel(key), n)
	}
}

func (sdb *StateDB) insertHyperliquidCashflowFills(key SharedWalletKey, fills []hlFillRecord, sinceMs, cutoffMs int64) (int64, int64, int, error) {
	if sdb == nil || sdb.db == nil {
		return sinceMs - 1, sinceMs, 0, fmt.Errorf("state db unavailable")
	}
	pageEndMs := int64(-1)
	if len(fills) >= hlUserFillsByTimeLimit {
		minTime := fills[0].Time
		for _, f := range fills {
			if f.Time > pageEndMs {
				pageEndMs = f.Time
			}
			if f.Time < minTime {
				minTime = f.Time
			}
		}
		if minTime == pageEndMs {
			pageEndMs = -1
		}
	}
	eligible := make([]hlFillRecord, 0, len(fills))
	maxTime := sinceMs - 1
	for _, f := range fills {
		if f.Time < sinceMs || f.Time > cutoffMs || (pageEndMs >= 0 && f.Time >= pageEndMs) {
			continue
		}
		eligible = append(eligible, f)
		if f.Time > maxTime {
			maxTime = f.Time
		}
	}
	if len(eligible) == 0 {
		return maxTime, -1, 0, nil
	}
	fail := func(err error) (int64, int64, int, error) { return sinceMs - 1, sinceMs, 0, err }
	tx, err := sdb.db.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	books, err := loadHLBasisBooks(tx, key)
	if err != nil {
		return fail(err)
	}
	newFills := make([]hlFillRecord, 0, len(eligible))
	journaled := make(map[string]struct{})
	inPage := make(map[string]struct{}, len(eligible))
	for _, f := range eligible {
		id := cashflowFillDedupID(f)
		if _, dup := inPage[id]; dup {
			continue
		}
		inPage[id] = struct{}{}
		var exists int
		err := tx.QueryRow(`SELECT 1 FROM cashflow_journal WHERE dedup_id = ?`, id).Scan(&exists)
		if err != nil && err != sql.ErrNoRows {
			return fail(err)
		}
		if err == nil {
			continue
		}
		newFills = append(newFills, f)
		if !hlFillIsSpot(f.Coin) {
			journaled[id] = struct{}{}
		}
	}
	contributions, _, unpriced := processHLBasisFills(books, newFills, journaled)
	for _, f := range newFills {
		coin := strings.ToUpper(strings.TrimSpace(f.Coin))
		closedPnl := parseHLFloat(f.ClosedPnl)
		fee := parseHLFloat(f.Fee)
		kind := "fill"
		delta := cashflowFillSettledDelta(closedPnl, fee)
		if hlFillIsSpot(coin) {
			kind = "fill_spot"
			delta = 0
		}
		id := cashflowFillDedupID(f)
		if _, err := tx.Exec(`INSERT INTO cashflow_journal
			(platform, account, time_ms, kind, amount_usd, coin, closed_pnl_gross, fee_usd, hl_basis_error, hl_basis_resolved, dedup_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`, key.Platform, key.Account, f.Time, kind, delta, coin, closedPnl, fee, contributions[id], id); err != nil {
			return fail(err)
		}
	}
	if err := saveHLBasisBooks(tx, key, books); err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return maxTime, -1, unpriced, nil
}
