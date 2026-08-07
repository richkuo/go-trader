package main

import (
	"fmt"
	"os"
	"sort"
)

// SharedWalletKey identifies a shared exchange account by platform + account ID.
// Multiple strategies that map to the same key are assumed to trade from the
// same on-exchange wallet, so per-strategy capital must NOT be summed when
// computing total portfolio value.
type SharedWalletKey struct {
	Platform string
	Account  string
}

// WalletBalanceFetcher returns the live wallet balance for a given key.
// Injected so tests can stub out network calls.
//
// NOTE: distinct from risk.go's SharedWalletBalanceFetcher (#244), which is
// keyed by platform string and used by ClearLatchedKillSwitchSharedWallet on
// startup. This one is keyed by SharedWalletKey (platform + account) so a
// single platform can host multiple distinct wallets if that ever comes up.
type WalletBalanceFetcher func(SharedWalletKey) (float64, error)

// configuredWalletKey identifies a wallet-shaped strategy without reading
// credentials. It is used only during config validation, where live
// credentials may deliberately be skipped (startup probes and tests). The
// account token is the registry env-var name, not an account value; runtime
// accounting must continue to use walletKeyFor.
type configuredWalletKey struct {
	Platform   string
	Instrument string
	AccountEnv string
}

// walletKeyRegistry enumerates the (platform, instrument) pairs we recognize
// as single-on-exchange-account trading. Each entry supplies the live-mode
// predicate and the env-var that identifies the account. Adding a new live
// platform = append one entry; no other code in this file changes.
//
// NOTE: recognition via walletKeyFor does NOT imply that a live balance can be
// fetched — that's a separate capability tracked by hasSharedWalletBalanceFetcher.
// detectSharedWallets filters by fetcher availability so expanding this
// registry does not regress portfolio-value math for platforms whose balance
// fetcher is not yet implemented (phase 1a of #357).
var walletKeyRegistry = []struct {
	platform   string
	instrument string // sc.Type value ("perps", "futures", "spot")
	liveFn     func([]string) bool
	envVar     string
}{
	// Hyperliquid perps live — original entry, trades from HYPERLIQUID_ACCOUNT_ADDRESS.
	{platform: "hyperliquid", instrument: "perps", liveFn: hyperliquidIsLive, envVar: "HYPERLIQUID_ACCOUNT_ADDRESS"},
	// OKX perps (swap) live — multi-strategy on one API key share the same
	// margin account; OKX_API_KEY uniquely identifies the account (#357 phase 1a).
	{platform: "okx", instrument: "perps", liveFn: okxIsLive, envVar: "OKX_API_KEY"},
	// TopStep futures live — TOPSTEP_ACCOUNT_ID is the natural account key
	// (#357 phase 1a).
	{platform: "topstep", instrument: "futures", liveFn: topstepIsLive, envVar: "TOPSTEP_ACCOUNT_ID"},
	// Robinhood crypto spot live — multi-strategy on one username share the
	// same spot asset balance; ROBINHOOD_USERNAME identifies the account
	// (#357 phase 1a).
	{platform: "robinhood", instrument: "spot", liveFn: robinhoodIsLive, envVar: "ROBINHOOD_USERNAME"},
}

// walletKeyFor returns the on-exchange account key for a strategy if it trades
// from an identifiable live wallet, otherwise (zero, false).
//
// Recognition is driven by walletKeyRegistry (above). The returned key is
// suitable for:
//   - grouping multiple strategies on the same account for per-strategy
//     circuit-breaker close sizing (#357)
//   - shared-wallet double-count protection in portfolio value (#243) — but
//     only when a balance fetcher is registered, see hasSharedWalletBalanceFetcher
//
// Paper-mode strategies and strategies missing their account env var return
// (zero, false) by design.
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

// usesSharedWalletPoolBudget is the structural opt-in for scheduler-owned
// shared-wallet budgeting. Validation guarantees that every member of the
// configured live wallet cluster opts in together and supplies a positive
// margin_per_trade_usd cap. Runtime sizing additionally requires a fresh
// account balance; missing balance data therefore fails position increases
// closed while close-only orders remain available.
func usesSharedWalletPoolBudget(sc StrategyConfig) bool {
	return sc.sharedWalletPoolBudget
}

// sharedWalletRiskBalanceSnapshot is the most recent real balance accepted by
// the portfolio-risk path for one pooled wallet. Generation counts portfolio
// risk evaluations (not scheduler ticks), so a snapshot is valid for exactly
// the next risk evaluation after one failed fetch regardless of strategy
// interval spacing. It is process-local and never used for order sizing,
// reconciliation, or operator display.
type sharedWalletRiskBalanceSnapshot struct {
	Balance    float64
	Generation int
}

// resolveSharedWalletRiskBalances builds the balance map used only by
// computeTotalPortfolioValue in the portfolio kill-switch phase. Allocated
// shared wallets retain the historical modeled-book fallback. A pool wallet,
// whose books contain performance rather than deposits, may reuse exactly the
// preceding risk evaluation's real balance for one fetch miss. With no such
// snapshot the returned equityComplete flag is false so the caller suppresses
// only the equity-drawdown arm; perps margin drawdown remains live. The durable
// state marker keeps this protection active while a pool exit is deferred,
// even though the replacement config already uses an allocated budget.
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

// sharedWalletPoolMarginBasisPrice returns the conservative price used for
// both deployed-margin reservation and flip-release accounting. Account
// equity already includes unrealized PnL, while an exchange can continue to
// reserve the entry-margin amount on an underwater position. Using the larger
// of entry and mark prevents a losing position from making free margin look
// larger than the venue will accept. The exact same basis must be released on
// a flip so reservation and release cancel cleanly.
func sharedWalletPoolMarginBasisPrice(markPrice, avgCost float64) float64 {
	if avgCost > markPrice {
		return avgCost
	}
	return markPrice
}

// sharedWalletPoolMarginLeverage is the single leverage resolver for both
// reservation and flip release. Stored position leverage wins because it is
// the leverage under which the open was booked; current config is only a
// fallback for legacy positions that lack the stamp.
func sharedWalletPoolMarginLeverage(positionLeverage, configLeverage float64) float64 {
	if positionLeverage > 0 {
		return positionLeverage
	}
	if configLeverage > 0 {
		return configLeverage
	}
	return 1
}

// validateConfiguredSharedWalletPools returns the zero-capital strategy IDs
// that belong to a configured 2+ member wallet and any cluster-level errors.
// A pool is all-or-nothing: mixing virtual allocations with pooled members
// would let the allocated member bypass the account reservation path.
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

// platformsWithSharedWalletBalanceFetcher lists platforms for which
// defaultSharedWalletFetcher can return a live balance. Keep this data-driven
// (matches walletKeyRegistry style) so enabling a new platform = one-line flip
// alongside its fetcher wiring in the corresponding phase PR.
//
// TODO(#357): this is keyed on platform alone, while walletKeyRegistry is keyed
// on (platform, instrument). That's fine today because each platform has a
// single instrument flavor in the registry, but if a platform ever gains a
// second flavor (e.g. OKX spot in addition to OKX swap) the fetcher-capability
// bit will auto-enable/disable both — confirm the fetcher handles every
// registered instrument for the platform before flipping it on.
var platformsWithSharedWalletBalanceFetcher = map[string]bool{
	"hyperliquid": true,
	"okx":         true, // #360 phase 2 of #357 — fetch_okx_balance.py
	// #1106 phase 4 of #1100: TopStep is DELIBERATELY NOT listed here during the
	// shadow phase. Its /v1/account/balance equity feed is unverified, and
	// listing it would pull TopStep into detectSharedWallets → the live
	// computeTotalPortfolioValue / CheckPortfolioRisk path (the all-platform kill
	// switch), where a wrong-but-positive equity could crater totalPV behind only
	// a >0 check and trip a cross-platform close — and a missing balance would set
	// usedFallback every cycle, freezing the portfolio peak ratchet. The shadow
	// cash-flow journal detects its own grouping via detectTopStepSharedWallet and
	// consumes the equity directly, so portfolio risk stays on the pre-PR
	// per-strategy member-PV behavior until Phase 4b verifies the feed.
}

// hasSharedWalletBalanceFetcher reports whether defaultSharedWalletFetcher can
// return a live balance for the given platform. Platforms recognized by
// walletKeyFor but without a fetcher are EXCLUDED from detectSharedWallets so
// multi-strategy setups on those platforms don't cause computeTotalPortfolioValue
// to enter fallback and freeze the portfolio peak on every cycle (#357 phase 1a
// preserves HL-only portfolio-value behavior).
//
// As phase 2-4 land real balance fetchers for OKX / TopStep / Robinhood, add
// their platform strings to platformsWithSharedWalletBalanceFetcher to enable
// double-count protection for them.
func hasSharedWalletBalanceFetcher(platform string) bool {
	return platformsWithSharedWalletBalanceFetcher[platform]
}

// detectSharedWallets returns the set of shared-wallet keys that have more
// than one strategy attached, mapped to the list of strategy IDs that share
// the wallet. Wallets with only a single strategy are NOT included — for
// those the existing per-strategy sum is already correct.
//
// Wallets on platforms without a registered balance fetcher (see
// hasSharedWalletBalanceFetcher) are also excluded: without a real-balance
// fetch, computeTotalPortfolioValue would use fallback every cycle and freeze
// the peak (#357 phase 1a preserves HL-only behavior).
// As phase 2-4 land balance fetchers for OKX / TS / RH, those platforms
// become eligible for double-count protection automatically.
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

// sharedWalletPoolAvailableMargin returns the account margin still available
// to a pooled strategy after reserving margin for every virtual position on
// the same wallet, including same-account live HL manual positions. The caller
// must hold the state read lock. pooled reports pool mode; balanceKnown
// distinguishes a real fully-deployed wallet (known balance, zero or negative
// headroom) from missing balance/identity data. Available is intentionally SIGNED:
// fresh-open and scale-in notional helpers clamp non-positive cash to zero,
// while a flip must preserve any account-margin deficit until it adds back the
// closing position's reservation. Clamping here would overstate post-close
// headroom and could make an oversized flip order lose its close leg.
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

// detectTopStepSharedWallet reports whether 2+ live TopStep strategies share an
// account, gating the #1106 shadow cash-flow journal. It is INTENTIONALLY
// independent of detectSharedWallets / platformsWithSharedWalletBalanceFetcher:
// the journal must be able to run without TopStep being a member of the
// kill-switch sharedWallets map, so the unverified /v1/account/balance equity
// never enters computeTotalPortfolioValue (the live all-platform portfolio kill
// switch) during the shadow phase. Membership is derived the same way as
// detectSharedWallets — walletKeyFor already filters to live mode and a present
// TOPSTEP_ACCOUNT_ID — minus the fetcher-capability gate.
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

// defaultSharedWalletFetcher dispatches to the platform-specific balance API.
func defaultSharedWalletFetcher(key SharedWalletKey) (float64, error) {
	switch key.Platform {
	case "hyperliquid":
		return fetchHyperliquidBalance(key.Account)
	case "okx":
		// OKX credentials identify the process account; defaultSharedWalletBalance
		// owns the validated fetch_okx_balance.py path used by startup recovery.
		return defaultSharedWalletBalance("okx")
	}
	return 0, fmt.Errorf("unsupported shared-wallet platform %q", key.Platform)
}

// fetchSharedWalletBalances fetches the live balance of every shared wallet
// referenced by the strategy list. Performs network I/O and MUST be called
// without holding any state lock. Wallets whose fetch fails are reported via
// the returned error map so the caller can fall back to per-strategy sums.
//
// NOTE: main.go bypasses this helper and fetches clearinghouseState directly
// so the same HTTP call can feed both the risk check and the position sync
// (see fetchHyperliquidState). This function is retained for tests and for
// any caller that only needs balances.
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

// computeSubsetPortfolioValue returns the portfolio value for a subset of
// strategies, deduplicating shared wallets only when the wallet is fully
// contained within the subset. If a shared wallet straddles the subset
// boundary (some members are outside the subset), that wallet's strategies
// are virtual-summed rather than deduped — a single on-exchange balance
// cannot be split across a partial subset (#915). Same-account live HL manual
// strategies are folded into the dedup set via riskPathWalletMemberIDs (#921).
//
// accountShared is the shared-wallet map for the full account (all strategies),
// used to detect straddle boundaries. Pass the result of
// detectSharedWallets(allStrategies) so the containment check sees the real
// membership. When accountShared is nil the subset is treated as the full
// account (equivalent to calling computeTotalPortfolioValue).
//
// walletBalances, state, and prices semantics are identical to
// computeTotalPortfolioValue.
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

	// Detect shared wallets within the subset.
	subShared := detectSharedWallets(subset)

	// A wallet is "fully contained" when every account member is also in the
	// subset. Only fully-contained wallets get real-balance dedup; strategies
	// whose wallet straddles the boundary are virtual-summed instead.
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

	// Per-strategy sum for everything NOT in a fully-contained shared wallet
	// (includes strategies from straddling wallets — virtual-summed).
	for _, sc := range subset {
		if dedupeIDs[sc.ID] {
			continue
		}
		if s, ok := state.Strategies[sc.ID]; ok {
			total += PortfolioValue(s, prices)
		}
	}

	// One real-balance contribution per fully-contained shared wallet.
	// On fetch failure, sum member strategy PVs; usedFallback still freezes
	// peak ratcheting.
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

// computeTotalPortfolioValue returns the total portfolio value across all
// strategies, using pre-fetched real exchange balances for shared wallets so
// the same account is not double-counted across multiple strategies (#243).
//
// Strategies whose wallet is shared with at least one other strategy are
// excluded from the per-strategy sum and replaced with a single fetched
// balance per wallet.
//
// Fallback: when a shared-wallet balance is missing from walletBalances (e.g.
// transient API failure), the function sums member strategies' PortfolioValue.
// The real-balance path still contributes the wallet once (#243); fallback has
// no real wallet balance to de-duplicate, and each strategy carries its own
// virtual cash/position slice. The returned usedFallback flag tells the caller
// to skip peak ratcheting for that cycle so a network blip cannot move the
// high-water mark.
//
// This function only reads state and does NOT perform network I/O — call
// fetchSharedWalletBalances (or fetch clearinghouseState directly) first
// without the lock, then call this under the state read lock.
//
// The sharedWallets parameter is pre-computed by the caller so the map is
// built once per cycle instead of twice (detection + computation).
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
	// Whole-account call: every shared wallet is fully contained in strategies,
	// so delegating to computeSubsetPortfolioValue gives identical results.
	return computeSubsetPortfolioValue(strategies, state, prices, walletBalances, sharedWallets)
}

// computeInitialPortfolioPeak returns the initial PortfolioRisk.PeakValue used
// when no peak has been recorded yet. It uses real wallet balances for shared
// wallets (#243) so the peak is not inflated by summing the same account
// multiple times across strategies. Same-account live HL manual strategies are
// excluded from the standalone capital sum via riskPathWalletMemberIDs (#921). Strategies that use capital_pct on a
// non-shared platform fall back to the legacy "wallet balance once per
// platform" computation (Capital / CapitalPct) so existing single-strategy
// setups are unaffected.
//
// Behavioral note (for release notes): a single live HL strategy with
// CapitalPct > 0 is NOT shared (only one strategy on the wallet) and still
// takes the legacy Capital/CapitalPct path. Adding a second live HL strategy
// later flips the peak init to the real on-exchange balance — usually more
// accurate, but a visible behavior change for existing users.
//
// Performs network I/O for shared-wallet platforms — call from startup, not
// from inside the hot loop.
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

	// Index strategies by ID once for fallback lookups.
	byID := make(map[string]StrategyConfig, len(strategies))
	for _, sc := range strategies {
		byID[sc.ID] = sc
	}

	total := 0.0
	walletCounted := make(map[string]bool)
	for _, sc := range strategies {
		if sharedStrategyIDs[sc.ID] {
			continue // handled below via real balance fetch
		}
		// Legacy: capital_pct strategies derive wallet from Capital / CapitalPct
		// and count each platform's wallet once. Preserved unchanged for
		// non-shared setups so existing behavior is identical.
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

// rebaselinePortfolioPeakAfterPrune recomputes PortfolioRisk.PeakValue from the
// per-strategy peaks of remaining strategies after one or more strategies are
// pruned from config. Without this, the stale portfolio peak (which reflects
// the pre-prune strategy set) can immediately latch the kill switch on the
// first risk-check cycle since current portfolio value drops to the sum of
// only the surviving strategies. See issue #650.
//
// For each surviving strategy, prefers RiskState.PeakValue (the per-strategy
// high-water mark recorded by CheckRisk). Falls back to that strategy's
// configured Capital when no per-strategy peak has been recorded yet
// (cold-start or migrated state).
//
// The result is floored at computeInitialPortfolioPeak(remaining) so the
// rebaseline never drops below the sum-of-capitals baseline that a fresh
// install would use — protects against under-baseline when most surviving
// strategies are themselves cold-started.
//
// Same-account live HL manual strategies on a deduped shared wallet are
// excluded from the per-strategy sum — CheckRisk never records their peak and
// their collateral is inside the real balance (#921).
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
