package main

import "testing"

func TestHLReleaseUnreadableStop(t *testing.T) {
	old := runHyperliquidListOpenOrderOIDsFunc
	defer func() { runHyperliquidListOpenOrderOIDsFunc = old }()

	const symbol = "ETH"
	const oldOID int64 = 11
	stop := hlListedOpenOrder{OID: 22, Side: "A", Sz: 1, ReduceOnly: true, IsTrigger: true, OrderType: "Stop Market", TriggerPx: 100}
	takeProfit := hlListedOpenOrder{OID: 33, Side: "A", Sz: 1, ReduceOnly: true, IsTrigger: true, OrderType: "Take Profit Market", TriggerPx: 120}
	otherSide := hlListedOpenOrder{OID: 44, Side: "B", Sz: 1, ReduceOnly: true, IsTrigger: true, OrderType: "Stop Market", TriggerPx: 100}

	cases := []struct {
		name      string
		orders    []hlListedOpenOrder
		readErr   string
		wantHeld  bool
		wantOID   int64
		wantPx    float64
		wantNamed []int64
	}{
		{name: "unreadable book keeps the hold", readErr: "indexer down", wantHeld: true},
		{name: "one matching stop is adopted", orders: []hlListedOpenOrder{stop}, wantOID: 22, wantPx: 100},
		{name: "take-profit and other-side stop are named", orders: []hlListedOpenOrder{takeProfit, otherSide}, wantNamed: []int64{33, 44}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hlUnreadableStopPlace.Delete(hlStopPlaceUnreadKey(symbol, oldOID))
			hlRememberUnreadableStop(symbol, oldOID, nil, 100, 1)
			runHyperliquidListOpenOrderOIDsFunc = func(string, string) ([]hlListedOpenOrder, string, error) {
				return tc.orders, tc.readErr, nil
			}
			released, adopted, alert := hlReleaseUnreadableStop("", symbol, "long", oldOID, 1, 100)
			if tc.wantHeld {
				if released || !hlStopPlaceUnread(symbol, oldOID) {
					t.Fatalf("released=%v hold=%v, want held", released, hlStopPlaceUnread(symbol, oldOID))
				}
				return
			}
			if !released || hlStopPlaceUnread(symbol, oldOID) {
				t.Fatalf("released=%v hold=%v, want cleared", released, hlStopPlaceUnread(symbol, oldOID))
			}
			if tc.wantOID > 0 {
				if adopted == nil || adopted.StopLossOID != tc.wantOID || adopted.StopLossTriggerPx != tc.wantPx {
					t.Fatalf("adopted=%+v, want oid %d trigger %v", adopted, tc.wantOID, tc.wantPx)
				}
			} else if adopted != nil {
				t.Fatalf("adopted oid %d, want none", adopted.StopLossOID)
			}
			for _, id := range tc.wantNamed {
				if !containsInt64Text(alert, id) {
					t.Fatalf("alert %q does not name oid %d", alert, id)
				}
			}
		})
	}
}

func containsInt64Text(s string, id int64) bool {
	return len(s) > 0 && (func() bool {
		n := id
		if n == 0 {
			return false
		}
		buf := make([]byte, 0, 8)
		for n > 0 {
			buf = append([]byte{byte('0' + n%10)}, buf...)
			n /= 10
		}
		return len(buf) > 0 && (func() bool {
			for i := 0; i+len(buf) <= len(s); i++ {
				if s[i:i+len(buf)] == string(buf) {
					return true
				}
			}
			return false
		})()
	})()
}
