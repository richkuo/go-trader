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

func hlLivePerps(id, symbol string, capital float64) StrategyConfig {
	return StrategyConfig{ID: id, Platform: "hyperliquid", Type: "perps", Args: []string{"sma", symbol, "1h", "--mode=live"}, Capital: capital}
}

func hlLiveManual(id string, capital float64) StrategyConfig {
	return StrategyConfig{ID: id, Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=live"}, Capital: capital}
}

func flatCashState(cash map[string]float64) *AppState {
	state := &AppState{Strategies: map[string]*StrategyState{}}
	for id, c := range cash {
		state.Strategies[id] = &StrategyState{ID: id, Cash: c, Positions: map[string]*Position{}, OptionPositions: map[string]*OptionPosition{}}
	}
	return state
}

func TestWalletKeyFor(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		envVal string
		sc     StrategyConfig
		wantOK bool
		want   SharedWalletKey
	}{
		{"OKX perps live", "OKX_API_KEY", "okx-key-abc",
			StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
			true, SharedWalletKey{Platform: "okx", Account: "okx-key-abc"}},
		{"OKX paper has no key", "OKX_API_KEY", "okx-key-abc",
			StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}},
			false, SharedWalletKey{}},
		{"OKX spot not in registry", "OKX_API_KEY", "okx-key-abc",
			StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "spot", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
			false, SharedWalletKey{}},
		{"OKX missing env var", "OKX_API_KEY", "",
			StrategyConfig{ID: "okx-sma-btc", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
			false, SharedWalletKey{}},
		{"TopStep futures live", "TOPSTEP_ACCOUNT_ID", "ts-account-42",
			StrategyConfig{ID: "ts-sma-es", Platform: "topstep", Type: "futures", Args: []string{"sma", "ES", "15m", "--mode=live"}},
			true, SharedWalletKey{Platform: "topstep", Account: "ts-account-42"}},
		{"TopStep paper has no key", "TOPSTEP_ACCOUNT_ID", "ts-account-42",
			StrategyConfig{ID: "ts-sma-es", Platform: "topstep", Type: "futures", Args: []string{"sma", "ES", "15m", "--mode=paper"}},
			false, SharedWalletKey{}},
		{"TopStep missing env var", "TOPSTEP_ACCOUNT_ID", "",
			StrategyConfig{ID: "ts-sma-es", Platform: "topstep", Type: "futures", Args: []string{"sma", "ES", "15m", "--mode=live"}},
			false, SharedWalletKey{}},
		{"Robinhood crypto live", "ROBINHOOD_USERNAME", "rh-user@example.com",
			StrategyConfig{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot", Args: []string{"sma", "BTC", "1h", "--mode=live"}},
			true, SharedWalletKey{Platform: "robinhood", Account: "rh-user@example.com"}},
		{"Robinhood paper has no key", "ROBINHOOD_USERNAME", "rh-user@example.com",
			StrategyConfig{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot", Args: []string{"sma", "BTC", "1h", "--mode=paper"}},
			false, SharedWalletKey{}},
		{"Robinhood options not in registry", "ROBINHOOD_USERNAME", "rh-user@example.com",
			StrategyConfig{ID: "rh-ccall-spy", Platform: "robinhood", Type: "options", Args: []string{"ccall", "SPY", "1h", "--mode=live"}},
			false, SharedWalletKey{}},
		{"HL split-form --mode live recognized", "HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest",
			StrategyConfig{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode", "live"}},
			true, SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.envVal)
			key, ok := walletKeyFor(tc.sc)
			if ok != tc.wantOK {
				t.Fatalf("walletKeyFor ok=%v key=%+v, want ok=%v", ok, key, tc.wantOK)
			}
			if ok && key != tc.want {
				t.Errorf("unexpected key %+v, want %+v", key, tc.want)
			}
		})
	}
}

func TestDetectSharedWallets(t *testing.T) {
	hlPair := []StrategyConfig{hlLivePerps("hl-sma-btc", "BTC", 5000), hlLivePerps("hl-rsi-eth", "ETH", 5000)}
	okxPair := []StrategyConfig{
		{ID: "okx-sma-btc", Platform: "okx", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
		{ID: "okx-rsi-eth", Platform: "okx", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
	}
	cases := []struct {
		name          string
		env           map[string]string
		strategies    []StrategyConfig
		want          map[SharedWalletKey]int
		wantKeyForAll bool
	}{
		{"two live HL perps share one wallet", map[string]string{"HYPERLIQUID_ACCOUNT_ADDRESS": "0xtest"}, hlPair,
			map[SharedWalletKey]int{{Platform: "hyperliquid", Account: "0xtest"}: 2}, false},
		{"paper HL ignored", map[string]string{"HYPERLIQUID_ACCOUNT_ADDRESS": "0xtest"}, []StrategyConfig{
			{ID: "hl-sma-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 5000},
			{ID: "hl-rsi-eth", Platform: "hyperliquid", Type: "perps", Args: []string{"rsi", "ETH", "1h", "--mode=paper"}, Capital: 5000},
		}, nil, false},
		{"single live HL not shared", map[string]string{"HYPERLIQUID_ACCOUNT_ADDRESS": "0xtest"}, hlPair[:1], nil, false},
		{"mixed paper and live HL not shared", map[string]string{"HYPERLIQUID_ACCOUNT_ADDRESS": "0xtest"}, []StrategyConfig{
			{ID: "hl-paper-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 5000},
			hlLivePerps("hl-live-eth", "ETH", 5000),
		}, nil, false},
		{"no HL env var", map[string]string{"HYPERLIQUID_ACCOUNT_ADDRESS": ""}, hlPair, nil, false},
		{"OKX grouped as one wallet (#360)", map[string]string{"OKX_API_KEY": "okx-key-abc"}, okxPair,
			map[SharedWalletKey]int{{Platform: "okx", Account: "okx-key-abc"}: 2}, true},
		{"TopStep excluded without balance fetcher, key still recognized (#1106)", map[string]string{"TOPSTEP_ACCOUNT_ID": "ts-account-42"}, []StrategyConfig{
			{ID: "ts-sma-es", Platform: "topstep", Type: "futures", Args: []string{"sma", "ES", "15m", "--mode=live"}, Capital: 5000},
			{ID: "ts-rsi-nq", Platform: "topstep", Type: "futures", Args: []string{"rsi", "NQ", "15m", "--mode=live"}, Capital: 5000},
		}, nil, true},
		{"Robinhood excluded without balance fetcher", map[string]string{"ROBINHOOD_USERNAME": "rh-user@example.com"}, []StrategyConfig{
			{ID: "rh-sma-btc", Platform: "robinhood", Type: "spot", Args: []string{"sma", "BTC", "1h", "--mode=live"}, Capital: 5000},
			{ID: "rh-rsi-eth", Platform: "robinhood", Type: "spot", Args: []string{"rsi", "ETH", "1h", "--mode=live"}, Capital: 5000},
		}, nil, false},
		{"HL and OKX form two wallets", map[string]string{"HYPERLIQUID_ACCOUNT_ADDRESS": "0xhl", "OKX_API_KEY": "okx-key-abc"},
			append(append([]StrategyConfig{}, hlPair...), okxPair...),
			map[SharedWalletKey]int{
				{Platform: "hyperliquid", Account: "0xhl"}: 2,
				{Platform: "okx", Account: "okx-key-abc"}:  2,
			}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			shared := detectSharedWallets(tc.strategies)
			if len(shared) != len(tc.want) {
				t.Fatalf("expected %d shared wallets; got %d: %+v", len(tc.want), len(shared), shared)
			}
			for key, n := range tc.want {
				if ids, ok := shared[key]; !ok || len(ids) != n {
					t.Errorf("wallet %+v: ok=%v ids=%v, want %d members", key, ok, ids, n)
				}
			}
			if tc.wantKeyForAll {
				for _, sc := range tc.strategies {
					if _, ok := walletKeyFor(sc); !ok {
						t.Errorf("walletKeyFor should recognize %s", sc.ID)
					}
				}
			}
		})
	}
}

func TestComputeTotalPortfolioValue(t *testing.T) {
	hlKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	hlPair := []StrategyConfig{hlLivePerps("hl-sma-btc", "BTC", 5000), hlLivePerps("hl-rsi-eth", "ETH", 5000)}
	withManual := []StrategyConfig{hlLivePerps("hl-btc", "BTC", 500), hlLivePerps("hl-eth", "ETH", 500), hlLiveManual("hl-manual", 200)}
	paperManual := append(append([]StrategyConfig{}, withManual[:2]...), StrategyConfig{
		ID: "hl-manual", Platform: "hyperliquid", Type: "manual", Symbol: "SOL", Args: []string{"hold", "SOL", "1h", "--mode=paper"}, Capital: 200,
	})
	cases := []struct {
		name         string
		env          string
		strategies   []StrategyConfig
		cash         map[string]float64
		balances     map[SharedWalletKey]float64
		sharedFrom   int
		want         float64
		wantFallback bool
	}{
		{"shared wallet uses real balance (no double count)", "0xtest", hlPair,
			map[string]float64{"hl-sma-btc": 5000, "hl-rsi-eth": 5000},
			map[SharedWalletKey]float64{hlKey: 5000}, 0, 5000, false},
		{"fetch failure falls back to sum of member PVs and signals peak freeze", "0xtest", hlPair,
			map[string]float64{"hl-sma-btc": 4000, "hl-rsi-eth": 6000},
			nil, 0, 10000, true},
		{"mixed shared and non-shared adds real balance to spot PV", "0xtest",
			append(append([]StrategyConfig{}, hlPair...), StrategyConfig{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000}),
			map[string]float64{"hl-sma-btc": 5000, "hl-rsi-eth": 5000, "spot-btc": 2000},
			map[SharedWalletKey]float64{hlKey: 7500}, 0, 9500, false},
		{"mixed paper and live HL sums PVs, nothing shared", "0xtest", []StrategyConfig{
			{ID: "hl-paper-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "BTC", "1h", "--mode=paper"}, Capital: 5000},
			hlLivePerps("hl-live-eth", "ETH", 5000),
		}, map[string]float64{"hl-paper-btc": 5000, "hl-live-eth": 4500}, nil, 0, 9500, false},
		{"no shared wallets behaves like old sum", "", []StrategyConfig{
			{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000},
			{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 3000},
		}, map[string]float64{"spot-btc": 2000, "spot-eth": 3000}, nil, 0, 5000, false},
		{"live manual member counts the real balance exactly once", "0xtest", withManual,
			map[string]float64{"hl-btc": 350, "hl-eth": 500, "hl-manual": 200},
			map[SharedWalletKey]float64{hlKey: 1000}, 2, 1000, false},
		{"live manual member fallback sums member PVs once", "0xtest", withManual,
			map[string]float64{"hl-btc": 400, "hl-eth": 400, "hl-manual": 200},
			nil, 2, 1000, true},
		{"paper manual is not deduped against the wallet", "0xtest", paperManual,
			map[string]float64{"hl-btc": 350, "hl-eth": 500, "hl-manual": 200},
			map[SharedWalletKey]float64{hlKey: 1000}, 2, 1200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", tc.env)
			var accountShared map[SharedWalletKey][]string
			if tc.sharedFrom > 0 {
				accountShared = detectSharedWallets(tc.strategies[:tc.sharedFrom])
			}
			got, usedFallback := computeTotalPortfolioValue(tc.strategies, flatCashState(tc.cash), nil, tc.balances, accountShared)
			if got != tc.want {
				t.Errorf("total=%v, want %v", got, tc.want)
			}
			if usedFallback != tc.wantFallback {
				t.Errorf("usedFallback=%v, want %v", usedFallback, tc.wantFallback)
			}
		})
	}
}

func TestFetchSharedWalletBalances(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
	strategies := []StrategyConfig{hlLivePerps("hl-sma-btc", "BTC", 5000), hlLivePerps("hl-rsi-eth", "ETH", 5000)}
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}

	t.Run("stub returns balance", func(t *testing.T) {
		balances, errs := fetchSharedWalletBalances(strategies, stubFetcher(map[SharedWalletKey]float64{key: 7777}, nil))
		if len(errs) != 0 {
			t.Errorf("expected no errors; got %v", errs)
		}
		if balances[key] != 7777 {
			t.Errorf("expected balance=7777; got %v", balances[key])
		}
	})
	t.Run("records errors and omits balance", func(t *testing.T) {
		balances, errs := fetchSharedWalletBalances(strategies, stubFetcher(nil, map[SharedWalletKey]error{key: errors.New("boom")}))
		if len(balances) != 0 {
			t.Errorf("expected no balances on error; got %v", balances)
		}
		if errs[key] == nil {
			t.Errorf("expected recorded error for key %+v", key)
		}
	})
}

func TestComputeInitialPortfolioPeak(t *testing.T) {
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	hlPair := []StrategyConfig{hlLivePerps("hl-sma-btc", "BTC", 5000), hlLivePerps("hl-rsi-eth", "ETH", 5000)}
	withManual := []StrategyConfig{hlLivePerps("hl-btc", "BTC", 500), hlLivePerps("hl-eth", "ETH", 500), hlLiveManual("hl-manual", 200)}
	cases := []struct {
		name       string
		env        string
		strategies []StrategyConfig
		fetcher    WalletBalanceFetcher
		want       float64
	}{
		{"shared wallet uses balance plus non-shared capital", "0xtest",
			append(append([]StrategyConfig{}, hlPair...), StrategyConfig{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000}),
			stubFetcher(map[SharedWalletKey]float64{key: 8000}, nil), 10000},
		{"fetch error falls back to member capital", "0xtest", hlPair,
			stubFetcher(nil, map[SharedWalletKey]error{key: errors.New("network down")}), 10000},
		{"legacy capital_pct", "", []StrategyConfig{
			{ID: "binance-spot", Platform: "binanceus", Type: "spot", Capital: 2500, CapitalPct: 0.5},
			{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 1000},
		}, nil, 6000},
		{"no shared wallets sums capital", "", []StrategyConfig{
			{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000},
			{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 3000},
		}, nil, 5000},
		{"live manual member: real balance counted once", "0xtest", withManual,
			stubFetcher(map[SharedWalletKey]float64{key: 1000}, nil), 1000},
		{"live manual member fallback sums member capital once", "0xtest", withManual,
			stubFetcher(nil, map[SharedWalletKey]error{key: errors.New("network down")}), 1200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", tc.env)
			if got := computeInitialPortfolioPeak(tc.strategies, tc.fetcher); got != tc.want {
				t.Errorf("peak=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestRebaselinePortfolioPeakAfterPrune(t *testing.T) {
	key := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	spotBTC := StrategyConfig{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 5000}
	spotETH := StrategyConfig{ID: "spot-eth", Platform: "binanceus", Type: "spot", Capital: 3000}
	cases := []struct {
		name       string
		env        string
		strategies []StrategyConfig
		peaks      map[string]float64
		fetcher    WalletBalanceFetcher
		want       float64
	}{
		{"sums remaining per-strategy peaks", "", []StrategyConfig{spotBTC}, map[string]float64{"spot-btc": 7000}, nil, 7000},
		{"floors at capital sum when peaks missing", "", []StrategyConfig{spotBTC, spotETH}, map[string]float64{"spot-btc": 0, "spot-eth": 0}, nil, 8000},
		{"mixed peak and capital fallback", "", []StrategyConfig{spotBTC, spotETH}, map[string]float64{"spot-btc": 6000, "spot-eth": 0}, nil, 9000},
		{"single perps plus manual sums capital", "0xtest",
			[]StrategyConfig{hlLivePerps("hl-btc", "BTC", 500), hlLiveManual("hl-manual", 200)},
			map[string]float64{"hl-btc": 0, "hl-manual": 0}, nil, 700},
		{"deduped zero-capital manual leaves balance unchanged", "0xtest",
			[]StrategyConfig{hlLivePerps("hl-btc", "BTC", 500), hlLivePerps("hl-eth", "ETH", 500), hlLiveManual("hl-manual", 0)},
			map[string]float64{"hl-btc": 0, "hl-eth": 0, "hl-manual": 0},
			stubFetcher(map[SharedWalletKey]float64{key: 1000}, nil), 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", tc.env)
			state := &AppState{Strategies: map[string]*StrategyState{}}
			for id, peak := range tc.peaks {
				state.Strategies[id] = &StrategyState{ID: id, RiskState: RiskState{PeakValue: peak}}
			}
			got := rebaselinePortfolioPeakAfterPrune(state, &Config{Strategies: tc.strategies}, tc.fetcher)
			if got != tc.want {
				t.Errorf("rebaselined peak=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestComputeSubsetPortfolioValue(t *testing.T) {
	hlKey := SharedWalletKey{Platform: "hyperliquid", Account: "0xtest"}
	hlPair := []StrategyConfig{hlLivePerps("hl-btc", "BTC", 5000), hlLivePerps("hl-eth", "ETH", 5000)}
	cases := []struct {
		name         string
		strategies   []StrategyConfig
		cash         map[string]float64
		balances     map[SharedWalletKey]float64
		subsetN      int
		want         float64
		wantFallback bool
	}{
		{"fully contained wallet uses real balance", hlPair,
			map[string]float64{"hl-btc": 5000, "hl-eth": 5000}, map[SharedWalletKey]float64{hlKey: 8000}, 2, 8000, false},
		{"straddling wallet sums virtual PV of the subset only", hlPair,
			map[string]float64{"hl-btc": 4000, "hl-eth": 6000}, map[SharedWalletKey]float64{hlKey: 8000}, 1, 4000, false},
		{"mixed shared and non-shared", append(append([]StrategyConfig{}, hlPair...), StrategyConfig{ID: "spot-btc", Platform: "binanceus", Type: "spot", Capital: 2000}),
			map[string]float64{"hl-btc": 5000, "hl-eth": 5000, "spot-btc": 2000}, map[SharedWalletKey]float64{hlKey: 7500}, 3, 9500, false},
		{"missing balance falls back to member sum", hlPair,
			map[string]float64{"hl-btc": 4000, "hl-eth": 6000}, nil, 2, 10000, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xtest")
			accountShared := detectSharedWallets(tc.strategies)
			got, fb := computeSubsetPortfolioValue(tc.strategies[:tc.subsetN], flatCashState(tc.cash), nil, tc.balances, accountShared)
			if got != tc.want {
				t.Errorf("subset value=%.2f, want %.2f", got, tc.want)
			}
			if fb != tc.wantFallback {
				t.Errorf("usedFallback=%v, want %v", fb, tc.wantFallback)
			}
		})
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
			name:        "deficit remains after release and closes only",
			flippingQty: 6, peerQty: 12, wantAvailable: -80, wantSize: 6,
		},
		{
			name:        "deficit before release sizes within true remainder",
			flippingQty: 6, peerQty: 6, wantAvailable: -20, wantSize: 10,
		},
		{
			name:        "only position can reuse full equity",
			flippingQty: 6, wantAvailable: 40, wantSize: 16,
		},
		{
			name:        "healthy wallet keeps existing sizing",
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
