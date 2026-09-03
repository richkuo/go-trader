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
	Scope                   PortfolioScope
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
}

func scopePrefixedDM(scope PortfolioScope, msg string) string {
	return fmt.Sprintf("[%s scope] %s", scopeLabel(scope), msg)
}

func scopeCycleRiskFired(scopeRisk map[PortfolioScope]*scopeCycleRisk, scope PortfolioScope) bool {
	sr, ok := scopeRisk[scope]
	return ok && sr != nil && sr.KillSwitchFired
}

func dueStrategiesNotLatched(due []StrategyConfig, scopeRisk map[PortfolioScope]*scopeCycleRisk) []StrategyConfig {
	out := make([]StrategyConfig, 0, len(due))
	for _, sc := range due {
		if scopeCycleRiskFired(scopeRisk, portfolioScopeFor(sc)) {
			continue
		}
		out = append(out, sc)
	}
	return out
}

func measureScopeCycleRisk(
	scope PortfolioScope,
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
	sr := &scopeCycleRisk{Scope: scope, Config: pr}
	scopedCfgs := strategiesInScope(cfgStrategies, scope)
	scopedStates := filterStatesByScope(state.Strategies, cfgStrategies, scope)
	if scope == ScopeLive {
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
	sr.TotalNotional = PortfolioNotional(scopedStates, prices)
	sr.PerpsLoss, sr.PerpsMargin = AggregatePerpsMarginInputs(scopedStates, scopedCfgs, prices)
	sr.DailyLossStatus = evaluateDailyLossLimit(pr, scopedStates, scopedCfgs, now)
	sr.ExposureCapStatus = evaluateExposureCap(pr, scopedStates, scopedCfgs, prices, sr.TotalPV)
	return sr
}

func applyScopeCycleRisk(sr *scopeCycleRisk, prs *PortfolioRiskState) {
	sr.Prs = prs
	origPeak := prs.PeakValue
	prevWarningSent := prs.WarningSent
	allowed, notionalBlocked, warning, reason := checkPortfolioRiskWithEquityAvailability(
		prs, sr.Config, sr.TotalPV, sr.TotalNotional, sr.PerpsLoss, sr.PerpsMargin, sr.EquityAvailable, sr.EquityTrusted)
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
