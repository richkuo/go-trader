package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// okxFetchBillsScript is the path to the Python account-bills fetcher (#1105).
// Exposed as a var so tests can substitute — same pattern as
// okxFetchPositionsScript / okxBalanceScript.
var okxFetchBillsScript = "shared_scripts/fetch_okx_bills.py"

// defaultOKXAccountBillsFetcher is the production bills fetch: shells out to
// fetch_okx_bills.py via RunOKXFetchBills and returns the typed bills + the
// page-cap flag. Any subprocess/parse failure surfaces as a non-nil error so
// the journal fails closed (no cursor advance, no shadow reading this cycle).
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

// #1105: exchange-sourced cash-flow journal for OKX shared wallets — SHADOW phase
// (Phase 3a of #1100, mirroring HL's #1103 shadow / #1104 flip two-step).
//
// This reuses the platform-generic journal machinery built for Hyperliquid in
// children 1–2 (cashflow_journal.go): the cashflow_journal / cashflow_journal_state
// tables (keyed by (platform, account)), the durable per-wallet cursor + adoption
// baseline, per-event dedup, halt-on-persist-failure, snapshot-bounded ingestion,
// the incomplete fail-closed latch, and the shared equity equation
// (cashflowJournalExpectedEquity). HL behavior is untouched.
//
// DELIBERATE DIVERGENCE FROM HL'S THREE-STREAM SHAPE. HL reconstructs settled
// cash from three separate streams (userFills + userFunding + ledgerUpdates) and
// must compute each fill's cash delta as realized-PnL-gross minus the fee
// actually charged (#698 convention). OKX exposes a SINGLE settled-cash-flow
// source — the account bills feed (ccxt fetch_ledger → /api/v5/account/bills):
// EVERY balance change is a bill carrying `balChg`, the signed change to the
// settled cash balance, already netting trade PnL, fees, funding, transfers,
// deposits and withdrawals. So OKX needs no per-fill fee arithmetic and no
// gross-PnL handling at all — `balChg` IS the settled-cash delta. The
// venue-reported `pnl`/`fee` fields are retained as attribution metadata only
// and are NEVER summed into equity (same gross-PnL discipline as HL).
//
// Because OKX is single-stream, only the FillsSinceMs cursor of the shared
// CashflowJournalState is used (as the bills watermark); FundingSinceMs /
// TransfersSinceMs stay 0 and unused for OKX wallets.
//
// Equity decomposition is identical to HL (eq = settled cash + unrealized PnL):
//
//	expected = baseline_eq
//	         + Σ(balChg since baseline)        // every settled-cash movement
//	         + (current_uPnL − baseline_uPnL)
//
// The reconciled field is OKX `eq` (ccxt balance["total"]["USDT"], equity =
// cash + uPnL — the same field fetch_okx_balance.py already reports and the
// #918 capital-weight split already reconciles against). current_uPnL /
// baseline_uPnL come from the SAME balance read as eq (uPnL = eq − cashBal,
// not a separately-timed fetch_positions sum), so eq and uPnL are one coherent
// snapshot: the uPnL term cancels against eq and residual drift reduces to the
// pure settled-cash reconstruction error (cashBal − baseline_cashBal − Σ balChg)
// — no intra-cycle uPnL jitter. The journal is USDT-settlement only: a non-USDT
// bill with a non-zero balChg cannot be reconciled against the USDT-denominated
// eq, so it contributes $0 and latches the journal incomplete (fail closed).
//
// SHADOW ONLY. This phase NEVER drives the OKX drift alarm — the live OKX alarm
// stays on the #918 capital-weight split (reconcileSharedWalletMemberValues).
// logOKXCashflowJournalShadow computes the journal-derived expected equity and
// logs it next to the capital-weight drift every cycle for operator comparison;
// it does not mutate any driftResult. Flipping the OKX alarm onto the journal is
// the follow-up (Phase 3b), gated on the shadow log proving the OKX feed/field
// in production first. Nothing here mutates StrategyState, so the whole flow runs
// OUTSIDE the state lock (DB writes are serialized by SQLite).

// okxJournalSettlementCcy is the only bill currency the OKX journal reconciles.
// OKX-USDT-margined perps settle in USDT and the reconciled eq is USDT-
// denominated, so a bill in any other currency (e.g. an OKB fee discount)
// cannot be summed against eq — it contributes $0 and latches incomplete.
const okxJournalSettlementCcy = "USDT"

// okxBillRecord is one OKX account-bill (a ccxt fetch_ledger entry's raw
// `info`), the single settled-cash-flow event for OKX. BalChg is the signed
// change to the settled cash balance and is authoritative; Pnl/Fee are
// attribution metadata only (never summed into equity).
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

// okxBillKindByType maps OKX bill `type` codes to human-readable journal kinds
// (OKX /api/v5/account/bills `type` enum). The settled-cash delta comes from
// `balChg` regardless of type, so this map is for operator-readable labels and
// drift visibility; an UNMAPPED type still books its authoritative balChg but
// latches the journal incomplete so the shadow log flags the unclassified event
// for the operator to extend this map before any Phase-3b alarm flip.
var okxBillKindByType = map[string]string{
	"1":  "transfer",          // funding-account ⇄ trading-account transfer
	"2":  "trade",             // realized trade pnl / fee on a fill
	"3":  "delivery",          // futures/options delivery
	"4":  "auto_margin_buy",   // forced margin replenishment
	"5":  "liquidation",       // forced liquidation
	"6":  "margin_transfer",   // margin add/remove
	"7":  "interest_deduct",   // interest deduction
	"8":  "funding_fee",       // perp funding payment/receipt
	"9":  "adl",               // auto-deleveraging
	"10": "clawback",          // socialized loss clawback
	"11": "system_token_conv", // system token conversion
	"12": "strategy_transfer", // strategy account transfer
	"13": "ddh",               // delta dynamic hedging
	"14": "settlement",        // position settlement
	"22": "repay_forced",      // forced repayment
}

// okxBillSettledDelta returns one bill's signed effect on settled cash, its
// human-readable journal kind, and whether the event is fully classifiable.
// For a USDT bill the delta is its authoritative balChg; the bill is "known"
// only when its type is in okxBillKindByType. A non-USDT bill contributes $0
// (it cannot reconcile against the USDT eq) and is never known. A not-known
// result latches the journal incomplete (fail closed). Pure — unit-tested.
func okxBillSettledDelta(b okxBillRecord) (delta float64, kind string, known bool) {
	ccy := strings.ToUpper(strings.TrimSpace(b.Ccy))
	if ccy != "" && ccy != okxJournalSettlementCcy {
		// Cross-currency: cannot be expressed against the USDT-denominated eq.
		return 0, "nonsettle_" + strings.ToLower(ccy), false
	}
	if mapped, ok := okxBillKindByType[strings.TrimSpace(b.Type)]; ok {
		return b.BalChg, mapped, true
	}
	// Unknown type: balChg is still authoritative so it is booked and summed,
	// but the event is unclassified → latch incomplete for operator review.
	return b.BalChg, "type_" + strings.TrimSpace(b.Type), false
}

// okxBillDedupID is a bill's stable journal identity. OKX assigns each bill a
// unique billId; the type:time:tradeId form disambiguates the rare missing-id
// case so two genuine bills never collide. Namespaced ("okxbill:") so it can
// never collide with the HL fill/funding/transfer dedup namespaces in the
// shared cashflow_journal table.
func okxBillDedupID(b okxBillRecord) string {
	if id := strings.TrimSpace(b.BillID); id != "" && id != "0" {
		return "okxbill:" + id
	}
	return fmt.Sprintf("okxbill:%s:%d:%s", strings.TrimSpace(b.Type), b.TimeMs, strings.TrimSpace(b.TradeID))
}

// fetchOKXAccountBills is a function variable so tests can stub the OKX bills
// fetch without spawning Python. It pulls every account bill settled since
// sinceMs (ascending), returning capped=true when the safety page cap was hit
// (a pathological backlog — the caller treats that cycle as not-usable but
// still advances past the contiguous prefix it did receive).
var fetchOKXAccountBills = func(sinceMs int64) (bills []okxBillRecord, capped bool, err error) {
	return defaultOKXAccountBillsFetcher(sinceMs)
}

// okxCashflowJournalFetchResult carries one OKX wallet's raw bills plus the
// coherent eq / uPnL snapshot from the no-lock fetch phase to the no-lock
// ingest+compare phase. Mirrors cashflowJournalFetchResult (HL) but single
// stream.
type okxCashflowJournalFetchResult struct {
	Key          SharedWalletKey
	State        CashflowJournalState
	StateFound   bool
	AccountValue float64 // OKX eq (equity incl. uPnL) this cycle
	CurrentUPnL  float64 // uPnL component of eq this cycle (eq − cashBal, same balance read)
	Bills        []okxBillRecord
	BillsFetched bool
	Capped       bool
}

// fetchOKXCashflowJournalEvents reads the wallet's bills cursor and pulls every
// account bill since it (one subprocess). Runs OUTSIDE the state lock. First
// contact anchors the baseline to the supplied eq / uPnL snapshot and the
// cursor to now, fetching no history — pre-adoption movement belongs to the
// baseline, not the journal. Mirrors fetchCashflowJournalEvents (HL).
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
		// OKX is single-stream: FillsSinceMs is the bills watermark;
		// FundingSinceMs/TransfersSinceMs stay 0 (unused for OKX).
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

// ingestOKXCashflowJournalEvents books the fetched bills into cashflow_journal
// and advances the bills cursor. DB-only (no StrategyState mutation), so it
// runs OUTSIDE the state lock. Returns the post-ingest state (cursor advanced,
// incomplete latched if an unclassifiable bill was seen). Every insert halts
// the cursor on persistence failure so a watermark never advances past an
// un-booked event. cutoffMs bounds ingestion to bills settled AT OR BEFORE the
// eq / uPnL snapshot — a bill settled after the snapshot is deferred to next
// cycle (else it would inflate expected-equity but not eq and read as a full
// cycle of false drift). Mirrors ingestCashflowJournalEvents (HL).
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
			continue // cursor-overlap boundary; dedup also covers this
		}
		if b.TimeMs > cutoffMs {
			continue // settled after the balance snapshot — defer to next cycle
		}
		delta, kind, known := okxBillSettledDelta(b)
		if !known {
			// An unmapped bill type or a non-USDT bill means the journal cannot
			// claim an exact total, so a future Phase-3b alarm flip must fail
			// closed. The row is still booked (authoritative balChg for a known
			// currency, $0 for cross-currency) so it stays visible + deduped and
			// the running drift surfaces it.
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
	// A capped/truncated fetch is NOT a safe contiguous prefix at its final
	// millisecond: BOTH the single-ms-overflow cap (returns only the first
	// page_limit of a >page_limit same-ms block) AND the max_bills bills[:N]
	// truncation can cut a same-ms group at maxTime. Advancing to maxTime+1 would
	// then strand the un-returned siblings behind the cursor forever — a standing
	// offset in SumCashflowJournal that survives into a Phase-3b alarm flip. When
	// capped, advance only to maxTime so that boundary millisecond is re-fetched
	// next cycle; the okxBillDedupID / INSERT OR IGNORE dedup absorbs the re-read,
	// and a genuinely un-pageable >page_limit same-ms block pins fail-closed at
	// that ms (Usable stays false on every capped cycle) instead of corrupting the
	// sum. advanceCashflowCursor still clamps to the current cursor, so this never
	// moves the watermark backwards.
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

// reconcileOKXCashflowJournal runs the full journal flow for one OKX shared
// wallet OUTSIDE the state lock: fetch the account bills (subprocess), book them
// (DB-only, bounded to the snapshot), and reconstruct the wallet's expected
// equity. Pure of StrategyState. Returns nil when the wallet is not OKX, the
// state load/init failed, or the baseline was just anchored this cycle. Reuses
// the HL cashflowJournalReconcile result shape. snapshotAt is the eq / uPnL
// sample time and bounds journal ingestion. Mirrors reconcileCashflowJournal.
func reconcileOKXCashflowJournal(sdb *StateDB, key SharedWalletKey, accountValue, currentUPnL float64, snapshotAt time.Time) *cashflowJournalReconcile {
	if sdb == nil || key.Platform != "okx" || key.Account == "" {
		return nil
	}
	res := fetchOKXCashflowJournalEvents(sdb, key, accountValue, currentUPnL, snapshotAt)
	if !res.StateFound {
		return nil // baseline just anchored (logged there) or fetch/load failed
	}
	st := ingestOKXCashflowJournalEvents(sdb, res, snapshotAt.UnixMilli())
	rec := &cashflowJournalReconcile{Key: key, AccountValue: res.AccountValue, Incomplete: st.Incomplete}
	if !st.BaselineSet {
		return rec // un-anchored: not usable
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
	// Usable only when this cycle's reconstruction is complete: bills fetched, no
	// page-cap truncation (a cap leaves newer bills un-booked → would read as
	// drift), and no unclassifiable bill has latched the journal incomplete.
	rec.Usable = res.BillsFetched && !res.Capped && !st.Incomplete
	return rec
}

// logOKXCashflowJournalShadow logs one OKX wallet's journal-derived expected
// equity next to the live capital-weight drift every cycle (SHADOW phase). It
// NEVER mutates driftResults — the OKX drift alarm stays on the #918 capital-
// weight split. Phase 3b will switch the alarm onto the journal once this shadow
// log proves the OKX bills feed / eq field in production. Mirrors the per-cycle
// comparison log of applyCashflowJournalDriftBasis (HL) without the basis flip.
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
