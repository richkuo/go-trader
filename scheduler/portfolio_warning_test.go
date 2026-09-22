package main

import (
	"strings"
	"testing"
	"time"
)

func TestPortfolioWarningContributors_FilterPausedByDefault(t *testing.T) {
	cfgStrategies := []StrategyConfig{
		{ID: "live-active-eth", Type: "futures", Args: []string{"--mode=live"}},
		{ID: "live-paused-flat", Type: "futures", Args: []string{"--mode=live"}, Paused: true},
		{ID: "live-paused-btc", Type: "futures", Args: []string{"--mode=live"}, Paused: true},
		{ID: "live-paused-options", Type: "options", Args: []string{"--mode=live"}, Paused: true},
		{ID: "paper-only-sol", Type: "futures"},
	}
	state := NewAppState()
	state.Strategies["live-active-eth"] = &StrategyState{
		ID: "live-active-eth", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 5},
	}
	state.Strategies["live-paused-flat"] = &StrategyState{
		ID: "live-paused-flat", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 25},
	}
	state.Strategies["live-paused-btc"] = &StrategyState{
		ID: "live-paused-btc", InitialCapital: 100,
		Positions: map[string]*Position{"BTC": {Symbol: "BTC", Side: "long", Quantity: 0.001, AvgCost: 60000}},
		RiskState: RiskState{CurrentDrawdownPct: 25},
	}
	state.Strategies["live-paused-options"] = &StrategyState{
		ID: "live-paused-options", InitialCapital: 100,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{"BTC-call": {ID: "BTC-call", Quantity: 1, CurrentValueUSD: 50}},
		RiskState:       RiskState{CurrentDrawdownPct: 25},
	}
	state.Strategies["paper-only-sol"] = &StrategyState{
		ID: "paper-only-sol", InitialCapital: 100,
		Positions: map[string]*Position{},
		RiskState: RiskState{CurrentDrawdownPct: 10},
	}
	state.Strategies["live-paused-flat"].Cash = 50
	state.Strategies["live-paused-btc"].Cash = 0
	state.Strategies["live-paused-options"].Cash = 0
	state.Strategies["live-active-eth"].Cash = 110

	prices := map[string]float64{"ETH": 3000, "BTC": 60000, "SOL": 100}

	contribs := portfolioWarningContributors(state, cfgStrategies, livePartition, prices, false)
	got := make(map[string]bool, len(contribs))
	for _, c := range contribs {
		got[c.ID] = true
	}
	if got["live-paused-flat"] {
		t.Fatalf("flat paused strategy surfaced in contributors: %+v", contribs)
	}
	for _, id := range []string{"live-active-eth", "live-paused-btc", "live-paused-options"} {
		if !got[id] {
			t.Fatalf("strategy %q missing from contributors: %+v", id, contribs)
		}
	}
	if got := portfolioWarningFlatPausedExcluded[livePartition]; got != 1 {
		t.Fatalf("portfolioWarningFlatPausedExcluded[ScopeLive] = %d, want 1", got)
	}
}

func TestPortfolioWarningContributors_PartitionIsolation(t *testing.T) {
	cfgStrategies := []StrategyConfig{
		{ID: "live-flat", Type: "futures", Args: []string{"--mode=live"}, Paused: true},
		{ID: "paper-flat", Type: "futures", Paused: true},
		{ID: "source-flat", Type: "futures", PaperSource: "source-a", Paused: true},
	}
	state := NewAppState()
	state.Strategies["live-flat"] = &StrategyState{ID: "live-flat", Positions: map[string]*Position{}}
	state.Strategies["paper-flat"] = &StrategyState{ID: "paper-flat", Positions: map[string]*Position{}}
	state.Strategies["source-flat"] = &StrategyState{ID: "source-flat", Positions: map[string]*Position{}}

	portfolioWarningContributors(state, cfgStrategies, livePartition, nil, false)
	portfolioWarningContributors(state, cfgStrategies, defaultPaperPartition, nil, false)
	portfolioWarningContributors(state, cfgStrategies, paperSourcePartition("source-a"), nil, false)

	if got := portfolioWarningFlatPausedExcluded[livePartition]; got != 1 {
		t.Fatalf("live excluded count = %d, want 1", got)
	}
	if got := portfolioWarningFlatPausedExcluded[defaultPaperPartition]; got != 1 {
		t.Fatalf("paper excluded count = %d, want 1", got)
	}
	if got := portfolioWarningFlatPausedExcluded[paperSourcePartition("source-a")]; got != 1 {
		t.Fatalf("paper source excluded count = %d, want 1", got)
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

	contribs := portfolioWarningContributors(state, cfgStrategies, livePartition, prices, true)
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
	state.PortfolioRisk = map[RiskPartition]*PortfolioRiskState{
		livePartition: {PeakValue: 200, WarningSent: true, WarnBandEnteredAt: time.Now().UTC()},
	}

	prices := map[string]float64{"ETH": 3000, "BTC": 60000}
	msg := BuildPortfolioWarningMessage(PortfolioWarningMessageInputs{
		Reason:           "test",
		Config:           &PortfolioRiskConfig{MaxDrawdownPct: 30, WarnThresholdPct: 60},
		State:            state,
		Partition:        livePartition,
		CfgStrategies:    cfgStrategies,
		Prices:           prices,
		TotalValue:       150,
		PerpsMargin:      50,
		PerpsLoss:        15,
		Now:              time.Now().UTC(),
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
	portfolioWarningFlatPausedExcluded[livePartition] = 7
	portfolioWarningAlerts[livePartition] = portfolioWarningAlertState{Notified: true}

	portfolioWarningAlertsReset(livePartition)

	if _, ok := portfolioWarningFlatPausedExcluded[livePartition]; ok {
		t.Fatalf("portfolioWarningFlatPausedExcluded[ScopeLive] should be cleared")
	}
	if _, ok := portfolioWarningAlerts[livePartition]; ok {
		t.Fatalf("portfolioWarningAlerts[ScopeLive] should be cleared")
	}
}
