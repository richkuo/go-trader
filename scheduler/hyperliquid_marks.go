package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// fetchHyperliquidMids fetches the current mid prices for all HL perpetuals
// in a single authentication-free round-trip and returns a coin→price map
// filtered to the requested coins. Reuses hlMainnetURL from
// hyperliquid_balance.go so tests can redirect to a stub server.
//
// The /info allMids response is a flat JSON object: {"BTC":"67500.50", ...}.
// Each value is a numeric string. Coins not listed on HL are omitted from the
// returned map — the caller falls back to pos.AvgCost via the prices-miss
// path in PortfolioValue / PortfolioNotional (same graceful degradation as
// fetch_futures_marks.py misses).
//
// This is the correct oracle for HL perps positions; BinanceUS spot is wrong
// because spot/perps basis divergence (funding, liquidity, exchange-specific
// pricing) shows up as phantom PnL in PortfolioValue — fixes issue #263.
func fetchHyperliquidMids(coins []string) (map[string]float64, error) {
	if len(coins) == 0 {
		return map[string]float64{}, nil
	}

	payload := map[string]string{"type": "allMids"}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal allMids request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(hlMainnetURL+"/info", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d from %s/info allMids", resp.StatusCode, hlMainnetURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read allMids response: %w", err)
	}

	// Flat object: {"BTC": "67500.50", "ETH": "3200.10", ...}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse allMids response: %w", err)
	}

	want := make(map[string]bool, len(coins))
	for _, c := range coins {
		want[c] = true
	}

	marks := make(map[string]float64, len(coins))
	for coin, priceStr := range raw {
		if !want[coin] {
			continue
		}
		p, err := strconv.ParseFloat(priceStr, 64)
		if err != nil || p <= 0 {
			continue
		}
		marks[coin] = p
	}
	return marks, nil
}
