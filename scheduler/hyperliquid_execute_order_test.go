package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
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
		wantBookedQty float64
	}{
		{name: "sole owner partial above chain is capped reduce-only on the refetched reading", direction: DirectionLong, signal: -1, closeFraction: 0.5, posQty: 10, ctx: hlCloseContext{OnChain: hlSignedView("ETH", 7)}, refetch: func() *hlOnChainCoinView { v := hlSignedView("ETH", 7); return &v }(), wantCalls: 1, wantRefetches: 1, wantMode: hlCloseModeReduceOnly, wantSize: 2},
		{name: "sole owner full close keeps the whole-position close", direction: DirectionLong, signal: -1, closeFraction: 1.0, posQty: 10, ctx: hlCloseContext{OnChain: hlSignedView("ETH", 7)}, wantCalls: 1, wantMode: hlCloseModeWhole, wantSize: 10},
		{name: "same-side peer full close is reduce-only capped on the refetched reading and hands the short fill to the re-arm", direction: DirectionLong, signal: -1, closeFraction: 1.0, posQty: 10, shared: true, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 12)}, refetch: func() *hlOnChainCoinView { v := hlSignedView("ETH", 12); return &v }(), wantCalls: 1, wantRefetches: 1, wantMode: hlCloseModeReduceOnly, wantSize: 7, wantBookedQty: 7},
		{name: "opposite-side peer full close crosses after one refetch", direction: DirectionLong, signal: -1, closeFraction: 1.0, posQty: 10, shared: true, ctx: hlCloseContext{PeerOppQty: 4, OnChain: hlSignedView("ETH", 6)}, refetch: func() *hlOnChainCoinView { v := hlSignedView("ETH", 6); return &v }(), wantCalls: 1, wantRefetches: 1, wantMode: hlCloseModeCross, wantSize: 10},
		{name: "opposite-side peer with refetch error sends nothing", direction: DirectionLong, signal: -1, closeFraction: 1.0, posQty: 10, shared: true, ctx: hlCloseContext{PeerOppQty: 4, OnChain: hlSignedView("ETH", 6)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), wantRefetches: 1},
		{name: "sole owner with flat chain on the refetched reading sends nothing and alerts", direction: DirectionLong, signal: -1, closeFraction: 0.5, posQty: 10, ctx: hlCloseContext{OnChain: hlSignedView("ETH", 0)}, refetch: func() *hlOnChainCoinView { v := hlSignedView("ETH", 0); return &v }(), wantRefetches: 1, wantAlert: true},
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
			if math.Abs(result.SizedCloseBookedQty-tc.wantBookedQty) > 1e-9 {
				t.Fatalf("short-fill booked qty=%g, want %g", result.SizedCloseBookedQty, tc.wantBookedQty)
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

func TestHyperliquidSizedCloseFreshReadingAndBooking(t *testing.T) {
	originalExecute := runHyperliquidExecuteFn
	originalThrottle := liveExecThrottle
	originalRecorder := tradeRecorder
	originalUpdate := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() {
		runHyperliquidExecuteFn = originalExecute
		liveExecThrottle = originalThrottle
		tradeRecorder = originalRecorder
		runHyperliquidUpdateStopLossFunc = originalUpdate
	})
	tradeRecorder = nil
	view := func(signed float64) *hlOnChainCoinView { v := hlSignedView("ETH", signed); return &v }
	peer := StrategyConfig{ID: "hl-peer", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "ETH", "1h", "--mode=live"}}
	cases := []struct {
		name         string
		posQty       float64
		ctx          hlCloseContext
		refetch      *hlOnChainCoinView
		refetchErr   error
		fillCap      float64
		failedSLOID  bool
		wantCalls    int
		wantMode     hlCloseMode
		wantSize     float64
		wantBookQty  float64
		wantSLOID    int64
		wantTPOIDs   []int64
		wantAlert    bool
		wantRefetch  int
		wantBookFlat bool
		wantStopQty  float64
		wantStopOID  int64
		wantCritical bool
		wantDetail   string
	}{
		{name: "same-side peer opened earlier in the cycle closes the whole book", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 10)}, refetch: view(15), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookFlat: true, wantRefetch: 1},
		{name: "opposite-side peer closed earlier in the cycle sends instead of skipping", posQty: 4, ctx: hlCloseContext{OnChain: hlSignedView("ETH", -6)}, refetch: view(4), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 4, wantBookFlat: true, wantRefetch: 1},
		{name: "opposite-side peer closed earlier in the cycle sends the whole book instead of a cap", posQty: 10, ctx: hlCloseContext{OnChain: hlSignedView("ETH", 6)}, refetch: view(10), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookFlat: true, wantRefetch: 1},
		{name: "refetch failure on a capped plan defers and cancels nothing", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 12)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), wantBookQty: 10, wantSLOID: 111, wantTPOIDs: []int64{201, 202}, wantRefetch: 1},
		{name: "refetch failure on a skip plan defers without a critical alert", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 5)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), wantBookQty: 10, wantSLOID: 111, wantTPOIDs: []int64{201, 202}, wantRefetch: 1},
		{name: "refetch failure on an uncapped reduce-only plan still sends the book size", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookFlat: true, wantRefetch: 1},
		{name: "real drift on the fresh reading books the fill and keeps the remainder", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: view(12), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 7, wantBookQty: 3, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 1, wantCritical: true},
		{name: "partial IOC fill books the fill and keeps the remainder", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: view(15), fillCap: 4, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 6, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 1, wantStopQty: 6, wantStopOID: 999},
		{name: "a stop cancel the venue reported as failed keeps its id on the remainder", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: view(15), fillCap: 4, failedSLOID: true, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 6, wantSLOID: 111, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 1, wantStopQty: 6, wantStopOID: 999},
		{name: "a failed pre-send and post-fill read on a partial IOC fill arms the remainder and says so", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), fillCap: 4, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 6, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 2, wantStopQty: 6, wantStopOID: 999, wantDetail: "clearinghouseState timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var gotMode hlCloseMode
			var gotSize float64
			runHyperliquidExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				calls++
				gotMode, gotSize = closeMode, size
				filled := size
				if tc.fillCap > 0 && tc.fillCap < filled {
					filled = tc.fillCap
				}
				res := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: filled}}}
				requested := append([]int64{cancelOID}, extraCancelOIDs...)
				if tc.failedSLOID {
					res.CancelStopLossError = "cancel rejected"
					res.CancelStopLossFailedOIDs = []int64{cancelOID}
					res.CancelStopLossSucceededOIDs = extraCancelOIDs
				} else {
					res.CancelStopLossSucceeded = true
					res.CancelStopLossSucceededOIDs = requested
				}
				return res, "", nil
			}
			var stopCalls []rearmSLCall
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				stopCalls = append(stopCalls, rearmSLCall{symbol: symbol, side: side, size: size, triggerPx: triggerPx, cancelOID: cancelOID})
				return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: cancelOID > 0}, "", nil
			}
			liveExecThrottle = &LiveExecFailureThrottle{}
			notifier, backend := confirmationNotifier()
			sc := confirmationTestStrategy(DirectionLong)
			stopPct := 5.0
			sc.StopLossPct = &stopPct
			refetches := 0
			ctx := tc.ctx
			ctx.Refetch = func() (hlOnChainCoinView, error) {
				refetches++
				if tc.refetchErr != nil {
					return hlOnChainCoinView{}, tc.refetchErr
				}
				return *tc.refetch, nil
			}
			state := &StrategyState{ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: tc.posQty, InitialQuantity: tc.posQty, AvgCost: 2000, Side: "long", Multiplier: 1, OwnerStrategyID: sc.ID, StopLossOID: 111, StopLossTriggerPx: 1900, TPOIDs: []int64{201, 202}, TPArmedTiers: []bool{true, true}},
			}}
			result := &HyperliquidResult{Symbol: "ETH", Signal: -1, Price: 2100}
			result.CloseFraction = 1.0
			execResult, ok := runHyperliquidExecuteOrder(sc, result, 2100, 1000, false, tc.posQty, "long", 2000, 2, 111, []int64{201, 202}, []StrategyConfig{sc, peer}, hlExecuteSnapshot{}, ctx, HurstGateDecision{}, notifier, silentStrategyLogger(sc.ID))
			if calls != tc.wantCalls || ok != (tc.wantCalls > 0) || result.LiveOrderSubmitted != (tc.wantCalls > 0) {
				t.Fatalf("calls=%d ok=%t submitted=%t, want calls=%d", calls, ok, result.LiveOrderSubmitted, tc.wantCalls)
			}
			if ok {
				if gotMode != tc.wantMode || math.Abs(gotSize-tc.wantSize) > 1e-9 {
					t.Fatalf("sent mode=%v size=%g, want mode=%v size=%g", gotMode, gotSize, tc.wantMode, tc.wantSize)
				}
				executeHyperliquidResultDeferredOpen(sc, state, result, execResult, "SELL", 2100, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(sc.ID))
				if result.SizedCloseBookedQty > 0 {
					var mu sync.RWMutex
					rearm := hlCloseRearmContext{Price: 2100, PrevStopOID: 111, PrevTriggerPx: 1900, OnChainAbsQty: ctx.OnChain.AbsQty, Backing: hlCloseBacking{PeerSameQty: ctx.PeerSameQty, PeerOppQty: ctx.PeerOppQty, Refetch: ctx.Refetch}}
					rearmExecuteLaneSizedCloseShortFill(sc, state, nil, result, tc.posQty, "long", rearm, &mu, notifier, silentStrategyLogger(sc.ID))
				}
			}
			if tc.wantStopQty == 0 && len(stopCalls) != 0 {
				t.Fatalf("stop re-arms = %+v, want none", stopCalls)
			}
			if tc.wantStopQty > 0 && (len(stopCalls) != 1 || math.Abs(stopCalls[0].size-tc.wantStopQty) > 1e-9) {
				t.Fatalf("stop re-arms = %+v, want one of %g", stopCalls, tc.wantStopQty)
			}
			if refetches != tc.wantRefetch {
				t.Fatalf("refetches=%d, want %d", refetches, tc.wantRefetch)
			}
			pos := state.Positions["ETH"]
			if tc.wantBookFlat {
				if pos != nil {
					t.Fatalf("book = %+v, want the whole book closed", pos)
				}
			} else {
				if pos == nil {
					t.Fatalf("book deleted, want %g left", tc.wantBookQty)
				}
				wantSL := tc.wantSLOID
				if tc.wantStopOID > 0 {
					wantSL = tc.wantStopOID
				}
				if math.Abs(pos.Quantity-tc.wantBookQty) > 1e-9 || pos.StopLossOID != wantSL || fmt.Sprint(pos.TPOIDs) != fmt.Sprint(tc.wantTPOIDs) {
					t.Fatalf("book qty=%g sl=%d tps=%v, want qty=%g sl=%d tps=%v", pos.Quantity, pos.StopLossOID, pos.TPOIDs, tc.wantBookQty, wantSL, tc.wantTPOIDs)
				}
			}
			backend.mu.Lock()
			var alerts []string
			for _, m := range backend.messages {
				alerts = append(alerts, m.content)
			}
			alerted := len(backend.messages) > 0 || len(backend.dms) > 0
			backend.mu.Unlock()
			if alerted != tc.wantAlert || len(alerts) > 1 || (tc.wantAlert && len(alerts) != 1) {
				t.Fatalf("alerted=%t channel alerts=%q, want one: %t", alerted, alerts, tc.wantAlert)
			}
			if tc.wantAlert && (strings.HasPrefix(alerts[0], "CRITICAL") != tc.wantCritical || !strings.Contains(alerts[0], tc.wantDetail)) {
				t.Fatalf("alert = %q, want critical %t and the detail %q", alerts[0], tc.wantCritical, tc.wantDetail)
			}
		})
	}
}
