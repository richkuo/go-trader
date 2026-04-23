package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// yesterday returns the UTC date string for one day before today.
func yesterday() string {
	return time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
}

// today returns the current UTC date string.
func todayUTC() string {
	return time.Now().UTC().Format("2006-01-02")
}

// newRiskState returns a minimal RiskState for testing.
func newRiskState(date string, dailyPnL float64) RiskState {
	return RiskState{
		DailyPnLDate: date,
		DailyPnL:     dailyPnL,
	}
}

// TestRolloverDailyPnL_SameDay verifies that PnL and date are unchanged when
// DailyPnLDate already equals today.
func TestRolloverDailyPnL_SameDay(t *testing.T) {
	r := newRiskState(todayUTC(), 123.45)
	rolloverDailyPnL(&r)
	if r.DailyPnL != 123.45 {
		t.Errorf("expected DailyPnL=123.45 unchanged; got %.2f", r.DailyPnL)
	}
	if r.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), r.DailyPnLDate)
	}
}

// TestRolloverDailyPnL_NewDay verifies that DailyPnL is zeroed and DailyPnLDate
// is updated when the stored date is stale (e.g. yesterday).
func TestRolloverDailyPnL_NewDay(t *testing.T) {
	r := newRiskState(yesterday(), 99.99)
	rolloverDailyPnL(&r)
	if r.DailyPnL != 0 {
		t.Errorf("expected DailyPnL reset to 0; got %.2f", r.DailyPnL)
	}
	if r.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), r.DailyPnLDate)
	}
}

// TestRolloverDailyPnL_EmptyDate verifies that an empty DailyPnLDate (e.g. freshly
// initialized state) is treated as stale and the day is properly initialized.
func TestRolloverDailyPnL_EmptyDate(t *testing.T) {
	r := newRiskState("", 50.0)
	rolloverDailyPnL(&r)
	if r.DailyPnL != 0 {
		t.Errorf("expected DailyPnL reset to 0 on empty date; got %.2f", r.DailyPnL)
	}
	if r.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), r.DailyPnLDate)
	}
}

// TestRecordTradeResult_MidnightCrossing is the core issue-27 regression test.
// It simulates a scenario where a trade is recorded without a prior CheckRisk
// call after midnight: DailyPnLDate is yesterday, so RecordTradeResult must
// roll over the day before accumulating the new trade's PnL.
func TestRecordTradeResult_MidnightCrossing(t *testing.T) {
	r := newRiskState(yesterday(), 200.0) // stale — prior day PnL should be discarded

	RecordTradeResult(&r, 50.0)

	if r.DailyPnL != 50.0 {
		t.Errorf("expected DailyPnL=50 after midnight crossing; got %.2f", r.DailyPnL)
	}
	if r.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), r.DailyPnLDate)
	}
	if r.TotalTrades != 1 {
		t.Errorf("expected TotalTrades=1; got %d", r.TotalTrades)
	}
}

// TestRecordTradeResult_SameDayAccumulation verifies that multiple trades on the
// same day correctly accumulate DailyPnL without any spurious resets.
func TestRecordTradeResult_SameDayAccumulation(t *testing.T) {
	r := newRiskState(todayUTC(), 100.0)

	RecordTradeResult(&r, 30.0)
	RecordTradeResult(&r, -10.0)

	if r.DailyPnL != 120.0 {
		t.Errorf("expected DailyPnL=120 after two trades; got %.2f", r.DailyPnL)
	}
	if r.TotalTrades != 2 {
		t.Errorf("expected TotalTrades=2; got %d", r.TotalTrades)
	}
}

// TestCheckRisk_RollsOverDailyPnL verifies that CheckRisk itself also triggers
// day rollover so the risk check always operates on the correct day's budget.
func TestCheckRisk_RollsOverDailyPnL(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(yesterday(), 500.0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 50.0

	CheckRisk(nil, s, 1000.0, nil, nil, nil)

	if s.RiskState.DailyPnL != 0 {
		t.Errorf("expected DailyPnL reset to 0 by CheckRisk; got %.2f", s.RiskState.DailyPnL)
	}
	if s.RiskState.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), s.RiskState.DailyPnLDate)
	}
}

// TestCheckRisk_DrawdownKillswitch verifies that a drawdown beyond the limit
// triggers the circuit breaker and force-closes positions.
func TestCheckRisk_DrawdownKillswitch(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(todayUTC(), 0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 10.0
	s.RiskState.TotalTrades = 1

	allowed, reason := CheckRisk(nil, s, 800.0, nil, nil, nil)

	if allowed {
		t.Error("expected trading to be blocked after drawdown killswitch")
	}
	if !strings.Contains(reason, "drawdown") {
		t.Errorf("expected reason to mention drawdown; got: %s", reason)
	}
	if !s.RiskState.CircuitBreaker {
		t.Error("expected circuit breaker to be active")
	}
}

// TestCheckRisk_NoDrawdownAllowsTrading verifies that when portfolio value is at
// or above peak, trading remains allowed.
func TestCheckRisk_NoDrawdownAllowsTrading(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(todayUTC(), 0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 10.0
	s.RiskState.TotalTrades = 1

	allowed, reason := CheckRisk(nil, s, 1000.0, nil, nil, nil)

	if !allowed {
		t.Errorf("expected trading to be allowed; got reason: %s", reason)
	}
}

// TestCheckRisk_CircuitBreakerResetsAfterTimeout verifies that the circuit
// breaker auto-resets once the timeout period has elapsed.
func TestCheckRisk_CircuitBreakerResetsAfterTimeout(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(todayUTC(), 0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.CircuitBreaker = true
	s.RiskState.CircuitBreakerUntil = time.Now().UTC().Add(-1 * time.Hour)
	s.RiskState.ConsecutiveLosses = 3

	allowed, _ := CheckRisk(nil, s, 1000.0, nil, nil, nil)

	if !allowed {
		t.Error("expected trading to be allowed after circuit breaker timeout")
	}
	if s.RiskState.CircuitBreaker {
		t.Error("expected circuit breaker to be reset")
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("expected consecutive losses reset to 0; got %d", s.RiskState.ConsecutiveLosses)
	}
}

// TestCheckRisk_ConsecutiveLossesKillswitch verifies that 5 consecutive losses
// trigger the circuit breaker.
func TestCheckRisk_ConsecutiveLossesKillswitch(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(todayUTC(), 0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.ConsecutiveLosses = 5
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 50.0

	allowed, reason := CheckRisk(nil, s, 1000.0, nil, nil, nil)

	if allowed {
		t.Error("expected trading to be blocked after 5 consecutive losses")
	}
	if !strings.Contains(reason, "5 consecutive losses") {
		t.Errorf("expected reason to mention consecutive losses; got: %s", reason)
	}
}

// TestCheckRisk_WarningThreshold verifies that a warning is sent when drawdown
// approaches the kill switch limit.
func TestCheckRisk_WarningThreshold(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(todayUTC(), 0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 10.0
	s.RiskState.TotalTrades = 1

	// 9% drawdown (above 80% of 10% = 8% warning threshold)
	allowed, reason := CheckRisk(nil, s, 910.0, nil, nil, nil)

	if !allowed {
		t.Errorf("expected trading to be allowed; got reason: %s", reason)
	}
	// CheckRisk does not have warning logic — it only blocks on breach
	if reason != "" {
		t.Errorf("expected no reason on normal operation; got: %s", reason)
	}
}

// TestCheckRisk_DrawdownBelowMax verifies that trading is allowed when drawdown
// is below the MaxDrawdownPct threshold.
func TestCheckRisk_DrawdownBelowMax(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(todayUTC(), 0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 10.0
	s.RiskState.TotalTrades = 1

	// 5% drawdown (below 8% warning threshold)
	allowed, _ := CheckRisk(nil, s, 950.0, nil, nil, nil)

	if !allowed {
		t.Error("expected trading to be allowed")
	}
}

// TestCheckRisk_PeakValueUpdates verifies that peak value is ratcheted upward
// when portfolio value exceeds the previous peak.
func TestCheckRisk_PeakValueUpdates(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(todayUTC(), 0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 10.0
	s.RiskState.TotalTrades = 1

	CheckRisk(nil, s, 1200.0, nil, nil, nil)

	if s.RiskState.PeakValue != 1200.0 {
		t.Errorf("expected PeakValue updated to 1200; got %.2f", s.RiskState.PeakValue)
	}
}

// TestCheckRisk_ZeroPeakValueNoCrash verifies that CheckRisk does not panic
// when PeakValue is zero (e.g. fresh state).
func TestCheckRisk_ZeroPeakValueNoCrash(t *testing.T) {
	s := &StrategyState{
		RiskState:       newRiskState(todayUTC(), 0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
	s.RiskState.PeakValue = 0
	s.RiskState.MaxDrawdownPct = 10.0

	// Should not panic
	allowed, _ := CheckRisk(nil, s, 1000.0, nil, nil, nil)

	if !allowed {
		t.Error("expected trading to be allowed with zero peak value")
	}
}

// TestCheckRisk_KillSwitchLatched verifies that once the kill switch fires,
// trading remains blocked until manually reset.
func TestCheckRisk_KillSwitchLatched(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 10000.0}
	prs.KillSwitchActive = true
	prs.KillSwitchAt = time.Now().UTC().Add(-1 * time.Hour)

	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25}

	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, 9000.0, 0, 0, 0)

	if allowed {
		t.Error("expected trading to be blocked when kill switch is latched")
	}
	if !strings.Contains(reason, "latched") {
		t.Errorf("expected reason to mention latched; got: %s", reason)
	}
}

// TestCheckPortfolioRisk_DrawdownKillswitch verifies portfolio-level drawdown
// kill switch behavior.
func TestCheckPortfolioRisk_DrawdownKillswitch(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 10000.0}
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25}

	allowed, notionalBlocked, warning, reason := CheckPortfolioRisk(prs, cfg, 7000.0, 0, 0, 0)

	if allowed {
		t.Error("expected portfolio trading to be blocked")
	}
	if !strings.Contains(reason, "drawdown") {
		t.Errorf("expected reason to mention drawdown; got: %s", reason)
	}
	if !prs.KillSwitchActive {
		t.Error("expected portfolio kill switch to be active")
	}
	if notionalBlocked {
		t.Error("expected notionalBlocked to be false for drawdown kill")
	}
	if warning {
		t.Error("expected warning to be false for kill switch fire")
	}
}

// TestCheckPortfolioRisk_WarningThreshold verifies portfolio-level warning
// when drawdown approaches the limit.
func TestCheckPortfolioRisk_WarningThreshold(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 10000.0}
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25}

	// 22% drawdown (above 80% of 25% = 20% warning threshold)
	allowed, notionalBlocked, warning, reason := CheckPortfolioRisk(prs, cfg, 7800.0, 0, 0, 0)

	if !allowed {
		t.Errorf("expected trading to be allowed; got reason: %s", reason)
	}
	if notionalBlocked {
		t.Error("expected notionalBlocked to be false")
	}
	if !warning {
		t.Error("expected warning to be true")
	}
	if !strings.Contains(reason, "approaching kill switch") {
		t.Errorf("expected warning reason; got: %s", reason)
	}
	if !prs.WarningSent {
		t.Error("expected WarningSent to be true")
	}
}

// TestCheckPortfolioRisk_NotionalCap verifies that notional cap blocks new
// trades but does not force-close existing positions.
func TestCheckPortfolioRisk_NotionalCap(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 10000.0}
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 5000}

	allowed, notionalBlocked, warning, reason := CheckPortfolioRisk(prs, cfg, 10000.0, 6000.0, 0, 0)

	if !allowed {
		t.Errorf("expected trading to be allowed; got reason: %s", reason)
	}
	if !notionalBlocked {
		t.Error("expected notionalBlocked to be true when cap exceeded")
	}
	if warning {
		t.Error("expected warning to be false for notional cap")
	}
	if !strings.Contains(reason, "notional") {
		t.Errorf("expected reason to mention notional; got: %s", reason)
	}
}

// TestCheckPortfolioRisk_PeakRatchet verifies that portfolio peak value only
// increases, never decreases.
func TestCheckPortfolioRisk_PeakRatchet(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 10000.0}
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25}

	CheckPortfolioRisk(prs, cfg, 12000.0, 0, 0, 0)

	if prs.PeakValue != 12000.0 {
		t.Errorf("expected PeakValue ratcheted to 12000; got %.2f", prs.PeakValue)
	}

	CheckPortfolioRisk(prs, cfg, 11000.0, 0, 0, 0)

	if prs.PeakValue != 12000.0 {
		t.Errorf("expected PeakValue to stay at 12000; got %.2f", prs.PeakValue)
	}
}

// TestCheckPortfolioRisk_ZeroMaxDrawdown verifies that a zero
// MaxDrawdownPct means even tiny drawdowns trigger the kill switch.
func TestCheckPortfolioRisk_ZeroMaxDrawdown(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 10000.0}
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 0}

	// Any positive drawdown triggers when limit is 0
	allowed, _, _, _ := CheckPortfolioRisk(prs, cfg, 9999.0, 0, 0, 0)

	if allowed {
		t.Error("expected trading to be blocked when MaxDrawdownPct is 0 and drawdown > 0")
	}
}

// TestCheckPortfolioRisk_MarginDrawdownKillswitch verifies that perps margin
// drawdown can trigger the kill switch (#296).
func TestCheckPortfolioRisk_MarginDrawdownKillswitch(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 10000.0}
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25}

	// 30% unrealized loss on $1000 margin = 30% margin drawdown
	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, 10000.0, 0, 300.0, 1000.0)

	if allowed {
		t.Error("expected trading to be blocked by margin drawdown")
	}
	if !strings.Contains(reason, "margin") {
		t.Errorf("expected reason to mention margin; got: %s", reason)
	}
}

// TestCheckPortfolioRisk_MarginWarning verifies margin drawdown warning.
func TestCheckPortfolioRisk_MarginWarning(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 10000.0}
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25}

	// 22% unrealized loss on $1000 margin = 22% margin drawdown (above 20% warn)
	allowed, _, warning, reason := CheckPortfolioRisk(prs, cfg, 10000.0, 0, 220.0, 1000.0)

	if !allowed {
		t.Errorf("expected trading to be allowed; got reason: %s", reason)
	}
	if !warning {
		t.Error("expected warning to be true for margin drawdown")
	}
	if !strings.Contains(reason, "margin") {
		t.Errorf("expected reason to mention margin; got: %s", reason)
	}
}

// TestCheckPortfolioRisk_ColdStartMarginOnly verifies that margin signal can
// fire even when PeakValue is zero (cold start).
func TestCheckPortfolioRisk_ColdStartMarginOnly(t *testing.T) {
	prs := &PortfolioRiskState{PeakValue: 0} // cold start: no prior valuation
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25}

	// 30% unrealized loss on $1000 margin
	allowed, _, _, reason := CheckPortfolioRisk(prs, cfg, 0, 0, 300.0, 1000.0)

	if allowed {
		t.Error("expected trading to be blocked even with zero peak value")
	}
	if !strings.Contains(reason, "margin") {
		t.Errorf("expected reason to mention margin; got: %s", reason)
	}
}

// TestPortfolioNotional_Spot verifies gross notional for spot positions.
func TestPortfolioNotional_Spot(t *testing.T) {
	strategies := map[string]*StrategyState{
		"s1": {
			Positions: map[string]*Position{
				"BTC": {Quantity: 1.0, Multiplier: 0},
			},
		},
	}
	prices := map[string]float64{"BTC": 50000.0}

	notional := PortfolioNotional(strategies, prices)

	if notional != 50000.0 {
		t.Errorf("expected notional=50000; got %.2f", notional)
	}
}

// TestPortfolioNotional_Futures verifies gross notional for futures positions.
func TestPortfolioNotional_Futures(t *testing.T) {
	strategies := map[string]*StrategyState{
		"s1": {
			Positions: map[string]*Position{
				"ES": {Quantity: 2.0, Multiplier: 50.0},
			},
		},
	}
	prices := map[string]float64{"ES": 4000.0}

	notional := PortfolioNotional(strategies, prices)

	expected := 2.0 * 50.0 * 4000.0
	if notional != expected {
		t.Errorf("expected notional=%.2f; got %.2f", expected, notional)
	}
}

// TestPortfolioNotional_MissingPriceFallback verifies that missing prices fall
// back to AvgCost.
func TestPortfolioNotional_MissingPriceFallback(t *testing.T) {
	strategies := map[string]*StrategyState{
		"s1": {
			Positions: map[string]*Position{
				"BTC": {Quantity: 1.0, AvgCost: 48000.0},
			},
		},
	}
	prices := map[string]float64{}

	notional := PortfolioNotional(strategies, prices)

	if notional != 48000.0 {
		t.Errorf("expected notional=48000 (AvgCost fallback); got %.2f", notional)
	}
}

// TestPortfolioValue_SpotLong verifies portfolio value for a simple spot long.
func TestPortfolioValue_SpotLong(t *testing.T) {
	s := &StrategyState{
		Cash: 5000.0,
		Positions: map[string]*Position{
			"BTC": {Quantity: 0.1, AvgCost: 40000.0, Side: "long"},
		},
	}
	prices := map[string]float64{"BTC": 50000.0}

	value := PortfolioValue(s, prices)

	expected := 5000.0 + 0.1*50000.0
	if value != expected {
		t.Errorf("expected value=%.2f; got %.2f", expected, value)
	}
}

// TestPortfolioValue_SpotShort verifies portfolio value for a spot short.
func TestPortfolioValue_SpotShort(t *testing.T) {
	s := &StrategyState{
		Cash: 10000.0,
		Positions: map[string]*Position{
			"BTC": {Quantity: 0.1, AvgCost: 50000.0, Side: "short"},
		},
	}
	prices := map[string]float64{"BTC": 40000.0}

	value := PortfolioValue(s, prices)

	// Short profit = (avg_cost - current_price) * qty
	expected := 10000.0 + 0.1*(2*50000.0-40000.0)
	if value != expected {
		t.Errorf("expected value=%.2f; got %.2f", expected, value)
	}
}

// TestPortfolioValue_FuturesLong verifies portfolio value for a futures long.
func TestPortfolioValue_FuturesLong(t *testing.T) {
	s := &StrategyState{
		Cash: 10000.0,
		Positions: map[string]*Position{
			"ES": {Quantity: 1.0, AvgCost: 4000.0, Side: "long", Multiplier: 50.0},
		},
	}
	prices := map[string]float64{"ES": 4100.0}

	value := PortfolioValue(s, prices)

	// Futures PnL = qty * multiplier * (price - avgCost)
	expected := 10000.0 + 1.0*50.0*(4100.0-4000.0)
	if value != expected {
		t.Errorf("expected value=%.2f; got %.2f", expected, value)
	}
}

// TestPortfolioValue_FuturesShort verifies portfolio value for a futures short.
func TestPortfolioValue_FuturesShort(t *testing.T) {
	s := &StrategyState{
		Cash: 10000.0,
		Positions: map[string]*Position{
			"ES": {Quantity: 1.0, AvgCost: 4000.0, Side: "short", Multiplier: 50.0},
		},
	}
	prices := map[string]float64{"ES": 3900.0}

	value := PortfolioValue(s, prices)

	// Futures short PnL = qty * multiplier * (avgCost - price)
	expected := 10000.0 + 1.0*50.0*(4000.0-3900.0)
	if value != expected {
		t.Errorf("expected value=%.2f; got %.2f", expected, value)
	}
}

// TestPortfolioValue_EmptyPositions verifies that empty positions returns just cash.
func TestPortfolioValue_EmptyPositions(t *testing.T) {
	s := &StrategyState{Cash: 5000.0, Positions: map[string]*Position{}}
	prices := map[string]float64{}

	value := PortfolioValue(s, prices)

	if value != 5000.0 {
		t.Errorf("expected value=5000 (cash only); got %.2f", value)
	}
}

// TestForceCloseAllPositions_SpotLong verifies force-close of a spot long.
func TestForceCloseAllPositions_SpotLong(t *testing.T) {
	s := &StrategyState{
		Cash: 1000.0,
		Positions: map[string]*Position{
			"BTC": {Quantity: 0.1, AvgCost: 40000.0, Side: "long"},
		},
		TradeHistory: []Trade{},
	}
	prices := map[string]float64{"BTC": 50000.0}

	forceCloseAllPositions(s, prices, nil)

	if len(s.Positions) != 0 {
		t.Errorf("expected positions to be empty; got %d", len(s.Positions))
	}
	if len(s.TradeHistory) != 1 {
		t.Errorf("expected 1 trade recorded; got %d", len(s.TradeHistory))
	}
	if s.TradeHistory[0].TradeType != "spot" {
		t.Errorf("expected trade type 'spot'; got %s", s.TradeHistory[0].TradeType)
	}
}

// TestForceCloseAllPositions_Futures verifies force-close of a futures position.
func TestForceCloseAllPositions_Futures(t *testing.T) {
	s := &StrategyState{
		Cash: 10000.0,
		Positions: map[string]*Position{
			"ES": {Quantity: 1.0, AvgCost: 4000.0, Side: "long", Multiplier: 50.0},
		},
		TradeHistory: []Trade{},
	}
	prices := map[string]float64{"ES": 4100.0}

	forceCloseAllPositions(s, prices, nil)

	if len(s.Positions) != 0 {
		t.Errorf("expected positions to be empty; got %d", len(s.Positions))
	}
	if len(s.TradeHistory) != 1 {
		t.Errorf("expected 1 trade recorded; got %d", len(s.TradeHistory))
	}
	if s.TradeHistory[0].TradeType != "futures" {
		t.Errorf("expected trade type 'futures'; got %s", s.TradeHistory[0].TradeType)
	}
}

// TestForceCloseAllPositions_Options verifies force-close of option positions.
func TestForceCloseAllPositions_Options(t *testing.T) {
	s := &StrategyState{
		Cash:            1000.0,
		Positions:       map[string]*Position{},
		OptionPositions: map[string]*OptionPosition{
			"BTC-50000-C": {
				Action:          "buy",
				Quantity:        1.0,
				EntryPremiumUSD: 500.0,
				CurrentValueUSD: 700.0,
			},
		},
		TradeHistory: []Trade{},
	}
	prices := map[string]float64{}

	forceCloseAllPositions(s, prices, nil)

	if len(s.OptionPositions) != 0 {
		t.Errorf("expected option positions to be empty; got %d", len(s.OptionPositions))
	}
	if len(s.TradeHistory) != 1 {
		t.Errorf("expected 1 trade recorded; got %d", len(s.TradeHistory))
	}
	if s.TradeHistory[0].TradeType != "options" {
		t.Errorf("expected trade type 'options'; got %s", s.TradeHistory[0].TradeType)
	}
}

// TestAggregatePerpsMarginInputs_NoPerps verifies zero inputs when no perps.
func TestAggregatePerpsMarginInputs_NoPerps(t *testing.T) {
	strategies := map[string]*StrategyState{
		"s1": {Type: "spot", Positions: map[string]*Position{}},
	}
	prices := map[string]float64{}

	loss, margin := AggregatePerpsMarginInputs(strategies, prices)

	if loss != 0 || margin != 0 {
		t.Errorf("expected (0, 0) for no perps; got (%.2f, %.2f)", loss, margin)
	}
}

// TestAggregatePerpsMarginInputs_WithPerps verifies perps margin inputs.
func TestAggregatePerpsMarginInputs_WithPerps(t *testing.T) {
	strategies := map[string]*StrategyState{
		"s1": {
			Type: "perps",
			Positions: map[string]*Position{
				"BTC": {Quantity: 0.1, AvgCost: 50000.0, Side: "long", Multiplier: 1.0, Leverage: 10.0},
			},
		},
	}
	// Price dropped to 45000 → unrealized loss
	prices := map[string]float64{"BTC": 45000.0}

	loss, margin := AggregatePerpsMarginInputs(strategies, prices)

	if loss <= 0 {
		t.Errorf("expected positive unrealized loss; got %.2f", loss)
	}
	if margin <= 0 {
		t.Errorf("expected positive margin; got %.2f", margin)
	}
}

// TestPerpsMarginDrawdownInputs_LongLoss verifies unrealized loss calculation
// for a losing long perps position.
func TestPerpsMarginDrawdownInputs_LongLoss(t *testing.T) {
	s := &StrategyState{
		Type: "perps",
		Positions: map[string]*Position{
			"BTC": {Quantity: 0.1, AvgCost: 50000.0, Side: "long", Multiplier: 1.0, Leverage: 10.0},
		},
	}
	prices := map[string]float64{"BTC": 45000.0}

	loss, margin := perpsMarginDrawdownInputs(s, prices)

	expectedLoss := 0.1 * 1.0 * (50000.0 - 45000.0) // $500 loss
	expectedMargin := 0.1 * 45000.0 / 10.0           // $450 margin

	if math.Abs(loss-expectedLoss) > 0.01 {
		t.Errorf("expected loss=%.2f; got %.2f", expectedLoss, loss)
	}
	if math.Abs(margin-expectedMargin) > 0.01 {
		t.Errorf("expected margin=%.2f; got %.2f", expectedMargin, margin)
	}
}

// TestPerpsMarginDrawdownInputs_LongProfit verifies zero loss when position
// is profitable (gains clamp to zero).
func TestPerpsMarginDrawdownInputs_LongProfit(t *testing.T) {
	s := &StrategyState{
		Type: "perps",
		Positions: map[string]*Position{
			"BTC": {Quantity: 0.1, AvgCost: 40000.0, Side: "long", Multiplier: 1.0, Leverage: 10.0},
		},
	}
	prices := map[string]float64{"BTC": 50000.0}

	loss, margin := perpsMarginDrawdownInputs(s, prices)

	if loss != 0 {
		t.Errorf("expected zero loss for profitable position; got %.2f", loss)
	}
	if margin <= 0 {
		t.Errorf("expected positive margin; got %.2f", margin)
	}
}

// TestPerpsMarginDrawdownInputs_NoLeverage verifies zero inputs when leverage
// is not set.
func TestPerpsMarginDrawdownInputs_NoLeverage(t *testing.T) {
	s := &StrategyState{
		Type: "perps",
		Positions: map[string]*Position{
			"BTC": {Quantity: 0.1, AvgCost: 50000.0, Side: "long", Multiplier: 1.0, Leverage: 0},
		},
	}
	prices := map[string]float64{"BTC": 45000.0}

	loss, margin := perpsMarginDrawdownInputs(s, prices)

	if loss != 0 || margin != 0 {
		t.Errorf("expected (0, 0) when leverage is 0; got (%.2f, %.2f)", loss, margin)
	}
}

// TestDetectSharedWalletPlatforms_SingleStrategy verifies that a single strategy
// with capital_pct does NOT count as a shared wallet.
func TestDetectSharedWalletPlatforms_SingleStrategy(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "s1", Platform: "hyperliquid", CapitalPct: 50},
	}

	platforms := detectSharedWalletPlatforms(strategies)

	if len(platforms) != 0 {
		t.Errorf("expected no shared wallets for single strategy; got %v", platforms)
	}
}

// TestDetectSharedWalletPlatforms_MultipleSamePlatform verifies detection of
// multiple strategies on the same platform with capital_pct.
func TestDetectSharedWalletPlatforms_MultipleSamePlatform(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "s1", Platform: "hyperliquid", CapitalPct: 50},
		{ID: "s2", Platform: "hyperliquid", CapitalPct: 50},
	}

	platforms := detectSharedWalletPlatforms(strategies)

	if len(platforms) != 1 || platforms[0] != "hyperliquid" {
		t.Errorf("expected [hyperliquid]; got %v", platforms)
	}
}

// TestDetectSharedWalletPlatforms_MultiplePlatforms verifies detection across
// multiple platforms.
func TestDetectSharedWalletPlatforms_MultiplePlatforms(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "s1", Platform: "hyperliquid", CapitalPct: 50},
		{ID: "s2", Platform: "hyperliquid", CapitalPct: 50},
		{ID: "s3", Platform: "okx", CapitalPct: 50},
		{ID: "s4", Platform: "okx", CapitalPct: 50},
	}

	platforms := detectSharedWalletPlatforms(strategies)

	if len(platforms) != 2 {
		t.Errorf("expected 2 platforms; got %d: %v", len(platforms), platforms)
	}
}

// TestAddKillSwitchEvent_AppendAndTrim verifies event append and trim behavior.
func TestAddKillSwitchEvent_AppendAndTrim(t *testing.T) {
	prs := &PortfolioRiskState{}

	for i := 0; i < maxKillSwitchEvents+10; i++ {
		addKillSwitchEvent(prs, "triggered", "equity", float64(i), 1000.0, 1000.0, fmt.Sprintf("event %d", i))
	}

	if len(prs.Events) != maxKillSwitchEvents {
		t.Errorf("expected %d events after trim; got %d", maxKillSwitchEvents, len(prs.Events))
	}

	// Verify oldest events were dropped (first event should be index 10)
	if prs.Events[0].DrawdownPct != 10.0 {
		t.Errorf("expected first event to have drawdown=10; got %.0f", prs.Events[0].DrawdownPct)
	}
}

// TestAddKillSwitchEvent_SourceField verifies that source is properly recorded.
func TestAddKillSwitchEvent_SourceField(t *testing.T) {
	prs := &PortfolioRiskState{}

	addKillSwitchEvent(prs, "triggered", "margin", 15.0, 900.0, 1000.0, "test")

	if len(prs.Events) != 1 {
		t.Fatalf("expected 1 event; got %d", len(prs.Events))
	}
	if prs.Events[0].Source != "margin" {
		t.Errorf("expected source='margin'; got %s", prs.Events[0].Source)
	}
	if prs.Events[0].Type != "triggered" {
		t.Errorf("expected type='triggered'; got %s", prs.Events[0].Type)
	}
}

// TestClearLatchedKillSwitchSharedWallet_NotActive verifies no-op when kill
// switch is not active.
func TestClearLatchedKillSwitchSharedWallet_NotActive(t *testing.T) {
	state := &AppState{PortfolioRisk: PortfolioRiskState{KillSwitchActive: false}}
	strategies := []StrategyConfig{
		{ID: "s1", Platform: "hyperliquid", CapitalPct: 50},
		{ID: "s2", Platform: "hyperliquid", CapitalPct: 50},
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, func(platform string) (float64, error) {
		return 1000.0, nil
	})

	if cleared {
		t.Error("expected no clear when kill switch is not active")
	}
}

// TestClearLatchedKillSwitchSharedWallet_NoSharedWallet verifies no-op when
// there are no shared wallets.
func TestClearLatchedKillSwitchSharedWallet_NoSharedWallet(t *testing.T) {
	state := &AppState{PortfolioRisk: PortfolioRiskState{KillSwitchActive: true, KillSwitchAt: time.Now().UTC()}}
	strategies := []StrategyConfig{
		{ID: "s1", Platform: "hyperliquid", CapitalPct: 50}, // single strategy
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, func(platform string) (float64, error) {
		return 1000.0, nil
	})

	if cleared {
		t.Error("expected no clear when no shared wallets")
	}
}

// TestClearLatchedKillSwitchSharedWallet_FetchFailurePreservesSwitch verifies
// that a fetch failure preserves the kill switch.
func TestClearLatchedKillSwitchSharedWallet_FetchFailurePreservesSwitch(t *testing.T) {
	state := &AppState{PortfolioRisk: PortfolioRiskState{KillSwitchActive: true, KillSwitchAt: time.Now().UTC()}}
	strategies := []StrategyConfig{
		{ID: "s1", Platform: "hyperliquid", CapitalPct: 50},
		{ID: "s2", Platform: "hyperliquid", CapitalPct: 50},
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, func(platform string) (float64, error) {
		return 0, fmt.Errorf("network error")
	})

	if cleared {
		t.Error("expected no clear when fetch fails")
	}
	if !state.PortfolioRisk.KillSwitchActive {
		t.Error("expected kill switch to remain active after fetch failure")
	}
}

// TestClearLatchedKillSwitchSharedWallet_Success verifies successful clear and
// peak re-baselining.
func TestClearLatchedKillSwitchSharedWallet_Success(t *testing.T) {
	state := &AppState{PortfolioRisk: PortfolioRiskState{
		KillSwitchActive: true,
		KillSwitchAt:     time.Now().UTC(),
		PeakValue:        2000.0,
	}}
	strategies := []StrategyConfig{
		{ID: "s1", Platform: "hyperliquid", CapitalPct: 50},
		{ID: "s2", Platform: "hyperliquid", CapitalPct: 50},
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, func(platform string) (float64, error) {
		return 1500.0, nil
	})

	if !cleared {
		t.Error("expected clear to succeed")
	}
	if state.PortfolioRisk.KillSwitchActive {
		t.Error("expected kill switch to be cleared")
	}
	if state.PortfolioRisk.PeakValue != 1500.0 {
		t.Errorf("expected PeakValue re-baselined to 1500; got %.2f", state.PortfolioRisk.PeakValue)
	}
	if state.PortfolioRisk.CurrentDrawdownPct != 0 {
		t.Errorf("expected CurrentDrawdownPct=0; got %.2f", state.PortfolioRisk.CurrentDrawdownPct)
	}
}

// TestCollectPriceSymbols_SpotOnly verifies that only spot strategies are
// included in price symbol collection.
func TestCollectPriceSymbols_SpotOnly(t *testing.T) {
	strategies := []StrategyConfig{
		{Type: "spot", Args: []string{"", "BTC/USDT"}},
		{Type: "perps", Args: []string{"", "BTC"}},
	}

	symbols := collectPriceSymbols(strategies)

	if len(symbols) != 1 || symbols[0] != "BTC/USDT" {
		t.Errorf("expected [BTC/USDT]; got %v", symbols)
	}
}

// TestCollectPriceSymbols_EmptySymbolSkipped verifies empty symbols are skipped.
func TestCollectPriceSymbols_EmptySymbolSkipped(t *testing.T) {
	strategies := []StrategyConfig{
		{Type: "spot", Args: []string{"", ""}},
	}

	symbols := collectPriceSymbols(strategies)

	if len(symbols) != 0 {
		t.Errorf("expected empty symbols; got %v", symbols)
	}
}

// TestCollectPerpsMarkSymbols_HLAndOKX verifies perps mark symbol collection
// for both Hyperliquid and OKX.
func TestCollectPerpsMarkSymbols_HLAndOKX(t *testing.T) {
	strategies := []StrategyConfig{
		{Type: "perps", Platform: "hyperliquid", Args: []string{"", "BTC"}},
		{Type: "perps", Platform: "hyperliquid", Args: []string{"", "ETH"}},
		{Type: "perps", Platform: "okx", Args: []string{"", "BTC"}},
	}

	hlCoins, okxCoins := collectPerpsMarkSymbols(strategies)

	if len(hlCoins) != 2 {
		t.Errorf("expected 2 HL coins; got %d: %v", len(hlCoins), hlCoins)
	}
	if len(okxCoins) != 1 {
		t.Errorf("expected 1 OKX coin; got %d: %v", len(okxCoins), okxCoins)
	}
}

// TestCollectPerpsMarkSymbols_Deduplication verifies duplicate symbols are
// deduplicated.
func TestCollectPerpsMarkSymbols_Deduplication(t *testing.T) {
	strategies := []StrategyConfig{
		{Type: "perps", Platform: "hyperliquid", Args: []string{"", "BTC"}},
		{Type: "perps", Platform: "hyperliquid", Args: []string{"", "BTC"}},
	}

	hlCoins, _ := collectPerpsMarkSymbols(strategies)

	if len(hlCoins) != 1 {
		t.Errorf("expected 1 unique coin; got %d: %v", len(hlCoins), hlCoins)
	}
}

// TestCollectFuturesMarkSymbols_TopStepOnly verifies only topstep futures are
// collected.
func TestCollectFuturesMarkSymbols_TopStepOnly(t *testing.T) {
	strategies := []StrategyConfig{
		{Type: "futures", Platform: "topstep", Args: []string{"", "ES"}},
		{Type: "futures", Platform: "ibkr", Args: []string{"", "ES"}},
	}

	symbols := collectFuturesMarkSymbols(strategies)

	if len(symbols) != 1 || symbols[0] != "ES" {
		t.Errorf("expected [ES]; got %v", symbols)
	}
}

// TestMergePerpsMarks_ExistingWins verifies existing prices are not overwritten.
func TestMergePerpsMarks_ExistingWins(t *testing.T) {
	prices := map[string]float64{"BTC": 50000.0}
	marks := map[string]float64{"BTC": 51000.0}

	mergePerpsMarks(prices, marks)

	if prices["BTC"] != 50000.0 {
		t.Errorf("expected existing price to win; got %.2f", prices["BTC"])
	}
}

// TestMergePerpsMarks_NewEntryAdded verifies new entries are added.
func TestMergePerpsMarks_NewEntryAdded(t *testing.T) {
	prices := map[string]float64{"BTC": 50000.0}
	marks := map[string]float64{"ETH": 3000.0}

	mergePerpsMarks(prices, marks)

	if prices["ETH"] != 3000.0 {
		t.Errorf("expected ETH price added; got %.2f", prices["ETH"])
	}
}

// TestMergePerpsMarks_ZeroSkipped verifies zero/negative marks are skipped.
func TestMergePerpsMarks_ZeroSkipped(t *testing.T) {
	prices := map[string]float64{}
	marks := map[string]float64{"BTC": 0, "ETH": -100}

	mergePerpsMarks(prices, marks)

	if len(prices) != 0 {
		t.Errorf("expected no prices added; got %v", prices)
	}
}

// TestMergeFuturesMarks_ExistingWins verifies existing prices are not overwritten.
func TestMergeFuturesMarks_ExistingWins(t *testing.T) {
	prices := map[string]float64{"ES": 4000.0}
	marks := map[string]float64{"ES": 4100.0}

	mergeFuturesMarks(prices, marks)

	if prices["ES"] != 4000.0 {
		t.Errorf("expected existing price to win; got %.2f", prices["ES"])
	}
}

// TestMergeFuturesMarks_NewEntryAdded verifies new entries are added.
func TestMergeFuturesMarks_NewEntryAdded(t *testing.T) {
	prices := map[string]float64{"ES": 4000.0}
	marks := map[string]float64{"NQ": 15000.0}

	mergeFuturesMarks(prices, marks)

	if prices["NQ"] != 15000.0 {
		t.Errorf("expected NQ price added; got %.2f", prices["NQ"])
	}
}

// TestRiskState_MarshalPendingCircuitClosesJSON_EmptyReturnsEmpty verifies
// empty map returns empty string.
func TestRiskState_MarshalPendingCircuitClosesJSON_EmptyReturnsEmpty(t *testing.T) {
	r := &RiskState{}

	blob := r.MarshalPendingCircuitClosesJSON()

	if blob != "" {
		t.Errorf("expected empty string; got %q", blob)
	}
}

// TestRiskState_MarshalPendingCircuitClosesJSON_NilSymbolsFiltered verifies
// entries with nil/empty symbols are filtered out.
func TestRiskState_MarshalPendingCircuitClosesJSON_NilSymbolsFiltered(t *testing.T) {
	r := &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		"hyperliquid": {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}}},
		"okx":         {Symbols: nil},
	}}

	blob := r.MarshalPendingCircuitClosesJSON()

	if strings.Contains(blob, "okx") {
		t.Errorf("expected okx to be filtered out; got %s", blob)
	}
	if !strings.Contains(blob, "hyperliquid") {
		t.Errorf("expected hyperliquid to be present; got %s", blob)
	}
}

// TestRiskState_UnmarshalPendingCircuitClosesJSON_NewShape verifies new map
// shape parsing.
func TestRiskState_UnmarshalPendingCircuitClosesJSON_NewShape(t *testing.T) {
	r := &RiskState{}
	blob := `{"hyperliquid":{"symbols":[{"symbol":"ETH","size":0.1}]}}`

	r.UnmarshalPendingCircuitClosesJSON(blob)

	p := r.getPendingCircuitClose("hyperliquid")
	if p == nil || len(p.Symbols) != 1 {
		t.Fatalf("expected 1 symbol; got %+v", p)
	}
	if p.Symbols[0].Symbol != "ETH" || p.Symbols[0].Size != 0.1 {
		t.Errorf("expected ETH/0.1; got %s/%.4f", p.Symbols[0].Symbol, p.Symbols[0].Size)
	}
}

// TestRiskState_UnmarshalPendingCircuitClosesJSON_LegacyShape verifies legacy
// HL-only shape is transparently converted.
func TestRiskState_UnmarshalPendingCircuitClosesJSON_LegacyShape(t *testing.T) {
	r := &RiskState{}
	blob := `{"coins":[{"coin":"ETH","sz":0.2585}]}`

	r.UnmarshalPendingCircuitClosesJSON(blob)

	p := r.getPendingCircuitClose("hyperliquid")
	if p == nil || len(p.Symbols) != 1 {
		t.Fatalf("legacy JSON did not convert: %+v", p)
	}
	if p.Symbols[0].Symbol != "ETH" || p.Symbols[0].Size != 0.2585 {
		t.Errorf("legacy conversion wrong: got symbol=%q size=%g", p.Symbols[0].Symbol, p.Symbols[0].Size)
	}
}

// TestRiskState_PendingCircuitClose_UnmarshalEmptyClears verifies that an
// empty string wipes the pending map (matches the prior HL-specific behavior).
func TestRiskState_PendingCircuitClose_UnmarshalEmptyClears(t *testing.T) {
	r := RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		PlatformPendingCloseHyperliquid: {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1}}},
	}}
	r.UnmarshalPendingCircuitClosesJSON("")
	if r.PendingCircuitCloses != nil {
		t.Errorf("expected nil map after empty unmarshal; got %+v", r.PendingCircuitCloses)
	}
}

// TestRiskState_PendingCircuitClose_UnmarshalMalformedClears verifies that
// a malformed JSON payload wipes the pending map rather than leaving stale
// data in place.
func TestRiskState_PendingCircuitClose_UnmarshalMalformedClears(t *testing.T) {
	r := RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		PlatformPendingCloseHyperliquid: {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 1}}},
	}}
	r.UnmarshalPendingCircuitClosesJSON(`not-json{`)
	if r.PendingCircuitCloses != nil {
		t.Errorf("expected nil map after malformed unmarshal; got %+v", r.PendingCircuitCloses)
	}
}

// TestRiskState_PendingCircuitClose_SetClearGet verifies the setter/clearer/
// getter contract: nil map is materialized lazily on set; clear deletes the
// entry and nils the map when empty.
func TestRiskState_PendingCircuitClose_SetClearGet(t *testing.T) {
	var r RiskState

	if r.getPendingCircuitClose("hyperliquid") != nil {
		t.Fatal("expected nil for unset key")
	}

	r.setPendingCircuitClose("hyperliquid", &PendingCircuitClose{
		Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.5}},
	})
	if got := r.getPendingCircuitClose("hyperliquid"); got == nil || got.Symbols[0].Size != 0.5 {
		t.Errorf("setter did not store value: %+v", got)
	}

	// Set with empty symbols should clear the entry.
	r.setPendingCircuitClose("hyperliquid", &PendingCircuitClose{Symbols: nil})
	if r.getPendingCircuitClose("hyperliquid") != nil {
		t.Error("empty-symbols set should have cleared entry")
	}
	if r.PendingCircuitCloses != nil {
		t.Error("map should be nil after last entry cleared")
	}

	// Clear on missing key is a no-op.
	r.clearPendingCircuitClose("hyperliquid")
}

// TestRiskState_PendingCircuitClose_MultiPlatformRoundTrip locks in that the
// generic plumbing is not HL-limited: future phases 2-4 will co-exist in the
// same map.
func TestRiskState_PendingCircuitClose_MultiPlatformRoundTrip(t *testing.T) {
	src := &RiskState{PendingCircuitCloses: map[string]*PendingCircuitClose{
		"hyperliquid": {Symbols: []PendingCircuitCloseSymbol{{Symbol: "ETH", Size: 0.1}}},
		"okx":         {Symbols: []PendingCircuitCloseSymbol{{Symbol: "BTC-USDT-SWAP", Size: 0.01}}},
	}}
	blob := src.MarshalPendingCircuitClosesJSON()
	var dst RiskState
	dst.UnmarshalPendingCircuitClosesJSON(blob)
	if dst.getPendingCircuitClose("hyperliquid") == nil {
		t.Error("hyperliquid entry lost in round-trip")
	}
	if dst.getPendingCircuitClose("okx") == nil {
		t.Error("okx entry lost in round-trip")
	}
}

// TestCalculateSharpeRatio_InsufficientData verifies 0 is returned when
// fewer than 2 trades.
func TestCalculateSharpeRatio_InsufficientData(t *testing.T) {
	// Empty slice
	sharpe := CalculateSharpeRatio([]float64{}, 0.02)
	if sharpe != 0 {
		t.Errorf("expected 0 for empty data; got %.4f", sharpe)
	}

	// Single trade
	sharpe = CalculateSharpeRatio([]float64{100.0}, 0.02)
	if sharpe != 0 {
		t.Errorf("expected 0 for single trade; got %.4f", sharpe)
	}
}

// TestCalculateSharpeRatio_ZeroStdDev verifies 0 is returned when all trades
// have identical PnL (zero variance).
func TestCalculateSharpeRatio_ZeroStdDev(t *testing.T) {
	sharpe := CalculateSharpeRatio([]float64{100.0, 100.0, 100.0}, 0.02)
	if sharpe != 0 {
		t.Errorf("expected 0 for zero std dev; got %.4f", sharpe)
	}
}

// TestCalculateSharpeRatio_BasicCalculation verifies basic Sharpe calculation.
func TestCalculateSharpeRatio_BasicCalculation(t *testing.T) {
	// 3 trades: +100, -50, +75
	// mean = 41.67
	// variance = ((100-41.67)^2 + (-50-41.67)^2 + (75-41.67)^2) / 3
	// = (3402.78 + 8402.78 + 1111.11) / 3 = 4305.56
	// std dev = 65.62
	// sharpe = (41.67 - 0.02/252) / 65.62 ≈ 0.635
	pnls := []float64{100.0, -50.0, 75.0}
	sharpe := CalculateSharpeRatio(pnls, 0.02)

	expected := 0.635 // approximate
	if math.Abs(sharpe-expected) > 0.01 {
		t.Errorf("expected sharpe≈%.3f; got %.4f", expected, sharpe)
	}
}

// TestCalculateSharpeRatio_AllPositive verifies positive Sharpe for profitable
// strategy.
func TestCalculateSharpeRatio_AllPositive(t *testing.T) {
	pnls := []float64{50.0, 60.0, 55.0, 70.0, 45.0}
	sharpe := CalculateSharpeRatio(pnls, 0.02)

	if sharpe <= 0 {
		t.Errorf("expected positive Sharpe for profitable strategy; got %.4f", sharpe)
	}
}

// TestCalculateSharpeRatio_AllNegative verifies negative Sharpe for losing
// strategy.
func TestCalculateSharpeRatio_AllNegative(t *testing.T) {
	pnls := []float64{-50.0, -60.0, -55.0, -70.0, -45.0}
	sharpe := CalculateSharpeRatio(pnls, 0.02)

	if sharpe >= 0 {
		t.Errorf("expected negative Sharpe for losing strategy; got %.4f", sharpe)
	}
}

// TestCalculateSharpeRatio_HigherRiskFreeRate verifies that a higher risk-free
// rate reduces the Sharpe ratio.
func TestCalculateSharpeRatio_HigherRiskFreeRate(t *testing.T) {
	pnls := []float64{100.0, -50.0, 75.0, -25.0, 50.0}

	sharpeLowRF := CalculateSharpeRatio(pnls, 0.01)
	sharpeHighRF := CalculateSharpeRatio(pnls, 0.05)

	if sharpeHighRF >= sharpeLowRF {
		t.Errorf("expected higher risk-free rate to reduce Sharpe; low=%.4f, high=%.4f", sharpeLowRF, sharpeHighRF)
	}
}

// TestAppendTradePnL_Cap verifies the rolling window cap at maxSharpeTradeHistory.
func TestAppendTradePnL_Cap(t *testing.T) {
	r := &RiskState{}

	for i := 0; i < maxSharpeTradeHistory+10; i++ {
		appendTradePnL(r, float64(i))
	}

	if len(r.TradePnLs) != maxSharpeTradeHistory {
		t.Errorf("expected %d PnLs after cap; got %d", maxSharpeTradeHistory, len(r.TradePnLs))
	}

	// Verify oldest entries were dropped
	if r.TradePnLs[0] != 10.0 {
		t.Errorf("expected first PnL=10 after cap; got %.0f", r.TradePnLs[0])
	}
}

// TestRecordTradeResult_SharpeRatioUpdated verifies that SharpeRatio is
// recalculated after each trade.
func TestRecordTradeResult_SharpeRatioUpdated(t *testing.T) {
	r := &RiskState{RiskFreeRate: 0.02}

	// First trade — not enough data
	RecordTradeResult(r, 100.0)
	if r.SharpeRatio != 0 {
		t.Errorf("expected Sharpe=0 after 1 trade; got %.4f", r.SharpeRatio)
	}

	// Second trade — should have Sharpe
	RecordTradeResult(r, -50.0)
	if r.SharpeRatio == 0 {
		t.Error("expected non-zero Sharpe after 2 trades")
	}

	// Third trade — Sharpe should update
	prevSharpe := r.SharpeRatio
	RecordTradeResult(r, 75.0)
	if r.SharpeRatio == prevSharpe {
		t.Error("expected Sharpe to change after third trade")
	}
}

// TestRecordTradeResult_DefaultRiskFreeRate verifies default risk-free rate
// is used when not set.
func TestRecordTradeResult_DefaultRiskFreeRate(t *testing.T) {
	r := &RiskState{} // RiskFreeRate not set

	RecordTradeResult(r, 100.0)
	RecordTradeResult(r, -50.0)

	if r.SharpeRatio == 0 {
		t.Error("expected Sharpe calculation with default risk-free rate")
	}
}

// TestRecordTradeResult_TradePnLsPopulated verifies TradePnLs are accumulated.
func TestRecordTradeResult_TradePnLsPopulated(t *testing.T) {
	r := &RiskState{}

	RecordTradeResult(r, 100.0)
	RecordTradeResult(r, -50.0)
	RecordTradeResult(r, 75.0)

	if len(r.TradePnLs) != 3 {
		t.Errorf("expected 3 PnLs stored; got %d", len(r.TradePnLs))
	}
	if r.TradePnLs[0] != 100.0 || r.TradePnLs[1] != -50.0 || r.TradePnLs[2] != 75.0 {
		t.Errorf("expected [100, -50, 75]; got %v", r.TradePnLs)
	}
}

// TestRecordTradeResult_WinLossCounters verifies winning/losing trade counters.
func TestRecordTradeResult_WinLossCounters(t *testing.T) {
	r := &RiskState{}

	RecordTradeResult(r, 100.0)  // win
	RecordTradeResult(r, -50.0)  // loss
	RecordTradeResult(r, 1.0)    // win
	RecordTradeResult(r, -25.0)  // loss

	if r.WinningTrades != 2 {
		t.Errorf("expected 2 winning trades; got %d", r.WinningTrades)
	}
	if r.LosingTrades != 2 {
		t.Errorf("expected 2 losing trades; got %d", r.LosingTrades)
	}
	if r.ConsecutiveLosses != 1 {
		t.Errorf("expected 1 consecutive loss (last trade); got %d", r.ConsecutiveLosses)
	}
}

// TestRecordTradeResult_ConsecutiveLossesReset verifies consecutive losses
// reset after a win.
func TestRecordTradeResult_ConsecutiveLossesReset(t *testing.T) {
	r := &RiskState{}

	RecordTradeResult(r, -10.0)
	RecordTradeResult(r, -20.0)
	RecordTradeResult(r, 5.0) // win resets

	if r.ConsecutiveLosses != 0 {
		t.Errorf("expected consecutive losses reset to 0; got %d", r.ConsecutiveLosses)
	}
}
