package main

import "testing"

func TestHyperliquidExecuteOrderSkipsGatedCloseWithoutAlert(t *testing.T) {
	originalExecute := runHyperliquidExecuteFn
	originalThrottle := liveExecThrottle
	t.Cleanup(func() {
		runHyperliquidExecuteFn = originalExecute
		liveExecThrottle = originalThrottle
	})

	cases := []struct {
		name string
		dir  string
	}{
		{name: "long", dir: DirectionLong},
		{name: "both", dir: DirectionBoth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := 0
			runHyperliquidExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				called++
				return unconfirmedExecuteResult(), "", nil
			}
			liveExecThrottle = &LiveExecFailureThrottle{}
			notifier, backend := confirmationNotifier()
			sc := confirmationTestStrategy(tc.dir)
			result := &HyperliquidResult{Symbol: "ETH", Signal: 0, Price: 2000}
			result.CloseFraction = 0
			result.CloseGate = "below_venue_minimum"
			got, ok := runHyperliquidExecuteOrder(sc, result, 2000, 1000, false, 0.0741, "long", 1900, 2, 111, nil, nil, hlExecuteSnapshot{}, HurstGateDecision{}, notifier, silentStrategyLogger(sc.ID))
			if ok || got != nil {
				t.Fatalf("execute result = %+v, ok=%t, want (nil, false)", got, ok)
			}
			if called != 0 {
				t.Fatalf("execute calls = %d, want 0", called)
			}
			liveExecThrottle.mu.Lock()
			entries := len(liveExecThrottle.entries)
			liveExecThrottle.mu.Unlock()
			if entries != 0 {
				t.Fatalf("throttle entries = %d, want none", entries)
			}
			backend.mu.Lock()
			messages := len(backend.messages)
			dms := len(backend.dms)
			backend.mu.Unlock()
			if messages != 0 || dms != 0 {
				t.Fatalf("notifications = channels %d DMs %d, want none", messages, dms)
			}
		})
	}
}
