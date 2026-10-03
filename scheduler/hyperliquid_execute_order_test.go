package main

import (
	"fmt"
	"testing"
)

func TestHyperliquidExecuteOrderMarksSubmissionOnlyAfterSend(t *testing.T) {
	originalExecute := runHyperliquidExecuteFn
	originalThrottle := liveExecThrottle
	t.Cleanup(func() {
		runHyperliquidExecuteFn = originalExecute
		liveExecThrottle = originalThrottle
	})

	cases := []struct {
		name          string
		signal        int
		closeFraction float64
		wantSubmitted bool
		wantCancel    bool
	}{
		{name: "already long skip sends nothing", signal: 1, wantSubmitted: false},
		{name: "rejected full close was sent and asked to cancel the stop", signal: -1, closeFraction: 1.0, wantSubmitted: true, wantCancel: true},
		{name: "rejected partial close was sent but asked for no cancel", signal: -1, closeFraction: 0.5, wantSubmitted: true, wantCancel: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := 0
			runHyperliquidExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				called++
				return nil, "", fmt.Errorf("exchange rejected order: insufficient margin")
			}
			liveExecThrottle = &LiveExecFailureThrottle{}
			notifier, _ := confirmationNotifier()
			sc := confirmationTestStrategy(DirectionLong)
			result := &HyperliquidResult{Symbol: "ETH", Signal: tc.signal, Price: 2000}
			result.CloseFraction = tc.closeFraction
			got, ok := runHyperliquidExecuteOrder(sc, result, 2000, 1000, false, 0.0741, "long", 1900, 2, 111, nil, nil, hlExecuteSnapshot{}, hlCloseContext{}, HurstGateDecision{}, notifier, silentStrategyLogger(sc.ID))
			if ok || got != nil {
				t.Fatalf("execute result = %+v, ok=%t, want (nil, false)", got, ok)
			}
			if (called > 0) != tc.wantSubmitted || result.LiveOrderSubmitted != tc.wantSubmitted || result.LiveOrderCancelRequested != tc.wantCancel {
				t.Fatalf("execute calls = %d, submitted = %t, cancel requested = %t, want submitted %t cancel %t", called, result.LiveOrderSubmitted, result.LiveOrderCancelRequested, tc.wantSubmitted, tc.wantCancel)
			}
		})
	}
}
