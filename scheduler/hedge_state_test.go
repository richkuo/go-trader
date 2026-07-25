package main

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hedgeTestStrategyState returns a minimal HL perps strategy state holding a
// primary ETH long plus its inverse hedge leg on BTC.
func hedgeTestStrategyState() *StrategyState {
	return &StrategyState{
		ID:       "hl-eth",
		Type:     "perps",
		Platform: "hyperliquid",
		Cash:     1000,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: 1.5, InitialQuantity: 1.5, AvgCost: 3000,
				Side: "long", Multiplier: 1, Leverage: 1,
				TradePositionID: "hl-eth-ETH-open-1",
			},
			"BTC": {
				Symbol: "BTC", Quantity: 0.05, InitialQuantity: 0.05, AvgCost: 90000,
				Side: "short", Multiplier: 1, Leverage: 1,
				TradePositionID: "hl-eth-BTC-open-1",
				HedgeFor:        "ETH", HedgePrimaryQtyBasis: 1.5,
			},
		},
		OptionPositions: map[string]*OptionPosition{},
	}
}

func TestPositionHedgeFields_DBRoundTrip(t *testing.T) {
	db := openTestDB(t)
	state := NewAppState()
	state.Strategies["hl-eth"] = hedgeTestStrategyState()

	if err := db.SaveState(state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := db.LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	s := loaded.Strategies["hl-eth"]
	if s == nil {
		t.Fatal("missing strategy hl-eth after load")
	}
	hedge := s.Positions["BTC"]
	if hedge == nil {
		t.Fatal("missing hedge position BTC after load")
	}
	if hedge.HedgeFor != "ETH" {
		t.Errorf("HedgeFor = %q, want ETH", hedge.HedgeFor)
	}
	if hedge.HedgePrimaryQtyBasis != 1.5 {
		t.Errorf("HedgePrimaryQtyBasis = %g, want 1.5", hedge.HedgePrimaryQtyBasis)
	}

	// The primary leg round-trips with zero-value hedge fields.
	primary := s.Positions["ETH"]
	if primary == nil {
		t.Fatal("missing primary position ETH after load")
	}
	if primary.HedgeFor != "" || primary.HedgePrimaryQtyBasis != 0 {
		t.Errorf("primary hedge fields = (%q, %g), want zero values", primary.HedgeFor, primary.HedgePrimaryQtyBasis)
	}
}

// TestPositionHedgeFields_MigrationIdempotent verifies the ALTER TABLE
// migrations apply cleanly on a fresh DB and survive a re-open (the
// duplicate-column skip path).
func TestPositionHedgeFields_MigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenStateDB(path)
	if err != nil {
		t.Fatalf("re-open (migrations re-run) failed: %v", err)
	}
	defer reopened.Close()

	// The new columns are queryable on a re-opened DB.
	if _, err := reopened.db.Exec(`UPDATE positions SET hedge_for = '', hedge_primary_qty_basis = 0 WHERE 1 = 0`); err != nil {
		t.Fatalf("hedge columns missing after migration: %v", err)
	}
}

func TestLifetimeTradeStats_HedgeExcluded(t *testing.T) {
	sdb := openTestDB(t)
	now := time.Now().UTC()
	trades := []Trade{
		// One normal round-trip (win).
		{StrategyID: "s1", Timestamp: now, Symbol: "ETH", PositionID: "s1-ETH-open-1", Side: "buy", Quantity: 1, Price: 3000, Value: 3000, TradeType: "perps", Details: "Open long"},
		{StrategyID: "s1", Timestamp: now.Add(time.Second), Symbol: "ETH", PositionID: "s1-ETH-open-1", Side: "sell", Quantity: 1, Price: 3100, Value: 3100, TradeType: "perps", Details: "Close long, PnL: $100.00", IsClose: true, RealizedPnL: 100},
		// The coupled hedge round-trip (loss — hedges lose when the primary
		// wins). trade_type "hedge" rows must not count toward #T or W/L.
		{StrategyID: "s1", Timestamp: now.Add(2 * time.Second), Symbol: "BTC", PositionID: "s1-BTC-open-1", Side: "sell", Quantity: 0.05, Price: 90000, Value: 4500, TradeType: "hedge", Details: "hedge(ETH) open short"},
		{StrategyID: "s1", Timestamp: now.Add(3 * time.Second), Symbol: "BTC", PositionID: "s1-BTC-open-1", Side: "buy", Quantity: 0.05, Price: 91000, Value: 4550, TradeType: "hedge", Details: "hedge(ETH) close, PnL: $-50.00", IsClose: true, RealizedPnL: -50},
	}
	for _, tr := range trades {
		if err := sdb.InsertTrade(tr.StrategyID, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	stats, err := sdb.LifetimeTradeStatsAll()
	if err != nil {
		t.Fatalf("LifetimeTradeStatsAll: %v", err)
	}
	if got := stats["s1"]; got.PositionsOpened != 1 || got.Wins != 1 || got.Losses != 0 {
		t.Errorf("stats = %+v, want PositionsOpened=1 Wins=1 Losses=0 (hedge legs excluded)", got)
	}
	if got, err := sdb.LifetimeTradeStatsForStrategy("s1"); err != nil {
		t.Fatalf("LifetimeTradeStatsForStrategy: %v", err)
	} else if got.PositionsOpened != 1 || got.Wins != 1 || got.Losses != 0 {
		t.Errorf("single-strategy stats = %+v, want PositionsOpened=1 Wins=1 Losses=0 (hedge legs excluded)", got)
	}
}

func TestRecordHedgeTradeResult(t *testing.T) {
	r := RiskState{}

	// Hedge loss: DailyPnL moves, loss streak does NOT.
	RecordHedgeTradeResult(&r, -50)
	if r.DailyPnL != -50 {
		t.Errorf("DailyPnL = %g, want -50", r.DailyPnL)
	}
	if r.ConsecutiveLosses != 0 {
		t.Errorf("ConsecutiveLosses = %d, want 0 (hedge legs never feed the streak)", r.ConsecutiveLosses)
	}

	// A primary loss feeds both — proves the two accumulators stay distinct.
	RecordTradeResult(&r, -10)
	if r.DailyPnL != -60 {
		t.Errorf("DailyPnL = %g, want -60", r.DailyPnL)
	}
	if r.ConsecutiveLosses != 1 {
		t.Errorf("ConsecutiveLosses = %d, want 1", r.ConsecutiveLosses)
	}

	// A hedge WIN does not reset the primary's loss streak either — the
	// streak tracks the primary thesis only.
	RecordHedgeTradeResult(&r, 20)
	if r.DailyPnL != -40 {
		t.Errorf("DailyPnL = %g, want -40", r.DailyPnL)
	}
	if r.ConsecutiveLosses != 1 {
		t.Errorf("ConsecutiveLosses = %d, want 1 (hedge win must not reset the streak)", r.ConsecutiveLosses)
	}
}

func TestRecordCloseTradeResultRouting(t *testing.T) {
	s := &StrategyState{ID: "hl-eth"}
	hedgePos := &Position{Symbol: "BTC", HedgeFor: "ETH"}
	normalPos := &Position{Symbol: "ETH"}

	recordCloseTradeResult(s, hedgePos, -25)
	if s.RiskState.DailyPnL != -25 || s.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("hedge route: DailyPnL=%g streak=%d, want -25/0", s.RiskState.DailyPnL, s.RiskState.ConsecutiveLosses)
	}
	recordCloseTradeResult(s, normalPos, -5)
	if s.RiskState.DailyPnL != -30 || s.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("normal route: DailyPnL=%g streak=%d, want -30/1", s.RiskState.DailyPnL, s.RiskState.ConsecutiveLosses)
	}
	// Nil position defensively routes to the primary accumulator.
	recordCloseTradeResult(s, nil, -1)
	if s.RiskState.ConsecutiveLosses != 2 {
		t.Errorf("nil pos should route to RecordTradeResult, streak=%d want 2", s.RiskState.ConsecutiveLosses)
	}
}

// TestBookPerpsCloseWithFillFee_HedgeRouting verifies the close-booking site
// routes hedge PnL to DailyPnL only, never to the consecutive-loss streak.
func TestBookPerpsCloseWithFillFee_HedgeRouting(t *testing.T) {
	s := hedgeTestStrategyState()

	// Close the hedge leg at an adverse mark (loss): booked into DailyPnL,
	// streak untouched, position removed.
	delete(s.Positions, "ETH") // isolate the hedge leg for this test
	before := s.Cash
	ok := bookPerpsCloseWithFillFee(s, "BTC", 92000, 0, false, "", "hedge_close", "Hedge close", "Hedge close", nil)
	if !ok {
		t.Fatal("bookPerpsCloseWithFillFee returned false")
	}
	if _, exists := s.Positions["BTC"]; exists {
		t.Error("hedge position should be deleted after full close")
	}
	// short 0.05 @ 90000 closed at 92000 → gross -100, minus modeled fee.
	wantPnL := s.Cash - before
	if math.Abs(s.RiskState.DailyPnL-wantPnL) > 1e-9 {
		t.Errorf("DailyPnL = %g, want booked pnl %g", s.RiskState.DailyPnL, wantPnL)
	}
	if s.RiskState.ConsecutiveLosses != 0 {
		t.Errorf("ConsecutiveLosses = %d, want 0 for a hedge-leg loss", s.RiskState.ConsecutiveLosses)
	}

	// Control: the same close on a NON-hedge position feeds the streak.
	s2 := hedgeTestStrategyState()
	delete(s2.Positions, "BTC")
	ok = bookPerpsCloseWithFillFee(s2, "ETH", 2900, 0, false, "", "close", "Close", "Close", nil)
	if !ok {
		t.Fatal("bookPerpsCloseWithFillFee (control) returned false")
	}
	if s2.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("control ConsecutiveLosses = %d, want 1", s2.RiskState.ConsecutiveLosses)
	}
}

// TestForceCloseAllPositions_HedgeRouting verifies the circuit-breaker sweep
// keeps hedge legs out of the loss streak while still booking DailyPnL.
func TestForceCloseAllPositions_HedgeRouting(t *testing.T) {
	s := hedgeTestStrategyState()
	prices := map[string]float64{"ETH": 2900, "BTC": 92000}
	forceCloseAllPositions(s, prices, nil)
	if len(s.Positions) != 0 {
		t.Fatalf("all positions should be closed, still have: %v", s.Positions)
	}
	// ETH long: 1.5*(2900-3000) = -150 (feeds streak). BTC hedge short:
	// 0.05*(90000-92000) = -100 (DailyPnL only).
	if s.RiskState.DailyPnL != -250 {
		t.Errorf("DailyPnL = %g, want -250", s.RiskState.DailyPnL)
	}
	if s.RiskState.ConsecutiveLosses != 1 {
		t.Errorf("ConsecutiveLosses = %d, want 1 (primary leg only)", s.RiskState.ConsecutiveLosses)
	}
}

// TestValidatePerpsDirectionConfig_HedgeExempt verifies inverse hedge legs
// don't trip the perps state-vs-config startup warning (#1159).
func TestValidatePerpsDirectionConfig_HedgeExempt(t *testing.T) {
	state := NewAppState()
	state.Strategies["hl-eth"] = &StrategyState{
		ID:   "hl-eth",
		Type: "perps",
		Positions: map[string]*Position{
			// Inverse hedge leg: short under direction="long" BY CONSTRUCTION.
			"BTC": {Symbol: "BTC", Quantity: 0.05, AvgCost: 90000, Side: "short", Multiplier: 1, Leverage: 1, HedgeFor: "ETH"},
		},
	}
	// Control strategy: identical shape but NOT marked as a hedge — must warn.
	state.Strategies["hl-sol"] = &StrategyState{
		ID:   "hl-sol",
		Type: "perps",
		Positions: map[string]*Position{
			"SOL": {Symbol: "SOL", Quantity: 1, AvgCost: 200, Side: "short", Multiplier: 1, Leverage: 1},
		},
	}
	cfg := &Config{Strategies: []StrategyConfig{
		{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong},
		{ID: "hl-sol", Type: "perps", Platform: "hyperliquid", Direction: DirectionLong},
	}}

	warnings := ValidatePerpsDirectionConfig(state, cfg)
	if len(warnings) != 1 {
		t.Fatalf("want exactly 1 warning (control only), got %d: %v", len(warnings), warnings)
	}
	if strings.Contains(warnings[0], "hl-eth") {
		t.Errorf("hedge leg must not warn, got: %s", warnings[0])
	}
	if !strings.Contains(warnings[0], "hl-sol") {
		t.Errorf("control position should warn, got: %s", warnings[0])
	}
}

func TestValidateHedgeStateConsistency(t *testing.T) {
	hedgerCfg := func(hedge *HedgeConfig) StrategyConfig {
		return StrategyConfig{
			ID: "hl-eth", Type: "perps", Platform: "hyperliquid",
			Args: []string{"sma_crossover", "ETH", "1h"}, Hedge: hedge,
		}
	}

	t.Run("matching block is silent", func(t *testing.T) {
		state := NewAppState()
		state.Strategies["hl-eth"] = hedgeTestStrategyState()
		cfg := &Config{Strategies: []StrategyConfig{hedgerCfg(&HedgeConfig{Enabled: true, Symbol: "BTC"})}}
		if warnings := validateHedgeStateConsistency(state, cfg); len(warnings) != 0 {
			t.Errorf("want no warnings for matching hedge block, got: %v", warnings)
		}
	})

	t.Run("block removed warns, position untouched", func(t *testing.T) {
		state := NewAppState()
		state.Strategies["hl-eth"] = hedgeTestStrategyState()
		cfg := &Config{Strategies: []StrategyConfig{hedgerCfg(nil)}}
		warnings := validateHedgeStateConsistency(state, cfg)
		if len(warnings) != 1 {
			t.Fatalf("want 1 warning for removed hedge block, got: %v", warnings)
		}
		if !strings.Contains(warnings[0], "hl-eth") || !strings.Contains(warnings[0], "no enabled hedge block") {
			t.Errorf("unexpected message: %s", warnings[0])
		}
		if _, ok := state.Strategies["hl-eth"].Positions["BTC"]; !ok {
			t.Error("hedge position must be left frozen (non-destructive)")
		}
	})

	t.Run("block disabled warns", func(t *testing.T) {
		state := NewAppState()
		state.Strategies["hl-eth"] = hedgeTestStrategyState()
		cfg := &Config{Strategies: []StrategyConfig{hedgerCfg(&HedgeConfig{Enabled: false, Symbol: "BTC"})}}
		if warnings := validateHedgeStateConsistency(state, cfg); len(warnings) != 1 {
			t.Fatalf("want 1 warning for disabled hedge block, got: %v", warnings)
		}
	})

	t.Run("coin mismatch warns", func(t *testing.T) {
		state := NewAppState()
		state.Strategies["hl-eth"] = hedgeTestStrategyState()
		cfg := &Config{Strategies: []StrategyConfig{hedgerCfg(&HedgeConfig{Enabled: true, Symbol: "SOL"})}}
		warnings := validateHedgeStateConsistency(state, cfg)
		if len(warnings) != 1 {
			t.Fatalf("want 1 warning for coin mismatch, got: %v", warnings)
		}
		if !strings.Contains(warnings[0], "BTC") || !strings.Contains(warnings[0], "SOL") {
			t.Errorf("message should name persisted and configured coins, got: %s", warnings[0])
		}
	})

	t.Run("non-hedge positions ignored", func(t *testing.T) {
		state := NewAppState()
		s := hedgeTestStrategyState()
		delete(s.Positions, "BTC")
		state.Strategies["hl-eth"] = s
		cfg := &Config{Strategies: []StrategyConfig{hedgerCfg(nil)}}
		if warnings := validateHedgeStateConsistency(state, cfg); len(warnings) != 0 {
			t.Errorf("plain positions must not warn, got: %v", warnings)
		}
	})

	t.Run("nil inputs tolerated", func(t *testing.T) {
		if warnings := validateHedgeStateConsistency(nil, nil); len(warnings) != 0 {
			t.Errorf("nil inputs should produce no warnings, got: %v", warnings)
		}
	})
}

// TestRecordClosedPosition_SkipsDiagnosticsForHedge pins the #1147 boundary:
// hedge round-trips must not pollute per-strategy trade-quality aggregates.
func TestRecordClosedPosition_SkipsDiagnosticsForHedge(t *testing.T) {
	var captured []string
	prevRecorder := tradeDiagnosticsRecorder
	tradeDiagnosticsRecorder = func(row *TradeDiagnosticsRow) error {
		captured = append(captured, row.Symbol)
		return nil
	}
	defer func() { tradeDiagnosticsRecorder = prevRecorder }()

	s := hedgeTestStrategyState()
	now := time.Now().UTC()

	hedgePos := s.Positions["BTC"]
	recordClosedPosition(s, hedgePos, 92000, -100, "hedge_close", now)
	if len(captured) != 0 {
		t.Errorf("hedge close must skip captureTradeDiagnostics, captured: %v", captured)
	}

	// Control: the primary leg still captures.
	primaryPos := s.Positions["ETH"]
	recordClosedPosition(s, primaryPos, 2900, -150, "close", now)
	if len(captured) != 1 || captured[0] != "ETH" {
		t.Errorf("primary close should capture diagnostics exactly once, got: %v", captured)
	}
}
