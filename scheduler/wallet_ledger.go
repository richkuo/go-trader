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

type WalletLedgerState struct {
	FundingSinceMs   int64
	TransfersSinceMs int64

	BaselineOffset float64
	BaselineSet    bool
}

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

type hlLedgerEventDelta struct {
	Type        string `json:"type"`
	Coin        string `json:"coin,omitempty"`
	USDC        string `json:"usdc,omitempty"`
	Fee         string `json:"fee,omitempty"`
	ToPerp      bool   `json:"toPerp,omitempty"`
	User        string `json:"user,omitempty"`
	Destination string `json:"destination,omitempty"`

	Token  string `json:"token,omitempty"`
	Amount string `json:"amount,omitempty"`

	SourceDex      string `json:"sourceDex,omitempty"`
	DestinationDex string `json:"destinationDex,omitempty"`

	NetWithdrawnUSD string `json:"netWithdrawnUsd,omitempty"`
}

type hlLedgerEvent struct {
	Time  int64              `json:"time"`
	Hash  string             `json:"hash"`
	Delta hlLedgerEventDelta `json:"delta"`
}

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

func signedPerpFlowUSD(d hlLedgerEventDelta, account string) (float64, bool) {
	usdc := parseHLFloat(d.USDC)
	switch d.Type {
	case "deposit":
		return usdc, true
	case "withdraw":

		return -(usdc + parseHLFloat(d.Fee)), true
	case "accountClassTransfer":

		if d.ToPerp {
			return usdc, true
		}
		return -usdc, true
	case "internalTransfer", "subAccountTransfer":

		if strings.EqualFold(d.Destination, account) {
			return usdc, true
		}
		return -(usdc + parseHLFloat(d.Fee)), true
	case "send":

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

		return parseHLFloat(d.NetWithdrawnUSD), true
	case "vaultDistribution":
		return usdc, true
	case "rewardsClaim":

		if strings.EqualFold(d.Token, "USDC") {
			return parseHLFloat(d.Amount), true
		}
		return 0, true
	case "spotTransfer", "spotGenesis", "cStakingTransfer",
		"gossipPriorityGasAuction", "deployGasAuction":

		return 0, true
	case "liquidation":

		return 0, true
	}
	return 0, false
}

type walletLedgerFetchResult struct {
	Key              SharedWalletKey
	State            WalletLedgerState
	StateFound       bool
	Funding          []hlLedgerEvent
	Transfers        []hlLedgerEvent
	FundingFetched   bool
	TransfersFetched bool
}

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

func fundingDedupID(ev hlLedgerEvent) string {
	return fmt.Sprintf("funding:%d:%s:%s", ev.Time, ev.Hash, ev.Delta.Coin)
}

func transferDedupID(ev hlLedgerEvent) string {
	return fmt.Sprintf("%s:%d:%s", ev.Delta.Type, ev.Time, ev.Hash)
}

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
				continue
			}
			if ok := ingestFundingEvent(sdb, state, key, ev, virtualQty); !ok {

				failedAt = ev.Time
				break
			}
			if ev.Time > maxTime {
				maxTime = ev.Time
			}
		}
		next := maxTime + 1
		if failedAt >= 0 && failedAt < next {
			next = failedAt
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
			next = failedAt
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

		if tradeRecorder != nil && len(ss.TradeHistory) > 0 && !ss.TradeHistory[len(ss.TradeHistory)-1].persisted {
			fmt.Printf("[WARN] wallet-ledger %s: funding row persist failed for %s — holding watermark for retry\n", sharedWalletKeyLabel(key), id)
			return false
		}
	}
	return true
}
