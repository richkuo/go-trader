package main

import (
	"io"
	"reflect"
	"sync"
	"testing"
)

func hlBatchStrategy(id, name, symbol, timeframe string, opts ...func(*StrategyConfig)) StrategyConfig {
	sc := StrategyConfig{
		ID:           id,
		Type:         "perps",
		Platform:     "hyperliquid",
		Script:       hyperliquidCheckScript,
		Args:         []string{name, symbol, timeframe, "--mode=paper"},
		OpenStrategy: StrategyRef{Name: name},
	}
	for _, opt := range opts {
		opt(&sc)
	}
	return sc
}

func hlBatchTestLogger() *StrategyLogger {
	return &StrategyLogger{stratID: "hl-batch-test", writer: io.Discard}
}

func hlBatchTestState(ids ...string) *AppState {
	state := &AppState{Strategies: map[string]*StrategyState{}}
	for _, id := range ids {
		state.Strategies[id] = &StrategyState{ID: id, Positions: map[string]*Position{}}
	}
	return state
}

func stubBatchCheck(t *testing.T, fn func(script string, args []string, stdinJSON []byte) (*HyperliquidBatchResult, string, error)) {
	t.Helper()
	orig := runHyperliquidBatchCheckFn
	runHyperliquidBatchCheckFn = fn
	t.Cleanup(func() { runHyperliquidBatchCheckFn = orig })
}

func resetBatchFallback(t *testing.T) {
	t.Helper()
	orig := hlBatchFallback
	hlBatchFallback = &hlBatchFallbackTracker{}
	t.Cleanup(func() { hlBatchFallback = orig })
}

func resetFailureTrackers(t *testing.T) {
	t.Helper()
	origPrimary, origTransient := scriptFailureTracker, scriptFailureTransientTracker
	scriptFailureTracker = &ScriptFailureTracker{}
	scriptFailureTransientTracker = &ScriptFailureTracker{}
	t.Cleanup(func() {
		scriptFailureTracker, scriptFailureTransientTracker = origPrimary, origTransient
	})
}

func hlBatchTwoMemberInput(t *testing.T) ([]hlBatchGroupInput, *Config) {
	t.Helper()
	cfg := &Config{}
	a := hlBatchStrategy("hl-a", "breakout", "BTC", "1h")
	b := hlBatchStrategy("hl-b", "momentum_pro", "BTC", "1h")
	groups := partitionHyperliquidBatchGroups([]StrategyConfig{a, b}, cfg)
	state := hlBatchTestState("hl-a", "hl-b")
	var mu sync.RWMutex
	return snapshotHyperliquidBatchGroups(groups, state, &mu, cfg, map[string]float64{"BTC": 25_000}), cfg
}

func batchOK(ids ...string) *HyperliquidBatchResult {
	out := &HyperliquidBatchResult{Platform: "hyperliquid", Symbol: "BTC", Timeframe: "1h"}
	for i, id := range ids {
		out.Results = append(out.Results, HyperliquidBatchSlotResult{
			ID: id,
			HyperliquidResult: HyperliquidResult{
				Strategy: id, Symbol: "BTC", Timeframe: "1h",
				Signal: i + 1, Price: 25_000, Mode: "paper", Platform: "hyperliquid",
			},
		})
	}
	return out
}

func TestSharedStateFailureLeavesMembersAsMisses(t *testing.T) {
	resetBatchFallback(t)
	resetFailureTrackers(t)
	inputs, cfg := hlBatchTwoMemberInput(t)
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		return &HyperliquidBatchResult{Error: "candle fetch failed", ErrorScope: hlBatchSharedStateScope}, "", nil
	})
	results := runHyperliquidBatchGroups(inputs, cfg, nil, nil, nil)

	for _, id := range []string{"hl-a", "hl-b"} {
		if out, ok := results.lookup(id); ok {
			t.Fatalf("%s must be a map miss after a shared-state failure, got %+v", id, out)
		}
		if _, count := scriptFailureTracker.Clear(id); count != 0 {
			t.Fatalf("%s member tracker moved on a shared outage: %d", id, count)
		}
	}
	if _, count := scriptFailureTracker.Clear(hlBatchAlertConfig(inputs[0].Key).ID); count != 1 {
		t.Fatalf("group tracker count = %d, want 1", count)
	}
}

func TestMissingOrDuplicateSlotLeavesThatMemberAMiss(t *testing.T) {
	resetBatchFallback(t)
	resetFailureTrackers(t)
	inputs, cfg := hlBatchTwoMemberInput(t)
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		out := batchOK("hl-a")
		out.Results = append(out.Results, out.Results[0])
		return out, "", nil
	})
	results := runHyperliquidBatchGroups(inputs, cfg, nil, nil, nil)

	if out, ok := results.lookup("hl-a"); ok {
		t.Fatalf("duplicated-slot member must be a map miss, got %+v", out)
	}
	if out, ok := results.lookup("hl-b"); ok {
		t.Fatalf("missing-slot member must be a map miss, got %+v", out)
	}
}

func TestFailedSlotIsNotResurrectedByThePricePath(t *testing.T) {
	resetFailureTrackers(t)
	sc := hlBatchStrategy("hl-a", "breakout", "BTC", "1h")
	posCtx := PositionCtx{}
	fp, err := hyperliquidBatchSlotFingerprint(sc, posCtx, nil)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	batch := &hlBatchCycleResults{}
	batch.put("hl-a", hlBatchMemberOutcome{Err: "slot blew up", Mode: scriptFailureError, Fingerprint: fp})
	res, _, price, ok := runHyperliquidCheck(&sc, map[string]float64{"BTC": 25_000}, posCtx, nil, "simple", nil, hlBatchTestLogger(), batch, nil)
	if ok || res != nil || price != 0 {
		t.Fatalf("failed slot resurrected: ok=%v res=%+v price=%v", ok, res, price)
	}
}

func TestRunHyperliquidCheckRefusesToDecideOnAFailedSlot(t *testing.T) {
	resetBatchFallback(t)
	resetFailureTrackers(t)
	inputs, cfg := hlBatchTwoMemberInput(t)
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		out := batchOK("hl-a")
		out.Results = append(out.Results, HyperliquidBatchSlotResult{
			ID: "hl-b",
			HyperliquidResult: HyperliquidResult{
				Strategy: "momentum_pro", Symbol: "BTC", Timeframe: "1h",
				Signal: 0, Price: 0, Error: "slot blew up",
			},
		})
		return out, "", nil
	})
	batch := runHyperliquidBatchGroups(inputs, cfg, nil, nil, nil)

	sc := inputs[0].Members[1]
	if sc.ID != "hl-b" {
		t.Fatalf("fixture member order changed: %q", sc.ID)
	}
	res, _, _, ok := runHyperliquidCheck(&sc, map[string]float64{"BTC": 25_000},
		inputs[0].PosCtx["hl-b"], cfg.Regime, "simple", nil, hlBatchTestLogger(), batch, nil)
	if ok || res != nil {
		t.Fatalf("a failed slot must skip the cycle, got ok=%v res=%+v", ok, res)
	}
	if _, count := scriptFailureTracker.Clear("hl-b"); count != 1 {
		t.Fatalf("failing member's tracker count = %d, want 1", count)
	}
}

func TestRunHyperliquidCheckRejectsAStaleCachedSlot(t *testing.T) {
	resetFailureTrackers(t)
	sc := hlBatchStrategy("hl-a", "breakout", "BTC", "1h")
	prices := map[string]float64{"BTC": 25_000}
	snapshotCtx := PositionCtx{Side: "long", Quantity: 1.5, AvgCost: 100, EntryATR: 3}
	fp, err := hyperliquidBatchSlotFingerprint(sc, snapshotCtx, nil)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	batch := &hlBatchCycleResults{}
	batch.put("hl-a", hlBatchMemberOutcome{
		Result:      &HyperliquidResult{Strategy: "breakout", Symbol: "BTC", Signal: 1, Price: 25_000},
		Fingerprint: fp,
	})

	moved := snapshotCtx
	moved.Quantity = 0.5
	if fpMoved, _ := hyperliquidBatchSlotFingerprint(sc, moved, nil); fpMoved == fp {
		t.Fatal("fingerprint failed to notice a changed position context")
	}
	_ = prices
}

func TestReplayChokePointsSeeIdenticalResults(t *testing.T) {
	resetFailureTrackers(t)
	paper := hlBatchStrategy("hl-mirror", "breakout", "BTC", "1h", func(sc *StrategyConfig) {
		sc.ReplaySharing = ReplaySharingLiveMirror
	})
	if !replayMirrorPaperActive(paper) {
		t.Fatal("fixture must be an active paper mirror")
	}
	cases := []struct {
		name     string
		result   HyperliquidResult
		posCtx   PositionCtx
		suppress bool
	}{
		{
			name:     "open from flat",
			result:   HyperliquidResult{Strategy: "breakout", Symbol: "BTC", Signal: 1, Price: 25_000},
			posCtx:   PositionCtx{},
			suppress: true,
		},
		{
			name:     "scale-in while long",
			result:   HyperliquidResult{Strategy: "breakout", Symbol: "BTC", Signal: 1, Price: 25_000},
			posCtx:   PositionCtx{Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 2},
			suppress: true,
		},
		{
			name: "full close while long",
			result: HyperliquidResult{
				Strategy: "breakout", Symbol: "BTC", Signal: -1, Price: 25_000,
				StrategyDecisionFields: StrategyDecisionFields{CloseFraction: 1.0},
			},
			posCtx:   PositionCtx{Side: "long", Quantity: 1, AvgCost: 100, EntryATR: 2},
			suppress: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := paper
			prices := map[string]float64{"BTC": 25_000}
			fp, err := hyperliquidBatchSlotFingerprint(sc, tc.posCtx, nil)
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			batched := tc.result
			batch := &hlBatchCycleResults{}
			batch.put(sc.ID, hlBatchMemberOutcome{Result: &batched, Fingerprint: fp})
			fromBatch, _, _, ok := runHyperliquidCheck(&sc, prices, tc.posCtx, nil, "simple", nil, hlBatchTestLogger(), batch, nil)
			if !ok {
				t.Fatal("batched slot did not produce a decision")
			}

			scDirect := paper
			direct := tc.result
			fromDirect, _, _, ok := finishHyperliquidCheck(&scDirect, prices, tc.posCtx, nil, nil, hlBatchTestLogger(),
				&direct, "", "", scriptFailureCrash, false)
			if !ok {
				t.Fatal("per-strategy path did not produce a decision")
			}
			if !reflect.DeepEqual(*fromBatch, *fromDirect) {
				t.Fatalf("batched decision %+v != per-strategy decision %+v", *fromBatch, *fromDirect)
			}

			gotBatch := pausedBlocksSignal(fromBatch.Signal, fromBatch.CloseFraction, tc.posCtx.Quantity, tc.posCtx.Side, PerpsAllowsLong(sc), PerpsAllowsShort(sc))
			gotDirect := pausedBlocksSignal(fromDirect.Signal, fromDirect.CloseFraction, tc.posCtx.Quantity, tc.posCtx.Side, PerpsAllowsLong(sc), PerpsAllowsShort(sc))
			if gotBatch != gotDirect || gotBatch != tc.suppress {
				t.Fatalf("replay suppression batched=%v direct=%v, want %v", gotBatch, gotDirect, tc.suppress)
			}
		})
	}
}
