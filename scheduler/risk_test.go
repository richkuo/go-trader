package main

import (
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

	CheckRisk(s, 1000.0, nil, nil)

	if s.RiskState.DailyPnL != 0 {
		t.Errorf("expected DailyPnL reset to 0 by CheckRisk; got %.2f", s.RiskState.DailyPnL)
	}
	if s.RiskState.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), s.RiskState.DailyPnLDate)
	}
}

// TestCheckRisk_ForceCloseOnDrawdown verifies that positions are liquidated when
// the max drawdown circuit breaker fires.
func TestCheckRisk_ForceCloseOnDrawdown(t *testing.T) {
	s := &StrategyState{
		ID:   "test-strategy",
		Cash: 5000.0,
		RiskState: RiskState{
			PeakValue:      10000.0,
			MaxDrawdownPct: 20.0,
			TotalTrades:    1,
			DailyPnLDate:   todayUTC(),
		},
		InitialCapital: 10000.0,
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000.0, Side: "long"},
		},
		OptionPositions: map[string]*OptionPosition{
			"BTC-call-60000-2026-03-01": {
				ID:              "BTC-call-60000-2026-03-01",
				Action:          "buy",
				Quantity:        1,
				EntryPremiumUSD: 1000.0,
				CurrentValueUSD: 500.0,
			},
			"BTC-put-50000-2026-03-01": {
				ID:              "BTC-put-50000-2026-03-01",
				Action:          "sell",
				Quantity:        1,
				EntryPremiumUSD: 600.0,
				CurrentValueUSD: -800.0,
			},
		},
		TradeHistory: []Trade{},
	}

	// BTC at $45000 → portfolio ≈ $5000 + 0.1*45000 + 500 + (-800) = $5000+4500+500-800 = $9200
	// drawdown = (10000-9200)/10000 = 8% → below 20% threshold
	// We need drawdown > 20%, so use BTC=$30000:
	// portfolio = $5000 + 0.1*30000 + 500 + (-800) = $5000+3000+500-800 = $7700
	// drawdown = (10000-7700)/10000 = 23% > 20% ✓
	prices := map[string]float64{"BTC": 30000.0}
	pv := PortfolioValue(s, prices)

	allowed, reason := CheckRisk(s, pv, prices, nil)

	if allowed {
		t.Error("expected CheckRisk to return false on drawdown breach")
	}
	if len(reason) == 0 {
		t.Error("expected non-empty reason")
	}

	// All positions should be closed
	if len(s.Positions) != 0 {
		t.Errorf("expected Positions empty after force-close; got %d entries", len(s.Positions))
	}
	if len(s.OptionPositions) != 0 {
		t.Errorf("expected OptionPositions empty after force-close; got %d entries", len(s.OptionPositions))
	}

	// 3 trades recorded (1 spot + 2 options)
	if len(s.TradeHistory) != 3 {
		t.Errorf("expected 3 trades in history; got %d", len(s.TradeHistory))
	}

	// RiskState.TotalTrades incremented by 3 (was 1, now 4)
	if s.RiskState.TotalTrades != 4 {
		t.Errorf("expected TotalTrades=4; got %d", s.RiskState.TotalTrades)
	}

	// Cash: started $5000
	// + long BTC close: 0.1 * 30000 = $3000 → pnl = 3000 - 0.1*50000 = -$2000
	// + bought call close: +$500 → pnl = 500 - 1000 = -$500
	// + sold put close: buyback = 800 → cash -= 800 → pnl = 600 - 800 = -$200
	// expected Cash = 5000 + 3000 + 500 - 800 = $7700
	expectedCash := 7700.0
	if s.Cash != expectedCash {
		t.Errorf("expected Cash=%.2f after force-close; got %.2f", expectedCash, s.Cash)
	}
}

// TestCheckRisk_ConsecutiveLossesForceClose verifies that the consecutive-losses
// circuit breaker force-closes all open positions.
func TestCheckRisk_ConsecutiveLossesForceClose(t *testing.T) {
	s := &StrategyState{
		ID:   "test-strategy",
		Cash: 5000.0,
		RiskState: RiskState{
			PeakValue:         10000.0,
			MaxDrawdownPct:    50.0,
			TotalTrades:       5,
			ConsecutiveLosses: 5,
			DailyPnLDate:      todayUTC(),
		},
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 0.1, AvgCost: 50000.0, Side: "long"},
		},
		OptionPositions: make(map[string]*OptionPosition),
		TradeHistory:    []Trade{},
	}

	prices := map[string]float64{"BTC": 50000.0}
	pv := PortfolioValue(s, prices)

	allowed, reason := CheckRisk(s, pv, prices, nil)

	if allowed {
		t.Errorf("expected circuit breaker to fire; reason=%s", reason)
	}

	// Positions must be force-closed
	if len(s.Positions) != 0 {
		t.Errorf("expected Positions empty after force-close; got %d entries", len(s.Positions))
	}
	if len(s.TradeHistory) != 1 {
		t.Errorf("expected 1 trade recorded for force-close; got %d", len(s.TradeHistory))
	}
	// BTC long: proceeds = 0.1 * 50000 = 5000, cash = 5000 + 5000 = 10000
	expectedCash := 10000.0
	if s.Cash != expectedCash {
		t.Errorf("expected Cash=%.2f after force-close; got %.2f", expectedCash, s.Cash)
	}
}
