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

var schedulerStarted atomic.Bool

func markSchedulerStarted() {
	schedulerStarted.Store(true)
}

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

func collectPerpsMarkSymbols(strategies []StrategyConfig) (hlCoins, okxCoins []string) {
	hlSet := make(map[string]bool)
	okxSet := make(map[string]bool)
	for _, sc := range strategies {
		var coin string
		switch sc.Type {
		case "perps":
			if len(sc.Args) < 2 {
				continue
			}
			coin = sc.Args[1]
		case "manual":
			if sc.Platform != "hyperliquid" {
				continue
			}
			coin = sc.Symbol
		default:
			continue
		}
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
	for _, coin := range hedgeCoinsForStrategies(strategies) {
		hlSet[coin] = true
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

type missingMarkPosition struct {
	StrategyID       string
	Symbol           string
	Live             bool
	Platform         string
	Type             string
	DisabledManagers []string
}

func markGatedManagers(sc StrategyConfig) []string {
	if sc.Platform != "hyperliquid" {
		return nil
	}
	switch sc.Type {
	case "perps", "manual":
		return []string{"Trailing stop-loss walker", "Take-profit ratchet"}
	}
	return nil
}

func collectMissingMarkPositions(strategies []StrategyConfig, openSymbols map[string][]string, prices map[string]float64) []missingMarkPosition {
	if len(openSymbols) == 0 {
		return nil
	}
	var out []missingMarkPosition
	for _, sc := range strategies {
		switch sc.Type {
		case "spot", "perps", "futures", "manual":
		default:
			continue
		}
		syms := openSymbols[sc.ID]
		if len(syms) == 0 {
			continue
		}
		live := isLiveArgs(sc.Args)
		sorted := append([]string(nil), syms...)
		sort.Strings(sorted)
		for _, sym := range sorted {
			if sym == "" {
				continue
			}
			if prices[sym] > 0 {
				continue
			}
			out = append(out, missingMarkPosition{
				StrategyID:       sc.ID,
				Symbol:           sym,
				Live:             live,
				Platform:         sc.Platform,
				Type:             sc.Type,
				DisabledManagers: markGatedManagers(sc),
			})
		}
	}
	return out
}

func snapshotOpenSymbolsByStrategy(state *AppState) map[string][]string {
	if state == nil {
		return nil
	}
	out := make(map[string][]string, len(state.Strategies))
	for sid, s := range state.Strategies {
		if s == nil {
			continue
		}
		for sym, pos := range s.Positions {
			if pos == nil || pos.Quantity <= 0 {
				continue
			}
			out[sid] = append(out[sid], sym)
		}
	}
	return out
}

func formatManualMarkBasisRebaselineDM(priorPeak, newPeak, liveTotal, legacyTotal float64) string {
	return fmt.Sprintf("ℹ️ **Portfolio peak re-baselined once (#1444 valuation basis)**\nManual positions now value at the live mark instead of entry cost, so the stored peak was measured on the old basis.\n• Peak: $%.2f → $%.2f\n• Live-priced total: $%.2f\n• Same book on the old basis: $%.2f\n• Basis delta: $%.2f\nThe drawdown reading was NOT reset — only the units were corrected. This runs once and is recorded in the kill-switch event log.",
		priorPeak, newPeak, liveTotal, legacyTotal, liveTotal-legacyTotal)
}

func manualOnlyMarkSymbols(strategies []StrategyConfig) []string {
	donors := make(map[string]bool)
	for _, sc := range strategies {
		switch sc.Type {
		case "perps", "spot":
			if len(sc.Args) >= 2 && sc.Args[1] != "" {
				donors[sc.Args[1]] = true
			}
		}
	}
	for _, coin := range hedgeCoinsForStrategies(strategies) {
		donors[coin] = true
	}
	for _, sym := range collectFuturesMarkSymbols(strategies) {
		donors[sym] = true
	}

	set := make(map[string]bool)
	for _, sc := range strategies {
		if sc.Type != "manual" || sc.Platform != "hyperliquid" {
			continue
		}
		if sc.Symbol == "" || donors[sc.Symbol] {
			continue
		}
		set[sc.Symbol] = true
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func missingManualOnlyMarks(strategies []StrategyConfig, openSymbols map[string][]string, prices map[string]float64) []string {
	manualOnly := manualOnlyMarkSymbols(strategies)
	if len(manualOnly) == 0 || len(openSymbols) == 0 {
		return nil
	}
	want := make(map[string]bool, len(manualOnly))
	for _, sym := range manualOnly {
		want[sym] = true
	}
	missing := make(map[string]bool)
	for _, syms := range openSymbols {
		for _, sym := range syms {
			if sym == "" || !want[sym] || prices[sym] > 0 {
				continue
			}
			missing[sym] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	out := make([]string, 0, len(missing))
	for sym := range missing {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}

func pricesWithoutSymbols(prices map[string]float64, drop []string) map[string]float64 {
	if len(drop) == 0 {
		return prices
	}
	out := make(map[string]float64, len(prices))
	for k, v := range prices {
		out[k] = v
	}
	for _, sym := range drop {
		delete(out, sym)
	}
	return out
}

func manualMarkBasisPeakAdjustment(oldPeak, liveTotal, legacyTotal float64) (float64, bool) {
	if oldPeak <= 0 {
		return oldPeak, false
	}
	delta := liveTotal - legacyTotal
	if delta == 0 {
		return oldPeak, false
	}
	newPeak := oldPeak + delta
	if newPeak <= 0 {
		return oldPeak, false
	}
	return newPeak, true
}

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

const untrustedEquityLatchDeferral = 15 * time.Minute

type KillSwitchEvent struct {
	Scope          PortfolioScope `json:"scope,omitempty"`
	Timestamp      time.Time      `json:"timestamp"`
	Type           string         `json:"type"`
	Source         string         `json:"source,omitempty"`
	DrawdownPct    float64        `json:"drawdown_pct"`
	PortfolioValue float64        `json:"portfolio_value"`
	PeakValue      float64        `json:"peak_value"`
	Details        string         `json:"details"`
}

type PortfolioRiskState struct {
	PeakValue                  float64           `json:"peak_value"`
	CurrentDrawdownPct         float64           `json:"current_drawdown_pct"`
	CurrentMarginDrawdownPct   float64           `json:"current_margin_drawdown_pct,omitempty"`
	DrawdownReadingSubstituted bool              `json:"drawdown_reading_substituted,omitempty"`
	UntrustedOverLimitSince    time.Time         `json:"untrusted_over_limit_since,omitempty"`
	KillSwitchActive           bool              `json:"kill_switch_active"`
	KillSwitchAt               time.Time         `json:"kill_switch_at,omitempty"`
	WarningSent                bool              `json:"warning_sent,omitempty"`
	WarnBandEnteredAt          time.Time         `json:"warn_band_entered_at,omitempty"`
	LastWarningEquityDDPct     float64           `json:"last_warning_equity_dd_pct,omitempty"`
	LastWarningMarginDDPct     float64           `json:"last_warning_margin_dd_pct,omitempty"`
	WarningEquityDeltaPct      float64           `json:"warning_equity_delta_pct,omitempty"`
	WarningMarginDeltaPct      float64           `json:"warning_margin_delta_pct,omitempty"`
	Events                     []KillSwitchEvent `json:"events,omitempty"`

	ManualMarkBasisRebaselined bool `json:"manual_mark_basis_rebaselined,omitempty"`
	KillSwitchCloseApplied     bool `json:"kill_switch_close_applied,omitempty"`
}

type SharedWalletBalanceFetcher func(platform string) (float64, error)

func detectSharedWalletPlatforms(strategies []StrategyConfig) []string {
	byID := make(map[string]StrategyConfig, len(strategies))
	walletMembers := make(map[SharedWalletKey][]string)
	for _, sc := range strategies {
		byID[sc.ID] = sc
		if key, ok := walletKeyFor(sc); ok && hasSharedWalletBalanceFetcher(key.Platform) {
			walletMembers[key] = append(walletMembers[key], sc.ID)
		}
	}
	platformSet := make(map[string]bool)
	for key, memberIDs := range walletMembers {
		memberIDs = riskPathWalletMemberIDs(key, memberIDs, strategies)
		if len(memberIDs) < 2 {
			continue
		}
		allLegacyPct := true
		for _, id := range memberIDs {
			if byID[id].CapitalPct <= 0 {
				allLegacyPct = false
				break
			}
		}
		if allLegacyPct {
			platformSet[key.Platform] = true
		}
	}
	var platforms []string
	for platform := range platformSet {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	return platforms
}

func ClearLatchedKillSwitchSharedWallet(state *AppState, strategies []StrategyConfig, fetcher SharedWalletBalanceFetcher) bool {
	if schedulerStarted.Load() {
		panic("ClearLatchedKillSwitchSharedWallet called after scheduler started")
	}
	if state == nil {
		return false
	}
	prs := state.scopeRiskIfPresent(ScopeLive)
	if prs == nil || !prs.KillSwitchActive {
		return false
	}

	sharedPlatforms := detectSharedWalletPlatforms(strategies)
	if len(sharedPlatforms) == 0 {
		return false
	}

	totalBalance := 0.0
	for _, plat := range sharedPlatforms {
		balance, err := fetcher(plat)
		if err != nil {
			fmt.Printf("[INFO] Shared wallet (%s): kill switch NOT cleared — balance fetch failed: %v\n", plat, err)
			return false
		}
		totalBalance += balance
	}

	latchedAt := prs.KillSwitchAt.Format("2006-01-02 15:04 UTC")
	fmt.Printf("[INFO] Shared wallet (%v): clearing kill switch (was latched at %s, real total balance=$%.2f, prior peak=$%.2f)\n",
		sharedPlatforms, latchedAt, totalBalance, prs.PeakValue)

	prs.KillSwitchActive = false
	prs.KillSwitchAt = time.Time{}
	prs.WarningSent = false
	prs.WarnBandEnteredAt = time.Time{}
	prs.LastWarningEquityDDPct = 0
	prs.LastWarningMarginDDPct = 0
	prs.WarningEquityDeltaPct = 0
	prs.WarningMarginDeltaPct = 0
	prs.PeakValue = totalBalance
	prs.CurrentDrawdownPct = 0
	prs.CurrentMarginDrawdownPct = 0
	prs.DrawdownReadingSubstituted = false
	prs.UntrustedOverLimitSince = time.Time{}
	prs.KillSwitchCloseApplied = false
	addKillSwitchEvent(prs, "auto_reset", "",
		0, totalBalance, totalBalance,
		fmt.Sprintf("startup auto-clear: shared wallets %v reachable, total balance=$%.2f (peak re-baselined)",
			sharedPlatforms, totalBalance))
	return true
}

func portfolioPeakRebaselineAvailable(usedPVFallback, usedStaleRiskBalance, pooledEquityComplete bool) bool {
	return !usedPVFallback && !usedStaleRiskBalance && pooledEquityComplete
}

func AutoResetConfirmedFlatKillSwitch(
	prs *PortfolioRiskState,
	rebaselineValue float64,
	rebaselineAvailable bool,
	details string,
) bool {
	if prs == nil || !prs.KillSwitchActive {
		return false
	}

	prevEquityDrawdownPct := prs.CurrentDrawdownPct
	prevMarginDrawdownPct := prs.CurrentMarginDrawdownPct
	if details != "" {
		details = fmt.Sprintf("%s (previous equity drawdown=%.2f%%, previous margin drawdown=%.2f%%)",
			details, prevEquityDrawdownPct, prevMarginDrawdownPct)
	}
	if !rebaselineAvailable {
		details = fmt.Sprintf("%s (portfolio peak retained at $%.2f because current equity is not trustworthy)",
			details, prs.PeakValue)
	}

	prs.KillSwitchActive = false
	prs.KillSwitchAt = time.Time{}
	prs.WarningSent = false
	prs.WarnBandEnteredAt = time.Time{}
	prs.LastWarningEquityDDPct = 0
	prs.LastWarningMarginDDPct = 0
	prs.WarningEquityDeltaPct = 0
	prs.WarningMarginDeltaPct = 0
	if rebaselineAvailable {
		prs.PeakValue = rebaselineValue
	}
	prs.CurrentDrawdownPct = 0
	prs.CurrentMarginDrawdownPct = 0
	prs.DrawdownReadingSubstituted = false
	prs.UntrustedOverLimitSince = time.Time{}
	prs.KillSwitchCloseApplied = false
	addKillSwitchEvent(prs, "auto_reset", "", 0, rebaselineValue, prs.PeakValue, details)
	return true
}

func ResetPortfolioKillSwitchManual(prs *PortfolioRiskState) float64 {
	if prs == nil {
		return 0
	}
	priorDrawdownPct := prs.CurrentDrawdownPct
	prs.KillSwitchActive = false
	prs.KillSwitchAt = time.Time{}
	prs.CurrentDrawdownPct = 0
	prs.CurrentMarginDrawdownPct = 0
	prs.DrawdownReadingSubstituted = false
	prs.UntrustedOverLimitSince = time.Time{}
	prs.KillSwitchCloseApplied = false
	return priorDrawdownPct
}

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

func CheckPortfolioRisk(prs *PortfolioRiskState, cfg *PortfolioRiskConfig, totalValue, totalNotional, perpsUnrealizedLoss, perpsMargin float64) (allowed, notionalBlocked, warning bool, reason string) {
	return checkPortfolioRiskWithEquityAvailability(prs, cfg, totalValue, totalNotional, perpsUnrealizedLoss, perpsMargin, true, true)
}

func checkPortfolioRiskWithEquityAvailability(prs *PortfolioRiskState, cfg *PortfolioRiskConfig, totalValue, totalNotional, perpsUnrealizedLoss, perpsMargin float64, equityAvailable, equityTrusted bool) (allowed, notionalBlocked, warning bool, reason string) {
	if prs.KillSwitchActive {
		return false, false, false, fmt.Sprintf("portfolio kill switch is latched (triggered at %s, manual reset required)",
			prs.KillSwitchAt.Format("2006-01-02 15:04:05 UTC"))
	}

	priorEquityDD := prs.CurrentDrawdownPct
	if priorEquityDD > cfg.MaxDrawdownPct {
		priorEquityDD = cfg.MaxDrawdownPct
	}

	if equityAvailable && equityTrusted && totalValue > prs.PeakValue {
		prs.PeakValue = totalValue
	}

	var equityDD, marginDD float64
	if equityAvailable && prs.PeakValue > 0 {
		equityDD = (prs.PeakValue - totalValue) / prs.PeakValue * 100
		if equityDD < 0 {
			equityDD = 0
		}
		substituted := false
		if !equityTrusted && equityDD < priorEquityDD {
			equityDD = priorEquityDD
			substituted = true
		}
		prs.CurrentDrawdownPct = equityDD
		prs.DrawdownReadingSubstituted = substituted
	}
	if perpsMargin > 0 && perpsUnrealizedLoss > 0 {
		marginDD = perpsUnrealizedLoss / perpsMargin * 100
	}
	prs.CurrentMarginDrawdownPct = marginDD

	equityGuardArmed := equityAvailable && prs.PeakValue > 0

	equityLatchDeferred := false
	if equityGuardArmed && !equityTrusted && cfg.MaxDrawdownPct > 0 && equityDD > cfg.MaxDrawdownPct {
		now := time.Now().UTC()
		if prs.UntrustedOverLimitSince.IsZero() {
			prs.UntrustedOverLimitSince = now
			addKillSwitchEvent(prs, "latch_deferred", "equity", equityDD, totalValue, prs.PeakValue,
				fmt.Sprintf("equity drawdown %.1f%% exceeds limit %.1f%% on an untrusted total (substituted or one-generation-stale); portfolio latch deferred up to %s pending a trusted measurement",
					equityDD, cfg.MaxDrawdownPct, formatWarningDuration(untrustedEquityLatchDeferral)))
		}
		equityLatchDeferred = now.Sub(prs.UntrustedOverLimitSince) < untrustedEquityLatchDeferral
	} else {
		prs.UntrustedOverLimitSince = time.Time{}
	}

	if (equityGuardArmed && !equityLatchDeferred && equityDD > cfg.MaxDrawdownPct) || (!equityGuardArmed && marginDD > cfg.MaxDrawdownPct) {
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
		if !equityGuardArmed {
			source = "margin"
			dd = marginDD
			if equityAvailable {
				r = fmt.Sprintf("portfolio perps margin drawdown %.1f%% exceeds limit %.1f%% (unrealized loss=$%.2f, margin=$%.2f, value=$%.2f, peak=$%.2f)",
					marginDD, cfg.MaxDrawdownPct, perpsUnrealizedLoss, perpsMargin, totalValue, prs.PeakValue)
			} else {
				r = fmt.Sprintf("portfolio perps margin drawdown %.1f%% exceeds limit %.1f%% (unrealized loss=$%.2f, margin=$%.2f; equity unavailable)",
					marginDD, cfg.MaxDrawdownPct, perpsUnrealizedLoss, perpsMargin)
			}
		} else {
			source = "equity"
			dd = equityDD
			if !prs.UntrustedOverLimitSince.IsZero() {
				r = fmt.Sprintf("portfolio drawdown %.1f%% exceeds limit %.1f%% (value=$%.2f, peak=$%.2f); measurement is UNTRUSTED (substituted or stale total) and has read over the limit continuously since %s — latch escalated after %s",
					equityDD, cfg.MaxDrawdownPct, totalValue, prs.PeakValue,
					prs.UntrustedOverLimitSince.Format("2006-01-02 15:04 UTC"),
					formatWarningDuration(untrustedEquityLatchDeferral))
			} else {
				r = fmt.Sprintf("portfolio drawdown %.1f%% exceeds limit %.1f%% (value=$%.2f, peak=$%.2f)",
					equityDD, cfg.MaxDrawdownPct, totalValue, prs.PeakValue)
			}
		}
		prs.UntrustedOverLimitSince = time.Time{}
		addKillSwitchEvent(prs, "triggered", source, dd, totalValue, prs.PeakValue, r)
		return false, false, false, r
	}

	if cfg.MaxDrawdownPct > 0 {
		warnDrawdownPct := cfg.MaxDrawdownPct * cfg.WarnThresholdPct / 100
		equityWarn, marginWarn := portfolioWarnBandSignals(cfg, prs, equityAvailable)
		if equityWarn || marginWarn {
			now := time.Now().UTC()
			if !prs.WarningSent {
				prs.WarnBandEnteredAt = now
				prs.WarningEquityDeltaPct = 0
				prs.WarningMarginDeltaPct = 0
			} else {
				if equityAvailable {
					prs.WarningEquityDeltaPct = equityDD - prs.LastWarningEquityDDPct
				} else {
					prs.WarningEquityDeltaPct = 0
				}
				prs.WarningMarginDeltaPct = marginDD - prs.LastWarningMarginDDPct
			}
			if equityAvailable {
				prs.LastWarningEquityDDPct = equityDD
			}
			prs.LastWarningMarginDDPct = marginDD
			prs.WarningSent = true
			warning = true
			marginOverLimit := marginDD > cfg.MaxDrawdownPct
			switch {
			case equityLatchDeferred:
				reason = fmt.Sprintf("portfolio equity drawdown %.1f%% exceeds limit %.1f%% (value=$%.2f, peak=$%.2f) but the total is UNTRUSTED (substituted or one-generation-stale) — full-book latch DEFERRED since %s, escalates at %s unless a trusted measurement lands first; per-strategy circuit breakers (#292) remain active",
					equityDD, cfg.MaxDrawdownPct, totalValue, prs.PeakValue,
					prs.UntrustedOverLimitSince.Format("2006-01-02 15:04 UTC"),
					prs.UntrustedOverLimitSince.Add(untrustedEquityLatchDeferral).Format("2006-01-02 15:04 UTC"))
				if marginWarn {
					reason += fmt.Sprintf("; perps margin=%.1f%% (unrealized loss=$%.2f, margin=$%.2f)",
						marginDD, perpsUnrealizedLoss, perpsMargin)
				}
			case equityWarn && marginWarn && marginOverLimit:
				reason = fmt.Sprintf("portfolio drawdown warning: equity=%.1f%% (value=$%.2f, peak=$%.2f); perps margin=%.1f%% exceeds limit %.1f%% (unrealized loss=$%.2f, margin=$%.2f); portfolio latch governed by equity drawdown (limit %.1f%%); per-strategy circuit breakers own margin protection (#1448)",
					equityDD, totalValue, prs.PeakValue, marginDD, cfg.MaxDrawdownPct, perpsUnrealizedLoss, perpsMargin, cfg.MaxDrawdownPct)
			case equityWarn && marginWarn:
				reason = fmt.Sprintf("portfolio drawdown approaching kill switch limit %.1f%% (warn at %.1f%%): equity=%.1f%% (value=$%.2f, peak=$%.2f); perps margin=%.1f%% (unrealized loss=$%.2f, margin=$%.2f)",
					cfg.MaxDrawdownPct, warnDrawdownPct, equityDD, totalValue, prs.PeakValue, marginDD, perpsUnrealizedLoss, perpsMargin)
			case marginWarn && marginOverLimit:
				reason = fmt.Sprintf("portfolio perps margin drawdown %.1f%% exceeds limit %.1f%% (unrealized loss=$%.2f, margin=$%.2f); portfolio latch governed by equity drawdown %.1f%% (limit %.1f%%); per-strategy circuit breakers own margin protection (#1448)",
					marginDD, cfg.MaxDrawdownPct, perpsUnrealizedLoss, perpsMargin, equityDD, cfg.MaxDrawdownPct)
			case marginWarn:
				reason = fmt.Sprintf("portfolio perps margin drawdown %.1f%% approaching kill switch limit %.1f%% (warn at %.1f%%, unrealized loss=$%.2f, margin=$%.2f)",
					marginDD, cfg.MaxDrawdownPct, warnDrawdownPct, perpsUnrealizedLoss, perpsMargin)
			default:
				reason = fmt.Sprintf("portfolio drawdown %.1f%% approaching kill switch limit %.1f%% (warn at %.1f%%, value=$%.2f, peak=$%.2f)",
					equityDD, cfg.MaxDrawdownPct, warnDrawdownPct, totalValue, prs.PeakValue)
			}
		} else if equityAvailable {
			prs.WarningSent = false
			prs.WarnBandEnteredAt = time.Time{}
			prs.LastWarningEquityDDPct = 0
			prs.LastWarningMarginDDPct = 0
			prs.WarningEquityDeltaPct = 0
			prs.WarningMarginDeltaPct = 0
		}
	}

	if cfg.MaxNotionalUSD > 0 && totalNotional > cfg.MaxNotionalUSD {
		return true, true, warning, notionalCapHoldDetail(totalNotional, cfg.MaxNotionalUSD)
	}

	return true, false, warning, reason
}

func portfolioWarnBandSignals(cfg *PortfolioRiskConfig, prs *PortfolioRiskState, equityAvailable bool) (equityInBand, marginInBand bool) {
	if cfg == nil || prs == nil || cfg.MaxDrawdownPct <= 0 {
		return false, false
	}
	warnDrawdownPct := cfg.MaxDrawdownPct * cfg.WarnThresholdPct / 100
	equityInBand = equityAvailable && prs.PeakValue > 0 && prs.CurrentDrawdownPct > warnDrawdownPct
	marginInBand = prs.CurrentMarginDrawdownPct > warnDrawdownPct
	return equityInBand, marginInBand
}

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

type RiskState struct {
	PeakValue            float64                         `json:"peak_value"`
	MaxDrawdownPct       float64                         `json:"max_drawdown_pct"`
	CurrentDrawdownPct   float64                         `json:"current_drawdown_pct"`
	DailyPnL             float64                         `json:"daily_pnl"`
	DailyPnLDate         string                          `json:"daily_pnl_date"`
	ConsecutiveLosses    int                             `json:"consecutive_losses"`
	CircuitBreaker       bool                            `json:"circuit_breaker"`
	CircuitBreakerUntil  time.Time                       `json:"circuit_breaker_until"`
	PendingCircuitCloses map[string]*PendingCircuitClose `json:"pending_circuit_closes,omitempty"`
}

const PlatformPendingCloseHyperliquid = "hyperliquid"

const PlatformPendingCloseOKX = "okx"

const PlatformPendingCloseRobinhood = "robinhood"

const PlatformPendingCloseTopStep = "topstep"

const (
	PlatformPendingCloseOKXSpot          = "okx_spot"
	PlatformPendingCloseRobinhoodOptions = "robinhood_options"
)

type PendingCircuitClose struct {
	Symbols             []PendingCircuitCloseSymbol `json:"symbols"`
	OperatorRequired    bool                        `json:"operator_required,omitempty"`
	ConsecutiveFailures int                         `json:"consecutive_failures,omitempty"`
	LastNotifiedAt      time.Time                   `json:"last_notified_at,omitempty"`
}

type PendingCircuitCloseSymbol struct {
	Symbol string  `json:"symbol"`
	Size   float64 `json:"size"`
}

type PlatformRiskAssist struct {
	HLPositions  []HLPosition
	HLLiveAll    []StrategyConfig
	OKXPositions []OKXPosition
	OKXLiveAll   []StrategyConfig
	RHPositions  []RobinhoodPosition
	RHLiveAll    []StrategyConfig
	TSPositions  []TopStepPosition
	TSLiveAll    []StrategyConfig
}

func (r *RiskState) MarshalPendingCircuitClosesJSON() string {
	if r == nil || len(r.PendingCircuitCloses) == 0 {
		return ""
	}
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

func (r *RiskState) UnmarshalPendingCircuitClosesJSON(raw string) {
	if r == nil {
		return
	}
	if raw == "" {
		r.PendingCircuitCloses = nil
		return
	}

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

func (r *RiskState) clearPendingCircuitClose(platform string) {
	if r == nil {
		return
	}
	delete(r.PendingCircuitCloses, platform)
	if len(r.PendingCircuitCloses) == 0 {
		r.PendingCircuitCloses = nil
	}
}

func (r *RiskState) getPendingCircuitClose(platform string) *PendingCircuitClose {
	if r == nil {
		return nil
	}
	return r.PendingCircuitCloses[platform]
}

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
	if _, ok := s.Positions[sym]; !ok {
		return
	}
	qty, ok := computeHyperliquidCircuitCloseQty(sym, s.ID, assist.HLPositions, assist.HLLiveAll)
	if !ok || qty <= 0 {
		return
	}
	symbols := []PendingCircuitCloseSymbol{{Symbol: sym, Size: qty}}
	if hCoin := heldHedgeCoin(*sc, s); hCoin != "" {
		if hQty, hok := computeHyperliquidCircuitCloseQty(hCoin, s.ID, assist.HLPositions, assist.HLLiveAll); hok && hQty > 0 {
			symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: hCoin, Size: hQty})
		}
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
		symbols := make([]PendingCircuitCloseSymbol, 0, len(s.OptionPositions))
		for id, op := range s.OptionPositions {
			if op == nil {
				continue
			}
			symbols = append(symbols, PendingCircuitCloseSymbol{Symbol: id, Size: op.Quantity})
		}
		if len(symbols) == 0 {
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

func rolloverDailyPnL(r *RiskState) {
	today := time.Now().UTC().Format("2006-01-02")
	if r.DailyPnLDate != today {
		r.DailyPnL = 0
		r.DailyPnLDate = today
	}
}

func forceCloseKillSwitchPositions(s *StrategyState, sc StrategyConfig, prices map[string]float64, hlFills map[string]HyperliquidCloseFill, hlLiveAll []StrategyConfig, hlVirtualQty hlVirtualQuantitySnapshot, logger *StrategyLogger) {
	applyHyperliquidKillSwitchCloseFill(s, sc, hlFills, hlLiveAll, hlVirtualQty)
	applyHyperliquidKillSwitchHedgeFill(s, sc, hlFills)
	forceCloseAllPositions(s, &sc, prices, logger)
}

func classifyPositionTradeType(s *StrategyState, pos *Position) string {
	if pos == nil {
		return "spot"
	}
	if pos.isHedgeLeg() {
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

func forceCloseAllPositions(s *StrategyState, sc *StrategyConfig, prices map[string]float64, logger *StrategyLogger) {
	for symbol, pos := range s.Positions {
		closeVirtualPositionAtMark(s, sc, symbol, pos, prices, logger)
	}
	now := time.Now().UTC()

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
			PnLGross:    true,
			FeeSource:   FeeSourceReconcileAdjustment,
			Regime:      s.Regime,
		}
		RecordTrade(s, trade)
		RecordTradeResult(&s.RiskState, pnl)
		recordClosedOptionPosition(s, pos, closePrice, pnl, "circuit_breaker", now)
		delete(s.OptionPositions, id)
	}
}

func closeVirtualPositionAtMark(s *StrategyState, sc *StrategyConfig, symbol string, pos *Position, prices map[string]float64, logger *StrategyLogger) {
	now := time.Now().UTC()
	price, ok := prices[symbol]
	if !ok {
		price = pos.AvgCost
	}
	var pnl, value float64
	tradeType := classifyPositionTradeType(s, pos)
	reason := "circuit_breaker"
	details := ""
	if closePositionIsCorrupt(pos) {
		reason = "circuit_breaker_corrupt"
		details = fmt.Sprintf("Circuit breaker close %s (corrupt qty=%.6f avg_cost=%.4f) — zero PnL booked", pos.Side, pos.Quantity, pos.AvgCost)
		if logger != nil {
			logger.Warn("Circuit breaker: corrupt %s position %s (qty=%.6f avg_cost=%.4f) — booking zero realized PnL, not qty*(price-avgCost)", pos.Side, symbol, pos.Quantity, pos.AvgCost)
		}
	} else if pos.Multiplier > 0 {
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
		if sc != nil && isLiveArgs(sc.Args) {
			queueModelOnlyCloseAlert(s.ID, symbol, pos.Quantity)
		}
	}
	details = fmt.Sprintf("%s, pre-streak=%d", details, s.RiskState.ConsecutiveLosses)
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
		PnLGross:          true,
		FeeSource:         FeeSourceReconcileAdjustment,
		Regime:            s.Regime,
		EntryATR:          pos.EntryATR,
		StopLossTriggerPx: pos.StopLossTriggerPx,
		StopLossATRMult:   pos.StopLossATRMult,
		TPTiersJSON:       pos.TPTiersJSON,
	}
	RecordTrade(s, trade)
	recordPositionTradeResult(s, pos, pnl)
	recordClosedPosition(s, pos, price, pnl, reason, now)
	delete(s.Positions, symbol)
	clearHLPerpsPositionAlertThrottles(s, symbol)
}

func forceCloseSettledPositions(s *StrategyState, sc StrategyConfig, prices map[string]float64, settled map[string]map[string]bool, logger *StrategyLogger) {
	coins := settled[sc.Platform]
	if len(coins) == 0 || s == nil {
		return
	}
	var syms []string
	for sym := range s.Positions {
		if coins[sym] {
			syms = append(syms, sym)
		}
	}
	sort.Strings(syms)
	for _, sym := range syms {
		closeVirtualPositionAtMark(s, &sc, sym, s.Positions[sym], prices, logger)
	}
}

func perpsMarginDrawdownInputs(s *StrategyState, configLeverage float64, prices map[string]float64) (unrealizedLoss, margin float64) {
	if configLeverage <= 0 {
		return 0, 0
	}
	for sym, pos := range s.Positions {
		if pos.Multiplier <= 0 {
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

const (
	RiskReasonCircuitBreakerActive = "circuit breaker active"
	RiskReasonMaxDrawdownExceeded  = "max drawdown exceeded"
	RiskReasonConsecutiveLosses    = "consecutive losses"
)

func circuitBreakerPermitsManagement(reason, platform, stratType string, posQty float64) bool {
	return reason == RiskReasonCircuitBreakerActive &&
		platform == "hyperliquid" && stratType == "perps" && posQty > 0
}

func CheckRisk(sc *StrategyConfig, s *StrategyState, portfolioValue float64, prices map[string]float64, logger *StrategyLogger, assist *PlatformRiskAssist) (bool, string) {
	if sc != nil && sc.Type == "manual" {
		return true, ""
	}
	r := &s.RiskState
	now := time.Now().UTC()

	rolloverDailyPnL(r)

	if r.CircuitBreaker {
		if now.Before(r.CircuitBreakerUntil) {
			return false, RiskReasonCircuitBreakerActive
		}
		r.CircuitBreaker = false
		r.ConsecutiveLosses = 0
	}

	cbEnabled := sc.CircuitBreakerEnabled()
	if cbEnabled {
		circuitBreakerSuppressedWarned.Delete(s.ID)
	}

	poolBudget := sc != nil && usesSharedWalletPoolBudget(*sc)

	if !poolBudget && portfolioValue > r.PeakValue {
		r.PeakValue = portfolioValue
	}

	loss := 0.0
	denom := 0.0
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
	if denom <= 0 && !poolBudget && r.PeakValue > 0 {
		loss = r.PeakValue - portfolioValue
		denom = r.PeakValue
	}
	if denom > 0 {
		if loss < 0 {
			loss = 0
		}
		r.CurrentDrawdownPct = (loss / denom) * 100
		if r.CurrentDrawdownPct > r.MaxDrawdownPct && cbEnabled {
			r.CircuitBreaker = true
			r.CircuitBreakerUntil = now.Add(sc.CircuitBreakerDrawdownCooldown())
			setHyperliquidCircuitBreakerPending(sc, s, assist)
			setOKXCircuitBreakerPending(sc, s, assist)
			setRobinhoodCircuitBreakerPending(sc, s, assist)
			setTopStepCircuitBreakerPending(sc, s, assist)
			setOperatorRequiredCircuitBreakerPending(sc, s)
			if shouldForceCloseAllPositionsOnCircuitBreaker(sc, assist) {
				forceCloseAllPositions(s, sc, prices, logger)
			}
			return false, fmt.Sprintf("%s (%.1f%% > %.1f%%, portfolio=$%.2f peak=$%.2f, denom=%s=$%.2f)",
				RiskReasonMaxDrawdownExceeded, r.CurrentDrawdownPct, r.MaxDrawdownPct, portfolioValue, r.PeakValue, denomLabel, denom)
		}
	} else {
		r.CurrentDrawdownPct = 0
	}

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
			forceCloseAllPositions(s, sc, prices, logger)
		}
		return false, fmt.Sprintf("%s (%d in a row, threshold %d)", RiskReasonConsecutiveLosses, r.ConsecutiveLosses, lossStreakThreshold)
	}

	recordCircuitBreakerSuppression(s, cbEnabled, lossStreakThreshold, logger)

	return true, ""
}

var circuitBreakerSuppressedWarned sync.Map

func recordCircuitBreakerSuppression(s *StrategyState, cbEnabled bool, lossStreakThreshold int, logger *StrategyLogger) {
	if s == nil {
		return
	}
	r := &s.RiskState
	drawdownBreached := r.CurrentDrawdownPct > r.MaxDrawdownPct
	lossBreached := r.ConsecutiveLosses >= lossStreakThreshold
	if cbEnabled || (!drawdownBreached && !lossBreached) {
		circuitBreakerSuppressedWarned.Delete(s.ID)
		return
	}
	if _, loaded := circuitBreakerSuppressedWarned.LoadOrStore(s.ID, struct{}{}); loaded {
		return
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
	queueCircuitBreakerSuppressionAlert(s.ID, reasons)
}

func RecordTradeResult(r *RiskState, pnl float64) {
	rolloverDailyPnL(r)
	r.DailyPnL += pnl
	if pnl >= 0 {
		r.ConsecutiveLosses = 0
	} else {
		r.ConsecutiveLosses++
	}
}

func RecordHedgeTradeResult(r *RiskState, pnl float64) {
	if r == nil {
		return
	}
	rolloverDailyPnL(r)
	r.DailyPnL += pnl
}

func recordPositionTradeResult(s *StrategyState, pos *Position, pnl float64) {
	if s == nil {
		return
	}
	if pos.isHedgeLeg() {
		RecordHedgeTradeResult(&s.RiskState, pnl)
		return
	}
	RecordTradeResult(&s.RiskState, pnl)
}

func forceClosePaperScopePositions(state *AppState, cfg *Config, prices map[string]float64) []string {
	if state == nil || cfg == nil {
		return nil
	}
	var closed []string
	for _, sc := range strategiesInScope(cfg.Strategies, ScopePaper) {
		s, ok := state.Strategies[sc.ID]
		if !ok || s == nil {
			continue
		}
		if len(s.Positions) == 0 && len(s.OptionPositions) == 0 {
			continue
		}
		scCopy := sc
		forceCloseAllPositions(s, &scCopy, prices, nil)
		closed = append(closed, sc.ID)
	}
	sort.Strings(closed)
	return closed
}

func formatPaperKillSwitchMessage(reason string, closed []string) string {
	header := "**PORTFOLIO KILL SWITCH (PAPER)**"
	detail := "No open paper books to close."
	if len(closed) > 0 {
		detail = fmt.Sprintf("Paper books force-closed at mark: %s", strings.Join(closed, ", "))
	}
	return fmt.Sprintf("%s\n%s\n%s No exchange order was sent. Live strategies are unaffected. Reply to the owner DM with 'reset paper' to clear this latch.",
		header, reason, detail)
}
