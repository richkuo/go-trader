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
		RiskState: newRiskState(yesterday(), 500.0),
	}
	s.RiskState.PeakValue = 1000.0
	s.RiskState.MaxDrawdownPct = 50.0

	CheckRisk(s, 1000.0)

	if s.RiskState.DailyPnL != 0 {
		t.Errorf("expected DailyPnL reset to 0 by CheckRisk; got %.2f", s.RiskState.DailyPnL)
	}
	if s.RiskState.DailyPnLDate != todayUTC() {
		t.Errorf("expected DailyPnLDate=%s; got %s", todayUTC(), s.RiskState.DailyPnLDate)
	}
}
