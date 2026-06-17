package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Tests in this file mutate package-level hlMainnetURL / fetchHyperliquidUserFillsByTime
// and must NOT use t.Parallel().

func newHLUserFillsServer(t *testing.T, fills []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fills)
	}))
}

func withFastFillRetries(t *testing.T) {
	t.Helper()
	origRetries := hlFillLookupRetries
	origDelay := hlFillLookupRetryDelay
	hlFillLookupRetries = 1
	hlFillLookupRetryDelay = 0
	t.Cleanup(func() {
		hlFillLookupRetries = origRetries
		hlFillLookupRetryDelay = origDelay
	})
}

func TestLookupHyperliquidFillByOID_AggregatesPartialFills(t *testing.T) {
	withFastFillRetries(t)
	srv := newHLUserFillsServer(t, []map[string]any{
		{"coin": "BTC", "oid": 12345, "fee": "0.50", "closedPnl": "100.00", "sz": "0.1"},
		{"coin": "BTC", "oid": 12345, "fee": "0.30", "closedPnl": "50.00", "sz": "0.05"},
		{"coin": "BTC", "oid": 99999, "fee": "1.00", "closedPnl": "200.00", "sz": "0.2"}, // unrelated
	})
	defer srv.Close()
	origURL := hlMainnetURL
	hlMainnetURL = srv.URL
	defer func() { hlMainnetURL = origURL }()

	got, ok := lookupHyperliquidFillByOID("0xtest", 12345, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Count != 2 {
		t.Errorf("Count = %d, want 2", got.Count)
	}
	if got.Fee < 0.799 || got.Fee > 0.801 {
		t.Errorf("Fee = %g, want ~0.80", got.Fee)
	}
	if got.ClosedPnLGross < 149.99 || got.ClosedPnLGross > 150.01 {
		t.Errorf("ClosedPnLGross = %g, want ~150.00", got.ClosedPnLGross)
	}
}

func TestLookupHyperliquidFillByOID_NoMatchReturnsFalse(t *testing.T) {
	withFastFillRetries(t)
	srv := newHLUserFillsServer(t, []map[string]any{
		{"coin": "BTC", "oid": 99999, "fee": "1.00", "sz": "0.2"},
	})
	defer srv.Close()
	origURL := hlMainnetURL
	hlMainnetURL = srv.URL
	defer func() { hlMainnetURL = origURL }()

	if _, ok := lookupHyperliquidFillByOID("0xtest", 12345, 0); ok {
		t.Error("expected ok=false for missing OID")
	}
}

func TestLookupHyperliquidFillByOID_EmptyAddressShortCircuits(t *testing.T) {
	withFastFillRetries(t)
	if _, ok := lookupHyperliquidFillByOID("", 12345, 0); ok {
		t.Error("expected ok=false for empty address")
	}
}

func TestLookupHyperliquidFillByCoinSize_MatchesByCoinAndSize(t *testing.T) {
	withFastFillRetries(t)
	srv := newHLUserFillsServer(t, []map[string]any{
		{"coin": "BTC", "oid": 1, "fee": "0.40", "closedPnl": "75.00", "sz": "0.123456"},
		{"coin": "ETH", "oid": 2, "fee": "0.10", "closedPnl": "5.00", "sz": "0.123456"}, // wrong coin
		{"coin": "BTC", "oid": 3, "fee": "0.20", "closedPnl": "10.00", "sz": "0.5"},     // wrong size
	})
	defer srv.Close()
	origURL := hlMainnetURL
	hlMainnetURL = srv.URL
	defer func() { hlMainnetURL = origURL }()

	got, ok := lookupHyperliquidFillByCoinSize("0xtest", "BTC", 0.123456, 1e-4, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Count != 1 {
		t.Errorf("Count = %d, want 1", got.Count)
	}
	if got.Fee < 0.399 || got.Fee > 0.401 {
		t.Errorf("Fee = %g, want ~0.40", got.Fee)
	}
}

func TestLookupHyperliquidFillByCoinSize_PicksNewestGroupNotSumOfWindow(t *testing.T) {
	// Regression for #596: two unrelated same-size BTC closes within the
	// 24h window must not be summed. The fallback should anchor on the
	// newest matching fill and aggregate only that OID's group.
	withFastFillRetries(t)
	srv := newHLUserFillsServer(t, []map[string]any{
		{"coin": "BTC", "oid": 100, "fee": "0.40", "closedPnl": "75.00", "sz": "0.1", "time": 1_000_000_000},
		{"coin": "BTC", "oid": 200, "fee": "0.50", "closedPnl": "90.00", "sz": "0.1", "time": 2_000_000_000},
	})
	defer srv.Close()
	origURL := hlMainnetURL
	hlMainnetURL = srv.URL
	defer func() { hlMainnetURL = origURL }()

	got, ok := lookupHyperliquidFillByCoinSize("0xtest", "BTC", 0.1, 1e-4, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Count != 1 {
		t.Errorf("Count = %d, want 1 (only newest OID's group)", got.Count)
	}
	if got.OID != 200 {
		t.Errorf("OID = %d, want 200 (newest fill)", got.OID)
	}
	if got.Fee < 0.499 || got.Fee > 0.501 {
		t.Errorf("Fee = %g, want ~0.50 (newest only, not 0.40+0.50)", got.Fee)
	}
	if got.ClosedPnLGross < 89.99 || got.ClosedPnLGross > 90.01 {
		t.Errorf("ClosedPnLGross = %g, want ~90.00 (newest only)", got.ClosedPnLGross)
	}
}

func TestLookupHyperliquidFillByCoinSize_AggregatesPartialFillsByAnchorOID(t *testing.T) {
	// When the newest matching fill has partial siblings sharing its OID,
	// fee/closedPnl should aggregate across the whole group.
	withFastFillRetries(t)
	srv := newHLUserFillsServer(t, []map[string]any{
		{"coin": "BTC", "oid": 555, "fee": "0.20", "closedPnl": "30.00", "sz": "0.04", "time": 1_500_000_000}, // partial of OID 555
		{"coin": "BTC", "oid": 555, "fee": "0.30", "closedPnl": "40.00", "sz": "0.10", "time": 2_000_000_000}, // anchor
		{"coin": "BTC", "oid": 999, "fee": "0.99", "closedPnl": "99.00", "sz": "0.10", "time": 1_000_000_000}, // older same-size, different OID — must be ignored
	})
	defer srv.Close()
	origURL := hlMainnetURL
	hlMainnetURL = srv.URL
	defer func() { hlMainnetURL = origURL }()

	got, ok := lookupHyperliquidFillByCoinSize("0xtest", "BTC", 0.10, 1e-4, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.OID != 555 {
		t.Errorf("OID = %d, want 555 (newest matching anchor)", got.OID)
	}
	if got.Count != 2 {
		t.Errorf("Count = %d, want 2 (both fills sharing OID 555)", got.Count)
	}
	if got.Fee < 0.499 || got.Fee > 0.501 {
		t.Errorf("Fee = %g, want ~0.50 (0.20+0.30, not including OID 999)", got.Fee)
	}
	if got.ClosedPnLGross < 69.99 || got.ClosedPnLGross > 70.01 {
		t.Errorf("ClosedPnLGross = %g, want ~70.00 (30+40, not including OID 999)", got.ClosedPnLGross)
	}
}

func TestLookupHyperliquidReconcileFillFee_OIDFirstFallsBackToCoinSize(t *testing.T) {
	withFastFillRetries(t)
	// First call (OID lookup) returns no match; second call (coin+size) returns hit.
	calls := 0
	orig := fetchHyperliquidUserFillsByTime
	defer func() { fetchHyperliquidUserFillsByTime = orig }()
	fetchHyperliquidUserFillsByTime = func(addr string, sinceMs int64) ([]hlFillRecord, error) {
		calls++
		switch calls {
		case 1:
			// No match for OID 999.
			return []hlFillRecord{
				{Coin: "BTC", OID: "1", Fee: "0.10", Sz: "0.5"},
			}, nil
		default:
			// Coin+size match for BTC@0.5.
			return []hlFillRecord{
				{Coin: "BTC", OID: "1", Fee: "0.10", ClosedPnl: "20.00", Sz: "0.5"},
			}, nil
		}
	}

	got, ok := lookupHyperliquidReconcileFillFee("0xtest", "BTC", 999, 0.5)
	if !ok {
		t.Fatal("expected ok=true via coin+size fallback")
	}
	if got.Fee < 0.099 || got.Fee > 0.101 {
		t.Errorf("Fee = %g, want ~0.10", got.Fee)
	}
}

func TestReconcileHyperliquidPositions_ExternalCloseUsesFillFee(t *testing.T) {
	// Stub the fee lookup to return a known fee for the SL trigger OID.
	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(addr, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		// #685: include FilledQty so the SL-confirmed gate accepts the fill.
		if oid == 4242 && coin == "BTC" {
			return HLFillLookup{Fee: 1.23, ClosedPnLGross: 0, FilledQty: 0.1, Px: 58000, Count: 1, OID: oid}, true
		}
		return HLFillLookup{}, false
	}

	s := &StrategyState{
		ID: "hl-test", Platform: "hyperliquid", Type: "perps", Cash: 10000,
		Positions: map[string]*Position{
			"BTC": {
				Symbol: "BTC", Quantity: 0.1, AvgCost: 60000, Side: "long",
				Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-test",
				StopLossOID: 4242, StopLossTriggerPx: 58000,
			},
		},
	}
	logMgr, _ := NewLogManager(t.TempDir())
	logger, _ := logMgr.GetStrategyLogger("hl-test")

	changed := reconcileHyperliquidPositions(s, "BTC", nil, "0xtest", logger)
	if !changed {
		t.Fatal("expected changed=true")
	}

	// One open trade (none in this test) + one close trade.
	if len(s.TradeHistory) != 1 {
		t.Fatalf("TradeHistory = %d, want 1", len(s.TradeHistory))
	}
	closeTrade := s.TradeHistory[0]
	if !closeTrade.IsClose {
		t.Error("expected IsClose=true on the booked close trade")
	}
	if closeTrade.ExchangeFee < 1.229 || closeTrade.ExchangeFee > 1.231 {
		t.Errorf("ExchangeFee = %g, want ~1.23 (real fill fee from userFills)", closeTrade.ExchangeFee)
	}
	if closeTrade.ExchangeOrderID != "4242" {
		t.Errorf("ExchangeOrderID = %q, want %q", closeTrade.ExchangeOrderID, "4242")
	}
}

func TestReconcileHyperliquidAccountPositions_DetectorOneUsesFillFee(t *testing.T) {
	// Detector 1 (Full external close on shared coin): SL owner gets OID-keyed
	// fee lookup; non-owner peer gets the coin-level external fill split by
	// virtual qty.
	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(addr, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 5005 {
			// #756: FilledQty + OID echo required for shared-coin SL attribution.
			return HLFillLookup{Fee: 2.50, Count: 1, OID: oid, FilledQty: 0.2}, true
		}
		if oid == 0 && coin == "BTC" && qty > 0 {
			return HLFillLookup{Fee: 0.75, Count: 1}, true
		}
		return HLFillLookup{}, false
	}

	state := &AppState{
		Strategies: map[string]*StrategyState{
			"hl-owner": {
				ID: "hl-owner", Platform: "hyperliquid", Type: "perps", Cash: 5000,
				Positions: map[string]*Position{
					"BTC": {
						Symbol: "BTC", Quantity: 0.2, AvgCost: 60000, Side: "long",
						Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-owner",
						StopLossOID: 5005, StopLossTriggerPx: 58000,
					},
				},
			},
			"hl-peer": {
				ID: "hl-peer", Platform: "hyperliquid", Type: "perps", Cash: 5000,
				Positions: map[string]*Position{
					"BTC": {
						Symbol: "BTC", Quantity: 0.1, AvgCost: 60500, Side: "long",
						Multiplier: 1, Leverage: 5, OwnerStrategyID: "hl-peer",
					},
				},
			},
		},
	}
	scs := []StrategyConfig{
		{ID: "hl-owner", Platform: "hyperliquid", Type: "perps", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Leverage: 5},
		{ID: "hl-peer", Platform: "hyperliquid", Type: "perps", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Leverage: 5},
	}
	logMgr, _ := NewLogManager(t.TempDir())
	var mu sync.RWMutex

	prices := map[string]float64{"BTC": 59000}
	// nil on-chain positions => Detector 1 fires for both peers.
	_, _, _ = reconcileHyperliquidAccountPositions(scs, scs, state, &mu, logMgr, nil, prices, "0xtest", nil, false)

	ownerSS := state.Strategies["hl-owner"]
	if _, open := ownerSS.Positions["BTC"]; open {
		t.Error("owner BTC position should have been closed")
	}
	if len(ownerSS.TradeHistory) != 1 {
		t.Fatalf("owner TradeHistory = %d, want 1", len(ownerSS.TradeHistory))
	}
	if ownerSS.TradeHistory[0].ExchangeFee < 2.499 || ownerSS.TradeHistory[0].ExchangeFee > 2.501 {
		t.Errorf("owner ExchangeFee = %g, want ~2.50 (OID-keyed)", ownerSS.TradeHistory[0].ExchangeFee)
	}

	peerSS := state.Strategies["hl-peer"]
	if _, open := peerSS.Positions["BTC"]; open {
		t.Error("peer BTC position should have been closed")
	}
	if len(peerSS.TradeHistory) != 1 {
		t.Fatalf("peer TradeHistory = %d, want 1", len(peerSS.TradeHistory))
	}
	if peerSS.TradeHistory[0].ExchangeFee < 0.249 || peerSS.TradeHistory[0].ExchangeFee > 0.251 {
		t.Errorf("peer ExchangeFee = %g, want ~0.25 (aggregate split)", peerSS.TradeHistory[0].ExchangeFee)
	}
}

func TestReconcileFillLookupSinceMs_BoundsTo24h(t *testing.T) {
	now := time.Now().UTC()
	got := reconcileFillLookupSinceMs(now)
	want := now.Add(-hlReconcileFillLookupWindow).UnixMilli()
	if got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

// #621: FilledQty must be accumulated across all OID-matching fill records.
func TestLookupHyperliquidFillByOID_AccumulatesFilledQty(t *testing.T) {
	withFastFillRetries(t)
	srv := newHLUserFillsServer(t, []map[string]any{
		{"coin": "ETH", "oid": 55555, "fee": "0.10", "closedPnl": "5.00", "sz": "0.211"},
		{"coin": "ETH", "oid": 55555, "fee": "0.05", "closedPnl": "2.50", "sz": "0.100"},
		{"coin": "ETH", "oid": 99999, "fee": "1.00", "closedPnl": "50.00", "sz": "0.422"}, // unrelated
	})
	defer srv.Close()
	origURL := hlMainnetURL
	hlMainnetURL = srv.URL
	defer func() { hlMainnetURL = origURL }()

	got, ok := lookupHyperliquidFillByOID("0xtest", 55555, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.Count != 2 {
		t.Errorf("Count = %d, want 2", got.Count)
	}
	wantQty := 0.311
	if got.FilledQty < wantQty-1e-9 || got.FilledQty > wantQty+1e-9 {
		t.Errorf("FilledQty = %g, want %g", got.FilledQty, wantQty)
	}
}

// #621: FilledQty must be set in the coin+size fallback (no-OID anchor case).
func TestLookupHyperliquidFillByCoinSize_SetsFilledQtyNoOIDCase(t *testing.T) {
	withFastFillRetries(t)
	srv := newHLUserFillsServer(t, []map[string]any{
		{"coin": "ETH", "oid": nil, "fee": "0.08", "closedPnl": "4.00", "sz": "0.211", "time": 1000},
	})
	defer srv.Close()
	origURL := hlMainnetURL
	hlMainnetURL = srv.URL
	defer func() { hlMainnetURL = origURL }()

	got, ok := lookupHyperliquidFillByCoinSize("0xtest", "ETH", 0.211, 1e-4, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got.FilledQty < 0.211-1e-9 || got.FilledQty > 0.211+1e-9 {
		t.Errorf("FilledQty = %g, want 0.211", got.FilledQty)
	}
}

// #621: FilledQty must be accumulated across OID-aggregation fills in the
// coin+size fallback when the anchor has a valid OID.
func TestLookupHyperliquidFillByCoinSize_AccumulatesFilledQtyWithOID(t *testing.T) {
	withFastFillRetries(t)
	srv := newHLUserFillsServer(t, []map[string]any{
		{"coin": "ETH", "oid": 77777, "fee": "0.05", "closedPnl": "2.00", "sz": "0.100", "time": 2000},
		{"coin": "ETH", "oid": 77777, "fee": "0.06", "closedPnl": "3.00", "sz": "0.111", "time": 1900},
		{"coin": "ETH", "oid": 88888, "fee": "0.50", "closedPnl": "10.00", "sz": "0.422", "time": 1000},
	})
	defer srv.Close()
	origURL := hlMainnetURL
	hlMainnetURL = srv.URL
	defer func() { hlMainnetURL = origURL }()

	// 0.100 is the sz of the newest fill that matches; anchor OID is 77777.
	got, ok := lookupHyperliquidFillByCoinSize("0xtest", "ETH", 0.100, 1e-4, 0)
	if !ok {
		t.Fatal("expected ok=true")
	}
	wantQty := 0.211
	if got.FilledQty < wantQty-1e-9 || got.FilledQty > wantQty+1e-9 {
		t.Errorf("FilledQty = %g, want %g (sum across OID group)", got.FilledQty, wantQty)
	}
}
