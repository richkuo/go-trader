package main

import (
	"fmt"
	"os"
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

// walletKeyFor returns the shared-wallet key for a strategy if it trades from
// a shared on-exchange account, otherwise (zero, false).
//
// Currently only Hyperliquid live perps strategies are recognized: they all
// trade from the address in HYPERLIQUID_ACCOUNT_ADDRESS, so any two such
// strategies share the wallet by definition.
//
// TODO(#243-extension): extend to other live perps/futures platforms once they
// grow multi-strategy setups on a single account (candidates: okx live swap,
// topstep live, robinhood live). Each needs its own env-var / account-id source
// and live-mode predicate. Consider a small registry keyed on
// (platform, type, live-mode flag) to keep this centralized.
func walletKeyFor(sc StrategyConfig) (SharedWalletKey, bool) {
	if sc.Platform == "hyperliquid" && sc.Type == "perps" && hyperliquidIsLive(sc.Args) {
		addr := os.Getenv("HYPERLIQUID_ACCOUNT_ADDRESS")
		if addr == "" {
			return SharedWalletKey{}, false
		}
		return SharedWalletKey{Platform: "hyperliquid", Account: addr}, true
	}
	return SharedWalletKey{}, false
}

// detectSharedWallets returns the set of shared-wallet keys that have more
// than one strategy attached, mapped to the list of strategy IDs that share
// the wallet. Wallets with only a single strategy are NOT included — for
// those the existing per-strategy sum is already correct.
func detectSharedWallets(strategies []StrategyConfig) map[SharedWalletKey][]string {
	walletStrategies := make(map[SharedWalletKey][]string)
	for _, sc := range strategies {
		key, ok := walletKeyFor(sc)
		if !ok {
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

// defaultSharedWalletFetcher dispatches to the platform-specific balance API.
func defaultSharedWalletFetcher(key SharedWalletKey) (float64, error) {
	switch key.Platform {
	case "hyperliquid":
		return fetchHyperliquidBalance(key.Account)
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

// computeTotalPortfolioValue returns the total portfolio value across all
// strategies, using pre-fetched real exchange balances for shared wallets so
// the same account is not double-counted across multiple strategies (#243).
//
// Strategies whose wallet is shared with at least one other strategy are
// excluded from the per-strategy sum and replaced with a single fetched
// balance per wallet.
//
// Fallback: when a shared-wallet balance is missing from walletBalances (e.g.
// transient API failure), the function uses the MAX of member strategies'
// PortfolioValue — NOT the sum. Summing members would re-introduce the exact
// #243 double-count bug and can permanently inflate PortfolioRisk.PeakValue
// (peak is sticky). Max is a lower-bound approximation that never exceeds a
// single strategy's slice of the wallet. The returned usedFallback flag tells
// the caller to skip peak ratcheting for that cycle so a network blip cannot
// move the high-water mark.
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

	// Build a quick lookup of strategy IDs that belong to a shared wallet.
	sharedStrategyIDs := make(map[string]bool)
	for _, ids := range sharedWallets {
		for _, id := range ids {
			sharedStrategyIDs[id] = true
		}
	}

	total := 0.0

	// Per-strategy sum for everything that does NOT live in a shared wallet.
	for _, sc := range strategies {
		if sharedStrategyIDs[sc.ID] {
			continue
		}
		if s, ok := state.Strategies[sc.ID]; ok {
			total += PortfolioValue(s, prices)
		}
	}

	// One real-balance contribution per shared wallet. On fetch failure,
	// use MAX of member strategies' PVs (never the sum — that's #243).
	usedFallback := false
	for key, ids := range sharedWallets {
		if bal, ok := walletBalances[key]; ok {
			total += bal
			continue
		}
		usedFallback = true
		maxPV := 0.0
		for _, id := range ids {
			s, ok := state.Strategies[id]
			if !ok {
				continue
			}
			if pv := PortfolioValue(s, prices); pv > maxPV {
				maxPV = pv
			}
		}
		fmt.Printf("[WARN] shared-wallet %s/%s: balance fetch missing, falling back to max(member PV)=$%.2f (peak will NOT be updated this cycle)\n",
			key.Platform, key.Account, maxPV)
		total += maxPV
	}

	return total, usedFallback
}

// computeInitialPortfolioPeak returns the initial PortfolioRisk.PeakValue used
// when no peak has been recorded yet. It uses real wallet balances for shared
// wallets (#243) so the peak is not inflated by summing the same account
// multiple times across strategies. Strategies that use capital_pct on a
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
	for _, ids := range sharedWallets {
		for _, id := range ids {
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
			for _, id := range ids {
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
