package main

import (
	"math"
	"testing"
)

// soleOwnerTPSC builds a sole-owner HL perps strategy with two TP tiers
// (2× ATR / 50%, 3× ATR / 100%). Mirrors tieredTPATRSC but typed with
// Platform/Type so production helpers that gate on those fields apply.
func soleOwnerTPSC() StrategyConfig {
	return StrategyConfig{
		ID:       "hl-tp-sole",
		Platform: "hyperliquid",
		Type:     "perps",
		Symbol:   "ETH",
		Args:     []string{"sma", "ETH", "1h", "--mode=live"},
		CloseStrategies: []StrategyRef{{Name: "tiered_tp_atr_live", Params: map[string]interface{}{
			"tiers": []interface{}{
				map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.5},
				map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
			},
		}}},
	}
}

// TestSoleOwnerTPPartial_BooksAtTPPriceFromTiers verifies that when on-chain
// qty is a same-direction strict subset of virtual qty AND the cleared TP tier
// is unambiguous, the reconciler books a partial close at the configured TP
// price (no userFills px available) instead of silently resyncing the qty.
//
// Regression for #670 — sole-owner partial TP fills were dropped.
func TestSoleOwnerTPPartial_BooksAtTPPriceFromTiers(t *testing.T) {
	const (
		entryPx     = 2000.0
		entryATR    = 50.0
		fullQty     = 0.4 // tier 0 closes 50% → 0.2
		onChainQty  = 0.2
		expectedTP1 = entryPx + 2.0*entryATR // long 2× → 2100
	)
	ss := &StrategyState{
		ID:   "hl-tp-sole",
		Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: fullQty, InitialQuantity: fullQty,
				AvgCost: entryPx, EntryATR: entryATR, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
				TPOIDs: []int64{0, 222}, // tier 0 cleared (filled), tier 1 still active
			},
		},
	}
	positions := []HLPosition{{Coin: "ETH", Size: onChainQty, EntryPrice: entryPx, Leverage: 5}}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false // no userFills px → fall back to TP price
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	changed := reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)
	if !changed {
		t.Fatal("expected changed=true")
	}

	pos := ss.Positions["ETH"]
	if pos == nil {
		t.Fatal("expected ETH position to remain after partial close")
	}
	if math.Abs(pos.Quantity-onChainQty) > 1e-9 {
		t.Errorf("Quantity = %g, want %g (post-partial residual)", pos.Quantity, onChainQty)
	}
	if pos.InitialQuantity != fullQty {
		t.Errorf("InitialQuantity = %g, want %g (preserved)", pos.InitialQuantity, fullQty)
	}

	if len(ss.TradeHistory) != 1 {
		t.Fatalf("TradeHistory = %d, want 1 close trade", len(ss.TradeHistory))
	}
	trade := ss.TradeHistory[0]
	if !trade.IsClose {
		t.Error("trade.IsClose = false, want true")
	}
	if math.Abs(trade.Price-expectedTP1) > 1e-9 {
		t.Errorf("trade.Price = %g, want %g (TP1 price from tieredTPATRPrices)", trade.Price, expectedTP1)
	}
	if math.Abs(trade.Quantity-(fullQty-onChainQty)) > 1e-9 {
		t.Errorf("trade.Quantity = %g, want %g (drop qty)", trade.Quantity, fullQty-onChainQty)
	}
	if trade.Side != "sell" {
		t.Errorf("trade.Side = %q, want %q (long-close = sell)", trade.Side, "sell")
	}
	if trade.ExchangeOrderID != "" {
		t.Errorf("trade.ExchangeOrderID = %q, want \"\" (no fill fee, no fabricated OID)", trade.ExchangeOrderID)
	}
	// PnL = (TP1 - AvgCost) * dropQty - fee. fee = modeled spot fee.
	wantPnLBeforeFee := (expectedTP1 - entryPx) * (fullQty - onChainQty)
	if trade.RealizedPnL > wantPnLBeforeFee {
		t.Errorf("RealizedPnL = %g should not exceed PnL-before-fee %g", trade.RealizedPnL, wantPnLBeforeFee)
	}

	if len(alerts) != 1 {
		t.Fatalf("pendingAlerts = %d, want 1", len(alerts))
	}
	a := alerts[0]
	if a.FillType != "TP1" {
		t.Errorf("FillType = %q, want TP1", a.FillType)
	}
	if !a.IsPartial {
		t.Error("IsPartial = false, want true (residual remains)")
	}
	if math.Abs(a.FillPrice-expectedTP1) > 1e-9 {
		t.Errorf("FillPrice = %g, want %g", a.FillPrice, expectedTP1)
	}
	if math.Abs(a.RemainingQty-onChainQty) > 1e-9 {
		t.Errorf("RemainingQty = %g, want %g", a.RemainingQty, onChainQty)
	}
}

// TestSoleOwnerTPPartial_PrefersUserFillsPxOverConfiguredTP verifies that when
// the userFills resolver returns a real fill price, that price is used for the
// close trade in preference to the configured TP price — covers the
// "(not mark + size-matched fallback)" requirement in #670.
func TestSoleOwnerTPPartial_PrefersUserFillsPxOverConfiguredTP(t *testing.T) {
	const (
		entryPx    = 2000.0
		entryATR   = 50.0
		fullQty    = 0.4
		onChainQty = 0.2
		actualPx   = 2105.25 // slightly above configured TP1 (2100)
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
		// Match the partial drop qty (fullQty - onChainQty = 0.2).
		if math.Abs(qty-0.2) < 1e-6 {
			return HLFillLookup{Fee: actualFee, FilledQty: 0.2, Px: actualPx, OID: 999, Count: 1}, true
		}
		return HLFillLookup{}, false
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)

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
	// Realized PnL = (actualPx - entryPx) * 0.2 - actualFee
	wantPnL := (actualPx-entryPx)*0.2 - actualFee
	if math.Abs(trade.RealizedPnL-wantPnL) > 1e-6 {
		t.Errorf("RealizedPnL = %g, want %g", trade.RealizedPnL, wantPnL)
	}
}

// TestSoleOwnerTPFinal_FullCloseAtTPPrice_NotSL verifies the bug fix described
// in #670 issue #3: when the final TP tier flattens a sole-owner position AND
// the SL OID is still set (race: HL hasn't auto-cancelled the reduce-only SL
// trigger between the TP fill and the next reconcile cycle), the close must be
// attributed to the TP price, not the SL trigger price.
func TestSoleOwnerTPFinal_FullCloseAtTPPrice_NotSL(t *testing.T) {
	const (
		entryPx     = 2000.0
		entryATR    = 50.0
		fullQty     = 0.2
		slTriggerPx = 1900.0                 // far from TP — exposes a wrong-price booking
		expectedTP2 = entryPx + 3.0*entryATR // long final tier @ 3× → 2150
	)
	ss := &StrategyState{
		ID:   "hl-tp-sole",
		Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: fullQty, InitialQuantity: fullQty,
				AvgCost: entryPx, EntryATR: entryATR, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
				// All TP OIDs cleared (final tier filled, sole peer); SL OID
				// still positive — the auto-cancel hasn't propagated yet.
				TPOIDs:            []int64{0, 0},
				StopLossOID:       42,
				StopLossTriggerPx: slTriggerPx,
			},
		},
	}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	changed := reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", nil, resolver, logger, &alerts)
	if !changed {
		t.Fatal("expected changed=true")
	}
	if _, open := ss.Positions["ETH"]; open {
		t.Error("position should be closed after final TP")
	}
	if len(ss.TradeHistory) != 1 {
		t.Fatalf("TradeHistory = %d, want 1", len(ss.TradeHistory))
	}
	trade := ss.TradeHistory[0]
	if math.Abs(trade.Price-expectedTP2) > 1e-9 {
		t.Errorf("trade.Price = %g, want %g (TP2 final tier, NOT SL trigger %g)", trade.Price, expectedTP2, slTriggerPx)
	}
	if len(alerts) != 1 || alerts[0].FillType != "TP2" {
		t.Errorf("alert = %+v, want one TP2 alert", alerts)
	}
}

// TestSoleOwnerTPFinal_PartialCloseShort verifies the short-side mirror of the
// partial path — drop sign math + TP price formula.
func TestSoleOwnerTPFinal_PartialCloseShort(t *testing.T) {
	const (
		entryPx     = 2000.0
		entryATR    = 50.0
		fullQty     = 0.4
		onChainQty  = -0.2 // signed: short residual
		expectedTP1 = entryPx - 2.0*entryATR
	)
	ss := &StrategyState{
		ID:   "hl-tp-sole",
		Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: fullQty, InitialQuantity: fullQty,
				AvgCost: entryPx, EntryATR: entryATR, Side: "short",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
				TPOIDs: []int64{0, 222},
			},
		},
	}
	positions := []HLPosition{{Coin: "ETH", Size: onChainQty, EntryPrice: entryPx, Leverage: 5}}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)

	pos := ss.Positions["ETH"]
	if pos == nil {
		t.Fatal("expected position to remain after partial")
	}
	if math.Abs(pos.Quantity-0.2) > 1e-9 {
		t.Errorf("Quantity = %g, want 0.2", pos.Quantity)
	}
	if len(ss.TradeHistory) != 1 {
		t.Fatalf("TradeHistory = %d, want 1", len(ss.TradeHistory))
	}
	trade := ss.TradeHistory[0]
	if math.Abs(trade.Price-expectedTP1) > 1e-9 {
		t.Errorf("trade.Price = %g, want %g (short TP1 below entry)", trade.Price, expectedTP1)
	}
	if trade.Side != "buy" {
		t.Errorf("trade.Side = %q, want %q (short-close = buy)", trade.Side, "buy")
	}
}

// TestSoleOwnerTP_SkipsWhenNoTierCleared verifies that the new helper falls
// through to the legacy reconciler when nothing in TPOIDs has cleared. Without
// this, every qty drift (e.g. funding adjustment) would be mis-attributed to a
// TP fill.
func TestSoleOwnerTP_SkipsWhenNoTierCleared(t *testing.T) {
	ss := &StrategyState{
		ID:   "hl-tp-sole",
		Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: 0.4, InitialQuantity: 0.4,
				AvgCost: 2000, EntryATR: 50, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
				TPOIDs: []int64{111, 222}, // both still active — no clear
			},
		},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.2, EntryPrice: 2000, Leverage: 5}}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)

	if len(ss.TradeHistory) != 0 {
		t.Errorf("TradeHistory = %d, want 0 (no TP cleared, legacy resync should be silent)", len(ss.TradeHistory))
	}
	if len(alerts) != 0 {
		t.Errorf("alerts = %d, want 0", len(alerts))
	}
	// Legacy reconciler resyncs qty.
	if math.Abs(ss.Positions["ETH"].Quantity-0.2) > 1e-9 {
		t.Errorf("Quantity = %g, want 0.2 (legacy resync)", ss.Positions["ETH"].Quantity)
	}
}

// TestSoleOwnerTP_SkipsWhenAvgCostOrATRMissing — TP price computation needs
// both inputs; without them the helper must fall through silently.
func TestSoleOwnerTP_SkipsWhenAvgCostOrATRMissing(t *testing.T) {
	ss := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: 0.4, AvgCost: 2000, EntryATR: 0,
				Side: "long", Multiplier: 1, OwnerStrategyID: "hl-tp-sole",
				TPOIDs: []int64{0, 222},
			},
		},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.2, EntryPrice: 2000}}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)
	if len(ss.TradeHistory) != 0 {
		t.Errorf("TradeHistory = %d, want 0 (missing EntryATR)", len(ss.TradeHistory))
	}
}

// --- HLFillLookup px aggregation ---

func TestLookupHyperliquidFillByOID_AggregatesPxAsSizeWeightedAvg(t *testing.T) {
	prevFetcher := fetchHyperliquidUserFillsByTime
	defer func() { fetchHyperliquidUserFillsByTime = prevFetcher }()
	prevRetries, prevDelay := hlFillLookupRetries, hlFillLookupRetryDelay
	hlFillLookupRetries, hlFillLookupRetryDelay = 1, 0
	defer func() {
		hlFillLookupRetries = prevRetries
		hlFillLookupRetryDelay = prevDelay
	}()

	// One OID across two partial fills at different prices: 0.1@2100 + 0.3@2104
	// → weighted avg = (0.1*2100 + 0.3*2104) / 0.4 = 841.2 / 0.4 = 2103.0
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

// TestSoleOwnerTPPartial_FallsBackToConfiguredTPWhenLookupPxZero verifies that
// the resolver's `useFillFee=true` branch with `lookup.Px <= 0` (real fee
// available but no usable price — e.g. fee aggregated from fragments without
// price data) falls back to the configured TP price rather than booking at $0.
// Pairs with the userFills-px-prefer test to lock in both branches of the
// `useFillFee && lookup.Px > 0` guard in tryBookSoleOwnerTPFill.
func TestSoleOwnerTPPartial_FallsBackToConfiguredTPWhenLookupPxZero(t *testing.T) {
	const (
		entryPx     = 2000.0
		entryATR    = 50.0
		fullQty     = 0.4
		onChainQty  = 0.2
		realFee     = 0.07
		expectedTP1 = entryPx + 2.0*entryATR // 2100
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
		// Real fee returned, but Px=0 (e.g. only fee aggregation succeeded).
		return HLFillLookup{Fee: realFee, FilledQty: 0.2, Px: 0, OID: 555, Count: 1}, true
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)

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

// TestSoleOwnerTP_FullCloseWithStaleClearedTier_DefersToSL is the regression
// test for review finding #1: when a prior TP1 partial has already booked
// (state: TPOIDs=[0, 222], Quantity=residual) and the residual is then closed
// by a different mechanism — SL fire, operator close, kill-switch — the
// helper must NOT mis-attribute the close to TP1. It must defer to the legacy
// SL-owner branch, which books at the SL trigger price with a "stop_loss"
// close reason (not "TP1" alert).
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
				// TP1 already booked from a prior cycle's partial fill;
				// TP2 still active. SL OID still positive — operator/SL/CB
				// just flattened the residual.
				TPOIDs:            []int64{0, 222},
				StopLossOID:       42,
				StopLossTriggerPx: slTriggerPx,
			},
		},
	}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	changed := reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", nil, resolver, logger, &alerts)
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
	// No TP alert — SL-owner branch records the close reason but does not
	// emit a ProtectionFillAlert; that's the existing legacy behavior.
	for _, a := range alerts {
		if a.FillType == "TP1" || a.FillType == "TP2" {
			t.Errorf("unexpected TP alert %+v — SL close must not mis-attribute to a TP tier", a)
		}
	}
}

// TestSoleOwnerTP_TwoCycleSequence_BooksAfterProtectionSyncZerosTPOID
// exercises the cleared-tier path (no userFills hit available — e.g. indexer
// down, or this strategy is not perps-paper-with-real-fees). When the
// userFills resolver returns nothing, the cycle-ordering recovery path can't
// fire (it needs an OID match), so attribution still depends on
// protection-sync zeroing pos.TPOIDs[i] before reconcile sees the drift.
//
// Cycle 1 models the post-fill, pre-protection-sync state where TPOIDs are
// still all positive AND userFills returns nothing — the helper declines and
// legacy resync runs. Cycle 2 manually resets pos.Quantity to model the path
// where the strategy was NOT in hlReconcileDue for cycle 1, then advances
// pos.TPOIDs[0]=0 to model post-protection-sync state — attribution fires.
//
// When the userFills resolver IS populated (the production case for HL live),
// the cycle-ordering recovery path in tryBookSoleOwnerTPFill books the same
// fill in ONE cycle — see TestSoleOwnerTP_CycleOrderingRecovery_*.
func TestSoleOwnerTP_TwoCycleSequence_BooksAfterProtectionSyncZerosTPOID(t *testing.T) {
	const (
		entryPx     = 2000.0
		entryATR    = 50.0
		fullQty     = 0.4
		onChainQty  = 0.2
		expectedTP1 = entryPx + 2.0*entryATR
	)
	pos := &Position{
		Symbol: "ETH", Quantity: fullQty, InitialQuantity: fullQty,
		AvgCost: entryPx, EntryATR: entryATR, Side: "long",
		Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
		TPOIDs: []int64{111, 222}, // cycle 1: still all positive
	}
	ss := &StrategyState{
		ID: "hl-tp-sole", Cash: 100,
		Positions: map[string]*Position{"ETH": pos},
	}
	positions := []HLPosition{{Coin: "ETH", Size: onChainQty, EntryPrice: entryPx, Leverage: 5}}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		return HLFillLookup{}, false
	})
	logger := newTestLogger(t)

	// Cycle 1: protection-sync hasn't run yet → no cleared tier → helper
	// declines. Legacy reconciler runs and resyncs qty silently (existing
	// behavior — the fix relies on protection-sync running first in the
	// non-due cycle case to preserve the drift signal).
	var alerts []ProtectionFillAlert
	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)
	if len(alerts) != 0 {
		t.Errorf("cycle 1 alerts = %d, want 0 (no cleared TP tier, must not attribute)", len(alerts))
	}
	if len(ss.TradeHistory) != 0 {
		t.Errorf("cycle 1 trades = %d, want 0", len(ss.TradeHistory))
	}

	// Simulate protection-sync running between cycles (zeros TPOIDs[0] for
	// the externally filled tier). In production the strategy must NOT have
	// been in hlReconcileDue at cycle 1 for this state to be visible at
	// cycle 2 with drift intact — otherwise legacy resync above already
	// shrank pos.Quantity. Reset the qty here to model that path.
	pos.Quantity = fullQty
	pos.TPOIDs = []int64{0, 222}

	// Cycle 2: TPOIDs[0] cleared, drift visible → attribution fires.
	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)
	if len(ss.TradeHistory) != 1 {
		t.Fatalf("cycle 2 trades = %d, want 1 (TP1 attribution after protection-sync)", len(ss.TradeHistory))
	}
	if math.Abs(ss.TradeHistory[0].Price-expectedTP1) > 1e-9 {
		t.Errorf("cycle 2 trade.Price = %g, want %g", ss.TradeHistory[0].Price, expectedTP1)
	}
	if len(alerts) != 1 || alerts[0].FillType != "TP1" {
		t.Errorf("cycle 2 alerts = %+v, want one TP1 alert", alerts)
	}
}

func TestLookupHyperliquidFillByCoinSize_PopulatesPx(t *testing.T) {
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
			{Coin: "ETH", Sz: "0.2", Px: "2105.25", OID: "777", Fee: "0.04", Time: 100},
		}, nil
	}

	lookup, ok := lookupHyperliquidFillByCoinSize("0xacct", "ETH", 0.2, 1e-4, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if math.Abs(lookup.Px-2105.25) > 1e-9 {
		t.Errorf("Px = %g, want 2105.25", lookup.Px)
	}
}

// TestSoleOwnerTP_CycleOrderingRecovery_BooksWhenUserFillsOIDMatchesTPOID is
// the regression test for the production cycle-ordering window the reviewer
// flagged: a TP fills between cycle N's protection-sync and cycle N+1's
// reconcile snapshot. At cycle N+1 reconcile time, pos.TPOIDs are still all
// positive (protection-sync hasn't run for cycle N+1 yet) BUT the userFills
// resolver has already pre-fetched the matching fill record. The recovery
// path must attribute the drift to the matched tier in ONE cycle — without
// it, legacy resync would silently wipe the drift before protection-sync
// gets to observe it (see #672).
func TestSoleOwnerTP_CycleOrderingRecovery_BooksWhenUserFillsOIDMatchesTPOID(t *testing.T) {
	const (
		entryPx     = 2000.0
		entryATR    = 50.0
		fullQty     = 0.4
		onChainQty  = 0.2
		tp1OID      = int64(111)
		fillPx      = 2103.50
		fillFee     = 0.05
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
				// Both TPOIDs still positive — protection-sync hasn't observed
				// the fill yet for this cycle.
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
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	changed := reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)
	if !changed {
		t.Fatal("expected changed=true")
	}
	if len(ss.TradeHistory) != 1 {
		t.Fatalf("TradeHistory = %d, want 1 (cycle-ordering recovery should book in one cycle)", len(ss.TradeHistory))
	}
	trade := ss.TradeHistory[0]
	if math.Abs(trade.Price-fillPx) > 1e-9 {
		t.Errorf("trade.Price = %g, want %g (userFills px preferred over configured TP %g)", trade.Price, fillPx, expectedTP1)
	}
	if math.Abs(trade.ExchangeFee-fillFee) > 1e-9 {
		t.Errorf("trade.ExchangeFee = %g, want %g", trade.ExchangeFee, fillFee)
	}
	if len(alerts) != 1 || alerts[0].FillType != "TP1" {
		t.Errorf("alerts = %+v, want one TP1 alert", alerts)
	}
	pos := ss.Positions["ETH"]
	if pos == nil {
		t.Fatal("expected position to remain after partial close")
	}
	// TPOIDs are NOT zeroed inline — protection-sync zeros them later this
	// cycle via applyHyperliquidProtectionSync (idempotent). Asserting the
	// invariant locks in the contract with the protection-sync plan-builder.
	if pos.TPOIDs[0] != tp1OID || pos.TPOIDs[1] != 222 {
		t.Errorf("TPOIDs = %v, want [%d 222] (recovery path must not zero TPOIDs inline)", pos.TPOIDs, tp1OID)
	}
}

// TestSoleOwnerTP_CycleOrderingRecovery_DefersWhenUserFillsOIDDoesNotMatch
// verifies that the recovery path's OID match is precise — a fill from a
// different mechanism (SL trigger, operator close, unrelated peer order)
// returns an OID that does not equal any TPOID, so the helper declines and
// the legacy reconciler resyncs the qty silently. Critical for safety: a
// looser match would mis-attribute SL fires to TP tiers.
func TestSoleOwnerTP_CycleOrderingRecovery_DefersWhenUserFillsOIDDoesNotMatch(t *testing.T) {
	ss := &StrategyState{
		ID:   "hl-tp-sole",
		Cash: 100,
		Positions: map[string]*Position{
			"ETH": {
				Symbol: "ETH", Quantity: 0.4, InitialQuantity: 0.4,
				AvgCost: 2000, EntryATR: 50, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-tp-sole",
				TPOIDs: []int64{111, 222}, // both still positive
			},
		},
	}
	positions := []HLPosition{{Coin: "ETH", Size: 0.2, EntryPrice: 2000, Leverage: 5}}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		// Some other order (e.g. SL OID, peer market order) — OID does not
		// match either TPOID.
		return HLFillLookup{Fee: 0.04, FilledQty: 0.2, Px: 1900, OID: 999, Count: 1}, true
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", positions, resolver, logger, &alerts)

	if len(ss.TradeHistory) != 0 {
		t.Errorf("TradeHistory = %d, want 0 (OID mismatch must not attribute to TP)", len(ss.TradeHistory))
	}
	if len(alerts) != 0 {
		t.Errorf("alerts = %d, want 0", len(alerts))
	}
	// Legacy resync runs.
	if math.Abs(ss.Positions["ETH"].Quantity-0.2) > 1e-9 {
		t.Errorf("Quantity = %g, want 0.2 (legacy resync after recovery declines)", ss.Positions["ETH"].Quantity)
	}
}

// TestSoleOwnerTP_CycleOrderingRecovery_NotAppliedToFullClose verifies that
// recovery is restricted to the partial path. Even when on-chain is flat AND
// userFills returns an OID matching pos.TPOIDs, the helper must still decline
// (full close requires all-tiers-cleared per #672 finding 1) and defer to
// the legacy SL-owner branch. Otherwise an SL fire mis-cancelling a stale TP
// trigger could be mis-attributed.
func TestSoleOwnerTP_CycleOrderingRecovery_NotAppliedToFullClose(t *testing.T) {
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
				// TPOIDs[0] cleared earlier; TPOIDs[1] still active — i.e. the
				// post-TP1-fill steady state. SL OID still set.
				TPOIDs:            []int64{0, 222},
				StopLossOID:       42,
				StopLossTriggerPx: slTriggerPx,
			},
		},
	}
	resolver := hlReconcileFillResolver(func(string, int64, float64) (HLFillLookup, bool) {
		// Stale fill record matching tier 1 OID is irrelevant — full close
		// path must defer regardless.
		return HLFillLookup{Fee: 0.04, FilledQty: residualQty, Px: 2150, OID: 222, Count: 1}, true
	})
	var alerts []ProtectionFillAlert
	logger := newTestLogger(t)

	reconcileHyperliquidPositionsForStrategy(soleOwnerTPSC(), ss, "ETH", nil, resolver, logger, &alerts)

	if len(ss.TradeHistory) != 1 {
		t.Fatalf("TradeHistory = %d, want 1", len(ss.TradeHistory))
	}
	trade := ss.TradeHistory[0]
	if math.Abs(trade.Price-slTriggerPx) > 1e-9 {
		t.Errorf("trade.Price = %g, want %g (legacy SL-owner branch should book at SL trigger)", trade.Price, slTriggerPx)
	}
	if len(ss.ClosedPositions) != 1 || ss.ClosedPositions[0].CloseReason != "stop_loss" {
		t.Errorf("CloseReason = %q, want \"stop_loss\"", ss.ClosedPositions[0].CloseReason)
	}
	for _, a := range alerts {
		if a.FillType == "TP1" || a.FillType == "TP2" {
			t.Errorf("unexpected TP alert %+v — full close must defer to SL handler even with matching userFills OID", a)
		}
	}
}
