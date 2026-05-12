package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
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

type UICandleResponse struct {
	Candles []UICandle
	Source  string
}

type UICandleFetcher func(UICandleRequest) ([]UICandle, string, error)

type UICandleCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]cachedUICandleEntry
}

type cachedUICandleEntry struct {
	resp      UICandleResponse
	expiresAt time.Time
}

func NewUICandleCache(ttl time.Duration) *UICandleCache {
	return &UICandleCache{
		ttl:     ttl,
		entries: make(map[string]cachedUICandleEntry),
	}
}

func (c *UICandleCache) Get(key string) (UICandleResponse, bool) {
	if c == nil {
		return UICandleResponse{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return UICandleResponse{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, key)
		return UICandleResponse{}, false
	}
	return entry.resp, true
}

func (c *UICandleCache) Set(key string, resp UICandleResponse) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cachedUICandleEntry{
		resp:      resp,
		expiresAt: time.Now().Add(c.ttl),
	}
}

func (r UICandleRequest) CacheKey() string {
	return fmt.Sprintf("%s|%d|%d|%d",
		r.Strategy.ID,
		r.From.UTC().Unix(),
		r.To.UTC().Unix(),
		r.Limit,
	)
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
	if mode := strategyMode(sc); mode != "" {
		args = append(args, "--mode", mode)
	}
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

func strategyMode(sc StrategyConfig) string {
	for _, arg := range sc.Args {
		arg = strings.TrimSpace(arg)
		switch {
		case arg == "live", arg == "paper":
			return arg
		case strings.HasPrefix(arg, "--mode="):
			return strings.TrimPrefix(arg, "--mode=")
		}
	}
	return ""
}
