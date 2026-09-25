package main

import (
	"context"
	"testing"
	"time"
)

func feedDispatchFixture(t *testing.T) (*marketFeedContext, *Config, StrategyConfig, StrategyConfig) {
	t.Helper()
	return feedDispatchFixtureWithRegime(t, &RegimeConfig{Enabled: true, Timeframe: "4h", Period: 14, ADXThreshold: 20})
}

func feedDispatchFixtureWithRegime(t *testing.T, rc *RegimeConfig) (*marketFeedContext, *Config, StrategyConfig, StrategyConfig) {
	t.Helper()
	now := time.Unix(1_700_003_600, 0).UTC()
	scA := StrategyConfig{
		ID: "hl-a", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"momentum", "BTC", "1h", "--mode=paper"}, HTFFilter: true, Capital: 1000,
	}
	scB := StrategyConfig{
		ID: "hl-b", Type: "perps", Platform: "hyperliquid", Script: hyperliquidCheckScript,
		Args: []string{"momentum", "BTC", "1h", "--mode=live"}, Capital: 1000,
	}
	cfg := &Config{IntervalSeconds: 300, MarketFeed: marketFeedWebsocket, Regime: rc, Strategies: []StrategyConfig{scA, scB}}
	req, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatalf("derive requirements: %v", err)
	}

	owner := newMarketFeedOwner(func() time.Time { return now }, nil)
	for key, lookback := range req.Keys {
		st := newFeedKeyState(key, mustIntervalMs(t, key.Timeframe), lookback)
		bars := lookback + 5
		base := now.UnixMilli() - int64(bars)*st.IntervalMs
		rows := make([]hlCandleRaw, 0, bars)
		for i := 0; i < bars; i++ {
			open := base + int64(i)*st.IntervalMs
			rows = append(rows, hlCandleRaw{
				OpenMs: open, CloseMs: open + st.IntervalMs - 1, HasClose: true,
				Open: 100, High: 102, Low: 98, Close: 101, Volume: 5,
			})
		}
		mergeRestRows(st, rows, now.Add(-time.Second))
		st.Status = feedStatusReady
		st.LastRecvAt = now
		owner.keys[key] = st
		owner.published[key] = true
	}
	owner.midCoins["BTC"] = true
	owner.mids["BTC"] = feedMid{Px: 101, RecvAt: now, Source: "ws"}
	owner.gen = 2

	reqs := cycleRequirementsForDue([]StrategyConfig{scA, scB}, req)
	snap := sealCycleMarketSnapshot(context.Background(), owner, reqs, "300s/1700003600", now)
	return &marketFeedContext{Enabled: true, Requirements: req, Snapshot: snap, Interval: 300}, cfg, scA, scB
}

func mustIntervalMs(t *testing.T, timeframe string) int64 {
	t.Helper()
	ms, ok := hlCandleIntervalMs(timeframe)
	if !ok {
		t.Fatalf("unsupported interval %q", timeframe)
	}
	return ms
}

func TestFeedOutageHoldsEntriesAndKeepsProtection(t *testing.T) {
	feed, cfg, scA, _ := feedDispatchFixture(t)
	failedKey := feed.Requirements.Strategies[scA.ID].Signal
	delete(feed.Snapshot.keys, failedKey)

	origPlain := runHyperliquidCheckFn
	origStdin := runHyperliquidCheckWithStdinFn
	spawns := 0
	runHyperliquidCheckFn = func(string, []string) (*HyperliquidResult, string, error) {
		spawns++
		return nil, "", nil
	}
	runHyperliquidCheckWithStdinFn = func(string, []string, []byte) (*HyperliquidResult, string, error) {
		spawns++
		return nil, "", nil
	}
	t.Cleanup(func() {
		runHyperliquidCheckFn = origPlain
		runHyperliquidCheckWithStdinFn = origStdin
	})

	scriptFailureTracker.Record(scA.ID, "boom", time.Now())
	t.Cleanup(func() { scriptFailureTracker.Clear(scA.ID) })

	result, _, price, ok := runHyperliquidCheck(&scA, map[string]float64{"BTC": 101}, PositionCtx{}, cfg.Regime, "simple", nil, hlBatchTestLogger(), nil, feed)
	if !ok {
		t.Fatalf("a degraded evaluation must still return a result so protection can run")
	}
	if _, streak := scriptFailureTracker.Clear(scA.ID); streak != 1 {
		t.Fatalf("a degraded evaluation runs no script, so it must not clear the script-failure streak: prior count %d", streak)
	}
	if spawns != 0 {
		t.Fatalf("a degraded evaluation must never spawn a private fetch, got %d spawns", spawns)
	}
	if result.Signal != 0 || result.CloseFraction != 0 {
		t.Fatalf("a degraded result must carry no signal and no close: %+v", result)
	}
	if result.Degraded == "" {
		t.Fatalf("a degraded result must say why")
	}
	if price != 101 {
		t.Fatalf("a degraded result must price from the verified mid, got %v", price)
	}

	held, why := feed.feedHoldsSignal(scA)
	if !held || why == "" {
		t.Fatalf("the dispatch site must see the hold: %v %q", held, why)
	}

	tests := []struct {
		name       string
		signal     int
		closeFrac  float64
		posQty     float64
		posSide    string
		allowsLong bool
		allowsShrt bool
		wantHeld   bool
	}{
		{name: "flat long entry is held", signal: 1, posQty: 0, allowsLong: true, allowsShrt: true, wantHeld: true},
		{name: "flat short entry is held", signal: -1, posQty: 0, allowsLong: true, allowsShrt: true, wantHeld: true},
		{name: "scale-in on an open long is held", signal: 1, posQty: 2, posSide: "long", allowsLong: true, allowsShrt: true, wantHeld: true},
		{name: "scale-in on an open short is held", signal: -1, posQty: 2, posSide: "short", allowsLong: true, allowsShrt: true, wantHeld: true},
		{name: "a close on an open long passes", signal: -1, posQty: 2, posSide: "long", allowsLong: true, allowsShrt: false, wantHeld: false},
		{name: "a close on an open short passes", signal: 1, posQty: 2, posSide: "short", allowsLong: false, allowsShrt: true, wantHeld: false},
		{name: "a close fraction always passes", signal: 0, closeFrac: 0.5, posQty: 2, posSide: "long", allowsLong: true, allowsShrt: true, wantHeld: false},
		{name: "no signal at all is nothing to hold", signal: 0, posQty: 2, posSide: "long", allowsLong: true, allowsShrt: true, wantHeld: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pausedBlocksSignal(tc.signal, tc.closeFrac, tc.posQty, tc.posSide, tc.allowsLong, tc.allowsShrt)
			if got != tc.wantHeld {
				t.Fatalf("hold: got %v want %v", got, tc.wantHeld)
			}
		})
	}
}
