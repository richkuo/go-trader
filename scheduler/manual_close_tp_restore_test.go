package main

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClassifyManualCloseTPTiers(t *testing.T) {
	requested := []int64{5150, 7001, 7002, 7003}
	cases := []struct {
		name      string
		tierCount int
		oids      []int64
		armed     []bool
		requested []int64
		result    *HyperliquidExecuteResult
		want      []manualCloseTPTierClass
	}{
		{
			name:      "a confirmed cancel is restorable and an unconfirmed one is verified first",
			tierCount: 3,
			oids:      []int64{7001, 7002, 7003},
			armed:     []bool{true, true, true},
			requested: requested,
			result:    &HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{5150, 7002}},
			want:      []manualCloseTPTierClass{manualCloseTPTierUnconfirmed, manualCloseTPTierCancelled, manualCloseTPTierUnconfirmed},
		},
		{
			name:      "a cleared armed tier is completed and an unplaced tier is untouched",
			tierCount: 3,
			oids:      []int64{0, 0, 7003},
			armed:     []bool{true, false, true},
			requested: requested,
			result:    &HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{7003}},
			want:      []manualCloseTPTierClass{manualCloseTPTierCompleted, manualCloseTPTierUntouched, manualCloseTPTierCancelled},
		},
		{
			name:      "a partial close requests no take-profit cancel",
			tierCount: 3,
			oids:      []int64{7001, 7002, 7003},
			armed:     []bool{true, true, true},
			requested: []int64{0},
			result:    &HyperliquidExecuteResult{Error: "rejected"},
			want:      []manualCloseTPTierClass{manualCloseTPTierUntouched, manualCloseTPTierUntouched, manualCloseTPTierUntouched},
		},
		{
			name:      "an unreadable subprocess outcome leaves every requested tier unconfirmed",
			tierCount: 2,
			oids:      []int64{7001, 7002},
			armed:     []bool{true, true},
			requested: requested,
			result:    nil,
			want:      []manualCloseTPTierClass{manualCloseTPTierUnconfirmed, manualCloseTPTierUnconfirmed},
		},
		{
			name:      "a tier count beyond the snapshot is untouched",
			tierCount: 3,
			oids:      []int64{7001},
			armed:     []bool{true},
			requested: requested,
			result:    &HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{7001}},
			want:      []manualCloseTPTierClass{manualCloseTPTierCancelled, manualCloseTPTierUntouched, manualCloseTPTierUntouched},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyManualCloseTPTiers(tc.tierCount, tc.oids, tc.armed, tc.requested, tc.result)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("classes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInterpretManualCloseTPRestore(t *testing.T) {
	classes := []manualCloseTPTierClass{manualCloseTPTierUnconfirmed, manualCloseTPTierCancelled, manualCloseTPTierCompleted}
	prev := []int64{7001, 7002, 0}
	cases := []struct {
		name      string
		result    *HyperliquidProtectionSyncResult
		syncErr   error
		wantKinds []manualCloseTPOutcomeKind
		wantOIDs  []int64
	}{
		{
			name:      "a resting previous order is preserved and a cancelled tier is replaced",
			result:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{7001, 9002, 0}, TPPxs: []float64{2050, 2100, 2150}},
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPPreserved, manualCloseTPRestored, manualCloseTPSkipped},
			wantOIDs:  []int64{7001, 9002, 0},
		},
		{
			name:      "an unreadable open-order book leaves the unconfirmed tier unverified",
			result:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{7001, 9002, 0}, OpenOrderCheckError: "userOpenOrders failed"},
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPUnverified, manualCloseTPRestored, manualCloseTPSkipped},
			wantOIDs:  []int64{7001, 9002, 0},
		},
		{
			name:      "a filled previous order and an immediately filled replacement both complete",
			result:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 0, 0}, TPFilledExternally: []bool{true, false, false}, TPFilledImmediately: []bool{false, true, false}},
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPFilledExternally, manualCloseTPFilledImmediately, manualCloseTPSkipped},
			wantOIDs:  []int64{0, 0, 0},
		},
		{
			name:      "a per-tier error is missing protection",
			result:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{7001, 0, 0}, TPErrors: []string{"", "SDK error: rejected", ""}},
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPPreserved, manualCloseTPMissing, manualCloseTPSkipped},
			wantOIDs:  []int64{7001, 0, 0},
		},
		{
			name:      "a tier that did not rest is missing protection",
			result:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{0, 0, 0}},
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPMissing, manualCloseTPMissing, manualCloseTPSkipped},
			wantOIDs:  []int64{0, 0, 0},
		},
		{
			name:      "a subprocess failure keeps an unconfirmed tier unverified and marks a cancelled tier missing",
			syncErr:   fmt.Errorf("script error"),
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPUnverified, manualCloseTPMissing, manualCloseTPSkipped},
			wantOIDs:  []int64{7001, 0, 0},
		},
		{
			name:      "a missing result keeps an unconfirmed tier unverified",
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPUnverified, manualCloseTPMissing, manualCloseTPSkipped},
			wantOIDs:  []int64{7001, 0, 0},
		},
		{
			name:      "a script-level error keeps an unconfirmed tier unverified and marks a cancelled tier missing",
			result:    &HyperliquidProtectionSyncResult{Error: "avg-cost and entry-atr must be > 0"},
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPUnverified, manualCloseTPMissing, manualCloseTPSkipped},
			wantOIDs:  []int64{7001, 0, 0},
		},
		{
			name:      "an unresolved placement outcome arms the tier and never re-places it",
			result:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{7001, 0, 0}, TPOutcomeUnknown: []bool{false, true, false}, TPErrors: []string{"", "connection refused", ""}},
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPPreserved, manualCloseTPOutcomeUnknown, manualCloseTPSkipped},
			wantOIDs:  []int64{7001, 0, 0},
		},
		{
			name:      "a size-skipped unconfirmed tier is unverified, never verified-resting",
			result:    &HyperliquidProtectionSyncResult{TPOIDs: []int64{7001, 9002, 0}, TPSizeSkipped: []bool{true, false, false}},
			wantKinds: []manualCloseTPOutcomeKind{manualCloseTPUnverified, manualCloseTPRestored, manualCloseTPSkipped},
			wantOIDs:  []int64{7001, 9002, 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := interpretManualCloseTPRestore(classes, prev, tc.result, tc.syncErr)
			if len(got) != len(classes) {
				t.Fatalf("outcomes = %d, want %d", len(got), len(classes))
			}
			for i, o := range got {
				if o.Kind != tc.wantKinds[i] {
					t.Fatalf("tier %d kind = %d, want %d", i+1, o.Kind, tc.wantKinds[i])
				}
				if o.NewOID != tc.wantOIDs[i] {
					t.Fatalf("tier %d new OID = %d, want %d", i+1, o.NewOID, tc.wantOIDs[i])
				}
				if o.PrevOID != prev[i] {
					t.Fatalf("tier %d prev OID = %d, want %d", i+1, o.PrevOID, prev[i])
				}
			}
		})
	}
}

func TestApplyRestoredTakeProfitTiers(t *testing.T) {
	cases := []struct {
		name      string
		pos       *Position
		outcomes  []manualCloseTPTierOutcome
		wantOIDs  []int64
		wantArmed []bool
	}{
		{
			name: "a restored tier is booked armed and a missing tier is cleared for the next sync",
			pos:  &Position{TPOIDs: []int64{0, 0, 0}, TPArmedTiers: []bool{true, false, false}},
			outcomes: []manualCloseTPTierOutcome{
				{Kind: manualCloseTPSkipped, PrevOID: 0, NewOID: 0},
				{Kind: manualCloseTPRestored, PrevOID: 7002, NewOID: 9002},
				{Kind: manualCloseTPMissing, PrevOID: 7003, NewOID: 0},
			},
			wantOIDs:  []int64{0, 9002, 0},
			wantArmed: []bool{true, true, false},
		},
		{
			name: "a filled tier stays completed and an unverified tier keeps its order id",
			pos:  &Position{TPOIDs: []int64{7001, 7002, 7003}, TPArmedTiers: []bool{true, true, true}},
			outcomes: []manualCloseTPTierOutcome{
				{Kind: manualCloseTPFilledExternally, PrevOID: 7001, NewOID: 0},
				{Kind: manualCloseTPFilledImmediately, PrevOID: 7002, NewOID: 0},
				{Kind: manualCloseTPUnverified, PrevOID: 7003, NewOID: 7003},
			},
			wantOIDs:  []int64{0, 0, 7003},
			wantArmed: []bool{true, true, true},
		},
		{
			name: "an unresolved placement arms the tier so the next sync places nothing",
			pos:  &Position{TPOIDs: []int64{0, 0}, TPArmedTiers: []bool{false, false}},
			outcomes: []manualCloseTPTierOutcome{
				{Kind: manualCloseTPOutcomeUnknown, PrevOID: 7001, NewOID: 0},
				{Kind: manualCloseTPSkipped, PrevOID: 0, NewOID: 0},
			},
			wantOIDs:  []int64{0, 0},
			wantArmed: []bool{true, false},
		},
		{
			name: "a preserved tier keeps the verified resting order",
			pos:  &Position{},
			outcomes: []manualCloseTPTierOutcome{
				{Kind: manualCloseTPPreserved, PrevOID: 7001, NewOID: 7001},
				{Kind: manualCloseTPSkipped, PrevOID: 0, NewOID: 0},
			},
			wantOIDs:  []int64{7001, 0},
			wantArmed: []bool{true, false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyRestoredTakeProfitTiers(tc.pos, tc.outcomes)
			if !reflect.DeepEqual(tc.pos.TPOIDs, tc.wantOIDs) {
				t.Fatalf("tp oids = %v, want %v", tc.pos.TPOIDs, tc.wantOIDs)
			}
			if !reflect.DeepEqual(tc.pos.TPArmedTiers, tc.wantArmed) {
				t.Fatalf("armed tiers = %v, want %v", tc.pos.TPArmedTiers, tc.wantArmed)
			}
		})
	}
}

func TestApplyManualActionRestoreTP(t *testing.T) {
	sc := StrategyConfig{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH"}
	scByID := map[string]StrategyConfig{sc.ID: sc}
	action := PendingManualAction{
		StrategyID: sc.ID, Action: "restore-tp", Symbol: "ETH", Side: "long",
		PositionID:   "pos-1",
		PrevTPOIDs:   []int64{7001, 7002, 7003},
		TPOIDs:       []int64{7001, 9002, 0},
		TPArmedTiers: []bool{true, true, false},
	}
	cases := []struct {
		name         string
		pos          *Position
		wantOIDs     []int64
		wantArmed    []bool
		wantCritical []string
	}{
		{
			name:      "a cleared book adopts the restored order ids",
			pos:       &Position{Symbol: "ETH", Quantity: 0.4, Side: "long", OwnerStrategyID: sc.ID, TradePositionID: "pos-1", TPOIDs: []int64{7001, 0, 0}, TPArmedTiers: []bool{true, false, false}},
			wantOIDs:  []int64{7001, 9002, 0},
			wantArmed: []bool{true, true, false},
		},
		{
			name:      "a repeated adoption changes nothing",
			pos:       &Position{Symbol: "ETH", Quantity: 0.4, Side: "long", OwnerStrategyID: sc.ID, TradePositionID: "pos-1", TPOIDs: []int64{7001, 9002, 0}, TPArmedTiers: []bool{true, true, false}},
			wantOIDs:  []int64{7001, 9002, 0},
			wantArmed: []bool{true, true, false},
		},
		{
			name:         "a third order id keeps memory and reports both ids",
			pos:          &Position{Symbol: "ETH", Quantity: 0.4, Side: "long", OwnerStrategyID: sc.ID, TradePositionID: "pos-1", TPOIDs: []int64{7001, 8888, 0}, TPArmedTiers: []bool{true, true, false}},
			wantOIDs:     []int64{7001, 8888, 0},
			wantArmed:    []bool{true, true, false},
			wantCritical: []string{"8888", "9002"},
		},
		{
			name:      "a replacement position is never touched",
			pos:       &Position{Symbol: "ETH", Quantity: 0.4, Side: "long", OwnerStrategyID: sc.ID, TradePositionID: "pos-2", TPOIDs: []int64{0, 0, 0}, TPArmedTiers: []bool{false, false, false}},
			wantOIDs:  []int64{0, 0, 0},
			wantArmed: []bool{false, false, false},
		},
		{
			name:         "a position owned by another strategy is acknowledged with a critical, never adopted",
			pos:          &Position{Symbol: "ETH", Quantity: 0.4, Side: "long", OwnerStrategyID: "other", TradePositionID: "pos-1", TPOIDs: []int64{4001}, TPArmedTiers: []bool{true}},
			wantOIDs:     []int64{4001},
			wantArmed:    []bool{true},
			wantCritical: []string{"9002", "other"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &AppState{Strategies: map[string]*StrategyState{
				sc.ID: {ID: sc.ID, Type: "manual", Platform: "hyperliquid", Positions: map[string]*Position{"ETH": tc.pos}},
			}}
			criticals, err := applyManualActionWithCriticals(state, nil, scByID, action)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			wantCount := 0
			if len(tc.wantCritical) > 0 {
				wantCount = 1
			}
			if len(criticals) != wantCount {
				t.Fatalf("criticals = %v, want %d", criticals, wantCount)
			}
			for _, c := range criticals {
				for _, want := range tc.wantCritical {
					if !strings.Contains(c, want) {
						t.Fatalf("critical = %q, want it to name %q", c, want)
					}
				}
			}
			if !reflect.DeepEqual(tc.pos.TPOIDs, tc.wantOIDs) {
				t.Fatalf("tp oids = %v, want %v", tc.pos.TPOIDs, tc.wantOIDs)
			}
			if !reflect.DeepEqual(tc.pos.TPArmedTiers, tc.wantArmed) {
				t.Fatalf("armed tiers = %v, want %v", tc.pos.TPArmedTiers, tc.wantArmed)
			}
		})
	}
}

func TestManualCloseTPRestoreSerialisesWithTheProtectionSync(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xoperator")

	sc := StrategyConfig{ID: "hl-manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH-LOCK",
		Script: "shared_scripts/check_hyperliquid.py",
		Args:   []string{"hold", "ETH-LOCK", "1h", "--mode=live"}, CloseStrategy: tieredTPCloseStrategy()}
	snap := manualCloseProtectionSnapshot{Symbol: "ETH-LOCK", Side: "long", Quantity: 0.4,
		TPOIDs: []int64{7001}, TPArmedTiers: []bool{true}}

	t.Run("the operator restore waits for an in-flight cycle sync", func(t *testing.T) {
		reached := make(chan struct{})
		d := manualCoreDeps{
			loadState: func(strategyID, symbol string) (manualStateView, error) {
				return manualStateView{}, fmt.Errorf("stub")
			},
			fetchPositions: func(addr string) ([]HLPosition, error) {
				close(reached)
				return nil, fmt.Errorf("stub")
			},
			syncProtection: func(sc StrategyConfig, plan hlProtectionPlan) (*HyperliquidProtectionSyncResult, string, error) {
				return nil, "", fmt.Errorf("stub")
			},
			recordRestoredTakeProfits: func(strategyID, symbol, side, positionID string, outcomes []manualCloseTPTierOutcome) error {
				return nil
			},
		}
		unlock := lockHyperliquidProtectionSync(snap.Symbol)
		done := make(chan struct{})
		go func() {
			defer close(done)
			restoreManualTakeProfitsAfterFailedClose(d, &manualCoreResult{}, sc, sc.ID, snap,
				&HyperliquidExecuteResult{Error: "rejected", CancelStopLossSucceededOIDs: []int64{7001}},
				[]int64{7001}, false)
		}()
		select {
		case <-reached:
			unlock()
			t.Fatal("the restore read the account while the per-symbol protection lock was held by a cycle sync")
		case <-time.After(200 * time.Millisecond):
		}
		unlock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the restore never ran after the per-symbol protection lock was released")
		}
	})

	t.Run("a cycle sync waits for an in-flight operator restore", func(t *testing.T) {
		stratState := &StrategyState{ID: sc.ID, Positions: map[string]*Position{
			"ETH-LOCK": {Symbol: "ETH-LOCK", Quantity: 0.4, AvgCost: 2000, EntryATR: 50, Side: "long"},
		}}
		placed := make(chan struct{})
		orig := syncHyperliquidProtection
		syncHyperliquidProtection = func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
			close(placed)
			return nil, false
		}
		t.Cleanup(func() { syncHyperliquidProtection = orig })

		unlock := lockHyperliquidProtectionSync("ETH-LOCK")
		done := make(chan struct{})
		go func() {
			defer close(done)
			var mu sync.RWMutex
			runHyperliquidProtectionSync(sc, stratState, nil, "ETH-LOCK", &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull)
		}()
		select {
		case <-placed:
			unlock()
			t.Fatal("the cycle sync placed protection while the operator restore held the per-symbol lock")
		case <-time.After(200 * time.Millisecond):
		}
		unlock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("the cycle sync never ran after the per-symbol protection lock was released")
		}
	})
}

func TestManualCloseRearmSerialisesWithProtectionSync(t *testing.T) {
	t.Setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "")
	for _, cycleFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("cycleFirst=%v", cycleFirst), func(t *testing.T) {
			sc := StrategyConfig{ID: "manual-lock", Type: "manual", Platform: "hyperliquid", Symbol: "ETH-REARM-LOCK", Args: []string{"--mode=live"}, CloseStrategy: tieredTPCloseStrategy()}
			snap := manualCloseProtectionSnapshot{Symbol: sc.Symbol, Side: "long", Quantity: 1, StopLossOID: 701, TriggerPx: 1900}
			firstEntered := make(chan struct{})
			secondEntered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			visit := func(first bool) {
				if first {
					close(firstEntered)
					<-release
				} else {
					close(secondEntered)
				}
			}
			withStubbedSyncHyperliquidProtection(t, func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
				visit(cycleFirst)
				return nil, false
			})
			d := manualCoreDeps{
				updateSL: func(string, string, string, float64, float64, int64) (*HyperliquidStopLossUpdateResult, string, error) {
					visit(!cycleFirst)
					return &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: 1900}, "", nil
				},
				recordRearmedStopLoss: func(string, string, string, float64, int64, *HyperliquidStopLossUpdateResult) error { return nil },
			}
			flat := make(chan bool, 1)
			rearm := func() {
				flat <- restoreManualStopLossAfterFailedClose(d, &manualCoreResult{}, sc, sc.ID, snap, &HyperliquidExecuteResult{CancelStopLossSucceededOIDs: []int64{701}}, []int64{701})
			}
			cycleDone := make(chan struct{})
			cycle := func() {
				defer close(cycleDone)
				var mu sync.RWMutex
				state := &StrategyState{Positions: map[string]*Position{sc.Symbol: {Symbol: sc.Symbol, Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 50}}}
				runHyperliquidProtectionSync(sc, state, nil, sc.Symbol, &mu, nil, nil, "test", nil, nil, nil, hlProtectionGuardFull)
			}
			if cycleFirst {
				go cycle()
			} else {
				go rearm()
			}
			select {
			case <-firstEntered:
			case <-time.After(2 * time.Second):
				t.Fatal("first operation did not start")
			}
			if cycleFirst {
				go rearm()
			} else {
				go cycle()
			}
			select {
			case <-secondEntered:
				t.Fatal("protection operations overlapped")
			case <-time.After(100 * time.Millisecond):
			}
			unblock()
			select {
			case got := <-flat:
				if !got {
					t.Fatal("immediate stop fill did not report flat")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("rearm did not finish")
			}
			select {
			case <-cycleDone:
			case <-time.After(2 * time.Second):
				t.Fatal("cycle did not finish")
			}
		})
	}
}
