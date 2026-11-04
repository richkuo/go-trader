package main

import (
	"math"
	"sync"
	"testing"
)

func soleOwnerTPSC() StrategyConfig {
	return StrategyConfig{
		ID:       "hl-tp-sole",
		Platform: "hyperliquid",
		Type:     "perps",
		Symbol:   "ETH",
		Args:     []string{"sma", "ETH", "1h", "--mode=live"},
		CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tp_tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
			},
		}},
	}
}

func TestReconcileHyperliquidTPFills(t *testing.T) {
	t.Run("SoleOwnerPartialPrefersUserFillsPxOverConfiguredTP", func(t *testing.T) {
		const (
			entryPx    = 2000.0
			entryATR   = 50.0
			fullQty    = 0.4
			onChainQty = 0.2
			actualPx   = 2105.25
			actualFee  = 0.04
		)
		ss := &StrategyState{
			ID:   "hl-tp-sole",
			Cash: 100,
			Positions: map[string]*Position{
				"ETH": {
					Symbol: "ETH", Quantity: fullQty, InitialQuantity: fullQty,
					AvgCost: entryPx, EntryATR: entryATR, Side: "long",
					Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
					TPOIDs: []int64{0, 222},
				},
			},
		}
		positions := []HLPosition{{Coin: "ETH", Size: onChainQty, EntryPrice: entryPx, Leverage: 5}}
		resolver := hlReconcileFillResolver(func(_ string, _ int64, qty float64) (HLFillLookup, bool) {
			if math.Abs(qty-0.2) < 1e-6 {
				return HLFillLookup{Fee: actualFee, FilledQty: 0.2, Px: actualPx, OID: 999, Count: 1}, true
			}
			return HLFillLookup{}, false
		})
		var alerts []ProtectionFillAlert
		logger := newTestLogger(t)

		reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts, nil, nil)

		if len(ss.TradeHistory) != 1 {
			t.Fatalf("TradeHistory = %d, want 1", len(ss.TradeHistory))
		}
		trade := ss.TradeHistory[0]
		if math.Abs(trade.Price-actualPx) > 1e-9 {
			t.Errorf("trade.Price = %g, want %g (userFills px takes precedence)", trade.Price, actualPx)
		}
		if math.Abs(trade.ExchangeFee-actualFee) > 1e-9 {
			t.Errorf("trade.ExchangeFee = %g, want %g (real fee, not modeled)", trade.ExchangeFee, actualFee)
		}
		if trade.ExchangeOrderID != "999" {
			t.Errorf("trade.ExchangeOrderID = %q, want %q (from lookup.OID)", trade.ExchangeOrderID, "999")
		}
		wantGross := (actualPx - entryPx) * 0.2
		if !trade.PnLGross || math.Abs(trade.RealizedPnL-wantGross) > 1e-6 {
			t.Errorf("RealizedPnL = %g (gross=%v), want gross %g", trade.RealizedPnL, trade.PnLGross, wantGross)
		}
		if math.Abs(tradeNetPnL(trade)-(wantGross-actualFee)) > 1e-6 {
			t.Errorf("tradeNetPnL = %g, want %g", tradeNetPnL(trade), wantGross-actualFee)
		}
	})
	t.Run("SoleOwnerSkipsWhenNoTierCleared", func(t *testing.T) {
		ss := &StrategyState{
			ID:   "hl-tp-sole",
			Cash: 100,
			Positions: map[string]*Position{
				"ETH": {
					Symbol: "ETH", Quantity: 0.4, InitialQuantity: 0.4,
					AvgCost: 2000, EntryATR: 50, Side: "long",
					Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
					TPOIDs: []int64{111, 222},
				},
			},
		}
		positions := []HLPosition{{Coin: "ETH", Size: 0.2, EntryPrice: 2000, Leverage: 5}}
		resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
			return HLFillLookup{}, false
		})
		var alerts []ProtectionFillAlert
		logger := newTestLogger(t)

		reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts, nil, nil)

		if len(ss.TradeHistory) != 0 {
			t.Errorf("TradeHistory = %d, want 0 (no TP cleared, legacy resync should be silent)", len(ss.TradeHistory))
		}
		if len(alerts) != 0 {
			t.Errorf("alerts = %d, want 0", len(alerts))
		}
		if math.Abs(ss.Positions["ETH"].Quantity-0.2) > 1e-9 {
			t.Errorf("Quantity = %g, want 0.2 (legacy resync)", ss.Positions["ETH"].Quantity)
		}
	})
	t.Run("SoleOwnerPartialFallsBackToConfiguredTPWhenLookupPxZero", func(t *testing.T) {
		const (
			entryPx     = 2000.0
			entryATR    = 50.0
			fullQty     = 0.4
			onChainQty  = 0.2
			realFee     = 0.07
			expectedTP1 = entryPx + 2.0*entryATR
		)
		ss := &StrategyState{
			ID:   "hl-tp-sole",
			Cash: 100,
			Positions: map[string]*Position{
				"ETH": {
					Symbol: "ETH", Quantity: fullQty, InitialQuantity: fullQty,
					AvgCost: entryPx, EntryATR: entryATR, Side: "long",
					Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
					TPOIDs: []int64{0, 222},
				},
			},
		}
		positions := []HLPosition{{Coin: "ETH", Size: onChainQty, EntryPrice: entryPx, Leverage: 5}}
		resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
			return HLFillLookup{Fee: realFee, FilledQty: 0.2, Px: 0, OID: 555, Count: 1}, true
		})
		var alerts []ProtectionFillAlert
		logger := newTestLogger(t)

		reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts, nil, nil)

		if len(ss.TradeHistory) != 1 {
			t.Fatalf("TradeHistory = %d, want 1", len(ss.TradeHistory))
		}
		trade := ss.TradeHistory[0]
		if math.Abs(trade.Price-expectedTP1) > 1e-9 {
			t.Errorf("trade.Price = %g, want %g (configured TP1 fallback when lookup.Px<=0)", trade.Price, expectedTP1)
		}
		if math.Abs(trade.ExchangeFee-realFee) > 1e-9 {
			t.Errorf("trade.ExchangeFee = %g, want %g (real fee retained even when Px<=0)", trade.ExchangeFee, realFee)
		}
	})
	t.Run("SoleOwnerFullCloseWithStaleClearedTierDefersToSL", func(t *testing.T) {
		const (
			entryPx     = 2000.0
			entryATR    = 50.0
			residualQty = 0.2
			slTriggerPx = 1900.0
		)
		ss := &StrategyState{
			ID:   "hl-tp-sole",
			Cash: 100,
			Positions: map[string]*Position{
				"ETH": {
					Symbol: "ETH", Quantity: residualQty, InitialQuantity: 0.4,
					AvgCost: entryPx, EntryATR: entryATR, Side: "long",
					Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
					TPOIDs:            []int64{0, 222},
					StopLossOID:       42,
					StopLossTriggerPx: slTriggerPx,
				},
			},
		}
		resolver := hlReconcileFillResolver(func(_ string, oid int64, _ float64) (HLFillLookup, bool) {
			if oid == 42 {
				return HLFillLookup{Fee: 0.05, FilledQty: residualQty, Px: slTriggerPx, Count: 1, OID: 42}, true
			}
			return HLFillLookup{}, false
		})
		var alerts []ProtectionFillAlert
		logger := newTestLogger(t)

		changed := reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", nil, resolver, logger, &alerts, nil, nil)
		if !changed {
			t.Fatal("expected changed=true (legacy SL-owner branch should still book)")
		}
		if _, open := ss.Positions["ETH"]; open {
			t.Error("position should be closed")
		}
		if len(ss.TradeHistory) != 1 {
			t.Fatalf("TradeHistory = %d, want 1", len(ss.TradeHistory))
		}
		trade := ss.TradeHistory[0]
		if math.Abs(trade.Price-slTriggerPx) > 1e-9 {
			t.Errorf("trade.Price = %g, want %g (SL trigger, NOT a stale TP price)", trade.Price, slTriggerPx)
		}
		if len(ss.ClosedPositions) != 1 {
			t.Fatalf("ClosedPositions = %d, want 1", len(ss.ClosedPositions))
		}
		if got := ss.ClosedPositions[0].CloseReason; got != "stop_loss" {
			t.Errorf("CloseReason = %q, want \"stop_loss\" (defer to legacy SL handler)", got)
		}
		for _, a := range alerts {
			if a.FillType == "TP1" || a.FillType == "TP2" {
				t.Errorf("unexpected TP alert %+v — SL close must not mis-attribute to a TP tier", a)
			}
		}
	})
	t.Run("SoleOwner758RecoveryStampsTierSoHlAttemptSkipsOID", func(t *testing.T) {
		const (
			entryPx    = 2000.0
			entryATR   = 50.0
			fullQty    = 0.4
			onChainQty = 0.2
			tp1OID     = int64(111)
			fillPx     = 2103.50
			fillFee    = 0.05
		)
		ss := &StrategyState{
			ID:   "hl-tp-sole",
			Cash: 100,
			Positions: map[string]*Position{
				"ETH": {
					Symbol: "ETH", Quantity: fullQty, InitialQuantity: fullQty,
					AvgCost: entryPx, EntryATR: entryATR, Side: "long",
					Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
					TPOIDs: []int64{tp1OID, 222},
				},
			},
		}
		positions := []HLPosition{{Coin: "ETH", Size: onChainQty, EntryPrice: entryPx, Leverage: 5}}
		resolver := hlReconcileFillResolver(func(_ string, _ int64, qty float64) (HLFillLookup, bool) {
			if math.Abs(qty-(fullQty-onChainQty)) < 1e-6 {
				return HLFillLookup{Fee: fillFee, FilledQty: 0.2, Px: fillPx, OID: tp1OID, Count: 1}, true
			}
			return HLFillLookup{}, false
		})
		logger := newTestLogger(t)
		if !reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, nil, nil, nil) {
			t.Fatal("expected reconcile to return true")
		}
		if len(ss.TradeHistory) != 1 {
			t.Fatalf("TradeHistory = %d, want 1", len(ss.TradeHistory))
		}
		pos := ss.Positions["ETH"]
		if pos == nil {
			t.Fatal("expected position after partial recovery")
		}
		dupResolver := hlReconcileFillResolver(func(_ string, oid int64, _ float64) (HLFillLookup, bool) {
			if oid == tp1OID {
				return HLFillLookup{Fee: fillFee, FilledQty: 0.2, Px: fillPx, OID: tp1OID, Count: 1}, true
			}
			return HLFillLookup{}, false
		})
		if hlAttemptCloseFromTPFills(ss, "ETH", pos, dupResolver, logger, nil) {
			t.Fatal("hlAttemptCloseFromTPFills should not re-book TP1 after recovery stamped tier consumed")
		}
		if len(ss.TradeHistory) != 1 {
			t.Fatalf("TradeHistory = %d after hlAttempt, want 1 (no duplicate TP1 book)", len(ss.TradeHistory))
		}
	})
	t.Run("SoleOwnerDustBooksBothTiersAtUserFills", func(t *testing.T) {
		const (
			fullQty  = 0.012
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

		changed := reconcileHyperliquidPositionsForStrategy(sc, ss, "BTC", positions, resolver, logger, nil, nil, nil)
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
		wantTP2Qty := fullQty - dustQty - tp1Qty
		if math.Abs(closes[1].Quantity-wantTP2Qty) > 1e-9 || math.Abs(closes[1].Price-tp2Px) > 1e-9 {
			t.Errorf("TP2 trade = %+v, want qty=%g px=%g", closes[1], wantTP2Qty, tp2Px)
		}
	})
	t.Run("SharedCoinDustBooksBothTiers", func(t *testing.T) {
		const (
			fullQty  = 0.012
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
			CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
				"tp_tiers": []interface{}{
					map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
					map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
				},
			}},
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
		_, _, _ = reconcileHyperliquidAccountPositions(allStrategies, allStrategies, state, &mu, logMgr, nil, positions, map[string]float64{"BTC": entryPx}, "0xtest", nil, false)

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
	})
}
