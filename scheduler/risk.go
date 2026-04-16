package main

import (
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
// FetchOKXPerpsMarks (see collectPerpsMarkSymbols). Routing perps through
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
// FetchOKXPerpsMarks respectively. This is the correct oracle for perps
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
type KillSwitchEvent struct {
	Timestamp      time.Time `json:"timestamp"`
	Type           string    `json:"type"` // "triggered", "reset", "warning"
	DrawdownPct    float64   `json:"drawdown_pct"`
	PortfolioValue float64   `json:"portfolio_value"`
	PeakValue      float64   `json:"peak_value"`
	Details        string    `json:"details"`
}

// PortfolioRiskState tracks aggregate portfolio-level risk (#42).
type PortfolioRiskState struct {
	PeakValue          float64           `json:"peak_value"`
	CurrentDrawdownPct float64           `json:"current_drawdown_pct"`
	KillSwitchActive   bool              `json:"kill_switch_active"`
	KillSwitchAt       time.Time         `json:"kill_switch_at,omitempty"`
	WarningSent        bool              `json:"warning_sent,omitempty"`
	Events             []KillSwitchEvent `json:"events,omitempty"`
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
	addKillSwitchEvent(&state.PortfolioRisk, "auto_reset",
		0, totalBalance, totalBalance,
		fmt.Sprintf("startup auto-clear: shared wallets %v reachable, total balance=$%.2f (peak re-baselined)",
			sharedPlatforms, totalBalance))
	return true
}

// addKillSwitchEvent appends an event and trims to maxKillSwitchEvents.
func addKillSwitchEvent(prs *PortfolioRiskState, eventType string, drawdownPct, portfolioValue, peakValue float64, details string) {
	prs.Events = append(prs.Events, KillSwitchEvent{
		Timestamp:      time.Now().UTC(),
		Type:           eventType,
		DrawdownPct:    drawdownPct,
		PortfolioValue: portfolioValue,
		PeakValue:      peakValue,
		Details:        details,
	})
	if len(prs.Events) > maxKillSwitchEvents {
		prs.Events = prs.Events[len(prs.Events)-maxKillSwitchEvents:]
	}
}

// CheckPortfolioRisk evaluates aggregate portfolio risk.
// Returns (allowed, notionalBlocked, warning, reason).
// allowed=false means the kill switch has fired or is latched; notionalBlocked=true
// means new trades should be skipped but existing positions kept; warning=true
// means drawdown is approaching the kill switch threshold.
func CheckPortfolioRisk(prs *PortfolioRiskState, cfg *PortfolioRiskConfig, totalValue, totalNotional float64) (allowed, notionalBlocked, warning bool, reason string) {
	if prs.KillSwitchActive {
		return false, false, false, fmt.Sprintf("portfolio kill switch is latched (triggered at %s, manual reset required)",
			prs.KillSwitchAt.Format("2006-01-02 15:04:05 UTC"))
	}

	// Ratchet peak high-water mark upward only.
	if totalValue > prs.PeakValue {
		prs.PeakValue = totalValue
	}

	// Check drawdown kill switch.
	if prs.PeakValue > 0 {
		prs.CurrentDrawdownPct = (prs.PeakValue - totalValue) / prs.PeakValue * 100

		// Kill switch fires if drawdown exceeds limit.
		if prs.CurrentDrawdownPct > cfg.MaxDrawdownPct {
			prs.KillSwitchActive = true
			prs.KillSwitchAt = time.Now().UTC()
			r := fmt.Sprintf("portfolio drawdown %.1f%% exceeds limit %.1f%% (value=$%.2f, peak=$%.2f)",
				prs.CurrentDrawdownPct, cfg.MaxDrawdownPct, totalValue, prs.PeakValue)
			addKillSwitchEvent(prs, "triggered", prs.CurrentDrawdownPct, totalValue, prs.PeakValue, r)
			return false, false, false, r
		}

		// Warning check: approaching kill switch threshold.
		warnDrawdownPct := cfg.MaxDrawdownPct * cfg.WarnThresholdPct / 100
		if prs.CurrentDrawdownPct > warnDrawdownPct {
			if !prs.WarningSent {
				prs.WarningSent = true
				warning = true
				reason = fmt.Sprintf("portfolio drawdown %.1f%% approaching kill switch limit %.1f%% (warn at %.1f%%, value=$%.2f, peak=$%.2f)",
					prs.CurrentDrawdownPct, cfg.MaxDrawdownPct, warnDrawdownPct, totalValue, prs.PeakValue)
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
		s.TradeHistory = append(s.TradeHistory, trade)
		RecordTradeResult(&s.RiskState, pnl)
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
		s.TradeHistory = append(s.TradeHistory, trade)
		RecordTradeResult(&s.RiskState, pnl)
		delete(s.OptionPositions, id)
	}
}

// CheckRisk evaluates risk state and returns whether trading is allowed.
func CheckRisk(s *StrategyState, portfolioValue float64, prices map[string]float64, logger *StrategyLogger) (bool, string) {
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

	// Check drawdown
	if r.PeakValue > 0 {
		r.CurrentDrawdownPct = ((r.PeakValue - portfolioValue) / r.PeakValue) * 100
		if r.TotalTrades > 0 && r.CurrentDrawdownPct > r.MaxDrawdownPct {
			r.CircuitBreaker = true
			r.CircuitBreakerUntil = now.Add(24 * time.Hour)
			forceCloseAllPositions(s, prices, logger)
			return false, fmt.Sprintf("max drawdown exceeded (%.1f%% > %.1f%%, portfolio=$%.2f peak=$%.2f)",
				r.CurrentDrawdownPct, r.MaxDrawdownPct, portfolioValue, r.PeakValue)
		}
	}

	// Consecutive losses circuit breaker (5 in a row → pause 1h, close positions)
	if r.ConsecutiveLosses >= 5 {
		r.CircuitBreaker = true
		r.CircuitBreakerUntil = now.Add(1 * time.Hour)
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
