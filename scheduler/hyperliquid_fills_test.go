package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

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

func TestLookupHyperliquidReconcileFillFee_OIDFirstFallsBackToCoinSize(t *testing.T) {
	withFastFillRetries(t)
	calls := 0
	orig := fetchHyperliquidUserFillsByTime
	defer func() { fetchHyperliquidUserFillsByTime = orig }()
	fetchHyperliquidUserFillsByTime = func(addr string, sinceMs int64) ([]hlFillRecord, error) {
		calls++
		switch calls {
		case 1:
			return []hlFillRecord{
				{Coin: "BTC", OID: "1", Fee: "0.10", Sz: "0.5"},
			}, nil
		default:
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
	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(addr, coin string, oid int64, qty float64) (HLFillLookup, bool) {
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

	changed := reconcileHyperliquidPositionsWithResolver(s, "BTC", nil, func(coin string, oid int64, expectedQty float64) (HLFillLookup, bool) {
		return lookupHyperliquidReconcileFillFee("0xtest", coin, oid, expectedQty)
	}, logger, nil, nil, StrategyConfig{})
	if !changed {
		t.Fatal("expected changed=true")
	}

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
	origLookup := lookupHyperliquidReconcileFillFee
	defer func() { lookupHyperliquidReconcileFillFee = origLookup }()
	lookupHyperliquidReconcileFillFee = func(addr, coin string, oid int64, qty float64) (HLFillLookup, bool) {
		if oid == 5005 {
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

func withHLFillsServer(t *testing.T, fills []map[string]any) {
	t.Helper()
	withFastFillRetries(t)
	srv := newHLUserFillsServer(t, fills)
	origURL := hlMainnetURL
	hlMainnetURL = srv.URL
	t.Cleanup(func() {
		hlMainnetURL = origURL
		srv.Close()
	})
}

func assertFillField(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %g, want ~%g", name, got, want)
	}
}

func TestLookupHyperliquidFillByOID(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fills     []map[string]any
		addr      string
		oid       int64
		wantOK    bool
		wantCount int
		wantFee   float64
		wantPnL   float64
		wantQty   float64
	}{
		{
			name: "aggregates partial fills sharing the OID",
			fills: []map[string]any{
				{"coin": "BTC", "oid": 12345, "fee": "0.50", "closedPnl": "100.00", "sz": "0.1"},
				{"coin": "BTC", "oid": 12345, "fee": "0.30", "closedPnl": "50.00", "sz": "0.05"},
				{"coin": "BTC", "oid": 99999, "fee": "1.00", "closedPnl": "200.00", "sz": "0.2"},
			},
			addr: "0xtest", oid: 12345, wantOK: true, wantCount: 2, wantFee: 0.80, wantPnL: 150.00, wantQty: 0.15,
		},
		{
			name: "accumulates filled qty across partial fills",
			fills: []map[string]any{
				{"coin": "ETH", "oid": 55555, "fee": "0.10", "closedPnl": "5.00", "sz": "0.211"},
				{"coin": "ETH", "oid": 55555, "fee": "0.05", "closedPnl": "2.50", "sz": "0.100"},
				{"coin": "ETH", "oid": 99999, "fee": "1.00", "closedPnl": "50.00", "sz": "0.422"},
			},
			addr: "0xtest", oid: 55555, wantOK: true, wantCount: 2, wantFee: 0.15, wantPnL: 7.50, wantQty: 0.311,
		},
		{
			name: "no match returns false",
			fills: []map[string]any{
				{"coin": "BTC", "oid": 99999, "fee": "1.00", "sz": "0.2"},
			},
			addr: "0xtest", oid: 12345, wantOK: false,
		},
		{
			name: "empty address short-circuits",
			fills: []map[string]any{
				{"coin": "BTC", "oid": 12345, "fee": "1.00", "sz": "0.2"},
			},
			addr: "", oid: 12345, wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withHLFillsServer(t, tc.fills)
			got, ok := lookupHyperliquidFillByOID(tc.addr, tc.oid, 0)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.Count != tc.wantCount {
				t.Errorf("Count = %d, want %d", got.Count, tc.wantCount)
			}
			assertFillField(t, "Fee", got.Fee, tc.wantFee, 1e-3)
			assertFillField(t, "ClosedPnLGross", got.ClosedPnLGross, tc.wantPnL, 1e-2)
			assertFillField(t, "FilledQty", got.FilledQty, tc.wantQty, 1e-9)
		})
	}
}

func TestLookupHyperliquidFillByCoinSize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fills     []map[string]any
		coin      string
		size      float64
		wantOID   int64
		wantCount int
		wantFee   float64
		wantPnL   float64
		wantQty   float64
	}{
		{
			name: "matches by coin and size",
			fills: []map[string]any{
				{"coin": "BTC", "oid": 1, "fee": "0.40", "closedPnl": "75.00", "sz": "0.123456"},
				{"coin": "ETH", "oid": 2, "fee": "0.10", "closedPnl": "5.00", "sz": "0.123456"},
				{"coin": "BTC", "oid": 3, "fee": "0.20", "closedPnl": "10.00", "sz": "0.5"},
			},
			coin: "BTC", size: 0.123456, wantOID: 1, wantCount: 1, wantFee: 0.40, wantPnL: 75.00, wantQty: 0.123456,
		},
		{
			name: "picks the newest group and never the sum of the window",
			fills: []map[string]any{
				{"coin": "BTC", "oid": 100, "fee": "0.40", "closedPnl": "75.00", "sz": "0.1", "time": 1_000_000_000},
				{"coin": "BTC", "oid": 200, "fee": "0.50", "closedPnl": "90.00", "sz": "0.1", "time": 2_000_000_000},
			},
			coin: "BTC", size: 0.1, wantOID: 200, wantCount: 1, wantFee: 0.50, wantPnL: 90.00, wantQty: 0.1,
		},
		{
			name: "aggregates partial fills by the newest anchor OID",
			fills: []map[string]any{
				{"coin": "BTC", "oid": 555, "fee": "0.20", "closedPnl": "30.00", "sz": "0.04", "time": 1_500_000_000},
				{"coin": "BTC", "oid": 555, "fee": "0.30", "closedPnl": "40.00", "sz": "0.10", "time": 2_000_000_000},
				{"coin": "BTC", "oid": 999, "fee": "0.99", "closedPnl": "99.00", "sz": "0.10", "time": 1_000_000_000},
			},
			coin: "BTC", size: 0.10, wantOID: 555, wantCount: 2, wantFee: 0.50, wantPnL: 70.00, wantQty: 0.14,
		},
		{
			name: "sets filled qty when the fill carries no OID",
			fills: []map[string]any{
				{"coin": "ETH", "oid": nil, "fee": "0.08", "closedPnl": "4.00", "sz": "0.211", "time": 1000},
			},
			coin: "ETH", size: 0.211, wantOID: 0, wantCount: 1, wantFee: 0.08, wantPnL: 4.00, wantQty: 0.211,
		},
		{
			name: "accumulates filled qty across the OID group",
			fills: []map[string]any{
				{"coin": "ETH", "oid": 77777, "fee": "0.05", "closedPnl": "2.00", "sz": "0.100", "time": 2000},
				{"coin": "ETH", "oid": 77777, "fee": "0.06", "closedPnl": "3.00", "sz": "0.111", "time": 1900},
				{"coin": "ETH", "oid": 88888, "fee": "0.50", "closedPnl": "10.00", "sz": "0.422", "time": 1000},
			},
			coin: "ETH", size: 0.100, wantOID: 77777, wantCount: 2, wantFee: 0.11, wantPnL: 5.00, wantQty: 0.211,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withHLFillsServer(t, tc.fills)
			got, ok := lookupHyperliquidFillByCoinSize("0xtest", tc.coin, tc.size, 1e-4, 0)
			if !ok {
				t.Fatal("expected ok=true")
			}
			if got.OID != tc.wantOID {
				t.Errorf("OID = %d, want %d", got.OID, tc.wantOID)
			}
			if got.Count != tc.wantCount {
				t.Errorf("Count = %d, want %d", got.Count, tc.wantCount)
			}
			assertFillField(t, "Fee", got.Fee, tc.wantFee, 1e-3)
			assertFillField(t, "ClosedPnLGross", got.ClosedPnLGross, tc.wantPnL, 1e-2)
			assertFillField(t, "FilledQty", got.FilledQty, tc.wantQty, 1e-9)
		})
	}
}
