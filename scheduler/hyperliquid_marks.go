package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

func fetchHyperliquidMids(coins []string) (map[string]float64, error) {
	return fetchHyperliquidMidsCtx(context.Background(), coins)
}

var fetchHyperliquidMidsCtxFn = fetchHyperliquidMidsCtx

func fetchHyperliquidMidsCtx(ctx context.Context, coins []string) (map[string]float64, error) {
	if len(coins) == 0 {
		return map[string]float64{}, nil
	}

	payload := map[string]string{"type": "allMids"}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal allMids request: %w", err)
	}
	if err := feedBudgetAcquire(ctx, "allMids"); err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, hlMainnetURL+"/info", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build allMids request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
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
