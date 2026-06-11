package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// #954 wallet-ledger ingestion: funding payments + non-trade cash flows.
//
// The shared-wallet display value is derived from the local trades ledger
// (initial_capital + Σ ledger deltas + owned uPnL). The real wallet balance
// also moves on events that produce no fill: hourly funding payments,
// deposits, withdrawals, and class/internal/sub-account transfers. Left
// unbooked, those flows would read as permanent ledger-vs-balance drift and
// the $0.01 alarm would fire forever.
//
//   - Funding on a coin some member owns → a trades row per owning member
//     (trade_type='funding', RealizedPnL=share, PnLGross=true), split by
//     virtual-quantity share exactly like unrealized PnL attribution.
//     Ownership is read at INGESTION time, not at the funding timestamp — an
//     acceptable approximation since ingestion runs every cycle and funding
//     accrues only while a position is open.
//   - Funding on a coin no member owns ("funding_orphan"), plus every
//     deposit/withdraw/transfer → a wallet_transfers row. The drift
//     comparison subtracts Σ wallet_transfers so these flows never read as
//     accounting bugs; they belong to no strategy's PnL.
//
// Fetch (HTTP, outside mu) and ingest (state mutation, under mu.Lock) are
// split so no network call ever runs under the state lock — same shape as
// buildCachedHyperliquidReconcileFillResolver. Watermarks advance only past
// processed events and only after their rows are durably inserted; the
// overlap on retry is absorbed by per-event dedup (trades: strategy_id +
// exchange_order_id existence; wallet_transfers: UNIQUE dedup_id).
//
// Scope: Hyperliquid shared wallets only. OKX shared wallets keep the #918
// capital-weight split (see reconcileSharedWalletDisplayValues).

// WalletLedgerState is one wallet's ingestion watermarks + drift baseline.
type WalletLedgerState struct {
	FundingSinceMs   int64
	TransfersSinceMs int64
	// BaselineOffset zeroes the ledger-vs-balance drift at adoption: history
	// before #954 lives in neither the trades ledger nor wallet_transfers, so
	// the first reconciled cycle stores (balance − Σ values − flows) here and
	// the alarm watches NEW divergence only. Reset to unset by
	// `backfill trade-ledger --apply` so the repaired ledger re-baselines.
	BaselineOffset float64
	BaselineSet    bool
}

// GetWalletLedgerState loads one wallet's ledger state; found=false when the
// wallet has never been ingested.
func (sdb *StateDB) GetWalletLedgerState(platform, account string) (WalletLedgerState, bool, error) {
	var st WalletLedgerState
	if sdb == nil || sdb.db == nil {
		return st, false, fmt.Errorf("state db unavailable")
	}
	var baselineSet int
	err := sdb.db.QueryRow(
		`SELECT funding_since_ms, transfers_since_ms, baseline_offset_usd, baseline_set
		 FROM wallet_ledger_state WHERE platform = ? AND account = ?`,
		platform, account).Scan(&st.FundingSinceMs, &st.TransfersSinceMs, &st.BaselineOffset, &baselineSet)
	if err == sql.ErrNoRows {
		return st, false, nil
	}
	if err != nil {
		return st, false, fmt.Errorf("load wallet ledger state: %w", err)
	}
	st.BaselineSet = baselineSet != 0
	return st, true, nil
}

// UpsertWalletLedgerState writes one wallet's ledger state row.
func (sdb *StateDB) UpsertWalletLedgerState(platform, account string, st WalletLedgerState) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	baselineSet := 0
	if st.BaselineSet {
		baselineSet = 1
	}
	_, err := sdb.db.Exec(
		`INSERT INTO wallet_ledger_state (platform, account, funding_since_ms, transfers_since_ms, baseline_offset_usd, baseline_set)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(platform, account) DO UPDATE SET
		   funding_since_ms = excluded.funding_since_ms,
		   transfers_since_ms = excluded.transfers_since_ms,
		   baseline_offset_usd = excluded.baseline_offset_usd,
		   baseline_set = excluded.baseline_set`,
		platform, account, st.FundingSinceMs, st.TransfersSinceMs, st.BaselineOffset, baselineSet)
	if err != nil {
		return fmt.Errorf("upsert wallet ledger state: %w", err)
	}
	return nil
}

// ResetWalletLedgerBaseline marks ONE wallet's drift baseline unset so the
// next reconciled cycle recomputes it. Called by `backfill trade-ledger
// --apply` for each wallet whose members were repaired: the repaired ledger
// changes that wallet's Σ member values, so its old offset would misread the
// correction as drift. Deliberately scoped — clearing an untouched wallet's
// baseline would fold its genuine standing drift into the new offset and
// silence a real alarm.
func (sdb *StateDB) ResetWalletLedgerBaseline(platform, account string) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	if _, err := sdb.db.Exec(
		`UPDATE wallet_ledger_state SET baseline_set = 0, baseline_offset_usd = 0 WHERE platform = ? AND account = ?`,
		platform, account); err != nil {
		return fmt.Errorf("reset wallet ledger baseline: %w", err)
	}
	return nil
}

// InsertWalletTransfer appends one non-trade flow row; duplicate dedup_id is
// silently ignored (watermark-overlap re-reads).
func (sdb *StateDB) InsertWalletTransfer(platform, account string, timeMs int64, kind string, amountUSD float64, dedupID string) error {
	if sdb == nil || sdb.db == nil {
		return fmt.Errorf("state db unavailable")
	}
	_, err := sdb.db.Exec(
		`INSERT OR IGNORE INTO wallet_transfers (platform, account, time_ms, kind, amount_usd, dedup_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		platform, account, timeMs, kind, amountUSD, dedupID)
	if err != nil {
		return fmt.Errorf("insert wallet transfer: %w", err)
	}
	return nil
}

// SumWalletTransfers returns the signed total of one wallet's non-trade flows.
func (sdb *StateDB) SumWalletTransfers(platform, account string) (float64, error) {
	if sdb == nil || sdb.db == nil {
		return 0, fmt.Errorf("state db unavailable")
	}
	var sum sql.NullFloat64
	err := sdb.db.QueryRow(
		`SELECT SUM(amount_usd) FROM wallet_transfers WHERE platform = ? AND account = ?`,
		platform, account).Scan(&sum)
	if err != nil {
		return 0, fmt.Errorf("sum wallet transfers: %w", err)
	}
	return sum.Float64, nil
}

// hlLedgerEventDelta is the union of the HL info-endpoint delta payloads we
// consume from userFunding and userNonFundingLedgerUpdates. Numeric fields
// arrive as strings (same encoding as userFills).
type hlLedgerEventDelta struct {
	Type        string `json:"type"`
	Coin        string `json:"coin,omitempty"`
	USDC        string `json:"usdc,omitempty"`
	Fee         string `json:"fee,omitempty"`
	ToPerp      bool   `json:"toPerp,omitempty"`
	User        string `json:"user,omitempty"`
	Destination string `json:"destination,omitempty"`
	// Token-denominated deltas (send, rewardsClaim, spot/staking kinds).
	Token  string `json:"token,omitempty"`
	Amount string `json:"amount,omitempty"`
	// send: non-empty when routed through a builder dex (not core USDC).
	SourceDex      string `json:"sourceDex,omitempty"`
	DestinationDex string `json:"destinationDex,omitempty"`
	// vaultWithdraw carries NO usdc field — the perps account is credited
	// netWithdrawnUsd (= requestedUsd − commission − closingCost).
	NetWithdrawnUSD string `json:"netWithdrawnUsd,omitempty"`
}

// hlLedgerEvent is one userFunding / userNonFundingLedgerUpdates entry.
type hlLedgerEvent struct {
	Time  int64              `json:"time"`
	Hash  string             `json:"hash"`
	Delta hlLedgerEventDelta `json:"delta"`
}

// fetchHyperliquidUserFunding / fetchHyperliquidLedgerUpdates are function
// variables so tests stub the HTTP layer (mirrors fetchHyperliquidUserFillsByTime).
var fetchHyperliquidUserFunding = func(accountAddress string, startTimeMs int64) ([]hlLedgerEvent, error) {
	return fetchHLLedgerEndpoint("userFunding", accountAddress, startTimeMs)
}

var fetchHyperliquidLedgerUpdates = func(accountAddress string, startTimeMs int64) ([]hlLedgerEvent, error) {
	return fetchHLLedgerEndpoint("userNonFundingLedgerUpdates", accountAddress, startTimeMs)
}

func fetchHLLedgerEndpoint(infoType, accountAddress string, startTimeMs int64) ([]hlLedgerEvent, error) {
	if accountAddress == "" {
		return nil, fmt.Errorf("HYPERLIQUID_ACCOUNT_ADDRESS not set")
	}
	payload := map[string]any{
		"type":      infoType,
		"user":      accountAddress,
		"startTime": startTimeMs,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(hlMainnetURL+"/info", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d from %s (%s)", resp.StatusCode, hlMainnetURL, infoType)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var events []hlLedgerEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return nil, fmt.Errorf("parse %s response: %w", infoType, err)
	}
	return events, nil
}

// signedPerpFlowUSD maps a non-funding ledger delta to its SIGNED effect on
// the PERPS account balance (the clearinghouseState accountValue the
// shared-wallet reconcile compares against). ok=false marks kinds we do not
// recognize or that do not move the perps balance — the caller records a
// zero-amount row so the event is visible (and shows up as drift, pointing
// the operator at the unmapped kind) instead of being silently mis-signed.
func signedPerpFlowUSD(d hlLedgerEventDelta, account string) (float64, bool) {
	usdc := parseHLFloat(d.USDC)
	switch d.Type {
	case "deposit":
		return usdc, true
	case "withdraw":
		// VERIFIED against mainnet userNonFundingLedgerUpdates samples
		// (2026-06): `usdc` is the NET amount (post-fee) and `fee` is
		// debited ON TOP — real entries read usdc=1999999.0/999.0/9.0 with
		// fee=1.0, i.e. round requested amounts minus the $1 fee. Total
		// account debit = usdc + fee = the requested amount.
		return -(usdc + parseHLFloat(d.Fee)), true
	case "accountClassTransfer":
		// Spot ↔ perps transfer inside the same account.
		if d.ToPerp {
			return usdc, true
		}
		return -usdc, true
	case "internalTransfer", "subAccountTransfer":
		// Outbound legs may carry a fee (mainnet samples show fee=0.0, but
		// mirror the withdraw convention defensively: usdc is what the peer
		// receives, the fee is debited on top). subAccountTransfer has no
		// fee field → parseHLFloat("") = 0.
		if strings.EqualFold(d.Destination, account) {
			return usdc, true
		}
		return -(usdc + parseHLFloat(d.Fee)), true
	case "send":
		// Unified transfer primitive. Core USDC sends (no builder dex on
		// either side) move the perps balance exactly like internalTransfer;
		// non-USDC tokens are spot-side. Dex-routed USDC falls through to
		// unmapped (visible via the $0 row + drift) rather than guessing.
		if !strings.EqualFold(d.Token, "USDC") {
			return 0, true
		}
		if d.SourceDex == "" && d.DestinationDex == "" {
			amt := parseHLFloat(d.Amount)
			if strings.EqualFold(d.Destination, account) {
				return amt, true
			}
			return -(amt + parseHLFloat(d.Fee)), true
		}
		return 0, false
	case "vaultDeposit", "vaultCreate":
		return -(usdc + parseHLFloat(d.Fee)), true
	case "vaultWithdraw":
		// No usdc field on this kind — the account is credited the net
		// amount after vault commission and position closing costs.
		return parseHLFloat(d.NetWithdrawnUSD), true
	case "vaultDistribution":
		return usdc, true
	case "rewardsClaim":
		// USDC rewards land in the perps balance; token rewards are spot-side.
		if strings.EqualFold(d.Token, "USDC") {
			return parseHLFloat(d.Amount), true
		}
		return 0, true
	case "spotTransfer", "spotGenesis", "cStakingTransfer",
		"gossipPriorityGasAuction", "deployGasAuction":
		// Spot/staking-side token movement; perps accountValue unaffected.
		return 0, true
	case "liquidation":
		// Informational (no USDC amount on the delta). The balance impact of
		// a liquidation arrives through its fills, which the reconciler books
		// as external closes — recording a flow here would double-count.
		return 0, true
	}
	return 0, false
}

// walletLedgerFetchResult carries one wallet's raw ledger events from the
// no-lock fetch phase to the locked ingest phase.
type walletLedgerFetchResult struct {
	Key              SharedWalletKey
	State            WalletLedgerState
	StateFound       bool
	Funding          []hlLedgerEvent
	Transfers        []hlLedgerEvent
	FundingFetched   bool
	TransfersFetched bool
}

// fetchWalletLedgerEvents reads the wallet's watermarks and pulls funding +
// non-funding ledger events since each. Runs OUTSIDE the state lock (two
// HTTP POSTs). First contact with a wallet initializes the watermarks to now
// and fetches nothing — history before adoption is part of the drift
// baseline, not the ledger.
func fetchWalletLedgerEvents(sdb *StateDB, key SharedWalletKey, now time.Time) walletLedgerFetchResult {
	res := walletLedgerFetchResult{Key: key}
	if sdb == nil || key.Platform != "hyperliquid" || key.Account == "" {
		return res
	}
	st, found, err := sdb.GetWalletLedgerState(key.Platform, key.Account)
	if err != nil {
		fmt.Printf("[WARN] wallet-ledger %s: state load failed: %v — skipping ingestion this cycle\n", sharedWalletKeyLabel(key), err)
		return res
	}
	if !found {
		st = WalletLedgerState{FundingSinceMs: now.UnixMilli(), TransfersSinceMs: now.UnixMilli()}
		if err := sdb.UpsertWalletLedgerState(key.Platform, key.Account, st); err != nil {
			fmt.Printf("[WARN] wallet-ledger %s: watermark init failed: %v\n", sharedWalletKeyLabel(key), err)
			return res
		}
		fmt.Printf("[wallet-ledger] %s: initialized funding/transfer watermarks at %s (no historical replay)\n",
			sharedWalletKeyLabel(key), now.UTC().Format(time.RFC3339))
	}
	res.State = st
	res.StateFound = true

	if funding, err := fetchHyperliquidUserFunding(key.Account, st.FundingSinceMs); err != nil {
		fmt.Printf("[WARN] wallet-ledger %s: userFunding fetch failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
	} else {
		res.Funding = funding
		res.FundingFetched = true
	}
	if transfers, err := fetchHyperliquidLedgerUpdates(key.Account, st.TransfersSinceMs); err != nil {
		fmt.Printf("[WARN] wallet-ledger %s: userNonFundingLedgerUpdates fetch failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
	} else {
		res.Transfers = transfers
		res.TransfersFetched = true
	}
	return res
}

// fundingDedupID is the trades.exchange_order_id stamped on funding rows —
// per-event identity so watermark-overlap re-reads insert nothing twice.
// The owning strategy is implicit (existence is checked per strategy_id).
func fundingDedupID(ev hlLedgerEvent) string {
	return fmt.Sprintf("funding:%d:%s:%s", ev.Time, ev.Hash, ev.Delta.Coin)
}

// transferDedupID keys wallet_transfers rows. Hash+type disambiguates the
// two legs HL emits for some transfer kinds at the same timestamp.
func transferDedupID(ev hlLedgerEvent) string {
	return fmt.Sprintf("%s:%d:%s", ev.Delta.Type, ev.Time, ev.Hash)
}

// ingestWalletLedgerEvents books the fetched events: funding → trades rows
// for owning members (split by virtual-quantity share) or a funding_orphan
// transfer row when no member owns the coin; non-funding deltas →
// wallet_transfers rows. Watermarks advance only past processed events.
//
// MUST run under the state write lock: it mutates StrategyState.TradeHistory
// via RecordTrade. DB writes are immediate (RecordTrade → InsertTrade hook;
// transfers insert directly), and EVERY insert path — funding-owner rows
// included — halts the watermark on persist failure, so a crash before the
// watermark write only causes a re-read that the dedup keys absorb. A
// watermark must never advance past an event whose row is not durably
// persisted.
func ingestWalletLedgerEvents(sdb *StateDB, state *AppState, res walletLedgerFetchResult, virtualQty map[string]map[string]float64) {
	if sdb == nil || !res.StateFound {
		return
	}
	key := res.Key
	st := res.State

	if res.FundingFetched {
		maxTime := st.FundingSinceMs - 1
		failedAt := int64(-1)
		events := append([]hlLedgerEvent(nil), res.Funding...)
		sort.SliceStable(events, func(i, j int) bool { return events[i].Time < events[j].Time })
		for _, ev := range events {
			if ev.Time < st.FundingSinceMs {
				continue // endpoint may return boundary events; dedup also covers this
			}
			if ok := ingestFundingEvent(sdb, state, key, ev, virtualQty); !ok {
				// Persist failure: stop, and remember WHERE — the watermark
				// must not advance to or past this event's timestamp even if
				// a same-ms sibling already succeeded (maxTime == ev.Time
				// would otherwise skip it forever).
				failedAt = ev.Time
				break
			}
			if ev.Time > maxTime {
				maxTime = ev.Time
			}
		}
		next := maxTime + 1
		if failedAt >= 0 && failedAt < next {
			next = failedAt // re-fetch from the failed event; dedup absorbs same-ms re-reads
		}
		if next > st.FundingSinceMs {
			st.FundingSinceMs = next
		}
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
				fmt.Printf("[WARN] wallet-ledger %s: unmapped ledger delta type %q (hash %s) — recorded with $0 effect; balance drift will surface it\n",
					sharedWalletKeyLabel(key), ev.Delta.Type, ev.Hash)
			}
			if err := sdb.InsertWalletTransfer(key.Platform, key.Account, ev.Time, ev.Delta.Type, amount, transferDedupID(ev)); err != nil {
				fmt.Printf("[WARN] wallet-ledger %s: transfer insert failed: %v — retrying next cycle\n", sharedWalletKeyLabel(key), err)
				failedAt = ev.Time
				break
			}
			if ev.Time > maxTime {
				maxTime = ev.Time
			}
		}
		next := maxTime + 1
		if failedAt >= 0 && failedAt < next {
			next = failedAt // same-ms sibling may have succeeded; never skip the failed event
		}
		if next > st.TransfersSinceMs {
			st.TransfersSinceMs = next
		}
	}

	if st != res.State {
		if err := sdb.UpsertWalletLedgerState(key.Platform, key.Account, st); err != nil {
			fmt.Printf("[WARN] wallet-ledger %s: watermark advance failed: %v\n", sharedWalletKeyLabel(key), err)
		}
	}
}

// ingestSharedWalletLedgers books every HL shared wallet's fetched ledger
// events. MUST run under the state write lock, BEFORE
// reconcileSharedWalletDisplayValues, so this cycle's funding rows are in the
// ledger sums that back this cycle's display values (the balance being
// reconciled already includes them).
func ingestSharedWalletLedgers(
	sdb *StateDB,
	state *AppState,
	strategies []StrategyConfig,
	sharedWallets map[SharedWalletKey][]string,
	fetches []walletLedgerFetchResult,
) {
	if sdb == nil || len(fetches) == 0 {
		return
	}
	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
	}
	for _, res := range fetches {
		if !res.StateFound {
			continue
		}
		members := sharedWalletMembersWithManual(res.Key, sharedWallets[res.Key], strategies)
		_, virtualQty := buildSharedWalletBooks(res.Key, members, byID, state)
		ingestWalletLedgerEvents(sdb, state, res, virtualQty)
	}
}

// ingestFundingEvent books one funding payment. Returns false only on a
// persistence error (caller halts the watermark before this event).
func ingestFundingEvent(sdb *StateDB, state *AppState, key SharedWalletKey, ev hlLedgerEvent, virtualQty map[string]map[string]float64) bool {
	amount := parseHLFloat(ev.Delta.USDC)
	coin := strings.ToUpper(strings.TrimSpace(ev.Delta.Coin))
	dedupID := fundingDedupID(ev)

	owners := virtualQty[coin]
	sumQty := 0.0
	for _, qty := range owners {
		if qty > 0 {
			sumQty += qty
		}
	}
	if sumQty <= 0 {
		// No member owns the coin at ingestion time (closed since the payment,
		// or a genuinely foreign position) — keep the flow at the wallet level
		// so the drift comparison stays clean.
		if err := sdb.InsertWalletTransfer(key.Platform, key.Account, ev.Time, "funding_orphan", amount, "funding_orphan:"+dedupID); err != nil {
			fmt.Printf("[WARN] wallet-ledger %s: funding_orphan insert failed: %v\n", sharedWalletKeyLabel(key), err)
			return false
		}
		return true
	}

	ids := make([]string, 0, len(owners))
	for id, qty := range owners {
		if qty > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		ss := state.Strategies[id]
		if ss == nil {
			continue
		}
		// Dedup against both the DB and the in-memory history: a row whose
		// immediate persist failed is invisible to the DB check until the next
		// SaveState flush, but it is already in TradeHistory.
		exists, err := sdb.HasTradeWithExchangeOrderID(id, dedupID)
		if err != nil {
			fmt.Printf("[WARN] wallet-ledger %s: funding dedup check failed for %s: %v\n", sharedWalletKeyLabel(key), id, err)
			return false
		}
		if !exists {
			for i := len(ss.TradeHistory) - 1; i >= 0; i-- {
				if ss.TradeHistory[i].ExchangeOrderID != dedupID {
					continue
				}
				exists = true
				// The row exists in memory but its eager persist failed
				// (SaveState will retry it). Don't re-book — but don't let
				// the watermark advance past an event that is not yet on
				// disk either: a crash before the flush would lose it
				// behind an advanced watermark (permanent ledger shortfall,
				// alarm forever). Only enforced under eager persistence;
				// with tradeRecorder unset, rows are flushed in batch and
				// persisted is legitimately false.
				if tradeRecorder != nil && !ss.TradeHistory[i].persisted {
					fmt.Printf("[WARN] wallet-ledger %s: funding row for %s still awaiting persist — holding watermark\n", sharedWalletKeyLabel(key), id)
					return false
				}
				break
			}
		}
		if exists {
			continue
		}
		share := amount * (owners[id] / sumQty)
		trade := Trade{
			Timestamp:       time.UnixMilli(ev.Time).UTC(),
			StrategyID:      id,
			Symbol:          coin,
			Side:            "funding",
			TradeType:       TradeTypeFunding,
			Details:         fmt.Sprintf("Funding payment $%+.4f on %s (qty share %.4f/%.4f)", share, coin, owners[id], sumQty),
			ExchangeOrderID: dedupID,
			RealizedPnL:     share,
			PnLGross:        true,
		}
		RecordTrade(ss, trade)
		// RecordTrade is void: on eager-insert failure it logs, leaves the
		// row persisted=false, and SaveState retries later. Mirror the
		// transfer/orphan paths and hold the watermark until the row is
		// durably on disk (already-persisted co-owners dedup-skip on retry).
		if tradeRecorder != nil && len(ss.TradeHistory) > 0 && !ss.TradeHistory[len(ss.TradeHistory)-1].persisted {
			fmt.Printf("[WARN] wallet-ledger %s: funding row persist failed for %s — holding watermark for retry\n", sharedWalletKeyLabel(key), id)
			return false
		}
	}
	return true
}
