package main

import (
	"fmt"
	"testing"
)

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
	}{
		{name: "already long skip sends nothing", signal: 1, wantSubmitted: false},
		{name: "rejected close was sent", signal: -1, closeFraction: 1.0, wantSubmitted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := 0
			runHyperliquidExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				called++
				return nil, "", fmt.Errorf("exchange rejected order: insufficient margin")
			}
			liveExecThrottle = &LiveExecFailureThrottle{}
			notifier, _ := confirmationNotifier()
			sc := confirmationTestStrategy(DirectionLong)
			result := &HyperliquidResult{Symbol: "ETH", Signal: tc.signal, Price: 2000}
			result.CloseFraction = tc.closeFraction
			got, ok := runHyperliquidExecuteOrder(sc, result, 2000, 1000, false, 0.0741, "long", 1900, 2, 111, nil, nil, hlExecuteSnapshot{}, HurstGateDecision{}, notifier, silentStrategyLogger(sc.ID))
			if ok || got != nil {
				t.Fatalf("execute result = %+v, ok=%t, want (nil, false)", got, ok)
			}
			if (called > 0) != tc.wantSubmitted || result.LiveOrderSubmitted != tc.wantSubmitted {
				t.Fatalf("execute calls = %d, submitted = %t, want submitted %t", called, result.LiveOrderSubmitted, tc.wantSubmitted)
			}
		})
	}
}
