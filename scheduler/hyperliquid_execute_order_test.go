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
	originalSync := syncHyperliquidProtection
	t.Cleanup(func() {
		runHyperliquidExecuteFn = originalExecute
		liveExecThrottle = originalThrottle
		tradeRecorder = originalRecorder
		runHyperliquidUpdateStopLossFunc = originalUpdate
		syncHyperliquidProtection = originalSync
	})
	tradeRecorder = nil
	reads := func(signed ...float64) []hlOnChainCoinView {
		var out []hlOnChainCoinView
		for _, v := range signed {
			out = append(out, hlSignedView("ETH", v))
		}
		return out
	}
	peer := StrategyConfig{ID: "hl-peer", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "ETH", "1h", "--mode=live"}}
	removed := &HyperliquidStopLossUpdateResult{CancelOnly: true, CancelStopLossSucceeded: true}
	removalRejected := &HyperliquidStopLossUpdateResult{CancelOnly: true, Error: "cancel of stop-loss OID=111 failed: busy", CancelStopLossError: "busy"}
	cases := []struct {
		name              string
		posQty            float64
		ctx               hlCloseContext
		refetch           []hlOnChainCoinView
		refetchErr        error
		fillCap           float64
		failedSLOID       bool
		failedTPOIDs      []int64
		noCancelMeta      bool
		tiered            bool
		slResult          *HyperliquidStopLossUpdateResult
		syncResult        *HyperliquidProtectionSyncResult
		wantRemovalCancel int64
		wantSyncCancel    []int64
		wantForceTP       []bool
		failure           *HyperliquidExecuteResult
		wantCalls         int
		wantMode          hlCloseMode
		wantSize          float64
		wantBookQty       float64
		wantSLOID         int64
		wantTPOIDs        []int64
		wantAlert         bool
		wantCloseFail     bool
		wantRefetch       int
		wantBookFlat      bool
		wantStopQty       float64
		wantStopOID       int64
		wantCritical      bool
		wantDetail        string
		notDetail         string
	}{
		{name: "same-side peer opened earlier in the cycle closes the whole book", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 10)}, refetch: reads(15), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookFlat: true, wantRefetch: 1},
		{name: "opposite-side peer closed earlier in the cycle sends instead of skipping", posQty: 4, ctx: hlCloseContext{OnChain: hlSignedView("ETH", -6)}, refetch: reads(4), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 4, wantBookFlat: true, wantRefetch: 1},
		{name: "opposite-side peer closed earlier in the cycle sends the whole book instead of a cap", posQty: 10, ctx: hlCloseContext{OnChain: hlSignedView("ETH", 6)}, refetch: reads(10), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookFlat: true, wantRefetch: 1},
		{name: "refetch failure on a capped plan defers and cancels nothing", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 12)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), wantBookQty: 10, wantSLOID: 111, wantTPOIDs: []int64{201, 202}, wantRefetch: 1},
		{name: "refetch failure on a skip plan defers without a critical alert", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 5)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), wantBookQty: 10, wantSLOID: 111, wantTPOIDs: []int64{201, 202}, wantRefetch: 1},
		{name: "refetch failure on an uncapped reduce-only plan still sends the book size", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookFlat: true, wantRefetch: 1},
		{name: "real drift on the fresh reading books the fill and keeps the remainder", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: reads(12, 5), wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 7, wantBookQty: 3, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 2, wantCritical: true, wantDetail: "NO STOP"},
		{name: "partial IOC fill books the fill and keeps the remainder", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: reads(15, 11), fillCap: 4, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 6, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 2, wantStopQty: 6, wantStopOID: 999},
		{name: "a stop cancel the venue reported as failed keeps its id on the remainder", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: reads(15, 11), fillCap: 4, failedSLOID: true, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 6, wantSLOID: 111, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 2, wantStopQty: 6, wantStopOID: 999},
		{name: "a failed pre-send and post-fill read on a partial IOC fill derives the stop from the cycle-start reading and alerts", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetchErr: fmt.Errorf("clearinghouseState timeout"), fillCap: 4, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 6, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 2, wantStopQty: 6, wantStopOID: 999, wantCritical: true, wantDetail: "clearinghouseState timeout"},
		{name: "an explicit rejection after the cancel landed beside an opposite-side peer arms the own-side units", posQty: 10, ctx: hlCloseContext{PeerOppQty: 4, OnChain: hlSignedView("ETH", 6)}, refetch: reads(6, 6), failure: &HyperliquidExecuteResult{OrderOutcome: "rejected", Error: "order rejected"}, wantCalls: 1, wantMode: hlCloseModeCross, wantSize: 10, wantBookQty: 10, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantCloseFail: true, wantRefetch: 2, wantStopQty: 6, wantStopOID: 999, wantCritical: true, wantDetail: "other 4.000000 units"},
		{name: "a catch-all reply reads the account after the close and caps at the backed units", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: reads(15, 11), failure: &HyperliquidExecuteResult{OrderOutcome: "unknown", Error: "socket closed"}, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 10, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantCloseFail: true, wantRefetch: 2, wantStopQty: 6, wantStopOID: 999, wantCritical: true, wantDetail: "reconciler books any fill"},
		{name: "a capped shared fill after a rejected stop cancel removes the pre-close stop and alerts once", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 12)}, refetch: reads(12, 5), failedSLOID: true, slResult: removed, wantRemovalCancel: 111, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 7, wantBookQty: 3, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 2, wantCritical: true, wantDetail: "stop OID 111 removed"},
		{name: "a capped shared fill whose stop removal is rejected names the resting stop", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 12)}, refetch: reads(12, 5), failedSLOID: true, slResult: removalRejected, wantRemovalCancel: 111, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 7, wantBookQty: 3, wantSLOID: 111, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantRefetch: 2, wantCritical: true, wantDetail: "stop OID 111 STILL RESTING"},
		{name: "a lost-reply full fill with no cancel metadata removes every requested order", posQty: 3, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 8)}, refetch: reads(8, 5), noCancelMeta: true, failure: &HyperliquidExecuteResult{OrderOutcome: "unknown", Error: "socket closed"}, slResult: removed, wantRemovalCancel: 111, wantSyncCancel: []int64{201, 202}, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 3, wantBookQty: 3, wantTPOIDs: []int64{0, 0}, wantAlert: true, wantCloseFail: true, wantRefetch: 2, wantCritical: true, wantDetail: "take-profit OIDs [201 202] removed"},
		{name: "a partial IOC fill force-replaces a take-profit whose cancel failed", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: reads(15, 11), fillCap: 4, failedTPOIDs: []int64{201}, tiered: true, syncResult: &HyperliquidProtectionSyncResult{TPOIDs: []int64{9101, 0, 0}}, wantForceTP: []bool{true, false, false}, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 6, wantTPOIDs: []int64{9101, 0, 0}, wantAlert: true, wantRefetch: 2, wantStopQty: 6, wantStopOID: 999, wantDetail: "Take-profit leg: placed"},
		{name: "a partial IOC fill whose take-profit placement is unresolved reports it UNKNOWN in the one re-arm alert", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: reads(15, 11), fillCap: 4, failedTPOIDs: []int64{201}, tiered: true, syncResult: &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 0, 0}, TPErrors: []string{"read timeout", "", ""}, TPOutcomeUnknown: []bool{true, false, false}}, wantForceTP: []bool{true, false, false}, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 6, wantTPOIDs: []int64{0, 0, 0}, wantAlert: true, wantRefetch: 2, wantStopQty: 6, wantStopOID: 999, wantCritical: true, wantDetail: "Take-profit leg UNKNOWN (the placement of tier(s) [1]", notDetail: "by hand"},
		{name: "a partial IOC fill with one unresolved and one rejected take-profit names both in the one re-arm alert", posQty: 10, ctx: hlCloseContext{PeerSameQty: 5, OnChain: hlSignedView("ETH", 15)}, refetch: reads(15, 11), fillCap: 4, failedTPOIDs: []int64{201}, tiered: true, syncResult: &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 0, 0}, TPErrors: []string{"read timeout", "open order limit", ""}, TPOutcomeUnknown: []bool{true, false, false}}, wantForceTP: []bool{true, false, false}, wantCalls: 1, wantMode: hlCloseModeReduceOnly, wantSize: 10, wantBookQty: 6, wantTPOIDs: []int64{0, 0, 0}, wantAlert: true, wantRefetch: 2, wantStopQty: 6, wantStopOID: 999, wantCritical: true, wantDetail: "FAILED (tier 2: open order limit) and UNKNOWN (the placement of tier(s) [1]"},
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
				res := &HyperliquidExecuteResult{OrderOutcome: "filled", Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: filled}}}
				if tc.failure != nil {
					failure := *tc.failure
					res = &failure
				}
				requested := append([]int64{cancelOID}, extraCancelOIDs...)
				switch {
				case tc.noCancelMeta:
				case tc.failedSLOID:
					res.CancelStopLossError = "cancel rejected"
					res.CancelStopLossFailedOIDs = []int64{cancelOID}
					res.CancelStopLossSucceededOIDs = extraCancelOIDs
				case len(tc.failedTPOIDs) > 0:
					res.CancelStopLossError = "cancel rejected"
					res.CancelStopLossFailedOIDs = tc.failedTPOIDs
					for _, oid := range requested {
						if !containsInt64(tc.failedTPOIDs, oid) {
							res.CancelStopLossSucceededOIDs = append(res.CancelStopLossSucceededOIDs, oid)
						}
					}
				default:
					res.CancelStopLossSucceeded = true
					res.CancelStopLossSucceededOIDs = requested
				}
				return res, "", nil
			}
			var stopCalls []rearmSLCall
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				stopCalls = append(stopCalls, rearmSLCall{symbol: symbol, side: side, size: size, triggerPx: triggerPx, cancelOID: cancelOID})
				if tc.slResult != nil {
					return tc.slResult, "", nil
				}
				return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: cancelOID > 0}, "", nil
			}
			var syncPlans []hlProtectionPlan
			syncHyperliquidProtection = func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger, hints []byte) (*HyperliquidProtectionSyncResult, bool) {
				syncPlans = append(syncPlans, plan)
				if tc.syncResult != nil {
					return tc.syncResult, true
				}
				return &HyperliquidProtectionSyncResult{}, true
			}
			liveExecThrottle = &LiveExecFailureThrottle{}
			notifier, backend := confirmationNotifier()
			sc := confirmationTestStrategy(DirectionLong)
			stopPct := 5.0
			sc.StopLossPct = &stopPct
			if tc.tiered {
				sc.CloseStrategy = &StrategyRef{Name: "tiered_tp_atr_live"}
			}
			refetches := 0
			ctx := tc.ctx
			ctx.Refetch = func() (hlOnChainCoinView, error) {
				refetches++
				if tc.refetchErr != nil {
					return hlOnChainCoinView{}, tc.refetchErr
				}
				if refetches > len(tc.refetch) {
					return hlOnChainCoinView{}, fmt.Errorf("unexpected refetch")
				}
				return tc.refetch[refetches-1], nil
			}
			state := &StrategyState{ID: sc.ID, Platform: "hyperliquid", Type: "perps", Cash: 1000, Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Quantity: tc.posQty, InitialQuantity: tc.posQty, AvgCost: 2000, EntryATR: 50, Side: "long", Multiplier: 1, OwnerStrategyID: sc.ID, StopLossOID: 111, StopLossTriggerPx: 1900, TPOIDs: []int64{201, 202}, TPArmedTiers: []bool{true, true}},
			}}
			result := &HyperliquidResult{Symbol: "ETH", Signal: -1, Price: 2100}
			result.CloseFraction = 1.0
			execResult, ok := runHyperliquidExecuteOrder(sc, result, 2100, 1000, false, tc.posQty, "long", 2000, 2, 111, []int64{201, 202}, []StrategyConfig{sc, peer}, hlExecuteSnapshot{}, ctx, HurstGateDecision{}, notifier, silentStrategyLogger(sc.ID))
			if calls != tc.wantCalls || ok != (tc.wantCalls > 0 && tc.failure == nil) || result.LiveOrderSubmitted != (tc.wantCalls > 0) {
				t.Fatalf("calls=%d ok=%t submitted=%t, want calls=%d", calls, ok, result.LiveOrderSubmitted, tc.wantCalls)
			}
			if calls > 0 && (gotMode != tc.wantMode || math.Abs(gotSize-tc.wantSize) > 1e-9) {
				t.Fatalf("sent mode=%v size=%g, want mode=%v size=%g", gotMode, gotSize, tc.wantMode, tc.wantSize)
			}
			var mu sync.RWMutex
			rearm := hlCloseRearmContext{Price: 2100, PrevStopOID: 111, PrevTPOIDs: []int64{201, 202}, PrevTriggerPx: 1900, Backing: hlCloseBacking{PeerSameQty: ctx.PeerSameQty, PeerOppQty: ctx.PeerOppQty, PreSend: result.SizedClosePreSend, Refetch: ctx.Refetch}}
			if ok {
				executeHyperliquidResultDeferredOpen(sc, state, result, execResult, "SELL", 2100, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(sc.ID))
				if result.SizedCloseBookedQty > 0 {
					rearmAfterSizedClose(sc, state, nil, result.Symbol, "long", tc.posQty, hlCloseFillOutcome{Filled: result.SizedCloseBookedQty, Known: true}, rearm, &mu, notifier, silentStrategyLogger(sc.ID))
				}
			} else if result.LiveOrderSubmitted {
				canceled := hyperliquidExecuteSucceededCancelOIDs(execResult, []int64{111, 201, 202})
				clearHyperliquidProtectionOIDsMatching(state.Positions["ETH"], canceled)
				unfilled := hlExecuteFillOutcome(execResult, nil, tc.posQty)
				if hlUnfilledCloseNeedsRearm(result.LiveOrderCancelRequested, unfilled, canceled) {
					rearmAfterSizedClose(sc, state, nil, result.Symbol, "long", tc.posQty, unfilled, rearm, &mu, notifier, silentStrategyLogger(sc.ID))
				}
			}
			switch {
			case tc.wantRemovalCancel > 0:
				if len(stopCalls) != 1 || stopCalls[0].size != 0 || stopCalls[0].triggerPx != 0 || stopCalls[0].cancelOID != tc.wantRemovalCancel {
					t.Fatalf("stop calls = %+v, want one cancel-only call for OID %d and no placement", stopCalls, tc.wantRemovalCancel)
				}
			case tc.wantStopQty == 0 && len(stopCalls) != 0:
				t.Fatalf("stop re-arms = %+v, want none", stopCalls)
			}
			var syncCancel []int64
			var forceTP []bool
			for _, p := range syncPlans {
				syncCancel = append(syncCancel, p.CancelTPOIDs...)
				forceTP = append(forceTP, p.ForceTPReplace...)
			}
			if fmt.Sprint(syncCancel) != fmt.Sprint(tc.wantSyncCancel) || fmt.Sprint(forceTP) != fmt.Sprint(tc.wantForceTP) {
				t.Fatalf("protection syncs = %+v, want cancel %v force_tp %v", syncPlans, tc.wantSyncCancel, tc.wantForceTP)
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
			var alerts, closeFails []string
			for _, m := range backend.messages {
				if strings.Contains(m.content, "LIVE ORDER FAILED") {
					closeFails = append(closeFails, m.content)
					continue
				}
				alerts = append(alerts, m.content)
			}
			dms := len(backend.dms)
			backend.mu.Unlock()
			if !tc.wantAlert && !tc.wantCloseFail && dms > 0 {
				t.Fatalf("owner DMs = %d, want none", dms)
			}
			if tc.wantAlert != (len(alerts) == 1) || len(alerts) > 1 || tc.wantCloseFail != (len(closeFails) == 1) || len(closeFails) > 1 {
				t.Fatalf("re-arm alerts=%q close-failure alerts=%q, want one re-arm alert: %t and one close-failure alert: %t", alerts, closeFails, tc.wantAlert, tc.wantCloseFail)
			}
			if tc.wantAlert && (strings.HasPrefix(alerts[0], "CRITICAL") != tc.wantCritical || !strings.Contains(alerts[0], tc.wantDetail) || (tc.notDetail != "" && strings.Contains(alerts[0], tc.notDetail))) {
				t.Fatalf("alert = %q, want critical %t, the detail %q and no %q", alerts[0], tc.wantCritical, tc.wantDetail, tc.notDetail)
			}
		})
	}
}

func confirmationTestStrategy(direction string) StrategyConfig {
	return StrategyConfig{
		ID: "hl-confirmation", Type: "perps", Platform: "hyperliquid", Symbol: "ETH",
		Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "1h", "--mode=live"},
		Direction: direction, Leverage: 2, SizingLeverage: 1,
	}
}

func unconfirmedExecuteResult() *HyperliquidExecuteResult {
	return &HyperliquidExecuteResult{
		Execution: &HyperliquidExecution{Fill: &HyperliquidFill{}},
	}
}

func confirmationNotifier() (*MultiNotifier, *mockNotifier) {
	backend := &mockNotifier{}
	return NewMultiNotifier(notifierBackend{
		notifier: backend,
		channels: map[string]string{"alerts": "alerts"},
		ownerID:  "owner",
	}), backend
}
