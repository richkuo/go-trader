package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestResolveManualSize checks the three sizing modes.
func TestResolveManualSize(t *testing.T) {
	cases := []struct {
		size, notional, margin, price, leverage float64
		want                                    float64
	}{
		{size: 0.5, notional: 0, margin: 0, price: 2000, leverage: 10, want: 0.5},
		{size: 0, notional: 1000, margin: 0, price: 2000, leverage: 10, want: 0.5},
		{size: 0, notional: 0, margin: 100, price: 2000, leverage: 10, want: 0.5},
		{size: 0, notional: 0, margin: 0, price: 2000, leverage: 10, want: 0}, // no input
		{size: 0, notional: 500, margin: 0, price: 0, leverage: 10, want: 0},  // price=0
	}
	for _, c := range cases {
		got := resolveManualSize(c.size, c.notional, c.margin, c.price, c.leverage)
		if fmt.Sprintf("%.6f", got) != fmt.Sprintf("%.6f", c.want) {
			t.Errorf("resolveManualSize(size=%g,notional=%g,margin=%g,price=%g,lev=%g) = %g, want %g",
				c.size, c.notional, c.margin, c.price, c.leverage, got, c.want)
		}
	}
}

func TestCountSizingFlags(t *testing.T) {
	if countSizingFlags(1, 0, 0) != 1 {
		t.Error("size only should be 1")
	}
	if countSizingFlags(0, 500, 0) != 1 {
		t.Error("notional only should be 1")
	}
	if countSizingFlags(1, 500, 0) != 2 {
		t.Error("size+notional should be 2")
	}
	if countSizingFlags(1, 500, 100) != 3 {
		t.Error("all three should be 3")
	}
	if countSizingFlags(0, 0, 0) != 0 {
		t.Error("none should be 0")
	}
}

func TestOpenTradeSide(t *testing.T) {
	if openTradeSide("long") != "buy" {
		t.Error("long should map to buy")
	}
	if openTradeSide("short") != "sell" {
		t.Error("short should map to sell")
	}
}

// TestApplyManualActionOpen verifies that an open action creates a Position and Trade.
func TestApplyManualActionOpen(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-manual-eth-live": {
				ID:        "hl-manual-eth-live",
				Platform:  "hyperliquid",
				Type:      "manual",
				Positions: map[string]*Position{},
				Cash:      10000,
			},
		},
	}
	scByID := map[string]StrategyConfig{
		"hl-manual-eth-live": {
			ID:       "hl-manual-eth-live",
			Type:     "manual",
			Platform: "hyperliquid",
			Symbol:   "ETH",
			Leverage: 10,
		},
	}
	now := time.Now().UTC()
	a := PendingManualAction{
		ID:         1,
		StrategyID: "hl-manual-eth-live",
		Action:     "open",
		Symbol:     "ETH",
		Side:       "long",
		Quantity:   0.5,
		FillPrice:  2000,
		FillFee:    0.7,
		EntryATR:   50,
		CreatedAt:  now,
	}

	var recorded []Trade
	origRecorder := tradeRecorder
	tradeRecorder = func(stratID string, trade Trade) error {
		recorded = append(recorded, trade)
		return nil
	}
	defer func() { tradeRecorder = origRecorder }()

	if err := applyManualAction(state, scByID, a); err != nil {
		t.Fatalf("applyManualAction open: %v", err)
	}

	ss := state.Strategies["hl-manual-eth-live"]
	pos := ss.Positions["ETH"]
	if pos == nil {
		t.Fatal("expected position to be created")
	}
	if pos.Quantity != 0.5 {
		t.Errorf("pos.Quantity = %g, want 0.5", pos.Quantity)
	}
	if pos.AvgCost != 2000 {
		t.Errorf("pos.AvgCost = %g, want 2000", pos.AvgCost)
	}
	if pos.Side != "long" {
		t.Errorf("pos.Side = %q, want \"long\"", pos.Side)
	}
	if pos.EntryATR != 50 {
		t.Errorf("pos.EntryATR = %g, want 50", pos.EntryATR)
	}
	if !pos.OpenedAt.Equal(now) {
		t.Errorf("pos.OpenedAt = %v, want %v", pos.OpenedAt, now)
	}
	if pos.TradePositionID == "" {
		t.Error("expected TradePositionID to be set")
	}

	if len(recorded) != 1 {
		t.Fatalf("expected 1 trade recorded, got %d", len(recorded))
	}
	tr := recorded[0]
	if !tr.Manual {
		t.Error("expected trade.Manual = true")
	}
	if tr.Side != "buy" {
		t.Errorf("trade.Side = %q, want \"buy\"", tr.Side)
	}
	if tr.EntryATR != 50 {
		t.Errorf("trade.EntryATR = %g, want 50", tr.EntryATR)
	}
}

// TestApplyManualActionClose verifies that a close action records a closing trade and removes the position.
func TestApplyManualActionClose(t *testing.T) {
	openAt := time.Now().UTC().Add(-time.Hour)
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-manual-eth-live": {
				ID:       "hl-manual-eth-live",
				Platform: "hyperliquid",
				Type:     "manual",
				Positions: map[string]*Position{
					"ETH": {
						Symbol:          "ETH",
						Quantity:        0.5,
						InitialQuantity: 0.5,
						AvgCost:         2000,
						Side:            "long",
						Multiplier:      1,
						Leverage:        10,
						OwnerStrategyID: "hl-manual-eth-live",
						OpenedAt:        openAt,
					},
				},
				Cash: 9000, // after open deduction
			},
		},
	}
	scByID := map[string]StrategyConfig{
		"hl-manual-eth-live": {
			ID:       "hl-manual-eth-live",
			Type:     "manual",
			Platform: "hyperliquid",
			Symbol:   "ETH",
			Leverage: 10,
		},
	}

	var recorded []Trade
	origRecorder := tradeRecorder
	tradeRecorder = func(stratID string, trade Trade) error {
		recorded = append(recorded, trade)
		return nil
	}
	defer func() { tradeRecorder = origRecorder }()

	now := time.Now().UTC()
	a := PendingManualAction{
		ID:          2,
		StrategyID:  "hl-manual-eth-live",
		Action:      "close",
		Symbol:      "ETH",
		Side:        "sell",
		Quantity:    0.5,
		FillPrice:   2100,
		FillFee:     0.7,
		RealizedPnL: 49.3, // 0.5*(2100-2000) - 0.7
		IsFullClose: true,
		CreatedAt:   now,
	}
	if err := applyManualAction(state, scByID, a); err != nil {
		t.Fatalf("applyManualAction close: %v", err)
	}

	ss := state.Strategies["hl-manual-eth-live"]
	if _, exists := ss.Positions["ETH"]; exists {
		t.Error("expected position to be removed after full close")
	}

	if len(recorded) != 1 {
		t.Fatalf("expected 1 trade recorded, got %d", len(recorded))
	}
	tr := recorded[0]
	if !tr.IsClose {
		t.Error("expected trade.IsClose = true")
	}
	if !tr.Manual {
		t.Error("expected trade.Manual = true")
	}
	if tr.Side != "sell" {
		t.Errorf("trade.Side = %q, want \"sell\"", tr.Side)
	}

	if len(ss.ClosedPositions) != 1 {
		t.Errorf("expected 1 closed position, got %d", len(ss.ClosedPositions))
	}
}

// TestApplyManualActionPartialClose verifies that partial close decrements quantity without removing the position.
func TestApplyManualActionPartialClose(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-manual-eth-live": {
				ID:       "hl-manual-eth-live",
				Platform: "hyperliquid",
				Type:     "manual",
				Positions: map[string]*Position{
					"ETH": {
						Symbol:          "ETH",
						Quantity:        1.0,
						InitialQuantity: 1.0,
						AvgCost:         2000,
						Side:            "long",
						Multiplier:      1,
						OwnerStrategyID: "hl-manual-eth-live",
					},
				},
				Cash: 8000,
			},
		},
	}
	scByID := map[string]StrategyConfig{
		"hl-manual-eth-live": {ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 10},
	}

	origRecorder := tradeRecorder
	tradeRecorder = func(_ string, _ Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()

	a := PendingManualAction{
		StrategyID:  "hl-manual-eth-live",
		Action:      "close",
		Symbol:      "ETH",
		Side:        "sell",
		Quantity:    0.4, // partial
		FillPrice:   2100,
		RealizedPnL: 40,
		CreatedAt:   time.Now().UTC(),
	}
	if err := applyManualAction(state, scByID, a); err != nil {
		t.Fatalf("partial close: %v", err)
	}

	pos := state.Strategies["hl-manual-eth-live"].Positions["ETH"]
	if pos == nil {
		t.Fatal("position should remain after partial close")
	}
	if fmt.Sprintf("%.4f", pos.Quantity) != "0.6000" {
		t.Errorf("pos.Quantity after partial close = %g, want 0.6", pos.Quantity)
	}
}

func TestApplyManualActionCloseRejectsOwnerMismatch(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-manual-eth-live": {
				ID:       "hl-manual-eth-live",
				Platform: "hyperliquid",
				Type:     "manual",
				Positions: map[string]*Position{
					"ETH": {
						Symbol:          "ETH",
						Quantity:        1,
						AvgCost:         2000,
						Side:            "long",
						OwnerStrategyID: "hl-other-eth-live",
					},
				},
				Cash: 10000,
			},
		},
	}
	scByID := map[string]StrategyConfig{
		"hl-manual-eth-live": {ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 10},
	}

	origRecorder := tradeRecorder
	tradeRecorder = func(_ string, _ Trade) error {
		t.Fatal("tradeRecorder should not be called for owner mismatch")
		return nil
	}
	defer func() { tradeRecorder = origRecorder }()

	err := applyManualAction(state, scByID, PendingManualAction{
		StrategyID:  "hl-manual-eth-live",
		Action:      "close",
		Symbol:      "ETH",
		Quantity:    1,
		FillPrice:   2100,
		RealizedPnL: 100,
		IsFullClose: true,
		CreatedAt:   time.Now().UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "owned by") {
		t.Fatalf("expected owner mismatch error, got: %v", err)
	}
	if pos := state.Strategies["hl-manual-eth-live"].Positions["ETH"]; pos == nil || pos.Quantity != 1 {
		t.Fatalf("position should remain untouched, got %#v", pos)
	}
}

// TestApplyManualAction99PercentPartialNotCollapsedToFull verifies that a
// deliberate ~99% partial close is NOT collapsed into a full close (the prior
// 0.99 relative tolerance would silently delete the residual dust).
func TestApplyManualAction99PercentPartialNotCollapsedToFull(t *testing.T) {
	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-manual-eth-live": {
				ID:       "hl-manual-eth-live",
				Platform: "hyperliquid",
				Type:     "manual",
				Positions: map[string]*Position{
					"ETH": {
						Symbol:          "ETH",
						Quantity:        0.5,
						InitialQuantity: 0.5,
						AvgCost:         2000,
						Side:            "long",
						Multiplier:      1,
						OwnerStrategyID: "hl-manual-eth-live",
					},
				},
				Cash: 9000,
			},
		},
	}
	scByID := map[string]StrategyConfig{
		"hl-manual-eth-live": {ID: "hl-manual-eth-live", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 10},
	}

	origRecorder := tradeRecorder
	tradeRecorder = func(_ string, _ Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()

	a := PendingManualAction{
		StrategyID:  "hl-manual-eth-live",
		Action:      "close",
		Symbol:      "ETH",
		Side:        "sell",
		Quantity:    0.495, // 99% of 0.5 — exactly at the prior tolerance boundary
		FillPrice:   2100,
		RealizedPnL: 49.0,
		IsFullClose: false, // explicit partial-close intent
		CreatedAt:   time.Now().UTC(),
	}
	if err := applyManualAction(state, scByID, a); err != nil {
		t.Fatalf("99%% partial close: %v", err)
	}

	pos := state.Strategies["hl-manual-eth-live"].Positions["ETH"]
	if pos == nil {
		t.Fatal("99%% partial close should leave the position open with dust qty (regression: 0.99 tolerance was collapsing this to full)")
	}
	expectedQty := 0.5 - 0.495
	if abs(pos.Quantity-expectedQty) > 1e-9 {
		t.Errorf("residual qty = %g, want %g", pos.Quantity, expectedQty)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestDrainPendingManualActions verifies the queue drain applies actions and cleans up.
func TestDrainPendingManualActions(t *testing.T) {
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer db.Close()

	stratID := "hl-manual-eth-live"
	state := &AppState{
		Strategies: map[string]*StrategyState{
			stratID: {
				ID:        stratID,
				Platform:  "hyperliquid",
				Type:      "manual",
				Positions: map[string]*Position{},
				Cash:      10000,
			},
		},
	}
	cfg := &Config{
		Strategies: []StrategyConfig{{
			ID: stratID, Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 10,
		}},
	}

	origRecorder := tradeRecorder
	tradeRecorder = func(_ string, _ Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()

	_ = db.InsertPendingManualAction(PendingManualAction{
		StrategyID: stratID, Action: "open", Symbol: "ETH", Side: "long",
		Quantity: 0.5, FillPrice: 2000, FillFee: 0.7, EntryATR: 50,
		CreatedAt: time.Now().UTC(),
	})

	drainPendingManualActions(state, cfg, db)

	pos := state.Strategies[stratID].Positions["ETH"]
	if pos == nil {
		t.Fatal("expected position after drain")
	}
	if pos.Quantity != 0.5 {
		t.Errorf("pos.Quantity = %g, want 0.5", pos.Quantity)
	}

	// Queue should be empty after drain.
	remaining, _ := db.LoadPendingManualActions()
	if len(remaining) != 0 {
		t.Errorf("expected empty queue after drain, got %d rows", len(remaining))
	}
}

// TestManualPositionOwnedByStrategy covers the owner guard the CLI runManualClose
// path, the drain path (applyManualAction), and the main-loop manual case all
// share. Empty OwnerStrategyID is intentionally treated as owned for backward
// compat with positions opened pre-#569 / discovered by the reconciler.
func TestManualPositionOwnedByStrategy(t *testing.T) {
	cases := []struct {
		name     string
		pos      *Position
		strategy string
		want     bool
	}{
		{"nil pos is treated as owned (no-op)", nil, "hl-manual-eth", true},
		{"empty owner is treated as owned (legacy/reconciler)", &Position{}, "hl-manual-eth", true},
		{"matching owner", &Position{OwnerStrategyID: "hl-manual-eth"}, "hl-manual-eth", true},
		{"mismatched owner is rejected", &Position{OwnerStrategyID: "hl-other-eth"}, "hl-manual-eth", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := manualPositionOwnedByStrategy(tc.pos, tc.strategy); got != tc.want {
				t.Errorf("manualPositionOwnedByStrategy(%+v, %q) = %v, want %v",
					tc.pos, tc.strategy, got, tc.want)
			}
		})
	}
}

// TestPendingManualActionTPOIDsRoundtrip verifies that TPOIDs survive an
// InsertPendingManualAction → LoadPendingManualActions round-trip (#632).
func TestPendingManualActionTPOIDsRoundtrip(t *testing.T) {
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer db.Close()

	want := []int64{111, 222, 333}
	if err := db.InsertPendingManualAction(PendingManualAction{
		StrategyID: "hl-manual-eth-live", Action: "open", Symbol: "ETH", Side: "long",
		Quantity: 0.8, FillPrice: 2500, FillFee: 0.35, EntryATR: 12.5,
		TPOIDs:    want,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	actions, err := db.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	got := actions[0].TPOIDs
	if len(got) != len(want) {
		t.Fatalf("TPOIDs len=%d want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("TPOIDs[%d]=%d want %d", i, got[i], want[i])
		}
	}
}

// TestApplyManualAction_OpenSetsTPOIDs verifies that applyManualAction stamps
// TPOIDs onto the materialised position (#632).
func TestApplyManualAction_OpenSetsTPOIDs(t *testing.T) {
	db, err := OpenStateDB(":memory:")
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	defer db.Close()

	stratID := "hl-manual-eth-live"
	state := &AppState{
		Strategies: map[string]*StrategyState{
			stratID: {
				ID:        stratID,
				Type:      "manual",
				Platform:  "hyperliquid",
				Positions: map[string]*Position{},
				Cash:      1000,
			},
		},
	}
	cfg := &Config{
		Strategies: []StrategyConfig{{
			ID: stratID, Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Leverage: 20,
		}},
	}

	origRecorder := tradeRecorder
	tradeRecorder = func(_ string, _ Trade) error { return nil }
	defer func() { tradeRecorder = origRecorder }()

	wantOIDs := []int64{2001, 2002}
	_ = db.InsertPendingManualAction(PendingManualAction{
		StrategyID: stratID, Action: "open", Symbol: "ETH", Side: "long",
		Quantity: 0.8, FillPrice: 2500, FillFee: 0.875, EntryATR: 12.5,
		TPOIDs:    wantOIDs,
		CreatedAt: time.Now().UTC(),
	})

	drainPendingManualActions(state, cfg, db)

	pos := state.Strategies[stratID].Positions["ETH"]
	if pos == nil {
		t.Fatal("expected position after drain")
	}
	if len(pos.TPOIDs) != len(wantOIDs) {
		t.Fatalf("pos.TPOIDs len=%d want %d (got=%v)", len(pos.TPOIDs), len(wantOIDs), pos.TPOIDs)
	}
	for i, oid := range wantOIDs {
		if pos.TPOIDs[i] != oid {
			t.Errorf("pos.TPOIDs[%d]=%d want %d", i, pos.TPOIDs[i], oid)
		}
	}
}
