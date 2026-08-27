package main

import (
	"errors"
	"strings"
	"testing"
)

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

func TestWalletKeyFor_OKX_PaperNoKey(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-key-abc")

	sc := StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
		Args: []string{"sma", "BTC", "1h", "--mode=paper"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for paper-mode OKX")
	}
}

func TestWalletKeyFor_OKX_SpotNoKey(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-key-abc")

	sc := StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "spot",
		Args: []string{"sma", "BTC", "1h", "--mode=live"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for OKX spot (not in registry)")
	}
}

func TestWalletKeyFor_OKX_MissingEnvVar(t *testing.T) {
	t.Setenv("OKX_API_KEY", "")

	sc := StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "perps",
		Args: []string{"sma", "BTC", "1h", "--mode=live"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key when OKX_API_KEY is unset")
	}
}

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

func TestWalletKeyFor_TopStep_PaperNoKey(t *testing.T) {
	t.Setenv("TOPSTEP_ACCOUNT_ID", "ts-account-42")

	sc := StrategyConfig{ID: "ts-sma-es", Platform: "topstep", Type: "futures",
		Args: []string{"sma", "ES", "15m", "--mode=paper"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for paper-mode TopStep")
	}
}

func TestWalletKeyFor_TopStep_MissingEnvVar(t *testing.T) {
	t.Setenv("TOPSTEP_ACCOUNT_ID", "")

	sc := StrategyConfig{ID: "ts-sma-es", Platform: "topstep", Type: "futures",
		Args: []string{"sma", "ES", "15m", "--mode=live"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key when TOPSTEP_ACCOUNT_ID is unset")
	}
}

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

func TestWalletKeyFor_Robinhood_PaperNoKey(t *testing.T) {
	t.Setenv("ROBINHOOD_USERNAME", "rh-user@example.com")

	sc := StrategyConfig{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot",
		Args: []string{"sma", "BTC", "1h", "--mode=paper"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for paper-mode Robinhood")
	}
}

func TestWalletKeyFor_Robinhood_OptionsNoKey(t *testing.T) {
	t.Setenv("ROBINHOOD_USERNAME", "rh-user@example.com")

	sc := StrategyConfig{ID: "rh-ccall-spy", Platform: "robinhood", Type: "options",
		Args: []string{"ccall", "SPY", "1h", "--mode=live"}}

	if _, ok := walletKeyFor(sc); ok {
		t.Errorf("expected no wallet key for Robinhood options (not in registry)")
	}
}

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

func TestDetectSharedWallets_TopStepExcludedNoFetcher(t *testing.T) {
	t.Setenv("TOPSTEP_ACCOUNT_ID", "ts-account-42")

	strategies := []StrategyConfig{
		{ID: "ts-sma-es", Platform: "topstep", Type: "futures", Args: []string{"sma", "ES", "15m", "--mode=live"}, Capital: 5000},
		{ID: "ts-rsi-nq", Platform: "topstep", Type: "futures", Args: []string{"rsi", "NQ", "15m", "--mode=live"}, Capital: 5000},
	}

	shared := detectSharedWallets(strategies)
	if len(shared) != 0 {
		t.Fatalf("expected TopStep to stay excluded from detectSharedWallets during the shadow phase (#1106); got %d entries", len(shared))
	}
	for _, sc := range strategies {
		if _, ok := walletKeyFor(sc); !ok {
			t.Errorf("walletKeyFor should still recognize live %s", sc.ID)
		}
	}
}

func TestDetectTopStepSharedWallet(t *testing.T) {
	t.Setenv("TOPSTEP_ACCOUNT_ID", "ts-account-42")

	twoLive := []StrategyConfig{
		{ID: "ts-sma-es", Platform: "topstep", Type: "futures", Args: []string{"sma", "ES", "15m", "--mode=live"}, Capital: 5000},
		{ID: "ts-rsi-nq", Platform: "topstep", Type: "futures", Args: []string{"rsi", "NQ", "15m", "--mode=live"}, Capital: 5000},
	}
	if key, ok := detectTopStepSharedWallet(twoLive); !ok || key.Account != "ts-account-42" {
		t.Fatalf("expected 2 live TopStep strategies to be a shared wallet, got ok=%v key=%+v", ok, key)
	}

	if len(detectSharedWallets(twoLive)) != 0 {
		t.Errorf("TopStep must stay out of detectSharedWallets (kill-switch path)")
	}

	oneLive := twoLive[:1]
	if _, ok := detectTopStepSharedWallet(oneLive); ok {
		t.Errorf("a single live TopStep strategy is not a shared wallet")
	}

	paper := []StrategyConfig{
		{ID: "ts-sma-es", Platform: "topstep", Type: "futures", Args: []string{"sma", "ES", "15m", "--mode=paper"}, Capital: 5000},
		{ID: "ts-rsi-nq", Platform: "topstep", Type: "futures", Args: []string{"rsi", "NQ", "15m", "--mode=paper"}, Capital: 5000},
	}
	if _, ok := detectTopStepSharedWallet(paper); ok {
		t.Errorf("paper-mode TopStep strategies must not form a shared wallet")
	}
}

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

func TestHasSharedWalletBalanceFetcher_HLAndOKX(t *testing.T) {
	cases := map[string]bool{
		"hyperliquid": true,
		"okx":         true,
		"topstep":     false,
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
	want := 5000.0
	if got != want {
		t.Errorf("expected total=%v (real wallet balance); got %v (likely double-counted)", want, got)
	}
	if usedFallback {
		t.Errorf("expected usedFallback=false when balance was provided")
	}
}

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

	got, usedFallback := computeTotalPortfolioValue(strategies, state, nil, nil, nil)
	want := 10000.0
	if got != want {
		t.Errorf("expected fallback total=%v (sum of members); got %v", want, got)
	}
	if !usedFallback {
		t.Errorf("expected usedFallback=true on fetch failure so caller can freeze peak")
	}
}

func TestComputeTotalPortfolioValue_FallbackKeepsPeakFreezeSignal(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}

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
		{Platform: "hyperliquid", Account: "0xtest"}: 7500,
	}

	got, usedFallback := computeTotalPortfolioValue(strategies, state, nil, walletBalances, nil)
	want := 9500.0
	if got != want {
		t.Errorf("expected mixed total=%v; got %v", want, got)
	}
	if usedFallback {
		t.Errorf("expected usedFallback=false when balance was provided")
	}
}

func TestComputeTotalPortfolioValue_MixedPaperAndLiveHL(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-paper-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 5000},
		{ID: "hl-live-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}

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
	want := 9500.0
	if got != want {
		t.Errorf("expected mixed paper+live total=%v; got %v", want, got)
	}
	if usedFallback {
		t.Errorf("expected usedFallback=false; nothing was classified as shared")
	}
}

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
	want := 10000.0
	if got != want {
		t.Errorf("expected peak=%v; got %v", want, got)
	}
}

func TestComputeInitialPortfolioPeak_FallbackOnFetchError(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")

	strategies := []StrategyConfig{
		{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	fetcher := stubFetcher(nil, map[SharedWalletKey]error{key: errors.New("network down")})

	got := computeInitialPortfolioPeak(strategies, fetcher)
	want := 10000.0
	if got != want {
		t.Errorf("expected fallback peak=%v; got %v", want, got)
	}
}

func TestComputeInitialPortfolioPeak_LegacyCapitalPct(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	strategies := []StrategyConfig{
		{ID: "binance-spot", Platform: "binanceus", Type: "spot", Capital: 2500, CapitalPct: 0.5},
		{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 1000},
	}

	got := computeInitialPortfolioPeak(strategies, nil)
	want := 6000.0
	if got != want {
		t.Errorf("expected legacy capital_pct peak=%v; got %v", want, got)
	}
}

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

func TestRebaselinePortfolioPeakAfterPrune_FloorAtCapitalSum(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 5000},
		{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 3000},
	}}
	state := &AppState{Strategies: map[string]*StrategyState{

		"spot-btc": {ID: "spot-btc"},
		"spot-eth": {ID: "spot-eth"},
	}}

	got := rebaselinePortfolioPeakAfterPrune(state, cfg, nil)
	want := 8000.0
	if got != want {
		t.Errorf("expected floored peak=%v; got %v", want, got)
	}
}

func TestRebaselinePortfolioPeakAfterPrune_FallbackToCapitalWhenPeakMissing(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 5000},
		{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 3000},
	}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"spot-btc": {ID: "spot-btc", RiskState: RiskState{PeakValue: 6000}},
		"spot-eth": {ID: "spot-eth"},
	}}

	got := rebaselinePortfolioPeakAfterPrune(state, cfg, nil)
	want := 9000.0
	if got != want {
		t.Errorf("expected mixed peak=%v; got %v", want, got)
	}
}

func TestRebaselinePortfolioPeakAfterPrune_PreventsImmediateKillSwitch(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 5000},
	}}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"spot-btc": {ID: "spot-btc", RiskState: RiskState{PeakValue: 9034.24}},
		},
		PortfolioRisk: PortfolioRiskState{PeakValue: 15148.90},
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
		{Platform: "hyperliquid", Account: "0xtest"}: 8000,
	}
	accountShared := detectSharedWallets(allStrategies)

	got, fb := computeSubsetPortfolioValue(allStrategies, state, nil, walletBalances, accountShared)
	if got != 8000 {
		t.Errorf("fully-contained subset: want 8000 (real balance), got %.2f", got)
	}
	if fb {
		t.Errorf("fully-contained subset: expected usedFallback=false")
	}
}

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

	subset := allStrategies[:1]
	got, fb := computeSubsetPortfolioValue(subset, state, nil, walletBalances, accountShared)
	if got != 4000 {
		t.Errorf("straddling wallet subset: want 4000 (virtual sum of hl-btc only), got %.2f", got)
	}
	if fb {
		t.Errorf("straddling wallet subset: expected usedFallback=false (no dedup attempted)")
	}
}

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

	got, fb := computeSubsetPortfolioValue(allStrategies, state, nil, walletBalances, accountShared)
	want := 7500.0 + 2000.0
	if got != want {
		t.Errorf("mixed subset: want %.2f, got %.2f", want, got)
	}
	if fb {
		t.Errorf("mixed subset: expected usedFallback=false")
	}
}

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

	got, fb := computeSubsetPortfolioValue(allStrategies, state, nil, nil, accountShared)
	if got != 10000 {
		t.Errorf("missing balance: want 10000 (fallback sum), got %.2f", got)
	}
	if !fb {
		t.Errorf("missing balance: expected usedFallback=true")
	}
}

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
	want := 9000.0 + 2000.0
	if got != want {
		t.Errorf("delegation: want %.2f, got %.2f", want, got)
	}
	if fb {
		t.Errorf("delegation: expected usedFallback=false")
	}
}

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

func TestSharedWalletPoolAvailableMarginReservesAllAccountPositions(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xpool")
	marginCap := 100.0
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Leverage: 5, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}, Leverage: 10, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
		{ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Leverage: 2},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {Positions: map[string]*Position{
			"BTC": {Quantity: 1, AvgCost: 100, Leverage: 5},
		}},
		"hl-b": {Positions: map[string]*Position{
			"ETH": {Quantity: 2, AvgCost: 200, Leverage: 10},
		}},
		"hl-manual": {Positions: map[string]*Position{
			"SOL": {Quantity: 1, AvgCost: 50, Leverage: 2},
		}},
	}}
	shared := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xpool"}
	available, pooled, balanceKnown := sharedWalletPoolAvailableMargin(
		strategies[0], strategies, state,
		map[string]float64{"BTC": 100, "ETH": 200, "SOL": 50},
		shared, map[SharedWalletKey]float64{key: 1000},
	)

	if !pooled || !balanceKnown || available != 915 {
		t.Fatalf("available=%v pooled=%v known=%v, want 915/true/true", available, pooled, balanceKnown)
	}

	available, pooled, balanceKnown = sharedWalletPoolAvailableMargin(
		strategies[0], strategies, state, nil, shared,
		map[SharedWalletKey]float64{key: 85},
	)
	if !pooled || !balanceKnown || available != 0 {
		t.Fatalf("fully deployed wallet must remain known: available=%v pooled=%v known=%v", available, pooled, balanceKnown)
	}

	available, pooled, balanceKnown = sharedWalletPoolAvailableMargin(
		strategies[0], strategies, state, nil, shared, nil,
	)
	if !pooled || balanceKnown || available != 0 {
		t.Fatalf("missing balance must fail closed: available=%v pooled=%v known=%v", available, pooled, balanceKnown)
	}
}

func TestSharedWalletPoolAvailableMarginMissingIdentityFailsClosed(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")
	sc := StrategyConfig{
		ID: "hl-a", Platform: "hyperliquid", Type: "perps",
		Args:                   []string{"sma", "BTC", "1h", "--mode=live"},
		sharedWalletPoolBudget: true,
	}
	available, pooled, balanceKnown := sharedWalletPoolAvailableMargin(
		sc, []StrategyConfig{sc}, NewAppState(), nil, nil, nil,
	)
	if available != 0 || !pooled || balanceKnown {
		t.Fatalf("missing identity must fail closed: available=%v pooled=%v known=%v", available, pooled, balanceKnown)
	}
}

func TestSharedWalletPoolAvailableMarginReservesUnderwaterEntryMargin(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xpool")
	marginCap := 100.0
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Leverage: 5, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"tema", "ETH", "1h", "--mode=live"}, Leverage: 10, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {Positions: map[string]*Position{
			"BTC": {Quantity: 1, AvgCost: 100, Leverage: 5},
		}},
		"hl-b": {Positions: map[string]*Position{
			"ETH": {Quantity: 2, AvgCost: 200, Leverage: 10},
		}},
	}}
	shared := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xpool"}
	available, pooled, balanceKnown := sharedWalletPoolAvailableMargin(
		strategies[0], strategies, state,
		map[string]float64{"BTC": 70, "ETH": 250},
		shared, map[SharedWalletKey]float64{key: 1000},
	)

	if !pooled || !balanceKnown || available != 930 {
		t.Fatalf("available=%v pooled=%v known=%v, want 930/true/true", available, pooled, balanceKnown)
	}
}

func TestResolveSharedWalletRiskBalancesUsesOnlyPriorRiskGeneration(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xpool")
	marginCap := 100.0
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
	}
	shared := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xpool"}
	cache := make(map[SharedWalletKey]sharedWalletRiskBalanceSnapshot)

	current := map[SharedWalletKey]float64{key: 10000}
	resolved, stale, complete := resolveSharedWalletRiskBalances(strategies, nil, shared, current, cache, 1)
	if stale || !complete || resolved[key] != 10000 {
		t.Fatalf("fresh snapshot: resolved=%v stale=%v complete=%v", resolved, stale, complete)
	}

	resolved, stale, complete = resolveSharedWalletRiskBalances(strategies, nil, shared, nil, cache, 2)
	if !stale || !complete || resolved[key] != 10000 {
		t.Fatalf("single fetch miss must use prior risk generation: resolved=%v stale=%v complete=%v", resolved, stale, complete)
	}

	resolved, stale, complete = resolveSharedWalletRiskBalances(strategies, nil, shared, nil, cache, 3)
	if stale || complete {
		t.Fatalf("second consecutive miss must expire snapshot: resolved=%v stale=%v complete=%v", resolved, stale, complete)
	}
}

func TestResolveSharedWalletRiskBalancesProtectsDeferredPoolExit(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xpool")
	strategies := []StrategyConfig{
		{
			ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}, CapitalPct: 50,
		},
		{
			ID: "hl-b", Platform: "hyperliquid", Type: "perps",
			Args: []string{"rsi", "ETH", "1h", "--mode=live"}, CapitalPct: 50,
		},
	}
	states := map[string]*StrategyState{
		"hl-a": {SharedWalletPoolBudget: true},
		"hl-b": {SharedWalletPoolBudget: true},
	}
	shared := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xpool"}
	cache := make(map[SharedWalletKey]sharedWalletRiskBalanceSnapshot)

	resolved, stale, complete := resolveSharedWalletRiskBalances(
		strategies, states, shared, nil, cache, 1)
	if stale || complete || len(resolved) != 0 {
		t.Fatalf("deferred exit miss: resolved=%v stale=%v complete=%v", resolved, stale, complete)
	}

	deferredStrategies := append([]StrategyConfig(nil), strategies...)
	deferredStrategies[0].sharedWalletModeDeferred = true
	_, stale, complete = resolveSharedWalletRiskBalances(
		deferredStrategies, nil, shared, nil, nil, 1)
	if stale || complete {
		t.Fatalf("process-local deferred exit must suppress equity: stale=%v complete=%v", stale, complete)
	}

	resolved, stale, complete = resolveSharedWalletRiskBalances(
		strategies, states, shared, map[SharedWalletKey]float64{key: 8000}, cache, 2)
	if stale || !complete || resolved[key] != 8000 {
		t.Fatalf("deferred exit recovery: resolved=%v stale=%v complete=%v", resolved, stale, complete)
	}
}

func TestResolveSharedWalletRiskBalancesCompletedPoolExitUsesAllocatedFallback(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xpool")
	strategies := []StrategyConfig{
		{
			ID: "hl-a", Platform: "hyperliquid", Type: "perps",
			Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 500,
		},
		{
			ID: "hl-b", Platform: "hyperliquid", Type: "perps",
			Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 500,
		},
	}
	states := map[string]*StrategyState{
		"hl-a": {Cash: 500, InitialCapital: 500},
		"hl-b": {Cash: 500, InitialCapital: 500},
	}

	_, stale, complete := resolveSharedWalletRiskBalances(
		strategies, states, detectSharedWallets(strategies), nil, nil, 1)
	if stale || !complete {
		t.Fatalf("completed exit must use allocated fallback: stale=%v complete=%v", stale, complete)
	}
}

func TestPooledRiskFallbackPreservesMixedAllocatedDrawdown(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xpool")
	marginCap := 100.0
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
		{ID: "hl-b", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true},
		{ID: "spot", Platform: "binanceus", Type: "spot", Capital: 2000},
	}
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-a": {Cash: 0, Positions: map[string]*Position{}},
		"hl-b": {Cash: 0, Positions: map[string]*Position{}},
		"spot": {Cash: 1000, Positions: map[string]*Position{}},
	}}
	shared := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xpool"}
	cache := make(map[SharedWalletKey]sharedWalletRiskBalanceSnapshot)
	_, _, _ = resolveSharedWalletRiskBalances(
		strategies, state.Strategies, shared, map[SharedWalletKey]float64{key: 10000}, cache, 1)

	riskBalances, stale, complete := resolveSharedWalletRiskBalances(
		strategies, state.Strategies, shared, nil, cache, 2)
	if !stale || !complete {
		t.Fatalf("single pooled fetch miss must retain complete equity: stale=%v complete=%v", stale, complete)
	}
	total, fallback := computeTotalPortfolioValue(strategies, state, nil, riskBalances, shared)
	if total != 11000 || fallback {
		t.Fatalf("mixed total=%v fallback=%v, want 11000/false", total, fallback)
	}
	prs := &PortfolioRiskState{PeakValue: 12000}
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 5, WarnThresholdPct: 80}
	allowed, _, _, reason := checkPortfolioRiskWithEquityAvailability(prs, cfg, total, 0, 0, 0, complete, complete)
	if allowed || !prs.KillSwitchActive || !strings.Contains(reason, "portfolio drawdown") {
		t.Fatalf("allocated strategy drawdown must remain detectable: allowed=%v state=%+v reason=%q", allowed, prs, reason)
	}
}

func TestSharedWalletPoolFlipReleaseMatchesStoredLeverageReservation(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-pool")
	marginCap := 500.0
	sc := StrategyConfig{
		ID: "okx-a", Platform: "okx", Type: "perps",
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
		Leverage: 5, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true,
	}
	peer := sc
	peer.ID = "okx-b"
	peer.Args = []string{"rsi", "ETH", "1h", "--mode=live"}
	strategies := []StrategyConfig{sc, peer}
	state := &AppState{Strategies: map[string]*StrategyState{
		"okx-a": {Positions: map[string]*Position{
			"BTC": {Quantity: 1, AvgCost: 100, Leverage: 10},
		}},
		"okx-b": {Positions: map[string]*Position{}},
	}}
	shared := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "okx", Account: "okx-pool"}
	available, pooled, known := sharedWalletPoolAvailableMargin(
		sc, strategies, state, map[string]float64{"BTC": 100}, shared,
		map[SharedWalletKey]float64{key: 1000},
	)
	if !pooled || !known || available != 990 {
		t.Fatalf("reservation: available=%v pooled=%v known=%v, want 990/true/true", available, pooled, known)
	}
	sizing := withSharedWalletPoolSizing(sc, PerpsSizingFor(sc, 100, 0), 1, 100, 100, 10, true)
	if sizing.ReleasableMarginUSD != 10 || available+sizing.ReleasableMarginUSD != 1000 {
		t.Fatalf("release must exactly cancel stored-leverage reservation: available=%v sizing=%+v", available, sizing)
	}
}

func TestSharedWalletPoolFlipPreservesSignedAccountHeadroom(t *testing.T) {
	t.Setenv("OKX_API_KEY", "okx-pool")
	marginCap := 100.0
	sc := StrategyConfig{
		ID: "okx-a", Platform: "okx", Type: "perps",
		Args:     []string{"sma", "BTC", "1h", "--mode=live"},
		Leverage: 1, MarginPerTradeUSD: &marginCap, sharedWalletPoolBudget: true,
	}
	peer := sc
	peer.ID = "okx-b"
	peer.Args = []string{"rsi", "ETH", "1h", "--mode=live"}
	strategies := []StrategyConfig{sc, peer}
	shared := detectSharedWallets(strategies)
	key := SharedWalletKey{Platform: "okx", Account: "okx-pool"}

	tests := []struct {
		name          string
		flippingQty   float64
		peerQty       float64
		wantAvailable float64
		wantSize      float64
	}{
		{
			name: "deficit remains after release and closes only",

			flippingQty: 6, peerQty: 12, wantAvailable: -80, wantSize: 6,
		},
		{
			name: "deficit before release sizes within true remainder",

			flippingQty: 6, peerQty: 6, wantAvailable: -20, wantSize: 10,
		},
		{
			name: "only position can reuse full equity",

			flippingQty: 6, wantAvailable: 40, wantSize: 16,
		},
		{
			name: "healthy wallet keeps existing sizing",

			flippingQty: 2, peerQty: 2, wantAvailable: 60, wantSize: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &AppState{Strategies: map[string]*StrategyState{
				"okx-a": {Positions: map[string]*Position{
					"BTC": {Quantity: tt.flippingQty, AvgCost: 10, Leverage: 1, Side: "long"},
				}},
				"okx-b": {Positions: map[string]*Position{}},
			}}
			if tt.peerQty > 0 {
				state.Strategies["okx-b"].Positions["ETH"] = &Position{
					Quantity: tt.peerQty, AvgCost: 10, Leverage: 1, Side: "long",
				}
			}
			prices := map[string]float64{"BTC": 10, "ETH": 10}
			available, pooled, known := sharedWalletPoolAvailableMargin(
				sc, strategies, state, prices, shared,
				map[SharedWalletKey]float64{key: 100},
			)
			if !pooled || !known || available != tt.wantAvailable {
				t.Fatalf("available=%v pooled=%v known=%v, want %v/true/true",
					available, pooled, known, tt.wantAvailable)
			}
			sizing := withSharedWalletPoolSizing(
				sc, PerpsSizingFor(sc, 10, 0), tt.flippingQty, 10, 10, 1, true,
			)
			size, ok, reason := perpsLiveOrderSize(
				-1, 10, available, tt.flippingQty, 10, sizing, "long", DirectionBoth, 0,
			)
			if !ok || reason != "" {
				t.Fatalf("flip sizing failed: ok=%v reason=%q", ok, reason)
			}
			if size != tt.wantSize {
				t.Fatalf("flip size=%v, want %v", size, tt.wantSize)
			}
			if available <= 0 && PerpsOpenNotionalSized(available, 10, sizing) != 0 {
				t.Fatal("fresh opens/adds must still clamp signed non-positive headroom to zero notional")
			}
		})
	}
}
