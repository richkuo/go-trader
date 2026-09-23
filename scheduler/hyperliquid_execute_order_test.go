package main

import (
	"fmt"
	"math"
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
			runHyperliquidExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				called++
				return unconfirmedExecuteResult(), "", nil
			}
			liveExecThrottle = &LiveExecFailureThrottle{}
			notifier, backend := confirmationNotifier()
			sc := confirmationTestStrategy(tc.dir)
			result := &HyperliquidResult{Symbol: "ETH", Signal: 0, Price: 2000}
			result.CloseFraction = 0
			result.CloseGate = "below_venue_minimum"
			got, ok := runHyperliquidExecuteOrder(sc, result, 2000, 1000, false, 0.0741, "long", 1900, 2, 111, nil, nil, hlExecuteSnapshot{}, hlCloseContext{}, HurstGateDecision{}, notifier, silentStrategyLogger(sc.ID))
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

func TestHyperliquidExecuteOrderCloseModeRouting(t *testing.T) {
	originalExecute := runHyperliquidExecuteFn
	originalThrottle := liveExecThrottle
	t.Cleanup(func() {
		runHyperliquidExecuteFn = originalExecute
		liveExecThrottle = originalThrottle
	})
	peer := StrategyConfig{ID: "hl-peer", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "ETH", "1h", "--mode=live"}}
	cases := []struct {
		name          string
		direction     string
		signal        int
		closeFraction float64
		posQty        float64
		shared        bool
		ctx           hlCloseContext
		refetch       *hlOnChainCoinView
		refetchErr    error
		wantCalls     int
		wantRefetches int
		wantMode      hlCloseMode
		wantSize      float64
		wantFlipSize  bool
		wantAlert     bool
	}{
		{name: "sole owner partial above chain is capped reduce-only", direction: DirectionLong, signal: -1, closeFraction: 0.5, posQty: 10, ctx: hlCloseContext{OnChain: hlSignedView("ETH", 7)}, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 2},
		{name: "sole owner full close keeps the whole-position close", direction: DirectionLong, signal: -1, closeFraction: 1.0, posQty: 10, ctx: hlCloseContext{OnChain: hlSignedView("ETH", 7)}, wantCalls: 1, wantMode: hlCloseModeWhole, wantSize: 10},
		{name: "same-side peer full close is reduce-only capped", direction: DirectionLong, signal: -1, closeFraction: 1.0, posQty: 10, shared: true, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 12)}, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 7},
		{name: "opposite-side peer full close crosses after one refetch", direction: DirectionLong, signal: -1, closeFraction: 1.0, posQty: 10, shared: true, ctx: hlCloseContext{PeerOppQty: 4, OnChain: hlSignedView("ETH", 6)}, refetch: func() *hlOnChainCoinView { v := hlSignedView("ETH", 6); return &v }(), wantCalls: 1, wantRefetches: 1, wantMode: hlCloseModeCross, wantSize: 10},
		{name: "opposite-side peer with refetch error sends nothing", direction: DirectionLong, signal: -1, closeFraction: 1.0, posQty: 10, shared: true, ctx: hlCloseContext{PeerOppQty: 4, OnChain: hlSignedView("ETH", 6)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), wantRefetches: 1},
		{name: "sole owner with flat chain sends nothing and alerts", direction: DirectionLong, signal: -1, closeFraction: 0.5, posQty: 10, ctx: hlCloseContext{OnChain: hlSignedView("ETH", 0)}, wantAlert: true},
		{name: "direction both flip still crosses unplanned", direction: DirectionBoth, signal: -1, posQty: 0.2, ctx: hlCloseContext{OnChain: hlSignedView("ETH", 0)}, wantCalls: 1, wantMode: hlCloseModeNone, wantFlipSize: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var gotMode hlCloseMode
			var gotSize float64
			runHyperliquidExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				calls++
				gotMode, gotSize = closeMode, size
				return &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: size}}}, "", nil
			}
			liveExecThrottle = &LiveExecFailureThrottle{}
			notifier, backend := confirmationNotifier()
			sc := confirmationTestStrategy(tc.direction)
			var hlLiveAll []StrategyConfig
			if tc.shared {
				hlLiveAll = []StrategyConfig{sc, peer}
			}
			refetches := 0
			ctx := tc.ctx
			ctx.Refetch = func() (hlOnChainCoinView, error) {
				refetches++
				if tc.refetchErr != nil {
					return hlOnChainCoinView{}, tc.refetchErr
				}
				if tc.refetch == nil {
					return hlOnChainCoinView{}, fmt.Errorf("unexpected refetch")
				}
				return *tc.refetch, nil
			}
			result := &HyperliquidResult{Symbol: "ETH", Signal: tc.signal, Price: 2000}
			result.CloseFraction = tc.closeFraction
			_, ok := runHyperliquidExecuteOrder(sc, result, 2000, 1000, false, tc.posQty, "long", 2000, 2, 0, nil, hlLiveAll, hlExecuteSnapshot{}, ctx, HurstGateDecision{}, notifier, silentStrategyLogger(sc.ID))
			if calls != tc.wantCalls || refetches != tc.wantRefetches || ok != (tc.wantCalls > 0) || result.LiveOrderSubmitted != (tc.wantCalls > 0) {
				t.Fatalf("calls=%d refetches=%d ok=%t submitted=%t, want calls=%d refetches=%d", calls, refetches, ok, result.LiveOrderSubmitted, tc.wantCalls, tc.wantRefetches)
			}
			backend.mu.Lock()
			alerted := len(backend.messages) > 0 || len(backend.dms) > 0
			backend.mu.Unlock()
			if alerted != tc.wantAlert {
				t.Fatalf("alerted=%t, want %t", alerted, tc.wantAlert)
			}
			if tc.wantCalls == 0 {
				return
			}
			if gotMode != tc.wantMode {
				t.Fatalf("mode=%v, want %v", gotMode, tc.wantMode)
			}
			if tc.wantFlipSize {
				if gotSize <= tc.posQty {
					t.Fatalf("flip size %g must exceed the close leg %g", gotSize, tc.posQty)
				}
				return
			}
			if gotMode != hlCloseModeWhole && math.Abs(gotSize-tc.wantSize) > 1e-9 {
				t.Fatalf("size=%g, want %g", gotSize, tc.wantSize)
			}
		})
	}
}
