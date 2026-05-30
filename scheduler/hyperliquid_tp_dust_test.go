package main

import (
	"math"
	"sync"
	"testing"
)

// TestHyperliquidAllTiersArmedAndCleared_DustGuard verifies #777: all-zero TPOIDs
// with every tier armed is distinct from a never-placed TP list.
func TestHyperliquidAllTiersArmedAndCleared_DustGuard(t *testing.T) {
	sc := tieredTPATRSC()
	pos := &Position{
		Quantity:     0.013,
		TPOIDs:       []int64{0, 0},
		TPArmedTiers: []bool{true, true},
	}
	if !hyperliquidAllTiersArmedAndCleared(sc, pos) {
		t.Fatal("expected all tiers armed and cleared")
	}
	pos.TPArmedTiers = []bool{false, false}
	if hyperliquidAllTiersArmedAndCleared(sc, pos) {
		t.Error("never-placed TP list must not look armed+cleared")
	}
	if _, ok := hyperliquidClearedTPTier(sc, pos, 0.012); ok {
		t.Error("hyperliquidClearedTPTier must not attribute ambiguous all-zero dust")
	}
}

// TestSoleOwnerTPDust_BooksBothTiersAtUserFills is the #777 positive case:
// short qty=0.013, all TPOIDs zero, all tiers armed, on-chain dust -0.001,
// userFills carry both TP partials — expect two close trades and qty=0.001.
func TestSoleOwnerTPDust_BooksBothTiersAtUserFills(t *testing.T) {
	const (
		fullQty  = 0.012 // 50/50 tiers → 0.006 per TP leg
		dustQty  = 0.001
		tp1OID   = int64(438613562733)
		tp2OID   = int64(438613569010)
		tp1Qty   = 0.006
		tp2Qty   = 0.006
		tp1Px    = 74605.0
		tp2Px    = 74284.0
		tp1Fee   = 0.064458
		tp2Fee   = 0.06418
		entryPx  = 75249.0
		entryATR = 200.0
	)
	sc := soleOwnerTPSC()
	sc.Symbol = "BTC"
	sc.Args = []string{"sma", "BTC", "1h", "--mode=live"}

	ss := &StrategyState{
		ID:   sc.ID,
		Cash: 1000,
		Positions: map[string]*Position{
			"BTC": {
				Symbol: "BTC", Quantity: fullQty, InitialQuantity: fullQty,
				AvgCost: entryPx, EntryATR: entryATR, Side: "short",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: sc.ID,
				TPOIDs: []int64{0, 0}, TPArmedTiers: []bool{true, true},
			},
		},
		TradeHistory: []Trade{{
			Symbol: "BTC", Side: "sell", Quantity: fullQty, Price: entryPx,
			TPOIDs: []int64{tp1OID, tp2OID},
		}},
	}
	positions := []HLPosition{{Coin: "BTC", Size: -dustQty, EntryPrice: entryPx, Leverage: 5}}
	resolver := hlReconcileFillResolver(func(_ string, oid int64, qty float64) (HLFillLookup, bool) {
		switch {
		case oid == tp1OID || (oid == 0 && math.Abs(qty-tp1Qty) < 1e-9):
			return HLFillLookup{Fee: tp1Fee, FilledQty: tp1Qty, Px: tp1Px, Count: 1, OID: tp1OID}, true
		case oid == tp2OID || (oid == 0 && math.Abs(qty-tp2Qty) < 1e-9):
			return HLFillLookup{Fee: tp2Fee, FilledQty: tp2Qty, Px: tp2Px, Count: 1, OID: tp2OID}, true
		default:
			return HLFillLookup{}, false
		}
	})
	logger := newTestLogger(t)

	changed := reconcileHyperliquidPositionsForStrategy(sc, ss, "BTC", positions, resolver, logger, nil, nil)
	if !changed {
		t.Fatal("expected changed=true")
	}
	pos := ss.Positions["BTC"]
	if pos == nil {
		t.Fatal("expected BTC position to remain as dust")
	}
	if math.Abs(pos.Quantity-dustQty) > 1e-9 {
		t.Errorf("Quantity = %g, want %g", pos.Quantity, dustQty)
	}
	if len(ss.TradeHistory) < 2 {
		t.Fatalf("TradeHistory = %d rows, want at least 2 TP closes", len(ss.TradeHistory))
	}
	var closes []Trade
	for _, tr := range ss.TradeHistory {
		if tr.IsClose {
			closes = append(closes, tr)
		}
	}
	if len(closes) != 2 {
		t.Fatalf("close trades = %d, want 2", len(closes))
	}
	if math.Abs(closes[0].Quantity-tp1Qty) > 1e-9 || math.Abs(closes[0].Price-tp1Px) > 1e-9 {
		t.Errorf("TP1 trade = %+v, want qty=%g px=%g", closes[0], tp1Qty, tp1Px)
	}
	wantTP2Qty := fullQty - dustQty - tp1Qty // clamped to leave dust residual
	if math.Abs(closes[1].Quantity-wantTP2Qty) > 1e-9 || math.Abs(closes[1].Price-tp2Px) > 1e-9 {
		t.Errorf("TP2 trade = %+v, want qty=%g px=%g", closes[1], wantTP2Qty, tp2Px)
	}
}

// TestSoleOwnerTPDust_NeverPlaced_NoBook is the #777 negative guard: all-zero
// TPOIDs with TPArmedTiers never set must not fabricate TP close trades.
func TestSoleOwnerTPDust_NeverPlaced_NoBook(t *testing.T) {
	const (
		fullQty  = 0.013
		dustQty  = 0.001
		entryPx  = 75249.0
		entryATR = 200.0
	)
	sc := soleOwnerTPSC()
	sc.Symbol = "BTC"
	sc.Args = []string{"sma", "BTC", "1h", "--mode=live"}

	ss := &StrategyState{
		ID:   sc.ID,
		Cash: 1000,
		Positions: map[string]*Position{
			"BTC": {
				Symbol: "BTC", Quantity: fullQty, InitialQuantity: fullQty,
				AvgCost: entryPx, EntryATR: entryATR, Side: "short",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: sc.ID,
				TPOIDs: []int64{0, 0}, TPArmedTiers: []bool{false, false},
			},
		},
	}
	positions := []HLPosition{{Coin: "BTC", Size: -dustQty, EntryPrice: entryPx, Leverage: 5}}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	})
	logger := newTestLogger(t)

	reconcileHyperliquidPositionsForStrategy(sc, ss, "BTC", positions, resolver, logger, nil, nil)

	if len(ss.TradeHistory) != 0 {
		t.Fatalf("TradeHistory = %d, want 0 close trades", len(ss.TradeHistory))
	}
	pos := ss.Positions["BTC"]
	if pos == nil {
		t.Fatal("expected position to remain")
	}
	// Legacy reconciler resyncs qty to on-chain when dust is not TP-attributable.
	if math.Abs(pos.Quantity-dustQty) > 1e-9 {
		t.Errorf("Quantity = %g, want legacy resync to %g", pos.Quantity, dustQty)
	}
}

// TestReconcileSharedCoin_TPDust_BooksBothTiers is Detector 3 parity for #777.
func TestReconcileSharedCoin_TPDust_BooksBothTiers(t *testing.T) {
	const (
		fullQty  = 0.012 // 50/50 tiers → 0.006 per TP leg
		dustQty  = 0.001
		tp1OID   = int64(438613562733)
		tp2OID   = int64(438613569010)
		tp1Qty   = 0.006
		tp2Qty   = 0.006
		tp1Px    = 74605.0
		tp2Px    = 74284.0
		tp1Fee   = 0.064458
		tp2Fee   = 0.06418
		entryPx  = 75249.0
		entryATR = 200.0
	)
	ownerID := "hl-owner-btc"
	sc := StrategyConfig{
		ID: ownerID, Platform: "hyperliquid", Type: "perps",
		Args: []string{"sma", "BTC", "1h", "--mode=live"},
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
			},
		}}},
	}
	state := &AppState{
		Strategies: map[string]*StrategyState{
			ownerID: {
				ID: ownerID, Cash: 1000, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{
					"BTC": {
						Symbol: "BTC", Quantity: fullQty, InitialQuantity: fullQty,
						AvgCost: entryPx, EntryATR: entryATR, Side: "short",
						Multiplier: 1, Leverage: 5, OwnerStrategyID: ownerID,
						TPOIDs: []int64{0, 0}, TPArmedTiers: []bool{true, true},
					},
				},
				TradeHistory: []Trade{{
					Symbol: "BTC", Side: "sell", Quantity: fullQty, Price: entryPx,
					TPOIDs: []int64{tp1OID, tp2OID},
				}},
			},
			"hl-peer-btc": {
				ID: "hl-peer-btc", Cash: 500, Platform: "hyperliquid", Type: "perps",
				Positions: map[string]*Position{},
			},
		},
	}
	allStrategies := []StrategyConfig{
		sc,
		{ID: "hl-peer-btc", Platform: "hyperliquid", Type: "perps", Args: []string{"hold", "BTC", "1h", "--mode=live"}},
	}
	positions := []HLPosition{{Coin: "BTC", Size: -dustQty, EntryPrice: entryPx, Leverage: 5}}
	oldLookup := lookupHyperliquidReconcileFillFee
	lookupHyperliquidReconcileFillFee = func(_ string, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if coin != "BTC" {
			return HLFillLookup{}, false
		}
		switch {
		case oid == tp1OID || (oid == 0 && math.Abs(qty-tp1Qty) < 1e-9):
			return HLFillLookup{Fee: tp1Fee, FilledQty: tp1Qty, Px: tp1Px, Count: 1, OID: tp1OID}, true
		case oid == tp2OID || (oid == 0 && math.Abs(qty-tp2Qty) < 1e-9):
			return HLFillLookup{Fee: tp2Fee, FilledQty: tp2Qty, Px: tp2Px, Count: 1, OID: tp2OID}, true
		default:
			return HLFillLookup{}, false
		}
	}
	t.Cleanup(func() { lookupHyperliquidReconcileFillFee = oldLookup })

	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex
	_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, positions, map[string]float64{"BTC": entryPx}, "0xtest", nil, false)

	owner := state.Strategies[ownerID]
	pos := owner.Positions["BTC"]
	if pos == nil || math.Abs(pos.Quantity-dustQty) > 1e-9 {
		t.Fatalf("owner BTC = %+v, want qty %g", pos, dustQty)
	}
	var closes int
	for _, tr := range owner.TradeHistory {
		if tr.IsClose {
			closes++
		}
	}
	if closes != 2 {
		t.Fatalf("owner close trades = %d, want 2", closes)
	}
	gap := state.ReconciliationGaps["BTC"]
	if gap == nil {
		t.Fatal("expected ReconciliationGaps[BTC]")
	}
	if math.Abs(gap.DeltaQty) > 1e-6 {
		t.Errorf("DeltaQty = %g, want 0", gap.DeltaQty)
	}
	if math.Abs(gap.VirtualQty-(-dustQty)) > 1e-6 {
		t.Errorf("VirtualQty = %g, want %g", gap.VirtualQty, -dustQty)
	}
}
