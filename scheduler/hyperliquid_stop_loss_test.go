package main

import (
	"errors"
	"sync"
	"testing"
)

func TestEffectiveStopLossPct(t *testing.T) {
	hlPerps := func(sc StrategyConfig) StrategyConfig {
		sc.Platform = "hyperliquid"
		sc.Type = "perps"
		return sc
	}
	pf := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		sc   StrategyConfig
		want float64
	}{
		{"non-HL returns 0", StrategyConfig{Platform: "okx", Type: "perps", StopLossPct: pf(1.5)}, 0},
		{"non-perps returns 0", StrategyConfig{Platform: "hyperliquid", Type: "spot", StopLossPct: pf(1.5)}, 0},
		{"unset and no drawdown", hlPerps(StrategyConfig{Leverage: 5}), 0},
		{"explicit pct", hlPerps(StrategyConfig{StopLossPct: pf(1.5), Leverage: 5}), 1.5},
		{"trailing pct wins", hlPerps(StrategyConfig{TrailingStopPct: pf(2.5), Leverage: 5}), 2.5},
		{"trailing zero is disabled (no fallback)", hlPerps(StrategyConfig{TrailingStopPct: pf(0), MaxDrawdownPct: 5, Leverage: 5}), 0},
		{"explicit zero is disabled (no fallback)", hlPerps(StrategyConfig{StopLossPct: pf(0), MaxDrawdownPct: 5, Leverage: 5}), 0},
		{"margin pct at 20x", hlPerps(StrategyConfig{StopLossMarginPct: pf(20), Leverage: 20}), 1.0},
		{"margin pct at 10x rescales", hlPerps(StrategyConfig{StopLossMarginPct: pf(20), Leverage: 10}), 2.0},
		{"margin pct without leverage fails safe", hlPerps(StrategyConfig{StopLossMarginPct: pf(20)}), 0},
		{"explicit-zero margin disables (no fallback)", hlPerps(StrategyConfig{StopLossMarginPct: pf(0), MaxDrawdownPct: 7, Leverage: 5}), 0},
		{"explicit wins over margin", hlPerps(StrategyConfig{StopLossPct: pf(3), StopLossMarginPct: pf(20), Leverage: 10}), 3},
		{"trailing wins over explicit before validation", hlPerps(StrategyConfig{TrailingStopPct: pf(4), StopLossPct: pf(3), Leverage: 10}), 4},
		{"drawdown fallback when both nil", hlPerps(StrategyConfig{MaxDrawdownPct: 5, Leverage: 5}), 5},
		{"drawdown fallback capped at 50", hlPerps(StrategyConfig{MaxDrawdownPct: 60, Leverage: 5}), 50},
		{"drawdown fallback at cap boundary", hlPerps(StrategyConfig{MaxDrawdownPct: 50, Leverage: 5}), 50},
		{"drawdown fallback ignored when explicit set", hlPerps(StrategyConfig{StopLossPct: pf(2), MaxDrawdownPct: 10}), 2},
		{"margin fallthrough beats drawdown", hlPerps(StrategyConfig{StopLossMarginPct: pf(20), MaxDrawdownPct: 5, Leverage: 20}), 1.0},
		{"stop_loss_atr_mult_regime defers, no drawdown fallback",
			hlPerps(StrategyConfig{StopLossATRMultRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending": {ATR: 2}}}, MaxDrawdownPct: 5, Leverage: 5}), 0},
		{"trailing_stop_atr_mult_regime defers, no drawdown fallback",
			hlPerps(StrategyConfig{TrailingStopATRMultRegime: &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending": {ATR: 2}}}, MaxDrawdownPct: 5, Leverage: 5}), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EffectiveStopLossPct(c.sc)
			if got != c.want {
				t.Errorf("EffectiveStopLossPct(%+v) = %g, want %g", c.sc, got, c.want)
			}
		})
	}
}

func TestParseHyperliquidExecuteOutput_NonFatalSLErrors(t *testing.T) {
	stdout := []byte(`{
		"execution": {
			"action": "sell",
			"symbol": "BTC",
			"size": 0.01,
			"fill": {"avg_px": 67000, "total_sz": 0.01, "oid": 42}
		},
		"platform": "hyperliquid",
		"timestamp": "2026-04-23T12:00:00+00:00",
		"cancel_stop_loss_error": "trigger already cancelled",
		"stop_loss_error": "placement rejected: max triggers reached"
	}`)

	result, _, err := parseHyperliquidExecuteOutput(stdout, "warn: something", nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if result.CancelStopLossError != "trigger already cancelled" {
		t.Errorf("CancelStopLossError: got %q", result.CancelStopLossError)
	}
	if result.StopLossError != "placement rejected: max triggers reached" {
		t.Errorf("StopLossError: got %q", result.StopLossError)
	}
	if result.Execution == nil || result.Execution.Fill.OID != 42 {
		t.Errorf("main fill should still parse: %+v", result)
	}
}

func TestRunHyperliquidTrailingStopUpdate_DefersOnOpenOrderCheckFailure(t *testing.T) {
	old := runHyperliquidUpdateStopLossFunc
	defer func() { runHyperliquidUpdateStopLossFunc = old }()

	runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
		return &HyperliquidStopLossUpdateResult{OpenOrderCheckError: "indexer down"}, "", nil
	}

	trail := 3.0
	sc := StrategyConfig{ID: "hl-test", Platform: "hyperliquid", Type: "perps", Script: "shared_scripts/check_hyperliquid.py", TrailingStopPct: &trail}
	logger := silentStrategyLogger("hl-test")
	defer logger.Close()

	newHighWater, result, ok := runHyperliquidTrailingStopUpdate(sc, "ETH", "long", 0.5, &Position{AvgCost: 100}, 110, 100, 97, 111, trailingReplacePolicy{}, nil, logger)
	if ok || result == nil {
		t.Fatalf("runHyperliquidTrailingStopUpdate = (%+v, %v), want deferred result", result, ok)
	}
	if newHighWater != 100 {
		t.Fatalf("newHighWater=%v, want unchanged 100", newHighWater)
	}
}

func TestParseHyperliquidExecuteOutput_StopLossFilledImmediately(t *testing.T) {
	stdout := []byte(`{
		"execution": {"action": "buy", "symbol": "ETH", "size": 0.1, "fill": {"avg_px": 3200, "total_sz": 0.1, "oid": 1}},
		"platform": "hyperliquid",
		"timestamp": "2026-04-25T00:00:00+00:00",
		"stop_loss_filled_immediately": true
	}`)
	result, _, err := parseHyperliquidExecuteOutput(stdout, "", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !result.StopLossFilledImmediately {
		t.Errorf("expected StopLossFilledImmediately=true, got %+v", result)
	}
	if result.Execution.Fill.StopLossOID != 0 {
		t.Errorf("instant-fill should have no resting OID; got %d", result.Execution.Fill.StopLossOID)
	}
}

func TestParseHyperliquidExecuteOutput_CancelSucceededOnFailure(t *testing.T) {
	stdout := []byte(`{
		"execution": null,
		"platform": "hyperliquid",
		"timestamp": "2026-04-25T00:00:00+00:00",
		"error": "market_open: insufficient balance",
		"cancel_stop_loss_succeeded": true
	}`)
	runErr := errors.New("exit status 1")
	result, _, err := parseHyperliquidExecuteOutput(stdout, "", runErr)
	if err != nil {
		t.Fatalf("parse should swallow runErr when JSON carries .error: %v", err)
	}
	if !result.CancelStopLossSucceeded {
		t.Errorf("CancelStopLossSucceeded should be true, got %+v", result)
	}
	if result.Error == "" {
		t.Errorf("error payload should be preserved")
	}
}

func TestIsHLOpenOrderCapRejection(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"too many", "Too many open trigger orders", true},
		{"rate limit", "trigger order rate limit exceeded", true},
		{"max", "max trigger orders per day reached", true},
		{"generic too many open orders", "Too many open orders", true},
		{"generic open orders limit", "open orders limit exceeded", true},
		{"unrelated", "insufficient margin", false},
		{"empty", "", false},
		{"trigger only", "trigger price out of range", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isHLOpenOrderCapRejection(c.in); got != c.want {
				t.Errorf("isHLOpenOrderCapRejection(%q)=%v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestExecuteHyperliquidResult_StopLossFilledImmediately_ReconcilesState(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-test-eth",
		Platform: "hyperliquid",
		Type:     "perps",
		Capital:  1000,
		Leverage: 5,
	}
	state := &StrategyState{
		ID:        "hl-test-eth",
		Platform:  "hyperliquid",
		Type:      "perps",
		Cash:      1000,
		Positions: map[string]*Position{},
	}
	result := &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 3200}
	execResult := &HyperliquidExecuteResult{
		Execution: &HyperliquidExecution{
			Action: "buy",
			Symbol: "ETH",
			Size:   0.1,
			Fill: &HyperliquidFill{
				AvgPx:             3200,
				TotalSz:           0.1,
				OID:               1,
				StopLossTriggerPx: 3104.0,
			},
		},
		StopLossFilledImmediately: true,
	}

	logger := silentStrategyLogger("hl-test-eth")
	defer logger.Close()
	trades, _ := executeHyperliquidResult(sc, state, result, execResult, "BUY", 3200, nil, nil, HurstGateDecision{}, logger)

	if trades != 2 {
		t.Errorf("trades=%d, want 2 (open + synthetic close)", trades)
	}
	if _, exists := state.Positions["ETH"]; exists {
		t.Errorf("Position should have been deleted; got %+v", state.Positions["ETH"])
	}
	if len(state.ClosedPositions) != 1 {
		t.Fatalf("ClosedPositions=%d, want 1", len(state.ClosedPositions))
	}
	cp := state.ClosedPositions[0]
	if cp.CloseReason != "stop_loss_immediate" {
		t.Errorf("CloseReason=%q, want stop_loss_immediate", cp.CloseReason)
	}
	if cp.ClosePrice != 3104.0 {
		t.Errorf("ClosePrice=%v, want 3104", cp.ClosePrice)
	}
	if cp.RealizedPnL >= 0 {
		t.Errorf("RealizedPnL=%v should be negative for a long stopped out below entry", cp.RealizedPnL)
	}
}

func paperStopTestState(sc StrategyConfig, pos *Position) *StrategyState {
	return &StrategyState{ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{"ETH": pos}}
}

func TestPaperStopArmsTheLiveTriggerForEveryOwner(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	pctLive := func(sc StrategyConfig, pos *Position) float64 {
		return hlLiquidationScalarRearmTriggerPx(sc, pos.Side, pos.riskAnchorPrice(), 0)
	}
	atrLive := func(sc StrategyConfig, pos *Position) float64 {
		plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
		if !ok {
			return 0
		}
		return hlProtectionSLTriggerPx(pos.Side, plan.AvgCost, plan.EntryATR, plan.StopLossATRMult)
	}
	trailingLive := func(sc StrategyConfig, pos *Position) float64 {
		old := runHyperliquidUpdateStopLossFunc
		defer func() { runHyperliquidUpdateStopLossFunc = old }()
		placed := 0.0
		runHyperliquidUpdateStopLossFunc = func(_, _, _ string, _, triggerPx float64, _ int64) (*HyperliquidStopLossUpdateResult, string, error) {
			placed = triggerPx
			return &HyperliquidStopLossUpdateResult{StopLossOID: 1, StopLossTriggerPx: triggerPx}, "", nil
		}
		runHyperliquidTrailingStopUpdate(sc, pos.Symbol, pos.Side, pos.Quantity, hyperliquidProtectionPositionSnapshot(pos), pos.AvgCost, 0, 0, 0, trailingReplacePolicy{}, nil, silentStrategyLogger(sc.ID))
		return placed
	}
	regime := func(ranging float64) *RegimeATRBlock {
		return &RegimeATRBlock{TrendRegime: map[string]RegimeATREntry{"trending": {ATR: 3.0}, "ranging": {ATR: ranging}}}
	}
	cases := []struct {
		name       string
		side       string
		mutate     func(*StrategyConfig)
		live       func(StrategyConfig, *Position) float64
		want       float64
		wantReason string
		applied    string
	}{
		{"stop_loss_pct long", "long", func(sc *StrategyConfig) { sc.StopLossPct = pf(3) }, pctLive, 1940, paperStopReasonPct, ""},
		{"stop_loss_pct short", "short", func(sc *StrategyConfig) { sc.StopLossPct = pf(3) }, pctLive, 2060, paperStopReasonPct, ""},
		{"stop_loss_margin_pct over leverage", "long", func(sc *StrategyConfig) { sc.StopLossMarginPct = pf(30); sc.Leverage = 10 }, pctLive, 1940, paperStopReasonPct, ""},
		{"max_drawdown_pct fallback", "long", func(sc *StrategyConfig) { sc.MaxDrawdownPct = 5 }, pctLive, 1900, paperStopReasonPct, ""},
		{"trailing_stop_pct", "long", func(sc *StrategyConfig) { sc.TrailingStopPct = pf(2) }, trailingLive, 1960, paperStopReasonTrailing, ""},
		{"trailing_stop_atr_mult", "short", func(sc *StrategyConfig) { sc.TrailingStopATRMult = pf(1.5) }, trailingLive, 2060, paperStopReasonTrailing, ""},
		{"trailing_stop_atr_mult_regime", "long", func(sc *StrategyConfig) { sc.TrailingStopATRMultRegime = regime(2) }, trailingLive, 1920, paperStopReasonTrailing, ""},
		{"stop_loss_atr_mult", "long", func(sc *StrategyConfig) { sc.StopLossATRMult = pf(1.5) }, atrLive, 1940, paperStopReasonATR, ""},
		{"stop_loss_atr_mult_regime", "short", func(sc *StrategyConfig) { sc.StopLossATRMultRegime = regime(1) }, atrLive, 2040, paperStopReasonATR, ""},
		{"unified per-regime close", "long", func(sc *StrategyConfig) {
			sc.CloseStrategy = &StrategyRef{Name: "tiered_tp_atr_regime", Params: unifiedBlock()}
		}, atrLive, 1968, paperStopReasonATR, ""},
		{"dynamic unified close follows the applied label", "long", func(sc *StrategyConfig) {
			sc.CloseStrategy = &StrategyRef{Name: dynamicCloseStrategyName, Params: unifiedBlock()}
		}, atrLive, 1940, paperStopReasonATR, "trending_up"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := StrategyConfig{ID: "hl-paper", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h"}}
			c.mutate(&sc)
			newPos := func() *Position {
				return &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 40, Side: c.side, Regime: "ranging", RegimeAppliedLabel: c.applied}
			}
			live := c.live(sc, newPos())
			if !approxEq(live, c.want) {
				t.Fatalf("live trigger = %v, want %v", live, c.want)
			}

			pos := newPos()
			s := paperStopTestState(sc, pos)
			var mu sync.RWMutex
			breach, _, reason := armPaperStopLossAtOpen(sc, s, "ETH", 2000, silentStrategyLogger(sc.ID))
			if breach || reason != c.wantReason || !approxEq(pos.StopLossTriggerPx, live) {
				t.Fatalf("paper arm = (breach %v reason %q trigger %v), want (false %q %v)", breach, reason, pos.StopLossTriggerPx, c.wantReason, live)
			}

			inside, past := live+5, live-5
			if c.side == "short" {
				inside, past = live-5, live+5
			}
			if n, _ := applyPaperStopLossBreach(sc, s, "ETH", c.side, inside, &mu, silentStrategyLogger(sc.ID)); n != 0 || s.Positions["ETH"] == nil {
				t.Fatalf("mark inside the trigger closed the position (trades %d)", n)
			}
			n, _ := applyPaperStopLossBreach(sc, s, "ETH", c.side, past, &mu, silentStrategyLogger(sc.ID))
			if n != 1 || s.Positions["ETH"] != nil {
				t.Fatalf("mark past the trigger: trades %d, position %+v; want one stop close", n, s.Positions["ETH"])
			}
			if len(s.ClosedPositions) != 1 || s.ClosedPositions[0].ClosePrice != past || s.ClosedPositions[0].CloseReason != c.wantReason {
				t.Fatalf("closed = %+v, want one close @ %v reason %q", s.ClosedPositions, past, c.wantReason)
			}
		})
	}
}

func TestPaperStopBreachesOnEveryCycle(t *testing.T) {
	pf := func(v float64) *float64 { return &v }
	sc := StrategyConfig{ID: "hl-paper", Platform: "hyperliquid", Type: "perps", Args: []string{"sma", "ETH", "1h"}, StopLossPct: pf(3), Direction: DirectionBoth, Leverage: 1, SizingLeverage: 1}
	logger := silentStrategyLogger(sc.ID)

	t.Run("open cycle with the mark already past the trigger", func(t *testing.T) {
		s := paperStopTestState(sc, &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, Side: "long"})
		breach, fillPx, reason := armPaperStopLossAtOpen(sc, s, "ETH", 1900, logger)
		if !breach || fillPx != 1900 || reason != paperStopReasonPct || s.Positions["ETH"].StopLossTriggerPx != 1940 {
			t.Fatalf("open-cycle arm = (breach %v fill %v reason %q trigger %v), want (true 1900 %q 1940)", breach, fillPx, reason, s.Positions["ETH"].StopLossTriggerPx, paperStopReasonPct)
		}
	})

	cases := []struct {
		name          string
		side          string
		trigger       float64
		mark          float64
		signal        int
		closeFraction float64
		wantFill      float64
		wantOpenSide  string
	}{
		{"hold cycle books the trigger when the mark touches it", "long", 1940, 1940, 0, 0, 1940, ""},
		{"hold cycle gap books the worse mark", "long", 1940, 1900, 0, 0, 1900, ""},
		{"short gap books the worse mark", "short", 2060, 2100, 0, 0, 2100, ""},
		{"close signal runs against the flat book", "long", 1940, 1900, -1, 1, 1900, ""},
		{"same-side repeat signal reopens from flat", "long", 1940, 1900, 1, 0, 1900, "long"},
		{"flip signal opens the new side from flat", "long", 1940, 1900, -1, 0, 1900, "short"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := paperStopTestState(sc, &Position{Symbol: "ETH", Quantity: 0.5, AvgCost: 2000, Side: c.side, StopLossTriggerPx: c.trigger})
			var mu sync.RWMutex
			n, _ := applyPaperStopLossBreach(sc, s, "ETH", c.side, c.mark, &mu, logger)
			if n != 1 || len(s.ClosedPositions) != 1 || s.ClosedPositions[0].ClosePrice != c.wantFill {
				t.Fatalf("stop = trades %d closed %+v, want one close @ %v", n, s.ClosedPositions, c.wantFill)
			}
			if c.signal == 0 {
				return
			}
			result := &HyperliquidResult{Symbol: "ETH", Signal: c.signal, Price: c.mark}
			result.CloseFraction = c.closeFraction
			executeHyperliquidResultDeferredOpen(sc, s, result, nil, "SIGNAL", c.mark, nil, &Config{}, HurstGateDecision{}, logger)
			pos := s.Positions["ETH"]
			if len(s.ClosedPositions) != 1 {
				t.Fatalf("closed positions after the signal = %+v, want only the stop close", s.ClosedPositions)
			}
			if c.wantOpenSide == "" {
				if pos != nil {
					t.Fatalf("signal opened %+v, want a flat book", pos)
				}
				return
			}
			want := percentStopLossTriggerPx(sc, c.wantOpenSide, pos.AvgCost)
			if pos == nil || pos.Side != c.wantOpenSide || want <= 0 || !approxEq(pos.StopLossTriggerPx, want) {
				t.Fatalf("reopened position = %+v, want %s armed at %v", pos, c.wantOpenSide, want)
			}
		})
	}
}

func TestDeferredOpenArmsTheStopOnlyForPaper(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })
	pf := func(v float64) *float64 { return &v }
	cases := []struct {
		name        string
		args        []string
		exec        *HyperliquidExecuteResult
		wantTrigger func(pos *Position) float64
	}{
		{"paper open arms at the fill", []string{"sma", "ETH", "1h"}, nil, func(pos *Position) float64 { return pos.AvgCost * 0.97 }},
		{"live open keeps the execute trigger", []string{"sma", "ETH", "1h", "--mode=live"}, &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: 0.5, StopLossOID: 77, StopLossTriggerPx: 1941}}}, func(*Position) float64 { return 1941 }},
		{"live open without an execute stop stays unarmed", []string{"sma", "ETH", "1h", "--mode=live"}, &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: 0.5}}}, func(*Position) float64 { return 0 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := StrategyConfig{ID: "hl-open", Platform: "hyperliquid", Type: "perps", Args: c.args, StopLossPct: pf(3), Direction: DirectionLong, Leverage: 1, SizingLeverage: 1}
			s := &StrategyState{ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{}}
			trades, _, openTrade, _ := executeHyperliquidResultDeferredOpen(sc, s, &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 2000}, c.exec, "BUY", 2000, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(sc.ID))
			pos := s.Positions["ETH"]
			if trades != 1 || pos == nil {
				t.Fatalf("open = trades %d position %+v, want one open", trades, pos)
			}
			want := c.wantTrigger(pos)
			if !approxEq(pos.StopLossTriggerPx, want) {
				t.Fatalf("trigger = %v, want %v", pos.StopLossTriggerPx, want)
			}
			if c.exec == nil {
				if openTrade != nil || len(s.TradeHistory) != 1 || !approxEq(s.TradeHistory[0].StopLossTriggerPx, want) {
					t.Fatalf("paper open trade row = %+v, want the armed trigger %v recorded", s.TradeHistory, want)
				}
			}
		})
	}
}
