package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// collectPriceSymbols returns the list of BinanceUS-format symbols to fetch
// for spot strategy valuation/notional. Only "spot" strategy types are
// included — spot positions are stored and fetched under the same key
// (e.g. "BTC/USDT"), so no aliasing is needed.
//
// Perps strategies are intentionally excluded: HL and OKX perps marks are
// now sourced from the venues they live on via fetchHyperliquidMids and
// fetchOKXPerpsMids (see collectPerpsMarkSymbols). Routing perps through
// BinanceUS spot introduced phantom PnL on shorts due to spot/perps basis
// drift — fixes issue #263 as a side effect (HL-only coins like HYPE,
// kPEPE, PURR no longer emit [WARN] Skipping zero price — fixes #262).
func collectPriceSymbols(strategies []StrategyConfig) []string {
	set := make(map[string]bool)
	for _, sc := range strategies {
		if sc.Type != "spot" {
			continue
		}
		if len(sc.Args) < 2 {
			continue
		}
		sym := sc.Args[1]
		if sym == "" {
			continue
		}
		set[sym] = true
	}
	symbols := make([]string, 0, len(set))
	for s := range set {
		symbols = append(symbols, s)
	}
	return symbols
}

// collectPerpsMarkSymbols returns two sorted slices of base-coin symbols
// for which the scheduler should fetch venue-native perps marks this cycle.
// hlCoins contains coins traded on Hyperliquid; okxCoins contains coins
// traded on OKX — each slice is deduplicated and sorted for deterministic
// iteration. Strategies with Type != "perps" are ignored.
//
// The returned coins are used as inputs to fetchHyperliquidMids and
// fetchOKXPerpsMids respectively. This is the correct oracle for perps
// positions; see issue #263 for why BinanceUS spot is wrong.
func collectPerpsMarkSymbols(strategies []StrategyConfig) (hlCoins, okxCoins []string) {
	hlSet := make(map[string]bool)
	okxSet := make(map[string]bool)
	for _, sc := range strategies {
		if sc.Type != "perps" {
			continue
		}
		if len(sc.Args) < 2 {
			continue
		}
		coin := sc.Args[1]
		if coin == "" {
			continue
		}
		switch sc.Platform {
		case "hyperliquid":
			hlSet[coin] = true
		case "okx":
			okxSet[coin] = true
		}
	}
	hlCoins = make([]string, 0, len(hlSet))
	for c := range hlSet {
		hlCoins = append(hlCoins, c)
	}
	sort.Strings(hlCoins)

	okxCoins = make([]string, 0, len(okxSet))
	for c := range okxSet {
		okxCoins = append(okxCoins, c)
	}
	sort.Strings(okxCoins)
	return hlCoins, okxCoins
}

// mergePerpsMarks copies non-zero perps mark prices into the shared prices
// map. An existing entry wins — a mark published by a strategy earlier in
// the cycle (ground truth for that cycle) must not be overwritten by a
// potentially staler exchange snapshot. Zero and negative marks are skipped.
//
// DO NOT remove the skip-if-exists guard: it preserves the invariant that
// strategy-published marks always win over fetcher snapshots. This mirrors
// the mergeFuturesMarks contract (scheduler/risk.go).
func mergePerpsMarks(prices map[string]float64, marks map[string]float64) {
	for sym, p := range marks {
		if p <= 0 {
			continue
		}
		if _, exists := prices[sym]; exists {
			continue
		}
		prices[sym] = p
	}
}

// collectFuturesMarkSymbols returns the list of CME futures contract
// symbols (e.g. "ES", "NQ", "MES", "MNQ", "CL") that need live marks to
// revalue open futures positions. Sibling to collectPriceSymbols — kept
// separate because the price-source rail is different: check_price.py
// queries BinanceUS which does not list CME futures, so the Go scheduler
// has to dispatch these symbols to fetch_futures_marks.py (TopStep
// adapter) instead.
//
// Futures strategies store positions under the bare contract symbol
// (state.Positions["ES"]) with Multiplier > 0; the strategy's Args[1] is
// the same symbol, so no normalization or alias mirroring is needed.
// Issue #261: without this, PortfolioNotional / PortfolioValue fell back
// to pos.AvgCost for futures, freezing exposure at entry cost.
//
// Platform filter: only "topstep" futures strategies are emitted.
// fetch_futures_marks.py hardcodes TopStepExchangeAdapter, so routing a
// non-TopStep futures symbol (e.g. a future IBKR futures adapter) through
// this path would either fail outright or — worse — succeed against a
// different contract on a different exchange. When a second futures
// adapter is added, this helper should be generalized to return a
// platform→symbols map (or similar) and fetch_futures_marks.py should
// gain platform-aware dispatch.
func collectFuturesMarkSymbols(strategies []StrategyConfig) []string {
	set := make(map[string]bool)
	for _, sc := range strategies {
		if sc.Type != "futures" {
			continue
		}
		if sc.Platform != "topstep" {
			continue
		}
		if len(sc.Args) < 2 {
			continue
		}
		sym := sc.Args[1]
		if sym == "" {
			continue
		}
		set[sym] = true
	}
	if len(set) == 0 {
		return nil
	}
	symbols := make([]string, 0, len(set))
	for s := range set {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)
	return symbols
}

// mergeFuturesMarks copies non-zero futures mark prices into the shared
// prices map. Existing entries win — matches mirrorPerpsPrices semantics
// so that any live mark a strategy may have already published during the
// cycle is not overwritten by a (possibly staler) fetch result.
//
// DO NOT "simplify" the skip-if-exists branch. Today the only writer
// that could pre-populate e.g. prices["ES"] is a hypothetical futures
// strategy publishing its own live exchange mark via result.Symbol
// earlier in the cycle — mirrorPerpsPrices runs first but only writes
// "/USDT"-quoted spot keys, so collisions with bare CME symbols like
// "ES" or "NQ" are not expected in the current code paths. The guard is
// still required: a strategy-published mark is ground truth for the
// cycle (observed during check), whereas fetch_futures_marks is a
// generic snapshot pulled afterwards and may be slightly stale. When
// both exist, prefer the former. Preserving this invariant matters for
// anyone adding strategy-level mark publishing later — removing the
// skip would silently regress that contract.
func mergeFuturesMarks(prices map[string]float64, marks map[string]float64) {
	for sym, p := range marks {
		if p <= 0 {
			continue
		}
		if _, exists := prices[sym]; exists {
			continue
		}
		prices[sym] = p
	}
}

const maxKillSwitchEvents = 50

// KillSwitchEvent records a kill switch lifecycle event for audit purposes.
//
// Source identifies which drawdown signal drove a "triggered" or "warning"
// event: "equity" (classic peak-relative equity drawdown) or "margin" (perps
// unrealized loss vs. deployed margin, #296). Empty for events that predate
// #296 or are signal-agnostic (e.g. "reset", "auto_reset"). DrawdownPct is the
// percentage of the signal named by Source, so tailing the event log for a
// post-incident review gives an arithmetically consistent record.
type KillSwitchEvent struct {
	Timestamp      time.Time `json:"timestamp"`
	Type           string    `json:"type"` // "triggered", "reset", "warning"
	Source         string    `json:"source,omitempty"`
	DrawdownPct    float64   `json:"drawdown_pct"`
	PortfolioValue float64   `json:"portfolio_value"`
	PeakValue      float64   `json:"peak_value"`
	Details        string    `json:"details"`
}

// PortfolioRiskState tracks aggregate portfolio-level risk (#42).
//
// CurrentDrawdownPct is pure equity drawdown ((PeakValue - totalValue) /
// PeakValue). CurrentMarginDrawdownPct is the #296 perps-margin drawdown
// (perps unrealized loss / deployed margin). Keeping them as separate fields
// preserves the arithmetic invariant that (PeakValue, CurrentDrawdownPct) is
// reconstructable, while still exposing the margin signal for operators and
// the kill switch. The kill switch fires on whichever signal breaches first.
type PortfolioRiskState struct {
	PeakValue                float64           `json:"peak_value"`
	CurrentDrawdownPct       float64           `json:"current_drawdown_pct"`
	CurrentMarginDrawdownPct float64           `json:"current_margin_drawdown_pct,omitempty"`
	KillSwitchActive         bool              `json:"kill_switch_active"`
	KillSwitchAt             time.Time         `json:"kill_switch_at,omitempty"`
	WarningSent              bool              `json:"warning_sent,omitempty"`
	Events                   []KillSwitchEvent `json:"events,omitempty"`
}

// SharedWalletBalanceFetcher returns the real on-chain balance for a given
// platform. Implementations are expected to encapsulate any address/credential
// lookup (e.g. environment variables) and return a non-nil error on any
// network or configuration failure.
type SharedWalletBalanceFetcher func(platform string) (float64, error)

// detectSharedWalletPlatforms returns the list of platforms that have more
// than one strategy sharing the same wallet (capital_pct > 0). A single
// strategy with capital_pct alone is not a "shared" wallet — there is no
// double-counting risk to recover from. The result is sorted alphabetically
// for deterministic iteration order (callers rely on this).
func detectSharedWalletPlatforms(strategies []StrategyConfig) []string {
	walletCount := make(map[string]int)
	for _, sc := range strategies {
		if sc.CapitalPct > 0 {
			walletCount[sc.Platform]++
		}
	}
	var platforms []string
	for plat, n := range walletCount {
		if n > 1 {
			platforms = append(platforms, plat)
		}
	}
	sort.Strings(platforms)
	return platforms
}

// ClearLatchedKillSwitchSharedWallet auto-clears a latched portfolio kill
// switch on startup when a shared wallet is in use AND the real on-chain
// balance can be successfully fetched for every shared-wallet platform. This
// protects against legacy state where an inflated PortfolioRisk.PeakValue
// (e.g. from earlier shared-wallet double-counting) would otherwise leave the
// kill switch latched forever across restarts. See issue #244.
//
// Guards (all must hold):
//   - the kill switch must currently be active (otherwise no-op)
//   - at least one platform must host a shared wallet (capital_pct > 0 with
//     more than one strategy on the same platform)
//   - fetcher must successfully return a real balance for EVERY shared-wallet
//     platform — any network/config failure preserves the kill switch so the
//     re-baselined peak reflects the full portfolio-wide truth rather than a
//     partial slice that would under-baseline PeakValue
//
// On success, PortfolioRisk.PeakValue is re-baselined to the verified total
// balance (and CurrentDrawdownPct zeroed) so the very next CheckPortfolioRisk
// call cannot immediately re-latch the kill switch using a stale inflated
// peak — the original root cause from #244.
//
// CONCURRENCY: This function mutates state.PortfolioRisk without holding any
// lock. It is only safe to call during startup, before the state mutex is
// created and before any goroutines are spawned. See main.go:109.
//
// Returns true iff the kill switch was cleared.
func ClearLatchedKillSwitchSharedWallet(state *AppState, strategies []StrategyConfig, fetcher SharedWalletBalanceFetcher) bool {
	if state == nil || !state.PortfolioRisk.KillSwitchActive {
		return false
	}

	sharedPlatforms := detectSharedWalletPlatforms(strategies)
	if len(sharedPlatforms) == 0 {
		return false
	}

	// Fetch every shared-wallet platform up front. Any failure aborts the
	// clear so we never re-baseline PeakValue from an incomplete picture.
	totalBalance := 0.0
	for _, plat := range sharedPlatforms {
		balance, err := fetcher(plat)
		if err != nil {
			fmt.Printf("[INFO] Shared wallet (%s): kill switch NOT cleared — balance fetch failed: %v\n", plat, err)
			return false
		}
		totalBalance += balance
	}

	latchedAt := state.PortfolioRisk.KillSwitchAt.Format("2006-01-02 15:04 UTC")
	fmt.Printf("[INFO] Shared wallet (%v): clearing kill switch (was latched at %s, real total balance=$%.2f, prior peak=$%.2f)\n",
		sharedPlatforms, latchedAt, totalBalance, state.PortfolioRisk.PeakValue)

	state.PortfolioRisk.KillSwitchActive = false
	state.PortfolioRisk.KillSwitchAt = time.Time{}
	state.PortfolioRisk.WarningSent = false
	// Re-baseline peak to the verified on-chain total so CheckPortfolioRisk
	// does not immediately re-latch on the first tick using the stale
	// (potentially double-counted) peak.
	state.PortfolioRisk.PeakValue = totalBalance
	state.PortfolioRisk.CurrentDrawdownPct = 0
	addKillSwitchEvent(&state.PortfolioRisk, "auto_reset", "",
		0, totalBalance, totalBalance,
		fmt.Sprintf("startup auto-clear: shared wallets %v reachable, total balance=$%.2f (peak re-baselined)",
			sharedPlatforms, totalBalance))
	return true
}

// addKillSwitchEvent appends an event and trims to maxKillSwitchEvents.
//
// source identifies which drawdown signal drove the event: "equity", "margin",
// or "" (unknown / signal-agnostic). For "triggered" and "warning" events it
// must be set; for "reset" / "auto_reset" it is typically empty. DrawdownPct
// is interpreted as a pct of the signal named by source — do not pass
// max(equity, margin) here; pass the value for the specific source, otherwise
// the event log becomes arithmetically inconsistent.
func addKillSwitchEvent(prs *PortfolioRiskState, eventType, source string, drawdownPct, portfolioValue, peakValue float64, details string) {
	prs.Events = append(prs.Events, KillSwitchEvent{
		Timestamp:      time.Now().UTC(),
		Type:           eventType,
		Source:         source,
		DrawdownPct:    drawdownPct,
		PortfolioValue: portfolioValue,
		PeakValue:      peakValue,
		Details:        details,
	})
	if len(prs.Events) > maxKillSwitchEvents {
		prs.Events = prs.Events[len(prs.Events)-maxKillSwitchEvents:]
	}
}

// AggregatePerpsMarginInputs sums unrealized loss and deployed margin across
// every perps strategy in the portfolio. It returns the numerator and
// denominator inputs of the drawdown ratio (not a ratio itself) — matches the
// per-strategy counterpart perpsMarginDrawdownInputs (#292), aggregated to the
// portfolio level for the kill switch (#296).
//
// Only strategies with Type == "perps" contribute; the inner filter (Multiplier
// > 0 && Leverage > 0) inside perpsMarginDrawdownInputs is the second guard
// against any hypothetical non-perps leveraged position leaking through.
//
// Returns (0, 0) when no perps margin is deployed — the caller treats a zero
// margin as "no perps signal this cycle" and falls back to pure equity
// drawdown. This preserves existing behavior for all-spot / all-options
// portfolios.
func AggregatePerpsMarginInputs(strategies map[string]*StrategyState, prices map[string]float64) (unrealizedLoss, margin float64) {
	for _, s := range strategies {
		if s.Type != "perps" {
			continue
		}
		loss, m := perpsMarginDrawdownInputs(s, prices)
		unrealizedLoss += loss
		margin += m
	}
	return unrealizedLoss, margin
}

// CheckPortfolioRisk evaluates aggregate portfolio risk.
// Returns (allowed, notionalBlocked, warning, reason).
// allowed=false means the kill switch has fired or is latched; notionalBlocked=true
// means new trades should be skipped but existing positions kept; warning=true
// means drawdown is approaching the kill switch threshold.
//
// Two independent drawdown signals feed the kill switch:
//
//  1. Equity drawdown — (peak - totalValue) / peak. Captures spot/options
//     PnL and overall cash erosion. Persisted as CurrentDrawdownPct.
//  2. Perps margin drawdown (#296) — perpsUnrealizedLoss / perpsMargin.
//     Captures leveraged-position losses against deployed margin, which a
//     pure equity view understates dramatically for all-perps accounts: a
//     50% loss on 10x margin shows up as ~5% of total account value, so the
//     equity-only kill switch fires far too late (or not at all before
//     liquidation). Persisted as CurrentMarginDrawdownPct.
//
// The two signals live on separate fields so (PeakValue, CurrentDrawdownPct)
// remains an arithmetically consistent equity tuple for post-incident review.
// The kill switch fires on whichever signal breaches cfg.MaxDrawdownPct
// first, so a mixed portfolio is guarded on both fronts. For all-perps
// accounts, the margin signal dominates; for all-spot/options, the margin
// inputs are zero and behavior is identical to the pre-#296 baseline.
//
// The emitted KillSwitchEvent.Source records whether equity or margin drove
// the fire/warning so operators can tell at a glance which lever tripped.
func CheckPortfolioRisk(prs *PortfolioRiskState, cfg *PortfolioRiskConfig, totalValue, totalNotional, perpsUnrealizedLoss, perpsMargin float64) (allowed, notionalBlocked, warning bool, reason string) {
	if prs.KillSwitchActive {
		return false, false, false, fmt.Sprintf("portfolio kill switch is latched (triggered at %s, manual reset required)",
			prs.KillSwitchAt.Format("2006-01-02 15:04:05 UTC"))
	}

	// Ratchet peak high-water mark upward only.
	if totalValue > prs.PeakValue {
		prs.PeakValue = totalValue
	}

	// Compute both drawdown signals independently. Each is persisted to its
	// own field so (PeakValue, CurrentDrawdownPct) stays internally consistent
	// and operators can see both lenses at once.
	var equityDD, marginDD float64
	if prs.PeakValue > 0 {
		equityDD = (prs.PeakValue - totalValue) / prs.PeakValue * 100
		if equityDD < 0 {
			equityDD = 0
		}
		prs.CurrentDrawdownPct = equityDD
	}
	if perpsMargin > 0 && perpsUnrealizedLoss > 0 {
		marginDD = perpsUnrealizedLoss / perpsMargin * 100
	}
	prs.CurrentMarginDrawdownPct = marginDD

	// Kill switch: fire if either signal breaches the limit. The reason names
	// the breaching signal so operators know whether to investigate spot /
	// options equity or perps margin.
	//
	// Note: this branch runs even when PeakValue == 0, so a cold-start
	// account that blows up margin on bar 1 (before any equity snapshot) is
	// still protected — equityDD is zero in that case and only the margin
	// signal can fire.
	if equityDD > cfg.MaxDrawdownPct || marginDD > cfg.MaxDrawdownPct {
		prs.KillSwitchActive = true
		prs.KillSwitchAt = time.Now().UTC()
		var r, source string
		var dd float64
		// Tie-break to margin when the two signals are equal: the margin
		// signal is the newer, more sensitive lens (#296) and surfacing it
		// preferentially helps operators notice leveraged blow-ups.
		if marginDD >= equityDD {
			source = "margin"
			dd = marginDD
			r = fmt.Sprintf("portfolio perps margin drawdown %.1f%% exceeds limit %.1f%% (unrealized loss=$%.2f, margin=$%.2f, value=$%.2f, peak=$%.2f)",
				marginDD, cfg.MaxDrawdownPct, perpsUnrealizedLoss, perpsMargin, totalValue, prs.PeakValue)
		} else {
			source = "equity"
			dd = equityDD
			r = fmt.Sprintf("portfolio drawdown %.1f%% exceeds limit %.1f%% (value=$%.2f, peak=$%.2f)",
				equityDD, cfg.MaxDrawdownPct, totalValue, prs.PeakValue)
		}
		addKillSwitchEvent(prs, "triggered", source, dd, totalValue, prs.PeakValue, r)
		return false, false, false, r
	}

	// Warning check: approaching kill switch threshold on either signal.
	if cfg.MaxDrawdownPct > 0 {
		warnDrawdownPct := cfg.MaxDrawdownPct * cfg.WarnThresholdPct / 100
		equityWarn := equityDD > warnDrawdownPct
		marginWarn := marginDD > warnDrawdownPct
		if equityWarn || marginWarn {
			if !prs.WarningSent {
				prs.WarningSent = true
				warning = true
				switch {
				case equityWarn && marginWarn:
					// Both breached — surface both in the reason so a
					// correlated move is visible to the operator. Ties go
					// to margin (see kill-switch branch above).
					reason = fmt.Sprintf("portfolio drawdown approaching kill switch limit %.1f%% (warn at %.1f%%): equity=%.1f%% (value=$%.2f, peak=$%.2f); perps margin=%.1f%% (unrealized loss=$%.2f, margin=$%.2f)",
						cfg.MaxDrawdownPct, warnDrawdownPct, equityDD, totalValue, prs.PeakValue, marginDD, perpsUnrealizedLoss, perpsMargin)
				case marginWarn:
					reason = fmt.Sprintf("portfolio perps margin drawdown %.1f%% approaching kill switch limit %.1f%% (warn at %.1f%%, unrealized loss=$%.2f, margin=$%.2f)",
						marginDD, cfg.MaxDrawdownPct, warnDrawdownPct, perpsUnrealizedLoss, perpsMargin)
				default:
					reason = fmt.Sprintf("portfolio drawdown %.1f%% approaching kill switch limit %.1f%% (warn at %.1f%%, value=$%.2f, peak=$%.2f)",
						equityDD, cfg.MaxDrawdownPct, warnDrawdownPct, totalValue, prs.PeakValue)
				}
			}
		} else {
			// Recovered below warning threshold — reset so it can fire again.
			prs.WarningSent = false
		}
	}

	// Check notional cap — blocks new trades but does not force-close.
	if cfg.MaxNotionalUSD > 0 && totalNotional > cfg.MaxNotionalUSD {
		return true, true, warning, fmt.Sprintf("portfolio notional $%.2f exceeds cap $%.2f — new trades blocked",
			totalNotional, cfg.MaxNotionalUSD)
	}

	return true, false, warning, reason
}

// PortfolioNotional computes gross market exposure across all strategies.
// Spot: quantity * price. Options sold: strike * quantity (max obligation).
// Options bought: CurrentValueUSD if positive.
func PortfolioNotional(strategies map[string]*StrategyState, prices map[string]float64) float64 {
	total := 0.0
	for _, s := range strategies {
		for sym, pos := range s.Positions {
			price, ok := prices[sym]
			if !ok {
				price = pos.AvgCost
			}
			if pos.Multiplier > 0 {
				total += pos.Quantity * pos.Multiplier * price
			} else {
				total += pos.Quantity * price
			}
		}
		for _, opt := range s.OptionPositions {
			if opt.Action == "sell" {
				total += opt.Strike * opt.Quantity
			} else if opt.CurrentValueUSD > 0 {
				total += opt.CurrentValueUSD
			}
		}
	}
	return total
}

// RiskState tracks risk metrics for a strategy.
type RiskState struct {
	PeakValue           float64   `json:"peak_value"`
	MaxDrawdownPct      float64   `json:"max_drawdown_pct"`
	CurrentDrawdownPct  float64   `json:"current_drawdown_pct"`
	DailyPnL            float64   `json:"daily_pnl"`
	DailyPnLDate        string    `json:"daily_pnl_date"`
	ConsecutiveLosses   int       `json:"consecutive_losses"`
	CircuitBreaker      bool      `json:"circuit_breaker"`
	CircuitBreakerUntil time.Time `json:"circuit_breaker_until"`
	TotalTrades         int       `json:"total_trades"`
	WinningTrades       int       `json:"winning_trades"`
	LosingTrades        int       `json:"losing_trades"`
	// PendingCircuitCloses holds venue-appropriate reduce-only / flatten close
	// requests queued by per-strategy circuit breakers, keyed by platform string.
	// The key MUST match StrategyConfig.Platform ("hyperliquid", "okx",
	// "topstep", "robinhood") — not the strategy-ID prefix (hl-/ts-/rh-/okx-)
	// and not an ad-hoc label — so the drain runners can correlate pending
	// entries with live strategies by platform. Use the PlatformPendingClose*
	// constants when setting or reading entries. Serialized to SQLite as
	// risk_pending_circuit_closes_json. Drained out-of-lock by platform-specific
	// runners (e.g. runPendingHyperliquidCircuitCloses for "hyperliquid").
	//
	// Generalized from the HL-specific PendingHyperliquidCircuitClose field in
	// #359 phase 1b. The per-platform drain code interprets the symbol/size
	// pairs according to its API; HL uses coin name + base-unit size, other
	// venues will use their own identifier conventions (phases 2-4).
	PendingCircuitCloses map[string]*PendingCircuitClose `json:"pending_circuit_closes,omitempty"`
}

// PlatformPendingCloseHyperliquid is the map key in RiskState.PendingCircuitCloses
// for Hyperliquid perps closes. Other platform constants land alongside their
// phase PRs (#360 OKX, #361 RH, #362 TS).
const PlatformPendingCloseHyperliquid = "hyperliquid"

// PendingCircuitClose is a queued request to close one or more positions on a
// single venue after a per-strategy circuit breaker fired. The drain runner
// for that venue (platform key in RiskState.PendingCircuitCloses) translates
// the symbol/size legs into venue-specific orders.
type PendingCircuitClose struct {
	Symbols []PendingCircuitCloseSymbol `json:"symbols"`
}

// PendingCircuitCloseSymbol is one position leg of a pending close. Symbol is
// venue-specific (e.g. HL coin "ETH", OKX inst_id "BTC-USDT-SWAP", TS
// contract "ESM25"). Size is a positive magnitude; units are venue-specific
// (coin units for HL, contracts for TS, quote-currency amount for OKX).
type PendingCircuitCloseSymbol struct {
	Symbol string  `json:"symbol"`
	Size   float64 `json:"size"`
}

// PlatformRiskAssist carries pre-fetched venue state that
// setCircuitBreakerPending helpers need to size per-strategy on-chain closes
// when a CB fires. Nil fields disable pending enqueue for that platform; the
// drain runner's stuck-CB recovery path then re-enqueues once the fetch
// succeeds on a later cycle (#356).
//
// Only HL fields are populated today (#359 phase 1b generalizes HLRiskAssist).
// Phases 2-4 will add OKX / TopStep / Robinhood fields as their per-strategy
// close plumbing lands.
type PlatformRiskAssist struct {
	HLPositions []HLPosition
	HLLiveAll   []StrategyConfig
}

// MarshalPendingCircuitClosesJSON returns a DB-safe JSON blob for the pending
// field. A marshal error is logged loudly rather than silently swallowed: the
// map-of-struct payload is essentially unreachable for json.Marshal failures,
// but silently returning "" would persist a blank column that wipes pending
// closes on reload. Logging gives operators a chance to notice (#356 review).
func (r *RiskState) MarshalPendingCircuitClosesJSON() string {
	if r == nil || len(r.PendingCircuitCloses) == 0 {
		return ""
	}
	// Drop platforms whose pending payload has no legs — persisting
	// {"hyperliquid":{"symbols":[]}} is noise and makes reload ambiguous.
	filtered := make(map[string]*PendingCircuitClose, len(r.PendingCircuitCloses))
	for k, v := range r.PendingCircuitCloses {
		if v == nil || len(v.Symbols) == 0 {
			continue
		}
		filtered[k] = v
	}
	if len(filtered) == 0 {
		return ""
	}
	b, err := json.Marshal(filtered)
	if err != nil {
		fmt.Printf("[CRITICAL] MarshalPendingCircuitClosesJSON: refusing to persist pending circuit closes — json.Marshal failed: %v (pending=%+v)\n",
			err, filtered)
		return ""
	}
	return string(b)
}

// UnmarshalPendingCircuitClosesJSON restores PendingCircuitCloses from DB.
//
// Accepts two JSON shapes for backwards-compatibility with rows written by
// pre-#359 (#356) builds:
//
//  1. New map shape: {"hyperliquid":{"symbols":[{"symbol":"ETH","size":0.1}]}}
//  2. Legacy HL-only shape: {"coins":[{"coin":"ETH","sz":0.1}]} — transparently
//     converted to {"hyperliquid":{"symbols":[...]}} on first load. Subsequent
//     saves write the new shape, so the DB self-heals within one cycle.
func (r *RiskState) UnmarshalPendingCircuitClosesJSON(raw string) {
	if r == nil {
		return
	}
	if raw == "" {
		r.PendingCircuitCloses = nil
		return
	}

	// Try new map shape first.
	var asMap map[string]*PendingCircuitClose
	if err := json.Unmarshal([]byte(raw), &asMap); err == nil {
		filtered := make(map[string]*PendingCircuitClose, len(asMap))
		for k, v := range asMap {
			if v == nil || len(v.Symbols) == 0 {
				continue
			}
			filtered[k] = v
		}
		if len(filtered) > 0 {
			r.PendingCircuitCloses = filtered
			return
		}
	}

	// Legacy shape fallback: {"coins":[{"coin":"ETH","sz":0.1}]} from #356.
	// json.Unmarshal into map[string]*PendingCircuitClose errors out on the
	// legacy payload (the "coins" value is an array, which cannot decode into
	// a *PendingCircuitClose), so the new-shape attempt above returns non-nil
	// err and execution falls through here.
	var legacy struct {
		Coins []struct {
			Coin string  `json:"coin"`
			Sz   float64 `json:"sz"`
		} `json:"coins"`
	}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil || len(legacy.Coins) == 0 {
		r.PendingCircuitCloses = nil
		return
	}
	symbols := make([]PendingCircuitCloseSymbol, 0, len(legacy.Coins))
	for _, c := range legacy.Coins {
		symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: c.Coin, Size: c.Sz})
	}
	r.PendingCircuitCloses = map[string]*PendingCircuitClose{
		PlatformPendingCloseHyperliquid: {Symbols: symbols},
	}
}

// setPendingCircuitClose stores a pending close for the given platform,
// creating the map on first use. Passing nil or an empty-symbols close deletes
// the platform entry instead of storing an empty shell.
func (r *RiskState) setPendingCircuitClose(platform string, pending *PendingCircuitClose) {
	if r == nil {
		return
	}
	if pending == nil || len(pending.Symbols) == 0 {
		delete(r.PendingCircuitCloses, platform)
		if len(r.PendingCircuitCloses) == 0 {
			r.PendingCircuitCloses = nil
		}
		return
	}
	if r.PendingCircuitCloses == nil {
		r.PendingCircuitCloses = make(map[string]*PendingCircuitClose)
	}
	r.PendingCircuitCloses[platform] = pending
}

// clearPendingCircuitClose removes the pending entry for a platform, if any.
func (r *RiskState) clearPendingCircuitClose(platform string) {
	if r == nil {
		return
	}
	delete(r.PendingCircuitCloses, platform)
	if len(r.PendingCircuitCloses) == 0 {
		r.PendingCircuitCloses = nil
	}
}

// getPendingCircuitClose returns the pending entry for a platform, or nil if
// none is queued.
func (r *RiskState) getPendingCircuitClose(platform string) *PendingCircuitClose {
	if r == nil {
		return nil
	}
	return r.PendingCircuitCloses[platform]
}

func setHyperliquidCircuitBreakerPending(sc *StrategyConfig, s *StrategyState, assist *PlatformRiskAssist) {
	if sc == nil || assist == nil || len(assist.HLPositions) == 0 {
		return
	}
	if sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
		return
	}
	sym := hyperliquidSymbol(sc.Args)
	if sym == "" {
		return
	}
	if _, ok := s.Positions[sym]; !ok {
		return
	}
	qty, ok := computeHyperliquidCircuitCloseQty(sym, s.ID, assist.HLPositions, assist.HLLiveAll)
	if !ok || qty <= 0 {
		return
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingCloseHyperliquid, &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: sym, Size: qty}},
	})
}

// rolloverDailyPnL resets DailyPnL to zero whenever the UTC date has advanced
// past DailyPnLDate. Calling this at both risk-check time and trade-record time
// ensures the reset is applied regardless of which code path runs first after
// midnight — fixing issue #27 where a skipped or late risk check could cause
// trades to be counted against the wrong day.
func rolloverDailyPnL(r *RiskState) {
	today := time.Now().UTC().Format("2006-01-02")
	if r.DailyPnLDate != today {
		r.DailyPnL = 0
		r.DailyPnLDate = today
	}
}

// forceCloseAllPositions liquidates all open positions at current prices.
// Called when any circuit breaker fires.
func forceCloseAllPositions(s *StrategyState, prices map[string]float64, logger *StrategyLogger) {
	now := time.Now().UTC()

	for symbol, pos := range s.Positions {
		price, ok := prices[symbol]
		if !ok {
			price = pos.AvgCost
		}
		var pnl, value float64
		tradeType := "spot"
		if pos.Multiplier > 0 {
			// Futures: PnL-based (contracts * multiplier * price delta)
			tradeType = "futures"
			if pos.Side == "long" {
				pnl = pos.Quantity * pos.Multiplier * (price - pos.AvgCost)
			} else {
				pnl = pos.Quantity * pos.Multiplier * (pos.AvgCost - price)
			}
			s.Cash += pnl
			value = pos.Quantity * pos.Multiplier * price
		} else if pos.Side == "long" {
			proceeds := pos.Quantity * price
			pnl = proceeds - pos.Quantity*pos.AvgCost
			s.Cash += proceeds
			value = proceeds
		} else {
			pnl = pos.Quantity * (pos.AvgCost - price)
			s.Cash += pos.Quantity*pos.AvgCost - pos.Quantity*price
			value = pos.Quantity * price
		}
		if logger != nil {
			logger.Warn("Circuit breaker: force-closing %s %s @ $%.2f (PnL: $%.2f)", pos.Side, symbol, price, pnl)
		}
		trade := Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     symbol,
			Side:       "close",
			Quantity:   pos.Quantity,
			Price:      price,
			Value:      value,
			TradeType:  tradeType,
			Details:    fmt.Sprintf("Circuit breaker force-close, PnL: $%.2f", pnl),
		}
		RecordTrade(s, trade)
		RecordTradeResult(&s.RiskState, pnl)
		recordClosedPosition(s, pos, price, pnl, "circuit_breaker", now)
		delete(s.Positions, symbol)
	}

	for id, pos := range s.OptionPositions {
		var pnl, closePrice float64
		if pos.Action == "buy" {
			pnl = pos.CurrentValueUSD - pos.EntryPremiumUSD
			s.Cash += pos.CurrentValueUSD
			closePrice = pos.CurrentValueUSD
		} else {
			buybackCost := -pos.CurrentValueUSD
			pnl = pos.EntryPremiumUSD - buybackCost
			s.Cash -= buybackCost
			closePrice = buybackCost
		}
		if logger != nil {
			logger.Warn("Circuit breaker: force-closing %s %s @ $%.2f (PnL: $%.2f)", pos.Action, id, closePrice, pnl)
		}
		trade := Trade{
			Timestamp:  now,
			StrategyID: s.ID,
			Symbol:     id,
			Side:       "close",
			Quantity:   pos.Quantity,
			Price:      closePrice,
			Value:      closePrice,
			TradeType:  "options",
			Details:    fmt.Sprintf("Circuit breaker force-close, PnL: $%.2f", pnl),
		}
		RecordTrade(s, trade)
		RecordTradeResult(&s.RiskState, pnl)
		recordClosedOptionPosition(s, pos, closePrice, pnl, "circuit_breaker", now)
		delete(s.OptionPositions, id)
	}
}

// perpsMarginDrawdownInputs iterates open perps positions and returns the sum
// of unrealized losses (positive number; gains clamp to zero) and the sum of
// deployed margin (notional / leverage). These are the numerator and
// denominator of the perps-specific drawdown ratio introduced in #292.
//
// Positions are filtered by Leverage > 0; Multiplier > 0 is also required as
// a double-belt guard against any future code path that might attach Leverage
// to a non-PnL-branch position. The outer s.Type == "perps" check at the call
// site is the primary guard.
//
// The unrealized-loss numerator (rather than peakValue - portfolioValue) keeps
// the drawdown ratio referenced to the currently-open position: prior realized
// losses that already live in Cash below the high-water mark do NOT inflate
// drawdown against a fresh small position's margin. See #292 code review.
//
// Mark price falls back to AvgCost when missing or non-positive so numerator
// and denominator share the same basis as PortfolioValue's valuation.
//
// Returns (0, 0) when no perps positions are open; the caller uses a zero
// margin as the signal to fall back to peak-relative drawdown.
func perpsMarginDrawdownInputs(s *StrategyState, prices map[string]float64) (unrealizedLoss, margin float64) {
	for sym, pos := range s.Positions {
		if pos.Multiplier <= 0 || pos.Leverage <= 0 {
			continue
		}
		price, ok := prices[sym]
		if !ok || price <= 0 {
			price = pos.AvgCost
		}
		if price <= 0 {
			continue
		}
		notional := pos.Quantity * price
		if notional <= 0 {
			continue
		}
		margin += notional / pos.Leverage

		var pnl float64
		if pos.Side == "long" {
			pnl = pos.Quantity * pos.Multiplier * (price - pos.AvgCost)
		} else {
			pnl = pos.Quantity * pos.Multiplier * (pos.AvgCost - price)
		}
		if pnl < 0 {
			unrealizedLoss += -pnl
		}
	}
	return unrealizedLoss, margin
}

// CheckRisk evaluates risk state and returns whether trading is allowed.
// sc is the strategy config for this state (nil in some tests — platform
// pending logic is skipped). assist carries pre-fetched per-platform state
// (HL clearinghouse positions today; OKX/TS/RH in later phases) so live
// strategies can enqueue on-chain closes on circuit breaker (#356 / #359).
func CheckRisk(sc *StrategyConfig, s *StrategyState, portfolioValue float64, prices map[string]float64, logger *StrategyLogger, assist *PlatformRiskAssist) (bool, string) {
	r := &s.RiskState
	now := time.Now().UTC()

	rolloverDailyPnL(r)

	// Check circuit breaker
	if r.CircuitBreaker {
		if now.Before(r.CircuitBreakerUntil) {
			return false, "circuit breaker active"
		}
		r.CircuitBreaker = false
		r.ConsecutiveLosses = 0
	}

	// Update peak
	if portfolioValue > r.PeakValue {
		r.PeakValue = portfolioValue
	}

	// Check drawdown.
	//
	// For perps strategies with open leveraged positions, drawdown is measured
	// as unrealized loss on currently-open positions divided by deployed margin
	// (capital at risk). A 20x leveraged position only puts ~5% of notional at
	// risk as margin; using the full portfolio as denominator with peak-relative
	// numerator under-states near-100% margin losses as a few-percent drawdown,
	// so the circuit breaker would only fire after the position had already been
	// liquidated. See #292.
	//
	// Referencing the numerator to unrealized PnL on *currently-open* positions
	// (rather than peak - portfolioValue, which is cumulative from the
	// high-water mark) keeps prior realized losses from inflating drawdown
	// against a freshly opened position's margin. A strategy that has taken
	// past losses but just opened a small untouched position should not fire.
	//
	// When the strategy has no perps margin deployed (all positions closed,
	// leverage unset, or non-perps type), we fall back to the classic
	// peak-relative drawdown so strategies without leverage behave identically
	// to before.
	if r.PeakValue > 0 {
		loss := r.PeakValue - portfolioValue
		denom := r.PeakValue
		denomLabel := "peak"
		if s.Type == "perps" {
			if pnlLoss, margin := perpsMarginDrawdownInputs(s, prices); margin > 0 {
				loss = pnlLoss
				denom = margin
				denomLabel = "margin"
			}
		}
		if loss < 0 {
			loss = 0
		}
		if denom > 0 {
			r.CurrentDrawdownPct = (loss / denom) * 100
		} else {
			r.CurrentDrawdownPct = 0
		}
		if r.TotalTrades > 0 && r.CurrentDrawdownPct > r.MaxDrawdownPct {
			r.CircuitBreaker = true
			r.CircuitBreakerUntil = now.Add(24 * time.Hour)
			setHyperliquidCircuitBreakerPending(sc, s, assist)
			forceCloseAllPositions(s, prices, logger)
			return false, fmt.Sprintf("max drawdown exceeded (%.1f%% > %.1f%%, portfolio=$%.2f peak=$%.2f, denom=%s=$%.2f)",
				r.CurrentDrawdownPct, r.MaxDrawdownPct, portfolioValue, r.PeakValue, denomLabel, denom)
		}
	}

	// Consecutive losses circuit breaker (5 in a row → pause 1h, close positions)
	if r.ConsecutiveLosses >= 5 {
		r.CircuitBreaker = true
		r.CircuitBreakerUntil = now.Add(1 * time.Hour)
		setHyperliquidCircuitBreakerPending(sc, s, assist)
		forceCloseAllPositions(s, prices, logger)
		return false, "5 consecutive losses"
	}

	return true, ""
}

// RecordTradeResult updates risk state with trade outcome.
func RecordTradeResult(r *RiskState, pnl float64) {
	rolloverDailyPnL(r)
	r.TotalTrades++
	r.DailyPnL += pnl
	if pnl >= 0 {
		r.WinningTrades++
		r.ConsecutiveLosses = 0
	} else {
		r.LosingTrades++
		r.ConsecutiveLosses++
	}
}
