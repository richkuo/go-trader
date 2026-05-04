package main

import (
	"fmt"
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
