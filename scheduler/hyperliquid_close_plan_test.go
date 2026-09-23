package main

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func hlSignedView(coin string, signed float64) hlOnChainCoinView {
	v := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{}, NetSide: map[string]string{}}
	if signed > 0 {
		v.AbsQty[coin] = signed
		v.NetSide[coin] = "long"
	} else if signed < 0 {
		v.AbsQty[coin] = -signed
		v.NetSide[coin] = "short"
	}
	return v
}

func TestPlanHLCloseOrder(t *testing.T) {
	unknown := hlOnChainCoinView{}
	sideless := hlOnChainCoinView{Known: true, AbsQty: map[string]float64{"ETH": 3}, NetSide: map[string]string{}}
	cases := []struct {
		name       string
		side       string
		posQty     float64
		closeQty   float64
		same, opp  float64
		view       hlOnChainCoinView
		wantAction hlCloseAction
		wantMode   hlCloseMode
		wantSize   float64
		wantCapped bool
	}{
		{"sole owner book above chain caps reduce-only", "long", 10, 10, 0, 0, hlSignedView("ETH", 7), hlCloseSend, hlCloseModeReduceOnly, 7, true},
		{"sole owner chain flat sends nothing", "long", 10, 10, 0, 0, hlSignedView("ETH", 0), hlCloseSkip, hlCloseModeNone, 0, false},
		{"sole owner chain opposite sends nothing", "long", 10, 10, 0, 0, hlSignedView("ETH", -2), hlCloseSkip, hlCloseModeNone, 0, false},
		{"sole owner partial within chain", "long", 10, 4, 0, 0, hlSignedView("ETH", 10), hlCloseSend, hlCloseModeReduceOnly, 4, false},
		{"sole owner partial with book above chain caps", "long", 10, 5, 0, 0, hlSignedView("ETH", 7), hlCloseSend, hlCloseModeReduceOnly, 2, true},
		{"same-side peer keeps its share", "long", 10, 10, 5, 0, hlSignedView("ETH", 12), hlCloseSend, hlCloseModeReduceOnly, 7, true},
		{"opposite-side peer crosses to its book", "long", 10, 10, 0, 4, hlSignedView("ETH", 6), hlCloseSend, hlCloseModeCross, 10, false},
		{"opposite-side net already short crosses", "long", 10, 10, 0, 14, hlSignedView("ETH", -4), hlCloseSend, hlCloseModeCross, 10, false},
		{"opposite-side chain flat crosses only to the peer book", "long", 10, 10, 0, 4, hlSignedView("ETH", 0), hlCloseSend, hlCloseModeCross, 4, true},
		{"opposite-side peer stop unbooked caps at own book", "long", 10, 10, 0, 4, hlSignedView("ETH", 10), hlCloseSend, hlCloseModeCross, 10, false},
		{"opposite-side partial stays reduce-only", "long", 10, 3, 0, 4, hlSignedView("ETH", 6), hlCloseSend, hlCloseModeReduceOnly, 3, false},
		{"short strategy with long peer buys cross", "short", 10, 10, 0, 4, hlSignedView("ETH", -6), hlCloseSend, hlCloseModeCross, 10, false},
		{"short sole owner book above chain caps", "short", 10, 10, 0, 0, hlSignedView("ETH", -3), hlCloseSend, hlCloseModeReduceOnly, 3, true},
		{"unknown chain without opposite peer sends reduce-only book size", "long", 10, 10, 5, 0, unknown, hlCloseSend, hlCloseModeReduceOnly, 10, false},
		{"unknown chain with opposite peer defers", "long", 10, 10, 0, 4, unknown, hlCloseDefer, hlCloseModeNone, 0, false},
		{"unreadable net side defers", "long", 10, 10, 0, 0, sideless, hlCloseDefer, hlCloseModeNone, 0, false},
		{"close above book defers", "long", 10, 11, 0, 0, hlSignedView("ETH", 10), hlCloseDefer, hlCloseModeNone, 0, false},
		{"unknown side defers", "", 10, 10, 0, 0, hlSignedView("ETH", 10), hlCloseDefer, hlCloseModeNone, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planHLCloseOrder("ETH", tc.side, tc.posQty, tc.closeQty, tc.same, tc.opp, tc.view)
			if got.Action != tc.wantAction || got.Mode != tc.wantMode || math.Abs(got.Size-tc.wantSize) > 1e-9 || got.Capped != tc.wantCapped {
				t.Fatalf("plan = %+v, want action %v mode %v size %g capped %t", got, tc.wantAction, tc.wantMode, tc.wantSize, tc.wantCapped)
			}
			if got.Action != hlCloseSend {
				return
			}
			if got.Size > tc.closeQty+1e-9 {
				t.Fatalf("size %g exceeds the book close %g", got.Size, tc.closeQty)
			}
			if !tc.view.Known {
				return
			}
			net, _ := hlOnChainSignedQty(tc.view, "ETH")
			s := hlSideSign(tc.side)
			target := s*(tc.posQty-tc.closeQty) + s*tc.same - s*tc.opp
			after := net - s*got.Size
			lo, hi := math.Min(net, target), math.Max(net, target)
			if after < lo-1e-9 || after > hi+1e-9 {
				t.Fatalf("chain after close %g leaves the range [%g, %g] between the net and the books", after, lo, hi)
			}
			if got.Mode == hlCloseModeReduceOnly && got.Size > math.Abs(net)+1e-9 {
				t.Fatalf("reduce-only size %g exceeds the on-chain %g", got.Size, math.Abs(net))
			}
		})
	}
}

func TestHLPeerBooksOnCoin(t *testing.T) {
	live := []string{"hold", "ETH", "1h", "--mode=live"}
	cfgs := []StrategyConfig{
		{ID: "self", Type: "perps", Platform: "hyperliquid", Args: live},
		{ID: "peer-long", Type: "perps", Platform: "hyperliquid", Args: live},
		{ID: "peer-short", Type: "perps", Platform: "hyperliquid", Args: live},
		{ID: "hedger", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "BTC", "1h", "--mode=live"}, Hedge: &HedgeConfig{Enabled: true, Symbol: "eth"}},
		{ID: "manual-eth", Type: "manual", Platform: "hyperliquid", Symbol: "eth", Args: []string{"hold", "eth", "1h", "--mode=live"}},
		{ID: "other-coin", Type: "perps", Platform: "hyperliquid", Args: []string{"hold", "SOL", "1h", "--mode=live"}},
	}
	states := map[string]*StrategyState{
		"self":       {Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 10}}},
		"peer-long":  {Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "long", Quantity: 5}}},
		"peer-short": {Positions: map[string]*Position{"ETH": {Symbol: "ETH", Side: "short", Quantity: 3}}},
		"hedger": {Positions: map[string]*Position{
			"BTC": {Symbol: "BTC", Side: "long", Quantity: 1},
			"ETH": {Symbol: "ETH", Side: "short", Quantity: 2, HedgeFor: "BTC"},
		}},
		"manual-eth": {Positions: map[string]*Position{"eth": {Symbol: "eth", Side: "long", Quantity: 1}}},
		"other-coin": {Positions: map[string]*Position{"SOL": {Symbol: "SOL", Side: "short", Quantity: 9}}},
	}
	cases := []struct {
		name     string
		selfID   string
		selfSide string
		wantSame float64
		wantOpp  float64
	}{
		{"long self counts long peers as same side", "self", "long", 6, 5},
		{"short self counts long peers as opposite side", "self", "short", 5, 6},
		{"a peer excludes only itself", "peer-long", "long", 11, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			same, opp := hlPeerBooksOnCoin(states, cfgs, "Eth", tc.selfID, tc.selfSide)
			if math.Abs(same-tc.wantSame) > 1e-9 || math.Abs(opp-tc.wantOpp) > 1e-9 {
				t.Fatalf("same=%g opp=%g, want same=%g opp=%g", same, opp, tc.wantSame, tc.wantOpp)
			}
		})
	}
}

func TestManualCloseFillAttribution(t *testing.T) {
	cases := []struct {
		name       string
		posQty     float64
		fill       *HyperliquidFill
		wantQty    float64
		wantFee    float64
		wantFullBk bool
	}{
		{"fill above book books only the book", 1.0, &HyperliquidFill{TotalSz: 1.25, Fee: 1.0}, 1.0, 0.8, true},
		{"capped fill below request books the fill", 1.0, &HyperliquidFill{TotalSz: 0.7, Fee: 0.35}, 0.7, 0.35, false},
		{"exact fill closes the book", 1.0, &HyperliquidFill{TotalSz: 1.0, Fee: 0.5}, 1.0, 0.5, true},
		{"dust under threshold counts as full", 1.0, &HyperliquidFill{TotalSz: 0.99995, Fee: 0.5}, 0.99995, 0.5, true},
		{"missing fill books nothing", 1.0, nil, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qty, fee, full := manualCloseFillAttribution(tc.posQty, tc.fill)
			if math.Abs(qty-tc.wantQty) > 1e-12 || math.Abs(fee-tc.wantFee) > 1e-12 || full != tc.wantFullBk {
				t.Fatalf("got qty=%g fee=%g full=%t, want qty=%g fee=%g full=%t", qty, fee, full, tc.wantQty, tc.wantFee, tc.wantFullBk)
			}
		})
	}
}

func TestBookManualCycleClose(t *testing.T) {
	sc := StrategyConfig{ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Args: []string{"--mode=live"}}
	cases := []struct {
		name        string
		intentFull  bool
		closeQty    float64
		fillSz      float64
		requested   []int64
		succeeded   []int64
		failed      []int64
		wantQty     float64
		wantFull    bool
		wantShort   bool
		wantSLOID   int64
		wantTPOIDs  []int64
		wantTPArmed []bool
		wantPnL     float64
	}{
		{name: "capped fill with the stop and every take-profit cancelled clears all ids", intentFull: true, closeQty: 7, fillSz: 7, requested: []int64{111, 201, 202}, succeeded: []int64{111, 201, 202}, wantQty: 7, wantShort: true, wantTPOIDs: []int64{0, 0}, wantTPArmed: []bool{false, false}, wantPnL: 699},
		{name: "partial IOC fill of a full close clears the cancelled ids", intentFull: true, closeQty: 10, fillSz: 4, requested: []int64{111, 201, 202}, succeeded: []int64{111, 201, 202}, wantQty: 4, wantShort: true, wantTPOIDs: []int64{0, 0}, wantTPArmed: []bool{false, false}, wantPnL: 399},
		{name: "a stop cancel the venue reported as failed keeps its id", intentFull: true, closeQty: 10, fillSz: 4, requested: []int64{111, 201, 202}, succeeded: []int64{201, 202}, failed: []int64{111}, wantQty: 4, wantShort: true, wantSLOID: 111, wantTPOIDs: []int64{0, 0}, wantTPArmed: []bool{false, false}, wantPnL: 399},
		{name: "a full fill closes the book and leaves the ids to the book delete", intentFull: true, closeQty: 10, fillSz: 10, requested: []int64{111, 201, 202}, succeeded: []int64{111, 201, 202}, wantQty: 10, wantFull: true, wantSLOID: 111, wantTPOIDs: []int64{201, 202}, wantTPArmed: []bool{true, true}, wantPnL: 999},
		{name: "a partial-intent close requested no cancel and keeps every id", closeQty: 5, fillSz: 5, requested: []int64{0}, wantQty: 5, wantSLOID: 111, wantTPOIDs: []int64{201, 202}, wantTPArmed: []bool{true, true}, wantPnL: 499},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := &Position{Symbol: "ETH", Quantity: 10, AvgCost: 2000, Side: "long", OwnerStrategyID: sc.ID, StopLossOID: 111, StopLossTriggerPx: 1900, TPOIDs: []int64{201, 202}, TPArmedTiers: []bool{true, true}}
			exec := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: tc.fillSz, Fee: 1, OID: 9}}, CancelStopLossSucceededOIDs: tc.succeeded, CancelStopLossFailedOIDs: tc.failed}
			if len(tc.failed) > 0 {
				exec.CancelStopLossError = "cancel rejected"
			}
			booking := bookManualCycleClose(sc, pos, "sell", tc.closeQty, tc.intentFull, exec, tc.requested, time.Unix(0, 0).UTC())
			clearHyperliquidProtectionOIDsMatching(pos, booking.ClearOIDs)
			a := booking.Action
			if math.Abs(a.Quantity-tc.wantQty) > 1e-9 || a.IsFullClose != tc.wantFull || booking.ShortOfIntent != tc.wantShort || math.Abs(a.RealizedPnL-tc.wantPnL) > 1e-9 || a.ExchangeOrderID != "9" {
				t.Fatalf("action qty=%g full=%t short=%t pnl=%g oid=%q, want qty=%g full=%t short=%t pnl=%g", a.Quantity, a.IsFullClose, booking.ShortOfIntent, a.RealizedPnL, a.ExchangeOrderID, tc.wantQty, tc.wantFull, tc.wantShort, tc.wantPnL)
			}
			if pos.StopLossOID != tc.wantSLOID || fmt.Sprint(pos.TPOIDs) != fmt.Sprint(tc.wantTPOIDs) || fmt.Sprint(pos.TPArmedTiers) != fmt.Sprint(tc.wantTPArmed) {
				t.Fatalf("book sl=%d tps=%v armed=%v, want sl=%d tps=%v armed=%v", pos.StopLossOID, pos.TPOIDs, pos.TPArmedTiers, tc.wantSLOID, tc.wantTPOIDs, tc.wantTPArmed)
			}
		})
	}
}

func TestSettleManualCycleCloseRearmsTheRemainderStop(t *testing.T) {
	oldUpdate, oldSync, oldRecorder := runHyperliquidUpdateStopLossFunc, syncHyperliquidProtection, tradeRecorder
	t.Cleanup(func() {
		runHyperliquidUpdateStopLossFunc, syncHyperliquidProtection, tradeRecorder = oldUpdate, oldSync, oldRecorder
	})
	tradeRecorder = func(string, Trade) error { return nil }
	live := []string{"hold", "ETH", "1h", "--mode=live"}
	atrMult, trailMult := 2.0, 2.0
	recorded := StrategyConfig{ID: "hl-manual", Type: "manual", Platform: "hyperliquid", Symbol: "ETH", Script: "x.py", Args: live, Leverage: 2}
	atr := recorded
	atr.StopLossATRMult = &atrMult
	ratchet := recorded
	ratchet.CloseStrategy = &StrategyRef{Name: "trailing_tp_ratchet"}
	ratchet.TrailingStopATRMult = &trailMult
	fill := func(sz float64, succeeded, failed []int64) *HyperliquidExecuteResult {
		r := &HyperliquidExecuteResult{Execution: &HyperliquidExecution{Fill: &HyperliquidFill{AvgPx: 2100, TotalSz: sz, Fee: 1, OID: 9}}, CancelStopLossSucceededOIDs: succeeded, CancelStopLossFailedOIDs: failed}
		if len(failed) > 0 {
			r.CancelStopLossError = "cancel rejected"
		}
		return r
	}
	rejected := &HyperliquidExecuteResult{Error: "order rejected", CancelStopLossSucceededOIDs: []int64{111}}
	cases := []struct {
		name          string
		sc            StrategyConfig
		exec          *HyperliquidExecuteResult
		immediate     bool
		wantUpdate    bool
		wantSync      bool
		wantSize      float64
		wantCancel    int64
		wantForce     bool
		wantTrigger   float64
		wantDrainQty  float64
		wantDrainSL   int64
		wantCloseQtys []float64
	}{
		{name: "recorded percentage stop is restored at the remainder with the old oid verified first", sc: recorded, exec: fill(4, []int64{111}, nil), wantUpdate: true, wantSize: 6, wantCancel: 111, wantTrigger: 1900, wantDrainQty: 6, wantDrainSL: 999, wantCloseQtys: []float64{4}},
		{name: "ATR owner re-arms the stop leg at the remainder while the close row is queued", sc: atr, exec: fill(4, []int64{111}, nil), wantSync: true, wantSize: 6, wantTrigger: 1900, wantDrainQty: 6, wantDrainSL: 999, wantCloseQtys: []float64{4}},
		{name: "ratchet trail owner re-arms from the high-water at the remainder", sc: ratchet, exec: fill(4, []int64{111}, nil), wantUpdate: true, wantSize: 6, wantCancel: 111, wantTrigger: 1900, wantDrainQty: 6, wantDrainSL: 999, wantCloseQtys: []float64{4}},
		{name: "a stop cancel the venue reported as failed keeps the id and the recorded re-arm verifies it on-chain", sc: recorded, exec: fill(4, nil, []int64{111}), wantUpdate: true, wantSize: 6, wantCancel: 111, wantTrigger: 1900, wantDrainQty: 6, wantDrainSL: 999, wantCloseQtys: []float64{4}},
		{name: "a stop cancel the venue reported as failed forces the ATR stop to resize to the remainder", sc: atr, exec: fill(4, nil, []int64{111}), wantSync: true, wantSize: 6, wantCancel: 111, wantForce: true, wantTrigger: 1900, wantDrainQty: 6, wantDrainSL: 999, wantCloseQtys: []float64{4}},
		{name: "a rejection after the cancel succeeded re-arms the whole book in the same cycle", sc: recorded, exec: rejected, wantUpdate: true, wantSize: 10, wantCancel: 111, wantTrigger: 1900, wantDrainQty: 10, wantDrainSL: 999},
		{name: "a re-armed stop that fills at once books only the remainder and the drain closes the rest once", sc: recorded, exec: fill(4, []int64{111}, nil), immediate: true, wantUpdate: true, wantSize: 6, wantCancel: 111, wantTrigger: 1900, wantCloseQtys: []float64{6, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var updates []rearmSLCall
			runHyperliquidUpdateStopLossFunc = func(script, symbol, side string, size, triggerPx float64, cancelOID int64) (*HyperliquidStopLossUpdateResult, string, error) {
				updates = append(updates, rearmSLCall{symbol: symbol, side: side, size: size, triggerPx: triggerPx, cancelOID: cancelOID})
				if tc.immediate {
					return &HyperliquidStopLossUpdateResult{StopLossFilledImmediately: true, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: true}, "", nil
				}
				return &HyperliquidStopLossUpdateResult{StopLossOID: 999, StopLossTriggerPx: triggerPx, CancelStopLossSucceeded: cancelOID > 0}, "", nil
			}
			var plans []hlProtectionPlan
			syncHyperliquidProtection = func(sc StrategyConfig, plan hlProtectionPlan, notifier *MultiNotifier, logger *StrategyLogger, hints []byte) (*HyperliquidProtectionSyncResult, bool) {
				plans = append(plans, plan)
				return &HyperliquidProtectionSyncResult{StopLossOID: 999, StopLossTriggerPx: plan.AvgCost - plan.StopLossATRMult*plan.EntryATR}, true
			}
			db := openTestDB(t)
			pos := &Position{Symbol: "ETH", Side: "long", Quantity: 10, InitialQuantity: 10, AvgCost: 2000, RiskAnchorPrice: 2000, EntryATR: 50, OwnerStrategyID: tc.sc.ID, StopLossOID: 111, StopLossTriggerPx: 1900, StopLossHighWaterPx: 2000, OpenedAt: time.Now().UTC().Add(-time.Hour)}
			ss := &StrategyState{ID: tc.sc.ID, Type: "manual", Platform: "hyperliquid", Cash: 1000, InitialCapital: 1000, Positions: map[string]*Position{"ETH": pos}}
			state := &AppState{Strategies: map[string]*StrategyState{tc.sc.ID: ss}}
			rearm := hlCloseRearmContext{Price: 2000, PrevStopOID: 111, PrevTriggerPx: 1900, PrevHighWater: 2000, OnChainAbsQty: map[string]float64{"ETH": 25}}
			var mu sync.RWMutex
			var execErr error
			if tc.exec.Error != "" {
				execErr = errors.New(tc.exec.Error)
			}
			settleManualCycleClose(tc.sc, ss, db, pos, "sell", 10, true, tc.exec, execErr, []int64{111}, rearm, &mu, nil, newTestLogger(t))
			if tc.wantUpdate != (len(updates) == 1) || len(updates) > 1 {
				t.Fatalf("stop updates = %+v, want one: %t", updates, tc.wantUpdate)
			}
			if tc.wantSync != (len(plans) == 1) || len(plans) > 1 {
				t.Fatalf("protection syncs = %+v, want one: %t", plans, tc.wantSync)
			}
			if tc.wantUpdate {
				got := updates[0]
				if math.Abs(got.size-tc.wantSize) > 1e-9 || got.cancelOID != tc.wantCancel || math.Abs(got.triggerPx-tc.wantTrigger) > 1e-6 {
					t.Fatalf("stop update = %+v, want size %g cancel %d trigger %g", got, tc.wantSize, tc.wantCancel, tc.wantTrigger)
				}
			}
			if tc.wantSync {
				got := plans[0]
				if math.Abs(got.Size-tc.wantSize) > 1e-9 || got.StopLossOID != tc.wantCancel || got.ForceSLReplace != tc.wantForce || got.StopLossATRMult <= 0 || len(got.Tiers) != 0 {
					t.Fatalf("protection plan size=%g sl_oid=%d force=%t mult=%g tiers=%d, want size %g sl_oid %d force %t and the stop leg only", got.Size, got.StopLossOID, got.ForceSLReplace, got.StopLossATRMult, len(got.Tiers), tc.wantSize, tc.wantCancel, tc.wantForce)
				}
			}
			drainPendingManualActions(state, &Config{Strategies: []StrategyConfig{tc.sc}}, openTestStore(t, db))
			after := ss.Positions["ETH"]
			switch {
			case tc.wantDrainQty == 0 && after != nil:
				t.Fatalf("book after drain = %+v, want the position closed", after)
			case tc.wantDrainQty > 0 && after == nil:
				t.Fatalf("book after drain is empty, want %g", tc.wantDrainQty)
			case tc.wantDrainQty > 0 && (math.Abs(after.Quantity-tc.wantDrainQty) > 1e-9 || after.StopLossOID != tc.wantDrainSL):
				t.Fatalf("book after drain qty=%g sl=%d, want qty %g sl %d", after.Quantity, after.StopLossOID, tc.wantDrainQty, tc.wantDrainSL)
			}
			var closeQtys []float64
			for _, tr := range ss.TradeHistory {
				if tr.IsClose {
					closeQtys = append(closeQtys, tr.Quantity)
				}
			}
			if fmt.Sprint(closeQtys) != fmt.Sprint(tc.wantCloseQtys) {
				t.Fatalf("close trades = %v, want %v", closeQtys, tc.wantCloseQtys)
			}
			if rows, err := db.LoadPendingManualActions(); err != nil || len(rows) != 0 {
				t.Fatalf("queued rows after drain = %+v (err %v), want none", rows, err)
			}
		})
	}
}
