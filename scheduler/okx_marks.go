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
