package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

func newPairCostMatchedEnv(t *testing.T) *pairTestEnv {
	t.Helper()
	e := newPairTestEnv(t, pairCostRates)
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	e.slotStep(1)
	e.assertBooks("matched", 0.06, -0.06)
	return e
}

type pairBookSnapshot struct {
	cash   float64
	qty    float64
	trades int
}

func (e *pairTestEnv) bookSnapshot(id string) pairBookSnapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s := e.state.Strategies[id]
	snap := pairBookSnapshot{cash: s.Cash, trades: len(s.TradeHistory)}
	if pos := s.Positions["BTC"]; pos != nil {
		snap.qty = pos.Quantity
	}
	return snap
}

func (e *pairTestEnv) lastTrade(id string) Trade {
	e.mu.RLock()
	defer e.mu.RUnlock()
	h := e.state.Strategies[id].TradeHistory
	return h[len(h)-1]
}

func (e *pairTestEnv) assertCostFaultRecovery(label, id string) {
	e.t.Helper()
	p := e.rt().Pair.Progress
	if p.Lifecycle != pairLifecycleRecovery || !p.IncreasesFrozen || p.obligation(pairObligationCostProvenance, id, "") == nil {
		e.t.Fatalf("%s: a cost provenance fault must open recovery, freeze increases and keep an obligation: %+v", label, p)
	}
	if !strings.Contains(e.savedProgress(), pairObligationCostProvenance) {
		e.t.Fatalf("%s: the cost provenance obligation must be saved", label)
	}
}

func TestBasketPairCostNativeOwnersUseTheMemberBinding(t *testing.T) {
	e := newPairCostMatchedEnv(t)
	hlSC, okxSC := e.bc.members[pairTestHL], e.bc.members[pairTestOKX]
	hl, okx := e.state.Strategies[pairTestHL], e.state.Strategies[pairTestOKX]

	e.mu.Lock()
	hl.Positions["BTC"].StopLossTriggerPx = 59000
	e.mu.Unlock()
	if n, _ := applyPaperStopLossBreach(hlSC, hl, "BTC", entrySideLong, 58800, e.mu, pairCostLogger(pairTestHL)); n != 1 {
		t.Fatal("the paper stop must book the breach")
	}
	tr := e.lastTrade(pairTestHL)
	wantPx := 58800 * (1 - 0.001)
	if !pairCostNear(tr.Price, wantPx) || !pairCostNear(tr.ExchangeFee, 0.06*wantPx*0.0003) || tr.CostPolicyID != e.binding(pairTestHL).PolicyID || tr.CostModelVersion != FillCostModelReplayCustomVersion {
		t.Fatalf("the paper stop row %+v must use the member slippage, taker rate and stamp", tr)
	}

	res := &OKXResult{Symbol: "BTC", Signal: 1, Mode: "paper", Platform: "okx"}
	res.CloseFraction = 1
	e.mu.Lock()
	trades, _, _ := executeOKXResult(okxSC, okx, e.db(), res, nil, "BUY", 60000, nil, e.cfg, HurstGateDecision{}, pairCostLogger(pairTestOKX))
	e.mu.Unlock()
	if trades != 1 || e.signed(pairTestOKX) != 0 {
		t.Fatalf("the native OKX management close must book: trades %d, book %v", trades, e.signed(pairTestOKX))
	}
	tr = e.lastTrade(pairTestOKX)
	wantPx = 60000 * (1 + 0.002)
	if !pairCostNear(tr.Price, wantPx) || !pairCostNear(tr.ExchangeFee, 0.06*wantPx*0.0007) || tr.CostPolicyID != e.binding(pairTestOKX).PolicyID {
		t.Fatalf("the native OKX close row %+v must use the member slippage, taker rate and stamp", tr)
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{pairTestHL, pairTestOKX} {
		for _, r := range e.savedTrades(id) {
			if r.PolicyID != e.binding(id).PolicyID {
				t.Fatalf("saved %s row %+v lost the member stamp", id, r)
			}
		}
	}

	k := newPairCostMatchedEnv(t)
	for _, id := range []string{pairTestHL, pairTestOKX} {
		sc := k.bc.members[id]
		s := k.state.Strategies[id]
		before := k.bookSnapshot(id)
		k.mu.Lock()
		closeVirtualPositionAtMark(s, &sc, "BTC", s.Positions["BTC"], map[string]float64{"BTC": 60100}, pairCostLogger(id))
		pending := s.RiskState.getPendingCircuitClose(PlatformPendingClosePair)
		k.mu.Unlock()
		if got := k.bookSnapshot(id); got != before || pending == nil {
			t.Fatalf("a shared-price circuit-breaker close of %s must book nothing and request the pair close: %+v -> %+v pending %+v", id, before, got, pending)
		}
	}
	k.step(k.now.Add(time.Second))
	for _, id := range []string{pairTestHL, pairTestOKX} {
		k.mu.RLock()
		view := k.rt().pairView[id]
		k.mu.RUnlock()
		own, valid, why := view.usdMarkAt(k.now.UnixMilli())
		tr := k.lastTrade(id)
		if !valid || !tr.IsClose || tr.FeeSource != FeeSourceReconcileAdjustment || tr.ExchangeFee != 0 || tr.CostPolicyID != k.binding(id).PolicyID || !pairCostNear(tr.Price, own) || !strings.Contains(tr.Details, "own checked mark") {
			t.Fatalf("the circuit-breaker close of %s must book at its own checked mark %v (%s), keep its model-only classification and carry the member stamp: %+v", id, own, why, tr)
		}
		if k.signed(id) != 0 {
			t.Fatalf("%s must be flat after the own-mark close", id)
		}
	}
}

func TestBasketPairCostMissingOwnershipRefusesBeforeAnyBookChange(t *testing.T) {
	e := newPairCostMatchedEnv(t)
	hlSC, okxSC := e.bc.members[pairTestHL], e.bc.members[pairTestOKX]
	hl, okx := e.state.Strategies[pairTestHL], e.state.Strategies[pairTestOKX]
	beforeHL, beforeOKX := e.bookSnapshot(pairTestHL), e.bookSnapshot(pairTestOKX)
	e.mu.Lock()
	rtHL, rtOKX := hl.Basket, okx.Basket
	hl.Basket, okx.Basket = nil, nil
	hl.Positions["BTC"].StopLossTriggerPx = 59000
	e.mu.Unlock()

	if n, _ := applyPaperStopLossBreach(hlSC, hl, "BTC", entrySideLong, 58800, e.mu, pairCostLogger(pairTestHL)); n != 0 {
		t.Fatal("a paper stop without pair ownership must not book")
	}
	e.mu.Lock()
	recordPerpsStopLossClose(hl, "BTC", 59000, "stop_loss", pairCostLogger(pairTestHL))
	closeVirtualPositionAtMark(hl, &hlSC, "BTC", hl.Positions["BTC"], map[string]float64{"BTC": 60000}, pairCostLogger(pairTestHL))
	applyHyperliquidCircuitCloseFill(hl, "BTC", 0.06, 59900, 1.2, 0.06, 4242, "kill_switch")
	res := &OKXResult{Symbol: "BTC", Signal: 1, Mode: "paper", Platform: "okx"}
	res.CloseFraction = 1
	trades, _, _ := executeOKXResult(okxSC, okx, e.db(), res, nil, "BUY", 60000, nil, e.cfg, HurstGateDecision{}, pairCostLogger(pairTestOKX))
	if _, err := ExecutePerpsSignalWithLeverage(okx, 1, "BTC", 60000, PerpsSizing{SizingLeverage: 1}, 0.06, "x", 0, DirectionBoth, 1, pairCostLogger(pairTestOKX)); err == nil {
		t.Error("the shared perps owner must refuse a pair member without ownership")
	}
	e.mu.Unlock()
	if trades != 0 {
		t.Fatal("the OKX result owner booked without pair ownership")
	}
	if got := e.bookSnapshot(pairTestHL); got != beforeHL {
		t.Fatalf("the hyperliquid book changed without ownership: %+v -> %+v", beforeHL, got)
	}
	if got := e.bookSnapshot(pairTestOKX); got != beforeOKX {
		t.Fatalf("the okx book changed without ownership: %+v -> %+v", beforeOKX, got)
	}
	if len(hl.pairCostFaults) == 0 || len(okx.pairCostFaults) == 0 {
		t.Fatal("every refusal must queue a cost provenance fault")
	}
	e.mu.Lock()
	hl.Basket, okx.Basket = rtHL, rtOKX
	e.mu.Unlock()
	e.step(e.now.Add(time.Minute))
	e.assertCostFaultRecovery("lost link", pairTestHL)

	r := newPairCostMatchedEnv(t)
	before := r.bookSnapshot(pairTestHL)
	r.adm.release()
	r.mu.Lock()
	recordPerpsStopLossClose(r.state.Strategies[pairTestHL], "BTC", 59000, "stop_loss", pairCostLogger(pairTestHL))
	r.mu.Unlock()
	if got := r.bookSnapshot(pairTestHL); got != before {
		t.Fatalf("a released admission let a native close book: %+v -> %+v", before, got)
	}
}

func TestBasketPairCostRecordTradeBackstopNeverChangesRows(t *testing.T) {
	e := newPairCostMatchedEnv(t)
	hl := e.state.Strategies[pairTestHL]
	cash := hl.Cash
	e.mu.Lock()
	RecordTrade(hl, Trade{Timestamp: e.now, Symbol: "BTC", Side: "sell", Quantity: 0.06, Price: 60000, Value: -0.5, TradeType: TradeTypeFunding, Details: "funding"})
	funding := hl.TradeHistory[len(hl.TradeHistory)-1]
	faultsAfterFunding := len(hl.pairCostFaults)
	RecordTrade(hl, Trade{Timestamp: e.now.Add(time.Millisecond), Symbol: "BTC", Side: "sell", Quantity: 0.01, Price: 60000, Value: 600, TradeType: "perps", IsClose: true, Details: "forced unscoped row"})
	forced := hl.TradeHistory[len(hl.TradeHistory)-1]
	e.mu.Unlock()
	if funding.CostModelVersion != FillCostModelVersion || funding.CostPolicyID != "" || faultsAfterFunding != 0 {
		t.Fatalf("a pair member funding row keeps the process stamp and raises no fault: %+v", funding)
	}
	if forced.CostModelVersion != 0 || forced.CostPolicyID != "" || len(hl.pairCostFaults) != 1 || hl.Cash != cash {
		t.Fatalf("the backstop must record an unscoped row unchanged and raise one fault: %+v faults %v", forced, hl.pairCostFaults)
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		t.Fatal(err)
	}
	rows := e.savedTrades(pairTestHL)
	if len(rows) < 2 || rows[len(rows)-2].Type != TradeTypeFunding || rows[len(rows)-2].Version != FillCostModelVersion || rows[len(rows)-1].Version != 0 {
		t.Fatalf("the saved rows must keep their recorded stamps: %+v", rows)
	}
	e.step(e.now.Add(time.Minute))
	e.assertCostFaultRecovery("backstop", pairTestHL)
}

func pairCostFeeEnv(t *testing.T) (*pairTestEnv, *basketAction, basketVenueOutcome) {
	t.Helper()
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	e.runner.respond = func(r pairPolicyRequest) (pairPolicyResult, error) {
		if r.Lifecycle == pairLifecycleMatched {
			return pairHoldResult(r, "keep"), nil
		}
		return pairEnterResult(r, 0.06), nil
	}
	fill := pairThreeFills()
	pairOpenVenue(e, pairTestOKX, pairCostFees(fill, 1, false))
	e.slotStep(0)
	a := e.actionsByPurpose(pairPurposeEntry)[0]
	if !basketQtyEqual(a.AppliedQty, 0.06) || a.AppliedFeeVersion != 1 {
		t.Fatalf("setup: the entry must book with fee version 1: %+v", a)
	}
	return e, a, fill
}

func pairCostFees(fill basketVenueOutcome, n int, final bool) basketVenueOutcome {
	recs := []pairTestFeeRec{{"a", "1.8", "USDT"}, {"b", "0.3", "USDT"}, {"c", "0.3", "USDT"}}
	return pairWithFees(fill, final, recs[:n]...)
}

func (e *pairTestEnv) savedFeeRow(a *basketAction) (float64, int, string) {
	e.t.Helper()
	var fee float64
	var version int
	var policy string
	if err := e.db().db.QueryRow(`SELECT exchange_fee, cost_model_version, cost_policy_id FROM trades WHERE symbol = 'BTC' AND exchange_order_id = ?`, a.ClientOrderID).Scan(&fee, &version, &policy); err != nil {
		e.t.Fatal(err)
	}
	return fee, version, policy
}

func (e *pairTestEnv) savedIntentFee(a *basketAction) float64 {
	e.t.Helper()
	var fee float64
	if err := e.db().db.QueryRow(`SELECT applied_fee FROM entry_order_intents WHERE intent_id = ?`, a.EntryIntentID).Scan(&fee); err != nil {
		e.t.Fatal(err)
	}
	return fee
}

func TestBasketPairCostFeeCorrectionChecksTheSavedMemberStamp(t *testing.T) {
	e, a, fill := pairCostFeeEnv(t)
	okx := e.state.Strategies[pairTestOKX]
	want := e.binding(pairTestOKX)
	if m, _ := e.adm.ExecutionModel(pairTestOKX); m != pairExecutionVenueConfirmed {
		t.Fatalf("the live-shaped transport must freeze %s, got %s", pairExecutionVenueConfirmed, m)
	}
	if _, v, p := e.savedFeeRow(a); v != FillCostModelReplayCustomVersion || p != want.PolicyID {
		t.Fatalf("the booked entry row must carry the member stamp, got %d %s", v, p)
	}
	e.mu.Lock()
	okx.TradeHistory = nil
	e.mu.Unlock()
	cash := okx.Cash
	pairAnswerVenue(e, a, pairCostFees(fill, 2, false))
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	fee, v, p := e.savedFeeRow(a)
	if a.AppliedFeeVersion != 2 || !pairCostNear(cash-okx.Cash, 0.30) || !pairCostNear(fee, 2.10) || v != FillCostModelReplayCustomVersion || p != want.PolicyID {
		t.Fatalf("a correction whose row left the retained history must apply once and keep the stamp: version %d cash delta %v fee %v stamp %d %s", a.AppliedFeeVersion, cash-okx.Cash, fee, v, p)
	}

	if _, err := e.db().db.Exec(`UPDATE trades SET cost_policy_id = 'pair-cost-forged' WHERE exchange_order_id = ?`, a.ClientOrderID); err != nil {
		t.Fatal(err)
	}
	cash = okx.Cash
	intentFee := e.savedIntentFee(a)
	pairAnswerVenue(e, a, pairCostFees(fill, 3, true))
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	fee, _, p = e.savedFeeRow(a)
	if a.AppliedFeeVersion != 2 || okx.Cash != cash || !pairCostNear(fee, 2.10) || p != "pair-cost-forged" || e.savedIntentFee(a) != intentFee {
		t.Fatalf("a mismatched saved stamp must hold the correction with nothing changed: version %d cash %v->%v fee %v", a.AppliedFeeVersion, cash, okx.Cash, fee)
	}
	if e.rt().Pair.Progress.obligation(pairObligationCostProvenance, pairTestOKX, a.ActionID) == nil || !e.rt().Pair.Progress.IncreasesFrozen {
		t.Fatalf("the refused correction must report an accounting fault: %+v", e.rt().Pair.Progress)
	}
}

func TestBasketPairCostFeeCorrectionRefusedInTheSaveRollsBack(t *testing.T) {
	e, a, fill := pairCostFeeEnv(t)
	okx := e.state.Strategies[pairTestOKX]
	cash := okx.Cash
	daily := okx.RiskState.DailyPnL
	intentApplied := e.savedIntentFee(a)
	in := okx.EntryGuard.Intents[a.EntryIntentID]
	var memApplied, memConfirmed float64
	if in != nil {
		memApplied, memConfirmed = in.AppliedFee, in.ConfirmedFee
	}
	f := &pairCommitFault{from: 2, to: 2}
	f.install(t)
	pairAnswerVenue(e, a, pairCostFees(fill, 2, false))
	e.step(e.now.Add(time.Second))
	if !e.rt().needsDurable || a.pendingFee == nil || !pairCostNear(cash-okx.Cash, 0.30) {
		t.Fatalf("setup: the correction must sit in memory behind a failed save: needsDurable %v pending %+v cash delta %v (commits %d)", e.rt().needsDurable, a.pendingFee, cash-okx.Cash, f.n)
	}
	if _, err := e.db().db.Exec(`UPDATE trades SET cost_policy_id = 'pair-cost-changed' WHERE exchange_order_id = ?`, a.ClientOrderID); err != nil {
		t.Fatal(err)
	}
	e.step(e.now.Add(time.Second))
	fee, _, _ := e.savedFeeRow(a)
	switch {
	case e.rt().needsDurable:
		t.Fatal("the rollback must let the partition save again")
	case okx.Cash != cash || okx.RiskState.DailyPnL != daily:
		t.Fatalf("the rollback must restore cash and daily PnL: cash %v->%v daily %v->%v", cash, okx.Cash, daily, okx.RiskState.DailyPnL)
	case e.savedIntentFee(a) != intentApplied || (in != nil && (in.AppliedFee != memApplied || in.ConfirmedFee != memConfirmed)):
		t.Fatalf("the rollback must leave the intent unchanged: saved %v", e.savedIntentFee(a))
	case a.AppliedFeeVersion != 1 || !pairCostNear(a.AppliedFee, 1.80) || a.pendingFee != nil:
		t.Fatalf("the rollback must restore the action: %+v", a)
	case !pairCostNear(fee, 1.80):
		t.Fatalf("the saved row fee changed to %v", fee)
	case e.rt().Pair.Progress.obligation(pairObligationCostProvenance, pairTestOKX, a.ActionID) == nil:
		t.Fatal("the refused correction must keep an accounting fault obligation")
	}
	if math.Abs(okx.Cash-cash) > 0 {
		t.Fatal("cash moved")
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		t.Fatalf("a later ordinary partition save must succeed after the rollback: %v", err)
	}
}

func TestBasketPairCostRefusedNativeCloseOpensRecoveryAndNeverBooksLegacy(t *testing.T) {
	f := newPairKillSwitchFixture(t)
	e := f.env
	hl := e.state.Strategies[pairTestHL]
	before := e.bookSnapshot(pairTestHL)
	e.mu.Lock()
	rt := hl.Basket
	hl.Basket = nil
	e.mu.Unlock()
	f.fire()
	if f.hlCalls != 1 {
		t.Fatal("the native close must still be sent")
	}
	for pass := 0; pass < 2; pass++ {
		e.mu.Lock()
		applyHyperliquidCircuitCloseFill(hl, "BTC", 0.06, 59900, 1.2, 0.06, 4242, "kill_switch")
		e.mu.Unlock()
		if got := e.bookSnapshot(pairTestHL); got != before {
			t.Fatalf("pass %d: a confirmed close without pair ownership was booked: %+v -> %+v", pass, before, got)
		}
	}
	e.mu.Lock()
	hl.Basket = rt
	e.mu.Unlock()
	e.step(e.now.Add(time.Minute))
	e.assertCostFaultRecovery("refused native close", pairTestHL)
	critical := false
	for _, k := range e.rt().Pair.Progress.Alerts {
		if strings.HasPrefix(k, "cost|"+pairTestHL) {
			critical = true
		}
	}
	if !critical {
		t.Fatalf("the refused close must raise a critical cost alert: %v", e.rt().Pair.Progress.Alerts)
	}
	booked := e.bookSnapshot(pairTestHL)
	if booked.qty != 0 || booked.trades != before.trades+1 {
		t.Fatalf("with ownership restored the coordinator must book the confirmed native fill once from its fill records: %+v -> %+v", before, booked)
	}
	e.mu.Lock()
	applyHyperliquidCircuitCloseFill(hl, "BTC", 0.06, 59900, 1.2, 0.06, 4242, "kill_switch")
	e.mu.Unlock()
	if got := e.bookSnapshot(pairTestHL); got != booked {
		t.Fatalf("the generic close owner never books a pair member: %+v -> %+v", booked, got)
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.savedTrades(pairTestHL) {
		if r.Version != FillCostModelReplayCustomVersion || r.PolicyID != e.binding(pairTestHL).PolicyID {
			t.Fatalf("no pair row may carry a legacy or missing stamp: %+v", r)
		}
	}
}
