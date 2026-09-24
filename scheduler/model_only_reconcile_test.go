package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetModelOnlyReconcileHooks(t *testing.T) {
	t.Helper()
	prevUpdater := modelOnlyCloseUpdater
	prevLoader := modelOnlyCloseBasisLoader
	t.Cleanup(func() {
		modelOnlyCloseUpdater = prevUpdater
		modelOnlyCloseBasisLoader = prevLoader
	})
}

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
	forceCloseAllPositions(s, nil, map[string]float64{"ETH": 2800}, nil)
	if len(s.Positions) != 0 {
		t.Fatalf("fire must delete the virtual position, has %v", s.Positions)
	}
	model := findModelOnlyCloseTrade(s, "ETH")
	if model == nil {
		t.Fatal("expected an uncorrected model-only close row after the fire")
	}
	return s
}

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
		Details: fmt.Sprintf("Circuit breaker close %s, PnL: $%.2f (%s; no exchange fill), pre-streak=0", side, estGross, modelOnlyDetailMarker),
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
	cashAfterFire := s.Cash

	out := reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 777, "")
	if out != modelOnlyReconcileApplied {
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
	s := fireModelOnlyCircuitBreakerClose(t)

	if reconcileModelOnlyCloseWithFill(s, "ETH", 1.0, 2900, 2.0, 777, "") != modelOnlyReconcileApplied {
		t.Fatal("first partial fill must reconcile")
	}
	trade := findModelOnlyCloseTrade(s, "ETH")
	if trade == nil {
		t.Fatal("partially-reconciled row must stay matchable for the residual retry")
	}
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
	if trade.Price != 2800 {
		t.Errorf("partial row price = %.2f; want the estimate price 2800", trade.Price)
	}
	if !strings.Contains(trade.Details, "partial") || !strings.Contains(trade.Details, modelOnlyDetailMarker) {
		t.Errorf("partial details must stay marker-matchable: %s", trade.Details)
	}
	if last == nil || last.Complete || last.RowPrice != 2800 {
		t.Errorf("first-leg DB correction must be incomplete with the estimate price: %+v", last)
	}

	if reconcileModelOnlyCloseWithFill(s, "ETH", 1.0, 2950, 1.0, 778, "") != modelOnlyReconcileApplied {
		t.Fatal("residual fill must reconcile against the same row")
	}
	trade = &s.TradeHistory[len(s.TradeHistory)-1]
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

	if reconcileModelOnlyCloseWithFill(s, "ETH", 1.0, 2950, 1.0, 779, "") != modelOnlyReconcileNone {
		t.Fatal("fill beyond the reconciled basis must be refused")
	}
}

func TestModelOnlyClose_OverFillClampsToBasisQuantity(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	modelOnlyCloseUpdater = func(modelOnlyCloseCorrection) error { return nil }
	s := fireModelOnlyCircuitBreakerClose(t)

	if reconcileModelOnlyCloseWithFill(s, "ETH", 3.0, 2900, 6.0, 900, "") != modelOnlyReconcileApplied {
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
	if s.Cash != 600+196 {
		t.Errorf("cash = %.2f; want 796", s.Cash)
	}
}

func TestModelOnlyClose_ReconciledRowIsDuplicateProof(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	s := fireModelOnlyCircuitBreakerClose(t)
	if reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 777, "") != modelOnlyReconcileApplied {
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

func TestModelOnlyClose_DayCrossingCorrectionSkipsDailyMeter(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	modelOnlyCloseUpdater = func(modelOnlyCloseCorrection) error { return nil }
	ts := time.Now().UTC().Add(-25 * time.Hour)
	s := manualModelOnlyCloseState("hl-day", "ETH", ts, 2, 3000, "long", 2800)
	s.RiskState.DailyPnL = -50
	dailyBefore := s.RiskState.DailyPnL
	cashBefore := s.Cash

	if reconcileModelOnlyCloseWithFill(s, "ETH", 2, 3100, 5.0, 920, "") != modelOnlyReconcileApplied {
		t.Fatal("next-day correction must still reconcile cash")
	}
	if delta := s.Cash - cashBefore; delta != (2*(3100-3000)-5.0)-(-400) {
		t.Errorf("cash delta = %.2f; want %.2f", delta, (2*(3100-3000)-5.0)-(-400))
	}
	if s.RiskState.DailyPnL != dailyBefore {
		t.Errorf("DailyPnL = %.2f; want unchanged %.2f (correction of another day's trade)", s.RiskState.DailyPnL, dailyBefore)
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

func TestModelOnlyClose_SharedCoinWritesNoModelOnlyRow(t *testing.T) {
	sc := StrategyConfig{ID: "hl-a", Platform: "hyperliquid", Type: "perps",
		Args: []string{"sma", "ETH", "1h", "--mode=live"}}
	peer := StrategyConfig{ID: "hl-manual-eth", Platform: "hyperliquid", Type: "manual", Symbol: "ETH",
		Args: []string{"hold", "ETH", "1h", "--mode=live"}}
	assist := &PlatformRiskAssist{HLLiveAll: []StrategyConfig{sc, peer}}
	if shouldForceCloseAllPositionsOnCircuitBreaker(&sc, assist) {
		t.Fatal("shared-coin CB must take the operator-required path, not the force-close sweep")
	}
}

func TestModelOnlyClose_InFlightSliceOIDsAreDuplicateProof(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	modelOnlyCloseUpdater = func(modelOnlyCloseCorrection) error { return nil }
	s := fireModelOnlyCircuitBreakerClose(t)

	applyHyperliquidCircuitCloseFill(s, "ETH", 1.0, 2900, 2.0, 2.0, 700, "")
	cash, rows := s.Cash, len(s.TradeHistory)
	trade := findModelOnlyCloseTrade(s, "ETH")
	if trade == nil || trade.Quantity != 1 || !strings.Contains(trade.Details, "oids=700") {
		t.Fatalf("slice 1 must record its OID on the partial row: %+v", trade)
	}

	applyHyperliquidCircuitCloseFill(s, "ETH", 1.0, 2900, 2.0, 2.0, 700, "")
	if len(s.TradeHistory) != rows || s.Cash != cash {
		t.Fatalf("replayed in-flight OID must be a no-op: rows %d→%d cash %.2f→%.2f", rows, len(s.TradeHistory), cash, s.Cash)
	}
	if trade := findModelOnlyCloseTrade(s, "ETH"); trade == nil || trade.Quantity != 1 {
		t.Fatalf("replayed OID must not extend the filled quantity: %+v", trade)
	}

	if reconcileModelOnlyCloseWithFill(s, "ETH", 1.0, 2950, 1.0, 701, "") != modelOnlyReconcileApplied {
		t.Fatal("distinct residual OID must reconcile")
	}
	done := &s.TradeHistory[len(s.TradeHistory)-1]
	if done.ExchangeOrderID != "701" || !strings.Contains(done.Details, "oids=700") {
		t.Errorf("completed row must carry the final OID with earlier slices recorded: %q / %q", done.ExchangeOrderID, done.Details)
	}
	applyHyperliquidCircuitCloseFill(s, "ETH", 1.0, 2950, 1.0, 1.0, 701, "")
	if len(s.TradeHistory) != rows {
		t.Fatal("completed row replay must stay a no-op")
	}
}

func seedModelOnlyCloseInDB(t *testing.T, sdb *StateDB, strategyID string, now time.Time) {
	t.Helper()
	trade := Trade{
		Timestamp: now, StrategyID: strategyID, Symbol: "ETH", Side: "sell", Quantity: 2,
		Price: 2800, Value: 5600, TradeType: "perps", PositionID: "pos-r2", IsClose: true,
		RealizedPnL: -400, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: "Circuit breaker close long, PnL: $-400.00 (" + modelOnlyDetailMarker + "; no exchange fill)",
	}
	if err := sdb.InsertTrade(strategyID, trade); err != nil {
		t.Fatalf("insert trade: %v", err)
	}
	cpSQL := `INSERT INTO closed_positions (strategy_id, symbol, quantity, avg_cost, side, multiplier, opened_at, closed_at, close_price, realized_pnl, close_reason, duration_seconds)
	          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := sdb.db.Exec(cpSQL, strategyID, "ETH", 2, 3000, "long", 1, "", formatTime(now), 2800, -400, "circuit_breaker", 60); err != nil {
		t.Fatalf("insert closed_position: %v", err)
	}
}

func TestLoadModelOnlyCloseBasis_ReasonGuardHoldsOnProductionPath(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	sdb, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer sdb.Close()
	modelOnlyCloseUpdater = sdb.ReconcileModelOnlyClose
	modelOnlyCloseBasisLoader = sdb.LoadModelOnlyCloseBasis

	now := time.Now().UTC().Truncate(time.Second)
	strat := &StrategyState{ID: "r2-guard", Type: "perps", Platform: "hyperliquid"}
	seedModelOnlyCloseInDB(t, sdb, strat.ID, now)

	history, err := sdb.RecentTradesForStrategy(strat.ID, 100)
	if err != nil {
		t.Fatalf("rehydrate trades: %v", err)
	}
	s := &StrategyState{ID: strat.ID, Type: "perps", Platform: "hyperliquid", Cash: 1000,
		RiskState: RiskState{DailyPnLDate: now.Format("2006-01-02")}, TradeHistory: history}

	rowsBefore := len(s.TradeHistory)
	applyHyperliquidCircuitCloseFill(s, "ETH", 2.0, 2900, 3.0, 2.0, 960, "kill_switch")
	if len(s.TradeHistory) != rowsBefore+1 {
		t.Fatal("reason mismatch on the production path must take the defensive branch, never fail silently")
	}
	defensive := s.TradeHistory[len(s.TradeHistory)-1]
	if defensive.ExchangeOrderID != "960" || defensive.RealizedPnL != 0 {
		t.Errorf("defensive row = oid %q pnl %.2f; want 960/0", defensive.ExchangeOrderID, defensive.RealizedPnL)
	}
	var rpnl float64
	if err := sdb.db.QueryRow(`SELECT realized_pnl FROM trades WHERE strategy_id=? AND exchange_order_id=''`, strat.ID).Scan(&rpnl); err != nil {
		t.Fatalf("read untouched model row: %v", err)
	}
	if rpnl != -400 {
		t.Errorf("stale CB model row must stay untouched by a mismatched-reason fill, pnl=%.2f", rpnl)
	}

	if reconcileModelOnlyCloseWithFill(s, "ETH", 2.0, 2900, 3.0, 961, "") != modelOnlyReconcileApplied {
		t.Fatal("genuine CB fill after restart must reconcile via the persisted basis")
	}
	var cpPx float64
	if err := sdb.db.QueryRow(`SELECT close_price FROM closed_positions WHERE strategy_id=?`, strat.ID).Scan(&cpPx); err != nil {
		t.Fatalf("read closed_positions: %v", err)
	}
	if cpPx != 2900 {
		t.Errorf("persisted basis correction = %.2f; want 2900", cpPx)
	}
}

func TestTradeLedgerNoOIDReconcileMatches_SkipsInFlightPartialRows(t *testing.T) {
	now := time.Now().UTC()
	fillMap := map[string]HLFillSummary{
		"700": {Coin: "ETH", Qty: 1.0, Px: 2900, Fee: 2.0, Count: 1,
			FirstTimeMS: now.Add(-30 * time.Second).UnixMilli(), LastTimeMS: now.UnixMilli()},
		"701": {Coin: "SOL", Qty: 1.0, Px: 90, Fee: 0.5, Count: 1,
			FirstTimeMS: now.Add(-30 * time.Second).UnixMilli(), LastTimeMS: now.UnixMilli()},
	}
	trades := []TradeBackfillRow{
		{RowID: 1, Timestamp: now.Add(-time.Minute), Symbol: "ETH", IsClose: true,
			Quantity: 1.0, Price: 2800, Value: 2800, FeeSource: FeeSourceReconcileAdjustment,
			PnLGross: true, RealizedPnL: -200,
			Details: "Circuit breaker close long [fill-reconciled partial 1.000000/2.000000 oids=700], PnL so far: $-100.00 gross (" + modelOnlyDetailMarker + ")"},
		{RowID: 2, Timestamp: now.Add(-time.Minute), Symbol: "SOL", IsClose: true,
			Quantity: 1.0, Price: 90, Value: 90, FeeSource: FeeSourceReconcileAdjustment,
			PnLGross: true, RealizedPnL: -10,
			Details: "Circuit breaker close long, PnL: $-10.00 (" + modelOnlyDetailMarker + "; no exchange fill)"},
	}
	trades = append(trades, TradeBackfillRow{RowID: 3, Timestamp: now.Add(-time.Minute), Symbol: "DOT", IsClose: true,
		Quantity: 1.0, Price: 90, Value: 90, FeeSource: FeeSourceReconcileAdjustment,
		PnLGross: true, RealizedPnL: -10,
		Details: "Circuit breaker close long [fill-reconciled partial 1.000000/2.000000 oids=702] (" + modelOnlyDetailMarker + ") [reconcile-abandoned]"})
	fillMap["702"] = HLFillSummary{Coin: "DOT", Qty: 1.0, Px: 90, Fee: 0.5, Count: 1,
		FirstTimeMS: now.Add(-30 * time.Second).UnixMilli(), LastTimeMS: now.UnixMilli()}

	matches := tradeLedgerNoOIDReconcileMatches(trades, fillMap, map[string]bool{})
	if _, ok := matches[1]; ok {
		t.Fatal("in-flight partial reconciliation rows must be skipped by the offline backfill")
	}
	if _, ok := matches[2]; !ok {
		t.Fatal("an untouched pre-#1455 model-only row must still be repairable")
	}
	if _, ok := matches[3]; !ok {
		t.Fatal("an ABANDONED row must be released for offline repair — it is the recovery the owner DM names")
	}
}

func TestModelOnlyClose_StreakClassifiedOnNetPnL(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	modelOnlyCloseUpdater = func(modelOnlyCloseCorrection) error { return nil }
	now := time.Now().UTC()

	s := &StrategyState{
		ID: "hl-fee-a", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		RiskState: RiskState{ConsecutiveLosses: 4, DailyPnLDate: now.Format("2006-01-02")},
	}
	estLoss := 2.0 * (2990 - 3000)
	s.TradeHistory = []Trade{{
		Timestamp: now, StrategyID: s.ID, Symbol: "ETH", Side: "sell", Quantity: 2,
		Price: 2990, Value: 5980, TradeType: "perps", PositionID: "pos-fa", IsClose: true,
		RealizedPnL: estLoss, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: fmt.Sprintf("Circuit breaker close long, PnL: $%.2f (%s; no exchange fill), pre-streak=3", estLoss, modelOnlyDetailMarker),
	}}
	s.ClosedPositions = []ClosedPosition{{
		StrategyID: s.ID, Symbol: "ETH", Quantity: 2, AvgCost: 3000, Side: "long",
		Multiplier: 1, ClosedAt: now, ClosePrice: 2990, RealizedPnL: estLoss, CloseReason: "circuit_breaker",
	}}
	if reconcileModelOnlyCloseWithFill(s, "ETH", 2, 3000.10, 0.60, 850, "") != modelOnlyReconcileApplied {
		t.Fatal("fill must reconcile")
	}
	if got := s.RiskState.ConsecutiveLosses; got != 4 {
		t.Errorf("net loss inside the fee band must keep pre-fire+1 = 4, got %d", got)
	}

	s2 := &StrategyState{
		ID: "hl-fee-b", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		RiskState: RiskState{ConsecutiveLosses: 0, DailyPnLDate: now.Format("2006-01-02")},
	}
	estWin := 2.0 * (3010 - 3000)
	s2.TradeHistory = []Trade{{
		Timestamp: now, StrategyID: s2.ID, Symbol: "ETH", Side: "sell", Quantity: 2,
		Price: 3010, Value: 6020, TradeType: "perps", PositionID: "pos-fb", IsClose: true,
		RealizedPnL: estWin, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: fmt.Sprintf("Circuit breaker close long, PnL: $%.2f (%s; no exchange fill), pre-streak=2", estWin, modelOnlyDetailMarker),
	}}
	s2.ClosedPositions = []ClosedPosition{{
		StrategyID: s2.ID, Symbol: "ETH", Quantity: 2, AvgCost: 3000, Side: "long",
		Multiplier: 1, ClosedAt: now, ClosePrice: 3010, RealizedPnL: estWin, CloseReason: "circuit_breaker",
	}}
	if reconcileModelOnlyCloseWithFill(s2, "ETH", 2, 3000.05, 0.50, 851, "") != modelOnlyReconcileApplied {
		t.Fatal("fill must reconcile")
	}
	if got := s2.RiskState.ConsecutiveLosses; got != 3 {
		t.Errorf("net loss under an estimated win must become pre-fire+1 = 3, got %d", got)
	}

	sh := &StrategyState{
		ID: "hl-fee-hedge", Type: "perps", Platform: "hyperliquid",
		RiskState: RiskState{ConsecutiveLosses: 7, DailyPnLDate: now.Format("2006-01-02")},
	}
	sh.TradeHistory = []Trade{{
		Timestamp: now, StrategyID: sh.ID, Symbol: "SOL", Side: "buy", Quantity: 10,
		Price: 95, Value: 950, TradeType: hedgeTradeType, IsClose: true,
		RealizedPnL: 0.10, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: fmt.Sprintf("Circuit breaker close short, PnL: $0.10 (%s; no exchange fill), pre-streak=6", modelOnlyDetailMarker),
	}}
	sh.ClosedPositions = []ClosedPosition{{
		StrategyID: sh.ID, Symbol: "SOL", Quantity: 10, AvgCost: 95, Side: "short",
		Multiplier: 1, ClosedAt: now, ClosePrice: 95, RealizedPnL: 0.10, CloseReason: "circuit_breaker",
	}}
	if reconcileModelOnlyCloseWithFill(sh, "SOL", 10, 94.995, 0.90, 852, "") != modelOnlyReconcileApplied {
		t.Fatal("hedge fill must reconcile")
	}
	if got := sh.RiskState.ConsecutiveLosses; got != 7 {
		t.Errorf("hedge correction in the fee band must never touch the streak, got %d", got)
	}
}

func TestModelOnlyClose_PreStreakStampSurvivesMultiSliceRewrites(t *testing.T) {
	resetModelOnlyReconcileHooks(t)
	modelOnlyCloseUpdater = func(modelOnlyCloseCorrection) error { return nil }
	now := time.Now().UTC()
	estGross := 2.0 * (3010 - 3000)
	s := &StrategyState{
		ID: "hl-ms3", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		RiskState: RiskState{ConsecutiveLosses: 0, DailyPnLDate: now.Format("2006-01-02")},
	}
	s.TradeHistory = []Trade{{
		Timestamp: now, StrategyID: s.ID, Symbol: "ETH", Side: "sell", Quantity: 2,
		Price: 3010, Value: 6020, TradeType: "perps", PositionID: "pos-m", IsClose: true,
		RealizedPnL: estGross, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: fmt.Sprintf("Circuit breaker close long, PnL: $%.2f (%s; no exchange fill), pre-streak=3", estGross, modelOnlyDetailMarker),
	}}
	s.ClosedPositions = []ClosedPosition{{
		StrategyID: s.ID, Symbol: "ETH", Quantity: 2, AvgCost: 3000, Side: "long",
		Multiplier: 1, ClosedAt: now, ClosePrice: 3010, RealizedPnL: estGross, CloseReason: "circuit_breaker",
	}}

	if reconcileModelOnlyCloseWithFill(s, "ETH", 1, 3100, 0, 860, "") != modelOnlyReconcileApplied {
		t.Fatal("slice 1 must reconcile")
	}
	if got := s.RiskState.ConsecutiveLosses; got != 0 {
		t.Fatalf("an incomplete sequence must not touch the streak, got %d", got)
	}
	if trade := findModelOnlyCloseTrade(s, "ETH"); trade == nil || !strings.Contains(trade.Details, ", pre-streak=3") {
		t.Fatalf("slice 1's rewrite must preserve the pre-streak stamp: %+v", trade)
	}
	if reconcileModelOnlyCloseWithFill(s, "ETH", 1, 2800, 0, 861, "") != modelOnlyReconcileApplied {
		t.Fatal("slice 2 must reconcile")
	}
	if got := s.RiskState.ConsecutiveLosses; got != 4 {
		t.Errorf("two-slice est-win→real-loss must reconstruct pre-fire+1 = 4, got %d", got)
	}

	s2 := &StrategyState{
		ID: "hl-ms4", Type: "perps", Platform: "hyperliquid", Cash: 1000,
		RiskState: RiskState{ConsecutiveLosses: 5, DailyPnLDate: now.Format("2006-01-02")},
	}
	estLoss := 2.0 * (2800 - 3000)
	s2.TradeHistory = []Trade{{
		Timestamp: now, StrategyID: s2.ID, Symbol: "ETH", Side: "sell", Quantity: 2,
		Price: 2800, Value: 5600, TradeType: "perps", PositionID: "pos-n", IsClose: true,
		RealizedPnL: estLoss, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: fmt.Sprintf("Circuit breaker close long, PnL: $%.2f (%s; no exchange fill), pre-streak=4", estLoss, modelOnlyDetailMarker),
	}}
	s2.ClosedPositions = []ClosedPosition{{
		StrategyID: s2.ID, Symbol: "ETH", Quantity: 2, AvgCost: 3000, Side: "long",
		Multiplier: 1, ClosedAt: now, ClosePrice: 2800, RealizedPnL: estLoss, CloseReason: "circuit_breaker",
	}}
	if reconcileModelOnlyCloseWithFill(s2, "ETH", 1, 2700, 0, 862, "") != modelOnlyReconcileApplied {
		t.Fatal("slice 1 must reconcile")
	}
	if reconcileModelOnlyCloseWithFill(s2, "ETH", 1, 3400, 0, 863, "") != modelOnlyReconcileApplied {
		t.Fatal("slice 2 must reconcile")
	}
	if got := s2.RiskState.ConsecutiveLosses; got != 0 {
		t.Errorf("two-slice est-loss→real-win must reset the streak to 0, got %d", got)
	}

	sh := &StrategyState{
		ID: "hl-ms-hedge", Type: "perps", Platform: "hyperliquid",
		RiskState: RiskState{ConsecutiveLosses: 6, DailyPnLDate: now.Format("2006-01-02")},
	}
	sh.TradeHistory = []Trade{{
		Timestamp: now, StrategyID: sh.ID, Symbol: "SOL", Side: "buy", Quantity: 10,
		Price: 95, Value: 950, TradeType: hedgeTradeType, IsClose: true,
		RealizedPnL: -50, PnLGross: true, FeeSource: FeeSourceReconcileAdjustment,
		Details: fmt.Sprintf("Circuit breaker close short, PnL: $-50.00 (%s; no exchange fill), pre-streak=5", modelOnlyDetailMarker),
	}}
	sh.ClosedPositions = []ClosedPosition{{
		StrategyID: sh.ID, Symbol: "SOL", Quantity: 10, AvgCost: 95, Side: "short",
		Multiplier: 1, ClosedAt: now, ClosePrice: 95, RealizedPnL: -50, CloseReason: "circuit_breaker",
	}}
	if reconcileModelOnlyCloseWithFill(sh, "SOL", 4, 90, 0, 864, "") != modelOnlyReconcileApplied {
		t.Fatal("hedge slice 1 must reconcile")
	}
	if reconcileModelOnlyCloseWithFill(sh, "SOL", 6, 92, 0, 865, "") != modelOnlyReconcileApplied {
		t.Fatal("hedge slice 2 must reconcile")
	}
	if got := sh.RiskState.ConsecutiveLosses; got != 6 {
		t.Errorf("hedge multi-slice correction must never touch the streak, got %d", got)
	}
}
