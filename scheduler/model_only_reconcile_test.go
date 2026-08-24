package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #1455 regression tests: a fire-time model-only circuit-breaker close row is
// corrected to the real exchange fill instead of stacking a second, PnL-free
// defensive row. Partial fills reconcile slice-by-slice against the SAME row
// until the persisted basis quantity is fully covered (#1455 review).

func resetModelOnlyReconcileHooks(t *testing.T) {
	t.Helper()
	prevUpdater := modelOnlyCloseUpdater
	prevLoader := modelOnlyCloseBasisLoader
	t.Cleanup(func() {
		modelOnlyCloseUpdater = prevUpdater
		modelOnlyCloseBasisLoader = prevLoader
	})
}

// fireModelOnlyCircuitBreakerClose reproduces the fire-time state a per-position
// circuit breaker leaves behind: CheckRisk -> forceCloseAllPositions books the
// mark-derived estimate, deletes the virtual position, and buffers the
// closed_positions row.
func fireModelOnlyCircuitBreakerClose(t *testing.T) *StrategyState {
	t.Helper()
	s := &StrategyState{
		ID:       "hl-cb-eth",
		Type:     "perps",
		Platform: "hyperliquid",
		Cash:     1000,
		RiskState: RiskState{
			DailyPnLDate: time.Now().UTC().Format("2006-01-02"),
		},
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 2.0, AvgCost: 3000, Side: "long", Multiplier: 1, Leverage: 5},
		},
	}
	forceCloseAllPositions(s, map[string]float64{"ETH": 2800}, nil)
	if len(s.Positions) != 0 {
		t.Fatalf("fire must delete the virtual position, has %v", s.Positions)
	}
	model := findModelOnlyCloseTrade(s, "ETH")
	if model == nil {
		t.Fatal("expected an uncorrected model-only close row after the fire")
	}
	return s
}

// manualModelOnlyCloseState builds a strategy holding one uncorrected
// model-only row plus its matching buffered closed_positions basis, with
// caller-controlled timestamps/side/quantity.
func manualModelOnlyCloseState(id, symbol string, ts time.Time, qty, avgCost float64, side string, estPx float64) *StrategyState {
	dirSign := 1.0
	if side == "short" {
		dirSign = -1.0
	}
	estGross := qty * dirSign * (estPx - avgCost)
	s := &StrategyState{
		ID: id, Type: "perps", Platform: "hyperliquid", Cash: 1000,
		RiskState: RiskState{DailyPnL: estGross, ConsecutiveLosses: 1, DailyPnLDate: time.Now().UTC().Format("2006-01-02")},
	}
	if estGross >= 0 {
		s.RiskState.ConsecutiveLosses = 0
	}
	s.TradeHistory = []Trade{{
		Timestamp: ts, StrategyID: s.ID, Symbol: symbol, Side: closeTradeSide(side), Quantity: qty,
		Price: estPx, Value: qty * estPx, TradeType: "perps", PositionID: "pos-1", IsClose: true,
		RealizedPnL: estGross, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: fmt.Sprintf("Circuit breaker close %s, PnL: $%.2f (%s; no exchange fill)", side, estGross, modelOnlyDetailMarker),
	}}
	s.ClosedPositions = []ClosedPosition{{
		StrategyID: s.ID, Symbol: symbol, Quantity: qty, AvgCost: avgCost, Side: side,
		Multiplier: 1, ClosedAt: ts, ClosePrice: estPx, RealizedPnL: estGross, CloseReason: "circuit_breaker",
	}}
	return s
}

func TestModelOnlyClose_FillReconcilesInsteadOfSecondRow(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	var got *modelOnlyCloseCorrection
	modelOnlyCloseUpdater = func(u modelOnlyCloseCorrection) error { got = &u; return nil }
	s := fireModelOnlyCircuitBreakerClose(t)
	rowsBefore := len(s.TradeHistory)
	cashAfterFire := s.Cash // 1000 - 400 (model estimate)

	ok := reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 777, "")
	if !ok {
		t.Fatal("expected reconciliation to succeed with a recoverable basis")
	}
	if len(s.TradeHistory) != rowsBefore {
		t.Fatalf("#954: fill must correct the existing row, not add one: %d rows", len(s.TradeHistory))
	}
	trade := s.TradeHistory[0]
	if trade.ExchangeOrderID != "777" || trade.FeeSource != FeeSourceUserFills {
		t.Errorf("trade oid/fee_source = %q/%q; want 777/userfills", trade.ExchangeOrderID, trade.FeeSource)
	}
	if trade.Price != 2900 || trade.Quantity != 2 || trade.Value != 2*2900 {
		t.Errorf("trade px/qty/value = %.2f/%.2f/%.2f; want 2900/2/5800", trade.Price, trade.Quantity, trade.Value)
	}
	wantGross := 2.0 * (2900 - 3000)
	if trade.RealizedPnL != wantGross || !trade.PnLGross {
		t.Errorf("realized_pnl = %.2f gross=%v; want %.2f/gross", trade.RealizedPnL, trade.PnLGross, wantGross)
	}
	if !strings.Contains(trade.Details, "fill-reconciled") {
		t.Errorf("details should mark the correction: %s", trade.Details)
	}
	wantNet := wantGross - 3.0
	// Cash: fire booked -400; the true result is -203, so cash must move +197.
	if s.Cash != cashAfterFire+wantNet+400 {
		t.Errorf("cash = %.4f; want %.4f (fill-derived)", s.Cash, cashAfterFire+wantNet+400)
	}
	if s.RiskState.DailyPnL != wantNet {
		t.Errorf("DailyPnL = %.4f; want %.4f (delta applied)", s.RiskState.DailyPnL, wantNet)
	}
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("loss streak must carry exactly the fire-time result, got %d", s.RiskState.ConsecutiveLosses)
	}
	if len(s.ClosedPositions) != 1 || s.ClosedPositions[0].RealizedPnL != wantNet || s.ClosedPositions[0].ClosePrice != 2900 {
		t.Errorf("closed_positions buffer = %+v; want net %.2f @ 2900", s.ClosedPositions, wantNet)
	}
	if got == nil || got.OID != "777" || !got.Complete || got.CumGross != wantGross || got.CumFee != 3.0 ||
		got.PositionID == "" || got.CloseReason != "circuit_breaker" || got.RowPrice != 2900 || got.VwapPx != 2900 {
		t.Errorf("DB correction = %+v; want complete oid 777 cum_gross %.2f fee 3.0 with position id", got, wantGross)
	}
}

func TestModelOnlyClose_PartialThenResidualCoversFullQuantity(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	var last *modelOnlyCloseCorrection
	modelOnlyCloseUpdater = func(u modelOnlyCloseCorrection) error { u2 := u; last = &u2; return nil }
	s := fireModelOnlyCircuitBreakerClose(t) // cash 600, DailyPnL -400, streak 1

	// First leg fills HALF the basis at a different price/fee.
	if !reconcileModelOnlyCloseWithFill(s, "ETH", 1.0, 2900, 2.0, 777, "") {
		t.Fatal("first partial fill must reconcile")
	}
	trade := findModelOnlyCloseTrade(s, "ETH")
	if trade == nil {
		t.Fatal("partially-reconciled row must stay matchable for the residual retry")
	}
	// gross1 = 1*(2900-3000) = -100; est share = -200; delta = (-100-2)-(-200) = +98.
	if s.Cash != 600+98 {
		t.Errorf("cash after first leg = %.2f; want 698", s.Cash)
	}
	if s.RiskState.DailyPnL != -302 {
		t.Errorf("DailyPnL after first leg = %.2f; want -302", s.RiskState.DailyPnL)
	}
	if trade.Quantity != 1 || trade.RealizedPnL != -100 || trade.ExchangeFee != 2 ||
		trade.ExchangeOrderID != "" || trade.FeeSource != FeeSourceReconcileAdjustment {
		t.Errorf("partial row = qty %.2f pnl %.2f fee %.2f oid %q src %q; want 1/-100/2/''/reconcile_adjustment",
			trade.Quantity, trade.RealizedPnL, trade.ExchangeFee, trade.ExchangeOrderID, trade.FeeSource)
	}
	if trade.Price != 2800 { // estimate price preserved while partial
		t.Errorf("partial row price = %.2f; want the estimate price 2800", trade.Price)
	}
	if !strings.Contains(trade.Details, "partial") || !strings.Contains(trade.Details, modelOnlyDetailMarker) {
		t.Errorf("partial details must stay marker-matchable: %s", trade.Details)
	}
	if last == nil || last.Complete || last.RowPrice != 2800 {
		t.Errorf("first-leg DB correction must be incomplete with the estimate price: %+v", last)
	}

	// Residual leg completes the basis at yet another price/fee.
	if !reconcileModelOnlyCloseWithFill(s, "ETH", 1.0, 2950, 1.0, 778, "") {
		t.Fatal("residual fill must reconcile against the same row")
	}
	trade = &s.TradeHistory[len(s.TradeHistory)-1]
	// gross2 = -50; est share = -200; delta = (-50-1)-(-200) = +149.
	if s.Cash != 847 {
		t.Errorf("cash after both legs = %.2f; want 847 (= 1000 + true net -153)", s.Cash)
	}
	if s.RiskState.DailyPnL != -153 {
		t.Errorf("DailyPnL after both legs = %.2f; want -153", s.RiskState.DailyPnL)
	}
	if trade.Quantity != 2 || trade.RealizedPnL != -150 || trade.ExchangeFee != 3 ||
		trade.ExchangeOrderID != "778" || trade.FeeSource != FeeSourceUserFills || trade.Price != 2925 {
		t.Errorf("completed row = qty %.2f pnl %.2f fee %.2f oid %q src %q px %.2f; want 2/-150/3/778/userfills/2925",
			trade.Quantity, trade.RealizedPnL, trade.ExchangeFee, trade.ExchangeOrderID, trade.FeeSource, trade.Price)
	}
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("streak must stay at the fire-time count across both legs, got %d", s.RiskState.ConsecutiveLosses)
	}
	if len(s.ClosedPositions) != 1 || s.ClosedPositions[0].RealizedPnL != -153 || s.ClosedPositions[0].ClosePrice != 2925 {
		t.Errorf("closed_positions = %+v; want net -153 @ vwap 2925", s.ClosedPositions)
	}
	if last == nil || !last.Complete || last.OID != "778" || last.CumGross != -150 || last.CumFee != 3 || last.VwapPx != 2925 {
		t.Errorf("final DB correction = %+v; want complete 778 cum -150 fee 3 vwap 2925", last)
	}

	// A third fill has no basis left — it must NOT book phantom PnL.
	if reconcileModelOnlyCloseWithFill(s, "ETH", 1.0, 2950, 1.0, 779, "") {
		t.Fatal("fill beyond the reconciled basis must be refused")
	}
}

func TestModelOnlyClose_ShortResidualCrossingAvgCostFlipsStreak(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	now := time.Now().UTC()
	s := manualModelOnlyCloseState("hl-short", "SOL", now, 10, 95, "short", 90) // est win +50 → streak 0
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Fatalf("precondition: estimated win resets the streak, got %d", s.RiskState.ConsecutiveLosses)
	}
	cashBefore := s.Cash

	// The real fill lands ABOVE avg cost: the "win" was actually a loss.
	if !reconcileModelOnlyCloseWithFill(s, "SOL", 10, 101, 2.0, 990, "") {
		t.Fatal("short fill must reconcile")
	}
	// gross = 10*(95-101) = -60; delta = (-60-2) - (+50) = -112.
	if s.Cash != cashBefore-112 {
		t.Errorf("cash = %.2f; want %.2f", s.Cash, cashBefore-112)
	}
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("a booked win that was really a loss must increment the streak, got %d", s.RiskState.ConsecutiveLosses)
	}
}

func TestModelOnlyClose_BookedLossTurnedWinDecrementsStreak(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	now := time.Now().UTC()
	s := manualModelOnlyCloseState("hl-loss", "AVAX", now, 2, 3000, "long", 2800) // est loss -400 → streak 1
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Fatalf("precondition: fire-time loss books streak 1, got %d", s.RiskState.ConsecutiveLosses)
	}

	if !reconcileModelOnlyCloseWithFill(s, "AVAX", 2, 3100, 5.0, 991, "") {
		t.Fatal("fill must reconcile")
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("a booked loss that was really a win must decrement the streak, got %d", s.RiskState.ConsecutiveLosses)
	}
}

func TestModelOnlyClose_OverFillClampsToBasisQuantity(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	modelOnlyCloseUpdater = func(modelOnlyCloseCorrection) error { return nil }
	s := fireModelOnlyCircuitBreakerClose(t)

	if !reconcileModelOnlyCloseWithFill(s, "ETH", 3.0, 2900, 6.0, 900, "") {
		t.Fatal("over-fill up to the basis must reconcile")
	}
	trade := findModelOnlyCloseTrade(s, "ETH")
	if trade != nil {
		t.Fatalf("completed row must carry an OID, got %+v", trade)
	}
	done := s.TradeHistory[0]
	if done.Quantity != 2 || done.RealizedPnL != -200 || done.ExchangeFee != 4 {
		t.Errorf("clamped row = qty %.2f pnl %.2f fee %.2f; want 2/-200/4 (fee scaled to the covered slice)",
			done.Quantity, done.RealizedPnL, done.ExchangeFee)
	}
	// delta = (-200-4) - (-400) = +196
	if s.Cash != 600+196 {
		t.Errorf("cash = %.2f; want 796", s.Cash)
	}
}

func TestModelOnlyClose_NonPositiveMultiplierRefused(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	now := time.Now().UTC()
	s := manualModelOnlyCloseState("hl-mult", "ETH", now, 2, 3000, "long", 2800)
	s.ClosedPositions[0].Multiplier = 0

	if reconcileModelOnlyCloseWithFill(s, "ETH", 2, 2900, 1, 910, "") {
		t.Fatal("a non-positive multiplier basis must refuse reconciliation")
	}
	rowsBefore := len(s.TradeHistory)
	applyHyperliquidCircuitCloseFill(s, "ETH", 2, 2900, 1, 2.0, 910, "")
	if len(s.TradeHistory) != rowsBefore+1 {
		t.Fatal("refused reconciliation must fall through to the defensive branch")
	}
}

func TestModelOnlyClose_NoBasisTakesDefensiveZeroPnlRow(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	s := &StrategyState{ID: "hl-x", Type: "perps", Platform: "hyperliquid"}
	rowsBefore := len(s.TradeHistory)

	applied := reconcileModelOnlyCloseWithFill(s, "ETH", 1.0, 2000, 1.0, 555, "")
	if applied {
		t.Fatal("no model-only row and no basis must NOT reconcile")
	}
	applyHyperliquidCircuitCloseFill(s, "ETH", 1.0, 2000, 1.0, 2.0, 555, "")
	if len(s.TradeHistory) != rowsBefore+1 {
		t.Fatalf("defensive branch must record its own row, rows=%d", len(s.TradeHistory))
	}
	defensive := s.TradeHistory[len(s.TradeHistory)-1]
	if defensive.RealizedPnL != 0 || defensive.ExchangeOrderID != "555" {
		t.Errorf("defensive row = pnl %.2f oid %q; want 0 / 555", defensive.RealizedPnL, defensive.ExchangeOrderID)
	}
}

func TestModelOnlyClose_SurvivesRestartBetweenFireAndFill(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	s := fireModelOnlyCircuitBreakerClose(t)
	ts := findModelOnlyCloseTrade(s, "ETH").Timestamp

	// Simulate a restart: the in-memory closed-position buffer is gone (it was
	// flushed before the crash), but the persisted basis survives in SQLite.
	s.ClosedPositions = nil
	modelOnlyCloseBasisLoader = func(strategyID, symbol string, closedAt time.Time) (*modelOnlyClosedBasis, error) {
		if strategyID != "hl-cb-eth" || symbol != "ETH" || !closedAt.Equal(ts) {
			t.Errorf("loader called with (%s, %s, %v); want (hl-cb-eth, ETH, %v)", strategyID, symbol, closedAt, ts)
		}
		return &modelOnlyClosedBasis{Quantity: 2.0, AvgCost: 3000, Side: "long", Multiplier: 1}, nil
	}

	if !reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 778, "") {
		t.Fatal("restart between fire and fill must still reconcile via the persisted basis")
	}
	if s.RiskState.DailyPnL != -203 {
		t.Errorf("DailyPnL after restart reconcile = %.4f; want -203", s.RiskState.DailyPnL)
	}
}

func TestModelOnlyClose_LoaderFailureFallsBackToDefensive(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	s := fireModelOnlyCircuitBreakerClose(t)
	s.ClosedPositions = nil
	modelOnlyCloseBasisLoader = func(string, string, time.Time) (*modelOnlyClosedBasis, error) {
		return nil, fmt.Errorf("basis row not found")
	}
	rowsBefore := len(s.TradeHistory)
	if reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 779, "") {
		t.Fatal("a failed basis lookup must not reconcile")
	}
	// And the unexplained fill falls through to the defensive zero-PnL branch.
	applyHyperliquidCircuitCloseFill(s, "ETH", 2.0, 2900, 3.0, 2.0, 779, "")
	if len(s.TradeHistory) != rowsBefore+1 {
		t.Fatalf("expected the defensive row fallback, rows=%d", len(s.TradeHistory))
	}
}

func TestModelOnlyClose_ReconciledRowIsDuplicateProof(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	s := fireModelOnlyCircuitBreakerClose(t)
	if !reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 777, "") {
		t.Fatal("expected first fill to reconcile")
	}
	cash := s.Cash
	rows := len(s.TradeHistory)
	daily := s.RiskState.DailyPnL

	applyHyperliquidCircuitCloseFill(s, "ETH", 2.0, 2900, 3.0, 2.0, 777, "")
	if len(s.TradeHistory) != rows || s.Cash != cash || s.RiskState.DailyPnL != daily {
		t.Fatalf("#954: replaying the reconciled fill must be a no-op — rows %d→%d cash %.2f daily %.2f",
			rows, len(s.TradeHistory), s.Cash, s.RiskState.DailyPnL)
	}
}

func TestModelOnlyClose_HedgeLegAdjustsDailyPnLNeverStreak(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	now := time.Now().UTC().Add(-time.Minute)
	s := &StrategyState{
		ID: "hl-cb-hedge", Type: "perps", Platform: "hyperliquid",
		RiskState: RiskState{DailyPnL: -50, ConsecutiveLosses: 2, DailyPnLDate: now.Format("2006-01-02")},
	}
	// Consistent fire-time row: short 10 @ avg 95 estimated at 95.5 → PnL -5.
	s.TradeHistory = []Trade{{
		Timestamp: now, StrategyID: s.ID, Symbol: "SOL", Side: "sell", Quantity: 10,
		Price: 95.5, Value: 955, TradeType: hedgeTradeType, IsClose: true,
		RealizedPnL: -5, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: "Circuit breaker close short, PnL: $-5.00 (" + modelOnlyDetailMarker + "; no exchange fill)",
	}}
	s.ClosedPositions = []ClosedPosition{{
		StrategyID: s.ID, Symbol: "SOL", Quantity: 10, AvgCost: 95, Side: "short",
		Multiplier: 1, ClosedAt: now, ClosePrice: 95.5, RealizedPnL: -5, CloseReason: "circuit_breaker",
	}}

	if !reconcileModelOnlyCloseWithFill(s, "SOL", 10, 90, 1.0, 880, "") {
		t.Fatal("hedge leg must reconcile")
	}
	wantGross := 10.0 * (95.0 - 90.0)
	wantNet := wantGross - 1
	if s.RiskState.DailyPnL != -50.0+wantNet+5.0 {
		t.Errorf("DailyPnL = %.2f; want %.2f (hedge delta lands in the daily aggregate)", s.RiskState.DailyPnL, -50.0+wantNet+5.0)
	}
	if s.RiskState.ConsecutiveLosses != 2 {
		t.Errorf("streak untouched by a hedge correction, got %d", s.RiskState.ConsecutiveLosses)
	}
}

func TestModelOnlyClose_DayCrossingCorrectionSkipsDailyMeter(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	modelOnlyCloseUpdater = func(modelOnlyCloseCorrection) error { return nil }
	// 25h back always lands on a different UTC day while staying inside the
	// 48h match window.
	ts := time.Now().UTC().Add(-25 * time.Hour)
	s := manualModelOnlyCloseState("hl-day", "ETH", ts, 2, 3000, "long", 2800)
	s.RiskState.DailyPnL = -50 // today's meter, unrelated to yesterday's fire
	dailyBefore := s.RiskState.DailyPnL
	cashBefore := s.Cash

	if !reconcileModelOnlyCloseWithFill(s, "ETH", 2, 3100, 5.0, 920, "") {
		t.Fatal("next-day correction must still reconcile cash")
	}
	// Cash carries the true-up...
	if delta := s.Cash - cashBefore; delta != (2*(3100-3000)-5.0)-(-400) {
		t.Errorf("cash delta = %.2f; want %.2f", delta, (2*(3100-3000)-5.0)-(-400))
	}
	// ...but today's daily-loss meter is untouched.
	if s.RiskState.DailyPnL != dailyBefore {
		t.Errorf("DailyPnL = %.2f; want unchanged %.2f (correction of another day's trade)", s.RiskState.DailyPnL, dailyBefore)
	}
}

func TestModelOnlyClose_PersistFailureRollsBackAndAlerts(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	var warns []string
	prevWarn := tradePersistWarn
	tradePersistWarn = func(msg string) { warns = append(warns, msg) }
	t.Cleanup(func() { tradePersistWarn = prevWarn })

	s := fireModelOnlyCircuitBreakerClose(t)
	snapshot := s.TradeHistory[0]
	cashAfterFire, dailyAfterFire, streakAfterFire := s.Cash, s.RiskState.DailyPnL, s.RiskState.ConsecutiveLosses

	modelOnlyCloseUpdater = func(modelOnlyCloseCorrection) error { return fmt.Errorf("disk full") }
	if ok := reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 930, ""); !ok {
		t.Fatal("handled persist failure must still keep the caller off the defensive branch")
	}
	trade := s.TradeHistory[0]
	if trade.Timestamp != snapshot.Timestamp || trade.Price != snapshot.Price || trade.Quantity != snapshot.Quantity ||
		trade.RealizedPnL != snapshot.RealizedPnL || trade.ExchangeFee != snapshot.ExchangeFee ||
		trade.ExchangeOrderID != snapshot.ExchangeOrderID || trade.FeeSource != snapshot.FeeSource ||
		trade.Details != snapshot.Details {
		t.Errorf("failed persist must roll the row back: got %+v want %+v", trade, snapshot)
	}
	if s.Cash != cashAfterFire || s.RiskState.DailyPnL != dailyAfterFire || s.RiskState.ConsecutiveLosses != streakAfterFire {
		t.Errorf("money state must roll back: cash %.2f daily %.2f streak %d", s.Cash, s.RiskState.DailyPnL, s.RiskState.ConsecutiveLosses)
	}
	if len(s.ClosedPositions) != 1 || s.ClosedPositions[0].RealizedPnL != snapshot.RealizedPnL {
		t.Errorf("closed_positions buffer must roll back: %+v", s.ClosedPositions)
	}
	if len(warns) != 1 {
		t.Fatalf("persist failure must raise the operator alert once, got %d", len(warns))
	}

	// Consistent state again: the SAME fill retries cleanly.
	modelOnlyCloseUpdater = func(modelOnlyCloseCorrection) error { return nil }
	if !reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 930, "") {
		t.Fatal("retry after rollback must reconcile")
	}
	if s.Cash != cashAfterFire+197 {
		t.Errorf("retry cash = %.2f; want %.2f", s.Cash, cashAfterFire+197)
	}
}

func TestModelOnlyClose_ReasonMismatchAndAgeBoundTakeDefensiveBranch(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	s := fireModelOnlyCircuitBreakerClose(t)
	rowsBefore := len(s.TradeHistory)

	// A regime-flip or kill-switch fill must never correct a circuit-breaker
	// row from a different close event.
	for _, reason := range []string{"regime_direction_flip", "kill_switch"} {
		if reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 940, reason) {
			t.Fatalf("%s fill must not correct a circuit_breaker row", reason)
		}
	}
	applyHyperliquidCircuitCloseFill(s, "ETH", 2.0, 2900, 3.0, 2.0, 941, "regime_direction_flip")
	if len(s.TradeHistory) != rowsBefore+1 {
		t.Fatal("mismatched-reason fill must take the defensive branch")
	}
	model := s.TradeHistory[rowsBefore-1]
	if model.RealizedPnL != -400 || model.ExchangeOrderID != "" {
		t.Error("mismatched-reason fill must leave the model row untouched")
	}

	// Age bound: a stale uncorrected row stays matchable never.
	stale := manualModelOnlyCloseState("hl-stale", "DOT", time.Now().UTC().Add(-modelOnlyReconcileMaxAge-time.Hour), 2, 3000, "long", 2800)
	if reconcileModelOnlyCloseWithFill(stale, "DOT", 2, 2900, 1, 942, "") {
		t.Fatal("a row past the age bound must not reconcile")
	}
}

func TestModelOnlyClose_RealDBTransactionUpdatesAllThreeRows(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer sdb.Close()

	now := time.Now().UTC().Truncate(time.Second)
	strat := &StrategyState{ID: "db-strat", Type: "perps", Platform: "hyperliquid"}
	trade := Trade{
		Timestamp: now, StrategyID: strat.ID, Symbol: "ETH", Side: "sell", Quantity: 2,
		Price: 2800, Value: 5600, TradeType: "perps", PositionID: "pos-1", IsClose: true,
		RealizedPnL: -400, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: "Circuit breaker close long, PnL: $-400.00 (" + modelOnlyDetailMarker + "; no exchange fill)",
	}
	if err := sdb.InsertTrade(strat.ID, trade); err != nil {
		t.Fatalf("insert trade: %v", err)
	}
	cpSQL := `INSERT INTO closed_positions (strategy_id, symbol, quantity, avg_cost, side, multiplier, opened_at, closed_at, close_price, realized_pnl, close_reason, duration_seconds)
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := sdb.db.Exec(cpSQL, strat.ID, "ETH", 2, 3000, "long", 1, "", formatTime(now), 2800, -400, "circuit_breaker", 60); err != nil {
		t.Fatalf("insert closed_position: %v", err)
	}
	diagSQL := `INSERT INTO trade_diagnostics (strategy_id, position_id, symbol, side, close_reason, entry_price, exit_price, quantity, realized_pnl, opened_at, closed_at, metrics_status)
	            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := sdb.db.Exec(diagSQL, strat.ID, "pos-1", "ETH", "long", "circuit_breaker", 3000, 2800, 2, -400, formatTime(now.Add(-time.Hour)), formatTime(now), diagMetricsPending); err != nil {
		t.Fatalf("insert diagnostics: %v", err)
	}

	u := modelOnlyCloseCorrection{
		StrategyID: strat.ID, Timestamp: now, Symbol: "ETH", PositionID: "pos-1", ClosedAt: now,
		CloseReason: "circuit_breaker", FilledQty: 2, RowPrice: 2900, VwapPx: 2900, Value: 5800,
		CumGross: -200, CumFee: 3, Complete: true, OID: "900",
		Details: "reconciled",
	}
	if err := sdb.ReconcileModelOnlyClose(u); err != nil {
		t.Fatalf("ReconcileModelOnlyClose: %v", err)
	}

	var price, rpnl, fee, qty float64
	var oid, feeSrc string
	if err := sdb.db.QueryRow(`SELECT price, realized_pnl, exchange_fee, exchange_order_id, fee_source, quantity FROM trades WHERE strategy_id=? AND timestamp=?`, strat.ID, formatTime(now)).
		Scan(&price, &rpnl, &fee, &oid, &feeSrc, &qty); err != nil {
		t.Fatalf("read trades: %v", err)
	}
	if price != 2900 || rpnl != -200 || fee != 3 || oid != "900" || feeSrc != FeeSourceUserFills || qty != 2 {
		t.Errorf("trades row = (%.2f, %.2f, %.2f, %s, %s, %.2f); want (2900, -200, 3, 900, userfills, 2)", price, rpnl, fee, oid, feeSrc, qty)
	}
	var cpPx, cpPnl float64
	if err := sdb.db.QueryRow(`SELECT close_price, realized_pnl FROM closed_positions WHERE strategy_id=? AND symbol=?`, strat.ID, "ETH").
		Scan(&cpPx, &cpPnl); err != nil {
		t.Fatalf("read closed_positions: %v", err)
	}
	if cpPx != 2900 || cpPnl != -203 {
		t.Errorf("closed_positions = (%.2f, %.2f); want (2900, -203)", cpPx, cpPnl)
	}
	var dPx, dPnl float64
	if err := sdb.db.QueryRow(`SELECT exit_price, realized_pnl FROM trade_diagnostics WHERE strategy_id=? AND position_id=?`, strat.ID, "pos-1").
		Scan(&dPx, &dPnl); err != nil {
		t.Fatalf("read diagnostics: %v", err)
	}
	if dPx != 2900 || dPnl != -203 {
		t.Errorf("diagnostics = (%.2f, %.2f); want (2900, -203)", dPx, dPnl)
	}

	// Reconciliation is one-shot: the WHERE clause no longer matches — a
	// replayed partial correction against a completed row fails the same way.
	u2 := u
	u2.Complete = false
	u2.OID = ""
	u2.FilledQty = 1
	u2.CumGross = -100
	u2.CumFee = 2
	if err := sdb.ReconcileModelOnlyClose(u2); err == nil {
		t.Fatal("replayed correction against a completed row must fail (already corrected)")
	}
}

func TestModelOnlyClose_DiagnosticsErrorFailsWholeTransaction(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer sdb.Close()

	now := time.Now().UTC().Truncate(time.Second)
	trade := Trade{
		Timestamp: now, StrategyID: "diag-fail", Symbol: "ETH", Side: "sell", Quantity: 2,
		Price: 2800, Value: 5600, TradeType: "perps", PositionID: "pos-9", IsClose: true,
		RealizedPnL: -400, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: "x (" + modelOnlyDetailMarker + ")",
	}
	if err := sdb.InsertTrade(trade.StrategyID, trade); err != nil {
		t.Fatalf("insert trade: %v", err)
	}
	cpSQL := `INSERT INTO closed_positions (strategy_id, symbol, quantity, avg_cost, side, multiplier, opened_at, closed_at, close_price, realized_pnl, close_reason, duration_seconds)
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := sdb.db.Exec(cpSQL, trade.StrategyID, "ETH", 2, 3000, "long", 1, "", formatTime(now), 2800, -400, "circuit_breaker", 60); err != nil {
		t.Fatalf("insert closed_position: %v", err)
	}
	// Force a genuine SQL failure on the diagnostics statement only.
	if _, err := sdb.db.Exec(`DROP TABLE trade_diagnostics`); err != nil {
		t.Fatalf("drop trade_diagnostics: %v", err)
	}

	u := modelOnlyCloseCorrection{
		StrategyID: trade.StrategyID, Timestamp: now, Symbol: "ETH", PositionID: "pos-9", ClosedAt: now,
		CloseReason: "circuit_breaker", FilledQty: 2, RowPrice: 2900, VwapPx: 2900, Value: 5800,
		CumGross: -200, CumFee: 3, Complete: true, OID: "950", Details: "reconciled",
	}
	if err := sdb.ReconcileModelOnlyClose(u); err == nil {
		t.Fatal("a real SQL error on the diagnostics update must fail the transaction")
	}
	// Nothing else may have committed.
	var feeSrc string
	if err := sdb.db.QueryRow(`SELECT fee_source FROM trades WHERE strategy_id=? AND timestamp=?`, trade.StrategyID, formatTime(now)).Scan(&feeSrc); err != nil {
		t.Fatalf("read trades: %v", err)
	}
	if feeSrc != FeeSourceReconcileAdjustment {
		t.Errorf("trades row must stay uncorrected after a rolled-back tx, fee_source=%q", feeSrc)
	}
}

func TestModelOnlyClose_EndToEndThroughRealSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer sdb.Close()

	// Production wiring: eager trade inserts, eager diagnostics inserts, the
	// real updater, and the real basis loader — no stubs anywhere on the path
	// that runs live.
	prevRec := tradeRecorder
	prevDiag := tradeDiagnosticsRecorder
	prevUpd := modelOnlyCloseUpdater
	prevLoad := modelOnlyCloseBasisLoader
	tradeRecorder = sdb.InsertTrade
	tradeDiagnosticsRecorder = sdb.InsertTradeDiagnostics
	modelOnlyCloseUpdater = sdb.ReconcileModelOnlyClose
	modelOnlyCloseBasisLoader = sdb.LoadModelOnlyCloseBasis
	t.Cleanup(func() {
		tradeRecorder = prevRec
		tradeDiagnosticsRecorder = prevDiag
		modelOnlyCloseUpdater = prevUpd
		modelOnlyCloseBasisLoader = prevLoad
	})

	s := fireModelOnlyCircuitBreakerClose(t)
	state := &AppState{CycleCount: 1, Strategies: map[string]*StrategyState{s.ID: s}}
	if err := sdb.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if s.ClosedPositions != nil {
		t.Fatal("SaveState must clear the closed-position buffer (production flush semantics)")
	}

	// Restart semantics: the buffer is gone; the loader recovers the basis
	// from the persisted closed_positions row through formatTime round-trip.
	s.ClosedPositions = nil
	if !reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 900, "") {
		t.Fatal("end-to-end reconcile through real SQLite must succeed")
	}

	var rpnl float64
	var oid string
	if err := sdb.db.QueryRow(`SELECT realized_pnl, exchange_order_id FROM trades WHERE strategy_id=?`, s.ID).Scan(&rpnl, &oid); err != nil {
		t.Fatalf("read trades: %v", err)
	}
	if rpnl != -200 || oid != "900" {
		t.Errorf("persisted trades row = (%.2f, %s); want (-200, 900)", rpnl, oid)
	}
	var cpPx, cpPnl float64
	if err := sdb.db.QueryRow(`SELECT close_price, realized_pnl FROM closed_positions WHERE strategy_id=?`, s.ID).Scan(&cpPx, &cpPnl); err != nil {
		t.Fatalf("read closed_positions: %v", err)
	}
	if cpPx != 2900 || cpPnl != -203 {
		t.Errorf("persisted closed_positions = (%.2f, %.2f); want (2900, -203)", cpPx, cpPnl)
	}
	var dPx, dPnl float64
	if err := sdb.db.QueryRow(`SELECT exit_price, realized_pnl FROM trade_diagnostics WHERE strategy_id=?`, s.ID).Scan(&dPx, &dPnl); err != nil {
		t.Fatalf("read diagnostics: %v", err)
	}
	if dPx != 2900 || dPnl != -203 {
		t.Errorf("persisted diagnostics = (%.2f, %.2f); want (2900, -203)", dPx, dPnl)
	}
}

func TestModelOnlyClose_SharedCoinWritesNoModelOnlyRow(t *testing.T) {
	// #620 scope guard under #1455: a strategy sharing its coin with a peer
	// takes the operator-required path — no pending close, no fire-time sweep,
	// therefore no model-only row to reconcile.
	sc := StrategyConfig{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
		Args: []string{"sma", "ETH", "1h", "--mode=live"}}
	peer := StrategyConfig{ID: "hl-manual-eth", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
		Args: []string{"hold", "ETH", "1h", "--mode=live"}}
	assist := &PlatformRiskAssist{HLLiveAll: []StrategyConfig{sc, peer}}
	if shouldForceCloseAllPositionsOnCircuitBreaker(&sc, assist) {
		t.Fatal("shared-coin CB must take the operator-required path, not the force-close sweep")
	}
}
