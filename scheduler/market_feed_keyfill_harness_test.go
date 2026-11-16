package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

type keyFillHarness struct {
	t     *testing.T
	ctx   context.Context
	dir   string
	ctxOf map[*feedSealer]context.Context
}

func newKeyFillHarness(t *testing.T, ctx context.Context, intervalMs int64, failAlways, failWithoutLedger map[string]bool) *keyFillHarness {
	t.Helper()
	origCandles := fetchHyperliquidCandleSnapshotFn
	fetchHyperliquidCandleSnapshotFn = func(ctx context.Context, coin, interval string, startMs, endMs int64) ([]hlCandleRaw, error) {
		if _, err := feedBudgetAcquire(ctx, hlMainnetURL, "candleSnapshot"); err != nil {
			return nil, err
		}
		if failAlways[coin] || (failWithoutLedger[coin] && feedLedgerFrom(ctx) == nil) {
			return nil, errors.New("stub venue refuses candles for " + coin)
		}
		lastOpen := time.Now().UTC().UnixMilli() / intervalMs * intervalMs
		base := 10.0 + float64(len(coin))
		out := make([]hlCandleRaw, 0, 260)
		for i := 259; i >= 0; i-- {
			open := lastOpen - int64(i)*intervalMs
			px := base + float64(260-i)*0.25
			out = append(out, hlCandleRaw{OpenMs: open, CloseMs: open + intervalMs - 1, HasClose: i != 0,
				Open: px, High: px + 2, Low: px - 1, Close: px + 1, Volume: 5})
		}
		return out, nil
	}
	origMids := fetchHyperliquidMidsCtxFn
	fetchHyperliquidMidsCtxFn = func(ctx context.Context, coins []string) (map[string]float64, error) {
		if _, err := feedBudgetAcquire(ctx, hlMainnetURL, "allMids"); err != nil {
			return nil, err
		}
		out := make(map[string]float64, len(coins))
		for _, c := range coins {
			out[c] = 11
		}
		return out, nil
	}
	t.Cleanup(func() {
		fetchHyperliquidCandleSnapshotFn = origCandles
		fetchHyperliquidMidsCtxFn = origMids
	})
	dir, err := os.MkdirTemp("", "gkf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return &keyFillHarness{t: t, ctx: ctx, dir: dir, ctxOf: map[*feedSealer]context.Context{}}
}

func keyFillConsumer(need map[marketFeedKey]int) feedRequirements {
	r := feedRequirements{Keys: map[marketFeedKey]int{}, Funding: map[string]feedFundingNeed{}, Strategies: map[string]feedStrategyRequirement{}}
	for k, n := range need {
		r.Keys[k] = n
		r.MidCoins = append(r.MidCoins, k.Symbol)
		id := "s-" + k.Symbol
		r.Strategies[id] = feedStrategyRequirement{ID: id, Signal: k, SignalLookback: n, Coin: k.Symbol}
	}
	sort.Strings(r.MidCoins)
	r.finalize()
	return r
}

func keyFillCycle(r feedRequirements) cycleMarketRequirements {
	ids := make([]string, 0, len(r.Strategies))
	for id := range r.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	due := make([]StrategyConfig, 0, len(ids))
	for _, id := range ids {
		due = append(due, StrategyConfig{ID: id})
	}
	return cycleRequirementsForDue(due, r)
}

func (h *keyFillHarness) feed(source, instance string, ledger *feedRequestLedger, consumers ...feedConsumer) *feedSealer {
	h.t.Helper()
	now := time.Now().UTC()
	u := unionFeedRequirements(consumers)
	owner := newMarketFeedOwner(nil, h.t.Logf)
	lctx := withFeedLedger(h.ctx, ledger)
	if ledger != nil {
		ledger.openStartup()
	}
	select {
	case <-owner.ApplyGeneration(lctx, u):
	case <-time.After(5 * time.Second):
		h.t.Fatalf("%s generation did not publish", instance)
	}
	owner.IngestMids(map[string]float64{"BTC": 11, "ETH": 11, "SOL": 11, "XRP": 11, "AVAX": 11, "DOGE": 11}, now, "ws")
	sealer := newFeedSealer(owner, source, instance, nil, h.t.Logf)
	sealer.settle = 0
	sealer.prepare = time.Hour
	sealer.ledger = ledger
	if source == feedSourceREST {
		rest := newFeedRESTSource(owner, h.t.Logf)
		rest.backoff = 10 * time.Millisecond
		sealer.prepareFn = rest.prepare
	} else {
		sealer.prepareFn = websocketFeedPrepare(owner)
	}
	sealer.setGeneration(u, []int{60}, now.Add(-30*time.Minute))
	sealer.startServing(now.Add(-30 * time.Minute))
	h.ctxOf[sealer] = lctx
	return sealer
}

func (h *keyFillHarness) seal(s *feedSealer, keys ...int64) {
	for _, key := range keys {
		s.sealOne(h.ctxOf[s], key)
	}
}

func (h *keyFillHarness) listen(name string, sealer *feedSealer) string {
	h.t.Helper()
	path := filepath.Join(h.dir, name+".sock")
	srv, err := listenFeedSocket(path, sealer, h.t.Logf)
	if err != nil {
		h.t.Fatal(err)
	}
	go srv.serve()
	h.t.Cleanup(srv.close)
	return path
}

func (h *keyFillHarness) stalled(name string) string {
	h.t.Helper()
	path := filepath.Join(h.dir, name+".sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		h.t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	h.t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	})
	return path
}

func keyFillClient(primary, backup string) (*sharedFeedClient, func(string) int) {
	c := newSharedFeedClientWithEndpoints([]sharedFeedEndpoint{
		{Name: "primary", Socket: primary},
		{Name: "backup", Socket: backup},
	})
	c.settle, c.prepare = 0, time.Hour
	var mu sync.Mutex
	counts := map[string]int{}
	inner := c.dial
	c.dial = func(ctx context.Context, path string) (net.Conn, error) {
		mu.Lock()
		counts[path]++
		mu.Unlock()
		return inner(ctx, path)
	}
	return c, func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[path]
	}
}
