package main

import (
	"testing"
	"time"
)

func dlState(id string, initialCapital, dailyPnL float64, date string) *StrategyState {
	return &StrategyState{
		ID:             id,
		InitialCapital: initialCapital,
		RiskState:      RiskState{DailyPnL: dailyPnL, DailyPnLDate: date},
	}
}

func dlToday() string { return time.Now().UTC().Format("2006-01-02") }

func dlBool(v bool) *bool { return &v }

func TestEvaluateDailyLossLimit(t *testing.T) {
	now := time.Now().UTC()
	today := dlToday()
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	cases := []struct {
		name         string
		pr           *PortfolioRiskConfig
		states       map[string]*StrategyState
		strategies   []StrategyConfig
		tripped      bool
		configured   *bool
		lossUSD      *float64
		thresholdUSD *float64
		capitalBasis *float64
		pctBasisMiss *bool
	}{
		{name: "unconfigured/max_drawdown_only_never_trips",
			pr:         &PortfolioRiskConfig{MaxDrawdownPct: 25},
			states:     map[string]*StrategyState{"a": dlState("a", 1000, -900, today)},
			configured: dlBool(false), lossUSD: fp(900)},
		{name: "unconfigured/nil_portfolio_risk_never_trips",
			pr:         nil,
			states:     map[string]*StrategyState{"a": dlState("a", 1000, -900, today)},
			configured: dlBool(false)},
		{name: "usd/below_threshold_not_tripped",
			pr:     &PortfolioRiskConfig{DailyMaxLossUSD: 500},
			states: map[string]*StrategyState{"a": dlState("a", 0, -499.99, today)}},
		{name: "usd/at_threshold_trips",
			pr:      &PortfolioRiskConfig{DailyMaxLossUSD: 500},
			states:  map[string]*StrategyState{"a": dlState("a", 0, -500, today)},
			tripped: true},
		{name: "usd/multi_strategy_aggregate",
			pr: &PortfolioRiskConfig{DailyMaxLossUSD: 500},
			states: map[string]*StrategyState{
				"a": dlState("a", 0, -300, today),
				"b": dlState("b", 0, -250, today),
			},
			tripped: true, lossUSD: fp(550), thresholdUSD: fp(500)},
		{name: "pct/basis_sums_initial_capital",
			pr: &PortfolioRiskConfig{DailyMaxLossPct: 5},
			states: map[string]*StrategyState{
				"a": dlState("a", 2000, -100, today),
				"b": dlState("b", 3000, -160, today),
			},
			tripped: true, capitalBasis: fp(5000), thresholdUSD: fp(250)},
		{name: "pct/under_threshold_not_tripped",
			pr: &PortfolioRiskConfig{DailyMaxLossPct: 5},
			states: map[string]*StrategyState{
				"a": dlState("a", 2000, -100, today),
				"b": dlState("b", 3000, -140, today),
			}},
		{name: "pct_basis/excludes_shared_wallet_pool",
			pr: &PortfolioRiskConfig{DailyMaxLossPct: 5},
			states: map[string]*StrategyState{
				"pool-a":    dlState("pool-a", 1000, -100, today),
				"allocated": dlState("allocated", 2000, -20, today),
			},
			strategies: []StrategyConfig{{ID: "pool-a", sharedWalletPoolBudget: true}, {ID: "allocated"}},
			tripped:    true, capitalBasis: fp(2000), thresholdUSD: fp(100)},
		{name: "pct_basis/all_pool_surfaces_basis_miss",
			pr:           &PortfolioRiskConfig{DailyMaxLossPct: 5},
			states:       map[string]*StrategyState{"pool-a": dlState("pool-a", 1000, -100, today)},
			strategies:   []StrategyConfig{{ID: "pool-a", sharedWalletPoolBudget: true}},
			capitalBasis: fp(0), pctBasisMiss: dlBool(true)},
		{name: "both_arms/usd_lower_wins",
			pr:      &PortfolioRiskConfig{DailyMaxLossUSD: 400, DailyMaxLossPct: 5},
			states:  map[string]*StrategyState{"a": dlState("a", 20000, -450, today)},
			tripped: true, thresholdUSD: fp(400)},
		{name: "both_arms/pct_lower_wins",
			pr:           &PortfolioRiskConfig{DailyMaxLossUSD: 2000, DailyMaxLossPct: 5},
			states:       map[string]*StrategyState{"a": dlState("a", 20000, -450, today)},
			thresholdUSD: fp(1000)},
		{name: "stale_day_counts_zero",
			pr: &PortfolioRiskConfig{DailyMaxLossUSD: 100},
			states: map[string]*StrategyState{
				"stale": dlState("stale", 0, -5000, yesterday),
				"fresh": dlState("fresh", 0, -50, today),
			},
			lossUSD: fp(50)},
		{name: "wins_offset_losses",
			pr: &PortfolioRiskConfig{DailyMaxLossUSD: 100},
			states: map[string]*StrategyState{
				"win":  dlState("win", 0, 400, today),
				"loss": dlState("loss", 0, -450, today),
			},
			lossUSD: fp(50)},
		{name: "net_positive_day_loss_zero",
			pr: &PortfolioRiskConfig{DailyMaxLossUSD: 100},
			states: map[string]*StrategyState{
				"win":  dlState("win", 0, 400, today),
				"loss": dlState("loss", 0, -100, today),
			},
			lossUSD: fp(0)},
		{name: "manual_strategy_pnl_counts",
			pr: &PortfolioRiskConfig{DailyMaxLossUSD: 300},
			states: map[string]*StrategyState{
				"hl-perps": {ID: "hl-perps", Type: "perps", RiskState: RiskState{DailyPnL: -200, DailyPnLDate: today}},
				"manual":   {ID: "manual", Type: "manual", RiskState: RiskState{DailyPnL: -150, DailyPnLDate: today}},
			},
			tripped: true, lossUSD: fp(350)},
		{name: "pct_basis_miss/pct_only_arm_inert",
			pr:           &PortfolioRiskConfig{DailyMaxLossPct: 5},
			states:       map[string]*StrategyState{"a": dlState("a", 0, -10000, today)},
			pctBasisMiss: dlBool(true)},
		{name: "pct_basis_miss/usd_arm_still_enforces",
			pr:      &PortfolioRiskConfig{DailyMaxLossUSD: 500, DailyMaxLossPct: 5},
			states:  map[string]*StrategyState{"a": dlState("a", 0, -10000, today)},
			tripped: true, thresholdUSD: fp(500)},
		{name: "nil_state_entry_skipped",
			pr: &PortfolioRiskConfig{DailyMaxLossUSD: 100},
			states: map[string]*StrategyState{
				"nil": nil,
				"a":   dlState("a", 0, -150, today),
			},
			tripped: true, lossUSD: fp(150)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := evaluateDailyLossLimit(tc.pr, tc.states, tc.strategies, now)
			if st.Tripped != tc.tripped {
				t.Fatalf("Tripped = %v, want %v: %+v", st.Tripped, tc.tripped, st)
			}
			if tc.configured != nil && st.Configured != *tc.configured {
				t.Fatalf("Configured = %v, want %v: %+v", st.Configured, *tc.configured, st)
			}
			if tc.lossUSD != nil && st.LossUSD != *tc.lossUSD {
				t.Fatalf("LossUSD = %g, want %g: %+v", st.LossUSD, *tc.lossUSD, st)
			}
			if tc.thresholdUSD != nil && st.ThresholdUSD != *tc.thresholdUSD {
				t.Fatalf("ThresholdUSD = %g, want %g: %+v", st.ThresholdUSD, *tc.thresholdUSD, st)
			}
			if tc.capitalBasis != nil && st.CapitalBasis != *tc.capitalBasis {
				t.Fatalf("CapitalBasis = %g, want %g: %+v", st.CapitalBasis, *tc.capitalBasis, st)
			}
			if tc.pctBasisMiss != nil && st.PctBasisMiss != *tc.pctBasisMiss {
				t.Fatalf("PctBasisMiss = %v, want %v: %+v", st.PctBasisMiss, *tc.pctBasisMiss, st)
			}
		})
	}
}
