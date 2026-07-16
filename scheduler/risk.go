package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// schedulerStarted is set once in main immediately before server.Start — the
// first spawn that can read PortfolioRisk under mu. ClearLatchedKillSwitchSharedWallet
// may only run while this is false (#1272).
var schedulerStarted atomic.Bool

// markSchedulerStarted ends the single-threaded startup phase. Call exactly
// once immediately before server.Start (or any other goroutine that reads
// AppState under mu).
func markSchedulerStarted() {
	schedulerStarted.Store(true)
}

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
//
// #1159: a hedge-enabled strategy's hedge coin is included in hlCoins
// unconditionally (not only while a hedge leg is held) — hedgeTargetDecision
// needs a live hedgePx to size the FIRST hedge open, and PortfolioValue/
// exposure math must never fall back to AvgCost for a held hedge leg.
// hedgeCollisionErrors guarantees this coin is never any strategy's own
// configured coin, so it can't already be in the set from another entry.
func collectPerpsMarkSymbols(strategies []StrategyConfig) (hlCoins, okxCoins []string) {
	hlSet := make(map[string]bool)
	okxSet := make(map[string]bool)
	for _, sc := range strategies {
		if sc.Type != "perps" {
			continue
		}
		if HedgeEnabled(sc) {
			if coin := hedgeCoin(sc); coin != "" {
				hlSet[coin] = true
			}
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
// WarningSent is retained for persisted status visibility and is true while
// either drawdown signal is in the warning band; notifications are emitted on
// every cycle in that band.
type PortfolioRiskState struct {
	PeakValue                float64           `json:"peak_value"`
	CurrentDrawdownPct       float64           `json:"current_drawdown_pct"`
	CurrentMarginDrawdownPct float64           `json:"current_margin_drawdown_pct,omitempty"`
	KillSwitchActive         bool              `json:"kill_switch_active"`
	KillSwitchAt             time.Time         `json:"kill_switch_at,omitempty"`
	WarningSent              bool              `json:"warning_sent,omitempty"`
	WarnBandEnteredAt        time.Time         `json:"warn_band_entered_at,omitempty"`
	LastWarningEquityDDPct   float64           `json:"last_warning_equity_dd_pct,omitempty"`
	LastWarningMarginDDPct   float64           `json:"last_warning_margin_dd_pct,omitempty"`
	WarningEquityDeltaPct    float64           `json:"warning_equity_delta_pct,omitempty"`
	WarningMarginDeltaPct    float64           `json:"warning_margin_delta_pct,omitempty"`
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
// balance (and drawdown fields zeroed) so the very next CheckPortfolioRisk
// call cannot immediately re-latch the kill switch using a stale inflated
// peak — the original root cause from #244.
//
// CONCURRENCY: This function mutates state.PortfolioRisk without holding any
// lock. It is only safe during the single-threaded startup phase — before
// markSchedulerStarted(). Calling it after that panics (#1272); do not hold
// mu across the balance fetcher I/O inside this helper.
//
// Returns true iff the kill switch was cleared.
func ClearLatchedKillSwitchSharedWallet(state *AppState, strategies []StrategyConfig, fetcher SharedWalletBalanceFetcher) bool {
	if schedulerStarted.Load() {
		panic("ClearLatchedKillSwitchSharedWallet called after scheduler started")
	}
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
	state.PortfolioRisk.WarnBandEnteredAt = time.Time{}
	state.PortfolioRisk.LastWarningEquityDDPct = 0
	state.PortfolioRisk.LastWarningMarginDDPct = 0
	state.PortfolioRisk.WarningEquityDeltaPct = 0
	state.PortfolioRisk.WarningMarginDeltaPct = 0
	// Re-baseline peak to the verified on-chain total so CheckPortfolioRisk
	// does not immediately re-latch on the first tick using the stale
	// (potentially double-counted) peak.
	state.PortfolioRisk.PeakValue = totalBalance
	state.PortfolioRisk.CurrentDrawdownPct = 0
	state.PortfolioRisk.CurrentMarginDrawdownPct = 0
	addKillSwitchEvent(&state.PortfolioRisk, "auto_reset", "",
		0, totalBalance, totalBalance,
		fmt.Sprintf("startup auto-clear: shared wallets %v reachable, total balance=$%.2f (peak re-baselined)",
			sharedPlatforms, totalBalance))
	return true
}

// AutoResetConfirmedFlatKillSwitch clears a portfolio kill-switch latch after
// live close planning has confirmed all automated venues are flat. This is used
// only when no DM-capable owner is configured; owner-backed deployments keep the
// existing human-in-the-loop reset path.
//
// rebaselineValue is the best available estimate for post-close portfolio
// value. The hot loop typically passes the pre-close mark-to-market totalPV,
// which closely approximates post-close cash apart from fees and slippage.
//
// Note: callers should suppress this auto-reset when the close plan has
// operator-required gaps such as OKX spot or Robinhood options. Those venues do
// not block OnChainConfirmedFlat because there is no safe automated close path,
// but resuming trading without a human reset would hide remaining live exposure.
//
// CONCURRENCY: lock-free body — the caller must hold mu while invoking this
// (hot-loop site in main does). Unlike ClearLatchedKillSwitchSharedWallet,
// this helper is intended for post-startup use under the state lock.
func AutoResetConfirmedFlatKillSwitch(prs *PortfolioRiskState, rebaselineValue float64, details string) bool {
	if prs == nil || !prs.KillSwitchActive {
		return false
	}

	prevEquityDrawdownPct := prs.CurrentDrawdownPct
	prevMarginDrawdownPct := prs.CurrentMarginDrawdownPct
	if details != "" {
		details = fmt.Sprintf("%s (previous equity drawdown=%.2f%%, previous margin drawdown=%.2f%%)",
			details, prevEquityDrawdownPct, prevMarginDrawdownPct)
	}

	prs.KillSwitchActive = false
	prs.KillSwitchAt = time.Time{}
	prs.WarningSent = false
	prs.WarnBandEnteredAt = time.Time{}
	prs.LastWarningEquityDDPct = 0
	prs.LastWarningMarginDDPct = 0
	prs.WarningEquityDeltaPct = 0
	prs.WarningMarginDeltaPct = 0
	prs.PeakValue = rebaselineValue
	prs.CurrentDrawdownPct = 0
	prs.CurrentMarginDrawdownPct = 0
	addKillSwitchEvent(prs, "auto_reset", "", 0, rebaselineValue, rebaselineValue, details)
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
// Only strategies with Type == "perps" contribute. configs maps strategy ID
// to StrategyConfig — used to source exchange sc.Leverage so the margin
// denominator matches the actual exchange leverage rather than the
// sizing_leverage order multiplier (#497). Strategies whose config is missing or has Leverage <= 0 are
// skipped; they don't contribute to the perps margin signal and the kill
// switch falls back to equity drawdown for them.
//
// Returns (0, 0) when no perps margin is deployed — the caller treats a zero
// margin as "no perps signal this cycle" and falls back to pure equity
// drawdown. This preserves existing behavior for all-spot / all-options
// portfolios.
func AggregatePerpsMarginInputs(strategies map[string]*StrategyState, configs []StrategyConfig, prices map[string]float64) (unrealizedLoss, margin float64) {
	leverageByID := make(map[string]float64, len(configs))
	for _, sc := range configs {
		leverageByID[sc.ID] = sc.Leverage
	}
	for id, s := range strategies {
		if s.Type != "perps" {
			continue
		}
		lev := leverageByID[id]
		if lev <= 0 {
			continue
		}
		loss, m := perpsMarginDrawdownInputs(s, lev, prices)
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
		prs.WarningSent = false
		prs.WarnBandEnteredAt = time.Time{}
		prs.LastWarningEquityDDPct = 0
		prs.LastWarningMarginDDPct = 0
		prs.WarningEquityDeltaPct = 0
		prs.WarningMarginDeltaPct = 0
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
			now := time.Now().UTC()
			if !prs.WarningSent {
				prs.WarnBandEnteredAt = now
				prs.WarningEquityDeltaPct = 0
				prs.WarningMarginDeltaPct = 0
			} else {
				prs.WarningEquityDeltaPct = equityDD - prs.LastWarningEquityDDPct
				prs.WarningMarginDeltaPct = marginDD - prs.LastWarningMarginDDPct
			}
			prs.LastWarningEquityDDPct = equityDD
			prs.LastWarningMarginDDPct = marginDD
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
		} else {
			// Recovered below warning threshold — no active warning band.
			prs.WarningSent = false
			prs.WarnBandEnteredAt = time.Time{}
			prs.LastWarningEquityDDPct = 0
			prs.LastWarningMarginDDPct = 0
			prs.WarningEquityDeltaPct = 0
			prs.WarningMarginDeltaPct = 0
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

// PlatformPendingCloseOKX is the map key in RiskState.PendingCircuitCloses for
// OKX perpetual swap reduce-only closes (#360 phase 2 of #357).
const PlatformPendingCloseOKX = "okx"

// PlatformPendingCloseRobinhood is the map key in RiskState.PendingCircuitCloses
// for Robinhood crypto closes (#361 phase 3). Robinhood crypto has no
// reduce-only primitive — the drain submits a full market_sell of the coin's
// on-account balance, gated on sole-ownership (only one live configured RH
// crypto strategy trading that coin on the account). Shared-coin setups
// cannot CB-close safely and are surfaced to the owner via DM instead.
const PlatformPendingCloseRobinhood = "robinhood"

// PlatformPendingCloseTopStep is the map key in RiskState.PendingCircuitCloses
// for TopStep futures closes. Size entries are integer contract counts encoded
// as float64 (PendingCircuitCloseSymbol.Size is float64 across all venues for
// storage uniformity; the TopStep drain logs the live on-account count at
// drain time — market_close has no size argument and flattens the full position).
const PlatformPendingCloseTopStep = "topstep"

// PlatformPendingCloseOKXSpot and PlatformPendingCloseRobinhoodOptions are map
// keys for per-strategy circuit-breaker closes the scheduler CANNOT auto-close
// safely (#363 phase 5, mirrors the portfolio-kill gaps from #345 / #346).
//
// OKX spot: no reduce-only semantic for asset balances; a net-close would wipe
// holdings that other strategies or the operator's manual positions rely on.
//
// Robinhood options: stock options close semantics (sell-to-close vs
// buy-to-close per leg, multi-leg spreads) are non-trivial to automate and the
// failure mode is high-risk.
//
// Pending entries under these keys carry OperatorRequired=true. The drain does
// NOT submit orders — it emits a CRITICAL warning once per cycle and leaves
// the pending intact until the operator intervenes (or the CB naturally
// resets). Deliberately distinct from "okx" / "robinhood" portfolio-kill keys
// so the auto-close drains never dequeue an operator-required entry.
const (
	PlatformPendingCloseOKXSpot          = "okx_spot"
	PlatformPendingCloseRobinhoodOptions = "robinhood_options"
)

// PendingCircuitClose is a queued request to close one or more positions on a
// single venue after a per-strategy circuit breaker fired. The drain runner
// for that venue (platform key in RiskState.PendingCircuitCloses) translates
// the symbol/size legs into venue-specific orders.
//
// When OperatorRequired is true the scheduler will not attempt an automated
// close — the venue lacks a safe reduce-only primitive or the close semantics
// are leg-aware enough that automation is unsafe (OKX spot, Robinhood options;
// #363). The drain emits a CRITICAL warning each cycle instead and leaves the
// pending populated so /status, Discord, and Telegram all surface the gap
// continuously until the operator clears it manually.
//
// ConsecutiveFailures and LastNotifiedAt track consecutive close-attempt
// failures (without any partial progress) for the throttled owner-DM alert
// added in #427. The drain increments ConsecutiveFailures on each hard error
// and resets it to 0 on any partial fill progress. The DM fires on the first
// failure, every 10th consecutive failure, or once per hour — whichever fires
// first. The counter is discarded together with the entry when the close
// fully succeeds.
type PendingCircuitClose struct {
	Symbols             []PendingCircuitCloseSymbol `json:"symbols"`
	OperatorRequired    bool                        `json:"operator_required,omitempty"`
	ConsecutiveFailures int                         `json:"consecutive_failures,omitempty"`
	LastNotifiedAt      time.Time                   `json:"last_notified_at,omitempty"`
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
// HL (#356), OKX (#360), Robinhood (#361), and TopStep (#362) fields are all
// populated today. RH fields are left unpopulated at the CheckRisk call site —
// see setRobinhoodCircuitBreakerPending for why the RH enqueue is driven
// exclusively by the drain's stuck-CB recovery path rather than at CB-fire time.
type PlatformRiskAssist struct {
	HLPositions  []HLPosition
	HLLiveAll    []StrategyConfig
	OKXPositions []OKXPosition
	OKXLiveAll   []StrategyConfig
	// RHPositions is reserved for a future main.go wiring that fetches live
	// Robinhood crypto balances once per cycle. It is intentionally left nil
	// at the CheckRisk call site today (see setRobinhoodCircuitBreakerPending
	// doc for rationale — fetching per cycle would cost a TOTP round-trip
	// even when no CB fires).
	RHPositions []RobinhoodPosition
	// RHLiveAll mirrors HLLiveAll/OKXLiveAll: every live configured Robinhood
	// crypto (Type=="spot") strategy. Left nil at the CheckRisk call site today
	// — see setRobinhoodCircuitBreakerPending.
	RHLiveAll []StrategyConfig
	// TSPositions is the pre-fetched live TopStep futures position snapshot
	// for the configured account. Populated in main.go from a once-per-cycle
	// fetch_topstep_positions.py call (#362). Empty slice with TSLiveAll set
	// is a successful fetch that found no open positions; nil slice signals
	// a fetch failure (stuck-CB path will retry).
	TSPositions []TopStepPosition
	// TSLiveAll mirrors HLLiveAll — every configured live TopStep futures
	// strategy on this scheduler. Needed by the sole-vs-shared-peer branch
	// in computeTopStepCircuitCloseQty.
	TSLiveAll []StrategyConfig
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

// setTopStepCircuitBreakerPending enqueues a reduce-only flatten request for
// the firing strategy's TopStep futures contract (#362). Sole-peer strategies
// enqueue the full on-account contract count; multi-peer shared contracts are
// skipped because TopStepX's market_close only flattens the entire on-account
// size — no safe partial-close primitive exists for whole-contract futures.
// The operator is notified via the virtual force-close (CheckRisk still calls
// forceCloseAllPositions), and manual intervention is required to split a
// shared contract.
//
// A nil or empty assist bails — same stuck-CB semantics as the HL helper:
// a fetch failure at CB fire time leaves pending nil, and the drain's
// stuck-CB recovery phase reconstructs the pending once TS is reachable.
func setTopStepCircuitBreakerPending(sc *StrategyConfig, s *StrategyState, assist *PlatformRiskAssist) {
	if sc == nil || assist == nil || len(assist.TSPositions) == 0 {
		return
	}
	if sc.Platform != "topstep" || sc.Type != "futures" || !topstepIsLive(sc.Args) {
		return
	}
	sym := topstepSymbol(sc.Args)
	if sym == "" {
		return
	}
	if _, ok := s.Positions[sym]; !ok {
		return
	}
	qty, ok := computeTopStepCircuitCloseQty(sym, s.ID, assist.TSPositions, assist.TSLiveAll)
	if !ok || qty <= 0 {
		return
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingCloseTopStep, &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: sym, Size: float64(qty)}},
	})
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
	if hyperliquidCircuitBreakerHasSharedCoin(sc, assist) {
		return
	}
	// #1159 review round 3: the primary being flat must not skip queuing the
	// hedge leg below — a hedge dangling from a prior failed closeFull
	// (primary flat, hedge still held) still needs an on-chain flatten, or
	// forceCloseAllPositions (called unconditionally by both CB arms right
	// after this function) closes it in-state only and strands it on the
	// exchange. Build symbols conditionally per leg instead of returning
	// early on the primary.
	var symbols []PendingCircuitCloseSymbol
	if _, ok := s.Positions[sym]; ok {
		if qty, ok := computeHyperliquidCircuitCloseQty(sym, s.ID, assist.HLPositions, assist.HLLiveAll); ok && qty > 0 {
			symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: sym, Size: qty})
		}
	}
	// #1159: append the hedge coin as a SECOND leg of the same pending close —
	// setPendingCircuitClose REPLACES the whole entry per platform, so both
	// legs must be built before the single call below, or a second call would
	// silently drop the primary's just-set request. computeHyperliquidCircuitCloseQty
	// treats a coin with zero hlLiveStrategiesForCoin peers (true of every
	// hedge coin, by hedgeCollisionErrors construction) as sole-owner, sizing
	// off the full on-chain qty exactly like the primary above.
	// hedgeCoinForProtection (not HedgeEnabled+hedgeCoin directly) so a leg
	// orphaned by hedge.enabled being flipped off via config edit + cold
	// restart is still discovered from persisted state (round-3 Optional).
	if hCoin := hedgeCoinForProtection(*sc, s, sym); hCoin != "" {
		if hPos, held := s.Positions[hCoin]; held && hPos != nil && hPos.HedgeFor != "" {
			if hQty, hOk := computeHyperliquidCircuitCloseQty(hCoin, s.ID, assist.HLPositions, assist.HLLiveAll); hOk && hQty > 0 {
				symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: hCoin, Size: hQty})
			}
		}
	}
	if len(symbols) == 0 {
		return
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingCloseHyperliquid, &PendingCircuitClose{
		Symbols: symbols,
	})
}

func hyperliquidCircuitBreakerHasSharedCoin(sc *StrategyConfig, assist *PlatformRiskAssist) bool {
	if sc == nil || assist == nil || sc.Platform != "hyperliquid" || sc.Type != "perps" || !hyperliquidIsLive(sc.Args) {
		return false
	}
	sym := hyperliquidSymbol(sc.Args)
	if sym == "" {
		return false
	}
	return len(hlLiveStrategiesForCoin(sym, assist.HLLiveAll)) > 1
}

func shouldForceCloseAllPositionsOnCircuitBreaker(sc *StrategyConfig, assist *PlatformRiskAssist) bool {
	return !hyperliquidCircuitBreakerHasSharedCoin(sc, assist)
}

// setOperatorRequiredCircuitBreakerPending enqueues an OperatorRequired=true
// pending close for OKX spot and Robinhood options strategies, the two live
// venues the scheduler has no safe auto-close path for (#345 / #346 / #363).
//
// Unlike setHyperliquidCircuitBreakerPending, this helper does not size the
// close — no subprocess round-trip is ever attempted, so a notional size is
// unnecessary. Size is set to the strategy's virtual position quantity (or 0
// when no virtual position exists, e.g. options strategies whose positions
// live in OptionPositions rather than Positions) purely for operator-facing
// context in the warning message.
//
// No-op when the strategy is not live, or when the strategy is not one of the
// two covered operator-gap configurations (call sites can invoke it broadly;
// the guard keeps it cheap).
func setOperatorRequiredCircuitBreakerPending(sc *StrategyConfig, s *StrategyState) {
	if sc == nil || s == nil {
		return
	}
	switch {
	case sc.Platform == "okx" && sc.Type == "spot" && okxIsLive(sc.Args):
		sym := okxSymbol(sc.Args)
		if sym == "" {
			return
		}
		var size float64
		if pos, ok := s.Positions[sym]; ok {
			size = pos.Quantity
		}
		s.RiskState.setPendingCircuitClose(PlatformPendingCloseOKXSpot, &PendingCircuitClose{
			Symbols:          []PendingCircuitCloseSymbol{{Symbol: sym, Size: size}},
			OperatorRequired: true,
		})
	case sc.Platform == "robinhood" && sc.Type == "options" && robinhoodIsLive(sc.Args):
		// Options positions live in s.OptionPositions keyed by option ID, not
		// a single underlier. Collect every open leg's ID so the operator sees
		// exactly which positions need manual close (not just the underlier).
		symbols := make([]PendingCircuitCloseSymbol, 0, len(s.OptionPositions))
		for id, op := range s.OptionPositions {
			if op == nil {
				continue
			}
			symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: id, Size: op.Quantity})
		}
		if len(symbols) == 0 {
			// No open option legs — emit a single marker entry with the
			// underlier so the operator still sees the strategy-level CB fire
			// on /status and in notifications.
			sym := robinhoodSymbol(sc.Args)
			if sym == "" {
				return
			}
			symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: sym, Size: 0})
		}
		sort.Slice(symbols, func(i, j int) bool { return symbols[i].Symbol < symbols[j].Symbol })
		s.RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhoodOptions, &PendingCircuitClose{
			Symbols:          symbols,
			OperatorRequired: true,
		})
	}
}

// setRobinhoodCircuitBreakerPending enqueues a pending full-close for a live
// Robinhood crypto strategy whose per-strategy circuit breaker fired (#361
// phase 3). Robinhood crypto has no reduce-only primitive: market_sell
// consumes the entire on-account balance for the coin. We still enqueue
// unconditionally when an on-account position exists — the sole-ownership
// gate lives in the drain (runPendingRobinhoodCircuitCloses) so that shared-
// coin setups DM the owner exactly once per fire cycle rather than silently
// stalling forever.
//
// Wiring note (important): under the current main.go wiring, `assist` is
// built from HL and OKX pre-fetches only — `assist.RHPositions` is always
// nil when CheckRisk calls this setter (see scheduler/main.go where the
// riskAssist literal sets HLPositions/HLLiveAll/OKXPositions/OKXLiveAll but
// leaves RH fields unset). This function therefore no-ops on the CB-fire
// cycle itself and relies on the drain's stuck-CB recovery path
// (runPendingRobinhoodCircuitCloses) to reconstruct the pending leg on the
// next cycle once the drain's lazy RH positions fetch succeeds. The trade-
// off is deliberate: wiring RH into CheckRisk would require a live TOTP
// round-trip every cycle (including cycles where no RH CB fires), which is
// the exact cost we are avoiding. Do not "fix" this by populating
// assist.RHPositions at the CheckRisk call site without revisiting the
// lazy-fetch design, or every cycle will pay a TOTP round-trip for an RH
// CB that fires maybe once per month.
//
// No-op also when assist is nil (defensive — same code path as the design
// above, mirroring the HL pattern).
func setRobinhoodCircuitBreakerPending(sc *StrategyConfig, s *StrategyState, assist *PlatformRiskAssist) {
	if sc == nil || assist == nil || len(assist.RHPositions) == 0 {
		return
	}
	if sc.Platform != "robinhood" || sc.Type != "spot" || !robinhoodIsLive(sc.Args) {
		return
	}
	coin := robinhoodSymbol(sc.Args)
	if coin == "" {
		return
	}
	if _, ok := s.Positions[coin]; !ok {
		return
	}
	qty := robinhoodOnAccountSize(coin, assist.RHPositions)
	if qty <= 0 {
		return
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingCloseRobinhood, &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: coin, Size: qty}},
	})
}

// robinhoodOnAccountSize returns the unsigned on-account size of a coin,
// or 0 if not found. Robinhood crypto is spot so Size is always >= 0.
func robinhoodOnAccountSize(coin string, positions []RobinhoodPosition) float64 {
	for i := range positions {
		if positions[i].Coin == coin {
			if positions[i].Size > 0 {
				return positions[i].Size
			}
			return 0
		}
	}
	return 0
}

// setOKXCircuitBreakerPending mirrors setHyperliquidCircuitBreakerPending for
// OKX perps (#360 phase 2 of #357). Bails on any nil dependency or missing
// fetched assist so the stuck-CB recovery path in runPendingOKXCircuitCloses
// can reconstruct the pending on a later cycle once OKX is reachable again.
func setOKXCircuitBreakerPending(sc *StrategyConfig, s *StrategyState, assist *PlatformRiskAssist) {
	if sc == nil || assist == nil || len(assist.OKXPositions) == 0 {
		return
	}
	if sc.Platform != "okx" || sc.Type != "perps" || !okxIsLive(sc.Args) {
		return
	}
	sym := okxSymbol(sc.Args)
	if sym == "" {
		return
	}
	if _, ok := s.Positions[sym]; !ok {
		return
	}
	qty, ok := computeOKXCircuitCloseQty(sym, s.ID, assist.OKXPositions, assist.OKXLiveAll)
	if !ok || qty <= 0 {
		return
	}
	s.RiskState.setPendingCircuitClose(PlatformPendingCloseOKX, &PendingCircuitClose{
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

// forceCloseKillSwitchPositions clears virtual positions after a confirmed
// portfolio kill-switch close. `hlFills` carries the realized Hyperliquid
// close fills (price/size/fee) so HL strategies record accurate Trade and
// ClosedPosition rows; `hlVirtualQty` is the pre-close peer snapshot used to
// split shared-coin fills by virtual quantity. Pass nil for non-HL or when no
// fill data is available.
func forceCloseKillSwitchPositions(s *StrategyState, sc StrategyConfig, prices map[string]float64, hlFills map[string]HyperliquidCloseFill, hlLiveAll []StrategyConfig, hlVirtualQty hlVirtualQuantitySnapshot, logger *StrategyLogger) {
	// Live HL portfolio-kill closes can carry real exchange fills. Apply them
	// first so Trade and ClosedPosition rows use realized fill price/fee; the
	// generic pass below remains the cleanup path for non-HL strategies,
	// missing-fill fallbacks, options, and any residual virtual positions.
	applyHyperliquidKillSwitchCloseFill(s, sc, hlFills, hlLiveAll, hlVirtualQty)
	forceCloseAllPositions(s, prices, logger)
}

// classifyPositionTradeType maps a position to the correct trade_type label
// for circuit-breaker / kill-switch close records. HL perps and OKX perps
// carry pos.Multiplier=1 (#254/#497 perps PnL valuation convention — NOT a
// contract multiplier), so the legacy "Multiplier>0 → futures" classifier
// mislabels every perps force-close as "futures". This is an operator-facing
// label fix only: tradeLedgerDeltaSQL (trade_pnl.go) keys on
// is_close/pnl_gross/realized_pnl/exchange_fee and never reads trade_type, so
// the label does NOT affect any ledger sum — relabeling here changes what an
// operator sees (Discord/leaderboard/audit), not the #954 ledger math.
// TopStep/CME futures keep pos.Multiplier as the real contract multiplier;
// that is the only branch where "futures" is correct.
func classifyPositionTradeType(s *StrategyState, pos *Position) string {
	if pos == nil {
		return "spot"
	}
	// #1159: a hedge leg is display-only labeled distinctly (operator-facing;
	// does not feed any ledger sum) — mirrors the #1008 doc comment above.
	if pos.HedgeFor != "" {
		return hedgeTradeType
	}
	if pos.Multiplier > 0 {
		if s != nil {
			switch {
			case s.Platform == "hyperliquid" && (s.Type == "perps" || s.Type == "manual"):
				return "perps"
			case s.Platform == "okx" && s.Type == "perps":
				return "perps"
			}
		}
		return "futures"
	}
	return "spot"
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
		// PnL branch is the same for perps (Multiplier=1) and futures
		// (Multiplier=contract size) — qty*multiplier*price_delta. Only the
		// trade_type LABEL differs by venue, classified via
		// classifyPositionTradeType so perps force-closes carry an accurate
		// operator-facing label. The label does not feed any ledger sum
		// (tradeLedgerDeltaSQL ignores trade_type); it is display-only.
		tradeType := classifyPositionTradeType(s, pos)
		reason := "circuit_breaker"
		details := ""
		// #1009: a force-close must never book PnL off a structurally-corrupt
		// position. A non-positive quantity (the negative residual a mis-sized
		// direction reversal used to leave) or a non-positive avg cost (a zeroed
		// entry that books the full notional as PnL — the ~4884x overstatement
		// folded in from PR #1008) makes qty*(price-avgCost) meaningless. Clear
		// it with a zero-PnL leg and leave cash untouched so the booked
		// realized_pnl reconciles with the closed_positions row.
		if closePositionIsCorrupt(pos) {
			reason = "circuit_breaker_corrupt"
			details = fmt.Sprintf("Circuit breaker close %s (corrupt qty=%.6f avg_cost=%.4f) — zero PnL booked", pos.Side, pos.Quantity, pos.AvgCost)
			if logger != nil {
				logger.Warn("Circuit breaker: corrupt %s position %s (qty=%.6f avg_cost=%.4f) — booking zero realized PnL, not qty*(price-avgCost)", pos.Side, symbol, pos.Quantity, pos.AvgCost)
			}
		} else if pos.Multiplier > 0 {
			// Futures/perps: PnL-based (contracts * multiplier * price delta)
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
		if details == "" {
			details = fmt.Sprintf("Circuit breaker close %s, PnL: $%.2f (model-only reconciliation adjustment; no exchange fill)", pos.Side, pnl)
		}
		if logger != nil {
			logger.Warn("Circuit breaker: force-closing %s %s @ $%.2f (PnL: $%.2f)", pos.Side, symbol, price, pnl)
		}
		positionID := ensurePositionTradeID(s.ID, symbol, pos)
		trade := Trade{
			Timestamp:         now,
			StrategyID:        s.ID,
			Symbol:            symbol,
			PositionID:        positionID,
			Side:              closeTradeSide(pos.Side),
			Quantity:          absQty(pos.Quantity),
			Price:             price,
			Value:             value,
			TradeType:         tradeType,
			Details:           details,
			IsClose:           true,
			RealizedPnL:       pnl,
			PnLGross:          true, // model-only adjustment has no exchange fee: gross == net
			FeeSource:         FeeSourceReconcileAdjustment,
			Regime:            s.Regime,
			EntryATR:          pos.EntryATR,
			StopLossTriggerPx: pos.StopLossTriggerPx,
			StopLossATRMult:   pos.StopLossATRMult,
			TPTiersJSON:       pos.TPTiersJSON,
		}
		RecordTrade(s, trade)
		// #1159: a hedge leg loses by construction whenever the primary wins —
		// counting it in the loss streak would double-count one economic
		// outcome and mis-fire the CB loss-streak arm on a strategy behaving
		// exactly as configured.
		if pos.HedgeFor != "" {
			RecordHedgeTradeResult(&s.RiskState, pnl)
		} else {
			RecordTradeResult(&s.RiskState, pnl)
		}
		recordClosedPosition(s, pos, price, pnl, reason, now)
		delete(s.Positions, symbol)
		clearATRMultMissingEntryATRWarningOnHLPerpsClose(s, symbol)
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
		positionID := ensureOptionTradeID(s.ID, pos)
		trade := Trade{
			Timestamp:   now,
			StrategyID:  s.ID,
			Symbol:      id,
			PositionID:  positionID,
			Side:        optionCloseTradeSide(pos.Action),
			Quantity:    pos.Quantity,
			Price:       closePrice,
			Value:       closePrice,
			TradeType:   "options",
			Details:     fmt.Sprintf("Circuit breaker force-close, PnL: $%.2f", pnl),
			IsClose:     true,
			RealizedPnL: pnl,
			PnLGross:    true, // model-only adjustment has no exchange fee: gross == net
			FeeSource:   FeeSourceReconcileAdjustment,
			Regime:      s.Regime,
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
// configLeverage is the strategy-config exchange leverage (sc.Leverage), not
// sc.SizingLeverage and not pos.Leverage. This lets operators size small
// positions with sizing_leverage while calculating margin drawdown against the
// leverage actually configured at the exchange (#497).
//
// Positions are filtered by Multiplier > 0 (perps marker). The outer
// s.Type == "perps" check at the call site is the primary guard. configLeverage
// must be > 0 — when zero, the function returns (0, 0) and the caller falls
// back to peak-relative drawdown.
//
// #1159: a correlated hedge leg (pos.HedgeFor != "") is excluded entirely —
// numerator and denominator alike. A hedge is by construction in unrealized
// loss whenever its primary is in profit; folding that loss into the
// numerator while the primary's offsetting gain clamps to zero would inflate
// the ratio and mis-fire the CB/kill switch on a strategy that is net flat or
// winning. RecordHedgeTradeResult already excludes the hedge from
// ConsecutiveLosses for the same reason — this is the drawdown arm's
// counterpart.
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
func perpsMarginDrawdownInputs(s *StrategyState, configLeverage float64, prices map[string]float64) (unrealizedLoss, margin float64) {
	if configLeverage <= 0 {
		return 0, 0
	}
	for sym, pos := range s.Positions {
		if pos.Multiplier <= 0 || pos.HedgeFor != "" {
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
		margin += notional / configLeverage

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

// Shared reason-string prefixes for CheckRisk return values. Consumers
// (main.go notification dispatch, circuit_breaker_alert.go classification,
// tests) must reference these constants rather than re-typing literals so
// reason-string tweaks stay safe under refactor.
//
// RiskReasonConsecutiveLosses is deliberately threshold-free (#1273): the
// loss-streak threshold is per-strategy-configurable, so the fire site appends
// the actual count/threshold after the prefix and every consumer matches with
// strings.HasPrefix — a non-default threshold can never break classification.
const (
	RiskReasonCircuitBreakerActive = "circuit breaker active"
	RiskReasonMaxDrawdownExceeded  = "max drawdown exceeded"
	RiskReasonConsecutiveLosses    = "consecutive losses"
)

// circuitBreakerPermitsManagement reports whether a CheckRisk block should still
// run existing-position management (trailing SL ratchet, TP ratchet, protection
// sync) for an open position instead of skipping the strategy outright. A
// per-strategy circuit breaker exists to block NEW entries; it must not freeze
// the stop-loss on a position that is already open — e.g. a shared-coin residual
// the CB cannot force-close (shouldForceCloseAllPositionsOnCircuitBreaker is
// false when the coin is shared), which then sits with a stale trailing SL for
// the whole latch window and fails to lock in favorable movement (#1046).
//
// Scoped to the latched reason and to HL perps: only that path runs the
// trailing-SL/TP-ratchet walker. Manual strategies are exempt from CheckRisk
// entirely (returns allowed early), and other platforms/types have no equivalent
// in-loop SL ratchet, so they keep the plain skip. The first-fire cycle (reason
// "max drawdown exceeded" / "consecutive losses") is deliberately excluded:
// that is the cycle that force-closes / enqueues the reduce-only drain, so the
// position state is mid-transition; management resumes on the next (latched)
// cycle, which is ~the entire latch window.
func circuitBreakerPermitsManagement(reason, platform, stratType string, posQty float64) bool {
	return reason == RiskReasonCircuitBreakerActive &&
		platform == "hyperliquid" && stratType == "perps" && posQty > 0
}

// CheckRisk evaluates risk state and returns whether trading is allowed.
// sc is the strategy config for this state (nil in some tests — platform
// pending logic is skipped). assist carries pre-fetched per-platform state
// (HL clearinghouse positions today; OKX/TS/RH in later phases) so live
// strategies can enqueue on-chain closes on circuit breaker (#356 / #359).
func CheckRisk(sc *StrategyConfig, s *StrategyState, portfolioValue float64, prices map[string]float64, logger *StrategyLogger, assist *PlatformRiskAssist) (bool, string) {
	// #574: manual strategies are operator-controlled and start with capital=0
	// funded ad-hoc, so peak-relative drawdown is meaningless.
	if sc != nil && sc.Type == "manual" {
		return true, ""
	}
	r := &s.RiskState
	now := time.Now().UTC()

	rolloverDailyPnL(r)

	// Check circuit breaker
	if r.CircuitBreaker {
		if now.Before(r.CircuitBreakerUntil) {
			return false, RiskReasonCircuitBreakerActive
		}
		r.CircuitBreaker = false
		r.ConsecutiveLosses = 0
	}

	// #1048: per-strategy circuit-breaker opt-out. When explicitly disabled, both
	// firing arms below are suppressed so the strategy never latches a NEW circuit
	// break. The gate sits BELOW the latch check and the drawdown computation on
	// purpose: an already-latched CB still blocks/drains (the latch check above is
	// ungated), and CurrentDrawdownPct/peak still update for the status UI. Manual
	// is already exempt via the early return at the top of CheckRisk.
	cbEnabled := sc.CircuitBreakerEnabled()
	if cbEnabled {
		// Clear any sticky suppression-warning throttle eagerly: while the
		// breaker is enabled the warning never applies, and an enabled+breached
		// cycle fires (returning before recordCircuitBreakerSuppression below),
		// so this is the only place a re-enable reliably resets the throttle —
		// ensuring a later re-disable warns afresh. (#1048)
		circuitBreakerSuppressedWarned.Delete(s.ID)
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
			var configLev float64
			if sc != nil {
				configLev = sc.Leverage
			}
			if pnlLoss, margin := perpsMarginDrawdownInputs(s, configLev, prices); margin > 0 {
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
		if r.CurrentDrawdownPct > r.MaxDrawdownPct && cbEnabled {
			r.CircuitBreaker = true
			r.CircuitBreakerUntil = now.Add(sc.CircuitBreakerDrawdownCooldown())
			setHyperliquidCircuitBreakerPending(sc, s, assist)
			setOKXCircuitBreakerPending(sc, s, assist)
			setRobinhoodCircuitBreakerPending(sc, s, assist)
			setTopStepCircuitBreakerPending(sc, s, assist)
			setOperatorRequiredCircuitBreakerPending(sc, s)
			if shouldForceCloseAllPositionsOnCircuitBreaker(sc, assist) {
				forceCloseAllPositions(s, prices, logger)
			}
			return false, fmt.Sprintf("%s (%.1f%% > %.1f%%, portfolio=$%.2f peak=$%.2f, denom=%s=$%.2f)",
				RiskReasonMaxDrawdownExceeded, r.CurrentDrawdownPct, r.MaxDrawdownPct, portfolioValue, r.PeakValue, denomLabel, denom)
		}
	}

	// Consecutive losses circuit breaker (default 5 in a row → pause 1h, close
	// positions; threshold and cooldown per-strategy-tunable, #1273). The
	// reason string keeps RiskReasonConsecutiveLosses as its prefix — every
	// classifier matches on that prefix, so the appended count/threshold is
	// operator display only.
	lossStreakThreshold := sc.CircuitBreakerLossStreakThreshold()
	if r.ConsecutiveLosses >= lossStreakThreshold && cbEnabled {
		r.CircuitBreaker = true
		r.CircuitBreakerUntil = now.Add(sc.CircuitBreakerLossStreakCooldown())
		setHyperliquidCircuitBreakerPending(sc, s, assist)
		setOKXCircuitBreakerPending(sc, s, assist)
		setRobinhoodCircuitBreakerPending(sc, s, assist)
		setTopStepCircuitBreakerPending(sc, s, assist)
		setOperatorRequiredCircuitBreakerPending(sc, s)
		if shouldForceCloseAllPositionsOnCircuitBreaker(sc, assist) {
			forceCloseAllPositions(s, prices, logger)
		}
		return false, fmt.Sprintf("%s (%d in a row, threshold %d)", RiskReasonConsecutiveLosses, r.ConsecutiveLosses, lossStreakThreshold)
	}

	// #1048: if the circuit breaker is disabled and a halt threshold was just
	// crossed, the two arms above fell through silently. Leave a runtime trace
	// (a WARNING, not a halt) so the missing auto-protection is observable in
	// logs at the cycle it matters — not only at startup / on-demand inspect.
	recordCircuitBreakerSuppression(s, cbEnabled, lossStreakThreshold, logger)

	return true, ""
}

// circuitBreakerSuppressedWarned throttles the "circuit breaker disabled but a
// halt threshold was crossed" warning to once per strategy per suppression
// episode. The key is cleared by recordCircuitBreakerSuppression when the
// breaker is re-enabled or the breach clears, so a fresh crossing — or a later
// re-disable — warns again. (#1048)
var circuitBreakerSuppressedWarned sync.Map

// recordCircuitBreakerSuppression emits a one-shot WARNING when a strategy with
// the circuit breaker explicitly disabled (circuit_breaker:false) crosses a
// halt threshold that WOULD have fired. It makes the absence of the
// auto-protective halt observable at the cycle it matters, not only via the
// startup summary / inspect surfaces. It is a warning, never a halt: nothing is
// closed and trading continues. The notice is deduped to once per suppression
// episode and cleared when the breaker is re-enabled or all thresholds clear —
// so a later genuine fire (once re-enabled) still alerts through the normal
// circuit-breaker path, and a subsequent re-disable warns afresh. (#1048)
// lossStreakThreshold is the caller's resolved CircuitBreakerLossStreakThreshold()
// so the warning fires at exactly the same streak length as the firing arm,
// including per-strategy overrides (#1273).
func recordCircuitBreakerSuppression(s *StrategyState, cbEnabled bool, lossStreakThreshold int, logger *StrategyLogger) {
	if s == nil {
		return
	}
	r := &s.RiskState
	// Mirror the drawdown arm's condition exactly (risk.go ~1470) so the warning
	// stays in sync if that gate is later edited — the PeakValue>0 guard is
	// implicit there (CurrentDrawdownPct is 0 when PeakValue is 0). (#1048)
	drawdownBreached := r.CurrentDrawdownPct > r.MaxDrawdownPct
	lossBreached := r.ConsecutiveLosses >= lossStreakThreshold
	if cbEnabled || (!drawdownBreached && !lossBreached) {
		circuitBreakerSuppressedWarned.Delete(s.ID)
		return
	}
	if _, loaded := circuitBreakerSuppressedWarned.LoadOrStore(s.ID, struct{}{}); loaded {
		return // already warned this episode — do not repeat every cycle
	}
	var reasons []string
	if drawdownBreached {
		reasons = append(reasons, fmt.Sprintf("drawdown %.1f%% > %.1f%%", r.CurrentDrawdownPct, r.MaxDrawdownPct))
	}
	if lossBreached {
		reasons = append(reasons, fmt.Sprintf("%d consecutive losses", r.ConsecutiveLosses))
	}
	if logger != nil {
		logger.Warn("WARNING: circuit breaker is DISABLED (circuit_breaker:false) and a halt threshold was crossed (%s) — NO circuit breaker fired. This strategy is trading WITHOUT the drawdown/consecutive-loss auto-halt and positions are NOT being auto-closed on this condition. This is a warning only (nothing was closed); re-enable circuit_breaker to restore protection.",
			strings.Join(reasons, "; "))
	}
}

// RecordTradeResult updates risk state with realized PnL for daily limits and
// consecutive-loss circuit breakers. Lifetime trade stats come from SQLite.
func RecordTradeResult(r *RiskState, pnl float64) {
	rolloverDailyPnL(r)
	r.DailyPnL += pnl
	if pnl >= 0 {
		r.ConsecutiveLosses = 0
	} else {
		r.ConsecutiveLosses++
	}
}

// RecordHedgeTradeResult books a hedge leg's realized PnL into DailyPnL (#1269
// accounting integrity) WITHOUT touching ConsecutiveLosses. A hedge leg loses
// by construction whenever the primary thesis wins — counting it in the loss
// streak would double-count one economic outcome and mis-fire the CB
// loss-streak arm on a strategy that is behaving exactly as configured
// (#1159). Route every hedge close-booking site through this, never
// RecordTradeResult.
func RecordHedgeTradeResult(r *RiskState, pnl float64) {
	rolloverDailyPnL(r)
	r.DailyPnL += pnl
}
