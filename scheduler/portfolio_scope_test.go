package main

import (
	"strings"
	"testing"
	"time"
)

func scopeCfg(id string, live bool) StrategyConfig {
	args := []string{"momentum", "BTC", "1h"}
	if live {
		args = append(args, "--mode=live")
	}
	return StrategyConfig{ID: id, Type: "perps", Platform: "hyperliquid", Capital: 10000, InitialCapital: 10000, Leverage: 3, Args: args}
}

func scopeState(id string, cash float64) *StrategyState {
	return &StrategyState{
		ID:              id,
		Type:            "perps",
		Platform:        "hyperliquid",
		Cash:            cash,
		InitialCapital:  10000,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{},
	}
}

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

func TestActiveScopes_OnlyConfiguredScopes(t *testing.T) {
	cases := []struct {
		name string
		cfgs []StrategyConfig
		want []PortfolioScope
	}{
		{"live only", []StrategyConfig{scopeCfg("a", true)}, []PortfolioScope{ScopeLive}},
		{"paper only", []StrategyConfig{scopeCfg("a", false)}, []PortfolioScope{ScopePaper}},
		{"mixed orders live first", []StrategyConfig{scopeCfg("p", false), scopeCfg("l", true)}, []PortfolioScope{ScopeLive, ScopePaper}},
		{"empty roster", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := activeScopes(tc.cfgs)
			if len(got) != len(tc.want) {
				t.Fatalf("activeScopes = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("activeScopes = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestStrategiesInScope_AndStateFilters(t *testing.T) {
	cfgs := []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-b", false), scopeCfg("live-c", true)}
	states := map[string]*StrategyState{
		"live-a":  scopeState("live-a", 1000),
		"paper-b": scopeState("paper-b", 2000),
		"live-c":  scopeState("live-c", 3000),
		"orphan":  scopeState("orphan", 4000),
	}
	if got := len(strategiesInScope(cfgs, ScopeLive)); got != 2 {
		t.Errorf("live strategies = %d, want 2", got)
	}
	live := filterStatesByScope(states, cfgs, ScopeLive)
	if len(live) != 2 || live["paper-b"] != nil || live["orphan"] != nil {
		t.Errorf("live state subset = %v, want live-a and live-c only", live)
	}
	ids := stateIDsInScope(states, cfgs, ScopeLive)
	if len(ids) != 2 || ids[0] != "live-a" || ids[1] != "live-c" {
		t.Errorf("stateIDsInScope = %v, want sorted [live-a live-c]", ids)
	}
	if scope, ok := scopeOfStrategyID(cfgs, "paper-b"); !ok || scope != ScopePaper {
		t.Errorf("scopeOfStrategyID(paper-b) = %q,%v, want paper,true", scope, ok)
	}
	if _, ok := scopeOfStrategyID(cfgs, "nope"); ok {
		t.Error("an unknown id must report not-found")
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

func runScopeCycle(t *testing.T, cfg *Config, state *AppState, peaks map[PortfolioScope]float64) map[PortfolioScope]*scopeCycleRisk {
	t.Helper()
	out := make(map[PortfolioScope]*scopeCycleRisk)
	now := time.Now().UTC()
	for _, scope := range activeScopes(cfg.Strategies) {
		sr := measureScopeCycleRisk(scope, scopeRiskConfig(cfg, scope), cfg.Strategies, state, nil, nil, nil, true, false, now)
		prs := state.scopeRisk(scope)
		if peak, ok := peaks[scope]; ok {
			prs.PeakValue = peak
		}
		applyScopeCycleRisk(sr, prs)
		out[scope] = sr
	}
	return out
}

func TestCycleScopeRisk_LiveLatchLeavesPaperFree(t *testing.T) {
	cfg := scopeTestConfig(true, true)
	state := scopeTestState(cfg, 5000, 9900)
	res := runScopeCycle(t, cfg, state, map[PortfolioScope]float64{ScopeLive: 10000, ScopePaper: 10000})

	if !res[ScopeLive].KillSwitchFired {
		t.Fatalf("live 50%% drawdown must latch; reason=%q", res[ScopeLive].Reason)
	}
	if res[ScopePaper].KillSwitchFired {
		t.Fatalf("paper under the limit must stay free; reason=%q", res[ScopePaper].Reason)
	}
	if !state.scopeLatched(ScopeLive) || state.scopeLatched(ScopePaper) {
		t.Errorf("latched scopes = %v, want live only", state.latchedScopes())
	}
	due := dueStrategiesNotLatched(cfg.Strategies, res)
	if len(due) != 1 || due[0].ID != "paper-a" {
		t.Errorf("unlatched due set = %v, want the paper strategy only", due)
	}
}

func TestCycleScopeRisk_PaperLatchLeavesLiveFree(t *testing.T) {
	cfg := scopeTestConfig(true, true)
	state := scopeTestState(cfg, 9900, 5000)
	res := runScopeCycle(t, cfg, state, map[PortfolioScope]float64{ScopeLive: 10000, ScopePaper: 10000})

	if !res[ScopePaper].KillSwitchFired {
		t.Fatalf("paper 50%% drawdown must latch; reason=%q", res[ScopePaper].Reason)
	}
	if res[ScopeLive].KillSwitchFired {
		t.Fatalf("live under the limit must stay free; reason=%q", res[ScopeLive].Reason)
	}
	if state.scopeLatched(ScopeLive) {
		t.Error("a paper latch must never latch live")
	}
	if len(state.scopeRisk(ScopeLive).Events) != 0 {
		t.Errorf("live must record no event on a paper latch; got %v", state.scopeRisk(ScopeLive).Events)
	}
	due := dueStrategiesNotLatched(cfg.Strategies, res)
	if len(due) != 1 || due[0].ID != "live-a" {
		t.Errorf("unlatched due set = %v, want the live strategy only", due)
	}
}

func TestCycleScopeRisk_PaperEquityAlwaysTrusted(t *testing.T) {
	cfg := scopeTestConfig(true, true)
	state := scopeTestState(cfg, 5000, 5000)
	now := time.Now().UTC()
	for _, scope := range activeScopes(cfg.Strategies) {
		sr := measureScopeCycleRisk(scope, scopeRiskConfig(cfg, scope), cfg.Strategies, state, nil, nil, nil, true, true, now)
		prs := state.scopeRisk(scope)
		prs.PeakValue = 10000
		applyScopeCycleRisk(sr, prs)
	}
	paper := state.scopeRisk(ScopePaper)
	if paper.DrawdownReadingSubstituted {
		t.Error("paper equity is always trusted, so a reading can never be substituted")
	}
	if !paper.UntrustedOverLimitSince.IsZero() {
		t.Errorf("paper must never open a deferral window; got %v", paper.UntrustedOverLimitSince)
	}
	if !paper.KillSwitchActive {
		t.Error("paper over the limit must latch immediately, with no deferral")
	}
	live := state.scopeRisk(ScopeLive)
	if live.UntrustedOverLimitSince.IsZero() {
		t.Error("an untrusted live reading over the limit must open the deferral window")
	}
	if live.KillSwitchActive {
		t.Error("an untrusted live reading must defer the latch, not fire it immediately")
	}
}

func TestCycleScopeRisk_SingleModeMatchesLegacy(t *testing.T) {
	for _, live := range []bool{true, false} {
		cfg := scopeTestConfig(live, !live)
		state := scopeTestState(cfg, 9000, 9000)
		scope := ScopeLive
		if !live {
			scope = ScopePaper
		}
		res := runScopeCycle(t, cfg, state, map[PortfolioScope]float64{scope: 10000})

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
		got := state.scopeRisk(scope)
		if got.PeakValue != legacyPrs.PeakValue || got.CurrentDrawdownPct != legacyPrs.CurrentDrawdownPct ||
			got.CurrentMarginDrawdownPct != legacyPrs.CurrentMarginDrawdownPct || got.KillSwitchActive != legacyPrs.KillSwitchActive ||
			got.WarningSent != legacyPrs.WarningSent {
			t.Fatalf("single-mode prs diverged: %+v vs %+v", got, legacyPrs)
		}
	}
}

func TestEvaluateDailyLossLimit_Scope(t *testing.T) {
	cfg := &Config{
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60, DailyMaxLossUSD: 500},
		Strategies:    []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)},
	}
	today := time.Now().UTC().Format("2006-01-02")
	states := map[string]*StrategyState{
		"live-a":  {ID: "live-a", InitialCapital: 10000, RiskState: RiskState{DailyPnL: -600, DailyPnLDate: today}},
		"paper-a": {ID: "paper-a", InitialCapital: 10000, RiskState: RiskState{DailyPnL: 100, DailyPnLDate: today}},
	}
	liveSt := evaluateDailyLossLimit(cfg.PortfolioRisk, filterStatesByScope(states, cfg.Strategies, ScopeLive), strategiesInScope(cfg.Strategies, ScopeLive), time.Now().UTC())
	paperSt := evaluateDailyLossLimit(cfg.PortfolioRisk, filterStatesByScope(states, cfg.Strategies, ScopePaper), strategiesInScope(cfg.Strategies, ScopePaper), time.Now().UTC())
	if !liveSt.Tripped {
		t.Error("the live scope must trip on its own realized loss")
	}
	if paperSt.Tripped {
		t.Error("a live loss must never trip the paper scope")
	}
	if liveSt.CapitalBasis != 10000 || paperSt.CapitalBasis != 10000 {
		t.Errorf("pct basis must be per scope; live=%v paper=%v", liveSt.CapitalBasis, paperSt.CapitalBasis)
	}

	states["paper-a"].RiskState.DailyPnL = -900
	states["live-a"].RiskState.DailyPnL = 100
	liveSt = evaluateDailyLossLimit(cfg.PortfolioRisk, filterStatesByScope(states, cfg.Strategies, ScopeLive), strategiesInScope(cfg.Strategies, ScopeLive), time.Now().UTC())
	paperSt = evaluateDailyLossLimit(cfg.PortfolioRisk, filterStatesByScope(states, cfg.Strategies, ScopePaper), strategiesInScope(cfg.Strategies, ScopePaper), time.Now().UTC())
	if liveSt.Tripped {
		t.Error("a paper loss must never trip the live scope")
	}
	if !paperSt.Tripped {
		t.Error("the paper scope must trip on its own realized loss")
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

func TestComputeAssetDeltas_Scope(t *testing.T) {
	cfgs := []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}
	states := map[string]*StrategyState{
		"live-a":  scopeLongPosState("live-a", 0.2),
		"paper-a": scopeLongPosState("paper-a", 0.5),
	}
	prices := map[string]float64{"BTC": 50000}
	all, _ := computeAssetDeltas(states, cfgs, prices)
	liveAssets, _ := computeAssetDeltas(filterStatesByScope(states, cfgs, ScopeLive), strategiesInScope(cfgs, ScopeLive), prices)
	paperAssets, _ := computeAssetDeltas(filterStatesByScope(states, cfgs, ScopePaper), strategiesInScope(cfgs, ScopePaper), prices)

	if all["BTC"].NetDeltaUSD != 35000 {
		t.Fatalf("unscoped net delta = %v, want 35000", all["BTC"].NetDeltaUSD)
	}
	if liveAssets["BTC"].NetDeltaUSD != 10000 {
		t.Errorf("live net delta = %v, want 10000", liveAssets["BTC"].NetDeltaUSD)
	}
	if paperAssets["BTC"].NetDeltaUSD != 25000 {
		t.Errorf("paper net delta = %v, want 25000", paperAssets["BTC"].NetDeltaUSD)
	}
	if len(liveAssets["BTC"].Strategies) != 1 || len(paperAssets["BTC"].Strategies) != 1 {
		t.Error("each scope map must hold only its own strategies")
	}
}

func TestEvaluateExposureCap_Scope(t *testing.T) {
	cfgs := []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}
	states := map[string]*StrategyState{
		"live-a":  scopeLongPosState("live-a", 0.3),
		"paper-a": scopeLongPosState("paper-a", 0.3),
	}
	prices := map[string]float64{"BTC": 50000}
	pr := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60, MaxSameDirectionNotionalUSD: 20000}

	all := evaluateExposureCap(pr, states, cfgs, prices, 100000)
	if !all.LongBlocked {
		t.Fatal("the unscoped sum must exceed the cap, which is the bug under test")
	}
	live := evaluateExposureCap(pr, filterStatesByScope(states, cfgs, ScopeLive), strategiesInScope(cfgs, ScopeLive), prices, 50000)
	paper := evaluateExposureCap(pr, filterStatesByScope(states, cfgs, ScopePaper), strategiesInScope(cfgs, ScopePaper), prices, 50000)
	if live.LongBlocked || paper.LongBlocked {
		t.Errorf("neither scope alone reaches the cap; live=%v paper=%v", live.LongUSD, paper.LongUSD)
	}
	if live.LongUSD != 15000 || paper.LongUSD != 15000 {
		t.Errorf("per-scope long buckets = %v / %v, want 15000 each", live.LongUSD, paper.LongUSD)
	}
}

func TestPortfolioNotional_Scope(t *testing.T) {
	cfgs := []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}
	states := map[string]*StrategyState{
		"live-a":  scopeLongPosState("live-a", 0.2),
		"paper-a": scopeLongPosState("paper-a", 0.4),
	}
	prices := map[string]float64{"BTC": 50000}
	if got := PortfolioNotional(filterStatesByScope(states, cfgs, ScopeLive), prices); got != 10000 {
		t.Errorf("live notional = %v, want 10000", got)
	}
	if got := PortfolioNotional(filterStatesByScope(states, cfgs, ScopePaper), prices); got != 20000 {
		t.Errorf("paper notional = %v, want 20000", got)
	}
}

func TestAggregatePerpsMarginInputs_Scope(t *testing.T) {
	cfgs := []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}
	states := map[string]*StrategyState{
		"live-a":  scopeLongPosState("live-a", 0.2),
		"paper-a": scopeLongPosState("paper-a", 0.4),
	}
	prices := map[string]float64{"BTC": 45000}
	liveLoss, liveMargin := AggregatePerpsMarginInputs(filterStatesByScope(states, cfgs, ScopeLive), strategiesInScope(cfgs, ScopeLive), prices)
	paperLoss, paperMargin := AggregatePerpsMarginInputs(filterStatesByScope(states, cfgs, ScopePaper), strategiesInScope(cfgs, ScopePaper), prices)
	allLoss, allMargin := AggregatePerpsMarginInputs(states, cfgs, prices)
	if liveLoss <= 0 || paperLoss <= 0 {
		t.Fatalf("both scopes must report their own unrealized loss; live=%v paper=%v", liveLoss, paperLoss)
	}
	if liveLoss+paperLoss != allLoss || liveMargin+paperMargin != allMargin {
		t.Errorf("scope sums must partition the whole: %v+%v vs %v, %v+%v vs %v",
			liveLoss, paperLoss, allLoss, liveMargin, paperMargin, allMargin)
	}
	if paperLoss <= liveLoss {
		t.Errorf("the larger paper book must carry the larger loss; live=%v paper=%v", liveLoss, paperLoss)
	}
}

func TestComputeCorrelation_PerScope(t *testing.T) {
	cfgs := []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}
	states := map[string]*StrategyState{
		"live-a":  scopeLongPosState("live-a", 0.2),
		"paper-a": scopeLongPosState("paper-a", 0.4),
	}
	prices := map[string]float64{"BTC": 50000}
	corrCfg := &CorrelationConfig{Enabled: true, MaxConcentrationPct: 50, MaxSameDirectionPct: 50}

	state := NewAppState()
	for _, scope := range activeScopes(cfgs) {
		snap := ComputeCorrelation(filterStatesByScope(states, cfgs, scope), strategiesInScope(cfgs, scope), prices, corrCfg)
		state.setScopeCorrelation(scope, snap)
	}
	live := state.scopeCorrelation(ScopeLive)
	paper := state.scopeCorrelation(ScopePaper)
	if live == nil || paper == nil {
		t.Fatal("both scopes must hold their own snapshot")
	}
	if live.Scope != ScopeLive || paper.Scope != ScopePaper {
		t.Errorf("snapshots must carry their scope; live=%q paper=%q", live.Scope, paper.Scope)
	}
	if live.PortfolioGrossUSD != 10000 || paper.PortfolioGrossUSD != 20000 {
		t.Errorf("gross per scope = %v / %v, want 10000 / 20000", live.PortfolioGrossUSD, paper.PortfolioGrossUSD)
	}
	resp := formatCorrelationResponse(state.CorrelationSnapshot)
	if !strings.Contains(resp, "[live]") || !strings.Contains(resp, "[paper]") {
		t.Errorf("the correlation response must name both scopes:\n%s", resp)
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
	before := rebaselinePortfolioPeakAfterPruneForScope(state, cfg, ScopeLive, nil)
	delete(state.Strategies, "paper-a")
	after := rebaselinePortfolioPeakAfterPruneForScope(state, cfg, ScopeLive, nil)
	if before != after {
		t.Errorf("pruning a paper strategy must not move the live peak: %v -> %v", before, after)
	}
	paperAfter := rebaselinePortfolioPeakAfterPruneForScope(state, cfg, ScopePaper, nil)
	if paperAfter >= before+12000 {
		t.Errorf("the paper peak must fall back to its own configured capital, got %v", paperAfter)
	}
	if got := computeInitialPortfolioPeakForScope(cfg.Strategies, ScopeLive, nil); got != 10000 {
		t.Errorf("live initial peak = %v, want 10000 (its own capital only)", got)
	}
}

func TestPaperKillSwitch_ForceClosesPaperOnly(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}}
	state := NewAppState()
	state.Strategies["live-a"] = scopeLongPosState("live-a", 0.2)
	state.Strategies["paper-a"] = scopeLongPosState("paper-a", 0.4)

	closed := forceClosePaperScopePositions(state, cfg, map[string]float64{"BTC": 50000})
	if len(closed) != 1 || closed[0] != "paper-a" {
		t.Fatalf("closed = %v, want the paper strategy only", closed)
	}
	if len(state.Strategies["paper-a"].Positions) != 0 {
		t.Error("the paper book must be flat after a paper latch")
	}
	if len(state.Strategies["live-a"].Positions) != 1 {
		t.Error("a paper latch must never touch a live book")
	}
	msg := formatPaperKillSwitchMessage("paper drawdown 50.0% exceeds limit 25.0%", closed)
	for _, want := range []string{"PAPER", "paper-a", "No exchange order was sent", "reset paper"} {
		if !strings.Contains(msg, want) {
			t.Errorf("paper kill-switch message missing %q:\n%s", want, msg)
		}
	}
}

func TestLiveKillSwitch_LeavesPaperBooks(t *testing.T) {
	cfg := &Config{Strategies: []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}}
	state := NewAppState()
	state.Strategies["live-a"] = scopeLongPosState("live-a", 0.2)
	state.Strategies["paper-a"] = scopeLongPosState("paper-a", 0.4)
	state.scopeRisk(ScopeLive).KillSwitchActive = true

	for _, sc := range strategiesInScope(cfg.Strategies, ScopeLive) {
		if s, ok := state.Strategies[sc.ID]; ok {
			forceCloseKillSwitchPositions(s, sc, map[string]float64{"BTC": 50000}, nil, nil, hlVirtualQuantitySnapshot{}, nil)
		}
	}
	if len(state.Strategies["live-a"].Positions) != 0 {
		t.Error("the live roster must be flattened")
	}
	if len(state.Strategies["paper-a"].Positions) != 1 {
		t.Error("a live latch must never close a paper book")
	}
	if state.scopeLatched(ScopePaper) {
		t.Error("a live latch must never latch paper")
	}
	res := map[PortfolioScope]*scopeCycleRisk{
		ScopeLive:  {Scope: ScopeLive, KillSwitchFired: true},
		ScopePaper: {Scope: ScopePaper},
	}
	due := dueStrategiesNotLatched(cfg.Strategies, res)
	if len(due) != 1 || due[0].ID != "paper-a" {
		t.Errorf("the paper strategy must stay due while live is latched; got %v", due)
	}
}

func TestAutoResetConfirmedFlat_LiveScopeOnly(t *testing.T) {
	state := NewAppState()
	state.scopeRisk(ScopeLive).KillSwitchActive = true
	state.scopeRisk(ScopeLive).KillSwitchAt = time.Now().UTC()
	state.scopeRisk(ScopePaper).KillSwitchActive = true
	state.scopeRisk(ScopePaper).KillSwitchAt = time.Now().UTC()

	if !AutoResetConfirmedFlatKillSwitch(state.scopeRisk(ScopeLive), 10000, true, "confirmed flat") {
		t.Fatal("the live latch must auto-reset when confirmed flat with no owner")
	}
	if state.scopeLatched(ScopeLive) {
		t.Error("live must be cleared")
	}
	if !state.scopeLatched(ScopePaper) {
		t.Error("paper must never auto-reset without an owner")
	}
	if state.scopeRisk(ScopeLive).KillSwitchCloseApplied {
		t.Error("an auto reset must clear the one-shot close marker so a later latch closes again")
	}
}

func TestParseKillSwitchResetReply(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		latched []PortfolioScope
		want    PortfolioScope
		wantErr string
	}{
		{"bare reset with one latch", "reset", []PortfolioScope{ScopeLive}, ScopeLive, ""},
		{"bare reset with one paper latch", "reset", []PortfolioScope{ScopePaper}, ScopePaper, ""},
		{"bare reset with two latches refused", "reset", []PortfolioScope{ScopeLive, ScopePaper}, scopeUnassigned, "reply 'reset live' or 'reset paper'"},
		{"reset live with two latches", "reset live", []PortfolioScope{ScopeLive, ScopePaper}, ScopeLive, ""},
		{"reset paper with two latches", "reset paper", []PortfolioScope{ScopeLive, ScopePaper}, ScopePaper, ""},
		{"reset paper when only live latched", "reset paper", []PortfolioScope{ScopeLive}, scopeUnassigned, "paper scope is not latched"},
		{"nothing latched", "reset", nil, scopeUnassigned, "nothing to reset"},
		{"garbage reply", "yes please", []PortfolioScope{ScopeLive}, scopeUnassigned, "unexpected reply"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseKillSwitchResetReply(tc.reply, tc.latched)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("scope = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestManualResetOneScope_OtherStaysLatched(t *testing.T) {
	state := NewAppState()
	latchedAt := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	for _, scope := range []PortfolioScope{ScopeLive, ScopePaper} {
		prs := state.scopeRisk(scope)
		prs.KillSwitchActive = true
		prs.KillSwitchAt = latchedAt
		prs.CurrentDrawdownPct = 40
	}
	target, err := parseKillSwitchResetReply("reset paper", state.latchedScopes())
	if err != nil || target != ScopePaper {
		t.Fatalf("parse = %q, %v; want paper", target, err)
	}
	ResetPortfolioKillSwitchManual(state.scopeRisk(target))
	if state.scopeLatched(ScopePaper) {
		t.Error("the paper latch must clear")
	}
	live := state.scopeRisk(ScopeLive)
	if !live.KillSwitchActive || !live.KillSwitchAt.Equal(latchedAt) {
		t.Errorf("the live latch must survive a paper reset: %+v", live)
	}
}

func TestKillSwitchResetPrompt_NamesScope(t *testing.T) {
	plan := KillSwitchClosePlan{OnChainConfirmedFlat: true, DiscordMessage: "**PORTFOLIO KILL SWITCH**\nreason"}
	single := formatKillSwitchResetPrompt("live", "0xabc", plan, ScopeLive, []PortfolioScope{ScopeLive})
	if !strings.Contains(single, "[KILL SWITCH live]") || !strings.Contains(single, "Reply 'reset' to proceed.") {
		t.Errorf("single-latch prompt = %s", single)
	}
	both := formatKillSwitchResetPrompt("live", "0xabc", plan, ScopePaper, []PortfolioScope{ScopeLive, ScopePaper})
	if !strings.Contains(both, "[KILL SWITCH paper]") || !strings.Contains(both, "Reply 'reset paper' to proceed.") {
		t.Errorf("two-latch paper prompt = %s", both)
	}
	if strings.Contains(both, "Hyperliquid 0xabc") {
		t.Errorf("a paper prompt must not claim a live exchange identity: %s", both)
	}
	if !strings.Contains(both, "clears the paper scope only") {
		t.Errorf("a two-latch prompt must say it clears one scope only: %s", both)
	}
}

func TestManualStateView_ScopeLatch(t *testing.T) {
	cfg := &Config{
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60},
		Strategies:    []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)},
	}
	state := NewAppState()
	state.Strategies["live-a"] = scopeState("live-a", 10000)
	state.Strategies["paper-a"] = scopeState("paper-a", 10000)
	state.scopeRisk(ScopePaper).KillSwitchActive = true

	if v := manualStateViewFromState(cfg, state, "live-a", "BTC"); v.KillSwitch {
		t.Error("a paper latch must not block a live manual open")
	}
	if v := manualStateViewFromState(cfg, state, "paper-a", "BTC"); !v.KillSwitch {
		t.Error("a paper latch must block a paper manual open")
	}
	if v := manualStateViewFromState(cfg, state, "unknown-id", "BTC"); !v.KillSwitch {
		t.Error("an unknown strategy id must fail closed while any scope is latched")
	}

	state.scopeRisk(ScopePaper).KillSwitchActive = false
	state.scopeRisk(ScopeLive).KillSwitchActive = true
	if v := manualStateViewFromState(cfg, state, "paper-a", "BTC"); v.KillSwitch {
		t.Error("a live latch must not block a paper manual open")
	}
	if v := manualStateViewFromState(cfg, state, "live-a", "BTC"); !v.KillSwitch {
		t.Error("a live latch must block a live manual open")
	}
}

func TestManualStateView_ScopeHoldsDoNotCross(t *testing.T) {
	cfg := &Config{
		PortfolioRisk: &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 60, MaxNotionalUSD: 15000},
		Strategies:    []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)},
	}
	state := NewAppState()
	state.Strategies["live-a"] = scopeLongPosState("live-a", 0.1)
	state.Strategies["paper-a"] = scopeLongPosState("paper-a", 0.4)

	if v := manualStateViewFromState(cfg, state, "live-a", "BTC"); v.NotionalHold {
		t.Error("a paper book must not push the live scope over the notional cap")
	}
	if v := manualStateViewFromState(cfg, state, "paper-a", "BTC"); !v.NotionalHold {
		t.Error("the paper scope must hold on its own notional")
	}
}

func TestScopeRiskConfig_Merge(t *testing.T) {
	parent := &PortfolioRiskConfig{
		MaxDrawdownPct: 25, WarnThresholdPct: 60, MaxNotionalUSD: 100000,
		DailyMaxLossUSD: 500, DailyMaxLossPct: 4, MaxSameDirectionNotionalUSD: 50000, MaxAssetConcentrationPct: 40,
	}
	cfg := &Config{PortfolioRisk: parent}

	if got := scopeRiskConfig(cfg, ScopePaper); got != parent {
		t.Error("with no override the paper scope must reuse the parent config")
	}

	parent.Paper = &PortfolioRiskConfig{MaxDrawdownPct: 60, DailyMaxLossUSD: 2000}
	live := scopeRiskConfig(cfg, ScopeLive)
	paper := scopeRiskConfig(cfg, ScopePaper)
	if live.MaxDrawdownPct != 25 || live.DailyMaxLossUSD != 500 {
		t.Errorf("the live scope must never read the paper override: %+v", live)
	}
	if paper.MaxDrawdownPct != 60 || paper.DailyMaxLossUSD != 2000 {
		t.Errorf("non-zero override fields must win: %+v", paper)
	}
	if paper.WarnThresholdPct != 60 || paper.MaxNotionalUSD != 100000 ||
		paper.DailyMaxLossPct != 4 || paper.MaxSameDirectionNotionalUSD != 50000 || paper.MaxAssetConcentrationPct != 40 {
		t.Errorf("zero override fields must inherit from the parent: %+v", paper)
	}
	if paper.Paper != nil {
		t.Error("the merged paper config must not carry a nested override")
	}
}

func TestValidateConfig_PaperOverrideRanges(t *testing.T) {
	cfg := &Config{}
	cfg.PortfolioRisk = &PortfolioRiskConfig{
		MaxDrawdownPct: 25, WarnThresholdPct: 60,
		Paper: &PortfolioRiskConfig{MaxDrawdownPct: 150, DailyMaxLossUSD: -1, Paper: &PortfolioRiskConfig{}},
	}
	err := validateConfig(cfg, false)
	if err == nil {
		t.Fatal("an out-of-range paper override must be rejected")
	}
	for _, want := range []string{"portfolio_risk.paper.max_drawdown_pct", "portfolio_risk.paper.daily_max_loss_usd", "portfolio_risk.paper.paper is not allowed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error missing %q: %v", want, err)
		}
	}

	cfg.PortfolioRisk = &PortfolioRiskConfig{
		MaxDrawdownPct: 25, WarnThresholdPct: 60,
		Paper: &PortfolioRiskConfig{MaxDrawdownPct: 0, WarnThresholdPct: 0, DailyMaxLossUSD: 100},
	}
	if err := validateConfig(cfg, false); err != nil && strings.Contains(err.Error(), "portfolio_risk.paper") {
		t.Fatalf("zero override fields mean inherit and must validate: %v", err)
	}
}

func TestPaperKillSwitch_AutoResetsWithoutOwner(t *testing.T) {
	cases := []struct {
		name          string
		hasOwner      bool
		closeApplied  bool
		wantAutoReset bool
		wantLatched   bool
	}{
		{"no owner first cycle", false, false, true, false},
		{"no owner after restart with close already applied", false, true, true, false},
		{"owner configured stays latched", true, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Strategies: []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false)}}
			state := NewAppState()
			state.Strategies["live-a"] = scopeLongPosState("live-a", 0.2)
			state.Strategies["paper-a"] = scopeLongPosState("paper-a", 0.4)
			paperPrs := state.scopeRisk(ScopePaper)
			paperPrs.KillSwitchActive = true
			paperPrs.KillSwitchAt = time.Now()
			paperPrs.PeakValue = 20000
			paperPrs.KillSwitchCloseApplied = tc.closeApplied
			livePrs := state.scopeRisk(ScopeLive)
			livePrs.KillSwitchActive = true
			sr := &scopeCycleRisk{Scope: ScopePaper, KillSwitchFired: true, TotalPV: 15000, PeakRebaselineAvailable: true, Reason: "paper drawdown 50.0% exceeds limit 25.0%"}

			out := applyPaperKillSwitchCycle(state, cfg, map[string]float64{"BTC": 50000}, sr, tc.hasOwner)
			if out.AutoReset != tc.wantAutoReset {
				t.Fatalf("AutoReset = %v, want %v", out.AutoReset, tc.wantAutoReset)
			}
			if paperPrs.KillSwitchActive != tc.wantLatched {
				t.Fatalf("paper KillSwitchActive = %v, want %v", paperPrs.KillSwitchActive, tc.wantLatched)
			}
			if !livePrs.KillSwitchActive {
				t.Error("a paper auto-reset must never clear the live latch")
			}
			if len(state.Strategies["live-a"].Positions) != 1 {
				t.Error("a paper latch must never touch a live book")
			}
			if out.Message == "" {
				t.Fatal("the paper kill-switch message must carry the reason on every outcome")
			}
			if !strings.Contains(out.Message, sr.Reason) {
				t.Errorf("message must carry the drawdown reason:\n%s", out.Message)
			}
			if tc.wantAutoReset {
				if paperPrs.KillSwitchCloseApplied {
					t.Error("auto-reset must clear KillSwitchCloseApplied so the next latch force-closes again")
				}
				if paperPrs.PeakValue != 15000 {
					t.Errorf("auto-reset must re-baseline the paper peak; got %v", paperPrs.PeakValue)
				}
				if !strings.Contains(out.Message, paperKillSwitchAutoResetLine) || strings.Contains(out.Message, paperKillSwitchManualResetLine) {
					t.Errorf("auto-reset message must say the latch cleared itself:\n%s", out.Message)
				}
			} else if !strings.Contains(out.Message, paperKillSwitchManualResetLine) {
				t.Errorf("an owner-gated latch must ask for the DM reset:\n%s", out.Message)
			}
			if !tc.closeApplied && len(state.Strategies["paper-a"].Positions) != 0 {
				t.Error("the paper book must be flat after the latch")
			}
		})
	}
}

func TestPaperKillSwitchPromptMessage_CarriesReason(t *testing.T) {
	reason := "portfolio kill switch latched at 2026-09-03T05:00:00Z (paper drawdown 50.0%)"
	msg := formatPaperKillSwitchPromptMessage(reason)
	if !strings.Contains(msg, reason) || !strings.Contains(msg, "PAPER") {
		t.Fatalf("prompt message must name the scope and reason:\n%s", msg)
	}
	prompt := formatKillSwitchResetPrompt("paper-testing", "0xabc", KillSwitchClosePlan{OnChainConfirmedFlat: true, DiscordMessage: msg}, ScopePaper, []PortfolioScope{ScopePaper})
	if !strings.Contains(prompt, reason) {
		t.Errorf("a re-issued paper reset prompt must carry the drawdown reason:\n%s", prompt)
	}
}

func TestScopeHasPersistedState(t *testing.T) {
	cfgs := []StrategyConfig{scopeCfg("live-a", true), scopeCfg("paper-a", false), scopeCfg("paper-new", false)}
	persisted := map[string]bool{"live-a": true, "paper-a": true}
	if !scopeHasPersistedState(cfgs, ScopePaper, persisted) {
		t.Error("a paper scope with one persisted member carries state")
	}
	if scopeHasPersistedState(cfgs, ScopePaper, map[string]bool{"live-a": true}) {
		t.Error("a paper scope whose members are all new carries no state")
	}
}

func TestCycleScopeRisk_NewScopeSeedsPeakFromCurrentValue(t *testing.T) {
	cfg := scopeTestConfig(true, true)
	state := scopeTestState(cfg, 10000, 7000)
	state.scopeRisk(ScopeLive).PeakValue = 10000

	res := runScopeCycle(t, cfg, state, nil)
	paperPrs := state.scopeRisk(ScopePaper)
	if paperPrs.PeakValue != res[ScopePaper].TotalPV || paperPrs.PeakValue != 7000 {
		t.Fatalf("a new paper scope must seed its peak from the current paper value; peak=%v total=%v", paperPrs.PeakValue, res[ScopePaper].TotalPV)
	}
	if res[ScopePaper].KillSwitchFired || paperPrs.CurrentDrawdownPct != 0 {
		t.Fatalf("a paper book already below configured capital must not latch on its first cycle; reason=%q dd=%v", res[ScopePaper].Reason, paperPrs.CurrentDrawdownPct)
	}

	state.Strategies["paper-a"].Cash = 5000
	res = runScopeCycle(t, cfg, state, nil)
	if !res[ScopePaper].KillSwitchFired {
		t.Errorf("a later paper loss past the limit must latch against the seeded peak; reason=%q", res[ScopePaper].Reason)
	}

	above := scopeTestState(cfg, 10000, 13000)
	above.scopeRisk(ScopeLive).PeakValue = 10000
	runScopeCycle(t, cfg, above, nil)
	if above.scopeRisk(ScopePaper).PeakValue != 13000 {
		t.Errorf("a paper book above configured capital must seed at its current value, not lower; got %v", above.scopeRisk(ScopePaper).PeakValue)
	}
}

func TestKillSwitchResetPromptForScopes(t *testing.T) {
	livePlan := KillSwitchClosePlan{OnChainConfirmedFlat: false, DiscordMessage: "**PORTFOLIO KILL SWITCH**\nlive drawdown 30%"}
	paperPlan := KillSwitchClosePlan{OnChainConfirmedFlat: true, DiscordMessage: formatPaperKillSwitchPromptMessage("paper drawdown 40%")}
	plans := map[PortfolioScope]KillSwitchClosePlan{ScopeLive: livePlan, ScopePaper: paperPlan}
	both := formatKillSwitchResetPromptForScopes("inst", "0xabc", plans, []PortfolioScope{ScopeLive, ScopePaper}, []PortfolioScope{ScopeLive, ScopePaper})
	for _, want := range []string{"[KILL SWITCH live]", "[KILL SWITCH paper]", "live drawdown 30%", "paper drawdown 40%", "Hyperliquid 0xabc", "Reply 'reset live' or 'reset paper' to proceed.", "resting stop-losses may already be cancelled"} {
		if !strings.Contains(both, want) {
			t.Errorf("two-scope prompt missing %q:\n%s", want, both)
		}
	}
	if strings.Count(both, "Hyperliquid 0xabc") != 1 {
		t.Errorf("only the live section may carry the exchange identity:\n%s", both)
	}
	single := formatKillSwitchResetPromptForScopes("inst", "0xabc", plans, []PortfolioScope{ScopePaper}, []PortfolioScope{ScopePaper})
	if single != formatKillSwitchResetPrompt("inst", "0xabc", paperPlan, ScopePaper, []PortfolioScope{ScopePaper}) {
		t.Error("a single latched scope must produce the single-scope prompt")
	}
	if target, err := parseKillSwitchResetReply("reset paper", []PortfolioScope{ScopeLive, ScopePaper}); err != nil || target != ScopePaper {
		t.Errorf("the one reply must resolve the named scope: %q %v", target, err)
	}
	if target, err := parseKillSwitchResetReply("reset", []PortfolioScope{ScopeLive}); err != nil || target != ScopeLive {
		t.Errorf("a bare reset must resolve the surviving scope: %q %v", target, err)
	}
}
