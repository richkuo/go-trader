package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type PortfolioScope string

const (
	ScopeLive       PortfolioScope = "live"
	ScopePaper      PortfolioScope = "paper"
	scopeUnassigned PortfolioScope = ""
)

func portfolioScopeFor(sc StrategyConfig) PortfolioScope {
	if isLiveArgs(sc.Args) {
		return ScopeLive
	}
	return ScopePaper
}

func scopeLabel(scope PortfolioScope) string {
	switch scope {
	case ScopeLive:
		return "live"
	case ScopePaper:
		return "paper"
	}
	return "unassigned"
}

func activeScopes(cfgs []StrategyConfig) []PortfolioScope {
	hasLive := false
	hasPaper := false
	for _, sc := range cfgs {
		if portfolioScopeFor(sc) == ScopeLive {
			hasLive = true
		} else {
			hasPaper = true
		}
	}
	var out []PortfolioScope
	if hasLive {
		out = append(out, ScopeLive)
	}
	if hasPaper {
		out = append(out, ScopePaper)
	}
	return out
}

func strategiesInScope(cfgs []StrategyConfig, scope PortfolioScope) []StrategyConfig {
	out := make([]StrategyConfig, 0, len(cfgs))
	for _, sc := range cfgs {
		if portfolioScopeFor(sc) == scope {
			out = append(out, sc)
		}
	}
	return out
}

func scopeOfStrategyID(cfgs []StrategyConfig, id string) (PortfolioScope, bool) {
	for _, sc := range cfgs {
		if sc.ID == id {
			return portfolioScopeFor(sc), true
		}
	}
	return scopeUnassigned, false
}

func filterStatesByScope(states map[string]*StrategyState, cfgs []StrategyConfig, scope PortfolioScope) map[string]*StrategyState {
	out := make(map[string]*StrategyState, len(states))
	for _, sc := range cfgs {
		if portfolioScopeFor(sc) != scope {
			continue
		}
		if ss, ok := states[sc.ID]; ok {
			out[sc.ID] = ss
		}
	}
	return out
}

func stateIDsInScope(states map[string]*StrategyState, cfgs []StrategyConfig, scope PortfolioScope) []string {
	ids := make([]string, 0, len(states))
	for _, sc := range cfgs {
		if portfolioScopeFor(sc) != scope {
			continue
		}
		if _, ok := states[sc.ID]; ok {
			ids = append(ids, sc.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func scopeStrategyCounts(cfgs []StrategyConfig) (live, paper int) {
	for _, sc := range cfgs {
		if portfolioScopeFor(sc) == ScopeLive {
			live++
		} else {
			paper++
		}
	}
	return live, paper
}

type scopeCycleRisk struct {
	Partition               RiskPartition
	Config                  *PortfolioRiskConfig
	Prs                     *PortfolioRiskState
	TotalPV                 float64
	TotalNotional           float64
	PerpsLoss               float64
	PerpsMargin             float64
	UsedPVFallback          bool
	EquityAvailable         bool
	EquityTrusted           bool
	EquityGuardArmed        bool
	PeakRebaselineAvailable bool
	KillSwitchFired         bool
	NotionalBlocked         bool
	DailyLossEntriesHeld    bool
	DailyLossStatus         DailyLossLimitStatus
	ExposureCapStatus       ExposureCapStatus
	Warning                 bool
	WarnBandEntered         bool
	Reason                  string
	CloseApplied            bool

	ValuationComplete    bool
	ValuationGap         string
	InputsFrom           time.Time
	Rebaselined          bool
	RebaselineOldPeak    float64
	RebaselineNewPeak    float64
	RebaselineSaveErr    error
	rebaselineSuppressed bool
}

func scopeCycleRiskFired(scopeRisk map[RiskPartition]*scopeCycleRisk, part RiskPartition) bool {
	sr, ok := scopeRisk[part]
	return ok && sr != nil && sr.KillSwitchFired
}

func dueStrategiesNotLatched(due []StrategyConfig, scopeRisk map[RiskPartition]*scopeCycleRisk) []StrategyConfig {
	out := make([]StrategyConfig, 0, len(due))
	for _, sc := range due {
		if scopeCycleRiskFired(scopeRisk, partitionFor(sc)) {
			continue
		}
		out = append(out, sc)
	}
	return out
}

func measureScopeCycleRisk(
	part RiskPartition,
	pr *PortfolioRiskConfig,
	cfgStrategies []StrategyConfig,
	state *AppState,
	prices map[string]float64,
	riskWalletBalances map[SharedWalletKey]float64,
	sharedWallets map[SharedWalletKey][]string,
	pooledEquityComplete bool,
	usedStaleRiskBalance bool,
	now time.Time,
) *scopeCycleRisk {
	sr := &scopeCycleRisk{Partition: part, Config: pr}
	scopedCfgs := strategiesInPartition(cfgStrategies, part)
	scopedStates := filterStatesByPartition(state.Strategies, cfgStrategies, part)
	if part.IsLive() {
		sr.TotalPV, sr.UsedPVFallback = computeSubsetPortfolioValue(scopedCfgs, state, prices, riskWalletBalances, sharedWallets)
		sr.EquityAvailable = pooledEquityComplete
		sr.EquityTrusted = pooledEquityComplete && !sr.UsedPVFallback && !usedStaleRiskBalance
		sr.PeakRebaselineAvailable = portfolioPeakRebaselineAvailable(sr.UsedPVFallback, usedStaleRiskBalance, pooledEquityComplete)
	} else {
		sr.TotalPV, _ = computeSubsetPortfolioValue(scopedCfgs, state, prices, nil, sharedWallets)
		sr.EquityAvailable = true
		sr.EquityTrusted = true
		sr.PeakRebaselineAvailable = true
	}
	sr.TotalNotional = portfolioNotional(scopedStates, prices)
	sr.PerpsLoss, sr.PerpsMargin = aggregatePerpsMarginInputs(scopedStates, scopedCfgs, prices)
	sr.DailyLossStatus = evaluateDailyLossLimit(pr, scopedStates, scopedCfgs, now)
	sr.ExposureCapStatus = evaluateExposureCap(pr, scopedStates, scopedCfgs, prices, sr.TotalPV)
	return sr
}

func scopePeakRebaselineEligible(sr *scopeCycleRisk, prs *PortfolioRiskState) bool {
	if sr == nil || prs == nil || sr.rebaselineSuppressed {
		return false
	}
	if !prs.PeakRebaselinePending || prs.KillSwitchActive {
		return false
	}
	if !sr.EquityAvailable || !sr.EquityTrusted || !sr.PeakRebaselineAvailable || !sr.ValuationComplete {
		return false
	}
	if !rebaselineInputsAfterReset(sr, prs) {
		return false
	}
	return finitePositive(sr.TotalPV)
}

func rebaselineInputsAfterReset(sr *scopeCycleRisk, prs *PortfolioRiskState) bool {
	return prs.peakRebaselineArmedAt.IsZero() || sr.InputsFrom.After(prs.peakRebaselineArmedAt)
}

func applyScopeCycleRisk(sr *scopeCycleRisk, prs *PortfolioRiskState) {
	sr.Prs = prs
	sr.Rebaselined = false
	sr.RebaselineOldPeak = 0
	sr.RebaselineNewPeak = 0
	if scopePeakRebaselineEligible(sr, prs) {
		sr.Rebaselined = true
		sr.RebaselineOldPeak = prs.PeakValue
		sr.RebaselineNewPeak = sr.TotalPV
		prs.PeakValue = sr.TotalPV
		prs.cancelPeakRebaseline()
		prs.ManualMarkBasisRebaselined = true
		prs.QuotePrecisionRebaselined = true
		addKillSwitchEvent(prs, "reset_rebaseline", "equity", 0, sr.TotalPV, sr.TotalPV,
			fmt.Sprintf("first eligible equity reading after the kill-switch reset: peak $%.2f -> $%.2f (%s scope)",
				sr.RebaselineOldPeak, sr.RebaselineNewPeak, partitionLabel(sr.Partition)))
		prs.Events[len(prs.Events)-1].Partition = sr.Partition
	}
	origPeak := prs.PeakValue
	prevWarningSent := prs.WarningSent
	allowed, notionalBlocked, warning, reason := checkPortfolioRiskWithEquityAvailability(
		prs, sr.Config, sr.TotalPV, sr.TotalNotional, sr.PerpsLoss, sr.PerpsMargin, sr.EquityAvailable, sr.EquityTrusted)
	sr.Warning = warning
	sr.WarnBandEntered = warning && !prevWarningSent
	sr.Reason = reason
	if (!sr.PeakRebaselineAvailable || (prs.PeakRebaselinePending && origPeak > 0)) && prs.PeakValue > origPeak {
		prs.PeakValue = origPeak
	}
	sr.EquityGuardArmed = sr.EquityAvailable && prs.PeakValue > 0
	sr.KillSwitchFired = !allowed
	sr.NotionalBlocked = notionalBlocked
	sr.DailyLossEntriesHeld = sr.DailyLossStatus.Tripped
}

func applyScopeCycleRiskCommitted(sr *scopeCycleRisk, prs *PortfolioRiskState, save func(RiskPartition) error) {
	before := clonePortfolioRiskState(prs)
	sr.RebaselineSaveErr = nil
	sr.rebaselineSuppressed = false
	applyScopeCycleRisk(sr, prs)
	if !sr.Rebaselined {
		return
	}
	err := fmt.Errorf("state store unavailable")
	if save != nil {
		err = save(sr.Partition)
	}
	if err == nil {
		return
	}
	*prs = before
	sr.rebaselineSuppressed = true
	applyScopeCycleRisk(sr, prs)
	sr.rebaselineSuppressed = false
	sr.RebaselineSaveErr = err
}

type cyclePriceSources map[string]string

const (
	priceSourceSpot            = "spot"
	priceSourceHyperliquid     = "hyperliquid"
	priceSourceOKX             = "okx"
	priceSourceFutures         = "futures"
	priceSourceFuturesDegraded = "futures_degraded"
)

func (ps cyclePriceSources) label(prices map[string]float64, source string) {
	if ps == nil {
		return
	}
	for sym := range prices {
		if _, ok := ps[sym]; !ok {
			ps[sym] = source
		}
	}
}

func expectedPriceSource(platform, stratType string) string {
	switch stratType {
	case "perps":
		switch platform {
		case "hyperliquid":
			return priceSourceHyperliquid
		case "okx":
			return priceSourceOKX
		}
	case "manual":
		if platform == "hyperliquid" {
			return priceSourceHyperliquid
		}
	case "futures":
		return priceSourceFutures
	case "spot":
		return priceSourceSpot
	}
	return ""
}

func finiteNumber(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func bookValuationGaps(sc StrategyConfig, s *StrategyState, prices map[string]float64, sources cyclePriceSources, intervalSeconds int, armedAt, now time.Time) []string {
	var gaps []string
	if !finiteNumber(s.Cash) {
		gaps = append(gaps, fmt.Sprintf("%s cash is not finite", sc.ID))
	}
	want := expectedPriceSource(s.Platform, s.Type)
	syms := make([]string, 0, len(s.Positions))
	for sym := range s.Positions {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	for _, sym := range syms {
		pos := s.Positions[sym]
		if pos == nil {
			continue
		}
		if !finitePositive(pos.Quantity) || !finitePositive(pos.AvgCost) || !finiteNumber(pos.Multiplier) {
			gaps = append(gaps, fmt.Sprintf("%s %s position inputs are not valid", sc.ID, sym))
			continue
		}
		px, ok := prices[sym]
		if !ok || !finitePositive(px) {
			gaps = append(gaps, fmt.Sprintf("%s %s has no current mark", sc.ID, sym))
			continue
		}
		src := sources[sym]
		switch {
		case src == "":
			gaps = append(gaps, fmt.Sprintf("%s %s mark has no source this cycle", sc.ID, sym))
		case src == priceSourceFuturesDegraded:
			gaps = append(gaps, fmt.Sprintf("%s %s mark came from the degraded futures fallback", sc.ID, sym))
		case want != "" && src != want:
			gaps = append(gaps, fmt.Sprintf("%s %s mark came from the %s source, not %s", sc.ID, sym, src, want))
		}
	}
	clock := newOptionHarvestClock(sc, intervalSeconds, now)
	for _, key := range sortedOptionLegKeys(s.OptionPositions) {
		leg := s.OptionPositions[key]
		if leg == nil {
			continue
		}
		if !finiteNumber(leg.CurrentValueUSD) {
			gaps = append(gaps, fmt.Sprintf("%s option %s value is not finite", sc.ID, key))
			continue
		}
		if optionLegAwaitingSettlement(s.Platform, leg, now) {
			continue
		}
		if _, reason := optionHarvestEligibility(leg, clock); reason != "" {
			gaps = append(gaps, fmt.Sprintf("%s option %s: %s", sc.ID, key, reason))
			continue
		}
		if !armedAt.IsZero() && !leg.markVerifiedAt.After(armedAt) {
			gaps = append(gaps, fmt.Sprintf("%s option %s has no verified mark after the reset", sc.ID, key))
		}
	}
	return gaps
}

func measureScopeRebaselineValuation(
	sr *scopeCycleRisk,
	prs *PortfolioRiskState,
	cfgStrategies []StrategyConfig,
	state *AppState,
	prices map[string]float64,
	sources cyclePriceSources,
	walletBalances map[SharedWalletKey]float64,
	sharedWallets map[SharedWalletKey][]string,
	intervalSeconds int,
	inputsFrom time.Time,
	now time.Time,
) {
	if sr == nil {
		return
	}
	sr.ValuationComplete = false
	sr.ValuationGap = ""
	sr.InputsFrom = inputsFrom
	if prs == nil || !prs.PeakRebaselinePending {
		return
	}
	scopedCfgs := strategiesInPartition(cfgStrategies, sr.Partition)
	if !sr.Partition.IsLive() {
		walletBalances = nil
	}
	_, _, markValued := computeSubsetPortfolioValuation(scopedCfgs, state, prices, walletBalances, sharedWallets)
	byID := make(map[string]StrategyConfig, len(scopedCfgs))
	for _, sc := range scopedCfgs {
		byID[sc.ID] = sc
	}
	var gaps []string
	for _, id := range markValued {
		s := state.Strategies[id]
		if s == nil {
			continue
		}
		gaps = append(gaps, bookValuationGaps(byID[id], s, prices, sources, intervalSeconds, prs.peakRebaselineArmedAt, now)...)
	}
	if len(gaps) > 0 {
		sr.ValuationGap = strings.Join(gaps, "; ")
		return
	}
	sr.ValuationComplete = true
}

func scopeRebaselineWaitReason(sr *scopeCycleRisk, prs *PortfolioRiskState) string {
	switch {
	case prs == nil || !prs.PeakRebaselinePending:
		return ""
	case prs.KillSwitchActive:
		return "the partition is latched"
	case !sr.EquityAvailable:
		return "equity is unavailable this cycle"
	case !sr.EquityTrusted || !sr.PeakRebaselineAvailable:
		return "the equity total is not trusted this cycle"
	case !finitePositive(sr.TotalPV):
		return "the equity total is not a positive finite amount"
	case !rebaselineInputsAfterReset(sr, prs):
		return "this cycle's inputs were acquired before the reset"
	case !sr.ValuationComplete:
		if sr.ValuationGap != "" {
			return "valuation is incomplete: " + sr.ValuationGap
		}
		return "valuation is incomplete"
	}
	return ""
}

func formatPeakRebaselineDM(part RiskPartition, oldPeak, newPeak float64) string {
	return fmt.Sprintf("Kill switch reset follow-up (%s scope): the drawdown peak moved from $%.2f to $%.2f, the first eligible equity reading after the reset. Drawdown is now measured from $%.2f. The reset and this new peak do not close or protect any open position.",
		partitionLabel(part), oldPeak, newPeak, newPeak)
}
