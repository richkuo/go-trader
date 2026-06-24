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

// buildSharedWalletTestState assembles two HL members (BTC, ETH) plus one
// non-member paper strategy, each with a virtual position so the reconciler can
// attribute on-chain P&L.
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

// sdb=nil → the HL wallet uses the #918 capital-weight split fallback (the
// #954 ledger path needs a StateDB; see shared_wallet_ledger_test.go). The
// gating/summing contract under test is identical on both paths.
func TestReconcileSharedWalletDisplayValues_SetsGatesAndSums(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	state, strategies := buildSharedWalletTestState()
	sharedWallets := detectSharedWallets(strategies)
	if len(sharedWallets) != 1 {
		t.Fatalf("expected 1 shared wallet, got %d", len(sharedWallets))
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	walletBalances := map[SharedWalletKey]float64{key: 1030.0} // base 1000 + 50 - 20
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
	// base=1000; btc: 0.6*1000+50=650; eth: 0.4*1000-20=380.
	if math.Abs(btc.SharedWalletValue-650) > 0.01 {
		t.Errorf("btc value = %v, want 650", btc.SharedWalletValue)
	}
	if math.Abs(eth.SharedWalletValue-380) > 0.01 {
		t.Errorf("eth value = %v, want 380", eth.SharedWalletValue)
	}
	if sum := btc.SharedWalletValue + eth.SharedWalletValue; math.Abs(sum-1030.0) > 0.01 {
		t.Errorf("member sum %v != balance 1030", sum)
	}
	// displayStrategyValue must now return the exchange-derived value.
	if got := displayStrategyValue(btc, nil); math.Abs(got-650) > 0.01 {
		t.Errorf("displayStrategyValue(btc) = %v, want 650", got)
	}
}

// A live HL `manual` strategy on the same account holds a real on-chain
// position (returned by fetchHyperliquidState) but is not a perps member. It
// must be folded in as a member so its position is attributed (no orphan drift)
// and it receives an exchange-derived value (#920 review).
func TestReconcileSharedWalletDisplayValues_ManualMemberAttributed(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	state, strategies := buildSharedWalletTestState()
	// Add a live manual strategy on SOL (same account), with a virtual position.
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
	// Balance includes the SOL manual position's uPnL (+15).
	walletBalances := map[SharedWalletKey]float64{key: 1045.0} // base 1000 + 50 - 20 + 15
	hlPositions := []HLPosition{
		{Coin: "BTC", Size: 0.1, UnrealizedPnL: 50},
		{Coin: "ETH", Size: 2, UnrealizedPnL: -20},
		{Coin: "SOL", Size: 5, UnrealizedPnL: 15}, // manual's on-chain position
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
	// Σ all three members == balance.
	sum := state.Strategies["hl-btc"].SharedWalletValue +
		state.Strategies["hl-eth"].SharedWalletValue + msol.SharedWalletValue
	if math.Abs(sum-1045.0) > 0.01 {
		t.Errorf("member sum %v != balance 1045", sum)
	}
	// Manual gets its own uPnL (+15) plus a capital-weighted base share.
	if math.Abs(msol.SharedWalletValue-(200.0/1200.0*1000.0+15)) > 0.01 {
		t.Errorf("manual value = %v, want %v", msol.SharedWalletValue, 200.0/1200.0*1000.0+15)
	}
}

// OKX with a failed position fetch this cycle must be skipped (members fall back
// to PortfolioValue) rather than reconciled with U=0.
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

	// okxPositionsFetched=false → OKX wallet must be skipped.
	results := reconcileSharedWalletDisplayValues(strategies, state, nil, sharedWallets, walletBalances, nil, nil, false)
	if len(results) != 0 {
		t.Fatalf("expected OKX wallet skipped when positions not fetched, got %d results", len(results))
	}
	if state.Strategies["okx-a"].SharedWalletValueSet || state.Strategies["okx-b"].SharedWalletValueSet {
		t.Error("OKX members must fall back (Set=false) when positions fetch failed")
	}

	// With okxPositionsFetched=true it reconciles.
	results = reconcileSharedWalletDisplayValues(strategies, state, nil, sharedWallets, walletBalances, nil, nil, true)
	if len(results) != 1 || !state.Strategies["okx-a"].SharedWalletValueSet {
		t.Fatalf("expected OKX reconcile when positions fetched, got %+v", results)
	}
}

func TestReconcileSharedWalletDisplayValues_FetchFailedFallsBack(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	state, strategies := buildSharedWalletTestState()
	// Pre-set a stale value to prove it gets cleared.
	state.Strategies["hl-btc"].SharedWalletValue = 999
	state.Strategies["hl-btc"].SharedWalletValueSet = true
	sharedWallets := detectSharedWallets(strategies)

	// Empty walletBalances simulates a failed balance fetch this cycle.
	results := reconcileSharedWalletDisplayValues(strategies, state, nil, sharedWallets, map[SharedWalletKey]float64{}, nil, nil, false)

	if len(results) != 0 {
		t.Fatalf("expected no drift results when balance missing, got %d", len(results))
	}
	if state.Strategies["hl-btc"].SharedWalletValueSet {
		t.Error("stale SharedWalletValueSet must be cleared when fetch fails")
	}
	// Fallback to modeled PortfolioValue (cash 300 + 0.1*price; price absent → AvgCost).
	want := PortfolioValue(state.Strategies["hl-btc"], nil)
	if got := displayStrategyValue(state.Strategies["hl-btc"], nil); got != want {
		t.Errorf("display fallback = %v, want PortfolioValue %v", got, want)
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

// --- Drift alarm tracker ---

func TestSharedWalletDriftTracker_ConfirmThenThrottleThenRecover(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	// First detection is within the confirmation window → no alert yet.
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now); notify {
		t.Fatal("first detection must NOT alert (confirmation window)")
	}
	// Second consecutive detection crosses the threshold → alert.
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Minute)); !notify {
		t.Fatal("second consecutive detection must alert")
	}
	// Same drift again → throttled (no signature change, <1h since last alert).
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(2*time.Minute)); notify {
		t.Error("third identical detection should be throttled")
	}
	// Materially changed drift → re-alert.
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 9.00, []string{"BTC"}, now.Add(3*time.Minute)); !notify {
		t.Error("materially changed drift should re-alert")
	}
	// Recovery: within tolerance clears and reports recovered.
	recovered, prior := tr.Clear("hyperliquid/0xabc")
	if !recovered || prior == 0 {
		t.Errorf("expected recovery after alerted streak, got recovered=%v prior=%d", recovered, prior)
	}
	// Clearing a never-seen wallet is a no-op.
	if r, _ := tr.Clear("okx/none"); r {
		t.Error("clearing unknown wallet must not report recovery")
	}
}

// A one-cycle orphan (e.g. a freshly-filled limit order not yet booked into the
// virtual book) must produce NEITHER an alert NOR a recovery notice.
func TestSharedWalletDriftTracker_OneCycleTransientSilent(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now); notify {
		t.Fatal("single transient detection must not alert")
	}
	// Next cycle the book catches up → within tolerance → Clear.
	recovered, _ := tr.Clear("hyperliquid/0xabc")
	if recovered {
		t.Error("a never-alerted transient must not fire a recovery notice")
	}
}

func TestReportSharedWalletDrift_WithinToleranceNoPanic(t *testing.T) {
	// nil notifier must be safe; within-tolerance drift records nothing.
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: SharedWalletKey{Platform: "hyperliquid", Account: "0x"}, Drift: 0.004, Balance: 100, MemberSum: 100},
	})
}

// --- Parse extensions carry unrealized P&L ---

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

// Two DIFFERENT one-cycle transients on consecutive cycles (e.g. a resting
// limit fill on BTC, then an external manual open on ETH) must not be read as
// one persistent orphan: the streak is keyed on the orphan-coin signature, so
// neither alerts and no recovery notice fires (#920 review).
func TestSharedWalletDriftTracker_DistinctConsecutiveTransientsNoAlert(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _, count := tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now); notify || count != 1 {
		t.Fatalf("first transient: want no alert at count 1, got notify=%v count=%d", notify, count)
	}
	// Next cycle a DIFFERENT orphan appears → per-coin confirmation restarts
	// (no alert); the wallet-level duration counter still advances to 2.
	if notify, _, count := tr.Record("hyperliquid/0xabc", 12.00, []string{"ETH"}, now.Add(time.Minute)); notify || count != 2 {
		t.Fatalf("second distinct transient: want no alert at cycle 2, got notify=%v count=%d", notify, count)
	}
	// Clean cycle → never alerted, so no recovery notice either.
	if recovered, _ := tr.Clear("hyperliquid/0xabc"); recovered {
		t.Error("never-alerted streak must not fire a recovery notice")
	}
}

// A persistent orphan keeps the same coin signature even as its drift magnitude
// moves with the mark each cycle — it must still alert on the second cycle.
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

// --- computeSubsetDisplayValue (#920 review: TOTAL must reconcile with rows) ---

// A partial slice of a shared wallet (per-asset summary, leaderboard top-N)
// whose members carry exchange-derived values must total to the SAME values the
// rows show — not the modeled virtual sum.
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

	// Per-asset slice: just hl-btc. The single row shows 650; the TOTAL must too
	// (the old virtual-sum path would show the modeled 350).
	got, fb := computeSubsetDisplayValue(allStrategies[:1], state, nil, walletBalances, accountShared)
	if got != 650 {
		t.Errorf("gated partial slice: want 650 (= row value), got %.2f", got)
	}
	if fb {
		t.Error("gated partial slice: expected usedFallback=false")
	}

	// Full wallet: gated values sum to the real balance exactly.
	got, _ = computeSubsetDisplayValue(allStrategies, state, nil, walletBalances, accountShared)
	if got != 1000 {
		t.Errorf("gated full wallet: want 1000 (real balance), got %.2f", got)
	}
}

// Gated wallet members plus a non-shared strategy: gated sum + modeled PV.
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

// With no gates set (reconcile skipped — fetch failure, or summary CLI where no
// reconcile ran), the function must be byte-identical to the #915
// computeSubsetPortfolioValue semantics, including the fallback flag.
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

	// Fully contained, balance present → real-balance dedup (matches #915).
	got, fb := computeSubsetDisplayValue(allStrategies, state, nil, walletBalances, accountShared)
	if got != 800 || fb {
		t.Errorf("ungated dedup: want 800/false, got %.2f/%v", got, fb)
	}
	// Balance missing → virtual-sum fallback with usedFallback=true.
	got, fb = computeSubsetDisplayValue(allStrategies, state, nil, nil, accountShared)
	if got != 1000 || !fb {
		t.Errorf("ungated missing balance: want 1000/true, got %.2f/%v", got, fb)
	}
}

// A gated same-account live manual strategy is OUTSIDE detectSharedWallets
// membership but INSIDE the reconciled wallet balance. Summing display values
// must yield exactly the balance — the old path added the manual's modeled PV
// on top of the wallet balance (double count).
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
	// detectSharedWallets is perps-only: hl-manual is NOT a member here.
	accountShared := detectSharedWallets(allStrategies[:2])

	got, _ := computeSubsetDisplayValue(allStrategies, state, nil, walletBalances, accountShared)
	if got != 1000 {
		t.Errorf("gated wallet incl. manual: want exactly 1000 (real balance, no double count), got %.2f", got)
	}
}

// A persistent orphan must keep confirming even while one-cycle transients on
// OTHER coins churn the orphan set around it ({A} → {A,B} → {A,C}): continuity
// is per coin, not exact-set equality (#920 review round 2).
func TestSharedWalletDriftTracker_PersistentOrphanSurvivesChurn(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now); notify {
		t.Fatal("first detection must not alert")
	}
	// BTC persists; a transient DOGE orphan joins → BTC's streak reaches the
	// threshold and must alert despite the set change.
	if notify, _, count := tr.Record("hyperliquid/0xabc", 30.00, []string{"BTC", "DOGE"}, now.Add(time.Minute)); !notify || count != 2 {
		t.Fatalf("persistent BTC orphan must alert through churn, got notify=%v count=%d", notify, count)
	}
	// DOGE clears, a different transient SHIB joins; BTC drift unchanged →
	// throttled (BTC already alerted, SHIB at streak 1, magnitude stable).
	if notify, _, count := tr.Record("hyperliquid/0xabc", 30.00, []string{"BTC", "SHIB"}, now.Add(2*time.Minute)); notify || count != 3 {
		t.Errorf("already-alerted persistent orphan should be throttled, got notify=%v count=%d", notify, count)
	}
}

// A NEW persistent orphan appearing right after a prior alert (no clean cycle
// in between) must re-confirm and alert deterministically once ITS streak
// crosses the threshold — even when the drift magnitude happens to match the
// last-notified value, so the magnitude-based re-alert never fires.
func TestSharedWalletDriftTracker_NewOrphanAfterAlertReconfirms(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now)
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now.Add(time.Minute)); !notify {
		t.Fatal("BTC orphan must alert on its second cycle")
	}
	// BTC clears but ETH goes orphan the same cycle (drift stays over
	// tolerance, magnitude coincidentally identical) → new confirmation window
	// (the wallet-level duration counter keeps running: cycle 3).
	if notify, _, count := tr.Record("hyperliquid/0xabc", 25.00, []string{"ETH"}, now.Add(2*time.Minute)); notify || count != 3 {
		t.Fatalf("new orphan's first cycle must not alert, got notify=%v count=%d", notify, count)
	}
	// ETH persists → crosses ITS confirmation window → must alert even though
	// the wallet already alerted and the magnitude never changed.
	if notify, _, count := tr.Record("hyperliquid/0xabc", 25.00, []string{"ETH"}, now.Add(3*time.Minute)); !notify || count != 4 {
		t.Fatalf("new persistent orphan must re-confirm and alert, got notify=%v count=%d", notify, count)
	}
}

// Over-tolerance drift with NO orphan coins (weighting bug) confirms like a
// bare consecutive counter via the pseudo-coin slot.
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

// A confirmed orphan's uPnL moves with the mark every cycle. Sub-10% wiggle
// around the last-NOTIFIED drift must stay throttled (the old cycle-over-cycle
// 1¢ gate re-alerted every cycle); only a cumulative move past the relative
// threshold re-surfaces it (#920 review round 4).
func TestSharedWalletDriftTracker_MarkWiggleStaysThrottled(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now)
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Minute)); !notify {
		t.Fatal("confirmation alert must fire on cycle 2")
	}
	// +4% then +8% vs the notified $5.00 anchor → throttled despite each move
	// exceeding a cent.
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.20, []string{"BTC"}, now.Add(2*time.Minute)); notify {
		t.Error("+4% mark wiggle must stay throttled")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.40, []string{"BTC"}, now.Add(3*time.Minute)); notify {
		t.Error("+8% cumulative wiggle must stay throttled")
	}
	// +12% vs the anchor → materially changed → re-alert, and the anchor moves.
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.60, []string{"BTC"}, now.Add(4*time.Minute)); !notify {
		t.Error("+12% cumulative move must re-alert")
	}
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.65, []string{"BTC"}, now.Add(5*time.Minute)); notify {
		t.Error("small wiggle vs the NEW anchor must stay throttled")
	}
}

// The recovery notice reports the wallet-level over-tolerance duration, which
// must survive the orphan coin churning during the episode (per-coin streaks
// would report the final coin's short streak).
func TestSharedWalletDriftTracker_RecoveryCountSurvivesChurn(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now)
	tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now.Add(time.Minute))   // alert
	tr.Record("hyperliquid/0xabc", 25.00, []string{"BTC"}, now.Add(2*time.Minute)) // persists
	// Final over-tolerance cycle: BTC resolves but a fresh ETH transient drifts.
	tr.Record("hyperliquid/0xabc", 8.00, []string{"ETH"}, now.Add(3*time.Minute))
	recovered, prior := tr.Clear("hyperliquid/0xabc")
	if !recovered || prior != 4 {
		t.Fatalf("want recovery with 4-cycle duration, got recovered=%v prior=%d", recovered, prior)
	}
}

// #1088: the stdout [WARN] log must NOT fire every reconcile cycle. For a
// STABLE drift it logs at the onset of an over-tolerance episode, on any cycle
// that fires an operator alert, and otherwise at most once per the (hourly)
// sharedWalletDriftLogInterval heartbeat — so intra-hour cycles stay silent even
// though every cycle is over tolerance and Record runs every cycle. This is the
// 620-lines-in-6h spam the issue reported; a 1-minute interval only halved it,
// so the heartbeat is aligned to the hourly notification cadence.
func TestSharedWalletDriftTracker_LogThrottledPerInterval(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	// Cycle 1: onset. In the confirmation window (no alert) but MUST log once.
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now); notify || !log {
		t.Fatalf("onset cycle: want notify=false log=true, got notify=%v log=%v", notify, log)
	}
	// Cycle 2 (+1s): confirmation alert fires → log forced true alongside it.
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Second)); !notify || !log {
		t.Fatalf("alert cycle: want notify=true log=true, got notify=%v log=%v", notify, log)
	}
	// Cycle 3 (+2s): stable drift, throttled alert AND within the heartbeat
	// interval of the last log → MUST NOT log (this is the spam the issue
	// reported).
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(2*time.Second)); notify || log {
		t.Fatalf("intra-interval cycle: want notify=false log=false, got notify=%v log=%v", notify, log)
	}
	// A cycle well into the hour but still under the heartbeat (and under the
	// hourly re-alert) stays silent — no spam from a stable drift.
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(30*time.Minute)); notify || log {
		t.Fatalf("mid-hour stable cycle: want notify=false log=false, got notify=%v log=%v", notify, log)
	}
	// The next log for a stable drift coincides with the hourly re-alert (which
	// forces a log), one hour after the last notification (the +1s alert).
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Second+time.Hour)); !notify || !log {
		t.Fatalf("hourly re-alert cycle: want notify=true log=true, got notify=%v log=%v", notify, log)
	}
}

// #1088: a WORSENING drift must log immediately — even within the heartbeat
// interval and even when no alert fires — so the operator keeps per-move
// visibility of a growing sub-threshold drift (the explicit reason #1088
// throttled rather than gated the log behind shouldNotify). Driven here through
// a churning orphan set so each coin only ever reaches streak 1: the wallet
// stays in the confirmation window (no alert can fire), isolating the
// materially-changed LOG gate from the notification path.
func TestSharedWalletDriftTracker_WorseningDriftLogsWithinInterval(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	// Cycle 1: onset, orphan BTC. Confirmation window (no alert) but logs once.
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now); notify || !log {
		t.Fatalf("onset cycle: want notify=false log=true, got notify=%v log=%v", notify, log)
	}
	// Cycle 2 (+1s): orphan churns to ETH (BTC drops out) → still streak 1, no
	// alert; drift unchanged and within the heartbeat → MUST NOT log.
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"ETH"}, now.Add(time.Second)); notify || log {
		t.Fatalf("stable churn cycle: want notify=false log=false, got notify=%v log=%v", notify, log)
	}
	// Cycle 3 (+2s): orphan churns to SOL (still no alert), but the drift jumps
	// from $5 to $20 (a >10% material move) → MUST log immediately despite being
	// far inside the hourly heartbeat and despite no alert firing.
	if notify, log, _ := tr.Record("hyperliquid/0xabc", 20.00, []string{"SOL"}, now.Add(2*time.Second)); notify || !log {
		t.Fatalf("worsening cycle: want notify=false log=true, got notify=%v log=%v", notify, log)
	}
}

// #1088: removing the cycles%10 re-alert case means a stable, already-alerted
// drift re-alerts on the HOURLY back-off only — not every 10th cycle. With a
// short reconcile cadence the old %10 rule fired a Discord/DM alert roughly
// every few minutes; the hourly case was preempted and never reached.
func TestSharedWalletDriftTracker_StableDriftRealertsHourlyNotEveryTenth(t *testing.T) {
	tr := &SharedWalletDriftTracker{}
	now := time.Now().UTC()
	tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now) // onset
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, now.Add(time.Second)); !notify {
		t.Fatal("confirmation alert must fire on cycle 2")
	}
	// Drive many stable over-tolerance cycles, each 3s apart (well under 1h).
	// The old %10 rule would have re-alerted at cycles 10, 20, 30, ...; the
	// hourly back-off must keep every one of them throttled.
	base := now.Add(time.Second)
	for i := 1; i <= 40; i++ {
		ts := base.Add(time.Duration(i) * 3 * time.Second)
		if notify, _, count := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, ts); notify {
			t.Fatalf("stable drift must not re-alert within the hour (cycle count=%d)", count)
		}
	}
	// Past one hour since the last notification → the hourly back-off re-alerts.
	if notify, _, _ := tr.Record("hyperliquid/0xabc", 5.00, []string{"BTC"}, base.Add(time.Hour+time.Second)); !notify {
		t.Fatal("stable drift must re-alert once the hourly back-off elapses")
	}
}

// #1107 (Optional #1): a transient not-usable journal cycle (JournalPending)
// must PRESERVE the journal streak — never reset the 2-cycle confirmation off
// the within-tolerance trade-ledger fallback — and the journal basis must track
// a DISTINCT streak key from the trade-ledger basis so neither resets the other.
func TestReportSharedWalletDrift_JournalStreakPreservedOnPending(t *testing.T) {
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	jkey := sharedWalletKeyLabel(key) + journalDriftStreakKeySuffix

	// Cycle 1: journal basis, total drift over tolerance -> recorded under the
	// DISTINCT journal key, NOT the bare trade-ledger label.
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 1 {
		t.Fatalf("journal streak should record under %q: %+v", jkey, e)
	}
	if sharedWalletDriftTracker.entries[sharedWalletKeyLabel(key)] != nil {
		t.Error("journal basis must NOT touch the bare trade-ledger streak key")
	}

	// Cycle 2: journal transiently not usable -> JournalPending. The streak is
	// PRESERVED (no Record, no Clear), so the confirmation window survives the
	// feed miss instead of resetting off a clean trade-ledger fallback.
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 0.0, Balance: 1000, JournalPending: true},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 1 {
		t.Fatalf("JournalPending must preserve the journal streak unchanged: %+v", e)
	}

	// Cycle 3: journal usable and over tolerance again -> the preserved streak
	// confirms within the 2-cycle window despite the intervening transient miss.
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 2 {
		t.Fatalf("streak must continue across the transient, reaching confirmation: %+v", e)
	}
}

// #1107 (Optional #2): under the journal basis the exchange total reconciles an
// unowned position to ~0, so orphan exposure is absent from the total drift. The
// alarm must TRIP on an orphan coin regardless (real unmanaged exposure), confirm
// across cycles, and reset only when the orphan is gone AND the total reconciles.
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

	// Orphan resolved AND total reconciles -> within tolerance, streak clears.
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

// #1107 (Needs Fixing): a PERSISTENT journal feed outage must not suppress the
// money-path drift alarm indefinitely. A short transient stays suppressed
// (journal streak preserved), but once the consecutive-pending streak passes the
// confirmation window, applyCashflowJournalDriftBasis fails closed to the
// trade-ledger basis — exactly like the incomplete latch — so a real drift still
// confirms and alarms within a bounded window during the outage.
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
	// A real, persistent trade-ledger drift sits under the wallet the entire time.
	mk := func() []sharedWalletDriftResult {
		return []sharedWalletDriftResult{
			{Key: key, Drift: 5.00, Balance: 1000, MemberSum: 1005, OrphanCoins: []string{"BTC"}},
		}
	}
	notUsable := &cashflowJournalReconcile{Key: key, Usable: false}

	// Transient window: each miss is suppressed (JournalPending) — nothing is
	// recorded under any streak key, so the alarm stays quiet (and the journal
	// streak is preserved for a journal-gap episode).
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

	// First persistent cycle (past the window): fail closed to the trade-ledger
	// basis → Records the real drift under the bare label key (count 1, no alert).
	res := mk()
	applyCashflowJournalDriftBasis(res, key, notUsable, true)
	if res[0].JournalPending || res[0].Basis != "" || res[0].Drift != 5.00 {
		t.Fatalf("first persistent cycle must fall back to the trade-ledger drift: %+v", res[0])
	}
	reportSharedWalletDrift(nil, res)
	if e := sharedWalletDriftTracker.entries[label]; e == nil || e.cycles != 1 {
		t.Fatalf("persistent fallback must Record the trade-ledger drift under the bare key: %+v", e)
	}

	// Second persistent cycle: the trade-ledger streak confirms within the
	// 2-cycle window → the alarm fires despite the ongoing journal outage. The
	// alarm is bounded, not dark for the outage's duration.
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

// #1107 (Optional): when the journal basis carries BOTH an over-tolerance total
// drift AND an unowned position, reportSharedWalletDrift must emit the
// journal-DRIFT alert (reporting the real drift) and must NEVER select the orphan
// alert, whose wording asserts the total is "within tolerance". The orphan is
// folded in as context.
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
	// Two consecutive cycles confirm and fire the alert on the second.
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

// #1107 (Optional): a basis switch must not strand a stale trade-ledger tracker
// entry. After a persistent journal outage alarms under the bare label key, a
// return to the journal basis must clear that entry (firing its RESOLVED notice),
// so the operator's alert resolves AND a LATER outage still requires the full
// 2-cycle confirmation rather than re-firing off the stale alerted entry.
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

	// Trade-ledger basis (Basis == "") — the persistent-outage fallback path.
	ledgerDrift := func(d float64) []sharedWalletDriftResult {
		return []sharedWalletDriftResult{{Key: key, Drift: d, Balance: 1000, MemberSum: 1000 + d}}
	}
	// Journal basis, within tolerance — the authoritative basis governing cleanly.
	journalClean := func() []sharedWalletDriftResult {
		return []sharedWalletDriftResult{{Key: key, Drift: 0.0, Balance: 1000, ExpectedEquity: 1000, Basis: driftBasisJournal}}
	}

	// Persistent outage on the trade-ledger basis: confirm + alert under `label`.
	reportSharedWalletDrift(notifier, ledgerDrift(5.00))
	reportSharedWalletDrift(notifier, ledgerDrift(5.00))
	if e := sharedWalletDriftTracker.entries[label]; e == nil || !e.alerted {
		t.Fatalf("trade-ledger outage must alert under the bare label key: %+v", e)
	}
	mock.dms = nil // drop the alert DMs; the RESOLVED notice is what we assert next

	// (a) Journal recovers clean → the stale `label` entry is cleared and a
	// RESOLVED notice fires; the bare-label entry is gone.
	reportSharedWalletDrift(notifier, journalClean())
	if sharedWalletDriftTracker.entries[label] != nil {
		t.Error("a return to the journal basis must clear the stale trade-ledger entry")
	}
	if len(mock.dms) == 0 || !strings.Contains(mock.dms[len(mock.dms)-1].content, "RESOLVED") {
		t.Errorf("a stranded trade-ledger alert must fire a RESOLVED notice on recovery: %+v", mock.dms)
	}

	// (b) A SECOND outage (materially different drift, so a stale alerted entry
	// would re-fire on cycle 1 via the e.alerted && sigChanged arm) must instead
	// require the full 2-cycle confirmation — proving the stale entry was cleared.
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

	// (c) sanity: the journal streak key was never touched by the trade-ledger path.
	if sharedWalletDriftTracker.entries[jkey] != nil {
		t.Error("the trade-ledger path must never create the journal streak key")
	}
}

// #1107 (Optional): the directional clear must NOT touch the journal streak from
// the trade-ledger basis — the journal streak is deliberately preserved across
// journal unavailability and resumes on recovery (inverse of the stranding fix).
func TestReportSharedWalletDrift_TradeLedgerBasisPreservesJournalStreak(t *testing.T) {
	prev := sharedWalletDriftTracker
	sharedWalletDriftTracker = &SharedWalletDriftTracker{}
	defer func() { sharedWalletDriftTracker = prev }()

	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xabc"}
	jkey := sharedWalletKeyLabel(key) + journalDriftStreakKeySuffix

	// A journal-basis episode confirms (count 1).
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 5.00, Balance: 1000, ExpectedEquity: 995, Basis: driftBasisJournal},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 1 {
		t.Fatalf("journal episode should record under the journal key: %+v", e)
	}
	// A within-tolerance trade-ledger cycle (persistent-outage fallback that
	// happens to be clean) must NOT clear the journal streak.
	reportSharedWalletDrift(nil, []sharedWalletDriftResult{
		{Key: key, Drift: 0.0, Balance: 1000, MemberSum: 1000},
	})
	if e := sharedWalletDriftTracker.entries[jkey]; e == nil || e.cycles != 1 {
		t.Fatalf("trade-ledger basis must preserve the journal streak (not clear it): %+v", e)
	}
}

// #1107 (Optional): the journal-drift alert formatter folds an orphan in as
// context when present and omits the clause otherwise — and never claims "within
// tolerance" (it is the over-tolerance alert).
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
