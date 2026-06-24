package main

import (
	"errors"
	"testing"
)

// stubFetcher returns canned balances/errors for tests so we never hit the network.
func stubFetcher(balances map[SharedWalletKey]float64, errs map[SharedWalletKey]error) WalletBalanceFetcher {
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

// TestWalletKeyFor_OKX_PerpsLive verifies OKX perps live recognition via
// OKX_API_KEY (#357 phase 1a). The API key uniquely identifies the account.
func TestWalletKeyFor_OKX_PerpsLive(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-key-abc")

	sc := StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
		Args: []string{"sma", "BTC", "1h", "--mode=live"}}

	key, ok := walletKeyFor(sc)
	if !ok {
		t.Fatalf("expected OKX perps live to produce a wallet key")
	}
	if key.Platform != "okx" || key.Account != "okx-key-abc" {
		t.Errorf("unexpected key %+v", key)
	}
}

// TestWalletKeyFor_OKX_PaperNoKey verifies paper-mode OKX returns no key.
func TestWalletKeyFor_OKX_PaperNoKey(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-key-abc")

	sc := StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
		Args: []string{"sma", "BTC", "1h", "--mode=paper"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for paper-mode OKX")
	}
}

// TestWalletKeyFor_OKX_SpotNoKey verifies OKX spot is NOT recognized — only
// perps/swap uses margin positions that need shared-wallet grouping (#357).
func TestWalletKeyFor_OKX_SpotNoKey(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-key-abc")

	sc := StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "spot",
		Args: []string{"sma", "BTC", "1h", "--mode=live"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for OKX spot (not in registry)")
	}
}

// TestWalletKeyFor_OKX_MissingEnvVar verifies missing OKX_API_KEY returns no key.
func TestWalletKeyFor_OKX_MissingEnvVar(t *testing.T) {
	t.Setenv("OKX_API_KEY", "")

	sc := StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
		Args: []string{"sma", "BTC", "1h", "--mode=live"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key when OKX_API_KEY is unset")
	}
}

// TestWalletKeyFor_TopStep_FuturesLive verifies TopStep futures live recognition
// via TOPSTEP_ACCOUNT_ID (#357 phase 1a).
func TestWalletKeyFor_TopStep_FuturesLive(t *testing.T) {
	t.Setenv("TOPSTEP_ACCOUNT_ID", "ts-account-42")

	sc := StrategyConfig{ID: "ts-sma-es", Platform: "topstep", Type: "futures",
		Args: []string{"sma", "ES", "15m", "--mode=live"}}

	key, ok := walletKeyFor(sc)
	if !ok {
		t.Fatalf("expected TopStep futures live to produce a wallet key")
	}
	if key.Platform != "topstep" || key.Account != "ts-account-42" {
		t.Errorf("unexpected key %+v", key)
	}
}

// TestWalletKeyFor_TopStep_PaperNoKey verifies paper-mode TopStep returns no key.
func TestWalletKeyFor_TopStep_PaperNoKey(t *testing.T) {
	t.Setenv("TOPSTEP_ACCOUNT_ID", "ts-account-42")

	sc := StrategyConfig{ID: "ts-sma-es", Platform: "topstep", Type: "futures",
		Args: []string{"sma", "ES", "15m", "--mode=paper"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for paper-mode TopStep")
	}
}

// TestWalletKeyFor_TopStep_MissingEnvVar verifies missing TOPSTEP_ACCOUNT_ID
// returns no key.
func TestWalletKeyFor_TopStep_MissingEnvVar(t *testing.T) {
	t.Setenv("TOPSTEP_ACCOUNT_ID", "")

	sc := StrategyConfig{ID: "ts-sma-es", Platform: "topstep", Type: "futures",
		Args: []string{"sma", "ES", "15m", "--mode=live"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key when TOPSTEP_ACCOUNT_ID is unset")
	}
}

// TestWalletKeyFor_Robinhood_CryptoLive verifies Robinhood crypto spot live
// recognition via ROBINHOOD_USERNAME (#357 phase 1a). Multiple strategies
// trading the same asset from one RH account share its spot balance.
func TestWalletKeyFor_Robinhood_CryptoLive(t *testing.T) {
	t.Setenv("ROBINHOOD_USERNAME", "rh-user@example.com")

	sc := StrategyConfig{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
		Args: []string{"sma", "BTC", "1h", "--mode=live"}}

	key, ok := walletKeyFor(sc)
	if !ok {
		t.Fatalf("expected Robinhood crypto live to produce a wallet key")
	}
	if key.Platform != "robinhood" || key.Account != "rh-user@example.com" {
		t.Errorf("unexpected key %+v", key)
	}
}

// TestWalletKeyFor_Robinhood_PaperNoKey verifies paper-mode RH returns no key.
func TestWalletKeyFor_Robinhood_PaperNoKey(t *testing.T) {
	t.Setenv("ROBINHOOD_USERNAME", "rh-user@example.com")

	sc := StrategyConfig{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
		Args: []string{"sma", "BTC", "1h", "--mode=paper"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for paper-mode Robinhood")
	}
}

// TestWalletKeyFor_Robinhood_OptionsNoKey verifies RH options is NOT recognized
// (leg-aware close semantics are out of scope — tracked in #363).
func TestWalletKeyFor_Robinhood_OptionsNoKey(t *testing.T) {
	t.Setenv("ROBINHOOD_USERNAME", "rh-user@example.com")

	sc := StrategyConfig{ID: "rh-ccall-spy", Platform: "robinhood", Type: "options",
		Args: []string{"ccall", "SPY", "1h", "--mode=live"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for Robinhood options (not in registry)")
	}
}

// TestDetectSharedWallets_OKXIncludedAfterFetcher locks in #360 phase 2
// of #357: two live OKX perps strategies on the same API key are now grouped
// as a shared wallet because fetch_okx_balance.py provides real-balance
// lookup via defaultSharedWalletBalance. Before #360, OKX was deliberately
// excluded to avoid freezing the portfolio peak via fallback every cycle in
// computeTotalPortfolioValue.
func TestDetectSharedWallets_OKXIncludedAfterFetcher(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-key-abc")

	strategies := []StrategyConfig{
		{ID: "okx-sma-btc", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "okx-rsi-eth", Platform: "okx", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}

	shared := detectSharedWallets(strategies)
	if len(shared) != 1 {
		t.Fatalf("expected OKX to be grouped as one shared wallet (phase 2 #360), got %d entries", len(shared))
	}
	for _, sc := range strategies {
		if _, ok := walletKeyFor(sc); !ok {
			t.Errorf("walletKeyFor should recognize %s", sc.ID)
		}
	}
}

// TestDetectSharedWallets_TopStepGroupedWithFetcher — #1106 phase 4 of #1100
// added a TopStep balance fetcher, so two live TopStep futures strategies on one
// account are now grouped as a single shared wallet (mirrors the OKX test above).
func TestDetectSharedWallets_TopStepGroupedWithFetcher(t *testing.T) {
	t.Setenv("TOPSTEP_ACCOUNT_ID", "ts-account-42")

	strategies := []StrategyConfig{
		{ID: "ts-sma-es", Platform: "topstep", Type: "futures", Args: []string{"sma", "ES", "15m", "--mode=live"}, Capital: 5000},
		{ID: "ts-rsi-nq", Platform: "topstep", Type: "futures", Args: []string{"rsi", "NQ", "15m", "--mode=live"}, Capital: 5000},
	}

	shared := detectSharedWallets(strategies)
	if len(shared) != 1 {
		t.Fatalf("expected TopStep to be grouped as one shared wallet (phase 4 #1106), got %d entries", len(shared))
	}
	for _, sc := range strategies {
		if _, ok := walletKeyFor(sc); !ok {
			t.Errorf("walletKeyFor should recognize %s", sc.ID)
		}
	}
}

// TestDetectSharedWallets_RobinhoodExcludedNoFetcher — same as OKX, for Robinhood.
func TestDetectSharedWallets_RobinhoodExcludedNoFetcher(t *testing.T) {
	t.Setenv("ROBINHOOD_USERNAME", "rh-user@example.com")

	strategies := []StrategyConfig{
		{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "rh-rsi-eth", Platform: "robinhood", Type: "spot", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}

	shared := detectSharedWallets(strategies)
	if len(shared) != 0 {
		t.Errorf("expected Robinhood to be excluded from detectSharedWallets until a balance fetcher exists; got %d entries", len(shared))
	}
}

// TestHasSharedWalletBalanceFetcher_HLAndOKX locks in the contract that HL,
// OKX (#360 phase 2 of #357), and TopStep (#1106 phase 4 of #1100) have balance
// fetchers today. When phase 4 (RH) adds a fetcher, this test should be updated
// in the same PR as the fetcher wiring.
func TestHasSharedWalletBalanceFetcher_HLAndOKX(t *testing.T) {
	cases := map[string]bool{
		"hyperliquid": true,
		"okx":         true,
		"topstep":     true,
		"robinhood":   false,
		"binanceus":   false,
		"unknown":     false,
	}
	for platform, want := range cases {
		if got := hasSharedWalletBalanceFetcher(platform); got != want {
			t.Errorf("hasSharedWalletBalanceFetcher(%q) = %v; want %v", platform, got, want)
		}
	}
}

// TestDetectSharedWallets_MixedHLAndOKX verifies that when HL and OKX live
// strategies are configured together, BOTH are grouped as shared wallets
// after #360 phase 2 of #357 (OKX gained a balance fetcher). Guards against
// future refactors accidentally cross-contaminating the platform filter.
func TestDetectSharedWallets_MixedHLAndOKX(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xhl")
	t.Setenv("OKX_API_KEY", "okx-key-abc")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
		{ID: "okx-sma-btc", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "okx-rsi-eth", Platform: "okx", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}

	shared := detectSharedWallets(strategies)
	if len(shared) != 2 {
		t.Fatalf("expected 2 shared wallets (HL + OKX); got %d entries %+v", len(shared), shared)
	}
	hlKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xhl"}
	if ids, ok := shared[hlKey]; !ok || len(ids) != 2 {
		t.Errorf("expected HL wallet with 2 strategies; got ok=%v ids=%v", ok, ids)
	}
	okxKey := SharedWalletKey{Platform: "okx", Account: "okx-key-abc"}
	if ids, ok := shared[okxKey]; !ok || len(ids) != 2 {
		t.Errorf("expected OKX wallet with 2 strategies; got ok=%v ids=%v", ok, ids)
	}
}

// TestWalletKeyFor_SplitModeLiveRecognized verifies that the split-form
// "--mode live" (separate args) is recognized as live, not just the joined
// "--mode=live" form. HasLiveStrategy accepts both forms; walletKeyFor must
// agree so a split-form config does not silently bypass shared-wallet grouping.
func TestWalletKeyFor_SplitModeLiveRecognized(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	sc := StrategyConfig{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps",
		Args: []string{"sma", "BTC", "1h", "--mode", "live"}}

	key, ok := walletKeyFor(sc)
	if !ok {
		t.Fatalf("expected split-form --mode live to be recognized as live")
	}
	if key.Platform != "hyperliquid" || key.Account != "0xtest" {
		t.Errorf("unexpected key %+v", key)
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

// TestComputeTotalPortfolioValue_FallbackSumsMemberPVs verifies issue #452:
// when the real-balance fetch fails for a shared wallet, fallback must sum the
// member strategy PVs. The real-balance path above still prevents #243
// double-counting; fallback has no fetched wallet value to de-duplicate.
func TestComputeTotalPortfolioValue_FallbackSumsMemberPVs(t *testing.T) {
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
	// 4000 + 6000 = 10000, not max(4000, 6000) = 6000.
	got, usedFallback := computeTotalPortfolioValue(strategies, state, nil, nil, nil)
	want := 10000.0
	if got != want {
		t.Errorf("expected fallback total=%v (sum of members); got %v", want, got)
	}
	if !usedFallback {
		t.Errorf("expected usedFallback=true on fetch failure so caller can freeze peak")
	}
}

// TestComputeTotalPortfolioValue_FallbackKeepsPeakFreezeSignal verifies that
// the #452 sum fallback still tells main.go not to ratchet PeakValue upward
// during a balance-fetch failure.
func TestComputeTotalPortfolioValue_FallbackKeepsPeakFreezeSignal(t *testing.T) {
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
	if got != 7000 {
		t.Errorf("expected fallback total=7000 (sum of members); got %v", got)
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

// --- rebaselinePortfolioPeakAfterPrune (#650) ---

// TestRebaselinePortfolioPeakAfterPrune_SumsRemainingPerStrategyPeaks verifies
// that the rebaselined peak sums RiskState.PeakValue from surviving strategies,
// dropping the contribution of the pruned one.
func TestRebaselinePortfolioPeakAfterPrune_SumsRemainingPerStrategyPeaks(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 5000},
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"spot-btc": {ID: "spot-btc", RiskState: RiskState{PeakValue: 7000}},
	}}

	got := rebaselinePortfolioPeakAfterPrune(state, cfg, nil)
	want := 7000.0
	if got != want {
		t.Errorf("expected rebaselined peak=%v; got %v", want, got)
	}
}

// TestRebaselinePortfolioPeakAfterPrune_FloorAtCapitalSum verifies that when a
// surviving strategy has zero per-strategy peak (cold-start) the result floors
// at computeInitialPortfolioPeak so we never under-baseline.
func TestRebaselinePortfolioPeakAfterPrune_FloorAtCapitalSum(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 5000},
		{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 3000},
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		// One surviving strategy with no per-strategy peak yet.
		"spot-btc": {ID: "spot-btc"},
		"spot-eth": {ID: "spot-eth"},
	}}

	got := rebaselinePortfolioPeakAfterPrune(state, cfg, nil)
	want := 8000.0 // floor: 5000 + 3000
	if got != want {
		t.Errorf("expected floored peak=%v; got %v", want, got)
	}
}

// TestRebaselinePortfolioPeakAfterPrune_FallbackToCapitalWhenPeakMissing verifies
// that strategies missing per-strategy peak fall back to their configured
// capital (mixed with surviving strategies that do have peaks).
func TestRebaselinePortfolioPeakAfterPrune_FallbackToCapitalWhenPeakMissing(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 5000},
		{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 3000},
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"spot-btc": {ID: "spot-btc", RiskState: RiskState{PeakValue: 6000}},
		"spot-eth": {ID: "spot-eth"}, // no peak yet → use capital
	}}

	got := rebaselinePortfolioPeakAfterPrune(state, cfg, nil)
	want := 9000.0 // 6000 (peak) + 3000 (capital fallback)
	if got != want {
		t.Errorf("expected mixed peak=%v; got %v", want, got)
	}
}

// TestRebaselinePortfolioPeakAfterPrune_PreventsImmediateKillSwitch is the
// regression test for #650: pre-fix, a stale peak from a pruned multi-strategy
// run latched the kill switch on the first cycle. With the fix, the rebaseline
// reflects only surviving strategies so CheckPortfolioRisk does not fire.
func TestRebaselinePortfolioPeakAfterPrune_PreventsImmediateKillSwitch(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	// Surviving config: one strategy that itself peaked at $9034 and is still
	// sitting at $9034 (no drawdown on what's left).
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 5000},
	}}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"spot-btc": {ID: "spot-btc", RiskState: RiskState{PeakValue: 9034.24}},
		},
		PortfolioRisk: PortfolioRiskState{PeakValue: 15148.90}, // pre-prune peak
	}

	state.PortfolioRisk.PeakValue = rebaselinePortfolioPeakAfterPrune(state, cfg, nil)

	prsCfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	allowed, _, _, reason := CheckPortfolioRisk(&state.PortfolioRisk, prsCfg, 9034.24, 0, 0, 0)
	if !allowed {
		t.Errorf("expected kill switch NOT to fire after rebaseline; got reason=%q", reason)
	}
	if state.PortfolioRisk.KillSwitchActive {
		t.Errorf("expected kill switch inactive after rebaseline; got active")
	}
}

// Post-prune rebaseline must match the deduped per-cycle total when a live
// manual shares a deduped HL wallet (#921 review).
func TestRebaselinePortfolioPeakAfterPrune_MatchesRiskPathTotalWithManual(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200},
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc":    {ID: "hl-btc", Cash: 350, Positions: map[string]*Position{}},
		"hl-eth":    {ID: "hl-eth", Cash: 500, Positions: map[string]*Position{}},
		"hl-manual": {ID: "hl-manual", Cash: 200, Positions: map[string]*Position{}},
	}}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	walletBalances := map[SharedWalletKey]float64{key: 1000}
	fetcher := stubFetcher(walletBalances, nil)
	accountShared := detectSharedWallets(cfg.Strategies[:2])

	rebaseline := rebaselinePortfolioPeakAfterPrune(state, cfg, fetcher)
	totalPV, fb := computeTotalPortfolioValue(cfg.Strategies, state, nil, walletBalances, accountShared)
	if rebaseline != totalPV {
		t.Fatalf("post-prune rebaseline: peak=%.2f totalPV=%.2f, want equal", rebaseline, totalPV)
	}
	if fb {
		t.Fatal("post-prune rebaseline: expected usedFallback=false")
	}

	state.PortfolioRisk.PeakValue = rebaseline
	prsCfg := &PortfolioRiskConfig{MaxDrawdownPct: 20, WarnThresholdPct: 16}
	allowed, _, warning, reason := CheckPortfolioRisk(&state.PortfolioRisk, prsCfg, totalPV, 0, 0, 0)
	if !allowed || warning || state.PortfolioRisk.CurrentDrawdownPct != 0 {
		t.Errorf("flat post-prune: allowed=%v warning=%v dd=%.2f reason=%q", allowed, warning, state.PortfolioRisk.CurrentDrawdownPct, reason)
	}
}

// Without a shared wallet (single perps), live manual capital must still count.
func TestRebaselinePortfolioPeakAfterPrune_SinglePerpsPlusManualSumsManual(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200},
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc":    {ID: "hl-btc"},
		"hl-manual": {ID: "hl-manual"},
	}}

	got := rebaselinePortfolioPeakAfterPrune(state, cfg, nil)
	if got != 700 {
		t.Errorf("single perps + manual: want 700 (500+200 capital), got %.2f", got)
	}
}

// Zero-capital live manual on a deduped wallet is a no-op for the sum.
func TestRebaselinePortfolioPeakAfterPrune_DedupedManualZeroCapitalUnchanged(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 0},
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc":    {ID: "hl-btc"},
		"hl-eth":    {ID: "hl-eth"},
		"hl-manual": {ID: "hl-manual"},
	}}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	fetcher := stubFetcher(map[SharedWalletKey]float64{key: 1000}, nil)

	got := rebaselinePortfolioPeakAfterPrune(state, cfg, fetcher)
	if got != 1000 {
		t.Errorf("zero-capital manual: want 1000, got %.2f", got)
	}
}

// --- computeSubsetPortfolioValue tests (#915) ---

// TestComputeSubsetPortfolioValue_FullyContainedWallet verifies that when all
// shared-wallet members are in the subset, the real balance is used once (no
// double-count) — same result as computeTotalPortfolioValue for the whole account.
func TestComputeSubsetPortfolioValue_FullyContainedWallet(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	allStrategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc": {ID: "hl-btc", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-eth": {ID: "hl-eth", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}
	walletBalances := map[SharedWalletKey]float64{
		{Platform: "hyperliquid", Account: "0xtest"}: 8000, // real balance after fees
	}
	accountShared := detectSharedWallets(allStrategies)

	// Subset = all strategies → fully contained → real balance used once.
	got, fb := computeSubsetPortfolioValue(allStrategies, state, nil, walletBalances, accountShared)
	if got != 8000 {
		t.Errorf("fully-contained subset: want 8000 (real balance), got %.2f", got)
	}
	if fb {
		t.Errorf("fully-contained subset: expected usedFallback=false")
	}
}

// TestComputeSubsetPortfolioValue_StradddlingWalletVirtualSum verifies that when
// a shared wallet has members OUTSIDE the subset (straddle), the subset members
// are virtual-summed rather than deduped — the real balance cannot be split.
func TestComputeSubsetPortfolioValue_StraddlingWalletVirtualSum(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	allStrategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc": {ID: "hl-btc", Cash: 4000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-eth": {ID: "hl-eth", Cash: 6000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}
	walletBalances := map[SharedWalletKey]float64{
		{Platform: "hyperliquid", Account: "0xtest"}: 8000,
	}
	accountShared := detectSharedWallets(allStrategies)

	// Subset contains only one of two wallet members → straddle → virtual sum.
	subset := allStrategies[:1] // just hl-btc
	got, fb := computeSubsetPortfolioValue(subset, state, nil, walletBalances, accountShared)
	if got != 4000 {
		t.Errorf("straddling wallet subset: want 4000 (virtual sum of hl-btc only), got %.2f", got)
	}
	if fb {
		t.Errorf("straddling wallet subset: expected usedFallback=false (no dedup attempted)")
	}
}

// TestComputeSubsetPortfolioValue_MixedSharedAndNonShared verifies the common
// case: a shared-wallet channel plus a non-shared standalone strategy.
func TestComputeSubsetPortfolioValue_MixedSharedAndNonShared(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	allStrategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc":   {ID: "hl-btc", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-eth":   {ID: "hl-eth", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"spot-btc": {ID: "spot-btc", Cash: 2000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}
	walletBalances := map[SharedWalletKey]float64{
		{Platform: "hyperliquid", Account: "0xtest"}: 7500,
	}
	accountShared := detectSharedWallets(allStrategies)

	// Subset = all three → fully-contained wallet + non-shared spot.
	got, fb := computeSubsetPortfolioValue(allStrategies, state, nil, walletBalances, accountShared)
	want := 7500.0 + 2000.0 // real HL balance + spot PV
	if got != want {
		t.Errorf("mixed subset: want %.2f, got %.2f", want, got)
	}
	if fb {
		t.Errorf("mixed subset: expected usedFallback=false")
	}
}

// TestComputeSubsetPortfolioValue_MissingBalance verifies that a missing
// wallet balance triggers usedFallback=true and falls back to virtual sum.
func TestComputeSubsetPortfolioValue_MissingBalance(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	allStrategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc": {ID: "hl-btc", Cash: 4000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-eth": {ID: "hl-eth", Cash: 6000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}
	accountShared := detectSharedWallets(allStrategies)

	// walletBalances empty → fallback to virtual sum.
	got, fb := computeSubsetPortfolioValue(allStrategies, state, nil, nil, accountShared)
	if got != 10000 {
		t.Errorf("missing balance: want 10000 (fallback sum), got %.2f", got)
	}
	if !fb {
		t.Errorf("missing balance: expected usedFallback=true")
	}
}

// TestComputeTotalPortfolioValue_DelegatesCorrectly verifies that the refactored
// computeTotalPortfolioValue (delegating to computeSubsetPortfolioValue) gives
// identical results to the pre-#915 inline implementation for the whole-account case.
func TestComputeTotalPortfolioValue_DelegatesCorrectly(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-btc":   {ID: "hl-btc", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"hl-eth":   {ID: "hl-eth", Cash: 5000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
			"spot-btc": {ID: "spot-btc", Cash: 2000, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}},
		},
	}
	walletBalances := map[SharedWalletKey]float64{
		{Platform: "hyperliquid", Account: "0xtest"}: 9000,
	}

	got, fb := computeTotalPortfolioValue(strategies, state, nil, walletBalances, nil)
	want := 9000.0 + 2000.0 // real HL balance + spot
	if got != want {
		t.Errorf("delegation: want %.2f, got %.2f", want, got)
	}
	if fb {
		t.Errorf("delegation: expected usedFallback=false")
	}
}

// TestComputeInitialPortfolioPeak_SharedWalletManualNoDoubleCount verifies
// peak init uses the real wallet balance once when a same-account live manual
// shares the HL wallet with 2+ perps (#921).
func TestComputeInitialPortfolioPeak_SharedWalletManualNoDoubleCount(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200},
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	fetcher := stubFetcher(map[SharedWalletKey]float64{key: 1000}, nil)

	got := computeInitialPortfolioPeak(strategies, fetcher)
	if got != 1000 {
		t.Errorf("peak init incl. manual: want 1000 (real balance, no double count), got %.2f", got)
	}
}

// Peak init fallback sums perps + manual capital exactly once each.
func TestComputeInitialPortfolioPeak_SharedWalletManualFallback(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200},
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	fetcher := stubFetcher(nil, map[SharedWalletKey]error{key: errors.New("network down")})

	got := computeInitialPortfolioPeak(strategies, fetcher)
	if got != 1200 {
		t.Errorf("peak init manual fallback: want 1200 (sum member capital once), got %.2f", got)
	}
}

// Cold-start peak must match the first-cycle risk-path total so a flat account
// shows 0% equity drawdown on cycle 1 (#921 review).
func TestComputeInitialPortfolioPeak_MatchesRiskPathTotalOnColdStart(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc":    {ID: "hl-btc", Cash: 350, Positions: map[string]*Position{}},
		"hl-eth":    {ID: "hl-eth", Cash: 500, Positions: map[string]*Position{}},
		"hl-manual": {ID: "hl-manual", Cash: 200, Positions: map[string]*Position{}},
	}}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	walletBalances := map[SharedWalletKey]float64{key: 1000}
	fetcher := stubFetcher(walletBalances, nil)
	accountShared := detectSharedWallets(strategies[:2])

	peak := computeInitialPortfolioPeak(strategies, fetcher)
	totalPV, fb := computeTotalPortfolioValue(strategies, state, nil, walletBalances, accountShared)
	if peak != totalPV {
		t.Fatalf("cold start: peak=%.2f totalPV=%.2f, want equal", peak, totalPV)
	}
	if fb {
		t.Fatal("cold start: expected usedFallback=false")
	}

	prs := &PortfolioRiskState{PeakValue: peak}
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 20, WarnThresholdPct: 16}
	allowed, _, warning, reason := CheckPortfolioRisk(prs, cfg, totalPV, 0, 0, 0)
	if !allowed || warning || prs.CurrentDrawdownPct != 0 {
		t.Errorf("flat cold start: allowed=%v warning=%v dd=%.2f reason=%q", allowed, warning, prs.CurrentDrawdownPct, reason)
	}
}

// A same-account live manual strategy is outside detectSharedWallets membership
// but inside the wallet real balance. Risk-path total must not add its modeled
// PV on top of the balance (#921).
func TestComputeTotalPortfolioValue_SharedWalletManualNoDoubleCount(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc":    {ID: "hl-btc", Cash: 350, Positions: map[string]*Position{}},
		"hl-eth":    {ID: "hl-eth", Cash: 500, Positions: map[string]*Position{}},
		"hl-manual": {ID: "hl-manual", Cash: 200, Positions: map[string]*Position{}},
	}}
	walletBalances := map[SharedWalletKey]float64{{Platform: "hyperliquid", Account: "0xtest"}: 1000}
	accountShared := detectSharedWallets(strategies[:2])

	got, fb := computeTotalPortfolioValue(strategies, state, nil, walletBalances, accountShared)
	if got != 1000 {
		t.Errorf("risk path incl. manual: want exactly 1000 (real balance, no double count), got %.2f", got)
	}
	if fb {
		t.Errorf("risk path incl. manual: expected usedFallback=false")
	}
}

// Missing balance: fallback sums perps + manual member PVs once each (no double count).
func TestComputeTotalPortfolioValue_SharedWalletManualFallback(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: 200},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc":    {ID: "hl-btc", Cash: 400, Positions: map[string]*Position{}},
		"hl-eth":    {ID: "hl-eth", Cash: 400, Positions: map[string]*Position{}},
		"hl-manual": {ID: "hl-manual", Cash: 200, Positions: map[string]*Position{}},
	}}
	accountShared := detectSharedWallets(strategies[:2])

	got, fb := computeTotalPortfolioValue(strategies, state, nil, nil, accountShared)
	if got != 1000 {
		t.Errorf("risk path manual fallback: want 1000 (sum member PVs once), got %.2f", got)
	}
	if !fb {
		t.Errorf("risk path manual fallback: expected usedFallback=true")
	}
}

// Paper / record-only same-account manuals carry a separate virtual book and
// must stay in the per-strategy sum — only live manuals are deduped (#921).
func TestComputeTotalPortfolioValue_PaperManualNotDeduped(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	strategies := []StrategyConfig{
		{ID: "hl-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=paper"}, Capital: 200},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-btc":    {ID: "hl-btc", Cash: 350, Positions: map[string]*Position{}},
		"hl-eth":    {ID: "hl-eth", Cash: 500, Positions: map[string]*Position{}},
		"hl-manual": {ID: "hl-manual", Cash: 200, Positions: map[string]*Position{}},
	}}
	walletBalances := map[SharedWalletKey]float64{{Platform: "hyperliquid", Account: "0xtest"}: 1000}
	accountShared := detectSharedWallets(strategies[:2])

	got, fb := computeTotalPortfolioValue(strategies, state, nil, walletBalances, accountShared)
	if got != 1200 {
		t.Errorf("paper manual: want 1200 (balance + manual PV), got %.2f", got)
	}
	if fb {
		t.Errorf("paper manual: expected usedFallback=false")
	}
}
