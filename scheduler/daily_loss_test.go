package main

import (
	"strings"
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

func TestDailyLossAlertDue(t *testing.T) {
	if !dailyLossAlertDue(true, "", "2026-07-09") {
		t.Fatal("first trip of the day must DM")
	}
	if dailyLossAlertDue(true, "2026-07-09", "2026-07-09") {
		t.Fatal("second cycle same day must not re-DM")
	}
	if !dailyLossAlertDue(true, "2026-07-09", "2026-07-10") {
		t.Fatal("a new trip day must DM again")
	}
	if dailyLossAlertDue(false, "", "2026-07-09") {
		t.Fatal("untripped must never DM")
	}
}

func TestDailyLossStatusNote(t *testing.T) {
	now := time.Now().UTC()
	states := map[string]*StrategyState{"a": dlState("a", 1000, -600, dlToday())}

	if note := dailyLossStatusNote(dlNoteCfg(nil), states, now); note != "" {
		t.Fatalf("unconfigured note = %q, want empty", note)
	}
	if note := dailyLossStatusNote(dlNoteCfg(&PortfolioRiskConfig{MaxDrawdownPct: 25}), states, now); note != "" {
		t.Fatalf("unconfigured note = %q, want empty", note)
	}
	tripped := dailyLossStatusNote(dlNoteCfg(&PortfolioRiskConfig{DailyMaxLossUSD: 500}), states, now)
	if !strings.Contains(tripped, "TRIPPED") || !strings.Contains(tripped, "$600.00") {
		t.Fatalf("tripped note = %q", tripped)
	}
	armed := dailyLossStatusNote(dlNoteCfg(&PortfolioRiskConfig{DailyMaxLossUSD: 5000}), states, now)
	if !strings.Contains(armed, "armed") || !strings.Contains(armed, "$5000.00") {
		t.Fatalf("armed note = %q", armed)
	}
	miss := dailyLossStatusNote(dlNoteCfg(&PortfolioRiskConfig{DailyMaxLossPct: 5}), map[string]*StrategyState{
		"a": dlState("a", 0, -600, dlToday()),
	}, now)
	if !strings.Contains(miss, "initial_capital") || !strings.Contains(miss, "CANNOT evaluate") {
		t.Fatalf("basis-miss note = %q", miss)
	}
	both := dailyLossStatusNote(dlNoteCfg(&PortfolioRiskConfig{DailyMaxLossUSD: 5000, DailyMaxLossPct: 5}), map[string]*StrategyState{
		"a": dlState("a", 0, -600, dlToday()),
	}, now)
	if !strings.Contains(both, "armed") || !strings.Contains(both, "CANNOT evaluate") {
		t.Fatalf("both-arms basis-miss note = %q, want armed + pct warning", both)
	}
	trippedMiss := dailyLossStatusNote(dlNoteCfg(&PortfolioRiskConfig{DailyMaxLossUSD: 500, DailyMaxLossPct: 5}), map[string]*StrategyState{
		"a": dlState("a", 0, -600, dlToday()),
	}, now)
	if !strings.Contains(trippedMiss, "TRIPPED") || !strings.Contains(trippedMiss, "CANNOT evaluate") {
		t.Fatalf("tripped basis-miss note = %q, want TRIPPED + pct warning", trippedMiss)
	}
}

func TestFormatDailyLossPctBasisMissDM(t *testing.T) {
	now := time.Now().UTC()
	st := DailyLossLimitStatus{Configured: true, PctBasisMiss: true, DailyPnL: -600}
	dm := formatDailyLossPctBasisMissDM(st, now)
	if !strings.Contains(dm, "CANNOT evaluate") || !strings.Contains(dm, "fully inert") {
		t.Fatalf("pct-only DM = %q", dm)
	}
	st.ThresholdUSD = 500
	dm = formatDailyLossPctBasisMissDM(st, now)
	if !strings.Contains(dm, "USD arm still enforces at $500.00") {
		t.Fatalf("both-arms DM = %q", dm)
	}
}

func TestDailyLossStartupSummaryLine(t *testing.T) {
	if line := dailyLossStartupSummaryLine(nil); line != "" {
		t.Fatalf("nil config line = %q, want empty", line)
	}
	if line := dailyLossStartupSummaryLine(&PortfolioRiskConfig{MaxDrawdownPct: 25}); line != "" {
		t.Fatalf("unconfigured line = %q, want empty", line)
	}
	line := dailyLossStartupSummaryLine(&PortfolioRiskConfig{DailyMaxLossUSD: 500, DailyMaxLossPct: 5})
	for _, want := range []string{"daily_max_loss", "usd=$500.00", "pct=5.00%"} {
		if !strings.Contains(line, want) {
			t.Fatalf("summary line %q missing %q", line, want)
		}
	}
}

func TestConfigValidationDailyLossThresholds(t *testing.T) {
	cfg := Config{PortfolioRisk: &PortfolioRiskConfig{
		MaxDrawdownPct:   25,
		WarnThresholdPct: 60,
		DailyMaxLossUSD:  -5,
		DailyMaxLossPct:  150,
	}}
	err := validateConfig(&cfg, false)
	if err == nil {
		t.Fatal("expected negative/out-of-range daily loss thresholds to be rejected")
	}
	msg := err.Error()
	for _, want := range []string{"daily_max_loss_usd must be >= 0", "daily_max_loss_pct must be in [0, 100]"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("validation error %q missing %q", msg, want)
		}
	}
	cfg.PortfolioRisk.DailyMaxLossUSD = 0
	cfg.PortfolioRisk.DailyMaxLossPct = 0
	if err := validateConfig(&cfg, false); err != nil && strings.Contains(err.Error(), "daily_max_loss") {
		t.Fatalf("disabled thresholds must not error: %v", err)
	}
}

func TestManualStateViewDailyLossHold(t *testing.T) {
	cfg := &Config{PortfolioRisk: &PortfolioRiskConfig{DailyMaxLossUSD: 500}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"m": {ID: "m", Type: "manual", Positions: map[string]*Position{},
			RiskState: RiskState{DailyPnL: -600, DailyPnLDate: dlToday()}},
	}}
	v := manualStateViewFromState(cfg, state, "m", "ETH")
	if !v.DailyLossHold || v.DailyLossNote == "" {
		t.Fatalf("view = %+v, want DailyLossHold with note", v)
	}
	state.Strategies["m"].RiskState.DailyPnL = -100
	v = manualStateViewFromState(cfg, state, "m", "ETH")
	if v.DailyLossHold {
		t.Fatalf("view = %+v, want no hold under threshold", v)
	}
	v = manualStateViewFromState(nil, state, "m", "ETH")
	if v.DailyLossHold {
		t.Fatalf("nil cfg view = %+v, want no hold", v)
	}
}

func TestManualCoreRefusesDailyLossHold(t *testing.T) {
	sc := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 3}
	const note = "daily loss limit tripped: today's realized loss $600.00 >= threshold $500.00 (pre-fee; basis=$0.00 initial capital)"
	cases := []struct {
		name string
		pos  *Position
		run  func(deps manualCoreDeps) error
	}{
		{"manual-open", nil, func(deps manualCoreDeps) error {
			_, err := manualOpenCore(deps, sc, manualOpenInputs{StrategyID: "m", Margin: 50})
			return err
		}},
		{"manual-add", &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}, func(deps manualCoreDeps) error {
			_, err := manualAddCore(deps, sc, manualAddInputs{StrategyID: "m", Margin: 50})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := manualCoreDeps{
				cfg: &Config{},
				loadState: func(strategyID, symbol string) (manualStateView, error) {
					return manualStateView{HasStrategy: true, Pos: tc.pos, DailyLossHold: true, DailyLossNote: note}, nil
				},
				execute: func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
					t.Error("execute must not be called while the daily loss limit is tripped")
					return nil, "", nil
				},
				fetchMids: func([]string) (map[string]float64, error) {
					return map[string]float64{"ETH": 2000}, nil
				},
			}
			err := tc.run(deps)
			if err == nil || !strings.Contains(err.Error(), "daily loss limit tripped") {
				t.Fatalf("%s err = %v, want daily-loss refusal", tc.name, err)
			}
		})
	}
}

func dlNoteCfg(pr *PortfolioRiskConfig) *Config {
	return &Config{
		PortfolioRisk: pr,
		Strategies:    []StrategyConfig{{ID: "a", Args: []string{"strat", "BTC/USDT", "--mode=live"}}},
	}
}
