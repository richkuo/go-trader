package main

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type tpSyncCall struct {
	symbol   string
	side     string
	size     float64
	slMult   float64
	forceSL  bool
	tpOIDs   []int64
	tpArmed  []bool
	tierQty  int
	cancelTP []int64
}

func TestManualCloseRestoresTakeProfitsAfterAVenueRejection(t *testing.T) {
	t.Setenv("HYPERLIQUID_SECRET_KEY", "test-secret")
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xoperator")

	const bookQty = 0.4
	const prevSLOID = int64(5150)
	const prevTrigger = 1900.0

	rejected := func(succeeded ...int64) func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
			return &HyperliquidExecuteResult{
				Error:                       "order value below the venue minimum",
				CancelStopLossSucceeded:     len(succeeded) > 0,
				CancelStopLossSucceededOIDs: succeeded,
			}, "", nil
		}
	}

	cases := []struct {
		name          string
		closeStrategy *StrategyRef
		args          []string
		bookTPOIDs    []int64
		bookTPArmed   []bool
		stopOID       int64
		stopTrigger   float64
		closeQty      float64
		onChain       []HLPosition
		execute       func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeMode hlCloseMode, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error)
		slResult      *HyperliquidStopLossUpdateResult
		syncResult    *HyperliquidProtectionSyncResult
		syncErr       error
		recordErr     error
		wantSyncCall  *tpSyncCall
		wantBookOIDs  []int64
		wantBookArmed []bool
		wantAction    []int64
		wantPrevOIDs  []int64
		wantOutPart   string
		wantAlertPart string
		wantQuiet     bool
	}{
		{
			name:        "a close the script never sent restores nothing and alerts nothing",
			bookTPOIDs:  []int64{7001, 7002, 7003},
			bookTPArmed: []bool{true, true, true},
			stopOID:     prevSLOID,
			stopTrigger: prevTrigger,
			onChain:     []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute: func(string, string, string, float64, float64, int64, float64, string, float64, hlCloseMode, hlExecuteSnapshot, ...int64) (*HyperliquidExecuteResult, string, error) {
				return &HyperliquidExecuteResult{OrderOutcome: "not_sent", Error: "close size floors to zero at lot precision"}, "", nil
			},
			wantBookOIDs:  []int64{7001, 7002, 7003},
			wantBookArmed: []bool{true, true, true},
			wantOutPart:   "no order was sent and no protection was cancelled; nothing to restore",
			wantQuiet:     true,
		},
		{
			name:        "a confirmed cancel restores the cancelled tiers and leaves the restored stop alone",
			bookTPOIDs:  []int64{0, 7002, 7003},
			bookTPArmed: []bool{true, true, true},
			stopOID:     prevSLOID,
			stopTrigger: prevTrigger,
			onChain:     []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:     rejected(prevSLOID, 7002, 7003),
			slResult:    &HyperliquidStopLossUpdateResult{StopLossOID: 6200, StopLossTriggerPx: prevTrigger},
			syncResult:  &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 9002, 9003}, TPPxs: []float64{2050, 2100, 2150}},
			wantSyncCall: &tpSyncCall{symbol: "ETH", side: "long", size: bookQty, slMult: 0,
				tpOIDs: []int64{0, 0, 0}, tpArmed: []bool{true, false, false}, tierQty: 3},
			wantBookOIDs:  []int64{0, 9002, 9003},
			wantBookArmed: []bool{true, true, true},
			wantAction:    []int64{0, 9002, 9003},
			wantPrevOIDs:  []int64{0, 7002, 7003},
			wantOutPart:   "Take-profit tier 2 restored after the rejected close",
		},
		{
			name:          "recovery runs when no stop-loss was recorded",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(7001, 7002, 7003),
			syncResult:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{9001, 9002, 9003}, TPPxs: []float64{2050, 2100, 2150}},
			wantSyncCall:  &tpSyncCall{symbol: "ETH", side: "long", size: bookQty, tpOIDs: []int64{0, 0, 0}, tpArmed: []bool{false, false, false}, tierQty: 3},
			wantBookOIDs:  []int64{9001, 9002, 9003},
			wantBookArmed: []bool{true, true, true},
			wantAction:    []int64{9001, 9002, 9003},
			wantPrevOIDs:  []int64{7001, 7002, 7003},
			wantOutPart:   "Take-profit tier 1 restored after the rejected close",
		},
		{
			name:          "a stop-loss that filled immediately places no take-profit",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			stopOID:       prevSLOID,
			stopTrigger:   prevTrigger,
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(prevSLOID, 7001, 7002, 7003),
			slResult:      &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: prevTrigger},
			wantBookOIDs:  []int64{0, 0, 0},
			wantBookArmed: []bool{false, false, false},
			wantOutPart:   "the stop-loss leg left the position flat on-chain",
		},
		{
			name:          "a position gone on-chain places no take-profit",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       nil,
			execute:       rejected(7001, 7002, 7003),
			wantBookOIDs:  []int64{0, 0, 0},
			wantBookArmed: []bool{false, false, false},
			wantOutPart:   "the venue reports no open ETH position",
		},
		{
			name:          "a reversed position places no take-profit",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: -bookQty}},
			execute:       rejected(7001, 7002, 7003),
			wantBookOIDs:  []int64{0, 0, 0},
			wantBookArmed: []bool{false, false, false},
			wantOutPart:   `the venue reports the ETH position net "short"`,
		},
		{
			name:          "a partial placement books the resting tier and alerts with the failed tier",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(7001, 7002, 7003),
			syncResult:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{9001, 9002, 0}, TPErrors: []string{"", "", "place_take_profit_limit SDK error: rejected"}},
			wantSyncCall:  &tpSyncCall{symbol: "ETH", side: "long", size: bookQty, tpOIDs: []int64{0, 0, 0}, tpArmed: []bool{false, false, false}, tierQty: 3},
			wantBookOIDs:  []int64{9001, 9002, 0},
			wantBookArmed: []bool{true, true, false},
			wantAction:    []int64{9001, 9002, 0},
			wantPrevOIDs:  []int64{7001, 7002, 7003},
			wantAlertPart: "tier 3 (cancelled OID=7003) was not restored",
		},
		{
			name:          "a subprocess error clears the tiers and alerts",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(7001, 7002, 7003),
			syncErr:       fmt.Errorf("script error: exit status 1"),
			wantSyncCall:  &tpSyncCall{symbol: "ETH", side: "long", size: bookQty, tpOIDs: []int64{0, 0, 0}, tpArmed: []bool{false, false, false}, tierQty: 3},
			wantBookOIDs:  []int64{0, 0, 0},
			wantBookArmed: []bool{false, false, false},
			wantAction:    []int64{0, 0, 0},
			wantPrevOIDs:  []int64{7001, 7002, 7003},
			wantAlertPart: "script error: exit status 1",
		},
		{
			name:          "an unconfirmed tier with unreadable open orders is reported unverified",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(),
			syncResult:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{7001, 7002, 7003}, OpenOrderCheckError: "userOpenOrders failed"},
			wantSyncCall:  &tpSyncCall{symbol: "ETH", side: "long", size: bookQty, tpOIDs: []int64{7001, 7002, 7003}, tpArmed: []bool{true, true, true}, tierQty: 3},
			wantBookOIDs:  []int64{7001, 7002, 7003},
			wantBookArmed: []bool{true, true, true},
			wantAlertPart: "could not be verified: userOpenOrders failed",
		},
		{
			name:          "an immediately filled replacement completes the tier and defers booking",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(7002),
			syncResult:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{7001, 0, 7003}, TPFilledImmediately: []bool{false, true, false}},
			wantSyncCall:  &tpSyncCall{symbol: "ETH", side: "long", size: bookQty, tpOIDs: []int64{7001, 0, 7003}, tpArmed: []bool{true, false, true}, tierQty: 3},
			wantBookOIDs:  []int64{7001, 0, 7003},
			wantBookArmed: []bool{true, true, true},
			wantAction:    []int64{7001, 0, 7003},
			wantPrevOIDs:  []int64{7001, 7002, 7003},
			wantOutPart:   "The restored take-profit for tier 2 of ETH filled immediately",
		},
		{
			name:          "a virtual quantity over the on-chain size is capped",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: 0.25}},
			execute:       rejected(7001, 7002, 7003),
			syncResult:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{9001, 9002, 9003}},
			wantSyncCall:  &tpSyncCall{symbol: "ETH", side: "long", size: 0.25, tpOIDs: []int64{0, 0, 0}, tpArmed: []bool{false, false, false}, tierQty: 3},
			wantBookOIDs:  []int64{9001, 9002, 9003},
			wantBookArmed: []bool{true, true, true},
			wantAction:    []int64{9001, 9002, 9003},
			wantPrevOIDs:  []int64{7001, 7002, 7003},
			wantOutPart:   "Take-profit tier 1 restored after the rejected close",
		},
		{
			name:          "a bookkeeping failure alerts with the restored order ids",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(7001, 7002, 7003),
			syncResult:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{9001, 9002, 9003}},
			recordErr:     fmt.Errorf("database is locked"),
			wantSyncCall:  &tpSyncCall{symbol: "ETH", side: "long", size: bookQty, tpOIDs: []int64{0, 0, 0}, tpArmed: []bool{false, false, false}, tierQty: 3},
			wantBookOIDs:  []int64{0, 0, 0},
			wantBookArmed: []bool{false, false, false},
			wantAlertPart: "could not be recorded in the book (database is locked)",
		},
		{
			name:          "a partial close never restores take-profits",
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			closeQty:      0.1,
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(),
			wantBookOIDs:  []int64{7001, 7002, 7003},
			wantBookArmed: []bool{true, true, true},
		},
		{
			name:          "a paper strategy places no take-profit",
			args:          []string{"hold", "ETH", "1h", "--mode=paper"},
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(7001, 7002, 7003),
			wantBookOIDs:  []int64{0, 0, 0},
			wantBookArmed: []bool{false, false, false},
		},
		{
			name:          "a non-tiered strategy places no take-profit",
			closeStrategy: &StrategyRef{Name: "trailing_tp_ratchet"},
			bookTPOIDs:    []int64{7001, 7002, 7003},
			bookTPArmed:   []bool{true, true, true},
			onChain:       []HLPosition{{Coin: "ETH", Size: bookQty}},
			execute:       rejected(7001, 7002, 7003),
			wantBookOIDs:  []int64{0, 0, 0},
			wantBookArmed: []bool{false, false, false},
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

			args := tc.args
			if args == nil {
				args = []string{"hold", "ETH", "1h", "--mode=live"}
			}
			closeStrategy := tc.closeStrategy
			if closeStrategy == nil {
				closeStrategy = tieredTPCloseStrategy()
			}
			subject := StrategyConfig{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
				Script: "shared_scripts/check_hyperliquid.py", Args: args, Capital: 1000, Leverage: 2,
				CloseStrategy: closeStrategy}
			cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{subject}}

			state := &AppState{Strategies: map[string]*StrategyState{
				subject.ID: {ID: subject.ID, Type: subject.Type, Platform: "hyperliquid",
					Cash: 1000, InitialCapital: 1000,
					Positions: map[string]*Position{"ETH": {
						Symbol: "ETH", TradePositionID: "pos-1", Quantity: bookQty, InitialQuantity: bookQty,
						AvgCost: 2000, EntryATR: 50, Side: "long", Multiplier: 1, Leverage: 2,
						OwnerStrategyID: subject.ID, StopLossOID: tc.stopOID, StopLossTriggerPx: tc.stopTrigger,
						TPOIDs: cloneInt64s(tc.bookTPOIDs), TPArmedTiers: append([]bool(nil), tc.bookTPArmed...),
						OpenedAt: time.Now().UTC().Add(-time.Hour),
					}}},
			}}
			if err := db.SaveState(state); err != nil {
				t.Fatalf("SaveState: %v", err)
			}

			notifier, backend := confirmationNotifier()
			d := newCLIManualCoreDeps(cfg, openTestStore(t, db), notifier)
			d.fetchMids = func(coins []string) (map[string]float64, error) { return map[string]float64{"ETH": 2000}, nil }
			reads, updates := 0, 0
			d.fetchPositions = func(addr string) ([]HLPosition, error) {
				reads++
				return tc.onChain, nil
			}
			d.execute = tc.execute
			d.updateSL = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				updates++
				return tc.slResult, "", nil
			}
			var syncCalls []tpSyncCall
			d.syncProtection = func(sc StrategyConfig, plan hlProtectionPlan) (*HyperliquidProtectionSyncResult, string, error) {
				syncCalls = append(syncCalls, tpSyncCall{
					symbol: plan.Symbol, side: plan.Side, size: plan.Size, slMult: plan.StopLossATRMult,
					forceSL: plan.ForceSLReplace, tpOIDs: cloneInt64s(plan.TPOIDs),
					tpArmed: append([]bool(nil), plan.TPArmedTiers...), tierQty: len(plan.Tiers),
					cancelTP: cloneInt64s(plan.CancelTPOIDs),
				})
				return tc.syncResult, "", tc.syncErr
			}
			if tc.recordErr != nil {
				d.recordRestoredTakeProfits = func(strategyID, symbol, side, positionID string, outcomes []manualCloseTPTierOutcome) error {
					return tc.recordErr
				}
			}

			sc, lookupErr := lookupManualStrategy(cfg, subject.ID)
			if lookupErr != nil {
				t.Fatalf("lookup: %v", lookupErr)
			}
			res, coreErr := manualCloseCore(d, sc, manualCloseInputs{StrategyID: subject.ID, Qty: tc.closeQty})
			if coreErr == nil {
				t.Fatalf("manualCloseCore returned no error, want the rejected close to fail")
			}

			if tc.wantSyncCall == nil {
				if len(syncCalls) != 0 {
					t.Fatalf("protection sync calls = %+v, want none", syncCalls)
				}
			} else {
				if len(syncCalls) != 1 {
					t.Fatalf("protection sync calls = %+v, want exactly one", syncCalls)
				}
				got, want := syncCalls[0], *tc.wantSyncCall
				if got.symbol != want.symbol || got.side != want.side || math.Abs(got.size-want.size) > 1e-9 ||
					got.slMult != want.slMult || got.forceSL || got.tierQty != want.tierQty || len(got.cancelTP) != 0 {
					t.Fatalf("protection sync call = %+v, want %+v with no stop-loss leg and no cancels", got, want)
				}
				if !reflect.DeepEqual(got.tpOIDs, want.tpOIDs) || !reflect.DeepEqual(got.tpArmed, want.tpArmed) {
					t.Fatalf("protection sync tp inputs = %v / %v, want %v / %v", got.tpOIDs, got.tpArmed, want.tpOIDs, want.tpArmed)
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
			if !reflect.DeepEqual(pos.TPOIDs, tc.wantBookOIDs) {
				t.Fatalf("book tp oids = %v, want %v", pos.TPOIDs, tc.wantBookOIDs)
			}
			if !reflect.DeepEqual(pos.TPArmedTiers, tc.wantBookArmed) {
				t.Fatalf("book armed tiers = %v, want %v", pos.TPArmedTiers, tc.wantBookArmed)
			}
			if tc.slResult != nil && tc.slResult.StopLossOID > 0 && pos.StopLossOID != tc.slResult.StopLossOID {
				t.Fatalf("book stop OID = %d, want the restored %d left intact", pos.StopLossOID, tc.slResult.StopLossOID)
			}

			actions, actErr := db.LoadPendingManualActions()
			if actErr != nil {
				t.Fatalf("LoadPendingManualActions: %v", actErr)
			}
			var restore *PendingManualAction
			for i := range actions {
				if actions[i].Action == "restore-tp" {
					restore = &actions[i]
				}
			}
			if tc.wantAction == nil {
				if restore != nil {
					t.Fatalf("queued restore-tp = %+v, want none", restore)
				}
			} else {
				if restore == nil {
					t.Fatalf("queued actions = %+v, want a restore-tp row", actions)
				}
				if !reflect.DeepEqual(restore.TPOIDs, tc.wantAction) {
					t.Fatalf("queued restore-tp oids = %v, want %v", restore.TPOIDs, tc.wantAction)
				}
				if !reflect.DeepEqual(restore.PrevTPOIDs, tc.wantPrevOIDs) {
					t.Fatalf("queued restore-tp prev oids = %v, want %v", restore.PrevTPOIDs, tc.wantPrevOIDs)
				}
				if restore.PositionID != "pos-1" {
					t.Fatalf("queued restore-tp position = %q, want pos-1", restore.PositionID)
				}
			}

			out := res.uiMessage()
			if tc.wantOutPart != "" && !strings.Contains(out, tc.wantOutPart) {
				t.Fatalf("operator output = %q, want it to contain %q", out, tc.wantOutPart)
			}
			if tc.wantQuiet {
				backend.mu.Lock()
				sent := len(backend.messages) + len(backend.dms)
				backend.mu.Unlock()
				if sent != 0 || reads != 0 || updates != 0 {
					t.Fatalf("alerts=%d account reads=%d stop updates=%d, want none after a close that was never sent", sent, reads, updates)
				}
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
