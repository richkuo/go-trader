package main

import (
	"fmt"
	"os"
	"sort"
)

type SharedWalletKey struct {
	Platform string
	Account  string
}

type WalletBalanceFetcher func(SharedWalletKey) (float64, error)

type configuredWalletKey struct {
	Platform   string
	Instrument string
	AccountEnv string
}

var walletKeyRegistry = []struct {
	platform   string
	instrument string
	liveFn     func([]string) bool
	envVar     string
}{
	{platform: "hyperliquid", instrument: "perps", liveFn: hyperliquidIsLive, envVar: "HYPERLIQUID_ACCOUNT_ADDRESS"},
	{platform: "okx", instrument: "perps", liveFn: okxIsLive, envVar: "OKX_API_KEY"},
	{platform: "topstep", instrument: "futures", liveFn: topstepIsLive, envVar: "TOPSTEP_ACCOUNT_ID"},
	{platform: "robinhood", instrument: "spot", liveFn: robinhoodIsLive, envVar: "ROBINHOOD_USERNAME"},
}

func walletKeyFor(sc StrategyConfig) (SharedWalletKey, bool) {
	for _, entry := range walletKeyRegistry {
		if sc.Platform != entry.platform || sc.Type != entry.instrument {
			continue
		}
		if !entry.liveFn(sc.Args) {
			continue
		}
		account := os.Getenv(entry.envVar)
		if account == "" {
			return SharedWalletKey{}, false
		}
		return SharedWalletKey{Platform: entry.platform, Account: account}, true
	}
	return SharedWalletKey{}, false
}

func configuredWalletKeyFor(sc StrategyConfig) (configuredWalletKey, bool) {
	for _, entry := range walletKeyRegistry {
		if sc.Platform != entry.platform || sc.Type != entry.instrument {
			continue
		}
		if !entry.liveFn(sc.Args) || !hasSharedWalletBalanceFetcher(entry.platform) {
			continue
		}
		return configuredWalletKey{
			Platform:   entry.platform,
			Instrument: entry.instrument,
			AccountEnv: entry.envVar,
		}, true
	}
	return configuredWalletKey{}, false
}

func usesSharedWalletPoolBudget(sc StrategyConfig) bool {
	return sc.sharedWalletPoolBudget
}

type sharedWalletRiskBalanceSnapshot struct {
	Balance    float64
	Generation int
}

func resolveSharedWalletRiskBalances(
	strategies []StrategyConfig,
	strategyStates map[string]*StrategyState,
	sharedWallets map[SharedWalletKey][]string,
	current map[SharedWalletKey]float64,
	cache map[SharedWalletKey]sharedWalletRiskBalanceSnapshot,
	generation int,
) (resolved map[SharedWalletKey]float64, usedStale, equityComplete bool) {
	resolved = make(map[SharedWalletKey]float64, len(current))
	for key, balance := range current {
		resolved[key] = balance
	}
	equityComplete = true

	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
	}
	for key, memberIDs := range sharedWallets {
		pooled := false
		for _, id := range memberIDs {
			sc := byID[id]
			s := strategyStates[id]
			if usesSharedWalletPoolBudget(sc) || sc.sharedWalletModeDeferred ||
				(s != nil && s.SharedWalletPoolBudget) {
				pooled = true
				break
			}
		}
		if !pooled {
			continue
		}
		if balance, ok := current[key]; ok {
			if cache != nil {
				cache[key] = sharedWalletRiskBalanceSnapshot{Balance: balance, Generation: generation}
			}
			continue
		}
		if snapshot, ok := cache[key]; ok && snapshot.Generation == generation-1 {
			resolved[key] = snapshot.Balance
			usedStale = true
			fmt.Printf("[WARN] shared-wallet %s/%s: balance fetch missing, using prior risk snapshot $%.2f for this cycle only (portfolio peak frozen)\n",
				key.Platform, key.Account, snapshot.Balance)
			continue
		}
		equityComplete = false
		fmt.Printf("[WARN] shared-wallet %s/%s: pooled balance unavailable with no prior risk snapshot — suppressing portfolio equity drawdown this cycle (perps margin risk remains active)\n",
			key.Platform, key.Account)
	}
	return resolved, usedStale, equityComplete
}

func sharedWalletPoolMarginBasisPrice(markPrice, avgCost float64) float64 {
	if avgCost > markPrice {
		return avgCost
	}
	return markPrice
}

func sharedWalletPoolMarginLeverage(positionLeverage, configLeverage float64) float64 {
	if positionLeverage > 0 {
		return positionLeverage
	}
	if configLeverage > 0 {
		return configLeverage
	}
	return 1
}

func validateConfiguredSharedWalletPools(strategies []StrategyConfig) (map[string]bool, []string) {
	groups := make(map[configuredWalletKey][]StrategyConfig)
	for _, sc := range strategies {
		if key, ok := configuredWalletKeyFor(sc); ok {
			groups[key] = append(groups[key], sc)
		}
	}

	pooledIDs := make(map[string]bool)
	var errs []string
	for key, members := range groups {
		if len(members) < 2 {
			continue
		}
		poolRequested := false
		for _, sc := range members {
			if sc.Capital == 0 && sc.CapitalPct == 0 {
				poolRequested = true
				pooledIDs[sc.ID] = true
			}
		}
		if !poolRequested {
			continue
		}
		for _, sc := range members {
			if sc.Capital != 0 || sc.CapitalPct != 0 {
				errs = append(errs, fmt.Sprintf(
					"shared-wallet pool %s/%s: strategy[%s] uses a virtual capital allocation; every member must omit capital and capital_pct",
					key.Platform, key.Instrument, sc.ID))
				continue
			}
			pooledIDs[sc.ID] = true
			if EffectiveMarginPerTradeUSD(sc) <= 0 {
				errs = append(errs, fmt.Sprintf(
					"strategy[%s]: shared-wallet pool members require positive margin_per_trade_usd as the per-open hard cap",
					sc.ID))
			}
			if sc.InitialCapital != 0 {
				errs = append(errs, fmt.Sprintf(
					"strategy[%s]: initial_capital is not supported in shared-wallet pool mode; pooled performance has no per-strategy deposit baseline",
					sc.ID))
			}
			if EffectiveRiskPerTradePct(sc) > 0 {
				errs = append(errs, fmt.Sprintf(
					"strategy[%s]: risk_per_trade_pct requires a per-strategy capital denominator and is not supported in shared-wallet pool mode",
					sc.ID))
			}
		}
	}
	sort.Strings(errs)
	return pooledIDs, errs
}

var platformsWithSharedWalletBalanceFetcher = map[string]bool{
	"hyperliquid": true,
	"okx":         true,
}

func hasSharedWalletBalanceFetcher(platform string) bool {
	return platformsWithSharedWalletBalanceFetcher[platform]
}

func detectSharedWallets(strategies []StrategyConfig) map[SharedWalletKey][]string {
	walletStrategies := make(map[SharedWalletKey][]string)
	for _, sc := range strategies {
		key, ok := walletKeyFor(sc)
		if !ok {
			continue
		}
		if !hasSharedWalletBalanceFetcher(key.Platform) {
			continue
		}
		walletStrategies[key] = append(walletStrategies[key], sc.ID)
	}
	shared := make(map[SharedWalletKey][]string)
	for k, ids := range walletStrategies {
		if len(ids) > 1 {
			shared[k] = ids
		}
	}
	return shared
}

func sharedWalletPoolAvailableMargin(
	sc StrategyConfig,
	strategies []StrategyConfig,
	state *AppState,
	prices map[string]float64,
	sharedWallets map[SharedWalletKey][]string,
	walletBalances map[SharedWalletKey]float64,
) (available float64, pooled bool, balanceKnown bool) {
	if !usesSharedWalletPoolBudget(sc) {
		return 0, false, false
	}
	pooled = true
	key, ok := walletKeyFor(sc)
	if !ok {
		return 0, true, false
	}
	perpsMemberIDs, ok := sharedWallets[key]
	if !ok {
		return 0, true, false
	}
	balance, ok := walletBalances[key]
	if !ok || balance <= 0 || state == nil {
		return 0, true, false
	}
	balanceKnown = true

	byID := make(map[string]StrategyConfig, len(strategies))
	for _, member := range strategies {
		byID[member.ID] = member
	}
	deployedMargin := 0.0
	for _, id := range riskPathWalletMemberIDs(key, perpsMemberIDs, strategies) {
		ss := state.Strategies[id]
		if ss == nil {
			continue
		}
		memberCfg := byID[id]
		for sym, pos := range ss.Positions {
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			price := sharedWalletPoolMarginBasisPrice(prices[sym], pos.AvgCost)
			if price <= 0 {
				continue
			}
			leverage := sharedWalletPoolMarginLeverage(pos.Leverage, EffectiveExchangeLeverage(memberCfg))
			deployedMargin += pos.Quantity * price / leverage
		}
	}
	available = balance - deployedMargin
	return available, true, true
}

func detectTopStepSharedWallet(strategies []StrategyConfig) (SharedWalletKey, bool) {
	counts := make(map[SharedWalletKey]int)
	for _, sc := range strategies {
		if sc.Platform != "topstep" {
			continue
		}
		key, ok := walletKeyFor(sc)
		if !ok {
			continue
		}
		counts[key]++
	}
	for key, n := range counts {
		if n > 1 {
			return key, true
		}
	}
	return SharedWalletKey{}, false
}

func defaultSharedWalletFetcher(key SharedWalletKey) (float64, error) {
	switch key.Platform {
	case "hyperliquid":
		return fetchHyperliquidBalance(key.Account)
	case "okx":
		return defaultSharedWalletBalance("okx")
	}
	return 0, fmt.Errorf("unsupported shared-wallet platform %q", key.Platform)
}

func fetchSharedWalletBalances(
	strategies []StrategyConfig,
	fetcher WalletBalanceFetcher,
) (map[SharedWalletKey]float64, map[SharedWalletKey]error) {
	if fetcher == nil {
		fetcher = defaultSharedWalletFetcher
	}
	sharedWallets := detectSharedWallets(strategies)
	balances := make(map[SharedWalletKey]float64, len(sharedWallets))
	errs := make(map[SharedWalletKey]error)
	for key := range sharedWallets {
		bal, err := fetcher(key)
		if err != nil {
			errs[key] = err
			continue
		}
		balances[key] = bal
	}
	return balances, errs
}

func computeSubsetPortfolioValue(
	subset []StrategyConfig,
	state *AppState,
	prices map[string]float64,
	walletBalances map[SharedWalletKey]float64,
	accountShared map[SharedWalletKey][]string,
) (float64, bool) {
	if accountShared == nil {
		accountShared = detectSharedWallets(subset)
	}

	subShared := detectSharedWallets(subset)

	dedupeIDs := make(map[string]bool)
	var fullyContainedKeys []SharedWalletKey
	for key, subIDs := range subShared {
		if len(subIDs) == len(accountShared[key]) {
			fullyContainedKeys = append(fullyContainedKeys, key)
			for _, id := range riskPathWalletMemberIDs(key, subIDs, subset) {
				dedupeIDs[id] = true
			}
		}
	}

	total := 0.0

	for _, sc := range subset {
		if dedupeIDs[sc.ID] {
			continue
		}
		if s, ok := state.Strategies[sc.ID]; ok {
			total += PortfolioValue(s, prices)
		}
	}

	usedFallback := false
	for _, key := range fullyContainedKeys {
		if bal, ok := walletBalances[key]; ok {
			total += bal
			continue
		}
		usedFallback = true
		sumPV := 0.0
		for _, id := range riskPathWalletMemberIDs(key, subShared[key], subset) {
			if s, ok := state.Strategies[id]; ok {
				sumPV += PortfolioValue(s, prices)
			}
		}
		fmt.Printf("[WARN] shared-wallet %s/%s: balance fetch missing, falling back to sum(member PV)=$%.2f (peak will NOT be updated this cycle)\n",
			key.Platform, key.Account, sumPV)
		total += sumPV
	}

	return total, usedFallback
}

func computeTotalPortfolioValue(
	strategies []StrategyConfig,
	state *AppState,
	prices map[string]float64,
	walletBalances map[SharedWalletKey]float64,
	sharedWallets map[SharedWalletKey][]string,
) (float64, bool) {
	if sharedWallets == nil {
		sharedWallets = detectSharedWallets(strategies)
	}
	return computeSubsetPortfolioValue(strategies, state, prices, walletBalances, sharedWallets)
}

func computeInitialPortfolioPeak(strategies []StrategyConfig, fetcher WalletBalanceFetcher) float64 {
	if fetcher == nil {
		fetcher = defaultSharedWalletFetcher
	}
	sharedWallets := detectSharedWallets(strategies)
	sharedStrategyIDs := make(map[string]bool)
	for key, ids := range sharedWallets {
		for _, id := range riskPathWalletMemberIDs(key, ids, strategies) {
			sharedStrategyIDs[id] = true
		}
	}

	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
	}

	total := 0.0
	walletCounted := make(map[string]bool)
	for _, sc := range strategies {
		if sharedStrategyIDs[sc.ID] {
			continue
		}
		if sc.CapitalPct > 0 {
			if !walletCounted[sc.Platform] {
				total += sc.Capital / sc.CapitalPct
				walletCounted[sc.Platform] = true
			}
			continue
		}
		total += sc.Capital
	}
	for key, ids := range sharedWallets {
		bal, err := fetcher(key)
		if err != nil {
			fmt.Printf("[WARN] shared-wallet peak init: balance fetch failed for %s/%s: %v — falling back to summed capital\n",
				key.Platform, key.Account, err)
			for _, id := range riskPathWalletMemberIDs(key, ids, strategies) {
				if sc, ok := byID[id]; ok {
					total += sc.Capital
				}
			}
			continue
		}
		total += bal
	}
	return total
}

func computeInitialPortfolioPeakForScope(strategies []StrategyConfig, scope PortfolioScope, fetcher WalletBalanceFetcher) float64 {
	return computeInitialPortfolioPeak(strategiesInScope(strategies, scope), fetcher)
}

func rebaselinePortfolioPeakAfterPruneForScope(state *AppState, cfg *Config, scope PortfolioScope, fetcher WalletBalanceFetcher) float64 {
	scopedCfg := &Config{Strategies: strategiesInScope(cfg.Strategies, scope)}
	scopedState := &AppState{Strategies: filterStatesByScope(state.Strategies, cfg.Strategies, scope)}
	return rebaselinePortfolioPeakAfterPrune(scopedState, scopedCfg, fetcher)
}

func rebaselinePortfolioPeakAfterPrune(state *AppState, cfg *Config, fetcher WalletBalanceFetcher) float64 {
	byID := make(map[string]StrategyConfig, len(cfg.Strategies))
	for _, sc := range cfg.Strategies {
		byID[sc.ID] = sc
	}

	dedupedManual := dedupedSameAccountLiveManualIDs(cfg.Strategies)

	sum := 0.0
	for id, ss := range state.Strategies {
		if ss == nil {
			continue
		}
		if dedupedManual[id] {
			continue
		}
		if ss.RiskState.PeakValue > 0 {
			sum += ss.RiskState.PeakValue
			continue
		}
		if sc, ok := byID[id]; ok {
			sum += sc.Capital
		}
	}

	floor := computeInitialPortfolioPeak(cfg.Strategies, fetcher)
	if sum < floor {
		sum = floor
	}
	return sum
}
