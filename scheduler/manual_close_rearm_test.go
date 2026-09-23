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
			result:    &HyperliquidExecuteResult{OrderOutcome: "rejected", Error: "rejected", CancelStopLossSucceededOIDs: []int64{5150}},
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmStopCancelled,
		},
		{
			name:      "a failed stop cancel is unconfirmed, never proof the trigger survived",
			result:    &HyperliquidExecuteResult{OrderOutcome: "rejected", Error: "rejected", CancelStopLossError: "5150: rejected", CancelStopLossFailedOIDs: []int64{5150}},
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmCancelNotConfirmed,
		},
		{
			name:      "a result that names no cancel outcome is unconfirmed",
			result:    &HyperliquidExecuteResult{OrderOutcome: "rejected", Error: "exchange rejected order"},
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmCancelNotConfirmed,
		},
		{
			name:      "a catch-all reply is an unknown outcome",
			result:    &HyperliquidExecuteResult{OrderOutcome: "unknown", Error: "socket closed"},
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmOutcomeUnknown,
		},
		{
			name:      "a reply that names no order outcome is unknown",
			result:    &HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{5150}},
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmOutcomeUnknown,
		},
		{
			name:      "a close the script never sent cancelled nothing and re-arms nothing",
			result:    &HyperliquidExecuteResult{OrderOutcome: "not_sent", Error: "update_leverage failed"},
			requested: []int64{5150},
			snap:      snap,
			want:      manualCloseRearmNoCancelRequested,
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

	rejectedWithConfirmedCancel := func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return &HyperliquidExecuteResult{
			OrderOutcome:                "rejected",
			Error:                       "order value below the venue minimum",
			CancelStopLossSucceeded:     true,
			CancelStopLossSucceededOIDs: []int64{prevOID, 7001},
		}, "", nil
	}

	cases := []struct {
		name          string
		onChain       []HLPosition
		execute       func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error)
		slResult      *HyperliquidStopLossUpdateResult
		slErr         error
		wantSLCall    *rearmSLCall
		wantBookOID   int64
		wantBookTrig  float64
		wantAction    string
		wantActionPx  float64
		wantOutPart   string
		wantAlertPart string
		shortPeer     float64
		readFails     bool
		wantCritical  int
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
			execute: func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				return &HyperliquidExecuteResult{
					OrderOutcome:             "rejected",
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
			execute: func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				return &HyperliquidExecuteResult{
					OrderOutcome:             "rejected",
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
			execute: func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				return &HyperliquidExecuteResult{
					OrderOutcome:             "rejected",
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
			name:          "a venue that reports the position already gone places nothing and alerts",
			onChain:       []HLPosition{{Coin: "BTC", Size: 1}},
			execute:       rejectedWithConfirmedCancel,
			wantBookOID:   0,
			wantOutPart:   "No stop-loss was placed",
			wantAlertPart: "shows no on-chain units behind the 0.400000 remainder",
		},
		{
			name:          "a venue that reports the coin net short places nothing for a long book and alerts",
			onChain:       []HLPosition{{Coin: "ETH", Size: -bookQty}},
			execute:       rejectedWithConfirmedCancel,
			wantBookOID:   0,
			wantOutPart:   "No stop-loss was placed",
			wantAlertPart: "shows no on-chain units behind the 0.400000 remainder",
		},
		{
			name:          "a rejection beside an opposite-side peer caps the stop at the own-side units and alerts once",
			onChain:       []HLPosition{{Coin: "ETH", Size: 0.24}},
			shortPeer:     0.16,
			execute:       rejectedWithConfirmedCancel,
			slResult:      &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger},
			wantSLCall:    &rearmSLCall{symbol: "ETH", side: "long", size: 0.24, triggerPx: prevTrigger, cancelOID: prevOID},
			wantBookOID:   6200,
			wantBookTrig:  prevTrigger,
			wantAction:    "update-sl",
			wantOutPart:   "Stop-loss re-armed after the rejected close",
			wantAlertPart: "other 0.160000 units",
			wantCritical:  1,
		},
		{
			name:          "a failed post-close read derives the stop from the pre-send reading and alerts once",
			onChain:       []HLPosition{{Coin: "ETH", Size: 0.24}},
			shortPeer:     0.16,
			readFails:     true,
			execute:       rejectedWithConfirmedCancel,
			slResult:      &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger},
			wantSLCall:    &rearmSLCall{symbol: "ETH", side: "long", size: 0.24, triggerPx: prevTrigger, cancelOID: prevOID},
			wantBookOID:   6200,
			wantBookTrig:  prevTrigger,
			wantAction:    "update-sl",
			wantOutPart:   "Stop-loss re-armed after the rejected close",
			wantAlertPart: "the pre-send account reading less the confirmed fill",
			wantCritical:  1,
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
			execute: func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
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
			peer := StrategyConfig{ID: "hl-manual-eth-short", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Script: subject.Script, Args: subject.Args, Capital: 1000, Leverage: 2}
			if tc.shortPeer > 0 {
				cfg.Strategies = append(cfg.Strategies, peer)
			}

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
			if tc.shortPeer > 0 {
				state.Strategies[peer.ID] = &StrategyState{ID: peer.ID, Type: "manual", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{"ETH": {
					Symbol: "ETH", Quantity: tc.shortPeer, InitialQuantity: tc.shortPeer, AvgCost: 2000, Side: "short", Multiplier: 1, Leverage: 2, OwnerStrategyID: peer.ID,
				}}}
			}
			if err := db.SaveState(state); err != nil {
				t.Fatalf("SaveState: %v", err)
			}

			notifier, backend := confirmationNotifier()
			d := newCLIManualCoreDeps(cfg, openTestStore(t, db), notifier)
			d.fetchMids = func(coins []string) (map[string]float64, error) {
				return map[string]float64{"ETH": 2000}, nil
			}
			closeSent := false
			d.fetchPositions = func(addr string) ([]HLPosition, error) {
				if closeSent && tc.readFails {
					return nil, fmt.Errorf("clearinghouseState timeout")
				}
				return tc.onChain, nil
			}
			d.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				closeSent = true
				return tc.execute(script, symbol, side, size, stopLossPct, cancelOID, prevPosQty, marginMode, leverage, closeMode, snapshot, extraCancelOIDs...)
			}
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
			if tc.wantCritical > 0 {
				backend.mu.Lock()
				critical := 0
				for _, m := range backend.messages {
					if strings.HasPrefix(m.content, "CRITICAL") {
						critical++
					}
				}
				backend.mu.Unlock()
				if critical != tc.wantCritical {
					t.Fatalf("critical channel alerts = %d, want %d", critical, tc.wantCritical)
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

func TestManualCloseShortFillRearmsTheRemainderStop(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "test-secret")
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xoperator")
	oldRecorder := tradeRecorder
	t.Cleanup(func() { tradeRecorder = oldRecorder })
	tradeRecorder = func(string, Trade) error { return nil }

	const prevOID = int64(5150)
	const prevTrigger = 1900.0
	cases := []struct {
		name         string
		fillSz       float64
		succeeded    []int64
		failed       []int64
		slResult     *HyperliquidStopLossUpdateResult
		wantSLCall   *rearmSLCall
		wantDrainQty float64
		wantDrainSL  int64
		wantOutPart  string
		chains       []float64
		wantAlert    bool
		peerSide     string
		peerQty      float64
		failure      *HyperliquidExecuteResult
		wantAlertIn  string
		wantAlsoIn   string
		syncResult   *HyperliquidProtectionSyncResult
		wantSyncTPs  []int64
		wantRestore  []int64
		wantReads    int
	}{
		{name: "a confirmed stop cancel restores the recorded trigger at the remainder", fillSz: 0.3, succeeded: []int64{prevOID, 7001},
			slResult:   &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger},
			wantSLCall: &rearmSLCall{symbol: "ETH", side: "long", size: 0.1, triggerPx: prevTrigger, cancelOID: prevOID}, wantDrainQty: 0.1, wantDrainSL: 6200,
			wantOutPart: "Stop-loss re-armed for the remainder after the short fill"},
		{name: "a stop cancel the venue reported as failed is verified on-chain and replaced at the remainder", fillSz: 0.3, succeeded: []int64{7001}, failed: []int64{prevOID},
			slResult:   &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger, CancelStopLossSucceeded: true},
			wantSLCall: &rearmSLCall{symbol: "ETH", side: "long", size: 0.1, triggerPx: prevTrigger, cancelOID: prevOID}, wantDrainQty: 0.1, wantDrainSL: 6200,
			wantOutPart: "is checked on-chain before a stop for the 0.100000 remainder is placed"},
		{name: "a re-armed stop that fills at once leaves the remainder to the reconciler with no stop id", fillSz: 0.3, succeeded: []int64{prevOID, 7001},
			slResult:   &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: prevTrigger},
			wantSLCall: &rearmSLCall{symbol: "ETH", side: "long", size: 0.1, triggerPx: prevTrigger, cancelOID: prevOID}, wantDrainQty: 0.1,
			wantOutPart: "filled immediately"},
		{name: "a full fill places no stop", fillSz: 0.4, succeeded: []int64{prevOID, 7001}, wantOutPart: "Queued"},
		{name: "a capped shared close places no stop for the unbacked remainder and alerts", fillSz: 0.2, succeeded: []int64{prevOID, 7001}, chains: []float64{0.7, 0.5},
			wantDrainQty: 0.2, wantOutPart: "No stop-loss was placed", wantAlert: true},
		{name: "a short fill beside an opposite-side peer caps the stop at the own-side units and alerts", fillSz: 0.1, succeeded: []int64{prevOID, 7001}, chains: []float64{0.3, 0.2}, peerSide: "short", peerQty: 0.1,
			slResult:   &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger},
			wantSLCall: &rearmSLCall{symbol: "ETH", side: "long", size: 0.2, triggerPx: prevTrigger, cancelOID: prevOID}, wantDrainQty: 0.3, wantDrainSL: 6200,
			wantOutPart: "Stop-loss re-armed for the remainder after the short fill", wantAlert: true, wantAlertIn: "other 0.100000 units"},
		{name: "a catch-all reply whose post-close read holds only the peer units places no stop and alerts", fillSz: 0, succeeded: []int64{prevOID, 7001}, chains: []float64{0.9, 0.5},
			failure:      &HyperliquidExecuteResult{OrderOutcome: "unknown", Error: "socket closed"},
			wantDrainQty: 0.4, wantOutPart: "No stop-loss was placed", wantAlert: true, wantAlertIn: "reconciler books any fill"},
		{name: "a chain that reads flat after the fill places nothing and alerts", fillSz: 0.3, succeeded: []int64{prevOID, 7001}, chains: []float64{0.9, 0},
			wantDrainQty: 0.1, wantOutPart: "No stop-loss was placed", wantAlert: true},
		{name: "a capped shared close after a rejected stop cancel removes the pre-close stop verified first", fillSz: 0.2, succeeded: []int64{7001}, failed: []int64{prevOID}, chains: []float64{0.7, 0.5},
			slResult:   &HyperliquidStopLossUpdateResult{CancelOnly: true, CancelStopLossSucceeded: true},
			wantSLCall: &rearmSLCall{symbol: "ETH", side: "long", size: 0, triggerPx: 0, cancelOID: prevOID}, wantDrainQty: 0.2, wantDrainSL: 0,
			wantOutPart: "No stop-loss was placed", wantAlert: true, wantAlertIn: "stop OID 5150 removed"},
		{name: "a rejected removal of the pre-close stop names it still resting and the manual cancel", fillSz: 0.2, succeeded: []int64{7001}, failed: []int64{prevOID}, chains: []float64{0.7, 0.5},
			slResult:   &HyperliquidStopLossUpdateResult{CancelOnly: true, Error: "cancel of stop-loss OID=5150 failed: busy", CancelStopLossError: "busy"},
			wantSLCall: &rearmSLCall{symbol: "ETH", side: "long", size: 0, triggerPx: 0, cancelOID: prevOID}, wantDrainQty: 0.2, wantDrainSL: prevOID,
			wantOutPart: "No stop-loss was placed", wantAlert: true, wantAlertIn: "stop OID 5150 STILL RESTING", wantAlsoIn: "manual-cancel-sl hl-manual-eth"},
		{name: "a take-profit whose cancel failed on a short fill is removed verify-first and cleared by a restore-tp row", fillSz: 0.3, succeeded: []int64{prevOID}, failed: []int64{7001},
			slResult:   &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger},
			wantSLCall: &rearmSLCall{symbol: "ETH", side: "long", size: 0.1, triggerPx: prevTrigger, cancelOID: prevOID}, wantDrainQty: 0.1, wantDrainSL: 6200,
			syncResult:  &HyperliquidProtectionSyncResult{},
			wantSyncTPs: []int64{7001}, wantRestore: []int64{0},
			wantOutPart: "Take-profit OID=7001 for ETH was removed after the short fill"},
		{name: "a close the script never sent restores nothing and reads nothing after it", failed: nil, chains: []float64{0.9},
			failure:      &HyperliquidExecuteResult{OrderOutcome: "not_sent", Error: "close size floors to zero at lot precision"},
			wantDrainQty: 0.4, wantDrainSL: prevOID, wantOutPart: "no order was sent and no protection was cancelled; nothing to restore", wantReads: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatalf("OpenStateDB: %v", err)
			}
			defer db.Close()
			live := []string{"hold", "ETH", "1h", "--mode=live"}
			subject := StrategyConfig{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Args: live, Capital: 1000, Leverage: 2}
			peer := StrategyConfig{ID: "hl-manual-eth-peer", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Script: subject.Script, Args: live, Capital: 1000, Leverage: 2}
			peerSide, peerQty := "long", 0.5
			if tc.peerSide != "" {
				peerSide, peerQty = tc.peerSide, tc.peerQty
			}
			cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{subject, peer}}
			state := &AppState{Strategies: map[string]*StrategyState{
				subject.ID: {ID: subject.ID, Type: "manual", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{"ETH": {
					Symbol: "ETH", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000, Side: "long", Multiplier: 1, Leverage: 2, OwnerStrategyID: subject.ID,
					StopLossOID: prevOID, StopLossTriggerPx: prevTrigger, TPOIDs: []int64{7001}, OpenedAt: time.Now().UTC().Add(-time.Hour),
				}}},
				peer.ID: {ID: peer.ID, Type: "manual", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{"ETH": {
					Symbol: "ETH", Quantity: peerQty, InitialQuantity: peerQty, AvgCost: 2000, Side: peerSide, Multiplier: 1, Leverage: 2, OwnerStrategyID: peer.ID,
				}}},
			}}
			if err := db.SaveState(state); err != nil {
				t.Fatalf("SaveState: %v", err)
			}
			notifier, backend := confirmationNotifier()
			d := newCLIManualCoreDeps(cfg, openTestStore(t, db), notifier)
			d.fetchMids = func([]string) (map[string]float64, error) { return map[string]float64{"ETH": 2000}, nil }
			reads := 0
			d.fetchPositions = func(string) ([]HLPosition, error) {
				chain := 0.9
				if len(tc.chains) > 0 {
					chain = tc.chains[len(tc.chains)-1]
					if reads < len(tc.chains) {
						chain = tc.chains[reads]
					}
				}
				reads++
				if chain == 0 {
					return nil, nil
				}
				return []HLPosition{{Coin: "ETH", Size: chain}}, nil
			}
			d.execute = func(_ string, _ string, _ string, size float64, _ float64, _ int64, _ float64, _ string, _ float64, _ hlCloseMode, _ hlExecuteSnapshot, _ ...int64) (*HyperliquidExecuteResult, string, error) {
				r := &HyperliquidExecuteResult{OrderOutcome: "filled", Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: tc.fillSz, OID: 4343, Fee: 0.1}}, CancelStopLossSucceededOIDs: tc.succeeded, CancelStopLossFailedOIDs: tc.failed}
				if tc.failure != nil {
					failure := *tc.failure
					failure.CancelStopLossSucceededOIDs = tc.succeeded
					r = &failure
				}
				if len(tc.failed) > 0 {
					r.CancelStopLossError = "cancel rejected"
				}
				return r, "", nil
			}
			var slCalls []rearmSLCall
			d.updateSL = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				slCalls = append(slCalls, rearmSLCall{symbol: symbol, side: side, size: size, triggerPx: triggerPx, cancelOID: cancelOID})
				return tc.slResult, "", nil
			}
			var syncTPs []int64
			d.syncProtection = func(sc StrategyConfig, plan hlProtectionPlan) (*HyperliquidProtectionSyncResult, string, error) {
				if plan.Size != 0 || plan.StopLossATRMult != 0 || len(plan.Tiers) != 0 || plan.AvgCost <= 0 {
					t.Fatalf("take-profit removal plan = %+v, want size 0, no stop leg, no tiers and a valid anchor", plan)
				}
				syncTPs = append(syncTPs, plan.CancelTPOIDs...)
				return tc.syncResult, "", nil
			}
			res, coreErr := manualCloseCore(d, subject, manualCloseInputs{StrategyID: subject.ID})
			if fmt.Sprint(syncTPs) != fmt.Sprint(tc.wantSyncTPs) {
				t.Fatalf("take-profit removals = %v, want %v", syncTPs, tc.wantSyncTPs)
			}
			if tc.wantReads > 0 && reads != tc.wantReads {
				t.Fatalf("account reads = %d, want %d", reads, tc.wantReads)
			}
			var restore []int64
			if rows, rowsErr := db.LoadPendingManualActions(); rowsErr == nil {
				for _, row := range rows {
					if row.Action == "restore-tp" {
						restore = row.TPOIDs
					}
				}
			}
			if fmt.Sprint(restore) != fmt.Sprint(tc.wantRestore) {
				t.Fatalf("queued restore-tp oids = %v, want %v", restore, tc.wantRestore)
			}
			if (coreErr != nil) != (tc.failure != nil) {
				t.Fatalf("manualCloseCore error = %v, want an error: %t", coreErr, tc.failure != nil)
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
				if got.symbol != want.symbol || got.side != want.side || got.cancelOID != want.cancelOID || math.Abs(got.size-want.size) > 1e-9 || math.Abs(got.triggerPx-want.triggerPx) > 1e-9 {
					t.Fatalf("updateSL call = %+v, want %+v", got, want)
				}
			}
			if out := res.uiMessage(); !strings.Contains(out, tc.wantOutPart) {
				t.Fatalf("operator output = %q, want it to contain %q", out, tc.wantOutPart)
			}
			backend.mu.Lock()
			var critical []string
			for _, m := range backend.messages {
				if strings.HasPrefix(m.content, "CRITICAL") {
					critical = append(critical, m.content)
				}
			}
			backend.mu.Unlock()
			if tc.wantAlert != (len(critical) == 1) || len(critical) > 1 {
				t.Fatalf("critical alerts = %q, want one: %t", critical, tc.wantAlert)
			}
			if tc.wantAlert && (!strings.Contains(critical[0], tc.wantAlertIn) || !strings.Contains(critical[0], tc.wantAlsoIn)) {
				t.Fatalf("critical alert = %q, want it to contain %q and %q", critical[0], tc.wantAlertIn, tc.wantAlsoIn)
			}
			store := openTestStore(t, db)
			reloaded, _, loadErr := LoadStateWithStore(cfg, store)
			if loadErr != nil {
				t.Fatalf("LoadStateWithStore: %v", loadErr)
			}
			drainPendingManualActions(reloaded, cfg, store)
			pos := reloaded.Strategies[subject.ID].Positions["ETH"]
			switch {
			case tc.wantDrainQty == 0 && pos != nil:
				t.Fatalf("book after drain = %+v, want the position closed", pos)
			case tc.wantDrainQty > 0 && pos == nil:
				t.Fatalf("book after drain is empty, want %g", tc.wantDrainQty)
			case tc.wantDrainQty > 0 && (math.Abs(pos.Quantity-tc.wantDrainQty) > 1e-9 || pos.StopLossOID != tc.wantDrainSL):
				t.Fatalf("book after drain qty=%g sl=%d, want qty %g sl %d", pos.Quantity, pos.StopLossOID, tc.wantDrainQty, tc.wantDrainSL)
			}
			var closeQtys []float64
			for _, tr := range reloaded.Strategies[subject.ID].TradeHistory {
				if tr.IsClose {
					closeQtys = append(closeQtys, tr.Quantity)
				}
			}
			var wantCloseQtys []float64
			if tc.failure == nil {
				wantCloseQtys = []float64{tc.fillSz}
			}
			if fmt.Sprint(closeQtys) != fmt.Sprint(wantCloseQtys) {
				t.Fatalf("close trades = %v, want %v", closeQtys, wantCloseQtys)
			}
		})
	}
}
