package main

import (
	"math"
	"testing"
)

func newHedgeState() *StrategyState {
	return &StrategyState{
		ID:              "hl-eth",
		Platform:        "hyperliquid",
		Type:            "perps",
		Cash:            10000,
		RiskState:       newRiskState(todayUTC(), 0),
		Positions:       make(map[string]*Position),
		OptionPositions: make(map[string]*OptionPosition),
	}
}

func TestRecordHedgeTradeResultDoesNotTouchStreak(t *testing.T) {
	r := newRiskState(todayUTC(), 0)
	r.ConsecutiveLosses = 3
	RecordHedgeTradeResult(&r, -50)
	if r.DailyPnL != -50 {
		t.Errorf("DailyPnL: want -50, got %g", r.DailyPnL)
	}
	if r.ConsecutiveLosses != 3 {
		t.Errorf("hedge loss must NOT change ConsecutiveLosses; want 3, got %d", r.ConsecutiveLosses)
	}
}

func TestRecordTradeResultForPositionRouting(t *testing.T) {
	r := newRiskState(todayUTC(), 0)
	recordTradeResultForPosition(&r, &Position{HedgeFor: "ETH"}, -10)
	if r.ConsecutiveLosses != 0 {
		t.Errorf("hedge routing must not increment streak, got %d", r.ConsecutiveLosses)
	}
	recordTradeResultForPosition(&r, &Position{}, -10)
	if r.ConsecutiveLosses != 1 {
		t.Errorf("non-hedge routing must increment streak, got %d", r.ConsecutiveLosses)
	}
}

func TestApplyHedgeOpenFillCreatesLeg(t *testing.T) {
	s := newHedgeState()
	sc := hedgeSC(1.0)
	decision := hedgeAction{Kind: hedgeActionOpen, Qty: 0.5, Side: "sell", TargetBasis: 10}
	snap := hedgeSnapshot{PrimaryQty: 10, PrimarySide: "long"}
	cashBefore := s.Cash
	n := applyHedgeOpenOrAddFill(sc, s, "BTC", "ETH", decision, snap, 0.5, 40000, 0, false, 0, false, nil)
	if n != 1 {
		t.Fatalf("expected 1 hedge trade booked, got %d", n)
	}
	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge Position not created")
	}
	if pos.HedgeFor != "ETH" {
		t.Errorf("HedgeFor: want ETH, got %q", pos.HedgeFor)
	}
	if pos.Side != "short" {
		t.Errorf("hedge position side: want short, got %s", pos.Side)
	}
	if math.Abs(pos.Quantity-0.5) > 1e-9 {
		t.Errorf("hedge qty: want 0.5, got %g", pos.Quantity)
	}
	if math.Abs(pos.HedgePrimaryQtyBasis-10) > 1e-9 {
		t.Errorf("basis: want 10, got %g", pos.HedgePrimaryQtyBasis)
	}
	if pos.Multiplier != 1 {
		t.Errorf("hedge Multiplier: want 1, got %g", pos.Multiplier)
	}
	if s.Cash >= cashBefore {
		t.Errorf("open fee should reduce cash; before=%g after=%g", cashBefore, s.Cash)
	}
	// The booked trade must carry trade_type=hedge (an open leg, not a close).
	found := false
	for _, tr := range s.TradeHistory {
		if tr.Symbol == "BTC" && tr.TradeType == "hedge" && !tr.IsClose {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a hedge open Trade with trade_type=hedge; history=%+v", s.TradeHistory)
	}
}

func TestApplyHedgeAddFillBlends(t *testing.T) {
	s := newHedgeState()
	sc := hedgeSC(1.0)
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.5, AvgCost: 40000, Side: "short", Multiplier: 1,
		OwnerStrategyID: s.ID, HedgeFor: "ETH", HedgePrimaryQtyBasis: 10,
	}
	decision := hedgeAction{Kind: hedgeActionAdd, Qty: 0.25, Side: "sell", TargetBasis: 15}
	snap := hedgeSnapshot{PrimaryQty: 15, PrimarySide: "long", HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}
	applyHedgeOpenOrAddFill(sc, s, "BTC", "ETH", decision, snap, 0.25, 44000, 0, false, 0, false, nil)
	pos := s.Positions["BTC"]
	if math.Abs(pos.Quantity-0.75) > 1e-9 {
		t.Errorf("blended qty: want 0.75, got %g", pos.Quantity)
	}
	// AvgCost = (0.5*40000 + 0.25*44000) / 0.75 = 41333.33
	if math.Abs(pos.AvgCost-41333.333333) > 1e-3 {
		t.Errorf("blended avgcost: want ~41333.33, got %g", pos.AvgCost)
	}
	if math.Abs(pos.HedgePrimaryQtyBasis-15) > 1e-9 {
		t.Errorf("advanced basis: want 15, got %g", pos.HedgePrimaryQtyBasis)
	}
}

func TestApplyHedgeCloseFillBooksAndRoutes(t *testing.T) {
	s := newHedgeState()
	sc := hedgeSC(1.0)
	s.RiskState.ConsecutiveLosses = 2
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 40000, Side: "short", Multiplier: 1,
		OwnerStrategyID: s.ID, HedgeFor: "ETH", HedgePrimaryQtyBasis: 10,
	}
	decision := hedgeAction{Kind: hedgeActionCloseFull, Qty: 0.5, Side: "buy"}
	snap := hedgeSnapshot{HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}
	// Close at a HIGHER price → a short hedge LOSES (correct: primary long won).
	applyHedgeReduceOrCloseFill(sc, s, "BTC", "ETH", decision, snap, 0.5, 42000, 0, false, 0, true, nil)
	if _, ok := s.Positions["BTC"]; ok {
		t.Error("hedge Position should be deleted after full close")
	}
	// Hedge loss must NOT increment the loss streak (RecordHedgeTradeResult).
	if s.RiskState.ConsecutiveLosses != 2 {
		t.Errorf("hedge close must not touch loss streak; want 2, got %d", s.RiskState.ConsecutiveLosses)
	}
	// A close Trade with trade_type=hedge must be recorded.
	found := false
	for _, tr := range s.TradeHistory {
		if tr.Symbol == "BTC" && tr.TradeType == "hedge" && tr.IsClose {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a hedge close Trade with trade_type=hedge; history=%+v", s.TradeHistory)
	}
}

func TestApplyHedgeReduceFillAdvancesBasis(t *testing.T) {
	s := newHedgeState()
	sc := hedgeSC(1.0)
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 40000, Side: "short", Multiplier: 1,
		OwnerStrategyID: s.ID, HedgeFor: "ETH", HedgePrimaryQtyBasis: 10,
	}
	// Primary reduced 10 → 6; hedge reduce 0.2 (frac 0.4), target basis 6.
	decision := hedgeAction{Kind: hedgeActionReduce, Qty: 0.2, Side: "buy", TargetBasis: 6}
	snap := hedgeSnapshot{HedgeQty: 0.5, HedgeSide: "short", HedgeBasis: 10}
	applyHedgeReduceOrCloseFill(sc, s, "BTC", "ETH", decision, snap, 0.2, 40000, 0, false, 0, false, nil)
	pos := s.Positions["BTC"]
	if pos == nil {
		t.Fatal("hedge Position should survive a partial reduce")
	}
	if math.Abs(pos.Quantity-0.3) > 1e-9 {
		t.Errorf("reduced qty: want 0.3, got %g", pos.Quantity)
	}
	if math.Abs(pos.HedgePrimaryQtyBasis-6) > 1e-9 {
		t.Errorf("advanced basis: want 6, got %g", pos.HedgePrimaryQtyBasis)
	}
}

func TestHedgeCloseSkipsDiagnostics(t *testing.T) {
	var rows []TradeDiagnosticsRow
	tradeDiagnosticsRecorder = func(row *TradeDiagnosticsRow) error {
		rows = append(rows, *row)
		return nil
	}
	defer func() { tradeDiagnosticsRecorder = nil }()

	s := newHedgeState()
	// A hedge close must NOT produce a diagnostics row.
	s.Positions["BTC"] = &Position{
		Symbol: "BTC", Quantity: 0.5, InitialQuantity: 0.5, AvgCost: 40000, Side: "short",
		Multiplier: 1, OwnerStrategyID: s.ID, HedgeFor: "ETH", TradePositionID: "hp1",
	}
	bookPerpsCloseWithFillFee(s, "BTC", 41000, 0, false, "", "hedge_close", "hedge(ETH) close", "hedge close", nil)
	if len(rows) != 0 {
		t.Errorf("hedge close must not record trade diagnostics, got %d rows", len(rows))
	}
	// A normal perps close MUST produce a diagnostics row (control).
	s.Positions["ETH"] = &Position{
		Symbol: "ETH", Quantity: 1, InitialQuantity: 1, AvgCost: 2000, Side: "long",
		Multiplier: 1, OwnerStrategyID: s.ID, TradePositionID: "pp1",
	}
	bookPerpsCloseWithFillFee(s, "ETH", 2100, 0, false, "", "close", "Close", "close", nil)
	if len(rows) != 1 {
		t.Errorf("normal perps close should record 1 diagnostics row, got %d", len(rows))
	}
}

func TestValidateHedgeStateConsistencyDetectsOrphan(t *testing.T) {
	state := &AppState{Strategies: map[string]*StrategyState{
		"hl-eth": {
			ID:        "hl-eth",
			Positions: map[string]*Position{"BTC": {Symbol: "BTC", Quantity: 0.5, HedgeFor: "ETH"}},
		},
	}}
	// Config no longer enables a hedge on hl-eth.
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Args: []string{"check_hyperliquid.py", "ETH", "live"}},
	}}
	warnings := validateHedgeStateConsistency(state, cfg)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 orphan-hedge warning, got %d: %v", len(warnings), warnings)
	}
	// Now enable a hedge but on a DIFFERENT coin than the persisted leg.
	cfg.Strategies[0].Hedge = &HedgeConfig{Enabled: true, Symbol: "SOL"}
	warnings = validateHedgeStateConsistency(state, cfg)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 coin-mismatch warning, got %d: %v", len(warnings), warnings)
	}
	// Matching config → no warning.
	cfg.Strategies[0].Hedge.Symbol = "BTC"
	if w := validateHedgeStateConsistency(state, cfg); len(w) != 0 {
		t.Errorf("matching hedge config should produce no warning, got %v", w)
	}
}

func TestHedgeSnapshotForStrategy(t *testing.T) {
	s := newHedgeState()
	s.Positions["ETH"] = &Position{Symbol: "ETH", Quantity: 10, Side: "long"}
	s.Positions["BTC"] = &Position{Symbol: "BTC", Quantity: 0.5, Side: "short", HedgeFor: "ETH", HedgePrimaryQtyBasis: 10}
	sc := hedgeSC(1.0)
	snap := hedgeSnapshotForStrategy(s, sc)
	if snap.PrimaryQty != 10 || snap.PrimarySide != "long" {
		t.Errorf("primary snapshot wrong: %+v", snap)
	}
	if snap.HedgeQty != 0.5 || snap.HedgeSide != "short" || snap.HedgeBasis != 10 {
		t.Errorf("hedge snapshot wrong: %+v", snap)
	}
}
