package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func buildSharedWalletTestState() (*AppState, []StrategyConfig) {
	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 600},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 400},
		{ID: "paper-sol", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "SOL", "1h", "--mode=paper"}, Capital: 1000},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc": {ID: "hl-btc", Cash: 300, Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.1, AvgCost: 60000},
		}},
		"hl-eth": {ID: "hl-eth", Cash: 420, Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "long", Quantity: 2, AvgCost: 3000},
		}},
		"paper-sol": {ID: "paper-sol", Cash: 1000, Positions: map[string]*Position{}},
	}}
	return state, strategies
}

func TestReconcileSharedWalletDisplayValues_SetsGatesAndSums(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	state, strategies := buildSharedWalletTestState()
	sharedWallets := detectSharedWallets(strategies)
	if len(sharedWallets) != 1 {
		t.Fatalf("expected 1 shared wallet, got %d", len(sharedWallets))
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	walletBalances := map[SharedWalletKey]float64{key: 1030.0}
	hlPositions := []HLPosition{
		{Coin: "BTC", Size: 0.1, UnrealizedPnL: 50},
		{Coin: "ETH", Size: 2, UnrealizedPnL: -20},
	}

	results := reconcileSharedWalletDisplayValues(strategies, state, nil, sharedWallets, walletBalances, hlPositions, nil, false)

	if len(results) != 1 || math.Abs(results[0].Drift) > 0.01 {
		t.Fatalf("expected 1 result with ~0 drift, got %+v", results)
	}
	btc := state.Strategies["hl-btc"]
	eth := state.Strategies["hl-eth"]
	sol := state.Strategies["paper-sol"]
	if !btc.SharedWalletValueSet || !eth.SharedWalletValueSet {
		t.Fatal("expected both HL members to have SharedWalletValueSet=true")
	}
	if sol.SharedWalletValueSet {
		t.Error("non-member paper strategy must NOT be gated on")
	}
	if math.Abs(btc.SharedWalletValue-650) > 0.01 {
		t.Errorf("btc value = %v, want 650", btc.SharedWalletValue)
	}
	if math.Abs(eth.SharedWalletValue-380) > 0.01 {
		t.Errorf("eth value = %v, want 380", eth.SharedWalletValue)
	}
	if sum := btc.SharedWalletValue + eth.SharedWalletValue; math.Abs(sum-1030.0) > 0.01 {
		t.Errorf("member sum %v != balance 1030", sum)
	}
	if got := displayStrategyValue(btc, nil); math.Abs(got-650) > 0.01 {
		t.Errorf("displayStrategyValue(btc) = %v, want 650", got)
	}
}

func TestReconcileSharedWalletDisplayValues_ManualMemberAttributed(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	state, strategies := buildSharedWalletTestState()
	strategies = append(strategies, StrategyConfig{
		ID: "hl-manual-sol", Platform: "hyperliquid", Type: "manual",
		Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200,
	})
	state.Strategies["hl-manual-sol"] = &StrategyState{
		ID: "hl-manual-sol", Cash: 100,
		Positions: map[string]*Position{"SOL": {Symbol: "SOL", Side: "long", Quantity: 5, AvgCost: 150}},
	}
	sharedWallets := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	walletBalances := map[SharedWalletKey]float64{key: 1045.0}
	hlPositions := []HLPosition{
		{Coin: "BTC", Size: 0.1, UnrealizedPnL: 50},
		{Coin: "ETH", Size: 2, UnrealizedPnL: -20},
		{Coin: "SOL", Size: 5, UnrealizedPnL: 15},
	}

	results := reconcileSharedWalletDisplayValues(strategies, state, nil, sharedWallets, walletBalances, hlPositions, nil, false)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if math.Abs(results[0].Drift) > 0.01 {
		t.Fatalf("SOL manual position must be attributed (no orphan drift), got drift %v", results[0].Drift)
	}
	msol := state.Strategies["hl-manual-sol"]
	if !msol.SharedWalletValueSet {
		t.Fatal("manual member must be gated on")
	}
	sum := state.Strategies["hl-btc"].SharedWalletValue +
		state.Strategies["hl-eth"].SharedWalletValue + msol.SharedWalletValue
	if math.Abs(sum-1045.0) > 0.01 {
		t.Errorf("member sum %v != balance 1045", sum)
	}
	if math.Abs(msol.SharedWalletValue-(200.0/1200.0*1000.0+15)) > 0.01 {
		t.Errorf("manual value = %v, want %v", msol.SharedWalletValue, 200.0/1200.0*1000.0+15)
	}
}

func TestReconcileSharedWalletDisplayValues_OKXPositionsNotFetchedSkips(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okxkey")
	strategies := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "okx-b", Platform: "okx", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"okx-a": {ID: "okx-a", Cash: 500, Positions: map[string]*Position{}},
		"okx-b": {ID: "okx-b", Cash: 500, Positions: map[string]*Position{}},
	}}
	sharedWallets := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "okx", Account: "okxkey"}
	walletBalances := map[SharedWalletKey]float64{key: 1000.0}

	results := reconcileSharedWalletDisplayValues(strategies, state, nil, sharedWallets, walletBalances, nil, nil, false)
	if len(results) != 0 {
		t.Fatalf("expected OKX wallet skipped when positions not fetched, got %d results", len(results))
	}
	if state.Strategies["okx-a"].SharedWalletValueSet || state.Strategies["okx-b"].SharedWalletValueSet {
		t.Error("OKX members must fall back (Set=false) when positions fetch failed")
	}

	results = reconcileSharedWalletDisplayValues(strategies, state, nil, sharedWallets, walletBalances, nil, nil, true)
	if len(results) != 1 || !state.Strategies["okx-a"].SharedWalletValueSet {
		t.Fatalf("expected OKX reconcile when positions fetched, got %+v", results)
	}
}

func TestReconcileSharedWalletDisplayValues_OKXPoolShowsAttributedPerformance(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-pool")
	marginCap := 100.0
	strategies := []StrategyConfig{
		{ID: "okx-a", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
		{ID: "okx-b", Platform: "okx", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"okx-a": {ID: "okx-a", Platform: "okx", Type: "perps", Positions: map[string]*Position{}},
		"okx-b": {ID: "okx-b", Platform: "okx", Type: "perps", Positions: map[string]*Position{}},
	}}
	db := newLedgerTestDB(t)
	if err := db.InsertTrade("okx-a", Trade{
		Timestamp: time.Now().UTC(), StrategyID: "okx-a", Symbol: "BTC",
		Side: "buy", Quantity: 1, Price: 100, Value: 100,
		TradeType: "perps", ExchangeFee: 2,
	}); err != nil {
		t.Fatalf("insert open fee: %v", err)
	}

	sharedWallets := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "okx", Account: "okx-pool"}
	results := reconcileSharedWalletDisplayValues(
		strategies, state, db, sharedWallets,
		map[SharedWalletKey]float64{key: 1000},
		nil, nil, true,
	)
	if len(results) != 1 {
		t.Fatalf("expected one pooled reconcile result, got %+v", results)
	}
	if got := state.Strategies["okx-a"].SharedWalletValue; got != -2 {
		t.Fatalf("okx-a performance = %v, want -2 open fee (not a wallet allocation)", got)
	}
	if got := state.Strategies["okx-b"].SharedWalletValue; got != 0 {
		t.Fatalf("idle okx-b performance = %v, want 0", got)
	}
	if got := latestDisplayTotal(state, nil); got != 1000 {
		t.Fatalf("operator total=%v, want real wallet equity 1000 counted once", got)
	}
}

func TestReconcileSharedWalletDisplayValues_FetchFailedFallsBack(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	state, strategies := buildSharedWalletTestState()
	state.Strategies["hl-btc"].SharedWalletValue = 999
	state.Strategies["hl-btc"].SharedWalletValueSet = true
	sharedWallets := detectSharedWallets(strategies)

	results := reconcileSharedWalletDisplayValues(strategies, state, nil, sharedWallets, map[SharedWalletKey]float64{}, nil, nil, false)

	if len(results) != 0 {
		t.Fatalf("expected no drift results when balance missing, got %d", len(results))
	}
	if state.Strategies["hl-btc"].SharedWalletValueSet {
		t.Error("stale SharedWalletValueSet must be cleared when fetch fails")
	}
	want := PortfolioValue(state.Strategies["hl-btc"], nil)
	if got := displayStrategyValue(state.Strategies["hl-btc"], nil); got != want {
		t.Errorf("display fallback = %v, want PortfolioValue %v", got, want)
	}
}

func TestPooledReconcileFailureStillCountsFreshWalletBalanceInTotal(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xpool")
	marginCap := 100.0
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
		{ID: "spot", Platform: "binanceus", Type: "spot", Capital: 200},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {Cash: -10, Positions: map[string]*Position{}},
		"hl-b": {Cash: 5, Positions: map[string]*Position{}},
		"spot": {Cash: 200, Positions: map[string]*Position{}},
	}}
	shared := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xpool"}

	results := reconcileSharedWalletDisplayValues(
		strategies, state, nil, shared,
		map[SharedWalletKey]float64{key: 1000}, nil, nil, false,
	)
	if len(results) != 0 {
		t.Fatalf("failed attribution must not emit a drift result: %+v", results)
	}
	if state.Strategies["hl-a"].SharedWalletValueSet || state.Strategies["hl-b"].SharedWalletValueSet {
		t.Fatal("failed attribution must leave per-member rows on modeled fallback")
	}
	if got := latestDisplayTotal(state, nil); got != 1200 {
		t.Fatalf("operator total=%v, want fresh wallet 1000 + allocated spot 200", got)
	}

	reconcileSharedWalletDisplayValues(strategies, state, nil, shared, nil, nil, nil, false)
	if got := latestDisplayTotal(state, nil); got != 195 {
		t.Fatalf("missing balance must retain modeled fallback without double count: got %v, want 195", got)
	}
}

func TestDisplayStrategyValue_PrefersSetValue(t *testing.T) {
	s := &StrategyState{ID: "x", Cash: 100, Positions: map[string]*Position{}}
	if got := displayStrategyValue(s, nil); got != 100 {
		t.Errorf("unset → PortfolioValue, got %v want 100", got)
	}
	s.SharedWalletValue = 777
	s.SharedWalletValueSet = true
	if got := displayStrategyValue(s, nil); got != 777 {
		t.Errorf("set → SharedWalletValue, got %v want 777", got)
	}
}

func TestParseOKXPositionsOutput_CarriesUnrealizedPnL(t *testing.T) {
	stdout := []byte(`{"positions":[{"coin":"BTC","size":0.3,"entry_price":60000,"side":"long","unrealized_pnl":123.45}],"platform":"okx"}`)
	res, _, err := parseOKXPositionsOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(res.Positions) != 1 || math.Abs(res.Positions[0].UnrealizedPnL-123.45) > 1e-9 {
		t.Fatalf("expected unrealized_pnl 123.45, got %+v", res.Positions)
	}
}

func TestFetchHyperliquidState_ParsesUnrealizedPnL(t *testing.T) {
	resp := map[string]interface{}{
		"marginSummary": map[string]string{"accountValue": "1000.00"},
		"assetPositions": []map[string]interface{}{
			{"position": map[string]string{
				"coin": "BTC", "szi": "0.1", "entryPx": "60000", "unrealizedPnl": "42.50",
			}},
		},
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()
	origURL := hlMainnetURL
	hlMainnetURL = ts.URL
	defer func() { hlMainnetURL = origURL }()

	_, positions, err := fetchHyperliquidState("0xabc")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(positions) != 1 || math.Abs(positions[0].UnrealizedPnL-42.50) > 1e-9 {
		t.Fatalf("expected UnrealizedPnL 42.50, got %+v", positions)
	}
}

func useFreshDriftTracker(t *testing.T) {
	t.Helper()
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	t.Cleanup(func() { sharedWalletDriftTracker = prev })
}

type driftTrackerStep struct {
	at     time.Duration
	drift  float64
	coins  []string
	notify *bool
	log    *bool
	count  int
}

func TestSharedWalletDriftTracker(t *testing.T) {
	yes, no := new(bool), new(bool)
	*yes = true
	btc := []string{"BTC"}
	stableHour := func() []driftTrackerStep {
		steps := []driftTrackerStep{
			{at: 0, drift: 5.00, coins: btc},
			{at: time.Second, drift: 5.00, coins: btc, notify: yes},
		}
		base := time.Second
		for i := 1; i <= 40; i++ {
			steps = append(steps, driftTrackerStep{at: base + time.Duration(i)*3*time.Second, drift: 5.00, coins: btc, notify: no})
		}
		return append(steps, driftTrackerStep{at: base + time.Hour + time.Second, drift: 5.00, coins: btc, notify: yes})
	}
	cases := []struct {
		name             string
		throttle         time.Duration
		key              string
		steps            []driftTrackerStep
		clear            bool
		wantRecovered    bool
		wantPriorNonZero bool
		wantPrior        int
	}{
		{
			name: "confirm then throttle then recover",
			steps: []driftTrackerStep{
				{at: 0, drift: 5.00, coins: btc, notify: no},
				{at: time.Minute, drift: 5.00, coins: btc, notify: yes},
				{at: 2 * time.Minute, drift: 5.00, coins: btc, notify: no},
				{at: 3 * time.Minute, drift: 9.00, coins: btc, notify: yes},
			},
			clear: true, wantRecovered: true, wantPriorNonZero: true,
		},
		{
			name: "clearing unknown wallet reports no recovery",
			key:  "okx/none", clear: true, wantRecovered: false,
		},
		{
			name:  "one-cycle transient stays silent and never recovers",
			steps: []driftTrackerStep{{at: 0, drift: 25.00, coins: btc, notify: no}},
			clear: true, wantRecovered: false,
		},
		{
			name: "distinct consecutive transients do not alert",
			steps: []driftTrackerStep{
				{at: 0, drift: 25.00, coins: btc, notify: no, count: 1},
				{at: time.Minute, drift: 12.00, coins: []string{"ETH"}, notify: no, count: 2},
			},
			clear: true, wantRecovered: false,
		},
		{
			name: "same orphan with changing magnitude still alerts",
			steps: []driftTrackerStep{
				{at: 0, drift: 25.00, coins: []string{"SOL"}, notify: no},
				{at: time.Minute, drift: 31.40, coins: []string{"SOL"}, notify: yes, count: 2},
			},
		},
		{
			name: "persistent orphan survives churn",
			steps: []driftTrackerStep{
				{at: 0, drift: 25.00, coins: btc, notify: no},
				{at: time.Minute, drift: 30.00, coins: []string{"BTC", "DOGE"}, notify: yes, count: 2},
				{at: 2 * time.Minute, drift: 30.00, coins: []string{"BTC", "SHIB"}, notify: no, count: 3},
			},
		},
		{
			name: "new orphan after alert re-confirms",
			steps: []driftTrackerStep{
				{at: 0, drift: 25.00, coins: btc},
				{at: time.Minute, drift: 25.00, coins: btc, notify: yes},
				{at: 2 * time.Minute, drift: 25.00, coins: []string{"ETH"}, notify: no, count: 3},
				{at: 3 * time.Minute, drift: 25.00, coins: []string{"ETH"}, notify: yes, count: 4},
			},
		},
		{
			name: "no orphan coins still confirms",
			key:  "okx/acct",
			steps: []driftTrackerStep{
				{at: 0, drift: 5.00, notify: no},
				{at: time.Minute, drift: 5.00, notify: yes, count: 2},
			},
		},
		{
			name: "mark wiggle stays throttled until cumulative move exceeds ratio",
			steps: []driftTrackerStep{
				{at: 0, drift: 5.00, coins: btc},
				{at: time.Minute, drift: 5.00, coins: btc, notify: yes},
				{at: 2 * time.Minute, drift: 5.20, coins: btc, notify: no},
				{at: 3 * time.Minute, drift: 5.40, coins: btc, notify: no},
				{at: 4 * time.Minute, drift: 5.60, coins: btc, notify: yes},
				{at: 5 * time.Minute, drift: 5.65, coins: btc, notify: no},
			},
		},
		{
			name: "sign flip re-alerts, small move against new anchor stays throttled",
			steps: []driftTrackerStep{
				{at: 0, drift: 5.00, coins: btc, notify: no},
				{at: time.Second, drift: 5.00, coins: btc, notify: yes},
				{at: 2 * time.Second, drift: -5.00, coins: btc, notify: yes},
				{at: 3 * time.Second, drift: -5.05, coins: btc, notify: no},
			},
		},
		{
			name: "recovery count survives churn",
			steps: []driftTrackerStep{
				{at: 0, drift: 25.00, coins: btc},
				{at: time.Minute, drift: 25.00, coins: btc},
				{at: 2 * time.Minute, drift: 25.00, coins: btc},
				{at: 3 * time.Minute, drift: 8.00, coins: []string{"ETH"}},
			},
			clear: true, wantRecovered: true, wantPrior: 4,
		},
		{
			name:     "log throttled per interval",
			throttle: time.Hour,
			steps: []driftTrackerStep{
				{at: 0, drift: 5.00, coins: btc, notify: no, log: yes},
				{at: time.Second, drift: 5.00, coins: btc, notify: yes, log: yes},
				{at: 2 * time.Second, drift: 5.00, coins: btc, notify: no, log: no},
				{at: 30 * time.Minute, drift: 5.00, coins: btc, notify: no, log: no},
				{at: time.Second + time.Hour, drift: 5.00, coins: btc, notify: yes, log: yes},
			},
		},
		{
			name: "worsening drift logs within interval",
			steps: []driftTrackerStep{
				{at: 0, drift: 5.00, coins: btc, notify: no, log: yes},
				{at: time.Second, drift: 5.00, coins: []string{"ETH"}, notify: no, log: no},
				{at: 2 * time.Second, drift: 20.00, coins: []string{"SOL"}, notify: no, log: yes},
			},
		},
		{
			name:     "stable drift re-alerts hourly, not every tenth cycle",
			throttle: time.Hour,
			steps:    stableHour(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.throttle > 0 {
				withAlertThrottleInterval(t, tc.throttle)
			}
			key := tc.key
			if key == "" {
				key = "hyperliquid/0xabc"
			}
			tr := &SharedWalletDriftTracker{}
			now := time.Now().UTC()
			for i, st := range tc.steps {
				notify, log, count := tr.Record(key, st.drift, st.coins, now.Add(st.at))
				if st.notify != nil && notify != *st.notify {
					t.Fatalf("step %d (drift %v coins %v): notify=%v, want %v (count=%d)", i+1, st.drift, st.coins, notify, *st.notify, count)
				}
				if st.log != nil && log != *st.log {
					t.Fatalf("step %d (drift %v coins %v): log=%v, want %v", i+1, st.drift, st.coins, log, *st.log)
				}
				if st.count > 0 && count != st.count {
					t.Fatalf("step %d (drift %v coins %v): count=%d, want %d", i+1, st.drift, st.coins, count, st.count)
				}
			}
			if tc.clear {
				recovered, prior := tr.Clear(key)
				if recovered != tc.wantRecovered {
					t.Fatalf("Clear: recovered=%v prior=%d, want recovered=%v", recovered, prior, tc.wantRecovered)
				}
				if tc.wantPriorNonZero && prior == 0 {
					t.Fatalf("Clear: prior=%d, want nonzero", prior)
				}
				if tc.wantPrior > 0 && prior != tc.wantPrior {
					t.Fatalf("Clear: prior=%d, want %d", prior, tc.wantPrior)
				}
			}
		})
	}
}

func TestComputeSubsetDisplayValue(t *testing.T) {
	hlKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	hlPair := []StrategyConfig{hlLivePerps("hl-btc", "BTC", 500), hlLivePerps("hl-eth", "ETH", 500)}
	gated := func(cash, value float64) *StrategyState {
		return &StrategyState{Cash: cash, Positions: map[string]*Position{}, SharedWalletValue: value, SharedWalletValueSet: true}
	}
	ungated := func(cash float64) *StrategyState {
		return &StrategyState{Cash: cash, Positions: map[string]*Position{}}
	}
	cases := []struct {
		name         string
		strategies   []StrategyConfig
		states       map[string]*StrategyState
		balances     map[SharedWalletKey]float64
		sharedFrom   int
		subsetN      int
		want         float64
		wantFallback *bool
	}{
		{"gated partial slice matches row value", hlPair,
			map[string]*StrategyState{"hl-btc": gated(350, 650), "hl-eth": gated(500, 350)},
			map[SharedWalletKey]float64{hlKey: 1000}, 2, 1, 650, new(bool)},
		{"gated full wallet uses real balance", hlPair,
			map[string]*StrategyState{"hl-btc": gated(350, 650), "hl-eth": gated(500, 350)},
			map[SharedWalletKey]float64{hlKey: 1000}, 2, 2, 1000, nil},
		{"mixed gated and non-shared", append(append([]StrategyConfig{}, hlPair...), StrategyConfig{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000}),
			map[string]*StrategyState{"hl-btc": gated(350, 650), "hl-eth": gated(500, 350), "spot-btc": ungated(2000)},
			map[SharedWalletKey]float64{hlKey: 1000}, 3, 3, 650 + 350 + 2000, new(bool)},
		{"ungated dedups against balance", hlPair,
			map[string]*StrategyState{"hl-btc": ungated(400), "hl-eth": ungated(600)},
			map[SharedWalletKey]float64{hlKey: 800}, 2, 2, 800, new(bool)},
		{"ungated missing balance falls back to subset sum", hlPair,
			map[string]*StrategyState{"hl-btc": ungated(400), "hl-eth": ungated(600)},
			nil, 2, 2, 1000, func() *bool { b := true; return &b }()},
		{"gated wallet incl. live manual counts real balance once",
			append(append([]StrategyConfig{}, hlPair...), hlLiveManual("hl-manual", 200)),
			map[string]*StrategyState{"hl-btc": gated(350, 500), "hl-eth": gated(500, 300), "hl-manual": gated(200, 200)},
			map[SharedWalletKey]float64{hlKey: 1000}, 2, 3, 1000, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
			state := &AppState{Strategies: map[string]*StrategyState{}}
			for id, s := range tc.states {
				s.ID = id
				state.Strategies[id] = s
			}
			accountShared := detectSharedWallets(tc.strategies[:tc.sharedFrom])
			got, fb := computeSubsetDisplayValue(tc.strategies[:tc.subsetN], state, nil, tc.balances, accountShared)
			if got != tc.want {
				t.Errorf("display value=%.2f, want %.2f", got, tc.want)
			}
			if tc.wantFallback != nil && fb != *tc.wantFallback {
				t.Errorf("usedFallback=%v, want %v", fb, *tc.wantFallback)
			}
		})
	}
}

func TestFormatSharedWalletJournalAlerts(t *testing.T) {
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	t.Run("orphan alert names wallet, coins, and untracked status", func(t *testing.T) {
		msg := formatSharedWalletJournalOrphanAlert(key, 1000, 1000, 0.0, 2, []string{"BTC", "ETH"})
		for _, want := range []string{"ORPHAN POSITION", "hyperliquid/0xabc", "BTC, ETH", "NO strategy"} {
			if !strings.Contains(msg, want) {
				t.Errorf("orphan alert missing %q: %s", want, msg)
			}
		}
	})
	t.Run("drift alert carries orphan context and never claims within tolerance", func(t *testing.T) {
		with := formatSharedWalletJournalDriftAlert(key, 1000, 995, 5.00, 2, []string{"BTC"})
		if strings.Contains(with, "within tolerance") {
			t.Errorf("over-tolerance drift alert must never claim within tolerance: %s", with)
		}
		for _, want := range []string{"DRIFT (exchange journal)", "BTC", "NO strategy"} {
			if !strings.Contains(with, want) {
				t.Errorf("drift alert with orphan missing %q: %s", want, with)
			}
		}
		if without := formatSharedWalletJournalDriftAlert(key, 1000, 995, 5.00, 2, nil); strings.Contains(without, "NO strategy") {
			t.Errorf("no-orphan drift alert must not mention orphans: %s", without)
		}
	})
}

func TestReportSharedWalletDrift_JournalStreakPreservedOnPending(t *testing.T) {
	useFreshDriftTracker(t)

	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	jkey := sharedWalletKeyLabel(key) + journalDriftStreakKeySuffix

	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 1 {
		t.Fatalf("journal streak should record under %q: %+v", jkey, e)
	}
	if sharedWalletDriftTracker.entries[sharedWalletKeyLabel(key)] != nil {
		t.Error("journal basis must NOT touch the bare trade-ledger streak key")
	}

	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 0.0, Balance: 1000, JournalPending: true},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 1 {
		t.Fatalf("JournalPending must preserve the journal streak unchanged: %+v", e)
	}

	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 2 {
		t.Fatalf("streak must continue across the transient, reaching confirmation: %+v", e)
	}
}

func TestReportSharedWalletDrift_JournalOrphanTripsWithoutTotalDrift(t *testing.T) {
	useFreshDriftTracker(t)

	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	jkey := sharedWalletKeyLabel(key) + journalDriftStreakKeySuffix
	orphan := func() []sharedWalletDriftResult {
		return []sharedWalletDriftResult{
			{Key: key, Drift: 0.0, Balance: 1000, ExpectedEquity: 1000, Basis: driftBasisJournal, OrphanCoins: []string{"BTC"}},
		}
	}

	reportSharedWalletDrift(nil, orphan())
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 1 {
		t.Fatalf("orphan with ~0 total drift must still record (trip): %+v", e)
	}
	reportSharedWalletDrift(nil, orphan())
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 2 {
		t.Fatalf("a persistent journal orphan must keep confirming: %+v", e)
	}

	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 0.0, Balance: 1000, ExpectedEquity: 1000, Basis: driftBasisJournal},
	})
	if sharedWalletDriftTracker.entries[jkey] != nil {
		t.Error("a clean journal cycle (no orphan, no total drift) must clear the streak")
	}
}

func TestCashflowJournalPersistentPendingFallsBackToLedgerAlarm(t *testing.T) {
	useFreshDriftTracker(t)
	prevPending := cashflowJournalPendingStreaks
	cashflowJournalPendingStreaks = &cashflowJournalPendingTracker{}
	defer func() { cashflowJournalPendingStreaks = prevPending }()

	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	label := sharedWalletKeyLabel(key)
	jkey := label + journalDriftStreakKeySuffix
	mk := func() []sharedWalletDriftResult {
		return []sharedWalletDriftResult{
			{Key: key, Drift: 5.00, Balance: 1000, MemberSum: 1005, OrphanCoins: []string{"BTC"}},
		}
	}
	notUsable := &cashflowJournalReconcile{Key: key, Usable: false}

	for cycle := 1; cycle <= sharedWalletDriftAlertThreshold; cycle++ {
		res := mk()
		applyCashflowJournalDriftBasis(res, key, notUsable, true)
		if !res[0].JournalPending {
			t.Fatalf("cycle %d within the window must stay pending: %+v", cycle, res[0])
		}
		reportSharedWalletDrift(nil, res)
		if sharedWalletDriftTracker.entries[label] != nil {
			t.Fatalf("a suppressed transient cycle must not touch the trade-ledger streak (cycle %d)", cycle)
		}
	}

	res := mk()
	applyCashflowJournalDriftBasis(res, key, notUsable, true)
	if res[0].JournalPending || res[0].Basis != "" || res[0].Drift != 5.00 {
		t.Fatalf("first persistent cycle must fall back to the trade-ledger drift: %+v", res[0])
	}
	reportSharedWalletDrift(nil, res)
	if e := sharedWalletDriftTracker.entries[label]; e == nil || e.cycles != 1 {
		t.Fatalf("persistent fallback must Record the trade-ledger drift under the bare key: %+v", e)
	}

	res = mk()
	applyCashflowJournalDriftBasis(res, key, notUsable, true)
	reportSharedWalletDrift(nil, res)
	if e := sharedWalletDriftTracker.entries[label]; e == nil || e.cycles != 2 || !e.alerted {
		t.Fatalf("trade-ledger alarm must confirm within a bounded window during a persistent outage: %+v", e)
	}
	if sharedWalletDriftTracker.entries[jkey] != nil {
		t.Error("the trade-ledger fallback must not touch the journal streak key")
	}
}

func TestReportSharedWalletDrift_CompoundOrphanAndDriftReportsDrift(t *testing.T) {
	useFreshDriftTracker(t)

	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"hyperliquid": "chan"},
		ownerID:  "owner",
	})
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	compound := func() []sharedWalletDriftResult {
		return []sharedWalletDriftResult{
			{Key: key, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal, OrphanCoins: []string{"BTC"}},
		}
	}
	reportSharedWalletDrift(notifier, compound())
	reportSharedWalletDrift(notifier, compound())
	if len(mock.dms) == 0 {
		t.Fatal("compound over-tolerance drift + orphan must alarm after confirmation")
	}
	msg := mock.dms[len(mock.dms)-1].content
	if strings.Contains(msg, "within tolerance") {
		t.Errorf("compound state must NOT claim the total is within tolerance: %s", msg)
	}
	for _, want := range []string{"DRIFT (exchange journal)", "BTC", "NO strategy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("compound drift alert missing %q: %s", want, msg)
		}
	}
}

func TestReportSharedWalletDrift_JournalGapConfirmsDespiteOrphanChurn(t *testing.T) {
	useFreshDriftTracker(t)

	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"hyperliquid": "chan"},
		ownerID:  "owner",
	})

	churnKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xchurn"}
	churnJKey := sharedWalletKeyLabel(churnKey) + journalDriftStreakKeySuffix
	gap := func(orphan string) []sharedWalletDriftResult {
		return []sharedWalletDriftResult{
			{Key: churnKey, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal, OrphanCoins: []string{orphan}},
		}
	}
	reportSharedWalletDrift(notifier, gap("BTC"))
	if len(mock.dms) != 0 {
		t.Fatalf("cycle 1 must not confirm yet: %+v", mock.dms)
	}
	reportSharedWalletDrift(notifier, gap("ETH"))
	if e := sharedWalletDriftTracker.entries[churnJKey]; e == nil || !e.alerted {
		t.Fatalf("a persistent journal gap must confirm despite orphan churn: %+v", e)
	}
	if len(mock.dms) == 0 || !strings.Contains(mock.dms[len(mock.dms)-1].content, "DRIFT (exchange journal)") {
		t.Errorf("the journal-gap alarm must fire on confirmation: %+v", mock.dms)
	}

	mock.dms = nil
	noOrphanKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xclean"}
	noOrphanJKey := sharedWalletKeyLabel(noOrphanKey) + journalDriftStreakKeySuffix
	clean := []sharedWalletDriftResult{{Key: noOrphanKey, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal}}
	reportSharedWalletDrift(notifier, clean)
	reportSharedWalletDrift(notifier, clean)
	if e := sharedWalletDriftTracker.entries[noOrphanJKey]; e == nil || !e.alerted {
		t.Fatalf("a no-orphan persistent journal gap must still confirm: %+v", e)
	}
}

func TestReportSharedWalletDrift_JournalGapBlipDoesNotConfirm(t *testing.T) {
	useFreshDriftTracker(t)

	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	jkey := sharedWalletKeyLabel(key) + journalDriftStreakKeySuffix

	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal, OrphanCoins: []string{"BTC"}},
	})
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 0.0, Balance: 1000, ExpectedEquity: 1000, Basis: driftBasisJournal, OrphanCoins: []string{"ETH"}},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e != nil && e.alerted {
		t.Fatalf("a single-cycle total-drift blip with churning orphans must NOT confirm: %+v", e)
	}
}

func TestReportSharedWalletDrift_BasisSwitchClearsStaleTradeLedgerEntry(t *testing.T) {
	useFreshDriftTracker(t)

	mock := &mockNotifier{}
	notifier := NewMultiNotifier(notifierBackend{
		notifier: mock,
		channels: map[string]string{"hyperliquid": "chan"},
		ownerID:  "owner",
	})
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	label := sharedWalletKeyLabel(key)
	jkey := label + journalDriftStreakKeySuffix

	ledgerDrift := func(d float64) []sharedWalletDriftResult {
		return []sharedWalletDriftResult{{Key: key, Drift: d, Balance: 1000, MemberSum: 1000 + d}}
	}
	journalClean := func() []sharedWalletDriftResult {
		return []sharedWalletDriftResult{{Key: key, Drift: 0.0, Balance: 1000, ExpectedEquity: 1000, Basis: driftBasisJournal}}
	}

	reportSharedWalletDrift(notifier, ledgerDrift(5.00))
	reportSharedWalletDrift(notifier, ledgerDrift(5.00))
	if e := sharedWalletDriftTracker.entries[label]; e == nil || !e.alerted {
		t.Fatalf("trade-ledger outage must alert under the bare label key: %+v", e)
	}
	mock.dms = nil

	reportSharedWalletDrift(notifier, journalClean())
	if sharedWalletDriftTracker.entries[label] != nil {
		t.Error("a return to the journal basis must clear the stale trade-ledger entry")
	}
	if len(mock.dms) == 0 || !strings.Contains(mock.dms[len(mock.dms)-1].content, "RESOLVED") {
		t.Errorf("a stranded trade-ledger alert must fire a RESOLVED notice on recovery: %+v", mock.dms)
	}

	mock.dms = nil
	reportSharedWalletDrift(notifier, ledgerDrift(10.00))
	if e := sharedWalletDriftTracker.entries[label]; e == nil || e.cycles != 1 || e.alerted {
		t.Fatalf("second outage cycle 1 must start fresh (count 1, not alerted): %+v", e)
	}
	if len(mock.dms) != 0 {
		t.Errorf("second outage must NOT alert on its first cycle (no early fire off a stale entry): %+v", mock.dms)
	}
	reportSharedWalletDrift(notifier, ledgerDrift(10.00))
	if e := sharedWalletDriftTracker.entries[label]; e == nil || !e.alerted {
		t.Fatalf("second outage must alert only after the 2-cycle confirmation: %+v", e)
	}

	if sharedWalletDriftTracker.entries[jkey] != nil {
		t.Error("the trade-ledger path must never create the journal streak key")
	}
}

func TestReportSharedWalletDrift_TradeLedgerBasisPreservesJournalStreak(t *testing.T) {
	useFreshDriftTracker(t)

	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	jkey := sharedWalletKeyLabel(key) + journalDriftStreakKeySuffix

	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 1 {
		t.Fatalf("journal episode should record under the journal key: %+v", e)
	}
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 0.0, Balance: 1000, MemberSum: 1000},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 1 {
		t.Fatalf("trade-ledger basis must preserve the journal streak (not clear it): %+v", e)
	}
}
