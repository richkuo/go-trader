package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pairLiveMatched(t *testing.T, e *pairTestEnv) time.Time {
	t.Helper()
	pairEnterHoldRunner(e, 0.06)
	fill := func(px float64, cur string) basketVenueOutcome {
		return pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * px, Terminal: true}, 1.5, cur, true)
	}
	pairOpenVenue(e, pairTestOKX, fill(59800, "USDT"))
	e.slotStep(0)
	pairOpenVenue(e, pairTestHL, fill(60200, "USDC"))
	at := e.slotStep(1)
	e.assertBooks("matched", 0.06, -0.06)
	return at
}

func pairReductionRequest(t *testing.T, a *basketAction) pairActionRequest {
	t.Helper()
	var req pairActionRequest
	if err := json.Unmarshal([]byte(a.RequestJSON), &req); err != nil {
		t.Fatalf("saved request %s is unreadable: %v", a.ActionID, err)
	}
	wire, err := pairTransportRequestJSON(a.RequestJSON)
	if err != nil || strings.Contains(wire, `"reference":`) {
		t.Fatalf("the transport request of %s must drop the reference observation (%v)", a.ActionID, err)
	}
	return req
}

func (e *pairTestEnv) shiftHLBids(top float64) {
	e.quotes.mu.Lock()
	defer e.quotes.mu.Unlock()
	var bids [][2]string
	for i := 0; i < 6; i++ {
		bids = append(bids, [2]string{fmt.Sprint(top - float64(i)), "0.5"})
	}
	e.quotes.hlBids = bids
}

func pairDepthLevels(top, step float64, n int) [][2]string {
	var out [][2]string
	for i := 0; i < n; i++ {
		out = append(out, [2]string{strconv.FormatFloat(math.Round((top+step*float64(i))*10)/10, 'f', -1, 64), "1"})
	}
	return out
}

func TestBasketPairReductionLiveProtectiveNeedsAFreshReference(t *testing.T) {
	t.Run("a missing reference holds with backoff, one alert and no send", func(t *testing.T) {
		e := newPairLiveShapedEnv(t, &pairNativeClosers{})
		at := pairLiveFirstLeg(t, e)
		e.quotes.mu.Lock()
		e.quotes.refErr = errors.New("quote outage")
		e.quotes.mu.Unlock()
		e.externalBookChange(pairTestOKX, -0.03)
		e.step(at.Add(2 * time.Minute))
		if e.lifecycle() != pairLifecycleRecovery {
			t.Fatalf("lifecycle %s, want recovery after the outside reduction", e.lifecycle())
		}
		if n := len(e.actionsByPurpose(pairPurposeProtective)); n != 0 {
			t.Fatalf("a protective reduction without a fresh reference reserved %d actions", n)
		}
		p := e.rt().Pair.Progress
		o := p.obligation(pairObligationReductionReference, pairTestOKX, "")
		r, ok := p.Retries[pairTestOKX]
		if o == nil || !ok || r.Count != 1 || r.RetryAtMs != at.Add(2*time.Minute).UnixMilli()+5000 {
			t.Fatalf("the hold needs a visible obligation and the first backoff, got %+v %+v", o, r)
		}
		e.step(at.Add(2*time.Minute + 2*time.Second))
		if r := e.rt().Pair.Progress.Retries[pairTestOKX]; r.Count != 1 {
			t.Fatalf("the hold must wait for its backoff, got %+v", r)
		}
		e.step(at.Add(2*time.Minute + 6*time.Second))
		if r := e.rt().Pair.Progress.Retries[pairTestOKX]; r.Count != 2 || r.RetryAtMs != at.Add(2*time.Minute+6*time.Second).UnixMilli()+10000 {
			t.Fatalf("the retry backoff must double, got %+v", r)
		}
		if n := e.alertsWithPrefix("reduction_reference|"); n != 1 {
			t.Fatalf("one recovery episode must send one reduction-reference alert, got %d", n)
		}
		e.quotes.mu.Lock()
		e.quotes.refErr = nil
		e.quotes.mu.Unlock()
		e.step(at.Add(2*time.Minute + 17*time.Second))
		if n := len(e.actionsByPurpose(pairPurposeProtective)); n != 1 {
			t.Fatalf("a fresh reference after the backoff must send the reduction, got %d actions", n)
		}
		if o := e.rt().Pair.Progress.obligation(pairObligationReductionReference, pairTestOKX, ""); o != nil {
			t.Fatalf("the reference obligation must clear when the reduction reserves, got %+v", o)
		}
	})
	t.Run("a fresh reference sends a bounded reduce-only reduction", func(t *testing.T) {
		e := newPairLiveShapedEnv(t, &pairNativeClosers{})
		at := pairLiveFirstLeg(t, e)
		e.externalBookChange(pairTestOKX, -0.03)
		e.step(at.Add(2 * time.Minute))
		prot := e.actionsByPurpose(pairPurposeProtective)
		if len(prot) != 1 || prot[0].OrderSide != "buy" || prot[0].PriceBound != 60400.6 || !basketQtyEqual(prot[0].RequestedQty, 0.03) {
			t.Fatalf("the survivor must buy 0.03 base with the width cap 60400.6 (best ask 60100.1 plus 50 bps, rounded down to the 0.1 tick), got %+v", prot)
		}
		req := pairReductionRequest(t, prot[0])
		if req.Reference == nil || req.ReferencePx != "60100.1" || req.ReferenceMaxAgeMs != 5000 || req.ReferenceReceiptMs != req.Reference.FirstReceiptMs || !req.ReduceOnly {
			t.Fatalf("the saved request must carry the detached reference, its best ask and the quote age policy: %+v", req)
		}
		if v := e.rt().pairView[pairTestOKX]; v.Observation.PayloadHash != req.Reference.PayloadHash {
			t.Fatal("the saved reference must be the published view observation")
		}
		if n := len(e.venues.okx.submits); n != 2 {
			t.Fatalf("want the first leg and the protective reduction at the venue, got %d", n)
		}
	})
}

func TestBasketPairReductionExitBounds(t *testing.T) {
	t.Run("a hold-age exit uses the reference width bound on both venues", func(t *testing.T) {
		e := newPairTestEnv(t, pairSetLimit("max_hold_age_ms", 120000))
		pairEnterHoldRunner(e, 0.06)
		e.slotStep(0)
		e.slotStep(1)
		e.assertBooks("matched", 0.06, -0.06)
		e.step(pairSlotAt(pairTestStart(), 2).Add(5 * time.Second))
		exits := e.actionsByPurpose(pairPurposeExit)
		if len(exits) != 2 {
			t.Fatalf("both members must exit at the hold age, got %d", len(exits))
		}
		want := map[string]float64{pairTestHL: 59700, pairTestOKX: 60400.6}
		for _, a := range exits {
			if a.PriceBound != want[a.StrategyID] {
				t.Fatalf("%s exit bound %v, want the width bound %v", a.StrategyID, a.PriceBound, want[a.StrategyID])
			}
			req := pairReductionRequest(t, a)
			if req.Reference == nil || req.ReferenceMaxAgeMs != 5000 || req.PriceBound != a.PriceBound {
				t.Fatalf("%s exit request must save its reference authority: %+v", a.StrategyID, req)
			}
		}
		e.assertBooks("exited", 0, 0)
	})
	t.Run("a policy exit keeps its saved bound and holds while it does not cross", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		e.slotStep(1)
		e.assertBooks("matched", 0.06, -0.06)
		e.shiftHLBids(59400)
		slot2 := e.slotStep(2)
		if n := len(e.actionsByPurpose(pairPurposeExit)); n != 0 {
			t.Fatalf("a saved sell bound above the current best bid must hold every exit leg, got %d", n)
		}
		if o := e.rt().Pair.Progress.obligation(pairObligationReductionReference, pairTestHL, ""); o == nil {
			t.Fatal("the non-crossing exit needs a durable reference obligation")
		}
		if rb := e.rt().Rebalances[slot2.UnixMilli()]; rb == nil || rb.Status != basketRebalanceFrozen {
			t.Fatalf("the exit decision must stay frozen for a later step, got %+v", rb)
		}
		e.shiftHLBids(59999)
		e.step(slot2.Add(20 * time.Second))
		exits := e.actionsByPurpose(pairPurposeExit)
		want := map[string]float64{pairTestHL: 59500, pairTestOKX: 60600}
		if len(exits) != 2 {
			t.Fatalf("both exit legs must send once the saved bound crosses again, got %d", len(exits))
		}
		for _, a := range exits {
			if a.PriceBound != want[a.StrategyID] {
				t.Fatalf("%s exit bound %v, want the saved policy bound %v", a.StrategyID, a.PriceBound, want[a.StrategyID])
			}
		}
		if o := e.rt().Pair.Progress.obligation(pairObligationReductionReference, pairTestHL, ""); o != nil {
			t.Fatalf("the obligation must clear once the exit reserves, got %+v", o)
		}
	})
	t.Run("a reference that expires at the transport boundary sends nothing", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		pairEnterExitRunner(e, 0.06)
		e.slotStep(0)
		e.slotStep(1)
		slot2 := pairSlotAt(pairTestStart(), 2)
		e.slot(slot2, "in-2")
		armed := true
		e.bc.pairClock = func() time.Time {
			if armed && e.mu.TryLock() {
				e.mu.Unlock()
				return e.now.Add(6 * time.Second)
			}
			return e.now
		}
		before := len(e.venues.hl.submits) + len(e.venues.okx.submits)
		e.step(slot2.Add(time.Second))
		armed = false
		exits := e.actionsByPurpose(pairPurposeExit)
		if len(exits) == 0 || exits[0].Status != basketActionNoFill || len(e.venues.hl.submits)+len(e.venues.okx.submits) != before {
			t.Fatalf("an expired reference at the unlocked transport boundary must end the reserved exit unsent, got %+v", exits)
		}
		if !strings.Contains(exits[0].HoldReason, "reference_max_age_ms") {
			t.Fatalf("the refusal must name the saved reference age, got %q", exits[0].HoldReason)
		}
	})
}

func TestBasketPairReductionHyperliquidSizing(t *testing.T) {
	type calls struct{ sync, stop, state int }
	guard := func(t *testing.T) *calls {
		c := &calls{}
		prevSync, prevStop, prevState := syncHyperliquidProtection, runHyperliquidUpdateStopLossFunc, fetchHyperliquidStateFn
		syncHyperliquidProtection = func(StrategyConfig, hlProtectionPlan, *MultiNotifier, *StrategyLogger, []byte) (*HyperliquidProtectionSyncResult, bool) {
			c.sync++
			return nil, false
		}
		runHyperliquidUpdateStopLossFunc = func(string, string, string, float64, float64, int64) (*HyperliquidStopLossUpdateResult, string, error) {
			c.stop++
			return nil, "", fmt.Errorf("the fixture sends no stop update")
		}
		fetchHyperliquidStateFn = func(string) (float64, []HLPosition, error) {
			c.state++
			return 0, nil, fmt.Errorf("the fixture reads no account")
		}
		t.Cleanup(func() {
			syncHyperliquidProtection, runHyperliquidUpdateStopLossFunc, fetchHyperliquidStateFn = prevSync, prevStop, prevState
		})
		return c
	}
	survivor := func(t *testing.T) (*pairTestEnv, time.Time, *calls) {
		c := guard(t)
		e := newPairLiveShapedEnv(t, &pairNativeClosers{})
		at := pairLiveMatched(t, e)
		e.externalBookChange(pairTestOKX, 0)
		return e, at.Add(2 * time.Minute), c
	}
	t.Run("the uncapped size keeps the width bound and releases both locks before transport", func(t *testing.T) {
		e, at, c := survivor(t)
		lockFree, muFree := false, false
		e.venues.hl.onSubmit = func(a *basketAction) {
			if a.Purpose != pairPurposeProtective {
				return
			}
			done := make(chan struct{})
			go func() {
				unlock := lockHyperliquidProtectionSync("BTC")
				unlock()
				close(done)
			}()
			select {
			case <-done:
				lockFree = true
			case <-time.After(2 * time.Second):
			}
			if e.mu.TryLock() {
				muFree = true
				e.mu.Unlock()
			}
		}
		pairOpenVenue(e, pairTestHL, pairFee(basketVenueOutcome{Outcome: basketVenuePartial, CumQty: 0.02, CumNotional: 0.02 * 59700, Terminal: true}, 0.5, "USDC", true))
		e.step(at)
		prot := e.actionsByPurpose(pairPurposeProtective)
		if len(prot) != 1 || prot[0].StrategyID != pairTestHL || !basketQtyEqual(prot[0].RequestedQty, 0.06) || prot[0].PriceBound != 59700 {
			t.Fatalf("the survivor must sell its full 0.06 with the width floor 59700, got %+v", prot)
		}
		if !lockFree || !muFree {
			t.Fatalf("the transport must run without the symbol lock (%v) and without mu (%v), so native closes and protection are never delayed", lockFree, muFree)
		}
		e.assertBooks("partial", 0.04, 0)
		pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "disconnect"})
		e.step(at.Add(time.Minute))
		if c.sync != 0 || c.stop != 0 || c.state != 0 {
			t.Fatalf("a partial or unknown coordinator IOC never cancels protection, so no protection call may run, got %+v", *c)
		}
	})
	t.Run("a smaller venue position caps the reserved size", func(t *testing.T) {
		e, at, _ := survivor(t)
		e.venues.hl.setVenueBase(0.04)
		pairOpenVenue(e, pairTestHL, pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.04, CumNotional: 0.04 * 59700, Terminal: true}, 0.5, "USDC", true))
		e.step(at)
		prot := e.actionsByPurpose(pairPurposeProtective)
		if len(prot) != 1 || !basketQtyEqual(prot[0].RequestedQty, 0.04) || prot[0].NativeQty != 0.04 {
			t.Fatalf("the venue holds 0.04, so the reserved reduction must be capped to 0.04, got %+v", prot)
		}
		if req := pairReductionRequest(t, prot[0]); !basketQtyEqual(req.BaseQty, 0.04) || req.PriceBound != 59700 {
			t.Fatalf("the saved request must carry the capped size and the width bound: %+v", req)
		}
		e.assertBooks("only the confirmed quantity books", 0.02, 0)
	})
	t.Run("a failed refetch keeps the reduce-only fallback at the price bound", func(t *testing.T) {
		e, at, _ := survivor(t)
		e.venues.hl.position = func(pairContractMember, int64) (pairVenuePosition, error) {
			return pairVenuePosition{}, fmt.Errorf("venue unreachable")
		}
		e.step(at)
		prot := e.actionsByPurpose(pairPurposeProtective)
		if len(prot) != 1 || !basketQtyEqual(prot[0].RequestedQty, 0.06) || prot[0].PriceBound != 59700 {
			t.Fatalf("an uncapped reduce-only fallback with no opposite peer must send the book size at the width floor, got %+v", prot)
		}
	})
	t.Run("a flat venue skips with a durable hold and no send", func(t *testing.T) {
		e, at, c := survivor(t)
		e.venues.hl.setVenueBase(0)
		e.step(at)
		if n := len(e.actionsByPurpose(pairPurposeProtective)); n != 0 {
			t.Fatalf("a skip answer must send nothing, got %d actions", n)
		}
		o := e.rt().Pair.Progress.obligation(pairObligationReductionReference, pairTestHL, "")
		r := e.rt().Pair.Progress.Retries[pairTestHL]
		if o == nil || r.Count != 1 || c.sync != 0 {
			t.Fatalf("a skip needs a durable obligation and backoff and cancels nothing, got %+v %+v %+v", o, r, *c)
		}
	})
	t.Run("a book change during the refetch sends nothing", func(t *testing.T) {
		e, at, _ := survivor(t)
		n := 0
		e.venues.hl.position = func(m pairContractMember, nowMs int64) (pairVenuePosition, error) {
			n++
			if n == 2 {
				e.mu.Lock()
				e.state.Strategies[pairTestHL].Positions["BTC"].Quantity = 0.05
				e.mu.Unlock()
			}
			return pairTestVenuePosition(m, 0.06, nowMs, nowMs)
		}
		e.step(at)
		if got := len(e.actionsByPurpose(pairPurposeProtective)); got != 0 || n < 2 {
			t.Fatalf("a book that changed during the refetch must hold the reduction (reads %d), got %d actions", n, got)
		}
	})
}

func TestBasketPairReductionNativeCloseKeepsAuthorityWithoutAReference(t *testing.T) {
	sent := 0
	closers := &pairNativeClosers{okx: func(string, *float64) (*OKXCloseResult, error) {
		sent++
		return &OKXCloseResult{Close: &OKXClose{Fill: &OKXCloseFill{TotalSz: 6, AvgPx: 60000}}}, nil
	}}
	e := newPairLiveShapedEnv(t, closers)
	pairLiveMatched(t, e)
	e.quotes.mu.Lock()
	e.quotes.refErr = errors.New("quote outage")
	e.quotes.mu.Unlock()
	e.prepare(e.now.Add(time.Second))
	if v := e.rt().pairView[pairTestOKX]; v.ReferenceValid {
		t.Fatal("the outage must leave the okx member without a committed reference")
	}
	closer := e.bc.pairKillSwitchOKXCloser(closers.okx)
	if _, err := closer("BTC-USDT-SWAP", nil); err != nil {
		t.Fatal(err)
	}
	ks := e.actionsByPurpose(pairPurposeKillSwitch)
	if sent != 1 || len(ks) != 1 || ks[0].PriceBound != 0 {
		t.Fatalf("the native close must send once with its own route and no price bound, got %d sends and %+v", sent, ks)
	}
	req := pairRequestOf(t, ks[0])
	if req["reference"] != nil || req["reference_px"] != nil || req["native_booked"] != true {
		t.Fatalf("a native request carries no reduction reference: %s", ks[0].RequestJSON)
	}
}

func TestBasketPairReductionVirtualAndRestartRules(t *testing.T) {
	t.Run("virtual reductions make no private read", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.externalBookChange(pairTestHL, 0.06)
		pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "held open for inspection"})
		e.step(pairTestStart().Add(time.Second))
		prot := e.actionsByPurpose(pairPurposeProtective)
		if len(prot) != 1 || prot[0].PriceBound != 59700 {
			t.Fatalf("the virtual survivor must reduce at the width floor, got %+v", prot)
		}
		if e.venues.hl.posReads != 0 || e.venues.okx.posReads != 0 {
			t.Fatalf("virtual venues are never asked for a private position, got %d and %d reads", e.venues.hl.posReads, e.venues.okx.posReads)
		}
	})
	t.Run("a restart waits for a new publication", func(t *testing.T) {
		e := newPairTestEnv(t, nil)
		e.quotes.mu.Lock()
		e.quotes.refErr = errors.New("quote outage")
		e.quotes.mu.Unlock()
		e.externalBookChange(pairTestOKX, -0.06)
		at := pairTestStart().Add(time.Second)
		e.step(at)
		e.quotes.mu.Lock()
		e.quotes.refErr = nil
		e.quotes.mu.Unlock()
		e.restart()
		if len(e.rt().pairView) != 0 {
			t.Fatal("the reference view is process memory and starts empty after a restart")
		}
		e.now = at.Add(6 * time.Second)
		e.bc.beginCycle()
		e.observe("")
		e.bc.step(e.inputs(e.now))
		if n := len(e.actionsByPurpose(pairPurposeProtective)); n != 0 {
			t.Fatalf("no reduction may use a reference from before the restart, got %d", n)
		}
		pairOpenVenue(e, pairTestOKX, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "held open for inspection"})
		e.step(at.Add(20 * time.Second))
		if n := len(e.actionsByPurpose(pairPurposeProtective)); n != 1 {
			t.Fatalf("a new publication after the restart must release the reduction, got %d", n)
		}
	})
}

func TestBasketPairReductionSealedReferenceStaysOutOfTheOrderArgument(t *testing.T) {
	e := newPairTestEnv(t, func(root map[string]interface{}) {
		pairSetBook("depth", 20)(root)
		pairSetBook("window_ms", 120000)(root)
		pairSetLimit("envelope_max_bytes", 4194304)(root)
	})
	e.quotes.mu.Lock()
	e.quotes.hlBids, e.quotes.hlAsks = pairDepthLevels(59999, -1, 20), pairDepthLevels(60001, 1, 20)
	e.quotes.okxBids, e.quotes.okxAsks = pairDepthLevels(60099.9, -0.1, 20), pairDepthLevels(60100.1, 0.1, 20)
	e.quotes.mu.Unlock()
	e.externalBookChange(pairTestHL, 0.06)
	pairOpenVenue(e, pairTestHL, basketVenueOutcome{Outcome: basketVenueUnknown, Detail: "held open for inspection"})
	e.step(pairTestStart().Add(time.Second))
	prot := e.actionsByPurpose(pairPurposeProtective)
	if len(prot) != 1 {
		t.Fatalf("the sealed survivor must reduce with its window reference, got %+v", prot)
	}
	wire, err := pairTransportRequestJSON(prot[0].RequestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(prot[0].RequestJSON) <= 131072 || len(wire) >= 4096 || strings.Contains(wire, `"reference":`) {
		t.Fatalf("the saved request (%d bytes) keeps the window reference while the order and query argument (%d bytes) must stay far below the 131072-byte Linux argument limit", len(prot[0].RequestJSON), len(wire))
	}
	if pairReductionRequest(t, prot[0]).PriceBound != 59700 {
		t.Fatal("the transport projection must keep the saved bound")
	}
}

func pairHeldExitLeg(t *testing.T) (*pairTestEnv, string) {
	t.Helper()
	e := newPairTestEnv(t, nil)
	pairEnterExitRunner(e, 0.06)
	e.slotStep(0)
	e.slotStep(1)
	e.assertBooks("matched", 0.06, -0.06)
	slot2 := pairSlotAt(pairTestStart(), 2)
	stepAt := slot2.Add(time.Second)
	advanced := false
	slow := func(a *basketAction) {
		if a.Purpose == pairPurposeExit && !advanced {
			advanced = true
			e.now = stepAt.Add(5001 * time.Millisecond)
		}
	}
	for _, v := range []*pairScriptedVenue{e.venues.hl, e.venues.okx} {
		v.mu.Lock()
		v.onSubmit = slow
		v.mu.Unlock()
	}
	e.slot(slot2, "in-2")
	e.step(stepAt)
	for _, v := range []*pairScriptedVenue{e.venues.hl, e.venues.okx} {
		v.mu.Lock()
		v.onSubmit = nil
		v.mu.Unlock()
	}
	exits := e.actionsByPurpose(pairPurposeExit)
	if !advanced || len(exits) != 1 || exits[0].Status != basketActionCommitted {
		t.Fatalf("want one settled exit leg and one held leg, got %+v", exits)
	}
	held := pairTestOKX
	if exits[0].StrategyID == pairTestOKX {
		held = pairTestHL
	}
	if o := e.rt().Pair.Progress.obligation(pairObligationReductionReference, held, ""); o == nil || o.RetryAtMs != e.now.UnixMilli()+5000 {
		t.Fatalf("the held exit leg must save its obligation with a retry at the hold time plus retry_initial_ms, got %+v", o)
	}
	return e, held
}

func TestBasketPairReductionHeldExitWakesForRetry(t *testing.T) {
	t.Run("the coordinator wakes after retry_initial_ms and sends the held leg", func(t *testing.T) {
		e, held := pairHeldExitLeg(t)
		holdAt := e.now
		d := pairWakeAt(t, e, holdAt)
		if d != 5*time.Second {
			t.Fatalf("a held exit leg must wake the coordinator after retry_initial_ms, got %s", d)
		}
		e.step(holdAt.Add(d))
		if exits := e.actionsByPurpose(pairPurposeExit); len(exits) != 2 {
			t.Fatalf("the wake step must publish a fresh reference and send the held leg, got %d exit actions", len(exits))
		}
		if o := e.rt().Pair.Progress.obligation(pairObligationReductionReference, held, ""); o != nil {
			t.Fatalf("the reservation must clear the hold, got %+v", o)
		}
		e.assertBooks("exited", 0, 0)
	})
	t.Run("a member flat by another route drops the wake and sends nothing", func(t *testing.T) {
		e, held := pairHeldExitLeg(t)
		holdAt := e.now
		e.externalBookChange(held, 0)
		if d := pairWakeAt(t, e, holdAt); d <= 5*time.Second {
			t.Fatalf("a flat member needs no retry wake, got %s", d)
		}
		e.step(holdAt.Add(5 * time.Second))
		if exits := e.actionsByPurpose(pairPurposeExit); len(exits) != 1 {
			t.Fatalf("a flat member must not be reduced, got %d exit actions", len(exits))
		}
		if o := e.rt().Pair.Progress.obligation(pairObligationReductionReference, held, ""); o != nil {
			t.Fatalf("a flat member must clear the hold, got %+v", o)
		}
		e.assertBooks("flat", 0, 0)
	})
	t.Run("a reference that keeps failing wakes no more often than retry_initial_ms", func(t *testing.T) {
		e, held := pairHeldExitLeg(t)
		e.quotes.mu.Lock()
		e.quotes.refErr = errors.New("quote outage")
		e.quotes.mu.Unlock()
		at := e.now
		for i := 0; i < 3; i++ {
			at = at.Add(5 * time.Second)
			e.step(at)
			if exits := e.actionsByPurpose(pairPurposeExit); len(exits) != 1 {
				t.Fatalf("retry %d: a failed reference read must not send the held leg, got %d exit actions", i, len(exits))
			}
			if o := e.rt().Pair.Progress.obligation(pairObligationReductionReference, held, ""); o == nil {
				t.Fatalf("retry %d: the hold must stay open while the reference read fails", i)
			}
			if d := pairWakeAt(t, e, at); d != 5*time.Second {
				t.Fatalf("retry %d: the next wake must be retry_initial_ms after the hold, got %s", i, d)
			}
			if d := pairWakeAt(t, e, at.Add(time.Second)); d != 4*time.Second {
				t.Fatalf("retry %d: no earlier wake may fire inside retry_initial_ms, got %s", i, d)
			}
		}
		if n := e.alertsWithPrefix("reduction_reference|"); n != 1 {
			t.Fatalf("one hold episode must send one alert, got %d", n)
		}
	})
}
