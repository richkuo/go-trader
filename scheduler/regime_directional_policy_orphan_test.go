package main

import (
	"context"
	"strings"
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

				DirectionCertifiedAtOpen:       true,
				DirectionCertifiedStatesAtOpen: map[string]string{"trending_up": DirectionLong, "trending_down": DirectionShort, "ranging": DirectionLong},
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

				DirectionCertifiedAtOpen:       true,
				DirectionCertifiedStatesAtOpen: map[string]string{"trending_up": DirectionLong, "trending_down": DirectionShort, "ranging": DirectionLong},
			},
		},
	}
	if conflict, _, _ := perpsRegimeDirectionOrphanConflict(ss, sc, ss.Positions["BTC"]); conflict {
		t.Fatal("short under trending_down should not conflict with current regime")
	}
}

func TestPerpsRegimeDirectionOrphanConflict_UncertifiedResolvesToBase(t *testing.T) {
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
	conflict, _, eff := perpsRegimeDirectionOrphanConflict(ss, sc, ss.Positions["BTC"])
	if !conflict {
		t.Fatal("uncertified short must conflict with base direction (migration)")
	}
	if eff != DirectionLong {
		t.Fatalf("uncertified orphan must resolve to base direction long, got %q", eff)
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
	last := ss.TradeHistory[len(ss.TradeHistory)-1]
	if !strings.Contains(last.Details, "Regime/direction flip") {
		t.Fatalf("Details = %q, want regime-flip label", last.Details)
	}
}

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

func TestRunRegimeDirectionOrphanCloses_SharedCoinSurfacesNoAutoClose(t *testing.T) {
	scA := StrategyConfig{
		ID: "hl-a", Type: "perps", Platform: "hyperliquid",
		Args: []string{"vwap", "BTC", "1h", "--mode=live"}, Direction: DirectionLong,
	}
	scB := StrategyConfig{
		ID: "hl-b", Type: "perps", Platform: "hyperliquid",
		Args: []string{"vwap", "BTC", "1h", "--mode=live"}, Direction: DirectionLong,
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			scA.ID: {
				ID: scA.ID, Cash: 1000, Type: "perps", Platform: "hyperliquid",
				Positions: map[string]*Position{
					"BTC": {Symbol: "BTC", Quantity: 0.01, AvgCost: 50000, Side: "short", Multiplier: 1},
				},
			},
		},
	}
	closer, calls := fakeCloser(nil)
	var dms []string
	jobs := []RegimeDirectionOrphanCloseJob{{
		StrategyID: scA.ID, Symbol: "BTC", CloseQty: 0.01, PosSide: "short",
		CurrentRegime: "trending_up", EffectiveDir: DirectionLong,
	}}
	runRegimeDirectionOrphanCloses(context.Background(), state, []StrategyConfig{scA, scB}, jobs,
		[]HLPosition{{Coin: "BTC", Size: -0.01}}, closer, &sync.RWMutex{},
		func(m string) { dms = append(dms, m) })
	if len(*calls) != 0 {
		t.Fatalf("shared-coin orphan must NOT auto-close, got closer calls %v", *calls)
	}
	if len(dms) != 1 || !strings.Contains(dms[0], "MANUAL CLOSE REQUIRED") {
		t.Fatalf("expected one owner DM citing manual close, got %v", dms)
	}
	if pos := state.Strategies[scA.ID].Positions["BTC"]; pos == nil {
		t.Fatal("shared-coin position must be left in place (not closed)")
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

func TestPerpsRegimeDirectionOrphanConflict_SignContradictionResolvesToBase(t *testing.T) {
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
				Symbol:                   "BTC",
				Quantity:                 0.01,
				Side:                     "long",
				OwnerStrategyID:          sc.ID,
				Regime:                   "trending_down",
				DirectionCertifiedAtOpen: true,
				DirectionCertifiedStatesAtOpen: map[string]string{
					"trending_down": DirectionLong,
					"trending_up":   DirectionLong,
					"ranging":       DirectionLong,
				},
			},
		},
	}
	conflict, current, eff := perpsRegimeDirectionOrphanConflict(ss, sc, ss.Positions["BTC"])
	if conflict {
		t.Fatalf("sign-contradicting current regime must resolve to base (no spurious close); current=%q eff=%q", current, eff)
	}
	if eff != DirectionLong {
		t.Fatalf("contradicting current state must resolve to base long, got %q", eff)
	}
}
