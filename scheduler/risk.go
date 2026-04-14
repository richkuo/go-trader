package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// collectPriceSymbols returns the list of symbols to fetch for portfolio
// valuation/notional and a mirror map (position-key → fetch-key) used to
// back-fill prices for perps positions.
//
// Spot positions are stored under the same key the price fetcher uses
// (e.g. "BTC/USDT"), so they need no mirroring. Perps positions are stored
// under the base asset only (e.g. "BTC" for Hyperliquid/OKX perps), but
// check_price.py queries BinanceUS which requires "BTC/USDT" format. The
// caller fetches under the normalized key and then invokes
// mirrorPerpsPrices to populate the base-asset alias so that both
// PortfolioNotional and PortfolioValue can resolve prices for open perps
// positions — fixes issue #245 where perps exposure in portfolio-notional
// risk checks was frozen at entry cost (pos.AvgCost) instead of being
// revalued at the live mark, causing notional to drift away from true
// exposure after price moved.
//
// Assumptions and limits:
//   - The fetch-key quote is hardcoded to "/USDT". HL and OKX perps today
//     both settle vs. USDT and BinanceUS quotes BTC/USDT, so this holds.
//     A future USDC- or BTC-settled perps platform would need a
//     per-platform fetch-key derivation (likely pushed into the adapter
//     layer).
//   - BinanceUS coverage is best-effort. HL lists many coins BinanceUS
//     doesn't (HYPE, kPEPE, kSHIB, PURR, …); for those, FetchPrices will
//     return 0 → mirrorPerpsPrices skips → PortfolioNotional/Value fall
//     back to pos.AvgCost, same as before this fix (not a regression).
func collectPriceSymbols(strategies []StrategyConfig) ([]string, map[string]string) {
	set := make(map[string]bool)
	mirror := make(map[string]string)
	for _, sc := range strategies {
		if len(sc.Args) < 2 {
			continue
		}
		switch sc.Type {
		case "spot":
			set[sc.Args[1]] = true
		case "perps":
			baseSym := sc.Args[1]
			fetchSym := baseSym
			if !strings.Contains(baseSym, "/") {
				// HL/OKX perps quote vs. USDT — see "Assumptions" above.
				fetchSym = baseSym + "/USDT"
			}
			set[fetchSym] = true
			if fetchSym != baseSym {
				mirror[baseSym] = fetchSym
			}
		}
	}
	symbols := make([]string, 0, len(set))
	for s := range set {
		symbols = append(symbols, s)
	}
	return symbols, mirror
}

// mirrorPerpsPrices back-fills price aliases so that fetched quotes keyed
// under a normalized fetch symbol (e.g. "BTC/USDT") are also available
// under the position-storage key (e.g. "BTC") used by perps state. An
// existing price under the position key is preserved — if a strategy has
// already published a live exchange mid via result.Symbol during the same
// cycle, that value wins over the (possibly stale) BinanceUS spot quote.
func mirrorPerpsPrices(prices map[string]float64, mirror map[string]string) {
	for posKey, fetchKey := range mirror {
		if _, exists := prices[posKey]; exists {
			continue
		}
		if p, ok := prices[fetchKey]; ok && p > 0 {
			prices[posKey] = p
		}
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
