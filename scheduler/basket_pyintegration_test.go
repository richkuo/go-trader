//go:build pyintegration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func basketPyRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".venv", "bin", "python3")); err != nil {
		t.Skipf("repository virtualenv missing (%v); run uv sync", err)
	}
	return root
}

func basketPyConfig(t *testing.T, params map[string]interface{}, timeout int) *Config {
	t.Helper()
	return loadBasketTestConfig(t, func(root map[string]interface{}) {
		b := basketTestBasket(root)
		b["rebalance_interval_seconds"] = 7200
		b["evaluation_window_seconds"] = 7140
		b["ranker_timeout_seconds"] = timeout
		b["max_transition_net_usd"] = 1100
		if params != nil {
			b["params"] = params
		}
	})
}

func basketPyDeadline() time.Time {
	return time.Now().UTC().Truncate(2 * time.Hour).Add(2 * time.Hour)
}

func TestBasketRealRankerSubprocessDrivesAPaperRebalance(t *testing.T) {
	root := basketPyRoot(t)
	cfg := basketPyConfig(t, nil, 60)
	t.Chdir(root)
	env := newBasketTestEnv(t, cfg, nil, nil)
	deadline := basketPyDeadline()
	snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 11)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	rb := env.rt().Rebalances[deadline.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("the real ranker subprocess did not drive a completed rebalance: %+v", rb)
	}
	if len(rb.InputFP) != 64 || !strings.Contains(rb.EvidenceJSON, `"status":"eligible"`) {
		t.Fatalf("the frozen evidence is missing: fp=%q evidence=%s", rb.InputFP, rb.EvidenceJSON)
	}
	targets, _ := rb.Targets()
	for _, m := range basketTestMembers {
		if got := env.signedQty(m.ID, m.Coin); !basketQtyEqual(got, targets.Members[m.ID].TargetQty) || got == 0 {
			t.Fatalf("%s holds %.10g, target %.10g", m.ID, got, targets.Members[m.ID].TargetQty)
		}
	}
}

func TestBasketRealRankerFailuresHoldTheDeadline(t *testing.T) {
	root := basketPyRoot(t)
	cases := []struct {
		name    string
		params  map[string]interface{}
		timeout int
		want    string
	}{
		{"invalid output", map[string]interface{}{"raw": map[string]interface{}{"weights": map[string]interface{}{"BTC": 1}}}, 60, "the ranker refused its output"},
		{"scripted hold", map[string]interface{}{"hold": "no clear dispersion"}, 60, "no clear dispersion"},
		{"timeout", map[string]interface{}{"sleep_seconds": 5, "weights": map[string]interface{}{"BTC": 0.25, "ETH": 0.25, "SOL": -0.25, "AVAX": -0.25}}, 1, "timed out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := basketPyConfig(t, tc.params, tc.timeout)
			t.Chdir(root)
			env := newBasketTestEnv(t, cfg, nil, nil)
			deadline := basketPyDeadline()
			snap := basketTestSnapshot(deadline, 60, basketTestMarks(), 3)
			env.cycle(deadline.Add(time.Minute), deadline, snap, true)
			rb := env.rt().Rebalances[deadline.UnixMilli()]
			if rb == nil || rb.Status != basketRebalanceHeld || !strings.Contains(rb.HoldReason, tc.want) {
				t.Fatalf("want a held deadline naming %q, got %+v", tc.want, rb)
			}
			for _, m := range basketTestMembers {
				if q := env.signedQty(m.ID, m.Coin); q != 0 {
					t.Fatalf("%s traded %.10g after a ranker failure", m.ID, q)
				}
			}
		})
	}
}

func TestBasketMemberRealCheckIsManagementOnly(t *testing.T) {
	root := basketPyRoot(t)
	cfg := loadBasketTestConfig(t, nil)
	t.Chdir(root)
	feedReq, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Truncate(time.Hour)
	snap := basketTestSnapshot(deadline, 260, basketTestMarks(), 1)
	snap.Deadline = time.Time{}
	snap.SealedAt = deadline.Add(30 * time.Second)
	feed := &marketFeedContext{Enabled: true, Requirements: feedReq, Snapshot: snap, Interval: cfg.IntervalSeconds}
	var sc StrategyConfig
	for _, s := range cfg.Strategies {
		if s.ID == "hl-bsk-btc" {
			sc = s
		}
	}
	logger := &StrategyLogger{stratID: sc.ID, writer: os.Stdout}
	prices := map[string]float64{"BTC": 60000}
	res, _, _, ok := runHyperliquidCheck(&sc, prices, PositionCtx{}, cfg.Regime, "simple", NewMultiNotifier(), logger, nil, feed)
	if !ok || res == nil {
		t.Fatal("the member check failed")
	}
	if !res.BasketMember || res.Signal != 0 || res.OpenAction != "none" {
		t.Fatalf("a flat member check must be management-only with no entry: %+v", res)
	}
	if res.ClosedBar == nil || res.ClosedBar.Held || res.ClosedBar.DecisionBoundaryMs != deadline.UnixMilli() {
		t.Fatalf("the member check must carry the closed-bar decision for the deadline candle: %+v", res.ClosedBar)
	}
	if indicatorsATRValue(res.Indicators) <= 0 {
		t.Fatalf("the member check must supply ATR for the entry stop: %v", res.Indicators)
	}
	pos := PositionCtx{Side: "long", AvgCost: 30000, Quantity: 0.01, InitialQuantity: 0.01}
	res, _, _, ok = runHyperliquidCheck(&sc, prices, pos, cfg.Regime, "simple", NewMultiNotifier(), logger, nil, feed)
	if !ok || res == nil || !res.BasketMember || res.Signal != -1 || res.CloseFraction != 1 {
		t.Fatalf("the member's native close strategy must still close: %+v", res)
	}
}

func TestBasketRealRankerExcludesADelistedMemberWithNoFrameOrMark(t *testing.T) {
	root := basketPyRoot(t)
	deadline := basketPyDeadline()
	delisted := deadline.Add(-time.Hour).UTC().Format(time.RFC3339)
	cfg := loadBasketTestConfig(t, func(r map[string]interface{}) {
		b := basketTestBasket(r)
		b["rebalance_interval_seconds"] = 7200
		b["evaluation_window_seconds"] = 7140
		b["max_transition_net_usd"] = 1100
		b["params"] = map[string]interface{}{"weights": map[string]interface{}{"BTC": 0.25, "ETH": 0.25, "AVAX": -0.25}}
		b["eligibility"].(map[string]interface{})["listings"].(map[string]interface{})["SOL"] = map[string]interface{}{
			"listed_at": "2020-01-01T00:00:00Z", "known_at": delisted, "delisted_at": delisted,
		}
	})
	t.Chdir(root)
	env := newBasketTestEnv(t, cfg, nil, nil)
	snap := basketTestSnapshot(deadline, 60, basketMarksWithout("SOL"), 11)
	env.cycle(deadline.Add(time.Minute), deadline, snap, true)
	rb := env.rt().Rebalances[deadline.UnixMilli()]
	if rb == nil || rb.Status != basketRebalanceCompleted {
		t.Fatalf("the real ranker must rank BTC, ETH and AVAX with SOL delisted and absent from the seal: %+v", rb)
	}
	if !strings.Contains(rb.EvidenceJSON, `"reason":"delisted at or before the deadline"`) {
		t.Fatalf("the frozen evidence must record the SOL exclusion reason: %s", rb.EvidenceJSON)
	}
	targets, _ := rb.Targets()
	if got := targets.Members["hl-bsk-sol"].Excluded; got != "delisted at or before the deadline" {
		t.Fatalf("SOL exclusion %q", got)
	}
	for _, m := range basketTestMembers {
		got := env.signedQty(m.ID, m.Coin)
		if m.Coin == "SOL" {
			if got != 0 {
				t.Fatalf("the delisted SOL member traded %.10g", got)
			}
			continue
		}
		if got == 0 || !basketQtyEqual(got, targets.Members[m.ID].TargetQty) {
			t.Fatalf("%s holds %.10g, target %.10g", m.ID, got, targets.Members[m.ID].TargetQty)
		}
	}
}

func basketBatchPeerConfig(t *testing.T) *Config {
	t.Helper()
	return loadBasketTestConfig(t, func(r map[string]interface{}) {
		r["paper_sources"] = []interface{}{map[string]interface{}{"id": "alt", "db_file": filepath.Join(t.TempDir(), "alt.db")}}
		r["strategies"] = append(r["strategies"].([]interface{}), map[string]interface{}{
			"id": "hl-alt-btc", "type": "perps", "platform": "hyperliquid", "script": "shared_scripts/check_hyperliquid.py",
			"args": []interface{}{"rsi", "BTC", "1h", "--mode=paper"}, "capital": 1000, "paper_source": "alt",
		})
	})
}

func basketBatchRun(t *testing.T, cfg *Config, memberPos *Position) (member, peer *HyperliquidResult, memberSC StrategyConfig, state *AppState) {
	t.Helper()
	t.Setenv("GO_TRADER_HL_BATCH", "1")
	feedReq, err := deriveFeedRequirements(cfg)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().UTC().Truncate(time.Hour)
	snap := basketTestSnapshot(deadline, 260, basketTestMarks(), 1)
	snap.Deadline = time.Time{}
	snap.SealedAt = deadline.Add(30 * time.Second)
	feed := &marketFeedContext{Enabled: true, Requirements: feedReq, Snapshot: snap, Interval: cfg.IntervalSeconds}
	var peerSC StrategyConfig
	for _, s := range cfg.Strategies {
		switch s.ID {
		case "hl-bsk-btc":
			memberSC = s
		case "hl-alt-btc":
			peerSC = s
		}
	}
	if !isBasketMember(memberSC) || peerSC.ID == "" || isBasketMember(peerSC) || partitionFor(memberSC) == partitionFor(peerSC) {
		t.Fatalf("setup: member %+v peer %+v", memberSC.ID, peerSC.ID)
	}
	mk, ok1 := hlBatchKeyForStrategy(memberSC, cfg)
	pk, ok2 := hlBatchKeyForStrategy(peerSC, cfg)
	if !ok1 || !ok2 || mk != pk {
		t.Fatalf("the member and its peer must share one batch key: %v %v", mk, pk)
	}
	state = NewAppState()
	for _, s := range cfg.Strategies {
		state.Strategies[s.ID] = NewStrategyState(s)
	}
	if memberPos != nil {
		state.Strategies[memberSC.ID].Positions["BTC"] = memberPos
	}
	mu := &sync.RWMutex{}
	prices := map[string]float64{"BTC": 60000}
	origFallback := hlBatchFallback
	hlBatchFallback = &hlBatchFallbackTracker{}
	t.Cleanup(func() { hlBatchFallback = origFallback })
	results := runHyperliquidBatchPrePass([]StrategyConfig{memberSC, peerSC}, state, mu, cfg, prices, NewMultiNotifier(), t.Logf, feed)
	for _, id := range []string{memberSC.ID, peerSC.ID} {
		out, ok := results.lookup(id)
		if !ok || out.Err != "" || out.Result == nil {
			t.Fatalf("the batched check for %s failed: ok=%v err=%q", id, ok, out.Err)
		}
	}
	origSingle, origStdin := runHyperliquidCheckFn, runHyperliquidCheckWithStdinFn
	t.Cleanup(func() { runHyperliquidCheckFn, runHyperliquidCheckWithStdinFn = origSingle, origStdin })
	runHyperliquidCheckFn = func(string, []string) (*HyperliquidResult, string, error) {
		t.Fatal("the single-check path ran; the batched result must be used")
		return nil, "", nil
	}
	runHyperliquidCheckWithStdinFn = func(string, []string, []byte) (*HyperliquidResult, string, error) {
		t.Fatal("the single-check path ran; the batched result must be used")
		return nil, "", nil
	}
	run := func(sc StrategyConfig) *HyperliquidResult {
		var pos PositionCtx
		if p := state.Strategies[sc.ID].Positions["BTC"]; p != nil {
			pos = positionCtxForCheck(sc, p, cfg.Regime)
		}
		logger := &StrategyLogger{stratID: sc.ID, writer: os.Stdout}
		res, _, _, ok := runHyperliquidCheck(&sc, prices, pos, cfg.Regime, resolveATRMethod(sc, cfg), NewMultiNotifier(), logger, results, feed)
		if !ok || res == nil {
			t.Fatalf("%s: the batched check result was refused", sc.ID)
		}
		return res
	}
	return run(memberSC), run(peerSC), memberSC, state
}

func TestBasketMemberBatchedWithANonMemberStaysManagementOnly(t *testing.T) {
	root := basketPyRoot(t)
	cfg := basketBatchPeerConfig(t)
	t.Chdir(root)
	member, peer, _, _ := basketBatchRun(t, cfg, nil)
	if !member.BasketMember || member.Signal != 0 || member.OpenAction != "none" {
		t.Fatalf("a batched member must run management-only and echo the contract: %+v", member)
	}
	if peer.BasketMember || peer.Strategy != "rsi" || peer.Indicators == nil {
		t.Fatalf("the batched non-member must still evaluate its own open strategy: %+v", peer)
	}
	if _, ok := peer.Indicators["rsi"]; !ok {
		t.Fatalf("the non-member open strategy did not run: %v", peer.Indicators)
	}
}

func TestBasketMemberBatchedNativeCloseBooks(t *testing.T) {
	root := basketPyRoot(t)
	cfg := basketBatchPeerConfig(t)
	t.Chdir(root)
	pos := &Position{Symbol: "BTC", Quantity: 0.01, InitialQuantity: 0.01, AvgCost: 30000, Side: "long", Multiplier: 1}
	member, _, sc, state := basketBatchRun(t, cfg, pos)
	if !member.BasketMember || member.Signal != -1 || member.CloseFraction != 1 {
		t.Fatalf("the member's native close must reach Go through the batch: %+v", member)
	}
	s := state.Strategies[sc.ID]
	before := len(s.TradeHistory)
	logger := &StrategyLogger{stratID: sc.ID, writer: os.Stdout}
	n, detail := executeHyperliquidResult(sc, s, member, nil, "SELL", 60000, cfg.Regime, cfg, HurstGateDecision{}, logger)
	if n == 0 {
		t.Fatalf("the batched member close did not book: %s", detail)
	}
	if p := s.Positions["BTC"]; p != nil && p.Quantity > 0 {
		t.Fatalf("the member position is still open after the close: %+v", p)
	}
	if len(s.TradeHistory) <= before || !s.TradeHistory[len(s.TradeHistory)-1].IsClose {
		t.Fatalf("no close trade was booked: %+v", s.TradeHistory)
	}
}
