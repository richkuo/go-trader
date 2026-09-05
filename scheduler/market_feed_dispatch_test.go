package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedCheck struct {
	Args   []string
	Market map[string]any
}

func decodeMarketEnvelope(t *testing.T, stdin []byte) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(stdin, &envelope); err != nil {
		t.Fatalf("stdin is not JSON: %v (%s)", err, string(stdin))
	}
	market, _ := envelope["market"].(map[string]any)
	if market == nil {
		t.Fatalf("stdin carries no market payload: %s", string(stdin))
	}
	return market
}

func feedDispatchFixture(t *testing.T) (*marketFeedContext, *Config, StrategyConfig, StrategyConfig) {
	t.Helper()
	now := time.Unix(1_700_003_600, 0).UTC()
	rc := &RegimeConfig{Enabled: true, Timeframe: "4h", Period: 14, ADXThreshold: 20}
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

func stubSingletonChecks(t *testing.T, captured *[]capturedCheck, mu *sync.Mutex) {
	t.Helper()
	origPlain := runHyperliquidCheckFn
	origStdin := runHyperliquidCheckWithStdinFn
	runHyperliquidCheckFn = func(script string, args []string) (*HyperliquidResult, string, error) {
		t.Errorf("a covered strategy must never run a check without the sealed payload: %v", args)
		return nil, "", nil
	}
	runHyperliquidCheckWithStdinFn = func(script string, args []string, stdin []byte) (*HyperliquidResult, string, error) {
		mu.Lock()
		*captured = append(*captured, capturedCheck{Args: append([]string(nil), args...), Market: decodeMarketEnvelope(t, stdin)})
		mu.Unlock()
		return &HyperliquidResult{Strategy: "momentum", Symbol: "BTC", Timeframe: "1h", Signal: 0, Price: 101, Mode: "paper", Platform: "hyperliquid"}, "", nil
	}
	t.Cleanup(func() {
		runHyperliquidCheckFn = origPlain
		runHyperliquidCheckWithStdinFn = origStdin
	})
}

func TestEveryCheckPathConsumesTheSealedSnapshot(t *testing.T) {
	feed, cfg, scA, scB := feedDispatchFixture(t)
	snapshotID := feed.Snapshot.EvaluationID

	t.Run("the batch request carries version 2 and every frame the group needs", func(t *testing.T) {
		var stdinSeen []byte
		var argsSeen []string
		orig := runHyperliquidBatchCheckFn
		runHyperliquidBatchCheckFn = func(script string, args []string, stdin []byte) (*HyperliquidBatchResult, string, error) {
			stdinSeen = append([]byte(nil), stdin...)
			argsSeen = append([]string(nil), args...)
			return &HyperliquidBatchResult{Results: []HyperliquidBatchSlotResult{
				{ID: "hl-a", HyperliquidResult: HyperliquidResult{Symbol: "BTC", Price: 101}},
				{ID: "hl-b", HyperliquidResult: HyperliquidResult{Symbol: "BTC", Price: 101}},
			}}, "", nil
		}
		t.Cleanup(func() { runHyperliquidBatchCheckFn = orig })

		key, _ := hlBatchKeyForStrategy(scA, cfg)
		inputs := []hlBatchGroupInput{{Key: key, Members: []StrategyConfig{scA, scB}, PosCtx: map[string]PositionCtx{}}}
		results := runHyperliquidBatchGroups(inputs, cfg, nil, func(string, ...any) {}, feed)
		if results == nil {
			t.Fatalf("the batch produced no results")
		}
		if !containsArg(argsSeen, marketStdinFlag) {
			t.Fatalf("the batch argv must declare the sealed payload: %v", argsSeen)
		}
		var envelope map[string]any
		if err := json.Unmarshal(stdinSeen, &envelope); err != nil {
			t.Fatalf("batch stdin: %v", err)
		}
		if int(envelope["v"].(float64)) != hlBatchProtocolVersionMarket {
			t.Fatalf("batch protocol version: got %v want %d", envelope["v"], hlBatchProtocolVersionMarket)
		}
		market := decodeMarketEnvelope(t, stdinSeen)
		if market["snapshot_id"] != snapshotID {
			t.Fatalf("snapshot id: got %v want %v", market["snapshot_id"], snapshotID)
		}
		frames := market["frames"].(map[string]any)
		for _, want := range []string{"BTC|1h", "BTC|4h"} {
			if _, ok := frames[want]; !ok {
				t.Fatalf("frame %s missing from the batch payload: %v", want, keysOf(frames))
			}
		}
		signal := frames["BTC|1h"].(map[string]any)
		htfRegime := frames["BTC|4h"].(map[string]any)
		if int(signal["required"].(float64)) != key.OhlcvLimit {
			t.Fatalf("the signal frame must carry the group lookback: %v", signal["required"])
		}
		if len(htfRegime["rows"].([]any)) == 0 {
			t.Fatalf("the higher-timeframe and regime frame must carry rows")
		}
	})

	t.Run("singleton, changed-input retry and shared-failure retry all reuse the sealed data", func(t *testing.T) {
		var captured []capturedCheck
		var mu sync.Mutex
		stubSingletonChecks(t, &captured, &mu)

		logger := hlBatchTestLogger()
		if _, _, _, ok := runHyperliquidCheck(&scA, map[string]float64{"BTC": 101}, PositionCtx{}, cfg.Regime, "simple", nil, logger, nil, feed); !ok {
			t.Fatalf("the singleton path must produce a result")
		}

		drifted := &hlBatchCycleResults{}
		drifted.put(scB.ID, hlBatchMemberOutcome{Result: &HyperliquidResult{Symbol: "BTC", Price: 101}, Fingerprint: "stale-fingerprint"})
		if _, _, _, ok := runHyperliquidCheck(&scB, map[string]float64{"BTC": 101}, PositionCtx{}, cfg.Regime, "simple", nil, logger, drifted, feed); !ok {
			t.Fatalf("the changed-input retry must produce a result")
		}

		sharedFailure := &hlBatchCycleResults{}
		if _, _, _, ok := runHyperliquidCheck(&scB, map[string]float64{"BTC": 101}, PositionCtx{}, cfg.Regime, "simple", nil, logger, sharedFailure, feed); !ok {
			t.Fatalf("the shared-failure retry must produce a result")
		}

		mu.Lock()
		defer mu.Unlock()
		if len(captured) != 3 {
			t.Fatalf("expected three sealed-payload checks, got %d", len(captured))
		}
		for i, c := range captured {
			if !containsArg(c.Args, marketStdinFlag) {
				t.Fatalf("check %d argv must declare the sealed payload: %v", i, c.Args)
			}
			if c.Market["snapshot_id"] != snapshotID {
				t.Fatalf("check %d snapshot id: got %v want %v", i, c.Market["snapshot_id"], snapshotID)
			}
			frames := c.Market["frames"].(map[string]any)
			if _, ok := frames["BTC|1h"]; !ok {
				t.Fatalf("check %d is missing the signal frame: %v", i, keysOf(frames))
			}
			signal := frames["BTC|1h"].(map[string]any)
			if int(signal["required"].(float64)) != hlBatchPythonDefaultOhlcvLimit {
				t.Fatalf("a singleton check must ask for the legacy 200-bar lookback, got %v", signal["required"])
			}
		}
		if _, ok := captured[0].Market["frames"].(map[string]any)["BTC|4h"]; !ok {
			t.Fatalf("the higher-timeframe filter frame must ride along for hl-a")
		}
	})

	t.Run("the regime bundle check reads the same sealed frame", func(t *testing.T) {
		var seen []byte
		var seenReq regimeBundleRequest
		stubRegimeBundleCheckWithMarket(t, func(_ context.Context, req regimeBundleRequest, stdin []byte) (*RegimeBundle, error) {
			seen = append([]byte(nil), stdin...)
			seenReq = req
			return &RegimeBundle{Key: req.Key, Payload: RegimePayload{Legacy: "trending_up"}, RawRegimeJSON: `"trending_up"`, At: time.Now().UTC()}, nil
		})
		store := &RegimeStore{}
		startRegimeStorePopulation(store, []StrategyConfig{scA}, cfg.Regime, nil, feed)()
		if seen == nil {
			t.Fatalf("the regime bundle check never ran")
		}
		if seenReq.Key.Timeframe != "4h" {
			t.Fatalf("the regime bundle must use its own timeframe: %s", seenReq.Key.Timeframe)
		}
		market := decodeMarketEnvelope(t, seen)
		if market["snapshot_id"] != snapshotID {
			t.Fatalf("regime snapshot id: got %v want %v", market["snapshot_id"], snapshotID)
		}
		frames := market["frames"].(map[string]any)
		if _, ok := frames["BTC|4h"]; !ok {
			t.Fatalf("the regime frame must be keyed at its own timeframe: %v", keysOf(frames))
		}
	})
}

func TestRestModeLeavesEveryCheckPathOnLegacyPolling(t *testing.T) {
	_, cfg, scA, _ := feedDispatchFixture(t)
	var plainCalls int
	origPlain := runHyperliquidCheckFn
	origStdin := runHyperliquidCheckWithStdinFn
	runHyperliquidCheckFn = func(script string, args []string) (*HyperliquidResult, string, error) {
		plainCalls++
		if containsArg(args, marketStdinFlag) {
			t.Fatalf("rest mode must never declare a sealed payload: %v", args)
		}
		return &HyperliquidResult{Symbol: "BTC", Price: 101}, "", nil
	}
	runHyperliquidCheckWithStdinFn = func(string, []string, []byte) (*HyperliquidResult, string, error) {
		t.Fatalf("rest mode must never send a market payload on stdin")
		return nil, "", nil
	}
	t.Cleanup(func() {
		runHyperliquidCheckFn = origPlain
		runHyperliquidCheckWithStdinFn = origStdin
	})

	if _, _, _, ok := runHyperliquidCheck(&scA, map[string]float64{"BTC": 101}, PositionCtx{}, cfg.Regime, "simple", nil, hlBatchTestLogger(), nil, nil); !ok {
		t.Fatalf("rest mode must keep producing results")
	}
	if plainCalls != 1 {
		t.Fatalf("rest mode runs exactly one legacy check, got %d", plainCalls)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
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

	result, _, price, ok := runHyperliquidCheck(&scA, map[string]float64{"BTC": 101}, PositionCtx{}, cfg.Regime, "simple", nil, hlBatchTestLogger(), nil, feed)
	if !ok {
		t.Fatalf("a degraded evaluation must still return a result so protection can run")
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

func TestFeedOutageWithNoVerifiedMarkSkipsTheStrategy(t *testing.T) {
	feed, cfg, scA, _ := feedDispatchFixture(t)
	delete(feed.Snapshot.keys, feed.Requirements.Strategies[scA.ID].Signal)
	delete(feed.Snapshot.mids, "BTC")

	origPlain := runHyperliquidCheckFn
	runHyperliquidCheckFn = func(string, []string) (*HyperliquidResult, string, error) {
		t.Fatalf("a strategy with no verified mark must not fall back to a private fetch")
		return nil, "", nil
	}
	t.Cleanup(func() { runHyperliquidCheckFn = origPlain })

	if _, _, _, ok := runHyperliquidCheck(&scA, nil, PositionCtx{}, cfg.Regime, "simple", nil, hlBatchTestLogger(), nil, feed); ok {
		t.Fatalf("with no candle frame and no mid the strategy must be skipped, never guessed")
	}
}

func TestBatchFallsBackWhenTheSealedPayloadIsUnavailable(t *testing.T) {
	feed, cfg, scA, scB := feedDispatchFixture(t)
	delete(feed.Snapshot.keys, feed.Requirements.Strategies[scA.ID].Signal)

	orig := runHyperliquidBatchCheckFn
	runHyperliquidBatchCheckFn = func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		t.Fatalf("the batch must not run without a sealed payload")
		return nil, "", nil
	}
	t.Cleanup(func() { runHyperliquidBatchCheckFn = orig })

	key, _ := hlBatchKeyForStrategy(scA, cfg)
	var logs []string
	results := runHyperliquidBatchGroups(
		[]hlBatchGroupInput{{Key: key, Members: []StrategyConfig{scA, scB}, PosCtx: map[string]PositionCtx{}}},
		cfg, nil, func(format string, a ...any) { logs = append(logs, format) }, feed)
	if results != nil && len(results.byStrategy) != 0 {
		t.Fatalf("no member may take a batched result: %+v", results)
	}
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, " "), "sealed market payload unavailable") {
		t.Fatalf("the fallback must be reported: %v", logs)
	}
}
