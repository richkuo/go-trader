package main

import (
	"context"
	"sync"
	"testing"
)

func TestRunRegimeDirectionOrphanCloses_AlreadyFlatLeavesVirtual(t *testing.T) {
	sc := StrategyConfig{
		ID:       "hl-vwap-btc",
		Type:     "perps",
		Platform: "hyperliquid",
		Args:     []string{"vwap", "BTC", "1h", "--mode=live"},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			sc.ID: {
				ID:       sc.ID,
				Cash:     1000,
				Type:     "perps",
				Platform: "hyperliquid",
				Positions: map[string]*Position{
					"BTC": {
						Symbol: "BTC", Quantity: 0.01, AvgCost: 50000, Side: "short",
						Multiplier: 1, StopLossOID: 99,
					},
				},
			},
		},
	}
	var calls []string
	closer := func(symbol string, partialSz *float64, cancelStopLossOIDs []int64) (*HyperliquidCloseResult, error) {
		calls = append(calls, symbol)
		return &HyperliquidCloseResult{
			Close:                   &HyperliquidClose{Symbol: symbol, AlreadyFlat: true},
			CancelStopLossSucceeded: true,
		}, nil
	}
	jobs := []RegimeDirectionOrphanCloseJob{{
		StrategyID: sc.ID, Symbol: "BTC", CloseQty: 0.01,
		CancelOIDs: []int64{99},
	}}
	runRegimeDirectionOrphanCloses(context.Background(), state, []StrategyConfig{sc}, jobs,
		[]HLPosition{{Coin: "BTC", Size: -0.01}}, closer, &sync.RWMutex{}, nil)
	if len(calls) != 1 {
		t.Fatalf("closer calls = %v", calls)
	}
	ss := state.Strategies[sc.ID]
	pos := ss.Positions["BTC"]
	if pos == nil {
		t.Fatal("AlreadyFlat: virtual position kept for next reconcile")
	}
	if pos.StopLossOID != 0 {
		t.Fatalf("StopLossOID = %d, want 0 after cancel succeeded before fill booking", pos.StopLossOID)
	}
	if len(ss.TradeHistory) != 0 {
		t.Fatal("AlreadyFlat: no trade should be booked without a fill")
	}
}
