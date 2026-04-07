package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// HLPosition represents an on-chain Hyperliquid perps position.
type HLPosition struct {
	Coin       string
	Size       float64 // signed: positive = long, negative = short
	EntryPrice float64
}

var hlMainnetURL = "https://api.hyperliquid.xyz"

// fetchHyperliquidBalance fetches the live USDC balance (accountValue) from
// the Hyperliquid clearinghouseState endpoint for a given address.
// Returns 0 and a non-nil error if the request fails or the response is unexpected.
func fetchHyperliquidBalance(accountAddress string) (float64, error) {
	payload := map[string]string{
		"type": "clearinghouseState",
		"user": accountAddress,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(hlMainnetURL+"/info", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("http %d from %s", resp.StatusCode, hlMainnetURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}

	var result struct {
		MarginSummary struct {
			AccountValue string `json:"accountValue"`
		} `json:"marginSummary"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("parse response: %w", err)
	}

	val, err := strconv.ParseFloat(result.MarginSummary.AccountValue, 64)
	if err != nil {
		return 0, fmt.Errorf("parse accountValue %q: %w", result.MarginSummary.AccountValue, err)
	}
	return val, nil
}

// syncHyperliquidLiveCapital checks if a strategy is a live Hyperliquid strategy
// and if so, fetches the real account balance and updates the strategy config's Capital
// to match. Logs a warning and falls back to the configured capital on any error.
//
// This ensures the bot trades 95% of the actual wallet balance rather than a
// stale config value.
func syncHyperliquidLiveCapital(sc *StrategyConfig) {
	if sc.Platform != "hyperliquid" {
		return
	}

	// Only sync in live mode (--mode=live arg present)
	isLive := false
	for _, arg := range sc.Args {
		if arg == "--mode=live" {
			isLive = true
			break
		}
	}
	if !isLive {
		return
	}

	// HYPERLIQUID_ACCOUNT_ADDRESS must be set explicitly; address derivation
	// from HYPERLIQUID_SECRET_KEY requires go-ethereum which is not in scope.
	accountAddr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
	if accountAddr == "" {
		fmt.Printf("[WARN] hl-live-balance: no account address for %s, using config capital=$%.2f\n",
			sc.ID, sc.Capital)
		return
	}

	balance, err := fetchHyperliquidBalance(accountAddr)
	if err != nil {
		fmt.Printf("[WARN] hl-live-balance: failed to fetch balance for %s (%s): %v — using config capital=$%.2f\n",
			sc.ID, accountAddr, err, sc.Capital)
		return
	}

	if balance <= 0 {
		fmt.Printf("[WARN] hl-live-balance: live balance is $%.2f for %s — wallet may be unfunded, using config capital=$%.2f\n",
			balance, sc.ID, sc.Capital)
		return
	}

	fmt.Printf("[INFO] hl-live-balance: synced %s capital from config $%.2f → live balance $%.2f\n",
		sc.ID, sc.Capital, balance)
	sc.Capital = balance
}

// fetchHyperliquidState fetches the account value and open positions from the
// Hyperliquid clearinghouseState endpoint in a single API call.
func fetchHyperliquidState(accountAddress string) (float64, []HLPosition, error) {
	payload := map[string]string{
		"type": "clearinghouseState",
		"user": accountAddress,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(hlMainnetURL+"/info", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("http %d from %s", resp.StatusCode, hlMainnetURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}

	var result struct {
		MarginSummary struct {
			AccountValue string `json:"accountValue"`
		} `json:"marginSummary"`
		AssetPositions []struct {
			Position struct {
				Coin    string `json:"coin"`
				Szi     string `json:"szi"`
				EntryPx string `json:"entryPx"`
			} `json:"position"`
		} `json:"assetPositions"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, nil, fmt.Errorf("parse response: %w", err)
	}

	balance, err := strconv.ParseFloat(result.MarginSummary.AccountValue, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("parse accountValue %q: %w", result.MarginSummary.AccountValue, err)
	}

	var positions []HLPosition
	for _, ap := range result.AssetPositions {
		szi, err := strconv.ParseFloat(ap.Position.Szi, 64)
		if err != nil || szi == 0 {
			continue
		}
		entryPx, err := strconv.ParseFloat(ap.Position.EntryPx, 64)
		if err != nil {
			fmt.Printf("[WARN] hl-sync: failed to parse entryPx %q for %s: %v\n", ap.Position.EntryPx, ap.Position.Coin, err)
		}
		positions = append(positions, HLPosition{
			Coin:       ap.Position.Coin,
			Size:       szi,
			EntryPrice: entryPx,
		})
	}

	return balance, positions, nil
}

// reconcileHyperliquidPositions applies on-chain position data to a StrategyState.
// It updates, adds, or removes the position for the given symbol and syncs cash.
// Returns true if any state was changed. Must be called under Lock.
func reconcileHyperliquidPositions(stratState *StrategyState, sym string, balance float64, positions []HLPosition, logger *StrategyLogger) bool {
	changed := false

	// Find the on-chain position for this strategy's symbol.
	var onChainPos *HLPosition
	for i := range positions {
		if positions[i].Coin == sym {
			onChainPos = &positions[i]
			break
		}
	}

	statePos := stratState.Positions[sym]

	if onChainPos != nil {
		qty := math.Abs(onChainPos.Size)
		side := "long"
		if onChainPos.Size < 0 {
			side = "short"
		}

		if statePos == nil {
			// Position exists on-chain but not in state — add it.
			stratState.Positions[sym] = &Position{
				Symbol:   sym,
				Quantity: qty,
				AvgCost:  onChainPos.EntryPrice,
				Side:     side,
			}
			logger.Info("hl-sync: discovered on-chain %s position %.6f %s @ $%.2f", side, qty, sym, onChainPos.EntryPrice)
			changed = true
		} else if statePos.Quantity != qty || statePos.Side != side {
			// Position exists in both but differs — update state to match on-chain.
			logger.Info("hl-sync: reconciled %s: state=%.6f %s → on-chain=%.6f %s @ $%.2f",
				sym, statePos.Quantity, statePos.Side, qty, side, onChainPos.EntryPrice)
			statePos.Quantity = qty
			statePos.Side = side
			statePos.AvgCost = onChainPos.EntryPrice
			changed = true
		}
	} else if statePos != nil {
		// Position in state but not on-chain — it was closed externally.
		logger.Info("hl-sync: %s position (%.6f %s) no longer on-chain, removing",
			sym, statePos.Quantity, statePos.Side)
		delete(stratState.Positions, sym)
		changed = true
	}

	// Sync cash with on-chain account value.
	if balance > 0 && balance != stratState.Cash {
		logger.Info("hl-sync: cash $%.2f → $%.2f (on-chain)", stratState.Cash, balance)
		stratState.Cash = balance
		changed = true
	}

	return changed
}

// syncHyperliquidPositions fetches on-chain positions from Hyperliquid and
// reconciles them with the internal StrategyState. This ensures hlPosQty and
// hlCash reflect reality before each execution cycle.
// Must be called WITHOUT holding any lock; acquires Lock internally.
func syncHyperliquidPositions(sc StrategyConfig, stratState *StrategyState, mu *sync.RWMutex, logger *StrategyLogger) bool {
	accountAddr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
	if accountAddr == "" {
		return false
	}

	sym := hyperliquidSymbol(sc.Args)
	if sym == "" {
		return false
	}

	// Fetch on-chain state (no lock held — I/O).
	balance, positions, err := fetchHyperliquidState(accountAddr)
	if err != nil {
		logger.Warn("hl-sync: failed to fetch on-chain state: %v", err)
		return false
	}

	// Reconcile under Lock.
	mu.Lock()
	changed := reconcileHyperliquidPositions(stratState, sym, balance, positions, logger)
	mu.Unlock()
	return changed
}
