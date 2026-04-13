package main

import (
	"errors"
	"testing"
)

// stubFetcher returns canned balances/errors for tests so we never hit the network.
func stubFetcher(balances map[SharedWalletKey]float64, errs map[SharedWalletKey]error) SharedWalletBalanceFetcher {
	return func(key SharedWalletKey) (float64, error) {
		if err, ok := errs[key]; ok {
			return 0, err
		}
		if bal, ok := balances[key]; ok {
			return bal, nil
		}
		return 0, errors.New("no stub for key")
	}
}

// TestDetectSharedWallets_MultipleHLPerps verifies that two live Hyperliquid
// perps strategies on the same account are detected as sharing one wallet.
func TestDetectSharedWallets_MultipleHLPerps(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}

	shared := detectSharedWallets(strategies)
	if len(shared) != 1 {
		t.Fatalf("expected 1 shared wallet; got %d", len(shared))
	}
	for key, ids := range shared {
		if key.Platform != "hyperliquid" || key.Account != "0xtest" {
			t.Errorf("unexpected key %+v", key)
		}
		if len(ids) != 2 {
			t.Errorf("expected 2 strategies in wallet; got %d", len(ids))
		}
	}
}

// TestDetectSharedWallets_PaperModeIgnored verifies that paper-mode HL strategies
// are not treated as shared (they don't actually touch a real account).
func TestDetectSharedWallets_PaperModeIgnored(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=paper"}, Capital: 5000},
	}

	shared := detectSharedWallets(strategies)
	if len(shared) != 0 {
		t.Errorf("expected no shared wallets for paper-mode strategies; got %d", len(shared))
	}
}

// TestDetectSharedWallets_SingleStrategyNotShared verifies that a single
// strategy on a wallet is NOT classified as shared (no double-count concern).
func TestDetectSharedWallets_SingleStrategyNotShared(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
	}

	shared := detectSharedWallets(strategies)
	if len(shared) != 0 {
		t.Errorf("expected single-strategy wallet not to be shared; got %d entries", len(shared))
	}
}

// TestDetectSharedWallets_NoEnvVar verifies that without HYPERLIQUID_ACCOUNT_ADDRESS
// no wallets are detected as shared (we have no way to identify them).
func TestDetectSharedWallets_NoEnvVar(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}

	shared := detectSharedWallets(strategies)
	if len(shared) != 0 {
		t.Errorf("expected no shared wallets without HYPERLIQUID_ACCOUNT_ADDRESS; got %d", len(shared))
	}
}

// TestComputeTotalPortfolioValue_SharedWalletUsesRealBalance is the core
// regression test for issue #243: two live HL strategies on the same account
// must contribute the real wallet balance ONCE, not the sum of their per-strategy
// PortfolioValue.
func TestComputeTotalPortfolioValue_SharedWalletUsesRealBalance(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-sma-btc": {ID: "hl-sma-btc", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-rsi-eth": {ID: "hl-rsi-eth", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}
	walletBalances := map[SharedWalletKey]float64{
		{Platform: "hyperliquid", Account: "0xtest"}: 5000,
	}

	got, usedFallback := computeTotalPortfolioValue(strategies, state, nil, walletBalances, nil)
	want := 5000.0 // single wallet, NOT 5000 + 5000
	if got != want {
		t.Errorf("expected total=%v (real wallet balance); got %v (likely double-counted)", want, got)
	}
	if usedFallback {
		t.Errorf("expected usedFallback=false when balance was provided")
	}
}

// TestComputeTotalPortfolioValue_FallbackUsesMaxNotSum verifies that when the
// real-balance fetch fails for a shared wallet, the function falls back to the
// MAX of member strategies' PVs — NOT the sum. Summing members would
// re-introduce the exact #243 double-count bug during transient fetch failures
// and could permanently inflate PortfolioRisk.PeakValue.
func TestComputeTotalPortfolioValue_FallbackUsesMaxNotSum(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-sma-btc": {ID: "hl-sma-btc", Cash: 4000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-rsi-eth": {ID: "hl-rsi-eth", Cash: 6000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}

	// Empty walletBalances (simulates fetch failure) — should fall back to
	// MAX(4000, 6000) = 6000, NOT 4000 + 6000 = 10000 (which would be the
	// #243 double-count bug in disguise).
	got, usedFallback := computeTotalPortfolioValue(strategies, state, nil, nil, nil)
	want := 6000.0
	if got != want {
		t.Errorf("expected fallback total=%v (max of members); got %v — sum of members would re-introduce #243", want, got)
	}
	if !usedFallback {
		t.Errorf("expected usedFallback=true on fetch failure so caller can freeze peak")
	}
}

// TestComputeTotalPortfolioValue_FallbackPreventsPeakInflation is a tabletop
// verification of the peak-protection contract: two members with PVs that
// summed would exceed PeakValue must NOT produce a total above the max member.
func TestComputeTotalPortfolioValue_FallbackPreventsPeakInflation(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	// Simulate a real wallet of ~7000 split across two strategies.
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-a": {ID: "hl-a", Cash: 3500, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-b": {ID: "hl-b", Cash: 3500, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}

	got, usedFallback := computeTotalPortfolioValue(strategies, state, nil, nil, nil)
	// Must NOT be 7000 (sum) — that's the #243 bug. Should be 3500 (max).
	if got == 7000 {
		t.Errorf("fallback total=%v matches sum-of-members — #243 double-count bug returned!", got)
	}
	if got != 3500 {
		t.Errorf("expected fallback total=3500 (max of members); got %v", got)
	}
	if !usedFallback {
		t.Errorf("usedFallback must be true so main.go can freeze peak")
	}
}

// TestComputeTotalPortfolioValue_MixedSharedAndNonShared verifies that a mix of
// shared-wallet and standalone strategies sums correctly: real balance once for
// the shared wallet PLUS per-strategy PV for the standalone ones.
func TestComputeTotalPortfolioValue_MixedSharedAndNonShared(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-sma-btc": {ID: "hl-sma-btc", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-rsi-eth": {ID: "hl-rsi-eth", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"spot-btc":   {ID: "spot-btc", Cash: 2000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}
	walletBalances := map[SharedWalletKey]float64{
		{Platform: "hyperliquid", Account: "0xtest"}: 7500, // wallet has dropped from 10k to 7500
	}

	got, usedFallback := computeTotalPortfolioValue(strategies, state, nil, walletBalances, nil)
	want := 9500.0 // 7500 (shared wallet) + 2000 (spot)
	if got != want {
		t.Errorf("expected mixed total=%v; got %v", want, got)
	}
	if usedFallback {
		t.Errorf("expected usedFallback=false when balance was provided")
	}
}

// TestComputeTotalPortfolioValue_MixedPaperAndLiveHL verifies the edge case
// raised in the PR review (#256): one --mode=paper HL strategy and one
// --mode=live HL strategy on the same env-var address. walletKeyFor filters
// on live-mode, so neither should be classified as shared (the single live
// strategy is alone on its wallet); the live strategy contributes its own PV
// like any non-shared strategy, and the paper strategy is always non-shared.
// No real-balance fetch should be needed because nothing is shared.
func TestComputeTotalPortfolioValue_MixedPaperAndLiveHL(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-paper-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 5000},
		{ID: "hl-live-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}

	// Sanity-check that detection matches expectation: nothing shared, since
	// only one live strategy is on the wallet.
	shared := detectSharedWallets(strategies)
	if len(shared) != 0 {
		t.Fatalf("expected no shared wallets in mixed paper+live setup; got %d", len(shared))
	}

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-paper-btc": {ID: "hl-paper-btc", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-live-eth":  {ID: "hl-live-eth", Cash: 4500, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}

	got, usedFallback := computeTotalPortfolioValue(strategies, state, nil, nil, nil)
	want := 9500.0 // both strategies contribute their PV independently
	if got != want {
		t.Errorf("expected mixed paper+live total=%v; got %v", want, got)
	}
	if usedFallback {
		t.Errorf("expected usedFallback=false; nothing was classified as shared")
	}
}

// TestComputeTotalPortfolioValue_NoSharedWalletsBehavesLikeOldSum verifies that
// when no strategies share a wallet, the function reduces to the original
// per-strategy sum (no behavioral change for non-shared setups — issue #243
// acceptance criterion).
func TestComputeTotalPortfolioValue_NoSharedWalletsBehavesLikeOldSum(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	strategies := []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000},
		{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 3000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"spot-btc": {ID: "spot-btc", Cash: 2000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"spot-eth": {ID: "spot-eth", Cash: 3000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}

	got, usedFallback := computeTotalPortfolioValue(strategies, state, nil, nil, nil)
	want := 5000.0
	if got != want {
		t.Errorf("expected total=%v; got %v", want, got)
	}
	if usedFallback {
		t.Errorf("expected usedFallback=false when no shared wallets exist")
	}
}

// TestFetchSharedWalletBalances_StubReturnsBalance verifies that the fetcher
// shim collects balances from the injected fetcher.
func TestFetchSharedWalletBalances_StubReturnsBalance(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	fetcher := stubFetcher(map[SharedWalletKey]float64{key: 7777}, nil)

	balances, errs := fetchSharedWalletBalances(strategies, fetcher)
	if len(errs) != 0 {
		t.Errorf("expected no errors; got %v", errs)
	}
	if balances[key] != 7777 {
		t.Errorf("expected balance=7777; got %v", balances[key])
	}
}

// TestFetchSharedWalletBalances_RecordsErrors verifies that fetcher errors are
// surfaced via the errs map (so the caller can warn-and-fall-back).
func TestFetchSharedWalletBalances_RecordsErrors(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	fetcher := stubFetcher(nil, map[SharedWalletKey]error{key: errors.New("boom")})

	balances, errs := fetchSharedWalletBalances(strategies, fetcher)
	if len(balances) != 0 {
		t.Errorf("expected no balances on error; got %v", balances)
	}
	if errs[key] == nil {
		t.Errorf("expected recorded error for key %+v", key)
	}
}

// TestComputeInitialPortfolioPeak_SharedWalletUsesBalance verifies that
// PeakValue init uses the real wallet balance once for shared wallets instead
// of summing per-strategy capital.
func TestComputeInitialPortfolioPeak_SharedWalletUsesBalance(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000},
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	fetcher := stubFetcher(map[SharedWalletKey]float64{key: 8000}, nil)

	got := computeInitialPortfolioPeak(strategies, fetcher)
	want := 10000.0 // 8000 wallet + 2000 spot
	if got != want {
		t.Errorf("expected peak=%v; got %v", want, got)
	}
}

// TestComputeInitialPortfolioPeak_FallbackOnFetchError verifies that when the
// fetch fails the peak falls back to the sum of per-strategy capital so the
// risk loop is not initialized with a 0 wallet.
func TestComputeInitialPortfolioPeak_FallbackOnFetchError(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	fetcher := stubFetcher(nil, map[SharedWalletKey]error{key: errors.New("network down")})

	got := computeInitialPortfolioPeak(strategies, fetcher)
	want := 10000.0 // fallback to summed capital
	if got != want {
		t.Errorf("expected fallback peak=%v; got %v", want, got)
	}
}

// TestComputeInitialPortfolioPeak_LegacyCapitalPct verifies that single-strategy
// capital_pct setups still derive wallet balance via Capital / CapitalPct and
// count each platform once — preserving the pre-#243 behavior so existing
// non-shared setups are unchanged.
func TestComputeInitialPortfolioPeak_LegacyCapitalPct(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	// Single capital_pct strategy: Capital=2500, CapitalPct=0.5 → wallet=5000.
	strategies := []StrategyConfig{
		{ID: "binance-spot", Platform: "binanceus", Type: "spot", Capital: 2500, CapitalPct: 0.5},
		{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 1000},
	}

	got := computeInitialPortfolioPeak(strategies, nil)
	want := 6000.0 // 5000 (derived wallet via legacy) + 1000 (fixed capital)
	if got != want {
		t.Errorf("expected legacy capital_pct peak=%v; got %v", want, got)
	}
}

// TestComputeInitialPortfolioPeak_NoSharedWalletsSumsCapital verifies that
// existing non-shared setups are unchanged.
func TestComputeInitialPortfolioPeak_NoSharedWalletsSumsCapital(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	strategies := []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000},
		{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 3000},
	}

	got := computeInitialPortfolioPeak(strategies, nil)
	want := 5000.0
	if got != want {
		t.Errorf("expected peak=%v; got %v", want, got)
	}
}
