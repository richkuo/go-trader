package main

import (
	"strings"
	"testing"
	"time"
)

func TestPortfolioWarningContributors_FilterPausedByDefault(t *testing.T) {
	cfgStrategies := []StrategyConfig{
		{ID: "live-active-eth", Type: "futures", Args: []string{"--mode=live"}},
		{ID: "live-paused-btc", Type: "futures", Args: []string{"--mode=live"}, Paused: true},
		{ID: "paper-only-sol", Type: "futures"},
	}
	state := NewAppState()
	state.Strategies["live-active-eth"] = &StrategyState{
		ID: "live-active-eth", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 5},
	}
	state.Strategies["live-paused-btc"] = &StrategyState{
		ID: "live-paused-btc", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 25},
	}
	state.Strategies["paper-only-sol"] = &StrategyState{
		ID: "paper-only-sol", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 10},
	}
	// Force live-paused-btc to be the worst P&L so it would dominate if not filtered.
	// After: pv = 50 → PnL = -50. live-active-eth: pv = 110 → PnL = +10. paper-only-sol excluded by scope.
	state.Strategies["live-paused-btc"].Cash = 50 // fake a -50 PnL vs initial 100
	state.Strategies["live-active-eth"].Cash = 110

	prices := map[string]float64{"ETH": 3000, "BTC": 60000, "SOL": 100}

	contribs := portfolioWarningContributors(state, cfgStrategies, ScopeLive, prices, false)
	for _, c := range contribs {
		if c.ID == "live-paused-btc" {
			t.Fatalf("paused strategy surfaced in contributors: %+v", c)
		}
	}
	if got := portfolioWarningPausedExcluded[ScopeLive]; got != 1 {
		t.Fatalf("portfolioWarningPausedExcluded[ScopeLive] = %d, want 1", got)
	}
}

func TestPortfolioWarningContributors_IncludePausedOptIn(t *testing.T) {
	cfgStrategies := []StrategyConfig{
		{ID: "live-paused-btc", Type: "futures", Args: []string{"--mode=live"}, Paused: true},
	}
	state := NewAppState()
	state.Strategies["live-paused-btc"] = &StrategyState{
		ID: "live-paused-btc", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 25},
	}
	state.Strategies["live-paused-btc"].Cash = 50
	prices := map[string]float64{"BTC": 60000}

	contribs := portfolioWarningContributors(state, cfgStrategies, ScopeLive, prices, true)
	if len(contribs) != 1 || contribs[0].ID != "live-paused-btc" {
		t.Fatalf("IncludePausedInWarning=true should keep paused strategy, got: %+v", contribs)
	}
}

func TestPortfolioWarningMessage_PausedFootnote(t *testing.T) {
	cfgStrategies := []StrategyConfig{
		{ID: "live-paused-btc", Type: "futures", Args: []string{"--mode=live"}, Paused: true},
		{ID: "live-active-eth", Type: "futures", Args: []string{"--mode=live"}},
	}
	state := NewAppState()
	state.Strategies["live-paused-btc"] = &StrategyState{
		ID: "live-paused-btc", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 30},
	}
	state.Strategies["live-active-eth"] = &StrategyState{
		ID: "live-active-eth", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 5},
	}
	state.Strategies["live-paused-btc"].Cash = 50
	state.Strategies["live-active-eth"].Cash = 100
	state.PortfolioRisk = map[PortfolioScope]*PortfolioRiskState{
		ScopeLive: {PeakValue: 200, WarningSent: true, WarnBandEnteredAt: time.Now().UTC()},
	}

	prices := map[string]float64{"ETH": 3000, "BTC": 60000}
	msg := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Reason:        "test",
		Config:        &PortfolioRiskConfig{MaxDrawdownPct: 30, WarnThresholdPct: 60},
		State:         state,
		Scope:         ScopeLive,
		CfgStrategies: cfgStrategies,
		Prices:        prices,
		TotalValue:    150,
		PerpsMargin:   50,
		PerpsLoss:     15,
		Now:           time.Now().UTC(),
		EquityGuardArmed: true,
	})
	if !strings.Contains(msg, "live-paused-btc") {
		// OK — paused strategy shouldn't appear in Top contributors.
		for _, line := range strings.Split(msg, "\n") {
			if strings.Contains(line, "live-paused-btc") {
				t.Fatalf("paused strategy leaked into Top contributors line: %q", line)
			}
		}
	}
	if !strings.Contains(msg, "paused strateg") || !strings.Contains(msg, "excluded from contributors") {
		t.Fatalf("expected paused footnote in message, got:\n%s", msg)
	}
}

func TestPortfolioWarningAlertsReset_ClearsExcludedCounter(t *testing.T) {
	portfolioWarningPausedExcluded[ScopeLive] = 7
	portfolioWarningAlerts[ScopeLive] = portfolioWarningAlertState{Notified: true}

	portfolioWarningAlertsReset(ScopeLive)

	if _, ok := portfolioWarningPausedExcluded[ScopeLive]; ok {
		t.Fatalf("portfolioWarningPausedExcluded[ScopeLive] should be cleared")
	}
	if _, ok := portfolioWarningAlerts[ScopeLive]; ok {
		t.Fatalf("portfolioWarningAlerts[ScopeLive] should be cleared")
	}
}
