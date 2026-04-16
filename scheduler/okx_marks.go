package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// okxMainnetURL is the OKX REST base. Exposed as a var so tests can redirect
// to a httptest stub (mirrors hlMainnetURL in hyperliquid_balance.go).
var okxMainnetURL = "https://www.okx.com"

// fetchOKXPerpsMids fetches the current last prices for all OKX USDT-margined
// perpetual swaps in a single authentication-free round-trip and returns a
// coin→price map filtered to the requested coins.
//
// The /api/v5/market/tickers?instType=SWAP endpoint returns every swap ticker
// in one payload, matching the HL allMids pattern (one fetch for N coins).
// Using the `last` field preserves the semantics of the previous Python path
// (OKXExchangeAdapter.get_perp_price → ccxt fetch_ticker → ticker.last) so
// mark-sourcing behavior is unchanged — this PR is a transport swap, not a
// price-oracle change.
//
// Closes #279: removes the per-cycle subprocess cold-start cost (Python
// interpreter + ccxt import + load_markets REST call) that was paid on every
// /status poll. A non-USDT-margined OKX perp (e.g. USDC-margined or crypto-
// settled) would require a different instId suffix; today only USDT-SWAP is
// supported because collectPerpsMarkSymbols emits a bare base coin and OKX
// perps in the repo all settle vs. USDT.
func fetchOKXPerpsMids(coins []string) (map[string]float64, error) {
	if len(coins) == 0 {
		return map[string]float64{}, nil
	}

	url := okxMainnetURL + "/api/v5/market/tickers?instType=SWAP"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d from %s", resp.StatusCode, url)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tickers response: %w", err)
	}

	// OKX envelope: {"code":"0","msg":"","data":[{"instId":"BTC-USDT-SWAP","last":"67500.5",...},...]}
	var env struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID string `json:"instId"`
			Last   string `json:"last"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parse tickers response: %w", err)
	}
	if env.Code != "0" {
		return nil, fmt.Errorf("okx api error code=%s msg=%s", env.Code, env.Msg)
	}

	want := make(map[string]string, len(coins))
	for _, c := range coins {
		want[c+"-USDT-SWAP"] = c
	}

	marks := make(map[string]float64, len(coins))
	for _, t := range env.Data {
		coin, ok := want[t.InstID]
		if !ok {
			continue
		}
		p, err := strconv.ParseFloat(t.Last, 64)
		if err != nil || p <= 0 {
			continue
		}
		marks[coin] = p
	}
	return marks, nil
}
