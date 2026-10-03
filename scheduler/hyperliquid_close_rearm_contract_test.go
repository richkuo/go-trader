package main

import (
	"strings"
	"sync"
	"testing"
)

func TestSizedClosePartialFillBooksTheFill(t *testing.T) {
	s := &StrategyState{
		ID:       "hl-eth",
		Platform: "hyperliquid",
		Type:     "perps",
		Cash:     10000,
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Side: "long", Quantity: 10, AvgCost: 2000},
		},
	}
	result := &HyperliquidResult{Signal: -1, Symbol: "ETH", SizedCloseBookFraction: 0.4}
	result.CloseFraction = 1
	exec := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: 4, OID: 9}}}
	executeHyperliquidResultDeferredOpen(StrategyConfig{ID: s.ID, Platform: "hyperliquid", Type: "perps", Direction: DirectionLong}, s, result, exec, "SELL", 2100, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(s.ID))
	pos := s.Positions["ETH"]
	if pos == nil || pos.Quantity < 5.9 || pos.Quantity > 6.1 {
		t.Fatalf("book = %+v, want 6 of 10 kept after a fill of 4", pos)
	}
}

func TestRearmQZeroRemoveUnconfirmedStop(t *testing.T) {
	orig := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = orig })
	for _, tc := range []struct {
		name        string
		result      *HyperliquidStopLossUpdateResult
		wantOID     int64
		wantResting bool
	}{
		{name: "a rejected cancel is removed", result: &HyperliquidStopLossUpdateResult{CancelOnly: true, CancelStopLossSucceeded: true}, wantOID: 0},
		{name: "a rejected removal leaves the stop and says it is still resting", result: &HyperliquidStopLossUpdateResult{CancelOnly: true, CancelStopLossError: "busy"}, wantOID: 111, wantResting: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var gotSize float64
			var gotOID int64
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				calls++
				gotSize, gotOID = size, cancelOID
				return tc.result, "", nil
			}
			state := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: 10, StopLossOID: 111, StopLossTriggerPx: 1900},
			}}
			var mu sync.RWMutex
			sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid"}
			_, _, res, _ := rearmProtectionForCloseRemainder(sc, state, nil, "ETH", 2000, 111, 1900, 0, nil, nil, nil, hlCloseUnconfirmed{StopOID: 111}, hlCloseRemainderStop{}, &mu, nil, nil)
			if calls != 1 || gotSize != 0 || gotOID != 111 {
				t.Fatalf("removal calls=%d size=%g oid=%d, want one size-0 call for 111", calls, gotSize, gotOID)
			}
			if state.Positions["ETH"].StopLossOID != tc.wantOID {
				t.Fatalf("book stop = %d, want %d", state.Positions["ETH"].StopLossOID, tc.wantOID)
			}
			report := formatCloseRemovalReport(sc, "ETH", res.Removal)
			if tc.wantResting != strings.Contains(report, "STILL RESTING") {
				t.Fatalf("report = %q, resting %t", report, tc.wantResting)
			}
		})
	}
}

func TestDecideManualCloseRearmUnknownAndNotSent(t *testing.T) {
	snap := manualCloseProtectionSnapshot{Symbol: "ETH", Side: "long", Quantity: 0.4, StopLossOID: 5150}
	for _, tc := range []struct {
		name   string
		result *HyperliquidExecuteResult
		want   manualCloseRearmDecision
	}{
		{name: "not_sent", result: &HyperliquidExecuteResult{OrderOutcome: "not_sent", Error: "update_leverage failed"}, want: manualCloseRearmNoCancelRequested},
		{name: "unknown", result: &HyperliquidExecuteResult{OrderOutcome: "unknown", Error: "socket closed"}, want: manualCloseRearmOutcomeUnknown},
		{name: "missing field", result: &HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{5150}}, want: manualCloseRearmOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideManualCloseRearm(tc.result, []int64{5150}, snap); got != tc.want {
				t.Fatalf("decision = %d, want %d", got, tc.want)
			}
		})
	}
}
