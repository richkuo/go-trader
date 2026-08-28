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

func TestEvaluateDailyLossLimitUnconfigured(t *testing.T) {
	states := map[string]*StrategyState{
		"a": dlState("a", 1000, -900, dlToday()),
	}
	st := evaluateDailyLossLimit(&PortfolioRiskConfig{MaxDrawdownPct: 25}, states, nil, time.Now().UTC())
	if st.Configured || st.Tripped {
		t.Fatalf("unconfigured limit must never trip: %+v", st)
	}
	if st.LossUSD != 900 {
		t.Fatalf("LossUSD = %g, want 900", st.LossUSD)
	}
	st = evaluateDailyLossLimit(nil, states, nil, time.Now().UTC())
	if st.Configured || st.Tripped {
		t.Fatalf("nil portfolio risk must never trip: %+v", st)
	}
}

func TestEvaluateDailyLossLimitUSDThreshold(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 500}
	now := time.Now().UTC()

	below := map[string]*StrategyState{"a": dlState("a", 0, -499.99, dlToday())}
	if st := evaluateDailyLossLimit(pr, below, nil, now); st.Tripped {
		t.Fatalf("loss below threshold must not trip: %+v", st)
	}
	atLimit := map[string]*StrategyState{"a": dlState("a", 0, -500, dlToday())}
	if st := evaluateDailyLossLimit(pr, atLimit, nil, now); !st.Tripped {
		t.Fatalf("loss at threshold must trip: %+v", st)
	}
	beyond := map[string]*StrategyState{
		"a": dlState("a", 0, -300, dlToday()),
		"b": dlState("b", 0, -250, dlToday()),
	}
	st := evaluateDailyLossLimit(pr, beyond, nil, now)
	if !st.Tripped || st.LossUSD != 550 || st.ThresholdUSD != 500 {
		t.Fatalf("multi-strategy aggregate: %+v, want tripped loss=550 threshold=500", st)
	}
}

func TestEvaluateDailyLossLimitPctThreshold(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossPct: 5}
	now := time.Now().UTC()
	states := map[string]*StrategyState{
		"a": dlState("a", 2000, -100, dlToday()),
		"b": dlState("b", 3000, -160, dlToday()),
	}
	st := evaluateDailyLossLimit(pr, states, nil, now)
	if !st.Tripped || st.CapitalBasis != 5000 || st.ThresholdUSD != 250 {
		t.Fatalf("pct arm: %+v, want tripped basis=5000 threshold=250", st)
	}
	states["b"].RiskState.DailyPnL = -140
	if st := evaluateDailyLossLimit(pr, states, nil, now); st.Tripped {
		t.Fatalf("loss under pct threshold must not trip: %+v", st)
	}
}

func TestEvaluateDailyLossLimitPctBasisExcludesSharedWalletPool(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossPct: 5}
	now := time.Now().UTC()
	states := map[string]*StrategyState{
		"pool-a":    dlState("pool-a", 1000, -100, dlToday()),
		"allocated": dlState("allocated", 2000, -20, dlToday()),
	}
	strategies := []StrategyConfig{
		{ID: "pool-a", sharedWalletPoolBudget: true},
		{ID: "allocated"},
	}
	st := evaluateDailyLossLimit(pr, states, strategies, now)
	if st.CapitalBasis != 2000 || st.ThresholdUSD != 100 {
		t.Fatalf("mixed pool basis=%v threshold=%v, want allocated-only 2000/100", st.CapitalBasis, st.ThresholdUSD)
	}
	if !st.Tripped {
		t.Fatalf("loss $120 must trip allocated-only $100 threshold: %+v", st)
	}

	delete(states, "allocated")
	strategies = strategies[:1]
	st = evaluateDailyLossLimit(pr, states, strategies, now)
	if st.CapitalBasis != 0 || !st.PctBasisMiss || st.Tripped {
		t.Fatalf("all-pool pct arm must surface a basis miss: %+v", st)
	}
}

func TestEvaluateDailyLossLimitBothArmsLowerWins(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 400, DailyMaxLossPct: 5}
	states := map[string]*StrategyState{"a": dlState("a", 20000, -450, dlToday())}
	st := evaluateDailyLossLimit(pr, states, nil, time.Now().UTC())
	if !st.Tripped || st.ThresholdUSD != 400 {
		t.Fatalf("lower arm must win: %+v, want tripped threshold=400", st)
	}
	pr = &PortfolioRiskConfig{DailyMaxLossUSD: 2000, DailyMaxLossPct: 5}
	st = evaluateDailyLossLimit(pr, states, nil, time.Now().UTC())
	if st.Tripped || st.ThresholdUSD != 1000 {
		t.Fatalf("pct arm lower: %+v, want not tripped threshold=1000", st)
	}
}

func TestEvaluateDailyLossLimitStaleDayExcluded(t *testing.T) {
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 100}
	states := map[string]*StrategyState{
		"stale": dlState("stale", 0, -5000, yesterday),
		"fresh": dlState("fresh", 0, -50, dlToday()),
	}
	st := evaluateDailyLossLimit(pr, states, nil, time.Now().UTC())
	if st.Tripped || st.LossUSD != 50 {
		t.Fatalf("stale day must count 0: %+v, want loss=50 not tripped", st)
	}
}

func TestEvaluateDailyLossLimitWinsOffsetLosses(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 100}
	states := map[string]*StrategyState{
		"win":  dlState("win", 0, 400, dlToday()),
		"loss": dlState("loss", 0, -450, dlToday()),
	}
	st := evaluateDailyLossLimit(pr, states, nil, time.Now().UTC())
	if st.Tripped || st.LossUSD != 50 {
		t.Fatalf("net aggregate: %+v, want loss=50 not tripped", st)
	}
	states["loss"].RiskState.DailyPnL = -100
	st = evaluateDailyLossLimit(pr, states, nil, time.Now().UTC())
	if st.Tripped || st.LossUSD != 0 {
		t.Fatalf("net-positive day: %+v, want loss=0 not tripped", st)
	}
}

func TestEvaluateDailyLossLimitManualStrategyIncluded(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 300}
	states := map[string]*StrategyState{
		"hl-perps": {ID: "hl-perps", Type: "perps", RiskState: RiskState{DailyPnL: -200, DailyPnLDate: dlToday()}},
		"manual":   {ID: "manual", Type: "manual", RiskState: RiskState{DailyPnL: -150, DailyPnLDate: dlToday()}},
	}
	st := evaluateDailyLossLimit(pr, states, nil, time.Now().UTC())
	if !st.Tripped || st.LossUSD != 350 {
		t.Fatalf("manual PnL must count: %+v, want tripped loss=350", st)
	}
}

func TestEvaluateDailyLossLimitPctBasisMiss(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossPct: 5}
	states := map[string]*StrategyState{"a": dlState("a", 0, -10000, dlToday())}
	st := evaluateDailyLossLimit(pr, states, nil, time.Now().UTC())
	if st.Tripped || !st.PctBasisMiss {
		t.Fatalf("basis-less pct arm: %+v, want not tripped with PctBasisMiss", st)
	}
	pr.DailyMaxLossUSD = 500
	st = evaluateDailyLossLimit(pr, states, nil, time.Now().UTC())
	if !st.Tripped || st.ThresholdUSD != 500 {
		t.Fatalf("usd arm with basis miss: %+v, want tripped threshold=500", st)
	}
}

func TestEvaluateDailyLossLimitNilStateSkipped(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 100}
	states := map[string]*StrategyState{
		"nil": nil,
		"a":   dlState("a", 0, -150, dlToday()),
	}
	st := evaluateDailyLossLimit(pr, states, nil, time.Now().UTC())
	if !st.Tripped || st.LossUSD != 150 {
		t.Fatalf("nil entries must be skipped: %+v", st)
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

	if note := dailyLossStatusNote(nil, states, nil, now); note != "" {
		t.Fatalf("unconfigured note = %q, want empty", note)
	}
	if note := dailyLossStatusNote(&PortfolioRiskConfig{MaxDrawdownPct: 25}, states, nil, now); note != "" {
		t.Fatalf("unconfigured note = %q, want empty", note)
	}
	tripped := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossUSD: 500}, states, nil, now)
	if !strings.Contains(tripped, "TRIPPED") || !strings.Contains(tripped, "$600.00") {
		t.Fatalf("tripped note = %q", tripped)
	}
	armed := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossUSD: 5000}, states, nil, now)
	if !strings.Contains(armed, "armed") || !strings.Contains(armed, "$5000.00") {
		t.Fatalf("armed note = %q", armed)
	}
	miss := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossPct: 5}, map[string]*StrategyState{
		"a": dlState("a", 0, -600, dlToday()),
	}, nil, now)
	if !strings.Contains(miss, "initial_capital") || !strings.Contains(miss, "CANNOT evaluate") {
		t.Fatalf("basis-miss note = %q", miss)
	}
	both := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossUSD: 5000, DailyMaxLossPct: 5}, map[string]*StrategyState{
		"a": dlState("a", 0, -600, dlToday()),
	}, nil, now)
	if !strings.Contains(both, "armed") || !strings.Contains(both, "CANNOT evaluate") {
		t.Fatalf("both-arms basis-miss note = %q, want armed + pct warning", both)
	}
	trippedMiss := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossUSD: 500, DailyMaxLossPct: 5}, map[string]*StrategyState{
		"a": dlState("a", 0, -600, dlToday()),
	}, nil, now)
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

func TestManualOpenCoreRefusesDailyLossHold(t *testing.T) {
	sc := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 3}
	deps := manualCoreDeps{
		cfg: &Config{},
		loadState: func(strategyID, symbol string) (manualStateView, error) {
			return manualStateView{HasStrategy: true, DailyLossHold: true,
				DailyLossNote: "daily loss limit tripped: today's realized loss $600.00 >= threshold $500.00 (pre-fee; basis=$0.00 initial capital)"}, nil
		},
		execute: func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
			t.Error("execute must not be called while the daily loss limit is tripped")
			return nil, "", nil
		},
		fetchMids: func([]string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		},
	}
	_, err := manualOpenCore(deps, sc, manualOpenInputs{StrategyID: "m", Margin: 50})
	if err == nil || !strings.Contains(err.Error(), "daily loss limit tripped") {
		t.Fatalf("manual-open err = %v, want daily-loss refusal", err)
	}
}

func TestManualAddCoreRefusesDailyLossHold(t *testing.T) {
	sc := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 3}
	pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}
	deps := manualCoreDeps{
		cfg: &Config{},
		loadState: func(strategyID, symbol string) (manualStateView, error) {
			return manualStateView{HasStrategy: true, Pos: pos, DailyLossHold: true,
				DailyLossNote: "daily loss limit tripped: today's realized loss $600.00 >= threshold $500.00 (pre-fee; basis=$0.00 initial capital)"}, nil
		},
		execute: func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
			t.Error("execute must not be called while the daily loss limit is tripped")
			return nil, "", nil
		},
		fetchMids: func([]string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		},
	}
	_, err := manualAddCore(deps, sc, manualAddInputs{StrategyID: "m", Margin: 50})
	if err == nil || !strings.Contains(err.Error(), "daily loss limit tripped") {
		t.Fatalf("manual-add err = %v, want daily-loss refusal", err)
	}
}
