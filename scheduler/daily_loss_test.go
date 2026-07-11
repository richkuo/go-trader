package main

import (
	"strings"
	"testing"
	"time"
)

// #1269: portfolio-wide daily loss limit — pure gate evaluation, alert
// throttle, operator surfaces, and the manual-core refusals. Signal
// classification parity with pause is by construction (the dispatch sites
// call the same pausedBlocksSignal predicate, covered by pause_test.go).

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
	st := evaluateDailyLossLimit(&PortfolioRiskConfig{MaxDrawdownPct: 25}, states, time.Now().UTC())
	if st.Configured || st.Tripped {
		t.Fatalf("unconfigured limit must never trip: %+v", st)
	}
	if st.LossUSD != 900 {
		t.Fatalf("LossUSD = %g, want 900", st.LossUSD)
	}
	// nil PortfolioRisk must be safe too
	st = evaluateDailyLossLimit(nil, states, time.Now().UTC())
	if st.Configured || st.Tripped {
		t.Fatalf("nil portfolio risk must never trip: %+v", st)
	}
}

func TestEvaluateDailyLossLimitUSDThreshold(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 500}
	now := time.Now().UTC()

	below := map[string]*StrategyState{"a": dlState("a", 0, -499.99, dlToday())}
	if st := evaluateDailyLossLimit(pr, below, now); st.Tripped {
		t.Fatalf("loss below threshold must not trip: %+v", st)
	}
	// Boundary: loss exactly at the threshold trips (>=, most protective).
	atLimit := map[string]*StrategyState{"a": dlState("a", 0, -500, dlToday())}
	if st := evaluateDailyLossLimit(pr, atLimit, now); !st.Tripped {
		t.Fatalf("loss at threshold must trip: %+v", st)
	}
	beyond := map[string]*StrategyState{
		"a": dlState("a", 0, -300, dlToday()),
		"b": dlState("b", 0, -250, dlToday()),
	}
	st := evaluateDailyLossLimit(pr, beyond, now)
	if !st.Tripped || st.LossUSD != 550 || st.ThresholdUSD != 500 {
		t.Fatalf("multi-strategy aggregate: %+v, want tripped loss=550 threshold=500", st)
	}
}

func TestEvaluateDailyLossLimitPctThreshold(t *testing.T) {
	pr := &PortfolioRiskConfig{DailyMaxLossPct: 5}
	now := time.Now().UTC()
	// basis = 2000+3000 = 5000 → threshold $250
	states := map[string]*StrategyState{
		"a": dlState("a", 2000, -100, dlToday()),
		"b": dlState("b", 3000, -160, dlToday()),
	}
	st := evaluateDailyLossLimit(pr, states, now)
	if !st.Tripped || st.CapitalBasis != 5000 || st.ThresholdUSD != 250 {
		t.Fatalf("pct arm: %+v, want tripped basis=5000 threshold=250", st)
	}
	states["b"].RiskState.DailyPnL = -140 // loss 240 < 250
	if st := evaluateDailyLossLimit(pr, states, now); st.Tripped {
		t.Fatalf("loss under pct threshold must not trip: %+v", st)
	}
}

func TestEvaluateDailyLossLimitBothArmsLowerWins(t *testing.T) {
	// usd=$400 vs pct 5% of $20k=$1000 → usd is lower and wins
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 400, DailyMaxLossPct: 5}
	states := map[string]*StrategyState{"a": dlState("a", 20000, -450, dlToday())}
	st := evaluateDailyLossLimit(pr, states, time.Now().UTC())
	if !st.Tripped || st.ThresholdUSD != 400 {
		t.Fatalf("lower arm must win: %+v, want tripped threshold=400", st)
	}
	// pct lower: usd=$2000 vs 5% of $20k=$1000
	pr = &PortfolioRiskConfig{DailyMaxLossUSD: 2000, DailyMaxLossPct: 5}
	st = evaluateDailyLossLimit(pr, states, time.Now().UTC())
	if st.Tripped || st.ThresholdUSD != 1000 {
		t.Fatalf("pct arm lower: %+v, want not tripped threshold=1000", st)
	}
}

func TestEvaluateDailyLossLimitStaleDayExcluded(t *testing.T) {
	// A strategy whose DailyPnLDate has not rolled over yet contributes 0 —
	// identical to what rolloverDailyPnL would reset it to. This is the UTC
	// rollover boundary: yesterday's bleed cannot hold today's entries.
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 100}
	states := map[string]*StrategyState{
		"stale": dlState("stale", 0, -5000, yesterday),
		"fresh": dlState("fresh", 0, -50, dlToday()),
	}
	st := evaluateDailyLossLimit(pr, states, time.Now().UTC())
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
	st := evaluateDailyLossLimit(pr, states, time.Now().UTC())
	if st.Tripped || st.LossUSD != 50 {
		t.Fatalf("net aggregate: %+v, want loss=50 not tripped", st)
	}
	// Net-positive day: LossUSD stays 0.
	states["loss"].RiskState.DailyPnL = -100
	st = evaluateDailyLossLimit(pr, states, time.Now().UTC())
	if st.Tripped || st.LossUSD != 0 {
		t.Fatalf("net-positive day: %+v, want loss=0 not tripped", st)
	}
}

func TestEvaluateDailyLossLimitManualStrategyIncluded(t *testing.T) {
	// Manual strategies skip CheckRisk but their closes call RecordTradeResult
	// (manual.go), so their DailyPnL is part of the portfolio aggregate.
	pr := &PortfolioRiskConfig{DailyMaxLossUSD: 300}
	states := map[string]*StrategyState{
		"hl-perps": {ID: "hl-perps", Type: "perps", RiskState: RiskState{DailyPnL: -200, DailyPnLDate: dlToday()}},
		"manual":   {ID: "manual", Type: "manual", RiskState: RiskState{DailyPnL: -150, DailyPnLDate: dlToday()}},
	}
	st := evaluateDailyLossLimit(pr, states, time.Now().UTC())
	if !st.Tripped || st.LossUSD != 350 {
		t.Fatalf("manual PnL must count: %+v, want tripped loss=350", st)
	}
}

func TestEvaluateDailyLossLimitPctBasisMiss(t *testing.T) {
	// pct arm configured but no strategy carries initial_capital: the arm
	// cannot evaluate. It must not trip (no phantom threshold) and must be
	// flagged so operators see the inert protection instead of assuming cover.
	pr := &PortfolioRiskConfig{DailyMaxLossPct: 5}
	states := map[string]*StrategyState{"a": dlState("a", 0, -10000, dlToday())}
	st := evaluateDailyLossLimit(pr, states, time.Now().UTC())
	if st.Tripped || !st.PctBasisMiss {
		t.Fatalf("basis-less pct arm: %+v, want not tripped with PctBasisMiss", st)
	}
	// USD arm still enforces when both are set and basis is missing.
	pr.DailyMaxLossUSD = 500
	st = evaluateDailyLossLimit(pr, states, time.Now().UTC())
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
	st := evaluateDailyLossLimit(pr, states, time.Now().UTC())
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

	if note := dailyLossStatusNote(nil, states, now); note != "" {
		t.Fatalf("unconfigured note = %q, want empty", note)
	}
	if note := dailyLossStatusNote(&PortfolioRiskConfig{MaxDrawdownPct: 25}, states, now); note != "" {
		t.Fatalf("unconfigured note = %q, want empty", note)
	}
	tripped := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossUSD: 500}, states, now)
	if !strings.Contains(tripped, "TRIPPED") || !strings.Contains(tripped, "$600.00") {
		t.Fatalf("tripped note = %q", tripped)
	}
	armed := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossUSD: 5000}, states, now)
	if !strings.Contains(armed, "armed") || !strings.Contains(armed, "$5000.00") {
		t.Fatalf("armed note = %q", armed)
	}
	miss := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossPct: 5}, map[string]*StrategyState{
		"a": dlState("a", 0, -600, dlToday()),
	}, now)
	if !strings.Contains(miss, "initial_capital") || !strings.Contains(miss, "CANNOT evaluate") {
		t.Fatalf("basis-miss note = %q", miss)
	}
	// #1291 review: with BOTH arms set and no basis, the note must show the
	// armed USD arm AND the inert pct arm — "armed" alone hides the gap.
	both := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossUSD: 5000, DailyMaxLossPct: 5}, map[string]*StrategyState{
		"a": dlState("a", 0, -600, dlToday()),
	}, now)
	if !strings.Contains(both, "armed") || !strings.Contains(both, "CANNOT evaluate") {
		t.Fatalf("both-arms basis-miss note = %q, want armed + pct warning", both)
	}
	// Tripped USD arm with inert pct arm: both lines too.
	trippedMiss := dailyLossStatusNote(&PortfolioRiskConfig{DailyMaxLossUSD: 500, DailyMaxLossPct: 5}, map[string]*StrategyState{
		"a": dlState("a", 0, -600, dlToday()),
	}, now)
	if !strings.Contains(trippedMiss, "TRIPPED") || !strings.Contains(trippedMiss, "CANNOT evaluate") {
		t.Fatalf("tripped basis-miss note = %q, want TRIPPED + pct warning", trippedMiss)
	}
}

func TestFormatDailyLossPctBasisMissDM(t *testing.T) {
	now := time.Now().UTC()
	// pct-only: the limit is fully inert.
	st := DailyLossLimitStatus{Configured: true, PctBasisMiss: true, DailyPnL: -600}
	dm := formatDailyLossPctBasisMissDM(st, now)
	if !strings.Contains(dm, "CANNOT evaluate") || !strings.Contains(dm, "fully inert") {
		t.Fatalf("pct-only DM = %q", dm)
	}
	// both arms: the DM must say the USD arm still enforces.
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
	// 0/0 (disabled) and sane values pass this check.
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
	// Under the threshold: no hold.
	state.Strategies["m"].RiskState.DailyPnL = -100
	v = manualStateViewFromState(cfg, state, "m", "ETH")
	if v.DailyLossHold {
		t.Fatalf("view = %+v, want no hold under threshold", v)
	}
	// nil cfg (bare test deps) must not panic or hold.
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
