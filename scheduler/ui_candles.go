package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type UICandle struct {
	Time   int64   `json:"time"`
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume,omitempty"`
}

type UICandleRequest struct {
	Strategy StrategyConfig
	From     time.Time
	To       time.Time
	Limit    int
}

func FetchUICandles(req UICandleRequest) ([]UICandle, string, error) {
	sc := req.Strategy
	symbol := strategyDisplaySymbol(sc)
	timeframe := strategyDisplayTimeframe(sc)
	if symbol == "" || timeframe == "" {
		return nil, "", fmt.Errorf("strategy %s has no symbol/timeframe", sc.ID)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 300
	}
	args := []string{
		"--platform", sc.Platform,
		"--type", sc.Type,
		"--symbol", symbol,
		"--timeframe", timeframe,
		"--limit", fmt.Sprintf("%d", limit),
	}
	args = append(args, "--mode", string(portfolioScopeFor(sc)))
	if !req.From.IsZero() {
		args = append(args, "--from", req.From.UTC().Format(time.RFC3339))
	}
	if !req.To.IsZero() {
		args = append(args, "--to", req.To.UTC().Format(time.RFC3339))
	}

	stdout, stderr, err := runPythonReadOnly("shared_scripts/fetch_candles.py", args)
	if err != nil {
		return nil, "", fmt.Errorf("fetch_candles: %w (stderr: %s)", err, strings.TrimSpace(string(stderr)))
	}
	var resp struct {
		Candles []UICandle `json:"candles"`
		Source  string     `json:"source"`
		Error   string     `json:"error,omitempty"`
	}
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, "", fmt.Errorf("parse candles: %w", err)
	}
	if resp.Error != "" {
		return nil, "", errors.New(resp.Error)
	}
	sort.Slice(resp.Candles, func(i, j int) bool { return resp.Candles[i].Time < resp.Candles[j].Time })
	return resp.Candles, resp.Source, nil
}
