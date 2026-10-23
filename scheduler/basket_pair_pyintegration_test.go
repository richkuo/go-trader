//go:build pyintegration

package main

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func pairPyEnv(t *testing.T, mutate func(root map[string]interface{})) (*pairTestEnv, string) {
	t.Helper()
	root := basketPyRoot(t)
	e := newPairTestEnv(t, func(r map[string]interface{}) {
		r["market_feed"] = "websocket"
		if mutate != nil {
			mutate(r)
		}
	})
	return e, root
}

func pairPyFeed(t *testing.T, cfg *Config) *marketFeedContext {
	t.Helper()
	feedReq, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Truncate(time.Hour)
	snap := basketTestSnapshot(deadline, 260, map[string]float64{"BTC": 60000}, 1)
	snap.Deadline = time.Time{}
	snap.SealedAt = deadline.Add(30 * time.Second)
	return &marketFeedContext{Enabled: true, Requirements: feedReq, Snapshot: snap, Interval: cfg.IntervalSeconds}
}

func pairPyManagementInput(mark float64) pairManagementInput {
	var in pairManagementInput
	start := time.Now().UTC().Truncate(time.Hour).Add(-120 * time.Hour).UnixMilli()
	for i := 0; i < 120; i++ {
		px := mark * (1 + 0.003*math.Sin(float64(i)/4))
		in.Candles = append(in.Candles, [6]float64{float64(start + int64(i)*3_600_000), px, px * 1.004, px * 0.996, px * 1.001, 100 + float64(i)})
	}
	in.MarkPrice = mark
	return in
}

func pairPySC(e *pairTestEnv, id string) StrategyConfig {
	for _, sc := range e.cfg.Strategies {
		if sc.ID == id {
			return sc
		}
	}
	return StrategyConfig{}
}

func TestBasketPairMemberRealChecksAreManagementOnly(t *testing.T) {
	e, root := pairPyEnv(t, nil)
	t.Chdir(root)
	feed := pairPyFeed(t, e.cfg)
	hl := pairPySC(e, pairTestHL)
	okx := pairPySC(e, pairTestOKX)
	logger := &StrategyLogger{stratID: hl.ID, writer: os.Stdout}
	prices := map[string]float64{"BTC": 60000}
	wantHL, _ := pairMemberRequestOf(hl)
	wantOKX, _ := pairMemberRequestOf(okx)
	cases := []struct {
		name     string
		pos      PositionCtx
		signal   int
		fraction float64
	}{
		{"flat", PositionCtx{}, 0, 0},
		{"long in profit", PositionCtx{Side: "long", AvgCost: 30000, Quantity: 0.01, InitialQuantity: 0.01, EntryATR: 1000}, -1, 1},
		{"short in profit", PositionCtx{Side: "short", AvgCost: 150000, Quantity: 0.01, InitialQuantity: 0.01, EntryATR: 1000}, 1, 1},
	}
	for _, tc := range cases {
		t.Run("hyperliquid "+tc.name, func(t *testing.T) {
			res, _, _, ok := runHyperliquidCheck(&hl, prices, tc.pos, e.cfg.Regime, "simple", NewMultiNotifier(), logger, nil, feed)
			if !ok || res == nil || res.PairMember == nil || *res.PairMember != wantHL {
				t.Fatalf("the hyperliquid member check must echo the typed request: %+v", res)
			}
			if res.Signal != tc.signal || res.CloseFraction != tc.fraction || (tc.pos.Side == "" && res.OpenAction != "none") {
				t.Fatalf("the member must run management only with its native close: signal %d fraction %v action %q", res.Signal, res.CloseFraction, res.OpenAction)
			}
			if indicatorsATRValue(res.Indicators) <= 0 {
				t.Fatalf("management output must carry ATR: %v", res.Indicators)
			}
		})
	}
	okxCases := []struct {
		name   string
		pos    PositionCtx
		signal int
	}{
		{"flat", PositionCtx{}, 0},
		{"long below its stop", PositionCtx{Side: "long", AvgCost: 90000, Quantity: 0.02, InitialQuantity: 0.02, EntryATR: 1000}, -1},
		{"short above its stop", PositionCtx{Side: "short", AvgCost: 30000, Quantity: 0.02, InitialQuantity: 0.02, EntryATR: 1000}, 1},
	}
	for _, tc := range okxCases {
		t.Run("okx "+tc.name, func(t *testing.T) {
			res, stderr, err := runOKXPairManagementCheck(okx, tc.pos, pairPyManagementInput(60000), logger)
			if err != nil || res == nil || res.PairMember == nil || *res.PairMember != wantOKX {
				t.Fatalf("the okx member check must echo the typed request: %+v %v %s", res, err, stderr)
			}
			if res.Signal != tc.signal {
				t.Fatalf("the okx member must run management only with its native close: signal %d, want %d", res.Signal, tc.signal)
			}
		})
	}
}

func TestBasketPairMemberRealOKXFrozenInputNeedsNoExchangeClient(t *testing.T) {
	e, root := pairPyEnv(t, nil)
	okx := pairPySC(e, pairTestOKX)
	req, _ := pairMemberRequestOf(okx)
	blob, _ := json.Marshal(pairManagementInput{V: pairManagementInputVersion, Candles: pairPyManagementInput(60000).Candles, MarkPrice: 60000})
	prelude := `import builtins, runpy, sys
real = builtins.__import__
def guard(name, *a, **k):
    if name in ("adapter", "ccxt") or name.startswith("ccxt."):
        raise ImportError("frozen management input must not construct an exchange client: " + name)
    return real(name, *a, **k)
builtins.__import__ = guard
sys.argv = sys.argv[1:]
runpy.run_path(sys.argv[0], run_name="__main__")
`
	args := []string{"-c", prelude, filepath.Join(root, "shared_scripts", "check_okx.py"), "pair_member", "BTC", "1h", "--mode=paper", "--inst-type=swap",
		"--strategy-refs", `{"open":{"name":"pair_member","params":{}},"closes":[{"name":"atr_stop","params":{"atr_mult":3}}]}`,
		pairMemberFlag + "=" + req.JSON(), pairManagementStdinFlag}
	cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), args...)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(string(blob))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the frozen okx management check failed: %v %s", err, out)
	}
	var res OKXResult
	if err := json.Unmarshal(out, &res); err != nil || res.Error != "" || res.PairMember == nil || *res.PairMember != req {
		t.Fatalf("the frozen check must echo without an exchange client: %s (%v)", out, err)
	}
	cmd = exec.Command(filepath.Join(root, ".venv", "bin", "python3"), append([]string{"-c", prelude}, args[2:len(args)-1]...)...)
	cmd.Dir = root
	out, _ = cmd.Output()
	if err := json.Unmarshal(out, &res); err != nil || res.Error == "" {
		t.Fatalf("without frozen input the guard must prove the adapter would be constructed: %s", out)
	}
}

func pairPyStep(t *testing.T, e *pairTestEnv, sc StrategyConfig, feed *marketFeedContext, results *hlBatchCycleResults) {
	t.Helper()
	s := e.state.Strategies[sc.ID]
	stratDB, err := e.store.dbForStrategy(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	part := partitionFor(sc)
	sr := &scopeCycleRisk{Partition: part, Config: partitionRiskConfig(e.cfg, part)}
	prices := map[string]float64{"BTC": 60000}
	logger := &StrategyLogger{stratID: sc.ID, writer: os.Stdout}
	pos, hasPos := readHLPerpsPosition(sc, s, "BTC", e.cfg.Regime)
	x := &hlPerpsStep{sc: sc, mu: e.mu, logger: logger, stratState: s, notifier: NewMultiNotifier(), cfg: e.cfg, stratDB: stratDB, store: e.store, state: e.state,
		sr: sr, prices: prices, feedCtx: feed, basketCoord: e.bc, intervals: effectiveStrategyIntervals(e.cfg.Strategies, e.state.Strategies, e.cfg.IntervalSeconds, configuredDrawdownWarnThresholdPct(e.cfg)),
		stratScope: part.Scope, hlScaleInCash: s.Cash, hlBatchResults: results}
	if hasPos {
		x.hlPosCtx, x.hlPosSide, x.hlPosQty, x.hlAvgCost = pos.Ctx, pos.Ctx.Side, pos.Ctx.Quantity, pos.Ctx.AvgCost
		x.hlPosLeverage, x.hlEntryATR, x.hlStopLossOID, x.hlTPOIDs = pos.Leverage, pos.EntryATR, pos.StopLossOID, pos.TPOIDs
		x.hlStopLossTriggerPx, x.hlStopLossHighWaterPx, x.hlPosSnapshot = pos.StopLossTriggerPx, pos.StopLossHighWaterPx, pos.Snapshot
	}
	runHyperliquidPerpsStep(x)
}

func TestBasketPairMemberRealInjectedAndFallbackIncreasesBookNothing(t *testing.T) {
	e, root := pairPyEnv(t, nil)
	t.Chdir(root)
	feed := pairPyFeed(t, e.cfg)
	hl := pairPySC(e, pairTestHL)
	origStdin := runHyperliquidCheckWithStdinFn
	t.Cleanup(func() { runHyperliquidCheckWithStdinFn = origStdin })
	runHyperliquidCheckWithStdinFn = func(script string, args []string, stdin []byte) (*HyperliquidResult, string, error) {
		res, stderr, err := origStdin(script, args, stdin)
		if err == nil && res != nil && res.Error == "" && basketPositionSigned(e.state.Strategies[pairTestHL], "BTC") == 0 {
			res.Signal = 1
		}
		return res, stderr, err
	}
	pairPyStep(t, e, hl, feed, nil)
	if q := e.signed(pairTestHL); q != 0 || e.bookedTrades(pairTestHL) != 0 {
		t.Fatalf("an injected member increase booked %v (%d trades)", q, e.bookedTrades(pairTestHL))
	}
	if _, ok := e.rt().observations[pairTestHL]; !ok {
		t.Fatal("a valid management echo must still feed the coordinator observation")
	}
	results := &hlBatchCycleResults{}
	pairPyStep(t, e, hl, feed, results)
	if q := e.signed(pairTestHL); q != 0 {
		t.Fatalf("an increase on the batch fallback path booked %v", q)
	}
	e.externalBookChange(pairTestHL, 0.01)
	e.mu.Lock()
	pos := e.state.Strategies[pairTestHL].Positions["BTC"]
	pos.AvgCost, pos.InitialQuantity, pos.EntryATR = 30000, 0.01, 1000
	e.mu.Unlock()
	runHyperliquidCheckWithStdinFn = origStdin
	pairPyStep(t, e, hl, feed, nil)
	if q := e.signed(pairTestHL); q != 0 {
		t.Fatalf("the member's native close must stay active and close the position, it holds %v", q)
	}
}

func TestBasketPairMemberRealEchoFailuresHoldEntry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mangle func(args []string) []string
		strip  bool
	}{
		{"wrong echo", func(args []string) []string {
			out := append([]string(nil), args...)
			for i, a := range out {
				if strings.HasPrefix(a, pairMemberFlag+"=") {
					out[i] = strings.Replace(a, pairTestHL, "pair-hl-other", 1)
				}
			}
			return out
		}, false},
		{"missing echo", func(args []string) []string { return args }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, root := pairPyEnv(t, nil)
			t.Chdir(root)
			feed := pairPyFeed(t, e.cfg)
			hl := pairPySC(e, pairTestHL)
			origStdin := runHyperliquidCheckWithStdinFn
			t.Cleanup(func() { runHyperliquidCheckWithStdinFn = origStdin })
			runHyperliquidCheckWithStdinFn = func(script string, args []string, stdin []byte) (*HyperliquidResult, string, error) {
				res, stderr, err := origStdin(script, tc.mangle(args), stdin)
				if tc.strip && res != nil {
					res.PairMember = nil
				}
				return res, stderr, err
			}
			pairPyStep(t, e, hl, feed, nil)
			if _, ok := e.rt().observations[pairTestHL]; ok {
				t.Fatal("a failed management echo must not feed the coordinator")
			}
			if why := e.rt().pairEchoFailures[pairTestHL]; why == "" {
				t.Fatal("a failed management echo must be recorded")
			}
			e.mu.Lock()
			hold := e.bc.pairIncreaseHold(e.rt(), basketCycleInputs{Now: time.Now()})
			e.mu.Unlock()
			if hold == "" {
				t.Fatal("pair entry must hold after a failed management echo")
			}
			if q := e.signed(pairTestHL); q != 0 {
				t.Fatalf("a failed echo booked %v", q)
			}
		})
	}
}

func TestBasketPairMemberRealBatchedSlotStaysManagementOnly(t *testing.T) {
	e, root := pairPyEnv(t, func(r map[string]interface{}) {
		dir := filepath.Dir(r["db_file"].(string))
		r["paper_sources"] = []interface{}{map[string]interface{}{"id": "alt", "db_file": filepath.Join(dir, "alt.db")}}
		r["strategies"] = append(r["strategies"].([]interface{}), map[string]interface{}{
			"id": "hl-alt-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []interface{}{"rsi", "BTC", "1h", "--mode=paper"}, "capital": 1000, "paper_source": "alt", "leverage": 2, "margin_mode": "cross",
		})
	})
	t.Chdir(root)
	t.Setenv("GO_TRADER_HL_BATCH", "1")
	feed := pairPyFeed(t, e.cfg)
	hl := pairPySC(e, pairTestHL)
	peer := pairPySC(e, "hl-alt-btc")
	mk, ok1 := hlBatchKeyForStrategy(hl, e.cfg)
	pk, ok2 := hlBatchKeyForStrategy(peer, e.cfg)
	if !ok1 || !ok2 || mk != pk {
		t.Fatalf("the member and its peer must share one batch key: %v %v", mk, pk)
	}
	origFallback := hlBatchFallback
	hlBatchFallback = &hlBatchFallbackTracker{}
	t.Cleanup(func() { hlBatchFallback = origFallback })
	results := runHyperliquidBatchPrePass([]StrategyConfig{hl, peer}, e.state, &sync.RWMutex{}, e.cfg, map[string]float64{"BTC": 60000}, NewMultiNotifier(), t.Logf, feed)
	out, ok := results.lookup(hl.ID)
	if !ok || out.Err != "" || out.Result == nil {
		t.Fatalf("the batched pair member slot failed: %+v", out)
	}
	want, _ := pairMemberRequestOf(hl)
	if out.Result.PairMember == nil || *out.Result.PairMember != want || out.Result.Signal != 0 || out.Result.OpenAction != "none" {
		t.Fatalf("the batched member must run management only and echo its request: %+v", out.Result)
	}
	peerOut, _ := results.lookup(peer.ID)
	if peerOut.Result == nil || peerOut.Result.PairMember != nil {
		t.Fatalf("the batched peer must keep its own open strategy without a pair echo: %+v", peerOut.Result)
	}
	origSingle, origStdin := runHyperliquidCheckFn, runHyperliquidCheckWithStdinFn
	t.Cleanup(func() { runHyperliquidCheckFn, runHyperliquidCheckWithStdinFn = origSingle, origStdin })
	runHyperliquidCheckWithStdinFn = func(string, []string, []byte) (*HyperliquidResult, string, error) {
		t.Fatal("the single-check path ran; the batched result must be used")
		return nil, "", nil
	}
	out.Result.Signal = 1
	pairPyStep(t, e, hl, feed, results)
	if q := e.signed(pairTestHL); q != 0 {
		t.Fatalf("a batched increase booked %v", q)
	}
}

func TestBasketPairProbeArgvsParseInEveryStrictCheck(t *testing.T) {
	root := basketPyRoot(t)
	t.Chdir(root)
	for _, script := range []string{"shared_scripts/check_hyperliquid.py", "shared_scripts/check_okx.py", "shared_scripts/check_robinhood.py", "shared_scripts/check_topstep.py"} {
		for name, argv := range map[string][]string{"probeArgv": probeArgv, "probeCompositeArgv": probeCompositeArgv} {
			if err := probeOneCheckScript(script, argv); err != nil {
				t.Errorf("%s %s: %v", script, name, err)
			}
		}
	}
	for _, script := range []string{"shared_scripts/check_robinhood.py", "shared_scripts/check_topstep.py"} {
		cmd := exec.Command(filepath.Join(root, ".venv", "bin", "python3"), script, "pair_member", "BTC", "1h", "--mode=paper", pairMemberFlag+"="+pairMemberProbeRequestJSON)
		cmd.Dir = root
		out, _ := cmd.Output()
		var res map[string]interface{}
		if err := json.Unmarshal(out, &res); err != nil || res["error"] == nil || res["error"] == "" {
			t.Fatalf("%s must refuse a pair member at run time: %s", script, out)
		}
	}
}
