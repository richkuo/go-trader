package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)


const hlMainnetURL = "https://api.hyperliquid.xyz"

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

	accountAddr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
	if accountAddr == "" {
		// Fall back to the key's own address via HYPERLIQUID_SECRET_KEY.
		// Address derivation requires go-ethereum which is not in scope here;
		// set HYPERLIQUID_ACCOUNT_ADDRESS explicitly for agent-wallet setups.
	}
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
