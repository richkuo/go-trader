package main

import (
	"strings"
	"testing"
	"time"
)

func TestPortfolioScopeFor(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want PortfolioScope
	}{
		{"mode equals live", []string{"momentum", "BTC", "--mode=live"}, ScopeLive},
		{"mode space live", []string{"momentum", "BTC", "--mode", "live"}, ScopeLive},
		{"mode equals paper", []string{"momentum", "BTC", "--mode=paper"}, ScopePaper},
		{"no mode flag", []string{"momentum", "BTC"}, ScopePaper},
		{"bare live positional is not live", []string{"momentum", "BTC", "live"}, ScopePaper},
		{"mode space paper", []string{"momentum", "BTC", "--mode", "paper"}, ScopePaper},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := portfolioScopeFor(StrategyConfig{Args: tc.args}); got != tc.want {
				t.Errorf("portfolioScopeFor(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
	if hyperliquidModeFromArgs([]string{"momentum", "BTC", "live"}) != "paper" {
		t.Error("the batch slot mode must follow isLiveArgs, so a bare live positional stays paper")
	}
	if hyperliquidModeFromArgs([]string{"momentum", "BTC", "--mode=live"}) != "live" {
		t.Error("the batch slot mode must report live for --mode=live")
	}
}

func scopeTestConfig(live, paper bool) *Config {
	cfg := &Config{PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60}}
	if live {
		cfg.Strategies = append(cfg.Strategies, scopeCfg("live-a", true))
	}
	if paper {
		cfg.Strategies = append(cfg.Strategies, scopeCfg("paper-a", false))
	}
	return cfg
}

func scopeTestState(cfg *Config, live, paper float64) *AppState {
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		s := scopeState(sc.ID, live)
		if portfolioScopeFor(sc) == ScopePaper {
			s.Cash = paper
		}
		state.Strategies[sc.ID] = s
	}
	return state
}

func TestCycleScopeRisk_LiveLatchLeavesPaperFree(t *testing.T) {
	cfg := scopeTestConfig(true, true)
	state := scopeTestState(cfg, 5000, 9900)
	res := runScopeCycle(t, cfg, state, map[RiskPartition]float64{livePartition: 10000, defaultPaperPartition: 10000})

	if !res[livePartition].KillSwitchFired {
		t.Fatalf("live 50%% drawdown must latch; reason=%q", res[livePartition].Reason)
	}
	if res[defaultPaperPartition].KillSwitchFired {
		t.Fatalf("paper under the limit must stay free; reason=%q", res[defaultPaperPartition].Reason)
	}
	if !state.partitionLatched(livePartition) || state.partitionLatched(defaultPaperPartition) {
		t.Errorf("latched scopes = %v, want live only", state.latchedPartitions())
	}
	due := dueStrategiesNotLatched(cfg.Strategies, res)
	if len(due) != 1 || due[0].ID != "paper-a" {
		t.Errorf("unlatched due set = %v, want the paper strategy only", due)
	}
}

func TestCycleScopeRisk_SingleModeMatchesLegacy(t *testing.T) {
	for _, live := range []bool{true, false} {
		cfg := scopeTestConfig(live, !live)
		state := scopeTestState(cfg, 9000, 9000)
		scope := livePartition
		if !live {
			scope = defaultPaperPartition
		}
		res := runScopeCycle(t, cfg, state, map[RiskPartition]float64{scope: 10000})

		legacyPrs := &PortfolioRiskState{PeakValue: 10000}
		legacyStates := map[string]*StrategyState{}
		for id, s := range state.Strategies {
			legacyStates[id] = s
		}
		legacyPV, _ := computeSubsetPortfolioValue(cfg.Strategies, state, nil, nil, nil)
		legacyNotional := PortfolioNotional(legacyStates, nil)
		legacyLoss, legacyMargin := AggregatePerpsMarginInputs(legacyStates, cfg.Strategies, nil)
		allowed, nb, warning, reason := checkPortfolioRiskWithEquityAvailability(
			legacyPrs, cfg.PortfolioRisk, legacyPV, legacyNotional, legacyLoss, legacyMargin, true, true)

		sr := res[scope]
		if sr.TotalPV != legacyPV || sr.TotalNotional != legacyNotional || sr.PerpsLoss != legacyLoss || sr.PerpsMargin != legacyMargin {
			t.Fatalf("scope inputs diverged from the whole-portfolio inputs: %+v vs pv=%v notional=%v loss=%v margin=%v",
				sr, legacyPV, legacyNotional, legacyLoss, legacyMargin)
		}
		if sr.KillSwitchFired == allowed || sr.NotionalBlocked != nb || sr.Warning != warning || sr.Reason != reason {
			t.Fatalf("scope decision diverged: fired=%v blocked=%v warning=%v reason=%q vs allowed=%v nb=%v warning=%v reason=%q",
				sr.KillSwitchFired, sr.NotionalBlocked, sr.Warning, sr.Reason, allowed, nb, warning, reason)
		}
		got := state.partitionRisk(scope)
		if got.PeakValue != legacyPrs.PeakValue || got.CurrentDrawdownPct != legacyPrs.CurrentDrawdownPct ||
			got.CurrentMarginDrawdownPct != legacyPrs.CurrentMarginDrawdownPct || got.KillSwitchActive != legacyPrs.KillSwitchActive ||
			got.WarningSent != legacyPrs.WarningSent {
			t.Fatalf("single-mode prs diverged: %+v vs %+v", got, legacyPrs)
		}
	}
}

func scopeLongPosState(id string, qty float64) *StrategyState {
	return &StrategyState{
		ID: id, Type: "perps", Platform: "hyperliquid", InitialCapital: 10000,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: qty, AvgCost: 50000, Side: "long", Multiplier: 1},
		},
		OptionPositions: map[string]*OptionPosition{},
	}
}

func TestRebaselinePeakAfterPrune_PerScope(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}}
	state := NewAppState()
	for _, sc := range cfg.Strategies {
		s := scopeState(sc.ID, 10000)
		s.RiskState.PeakValue = 12000
		state.Strategies[sc.ID] = s
	}
	before := rebaselinePortfolioPeakAfterPruneForPartition(state, cfg, livePartition, nil)
	delete(state.Strategies, "paper-a")
	after := rebaselinePortfolioPeakAfterPruneForPartition(state, cfg, livePartition, nil)
	if before != after {
		t.Errorf("pruning a paper strategy must not move the live peak: %v -> %v", before, after)
	}
	paperAfter := rebaselinePortfolioPeakAfterPruneForPartition(state, cfg, defaultPaperPartition, nil)
	if paperAfter >= before+12000 {
		t.Errorf("the paper peak must fall back to its own configured capital, got %v", paperAfter)
	}
	if got := computeInitialPortfolioPeakForPartition(cfg.Strategies, livePartition, nil); got != 10000 {
		t.Errorf("live initial peak = %v, want 10000 (its own capital only)", got)
	}
}

func TestPaperKillSwitch_ForceClosesPaperOnly(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}}
	state := NewAppState()
	state.Strategies["live-a"] = scopeLongPosState("live-a", 0.2)
	state.Strategies["paper-a"] = scopeLongPosState("paper-a", 0.4)

	closed := forceClosePaperScopePositions(state, cfg, defaultPaperPartition, map[string]float64{"BTC": 50000})
	if len(closed) != 1 || closed[0] != "paper-a" {
		t.Fatalf("closed = %v, want the paper strategy only", closed)
	}
	if len(state.Strategies["paper-a"].Positions) != 0 {
		t.Error("the paper book must be flat after a paper latch")
	}
	if len(state.Strategies["live-a"].Positions) != 1 {
		t.Error("a paper latch must never touch a live book")
	}
	msg := formatPaperKillSwitchMessage(defaultPaperPartition, "paper drawdown 50.0% exceeds limit 25.0%", closed)
	for _, want := range []string{"PAPER", "paper-a", "No exchange order was sent", "reset paper"} {
		if !strings.Contains(msg, want) {
			t.Errorf("paper kill-switch message missing %q:\n%s", want, msg)
		}
	}
}

func TestAutoResetConfirmedFlat_LiveScopeOnly(t *testing.T) {
	state := NewAppState()
	state.partitionRisk(livePartition).KillSwitchActive = true
	state.partitionRisk(livePartition).KillSwitchAt = time.Now().UTC()
	state.partitionRisk(defaultPaperPartition).KillSwitchActive = true
	state.partitionRisk(defaultPaperPartition).KillSwitchAt = time.Now().UTC()

	if !AutoResetConfirmedFlatKillSwitch(state.partitionRisk(livePartition), 10000, true, "confirmed flat") {
		t.Fatal("the live latch must auto-reset when confirmed flat with no owner")
	}
	if state.partitionLatched(livePartition) {
		t.Error("live must be cleared")
	}
	if !state.partitionLatched(defaultPaperPartition) {
		t.Error("paper must never auto-reset without an owner")
	}
	if state.partitionRisk(livePartition).KillSwitchCloseApplied {
		t.Error("an auto reset must clear the one-shot close marker so a later latch closes again")
	}
}
