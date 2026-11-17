package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	var raw map[string]string
	err = hlInfoPost(ctx, hlInfoCall{
		BaseURL: hlMainnetURL, Type: "allMids", Body: body, Timeout: 10 * time.Second,
		Decode: func(data []byte) (int, error) {
			if perr := json.Unmarshal(data, &raw); perr != nil {
				return 0, fmt.Errorf("parse allMids response: %w", perr)
			}
			return 0, nil
		},
	})
	if err != nil {
		return nil, err
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
