//go:build pyintegration

package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestBasketRealRankerSubprocessDrivesAPaperRebalance(t *testing.T) {
	root := basketPyRoot(t)
	cfg := basketPyConfig(t, nil, 60)
	t.Chdir(root)
	env := newBasketTestEnv(t, cfg, nil, nil)
	deadline := time.Now().UTC().Truncate(2 * time.Hour)
	if time.Until(deadline.Add(7140*time.Second)) < 30*time.Second {
		t.Skip("too close to the end of the evaluation window for a stable real-subprocess run")
	}
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
			deadline := time.Now().UTC().Truncate(2 * time.Hour)
			if time.Until(deadline.Add(7140*time.Second)) < 30*time.Second {
				t.Skip("too close to the end of the evaluation window")
			}
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
