package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestApplyHyperliquidProtectionSyncRetainsOnZeroFields(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 11, TPOIDs: []int64{22, 33}}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		OpenOrderCheckError: "indexer down",
	}, nil)
	if pos.StopLossOID != 11 || !reflect.DeepEqual(pos.TPOIDs, []int64{22, 33}) {
		t.Errorf("zero-field result mutated OIDs: SL=%d TPs=%v, want 11/[22 33]", pos.StopLossOID, pos.TPOIDs)
	}
}

func TestApplyHyperliquidProtectionSyncClearsFilledExternally(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 11, TPOIDs: []int64{22, 33}}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		StopLossFilledExternally: true,
		TPFilledExternally:       []bool{true, false},
		TPOIDs:                   []int64{0, 33},
	}, nil)
	if pos.StopLossOID != 0 {
		t.Errorf("StopLossOID = %d, want 0 (cleared because filled externally)", pos.StopLossOID)
	}
	if !reflect.DeepEqual(pos.TPOIDs, []int64{0, 33}) {
		t.Errorf("TPOIDs = %v, want [0 33] (TP1 cleared because filled externally)", pos.TPOIDs)
	}
}

func TestApplySurplusTPCancelOutcome(t *testing.T) {
	t.Run("re-appends failed surplus OID", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPOIDs: []int64{10, 20}, TPArmedTiers: []bool{true, true}}
		applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
			TPOIDs:             []int64{10, 20},
			TPCancelFailedOIDs: []int64{303},
		}, []int64{303})
		if !reflect.DeepEqual(pos.TPOIDs, []int64{10, 20, 303}) {
			t.Errorf("TPOIDs = %v, want [10 20 303]", pos.TPOIDs)
		}
		if len(pos.TPArmedTiers) != 3 || !pos.TPArmedTiers[2] {
			t.Errorf("TPArmedTiers = %v, want third tier armed", pos.TPArmedTiers)
		}
	})

	t.Run("does not duplicate OID already present", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPOIDs: []int64{10, 20, 303}}
		applySurplusTPCancelOutcome(pos, &HyperliquidProtectionSyncResult{
			TPCancelFailedOIDs: []int64{303},
		}, []int64{303})
		if !reflect.DeepEqual(pos.TPOIDs, []int64{10, 20, 303}) {
			t.Errorf("TPOIDs = %v, want unchanged [10 20 303]", pos.TPOIDs)
		}
	})

	t.Run("clears successfully canceled surplus OID", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPOIDs: []int64{10, 20, 303}, TPArmedTiers: []bool{true, true, true}}
		applySurplusTPCancelOutcome(pos, &HyperliquidProtectionSyncResult{}, []int64{303})
		if !reflect.DeepEqual(pos.TPOIDs, []int64{10, 20, 0}) {
			t.Errorf("TPOIDs = %v, want [10 20 0]", pos.TPOIDs)
		}
		if !pos.TPArmedTiers[2] {
			t.Errorf("surplus slot should stay armed after clear")
		}
	})

	t.Run("clears filled surplus OID", func(t *testing.T) {
		pos := &Position{Symbol: "ETH", TPOIDs: []int64{10, 20, 303}, TPArmedTiers: []bool{true, true, true}}
		applySurplusTPCancelOutcome(pos, &HyperliquidProtectionSyncResult{
			TPCancelFilledOIDs: []int64{303},
		}, []int64{303})
		if !reflect.DeepEqual(pos.TPOIDs, []int64{10, 20, 0}) {
			t.Errorf("TPOIDs = %v, want [10 20 0]", pos.TPOIDs)
		}
	})
}

func withStubbedSyncHyperliquidProtection(
	t *testing.T,
	stub func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger, reconcileFillHintsJSON []byte) (*HyperliquidProtectionSyncResult, bool),
) {
	t.Helper()
	orig := syncHyperliquidProtection
	syncHyperliquidProtection = stub
	t.Cleanup(func() { syncHyperliquidProtection = orig })
}

func TestRunHyperliquidProtectionSyncSkipsApplyAfterExternalClose(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-manual-eth",
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
		StopLossATRMult: &mult,
	}
	state := &StrategyState{
		Positions: map[string]*Position{
			"ETH": {Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, EntryATR: 100, Side: "long"},
		},
	}
	withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		state.Positions["ETH"].Quantity = 0
		return &HyperliquidProtectionSyncResult{StopLossOID: 999, TPOIDs: []int64{111}}, true
	})

	var mu sync.RWMutex
	if syncedNeg, _ := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull, nil); syncedNeg {
		t.Fatal("expected apply to be skipped after position closed externally")
	}
	pos := state.Positions["ETH"]
	if pos.StopLossOID != 0 || len(pos.TPOIDs) != 0 {
		t.Errorf("OIDs leaked into closed position: sl=%d tp=%v", pos.StopLossOID, pos.TPOIDs)
	}
}

func TestApplyHyperliquidProtectionSyncClearsDeadSLOnCancelLandedPlaceFailed(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 5150, StopLossTriggerPx: 2325}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		CancelStopLossSucceeded: true,
		StopLossError:           "place_stop_loss SDK error: open order cap",
	}, nil)
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Errorf("SL = oid %d @ %g after cancel-landed/place-failed, want both cleared", pos.StopLossOID, pos.StopLossTriggerPx)
	}
}

func TestApplyHyperliquidProtectionSyncForceReplaceSuccessUnchanged(t *testing.T) {
	pos := &Position{Symbol: "ETH", StopLossOID: 5150, StopLossTriggerPx: 2325}
	applyHyperliquidProtectionSync(pos, &HyperliquidProtectionSyncResult{
		CancelStopLossSucceeded: true,
		StopLossOID:             6000,
		StopLossTriggerPx:       2300,
	}, nil)
	if pos.StopLossOID != 6000 || pos.StopLossTriggerPx != 2300 {
		t.Errorf("SL = oid %d @ %g, want 6000 @ 2300 from the successful replacement", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	pos2 := &Position{Symbol: "ETH", StopLossOID: 5150, StopLossTriggerPx: 2325}
	applyHyperliquidProtectionSync(pos2, &HyperliquidProtectionSyncResult{
		StopLossError: "force replace cancel: timeout",
	}, nil)
	if pos2.StopLossOID != 5150 || pos2.StopLossTriggerPx != 2325 {
		t.Errorf("failed-cancel result mutated SL to oid %d @ %g, want 5150 @ 2325 kept (order may still rest)", pos2.StopLossOID, pos2.StopLossTriggerPx)
	}
}

func TestRunHyperliquidProtectionSyncBooksFillAtSubmit(t *testing.T) {
	mult := 1.5
	sc := StrategyConfig{
		ID:              "hl-manual-eth",
		Type:            "manual",
		Platform:        "hyperliquid",
		CloseStrategy:   &StrategyRef{Name: "tiered_tp_atr_live"},
		StopLossATRMult: &mult,
	}
	pos := &Position{Symbol: "ETH", Quantity: 0.4, AvgCost: 3000, EntryATR: 100, Side: "long", StopLossOID: 4242, StopLossTriggerPx: 2325}
	state := &StrategyState{Positions: map[string]*Position{"ETH": pos}}
	withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, _ hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
		return &HyperliquidProtectionSyncResult{
			CancelStopLossSucceeded:   true,
			StopLossFilledImmediately: true,
			StopLossTriggerPx:         2318.5,
		}, true
	})
	var mu sync.RWMutex
	synced, fillPx := runHyperliquidProtectionSync(sc, state, nil, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull, nil)
	if !synced {
		t.Fatal("expected sync to report success after booking the submit-fill close")
	}
	if fillPx != 2318.5 {
		t.Errorf("fillPx = %g, want 2318.5 (the price that filled)", fillPx)
	}
	if _, stillOpen := state.Positions["ETH"]; stillOpen {
		t.Error("position must be gone after the submit-fill close is booked")
	}
}

func TestProtectionSyncOutcomeUnknownDefersInsteadOfClearing(t *testing.T) {
	newPos := func() *Position {
		return &Position{Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 25, StopLossOID: 111, StopLossTriggerPx: 1850}
	}

	unknown := &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOutcomeUnknown: true, StopLossError: "place_stop_loss returned no usable status"}
	if hlProtectionLostExchangeStop(unknown) {
		t.Errorf("outcome-unknown classified as protection lost — that CRITICAL would be false")
	}
	if !hlProtectionStopOutcomeUnknown(unknown) {
		t.Errorf("outcome-unknown not classified as such — the operator gets no alert at all")
	}
	pos := newPos()
	applyHyperliquidProtectionSync(pos, unknown, nil)
	if pos.StopLossOID != 111 || pos.StopLossTriggerPx != 1850 {
		t.Errorf("outcome-unknown cleared recorded state: OID %d trigger %.2f, want 111 / 1850", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	rejected := &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossError: "place_stop_loss SDK error: insufficient margin"}
	if !hlProtectionLostExchangeStop(rejected) {
		t.Errorf("a positively rejected placement must still read as protection lost")
	}
	if hlProtectionStopOutcomeUnknown(rejected) {
		t.Errorf("a positively rejected placement must not read as outcome unknown")
	}
	pos = newPos()
	applyHyperliquidProtectionSync(pos, rejected, nil)
	if pos.StopLossOID != 0 || pos.StopLossTriggerPx != 0 {
		t.Errorf("rejected placement left stale state: OID %d trigger %.2f, want 0 / 0", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	rested := &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOID: 222, StopLossTriggerPx: 1900}
	if hlProtectionLostExchangeStop(rested) || hlProtectionStopOutcomeUnknown(rested) {
		t.Errorf("a resting replacement must raise neither alert")
	}
	pos = newPos()
	applyHyperliquidProtectionSync(pos, rested, nil)
	if pos.StopLossOID != 222 || pos.StopLossTriggerPx != 1900 {
		t.Errorf("resting replacement not adopted: OID %d trigger %.2f, want 222 / 1900", pos.StopLossOID, pos.StopLossTriggerPx)
	}

	both := &HyperliquidProtectionSyncResult{CancelStopLossSucceeded: true, StopLossOID: 333, StopLossOutcomeUnknown: true}
	if hlProtectionLostExchangeStop(both) || hlProtectionStopOutcomeUnknown(both) {
		t.Errorf("a resolved placement must raise neither alert")
	}
}

func TestRunHyperliquidProtectionSyncManualActionGuard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		action   string
		strategy string
		symbol   string
		locked   bool
		broken   bool
		blocked  bool
	}{
		{name: "in flight command", locked: true, blocked: true},
		{name: "restored orders awaiting adoption", action: "restore-tp", blocked: true},
		{name: "open awaiting adoption", action: "open", blocked: true},
		{name: "add awaiting adoption", action: "add", blocked: true},
		{name: "close awaiting adoption", action: "close", blocked: true},
		{name: "stop edit awaiting adoption", action: "update-sl", blocked: true},
		{name: "stop cancel awaiting adoption", action: "cancel-sl", blocked: true},
		{name: "different strategy", action: "restore-tp", strategy: "peer"},
		{name: "different symbol", action: "restore-tp", symbol: "BTC"},
		{name: "unreadable queue", broken: true, blocked: true},
		{name: "no pending action"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			sc := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", CloseStrategy: tieredTPCloseStrategy()}
			if tc.action != "" {
				id, symbol := tc.strategy, tc.symbol
				if id == "" {
					id = sc.ID
				}
				if symbol == "" {
					symbol = "eth"
				}
				if err := singleFileStore(db).InsertPendingManualAction(PendingManualAction{StrategyID: id, Symbol: symbol, Action: tc.action, CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			db, err = OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if tc.broken {
				db.Close()
			}
			if tc.locked {
				unlock, err := acquireManualActionFileLock(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			clearHLProtectionGuardBlocks(sc.ID, "ETH")
			t.Cleanup(func() { clearHLProtectionGuardBlocks(sc.ID, "ETH") })
			calls := 0
			withStubbedSyncHyperliquidProtection(t, func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
				calls++
				unlock, err := acquireManualActionFileLockWithWait(dbPath, 0)
				if err == nil {
					unlock()
					t.Error("placement did not hold the manual-action file lock")
				}
				return &HyperliquidProtectionSyncResult{TPOIDs: []int64{901, 902, 903}}, true
			})
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long", TPOIDs: []int64{701, 702, 703}}
			state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
			var mu sync.RWMutex
			synced, _ := runHyperliquidProtectionSync(sc, state, db, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull, nil)
			if tc.blocked {
				if synced || calls != 0 || !reflect.DeepEqual(pos.TPOIDs, []int64{701, 702, 703}) {
					t.Fatalf("blocked sync mutated protection: synced=%v calls=%d oids=%v", synced, calls, pos.TPOIDs)
				}
			} else if !synced || calls != 1 || !reflect.DeepEqual(pos.TPOIDs, []int64{901, 902, 903}) {
				t.Fatalf("allowed sync failed: synced=%v calls=%d oids=%v", synced, calls, pos.TPOIDs)
			}
		})
	}
}

func TestApplyHyperliquidProtectionSyncImmediateTiers(t *testing.T) {
	for _, raw := range []string{
		`{"tp_oids":[0,702],"tp_filled_immediately":[true,false]}`,
		`{"tp_oids":[0,0],"tp_filled_immediately":[true,false],"tp_filled_externally":[false,true]}`,
		`{"tp2_oid":702,"tp_filled_immediately":[true,false]}`,
		`{"tp_filled_immediately":[true,false],"tp2_filled_externally":true}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var result HyperliquidProtectionSyncResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				t.Fatal(err)
			}
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long"}
			applyHyperliquidProtectionSync(pos, &result, nil)
			if len(pos.TPOIDs) != 2 || pos.TPOIDs[0] != 0 || !reflect.DeepEqual(pos.TPArmedTiers, []bool{true, true}) {
				t.Fatalf("completed tier lost: %+v", pos)
			}
			if pos.Quantity != 1 || pos.AvgCost != 2000 {
				t.Fatal("protection result booked an unconfirmed fill")
			}
			sc := StrategyConfig{Type: "manual", Platform: "hyperliquid", CloseStrategy: &StrategyRef{Name: "tiered_tp_atr"}}
			plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
			if !ok || plan.TPOIDs[0] != 0 || !plan.TPArmedTiers[0] {
				t.Fatalf("next cycle would replace the completed tier: %+v", plan)
			}
		})
	}
}

func TestRunHyperliquidProtectionSyncStopLegAfterFailedClose(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stopOwned bool
		wantCalls int
	}{
		{name: "protection sync owns the stop", stopOwned: true, wantCalls: 1},
		{name: "another owner holds the stop", wantCalls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sc := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", CloseStrategy: tieredTPCloseStrategy()}
			if tc.stopOwned {
				mult := 2.0
				sc.StopLossATRMult = &mult
			}
			if err := singleFileStore(db).InsertPendingManualAction(PendingManualAction{StrategyID: sc.ID, Symbol: "ETH", Action: "restore-tp", CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			clearHLProtectionGuardBlocks(sc.ID, "ETH")
			t.Cleanup(func() { clearHLProtectionGuardBlocks(sc.ID, "ETH") })
			calls := 0
			var seen hlProtectionPlan
			withStubbedSyncHyperliquidProtection(t, func(_ StrategyConfig, plan hlProtectionPlan, _ *MultiNotifier, _ *StrategyLogger, _ []byte) (*HyperliquidProtectionSyncResult, bool) {
				calls++
				seen = plan
				return &HyperliquidProtectionSyncResult{StopLossOID: 555, StopLossTriggerPx: 1900}, true
			})
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long", TPOIDs: []int64{701, 702, 703}, TPArmedTiers: []bool{true, true, true}}
			state := &StrategyState{ID: sc.ID, Positions: map[string]*Position{"ETH": pos}}
			var mu sync.RWMutex
			runHyperliquidProtectionSync(sc, state, db, "ETH", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardStopLegAfterFailedClose, nil)
			if calls != tc.wantCalls {
				t.Fatalf("placement calls=%d want %d", calls, tc.wantCalls)
			}
			if !reflect.DeepEqual(pos.TPOIDs, []int64{701, 702, 703}) {
				t.Fatalf("the gated take-profit tiers were rewritten: %v", pos.TPOIDs)
			}
			if tc.wantCalls == 0 {
				if pos.StopLossOID != 0 {
					t.Fatalf("a stop was placed for a strategy the sync does not own: %d", pos.StopLossOID)
				}
				return
			}
			if len(seen.Tiers) != 0 || len(seen.TPOIDs) != 0 || len(seen.TPArmedTiers) != 0 || len(seen.CancelTPOIDs) != 0 {
				t.Fatalf("the stop-leg plan carried take-profit inputs: %+v", seen)
			}
			if seen.StopLossATRMult <= 0 {
				t.Fatalf("the stop-leg plan carried no stop: %+v", seen)
			}
			if pos.StopLossOID != 555 || pos.StopLossTriggerPx != 1900 {
				t.Fatalf("the re-armed stop was not booked: %+v", pos)
			}
		})
	}
}

func TestApplyUnknownTPPlacementOutcome(t *testing.T) {
	for _, raw := range []string{
		`{"tp_oids":[0,802],"tp_outcome_unknown":[true,false],"tp_errors":["read timeout",""]}`,
		`{"tp_outcome_unknown":[true,false]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var result HyperliquidProtectionSyncResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				t.Fatal(err)
			}
			pos := &Position{Symbol: "ETH", Quantity: 1, AvgCost: 2000, EntryATR: 50, Side: "long"}
			applyHyperliquidProtectionSync(pos, &result, nil)
			if pos.TPOIDs[0] != 0 || !pos.TPArmedTiers[0] {
				t.Fatalf("an unresolved placement left the tier replaceable: oids=%v armed=%v", pos.TPOIDs, pos.TPArmedTiers)
			}
			sc := StrategyConfig{Type: "manual", Platform: "hyperliquid", CloseStrategy: tieredTPCloseStrategy()}
			plan, ok := buildHyperliquidProtectionPlan(sc, pos, 0)
			if !ok || plan.TPOIDs[0] != 0 || !plan.TPArmedTiers[0] {
				t.Fatalf("the next cycle would place a second order at the same price: %+v", plan)
			}
			if got := unknownTPPlacementTiers(&result); !reflect.DeepEqual(got, []int{1}) {
				t.Fatalf("the operator alert named tiers %v", got)
			}
		})
	}
}
