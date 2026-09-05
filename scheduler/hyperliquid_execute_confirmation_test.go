package main

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestConfirmHyperliquidExecuteFill(t *testing.T) {
	transportErr := errors.New("transport failed")
	parsed := &HyperliquidExecuteResult{
		CancelStopLossSucceeded:     true,
		CancelStopLossSucceededOIDs: []int64{111},
	}
	cases := []struct {
		name          string
		result        *HyperliquidExecuteResult
		err           error
		wantErr       bool
		wantResult    *HyperliquidExecuteResult
		wantConfirmed bool
		contains      string
	}{
		{name: "transport error retains parsed result", result: parsed, err: transportErr, wantErr: true, wantResult: parsed, contains: "transport failed"},
		{name: "nil result", wantErr: true, contains: "exchange returned no confirmed fill"},
		{name: "top level error", result: &HyperliquidExecuteResult{Error: "rejected"}, wantErr: true, contains: "rejected"},
		{name: "missing execution", result: &HyperliquidExecuteResult{}, wantErr: true, contains: "exchange returned no confirmed fill"},
		{name: "missing fill", result: &HyperliquidExecuteResult{Execution: &HyperliquidExecution{}}, wantErr: true, contains: "exchange returned no confirmed fill"},
		{name: "zero price", result: &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 0, TotalSz: 1}}}, wantErr: true, contains: "sz=1.00000000 px=0.00000000"},
		{name: "zero size", result: &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: 0}}}, wantErr: true, contains: "sz=0.00000000 px=2000.00000000"},
		{name: "nan price", result: &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: math.NaN(), TotalSz: 1}}}, wantErr: true, contains: "exchange returned no confirmed fill"},
		{name: "positive fill without oid", result: &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2000, TotalSz: 0.4}}}, wantConfirmed: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := confirmHyperliquidExecuteFill(tc.result, tc.err)
			if tc.wantConfirmed {
				if got == nil || got.Execution == nil || got.Execution.Fill == nil {
					t.Fatalf("result = %+v, want a confirmed fill", got)
				}
				if got.Execution.Fill.OID != 0 {
					t.Fatalf("OID = %d, want absent", got.Execution.Fill.OID)
				}
			}
			if tc.wantResult == parsed && got != parsed {
				t.Fatalf("result pointer = %p, want parsed result %p", got, parsed)
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("error = %v, want error=%t", err, tc.wantErr)
			}
			if tc.contains != "" && (err == nil || !strings.Contains(err.Error(), tc.contains)) {
				t.Fatalf("error = %v, want substring %q", err, tc.contains)
			}
		})
	}
}

func TestParseHyperliquidExecuteOutputRetainsParsedResultOnTransportError(t *testing.T) {
	stdout := []byte(`{"execution":null,"cancel_stop_loss_succeeded":true,"cancel_stop_loss_succeeded_oids":[111]}`)
	result, _, parseErr := parseHyperliquidExecuteOutput(stdout, "cancel warning", errors.New("exit status 1"))
	if parseErr == nil {
		t.Fatal("parse error = nil, want transport error")
	}
	if result == nil || !result.CancelStopLossSucceeded || !reflect.DeepEqual(result.CancelStopLossSucceededOIDs, []int64{111}) {
		t.Fatalf("parsed cancellation metadata = %+v, want retained metadata", result)
	}
	confirmed, confirmErr := confirmHyperliquidExecuteFill(result, parseErr)
	if confirmed != result || confirmErr == nil {
		t.Fatalf("confirmed result/error = %p/%v, want parsed result and error", confirmed, confirmErr)
	}
}

func TestHyperliquidExecuteCancellationClearsOnlyConfirmedProtection(t *testing.T) {
	result := &HyperliquidExecuteResult{
		CancelStopLossSucceeded:     true,
		CancelStopLossSucceededOIDs: []int64{111, 333, 333, 0},
		CancelStopLossFailedOIDs:    []int64{222},
	}
	requested := []int64{111, 222, 333}
	if got, want := hyperliquidExecuteSucceededCancelOIDs(result, requested), []int64{111, 333}; !reflect.DeepEqual(got, want) {
		t.Fatalf("confirmed cancellation OIDs = %v, want %v", got, want)
	}
	if got := hyperliquidExecuteSucceededCancelOIDs(result, nil); got != nil {
		t.Fatalf("unrequested cancellation OIDs = %v, want none", got)
	}

	pos := &Position{
		StopLossOID:       111,
		StopLossTriggerPx: 1900,
		TPOIDs:            []int64{222, 333},
		TPArmedTiers:      []bool{true, true},
	}
	clearHyperliquidProtectionOIDsMatching(pos, hyperliquidExecuteSucceededCancelOIDs(result, requested))
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Fatalf("stop-loss = %d @ %g, want cleared", pos.StopLossOID, pos.StopLossTriggerPx)
	}
	if !reflect.DeepEqual(pos.TPOIDs, []int64{222, 0}) || !reflect.DeepEqual(pos.TPArmedTiers, []bool{true, false}) {
		t.Fatalf("protection = oids %v armed %v, want only confirmed TP cancellation cleared", pos.TPOIDs, pos.TPArmedTiers)
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

func TestAutomaticHyperliquidExecuteRejectsUnconfirmedOpenCloseFlip(t *testing.T) {
	originalExecute := runHyperliquidExecuteFn
	originalThrottle := liveExecThrottle
	t.Cleanup(func() {
		runHyperliquidExecuteFn = originalExecute
		liveExecThrottle = originalThrottle
	})

	cases := []struct {
		name    string
		dir     string
		signal  int
		posQty  float64
		posSide string
	}{
		{name: "open", dir: DirectionLong, signal: 1},
		{name: "close", dir: DirectionLong, signal: -1, posQty: 0.5, posSide: "long"},
		{name: "flip", dir: DirectionBoth, signal: 1, posQty: 0.5, posSide: "short"},
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
			result := &HyperliquidResult{Symbol: "ETH", Signal: tc.signal, Price: 2000}
			got, ok := runHyperliquidExecuteOrder(sc, result, 2000, 1000, false, tc.posQty, tc.posSide, 2000, 2, 111, []int64{222}, nil, hlExecuteSnapshot{}, HurstGateDecision{}, notifier, silentStrategyLogger(sc.ID))
			if ok || got == nil {
				t.Fatalf("execute result = %+v, ok=%t, want rejected result", got, ok)
			}
			if called != 1 {
				t.Fatalf("execute calls = %d, want 1", called)
			}
			key := liveExecKey(sc.ID, sc.Platform, "ETH", directionOpen)
			if tc.signal == -1 {
				key = liveExecKey(sc.ID, sc.Platform, "ETH", directionClose)
			}
			liveExecThrottle.mu.Lock()
			entry := liveExecThrottle.entries[key]
			liveExecThrottle.mu.Unlock()
			if entry == nil || entry.count != 1 {
				t.Fatalf("throttle entry = %+v, want one retained failure", entry)
			}
			backend.mu.Lock()
			messages := len(backend.messages)
			dms := len(backend.dms)
			backend.mu.Unlock()
			if messages == 0 || dms == 0 {
				t.Fatalf("failure notifications = channels %d DMs %d, want both", messages, dms)
			}
		})
	}
}

func TestHyperliquidScaleInRejectsUnconfirmedBeforeBooking(t *testing.T) {
	originalExecute := runHyperliquidExecuteFn
	originalThrottle := liveExecThrottle
	t.Cleanup(func() {
		runHyperliquidExecuteFn = originalExecute
		liveExecThrottle = originalThrottle
	})
	runHyperliquidExecuteFn = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return unconfirmedExecuteResult(), "", nil
	}
	liveExecThrottle = &LiveExecFailureThrottle{}
	notifier, _ := confirmationNotifier()
	sc := confirmationTestStrategy(DirectionLong)
	result := &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 2000}
	got, ok := runHyperliquidScaleInOrder(sc, result, 0.1, hlExecuteSnapshot{}, notifier, silentStrategyLogger(sc.ID))
	if ok || got == nil {
		t.Fatalf("scale-in result = %+v, ok=%t, want rejected result", got, ok)
	}

	state := &StrategyState{ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 1000, Positions: map[string]*Position{
		"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 2000, Side: "long"},
	}}
	trades, detail, trade, alert := executeHyperliquidScaleInDeferredOpen(sc, state, result, unconfirmedExecuteResult(), "BUY", 2000, 0.1, silentStrategyLogger(sc.ID))
	if trades != 0 || detail != "" || trade != nil || alert != nil {
		t.Fatalf("unconfirmed scale-in booking = trades %d detail %q trade %+v alert %+v", trades, detail, trade, alert)
	}
	if state.Positions["ETH"].Quantity != 0.4 || state.Cash != 1000 {
		t.Fatalf("state changed after unconfirmed scale-in: position=%+v cash=%g", state.Positions["ETH"], state.Cash)
	}
}

func TestHyperliquidResultRejectsUnconfirmedBeforeBooking(t *testing.T) {
	sc := confirmationTestStrategy(DirectionLong)
	state := &StrategyState{ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 1000, Positions: map[string]*Position{}, TradeHistory: []Trade{{StrategyID: sc.ID, Symbol: "ETH", Quantity: 0.2, Price: 2000}}}
	result := &HyperliquidResult{Symbol: "ETH", Signal: 1, Price: 2000}
	trades, detail, trade, alert := executeHyperliquidResultDeferredOpen(sc, state, result, unconfirmedExecuteResult(), "BUY", 2000, nil, &Config{}, HurstGateDecision{}, silentStrategyLogger(sc.ID))
	if trades != 0 || detail != "" || trade != nil || alert != nil {
		t.Fatalf("unconfirmed result booking = trades %d detail %q trade %+v alert %+v", trades, detail, trade, alert)
	}
	if state.Cash != 1000 || len(state.TradeHistory) != 1 || len(state.Positions) != 0 {
		t.Fatalf("state changed after unconfirmed result: cash=%g trades=%d positions=%v", state.Cash, len(state.TradeHistory), state.Positions)
	}
}

func TestHedgeRejectsUnconfirmedOpenAndAdd(t *testing.T) {
	prev := tradeRecorder
	tradeRecorder = nil
	t.Cleanup(func() { tradeRecorder = prev })

	cases := []struct {
		name       string
		primaryQty float64
		hedgeQty   float64
		freshQty   float64
	}{
		{name: "open", primaryQty: 10, freshQty: 10},
		{name: "add", primaryQty: 15, hedgeQty: 0.4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := hedgeTestConfig()
			s := hedgeTestState("eth-long")
			s.Positions["ETH"] = primaryPos(tc.primaryQty, "long")
			if tc.hedgeQty > 0 {
				s.Positions["BTC"] = hedgePos(tc.hedgeQty, "short", 10)
			}
			var mu sync.RWMutex
			f := &fakeHedgeExec{openResult: unconfirmedExecuteResult()}

			if kind := runHedgeSync(sc, s, &mu, f.executor(), hedgeSyncInputs{
				PrimaryPx: testPrimaryPx, HedgePx: testHedgePx, FreshExposureQty: tc.freshQty, Live: true,
			}, nil, silentStrategyLogger("eth-long")); kind != hedgeActionNone {
				t.Fatalf("action = %v, want none", kind)
			}
			if tc.hedgeQty == 0 {
				if _, ok := s.Positions["BTC"]; ok {
					t.Fatal("unconfirmed hedge open created a position")
				}
			} else if got := s.Positions["BTC"].Quantity; got != tc.hedgeQty {
				t.Fatalf("hedge quantity = %g, want %g", got, tc.hedgeQty)
			}
		})
	}
}

func TestManualLiveExecuteRejectsUnconfirmedWithoutQueueing(t *testing.T) {
	cases := []string{"open", "add", "close"}
	for _, action := range cases {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatalf("OpenStateDB: %v", err)
			}
			defer db.Close()
			strategyID := "hl-manual-confirmation"
			sc := StrategyConfig{
				ID: strategyID, Type: "manual", Platform: "hyperliquid", Symbol: "ETH",
				Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "1h", "--mode=live"}, Leverage: 2,
			}
			position := &Position{
				Symbol: "ETH", Quantity: 0.4, InitialQuantity: 0.4, AvgCost: 2000, Side: "long",
				OwnerStrategyID: strategyID, StopLossOID: 111, StopLossTriggerPx: 1900,
				TPOIDs: []int64{222, 333}, TPArmedTiers: []bool{true, true},
			}
			strategyState := &StrategyState{
				ID: strategyID, Type: "manual", Platform: "hyperliquid", Cash: 1000,
				Positions: map[string]*Position{}, TradeHistory: []Trade{{StrategyID: strategyID, Symbol: "ETH", Quantity: 0.2, Price: 2000}},
			}
			if action != "open" {
				strategyState.Positions["ETH"] = position
			}
			state := &AppState{Strategies: map[string]*StrategyState{strategyID: strategyState}}
			if err := db.SaveState(state); err != nil {
				t.Fatalf("SaveState: %v", err)
			}
			cfg := &Config{DBFile: dbPath, Strategies: []StrategyConfig{sc}}
			deps := newCLIManualCoreDeps(cfg, openTestStore(t, db), nil)
			deps.fetchMids = func([]string) (map[string]float64, error) { return map[string]float64{"ETH": 2000}, nil }
			deps.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
				result := unconfirmedExecuteResult()
				if action == "close" {
					result.CancelStopLossSucceeded = true
					result.CancelStopLossSucceededOIDs = []int64{111, 333}
				}
				return result, "", nil
			}

			var result *manualCoreResult
			switch action {
			case "open":
				result, err = manualOpenCore(deps, sc, manualOpenInputs{StrategyID: strategyID, Margin: 50})
			case "add":
				result, err = manualAddCore(deps, sc, manualAddInputs{StrategyID: strategyID, Margin: 50})
			case "close":
				result, err = manualCloseCore(deps, sc, manualCloseInputs{StrategyID: strategyID})
			}
			if err == nil || result == nil || result.queued || !strings.Contains(err.Error(), "exchange returned no confirmed fill") {
				t.Fatalf("result=%+v err=%v, want an unconfirmed-fill refusal without queue", result, err)
			}
			actions, err := db.LoadPendingManualActions()
			if err != nil {
				t.Fatalf("LoadPendingManualActions: %v", err)
			}
			if len(actions) != 0 {
				t.Fatalf("pending actions = %+v, want none", actions)
			}
			loaded, err := LoadStateWithDB(cfg, db)
			if err != nil {
				t.Fatalf("LoadStateWithDB: %v", err)
			}
			loadedStrategy := loaded.Strategies[strategyID]
			if loadedStrategy.Cash != 1000 || len(loadedStrategy.TradeHistory) != 1 {
				t.Fatalf("accounting changed: cash=%g trade history=%d", loadedStrategy.Cash, len(loadedStrategy.TradeHistory))
			}
			if action == "open" {
				if loadedStrategy.Positions["ETH"] != nil {
					t.Fatal("unconfirmed open created a position")
				}
				return
			}
			loadedPosition := loadedStrategy.Positions["ETH"]
			if loadedPosition == nil || loadedPosition.Quantity != 0.4 {
				t.Fatalf("position = %+v, want unchanged quantity", loadedPosition)
			}
			if action == "close" {
				if loadedPosition.StopLossOID != 0 || loadedPosition.StopLossTriggerPx != 0 || !reflect.DeepEqual(loadedPosition.TPOIDs, []int64{222, 0}) || !reflect.DeepEqual(loadedPosition.TPArmedTiers, []bool{true, false}) {
					t.Fatalf("recorded protection = sl %d @ %g tp %v armed %v, want canceled OIDs cleared", loadedPosition.StopLossOID, loadedPosition.StopLossTriggerPx, loadedPosition.TPOIDs, loadedPosition.TPArmedTiers)
				}
			}
		})
	}
}

func TestDaemonManualCloseRejectsUnconfirmedWithoutQueueing(t *testing.T) {
	ss, db, _ := newTradeActionTestServer(t)
	stubs := stubTradeDeps(t, ss)
	stubs.execute = func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return &HyperliquidExecuteResult{
			Execution:                   &HyperliquidExecution{Fill: &HyperliquidFill{}},
			CancelStopLossSucceeded:     true,
			CancelStopLossSucceededOIDs: []int64{111},
		}, "", nil
	}
	nonce := confirmNonceFor(t, ss, "close", "hl-manual-eth", `{}`)
	w := tradeActionPost(ss, "/api/strategies/hl-manual-eth/close", fmt.Sprintf(`{"nonce":%q,"params":{}}`, nonce), nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "exchange returned no confirmed fill") {
		t.Fatalf("daemon close status = %d, body %s", w.Code, w.Body.String())
	}
	actions, err := db.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("pending actions = %+v, want none", actions)
	}
	position := ss.state.Strategies["hl-manual-eth"].Positions["ETH"]
	if position.Quantity != 0.4 || position.StopLossOID != 0 || position.StopLossTriggerPx != 0 {
		t.Fatalf("daemon state = %+v, want quantity unchanged and canceled SL cleared", position)
	}
}
