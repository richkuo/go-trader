package main

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func sharedCloseFloorPeers() []StrategyConfig {
	return []StrategyConfig{
		confirmationTestStrategy(DirectionLong),
		{ID: "hl-peer", Type: "perps", Platform: "hyperliquid", Symbol: "ETH", Script: "shared_scripts/check_hyperliquid.py", Args: []string{"hold", "ETH", "1h", "--mode=live"}},
	}
}

func TestRearmTrailingStopAfterFailedCloseKeepsRatchetAndClamp(t *testing.T) {
	oldUpdate := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = oldUpdate })
	trail := 2.0
	liveArgs := []string{"x.py", "ETH", "1h", "--mode=live"}
	sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: liveArgs, TrailingStopATRMult: &trail}
	cases := []struct {
		name          string
		bookOID       int64
		bookTrigger   float64
		bookHighWater float64
		prevOID       int64
		prevTrigger   float64
		prevHighWater float64
		onChainQty    float64
		liqPx         float64
		wantCancel    int64
		wantSize      float64
		wantTrigger   float64
		wantHighWater float64
	}{
		{name: "readable cancel cleared the book: old oid is the cancel oid and the ratcheted high-water anchors the trigger", bookHighWater: 2200, prevOID: 444, prevTrigger: 2090, prevHighWater: 2200, onChainQty: 0.002, wantCancel: 444, wantSize: 0.002, wantTrigger: 2090, wantHighWater: 2200},
		{name: "unreadable result keeps the book oid and hands it to the update script as the cancel oid", bookOID: 444, bookTrigger: 2090, bookHighWater: 2200, prevOID: 444, prevTrigger: 2090, prevHighWater: 2200, onChainQty: 0.002, wantCancel: 444, wantSize: 0.002, wantTrigger: 2090, wantHighWater: 2200},
		{name: "virtual above on-chain arms at the capped size instead of skipping", bookHighWater: 2200, prevOID: 444, prevHighWater: 2200, onChainQty: 0.001, wantCancel: 444, wantSize: 0.001, wantTrigger: 2090, wantHighWater: 2200},
		{name: "trigger past the liquidation price is clamped inside it", bookHighWater: 2200, prevOID: 444, prevHighWater: 2200, onChainQty: 0.002, liqPx: 2095, wantCancel: 444, wantSize: 0.002, wantTrigger: 2095 * (1 + hlLiquidationStopBufferPct/100), wantHighWater: 2200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotCancel int64
			var gotSize, gotTrigger float64
			placed := 0
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				placed++
				gotCancel, gotSize, gotTrigger = cancelStopLossOID, size, triggerPx
				return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: cancelStopLossOID > 0}, "", nil
			}
			st := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: 0.002, InitialQuantity: 0.002, AvgCost: 2000, EntryATR: 50, RiskAnchorPrice: 2000, StopLossOID: tc.bookOID, StopLossTriggerPx: tc.bookTrigger, StopLossHighWaterPx: tc.bookHighWater},
			}}
			var liq map[string]float64
			var net map[string]string
			if tc.liqPx > 0 {
				liq = map[string]float64{"ETH": tc.liqPx}
				net = map[string]string{"ETH": "long"}
			}
			var mu sync.RWMutex
			stop := hlCloseRemainderStop{Remainder: 0.002, Qty: math.Min(0.002, tc.onChainQty), Basis: hlRemainderBasisFresh}
			rearmProtectionForCloseRemainder(sc, st, nil, "ETH", 2100, tc.prevOID, tc.prevTrigger, tc.prevHighWater, nil, liq, net, hlCloseUnconfirmed{}, stop, &mu, nil, newTestLogger(t))
			if placed != 1 {
				t.Fatalf("stop placements = %d, want 1", placed)
			}
			if gotCancel != tc.wantCancel || math.Abs(gotSize-tc.wantSize) > 1e-9 || math.Abs(gotTrigger-tc.wantTrigger) > 1e-6 {
				t.Fatalf("placed cancel=%d size=%g trigger=%g, want cancel=%d size=%g trigger=%g", gotCancel, gotSize, gotTrigger, tc.wantCancel, tc.wantSize, tc.wantTrigger)
			}
			pos := st.Positions["ETH"]
			if pos.StopLossOID != 999 || math.Abs(pos.StopLossTriggerPx-tc.wantTrigger) > 1e-6 || math.Abs(pos.StopLossHighWaterPx-tc.wantHighWater) > 1e-9 {
				t.Fatalf("book after re-arm oid=%d trigger=%g high_water=%g, want oid 999 trigger %g high_water %g", pos.StopLossOID, pos.StopLossTriggerPx, pos.StopLossHighWaterPx, tc.wantTrigger, tc.wantHighWater)
			}
		})
	}
}

func TestRearmProtectionAfterFailedCloseCoversPercentageStopOwners(t *testing.T) {
	oldUpdate := runHyperliquidUpdateStopLossFunc
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc = oldUpdate })
	liveArgs := []string{"x.py", "ETH", "1h", "--mode=live"}
	pct := 5.0
	marginPct := 20.0
	trailPct := 3.0
	base := func(mut func(*StrategyConfig)) StrategyConfig {
		sc := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: liveArgs}
		mut(&sc)
		return sc
	}
	cases := []struct {
		name        string
		sc          StrategyConfig
		bookOID     int64
		bookTrigger float64
		prevOID     int64
		liqPx       float64
		wantPlaced  int
		wantCancel  int64
		wantTrigger float64
	}{
		{name: "stop_loss_pct owner whose cancel landed is re-armed at the anchor-scaled trigger", sc: base(func(sc *StrategyConfig) { sc.StopLossPct = &pct }), prevOID: 444, wantPlaced: 1, wantCancel: 444, wantTrigger: 1900},
		{name: "stop_loss_margin_pct owner with an unreadable result hands the still-recorded oid to the update script", sc: base(func(sc *StrategyConfig) { sc.StopLossMarginPct = &marginPct; sc.Leverage = 4 }), bookOID: 444, bookTrigger: 1900, prevOID: 444, wantPlaced: 1, wantCancel: 444, wantTrigger: 1900},
		{name: "max_drawdown_pct fallback owner is re-armed", sc: base(func(sc *StrategyConfig) { sc.MaxDrawdownPct = 5 }), prevOID: 444, wantPlaced: 1, wantCancel: 444, wantTrigger: 1900},
		{name: "percentage trigger past the liquidation price is clamped inside it", sc: base(func(sc *StrategyConfig) { sc.StopLossPct = &pct }), prevOID: 444, liqPx: 1950, wantPlaced: 1, wantCancel: 444, wantTrigger: 1950 * (1 + hlLiquidationStopBufferPct/100)},
		{name: "trailing_stop_pct owner is armed once by the trailing arm and never double-armed", sc: base(func(sc *StrategyConfig) { sc.TrailingStopPct = &trailPct }), prevOID: 444, wantPlaced: 1, wantCancel: 444, wantTrigger: 2000 * (1 - trailPct/100)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotCancel int64
			var gotTrigger float64
			placed := 0
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelStopLossOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				placed++
				gotCancel, gotTrigger = cancelStopLossOID, triggerPx
				return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: cancelStopLossOID > 0}, "", nil
			}
			st := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: 0.002, InitialQuantity: 0.002, AvgCost: 2000, EntryATR: 50, RiskAnchorPrice: 2000, StopLossOID: tc.bookOID, StopLossTriggerPx: tc.bookTrigger, StopLossHighWaterPx: 2000},
			}}
			var liq map[string]float64
			var net map[string]string
			if tc.liqPx > 0 {
				liq = map[string]float64{"ETH": tc.liqPx}
				net = map[string]string{"ETH": "long"}
			}
			var mu sync.RWMutex
			rearmProtectionForCloseRemainder(tc.sc, st, nil, "ETH", 2000, tc.prevOID, tc.bookTrigger, 2000, nil, liq, net, hlCloseUnconfirmed{}, hlCloseRemainderStop{Remainder: 0.002, Qty: 0.002, Basis: hlRemainderBasisFresh}, &mu, nil, newTestLogger(t))
			if placed != tc.wantPlaced {
				t.Fatalf("stop placements = %d, want %d", placed, tc.wantPlaced)
			}
			if gotCancel != tc.wantCancel || math.Abs(gotTrigger-tc.wantTrigger) > 1e-6 {
				t.Fatalf("placed cancel=%d trigger=%g, want cancel=%d trigger=%g", gotCancel, gotTrigger, tc.wantCancel, tc.wantTrigger)
			}
			if pos := st.Positions["ETH"]; pos.StopLossOID != 999 || math.Abs(pos.StopLossTriggerPx-tc.wantTrigger) > 1e-6 {
				t.Fatalf("book after re-arm oid=%d trigger=%g, want oid 999 trigger %g", pos.StopLossOID, pos.StopLossTriggerPx, tc.wantTrigger)
			}
		})
	}
}

func TestRearmProtectionForCloseRemainderResizesThePreCloseStop(t *testing.T) {
	oldUpdate, oldSync := runHyperliquidUpdateStopLossFunc, syncHyperliquidProtection
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc, syncHyperliquidProtection = oldUpdate, oldSync })
	live := []string{"x.py", "ETH", "1h", "--mode=live"}
	atrMult, pct, trailMult := 2.0, 5.0, 2.0
	atr := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: live, StopLossATRMult: &atrMult}
	pctOwner := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: live, StopLossPct: &pct}
	trailOwner := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: live, TrailingStopATRMult: &trailMult}
	cases := []struct {
		name        string
		sc          StrategyConfig
		bookQty     float64
		bookOID     int64
		remainder   float64
		qty         float64
		slResult    *HyperliquidStopLossUpdateResult
		wantSync    bool
		wantUpdate  bool
		wantSize    float64
		wantForce   bool
		wantBookOID int64
	}{
		{name: "a pre-close stop whose cancel failed is force-replaced at the booked remainder", sc: atr, bookQty: 6, bookOID: 111, remainder: 6, qty: 6, wantSync: true, wantSize: 6, wantForce: true, wantBookOID: 999},
		{name: "a stop already placed after the close is kept without a replace", sc: atr, bookQty: 6, bookOID: 555, remainder: 6, qty: 6, wantSync: true, wantSize: 6, wantBookOID: 999},
		{name: "a pre-drain book is sized down to the remainder", sc: atr, bookQty: 10, remainder: 6, qty: 6, wantSync: true, wantSize: 6, wantBookOID: 999},
		{name: "a percentage owner re-arms at the remainder", sc: pctOwner, bookQty: 10, remainder: 6, qty: 6, wantUpdate: true, wantSize: 6, wantBookOID: 999},
		{name: "the ATR arm places the owner size below the remainder", sc: atr, bookQty: 6, bookOID: 111, remainder: 6, qty: 4, wantSync: true, wantSize: 4, wantForce: true, wantBookOID: 999},
		{name: "the percentage arm places the owner size below the remainder", sc: pctOwner, bookQty: 10, remainder: 6, qty: 4, wantUpdate: true, wantSize: 4, wantBookOID: 999},
		{name: "the trailing arm places the owner size below the remainder", sc: trailOwner, bookQty: 10, remainder: 6, qty: 4, wantUpdate: true, wantSize: 4, wantBookOID: 999},
		{name: "a rejected trailing cancel sends no alert from the arm", sc: trailOwner, bookQty: 6, bookOID: 111, remainder: 6, qty: 6, slResult: &HyperliquidStopLossUpdateResult{CancelStopLossError: "111 still resting"}, wantUpdate: true, wantSize: 6, wantBookOID: 111},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sizes []float64
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				sizes = append(sizes, size)
				if tc.slResult != nil {
					return tc.slResult, "", nil
				}
				return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx}, "", nil
			}
			var plans []hlProtectionPlan
			syncHyperliquidProtection = func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger, hints []byte) (*HyperliquidProtectionSyncResult, bool) {
				plans = append(plans, plan)
				return &HyperliquidProtectionSyncResult{StopLossOID: 999, StopLossTriggerPx: 1900}, true
			}
			st := &StrategyState{ID: "hl-eth", Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: tc.bookQty, InitialQuantity: 10, AvgCost: 2000, RiskAnchorPrice: 2000, EntryATR: 50, StopLossOID: tc.bookOID, StopLossTriggerPx: 1900, StopLossHighWaterPx: 2000},
			}}
			notifier, backend := confirmationNotifier()
			var mu sync.RWMutex
			rearmProtectionForCloseRemainder(tc.sc, st, nil, "ETH", 2000, 111, 1900, 2000, nil, nil, nil, hlCloseUnconfirmed{}, hlCloseRemainderStop{Remainder: tc.remainder, Qty: tc.qty, Basis: hlRemainderBasisFresh, AfterFill: true}, &mu, notifier, newTestLogger(t))
			if tc.wantSync != (len(plans) == 1) || tc.wantUpdate != (len(sizes) == 1) || len(plans)+len(sizes) != 1 {
				t.Fatalf("syncs=%d updates=%d, want sync %t update %t", len(plans), len(sizes), tc.wantSync, tc.wantUpdate)
			}
			if tc.wantSync && (math.Abs(plans[0].Size-tc.wantSize) > 1e-9 || plans[0].ForceSLReplace != tc.wantForce) {
				t.Fatalf("plan size=%g force=%t, want size %g force %t", plans[0].Size, plans[0].ForceSLReplace, tc.wantSize, tc.wantForce)
			}
			if tc.wantUpdate && math.Abs(sizes[0]-tc.wantSize) > 1e-9 {
				t.Fatalf("update size = %g, want %g", sizes[0], tc.wantSize)
			}
			if st.Positions["ETH"].StopLossOID != tc.wantBookOID {
				t.Fatalf("book stop oid = %d, want %d", st.Positions["ETH"].StopLossOID, tc.wantBookOID)
			}
			backend.mu.Lock()
			sent := len(backend.messages) + len(backend.dms)
			backend.mu.Unlock()
			if sent != 0 {
				t.Fatalf("the arm sent %d alerts, want none: the re-arm report is the single alert", sent)
			}
		})
	}
}

func TestRearmProtectionForCloseRemainderRemovesUnconfirmedOrders(t *testing.T) {
	oldUpdate, oldSync := runHyperliquidUpdateStopLossFunc, syncHyperliquidProtection
	t.Cleanup(func() { runHyperliquidUpdateStopLossFunc, syncHyperliquidProtection = oldUpdate, oldSync })
	live := []string{"x.py", "ETH", "1h", "--mode=live"}
	atrMult := 2.0
	tiered := StrategyConfig{ID: "hl-eth", Type: "perps", Platform: "hyperliquid", Script: "x.py", Args: live, StopLossATRMult: &atrMult, CloseStrategy: &StrategyRef{Name: "tiered_tp_atr_live"}}
	manualTiered := tiered
	manualTiered.Type = "manual"
	unbacked := hlCloseRemainderStop{Remainder: 3, Basis: hlRemainderBasisFresh, Unbacked: true, AfterFill: true}
	backed := hlCloseRemainderStop{Remainder: 3, Qty: 3, Basis: hlRemainderBasisFresh, AfterFill: true}
	cancelOnly := func(r HyperliquidStopLossUpdateResult) *HyperliquidStopLossUpdateResult {
		r.CancelOnly = true
		return &r
	}
	cases := []struct {
		name           string
		sc             StrategyConfig
		queued         bool
		stop           hlCloseRemainderStop
		u              hlCloseUnconfirmed
		slResult       *HyperliquidStopLossUpdateResult
		syncResult     *HyperliquidProtectionSyncResult
		wantUpdates    int
		wantSyncs      int
		wantPlanCancel []int64
		wantForceTP    []bool
		wantBookSL     int64
		wantBookTP     []int64
		wantBookArmed  []bool
		wantRemoved    []int64
		wantFilled     []int64
		wantResting    []int64
		wantUnverified []int64
	}{
		{name: "Q=0 cancels the unconfirmed pre-close stop", sc: tiered, stop: unbacked, u: hlCloseUnconfirmed{StopOID: 111}, slResult: cancelOnly(HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}), wantUpdates: 1, wantBookSL: 0, wantBookTP: []int64{7001, 7002, 0}, wantBookArmed: []bool{true, true, false}, wantRemoved: []int64{111}},
		{name: "Q=0 with a rejected removal cancel keeps the stop id and reports it resting", sc: tiered, stop: unbacked, u: hlCloseUnconfirmed{StopOID: 111}, slResult: cancelOnly(HyperliquidStopLossUpdateResult{Error: "cancel failed", CancelStopLossError: "busy"}), wantUpdates: 1, wantBookSL: 111, wantBookTP: []int64{7001, 7002, 0}, wantBookArmed: []bool{true, true, false}, wantResting: []int64{111}},
		{name: "Q=0 with unreadable open orders reports the stop unverified", sc: tiered, stop: unbacked, u: hlCloseUnconfirmed{StopOID: 111}, slResult: cancelOnly(HyperliquidStopLossUpdateResult{Error: "open orders unreadable", OpenOrderCheckError: "indexer down"}), wantUpdates: 1, wantBookSL: 111, wantBookTP: []int64{7001, 7002, 0}, wantBookArmed: []bool{true, true, false}, wantUnverified: []int64{111}},
		{name: "Q=0 with the stop already filled leaves it to the reconciler", sc: tiered, stop: unbacked, u: hlCloseUnconfirmed{StopOID: 111}, slResult: cancelOnly(HyperliquidStopLossUpdateResult{StopLossFilledExternally: true}), wantUpdates: 1, wantBookSL: 111, wantBookTP: []int64{7001, 7002, 0}, wantBookArmed: []bool{true, true, false}, wantFilled: []int64{111}},
		{name: "Q=0 with no update result reports the stop unverified", sc: tiered, stop: unbacked, u: hlCloseUnconfirmed{StopOID: 111}, wantUpdates: 1, wantBookSL: 111, wantBookTP: []int64{7001, 7002, 0}, wantBookArmed: []bool{true, true, false}, wantUnverified: []int64{111}},
		{name: "Q=0 removes the stop and an open and a gone take-profit", sc: tiered, stop: unbacked, u: hlCloseUnconfirmed{StopOID: 111, TPOIDs: []int64{7001, 7002}}, slResult: cancelOnly(HyperliquidStopLossUpdateResult{CancelStopLossSucceeded: true}), syncResult: &HyperliquidProtectionSyncResult{TPCancelNotOpenOIDs: []int64{7002}}, wantUpdates: 1, wantSyncs: 1, wantPlanCancel: []int64{7001, 7002}, wantBookSL: 0, wantBookTP: []int64{0, 0, 0}, wantBookArmed: []bool{false, false, false}, wantRemoved: []int64{111, 7001, 7002}},
		{name: "Q=0 keeps a take-profit whose removal cancel was rejected", sc: tiered, stop: unbacked, u: hlCloseUnconfirmed{TPOIDs: []int64{7001, 7002}}, syncResult: &HyperliquidProtectionSyncResult{TPCancelFailedOIDs: []int64{7001}}, wantSyncs: 1, wantPlanCancel: []int64{7001, 7002}, wantBookSL: 111, wantBookTP: []int64{7001, 0, 0}, wantBookArmed: []bool{true, false, false}, wantRemoved: []int64{7002}, wantResting: []int64{7001}},
		{name: "Q=0 with an empty unconfirmed set runs no subprocess", sc: tiered, stop: unbacked, wantBookSL: 111, wantBookTP: []int64{7001, 7002, 0}, wantBookArmed: []bool{true, true, false}},
		{name: "Q>0 force-replaces only the unconfirmed tiers", sc: tiered, stop: backed, u: hlCloseUnconfirmed{TPOIDs: []int64{7001}}, syncResult: &HyperliquidProtectionSyncResult{StopLossOID: 999, TPOIDs: []int64{9101, 7002, 9103}}, wantSyncs: 1, wantForceTP: []bool{true, false, false}, wantBookSL: 999, wantBookTP: []int64{9101, 7002, 9103}, wantBookArmed: []bool{true, true, true}},
		{name: "Q>0 behind a queued row removes the unconfirmed tiers for the next sync", sc: manualTiered, queued: true, stop: backed, u: hlCloseUnconfirmed{TPOIDs: []int64{7001}}, syncResult: &HyperliquidProtectionSyncResult{StopLossOID: 999}, wantSyncs: 1, wantPlanCancel: []int64{7001}, wantBookSL: 999, wantBookTP: []int64{0, 7002, 0}, wantBookArmed: []bool{false, true, false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var updates []rearmSLCall
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				updates = append(updates, rearmSLCall{symbol: symbol, side: side, size: size, triggerPx: triggerPx, cancelOID: cancelOID})
				if tc.slResult == nil {
					return nil, "", errors.New("exit status 1")
				}
				return tc.slResult, "", nil
			}
			var plans []hlProtectionPlan
			syncHyperliquidProtection = func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger, hints []byte) (*HyperliquidProtectionSyncResult, bool) {
				plans = append(plans, plan)
				return tc.syncResult, true
			}
			var db *StateDB
			if tc.queued {
				db = openTestDB(t)
				if err := db.InsertPendingManualAction(PendingManualAction{StrategyID: tc.sc.ID, Action: "close", Symbol: "ETH", Side: "sell", Quantity: 7, FillPrice: 2100, CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			st := &StrategyState{ID: tc.sc.ID, Positions: map[string]*Position{
				"ETH": {Symbol: "ETH", Side: "long", Quantity: 3, InitialQuantity: 10, AvgCost: 2000, RiskAnchorPrice: 2000, EntryATR: 50, OwnerStrategyID: tc.sc.ID, StopLossOID: 111, StopLossTriggerPx: 1900, TPOIDs: []int64{7001, 7002, 0}, TPArmedTiers: []bool{true, true, false}},
			}}
			notifier, backend := confirmationNotifier()
			var mu sync.RWMutex
			_, _, res, _ := rearmProtectionForCloseRemainder(tc.sc, st, db, "ETH", 2000, 111, 1900, 2000, nil, nil, nil, tc.u, tc.stop, &mu, notifier, newTestLogger(t))
			if len(updates) != tc.wantUpdates || len(plans) != tc.wantSyncs {
				t.Fatalf("updates=%+v syncs=%d, want %d updates and %d syncs", updates, len(plans), tc.wantUpdates, tc.wantSyncs)
			}
			if tc.stop.Unbacked {
				for _, u := range updates {
					if u.size != 0 || u.triggerPx != 0 || u.cancelOID != 111 {
						t.Fatalf("stop removal call = %+v, want size 0 trigger 0 cancel 111", u)
					}
				}
				for _, p := range plans {
					if p.Size != 0 || p.StopLossATRMult != 0 || len(p.Tiers) != 0 || p.AvgCost <= 0 || p.EntryATR <= 0 {
						t.Fatalf("take-profit removal plan = %+v, want size 0, no stop leg, no tiers and a valid anchor", p)
					}
				}
			}
			if len(plans) == 1 && (fmt.Sprint(plans[0].CancelTPOIDs) != fmt.Sprint(tc.wantPlanCancel) || fmt.Sprint(plans[0].ForceTPReplace) != fmt.Sprint(tc.wantForceTP)) {
				t.Fatalf("plan cancel=%v force_tp=%v, want cancel %v force_tp %v", plans[0].CancelTPOIDs, plans[0].ForceTPReplace, tc.wantPlanCancel, tc.wantForceTP)
			}
			pos := st.Positions["ETH"]
			if pos.StopLossOID != tc.wantBookSL || fmt.Sprint(pos.TPOIDs) != fmt.Sprint(tc.wantBookTP) || fmt.Sprint(pos.TPArmedTiers) != fmt.Sprint(tc.wantBookArmed) {
				t.Fatalf("book sl=%d tp=%v armed=%v, want sl %d tp %v armed %v", pos.StopLossOID, pos.TPOIDs, pos.TPArmedTiers, tc.wantBookSL, tc.wantBookTP, tc.wantBookArmed)
			}
			r := res.Removal
			if fmt.Sprint(r.Removed, r.Filled, r.Resting, r.Unverified) != fmt.Sprint(tc.wantRemoved, tc.wantFilled, tc.wantResting, tc.wantUnverified) {
				t.Fatalf("removal = %+v, want removed %v filled %v resting %v unverified %v", r, tc.wantRemoved, tc.wantFilled, tc.wantResting, tc.wantUnverified)
			}
			backend.mu.Lock()
			sent := len(backend.messages) + len(backend.dms)
			backend.mu.Unlock()
			if sent != 0 {
				t.Fatalf("the arm sent %d alerts, want none: the re-arm report is the single alert", sent)
			}
		})
	}
}
