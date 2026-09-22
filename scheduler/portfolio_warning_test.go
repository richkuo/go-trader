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
		{ID: "live-paused-stale", Type: "futures", Args: []string{"--mode=live"}, Paused: true},
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
	state.Strategies["live-paused-stale"] = &StrategyState{
		ID: "live-paused-stale", InitialCapital: 100,
		Positions: map[string]*Position{"BTC": {Symbol: "BTC", Side: "long", Quantity: 0, AvgCost: 60000}},
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
	state.Strategies["live-paused-stale"].Cash = 50
	state.Strategies["live-paused-btc"].Cash = 0
	state.Strategies["live-paused-options"].Cash = 0
	state.Strategies["live-active-eth"].Cash = 110

	prices := map[string]float64{"ETH": 3000, "BTC": 60000, "SOL": 100}

	contribs, excluded := portfolioWarningContributors(state, cfgStrategies, livePartition, prices, false)
	got := make(map[string]bool, len(contribs))
	for _, c := range contribs {
		got[c.ID] = true
	}
	for _, id := range []string{"live-paused-flat", "live-paused-stale"} {
		if got[id] {
			t.Fatalf("flat paused strategy %q surfaced in contributors: %+v", id, contribs)
		}
	}
	for _, id := range []string{"live-active-eth", "live-paused-btc", "live-paused-options"} {
		if !got[id] {
			t.Fatalf("strategy %q missing from contributors: %+v", id, contribs)
		}
	}
	if excluded != 2 {
		t.Fatalf("excluded flat paused count = %d, want 2", excluded)
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

	_, liveExcluded := portfolioWarningContributors(state, cfgStrategies, livePartition, nil, false)
	_, paperExcluded := portfolioWarningContributors(state, cfgStrategies, defaultPaperPartition, nil, false)
	_, sourceExcluded := portfolioWarningContributors(state, cfgStrategies, paperSourcePartition("source-a"), nil, false)
	_, nilStateExcluded := portfolioWarningContributors(nil, cfgStrategies, livePartition, nil, false)

	if liveExcluded != 1 {
		t.Fatalf("live excluded count = %d, want 1", liveExcluded)
	}
	if paperExcluded != 1 {
		t.Fatalf("paper excluded count = %d, want 1", paperExcluded)
	}
	if sourceExcluded != 1 {
		t.Fatalf("paper source excluded count = %d, want 1", sourceExcluded)
	}
	if nilStateExcluded != 0 {
		t.Fatalf("nil state excluded count = %d, want 0", nilStateExcluded)
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

	contribs, excluded := portfolioWarningContributors(state, cfgStrategies, livePartition, prices, true)
	if len(contribs) != 1 || contribs[0].ID != "live-paused-btc" {
		t.Fatalf("IncludePausedInWarning=true should keep paused strategy, got: %+v", contribs)
	}
	if excluded != 0 {
		t.Fatalf("IncludePausedInWarning=true excluded count = %d, want 0", excluded)
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
		Recent:           []Trade{{StrategyID: "live-paused-btc", Side: "buy", Quantity: 1, Price: 1, Timestamp: time.Now().UTC()}},
		Now:              time.Now().UTC(),
		EquityGuardArmed: true,
	})
	const contributorsPrefix = "Top contributors:\n```\n"
	start := strings.Index(msg, contributorsPrefix)
	if start < 0 {
		t.Fatalf("warning missing contributors block: %s", msg)
	}
	blockStart := start + len(contributorsPrefix)
	end := strings.Index(msg[blockStart:], "```\n")
	if end < 0 {
		t.Fatalf("warning contributors block is not closed: %s", msg)
	}
	if block := msg[blockStart : blockStart+end]; strings.Contains(block, "live-paused-btc") {
		t.Fatalf("paused strategy leaked into Top contributors block: %q", block)
	}
	if !strings.Contains(msg, "live-paused-btc") {
		t.Fatalf("recent activity fixture missing from warning: %s", msg)
	}
	if !strings.Contains(msg, "paused strateg") || !strings.Contains(msg, "excluded from contributors") {
		t.Fatalf("expected paused footnote in message, got:\n%s", msg)
	}
}
