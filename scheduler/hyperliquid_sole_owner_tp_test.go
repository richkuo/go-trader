package main

import (
	"math"
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

func TestSoleOwnerTPPartial_PrefersUserFillsPxOverConfiguredTP(t *testing.T) {
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

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts, nil)

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
}

func TestSoleOwnerTP_SkipsWhenNoTierCleared(t *testing.T) {
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

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts, nil)

	if len(ss.TradeHistory) != 0 {
		t.Errorf("TradeHistory = %d, want 0 (no TP cleared, legacy resync should be silent)", len(ss.TradeHistory))
	}
	if len(alerts) != 0 {
		t.Errorf("alerts = %d, want 0", len(alerts))
	}
	if math.Abs(ss.Positions["ETH"].Quantity-0.2) > 1e-9 {
		t.Errorf("Quantity = %g, want 0.2 (legacy resync)", ss.Positions["ETH"].Quantity)
	}
}

func TestLookupHyperliquidFillByOID_AggregatesPxAsSizeWeightedAvg(t *testing.T) {
	prevFetcher := fetchHyperliquidUserFillsByTime
	defer func() { fetchHyperliquidUserFillsByTime = prevFetcher }()
	prevRetries, prevDelay := hlFillLookupRetries, hlFillLookupRetryDelay
	hlFillLookupRetries, hlFillLookupRetryDelay = 1, 0
	defer func() {
		hlFillLookupRetries = prevRetries
		hlFillLookupRetryDelay = prevDelay
	}()

	fetchHyperliquidUserFillsByTime = func(string, int64) ([]hlFillRecord, error) {
		return []hlFillRecord{
			{Coin: "ETH", Sz: "0.1", Px: "2100", OID: "999", Fee: "0.01", ClosedPnl: "10"},
			{Coin: "ETH", Sz: "0.3", Px: "2104", OID: "999", Fee: "0.03", ClosedPnl: "31.2"},
		}, nil
	}

	lookup, ok := lookupHyperliquidFillByOID("0xacct", 999, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if math.Abs(lookup.Px-2103.0) > 1e-9 {
		t.Errorf("Px = %g, want 2103 (size-weighted avg)", lookup.Px)
	}
	if math.Abs(lookup.FilledQty-0.4) > 1e-9 {
		t.Errorf("FilledQty = %g, want 0.4", lookup.FilledQty)
	}
	if math.Abs(lookup.Fee-0.04) > 1e-9 {
		t.Errorf("Fee = %g, want 0.04", lookup.Fee)
	}
}

func TestSoleOwnerTPPartial_FallsBackToConfiguredTPWhenLookupPxZero(t *testing.T) {
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

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts, nil)

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
}

func TestSoleOwnerTP_FullCloseWithStaleClearedTier_DefersToSL(t *testing.T) {
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

	changed := reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", nil, resolver, logger, &alerts, nil)
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
}

func TestSoleOwnerTP758_RecoveryStampsTierSoHlAttemptSkipsOID(t *testing.T) {
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
	if !reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, nil, nil) {
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
}
