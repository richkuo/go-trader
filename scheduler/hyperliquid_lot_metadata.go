package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	hlLotMetadataRefreshAfter     = time.Hour
	hlLotMetadataExpiry           = 6 * time.Hour
	hlLotMetadataFetchTimeout     = 10 * time.Second
	hlLotMetadataRetryInitial     = 30 * time.Second
	hlLotMetadataRetryMax         = 10 * time.Minute
	hlLotMetadataMaxResponseBytes = 8 << 20
	hlLotMetadataMaxSzDecimals    = 12
	hlLotMetadataLedgerPerMinute  = 4
)

var hlTestnetURL = "https://api.hyperliquid-testnet.xyz"

func hlInfoEndpoint() string {
	if os.Getenv("HYPERLIQUID_TESTNET") == "1" {
		return hlTestnetURL
	}
	return hlMainnetURL
}

type hlLotKey struct {
	Endpoint string
	Coin     string
}

type hlLotEntry struct {
	SzDecimals int
	Problem    string
}

type hlLotSnapshot struct {
	Endpoint  string
	FetchedAt time.Time
	Coins     map[string]hlLotEntry
}

type hlLotLookup struct {
	Known      bool
	SzDecimals int
	Endpoint   string
	Coin       string
	Reason     string
}

type hlLotMetadataCache struct {
	mu        sync.Mutex
	clock     func() time.Time
	endpoint  func() string
	fetch     func(ctx context.Context, endpoint string) ([]byte, error)
	logf      func(format string, a ...any)
	baseCtx   context.Context
	snaps     map[string]*hlLotSnapshot
	inflight  map[string]chan struct{}
	retryAt   map[string]time.Time
	retryWait map[string]time.Duration
	lastErr   map[string]string
	outages   map[hlLotKey]string
}

var hlLotMetadata = newHLLotMetadataCache(nil)

func newHLLotMetadataCache(ctx context.Context) *hlLotMetadataCache {
	c := &hlLotMetadataCache{
		clock:     func() time.Time { return time.Now() },
		endpoint:  hlInfoEndpoint,
		fetch:     fetchHLLotMetadataRaw,
		logf:      func(format string, a ...any) { fmt.Printf(format+"\n", a...) },
		snaps:     map[string]*hlLotSnapshot{},
		inflight:  map[string]chan struct{}{},
		retryAt:   map[string]time.Time{},
		retryWait: map[string]time.Duration{},
		lastErr:   map[string]string{},
		outages:   map[hlLotKey]string{},
	}
	c.baseCtx = hlLotMetadataLedgerContext(ctx)
	return c
}

func hlLotMetadataLedgerContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if feedLedgerFrom(ctx) != nil {
		return ctx
	}
	local := newFeedRequestLedger(&feedBudgetConfig{PerMinute: hlLotMetadataLedgerPerMinute, Startup: 1}, true, nil)
	return withFeedLedger(ctx, local)
}

func (c *hlLotMetadataCache) configure(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseCtx = hlLotMetadataLedgerContext(ctx)
}

func fetchHLLotMetadataRaw(ctx context.Context, endpoint string) ([]byte, error) {
	return hlPostInfoTo(ctx, endpoint, map[string]string{"type": "meta"}, hlLotMetadataMaxResponseBytes)
}

func (c *hlLotMetadataCache) Ensure(coin string) {
	endpoint := c.endpoint()
	c.mu.Lock()
	now := c.clock()
	if snap := c.snaps[endpoint]; snap != nil {
		if age := now.Sub(snap.FetchedAt); age >= 0 && age < hlLotMetadataRefreshAfter {
			c.mu.Unlock()
			return
		}
	}
	if wait, ok := c.inflight[endpoint]; ok {
		c.mu.Unlock()
		<-wait
		return
	}
	if at, ok := c.retryAt[endpoint]; ok && now.Before(at) {
		c.mu.Unlock()
		return
	}
	done := make(chan struct{})
	c.inflight[endpoint] = done
	ctx := c.baseCtx
	c.mu.Unlock()

	snap, err := c.refresh(ctx, endpoint)

	c.mu.Lock()
	delete(c.inflight, endpoint)
	if err != nil {
		wait := c.retryWait[endpoint] * 2
		if wait < hlLotMetadataRetryInitial {
			wait = hlLotMetadataRetryInitial
		}
		if wait > hlLotMetadataRetryMax {
			wait = hlLotMetadataRetryMax
		}
		c.retryWait[endpoint] = wait
		c.retryAt[endpoint] = c.clock().Add(wait)
		c.lastErr[endpoint] = err.Error()
	} else {
		c.snaps[endpoint] = snap
		delete(c.retryWait, endpoint)
		delete(c.retryAt, endpoint)
		delete(c.lastErr, endpoint)
	}
	c.mu.Unlock()
	close(done)
}

func (c *hlLotMetadataCache) refresh(base context.Context, endpoint string) (*hlLotSnapshot, error) {
	ctx, cancel := context.WithTimeout(withFeedReason(base, string(feedRestLotMeta)), hlLotMetadataFetchTimeout)
	defer cancel()
	data, err := c.fetch(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	coins, err := parseHLLotMetadata(data)
	if err != nil {
		return nil, err
	}
	return &hlLotSnapshot{Endpoint: endpoint, FetchedAt: c.clock(), Coins: coins}, nil
}

func (c *hlLotMetadataCache) Lookup(coin string) hlLotLookup {
	endpoint := c.endpoint()
	c.mu.Lock()
	defer c.mu.Unlock()
	res := c.lookupLocked(endpoint, coin)
	key := hlLotKey{Endpoint: endpoint, Coin: coin}
	if res.Known {
		if prev, was := c.outages[key]; was {
			delete(c.outages, key)
			c.logf("[hl-lot] %s lot size restored at %s (szDecimals=%d) after outage: %s", coin, endpoint, res.SzDecimals, prev)
		}
		return res
	}
	if _, was := c.outages[key]; !was {
		c.outages[key] = res.Reason
		c.logf("[hl-lot] %s lot size unavailable at %s: %s; paper entries, scale-in adds and partial closes hold until it recovers (full closes and stops still run)", coin, endpoint, res.Reason)
	}
	return res
}

func (c *hlLotMetadataCache) Peek(coin string) hlLotLookup {
	if c == nil {
		return hlLotLookup{Coin: coin, Reason: "no venue metadata source"}
	}
	endpoint := c.endpoint()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookupLocked(endpoint, coin)
}

func (c *hlLotMetadataCache) lookupLocked(endpoint, coin string) hlLotLookup {
	res := hlLotLookup{Endpoint: endpoint, Coin: coin}
	snap := c.snaps[endpoint]
	if snap == nil {
		res.Reason = "no venue metadata fetched yet"
		if e := c.lastErr[endpoint]; e != "" {
			res.Reason += " (last refresh: " + e + ")"
		}
		return res
	}
	age := c.clock().Sub(snap.FetchedAt)
	if age < 0 || age >= hlLotMetadataExpiry {
		res.Reason = fmt.Sprintf("venue metadata expired (age %s, limit %s)", age.Round(time.Second), hlLotMetadataExpiry)
		if e := c.lastErr[endpoint]; e != "" {
			res.Reason += " (last refresh: " + e + ")"
		}
		return res
	}
	entry, ok := snap.Coins[coin]
	if !ok {
		res.Reason = "coin absent from the venue perps universe"
		return res
	}
	if entry.Problem != "" {
		res.Reason = entry.Problem
		return res
	}
	res.Known = true
	res.SzDecimals = entry.SzDecimals
	return res
}

func parseHLLotMetadata(data []byte) (map[string]hlLotEntry, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("parse meta: %w", err)
	}
	rawUniverse, ok := top["universe"]
	if !ok {
		return nil, errors.New("meta response has no universe")
	}
	var universe []map[string]json.RawMessage
	if err := json.Unmarshal(rawUniverse, &universe); err != nil {
		return nil, fmt.Errorf("parse meta universe: %w", err)
	}
	if len(universe) == 0 {
		return nil, errors.New("meta universe is empty")
	}
	coins := make(map[string]hlLotEntry, len(universe))
	for _, asset := range universe {
		var name string
		if raw, ok := asset["name"]; !ok || json.Unmarshal(raw, &name) != nil || name == "" {
			continue
		}
		entry := hlLotEntry{}
		if sz, problem := parseHLSzDecimals(asset["szDecimals"]); problem != "" {
			entry.Problem = problem
		} else {
			entry.SzDecimals = sz
		}
		if _, dup := coins[name]; dup {
			coins[name] = hlLotEntry{Problem: "duplicate venue universe entries for the coin"}
			continue
		}
		coins[name] = entry
	}
	return coins, nil
}

func parseHLSzDecimals(raw json.RawMessage) (int, string) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return 0, "szDecimals absent"
	}
	if text == "null" {
		return 0, "szDecimals is null"
	}
	if text[0] != '-' && (text[0] < '0' || text[0] > '9') {
		return 0, fmt.Sprintf("szDecimals %s is not a number", text)
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Sprintf("szDecimals %s is not an integer", text)
	}
	if v < 0 || v > hlLotMetadataMaxSzDecimals {
		return 0, fmt.Sprintf("szDecimals %d is outside 0..%d", v, hlLotMetadataMaxSzDecimals)
	}
	return int(v), ""
}
