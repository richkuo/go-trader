package main

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManualCloseRearmDecision(t *testing.T) {
	snap := manualCloseProtectionSnapshot{Symbol: "ETH", Side: "long", Quantity: 0.4, StopLossOID: 5150, TriggerPx: 1900}
	cases := []struct {
		name      string
		result    *HyperliquidExecuteResult
		requested []int64
		snap      manualCloseProtectionSnapshot
		want      manualCloseRearmDecision
	}{
		{
			name:      "a partial close requests no stop cancel",
			result:    &HyperliquidExecuteResult{Error: "rejected"},
			requested: []int64{0, 7001},
			snap:      manualCloseProtectionSnapshot{Symbol: "ETH", Side: "long", Quantity: 0.4},
			want:      manualCloseRearmNoCancelRequested,
		},
		{
			name:      "a confirmed stop cancel re-arms",
			result:    &HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{5150}},
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmStopCancelled,
		},
		{
			name:      "a failed stop cancel is unconfirmed, never proof the trigger survived",
			result:    &HyperliquidExecuteResult{Error: "rejected", CancelStopLossError: "5150: rejected", CancelStopLossFailedOIDs: []int64{5150}},
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmCancelNotConfirmed,
		},
		{
			name:      "a result that names no cancel outcome is unconfirmed",
			result:    &HyperliquidExecuteResult{Error: "update_leverage failed"},
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmCancelNotConfirmed,
		},
		{
			name:      "an unreadable subprocess outcome re-arms",
			result:    nil,
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmOutcomeUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideManualCloseRearm(tc.result, tc.requested, tc.snap)
			if got != tc.want {
				t.Fatalf("decision = %d, want %d", got, tc.want)
			}
		})
	}
}

type rearmSLCall struct {
	symbol    string
	side      string
	size      float64
	triggerPx float64
	cancelOID int64
}

func TestManualCloseRestoresTheStopAfterAVenueRejection(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "test-secret")
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xoperator")

	const bookQty = 0.4
	const prevOID = int64(5150)
	const prevTrigger = 1900.0

	rejectedWithConfirmedCancel := func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return &HyperliquidExecuteResult{
			Error:                       "order value below the venue minimum",
			CancelStopLossSucceeded:     true,
			CancelStopLossSucceededOIDs: []int64{prevOID, 7001},
		}, "", nil
	}

	cases := []struct {
		name          string
		onChain       []HLPosition
		execute       func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error)
		slResult      *HyperliquidStopLossUpdateResult
		slErr         error
		wantSLCall    *rearmSLCall
		wantBookOID   int64
		wantBookTrig  float64
		wantAction    string
		wantActionPx  float64
		wantOutPart   string
		wantAlertPart string
	}{
		{
			name:         "a confirmed cancel restores the recorded trigger at the on-chain size",
			onChain:      []HLPosition{{Coin: "ETH", Size: 0.3}},
			execute:      rejectedWithConfirmedCancel,
			slResult:     &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger},
			wantSLCall:   &rearmSLCall{symbol: "ETH", side: "long", size: 0.3, triggerPx: prevTrigger, cancelOID: prevOID},
			wantBookOID:  6200,
			wantBookTrig: prevTrigger,
			wantAction:   "update-sl",
			wantOutPart:  "Stop-loss re-armed after the rejected close",
		},
		{
			name:    "an unconfirmed cancel is verified on-chain and restored, never assumed live",
			onChain: []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute: func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				return &HyperliquidExecuteResult{
					Error:                    "order value below the venue minimum",
					CancelStopLossError:      "5150: rejected",
					CancelStopLossFailedOIDs: []int64{prevOID},
				}, "", nil
			},
			slResult:     &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger},
			wantSLCall:   &rearmSLCall{symbol: "ETH", side: "long", size: bookQty, triggerPx: prevTrigger, cancelOID: prevOID},
			wantBookOID:  6200,
			wantBookTrig: prevTrigger,
			wantAction:   "update-sl",
			wantOutPart:  "verified on-chain and restored rather than assumed live",
		},
		{
			name:    "a venue that still reports the old stop resting places no duplicate and says so",
			onChain: []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute: func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				return &HyperliquidExecuteResult{
					Error:                    "order value below the venue minimum",
					CancelStopLossError:      "5150: rejected",
					CancelStopLossFailedOIDs: []int64{prevOID},
				}, "", nil
			},
			slResult:      &HyperliquidStopLossUpdateResult{CancelStopLossError: "cancel rejected", StopLossTriggerPx: prevTrigger},
			wantSLCall:    &rearmSLCall{symbol: "ETH", side: "long", size: bookQty, triggerPx: prevTrigger, cancelOID: prevOID},
			wantBookOID:   prevOID,
			wantBookTrig:  prevTrigger,
			wantOutPart:   "still resting when its open orders were read and no replacement was placed",
			wantAlertPart: "5150",
		},
		{
			name:          "a failed re-arm alerts with the symbol and the cancelled order ids",
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejectedWithConfirmedCancel,
			slErr:         fmt.Errorf("subprocess exited 1"),
			wantSLCall:    &rearmSLCall{symbol: "ETH", side: "long", size: bookQty, triggerPx: prevTrigger, cancelOID: prevOID},
			wantBookOID:   0,
			wantOutPart:   "UNVERIFIED",
			wantAlertPart: "5150 7001",
		},
		{
			name:        "a re-arm that fills immediately books no close and leaves it to the reconciler",
			onChain:     []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:     rejectedWithConfirmedCancel,
			slResult:    &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: prevTrigger},
			wantSLCall:  &rearmSLCall{symbol: "ETH", side: "long", size: bookQty, triggerPx: prevTrigger, cancelOID: prevOID},
			wantBookOID: 0,
			wantAction:  "cancel-sl",
			wantOutPart: "the reconciler books the close, with its venue fill and fee",
		},
		{
			name:    "an immediate fill after an unconfirmed cancel clears the stale order id",
			onChain: []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute: func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				return &HyperliquidExecuteResult{
					Error:                    "order value below the venue minimum",
					CancelStopLossError:      "5150: rejected",
					CancelStopLossFailedOIDs: []int64{prevOID},
				}, "", nil
			},
			slResult:    &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: prevTrigger},
			wantSLCall:  &rearmSLCall{symbol: "ETH", side: "long", size: bookQty, triggerPx: prevTrigger, cancelOID: prevOID},
			wantBookOID: 0,
			wantAction:  "cancel-sl",
			wantOutPart: "filled immediately",
		},
		{
			name:        "a venue that reports the position already gone places nothing",
			onChain:     []HLPosition{{Coin: "BTC", Size: 1}},
			execute:     rejectedWithConfirmedCancel,
			wantBookOID: 0,
			wantOutPart: "the venue reports no open ETH position",
		},
		{
			name:        "a venue that reports the coin net short places nothing for a long book",
			onChain:     []HLPosition{{Coin: "ETH", Size: -bookQty}},
			execute:     rejectedWithConfirmedCancel,
			wantBookOID: 0,
			wantOutPart: `the venue reports the ETH position net "short", not "long"`,
		},
		{
			name:         "a liquidation price inside the recorded trigger tightens the re-arm",
			onChain:      []HLPosition{{Coin: "ETH", Size: bookQty, LiquidationPx: 1950}},
			execute:      rejectedWithConfirmedCancel,
			slResult:     &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: 1959.75},
			wantSLCall:   &rearmSLCall{symbol: "ETH", side: "long", size: bookQty, triggerPx: 1959.75, cancelOID: prevOID},
			wantBookOID:  6200,
			wantBookTrig: 1959.75,
			wantAction:   "update-sl",
			wantOutPart:  "Stop-loss re-armed after the rejected close",
		},
		{
			name:    "an unreadable execute outcome still re-arms against the previous order id",
			onChain: []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute: func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				return nil, "", fmt.Errorf("execute error: signal: killed")
			},
			slResult:     &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger},
			wantSLCall:   &rearmSLCall{symbol: "ETH", side: "long", size: bookQty, triggerPx: prevTrigger, cancelOID: prevOID},
			wantBookOID:  6200,
			wantBookTrig: prevTrigger,
			wantAction:   "update-sl",
			wantOutPart:  "Stop-loss re-armed after the rejected close",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatalf("OpenStateDB: %v", err)
			}
			defer db.Close()

			subject := StrategyConfig{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
				Script: "shared_scripts/check_hyperliquid.py",
				Args:   []string{"hold", "ETH", "1h", "--mode=live"}, Capital: 1000, Leverage: 2}
			cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{subject}}

			state := &AppState{Strategies: map[string]*StrategyState{
				subject.ID: {ID: subject.ID, Type: subject.Type, Platform: "hyperliquid",
					Cash: 1000, InitialCapital: 1000,
					Positions: map[string]*Position{"ETH": {
						Symbol: "ETH", Quantity: bookQty, InitialQuantity: bookQty, AvgCost: 2000,
						Side: "long", Multiplier: 1, Leverage: 2, OwnerStrategyID: subject.ID,
						StopLossOID: prevOID, StopLossTriggerPx: prevTrigger, TPOIDs: []int64{7001},
						OpenedAt: time.Now().UTC().Add(-time.Hour),
					}}},
			}}
			if err := db.SaveState(state); err != nil {
				t.Fatalf("SaveState: %v", err)
			}

			notifier, backend := confirmationNotifier()
			d := newCLIManualCoreDeps(cfg, openTestStore(t, db), notifier)
			d.fetchMids = func(coins []string) (map[string]float64, error) {
				return map[string]float64{"ETH": 2000}, nil
			}
			d.fetchPositions = func(addr string) ([]HLPosition, error) { return tc.onChain, nil }
			d.execute = tc.execute
			var slCalls []rearmSLCall
			d.updateSL = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				slCalls = append(slCalls, rearmSLCall{symbol: symbol, side: side, size: size, triggerPx: triggerPx, cancelOID: cancelOID})
				return tc.slResult, "", tc.slErr
			}

			sc, lookupErr := lookupManualStrategy(cfg, subject.ID)
			if lookupErr != nil {
				t.Fatalf("lookup: %v", lookupErr)
			}
			res, coreErr := manualCloseCore(d, sc, manualCloseInputs{StrategyID: subject.ID})
			if coreErr == nil {
				t.Fatalf("manualCloseCore returned no error, want the rejected close to fail")
			}

			if tc.wantSLCall == nil {
				if len(slCalls) != 0 {
					t.Fatalf("updateSL calls = %+v, want none", slCalls)
				}
			} else {
				if len(slCalls) != 1 {
					t.Fatalf("updateSL calls = %+v, want exactly one", slCalls)
				}
				got, want := slCalls[0], *tc.wantSLCall
				if got.symbol != want.symbol || got.side != want.side || got.cancelOID != want.cancelOID ||
					math.Abs(got.size-want.size) > 1e-9 || math.Abs(got.triggerPx-want.triggerPx) > 1e-9 {
					t.Fatalf("updateSL call = %+v, want %+v", got, want)
				}
			}

			reloaded, _, loadErr := LoadStateWithStore(cfg, openTestStore(t, db))
			if loadErr != nil {
				t.Fatalf("LoadStateWithStore: %v", loadErr)
			}
			pos := reloaded.Strategies[subject.ID].Positions["ETH"]
			if pos == nil {
				t.Fatalf("position was removed from the book")
			}
			if pos.Quantity != bookQty {
				t.Fatalf("book quantity = %v, want %v", pos.Quantity, bookQty)
			}
			if pos.StopLossOID != tc.wantBookOID || math.Abs(pos.StopLossTriggerPx-tc.wantBookTrig) > 1e-9 {
				t.Fatalf("book stop = OID %d @ %v, want OID %d @ %v", pos.StopLossOID, pos.StopLossTriggerPx, tc.wantBookOID, tc.wantBookTrig)
			}

			actions, actErr := db.LoadPendingManualActions()
			if actErr != nil {
				t.Fatalf("LoadPendingManualActions: %v", actErr)
			}
			if tc.wantAction == "" {
				if len(actions) != 0 {
					t.Fatalf("queued actions = %+v, want none", actions)
				}
			} else {
				if len(actions) != 1 || actions[0].Action != tc.wantAction {
					t.Fatalf("queued actions = %+v, want one %q", actions, tc.wantAction)
				}
				if tc.wantActionPx > 0 && actions[0].FillPrice != tc.wantActionPx {
					t.Fatalf("queued fill price = %v, want %v", actions[0].FillPrice, tc.wantActionPx)
				}
				if tc.wantAction == "update-sl" && actions[0].StopLossOID != tc.wantBookOID {
					t.Fatalf("queued stop OID = %d, want %d", actions[0].StopLossOID, tc.wantBookOID)
				}
			}

			out := res.uiMessage()
			if !strings.Contains(out, tc.wantOutPart) {
				t.Fatalf("operator output = %q, want it to contain %q", out, tc.wantOutPart)
			}
			if tc.wantAlertPart != "" {
				backend.mu.Lock()
				var alerts []string
				for _, m := range backend.messages {
					alerts = append(alerts, m.content)
				}
				for _, dm := range backend.dms {
					alerts = append(alerts, dm.content)
				}
				backend.mu.Unlock()
				joined := strings.Join(alerts, "\n")
				if !strings.Contains(joined, tc.wantAlertPart) || !strings.Contains(joined, "ETH") {
					t.Fatalf("alerts = %q, want the symbol and %q", joined, tc.wantAlertPart)
				}
			}
		})
	}
}

func TestManualCloseRearmWaitsForThePerSymbolStopLock(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")

	sc := StrategyConfig{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
		Script: "shared_scripts/check_hyperliquid.py",
		Args:   []string{"hold", "ETH", "1h", "--mode=live"}}
	snap := manualCloseProtectionSnapshot{Symbol: "ETH", Side: "long", Quantity: 0.4, StopLossOID: 5150, TriggerPx: 1900}

	started := make(chan struct{})
	d := manualCoreDeps{
		updateSL: func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
			close(started)
			return &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: triggerPx}, "", nil
		},
		recordRearmedStopLoss: func(strategyID, symbol, side string, qty float64, prevStopOID int64, result *HyperliquidStopLossUpdateResult) error {
			return nil
		},
	}

	unlock := lockHyperliquidTrailingUpdate(snap.Symbol)
	done := make(chan struct{})
	go func() {
		defer close(done)
		restoreManualStopLossAfterFailedClose(d, &manualCoreResult{}, sc, sc.ID, snap,
			&HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{snap.StopLossOID}},
			[]int64{snap.StopLossOID})
	}()

	select {
	case <-started:
		unlock()
		t.Fatal("the re-arm placed a stop while the per-symbol stop lock was held by another replacement")
	case <-time.After(200 * time.Millisecond):
	}

	unlock()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-arm never placed a stop after the per-symbol stop lock was released")
	}
	<-done
}
