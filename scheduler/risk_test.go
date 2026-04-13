package main

import (
	"fmt"
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
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 0, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Just under threshold — should be allowed.
	allowed, nb, _, reason := CheckPortfolioRisk(prs, cfg, 7600.0, 0)
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
	allowed, nb, _, reason = CheckPortfolioRisk(prs, cfg, 7400.0, 0)
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
	allowed, _, _, _ = CheckPortfolioRisk(prs, cfg, 10000.0, 0)
	if allowed {
		t.Error("expected kill switch to remain latched on subsequent call")
	}
}

// TestCheckPortfolioRisk_NotionalCap verifies the notional cap blocks new trades
// without triggering the kill switch.
func TestCheckPortfolioRisk_NotionalCap(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, MaxNotionalUSD: 50000, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Under cap — allowed, not notional-blocked.
	allowed, nb, _, _ := CheckPortfolioRisk(prs, cfg, 10000.0, 30000.0)
	if !allowed {
		t.Error("expected allowed under notional cap")
	}
	if nb {
		t.Error("expected notionalBlocked=false under cap")
	}

	// Over cap — allowed=true, notionalBlocked=true, kill switch NOT active.
	allowed, nb, _, reason := CheckPortfolioRisk(prs, cfg, 10000.0, 60000.0)
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
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 50, MaxNotionalUSD: 0, WarnThresholdPct: 80}
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

// TestCheckPortfolioRisk_WarningFires verifies that drawdown at 80% of limit
// triggers a warning once but not again on second call.
func TestCheckPortfolioRisk_WarningFires(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Warn threshold = 25 * 80/100 = 20%. Drawdown = (10000-7900)/10000 = 21% > 20%.
	_, _, warning, reason := CheckPortfolioRisk(prs, cfg, 7900.0, 0)
	if !warning {
		t.Error("expected warning=true at 21% drawdown (warn threshold=20%)")
	}
	if reason == "" {
		t.Error("expected non-empty reason for warning")
	}
	if !prs.WarningSent {
		t.Error("expected WarningSent=true after warning fires")
	}

	// Second call at same drawdown — warning should NOT fire again.
	_, _, warning, _ = CheckPortfolioRisk(prs, cfg, 7900.0, 0)
	if warning {
		t.Error("expected warning=false on second call (already sent)")
	}
}

// TestCheckPortfolioRisk_WarningResetOnRecovery verifies that recovery below
// the warning threshold resets WarningSent so it can fire again.
func TestCheckPortfolioRisk_WarningResetOnRecovery(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// Trigger warning at 21% drawdown.
	CheckPortfolioRisk(prs, cfg, 7900.0, 0)
	if !prs.WarningSent {
		t.Fatal("expected WarningSent=true after first warning")
	}

	// Recover to 15% drawdown (below 20% warn threshold).
	CheckPortfolioRisk(prs, cfg, 8500.0, 0)
	if prs.WarningSent {
		t.Error("expected WarningSent=false after recovery below warn threshold")
	}

	// Cross warning threshold again — should warn again.
	_, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7900.0, 0)
	if !warning {
		t.Error("expected warning=true after recovery and re-crossing threshold")
	}
}

// TestCheckPortfolioRisk_WarningNotAfterKillSwitch verifies that past the kill
// threshold the kill switch fires and no warning is returned.
func TestCheckPortfolioRisk_WarningNotAfterKillSwitch(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	// 26% drawdown > 25% kill switch threshold.
	allowed, _, warning, _ := CheckPortfolioRisk(prs, cfg, 7400.0, 0)
	if allowed {
		t.Error("expected kill switch to fire")
	}
	if warning {
		t.Error("expected warning=false when kill switch fires (kill takes precedence)")
	}
}

// TestAddKillSwitchEvent_MaxCap verifies that events are capped at maxKillSwitchEvents.
func TestAddKillSwitchEvent_MaxCap(t *testing.T) {
	prs := &PortfolioRiskState{}

	for i := 0; i < 60; i++ {
		addKillSwitchEvent(prs, "warning", float64(i), 1000, 2000, "test")
	}

	if len(prs.Events) != maxKillSwitchEvents {
		t.Errorf("expected %d events; got %d", maxKillSwitchEvents, len(prs.Events))
	}
	// Oldest event should be the 11th one added (index 10).
	if prs.Events[0].DrawdownPct != 10 {
		t.Errorf("expected oldest event drawdown=10; got %.0f", prs.Events[0].DrawdownPct)
	}
}

// TestCheckPortfolioRisk_EventLoggedOnTrigger verifies that a "triggered" event
// is appended when the kill switch fires.
func TestCheckPortfolioRisk_EventLoggedOnTrigger(t *testing.T) {
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	prs := &PortfolioRiskState{PeakValue: 10000.0}

	CheckPortfolioRisk(prs, cfg, 7400.0, 0)

	if len(prs.Events) != 1 {
		t.Fatalf("expected 1 event; got %d", len(prs.Events))
	}
	if prs.Events[0].Type != "triggered" {
		t.Errorf("expected event type='triggered'; got %q", prs.Events[0].Type)
	}
	if prs.Events[0].PortfolioValue != 7400.0 {
		t.Errorf("expected portfolio_value=7400; got %.2f", prs.Events[0].PortfolioValue)
	}
}

// --- ClearLatchedKillSwitchSharedWallet (#244) ---

// latchedSharedWalletState builds an AppState with a latched kill switch and
// shared-wallet strategies for use in #244 regression tests.
func latchedSharedWalletState() *AppState {
	return &AppState{
		Strategies: map[string]*StrategyState{},
		PortfolioRisk: PortfolioRiskState{
			PeakValue:          10000,
			CurrentDrawdownPct: 50,
			KillSwitchActive:   true,
			KillSwitchAt:       time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		},
	}
}

func sharedHLStrategies() []StrategyConfig {
	return []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "hl-b", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
	}
}

// TestClearLatchedKillSwitchSharedWallet_Success verifies the kill switch is
// cleared when a shared wallet's real balance is fetched successfully, and
// that PeakValue is re-baselined so the next CheckPortfolioRisk call does
// not immediately re-latch the switch (#244 regression).
func TestClearLatchedKillSwitchSharedWallet_Success(t *testing.T) {
	state := latchedSharedWalletState()
	strategies := sharedHLStrategies()

	calls := 0
	fetcher := func(platform string) (float64, error) {
		calls++
		if platform != "hyperliquid" {
			t.Errorf("expected fetcher called for hyperliquid; got %q", platform)
		}
		return 4500, nil
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
	if !cleared {
		t.Fatal("expected ClearLatchedKillSwitchSharedWallet to return true")
	}
	if calls != 1 {
		t.Errorf("expected 1 fetcher call; got %d", calls)
	}
	if state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive=false after clear")
	}
	if !state.PortfolioRisk.KillSwitchAt.IsZero() {
		t.Errorf("expected KillSwitchAt zeroed; got %v", state.PortfolioRisk.KillSwitchAt)
	}
	if state.PortfolioRisk.WarningSent {
		t.Error("expected WarningSent reset to false")
	}
	// Peak should be re-baselined from the fetched balance (was 10000, now 4500).
	if state.PortfolioRisk.PeakValue != 4500 {
		t.Errorf("expected PeakValue re-baselined to 4500; got %.2f", state.PortfolioRisk.PeakValue)
	}
	if state.PortfolioRisk.CurrentDrawdownPct != 0 {
		t.Errorf("expected CurrentDrawdownPct reset to 0; got %.2f", state.PortfolioRisk.CurrentDrawdownPct)
	}
	if len(state.PortfolioRisk.Events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(state.PortfolioRisk.Events))
	}
	evt := state.PortfolioRisk.Events[0]
	if evt.Type != "auto_reset" {
		t.Errorf("expected event type=auto_reset; got %q", evt.Type)
	}
	if evt.PortfolioValue != 4500 {
		t.Errorf("expected event portfolio_value=4500 (fetched balance); got %.2f", evt.PortfolioValue)
	}
	if evt.PeakValue != 4500 {
		t.Errorf("expected event peak_value=4500 (re-baselined); got %.2f", evt.PeakValue)
	}
}

// TestClearLatchedKillSwitchSharedWallet_NoRelatchOnNextTick is the core
// #244 regression test: after an auto-clear, the very next CheckPortfolioRisk
// call must NOT re-latch the kill switch using the stale inflated PeakValue.
// This reproduces the exact scenario from the issue — a $20K peak from
// shared-wallet double-counting against a real $5K balance.
func TestClearLatchedKillSwitchSharedWallet_NoRelatchOnNextTick(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{},
		PortfolioRisk: PortfolioRiskState{
			PeakValue:          20000, // inflated (double-counted)
			CurrentDrawdownPct: 75,
			KillSwitchActive:   true,
			KillSwitchAt:       time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
		},
	}
	strategies := sharedHLStrategies()

	// Real balance is $5K — well below the stale $20K peak.
	fetcher := func(platform string) (float64, error) {
		return 5000, nil
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); !cleared {
		t.Fatal("expected auto-clear to succeed")
	}

	// First tick after restart: CheckPortfolioRisk with real balance ~= $5K.
	// With a properly re-baselined peak, drawdown is 0% and the kill switch
	// stays cleared. With the old buggy behavior (peak still $20K), drawdown
	// would be 75% and the kill switch would re-latch immediately.
	cfg := &PortfolioRiskConfig{MaxDrawdownPct: 25, WarnThresholdPct: 80}
	allowed, _, _, reason := CheckPortfolioRisk(&state.PortfolioRisk, cfg, 5000, 0)
	if !allowed {
		t.Fatalf("expected kill switch to stay cleared after auto-clear; got reason=%s", reason)
	}
	if state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive=false after first post-clear tick — stale peak re-latched the switch")
	}
}

// TestClearLatchedKillSwitchSharedWallet_FetchFailurePreservesLatch verifies
// that a network/config failure on the balance fetch leaves the kill switch
// latched (acceptance criterion #2).
func TestClearLatchedKillSwitchSharedWallet_FetchFailurePreservesLatch(t *testing.T) {
	state := latchedSharedWalletState()
	strategies := sharedHLStrategies()
	originalLatchedAt := state.PortfolioRisk.KillSwitchAt

	fetcher := func(platform string) (float64, error) {
		return 0, fmt.Errorf("simulated network failure")
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
	if cleared {
		t.Fatal("expected ClearLatchedKillSwitchSharedWallet to return false on fetch failure")
	}
	if !state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive to remain true after fetch failure")
	}
	if !state.PortfolioRisk.KillSwitchAt.Equal(originalLatchedAt) {
		t.Errorf("expected KillSwitchAt unchanged; got %v", state.PortfolioRisk.KillSwitchAt)
	}
	if len(state.PortfolioRisk.Events) != 0 {
		t.Errorf("expected no audit event on failure; got %d", len(state.PortfolioRisk.Events))
	}
}

// TestClearLatchedKillSwitchSharedWallet_NoSharedWalletNoOp verifies that
// non-shared-wallet setups are unaffected (acceptance criterion #3).
func TestClearLatchedKillSwitchSharedWallet_NoSharedWalletNoOp(t *testing.T) {
	state := latchedSharedWalletState()
	// Strategies without capital_pct (or only one strategy on a wallet) are
	// not "shared" — there is no double-counting risk to recover from.
	strategies := []StrategyConfig{
		{ID: "spot-a", Platform: "binanceus", Capital: 1000},
		{ID: "spot-b", Platform: "binanceus", Capital: 1000},
		{ID: "hl-solo", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
	}

	calls := 0
	fetcher := func(platform string) (float64, error) {
		calls++
		return 5000, nil
	}

	cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher)
	if cleared {
		t.Error("expected no clear when no shared wallet detected")
	}
	if calls != 0 {
		t.Errorf("expected fetcher NOT called for non-shared wallets; got %d calls", calls)
	}
	if !state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive to remain true")
	}
}

// TestClearLatchedKillSwitchSharedWallet_InactiveSwitchNoOp verifies the
// helper is a no-op (and skips the network fetch entirely) when the kill
// switch is not active.
func TestClearLatchedKillSwitchSharedWallet_InactiveSwitchNoOp(t *testing.T) {
	state := &AppState{
		PortfolioRisk: PortfolioRiskState{PeakValue: 10000, KillSwitchActive: false},
	}
	strategies := sharedHLStrategies()

	calls := 0
	fetcher := func(platform string) (float64, error) {
		calls++
		return 5000, nil
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); cleared {
		t.Error("expected no clear when switch already inactive")
	}
	if calls != 0 {
		t.Errorf("expected fetcher NOT called when switch inactive; got %d calls", calls)
	}
}

// TestClearLatchedKillSwitchSharedWallet_MultiPlatformAllSuccess verifies
// that when multiple shared-wallet platforms are configured, the kill
// switch is cleared and PeakValue is re-baselined to the SUM of all
// fetched balances (not just the first).
func TestClearLatchedKillSwitchSharedWallet_MultiPlatformAllSuccess(t *testing.T) {
	state := latchedSharedWalletState()
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "hl-b", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "okx-a", Platform: "okx", CapitalPct: 0.3, Capital: 300},
		{ID: "okx-b", Platform: "okx", CapitalPct: 0.7, Capital: 700},
	}

	fetcher := func(platform string) (float64, error) {
		switch platform {
		case "hyperliquid":
			return 3000, nil
		case "okx":
			return 2000, nil
		}
		return 0, fmt.Errorf("unexpected platform %q", platform)
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); !cleared {
		t.Fatal("expected kill switch to clear when all platforms fetch successfully")
	}
	if state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive=false")
	}
	// PeakValue must be re-baselined to the SUM (3000 + 2000 = 5000), not
	// just the first platform's balance.
	if state.PortfolioRisk.PeakValue != 5000 {
		t.Errorf("expected PeakValue=5000 (sum of hyperliquid+okx); got %.2f", state.PortfolioRisk.PeakValue)
	}
	if len(state.PortfolioRisk.Events) != 1 {
		t.Fatalf("expected 1 audit event; got %d", len(state.PortfolioRisk.Events))
	}
	if state.PortfolioRisk.Events[0].PortfolioValue != 5000 {
		t.Errorf("expected audit event portfolio_value=5000 (total); got %.2f",
			state.PortfolioRisk.Events[0].PortfolioValue)
	}
}

// TestClearLatchedKillSwitchSharedWallet_MultiPlatformAnyFailPreservesLatch
// verifies that if ANY shared-wallet platform fails to fetch, the kill
// switch is preserved. We require the full portfolio-wide truth before
// re-baselining peak — a partial slice would under-baseline and still be
// unsafe.
func TestClearLatchedKillSwitchSharedWallet_MultiPlatformAnyFailPreservesLatch(t *testing.T) {
	state := latchedSharedWalletState()
	originalLatchedAt := state.PortfolioRisk.KillSwitchAt
	originalPeak := state.PortfolioRisk.PeakValue
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "hl-b", Platform: "hyperliquid", CapitalPct: 0.5, Capital: 1000},
		{ID: "okx-a", Platform: "okx", CapitalPct: 0.3, Capital: 300},
		{ID: "okx-b", Platform: "okx", CapitalPct: 0.7, Capital: 700},
	}

	// hyperliquid fails; okx would succeed — but we should NOT partially
	// clear because the re-baselined peak would miss hyperliquid capital.
	fetcher := func(platform string) (float64, error) {
		if platform == "hyperliquid" {
			return 0, fmt.Errorf("hyperliquid unreachable")
		}
		return 2000, nil
	}

	if cleared := ClearLatchedKillSwitchSharedWallet(state, strategies, fetcher); cleared {
		t.Fatal("expected kill switch to remain latched when any platform fails")
	}
	if !state.PortfolioRisk.KillSwitchActive {
		t.Error("expected KillSwitchActive to remain true")
	}
	if !state.PortfolioRisk.KillSwitchAt.Equal(originalLatchedAt) {
		t.Error("expected KillSwitchAt unchanged")
	}
	if state.PortfolioRisk.PeakValue != originalPeak {
		t.Errorf("expected PeakValue unchanged; got %.2f", state.PortfolioRisk.PeakValue)
	}
	if len(state.PortfolioRisk.Events) != 0 {
		t.Errorf("expected no audit event on partial failure; got %d", len(state.PortfolioRisk.Events))
	}
}

// TestDetectSharedWalletPlatforms verifies the shared-wallet detector picks
// out platforms with > 1 capital_pct strategy and ignores everything else.
func TestDetectSharedWalletPlatforms(t *testing.T) {
	strategies := []StrategyConfig{
		{ID: "hl-a", Platform: "hyperliquid", CapitalPct: 0.5},
		{ID: "hl-b", Platform: "hyperliquid", CapitalPct: 0.5},
		{ID: "okx-solo", Platform: "okx", CapitalPct: 0.5},   // only one — not shared
		{ID: "spot-a", Platform: "binanceus", Capital: 1000}, // no capital_pct
		{ID: "spot-b", Platform: "binanceus", Capital: 1000},
	}

	got := detectSharedWalletPlatforms(strategies)
	if len(got) != 1 || got[0] != "hyperliquid" {
		t.Errorf("expected [hyperliquid]; got %v", got)
	}
}
