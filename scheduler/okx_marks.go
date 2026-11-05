package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

var okxMainnetURL = "https://www.okx.com"

func fetchOKXTickerMids(instType string) (map[string]float64, error) {
	url := okxMainnetURL + "/api/v5/market/tickers?instType=" + instType
	client := egressHTTPClient(10 * time.Second)
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

	lasts := make(map[string]float64, len(env.Data))
	for _, t := range env.Data {
		p, err := strconv.ParseFloat(t.Last, 64)
		if err != nil || p <= 0 {
			continue
		}
		lasts[t.InstID] = p
	}
	return lasts, nil
}

func fetchOKXPerpsMids(coins []string) (map[string]float64, error) {
	if len(coins) == 0 {
		return map[string]float64{}, nil
	}
	lasts, err := fetchOKXTickerMids("SWAP")
	if err != nil {
		return nil, err
	}
	marks := make(map[string]float64, len(coins))
	for _, c := range coins {
		if p, ok := lasts[c+"-USDT-SWAP"]; ok {
			marks[c] = p
		}
	}
	return marks, nil
}

var okxSpotQuoteOrder = []string{"USDT", "USD", "USDC"}

func fetchOKXSpotMids(coins []string) (map[string]float64, map[string]string, error) {
	if len(coins) == 0 {
		return map[string]float64{}, map[string]string{}, nil
	}
	lasts, err := fetchOKXTickerMids("SPOT")
	if err != nil {
		return nil, nil, err
	}
	marks := make(map[string]float64, len(coins))
	instIDs := make(map[string]string, len(coins))
	for _, c := range coins {
		for _, quote := range okxSpotQuoteOrder {
			inst := c + "-" + quote
			if p, ok := lasts[inst]; ok {
				marks[c] = p
				instIDs[c] = inst
				break
			}
		}
	}
	return marks, instIDs, nil
}
