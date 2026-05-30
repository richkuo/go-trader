package main

import (
	"context"
	"sync"
	"testing"
)

func TestPerpsRegimeDirectionOrphanConflict_RegimeFlip(t *testing.T) {
	sc := StrategyConfig{
		ID:        "hl-test",
		Type:      "perps",
		Platform:  "hyperliquid",
		Args:      []string{"vwap", "BTC", "1h", "--mode=live"},
		Direction: DirectionLong,
		RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
			TrendRegime: map[string]RegimeDirectionalEntry{
				"trending_down": {Direction: DirectionShort},
				"trending_up":   {Direction: DirectionLong},
				"ranging":       {Direction: DirectionLong},
			},
		},
	}
	ss := &StrategyState{
		ID:     sc.ID,
		Regime: "trending_up",
		Positions: map[string]*Position{
			"BTC": {
				Symbol:          "BTC",
				Quantity:        0.01,
				Side:            "short",
				OwnerStrategyID: sc.ID,
				Regime:          "trending_down",
			},
		},
	}
	conflict, current, eff := perpsRegimeDirectionOrphanConflict(ss, sc, ss.Positions["BTC"])
	if !conflict {
		t.Fatalf("want conflict when current regime is long and position is short; current=%q eff=%q", current, eff)
	}
	if current != "trending_up" || eff != DirectionLong {
		t.Fatalf("got current=%q eff=%q", current, eff)
	}
}

func TestPerpsRegimeDirectionOrphanConflict_HoldStampedNoConflict(t *testing.T) {
	sc := StrategyConfig{
		ID:        "hl-test",
		Type:      "perps",
		Platform:  "hyperliquid",
		Args:      []string{"vwap", "BTC", "1h", "--mode=live"},
		Direction: DirectionLong,
		RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
			TrendRegime: map[string]RegimeDirectionalEntry{
				"trending_down": {Direction: DirectionShort},
				"trending_up":   {Direction: DirectionLong},
				"ranging":       {Direction: DirectionLong},
			},
		},
	}
	ss := &StrategyState{
		ID:     sc.ID,
		Regime: "trending_down",
		Positions: map[string]*Position{
			"BTC": {
				Symbol:          "BTC",
				Quantity:        0.01,
				Side:            "short",
				OwnerStrategyID: sc.ID,
				Regime:          "trending_down",
			},
		},
	}
	if conflict, _, _ := perpsRegimeDirectionOrphanConflict(ss, sc, ss.Positions["BTC"]); conflict {
		t.Fatal("short under trending_down should not conflict with current regime")
	}
}

func TestReconcileHyperliquidPositionsWithResolver_QueuesRegimeOrphanClose(t *testing.T) {
	sc := StrategyConfig{
		ID:        "hl-vwap-btc",
		Type:      "perps",
		Platform:  "hyperliquid",
		Args:      []string{"vwap", "BTC", "1h", "--mode=live"},
		Direction: DirectionLong,
		RegimeDirectionalPolicy: &RegimeDirectionalPolicy{
			TrendRegime: map[string]RegimeDirectionalEntry{
				"trending_down": {Direction: DirectionShort},
				"trending_up":   {Direction: DirectionLong},
				"ranging":       {Direction: DirectionLong},
			},
		},
	}
	ss := &StrategyState{
		ID:     sc.ID,
		Regime: "trending_up",
		Positions: map[string]*Position{
			"BTC": {
				Symbol:          "BTC",
				Quantity:        0.01,
				AvgCost:         50000,
				Side:            "short",
				Multiplier:      1,
				OwnerStrategyID: sc.ID,
				Regime:          "trending_down",
			},
		},
	}
	positions := []HLPosition{{Coin: "BTC", Size: -0.01, EntryPrice: 50000, Leverage: 2}}
	lm, _ := NewLogManager("")
	logger, _ := lm.GetStrategyLogger(sc.ID)
	defer logger.Close()
	var jobs []RegimeDirectionOrphanCloseJob
	reconcileHyperliquidPositionsWithResolver(ss, "BTC", positions, func(string, int64, float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	}, logger, nil, &jobs, sc)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].StrategyID != sc.ID || jobs[0].Symbol != "BTC" || jobs[0].EffectiveDir != DirectionLong {
		t.Fatalf("job = %+v", jobs[0])
	}
}

func TestRunRegimeDirectionOrphanCloses_BooksAndFlattens(t *testing.T) {
	sc := StrategyConfig{
		ID:        "hl-vwap-btc",
		Type:      "perps",
		Platform:  "hyperliquid",
		Args:      []string{"vwap", "BTC", "1h", "--mode=live"},
		Direction: DirectionLong,
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			sc.ID: {
				ID:       sc.ID,
				Cash:     1000,
				Type:     "perps",
				Platform: "hyperliquid",
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.01, AvgCost: 50000, Side: "short", Multiplier: 1},
				},
			},
		},
	}
	closer, calls := fakeCloser(nil)
	jobs := []RegimeDirectionOrphanCloseJob{{
		StrategyID: sc.ID, Symbol: "BTC", CloseQty: 0.01, PosSide: "short",
		CurrentRegime: "trending_up", EffectiveDir: DirectionLong,
	}}
	runRegimeDirectionOrphanCloses(context.Background(), state, []StrategyConfig{sc}, jobs,
		[]HLPosition{{Coin: "BTC", Size: -0.01}}, closer, &sync.RWMutex{}, nil)
	if len(*calls) != 1 {
		t.Fatalf("closer calls = %v", *calls)
	}
	ss := state.Strategies[sc.ID]
	if pos := ss.Positions["BTC"]; pos != nil {
		t.Fatal("position should be removed after orphan close")
	}
	if len(ss.TradeHistory) == 0 {
		t.Fatal("expected close trade")
	}
	if ss.TradeHistory[len(ss.TradeHistory)-1].Details == "" {
		t.Fatal("expected trade details")
	}
}

func TestPerpsRegimeDirectionOrphanConflict_SkipsPaper(t *testing.T) {
	sc := StrategyConfig{
		ID:        "hl-paper",
		Type:      "perps",
		Platform:  "hyperliquid",
		Args:      []string{"vwap", "BTC", "1h"},
		Direction: DirectionShort,
	}
	ss := &StrategyState{
		Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Quantity: 1, Side: "long"},
		},
	}
	if conflict, _, _ := perpsRegimeDirectionOrphanConflict(ss, sc, ss.Positions["BTC"]); conflict {
		t.Fatal("paper mode must not queue orphan close")
	}
}
