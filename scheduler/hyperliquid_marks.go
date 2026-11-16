package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	reply, err := hlInfoPost(ctx, hlMainnetURL, "allMids", body, 10*time.Second, 0)
	if err != nil {
		return nil, err
	}
	if reply.Status != http.StatusOK {
		return nil, fmt.Errorf("http %d from %s/info allMids", reply.Status, hlMainnetURL)
	}
	if reply.ReadErr != nil {
		return nil, fmt.Errorf("read allMids response: %w", reply.ReadErr)
	}
	data := reply.Body

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
