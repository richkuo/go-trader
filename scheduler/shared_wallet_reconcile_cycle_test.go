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


func TestSharedWalletDriftTracker_ConfirmThenThrottleThenRecover(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now); notify {
		t.Fatal("first detection must NOT alert (confirmation window)")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Minute)); !notify {
		t.Fatal("second consecutive detection must alert")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(2*time.Minute)); notify {
		t.Error("third identical detection should be throttled")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 9.00, []string{"BTC"}, now.Add(3*time.Minute)); !notify {
		t.Error("materially changed drift should re-alert")
	}
	recovered, prior := tr.Clear("hyperliquid/0xabc")
	if !recovered || prior == 0 {
		t.Errorf("expected recovery after alerted streak, got recovered=%v prior=%d", recovered, prior)
	}
	if r, _ := tr.Clear("okx/none"); r {
		t.Error("clearing unknown wallet must not report recovery")
	}
}

func TestSharedWalletDriftTracker_OneCycleTransientSilent(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now); notify {
		t.Fatal("single transient detection must not alert")
	}
	recovered, _ := tr.Clear("hyperliquid/0xabc")
	if recovered {
		t.Error("a never-alerted transient must not fire a recovery notice")
	}
}

func TestReportSharedWalletDrift_WithinToleranceNoPanic(t *testing.T) {
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: SharedWalletKey{Platform: "hyperliquid", Account: "0x"}, Drift: 0.004, Balance: 100, MemberSum: 100},
	})
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

func TestSharedWalletDriftTracker_DistinctConsecutiveTransientsNoAlert(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _, count := tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now); notify || count != 1 {
		t.Fatalf("first transient: want no alert at count 1, got notify=%v count=%d", notify, count)
	}
	if notify, _, count := tr.Record("hyperliquid/0xabc", 12.00, []string{"ETH"}, now.Add(time.Minute)); notify || count != 2 {
		t.Fatalf("second distinct transient: want no alert at cycle 2, got notify=%v count=%d", notify, count)
	}
	if recovered, _ := tr.Clear("hyperliquid/0xabc"); recovered {
		t.Error("never-alerted streak must not fire a recovery notice")
	}
}

func TestSharedWalletDriftTracker_SameOrphanChangingMagnitudeStillAlerts(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 25.00, []string{"SOL"}, now); notify {
		t.Fatal("first detection must not alert")
	}
	if notify, _, count := tr.Record("hyperliquid/0xabc", 31.40, []string{"SOL"}, now.Add(time.Minute)); !notify || count != 2 {
		t.Fatalf("same orphan second cycle must alert at count 2, got notify=%v count=%d", notify, count)
	}
}


func TestComputeSubsetDisplayValue_GatedPartialSliceMatchesRows(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	allStrategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc": {ID: "hl-btc", Cash: 350, Positions: map[string]*Position{}, SharedWalletValue: 650, SharedWalletValueSet: true},
		"hl-eth": {ID: "hl-eth", Cash: 500, Positions: map[string]*Position{}, SharedWalletValue: 350, SharedWalletValueSet: true},
	}}
	walletBalances := map[SharedWalletKey]float64{{Platform: "hyperliquid", Account: "0xtest"}: 1000}
	accountShared := detectSharedWallets(allStrategies)

	got, fb := computeSubsetDisplayValue(allStrategies[:1], state, nil, walletBalances, accountShared)
	if got != 650 {
		t.Errorf("gated partial slice: want 650 (= row value), got %.2f", got)
	}
	if fb {
		t.Error("gated partial slice: expected usedFallback=false")
	}

	got, _ = computeSubsetDisplayValue(allStrategies, state, nil, walletBalances, accountShared)
	if got != 1000 {
		t.Errorf("gated full wallet: want 1000 (real balance), got %.2f", got)
	}
}

func TestComputeSubsetDisplayValue_MixedGatedAndNonShared(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	allStrategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc":   {ID: "hl-btc", Cash: 350, Positions: map[string]*Position{}, SharedWalletValue: 650, SharedWalletValueSet: true},
		"hl-eth":   {ID: "hl-eth", Cash: 500, Positions: map[string]*Position{}, SharedWalletValue: 350, SharedWalletValueSet: true},
		"spot-btc": {ID: "spot-btc", Cash: 2000, Positions: map[string]*Position{}},
	}}
	walletBalances := map[SharedWalletKey]float64{{Platform: "hyperliquid", Account: "0xtest"}: 1000}
	accountShared := detectSharedWallets(allStrategies)

	got, fb := computeSubsetDisplayValue(allStrategies, state, nil, walletBalances, accountShared)
	if want := 650.0 + 350.0 + 2000.0; got != want {
		t.Errorf("mixed subset: want %.2f, got %.2f", want, got)
	}
	if fb {
		t.Error("mixed subset: expected usedFallback=false")
	}
}

func TestComputeSubsetDisplayValue_UngatedFallsBackToSubsetSemantics(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	allStrategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc": {ID: "hl-btc", Cash: 400, Positions: map[string]*Position{}},
		"hl-eth": {ID: "hl-eth", Cash: 600, Positions: map[string]*Position{}},
	}}
	accountShared := detectSharedWallets(allStrategies)
	walletBalances := map[SharedWalletKey]float64{{Platform: "hyperliquid", Account: "0xtest"}: 800}

	got, fb := computeSubsetDisplayValue(allStrategies, state, nil, walletBalances, accountShared)
	if got != 800 || fb {
		t.Errorf("ungated dedup: want 800/false, got %.2f/%v", got, fb)
	}
	got, fb = computeSubsetDisplayValue(allStrategies, state, nil, nil, accountShared)
	if got != 1000 || !fb {
		t.Errorf("ungated missing balance: want 1000/true, got %.2f/%v", got, fb)
	}
}

func TestComputeSubsetDisplayValue_GatedManualNoDoubleCount(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	allStrategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc":    {ID: "hl-btc", Cash: 350, Positions: map[string]*Position{}, SharedWalletValue: 500, SharedWalletValueSet: true},
		"hl-eth":    {ID: "hl-eth", Cash: 500, Positions: map[string]*Position{}, SharedWalletValue: 300, SharedWalletValueSet: true},
		"hl-manual": {ID: "hl-manual", Cash: 200, Positions: map[string]*Position{}, SharedWalletValue: 200, SharedWalletValueSet: true},
	}}
	walletBalances := map[SharedWalletKey]float64{{Platform: "hyperliquid", Account: "0xtest"}: 1000}
	accountShared := detectSharedWallets(allStrategies[:2])

	got, _ := computeSubsetDisplayValue(allStrategies, state, nil, walletBalances, accountShared)
	if got != 1000 {
		t.Errorf("gated wallet incl. manual: want exactly 1000 (real balance, no double count), got %.2f", got)
	}
}

func TestSharedWalletDriftTracker_PersistentOrphanSurvivesChurn(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now); notify {
		t.Fatal("first detection must not alert")
	}
	if notify, _, count := tr.Record("hyperliquid/0xabc", 30.00, []string{"BTC", "DOGE"}, now.Add(time.Minute)); !notify || count != 2 {
		t.Fatalf("persistent BTC orphan must alert through churn, got notify=%v count=%d", notify, count)
	}
	if notify, _, count := tr.Record("hyperliquid/0xabc", 30.00, []string{"BTC", "SHIB"}, now.Add(2*time.Minute)); notify || count != 3 {
		t.Errorf("already-alerted persistent orphan should be throttled, got notify=%v count=%d", notify, count)
	}
}

func TestSharedWalletDriftTracker_NewOrphanAfterAlertReconfirms(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now)
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now.Add(time.Minute)); !notify {
		t.Fatal("BTC orphan must alert on its second cycle")
	}
	if notify, _, count := tr.Record("hyperliquid/0xabc", 25.00, []string{"ETH"}, now.Add(2*time.Minute)); notify || count != 3 {
		t.Fatalf("new orphan's first cycle must not alert, got notify=%v count=%d", notify, count)
	}
	if notify, _, count := tr.Record("hyperliquid/0xabc", 25.00, []string{"ETH"}, now.Add(3*time.Minute)); !notify || count != 4 {
		t.Fatalf("new persistent orphan must re-confirm and alert, got notify=%v count=%d", notify, count)
	}
}

func TestSharedWalletDriftTracker_NoOrphanCoinsStillConfirms(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _, _ := tr.Record("okx/acct", 5.00, nil, now); notify {
		t.Fatal("first detection must not alert")
	}
	if notify, _, count := tr.Record("okx/acct", 5.00, nil, now.Add(time.Minute)); !notify || count != 2 {
		t.Fatalf("coinless drift must alert on second consecutive cycle, got notify=%v count=%d", notify, count)
	}
}

func TestSharedWalletDriftTracker_MarkWiggleStaysThrottled(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now)
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Minute)); !notify {
		t.Fatal("confirmation alert must fire on cycle 2")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.20, []string{"BTC"}, now.Add(2*time.Minute)); notify {
		t.Error("+4% mark wiggle must stay throttled")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.40, []string{"BTC"}, now.Add(3*time.Minute)); notify {
		t.Error("+8% cumulative wiggle must stay throttled")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.60, []string{"BTC"}, now.Add(4*time.Minute)); !notify {
		t.Error("+12% cumulative move must re-alert")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.65, []string{"BTC"}, now.Add(5*time.Minute)); notify {
		t.Error("small wiggle vs the NEW anchor must stay throttled")
	}
}

func TestSharedWalletDriftTracker_RecoveryCountSurvivesChurn(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now)
	tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now.Add(time.Minute))
	tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now.Add(2*time.Minute))
	tr.Record("hyperliquid/0xabc", 8.00, []string{"ETH"}, now.Add(3*time.Minute))
	recovered, prior := tr.Clear("hyperliquid/0xabc")
	if !recovered || prior != 4 {
		t.Fatalf("want recovery with 4-cycle duration, got recovered=%v prior=%d", recovered, prior)
	}
}

func TestSharedWalletDriftTracker_LogThrottledPerInterval(t *testing.T) {
	withAlertThrottleInterval(t, time.Hour)
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now); notify || !log {
		t.Fatalf("onset cycle: want notify=false log=true, got notify=%v log=%v", notify, log)
	}
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Second)); !notify || !log {
		t.Fatalf("alert cycle: want notify=true log=true, got notify=%v log=%v", notify, log)
	}
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(2*time.Second)); notify || log {
		t.Fatalf("intra-interval cycle: want notify=false log=false, got notify=%v log=%v", notify, log)
	}
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(30*time.Minute)); notify || log {
		t.Fatalf("mid-hour stable cycle: want notify=false log=false, got notify=%v log=%v", notify, log)
	}
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Second+time.Hour)); !notify || !log {
		t.Fatalf("hourly re-alert cycle: want notify=true log=true, got notify=%v log=%v", notify, log)
	}
}

func TestSharedWalletDriftTracker_WorseningDriftLogsWithinInterval(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now); notify || !log {
		t.Fatalf("onset cycle: want notify=false log=true, got notify=%v log=%v", notify, log)
	}
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"ETH"}, now.Add(time.Second)); notify || log {
		t.Fatalf("stable churn cycle: want notify=false log=false, got notify=%v log=%v", notify, log)
	}
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 20.00, []string{"SOL"}, now.Add(2*time.Second)); notify || !log {
		t.Fatalf("worsening cycle: want notify=false log=true, got notify=%v log=%v", notify, log)
	}
}

func TestSharedWalletDriftTracker_StableDriftRealertsHourlyNotEveryTenth(t *testing.T) {
	withAlertThrottleInterval(t, time.Hour)
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now)
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Second)); !notify {
		t.Fatal("confirmation alert must fire on cycle 2")
	}
	base := now.Add(time.Second)
	for i := 1; i <= 40; i++ {
		ts := base.Add(time.Duration(i) * 3 * time.Second)
		if notify, _, count := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, ts); notify {
			t.Fatalf("stable drift must not re-alert within the hour (cycle count=%d)", count)
		}
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, base.Add(time.Hour+time.Second)); !notify {
		t.Fatal("stable drift must re-alert once the hourly back-off elapses")
	}
}

func TestReportSharedWalletDrift_JournalStreakPreservedOnPending(t *testing.T) {
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

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
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

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

func TestFormatSharedWalletJournalOrphanAlert(t *testing.T) {
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	msg := formatSharedWalletJournalOrphanAlert(key, 1000, 1000, 0.0, 2, []string{"BTC", "ETH"})
	for _, want := range []string{"ORPHAN POSITION", "hyperliquid/0xabc", "BTC, ETH", "NO strategy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("orphan alert missing %q: %s", want, msg)
		}
	}
}

func TestCashflowJournalPersistentPendingFallsBackToLedgerAlarm(t *testing.T) {
	prevTracker := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prevTracker }()
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
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

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
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

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
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

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
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

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
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

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

func TestFormatSharedWalletJournalDriftAlert_OrphanContext(t *testing.T) {
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
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
}
