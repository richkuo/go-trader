package main

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fixtures --------------------------------------------------------------

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

// --- grouping --------------------------------------------------------------

func TestPartitionHyperliquidBatchGroups(t *testing.T) {
	cfg := &Config{}
	due := []StrategyConfig{
		hlBatchStrategy("hl-btc-a", "breakout", "BTC", "1h"),
		hlBatchStrategy("hl-btc-b", "momentum_pro", "BTC", "1h"),
		hlBatchStrategy("hl-btc-4h", "breakout", "BTC", "4h"),
		hlBatchStrategy("hl-eth-a", "breakout", "ETH", "1h"),
		hlBatchStrategy("hl-eth-b", "momentum_pro", "ETH", "1h"),
		// Different resolved ATR method: same coin and timeframe, different key.
		hlBatchStrategy("hl-btc-wilder", "breakout", "BTC", "1h", func(sc *StrategyConfig) {
			sc.ATRMethod = "wilder"
		}),
		// Excluded: a forked check script the batch mode may not implement.
		hlBatchStrategy("hl-btc-custom", "breakout", "BTC", "1h", func(sc *StrategyConfig) {
			sc.Script = "shared_scripts/check_hyperliquid_custom.py"
		}),
		// Excluded: not HL perps.
		{ID: "okx-btc", Type: "perps", Platform: "okx", Script: hyperliquidCheckScript,
			Args: []string{"breakout", "BTC", "1h", "--mode=paper"}},
		{ID: "spot-btc", Type: "spot", Platform: "binanceus", Script: "shared_scripts/check_strategy.py",
			Args: []string{"breakout", "BTC/USDT", "1h"}},
		{ID: "manual-btc", Type: "manual", Platform: "hyperliquid", Script: hyperliquidCheckScript,
			Args: []string{"hold", "BTC", "1h", "--mode=live"}},
		// Excluded: an argv shape the slot builder does not know how to carry.
		hlBatchStrategy("hl-btc-extra", "breakout", "BTC", "1h", func(sc *StrategyConfig) {
			sc.Args = append(sc.Args, "--params", `{"lookback":30}`)
		}),
	}

	groups := partitionHyperliquidBatchGroups(due, cfg)
	got := map[string][]string{}
	for _, g := range groups {
		got[g.Key.String()] = g.MemberIDs()
	}
	want := map[string][]string{
		"hyperliquid/BTC/1h/limit=200/atr=simple": {"hl-btc-a", "hl-btc-b"},
		"hyperliquid/BTC/4h/limit=200/atr=simple": {"hl-btc-4h"},
		"hyperliquid/ETH/1h/limit=200/atr=simple": {"hl-eth-a", "hl-eth-b"},
		"hyperliquid/BTC/1h/limit=200/atr=wilder": {"hl-btc-wilder"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}

	batchable := 0
	for _, g := range groups {
		if g.Batchable() {
			batchable++
		}
	}
	if batchable != 2 {
		t.Fatalf("batchable groups = %d, want 2 (single-member groups must not batch)", batchable)
	}
}

func TestPartitionKeepsGroupsSortedAndMembersInDueOrder(t *testing.T) {
	due := []StrategyConfig{
		hlBatchStrategy("hl-eth-b", "breakout", "ETH", "1h"),
		hlBatchStrategy("hl-btc-b", "breakout", "BTC", "1h"),
		hlBatchStrategy("hl-eth-a", "breakout", "ETH", "1h"),
		hlBatchStrategy("hl-btc-a", "breakout", "BTC", "1h"),
	}
	groups := partitionHyperliquidBatchGroups(due, &Config{})
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if groups[0].Key.Symbol != "BTC" || groups[1].Key.Symbol != "ETH" {
		t.Fatalf("groups not sorted by key: %v, %v", groups[0].Key, groups[1].Key)
	}
	if got := groups[0].MemberIDs(); !reflect.DeepEqual(got, []string{"hl-btc-b", "hl-btc-a"}) {
		t.Fatalf("members lost due order: %v", got)
	}
}

func TestPartitionBatchesExactlyTheDueSet(t *testing.T) {
	all := []StrategyConfig{
		hlBatchStrategy("hl-a", "breakout", "BTC", "1h"),
		hlBatchStrategy("hl-b", "momentum_pro", "BTC", "1h"),
		hlBatchStrategy("hl-not-due", "breakout", "BTC", "1h"),
	}
	due := all[:2] // hl-not-due's interval has not elapsed.
	groups := partitionHyperliquidBatchGroups(due, &Config{})
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if got := groups[0].MemberIDs(); !reflect.DeepEqual(got, []string{"hl-a", "hl-b"}) {
		t.Fatalf("batch = %v, want exactly the due set", got)
	}
}

func TestPartitionTakesNoLocks(t *testing.T) {
	// Grouping must be pure: hold the state write lock and assert the
	// partition still completes. A partition that reached for mu would hang.
	var mu sync.RWMutex
	mu.Lock()
	defer mu.Unlock()
	done := make(chan int, 1)
	go func() {
		groups := partitionHyperliquidBatchGroups([]StrategyConfig{
			hlBatchStrategy("hl-a", "breakout", "BTC", "1h"),
			hlBatchStrategy("hl-b", "breakout", "BTC", "1h"),
		}, &Config{})
		done <- len(groups)
	}()
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("groups = %d, want 1", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("partitionHyperliquidBatchGroups blocked on a lock; it must be pure")
	}
}

func TestBatchKeyUsesRegimeOhlcvLimitWhenRegimeEnabled(t *testing.T) {
	cfg := &Config{Regime: &RegimeConfig{Enabled: true, Period: 30}}
	key, ok := hlBatchKeyForStrategy(hlBatchStrategy("hl-a", "breakout", "BTC", "1h"), cfg)
	if !ok {
		t.Fatal("expected a batch key")
	}
	if want := regimeRequiredOhlcvLimit(cfg.Regime); key.OhlcvLimit != want {
		t.Fatalf("OhlcvLimit = %d, want %d", key.OhlcvLimit, want)
	}
	disabled, _ := hlBatchKeyForStrategy(hlBatchStrategy("hl-a", "breakout", "BTC", "1h"), &Config{})
	if disabled.OhlcvLimit != hlBatchPythonDefaultOhlcvLimit {
		t.Fatalf("regime-disabled OhlcvLimit = %d, want the script default %d",
			disabled.OhlcvLimit, hlBatchPythonDefaultOhlcvLimit)
	}
}

func TestHyperliquidBatchArgsSupported(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"three positionals", []string{"breakout", "BTC", "1h"}, true},
		{"mode joined", []string{"breakout", "BTC", "1h", "--mode=live"}, true},
		{"mode split", []string{"breakout", "BTC", "1h", "--mode", "live"}, true},
		{"too short", []string{"breakout", "BTC"}, false},
		{"flag as positional", []string{"--htf-filter", "BTC", "1h"}, false},
		{"unknown flag", []string{"breakout", "BTC", "1h", "--htf-filter"}, false},
		{"dangling mode", []string{"breakout", "BTC", "1h", "--mode"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hyperliquidBatchArgsSupported(tc.args); got != tc.want {
				t.Fatalf("hyperliquidBatchArgsSupported(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// --- slot construction mirrors the per-strategy argv ------------------------

func TestSlotCarriesEveryNonKeyArgument(t *testing.T) {
	rc := &RegimeConfig{Enabled: true}
	sc := hlBatchStrategy("hl-a", "breakout", "BTC", "1h", func(sc *StrategyConfig) {
		sc.Args = []string{"breakout", "BTC", "1h", "--mode=live"}
		sc.HTFFilter = true
		sc.OpenStrategy = StrategyRef{Name: "breakout", Params: map[string]interface{}{"lookback": 30.0}}
		sc.CloseStrategy = &StrategyRef{Name: "atr_stop", Params: map[string]interface{}{"atr_multiple": 2.0}}
	})
	posCtx := PositionCtx{
		Side: "long", AvgCost: 101.5, Quantity: 1.25, InitialQuantity: 2.0,
		EntryATR: 3.5, Regime: "trending_up",
	}

	slot, err := buildHyperliquidBatchSlot(sc, posCtx, rc)
	if err != nil {
		t.Fatalf("buildHyperliquidBatchSlot: %v", err)
	}
	if slot.ID != "hl-a" || slot.Strategy != "breakout" || slot.Mode != "live" || !slot.HTFFilter {
		t.Fatalf("slot header wrong: %+v", slot)
	}
	if slot.PositionSide != "long" {
		t.Fatalf("PositionSide = %q", slot.PositionSide)
	}
	want := map[string]any{
		"side": "long", "avg_cost": 101.5, "current_quantity": 1.25,
		"initial_quantity": 2.0, "entry_atr": 3.5, "regime": "trending_up",
	}
	if !reflect.DeepEqual(slot.PositionCtx, want) {
		t.Fatalf("PositionCtx = %v, want %v", slot.PositionCtx, want)
	}

	// The refs blob must equal the one the per-strategy argv carries.
	refsArgs, err := buildStrategyRefsArg(strategyConfigWithOnChainProtectionFilter(sc))
	if err != nil {
		t.Fatalf("buildStrategyRefsArg: %v", err)
	}
	if string(slot.StrategyRefs) != refsArgs[1] {
		t.Fatalf("StrategyRefs = %s, want %s", slot.StrategyRefs, refsArgs[1])
	}
}

func TestSlotOmitsPositionFieldsExactlyLikeTheArgvBuilder(t *testing.T) {
	rc := &RegimeConfig{Enabled: true}
	sc := hlBatchStrategy("hl-a", "breakout", "BTC", "1h")
	// Zero floats are skipped by appendPositionFloatArg, so the slot must omit
	// them too — otherwise Python would see 0.0 where argparse gave it None.
	posCtx := PositionCtx{Side: "short", AvgCost: 0, Quantity: 0, EntryATR: 4}
	slot, err := buildHyperliquidBatchSlot(sc, posCtx, rc)
	if err != nil {
		t.Fatalf("buildHyperliquidBatchSlot: %v", err)
	}
	want := map[string]any{"side": "short", "entry_atr": 4.0}
	if !reflect.DeepEqual(slot.PositionCtx, want) {
		t.Fatalf("PositionCtx = %v, want %v", slot.PositionCtx, want)
	}

	// A strategy that uses neither an open ref nor a close ref gets no
	// position context at all, matching appendOpenCloseArgs' early return.
	plain := hlBatchStrategy("hl-plain", "breakout", "BTC", "1h", func(sc *StrategyConfig) {
		sc.OpenStrategy = StrategyRef{}
	})
	plainSlot, err := buildHyperliquidBatchSlot(plain, posCtx, rc)
	if err != nil {
		t.Fatalf("buildHyperliquidBatchSlot: %v", err)
	}
	if plainSlot.PositionCtx != nil || plainSlot.PositionSide != "" {
		t.Fatalf("expected no position context, got %+v", plainSlot)
	}
}

func TestStrategiesDifferingOffKeyStillBatchWithTheirOwnValues(t *testing.T) {
	rc := &RegimeConfig{Enabled: true}
	cfg := &Config{Regime: rc}
	a := hlBatchStrategy("hl-a", "breakout", "BTC", "1h", func(sc *StrategyConfig) {
		sc.HTFFilter = true
		sc.CloseStrategy = &StrategyRef{Name: "atr_stop"}
	})
	b := hlBatchStrategy("hl-b", "momentum_pro", "BTC", "1h", func(sc *StrategyConfig) {
		sc.Args = []string{"momentum_pro", "BTC", "1h", "--mode=live"}
	})
	groups := partitionHyperliquidBatchGroups([]StrategyConfig{a, b}, cfg)
	if len(groups) != 1 || !groups[0].Batchable() {
		t.Fatalf("expected one batchable group, got %v", groups)
	}
	slotA, _ := buildHyperliquidBatchSlot(a, PositionCtx{Side: "long", Quantity: 1}, rc)
	slotB, _ := buildHyperliquidBatchSlot(b, PositionCtx{}, rc)
	if !slotA.HTFFilter || slotB.HTFFilter {
		t.Fatalf("HTF filter did not travel per slot: %v / %v", slotA.HTFFilter, slotB.HTFFilter)
	}
	if slotA.Mode != "paper" || slotB.Mode != "live" {
		t.Fatalf("mode did not travel per slot: %q / %q", slotA.Mode, slotB.Mode)
	}
	if slotA.PositionCtx == nil || slotB.PositionCtx != nil {
		t.Fatalf("position context did not travel per slot: %v / %v", slotA.PositionCtx, slotB.PositionCtx)
	}
}

func TestSharedArgsCarryOnlyKeyAndSharedFlags(t *testing.T) {
	rc := &RegimeConfig{Enabled: true}
	key := hlBatchKey{DataPlatform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", OhlcvLimit: 250, ATRMethod: "wilder"}
	args := hlBatchSharedArgs(key, rc, `{"default":{}}`, true, 25_000)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--batch-check", "--symbol=BTC", "--timeframe=1h", "--ohlcv-limit 250",
		"--atr-method=wilder", "--regime-enabled", "--regime-payload-json", "--mark-price=25000",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("shared argv %q missing %q", joined, want)
		}
	}
	// Nothing per-slot may leak into the shared argv.
	for _, banned := range []string{"--position-", "--strategy-refs", "--htf-filter", "--regime-atr-window", "--mode"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("shared argv %q leaked per-slot flag %q", joined, banned)
		}
	}
	if noMark := strings.Join(hlBatchSharedArgs(key, rc, "", false, 0), " "); strings.Contains(noMark, "--mark-price") {
		t.Fatalf("mark price must be omitted when the cycle has no mid: %q", noMark)
	}
}

// --- response parsing ------------------------------------------------------

func TestBatchSlotsParseThroughTheHyperliquidResultContract(t *testing.T) {
	single := `{"strategy":"breakout","symbol":"BTC","timeframe":"1h","signal":1,"price":109000.12,
	  "indicators":{"atr":123.4},"regime":{"default":{"regime":"trending_up"}},"mode":"live",
	  "platform":"hyperliquid","timestamp":"2026-08-21T00:00:00+00:00","open_action":"long","close_fraction":0}`
	batch := `{"platform":"hyperliquid","symbol":"BTC","timeframe":"1h",
	  "timestamp":"2026-08-21T00:00:00+00:00","error":"","error_scope":"",
	  "results":[{"id":"hl-a",` + strings.TrimPrefix(strings.TrimSpace(single), "{") + `]}`

	var want HyperliquidResult
	if err := json.Unmarshal([]byte(single), &want); err != nil {
		t.Fatalf("single unmarshal: %v", err)
	}
	got, err := parseHyperliquidBatchOutput([]byte(batch))
	if err != nil {
		t.Fatalf("parseHyperliquidBatchOutput: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].ID != "hl-a" {
		t.Fatalf("results = %+v", got.Results)
	}
	if !reflect.DeepEqual(got.Results[0].HyperliquidResult, want) {
		t.Fatalf("batched slot = %+v, want the single-mode parse %+v", got.Results[0].HyperliquidResult, want)
	}
}

func TestParseBatchOutputRejectsGarbage(t *testing.T) {
	if _, err := parseHyperliquidBatchOutput([]byte("not json")); err == nil {
		t.Fatal("expected a parse error")
	}
}

// --- dispatch, isolation and failure semantics ------------------------------

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

func TestBatchDispatchCachesOneResultPerMember(t *testing.T) {
	resetBatchFallback(t)
	inputs, cfg := hlBatchTwoMemberInput(t)
	var gotArgs []string
	var gotStdin []byte
	stubBatchCheck(t, func(script string, args []string, stdinJSON []byte) (*HyperliquidBatchResult, string, error) {
		gotArgs, gotStdin = args, stdinJSON
		return batchOK("hl-a", "hl-b"), "", nil
	})
	results := runHyperliquidBatchGroups(inputs, cfg, nil, nil)

	if gotArgs[0] != "--batch-check" {
		t.Fatalf("argv = %v", gotArgs)
	}
	var req hlBatchRequest
	if err := json.Unmarshal(gotStdin, &req); err != nil {
		t.Fatalf("stdin unmarshal: %v", err)
	}
	if req.Version != hlBatchProtocolVersion || len(req.Slots) != 2 {
		t.Fatalf("stdin envelope = %+v", req)
	}
	for _, id := range []string{"hl-a", "hl-b"} {
		out, ok := results.lookup(id)
		if !ok || out.Result == nil || out.Err != "" {
			t.Fatalf("%s outcome = %+v", id, out)
		}
	}
}

func TestBatchSlotErrorIsolatesOneMember(t *testing.T) {
	resetBatchFallback(t)
	resetFailureTrackers(t)
	inputs, cfg := hlBatchTwoMemberInput(t)
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		out := batchOK("hl-a")
		out.Results = append(out.Results, HyperliquidBatchSlotResult{
			ID:                "hl-b",
			HyperliquidResult: HyperliquidResult{Strategy: "hl-b", Error: "boom in this slot"},
		})
		return out, "", nil
	})
	results := runHyperliquidBatchGroups(inputs, cfg, nil, nil)

	good, _ := results.lookup("hl-a")
	if good.Result == nil || good.Result.Signal != 1 {
		t.Fatalf("healthy peer disturbed: %+v", good)
	}
	bad, _ := results.lookup("hl-b")
	// The error payload must NOT survive as a Result: a zero-signal error
	// object read as a decision would look like a legitimate hold.
	if bad.Result != nil || bad.Err != "boom in this slot" || bad.Mode != scriptFailureError {
		t.Fatalf("failing slot outcome = %+v", bad)
	}

	// The failing slot must take the per-strategy soft-error branch, and only
	// that strategy's tracker may move.
	sc := hlBatchStrategy("hl-b", "momentum_pro", "BTC", "1h")
	res, _, _, ok := finishHyperliquidCheck(&sc, nil, PositionCtx{}, nil, nil, hlBatchTestLogger(),
		bad.Result, bad.Stderr, bad.Err, bad.Mode, bad.SharedFailure)
	if ok || res != nil {
		t.Fatalf("failing slot must not produce a decision: ok=%v res=%v", ok, res)
	}
	if _, count := scriptFailureTracker.Clear("hl-b"); count != 1 {
		t.Fatalf("member tracker count = %d, want 1", count)
	}
	if _, count := scriptFailureTracker.Clear("hl-a"); count != 0 {
		t.Fatalf("healthy peer's tracker moved: %d", count)
	}
}

func TestSharedStateFailureFreezesMemberTrackers(t *testing.T) {
	resetBatchFallback(t)
	resetFailureTrackers(t)
	inputs, cfg := hlBatchTwoMemberInput(t)
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		return &HyperliquidBatchResult{Error: "candle fetch failed", ErrorScope: hlBatchSharedStateScope}, "", nil
	})
	results := runHyperliquidBatchGroups(inputs, cfg, nil, nil)

	for _, id := range []string{"hl-a", "hl-b"} {
		out, ok := results.lookup(id)
		if !ok || !out.SharedFailure || out.Result != nil || out.Mode != scriptFailureSharedState {
			t.Fatalf("%s outcome = %+v", id, out)
		}
		sc := hlBatchStrategy(id, "breakout", "BTC", "1h")
		if _, _, _, decided := finishHyperliquidCheck(&sc, nil, PositionCtx{}, nil, nil, hlBatchTestLogger(),
			nil, "", out.Err, out.Mode, true); decided {
			t.Fatalf("%s must not decide on a shared-state failure", id)
		}
		if _, count := scriptFailureTracker.Clear(id); count != 0 {
			t.Fatalf("%s member tracker moved on a shared outage: %d", id, count)
		}
	}
	// The group identity carries the streak instead.
	if _, count := scriptFailureTracker.Clear(hlBatchAlertConfig(inputs[0].Key).ID); count != 1 {
		t.Fatalf("group tracker count = %d, want 1", count)
	}
}

func TestSharedStateFailureDoesNotFalselyClearAMemberStreak(t *testing.T) {
	resetBatchFallback(t)
	resetFailureTrackers(t)
	// hl-a is already failing on its own account.
	scriptFailureTracker.Record("hl-a", "prior failure", time.Now().UTC())
	inputs, cfg := hlBatchTwoMemberInput(t)
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		return nil, "", fmt.Errorf("batch script error: exit 1")
	})
	results := runHyperliquidBatchGroups(inputs, cfg, nil, nil)
	out, _ := results.lookup("hl-a")
	sc := hlBatchStrategy("hl-a", "breakout", "BTC", "1h")
	finishHyperliquidCheck(&sc, nil, PositionCtx{}, nil, nil, hlBatchTestLogger(),
		nil, "", out.Err, out.Mode, true)
	if _, count := scriptFailureTracker.Clear("hl-a"); count != 1 {
		t.Fatalf("prior member streak = %d, want it preserved at 1", count)
	}
}

func TestMissingOrDuplicateSlotFailsOnlyThatMember(t *testing.T) {
	resetBatchFallback(t)
	inputs, cfg := hlBatchTwoMemberInput(t)
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		out := batchOK("hl-a")
		out.Results = append(out.Results, out.Results[0]) // duplicate hl-a, hl-b missing
		return out, "", nil
	})
	results := runHyperliquidBatchGroups(inputs, cfg, nil, nil)

	dup, _ := results.lookup("hl-a")
	if dup.Result != nil || !strings.Contains(dup.Err, "duplicate") || dup.Mode != scriptFailureCrash {
		t.Fatalf("duplicate-slot outcome = %+v", dup)
	}
	missing, _ := results.lookup("hl-b")
	if missing.Result != nil || !strings.Contains(missing.Err, "missing slot") || missing.Mode != scriptFailureCrash {
		t.Fatalf("missing-slot outcome = %+v", missing)
	}
}

func TestBatchTimeoutStaysTransient(t *testing.T) {
	// Batching concentrates N strategies under one deadline; the timeout text
	// must still classify transient so it does not trip the 3-strike alert.
	err := &pythonScriptTimeoutError{d: hlBatchTimeout}
	if !scriptFailureErrorIsTransient(err.Error()) {
		t.Fatalf("batch timeout %q must classify transient", err.Error())
	}
}

// --- fallback safety valve --------------------------------------------------

func TestSharedFailureFallbackAfterThreeStrikes(t *testing.T) {
	resetBatchFallback(t)
	key := hlBatchKey{DataPlatform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", OhlcvLimit: 200, ATRMethod: "simple"}
	for i := 1; i <= hlBatchSharedFailureFallbackThreshold; i++ {
		if !hlBatchFallback.Allow(key) {
			t.Fatalf("batching blocked before the threshold (strike %d)", i)
		}
		tripped := hlBatchFallback.RecordSharedFailure(key)
		if want := i == hlBatchSharedFailureFallbackThreshold; tripped != want {
			t.Fatalf("strike %d tripped = %v, want %v", i, tripped, want)
		}
	}
	for i := 1; i < hlBatchFallbackRetryEvery; i++ {
		if hlBatchFallback.Allow(key) {
			t.Fatalf("group re-batched at cycle %d, want it held until cycle %d", i, hlBatchFallbackRetryEvery)
		}
	}
	if !hlBatchFallback.Allow(key) {
		t.Fatalf("group never retried after %d cycles", hlBatchFallbackRetryEvery)
	}
	if !hlBatchFallback.RecordSuccess(key) {
		t.Fatal("a success after fallback should report recovery")
	}
	if !hlBatchFallback.Allow(key) {
		t.Fatal("a recovered group must batch again")
	}
}

func TestPrePassSkipsAFallenBackGroup(t *testing.T) {
	resetBatchFallback(t)
	resetFailureTrackers(t)
	cfg := &Config{}
	due := []StrategyConfig{
		hlBatchStrategy("hl-a", "breakout", "BTC", "1h"),
		hlBatchStrategy("hl-b", "momentum_pro", "BTC", "1h"),
	}
	key, _ := hlBatchKeyForStrategy(due[0], cfg)
	for i := 0; i < hlBatchSharedFailureFallbackThreshold; i++ {
		hlBatchFallback.RecordSharedFailure(key)
	}
	calls := 0
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		calls++
		return batchOK("hl-a", "hl-b"), "", nil
	})
	state := hlBatchTestState("hl-a", "hl-b")
	var mu sync.RWMutex
	if got := runHyperliquidBatchPrePass(due, state, &mu, cfg, nil, nil, nil); got != nil {
		t.Fatalf("fallen-back group must produce no batched results, got %+v", got)
	}
	if calls != 0 {
		t.Fatalf("batch subprocess called %d times while in fallback", calls)
	}
}

// --- pre-pass negative criteria --------------------------------------------

func TestPrePassNeverBatchesASingleMemberGroup(t *testing.T) {
	resetBatchFallback(t)
	calls := 0
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		calls++
		return batchOK("hl-a"), "", nil
	})
	cfg := &Config{}
	due := []StrategyConfig{
		hlBatchStrategy("hl-a", "breakout", "BTC", "1h"),
		hlBatchStrategy("hl-eth", "breakout", "ETH", "1h"),
	}
	state := hlBatchTestState("hl-a", "hl-eth")
	var mu sync.RWMutex
	if got := runHyperliquidBatchPrePass(due, state, &mu, cfg, nil, nil, nil); got != nil {
		t.Fatalf("single-member groups must produce no batch, got %+v", got)
	}
	if calls != 0 {
		t.Fatalf("batch subprocess called %d times for single-member groups", calls)
	}
}

func TestPrePassHonoursTheEnvironmentKillSwitch(t *testing.T) {
	resetBatchFallback(t)
	t.Setenv(hlBatchDisabledEnv, "0")
	if hyperliquidBatchEnabled() {
		t.Fatal("GO_TRADER_HL_BATCH=0 must disable batching")
	}
	calls := 0
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		calls++
		return batchOK("hl-a", "hl-b"), "", nil
	})
	cfg := &Config{}
	due := []StrategyConfig{
		hlBatchStrategy("hl-a", "breakout", "BTC", "1h"),
		hlBatchStrategy("hl-b", "momentum_pro", "BTC", "1h"),
	}
	state := hlBatchTestState("hl-a", "hl-b")
	var mu sync.RWMutex
	if got := runHyperliquidBatchPrePass(due, state, &mu, cfg, nil, nil, nil); got != nil {
		t.Fatalf("kill switch must produce no batched results, got %+v", got)
	}
	if calls != 0 {
		t.Fatalf("batch subprocess called %d times with the kill switch set", calls)
	}
	t.Setenv(hlBatchDisabledEnv, "")
	if !hyperliquidBatchEnabled() {
		t.Fatal("an unset kill switch must leave batching enabled")
	}
}

func TestSnapshotSkipsStrategiesWithNoState(t *testing.T) {
	cfg := &Config{}
	due := []StrategyConfig{
		hlBatchStrategy("hl-a", "breakout", "BTC", "1h"),
		hlBatchStrategy("hl-b", "momentum_pro", "BTC", "1h"),
	}
	groups := partitionHyperliquidBatchGroups(due, cfg)
	var mu sync.RWMutex
	// Only one member has state, so the group drops below two and is not batched.
	inputs := snapshotHyperliquidBatchGroups(groups, hlBatchTestState("hl-a"), &mu, cfg, nil)
	if len(inputs) != 0 {
		t.Fatalf("inputs = %+v, want none", inputs)
	}
}

func TestSnapshotReadsPositionsUnderTheReadLock(t *testing.T) {
	cfg := &Config{}
	due := []StrategyConfig{
		hlBatchStrategy("hl-a", "breakout", "BTC", "1h"),
		hlBatchStrategy("hl-b", "momentum_pro", "BTC", "1h"),
	}
	state := hlBatchTestState("hl-a", "hl-b")
	state.Strategies["hl-a"].Positions["BTC"] = &Position{
		Symbol: "BTC", Side: "long", Quantity: 1.5, InitialQuantity: 2, AvgCost: 100, EntryATR: 3,
	}
	var mu sync.RWMutex
	inputs := snapshotHyperliquidBatchGroups(partitionHyperliquidBatchGroups(due, cfg), state, &mu, cfg, map[string]float64{"BTC": 25_000})
	if len(inputs) != 1 {
		t.Fatalf("inputs = %d, want 1", len(inputs))
	}
	if got := inputs[0].PosCtx["hl-a"]; got.Side != "long" || got.Quantity != 1.5 {
		t.Fatalf("snapshotted position = %+v", got)
	}
	if got := inputs[0].PosCtx["hl-b"]; got.Side != "" {
		t.Fatalf("flat member should snapshot an empty context, got %+v", got)
	}
	if inputs[0].MarkPrice != 25_000 {
		t.Fatalf("MarkPrice = %v", inputs[0].MarkPrice)
	}
	// The read lock must have been released before returning.
	if !mu.TryLock() {
		t.Fatal("snapshot did not release the read lock")
	}
	mu.Unlock()
}

// --- consumption inside runHyperliquidCheck ---------------------------------

func TestRunHyperliquidCheckConsumesTheCachedSlot(t *testing.T) {
	resetFailureTrackers(t)
	sc := hlBatchStrategy("hl-a", "breakout", "BTC", "1h")
	posCtx := PositionCtx{Side: "long", Quantity: 1.5, AvgCost: 100, EntryATR: 3}
	prices := map[string]float64{"BTC": 25_000}
	fp, err := hyperliquidBatchSlotFingerprint(sc, posCtx, nil, 25_000)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	cached := &HyperliquidResult{Strategy: "breakout", Symbol: "BTC", Timeframe: "1h", Signal: 1, Price: 25_000, Mode: "paper"}
	batch := &hlBatchCycleResults{}
	batch.put("hl-a", hlBatchMemberOutcome{Result: cached, Fingerprint: fp})

	got, signalStr, price, ok := runHyperliquidCheck(&sc, prices, posCtx, nil, "simple", nil, hlBatchTestLogger(), batch)
	if !ok || got != cached || price != 25_000 || signalStr != signalLabel(1) {
		t.Fatalf("batched consumption = (%v, %q, %v, %v)", got, signalStr, price, ok)
	}
}

func TestRunHyperliquidCheckRefusesToDecideOnAFailedSlot(t *testing.T) {
	// A slot error payload carries signal 0. If it reached the dispatch loop
	// as a Result, the strategy would run its whole downstream block on a
	// fabricated hold instead of skipping the cycle.
	resetBatchFallback(t)
	resetFailureTrackers(t)
	inputs, cfg := hlBatchTwoMemberInput(t)
	stubBatchCheck(t, func(string, []string, []byte) (*HyperliquidBatchResult, string, error) {
		out := batchOK("hl-a")
		out.Results = append(out.Results, HyperliquidBatchSlotResult{
			ID: "hl-b",
			// The real slot-error payload: signal 0, price 0, symbol set. The
			// cycle's mid would resolve the price, so a Result that survived
			// here would read as a perfectly ordinary HOLD decision.
			HyperliquidResult: HyperliquidResult{
				Strategy: "momentum_pro", Symbol: "BTC", Timeframe: "1h",
				Signal: 0, Price: 0, Error: "slot blew up",
			},
		})
		return out, "", nil
	})
	batch := runHyperliquidBatchGroups(inputs, cfg, nil, nil)

	sc := inputs[0].Members[1]
	if sc.ID != "hl-b" {
		t.Fatalf("fixture member order changed: %q", sc.ID)
	}
	res, _, _, ok := runHyperliquidCheck(&sc, map[string]float64{"BTC": 25_000},
		inputs[0].PosCtx["hl-b"], cfg.Regime, "simple", nil, hlBatchTestLogger(), batch)
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
	fp, err := hyperliquidBatchSlotFingerprint(sc, snapshotCtx, nil, 25_000)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	batch := &hlBatchCycleResults{}
	batch.put("hl-a", hlBatchMemberOutcome{
		Result:      &HyperliquidResult{Strategy: "breakout", Symbol: "BTC", Signal: 1, Price: 25_000},
		Fingerprint: fp,
	})

	// The position was partially closed between the snapshot and this
	// strategy's own iteration, so the cached decision is not about the
	// current state and must be discarded.
	moved := snapshotCtx
	moved.Quantity = 0.5
	if fpMoved, _ := hyperliquidBatchSlotFingerprint(sc, moved, nil, 25_000); fpMoved == fp {
		t.Fatal("fingerprint failed to notice a changed position context")
	}
	// A changed cycle mark price is equally disqualifying.
	if fpMark, _ := hyperliquidBatchSlotFingerprint(sc, snapshotCtx, nil, 26_000); fpMark == fp {
		t.Fatal("fingerprint failed to notice a changed mark price")
	}
	_ = prices
}

// --- the #1431 replay choke points -----------------------------------------

// TestReplayChokePointsSeeIdenticalResults drives the paper-side suppression
// arm (main.go's replayMirrorPaperActive + pausedBlocksSignal gate) with a
// decision that came from a batched slot and with the same decision from the
// per-strategy path. Both must gate identically for an open, a scale-in and a
// full close, because the batch only replaces the subprocess.
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
			fp, err := hyperliquidBatchSlotFingerprint(sc, tc.posCtx, nil, 25_000)
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			batched := tc.result
			batch := &hlBatchCycleResults{}
			batch.put(sc.ID, hlBatchMemberOutcome{Result: &batched, Fingerprint: fp})
			fromBatch, _, _, ok := runHyperliquidCheck(&sc, prices, tc.posCtx, nil, "simple", nil, hlBatchTestLogger(), batch)
			if !ok {
				t.Fatal("batched slot did not produce a decision")
			}

			// The per-strategy path funnels the same parsed result through the
			// same finishing pipeline.
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

// --- alerts ----------------------------------------------------------------

func TestBatchSharedStateAlertRendersDistinctly(t *testing.T) {
	key := hlBatchKey{DataPlatform: "hyperliquid", Symbol: "BTC", Timeframe: "1h", OhlcvLimit: 200, ATRMethod: "simple"}
	sc := hlBatchAlertConfig(key)
	if sc.ID != "hl-batch[BTC/1h]" {
		t.Fatalf("synthetic identity = %q", sc.ID)
	}
	msg := formatBatchSharedStateFailureAlert(sc, "candle fetch failed", []string{"hl-a", "hl-b"}, 3)
	if !strings.HasPrefix(msg, "**HL BATCH SHARED STATE FAILED**") {
		t.Fatalf("alert must be visually distinct from a per-strategy failure: %q", msg)
	}
	if strings.Contains(msg, "**SIGNAL SCRIPT FAILING**") {
		t.Fatalf("alert must not reuse the per-strategy wording: %q", msg)
	}
	for _, want := range []string{"hl-a", "hl-b", "2 strategies affected", "candle fetch failed"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("alert %q missing %q", msg, want)
		}
	}
	if label := scriptFailureModeLabel(scriptFailureSharedState); label != "shared market state" {
		t.Fatalf("mode label = %q", label)
	}
}
