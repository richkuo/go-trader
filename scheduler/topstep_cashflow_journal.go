package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// #1106: exchange-sourced cash-flow journal for TopStep shared wallets — SHADOW
// phase (Phase 4 of #1100), mirroring HL's #1103 shadow / #1104 flip two-step and
// OKX's #1105 shadow.
//
// This reuses the platform-generic journal machinery built for Hyperliquid in
// children 1–2 (cashflow_journal.go): the cashflow_journal / cashflow_journal_state
// tables (keyed by (platform, account)), the durable per-wallet cursor + adoption
// baseline, per-event dedup, halt-on-persist-failure, snapshot-bounded ingestion,
// the incomplete fail-closed latch, and the shared equity equation
// (cashflowJournalExpectedEquity). HL and OKX behavior are untouched.
//
// EVENT SHAPE — HL-LIKE, NOT OKX-LIKE. OKX exposes a single pre-netted balChg
// per bill, so it needs no fee arithmetic. TopStep (CME futures via a prop
// account) settles per fill in USD with a GROSS realized PnL and a separately-
// reported commission, exactly like Hyperliquid's userFills. So a TopStep fill's
// settled-cash delta is realized-PnL-GROSS minus the commission charged —
// computed by the shared cashflowFillSettledDelta — and the gross value is
// retained for attribution only and is NEVER summed into equity on its own
// (same gross-PnL discipline as HL/OKX, #698). Entry fills carry realized PnL 0
// and settle −commission; closing fills settle gross−commission.
//
// SINGLE STREAM. TopStep fills are the one settled-cash source modeled here
// (CME futures have no perpetual funding, and prop-account payouts/resets are
// not fills). Only the FillsSinceMs cursor of the shared CashflowJournalState is
// used (as the fills watermark); FundingSinceMs / TransfersSinceMs stay 0 and
// unused for TopStep wallets — same as OKX.
//
// RECONCILED FIELD: TopStep account EQUITY (settled USD cash balance + unrealized
// PnL), USD-denominated. equity = cash + uPnL, decomposed identically to HL/OKX:
//
//	expected = baseline_equity
//	         + Σ(fill settled deltas since baseline)   // gross − commission per fill
//	         + (current_uPnL − baseline_uPnL)
//
// current_uPnL / baseline_uPnL come from the SAME balance read as equity
// (uPnL = equity − cashBalance, not a separately-timed positions sum), so equity
// and uPnL are one coherent snapshot: the uPnL term cancels against equity and
// residual drift reduces to the pure settled-cash reconstruction error — no
// intra-cycle uPnL jitter (same coherence guarantee as OKX).
//
// SHADOW ONLY. This phase NEVER drives a TopStep drift alarm — there is no live
// TopStep shared-wallet alarm to drive (TopStep is display-skipped in
// reconcileSharedWalletDisplayValues: no position source is wired, so it falls
// through `default: continue`). logTopStepCashflowJournalShadow computes the
// journal-derived expected equity and logs it every cycle for operator
// comparison; it mutates nothing. Flipping any TopStep alarm onto the journal is
// the follow-up (Phase 4b), gated on the shadow log proving the TopStep feed /
// equity field in production AND on the TopStepX fill/balance endpoint contracts
// (currently modeled on the existing adapter's unverified /v1/account/* shape)
// being confirmed against the live venue. Nothing here mutates StrategyState, so
// the whole flow runs OUTSIDE the state lock (DB writes are serialized by SQLite).

// topstepFillRecord is one TopStep settled trade fill (the single settled-cash
// event for TopStep). RealizedPnL is GROSS (0 for entry fills); Fee is the
// commission actually charged. The settled-cash delta is RealizedPnL−Fee
// (cashflowFillSettledDelta); RealizedPnL is retained as attribution metadata
// only and is never summed into equity on its own. Decodes straight from
// fetch_topstep_fills.py (json tags match) so there is no second representation.
type topstepFillRecord struct {
	FillID      string  `json:"fill_id"`
	TimeMs      int64   `json:"ts_ms"`
	Symbol      string  `json:"symbol"`
	Kind        string  `json:"kind"`
	RealizedPnL float64 `json:"realized_pnl"`
	Fee         float64 `json:"fee"`
}

// topstepFillKindByType maps a TopStep fill's reported `kind` to a human-readable
// journal kind. The settled-cash delta is RealizedPnL−Fee regardless of kind, so
// this map drives operator-readable labels and fail-closed classification: an
// UNMAPPED kind still books its authoritative gross−fee delta but latches the
// journal incomplete so the shadow log flags the unclassified event for the
// operator to extend this map before any Phase-4b alarm flip. An empty kind is a
// plain trade fill (the fills feed returns trades).
var topstepFillKindByType = map[string]string{
	"":           "trade",      // unlabeled fill from the trades feed
	"trade":      "trade",      // realized PnL + commission on a fill
	"fill":       "trade",      // synonym
	"commission": "commission", // standalone commission / fee
	"fee":        "commission", // synonym
}

// topstepFillSettledDelta returns one fill's signed effect on settled cash, its
// human-readable journal kind, and whether the event is fully classifiable. The
// delta is the shared gross-minus-fee convention (cashflowFillSettledDelta) for
// every record; the fill is "known" only when its kind is in
// topstepFillKindByType. A not-known result latches the journal incomplete (fail
// closed) but still books the gross−fee delta so the running drift surfaces it.
// Pure — unit-tested.
func topstepFillSettledDelta(f topstepFillRecord) (delta float64, kind string, known bool) {
	delta = cashflowFillSettledDelta(f.RealizedPnL, f.Fee)
	k := strings.ToLower(strings.TrimSpace(f.Kind))
	if mapped, ok := topstepFillKindByType[k]; ok {
		return delta, mapped, true
	}
	return delta, "kind_" + k, false
}

// topstepFillDedupID is a fill's stable journal identity. TopStep assigns each
// fill a unique id; the kind:time:symbol form disambiguates the rare missing-id
// case so two genuine fills never collide. Namespaced ("topstepfill:") so it can
// never collide with the HL fill/funding/transfer or OKX bill dedup namespaces in
// the shared cashflow_journal table.
func topstepFillDedupID(f topstepFillRecord) string {
	if id := strings.TrimSpace(f.FillID); id != "" && id != "0" {
		return "topstepfill:" + id
	}
	return fmt.Sprintf("topstepfill:%s:%d:%s", strings.TrimSpace(f.Kind), f.TimeMs, strings.ToUpper(strings.TrimSpace(f.Symbol)))
}

// topstepFillsScript is the path to the Python fills fetcher (#1106). Exposed as
// a var so tests can substitute — same pattern as okxFetchBillsScript /
// topstepFetchPositionsScript.
var topstepFillsScript = "shared_scripts/fetch_topstep_fills.py"

// topstepBalanceScript is the path to the Python balance fetcher (#1106).
var topstepBalanceScript = "shared_scripts/fetch_topstep_balance.py"

// defaultTopStepFillsFetcher is the production fills fetch: shells out to
// fetch_topstep_fills.py via RunTopStepFetchFills and returns the typed fills +
// the page-cap flag. Any subprocess/parse failure surfaces as a non-nil error so
// the journal fails closed (no cursor advance, no shadow reading this cycle).
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

// fetchTopStepAccountFills is a function variable so tests can stub the TopStep
// fills fetch without spawning Python. It pulls every fill settled since sinceMs
// (ascending), returning capped=true when the safety page cap was hit.
var fetchTopStepAccountFills = func(sinceMs int64) (fills []topstepFillRecord, capped bool, err error) {
	return defaultTopStepFillsFetcher(sinceMs)
}

// defaultTopStepEquitySnapshot returns a COHERENT (equity, uPnL) snapshot for the
// TopStep shared wallet from a SINGLE fetch_topstep_balance.py read (#1106).
// equity is the USD account value the journal reconciles; uPnL is equity −
// cashBalance from the same response, so the cash-flow journal's equity and uPnL
// are one atomic snapshot. Any failure surfaces as a non-nil error so the journal
// fails closed (no shadow reading this cycle).
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

// validatedTopStepEquity treats a non-positive (or NaN) equity as a fetch MISS
// rather than a real $0 account value. A funded TopStep account always carries
// positive equity, so equity ≤ 0 means the (unverified) /v1/account/balance
// response was malformed or shape-mismatched — e.g. a 200 missing the "equity"
// field, which the fetcher would otherwise coerce to 0 and report as success.
// Returning an error makes the caller skip the shadow journal this cycle instead
// of reconciling against a garbage $0 equity (which would emit phantom drift in
// the shadow log and corrupt the persisted baseline). The TopStep equity is
// shadow-only and never enters computeTotalPortfolioValue (see
// detectTopStepSharedWallet), so this guards journal correctness, not the live
// kill switch. A genuinely good positive equity flows through unchanged. Pure —
// unit-tested.
func validatedTopStepEquity(balance, upnl float64) (equity, upnlOut float64, err error) {
	if !(balance > 0) { // also rejects NaN
		return 0, 0, fmt.Errorf("non-positive TopStep equity $%.2f — treating as a fetch miss (malformed /v1/account/balance response)", balance)
	}
	return balance, upnl, nil
}

// topstepCashflowJournalFetchResult carries one TopStep wallet's raw fills plus
// the coherent equity / uPnL snapshot from the no-lock fetch phase to the no-lock
// ingest+compare phase. Mirrors okxCashflowJournalFetchResult (single stream).
type topstepCashflowJournalFetchResult struct {
	Key          SharedWalletKey
	State        CashflowJournalState
	StateFound   bool
	AccountValue float64 // TopStep equity (incl. uPnL) this cycle
	CurrentUPnL  float64 // uPnL component of equity this cycle (equity − cashBalance, same balance read)
	Fills        []topstepFillRecord
	FillsFetched bool
	Capped       bool
}

// fetchTopStepCashflowJournalEvents reads the wallet's fills cursor and pulls
// every fill since it (one subprocess). Runs OUTSIDE the state lock. First contact
// anchors the baseline to the supplied equity / uPnL snapshot and the cursor to
// now, fetching no history — pre-adoption movement belongs to the baseline, not
// the journal. Mirrors fetchOKXCashflowJournalEvents.
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
		// TopStep is single-stream: FillsSinceMs is the fills watermark;
		// FundingSinceMs/TransfersSinceMs stay 0 (unused for TopStep).
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

// ingestTopStepCashflowJournalEvents books the fetched fills into cashflow_journal
// and advances the fills cursor. DB-only (no StrategyState mutation), so it runs
// OUTSIDE the state lock. Returns the post-ingest state (cursor advanced,
// incomplete latched if an unclassifiable fill was seen). Every insert halts the
// cursor on persistence failure so a watermark never advances past an un-booked
// event. cutoffMs bounds ingestion to fills settled AT OR BEFORE the equity /
// uPnL snapshot — a fill settled after the snapshot is deferred to next cycle
// (else it would inflate expected-equity but not equity and read as a full cycle
// of false drift). Mirrors ingestOKXCashflowJournalEvents.
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
			continue // cursor-overlap boundary; dedup also covers this
		}
		if f.TimeMs > cutoffMs {
			continue // settled after the balance snapshot — defer to next cycle
		}
		delta, kind, known := topstepFillSettledDelta(f)
		if !known {
			// An unmapped fill kind means the journal cannot claim an exact total,
			// so a future Phase-4b alarm flip must fail closed. The row is still
			// booked (authoritative gross−fee delta) so it stays visible + deduped
			// and the running drift surfaces it.
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
	// A capped/truncated fetch is NOT a safe contiguous prefix at its final
	// millisecond: a same-ms group cut at maxTime would strand the un-returned
	// siblings behind the cursor forever (a standing offset in SumCashflowJournal).
	// When capped, advance only to maxTime so that boundary millisecond is
	// re-fetched next cycle; topstepFillDedupID / INSERT OR IGNORE absorbs the
	// re-read, and Usable stays false on every capped cycle. advanceCashflowCursor
	// clamps to the current cursor, so this never moves the watermark backwards.
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

// reconcileTopStepCashflowJournal runs the full journal flow for one TopStep
// shared wallet OUTSIDE the state lock: fetch the fills (subprocess), book them
// (DB-only, bounded to the snapshot), and reconstruct the wallet's expected
// equity. Pure of StrategyState. Returns nil when the wallet is not TopStep, the
// state load/init failed, or the baseline was just anchored this cycle. Reuses the
// HL cashflowJournalReconcile result shape. snapshotAt is the equity / uPnL sample
// time and bounds journal ingestion. Mirrors reconcileOKXCashflowJournal.
func reconcileTopStepCashflowJournal(sdb *StateDB, key SharedWalletKey, accountValue, currentUPnL float64, snapshotAt time.Time) *cashflowJournalReconcile {
	if sdb == nil || key.Platform != "topstep" || key.Account == "" {
		return nil
	}
	res := fetchTopStepCashflowJournalEvents(sdb, key, accountValue, currentUPnL, snapshotAt)
	if !res.StateFound {
		return nil // baseline just anchored (logged there) or fetch/load failed
	}
	st := ingestTopStepCashflowJournalEvents(sdb, res, snapshotAt.UnixMilli())
	rec := &cashflowJournalReconcile{Key: key, AccountValue: res.AccountValue, Incomplete: st.Incomplete}
	if !st.BaselineSet {
		return rec // un-anchored: not usable
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
	// Usable only when this cycle's reconstruction is complete: fills fetched, no
	// page-cap truncation (a cap leaves newer fills un-booked → would read as
	// drift), and no unclassifiable fill has latched the journal incomplete.
	rec.Usable = res.FillsFetched && !res.Capped && !st.Incomplete
	return rec
}

// logTopStepCashflowJournalShadow logs one TopStep wallet's journal-derived
// expected equity every cycle (SHADOW phase). It NEVER mutates driftResults —
// there is no live TopStep shared-wallet alarm (TopStep is display-skipped). The
// splitNote is "n/a" unless a TopStep drift result is ever wired in. Phase 4b will
// switch a real alarm onto the journal once this shadow log proves the TopStep
// fills feed / equity field in production. Mirrors logOKXCashflowJournalShadow.
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
