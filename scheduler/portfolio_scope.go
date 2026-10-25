package main

import (
	"fmt"
	"sort"
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
	PairValuation           pairValueStatus
	PairValuationOverdue    map[string]int64
	PairValuationOverdueMs  int64
	PairValuationSince      map[string]int64
	pairMarked              bool
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
	return measureScopeCycleRiskMarked(part, pr, cfgStrategies, state, prices, riskWalletBalances, sharedWallets, pooledEquityComplete, usedStaleRiskBalance, now, nil)
}

func measureScopeCycleRiskMarked(
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
	pm *pairMarks,
) *scopeCycleRisk {
	sr := &scopeCycleRisk{Partition: part, Config: pr}
	scopedCfgs := strategiesInPartition(cfgStrategies, part)
	scopedStates := filterStatesByPartition(state.Strategies, cfgStrategies, part)
	var status pairValueStatus
	if part.IsLive() {
		sr.TotalPV, sr.UsedPVFallback, status = computeSubsetPortfolioValueMarked(scopedCfgs, state, prices, riskWalletBalances, sharedWallets, pm)
		sr.EquityAvailable = pooledEquityComplete
		sr.EquityTrusted = pooledEquityComplete && !sr.UsedPVFallback && !usedStaleRiskBalance
		sr.PeakRebaselineAvailable = portfolioPeakRebaselineAvailable(sr.UsedPVFallback, usedStaleRiskBalance, pooledEquityComplete)
	} else {
		sr.TotalPV, _, status = computeSubsetPortfolioValueMarked(scopedCfgs, state, prices, nil, sharedWallets, pm)
		sr.EquityAvailable = true
		sr.EquityTrusted = true
		sr.PeakRebaselineAvailable = true
	}
	sr.PairValuation.merge(status)
	var ns, ms pairValueStatus
	sr.TotalNotional, ns = portfolioNotionalMarked(scopedStates, prices, pm)
	sr.PerpsLoss, sr.PerpsMargin, ms = aggregatePerpsMarginInputsMarked(scopedStates, scopedCfgs, prices, pm)
	sr.PairValuation.merge(ns)
	sr.PairValuation.merge(ms)
	sr.PairValuationOverdueMs, sr.PairValuationOverdue = pm.partitionOverdue(sr.PairValuation)
	sr.PairValuationSince = pm.sinces(sr.PairValuation)
	sr.pairMarked = pm != nil
	sr.DailyLossStatus = evaluateDailyLossLimit(pr, scopedStates, scopedCfgs, now)
	sr.ExposureCapStatus = evaluateExposureCapMarked(pr, scopedStates, scopedCfgs, prices, sr.TotalPV, pm)
	return sr
}

func recordPairValuationIncompleteSince(prs *PortfolioRiskState, complete bool, since map[string]int64) {
	if complete {
		prs.PairValuationIncompleteSince = time.Time{}
		return
	}
	var earliest int64
	for _, ms := range since {
		if ms > 0 && (earliest == 0 || ms < earliest) {
			earliest = ms
		}
	}
	if earliest == 0 {
		return
	}
	if t := time.UnixMilli(earliest).UTC(); prs.PairValuationIncompleteSince.IsZero() || t.Before(prs.PairValuationIncompleteSince) {
		prs.PairValuationIncompleteSince = t
	}
}

func applyScopeCycleRisk(sr *scopeCycleRisk, prs *PortfolioRiskState) {
	sr.Prs = prs
	pairComplete := sr.PairValuation.complete()
	if !pairComplete {
		sr.PeakRebaselineAvailable = false
	}
	if sr.pairMarked {
		recordPairValuationIncompleteSince(prs, pairComplete, sr.PairValuationSince)
	}
	origPeak := prs.PeakValue
	prevWarningSent := prs.WarningSent
	allowed, notionalBlocked, warning, reason := checkPortfolioRiskWithEquityAvailability(
		prs, sr.Config, sr.TotalPV, sr.TotalNotional, sr.PerpsLoss, sr.PerpsMargin, sr.EquityAvailable, sr.EquityTrusted && pairComplete)
	if allowed && len(sr.PairValuationOverdue) > 0 && sr.Config != nil && sr.Config.MaxDrawdownPct > 0 {
		allowed, notionalBlocked, warning = false, false, false
		reason = latchPortfolioKillSwitchForPairValuation(prs, sr.TotalPV, fmt.Sprintf("the partition valuation has been incomplete without a break since %s; %s",
			time.UnixMilli(sr.PairValuationOverdueMs).UTC().Format(time.RFC3339), pairOverdueDetail(sr.PairValuationOverdue, sr.PairValuation)))
	}
	if !pairComplete {
		if sr.Config != nil && sr.Config.MaxNotionalUSD > 0 {
			notionalBlocked = true
		}
		note := sr.PairValuation.label() + "; the reading values those members conservatively, never raises the peak and goes through the untrusted over-limit deferral, and new opens hold"
		if reason == "" {
			reason = note
		} else {
			reason += "; " + note
		}
	}
	sr.Warning = warning
	sr.WarnBandEntered = warning && !prevWarningSent
	sr.Reason = reason
	if !sr.PeakRebaselineAvailable && prs.PeakValue > origPeak {
		prs.PeakValue = origPeak
	}
	sr.EquityGuardArmed = sr.EquityAvailable && prs.PeakValue > 0
	sr.KillSwitchFired = !allowed
	sr.NotionalBlocked = notionalBlocked
	sr.DailyLossEntriesHeld = sr.DailyLossStatus.Tripped
}
