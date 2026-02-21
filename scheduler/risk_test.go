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

// TestCheckPortfolioRisk_DrawdownKillSwitch verifies the kill switch fires at the
// drawdown threshold and latches on subsequent calls.
func TestCheckPortfolioRisk_DrawdownKillSwitch(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 0}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Just under threshold — should be allowed.
	allowed, nb, reason := CheckPortfolioRisk(prs, cfg, 7600.0, 0)
	if !allowed {
		t.Errorf("expected allowed below threshold; got reason=%s", reason)
	}
	if nb {
		t.Error("expected notionalBlocked=false")
	}

	// Peak should not change (value dropped).
	if prs.PeakValue != 10000.0 {
		t.Errorf("expected peak=10000; got %.2f", prs.PeakValue)
	}

	// Drawdown = (10000-7400)/10000 = 26% > 25% — kill switch fires.
	allowed, nb, reason = CheckPortfolioRisk(prs, cfg, 7400.0, 0)
	if allowed {
		t.Error("expected kill switch to fire at 26% drawdown")
	}
	if nb {
		t.Error("expected notionalBlocked=false when kill switch fires")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
	if !prs.KillSwitchActive {
		t.Error("expected KillSwitchActive=true after firing")
	}
	if prs.KillSwitchAt.IsZero() {
		t.Error("expected KillSwitchAt to be set")
	}

	// Subsequent call — still latched even with recovered value.
	allowed, _, _ = CheckPortfolioRisk(prs, cfg, 10000.0, 0)
	if allowed {
		t.Error("expected kill switch to remain latched on subsequent call")
	}
}

// TestCheckPortfolioRisk_NotionalCap verifies the notional cap blocks new trades
// without triggering the kill switch.
func TestCheckPortfolioRisk_NotionalCap(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 50000}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Under cap — allowed, not notional-blocked.
	allowed, nb, _ := CheckPortfolioRisk(prs, cfg, 10000.0, 30000.0)
	if !allowed {
		t.Error("expected allowed under notional cap")
	}
	if nb {
		t.Error("expected notionalBlocked=false under cap")
	}

	// Over cap — allowed=true, notionalBlocked=true, kill switch NOT active.
	allowed, nb, reason := CheckPortfolioRisk(prs, cfg, 10000.0, 60000.0)
	if !allowed {
		t.Error("expected allowed=true (notional cap doesn't kill switch)")
	}
	if !nb {
		t.Errorf("expected notionalBlocked=true over cap; reason=%s", reason)
	}
	if prs.KillSwitchActive {
		t.Error("expected kill switch NOT fired for notional cap breach")
	}
}

// TestCheckPortfolioRisk_PeakTracking verifies the peak high-water mark only
// ratchets upward, never down.
func TestCheckPortfolioRisk_PeakTracking(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 50, MaxNotionalUSD: 0}
	prs := &PortfolioRiskState{PeakValue: 5000.0}

	// Value rises — peak should update.
	CheckPortfolioRisk(prs, cfg, 8000.0, 0)
	if prs.PeakValue != 8000.0 {
		t.Errorf("expected peak=8000 after rise; got %.2f", prs.PeakValue)
	}

	// Value drops — peak should NOT update.
	CheckPortfolioRisk(prs, cfg, 6000.0, 0)
	if prs.PeakValue != 8000.0 {
		t.Errorf("expected peak=8000 unchanged after drop; got %.2f", prs.PeakValue)
	}

	// Value rises again — peak updates.
	CheckPortfolioRisk(prs, cfg, 9000.0, 0)
	if prs.PeakValue != 9000.0 {
		t.Errorf("expected peak=9000 after new high; got %.2f", prs.PeakValue)
	}

	// Drawdown tracked correctly: (9000-6000)/9000 ≈ 33.3%.
	CheckPortfolioRisk(prs, cfg, 6000.0, 0)
	expectedDD := (9000.0 - 6000.0) / 9000.0 * 100
	if prs.CurrentDrawdownPct < expectedDD-0.01 || prs.CurrentDrawdownPct > expectedDD+0.01 {
		t.Errorf("expected drawdown≈%.2f%%; got %.2f%%", expectedDD, prs.CurrentDrawdownPct)
	}
}

// TestPortfolioNotional verifies notional computation for spot + sold options +
// bought options.
func TestPortfolioNotional(t *testing.T) {
	strategies := map[string]*StrategyState{
		"spot-strat": {
			Positions: map[string]*Position{
				"BTC": {Symbol: "BTC", Quantity: 0.5, AvgCost: 40000.0, Side: "long"},
				"ETH": {Symbol: "ETH", Quantity: 10.0, AvgCost: 3000.0, Side: "long"},
			},
			OptionPositions: make(map[string]*OptionPosition),
		},
		"options-strat": {
			Positions: make(map[string]*Position),
			OptionPositions: map[string]*OptionPosition{
				"BTC-put-40000-sell": {
					Action:          "sell",
					Strike:          40000.0,
					Quantity:        2.0,
					CurrentValueUSD: -500.0,
				},
				"BTC-call-50000-buy": {
					Action:          "buy",
					Strike:          50000.0,
					Quantity:        1.0,
					CurrentValueUSD: 800.0,
				},
			},
		},
	}

	prices := map[string]float64{
		"BTC": 50000.0,
		"ETH": 3500.0,
	}

	notional := PortfolioNotional(strategies, prices)

	// Spot: 0.5*50000 + 10*3500 = 25000 + 35000 = 60000
	// Sold put: 40000 * 2 = 80000
	// Bought call: CurrentValueUSD = 800 (positive)
	// Total = 60000 + 80000 + 800 = 140800
	expected := 140800.0
	if notional < expected-0.01 || notional > expected+0.01 {
		t.Errorf("expected notional=%.2f; got %.2f", expected, notional)
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
