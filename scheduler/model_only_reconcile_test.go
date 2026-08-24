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
// defensive row.

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

func TestModelOnlyClose_FillReconcilesInsteadOfSecondRow(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	var got *modelOnlyCloseCorrection
	modelOnlyCloseUpdater = func(u modelOnlyCloseCorrection) error { got = &u; return nil }
	s := fireModelOnlyCircuitBreakerClose(t)
	rowsBefore := len(s.TradeHistory)
	cashAfterFire := s.Cash // 1000 - 400 (model estimate)

	ok := reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 777)
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
	if trade.Price != 2900 {
		t.Errorf("trade price = %.2f; want fill px 2900", trade.Price)
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
	if got == nil || got.OID != "777" || got.GrossPnL != wantGross || got.Fee != 3.0 || got.PositionID == "" {
		t.Errorf("DB correction = %+v; want oid 777 gross %.2f fee 3.0 with position id", got, wantGross)
	}
}

func TestModelOnlyClose_NoBasisTakesDefensiveZeroPnlRow(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	s := &StrategyState{ID: "hl-x", Type: "perps", Platform: "hyperliquid"}
	rowsBefore := len(s.TradeHistory)

	applied := reconcileModelOnlyCloseWithFill(s, "ETH", 1.0, 2000, 1.0, 555)
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

	if !reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 778) {
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
	if reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 779) {
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
	if !reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 777) {
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
	s.TradeHistory = []Trade{{
		Timestamp: now, StrategyID: s.ID, Symbol: "SOL", Side: "sell", Quantity: 10,
		Price: 100, Value: 1000, TradeType: hedgeTradeType, IsClose: true,
		RealizedPnL: -5, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: "Circuit breaker close short, PnL: $-5.00 (" + modelOnlyDetailMarker + "; no exchange fill)",
	}}
	s.ClosedPositions = []ClosedPosition{{
		StrategyID: s.ID, Symbol: "SOL", Quantity: 10, AvgCost: 95, Side: "short",
		Multiplier: 1, ClosedAt: now, ClosePrice: 100, RealizedPnL: -5, CloseReason: "circuit_breaker",
	}}

	if !reconcileModelOnlyCloseWithFill(s, "SOL", 10, 90, 1.0, 880) {
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
		Price: 2900, Value: 5800, GrossPnL: -200, Fee: 3, OID: "900",
		Details: "reconciled",
	}
	if err := sdb.ReconcileModelOnlyClose(u); err != nil {
		t.Fatalf("ReconcileModelOnlyClose: %v", err)
	}

	var price, rpnl, fee float64
	var oid, feeSrc string
	if err := sdb.db.QueryRow(`SELECT price, realized_pnl, exchange_fee, exchange_order_id, fee_source FROM trades WHERE strategy_id=? AND timestamp=?`, strat.ID, formatTime(now)).
		Scan(&price, &rpnl, &fee, &oid, &feeSrc); err != nil {
		t.Fatalf("read trades: %v", err)
	}
	if price != 2900 || rpnl != -200 || fee != 3 || oid != "900" || feeSrc != FeeSourceUserFills {
		t.Errorf("trades row = (%.2f, %.2f, %.2f, %s, %s); want (2900, -200, 3, 900, userfills)", price, rpnl, fee, oid, feeSrc)
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

	// Reconciliation is one-shot: the WHERE clause no longer matches.
	u.OID = "901"
	if err := sdb.ReconcileModelOnlyClose(u); err == nil {
		t.Fatal("second reconciliation of the same row must fail (already corrected)")
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
