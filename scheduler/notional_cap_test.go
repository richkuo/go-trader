package main

import (
	"strings"
	"testing"
)

// #1344: portfolio gross notional cap must hold entries without skipping the
// strategy cycle (closes / SL/TP maintenance). Signal classification reuses
// pausedBlocksSignal (covered by pause_test.go); these tests lock the
// never-skip invariant, the entry-hold contract at the dispatch sites, and
// the manual-core refusals.

func TestNotionalCapNeverSkipsStrategyCycle(t *testing.T) {
	// Pre-#1344 the dispatch loop `continue`d when notionalBlocked — that is
	// the bug. The helper must stay false for both states so a reintroduced
	// whole-strategy skip fails this regression.
	if notionalCapSkipsStrategyCycle(false) {
		t.Fatal("notionalCapSkipsStrategyCycle(false) must be false")
	}
	if notionalCapSkipsStrategyCycle(true) {
		t.Fatal("notionalCapSkipsStrategyCycle(true) must be false — over-cap must not skip close/SL maintenance (#1344)")
	}
}

func TestNotionalCapHoldPassesReduceAndManage(t *testing.T) {
	// Acceptance: over max_notional with open long + SELL still closes/reduces;
	// Signal==0 manage (trailing SL/TP) is never held. Fresh opens / adds hold.
	// Mirrors the dispatch-site predicate: notionalBlocked && pausedBlocksSignal(...).
	const notionalBlocked = true
	cases := []struct {
		name          string
		signal        int
		closeFraction float64
		posQty        float64
		posSide       string
		allowsLong    bool
		allowsShort   bool
		wantHold      bool
	}{
		{"manage_only_signal0", 0, 0, 1, "long", true, true, false},
		{"long_sell_pure_close", -1, 0, 1, "long", true, false, false},
		{"long_close_fraction", -1, 0.5, 1, "long", true, true, false},
		{"flat_buy_fresh_open", 1, 0, 0, "", true, true, true},
		{"long_buy_scale_in", 1, 0, 1, "long", true, true, true},
		{"long_sell_flip_both", -1, 0, 1, "long", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			held := notionalBlocked && pausedBlocksSignal(tc.signal, tc.closeFraction, tc.posQty, tc.posSide, tc.allowsLong, tc.allowsShort)
			if held != tc.wantHold {
				t.Fatalf("hold=%v want %v (signal=%d cf=%.2f qty=%.1f side=%q)",
					held, tc.wantHold, tc.signal, tc.closeFraction, tc.posQty, tc.posSide)
			}
		})
	}
}

func TestEvaluateNotionalCapHold(t *testing.T) {
	states := map[string]*StrategyState{
		"a": {ID: "a", Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 1, AvgCost: 60000, Side: "long"},
		}},
	}
	// Unconfigured / nil — never holds.
	if held, _ := evaluateNotionalCapHold(nil, states, nil); held {
		t.Fatal("nil portfolio risk must not hold")
	}
	if held, _ := evaluateNotionalCapHold(&PortfolioRiskConfig{MaxNotionalUSD: 0}, states, nil); held {
		t.Fatal("disabled notional cap must not hold")
	}
	// Under cap (AvgCost fallback with nil prices).
	if held, _ := evaluateNotionalCapHold(&PortfolioRiskConfig{MaxNotionalUSD: 100000}, states, nil); held {
		t.Fatal("under-cap book must not hold")
	}
	// Over cap.
	held, detail := evaluateNotionalCapHold(&PortfolioRiskConfig{MaxNotionalUSD: 50000}, states, nil)
	if !held {
		t.Fatal("over-cap book must hold")
	}
	if !strings.Contains(detail, "new opens blocked, exits continue") {
		t.Fatalf("detail=%q, want exits-continue audit wording", detail)
	}
	if !strings.Contains(detail, "60000.00") || !strings.Contains(detail, "50000.00") {
		t.Fatalf("detail=%q, want notional and cap amounts", detail)
	}
}

func TestCheckPortfolioRisk_NotionalCapReasonMentionsExitsContinue(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 50000, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}
	_, nb, _, reason := CheckPortfolioRisk(prs, cfg, 10000.0, 60000.0, 0, 0)
	if !nb {
		t.Fatal("expected notionalBlocked=true over cap")
	}
	if !strings.Contains(reason, "new opens blocked, exits continue") {
		t.Fatalf("reason=%q, want #1344 exits-continue wording", reason)
	}
}

func TestManualStateViewNotionalHold(t *testing.T) {
	cfg := &Config{PortfolioRisk: &PortfolioRiskConfig{MaxNotionalUSD: 50000}}
	state := &AppState{Strategies: map[string]*StrategyState{
		"m": {ID: "m", Type: "manual", Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 30, AvgCost: 2000, Side: "long"}, // $60k > $50k
		}},
	}}
	v := manualStateViewFromState(cfg, state, "m", "ETH")
	if !v.NotionalHold || v.NotionalNote == "" {
		t.Fatalf("view = %+v, want NotionalHold with note", v)
	}
	// Under the cap: no hold.
	state.Strategies["m"].Positions["ETH"].Quantity = 10 // $20k
	v = manualStateViewFromState(cfg, state, "m", "ETH")
	if v.NotionalHold {
		t.Fatalf("view = %+v, want no hold under cap", v)
	}
	// nil cfg must not panic or hold.
	v = manualStateViewFromState(nil, state, "m", "ETH")
	if v.NotionalHold {
		t.Fatalf("nil cfg view = %+v, want no hold", v)
	}
}

func TestManualOpenCoreRefusesNotionalHold(t *testing.T) {
	sc := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 3}
	deps := manualCoreDeps{
		cfg: &Config{},
		loadState: func(strategyID, symbol string) (manualStateView, error) {
			return manualStateView{HasStrategy: true, NotionalHold: true,
				NotionalNote: "portfolio notional $60000.00 exceeds cap $50000.00 — new opens blocked, exits continue"}, nil
		},
		execute: func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
			t.Error("execute must not be called while the notional cap is breached")
			return nil, "", nil
		},
		fetchMids: func([]string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		},
	}
	_, err := manualOpenCore(deps, sc, manualOpenInputs{StrategyID: "m", Margin: 50})
	if err == nil || !strings.Contains(err.Error(), "new opens blocked, exits continue") {
		t.Fatalf("manual-open err = %v, want notional refusal", err)
	}
}

func TestManualAddCoreRefusesNotionalHold(t *testing.T) {
	sc := StrategyConfig{ID: "m", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 3}
	pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"}
	deps := manualCoreDeps{
		cfg: &Config{},
		loadState: func(strategyID, symbol string) (manualStateView, error) {
			return manualStateView{HasStrategy: true, Pos: pos, NotionalHold: true,
				NotionalNote: "portfolio notional $60000.00 exceeds cap $50000.00 — new opens blocked, exits continue"}, nil
		},
		execute: func(string, string, string, float64, float64, int64, float64, string, float64, bool, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
			t.Error("execute must not be called while the notional cap is breached")
			return nil, "", nil
		},
		fetchMids: func([]string) (map[string]float64, error) {
			return map[string]float64{"ETH": 2000}, nil
		},
	}
	_, err := manualAddCore(deps, sc, manualAddInputs{StrategyID: "m", Margin: 50})
	if err == nil || !strings.Contains(err.Error(), "new opens blocked, exits continue") {
		t.Fatalf("manual-add err = %v, want notional refusal", err)
	}
}
