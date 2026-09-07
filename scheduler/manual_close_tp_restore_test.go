package main

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func tieredTPCloseStrategy() *StrategyRef {
	return &StrategyRef{Name: "tiered_tp_atr", Params: map[string]interface{}{
		"tp_tiers": []interface{}{
			map[string]interface{}{"atr_multiple": 1.0, "close_fraction": 0.4},
			map[string]interface{}{"atr_multiple": 2.0, "close_fraction": 0.8},
			map[string]interface{}{"atr_multiple": 3.0, "close_fraction": 1.0},
		},
	}}
}

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

func TestManualCloseTPRestorePlanInputs(t *testing.T) {
	base := hlProtectionPlan{
		Symbol: "ETH", Side: "long", Size: 0.4, AvgCost: 2000, EntryATR: 50,
		StopLossATRMult: 1.5, StopLossOID: 5150, ForceSLReplace: true,
		ForceTPReplace: []bool{true, true, true}, CancelTPOIDs: []int64{8000},
		Tiers: []hlProtectionTier{{Multiple: 1, Fraction: 0.4}, {Multiple: 2, Fraction: 0.8}, {Multiple: 3, Fraction: 1}},
	}
	classes := []manualCloseTPTierClass{manualCloseTPTierUnconfirmed, manualCloseTPTierCancelled, manualCloseTPTierCompleted}
	got := manualCloseTPRestorePlanInputs(base, classes, []int64{7001, 7002, 0}, 0.3)

	if got.StopLossATRMult != 0 || got.StopLossOID != 0 || got.ForceSLReplace {
		t.Fatalf("stop-loss inputs = mult %v oid %d force %v, want the stop-loss leg disabled", got.StopLossATRMult, got.StopLossOID, got.ForceSLReplace)
	}
	if got.ForceTPReplace != nil || got.CancelTPOIDs != nil {
		t.Fatalf("force/cancel inputs = %v / %v, want none", got.ForceTPReplace, got.CancelTPOIDs)
	}
	if math.Abs(got.Size-0.3) > 1e-9 {
		t.Fatalf("size = %v, want the bounded 0.3", got.Size)
	}
	if !reflect.DeepEqual(got.TPOIDs, []int64{7001, 0, 0}) {
		t.Fatalf("tp oids = %v, want the unconfirmed tier verified and the rest zeroed", got.TPOIDs)
	}
	if !reflect.DeepEqual(got.TPArmedTiers, []bool{true, false, true}) {
		t.Fatalf("armed tiers = %v, want only the cancelled tier re-placed", got.TPArmedTiers)
	}
	if !reflect.DeepEqual(got.Tiers, base.Tiers) || got.AvgCost != base.AvgCost || got.EntryATR != base.EntryATR {
		t.Fatalf("plan geometry changed: %+v", got)
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

func TestDecideRestoredTPAdoption(t *testing.T) {
	cases := []struct {
		name       string
		prev, next int64
		curOID     int64
		curArmed   bool
		want       restoredTPAdoption
	}{
		{name: "an unchanged tier is never touched", prev: 7001, next: 7001, curOID: 8888, curArmed: true, want: restoredTPAdoptionNoChange},
		{name: "a cleared tier adopts the replacement", prev: 7002, next: 9002, curOID: 0, want: restoredTPAdoptionAdopt},
		{name: "a memory still holding the previous id adopts", prev: 7002, next: 0, curOID: 7002, curArmed: true, want: restoredTPAdoptionAdopt},
		{name: "a repeated adoption is idempotent", prev: 7002, next: 9002, curOID: 9002, curArmed: true, want: restoredTPAdoptionAlreadyApplied},
		{name: "a third order id is a conflict", prev: 7002, next: 9002, curOID: 8888, curArmed: true, want: restoredTPAdoptionConflict},
		{name: "an armed cleared tier that matches neither id is a conflict", prev: 7002, next: 9002, curOID: 0, curArmed: true, want: restoredTPAdoptionConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideRestoredTPAdoption(tc.prev, tc.next, tc.curOID, tc.curArmed); got != tc.want {
				t.Fatalf("adoption = %d, want %d", got, tc.want)
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
		name          string
		pos           *Position
		wantOIDs      []int64
		wantArmed     []bool
		wantConflicts int
		wantErr       bool
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
			name:          "a third order id keeps memory and reports both ids",
			pos:           &Position{Symbol: "ETH", Quantity: 0.4, Side: "long", OwnerStrategyID: sc.ID, TradePositionID: "pos-1", TPOIDs: []int64{7001, 8888, 0}, TPArmedTiers: []bool{true, true, false}},
			wantOIDs:      []int64{7001, 8888, 0},
			wantArmed:     []bool{true, true, false},
			wantConflicts: 1,
		},
		{
			name:      "a replacement position is never touched",
			pos:       &Position{Symbol: "ETH", Quantity: 0.4, Side: "long", OwnerStrategyID: sc.ID, TradePositionID: "pos-2", TPOIDs: []int64{0, 0, 0}, TPArmedTiers: []bool{false, false, false}},
			wantOIDs:  []int64{0, 0, 0},
			wantArmed: []bool{false, false, false},
		},
		{
			name:    "a position owned by another strategy errors",
			pos:     &Position{Symbol: "ETH", Quantity: 0.4, Side: "long", OwnerStrategyID: "other", TradePositionID: "pos-1"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &AppState{Strategies: map[string]*StrategyState{
				sc.ID: {ID: sc.ID, Type: "manual", Platform: "hyperliquid", Positions: map[string]*Position{"ETH": tc.pos}},
			}}
			criticals, err := applyManualActionWithCriticals(state, nil, scByID, action)
			if tc.wantErr {
				if err == nil {
					t.Fatal("apply returned no error, want the ownership refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if len(criticals) != tc.wantConflicts {
				t.Fatalf("criticals = %v, want %d", criticals, tc.wantConflicts)
			}
			for _, c := range criticals {
				if !strings.Contains(c, "8888") || !strings.Contains(c, "9002") {
					t.Fatalf("critical = %q, want it to name both order ids", c)
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

func TestRestoreTPActionSurvivesARestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("OpenStateDB: %v", err)
	}
	queued := PendingManualAction{
		StrategyID: "hl-manual-eth", Action: "restore-tp", Symbol: "ETH", Side: "long",
		PositionID:   "pos-1",
		PrevTPOIDs:   []int64{7001, 7002, 7003},
		TPOIDs:       []int64{7001, 9002, 0},
		TPArmedTiers: []bool{true, true, false},
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.InsertPendingManualAction(queued); err != nil {
		t.Fatalf("InsertPendingManualAction: %v", err)
	}
	db.Close()

	reopened, err := OpenStateDB(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	actions, err := reopened.LoadPendingManualActions()
	if err != nil {
		t.Fatalf("LoadPendingManualActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions = %+v, want one", actions)
	}
	got := actions[0]
	if got.Action != "restore-tp" || got.PositionID != "pos-1" {
		t.Fatalf("action = %q position %q, want restore-tp on pos-1", got.Action, got.PositionID)
	}
	if !reflect.DeepEqual(got.PrevTPOIDs, queued.PrevTPOIDs) || !reflect.DeepEqual(got.TPOIDs, queued.TPOIDs) ||
		!reflect.DeepEqual(got.TPArmedTiers, queued.TPArmedTiers) {
		t.Fatalf("round-trip = prev %v new %v armed %v, want prev %v new %v armed %v",
			got.PrevTPOIDs, got.TPOIDs, got.TPArmedTiers, queued.PrevTPOIDs, queued.TPOIDs, queued.TPArmedTiers)
	}
}

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

	rejected := func(succeeded ...int64) func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
		return func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error) {
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
		execute       func(script, symbol, side string, size, stopLossPct float64, cancelOID int64, prevPosQty float64, marginMode string, leverage float64, closeFullPosition bool, snapshot hlExecuteSnapshot, extraCancelOIDs ...int64) (*HyperliquidExecuteResult, string, error)
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
	}{
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
			d.fetchPositions = func(addr string) ([]HLPosition, error) { return tc.onChain, nil }
			d.execute = tc.execute
			d.updateSL = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
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

func TestDrainRestoreTPForClosedPosition(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			sc := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH"}
			store := singleFileStore(db)
			if err := store.InsertPendingManualAction(PendingManualAction{StrategyID: sc.ID, Symbol: sc.Symbol, Action: "restore-tp", PositionID: "closed", PrevTPOIDs: []int64{701}, TPOIDs: []int64{901}, TPArmedTiers: []bool{true}, CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if restart {
				db.Close()
				db, err = OpenStateDB(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				store = singleFileStore(db)
			}
			defer db.Close()
			state := &AppState{Strategies: map[string]*StrategyState{sc.ID: {ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 100, Positions: map[string]*Position{}}}}
			alerts, criticals := drainPendingManualActions(state, &Config{Strategies: []StrategyConfig{sc}}, store)
			if len(alerts) != 0 || len(criticals) != 0 {
				t.Fatalf("stale restore produced alerts: %v %v", alerts, criticals)
			}
			pending, err := store.LoadPendingManualActions()
			if err != nil || len(pending) != 0 {
				t.Fatalf("stale restore remains queued: %v %v", pending, err)
			}
			if len(state.Strategies[sc.ID].Positions) != 0 || state.Strategies[sc.ID].Cash != 100 || len(state.Strategies[sc.ID].TradeHistory) != 0 {
				t.Fatal("stale restore changed the closed book")
			}
		})
	}
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

func TestDrainRestoreTPTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		owner         string
		age           time.Duration
		wantCriticals int
		wantQueued    int
		wantAdopted   bool
	}{
		{name: "owned by a peer is acknowledged with a critical", owner: "peer", wantCriticals: 1},
		{name: "a fresh apply failure stays queued", owner: "peer", age: -time.Minute, wantCriticals: 1},
		{name: "an owned row still adopts", wantAdopted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sc := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH"}
			store := singleFileStore(db)
			if err := store.InsertPendingManualAction(PendingManualAction{StrategyID: sc.ID, Symbol: sc.Symbol, Action: "restore-tp", PrevTPOIDs: []int64{701}, TPOIDs: []int64{901}, TPArmedTiers: []bool{true}, CreatedAt: time.Now().UTC().Add(tc.age)}); err != nil {
				t.Fatal(err)
			}
			pos := &Position{Symbol: sc.Symbol, Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 50, OwnerStrategyID: tc.owner, TPOIDs: []int64{701}, TPArmedTiers: []bool{true}}
			state := &AppState{Strategies: map[string]*StrategyState{sc.ID: {ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Positions: map[string]*Position{sc.Symbol: pos}}}}
			_, criticals := drainPendingManualActions(state, &Config{Strategies: []StrategyConfig{sc}}, store)
			if len(criticals) != tc.wantCriticals {
				t.Fatalf("criticals=%v want %d", criticals, tc.wantCriticals)
			}
			pending, err := store.LoadPendingManualActions()
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != tc.wantQueued {
				t.Fatalf("queued rows=%d want %d", len(pending), tc.wantQueued)
			}
			if tc.wantAdopted && pos.TPOIDs[0] != 901 {
				t.Fatalf("the owned row did not adopt: %v", pos.TPOIDs)
			}
			if !tc.wantAdopted && pos.TPOIDs[0] != 701 {
				t.Fatalf("a row for another owner mutated the book: %v", pos.TPOIDs)
			}
		})
	}
}

func TestDrainAcknowledgesExpiredFailingAction(t *testing.T) {
	for _, tc := range []struct {
		name       string
		age        time.Duration
		wantQueued int
	}{
		{name: "an expired failing row is acknowledged", age: -2 * staleManualActionMaxAge},
		{name: "a fresh failing row is retried", age: -time.Minute, wantQueued: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			sc := StrategyConfig{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH"}
			store := singleFileStore(db)
			if err := store.InsertPendingManualAction(PendingManualAction{StrategyID: sc.ID, Symbol: sc.Symbol, Action: "cancel-sl", CreatedAt: time.Now().UTC().Add(tc.age)}); err != nil {
				t.Fatal(err)
			}
			state := &AppState{Strategies: map[string]*StrategyState{sc.ID: {ID: sc.ID, Type: sc.Type, Platform: sc.Platform, Cash: 100, Positions: map[string]*Position{}}}}
			_, criticals := drainPendingManualActions(state, &Config{Strategies: []StrategyConfig{sc}}, store)
			pending, err := store.LoadPendingManualActions()
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != tc.wantQueued {
				t.Fatalf("queued rows=%d want %d", len(pending), tc.wantQueued)
			}
			if wantCriticals := 1 - tc.wantQueued; len(criticals) != wantCriticals {
				t.Fatalf("criticals=%v want %d", criticals, wantCriticals)
			}
		})
	}
}

func TestSaveStrategyBookQueueingManualActionIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name       string
		action     PendingManualAction
		breakQueue bool
		wantErr    bool
		wantQueued int
		wantBook   bool
	}{
		{name: "book and queue commit together", action: PendingManualAction{StrategyID: "manual-eth", Symbol: "ETH", Action: "restore-tp", TPOIDs: []int64{901}}, wantQueued: 1, wantBook: true},
		{name: "a rejected queue write rolls the book back", action: PendingManualAction{StrategyID: "manual-eth", Symbol: "ETH", Action: "restore-tp", TPOIDs: []int64{901}}, breakQueue: true, wantErr: true},
		{name: "a queue row for another strategy is refused", action: PendingManualAction{StrategyID: "peer", Symbol: "ETH", Action: "restore-tp"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "state.db")
			db, err := OpenStateDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if tc.breakQueue {
				if _, err := db.db.Exec("DROP TABLE pending_manual_actions"); err != nil {
					t.Fatal(err)
				}
			}
			store := singleFileStore(db)
			strategy := &StrategyState{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: 1, AvgCost: 2000, EntryATR: 50, TPOIDs: []int64{901}, TPArmedTiers: []bool{true}},
			}}
			err = store.SaveStrategyBookQueueingManualAction(strategy, tc.action)
			if (err != nil) != tc.wantErr {
				t.Fatalf("save error=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.breakQueue {
				pending, loadErr := store.LoadPendingManualActions()
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if len(pending) != tc.wantQueued {
					t.Fatalf("queued rows=%d want %d", len(pending), tc.wantQueued)
				}
			}
			loaded, _, loadErr := LoadStateWithStore(&Config{Strategies: []StrategyConfig{{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "ETH"}}}, store)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			var pos *Position
			if ss := loaded.Strategies["manual-eth"]; ss != nil {
				pos = ss.Positions["ETH"]
			}
			if !tc.wantBook {
				if pos != nil {
					t.Fatalf("the book was persisted without its queue row: %+v", pos)
				}
				return
			}
			if pos == nil || pos.TPOIDs[0] != 901 {
				t.Fatalf("the book did not persist with its queue row: %+v", pos)
			}
		})
	}
}
