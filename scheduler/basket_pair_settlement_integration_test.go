package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func pairFiveFills() basketVenueOutcome {
	var fills []pairTestFill
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		fills = append(fills, pairTestFill{id: id, qty: "0.012", notional: "717.6"})
	}
	return pairWithFills(basketVenueOutcome{Outcome: basketVenueFilled, Terminal: true}, fills...)
}

func pairFeeRecs(amount string, ids ...string) []pairTestFeeRec {
	var out []pairTestFeeRec
	for _, id := range ids {
		out = append(out, pairTestFeeRec{id: id, amount: amount, currency: "USDT"})
	}
	return out
}

func (e *pairTestEnv) okxEntry() *basketAction {
	e.t.Helper()
	acts := e.actionsByPurpose(pairPurposeEntry)
	for _, a := range acts {
		if a.StrategyID == pairTestOKX {
			return a
		}
	}
	e.t.Fatalf("no okx entry action: %+v", acts)
	return nil
}

func (e *pairTestEnv) okxExit() *basketAction {
	for _, a := range e.actionsByPurpose(pairPurposeExit) {
		if a.StrategyID == pairTestOKX {
			return a
		}
	}
	return nil
}

func (e *pairTestEnv) alertCount(prefix string) int {
	n := 0
	for _, k := range e.rt().Pair.Progress.Alerts {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func (e *pairTestEnv) savedActionColumns(id string) (string, string, string, int64, int) {
	e.t.Helper()
	var fills, fees, ledger string
	var version int64
	var final int
	if err := e.db().db.QueryRow(`SELECT fill_coverage_json, fee_coverage_json, settlement_json, fee_evidence_version, fee_final FROM basket_member_actions WHERE action_id = ?`, id).Scan(&fills, &fees, &ledger, &version, &final); err != nil {
		e.t.Fatal(err)
	}
	return fills, fees, ledger, version, final
}

func TestBasketPairSettlementFeeSetsRefuseStaleAndNonSupersets(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e, 0.06)
	fill := pairFiveFills()
	pairOpenVenue(e, pairTestOKX, pairWithFees(fill, false, pairFeeRecs("0.25", "a", "b", "c")...))
	e.slotStep(0)
	a := e.okxEntry()
	okx := e.state.Strategies[pairTestOKX]
	if !basketQtyEqual(a.AppliedQty, 0.06) || math.Abs(a.AppliedFee-0.75) > 1e-9 || a.FeeEvidenceVersion != 3 || a.AppliedFeeVersion != 3 {
		t.Fatalf("version 3 {a,b,c} with complete execution records must book the quantity and its three covered fees: %+v", a)
	}
	accepted := a.FeeCoverageJSON
	cash := okx.Cash
	assertUnchanged := func(label string) {
		t.Helper()
		if a.FeeCoverageJSON != accepted || a.FeeEvidenceVersion != 3 || math.Abs(a.AppliedFee-0.75) > 1e-9 || okx.Cash != cash || a.FeeFinal {
			t.Fatalf("%s: the accepted fee state, the charge or finality changed: cash %v->%v %+v", label, cash, okx.Cash, a)
		}
		_, fees, _, version, final := e.savedActionColumns(a.ActionID)
		if fees != accepted || version != 3 || final != 0 {
			t.Fatalf("%s: the saved fee state changed: version %d final %d", label, version, final)
		}
	}
	e.mu.Lock()
	prevFees, prevVersion := a.FeeCoverageJSON, a.FeeEvidenceVersion
	older := pairWithFees(fill, false, pairFeeRecs("0.25", "a", "b")...)
	a.FeeCoverageJSON, a.FeeEvidenceVersion = pairMarshalRecords(older.FeeCoverage, 2), 2
	e.rt().touchPairProgress()
	err := e.store.PersistBasketProgress(e.rt())
	a.FeeCoverageJSON, a.FeeEvidenceVersion = prevFees, prevVersion
	e.mu.Unlock()
	if !errors.Is(err, errPairGuardMiss) {
		t.Fatalf("a stale runtime must not replace the newer saved fee set, got %v", err)
	}
	pairAnswerVenue(e, a, pairWithFees(fill, true, pairFeeRecs("0.25", "a", "b")...))
	e.step(e.now.Add(time.Second))
	assertUnchanged("stale version 2 {a,b}")
	if e.rt().Pair.Progress.IncreasesFrozen || e.rt().Pair.Progress.obligation(pairObligationFeeConflict, pairTestOKX, a.ActionID) != nil {
		t.Fatal("a stale subset must not open a conflict")
	}
	pairAnswerVenue(e, a, pairWithFees(fill, true, pairFeeRecs("0.25", "a", "b", "d", "e")...))
	e.step(e.now.Add(time.Second))
	assertUnchanged("inconsistent version 4 {a,b,d,e}")
	p := e.rt().Pair.Progress
	if !p.IncreasesFrozen || p.obligation(pairObligationFeeConflict, pairTestOKX, a.ActionID) == nil || e.alertCount("fee_conflict|") != 1 {
		t.Fatalf("a non-superset must freeze increases with one durable CRITICAL alert: %+v", p)
	}
	e.restart()
	a = e.okxEntry()
	okx = e.state.Strategies[pairTestOKX]
	assertUnchanged("after a restart")
	pairAnswerVenue(e, a, pairWithFees(fill, true, append(pairFeeRecs("0.25", "a", "b"), pairTestFeeRec{"c", "0.3", "USDT"}, pairTestFeeRec{"d", "0.25", "USDT"})...))
	e.step(e.now.Add(time.Second))
	assertUnchanged("a changed retained record under a higher version")
	if !e.rt().Pair.Progress.IncreasesFrozen || e.alertCount("fee_conflict|") != 1 {
		t.Fatal("the unresolved conflict must stay frozen with exactly one CRITICAL alert after a restart")
	}

	e2 := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e2, 0.06)
	part := pairWithFills(basketVenueOutcome{Outcome: pairVenueOpen}, pairTestFill{id: "a", qty: "0.012", notional: "717.6"})
	pairOpenVenue(e2, pairTestOKX, pairWithFees(part, false, pairFeeRecs("0.25", "a")...))
	e2.slotStep(0)
	b := e2.okxEntry()
	if !basketQtyEqual(b.AppliedQty, 0.012) {
		t.Fatalf("setup: the first partial fill must book: %+v", b)
	}
	more := pairWithFills(basketVenueOutcome{Outcome: pairVenueOpen}, pairTestFill{id: "a", qty: "0.012", notional: "717.6"}, pairTestFill{id: "b", qty: "0.012", notional: "717.6"})
	pairAnswerVenue(e2, b, pairWithFees(more, false, pairFeeRecs("0.4", "a")...))
	e2.step(e2.now.Add(time.Second))
	if !basketQtyEqual(b.ConfirmedQty, 0.024) || !basketQtyEqual(b.AppliedQty, 0.012) {
		t.Fatalf("valid execution evidence must be accepted beside a conflicting fee answer, and nothing more booked: %+v", b)
	}
	if got := e2.bc.pairExposure(e2.rt(), pairTestOKX); !basketQtyEqual(got, -0.024) || !basketQtyEqual(e2.signed(pairTestOKX), -0.012) {
		t.Fatalf("the confirmed unapplied fill must stay visible to risk while the fee conflict holds settlement: exposure %v", got)
	}
	if e2.rt().Pair.Progress.obligation(pairObligationFeeConflict, pairTestOKX, b.ActionID) == nil {
		t.Fatal("the changed retained fee must open a conflict")
	}
}

func TestBasketPairSettlementFinalityAdvancesOnceAndNeverRegresses(t *testing.T) {
	single := basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59800, Terminal: true}
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e, 0.06)
	pairOpenVenue(e, pairTestOKX, pairFee(single, 1.5, "USDT", false))
	e.slotStep(0)
	a := e.okxEntry()
	if a.FeeFinal || a.Status != basketActionUnresolved || !basketQtyEqual(a.AppliedQty, 0.06) {
		t.Fatalf("complete coverage without a final claim must book and stay unresolved: %+v", a)
	}
	pairAnswerVenue(e, a, pairFee(single, 1.5, "USDT", true))
	e.step(e.now.Add(time.Second))
	if !a.FeeFinal || a.Status != basketActionCommitted || math.Abs(a.AppliedFee-1.5) > 1e-9 {
		t.Fatalf("an identical same-version terminal complete answer must advance finality once and commit: %+v", a)
	}
	if _, _, _, _, final := e.savedActionColumns(a.ActionID); final != 1 {
		t.Fatal("finality must be saved")
	}
	e.mu.Lock()
	a.FeeFinal = false
	e.rt().touchPairProgress()
	err := e.store.PersistBasketProgress(e.rt())
	a.FeeFinal = true
	e.mu.Unlock()
	if !errors.Is(err, errPairGuardMiss) {
		t.Fatalf("a stale writer must not clear saved finality, got %v", err)
	}
	e.restart()
	if _, _, _, _, final := e.savedActionColumns(a.ActionID); final != 1 {
		t.Fatal("finality must survive a restart")
	}

	e2 := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e2, 0.06)
	two := pairWithFills(basketVenueOutcome{Outcome: basketVenueFilled, Terminal: true}, pairTestFill{id: "a", qty: "0.03", notional: "1794"}, pairTestFill{id: "b", qty: "0.03", notional: "1794"})
	pairOpenVenue(e2, pairTestOKX, pairWithFees(two, true, pairFeeRecs("0.75", "a")...))
	e2.slotStep(0)
	b := e2.okxEntry()
	if b.FeeFinal || b.Status != basketActionUnresolved || e2.rt().Pair.Progress.obligation(pairObligationFeeIncomplete, pairTestOKX, b.ActionID) == nil {
		t.Fatalf("an incomplete final claim must not advance finality and must keep fee_incomplete visible: %+v", b)
	}
	pairAnswerVenue(e2, b, pairWithFees(two, false, pairFeeRecs("0.75", "a", "b")...))
	e2.step(e2.now.Add(time.Second))
	e2.step(e2.now.Add(time.Second))
	if b.FeeFinal || b.Status != basketActionUnresolved || math.Abs(b.AppliedFee-1.5) > 1e-9 {
		t.Fatalf("complete coverage without a final claim applies the fee and stays unresolved: %+v", b)
	}
	pairAnswerVenue(e2, b, pairWithFees(two, true, pairFeeRecs("0.75", "a")...))
	e2.step(e2.now.Add(time.Second))
	if b.FeeFinal || b.Status == basketActionCommitted {
		t.Fatalf("a stale final claim must not advance finality: %+v", b)
	}
	pairAnswerVenue(e2, b, pairWithFees(two, true, pairFeeRecs("0.75", "a", "b")...))
	e2.step(e2.now.Add(time.Second))
	if !b.FeeFinal || b.Status != basketActionCommitted || math.Abs(b.AppliedFee-1.5) > 1e-9 {
		t.Fatalf("the identical complete final answer must advance finality once: %+v", b)
	}

	e3 := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e3, 0.06)
	pairOpenVenue(e3, pairTestOKX, basketVenueOutcome{Outcome: basketVenueRejected, Detail: "post-only would cross"})
	e3.slotStep(0)
	c := e3.okxEntry()
	if c.Status != basketActionNoFill || c.FeeFinal || len(e3.rt().Pair.Progress.Obligations) != 0 {
		t.Fatalf("a terminal zero-fill needs no fee record: %+v %+v", c, e3.rt().Pair.Progress.Obligations)
	}
}

func pairHistoricalEvidence(t0 time.Time) (func(d *pairEvidenceDocument), map[string]int64) {
	ms := func(d time.Duration) int64 { return t0.Add(d).UnixMilli() }
	times := map[string]int64{"x": ms(100 * time.Second), "y": ms(110 * time.Second), "gap": ms(92 * time.Second)}
	tx, ty := times["x"], times["y"]
	return func(d *pairEvidenceDocument) {
		var key pairEvidenceKey
		for i := range d.Conversions {
			c := &d.Conversions[i]
			if c.Key.MemberID == pairTestOKX && c.Currency == "USDT" {
				c.ValidThroughMs = ms(90 * time.Second)
				key = c.Key
			}
		}
		add := func(id, rate string, observed, receipt, from, through int64) {
			d.Conversions = append(d.Conversions, pairConversionEvidence{pairEvidenceStamp: pairEvidenceStamp{V: pairEvidenceVersion, Key: key, EvidenceID: id,
				ObservedMs: observed, FirstReceiptMs: receipt, ValidFromMs: from, ValidThroughMs: through, Provenance: "research-schedule:fixture"}, Currency: "USDT", RateUSD: pairDecimal(rate)})
		}
		add("conv-late", "1", ms(95*time.Second), ms(95*time.Second), ms(95*time.Second), 4_102_444_800_000)
		add("conv-hist-x", "1.1", tx-1000, tx+5000, tx, tx)
		add("conv-hist-y", "0.9", ty-500, ty+1000, ty-5000, ty)
		add("conv-future", "5", tx-1000, ms(24*time.Hour), tx-1000, 4_102_444_800_000)
		add("conv-late-observed", "7", tx+1, tx+6000, tx-5000, tx+8000)
	}, times
}

func (e *pairTestEnv) runOKXExit(t *testing.T, okxExit basketVenueOutcome) (*basketAction, *basketAction) {
	t.Helper()
	open := func(px float64, cur string) basketVenueOutcome {
		return pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * px, Terminal: true}, 1.5, cur, true)
	}
	pairOpenVenue(e, pairTestOKX, open(59800, "USDT"))
	e.slotStep(0)
	entry := e.okxEntry()
	pairOpenVenue(e, pairTestHL, open(60200, "USDC"))
	e.slotStep(1)
	e.assertBooks("matched", 0.06, -0.06)
	pairOpenVenue(e, pairTestOKX, okxExit)
	pairOpenVenue(e, pairTestHL, pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 59600, Terminal: true}, 1.80, "USDC", true))
	for i := 2; i < 8 && e.okxExit() == nil; i++ {
		at := pairSlotAt(pairTestStart(), i)
		e.slot(at, fmt.Sprintf("in-%d", i))
		e.step(at.Add(time.Second + 123456789*time.Nanosecond))
	}
	exit := e.okxExit()
	if exit == nil {
		t.Fatal("setup: the okx exit was never dispatched")
	}
	return entry, exit
}

func TestBasketPairSettlementHistoricalConversionsUseFillTime(t *testing.T) {
	mutate, times := pairHistoricalEvidence(pairTestStart())
	e := newPairLiveShapedEnvWithEvidence(t, &pairNativeClosers{}, mutate)
	pairEnterExitRunner(e, 0.06)
	exitFills := pairWithFills(basketVenueOutcome{Outcome: basketVenueFilled, Terminal: true},
		pairTestFill{id: "x", qty: "0.03", notional: "1809", atMs: times["x"]}, pairTestFill{id: "y", qty: "0.03", notional: "1809.3", atMs: times["y"]})
	entry, exit := e.runOKXExit(t, pairWithFees(exitFills, false, pairTestFeeRec{"x", "0.9", "USDT"}))
	if !basketQtyEqual(exit.AppliedQty, 0.06) || e.signed(pairTestOKX) != 0 {
		t.Fatalf("the exit must book once both fills convert at their own times: %+v", exit)
	}
	entryLedger, err := pairParseLedger(entry.SettlementJSON)
	if err != nil || len(entryLedger.Fills) != 1 || entryLedger.Fills[0].Conversion == nil || entryLedger.Fills[0].Conversion.EvidenceID != "conv-"+pairTestOKX+"-USDT" || entryLedger.Fills[0].TimeSource != pairFillTimeAccepted {
		t.Fatalf("the increase must first use its saved request conversion at the accepted time fallback: %+v %v", entryLedger, err)
	}
	ledger, err := pairParseLedger(exit.SettlementJSON)
	if err != nil || len(ledger.Fills) != 2 || len(ledger.Fees) != 1 {
		t.Fatalf("the exit ledger must save both fills and the covered fee: %+v %v", ledger, err)
	}
	want := map[string]struct {
		id   string
		rate string
		usd  float64
	}{"x": {"conv-hist-x", "1.1", 1809 * 1.1}, "y": {"conv-hist-y", "0.9", 1809.3 * 0.9}}
	for _, f := range ledger.Fills {
		w := want[f.FillID]
		if f.Conversion == nil || f.Conversion.EvidenceID != w.id || f.RateUSD != w.rate || math.Abs(f.NotionalUSD-w.usd) > 1e-9 || f.SelectedTimeMs != times[f.FillID] || f.TimeSource != pairFillTimeVenue {
			t.Fatalf("fill %s must convert with %s at its own venue time: %+v", f.FillID, w.id, f)
		}
		if f.SettlementAtMs < f.Conversion.FirstReceiptMs {
			t.Fatalf("fill %s used evidence first received after settlement: %+v", f.FillID, f)
		}
	}
	if fx := ledger.Fees[0]; fx.FillID != "x" || fx.Conversion.EvidenceID != "conv-hist-x" || math.Abs(fx.FeeUSD-0.99) > 1e-9 || fx.Mode != pairFeeModeQuantity {
		t.Fatalf("the covered fee must convert at its fill's rate and book with the quantity: %+v", fx)
	}
	var px, fee float64
	if err := e.db().db.QueryRow(`SELECT price, exchange_fee FROM trades WHERE symbol = 'BTC' AND is_close = 1 AND exchange_order_id = ?`, exit.ClientOrderID).Scan(&px, &fee); err != nil {
		t.Fatal(err)
	}
	if math.Abs(px-(1809*1.1+1809.3*0.9)/0.06) > 1e-6 || math.Abs(fee-0.99) > 1e-9 {
		t.Fatalf("the booked price must sum separately converted notionals, got %v fee %v", px, fee)
	}
	fillsBefore := exit.SettlementJSON[:strings.Index(exit.SettlementJSON, `"fees"`)]
	e.mu.Lock()
	e.venues.okx.val.Conversions = map[string]float64{"USD": 1, "USDC": 1, "USDT": 2}
	e.mu.Unlock()
	okx := e.state.Strategies[pairTestOKX]
	cash := okx.Cash
	full := pairWithFees(exitFills, true, pairTestFeeRec{"x", "0.9", "USDT"}, pairTestFeeRec{"y", "0.9", "USDT"})
	pairAnswerVenue(e, exit, full)
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	if got := cash - okx.Cash; math.Abs(got-0.81) > 1e-9 || exit.Status != basketActionCommitted {
		t.Fatalf("the late fee must charge only its own USD value at its fill's saved rate once, moved cash by %v: %+v", got, exit)
	}
	if !strings.HasPrefix(exit.SettlementJSON, fillsBefore) {
		t.Fatal("a later fee or a changed cycle valuation rewrote a saved fill conversion")
	}
	ledger, _ = pairParseLedger(exit.SettlementJSON)
	if len(ledger.Fees) != 2 || ledger.Fees[0].FeeUSD != 0.99 || ledger.Fees[1].Conversion.EvidenceID != "conv-hist-y" || ledger.Fees[1].Mode != pairFeeModeCorrection {
		t.Fatalf("the retained fee must keep its saved USD value and the new fee its own conversion: %+v", ledger.Fees)
	}
	saved := exit.SettlementJSON
	cash = okx.Cash
	e.restart()
	_, _, dbLedger, _, _ := e.savedActionColumns(exit.ActionID)
	if dbLedger != saved || math.Abs(e.state.Strategies[pairTestOKX].Cash-cash) > 1e-9 {
		t.Fatal("a restart changed the saved conversion ledger or the cash")
	}
	e.step(e.now.Add(time.Second))
	if math.Abs(e.state.Strategies[pairTestOKX].Cash-cash) > 1e-9 {
		t.Fatal("a cycle after the restart charged again")
	}

	t.Run("a conflicting tie holds visibly", func(t *testing.T) {
		mutate, times := pairHistoricalEvidence(pairTestStart())
		tie := func(d *pairEvidenceDocument) {
			mutate(d)
			for _, c := range d.Conversions {
				if c.EvidenceID == "conv-hist-x" {
					c.EvidenceID, c.RateUSD = "conv-hist-x-tie", "1.2"
					d.Conversions = append(d.Conversions, c)
					break
				}
			}
		}
		e := newPairLiveShapedEnvWithEvidence(t, &pairNativeClosers{}, tie)
		pairEnterExitRunner(e, 0.06)
		one := pairWithFills(basketVenueOutcome{Outcome: basketVenueFilled, Terminal: true}, pairTestFill{id: "x", qty: "0.06", notional: "3618", atMs: times["x"]})
		_, exit := e.runOKXExit(t, pairWithFees(one, true, pairTestFeeRec{"x", "1.8", "USDT"}))
		o := e.rt().Pair.Progress.obligation(pairObligationSettlementConversion, pairTestOKX, exit.ActionID)
		if exit.AppliedQty != 0 || o == nil || !strings.Contains(o.Detail, "different rates") {
			t.Fatalf("tied evidence with different rates must refuse and hold: %+v %+v", exit, e.rt().Pair.Progress.Obligations)
		}
	})

	t.Run("a held increment keeps corrections to booked fills", func(t *testing.T) {
		mutate, times := pairHistoricalEvidence(pairTestStart())
		e := newPairLiveShapedEnvWithEvidence(t, &pairNativeClosers{}, mutate)
		pairEnterExitRunner(e, 0.06)
		first := pairWithFills(basketVenueOutcome{Outcome: pairVenueOpen},
			pairTestFill{id: "x", qty: "0.02", notional: "1206", atMs: times["x"]}, pairTestFill{id: "y", qty: "0.02", notional: "1206.2", atMs: times["y"]})
		_, exit := e.runOKXExit(t, pairWithFees(first, false, pairTestFeeRec{"x", "0.6", "USDT"}))
		if !basketQtyEqual(exit.AppliedQty, 0.04) || math.Abs(exit.AppliedFee-0.66) > 1e-9 {
			t.Fatalf("setup: the first increment must book x and y with x's fee: %+v", exit)
		}
		more := pairWithFills(basketVenueOutcome{Outcome: pairVenueOpen},
			pairTestFill{id: "x", qty: "0.02", notional: "1206", atMs: times["x"]}, pairTestFill{id: "y", qty: "0.02", notional: "1206.2", atMs: times["y"]},
			pairTestFill{id: "z", qty: "0.02", notional: "1206", atMs: times["gap"]})
		pairAnswerVenue(e, exit, pairWithFees(more, false, pairTestFeeRec{"x", "0.6", "USDT"}, pairTestFeeRec{"y", "0.6", "USDT"}))
		okx := e.state.Strategies[pairTestOKX]
		cash := okx.Cash
		e.step(e.now.Add(time.Second))
		e.step(e.now.Add(time.Second))
		if !basketQtyEqual(exit.AppliedQty, 0.04) || e.rt().Pair.Progress.obligation(pairObligationSettlementConversion, pairTestOKX, exit.ActionID) == nil {
			t.Fatalf("the increment with no conversion at its fill time must hold visibly: %+v", exit)
		}
		if got := cash - okx.Cash; math.Abs(got-0.54) > 1e-9 || math.Abs(exit.AppliedFee-1.2) > 1e-9 {
			t.Fatalf("the booked fill's late fee must correct once at its saved rate while the next increment holds: cash moved %v %+v", got, exit)
		}
	})

	t.Run("a missing historical rate holds visibly", func(t *testing.T) {
		mutate, times := pairHistoricalEvidence(pairTestStart())
		e := newPairLiveShapedEnvWithEvidence(t, &pairNativeClosers{}, mutate)
		pairEnterExitRunner(e, 0.06)
		gap := pairWithFills(basketVenueOutcome{Outcome: basketVenueFilled, Terminal: true}, pairTestFill{id: "z", qty: "0.06", notional: "3618", atMs: times["gap"]})
		_, exit := e.runOKXExit(t, pairWithFees(gap, true, pairTestFeeRec{"z", "1.8", "USDT"}))
		o := e.rt().Pair.Progress.obligation(pairObligationSettlementConversion, pairTestOKX, exit.ActionID)
		if exit.AppliedQty != 0 || o == nil || !strings.Contains(o.Detail, "no USDT-to-USD conversion evidence") {
			t.Fatalf("a fill time with no eligible conversion must hold the booking with a visible obligation: %+v %+v", exit, e.rt().Pair.Progress.Obligations)
		}
		if got := e.bc.pairExposure(e.rt(), pairTestOKX); !basketQtyEqual(got, 0) || !basketQtyEqual(e.signed(pairTestOKX), -0.06) {
			t.Fatalf("the unapplied reduction must stay visible in the exposure, exposure %v", got)
		}
	})
}

func TestBasketPairSettlementPartialCloseCorrectionMovesDailyPnL(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterExitRunner(e, 0.06)
	partial := pairWithFills(basketVenueOutcome{Outcome: basketVenueRejected, Detail: "partly filled, then cancelled"},
		pairTestFill{id: "x", qty: "0.015", notional: "904.5"}, pairTestFill{id: "y", qty: "0.015", notional: "904.5"})
	_, exit := e.runOKXExit(t, pairWithFees(partial, false, pairTestFeeRec{"x", "0.45", "USDT"}))
	rows := exit.bookedTrades()
	if !basketQtyEqual(exit.AppliedQty, 0.03) || len(rows) == 0 || !rows[len(rows)-1].IsClose || rows[len(rows)-1].ClosedPositionID != "" || !basketQtyEqual(e.signed(pairTestOKX), -0.03) {
		t.Fatalf("setup: the rejected tranche must book a partial close: %+v %+v", exit, rows)
	}
	e.mu.Lock()
	s := e.state.Strategies[pairTestOKX]
	rolloverDailyPnL(&s.RiskState)
	s.RiskState.ConsecutiveLosses = 3
	daily := s.RiskState.DailyPnL
	e.mu.Unlock()
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		t.Fatal(err)
	}
	pairAnswerVenue(e, exit, pairWithFees(partial, true, pairTestFeeRec{"x", "0.45", "USDT"}, pairTestFeeRec{"y", "0.45", "USDT"}))
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	r := e.state.Strategies[pairTestOKX].RiskState
	if math.Abs(r.DailyPnL-(daily-0.45)) > 1e-9 || r.ConsecutiveLosses != 3 || math.Abs(exit.AppliedFee-0.9) > 1e-9 {
		t.Fatalf("a correction on a partial-close row must change daily PnL once and keep the loss streak: daily %v want %v losses %d %+v", r.DailyPnL, daily-0.45, r.ConsecutiveLosses, exit)
	}
	if n := e.count(`SELECT COUNT(*) FROM closed_positions WHERE symbol = 'BTC' AND side = 'short'`); n != 0 {
		t.Fatalf("a partial close has no closed position to correct, found %d", n)
	}
	e.restart()
	if r := e.state.Strategies[pairTestOKX].RiskState; math.Abs(r.DailyPnL-(daily-0.45)) > 1e-9 || r.ConsecutiveLosses != 3 {
		t.Fatalf("the daily PnL correction must survive a restart once: %+v", r)
	}
}

func TestBasketPairSettlementStaleLookupCannotMutate(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterExitRunner(e, 0.06)
	calls := 0
	e.bc.pairConversionLookup = func(ctx context.Context, m pairContractMember, cur string, fillAtMs, settleAtMs int64) (pairConversionEvidence, error) {
		calls++
		if calls == 1 {
			return pairConversionEvidence{}, fmt.Errorf("evidence source timeout")
		}
		ev, err := e.adm.evidence.SettlementConversion(ctx, m, cur, fillAtMs, settleAtMs)
		if calls == 2 {
			ev.RateUSD = "2"
			return ev, err
		}
		e.mu.Lock()
		if a := e.okxExit(); a != nil {
			a.Status = basketActionUnresolved
		}
		e.mu.Unlock()
		return ev, err
	}
	exitFill := pairFee(basketVenueOutcome{Outcome: basketVenueFilled, CumQty: 0.06, CumNotional: 0.06 * 60300, Terminal: true}, 1.8, "USDT", true)
	_, exit := e.runOKXExit(t, exitFill)
	o := e.rt().Pair.Progress.obligation(pairObligationSettlementConversion, pairTestOKX, exit.ActionID)
	if calls != 1 || exit.AppliedQty != 0 || o == nil || !strings.Contains(o.Detail, "evidence source timeout") {
		t.Fatalf("a source error must hold the booking with a visible obligation: calls %d %+v", calls, exit)
	}
	okx := e.state.Strategies[pairTestOKX]
	cash, trades, final := okx.Cash, e.bookedTrades(pairTestOKX), exit.FeeFinal
	pairAnswerVenue(e, exit, exitFill)
	e.step(e.now.Add(time.Second))
	o = e.rt().Pair.Progress.obligation(pairObligationSettlementConversion, pairTestOKX, exit.ActionID)
	if calls != 3 || o == nil || !strings.Contains(o.Detail, "refused") {
		t.Fatalf("a returned record whose hash does not match must hold visibly, then the query retries: calls %d %+v", calls, o)
	}
	if exit.AppliedQty != 0 || exit.SettlementJSON != "" || exit.FeeFinal != final || okx.Cash != cash || e.bookedTrades(pairTestOKX) != trades || !basketQtyEqual(e.signed(pairTestOKX), -0.06) {
		t.Fatalf("a lookup result for an action that changed while the lock was released must not mutate cash, applied records or the book: calls %d cash %v->%v %+v", calls, cash, okx.Cash, exit)
	}
	e.bc.pairConversionLookup = nil
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	if !basketQtyEqual(exit.AppliedQty, 0.06) || e.signed(pairTestOKX) != 0 || exit.Status != basketActionCommitted {
		t.Fatalf("the next cycle must settle from the saved evidence once: %+v", exit)
	}
}

func TestBasketPairSettlementGuardMissUndoesStagedCorrection(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e, 0.06)
	fill := pairThreeFills()
	pairOpenVenue(e, pairTestOKX, pairCostFees(fill, 1, false))
	e.slotStep(0)
	a := e.okxEntry()
	okx := e.state.Strategies[pairTestOKX]
	cash, daily := okx.Cash, okx.RiskState.DailyPnL
	ledger, booked := a.SettlementJSON, a.BookedJSON
	fee, _, _ := e.savedFeeRow(a)
	if _, err := e.db().db.Exec(`UPDATE basket_member_actions SET settlement_json = settlement_json || ' ' WHERE action_id = ?`, a.ActionID); err != nil {
		t.Fatal(err)
	}
	pairAnswerVenue(e, a, pairCostFees(fill, 2, false))
	e.step(e.now.Add(time.Second))
	switch {
	case okx.Cash != cash || okx.RiskState.DailyPnL != daily:
		t.Fatalf("the refused correction must leave no in-memory charge: cash %v->%v", cash, okx.Cash)
	case a.SettlementJSON != ledger || a.BookedJSON != booked || a.AppliedFeeVersion != 1 || len(a.pendingFee) != 0 || a.feeFixUndo != nil:
		t.Fatalf("the refused correction must restore the action: %+v", a)
	case e.rt().Pair.Progress.obligation(pairObligationCostProvenance, pairTestOKX, a.ActionID) == nil || !e.rt().Pair.Progress.IncreasesFrozen:
		t.Fatalf("the refused correction must hold visibly: %+v", e.rt().Pair.Progress)
	}
	if got, _, _ := e.savedFeeRow(a); got != fee {
		t.Fatalf("the saved trade row changed to %v", got)
	}
	if _, err := e.db().db.Exec(`UPDATE basket_member_actions SET settlement_json = ? WHERE action_id = ?`, ledger, a.ActionID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
		t.Fatalf("a later valid partition save must succeed after the refusal: %v", err)
	}
}

func TestBasketPairSettlementBookingOwnersKeepExplicitFees(t *testing.T) {
	for _, member := range []string{pairTestHL, pairTestOKX} {
		for _, fee := range []float64{1.25, 0, -0.75} {
			t.Run(fmt.Sprintf("%s fee %v", member, fee), func(t *testing.T) {
				e := newPairLiveShapedEnv(t, &pairNativeClosers{})
				e.step(e.now.Add(time.Second))
				sc := e.bc.members[member]
				s := e.state.Strategies[member]
				book := func(dir, side string, qty, px float64, oid string) Trade {
					t.Helper()
					a := &basketAction{StrategyID: member, Symbol: "BTC", Direction: dir, PositionSide: side, ClientOrderID: oid}
					f := pairConfirmedFee{Present: true, Covered: true, Currency: "USD", Amount: fee, EvidenceID: "pfe-test", EvidenceVersion: 1, DeltaUSD: fee}
					from := len(s.TradeHistory)
					cash := s.Cash
					logger := e.bc.memberLogger(member)
					e.mu.Lock()
					b := bookPairFill(sc, s, e.cfg, a, qty, px, f, basketMemberObservation{Indicators: map[string]interface{}{"atr": 300.0}}, logger)
					e.mu.Unlock()
					logger.Close()
					if !(b.Qty > 0) || len(s.TradeHistory) != from+1 || s.confirmedFee != nil {
						t.Fatalf("%s %s %v must book one row and release the fee scope: %+v", dir, side, qty, b)
					}
					tr := s.TradeHistory[from]
					if tr.ExchangeFee != fee || tr.FeeSource != FeeSourceUserFills || tr.ExchangeOrderID != oid {
						t.Fatalf("%s %s must keep the explicit fee, its confirmed source and order id: %+v", dir, side, tr)
					}
					wantCash := cash - fee
					if tr.IsClose {
						wantCash = cash + tr.RealizedPnL - fee
					}
					if math.Abs(s.Cash-wantCash) > 1e-9 {
						t.Fatalf("%s %s moved cash to %v, want %v", dir, side, s.Cash, wantCash)
					}
					return tr
				}
				book(basketDirectionIncrease, entrySideLong, 0.02, 60000, "o-open-long")
				book(basketDirectionIncrease, entrySideLong, 0.01, 60100, "o-scale-long")
				book(basketDirectionReduce, entrySideLong, 0.01, 60200, "o-cut-long")
				book(basketDirectionReduce, entrySideLong, 0.02, 60300, "o-close-long")
				book(basketDirectionIncrease, entrySideShort, 0.02, 60000, "o-open-short")
				book(basketDirectionIncrease, entrySideShort, 0.01, 59900, "o-scale-short")
				book(basketDirectionReduce, entrySideShort, 0.01, 59800, "o-cut-short")
				book(basketDirectionReduce, entrySideShort, 0.02, 59700, "o-close-short")
				if err := e.store.SavePartition(e.state, e.rt().Partition); err != nil {
					t.Fatal(err)
				}
				rows, err := e.db().db.Query(`SELECT exchange_order_id, exchange_fee, fee_source FROM trades WHERE strategy_id = ? AND symbol = 'BTC' ORDER BY rowid`, member)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				n := 0
				for rows.Next() {
					var oid, source string
					var got float64
					if err := rows.Scan(&oid, &got, &source); err != nil {
						t.Fatal(err)
					}
					if got != fee || source != FeeSourceUserFills || !strings.HasPrefix(oid, "o-") {
						t.Fatalf("saved row %s keeps fee %v source %q", oid, got, source)
					}
					n++
				}
				if n != 8 {
					t.Fatalf("want 8 saved rows, got %d", n)
				}
			})
		}
	}
}

func TestBasketPairSettlementVirtualKeepsModeledFees(t *testing.T) {
	e := newPairTestEnv(t, nil)
	pairEnterHoldRunner(e, 0.06)
	e.slotStep(0)
	a := e.okxEntry()
	if a.Status != basketActionCommitted || !basketQtyEqual(a.AppliedQty, 0.06) || a.SettlementJSON != "" || a.FillCoverageJSON != "" || a.FeeFinal {
		t.Fatalf("a virtual research fill must book without coverage, ledger or finality: %+v", a)
	}
	var qty, px, fee float64
	var source string
	if err := e.db().db.QueryRow(`SELECT quantity, price, exchange_fee, fee_source FROM trades WHERE strategy_id = ? AND symbol = 'BTC'`, pairTestOKX).Scan(&qty, &px, &fee, &source); err != nil {
		t.Fatal(err)
	}
	if !basketQtyEqual(qty, 0.06) || math.Abs(px-59700) > 1e-9 || math.Abs(fee-0.06*59700*0.00045) > 1e-9 || source != FeeSourceModeled {
		t.Fatalf("a virtual research booking must keep its valuation price and modeled fee: qty %v px %v fee %v source %q", qty, px, fee, source)
	}
}

func TestBasketPairSettlementMigratesAndInspectsReadOnly(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e, 0.06)
	fill := pairThreeFills()
	pairOpenVenue(e, pairTestOKX, pairCostFees(fill, 1, false))
	e.slotStep(0)
	a := e.okxEntry()
	if a.SettlementJSON == "" || a.Status != basketActionUnresolved {
		t.Fatalf("setup: the booked entry must have a ledger and wait for fee evidence: %+v", a)
	}
	var request string
	if err := e.db().db.QueryRow(`SELECT request_json FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&request); err != nil {
		t.Fatal(err)
	}
	path := e.db().path
	e.store.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"accepted_at_ms", "fill_coverage_json", "fee_coverage_json", "fee_final", "settlement_json"} {
		if _, err := raw.Exec(`ALTER TABLE basket_member_actions DROP COLUMN ` + col); err != nil {
			t.Fatalf("simulate a file saved before the settlement columns: %v", err)
		}
	}
	raw.Close()
	before := pairFileDigest(t, path)
	ro, err := openStateDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := ro.loadBasketRecords()
	if err != nil {
		t.Fatalf("read-only projection on an unmigrated file: %v", err)
	}
	var got *basketAction
	for _, acts := range recs.Actions {
		for _, x := range acts {
			if x.ActionID == a.ActionID {
				got = x
			}
		}
	}
	if got == nil || got.FillCoverageJSON != "" || got.FeeCoverageJSON != "" || got.SettlementJSON != "" || got.FeeFinal || got.AcceptedAtMs != 0 || !basketQtyEqual(got.AppliedQty, 0.06) || got.RequestJSON != request {
		t.Fatalf("an unmigrated file must project safe defaults and keep applied totals and request bytes: %+v", got)
	}
	_, touched, err := ro.basketReplayTouchedRows(got.UpdatedAtMs)
	if err != nil {
		t.Fatalf("the replay reader must read an unmigrated file: %v", err)
	}
	found := false
	for _, acts := range touched {
		if x, ok := acts[a.ActionID]; ok && x.SettlementJSON == "" && !x.FeeFinal {
			found = true
		}
	}
	if !found {
		t.Fatal("the replay reader must project the same safe defaults")
	}
	ro.Close()
	if pairFileDigest(t, path) != before {
		t.Fatal("read-only inspection changed the unmigrated file")
	}
	e.store = openSourceStore(t, e.cfg)
	e.store.Close()
	e.restart()
	a = e.okxEntry()
	if a.SettlementJSON != "" || a.FeeCoverageJSON != "" || a.FeeEvidenceVersion != 1 || !basketQtyEqual(a.AppliedQty, 0.06) || a.RequestJSON != request {
		t.Fatalf("the migrated legacy action must keep its applied totals and request with unknown coverage: %+v", a)
	}
	okx := e.state.Strategies[pairTestOKX]
	cash := okx.Cash
	pairAnswerVenue(e, a, pairCostFees(fill, 2, true))
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	if okx.Cash != cash || a.Status == basketActionCommitted || a.FeeFinal || e.rt().Pair.Progress.obligation(pairObligationLegacySettlement, pairTestOKX, a.ActionID) == nil {
		t.Fatalf("legacy reconciliation must stay visibly unresolved without inventing coverage, finality or rates: cash %v->%v %+v %+v", cash, okx.Cash, a, e.rt().Pair.Progress.Obligations)
	}
	var savedRequest string
	if err := e.db().db.QueryRow(`SELECT request_json FROM basket_member_actions WHERE action_id = ?`, a.ActionID).Scan(&savedRequest); err != nil || savedRequest != request {
		t.Fatalf("the saved request bytes changed: %v", err)
	}
}

func TestBasketPairSettlementCorrectionTargetsEachBookingRow(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e, 0.06)
	fill := func(n int) basketVenueOutcome {
		var fills []pairTestFill
		for _, id := range []string{"a", "b", "c", "d"}[:n] {
			fills = append(fills, pairTestFill{id: id, qty: "0.015", notional: "897"})
		}
		out := pairWithFills(basketVenueOutcome{Outcome: pairVenueOpen}, fills...)
		if n == 4 {
			out.Outcome, out.Terminal = basketVenueFilled, true
		}
		return out
	}
	pairOpenVenue(e, pairTestOKX, pairWithFees(fill(2), false, pairFeeRecs("0.4", "a")...))
	e.slotStep(0)
	a := e.okxEntry()
	pairAnswerVenue(e, a, pairWithFees(fill(4), false, pairFeeRecs("0.4", "a", "c")...))
	e.step(e.now.Add(time.Second))
	rows := a.bookedTrades()
	if len(rows) != 2 || !basketQtyEqual(a.AppliedQty, 0.06) || math.Abs(a.AppliedFee-0.8) > 1e-9 {
		t.Fatalf("setup: two increments must book two rows with their covered fees: %+v %+v", rows, a)
	}
	okx := e.state.Strategies[pairTestOKX]
	cash := okx.Cash
	pairAnswerVenue(e, a, pairWithFees(fill(4), true, pairFeeRecs("0.4", "a", "b", "c", "d")...))
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	if got := cash - okx.Cash; math.Abs(got-0.8) > 1e-9 || a.Status != basketActionCommitted {
		t.Fatalf("one answer covering fills of two bookings must charge each fee once: cash moved %v %+v", got, a)
	}
	got := map[int64]float64{}
	q, err := e.db().db.Query(`SELECT rowid, exchange_fee FROM trades WHERE symbol = 'BTC' AND exchange_order_id = ? ORDER BY rowid`, a.ClientOrderID)
	if err != nil {
		t.Fatal(err)
	}
	var fees []float64
	for q.Next() {
		var id int64
		var f float64
		if err := q.Scan(&id, &f); err != nil {
			t.Fatal(err)
		}
		got[id] = f
		fees = append(fees, f)
	}
	q.Close()
	if len(fees) != 2 || math.Abs(fees[0]-0.8) > 1e-9 || math.Abs(fees[1]-0.8) > 1e-9 {
		t.Fatalf("each booking row must carry its own fills' fees, got %v", got)
	}
	ledger, _ := pairParseLedger(a.SettlementJSON)
	targets := map[int64]bool{}
	for _, f := range ledger.Fees {
		if f.Mode == pairFeeModeCorrection {
			targets[f.Target.TradeTsMs] = true
		}
	}
	if len(targets) != 2 {
		t.Fatalf("the corrections must target both booking rows: %+v", ledger.Fees)
	}
	cash = okx.Cash
	e.restart()
	e.step(e.now.Add(time.Second))
	if math.Abs(e.state.Strategies[pairTestOKX].Cash-cash) > 1e-9 {
		t.Fatal("a restart charged a correction again")
	}
}

func TestBasketPairSettlementEndedActionsKeepNoSettlementHold(t *testing.T) {
	t0 := pairTestStart()
	receipt := t0.Add(time.Hour)
	mutate, times := pairHistoricalEvidence(t0)
	delayed := func(d *pairEvidenceDocument) {
		mutate(d)
		for _, c := range d.Conversions {
			if c.Key.MemberID == pairTestOKX && c.Currency == "USDT" {
				c.EvidenceID, c.RateUSD = "conv-delayed-gap", pairDecimal("1")
				c.ObservedMs, c.FirstReceiptMs, c.ValidFromMs, c.ValidThroughMs = times["gap"]-1000, receipt.UnixMilli(), times["gap"]-1000, times["gap"]+1000
				d.Conversions = append(d.Conversions, c)
				break
			}
		}
	}
	e := newPairLiveShapedEnvWithEvidence(t, &pairNativeClosers{}, delayed)
	pairEnterExitRunner(e, 0.06)
	gap := pairWithFills(basketVenueOutcome{Outcome: basketVenueFilled, Terminal: true}, pairTestFill{id: "z", qty: "0.06", notional: "3618", atMs: times["gap"]})
	_, exit := e.runOKXExit(t, pairWithFees(gap, true, pairTestFeeRec{"z", "1.8", "USDT"}))
	if exit.AppliedQty != 0 || !exit.FeeFinal || e.rt().Pair.Progress.obligation(pairObligationSettlementConversion, pairTestOKX, exit.ActionID) == nil {
		t.Fatalf("setup: a final complete fee set must be accepted while the fill-time rate is not yet received: %+v %+v", exit, e.rt().Pair.Progress.Obligations)
	}
	bad := pairWithFees(gap, true, pairTestFeeRec{"z", "1.8", "USDT"})
	bad.FeeEvidenceVersion = 2
	pairAnswerVenue(e, exit, bad)
	e.step(e.now.Add(time.Second))
	if exit.Status == basketActionCommitted || e.rt().Pair.Progress.obligation(pairObligationFeeInvalid, pairTestOKX, exit.ActionID) == nil {
		t.Fatalf("a malformed fee answer on an action that is not complete must keep fee_invalid visible: %+v %+v", exit, e.rt().Pair.Progress.Obligations)
	}
	e.step(receipt.Add(time.Second))
	e.step(e.now.Add(time.Second))
	if exit.Status != basketActionCommitted || !basketQtyEqual(exit.AppliedQty, 0.06) {
		t.Fatalf("the exit must book and commit once the fill-time rate is received: %+v %+v", exit, e.rt().Pair.Progress.Obligations)
	}
	for _, o := range e.rt().Pair.Progress.Obligations {
		if o.ActionID == exit.ActionID {
			t.Fatalf("a committed action must keep no settlement obligation: %+v", o)
		}
	}
	e.step(e.now.Add(time.Second))
	if e.lifecycle() != pairLifecycleFlat {
		t.Fatalf("a flat pair with terminal actions and no settlement obligation must end the exit lifecycle, got %s %+v", e.lifecycle(), e.rt().Pair.Progress.Obligations)
	}
	e.restart()
	if o := e.rt().Pair.Progress.obligation(pairObligationFeeInvalid, pairTestOKX, exit.ActionID); o != nil {
		t.Fatalf("the cleared obligation must stay cleared after a restart: %+v", o)
	}

	e2 := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e2, 0.06)
	pairOpenVenue(e2, pairTestOKX, pairWithFees(basketVenueOutcome{Outcome: pairVenueOpen}, false, pairTestFeeRec{"ghost", "0.1", "USDT"}))
	e2.slotStep(0)
	a := e2.okxEntry()
	if a.ConfirmedQty != 0 || e2.rt().Pair.Progress.obligation(pairObligationFeeInvalid, pairTestOKX, a.ActionID) == nil {
		t.Fatalf("setup: an open zero-fill answer with a fee record that matches no execution record must keep fee_invalid: %+v %+v", a, e2.rt().Pair.Progress.Obligations)
	}
	pairAnswerVenue(e2, a, basketVenueOutcome{Outcome: basketVenueRejected, Detail: "post-only would cross"})
	e2.step(e2.now.Add(time.Second))
	if a.Status != basketActionNoFill || len(e2.rt().Pair.Progress.Obligations) != 0 {
		t.Fatalf("a terminal zero-fill must keep no fee_invalid obligation: %+v %+v", a, e2.rt().Pair.Progress.Obligations)
	}
	e2.restart()
	if len(e2.rt().Pair.Progress.Obligations) != 0 {
		t.Fatalf("the saved progress must keep no obligation after a restart: %+v", e2.rt().Pair.Progress.Obligations)
	}
}

func (e *pairTestEnv) savedFeeRows(oid string) ([]float64, []string) {
	e.t.Helper()
	q, err := e.db().db.Query(`SELECT exchange_fee, fee_source FROM trades WHERE symbol = 'BTC' AND exchange_order_id = ? ORDER BY rowid`, oid)
	if err != nil {
		e.t.Fatal(err)
	}
	defer q.Close()
	var fees []float64
	var sources []string
	for q.Next() {
		var f float64
		var s string
		if err := q.Scan(&f, &s); err != nil {
			e.t.Fatal(err)
		}
		fees, sources = append(fees, f), append(sources, s)
	}
	return fees, sources
}

func TestBasketPairSettlementFeeSourceStaysPendingUntilEveryFillIsCovered(t *testing.T) {
	e := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e, 0.06)
	fills := func(n int) basketVenueOutcome {
		var fs []pairTestFill
		for _, id := range []string{"a", "b", "c"}[:n] {
			fs = append(fs, pairTestFill{id: id, qty: "0.02", notional: "1196"})
		}
		out := pairWithFills(basketVenueOutcome{Outcome: pairVenueOpen}, fs...)
		if n == 3 {
			out.Outcome, out.Terminal = basketVenueFilled, true
		}
		return out
	}
	pairOpenVenue(e, pairTestOKX, pairWithFees(fills(2), false, pairFeeRecs("0.4", "a")...))
	e.slotStep(0)
	a := e.okxEntry()
	fees, sources := e.savedFeeRows(a.ClientOrderID)
	if !basketQtyEqual(a.AppliedQty, 0.04) || len(fees) != 1 || math.Abs(fees[0]-0.4) > 1e-9 || sources[0] != FeeSourceUserFillsPending {
		t.Fatalf("one row booking fills a and b with only a's fee must keep the confirmed amount and a pending source: %v %v %+v", fees, sources, a)
	}
	pairAnswerVenue(e, a, pairWithFees(fills(3), false, pairFeeRecs("0.4", "a")...))
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	fees, sources = e.savedFeeRows(a.ClientOrderID)
	if !basketQtyEqual(a.AppliedQty, 0.06) || len(fees) != 2 || fees[1] != 0 || sources[1] != FeeSourceUserFillsPending || sources[0] != FeeSourceUserFillsPending {
		t.Fatalf("fill c booked while the fee set is still {a} must not claim a confirmed zero fee: %v %v %+v", fees, sources, a)
	}
	pairAnswerVenue(e, a, pairWithFees(fills(3), false, pairFeeRecs("0.4", "a", "b")...))
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	fees, sources = e.savedFeeRows(a.ClientOrderID)
	if len(fees) != 2 || math.Abs(fees[0]-0.8) > 1e-9 || sources[0] != FeeSourceUserFills || fees[1] != 0 || sources[1] != FeeSourceUserFillsPending {
		t.Fatalf("the correction that covers every fill of the first row must confirm only that row: %v %v", fees, sources)
	}
	pairAnswerVenue(e, a, pairWithFees(fills(3), true, pairFeeRecs("0.4", "a", "b", "c")...))
	e.step(e.now.Add(time.Second))
	e.step(e.now.Add(time.Second))
	fees, sources = e.savedFeeRows(a.ClientOrderID)
	if a.Status != basketActionCommitted || len(fees) != 2 || math.Abs(fees[1]-0.4) > 1e-9 || sources[0] != FeeSourceUserFills || sources[1] != FeeSourceUserFills {
		t.Fatalf("complete coverage must confirm every row: %v %v %+v", fees, sources, a)
	}
	for _, tr := range e.state.Strategies[pairTestOKX].TradeHistory {
		if tr.Symbol == "BTC" && tr.TradeType != TradeTypeFunding && (tr.FeeSource != FeeSourceUserFills || tr.ExchangeOrderID != a.ClientOrderID) {
			t.Fatalf("the in-memory rows must match the saved rows: %+v", tr)
		}
	}

	e2 := newPairLiveShapedEnv(t, &pairNativeClosers{})
	pairEnterHoldRunner(e2, 0.06)
	two := pairWithFills(basketVenueOutcome{Outcome: basketVenueFilled, Terminal: true}, pairTestFill{id: "a", qty: "0.03", notional: "1794"}, pairTestFill{id: "b", qty: "0.03", notional: "1794"})
	pairOpenVenue(e2, pairTestOKX, pairWithFees(two, true, pairTestFeeRec{"a", "0", "USDT"}, pairTestFeeRec{"b", "-0.1", "USDT"}))
	e2.slotStep(0)
	b := e2.okxEntry()
	fees, sources = e2.savedFeeRows(b.ClientOrderID)
	if b.Status != basketActionCommitted || len(fees) != 1 || math.Abs(fees[0]+0.1) > 1e-9 || sources[0] != FeeSourceUserFills {
		t.Fatalf("a row whose every fill has a fee record, with a zero fee and a rebate, keeps the confirmed source and order id: %v %v %+v", fees, sources, b)
	}
}
